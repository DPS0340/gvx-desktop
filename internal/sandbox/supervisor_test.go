package sandbox

import (
	"context"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ironpark/gvx-desktop/internal/rungv"
)

// rungvForTest finds a working rungv, or skips: the integration tests run a
// real sandbox.
func rungvForTest(t *testing.T) *rungv.Client {
	t.Helper()
	if testing.Short() {
		t.Skip("integration test")
	}
	path, err := rungv.Locate(filepath.Join("..", "..", "bin", rungv.BinaryName()))
	if err != nil {
		t.Skip("rungv not found")
	}
	c := &rungv.Client{Path: path}
	info, err := c.Info(context.Background())
	if err != nil || len(info.AvailableBackends) == 0 {
		t.Skipf("no usable backend: %v", err)
	}
	return c
}

func newTestSupervisor(t *testing.T, c *rungv.Client) (*Supervisor, chan Event) {
	events := make(chan Event, 1024)
	s := &Supervisor{
		Client:  func() *rungv.Client { return c },
		DataDir: t.TempDir(),
		Notify: func(e Event) {
			select {
			case events <- e:
			default:
			}
		},
	}
	t.Cleanup(func() { s.StopAll(10 * time.Second) })
	return s, events
}

func waitFor(t *testing.T, what string, timeout time.Duration, cond func() bool, logs ...*LogBuffer) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	for _, l := range logs {
		for _, line := range l.Snapshot() {
			t.Log(line.Text)
		}
	}
	t.Fatalf("timed out waiting for %s", what)
}

func TestSupervisorRunsToCompletion(t *testing.T) {
	c := rungvForTest(t)
	s, _ := newTestSupervisor(t, c)
	spec := Spec{ID: NewID(), Name: "once", Image: "alpine:3.20", Command: "echo hello-from-gvx"}
	if err := s.Start(spec); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "exit", 2*time.Minute, func() bool { return s.State(spec.ID).Status == Exited })
	var found bool
	for _, l := range s.Logs(spec.ID).Snapshot() {
		if l.Text == "hello-from-gvx" && l.Stream == Stdout {
			found = true
		}
	}
	if !found {
		t.Fatalf("output missing: %+v", s.Logs(spec.ID).Snapshot())
	}
	if st := s.State(spec.ID); st.Platform == "" {
		t.Errorf("platform not parsed: %+v", st)
	}
}

func TestSupervisorPortsReadyAndStop(t *testing.T) {
	c := rungvForTest(t)
	s, events := newTestSupervisor(t, c)
	spec := Spec{ID: NewID(), Name: "web", Image: "nginx:alpine", Ports: []string{"18481:80"},
		ReadyPattern: "start worker process"}
	if err := s.Start(spec); err != nil {
		t.Fatal(err)
	}
	if err := s.Start(spec); err == nil {
		t.Error("second start of a running sandbox accepted")
	}
	waitFor(t, "ready", 2*time.Minute, func() bool { return s.State(spec.ID).Status == Running })
	st := s.State(spec.ID)
	if len(st.Ports) != 1 || st.Ports[0].Host != "127.0.0.1:18481" || st.Ports[0].Guest != 80 || st.Ports[0].URL() != "http://127.0.0.1:18481" {
		t.Fatalf("ports %+v", st.Ports)
	}
	res, err := http.Get(st.Ports[0].URL())
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != 200 {
		t.Fatalf("status %d", res.StatusCode)
	}
	var sawReady bool
	for len(events) > 0 {
		if (<-events).Kind == EventReady {
			sawReady = true
		}
	}
	if !sawReady {
		t.Error("no ready event")
	}
	if err := s.Stop(spec.ID, 10*time.Second); err != nil {
		t.Fatal(err)
	}
	if st := s.State(spec.ID); st.Status != Stopped || st.PID != 0 {
		t.Fatalf("after stop: %+v", st)
	}
}

func TestSupervisorReportsFailure(t *testing.T) {
	c := rungvForTest(t)
	s, _ := newTestSupervisor(t, c)
	spec := Spec{ID: NewID(), Name: "bad", Image: "alpine:3.20", Command: "sh -c 'echo boom >&2; exit 3'"}
	if err := s.Start(spec); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "failure", 2*time.Minute, func() bool { return s.State(spec.ID).Status == Failed })
	if st := s.State(spec.ID); st.ExitCode != 3 || !strings.Contains(st.Error, "boom") {
		t.Fatalf("%+v", st)
	}
}

func TestSupervisorVolumeLock(t *testing.T) {
	c := rungvForTest(t)
	s, _ := newTestSupervisor(t, c)
	vol := "gvxd-test-" + NewID()
	t.Cleanup(func() { _ = c.RemoveVolume(context.Background(), vol) })
	a := Spec{ID: NewID(), Name: "a", Image: "alpine:3.20", Command: "sleep 30",
		Mounts: []Mount{{Type: MountVolume, Source: vol, Target: "/data"}}}
	b := a.Clone()
	b.ID, b.Name = NewID(), "b"
	if err := s.Start(a); err != nil {
		t.Fatal(err)
	}
	if err := s.Start(b); err == nil || !strings.Contains(err.Error(), "in use") {
		t.Fatalf("second user of a volume: %v", err)
	}
}

func TestSupervisorCheckpointRestore(t *testing.T) {
	c := rungvForTest(t)
	s, _ := newTestSupervisor(t, c)
	work := t.TempDir()
	spec := Spec{ID: NewID(), Name: "ck", Image: "alpine:3.20", Workspace: work, Checkpoint: true,
		Command: `sh -c 'i=0; while true; do i=$((i+1)); echo tick-$i; sleep 0.2; done'`}
	if err := s.Start(spec); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "ticks", 2*time.Minute, func() bool { return len(s.Logs(spec.ID).Tail(20, Stdout)) > 3 }, s.Logs(spec.ID))
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	dir, err := s.Checkpoint(ctx, spec.ID, false)
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, "stop after checkpoint", time.Minute, func() bool { return !s.State(spec.ID).Status.Active() })
	if _, err := os.Stat(filepath.Join(dir, "manifest.json")); err != nil {
		t.Fatal(err)
	}
	snaps := s.Snapshots(spec.ID)
	if len(snaps) != 1 || snaps[0].Dir != dir {
		t.Fatalf("snapshots %+v", snaps)
	}
	last := s.Logs(spec.ID).Tail(1, Stdout)[0]
	if err := s.Restore(spec, dir); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "resumed ticks", time.Minute, func() bool {
		tail := s.Logs(spec.ID).Tail(1, Stdout)
		return len(tail) == 1 && tail[0] != last
	}, s.Logs(spec.ID))
	// The counter continues instead of starting over.
	tail := s.Logs(spec.ID).Tail(1, Stdout)[0]
	if tail == "tick-1" || tail == "tick-2" {
		t.Fatalf("restore started over: last before %q, now %q", last, tail)
	}
}

// What Prepare writes is there before rungv runs, and a failure to write it
// keeps rungv from running at all.
func TestPrepareRunsBeforeRungv(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "started")
	// A stand-in rungv that says whether the prepared file was there.
	exe := filepath.Join(t.TempDir(), "rungv")
	prepared := filepath.Join(t.TempDir(), "prepared")
	script := "#!/bin/sh\nif [ -f " + prepared + " ]; then echo yes > " + marker + "; else echo no > " + marker + "; fi\n"
	if err := os.WriteFile(exe, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS == "windows" {
		t.Skip("the stand-in rungv is a shell script")
	}
	s, _ := newTestSupervisor(t, &rungv.Client{Path: exe})
	spec := Spec{ID: "p1", Name: "p", Image: "alpine"}
	s.Prepare = func(Spec) error { return os.ErrPermission }
	if err := s.Start(spec); err == nil {
		t.Fatal("started although Prepare failed")
	}
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("rungv ran although Prepare failed")
	}
	s.Prepare = func(got Spec) error {
		if got.ID != spec.ID {
			t.Errorf("prepared %+v", got)
		}
		return os.WriteFile(prepared, nil, 0o644)
	}
	if err := s.Start(spec); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the stand-in to end", 10*time.Second, func() bool { return !s.State(spec.ID).Status.Active() })
	if got, _ := os.ReadFile(marker); strings.TrimSpace(string(got)) != "yes" {
		t.Errorf("the prepared file was not there when rungv ran: %q", got)
	}
}

// rungv runs what Launch returns, the sandbox keeps the spec it was given,
// and the release comes once, as the run ends; a run that fails to launch
// does not run.
func TestLaunchHook(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the stand-in rungv is a shell script")
	}
	args := filepath.Join(t.TempDir(), "args")
	exe := filepath.Join(t.TempDir(), "rungv")
	script := "#!/bin/sh\ncase \"$*\" in *-help*) exit 2 ;; esac\necho \"$@\" > " + args + "\n"
	if err := os.WriteFile(exe, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	s, _ := newTestSupervisor(t, &rungv.Client{Path: exe})
	spec := Spec{ID: "l1", Name: "l", Image: "alpine", Command: "original"}
	s.Launch = func(Spec) (Spec, func(), error) { return Spec{}, nil, os.ErrPermission }
	if err := s.Start(spec); err == nil {
		t.Fatal("started although Launch failed")
	}
	if _, err := os.Stat(args); err == nil {
		t.Fatal("rungv ran although Launch failed")
	}
	released := make(chan struct{}, 2)
	s.Launch = func(got Spec) (Spec, func(), error) {
		run := got.Clone()
		run.Command = "launched"
		return run, func() { released <- struct{}{} }, nil
	}
	if err := s.Start(spec); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the stand-in to end", 10*time.Second, func() bool { return !s.State(spec.ID).Status.Active() })
	if got, _ := os.ReadFile(args); !strings.Contains(string(got), "launched") || strings.Contains(string(got), "original") {
		t.Errorf("rungv ran %q", got)
	}
	if got, ok := s.ActiveSpec(spec.ID); ok && got.Command != "original" {
		t.Errorf("the sandbox keeps %q", got.Command)
	}
	select {
	case <-released:
	default:
		t.Fatal("not released as the run ended")
	}
	select {
	case <-released:
		t.Fatal("released twice")
	case <-time.After(100 * time.Millisecond):
	}
}

// rungv's report of a port it could not publish, on stderr, becomes an event
// naming the port.
func TestSupervisorReportsABusyPort(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the fake rungv is a shell script")
	}
	path := filepath.Join(t.TempDir(), "rungv")
	script := "#!/bin/sh\ncase \"$*\" in *-help*) exit 2 ;; esac\n" +
		"echo 'gvx: warning: could not auto-publish 127.0.0.1:18082/tcp: listen tcp4 127.0.0.1:18082: bind: address already in use' >&2\n" +
		"exec sleep 30\n"
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	s, events := newTestSupervisor(t, &rungv.Client{Path: path})
	spec := Spec{ID: NewID(), Name: "engine", Image: "docker:dind"}
	if err := s.Start(spec); err != nil {
		t.Fatal(err)
	}
	deadline := time.After(10 * time.Second)
	for {
		select {
		case e := <-events:
			if e.Kind == EventPortBusy {
				if e.ID != spec.ID || e.Port != "127.0.0.1:18082" {
					t.Fatalf("event %+v", e)
				}
				return
			}
		case <-deadline:
			t.Fatal("no EventPortBusy")
		}
	}
}

func TestSupervisorCheckpointRetriesUnreachableSocket(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the fake rungv is a shell script")
	}
	origDelays := checkpointRetryDelays
	checkpointRetryDelays = []time.Duration{
		15 * time.Millisecond,
		15 * time.Millisecond,
		15 * time.Millisecond,
		15 * time.Millisecond,
	}
	t.Cleanup(func() { checkpointRetryDelays = origDelays })

	stateDir := t.TempDir()
	attemptsFile := filepath.Join(stateDir, "attempts")
	modeFile := filepath.Join(stateDir, "mode")
	exe := filepath.Join(stateDir, "rungv")
	script := `#!/bin/sh
case "$1" in
  run)
    case "$*" in *-help*) echo "  -control-socket"; exit 0 ;; esac
    exec sleep 30
    ;;
  restore)
    case "$*" in *-help*) exit 0 ;; esac
    exit 0
    ;;
  checkpoint)
    n=0
    if [ -f "` + attemptsFile + `" ]; then
      n=$(cat "` + attemptsFile + `")
    fi
    n=$((n + 1))
    echo "$n" > "` + attemptsFile + `"
    mode=$(cat "` + modeFile + `" 2>/dev/null || echo retry)
    if [ "$mode" = "protocol_err" ]; then
      echo "rungv checkpoint: NOT_FOUND: missing sandbox" >&2
      exit 1
    fi
    if [ "$mode" = "always_unreachable" ] || [ "$n" -lt 3 ]; then
      echo "rungv checkpoint: connect control socket: no such file or directory" >&2
      exit 1
    fi
    ckdir=""
    prev=""
    for a in "$@"; do
      if [ "$prev" = "-checkpoint-dir" ]; then ckdir="$a"; fi
      prev="$a"
    done
    if [ -n "$ckdir" ]; then
      mkdir -p "$ckdir"
      echo '{"version":1}' > "$ckdir/manifest.json"
    fi
    exit 0
    ;;
esac
exit 0
`
	if err := os.WriteFile(exe, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { rungv.ForgetFeatures(exe) })

	s, _ := newTestSupervisor(t, &rungv.Client{Path: exe})
	spec := Spec{ID: NewID(), Name: "ck-retry", Image: "alpine:3.20", Checkpoint: true}
	if err := s.Start(spec); err != nil {
		t.Fatalf("Start: %v", err)
	}

	// 1. Transient unreachable control socket retries and succeeds on 3rd attempt.
	if err := os.WriteFile(modeFile, []byte("retry"), 0o600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	dir, err := s.Checkpoint(ctx, spec.ID, true)
	if err != nil {
		t.Fatalf("Checkpoint with transient unreachable socket failed: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "manifest.json")); err != nil {
		t.Fatalf("expected checkpoint manifest.json at %s: %v", dir, err)
	}
	if got, _ := os.ReadFile(attemptsFile); strings.TrimSpace(string(got)) != "3" {
		t.Fatalf("attempts = %q, want 3", strings.TrimSpace(string(got)))
	}

	// 2. Non-unreachable protocol error fails immediately without retrying.
	_ = os.Remove(attemptsFile)
	if err := os.WriteFile(modeFile, []byte("protocol_err"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Checkpoint(ctx, spec.ID, true); err == nil || rungv.ControlCode(err) != rungv.CodeNotFound {
		t.Fatalf("expected NOT_FOUND error without retry, got %v", err)
	}
	if got, _ := os.ReadFile(attemptsFile); strings.TrimSpace(string(got)) != "1" {
		t.Fatalf("protocol error attempts = %q, want 1 (no retry)", strings.TrimSpace(string(got)))
	}

	// 3. Context cancellation stops unreachable retries promptly.
	_ = os.Remove(attemptsFile)
	if err := os.WriteFile(modeFile, []byte("always_unreachable"), 0o600); err != nil {
		t.Fatal(err)
	}
	cCtx, cCancel := context.WithTimeout(context.Background(), 35*time.Millisecond)
	defer cCancel()
	if _, err := s.Checkpoint(cCtx, spec.ID, true); err == nil || (!errors.Is(err, context.DeadlineExceeded) && !rungv.IsUnreachable(err)) {
		t.Fatalf("expected DeadlineExceeded or unreachable error on canceled ctx, got %v", err)
	}
}

func TestSupervisorCheckpointConcurrentStopPreservesStopped(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the fake rungv is a shell script")
	}
	for _, leaveRunning := range []bool{true, false} {
		name := "leave_running_true"
		if !leaveRunning {
			name = "leave_running_false"
		}
		t.Run(name, func(t *testing.T) {
			stateDir := t.TempDir()
			ckptStarted := filepath.Join(stateDir, "ckpt_started")
			ckptRelease := filepath.Join(stateDir, "ckpt_release")
			exe := filepath.Join(stateDir, "rungv")
			script := `#!/bin/sh
case "$1" in
  run)
    case "$*" in *-help*) echo "  -control-socket"; exit 0 ;; esac
    sleep 30 &
    child=$!
    trap 'kill "$child" 2>/dev/null; sleep 0.25; exit 1' INT TERM
    echo READY
    wait "$child"
    exit $?
    ;;
  checkpoint)
    : > "` + ckptStarted + `"
    while [ ! -f "` + ckptRelease + `" ]; do
      sleep 0.02
    done
    echo "rungv checkpoint: NOT_FOUND: stopping" >&2
    exit 1
    ;;
esac
exit 0
`
			if err := os.WriteFile(exe, []byte(script), 0o755); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { rungv.ForgetFeatures(exe) })

			s, _ := newTestSupervisor(t, &rungv.Client{Path: exe})
			spec := Spec{ID: NewID(), Name: "ck-stop-" + name, Image: "alpine:3.20", Checkpoint: true, ReadyPattern: "READY"}
			if err := s.Start(spec); err != nil {
				t.Fatalf("Start: %v", err)
			}
			waitFor(t, "running", 5*time.Second, func() bool {
				return s.State(spec.ID).Status == Running
			})

			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			ckptErrCh := make(chan error, 1)
			go func() {
				_, err := s.Checkpoint(ctx, spec.ID, leaveRunning)
				ckptErrCh <- err
			}()

			waitFor(t, "checkpoint started", 5*time.Second, func() bool {
				_, err := os.Stat(ckptStarted)
				return err == nil
			})

			stopErrCh := make(chan error, 1)
			go func() {
				stopErrCh <- s.Stop(spec.ID, 2*time.Second)
			}()

			waitFor(t, "status Stopping", 5*time.Second, func() bool {
				return s.State(spec.ID).Status == Stopping
			})

			if err := os.WriteFile(ckptRelease, []byte("go"), 0o600); err != nil {
				t.Fatal(err)
			}

			if err := <-ckptErrCh; err == nil {
				t.Fatalf("Checkpoint expected failure during concurrent Stop, got nil")
			}
			if err := <-stopErrCh; err != nil {
				t.Fatalf("Stop: %v", err)
			}
			if st := s.State(spec.ID); st.Status != Stopped {
				t.Fatalf("status after concurrent Stop + Checkpoint(leaveRunning=%v) = %v (err=%q), want Stopped", leaveRunning, st.Status, st.Error)
			}
		})
	}
}

func TestSupervisorCheckpointRejectedWhileStoppingPreservesStopping(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the fake rungv is a shell script")
	}
	stateDir := t.TempDir()
	runRelease := filepath.Join(stateDir, "run_release")
	exe := filepath.Join(stateDir, "rungv")
	script := `#!/bin/sh
case "$1" in
  run)
    case "$*" in *-help*) echo "  -control-socket"; exit 0 ;; esac
    sleep 30 &
    child=$!
    trap 'kill "$child" 2>/dev/null; while [ ! -f "` + runRelease + `" ]; do sleep 0.02; done; exit 1' INT TERM
    echo READY
    wait "$child"
    exit $?
    ;;
esac
exit 0
`
	if err := os.WriteFile(exe, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { rungv.ForgetFeatures(exe) })

	s, _ := newTestSupervisor(t, &rungv.Client{Path: exe})
	spec := Spec{ID: NewID(), Name: "ck-stopping", Image: "alpine:3.20", Checkpoint: true, ReadyPattern: "READY"}
	if err := s.Start(spec); err != nil {
		t.Fatalf("Start: %v", err)
	}
	waitFor(t, "running", 5*time.Second, func() bool {
		return s.State(spec.ID).Status == Running
	})

	stopErrCh := make(chan error, 1)
	go func() {
		stopErrCh <- s.Stop(spec.ID, 5*time.Second)
	}()
	waitFor(t, "status Stopping", 5*time.Second, func() bool {
		return s.State(spec.ID).Status == Stopping
	})

	ctx := context.Background()
	for _, leaveRunning := range []bool{true, false} {
		if _, err := s.Checkpoint(ctx, spec.ID, leaveRunning); err == nil || !strings.Contains(err.Error(), "stopping") {
			t.Fatalf("Checkpoint(leaveRunning=%v) while Stopping = %v, want stopping error", leaveRunning, err)
		}
	}

	// Simulate nil/empty clientFor(p) while Status == Stopping: must reject and preserve p.stopping.
	s.Client = func() *rungv.Client { return &rungv.Client{Path: ""} }
	s.mu.Lock()
	s.procs[spec.ID].client = nil
	stoppingBefore := s.procs[spec.ID].stopping
	s.mu.Unlock()
	if !stoppingBefore {
		t.Fatal("expected p.stopping == true while Status == Stopping")
	}

	for _, leaveRunning := range []bool{true, false} {
		if _, err := s.Checkpoint(ctx, spec.ID, leaveRunning); err == nil || !strings.Contains(err.Error(), "stopping") {
			t.Fatalf("Checkpoint(leaveRunning=%v) with nil client while Stopping = %v, want stopping error", leaveRunning, err)
		}
	}

	s.mu.Lock()
	stoppingAfter := s.procs[spec.ID].stopping
	s.mu.Unlock()
	if !stoppingAfter {
		t.Fatal("Checkpoint while Stopping clobbered p.stopping to false")
	}

	if err := os.WriteFile(runRelease, []byte("go"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := <-stopErrCh; err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if st := s.State(spec.ID); st.Status != Stopped {
		t.Fatalf("final status = %v (err=%q), want Stopped", st.Status, st.Error)
	}
}

func TestSupervisorCheckpointConcurrentCallsDoNotClobberStopping(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the fake rungv is a shell script")
	}
	stateDir := t.TempDir()
	ckptStarted := filepath.Join(stateDir, "ckpt_started")
	ckptRelease := filepath.Join(stateDir, "ckpt_release")
	exe := filepath.Join(stateDir, "rungv")
	script := `#!/bin/sh
case "$1" in
  run)
    case "$*" in *-help*) echo "  -control-socket"; exit 0 ;; esac
    exec sleep 30
    ;;
  checkpoint)
    : > "` + ckptStarted + `"
    while [ ! -f "` + ckptRelease + `" ]; do
      sleep 0.02
    done
    echo "rungv checkpoint: NOT_FOUND: simulated error" >&2
    exit 1
    ;;
esac
exit 0
`
	if err := os.WriteFile(exe, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { rungv.ForgetFeatures(exe) })

	s, _ := newTestSupervisor(t, &rungv.Client{Path: exe})
	spec := Spec{ID: NewID(), Name: "ck-concurrent", Image: "alpine:3.20", Checkpoint: true}
	if err := s.Start(spec); err != nil {
		t.Fatalf("Start: %v", err)
	}
	waitFor(t, "running", 5*time.Second, func() bool {
		return s.State(spec.ID).Status == Running
	})

	// 1. In-flight Checkpoint(leaveRunning=false) sets p.stopping=true and p.checkpointing=true;
	// concurrent Checkpoints must be rejected without clobbering p.stopping.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	firstErrCh := make(chan error, 1)
	go func() {
		_, err := s.Checkpoint(ctx, spec.ID, false)
		firstErrCh <- err
	}()
	waitFor(t, "first checkpoint started", 5*time.Second, func() bool {
		_, err := os.Stat(ckptStarted)
		return err == nil
	})

	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		lr := i%2 == 0
		wg.Add(1)
		go func(leaveRunning bool) {
			defer wg.Done()
			if _, err := s.Checkpoint(ctx, spec.ID, leaveRunning); err == nil {
				t.Errorf("concurrent Checkpoint(leaveRunning=%v) unexpectedly succeeded", leaveRunning)
			}
		}(lr)
	}
	wg.Wait()

	s.mu.Lock()
	stoppingMid := s.procs[spec.ID].stopping
	checkpointingMid := s.procs[spec.ID].checkpointing
	s.mu.Unlock()
	if !stoppingMid || !checkpointingMid {
		t.Fatalf("expected stopping=true and checkpointing=true mid-flight, got stopping=%v checkpointing=%v", stoppingMid, checkpointingMid)
	}

	if err := os.WriteFile(ckptRelease, []byte("go"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := <-firstErrCh; err == nil {
		t.Fatal("expected first checkpoint to fail with simulated error")
	}

	s.mu.Lock()
	stoppingAfter := s.procs[spec.ID].stopping
	checkpointingAfter := s.procs[spec.ID].checkpointing
	s.mu.Unlock()
	if stoppingAfter || checkpointingAfter {
		t.Fatalf("expected stopping=false and checkpointing=false after failure, got stopping=%v checkpointing=%v", stoppingAfter, checkpointingAfter)
	}

	// 2. In-flight Checkpoint(leaveRunning=true) sets p.checkpointing=true and p.stopping=false;
	// concurrent Checkpoint(leaveRunning=false) must be rejected without setting p.stopping=true.
	_ = os.Remove(ckptStarted)
	_ = os.Remove(ckptRelease)
	secondErrCh := make(chan error, 1)
	go func() {
		_, err := s.Checkpoint(ctx, spec.ID, true)
		secondErrCh <- err
	}()
	waitFor(t, "second checkpoint started", 5*time.Second, func() bool {
		_, err := os.Stat(ckptStarted)
		return err == nil
	})

	if _, err := s.Checkpoint(ctx, spec.ID, false); err == nil || !strings.Contains(err.Error(), "already in progress") {
		t.Fatalf("concurrent Checkpoint(false) while Checkpoint(true) in flight = %v, want already in progress", err)
	}

	s.mu.Lock()
	stoppingWhileTrue := s.procs[spec.ID].stopping
	checkpointingWhileTrue := s.procs[spec.ID].checkpointing
	s.mu.Unlock()
	if stoppingWhileTrue || !checkpointingWhileTrue {
		t.Fatalf("expected stopping=false and checkpointing=true, got stopping=%v checkpointing=%v", stoppingWhileTrue, checkpointingWhileTrue)
	}

	if err := os.WriteFile(ckptRelease, []byte("go"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := <-secondErrCh; err == nil {
		t.Fatal("expected second checkpoint to fail with simulated error")
	}
}

func TestSupervisorCheckpointNilClientDoesNotClobberStopping(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the fake rungv is a shell script")
	}
	stateDir := t.TempDir()
	runRelease := filepath.Join(stateDir, "run_release")
	exe := filepath.Join(stateDir, "rungv")
	script := `#!/bin/sh
case "$1" in
  run)
    case "$*" in *-help*) echo "  -control-socket"; exit 0 ;; esac
    sleep 30 &
    child=$!
    trap 'kill "$child" 2>/dev/null; while [ ! -f "` + runRelease + `" ]; do sleep 0.02; done; exit 1' INT TERM
    echo READY
    wait "$child"
    exit $?
    ;;
esac
exit 0
`
	if err := os.WriteFile(exe, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { rungv.ForgetFeatures(exe) })

	s, _ := newTestSupervisor(t, &rungv.Client{Path: exe})
	spec := Spec{ID: NewID(), Name: "ck-nilclient", Image: "alpine:3.20", Checkpoint: true, ReadyPattern: "READY"}
	if err := s.Start(spec); err != nil {
		t.Fatalf("Start: %v", err)
	}
	waitFor(t, "running", 5*time.Second, func() bool {
		return s.State(spec.ID).Status == Running
	})

	inClientFor := make(chan struct{})
	releaseClientFor := make(chan struct{})
	var once sync.Once
	s.mu.Lock()
	s.procs[spec.ID].client = nil
	s.mu.Unlock()
	s.Client = func() *rungv.Client {
		once.Do(func() {
			close(inClientFor)
			<-releaseClientFor
		})
		return &rungv.Client{Path: ""}
	}

	ckptErrCh := make(chan error, 1)
	go func() {
		_, err := s.Checkpoint(context.Background(), spec.ID, true)
		ckptErrCh <- err
	}()
	<-inClientFor

	stopErrCh := make(chan error, 1)
	go func() {
		stopErrCh <- s.Stop(spec.ID, 5*time.Second)
	}()
	waitFor(t, "status Stopping", 5*time.Second, func() bool {
		return s.State(spec.ID).Status == Stopping
	})

	close(releaseClientFor)
	if err := <-ckptErrCh; !errors.Is(err, rungv.ErrNotFound) {
		t.Fatalf("Checkpoint with nil client = %v, want rungv.ErrNotFound", err)
	}

	s.mu.Lock()
	stoppingAfter := s.procs[spec.ID].stopping
	checkpointingAfter := s.procs[spec.ID].checkpointing
	s.mu.Unlock()
	if !stoppingAfter || checkpointingAfter {
		t.Fatalf("expected stopping=true and checkpointing=false, got stopping=%v checkpointing=%v", stoppingAfter, checkpointingAfter)
	}

	if err := os.WriteFile(runRelease, []byte("go"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := <-stopErrCh; err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if st := s.State(spec.ID); st.Status != Stopped {
		t.Fatalf("final status = %v (err=%q), want Stopped", st.Status, st.Error)
	}
}
