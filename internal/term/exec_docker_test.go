package term

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/ironpark/gvx-desktop/internal/dockerapi"
)

// TestADockerExec runs a shell in a running container through the
// engine's API, as the Containers page's terminal does. It needs an engine
// and a container in it: GVX_TEST_DOCKER_HOST names the engine as
// DOCKER_HOST does, GVX_TEST_CONTAINER the container.
func TestADockerExec(t *testing.T) {
	host, ctr := os.Getenv("GVX_TEST_DOCKER_HOST"), os.Getenv("GVX_TEST_CONTAINER")
	if host == "" || ctr == "" {
		t.Skip("GVX_TEST_DOCKER_HOST and GVX_TEST_CONTAINER name no container to run a shell in")
	}
	c, err := dockerapi.New(host)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	s := dockerShell(t, ctx, c, ctr)
	defer s.Close()

	s.Write([]byte("echo $((6*7)); stty size; echo $TERM-$0\r"))
	waitFor(t, "the answer", func() bool { return strings.Contains(screenText(t, s), "\n42") })
	waitFor(t, "the size and TERM", func() bool {
		text := screenText(t, s)
		return strings.Contains(text, "\n24 80") && strings.Contains(text, "\nxterm-256color-")
	})
	time.Sleep(2 * resizeSettle)
	s.Resize(100, 30, 8, 16)
	s.Write([]byte("stty size\r"))
	waitFor(t, "the new size", func() bool { return strings.Contains(screenText(t, s), "30 100") })

	s.Write([]byte("exit 7\r"))
	select {
	case <-s.Done():
	case <-time.After(10 * time.Second):
		t.Fatal("the shell did not exit")
	}
	if _, code := s.Exited(); code != 7 {
		t.Fatalf("exited with %d, want 7", code)
	}
}

// TestClosingADockerExecEndsItsShell closes the stream of a shell that
// runs a command, and finds neither left in the container.
func TestClosingADockerExecEndsItsShell(t *testing.T) {
	host, ctr := os.Getenv("GVX_TEST_DOCKER_HOST"), os.Getenv("GVX_TEST_CONTAINER")
	if host == "" || ctr == "" {
		t.Skip("GVX_TEST_DOCKER_HOST and GVX_TEST_CONTAINER name no container to run a shell in")
	}
	c, err := dockerapi.New(host)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	s := dockerShell(t, ctx, c, ctr)
	s.Write([]byte("sleep 4242 & sleep 4243\r"))
	time.Sleep(500 * time.Millisecond)
	s.Close()

	// What runs in the container, asked with a second exec.
	ps := func() string {
		e, err := c.Exec(ctx, ctr, []string{"ps", "-o", "pid,args"}, nil, 200, 50)
		if err != nil {
			t.Fatal(err)
		}
		p, err := New(e, Options{Cols: 200, Rows: 50})
		if err != nil {
			t.Fatal(err)
		}
		defer p.Close()
		<-p.Done()
		return p.Text()
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		out := ps()
		if !strings.Contains(out, "sleep 424") && !strings.Contains(out, " sh\n") && !strings.Contains(out, "bash") {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("the shell's command outlived its stream:\n%s", out)
		}
		time.Sleep(500 * time.Millisecond)
	}
}

// dockerShell starts a shell in ctr as the Containers page does: marked,
// and swept when it is closed.
func dockerShell(t *testing.T, ctx context.Context, c *dockerapi.Client, ctr string) *Session {
	t.Helper()
	mark := NewMark()
	exec, err := c.Exec(ctx, ctr, ShellCommand(mark), nil, 80, 24)
	if err != nil {
		t.Fatal(err)
	}
	prog := Swept(exec, func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := c.ExecDetached(ctx, ctr, SweepCommand(mark)); err != nil {
			t.Logf("sweep: %v", err)
		}
	})
	s, err := New(prog, Options{Cols: 80, Rows: 24})
	if err != nil {
		t.Fatal(err)
	}
	return s
}
