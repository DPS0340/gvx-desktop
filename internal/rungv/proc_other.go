//go:build !windows

package rungv

import "os/exec"

func hideWindow(*exec.Cmd) {}
