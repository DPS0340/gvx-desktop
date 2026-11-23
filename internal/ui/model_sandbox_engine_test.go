package ui

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ironpark/ggui"

	"github.com/ironpark/gvx-desktop/internal/dockerapi"
	"github.com/ironpark/gvx-desktop/internal/engine"
	"github.com/ironpark/gvx-desktop/internal/recipes"
	"github.com/ironpark/gvx-desktop/internal/rungv"
	"github.com/ironpark/gvx-desktop/internal/sandbox"
	"github.com/ironpark/gvx-desktop/internal/store"
)

type fakeSandboxEngine struct {
	mu           sync.Mutex
	opens        int
	closes       int
	stopRuns     int
	lastTimeout  time.Duration
	running      int
	ownedVols    map[string]bool
	volUsers     map[string][]string
	openHook     func(call int) error
	handler      http.Handler
	builderSock  string
	builderState engine.BuilderState
	amd64Sock    string
	amd64State   engine.Amd64State
	amd64Configs []*engine.Amd64Config
	setAmd64Err  error
	resetDisks   int
	resetDiskErr error
	features     rungv.Features
	canControl   map[string]bool
}

func (f *fakeSandboxEngine) Open(context.Context) error {
	f.mu.Lock()
	f.opens++
	call := f.opens
	hook := f.openHook
	f.mu.Unlock()
	if hook != nil {
		return hook(call)
	}
	return nil
}

func (f *fakeSandboxEngine) Close(timeout time.Duration) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.closes++
	f.running = 0
	f.lastTimeout = timeout
	return nil
}

func (f *fakeSandboxEngine) StopRunning(timeout time.Duration) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.stopRuns++
	f.running = 0
	f.lastTimeout = timeout
}

func (f *fakeSandboxEngine) RunningCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.running
}

func (f *fakeSandboxEngine) VolumeOwnership() (map[string]bool, map[string][]string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	owned := map[string]bool{}
	for k, v := range f.ownedVols {
		owned[k] = v
	}
	users := map[string][]string{}
	for k, v := range f.volUsers {
		users[k] = slices.Clone(v)
	}
	return owned, users
}

func (f *fakeSandboxEngine) Handler() http.Handler {
	if f.handler != nil {
		return f.handler
	}
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
}

func (f *fakeSandboxEngine) BuilderSocket() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.builderSock
}

func (f *fakeSandboxEngine) BuilderState() engine.BuilderState {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.builderState.Status == "" {
		return engine.BuilderState{Status: engine.BuilderIdle}
	}
	return f.builderState
}

func (f *fakeSandboxEngine) SetAmd64(cfg *engine.Amd64Config) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.amd64Configs = append(f.amd64Configs, cfg)
	if f.setAmd64Err != nil {
		return f.setAmd64Err
	}
	f.amd64State.Enabled = cfg != nil
	return nil
}

func (f *fakeSandboxEngine) Amd64Socket() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.amd64Sock
}

func (f *fakeSandboxEngine) Amd64State() engine.Amd64State {
	f.mu.Lock()
	defer f.mu.Unlock()
	st := f.amd64State
	if st.Status == "" {
		st.Status = engine.BuilderIdle
	}
	return st
}

func (f *fakeSandboxEngine) ResetAmd64Disk(context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.resetDisks++
	if f.resetDiskErr != nil {
		return f.resetDiskErr
	}
	f.amd64State.DiskBytes = 0
	return nil
}

func (f *fakeSandboxEngine) Features() rungv.Features {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.features
}

func (f *fakeSandboxEngine) CanControl(ref string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.canControl[ref]
}

func (f *fakeSandboxEngine) counts() (opens, closes, stopRuns int, timeout time.Duration) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.opens, f.closes, f.stopRuns, f.lastTimeout
}

func TestSandboxEngineSwitchLifecycle(t *testing.T) {
	dir := t.TempDir()
	st := store.Store{Dir: dir}
	set := store.DefaultSettings()
	set.StopTimeout = 7
	_ = st.SaveSettings(set)

	var m *Model
	p := ggui.ProbeBuilder(func() ggui.Widget { return ggui.Provide(ggui.ReducedMotionKey, true, Build(m)) }, ggui.Sz(1000, 700))
	p.Setup(func() {
		m = NewModel(st, dir)
		m.Attach(p)
	})
	defer p.Close()
	p.Frame()

	blockOpen := make(chan struct{})
	fake := &fakeSandboxEngine{
		openHook: func(int) error {
			<-blockOpen
			return nil
		},
	}
	m.sandboxEngine = fake

	if m.dockerProxy.Local() != nil {
		t.Fatal("proxy Local() should start nil when SandboxEngine is off")
	}
	if got := m.dockerBackendHost(); got != recipes.DockerBackendHost() {
		t.Fatalf("off: dockerBackendHost() = %q, want %q", got, recipes.DockerBackendHost())
	}

	// Turning the switch on sets Local immediately (before Open finishes) so
	// requests never hit the dind backend while Open is in flight.
	m.SetSandboxEngine(true)
	if m.dockerProxy.Local() == nil {
		t.Fatal("proxy Local() must be set immediately on turning SandboxEngine on, before Open finishes")
	}
	fake.mu.Lock()
	fake.running = 1
	fake.mu.Unlock()
	close(blockOpen)
	// Wait until openSandboxEngine's UI callback has completed (reflected by
	// EngineRunning updating to 1).
	deadline := time.Now().Add(3 * time.Second)
	for ggui.Peek(m.EngineRunning) != 1 && time.Now().Before(deadline) {
		p.Frame()
		time.Sleep(10 * time.Millisecond)
	}
	opens, closes, _, _ := fake.counts()
	if opens != 1 || closes != 0 {
		t.Fatalf("after turning on: opens=%d closes=%d", opens, closes)
	}
	if got, want := m.dockerBackendHost(), m.dockerHost(); got != want {
		t.Fatalf("on: dockerBackendHost() = %q, want %q", got, want)
	}
	if saved, _ := st.Settings(); !saved.SandboxEngine {
		t.Fatal("SandboxEngine was not persisted to settings")
	}

	// Changing the Docker API endpoint updates dockerBackendHost in sandbox-engine mode.
	m.SetDockerAPI(recipes.DockerAPI{TCP: true, Port: 23759})
	if got := m.dockerBackendHost(); got != "tcp://127.0.0.1:23759" {
		t.Fatalf("after SetDockerAPI: dockerBackendHost() = %q", got)
	}

	// Turning the switch off clears Local immediately and closes the engine asynchronously.
	m.SetSandboxEngine(false)
	if m.dockerProxy.Local() != nil {
		t.Fatal("proxy Local() was not cleared immediately on turning SandboxEngine off")
	}
	deadline = time.Now().Add(3 * time.Second)
	for {
		_, closes, _, _ = fake.counts()
		if closes >= 1 || time.Now().After(deadline) {
			break
		}
		p.Frame()
		time.Sleep(10 * time.Millisecond)
	}
	opens, closes, _, timeout := fake.counts()
	if opens != 1 || closes != 1 || timeout != 7*time.Second {
		t.Fatalf("after turning off: opens=%d closes=%d timeout=%v", opens, closes, timeout)
	}
	if got := m.dockerBackendHost(); got != recipes.DockerBackendHost() {
		t.Fatalf("back off: dockerBackendHost() = %q, want %q", got, recipes.DockerBackendHost())
	}
}

// Regression test for review item 1a & 1b: a slow stale Open (gen 1) that
// completes after the user toggled off (gen 2) and back on (gen 3) must NOT
// close the reopened engine, while a stale Open completing while the setting
// is still off DOES close it. Also startDockerProxy sets Local immediately
// when SandboxEngine is on at launch.
func TestSandboxEngineStaleOpenAndStartProxyLocal(t *testing.T) {
	dir := t.TempDir()
	st := store.Store{Dir: dir}
	set := store.DefaultSettings()
	set.SandboxEngine = true
	set.DockerEndpoint = DockerTCPEndpoint
	set.DockerPort = 15891
	_ = st.SaveSettings(set)

	var m *Model
	p := ggui.ProbeBuilder(func() ggui.Widget { return ggui.Provide(ggui.ReducedMotionKey, true, Build(m)) }, ggui.Sz(1000, 700))
	p.Setup(func() {
		m = NewModel(st, dir)
		m.Attach(p)
	})
	defer p.Close()
	p.Frame()

	gen1Started := make(chan struct{})
	releaseGen1 := make(chan struct{})
	gen3Done := make(chan struct{})
	gen4Started := make(chan struct{})
	releaseGen4 := make(chan struct{})
	fake := &fakeSandboxEngine{
		openHook: func(call int) error {
			switch call {
			case 1:
				close(gen1Started)
				<-releaseGen1
			case 2:
				close(gen3Done)
			case 3:
				close(gen4Started)
				<-releaseGen4
			}
			return nil
		},
	}
	m.sandboxEngine = fake

	// startDockerProxy sets Local immediately when SandboxEngine is already on.
	m.startDockerProxy()
	if m.dockerProxy.Local() == nil {
		t.Fatal("startDockerProxy did not set Local immediately when SandboxEngine is on")
	}

	// Start gen 1 open (blocked).
	m.openSandboxEngine()
	<-gen1Started

	// Toggle off (gen 2) -> close runs once.
	m.SetSandboxEngine(false)
	deadline := time.Now().Add(3 * time.Second)
	for {
		_, closes, _, _ := fake.counts()
		if closes >= 1 || time.Now().After(deadline) {
			break
		}
		p.Frame()
		time.Sleep(10 * time.Millisecond)
	}

	// Toggle back on (gen 3) -> second Open finishes immediately.
	m.SetSandboxEngine(true)
	<-gen3Done
	for range 5 {
		p.Frame()
		time.Sleep(10 * time.Millisecond)
	}

	// Now release stale gen 1: because SandboxEngine is ON under gen 3, gen 1
	// must NOT call Close again.
	close(releaseGen1)
	for range 10 {
		p.Frame()
		time.Sleep(10 * time.Millisecond)
	}
	opens, closes, _, _ := fake.counts()
	if opens != 2 || closes != 1 {
		t.Fatalf("stale gen 1 closed reopened engine: opens=%d closes=%d (want 2, 1)", opens, closes)
	}
	if m.dockerProxy.Local() == nil {
		t.Fatal("proxy Local() cleared after stale gen 1")
	}

	// Contrast: a slow Open (call 3) that completes AFTER the setting stays OFF
	// MUST trigger a cleanup Close so the engine is not left open.
	m.openSandboxEngine()
	<-gen4Started
	m.SetSandboxEngine(false)
	deadline = time.Now().Add(3 * time.Second)
	for {
		_, c, _, _ := fake.counts()
		if c >= 2 || time.Now().After(deadline) {
			break
		}
		p.Frame()
		time.Sleep(10 * time.Millisecond)
	}
	close(releaseGen4)
	deadline = time.Now().Add(3 * time.Second)
	for {
		_, c, _, _ := fake.counts()
		if c >= 3 || time.Now().After(deadline) {
			break
		}
		p.Frame()
		time.Sleep(10 * time.Millisecond)
	}
	if _, c, _, _ := fake.counts(); c < 3 {
		t.Fatalf("stale Open completing while setting is OFF did not close engine: closes=%d", c)
	}
}

// Regression test for review item 1c: if rungv is missing at launch and later
// becomes ready via DetectEngine (e.g. after SignRungv or updating RungvPath),
// the sandbox engine opens automatically when SandboxEngine is enabled.
func TestSandboxEngineOpensWhenRungvBecomesReady(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell script fake rungv")
	}
	// Isolate HOME, PATH, and GVX_RUNGV so Locate does not find the host's real rungv.
	t.Setenv("HOME", t.TempDir())
	t.Setenv("PATH", t.TempDir())
	t.Setenv("GVX_RUNGV", "")

	dir := t.TempDir()
	st := store.Store{Dir: dir}
	set := store.DefaultSettings()
	set.SandboxEngine = true
	set.RungvPath = filepath.Join(dir, "missing-rungv")
	_ = st.SaveSettings(set)

	var m *Model
	p := ggui.ProbeBuilder(func() ggui.Widget { return ggui.Provide(ggui.ReducedMotionKey, true, Build(m)) }, ggui.Sz(1000, 700))
	p.Setup(func() {
		m = NewModel(st, dir)
		m.Attach(p)
	})
	defer p.Close()
	p.Frame()

	fake := &fakeSandboxEngine{}
	m.sandboxEngine = fake

	// Launch detection fails because rungv is missing: engine must not open.
	done := make(chan bool, 1)
	m.DetectEngine(func(ok bool) { done <- ok })
	deadline := time.Now().Add(3 * time.Second)
	var firstOK bool
	select {
	case firstOK = <-done:
	default:
		for time.Now().Before(deadline) {
			p.Frame()
			select {
			case firstOK = <-done:
				goto gotFirst
			default:
				time.Sleep(10 * time.Millisecond)
			}
		}
		t.Fatal("timed out waiting for first DetectEngine")
	}
gotFirst:
	if firstOK {
		t.Fatal("expected first DetectEngine to report not ready")
	}
	if opens, _, _, _ := fake.counts(); opens != 0 {
		t.Fatalf("engine opened without rungv: opens=%d", opens)
	}

	// Now provide a working fake rungv via settings change: DetectEngine runs
	// and opens the sandbox engine automatically.
	rungvScript := filepath.Join(dir, "fake-rungv")
	script := `#!/bin/sh
case "$1" in
  version) echo "rungv v0.18.0 (abc1234, go1.24.0)" ;;
  info) echo '{"OS":"darwin","Architecture":"arm64","NumCPU":4,"DefaultBackend":"hvf","AvailableBackends":["hvf"]}' ;;
  *) echo '[]' ;;
esac
`
	if err := os.WriteFile(rungvScript, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	m.UpdateSettings(func(s *store.Settings) { s.RungvPath = rungvScript })
	deadline = time.Now().Add(5 * time.Second)
	for {
		opens, _, _, _ := fake.counts()
		if opens >= 1 && m.dockerProxy.Local() != nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("sandbox engine did not open after rungv became ready: opens=%d local=%v eng=%+v",
				opens, m.dockerProxy.Local(), ggui.Peek(m.Engine))
		}
		p.Frame()
		time.Sleep(15 * time.Millisecond)
	}
}

func TestSandboxEngineOfflineNeverOpens(t *testing.T) {
	dir := t.TempDir()
	st := store.Store{Dir: dir}
	var m *Model
	p := ggui.ProbeBuilder(func() ggui.Widget { return ggui.Provide(ggui.ReducedMotionKey, true, Build(m)) }, ggui.Sz(1000, 700))
	p.Setup(func() {
		m = NewModel(st, dir)
		m.Offline = true
		m.Attach(p)
	})
	defer p.Close()
	p.Frame()

	fake := &fakeSandboxEngine{}
	m.sandboxEngine = fake

	m.SetSandboxEngine(true)
	p.Frame()
	time.Sleep(30 * time.Millisecond)
	opens, closes, _, _ := fake.counts()
	if opens != 0 || closes != 0 || m.dockerProxy.Local() != nil {
		t.Fatalf("offline mode should never open engine: opens=%d closes=%d local=%v", opens, closes, m.dockerProxy.Local())
	}
}

// Regression test for review item 2A: in sandbox-engine mode (shared rungv
// image cache), user sandboxes count as users of their images and
// AskRemoveDockerImage refuses rather than force-deleting them.
func TestSandboxEngineDockerImageUsersAndRemoveRefusal(t *testing.T) {
	dir := t.TempDir()
	st := store.Store{Dir: dir}
	set := store.DefaultSettings()
	set.Language = LangEnglish
	set.SandboxEngine = true
	_ = st.SaveSettings(set)
	sb := sandbox.Spec{ID: "sb1", Name: "my-pg", Image: "postgres:17-alpine"}
	_ = st.SaveSandboxes([]sandbox.Spec{sb})

	var m *Model
	p := ggui.ProbeBuilder(func() ggui.Widget { return ggui.Provide(ggui.ReducedMotionKey, true, Build(m)) }, ggui.Sz(1200, 800))
	p.Setup(func() {
		m = NewModel(st, dir)
		m.Offline = true
		m.Attach(p)
	})
	defer p.Close()
	p.Frame()

	pgRow := DockerImageRow{ID: "sha256:aaaa111122223333", ShortID: "aaaa11112222", Repository: "postgres", Tag: "17-alpine", Size: 100 << 20}
	unusedRow := DockerImageRow{ID: "sha256:bbbb111122223333", ShortID: "bbbb11112222", Repository: "alpine", Tag: "3.20", Size: 9 << 20}

	// Off mode ignores user sandboxes; sandbox-engine mode includes them.
	if got := dockerImageAllUsers(nil, ggui.Peek(m.Rows), false, pgRow); len(got) != 0 {
		t.Errorf("off mode counted sandbox user: %v", got)
	}
	if got := dockerImageAllUsers(nil, ggui.Peek(m.Rows), true, pgRow); !slices.Equal(got, []string{"my-pg"}) {
		t.Errorf("sandbox-engine mode image users = %v, want [my-pg]", got)
	}

	// AskRemoveDockerImage refuses (does not open Confirm dialog) for a sandbox's image.
	m.AskRemoveDockerImage(pgRow)
	p.Frame()
	if ggui.Peek(m.ConfirmOpen) {
		t.Fatal("AskRemoveDockerImage opened confirm dialog for image used by a user sandbox")
	}

	// Unused image still opens the Confirm dialog normally.
	m.AskRemoveDockerImage(unusedRow)
	p.Frame()
	if !ggui.Peek(m.ConfirmOpen) {
		t.Fatal("AskRemoveDockerImage did not open confirm dialog for unused image")
	}

	// Images → Docker Engine tab shows "In use" badge for the sandbox's image.
	m.ConfirmOpen.Set(false)
	m.DockerImages.Set(Listing[dockerapi.Image]{Loaded: true, Items: []dockerapi.Image{
		{ID: pgRow.ID, RepoTags: []string{"postgres:17-alpine"}, Size: pgRow.Size, Created: time.Now().Unix()},
	}})
	m.GoImages(ImagesDocker)
	p.Frame()
	found := false
	for _, n := range p.Semantics().All() {
		if n.Name == "In use" {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expected In use badge for sandbox image on Docker images tab:\n%s", p.Semantics())
	}
}

// Regression test for review item 2B: VolumeUsers counts engine containers'
// volumes, and pruneVolumes never deletes engine-owned or container-used
// volumes (even when SandboxEngine is off) or sandbox volumes, removing only
// truly unused volumes individually.
func TestVolumeUsersAndPruneProtectEngineVolumes(t *testing.T) {
	dir := t.TempDir()
	// Persist real engine store files on disk while SandboxEngine is OFF.
	engDir := filepath.Join(dir, "engine")
	if err := os.MkdirAll(engDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(engDir, "volumes.json"), []byte(`{
  "version": 1,
  "volumes": [
    {"name": "eng-owned-vol", "driver": "ext4lite"}
  ]
}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(engDir, "containers.json"), []byte(`{
  "version": 1,
  "containers": [
    {
      "id": "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
      "name": "web-ctr",
      "mounts": [{"Type": "volume", "Name": "ctr-mounted-vol", "Destination": "/data"}],
      "anonymousVolumes": ["anon-vol-1"]
    }
  ]
}`), 0o600); err != nil {
		t.Fatal(err)
	}

	st := store.Store{Dir: dir}
	sb := sandbox.Spec{
		ID:     "sb1",
		Name:   "pg-sandbox",
		Image:  "postgres:17-alpine",
		Mounts: []sandbox.Mount{{Type: sandbox.MountVolume, Source: "sb-vol", Target: "/var/lib/postgresql/data"}},
	}
	_ = st.SaveSandboxes([]sandbox.Spec{sb})

	var m *Model
	p := ggui.ProbeBuilder(func() ggui.Widget { return ggui.Provide(ggui.ReducedMotionKey, true, Build(m)) }, ggui.Sz(1000, 700))
	p.Setup(func() {
		m = NewModel(st, dir)
		m.Offline = true
		m.Attach(p)
	})
	defer p.Close()
	p.Frame()

	// SandboxEngine is OFF, yet the real *engine.Engine reads volumes.json and
	// containers.json from disk and protects all engine volumes.
	owned, engUsers := m.sandboxEngineVolumeOwnership()
	if !owned["eng-owned-vol"] {
		t.Fatalf("expected eng-owned-vol in owned map, got %v", owned)
	}
	if got := volumeUsersWithEngine(ggui.Peek(m.Rows), engUsers, "ctr-mounted-vol"); !slices.Equal(got, []string{"web-ctr"}) {
		t.Errorf("volumeUsersWithEngine(ctr-mounted-vol) = %v, want [web-ctr]", got)
	}
	if got := volumeUsersWithEngine(ggui.Peek(m.Rows), engUsers, "sb-vol"); !slices.Equal(got, []string{"pg-sandbox"}) {
		t.Errorf("volumeUsersWithEngine(sb-vol) = %v, want [pg-sandbox]", got)
	}

	vols := []rungv.Volume{
		{Name: "eng-owned-vol"},
		{Name: "ctr-mounted-vol"},
		{Name: "anon-vol-1"},
		{Name: engine.BuilderVolumeName},
		{Name: "sb-vol"},
		{Name: "locked-vol", InUse: true},
		{Name: "orphan-a"},
		{Name: "orphan-b"},
	}
	var removed []string
	deleted, err := m.pruneVolumes(
		context.Background(),
		func(context.Context) ([]rungv.Volume, error) { return vols, nil },
		func(_ context.Context, name string) error { removed = append(removed, name); return nil },
		ggui.Peek(m.Rows),
	)
	if err != nil {
		t.Fatalf("pruneVolumes error: %v", err)
	}
	if deleted != 2 || !slices.Equal(removed, []string{"orphan-a", "orphan-b"}) {
		t.Fatalf("pruneVolumes deleted=%d removed=%v, want 2 [orphan-a orphan-b]", deleted, removed)
	}
}

// Regression test for review item 3: Quit confirmation, ActiveCount,
// TotalRunning, and StopAll include running sandbox-engine containers.
func TestQuitAndStopAllIncludeEngineContainers(t *testing.T) {
	dir := t.TempDir()
	st := store.Store{Dir: dir}
	set := store.DefaultSettings()
	set.Language = LangEnglish
	set.SandboxEngine = true
	set.StopTimeout = 5
	_ = st.SaveSettings(set)

	var m *Model
	p := ggui.ProbeBuilder(func() ggui.Widget { return ggui.Provide(ggui.ReducedMotionKey, true, Build(m)) }, ggui.Sz(1000, 700))
	p.Setup(func() {
		m = NewModel(st, dir)
		m.Attach(p)
	})
	defer p.Close()
	p.Frame()

	fake := &fakeSandboxEngine{running: 2}
	m.sandboxEngine = fake
	m.dockerProxy.SetLocal(fake.Handler())

	// 0 user sandboxes running, 2 engine containers running.
	if got := m.ActiveCount(); got != 2 {
		t.Fatalf("ActiveCount() = %d, want 2", got)
	}
	if m.PrepareQuit() {
		t.Fatal("PrepareQuit() returned true with 2 running engine containers")
	}
	p.Frame()
	if !ggui.Peek(m.QuitOpen) {
		t.Fatal("QuitOpen was not set")
	}
	if got := ggui.Peek(m.TotalRunning); got != 2 {
		t.Fatalf("TotalRunning = %d, want 2", got)
	}

	// StopAll calls StopRunning (leaving the engine open and proxy Local intact).
	m.StopAll()
	deadline := time.Now().Add(3 * time.Second)
	for {
		_, _, stopRuns, _ := fake.counts()
		if stopRuns == 1 && ggui.Peek(m.EngineRunning) == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("StopAll did not stop engine containers: stopRuns=%d running=%d", stopRuns, ggui.Peek(m.EngineRunning))
		}
		p.Frame()
		time.Sleep(10 * time.Millisecond)
	}
	_, closes, stopRuns, timeout := fake.counts()
	if closes != 0 || stopRuns != 1 || timeout != 5*time.Second {
		t.Fatalf("StopAll: closes=%d stopRuns=%d timeout=%v (want 0, 1, 5s)", closes, stopRuns, timeout)
	}
	if m.dockerProxy.Local() == nil {
		t.Fatal("StopAll cleared proxy Local() — engine must stay open")
	}

	// With 0 running sandboxes and 0 running containers, PrepareQuit allows closing.
	if !m.PrepareQuit() {
		t.Fatal("PrepareQuit() returned false when nothing is running")
	}

	// StopAllAndThen (quit) closes the engine.
	fake.mu.Lock()
	fake.running = 1
	fake.mu.Unlock()
	done := make(chan struct{})
	m.StopAllAndThen(func() { close(done) })
	deadline = time.Now().Add(3 * time.Second)
	for {
		select {
		case <-done:
			goto quitDone
		default:
		}
		if time.Now().After(deadline) {
			t.Fatal("StopAllAndThen did not invoke callback")
		}
		p.Frame()
		time.Sleep(10 * time.Millisecond)
	}
quitDone:
	_, closes, _, _ = fake.counts()
	if closes != 1 {
		t.Fatalf("StopAllAndThen closes = %d, want 1", closes)
	}
}

func TestSandboxEngineUIAndGating(t *testing.T) {
	dir := t.TempDir()
	st := store.Store{Dir: dir}
	set := store.DefaultSettings()
	set.Language = LangEnglish
	_ = st.SaveSettings(set)
	docker := recipes.WithSharedFolders(recipes.DockerEngine(), []string{"/Users/me"})
	_ = st.SaveSandboxes([]sandbox.Spec{docker})

	var m *Model
	p := ggui.ProbeBuilder(func() ggui.Widget { return ggui.Provide(ggui.ReducedMotionKey, true, Build(m)) }, ggui.Sz(1200, 2000))
	p.Setup(func() {
		m = NewModel(st, dir)
		m.Offline = true
		m.Attach(p)
	})
	defer p.Close()
	p.Frame()

	shows := func(substr string) bool {
		for _, n := range p.Semantics().All() {
			if strings.Contains(n.Name, substr) {
				return true
			}
		}
		return false
	}

	// Off: Docker Engine sandbox is stopped so Containers is closed, and
	// Settings shows Shared folders controls and Docker Engine API copy.
	if ggui.Peek(m.ContainersAccess) != ContainersStopped {
		t.Fatalf("off: ContainersAccess = %v, want ContainersStopped", ggui.Peek(m.ContainersAccess))
	}
	m.Go(PageSettings)
	p.Frame()
	if !shows("Add folder") {
		t.Errorf("off: Settings missing Shared folders Add folder button")
	}
	if !shows("How the Docker Engine is reached") {
		t.Errorf("off: Settings missing Docker Engine API caption")
	}

	// Turn experimental sandbox engine on: Containers opens immediately even with
	// no running Docker Engine sandbox.
	m.SetSandboxEngine(true)
	p.Frame()
	if ggui.Peek(m.ContainersAccess) != ContainersOpen {
		t.Fatalf("on: ContainersAccess = %v, want ContainersOpen", ggui.Peek(m.ContainersAccess))
	}

	// Review item 4: Settings hides Shared folders picker with a short note,
	// updates Engine API wording, and shows the running-engine note when active.
	m.States.Set(map[string]sandbox.State{docker.ID: {Status: sandbox.Running}})
	p.Frame()
	if shows("Add folder") {
		t.Errorf("on: Settings should hide Shared folders picker in sandbox-engine mode")
	}
	if !shows("Applies only to the Docker Engine sandbox") {
		t.Errorf("on: Settings missing Shared folders disabled note:\n%s", p.Semantics())
	}
	if !shows("How docker reaches GVX Desktop's sandbox engine") {
		t.Errorf("on: Settings missing sandbox-engine Engine API caption:\n%s", p.Semantics())
	}
	// Labs has the switch, and the note on the engine still running.
	m.Go(PageLabs)
	p.Frame()
	if !shows("Run containers as sandboxes, without a Docker Engine") {
		t.Errorf("Labs page missing experimental switch:\n%s", p.Semantics())
	}
	if !shows("Your Docker Engine sandbox is still running") {
		t.Errorf("Labs page missing running Docker Engine note:\n%s", p.Semantics())
	}

	// Containers page opens, shows containers, and Terminal tab shows the
	// unsupported message without starting a shell.
	m.DockerContainers.Set(Listing[dockerapi.Container]{Loaded: true, Items: []dockerapi.Container{
		{
			ID: "3f2a9c1b7d4e5f60", Names: []string{"/web"}, Image: "nginx:alpine", State: "running",
			Mounts: []dockerapi.Mount{{Type: "bind", Source: "/Volumes/work/site", Destination: "/usr/share/nginx/html", RW: true}},
		},
	}})
	m.Go(PageContainers)
	m.ContainerDetail.Set(containerTerminalTab)
	p.Frame()
	if ggui.Peek(m.Page) != PageContainers {
		t.Fatalf("Go(PageContainers) did not open page in sandbox-engine mode")
	}
	if !shows("Needs rungv v0.20 or later") {
		t.Errorf("Terminal tab missing v0.20 requirement message:\n%s", p.Semantics())
	}
	m.terms.mu.Lock()
	liveTerms := len(m.terms.set)
	m.terms.mu.Unlock()
	if liveTerms != 0 {
		t.Errorf("Terminal tab started %d shell(s) in sandbox-engine mode", liveTerms)
	}

	// Docker Engine page shows the experimental banner.
	m.Go(PageDocker)
	p.Frame()
	if !shows("Experimental sandbox engine is on") {
		t.Errorf("Docker page missing experimental banner:\n%s", p.Semantics())
	}

	// Docker image explorer in sandbox-engine mode shows the unsupported message on Files tab.
	m.ExploreDockerImage(DockerImageRow{ID: "sha256:1234", Repository: "nginx", Tag: "alpine"}, ExploreFiles)
	p.Frame()
	if !shows("Not supported in the experimental sandbox engine") {
		t.Errorf("Explorer Files tab missing unsupported message:\n%s", p.Semantics())
	}

	// Labs shows builder state (idle / running) and gvx-desktop builder status line.
	m.ExploreOpen.Set(false)
	m.Go(PageLabs)
	m.BuilderState.Set(engine.BuilderState{Status: engine.BuilderIdle})
	m.BuildxStatus.Set(BuildxStatus{Checked: true, Ready: true, Selected: true})
	p.Frame()
	if !shows("Image builder: idle") {
		t.Errorf("Settings missing idle builder state:\n%s", p.Semantics())
	}
	if !shows("routes docker build through buildx (docker buildx install)") {
		t.Errorf("Settings missing buildx alias disclosure note:\n%s", p.Semantics())
	}
	if !shows("docker build uses the gvx-desktop builder for the gvx context.") {
		t.Errorf("Settings missing selected buildx status:\n%s", p.Semantics())
	}
	m.BuilderState.Set(engine.BuilderState{Status: engine.BuilderRunning})
	m.BuildxStatus.Set(BuildxStatus{Checked: true, NoBuildx: true})
	p.Frame()
	if !shows("Image builder: running") {
		t.Errorf("Settings missing running builder state:\n%s", p.Semantics())
	}
	if !shows("docker buildx plugin was not found") {
		t.Errorf("Settings missing missing-buildx note:\n%s", p.Semantics())
	}
}

func TestBuildxConfigureAndRestore(t *testing.T) {
	// Ensure no accidental fallback to user's ~/.docker.
	t.Setenv("DOCKER_CONFIG", t.TempDir())
	t.Setenv("BUILDX_CONFIG", t.TempDir())

	sock := "/tmp/gvxd-test/buildkit.sock"
	wantEP := "unix://" + sock
	dockerHost := "unix:///tmp/gvxd-test/docker.sock"

	t.Run("fresh_create_and_select_then_restore", func(t *testing.T) {
		dockerCfgDir := t.TempDir()
		t.Setenv("DOCKER_CONFIG", dockerCfgDir)
		t.Setenv("BUILDX_CONFIG", t.TempDir())
		cfgPath := filepath.Join(dockerCfgDir, "config.json")

		var calls [][]string
		var hasBuilder bool
		curBuilder := "gvx"
		hostBuilder := "default"
		run := func(_ context.Context, _ string, args ...string) ([]byte, error) {
			calls = append(calls, slices.Clone(args))
			key := strings.Join(args, " ")
			switch {
			case key == "buildx version":
				return []byte("github.com/docker/buildx v0.39.1"), nil
			case key == "buildx install":
				if err := os.WriteFile(cfgPath, []byte(`{"aliases":{"builder":"buildx"}}`), 0o600); err != nil {
					return nil, err
				}
				return nil, nil
			case key == "buildx uninstall":
				if err := os.WriteFile(cfgPath, []byte(`{}`), 0o600); err != nil {
					return nil, err
				}
				return nil, nil
			case key == "buildx inspect gvx-desktop":
				if !hasBuilder {
					return nil, os.ErrNotExist
				}
				return []byte("Name: gvx-desktop\nDriver: remote\nEndpoint: " + wantEP + "\nDriver Options: default-load=\"true\"\n"), nil
			case strings.HasPrefix(key, "buildx create --name gvx-desktop "):
				hasBuilder = true
				return []byte("gvx-desktop\n"), nil
			case key == "-H "+dockerHost+" buildx inspect":
				return []byte("Name: " + hostBuilder + "\n"), nil
			case strings.HasPrefix(key, "-H "+dockerHost+" buildx use --default "):
				hostBuilder = args[len(args)-1]
				return nil, nil
			case key == "context inspect gvx":
				return []byte("[{}]"), nil
			case key == "--context gvx buildx inspect":
				return []byte("Name: " + curBuilder + "\nDriver: docker\n"), nil
			case strings.HasPrefix(key, "--context gvx buildx use --default "):
				curBuilder = args[len(args)-1]
				return nil, nil
			}
			return nil, os.ErrInvalid
		}

		st, prev, save, installedAlias := configureBuildx(context.Background(), "/usr/local/bin/docker", run, sock, dockerHost, "")
		if !st.Ready || !st.Selected || st.Err != "" || !save || prev != "gvx" || hostBuilder != "gvx-desktop" || !installedAlias {
			t.Fatalf("configureBuildx = (%+v, %q, %v, host=%q, installedAlias=%v)", st, prev, save, hostBuilder, installedAlias)
		}
		if got := dockerBuilderAlias(cfgPath); got != "buildx" {
			t.Fatalf("after install: aliases.builder = %q, want buildx", got)
		}

		// Idempotent second call should neither rm nor create nor re-use gvx context nor re-run buildx install.
		calls = nil
		st2, _, save2, installedAlias2 := configureBuildx(context.Background(), "/usr/local/bin/docker", run, sock, dockerHost, prev)
		if !st2.Ready || !st2.Selected || save2 || installedAlias2 {
			t.Fatalf("second configureBuildx = (%+v, save=%v, installedAlias=%v)", st2, save2, installedAlias2)
		}
		for _, c := range calls {
			if len(c) >= 2 && (c[1] == "create" || c[1] == "rm" || c[1] == "install") {
				t.Errorf("second call was not idempotent, ran: %v", c)
			}
			if len(c) >= 4 && c[0] == "--context" && c[3] == "use" {
				t.Errorf("second call needlessly re-selected gvx context builder: %v", c)
			}
		}

		// Restore switches back to recorded previous builder, clears per-endpoint default,
		// and uninstalls the buildx alias because this app installed it.
		calls = nil
		clearPrev, clearAlias := restoreBuildx(context.Background(), "/usr/local/bin/docker", run, dockerHost, "my-prev", installedAlias)
		if !clearPrev || !clearAlias {
			t.Fatalf("restoreBuildx = (clearPrev=%v, clearAlias=%v), want (true, true)", clearPrev, clearAlias)
		}
		if curBuilder != "my-prev" || hostBuilder != "default" {
			t.Fatalf("restored builders = (ctx=%q, host=%q), want (my-prev, default)", curBuilder, hostBuilder)
		}
		if got := dockerBuilderAlias(cfgPath); got != "" {
			t.Fatalf("after restore: aliases.builder = %q, want empty", got)
		}
		var sawUninstall bool
		for _, c := range calls {
			if slices.Contains(c, "rm") {
				t.Errorf("restoreBuildx must never delete builders: %v", c)
			}
			if strings.Join(c, " ") == "buildx uninstall" {
				sawUninstall = true
			}
		}
		if !sawUninstall {
			t.Errorf("restoreBuildx did not run buildx uninstall; calls=%v", calls)
		}
	})

	t.Run("alias_already_present_left_alone", func(t *testing.T) {
		dockerCfgDir := t.TempDir()
		t.Setenv("DOCKER_CONFIG", dockerCfgDir)
		t.Setenv("BUILDX_CONFIG", t.TempDir())
		cfgPath := filepath.Join(dockerCfgDir, "config.json")
		orig := `{"aliases":{"builder":"buildx"}}`
		if err := os.WriteFile(cfgPath, []byte(orig), 0o600); err != nil {
			t.Fatal(err)
		}

		var calls [][]string
		curBuilder := "gvx"
		hostBuilder := "default"
		run := func(_ context.Context, _ string, args ...string) ([]byte, error) {
			calls = append(calls, slices.Clone(args))
			key := strings.Join(args, " ")
			switch {
			case key == "buildx version":
				return []byte("github.com/docker/buildx v0.39.1"), nil
			case key == "buildx inspect gvx-desktop":
				return []byte("Name: gvx-desktop\nDriver: remote\nEndpoint: " + wantEP + "\nDriver Options: default-load=\"true\"\n"), nil
			case key == "-H "+dockerHost+" buildx inspect":
				return []byte("Name: " + hostBuilder + "\n"), nil
			case strings.HasPrefix(key, "-H "+dockerHost+" buildx use --default "):
				hostBuilder = args[len(args)-1]
				return nil, nil
			case key == "context inspect gvx":
				return []byte("[{}]"), nil
			case key == "--context gvx buildx inspect":
				return []byte("Name: " + curBuilder + "\nDriver: docker\n"), nil
			case strings.HasPrefix(key, "--context gvx buildx use --default "):
				curBuilder = args[len(args)-1]
				return nil, nil
			}
			return nil, os.ErrInvalid
		}

		st, prev, save, installedAlias := configureBuildx(context.Background(), "/usr/local/bin/docker", run, sock, dockerHost, "")
		if !st.Ready || !st.Selected || !save || prev != "gvx" || installedAlias {
			t.Fatalf("configureBuildx = (%+v, prev=%q, save=%v, installedAlias=%v)", st, prev, save, installedAlias)
		}
		for _, c := range calls {
			if strings.Join(c, " ") == "buildx install" {
				t.Errorf("configureBuildx must not run buildx install when alias already present")
			}
		}

		calls = nil
		clearPrev, clearAlias := restoreBuildx(context.Background(), "/usr/local/bin/docker", run, dockerHost, prev, installedAlias)
		if !clearPrev || clearAlias {
			t.Fatalf("restoreBuildx = (clearPrev=%v, clearAlias=%v), want (true, false)", clearPrev, clearAlias)
		}
		for _, c := range calls {
			if strings.Join(c, " ") == "buildx uninstall" {
				t.Errorf("restoreBuildx must not run buildx uninstall when user already had alias")
			}
		}
		if got := dockerBuilderAlias(cfgPath); got != "buildx" {
			t.Fatalf("aliases.builder changed to %q, want buildx untouched", got)
		}
	})

	t.Run("alias_changed_by_user_not_uninstalled", func(t *testing.T) {
		dockerCfgDir := t.TempDir()
		t.Setenv("DOCKER_CONFIG", dockerCfgDir)
		t.Setenv("BUILDX_CONFIG", t.TempDir())
		cfgPath := filepath.Join(dockerCfgDir, "config.json")
		if err := os.WriteFile(cfgPath, []byte(`{"aliases":{"builder":"custom-builder"}}`), 0o600); err != nil {
			t.Fatal(err)
		}

		var calls [][]string
		run := func(_ context.Context, _ string, args ...string) ([]byte, error) {
			calls = append(calls, slices.Clone(args))
			key := strings.Join(args, " ")
			switch {
			case key == "buildx version":
				return []byte("v0.39.1"), nil
			case key == "-H "+dockerHost+" buildx inspect":
				return []byte("Name: default\n"), nil
			case key == "context inspect gvx":
				return nil, os.ErrNotExist
			}
			return nil, os.ErrInvalid
		}

		_, clearAlias := restoreBuildx(context.Background(), "/usr/local/bin/docker", run, dockerHost, "", true)
		if !clearAlias {
			t.Fatal("restoreBuildx should clear the recorded alias flag when alias no longer points to buildx")
		}
		for _, c := range calls {
			if strings.Join(c, " ") == "buildx uninstall" {
				t.Errorf("restoreBuildx must not uninstall a non-buildx alias: %v", c)
			}
		}
		if got := dockerBuilderAlias(cfgPath); got != "custom-builder" {
			t.Fatalf("aliases.builder = %q, want custom-builder untouched", got)
		}
	})

	t.Run("no_cli_and_no_buildx_and_no_ctx", func(t *testing.T) {
		t.Setenv("DOCKER_CONFIG", t.TempDir())
		t.Setenv("BUILDX_CONFIG", t.TempDir())

		st, _, _, installedAlias := configureBuildx(context.Background(), "", nil, sock, dockerHost, "")
		if !st.NoCLI || st.Ready || installedAlias {
			t.Fatalf("empty cli status = %+v installedAlias=%v, want NoCLI", st, installedAlias)
		}

		noPlugin := func(_ context.Context, _ string, args ...string) ([]byte, error) {
			return nil, os.ErrNotExist
		}
		st, _, _, installedAlias = configureBuildx(context.Background(), "/usr/local/bin/docker", noPlugin, sock, dockerHost, "")
		if !st.NoBuildx || st.Ready || installedAlias {
			t.Fatalf("no buildx status = %+v installedAlias=%v, want NoBuildx", st, installedAlias)
		}

		noCtx := func(_ context.Context, _ string, args ...string) ([]byte, error) {
			key := strings.Join(args, " ")
			switch {
			case key == "buildx version":
				return []byte("v0.39.1"), nil
			case key == "buildx install":
				return nil, nil
			case key == "buildx inspect gvx-desktop":
				return []byte("Name: gvx-desktop\nDriver: remote\nEndpoint: " + wantEP + "\nDriver Options: default-load=\"true\"\n"), nil
			case strings.HasPrefix(key, "-H "+dockerHost+" buildx use --default "):
				return nil, nil
			case key == "context inspect gvx":
				return nil, os.ErrNotExist
			}
			return nil, os.ErrInvalid
		}
		st, _, _, installedAlias = configureBuildx(context.Background(), "/usr/local/bin/docker", noCtx, sock, dockerHost, "")
		if !st.Ready || !st.NoCtx || st.Selected || !installedAlias {
			t.Fatalf("no gvx context status = %+v installedAlias=%v, want Ready+NoCtx+installedAlias", st, installedAlias)
		}
	})

	t.Run("real_docker_cli_temp_config", func(t *testing.T) {
		cli, ok := findCLI("docker", "")
		if !ok {
			t.Skip("docker CLI not installed")
		}
		// Link the read-only docker-buildx plugin binary into a temp
		// DOCKER_CONFIG so the real CLI finds buildx without touching ~/.docker.
		linkBuildxPlugin := func(dockerCfg string) {
			candidates := []string{
				"/opt/homebrew/lib/docker/cli-plugins/docker-buildx",
				"/usr/local/lib/docker/cli-plugins/docker-buildx",
			}
			if home, err := os.UserHomeDir(); err == nil && home != "" {
				candidates = append([]string{filepath.Join(home, ".docker", "cli-plugins", "docker-buildx")}, candidates...)
			}
			for _, cand := range candidates {
				if target, err := filepath.EvalSymlinks(cand); err == nil {
					dir := filepath.Join(dockerCfg, "cli-plugins")
					if err := os.MkdirAll(dir, 0o700); err == nil {
						_ = os.Symlink(target, filepath.Join(dir, "docker-buildx"))
					}
					return
				}
			}
		}
		probeCfg := t.TempDir()
		linkBuildxPlugin(probeCfg)
		t.Setenv("DOCKER_CONFIG", probeCfg)
		t.Setenv("BUILDX_CONFIG", t.TempDir())
		if _, err := runCLI(context.Background(), cli, "buildx", "version"); err != nil {
			t.Skipf("docker buildx plugin not available: %v", err)
		}

		// Case 1: alias absent -> installed on, removed on off.
		t.Run("alias_absent", func(t *testing.T) {
			dockerCfg := t.TempDir()
			buildxCfg := t.TempDir()
			linkBuildxPlugin(dockerCfg)
			t.Setenv("DOCKER_CONFIG", dockerCfg)
			t.Setenv("BUILDX_CONFIG", buildxCfg)
			cfgPath := filepath.Join(dockerCfg, "config.json")

			st, _, _, installedAlias := configureBuildx(context.Background(), cli, runCLI, sock, dockerHost, "")
			if !st.Ready || !installedAlias {
				t.Fatalf("configureBuildx = (%+v, installedAlias=%v), want Ready+installedAlias", st, installedAlias)
			}
			if got := dockerBuilderAlias(cfgPath); got != "buildx" {
				t.Fatalf("after configureBuildx: aliases.builder = %q, want buildx", got)
			}

			_, clearAlias := restoreBuildx(context.Background(), cli, runCLI, dockerHost, "", installedAlias)
			if !clearAlias {
				t.Fatal("restoreBuildx did not report clearAlias=true")
			}
			if got := dockerBuilderAlias(cfgPath); got != "" {
				t.Fatalf("after restoreBuildx: aliases.builder = %q, want empty", got)
			}
		})

		// Case 2: alias already present -> left alone on off.
		t.Run("alias_present", func(t *testing.T) {
			dockerCfg := t.TempDir()
			buildxCfg := t.TempDir()
			linkBuildxPlugin(dockerCfg)
			t.Setenv("DOCKER_CONFIG", dockerCfg)
			t.Setenv("BUILDX_CONFIG", buildxCfg)
			cfgPath := filepath.Join(dockerCfg, "config.json")
			if err := os.WriteFile(cfgPath, []byte(`{"aliases":{"builder":"buildx"}}`), 0o600); err != nil {
				t.Fatal(err)
			}

			st, _, _, installedAlias := configureBuildx(context.Background(), cli, runCLI, sock, dockerHost, "")
			if !st.Ready || installedAlias {
				t.Fatalf("configureBuildx = (%+v, installedAlias=%v), want Ready + installedAlias=false", st, installedAlias)
			}

			_, clearAlias := restoreBuildx(context.Background(), cli, runCLI, dockerHost, "", installedAlias)
			if clearAlias {
				t.Fatal("restoreBuildx should report clearAlias=false when app did not install alias")
			}
			if got := dockerBuilderAlias(cfgPath); got != "buildx" {
				t.Fatalf("after restoreBuildx: aliases.builder = %q, want buildx preserved", got)
			}
		})
	})
}
