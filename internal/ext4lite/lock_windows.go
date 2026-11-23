package ext4lite

import (
	"errors"
	"os"

	"golang.org/x/sys/windows"
)

// lockShared takes the shared whole-file lock that rungv's read-only mounts
// take; a writable mount holds it exclusively.
func lockShared(f *os.File) (func(), error) {
	h := windows.Handle(f.Fd())
	ol := new(windows.Overlapped)
	err := windows.LockFileEx(h, windows.LOCKFILE_FAIL_IMMEDIATELY, 0, 0xFFFFFFFF, 0xFFFFFFFF, ol)
	if err != nil {
		if errors.Is(err, windows.ERROR_LOCK_VIOLATION) {
			return nil, ErrInUse
		}
		return nil, err
	}
	return func() { _ = windows.UnlockFileEx(h, 0, 0xFFFFFFFF, 0xFFFFFFFF, ol) }, nil
}
