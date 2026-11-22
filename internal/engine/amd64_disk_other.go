//go:build windows

package engine

import "os"

func allocatedFileBytes(path string) int64 {
	fi, err := os.Stat(path)
	if err != nil {
		return 0
	}
	return fi.Size()
}
