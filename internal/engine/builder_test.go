//go:build !windows

package engine

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// startFakeBuildkit configures fake.Script so that starting builderSandboxID
// opens a real echo/half-close Unix listener on the spec's published backend
// socket and closes it when the sandbox exits.
func startFakeBuildkit(t *testing.T, fake *fakeRunner, startDelay time.Duration, starts *atomic.Int32) {
	t.Helper()
	fake.addImage(BuilderImage, ImageConfig{Entrypoint: []string{"/usr/bin/buildkitd-entrypoint"}})
	fake.Script = func(p *fakeProc) int {
		if p.Spec.ID != builderSandboxID {
			<-p.Done()
			return 0
		}
		if starts != nil {
			starts.Add(1)
		}
		if startDelay > 0 {
			time.Sleep(startDelay)
		}
		var sockPath string
		for _, port := range p.Spec.Ports {
			if rest, ok := strings.CutPrefix(port, "unix:"); ok {
				if i := strings.LastIndexByte(rest, ':'); i > 0 {
					sockPath = rest[:i]
				}
			}
		}
		if sockPath == "" {
			return 1
		}
		_ = os.Remove(sockPath)
		ln, err := net.Listen("unix", sockPath)
		if err != nil {
			return 1
		}
		defer func() {
			_ = ln.Close()
			_ = os.Remove(sockPath)
		}()

		go func() {
			for {
				c, err := ln.Accept()
				if err != nil {
					return
				}
				go func(conn net.Conn) {
					defer conn.Close()
					data, _ := io.ReadAll(conn)
					_, _ = conn.Write(append([]byte("echo:"), data...))
					if cw, ok := conn.(interface{ CloseWrite() error }); ok {
						_ = cw.CloseWrite()
					}
				}(c)
			}
		}()

		// Wait until Stop/Signal cancels the sandbox.
		code := <-p.end
		select {
		case p.end <- code:
		default:
		}
		return code
	}
}

func TestBuilderLazyStartConcurrentIdleAndClose(t *testing.T) {
	dir, err := os.MkdirTemp("", "gvxd-ut-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })

	fake := newFakeRunner()
	var starts atomic.Int32
	startFakeBuildkit(t, fake, 60*time.Millisecond, &starts)

	e := New(Config{
		Dir:                dir,
		Runner:             fake,
		Version:            "0.0.0-test",
		BuilderSocketDir:   dir,
		BuilderIdleTimeout: 150 * time.Millisecond,
	})
	if err := e.Open(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = e.Close(time.Second) })

	// Lazy: Open creates front socket with 0600 permissions without starting buildkitd.
	frontSock := e.BuilderSocket()
	fi, err := os.Lstat(frontSock)
	if err != nil {
		t.Fatalf("stat front socket: %v", err)
	}
	if fi.Mode()&os.ModeSocket == 0 {
		t.Fatalf("front path is not a socket: %v", fi.Mode())
	}
	if perm := fi.Mode().Perm(); perm != 0o600 {
		t.Errorf("front socket mode = %o, want 0600", perm)
	}
	if st := e.BuilderState(); st.Status != BuilderIdle {
		t.Fatalf("initial BuilderState = %+v, want idle", st)
	}
	if starts.Load() != 0 {
		t.Fatalf("builder started eagerly on Open: starts=%d", starts.Load())
	}

	// Concurrent first dials start buildkitd exactly once and pipe half-closed streams.
	const n = 8
	var wg sync.WaitGroup
	errs := make(chan error, n)
	for i := range n {
		wg.Go(func() {
			c, err := net.DialTimeout("unix", frontSock, 3*time.Second)
			if err != nil {
				errs <- err
				return
			}
			defer c.Close()
			payload := []byte("hello-" + string(rune('a'+i)))
			if _, err := c.Write(payload); err != nil {
				errs <- err
				return
			}
			if cw, ok := c.(interface{ CloseWrite() error }); ok {
				_ = cw.CloseWrite()
			}
			got, err := io.ReadAll(c)
			if err != nil {
				errs <- err
				return
			}
			want := append([]byte("echo:"), payload...)
			if !bytes.Equal(got, want) {
				errs <- io.ErrUnexpectedEOF
			}
		})
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("concurrent dial error: %v", err)
	}
	if got := starts.Load(); got != 1 {
		t.Fatalf("builder starts for %d concurrent dials = %d, want 1", n, got)
	}
	if st := e.BuilderState(); st.Status != BuilderRunning {
		t.Fatalf("BuilderState after dial = %+v, want running", st)
	}

	// Builder sandbox must never appear in docker ps -a.
	if ctrs := e.allContainers(); len(ctrs) != 0 {
		t.Fatalf("builder leaked into containers list: %+v", ctrs)
	}

	// Wait for idle timeout to stop the builder.
	deadline := time.Now().Add(3 * time.Second)
	for e.BuilderState().Status != BuilderIdle && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if st := e.BuilderState(); st.Status != BuilderIdle {
		t.Fatalf("builder did not stop after idle timeout: %+v", st)
	}
	// The builder reports idle as it asks its sandbox to stop; the stop
	// itself lands a moment later.
	for fake.State(builderSandboxID).Status.Active() && time.Now().Before(deadline.Add(2*time.Second)) {
		time.Sleep(20 * time.Millisecond)
	}
	if st := fake.State(builderSandboxID); st.Status.Active() {
		t.Fatalf("fake builder process still active after idle timeout: %+v", st)
	}

	// Next dial restarts the builder.
	c, err := net.DialTimeout("unix", frontSock, 3*time.Second)
	if err != nil {
		t.Fatalf("dial after idle stop: %v", err)
	}
	_, _ = c.Write([]byte("again"))
	if cw, ok := c.(interface{ CloseWrite() error }); ok {
		_ = cw.CloseWrite()
	}
	got, err := io.ReadAll(c)
	_ = c.Close()
	if err != nil || string(got) != "echo:again" {
		t.Fatalf("round trip after restart = %q, %v", string(got), err)
	}
	if got := starts.Load(); got != 2 {
		t.Fatalf("builder starts after idle restart = %d, want 2", got)
	}

	// Close stops the builder and removes both sockets.
	if err := e.Close(time.Second); err != nil {
		t.Fatal(err)
	}
	if st := fake.State(builderSandboxID); st.Status.Active() {
		t.Fatalf("builder still active after Close: %+v", st)
	}
	if _, err := os.Lstat(frontSock); !os.IsNotExist(err) {
		t.Errorf("front socket still exists after Close: %v", err)
	}
}

func TestBuilderSafeListenRefusesNonSocketAndLiveSocket(t *testing.T) {
	dir, err := os.MkdirTemp("", "gvxd-sl-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })

	// 1. Regular file at buildkit.sock must never be removed.
	sockPath := filepath.Join(dir, "buildkit.sock")
	if err := os.WriteFile(sockPath, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := listenBuilderUnix(sockPath); err == nil || !strings.Contains(err.Error(), "not a socket") {
		t.Fatalf("listenBuilderUnix on regular file = %v, want not a socket error", err)
	}
	if b, err := os.ReadFile(sockPath); err != nil || string(b) != "keep" {
		t.Fatalf("regular file was modified or removed: %q %v", string(b), err)
	}
	_ = os.Remove(sockPath)

	// 2. Live socket must never be replaced.
	live, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Fatal(err)
	}
	defer live.Close()
	if _, err := listenBuilderUnix(sockPath); err == nil || !strings.Contains(err.Error(), "already listening") {
		t.Fatalf("listenBuilderUnix on live socket = %v, want already listening error", err)
	}
}

func TestBuilderVolumeAndImageProtection(t *testing.T) {
	te := newTestEngine(t)
	_ = te.fake.CreateVolume(context.Background(), BuilderVolumeName, "ext4lite")
	te.fake.addImage("alpine:3.20", ImageConfig{Cmd: []string{"/bin/sh"}})

	// VolumeOwnership always protects BuilderVolumeName (open and closed).
	owned, users := te.VolumeOwnership()
	if !owned[BuilderVolumeName] || !slices.Contains(users[BuilderVolumeName], BuilderName) {
		t.Fatalf("VolumeOwnership (open) did not protect %s: owned=%v users=%v", BuilderVolumeName, owned, users)
	}

	// Builder volume is hidden from GET /volumes.
	st, body := te.do(t, http.MethodGet, "/v1.52/volumes", nil)
	if st != http.StatusOK || strings.Contains(string(body), BuilderVolumeName) {
		t.Fatalf("GET /volumes exposed builder volume: %d %s", st, body)
	}

	// POST /volumes/create refuses builder volume name.
	st, _ = te.do(t, http.MethodPost, "/v1.52/volumes/create", map[string]any{"Name": BuilderVolumeName})
	if st != http.StatusConflict {
		t.Fatalf("POST /volumes/create(%s) = %d, want 409", BuilderVolumeName, st)
	}

	// DELETE /volumes/<builderVol> refuses removal.
	st, _ = te.do(t, http.MethodDelete, "/v1.52/volumes/"+BuilderVolumeName, nil)
	if st != http.StatusForbidden {
		t.Fatalf("DELETE /volumes/%s = %d, want 403", BuilderVolumeName, st)
	}

	// Container bind to builder volume is rejected.
	st, _ = te.do(t, http.MethodPost, "/v1.52/containers/create?name=try-bvol", map[string]any{
		"Image":      "alpine:3.20",
		"HostConfig": map[string]any{"Binds": []string{BuilderVolumeName + ":/cache"}},
	})
	if st != http.StatusConflict {
		t.Fatalf("container create mounting builder volume = %d, want 409", st)
	}

	// POST /volumes/prune?all=1 never deletes builder volume.
	st, body = te.do(t, http.MethodPost, "/v1.52/volumes/prune?all=1", nil)
	if st != http.StatusOK {
		t.Fatalf("volume prune = %d %s", st, body)
	}
	vols, _ := te.fake.Volumes(context.Background())
	found := false
	for _, v := range vols {
		if v.Name == BuilderVolumeName {
			found = true
		}
	}
	if !found {
		t.Fatal("volume prune deleted builder cache volume")
	}

	// While builder is running, BuilderImage is protected from image delete and prune.
	te.fake.addImage(BuilderImage, ImageConfig{})
	te.bld.mu.Lock()
	te.bld.running = true
	te.bld.mu.Unlock()

	st, body = te.do(t, http.MethodDelete, "/v1.52/images/"+BuilderImage+"?force=1", nil)
	if st != http.StatusConflict {
		t.Fatalf("DELETE active BuilderImage = %d %s, want 409", st, body)
	}
	st, body = te.do(t, http.MethodPost, `/v1.52/images/prune?filters={"dangling":{"false":true}}`, nil)
	if st != http.StatusOK {
		t.Fatalf("image prune = %d %s", st, body)
	}
	var pruned struct {
		ImagesDeleted []map[string]string `json:"ImagesDeleted"`
	}
	_ = json.Unmarshal(body, &pruned)
	for _, item := range pruned.ImagesDeleted {
		if strings.Contains(item["Untagged"], "buildkit") {
			t.Fatalf("image prune deleted active BuilderImage: %+v", pruned)
		}
	}

	te.bld.mu.Lock()
	te.bld.running = false
	te.bld.mu.Unlock()

	// VolumeOwnership also protects BuilderVolumeName after Close.
	_ = te.Close(time.Second)
	owned, users = te.VolumeOwnership()
	if !owned[BuilderVolumeName] || !slices.Contains(users[BuilderVolumeName], BuilderName) {
		t.Fatalf("VolumeOwnership (closed) did not protect %s: owned=%v users=%v", BuilderVolumeName, owned, users)
	}
}
