package sandbox

import (
	"sync"
	"time"
)

// Stream says which output of the process a log line came from.
type Stream uint8

const (
	Stdout Stream = iota
	Stderr
	System // written by the desktop app itself
)

// LogLine is one line a sandbox wrote.
type LogLine struct {
	Seq    int64
	Time   time.Time
	Stream Stream
	Text   string
}

// LogBuffer keeps the last lines of a sandbox's output.
type LogBuffer struct {
	mu    sync.Mutex
	max   int
	lines []LogLine
	seq   int64
}

// NewLogBuffer keeps at most max lines.
func NewLogBuffer(max int) *LogBuffer {
	if max <= 0 {
		max = 5000
	}
	return &LogBuffer{max: max}
}

// Add appends a line and returns its sequence number.
func (b *LogBuffer) Add(stream Stream, text string) int64 {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.seq++
	b.lines = append(b.lines, LogLine{Seq: b.seq, Time: time.Now(), Stream: stream, Text: text})
	if over := len(b.lines) - b.max; over > 0 {
		// Drop a chunk at once so appends stay amortized O(1).
		drop := max(over, b.max/10)
		b.lines = append(b.lines[:0:0], b.lines[drop:]...)
	}
	return b.seq
}

// Seq is the sequence number of the last line, 0 when empty.
func (b *LogBuffer) Seq() int64 {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.seq
}

// Snapshot copies the lines.
func (b *LogBuffer) Snapshot() []LogLine {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]LogLine(nil), b.lines...)
}

// Clear removes every line.
func (b *LogBuffer) Clear() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.lines = nil
}

// Tail returns the last n lines of stream (any stream when stream is 255).
func (b *LogBuffer) Tail(n int, stream Stream) []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	var out []string
	for i := len(b.lines) - 1; i >= 0 && len(out) < n; i-- {
		if stream == 255 || b.lines[i].Stream == stream {
			out = append(out, b.lines[i].Text)
		}
	}
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	return out
}
