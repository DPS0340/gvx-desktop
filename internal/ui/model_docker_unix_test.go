//go:build !windows

package ui

import (
	"testing"
	"time"

	"github.com/ironpark/ggui"

	"github.com/ironpark/gvx-desktop/internal/recipes"
	"github.com/ironpark/gvx-desktop/internal/sandbox"
	"github.com/ironpark/gvx-desktop/internal/store"
)

// Changing shared folders in settings while the Docker Engine is running does
// not change what the running sandbox actually shares, so the proxy Check hook
// must continue judging against the spec the engine started with until it stops.
func TestCheckUsesRunningEngineSpecWhileActive(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	dir := t.TempDir()
	st := store.Store{Dir: dir}
	rungvPath, _ := fakeRungv(t)

	// Synthetic non-reserved paths (fakeRungv does not actually mount them).
	pathA := "/srv/gvx-test-a"
	pathB := "/srv/gvx-test-b"

	engine := recipes.WithSharedFolders(recipes.DockerEngine(), []string{pathA})
	if err := st.SaveSandboxes([]sandbox.Spec{engine}); err != nil {
		t.Fatal(err)
	}

	m, p := launch(t, st, rungvPath)
	t.Cleanup(func() { m.sup.StopAll(2 * time.Second) })

	m.StartSandbox(engine.ID)
	until(t, p, "engine running", func() bool {
		return m.sup.State(engine.ID).Status == sandbox.Running
	})

	// While running with pathA shared, change settings/saved spec to pathB.
	m.SetSharedFolders([]string{pathB})
	p.Frame()

	bindA := []byte(`{"HostConfig":{"Binds":["` + pathA + `/app:/app"]}}`)
	bindB := []byte(`{"HostConfig":{"Binds":["` + pathB + `/app:/app"]}}`)

	if err := m.checkContainerCreate(nil, bindA); err != nil {
		t.Fatalf("while running, started folder %s was rejected: %v", pathA, err)
	}
	if err := m.checkContainerCreate(nil, bindB); err == nil {
		t.Fatalf("while running, newly saved (unstarted) folder %s was allowed", pathB)
	}

	// Stop the engine: Check must now fall back to the saved spec (pathB).
	m.StopSandbox(engine.ID)
	until(t, p, "engine stopped", func() bool {
		return !m.sup.State(engine.ID).Status.Active() && ggui.Peek(m.States)[engine.ID].Status == sandbox.Stopped
	})
	p.Frame()

	if err := m.checkContainerCreate(nil, bindB); err != nil {
		t.Fatalf("after stop, saved folder %s was rejected: %v", pathB, err)
	}
	if err := m.checkContainerCreate(nil, bindA); err == nil {
		t.Fatalf("after stop, removed folder %s was still allowed", pathA)
	}
}
