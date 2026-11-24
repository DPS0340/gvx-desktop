//go:build !windows

package overlay

import "syscall"

func disableUDPConnReset(syscall.Conn) {}

func isPlatformTransientUDPErr(error) bool { return false }
