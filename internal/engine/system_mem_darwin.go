//go:build darwin

package engine

// Owner: workstream C.

import "golang.org/x/sys/unix"

func hostMemTotal() int64 {
	v, err := unix.SysctlUint64("hw.memsize")
	if err != nil {
		return 0
	}
	return int64(v)
}
