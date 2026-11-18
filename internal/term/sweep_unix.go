//go:build unix

package term

import (
	"os"
	"time"

	"golang.org/x/sys/unix"
)

// sweepStep is how long a process the sweep signalled has to exit before
// it is signalled again, harder.
const sweepStep = time.Second

// sweep ends every process left in the terminal session leader led, the
// leader gone: hung up at once, then, as long as any stays, terminated and
// killed. Each round finds them anew, so a process id reused meanwhile is
// not signalled.
func sweep(leader int) {
	if signalSwept(leader, unix.SIGHUP) {
		go func() {
			for _, sig := range []unix.Signal{unix.SIGTERM, unix.SIGKILL} {
				time.Sleep(sweepStep)
				if !signalSwept(leader, sig) {
					return
				}
			}
		}()
	}
}

func signalSwept(leader int, sig unix.Signal) bool {
	pids := swept(leader)
	for _, pid := range pids {
		_ = unix.Kill(pid, sig)
	}
	return len(pids) > 0
}

// swept are the processes of the terminal's session, which the program
// led. A process with the leader's id is not its, the leader gone: one
// that took its id.
func swept(leader int) []int {
	self := os.Getpid()
	var out []int
	for _, pid := range processes() {
		if pid <= 1 || pid == self || pid == leader {
			continue
		}
		if sid, err := unix.Getsid(pid); err == nil && sid == leader {
			out = append(out, pid)
		}
	}
	return out
}
