//go:build !windows

package engine

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestHarnessBuild(t *testing.T) {
	h := startHarness(t)

	// Make sure alpine:3.20 is available (optional: buildkit pulls it too,
	// and our test runs the built image with rungv).
	ctxDir := t.TempDir()
	dockerfile := `FROM alpine:3.20
RUN apk add --no-cache curl
COPY greeting.txt /greeting.txt
CMD ["sh", "-c", "cat /greeting.txt && curl --version | head -n 1"]
`
	if err := os.WriteFile(filepath.Join(ctxDir, "Dockerfile"), []byte(dockerfile), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ctxDir, "greeting.txt"), []byte("hello-from-gvx-build\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	frontEndpoint := "unix://" + h.e.BuilderSocket()

	// Create the gvx-desktop remote builder in the isolated temp BUILDX_CONFIG.
	if _, errOut, code := h.docker(t, "", "buildx", "create",
		"--name", BuilderName,
		"--driver", "remote",
		"--driver-opt", "default-load=true",
		frontEndpoint,
	); code != 0 {
		t.Fatalf("buildx create (%d): %s", code, errOut)
	}

	t.Cleanup(func() {
		_, _, _ = h.docker(t, "", "rmi", "-f", "gvxd-built:1", "gvxd-built:2", "gvxd-legacy:1", "gvxd-legacy:alt", "gvxd-fail:1")
	})

	// (a) docker buildx build --builder gvx-desktop -t gvxd-built:1 <ctx>
	t.Run("a_buildx_explicit_builder", func(t *testing.T) {
		if _, errOut, code := h.docker(t, "", "buildx", "build", "--builder", BuilderName, "-t", "gvxd-built:1", ctxDir); code != 0 {
			t.Fatalf("buildx build gvxd-built:1 (%d): %s", code, errOut)
		}
		imgsOut, errOut, code := h.docker(t, "", "images")
		if code != 0 || !strings.Contains(imgsOut, "gvxd-built") {
			t.Fatalf("docker images missing gvxd-built (%d): out=%q err=%q", code, imgsOut, errOut)
		}
		out, errOut, code := h.docker(t, "", "run", "--rm", "--name", "gvxd-built-run1", "gvxd-built:1")
		if code != 0 || !strings.Contains(out, "hello-from-gvx-build") || !strings.Contains(out, "curl") {
			t.Fatalf("run gvxd-built:1 (%d): out=%q err=%q", code, out, errOut)
		}
		// Builder sandbox must not appear in docker ps -a.
		if psOut, _, _ := h.docker(t, "", "ps", "-a"); strings.Contains(psOut, "buildkit") {
			t.Errorf("builder sandbox leaked into docker ps -a:\n%s", psOut)
		}
	})

	// (b) docker build -t gvxd-built:2 <ctx> with gvx-desktop selected for the gvx context
	t.Run("b_context_builder_selection", func(t *testing.T) {
		ctxEnv := h.envOpts(false, nil)
		if _, errOut, code := h.dockerEnv(t, ctxEnv, "", "", "context", "create", "gvx", "--docker", "host="+h.host); code != 0 {
			t.Fatalf("context create gvx (%d): %s", code, errOut)
		}
		if _, errOut, code := h.dockerEnv(t, ctxEnv, "", "", "context", "create", "other", "--docker", "host=unix:///tmp/gvxd-nonexistent.sock"); code != 0 {
			t.Fatalf("context create other (%d): %s", code, errOut)
		}
		if _, errOut, code := h.dockerEnv(t, ctxEnv, "", "", "buildx", "install"); code != 0 {
			t.Fatalf("buildx install (%d): %s", code, errOut)
		}
		if _, errOut, code := h.dockerEnv(t, ctxEnv, "", "", "-H", h.host, "buildx", "use", "--default", BuilderName); code != 0 {
			t.Fatalf("-H buildx use --default %s (%d): %s", BuilderName, code, errOut)
		}
		if _, errOut, code := h.dockerEnv(t, ctxEnv, "", "", "--context", "gvx", "buildx", "use", "--default", BuilderName); code != 0 {
			t.Fatalf("--context gvx buildx use --default %s (%d): %s", BuilderName, code, errOut)
		}
		// Confirm another context does NOT use gvx-desktop.
		otherInspect, otherErr, _ := h.dockerEnv(t, ctxEnv, "", "", "--context", "other", "buildx", "inspect")
		if strings.Contains(otherInspect+otherErr, BuilderName) {
			t.Errorf("other context unexpectedly uses %s: %s %s", BuilderName, otherInspect, otherErr)
		}
		if _, errOut, code := h.dockerEnv(t, ctxEnv, "", "", "context", "use", "gvx"); code != 0 {
			t.Fatalf("context use gvx (%d): %s", code, errOut)
		}
		if _, errOut, code := h.dockerEnv(t, ctxEnv, "", "", "build", "-t", "gvxd-built:2", ctxDir); code != 0 {
			t.Fatalf("docker build gvxd-built:2 (%d): %s", code, errOut)
		}
		out, errOut, code := h.dockerEnv(t, ctxEnv, "", "", "run", "--rm", "--name", "gvxd-built-run2", "gvxd-built:2")
		if code != 0 || !strings.Contains(out, "hello-from-gvx-build") || !strings.Contains(out, "curl") {
			t.Fatalf("run gvxd-built:2 (%d): out=%q err=%q", code, out, errOut)
		}
	})

	// (c) DOCKER_BUILDKIT=0 docker build -t gvxd-legacy:1 <ctx> (legacy POST /build)
	t.Run("c_legacy_post_build", func(t *testing.T) {
		legacyEnv := h.envOpts(true, []string{"DOCKER_BUILDKIT=0"})
		out, errOut, code := h.dockerEnv(t, legacyEnv, "", "", "build", "-t", "gvxd-legacy:1", "-t", "gvxd-legacy:alt", ctxDir)
		if code != 0 {
			t.Fatalf("legacy docker build (%d): out=%q err=%q", code, out, errOut)
		}
		runOut, runErr, runCode := h.docker(t, "", "run", "--rm", "--name", "gvxd-legacy-run", "gvxd-legacy:1")
		if runCode != 0 || !strings.Contains(runOut, "hello-from-gvx-build") || !strings.Contains(runOut, "curl") {
			t.Fatalf("run gvxd-legacy:1 (%d): out=%q err=%q", runCode, runOut, runErr)
		}
		if _, errOut, code := h.docker(t, "", "inspect", "gvxd-legacy:alt"); code != 0 {
			t.Fatalf("second tag gvxd-legacy:alt missing (%d): %s", code, errOut)
		}
	})

	// (d) docker compose build + up -d + down for a single service with build: .
	t.Run("d_compose_build_up_down", func(t *testing.T) {
		compDir := t.TempDir()
		compDockerfile := `FROM alpine:3.20
RUN echo compose-built-ok > /marker
CMD ["sh", "-c", "cat /marker && sleep 30"]
`
		composeYAML := `services:
  app:
    build: .
    image: gvxd-compose-app:1
`
		if err := os.WriteFile(filepath.Join(compDir, "Dockerfile"), []byte(compDockerfile), 0o600); err != nil {
			t.Fatal(err)
		}
		composeFile := filepath.Join(compDir, "compose.yaml")
		if err := os.WriteFile(composeFile, []byte(composeYAML), 0o600); err != nil {
			t.Fatal(err)
		}

		ctxEnv := h.envOpts(false, nil)
		t.Cleanup(func() {
			_, _, _ = h.dockerEnv(t, ctxEnv, compDir, "", "compose", "-p", "gvxd-cbuild", "-f", composeFile, "down", "-v")
			_, _, _ = h.docker(t, "", "rmi", "-f", "gvxd-compose-app:1")
		})

		if _, errOut, code := h.dockerEnv(t, ctxEnv, compDir, "", "compose", "-p", "gvxd-cbuild", "-f", composeFile, "build"); code != 0 {
			t.Fatalf("compose build (%d): %s", code, errOut)
		}
		if _, errOut, code := h.dockerEnv(t, ctxEnv, compDir, "", "compose", "-p", "gvxd-cbuild", "-f", composeFile, "up", "-d"); code != 0 {
			t.Fatalf("compose up -d (%d): %s", code, errOut)
		}
		time.Sleep(500 * time.Millisecond)
		logsOut, errOut, code := h.dockerEnv(t, ctxEnv, compDir, "", "compose", "-p", "gvxd-cbuild", "-f", composeFile, "logs")
		if code != 0 || !strings.Contains(logsOut, "compose-built-ok") {
			t.Fatalf("compose logs (%d): out=%q err=%q", code, logsOut, errOut)
		}
		if _, errOut, code := h.dockerEnv(t, ctxEnv, compDir, "", "compose", "-p", "gvxd-cbuild", "-f", composeFile, "down"); code != 0 {
			t.Fatalf("compose down (%d): %s", code, errOut)
		}
	})

	// (e) failing RUN step reports error & leaves no image; second build hits cache (CACHED)
	t.Run("e_fail_leaves_no_image_and_cache_hit", func(t *testing.T) {
		failDir := t.TempDir()
		if err := os.WriteFile(filepath.Join(failDir, "Dockerfile"), []byte("FROM alpine:3.20\nRUN echo boom >&2 && exit 42\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		out, errOut, code := h.docker(t, "", "buildx", "build", "--builder", BuilderName, "-t", "gvxd-fail:1", failDir)
		if code == 0 {
			t.Fatalf("expected failing RUN to fail, got code 0: %s %s", out, errOut)
		}
		if !strings.Contains(out+errOut, "exit code: 42") && !strings.Contains(out+errOut, "boom") {
			t.Errorf("failing RUN output did not report failure details:\n%s\n%s", out, errOut)
		}
		if imgsOut, _, _ := h.docker(t, "", "images"); strings.Contains(imgsOut, "gvxd-fail") {
			t.Errorf("failed build left image gvxd-fail in docker images:\n%s", imgsOut)
		}

		// Rebuild ctxDir with plain progress to confirm BuildKit cache hit.
		out2, errOut2, code2 := h.docker(t, "", "buildx", "build", "--builder", BuilderName, "--progress=plain", "-t", "gvxd-built:1", ctxDir)
		if code2 != 0 {
			t.Fatalf("cached rebuild (%d): %s", code2, errOut2)
		}
		if !strings.Contains(out2+errOut2, "CACHED") {
			t.Errorf("second build did not hit cache (no CACHED step):\n%s\n%s", out2, errOut2)
		}
	})
}

func TestHarnessBuildIdleTimeoutRestart(t *testing.T) {
	// (f) after the idle timeout the builder sandbox stops; the next build starts it again.
	h := startHarnessWithIdle(t, 3*time.Second)

	ctxDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(ctxDir, "Dockerfile"), []byte("FROM alpine:3.20\nCMD [\"echo\", \"idle-ok\"]\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if st := h.e.BuilderState(); st.Status != BuilderIdle {
		t.Fatalf("initial builder state before buildx create = %+v, want idle", st)
	}
	frontEndpoint := "unix://" + h.e.BuilderSocket()
	if _, errOut, code := h.docker(t, "", "buildx", "create",
		"--name", BuilderName,
		"--driver", "remote",
		"--driver-opt", "default-load=true",
		frontEndpoint,
	); code != 0 {
		t.Fatalf("buildx create (%d): %s", code, errOut)
	}
	t.Cleanup(func() {
		_, _, _ = h.docker(t, "", "rmi", "-f", "gvxd-idle:1", "gvxd-idle:2")
	})

	if _, errOut, code := h.docker(t, "", "buildx", "build", "--builder", BuilderName, "-t", "gvxd-idle:1", ctxDir); code != 0 {
		t.Fatalf("first build (%d): %s", code, errOut)
	}
	if st := h.e.BuilderState(); st.Status != BuilderRunning {
		t.Fatalf("builder state right after build = %+v, want running", st)
	}

	// Wait for idle timeout to stop the builder sandbox.
	deadline := time.Now().Add(20 * time.Second)
	for h.e.BuilderState().Status != BuilderIdle && time.Now().Before(deadline) {
		time.Sleep(200 * time.Millisecond)
	}
	if st := h.e.BuilderState(); st.Status != BuilderIdle {
		t.Fatalf("builder did not stop after idle timeout: %+v", st)
	}

	// Next build starts the builder again and succeeds.
	if _, errOut, code := h.docker(t, "", "buildx", "build", "--builder", BuilderName, "-t", "gvxd-idle:2", ctxDir); code != 0 {
		t.Fatalf("second build after idle stop (%d): %s", code, errOut)
	}
	out, errOut, code := h.docker(t, "", "run", "--rm", "--name", "gvxd-idle-run", "gvxd-idle:2")
	if code != 0 || strings.TrimSpace(out) != "idle-ok" {
		t.Fatalf("run gvxd-idle:2 (%d): out=%q err=%q", code, out, errOut)
	}
}
