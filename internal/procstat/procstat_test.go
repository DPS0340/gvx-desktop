package procstat

import (
	"os"
	"os/exec"
	"testing"
	"time"
)

func TestParseStat(t *testing.T) {
	// A command whose name holds a space and a parenthesis, as a thread's
	// may: the fields count from its last ")".
	line := "41234 (rungv (x) run) S 1 41234 41234 0 -1 4194560 52713 0 0 0 1234 566 0 0 20 0 18 0 98765 3906523136 74000 18446744073709551615 1 1 0 0 0 0 0 0 0 0 0 0 17 3 0 0 0 0 0\n"
	s, err := parseStat(line, 4096)
	if err != nil {
		t.Fatal(err)
	}
	if s.CPU != 18*time.Second || s.Memory != 74000*4096 {
		t.Errorf("%+v", s)
	}
	for _, bad := range []string{"", "41234 (rungv", "41234 (rungv) S 1 2 3", "1 (x) S 1 1 1 0 -1 0 0 0 0 0 a b 0 0 20 0 1 0 1 1 c"} {
		if _, err := parseStat(bad, 4096); err == nil {
			t.Errorf("%q parsed", bad)
		}
	}
}

func TestParsePS(t *testing.T) {
	at := time.Date(2026, 11, 19, 6, 0, 0, 0, time.UTC)
	// macOS writes minutes and hundredths; Linux and the BSDs hours and
	// seconds, with days ahead past one.
	out := "53480 299184   0:01.11\n53481 754688 287:12.87\n  901  1024 01:02:03\n 902 2048 2-01:00:00\nnot a line\n903 x 0:01\n"
	got := parsePS(out, at)
	want := map[int]Sample{
		53480: {At: at, CPU: 1110 * time.Millisecond, Memory: 299184 << 10},
		53481: {At: at, CPU: 287*time.Minute + 12870*time.Millisecond, Memory: 754688 << 10},
		901:   {At: at, CPU: time.Hour + 2*time.Minute + 3*time.Second, Memory: 1 << 20},
		902:   {At: at, CPU: 49 * time.Hour, Memory: 2 << 20},
	}
	if len(got) != len(want) {
		t.Fatalf("%d samples: %+v", len(got), got)
	}
	for pid, w := range want {
		g := got[pid]
		// The hundredths go through a float.
		if d := g.CPU - w.CPU; d < -time.Microsecond || d > time.Microsecond || g.Memory != w.Memory || !g.At.Equal(w.At) {
			t.Errorf("%d: %+v, want %+v", pid, g, w)
		}
	}
	for _, bad := range []string{"", "12", "1:2:3:4", "a:01", "1:-2", "x-01:00:00"} {
		if d, err := parseCPUTime(bad); err == nil {
			t.Errorf("%q parsed as %v", bad, d)
		}
	}
}

func TestCPUPercent(t *testing.T) {
	at := time.Date(2026, 11, 19, 6, 0, 0, 0, time.UTC)
	prev := Sample{At: at, CPU: 10 * time.Second}
	cur := Sample{At: at.Add(2 * time.Second), CPU: 13 * time.Second}
	if p, ok := CPUPercent(prev, cur); !ok || p != 150 {
		t.Errorf("1.5 CPUs over two seconds: %v %v", p, ok)
	}
	if _, ok := CPUPercent(Sample{}, cur); ok {
		t.Error("no sample before")
	}
	if _, ok := CPUPercent(cur, prev); ok {
		t.Error("backwards")
	}
	if _, ok := CPUPercent(cur, Sample{At: cur.At.Add(time.Second), CPU: time.Second}); ok {
		t.Error("a process that started again counts from zero")
	}
}

// TestReadThisProcess samples the test's own process, where the system
// can be read.
func TestReadThisProcess(t *testing.T) {
	// A process that has ended is left out.
	gone := exec.Command(os.Args[0], "-test.run=^$")
	if err := gone.Run(); err != nil {
		t.Fatal(err)
	}
	got, err := Read([]int{os.Getpid(), gone.Process.Pid})
	if err == ErrUnsupported {
		t.Skip(err)
	}
	if err != nil {
		t.Fatal(err)
	}
	s, ok := got[os.Getpid()]
	if !ok || s.Memory == 0 || s.At.IsZero() || len(got) != 1 {
		t.Errorf("%+v", got)
	}
	if none, err := Read(nil); err != nil || len(none) != 0 {
		t.Errorf("no pids: %v %v", none, err)
	}
}
