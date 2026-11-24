//go:build !windows

package engine

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestHarnessHealthcheckAndComposeServiceHealthy(t *testing.T) {
	h := startHarness(t)
	if !h.e.runner.Features().Control {
		t.Skip("healthcheck requires rungv v0.20+")
	}

	mustDocker := func(t *testing.T, args ...string) string {
		t.Helper()
		out, errOut, code := h.docker(t, "", args...)
		if code != 0 {
			t.Fatalf("docker %s (%d):\n%s%s", strings.Join(args, " "), code, out, errOut)
		}
		return out
	}

	mustDocker(t, "pull", "alpine:3.20")

	t.Run("docker_run_health_transitions_and_ps_filter", func(t *testing.T) {
		t.Cleanup(func() {
			_, _, _ = h.docker(t, "", "rm", "-f", "gvxd-hc-run")
		})
		// Starts unhealthy until /tmp/ready is created, then turns healthy.
		mustDocker(t, "run", "-d",
			"--name", "gvxd-hc-run",
			"--health-cmd", "test -f /tmp/ready && echo ready-ok",
			"--health-interval", "300ms",
			"--health-timeout", "2s",
			"--health-retries", "2",
			"alpine:3.20",
			"sh", "-c", "sleep 60",
		)

		// Should become unhealthy after 2 failed probes.
		waitHealth := func(want string) {
			t.Helper()
			deadline := time.Now().Add(15 * time.Second)
			for time.Now().Before(deadline) {
				got := strings.TrimSpace(mustDocker(t, "inspect", "-f", "{{.State.Health.Status}}", "gvxd-hc-run"))
				if got == want {
					return
				}
				time.Sleep(150 * time.Millisecond)
			}
			raw := mustDocker(t, "inspect", "-f", "{{json .State.Health}}", "gvxd-hc-run")
			t.Fatalf("timed out waiting for health=%q; got %s", want, raw)
		}
		waitHealth("unhealthy")

		// Touch /tmp/ready -> should transition to healthy and show "(healthy)" in docker ps.
		mustDocker(t, "exec", "gvxd-hc-run", "touch", "/tmp/ready")
		waitHealth("healthy")

		psOut := mustDocker(t, "ps", "--filter", "health=healthy", "--format", "{{.Names}} {{.Status}}")
		if !strings.Contains(psOut, "gvxd-hc-run") || !strings.Contains(psOut, "(healthy)") {
			t.Fatalf("docker ps --filter health=healthy unexpected: %q", psOut)
		}
		outLog := mustDocker(t, "inspect", "-f", "{{(index .State.Health.Log 0).Output}}", "gvxd-hc-run")
		t.Logf("latest/first health log output: %q", outLog)
	})

	t.Run("compose_depends_on_service_healthy", func(t *testing.T) {
		dir := t.TempDir()
		composeYAML := `services:
  backend:
    image: alpine:3.20
    command: ["sh", "-c", "sleep 1 && touch /tmp/ok && sleep 60"]
    healthcheck:
      test: ["CMD", "test", "-f", "/tmp/ok"]
      interval: 300ms
      timeout: 2s
      retries: 10
  frontend:
    image: alpine:3.20
    command: ["sh", "-c", "echo frontend-started-after-healthy && sleep 60"]
    depends_on:
      backend:
        condition: service_healthy
`
		if err := os.WriteFile(filepath.Join(dir, "compose.yaml"), []byte(composeYAML), 0o644); err != nil {
			t.Fatal(err)
		}
		compose := func(timeout time.Duration, args ...string) (string, error) {
			ctx, cancel := context.WithTimeout(context.Background(), timeout)
			defer cancel()
			cmd := exec.CommandContext(ctx, "docker", append([]string{"compose", "-p", "gvxdhc"}, args...)...)
			cmd.Dir = dir
			cmd.Env = h.env()
			out, err := cmd.CombinedOutput()
			return string(out), err
		}
		t.Cleanup(func() {
			_, _ = compose(15*time.Second, "down", "-v", "--remove-orphans")
		})

		upOut, err := compose(30*time.Second, "up", "-d")
		if err != nil {
			t.Fatalf("docker compose up -d with service_healthy failed: %v\n%s", err, upOut)
		}
		t.Logf("compose up -d output:\n%s", upOut)

		psOut, err := compose(10*time.Second, "ps")
		if err != nil {
			t.Fatalf("compose ps failed: %v\n%s", err, psOut)
		}
		if !strings.Contains(psOut, "healthy") || !strings.Contains(psOut, "frontend") {
			t.Fatalf("compose ps missing healthy backend or started frontend:\n%s", psOut)
		}
		logsOut, err := compose(10*time.Second, "logs", "frontend")
		if err != nil || !strings.Contains(logsOut, "frontend-started-after-healthy") {
			t.Fatalf("frontend did not start cleanly: err=%v logs=%q", err, logsOut)
		}
	})

	t.Run("image_embedded_healthcheck_inherited_and_disabled", func(t *testing.T) {
		t.Cleanup(func() {
			_, _, _ = h.docker(t, "", "rm", "-f", "gvxd-hc-src", "gvxd-hc-inh", "gvxd-hc-nohc")
			_, _, _ = h.docker(t, "", "rmi", "-f", "gvxd-hc-img:1")
		})
		// Commit an image with HEALTHCHECK baked into its OCI config.
		mustDocker(t, "run", "--name", "gvxd-hc-src", "alpine:3.20", "true")
		mustDocker(t, "commit",
			"--change", "HEALTHCHECK --interval=300ms --timeout=2s --retries=2 CMD echo image-hc-ok",
			"gvxd-hc-src", "gvxd-hc-img:1")

		// Verify docker image inspect exposes Config.Healthcheck from OCI config!
		imgHC := strings.TrimSpace(mustDocker(t, "image", "inspect", "-f", "{{json .Config.Healthcheck}}", "gvxd-hc-img:1"))
		if !strings.Contains(imgHC, "image-hc-ok") {
			t.Fatalf("image inspect missing embedded Healthcheck: %s", imgHC)
		}

		// Container created from image automatically inherits & executes HEALTHCHECK.
		mustDocker(t, "run", "-d", "--name", "gvxd-hc-inh", "gvxd-hc-img:1", "sleep", "60")
		deadline := time.Now().Add(15 * time.Second)
		healthy := false
		for time.Now().Before(deadline) {
			if strings.TrimSpace(mustDocker(t, "inspect", "-f", "{{.State.Health.Status}}", "gvxd-hc-inh")) == "healthy" {
				healthy = true
				break
			}
			time.Sleep(150 * time.Millisecond)
		}
		if !healthy {
			t.Fatalf("inherited image healthcheck never reached healthy: %s", mustDocker(t, "inspect", "-f", "{{json .State.Health}}", "gvxd-hc-inh"))
		}

		// --no-healthcheck disables the inherited image healthcheck.
		mustDocker(t, "run", "-d", "--name", "gvxd-hc-nohc", "--no-healthcheck", "gvxd-hc-img:1", "sleep", "60")
		noHC := strings.TrimSpace(mustDocker(t, "inspect", "-f", "{{json .State.Health}}", "gvxd-hc-nohc"))
		if noHC != "null" {
			t.Fatalf("--no-healthcheck expected State.Health=null, got %s", noHC)
		}
	})
}
