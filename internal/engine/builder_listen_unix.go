//go:build !windows

package engine

import (
	"errors"
	"fmt"
	"io/fs"
	"net"
	"os"
	"path/filepath"
	"syscall"
	"time"
)

const builderStaleDialTimeout = 500 * time.Millisecond

func isBuilderConnRefused(err error) bool {
	return errors.Is(err, syscall.ECONNREFUSED)
}

// listenBuilderUnix binds a Unix socket at sockPath with mode 0600 using the
// same safety rules as dockerproxy.listenUnix: an existing path is replaced
// only if Lstat confirms it is a socket file and dialing it fails with
// ECONNREFUSED. Live sockets, regular files, and symlinks are never removed.
func listenBuilderUnix(sockPath string) (net.Listener, error) {
	if err := os.MkdirAll(filepath.Dir(sockPath), 0o700); err != nil {
		return nil, err
	}
	if fi, err := os.Lstat(sockPath); err == nil {
		if fi.Mode()&fs.ModeSocket == 0 {
			return nil, fmt.Errorf("%s already exists and is not a socket", sockPath)
		}
		c, derr := net.DialTimeout("unix", sockPath, builderStaleDialTimeout)
		if derr == nil {
			_ = c.Close()
			return nil, fmt.Errorf("another process is already listening on %s", sockPath)
		}
		if !isBuilderConnRefused(derr) {
			return nil, fmt.Errorf("cannot check existing socket %s: %w", sockPath, derr)
		}
	} else if !errors.Is(err, fs.ErrNotExist) {
		return nil, err
	}

	tmpDir, err := os.MkdirTemp(filepath.Dir(sockPath), ".bk*")
	if err != nil {
		return nil, err
	}
	tmpSock := filepath.Join(tmpDir, "s")
	ln, err := net.Listen("unix", tmpSock)
	if err != nil {
		_ = os.RemoveAll(tmpDir)
		return nil, err
	}
	if ul, ok := ln.(*net.UnixListener); ok {
		ul.SetUnlinkOnClose(false)
	}
	if err := os.Chmod(tmpSock, 0o600); err != nil {
		_ = ln.Close()
		_ = os.RemoveAll(tmpDir)
		return nil, err
	}
	if err := os.Rename(tmpSock, sockPath); err != nil {
		_ = ln.Close()
		_ = os.RemoveAll(tmpDir)
		return nil, err
	}
	_ = os.Remove(tmpDir)
	return ln, nil
}

// removeStaleBuilderSocket deletes sockPath only if it is a stale socket
// (not a live listener and not a non-socket file or symlink).
func removeStaleBuilderSocket(sockPath string) error {
	fi, err := os.Lstat(sockPath)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if fi.Mode()&fs.ModeSocket == 0 {
		return fmt.Errorf("%s already exists and is not a socket", sockPath)
	}
	c, derr := net.DialTimeout("unix", sockPath, builderStaleDialTimeout)
	if derr == nil {
		_ = c.Close()
		return fmt.Errorf("another process is already listening on %s", sockPath)
	}
	if !isBuilderConnRefused(derr) {
		return fmt.Errorf("cannot check existing socket %s: %w", sockPath, derr)
	}
	return os.Remove(sockPath)
}
