package ui

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/ironpark/ggui"

	"github.com/ironpark/gvx-desktop/internal/store"
)

func TestRungvPathRegistrationInSettings(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uses POSIX shell script stub")
	}
	home := t.TempDir()
	customDir := t.TempDir()
	customRungv := filepath.Join(customDir, "rungv")
	if err := os.WriteFile(customRungv, []byte("#!/bin/sh\necho ok\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	t.Setenv("HOME", home)
	t.Setenv("SHELL", "/bin/zsh")
	t.Setenv("PATH", t.TempDir())
	t.Setenv("GVX_DOCKER_CLI_DIR", "none")

	oldLook := lookPathRungv
	lookPathRungv = func(string) (string, error) { return "", exec.ErrNotFound }
	t.Cleanup(func() { lookPathRungv = oldLook })

	dir := t.TempDir()
	stStore := store.Store{Dir: dir}
	set := store.DefaultSettings()
	set.Language = LangEnglish
	if err := stStore.SaveSettings(set); err != nil {
		t.Fatal(err)
	}
	var m *Model
	p := ggui.ProbeBuilder(func() ggui.Widget {
		return ggui.Provide(ggui.ReducedMotionKey, true, Build(m))
	}, ggui.Sz(1280, 900))
	p.Setup(func() {
		m = NewModel(stStore, dir)
		m.Offline = true
		m.Attach(p)
	})
	t.Cleanup(p.Close)
	p.Frame()

	hasText := func(want string) bool {
		for _, n := range p.Semantics().All() {
			if strings.Contains(n.Name, want) {
				return true
			}
		}
		return false
	}

	// Simulate an active external rungv binary outside ~/.gvx/bin and not on PATH.
	m.Engine.Set(Engine{Path: customRungv, Checked: true})
	m.RungvPath.Set(probeRungvPathState(customRungv))
	m.Page.Set(PageSettings)
	p.Frame()

	if !hasText("Terminal PATH") || !hasText("Add to PATH") {
		t.Fatalf("Settings → rungv missing Terminal PATH / Add to PATH row:\n%s", p.Semantics())
	}

	// Registering links customRungv into ~/.gvx/bin/rungv and writes ~/.zshrc.
	m.Offline = false
	m.AddRungvToPATH()
	until(t, p, "AddRungvToPATH", func() bool {
		s := ggui.Peek(m.RungvPath)
		return !s.Busy && s.BinInPATH && s.InBinDir && s.Managed
	})

	managed := filepath.Join(home, ".gvx", "bin", "rungv")
	if target, err := os.Readlink(managed); err != nil || target != customRungv {
		t.Fatalf("managed rungv symlink = (%q, %v), want %q", target, err, customRungv)
	}
	if data, err := os.ReadFile(filepath.Join(home, ".zshrc")); err != nil || !strings.Contains(string(data), ".gvx/bin") {
		t.Fatalf("zshrc after AddRungvToPATH = %q, err=%v", data, err)
	}
	// This stand-in is no rungvx, so it gets no rungvx link.
	if _, err := os.Lstat(filepath.Join(home, ".gvx", "bin", "rungvx")); !os.IsNotExist(err) {
		t.Fatalf("rungvx linked for a binary that is not one (%v)", err)
	}
	p.Frame()
	if !hasText("Remove from PATH") {
		t.Fatalf("Settings → rungv missing Remove from PATH button after adding")
	}

	// Removing cleans the managed ~/.gvx/bin entry from ~/.zshrc.
	m.RemoveRungvFromPATH()
	until(t, p, "RemoveRungvFromPATH", func() bool {
		s := ggui.Peek(m.RungvPath)
		return !s.Busy && !s.BinInPATH && !s.Managed
	})
	if data, err := os.ReadFile(filepath.Join(home, ".zshrc")); err != nil || strings.Contains(string(data), ".gvx/bin") {
		t.Fatalf("zshrc still contains .gvx/bin after RemoveRungvFromPATH: %q (err=%v)", data, err)
	}
}

// A rungvx in ~/.gvx/bin is also there as rungvx, the name gvx gives it
// since v0.23; an older rungv is not, and a rungvx the user put there stays.
func TestLinkRungvx(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uses POSIX shell script stubs")
	}
	home := t.TempDir()
	t.Setenv("HOME", home)
	bin := filepath.Join(home, ".gvx", "bin")
	own, link := filepath.Join(bin, "rungv"), filepath.Join(bin, "rungvx")
	stub := func(first string) {
		t.Helper()
		if err := os.MkdirAll(bin, 0o755); err != nil {
			t.Fatal(err)
		}
		script := "#!/bin/sh\necho '" + first + "'\necho 'commit 52e9405 (2026-11-25T05:53:27Z)'\n"
		if err := os.WriteFile(own, []byte(script), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	ctx := context.Background()

	if err := linkRungvx(ctx); err != nil {
		t.Fatalf("no rungv: %v", err)
	}
	if _, err := os.Lstat(link); !os.IsNotExist(err) {
		t.Fatalf("no rungv: rungvx is there (%v)", err)
	}

	stub("rungv v0.21.10012026111600")
	if err := linkRungvx(ctx); err != nil {
		t.Fatalf("rungv v0.21: %v", err)
	}
	if _, err := os.Lstat(link); !os.IsNotExist(err) {
		t.Fatalf("rungv v0.21: rungvx is there (%v)", err)
	}

	stub("rungvx v0.23.10022026112300")
	for range 2 { // again, as after the next update: still the one link
		if err := linkRungvx(ctx); err != nil {
			t.Fatalf("rungvx: %v", err)
		}
		if target, err := os.Readlink(link); err != nil || target != own {
			t.Fatalf("rungvx link = (%q, %v), want %q", target, err, own)
		}
	}

	if err := os.Remove(link); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(link, []byte("the user's own"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := linkRungvx(ctx); err != nil {
		t.Fatalf("the user's rungvx: %v", err)
	}
	if data, _ := os.ReadFile(link); string(data) != "the user's own" {
		t.Fatalf("the user's rungvx was replaced: %q", data)
	}
}
