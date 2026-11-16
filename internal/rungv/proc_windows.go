//go:build windows

package rungv

import (
	"os/exec"
	"syscall"
)

// hideWindow keeps a console window from flashing up for each rungv call.
func hideWindow(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: 0x08000000} // CREATE_NO_WINDOW
}
