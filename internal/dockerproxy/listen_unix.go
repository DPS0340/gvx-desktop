//go:build !windows

package dockerproxy

import (
	"errors"
	"net"
	"os"
	"path/filepath"
	"syscall"
)

func isConnRefused(err error) bool {
	return errors.Is(err, syscall.ECONNREFUSED)
}

// bindUnixSocket creates a Unix socket inside a fresh 0700 directory beside
// path, sets it to 0600, and atomically renames it onto path so the socket is
// never visible with permissions wider than 0600.
func bindUnixSocket(path string, _ bool) (net.Listener, error) {
	tmpDir, err := os.MkdirTemp(filepath.Dir(path), ".s*")
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
	if err := os.Rename(tmpSock, path); err != nil {
		_ = ln.Close()
		_ = os.RemoveAll(tmpDir)
		return nil, err
	}
	_ = os.Remove(tmpDir)
	return ln, nil
}
