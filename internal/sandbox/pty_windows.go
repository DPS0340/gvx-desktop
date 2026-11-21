//go:build windows

package sandbox

import (
	"errors"
	"io"
	"os"
	"os/exec"
	"syscall"
)

const ptySupported = false

var errWindowsPTY = errors.New("terminal is not supported on Windows")

type term struct{}

func openPTY(cmd *exec.Cmd) (*term, io.Reader, func(), error) {
	return nil, nil, nil, errWindowsPTY
}

func (t *term) writer() io.Writer              { return nil }
func (t *term) closeStdin() error              { return errWindowsPTY }
func (t *term) resize(rows, cols uint16) error { return errWindowsPTY }
func (t *term) close() error                   { return nil }

func isPTYEIO(err error) bool            { return false }
func waitSignal(ps *os.ProcessState) int { return 0 }

func signalProc(proc *os.Process, sig syscall.Signal) error {
	if sig == syscall.SIGKILL || sig == syscall.SIGTERM || sig == syscall.SIGINT {
		return proc.Kill()
	}
	return proc.Signal(sig)
}
