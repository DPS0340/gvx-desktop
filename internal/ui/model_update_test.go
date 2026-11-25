package ui

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ironpark/ggui"

	"github.com/ironpark/gvx-desktop/internal/rungv"
	"github.com/ironpark/gvx-desktop/internal/store"
)

func TestHasProgramPrefixForSandboxStderr(t *testing.T) {
	for _, c := range []struct {
		line string
		want bool
	}{
		{"rungv: platform=hvf", true},
		{"rungvx: platform=hvf", true},
		{"rungvx: publishing 127.0.0.1:8080 -> 80/tcp", true},
		{"rungv run: gvx: executable /nope not found", false},
		{"rungvx run: gvx: executable /nope not found", false},
		{"rungvy: something else", false},
		{"error: boom", false},
	} {
		if got := hasProgramPrefix(c.line, ":"); got != c.want {
			t.Errorf("hasProgramPrefix(%q, \":\") = %v, want %v", c.line, got, c.want)
		}
	}
}

func TestSelectAndInstallSpecificRungvRelease(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("USERPROFILE", os.Getenv("HOME"))
	dir := t.TempDir()

	rel23 := rungv.Release{Tag: "v0.23.10022026112300", Page: "https://example.com/23"}
	rel22 := rungv.Release{Tag: "v0.22.10002026111600", Page: "https://example.com/22"}
	relPre := rungv.Release{Tag: "v0.24.0-rc1", Page: "https://example.com/24", Prerelease: true}

	oldReleases := fetchRungvReleases
	oldLatest := fetchRungvLatest
	oldInstall := installRungvRelease
	oldSign := signRungvBinary
	t.Cleanup(func() {
		fetchRungvReleases = oldReleases
		fetchRungvLatest = oldLatest
		installRungvRelease = oldInstall
		signRungvBinary = oldSign
	})

	fetchRungvReleases = func(context.Context) ([]rungv.Release, error) {
		return []rungv.Release{relPre, rel23, rel22}, nil
	}
	fetchRungvLatest = func(context.Context) (rungv.Release, error) {
		return rel23, nil
	}
	signRungvBinary = func(context.Context, string) error { return nil }
	var installedTag string
	installRungvRelease = func(_ context.Context, r rungv.Release, target string, progress func(done, total int64)) error {
		installedTag = r.Tag
		if progress != nil {
			progress(50, 100)
			progress(100, 100)
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}
		return os.WriteFile(target, []byte("#!/bin/sh\necho 'rungv "+r.Tag+"'\n"), 0o755)
	}

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
		m.Attach(p)
		m.Engine.Set(Engine{
			Path:    filepath.Join(dir, "rungv"),
			Checked: true,
			Version: rungv.Version{Program: "rungvx", Version: rel23.Tag},
			Info:    rungv.Info{OS: "linux", AvailableBackends: []string{"kvm"}, DefaultBackend: "kvm"},
		})
	})
	defer p.Close()
	p.Frame()

	waitFor := func(desc string, cond func() bool) {
		t.Helper()
		deadline := time.Now().Add(3 * time.Second)
		for time.Now().Before(deadline) {
			p.Frame()
			if cond() {
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
		t.Fatalf("timed out waiting for %s", desc)
	}

	m.CheckUpdate(true)
	waitFor("releases list loaded", func() bool {
		return len(ggui.Peek(m.Releases)) == 3 && !ggui.Peek(m.Update).Checking
	})

	// Newest stable release (not prerelease) should be chosen as latest.
	if got := ggui.Peek(m.Update).Tag; got != rel23.Tag {
		t.Fatalf("Update.Tag = %q, want %q", got, rel23.Tag)
	}

	// Open Settings and confirm Release version selector appears.
	m.Go(PageSettings)
	p.Frame()
	if sem := p.Semantics().String(); !strings.Contains(sem, "Release version") {
		t.Fatalf("Settings missing Release version selector:\n%s", sem)
	}

	// Select an older release (downgrade path).
	m.SetSelectedRelease(rel22.Tag)
	p.Frame()
	if sem := p.Semantics().String(); !strings.Contains(sem, "older than v0.23.1002") || !strings.Contains(sem, "Install v0.22.1000") {
		t.Fatalf("Settings downgrade state unexpected:\n%s", sem)
	}

	// Select the currently installed release -> Reinstall state.
	m.SetSelectedRelease(rel23.Tag)
	p.Frame()
	if sem := p.Semantics().String(); !strings.Contains(sem, "is currently installed") || !strings.Contains(sem, "Reinstall") {
		t.Fatalf("Settings installed state unexpected:\n%s", sem)
	}

	// Install the older release explicitly and confirm Update.Tag stays at latest.
	m.SetSelectedRelease(rel22.Tag)
	m.InstallRungvRelease(rel22.Tag)
	waitFor("install finished", func() bool {
		return installedTag == rel22.Tag && !ggui.Peek(m.Update).Installing
	})
	if got := ggui.Peek(m.Update).Tag; got != rel23.Tag {
		t.Fatalf("Update.Tag changed after installing older release: got %q, want %q", got, rel23.Tag)
	}

	// Fallback to fetchRungvLatest when fetchRungvReleases fails.
	fetchRungvReleases = func(context.Context) ([]rungv.Release, error) {
		return nil, errors.New("list unavailable")
	}
	m.CheckUpdate(true)
	waitFor("fallback latest loaded", func() bool {
		return len(ggui.Peek(m.Releases)) == 1 && !ggui.Peek(m.Update).Checking
	})
	if got := ggui.Peek(m.SelectedRelease); got != "" {
		t.Fatalf("SelectedRelease = %q after disappearing from release list, want empty", got)
	}

	// Switching to source channel clears selected release.
	m.SelectedRelease.Set(rel23.Tag)
	m.Offline = true
	m.SetChannel(ChannelSource)
	if got := ggui.Peek(m.SelectedRelease); got != "" {
		t.Fatalf("SelectedRelease = %q after switching to source channel, want empty", got)
	}
}
