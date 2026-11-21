//go:build !windows

package sandbox

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/ironpark/gvx-desktop/internal/rungv"
	"golang.org/x/sys/unix"
)

func TestSupervisorTTYWithoutInteractiveAndNonBlockingResize(t *testing.T) {
	exe := filepath.Join(t.TempDir(), "rungv")
	script := "#!/bin/sh\nexec sleep 10\n"
	if err := os.WriteFile(exe, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}

	s, _ := newTestSupervisor(t, &rungv.Client{Path: exe})
	spec := Spec{ID: "tty-no-i", Name: "tty-no-i", Image: "alpine", TTY: true, Interactive: false}
	if err := s.Start(spec); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "running", 5*time.Second, func() bool { return s.State(spec.ID).Status == Running })

	// TTY without Interactive must not expose a writable Stdin.
	if w := s.Stdin(spec.ID); w != nil {
		t.Fatalf("Stdin returned non-nil for TTY=true, Interactive=false")
	}

	// Capture ptmx non-blocking flag before and after Resize to verify
	// TIOCSWINSZ via SyscallConn().Control does not flip ptmx to blocking mode.
	s.mu.Lock()
	ptmx := s.procs[spec.ID].term.ptmx
	s.mu.Unlock()

	getFlags := func() int {
		t.Helper()
		rc, err := ptmx.SyscallConn()
		if err != nil {
			t.Fatalf("SyscallConn: %v", err)
		}
		var flags int
		var ferr error
		if err := rc.Control(func(fd uintptr) {
			flags, ferr = unix.FcntlInt(fd, unix.F_GETFL, 0)
		}); err != nil || ferr != nil {
			t.Fatalf("F_GETFL: %v / %v", err, ferr)
		}
		return flags
	}

	before := getFlags()
	if err := s.Resize(spec.ID, 40, 120); err != nil {
		t.Fatalf("Resize: %v", err)
	}
	after := getFlags()
	if (before & unix.O_NONBLOCK) != (after & unix.O_NONBLOCK) {
		t.Fatalf("Resize changed O_NONBLOCK on ptmx: before=%#x after=%#x", before, after)
	}

	// Verify TIOCSWINSZ took effect on the PTY.
	rc, err := ptmx.SyscallConn()
	if err != nil {
		t.Fatal(err)
	}
	var ws *unix.Winsize
	var gerr error
	if err := rc.Control(func(fd uintptr) {
		ws, gerr = unix.IoctlGetWinsize(int(fd), unix.TIOCGWINSZ)
	}); err != nil || gerr != nil {
		t.Fatalf("TIOCGWINSZ: %v / %v", err, gerr)
	}
	if ws.Row != 40 || ws.Col != 120 {
		t.Fatalf("winsize = (%d,%d), want (40,120)", ws.Row, ws.Col)
	}

	// CloseStdin still sends EOT on a TTY spec.
	if err := s.CloseStdin(spec.ID); err != nil {
		t.Fatalf("CloseStdin on TTY: %v", err)
	}
}
