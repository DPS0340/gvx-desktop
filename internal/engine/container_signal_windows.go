//go:build windows

package engine

// Owner: workstream B. Signal parsing on Windows for /containers/{id}/kill.

import (
	"strconv"
	"strings"
	"syscall"
)

var signalNames = map[string]syscall.Signal{
	"HUP":  syscall.SIGHUP,
	"INT":  syscall.SIGINT,
	"QUIT": syscall.SIGQUIT,
	"KILL": syscall.SIGKILL,
	"USR1": syscall.Signal(10),
	"USR2": syscall.Signal(12),
	"PIPE": syscall.SIGPIPE,
	"ALRM": syscall.SIGALRM,
	"TERM": syscall.SIGTERM,
	"CONT": syscall.Signal(18),
	"STOP": syscall.Signal(19),
}

func parseSignal(raw string) (syscall.Signal, error) {
	s := strings.TrimSpace(raw)
	if s == "" {
		return syscall.SIGKILL, nil
	}
	if n, err := strconv.Atoi(s); err == nil {
		if n <= 0 || n > 64 {
			return 0, errInvalid("Invalid signal: %s", raw)
		}
		return syscall.Signal(n), nil
	}
	name := strings.ToUpper(s)
	name = strings.TrimPrefix(name, "SIG")
	if sig, ok := signalNames[name]; ok {
		return sig, nil
	}
	return 0, errInvalid("Invalid signal: %s", raw)
}
