package sandbox

import (
	"bytes"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/ironpark/gvx-desktop/internal/rungv"
)

func TestLineSplitter(t *testing.T) {
	var ls lineSplitter
	var got []string
	emit := func(s string) { got = append(got, s) }

	ls.write([]byte("hel"), emit)
	ls.write([]byte("lo\r\nworld\r\r\n\nlast"), emit)
	ls.flush(emit)

	want := []string{"hello", "world", "", "last"}
	if len(got) != len(want) {
		t.Fatalf("got %q, want %q", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("line %d: got %q, want %q", i, got[i], want[i])
		}
	}
}

func TestLineSplitterCap(t *testing.T) {
	var ls lineSplitter
	var got []string
	emit := func(s string) { got = append(got, s) }

	huge := bytes.Repeat([]byte("a"), maxLogLineBytes+1000)
	ls.write(huge, emit)
	ls.write([]byte("\nafter\n"), emit)
	ls.flush(emit)

	if len(got) != 2 {
		t.Fatalf("got %d lines, want 2", len(got))
	}
	if len(got[0]) != maxLogLineBytes {
		t.Errorf("line 0 len = %d, want %d", len(got[0]), maxLogLineBytes)
	}
	if got[1] != "after" {
		t.Errorf("line 1 = %q, want %q", got[1], "after")
	}
}

func TestRungvFilter(t *testing.T) {
	cases := []struct {
		name   string
		chunks []string
		want   string
	}{
		{
			name:   "plain guest stderr without delay",
			chunks: []string{"boom\n", "more"},
			want:   "boom\nmore",
		},
		{
			name:   "drops rungv and gvx prefixes",
			chunks: []string{"rungv: platform=hvf\ngvx: warning: oops\nreal error\n"},
			want:   "real error\n",
		},
		{
			name:   "drops rungvx prefixes",
			chunks: []string{"rungvx: platform=hvf\nrungvx: auto-publish enabled for guest listeners on 127.0.0.1\nreal error\n"},
			want:   "real error\n",
		},
		{
			name:   "keeps rungvx run fatal errors",
			chunks: []string{"rungvx run: gvx: executable /nope not found\n"},
			want:   "rungvx run: gvx: executable /nope not found\n",
		},
		{
			name:   "chunk split mid rungvx prefix",
			chunks: []string{"rungv", "x: publishing 127.0.0.1:80 -> 80/tcp\n", "rungvxy\n"},
			want:   "rungvxy\n",
		},
		{
			name:   "keeps rungv run fatal errors",
			chunks: []string{"rungv run: gvx: executable /nope not found\n"},
			want:   "rungv run: gvx: executable /nope not found\n",
		},
		{
			name:   "chunk split mid prefix dropped",
			chunks: []string{"run", "gv: publishing 127.0.0.1:80 -> 80/tcp\n", "ok\n"},
			want:   "ok\n",
		},
		{
			name:   "chunk split mid prefix kept",
			chunks: []string{"g", "o error\n", "rung", "x\n"},
			want:   "go error\nrungx\n",
		},
		{
			name:   "partial prefix flushed at EOF",
			chunks: []string{"gvx"},
			want:   "gvx",
		},
		{
			name:   "short line breaking prefix then dropped line",
			chunks: []string{"g\ngvx: warning: hi\n\nrungv: x\nend\n"},
			want:   "g\n\nend\n",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var f rungvFilter
			var out bytes.Buffer
			for _, c := range tc.chunks {
				out.Write(f.write([]byte(c)))
			}
			out.Write(f.flush())
			if out.String() != tc.want {
				t.Fatalf("got %q, want %q", out.String(), tc.want)
			}
		})
	}
}

func TestSupervisorStdioInteractiveAndFilter(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uses a POSIX shell script as stand-in rungv")
	}
	// rungv writes its own lines as rungvx since gvx v0.23.
	for _, prog := range []string{"rungv", "rungvx"} {
		t.Run(prog, func(t *testing.T) { testSupervisorStdioInteractiveAndFilter(t, prog) })
	}
}

func testSupervisorStdioInteractiveAndFilter(t *testing.T, prog string) {
	exe := filepath.Join(t.TempDir(), "rungv")
	script := "#!/bin/sh\n" +
		"echo '" + prog + ": platform=hvf' >&2\n" +
		"echo 'gvx: warning: test' >&2\n" +
		"echo 'guest-err' >&2\n" +
		"cat\n"
	if err := os.WriteFile(exe, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}

	var mu sync.Mutex
	var outBuf, errBuf bytes.Buffer
	s, _ := newTestSupervisor(t, &rungv.Client{Path: exe})
	s.Output = func(id string, o Output) {
		mu.Lock()
		defer mu.Unlock()
		if o.Stream == Stdout {
			outBuf.Write(o.Data)
		} else {
			errBuf.Write(o.Data)
		}
	}

	spec := Spec{ID: "i1", Name: "cat", Image: "alpine", Interactive: true}
	if err := s.Start(spec); err != nil {
		t.Fatal(err)
	}
	w := s.Stdin(spec.ID)
	if w == nil {
		t.Fatal("Stdin returned nil for Interactive spec")
	}
	if _, err := w.Write([]byte("hello stdin\n")); err != nil {
		t.Fatal(err)
	}
	if err := s.CloseStdin(spec.ID); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "cat exit", 10*time.Second, func() bool { return s.State(spec.ID).Status == Exited })

	mu.Lock()
	gotOut := outBuf.String()
	gotErr := errBuf.String()
	mu.Unlock()
	if gotOut != "hello stdin\n" {
		t.Errorf("stdout Output = %q, want %q", gotOut, "hello stdin\n")
	}
	if gotErr != "guest-err\n" {
		t.Errorf("stderr Output = %q, want %q", gotErr, "guest-err\n")
	}
	if st := s.State(spec.ID); st.Platform != "hvf" {
		t.Errorf("Platform = %q, want hvf", st.Platform)
	}
}

func TestSupervisorPTYResizeAndSignal(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("PTY is Unix-only")
	}
	exe := filepath.Join(t.TempDir(), "rungv")
	// Print a prompt without a newline, then read lines and respond to "size"
	// with stty size, or exit with a code.
	script := "#!/bin/sh\n" +
		"printf 'PROMPT> '\n" +
		"while IFS= read -r line; do\n" +
		"  case \"$line\" in\n" +
		"    size) stty size ;;\n" +
		"    quit) exit 3 ;;\n" +
		"  esac\n" +
		"done\n"
	if err := os.WriteFile(exe, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}

	var mu sync.Mutex
	var outBuf bytes.Buffer
	s, _ := newTestSupervisor(t, &rungv.Client{Path: exe})
	s.Output = func(id string, o Output) {
		mu.Lock()
		if o.Stream == Stdout {
			outBuf.Write(o.Data)
		}
		mu.Unlock()
	}

	spec := Spec{ID: "tty1", Name: "tty", Image: "alpine", TTY: true, Interactive: true}
	if err := s.Start(spec); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "prompt without newline", 5*time.Second, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return strings.Contains(outBuf.String(), "PROMPT> ")
	})
	if err := s.Resize(spec.ID, 33, 99); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Stdin(spec.ID).Write([]byte("size\n")); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "resized stty size", 5*time.Second, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return strings.Contains(outBuf.String(), "33 99")
	})
	if _, err := s.Stdin(spec.ID).Write([]byte("quit\n")); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "exit 3", 5*time.Second, func() bool { return !s.State(spec.ID).Status.Active() })
	if st := s.State(spec.ID); st.ExitCode != 3 {
		t.Fatalf("ExitCode = %d, want 3 (state=%+v)", st.ExitCode, st)
	}

	// Signal(SIGKILL) sets State.Signal == 9 and ExitCode == -1.
	sleepExe := filepath.Join(t.TempDir(), "rungv-sleep")
	if err := os.WriteFile(sleepExe, []byte("#!/bin/sh\nexec sleep 30\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	s2, _ := newTestSupervisor(t, &rungv.Client{Path: sleepExe})
	spec2 := Spec{ID: "sig1", Name: "sig", Image: "alpine"}
	if err := s2.Start(spec2); err != nil {
		t.Fatal(err)
	}
	if s2.Stdin(spec2.ID) != nil {
		t.Error("non-interactive spec returned non-nil Stdin")
	}
	if err := s2.Signal(spec2.ID, syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "killed", 5*time.Second, func() bool { return !s2.State(spec2.ID).Status.Active() })
	st := s2.State(spec2.ID)
	if st.Status != Failed || st.ExitCode != -1 || st.Signal != int(syscall.SIGKILL) {
		t.Fatalf("after SIGKILL: %+v", st)
	}
}
