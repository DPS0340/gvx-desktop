package ext

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"time"
)

const (
	healthPollTimeout   = 15 * time.Second
	healthPollInterval  = 50 * time.Millisecond
	maxExtLogFileBytes  = 5 << 20
	maxExtLogBackups    = 2
	maxExtMemLogLines   = 2000
	gracefulHTTPTimeout = 5 * time.Second
	gracefulTermTimeout = 3 * time.Second
)

type extRuntime interface {
	Start(ctx context.Context) error
	Stop(timeout time.Duration) error
	Client() *extClient
	WebUITarget() string
	Done() <-chan error
	Logs() []string
}

// extLogWriter writes extension stdout/stderr lines to a rotating log file under
// <dataDir>/extension-data/<id>/logs/extension.log and an in-memory ring buffer.
type extLogWriter struct {
	mu             sync.Mutex
	path           string
	maxSize        int64
	backups        int
	file           *os.File
	curSize        int64
	ring           []string
	maxRing        int
	redactedTokens []string
}

func newExtLogWriter(dataDir, extID string, redactedTokens ...string) (*extLogWriter, error) {
	dir := LogsDir(dataDir, extID)
	if err := EnsureDirWithin(dataDir, dir); err != nil {
		return nil, err
	}
	logPath := filepath.Join(dir, "extension.log")
	if err := CheckNoSymlinkWithin(dataDir, logPath); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return nil, err
	}
	var curSize int64
	if info, serr := f.Stat(); serr == nil {
		curSize = info.Size()
	}
	var filtered []string
	for _, tok := range redactedTokens {
		if len(tok) >= 8 {
			filtered = append(filtered, tok)
		}
	}
	return &extLogWriter{
		path:           logPath,
		maxSize:        maxExtLogFileBytes,
		backups:        maxExtLogBackups,
		file:           f,
		curSize:        curSize,
		maxRing:        maxExtMemLogLines,
		redactedTokens: filtered,
	}, nil
}

func redactExtLogLine(line string, tokens ...string) string {
	for _, key := range []string{"GVX_HOST_TOKEN", "GVX_EXT_TOKEN"} {
		line = redactEnvKeyInLine(line, key)
	}
	for _, tok := range tokens {
		if tok != "" {
			line = strings.ReplaceAll(line, tok, "<redacted>")
		}
	}
	return line
}

func isEnvKeyByte(b byte) bool {
	return (b >= 'A' && b <= 'Z') || (b >= 'a' && b <= 'z') || (b >= '0' && b <= '9') || b == '_'
}

func redactEnvKeyInLine(line, key string) string {
	prefix := key + "="
	var out strings.Builder
	pos := 0
	for {
		rel := strings.Index(line[pos:], prefix)
		if rel < 0 {
			if out.Len() == 0 {
				return line
			}
			out.WriteString(line[pos:])
			return out.String()
		}
		idx := pos + rel
		if idx > 0 && isEnvKeyByte(line[idx-1]) {
			out.WriteString(line[pos : idx+len(prefix)])
			pos = idx + len(prefix)
			continue
		}
		valStart := idx + len(prefix)
		if valStart >= len(line) {
			if out.Len() == 0 {
				return line
			}
			out.WriteString(line[pos:])
			return out.String()
		}
		if line[valStart] == '\'' || line[valStart] == '"' {
			q := line[valStart]
			endRel := strings.IndexByte(line[valStart+1:], q)
			if endRel >= 0 {
				valEnd := valStart + 1 + endRel
				out.WriteString(line[pos : valStart+1])
				if valEnd > valStart+1 {
					out.WriteString("<redacted>")
				}
				out.WriteByte(q)
				pos = valEnd + 1
				continue
			}
		}
		valEnd := valStart
		for valEnd < len(line) && line[valEnd] != ' ' && line[valEnd] != '\t' && line[valEnd] != '\r' && line[valEnd] != '\n' {
			valEnd++
		}
		if valEnd == valStart {
			out.WriteString(line[pos:valStart])
			pos = valStart
			continue
		}
		out.WriteString(line[pos:valStart])
		out.WriteString("<redacted>")
		pos = valEnd
	}
}

func (w *extLogWriter) AddLine(line string) {
	if w == nil {
		return
	}
	line = redactExtLogLine(line, w.redactedTokens...)
	w.mu.Lock()
	defer w.mu.Unlock()

	w.ring = append(w.ring, line)
	if over := len(w.ring) - w.maxRing; over > 0 {
		w.ring = append([]string(nil), w.ring[over:]...)
	}

	if w.file == nil {
		return
	}
	entry := line + "\n"
	if w.curSize+int64(len(entry)) > w.maxSize && w.curSize > 0 {
		w.rotateLocked()
	}
	n, err := w.file.WriteString(entry)
	if err == nil {
		w.curSize += int64(n)
	}
}

func (w *extLogWriter) rotateLocked() {
	if w.file != nil {
		_ = w.file.Close()
		w.file = nil
	}
	for i := w.backups - 1; i >= 1; i-- {
		src := fmt.Sprintf("%s.%d", w.path, i)
		dst := fmt.Sprintf("%s.%d", w.path, i+1)
		_ = os.Rename(src, dst)
	}
	_ = os.Rename(w.path, w.path+".1")
	f, err := os.OpenFile(w.path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err == nil {
		w.file = f
		w.curSize = 0
	}
}

func (w *extLogWriter) Lines() []string {
	if w == nil {
		return nil
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	return slices.Clone(w.ring)
}

func (w *extLogWriter) Close() {
	if w == nil {
		return
	}
	w.mu.Lock()
	if w.file != nil {
		_ = w.file.Close()
		w.file = nil
	}
	w.mu.Unlock()
}

type binaryRuntimeConfig struct {
	extID          string
	dataDir        string
	manifest       *Manifest
	source         SourceSpec
	grants         *GrantStore
	developerMode  func() bool
	hostEndpoint   string
	hostToken      string
	extToken       string
	dockerEndpoint string
}

type binaryRuntime struct {
	cfg       binaryRuntimeConfig
	logs      *extLogWriter
	client    *extClient
	sockPath  string
	doneCh    chan error
	mu        sync.Mutex
	cmd       *exec.Cmd
	stopOnce  sync.Once
	stopping  bool
	exitDone  chan struct{}
	exitErr   error
	pumpDone  chan struct{}
	webTarget string
}

func newBinaryRuntime(cfg binaryRuntimeConfig) *binaryRuntime {
	return &binaryRuntime{
		cfg:    cfg,
		doneCh: make(chan error, 1),
	}
}

func (r *binaryRuntime) Start(ctx context.Context) error {
	if r.cfg.developerMode == nil || !r.cfg.developerMode() {
		return errors.New("Developer Mode is required for binary extensions")
	}
	if r.cfg.manifest == nil {
		return errors.New("manifest is nil")
	}

	var sourceRoot string
	switch r.cfg.source.Kind {
	case SourceLocal:
		sourceRoot = r.cfg.source.LocalPath
	case SourceGit:
		sourceRoot = SourceCheckoutDir(r.cfg.dataDir, r.cfg.extID)
	default:
		return fmt.Errorf("binary runtime requires local or git source, got %q", r.cfg.source.Kind)
	}
	if strings.TrimSpace(sourceRoot) == "" {
		return errors.New("binary extension source path is empty")
	}

	entryPath, err := ResolveWithinRoot(sourceRoot, r.cfg.manifest.Runtime.DevEntrypoint, false)
	if err != nil {
		return fmt.Errorf("dev_entrypoint: %w", err)
	}
	info, err := os.Stat(entryPath)
	if err != nil {
		return fmt.Errorf("stat dev_entrypoint %q: %w", r.cfg.manifest.Runtime.DevEntrypoint, err)
	}
	if !isFileExecutable(info) {
		return fmt.Errorf("dev_entrypoint %q is not an executable file", r.cfg.manifest.Runtime.DevEntrypoint)
	}

	filesDir := FilesDir(r.cfg.dataDir, r.cfg.extID)
	if err := EnsureDirWithin(r.cfg.dataDir, filesDir); err != nil {
		return err
	}
	logWriter, err := newExtLogWriter(r.cfg.dataDir, r.cfg.extID, r.cfg.hostToken, r.cfg.extToken)
	if err != nil {
		return err
	}

	var extListen string
	var sockPath string
	if runtime.GOOS != "windows" {
		sockPath, err = NewPaths(r.cfg.dataDir).SocketPath("e", r.cfg.extID)
		if err != nil {
			logWriter.Close()
			return err
		}
		removeSocketIfSocket(sockPath)
		extListen = "unix://" + sockPath
	} else {
		port, perr := pickFreeLoopbackPort()
		if perr != nil {
			logWriter.Close()
			return perr
		}
		extListen = fmt.Sprintf("127.0.0.1:%d", port)
	}

	client, err := newExtClient(extListen, r.cfg.extToken)
	if err != nil {
		logWriter.Close()
		return err
	}

	env := cleanBinaryEnv()
	env = append(env,
		"GVX_API_VERSION=1",
		"GVX_EXT_ID="+r.cfg.extID,
		"GVX_EXT_VERSION="+r.cfg.manifest.Version,
		"GVX_HOST_API="+r.cfg.hostEndpoint,
		"GVX_HOST_TOKEN="+r.cfg.hostToken,
		"GVX_EXT_LISTEN="+extListen,
		"GVX_EXT_TOKEN="+r.cfg.extToken,
		"GVX_EXT_DATA_DIR="+filesDir,
	)
	if r.cfg.grants != nil && r.cfg.grants.HasPermission(r.cfg.extID, PermDocker, r.cfg.manifest.Permissions) && r.cfg.dockerEndpoint != "" {
		env = append(env, "GVX_DOCKER_HOST="+r.cfg.dockerEndpoint)
	}

	cmd := exec.Command(entryPath)
	cmd.Dir = sourceRoot
	cmd.Env = env
	setProcessGroup(cmd)

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		logWriter.Close()
		return err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		logWriter.Close()
		return err
	}

	if err := cmd.Start(); err != nil {
		logWriter.Close()
		return fmt.Errorf("start binary extension: %w", err)
	}

	var webTarget string
	if r.cfg.manifest.WebUI != nil && r.cfg.grants != nil && r.cfg.grants.HasPermission(r.cfg.extID, PermLocalProxy, r.cfg.manifest.Permissions) {
		webTarget = fmt.Sprintf("127.0.0.1:%d", r.cfg.manifest.WebUI.Port)
	}

	exitDone := make(chan struct{})
	pumpDone := make(chan struct{})

	r.mu.Lock()
	r.cmd = cmd
	r.logs = logWriter
	r.client = client
	r.sockPath = sockPath
	r.exitDone = exitDone
	r.pumpDone = pumpDone
	r.webTarget = webTarget
	r.mu.Unlock()

	var pumpWG sync.WaitGroup
	pumpWG.Add(2)
	go pumpLines(stdout, logWriter, &pumpWG)
	go pumpLines(stderr, logWriter, &pumpWG)
	go func() {
		pumpWG.Wait()
		close(pumpDone)
	}()

	go func() {
		<-pumpDone
		waitErr := cmd.Wait()
		r.mu.Lock()
		r.exitErr = waitErr
		stopping := r.stopping
		r.mu.Unlock()
		close(exitDone)
		if !stopping {
			select {
			case r.doneCh <- waitErr:
			default:
			}
		}
	}()

	if err := waitExtensionHealthy(ctx, client, exitDone); err != nil {
		_ = r.Stop(2 * time.Second)
		return err
	}
	return nil
}

func (r *binaryRuntime) Stop(timeout time.Duration) error {
	r.stopOnce.Do(func() {
		r.mu.Lock()
		r.stopping = true
		cmd := r.cmd
		client := r.client
		exitDone := r.exitDone
		sockPath := r.sockPath
		logs := r.logs
		r.mu.Unlock()

		if timeout <= 0 {
			timeout = 8 * time.Second
		}
		deadline := time.Now().Add(timeout)

		var shutErr error
		if client != nil && exitDone != nil && !isClosed(exitDone) {
			httpBudget := min(gracefulHTTPTimeout, timeout/2)
			if timeout > 2*time.Second {
				httpBudget = min(httpBudget, timeout-gracefulTermTimeout-500*time.Millisecond)
			}
			if httpBudget > 0 {
				sctx, cancel := context.WithTimeout(context.Background(), httpBudget)
				shutErr = client.Shutdown(sctx)
				cancel()
			}
		}

		if cmd != nil && cmd.Process != nil && exitDone != nil {
			// Only give the process a brief grace window to exit after /v1/shutdown
			// when the HTTP shutdown call succeeded.
			if shutErr == nil {
				waitHTTP := min(500*time.Millisecond, time.Until(deadline))
				if waitHTTP > 0 {
					select {
					case <-exitDone:
					case <-time.After(waitHTTP):
					}
				}
			}
			if !isClosed(exitDone) {
				signalTerminate(cmd)
				termWait := min(gracefulTermTimeout, time.Until(deadline))
				if termWait > 0 {
					select {
					case <-exitDone:
					case <-time.After(termWait):
					}
				}
			}
			if !isClosed(exitDone) {
				signalForceKill(cmd)
				select {
				case <-exitDone:
				case <-time.After(2 * time.Second):
				}
			}
		}

		if client != nil {
			client.closeIdle()
		}
		removeSocketIfSocket(sockPath)
		if logs != nil {
			logs.Close()
		}
	})
	return nil
}

func (r *binaryRuntime) Client() *extClient {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.client
}

func (r *binaryRuntime) WebUITarget() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.webTarget
}

func (r *binaryRuntime) Done() <-chan error {
	return r.doneCh
}

func (r *binaryRuntime) Logs() []string {
	r.mu.Lock()
	logs := r.logs
	r.mu.Unlock()
	if logs == nil {
		return nil
	}
	return logs.Lines()
}

func pumpLines(rc io.Reader, w *extLogWriter, wg *sync.WaitGroup) {
	defer wg.Done()
	scanner := bufio.NewScanner(rc)
	scanner.Buffer(make([]byte, 0, 16*1024), 1024*1024)
	for scanner.Scan() {
		w.AddLine(scanner.Text())
	}
}

func waitExtensionHealthy(ctx context.Context, client *extClient, exited <-chan struct{}) error {
	deadline := time.Now().Add(healthPollTimeout)
	pollCtx, cancel := context.WithDeadline(ctx, deadline)
	defer cancel()

	var lastErr error
	for {
		select {
		case <-pollCtx.Done():
			if lastErr != nil {
				return fmt.Errorf("extension failed health check within %s: %w", healthPollTimeout, lastErr)
			}
			return fmt.Errorf("extension failed health check: %w", pollCtx.Err())
		case <-exited:
			if lastErr != nil {
				return fmt.Errorf("extension exited before becoming healthy: %w", lastErr)
			}
			return errors.New("extension exited before becoming healthy")
		default:
		}

		attemptCtx, attemptCancel := context.WithTimeout(pollCtx, 1*time.Second)
		err := client.Health(attemptCtx)
		attemptCancel()
		if err == nil {
			return nil
		}
		lastErr = err

		timer := time.NewTimer(healthPollInterval)
		select {
		case <-pollCtx.Done():
			timer.Stop()
		case <-exited:
			timer.Stop()
		case <-timer.C:
		}
	}
}

func cleanBinaryEnv() []string {
	keep := []string{
		"PATH", "HOME", "USER", "LOGNAME", "TMPDIR", "TEMP", "TMP",
		"LANG", "LC_ALL", "SSL_CERT_FILE", "SSL_CERT_DIR",
		"SYSTEMROOT", "COMSPEC", "USERPROFILE", "APPDATA", "LOCALAPPDATA", "PATHEXT",
	}
	out := make([]string, 0, len(keep)+10)
	for _, k := range keep {
		if v, ok := os.LookupEnv(k); ok {
			out = append(out, k+"="+v)
		}
	}
	return out
}

func pickFreeLoopbackPort() (int, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	port := ln.Addr().(*net.TCPAddr).Port
	_ = ln.Close()
	return port, nil
}

func isClosed(ch <-chan struct{}) bool {
	select {
	case <-ch:
		return true
	default:
		return false
	}
}
