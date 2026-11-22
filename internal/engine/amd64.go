package engine

// Owner: workstream C (engine and buildx integration). The seam signatures
// below (SetAmd64, Amd64Socket, Amd64State, ResetAmd64Disk, openAmd64,
// closeAmd64) are frozen; their bodies are C's. See
// docs/design/amd64-builder.md, "Engine side".

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ironpark/gvx-desktop-vm/guestproto"
	"github.com/ironpark/gvx-desktop-vm/vmassets"
	"github.com/ironpark/gvx-desktop-vm/vmclient"
	"github.com/ironpark/gvx-desktop-vm/vmhelper"
	"github.com/ironpark/gvx-desktop-vm/vmproto"
)

// amd64VMClient is the subset of *vmclient.Client that amd64State drives.
// Unit tests replace launchFn to return a fake implementation.
type amd64VMClient interface {
	Start(ctx context.Context, spec vmproto.VMSpec) (vmproto.Status, error)
	Dial(ctx context.Context, req vmproto.DialRequest) (net.Conn, error)
	Done() <-chan struct{}
	ExitErr() error
	Close(timeout time.Duration) error
}

// amd64State owns the amd64 node's lazy front socket, the helper, and the
// VM's idle timers. The same lock order as builderState: startStopMu -> mu.
// Never call e.notify() holding either.
type amd64State struct {
	startStopMu sync.Mutex

	mu            sync.Mutex
	cfg           *Amd64Config
	engineOpen    bool
	opened        bool // front socket currently bound
	closed        bool
	ctx           context.Context
	cancel        context.CancelFunc
	frontSock     string
	tempSockDir   string
	helperDir     string
	tempHelperDir string
	ln            net.Listener
	tracked       map[net.Conn]struct{}

	client      amd64VMClient
	assets      *vmassets.Dir
	assetsLabel string
	downloading bool
	dlDone      int64
	dlTotal     int64

	// starting, startCh, startCtx, and startCancel belong to the single
	// in-flight runAmd64Start goroutine. Only runAmd64Start clears starting.
	starting    bool
	running     bool
	stopping    bool
	startCh     chan struct{}
	startCtx    context.Context
	startCancel context.CancelFunc
	startErr    error
	lastErr     string
	active      int
	runGen      uint64
	runBytes    int64
	idleTimer   *time.Timer

	// launchFn overrides Launcher.Launch in unit tests.
	launchFn func(ctx context.Context, l vmclient.Launcher) (amd64VMClient, error)
}

func cloneAmd64Config(cfg *Amd64Config) *Amd64Config {
	if cfg == nil {
		return nil
	}
	cp := *cfg
	if cfg.Launcher.Args != nil {
		cp.Launcher.Args = append([]string(nil), cfg.Launcher.Args...)
	}
	return &cp
}

func (e *Engine) isAmd64Enabled() bool {
	e.amd.mu.Lock()
	enabled := e.amd.cfg != nil
	e.amd.mu.Unlock()
	return enabled
}

func (e *Engine) amd64DiskPathLocked() string {
	if e.amd.cfg != nil {
		if p := strings.TrimSpace(e.amd.cfg.DiskPath); p != "" {
			return p
		}
	}
	if e.cfg.Dir != "" {
		return filepath.Join(e.cfg.Dir, amd64DirName, amd64DiskName)
	}
	return ""
}

func (e *Engine) amd64DiskSizeLocked() int64 {
	if e.amd.cfg != nil && e.amd.cfg.DiskSize > 0 {
		return e.amd.cfg.DiskSize
	}
	return defaultAmd64DiskSize
}

func (e *Engine) amd64IdleTimeoutLocked() time.Duration {
	if e.amd.cfg != nil && e.amd.cfg.IdleTimeout > 0 {
		return e.amd.cfg.IdleTimeout
	}
	return defaultAmd64IdleTimeout
}

func (e *Engine) amd64ProbeIdleTimeoutLocked() time.Duration {
	if e.amd.cfg != nil && e.amd.cfg.ProbeIdleTimeout > 0 {
		return e.amd.cfg.ProbeIdleTimeout
	}
	return defaultAmd64ProbeIdle
}

// SetAmd64 turns the amd64 builder on with cfg, or off with nil. On, while
// the engine is open, it binds the node's front socket (and does nothing
// more until a build connects); off closes the socket, stops the VM and
// the helper. Calling it before Open just records cfg, which Open uses.
func (e *Engine) SetAmd64(cfg *Amd64Config) error {
	cp := cloneAmd64Config(cfg)

	e.amd.mu.Lock()
	engineOpen := e.amd.engineOpen
	wasOpened := e.amd.opened
	if !engineOpen {
		e.amd.cfg = cp
		e.amd.assets = nil
		e.amd.assetsLabel = ""
		e.amd.lastErr = ""
		e.amd.mu.Unlock()
		if cp != nil {
			e.markForeignImagesPossible()
		}
		return nil
	}
	e.amd.mu.Unlock()

	if wasOpened {
		e.teardownAmd64(defaultAmd64HelperStopGrace)
	}

	e.amd.startStopMu.Lock()
	e.amd.mu.Lock()
	if !e.amd.engineOpen {
		e.amd.cfg = cp
		e.amd.assets = nil
		e.amd.assetsLabel = ""
		e.amd.mu.Unlock()
		e.amd.startStopMu.Unlock()
		return nil
	}
	e.amd.cfg = cp
	e.amd.assets = nil
	e.amd.assetsLabel = ""
	e.amd.lastErr = ""
	e.amd.downloading = false
	e.amd.dlDone = 0
	e.amd.dlTotal = 0
	e.amd.mu.Unlock()

	var err error
	if cp != nil {
		e.markForeignImagesPossible()
		err = e.bindAmd64Locked()
	}
	e.amd.startStopMu.Unlock()

	e.notify()
	return err
}

// Amd64Socket is the amd64 node's front socket, the endpoint of the buildx
// node Amd64NodeName, or "" when the engine has no socket directory.
func (e *Engine) Amd64Socket() string {
	e.amd.mu.Lock()
	if e.amd.frontSock != "" {
		s := e.amd.frontSock
		e.amd.mu.Unlock()
		return s
	}
	e.amd.mu.Unlock()

	if e.cfg.BuilderSocketDir != "" {
		return filepath.Join(e.cfg.BuilderSocketDir, amd64SocketName)
	}
	e.bld.mu.Lock()
	bldDir := e.bld.sockDir
	e.bld.mu.Unlock()
	if bldDir != "" {
		return filepath.Join(bldDir, amd64SocketName)
	}
	if e.cfg.Dir != "" {
		return filepath.Join(e.cfg.Dir, amd64SocketName)
	}
	return ""
}

// Amd64State reports the amd64 builder's state.
func (e *Engine) Amd64State() Amd64State {
	e.amd.mu.Lock()
	st := Amd64State{
		Enabled:     e.amd.cfg != nil,
		Downloading: e.amd.downloading,
		Done:        e.amd.dlDone,
		Total:       e.amd.dlTotal,
		Assets:      e.amd.assetsLabel,
	}
	switch {
	case e.amd.stopping:
		st.Status = Amd64Stopping
	case e.amd.starting:
		st.Status = BuilderStarting
	case e.amd.running:
		st.Status = BuilderRunning
	default:
		st.Status = BuilderIdle
		st.Err = e.amd.lastErr
	}
	diskPath := e.amd64DiskPathLocked()
	e.amd.mu.Unlock()

	if diskPath != "" {
		st.DiskBytes = allocatedFileBytes(diskPath)
	}
	return st
}

// ResetAmd64Disk stops the VM if it runs and deletes the persistent disk,
// buildkit's whole cache with it; the next build starts on a new one.
func (e *Engine) ResetAmd64Disk(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	timeout := defaultAmd64HelperStopGrace
	if dl, ok := ctx.Deadline(); ok {
		if rem := time.Until(dl); rem > 0 && rem < timeout {
			timeout = rem
		}
	}

	// Cancel any in-flight start via its per-start context, close active
	// connections, and wait for runAmd64Start to finish before taking
	// startStopMu. Only runAmd64Start clears e.amd.starting.
	for {
		e.amd.mu.Lock()
		startCh := e.amd.startCh
		startCancel := e.amd.startCancel
		var conns []net.Conn
		for c := range e.amd.tracked {
			conns = append(conns, c)
		}
		clear(e.amd.tracked)
		if e.amd.idleTimer != nil {
			e.amd.idleTimer.Stop()
			e.amd.idleTimer = nil
		}
		e.amd.mu.Unlock()

		if startCancel != nil {
			startCancel()
		}
		for _, c := range conns {
			_ = c.Close()
		}
		if startCh == nil {
			break
		}
		select {
		case <-startCh:
		case <-ctx.Done():
			return ctx.Err()
		}
	}

	if err := ctx.Err(); err != nil {
		return err
	}

	e.amd.startStopMu.Lock()
	e.amd.mu.Lock()
	if err := ctx.Err(); err != nil {
		e.amd.mu.Unlock()
		e.amd.startStopMu.Unlock()
		return err
	}
	// If another caller queued a start while we waited for startStopMu, cancel
	// its context now so it aborts as soon as we release startStopMu.
	if e.amd.startCancel != nil {
		e.amd.startCancel()
	}
	client := e.amd.client
	e.amd.client = nil
	e.amd.running = false
	if client != nil {
		e.amd.stopping = true
	}
	diskPath := e.amd64DiskPathLocked()
	e.amd.mu.Unlock()

	if client != nil {
		go e.notify()
		_ = client.Close(timeout)
		e.amd.mu.Lock()
		e.amd.stopping = false
		e.amd.mu.Unlock()
	}

	var rmErr error
	if err := ctx.Err(); err != nil {
		rmErr = err
	} else if diskPath != "" {
		if err := os.Remove(diskPath); err != nil && !errors.Is(err, os.ErrNotExist) {
			rmErr = err
		}
	}
	e.amd.startStopMu.Unlock()

	e.notify()
	return rmErr
}

// openAmd64 is Open's: it binds the front socket when a config is set.
func (e *Engine) openAmd64() error {
	e.amd.startStopMu.Lock()
	defer e.amd.startStopMu.Unlock()

	e.amd.mu.Lock()
	e.amd.engineOpen = true
	hasCfg := e.amd.cfg != nil
	e.amd.mu.Unlock()

	if !hasCfg {
		return nil
	}
	e.markForeignImagesPossible()
	return e.bindAmd64Locked()
}

// closeAmd64 is Close's: it closes the socket and stops the VM and the
// helper, waiting up to timeout for a graceful stop.
func (e *Engine) closeAmd64(timeout time.Duration) {
	e.amd.mu.Lock()
	e.amd.engineOpen = false
	e.amd.mu.Unlock()
	e.teardownAmd64(timeout)
}

// bindAmd64Locked binds the amd64 front Unix socket and starts accepting
// connections without launching the helper or VM yet. Caller holds startStopMu.
func (e *Engine) bindAmd64Locked() error {
	dir := strings.TrimSpace(e.cfg.BuilderSocketDir)
	tempDir := ""
	if dir == "" {
		e.bld.mu.Lock()
		bldDir := e.bld.sockDir
		e.bld.mu.Unlock()
		if bldDir != "" && len(filepath.Join(bldDir, amd64SocketName)) <= builderMaxUnixSockPath {
			dir = bldDir
		} else {
			dir = strings.TrimSpace(e.cfg.Dir)
			if dir != "" && len(filepath.Join(dir, amd64SocketName)) > builderMaxUnixSockPath {
				short, err := os.MkdirTemp("", "gvxd-a64-")
				if err != nil {
					return err
				}
				dir = short
				tempDir = short
			}
		}
	}
	if dir == "" {
		return nil
	}

	frontSock := filepath.Join(dir, amd64SocketName)
	ln, err := listenBuilderUnix(frontSock)
	if err != nil {
		if tempDir != "" {
			_ = os.RemoveAll(tempDir)
		}
		return fmt.Errorf("listen amd64 builder socket: %w", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	e.amd.mu.Lock()
	e.amd.opened = true
	e.amd.closed = false
	e.amd.ctx = ctx
	e.amd.cancel = cancel
	e.amd.frontSock = frontSock
	e.amd.tempSockDir = tempDir
	e.amd.ln = ln
	e.amd.tracked = map[net.Conn]struct{}{}
	e.amd.running = false
	e.amd.stopping = false
	e.amd.startErr = nil
	e.amd.active = 0
	e.amd.runBytes = 0
	e.amd.mu.Unlock()

	go e.acceptAmd64(ln)
	return nil
}

// teardownAmd64 closes the front socket and tracked connections, cancels any
// in-flight start and waits for it, and stops the running helper/VM. Caller
// must NOT hold startStopMu or mu.
func (e *Engine) teardownAmd64(timeout time.Duration) {
	e.amd.mu.Lock()
	if !e.amd.opened && e.amd.client == nil && !e.amd.starting {
		e.amd.mu.Unlock()
		return
	}
	e.amd.opened = false
	e.amd.closed = true
	cancel := e.amd.cancel
	e.amd.cancel = nil
	startCancel := e.amd.startCancel
	startCh := e.amd.startCh
	if e.amd.idleTimer != nil {
		e.amd.idleTimer.Stop()
		e.amd.idleTimer = nil
	}
	ln := e.amd.ln
	e.amd.ln = nil
	frontSock := e.amd.frontSock
	e.amd.frontSock = ""
	tempSockDir := e.amd.tempSockDir
	e.amd.tempSockDir = ""
	conns := make([]net.Conn, 0, len(e.amd.tracked))
	for c := range e.amd.tracked {
		conns = append(conns, c)
	}
	clear(e.amd.tracked)
	e.amd.mu.Unlock()

	if startCancel != nil {
		startCancel()
	}
	if cancel != nil {
		cancel()
	}
	if ln != nil {
		_ = ln.Close()
	}
	if frontSock != "" {
		_ = os.Remove(frontSock)
	}
	for _, c := range conns {
		_ = c.Close()
	}
	// Wait for any in-flight runAmd64Start to exit cleanly before taking
	// startStopMu so starting is cleared only by runAmd64Start.
	if startCh != nil {
		<-startCh
	}
	if timeout <= 0 {
		timeout = defaultAmd64HelperStopGrace
	}

	e.amd.startStopMu.Lock()
	e.amd.mu.Lock()
	client := e.amd.client
	e.amd.client = nil
	e.amd.running = false
	e.amd.downloading = false
	e.amd.assets = nil
	if client != nil {
		e.amd.stopping = true
	}
	tempHelperDir := e.amd.tempHelperDir
	e.amd.tempHelperDir = ""
	e.amd.helperDir = ""
	e.amd.mu.Unlock()

	if client != nil {
		go e.notify()
		_ = client.Close(timeout)
		e.amd.mu.Lock()
		e.amd.stopping = false
		e.amd.mu.Unlock()
	}
	if tempHelperDir != "" {
		_ = os.RemoveAll(tempHelperDir)
	}
	if tempSockDir != "" {
		_ = os.RemoveAll(tempSockDir)
	}
	e.amd.startStopMu.Unlock()
}

func (e *Engine) acceptAmd64(ln net.Listener) {
	for {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		go e.serveAmd64Conn(conn)
	}
}

func (e *Engine) trackAmd64Conns(conns ...net.Conn) bool {
	e.amd.mu.Lock()
	closed := e.amd.closed
	if !closed {
		for _, c := range conns {
			e.amd.tracked[c] = struct{}{}
		}
	}
	e.amd.mu.Unlock()
	if closed {
		for _, c := range conns {
			_ = c.Close()
		}
		return false
	}
	return true
}

func (e *Engine) untrackAmd64Conns(conns ...net.Conn) {
	e.amd.mu.Lock()
	for _, c := range conns {
		delete(e.amd.tracked, c)
	}
	e.amd.mu.Unlock()
}

func (e *Engine) serveAmd64Conn(clientConn net.Conn) {
	if !e.trackAmd64Conns(clientConn) {
		return
	}
	defer func() {
		e.untrackAmd64Conns(clientConn)
		_ = clientConn.Close()
	}()

	e.amd.mu.Lock()
	ctx := e.amd.ctx
	e.amd.mu.Unlock()
	if ctx == nil {
		return
	}

	client, gen, err := e.acquireAmd64(ctx)
	if err != nil {
		e.logf("engine: amd64 builder start failed: %v", err)
		return
	}
	var moved int64
	defer func() {
		e.releaseAmd64(gen, moved)
	}()

	dialCtx, cancel := context.WithTimeout(ctx, builderDialRetryTimeout)
	backendConn, err := client.Dial(dialCtx, vmproto.DialRequest{Service: guestproto.ServiceBuildkitd})
	cancel()
	if err != nil {
		e.logf("engine: dial amd64 buildkitd: %v", err)
		return
	}
	if !e.trackAmd64Conns(backendConn) {
		return
	}
	defer func() {
		e.untrackAmd64Conns(backendConn)
		_ = backendConn.Close()
	}()

	moved = pipeHalfCloseCount(clientConn, backendConn)
}

func (e *Engine) acquireAmd64(ctx context.Context) (client amd64VMClient, gen uint64, err error) {
	e.amd.mu.Lock()
	if e.amd.closed || !e.amd.opened || e.amd.cfg == nil {
		e.amd.mu.Unlock()
		return nil, 0, net.ErrClosed
	}
	e.amd.active++
	if e.amd.idleTimer != nil {
		e.amd.idleTimer.Stop()
		e.amd.idleTimer = nil
	}
	if e.amd.running && e.amd.client != nil {
		c := e.amd.client
		g := e.amd.runGen
		e.amd.mu.Unlock()
		return c, g, nil
	}
	var (
		ch        chan struct{}
		startCtx  context.Context
		needStart bool
	)
	if e.amd.starting {
		ch = e.amd.startCh
	} else {
		ch = make(chan struct{})
		startCtx, e.amd.startCancel = context.WithCancel(e.amd.ctx)
		e.amd.starting = true
		e.amd.startCh = ch
		e.amd.startCtx = startCtx
		e.amd.startErr = nil
		e.amd.lastErr = ""
		needStart = true
	}
	e.amd.mu.Unlock()

	if needStart {
		e.notify()
		go e.runAmd64Start(ch, startCtx)
	}

	select {
	case <-ch:
	case <-ctx.Done():
		e.releaseAmd64(0, 0)
		return nil, 0, ctx.Err()
	}

	e.amd.mu.Lock()
	startErr := e.amd.startErr
	running := e.amd.running
	closed := e.amd.closed
	c := e.amd.client
	g := e.amd.runGen
	e.amd.mu.Unlock()

	if closed {
		e.releaseAmd64(g, 0)
		return nil, 0, net.ErrClosed
	}
	if startErr != nil {
		e.releaseAmd64(g, 0)
		return nil, 0, startErr
	}
	if !running || c == nil {
		e.releaseAmd64(g, 0)
		return nil, 0, errors.New("amd64 builder stopped unexpectedly")
	}
	return c, g, nil
}

func (e *Engine) releaseAmd64(gen uint64, moved int64) {
	e.amd.mu.Lock()
	if gen != 0 && e.amd.runGen == gen {
		e.amd.runBytes += moved
	}
	if e.amd.active > 0 {
		e.amd.active--
	}
	if e.amd.active == 0 && e.amd.running && !e.amd.closed {
		curGen := e.amd.runGen
		if e.amd.idleTimer != nil {
			e.amd.idleTimer.Stop()
		}
		timeout := e.amd64IdleTimeoutLocked()
		if e.amd.runBytes < amd64ProbeBytes {
			timeout = e.amd64ProbeIdleTimeoutLocked()
		}
		e.amd.idleTimer = time.AfterFunc(timeout, func() { e.onAmd64Idle(curGen) })
	}
	e.amd.mu.Unlock()
}

func (e *Engine) onAmd64Idle(gen uint64) {
	e.amd.startStopMu.Lock()
	e.amd.mu.Lock()
	if e.amd.active > 0 || !e.amd.running || e.amd.closed || e.amd.runGen != gen {
		e.amd.mu.Unlock()
		e.amd.startStopMu.Unlock()
		return
	}
	e.amd.running = false
	e.amd.stopping = true
	e.amd.idleTimer = nil
	client := e.amd.client
	e.amd.client = nil
	runBytes := e.amd.runBytes
	e.amd.mu.Unlock()

	if runBytes < amd64ProbeBytes {
		e.logf("engine: stopping amd64 builder VM after probe idle (%d bytes)", runBytes)
	} else {
		e.logf("engine: stopping idle amd64 builder VM")
	}

	if client != nil {
		go e.notify()
		_ = client.Close(defaultAmd64HelperStopGrace)
	}

	e.amd.mu.Lock()
	e.amd.stopping = false
	e.amd.mu.Unlock()
	e.amd.startStopMu.Unlock()
	e.notify()
}

func (e *Engine) runAmd64Start(ch chan struct{}, startCtx context.Context) {
	e.amd.startStopMu.Lock()

	e.amd.mu.Lock()
	closed := e.amd.closed
	cfg := cloneAmd64Config(e.amd.cfg)
	cachedAssets := e.amd.assets
	launchFn := e.amd.launchFn
	e.amd.mu.Unlock()

	var (
		client      amd64VMClient
		assetsDir   vmassets.Dir
		assetsLabel string
		err         error
	)
	if closed || startCtx == nil || cfg == nil {
		err = net.ErrClosed
	} else if cerr := startCtx.Err(); cerr != nil {
		err = cerr
	} else {
		client, assetsDir, assetsLabel, err = e.startAmd64Locked(startCtx, cfg, cachedAssets, launchFn)
	}

	e.amd.mu.Lock()
	ok := err == nil && !e.amd.closed && startCtx.Err() == nil
	if !ok && err == nil {
		if e.amd.closed {
			err = net.ErrClosed
		} else {
			err = startCtx.Err()
		}
	}
	e.amd.mu.Unlock()

	// If the start failed, was cancelled (e.g. ResetAmd64Disk), or the engine
	// closed mid-start, close any helper client we created synchronously while
	// still holding startStopMu so helper.sock and the VM are fully released
	// before any subsequent start or disk reset proceeds.
	if !ok && client != nil {
		_ = client.Close(defaultAmd64HelperStopGrace)
		client = nil
	}

	var watchGen uint64
	var staleClient amd64VMClient
	e.amd.mu.Lock()
	e.amd.starting = false
	e.amd.downloading = false
	if e.amd.startCancel != nil {
		e.amd.startCancel()
	}
	e.amd.startCh = nil
	e.amd.startCtx = nil
	e.amd.startCancel = nil
	e.amd.startErr = err
	if ok {
		ad := assetsDir
		e.amd.assets = &ad
		e.amd.assetsLabel = assetsLabel
		staleClient = e.amd.client
		e.amd.client = client
		e.amd.running = true
		e.amd.lastErr = ""
		e.amd.runGen++
		e.amd.runBytes = 0
		watchGen = e.amd.runGen
		if e.amd.active == 0 {
			if e.amd.idleTimer != nil {
				e.amd.idleTimer.Stop()
			}
			gen := e.amd.runGen
			e.amd.idleTimer = time.AfterFunc(e.amd64ProbeIdleTimeoutLocked(), func() { e.onAmd64Idle(gen) })
		}
	} else {
		e.amd.running = false
		if err != nil && !errors.Is(err, net.ErrClosed) && !errors.Is(err, context.Canceled) {
			e.amd.lastErr = err.Error()
		}
	}
	close(ch)
	e.amd.mu.Unlock()

	if staleClient != nil {
		_ = staleClient.Close(defaultAmd64HelperStopGrace)
	}

	e.amd.startStopMu.Unlock()
	if watchGen != 0 && client != nil {
		go e.watchAmd64Helper(client, watchGen)
	}
	e.notify()
}

// startAmd64Locked resolves assets (unless cached), launches the helper, and
// starts the VM with buildkitd. Caller holds startStopMu. On any error, any
// helper client created during this call is closed before returning.
func (e *Engine) startAmd64Locked(
	ctx context.Context,
	cfg *Amd64Config,
	cachedAssets *vmassets.Dir,
	launchFn func(ctx context.Context, l vmclient.Launcher) (amd64VMClient, error),
) (amd64VMClient, vmassets.Dir, string, error) {
	var assetsDir vmassets.Dir
	if cachedAssets != nil {
		assetsDir = *cachedAssets
	} else {
		var lastNotify atomic.Int64
		progress := func(done, total int64) {
			e.amd.mu.Lock()
			e.amd.downloading = true
			e.amd.dlDone = done
			e.amd.dlTotal = total
			e.amd.mu.Unlock()
			now := time.Now().UnixNano()
			prev := lastNotify.Load()
			if prev == 0 || now-prev >= int64(100*time.Millisecond) || (total > 0 && done >= total) {
				if lastNotify.CompareAndSwap(prev, now) {
					go e.notify()
				}
			}
		}
		var err error
		if cfg.Assets != nil {
			assetsDir, err = cfg.Assets(ctx, progress)
		} else {
			assetsDir, err = vmassets.Resolve(ctx, vmassets.Options{Progress: progress})
		}
		e.amd.mu.Lock()
		e.amd.downloading = false
		e.amd.mu.Unlock()
		if err != nil {
			return nil, vmassets.Dir{}, "", err
		}
	}

	assetsLabel := assetsDir.Manifest.Version
	if assetsDir.Override {
		assetsLabel = "local"
	} else if assetsLabel == "" {
		assetsLabel = vmassets.Version
	}

	helperDir, err := e.ensureAmd64HelperDirLocked()
	if err != nil {
		return nil, vmassets.Dir{}, "", fmt.Errorf("prepare amd64 helper dir: %w", err)
	}

	e.amd.mu.Lock()
	diskPath := e.amd64DiskPathLocked()
	diskSize := e.amd64DiskSizeLocked()
	e.amd.mu.Unlock()

	if diskPath != "" {
		if err := os.MkdirAll(filepath.Dir(diskPath), 0o700); err != nil {
			return nil, vmassets.Dir{}, "", fmt.Errorf("prepare amd64 disk dir: %w", err)
		}
	}

	launcher := cfg.Launcher
	if launcher.Dir == "" {
		launcher.Dir = helperDir
	}
	if launcher.Log == nil {
		launcher.Log = func(line string) {
			e.logf("engine: amd64 helper: %s", line)
		}
	}

	var client amd64VMClient
	if launchFn != nil {
		client, err = launchFn(ctx, launcher)
	} else {
		client, err = launcher.Launch(ctx)
	}
	if err != nil {
		if client != nil {
			_ = client.Close(defaultAmd64HelperStopGrace)
		}
		return nil, vmassets.Dir{}, "", fmt.Errorf("launch amd64 helper: %w", err)
	}

	cpus := cfg.CPUs
	if cpus <= 0 {
		cpus = max(2, runtime.NumCPU()/2)
	}
	memMiB := cfg.MemoryMiB
	if memMiB <= 0 {
		memMiB = defaultAmd64MemoryMiB
	}
	spec := vmproto.VMSpec{
		Kernel:    assetsDir.Kernel(),
		Initrd:    assetsDir.Initrd(),
		Assets:    assetsDir.Path,
		CPUs:      cpus,
		MemoryMiB: memMiB,
		Rosetta:   true,
		Network:   true,
		Services:  []string{guestproto.ServiceBuildkitd},
	}
	if diskPath != "" {
		spec.Disk = &vmproto.DiskSpec{
			Path: diskPath,
			Size: diskSize,
		}
	}

	startCtx, cancel := context.WithTimeout(ctx, defaultAmd64StartTimeout)
	status, err := client.Start(startCtx, spec)
	cancel()
	if err != nil {
		_ = client.Close(defaultAmd64HelperStopGrace)
		return nil, vmassets.Dir{}, "", fmt.Errorf("start amd64 VM: %w", err)
	}
	if status.State == vmproto.Failed {
		_ = client.Close(defaultAmd64HelperStopGrace)
		msg := status.Err
		if msg == "" {
			msg = "amd64 VM failed to start"
		}
		return nil, vmassets.Dir{}, "", errors.New(msg)
	}
	if err := ctx.Err(); err != nil {
		_ = client.Close(defaultAmd64HelperStopGrace)
		return nil, vmassets.Dir{}, "", err
	}
	return client, assetsDir, assetsLabel, nil
}

func (e *Engine) ensureAmd64HelperDirLocked() (string, error) {
	e.amd.mu.Lock()
	existing := e.amd.helperDir
	e.amd.mu.Unlock()
	if existing != "" {
		if err := os.MkdirAll(existing, 0o700); err != nil {
			return "", err
		}
		return existing, nil
	}

	var dir, tempDir string
	if e.cfg.Dir != "" {
		cand := filepath.Join(e.cfg.Dir, amd64DirName)
		if len(filepath.Join(cand, vmhelper.SocketName)) <= builderMaxUnixSockPath {
			dir = cand
		}
	}
	if dir == "" {
		short, err := os.MkdirTemp("", "gvxd-vmh-")
		if err != nil {
			return "", err
		}
		dir = short
		tempDir = short
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		if tempDir != "" {
			_ = os.RemoveAll(tempDir)
		}
		return "", err
	}

	e.amd.mu.Lock()
	e.amd.helperDir = dir
	e.amd.tempHelperDir = tempDir
	e.amd.mu.Unlock()
	return dir, nil
}

func (e *Engine) watchAmd64Helper(client amd64VMClient, gen uint64) {
	<-client.Done()

	e.amd.mu.Lock()
	if e.amd.client != client || e.amd.runGen != gen {
		e.amd.mu.Unlock()
		return
	}
	e.amd.client = nil
	e.amd.running = false
	if e.amd.idleTimer != nil {
		e.amd.idleTimer.Stop()
		e.amd.idleTimer = nil
	}
	if err := client.ExitErr(); err != nil {
		e.amd.lastErr = err.Error()
	} else {
		e.amd.lastErr = "amd64 builder helper exited unexpectedly"
	}
	conns := make([]net.Conn, 0, len(e.amd.tracked))
	for c := range e.amd.tracked {
		conns = append(conns, c)
	}
	clear(e.amd.tracked)
	msg := e.amd.lastErr
	e.amd.mu.Unlock()

	for _, c := range conns {
		_ = c.Close()
	}
	e.logf("engine: amd64 builder helper exited: %s", msg)
	e.notify()
}

// pipeHalfCloseCount copies bytes bidirectionally between client and backend
// like pipeHalfClose and returns the total bytes transferred in both directions.
func pipeHalfCloseCount(client, backend net.Conn) int64 {
	closeWrite := func(c net.Conn) {
		if cw, ok := c.(interface{ CloseWrite() error }); ok {
			_ = cw.CloseWrite()
			return
		}
		_ = c.Close()
	}

	backendDone := make(chan struct{})
	var n1, n2 int64
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		var err error
		n1, err = io.Copy(backend, client)
		if err != nil {
			_ = backend.Close()
			return
		}
		closeWrite(backend)
		if attachWaitPeerClose(client, backendDone) {
			_ = backend.Close()
		}
	}()
	go func() {
		defer wg.Done()
		n2, _ = io.Copy(client, backend)
		closeWrite(client)
		close(backendDone)
		_ = client.Close()
		_ = backend.Close()
	}()
	wg.Wait()
	return n1 + n2
}
