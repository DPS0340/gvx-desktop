package engine

// Owner: frozen (architect). The fake Runner every workstream's unit tests
// use. Do not edit it: add helpers, even methods on *fakeRunner or
// *fakeProc, in your own _test.go files, and ask the coordinator for a
// change here.

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/ironpark/gvx-desktop/internal/rungv"
	"github.com/ironpark/gvx-desktop/internal/sandbox"
)

// fakeRunner runs no process: each Start makes a fakeProc that Script
// drives, and keeps images and volumes in maps.
type fakeRunner struct {
	mu      sync.Mutex
	event   func(sandbox.Event)
	output  func(string, sandbox.Output)
	procs   map[string]*fakeProc
	started []sandbox.Spec // every spec Start was called with, in order

	// Script, when set, is what a started sandbox does, in a goroutine of
	// its own; it returns when the guest ends, and its result is the exit
	// code. Without one, a sandbox runs until it is stopped or killed.
	Script func(p *fakeProc) int
	// StartErr, when set, fails Start.
	StartErr error
	// NoRunFlags names run flags this rungv lacks.
	NoRunFlags []string

	// rungv v0.20 fake controls (default zero Features = v0.19 behavior).
	features       rungv.Features
	controls       map[string]sandbox.Control
	defaultControl func(p *fakeProc) sandbox.Control
	pauseErr       error
	unpauseErr     error
	updateErr      error
	shutdownCalls  []fakeShutdownCall
	updateCalls    []fakeUpdateCall

	images    map[string]ImageInspect // by every RepoTag and by ID
	volumes   map[string]rungv.Volume
	pulls     []string
	pushCalls []rungv.PushOptions
	pushFunc  func(ctx context.Context, opts rungv.PushOptions, line func(string)) error

	// layerCacheDir, when set, is Info's LayerCacheDir: a real rungv cache
	// for what reads images from it (diff, export, commit).
	layerCacheDir string
	// loaded keeps every archive LoadImages read that was a tar.
	loaded [][]byte
}

type fakeShutdownCall struct {
	ID      string
	Signal  string
	Timeout time.Duration
}

type fakeUpdateCall struct {
	ID     string
	Update rungv.Update
}

// fakeProc is one run of a sandbox.
type fakeProc struct {
	r       *fakeRunner
	Spec    sandbox.Spec
	stdin   *io.PipeReader
	stdinW  *io.PipeWriter
	control sandbox.Control

	mu      sync.Mutex
	state   sandbox.State
	rows    uint16
	cols    uint16
	signals []syscall.Signal
	end     chan int // the exit code, from Stop, Signal or the script
	done    chan struct{}
}

func newFakeRunner() *fakeRunner {
	return &fakeRunner{
		procs:   map[string]*fakeProc{},
		images:  map[string]ImageInspect{},
		volumes: map[string]rungv.Volume{},
	}
}

// addImage caches an image under ref with cfg; it returns its ID.
func (f *fakeRunner) addImage(ref string, cfg ImageConfig) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	id := "sha256:" + fakeHex(ref)
	img := ImageInspect{ID: id, RepoTags: []string{ref}, Created: time.Unix(1700000000, 0).UTC(),
		Architecture: "arm64", Os: "linux", Config: cfg, Size: 1 << 20}
	f.images[ref] = img
	f.images[id] = img
	return id
}

// fakeHex is 64 hex digits that depend on s.
func fakeHex(s string) string {
	var b strings.Builder
	h := uint64(14695981039346656037)
	for b.Len() < 64 {
		for _, c := range []byte(s) {
			h = (h ^ uint64(c)) * 1099511628211
		}
		fmt.Fprintf(&b, "%016x", h)
	}
	return b.String()[:64]
}

// proc returns the current run of id, or nil.
func (f *fakeRunner) proc(id string) *fakeProc {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.procs[id]
}

// HasRunFlag reports whether the fake rungv takes the run flag name.
func (f *fakeRunner) HasRunFlag(name string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return !slices.Contains(f.NoRunFlags, name)
}

// Started returns the specs Start was called with.
func (f *fakeRunner) Started() []sandbox.Spec {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.started)
}

func (f *fakeRunner) SetHandlers(event func(sandbox.Event), output func(string, sandbox.Output)) {
	f.event, f.output = event, output
}

func (f *fakeRunner) Start(spec sandbox.Spec) error {
	f.mu.Lock()
	if f.StartErr != nil {
		f.mu.Unlock()
		return f.StartErr
	}
	if p := f.procs[spec.ID]; p != nil && p.State().Status.Active() {
		f.mu.Unlock()
		return fmt.Errorf("%s is already running", spec.Name)
	}
	pr, pw := io.Pipe()
	p := &fakeProc{r: f, Spec: spec.Clone(), stdin: pr, stdinW: pw,
		end: make(chan int, 1), done: make(chan struct{}),
		state: sandbox.State{Status: sandbox.Running, PID: 1000 + len(f.started), StartedAt: time.Now(), ReadyAt: time.Now(), Control: f.features.Control}}
	if f.features.Control {
		if ctl, ok := f.controls[spec.ID]; ok {
			p.control = ctl
		} else if f.defaultControl != nil {
			p.control = f.defaultControl(p)
		}
	}
	for _, port := range spec.Ports {
		p.state.Ports = append(p.state.Ports, fakePublished(port))
	}
	f.procs[spec.ID] = p
	f.started = append(f.started, spec.Clone())
	script := f.Script
	f.mu.Unlock()

	f.event(sandbox.Event{ID: spec.ID, Kind: sandbox.EventChanged, State: p.State()})
	if script != nil {
		go func() {
			code := script(p)
			select {
			case p.end <- code:
			default:
			}
		}()
	}
	go func() {
		code := <-p.end
		p.mu.Lock()
		p.state.PID = 0
		p.state.ExitedAt = time.Now()
		if p.state.Status == sandbox.Stopping {
			p.state.Status = sandbox.Stopped
		} else if code == 0 {
			p.state.Status = sandbox.Exited
		} else {
			p.state.Status = sandbox.Failed
		}
		if code < 0 {
			p.state.ExitCode = -1
			p.state.Signal = -code
		} else {
			p.state.ExitCode = code
		}
		st := p.state
		p.mu.Unlock()
		_ = p.stdinW.Close()
		close(p.done)
		f.event(sandbox.Event{ID: spec.ID, Kind: sandbox.EventExited, State: st})
	}()
	return nil
}

// fakePublished is the port rungv would say it published for a -p spec.
func fakePublished(spec string) sandbox.PublishedPort {
	proto := "tcp"
	if s, p, ok := strings.Cut(spec, "/"); ok {
		spec, proto = s, p
	}
	parts := strings.Split(spec, ":")
	guest := 0
	fmt.Sscan(parts[len(parts)-1], &guest)
	host := "127.0.0.1"
	port := parts[len(parts)-1]
	if len(parts) >= 2 && parts[len(parts)-2] != "" {
		port = parts[len(parts)-2]
	}
	if len(parts) == 3 {
		host = parts[0]
	}
	return sandbox.PublishedPort{Host: host + ":" + port, Guest: guest, Proto: proto}
}

func (f *fakeRunner) Stop(id string, timeout time.Duration) error {
	p := f.proc(id)
	if p == nil || !p.State().Status.Active() {
		return nil
	}
	p.mu.Lock()
	p.state.Status = sandbox.Stopping
	p.mu.Unlock()
	select {
	case p.end <- 130:
	default:
	}
	<-p.done
	return nil
}

func (f *fakeRunner) Signal(id string, sig syscall.Signal) error {
	p := f.proc(id)
	if p == nil || !p.State().Status.Active() {
		return errors.New("the sandbox is not running")
	}
	if ctl := f.Control(id); ctl != nil {
		err := ctl.Kill(context.Background(), rungv.KillOptions{Signal: signalName(sig)})
		if err == nil {
			return nil
		}
		if !isControlUnreachable(err) {
			return err
		}
	}
	p.mu.Lock()
	p.signals = append(p.signals, sig)
	p.mu.Unlock()
	if sig == syscall.SIGKILL || sig == syscall.SIGTERM || sig == syscall.SIGINT {
		select {
		case p.end <- -int(sig):
		default:
		}
	}
	return nil
}

func (f *fakeRunner) State(id string) sandbox.State {
	if p := f.proc(id); p != nil {
		return p.State()
	}
	return sandbox.State{}
}

func (f *fakeRunner) Stdin(id string) io.Writer {
	p := f.proc(id)
	if p == nil || !p.State().Status.Active() || !p.Spec.Interactive && !p.Spec.TTY {
		return nil
	}
	return p.stdinW
}

func (f *fakeRunner) CloseStdin(id string) error {
	p := f.proc(id)
	if p == nil {
		return errors.New("the sandbox is not running")
	}
	return p.stdinW.Close()
}

func (f *fakeRunner) Resize(id string, rows, cols uint16) error {
	p := f.proc(id)
	if p == nil || !p.Spec.TTY {
		return errors.New("the sandbox has no terminal")
	}
	p.mu.Lock()
	p.rows, p.cols = rows, cols
	p.mu.Unlock()
	return nil
}

func (f *fakeRunner) Forget(id string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if p := f.procs[id]; p != nil && !p.State().Status.Active() {
		delete(f.procs, id)
	}
}

// ---- rungv v0.20 (docs/design/gvx-v020.md §5): a v0.19 by default. ----

func (f *fakeRunner) Features() rungv.Features {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.features
}

func (f *fakeRunner) Control(id string) sandbox.Control {
	f.mu.Lock()
	defer f.mu.Unlock()
	p := f.procs[id]
	if p == nil || !p.State().Status.Active() {
		return nil
	}
	if ctl, ok := f.controls[id]; ok {
		return ctl
	}
	return p.control
}

func (f *fakeRunner) Pause(ctx context.Context, id string) error {
	if f.Control(id) == nil {
		return sandbox.ErrNoControl
	}
	f.mu.Lock()
	perr := f.pauseErr
	f.mu.Unlock()
	if perr != nil {
		return perr
	}
	p := f.proc(id)
	if p == nil || !p.State().Status.Active() {
		return errors.New("the sandbox is not running")
	}
	p.mu.Lock()
	p.state.Paused = true
	st := p.state
	p.mu.Unlock()
	f.event(sandbox.Event{ID: id, Kind: sandbox.EventChanged, State: st})
	return nil
}

func (f *fakeRunner) Unpause(ctx context.Context, id string) error {
	if f.Control(id) == nil {
		return sandbox.ErrNoControl
	}
	f.mu.Lock()
	uerr := f.unpauseErr
	f.mu.Unlock()
	if uerr != nil {
		return uerr
	}
	p := f.proc(id)
	if p == nil || !p.State().Status.Active() {
		return errors.New("the sandbox is not running")
	}
	p.mu.Lock()
	p.state.Paused = false
	st := p.state
	p.mu.Unlock()
	f.event(sandbox.Event{ID: id, Kind: sandbox.EventChanged, State: st})
	return nil
}

func (f *fakeRunner) Update(ctx context.Context, id string, u rungv.Update) (rungv.Resources, error) {
	if f.Control(id) == nil {
		return rungv.Resources{}, sandbox.ErrNoControl
	}
	f.mu.Lock()
	f.updateCalls = append(f.updateCalls, fakeUpdateCall{ID: id, Update: u})
	uerr := f.updateErr
	f.mu.Unlock()
	if uerr != nil {
		return rungv.Resources{}, uerr
	}
	return rungv.Resources{}, nil
}

func (f *fakeRunner) Shutdown(id string, signal string, timeout time.Duration) error {
	f.mu.Lock()
	f.shutdownCalls = append(f.shutdownCalls, fakeShutdownCall{ID: id, Signal: signal, Timeout: timeout})
	f.mu.Unlock()
	if p := f.proc(id); p != nil {
		p.mu.Lock()
		p.state.Paused = false
		p.mu.Unlock()
	}
	return f.Stop(id, timeout)
}

// State is the run's state.
func (p *fakeProc) State() sandbox.State {
	p.mu.Lock()
	defer p.mu.Unlock()
	st := p.state
	st.Ports = slices.Clone(st.Ports)
	return st
}

// Write sends what the guest writes on stream to the engine.
func (p *fakeProc) Write(stream sandbox.Stream, s string) {
	p.r.output(p.Spec.ID, sandbox.Output{Stream: stream, Data: []byte(s)})
}

// Stdin is what the engine writes to the guest's standard input; it ends
// at CloseStdin or when the run ends.
func (p *fakeProc) Stdin() io.Reader { return p.stdin }

// Exit ends the run with code, as the guest ending does.
func (p *fakeProc) Exit(code int) {
	select {
	case p.end <- code:
	default:
	}
}

// Done closes when the run has ended.
func (p *fakeProc) Done() <-chan struct{} { return p.done }

// Size is the terminal size Resize set.
func (p *fakeProc) Size() (rows, cols uint16) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.rows, p.cols
}

// Signals are the signals sent to the run.
func (p *fakeProc) Signals() []syscall.Signal {
	p.mu.Lock()
	defer p.mu.Unlock()
	return slices.Clone(p.signals)
}

func (f *fakeRunner) Images(ctx context.Context) ([]rungv.Image, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []rungv.Image
	for _, k := range slices.Sorted(maps.Keys(f.images)) {
		img := f.images[k]
		if k == img.ID {
			continue
		}
		repo, tag, _ := strings.Cut(k, ":")
		out = append(out, rungv.Image{ID: img.ID, Repository: repo, Tag: tag, Size: "1.049MB",
			CreatedAt: img.Created.Format("2006-01-02 15:04:05 -0700 MST")})
	}
	return out, nil
}

func (f *fakeRunner) InspectImages(ctx context.Context, refs ...string) ([]ImageInspect, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []ImageInspect
	for _, ref := range refs {
		img, ok := f.images[ref]
		if !ok {
			img, ok = f.images[ref+":latest"]
		}
		if !ok {
			return nil, &rungv.Error{Args: []string{"inspect", ref}, Code: 1, Stderr: "rungv inspect: image " + ref + " not found in the cache"}
		}
		out = append(out, img)
	}
	return out, nil
}

var _ ImagePushRunner = (*fakeRunner)(nil)

func (f *fakeRunner) PushImage(ctx context.Context, opts rungv.PushOptions, line func(string)) error {
	f.mu.Lock()
	f.pushCalls = append(f.pushCalls, opts)
	fn := f.pushFunc
	f.mu.Unlock()
	if fn != nil {
		return fn(ctx, opts, line)
	}
	if line != nil {
		ref := opts.Target
		if ref == "" {
			ref = opts.Ref
		}
		repo, tag := ref, "latest"
		if i := strings.LastIndex(ref, ":"); i > strings.LastIndex(ref, "/") {
			repo, tag = ref[:i], ref[i+1:]
		}
		line("The push refers to repository [" + repo + "]")
		line("a1b2c3d4e5f6: Pushed")
		line(tag + ": digest: sha256:" + fakeHex(ref) + " size: 528")
	}
	return nil
}

func (f *fakeRunner) Pull(ctx context.Context, ref, platform string, line func(string)) error {
	f.mu.Lock()
	f.pulls = append(f.pulls, ref)
	f.mu.Unlock()
	if strings.Contains(ref, "missing") {
		return &rungv.Error{Args: []string{"pull", ref}, Code: 1, Stderr: "rungv pull: manifest unknown"}
	}
	line(ref + ": Pulling from fake")
	line("Status: Downloaded newer image for " + ref)
	if !strings.Contains(ref, ":") {
		ref += ":latest"
	}
	f.addImage(ref, ImageConfig{Cmd: []string{"/bin/sh"}, Env: []string{"PATH=/usr/bin:/bin"}})
	return nil
}

func (f *fakeRunner) RemoveImage(ctx context.Context, ref string, force bool) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	img, ok := f.images[ref]
	if !ok {
		return &rungv.Error{Args: []string{"rmi", ref}, Code: 1, Stderr: "rungv rmi: image " + ref + " not found"}
	}
	delete(f.images, ref)
	if ref == img.ID {
		for k, v := range f.images {
			if v.ID == img.ID {
				delete(f.images, k)
			}
		}
	} else if !slices.ContainsFunc(slices.Collect(maps.Values(f.images)), func(v ImageInspect) bool { return v.ID == img.ID && v.RepoTags[0] != img.ID }) {
		delete(f.images, img.ID)
	}
	return nil
}

func (f *fakeRunner) TagImage(ctx context.Context, source, target string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	img, ok := f.images[source]
	if !ok {
		return &rungv.Error{Args: []string{"tag", source, target}, Code: 1, Stderr: "rungv tag: image " + source + " not found"}
	}
	img.RepoTags = append(slices.Clone(img.RepoTags), target)
	f.images[target] = img
	return nil
}

func (f *fakeRunner) LoadImages(ctx context.Context, archive io.Reader, line func(string)) error {
	b, err := io.ReadAll(archive)
	if err != nil {
		return err
	}
	if ids, ok := f.loadTar(b); ok {
		for _, id := range ids {
			line("Loaded image ID: " + id)
		}
		return nil
	}
	ref := strings.TrimSpace(string(b)) // the fake's archive is the reference it holds
	f.addImage(ref, ImageConfig{Cmd: []string{"/bin/sh"}})
	line("Loaded image: " + ref)
	return nil
}

// loadTar registers the images of a docker save archive or an OCI image
// layout: each by the digest of its config, under its tags.
func (f *fakeRunner) loadTar(b []byte) ([]string, bool) {
	files := map[string][]byte{}
	tr := tar.NewReader(bytes.NewReader(b))
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, false
		}
		body, _ := io.ReadAll(tr)
		files[h.Name] = body
	}
	var man []struct {
		Config   string
		RepoTags []string
	}
	if index, ok := files["index.json"]; ok {
		var idx struct {
			Manifests []struct {
				Digest      string
				Annotations map[string]string
			}
		}
		if json.Unmarshal(index, &idx) != nil {
			return nil, false
		}
		blob := func(d string) string { return "blobs/sha256/" + strings.TrimPrefix(d, "sha256:") }
		for _, m := range idx.Manifests {
			var mf struct{ Config struct{ Digest string } }
			if json.Unmarshal(files[blob(m.Digest)], &mf) != nil {
				return nil, false
			}
			var tags []string
			if n := m.Annotations["io.containerd.image.name"]; n != "" {
				tags = append(tags, n)
			}
			man = append(man, struct {
				Config   string
				RepoTags []string
			}{blob(mf.Config.Digest), tags})
		}
	} else if json.Unmarshal(files["manifest.json"], &man) != nil {
		return nil, false
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.loaded = append(f.loaded, b)
	var ids []string
	for _, m := range man {
		sum := sha256.Sum256(files[m.Config])
		id := "sha256:" + hex.EncodeToString(sum[:])
		var cfg struct {
			Config ImageConfig `json:"config"`
		}
		_ = json.Unmarshal(files[m.Config], &cfg)
		img := ImageInspect{ID: id, RepoTags: m.RepoTags, Created: time.Now().UTC(), Architecture: "arm64", Os: "linux", Config: cfg.Config}
		f.images[id] = img
		for _, t := range m.RepoTags {
			f.images[t] = img
		}
		ids = append(ids, id)
	}
	return ids, true
}

func (f *fakeRunner) SaveImages(ctx context.Context, w io.Writer, refs ...string) error {
	_, err := io.WriteString(w, strings.Join(refs, "\n"))
	return err
}

func (f *fakeRunner) PruneImages(ctx context.Context) (string, error) {
	return "Total reclaimed space: 0B", nil
}

func (f *fakeRunner) Volumes(ctx context.Context) ([]rungv.Volume, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []rungv.Volume
	for _, k := range slices.Sorted(maps.Keys(f.volumes)) {
		out = append(out, f.volumes[k])
	}
	return out, nil
}

func (f *fakeRunner) CreateVolume(ctx context.Context, name, driver string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if driver == "" {
		driver = "ext4lite"
	}
	if _, ok := f.volumes[name]; ok {
		return &rungv.Error{Args: []string{"volume", "create", name}, Code: 1, Stderr: "rungv volume create: volume " + name + " already exists"}
	}
	f.volumes[name] = rungv.Volume{Driver: driver, Name: name, Mountpoint: "/fake/volumes/" + name + ".ext4",
		CreatedAt: time.Now().UTC().Format("2006-01-02 15:04:05 -0700 MST")}
	return nil
}

func (f *fakeRunner) RemoveVolume(ctx context.Context, name string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.volumes[name]; !ok {
		return &rungv.Error{Args: []string{"volume", "rm", name}, Code: 1, Stderr: "rungv volume rm: volume " + name + " not found"}
	}
	delete(f.volumes, name)
	return nil
}

func (f *fakeRunner) DiskUsage(ctx context.Context) ([]rungv.DiskUsage, error) {
	return []rungv.DiskUsage{{Type: "Images", TotalCount: len(f.images) / 2, Size: "0B", Reclaimable: "0B"},
		{Type: "Local Volumes", TotalCount: len(f.volumes), Size: "0B", Reclaimable: "0B"}}, nil
}

func (f *fakeRunner) Info(ctx context.Context) (rungv.Info, error) {
	f.mu.Lock()
	cache := f.layerCacheDir
	f.mu.Unlock()
	if cache == "" {
		cache = "/fake/cache"
	}
	return rungv.Info{OS: "darwin", Architecture: "arm64", NumCPU: 8, DefaultBackend: "hvf",
		AvailableBackends: []string{"hvf"}, LayerCacheDir: cache, VolumeDir: "/fake/cache/volumes"}, nil
}

// testEngine is an open engine over a fake runner, in a temporary
// directory, with an HTTP server in front of it.
type testEngine struct {
	*Engine
	fake *fakeRunner
	srv  *httptest.Server
	dir  string
}

// newTestEngine opens an engine in a temporary directory over a new fake
// runner. A test that restarts the engine passes the same dir and fake to
// reopen.
func newTestEngine(t *testing.T) *testEngine {
	t.Helper()
	return reopenTestEngine(t, t.TempDir(), newFakeRunner())
}

// reopenTestEngine opens an engine over dir and fake, as the app does at
// its next launch.
func reopenTestEngine(t *testing.T, dir string, fake *fakeRunner) *testEngine {
	t.Helper()
	e := New(Config{Dir: dir, Runner: fake, Version: "0.0.0-test"})
	if err := e.Open(context.Background()); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(e.Handler())
	te := &testEngine{Engine: e, fake: fake, srv: srv, dir: dir}
	t.Cleanup(func() {
		srv.Close()
		_ = e.Close(time.Second)
	})
	return te
}

// do sends a request with a JSON body (nil for none) and returns the
// status and the body.
func (te *testEngine) do(t *testing.T, method, path string, body any) (int, []byte) {
	t.Helper()
	var rd io.Reader
	switch b := body.(type) {
	case nil:
	case string:
		rd = strings.NewReader(b)
	case []byte:
		rd = bytes.NewReader(b)
	default:
		rd = strings.NewReader(mustJSON(t, b))
	}
	req, err := http.NewRequest(method, te.srv.URL+path, rd)
	if err != nil {
		t.Fatal(err)
	}
	if rd != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	res, err := te.srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	out, err := io.ReadAll(res.Body)
	if err != nil {
		t.Fatal(err)
	}
	return res.StatusCode, out
}
