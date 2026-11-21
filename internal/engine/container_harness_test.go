//go:build !windows

package engine

// Owner: workstream B (containers). Real docker CLI verification against
// startHarness (GVX_ENGINE_HARNESS=1).

import (
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestContainerHarnessCLI(t *testing.T) {
	h := startHarness(t)

	// 1. Run a short-lived --rm container.
	out, _, code := h.docker(t, "", "run", "--rm", "alpine:3.20", "echo", "ok")
	if code != 0 || strings.TrimSpace(out) != "ok" {
		t.Fatalf("docker run --rm alpine:3.20 echo ok: code=%d out=%q", code, out)
	}

	// 2. Run detached nginx on 18095 + HTTP GET.
	_, _, code = h.docker(t, "", "run", "-d", "--name", "gvxd-web", "-p", "18095:80", "nginx:alpine")
	if code != 0 {
		t.Fatalf("docker run nginx failed: code=%d", code)
	}
	var body string
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		resp, err := http.Get("http://127.0.0.1:18095/")
		if err == nil {
			b, _ := io.ReadAll(resp.Body)
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusOK && strings.Contains(string(b), "Welcome to nginx") {
				body = string(b)
				break
			}
		}
		time.Sleep(250 * time.Millisecond)
	}
	if body == "" {
		t.Fatal("failed to reach nginx on http://127.0.0.1:18095/")
	}

	// 3. ps, ps -a, inspect.
	out, _, code = h.docker(t, "", "ps")
	if code != 0 || !strings.Contains(out, "gvxd-web") {
		t.Fatalf("docker ps missing gvxd-web: code=%d out=%q", code, out)
	}
	out, _, code = h.docker(t, "", "inspect", "--format", "{{.State.Status}}", "gvxd-web")
	if code != 0 || strings.TrimSpace(out) != "running" {
		t.Fatalf("docker inspect gvxd-web: code=%d out=%q", code, out)
	}

	// 4. stop, start, rm -f.
	if _, _, code = h.docker(t, "", "stop", "gvxd-web"); code != 0 {
		t.Fatalf("docker stop gvxd-web: code=%d", code)
	}
	if _, _, code = h.docker(t, "", "start", "gvxd-web"); code != 0 {
		t.Fatalf("docker start gvxd-web: code=%d", code)
	}
	if _, _, code = h.docker(t, "", "rm", "-f", "gvxd-web"); code != 0 {
		t.Fatalf("docker rm -f gvxd-web: code=%d", code)
	}

	// 5. Named volume written by one run and read by the next.
	if _, _, code = h.docker(t, "", "volume", "create", "gvxd-vol"); code != 0 {
		t.Fatalf("docker volume create: code=%d", code)
	}
	if _, _, code = h.docker(t, "", "run", "--rm", "-v", "gvxd-vol:/data", "alpine:3.20", "sh", "-c", "echo hello-vol > /data/msg"); code != 0 {
		t.Fatalf("write named volume: code=%d", code)
	}
	out, _, code = h.docker(t, "", "run", "--rm", "-v", "gvxd-vol:/data", "alpine:3.20", "cat", "/data/msg")
	if code != 0 || strings.TrimSpace(out) != "hello-vol" {
		t.Fatalf("read named volume: code=%d out=%q", code, out)
	}

	// 6. Bind mount of a temp dir.
	bindDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(bindDir, "in.txt"), []byte("from-host\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	out, _, code = h.docker(t, "", "run", "--rm", "-v", bindDir+":/mnt", "alpine:3.20", "sh", "-c", "cat /mnt/in.txt && echo from-guest > /mnt/out.txt")
	if code != 0 || strings.TrimSpace(out) != "from-host" {
		t.Fatalf("bind mount run: code=%d out=%q", code, out)
	}
	b, err := os.ReadFile(filepath.Join(bindDir, "out.txt"))
	if err != nil || strings.TrimSpace(string(b)) != "from-guest" {
		t.Fatalf("bind mount host file = %q, err=%v", string(b), err)
	}

	// 7. Harness restart keeps a stopped container in ps -a and restarts an always container.
	if _, _, code = h.docker(t, "", "run", "--name", "gvxd-kept", "alpine:3.20", "echo", "done"); code != 0 {
		t.Fatalf("run gvxd-kept: code=%d", code)
	}
	if _, _, code = h.docker(t, "", "run", "-d", "--name", "gvxd-always", "--restart", "always", "alpine:3.20", "sleep", "300"); code != 0 {
		t.Fatalf("run gvxd-always: code=%d", code)
	}

	h.restart(t)

	out, _, code = h.docker(t, "", "ps", "-a")
	if code != 0 || !strings.Contains(out, "gvxd-kept") || !strings.Contains(out, "gvxd-always") {
		t.Fatalf("ps -a after restart missing containers: code=%d out=%q", code, out)
	}
	out, _, code = h.docker(t, "", "inspect", "--format", "{{.State.Status}}", "gvxd-kept")
	if code != 0 || strings.TrimSpace(out) != "exited" {
		t.Fatalf("gvxd-kept status after restart = %q, want exited", out)
	}
	out, _, code = h.docker(t, "", "inspect", "--format", "{{.State.Status}}", "gvxd-always")
	if code != 0 || strings.TrimSpace(out) != "running" {
		t.Fatalf("gvxd-always status after restart = %q, want running", out)
	}

	_, _, _ = h.docker(t, "", "rm", "-f", "gvxd-always", "gvxd-kept")
	_, _, _ = h.docker(t, "", "volume", "rm", "gvxd-vol")

	// 8. docker build without the gvx-desktop builder prints 501 directing the
	// user to gvx-desktop and leaves ps -a empty (no BuildKit container booted).
	buildDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(buildDir, "Dockerfile"), []byte("FROM alpine:3.20\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	bOut, bErr, bCode := h.docker(t, "", "build", buildDir)
	combined := bOut + bErr
	if bCode == 0 || !strings.Contains(combined, BuilderName) {
		t.Fatalf("docker build: code=%d output=%q, want non-zero mentioning %q", bCode, combined, BuilderName)
	}
	psOut, _, psCode := h.docker(t, "", "ps", "-a", "-q")
	if psCode != 0 || strings.TrimSpace(psOut) != "" {
		t.Fatalf("ps -a after docker build not empty: code=%d out=%q", psCode, psOut)
	}
}
