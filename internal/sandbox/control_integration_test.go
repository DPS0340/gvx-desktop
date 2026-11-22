//go:build !windows

package sandbox

import (
	"archive/tar"
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ironpark/gvx-desktop/internal/rungv"
)

// controlHarnessClient returns a rungv client for real integration tests.
// Skips unless GVX_SANDBOX_HARNESS=1 and not -short. Always uses a temp cache
// directory and never touches the user's real cache or sandboxes.
func controlHarnessClient(t *testing.T) *rungv.Client {
	t.Helper()
	if testing.Short() || os.Getenv("GVX_SANDBOX_HARNESS") != "1" {
		t.Skip("set GVX_SANDBOX_HARNESS=1 (without -short) to run real rungv control integration tests")
	}
	path := os.Getenv("GVX_RUNGV")
	if path == "" {
		var err error
		path, err = rungv.Locate("")
		if err != nil {
			t.Skip("rungv not found; set GVX_RUNGV")
		}
	}
	cache := os.Getenv("GVX_SANDBOX_CACHE")
	if cache == "" {
		cache = t.TempDir()
	}
	if home, _ := os.UserHomeDir(); home != "" && strings.HasPrefix(cache, home) {
		t.Fatalf("temp cache %q must not be under home dir", cache)
	}
	c := &rungv.Client{Path: path, CacheDir: cache}
	info, err := c.Info(context.Background())
	if err != nil || len(info.AvailableBackends) == 0 {
		t.Skipf("no usable backend: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	if err := c.Stream(ctx, func(string) {}, "pull", "alpine:3.20"); err != nil {
		t.Fatalf("pull alpine:3.20: %v", err)
	}
	return c
}

func waitForPing(t *testing.T, c *rungv.Client, target rungv.Target) rungv.Ping {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	p, err := c.Ping(ctx, target, 15*time.Second)
	if err != nil {
		t.Fatalf("Ping(%+v): %v", target, err)
	}
	return p
}

func TestControlIntegration(t *testing.T) {
	c := controlHarnessClient(t)
	feat := c.Features(context.Background())
	t.Logf("rungv=%s features=%+v", c.Path, feat)

	if !feat.Control {
		// v0.19 fallback verification.
		t.Run("v0.19 fallback", func(t *testing.T) {
			s, _ := newTestSupervisor(t, c)
			spec := Spec{
				ID:      NewID(),
				Name:    "gvxd-ctl-v19-" + NewID(),
				Image:   "alpine:3.20",
				Command: "echo v19-ok",
				Pull:    "never",
			}
			if err := s.Start(spec); err != nil {
				t.Fatalf("Start: %v", err)
			}
			if st := s.State(spec.ID); st.Control {
				t.Fatalf("v0.19 run reported State.Control=true")
			}
			if _, ok := s.Control(spec.ID); ok {
				t.Fatalf("v0.19 run returned Control")
			}
			if err := s.Pause(context.Background(), spec.ID); !errors.Is(err, ErrNoControl) {
				t.Logf("Pause on v0.19 (may already have exited): %v", err)
			}
			waitFor(t, "v19 exit", 30*time.Second, func() bool { return s.State(spec.ID).Status == Exited }, s.Logs(spec.ID))
			var saw bool
			for _, l := range s.Logs(spec.ID).Snapshot() {
				if l.Text == "v19-ok" && l.Stream == Stdout {
					saw = true
				}
			}
			if !saw {
				t.Fatalf("v19-ok missing in logs")
			}
		})
		return
	}

	t.Run("exec cp stats top pause update shutdown", func(t *testing.T) {
		s, events := newTestSupervisor(t, c)
		spec := Spec{
			ID:      NewID(),
			Name:    "gvxd-ctl-main-" + NewID(),
			Image:   "alpine:3.20",
			Memory:  "512m",
			Command: "sleep 300",
			Pull:    "never",
		}
		if err := s.Start(spec); err != nil {
			t.Fatalf("Start: %v", err)
		}
		waitFor(t, "running", 20*time.Second, func() bool { return s.State(spec.ID).Status == Running }, s.Logs(spec.ID))

		st := s.State(spec.ID)
		if !st.Control {
			t.Fatalf("State.Control = false, want true")
		}
		ctl, ok := s.Control(spec.ID)
		if !ok {
			t.Fatal("Control returned false")
		}
		sockPath := ctl.Target().Socket
		t.Logf("control socket=%q (len=%d)", sockPath, len(sockPath))
		if len(sockPath) > maxSocketPath {
			t.Fatalf("socket path exceeds %d: %q", maxSocketPath, sockPath)
		}

		ping := waitForPing(t, c, ctl.Target())
		if ping.Version != 2 || !ping.Has("exec") || !ping.Has("exec.tty") {
			t.Fatalf("unexpected ping: %+v", ping)
		}

		// 1. Non-TTY exec: streams apart, exit 42, PID > 0.
		ep, err := ctl.Exec(context.Background(), ExecOptions{
			ExecOptions: rungv.ExecOptions{Argv: []string{"sh", "-c", "echo out; echo err >&2; exit 42"}},
		})
		if err != nil {
			t.Fatalf("Exec non-TTY: %v", err)
		}
		outBytes, _ := io.ReadAll(ep.Stdout())
		errBytes, _ := io.ReadAll(ep.Stderr())
		code, werr := ep.Wait()
		_ = ep.Close()
		if werr != nil || code != 42 || ep.PID() <= 0 {
			t.Fatalf("non-TTY Wait=(%d, %v) PID=%d", code, werr, ep.PID())
		}
		if strings.TrimSpace(string(outBytes)) != "out" || strings.TrimSpace(string(errBytes)) != "err" {
			t.Fatalf("streams mismatch: stdout=%q stderr=%q", outBytes, errBytes)
		}

		// 2. Interactive cat with Stdin().Close() -> EOF.
		epCat, err := ctl.Exec(context.Background(), ExecOptions{
			ExecOptions: rungv.ExecOptions{Argv: []string{"cat"}, Interactive: true},
		})
		if err != nil {
			t.Fatalf("Exec cat: %v", err)
		}
		if _, err := epCat.Stdin().Write([]byte("hello-exec-stdin\n")); err != nil {
			t.Fatal(err)
		}
		if err := epCat.Stdin().Close(); err != nil {
			t.Fatal(err)
		}
		catOut, _ := io.ReadAll(epCat.Stdout())
		code, werr = epCat.Wait()
		_ = epCat.Close()
		if werr != nil || code != 0 || string(catOut) != "hello-exec-stdin\n" {
			t.Fatalf("cat: code=%d err=%v out=%q", code, werr, catOut)
		}

		// 3. TTY exec: stty size after Resize(30, 100) -> 30 100, raw input not double-echoed.
		epTTY, err := ctl.Exec(context.Background(), ExecOptions{
			ExecOptions: rungv.ExecOptions{Argv: []string{"/bin/sh"}, Interactive: true, TTY: true},
			Rows:        25,
			Cols:        90,
		})
		if err != nil {
			t.Fatalf("Exec TTY: %v", err)
		}
		if epTTY.Stderr() != nil {
			t.Fatal("TTY exec returned non-nil Stderr")
		}
		var ttyMu sync.Mutex
		var ttyOut bytes.Buffer
		ttyReadDone := make(chan struct{})
		go func() {
			defer close(ttyReadDone)
			buf := make([]byte, 4096)
			for {
				n, rerr := epTTY.Stdout().Read(buf)
				if n > 0 {
					ttyMu.Lock()
					ttyOut.Write(buf[:n])
					ttyMu.Unlock()
				}
				if rerr != nil {
					return
				}
			}
		}()
		waitFor(t, "tty prompt", 15*time.Second, func() bool {
			ttyMu.Lock()
			defer ttyMu.Unlock()
			return strings.Contains(ttyOut.String(), "# ")
		})
		if err := epTTY.Resize(30, 100); err != nil {
			t.Fatalf("Resize(30, 100): %v", err)
		}
		// Give SIGWINCH -> exec.resize a brief moment to propagate.
		time.Sleep(150 * time.Millisecond)
		if _, err := epTTY.Stdin().Write([]byte("stty size\n")); err != nil {
			t.Fatal(err)
		}
		waitFor(t, "stty size 30 100", 15*time.Second, func() bool {
			ttyMu.Lock()
			defer ttyMu.Unlock()
			return strings.Contains(ttyOut.String(), "30 100")
		})
		// Check raw mode: "stty size" should not be double-echoed as "ssttttyy  ssiizzee".
		ttyMu.Lock()
		rawSnap := ttyOut.String()
		ttyMu.Unlock()
		if strings.Contains(rawSnap, "ssttttyy") {
			t.Fatalf("TTY double-echo detected: %q", rawSnap)
		}
		if _, err := epTTY.Stdin().Write([]byte("exit 5\n")); err != nil {
			t.Fatal(err)
		}
		code, werr = epTTY.Wait()
		_ = epTTY.Close()
		<-ttyReadDone
		if werr != nil || code != 5 {
			t.Fatalf("TTY exit: code=%d err=%v out=%q", code, werr, rawSnap)
		}

		// 4. Exec of a missing binary -> error before start (PID == 0).
		epMiss, err := ctl.Exec(context.Background(), ExecOptions{
			ExecOptions: rungv.ExecOptions{Argv: []string{"/no/such/binary"}},
		})
		if err != nil {
			t.Fatalf("Exec missing: %v", err)
		}
		code, werr = epMiss.Wait()
		_ = epMiss.Close()
		t.Logf("missing binary exec: code=%d pid=%d err=%v controlCode=%q", code, epMiss.PID(), werr, rungv.ControlCode(werr))
		if werr == nil || code != -1 || epMiss.PID() != 0 {
			t.Fatalf("missing binary expected pre-start error: code=%d pid=%d err=%v", code, epMiss.PID(), werr)
		}

		// 4b. Close() drops the rungv exec client with SIGKILL without signaling
		// the guest: an exec `sleep 30` keeps running detached and shows in top.
		epDetach, err := ctl.Exec(context.Background(), ExecOptions{
			ExecOptions: rungv.ExecOptions{Argv: []string{"sleep", "30"}},
		})
		if err != nil {
			t.Fatalf("Exec sleep 30: %v", err)
		}
		select {
		case <-epDetach.Started():
		case <-time.After(10 * time.Second):
			t.Fatal("sleep 30 Started timed out")
		}
		sleepPID := epDetach.PID()
		if sleepPID <= 0 {
			t.Fatalf("sleep 30 PID = %d, want > 0", sleepPID)
		}
		if err := epDetach.Close(); err != nil {
			t.Fatalf("epDetach.Close: %v", err)
		}
		// Give any accidental signal a moment to take effect, then check top.
		time.Sleep(250 * time.Millisecond)
		topAfterClose, err := ctl.Top(context.Background())
		if err != nil {
			t.Fatalf("Top after exec Close: %v", err)
		}
		var sleepStillAlive bool
		for _, proc := range topAfterClose {
			if int(proc.PID) == sleepPID && proc.Comm == "sleep" {
				sleepStillAlive = true
				break
			}
		}
		if !sleepStillAlive {
			t.Fatalf("exec sleep 30 (pid=%d) did not survive Close(); top=%+v", sleepPID, topAfterClose)
		}
		// Clean up the detached sleep via explicit Signal/Kill.
		_ = ctl.Kill(context.Background(), rungv.KillOptions{Signal: "SIGKILL", PID: sleepPID})

		// 5. CopyTo then CopyFrom round-trip + StatPath file/dir/root/missing.
		var tarBuf bytes.Buffer
		tw := tar.NewWriter(&tarBuf)
		body := []byte("roundtrip-payload-020\n")
		if err := tw.WriteHeader(&tar.Header{
			Name:     "rt.txt",
			Mode:     0o644,
			Size:     int64(len(body)),
			ModTime:  time.Now().Truncate(time.Second),
			Typeflag: tar.TypeReg,
		}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write(body); err != nil {
			t.Fatal(err)
		}
		if err := tw.Close(); err != nil {
			t.Fatal(err)
		}
		ctx := context.Background()
		if err := ctl.CopyTo(ctx, "/tmp", bytes.NewReader(tarBuf.Bytes()), rungv.CopyOptions{}); err != nil {
			t.Fatalf("CopyTo: %v", err)
		}
		rc, err := ctl.CopyFrom(ctx, "/tmp/rt.txt", rungv.CopyOptions{})
		if err != nil {
			t.Fatalf("CopyFrom: %v", err)
		}
		tr := tar.NewReader(rc)
		hdr, err := tr.Next()
		if err != nil {
			t.Fatalf("tar.Next: %v", err)
		}
		gotBody, err := io.ReadAll(tr)
		if err != nil {
			t.Fatalf("tar read: %v", err)
		}
		if err := rc.Close(); err != nil {
			t.Fatalf("CopyFrom Close: %v", err)
		}
		if hdr.Name != "rt.txt" || !bytes.Equal(gotBody, body) {
			t.Fatalf("CopyFrom got name=%q body=%q", hdr.Name, gotBody)
		}
		psFile, err := ctl.StatPath(ctx, "/tmp/rt.txt")
		if err != nil || psFile.Name != "rt.txt" || psFile.Size != int64(len(body)) || os.FileMode(psFile.Mode).IsDir() {
			t.Fatalf("StatPath file: %+v %v", psFile, err)
		}
		psDir, err := ctl.StatPath(ctx, "/tmp")
		if err != nil || psDir.Name != "tmp" || !os.FileMode(psDir.Mode).IsDir() {
			t.Fatalf("StatPath dir: %+v %v", psDir, err)
		}
		psRoot, err := ctl.StatPath(ctx, "/")
		if err != nil || psRoot.Name != "/" || !os.FileMode(psRoot.Mode).IsDir() {
			t.Fatalf("StatPath /: %+v %v", psRoot, err)
		}
		if _, err := ctl.StatPath(ctx, "/tmp/does-not-exist-020"); rungv.ControlCode(err) != rungv.CodeNotFound {
			t.Fatalf("StatPath missing: err=%v code=%q", err, rungv.ControlCode(err))
		}

		// 6. Stats, StreamStats, Top, Status.
		stats1, err := ctl.Stats(ctx)
		if err != nil || stats1.Memory.TotalBytes == 0 || stats1.PIDs < 1 {
			t.Fatalf("Stats: %+v %v", stats1, err)
		}
		streamCtx, streamCancel := context.WithTimeout(ctx, 5*time.Second)
		var samples int
		_ = ctl.StreamStats(streamCtx, 100*time.Millisecond, func(rungv.Stats) {
			samples++
			if samples >= 2 {
				streamCancel()
			}
		})
		streamCancel()
		if samples < 2 {
			t.Fatalf("StreamStats got %d samples, want >= 2", samples)
		}
		procs, err := ctl.Top(ctx)
		if err != nil || len(procs) == 0 || procs[0].PID != 1 {
			t.Fatalf("Top: %+v %v", procs, err)
		}
		status, err := c.Status(ctx, ctl.Target())
		if err != nil || status.State != "running" {
			t.Fatalf("Status: %+v %v", status, err)
		}

		// 7. Update -m 256m -> /proc/meminfo & ActiveSpec.
		res, err := s.Update(ctx, spec.ID, rungv.Update{Memory: "256m"})
		if err != nil || res.MemoryBytes != 256<<20 {
			t.Fatalf("Update -m 256m: %+v %v", res, err)
		}
		if active, ok := s.ActiveSpec(spec.ID); !ok || active.Memory != "256m" {
			t.Fatalf("ActiveSpec after update: %+v (ok=%v)", active, ok)
		}
		epMem, err := ctl.Exec(ctx, ExecOptions{ExecOptions: rungv.ExecOptions{Argv: []string{"head", "-n", "1", "/proc/meminfo"}}})
		if err != nil {
			t.Fatalf("exec meminfo: %v", err)
		}
		memOut, _ := io.ReadAll(epMem.Stdout())
		_, _ = epMem.Wait()
		_ = epMem.Close()
		if !strings.Contains(string(memOut), "262144 kB") {
			t.Fatalf("/proc/meminfo after 256m update = %q, want 262144 kB", memOut)
		}

		// 8. Pause + verify §2 "to verify" items while paused!
		// Drain old events first.
		for len(events) > 0 {
			<-events
		}
		if err := s.Pause(ctx, spec.ID); err != nil {
			t.Fatalf("Pause: %v", err)
		}
		if !s.State(spec.ID).Paused {
			t.Fatal("State.Paused is false")
		}
		var sawPausedEv bool
		for len(events) > 0 {
			if (<-events).State.Paused {
				sawPausedEv = true
			}
		}
		if !sawPausedEv {
			t.Error("expected EventChanged with Paused=true")
		}
		if _, err := ctl.Exec(ctx, ExecOptions{ExecOptions: rungv.ExecOptions{Argv: []string{"true"}}}); !errors.Is(err, ErrPaused) {
			t.Fatalf("Control.Exec while paused = %v, want ErrPaused", err)
		}

		// §2 verification: direct rungv exec/cp/stats/top while paused.
		{
			cmd := c.ExecCommand(ctx, ctl.Target(), rungv.ExecOptions{Argv: []string{"true"}})
			var se bytes.Buffer
			cmd.Stderr = &se
			execPausedErr := cmd.Run()
			t.Logf("VERIFY paused direct rungv exec: err=%v stderr=%q", execPausedErr, strings.TrimSpace(se.String()))

			cpPausedRC, cpPausedErr := c.CopyFrom(ctx, ctl.Target(), "/tmp/rt.txt", rungv.CopyOptions{})
			if cpPausedRC != nil {
				_, _ = io.ReadAll(cpPausedRC)
				_ = cpPausedRC.Close()
			}
			t.Logf("VERIFY paused rungv cp (from): err=%v code=%q", cpPausedErr, rungv.ControlCode(cpPausedErr))

			cpToPausedErr := c.CopyTo(ctx, ctl.Target(), "/tmp", bytes.NewReader(tarBuf.Bytes()), rungv.CopyOptions{})
			t.Logf("VERIFY paused rungv cp (to): err=%v code=%q", cpToPausedErr, rungv.ControlCode(cpToPausedErr))

			stPaused, statsPausedErr := c.Stats(ctx, ctl.Target())
			t.Logf("VERIFY paused rungv stats: err=%v code=%q mem=%d", statsPausedErr, rungv.ControlCode(statsPausedErr), stPaused.Memory.TotalBytes)

			topPaused, topPausedErr := c.Top(ctx, ctl.Target())
			t.Logf("VERIFY paused rungv top: err=%v code=%q count=%d", topPausedErr, rungv.ControlCode(topPausedErr), len(topPaused))
		}

		if err := s.Unpause(ctx, spec.ID); err != nil {
			t.Fatalf("Unpause: %v", err)
		}
		if s.State(spec.ID).Paused {
			t.Fatal("State.Paused still true after Unpause")
		}

		// 9. Shutdown of sleep with timeout 2s -> ~2s, Stopped, socket removed.
		startStop := time.Now()
		if err := s.Shutdown(spec.ID, "SIGTERM", 2*time.Second); err != nil {
			t.Fatalf("Shutdown: %v", err)
		}
		elapsed := time.Since(startStop)
		t.Logf("Shutdown(sleep, 2s) took %v", elapsed)
		if final := s.State(spec.ID); final.Status != Stopped || final.Paused {
			t.Fatalf("final state = %+v", final)
		}
		if _, err := os.Lstat(sockPath); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("control socket still exists after exit: %v", err)
		}
	})

	t.Run("kill SIGTERM trap and guest SIGKILL exit code", func(t *testing.T) {
		// Kill SIGTERM to trapping PID 1 -> Failed with 7.
		s, _ := newTestSupervisor(t, c)
		spec := Spec{
			ID:      NewID(),
			Name:    "gvxd-ctl-trap-" + NewID(),
			Image:   "alpine:3.20",
			Command: `sh -c 'trap "exit 7" TERM; echo ready; while true; do sleep 1; done'`,
			Pull:    "never",
		}
		if err := s.Start(spec); err != nil {
			t.Fatal(err)
		}
		waitFor(t, "trap ready", 20*time.Second, func() bool {
			for _, l := range s.Logs(spec.ID).Snapshot() {
				if l.Text == "ready" {
					return true
				}
			}
			return false
		}, s.Logs(spec.ID))
		ctl, ok := s.Control(spec.ID)
		if !ok {
			t.Fatal("no control")
		}
		waitForPing(t, c, ctl.Target())
		if err := ctl.Kill(context.Background(), rungv.KillOptions{Signal: "SIGTERM"}); err != nil {
			t.Fatalf("Kill SIGTERM: %v", err)
		}
		waitFor(t, "trap exit", 20*time.Second, func() bool { return !s.State(spec.ID).Status.Active() }, s.Logs(spec.ID))
		st := s.State(spec.ID)
		t.Logf("VERIFY Kill SIGTERM trap: status=%s exitCode=%d signal=%d", st.Status, st.ExitCode, st.Signal)
		if st.Status != Failed || st.ExitCode != 7 {
			t.Fatalf("Kill SIGTERM trap: got %+v, want Failed with exit 7", st)
		}

		// §2 verification: guest SIGKILL of PID 1 exit code.
		spec2 := Spec{
			ID:      NewID(),
			Name:    "gvxd-ctl-sigkill-" + NewID(),
			Image:   "alpine:3.20",
			Command: "sleep 300",
			Pull:    "never",
		}
		if err := s.Start(spec2); err != nil {
			t.Fatal(err)
		}
		waitFor(t, "running", 20*time.Second, func() bool { return s.State(spec2.ID).Status == Running })
		ctl2, ok := s.Control(spec2.ID)
		if !ok {
			t.Fatal("no control")
		}
		waitForPing(t, c, ctl2.Target())
		if err := ctl2.Kill(context.Background(), rungv.KillOptions{Signal: "SIGKILL"}); err != nil {
			t.Fatalf("Kill SIGKILL: %v", err)
		}
		waitFor(t, "sigkill exit", 20*time.Second, func() bool { return !s.State(spec2.ID).Status.Active() }, s.Logs(spec2.ID))
		st2 := s.State(spec2.ID)
		t.Logf("VERIFY guest SIGKILL of PID 1: status=%s exitCode=%d signal=%d", st2.Status, st2.ExitCode, st2.Signal)
		if st2.Status != Failed {
			t.Fatalf("guest SIGKILL expected Failed, got %+v", st2)
		}
	})

	t.Run("paused stop and shutdown and host SIGINT", func(t *testing.T) {
		s, _ := newTestSupervisor(t, c)
		// 1. Direct rungv stop on a paused sandbox (§2 verify).
		spec1 := Spec{
			ID:      NewID(),
			Name:    "gvxd-ctl-pstop-" + NewID(),
			Image:   "alpine:3.20",
			Command: "sleep 300",
			Pull:    "never",
		}
		if err := s.Start(spec1); err != nil {
			t.Fatal(err)
		}
		waitFor(t, "running", 20*time.Second, func() bool { return s.State(spec1.ID).Status == Running })
		ctl1, _ := s.Control(spec1.ID)
		waitForPing(t, c, ctl1.Target())
		if err := s.Pause(context.Background(), spec1.ID); err != nil {
			t.Fatal(err)
		}
		stopCtx, stopCancel := context.WithTimeout(context.Background(), 5*time.Second)
		directStopErr := c.StopGuest(stopCtx, ctl1.Target(), "SIGTERM", 1*time.Second)
		stopCancel()
		waitFor(t, "spec1 exit", 5*time.Second, func() bool { return !s.State(spec1.ID).Status.Active() })
		t.Logf("VERIFY direct rungv stop while paused: err=%v code=%q final=%+v", directStopErr, rungv.ControlCode(directStopErr), s.State(spec1.ID))

		// 2. Supervisor.Shutdown on a paused sandbox -> unpauses, stops, ends Stopped.
		spec2 := Spec{
			ID:      NewID(),
			Name:    "gvxd-ctl-pshutdown-" + NewID(),
			Image:   "alpine:3.20",
			Command: "sleep 300",
			Pull:    "never",
		}
		if err := s.Start(spec2); err != nil {
			t.Fatal(err)
		}
		waitFor(t, "running", 20*time.Second, func() bool { return s.State(spec2.ID).Status == Running })
		ctl2, _ := s.Control(spec2.ID)
		waitForPing(t, c, ctl2.Target())
		if err := s.Pause(context.Background(), spec2.ID); err != nil {
			t.Fatal(err)
		}
		if err := s.Shutdown(spec2.ID, "SIGTERM", 2*time.Second); err != nil {
			t.Fatalf("Shutdown while paused: %v", err)
		}
		if st := s.State(spec2.ID); st.Status != Stopped || st.Paused {
			t.Fatalf("after Shutdown while paused: %+v", st)
		}

		// 3. Host Stop (SIGINT) on a paused sandbox (§2 verify).
		spec3 := Spec{
			ID:      NewID(),
			Name:    "gvxd-ctl-psigint-" + NewID(),
			Image:   "alpine:3.20",
			Command: "sleep 300",
			Pull:    "never",
		}
		if err := s.Start(spec3); err != nil {
			t.Fatal(err)
		}
		waitFor(t, "running", 20*time.Second, func() bool { return s.State(spec3.ID).Status == Running })
		ctl3, _ := s.Control(spec3.ID)
		waitForPing(t, c, ctl3.Target())
		if err := s.Pause(context.Background(), spec3.ID); err != nil {
			t.Fatal(err)
		}
		t0 := time.Now()
		if err := s.Stop(spec3.ID, 5*time.Second); err != nil {
			t.Fatalf("host Stop(SIGINT) while paused: %v", err)
		}
		t.Logf("VERIFY host Stop(SIGINT) while paused: took=%v status=%s paused=%v", time.Since(t0), s.State(spec3.ID).Status, s.State(spec3.ID).Paused)
		if st := s.State(spec3.ID); st.Status != Stopped || st.Paused {
			t.Fatalf("after host Stop while paused: %+v", st)
		}

		// 4. Shutdown grace timeout escalation (process ignoring SIGTERM).
		spec4 := Spec{
			ID:      NewID(),
			Name:    "gvxd-ctl-grace-" + NewID(),
			Image:   "alpine:3.20",
			Command: `sh -c 'trap "" TERM; echo ready; while true; do sleep 1; done'`,
			Pull:    "never",
		}
		if err := s.Start(spec4); err != nil {
			t.Fatal(err)
		}
		waitFor(t, "grace ready", 20*time.Second, func() bool {
			for _, l := range s.Logs(spec4.ID).Snapshot() {
				if l.Text == "ready" {
					return true
				}
			}
			return false
		}, s.Logs(spec4.ID))
		ctl4, _ := s.Control(spec4.ID)
		waitForPing(t, c, ctl4.Target())
		tGrace := time.Now()
		if err := s.Shutdown(spec4.ID, "SIGTERM", 1500*time.Millisecond); err != nil {
			t.Fatalf("Shutdown grace: %v", err)
		}
		graceElapsed := time.Since(tGrace)
		t.Logf("VERIFY Shutdown(trap '' TERM, 1.5s): took=%v status=%s exitCode=%d", graceElapsed, s.State(spec4.ID).Status, s.State(spec4.ID).ExitCode)
		if graceElapsed < time.Second || s.State(spec4.ID).Status != Stopped {
			t.Fatalf("Shutdown grace: elapsed=%v state=%+v", graceElapsed, s.State(spec4.ID))
		}
	})

	t.Run("privileged exec in unprivileged sandbox", func(t *testing.T) {
		s, _ := newTestSupervisor(t, c)
		spec := Spec{
			ID:           NewID(),
			Name:         "gvxd-ctl-unpriv-" + NewID(),
			Image:        "alpine:3.20",
			Command:      "sleep 300",
			Unprivileged: true,
			Pull:         "never",
		}
		if err := s.Start(spec); err != nil {
			t.Fatal(err)
		}
		waitFor(t, "running", 20*time.Second, func() bool { return s.State(spec.ID).Status == Running })
		ctl, _ := s.Control(spec.ID)
		waitForPing(t, c, ctl.Target())

		tr := true
		fa := false
		for _, tc := range []struct {
			label string
			priv  *bool
		}{
			{"privileged=true", &tr},
			{"privileged=false", &fa},
		} {
			ep, err := ctl.Exec(context.Background(), ExecOptions{
				ExecOptions: rungv.ExecOptions{
					Argv:       []string{"sh", "-c", "grep CapEff /proc/self/status"},
					Privileged: tc.priv,
				},
			})
			if err != nil {
				t.Fatalf("%s Exec: %v", tc.label, err)
			}
			out, _ := io.ReadAll(ep.Stdout())
			code, werr := ep.Wait()
			_ = ep.Close()
			t.Logf("VERIFY %s in --privileged=false sandbox: code=%d err=%v out=%q", tc.label, code, werr, strings.TrimSpace(string(out)))
		}
		_ = s.Stop(spec.ID, 5*time.Second)
	})

	t.Run("socket path containing a space", func(t *testing.T) {
		// Create a short directory under /tmp with a space so len(sock) <= 100 and the space is exercised!
		spaceDir, err := os.MkdirTemp("/tmp", "gvxd App Support ")
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.RemoveAll(spaceDir) })
		if err := os.Chmod(spaceDir, 0o700); err != nil {
			t.Fatal(err)
		}

		s := &Supervisor{
			Client:  func() *rungv.Client { return c },
			DataDir: spaceDir,
		}
		t.Cleanup(func() { s.StopAll(10 * time.Second) })

		spec := Spec{
			ID:      NewID(),
			Name:    "gvxd-ctl-space-" + NewID(),
			Image:   "alpine:3.20",
			Command: "sleep 300",
			Pull:    "never",
		}
		if err := s.Start(spec); err != nil {
			t.Fatalf("Start with space in DataDir: %v", err)
		}
		waitFor(t, "running", 20*time.Second, func() bool { return s.State(spec.ID).Status == Running }, s.Logs(spec.ID))
		ctl, ok := s.Control(spec.ID)
		if !ok {
			t.Fatal("Control returned false")
		}
		sock := ctl.Target().Socket
		t.Logf("VERIFY space socket path: %q (len=%d)", sock, len(sock))
		if !strings.Contains(sock, " ") {
			t.Fatalf("expected space in socket path, got %q", sock)
		}
		p := waitForPing(t, c, ctl.Target())
		if p.Version != 2 {
			t.Fatalf("ping over space socket: %+v", p)
		}
		ep, err := ctl.Exec(context.Background(), ExecOptions{
			ExecOptions: rungv.ExecOptions{Argv: []string{"echo", "space-ok"}},
		})
		if err != nil {
			t.Fatalf("exec over space socket: %v", err)
		}
		out, _ := io.ReadAll(ep.Stdout())
		code, werr := ep.Wait()
		_ = ep.Close()
		if werr != nil || code != 0 || strings.TrimSpace(string(out)) != "space-ok" {
			t.Fatalf("exec over space socket: code=%d err=%v out=%q", code, werr, out)
		}
		_ = s.Stop(spec.ID, 5*time.Second)
		if _, err := os.Lstat(sock); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("space socket left behind: %v", err)
		}
	})
}
