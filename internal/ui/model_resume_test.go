//go:build !windows

package ui

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/ironpark/ggui"

	"github.com/ironpark/gvx-desktop/internal/recipes"
	"github.com/ironpark/gvx-desktop/internal/rungv"
	"github.com/ironpark/gvx-desktop/internal/sandbox"
	"github.com/ironpark/gvx-desktop/internal/store"
)

// fakeRungv writes a rungv that runs no sandbox: it says the Docker
// Engine's ready line and waits to be stopped, or, while a file named fail
// is beside it, fails at once.
func fakeRungv(t *testing.T) (path string, fail func(bool)) {
	t.Helper()
	dir := t.TempDir()
	path = filepath.Join(dir, "rungv")
	script := "#!/bin/sh\ncase \"$*\" in *-help*) exit 2 ;; esac\n[ -f '" + filepath.Join(dir, "fail") + "' ] && { echo 'rungv: boom' >&2; exit 3; }\n" +
		"echo 'API listen on /var/run/docker.sock'\nexec sleep 60\n"
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return path, func(on bool) {
		if on {
			os.WriteFile(filepath.Join(dir, "fail"), nil, 0o644)
		} else {
			os.Remove(filepath.Join(dir, "fail"))
		}
	}
}

// launch builds the app's model on st as a launch does, with rungv ready.
func launch(t *testing.T, st store.Store, rungvPath string) (*Model, *ggui.Probe) {
	t.Helper()
	var m *Model
	p := ggui.ProbeBuilder(func() ggui.Widget { return Build(m) }, ggui.Sz(900, 700))
	p.Setup(func() {
		m = NewModel(st, st.Dir)
		m.Offline = true
		m.Attach(p)
		m.client = &rungv.Client{Path: rungvPath}
		m.Engine.Set(Engine{Path: rungvPath, Checked: true, Info: rungv.Info{AvailableBackends: []string{"fake"}}})
	})
	t.Cleanup(func() {
		m.Supervisor().StopAll(5 * time.Second)
		p.Close()
	})
	p.Frame()
	return m, p
}

func resumed(m *Model) []string { return ggui.Peek(m.Settings).Resume }

// The Docker Engine the user left running starts again with the app, and
// one the user stopped does not. Quitting, which stops it, leaves it to
// start again; so does an engine that ended after it was up, and one that
// never came up is not tried again.
func TestResumeTheEngine(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	rungvPath, fail := fakeRungv(t)
	st := store.Store{Dir: t.TempDir()}
	web := sandbox.Spec{ID: "web00001", Name: "web", Image: "nginx", CreatedAt: time.Now()}
	if err := st.SaveSandboxes([]sandbox.Spec{web}); err != nil {
		t.Fatal(err)
	}

	m, p := launch(t, st, rungvPath)
	m.CreateFromRecipe(recipes.Docker)
	engine, _ := m.RecipeSandbox(ggui.Peek(m.Rows), recipes.Docker)
	id := engine.Spec.ID
	until(t, p, "the engine to be ready", func() bool { return m.sup.State(id).Status == sandbox.Running })
	if !slices.Equal(resumed(m), []string{id}) {
		t.Fatalf("started by the user: remembered %v", resumed(m))
	}
	// Any other sandbox is the user's to set to start with the app.
	m.StartSandbox(web.ID)
	if slices.Contains(resumed(m), web.ID) {
		t.Error("a sandbox of no recipe was remembered")
	}

	// A restart and the stop that quitting makes leave it remembered.
	m.RestartSandbox(id)
	until(t, p, "the restart", func() bool { st := m.sup.State(id); return st.Status == sandbox.Running && st.PID != 0 })
	quit := false
	m.StopAllAndThen(func() { quit = true })
	until(t, p, "the quit", func() bool { return quit })
	if !slices.Equal(resumed(m), []string{id}) {
		t.Fatalf("after quitting: remembered %v", resumed(m))
	}

	// The next launch starts it once rungv is ready.
	m, p = launch(t, st, rungvPath)
	if got := launchStarts(ggui.Peek(m.Specs), resumed(m)); !slices.Equal(got, []string{id}) {
		t.Fatalf("starts %v at launch", got)
	}
	m.startWithApp()
	until(t, p, "the engine to start again", func() bool { return m.sup.State(id).Status == sandbox.Running })
	if m.sup.State(web.ID).Status.Active() {
		t.Error("the other sandbox started too")
	}

	// It ends by itself after it was up, as a logout may end it: kept.
	proc, _ := os.FindProcess(m.sup.State(id).PID)
	proc.Kill()
	// The model hears of it, and tells of it, on the UI thread.
	until(t, p, "the end to be told", func() bool { return ggui.Peek(m.States)[id].Status == sandbox.Failed })
	if !slices.Equal(resumed(m), []string{id}) {
		t.Errorf("ended after it was up: remembered %v", resumed(m))
	}

	// The user stops it: forgotten, and not started at the next launch.
	m.StartSandbox(id)
	until(t, p, "the engine to start", func() bool { return m.sup.State(id).Status == sandbox.Running })
	m.StopSandbox(id)
	if len(resumed(m)) != 0 {
		t.Fatalf("stopped by the user: remembered %v", resumed(m))
	}
	until(t, p, "the engine to stop", func() bool { return !m.sup.State(id).Status.Active() })
	m, p = launch(t, st, rungvPath)
	if got := launchStarts(ggui.Peek(m.Specs), resumed(m)); len(got) != 0 {
		t.Errorf("starts %v at launch after the user stopped it", got)
	}

	// The user's Stop All forgets it as well.
	m.StartSandbox(id)
	until(t, p, "the engine to start", func() bool { return m.sup.State(id).Status == sandbox.Running })
	m.StopAll()
	if len(resumed(m)) != 0 {
		t.Errorf("after Stop All: remembered %v", resumed(m))
	}
	until(t, p, "the engine to stop", func() bool { return !m.sup.State(id).Status.Active() })

	// One that fails before it is ever up is forgotten, so as not to fail
	// at every launch.
	fail(true)
	m.StartSandbox(id)
	if !slices.Equal(resumed(m), []string{id}) {
		t.Fatalf("started: remembered %v", resumed(m))
	}
	until(t, p, "the engine to fail", func() bool { return m.sup.State(id).Status == sandbox.Failed })
	until(t, p, "the failure to be told", func() bool { return len(resumed(m)) == 0 })
	fail(false)

	// The Kubernetes recipe is remembered as well, and a deleted sandbox is
	// forgotten.
	m.CreateFromRecipe(recipes.Kubernetes)
	k3s, _ := m.RecipeSandbox(ggui.Peek(m.Rows), recipes.Kubernetes)
	if !slices.Contains(resumed(m), k3s.Spec.ID) {
		t.Errorf("k3s started: remembered %v", resumed(m))
	}
	m.AskDelete(k3s.Spec.ID)
	m.RunConfirmed()
	until(t, p, "the delete", func() bool { _, ok := m.spec(k3s.Spec.ID); return !ok })
	if slices.Contains(resumed(m), k3s.Spec.ID) {
		t.Errorf("deleted: remembered %v", resumed(m))
	}
}

// A quit asked again while the first one stops the sandboxes, as Homebrew
// asks until the app is gone, stops nothing more: the first quit ends the app.
func TestQuitAskedAgainWhileQuitting(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	rungvPath, _ := fakeRungv(t)
	m, p := launch(t, store.Store{Dir: t.TempDir()}, rungvPath)
	m.CreateFromRecipe(recipes.Docker)
	engine, _ := m.RecipeSandbox(ggui.Peek(m.Rows), recipes.Docker)
	until(t, p, "the engine to be ready", func() bool { return m.sup.State(engine.Spec.ID).Status == sandbox.Running })

	first, again := 0, 0
	m.StopAllAndThen(func() { first++ })
	m.StopAllAndThen(func() { again++ })
	until(t, p, "the quit", func() bool { return first > 0 })
	for deadline := time.Now().Add(300 * time.Millisecond); time.Now().Before(deadline); {
		time.Sleep(5 * time.Millisecond)
		p.Frame()
	}
	if first != 1 || again != 0 {
		t.Fatalf("the first quit ended %d times, the second %d times", first, again)
	}
}
