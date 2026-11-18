//go:build unix

package term

import (
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"github.com/ironpark/gostty/input"
)

// shell starts /bin/sh on a terminal cols by rows, closed with the test.
func shell(t *testing.T, cols, rows uint16, o Options, args ...string) *Session {
	t.Helper()
	prog, err := Start(append([]string{"/bin/sh"}, args...), Env(append(os.Environ(), "PS1=$ ")), cols, rows)
	if err != nil {
		t.Fatal(err)
	}
	o.Cols, o.Rows = cols, rows
	s, err := New(prog, o)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func TestAShellRunsAndExits(t *testing.T) {
	woken := make(chan struct{}, 1)
	s := shell(t, 80, 24, Options{Wake: func() {
		select {
		case woken <- struct{}{}:
		default:
		}
	}})
	if _, err := s.Write([]byte("echo hel''lo; exit 3\n")); err != nil {
		t.Fatal(err)
	}
	select {
	case <-s.Done():
	case <-time.After(10 * time.Second):
		t.Fatal("the shell did not exit")
	}
	if exited, code := s.Exited(); !exited || code != 3 {
		t.Fatalf("exited %v with %d, want 3", exited, code)
	}
	// The command line is echoed as typed; only the output reads hello.
	if text := screenText(t, s); !strings.Contains(text, "\nhello") {
		t.Fatalf("the screen shows no hello:\n%s", text)
	}
	select {
	case <-woken:
	default:
		t.Fatal("output asked for no paint")
	}
	if f, _ := s.Frame(); !f.Exited || f.ExitCode != 3 {
		t.Fatalf("the frame says exited %v with %d", f.Exited, f.ExitCode)
	}
	// What is typed after the end goes nowhere.
	if _, err := s.Write([]byte("x")); err == nil {
		t.Fatal("wrote to a program that exited")
	}
}

func TestResizeReachesTheProgram(t *testing.T) {
	s := shell(t, 80, 24, Options{})
	s.Resize(100, 30, 8, 16)
	// The first size goes at once; one inside resizeSettle of it waits.
	s.Resize(91, 33, 8, 16)
	if cols, rows := s.Size(); cols != 91 || rows != 33 {
		t.Fatalf("the terminal is %dx%d", cols, rows)
	}
	time.Sleep(2 * resizeSettle)
	s.Write([]byte("stty size\n"))
	waitFor(t, "stty size", func() bool { return strings.Contains(screenText(t, s), "33 91") })
	if f, _ := s.Frame(); f.Cols != 91 || f.Rows != 33 {
		t.Fatalf("the frame is %dx%d", f.Cols, f.Rows)
	}
}

func TestOutputPastTheScrollback(t *testing.T) {
	s := shell(t, 80, 24, Options{Scrollback: 200})
	const lines = 20000
	s.Write([]byte(fmt.Sprintf("i=1; while [ $i -le %d ]; do echo line$i; i=$((i+1)); done; echo done\n", lines)))
	waitFor(t, "the burst", func() bool { return strings.Contains(screenText(t, s), "\ndone") })
	text := s.Text()
	if !strings.Contains(text, fmt.Sprintf("line%d\n", lines)) {
		t.Fatal("the last line is missing")
	}
	if strings.Contains(text, "line1\n") || strings.Contains(text, "line100\n") {
		t.Fatal("lines past the scrollback were kept")
	}
	if n := strings.Count(text, "\n"); n > 2000 {
		t.Fatalf("%d lines kept for a scrollback of 200", n)
	}
	// Scrolled up, the view shows what went before; typing brings it back.
	s.Wheel(50)
	if strings.Contains(screenText(t, s), "\ndone") {
		t.Fatal("the wheel did not scroll the view")
	}
	s.Key(input.KeyEvent{Action: input.KeyActionPress, Key: input.KeyUnidentified}, "x")
	if !strings.Contains(screenText(t, s), "\ndone") {
		t.Fatal("typing left the view scrolled up")
	}
}

// Closing a session ends its program and everything it started, though
// they ignore the hang-up.
func TestCloseEndsTheProgramAndItsSession(t *testing.T) {
	s := shell(t, 80, 24, Options{}, "-c", "trap '' HUP; sleep 600 & sleep 601 & echo started; wait")
	waitFor(t, "the children", func() bool { return strings.Contains(screenText(t, s), "started") })
	leader := s.prog.(*proc).cmd.Process.Pid
	if n := len(swept(leader)); n < 2 {
		t.Fatalf("%d processes in the session, want the two sleeps", n)
	}
	start := time.Now()
	if err := s.Close(); err != nil {
		t.Logf("close: %v", err)
	}
	if d := time.Since(start); d > 5*time.Second {
		t.Fatalf("closing took %v", d)
	}
	if err := unix.Kill(leader, 0); err == nil {
		t.Fatalf("the leader %d is still there", leader)
	}
	waitFor(t, "the session's processes to end", func() bool { return len(swept(leader)) == 0 })
	// Closed, it says nothing and takes nothing.
	if _, err := s.Frame(); err == nil {
		t.Fatal("a closed session made a frame")
	}
	s.Key(input.KeyEvent{Action: input.KeyActionPress, Key: input.KeyEnter}, "")
	s.Paste("x")
	s.Resize(10, 10, 1, 1)
	s.Close()
}

// The terminal answers the program's own queries: a cursor position
// report reaches the program as typed.
func TestTheTerminalAnswersQueries(t *testing.T) {
	s := shell(t, 80, 24, Options{})
	s.Write([]byte("stty raw -echo; printf '\\033[6n'; dd bs=1 count=6 2>/dev/null | od -c | head -1; stty sane\n"))
	waitFor(t, "the report", func() bool { return strings.Contains(screenText(t, s), "033   [") })
}

func TestKeysAndPastesFollowTheModes(t *testing.T) {
	s := shell(t, 80, 24, Options{})
	// cat -v shows what it reads: the arrows as the modes encode them, and
	// a paste bracketed once the program asks for it.
	s.Write([]byte("printf '\\033[?2004h\\033[?1h'; stty -icanon -echo; cat -v\n"))
	time.Sleep(300 * time.Millisecond)
	s.Key(input.KeyEvent{Action: input.KeyActionPress, Key: input.KeyArrowUp}, "")
	s.Paste("pasted")
	// cat writes a line once it ends.
	s.Key(input.KeyEvent{Action: input.KeyActionPress, Key: input.KeyEnter}, "")
	deadline := time.Now().Add(5 * time.Second)
	for !strings.Contains(screenText(t, s), "^[OA^[[200~pasted^[[201~") {
		if time.Now().After(deadline) {
			t.Fatalf("cat read otherwise:\n%s", screenText(t, s))
		}
		time.Sleep(10 * time.Millisecond)
	}
	if s.NeedsKeys() {
		t.Fatal("a shell asked for keys as codes")
	}
}
