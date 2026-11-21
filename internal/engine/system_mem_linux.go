//go:build linux

package engine

// Owner: workstream C.

import "golang.org/x/sys/unix"

func hostMemTotal() int64 {
	var si unix.Sysinfo_t
	if err := unix.Sysinfo(&si); err != nil {
		return 0
	}
	return int64(uint64(si.Totalram) * uint64(si.Unit))
}
