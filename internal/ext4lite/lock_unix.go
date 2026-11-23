//go:build unix

package ext4lite

import (
	"errors"
	"os"

	"golang.org/x/sys/unix"
)

// lockShared takes the shared flock that rungv's read-only mounts take; a
// writable mount holds it exclusively.
func lockShared(f *os.File) (func(), error) {
	fd := int(f.Fd())
	for {
		err := unix.Flock(fd, unix.LOCK_SH|unix.LOCK_NB)
		if err == nil {
			return func() { _ = unix.Flock(fd, unix.LOCK_UN) }, nil
		}
		if errors.Is(err, unix.EINTR) {
			continue
		}
		if errors.Is(err, unix.EWOULDBLOCK) {
			return nil, ErrInUse
		}
		return nil, err
	}
}
