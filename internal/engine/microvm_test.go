package engine

import (
	"archive/tar"
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/net/dns/dnsmessage"

	"github.com/ironpark/gvx-desktop-vm/guestproto"
	"github.com/ironpark/gvx-desktop-vm/vmassets"
	"github.com/ironpark/gvx-desktop-vm/vmclient"
	"github.com/ironpark/gvx-desktop-vm/vmhelper"
	"github.com/ironpark/gvx-desktop-vm/vmproto"
	"github.com/ironpark/gvx-desktop/internal/rungv"
)

// ---- fake micro-VM client & stream ----

type fakeMicroVMStream struct {
	pid     int
	state   *vmproto.ContainerState
	stdoutR *io.PipeReader
	stdoutW *io.PipeWriter
	stderrR *io.PipeReader
	stderrW *io.PipeWriter
	stdinR  *io.PipeReader
	stdinW  *io.PipeWriter

	mu        sync.Mutex
	resizes   [][2]uint16
	exit      vmproto.ContainerExit
	exitErr   error
	finished  bool
	done      chan struct{}
	closeOnce sync.Once
}

func newFakeMicroVMStream(pid int) *fakeMicroVMStream {
	outR, outW := io.Pipe()
	errR, errW := io.Pipe()
	inR, inW := io.Pipe()
	return &fakeMicroVMStream{
		pid:     pid,
		state:   &vmproto.ContainerState{Running: true, PID: pid},
		stdoutR: outR,
		stdoutW: outW,
		stderrR: errR,
		stderrW: errW,
		stdinR:  inR,
		stdinW:  inW,
		done:    make(chan struct{}),
	}
}

func (s *fakeMicroVMStream) PID() int { return s.pid }
func (s *fakeMicroVMStream) State() *vmproto.ContainerState {
	if s.state == nil {
		return nil
	}
	cp := *s.state
	return &cp
}
func (s *fakeMicroVMStream) Stdin() io.WriteCloser            { return s.stdinW }
func (s *fakeMicroVMStream) WriteStdin(p []byte) (int, error) { return s.stdinW.Write(p) }
func (s *fakeMicroVMStream) CloseStdin() error                { return s.stdinW.Close() }
func (s *fakeMicroVMStream) Stdout() io.Reader                { return s.stdoutR }
func (s *fakeMicroVMStream) Stderr() io.Reader                { return s.stderrR }

func (s *fakeMicroVMStream) Resize(rows, cols uint16) error {
	s.mu.Lock()
	s.resizes = append(s.resizes, [2]uint16{rows, cols})
	s.mu.Unlock()
	return nil
}

func (s *fakeMicroVMStream) Wait() (vmproto.ContainerExit, error) {
	<-s.done
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.exit, s.exitErr
}

func (s *fakeMicroVMStream) finish(ex vmproto.ContainerExit, err error) {
	s.mu.Lock()
	if s.finished {
		s.mu.Unlock()
		return
	}
	s.finished = true
	s.exit = ex
	s.exitErr = err
	s.mu.Unlock()
	_ = s.stdoutW.Close()
	_ = s.stderrW.Close()
	_ = s.stdinR.Close()
	close(s.done)
}

func (s *fakeMicroVMStream) Close() error {
	s.closeOnce.Do(func() {
		_ = s.stdoutR.Close()
		_ = s.stderrR.Close()
		_ = s.stdinW.Close()
		s.finish(vmproto.ContainerExit{}, net.ErrClosed)
	})
	return nil
}

type fakeMicroVMClient struct {
	mu           sync.Mutex
	socket       string
	vmSpec       vmproto.VMSpec
	ctrSpec      vmproto.ContainerSpec
	signals      []vmproto.ContainerSignalRequest
	resizes      [][2]uint16
	execReqs     []vmproto.ContainerExecRequest
	copyFromReqs []vmproto.ContainerCopyRequest
	copyToReqs   []vmproto.ContainerCopyRequest
	copyToData   [][]byte
	exportReqs   []vmproto.ContainerExportRequest
	dialReqs     []vmproto.DialRequest
	netAttachCfg *vmproto.ContainerNetConfig
	netStream    *fakeMicroVMNetStream
	netUpdates   []vmproto.ContainerNetConfig

	attach  *fakeMicroVMStream
	paused  bool
	closed  bool
	done    chan struct{}
	exitErr error

	startErr    error
	ctrStartErr error
	onStart     func(c *fakeMicroVMClient)
	onSignal    func(c *fakeMicroVMClient, req vmproto.ContainerSignalRequest) error
	onDial      func(ctx context.Context, req vmproto.DialRequest) (net.Conn, error)
	onExec      func(ctx context.Context, req vmproto.ContainerExecRequest) (microVMStream, error)
	onStat      func(req vmproto.ContainerCopyRequest) (vmproto.ContainerPathStat, error)
	onCopyFrom  func(req vmproto.ContainerCopyRequest) ([]byte, error)
	onExport    func(req vmproto.ContainerExportRequest) ([]byte, error)
	topProcs    []vmproto.ContainerProcess
	statsSample vmproto.ContainerStats
}

func newFakeMicroVMClient(sockDir string) *fakeMicroVMClient {
	return &fakeMicroVMClient{
		socket: filepath.Join(sockDir, vmhelper.SocketName),
		attach: newFakeMicroVMStream(42),
		done:   make(chan struct{}),
		topProcs: []vmproto.ContainerProcess{
			{PID: 1, PPID: 0, UID: 0, GID: 0, State: "S", Threads: 1, CPUNanos: 2_000_000_000, Comm: "sh", Cmdline: []string{"/bin/sh"}},
		},
		statsSample: vmproto.ContainerStats{
			Timestamp:        time.Now().UTC(),
			CPUUserNanos:     50_000_000,
			CPUSystemNanos:   10_000_000,
			MemoryBytes:      16 << 20,
			MemoryAnonBytes:  12 << 20,
			MemoryFileBytes:  4 << 20,
			MemoryLimitBytes: 1024 << 20,
			Network: []vmproto.ContainerNetStats{
				{Interface: "eth0", RxBytes: 1234, TxBytes: 5678, RxPackets: 10, TxPackets: 20},
			},
			PIDs:      2,
			PIDsLimit: 128,
		},
	}
}

func (c *fakeMicroVMClient) Socket() string        { return c.socket }
func (c *fakeMicroVMClient) Done() <-chan struct{} { return c.done }
func (c *fakeMicroVMClient) ExitErr() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.exitErr
}

func (c *fakeMicroVMClient) Start(ctx context.Context, spec vmproto.VMSpec) (vmproto.Status, error) {
	c.mu.Lock()
	c.vmSpec = spec
	err := c.startErr
	c.mu.Unlock()
	if err != nil {
		return vmproto.Status{State: vmproto.Failed, Err: err.Error()}, err
	}
	// Simulate vmhelper creating the persistent rootfs ext4 file.
	if spec.Disk != nil && spec.Disk.Path != "" {
		if _, serr := os.Stat(spec.Disk.Path); os.IsNotExist(serr) {
			_ = os.WriteFile(spec.Disk.Path, []byte("fake-ext4"), 0o600)
		}
	}
	return vmproto.Status{State: vmproto.Running, VMPID: 9999}, nil
}

func (c *fakeMicroVMClient) Dial(ctx context.Context, req vmproto.DialRequest) (net.Conn, error) {
	c.mu.Lock()
	c.dialReqs = append(c.dialReqs, req)
	fn := c.onDial
	c.mu.Unlock()
	if fn != nil {
		return fn(ctx, req)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	go func() {
		defer ln.Close()
		srv, err := ln.Accept()
		if err != nil {
			return
		}
		defer srv.Close()
		data, _ := io.ReadAll(srv)
		_, _ = srv.Write(append([]byte("guest:"), data...))
		if cw, ok := srv.(interface{ CloseWrite() error }); ok {
			_ = cw.CloseWrite()
		}
	}()
	return net.DialTimeout("tcp", ln.Addr().String(), 2*time.Second)
}

func (c *fakeMicroVMClient) Close(timeout time.Duration) error {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return nil
	}
	c.closed = true
	close(c.done)
	c.mu.Unlock()
	c.attach.finish(vmproto.ContainerExit{Code: 0}, nil)
	return nil
}

func (c *fakeMicroVMClient) crash(err error) {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return
	}
	c.closed = true
	c.exitErr = err
	close(c.done)
	c.mu.Unlock()
}

func (c *fakeMicroVMClient) ContainerStart(ctx context.Context, spec vmproto.ContainerSpec) (vmproto.ContainerState, error) {
	c.mu.Lock()
	c.ctrSpec = spec
	err := c.ctrStartErr
	onStart := c.onStart
	c.mu.Unlock()
	if err != nil {
		return vmproto.ContainerState{}, err
	}
	if onStart != nil {
		go onStart(c)
	}
	return vmproto.ContainerState{Running: true, PID: c.attach.pid}, nil
}

func (c *fakeMicroVMClient) ContainerAttach(ctx context.Context) (microVMStream, error) {
	return c.attach, nil
}

type fakeNetFrame struct {
	kind    byte
	payload []byte
}

type fakeMicroVMNetStream struct {
	guestToHost chan fakeNetFrame
	hostToGuest chan fakeNetFrame
	closed      chan struct{}
	once        sync.Once
}

func newFakeMicroVMNetStream() *fakeMicroVMNetStream {
	return &fakeMicroVMNetStream{
		guestToHost: make(chan fakeNetFrame, 32),
		hostToGuest: make(chan fakeNetFrame, 32),
		closed:      make(chan struct{}),
	}
}

func (s *fakeMicroVMNetStream) ReadFrame() (byte, []byte, error) {
	select {
	case f := <-s.guestToHost:
		return f.kind, f.payload, nil
	case <-s.closed:
		return 0, nil, io.EOF
	}
}

func (s *fakeMicroVMNetStream) SendPacket(pkt []byte) error {
	select {
	case <-s.closed:
		return net.ErrClosed
	default:
	}
	select {
	case s.hostToGuest <- fakeNetFrame{kind: guestproto.FrameNetPacket, payload: append([]byte(nil), pkt...)}:
		return nil
	case <-s.closed:
		return net.ErrClosed
	}
}

func (s *fakeMicroVMNetStream) SendDNSReply(seq uint32, reply []byte) error {
	buf := make([]byte, 4+len(reply))
	binary.BigEndian.PutUint32(buf[0:4], seq)
	copy(buf[4:], reply)
	select {
	case <-s.closed:
		return net.ErrClosed
	default:
	}
	select {
	case s.hostToGuest <- fakeNetFrame{kind: guestproto.FrameDNSReply, payload: buf}:
		return nil
	case <-s.closed:
		return net.ErrClosed
	}
}

func (s *fakeMicroVMNetStream) Close() error {
	s.once.Do(func() { close(s.closed) })
	return nil
}

func (c *fakeMicroVMClient) ContainerNetAttach(ctx context.Context, cfg vmproto.ContainerNetConfig) (microVMNetStream, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	cp := cfg
	cp.Addresses = slices.Clone(cfg.Addresses)
	cp.Routes = slices.Clone(cfg.Routes)
	c.netAttachCfg = &cp
	c.netStream = newFakeMicroVMNetStream()
	return c.netStream, nil
}

func (c *fakeMicroVMClient) ContainerNetUpdate(ctx context.Context, cfg vmproto.ContainerNetConfig) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	cp := cfg
	cp.Addresses = slices.Clone(cfg.Addresses)
	cp.Routes = slices.Clone(cfg.Routes)
	c.netUpdates = append(c.netUpdates, cp)
	return nil
}

func (c *fakeMicroVMClient) ContainerSignal(ctx context.Context, req vmproto.ContainerSignalRequest) error {
	c.mu.Lock()
	c.signals = append(c.signals, req)
	fn := c.onSignal
	c.mu.Unlock()
	if fn != nil {
		return fn(c, req)
	}
	if req.PID == 0 {
		switch strings.ToUpper(req.Signal) {
		case "SIGTERM", "TERM":
			c.attach.finish(vmproto.ContainerExit{Code: 143, Signal: "SIGTERM"}, nil)
		case "SIGKILL", "KILL", "":
			c.attach.finish(vmproto.ContainerExit{Code: 137, Signal: "SIGKILL"}, nil)
		}
	}
	return nil
}

func (c *fakeMicroVMClient) ContainerResize(ctx context.Context, rows, cols uint16) error {
	c.mu.Lock()
	c.resizes = append(c.resizes, [2]uint16{rows, cols})
	c.mu.Unlock()
	return nil
}

func (c *fakeMicroVMClient) ContainerExec(ctx context.Context, req vmproto.ContainerExecRequest) (microVMStream, error) {
	c.mu.Lock()
	if c.paused {
		c.mu.Unlock()
		return nil, &vmproto.Error{Code: vmproto.CodePaused, Message: "container is paused"}
	}
	c.execReqs = append(c.execReqs, req)
	fn := c.onExec
	c.mu.Unlock()
	if fn != nil {
		return fn(ctx, req)
	}
	pid := 100
	if len(req.Argv) > 0 && req.Argv[0] == "missing-bin" {
		pid = 0
	}
	st := newFakeMicroVMStream(pid)
	go func() {
		if len(req.Argv) > 0 && req.Argv[0] == "missing-bin" {
			st.finish(vmproto.ContainerExit{Code: 127, Error: "exec: \"missing-bin\": executable file not found in $PATH"}, nil)
			return
		}
		if req.Interactive {
			in, _ := io.ReadAll(st.stdinR)
			_, _ = st.stdoutW.Write(append([]byte("echo:"), in...))
		} else {
			_, _ = st.stdoutW.Write([]byte("exec-out:" + strings.Join(req.Argv, " ") + "\n"))
			if !req.TTY {
				_, _ = st.stderrW.Write([]byte("exec-err\n"))
			}
		}
		st.finish(vmproto.ContainerExit{Code: 0}, nil)
	}()
	return st, nil
}

func (c *fakeMicroVMClient) ContainerTop(ctx context.Context) ([]vmproto.ContainerProcess, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return slices.Clone(c.topProcs), nil
}

func (c *fakeMicroVMClient) ContainerStats(ctx context.Context) (vmproto.ContainerStats, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	st := c.statsSample
	st.Timestamp = time.Now().UTC()
	return st, nil
}

func (c *fakeMicroVMClient) ContainerPause(ctx context.Context) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.paused {
		return &vmproto.Error{Code: vmproto.CodeAlreadyPaused, Message: "container is already paused"}
	}
	c.paused = true
	return nil
}

func (c *fakeMicroVMClient) ContainerUnpause(ctx context.Context) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.paused {
		return &vmproto.Error{Code: vmproto.CodeNotPaused, Message: "container is not paused"}
	}
	c.paused = false
	return nil
}

func (c *fakeMicroVMClient) ContainerStat(ctx context.Context, req vmproto.ContainerCopyRequest) (vmproto.ContainerPathStat, error) {
	c.mu.Lock()
	fn := c.onStat
	c.mu.Unlock()
	if fn != nil {
		return fn(req)
	}
	if req.Path == "/missing" {
		return vmproto.ContainerPathStat{}, &vmproto.Error{Code: vmproto.CodeNotFound, Message: "not found"}
	}
	if req.Path == "/tmp" || req.Path == "/" {
		return vmproto.ContainerPathStat{Name: filepath.Base(req.Path), Mode: uint32(os.ModeDir | 0o755), Mtime: time.Now().UTC()}, nil
	}
	return vmproto.ContainerPathStat{Name: filepath.Base(req.Path), Size: 5, Mode: 0o644, Mtime: time.Now().UTC()}, nil
}

func (c *fakeMicroVMClient) ContainerCopyFrom(ctx context.Context, req vmproto.ContainerCopyRequest) (io.ReadCloser, error) {
	c.mu.Lock()
	c.copyFromReqs = append(c.copyFromReqs, req)
	fn := c.onCopyFrom
	c.mu.Unlock()
	if fn != nil {
		data, err := fn(req)
		if err != nil {
			return nil, err
		}
		return io.NopCloser(bytes.NewReader(data)), nil
	}
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	body := []byte("hello")
	_ = tw.WriteHeader(&tar.Header{Name: filepath.Base(req.Path), Typeflag: tar.TypeReg, Mode: 0o644, Size: int64(len(body))})
	_, _ = tw.Write(body)
	_ = tw.Close()
	return io.NopCloser(bytes.NewReader(buf.Bytes())), nil
}

func (c *fakeMicroVMClient) ContainerCopyTo(ctx context.Context, req vmproto.ContainerCopyRequest, r io.Reader) error {
	data, err := io.ReadAll(r)
	if err != nil {
		return err
	}
	c.mu.Lock()
	c.copyToReqs = append(c.copyToReqs, req)
	c.copyToData = append(c.copyToData, data)
	c.mu.Unlock()
	return nil
}

func (c *fakeMicroVMClient) ContainerExport(ctx context.Context, req vmproto.ContainerExportRequest) (io.ReadCloser, error) {
	c.mu.Lock()
	c.exportReqs = append(c.exportReqs, req)
	fn := c.onExport
	c.mu.Unlock()
	if fn != nil {
		data, err := fn(req)
		if err != nil {
			return nil, err
		}
		return io.NopCloser(bytes.NewReader(data)), nil
	}
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	body := []byte("added-in-microvm\n")
	_ = tw.WriteHeader(&tar.Header{Name: "added.txt", Typeflag: tar.TypeReg, Mode: 0o644, Size: int64(len(body))})
	_, _ = tw.Write(body)
	_ = tw.Close()
	return io.NopCloser(bytes.NewReader(buf.Bytes())), nil
}

// ---- test helpers ----

// addCachedMicroVMImage writes a valid rungv image record + compiled .erofs
// layer files into te.fake.layerCacheDir and registers the image in fakeRunner.
func addCachedMicroVMImage(t *testing.T, te *testEngine, arch, ref string, cfg ImageConfig, numLayers int) (string, []string) {
	t.Helper()
	if te.fake.layerCacheDir == "" {
		te.fake.layerCacheDir = t.TempDir()
	}
	if arch == "" {
		arch = hostImageArch
	}
	if numLayers <= 0 {
		numLayers = 2
	}

	rawCfg, err := json.Marshal(map[string]any{
		"architecture": arch,
		"os":           "linux",
		"config":       cfg,
		"rootfs":       map[string]any{"type": "layers", "diff_ids": []string{}},
	})
	if err != nil {
		t.Fatal(err)
	}
	cfgSum := sha256.Sum256(rawCfg)
	id := "sha256:" + hex.EncodeToString(cfgSum[:])

	// Reuse a real fixture .erofs file for layer content so imagefs.OpenGVXImage
	// (used by diff/export/commit) can parse the layers too.
	fixtureErofs, err := os.ReadFile(filepath.Join("..", "imagefs", "testdata", "gvxcache",
		"688af45ba4817240ad535eda8600e73bd05274b29416d43aaf63793928b043aa.gvx-erofs-v3-b4096.erofs"))
	if err != nil {
		t.Fatalf("read fixture erofs: %v", err)
	}

	var layerDescs []map[string]any
	var layerPaths []string
	const format = "gvx-erofs-v3-b4096"
	for i := range numLayers {
		sum := sha256.Sum256([]byte(fmt.Sprintf("%s:%s:layer-%d", arch, ref, i)))
		lhex := hex.EncodeToString(sum[:])
		lp := filepath.Join(te.fake.layerCacheDir, lhex+"."+format+".erofs")
		if err := os.WriteFile(lp, fixtureErofs, 0o600); err != nil {
			t.Fatal(err)
		}
		absLP, _ := filepath.Abs(lp)
		layerPaths = append(layerPaths, absLP)
		layerDescs = append(layerDescs, map[string]any{
			"mediaType": "application/vnd.oci.image.layer.v1.tar",
			"digest":    "sha256:" + lhex,
			"size":      len(fixtureErofs),
		})
	}

	manifest, _ := json.Marshal(map[string]any{
		"schemaVersion": 2,
		"mediaType":     "application/vnd.oci.image.manifest.v1+json",
		"config":        map[string]any{"digest": id, "size": len(rawCfg)},
		"layers":        layerDescs,
	})
	rec, _ := json.Marshal(map[string]any{
		"version":      2,
		"repository":   ref,
		"digest":       "sha256:" + hex.EncodeToString(cfgSum[:]),
		"architecture": arch,
		"format":       format,
		"manifests":    [][]byte{manifest},
		"config":       rawCfg,
	})
	recPath := filepath.Join(te.fake.layerCacheDir, "image-"+hex.EncodeToString(cfgSum[:16])+".json")
	if err := os.WriteFile(recPath, rec, 0o600); err != nil {
		t.Fatal(err)
	}

	img := ImageInspect{
		ID:           id,
		RepoTags:     []string{ref},
		Created:      time.Unix(1700000000, 0).UTC(),
		Architecture: arch,
		Os:           "linux",
		Config:       cfg,
		Size:         1 << 20,
	}
	if arch == hostImageArch {
		te.fake.mu.Lock()
		te.fake.images[ref] = img
		te.fake.images[id] = img
		te.fake.mu.Unlock()
	} else {
		s := te.fake.archStore()
		s.mu.Lock()
		ns := s.ns(arch)
		ns[ref] = img
		ns[id] = img
		s.mu.Unlock()
	}
	return id, layerPaths
}

func makeFakeVMAssetsDir(t *testing.T) vmassets.Dir {
	t.Helper()
	dir := t.TempDir()
	_ = os.WriteFile(filepath.Join(dir, "vmlinuz"), []byte("fake-kernel"), 0o600)
	_ = os.WriteFile(filepath.Join(dir, "initramfs.gz"), []byte("fake-initrd"), 0o600)
	return vmassets.Dir{
		Path:     dir,
		Override: false,
		Manifest: vmassets.Manifest{Version: "2"},
	}
}

func setupFakeMicroVM(t *testing.T, te *testEngine, rosettaReady bool, configure func(*fakeMicroVMClient)) (clients func() []*fakeMicroVMClient, assetCalls *atomic.Int32) {
	t.Helper()
	assetsDir := makeFakeVMAssetsDir(t)
	var calls atomic.Int32
	var mu sync.Mutex
	var created []*fakeMicroVMClient

	te.Engine.mvm.launchFn = func(ctx context.Context, l vmclient.Launcher) (microVMClient, error) {
		fc := newFakeMicroVMClient(l.Dir)
		if configure != nil {
			configure(fc)
		}
		mu.Lock()
		created = append(created, fc)
		mu.Unlock()
		return fc, nil
	}

	err := te.SetMicroVM(&MicroVMConfig{
		Launcher: vmclient.Launcher{
			Path: "/fake/gvx-desktop",
			Args: []string{vmhelper.Arg, vmhelper.CmdServe},
		},
		Assets: func(ctx context.Context, progress func(done, total int64)) (vmassets.Dir, error) {
			calls.Add(1)
			if progress != nil {
				progress(512, 1024)
				progress(1024, 1024)
			}
			return assetsDir, nil
		},
		RosettaReady: func() bool { return rosettaReady },
	})
	if err != nil {
		t.Fatalf("SetMicroVM: %v", err)
	}
	return func() []*fakeMicroVMClient {
		mu.Lock()
		defer mu.Unlock()
		return slices.Clone(created)
	}, &calls
}

func waitForContainerStatus(t *testing.T, te *testEngine, id, want string, timeout time.Duration) containerInspectResponse {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		st, body := te.do(t, http.MethodGet, "/containers/"+id+"/json", nil)
		if st == http.StatusOK {
			var ins containerInspectResponse
			if json.Unmarshal(body, &ins) == nil && ins.State.Status == want {
				return ins
			}
		}
		time.Sleep(15 * time.Millisecond)
	}
	_, body := te.do(t, http.MethodGet, "/containers/"+id+"/json", nil)
	t.Fatalf("timed out waiting for container %s status %q; last inspect: %s", id, want, body)
	return containerInspectResponse{}
}

// ---- Tests ----

func TestMicroVMRuntimeDecisionMatrix(t *testing.T) {
	te := newTestEngine(t)
	foreign := foreignImageArch()

	addCachedMicroVMImage(t, te, hostImageArch, "native:1", ImageConfig{Cmd: []string{"/bin/sh"}}, 1)
	addCachedMicroVMImage(t, te, foreign, "foreign-only:1", ImageConfig{Cmd: []string{"/bin/sh"}}, 1)
	addCachedMicroVMImage(t, te, hostImageArch, "dual:1", ImageConfig{Cmd: []string{"/bin/sh"}}, 1)
	addCachedMicroVMImage(t, te, foreign, "dual:1", ImageConfig{Cmd: []string{"/bin/sh"}}, 1)

	getInfoRuntimes := func() map[string]map[string]string {
		st, body := te.do(t, http.MethodGet, "/info", nil)
		if st != http.StatusOK {
			t.Fatalf("GET /info = %d %s", st, body)
		}
		var info struct {
			DefaultRuntime string                       `json:"DefaultRuntime"`
			Runtimes       map[string]map[string]string `json:"Runtimes"`
		}
		if err := json.Unmarshal(body, &info); err != nil {
			t.Fatal(err)
		}
		if info.DefaultRuntime != RuntimeGVX {
			t.Errorf("DefaultRuntime = %q, want %q", info.DefaultRuntime, RuntimeGVX)
		}
		return info.Runtimes
	}

	// ---- Phase 1: Micro-VM OFF ----
	rts := getInfoRuntimes()
	if rts[RuntimeGVX]["path"] != "rungv" {
		t.Fatalf("OFF Runtimes[gvx] = %v, want rungv", rts[RuntimeGVX])
	}
	if _, ok := rts[RuntimeMicroVM]; ok {
		t.Fatalf("OFF Runtimes unexpectedly exposes microvm: %v", rts)
	}

	// "" + host -> 201 gvx
	st, body := te.do(t, http.MethodPost, "/containers/create?name=off-host", map[string]any{"Image": "native:1"})
	if st != http.StatusCreated {
		t.Fatalf("OFF host default = %d %s, want 201", st, body)
	}
	// "gvx" + host -> 201 gvx
	st, body = te.do(t, http.MethodPost, "/containers/create?name=off-gvx", map[string]any{
		"Image":      "native:1",
		"HostConfig": map[string]any{"Runtime": "gvx"},
	})
	if st != http.StatusCreated {
		t.Fatalf("OFF host gvx = %d %s, want 201", st, body)
	}
	// "" + foreign cached -> 400 (with foreignRunMessage + Labs hint)
	st, body = te.do(t, http.MethodPost, "/containers/create?name=off-foreign", map[string]any{"Image": "foreign-only:1"})
	if st != http.StatusBadRequest || !strings.Contains(string(body), "Labs") {
		t.Fatalf("OFF foreign cached = %d %s, want 400 Labs hint", st, body)
	}
	// "" + foreign uncached ?platform -> 400 (NOT 404)
	st, body = te.do(t, http.MethodPost, "/containers/create?name=off-foreign-plat&platform=linux/"+foreign, map[string]any{"Image": "uncached:1"})
	if st != http.StatusBadRequest {
		t.Fatalf("OFF foreign platform = %d %s, want 400", st, body)
	}
	// "microvm" + host -> 400 disabled
	st, body = te.do(t, http.MethodPost, "/containers/create?name=off-mvm-host", map[string]any{
		"Image":      "native:1",
		"HostConfig": map[string]any{"Runtime": "microvm"},
	})
	if st != http.StatusBadRequest || !strings.Contains(string(body), "microvm") {
		t.Fatalf("OFF microvm host = %d %s, want 400", st, body)
	}
	// Labels["gvx.runtime"]="microvm" -> 400 disabled
	st, body = te.do(t, http.MethodPost, "/containers/create?name=off-label", map[string]any{
		"Image":  "native:1",
		"Labels": map[string]string{"gvx.runtime": "microvm"},
	})
	if st != http.StatusBadRequest {
		t.Fatalf("OFF label microvm = %d %s, want 400", st, body)
	}
	// Unknown runtime -> 400
	st, body = te.do(t, http.MethodPost, "/containers/create?name=off-unknown", map[string]any{
		"Image":      "native:1",
		"HostConfig": map[string]any{"Runtime": "runc"},
	})
	if st != http.StatusBadRequest || !strings.Contains(string(body), "Unknown runtime specified runc") {
		t.Fatalf("OFF unknown runtime = %d %s, want 400", st, body)
	}

	// ---- Phase 2: Micro-VM ON (RosettaReady = true) ----
	setupFakeMicroVM(t, te, true, nil)

	rts = getInfoRuntimes()
	if rts[RuntimeGVX]["path"] != "rungv" || rts[RuntimeMicroVM]["path"] != "gvx-microvm" {
		t.Fatalf("ON Runtimes = %v, want gvx+microvm", rts)
	}

	// "" + host -> 201 gvx
	st, body = te.do(t, http.MethodPost, "/containers/create?name=on-host-def", map[string]any{"Image": "native:1"})
	if st != http.StatusCreated {
		t.Fatalf("ON host default = %d %s", st, body)
	}
	var cr containerCreateResponse
	_ = json.Unmarshal(body, &cr)
	_, ib := te.do(t, http.MethodGet, "/containers/"+cr.ID+"/json", nil)
	var ins containerInspectResponse
	_ = json.Unmarshal(ib, &ins)
	if ins.Driver != "gvx" {
		t.Errorf("host default Driver = %q, want gvx", ins.Driver)
	}

	// "microvm" + host -> 201 microvm
	st, body = te.do(t, http.MethodPost, "/containers/create?name=on-host-mvm", map[string]any{
		"Image":      "native:1",
		"HostConfig": map[string]any{"Runtime": "microvm"},
	})
	if st != http.StatusCreated {
		t.Fatalf("ON host microvm = %d %s", st, body)
	}
	cr = containerCreateResponse{}
	_ = json.Unmarshal(body, &cr)
	_, ib = te.do(t, http.MethodGet, "/containers/"+cr.ID+"/json", nil)
	ins = containerInspectResponse{}
	_ = json.Unmarshal(ib, &ins)
	if ins.Driver != "gvx-microvm" || ins.HostConfig.Runtime != RuntimeMicroVM || ins.Platform != "linux" {
		t.Errorf("host microvm inspect = driver=%q runtime=%q platform=%q", ins.Driver, ins.HostConfig.Runtime, ins.Platform)
	}
	rtMap := te.ContainerRuntimes()
	if got := rtMap[cr.ID]; got.Runtime != RuntimeMicroVM || got.Platform != "linux/"+hostImageArch {
		t.Errorf("ContainerRuntimes[host-mvm] = %+v, want microvm + linux/%s", got, hostImageArch)
	}

	// Labels["gvx.runtime"]="microvm" fallback -> 201 microvm
	st, body = te.do(t, http.MethodPost, "/containers/create?name=on-label-mvm", map[string]any{
		"Image":  "native:1",
		"Labels": map[string]string{"gvx.runtime": "microvm"},
	})
	if st != http.StatusCreated {
		t.Fatalf("ON label microvm = %d %s", st, body)
	}
	cr = containerCreateResponse{}
	_ = json.Unmarshal(body, &cr)
	if got := te.ContainerRuntimes()[cr.ID]; got.Runtime != RuntimeMicroVM {
		t.Errorf("label fallback runtime = %+v, want microvm", got)
	}

	// "" + uncached foreign ?platform -> 404 (so Docker CLI auto-pulls!)
	st, body = te.do(t, http.MethodPost, "/containers/create?name=on-uncached-foreign&platform=linux/"+foreign, map[string]any{
		"Image": "not-cached-yet:1",
	})
	if st != http.StatusNotFound {
		t.Fatalf("ON uncached foreign = %d %s, want 404", st, body)
	}

	// "" + cached foreign (?platform=linux/<foreign>) -> 201 microvm
	st, body = te.do(t, http.MethodPost, "/containers/create?name=on-foreign-plat&platform=linux/"+foreign, map[string]any{
		"Image": "dual:1",
	})
	if st != http.StatusCreated {
		t.Fatalf("ON cached foreign platform = %d %s", st, body)
	}
	cr = containerCreateResponse{}
	_ = json.Unmarshal(body, &cr)
	if got := te.ContainerRuntimes()[cr.ID]; got.Runtime != RuntimeMicroVM || got.Platform != "linux/"+foreign {
		t.Errorf("ContainerRuntimes[foreign-plat] = %+v, want microvm + linux/%s", got, foreign)
	}

	// "" + foreign-only cached image (no ?platform) -> 201 microvm
	st, body = te.do(t, http.MethodPost, "/containers/create?name=on-foreign-only", map[string]any{
		"Image": "foreign-only:1",
	})
	if st != http.StatusCreated {
		t.Fatalf("ON foreign-only = %d %s", st, body)
	}
	cr = containerCreateResponse{}
	_ = json.Unmarshal(body, &cr)
	_, ib = te.do(t, http.MethodGet, "/containers/"+cr.ID+"/json", nil)
	ins = containerInspectResponse{}
	_ = json.Unmarshal(ib, &ins)
	if ins.Driver != "gvx-microvm" || ins.HostConfig.Runtime != RuntimeMicroVM {
		t.Errorf("foreign-only inspect = driver=%q runtime=%q", ins.Driver, ins.HostConfig.Runtime)
	}

	// "gvx" + foreign -> 400
	st, body = te.do(t, http.MethodPost, "/containers/create?name=on-gvx-foreign&platform=linux/"+foreign, map[string]any{
		"Image":      "dual:1",
		"HostConfig": map[string]any{"Runtime": "gvx"},
	})
	if st != http.StatusBadRequest || !strings.Contains(string(body), "require the microvm runtime") {
		t.Fatalf("ON gvx+foreign = %d %s, want 400", st, body)
	}
	st, body = te.do(t, http.MethodPost, "/containers/create?name=on-gvx-foreign-only", map[string]any{
		"Image":      "foreign-only:1",
		"HostConfig": map[string]any{"Runtime": "gvx"},
	})
	if st != http.StatusBadRequest || !strings.Contains(string(body), "require the microvm runtime") {
		t.Fatalf("ON gvx+foreign-only = %d %s, want 400", st, body)
	}

	// Unknown runtime while ON -> 400
	st, body = te.do(t, http.MethodPost, "/containers/create?name=on-unknown", map[string]any{
		"Image":      "native:1",
		"HostConfig": map[string]any{"Runtime": "kata"},
	})
	if st != http.StatusBadRequest || !strings.Contains(string(body), "Unknown runtime specified kata") {
		t.Fatalf("ON unknown runtime = %d %s, want 400", st, body)
	}

	// ---- Phase 3: Micro-VM ON with RosettaReady = false ----
	setupFakeMicroVM(t, te, false, nil)

	// arm64 --runtime=microvm works WITHOUT Rosetta!
	st, body = te.do(t, http.MethodPost, "/containers/create?name=no-rosetta-arm64", map[string]any{
		"Image":      "native:1",
		"HostConfig": map[string]any{"Runtime": "microvm"},
	})
	if st != http.StatusCreated {
		t.Fatalf("no-Rosetta arm64 --runtime=microvm = %d %s, want 201", st, body)
	}

	// amd64 is refused with 400 Rosetta message (only on arm64 host where amd64 is foreign,
	// or explicit ?platform=linux/amd64).
	st, body = te.do(t, http.MethodPost, "/containers/create?name=no-rosetta-amd64&platform=linux/amd64", map[string]any{
		"Image": "dual:1",
	})
	if hostImageArch == "arm64" {
		if st != http.StatusBadRequest || !strings.Contains(string(body), "Rosetta") {
			t.Fatalf("no-Rosetta amd64 = %d %s, want 400 Rosetta error", st, body)
		}
	}
}

func TestMicroVMArm64NativeAndAmd64RosettaLifecycle(t *testing.T) {
	te := newTestEngine(t)
	_, armLayers := addCachedMicroVMImage(t, te, "arm64", "alpine:arm64", ImageConfig{Cmd: []string{"/bin/sh"}}, 2)
	_, amdLayers := addCachedMicroVMImage(t, te, "amd64", "alpine:amd64", ImageConfig{Cmd: []string{"/bin/sh"}}, 3)

	// Start with RosettaReady=false to verify arm64 --runtime=microvm runs with Rosetta:false!
	getClients, _ := setupFakeMicroVM(t, te, false, func(fc *fakeMicroVMClient) {
		fc.onStart = func(c *fakeMicroVMClient) {
			_, _ = c.attach.stdoutW.Write([]byte("hello-stdout\n"))
			_, _ = c.attach.stderrW.Write([]byte("hello-stderr\n"))
		}
	})

	// 1. arm64 --runtime=microvm
	st, body := te.do(t, http.MethodPost, "/containers/create?name=arm-mvm&platform=linux/arm64", map[string]any{
		"Image":      "alpine:arm64",
		"OpenStdin":  true,
		"StdinOnce":  true,
		"Tty":        false,
		"HostConfig": map[string]any{"Runtime": "microvm"},
	})
	if st != http.StatusCreated {
		t.Fatalf("create arm-mvm = %d %s", st, body)
	}
	var cr containerCreateResponse
	_ = json.Unmarshal(body, &cr)

	st, body = te.do(t, http.MethodPost, "/containers/"+cr.ID+"/start", nil)
	if st != http.StatusNoContent {
		t.Fatalf("start arm-mvm = %d %s", st, body)
	}

	if !te.CanControl(cr.ID) {
		t.Error("CanControl(arm-mvm) = false, want true")
	}
	if te.RunningCount() != 1 {
		t.Errorf("RunningCount() = %d, want 1", te.RunningCount())
	}
	if mst := te.MicroVMState(); !mst.Enabled || mst.RunningVMs != 1 || mst.Assets != "2" {
		t.Errorf("MicroVMState = %+v, want Enabled=true RunningVMs=1 Assets=2", mst)
	}

	clients := getClients()
	if len(clients) != 1 {
		t.Fatalf("launched %d helpers, want 1", len(clients))
	}
	fc := clients[0]
	fc.mu.Lock()
	spec := fc.vmSpec
	cspec := fc.ctrSpec
	fc.mu.Unlock()
	if spec.Rosetta {
		t.Error("arm64 microvm booted with Rosetta=true, want false")
	}
	if !spec.Network {
		t.Error("arm64 microvm Network=false, want true")
	}
	if spec.CPUs != min(DefaultMicroVMCPUs, runtime.NumCPU()) || spec.MemoryMiB != DefaultMicroVMMemoryMiB {
		t.Errorf("default resources = (%d CPUs, %d MiB), want (%d, %d)",
			spec.CPUs, spec.MemoryMiB, min(DefaultMicroVMCPUs, runtime.NumCPU()), DefaultMicroVMMemoryMiB)
	}
	wantRootfs := filepath.Join(te.dir, microVMDirName, microVMRootfsDirName, cr.ID+".ext4")
	if spec.Disk == nil || spec.Disk.Path != wantRootfs || spec.Disk.Size != defaultMicroVMRootfsSize {
		t.Errorf("Disk = %+v, want %s (%d)", spec.Disk, wantRootfs, defaultMicroVMRootfsSize)
	}
	if len(spec.ExtraDisks) != 2 || spec.ExtraDisks[0].Path != armLayers[0] || spec.ExtraDisks[1].Path != armLayers[1] || !spec.ExtraDisks[0].ReadOnly {
		t.Errorf("fastpath ExtraDisks = %+v, want %v", spec.ExtraDisks, armLayers)
	}
	if len(cspec.Layers) != 2 || cspec.Layers[0].Device != "/dev/vdb" || cspec.Layers[1].Device != "/dev/vdc" {
		t.Errorf("ContainerSpec.Layers = %+v, want /dev/vdb, /dev/vdc", cspec.Layers)
	}

	// Logs receive stdout and stderr.
	deadline := time.Now().Add(2 * time.Second)
	var logsOut string
	for time.Now().Before(deadline) {
		_, lb := te.do(t, http.MethodGet, "/containers/"+cr.ID+"/logs?stdout=1&stderr=1", nil)
		logsOut = string(lb)
		if strings.Contains(logsOut, "hello-stdout") && strings.Contains(logsOut, "hello-stderr") {
			break
		}
		time.Sleep(15 * time.Millisecond)
	}
	if !strings.Contains(logsOut, "hello-stdout") || !strings.Contains(logsOut, "hello-stderr") {
		t.Fatalf("logs missing output: %q", logsOut)
	}

	// Stdin forwarding works via ctrStdin / ctrCloseStdin.
	stdinReadDone := make(chan []byte, 1)
	go func() {
		b, _ := io.ReadAll(fc.attach.stdinR)
		stdinReadDone <- b
	}()
	w := te.ctrStdin(cr.ID)
	if w == nil {
		t.Fatal("ctrStdin returned nil")
	}
	_, _ = w.Write([]byte("ping-stdin"))
	_ = te.ctrCloseStdin(cr.ID)
	if got := string(<-stdinReadDone); got != "ping-stdin" {
		t.Errorf("guest stdin = %q, want ping-stdin", got)
	}

	// Stop -> SIGTERM -> exited, rootfs persists until rm.
	st, body = te.do(t, http.MethodPost, "/containers/"+cr.ID+"/stop?t=2", nil)
	if st != http.StatusNoContent {
		t.Fatalf("stop arm-mvm = %d %s", st, body)
	}
	waitForContainerStatus(t, te, cr.ID, StatusExited, 3*time.Second)
	if te.MicroVMState().RunningVMs != 0 || te.CanControl(cr.ID) {
		t.Errorf("after stop: RunningVMs=%d CanControl=%v", te.MicroVMState().RunningVMs, te.CanControl(cr.ID))
	}
	if _, err := os.Stat(wantRootfs); err != nil {
		t.Errorf("persistent rootfs missing after stop: %v", err)
	}

	// Delete removes persistent rootfs.
	st, body = te.do(t, http.MethodDelete, "/containers/"+cr.ID, nil)
	if st != http.StatusNoContent {
		t.Fatalf("delete arm-mvm = %d %s", st, body)
	}
	if _, err := os.Stat(wantRootfs); !os.IsNotExist(err) {
		t.Errorf("rootfs %s still exists after rm: %v", wantRootfs, err)
	}

	// 2. amd64 auto-Rosetta (enable RosettaReady=true).
	// On arm64 hosts linux/amd64 auto-dispatches to microvm without specifying
	// Runtime; on amd64 hosts request microvm explicitly so the Rosetta boot
	// spec is still exercised.
	getClients, _ = setupFakeMicroVM(t, te, true, nil)
	amdReq := map[string]any{
		"Image": "alpine:amd64",
	}
	if hostImageArch == "amd64" {
		amdReq["HostConfig"] = map[string]any{"Runtime": "microvm"}
	}
	st, body = te.do(t, http.MethodPost, "/containers/create?name=amd-mvm&platform=linux/amd64", amdReq)
	if st != http.StatusCreated {
		t.Fatalf("create amd-mvm = %d %s", st, body)
	}
	cr = containerCreateResponse{}
	_ = json.Unmarshal(body, &cr)
	st, body = te.do(t, http.MethodPost, "/containers/"+cr.ID+"/start", nil)
	if st != http.StatusNoContent {
		t.Fatalf("start amd-mvm = %d %s", st, body)
	}
	clients = getClients()
	if len(clients) != 1 {
		t.Fatalf("launched %d helpers for amd-mvm, want 1", len(clients))
	}
	fc = clients[0]
	fc.mu.Lock()
	amdSpec := fc.vmSpec
	fc.mu.Unlock()
	if !amdSpec.Rosetta {
		t.Error("amd64 microvm booted with Rosetta=false, want true")
	}
	if len(amdSpec.ExtraDisks) != 3 || amdSpec.ExtraDisks[2].Path != amdLayers[2] {
		t.Errorf("amd64 ExtraDisks = %+v, want %v", amdSpec.ExtraDisks, amdLayers)
	}
}

func TestMicroVMStorageOverflowVolumesBindsTmpfsAndFlock(t *testing.T) {
	te := newTestEngine(t)
	_, layerPaths := addCachedMicroVMImage(t, te, hostImageArch, "many-layers:1", ImageConfig{Cmd: []string{"/bin/sh"}}, 10)
	getClients, _ := setupFakeMicroVM(t, te, true, nil)

	// Create host bind dir, single file, and ext4 volume file.
	bindDir := t.TempDir()
	bindFile := filepath.Join(t.TempDir(), "app.conf")
	if err := os.WriteFile(bindFile, []byte("k=v\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	volDir := t.TempDir()
	volPath := filepath.Join(volDir, "mydata.ext4")
	if err := os.WriteFile(volPath, []byte("ext4-vol"), 0o600); err != nil {
		t.Fatal(err)
	}
	te.fake.mu.Lock()
	te.fake.volumes["mydata"] = rungv.Volume{Name: "mydata", Driver: "ext4lite", Mountpoint: volPath}
	te.fake.mu.Unlock()

	st, body := te.do(t, http.MethodPost, "/containers/create?name=storage-mvm", map[string]any{
		"Image": "many-layers:1",
		"HostConfig": map[string]any{
			"Runtime": "microvm",
			"Binds": []string{
				bindDir + ":/app",
				bindFile + ":/etc/app.conf:ro",
				"mydata:/data",
			},
			"Tmpfs": map[string]string{
				"/run/cache": "size=64m",
			},
		},
	})
	if st != http.StatusCreated {
		t.Fatalf("create storage-mvm = %d %s", st, body)
	}
	var cr containerCreateResponse
	_ = json.Unmarshal(body, &cr)

	st, body = te.do(t, http.MethodPost, "/containers/"+cr.ID+"/start", nil)
	if st != http.StatusNoContent {
		t.Fatalf("start storage-mvm = %d %s", st, body)
	}

	fc := getClients()[0]
	fc.mu.Lock()
	vmSpec := fc.vmSpec
	ctrSpec := fc.ctrSpec
	fc.mu.Unlock()

	// Overflow path: 10 layers via virtiofs tag="layers" + File paths (NOT ExtraDisks).
	hasLayersShare := false
	for _, sh := range vmSpec.Shares {
		if sh.Tag == "layers" && sh.ReadOnly {
			hasLayersShare = true
		}
	}
	if !hasLayersShare {
		t.Errorf("overflow layers share missing in Shares: %+v", vmSpec.Shares)
	}
	if len(ctrSpec.Layers) != 10 {
		t.Fatalf("len(Layers) = %d, want 10", len(ctrSpec.Layers))
	}
	for i, ls := range ctrSpec.Layers {
		wantFile := "/mnt/layers/" + filepath.Base(layerPaths[i])
		if ls.Device != "" || ls.File != wantFile {
			t.Errorf("Layers[%d] = %+v, want File=%q", i, ls, wantFile)
		}
	}

	// Named ext4 volume attached at /dev/vdb (first ExtraDisk since layers used overflow!).
	if len(vmSpec.ExtraDisks) != 1 || vmSpec.ExtraDisks[0].ReadOnly {
		t.Errorf("ExtraDisks = %+v, want 1 rw ext4 volume", vmSpec.ExtraDisks)
	}

	// Verify bind mounts (dir + single-file parent isolation), volume, and tmpfs.
	byTarget := map[string]vmproto.ContainerMount{}
	for _, m := range ctrSpec.Mounts {
		byTarget[m.Target] = m
	}
	if m := byTarget["/app"]; m.Kind != "bind" || m.Source != "/mnt/b0" || m.ReadOnly {
		t.Errorf("/app mount = %+v", m)
	}
	if m := byTarget["/etc/app.conf"]; m.Kind != "bind" || m.Source != "/mnt/b1/app.conf" || !m.ReadOnly {
		t.Errorf("/etc/app.conf single-file mount = %+v", m)
	}
	if m := byTarget["/data"]; m.Kind != "volume" || m.Source != "/dev/vdb" || m.ReadOnly {
		t.Errorf("/data volume mount = %+v", m)
	}
	if m := byTarget["/run/cache"]; m.Kind != "tmpfs" || m.Options != "size=64m" {
		t.Errorf("/run/cache tmpfs mount = %+v", m)
	}

	// Volume flock prevents a second container from mounting mydata rw while running (409).
	if runtime.GOOS != "windows" {
		st, body = te.do(t, http.MethodPost, "/containers/create?name=storage-mvm-2", map[string]any{
			"Image": "many-layers:1",
			"HostConfig": map[string]any{
				"Runtime": "microvm",
				"Binds":   []string{"mydata:/data"},
			},
		})
		if st != http.StatusCreated {
			t.Fatalf("create second container = %d %s", st, body)
		}
		var cr2 containerCreateResponse
		_ = json.Unmarshal(body, &cr2)
		st, body = te.do(t, http.MethodPost, "/containers/"+cr2.ID+"/start", nil)
		if st != http.StatusConflict {
			t.Fatalf("concurrent rw volume mount = %d %s, want 409 conflict", st, body)
		}
	}

	// Stopping first container releases the flock so lockVolumeFile succeeds again.
	st, _ = te.do(t, http.MethodPost, "/containers/"+cr.ID+"/stop?t=1", nil)
	if st != http.StatusNoContent {
		t.Fatalf("stop storage-mvm = %d", st)
	}
	unlock, err := lockVolumeFile(volPath, false)
	if err != nil {
		t.Fatalf("lockVolumeFile after stop failed: %v", err)
	}
	unlock()

	// Orphan rootfs sweep on Open.
	orphan := filepath.Join(te.dir, microVMDirName, microVMRootfsDirName, "0000000000000000.ext4")
	if err := os.WriteFile(orphan, []byte("orphan"), 0o600); err != nil {
		t.Fatal(err)
	}
	te.sweepMicroVMRootfs()
	if _, err := os.Stat(orphan); !os.IsNotExist(err) {
		t.Errorf("orphan rootfs %s was not swept", orphan)
	}
	realRootfs := filepath.Join(te.dir, microVMDirName, microVMRootfsDirName, cr.ID+".ext4")
	if _, err := os.Stat(realRootfs); err != nil {
		t.Errorf("active container rootfs %s was mistakenly swept: %v", realRootfs, err)
	}
}

func TestMicroVMResourcesAndAssetMemoization(t *testing.T) {
	te := newTestEngine(t)
	addCachedMicroVMImage(t, te, hostImageArch, "res:1", ImageConfig{Cmd: []string{"/bin/sh"}}, 1)
	getClients, assetCalls := setupFakeMicroVM(t, te, true, nil)

	// Start container 1 with HostConfig overrides.
	pidsLim := int64(64)
	st, body := te.do(t, http.MethodPost, "/containers/create?name=res-1", map[string]any{
		"Image": "res:1",
		"HostConfig": map[string]any{
			"Runtime":   "microvm",
			"NanoCpus":  int64(2_000_000_000),
			"Memory":    int64(512 << 20),
			"PidsLimit": pidsLim,
		},
	})
	if st != http.StatusCreated {
		t.Fatalf("create res-1 = %d %s", st, body)
	}
	var cr1 containerCreateResponse
	_ = json.Unmarshal(body, &cr1)
	if st, b := te.do(t, http.MethodPost, "/containers/"+cr1.ID+"/start", nil); st != http.StatusNoContent {
		t.Fatalf("start res-1 = %d %s", st, b)
	}

	fc1 := getClients()[0]
	fc1.mu.Lock()
	vm1 := fc1.vmSpec
	ctr1 := fc1.ctrSpec
	fc1.mu.Unlock()
	if vm1.CPUs != min(2, runtime.NumCPU()) || vm1.MemoryMiB != 512 {
		t.Errorf("res-1 VM = (%d CPUs, %d MiB), want (%d, 512)", vm1.CPUs, vm1.MemoryMiB, min(2, runtime.NumCPU()))
	}
	if ctr1.MemoryMax != 512<<20 || ctr1.PidsLimit != 64 || ctr1.CPUQuota != 200000 || ctr1.CPUPeriod != 100000 {
		t.Errorf("res-1 cgroup = mem=%d pids=%d quota=%d period=%d", ctr1.MemoryMax, ctr1.PidsLimit, ctr1.CPUQuota, ctr1.CPUPeriod)
	}

	// Update global resource defaults live while res-1 is running: res-1 stays running
	// and cached assets stay memoized!
	if err := te.SetMicroVM(&MicroVMConfig{
		Launcher:     vmclient.Launcher{Path: "/fake/gvx-desktop", Args: []string{vmhelper.Arg, vmhelper.CmdServe}},
		DefaultCPUs:  1,
		DefaultMemMB: 256,
		Assets: func(ctx context.Context, progress func(done, total int64)) (vmassets.Dir, error) {
			assetCalls.Add(1)
			return makeFakeVMAssetsDir(t), nil
		},
	}); err != nil {
		t.Fatal(err)
	}
	if te.MicroVMState().RunningVMs != 1 {
		t.Fatalf("live SetMicroVM stopped running VMs: %+v", te.MicroVMState())
	}

	// Start container 2 with no HostConfig overrides -> picks up updated global defaults & memoized assets!
	st, body = te.do(t, http.MethodPost, "/containers/create?name=res-2", map[string]any{
		"Image":      "res:1",
		"HostConfig": map[string]any{"Runtime": "microvm"},
	})
	if st != http.StatusCreated {
		t.Fatalf("create res-2 = %d %s", st, body)
	}
	var cr2 containerCreateResponse
	_ = json.Unmarshal(body, &cr2)
	if st, b := te.do(t, http.MethodPost, "/containers/"+cr2.ID+"/start", nil); st != http.StatusNoContent {
		t.Fatalf("start res-2 = %d %s", st, b)
	}
	fc2 := getClients()[1]
	fc2.mu.Lock()
	vm2 := fc2.vmSpec
	fc2.mu.Unlock()
	if vm2.CPUs != 1 || vm2.MemoryMiB != 256 {
		t.Errorf("res-2 VM = (%d CPUs, %d MiB), want (1, 256)", vm2.CPUs, vm2.MemoryMiB)
	}
	if got := assetCalls.Load(); got != 1 {
		t.Errorf("Assets resolver called %d times, want 1 (memoized)", got)
	}
}

func TestMicroVMTCPPortForwardingAndWarnings(t *testing.T) {
	te := newTestEngine(t)
	addCachedMicroVMImage(t, te, hostImageArch, "web:1", ImageConfig{
		Cmd:          []string{"/bin/httpd"},
		ExposedPorts: map[string]struct{}{"80/tcp": {}},
	}, 1)
	getClients, _ := setupFakeMicroVM(t, te, true, nil)

	// Create a user-defined network: micro-VM containers join the overlay cleanly with no NAT warning.
	if st, b := te.do(t, http.MethodPost, "/networks/create", map[string]any{"Name": "custom-net"}); st != http.StatusCreated {
		t.Fatalf("network create = %d %s", st, b)
	}

	st, body := te.do(t, http.MethodPost, "/containers/create?name=web-mvm", map[string]any{
		"Image": "web:1",
		"HostConfig": map[string]any{
			"Runtime":     "microvm",
			"NetworkMode": "custom-net",
			"PortBindings": map[string]any{
				"80/tcp": []map[string]string{{"HostIp": "0.0.0.0", "HostPort": ""}},
				"53/udp": []map[string]string{{"HostIp": "127.0.0.1", "HostPort": ""}},
			},
		},
	})
	if st != http.StatusCreated {
		t.Fatalf("create web-mvm = %d %s", st, body)
	}
	var cr containerCreateResponse
	_ = json.Unmarshal(body, &cr)
	joinedWarn := strings.Join(cr.Warnings, " | ")
	if !strings.Contains(joinedWarn, "UDP") {
		t.Errorf("expected UDP warning in %v", cr.Warnings)
	}
	if strings.Contains(joinedWarn, "not yet bridged") {
		t.Errorf("unexpected obsolete user-network warning in %v", cr.Warnings)
	}

	if st, b := te.do(t, http.MethodPost, "/containers/"+cr.ID+"/start", nil); st != http.StatusNoContent {
		t.Fatalf("start web-mvm = %d %s", st, b)
	}

	_, ib := te.do(t, http.MethodGet, "/containers/"+cr.ID+"/json", nil)
	var ins containerInspectResponse
	_ = json.Unmarshal(ib, &ins)
	binds := ins.NetworkSettings.Ports["80/tcp"]
	if len(binds) != 1 || binds[0].HostIP != "127.0.0.1" || binds[0].HostPort == "" {
		t.Fatalf("Ports[80/tcp] = %+v, want 127.0.0.1:<port> (AllowLANPorts off)", binds)
	}

	// Connect to forwarded TCP port and verify roundtrip through client.Dial(tcp, 127.0.0.1:80).
	addr := net.JoinHostPort(binds[0].HostIP, binds[0].HostPort)
	conn, err := net.DialTimeout("tcp", addr, 2*time.Second)
	if err != nil {
		t.Fatalf("dial forwarded port %s: %v", addr, err)
	}
	_, _ = conn.Write([]byte("hello-web"))
	if cw, ok := conn.(interface{ CloseWrite() error }); ok {
		_ = cw.CloseWrite()
	}
	reply, err := io.ReadAll(conn)
	_ = conn.Close()
	if err != nil {
		t.Fatalf("read forwarded reply: %v", err)
	}
	if string(reply) != "guest:hello-web" {
		t.Fatalf("forwarded reply = %q, want guest:hello-web", string(reply))
	}

	fc := getClients()[0]
	fc.mu.Lock()
	dials := slices.Clone(fc.dialReqs)
	fc.mu.Unlock()
	if len(dials) != 1 || dials[0].Network != "tcp" || dials[0].Addr != "127.0.0.1:80" {
		t.Errorf("dialReqs = %+v, want tcp 127.0.0.1:80", dials)
	}

	// Stopping closes the host listener.
	if st, b := te.do(t, http.MethodPost, "/containers/"+cr.ID+"/stop?t=1", nil); st != http.StatusNoContent {
		t.Fatalf("stop web-mvm = %d %s", st, b)
	}
	if c2, err := net.DialTimeout("tcp", addr, 200*time.Millisecond); err == nil {
		_ = c2.Close()
		t.Errorf("forwarded port %s still accepting connections after stop", addr)
	}
}

func TestMicroVMControlExecTopStatsPauseCpDiffExportCommitKillCrash(t *testing.T) {
	te := newTestEngine(t)
	// Deliberately leave te.fake.features.Control == false to verify micro-VM
	// containers always provide full control regardless of rungv version!
	foreign := foreignImageArch()
	addCachedMicroVMImage(t, te, foreign, "ctrl-amd64:1", ImageConfig{Cmd: []string{"/bin/sh"}}, 1)
	getClients, _ := setupFakeMicroVM(t, te, true, nil)

	st, body := te.do(t, http.MethodPost, "/containers/create?name=ctrl-mvm&platform=linux/"+foreign, map[string]any{
		"Image": "ctrl-amd64:1",
		"Tty":   true,
	})
	if st != http.StatusCreated {
		t.Fatalf("create ctrl-mvm = %d %s", st, body)
	}
	var cr containerCreateResponse
	_ = json.Unmarshal(body, &cr)

	if st, b := te.do(t, http.MethodPost, "/containers/"+cr.ID+"/start", nil); st != http.StatusNoContent {
		t.Fatalf("start ctrl-mvm = %d %s", st, b)
	}

	fc := getClients()[0]

	// 1. ExecArgv formats `<exe> __vm-helper exec -socket <sock> ... -- <argv>`
	ctl := te.ctrControl(Container{ID: cr.ID, Runtime: RuntimeMicroVM})
	if ctl == nil {
		t.Fatal("ctrControl returned nil")
	}
	execArgv := ctl.ExecArgv(rungv.ExecOptions{
		Argv:        []string{"/bin/sh", "-l"},
		Interactive: true,
		TTY:         true,
		User:        "1000",
		Workdir:     "/app",
		Env:         []string{"TERM=xterm-256color"},
	})
	wantPrefix := []string{"/fake/gvx-desktop", vmhelper.Arg, vmhelper.CmdExec, "-socket", fc.Socket()}
	if !slices.Equal(execArgv[:len(wantPrefix)], wantPrefix) || !slices.Contains(execArgv, "--") || execArgv[len(execArgv)-2] != "/bin/sh" {
		t.Errorf("ExecArgv = %v", execArgv)
	}

	// 2. Resize
	if st, b := te.do(t, http.MethodPost, "/containers/"+cr.ID+"/resize?h=30&w=120", nil); st != http.StatusOK {
		t.Fatalf("resize = %d %s", st, b)
	}
	fc.mu.Lock()
	resizes := slices.Clone(fc.resizes)
	fc.mu.Unlock()
	if len(resizes) == 0 || resizes[len(resizes)-1] != [2]uint16{30, 120} {
		t.Errorf("resizes = %v, want [30 120]", resizes)
	}

	// 3. Exec create + detached start + inspect
	st, body = te.do(t, http.MethodPost, "/containers/"+cr.ID+"/exec", map[string]any{
		"Cmd":          []string{"uname", "-m"},
		"AttachStdout": true,
		"AttachStderr": true,
	})
	if st != http.StatusCreated {
		t.Fatalf("exec create = %d %s", st, body)
	}
	var exResp struct{ ID string }
	_ = json.Unmarshal(body, &exResp)
	if st, b := te.do(t, http.MethodPost, "/exec/"+exResp.ID+"/start", map[string]any{"Detach": true}); st != http.StatusOK {
		t.Fatalf("exec start detach = %d %s", st, b)
	}
	deadline := time.Now().Add(2 * time.Second)
	var exIns execInspectResponse
	for time.Now().Before(deadline) {
		_, eb := te.do(t, http.MethodGet, "/exec/"+exResp.ID+"/json", nil)
		_ = json.Unmarshal(eb, &exIns)
		if !exIns.Running && exIns.ExitCode != nil {
			break
		}
		time.Sleep(15 * time.Millisecond)
	}
	if exIns.Running || exIns.ExitCode == nil || *exIns.ExitCode != 0 {
		t.Fatalf("exec inspect = %+v, want exited 0", exIns)
	}

	// Missing binary in exec -> exit code 127
	st, body = te.do(t, http.MethodPost, "/containers/"+cr.ID+"/exec", map[string]any{
		"Cmd":          []string{"missing-bin"},
		"AttachStdout": true,
	})
	if st != http.StatusCreated {
		t.Fatalf("exec create missing = %d %s", st, body)
	}
	_ = json.Unmarshal(body, &exResp)
	if st, b := te.do(t, http.MethodPost, "/exec/"+exResp.ID+"/start", map[string]any{"Detach": true}); st != http.StatusOK {
		t.Fatalf("exec start missing = %d %s", st, b)
	}
	deadline = time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		_, eb := te.do(t, http.MethodGet, "/exec/"+exResp.ID+"/json", nil)
		_ = json.Unmarshal(eb, &exIns)
		if !exIns.Running && exIns.ExitCode != nil {
			break
		}
		time.Sleep(15 * time.Millisecond)
	}
	if exIns.ExitCode == nil || *exIns.ExitCode != 127 {
		t.Fatalf("missing-bin exit code = %+v, want 127", exIns)
	}

	// 4. Top
	st, body = te.do(t, http.MethodGet, "/containers/"+cr.ID+"/top", nil)
	if st != http.StatusOK || !strings.Contains(string(body), "/bin/sh") {
		t.Fatalf("top = %d %s", st, body)
	}

	// 5. Stats (one-shot)
	st, body = te.do(t, http.MethodGet, "/containers/"+cr.ID+"/stats?stream=0&one-shot=1", nil)
	if st != http.StatusOK || !strings.Contains(string(body), `"eth0"`) {
		t.Fatalf("stats = %d %s", st, body)
	}

	// 6. Pause / Unpause & conflict checks
	if st, b := te.do(t, http.MethodPost, "/containers/"+cr.ID+"/pause", nil); st != http.StatusNoContent {
		t.Fatalf("pause = %d %s", st, b)
	}
	if st, _ := te.do(t, http.MethodPost, "/containers/"+cr.ID+"/pause", nil); st != http.StatusConflict {
		t.Fatalf("double pause = %d, want 409", st)
	}
	if st, _ := te.do(t, http.MethodPost, "/containers/"+cr.ID+"/exec", map[string]any{"Cmd": []string{"true"}}); st != http.StatusConflict {
		t.Fatalf("exec while paused = %d, want 409", st)
	}
	if st, b := te.do(t, http.MethodPost, "/containers/"+cr.ID+"/unpause", nil); st != http.StatusNoContent {
		t.Fatalf("unpause = %d %s", st, b)
	}
	if st, _ := te.do(t, http.MethodPost, "/containers/"+cr.ID+"/unpause", nil); st != http.StatusConflict {
		t.Fatalf("double unpause = %d, want 409", st)
	}

	// 7. Archive HEAD / GET / PUT + 404 on missing
	req, _ := http.NewRequest(http.MethodHead, te.srv.URL+"/containers/"+cr.ID+"/archive?path=/etc/motd", nil)
	res, err := te.srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = res.Body.Close()
	if res.StatusCode != http.StatusOK || res.Header.Get(archivePathStatHeader) == "" {
		t.Fatalf("archive HEAD = %d hdr=%q", res.StatusCode, res.Header.Get(archivePathStatHeader))
	}
	rawStat, _ := base64.StdEncoding.DecodeString(res.Header.Get(archivePathStatHeader))
	if !strings.Contains(string(rawStat), `"motd"`) {
		t.Errorf("archive stat header = %s", rawStat)
	}
	if st, _ := te.do(t, http.MethodGet, "/containers/"+cr.ID+"/archive?path=/missing", nil); st != http.StatusNotFound {
		t.Fatalf("archive missing = %d, want 404", st)
	}
	st, body = te.do(t, http.MethodGet, "/containers/"+cr.ID+"/archive?path=/etc/motd", nil)
	if st != http.StatusOK {
		t.Fatalf("archive GET = %d %s", st, body)
	}
	tr := tar.NewReader(bytes.NewReader(body))
	hdr, err := tr.Next()
	if err != nil || hdr.Name != "motd" {
		t.Fatalf("archive GET tar = %v %v", hdr, err)
	}
	var putBuf bytes.Buffer
	ptw := tar.NewWriter(&putBuf)
	_ = ptw.WriteHeader(&tar.Header{Name: "in.txt", Typeflag: tar.TypeReg, Mode: 0o644, Size: 2})
	_, _ = ptw.Write([]byte("ok"))
	_ = ptw.Close()
	if st, b := te.do(t, http.MethodPut, "/containers/"+cr.ID+"/archive?path=/tmp", putBuf.Bytes()); st != http.StatusOK {
		t.Fatalf("archive PUT = %d %s", st, b)
	}

	// 8. Diff, Export, and Commit (running)
	st, body = te.do(t, http.MethodGet, "/containers/"+cr.ID+"/changes", nil)
	if st != http.StatusOK || !strings.Contains(string(body), `"/added.txt"`) {
		t.Fatalf("changes running = %d %s", st, body)
	}
	st, body = te.do(t, http.MethodGet, "/containers/"+cr.ID+"/export", nil)
	if st != http.StatusOK || len(body) == 0 {
		t.Fatalf("export running = %d len=%d", st, len(body))
	}
	st, body = te.do(t, http.MethodPost, "/commit?container="+cr.ID+"&repo=committed-amd64&tag=v1", nil)
	if st != http.StatusCreated {
		t.Fatalf("commit running = %d %s", st, body)
	}
	if got := te.fake.recordedArchLoads(); !slices.Contains(got, foreign) {
		t.Errorf("commit of %s container recorded arch loads = %v, want %s", foreign, got, foreign)
	}

	// 9. Kill sends portable signal name
	if st, b := te.do(t, http.MethodPost, "/containers/"+cr.ID+"/kill?signal=SIGUSR1", nil); st != http.StatusNoContent {
		t.Fatalf("kill SIGUSR1 = %d %s", st, b)
	}
	fc.mu.Lock()
	lastSig := fc.signals[len(fc.signals)-1]
	fc.mu.Unlock()
	if lastSig.Signal != "SIGUSR1" {
		t.Errorf("last signal = %+v, want SIGUSR1", lastSig)
	}

	// 10. Helper crash -> container transitions to exited with ExitCode 255
	fc.crash(errors.New("simulated helper SIGKILL"))
	ins := waitForContainerStatus(t, te, cr.ID, StatusExited, 3*time.Second)
	if ins.State.ExitCode != 255 {
		t.Errorf("crashed container ExitCode = %d, want 255", ins.State.ExitCode)
	}

	// 11. Stopped micro-VM container returns 409 on diff, commit, archive GET
	for _, ep := range []struct{ method, path string }{
		{http.MethodGet, "/containers/" + cr.ID + "/changes"},
		{http.MethodPost, "/commit?container=" + cr.ID},
		{http.MethodGet, "/containers/" + cr.ID + "/archive?path=/etc/motd"},
	} {
		st, body = te.do(t, ep.method, ep.path, nil)
		if st != http.StatusConflict || !strings.Contains(string(body), "stopped micro-VM") {
			t.Errorf("stopped %s %s = %d %s, want 409 stopped micro-VM", ep.method, ep.path, st, body)
		}
	}

	// 12. SetMicroVM(nil) gracefully stops running micro-VMs
	if st, b := te.do(t, http.MethodPost, "/containers/"+cr.ID+"/start", nil); st != http.StatusNoContent {
		t.Fatalf("restart ctrl-mvm = %d %s", st, b)
	}
	if te.MicroVMState().RunningVMs != 1 {
		t.Fatalf("RunningVMs = %d, want 1", te.MicroVMState().RunningVMs)
	}
	if err := te.SetMicroVM(nil); err != nil {
		t.Fatalf("SetMicroVM(nil): %v", err)
	}
	waitForContainerStatus(t, te, cr.ID, StatusExited, 3*time.Second)
	if mst := te.MicroVMState(); mst.Enabled || mst.RunningVMs != 0 {
		t.Errorf("after SetMicroVM(nil): %+v", mst)
	}
}

func TestMicroVMAttachAndContainersJSONCompat(t *testing.T) {
	dir := t.TempDir()
	// Write a legacy containers.json without platform/runtime/Runtime fields.
	legacyID := strings.Repeat("a", 64)
	legacy := containerStoreFile{
		Version: storeVersion,
		Containers: []Container{{
			ID:      legacyID,
			Name:    "legacy-gvx",
			Created: time.Now().UTC(),
			Image:   "alpine:3.20",
			ImageID: "sha256:" + strings.Repeat("b", 64),
			Config:  ContainerConfig{Cmd: []string{"/bin/sh"}, Image: "alpine:3.20"},
			State:   ContainerState{Status: StatusExited},
		}},
	}
	raw, _ := json.Marshal(legacy)
	if err := os.WriteFile(filepath.Join(dir, "containers.json"), raw, 0o600); err != nil {
		t.Fatal(err)
	}

	fake := newFakeRunner()
	e := New(Config{Dir: dir, Runner: fake, Version: "0.0.0-test"})
	if err := e.Open(context.Background()); err != nil {
		t.Fatalf("Open with legacy containers.json: %v", err)
	}
	srv := httptest.NewServer(e.Handler())
	te := &testEngine{Engine: e, fake: fake, srv: srv, dir: dir}
	t.Cleanup(func() {
		srv.Close()
		_ = e.Close(time.Second)
	})

	// Legacy record loads cleanly as gvx.
	rts := e.ContainerRuntimes()
	if got := rts[legacyID]; got.Runtime != RuntimeGVX || got.Platform != "" {
		t.Fatalf("legacy ContainerRuntimes = %+v, want gvx / empty platform", got)
	}

	// Test real HTTP hijack attach on a micro-VM container.
	addCachedMicroVMImage(t, te, hostImageArch, "att:1", ImageConfig{Cmd: []string{"/bin/sh"}}, 1)
	setupFakeMicroVM(t, te, true, func(fc *fakeMicroVMClient) {
		fc.onStart = func(c *fakeMicroVMClient) {
			in, _ := io.ReadAll(c.attach.stdinR)
			_, _ = c.attach.stdoutW.Write(append([]byte("got:"), in...))
			c.attach.finish(vmproto.ContainerExit{Code: 0}, nil)
		}
	})

	st, body := te.do(t, http.MethodPost, "/containers/create?name=att-mvm", map[string]any{
		"Image":        "att:1",
		"AttachStdin":  true,
		"AttachStdout": true,
		"OpenStdin":    true,
		"StdinOnce":    true,
		"Tty":          true,
		"HostConfig":   map[string]any{"Runtime": "microvm"},
	})
	if st != http.StatusCreated {
		t.Fatalf("create att-mvm = %d %s", st, body)
	}
	var cr containerCreateResponse
	_ = json.Unmarshal(body, &cr)

	// Attach BEFORE start (standard docker run sequence).
	conn, err := net.DialTimeout("tcp", srv.Listener.Addr().String(), 2*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	_, _ = fmt.Fprintf(conn, "POST /containers/%s/attach?stream=1&stdin=1&stdout=1 HTTP/1.1\r\nHost: docker\r\nConnection: Upgrade\r\nUpgrade: tcp\r\n\r\n", cr.ID)
	br := bufio.NewReader(conn)
	resp, err := http.ReadResponse(br, &http.Request{Method: http.MethodPost})
	if err != nil || resp.StatusCode != http.StatusSwitchingProtocols {
		t.Fatalf("attach upgrade = %v %v", resp, err)
	}

	if st, b := te.do(t, http.MethodPost, "/containers/"+cr.ID+"/start", nil); st != http.StatusNoContent {
		t.Fatalf("start att-mvm = %d %s", st, b)
	}
	_, _ = conn.Write([]byte("hi-attach"))
	if cw, ok := conn.(interface{ CloseWrite() error }); ok {
		_ = cw.CloseWrite()
	}
	out, _ := io.ReadAll(br)
	if string(out) != "got:hi-attach" {
		t.Fatalf("attach output = %q, want got:hi-attach", string(out))
	}

	// Close stops any remaining running micro-VMs and persists Platform/Runtime.
	_ = e.Close(time.Second)

	e2 := New(Config{Dir: dir, Runner: fake, Version: "0.0.0-test"})
	if err := e2.Open(context.Background()); err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer e2.Close(time.Second)
	if got := e2.ContainerRuntimes()[cr.ID]; got.Runtime != RuntimeMicroVM || got.Platform != "linux/"+hostImageArch {
		t.Errorf("reopened ContainerRuntimes = %+v, want microvm + linux/%s", got, hostImageArch)
	}
}

func TestMicroVMInFlightStartAbort(t *testing.T) {
	reserveFreePort := func(t *testing.T) int {
		t.Helper()
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatalf("reserve tcp port: %v", err)
		}
		port := ln.Addr().(*net.TCPAddr).Port
		_ = ln.Close()
		return port
	}

	newInFlightFixture := func(t *testing.T) (te *testEngine, cid string, hostPort int, volPath string, enteredLaunch <-chan struct{}, releaseLaunch chan struct{}, getFC func() *fakeMicroVMClient) {
		t.Helper()
		te = newTestEngine(t)
		addCachedMicroVMImage(t, te, hostImageArch, "inflight:1", ImageConfig{Cmd: []string{"/bin/sh"}}, 1)

		volPath = filepath.Join(t.TempDir(), "inflight.ext4")
		if err := os.WriteFile(volPath, []byte("ext4"), 0o600); err != nil {
			t.Fatal(err)
		}
		te.fake.mu.Lock()
		te.fake.volumes["inflight-vol"] = rungv.Volume{Name: "inflight-vol", Driver: "ext4lite", Mountpoint: volPath}
		te.fake.mu.Unlock()

		entered := make(chan struct{})
		release := make(chan struct{})
		var (
			fcMu sync.Mutex
			fc   *fakeMicroVMClient
		)
		assetsDir := makeFakeVMAssetsDir(t)
		if err := te.SetMicroVM(&MicroVMConfig{
			Assets: func(ctx context.Context, _ func(int64, int64)) (vmassets.Dir, error) {
				return assetsDir, nil
			},
			Launcher:     vmclient.Launcher{Path: "/fake/gvx-desktop", Args: []string{vmhelper.Arg, "serve"}},
			RosettaReady: func() bool { return true },
		}); err != nil {
			t.Fatalf("SetMicroVM: %v", err)
		}
		te.mvm.mu.Lock()
		te.mvm.launchFn = func(ctx context.Context, l vmclient.Launcher) (microVMClient, error) {
			close(entered)
			<-release
			client := newFakeMicroVMClient(l.Dir)
			fcMu.Lock()
			fc = client
			fcMu.Unlock()
			return client, nil
		}
		te.mvm.mu.Unlock()

		hostPort = reserveFreePort(t)
		st, body := te.do(t, http.MethodPost, "/containers/create?name=inflight-ctr", map[string]any{
			"Image": "inflight:1",
			"ExposedPorts": map[string]any{
				"8080/tcp": map[string]any{},
			},
			"HostConfig": map[string]any{
				"Runtime": "microvm",
				"Binds":   []string{"inflight-vol:/data"},
				"PortBindings": map[string][]map[string]string{
					"8080/tcp": {{"HostIP": "127.0.0.1", "HostPort": fmt.Sprintf("%d", hostPort)}},
				},
			},
		})
		if st != http.StatusCreated {
			t.Fatalf("create inflight-ctr = %d %s", st, body)
		}
		var cr containerCreateResponse
		_ = json.Unmarshal(body, &cr)

		return te, cr.ID, hostPort, volPath, entered, release, func() *fakeMicroVMClient {
			fcMu.Lock()
			defer fcMu.Unlock()
			return fc
		}
	}

	assertCleanedUp := func(t *testing.T, te *testEngine, fc *fakeMicroVMClient, hostPort int, volPath string) {
		t.Helper()
		if fc == nil {
			t.Fatal("fake client was not created")
		}
		fc.mu.Lock()
		closed := fc.closed
		fc.mu.Unlock()
		if !closed {
			t.Error("aborted micro-VM client was not closed")
		}
		if got := te.MicroVMState().RunningVMs; got != 0 {
			t.Errorf("RunningVMs = %d, want 0", got)
		}
		te.mvm.mu.Lock()
		nVolRefs := len(te.mvm.volRefs)
		te.mvm.mu.Unlock()
		if nVolRefs != 0 {
			t.Errorf("leaked volRefs = %d, want 0", nVolRefs)
		}
		ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", hostPort))
		if err != nil {
			t.Errorf("host TCP forwarder listener on %d leaked: %v", hostPort, err)
		} else {
			_ = ln.Close()
		}
		unlock, err := lockVolumeFile(volPath, false)
		if err != nil {
			t.Errorf("volume OS flock on %s leaked: %v", volPath, err)
		} else {
			unlock()
		}
	}

	t.Run("SetMicroVM_nil_during_start", func(t *testing.T) {
		te, cid, hostPort, volPath, entered, release, getFC := newInFlightFixture(t)
		type res struct {
			st   int
			body []byte
		}
		startDone := make(chan res, 1)
		go func() {
			st, body := te.do(t, http.MethodPost, "/containers/"+cid+"/start", nil)
			startDone <- res{st, body}
		}()

		<-entered
		if err := te.SetMicroVM(nil); err != nil {
			t.Fatalf("SetMicroVM(nil): %v", err)
		}
		close(release)

		r := <-startDone
		if r.st == http.StatusNoContent {
			t.Fatalf("start succeeded after SetMicroVM(nil): %d %s", r.st, r.body)
		}
		assertCleanedUp(t, te, getFC(), hostPort, volPath)
		if te.MicroVMState().Enabled {
			t.Error("MicroVMState().Enabled = true after SetMicroVM(nil)")
		}
		st, body := te.do(t, http.MethodGet, "/containers/"+cid+"/json", nil)
		if st != http.StatusOK {
			t.Fatalf("inspect = %d %s", st, body)
		}
		var ins containerInspectResponse
		_ = json.Unmarshal(body, &ins)
		if ins.State.Running {
			t.Errorf("container still Running after aborted start: %+v", ins.State)
		}
	})

	t.Run("SetMicroVM_nil_then_reenable_during_start", func(t *testing.T) {
		te, cid, hostPort, volPath, entered, release, getFC := newInFlightFixture(t)
		assetsDir := makeFakeVMAssetsDir(t)
		type res struct {
			st   int
			body []byte
		}
		startDone := make(chan res, 1)
		go func() {
			st, body := te.do(t, http.MethodPost, "/containers/"+cid+"/start", nil)
			startDone <- res{st, body}
		}()

		<-entered
		if err := te.SetMicroVM(nil); err != nil {
			t.Fatalf("SetMicroVM(nil): %v", err)
		}
		if err := te.SetMicroVM(&MicroVMConfig{
			Assets: func(ctx context.Context, _ func(int64, int64)) (vmassets.Dir, error) {
				return assetsDir, nil
			},
			Launcher:     vmclient.Launcher{Path: "/fake/gvx-desktop", Args: []string{vmhelper.Arg, "serve"}},
			RosettaReady: func() bool { return true },
		}); err != nil {
			t.Fatalf("SetMicroVM(re-enable): %v", err)
		}
		close(release)

		r := <-startDone
		if r.st == http.StatusNoContent {
			t.Fatalf("stale start succeeded after disable+reenable: %d %s", r.st, r.body)
		}
		assertCleanedUp(t, te, getFC(), hostPort, volPath)
	})

	t.Run("Engine_Close_during_start", func(t *testing.T) {
		te, cid, hostPort, volPath, entered, release, getFC := newInFlightFixture(t)
		type res struct {
			st   int
			body []byte
		}
		startDone := make(chan res, 1)
		go func() {
			st, body := te.do(t, http.MethodPost, "/containers/"+cid+"/start", nil)
			startDone <- res{st, body}
		}()

		<-entered
		if err := te.Close(time.Second); err != nil {
			t.Fatalf("Close: %v", err)
		}
		close(release)

		r := <-startDone
		if r.st == http.StatusNoContent {
			t.Fatalf("start succeeded after Engine.Close: %d %s", r.st, r.body)
		}
		assertCleanedUp(t, te, getFC(), hostPort, volPath)
	})

	t.Run("force_remove_during_start", func(t *testing.T) {
		te, cid, hostPort, volPath, entered, release, getFC := newInFlightFixture(t)
		type res struct {
			st   int
			body []byte
		}
		startDone := make(chan res, 1)
		go func() {
			st, body := te.do(t, http.MethodPost, "/containers/"+cid+"/start", nil)
			startDone <- res{st, body}
		}()

		<-entered
		rmDone := make(chan res, 1)
		go func() {
			st, body := te.do(t, http.MethodDelete, "/containers/"+cid+"?force=1", nil)
			rmDone <- res{st, body}
		}()

		// Wait until remove has marked StatusRemoving or deleted the container.
		deadline := time.Now().Add(2 * time.Second)
		for time.Now().Before(deadline) {
			te.ctr.mu.Lock()
			c := te.ctr.byID[cid]
			removing := c == nil || c.State.Status == StatusRemoving
			te.ctr.mu.Unlock()
			if removing {
				break
			}
			time.Sleep(5 * time.Millisecond)
		}
		close(release)

		sr := <-startDone
		rr := <-rmDone
		if sr.st == http.StatusNoContent {
			t.Fatalf("start succeeded during force remove: %d %s", sr.st, sr.body)
		}
		if rr.st != http.StatusNoContent {
			t.Fatalf("force remove = %d %s", rr.st, rr.body)
		}
		assertCleanedUp(t, te, getFC(), hostPort, volPath)
		if st, _ := te.do(t, http.MethodGet, "/containers/"+cid+"/json", nil); st != http.StatusNotFound {
			t.Errorf("removed container status = %d, want 404", st)
		}
	})

	t.Run("stop_during_start", func(t *testing.T) {
		te, cid, hostPort, volPath, entered, release, getFC := newInFlightFixture(t)
		type res struct {
			st   int
			body []byte
		}
		startDone := make(chan res, 1)
		go func() {
			st, body := te.do(t, http.MethodPost, "/containers/"+cid+"/start", nil)
			startDone <- res{st, body}
		}()

		<-entered
		stopDone := make(chan res, 1)
		go func() {
			st, body := te.do(t, http.MethodPost, "/containers/"+cid+"/stop?t=1", nil)
			stopDone <- res{st, body}
		}()

		deadline := time.Now().Add(2 * time.Second)
		for time.Now().Before(deadline) {
			te.ctr.mu.Lock()
			c := te.ctr.byID[cid]
			stopped := c != nil && c.State.StoppedByUser
			te.ctr.mu.Unlock()
			if stopped {
				break
			}
			time.Sleep(5 * time.Millisecond)
		}
		close(release)

		sr := <-startDone
		str := <-stopDone
		if sr.st != http.StatusConflict {
			t.Fatalf("start during stop = %d %s, want 409", sr.st, sr.body)
		}
		if str.st != http.StatusNoContent {
			t.Fatalf("stop during start = %d %s, want 204", str.st, str.body)
		}
		assertCleanedUp(t, te, getFC(), hostPort, volPath)
		st, body := te.do(t, http.MethodGet, "/containers/"+cid+"/json", nil)
		if st != http.StatusOK {
			t.Fatalf("inspect = %d %s", st, body)
		}
		var ins containerInspectResponse
		_ = json.Unmarshal(body, &ins)
		if ins.State.Running {
			t.Errorf("container still Running after stop-during-start: %+v", ins.State)
		}
	})

	t.Run("SetMicroVM_nil_during_asset_download", func(t *testing.T) {
		te := newTestEngine(t)
		addCachedMicroVMImage(t, te, hostImageArch, "dl-abort:1", ImageConfig{Cmd: []string{"/bin/sh"}}, 1)

		enteredAssets := make(chan struct{})
		var launchCalled atomic.Bool
		if err := te.SetMicroVM(&MicroVMConfig{
			Assets: func(ctx context.Context, progress func(int64, int64)) (vmassets.Dir, error) {
				progress(10, 100)
				close(enteredAssets)
				<-ctx.Done()
				return vmassets.Dir{}, ctx.Err()
			},
			Launcher:     vmclient.Launcher{Path: "/fake/gvx-desktop", Args: []string{vmhelper.Arg, "serve"}},
			RosettaReady: func() bool { return true },
		}); err != nil {
			t.Fatalf("SetMicroVM: %v", err)
		}
		te.mvm.mu.Lock()
		te.mvm.launchFn = func(ctx context.Context, l vmclient.Launcher) (microVMClient, error) {
			launchCalled.Store(true)
			return newFakeMicroVMClient(l.Dir), nil
		}
		te.mvm.mu.Unlock()

		st, body := te.do(t, http.MethodPost, "/containers/create?name=dl-ctr", map[string]any{
			"Image":      "dl-abort:1",
			"HostConfig": map[string]any{"Runtime": "microvm"},
		})
		if st != http.StatusCreated {
			t.Fatalf("create dl-ctr = %d %s", st, body)
		}
		var cr containerCreateResponse
		_ = json.Unmarshal(body, &cr)

		startDone := make(chan int, 1)
		go func() {
			s, _ := te.do(t, http.MethodPost, "/containers/"+cr.ID+"/start", nil)
			startDone <- s
		}()

		<-enteredAssets
		if err := te.SetMicroVM(nil); err != nil {
			t.Fatalf("SetMicroVM(nil): %v", err)
		}
		if s := <-startDone; s == http.StatusNoContent {
			t.Fatalf("start succeeded after SetMicroVM(nil) during download")
		}
		if launchCalled.Load() {
			t.Error("launchFn was called despite SetMicroVM(nil) during asset download")
		}
		mst := te.MicroVMState()
		if mst.Enabled || mst.Downloading || mst.Err != "" || mst.RunningVMs != 0 {
			t.Errorf("MicroVMState after abort during download = %+v, want clean disabled state", mst)
		}
	})
}

func TestMicroVMOverlayNetworkingAndLiveConnect(t *testing.T) {
	te := newTestEngine(t)
	addCachedMicroVMImage(t, te, hostImageArch, "app:1", ImageConfig{Cmd: []string{"/bin/sh"}}, 1)
	getClients, _ := setupFakeMicroVM(t, te, true, nil)

	if st, b := te.do(t, http.MethodPost, "/networks/create", map[string]any{"Name": "net-a"}); st != http.StatusCreated {
		t.Fatalf("create net-a = %d %s", st, b)
	}
	if st, b := te.do(t, http.MethodPost, "/networks/create", map[string]any{"Name": "net-b"}); st != http.StatusCreated {
		t.Fatalf("create net-b = %d %s", st, b)
	}
	if st, b := te.do(t, http.MethodPost, "/networks/create", map[string]any{"Name": "net-int", "Internal": true}); st != http.StatusCreated {
		t.Fatalf("create net-int = %d %s", st, b)
	}

	createAndStart := func(name, netName string, aliases []string) (string, string, *fakeMicroVMClient) {
		t.Helper()
		body := map[string]any{
			"Image":      "app:1",
			"HostConfig": map[string]any{"Runtime": "microvm", "NetworkMode": netName},
		}
		if len(aliases) > 0 {
			body["NetworkingConfig"] = map[string]any{
				"EndpointsConfig": map[string]any{
					netName: map[string]any{"Aliases": aliases},
				},
			}
		}
		st, b := te.do(t, http.MethodPost, "/containers/create?name="+name, body)
		if st != http.StatusCreated {
			t.Fatalf("create %s = %d %s", name, st, b)
		}
		var cr containerCreateResponse
		_ = json.Unmarshal(b, &cr)
		if len(cr.Warnings) != 0 {
			t.Fatalf("create %s unexpected warnings = %v", name, cr.Warnings)
		}
		prevN := len(getClients())
		if st, b := te.do(t, http.MethodPost, "/containers/"+cr.ID+"/start", nil); st != http.StatusNoContent {
			t.Fatalf("start %s = %d %s", name, st, b)
		}
		clients := getClients()
		if len(clients) != prevN+1 {
			t.Fatalf("expected new fake client for %s", name)
		}
		_, ib := te.do(t, http.MethodGet, "/containers/"+cr.ID+"/json", nil)
		var ins containerInspectResponse
		_ = json.Unmarshal(ib, &ins)
		ip := ins.NetworkSettings.Networks[netName].IPAddress
		if ip == "" {
			t.Fatalf("%s has no IP on %s", name, netName)
		}
		return cr.ID, ip, clients[len(clients)-1]
	}

	idA, ipA, fcA := createAndStart("mvm-api", "net-a", []string{"api"})
	_, ipB, fcB := createAndStart("mvm-db", "net-a", []string{"db"})
	_, _, fcInt := createAndStart("mvm-int", "net-int", []string{"secret"})

	// Verify ContainerNetAttach & ContainerSpec.Network & VMSpec.Network.
	fcA.mu.Lock()
	nsA := fcA.netStream
	cfgA := fcA.netAttachCfg
	vmNetA := fcA.vmSpec.Network
	ctrNetA := fcA.ctrSpec.Network
	fcA.mu.Unlock()
	if nsA == nil || cfgA == nil || !cfgA.DNS || cfgA.Isolated || !vmNetA || ctrNetA == nil || !ctrNetA.DNS {
		t.Fatalf("mvm-api net setup: ns=%v cfg=%+v vmNet=%v ctrNet=%+v", nsA != nil, cfgA, vmNetA, ctrNetA)
	}

	fcInt.mu.Lock()
	nsInt := fcInt.netStream
	cfgInt := fcInt.netAttachCfg
	vmNetInt := fcInt.vmSpec.Network
	fcInt.mu.Unlock()
	if nsInt == nil || cfgInt == nil || !cfgInt.Isolated || vmNetInt {
		t.Fatalf("mvm-int isolated setup: ns=%v cfg=%+v vmNet=%v (want vmNet=false, isolated=true)", nsInt != nil, cfgInt, vmNetInt)
	}

	// 1. Packet forwarding between two micro-VMs via overlay.Fabric.
	a4 := netip.MustParseAddr(ipA).As4()
	b4 := netip.MustParseAddr(ipB).As4()
	pkt := make([]byte, 24)
	pkt[0] = 0x45
	copy(pkt[12:16], a4[:])
	copy(pkt[16:20], b4[:])
	pkt[20] = 0xab

	fcB.mu.Lock()
	nsB := fcB.netStream
	fcB.mu.Unlock()

	nsA.guestToHost <- fakeNetFrame{kind: guestproto.FrameNetPacket, payload: pkt}
	select {
	case got := <-nsB.hostToGuest:
		if got.kind != guestproto.FrameNetPacket || !bytes.Equal(got.payload, pkt) {
			t.Fatalf("nsB received (%d, %x), want packet %x", got.kind, got.payload, pkt)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for packet from mvm-api to mvm-db")
	}

	// 2. DNS resolution over FrameDNSQuery / FrameDNSReply.
	buildDNS := func(name string) []byte {
		b := dnsmessage.NewBuilder(nil, dnsmessage.Header{ID: 123, RecursionDesired: true})
		_ = b.StartQuestions()
		_ = b.Question(dnsmessage.Question{Name: dnsmessage.MustNewName(name + "."), Type: dnsmessage.TypeA, Class: dnsmessage.ClassINET})
		q, _ := b.Finish()
		return q
	}
	sendDNS := func(ns *fakeMicroVMNetStream, seq uint32, name string) dnsmessage.Message {
		t.Helper()
		q := buildDNS(name)
		payload := make([]byte, 5+len(q))
		binary.BigEndian.PutUint32(payload[0:4], seq)
		payload[4] = 0 // udp
		copy(payload[5:], q)
		ns.guestToHost <- fakeNetFrame{kind: guestproto.FrameDNSQuery, payload: payload}
		select {
		case f := <-ns.hostToGuest:
			if f.kind != guestproto.FrameDNSReply || len(f.payload) < 4 {
				t.Fatalf("unexpected frame (%d, len=%d)", f.kind, len(f.payload))
			}
			if gotSeq := binary.BigEndian.Uint32(f.payload[0:4]); gotSeq != seq {
				t.Fatalf("dns reply seq = %d, want %d", gotSeq, seq)
			}
			var m dnsmessage.Message
			if err := m.Unpack(f.payload[4:]); err != nil {
				t.Fatalf("unpack dns reply: %v", err)
			}
			return m
		case <-time.After(2 * time.Second):
			t.Fatalf("timed out waiting for DNS reply for %s", name)
			return dnsmessage.Message{}
		}
	}

	m := sendDNS(nsA, 1, "db")
	if len(m.Answers) != 1 {
		t.Fatalf("mvm-api DNS db answers = %+v", m.Answers)
	}
	if ar, ok := m.Answers[0].Body.(*dnsmessage.AResource); !ok || netip.AddrFrom4(ar.A).String() != ipB {
		t.Fatalf("mvm-api DNS db answer = %+v, want %s", m.Answers[0].Body, ipB)
	}
	// Isolated container refuses external DNS queries.
	mInt := sendDNS(nsInt, 2, "example.com")
	if mInt.RCode != dnsmessage.RCodeRefused {
		t.Fatalf("isolated DNS example.com RCode = %v, want REFUSED", mInt.RCode)
	}

	// 3. Live docker network connect / disconnect updates gvx0 via ContainerNetUpdate.
	if st, b := te.do(t, http.MethodPost, "/networks/net-b/connect", map[string]any{"Container": idA}); st != http.StatusOK {
		t.Fatalf("connect net-b = %d %s", st, b)
	}
	if st, b := te.do(t, http.MethodPost, "/networks/net-a/disconnect", map[string]any{"Container": idA}); st != http.StatusOK {
		t.Fatalf("disconnect net-a = %d %s", st, b)
	}
	fcA.mu.Lock()
	updates := slices.Clone(fcA.netUpdates)
	fcA.mu.Unlock()
	if len(updates) != 2 || len(updates[0].Addresses) != 2 || len(updates[1].Addresses) != 1 {
		t.Fatalf("live netUpdates = %+v, want 2 updates (2 addrs -> 1 addr)", updates)
	}

	// Stopping leaves overlay cleanly.
	if st, b := te.do(t, http.MethodPost, "/containers/"+idA+"/stop?t=1", nil); st != http.StatusNoContent {
		t.Fatalf("stop mvm-api = %d %s", st, b)
	}
	if _, joined := te.fabric().PeerAddrs(idA); joined {
		t.Fatal("mvm-api still in overlay fabric after stop")
	}
}
