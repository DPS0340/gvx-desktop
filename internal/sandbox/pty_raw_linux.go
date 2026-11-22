//go:build linux

package sandbox

import (
	"os"

	"golang.org/x/sys/unix"
)

func setRawTermios(f *os.File) error {
	rc, err := f.SyscallConn()
	if err != nil {
		return err
	}
	var ioctlErr error
	if err := rc.Control(func(fd uintptr) {
		orig, err := unix.IoctlGetTermios(int(fd), unix.TCGETS)
		if err != nil {
			ioctlErr = err
			return
		}
		raw := *orig
		raw.Iflag &^= unix.IGNBRK | unix.BRKINT | unix.PARMRK | unix.ISTRIP | unix.INLCR | unix.IGNCR | unix.ICRNL | unix.IXON
		raw.Oflag &^= unix.OPOST
		raw.Lflag &^= unix.ECHO | unix.ECHONL | unix.ICANON | unix.ISIG | unix.IEXTEN
		raw.Cflag &^= unix.CSIZE | unix.PARENB
		raw.Cflag |= unix.CS8
		raw.Cc[unix.VMIN] = 1
		raw.Cc[unix.VTIME] = 0
		ioctlErr = unix.IoctlSetTermios(int(fd), unix.TCSETS, &raw)
	}); err != nil {
		return err
	}
	return ioctlErr
}
