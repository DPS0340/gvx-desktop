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
	builderStaleDialTimeout   = 500 * time.Millisecond
	wsaeconnrefused           = syscall.Errno(10061)
	defaultWindowsBuilderPort = 23760
)

func isBuilderConnRefused(err error) bool {
	return errors.Is(err, wsaeconnrefused)
}

func isBuilderSocketMode(m fs.FileMode) bool {
	return m&(fs.ModeSocket|fs.ModeIrregular) != 0
}

// defaultBuilderEndpoint returns the loopback TCP endpoint advertised to
// docker buildx on Windows (buildx's remote driver uses url.Parse and cannot
// dial Windows drive-letter unix:// paths).
func defaultBuilderEndpoint(dir string) string {
	if dir == "" {
		return ""
	}
	return fmt.Sprintf("tcp://127.0.0.1:%d", defaultWindowsBuilderPort)
}

// listenBuilderFront binds a loopback TCP listener for docker buildx on
// Windows, trying the preferred port first and falling back to an ephemeral
// port when occupied.
func listenBuilderFront(_ string) (ln net.Listener, endpoint, sockFile string, err error) {
	ln, err = net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", defaultWindowsBuilderPort))
	if err != nil {
		ln, err = net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			return nil, "", "", err
		}
	}
	return ln, "tcp://" + ln.Addr().String(), "", nil
}

func listenBuilderUnix(sockPath string) (net.Listener, error) {
	if err := os.MkdirAll(filepath.Dir(sockPath), 0o700); err != nil {
		return nil, err
	}
	stale := false
	if fi, err := os.Lstat(sockPath); err == nil {
		if fi.IsDir() || fi.Mode()&fs.ModeSymlink != 0 || !isBuilderSocketMode(fi.Mode()) {
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
	if fi.IsDir() || fi.Mode()&fs.ModeSymlink != 0 || !isBuilderSocketMode(fi.Mode()) {
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
