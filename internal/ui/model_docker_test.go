package ui

import (
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ironpark/ggui"

	"github.com/ironpark/gvx-desktop/internal/recipes"
	"github.com/ironpark/gvx-desktop/internal/sandbox"
	"github.com/ironpark/gvx-desktop/internal/store"
)

// The user-facing Docker endpoint comes from settings only; changing it saves
// the setting and leaves the engine sandbox's private backend publish untouched.
func TestSetDockerAPIUpdatesSettingOnly(t *testing.T) {
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

	// An engine always publishes on the app-private backend socket.
	spec := recipes.DockerEngine()
	m.saveSpecs([]sandbox.Spec{spec})
	p.Frame()
	if spec.Ports[0] != recipes.DockerBackendPublish {
		t.Fatalf("a new engine publishes %q", spec.Ports[0])
	}

	// Moving the user-facing endpoint leaves the sandbox spec untouched.
	m.SetDockerAPI(recipes.DockerAPI{Port: 23750})
	p.Frame()
	saved, err := st.Sandboxes()
	if err != nil || len(saved) != 1 {
		t.Fatalf("saved sandboxes: %v, %v", saved, err)
	}
	if got := saved[0].Ports; len(got) != 2 || got[0] != recipes.DockerBackendPublish || got[1] != spec.Ports[1] {
		t.Errorf("saved ports %v", got)
	}
	if set, _ := st.Settings(); set.DockerEndpoint != DockerSocketEndpoint {
		t.Errorf("saved endpoint %q", set.DockerEndpoint)
	}
	if api := m.DockerAPI(); api.TCP {
		t.Errorf("the model still says %+v", api)
	}
}

func TestCheckCreateBinds(t *testing.T) {
	engine := recipes.WithSharedFolders(recipes.DockerEngine(), []string{"/Users/me"})
	for _, tc := range []struct {
		name    string
		body    string
		wantErr string
	}{
		{"shared bind", `{"HostConfig":{"Binds":["/Users/me/app:/app:ro"]}}`, ""},
		{"named volume", `{"HostConfig":{"Binds":["pgdata:/var/lib/postgresql/data"]}}`, ""},
		{"sandbox docker sock", `{"HostConfig":{"Binds":["/var/run/docker.sock:/var/run/docker.sock"]}}`, ""},
		{"unshared bind", `{"HostConfig":{"Binds":["/Volumes/work/data:/data"]}}`,
			"/Volumes/work/data is not in a folder shared with the Docker Engine. Share it in GVX Desktop → Settings → Docker, restart the engine, and run this again."},
		{"unshared mount bind", `{"HostConfig":{"Mounts":[{"Type":"bind","Source":"/Volumes/work/db","Target":"/db"}]}}`,
			"/Volumes/work/db is not in a folder shared with the Docker Engine. Share it in GVX Desktop → Settings → Docker, restart the engine, and run this again."},
		{"shared mount bind", `{"HostConfig":{"Mounts":[{"Type":"bind","Source":"/Users/me/db","Target":"/db"}]}}`, ""},
		{"mount volume", `{"HostConfig":{"Mounts":[{"Type":"volume","Source":"myvol","Target":"/db"}]}}`, ""},
		{"unparsable json passes through", `{not json`, ""},
	} {
		err := checkCreateBinds([]byte(tc.body), engine)
		if tc.wantErr == "" {
			if err != nil {
				t.Errorf("%s: unexpected error %q", tc.name, err)
			}
			continue
		}
		if err == nil || err.Error() != tc.wantErr {
			t.Errorf("%s: err = %v, want %q", tc.name, err, tc.wantErr)
		}
	}
}

func TestDockerProxyBindErrorSurfacesInModel(t *testing.T) {
	// Occupy a loopback port so binding to it fails.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	busyPort := ln.Addr().(*net.TCPAddr).Port

	dir := t.TempDir()
	st := store.Store{Dir: dir}
	set := store.DefaultSettings()
	set.DockerEndpoint = DockerTCPEndpoint
	set.DockerPort = busyPort
	_ = st.SaveSettings(set)

	var m *Model
	p := ggui.ProbeBuilder(func() ggui.Widget { return Build(m) }, ggui.Sz(900, 700))
	p.Setup(func() {
		m = NewModel(st, dir)
		m.Attach(p)
		m.startDockerProxy()
	})
	defer p.Close()
	p.Frame()

	if got := ggui.Peek(m.DockerProxyErr); got == "" {
		t.Fatal("expected DockerProxyErr to be set for busy port")
	}

	// Re-binding to a free port clears the error immediately.
	freeLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	freePort := freeLn.Addr().(*net.TCPAddr).Port
	_ = freeLn.Close()

	m.SetDockerAPI(recipes.DockerAPI{TCP: true, Port: freePort})
	p.Frame()
	if got := ggui.Peek(m.DockerProxyErr); got != "" {
		t.Fatalf("expected DockerProxyErr cleared after moving to free port, got %q", got)
	}
	if addr := m.dockerProxy.Addr(); addr == nil || !strings.HasSuffix(addr.String(), ":"+itoa(freePort)) {
		t.Fatalf("proxy not listening on free port %d: %v", freePort, addr)
	}
}

func TestNewModelMigratesLegacyTCPPortToSettings(t *testing.T) {
	// 1. No explicit Docker endpoint in settings + old TCP spec -> saves {TCP, port}.
	dir := t.TempDir()
	st := store.Store{Dir: dir}
	legacy := recipes.DockerEngine()
	legacy.Ports = []string{"127.0.0.1:23755:2375", "127.0.0.1:9000-9019:9000-9019"}
	if err := st.SaveSandboxes([]sandbox.Spec{legacy}); err != nil {
		t.Fatal(err)
	}
	var m *Model
	p := ggui.ProbeBuilder(func() ggui.Widget { return Build(m) }, ggui.Sz(900, 700))
	p.Setup(func() {
		m = NewModel(st, dir)
		m.Offline = true
		m.Attach(p)
	})
	p.Frame()
	p.Close()

	set, _ := st.Settings()
	if set.DockerEndpoint != DockerTCPEndpoint || set.DockerPort != 23755 {
		t.Fatalf("migrated settings = (%q, %d), want (%q, 23755)", set.DockerEndpoint, set.DockerPort, DockerTCPEndpoint)
	}
	if got := m.DockerHost(); got != "tcp://127.0.0.1:23755" {
		t.Errorf("DockerHost = %q", got)
	}
	saved, _ := st.Sandboxes()
	if len(saved) != 1 || saved[0].Ports[0] != recipes.DockerBackendPublish {
		t.Errorf("upgraded spec ports = %v", saved[0].Ports)
	}

	// 2. Old unix spec with empty settings leaves settings untouched.
	dir2 := t.TempDir()
	st2 := store.Store{Dir: dir2}
	legacyUnix := recipes.DockerEngine()
	legacyUnix.Ports = []string{"unix:~/.gvx/docker.sock:2375"}
	_ = st2.SaveSandboxes([]sandbox.Spec{legacyUnix})
	var m2 *Model
	p2 := ggui.ProbeBuilder(func() ggui.Widget { return Build(m2) }, ggui.Sz(900, 700))
	p2.Setup(func() {
		m2 = NewModel(st2, dir2)
		m2.Offline = true
		m2.Attach(p2)
	})
	p2.Frame()
	p2.Close()
	if set2, _ := st2.Settings(); set2.DockerEndpoint != "" || set2.DockerPort != 0 {
		t.Errorf("old unix spec changed settings to (%q, %d)", set2.DockerEndpoint, set2.DockerPort)
	}

	// 3. Explicit settings are not overwritten by a legacy TCP spec.
	dir3 := t.TempDir()
	st3 := store.Store{Dir: dir3}
	explicit := store.DefaultSettings()
	explicit.DockerEndpoint = DockerSocketEndpoint
	_ = st3.SaveSettings(explicit)
	_ = st3.SaveSandboxes([]sandbox.Spec{legacy})
	var m3 *Model
	p3 := ggui.ProbeBuilder(func() ggui.Widget { return Build(m3) }, ggui.Sz(900, 700))
	p3.Setup(func() {
		m3 = NewModel(st3, dir3)
		m3.Offline = true
		m3.Attach(p3)
	})
	p3.Frame()
	p3.Close()
	if set3, _ := st3.Settings(); set3.DockerEndpoint != DockerSocketEndpoint || set3.DockerPort != 0 {
		t.Errorf("explicit settings overwritten: (%q, %d)", set3.DockerEndpoint, set3.DockerPort)
	}
}

func TestSaveDraftKeepsDockerBackendPort(t *testing.T) {
	dir := t.TempDir()
	st := store.Store{Dir: dir}
	engine := recipes.DockerEngine()
	_ = st.SaveSandboxes([]sandbox.Spec{engine})

	var m *Model
	p := ggui.ProbeBuilder(func() ggui.Widget { return Build(m) }, ggui.Sz(900, 700))
	p.Setup(func() {
		m = NewModel(st, dir)
		m.Offline = true
		m.Attach(p)
	})
	defer p.Close()
	p.Frame()

	// Removing the backend port in the sandbox editor restores it on SaveDraft.
	m.EditSandbox(engine.ID)
	m.Draft.Update(func(d Draft) Draft {
		d.Ports = "127.0.0.1:9000-9019:9000-9019"
		return d
	})
	if !m.SaveDraft(false) {
		t.Fatal("SaveDraft failed")
	}
	p.Frame()
	saved, _ := st.Sandboxes()
	if len(saved) != 1 || len(saved[0].Ports) != 2 || saved[0].Ports[0] != recipes.DockerBackendPublish || saved[0].Ports[1] != "127.0.0.1:9000-9019:9000-9019" {
		t.Fatalf("saved ports after removal = %v", saved[0].Ports)
	}

	// Replacing the backend port with a custom TCP 2375 publish rewrites it back.
	m.EditSandbox(engine.ID)
	m.Draft.Update(func(d Draft) Draft {
		d.Ports = "127.0.0.1:23750:2375\n127.0.0.1:8080:80"
		return d
	})
	if !m.SaveDraft(false) {
		t.Fatal("SaveDraft failed on replacement")
	}
	p.Frame()
	saved, _ = st.Sandboxes()
	if len(saved) != 1 || len(saved[0].Ports) != 2 || saved[0].Ports[0] != recipes.DockerBackendPublish || saved[0].Ports[1] != "127.0.0.1:8080:80" {
		t.Fatalf("saved ports after replacement = %v", saved[0].Ports)
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
