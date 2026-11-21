//go:build !windows

package sandbox

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/ironpark/gvx-desktop/internal/rungv"
)

// rungvTempCacheForTest returns a rungv Client pointing at a fresh temporary
// cache directory so integration tests never touch the user's real cache.
func rungvTempCacheForTest(t *testing.T) *rungv.Client {
	t.Helper()
	if testing.Short() {
		t.Skip("integration test")
	}
	path, err := rungv.Locate(filepath.Join("..", "..", "bin", rungv.BinaryName()))
	if err != nil {
		t.Skip("rungv not found")
	}
	cache := t.TempDir()
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

func TestSupervisorIntegrationStdioPTYAndSignal(t *testing.T) {
	c := rungvTempCacheForTest(t)

	t.Run("interactive stdin round trip", func(t *testing.T) {
		var mu sync.Mutex
		var out bytes.Buffer
		s, _ := newTestSupervisor(t, c)
		s.Output = func(id string, o Output) {
			if o.Stream == Stdout {
				mu.Lock()
				out.Write(o.Data)
				mu.Unlock()
			}
		}
		spec := Spec{
			ID:          NewID(),
			Name:        "gvxd-stdio-cat",
			Image:       "alpine:3.20",
			Command:     "cat",
			Interactive: true,
			Pull:        "never",
		}
		if err := s.Start(spec); err != nil {
			t.Fatal(err)
		}
		w := s.Stdin(spec.ID)
		if w == nil {
			t.Fatal("nil Stdin")
		}
		if _, err := w.Write([]byte("hello-interactive\n")); err != nil {
			t.Fatal(err)
		}
		if err := s.CloseStdin(spec.ID); err != nil {
			t.Fatal(err)
		}
		waitFor(t, "cat exit", time.Minute, func() bool { return !s.State(spec.ID).Status.Active() }, s.Logs(spec.ID))
		if st := s.State(spec.ID); st.Status != Exited || st.ExitCode != 0 {
			t.Fatalf("state = %+v", st)
		}
		mu.Lock()
		got := out.String()
		mu.Unlock()
		if got != "hello-interactive\n" {
			t.Fatalf("stdout = %q, want %q", got, "hello-interactive\n")
		}
	})

	t.Run("TTY prompt resize and exit code", func(t *testing.T) {
		var mu sync.Mutex
		var out bytes.Buffer
		s, _ := newTestSupervisor(t, c)
		s.Output = func(id string, o Output) {
			if o.Stream == Stdout {
				mu.Lock()
				out.Write(o.Data)
				mu.Unlock()
			}
		}
		spec := Spec{
			ID:          NewID(),
			Name:        "gvxd-stdio-tty",
			Image:       "alpine:3.20",
			Command:     "/bin/sh",
			Interactive: true,
			TTY:         true,
			Pull:        "never",
		}
		if err := s.Start(spec); err != nil {
			t.Fatal(err)
		}
		// Prompt arrives without a trailing newline.
		waitFor(t, "shell prompt", time.Minute, func() bool {
			mu.Lock()
			defer mu.Unlock()
			return strings.Contains(out.String(), "# ")
		}, s.Logs(spec.ID))

		if err := s.Resize(spec.ID, 41, 107); err != nil {
			t.Fatal(err)
		}
		if _, err := s.Stdin(spec.ID).Write([]byte("stty size\n")); err != nil {
			t.Fatal(err)
		}
		waitFor(t, "stty size 41 107", 20*time.Second, func() bool {
			mu.Lock()
			defer mu.Unlock()
			return strings.Contains(out.String(), "41 107")
		}, s.Logs(spec.ID))

		if _, err := s.Stdin(spec.ID).Write([]byte("exit 3\n")); err != nil {
			t.Fatal(err)
		}
		waitFor(t, "exit 3", 20*time.Second, func() bool { return !s.State(spec.ID).Status.Active() }, s.Logs(spec.ID))
		if st := s.State(spec.ID); st.ExitCode != 3 {
			t.Fatalf("ExitCode = %d, want 3 (state=%+v)", st.ExitCode, st)
		}
	})

	t.Run("TTY CloseStdin sends EOT", func(t *testing.T) {
		var mu sync.Mutex
		var out bytes.Buffer
		s, _ := newTestSupervisor(t, c)
		s.Output = func(id string, o Output) {
			if o.Stream == Stdout {
				mu.Lock()
				out.Write(o.Data)
				mu.Unlock()
			}
		}
		spec := Spec{
			ID:          NewID(),
			Name:        "gvxd-stdio-eot",
			Image:       "alpine:3.20",
			Command:     "/bin/sh",
			Interactive: true,
			TTY:         true,
			Pull:        "never",
		}
		if err := s.Start(spec); err != nil {
			t.Fatal(err)
		}
		waitFor(t, "shell prompt", time.Minute, func() bool {
			mu.Lock()
			defer mu.Unlock()
			return strings.Contains(out.String(), "# ")
		}, s.Logs(spec.ID))
		if err := s.CloseStdin(spec.ID); err != nil {
			t.Fatal(err)
		}
		waitFor(t, "shell exit on EOT", 20*time.Second, func() bool { return !s.State(spec.ID).Status.Active() }, s.Logs(spec.ID))
		if st := s.State(spec.ID); st.Status != Exited || st.ExitCode != 0 {
			t.Fatalf("state = %+v", st)
		}
	})

	t.Run("Signal SIGKILL sets State.Signal == 9", func(t *testing.T) {
		s, _ := newTestSupervisor(t, c)
		spec := Spec{
			ID:      NewID(),
			Name:    "gvxd-stdio-kill",
			Image:   "alpine:3.20",
			Command: "sleep 30",
			Pull:    "never",
		}
		if err := s.Start(spec); err != nil {
			t.Fatal(err)
		}
		waitFor(t, "running", 20*time.Second, func() bool { return s.State(spec.ID).Status == Running }, s.Logs(spec.ID))
		if err := s.Signal(spec.ID, syscall.SIGKILL); err != nil {
			t.Fatal(err)
		}
		waitFor(t, "killed", 20*time.Second, func() bool { return !s.State(spec.ID).Status.Active() }, s.Logs(spec.ID))
		st := s.State(spec.ID)
		if st.Signal != int(syscall.SIGKILL) || st.ExitCode != -1 {
			t.Fatalf("after SIGKILL: %+v", st)
		}
	})
}
