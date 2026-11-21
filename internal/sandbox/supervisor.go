package sandbox

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/ironpark/gvx-desktop/internal/rungv"
)

// Status is where a sandbox is in its life.
type Status int

const (
	Stopped  Status = iota // never run, or stopped by the user
	Starting               // the process runs; the guest is not ready yet
	Running                // the guest is ready
	Stopping               // asked to stop
	Exited                 // the guest ended by itself with code 0
	Failed                 // the guest or rungv ended with an error
)

func (s Status) String() string {
	switch s {
	case Starting:
		return "Starting"
	case Running:
		return "Running"
	case Stopping:
		return "Stopping"
	case Exited:
		return "Exited"
	case Failed:
		return "Failed"
	}
	return "Stopped"
}

// Active reports whether a process is alive in this status.
func (s Status) Active() bool { return s == Starting || s == Running || s == Stopping }

// PublishedPort is a host endpoint rungv bound for the sandbox.
type PublishedPort struct {
	Host  string // 127.0.0.1:8080, or a socket path
	Guest int
	Proto string // tcp, udp
}

// URL is an address a browser can open, or "" for a socket or UDP port.
func (p PublishedPort) URL() string {
	if p.Proto != "tcp" || strings.HasPrefix(p.Host, "/") || strings.Contains(p.Host, `\`) {
		return ""
	}
	host := p.Host
	if after, ok := strings.CutPrefix(host, "0.0.0.0:"); ok {
		host = "127.0.0.1:" + after
	}
	return "http://" + host
}

// State is a snapshot of a sandbox's runtime state.
type State struct {
	Status    Status
	PID       int
	StartedAt time.Time
	ReadyAt   time.Time
	ExitedAt  time.Time
	ExitCode  int
	Error     string // the cause of a failure
	Advice    string // what to do about it
	Platform  string // the engine rungv picked: hvf, whpx, systrap
	Ports     []PublishedPort
	Restored  string // the checkpoint bundle this run resumed, if any
	LogSeq    int64
	// Signal is the signal that ended the process, 0 when it exited by
	// itself; ExitCode is then -1.
	Signal int
}

// Equal makes State comparable for signals.
func (s State) Equal(o State) bool {
	if len(s.Ports) != len(o.Ports) {
		return false
	}
	for i := range s.Ports {
		if s.Ports[i] != o.Ports[i] {
			return false
		}
	}
	a, b := s, o
	a.Ports, b.Ports = nil, nil
	return a.Status == b.Status && a.PID == b.PID && a.StartedAt.Equal(b.StartedAt) &&
		a.ReadyAt.Equal(b.ReadyAt) && a.ExitedAt.Equal(b.ExitedAt) && a.ExitCode == b.ExitCode &&
		a.Error == b.Error && a.Advice == b.Advice && a.Platform == b.Platform &&
		a.Restored == b.Restored && a.LogSeq == b.LogSeq && a.Signal == b.Signal
}

// Event is a change the supervisor reports.
type Event struct {
	ID    string
	Kind  EventKind
	State State
	Port  string // the host address rungv could not publish, for EventPortBusy
}

// EventKind classifies an Event.
type EventKind int

const (
	EventChanged  EventKind = iota // state or logs changed
	EventReady                     // the guest became ready
	EventExited                    // the process ended
	EventPortBusy                  // rungv could not publish a port: Port names it
)

// Supervisor runs sandboxes as rungv child processes. It is the daemon gvx
// does not have: it owns each process, keeps its output, and tells the UI
// what changed. Its methods are safe for concurrent use.
type Supervisor struct {
	// Client returns the rungv client to start processes with.
	Client func() *rungv.Client
	// DataDir holds control sockets and checkpoint bundles.
	DataDir string
	// Notify receives events from the supervisor's goroutines.
	Notify func(Event)
	// Prepare, when set, writes what a spec reads from this computer as it
	// starts, before rungv runs: the files a recipe gives its guest.
	Prepare func(Spec) error
	// Output, when set, receives what the guest of every run writes, as it
	// comes and without rungv's own lines, from the goroutines reading it.
	// It must not block. The log buffers get the same output either way.
	Output func(id string, o Output)

	mu    sync.Mutex
	procs map[string]*proc
	logs  map[string]*LogBuffer
}

type proc struct {
	cmd         *exec.Cmd
	state       State
	spec        Spec
	paths       Paths
	done        chan struct{}
	stopping    bool
	interactive bool

	stdinMu     sync.Mutex
	stdinW      *os.File
	stdinClosed bool
	term        *term
	closed      bool
}

func (p *proc) closeStdio() {
	p.stdinMu.Lock()
	defer p.stdinMu.Unlock()
	if p.closed {
		return
	}
	p.closed = true
	if p.stdinW != nil && !p.stdinClosed {
		p.stdinClosed = true
		_ = p.stdinW.Close()
	}
	if p.term != nil {
		_ = p.term.close()
	}
}

var publishRe = regexp.MustCompile(`^rungv: publishing (\S+) -> (\d+)/(\w+)`)

// busyRe is what rungv's -auto-publish says of a port the guest listens on
// that this computer has taken already: it tries it once, and again only
// after the guest lets it go.
var busyRe = regexp.MustCompile(`^gvx: warning: could not auto-publish (\S+)/\w+: `)

func (s *Supervisor) notify(e Event) {
	if s.Notify != nil {
		s.Notify(e)
	}
}

// Logs returns the log buffer of a sandbox, creating it.
func (s *Supervisor) Logs(id string) *LogBuffer {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.logsLocked(id)
}

func (s *Supervisor) logsLocked(id string) *LogBuffer {
	if s.logs == nil {
		s.logs = map[string]*LogBuffer{}
	}
	b := s.logs[id]
	if b == nil {
		b = NewLogBuffer(5000)
		s.logs[id] = b
	}
	return b
}

// State returns the state of a sandbox.
func (s *Supervisor) State(id string) State {
	s.mu.Lock()
	defer s.mu.Unlock()
	if p := s.procs[id]; p != nil {
		st := p.state
		st.Ports = append([]PublishedPort(nil), st.Ports...)
		st.LogSeq = s.logsLocked(id).Seq()
		return st
	}
	return State{LogSeq: s.logsLocked(id).Seq()}
}

// ActiveSpec returns a copy of the spec the active process for id started with,
// and false when no process for id is active.
func (s *Supervisor) ActiveSpec(id string) (Spec, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if p := s.procs[id]; p != nil && p.state.Status.Active() {
		return p.spec.Clone(), true
	}
	return Spec{}, false
}

// Running lists the IDs of sandboxes with a live process.
func (s *Supervisor) Running() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var ids []string
	for id, p := range s.procs {
		if p.state.Status.Active() {
			ids = append(ids, id)
		}
	}
	sort.Strings(ids)
	return ids
}

// SnapshotRoot is the directory holding the checkpoint bundles of a sandbox.
func (s *Supervisor) SnapshotRoot(id string) string {
	return filepath.Join(s.DataDir, "snapshots", id)
}

// maxSocketPath keeps a socket path inside sockaddr_un on every host.
const maxSocketPath = 100

// pathsFor allocates the control socket and a fresh bundle directory.
func (s *Supervisor) pathsFor(spec Spec) (Paths, error) {
	if !spec.Checkpoint {
		return Paths{}, nil
	}
	runDir := filepath.Join(s.DataDir, "run")
	if err := os.MkdirAll(runDir, 0o700); err != nil {
		return Paths{}, err
	}
	dir := filepath.Join(s.SnapshotRoot(spec.ID), time.Now().Format("20060102-150405"))
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return Paths{}, err
	}
	sock := filepath.Join(runDir, spec.ID+".sock")
	if len(sock) > maxSocketPath {
		// A Unix socket path has a small limit (104 bytes on macOS).
		short := filepath.Join(os.TempDir(), "gvxd")
		if err := os.MkdirAll(short, 0o700); err != nil {
			return Paths{}, err
		}
		sock = filepath.Join(short, spec.ID+".sock")
	}
	if runtime.GOOS == "windows" {
		sock = "127.0.0.1:0"
	}
	_ = os.Remove(sock)
	return Paths{ControlSocket: sock, CheckpointDir: dir}, nil
}

// Start runs the spec. It fails when the sandbox already runs.
func (s *Supervisor) Start(spec Spec) error {
	return s.launch(spec, "")
}

// Restore resumes the checkpoint bundle in dir under the spec's sandbox.
func (s *Supervisor) Restore(spec Spec, dir string) error {
	return s.launch(spec, dir)
}

func (s *Supervisor) launch(spec Spec, restoreDir string) error {
	if err := spec.Validate(); err != nil {
		return err
	}
	client := s.Client()
	if client == nil || client.Path == "" {
		return rungv.ErrNotFound
	}
	s.mu.Lock()
	if p := s.procs[spec.ID]; p != nil && p.state.Status.Active() {
		s.mu.Unlock()
		return fmt.Errorf("%s is already running", spec.Name)
	}
	if other := s.volumeHolderLocked(spec); other != "" {
		s.mu.Unlock()
		return fmt.Errorf("a volume of %s is in use by the running sandbox %s", spec.Name, other)
	}
	s.mu.Unlock()

	if err := spec.Prepare(); err != nil {
		return err
	}
	if s.Prepare != nil {
		if err := s.Prepare(spec); err != nil {
			return err
		}
	}
	paths, err := s.pathsFor(spec)
	if err != nil {
		return err
	}
	if HasAutoPublish(spec) && !client.HasRunFlag(context.Background(), "auto-publish") {
		// An older rungv would refuse the run: it starts without, and
		// reaches this computer through the ports it publishes.
		spec = WithAutoPublish(spec, false)
	}
	usePTY := spec.TTY && ptySupported
	launchSpec := spec
	if spec.TTY && !ptySupported {
		// On Windows creack/pty is unavailable; fall back to pipes.
		launchSpec = spec.Clone()
		launchSpec.TTY = false
	}
	var args []string
	if restoreDir != "" {
		// The resumed sandbox keeps the bundle directory it was restored
		// from pre-opened, so its next snapshot rewrites that bundle.
		if paths.CheckpointDir != "" {
			_ = os.Remove(paths.CheckpointDir)
			paths.CheckpointDir = restoreDir
		}
		args = launchSpec.RestoreArgs(restoreDir, paths)
	} else {
		args = launchSpec.RunArgs(paths)
	}

	cmd := client.Command(context.Background(), args...)
	cmd.Env = os.Environ()
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return err
	}
	var (
		stdout          io.Reader
		stdinW          *os.File
		ptyTerm         *term
		closeAfterStart func()
		closeOnFail     func()
	)
	if usePTY {
		var closeSlave func()
		ptyTerm, stdout, closeSlave, err = openPTY(cmd)
		if err != nil {
			return err
		}
		closeAfterStart = closeSlave
		closeOnFail = func() {
			closeSlave()
			_ = ptyTerm.close()
		}
	} else {
		stdout, err = cmd.StdoutPipe()
		if err != nil {
			return err
		}
		// The guest reads rungv's standard input; give it a pipe that
		// stays open so non-interactive shells and servers do not see EOF
		// and quit. Spec.Interactive exposes stdinW through Supervisor.Stdin.
		stdinR, w, err := os.Pipe()
		if err != nil {
			return err
		}
		stdinW = w
		cmd.Stdin = stdinR
		closeAfterStart = func() { _ = stdinR.Close() }
		closeOnFail = func() {
			_ = stdinR.Close()
			_ = stdinW.Close()
		}
	}

	logs := s.Logs(spec.ID)
	what := "Starting"
	if restoreDir != "" {
		what = "Restoring " + filepath.Base(restoreDir)
	}
	logs.Add(System, fmt.Sprintf("── %s %s · %s", what, spec.Name, time.Now().Format("2006-01-02 15:04:05")))
	logs.Add(System, "$ rungv "+rungv.FormatArgs(args))

	if err := cmd.Start(); err != nil {
		closeOnFail()
		logs.Add(System, "failed to start: "+err.Error())
		return err
	}
	closeAfterStart()

	p := &proc{
		cmd:         cmd,
		spec:        spec.Clone(),
		paths:       paths,
		done:        make(chan struct{}),
		interactive: spec.Interactive,
		stdinW:      stdinW,
		term:        ptyTerm,
		state:       State{Status: Starting, PID: cmd.Process.Pid, StartedAt: time.Now(), Restored: restoreDir},
	}
	if spec.ReadyPattern == "" {
		p.state.Status = Running
		p.state.ReadyAt = p.state.StartedAt
	}
	s.mu.Lock()
	if s.procs == nil {
		s.procs = map[string]*proc{}
	}
	s.procs[spec.ID] = p
	s.mu.Unlock()
	s.notify(Event{ID: spec.ID, Kind: EventChanged, State: s.State(spec.ID)})

	var wg sync.WaitGroup
	wg.Add(2)
	go s.pump(spec.ID, p, stdout, Stdout, &wg)
	go s.pump(spec.ID, p, stderr, Stderr, &wg)
	go func() {
		wg.Wait()
		err := cmd.Wait()
		p.closeStdio()
		s.exited(spec.ID, p, err)
	}()
	return nil
}

// volumeHolderLocked names a running sandbox that mounts a volume of spec.
func (s *Supervisor) volumeHolderLocked(spec Spec) string {
	want := map[string]bool{}
	for _, v := range spec.Volumes() {
		want[v] = true
	}
	if len(want) == 0 {
		return ""
	}
	for id, p := range s.procs {
		if id == spec.ID || !p.state.Status.Active() {
			continue
		}
		for _, v := range p.spec.Volumes() {
			if want[v] {
				return p.spec.Name
			}
		}
	}
	return ""
}

func (s *Supervisor) pump(id string, p *proc, r io.Reader, stream Stream, wg *sync.WaitGroup) {
	defer wg.Done()
	logs := s.Logs(id)
	var split lineSplitter
	var filter rungvFilter
	emitLine := func(line string) {
		logs.Add(stream, line)
		ev := Event{ID: id, Kind: EventChanged}
		s.mu.Lock()
		if stream == Stderr {
			if m := busyRe.FindStringSubmatch(line); m != nil {
				ev.Kind, ev.Port = EventPortBusy, m[1]
			} else if m := publishRe.FindStringSubmatch(line); m != nil {
				guest, _ := strconv.Atoi(m[2])
				p.state.Ports = append(p.state.Ports, PublishedPort{Host: m[1], Guest: guest, Proto: m[3]})
			} else if v, ok := strings.CutPrefix(line, "rungv: platform="); ok {
				p.state.Platform = strings.TrimSpace(v)
			}
		}
		if p.state.Status == Starting && p.spec.ReadyPattern != "" && strings.Contains(line, p.spec.ReadyPattern) {
			p.state.Status = Running
			p.state.ReadyAt = time.Now()
			ev.Kind = EventReady
		}
		s.mu.Unlock()
		s.notify(ev)
	}
	emitOutput := func(data []byte) {
		if s.Output == nil || len(data) == 0 {
			return
		}
		cp := make([]byte, len(data))
		copy(cp, data)
		s.Output(id, Output{Stream: stream, Data: cp})
	}
	buf := make([]byte, 32*1024)
	for {
		n, err := r.Read(buf)
		if n > 0 {
			chunk := buf[:n]
			if s.Output != nil {
				if stream == Stderr {
					emitOutput(filter.write(chunk))
				} else {
					emitOutput(chunk)
				}
			}
			split.write(chunk, emitLine)
		}
		if err != nil {
			// On Linux, reading the PTY master returns EIO once the slave closes.
			if stream == Stdout && p.term != nil && isPTYEIO(err) {
				break
			}
			break
		}
	}
	if s.Output != nil && stream == Stderr {
		emitOutput(filter.flush())
	}
	split.flush(emitLine)
}

func (s *Supervisor) exited(id string, p *proc, err error) {
	logs := s.Logs(id)
	s.mu.Lock()
	code := 0
	if p.cmd.ProcessState != nil {
		code = p.cmd.ProcessState.ExitCode()
	}
	p.state.ExitCode = code
	p.state.Signal = waitSignal(p.cmd.ProcessState)
	p.state.ExitedAt = time.Now()
	p.state.PID = 0
	switch {
	case p.stopping:
		p.state.Status = Stopped
	case err == nil && code == 0:
		p.state.Status = Exited
	default:
		p.state.Status = Failed
		tail := logs.Tail(8, Stderr)
		msg := ""
		for _, t := range slices.Backward(tail) {
			if t := strings.TrimSpace(t); t != "" && !publishRe.MatchString(t) {
				msg = t
				break
			}
		}
		if msg == "" && err != nil {
			msg = err.Error()
		}
		p.state.Error = msg
		p.state.Advice = rungv.Diagnose(strings.Join(tail, "\n"), code)
	}
	if p.paths.ControlSocket != "" && !strings.Contains(p.paths.ControlSocket, ":") {
		_ = os.Remove(p.paths.ControlSocket)
	}
	// rungv opens the bundle files before the lockdown; drop the bundle
	// directory of a run that never wrote a snapshot into it.
	if p.paths.CheckpointDir != "" && p.state.Restored == "" && !bundleComplete(p.paths.CheckpointDir) {
		_ = os.RemoveAll(p.paths.CheckpointDir)
	}
	st := p.state
	close(p.done)
	s.mu.Unlock()

	switch st.Status {
	case Stopped:
		logs.Add(System, "── Stopped")
	case Exited:
		logs.Add(System, "── Exited with code 0")
	default:
		logs.Add(System, fmt.Sprintf("── Failed with code %d", st.ExitCode))
	}
	s.notify(Event{ID: id, Kind: EventExited, State: s.State(id)})
}

// Stop asks a sandbox to end with an interrupt, as Ctrl-C in a terminal
// does, and kills it when it has not ended after timeout.
func (s *Supervisor) Stop(id string, timeout time.Duration) error {
	s.mu.Lock()
	p := s.procs[id]
	if p == nil || !p.state.Status.Active() {
		s.mu.Unlock()
		return nil
	}
	p.stopping = true
	p.state.Status = Stopping
	proc := p.cmd.Process
	done := p.done
	s.mu.Unlock()
	s.notify(Event{ID: id, Kind: EventChanged})
	s.Logs(id).Add(System, "── Stopping…")

	if runtime.GOOS == "windows" {
		_ = proc.Kill()
	} else if err := proc.Signal(os.Interrupt); err != nil {
		_ = proc.Kill()
	}
	select {
	case <-done:
		return nil
	case <-time.After(timeout):
		_ = proc.Kill()
		<-done
		return nil
	}
}

// StopAll stops every running sandbox in parallel.
func (s *Supervisor) StopAll(timeout time.Duration) {
	var wg sync.WaitGroup
	for _, id := range s.Running() {
		wg.Go(func() {
			_ = s.Stop(id, timeout)
		})
	}
	wg.Wait()
}

// Forget drops what the supervisor keeps of a stopped sandbox.
func (s *Supervisor) Forget(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if p := s.procs[id]; p != nil && p.state.Status.Active() {
		return
	}
	delete(s.procs, id)
	delete(s.logs, id)
}

// ErrNoCheckpoint reports a sandbox started without checkpoint support.
var ErrNoCheckpoint = errors.New("the sandbox was started without snapshot support; enable it in the sandbox settings and restart")

// Checkpoint writes a snapshot of a running sandbox. Without leaveRunning
// the sandbox stops once the bundle is written. It returns the bundle
// directory.
func (s *Supervisor) Checkpoint(ctx context.Context, id string, leaveRunning bool) (string, error) {
	s.mu.Lock()
	p := s.procs[id]
	if p == nil || !p.state.Status.Active() {
		s.mu.Unlock()
		return "", errors.New("the sandbox is not running")
	}
	if p.paths.ControlSocket == "" {
		s.mu.Unlock()
		return "", ErrNoCheckpoint
	}
	paths := p.paths
	if !leaveRunning {
		p.stopping = true
	}
	s.mu.Unlock()
	s.Logs(id).Add(System, "── Writing snapshot to "+paths.CheckpointDir)
	err := s.Client().Checkpoint(ctx, paths.ControlSocket, paths.CheckpointDir, leaveRunning)
	if err != nil {
		s.mu.Lock()
		p.stopping = false
		s.mu.Unlock()
		s.Logs(id).Add(System, "snapshot failed: "+err.Error())
		return "", err
	}
	s.Logs(id).Add(System, "── Snapshot written")
	s.notify(Event{ID: id, Kind: EventChanged})
	return paths.CheckpointDir, nil
}

// Snapshot is a checkpoint bundle on disk.
type Snapshot struct {
	SandboxID string
	Dir       string
	Name      string
	Time      time.Time
	Size      int64
}

// Snapshots lists the complete bundles of a sandbox, newest first.
func (s *Supervisor) Snapshots(id string) []Snapshot {
	root := s.SnapshotRoot(id)
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil
	}
	var out []Snapshot
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		dir := filepath.Join(root, e.Name())
		if !bundleComplete(dir) {
			continue
		}
		snap := Snapshot{SandboxID: id, Dir: dir, Name: e.Name()}
		if t, err := time.ParseInLocation("20060102-150405", e.Name(), time.Local); err == nil {
			snap.Time = t
		} else if info, err := e.Info(); err == nil {
			snap.Time = info.ModTime()
		}
		files, _ := os.ReadDir(dir)
		for _, f := range files {
			if info, err := f.Info(); err == nil {
				snap.Size += info.Size()
			}
		}
		out = append(out, snap)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Time.After(out[j].Time) })
	return out
}

// bundleComplete reports whether dir holds a written checkpoint bundle:
// rungv creates the files empty when the run starts, and fills them on a
// checkpoint.
func bundleComplete(dir string) bool {
	st, err := os.Stat(filepath.Join(dir, "manifest.json"))
	return err == nil && st.Size() > 0
}
