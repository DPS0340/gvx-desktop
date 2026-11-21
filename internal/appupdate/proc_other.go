//go:build !windows

package appupdate

import (
	"os/exec"
	"syscall"
)

// detach starts cmd in a session of its own, so that it outlives the app.
func detach(cmd *exec.Cmd) { cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true} }
