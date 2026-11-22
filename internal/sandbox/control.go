package sandbox

// Owner: W1 (rungv + sandbox layer). Design: docs/design/gvx-v020.md §4.
//
// The runtime control of a running sandbox over its control socket
// (rungv v0.20). The Control and ExecProcess interfaces, ErrNoControl, and
// the Supervisor methods in this file are frozen seams: the engine (W2)
// and the UI (W3) code against them, with fakes of the two interfaces in
// their own tests. Bodies are W1's.

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"runtime"
	"strings"
	"time"

	"github.com/ironpark/gvx-desktop/internal/rungv"
)

// ErrNoControl reports a run without a control socket: an older rungv, or
// a run started before the binary was upgraded. State.Control is false.
var ErrNoControl = errors.New("the sandbox was started without a control socket; restart it with rungv v0.20 or later")

// ErrPaused reports an operation a paused sandbox refuses (exec, cp).
var ErrPaused = errors.New("the sandbox is paused")

// Control drives one running sandbox through its control socket. It is
// bound to one run: once that run ends, every method fails (with
// ErrNotRunning-like errors from rungv: rungv.IsUnreachable).
//
// Lifecycle changes that the supervisor must know of (pause, unpause,
// update, graceful stop) are Supervisor methods, not Control methods.
type Control interface {
	// Target is the run's control socket, for callers that run rungv
	// themselves.
	Target() rungv.Target
	// ExecArgv is the full argument vector, rungv executable first, of
	// rungv exec in this run: what the UI's terminal starts on its own
	// PTY with internal/term (term.Start), as it does kubectl exec -it.
	ExecArgv(o rungv.ExecOptions) []string
	// Exec starts a command in the guest and returns at once. With
	// o.TTY on Unix, rungv exec runs on a PTY of the supervisor's
	// (raw mode, sized to o's Rows/Cols before it starts) so Resize
	// reaches the guest; on Windows it runs on pipes and Resize fails.
	// Exec fails with ErrPaused when the run is paused.
	Exec(ctx context.Context, o ExecOptions) (ExecProcess, error)
	// CopyFrom, CopyTo, StatPath and Export are rungv.Client's, bound to
	// this run.
	CopyFrom(ctx context.Context, guestPath string, o rungv.CopyOptions) (io.ReadCloser, error)
	CopyTo(ctx context.Context, guestDir string, r io.Reader, o rungv.CopyOptions) error
	StatPath(ctx context.Context, guestPath string) (rungv.PathStat, error)
	Export(ctx context.Context, guestPath string, upperOnly bool) (io.ReadCloser, error)
	// Stats takes one sample of the guest's counters.
	Stats(ctx context.Context) (rungv.Stats, error)
	// StreamStats samples every interval until ctx ends or the run does.
	StreamStats(ctx context.Context, interval time.Duration, fn func(rungv.Stats)) error
	// Top lists the guest's processes.
	Top(ctx context.Context) ([]rungv.Process, error)
	// Kill signals guest processes (the first one by default). A SIGKILL
	// of the first process ends the run, which then ends as Failed with
	// rungv's exit code, not as Stopped.
	Kill(ctx context.Context, o rungv.KillOptions) error
}

// ExecOptions are the options of Control.Exec: rungv's, plus the initial
// terminal size. PIDFile is the supervisor's own (it always asks for one).
type ExecOptions struct {
	rungv.ExecOptions
	Rows, Cols uint16 // initial terminal size with TTY; 0 is 24x80
}

// ExecProcess is one rungv exec started by Control.Exec.
type ExecProcess interface {
	// Stdin is the command's standard input; nil without Interactive.
	// Close sends end-of-file: it closes the pipe, or writes EOT (0x04)
	// on a terminal.
	Stdin() io.WriteCloser
	// Stdout is what the command writes; with TTY, the terminal's bytes,
	// standard error included.
	Stdout() io.Reader
	// Stderr is the command's standard error without TTY, and rungv's own
	// messages ("rungv exec: CODE: ..."); nil with TTY (rungv's messages
	// then go to Wait's error).
	Stderr() io.Reader
	// Resize sets the terminal's size; an error without TTY, and on
	// Windows.
	Resize(rows, cols uint16) error
	// PID is the command's guest PID once it started, 0 before (read
	// from rungv's --pid-file). Started is closed when PID is known, or
	// when the exec failed before starting.
	PID() int
	Started() <-chan struct{}
	// Wait waits for the command and returns its exit code. err is
	// non-nil when rungv failed rather than the command: the exec never
	// started (PID() == 0: paused, not found, too many sessions), or the
	// run ended under it. An *rungv.Error carries rungv's message; see
	// rungv.ControlCode.
	Wait() (code int, err error)
	// Signal sends sig to the command's guest process (rungv kill --pid).
	Signal(ctx context.Context, sig string) error
	// Close ends the rungv exec client without signaling the guest (the
	// guest command keeps running detached) and releases the PTY and pipes.
	// Safe to call twice.
	Close() error
}

type runControl struct {
	sup    *Supervisor
	id     string
	proc   *proc
	client *rungv.Client
	target rungv.Target
}

func (c *runControl) Target() rungv.Target { return c.target }

func (c *runControl) ExecArgv(o rungv.ExecOptions) []string {
	args := c.client.ExecArgs(c.target, o)
	out := make([]string, 0, len(args)+1)
	out = append(out, c.client.Path)
	return append(out, args...)
}

func (c *runControl) isPaused() bool {
	c.sup.mu.Lock()
	paused := c.proc.state.Paused
	c.sup.mu.Unlock()
	return paused
}

func (c *runControl) CopyFrom(ctx context.Context, guestPath string, o rungv.CopyOptions) (io.ReadCloser, error) {
	return c.client.CopyFrom(ctx, c.target, guestPath, o)
}

func (c *runControl) CopyTo(ctx context.Context, guestDir string, r io.Reader, o rungv.CopyOptions) error {
	return c.client.CopyTo(ctx, c.target, guestDir, r, o)
}

func (c *runControl) StatPath(ctx context.Context, guestPath string) (rungv.PathStat, error) {
	return c.client.StatPath(ctx, c.target, guestPath)
}

func (c *runControl) Export(ctx context.Context, guestPath string, upperOnly bool) (io.ReadCloser, error) {
	return c.client.Export(ctx, c.target, guestPath, upperOnly)
}

func (c *runControl) Stats(ctx context.Context) (rungv.Stats, error) {
	return c.client.Stats(ctx, c.target)
}

func (c *runControl) StreamStats(ctx context.Context, interval time.Duration, fn func(rungv.Stats)) error {
	return c.client.StreamStats(ctx, c.target, interval, fn)
}

func (c *runControl) Top(ctx context.Context) ([]rungv.Process, error) {
	return c.client.Top(ctx, c.target)
}

func (c *runControl) Kill(ctx context.Context, o rungv.KillOptions) error {
	return c.client.Kill(ctx, c.target, o)
}

func (s *Supervisor) clientFor(p *proc) *rungv.Client {
	if s.Client != nil {
		if cur := s.Client(); cur != nil && cur.Path != "" {
			return cur
		}
	}
	return p.client
}

// Control returns the control of the active run of id: false when none is
// active, or the run has no control socket (State.Control is false).
func (s *Supervisor) Control(id string) (Control, bool) {
	s.mu.Lock()
	p := s.procs[id]
	if p == nil || !p.state.Status.Active() || !p.state.Control || p.paths.ControlSocket == "" {
		s.mu.Unlock()
		return nil, false
	}
	target := rungv.Target{Socket: p.paths.ControlSocket, TokenFile: p.paths.TokenFile}
	s.mu.Unlock()
	client := s.clientFor(p)
	if client == nil || client.Path == "" {
		return nil, false
	}
	return &runControl{
		sup:    s,
		id:     id,
		proc:   p,
		client: client,
		target: target,
	}, true
}

func (s *Supervisor) controlTarget(id string) (*proc, *rungv.Client, rungv.Target, error) {
	s.mu.Lock()
	p := s.procs[id]
	if p == nil || !p.state.Status.Active() {
		s.mu.Unlock()
		return nil, nil, rungv.Target{}, errNotRunning
	}
	if !p.state.Control || p.paths.ControlSocket == "" {
		s.mu.Unlock()
		return nil, nil, rungv.Target{}, ErrNoControl
	}
	target := rungv.Target{Socket: p.paths.ControlSocket, TokenFile: p.paths.TokenFile}
	s.mu.Unlock()
	client := s.clientFor(p)
	if client == nil || client.Path == "" {
		return nil, nil, rungv.Target{}, rungv.ErrNotFound
	}
	return p, client, target, nil
}

// Pause freezes the guest of the active run of id. State.Paused becomes
// true and an EventChanged follows. ErrNoControl without a socket.
func (s *Supervisor) Pause(ctx context.Context, id string) error {
	p, client, target, err := s.controlTarget(id)
	if err != nil {
		return err
	}
	if err := client.Pause(ctx, target); err != nil && rungv.ControlCode(err) != rungv.CodeAlreadyPaused {
		return err
	}
	s.mu.Lock()
	if p.state.Status.Active() {
		p.state.Paused = true
	}
	s.mu.Unlock()
	s.notify(Event{ID: id, Kind: EventChanged, State: s.State(id)})
	return nil
}

// Unpause thaws the guest of the active run of id.
func (s *Supervisor) Unpause(ctx context.Context, id string) error {
	p, client, target, err := s.controlTarget(id)
	if err != nil {
		return err
	}
	if err := client.Unpause(ctx, target); err != nil && rungv.ControlCode(err) != rungv.CodeNotPaused {
		return err
	}
	s.mu.Lock()
	p.state.Paused = false
	s.mu.Unlock()
	s.notify(Event{ID: id, Kind: EventChanged, State: s.State(id)})
	return nil
}

// Update changes the resources of the active run of id live. On success
// the active spec (ActiveSpec) takes the new Memory, CPUs and IsolateLAN,
// so a later edit compares against what runs.
func (s *Supervisor) Update(ctx context.Context, id string, u rungv.Update) (rungv.Resources, error) {
	p, client, target, err := s.controlTarget(id)
	if err != nil {
		return rungv.Resources{}, err
	}
	res, err := client.Update(ctx, target, u)
	if err != nil {
		return rungv.Resources{}, err
	}
	s.mu.Lock()
	if u.Memory != "" {
		if u.Memory == "0" {
			p.spec.Memory = ""
		} else {
			p.spec.Memory = u.Memory
		}
	}
	if u.CPUs != "" {
		if u.CPUs == "0" {
			p.spec.CPUs = ""
		} else {
			p.spec.CPUs = u.CPUs
		}
	}
	if u.IsolateLAN != nil {
		p.spec.IsolateLAN = *u.IsolateLAN
	}
	s.mu.Unlock()

	if summary := formatUpdateSummary(u); summary != "" {
		s.Logs(id).Add(System, "── Updated: "+summary)
	}
	s.notify(Event{ID: id, Kind: EventChanged, State: s.State(id)})
	return res, nil
}

func formatUpdateSummary(u rungv.Update) string {
	var parts []string
	if u.Memory != "" {
		parts = append(parts, "memory "+u.Memory)
	}
	if u.CPUs != "" {
		parts = append(parts, "cpus "+u.CPUs)
	}
	if u.CPUQuota != 0 {
		parts = append(parts, fmt.Sprintf("cpu-quota %d", u.CPUQuota))
	}
	if u.CPUPeriod != 0 {
		parts = append(parts, fmt.Sprintf("cpu-period %d", u.CPUPeriod))
	}
	if u.PidsLimit != nil {
		parts = append(parts, fmt.Sprintf("pids-limit %d", *u.PidsLimit))
	}
	if u.IsolateLAN != nil {
		parts = append(parts, fmt.Sprintf("isolate-lan %t", *u.IsolateLAN))
	}
	return strings.Join(parts, ", ")
}

// Shutdown stops the active run of id as docker stop does: signal (""
// is SIGTERM) to the guest's first process through the control socket,
// which kills it after timeout; the sandbox ends with it, as Stopped. A
// paused run is unpaused first. Without a control socket, or when rungv
// stop fails, it is Stop(id, timeout). It returns once the run ended.
func (s *Supervisor) Shutdown(id string, signal string, timeout time.Duration) error {
	s.mu.Lock()
	p := s.procs[id]
	if p == nil || !p.state.Status.Active() {
		s.mu.Unlock()
		return nil
	}
	if !p.state.Control || p.paths.ControlSocket == "" {
		s.mu.Unlock()
		return s.Stop(id, timeout)
	}
	p.stopping = true
	p.state.Status = Stopping
	paused := p.state.Paused
	target := rungv.Target{Socket: p.paths.ControlSocket, TokenFile: p.paths.TokenFile}
	osProc := p.cmd.Process
	done := p.done
	s.mu.Unlock()

	client := s.clientFor(p)
	s.notify(Event{ID: id, Kind: EventChanged, State: s.State(id)})
	s.Logs(id).Add(System, "── Stopping…")

	fallbackStop := func() error {
		if runtime.GOOS == "windows" {
			_ = osProc.Kill()
		} else if err := osProc.Signal(os.Interrupt); err != nil {
			_ = osProc.Kill()
		}
		select {
		case <-done:
			return nil
		case <-time.After(5 * time.Second):
			_ = osProc.Kill()
			<-done
			return nil
		}
	}

	if client == nil || client.Path == "" {
		return fallbackStop()
	}

	if paused {
		uCtx, uCancel := context.WithTimeout(context.Background(), 5*time.Second)
		_ = client.Unpause(uCtx, target)
		uCancel()
		s.mu.Lock()
		p.state.Paused = false
		s.mu.Unlock()
	}

	grace := max(timeout, 0)
	stopCtx, stopCancel := context.WithTimeout(context.Background(), grace+10*time.Second)
	defer stopCancel()

	stopErrCh := make(chan error, 1)
	go func() {
		stopErrCh <- client.StopGuest(stopCtx, target, signal, grace)
	}()

	timer := time.NewTimer(grace + 5*time.Second)
	defer timer.Stop()

	select {
	case <-done:
		return nil
	case err := <-stopErrCh:
		select {
		case <-done:
			return nil
		default:
		}
		if err != nil {
			return fallbackStop()
		}
		// StopGuest returned cleanly; wait briefly for the host rungv process to exit.
		select {
		case <-done:
			return nil
		case <-time.After(5 * time.Second):
			return fallbackStop()
		}
	case <-timer.C:
		return fallbackStop()
	}
}

// LiveUpdate compares the spec a sandbox runs with (running) and an edit
// of it. live is true when every difference can be applied to the run with
// Supervisor.Update: only Memory (not to ""), CPUs ("" becomes "0"), and
// IsolateLAN differ, besides fields that never reach rungv (AutoStart).
// u is then the update to apply; it is zero when nothing differs.
func LiveUpdate(running, edited Spec) (u rungv.Update, live bool) {
	a, b := running.Clone(), edited.Clone()
	a.Memory, b.Memory = "", ""
	a.CPUs, b.CPUs = "", ""
	a.IsolateLAN, b.IsolateLAN = false, false
	a.AutoStart, b.AutoStart = false, false
	a.CreatedAt, b.CreatedAt = time.Time{}, time.Time{}
	if !a.Equal(b) {
		return rungv.Update{}, false
	}
	if running.Memory != edited.Memory {
		if edited.Memory == "" {
			return rungv.Update{}, false
		}
		u.Memory = edited.Memory
	}
	if running.CPUs != edited.CPUs {
		if edited.CPUs == "" {
			u.CPUs = "0"
		} else {
			u.CPUs = edited.CPUs
		}
	}
	if running.IsolateLAN != edited.IsolateLAN {
		v := edited.IsolateLAN
		u.IsolateLAN = &v
	}
	return u, true
}
