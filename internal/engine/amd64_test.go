//go:build !windows

package engine

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ironpark/gvx-desktop-vm/guestproto"
	"github.com/ironpark/gvx-desktop-vm/vmassets"
	"github.com/ironpark/gvx-desktop-vm/vmclient"
	"github.com/ironpark/gvx-desktop-vm/vmproto"
)

type fakeAmd64Client struct {
	mu       sync.Mutex
	spec     vmproto.VMSpec
	done     chan struct{}
	exitErr  error
	closed   bool
	startErr error
	startDel time.Duration
	onClose  func()
}

func newFakeAmd64Client(delay time.Duration) *fakeAmd64Client {
	return &fakeAmd64Client{
		done:     make(chan struct{}),
		startDel: delay,
	}
}

func (c *fakeAmd64Client) Start(ctx context.Context, spec vmproto.VMSpec) (vmproto.Status, error) {
	if c.startDel > 0 {
		select {
		case <-time.After(c.startDel):
		case <-ctx.Done():
			return vmproto.Status{}, ctx.Err()
		}
	}
	c.mu.Lock()
	c.spec = spec
	err := c.startErr
	c.mu.Unlock()
	if err != nil {
		return vmproto.Status{State: vmproto.Failed, Err: err.Error()}, err
	}
	return vmproto.Status{State: vmproto.Running}, nil
}

func (c *fakeAmd64Client) Dial(ctx context.Context, req vmproto.DialRequest) (net.Conn, error) {
	if req.Service != guestproto.ServiceBuildkitd {
		return nil, &vmproto.Error{Code: vmproto.CodeNoService, Message: "unknown service " + req.Service}
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
		_, _ = srv.Write(append([]byte("vm:"), data...))
		if cw, ok := srv.(interface{ CloseWrite() error }); ok {
			_ = cw.CloseWrite()
		}
	}()
	return net.DialTimeout("tcp", ln.Addr().String(), 2*time.Second)
}

func (c *fakeAmd64Client) Done() <-chan struct{} { return c.done }

func (c *fakeAmd64Client) ExitErr() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.exitErr
}

func (c *fakeAmd64Client) Close(timeout time.Duration) error {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return nil
	}
	c.closed = true
	cb := c.onClose
	close(c.done)
	c.mu.Unlock()
	if cb != nil {
		cb()
	}
	return nil
}

func (c *fakeAmd64Client) crash(err error) {
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

func waitForAmd64Status(t *testing.T, e *Engine, want string, timeout time.Duration) Amd64State {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		st := e.Amd64State()
		if st.Status == want {
			return st
		}
		time.Sleep(15 * time.Millisecond)
	}
	st := e.Amd64State()
	t.Fatalf("timed out waiting for Amd64State.Status == %q, got %+v", want, st)
	return st
}

func roundTripUnix(t *testing.T, sock string, payload []byte) []byte {
	t.Helper()
	c, err := net.DialTimeout("unix", sock, 3*time.Second)
	if err != nil {
		t.Fatalf("dial %s: %v", sock, err)
	}
	defer c.Close()
	if _, err := c.Write(payload); err != nil {
		t.Fatalf("write: %v", err)
	}
	if cw, ok := c.(interface{ CloseWrite() error }); ok {
		_ = cw.CloseWrite()
	}
	got, err := io.ReadAll(c)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	return got
}

func TestAmd64LazyStartSingleFlightDiskAndReset(t *testing.T) {
	dir, err := os.MkdirTemp("", "gvxd-a64t-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })

	var launches, closes, assetCalls atomic.Int32
	var lastSpec vmproto.VMSpec
	var specMu sync.Mutex

	e := New(Config{
		Dir:              dir,
		Runner:           newFakeRunner(),
		Version:          "0.0.0-test",
		BuilderSocketDir: dir,
	})
	e.amd.launchFn = func(ctx context.Context, l vmclient.Launcher) (amd64VMClient, error) {
		launches.Add(1)
		fc := newFakeAmd64Client(50 * time.Millisecond)
		fc.onClose = func() {
			specMu.Lock()
			lastSpec = fc.spec
			specMu.Unlock()
			closes.Add(1)
		}
		return fc, nil
	}

	if err := e.Open(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = e.Close(time.Second) })

	// Before SetAmd64, no front socket exists and Enabled is false.
	if st := e.Amd64State(); st.Enabled || st.Status != BuilderIdle {
		t.Fatalf("initial Amd64State = %+v, want disabled idle", st)
	}

	diskPath := filepath.Join(dir, amd64DirName, amd64DiskName)
	cfg := &Amd64Config{
		IdleTimeout:      5 * time.Second,
		ProbeIdleTimeout: 5 * time.Second,
		Assets: func(ctx context.Context, progress func(done, total int64)) (vmassets.Dir, error) {
			assetCalls.Add(1)
			progress(50, 100)
			progress(100, 100)
			return vmassets.Dir{
				Path:     filepath.Join(dir, "assets"),
				Manifest: vmassets.Manifest{Version: "1"},
				Override: true,
			}, nil
		},
	}
	if err := e.SetAmd64(cfg); err != nil {
		t.Fatalf("SetAmd64: %v", err)
	}

	sock := e.Amd64Socket()
	fi, err := os.Lstat(sock)
	if err != nil {
		t.Fatalf("lstat amd64 socket: %v", err)
	}
	if fi.Mode()&os.ModeSocket == 0 || fi.Mode().Perm() != 0o600 {
		t.Fatalf("amd64 socket mode = %v, want 0600 socket", fi.Mode())
	}
	if launches.Load() != 0 {
		t.Fatalf("helper launched eagerly on SetAmd64: %d", launches.Load())
	}

	// 8 concurrent dials launch the helper and resolve assets exactly once.
	const n = 8
	var wg sync.WaitGroup
	errs := make(chan error, n)
	for i := range n {
		wg.Go(func() {
			payload := []byte("req-" + string(rune('a'+i)))
			c, err := net.DialTimeout("unix", sock, 3*time.Second)
			if err != nil {
				errs <- err
				return
			}
			defer c.Close()
			if _, err := c.Write(payload); err != nil {
				errs <- err
				return
			}
			if cw, ok := c.(interface{ CloseWrite() error }); ok {
				_ = cw.CloseWrite()
			}
			got, err := io.ReadAll(c)
			if err != nil {
				errs <- err
				return
			}
			want := append([]byte("vm:"), payload...)
			if !bytes.Equal(got, want) {
				errs <- errors.New("mismatch: " + string(got))
			}
		})
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("concurrent amd64 dial error: %v", err)
	}

	if launches.Load() != 1 {
		t.Fatalf("helper launches = %d, want 1", launches.Load())
	}
	if assetCalls.Load() != 1 {
		t.Fatalf("asset resolves = %d, want 1", assetCalls.Load())
	}
	st := e.Amd64State()
	if !st.Enabled || st.Status != BuilderRunning || st.Assets != "local" || st.Downloading {
		t.Fatalf("running Amd64State = %+v", st)
	}

	// Simulate a disk file and test DiskBytes + ResetAmd64Disk.
	if err := os.WriteFile(diskPath, make([]byte, 8192), 0o600); err != nil {
		t.Fatal(err)
	}
	if st := e.Amd64State(); st.DiskBytes <= 0 {
		t.Errorf("DiskBytes = %d, want > 0", st.DiskBytes)
	}
	if err := e.ResetAmd64Disk(context.Background()); err != nil {
		t.Fatalf("ResetAmd64Disk: %v", err)
	}
	if closes.Load() != 1 {
		t.Fatalf("ResetAmd64Disk did not stop helper: closes=%d", closes.Load())
	}
	if _, err := os.Stat(diskPath); !os.IsNotExist(err) {
		t.Fatalf("disk still exists after ResetAmd64Disk: %v", err)
	}
	specMu.Lock()
	gotSpec := lastSpec
	specMu.Unlock()
	if !gotSpec.Rosetta || !gotSpec.Network || gotSpec.Disk == nil || gotSpec.Disk.Path != diskPath {
		t.Errorf("unexpected VMSpec: %+v", gotSpec)
	}

	// SetAmd64(nil) removes socket and marks disabled.
	if err := e.SetAmd64(nil); err != nil {
		t.Fatalf("SetAmd64(nil): %v", err)
	}
	if _, err := os.Lstat(sock); !os.IsNotExist(err) {
		t.Errorf("amd64 socket still exists after SetAmd64(nil): %v", err)
	}
	if st := e.Amd64State(); st.Enabled {
		t.Errorf("Amd64State still Enabled after SetAmd64(nil): %+v", st)
	}
}

func TestAmd64ProbeIdleVsBuildIdle(t *testing.T) {
	dir, err := os.MkdirTemp("", "gvxd-a64i-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })

	var launches atomic.Int32
	e := New(Config{
		Dir:              dir,
		Runner:           newFakeRunner(),
		Version:          "0.0.0-test",
		BuilderSocketDir: dir,
		Amd64: &Amd64Config{
			ProbeIdleTimeout: 100 * time.Millisecond,
			IdleTimeout:      450 * time.Millisecond,
			Assets: func(ctx context.Context, _ func(done, total int64)) (vmassets.Dir, error) {
				return vmassets.Dir{Path: dir, Manifest: vmassets.Manifest{Version: "1"}}, nil
			},
		},
	})
	e.amd.launchFn = func(ctx context.Context, l vmclient.Launcher) (amd64VMClient, error) {
		launches.Add(1)
		return newFakeAmd64Client(0), nil
	}

	if err := e.Open(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = e.Close(time.Second) })

	sock := e.Amd64Socket()

	// 1. Small probe (< 64 KiB) stops quickly after ProbeIdleTimeout (100ms).
	got := roundTripUnix(t, sock, []byte("probe"))
	if string(got) != "vm:probe" {
		t.Fatalf("probe reply = %q", got)
	}
	waitForAmd64Status(t, e, BuilderIdle, 350*time.Millisecond)
	if launches.Load() != 1 {
		t.Fatalf("launches = %d, want 1", launches.Load())
	}

	// 2. Real build traffic (>= 64 KiB) stays running past ProbeIdleTimeout (100ms)
	// and stops after IdleTimeout (450ms).
	big := bytes.Repeat([]byte("x"), amd64ProbeBytes)
	got = roundTripUnix(t, sock, big)
	if len(got) != len(big)+3 {
		t.Fatalf("big reply len = %d, want %d", len(got), len(big)+3)
	}
	time.Sleep(220 * time.Millisecond)
	if st := e.Amd64State(); st.Status != BuilderRunning {
		t.Fatalf("VM stopped too early after build traffic (probe idle instead of build idle): %+v", st)
	}
	waitForAmd64Status(t, e, BuilderIdle, 2*time.Second)
	if launches.Load() != 2 {
		t.Fatalf("launches after restart = %d, want 2", launches.Load())
	}
}

func TestAmd64HelperCrashAndRestart(t *testing.T) {
	dir, err := os.MkdirTemp("", "gvxd-a64c-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })

	var mu sync.Mutex
	var clients []*fakeAmd64Client

	e := New(Config{
		Dir:              dir,
		Runner:           newFakeRunner(),
		Version:          "0.0.0-test",
		BuilderSocketDir: dir,
		Amd64: &Amd64Config{
			IdleTimeout:      10 * time.Second,
			ProbeIdleTimeout: 10 * time.Second,
			Assets: func(ctx context.Context, _ func(done, total int64)) (vmassets.Dir, error) {
				return vmassets.Dir{Path: dir, Manifest: vmassets.Manifest{Version: "1"}}, nil
			},
		},
	})
	e.amd.launchFn = func(ctx context.Context, l vmclient.Launcher) (amd64VMClient, error) {
		fc := newFakeAmd64Client(0)
		mu.Lock()
		clients = append(clients, fc)
		mu.Unlock()
		return fc, nil
	}

	if err := e.Open(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = e.Close(time.Second) })

	sock := e.Amd64Socket()
	if got := roundTripUnix(t, sock, []byte("first")); string(got) != "vm:first" {
		t.Fatalf("first reply = %q", got)
	}

	// Simulate helper process crash.
	mu.Lock()
	firstClient := clients[0]
	mu.Unlock()
	firstClient.crash(errors.New("signal: killed"))

	st := waitForAmd64Status(t, e, BuilderIdle, 2*time.Second)
	if !strings.Contains(st.Err, "killed") {
		t.Fatalf("state after crash = %+v, want Err containing killed", st)
	}

	// Next connection starts a new helper and clears Err.
	if got := roundTripUnix(t, sock, []byte("second")); string(got) != "vm:second" {
		t.Fatalf("second reply after crash = %q", got)
	}
	st = e.Amd64State()
	if st.Status != BuilderRunning || st.Err != "" {
		t.Fatalf("state after recovery = %+v, want running with empty Err", st)
	}
	mu.Lock()
	total := len(clients)
	mu.Unlock()
	if total != 2 {
		t.Fatalf("expected 2 helper launches, got %d", total)
	}
}

// Regression test for review item 1: ResetAmd64Disk during an in-flight start
// must cancel the start via its per-start context, wait for runAmd64Start to
// finish and close any helper client it created (never clearing starting
// externally or allowing a concurrent second start), re-check ctx before
// deleting disk.img, and leave the builder healthy for the next build.
func TestAmd64ResetDiskDuringStartNoOrphanAndCtxReCheck(t *testing.T) {
	dir, err := os.MkdirTemp("", "gvxd-a64rst-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })

	diskPath := filepath.Join(dir, amd64DirName, amd64DiskName)
	if err := os.MkdirAll(filepath.Dir(diskPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(diskPath, make([]byte, 4096), 0o600); err != nil {
		t.Fatal(err)
	}

	var (
		launches      atomic.Int32
		activeHelpers atomic.Int32
		maxConcurrent atomic.Int32
		firstEntered  = make(chan struct{})
		firstDone     = make(chan struct{})
	)

	e := New(Config{
		Dir:              dir,
		Runner:           newFakeRunner(),
		Version:          "0.0.0-test",
		BuilderSocketDir: dir,
		Amd64: &Amd64Config{
			IdleTimeout:      10 * time.Second,
			ProbeIdleTimeout: 10 * time.Second,
			Assets: func(ctx context.Context, _ func(done, total int64)) (vmassets.Dir, error) {
				return vmassets.Dir{Path: dir, Manifest: vmassets.Manifest{Version: "1"}}, nil
			},
		},
	})
	e.amd.launchFn = func(ctx context.Context, l vmclient.Launcher) (amd64VMClient, error) {
		n := launches.Add(1)
		cur := activeHelpers.Add(1)
		for {
			prev := maxConcurrent.Load()
			if cur <= prev || maxConcurrent.CompareAndSwap(prev, cur) {
				break
			}
		}
		fc := newFakeAmd64Client(0)
		fc.onClose = func() {
			activeHelpers.Add(-1)
		}
		if n == 1 {
			// Slow first VM boot that blocks in Start until cancelled.
			return &blockingStartClient{
				fakeAmd64Client: fc,
				entered:         firstEntered,
				released:        firstDone,
			}, nil
		}
		return fc, nil
	}

	if err := e.Open(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = e.Close(time.Second) })

	sock := e.Amd64Socket()

	// Kick off a slow start in the background.
	dialDone := make(chan struct{})
	go func() {
		defer close(dialDone)
		c, err := net.DialTimeout("unix", sock, 2*time.Second)
		if err != nil {
			return
		}
		defer c.Close()
		_, _ = c.Write([]byte("slow"))
		_, _ = io.ReadAll(c)
	}()

	<-firstEntered

	// 1. A ResetAmd64Disk whose ctx expires while start is still blocked must NOT delete disk.img.
	shortCtx, shortCancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	err = e.ResetAmd64Disk(shortCtx)
	shortCancel()
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("ResetAmd64Disk with expired ctx = %v, want DeadlineExceeded", err)
	}
	if _, statErr := os.Stat(diskPath); statErr != nil {
		t.Fatalf("disk.img was deleted even though ResetAmd64Disk ctx expired: %v", statErr)
	}

	// Unblock the first start (which saw startCtx cancelled) and run a real ResetAmd64Disk.
	close(firstDone)
	if err := e.ResetAmd64Disk(context.Background()); err != nil {
		t.Fatalf("ResetAmd64Disk: %v", err)
	}
	<-dialDone

	if _, statErr := os.Stat(diskPath); !os.IsNotExist(statErr) {
		t.Fatalf("disk.img still exists after ResetAmd64Disk: %v", statErr)
	}
	if got := activeHelpers.Load(); got != 0 {
		t.Fatalf("orphaned helper after ResetAmd64Disk: activeHelpers=%d, want 0", got)
	}
	if got := maxConcurrent.Load(); got != 1 {
		t.Fatalf("concurrent helpers exceeded 1: maxConcurrent=%d", got)
	}

	// Next build starts a clean helper and succeeds.
	if got := roundTripUnix(t, sock, []byte("after-reset")); string(got) != "vm:after-reset" {
		t.Fatalf("reply after reset = %q, want vm:after-reset", got)
	}
	if got := activeHelpers.Load(); got != 1 {
		t.Fatalf("activeHelpers after recovery = %d, want 1", got)
	}
}

type blockingStartClient struct {
	*fakeAmd64Client
	entered  chan struct{}
	released chan struct{}
}

func (b *blockingStartClient) Start(ctx context.Context, spec vmproto.VMSpec) (vmproto.Status, error) {
	close(b.entered)
	<-b.released
	if err := ctx.Err(); err != nil {
		return vmproto.Status{}, err
	}
	return vmproto.Status{State: vmproto.Running}, nil
}
