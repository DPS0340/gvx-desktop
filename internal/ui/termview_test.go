package ui

import (
	"bytes"
	"io"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ironpark/ggui"
	uitheme "github.com/ironpark/ggui/ui/theme"
	"github.com/ironpark/ggui/widget"

	"github.com/ironpark/gvx-desktop/internal/term"
)

// fakeProgram stands in for a shell: what it is to print is pushed into
// out, and what is typed into it is recorded.
type fakeProgram struct {
	out    chan string
	closed chan struct{}
	once   sync.Once

	mu      sync.Mutex
	typed   bytes.Buffer
	resizes [][2]uint16
}

func newFakeProgram() *fakeProgram {
	return &fakeProgram{out: make(chan string, 16), closed: make(chan struct{})}
}

func (f *fakeProgram) Read(p []byte) (int, error) {
	select {
	case s := <-f.out:
		return copy(p, s), nil
	case <-f.closed:
		return 0, io.EOF
	}
}

func (f *fakeProgram) Write(p []byte) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.typed.Write(p)
}

func (f *fakeProgram) Resize(cols, rows, _, _ uint16) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.resizes = append(f.resizes, [2]uint16{cols, rows})
	return nil
}

func (f *fakeProgram) Wait() int               { <-f.closed; return 0 }
func (f *fakeProgram) EndOutput(time.Duration) {}
func (f *fakeProgram) Kill()                   { f.Close() }
func (f *fakeProgram) Close() error            { f.once.Do(func() { close(f.closed) }); return nil }

// take is what was typed since the last take.
func (f *fakeProgram) take() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	s := f.typed.String()
	f.typed.Reset()
	return s
}

// newTestTerminal is a terminal view over a fake shell, focused, under a
// focusable box that Tab must not reach.
func newTestTerminal(t *testing.T, dark bool) (*fakeProgram, *term.Session, *ggui.Probe) {
	t.Helper()
	prog := newFakeProgram()
	s, err := term.New(prog, term.Options{Cols: 80, Rows: 24})
	if err != nil {
		t.Fatal(err)
	}
	preset := uitheme.Preset{Base: uitheme.BaseZinc, Accent: uitheme.AccentBlue}
	p := ggui.NewProbe(widget.Column(
		widget.Focus(widget.Box().Size(800, 40)),
		widget.Expanded(newTerminalView(s, false)),
	).Stretch(), ggui.Sz(800, 460))
	p.Setup(func() { uitheme.Bind(ggui.Const(dark), preset.Dark(), preset.Light()) })
	t.Cleanup(func() {
		p.Close()
		s.Close()
	})
	p.Click(ggui.Pt(400, 300))
	return prog, s, p
}

// output has the fake shell print s, and waits for the terminal to show
// what it ends in.
func output(t *testing.T, prog *fakeProgram, s *term.Session, out, shows string) {
	t.Helper()
	prog.out <- out
	deadline := time.Now().Add(5 * time.Second)
	for !strings.Contains(s.Text(), shows) {
		if time.Now().After(deadline) {
			t.Fatalf("the terminal never showed %q:\n%s", shows, s.Text())
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestTypingReachesTheShell(t *testing.T) {
	prog, _, p := newTestTerminal(t, false)

	p.Text("ls")
	p.Type(ggui.Mods{}, ggui.KeyTab)
	p.Type(ggui.Mods{}, ggui.KeyEnter)
	if got := prog.take(); got != "ls\t\r" {
		t.Fatalf("typed %q, want %q", got, "ls\t\r")
	}
	if !p.Focused() {
		t.Fatal("Tab moved focus off the terminal")
	}

	// A printable key is sent once, as its text; the KeyPress that comes
	// with it is not described again.
	p.Type(ggui.Mods{}, ggui.KeyA)
	p.Text("a")
	if got := prog.take(); got != "a" {
		t.Fatalf("typed %q, want %q", got, "a")
	}

	for _, k := range []struct {
		name string
		mods ggui.Mods
		key  ggui.KeyboardKey
		want string
	}{
		{"Ctrl+C", ggui.Mods{Ctrl: true}, ggui.KeyC, "\x03"},
		{"Ctrl+D", ggui.Mods{Ctrl: true}, ggui.KeyD, "\x04"},
		{"Up", ggui.Mods{}, ggui.KeyArrowUp, "\x1b[A"},
		{"Left", ggui.Mods{}, ggui.KeyArrowLeft, "\x1b[D"},
		{"Escape", ggui.Mods{}, ggui.KeyEscape, "\x1b"},
		{"Backspace", ggui.Mods{}, ggui.KeyBackspace, "\x7f"},
		{"F5", ggui.Mods{}, ggui.KeyF5, "\x1b[15~"},
		{"Home", ggui.Mods{}, ggui.KeyHome, "\x1b[H"},
		{"Delete", ggui.Mods{}, ggui.KeyDelete, "\x1b[3~"},
		{"Shift+Tab", ggui.Mods{Shift: true}, ggui.KeyTab, "\x1b[Z"},
	} {
		p.Type(k.mods, k.key)
		if got := prog.take(); got != k.want {
			t.Errorf("%s sent %q, want %q", k.name, got, k.want)
		}
	}

	// A word edit goes as readline reads it: ⌥← on a Mac, Ctrl+← elsewhere.
	word := ggui.Mods{Ctrl: true}
	if runtime.GOOS == "darwin" {
		word = ggui.Mods{Alt: true}
	}
	p.Type(word, ggui.KeyArrowLeft)
	if got := prog.take(); got != "\x1bb" {
		t.Errorf("a word left sent %q", got)
	}

	// Where the IME may rewrite the last syllable, it is held until the
	// next key, which sends it first.
	p.Text("안녕")
	want, held := "안녕", ""
	if holdsSyllables {
		want, held = "안", "녕"
	}
	if got := prog.take(); got != want {
		t.Fatalf("Hangul sent %q, want %q", got, want)
	}
	p.Type(ggui.Mods{}, ggui.KeyEnter)
	if got := prog.take(); got != held+"\r" {
		t.Fatalf("Enter after Hangul sent %q", got)
	}
}

// The encoder follows the modes the program set: application cursor keys
// change what the arrows send, and a paste is bracketed once asked for.
func TestKeysAndPastesFollowTheShellsModes(t *testing.T) {
	prog, s, p := newTestTerminal(t, false)
	paste := ggui.Mods{Meta: true}
	if runtime.GOOS != "darwin" {
		paste = ggui.Mods{Ctrl: true, Shift: true}
	}
	// Inside a frame the clipboard is the probe's own, not the system's.
	p.Post(func() { ggui.CurrentClipboard().Write("echo hi") })
	p.Frame()
	p.Type(paste, ggui.KeyV)
	if got := prog.take(); got != "echo hi" {
		t.Fatalf("a paste sent %q", got)
	}

	output(t, prog, s, "\x1b[?1h\x1b[?2004hready", "ready")
	p.Type(ggui.Mods{}, ggui.KeyArrowUp)
	if got := prog.take(); got != "\x1bOA" {
		t.Fatalf("Up in application mode sent %q", got)
	}
	p.Type(paste, ggui.KeyV)
	if got := prog.take(); got != "\x1b[200~echo hi\x1b[201~" {
		t.Fatalf("a bracketed paste sent %q", got)
	}
}

// Selecting with the pointer and copying puts the text on the clipboard.
func TestSelectingCopiesText(t *testing.T) {
	prog, s, p := newTestTerminal(t, false)
	output(t, prog, s, "hello world\r\n", "hello world")
	p.Frame()
	// Twice on a word selects it.
	at := ggui.Pt(14, 40+6+8) // the first row's first cells, under the box above
	p.Click(at)
	p.Click(at)
	if got := s.Selection(); got != "hello" {
		t.Fatalf("selected %q, want hello", got)
	}
	copyMods := ggui.Mods{Meta: true}
	if runtime.GOOS != "darwin" {
		copyMods = ggui.Mods{Ctrl: true, Shift: true}
	}
	p.Type(copyMods, ggui.KeyC)
	var got string
	p.Post(func() { got = ggui.CurrentClipboard().Read() })
	p.Frame()
	if got != "hello" {
		t.Fatalf("copied %q", got)
	}
	if typed := prog.take(); typed != "" {
		t.Fatalf("copying typed %q", typed)
	}
}

// The view sizes the terminal to fit it.
func TestTheTerminalFollowsTheViewsSize(t *testing.T) {
	prog, s, p := newTestTerminal(t, false)
	p.Frame()
	cols, rows := s.Size()
	if cols < 50 || rows < 15 {
		t.Fatalf("the terminal is %dx%d in an 800x420 view", cols, rows)
	}
	time.Sleep(200 * time.Millisecond)
	p.Resize(ggui.Sz(500, 300))
	p.Frame()
	time.Sleep(200 * time.Millisecond)
	p.Frame()
	if c, r := s.Size(); c >= cols || r >= rows {
		t.Fatalf("shrunk, the terminal is still %dx%d", c, r)
	}
	prog.mu.Lock()
	defer prog.mu.Unlock()
	if len(prog.resizes) == 0 {
		t.Fatal("the shell was told of no size")
	}
}

// A terminal showing styled text of every kind paints, in either theme.
// render.go draws one to be looked at (-render-dir).
func TestPaintingATerminal(t *testing.T) {
	for _, dark := range []bool{false, true} {
		prog, s, p := newTestTerminal(t, dark)
		output(t, prog, s, "$ ls -l\r\n"+
			"\x1b[1;34mbin\x1b[0m  \x1b[1;32mrun.sh\x1b[0m  notes.txt  \x1b[31mred\x1b[0m \x1b[33myellow\x1b[0m \x1b[7minverse\x1b[0m \x1b[4munder\x1b[0m\r\n"+
			"\x1b[48;5;236m\x1b[38;2;255;128;0m 24-bit orange on 256-colour grey \x1b[0m 한글 wide ┌─┐\r\n"+
			"$ ", "wide")
		for range 3 {
			p.Frame()
		}
		f, err := s.Frame()
		if err != nil {
			t.Fatal(err)
		}
		// The ANSI blue is the theme's, as the view told the terminal.
		blue := f.Cells[f.Cols].Fg
		want := lightANSI[12] // bold brightens blue
		if dark {
			want = darkANSI[12]
		}
		if got := uint32(blue.R())<<16 | uint32(blue.G())<<8 | uint32(blue.B()); got != want && got != lightANSI[4] && got != darkANSI[4] {
			t.Errorf("dark %v: blue is %06x", dark, got)
		}
	}
}
