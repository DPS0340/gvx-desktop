//go:build !windows

package engine

// Owner: frozen (architect). The harness serves a real engine, over the
// real runner, on a Unix socket for the real docker CLI. Tests that use it
// are skipped unless GVX_ENGINE_HARNESS=1, and with -short. Everything it
// makes is under /tmp/gvxd-test-se: never the app's data, the user's rungv
// cache, ~/.gvx/docker.sock or ~/.docker.

import (
	"bytes"
	"context"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ironpark/gvx-desktop/internal/rungv"
)

// harnessRoot holds the harness's sockets, data and image cache. The cache
// stays between runs so images are pulled once; the workstream that runs
// the final verification deletes the directory.
const harnessRoot = "/tmp/gvxd-test-se"

type harness struct {
	e           *Engine
	host        string // DOCKER_HOST
	sock        string
	data        string
	cache       string
	config      string // DOCKER_CONFIG
	buildx      string // BUILDX_CONFIG
	builderIdle time.Duration
	srv         *http.Server
	ln          net.Listener
	client      *rungv.Client
}

// startHarness serves an engine for the docker CLI, or skips the test.
func startHarness(t *testing.T) *harness {
	return startHarnessWithIdle(t, 0)
}

func startHarnessWithIdle(t *testing.T, idle time.Duration) *harness {
	t.Helper()
	if testing.Short() || os.Getenv("GVX_ENGINE_HARNESS") != "1" {
		t.Skip("set GVX_ENGINE_HARNESS=1 and drop -short to drive the real docker CLI")
	}
	path, err := rungv.Locate("")
	if err != nil {
		t.Skip("rungv not found")
	}
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("docker CLI not found")
	}
	cache := filepath.Join(harnessRoot, "cache")
	if home, _ := os.UserHomeDir(); home != "" && strings.HasPrefix(cache, home) {
		t.Fatal("the harness cache must not be under the home folder")
	}
	run, err := os.MkdirTemp(harnessRoot+"-runs", "r")
	if err != nil {
		if err = os.MkdirAll(harnessRoot+"-runs", 0o700); err == nil {
			run, err = os.MkdirTemp(harnessRoot+"-runs", "r")
		}
	}
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range []string{cache, filepath.Join(run, "data"), filepath.Join(run, "docker", "buildx")} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	h := &harness{
		sock:        filepath.Join(run, "d.sock"),
		data:        filepath.Join(run, "data"),
		cache:       cache,
		config:      filepath.Join(run, "docker"),
		buildx:      filepath.Join(run, "docker", "buildx"),
		builderIdle: idle,
		client:      &rungv.Client{Path: path, CacheDir: cache},
	}
	h.host = "unix://" + h.sock
	// The CLI finds its compose and buildx plugins through DOCKER_CONFIG:
	// link the user's, which is only read.
	if home, _ := os.UserHomeDir(); home != "" {
		for _, p := range []string{"docker-compose", "docker-buildx"} {
			if target, err := filepath.EvalSymlinks(filepath.Join(home, ".docker", "cli-plugins", p)); err == nil {
				_ = os.MkdirAll(filepath.Join(h.config, "cli-plugins"), 0o700)
				_ = os.Symlink(target, filepath.Join(h.config, "cli-plugins", p))
			}
		}
	}
	h.open(t)
	t.Cleanup(func() {
		h.close()
		_ = os.RemoveAll(run)
	})
	return h
}

func (h *harness) open(t *testing.T) {
	t.Helper()
	h.e = New(Config{
		Dir:                h.data,
		Runner:             NewRunner(func() *rungv.Client { return h.client }, h.data),
		Version:            "0.0.0-harness",
		BuilderSocketDir:   h.data,
		BuilderIdleTimeout: h.builderIdle,
		Logf:               func(f string, a ...any) { t.Logf(f, a...) },
	})
	if err := h.e.Open(context.Background()); err != nil {
		t.Fatal(err)
	}
	_ = os.Remove(h.sock)
	ln, err := net.Listen("unix", h.sock)
	if err != nil {
		t.Fatal(err)
	}
	h.ln = ln
	h.srv = &http.Server{Handler: h.e.Handler()}
	go func() { _ = h.srv.Serve(ln) }()
}

func (h *harness) close() {
	if h.srv != nil {
		_ = h.srv.Close()
	}
	if h.e != nil {
		_ = h.e.Close(10 * time.Second)
	}
	_ = os.Remove(h.sock)
}

// restart closes the engine and opens a new one over the same data, as the
// app's next launch does.
func (h *harness) restart(t *testing.T) {
	t.Helper()
	h.close()
	h.open(t)
}

// env is the environment the docker CLI runs with.
func (h *harness) env() []string {
	return h.envOpts(true, nil)
}

func (h *harness) envOpts(withHost bool, extra []string) []string {
	var env []string
	for _, kv := range os.Environ() {
		if k, _, _ := strings.Cut(kv, "="); k == "DOCKER_HOST" || k == "DOCKER_CONTEXT" || k == "DOCKER_CONFIG" || k == "BUILDX_CONFIG" || k == "BUILDX_BUILDER" || k == "DOCKER_BUILDKIT" {
			continue
		}
		env = append(env, kv)
	}
	if withHost {
		env = append(env, "DOCKER_HOST="+h.host)
	}
	env = append(env, "DOCKER_CONFIG="+h.config, "BUILDX_CONFIG="+h.buildx, "DOCKER_CLI_HINTS=false")
	return append(env, extra...)
}

// docker runs the docker CLI with stdin and returns its output and exit
// code.
func (h *harness) docker(t *testing.T, stdin string, args ...string) (stdout, stderr string, code int) {
	t.Helper()
	return h.dockerEnv(t, h.env(), "", stdin, args...)
}

func (h *harness) dockerEnv(t *testing.T, env []string, workdir, stdin string, args ...string) (stdout, stderr string, code int) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, "docker", args...)
	cmd.Env = env
	if workdir != "" {
		cmd.Dir = workdir
	}
	cmd.Stdin = strings.NewReader(stdin)
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	err := cmd.Run()
	code = 0
	if err != nil {
		code = -1
		if ee, ok := err.(*exec.ExitError); ok {
			code = ee.ExitCode()
		}
	}
	t.Logf("$ docker %s → %d\n%s%s", strings.Join(args, " "), code, out.String(), errb.String())
	return out.String(), errb.String(), code
}
