//go:build unix

package term

import (
	"errors"
	"os"
	"os/exec"
	"time"

	"github.com/creack/pty"
	"golang.org/x/sys/unix"
)

// proc is a process on a pty: reading it reads the terminal's output, and
// writing it types.
type proc struct {
	cmd *exec.Cmd
	tty *os.File
}

// startProc starts argv with env, on a terminal cols by rows. pty makes
// the process the leader of a session of its own, whose controlling
// terminal the pty is, so what it starts is found by that session.
func startProc(argv []string, env []string, cols, rows uint16) (*proc, error) {
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Env = env
	tty, err := pty.StartWithSize(cmd, &pty.Winsize{Cols: cols, Rows: rows})
	if err != nil {
		return nil, err
	}
	return &proc{cmd: cmd, tty: pollable(tty)}, nil
}

// pollable is the terminal f as a file the runtime's poller waits on. pty
// leaves its file blocking, as its ioctls take its descriptor, and a read
// blocked on that is ended neither by a deadline nor by closing it, only
// by the last process holding the terminal letting go. f is kept, where
// it cannot be copied.
func pollable(f *os.File) *os.File {
	fd, err := unix.Dup(int(f.Fd()))
	if err != nil {
		return f
	}
	unix.CloseOnExec(fd)
	if err := unix.SetNonblock(fd, true); err != nil {
		unix.Close(fd)
		return f
	}
	g := os.NewFile(uintptr(fd), f.Name())
	f.Close()
	return g
}

func (p *proc) Read(b []byte) (int, error)  { return p.tty.Read(b) }
func (p *proc) Write(b []byte) (int, error) { return p.tty.Write(b) }

// Close closes the terminal.
func (p *proc) Close() error { return p.tty.Close() }

// EndOutput ends reading the terminal after grace, the process gone: the
// output otherwise ends only once every process holding the terminal has
// closed it, which one the process left in the background may never do.
func (p *proc) EndOutput(grace time.Duration) {
	_ = p.tty.SetReadDeadline(time.Now().Add(grace))
}

// Resize sizes the terminal cols by rows of cells cellW by cellH pixels,
// which zero leaves unsaid. The kernel tells the terminal's foreground
// process group with SIGWINCH.
func (p *proc) Resize(cols, rows, cellW, cellH uint16) error {
	raw, err := p.tty.SyscallConn()
	if err != nil {
		return err
	}
	ws := &unix.Winsize{Col: cols, Row: rows, Xpixel: cols * cellW, Ypixel: rows * cellH}
	// Control, rather than pty.Setsize, which takes the file's descriptor
	// and so makes it blocking again.
	if cerr := raw.Control(func(fd uintptr) { err = unix.IoctlSetWinsize(int(fd), unix.TIOCSWINSZ, ws) }); cerr != nil {
		return cerr
	}
	return err
}

// Kill ends the process and every process of its terminal's session: the
// leader at once, the rest as Wait sweeps them.
func (p *proc) Kill() {
	_ = p.cmd.Process.Kill()
}

// Wait is the process's exit code once it exits, -1 when it says none.
// What it started is swept then, while its id still names its terminal's
// session: later that id may be another process's.
func (p *proc) Wait() int {
	err := p.cmd.Wait()
	sweep(p.cmd.Process.Pid)
	var exitErr *exec.ExitError
	switch {
	case err == nil:
		return 0
	case errors.As(err, &exitErr):
		return exitErr.ExitCode()
	}
	return -1
}
