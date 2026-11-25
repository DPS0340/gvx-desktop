package ext

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/ironpark/gvx-desktop/internal/recipes"
	"github.com/ironpark/gvx-desktop/internal/rungv"
	"github.com/ironpark/gvx-desktop/internal/sandbox"
)

type sandboxRuntimeConfig struct {
	extID          string
	dataDir        string
	manifest       *Manifest
	source         SourceSpec
	grants         *GrantStore
	supervisor     *sandbox.Supervisor
	hostEndpoint   string
	hostSockPath   string
	hostPort       int
	hostToken      string
	extToken       string
	dockerEndpoint string
	dockerSockPath string
	dockerPort     int
}

type sandboxRuntime struct {
	cfg      sandboxRuntimeConfig
	sbxID    string
	logs     *extLogWriter
	client   *extClient
	extSock  string
	webSock  string
	bridge   *tcpUnixBridge
	doneCh   chan error
	stopCh   chan struct{}
	stopOnce sync.Once
	mu       sync.Mutex
	stopping bool
	exitedCh chan struct{}
	webAddr  string
}

func newSandboxRuntime(cfg sandboxRuntimeConfig) *sandboxRuntime {
	return &sandboxRuntime{
		cfg:    cfg,
		sbxID:  "ext-" + cfg.extID,
		doneCh: make(chan error, 1),
		stopCh: make(chan struct{}),
	}
}

func (r *sandboxRuntime) Start(ctx context.Context) error {
	if r.cfg.supervisor == nil {
		return errors.New("sandbox supervisor is unavailable (cannot degrade sandbox extension to binary)")
	}
	if r.cfg.manifest == nil {
		return errors.New("manifest is nil")
	}

	filesDir := FilesDir(r.cfg.dataDir, r.cfg.extID)
	if err := EnsureDirWithin(r.cfg.dataDir, filesDir); err != nil {
		return err
	}
	logWriter, err := newExtLogWriter(r.cfg.dataDir, r.cfg.extID, r.cfg.hostToken, r.cfg.extToken)
	if err != nil {
		return err
	}

	port := r.cfg.manifest.Runtime.Port
	if port <= 0 {
		port = DefaultRuntimePort
	}

	imageRef := r.cfg.source.ImageRef
	pullPolicy := "missing"
	if r.cfg.source.Kind != SourceImage && r.cfg.manifest.Runtime.Dockerfile != "" {
		if imageRef == "" {
			imageRef = ExtensionImageTag(r.cfg.manifest.ID, r.cfg.manifest.Version, r.cfg.source.Commit)
		}
		pullPolicy = "never"
	}
	if imageRef == "" {
		imageRef = r.cfg.manifest.Runtime.Image
	}
	if imageRef == "" {
		logWriter.Close()
		return errors.New("sandbox extension has no built or pulled image reference")
	}

	mem := strings.TrimSpace(r.cfg.manifest.Runtime.Memory)
	if mem == "" {
		mem = DefaultRuntimeMemory
	}

	hasNetwork := r.cfg.grants != nil && r.cfg.grants.HasPermission(r.cfg.extID, PermNetwork, r.cfg.manifest.Permissions)

	var feat rungv.Features
	if r.cfg.supervisor.Client != nil {
		if c := r.cfg.supervisor.Client(); c != nil && c.Path != "" {
			feat = c.Features(ctx)
		}
	}

	network := "nat"
	isolateLAN := false
	natPolicyFile := ""
	if !hasNetwork {
		if feat.NATPolicy {
			// rungv rejects `-net none` when `-p unix:...` is passed ("port forwards
			// need network mode nat"); enforce deny-all egress via NAT firewall policy
			// + IsolateLAN so the sandbox still gets -p unix:... with zero egress.
			polPath := filepath.Join(ExtensionDataDir(r.cfg.dataDir, r.cfg.extID), "no-egress-nat-policy.json")
			const denyAllEgressPolicy = "{\"egress\":{\"default\":\"deny\"},\"ingress\":{\"default\":\"allow\"}}\n"
			if werr := WriteFileAtomicWithin(r.cfg.dataDir, polPath, []byte(denyAllEgressPolicy), 0o600); werr != nil {
				logWriter.Close()
				return fmt.Errorf("write no-egress NAT policy: %w", werr)
			}
			network = "nat"
			isolateLAN = true
			natPolicyFile = polPath
		} else {
			network = "none"
		}
	}

	mounts := []sandbox.Mount{
		{Type: sandbox.MountBind, Source: filesDir, Target: "/data"},
	}
	seenTargets := map[string]bool{"/data": true}
	if r.cfg.grants != nil {
		for _, folder := range r.cfg.grants.Get(r.cfg.extID).AllowedFolders {
			target, gerr := recipes.GuestPath(folder)
			if gerr != nil || recipes.ReservedGuestPath(target) || seenTargets[target] {
				continue
			}
			seenTargets[target] = true
			mounts = append(mounts, sandbox.Mount{
				Type:   sandbox.MountBind,
				Source: folder,
				Target: target,
			})
		}
	}

	useUnix := runtime.GOOS != "windows" && feat.UnixForward && r.cfg.hostSockPath != ""

	var (
		ports        []string
		unixForwards []string
		extEndpoint  string
		extSock      string
		webSock      string
		hostAPIEnv   string
		dockerEnv    string
		webTarget    string
		bridge       *tcpUnixBridge
	)

	hasDocker := r.cfg.grants != nil && r.cfg.grants.HasPermission(r.cfg.extID, PermDocker, r.cfg.manifest.Permissions)
	hasWebUI := r.cfg.manifest.WebUI != nil && r.cfg.grants != nil && r.cfg.grants.HasPermission(r.cfg.extID, PermLocalProxy, r.cfg.manifest.Permissions)

	if useUnix {
		p := NewPaths(r.cfg.dataDir)
		// rungv's NAT policy firewall only allows TCP loopback port forwards
		// ("NAT policy denied every target for published TCP connection" for unix:).
		// When NATPolicyFile is active (!hasNetwork), publish Extension/WebUI on
		// 127.0.0.1:<ephemeral> (protected by 256-bit Bearer GVX_EXT_TOKEN) while
		// still using --unix-forward for Host API and scoped Docker socket.
		if natPolicyFile == "" {
			extSock, err = p.SocketPath("e", r.cfg.extID)
			if err != nil {
				logWriter.Close()
				return err
			}
			removeSocketIfSocket(extSock)
			ports = append(ports, fmt.Sprintf("unix:%s:%d", extSock, port))
			extEndpoint = "unix://" + extSock
		} else {
			hostPort, perr := pickFreeLoopbackPort()
			if perr != nil {
				logWriter.Close()
				return perr
			}
			ports = append(ports, fmt.Sprintf("127.0.0.1:%d:%d", hostPort, port))
			extEndpoint = fmt.Sprintf("http://127.0.0.1:%d", hostPort)
		}

		unixForwards = append(unixForwards, "/run/gvx/host.sock="+r.cfg.hostSockPath)
		hostAPIEnv = "unix:///run/gvx/host.sock"

		if hasDocker && r.cfg.dockerSockPath != "" {
			unixForwards = append(unixForwards, "/run/gvx/docker.sock="+r.cfg.dockerSockPath)
			dockerEnv = "unix:///run/gvx/docker.sock"
		}

		if hasWebUI {
			if natPolicyFile == "" {
				webSock, err = p.SocketPath("w", r.cfg.extID)
				if err != nil {
					logWriter.Close()
					return err
				}
				removeSocketIfSocket(webSock)
				ports = append(ports, fmt.Sprintf("unix:%s:%d", webSock, r.cfg.manifest.WebUI.Port))
				bridge, err = newTCPUnixBridge(webSock)
				if err != nil {
					logWriter.Close()
					return err
				}
				webTarget = bridge.Addr()
			} else {
				wHostPort, werr := pickFreeLoopbackPort()
				if werr != nil {
					logWriter.Close()
					return werr
				}
				ports = append(ports, fmt.Sprintf("127.0.0.1:%d:%d", wHostPort, r.cfg.manifest.WebUI.Port))
				webTarget = fmt.Sprintf("127.0.0.1:%d", wHostPort)
			}
		}
	} else {
		hostPort, perr := pickFreeLoopbackPort()
		if perr != nil {
			logWriter.Close()
			return perr
		}
		ports = append(ports, fmt.Sprintf("127.0.0.1:%d:%d", hostPort, port))
		extEndpoint = fmt.Sprintf("http://127.0.0.1:%d", hostPort)
		if r.cfg.hostPort > 0 {
			hostAPIEnv = fmt.Sprintf("http://10.0.2.2:%d", r.cfg.hostPort)
		} else {
			hostAPIEnv = r.cfg.hostEndpoint
		}
		if hasDocker {
			if r.cfg.dockerPort > 0 {
				dockerEnv = fmt.Sprintf("http://10.0.2.2:%d", r.cfg.dockerPort)
			} else if r.cfg.dockerEndpoint != "" {
				dockerEnv = r.cfg.dockerEndpoint
			}
		}
		if hasWebUI {
			wHostPort, werr := pickFreeLoopbackPort()
			if werr != nil {
				logWriter.Close()
				return werr
			}
			ports = append(ports, fmt.Sprintf("127.0.0.1:%d:%d", wHostPort, r.cfg.manifest.WebUI.Port))
			webTarget = fmt.Sprintf("127.0.0.1:%d", wHostPort)
		}
	}

	env := []string{
		"GVX_API_VERSION=1",
		"GVX_EXT_ID=" + r.cfg.extID,
		"GVX_EXT_VERSION=" + r.cfg.manifest.Version,
		"GVX_HOST_API=" + hostAPIEnv,
		"GVX_HOST_TOKEN=" + r.cfg.hostToken,
		"GVX_EXT_LISTEN=0.0.0.0:" + strconv.Itoa(port),
		"GVX_EXT_TOKEN=" + r.cfg.extToken,
		"GVX_EXT_DATA_DIR=/data",
	}
	if dockerEnv != "" {
		env = append(env, "GVX_DOCKER_HOST="+dockerEnv)
	}

	spec := sandbox.Spec{
		ID:            r.sbxID,
		Name:          r.sbxID,
		Image:         imageRef,
		Env:           env,
		Ports:         ports,
		UnixForwards:  unixForwards,
		Mounts:        mounts,
		Memory:        mem,
		CPUs:          r.cfg.manifest.Runtime.CPUs,
		Network:       network,
		IsolateLAN:    isolateLAN,
		NATPolicyFile: natPolicyFile,
		Pull:          pullPolicy,
		Labels:        map[string]string{"io.gvx.extension": r.cfg.extID},
		CreatedAt:     time.Now(),
	}

	client, err := newExtClient(extEndpoint, r.cfg.extToken)
	if err != nil {
		if bridge != nil {
			bridge.Close()
		}
		logWriter.Close()
		return err
	}

	if err := r.cfg.supervisor.Start(spec); err != nil {
		if bridge != nil {
			bridge.Close()
		}
		logWriter.Close()
		if network == "none" && !useUnix && !hasNetwork && !feat.NATPolicy {
			return fmt.Errorf("start extension sandbox (network=none requires rungv --unix-forward or --nat-policy-file): %w", err)
		}
		return fmt.Errorf("start extension sandbox: %w", err)
	}

	exitedCh := make(chan struct{})
	r.mu.Lock()
	r.logs = logWriter
	r.client = client
	r.extSock = extSock
	r.webSock = webSock
	r.bridge = bridge
	r.exitedCh = exitedCh
	r.webAddr = webTarget
	r.mu.Unlock()

	go r.watchSandbox(logWriter, exitedCh)

	if err := waitExtensionHealthy(ctx, client, exitedCh); err != nil {
		_ = r.Stop(2 * time.Second)
		return err
	}
	return nil
}

func (r *sandboxRuntime) watchSandbox(logWriter *extLogWriter, exitedCh chan struct{}) {
	lb := r.cfg.supervisor.Logs(r.sbxID)
	var lastSeq int64
	drainLogs := func() {
		for _, ln := range lb.Since(lastSeq) {
			lastSeq = ln.Seq
			logWriter.AddLine(ln.Text)
		}
	}

	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()

	for {
		drainLogs()
		st := r.cfg.supervisor.State(r.sbxID)
		if !st.Status.Active() {
			drainLogs()
			close(exitedCh)
			r.mu.Lock()
			stopping := r.stopping
			r.mu.Unlock()
			if !stopping {
				var err error
				if st.Error != "" {
					err = errors.New(st.Error)
				} else {
					err = fmt.Errorf("extension sandbox exited with status %s (code %d)", st.Status, st.ExitCode)
				}
				select {
				case r.doneCh <- err:
				default:
				}
			}
			return
		}
		select {
		case <-r.stopCh:
			drainLogs()
			close(exitedCh)
			return
		case <-ticker.C:
		}
	}
}

func (r *sandboxRuntime) Stop(timeout time.Duration) error {
	r.stopOnce.Do(func() {
		r.mu.Lock()
		r.stopping = true
		client := r.client
		extSock := r.extSock
		webSock := r.webSock
		bridge := r.bridge
		logs := r.logs
		exitedCh := r.exitedCh
		r.mu.Unlock()

		if timeout <= 0 {
			timeout = 8 * time.Second
		}
		deadline := time.Now().Add(timeout)

		if client != nil && exitedCh != nil && !isClosed(exitedCh) {
			httpBudget := min(gracefulHTTPTimeout, timeout/2)
			if timeout > 2*time.Second {
				httpBudget = min(httpBudget, timeout-gracefulTermTimeout-500*time.Millisecond)
			}
			if httpBudget > 0 {
				sctx, cancel := context.WithTimeout(context.Background(), httpBudget)
				_ = client.Shutdown(sctx)
				cancel()
			}
		}

		if r.cfg.supervisor != nil {
			rem := max(time.Until(deadline), 500*time.Millisecond)
			_ = r.cfg.supervisor.Stop(r.sbxID, min(gracefulTermTimeout, rem))
			r.cfg.supervisor.Forget(r.sbxID)
		}
		close(r.stopCh)
		if exitedCh != nil {
			select {
			case <-exitedCh:
			case <-time.After(1 * time.Second):
			}
		}
		if bridge != nil {
			bridge.Close()
		}
		if client != nil {
			client.closeIdle()
		}
		removeSocketIfSocket(extSock)
		removeSocketIfSocket(webSock)
		if logs != nil {
			logs.Close()
		}
	})
	return nil
}

func (r *sandboxRuntime) Client() *extClient {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.client
}

func (r *sandboxRuntime) WebUITarget() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.webAddr
}

func (r *sandboxRuntime) Done() <-chan error {
	return r.doneCh
}

func (r *sandboxRuntime) Logs() []string {
	r.mu.Lock()
	logs := r.logs
	r.mu.Unlock()
	if logs == nil {
		return nil
	}
	return logs.Lines()
}

// tcpUnixBridge exposes a guest Unix socket over 127.0.0.1:0 so localproxy
// can forward HTTP/WebSocket traffic to a sandboxed extension's web_ui port
// even when Network == "none".
type tcpUnixBridge struct {
	ln       net.Listener
	unixSock string
	closed   chan struct{}
	once     sync.Once
}

func newTCPUnixBridge(unixSock string) (*tcpUnixBridge, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	b := &tcpUnixBridge{
		ln:       ln,
		unixSock: unixSock,
		closed:   make(chan struct{}),
	}
	go b.serve()
	return b, nil
}

func (b *tcpUnixBridge) Addr() string {
	if b == nil || b.ln == nil {
		return ""
	}
	return b.ln.Addr().String()
}

func (b *tcpUnixBridge) Close() {
	if b == nil {
		return
	}
	b.once.Do(func() {
		close(b.closed)
		_ = b.ln.Close()
	})
}

func (b *tcpUnixBridge) serve() {
	for {
		c, err := b.ln.Accept()
		if err != nil {
			return
		}
		go b.handle(c)
	}
}

func (b *tcpUnixBridge) handle(client net.Conn) {
	defer client.Close()
	up, err := net.DialTimeout("unix", b.unixSock, 5*time.Second)
	if err != nil {
		return
	}
	defer up.Close()

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		_, _ = io.Copy(up, client)
		if cw, ok := up.(interface{ CloseWrite() error }); ok {
			_ = cw.CloseWrite()
		}
	}()
	go func() {
		defer wg.Done()
		_, _ = io.Copy(client, up)
		if cw, ok := client.(interface{ CloseWrite() error }); ok {
			_ = cw.CloseWrite()
		}
	}()
	wg.Wait()
}
