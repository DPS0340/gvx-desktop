package dockerapi

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"sync"
	"time"
)

// An Exec is a command run in a container on a terminal of the engine's,
// as docker exec -it runs one: reading it reads what the command writes
// to its terminal, and writing it types. The engine's API carries it on a
// connection it takes over, so no docker command is needed.
//
// It is a term.Program.
type Exec struct {
	c    *Client
	id   string
	conn net.Conn
	r    io.Reader // what the answer to the start left buffered, then conn

	ended     chan struct{} // closed once the stream ends
	endOnce   sync.Once
	closeOnce sync.Once
}

// Exec starts cmd in the container id on a terminal cols by rows, with env
// added to the container's environment.
func (c *Client) Exec(ctx context.Context, id string, cmd, env []string, cols, rows uint16) (*Exec, error) {
	size := [2]uint16{rows, cols}
	var created struct {
		ID string `json:"Id"`
	}
	err := c.postJSON(ctx, "/containers/"+url.PathEscape(id)+"/exec", map[string]any{
		"AttachStdin": true, "AttachStdout": true, "AttachStderr": true,
		"Tty": true, "Cmd": cmd, "Env": env, "ConsoleSize": size,
	}, &created)
	if err != nil {
		return nil, err
	}
	if created.ID == "" {
		return nil, errors.New("the engine made no exec")
	}

	// The start is answered by taking the connection over: what follows the
	// answer is the terminal's stream, both ways, raw as a terminal's is.
	conn, err := c.dial(ctx)
	if err != nil {
		return nil, err
	}
	body, _ := json.Marshal(map[string]any{"Detach": false, "Tty": true, "ConsoleSize": size})
	req, err := http.NewRequest(http.MethodPost, c.base+"/exec/"+url.PathEscape(created.ID)+"/start", bytes.NewReader(body))
	if err != nil {
		conn.Close()
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Connection", "Upgrade")
	req.Header.Set("Upgrade", "tcp")
	if dl, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(dl)
	}
	if err := req.Write(conn); err != nil {
		conn.Close()
		return nil, err
	}
	br := bufio.NewReader(conn)
	res, err := http.ReadResponse(br, req)
	if err != nil {
		conn.Close()
		return nil, err
	}
	if res.StatusCode != http.StatusSwitchingProtocols && res.StatusCode != http.StatusOK {
		defer conn.Close()
		msg, _ := io.ReadAll(io.LimitReader(res.Body, 64*1024))
		var e apiError
		if json.Unmarshal(msg, &e) == nil && e.Message != "" {
			return nil, errors.New(e.Message)
		}
		return nil, fmt.Errorf("starting the exec: %s", res.Status)
	}
	_ = conn.SetDeadline(time.Time{})
	return &Exec{c: c, id: created.ID, conn: conn, r: br, ended: make(chan struct{})}, nil
}

// ExecDetached starts cmd in the container id and leaves it running there,
// its output unread.
func (c *Client) ExecDetached(ctx context.Context, id string, cmd []string) error {
	var created struct {
		ID string `json:"Id"`
	}
	if err := c.postJSON(ctx, "/containers/"+url.PathEscape(id)+"/exec", map[string]any{"Cmd": cmd}, &created); err != nil {
		return err
	}
	return c.postJSON(ctx, "/exec/"+url.PathEscape(created.ID)+"/start", map[string]any{"Detach": true}, nil)
}

// postJSON posts in as JSON and decodes the answer into out.
func (c *Client) postJSON(ctx context.Context, path string, in, out any) error {
	body, err := json.Marshal(in)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.base+path, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	res, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	if res.StatusCode >= 300 {
		var e apiError
		msg, _ := io.ReadAll(io.LimitReader(res.Body, 64*1024))
		if json.Unmarshal(msg, &e) == nil && e.Message != "" {
			return errors.New(e.Message)
		}
		return fmt.Errorf("POST %s: %s", path, res.Status)
	}
	if out == nil {
		return nil
	}
	return json.NewDecoder(res.Body).Decode(out)
}

func (e *Exec) Read(p []byte) (int, error) {
	n, err := e.r.Read(p)
	if err != nil {
		e.endOnce.Do(func() { close(e.ended) })
	}
	return n, err
}

func (e *Exec) Write(p []byte) (int, error) { return e.conn.Write(p) }

// Resize sizes the exec's terminal cols by rows; the engine takes no pixel
// size.
func (e *Exec) Resize(cols, rows, _, _ uint16) error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	q := url.Values{"h": {strconv.Itoa(int(rows))}, "w": {strconv.Itoa(int(cols))}}
	return e.c.do(ctx, http.MethodPost, "/exec/"+url.PathEscape(e.id)+"/resize?"+q.Encode(), nil)
}

// Wait waits for the stream to end and is the command's exit code, as the
// engine tells it, or -1 when it does not: the stream was closed before
// the command ended, or the engine did not answer.
func (e *Exec) Wait() int {
	<-e.ended
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	// The stream ends a moment before the engine notes the exit.
	for {
		var st struct {
			Running  bool
			ExitCode int
		}
		if err := e.c.do(ctx, http.MethodGet, "/exec/"+url.PathEscape(e.id)+"/json", &st); err != nil {
			return -1
		}
		if !st.Running {
			return st.ExitCode
		}
		select {
		case <-ctx.Done():
			return -1
		case <-time.After(50 * time.Millisecond):
		}
	}
}

// EndOutput does nothing: the stream ends with the command.
func (e *Exec) EndOutput(time.Duration) {}

// Kill ends the command by closing its stream: the engine has no call to
// signal an exec, and closing its input hangs its terminal up, which ends
// a shell and what it runs in the foreground.
func (e *Exec) Kill() { _ = e.Close() }

// Close closes the stream.
func (e *Exec) Close() error {
	var err error
	e.closeOnce.Do(func() {
		err = e.conn.Close()
		e.endOnce.Do(func() { close(e.ended) })
	})
	return err
}
