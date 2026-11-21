package engine

import (
	"bufio"
	"context"
	"encoding/binary"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ironpark/gvx-desktop/internal/dockerapi"
	"github.com/ironpark/gvx-desktop/internal/sandbox"
)

// streamMockStore is a minimal container store injected via lookupOverride so
// workstream A's unit tests work independently of workstream B.
type streamMockStore struct {
	mu   sync.Mutex
	ctrs map[string]Container
}

func newStreamTestEngine(t *testing.T) (*testEngine, *streamMockStore) {
	t.Helper()
	te := newTestEngine(t)
	st := &streamMockStore{ctrs: map[string]Container{}}
	te.str.lookupOverride = st.lookup
	return te, st
}

func (s *streamMockStore) set(c Container) {
	s.mu.Lock()
	s.ctrs[c.ID] = c
	s.mu.Unlock()
}

func (s *streamMockStore) delete(id string) {
	s.mu.Lock()
	delete(s.ctrs, id)
	s.mu.Unlock()
}

func (s *streamMockStore) lookup(ref string) (Container, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if c, ok := s.ctrs[ref]; ok {
		return c, nil
	}
	name := strings.TrimPrefix(ref, "/")
	for _, c := range s.ctrs {
		if c.Name == name || strings.HasPrefix(c.ID, ref) {
			return c, nil
		}
	}
	return Container{}, errNotFound("No such container: %s", ref)
}

func streamDecodeMuxFrames(t *testing.T, raw []byte) (stdout, stderr string) {
	t.Helper()
	var out, errb strings.Builder
	for len(raw) > 0 {
		if len(raw) < 8 {
			t.Fatalf("truncated mux header: %d bytes left (%q)", len(raw), raw)
		}
		stream := raw[0]
		if raw[1] != 0 || raw[2] != 0 || raw[3] != 0 {
			t.Fatalf("bad mux header padding: %v", raw[:8])
		}
		n := int(binary.BigEndian.Uint32(raw[4:8]))
		raw = raw[8:]
		if len(raw) < n {
			t.Fatalf("truncated mux frame: want %d bytes, got %d", n, len(raw))
		}
		switch stream {
		case 1:
			out.Write(raw[:n])
		case 2:
			errb.Write(raw[:n])
		default:
			t.Fatalf("unexpected mux stream %d", stream)
		}
		raw = raw[n:]
	}
	return out.String(), errb.String()
}

func TestLogsMuxRawTailTimestampsAndPersistence(t *testing.T) {
	te, st := newStreamTestEngine(t)

	id := fakeHex("logs-mux")
	c := Container{
		ID:     id,
		Name:   "mux",
		Config: ContainerConfig{Tty: false},
		State:  ContainerState{Status: StatusExited},
	}
	st.set(c)

	// Neither stdout nor stderr -> 400.
	code, _ := te.do(t, http.MethodGet, "/containers/"+id+"/logs", nil)
	if code != http.StatusBadRequest {
		t.Fatalf("no stream status = %d, want 400", code)
	}
	// Missing container -> 404.
	code, _ = te.do(t, http.MethodGet, "/containers/nope/logs?stdout=1", nil)
	if code != http.StatusNotFound {
		t.Fatalf("missing container status = %d, want 404", code)
	}

	te.onRunnerOutput(id, sandbox.Output{Stream: sandbox.Stdout, Data: []byte("line1\nline2\n")})
	te.onRunnerOutput(id, sandbox.Output{Stream: sandbox.Stderr, Data: []byte("err1\n")})
	te.onRunnerOutput(id, sandbox.Output{Stream: sandbox.Stdout, Data: []byte("line3\n")})

	// Multiplexed framing, both streams.
	res, err := te.srv.Client().Get(te.srv.URL + "/containers/" + id + "/logs?stdout=1&stderr=1")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(res.Body)
	res.Body.Close()
	if got := res.Header.Get("Content-Type"); got != streamContentTypeMux {
		t.Fatalf("Content-Type = %q, want %q", got, streamContentTypeMux)
	}
	gotOut, gotErr := streamDecodeMuxFrames(t, body)
	if gotOut != "line1\nline2\nline3\n" || gotErr != "err1\n" {
		t.Fatalf("stdout=%q stderr=%q", gotOut, gotErr)
	}

	// Tail=2 counts lines across selected streams.
	code, body = te.do(t, http.MethodGet, "/containers/"+id+"/logs?stdout=1&stderr=1&tail=2", nil)
	if code != 200 {
		t.Fatalf("status %d", code)
	}
	gotOut, gotErr = streamDecodeMuxFrames(t, body)
	if gotOut != "line3\n" || gotErr != "err1\n" {
		t.Fatalf("tail=2 stdout=%q stderr=%q", gotOut, gotErr)
	}

	// Timestamps=1 prefixes each line with RFC3339Nano + space.
	code, body = te.do(t, http.MethodGet, "/containers/"+id+"/logs?stdout=1&timestamps=1", nil)
	if code != 200 {
		t.Fatalf("status %d", code)
	}
	gotOut, _ = streamDecodeMuxFrames(t, body)
	lines := strings.Split(strings.TrimSuffix(gotOut, "\n"), "\n")
	if len(lines) != 3 {
		t.Fatalf("timestamped lines = %q", lines)
	}
	for i, ln := range lines {
		ts, rest, ok := strings.Cut(ln, " ")
		if !ok {
			t.Fatalf("line %d missing space: %q", i, ln)
		}
		if _, err := time.Parse(time.RFC3339Nano, ts); err != nil {
			t.Fatalf("line %d bad timestamp %q: %v", i, ts, err)
		}
		if !strings.HasPrefix(rest, "line") {
			t.Fatalf("line %d text = %q", i, rest)
		}
	}

	// TTY container uses raw stream and merges stdout + stderr.
	ttyID := fakeHex("logs-tty")
	st.set(Container{ID: ttyID, Name: "tty", Config: ContainerConfig{Tty: true}, State: ContainerState{Status: StatusExited}})
	te.onRunnerOutput(ttyID, sandbox.Output{Stream: sandbox.Stdout, Data: []byte("/ # ")})
	te.onRunnerOutput(ttyID, sandbox.Output{Stream: sandbox.Stderr, Data: []byte("oops\r\n")})
	res, err = te.srv.Client().Get(te.srv.URL + "/containers/" + ttyID + "/logs?stdout=1")
	if err != nil {
		t.Fatal(err)
	}
	body, _ = io.ReadAll(res.Body)
	res.Body.Close()
	if got := res.Header.Get("Content-Type"); got != streamContentTypeRaw {
		t.Fatalf("tty Content-Type = %q, want %q", got, streamContentTypeRaw)
	}
	if string(body) != "/ # oops\r\n" {
		t.Fatalf("tty body = %q, want %q", body, "/ # oops\r\n")
	}

	// Logs persist across engine reopen.
	te.srv.Close()
	_ = te.Close(time.Second)
	te2 := reopenTestEngine(t, te.dir, newFakeRunner())
	te2.str.lookupOverride = st.lookup
	code, body = te2.do(t, http.MethodGet, "/containers/"+id+"/logs?stdout=1&stderr=1", nil)
	if code != 200 {
		t.Fatalf("after reopen status %d", code)
	}
	gotOut, gotErr = streamDecodeMuxFrames(t, body)
	if gotOut != "line1\nline2\nline3\n" || gotErr != "err1\n" {
		t.Fatalf("after reopen stdout=%q stderr=%q", gotOut, gotErr)
	}

	// dropLogs deletes logs/<id>.log.
	logPath := te2.logsFilePath(id)
	if _, err := os.Stat(logPath); err != nil {
		t.Fatalf("expected log file at %s: %v", logPath, err)
	}
	te2.dropLogs(id)
	if _, err := os.Stat(logPath); !os.IsNotExist(err) {
		t.Fatalf("log file still present after dropLogs: %v", err)
	}
}

func TestLogsFollowEndsOnExit(t *testing.T) {
	te, st := newStreamTestEngine(t)
	id := fakeHex("logs-follow")
	c := Container{ID: id, Name: "fl", State: ContainerState{Status: StatusRunning}}
	st.set(c)

	started := make(chan struct{})
	te.fake.Script = func(p *fakeProc) int {
		close(started)
		p.Write(sandbox.Stdout, "one\n")
		time.Sleep(30 * time.Millisecond)
		p.Write(sandbox.Stderr, "two\n")
		return 0
	}
	if err := te.fake.Start(sandbox.Spec{ID: id, Name: "fl", Image: "alpine"}); err != nil {
		t.Fatal(err)
	}
	<-started

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, te.srv.URL+"/containers/"+id+"/logs?follow=1&stdout=1&stderr=1", nil)
	res, err := te.srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	// Mark exited once fake finishes.
	go func() {
		<-te.fake.proc(id).Done()
		c.State.Status = StatusExited
		st.set(c)
		te.notify()
	}()
	body, err := io.ReadAll(res.Body)
	res.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	gotOut, gotErr := streamDecodeMuxFrames(t, body)
	if gotOut != "one\n" || gotErr != "two\n" {
		t.Fatalf("follow stdout=%q stderr=%q", gotOut, gotErr)
	}
}

func attachDialHijack(t *testing.T, srvURL, rawReq string) (net.Conn, *bufio.Reader, string) {
	t.Helper()
	u, err := url.Parse(srvURL)
	if err != nil {
		t.Fatal(err)
	}
	conn, err := net.DialTimeout("tcp", u.Host, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	if _, err := io.WriteString(conn, rawReq); err != nil {
		t.Fatal(err)
	}
	br := bufio.NewReader(conn)
	var head strings.Builder
	for {
		line, err := br.ReadString('\n')
		if err != nil {
			t.Fatalf("reading hijack response head: %v (got %q)", err, head.String())
		}
		head.WriteString(line)
		if line == "\r\n" || line == "\n" {
			break
		}
	}
	return conn, br, head.String()
}

func TestAttachBeforeStartStdinHalfClose(t *testing.T) {
	te, st := newStreamTestEngine(t)
	id := fakeHex("attach-cat")
	c := Container{
		ID:     id,
		Name:   "cat",
		Config: ContainerConfig{OpenStdin: true, StdinOnce: true, AttachStdin: true},
		State:  ContainerState{Status: StatusCreated},
	}
	st.set(c)

	// Attach BEFORE the container starts (docker run -i sequence).
	req := "POST /v1.52/containers/" + id + "/attach?stream=1&stdin=1&stdout=1&stderr=1 HTTP/1.1\r\n" +
		"Host: docker\r\nConnection: Upgrade\r\nUpgrade: tcp\r\n\r\n"
	conn, br, head := attachDialHijack(t, te.srv.URL, req)
	if !strings.HasPrefix(head, "HTTP/1.1 101 UPGRADED\r\n") {
		t.Fatalf("head = %q", head)
	}
	if !strings.Contains(head, "Content-Type: "+streamContentTypeMux) {
		t.Fatalf("missing multiplexed Content-Type in %q", head)
	}

	// Send stdin and half-close BEFORE start!
	if _, err := io.WriteString(conn, "hi-from-stdin\n"); err != nil {
		t.Fatal(err)
	}
	if tc, ok := conn.(*net.TCPConn); ok {
		if err := tc.CloseWrite(); err != nil {
			t.Fatal(err)
		}
	}

	// Now start the fake process: it reads stdin until EOF, echoes to stdout, and exits.
	te.fake.Script = func(p *fakeProc) int {
		b, _ := io.ReadAll(p.Stdin())
		p.Write(sandbox.Stdout, string(b))
		p.Write(sandbox.Stderr, "done-err\n")
		return 0
	}
	c.State.Status = StatusRunning
	st.set(c)
	if err := te.fake.Start(sandbox.Spec{ID: id, Name: "cat", Image: "alpine", Interactive: true}); err != nil {
		t.Fatal(err)
	}
	te.notify()

	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	raw, err := io.ReadAll(br)
	if err != nil {
		t.Fatal(err)
	}
	gotOut, gotErr := streamDecodeMuxFrames(t, raw)
	if gotOut != "hi-from-stdin\n" || gotErr != "done-err\n" {
		t.Fatalf("stdout=%q stderr=%q", gotOut, gotErr)
	}
}

func TestAttachTTY101And200AndDetach(t *testing.T) {
	te, st := newStreamTestEngine(t)
	id := fakeHex("attach-tty")
	c := Container{
		ID:     id,
		Name:   "sh",
		Config: ContainerConfig{Tty: true, OpenStdin: true},
		State:  ContainerState{Status: StatusRunning},
	}
	st.set(c)

	gotStdin := make(chan string, 4)
	release := make(chan struct{})
	te.fake.Script = func(p *fakeProc) int {
		go func() {
			buf := make([]byte, 64)
			for {
				n, err := p.Stdin().Read(buf)
				if n > 0 {
					gotStdin <- string(buf[:n])
				}
				if err != nil {
					return
				}
			}
		}()
		<-release
		return 0
	}
	if err := te.fake.Start(sandbox.Spec{ID: id, Name: "sh", Image: "alpine", TTY: true, Interactive: true}); err != nil {
		t.Fatal(err)
	}

	// 1. Upgraded 101 raw stream + detachKeys (ctrl-p ctrl-q) detaches without stopping.
	req101 := "POST /v1.52/containers/" + id + "/attach?stream=1&stdin=1&stdout=1&stderr=1 HTTP/1.1\r\n" +
		"Host: docker\r\nConnection: Upgrade\r\nUpgrade: tcp\r\n\r\n"
	conn, br, head := attachDialHijack(t, te.srv.URL, req101)
	if !strings.HasPrefix(head, "HTTP/1.1 101 UPGRADED\r\n") || !strings.Contains(head, "Content-Type: "+streamContentTypeRaw) {
		t.Fatalf("101 head = %q", head)
	}
	// Type "ls\n" then ctrl-p ctrl-q to detach.
	_, _ = conn.Write([]byte("ls\n\x10\x11"))
	select {
	case s := <-gotStdin:
		if s != "ls\n" {
			t.Fatalf("stdin = %q, want %q", s, "ls\n")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for stdin")
	}
	_ = conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	if _, err := io.ReadAll(br); err != nil {
		t.Fatalf("expected clean EOF after detach: %v", err)
	}
	// Container must still be running.
	if !te.fake.State(id).Status.Active() {
		t.Fatal("detach stopped the container")
	}

	// 2. Non-upgraded 200 raw stream.
	req200 := "POST /v1.52/containers/" + id + "/attach?stream=1&stdout=1&stderr=1 HTTP/1.1\r\nHost: docker\r\n\r\n"
	conn2, br2, head2 := attachDialHijack(t, te.srv.URL, req200)
	if !strings.HasPrefix(head2, "HTTP/1.1 200 OK\r\n") || !strings.Contains(head2, "Content-Type: "+streamContentTypeRaw) {
		t.Fatalf("200 head = %q", head2)
	}
	p := te.fake.proc(id)
	p.Write(sandbox.Stdout, "prompt> ")
	p.Write(sandbox.Stderr, "warn\r\n")
	close(release)
	_ = conn2.SetReadDeadline(time.Now().Add(3 * time.Second))
	out, err := io.ReadAll(br2)
	if err != nil {
		t.Fatal(err)
	}
	if string(out) != "prompt> warn\r\n" {
		t.Fatalf("200 raw output = %q, want %q", out, "prompt> warn\r\n")
	}
}

func TestWaitAllThreeConditions(t *testing.T) {
	te, st := newStreamTestEngine(t)

	// Missing container -> 404.
	code, _ := te.do(t, http.MethodPost, "/containers/missing/wait", nil)
	if code != http.StatusNotFound {
		t.Fatalf("missing wait status = %d, want 404", code)
	}
	// Invalid condition -> 400.
	id := fakeHex("wait-test")
	c := Container{ID: id, Name: "w", State: ContainerState{Status: StatusCreated, ExitCode: 0}}
	st.set(c)
	code, _ = te.do(t, http.MethodPost, "/containers/"+id+"/wait?condition=bogus", nil)
	if code != http.StatusBadRequest {
		t.Fatalf("bogus condition status = %d, want 400", code)
	}

	// 1. not-running returns immediately when created.
	code, body := te.do(t, http.MethodPost, "/containers/"+id+"/wait", nil)
	if code != 200 {
		t.Fatalf("not-running status = %d", code)
	}
	var wr streamWaitResponse
	if err := json.Unmarshal(body, &wr); err != nil || wr.StatusCode != 0 || wr.Error != nil {
		t.Fatalf("not-running body = %s (%v)", body, err)
	}

	// 2. next-exit flushes 200 header immediately before start, then returns exit code on exit.
	resCh := make(chan *http.Response, 1)
	go func() {
		res, err := te.srv.Client().Post(te.srv.URL+"/containers/"+id+"/wait?condition=next-exit", "", nil)
		if err == nil {
			resCh <- res
		}
	}()
	var res *http.Response
	select {
	case res = <-resCh:
		if res.StatusCode != 200 {
			t.Fatalf("wait header status = %d", res.StatusCode)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("wait did not flush 200 header before start")
	}
	go func() {
		time.Sleep(20 * time.Millisecond)
		c.State.Status = StatusExited
		c.State.ExitCode = 7
		c.State.Exits = 1
		st.set(c)
		te.notify()
	}()
	body, err := io.ReadAll(res.Body)
	res.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(body, &wr); err != nil || wr.StatusCode != 7 {
		t.Fatalf("next-exit body = %s", body)
	}

	// 3. removed after AutoRemove returns exit code from streamsEvent even when record is deleted immediately.
	rmID := fakeHex("wait-autorm")
	rmC := Container{
		ID:         rmID,
		Name:       "rm",
		HostConfig: HostConfig{AutoRemove: true},
		State:      ContainerState{Status: StatusCreated},
	}
	st.set(rmC)
	resCh2 := make(chan *http.Response, 1)
	go func() {
		res, err := te.srv.Client().Post(te.srv.URL+"/containers/"+rmID+"/wait?condition=removed", "", nil)
		if err == nil {
			resCh2 <- res
		}
	}()
	select {
	case res = <-resCh2:
	case <-time.After(3 * time.Second):
		t.Fatal("wait(removed) did not flush 200 header")
	}
	go func() {
		time.Sleep(20 * time.Millisecond)
		// Simulate AutoRemove deleting the record during containerEvent right before streamsEvent.
		st.delete(rmID)
		te.dropLogs(rmID)
		te.notify()
		te.streamsEvent(sandbox.Event{
			ID:    rmID,
			Kind:  sandbox.EventExited,
			State: sandbox.State{Status: sandbox.Failed, ExitCode: 42},
		})
	}()
	body, err = io.ReadAll(res.Body)
	res.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(body, &wr); err != nil || wr.StatusCode != 42 {
		t.Fatalf("removed body = %s, want StatusCode 42", body)
	}
}

func TestResizeAndStats(t *testing.T) {
	te, st := newStreamTestEngine(t)

	// Resize: 404, 409 not running, 200 non-tty, 200 tty reaches fakeProc.
	id := fakeHex("resize-tty")
	c := Container{ID: id, Name: "r", Config: ContainerConfig{Tty: true}, State: ContainerState{Status: StatusCreated}}
	st.set(c)

	if code, _ := te.do(t, http.MethodPost, "/containers/nope/resize?h=10&w=20", nil); code != 404 {
		t.Fatalf("missing resize = %d, want 404", code)
	}
	if code, _ := te.do(t, http.MethodPost, "/containers/"+id+"/resize?h=10&w=20", nil); code != 409 {
		t.Fatalf("stopped resize = %d, want 409", code)
	}

	// Start fakeProc.
	if err := te.fake.Start(sandbox.Spec{ID: id, Name: "r", Image: "alpine", TTY: true}); err != nil {
		t.Fatal(err)
	}
	c.State.Status = StatusRunning
	c.State.Pid = os.Getpid()
	c.HostConfig.Memory = 256 << 20
	st.set(c)

	if code, _ := te.do(t, http.MethodPost, "/containers/"+id+"/resize?h=45&w=120", nil); code != 200 {
		t.Fatalf("tty resize = %d, want 200", code)
	}
	rows, cols := te.fake.proc(id).Size()
	if rows != 45 || cols != 120 {
		t.Fatalf("fakeProc size = (%d,%d), want (45,120)", rows, cols)
	}

	// Non-TTY running container answers 200 without calling Resize.
	noTTY := fakeHex("resize-notty")
	st.set(Container{ID: noTTY, Name: "n", Config: ContainerConfig{Tty: false}, State: ContainerState{Status: StatusRunning}})
	if code, _ := te.do(t, http.MethodPost, "/containers/"+noTTY+"/resize?h=24&w=80", nil); code != 200 {
		t.Fatalf("non-tty resize = %d, want 200", code)
	}

	// Stats: shape parsed by dockerapi.Stats.
	code, body := te.do(t, http.MethodGet, "/containers/"+id+"/stats?stream=false&one-shot=true", nil)
	if code != 200 {
		t.Fatalf("stats status = %d: %s", code, body)
	}
	var ds dockerapi.Stats
	if err := json.Unmarshal(body, &ds); err != nil {
		t.Fatalf("dockerapi.Stats unmarshal: %v\n%s", err, body)
	}
	if ds.Read.IsZero() || ds.CPU.System == 0 || ds.CPU.OnlineCPUs < 1 || ds.Memory.Limit != 256<<20 {
		t.Fatalf("unexpected dockerapi.Stats: %+v", ds)
	}

	// Streaming stats delivers at least one object and stops on cancel.
	ctx, cancel := context.WithCancel(context.Background())
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, te.srv.URL+"/containers/"+id+"/stats?stream=true", nil)
	res, err := te.srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	var streamed dockerapi.Stats
	if err := json.NewDecoder(res.Body).Decode(&streamed); err != nil {
		t.Fatalf("streaming stats decode: %v", err)
	}
	cancel()
	res.Body.Close()
	if streamed.Read.IsZero() {
		t.Fatalf("streamed stats Read is zero")
	}
}

func TestLogsSinceUntilAndAttachLegacyVersionAndSlowSub(t *testing.T) {
	te, st := newStreamTestEngine(t)
	id := fakeHex("since-until")
	st.set(Container{ID: id, Name: "su", State: ContainerState{Status: StatusExited}})

	te.onRunnerOutput(id, sandbox.Output{Stream: sandbox.Stdout, Data: []byte("early\n")})
	time.Sleep(25 * time.Millisecond)
	mid := time.Now().UTC()
	time.Sleep(25 * time.Millisecond)
	te.onRunnerOutput(id, sandbox.Output{Stream: sandbox.Stdout, Data: []byte("late\n")})

	code, body := te.do(t, http.MethodGet, "/containers/"+id+"/logs?stdout=1&since="+url.QueryEscape(mid.Format(time.RFC3339Nano)), nil)
	if code != 200 {
		t.Fatalf("since status = %d", code)
	}
	gotOut, _ := streamDecodeMuxFrames(t, body)
	if gotOut != "late\n" {
		t.Fatalf("since stdout = %q, want %q", gotOut, "late\n")
	}

	code, body = te.do(t, http.MethodGet, "/containers/"+id+"/logs?stdout=1&until="+url.QueryEscape(mid.Format(time.RFC3339Nano)), nil)
	if code != 200 {
		t.Fatalf("until status = %d", code)
	}
	gotOut, _ = streamDecodeMuxFrames(t, body)
	if gotOut != "early\n" {
		t.Fatalf("until stdout = %q, want %q", gotOut, "early\n")
	}

	// Pre-1.42 attach on non-TTY uses raw-stream header with multiplexed frames,
	// and logs=1 replays existing logs without streaming.
	req := "POST /v1.41/containers/" + id + "/attach?logs=1&stream=0&stdout=1 HTTP/1.1\r\nHost: docker\r\n\r\n"
	conn, br, head := attachDialHijack(t, te.srv.URL, req)
	if !strings.HasPrefix(head, "HTTP/1.1 200 OK\r\n") || !strings.Contains(head, "Content-Type: "+streamContentTypeRaw) {
		t.Fatalf("v1.41 head = %q", head)
	}
	_ = conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	raw, err := io.ReadAll(br)
	if err != nil {
		t.Fatal(err)
	}
	gotOut, _ = streamDecodeMuxFrames(t, raw)
	if gotOut != "early\nlate\n" {
		t.Fatalf("v1.41 logs=1 stdout = %q, want %q", gotOut, "early\nlate\n")
	}

	// Slow subscriber overflow drops the subscriber without blocking onRunnerOutput.
	_, sub, unsub := te.streamReadAndSubscribe(id, true)
	defer unsub()
	for range streamSubBuffer + 50 {
		te.onRunnerOutput(id, sandbox.Output{Stream: sandbox.Stdout, Data: []byte("x\n")})
	}
	// Channel must eventually close due to overflow.
	drained := 0
	for range sub.ch {
		drained++
	}
	if drained != streamSubBuffer {
		t.Fatalf("drained %d entries, want %d", drained, streamSubBuffer)
	}

	// attachParseDetachKeys checks.
	seq, err := attachParseDetachKeys("ctrl-a,d")
	if err != nil || string(seq) != "\x01d" {
		t.Fatalf("attachParseDetachKeys(ctrl-a,d) = %q, %v", seq, err)
	}
	if _, err := attachParseDetachKeys("ctrl-99"); err == nil {
		t.Fatal("expected error for invalid detach key")
	}
}

func TestStreamsOpenCloseOpenOnSameEngine(t *testing.T) {
	dir := t.TempDir()
	fake := newFakeRunner()
	e := New(Config{Dir: dir, Runner: fake, Version: "0.0.0-test"})
	st := &streamMockStore{ctrs: map[string]Container{}}
	e.str.lookupOverride = st.lookup

	if err := e.Open(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := e.Close(time.Second); err != nil {
		t.Fatal(err)
	}
	if err := e.Open(context.Background()); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(e.Handler())
	t.Cleanup(func() {
		srv.Close()
		_ = e.Close(time.Second)
	})

	id := fakeHex("reopen-same-engine")
	c := Container{
		ID:     id,
		Name:   "reopened",
		Config: ContainerConfig{Tty: true, OpenStdin: true},
		State:  ContainerState{Status: StatusRunning},
	}
	st.set(c)

	release := make(chan struct{})
	fake.Script = func(p *fakeProc) int {
		<-release
		return 0
	}
	if err := fake.Start(sandbox.Spec{ID: id, Name: "reopened", Image: "alpine", TTY: true, Interactive: true}); err != nil {
		t.Fatal(err)
	}

	// Logs capture works after Open -> Close -> Open on the same Engine.
	fake.proc(id).Write(sandbox.Stdout, "before-attach\n")
	res, err := srv.Client().Get(srv.URL + "/containers/" + id + "/logs?stdout=1")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(res.Body)
	res.Body.Close()
	if res.StatusCode != 200 || string(body) != "before-attach\n" {
		t.Fatalf("logs after reopen: status=%d body=%q", res.StatusCode, body)
	}

	// Attach works after Open -> Close -> Open on the same Engine.
	req := "POST /v1.52/containers/" + id + "/attach?stream=1&stdout=1 HTTP/1.1\r\nHost: docker\r\nConnection: Upgrade\r\nUpgrade: tcp\r\n\r\n"
	conn, br, head := attachDialHijack(t, srv.URL, req)
	if !strings.HasPrefix(head, "HTTP/1.1 101 UPGRADED\r\n") {
		t.Fatalf("attach head after reopen = %q", head)
	}
	fake.proc(id).Write(sandbox.Stdout, "live-after-reopen\n")
	close(release)
	_ = conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	got, err := io.ReadAll(br)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "live-after-reopen\n" {
		t.Fatalf("attach output after reopen = %q, want %q", got, "live-after-reopen\n")
	}
}

func streamSubCount(e *Engine, id string) int {
	cs := e.streamGet(id)
	if cs == nil {
		return 0
	}
	cs.mu.Lock()
	defer cs.mu.Unlock()
	return len(cs.subs)
}

func waitSubCount(t *testing.T, e *Engine, id string, want int) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if streamSubCount(e, id) == want {
			return
		}
		time.Sleep(15 * time.Millisecond)
	}
	t.Fatalf("streamSubCount(%s) = %d, want %d", id, streamSubCount(e, id), want)
}

func dialUnixHijack(t *testing.T, sockPath, rawReq string) (*net.UnixConn, *bufio.Reader, string) {
	t.Helper()
	conn, err := net.DialTimeout("unix", sockPath, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	uc := conn.(*net.UnixConn)
	t.Cleanup(func() { _ = uc.Close() })
	if _, err := io.WriteString(uc, rawReq); err != nil {
		t.Fatal(err)
	}
	br := bufio.NewReader(uc)
	var head strings.Builder
	for {
		line, err := br.ReadString('\n')
		if err != nil {
			t.Fatalf("reading unix hijack response head: %v (got %q)", err, head.String())
		}
		head.WriteString(line)
		if line == "\r\n" || line == "\n" {
			break
		}
	}
	return uc, br, head.String()
}

func TestAttachPeerDisconnectAndOpenStdinGating(t *testing.T) {
	if runtime.GOOS != "darwin" && runtime.GOOS != "linux" {
		t.Skip("socket peer-close detection is darwin/linux only")
	}
	te, st := newStreamTestEngine(t)

	// Serve the engine on a short Unix socket (matching real CLI connections).
	sockDir, err := os.MkdirTemp("", "gvxd-peer-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(sockDir) })
	sockPath := filepath.Join(sockDir, "d.sock")
	ln, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: te.Handler()}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })

	// 1. Silent container, no stdin requested: client disconnect releases streamSub promptly.
	id1 := fakeHex("silent-nostdin")
	st.set(Container{ID: id1, Name: "s1", State: ContainerState{Status: StatusRunning}})
	if err := te.fake.Start(sandbox.Spec{ID: id1, Name: "s1", Image: "alpine"}); err != nil {
		t.Fatal(err)
	}
	req1 := "POST /v1.52/containers/" + id1 + "/attach?stream=1&stdout=1 HTTP/1.1\r\nHost: docker\r\nConnection: Upgrade\r\nUpgrade: tcp\r\n\r\n"
	c1, _, head1 := dialUnixHijack(t, sockPath, req1)
	if !strings.HasPrefix(head1, "HTTP/1.1 101 UPGRADED\r\n") {
		t.Fatalf("head1 = %q", head1)
	}
	waitSubCount(t, te.Engine, id1, 1)
	// Half-close (CloseWrite) must NOT tear down the attach.
	if err := c1.CloseWrite(); err != nil {
		t.Fatal(err)
	}
	time.Sleep(50 * time.Millisecond)
	if got := streamSubCount(te.Engine, id1); got != 1 {
		t.Fatalf("half-close dropped subscriber: got %d, want 1", got)
	}
	// Full close must release the subscriber without the container writing or exiting.
	_ = c1.Close()
	waitSubCount(t, te.Engine, id1, 0)

	// 2. Silent container with OpenStdin+StdinOnce: after stdin EOF, full client close releases streamSub.
	id2 := fakeHex("silent-after-eof")
	st.set(Container{
		ID:     id2,
		Name:   "s2",
		Config: ContainerConfig{OpenStdin: true, StdinOnce: true},
		State:  ContainerState{Status: StatusRunning},
	})
	if err := te.fake.Start(sandbox.Spec{ID: id2, Name: "s2", Image: "alpine", Interactive: true}); err != nil {
		t.Fatal(err)
	}
	req2 := "POST /v1.52/containers/" + id2 + "/attach?stream=1&stdin=1&stdout=1 HTTP/1.1\r\nHost: docker\r\nConnection: Upgrade\r\nUpgrade: tcp\r\n\r\n"
	c2, _, _ := dialUnixHijack(t, sockPath, req2)
	waitSubCount(t, te.Engine, id2, 1)
	if err := c2.CloseWrite(); err != nil {
		t.Fatal(err)
	}
	// Give stdin goroutine time to process EOF and enter attachWaitPeerClose.
	time.Sleep(50 * time.Millisecond)
	if got := streamSubCount(te.Engine, id2); got != 1 {
		t.Fatalf("half-close after stdin EOF dropped subscriber: got %d, want 1", got)
	}
	_ = c2.Close()
	waitSubCount(t, te.Engine, id2, 0)

	// 3. Container started detached (OpenStdin: false): attach with stdin=1 + typed bytes
	// must NOT block in attachWaitForStdin, and disconnect cleans up immediately.
	id3 := fakeHex("detached-nostdin")
	st.set(Container{
		ID:     id3,
		Name:   "s3",
		Config: ContainerConfig{OpenStdin: false},
		State:  ContainerState{Status: StatusRunning},
	})
	if err := te.fake.Start(sandbox.Spec{ID: id3, Name: "s3", Image: "alpine", Interactive: false}); err != nil {
		t.Fatal(err)
	}
	req3 := "POST /v1.52/containers/" + id3 + "/attach?stream=1&stdin=1&stdout=1 HTTP/1.1\r\nHost: docker\r\nConnection: Upgrade\r\nUpgrade: tcp\r\n\r\n"
	c3, _, _ := dialUnixHijack(t, sockPath, req3)
	waitSubCount(t, te.Engine, id3, 1)
	_, _ = io.WriteString(c3, "typed-into-detached-container\n")
	_ = c3.Close()
	waitSubCount(t, te.Engine, id3, 0)
}
