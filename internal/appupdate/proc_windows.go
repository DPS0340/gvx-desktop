package appupdate

import (
	"os/exec"
	"syscall"
)

// detach starts cmd with no console and in a process group of its own, so
// that it outlives the app.
func detach(cmd *exec.Cmd) {
	const detachedProcess, newProcessGroup = 0x00000008, 0x00000200
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: detachedProcess | newProcessGroup}
}
