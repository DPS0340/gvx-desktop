package term

import (
	"strings"
	"testing"
	"time"
)

// waitFor polls cond until it holds, failing the test after a while.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func screenText(t *testing.T, s *Session) string {
	t.Helper()
	f, err := s.Frame()
	if err != nil {
		t.Fatal(err)
	}
	var b strings.Builder
	for row := 0; row < f.Rows; row++ {
		for col := 0; col < f.Cols; col++ {
			c := f.Cells[row*f.Cols+col]
			if c.Codepoint == 0 {
				b.WriteByte(' ')
			} else {
				b.WriteString(f.TextAt(row*f.Cols + col))
			}
		}
		b.WriteByte('\n')
	}
	return b.String()
}
