package sandbox

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
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
