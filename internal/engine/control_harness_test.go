//go:build !windows

package engine

// Owner: W2 (docs/design/gvx-v020.md §5 & §9).
// Real docker CLI harness tests for rungv v0.20 runtime commands: exec (non-tty,
// -i, -it via PTY + resize, -d, exit codes, -u/-w/-e, GVX_NET blanked), cp both
// ways, pause/unpause, top, update, kill, stop timing, stats --no-stream, and
// live network connect between Compose services.
//
// Skipped unless GVX_ENGINE_HARNESS=1, without -short, and with rungv v0.20+.

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/creack/pty"
)

func TestHarnessControlV020(t *testing.T) {
	h := startHarness(t)
	if !h.e.Features().Control {
		t.Skip("rungv lacks v0.20 control protocol")
	}
	if _, errOut, code := h.docker(t, "", "pull", "alpine:3.20"); code != 0 {
		t.Fatalf("pull alpine:3.20 (%d): %s", code, errOut)
	}

	t.Run("exec_cp_pause_top_update_stats", func(t *testing.T) {
		t.Cleanup(func() {
			_, _, _ = h.docker(t, "", "rm", "-f", "gvxd-ctl-main")
			_, _, _ = h.docker(t, "", "network", "rm", "gvxd-ctl-net")
		})
		if _, errOut, code := h.docker(t, "", "network", "create", "gvxd-ctl-net"); code != 0 {
			t.Fatalf("network create: %s", errOut)
		}
		if _, errOut, code := h.docker(t, "", "run", "-d", "--name", "gvxd-ctl-main",
			"--network", "gvxd-ctl-net", "-m", "512m", "alpine:3.20", "sleep", "300"); code != 0 {
			t.Fatalf("run: %s", errOut)
		}

		// 1. Non-TTY exec: echo ok, exit code, -i stdin, -u/-w/-e, GVX_NET blanked.
		out, errOut, code := h.docker(t, "", "exec", "gvxd-ctl-main", "echo", "ok")
		if code != 0 || strings.TrimSpace(out) != "ok" {
			t.Fatalf("exec echo (%d): out=%q err=%q", code, out, errOut)
		}
		_, _, code = h.docker(t, "", "exec", "gvxd-ctl-main", "sh", "-c", "exit 3")
		if code != 3 {
			t.Fatalf("exec exit 3 = %d, want 3", code)
		}
		out, errOut, code = h.docker(t, "hi\n", "exec", "-i", "gvxd-ctl-main", "cat")
		if code != 0 || out != "hi\n" {
			t.Fatalf("exec -i cat (%d): out=%q err=%q", code, out, errOut)
		}
		out, errOut, code = h.docker(t, "", "exec", "-u", "nobody", "-w", "/tmp", "-e", "FOO=bar",
			"gvxd-ctl-main", "sh", "-c", `printf "%s|%s|%s|%s\n" "$(id -un)" "$(pwd)" "$FOO" "$GVX_NET"`)
		if code != 0 || strings.TrimSpace(out) != "nobody|/tmp|bar|" {
			t.Fatalf("exec -u -w -e (%d): out=%q err=%q", code, out, errOut)
		}
		// Root exec keeps guest root caps (e.g. chown succeeds); non-root exec does not.
		if _, errOut, code := h.docker(t, "", "exec", "gvxd-ctl-main", "sh", "-c", "touch /root/captest && chown nobody /root/captest && rm /root/captest"); code != 0 {
			t.Fatalf("root exec chown failed (%d): %s", code, errOut)
		}
		if _, _, code := h.docker(t, "", "exec", "-u", "nobody", "gvxd-ctl-main", "sh", "-c", "touch /tmp/captest_u && chown root /tmp/captest_u"); code == 0 {
			t.Fatal("non-root exec chown root unexpectedly succeeded")
		}

		// 2. exec -it via creack/pty with resize and exit code 5.
		t.Run("exec_it_pty", func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, "docker", "exec", "-it", "gvxd-ctl-main", "/bin/sh")
			cmd.Env = h.env()
			ptmx, err := pty.StartWithSize(cmd, &pty.Winsize{Rows: 24, Cols: 80})
			if err != nil {
				t.Fatal(err)
			}
			defer ptmx.Close()

			var (
				bufMu sync.Mutex
				buf   bytes.Buffer
			)
			readDone := make(chan struct{})
			go func() {
				defer close(readDone)
				tmp := make([]byte, 4096)
				for {
					n, rerr := ptmx.Read(tmp)
					if n > 0 {
						bufMu.Lock()
						buf.Write(tmp[:n])
						bufMu.Unlock()
					}
					if rerr != nil {
						return
					}
				}
			}()

			deadline := time.Now().Add(15 * time.Second)
			for time.Now().Before(deadline) {
				bufMu.Lock()
				hasPrompt := strings.Contains(buf.String(), "# ")
				bufMu.Unlock()
				if hasPrompt {
					break
				}
				time.Sleep(100 * time.Millisecond)
			}
			_ = pty.Setsize(ptmx, &pty.Winsize{Rows: 31, Cols: 101})
			time.Sleep(300 * time.Millisecond)
			_, _ = io.WriteString(ptmx, "test -t 0 && echo EXEC_TTY; stty size; exit 5\n")
			err = cmd.Wait()
			<-readDone
			exitCode := 0
			if err != nil {
				if ee, ok := errors.AsType[*exec.ExitError](err); ok {
					exitCode = ee.ExitCode()
				} else if !errors.Is(err, syscall.EIO) {
					t.Fatalf("cmd.Wait: %v", err)
				}
			}
			bufMu.Lock()
			got := buf.String()
			bufMu.Unlock()
			t.Logf("exec -it output (%d):\n%s", exitCode, got)
			if exitCode != 5 {
				t.Errorf("exit code = %d, want 5", exitCode)
			}
			if !strings.Contains(got, "EXEC_TTY") || !strings.Contains(got, "31 101") {
				t.Errorf("expected EXEC_TTY and 31 101, got %q", got)
			}
		})

		// 3. exec -d then verify background process via exec ps.
		if _, errOut, code := h.docker(t, "", "exec", "-d", "gvxd-ctl-main", "sleep", "47"); code != 0 {
			t.Fatalf("exec -d (%d): %s", code, errOut)
		}
		out, errOut, code = h.docker(t, "", "exec", "gvxd-ctl-main", "ps")
		if code != 0 || !strings.Contains(out, "sleep 47") {
			t.Fatalf("exec ps after -d (%d): out=%q err=%q", code, out, errOut)
		}

		// 4. docker cp both ways and streaming to stdout (-).
		tmpDir := t.TempDir()
		srcFile := filepath.Join(tmpDir, "upload.txt")
		if err := os.WriteFile(srcFile, []byte("from-host-cp\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, errOut, code := h.docker(t, "", "cp", srcFile, "gvxd-ctl-main:/tmp/upload.txt"); code != 0 {
			t.Fatalf("cp to container (%d): %s", code, errOut)
		}
		dstFile := filepath.Join(tmpDir, "download.txt")
		if _, errOut, code := h.docker(t, "", "cp", "gvxd-ctl-main:/tmp/upload.txt", dstFile); code != 0 {
			t.Fatalf("cp from container (%d): %s", code, errOut)
		}
		gotBytes, err := os.ReadFile(dstFile)
		if err != nil || string(gotBytes) != "from-host-cp\n" {
			t.Fatalf("downloaded file = %q err=%v", gotBytes, err)
		}
		out, errOut, code = h.docker(t, "", "cp", "gvxd-ctl-main:/etc/hostname", "-")
		if code != 0 || !strings.Contains(out, "hostname") {
			t.Fatalf("cp to stdout (%d): len=%d err=%q", code, len(out), errOut)
		}

		// 5. docker top.
		out, errOut, code = h.docker(t, "", "top", "gvxd-ctl-main")
		if code != 0 || !strings.Contains(out, "sleep 300") {
			t.Fatalf("top (%d): out=%q err=%q", code, out, errOut)
		}

		// 6. docker update --memory 256m -> guest /proc/meminfo follows at once.
		if _, errOut, code := h.docker(t, "", "update", "--memory", "256m", "gvxd-ctl-main"); code != 0 {
			t.Fatalf("update (%d): %s", code, errOut)
		}
		out, errOut, code = h.docker(t, "", "exec", "gvxd-ctl-main", "grep", "MemTotal", "/proc/meminfo")
		if code != 0 || !strings.Contains(out, "262144") {
			t.Fatalf("meminfo after update (%d): out=%q err=%q", code, out, errOut)
		}

		// 7. docker stats --no-stream reports non-zero memory.
		out, errOut, code = h.docker(t, "", "stats", "--no-stream", "--format", "{{.Name}} {{.MemUsage}}", "gvxd-ctl-main")
		if code != 0 || !strings.Contains(out, "gvxd-ctl-main") || strings.Contains(out, "0B /") {
			t.Fatalf("stats (%d): out=%q err=%q", code, out, errOut)
		}

		// 8. docker pause / ps (Paused) / unpause.
		if _, errOut, code := h.docker(t, "", "pause", "gvxd-ctl-main"); code != 0 {
			t.Fatalf("pause (%d): %s", code, errOut)
		}
		out, errOut, code = h.docker(t, "", "ps", "--filter", "name=gvxd-ctl-main")
		if code != 0 || !strings.Contains(out, "(Paused)") {
			t.Fatalf("ps while paused (%d): out=%q err=%q", code, out, errOut)
		}
		if _, errOut, code := h.docker(t, "", "unpause", "gvxd-ctl-main"); code != 0 {
			t.Fatalf("unpause (%d): %s", code, errOut)
		}
	})

	t.Run("kill_trap_and_stop_timing", func(t *testing.T) {
		t.Cleanup(func() {
			_, _, _ = h.docker(t, "", "rm", "-f", "gvxd-ctl-trap", "gvxd-ctl-sleep")
		})
		// Kill SIGTERM reaches guest trap.
		if _, errOut, code := h.docker(t, "", "run", "-d", "--name", "gvxd-ctl-trap", "alpine:3.20",
			"sh", "-c", `trap 'echo CAUGHT_TERM; exit 7' TERM; while true; do sleep 1; done`); code != 0 {
			t.Fatalf("run trap: %s", errOut)
		}
		time.Sleep(500 * time.Millisecond)
		if _, errOut, code := h.docker(t, "", "kill", "-s", "TERM", "gvxd-ctl-trap"); code != 0 {
			t.Fatalf("kill TERM: %s", errOut)
		}
		out, errOut, code := h.docker(t, "", "wait", "gvxd-ctl-trap")
		if code != 0 || strings.TrimSpace(out) != "7" {
			t.Fatalf("wait after SIGTERM trap = %q (%d, %s), want 7", out, code, errOut)
		}

		// Stop -t 2 of a container ignoring SIGTERM waits ~2 s before SIGKILL.
		if _, errOut, code := h.docker(t, "", "run", "-d", "--name", "gvxd-ctl-sleep", "alpine:3.20",
			"sh", "-c", `trap "" TERM; while true; do sleep 1; done`); code != 0 {
			t.Fatalf("run sleep: %s", errOut)
		}
		// Wait until the guest and control socket are ready before timing stop.
		ready := false
		for range 20 {
			if _, _, code := h.docker(t, "", "exec", "gvxd-ctl-sleep", "true"); code == 0 {
				ready = true
				break
			}
			time.Sleep(100 * time.Millisecond)
		}
		if !ready {
			t.Fatal("gvxd-ctl-sleep never became ready for exec")
		}
		start := time.Now()
		if _, errOut, code := h.docker(t, "", "stop", "-t", "2", "gvxd-ctl-sleep"); code != 0 {
			t.Fatalf("stop -t 2: %s", errOut)
		}
		elapsed := time.Since(start)
		t.Logf("docker stop -t 2 elapsed: %v", elapsed)
		if elapsed < 1500*time.Millisecond || elapsed > 12*time.Second {
			t.Errorf("stop -t 2 took %v, expected ~2s", elapsed)
		}
	})

	t.Run("compose_live_network_connect", func(t *testing.T) {
		dir := t.TempDir()
		file := filepath.Join(dir, "compose.yaml")
		yaml := `services:
  srv:
    image: alpine:3.20
    networks: [netb]
    command: ["sh", "-c", 'while true; do printf "HTTP/1.0 200 OK\r\n\r\nlive-connect-ok\n" | nc -l -p 8080; done']
  cli:
    image: alpine:3.20
    networks: [neta]
    command: ["sleep", "300"]
networks:
  neta: {}
  netb: {}
`
		if err := os.WriteFile(file, []byte(yaml), 0o600); err != nil {
			t.Fatal(err)
		}
		compose := func(args ...string) (string, string, int) {
			return h.docker(t, "", append([]string{"compose", "-p", "gvxd-live", "-f", file}, args...)...)
		}
		t.Cleanup(func() { compose("down", "-v", "-t", "0") })
		if _, errOut, code := compose("up", "-d"); code != 0 {
			t.Fatalf("compose up: %s", errOut)
		}

		// Before connect, cli cannot reach srv.
		if _, _, code := h.docker(t, "", "exec", "gvxd-live-cli-1", "wget", "-qO-", "-T", "2", "http://srv:8080/"); code == 0 {
			t.Fatal("cli reached srv before network connect")
		}

		// Connect running cli to gvxd-live_netb at runtime (no restart).
		if _, errOut, code := h.docker(t, "", "network", "connect", "gvxd-live_netb", "gvxd-live-cli-1"); code != 0 {
			t.Fatalf("network connect: %s", errOut)
		}
		srvIP, errOut, code := h.docker(t, "", "inspect", "-f", "{{range .NetworkSettings.Networks}}{{.IPAddress}}{{end}}", "gvxd-live-srv-1")
		if code != 0 || strings.TrimSpace(srvIP) == "" {
			t.Fatalf("inspect srv IP (%d): %q %q", code, srvIP, errOut)
		}
		srvIP = strings.TrimSpace(srvIP)

		// cli reaches srv by name AND by IP without a restart.
		for _, target := range []string{"srv", srvIP} {
			out, errOut, code := h.docker(t, "", "exec", "gvxd-live-cli-1", "sh", "-c",
				"for i in $(seq 1 15); do wget -qO- -T 2 http://"+target+":8080/ && exit 0; sleep 0.3; done; exit 1")
			if code != 0 || !strings.Contains(out, "live-connect-ok") {
				t.Fatalf("reach %s after live connect (%d): out=%q err=%q", target, code, out, errOut)
			}
		}

		// Disconnect cli from all user networks: 10.89 routes must be gone,
		// gvx0 down, and eth0 default route / host egress intact.
		if _, errOut, code := h.docker(t, "", "network", "disconnect", "gvxd-live_netb", "gvxd-live-cli-1"); code != 0 {
			t.Fatalf("disconnect netb: %s", errOut)
		}
		if _, errOut, code := h.docker(t, "", "network", "disconnect", "gvxd-live_neta", "gvxd-live-cli-1"); code != 0 {
			t.Fatalf("disconnect neta: %s", errOut)
		}
		routesOut, errOut, code := h.docker(t, "", "exec", "gvxd-live-cli-1", "ip", "route")
		if code != 0 {
			t.Fatalf("ip route after disconnect (%d): %s", code, errOut)
		}
		if strings.Contains(routesOut, "10.89.") || strings.Contains(routesOut, "gvx0") {
			t.Errorf("stale overlay route remained after disconnect to zero addrs:\n%s", routesOut)
		}
		if !strings.Contains(routesOut, "default via 10.0.2.2 dev eth0") {
			t.Errorf("default eth0 route missing after disconnect:\n%s", routesOut)
		}
		if out, errOut, code := h.docker(t, "", "exec", "gvxd-live-cli-1", "sh", "-c", "nslookup registry-1.docker.io >/dev/null && echo ok"); code != 0 || !strings.Contains(out, "ok") {
			t.Errorf("egress/DNS broken after disconnect (%d): out=%q err=%q", code, out, errOut)
		}
	})
}
