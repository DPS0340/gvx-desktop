package sandbox

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
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
	if strings.HasPrefix(host, "0.0.0.0:") {
		host = "127.0.0.1:" + strings.TrimPrefix(host, "0.0.0.0:")
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
		a.Restored == b.Restored && a.LogSeq == b.LogSeq
}

// Event is a change the supervisor reports.
type Event struct {
	ID    string
	Kind  EventKind
	State State
}

// EventKind classifies an Event.
type EventKind int

const (
	EventChanged EventKind = iota // state or logs changed
	EventReady                    // the guest became ready
	EventExited                   // the process ended
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

	mu    sync.Mutex
	procs map[string]*proc
	logs  map[string]*LogBuffer
}

type proc struct {
	cmd      *exec.Cmd
	state    State
	spec     Spec
	paths    Paths
	done     chan struct{}
	stopping bool
}

var publishRe = regexp.MustCompile(`^rungv: publishing (\S+) -> (\d+)/(\w+)`)

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
	paths, err := s.pathsFor(spec)
	if err != nil {
		return err
	}
	var args []string
	if restoreDir != "" {
		// The resumed sandbox keeps the bundle directory it was restored
		// from pre-opened, so its next snapshot rewrites that bundle.
		if paths.CheckpointDir != "" {
			_ = os.Remove(paths.CheckpointDir)
			paths.CheckpointDir = restoreDir
		}
		args = spec.RestoreArgs(restoreDir, paths)
	} else {
		args = spec.RunArgs(paths)
	}

	cmd := client.Command(context.Background(), args...)
	cmd.Env = os.Environ()
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return err
	}
	// The guest reads rungv's standard input; give it an empty one that
	// stays open so shells and servers do not see EOF and quit.
	stdinR, stdinW, err := os.Pipe()
	if err != nil {
		return err
	}
	cmd.Stdin = stdinR

	logs := s.Logs(spec.ID)
	what := "Starting"
	if restoreDir != "" {
		what = "Restoring " + filepath.Base(restoreDir)
	}
	logs.Add(System, fmt.Sprintf("── %s %s · %s", what, spec.Name, time.Now().Format("2006-01-02 15:04:05")))
	logs.Add(System, "$ rungv "+rungv.FormatArgs(args))

	if err := cmd.Start(); err != nil {
		stdinR.Close()
		stdinW.Close()
		logs.Add(System, "failed to start: "+err.Error())
		return err
	}
	stdinR.Close()

	p := &proc{
		cmd:   cmd,
		spec:  spec,
		paths: paths,
		done:  make(chan struct{}),
		state: State{Status: Starting, PID: cmd.Process.Pid, StartedAt: time.Now(), Restored: restoreDir},
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
		stdinW.Close()
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
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64*1024), 1024*1024)
	for sc.Scan() {
		line := strings.TrimRight(sc.Text(), "\r")
		logs.Add(stream, line)
		kind := EventChanged
		s.mu.Lock()
		if stream == Stderr {
			if m := publishRe.FindStringSubmatch(line); m != nil {
				guest, _ := strconv.Atoi(m[2])
				p.state.Ports = append(p.state.Ports, PublishedPort{Host: m[1], Guest: guest, Proto: m[3]})
			} else if v, ok := strings.CutPrefix(line, "rungv: platform="); ok {
				p.state.Platform = strings.TrimSpace(v)
			}
		}
		if p.state.Status == Starting && p.spec.ReadyPattern != "" && strings.Contains(line, p.spec.ReadyPattern) {
			p.state.Status = Running
			p.state.ReadyAt = time.Now()
			kind = EventReady
		}
		s.mu.Unlock()
		s.notify(Event{ID: id, Kind: kind})
	}
}

func (s *Supervisor) exited(id string, p *proc, err error) {
	logs := s.Logs(id)
	s.mu.Lock()
	code := 0
	if p.cmd.ProcessState != nil {
		code = p.cmd.ProcessState.ExitCode()
	}
	p.state.ExitCode = code
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
		for i := len(tail) - 1; i >= 0; i-- {
			if t := strings.TrimSpace(tail[i]); t != "" && !publishRe.MatchString(t) {
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
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = s.Stop(id, timeout)
		}()
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
