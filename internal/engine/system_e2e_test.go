//go:build !windows

package engine

// Owner: workstream C.
// Real docker CLI harness tests for workstream C acceptance and full
// end-to-end verification. Skipped unless GVX_ENGINE_HARNESS=1 and without
// -short.

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/creack/pty"
)

func TestHarnessSystemC(t *testing.T) {
	h := startHarness(t)

	// 1. docker version
	out, errOut, code := h.docker(t, "", "version")
	if code != 0 || !strings.Contains(out, "gvx-desktop-0.0.0-harness") {
		t.Fatalf("docker version failed (%d): out=%q err=%q", code, out, errOut)
	}

	// 2. docker info
	out, errOut, code = h.docker(t, "", "info")
	if code != 0 || !strings.Contains(out, "gvx-desktop-0.0.0-harness") {
		t.Fatalf("docker info failed (%d): out=%q err=%q", code, out, errOut)
	}

	// 3. docker pull alpine:3.20
	out, errOut, code = h.docker(t, "", "pull", "alpine:3.20")
	if code != 0 {
		t.Fatalf("docker pull alpine:3.20 failed (%d): out=%q err=%q", code, out, errOut)
	}

	// 4. docker images
	out, errOut, code = h.docker(t, "", "images")
	if code != 0 || !strings.Contains(out, "alpine") {
		t.Fatalf("docker images failed (%d): out=%q err=%q", code, out, errOut)
	}

	// 5. docker tag + rmi
	if _, errOut, code := h.docker(t, "", "tag", "alpine:3.20", "gvxd-tag:v1"); code != 0 {
		t.Fatalf("docker tag failed (%d): %s", code, errOut)
	}
	if _, errOut, code := h.docker(t, "", "rmi", "gvxd-tag:v1"); code != 0 {
		t.Fatalf("docker rmi failed (%d): %s", code, errOut)
	}

	// 6. docker volume create/ls/inspect/rm
	if out, errOut, code := h.docker(t, "", "volume", "create", "gvxd-cvol"); code != 0 || strings.TrimSpace(out) != "gvxd-cvol" {
		t.Fatalf("volume create (%d): out=%q err=%q", code, out, errOut)
	}
	if out, errOut, code := h.docker(t, "", "volume", "ls"); code != 0 || !strings.Contains(out, "gvxd-cvol") {
		t.Fatalf("volume ls (%d): out=%q err=%q", code, out, errOut)
	}
	if out, errOut, code := h.docker(t, "", "volume", "inspect", "gvxd-cvol"); code != 0 || !strings.Contains(out, "gvxd-cvol") {
		t.Fatalf("volume inspect (%d): out=%q err=%q", code, out, errOut)
	}
	if _, errOut, code := h.docker(t, "", "volume", "rm", "gvxd-cvol"); code != 0 {
		t.Fatalf("volume rm (%d): %s", code, errOut)
	}

	// 7. docker network ls/create/rm
	if out, errOut, code := h.docker(t, "", "network", "ls"); code != 0 || !strings.Contains(out, "bridge") {
		t.Fatalf("network ls (%d): out=%q err=%q", code, out, errOut)
	}
	if _, errOut, code := h.docker(t, "", "network", "create", "gvxd-cnet"); code != 0 {
		t.Fatalf("network create (%d): %s", code, errOut)
	}
	if _, errOut, code := h.docker(t, "", "network", "rm", "gvxd-cnet"); code != 0 {
		t.Fatalf("network rm (%d): %s", code, errOut)
	}

	// 8. docker system df
	if out, errOut, code := h.docker(t, "", "system", "df"); code != 0 || !strings.Contains(out, "Images") {
		t.Fatalf("system df (%d): out=%q err=%q", code, out, errOut)
	}

	// 9. docker events receives an emitted container start event
	evCtx, evCancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer evCancel()
	evCmd := exec.CommandContext(evCtx, "docker", "events", "--format", "{{.Type}} {{.Action}} {{.Actor.ID}}")
	evCmd.Env = h.env()
	evOut, err := evCmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := evCmd.Start(); err != nil {
		t.Fatal(err)
	}
	evLine := make(chan string, 1)
	go func() {
		sc := bufio.NewScanner(evOut)
		if sc.Scan() {
			evLine <- sc.Text()
		}
	}()
	time.Sleep(300 * time.Millisecond)
	h.e.emit(Event{Type: "container", Action: "start", ID: "gvxd-evtest", Attributes: map[string]string{"name": "gvxd-evtest"}})
	select {
	case got := <-evLine:
		if !strings.Contains(got, "container start gvxd-evtest") {
			t.Errorf("docker events got %q, want container start gvxd-evtest", got)
		}
	case <-evCtx.Done():
		t.Errorf("timed out waiting for docker events")
	}
	evCancel()
	_ = evCmd.Wait()

	// 10. docker exec (works with rungv v0.20+, 501 with v0.19) & docker build 501
	t.Cleanup(func() { _, _, _ = h.docker(t, "", "rm", "-f", "gvxd-exec501") })
	if _, errOut, code := h.docker(t, "", "run", "-d", "--name", "gvxd-exec501", "alpine:3.20", "sleep", "30"); code != 0 {
		t.Fatalf("run gvxd-exec501 (%d): %s", code, errOut)
	}
	out, errOut, code = h.docker(t, "", "exec", "gvxd-exec501", "echo", "exec-ok")
	if h.e.Features().Control {
		if code != 0 || strings.TrimSpace(out) != "exec-ok" {
			t.Errorf("docker exec = (%d) out=%q err=%q, want 0 exec-ok", code, out, errOut)
		}
	} else {
		if code == 0 || !strings.Contains(errOut, "not supported by GVX Desktop's experimental sandbox engine yet") {
			t.Errorf("docker exec = (%d) %q, want 501 unsupported message", code, errOut)
		}
	}
	_, _, _ = h.docker(t, "", "rm", "-f", "gvxd-exec501")

	buildDir := t.TempDir()
	_ = os.WriteFile(filepath.Join(buildDir, "Dockerfile"), []byte("FROM alpine:3.20\n"), 0o600)
	imgsBefore, _, _ := h.docker(t, "", "images")
	_, errOut, code = h.docker(t, "", "build", buildDir)
	if code == 0 || !strings.Contains(errOut, "docker buildx use --default "+BuilderName) {
		t.Errorf("docker build = (%d) %q, want 501 message pointing to %s", code, errOut, BuilderName)
	}
	// The builder's own buildkit image may be in the shared cache already;
	// docker build must not add one.
	if imgsOut, _, _ := h.docker(t, "", "images"); strings.Contains(imgsOut, "buildkit") && !strings.Contains(imgsBefore, "buildkit") {
		t.Errorf("docker build should not pull buildkit image:\n%s", imgsOut)
	}
	if psaOut, _, _ := h.docker(t, "", "ps", "-a"); strings.Contains(psaOut, "buildx") {
		t.Errorf("docker build should not create a buildx container:\n%s", psaOut)
	}
}

func TestHarnessEndToEnd(t *testing.T) {
	h := startHarness(t)

	t.Run("version", func(t *testing.T) {
		out, errOut, code := h.docker(t, "", "version")
		if code != 0 || !strings.Contains(out, "gvx-desktop-") {
			t.Fatalf("version (%d): out=%q err=%q", code, out, errOut)
		}
	})

	t.Run("info", func(t *testing.T) {
		out, errOut, code := h.docker(t, "", "info")
		if code != 0 || !strings.Contains(out, "gvx-desktop-") {
			t.Fatalf("info (%d): out=%q err=%q", code, out, errOut)
		}
	})

	t.Run("pull_and_images", func(t *testing.T) {
		if _, errOut, code := h.docker(t, "", "pull", "alpine:3.20"); code != 0 {
			t.Fatalf("pull (%d): %s", code, errOut)
		}
		out, errOut, code := h.docker(t, "", "images")
		if code != 0 || !strings.Contains(out, "alpine") {
			t.Fatalf("images (%d): out=%q err=%q", code, out, errOut)
		}
	})

	t.Run("run_rm_echo", func(t *testing.T) {
		out, errOut, code := h.docker(t, "", "run", "--rm", "--name", "gvxd-echo", "alpine:3.20", "echo", "ok")
		if code != 0 || strings.TrimSpace(out) != "ok" {
			t.Fatalf("run --rm echo ok (%d): out=%q err=%q", code, out, errOut)
		}
		psOut, _, _ := h.docker(t, "", "ps", "-a")
		if strings.Contains(psOut, "gvxd-echo") {
			t.Errorf("AutoRemove container gvxd-echo still in ps -a:\n%s", psOut)
		}
	})

	t.Run("run_interactive_stdin", func(t *testing.T) {
		out, errOut, code := h.docker(t, "hi\n", "run", "-i", "--rm", "--name", "gvxd-cat", "alpine:3.20", "cat")
		if code != 0 || strings.TrimSpace(out) != "hi" {
			t.Fatalf("run -i cat (%d): out=%q err=%q", code, out, errOut)
		}
	})

	t.Run("run_detached_nginx_curl_ps_inspect_logs_lifecycle", func(t *testing.T) {
		t.Cleanup(func() { _, _, _ = h.docker(t, "", "rm", "-f", "gvxd-web") })
		if _, errOut, code := h.docker(t, "", "pull", "nginx:alpine"); code != 0 {
			t.Fatalf("pull nginx:alpine (%d): %s", code, errOut)
		}
		out, errOut, code := h.docker(t, "", "run", "-d", "--name", "gvxd-web", "-p", "18095:80", "nginx:alpine")
		if code != 0 || strings.TrimSpace(out) == "" {
			t.Fatalf("run -d nginx (%d): out=%q err=%q", code, out, errOut)
		}

		// Wait for nginx to respond on 127.0.0.1:18095.
		httpClient := &http.Client{Timeout: 2 * time.Second}
		var body string
		deadline := time.Now().Add(20 * time.Second)
		for time.Now().Before(deadline) {
			resp, err := httpClient.Get("http://127.0.0.1:18095/")
			if err == nil {
				b, _ := io.ReadAll(resp.Body)
				resp.Body.Close()
				if resp.StatusCode == http.StatusOK && strings.Contains(string(b), "Welcome to nginx") {
					body = string(b)
					break
				}
			}
			time.Sleep(300 * time.Millisecond)
		}
		if body == "" {
			logsOut, logsErr, _ := h.docker(t, "", "logs", "gvxd-web")
			t.Fatalf("nginx did not respond on 127.0.0.1:18095; logs=%q err=%q", logsOut, logsErr)
		}

		// ps, ps -a, inspect
		if psOut, _, code := h.docker(t, "", "ps"); code != 0 || !strings.Contains(psOut, "gvxd-web") {
			t.Fatalf("ps (%d): %s", code, psOut)
		}
		if insOut, _, code := h.docker(t, "", "inspect", "gvxd-web"); code != 0 || !strings.Contains(insOut, "18095") {
			t.Fatalf("inspect (%d): %s", code, insOut)
		}

		// logs and logs -f
		if logsOut, logsErr, code := h.docker(t, "", "logs", "gvxd-web"); code != 0 || (!strings.Contains(logsOut+logsErr, "GET /") && !strings.Contains(logsOut+logsErr, "nginx")) {
			t.Fatalf("logs (%d): out=%q err=%q", code, logsOut, logsErr)
		}
		fCtx, fCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer fCancel()
		fCmd := exec.CommandContext(fCtx, "docker", "logs", "-f", "gvxd-web")
		fCmd.Env = h.env()
		var fBuf bytes.Buffer
		fCmd.Stdout, fCmd.Stderr = &fBuf, &fBuf
		_ = fCmd.Start()
		time.Sleep(400 * time.Millisecond)
		_, _ = httpClient.Get("http://127.0.0.1:18095/follow-check")
		time.Sleep(600 * time.Millisecond)
		fCancel()
		_ = fCmd.Wait()
		if !strings.Contains(fBuf.String(), "follow-check") {
			t.Errorf("logs -f did not see follow-check request: %q", fBuf.String())
		}

		// stop / start / rm -f
		if _, errOut, code := h.docker(t, "", "stop", "gvxd-web"); code != 0 {
			t.Fatalf("stop (%d): %s", code, errOut)
		}
		if psOut, _, _ := h.docker(t, "", "ps"); strings.Contains(psOut, "gvxd-web") {
			t.Errorf("stopped container still in docker ps: %s", psOut)
		}
		if psaOut, _, _ := h.docker(t, "", "ps", "-a"); !strings.Contains(psaOut, "gvxd-web") {
			t.Errorf("stopped container missing from docker ps -a: %s", psaOut)
		}
		if _, errOut, code := h.docker(t, "", "start", "gvxd-web"); code != 0 {
			t.Fatalf("start (%d): %s", code, errOut)
		}
		if _, errOut, code := h.docker(t, "", "rm", "-f", "gvxd-web"); code != 0 {
			t.Fatalf("rm -f (%d): %s", code, errOut)
		}
	})

	t.Run("volume_across_two_runs", func(t *testing.T) {
		t.Cleanup(func() { _, _, _ = h.docker(t, "", "volume", "rm", "gvxd-persist-vol") })
		if _, errOut, code := h.docker(t, "", "volume", "create", "gvxd-persist-vol"); code != 0 {
			t.Fatalf("volume create (%d): %s", code, errOut)
		}
		if _, errOut, code := h.docker(t, "", "run", "--rm", "--name", "gvxd-vw", "-v", "gvxd-persist-vol:/data", "alpine:3.20", "sh", "-c", "echo hello-vol > /data/msg.txt"); code != 0 {
			t.Fatalf("volume write (%d): %s", code, errOut)
		}
		out, errOut, code := h.docker(t, "", "run", "--rm", "--name", "gvxd-vr", "-v", "gvxd-persist-vol:/data", "alpine:3.20", "cat", "/data/msg.txt")
		if code != 0 || strings.TrimSpace(out) != "hello-vol" {
			t.Fatalf("volume read (%d): out=%q err=%q", code, out, errOut)
		}
	})

	t.Run("bind_mount", func(t *testing.T) {
		hostDir := t.TempDir()
		if err := os.WriteFile(filepath.Join(hostDir, "in.txt"), []byte("from-host\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		out, errOut, code := h.docker(t, "", "run", "--rm", "--name", "gvxd-bind", "-v", hostDir+":/host", "alpine:3.20", "sh", "-c", "cat /host/in.txt && echo from-guest > /host/out.txt")
		if code != 0 || strings.TrimSpace(out) != "from-host" {
			t.Fatalf("bind mount (%d): out=%q err=%q", code, out, errOut)
		}
		b, err := os.ReadFile(filepath.Join(hostDir, "out.txt"))
		if err != nil || strings.TrimSpace(string(b)) != "from-guest" {
			t.Fatalf("bind write back failed: %q err=%v", string(b), err)
		}
	})

	t.Run("run_it_pty", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, "docker", "run", "-it", "--rm", "--name", "gvxd-pty", "alpine:3.20", "sh", "-c", "test -t 0 && echo tty-ok && stty size && exit 3")
		cmd.Env = h.env()
		ptmx, err := pty.StartWithSize(cmd, &pty.Winsize{Rows: 24, Cols: 80})
		if err != nil {
			t.Fatalf("pty.StartWithSize: %v", err)
		}
		defer ptmx.Close()

		var out bytes.Buffer
		doneRead := make(chan struct{})
		go func() {
			defer close(doneRead)
			buf := make([]byte, 4096)
			for {
				n, err := ptmx.Read(buf)
				if n > 0 {
					out.Write(buf[:n])
				}
				if err != nil {
					if errors.Is(err, syscall.EIO) || errors.Is(err, io.EOF) {
						return
					}
					return
				}
			}
		}()

		err = cmd.Wait()
		<-doneRead
		exitCode := 0
		if err != nil {
			var ee *exec.ExitError
			if errors.As(err, &ee) {
				exitCode = ee.ExitCode()
			} else {
				t.Fatalf("pty wait error: %v", err)
			}
		}
		t.Logf("docker run -it output (%d): %q", exitCode, out.String())
		if exitCode != 3 {
			t.Errorf("exit code = %d, want 3", exitCode)
		}
		if !strings.Contains(out.String(), "tty-ok") || !strings.Contains(out.String(), "24 80") {
			t.Errorf("expected tty-ok and 24 80 in output, got %q", out.String())
		}
	})

	t.Run("exec_and_build_501", func(t *testing.T) {
		t.Cleanup(func() { _, _, _ = h.docker(t, "", "rm", "-f", "gvxd-exec-e2e") })
		if _, errOut, code := h.docker(t, "", "run", "-d", "--name", "gvxd-exec-e2e", "alpine:3.20", "sleep", "30"); code != 0 {
			t.Fatalf("run gvxd-exec-e2e (%d): %s", code, errOut)
		}
		out, errOut, code := h.docker(t, "", "exec", "gvxd-exec-e2e", "echo", "e2e-ok")
		if h.e.Features().Control {
			if code != 0 || strings.TrimSpace(out) != "e2e-ok" {
				t.Errorf("exec (%d): out=%q err=%q, want 0 e2e-ok", code, out, errOut)
			}
		} else {
			if code == 0 || !strings.Contains(errOut, "not supported by GVX Desktop's experimental sandbox engine yet") {
				t.Errorf("exec (%d): %q", code, errOut)
			}
		}
		_, _, _ = h.docker(t, "", "rm", "-f", "gvxd-exec-e2e")

		buildDir := t.TempDir()
		_ = os.WriteFile(filepath.Join(buildDir, "Dockerfile"), []byte("FROM alpine:3.20\n"), 0o600)
		imgsBefore, _, _ := h.docker(t, "", "images")
		_, errOut, code = h.docker(t, "", "build", buildDir)
		if code == 0 || !strings.Contains(errOut, "docker buildx use --default "+BuilderName) {
			t.Errorf("build (%d): %q", code, errOut)
		}
		// The builder's own buildkit image may be in the shared cache already;
		// docker build must not add one.
		if imgsOut, _, _ := h.docker(t, "", "images"); strings.Contains(imgsOut, "buildkit") && !strings.Contains(imgsBefore, "buildkit") {
			t.Errorf("docker build should not pull buildkit image:\n%s", imgsOut)
		}
		if psaOut, _, _ := h.docker(t, "", "ps", "-a"); strings.Contains(psaOut, "buildx") {
			t.Errorf("docker build should not create a buildx container:\n%s", psaOut)
		}
	})

	t.Run("compose_single_service", func(t *testing.T) {
		compDir := t.TempDir()
		composeYAML := `services:
  web:
    image: nginx:alpine
    ports:
      - "18096:80"
    volumes:
      - webdata:/var/cache/nginx
volumes:
  webdata:
`
		composeFile := filepath.Join(compDir, "compose.yaml")
		if err := os.WriteFile(composeFile, []byte(composeYAML), 0o600); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			_, _, _ = h.docker(t, "", "compose", "-p", "gvxd-compose", "-f", composeFile, "down", "-v")
		})

		if _, errOut, code := h.docker(t, "", "compose", "-p", "gvxd-compose", "-f", composeFile, "up", "-d"); code != 0 {
			t.Fatalf("compose up -d (%d): %s", code, errOut)
		}
		if out, errOut, code := h.docker(t, "", "compose", "-p", "gvxd-compose", "-f", composeFile, "ps"); code != 0 || !strings.Contains(out, "gvxd-compose") {
			t.Fatalf("compose ps (%d): out=%q err=%q", code, out, errOut)
		}
		if out, errOut, code := h.docker(t, "", "compose", "-p", "gvxd-compose", "-f", composeFile, "logs"); code != 0 {
			t.Fatalf("compose logs (%d): out=%q err=%q", code, out, errOut)
		} else {
			t.Logf("compose logs: %s", out)
		}
		if _, errOut, code := h.docker(t, "", "compose", "-p", "gvxd-compose", "-f", composeFile, "down", "-v"); code != 0 {
			t.Fatalf("compose down -v (%d): %s", code, errOut)
		}
	})

	t.Run("harness_restart_persistence", func(t *testing.T) {
		t.Cleanup(func() { _, _, _ = h.docker(t, "", "rm", "-f", "gvxd-keep") })
		if _, errOut, code := h.docker(t, "", "run", "--name", "gvxd-keep", "alpine:3.20", "echo", "persisted"); code != 0 {
			t.Fatalf("run gvxd-keep (%d): %s", code, errOut)
		}
		h.restart(t)
		psaOut, errOut, code := h.docker(t, "", "ps", "-a")
		if code != 0 || !strings.Contains(psaOut, "gvxd-keep") {
			t.Fatalf("ps -a after restart (%d): out=%q err=%q", code, psaOut, errOut)
		}
	})

	t.Run("regression_busy_port_rm_prompt_error", func(t *testing.T) {
		ln, err := net.Listen("tcp", "127.0.0.1:18097")
		if err != nil {
			t.Fatalf("listen 127.0.0.1:18097: %v", err)
		}
		defer ln.Close()

		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		start := time.Now()
		cmd := exec.CommandContext(ctx, "docker", "run", "--rm", "--name", "gvxd-busyport", "-p", "18097:80", "nginx:alpine")
		cmd.Env = h.env()
		out, err := cmd.CombinedOutput()
		elapsed := time.Since(start)
		t.Logf("$ docker run --rm --name gvxd-busyport -p 18097:80 nginx:alpine (%v, err=%v)\n%s", elapsed, err, string(out))
		if err == nil {
			t.Fatalf("expected error on busy port, got success: %s", string(out))
		}
		if ctx.Err() != nil {
			t.Fatalf("docker run --rm hung on busy port (%v)", elapsed)
		}
		psaOut, _, code := h.docker(t, "", "ps", "-a")
		if code != 0 {
			t.Fatalf("ps -a failed (%d)", code)
		}
		if strings.Contains(psaOut, "gvxd-busyport") {
			t.Errorf("failed --rm container still present in ps -a:\n%s", psaOut)
		}
	})

	t.Run("regression_resolved_ref_and_short_id", func(t *testing.T) {
		out, errOut, code := h.docker(t, "", "run", "--rm", "--name", "gvxd-fqref", "docker.io/library/alpine:3.20", "echo", "ok")
		if code != 0 || strings.TrimSpace(out) != "ok" {
			t.Fatalf("run docker.io/library/alpine:3.20 (%d): out=%q err=%q", code, out, errOut)
		}
		idOut, errOut, code := h.docker(t, "", "images", "-q", "alpine:3.20")
		if code != 0 || strings.TrimSpace(idOut) == "" {
			t.Fatalf("images -q alpine:3.20 (%d): out=%q err=%q", code, idOut, errOut)
		}
		shortID := strings.TrimSpace(strings.Split(strings.TrimSpace(idOut), "\n")[0])
		if len(shortID) > 12 {
			shortID = shortID[:12]
		}
		out, errOut, code = h.docker(t, "", "run", "--rm", "--name", "gvxd-shortid", shortID, "echo", "ok")
		if code != 0 || strings.TrimSpace(out) != "ok" {
			t.Fatalf("run by 12-char id %s (%d): out=%q err=%q", shortID, code, out, errOut)
		}
	})

	t.Run("regression_rmi_keep_images_refused", func(t *testing.T) {
		prev := h.e.cfg.KeepImages
		h.e.cfg.KeepImages = func() []string { return []string{"alpine:3.20"} }
		t.Cleanup(func() { h.e.cfg.KeepImages = prev })

		for _, args := range [][]string{{"rmi", "alpine:3.20"}, {"rmi", "-f", "alpine:3.20"}} {
			_, errOut, code := h.docker(t, "", args...)
			if code == 0 || !strings.Contains(errOut, "GVX Desktop sandbox uses it") {
				t.Errorf("docker %s = (%d) %q, want refusal mentioning GVX Desktop sandbox", strings.Join(args, " "), code, errOut)
			}
		}
		imgsOut, _, code := h.docker(t, "", "images")
		if code != 0 || !strings.Contains(imgsOut, "alpine") {
			t.Fatalf("kept image alpine:3.20 missing from docker images:\n%s", imgsOut)
		}
	})

	t.Run("regression_events_ends_on_engine_close", func(t *testing.T) {
		evCtx, evCancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer evCancel()
		evCmd := exec.CommandContext(evCtx, "docker", "events", "--format", "{{.Type}} {{.Action}} {{.Actor.ID}}")
		evCmd.Env = h.env()
		evOut, err := evCmd.StdoutPipe()
		if err != nil {
			t.Fatal(err)
		}
		if err := evCmd.Start(); err != nil {
			t.Fatal(err)
		}

		// Confirm stream is active by receiving one emitted event.
		firstLine := make(chan string, 1)
		go func() {
			sc := bufio.NewScanner(evOut)
			if sc.Scan() {
				firstLine <- sc.Text()
			}
			for sc.Scan() {
			}
		}()
		time.Sleep(300 * time.Millisecond)
		h.e.emit(Event{Type: "container", Action: "start", ID: "gvxd-evclose", Attributes: map[string]string{"name": "gvxd-evclose"}})
		select {
		case got := <-firstLine:
			if !strings.Contains(got, "gvxd-evclose") {
				t.Fatalf("unexpected first event: %q", got)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("timed out waiting for live event before close")
		}

		// Close the engine itself (keeping HTTP server open) and verify `docker events` exits promptly.
		if err := h.e.Close(5 * time.Second); err != nil {
			t.Fatalf("engine Close: %v", err)
		}
		waitDone := make(chan error, 1)
		go func() { waitDone <- evCmd.Wait() }()
		select {
		case err := <-waitDone:
			t.Logf("docker events exited after engine close (err=%v)", err)
		case <-time.After(5 * time.Second):
			t.Fatal("docker events hung after engine Close")
		}
	})
}
