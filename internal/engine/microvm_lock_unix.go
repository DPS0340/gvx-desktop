//go:build unix

package engine

import (
	"errors"
	"fmt"
	"os"

	"golang.org/x/sys/unix"
)

// lockVolumeFile acquires an advisory flock on an ext4 volume image matching
// rungv's ext4lite locking semantics (LOCK_EX for writable mounts, LOCK_SH for
// read-only mounts). The returned unlock function releases the lock and closes
// the file descriptor.
func lockVolumeFile(path string, readOnly bool) (func(), error) {
	flag := os.O_RDWR
	how := unix.LOCK_EX | unix.LOCK_NB
	if readOnly {
		flag = os.O_RDONLY
		how = unix.LOCK_SH | unix.LOCK_NB
	}
	f, err := os.OpenFile(path, flag, 0)
	if err != nil {
		return nil, err
	}
	fd := int(f.Fd())
	for {
		err = unix.Flock(fd, how)
		if err == nil {
			return func() {
				_ = unix.Flock(fd, unix.LOCK_UN)
				_ = f.Close()
			}, nil
		}
		if errors.Is(err, unix.EINTR) {
			continue
		}
		_ = f.Close()
		if errors.Is(err, unix.EWOULDBLOCK) {
			return nil, fmt.Errorf("volume %s is in use (locked)", path)
		}
		return nil, err
	}
}
