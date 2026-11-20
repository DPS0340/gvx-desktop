//go:build windows

package dockerproxy

import (
	"errors"
	"net"
	"os"
	"syscall"
)

const wsaeconnrefused = syscall.Errno(10061)

func isConnRefused(err error) bool {
	return errors.Is(err, wsaeconnrefused)
}

func bindUnixSocket(path string, stale bool) (net.Listener, error) {
	if stale {
		if err := os.Remove(path); err != nil {
			return nil, err
		}
	}
	return net.Listen("unix", path)
}
