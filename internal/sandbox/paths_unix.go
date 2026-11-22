//go:build !windows

package sandbox

import (
	"errors"
	"fmt"
	"os"
	"syscall"
)

func currentEUID() int { return os.Geteuid() }

// ensurePrivateSocketDir ensures dir exists as a real (non-symlink) directory
// owned by the current effective UID with mode 0700.
func ensurePrivateSocketDir(dir string) error {
	if err := os.Mkdir(dir, 0o700); err != nil && !errors.Is(err, os.ErrExist) {
		return fmt.Errorf("create control socket directory %s: %w", dir, err)
	}
	info, err := os.Lstat(dir)
	if err != nil {
		return fmt.Errorf("inspect control socket directory %s: %w", dir, err)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("control socket directory %s must not be a symlink", dir)
	}
	if !info.IsDir() {
		return fmt.Errorf("control socket directory %s is not a directory", dir)
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return fmt.Errorf("inspect ownership of control socket directory %s", dir)
	}
	wantUID := uint32(os.Geteuid())
	if st.Uid != wantUID {
		return fmt.Errorf("control socket directory %s is owned by uid %d, want %d", dir, st.Uid, wantUID)
	}
	if perm := info.Mode().Perm(); perm != 0o700 {
		return fmt.Errorf("control socket directory %s has mode %04o, want 0700", dir, perm)
	}
	return nil
}

// removeSocketFile removes path only when Lstat reports a Unix socket.
func removeSocketFile(path string) {
	if path == "" {
		return
	}
	if info, err := os.Lstat(path); err == nil && info.Mode()&os.ModeSocket != 0 {
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
