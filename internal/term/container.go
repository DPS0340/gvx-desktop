package term

import (
	"crypto/rand"
	"encoding/hex"
	"sync"
)

// A shell run in a container, by docker exec or kubectl exec, is not ended
// when the stream it was run over closes: neither engine signals what an
// exec started, and its terminal is not hung up. So, as mako finds what a
// session started by a variable of its own in their environment, the
// shell is started with MarkVar set to a mark of its own, which everything
// it starts inherits, and closing it runs a sweep in the container that
// ends every process holding the mark.

// MarkVar names the variable a container shell's processes are marked by.
const MarkVar = "GVX_DESKTOP_TERMINAL"

// NewMark is a mark no other shell has.
func NewMark() string {
	var b [12]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

// ShellCommand is the command that starts a shell in a container marked
// with mark: bash where the image has it, sh otherwise, with TERM naming
// the terminal it runs in.
func ShellCommand(mark string) []string {
	return []string{"sh", "-c", "export TERM=xterm-256color " + MarkVar + "=" + mark +
		"; if command -v bash >/dev/null 2>&1; then exec bash; fi; exec sh"}
}

// SweepCommand is the command that ends, in a container, every process
// marked with mark: hung up, then, as long as any stays, terminated and
// killed, a second apart. Its own processes carry the mark in their
// arguments alone, which /proc/PID/environ does not show.
func SweepCommand(mark string) []string {
	return []string{"sh", "-c", "m=" + MarkVar + "=" + mark +
		`; for s in HUP TERM KILL; do f=; for p in /proc/[0-9]*; do ` +
		`grep -qs "$m" "$p/environ" && kill -$s "${p#/proc/}" 2>/dev/null && f=1; done; ` +
		`[ -z "$f" ] && exit 0; sleep 1; done`}
}

// Swept is prog whose Kill also runs sweep, which ends what prog started
// where its own Kill does not reach. sweep runs once.
func Swept(prog Program, sweep func()) Program {
	return &sweptProgram{Program: prog, sweep: sync.OnceFunc(sweep)}
}

type sweptProgram struct {
	Program
	sweep func()
}

func (s *sweptProgram) Kill() {
	s.sweep()
	s.Program.Kill()
}
