//go:build !windows

package sandbox

import (
	"errors"
	"io"
	"os"
	"os/exec"
	"syscall"

	"github.com/creack/pty"
	"golang.org/x/sys/unix"
)

const ptySupported = true

type term struct {
	ptmx *os.File
}

// openPTY attaches a pseudo-terminal slave to cmd's stdin and stdout and
// makes it cmd's controlling terminal (Setsid + Setctty) so window resizes
// reach the guest. The caller must call closeSlave right after cmd.Start,
// and close t on failure or after the process exits.
func openPTY(cmd *exec.Cmd) (t *term, stdout io.Reader, closeSlave func(), err error) {
	ptmx, tty, err := pty.Open()
	if err != nil {
		return nil, nil, nil, err
	}
	cmd.Stdin = tty
	cmd.Stdout = tty
	attr := cmd.SysProcAttr
	if attr == nil {
		attr = &syscall.SysProcAttr{}
		cmd.SysProcAttr = attr
	}
	attr.Setsid = true
	attr.Setctty = true
	return &term{ptmx: ptmx}, ptmx, func() { _ = tty.Close() }, nil
}

func (t *term) writer() io.Writer { return t.ptmx }

func (t *term) closeStdin() error {
	_, err := t.ptmx.Write([]byte{0x04}) // EOT
	return err
}

// resize issues TIOCSWINSZ through SyscallConn().Control so ptmx is never
// switched into blocking mode by (*os.File).Fd().
func (t *term) resize(rows, cols uint16) error {
	rc, err := t.ptmx.SyscallConn()
	if err != nil {
		return err
	}
	ws := &unix.Winsize{Row: rows, Col: cols}
	var ioctlErr error
	if err := rc.Control(func(fd uintptr) {
		ioctlErr = unix.IoctlSetWinsize(int(fd), unix.TIOCSWINSZ, ws)
	}); err != nil {
		return err
	}
	return ioctlErr
}

func (t *term) close() error { return t.ptmx.Close() }

func isPTYEIO(err error) bool { return errors.Is(err, syscall.EIO) }

func waitSignal(ps *os.ProcessState) int {
	if ps == nil {
		return 0
	}
	if ws, ok := ps.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
		return int(ws.Signal())
	}
	return 0
}

func signalProc(proc *os.Process, sig syscall.Signal) error {
	return proc.Signal(sig)
}
