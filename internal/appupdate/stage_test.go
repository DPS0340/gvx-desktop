package appupdate

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/ironpark/gvx-desktop/internal/ghrelease"
)

// zipOf packs a Windows release's zip: the program alone.
func zipOf(t *testing.T, program []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	w, _ := zw.Create("GVX Desktop.exe")
	w.Write(program)
	zw.Close()
	return buf.Bytes()
}

// tarOf packs a Linux release's tarball: the tree of /usr.
func tarOf(t *testing.T, program []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for _, d := range []string{"usr/", "usr/bin/", "usr/share/", "usr/share/applications/"} {
		tw.WriteHeader(&tar.Header{Name: d, Typeflag: tar.TypeDir, Mode: 0o755})
	}
	for _, f := range []struct {
		name string
		data []byte
	}{
		{"usr/share/applications/io.github.ironpark.gvx-desktop.desktop", []byte("[Desktop Entry]\n")},
		{"usr/bin/gvx-desktop", program},
	} {
		tw.WriteHeader(&tar.Header{Name: f.name, Typeflag: tar.TypeReg, Mode: 0o755, Size: int64(len(f.data))})
		tw.Write(f.data)
	}
	tw.Close()
	gz.Close()
	return buf.Bytes()
}

// serveRelease stands in for GitHub with release v0.6.0, whose file name
// holds body and whose name.sha256 says sum (the file's own when empty,
// none at all when "none").
func serveRelease(t *testing.T, name string, body []byte, sum string) Release {
	t.Helper()
	if sum == "" {
		s := sha256.Sum256(body)
		sum = hex.EncodeToString(s[:])
	}
	mux := http.NewServeMux()
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	mux.HandleFunc("/file", func(w http.ResponseWriter, r *http.Request) { w.Write(body) })
	mux.HandleFunc("/sum", func(w http.ResponseWriter, r *http.Request) { w.Write([]byte(sum + "  " + name + "\n")) })
	rel := Release{Tag: "v0.6.0", Page: srv.URL + "/page", Assets: []ghrelease.Asset{{Name: name, URL: srv.URL + "/file", Size: int64(len(body))}}}
	if sum != "none" {
		rel.Assets = append(rel.Assets, ghrelease.Asset{Name: name + ".sha256", URL: srv.URL + "/sum"})
	}
	return rel
}

func read(t *testing.T, p string) string {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		return "<" + err.Error() + ">"
	}
	return string(b)
}

func exists(p string) bool {
	_, err := os.Lstat(p)
	return err == nil
}

// A Windows or Linux update downloads the release's archive beside the
// program, unpacks the new program there, and on Prepare puts it in place,
// the old one beside it until the next start removes it. Pure file work:
// it runs on any system.
func TestProgramUpdate(t *testing.T) {
	for _, c := range []struct {
		goos, program, newMagic string
		pack                    func(*testing.T, []byte) []byte
	}{
		{"windows", "GVX Desktop.exe", "MZ", zipOf},
		{"linux", "gvx-desktop", "\x7fELF", tarOf},
	} {
		t.Run(c.goos, func(t *testing.T) {
			name, _ := AssetName("v0.6.0", c.goos, "amd64", Program)
			rel := serveRelease(t, name, c.pack(t, []byte(c.newMagic+" new")), "")
			dir := t.TempDir()
			target := filepath.Join(dir, c.program)
			os.WriteFile(target, []byte(c.newMagic+" old"), 0o755)

			var done, total int64
			s, err := stageProgram(context.Background(), rel, target, c.goos, "amd64", func(d, n int64) { done, total = d, n })
			if err != nil {
				t.Fatal(err)
			}
			if done == 0 || done != total {
				t.Errorf("progress ended at %d of %d", done, total)
			}
			fresh, old := programPaths(target, c.goos)
			if got := read(t, fresh); got != c.newMagic+" new" {
				t.Errorf("staged %q", got)
			}
			if got := read(t, target); got != c.newMagic+" old" {
				t.Errorf("staging changed the program: %q", got)
			}
			if st, _ := os.Stat(fresh); runtime.GOOS != "windows" && st.Mode()&0o111 == 0 {
				t.Errorf("the new program is not executable: %v", st.Mode())
			}

			if err := s.Prepare(nil); err != nil {
				t.Fatal(err)
			}
			if got := read(t, target); got != c.newMagic+" new" {
				t.Errorf("after Prepare the program is %q", got)
			}
			if got := read(t, old); got != c.newMagic+" old" {
				t.Errorf("the old program beside it is %q", got)
			}
			if exists(fresh) {
				t.Error("the staged program is still there too")
			}

			// The next start removes the old program and nothing else.
			if err := cleanup(Install{Method: Program, Path: target}, c.goos); err != nil {
				t.Fatal(err)
			}
			entries, _ := os.ReadDir(dir)
			if len(entries) != 1 || entries[0].Name() != c.program {
				var names []string
				for _, e := range entries {
					names = append(names, e.Name())
				}
				t.Errorf("left %v", names)
			}
		})
	}
}

func TestProgramPaths(t *testing.T) {
	for _, c := range []struct{ goos, target, fresh, old string }{
		{"windows", `C:\Apps\GVX Desktop\GVX Desktop.exe`, `C:\Apps\GVX Desktop\GVX Desktop.new.exe`, `C:\Apps\GVX Desktop\GVX Desktop.old.exe`},
		{"windows", `C:\Apps\GVX Desktop\ggfx_driver.dll`, `C:\Apps\GVX Desktop\ggfx_driver.new.dll`, `C:\Apps\GVX Desktop\ggfx_driver.old.dll`},
		{"linux", "/home/me/.local/bin/gvx-desktop", "/home/me/.local/bin/gvx-desktop.new", "/home/me/.local/bin/gvx-desktop.old"},
		{"linux", "/home/me/.local/lib/gvx-desktop/libggfx_driver.so", "/home/me/.local/lib/gvx-desktop/libggfx_driver.so.new", "/home/me/.local/lib/gvx-desktop/libggfx_driver.so.old"},
	} {
		fresh, old := programPaths(c.target, c.goos)
		if fresh != c.fresh || old != c.old {
			t.Errorf("%s: %s, %s", c.target, fresh, old)
		}
	}
}

func TestProgramUpdateWithDriverLibrary(t *testing.T) {
	zipWithDLL := func(t *testing.T, program, dll, docker, buildx, compose []byte) []byte {
		t.Helper()
		var buf bytes.Buffer
		zw := zip.NewWriter(&buf)
		for _, f := range []struct {
			name string
			data []byte
		}{
			{"GVX Desktop.exe", program},
			{"ggfx_driver.dll", dll},
			{"bin/docker.exe", docker},
			{"cli-plugins/docker-buildx.exe", buildx},
			{"cli-plugins/docker-compose.exe", compose},
		} {
			w, _ := zw.Create(f.name)
			w.Write(f.data)
		}
		zw.Close()
		return buf.Bytes()
	}
	tarWithSO := func(t *testing.T, program, so, docker, buildx, compose []byte) []byte {
		t.Helper()
		var buf bytes.Buffer
		gz := gzip.NewWriter(&buf)
		tw := tar.NewWriter(gz)
		for _, d := range []string{"usr/", "usr/bin/", "usr/lib/", "usr/lib/gvx-desktop/", "usr/lib/gvx-desktop/bin/", "usr/lib/gvx-desktop/cli-plugins/"} {
			tw.WriteHeader(&tar.Header{Name: d, Typeflag: tar.TypeDir, Mode: 0o755})
		}
		for _, f := range []struct {
			name string
			mode int64
			data []byte
		}{
			{"usr/bin/gvx-desktop", 0o755, program},
			{"usr/lib/gvx-desktop/libggfx_driver.so", 0o644, so},
			{"usr/lib/gvx-desktop/bin/docker", 0o755, docker},
			{"usr/lib/gvx-desktop/cli-plugins/docker-buildx", 0o755, buildx},
			{"usr/lib/gvx-desktop/cli-plugins/docker-compose", 0o755, compose},
		} {
			tw.WriteHeader(&tar.Header{Name: f.name, Typeflag: tar.TypeReg, Mode: f.mode, Size: int64(len(f.data))})
			tw.Write(f.data)
		}
		tw.Close()
		gz.Close()
		return buf.Bytes()
	}

	for _, c := range []struct {
		goos, magic string
		hadPrevLib  bool
	}{
		{"windows", "MZ", false},
		{"windows", "MZ", true},
		{"linux", "\x7fELF", false},
		{"linux", "\x7fELF", true},
	} {
		label := c.goos + "-fresh-lib"
		if c.hadPrevLib {
			label = c.goos + "-replace-lib"
		}
		t.Run(label, func(t *testing.T) {
			name, _ := AssetName("v0.6.0", c.goos, "amd64", Program)
			var body []byte
			if c.goos == "windows" {
				body = zipWithDLL(t, []byte(c.magic+" new exe"), []byte(c.magic+" new lib"), []byte(c.magic+" new docker"), []byte(c.magic+" new buildx"), []byte(c.magic+" new compose"))
			} else {
				body = tarWithSO(t, []byte(c.magic+" new exe"), []byte(c.magic+" new lib"), []byte(c.magic+" new docker"), []byte(c.magic+" new buildx"), []byte(c.magic+" new compose"))
			}
			rel := serveRelease(t, name, body, "")

			root := t.TempDir()
			var target string
			if c.goos == "windows" {
				target = filepath.Join(root, "GVX Desktop.exe")
			} else {
				binDir := filepath.Join(root, "bin")
				os.MkdirAll(binDir, 0o755)
				target = filepath.Join(binDir, "gvx-desktop")
			}
			os.WriteFile(target, []byte(c.magic+" old exe"), 0o755)
			specs := companionSpecs(target, c.goos)
			libTarget := specs[0].target
			dockerTarget := specs[1].target
			if c.hadPrevLib {
				for _, sp := range specs {
					os.MkdirAll(filepath.Dir(sp.target), 0o755)
					os.WriteFile(sp.target, []byte(c.magic+" old "+filepath.Base(sp.target)), sp.mode)
				}
			}

			s, err := stageProgram(context.Background(), rel, target, c.goos, "amd64", nil)
			if err != nil {
				t.Fatal(err)
			}
			libFresh, libOld := programPaths(libTarget, c.goos)
			if got := read(t, libFresh); got != c.magic+" new lib" {
				t.Fatalf("staged lib = %q", got)
			}

			if err := s.Prepare(nil); err != nil {
				t.Fatal(err)
			}
			if got := read(t, target); got != c.magic+" new exe" {
				t.Errorf("after Prepare exe = %q", got)
			}
			if got := read(t, libTarget); got != c.magic+" new lib" {
				t.Errorf("after Prepare lib = %q", got)
			}
			if got := read(t, dockerTarget); got != c.magic+" new docker" {
				t.Errorf("after Prepare docker = %q", got)
			}
			if got := read(t, specs[2].target); got != c.magic+" new buildx" {
				t.Errorf("after Prepare buildx = %q", got)
			}
			if got := read(t, specs[3].target); got != c.magic+" new compose" {
				t.Errorf("after Prepare compose = %q", got)
			}
			if c.hadPrevLib {
				if got := read(t, libOld); got != c.magic+" old "+filepath.Base(libTarget) {
					t.Errorf("old lib = %q", got)
				}
			} else if exists(libOld) {
				t.Errorf("unexpected old lib at %s", libOld)
			}

			if err := cleanup(Install{Method: Program, Path: target}, c.goos); err != nil {
				t.Fatal(err)
			}
			for _, sp := range specs {
				cf, co := programPaths(sp.target, c.goos)
				if exists(cf) || exists(co) {
					t.Errorf("cleanup left staged/old files for %s", sp.target)
				}
			}
			if got := read(t, libTarget); got != c.magic+" new lib" {
				t.Errorf("cleanup touched live lib: %q", got)
			}
		})
	}
}

func TestStageRefusesAMismatchedChecksum(t *testing.T) {
	name, _ := AssetName("v0.6.0", "linux", "amd64", Program)
	rel := serveRelease(t, name, tarOf(t, []byte("\x7fELF tampered")), strings.Repeat("ab", 32))
	dir := t.TempDir()
	target := filepath.Join(dir, "gvx-desktop")
	os.WriteFile(target, []byte("\x7fELF old"), 0o755)
	_, err := stageProgram(context.Background(), rel, target, "linux", "amd64", nil)
	if err == nil || !strings.Contains(err.Error(), "checksum") {
		t.Fatalf("err = %v", err)
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 1 || read(t, target) != "\x7fELF old" {
		t.Errorf("the folder has %d entries, the program %q", len(entries), read(t, target))
	}
}

func TestStageRefusesAReleaseWithNoChecksum(t *testing.T) {
	name, _ := AssetName("v0.6.0", "windows", "amd64", Program)
	rel := serveRelease(t, name, zipOf(t, []byte("MZ new")), "none")
	target := filepath.Join(t.TempDir(), "GVX Desktop.exe")
	os.WriteFile(target, []byte("MZ old"), 0o755)
	_, err := stageProgram(context.Background(), rel, target, "windows", "amd64", nil)
	if err == nil || !strings.Contains(err.Error(), "no checksum") {
		t.Fatalf("err = %v", err)
	}
}

func TestStageRefusesWhatIsNotAProgram(t *testing.T) {
	name, _ := AssetName("v0.6.0", "windows", "amd64", Program)
	rel := serveRelease(t, name, zipOf(t, []byte("<html>not found</html>")), "")
	dir := t.TempDir()
	target := filepath.Join(dir, "GVX Desktop.exe")
	os.WriteFile(target, []byte("MZ old"), 0o755)
	if _, err := stageProgram(context.Background(), rel, target, "windows", "amd64", nil); err == nil {
		t.Fatal("staged a program that is not one")
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 1 {
		t.Errorf("left %d entries", len(entries))
	}
}

func TestStageWithNoBuild(t *testing.T) {
	_, err := stageProgram(context.Background(), Release{Tag: "v0.6.0"}, filepath.Join(t.TempDir(), "x"), "linux", "riscv64", nil)
	if !errors.Is(err, ErrNoBuild) {
		t.Errorf("no build for the machine: %v", err)
	}
	_, err = stageProgram(context.Background(), Release{Tag: "v0.6.0"}, filepath.Join(t.TempDir(), "x"), "linux", "amd64", nil)
	if !errors.Is(err, ErrNoBuild) {
		t.Errorf("no file in the release: %v", err)
	}
	if _, err := Stage(context.Background(), Release{}, Install{Method: Homebrew}, nil); !errors.Is(err, ErrNotSelf) {
		t.Errorf("Homebrew's install: %v", err)
	}
}

// Every step of a swap can be undone: the program that runs is never left
// missing.
func TestSwapRollsBack(t *testing.T) {
	t.Run("the new program is missing", func(t *testing.T) {
		dir := t.TempDir()
		target, fresh, old := filepath.Join(dir, "app"), filepath.Join(dir, "app.new"), filepath.Join(dir, "app.old")
		os.WriteFile(target, []byte("old"), 0o755)
		if err := swap(target, fresh, old); err == nil {
			t.Fatal("no error")
		}
		if read(t, target) != "old" || exists(old) {
			t.Errorf("the program is %q; app.old there: %v", read(t, target), exists(old))
		}
	})
	t.Run("an earlier update's program will not go", func(t *testing.T) {
		dir := t.TempDir()
		target, fresh, old := filepath.Join(dir, "app"), filepath.Join(dir, "app.new"), filepath.Join(dir, "app.old")
		os.WriteFile(target, []byte("old"), 0o755)
		os.WriteFile(fresh, []byte("new"), 0o755)
		os.MkdirAll(filepath.Join(old, "busy"), 0o755) // a folder with something in it: Remove fails
		if err := swap(target, fresh, old); err == nil {
			t.Fatal("no error")
		}
		if read(t, target) != "old" || read(t, fresh) != "new" {
			t.Errorf("the program is %q, the new one %q", read(t, target), read(t, fresh))
		}
	})
	t.Run("the new program goes in", func(t *testing.T) {
		dir := t.TempDir()
		target, fresh, old := filepath.Join(dir, "app"), filepath.Join(dir, "app.new"), filepath.Join(dir, "app.old")
		os.WriteFile(target, []byte("old"), 0o755)
		os.WriteFile(fresh, []byte("new"), 0o755)
		os.WriteFile(old, []byte("from an earlier update"), 0o755)
		if err := swap(target, fresh, old); err != nil {
			t.Fatal(err)
		}
		if read(t, target) != "new" || read(t, old) != "old" || exists(fresh) {
			t.Errorf("program %q, old %q, new left: %v", read(t, target), read(t, old), exists(fresh))
		}
		// The new one does not start: the old one goes back.
		if err := unswap(target, fresh, old); err != nil {
			t.Fatal(err)
		}
		if read(t, target) != "old" || read(t, fresh) != "new" || exists(old) {
			t.Errorf("after unswap: program %q, new %q, old left: %v", read(t, target), read(t, fresh), exists(old))
		}
	})
}

// A program that does not start is put back: Finish starts the old one.
func TestFinishPutsTheOldProgramBack(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("runs a shell script as the old program")
	}
	dir := t.TempDir()
	target := filepath.Join(dir, "gvx-desktop")
	ran := filepath.Join(dir, "ran")
	oldProgram := "#!/bin/sh\necho started > '" + ran + "'\n"
	os.WriteFile(target, []byte(oldProgram), 0o755)
	fresh, old := programPaths(target, runtime.GOOS)
	os.WriteFile(fresh, []byte("\x7fELF but not a program at all"), 0o755)
	s := &Staged{Method: Program, Target: target, fresh: fresh, old: old, goos: runtime.GOOS}
	if err := s.Prepare(nil); err != nil {
		t.Fatal(err)
	}
	err := s.Finish()
	if err == nil || !strings.Contains(err.Error(), "put back") {
		t.Fatalf("err = %v", err)
	}
	if read(t, target) != oldProgram {
		t.Errorf("the program is %q", read(t, target))
	}
	for range 50 {
		if exists(ran) {
			break
		}
		exec.Command("sleep", "0.1").Run()
	}
	if !exists(ran) {
		t.Error("the old program did not start")
	}
}

func TestDiscard(t *testing.T) {
	dir := t.TempDir()
	fresh := filepath.Join(dir, "app.new")
	os.WriteFile(fresh, []byte("new"), 0o755)
	(&Staged{Method: Program, fresh: fresh}).Discard()
	if exists(fresh) {
		t.Error("a program's update stays")
	}
	stage := filepath.Join(dir, stagePrefix+"1")
	os.MkdirAll(filepath.Join(stage, "GVX Desktop.app", "Contents"), 0o755)
	(&Staged{Method: Bundle, stage: stage}).Discard()
	if exists(stage) {
		t.Error("a bundle's update stays")
	}
}

func TestCleanupOfABundle(t *testing.T) {
	apps := t.TempDir()
	bundle := filepath.Join(apps, "GVX Desktop.app")
	os.MkdirAll(filepath.Join(bundle, "Contents"), 0o755)
	os.MkdirAll(filepath.Join(apps, stagePrefix+"123", "GVX Desktop.app", "Contents"), 0o755)
	os.MkdirAll(filepath.Join(apps, "Other.app"), 0o755)
	if err := cleanup(Install{Method: Bundle, Path: bundle}, "darwin"); err != nil {
		t.Fatal(err)
	}
	entries, _ := os.ReadDir(apps)
	if len(entries) != 2 || !exists(bundle) || !exists(filepath.Join(apps, "Other.app")) {
		t.Errorf("left %d entries", len(entries))
	}
}

func TestFindApp(t *testing.T) {
	dir := t.TempDir()
	if _, err := findApp(dir); err == nil {
		t.Error("found an app in an empty image")
	}
	os.MkdirAll(filepath.Join(dir, "GVX Desktop.app", "Contents"), 0o755)
	os.Symlink("/Applications", filepath.Join(dir, "Applications"))
	os.WriteFile(filepath.Join(dir, ".DS_Store"), nil, 0o644)
	if got, err := findApp(dir); err != nil || got != filepath.Join(dir, "GVX Desktop.app") {
		t.Errorf("%q, %v", got, err)
	}
	os.MkdirAll(filepath.Join(dir, "Another.app"), 0o755)
	if _, err := findApp(dir); err == nil {
		t.Error("chose between two apps")
	}
}

func TestHelperScript(t *testing.T) {
	got := helperScript(4242, "/Applications/GVX Desktop.app", "/Applications/.gvx-desktop-update-1/GVX Desktop.app",
		"/Applications/.gvx-desktop-update-1", "/usr/bin/open", []string{"-data-dir", "/Users/me/it's here"})
	for _, want := range []string{
		"#!/bin/sh\n",
		"\npid=4242\n",
		"\napp='/Applications/GVX Desktop.app'\n",
		"\nnew='/Applications/.gvx-desktop-update-1/GVX Desktop.app'\n",
		"\nstage=/Applications/.gvx-desktop-update-1\n",
		`while kill -0 "$pid" 2>/dev/null; do`,
		`if [ "$waited" -ge 600 ]; then`,
		`if mv "$app" "$old"; then`,
		`if ! mv "$new" "$app"; then`,
		`mv "$old" "$app"`,
		`rm -rf "$stage"`,
		"\n/usr/bin/open \"$app\" --args -data-dir '/Users/me/it'\\''s here'\n",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("the script has no %q:\n%s", want, got)
		}
	}
	// The old bundle moves aside before the new one moves in, and the
	// stage, which holds the old one, goes only once an app is in place.
	if strings.Index(got, `mv "$app" "$old"`) > strings.Index(got, `mv "$new" "$app"`) {
		t.Error("the new app moves in before the old one moves out")
	}
	if !strings.Contains(got, "if [ -d \"$app\" ]; then\n\trm -rf \"$stage\"") {
		t.Error("the stage is removed whether or not an app is in place")
	}
	if strings.Contains(helperScript(1, "/a.app", "/s/a.app", "/s", "/usr/bin/open", nil), "--args") {
		t.Error("--args with no arguments")
	}
}

// The helper script, run against made-up bundles in a temporary folder,
// swaps them and opens the new one; when the new one cannot move in, the
// old one goes back and opens.
func TestHelperScriptSwapsBundles(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("needs a POSIX shell")
	}
	// A process that has ended, for the script to find gone.
	done := exec.Command("true")
	if err := done.Run(); err != nil {
		t.Skip(err)
	}
	pid := done.Process.Pid

	setup := func(t *testing.T, withNew bool) (apps, app, stage, opened, script string) {
		apps = t.TempDir()
		app = filepath.Join(apps, "GVX Desktop.app")
		os.MkdirAll(filepath.Join(app, "Contents"), 0o755)
		os.WriteFile(filepath.Join(app, "Contents", "version"), []byte("old"), 0o644)
		stage = filepath.Join(apps, stagePrefix+"1")
		fresh := filepath.Join(stage, "GVX Desktop.app")
		os.MkdirAll(stage, 0o755)
		if withNew {
			os.MkdirAll(filepath.Join(fresh, "Contents"), 0o755)
			os.WriteFile(filepath.Join(fresh, "Contents", "version"), []byte("new"), 0o644)
		}
		bin := t.TempDir()
		opened = filepath.Join(bin, "opened")
		open := filepath.Join(bin, "open")
		os.WriteFile(open, []byte("#!/bin/sh\necho \"$@\" > '"+opened+"'\n"), 0o755)
		script = filepath.Join(stage, "finish.sh")
		os.WriteFile(script, []byte(helperScript(pid, app, fresh, stage, open, []string{"-x"})), 0o700)
		return
	}
	t.Run("in place", func(t *testing.T) {
		apps, app, stage, opened, script := setup(t, true)
		if out, err := exec.Command("/bin/sh", script).CombinedOutput(); err != nil {
			t.Fatalf("%v: %s", err, out)
		}
		if got := read(t, filepath.Join(app, "Contents", "version")); got != "new" {
			t.Errorf("the app is %q", got)
		}
		if exists(stage) {
			t.Error("the stage is still there")
		}
		if got := read(t, opened); got != app+" --args -x\n" {
			t.Errorf("opened %q", got)
		}
		if entries, _ := os.ReadDir(apps); len(entries) != 1 {
			t.Errorf("%d entries in the folder", len(entries))
		}
	})
	t.Run("rolled back", func(t *testing.T) {
		_, app, _, opened, script := setup(t, false)
		exec.Command("/bin/sh", script).Run()
		if got := read(t, filepath.Join(app, "Contents", "version")); got != "old" {
			t.Errorf("the app is %q", got)
		}
		if got := read(t, opened); got != app+" --args -x\n" {
			t.Errorf("opened %q", got)
		}
	})
}
