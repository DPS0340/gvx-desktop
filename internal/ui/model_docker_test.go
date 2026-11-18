package ui

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/ironpark/ggui"

	"github.com/ironpark/gvx-desktop/internal/recipes"
	"github.com/ironpark/gvx-desktop/internal/sandbox"
	"github.com/ironpark/gvx-desktop/internal/store"
)

// The engine's endpoint is the settings' until there is an engine, and the
// engine's sandbox's from then on; changing it moves both, and is saved.
func TestSetDockerAPIMovesTheSettingAndTheSandbox(t *testing.T) {
	dir := t.TempDir()
	st := store.Store{Dir: dir}
	var m *Model
	p := ggui.ProbeBuilder(func() ggui.Widget { return Build(m) }, ggui.Sz(900, 700))
	p.Setup(func() {
		m = NewModel(st, dir)
		m.Offline = true
		m.Attach(p)
	})
	defer p.Close()
	p.Frame()

	if got, want := m.DockerHost(), recipes.DefaultDockerAPI().Host(); got != want {
		t.Fatalf("with no engine and no setting: %q, want %q", got, want)
	}
	m.SetDockerAPI(recipes.DockerAPI{TCP: true, Port: 23750})
	p.Frame()
	if got := m.DockerHost(); got != "tcp://127.0.0.1:23750" {
		t.Errorf("the setting alone: %q", got)
	}

	// An engine made now publishes as the setting says.
	spec := recipes.DockerEngineWith(m.dockerSetting())
	m.saveSpecs([]sandbox.Spec{spec})
	p.Frame()
	if spec.Ports[0] != "127.0.0.1:23750:2375" {
		t.Fatalf("a new engine publishes %q", spec.Ports[0])
	}

	// Moving it rewrites the sandbox's port and nothing else of it.
	m.SetDockerAPI(recipes.DockerAPI{Port: 23750})
	p.Frame()
	saved, err := st.Sandboxes()
	if err != nil || len(saved) != 1 {
		t.Fatalf("saved sandboxes: %v, %v", saved, err)
	}
	if got := saved[0].Ports; len(got) != 2 || got[0] != "unix:~/.gvx/docker.sock:2375" || got[1] != spec.Ports[1] {
		t.Errorf("saved ports %v", got)
	}
	if set, _ := st.Settings(); set.DockerEndpoint != DockerSocketEndpoint {
		t.Errorf("saved endpoint %q", set.DockerEndpoint)
	}
	if api := m.DockerAPI(); api.TCP {
		t.Errorf("the model still says %+v", api)
	}
}

// A docker path in the settings is used as given, and one that names
// nothing is not quietly replaced by a docker found elsewhere.
func TestDockerCLIFollowsTheSetting(t *testing.T) {
	dir := t.TempDir()
	var m *Model
	p := ggui.ProbeBuilder(func() ggui.Widget { return Build(m) }, ggui.Sz(900, 700))
	p.Setup(func() {
		m = NewModel(store.Store{Dir: dir}, dir)
		m.Offline = true
		m.Attach(p)
	})
	defer p.Close()
	p.Frame()

	mine := filepath.Join(dir, "my-docker")
	os.WriteFile(mine, []byte("#!/bin/sh\n"), 0o755)
	m.UpdateSettings(func(s *store.Settings) { s.DockerPath = mine })
	if got, ok := m.DockerCLI(); !ok || got != mine {
		t.Errorf("a named docker: %q, %v", got, ok)
	}
	m.UpdateSettings(func(s *store.Settings) { s.DockerPath = filepath.Join(dir, "gone") })
	if got, ok := m.DockerCLI(); ok {
		t.Errorf("a path that names nothing found %q", got)
	}
}
