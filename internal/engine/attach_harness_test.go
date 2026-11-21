//go:build !windows

package engine

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os/exec"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/creack/pty"
)

func TestHarnessStreamsCLI(t *testing.T) {
	h := startHarness(t)

	// 1. docker run --rm alpine:3.20 echo ok
	out, errb, code := h.docker(t, "", "run", "--rm", "--name", "gvxd-echo", "alpine:3.20", "echo", "ok")
	if code != 0 || strings.TrimSpace(out) != "ok" {
		t.Fatalf("run echo: code=%d out=%q err=%q", code, out, errb)
	}

	// 2. echo hi | docker run -i --rm alpine:3.20 cat
	out, errb, code = h.docker(t, "hi\n", "run", "-i", "--rm", "--name", "gvxd-cat", "alpine:3.20", "cat")
	if code != 0 || out != "hi\n" {
		t.Fatalf("run -i cat: code=%d out=%q err=%q", code, out, errb)
	}

	// 3. docker run -it via creack/pty (tty shown, exit code propagated)
	t.Run("run -it via pty", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		cmd := exec.CommandContext(ctx, "docker", "run", "-it", "--rm", "--name", "gvxd-tty", "alpine:3.20", "/bin/sh")
		cmd.Env = h.env()
		ptmx, err := pty.StartWithSize(cmd, &pty.Winsize{Rows: 30, Cols: 90})
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

		// Wait briefly for the prompt, verify fd 0 is a tty and received the
		// 30x90 resize from the CLI, then exit with code 3.
		deadline := time.Now().Add(30 * time.Second)
		for time.Now().Before(deadline) {
			bufMu.Lock()
			hasPrompt := strings.Contains(buf.String(), "# ")
			bufMu.Unlock()
			if hasPrompt {
				break
			}
			time.Sleep(100 * time.Millisecond)
		}
		_, _ = io.WriteString(ptmx, "test -t 0 && echo ISATTY; stty size; exit 3\n")
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
		t.Logf("tty output:\n%s", got)
		if !strings.Contains(got, "ISATTY") {
			t.Errorf("expected fd 0 to be a tty (ISATTY), got %q", got)
		}
		if !strings.Contains(got, "30 90") {
			t.Errorf("expected resized stty size 30 90 in output, got %q", got)
		}
		if exitCode != 3 {
			t.Errorf("exit code = %d, want 3", exitCode)
		}
	})

	// 4. docker logs and docker logs -f
	out, errb, code = h.docker(t, "", "run", "-d", "--name", "gvxd-logs", "alpine:3.20", "sh", "-c", "echo first; sleep 0.5; echo second; echo errline >&2")
	if code != 0 {
		t.Fatalf("run -d: code=%d out=%q err=%q", code, out, errb)
	}
	out, errb, code = h.docker(t, "", "logs", "-f", "gvxd-logs")
	if code != 0 {
		t.Fatalf("logs -f: code=%d out=%q err=%q", code, out, errb)
	}
	if !strings.Contains(out, "first") || !strings.Contains(out, "second") || !strings.Contains(errb, "errline") {
		t.Fatalf("logs -f out=%q err=%q", out, errb)
	}
	out, errb, code = h.docker(t, "", "logs", "--tail", "1", "gvxd-logs")
	if code != 0 {
		t.Fatalf("logs --tail 1: code=%d out=%q err=%q", code, out, errb)
	}
	if strings.Contains(out, "first") {
		t.Fatalf("logs --tail 1 included old line: out=%q err=%q", out, errb)
	}
	_, _, _ = h.docker(t, "", "rm", "-f", "gvxd-logs")
}
