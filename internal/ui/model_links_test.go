package ui

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ironpark/ggui"

	"github.com/ironpark/gvx-desktop/internal/dockerapi"
	"github.com/ironpark/gvx-desktop/internal/kubeapi"
	"github.com/ironpark/gvx-desktop/internal/recipes"
	"github.com/ironpark/gvx-desktop/internal/sandbox"
	"github.com/ironpark/gvx-desktop/internal/store"
)

// What docker context ls --format '{{json .}}' writes, as docker 29.10
// wrote it: with the gvx context in use, and with DOCKER_HOST set, which
// makes the default context the current one whatever was chosen.
const (
	socket      = "unix:///Users/me/.gvx/docker.sock"
	dockerInUse = `{"Current":false,"Description":"Current DOCKER_HOST based configuration","DockerEndpoint":"unix:///var/run/docker.sock","Error":"","Name":"default"}
{"Current":true,"Description":"","DockerEndpoint":"unix:///Users/me/.gvx/docker.sock","Error":"","Name":"gvx"}
`
	dockerHostSet = `{"Current":true,"Description":"Current DOCKER_HOST based configuration","DockerEndpoint":"tcp://1.2.3.4:5","Error":"","Name":"default"}
{"Current":false,"Description":"","DockerEndpoint":"unix:///Users/me/.gvx/docker.sock","Error":"","Name":"gvx"}
`
)

func TestDockerContextStatus(t *testing.T) {
	for _, c := range []struct {
		name, out, want string
		status          ContextStatus
		current         string
	}{
		{"gvx in use", dockerInUse, socket, ContextInUse, "gvx"},
		{"a slash at the end changes nothing", dockerInUse, socket + "/", ContextInUse, "gvx"},
		{"gvx there, another in use", dockerHostSet, socket, ContextElsewhere, "default"},
		{"gvx points at the socket, the engine moved to TCP", dockerInUse, "tcp://127.0.0.1:2375", ContextStale, "gvx"},
		{"no gvx", `{"Current":true,"DockerEndpoint":"unix:///var/run/docker.sock","Name":"default"}`, socket, ContextMissing, "default"},
		{"a list, with a warning before it", "Warning: something\n" + `[{"Current":true,"DockerEndpoint":"unix:///Users/me/.gvx/docker.sock","Name":"gvx"},{"Name":"default"}]`, socket, ContextInUse, "gvx"},
		{"a warning line among the objects", "WARNING: x\n" + dockerHostSet, socket, ContextElsewhere, "default"},
	} {
		got, err := parseDockerContexts([]byte(c.out), DockerContextName)
		if err != nil {
			t.Errorf("%s: %v", c.name, err)
			continue
		}
		got.Checked, got.CLI = true, "/bin/docker"
		if s := got.Status(c.want); s != c.status || got.Current != c.current {
			t.Errorf("%s: status %d, current %q; want %d, %q", c.name, s, got.Current, c.status, c.current)
		}
	}
	if _, err := parseDockerContexts([]byte("\n"), DockerContextName); err == nil {
		t.Error("no contexts at all read as some")
	}
	for _, c := range []struct {
		name   string
		c      CLIContext
		status ContextStatus
	}{
		{"not asked yet", CLIContext{}, ContextUnknown},
		{"no tool", CLIContext{Checked: true}, ContextNoCLI},
		{"no answer", CLIContext{Checked: true, CLI: "x", Err: "boom"}, ContextFailed},
		{"no kubeconfig yet", CLIContext{Checked: true, CLI: "x", Pending: true, Exists: true}, ContextPending},
		{"stale credentials", CLIContext{Checked: true, CLI: "x", Exists: true, InUse: true, Stale: true}, ContextStale},
		{"an override in the environment is told, not counted", CLIContext{Checked: true, CLI: "x", Exists: true, InUse: true, Env: "DOCKER_HOST"}, ContextInUse},
	} {
		if s := c.c.Status(""); s != c.status {
			t.Errorf("%s: %d, want %d", c.name, s, c.status)
		}
	}
}

// The variables that override a context are named when set, and kept from
// the tools the app runs, in whatever case Windows spells them.
func TestOverrides(t *testing.T) {
	env := map[string]string{"DOCKER_CONTEXT": "x", "KUBECONFIG": " "}
	lookup := func(k string) (string, bool) { v, ok := env[k]; return v, ok }
	if got := envOverride(lookup, "DOCKER_HOST", "DOCKER_CONTEXT"); got != "DOCKER_CONTEXT" {
		t.Errorf("docker: %q", got)
	}
	if got := envOverride(lookup, "KUBECONFIG"); got != "" {
		t.Errorf("a blank KUBECONFIG: %q", got)
	}
	got := withoutOverrides([]string{"PATH=/bin", "docker_host=tcp://x", "DOCKER_CONTEXT=y", "KUBECONFIG=/k", "HOME=/h"})
	if !slices.Equal(got, []string{"PATH=/bin", "HOME=/h"}) {
		t.Errorf("left %v", got)
	}
}

// kubeconfigJSON writes a kubeconfig as kubectl config view --raw -o json
// does, with one context and its cluster and user, all named name.
func kubeconfigJSON(t *testing.T, name, server, ca, cert, key, current string) string {
	t.Helper()
	cfg := map[string]any{
		"kind": "Config", "apiVersion": "v1",
		"clusters":        []any{map[string]any{"name": name, "cluster": map[string]any{"server": server, "certificate-authority-data": ca}}},
		"users":           []any{map[string]any{"name": name, "user": map[string]any{"client-certificate-data": cert, "client-key-data": key}}},
		"contexts":        []any{map[string]any{"name": name, "context": map[string]any{"cluster": name, "user": name}}},
		"current-context": current,
	}
	b, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func b64(s string) string { return base64.StdEncoding.EncodeToString([]byte(s)) }

func TestKubeLink(t *testing.T) {
	const server = "https://127.0.0.1:6443"
	cluster, err := parseKubeconfig([]byte(kubeconfigJSON(t, "default", server, b64("CA"), b64("CERT"), b64("KEY"), "default")))
	if err != nil {
		t.Fatal(err)
	}
	mine := func(current, cert string) kubeconfig {
		k, err := parseKubeconfig([]byte(kubeconfigJSON(t, KubeContextName, server, b64("CA"), b64(cert), b64("KEY"), current)))
		if err != nil {
			t.Fatal(err)
		}
		return k
	}
	empty, _ := parseKubeconfig([]byte(`{"kind":"Config","apiVersion":"v1","preferences":{},"clusters":null,"users":null,"contexts":null,"current-context":""}`))
	for _, c := range []struct {
		name   string
		mine   kubeconfig
		status ContextStatus
	}{
		{"in use", mine(KubeContextName, "CERT"), ContextInUse},
		{"there, not in use", mine("kind-dev", "CERT"), ContextElsewhere},
		{"there, with no current context", mine("", "CERT"), ContextElsewhere},
		{"the cluster was made anew", mine(KubeContextName, "OTHER"), ContextStale},
		{"no kubeconfig of the user's", empty, ContextMissing},
	} {
		l := kubeLink(c.mine, cluster)
		l.Checked, l.CLI = true, "/bin/kubectl"
		if s := l.Status(""); s != c.status {
			t.Errorf("%s: %d, want %d (%+v)", c.name, s, c.status, l)
		}
	}
}

// Setting kubectl up reads the cluster's kubeconfig, keeps the user's as it
// was, and hands kubectl the credentials in files, never on its command
// line.
func TestKubeWrite(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	os.MkdirAll(filepath.Join(home, ".kube"), 0o700)
	os.WriteFile(filepath.Join(home, ".kube", "config"), []byte("# mine\n"), 0o600)

	file := filepath.Join(home, "k3s.yaml")
	view := kubeconfigJSON(t, "default", "https://127.0.0.1:6443", b64("THE-CA"), b64("THE-CERT"), b64("THE-KEY"), "default")
	var calls [][]string
	run := func(args ...string) ([]byte, error) {
		calls = append(calls, args)
		if slices.Contains(args, "view") {
			if !slices.Contains(args, file) {
				t.Errorf("read the user's kubeconfig to set up: %v", args)
			}
			return []byte(view), nil
		}
		for _, a := range args {
			if strings.Contains(a, b64("THE-KEY")) || strings.Contains(a, "THE-KEY") || strings.Contains(a, b64("THE-CERT")) {
				t.Errorf("a credential on the command line: %v", args)
			}
			for flag, want := range map[string]string{"--client-key=": "THE-KEY", "--client-certificate=": "THE-CERT", "--certificate-authority=": "THE-CA"} {
				if path, ok := strings.CutPrefix(a, flag); ok {
					if b, err := os.ReadFile(path); err != nil || string(b) != want {
						t.Errorf("%s holds %q, %v", a, b, err)
					}
				}
			}
		}
		return nil, nil
	}
	if err := kubeWrite(file)(context.Background(), run); err != nil {
		t.Fatal(err)
	}
	var verbs []string
	for _, c := range calls {
		if i := slices.Index(c, "config"); i >= 0 && i+1 < len(c) {
			verbs = append(verbs, c[i+1])
		}
	}
	if want := []string{"view", "set-cluster", "set-credentials", "set-context"}; !slices.Equal(verbs, want) {
		t.Errorf("ran %v", calls)
	}
	if b, err := os.ReadFile(KubeconfigBackup()); err != nil || string(b) != "# mine\n" {
		t.Errorf("backup %q, %v", b, err)
	}
	// The credentials do not outlive the setting up.
	for _, c := range calls {
		for _, a := range c {
			if _, path, ok := strings.Cut(a, "="); ok && strings.HasPrefix(path, os.TempDir()) {
				if _, err := os.Stat(path); err == nil {
					t.Errorf("%s was left", path)
				}
			}
		}
	}
	// A second setting up keeps the first backup.
	os.WriteFile(filepath.Join(home, ".kube", "config"), []byte("# changed by kubectl\n"), 0o600)
	if err := kubeWrite(file)(context.Background(), run); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(KubeconfigBackup()); string(b) != "# mine\n" {
		t.Errorf("the backup was replaced: %q", b)
	}
}

// fakeDocker answers as docker would, from a list of contexts it keeps,
// and records what it was asked.
type fakeDocker struct {
	mu       sync.Mutex
	contexts map[string]string // name: endpoint
	current  string
	calls    []string
}

// state is the context in use and where gvx points.
func (f *fakeDocker) state() (current, gvx string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.current, f.contexts["gvx"]
}

func (f *fakeDocker) run(_ context.Context, _ string, args ...string) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, strings.Join(args, " "))
	switch {
	case len(args) >= 2 && args[1] == "ls":
		var b strings.Builder
		for name, host := range f.contexts {
			fmt.Fprintf(&b, `{"Current":%v,"DockerEndpoint":%q,"Name":%q}`+"\n", name == f.current, host, name)
		}
		return []byte(b.String()), nil
	case args[1] == "create":
		if _, ok := f.contexts[args[2]]; ok {
			return nil, fmt.Errorf("context %q already exists", args[2])
		}
		f.contexts[args[2]] = strings.TrimPrefix(args[4], "host=")
	case args[1] == "update":
		f.contexts[args[2]] = strings.TrimPrefix(args[4], "host=")
	case args[1] == "use":
		if _, ok := f.contexts[args[2]]; !ok {
			return nil, errors.New("context not found")
		}
		f.current = args[2]
	}
	return nil, nil
}

// probeModel builds a model on a probe, with docker answered by f and no
// command run for real.
func probeModel(t *testing.T, f *fakeDocker) (*Model, *ggui.Probe) {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	dir := t.TempDir()
	docker := filepath.Join(dir, "docker")
	os.WriteFile(docker, []byte("#!/bin/sh\nexit 1\n"), 0o755)
	set := store.DefaultSettings()
	set.DockerPath = docker
	st := store.Store{Dir: dir}
	st.SaveSettings(set)
	var m *Model
	p := ggui.ProbeBuilder(func() ggui.Widget { return Build(m) }, ggui.Sz(900, 700))
	p.Setup(func() {
		m = NewModel(st, dir)
		m.runCLI = f.run
		m.Attach(p)
	})
	t.Cleanup(p.Close)
	p.Frame()
	return m, p
}

// until runs frames until cond holds, for work done in the background.
func until(t *testing.T, p *ggui.Probe, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
		p.Frame()
	}
}

// The docker context is read in the background, set up with the one action
// its state calls for, and switched back to the context in use before.
func TestDockerLinkActions(t *testing.T) {
	t.Setenv("DOCKER_HOST", "tcp://elsewhere:2375")
	f := &fakeDocker{contexts: map[string]string{"default": "unix:///var/run/docker.sock", "desktop-linux": "unix:///x"}, current: "desktop-linux"}
	m, p := probeModel(t, f)
	l := m.DockerLink
	host := m.DockerHost()
	check := func(want ContextStatus) LinkView {
		t.Helper()
		m.CheckLink(l)
		until(t, p, "a check", func() bool { return !l.checking })
		v := ggui.Peek(l.View)
		if v.Status != want {
			t.Fatalf("status %d, want %d: %+v", v.Status, want, v)
		}
		return v
	}
	if v := check(ContextMissing); v.C.Env != "DOCKER_HOST" || v.Back != "default" {
		t.Errorf("override %q, back %q", v.C.Env, v.Back)
	}

	m.SetUpLink(l)
	until(t, p, "the setting up", func() bool { cur, _ := f.state(); return !ggui.Peek(l.State).Busy && !l.checking && cur == "gvx" })
	if _, gvx := f.state(); gvx != host {
		t.Errorf("gvx points at %q, want %q", gvx, host)
	}
	if v := check(ContextInUse); v.Back != "desktop-linux" {
		t.Errorf("would switch back to %q", v.Back)
	}
	if got := ggui.Peek(m.Settings).DockerContextBefore; got != "desktop-linux" {
		t.Errorf("remembered %q", got)
	}

	// The engine's endpoint moves: the context is out of date, and updated.
	m.SetDockerAPI(recipes.DockerAPI{TCP: true, Port: 23750})
	check(ContextStale)
	m.UpdateLink(l)
	until(t, p, "the update", func() bool { _, gvx := f.state(); return gvx == "tcp://127.0.0.1:23750" && !l.checking })
	check(ContextInUse)

	m.SwitchLinkBack(l)
	until(t, p, "switching back", func() bool { cur, _ := f.state(); return cur == "desktop-linux" && !l.checking })
	check(ContextElsewhere)
	m.UseLink(l)
	until(t, p, "using it", func() bool { cur, _ := f.state(); return cur == "gvx" && !l.checking })

	// Setting up a context that exists, unknown to the app, updates it.
	f.mu.Lock()
	f.contexts["gvx"] = "unix:///old"
	f.calls = nil
	f.mu.Unlock()
	l.State.Set(CLIContext{})
	m.SetUpLink(l)
	until(t, p, "the setting up", func() bool { _, gvx := f.state(); return gvx != "unix:///old" && !l.checking })
	f.mu.Lock()
	defer f.mu.Unlock()
	if !slices.ContainsFunc(f.calls, func(c string) bool { return strings.HasPrefix(c, "context update gvx") }) {
		t.Errorf("ran %v", f.calls)
	}
}

func TestMenuState(t *testing.T) {
	engine := Row{Spec: sandbox.Spec{ID: "e", Recipe: recipes.Docker}, State: sandbox.State{Status: sandbox.Running, LogSeq: 1}}
	var items []dockerapi.Container
	for i := range 14 {
		state := "exited"
		if i%2 == 1 {
			state = "running"
		}
		items = append(items, dockerapi.Container{ID: fmt.Sprint(i), Names: []string{fmt.Sprintf("/c%d", i)}, State: state, Status: "Up 1 minute"})
	}
	list := Listing[dockerapi.Container]{Items: items, Loaded: true}
	s := menuState([]Row{engine}, list, Listing[kubeapi.Pod]{}, false, LinkView{}, LinkView{})
	if len(s.Containers) != maxMenuContainers || s.More != 4 || !s.Listed {
		t.Fatalf("%d listed, %d more", len(s.Containers), s.More)
	}
	for i, c := range s.Containers[:7] {
		if c.State != "running" {
			t.Fatalf("container %d is %s: the running ones come first", i, c.State)
		}
	}
	// A line of a log, or a container's uptime, leaves the menus as they are.
	engine.State.LogSeq = 2
	items[1].Status = "Up 2 minutes"
	if !s.Equal(menuState([]Row{engine}, list, Listing[kubeapi.Pod]{}, false, LinkView{}, LinkView{})) {
		t.Error("a log line or an uptime changed the menus")
	}
	items[1].State = "exited"
	if s.Equal(menuState([]Row{engine}, list, Listing[kubeapi.Pod]{}, false, LinkView{}, LinkView{})) {
		t.Error("a container that stopped left the menus as they were")
	}
	// A stopped engine lists none.
	engine.State.Status = sandbox.Stopped
	if s := menuState([]Row{engine}, list, Listing[kubeapi.Pod]{}, false, LinkView{}, LinkView{}); s.Listed || len(s.Containers) != 0 {
		t.Errorf("a stopped engine lists %d", len(s.Containers))
	}
}

// The contexts are asked about at the start and again when a screen that
// tells of them opens; kubectl has nothing to point at before the cluster
// wrote its kubeconfig.
func TestFollowLinks(t *testing.T) {
	f := &fakeDocker{contexts: map[string]string{"default": "unix:///var/run/docker.sock"}, current: "default"}
	m, p := probeModel(t, f)
	kubectl := filepath.Join(t.TempDir(), "kubectl")
	os.WriteFile(kubectl, []byte("#!/bin/sh\nexit 1\n"), 0o755)
	m.KubeLink.path = func(store.Settings) string { return kubectl }
	// Start runs them under the app's owner.
	t.Cleanup(ggui.Root(func() {
		m.followDocker()
		m.followLinks()
	}))
	until(t, p, "the first checks", func() bool {
		return ggui.Peek(m.DockerLink.State).Checked && ggui.Peek(m.KubeLink.State).Checked
	})
	if s := ggui.Peek(m.KubeLink.View).Status; s != ContextPending {
		t.Errorf("kubectl before the cluster is up: %d", s)
	}
	asked := func() int {
		f.mu.Lock()
		defer f.mu.Unlock()
		return len(f.calls)
	}
	before := asked()
	m.Go(PageDocker)
	until(t, p, "a check for the Docker page", func() bool { return asked() > before })
}
