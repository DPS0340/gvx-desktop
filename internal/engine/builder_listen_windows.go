//go:build windows

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

const (
	builderStaleDialTimeout = 500 * time.Millisecond
	wsaeconnrefused         = syscall.Errno(10061)
)

func isBuilderConnRefused(err error) bool {
	return errors.Is(err, wsaeconnrefused)
}

func listenBuilderUnix(sockPath string) (net.Listener, error) {
	if err := os.MkdirAll(filepath.Dir(sockPath), 0o700); err != nil {
		return nil, err
	}
	stale := false
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
		stale = true
	} else if !errors.Is(err, fs.ErrNotExist) {
		return nil, err
	}
	if stale {
		if err := os.Remove(sockPath); err != nil {
			return nil, err
		}
	}
	return net.Listen("unix", sockPath)
}

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
