//go:build windows

package main

import (
	"os/exec"
	"syscall"
	"time"
)

func stopRunningAppPlatform() {
	names := []string{"GVX Desktop.exe", "gvx-desktop.exe"}
	for _, name := range names {
		_ = runHiddenTaskkill("/IM", name)
	}
	time.Sleep(1500 * time.Millisecond)
	for _, name := range names {
		_ = runHiddenTaskkill("/F", "/IM", name)
	}
	time.Sleep(400 * time.Millisecond)
}

func runHiddenTaskkill(args ...string) error {
	cmd := exec.Command("taskkill", args...)
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
	return cmd.Run()
}
