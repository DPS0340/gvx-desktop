package engine

// Owner: W2 (sandbox engine, docs/design/gvx-v020.md §5.2).

import (
	"bufio"
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ironpark/gvx-desktop/internal/overlay/guestcfg"
	"github.com/ironpark/gvx-desktop/internal/rungv"
	"github.com/ironpark/gvx-desktop/internal/sandbox"
)

// execGCDelay is how long an exited exec record stays inspectable before
// being removed from memory. Tests may shorten it.
var execGCDelay = 5 * time.Minute

type execState struct {
	mu     sync.Mutex
	byID   map[string]*execRecord
	closed bool
}

type execRecord struct {
	ID          string
	ContainerID string
	Config      execCreateRequest
	OpenStdin   bool
	OpenStdout  bool
	OpenStderr  bool
	Started     bool
	Running     bool
	ExitCode    *int
	Pid         int
	CanRemove   bool

	proc    sandbox.ExecProcess
	gcTimer *time.Timer
}

type execCreateRequest struct {
	AttachStdin  bool     `json:"AttachStdin"`
	AttachStdout bool     `json:"AttachStdout"`
	AttachStderr bool     `json:"AttachStderr"`
	DetachKeys   string   `json:"DetachKeys"`
	Tty          bool     `json:"Tty"`
	ConsoleSize  *[2]uint `json:"ConsoleSize"`
	Env          []string `json:"Env"`
	Cmd          []string `json:"Cmd"`
	Privileged   bool     `json:"Privileged"`
	User         string   `json:"User"`
	WorkingDir   string   `json:"WorkingDir"`
}

type execStartRequest struct {
	Detach      bool     `json:"Detach"`
	Tty         bool     `json:"Tty"`
	ConsoleSize *[2]uint `json:"ConsoleSize"`
}

type execInspectResponse struct {
	ID            string            `json:"ID"`
	Running       bool              `json:"Running"`
	ExitCode      *int              `json:"ExitCode"`
	ProcessConfig execProcessConfig `json:"ProcessConfig"`
	OpenStdin     bool              `json:"OpenStdin"`
	OpenStderr    bool              `json:"OpenStderr"`
	OpenStdout    bool              `json:"OpenStdout"`
	CanRemove     bool              `json:"CanRemove"`
	ContainerID   string            `json:"ContainerID"`
	DetachKeys    string            `json:"DetachKeys"`
	Pid           int               `json:"Pid"`
}

type execProcessConfig struct {
	Tty        bool     `json:"tty"`
	Entrypoint string   `json:"entrypoint"`
	Arguments  []string `json:"arguments"`
	Privileged bool     `json:"privileged"`
	User       string   `json:"user"`
}

func (e *Engine) execIDsForContainer(cid string) []string {
	e.exc.mu.Lock()
	defer e.exc.mu.Unlock()
	var ids []string
	for id, rec := range e.exc.byID {
		if rec.ContainerID == cid {
			ids = append(ids, id)
		}
	}
	if len(ids) == 0 {
		return nil
	}
	slices.Sort(ids)
	return ids
}

func (e *Engine) dropExecsForContainer(cid string) {
	var procs []sandbox.ExecProcess
	e.exc.mu.Lock()
	for id, rec := range e.exc.byID {
		if rec.ContainerID != cid {
			continue
		}
		if rec.gcTimer != nil {
			rec.gcTimer.Stop()
			rec.gcTimer = nil
		}
		if rec.proc != nil {
			procs = append(procs, rec.proc)
			rec.proc = nil
		}
		delete(e.exc.byID, id)
	}
	e.exc.mu.Unlock()
	for _, p := range procs {
		_ = p.Close()
	}
}

func (e *Engine) onContainerExitedExecs(cid string) {
	e.exc.mu.Lock()
	defer e.exc.mu.Unlock()
	for id, rec := range e.exc.byID {
		if rec.ContainerID != cid || rec.Running {
			continue
		}
		if rec.gcTimer != nil {
			rec.gcTimer.Stop()
		}
		execID := id
		rec.gcTimer = time.AfterFunc(execGCDelay, func() {
			e.gcExec(execID)
		})
	}
}

func (e *Engine) gcExec(execID string) {
	e.exc.mu.Lock()
	if rec, ok := e.exc.byID[execID]; ok && !rec.Running {
		rec.gcTimer = nil
		delete(e.exc.byID, execID)
	}
	e.exc.mu.Unlock()
}

func (e *Engine) openExecs() {
	e.exc.mu.Lock()
	e.exc.closed = false
	e.exc.mu.Unlock()
}

func (e *Engine) closeExecs() {
	var procs []sandbox.ExecProcess
	e.exc.mu.Lock()
	e.exc.closed = true
	for id, rec := range e.exc.byID {
		if rec.gcTimer != nil {
			rec.gcTimer.Stop()
			rec.gcTimer = nil
		}
		if rec.proc != nil {
			procs = append(procs, rec.proc)
			rec.proc = nil
		}
		delete(e.exc.byID, id)
	}
	e.exc.mu.Unlock()
	for _, p := range procs {
		_ = p.Close()
	}
}

// mergeExecEnv overlays override onto base, dropping entries without '=' or
// with empty/whitespace keys so the host app's environment never leaks.
func mergeExecEnv(base, override []string) []string {
	var out []string
	index := map[string]int{}
	add := func(entries []string) {
		for _, entry := range entries {
			k, _, ok := strings.Cut(entry, "=")
			if !ok || strings.TrimSpace(k) == "" || strings.ContainsAny(k, " \t") {
				continue
			}
			if idx, exists := index[k]; exists {
				out[idx] = entry
			} else {
				index[k] = len(out)
				out = append(out, entry)
			}
		}
	}
	add(base)
	add(override)
	return out
}

func (e *Engine) execCreate(w http.ResponseWriter, r *http.Request) {
	if !e.runner.Features().Control {
		writeError(w, r, errNotSupported("exec"))
		return
	}
	c, err := e.lookup(r.PathValue("id"))
	if err != nil {
		writeError(w, r, err)
		return
	}
	if _, err := e.requireControlForExec(c); err != nil {
		writeError(w, r, err)
		return
	}

	var req execCreateRequest
	if err := decodeBody(r, &req); err != nil {
		writeError(w, r, err)
		return
	}
	if len(req.Cmd) == 0 {
		writeError(w, r, errInvalid("No exec command specified"))
		return
	}
	if req.Tty && req.DetachKeys != "" {
		if _, err := attachParseDetachKeys(req.DetachKeys); err != nil {
			writeError(w, r, err)
			return
		}
	}

	execID := randomHex64()
	rec := &execRecord{
		ID:          execID,
		ContainerID: c.ID,
		Config:      req,
		OpenStdin:   req.AttachStdin,
		OpenStdout:  req.AttachStdout,
		OpenStderr:  req.AttachStderr,
	}
	e.exc.mu.Lock()
	if e.exc.byID == nil {
		e.exc.byID = map[string]*execRecord{}
	}
	e.exc.byID[execID] = rec
	e.exc.mu.Unlock()

	attrs := containerAttrs(&c)
	attrs["execID"] = execID
	e.emit(Event{
		Type:       "container",
		Action:     "exec_create: " + strings.Join(req.Cmd, " "),
		ID:         c.ID,
		Attributes: attrs,
	})

	writeJSON(w, http.StatusCreated, struct {
		ID string `json:"Id"`
	}{ID: execID})
}

func (e *Engine) requireControlForExec(c Container) (sandbox.Control, error) {
	if c.State.Status == StatusRestarting {
		return nil, errConflict("Container %s is restarting, wait until the container is running", c.ID)
	}
	if c.State.Status != StatusRunning {
		return nil, errConflict("Container %s is not running", c.ID)
	}
	if e.isContainerPaused(c) {
		return nil, errConflict("Container %s is paused, unpause the container before exec", c.ID)
	}
	ctl := e.runner.Control(c.ID)
	if ctl == nil {
		return nil, errConflict("Container %s was started without a control socket; restart it to use exec", c.ID)
	}
	return ctl, nil
}

func (e *Engine) execStart(w http.ResponseWriter, r *http.Request) {
	if !e.runner.Features().Control {
		writeError(w, r, errNotSupported("exec"))
		return
	}
	execID := r.PathValue("id")
	var req execStartRequest
	if err := decodeBody(r, &req); err != nil {
		writeError(w, r, err)
		return
	}

	e.exc.mu.Lock()
	rec, ok := e.exc.byID[execID]
	if !ok {
		e.exc.mu.Unlock()
		writeError(w, r, errNotFound("No such exec instance: %s", execID))
		return
	}
	if rec.Started {
		e.exc.mu.Unlock()
		writeError(w, r, errConflict("Error: Exec command %s is already running", execID))
		return
	}
	rec.Started = true
	cid := rec.ContainerID
	cfg := rec.Config
	e.exc.mu.Unlock()

	revertStarted := func() {
		e.exc.mu.Lock()
		rec.Started = false
		e.exc.mu.Unlock()
	}

	c, err := e.lookup(cid)
	if err != nil {
		revertStarted()
		writeError(w, r, err)
		return
	}
	ctl, err := e.requireControlForExec(c)
	if err != nil {
		revertStarted()
		writeError(w, r, err)
		return
	}

	detachedStart := req.Detach || (!cfg.AttachStdin && !cfg.AttachStdout && !cfg.AttachStderr)
	tty := !detachedStart && (cfg.Tty || req.Tty)
	var detachSeq []byte
	if tty {
		if cfg.DetachKeys == "" {
			detachSeq = []byte{16, 17} // ctrl-p, ctrl-q
		} else {
			detachSeq, err = attachParseDetachKeys(cfg.DetachKeys)
			if err != nil {
				revertStarted()
				writeError(w, r, err)
				return
			}
		}
	}

	// Build exec environment: container Config.Env + request Env, blanking
	// GVX_NET for containers started with netinit so the WireGuard private
	// key from the sandbox spec never leaks into exec processes.
	baseEnv := slices.Clone(c.Config.Env)
	if e.startedWithNetinit(c.ID) {
		baseEnv = append(baseEnv, guestcfg.EnvVar+"=")
	}
	env := mergeExecEnv(baseEnv, cfg.Env)

	// Always pass User explicitly from request or container Config.User,
	// because a netinit container's sandbox spec runs as root while netinit
	// drops to Config.User.
	user := cfg.User
	if user == "" {
		user = c.Config.User
	}
	workdir := cfg.WorkingDir
	if workdir == "" {
		workdir = c.Config.WorkingDir
	}
	// Pass Privileged=&true only when the exec request or container explicitly
	// asks for privileged mode; otherwise leave it nil so rungv uses its
	// default (UID 0 keeps guest root caps bounded by the sandbox, non-root
	// gets none). Passing &false would strip all capabilities even from root.
	var privPtr *bool
	if cfg.Privileged || c.HostConfig.Privileged {
		t := true
		privPtr = &t
	}

	var rows, cols uint16
	size := req.ConsoleSize
	if size == nil || (size[0] == 0 && size[1] == 0) {
		size = cfg.ConsoleSize
	}
	if size != nil {
		rows = uint16(min(size[0], 65535))
		cols = uint16(min(size[1], 65535))
	}

	opts := sandbox.ExecOptions{
		ExecOptions: rungv.ExecOptions{
			Argv:        slices.Clone(cfg.Cmd),
			Env:         env,
			User:        user,
			Workdir:     workdir,
			Interactive: !detachedStart && cfg.AttachStdin,
			TTY:         tty,
			Detach:      false, // keep non-detached so Running/ExitCode stay accurate
			Privileged:  privPtr,
		},
		Rows: rows,
		Cols: cols,
	}

	proc, err := ctl.Exec(context.Background(), opts)
	if err != nil {
		revertStarted()
		if errors.Is(err, sandbox.ErrPaused) || controlCode(err) == rungv.CodeAlreadyPaused {
			writeError(w, r, errConflict("Container %s is paused, unpause the container before exec", c.ID))
			return
		}
		if errors.Is(err, sandbox.ErrNoControl) {
			writeError(w, r, errConflict("Container %s was started without a control socket; restart it to use exec", c.ID))
			return
		}
		writeError(w, r, err)
		return
	}

	// Re-check under lock in case container removal or Engine.Close dropped
	// rec while ctl.Exec was running.
	e.exc.mu.Lock()
	if e.exc.closed || e.exc.byID[execID] != rec {
		closed := e.exc.closed
		e.exc.mu.Unlock()
		_ = proc.Close()
		go func() {
			_, _ = proc.Wait()
			_ = proc.Close()
		}()
		if closed {
			writeError(w, r, &apiError{http.StatusServiceUnavailable, "GVX Desktop's sandbox engine is not running."})
		} else {
			writeError(w, r, errNotFound("No such exec instance: %s", execID))
		}
		return
	}
	rec.Running = true
	rec.proc = proc
	rec.Config.Tty = tty
	if pid := proc.PID(); pid > 0 {
		rec.Pid = pid
	}
	e.exc.mu.Unlock()

	go func() {
		if ch := proc.Started(); ch != nil {
			<-ch
		}
		if pid := proc.PID(); pid > 0 {
			e.exc.mu.Lock()
			rec.Pid = pid
			e.exc.mu.Unlock()
		}
	}()

	startAttrs := containerAttrs(&c)
	startAttrs["execID"] = execID
	e.emit(Event{
		Type:       "container",
		Action:     "exec_start: " + strings.Join(cfg.Cmd, " "),
		ID:         c.ID,
		Attributes: startAttrs,
	})

	if detachedStart {
		w.WriteHeader(http.StatusOK)
		go e.runDetachedExec(rec, c, proc)
		return
	}

	conn, rw, multiplex, err := hijackStream(w, r, tty)
	if err != nil {
		_ = proc.Close()
		go e.runDetachedExec(rec, c, proc)
		writeError(w, r, err)
		return
	}
	e.serveAttachedExec(rec, c, proc, conn, rw, multiplex, tty, opts.Interactive, cfg.AttachStdout, cfg.AttachStderr, detachSeq)
}

func (e *Engine) runDetachedExec(rec *execRecord, c Container, proc sandbox.ExecProcess) {
	defer func() { _ = proc.Close() }()
	var outWG sync.WaitGroup
	if r := proc.Stdout(); r != nil {
		outWG.Add(1)
		go func() {
			defer outWG.Done()
			_, _ = io.Copy(io.Discard, r)
		}()
	}
	if r := proc.Stderr(); r != nil {
		outWG.Add(1)
		go func() {
			defer outWG.Done()
			_, _ = io.Copy(io.Discard, r)
		}()
	}
	code, err := proc.Wait()
	outWG.Wait()
	exitCode, _ := mapExecExit(proc.PID(), code, err)
	e.finishExec(rec, c, exitCode)
}

func (e *Engine) serveAttachedExec(
	rec *execRecord,
	c Container,
	proc sandbox.ExecProcess,
	conn net.Conn,
	rw *bufio.ReadWriter,
	multiplex bool,
	tty bool,
	interactive bool,
	attachStdout bool,
	attachStderr bool,
	detachSeq []byte,
) {
	var (
		writeMu     sync.Mutex
		detached    atomic.Bool
		writeFailed atomic.Bool
		clientDone  = make(chan struct{})
		execDone    = make(chan struct{})
		inDone      = make(chan struct{})
		stopOnce    sync.Once
	)
	stopClient := func() {
		stopOnce.Do(func() {
			close(clientDone)
			_ = conn.SetReadDeadline(time.Now())
		})
	}

	writeFrame := func(stream byte, data []byte) error {
		if len(data) == 0 {
			return nil
		}
		writeMu.Lock()
		defer writeMu.Unlock()
		if detached.Load() || writeFailed.Load() {
			return nil
		}
		var err error
		if multiplex {
			err = streamWriteMuxFrame(conn, stream, data)
		} else {
			_, err = conn.Write(data)
		}
		if err != nil {
			writeFailed.Store(true)
		}
		return err
	}

	var outWG sync.WaitGroup
	pumpOut := func(r io.Reader, stream byte, forward bool) {
		defer outWG.Done()
		buf := make([]byte, 32*1024)
		for {
			n, err := r.Read(buf)
			if n > 0 && forward && !detached.Load() && !writeFailed.Load() {
				_ = writeFrame(stream, buf[:n])
			}
			if err != nil {
				return
			}
		}
	}

	if r := proc.Stdout(); r != nil {
		outWG.Add(1)
		go pumpOut(r, 1, attachStdout || tty)
	}
	if !tty {
		if r := proc.Stderr(); r != nil {
			outWG.Add(1)
			go pumpOut(r, 2, attachStderr)
		}
	}

	// Wait runs in its own goroutine so detaching or an HTTP peer disconnect
	// releases the client connection immediately while keeping the rungv exec
	// client alive draining to io.Discard until the guest command exits. After
	// both output and stdin pumps finish, proc.Close() releases the PTY/pipes.
	go func() {
		defer func() {
			<-inDone
			_ = proc.Close()
		}()
		defer close(execDone)
		code, waitErr := proc.Wait()
		outWG.Wait()
		exitCode, errMsg := mapExecExit(proc.PID(), code, waitErr)
		if errMsg != "" && !detached.Load() && !writeFailed.Load() {
			_ = writeFrame(2, []byte(errMsg))
		}
		// Record exit status BEFORE closing the client stream so GET
		// /exec/{id}/json right after EOF always sees Running:false and
		// the final ExitCode.
		e.finishExec(rec, c, exitCode)
		if !detached.Load() {
			attachCloseWriteConn(conn)
		}
	}()

	go func() {
		defer close(inDone)
		if interactive && proc.Stdin() != nil {
			e.execCopyStdin(proc, conn, rw.Reader, detachSeq, clientDone, &detached, stopClient)
			return
		}
		if attachWaitPeerClose(conn, clientDone) {
			// Treat peer disconnect like detach: leave the rungv exec client
			// running and draining until the guest command exits.
			detached.Store(true)
			stopClient()
		}
	}()

	select {
	case <-execDone:
		stopClient()
		<-inDone
		_ = conn.Close()
	case <-clientDone:
		<-inDone
		_ = conn.Close()
	}
}

func (e *Engine) execCopyStdin(
	proc sandbox.ExecProcess,
	conn net.Conn,
	br *bufio.Reader,
	detachSeq []byte,
	clientDone <-chan struct{},
	detached *atomic.Bool,
	stopClient func(),
) {
	stdinW := proc.Stdin()
	buf := make([]byte, 32*1024)
	matched := 0
	for {
		n, err := br.Read(buf)
		if n > 0 {
			chunk := buf[:n]
			if len(detachSeq) > 0 {
				var out []byte
				for _, b := range chunk {
					if b == detachSeq[matched] {
						matched++
						if matched == len(detachSeq) {
							if len(out) > 0 {
								_, _ = stdinW.Write(out)
							}
							// Detach the client only; keep the guest exec
							// running and its output drained.
							detached.Store(true)
							stopClient()
							return
						}
						continue
					}
					if matched > 0 {
						out = append(out, detachSeq[:matched]...)
						matched = 0
						if b == detachSeq[0] {
							matched = 1
							continue
						}
					}
					out = append(out, b)
				}
				if len(out) > 0 {
					if _, werr := stdinW.Write(out); werr != nil {
						return
					}
				}
			} else if _, werr := stdinW.Write(chunk); werr != nil {
				return
			}
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				if matched > 0 {
					_, _ = stdinW.Write(detachSeq[:matched])
				}
				_ = stdinW.Close()
				// Client half-closed stdin; keep output flowing unless the
				// peer also closed its read side, in which case detach cleanly
				// without interrupting the guest command.
				if attachWaitPeerClose(conn, clientDone) {
					detached.Store(true)
					stopClient()
				}
				return
			}
			// Peer disconnected abruptly: treat like detach so the guest
			// command keeps running and Running/ExitCode remain accurate.
			detached.Store(true)
			stopClient()
			return
		}
	}
}

func (e *Engine) finishExec(rec *execRecord, c Container, exitCode int) {
	code := exitCode
	e.exc.mu.Lock()
	if cur, ok := e.exc.byID[rec.ID]; ok && cur == rec {
		rec.Running = false
		rec.ExitCode = &code
		rec.proc = nil
		if rec.gcTimer != nil {
			rec.gcTimer.Stop()
		}
		execID := rec.ID
		rec.gcTimer = time.AfterFunc(execGCDelay, func() {
			e.gcExec(execID)
		})
	}
	e.exc.mu.Unlock()

	attrs := containerAttrs(&c)
	attrs["execID"] = rec.ID
	attrs["exitCode"] = strconv.Itoa(exitCode)
	e.emit(Event{
		Type:       "container",
		Action:     "exec_die",
		ID:         c.ID,
		Attributes: attrs,
	})
	e.notify()
}

// mapExecExit translates ExecProcess.Wait's (code, err) into Docker's exit
// code and optional OCI runtime failure message (§5.2).
func mapExecExit(pid int, code int, err error) (exitCode int, errMsg string) {
	if err == nil {
		return code, ""
	}
	if pid == 0 {
		msg := execErrorMessage(err)
		lower := strings.ToLower(msg)
		exitCode = 126
		if controlCode(err) == rungv.CodeNotFound || strings.Contains(lower, "not found") || strings.Contains(lower, "no such file") {
			exitCode = 127
		}
		return exitCode, "OCI runtime exec failed: " + msg + "\n"
	}
	// Failed after the guest process started (e.g. sandbox exited under it).
	return 137, ""
}

func execErrorMessage(err error) string {
	if re, ok := errors.AsType[*rungv.Error](err); ok {
		if line := lastNonEmptyLine(re.Stderr); line != "" {
			return line
		}
	}
	return err.Error()
}

func (e *Engine) execResize(w http.ResponseWriter, r *http.Request) {
	if !e.runner.Features().Control {
		writeError(w, r, errNotSupported("exec"))
		return
	}
	h, err := queryInt(r, "h", 0)
	if err != nil || h < 0 || h > 65535 {
		writeError(w, r, errInvalid("invalid height"))
		return
	}
	width, err := queryInt(r, "w", 0)
	if err != nil || width < 0 || width > 65535 {
		writeError(w, r, errInvalid("invalid width"))
		return
	}
	execID := r.PathValue("id")
	e.exc.mu.Lock()
	rec, ok := e.exc.byID[execID]
	if !ok {
		e.exc.mu.Unlock()
		writeError(w, r, errNotFound("No such exec instance: %s", execID))
		return
	}
	running := rec.Running
	tty := rec.Config.Tty
	proc := rec.proc
	e.exc.mu.Unlock()

	if !running || proc == nil {
		writeError(w, r, errConflict("Exec %s is not running", execID))
		return
	}
	if !tty {
		w.WriteHeader(http.StatusOK)
		return
	}
	if err := proc.Resize(uint16(h), uint16(width)); err != nil {
		if runtime.GOOS == "windows" {
			w.WriteHeader(http.StatusOK)
			return
		}
		writeError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusOK)
}

func (e *Engine) execInspect(w http.ResponseWriter, r *http.Request) {
	if !e.runner.Features().Control {
		writeError(w, r, errNotSupported("exec"))
		return
	}
	execID := r.PathValue("id")
	e.exc.mu.Lock()
	rec, ok := e.exc.byID[execID]
	if !ok {
		e.exc.mu.Unlock()
		writeError(w, r, errNotFound("No such exec instance: %s", execID))
		return
	}
	entrypoint := ""
	args := []string{}
	if len(rec.Config.Cmd) > 0 {
		entrypoint = rec.Config.Cmd[0]
		if len(rec.Config.Cmd) > 1 {
			args = slices.Clone(rec.Config.Cmd[1:])
		}
	}
	pid := rec.Pid
	if pid == 0 && rec.proc != nil {
		pid = rec.proc.PID()
	}
	var exitCode *int
	if rec.ExitCode != nil {
		v := *rec.ExitCode
		exitCode = &v
	}
	resp := execInspectResponse{
		ID:       rec.ID,
		Running:  rec.Running,
		ExitCode: exitCode,
		ProcessConfig: execProcessConfig{
			Tty:        rec.Config.Tty,
			Entrypoint: entrypoint,
			Arguments:  args,
			Privileged: rec.Config.Privileged,
			User:       rec.Config.User,
		},
		OpenStdin:   rec.OpenStdin,
		OpenStderr:  rec.OpenStderr,
		OpenStdout:  rec.OpenStdout,
		CanRemove:   rec.CanRemove,
		ContainerID: rec.ContainerID,
		DetachKeys:  rec.Config.DetachKeys,
		Pid:         pid,
	}
	e.exc.mu.Unlock()
	writeJSON(w, http.StatusOK, resp)
}
