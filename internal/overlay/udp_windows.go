//go:build windows

package overlay

import (
	"errors"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

// disableUDPConnReset disables Winsock's SIO_UDP_CONNRESET behavior so an ICMP
// Port Unreachable from a stopped container never poisons subsequent ReadFrom
// calls on the shared WireGuard hub or per-container DNS UDP sockets.
func disableUDPConnReset(sc syscall.Conn) {
	raw, err := sc.SyscallConn()
	if err != nil {
		return
	}
	var zero uint32
	var bytesReturned uint32
	_ = raw.Control(func(fd uintptr) {
		_ = windows.WSAIoctl(
			windows.Handle(fd),
			windows.SIO_UDP_CONNRESET,
			(*byte)(unsafe.Pointer(&zero)),
			uint32(unsafe.Sizeof(zero)),
			nil,
			0,
			&bytesReturned,
			nil,
			0,
		)
	})
}

// isPlatformTransientUDPErr reports whether err is a per-datagram Winsock
// error that should be ignored rather than terminating a UDP receive loop.
func isPlatformTransientUDPErr(err error) bool {
	return errors.Is(err, windows.WSAECONNRESET) ||
		errors.Is(err, windows.WSAEMSGSIZE) ||
		errors.Is(err, windows.WSAENETRESET)
}
