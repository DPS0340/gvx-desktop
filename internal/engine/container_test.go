package engine

// Owner: workstream B (containers). Unit tests over the fake Runner.

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ironpark/gvx-desktop/internal/rungv"
	"github.com/ironpark/gvx-desktop/internal/sandbox"
)

func createTestContainer(t *testing.T, te *testEngine, name string, body any) (int, containerCreateResponse, []byte) {
	t.Helper()
	path := "/v1.52/containers/create"
	if name != "" {
		path += "?name=" + url.QueryEscape(name)
	}
	status, raw := te.do(t, http.MethodPost, path, body)
	var resp containerCreateResponse
	_ = json.Unmarshal(raw, &resp)
	return status, resp, raw
}

func waitUntil(t *testing.T, timeout time.Duration, desc string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		if cond() {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", desc)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestContainerLifecycleAndInspect(t *testing.T) {
	te := newTestEngine(t)
	imgID := te.fake.addImage("alpine:3.20", ImageConfig{
		Cmd:        []string{"/bin/sh"},
		Env:        []string{"PATH=/usr/bin:/bin", "IMG=1"},
		WorkingDir: "/app",
	})

	// Missing image returns 404 so CLI pulls.
	status, _, raw := createTestContainer(t, te, "missing-img", map[string]any{"Image": "no-such:1"})
	if status != http.StatusNotFound || !strings.Contains(string(raw), "No such image") {
		t.Fatalf("missing image: got %d %s", status, raw)
	}

	// Create container.
	status, created, raw := createTestContainer(t, te, "gvxd-alpha", map[string]any{
		"Image":  "alpine:3.20",
		"Cmd":    []string{"echo", "ok"},
		"Labels": map[string]string{"com.docker.compose.project": "demo", "tier": "web"},
		"Tty":    true,
		"HostConfig": map[string]any{
			"ConsoleSize":    [2]uint{30, 100},
			"Memory":         256 * 1024 * 1024,
			"NanoCpus":       1500000000,
			"ReadonlyRootfs": true,
		},
	})
	if status != http.StatusCreated {
		t.Fatalf("create: %d %s", status, raw)
	}
	if len(created.ID) != 64 {
		t.Fatalf("expected 64-hex id, got %q", created.ID)
	}

	// Inspect created container.
	status, raw = te.do(t, http.MethodGet, "/containers/"+created.ID+"/json", nil)
	if status != http.StatusOK {
		t.Fatalf("inspect created: %d %s", status, raw)
	}
	var ins containerInspectResponse
	if err := json.Unmarshal(raw, &ins); err != nil {
		t.Fatal(err)
	}
	if ins.ID != created.ID || ins.Name != "/gvxd-alpha" || ins.Image != imgID {
		t.Fatalf("unexpected inspect identity: %+v", ins)
	}
	if ins.State.Status != StatusCreated || ins.State.Running || ins.State.StartedAt != "0001-01-01T00:00:00Z" {
		t.Fatalf("unexpected created state: %+v", ins.State)
	}
	if !ins.Config.Tty || ins.Path != "echo" || !slices.Equal(ins.Args, []string{"ok"}) {
		t.Fatalf("unexpected inspect config/args: path=%q args=%v tty=%v", ins.Path, ins.Args, ins.Config.Tty)
	}
	if ins.Driver != "gvx" || ins.Platform != "linux" {
		t.Fatalf("unexpected driver/platform: %q %q", ins.Driver, ins.Platform)
	}

	// Start container.
	status, raw = te.do(t, http.MethodPost, "/containers/"+created.ID+"/start", nil)
	if status != http.StatusNoContent {
		t.Fatalf("start: %d %s", status, raw)
	}
	// Starting an already running container returns 304.
	if status, _ := te.do(t, http.MethodPost, "/containers/gvxd-alpha/start", nil); status != http.StatusNotModified {
		t.Fatalf("double start: got %d, want 304", status)
	}

	// Verify spec passed to runner and initial ConsoleSize resize.
	started := te.fake.Started()
	if len(started) != 1 {
		t.Fatalf("expected 1 start, got %d", len(started))
	}
	sp := started[0]
	if sp.ID != created.ID || sp.ContainerID != created.ID || sp.Name != "gvxd-alpha" || sp.Pull != "never" {
		t.Fatalf("unexpected spec identity: %+v", sp)
	}
	if !sp.SetEntrypoint || sp.Entrypoint != "echo" || sp.Command != "ok" {
		t.Fatalf("unexpected spec command: setEP=%v ep=%q cmd=%q", sp.SetEntrypoint, sp.Entrypoint, sp.Command)
	}
	if sp.Memory != "268435456b" || sp.CPUs != "1.5" || sp.ExtraArgs != "-read-only" || !sp.Unprivileged || !sp.TTY {
		t.Fatalf("unexpected spec limits/flags: %+v", sp)
	}
	if p := te.fake.proc(created.ID); p == nil {
		t.Fatal("missing fakeProc")
	} else if r, c := p.Size(); r != 30 || c != 100 {
		t.Fatalf("ConsoleSize resize not applied: got %dx%d", r, c)
	}

	// Remove without force fails with 409 while running.
	if status, _ := te.do(t, http.MethodDelete, "/containers/"+created.ID, nil); status != http.StatusConflict {
		t.Fatalf("rm running without force: got %d, want 409", status)
	}

	// Stop container.
	if status, raw := te.do(t, http.MethodPost, "/containers/"+created.ID+"/stop?t=1", nil); status != http.StatusNoContent {
		t.Fatalf("stop: %d %s", status, raw)
	}
	// Stopping already stopped container returns 304.
	if status, _ := te.do(t, http.MethodPost, "/containers/"+created.ID+"/stop", nil); status != http.StatusNotModified {
		t.Fatalf("double stop: got %d, want 304", status)
	}

	// Inspect after stop.
	status, raw = te.do(t, http.MethodGet, "/containers/"+created.ID[:12]+"/json", nil)
	if status != http.StatusOK {
		t.Fatalf("inspect by prefix: %d %s", status, raw)
	}
	_ = json.Unmarshal(raw, &ins)
	if ins.State.Status != StatusExited || ins.State.Running || ins.State.ExitCode != 130 {
		t.Fatalf("unexpected stopped state: %+v", ins.State)
	}

	// Restart starts it again.
	if status, raw := te.do(t, http.MethodPost, "/containers/"+created.ID+"/restart?t=1", nil); status != http.StatusNoContent {
		t.Fatalf("restart: %d %s", status, raw)
	}
	c, err := te.lookup(created.ID)
	if err != nil || !c.State.Running() {
		t.Fatalf("expected running after restart: %+v %v", c.State, err)
	}

	// Kill with SIGKILL → exit code 137.
	if status, raw := te.do(t, http.MethodPost, "/containers/"+created.ID+"/kill?signal=KILL", nil); status != http.StatusNoContent {
		t.Fatalf("kill: %d %s", status, raw)
	}
	waitUntil(t, 2*time.Second, "killed container exited", func() bool {
		c, err := te.lookup(created.ID)
		return err == nil && c.State.Status == StatusExited
	})
	c, _ = te.lookup(created.ID)
	if c.State.ExitCode != 137 {
		t.Fatalf("kill exit code = %d, want 137", c.State.ExitCode)
	}
	// Kill when not running → 409.
	if status, _ := te.do(t, http.MethodPost, "/containers/"+created.ID+"/kill", nil); status != http.StatusConflict {
		t.Fatalf("kill stopped: got %d, want 409", status)
	}
	// Kill with bad signal → 400.
	if status, _ := te.do(t, http.MethodPost, "/containers/"+created.ID+"/kill?signal=BOGUS", nil); status != http.StatusBadRequest {
		t.Fatalf("kill invalid signal: got %d, want 400", status)
	}

	// Rename.
	if status, raw := te.do(t, http.MethodPost, "/containers/gvxd-alpha/rename?name=gvxd-beta", nil); status != http.StatusNoContent {
		t.Fatalf("rename: %d %s", status, raw)
	}
	if _, err := te.lookup("gvxd-alpha"); statusOf(err) != http.StatusNotFound {
		t.Fatalf("old name should be gone, err=%v", err)
	}
	if c, err := te.lookup("/gvxd-beta"); err != nil || c.ID != created.ID || c.Spec.Name != "gvxd-beta" {
		t.Fatalf("renamed lookup failed: %+v %v", c, err)
	}

	// Start again and force-remove.
	if status, raw := te.do(t, http.MethodPost, "/containers/gvxd-beta/start", nil); status != http.StatusNoContent {
		t.Fatalf("restart after rename: %d %s", status, raw)
	}
	if status, raw := te.do(t, http.MethodDelete, "/containers/gvxd-beta?force=1", nil); status != http.StatusNoContent {
		t.Fatalf("rm -f: %d %s", status, raw)
	}
	if _, err := te.lookup(created.ID); statusOf(err) != http.StatusNotFound {
		t.Fatalf("container still exists after rm -f: %v", err)
	}
}

func TestContainerNamesConflictsAndPrefixes(t *testing.T) {
	te := newTestEngine(t)
	te.fake.addImage("alpine", ImageConfig{Cmd: []string{"/bin/sh"}})

	// Invalid name → 400.
	if status, _, _ := createTestContainer(t, te, "-bad", map[string]any{"Image": "alpine"}); status != http.StatusBadRequest {
		t.Fatalf("invalid name: got %d, want 400", status)
	}

	// Generated names when unnamed.
	s1, c1, _ := createTestContainer(t, te, "", map[string]any{"Image": "alpine"})
	s2, c2, _ := createTestContainer(t, te, "", map[string]any{"Image": "alpine"})
	if s1 != http.StatusCreated || s2 != http.StatusCreated {
		t.Fatalf("unnamed creates failed: %d %d", s1, s2)
	}
	rec1, _ := te.lookup(c1.ID)
	rec2, _ := te.lookup(c2.ID)
	if !strings.Contains(rec1.Name, "_") || rec1.Name == rec2.Name {
		t.Fatalf("unexpected generated names: %q %q", rec1.Name, rec2.Name)
	}

	// Name conflict → 409 with docker's conflict message.
	status, c3, _ := createTestContainer(t, te, "gvxd-dup", map[string]any{"Image": "alpine"})
	if status != http.StatusCreated {
		t.Fatalf("first dup create: %d", status)
	}
	status, _, raw := createTestContainer(t, te, "gvxd-dup", map[string]any{"Image": "alpine"})
	var errResp struct{ Message string }
	_ = json.Unmarshal(raw, &errResp)
	if status != http.StatusConflict || !strings.Contains(errResp.Message, `already in use by container "`+c3.ID+`"`) {
		t.Fatalf("name conflict: got %d %s", status, raw)
	}

	// Rename conflict → 409.
	if status, _ := te.do(t, http.MethodPost, "/containers/"+c1.ID+"/rename?name=gvxd-dup", nil); status != http.StatusConflict {
		t.Fatalf("rename conflict: got %d, want 409", status)
	}

	// Ambiguous ID prefix → 404 mentioning multiple IDs.
	te.ctr.mu.Lock()
	te.ctr.byID["aaaa0000111122223333444455556666777788889999aaaabbbbccccddddeeee"] = &Container{
		ID: "aaaa0000111122223333444455556666777788889999aaaabbbbccccddddeeee", Name: "p1",
	}
	te.ctr.byID["aaaa9999111122223333444455556666777788889999aaaabbbbccccddddeeee"] = &Container{
		ID: "aaaa9999111122223333444455556666777788889999aaaabbbbccccddddeeee", Name: "p2",
	}
	te.ctr.mu.Unlock()

	if _, err := te.lookup("aaaa"); statusOf(err) != http.StatusNotFound || !strings.Contains(err.Error(), "Multiple") {
		t.Fatalf("ambiguous prefix: got %v", err)
	}
	if c, err := te.lookup("aaaa0000"); err != nil || c.Name != "p1" {
		t.Fatalf("unique prefix failed: %+v %v", c, err)
	}
}

func TestContainerImageConfigMerge(t *testing.T) {
	img := ImageInspect{
		ID: "sha256:" + fakeHex("img"),
		Config: ImageConfig{
			Entrypoint: []string{"/ep", "--flag"},
			Cmd:        []string{"default-arg"},
			Env:        []string{"A=1", "B=2", "HOST_ONLY"},
			WorkingDir: "/imgdir",
			User:       "1000:1000",
			Labels:     map[string]string{"img": "yes", "shared": "from-img"},
		},
	}

	cases := []struct {
		name     string
		body     string
		wantEP   []string
		wantCmd  []string
		wantArgv []string
		wantErr  bool
	}{
		{
			name:     "nil entrypoint nil cmd inherits both",
			body:     `{"Image":"img"}`,
			wantEP:   []string{"/ep", "--flag"},
			wantCmd:  []string{"default-arg"},
			wantArgv: []string{"/ep", "--flag", "default-arg"},
		},
		{
			name:     "nil entrypoint with cmd keeps image entrypoint",
			body:     `{"Image":"img","Cmd":["hi"]}`,
			wantEP:   []string{"/ep", "--flag"},
			wantCmd:  []string{"hi"},
			wantArgv: []string{"/ep", "--flag", "hi"},
		},
		{
			name:     "explicit empty entrypoint clears entrypoint and keeps image cmd",
			body:     `{"Image":"img","Entrypoint":[]}`,
			wantEP:   nil,
			wantCmd:  []string{"default-arg"},
			wantArgv: []string{"default-arg"},
		},
		{
			name:     "explicit empty entrypoint with cmd",
			body:     `{"Image":"img","Entrypoint":[],"Cmd":["/bin/echo","x"]}`,
			wantEP:   nil,
			wantCmd:  []string{"/bin/echo", "x"},
			wantArgv: []string{"/bin/echo", "x"},
		},
		{
			name:     "empty string entrypoint with cmd",
			body:     `{"Image":"img","Entrypoint":[""],"Cmd":["/bin/echo","x"]}`,
			wantEP:   nil,
			wantCmd:  []string{"/bin/echo", "x"},
			wantArgv: []string{"/bin/echo", "x"},
		},
		{
			name:    "empty string entrypoint without cmd is 400",
			body:    `{"Image":"img","Entrypoint":[""]}`,
			wantErr: true,
		},
		{
			name:     "custom entrypoint clears image cmd",
			body:     `{"Image":"img","Entrypoint":["/custom"]}`,
			wantEP:   []string{"/custom"},
			wantCmd:  nil,
			wantArgv: []string{"/custom"},
		},
		{
			name:     "custom entrypoint with cmd",
			body:     `{"Image":"img","Entrypoint":["/custom"],"Cmd":["arg1","arg2"]}`,
			wantEP:   []string{"/custom"},
			wantCmd:  []string{"arg1", "arg2"},
			wantArgv: []string{"/custom", "arg1", "arg2"},
		},
	}

	id := fakeHex("ctr")
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var req containerCreateRequest
			if err := json.Unmarshal([]byte(tc.body), &req); err != nil {
				t.Fatal(err)
			}
			cfg, argv, _, err := mergeContainerConfig(id, &req, img)
			if tc.wantErr {
				if statusOf(err) != http.StatusBadRequest {
					t.Fatalf("expected 400, got %v", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if !slices.Equal(cfg.Entrypoint, tc.wantEP) || !slices.Equal(cfg.Cmd, tc.wantCmd) || !slices.Equal(argv, tc.wantArgv) {
				t.Fatalf("got ep=%v cmd=%v argv=%v; want ep=%v cmd=%v argv=%v",
					cfg.Entrypoint, cfg.Cmd, argv, tc.wantEP, tc.wantCmd, tc.wantArgv)
			}
		})
	}

	// Env override, bare KEY dropping, default PATH, WorkingDir, User, Labels, Hostname, Healthcheck warning.
	var req containerCreateRequest
	raw := `{
		"Image": "img",
		"Env": ["B=override", "C=3", "BARE_LEAK"],
		"Labels": {"shared": "from-req", "extra": "1"},
		"Healthcheck": {"Test": ["CMD", "true"]}
	}`
	if err := json.Unmarshal([]byte(raw), &req); err != nil {
		t.Fatal(err)
	}
	cfg, _, warnings, err := mergeContainerConfig(id, &req, img)
	if err != nil {
		t.Fatal(err)
	}
	wantEnv := []string{"A=1", "B=override", "C=3", defaultContainerPATH}
	if !slices.Equal(cfg.Env, wantEnv) {
		t.Fatalf("env = %v, want %v", cfg.Env, wantEnv)
	}
	if cfg.WorkingDir != "/imgdir" || cfg.User != "1000:1000" || cfg.Hostname != id[:12] {
		t.Fatalf("unexpected workdir/user/hostname: %q %q %q", cfg.WorkingDir, cfg.User, cfg.Hostname)
	}
	if cfg.Labels["img"] != "yes" || cfg.Labels["shared"] != "from-req" || cfg.Labels["extra"] != "1" {
		t.Fatalf("unexpected labels: %v", cfg.Labels)
	}
	if len(warnings) == 0 || !strings.Contains(warnings[0], "Healthcheck") {
		t.Fatalf("expected healthcheck warning, got %v", warnings)
	}

	// Empty image WorkingDir defaults to "/".
	emptyImg := ImageInspect{ID: "sha256:" + fakeHex("empty"), Config: ImageConfig{Cmd: []string{"/bin/sh"}}}
	var req2 containerCreateRequest
	_ = json.Unmarshal([]byte(`{"Image":"empty"}`), &req2)
	cfg2, _, _, err := mergeContainerConfig(id, &req2, emptyImg)
	if err != nil || cfg2.WorkingDir != "/" {
		t.Fatalf("default workdir = %q, err=%v", cfg2.WorkingDir, err)
	}

	// Empty argv with empty image is 400 No command specified.
	noCmdImg := ImageInspect{ID: "sha256:" + fakeHex("nocmd")}
	if _, _, _, err := mergeContainerConfig(id, &req2, noCmdImg); statusOf(err) != http.StatusBadRequest || !strings.Contains(err.Error(), "No command specified") {
		t.Fatalf("expected 400 No command specified, got %v", err)
	}
}

func TestContainerListAndFilters(t *testing.T) {
	te := newTestEngine(t)
	te.fake.addImage("alpine:3.20", ImageConfig{Cmd: []string{"/bin/sh"}})
	te.fake.addImage("nginx:alpine", ImageConfig{Cmd: []string{"nginx"}})

	// Create three containers with distinct labels, images, and states.
	_, c1, _ := createTestContainer(t, te, "gvxd-one", map[string]any{
		"Image":  "alpine:3.20",
		"Labels": map[string]string{"com.docker.compose.project": "projA", "env": "dev"},
	})
	time.Sleep(2 * time.Millisecond)
	_, c2, _ := createTestContainer(t, te, "gvxd-two", map[string]any{
		"Image":  "nginx:alpine",
		"Labels": map[string]string{"com.docker.compose.project": "projA", "env": "prod"},
		"HostConfig": map[string]any{
			"Binds": []string{"sharedvol:/data"},
		},
	})
	time.Sleep(2 * time.Millisecond)
	_, c3, _ := createTestContainer(t, te, "gvxd-three", map[string]any{
		"Image":  "alpine:3.20",
		"Labels": map[string]string{"com.docker.compose.project": "projB"},
	})

	// Start only c2.
	if status, raw := te.do(t, http.MethodPost, "/containers/"+c2.ID+"/start", nil); status != http.StatusNoContent {
		t.Fatalf("start c2: %d %s", status, raw)
	}
	// Start and exit c3 with code 7.
	te.fake.Script = func(p *fakeProc) int { return 7 }
	if status, raw := te.do(t, http.MethodPost, "/containers/"+c3.ID+"/start", nil); status != http.StatusNoContent {
		t.Fatalf("start c3: %d %s", status, raw)
	}
	waitUntil(t, 2*time.Second, "c3 exited", func() bool {
		c, _ := te.lookup(c3.ID)
		return c.State.Status == StatusExited
	})
	te.fake.Script = nil

	list := func(q string) []containerListItem {
		t.Helper()
		status, raw := te.do(t, http.MethodGet, "/containers/json"+q, nil)
		if status != http.StatusOK {
			t.Fatalf("list %s: %d %s", q, status, raw)
		}
		var rows []containerListItem
		if err := json.Unmarshal(raw, &rows); err != nil {
			t.Fatal(err)
		}
		return rows
	}

	// Default lists only running.
	if rows := list(""); len(rows) != 1 || rows[0].ID != c2.ID {
		t.Fatalf("default list = %+v, want only c2", rows)
	}
	// all=1 lists newest first: c3, c2, c1.
	rows := list("?all=1")
	if len(rows) != 3 || rows[0].ID != c3.ID || rows[1].ID != c2.ID || rows[2].ID != c1.ID {
		t.Fatalf("all=1 order wrong: %v", []string{rows[0].ID, rows[1].ID, rows[2].ID})
	}
	// Label filter in map form (Compose) and slice form (legacy).
	for _, f := range []string{
		`{"label":{"com.docker.compose.project=projA":true,"env":true}}`,
		`{"label":["com.docker.compose.project=projA","env"]}`,
	} {
		got := list("?all=1&filters=" + url.QueryEscape(f))
		if len(got) != 2 || got[0].ID != c2.ID || got[1].ID != c1.ID {
			t.Fatalf("label filter %s: got %d items", f, len(got))
		}
	}
	// Negated label filter label!.
	got := list("?all=1&filters=" + url.QueryEscape(`{"label":{"com.docker.compose.project=projA":true},"label!":{"env=prod":true}}`))
	if len(got) != 1 || got[0].ID != c1.ID {
		t.Fatalf("label! filter: got %+v", got)
	}
	// Name, id, status, exited, ancestor, volume, network filters.
	if got := list("?all=1&filters=" + url.QueryEscape(`{"name":["two"]}`)); len(got) != 1 || got[0].ID != c2.ID {
		t.Fatalf("name filter: %+v", got)
	}
	if got := list("?all=1&filters=" + url.QueryEscape(`{"id":["`+c1.ID[:8]+`"]}`)); len(got) != 1 || got[0].ID != c1.ID {
		t.Fatalf("id filter: %+v", got)
	}
	if got := list("?filters=" + url.QueryEscape(`{"status":["exited"]}`)); len(got) != 1 || got[0].ID != c3.ID {
		t.Fatalf("status filter: %+v", got)
	}
	if got := list("?filters=" + url.QueryEscape(`{"exited":["7"]}`)); len(got) != 1 || got[0].ID != c3.ID {
		t.Fatalf("exited filter: %+v", got)
	}
	if got := list("?all=1&filters=" + url.QueryEscape(`{"ancestor":["nginx:alpine"]}`)); len(got) != 1 || got[0].ID != c2.ID {
		t.Fatalf("ancestor filter: %+v", got)
	}
	if got := list("?all=1&filters=" + url.QueryEscape(`{"volume":["sharedvol"]}`)); len(got) != 1 || got[0].ID != c2.ID {
		t.Fatalf("volume filter: %+v", got)
	}
	if got := list("?all=1&filters=" + url.QueryEscape(`{"network":["bridge"]}`)); len(got) != 3 {
		t.Fatalf("network filter: got %d, want 3", len(got))
	}
	if got := list("?limit=2"); len(got) != 2 {
		t.Fatalf("limit=2: got %d", len(got))
	}
}

func TestContainerPorts(t *testing.T) {
	te := newTestEngine(t)
	te.fake.addImage("nginx:alpine", ImageConfig{
		Cmd:          []string{"nginx"},
		ExposedPorts: map[string]struct{}{"80/tcp": {}, "443/tcp": {}},
	})

	// NetworkMode=none with published ports → 400.
	if status, _, _ := createTestContainer(t, te, "none-port", map[string]any{
		"Image": "nginx:alpine",
		"HostConfig": map[string]any{
			"NetworkMode":  "none",
			"PortBindings": map[string][]PortBinding{"80/tcp": {{HostPort: "18090"}}},
		},
	}); status != http.StatusBadRequest {
		t.Fatalf("none+ports: got %d, want 400", status)
	}

	// Non-loopback HostIp → warning + published on 127.0.0.1; empty HostPort → dynamic free port.
	status, c1, raw := createTestContainer(t, te, "gvxd-ports", map[string]any{
		"Image": "nginx:alpine",
		"HostConfig": map[string]any{
			"PortBindings": map[string][]PortBinding{
				"80/tcp":  {{HostIP: "192.168.1.5", HostPort: ""}},
				"443/tcp": {{HostIP: "0.0.0.0", HostPort: ""}},
			},
		},
	})
	if status != http.StatusCreated {
		t.Fatalf("create ports: %d %s", status, raw)
	}
	if len(c1.Warnings) != 1 || !strings.Contains(c1.Warnings[0], "192.168.1.5") {
		t.Fatalf("expected 1 non-loopback warning (and none for 0.0.0.0), got %v", c1.Warnings)
	}

	if status, raw := te.do(t, http.MethodPost, "/containers/"+c1.ID+"/start", nil); status != http.StatusNoContent {
		t.Fatalf("start ports: %d %s", status, raw)
	}
	rec, _ := te.lookup(c1.ID)
	if len(rec.Ports["80/tcp"]) != 1 || rec.Ports["80/tcp"][0].HostIP != "127.0.0.1" || rec.Ports["80/tcp"][0].HostPort == "" {
		t.Fatalf("unexpected resolved 80/tcp port: %+v", rec.Ports)
	}
	if len(rec.Ports["443/tcp"]) != 1 || rec.Ports["443/tcp"][0].HostIP != "127.0.0.1" || rec.Ports["443/tcp"][0].HostPort == "" {
		t.Fatalf("unexpected resolved 443/tcp port: %+v", rec.Ports)
	}

	// PublishAllPorts publishes all ExposedPorts.
	status, cAll, raw := createTestContainer(t, te, "gvxd-allports", map[string]any{
		"Image":      "nginx:alpine",
		"HostConfig": map[string]any{"PublishAllPorts": true},
	})
	if status != http.StatusCreated {
		t.Fatalf("create PublishAllPorts: %d %s", status, raw)
	}
	if status, raw := te.do(t, http.MethodPost, "/containers/"+cAll.ID+"/start", nil); status != http.StatusNoContent {
		t.Fatalf("start PublishAllPorts: %d %s", status, raw)
	}
	recAll, _ := te.lookup(cAll.ID)
	if len(recAll.Ports["80/tcp"]) != 1 || len(recAll.Ports["443/tcp"]) != 1 {
		t.Fatalf("PublishAllPorts did not publish all exposed ports: %+v", recAll.Ports)
	}

	// Busy fixed port fails start with 500 and exact message.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	busyPort := strconv.Itoa(ln.Addr().(*net.TCPAddr).Port)

	status, cBusy, raw := createTestContainer(t, te, "gvxd-busy", map[string]any{
		"Image": "nginx:alpine",
		"HostConfig": map[string]any{
			"PortBindings": map[string][]PortBinding{"80/tcp": {{HostPort: busyPort}}},
		},
	})
	if status != http.StatusCreated {
		t.Fatalf("create busy: %d %s", status, raw)
	}
	status, raw = te.do(t, http.MethodPost, "/containers/"+cBusy.ID+"/start", nil)
	wantMsg := fmt.Sprintf("Bind for 127.0.0.1:%s failed: port is already allocated", busyPort)
	if status != http.StatusInternalServerError || !strings.Contains(string(raw), wantMsg) {
		t.Fatalf("busy port start: got %d %s, want 500 with %q", status, raw, wantMsg)
	}

	// Host port range is expanded.
	status, cRange, raw := createTestContainer(t, te, "gvxd-range", map[string]any{
		"Image": "nginx:alpine",
		"HostConfig": map[string]any{
			"PortBindings": map[string][]PortBinding{"80/tcp": {{HostPort: "18090-18091"}}},
		},
	})
	if status != http.StatusCreated {
		t.Fatalf("create range: %d %s", status, raw)
	}
	recRange, _ := te.lookup(cRange.ID)
	if len(recRange.HostConfig.PortBindings["80/tcp"]) != 2 {
		t.Fatalf("expected expanded port range of 2, got %+v", recRange.HostConfig.PortBindings["80/tcp"])
	}
}

func TestContainerMountsAndVolumes(t *testing.T) {
	te := newTestEngine(t)
	te.fake.addImage("postgres:16", ImageConfig{
		Cmd:     []string{"postgres"},
		Volumes: map[string]struct{}{"/var/lib/postgresql/data": {}, "/covered": {}},
	})

	hostDir := t.TempDir()

	// Mounts bind with non-existent source → 400.
	missingDir := filepath.Join(hostDir, "does-not-exist")
	status, _, raw := createTestContainer(t, te, "bad-bind", map[string]any{
		"Image": "postgres:16",
		"HostConfig": map[string]any{
			"Mounts": []MountSpec{{Type: "bind", Source: missingDir, Target: "/mnt"}},
		},
	})
	if status != http.StatusBadRequest || !strings.Contains(string(raw), "bind source path does not exist") {
		t.Fatalf("missing bind source: got %d %s", status, raw)
	}

	// Unknown mount type → 400.
	if status, _, _ := createTestContainer(t, te, "bad-type", map[string]any{
		"Image": "postgres:16",
		"HostConfig": map[string]any{
			"Mounts": []MountSpec{{Type: "npipe", Source: "x", Target: "/mnt"}},
		},
	}); status != http.StatusBadRequest {
		t.Fatalf("bad mount type: got %d, want 400", status)
	}

	// Duplicate mount target → 400.
	if status, _, _ := createTestContainer(t, te, "dup-target", map[string]any{
		"Image": "postgres:16",
		"HostConfig": map[string]any{
			"Binds": []string{hostDir + ":/dup", "vol1:/dup"},
		},
	}); status != http.StatusBadRequest {
		t.Fatalf("duplicate target: got %d, want 400", status)
	}

	// Valid mix: host bind, named volume, Mounts bind + anonymous volume + tmpfs, HostConfig.Tmpfs, and image VOLUME.
	status, created, raw := createTestContainer(t, te, "gvxd-mounts", map[string]any{
		"Image": "postgres:16",
		"HostConfig": map[string]any{
			"Binds": []string{
				hostDir + ":/covered:ro,z,cached",
				"named-vol:/named",
			},
			"Mounts": []MountSpec{
				{Type: "bind", Source: hostDir, Target: "/hostbind"},
				{Type: "volume", Source: "", Target: "/anonmount"},
				{Type: "tmpfs", Target: "/tmpmount", TmpfsOptions: &TmpfsOptions{SizeBytes: 67108864}},
			},
			"Tmpfs": map[string]string{"/run": "size=32m,ro"},
		},
	})
	if status != http.StatusCreated {
		t.Fatalf("create mounts: %d %s", status, raw)
	}

	rec, err := te.lookup(created.ID)
	if err != nil {
		t.Fatal(err)
	}
	// Should have 2 anonymous volumes: /anonmount and /var/lib/postgresql/data (/covered was overridden by bind).
	if len(rec.AnonymousVolumes) != 2 {
		t.Fatalf("expected 2 anonymous volumes, got %v", rec.AnonymousVolumes)
	}
	// Check Windows bind parsing directly.
	wSrc, wDst, wOpts, err := parseBindSpec(`C:\Users\me\data:/data:ro`)
	if err != nil || wSrc != `C:\Users\me\data` || wDst != "/data" || wOpts != "ro" {
		t.Fatalf("windows bind parse failed: %q %q %q %v", wSrc, wDst, wOpts, err)
	}

	// Verify named-vol and anonymous volumes exist in fake runner.
	vols, _ := te.fake.Volumes(t.Context())
	volNames := map[string]bool{}
	for _, v := range vols {
		volNames[v.Name] = true
	}
	if !volNames["named-vol"] {
		t.Fatalf("named-vol not created in runner: %v", vols)
	}
	for _, av := range rec.AnonymousVolumes {
		if !volNames[av] {
			t.Fatalf("anonymous volume %s not created", av)
		}
	}

	// rm -v removes anonymous volumes but keeps named-vol.
	if status, raw := te.do(t, http.MethodDelete, "/containers/"+created.ID+"?v=1", nil); status != http.StatusNoContent {
		t.Fatalf("rm -v: %d %s", status, raw)
	}
	vols, _ = te.fake.Volumes(t.Context())
	volNames = map[string]bool{}
	for _, v := range vols {
		volNames[v.Name] = true
	}
	if !volNames["named-vol"] {
		t.Fatalf("rm -v deleted named volume!")
	}
	for _, av := range rec.AnonymousVolumes {
		if volNames[av] {
			t.Fatalf("rm -v did not delete anonymous volume %s", av)
		}
	}

	// Volume in-use Start error maps to 409.
	_, cVol, _ := createTestContainer(t, te, "gvxd-volerr", map[string]any{"Image": "postgres:16"})
	te.fake.StartErr = errors.New("rungv run: volume named-vol is in use by another sandbox")
	if status, _ := te.do(t, http.MethodPost, "/containers/"+cVol.ID+"/start", nil); status != http.StatusConflict {
		t.Fatalf("volume in use start: got %d, want 409", status)
	}
	te.fake.StartErr = nil
}

func TestContainerAutoRemoveAndWait(t *testing.T) {
	te := newTestEngine(t)
	te.fake.addImage("alpine:3.20", ImageConfig{
		Cmd:     []string{"/bin/sh"},
		Volumes: map[string]struct{}{"/data": {}},
	})

	// AutoRemove + restart policy → 400.
	if status, _, raw := createTestContainer(t, te, "bad-ar", map[string]any{
		"Image": "alpine:3.20",
		"HostConfig": map[string]any{
			"AutoRemove":    true,
			"RestartPolicy": RestartPolicy{Name: "always"},
		},
	}); status != http.StatusBadRequest || !strings.Contains(string(raw), "AutoRemove") {
		t.Fatalf("AutoRemove+RestartPolicy: got %d %s", status, raw)
	}

	// Create AutoRemove container.
	status, created, raw := createTestContainer(t, te, "gvxd-autorm", map[string]any{
		"Image":      "alpine:3.20",
		"HostConfig": map[string]any{"AutoRemove": true},
	})
	if status != http.StatusCreated {
		t.Fatalf("create AutoRemove: %d %s", status, raw)
	}
	rec, _ := te.lookup(created.ID)
	if len(rec.AnonymousVolumes) != 1 {
		t.Fatalf("expected 1 anonymous volume, got %v", rec.AnonymousVolumes)
	}
	anonVol := rec.AnonymousVolumes[0]

	// Start container, let it exit with code 42.
	te.fake.Script = func(p *fakeProc) int {
		time.Sleep(20 * time.Millisecond)
		return 42
	}
	// Issue wait?condition=removed concurrently with start (as docker run --rm does).
	waitDone := make(chan int, 1)
	go func() {
		st, body := te.do(t, http.MethodPost, "/containers/"+created.ID+"/wait?condition=removed", nil)
		if st != http.StatusOK {
			t.Errorf("wait removed: %d %s", st, body)
			waitDone <- -1
			return
		}
		var wr struct{ StatusCode int }
		_ = json.Unmarshal(body, &wr)
		waitDone <- wr.StatusCode
	}()
	time.Sleep(15 * time.Millisecond)
	if status, raw := te.do(t, http.MethodPost, "/containers/"+created.ID+"/start", nil); status != http.StatusNoContent {
		t.Fatalf("start AutoRemove: %d %s", status, raw)
	}

	select {
	case code := <-waitDone:
		if code != 42 {
			t.Fatalf("wait removed StatusCode = %d, want 42", code)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for wait?condition=removed")
	}

	// Container and its anonymous volume must be gone.
	if _, err := te.lookup(created.ID); statusOf(err) != http.StatusNotFound {
		t.Fatalf("AutoRemove container still exists: %v", err)
	}
	vols, _ := te.fake.Volumes(t.Context())
	for _, v := range vols {
		if v.Name == anonVol {
			t.Fatalf("AutoRemove did not delete anonymous volume %s", anonVol)
		}
	}
}

func TestContainerRestartPolicies(t *testing.T) {
	te := newTestEngine(t)
	te.ctr.restartBase = 15 * time.Millisecond
	te.ctr.restartMax = 60 * time.Millisecond
	te.ctr.restartReset = 10 * time.Second
	te.fake.addImage("alpine", ImageConfig{Cmd: []string{"/bin/sh"}})

	// on-failure with max retries = 2: runs 1 initial + 2 restarts = 3 total starts, then stays exited.
	var runs atomic.Int32
	te.fake.Script = func(p *fakeProc) int {
		runs.Add(1)
		return 1
	}
	status, cOnFail, raw := createTestContainer(t, te, "gvxd-onfail", map[string]any{
		"Image": "alpine",
		"HostConfig": map[string]any{
			"RestartPolicy": RestartPolicy{Name: "on-failure", MaximumRetryCount: 2},
		},
	})
	if status != http.StatusCreated {
		t.Fatalf("create on-failure: %d %s", status, raw)
	}
	if status, raw := te.do(t, http.MethodPost, "/containers/"+cOnFail.ID+"/start", nil); status != http.StatusNoContent {
		t.Fatalf("start on-failure: %d %s", status, raw)
	}
	waitUntil(t, 2*time.Second, "on-failure finishes retries", func() bool {
		c, err := te.lookup(cOnFail.ID)
		return err == nil && c.State.Status == StatusExited && c.RestartCount == 2
	})
	if got := runs.Load(); got != 3 {
		t.Fatalf("on-failure runs = %d, want 3", got)
	}

	// on-failure exits 0 → does NOT restart.
	runs.Store(0)
	te.fake.Script = func(p *fakeProc) int {
		runs.Add(1)
		return 0
	}
	_, cZero, _ := createTestContainer(t, te, "gvxd-onfail0", map[string]any{
		"Image": "alpine",
		"HostConfig": map[string]any{
			"RestartPolicy": RestartPolicy{Name: "on-failure", MaximumRetryCount: 3},
		},
	})
	_, _ = te.do(t, http.MethodPost, "/containers/"+cZero.ID+"/start", nil)
	waitUntil(t, 2*time.Second, "on-failure exit 0", func() bool {
		c, _ := te.lookup(cZero.ID)
		return c.State.Status == StatusExited
	})
	time.Sleep(50 * time.Millisecond)
	if got := runs.Load(); got != 1 {
		t.Fatalf("on-failure exit 0 restarted: runs = %d, want 1", got)
	}

	// always restarts even on exit 0; stop stops it.
	runs.Store(0)
	te.fake.Script = func(p *fakeProc) int {
		if runs.Add(1) < 3 {
			return 0
		}
		<-p.Done()
		return 0
	}
	_, cAlways, _ := createTestContainer(t, te, "gvxd-always", map[string]any{
		"Image": "alpine",
		"HostConfig": map[string]any{
			"RestartPolicy": RestartPolicy{Name: "always"},
		},
	})
	_, _ = te.do(t, http.MethodPost, "/containers/"+cAlways.ID+"/start", nil)
	waitUntil(t, 2*time.Second, "always restarts twice", func() bool {
		c, err := te.lookup(cAlways.ID)
		return err == nil && c.RestartCount >= 2 && c.State.Running()
	})
	if status, raw := te.do(t, http.MethodPost, "/containers/"+cAlways.ID+"/stop?t=1", nil); status != http.StatusNoContent {
		t.Fatalf("stop always: %d %s", status, raw)
	}
	c, _ := te.lookup(cAlways.ID)
	if c.State.Status != StatusExited || !c.State.StoppedByUser {
		t.Fatalf("expected exited+StoppedByUser, got %+v", c.State)
	}
}

func TestContainerPersistenceAndReopen(t *testing.T) {
	dir := t.TempDir()
	fake := newFakeRunner()
	fake.addImage("alpine", ImageConfig{
		Cmd:     []string{"/bin/sh"},
		Volumes: map[string]struct{}{"/anon": {}},
	})

	te := reopenTestEngine(t, dir, fake)

	_, cPlain, _ := createTestContainer(t, te, "gvxd-plain", map[string]any{"Image": "alpine"})
	_, cAlways, _ := createTestContainer(t, te, "gvxd-always", map[string]any{
		"Image":      "alpine",
		"HostConfig": map[string]any{"RestartPolicy": RestartPolicy{Name: "always"}},
	})
	_, cUnlessRunning, _ := createTestContainer(t, te, "gvxd-unless-run", map[string]any{
		"Image":      "alpine",
		"HostConfig": map[string]any{"RestartPolicy": RestartPolicy{Name: "unless-stopped"}},
	})
	_, cUnlessStopped, _ := createTestContainer(t, te, "gvxd-unless-stop", map[string]any{
		"Image":      "alpine",
		"HostConfig": map[string]any{"RestartPolicy": RestartPolicy{Name: "unless-stopped"}},
	})

	for _, id := range []string{cPlain.ID, cAlways.ID, cUnlessRunning.ID, cUnlessStopped.ID} {
		if status, raw := te.do(t, http.MethodPost, "/containers/"+id+"/start", nil); status != http.StatusNoContent {
			t.Fatalf("start %s: %d %s", id, status, raw)
		}
	}
	// User explicitly stops cUnlessStopped.
	if status, raw := te.do(t, http.MethodPost, "/containers/"+cUnlessStopped.ID+"/stop?t=1", nil); status != http.StatusNoContent {
		t.Fatalf("stop cUnlessStopped: %d %s", status, raw)
	}

	recPlainBefore, _ := te.lookup(cPlain.ID)
	anonVol := recPlainBefore.AnonymousVolumes[0]

	// Close the first engine cleanly (stops running containers without setting StoppedByUser).
	te.srv.Close()
	if err := te.Close(time.Second); err != nil {
		t.Fatal(err)
	}

	// Also simulate a crash by writing a container with status "running" directly
	// into containers.json? We already check normal reopen here; let's reopen now.
	te2 := reopenTestEngine(t, dir, fake)

	gotPlain, err := te2.lookup(cPlain.ID)
	if err != nil || gotPlain.State.Status != StatusExited {
		t.Fatalf("plain after reopen: %+v %v", gotPlain.State, err)
	}
	gotAlways, _ := te2.lookup(cAlways.ID)
	if !gotAlways.State.Running() {
		t.Fatalf("always should be running after reopen, got %+v", gotAlways.State)
	}
	gotUnlessRun, _ := te2.lookup(cUnlessRunning.ID)
	if !gotUnlessRun.State.Running() {
		t.Fatalf("unless-stopped (stopped by engine) should be running after reopen, got %+v", gotUnlessRun.State)
	}
	gotUnlessStop, _ := te2.lookup(cUnlessStopped.ID)
	if gotUnlessStop.State.Status != StatusExited {
		t.Fatalf("unless-stopped (stopped by user) should stay exited after reopen, got %+v", gotUnlessStop.State)
	}

	// Anonymous volume tracking must survive reopen so rm -v deletes it.
	if status, raw := te2.do(t, http.MethodDelete, "/containers/"+cPlain.ID+"?v=1", nil); status != http.StatusNoContent {
		t.Fatalf("rm -v after reopen: %d %s", status, raw)
	}
	vols, _ := fake.Volumes(t.Context())
	for _, v := range vols {
		if v.Name == anonVol {
			t.Fatalf("anonymous volume %s survived rm -v after reopen", anonVol)
		}
	}
}

func TestContainerNetworksPruneAndEvents(t *testing.T) {
	te := newTestEngine(t)
	te.fake.addImage("alpine", ImageConfig{Cmd: []string{"/bin/sh"}})

	// Missing user network → 404.
	if status, _, _ := createTestContainer(t, te, "bad-net", map[string]any{
		"Image":      "alpine",
		"HostConfig": map[string]any{"NetworkMode": "no-such-net"},
	}); status != http.StatusNotFound {
		t.Fatalf("missing network: got %d, want 404", status)
	}

	// EndpointsConfig with "default" maps to "bridge" (sent by docker CLI 29.x).
	status, cDef, raw := createTestContainer(t, te, "gvxd-default-net", map[string]any{
		"Image":      "alpine",
		"HostConfig": map[string]any{"NetworkMode": "default"},
		"NetworkingConfig": map[string]any{
			"EndpointsConfig": map[string]any{"default": map[string]any{}},
		},
	})
	if status != http.StatusCreated {
		t.Fatalf("create default endpoint: %d %s", status, raw)
	}
	recDef, _ := te.lookup(cDef.ID)
	if _, ok := recDef.Networks["bridge"]; !ok || recDef.HostConfig.NetworkMode != "bridge" {
		t.Fatalf("default endpoint did not map to bridge: mode=%q nets=%+v", recDef.HostConfig.NetworkMode, recDef.Networks)
	}
	_, _ = te.do(t, http.MethodDelete, "/containers/"+cDef.ID, nil)

	// Create a user network via C's handler.
	status, raw = te.do(t, http.MethodPost, "/networks/create", map[string]any{"Name": "gvxd-net"})
	if status != http.StatusCreated {
		t.Fatalf("create network: %d %s", status, raw)
	}

	// First container on gvxd-net: no warning.
	status, c1, raw := createTestContainer(t, te, "gvxd-n1", map[string]any{
		"Image":      "alpine",
		"HostConfig": map[string]any{"NetworkMode": "gvxd-net"},
		"NetworkingConfig": map[string]any{
			"EndpointsConfig": map[string]any{
				"gvxd-net": map[string]any{"Aliases": []string{"web"}},
			},
		},
	})
	if status != http.StatusCreated {
		t.Fatalf("create n1: %d %s", status, raw)
	}
	// Second container on gvxd-net: gets inter-container networking warning.
	status, c2, raw := createTestContainer(t, te, "gvxd-n2", map[string]any{
		"Image":      "alpine",
		"HostConfig": map[string]any{"NetworkMode": "gvxd-net"},
	})
	if status != http.StatusCreated {
		t.Fatalf("create n2: %d %s", status, raw)
	}
	if len(c2.Warnings) == 0 || !strings.Contains(c2.Warnings[0], "gvxd-net") {
		t.Fatalf("expected multi-container network warning, got %v", c2.Warnings)
	}

	// Inspect shows NetworkSettings.Networks["gvxd-net"] with aliases and 10.0.2.x.
	_, raw = te.do(t, http.MethodGet, "/containers/"+c1.ID+"/json", nil)
	var ins containerInspectResponse
	_ = json.Unmarshal(raw, &ins)
	ep, ok := ins.NetworkSettings.Networks["gvxd-net"]
	if !ok || ep.IPAddress != "10.0.2.15" || ep.Gateway != "10.0.2.2" || !slices.Contains(ep.Aliases, "web") {
		t.Fatalf("unexpected network endpoint: %+v", ep)
	}

	// Start c1, keep c2 stopped, then prune: only c2 is pruned.
	_, _ = te.do(t, http.MethodPost, "/containers/"+c1.ID+"/start", nil)
	status, raw = te.do(t, http.MethodPost, "/containers/prune", nil)
	if status != http.StatusOK {
		t.Fatalf("prune: %d %s", status, raw)
	}
	var pr containerPruneResponse
	_ = json.Unmarshal(raw, &pr)
	if !slices.Equal(pr.ContainersDeleted, []string{c2.ID}) {
		t.Fatalf("prune deleted = %v, want [%s]", pr.ContainersDeleted, c2.ID)
	}

	// Verify events recorded create, start, destroy.
	until := strconv.FormatInt(time.Now().Unix()+1, 10)
	status, raw = te.do(t, http.MethodGet, "/events?since=1&until="+until, nil)
	if status != http.StatusOK {
		t.Fatalf("events: %d %s", status, raw)
	}
	actions := map[string]bool{}
	sc := bufio.NewScanner(bytes.NewReader(raw))
	for sc.Scan() {
		var ev struct {
			Type   string `json:"Type"`
			Action string `json:"Action"`
		}
		if json.Unmarshal(sc.Bytes(), &ev) == nil && ev.Type == "container" {
			actions[ev.Action] = true
		}
	}
	for _, want := range []string{"create", "start", "destroy"} {
		if !actions[want] {
			t.Fatalf("missing container event %q in %v", want, actions)
		}
	}
}

func TestContainerHostNetworkWarningAndCrashLoad(t *testing.T) {
	dir := t.TempDir()
	fake := newFakeRunner()
	fake.addImage("alpine", ImageConfig{Cmd: []string{"/bin/sh"}})

	te := reopenTestEngine(t, dir, fake)

	// Host network mode warns and runs on NAT.
	status, cHost, raw := createTestContainer(t, te, "gvxd-hostnet", map[string]any{
		"Image":      "alpine",
		"HostConfig": map[string]any{"NetworkMode": "host"},
	})
	if status != http.StatusCreated {
		t.Fatalf("create host net: %d %s", status, raw)
	}
	if len(cHost.Warnings) == 0 || !strings.Contains(cHost.Warnings[0], "Host networking") {
		t.Fatalf("expected host networking warning, got %v", cHost.Warnings)
	}
	rec, _ := te.lookup(cHost.ID)
	if rec.Spec.Network != "nat" {
		t.Fatalf("host network spec.Network = %q, want nat", rec.Spec.Network)
	}

	// Simulate an unclean shutdown by marking status=running in containers.json directly.
	te.srv.Close()
	_ = te.Close(time.Second)

	path := filepath.Join(dir, "containers.json")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var sf containerStoreFile
	if err := json.Unmarshal(data, &sf); err != nil {
		t.Fatal(err)
	}
	sf.Containers[0].State.Status = StatusRunning
	sf.Containers[0].State.ExitCode = 0
	sf.Containers[0].State.StoppedByUser = false
	if err := saveFile(path, sf); err != nil {
		t.Fatal(err)
	}

	te2 := reopenTestEngine(t, dir, fake)
	rec2, err := te2.lookup(cHost.ID)
	if err != nil {
		t.Fatal(err)
	}
	if rec2.State.Status != StatusExited || rec2.State.ExitCode != 255 || rec2.State.FinishedAt.IsZero() {
		t.Fatalf("unclean crash container state = %+v, want exited with code 255", rec2.State)
	}
}

// Keep rungv import referenced by verifying Spec.RunArgs round-trips FormatArgs.
func TestContainerSpecRunArgs(t *testing.T) {
	argv := []string{"/bin/sh", "-c", "echo 'hello world'"}
	spec, err := buildSandboxSpec(randomHex64(), "gvxd-args", "", ContainerConfig{
		Image:      "alpine",
		WorkingDir: "/",
		Hostname:   "host1",
		Env:        []string{defaultContainerPATH},
	}, HostConfig{NetworkMode: "bridge"}, argv, nil)
	if err != nil {
		t.Fatal(err)
	}
	gotArgs := spec.RunArgs(sandbox.Paths{})
	wantTail := []string{"--entrypoint", "/bin/sh"}
	if !slices.Contains(gotArgs, "--entrypoint") {
		t.Fatalf("missing --entrypoint in %v", gotArgs)
	}
	_ = wantTail
	cmdParts, err := rungv.SplitArgs(spec.Command)
	if err != nil || !slices.Equal(cmdParts, argv[1:]) {
		t.Fatalf("SplitArgs(%q) = %v, %v; want %v", spec.Command, cmdParts, err, argv[1:])
	}
}

func TestContainerBuildkitNotSupported(t *testing.T) {
	te := newTestEngine(t)
	te.fake.addImage("alpine", ImageConfig{Cmd: []string{"/bin/sh"}})

	want := buildDriverNotSupportedMessage()
	check501 := func(label string, status int, raw []byte) {
		t.Helper()
		var errResp struct{ Message string }
		_ = json.Unmarshal(raw, &errResp)
		if status != http.StatusNotImplemented || errResp.Message != want {
			t.Fatalf("%s: got %d %q, want 501 %q", label, status, errResp.Message, want)
		}
	}

	// Inspect of missing buildx_buildkit_* container returns 501 instead of 404.
	st, raw := te.do(t, http.MethodGet, "/v1.52/containers/buildx_buildkit_default/json", nil)
	check501("inspect buildx_buildkit_default", st, raw)

	// Create backstops: name prefix and moby/buildkit image variants.
	st, _, raw = createTestContainer(t, te, "buildx_buildkit_default", map[string]any{"Image": "alpine"})
	check501("create name buildx_buildkit_default", st, raw)

	for _, img := range []string{
		"moby/buildkit",
		"moby/buildkit:buildx-stable-1",
		"docker.io/moby/buildkit",
		"docker.io/moby/buildkit:latest",
		"docker.io/moby/buildkit@sha256:" + fakeHex("bk"),
	} {
		st, _, raw = createTestContainer(t, te, "", map[string]any{"Image": img})
		check501("create image "+img, st, raw)
	}
}

func TestContainerAutoRemoveOnStartFailure(t *testing.T) {
	te := newTestEngine(t)
	te.fake.addImage("alpine:3.20", ImageConfig{
		Cmd:     []string{"/bin/sh"},
		Volumes: map[string]struct{}{"/data": {}},
	})

	// Case 1: busy port causes start failure → AutoRemove cleans up container
	// and anonymous volume, and wait?condition=removed unblocks.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	busyPort := strconv.Itoa(ln.Addr().(*net.TCPAddr).Port)

	status, cBusy, raw := createTestContainer(t, te, "gvxd-ar-busy", map[string]any{
		"Image": "alpine:3.20",
		"HostConfig": map[string]any{
			"AutoRemove":   true,
			"PortBindings": map[string][]PortBinding{"80/tcp": {{HostPort: busyPort}}},
		},
	})
	if status != http.StatusCreated {
		t.Fatalf("create busy AutoRemove: %d %s", status, raw)
	}
	recBusy, _ := te.lookup(cBusy.ID)
	anonBusy := recBusy.AnonymousVolumes[0]

	waitCh := make(chan int, 1)
	go func() {
		st, _ := te.do(t, http.MethodPost, "/containers/"+cBusy.ID+"/wait?condition=removed", nil)
		waitCh <- st
	}()
	time.Sleep(15 * time.Millisecond)

	if st, body := te.do(t, http.MethodPost, "/containers/"+cBusy.ID+"/start", nil); st != http.StatusInternalServerError {
		t.Fatalf("start busy AutoRemove: got %d %s, want 500", st, body)
	}
	select {
	case st := <-waitCh:
		if st != http.StatusOK {
			t.Fatalf("wait removed status = %d, want 200", st)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("wait?condition=removed hung after AutoRemove busy-port start failure")
	}
	if _, err := te.lookup(cBusy.ID); statusOf(err) != http.StatusNotFound {
		t.Fatalf("AutoRemove container leaked after busy port: %v", err)
	}
	vols, _ := te.fake.Volumes(t.Context())
	for _, v := range vols {
		if v.Name == anonBusy {
			t.Fatalf("anonymous volume %s leaked after busy-port start failure", anonBusy)
		}
	}

	// Case 2: locked volume / runner.Start error → AutoRemove cleans up and unblocks wait.
	status, cVol, raw := createTestContainer(t, te, "gvxd-ar-vol", map[string]any{
		"Image":      "alpine:3.20",
		"HostConfig": map[string]any{"AutoRemove": true},
	})
	if status != http.StatusCreated {
		t.Fatalf("create vol AutoRemove: %d %s", status, raw)
	}
	recVol, _ := te.lookup(cVol.ID)
	anonVol := recVol.AnonymousVolumes[0]

	te.fake.StartErr = errors.New("rungv run: volume v is in use by another sandbox")
	defer func() { te.fake.StartErr = nil }()

	waitCh2 := make(chan int, 1)
	go func() {
		st, _ := te.do(t, http.MethodPost, "/containers/"+cVol.ID+"/wait?condition=removed", nil)
		waitCh2 <- st
	}()
	time.Sleep(15 * time.Millisecond)

	if st, body := te.do(t, http.MethodPost, "/containers/"+cVol.ID+"/start", nil); st != http.StatusConflict {
		t.Fatalf("start locked-vol AutoRemove: got %d %s, want 409", st, body)
	}
	select {
	case st := <-waitCh2:
		if st != http.StatusOK {
			t.Fatalf("wait removed status = %d, want 200", st)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("wait?condition=removed hung after AutoRemove StartErr")
	}
	if _, err := te.lookup(cVol.ID); statusOf(err) != http.StatusNotFound {
		t.Fatalf("AutoRemove container leaked after StartErr: %v", err)
	}
	vols, _ = te.fake.Volumes(t.Context())
	for _, v := range vols {
		if v.Name == anonVol {
			t.Fatalf("anonymous volume %s leaked after StartErr", anonVol)
		}
	}
}

func TestContainerConcurrentPortAllocation(t *testing.T) {
	te := newTestEngine(t)
	te.fake.addImage("nginx:alpine", ImageConfig{Cmd: []string{"nginx"}})

	// Pick a currently free port and release it.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	sharedPort := strconv.Itoa(ln.Addr().(*net.TCPAddr).Port)
	_ = ln.Close()

	_, c1, _ := createTestContainer(t, te, "gvxd-race-1", map[string]any{
		"Image": "nginx:alpine",
		"HostConfig": map[string]any{
			"PortBindings": map[string][]PortBinding{"80/tcp": {{HostPort: sharedPort}}},
		},
	})
	_, c2, _ := createTestContainer(t, te, "gvxd-race-2", map[string]any{
		"Image": "nginx:alpine",
		"HostConfig": map[string]any{
			"PortBindings": map[string][]PortBinding{"80/tcp": {{HostPort: sharedPort}}},
		},
	})

	startCh := make(chan struct{})
	resCh := make(chan int, 2)
	for _, id := range []string{c1.ID, c2.ID} {
		go func(id string) {
			<-startCh
			st, _ := te.do(t, http.MethodPost, "/containers/"+id+"/start", nil)
			resCh <- st
		}(id)
	}
	close(startCh)

	s1, s2 := <-resCh, <-resCh
	okCount, errCount := 0, 0
	for _, s := range []int{s1, s2} {
		switch s {
		case http.StatusNoContent:
			okCount++
		case http.StatusInternalServerError:
			errCount++
		}
	}
	if okCount != 1 || errCount != 1 {
		t.Fatalf("concurrent same-port start statuses = [%d, %d], want one 204 and one 500", s1, s2)
	}
	if len(te.fake.Started()) != 1 {
		t.Fatalf("runner.Start called %d times for contested port, want 1", len(te.fake.Started()))
	}

	// Concurrent dynamic port allocations must all receive distinct host ports.
	const nDyn = 6
	dynIDs := make([]string, nDyn)
	for i := range nDyn {
		_, c, _ := createTestContainer(t, te, fmt.Sprintf("gvxd-dyn-%d", i), map[string]any{
			"Image": "nginx:alpine",
			"HostConfig": map[string]any{
				"PortBindings": map[string][]PortBinding{"80/tcp": {{HostPort: ""}}},
			},
		})
		dynIDs[i] = c.ID
	}
	dynStart := make(chan struct{})
	dynRes := make(chan int, nDyn)
	for _, id := range dynIDs {
		go func(id string) {
			<-dynStart
			st, _ := te.do(t, http.MethodPost, "/containers/"+id+"/start", nil)
			dynRes <- st
		}(id)
	}
	close(dynStart)
	for range nDyn {
		if st := <-dynRes; st != http.StatusNoContent {
			t.Fatalf("concurrent dynamic start returned %d, want 204", st)
		}
	}
	seenPorts := map[string]bool{}
	for _, id := range dynIDs {
		rec, _ := te.lookup(id)
		hp := rec.Ports["80/tcp"][0].HostPort
		if hp == "" || seenPorts[hp] {
			t.Fatalf("duplicate or empty dynamic host port %q across concurrent starts", hp)
		}
		seenPorts[hp] = true
	}
}

func TestContainerRestartFailureAndActiveState(t *testing.T) {
	te := newTestEngine(t)
	te.ctr.restartBase = 15 * time.Millisecond
	te.ctr.restartMax = 40 * time.Millisecond
	te.fake.addImage("alpine", ImageConfig{Cmd: []string{"/bin/sh"}})

	// Case 1: Port busy during doRestart must reschedule instead of wedging,
	// then succeed once the port is released.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	_ = ln.Close()

	var blocker net.Listener
	var runs atomic.Int32
	te.fake.Script = func(p *fakeProc) int {
		if runs.Add(1) == 1 {
			var lerr error
			blocker, lerr = net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
			if lerr != nil {
				t.Errorf("blocker listen: %v", lerr)
			}
			return 1
		}
		<-p.Done()
		return 0
	}

	_, cPort, _ := createTestContainer(t, te, "gvxd-retry-port", map[string]any{
		"Image": "alpine",
		"HostConfig": map[string]any{
			"RestartPolicy": RestartPolicy{Name: "always"},
			"PortBindings":  map[string][]PortBinding{"80/tcp": {{HostPort: strconv.Itoa(port)}}},
		},
	})
	if st, raw := te.do(t, http.MethodPost, "/containers/"+cPort.ID+"/start", nil); st != http.StatusNoContent {
		t.Fatalf("start retry-port: %d %s", st, raw)
	}
	// Wait until doRestart has failed at least once due to the busy port.
	waitUntil(t, 2*time.Second, "doRestart records port error", func() bool {
		c, _ := te.lookup(cPort.ID)
		return c.State.Status == StatusRestarting && strings.Contains(c.State.Error, "port is already allocated")
	})
	// Free the port: the rescheduled timer must bring the container back to Running.
	if blocker != nil {
		_ = blocker.Close()
	}
	waitUntil(t, 2*time.Second, "retry-port recovers to running", func() bool {
		c, _ := te.lookup(cPort.ID)
		return c.State.Status == StatusRunning
	})
	_, _ = te.do(t, http.MethodPost, "/containers/"+cPort.ID+"/stop?t=1", nil)

	// Case 2: runner.Start error during doRestart must reschedule instead of abandoning policy.
	runs.Store(0)
	te.fake.Script = func(p *fakeProc) int {
		if runs.Add(1) == 1 {
			te.fake.mu.Lock()
			te.fake.StartErr = errors.New("temporary start failure")
			te.fake.mu.Unlock()
			return 1
		}
		<-p.Done()
		return 0
	}
	_, cStartErr, _ := createTestContainer(t, te, "gvxd-retry-err", map[string]any{
		"Image":      "alpine",
		"HostConfig": map[string]any{"RestartPolicy": RestartPolicy{Name: "always"}},
	})
	_, _ = te.do(t, http.MethodPost, "/containers/"+cStartErr.ID+"/start", nil)
	waitUntil(t, 2*time.Second, "doRestart records runner.Start error", func() bool {
		c, _ := te.lookup(cStartErr.ID)
		return c.State.Status == StatusRestarting && strings.Contains(c.State.Error, "temporary start failure")
	})
	te.fake.mu.Lock()
	te.fake.StartErr = nil
	te.fake.mu.Unlock()
	waitUntil(t, 2*time.Second, "retry-err recovers to running", func() bool {
		c, _ := te.lookup(cStartErr.ID)
		return c.State.Status == StatusRunning
	})
	_, _ = te.do(t, http.MethodPost, "/containers/"+cStartErr.ID+"/stop?t=1", nil)

	// Case 3: StatusRestarting (with Pid == 0) is active in ps, inspect,
	// wait?condition=not-running, and kill cancels the restart loop.
	te.ctr.restartBase = 5 * time.Second
	te.ctr.restartMax = 10 * time.Second
	te.fake.Script = func(p *fakeProc) int { return 1 }

	_, cActive, _ := createTestContainer(t, te, "gvxd-restarting-active", map[string]any{
		"Image":      "alpine",
		"HostConfig": map[string]any{"RestartPolicy": RestartPolicy{Name: "always"}},
	})
	_, _ = te.do(t, http.MethodPost, "/containers/"+cActive.ID+"/start", nil)
	waitUntil(t, 2*time.Second, "enters StatusRestarting", func() bool {
		c, _ := te.lookup(cActive.ID)
		return c.State.Status == StatusRestarting && c.State.Pid == 0
	})

	// ps (without all=1) includes the restarting container.
	_, raw := te.do(t, http.MethodGet, "/containers/json", nil)
	var listRows []containerListItem
	_ = json.Unmarshal(raw, &listRows)
	foundInPS := false
	for _, row := range listRows {
		if row.ID == cActive.ID && row.State == StatusRestarting {
			foundInPS = true
		}
	}
	if !foundInPS {
		t.Fatalf("restarting container missing from default ps: %s", raw)
	}

	// inspect reports Running: true and Restarting: true.
	_, raw = te.do(t, http.MethodGet, "/containers/"+cActive.ID+"/json", nil)
	var ins containerInspectResponse
	_ = json.Unmarshal(raw, &ins)
	if !ins.State.Running || !ins.State.Restarting || ins.State.Status != StatusRestarting {
		t.Fatalf("inspect restarting state = %+v, want Running=true Restarting=true", ins.State)
	}

	// wait?condition=not-running stays blocked while restarting until kill cancels the loop.
	waitDone := make(chan int, 1)
	go func() {
		st, _ := te.do(t, http.MethodPost, "/containers/"+cActive.ID+"/wait?condition=not-running", nil)
		waitDone <- st
	}()
	select {
	case st := <-waitDone:
		t.Fatalf("wait?condition=not-running returned prematurely (%d) while container was restarting", st)
	case <-time.After(40 * time.Millisecond):
	}

	// kill cancels the restart loop, transitions to exited, and unblocks wait.
	if st, body := te.do(t, http.MethodPost, "/containers/"+cActive.ID+"/kill?signal=SIGKILL", nil); st != http.StatusNoContent {
		t.Fatalf("kill restarting container: got %d %s, want 204", st, body)
	}
	select {
	case st := <-waitDone:
		if st != http.StatusOK {
			t.Fatalf("wait after kill returned %d, want 200", st)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("wait?condition=not-running did not unblock after kill of restarting container")
	}
	rec, _ := te.lookup(cActive.ID)
	if rec.State.Status != StatusExited || !rec.State.StoppedByUser {
		t.Fatalf("restarting container after kill = %+v, want exited+StoppedByUser", rec.State)
	}
}

func TestContainerNameRegexFilterAndPruneAnonVolumes(t *testing.T) {
	te := newTestEngine(t)
	te.fake.addImage("alpine", ImageConfig{
		Cmd:     []string{"/bin/sh"},
		Volumes: map[string]struct{}{"/anon": {}},
	})

	_, cFoo, _ := createTestContainer(t, te, "foo", map[string]any{
		"Image":      "alpine",
		"HostConfig": map[string]any{"Binds": []string{"keep-named:/named"}},
	})
	_, cFoobar, _ := createTestContainer(t, te, "foobar", map[string]any{"Image": "alpine"})
	_, cMyfoo, _ := createTestContainer(t, te, "myfoo", map[string]any{"Image": "alpine"})

	listIDs := func(pattern string) []string {
		t.Helper()
		f := fmt.Sprintf(`{"name":[%q]}`, pattern)
		st, raw := te.do(t, http.MethodGet, "/containers/json?all=1&filters="+url.QueryEscape(f), nil)
		if st != http.StatusOK {
			t.Fatalf("name filter %q: %d %s", pattern, st, raw)
		}
		var rows []containerListItem
		_ = json.Unmarshal(raw, &rows)
		ids := make([]string, len(rows))
		for i, r := range rows {
			ids[i] = r.ID
		}
		slices.Sort(ids)
		return ids
	}

	if got := listIDs("^/foo$"); !slices.Equal(got, []string{cFoo.ID}) {
		t.Fatalf("^/foo$ matched %v, want [%s]", got, cFoo.ID)
	}
	if got := listIDs("^foo$"); !slices.Equal(got, []string{cFoo.ID}) {
		t.Fatalf("^foo$ matched %v, want [%s]", got, cFoo.ID)
	}
	wantPrefix := []string{cFoo.ID, cFoobar.ID}
	slices.Sort(wantPrefix)
	if got := listIDs("^/foo"); !slices.Equal(got, wantPrefix) {
		t.Fatalf("^/foo matched %v, want %v", got, wantPrefix)
	}
	wantSuffix := []string{cFoo.ID, cMyfoo.ID}
	slices.Sort(wantSuffix)
	if got := listIDs("foo$"); !slices.Equal(got, wantSuffix) {
		t.Fatalf("foo$ matched %v, want %v", got, wantSuffix)
	}
	// Invalid regex returns 400.
	if st, _ := te.do(t, http.MethodGet, "/containers/json?all=1&filters="+url.QueryEscape(`{"name":["[bad"]}`), nil); st != http.StatusBadRequest {
		t.Fatalf("invalid regex filter: got %d, want 400", st)
	}

	// Prune removes anonymous volumes of pruned containers while keeping named volumes.
	recFoo, _ := te.lookup(cFoo.ID)
	anonVol := recFoo.AnonymousVolumes[0]
	if st, raw := te.do(t, http.MethodPost, "/containers/prune", nil); st != http.StatusOK {
		t.Fatalf("prune: %d %s", st, raw)
	}
	vols, _ := te.fake.Volumes(t.Context())
	volNames := map[string]bool{}
	for _, v := range vols {
		volNames[v.Name] = true
	}
	if volNames[anonVol] {
		t.Fatalf("prune did not remove anonymous volume %s", anonVol)
	}
	if !volNames["keep-named"] {
		t.Fatalf("prune removed named volume keep-named!")
	}
}

func TestContainerResolvedImageRef(t *testing.T) {
	te := newTestEngine(t)
	imgID := te.fake.addImage("alpine:3.20", ImageConfig{Cmd: []string{"/bin/sh"}})

	// 1. Create with docker.io/library/ prefix → Container.Image/Config.Image keep
	// the client's ref, while Spec.Image uses the resolved rungv cache ref.
	st, c1, raw := createTestContainer(t, te, "gvxd-fqn", map[string]any{
		"Image": "docker.io/library/alpine:3.20",
	})
	if st != http.StatusCreated {
		t.Fatalf("create docker.io/library ref: %d %s", st, raw)
	}
	rec1, _ := te.lookup(c1.ID)
	if rec1.Image != "docker.io/library/alpine:3.20" || rec1.Config.Image != "docker.io/library/alpine:3.20" {
		t.Fatalf("client ref not preserved: container=%q config=%q", rec1.Image, rec1.Config.Image)
	}
	if rec1.Spec.Image != "alpine:3.20" {
		t.Fatalf("Spec.Image = %q, want resolved cache ref alpine:3.20", rec1.Spec.Image)
	}
	if st, raw := te.do(t, http.MethodPost, "/containers/"+c1.ID+"/start", nil); st != http.StatusNoContent {
		t.Fatalf("start fqn: %d %s", st, raw)
	}

	// 2. Create by short image ID prefix → Container.Image/Config.Image keep
	// the short ID, while Spec.Image uses the resolved cache ref (full ID).
	shortID := strings.TrimPrefix(imgID, "sha256:")[:12]
	st, c2, raw := createTestContainer(t, te, "gvxd-shortid", map[string]any{
		"Image": shortID,
	})
	if st != http.StatusCreated {
		t.Fatalf("create by short image ID: %d %s", st, raw)
	}
	rec2, _ := te.lookup(c2.ID)
	if rec2.Image != shortID || rec2.Config.Image != shortID || rec2.ImageID != imgID {
		t.Fatalf("short ID record mismatch: img=%q cfg=%q id=%q", rec2.Image, rec2.Config.Image, rec2.ImageID)
	}
	if rec2.Spec.Image == shortID || rec2.Spec.Image == "" {
		t.Fatalf("Spec.Image = %q, expected resolved cache ref (not short ID)", rec2.Spec.Image)
	}
	if st, raw := te.do(t, http.MethodPost, "/containers/"+c2.ID+"/start", nil); st != http.StatusNoContent {
		t.Fatalf("start shortid: %d %s", st, raw)
	}
	started := te.fake.Started()
	if len(started) != 2 || started[0].Image != "alpine:3.20" || started[1].Image != rec2.Spec.Image {
		t.Fatalf("started specs got images [%q, %q]", started[0].Image, started[1].Image)
	}
}
