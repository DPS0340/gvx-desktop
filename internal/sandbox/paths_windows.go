//go:build windows

package sandbox

import "os"

func currentEUID() int { return 0 }

func ensurePrivateSocketDir(dir string) error {
	return os.MkdirAll(dir, 0o700)
}

// removeSocketFile removes path only when Lstat reports a socket-like file
// (never a directory or symlink).
func removeSocketFile(path string) {
	if path == "" {
		return
	}
	info, err := os.Lstat(path)
	if err != nil || info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return
	}
	if info.Mode()&(os.ModeSocket|os.ModeIrregular) != 0 {
		_ = os.Remove(path)
	}
}

// removeTokenFile removes path only when Lstat reports a regular file.
func removeTokenFile(path string) {
	if path == "" {
		return
	}
	if info, err := os.Lstat(path); err == nil && info.Mode().IsRegular() {
		_ = os.Remove(path)
	}
}
