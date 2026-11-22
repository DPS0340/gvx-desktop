//go:build !windows

package engine

import (
	"os"
	"syscall"
)

// allocatedFileBytes returns the number of bytes allocated on disk for path
// (respecting sparse files such as the persistent amd64 ext4 disk image), or 0
// when the file does not exist.
func allocatedFileBytes(path string) int64 {
	fi, err := os.Stat(path)
	if err != nil {
		return 0
	}
	if st, ok := fi.Sys().(*syscall.Stat_t); ok && st != nil {
		return st.Blocks * 512
	}
	return fi.Size()
}
