package term

import (
	"io"
	"sync"
	"testing"
	"time"
)

type blockingResizeProgram struct {
	closed     chan struct{}
	once       sync.Once
	resizeGate chan struct{}
	resizeHit  chan struct{}

	mu      sync.Mutex
	resizes [][2]uint16
}

func (p *blockingResizeProgram) Read([]byte) (int, error) {
	<-p.closed
	return 0, io.EOF
}

func (p *blockingResizeProgram) Write(b []byte) (int, error) { return len(b), nil }

func (p *blockingResizeProgram) Resize(cols, rows, _, _ uint16) error {
	select {
	case p.resizeHit <- struct{}{}:
	default:
	}
	select {
	case <-p.resizeGate:
	case <-p.closed:
		return nil
	}
	p.mu.Lock()
	p.resizes = append(p.resizes, [2]uint16{cols, rows})
	p.mu.Unlock()
	return nil
}

func (p *blockingResizeProgram) Wait() int               { <-p.closed; return 0 }
func (p *blockingResizeProgram) EndOutput(time.Duration) {}
func (p *blockingResizeProgram) Kill()                   { p.Close() }
func (p *blockingResizeProgram) Close() error {
	p.once.Do(func() { close(p.closed) })
	return nil
}

func TestResizeIsNonBlockingAndCoalesces(t *testing.T) {
	prog := &blockingResizeProgram{
		closed:     make(chan struct{}),
		resizeGate: make(chan struct{}),
		resizeHit:  make(chan struct{}, 1),
	}
	s, err := New(prog, Options{Cols: 80, Rows: 24})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	// Resize must return immediately even while prog.Resize is blocked.
	done := make(chan struct{})
	go func() {
		s.Resize(100, 30, 8, 16)
		s.Resize(110, 35, 8, 16)
		s.Resize(120, 40, 8, 16)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(500 * time.Millisecond):
		t.Fatal("Session.Resize blocked caller while Program.Resize was slow")
	}

	// Wait for the async worker to enter prog.Resize, then unblock it.
	select {
	case <-prog.resizeHit:
	case <-time.After(2 * time.Second):
		t.Fatal("async resize worker never called Program.Resize")
	}
	close(prog.resizeGate)

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		prog.mu.Lock()
		n := len(prog.resizes)
		var last [2]uint16
		if n > 0 {
			last = prog.resizes[n-1]
		}
		prog.mu.Unlock()
		if last == [2]uint16{120, 40} {
			return
		}
		time.Sleep(15 * time.Millisecond)
	}
	prog.mu.Lock()
	defer prog.mu.Unlock()
	t.Fatalf("final resize = %v, want last=[120 40]", prog.resizes)
}
