package ui

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/ironpark/ggui"

	"github.com/ironpark/gvx-desktop/internal/store"
)

func TestBundledDockerCLIFallbackAndPathToggle(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uses POSIX shell script stub")
	}
	home := t.TempDir()
	bundleDir := t.TempDir()
	for _, rel := range []string{"docker", "cli-plugins/docker-buildx", "cli-plugins/docker-compose"} {
		p := filepath.Join(bundleDir, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("#!/bin/sh\necho ok\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("HOME", home)
	t.Setenv("SHELL", "/bin/zsh")
	t.Setenv("GVX_DOCKER_CLI_DIR", bundleDir)
	t.Setenv("PATH", t.TempDir())

	st := probeBundledDockerState("")
	if !st.Available || !st.HasBuildx || !st.HasCompose {
		t.Fatalf("probeBundledDockerState() = %+v, want bundled docker+buildx+compose", st)
	}
	if st.ToolsList() != "docker, buildx, compose" {
		t.Errorf("ToolsList() = %q", st.ToolsList())
	}

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

	m.BundledDocker.Set(BundledDockerState{
		Available:  true,
		HasBuildx:  true,
		HasCompose: true,
		Using:      true,
		CLIPath:    filepath.Join(home, ".gvx", "bin", "docker"),
		BinDir:     filepath.Join(home, ".gvx", "bin"),
		Profile:    filepath.Join(home, ".zshrc"),
		InPATH:     false,
	})
	hasText := func(want string) bool {
		for _, n := range p.Semantics().All() {
			if strings.Contains(n.Name, want) {
				return true
			}
		}
		return false
	}

	m.Page.Set(PageDocker)
	p.Frame()
	if !hasText("Bundled Docker CLI (docker, buildx, compose)") || !hasText("Add to PATH") {
		t.Errorf("Docker page missing bundled CLI PATH banner")
	}

	m.Page.Set(PageSettings)
	p.Frame()
	if !hasText("Bundled Docker CLI") || !hasText("Add to PATH") {
		t.Errorf("Settings page missing bundled Docker CLI row")
	}

	m.Offline = false
	m.AddBundledDockerToPATH()
	until(t, p, "AddBundledDockerToPATH", func() bool {
		s := ggui.Peek(m.BundledDocker)
		return !s.Busy && s.InPATH && s.Managed
	})
	if data, err := os.ReadFile(filepath.Join(home, ".zshrc")); err != nil || !strings.Contains(string(data), ".gvx/bin") {
		t.Fatalf("zshrc after AddBundledDockerToPATH = %q, err=%v", data, err)
	}
	m.RemoveBundledDockerFromPATH()
	until(t, p, "RemoveBundledDockerFromPATH", func() bool {
		s := ggui.Peek(m.BundledDocker)
		return !s.Busy && !s.InPATH && !s.Managed
	})
}
