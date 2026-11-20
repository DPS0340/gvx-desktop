package recipes

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/ironpark/gvx-desktop/internal/rungv"
	"github.com/ironpark/gvx-desktop/internal/sandbox"
)

// A Windows path is shared at its drive as a folder; one that names no
// folder of a drive has no guest path.
func TestGuestPath(t *testing.T) {
	for host, want := range map[string]string{
		`C:\Users\me`:         "/c/Users/me",
		`c:\Users\me\src\`:    "/c/Users/me/src",
		`D:/work/app`:         "/d/work/app",
		`C:\`:                 "/c",
		`\\server\share\x`:    "",
		`Users\me`:            "",
		`C:`:                  "",
		`C:Users`:             "",
		`/Users/me`:           "",
		`C:\Users\me\..\you`:  "/c/Users/you",
		`E:\a b\ünïcode dir`:  "/e/a b/ünïcode dir",
		`1:\not\a\drive\here`: "",
	} {
		got, err := guestPath("windows", host)
		if got != want || (err != nil) != (want == "") {
			t.Errorf("windows %q: %q, %v; want %q", host, got, err, want)
		}
	}
	for host, want := range map[string]string{
		"/Users/me":       "/Users/me",
		"/home/me/src/":   "/home/me/src",
		"relative/folder": "",
	} {
		got, err := guestPath("linux", host)
		if got != want || (err != nil) != (want == "") {
			t.Errorf("linux %q: %q, %v; want %q", host, got, err, want)
		}
	}
}

// A new engine shares the home folder at its own path; the list is read
// back from the sandbox, and replaced without touching the other mounts.
func TestSharedFolders(t *testing.T) {
	home := rungv.ExpandHome("~")
	spec := DockerEngine()
	if got := SharedFoldersOf(spec); !slices.Equal(got, []string{home}) {
		t.Fatalf("a new engine shares %v", got)
	}
	g, _ := GuestPath(home)
	if got := SharedGuestPaths(spec); !slices.Equal(got, []string{g}) {
		t.Errorf("guest paths %v", got)
	}
	if err := spec.Validate(); err != nil {
		t.Fatal(err)
	}

	// A bind mount of the user's, read-only or at another path, is not a
	// shared folder, and stays.
	spec.Mounts = append(spec.Mounts,
		sandbox.Mount{Type: sandbox.MountBind, Source: "/data", Target: "/srv/data"},
		sandbox.Mount{Type: sandbox.MountBind, Source: "/ro", Target: "/ro", ReadOnly: true})
	other := []string{"/opt/projects", "/Volumes/work"}
	moved := WithSharedFolders(spec, other)
	if got := SharedFoldersOf(moved); !slices.Equal(got, other) {
		t.Errorf("after the change %v", got)
	}
	if len(moved.Mounts) != 5 || moved.Mounts[0].Target != "/var/lib/docker" || moved.Mounts[1].Target != "/srv/data" || moved.Mounts[2].Target != "/ro" {
		t.Errorf("mounts %+v", moved.Mounts)
	}
	if len(SharedFoldersOf(spec)) != 1 {
		t.Error("the sandbox given was changed")
	}
	if got := WithSharedFolders(spec, nil); len(SharedFoldersOf(got)) != 0 || len(got.Mounts) != 3 {
		t.Errorf("none shared: %+v", got.Mounts)
	}
}

func TestAddSharedFolder(t *testing.T) {
	root := t.TempDir()
	src := filepath.Join(root, "src")
	app := filepath.Join(src, "app")
	other := filepath.Join(root, "other")
	for _, d := range []string{app, other} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	file := filepath.Join(root, "file.txt")
	os.WriteFile(file, nil, 0o644)

	got, err := AddSharedFolder(nil, src)
	if err != nil || !slices.Equal(got, []string{src}) {
		t.Fatalf("first: %v %v", got, err)
	}
	why := func(folders []string, dir string) error {
		_, err := AddSharedFolder(folders, dir)
		return err
	}
	for name, c := range map[string]struct {
		dir  string
		want error
	}{
		"the same":       {src, ErrAlreadyShared},
		"inside one":     {app, ErrAlreadyShared},
		"missing":        {filepath.Join(root, "gone"), ErrNotFolder},
		"a file":         {file, ErrNotFolder},
		"relative":       {"src", ErrNoGuestPath},
		"the system's":   {"/usr/local", ErrReserved},
		"above system's": {"/var", ErrReserved},
		"the root":       {"/", ErrReserved},
	} {
		if err := why(got, c.dir); !errors.Is(err, c.want) {
			t.Errorf("%s: %v, want %v", name, err, c.want)
		}
	}
	var se *ShareError
	if err := why(got, app); !errors.As(err, &se) || se.Folder != src {
		t.Errorf("the folder it is inside: %+v", se)
	}
	// One beside is added; one above takes the place of those inside it.
	got, _ = AddSharedFolder(got, other)
	got, err = AddSharedFolder(got, root)
	if err != nil || !slices.Equal(got, []string{root}) {
		t.Errorf("above: %v %v", got, err)
	}
}

func TestUnshared(t *testing.T) {
	shared := []string{"/Users/me"}
	for src, want := range map[string]bool{
		"/Users/me/src":        false,
		"/Users/me":            false,
		"/Users/meow/src":      true,
		"/Volumes/work/app":    true,
		"/var/run/docker.sock": false,
		"/etc/localtime":       false,
		"/lib/modules":         false,
		"relative":             false,
		"/Users/me/../you/x":   true,
	} {
		if got := Unshared(src, shared); got != want {
			t.Errorf("%s: %v", src, got)
		}
	}
}
