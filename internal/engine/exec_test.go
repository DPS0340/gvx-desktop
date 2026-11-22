package engine

import (
	"bufio"
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ironpark/gvx-desktop/internal/dockerapi"
	"github.com/ironpark/gvx-desktop/internal/overlay/guestcfg"
	"github.com/ironpark/gvx-desktop/internal/rungv"
	"github.com/ironpark/gvx-desktop/internal/sandbox"
)

// fakeControl is a fake sandbox.Control shared across W2 unit tests.
type fakeControl struct {
	mu          sync.Mutex
	target      rungv.Target
	execFn      func(ctx context.Context, o sandbox.ExecOptions) (sandbox.ExecProcess, error)
	execCalls   []sandbox.ExecOptions
	statFn      func(ctx context.Context, path string) (rungv.PathStat, error)
	copyFromFn  func(ctx context.Context, path string, o rungv.CopyOptions) (io.ReadCloser, error)
	copyToFn    func(ctx context.Context, dir string, r io.Reader, o rungv.CopyOptions) error
	statsFn     func(ctx context.Context) (rungv.Stats, error)
	streamFn    func(ctx context.Context, interval time.Duration, fn func(rungv.Stats)) error
	topFn       func(ctx context.Context) ([]rungv.Process, error)
	killFn      func(ctx context.Context, o rungv.KillOptions) error
	killCalls   []rungv.KillOptions
	copyToCalls []fakeCopyToCall
}

type fakeCopyToCall struct {
	Dir  string
	Data []byte
	Opts rungv.CopyOptions
}

func (c *fakeControl) Target() rungv.Target                  { return c.target }
func (c *fakeControl) ExecArgv(o rungv.ExecOptions) []string { return nil }

func (c *fakeControl) Exec(ctx context.Context, o sandbox.ExecOptions) (sandbox.ExecProcess, error) {
	c.mu.Lock()
	c.execCalls = append(c.execCalls, o)
	fn := c.execFn
	c.mu.Unlock()
	if fn != nil {
		return fn(ctx, o)
	}
	return newFakeExecProc(42, func(p *fakeExecProc) (int, error) {
		return 0, nil
	}), nil
}

func (c *fakeControl) CopyFrom(ctx context.Context, guestPath string, o rungv.CopyOptions) (io.ReadCloser, error) {
	c.mu.Lock()
	fn := c.copyFromFn
	c.mu.Unlock()
	if fn != nil {
		return fn(ctx, guestPath, o)
	}
	return nil, errors.New("not configured")
}

func (c *fakeControl) CopyTo(ctx context.Context, guestDir string, r io.Reader, o rungv.CopyOptions) error {
	data, err := io.ReadAll(r)
	if err != nil {
		return err
	}
	c.mu.Lock()
	c.copyToCalls = append(c.copyToCalls, fakeCopyToCall{Dir: guestDir, Data: data, Opts: o})
	fn := c.copyToFn
	c.mu.Unlock()
	if fn != nil {
		return fn(ctx, guestDir, bytes.NewReader(data), o)
	}
	return nil
}

func (c *fakeControl) StatPath(ctx context.Context, guestPath string) (rungv.PathStat, error) {
	c.mu.Lock()
	fn := c.statFn
	c.mu.Unlock()
	if fn != nil {
		return fn(ctx, guestPath)
	}
	return rungv.PathStat{}, errors.New("not configured")
}

func (c *fakeControl) Export(ctx context.Context, guestPath string, upperOnly bool) (io.ReadCloser, error) {
	return nil, errors.New("not configured")
}

func (c *fakeControl) Stats(ctx context.Context) (rungv.Stats, error) {
	c.mu.Lock()
	fn := c.statsFn
	c.mu.Unlock()
	if fn != nil {
		return fn(ctx)
	}
	return rungv.Stats{}, errors.New("no stats")
}

func (c *fakeControl) StreamStats(ctx context.Context, interval time.Duration, fn func(rungv.Stats)) error {
	c.mu.Lock()
	sfn := c.streamFn
	c.mu.Unlock()
	if sfn != nil {
		return sfn(ctx, interval, fn)
	}
	return errors.New("no stream stats")
}

func (c *fakeControl) Top(ctx context.Context) ([]rungv.Process, error) {
	c.mu.Lock()
	fn := c.topFn
	c.mu.Unlock()
	if fn != nil {
		return fn(ctx)
	}
	return nil, nil
}

func (c *fakeControl) Kill(ctx context.Context, o rungv.KillOptions) error {
	c.mu.Lock()
	c.killCalls = append(c.killCalls, o)
	fn := c.killFn
	c.mu.Unlock()
	if fn != nil {
		return fn(ctx, o)
	}
	return nil
}

// fakeExecProc is a fake sandbox.ExecProcess for unit tests.
type fakeExecProc struct {
	stdinR  *io.PipeReader
	stdinW  *io.PipeWriter
	stdoutR *io.PipeReader
	stdoutW *io.PipeWriter
	stderrR *io.PipeReader
	stderrW *io.PipeWriter

	mu       sync.Mutex
	pid      int
	rows     uint16
	cols     uint16
	resizes  [][2]uint16
	closed   bool
	started  chan struct{}
	done     chan struct{}
	exitCode int
	exitErr  error
	hasStdin bool
	hasErr   bool
}

func newFakeExecProc(pid int, run func(p *fakeExecProc) (int, error)) *fakeExecProc {
	return newFakeExecProcOpts(pid, true, true, run)
}

func newFakeExecProcOpts(pid int, interactive, hasStderr bool, run func(p *fakeExecProc) (int, error)) *fakeExecProc {
	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	errR, errW := io.Pipe()
	p := &fakeExecProc{
		stdinR:   inR,
		stdinW:   inW,
		stdoutR:  outR,
		stdoutW:  outW,
		stderrR:  errR,
		stderrW:  errW,
		pid:      pid,
		started:  make(chan struct{}),
		done:     make(chan struct{}),
		hasStdin: interactive,
		hasErr:   hasStderr,
	}
	close(p.started)
	go func() {
		code, err := run(p)
		_ = p.stdoutW.Close()
		_ = p.stderrW.Close()
		p.mu.Lock()
		p.exitCode = code
		p.exitErr = err
		p.mu.Unlock()
		close(p.done)
	}()
	return p
}

func (p *fakeExecProc) Stdin() io.WriteCloser {
	if !p.hasStdin {
		return nil
	}
	return p.stdinW
}
func (p *fakeExecProc) Stdout() io.Reader { return p.stdoutR }
func (p *fakeExecProc) Stderr() io.Reader {
	if !p.hasErr {
		return nil
	}
	return p.stderrR
}
func (p *fakeExecProc) Resize(rows, cols uint16) error {
	p.mu.Lock()
	p.rows, p.cols = rows, cols
	p.resizes = append(p.resizes, [2]uint16{rows, cols})
	p.mu.Unlock()
	return nil
}
func (p *fakeExecProc) PID() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.pid
}
func (p *fakeExecProc) Started() <-chan struct{} { return p.started }
func (p *fakeExecProc) Wait() (int, error) {
	<-p.done
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.exitCode, p.exitErr
}
func (p *fakeExecProc) Signal(ctx context.Context, sig string) error { return nil }
func (p *fakeExecProc) Close() error {
	p.mu.Lock()
	p.closed = true
	p.mu.Unlock()
	_ = p.stdinR.Close()
	_ = p.stdinW.Close()
	return nil
}

func newControlTestEngine(t *testing.T) (*testEngine, *fakeControl) {
	t.Helper()
	te := newTestEngine(t)
	ctl := &fakeControl{}
	te.fake.mu.Lock()
	te.fake.features = rungv.Features{Control: true}
	te.fake.defaultControl = func(*fakeProc) sandbox.Control { return ctl }
	te.fake.mu.Unlock()
	te.fake.addImage("alpine:3.20", ImageConfig{
		Cmd:        []string{"sleep", "60"},
		Env:        []string{"PATH=/usr/bin:/bin", "IMG=1"},
		WorkingDir: "/app",
	})
	return te, ctl
}

func createAndStartContainer(t *testing.T, te *testEngine, name string, body map[string]any) string {
	t.Helper()
	if body == nil {
		body = map[string]any{"Image": "alpine:3.20"}
	}
	if _, ok := body["Image"]; !ok {
		body["Image"] = "alpine:3.20"
	}
	st, created, raw := createTestContainer(t, te, name, body)
	if st != http.StatusCreated {
		t.Fatalf("create %s: %d %s", name, st, raw)
	}
	if st, raw := te.do(t, http.MethodPost, "/containers/"+created.ID+"/start", nil); st != http.StatusNoContent {
		t.Fatalf("start %s: %d %s", name, st, raw)
	}
	return created.ID
}

// dialHijack posts a JSON body to path on te.srv with optional Upgrade: tcp
// and returns the raw connection, buffered reader, status code, and headers.
func dialHijack(t *testing.T, te *testEngine, path string, body any, upgrade bool) (net.Conn, *bufio.Reader, int, http.Header) {
	t.Helper()
	addr := strings.TrimPrefix(te.srv.URL, "http://")
	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	payload := mustJSON(t, body)
	req, err := http.NewRequest(http.MethodPost, te.srv.URL+path, strings.NewReader(payload))
	if err != nil {
		conn.Close()
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	if upgrade {
		req.Header.Set("Connection", "Upgrade")
		req.Header.Set("Upgrade", "tcp")
	}
	if err := req.Write(conn); err != nil {
		conn.Close()
		t.Fatal(err)
	}
	br := bufio.NewReader(conn)
	res, err := http.ReadResponse(br, req)
	if err != nil {
		conn.Close()
		t.Fatal(err)
	}
	return conn, br, res.StatusCode, res.Header
}

func readMuxFrames(t *testing.T, r io.Reader) (stdout, stderr string) {
	t.Helper()
	var out, errb bytes.Buffer
	var hdr [8]byte
	for {
		_, err := io.ReadFull(r, hdr[:])
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("read mux hdr: %v", err)
		}
		n := binary.BigEndian.Uint32(hdr[4:8])
		buf := make([]byte, n)
		if _, err := io.ReadFull(r, buf); err != nil {
			t.Fatalf("read mux body: %v", err)
		}
		switch hdr[0] {
		case 1:
			out.Write(buf)
		case 2:
			errb.Write(buf)
		default:
			t.Fatalf("unexpected mux stream %d", hdr[0])
		}
	}
	return out.String(), errb.String()
}

func TestExecMuxAndStdinHalfClose(t *testing.T) {
	te, ctl := newControlTestEngine(t)
	cid := createAndStartContainer(t, te, "gvxd-exec1", map[string]any{
		"Image": "alpine:3.20",
		"User":  "nobody",
		"Env":   []string{"BASE=yes"},
	})

	var gotOpts sandbox.ExecOptions
	ctl.execFn = func(ctx context.Context, o sandbox.ExecOptions) (sandbox.ExecProcess, error) {
		gotOpts = o
		return newFakeExecProcOpts(77, o.Interactive, !o.TTY, func(p *fakeExecProc) (int, error) {
			in, _ := io.ReadAll(p.stdinR)
			_, _ = io.WriteString(p.stdoutW, "out:"+string(in))
			_, _ = io.WriteString(p.stderrW, "err-line\n")
			return 42, nil
		}), nil
	}

	// Create exec (bare KEY in Env must be dropped; User defaults to Config.User; Privileged defaults to false).
	st, body := te.do(t, http.MethodPost, "/v1.52/containers/"+cid+"/exec", map[string]any{
		"AttachStdin":  true,
		"AttachStdout": true,
		"AttachStderr": true,
		"Env":          []string{"FOO=bar", "BARE_HOST_LEAK"},
		"Cmd":          []string{"sh", "-c", "cat"},
	})
	if st != http.StatusCreated {
		t.Fatalf("exec create = %d %s", st, body)
	}
	var created struct{ ID string }
	_ = json.Unmarshal(body, &created)
	if len(created.ID) != 64 {
		t.Fatalf("exec ID = %q", created.ID)
	}

	// Container inspect lists ExecIDs.
	_, insBody := te.do(t, http.MethodGet, "/containers/"+cid+"/json", nil)
	var ins containerInspectResponse
	_ = json.Unmarshal(insBody, &ins)
	if !slices.Contains(ins.ExecIDs, created.ID) {
		t.Fatalf("ExecIDs = %v, want %s", ins.ExecIDs, created.ID)
	}

	// Start over 101 UPGRADED, send stdin, half-close, read mux stdout+stderr.
	conn, br, status, hdr := dialHijack(t, te, "/v1.52/exec/"+created.ID+"/start", map[string]any{
		"Detach": false,
		"Tty":    false,
	}, true)
	defer conn.Close()
	if status != http.StatusSwitchingProtocols {
		t.Fatalf("status = %d, want 101", status)
	}
	if ct := hdr.Get("Content-Type"); ct != streamContentTypeMux {
		t.Fatalf("Content-Type = %q, want %q", ct, streamContentTypeMux)
	}
	_, _ = io.WriteString(conn, "hello")
	_ = conn.(*net.TCPConn).CloseWrite()

	stdout, stderr := readMuxFrames(t, br)
	if stdout != "out:hello" || stderr != "err-line\n" {
		t.Fatalf("stdout=%q stderr=%q", stdout, stderr)
	}

	// Verify options passed to Control.Exec.
	if gotOpts.User != "nobody" || gotOpts.Workdir != "/app" {
		t.Errorf("User=%q Workdir=%q", gotOpts.User, gotOpts.Workdir)
	}
	if gotOpts.Privileged != nil {
		t.Errorf("Privileged = %v, want nil (rungv default)", gotOpts.Privileged)
	}
	if !slices.Contains(gotOpts.Env, "FOO=bar") || !slices.Contains(gotOpts.Env, "BASE=yes") {
		t.Errorf("Env missing expected keys: %v", gotOpts.Env)
	}
	if slices.Contains(gotOpts.Env, "BARE_HOST_LEAK") {
		t.Errorf("Env leaked bare key: %v", gotOpts.Env)
	}

	// Inspect shows Running: false, ExitCode: 42, Pid: 77.
	st, body = te.do(t, http.MethodGet, "/exec/"+created.ID+"/json", nil)
	if st != http.StatusOK {
		t.Fatalf("exec inspect = %d %s", st, body)
	}
	var ei execInspectResponse
	_ = json.Unmarshal(body, &ei)
	if ei.Running || ei.ExitCode == nil || *ei.ExitCode != 42 || ei.Pid != 77 || ei.ContainerID != cid {
		t.Fatalf("inspect = %+v", ei)
	}
	if ei.ProcessConfig.Entrypoint != "sh" || !slices.Equal(ei.ProcessConfig.Arguments, []string{"-c", "cat"}) {
		t.Fatalf("ProcessConfig = %+v", ei.ProcessConfig)
	}

	// Verify exec events (exec_create, exec_start, exec_die with exitCode=42).
	te.evt.mu.Lock()
	evs := slices.Clone(te.evt.ring)
	te.evt.mu.Unlock()
	var sawCreate, sawStart, sawDie bool
	for _, ev := range evs {
		if ev.ID != cid {
			continue
		}
		if strings.HasPrefix(ev.Action, "exec_create:") && ev.Attributes["execID"] == created.ID {
			sawCreate = true
		}
		if strings.HasPrefix(ev.Action, "exec_start:") && ev.Attributes["execID"] == created.ID {
			sawStart = true
		}
		if ev.Action == "exec_die" && ev.Attributes["execID"] == created.ID && ev.Attributes["exitCode"] == "42" {
			sawDie = true
		}
	}
	if !sawCreate || !sawStart || !sawDie {
		t.Errorf("missing exec events: create=%v start=%v die=%v", sawCreate, sawStart, sawDie)
	}
}

func TestExecTTY200ResizeAndDetachKeys(t *testing.T) {
	te, ctl := newControlTestEngine(t)
	cid := createAndStartContainer(t, te, "gvxd-exectty", nil)

	release := make(chan struct{})
	var procRef *fakeExecProc
	ctl.execFn = func(ctx context.Context, o sandbox.ExecOptions) (sandbox.ExecProcess, error) {
		p := newFakeExecProcOpts(88, o.Interactive, false, func(p *fakeExecProc) (int, error) {
			_, _ = io.WriteString(p.stdoutW, "PROMPT# ")
			buf := make([]byte, 64)
			n, _ := p.stdinR.Read(buf)
			_, _ = io.WriteString(p.stdoutW, "got:"+string(buf[:n]))
			<-release
			return 7, nil
		})
		procRef = p
		return p, nil
	}

	st, body := te.do(t, http.MethodPost, "/containers/"+cid+"/exec", map[string]any{
		"AttachStdin":  true,
		"AttachStdout": true,
		"Tty":          true,
		"ConsoleSize":  [2]uint{25, 90},
		"Cmd":          []string{"sh"},
	})
	if st != http.StatusCreated {
		t.Fatalf("create = %d %s", st, body)
	}
	var created struct{ ID string }
	_ = json.Unmarshal(body, &created)

	// Start over plain 200 OK (no Upgrade header) and raw stream.
	conn, br, status, hdr := dialHijack(t, te, "/v1.52/exec/"+created.ID+"/start", map[string]any{
		"Detach": false,
		"Tty":    true,
	}, false)
	defer conn.Close()
	if status != http.StatusOK || hdr.Get("Content-Type") != streamContentTypeRaw {
		t.Fatalf("status=%d ct=%q", status, hdr.Get("Content-Type"))
	}

	// Read prompt.
	buf := make([]byte, 8)
	if _, err := io.ReadFull(br, buf); err != nil || string(buf) != "PROMPT# " {
		t.Fatalf("prompt = %q err=%v", buf, err)
	}

	// Resize reaches ExecProcess.
	if st, body := te.do(t, http.MethodPost, "/exec/"+created.ID+"/resize?h=40&w=120", nil); st != http.StatusOK {
		t.Fatalf("resize = %d %s", st, body)
	}
	procRef.mu.Lock()
	r, c := procRef.rows, procRef.cols
	procRef.mu.Unlock()
	if r != 40 || c != 120 {
		t.Fatalf("size = %dx%d, want 40x120", r, c)
	}

	// Inspect while running: ExitCode must be null.
	_, body = te.do(t, http.MethodGet, "/exec/"+created.ID+"/json", nil)
	if !strings.Contains(string(body), `"ExitCode":null`) || !strings.Contains(string(body), `"Running":true`) {
		t.Fatalf("inspect while running = %s", body)
	}

	// Send input followed by default detach keys (ctrl-p, ctrl-q).
	_, _ = conn.Write([]byte{'h', 'i', 16, 17})
	// Connection should close promptly without closing procRef.
	rest, _ := io.ReadAll(br)
	if len(rest) > 0 && !strings.Contains(string(rest), "got:hi") {
		t.Logf("remaining bytes before detach: %q", rest)
	}

	procRef.mu.Lock()
	wasClosed := procRef.closed
	procRef.mu.Unlock()
	if wasClosed {
		t.Fatal("detach keys closed the exec process")
	}

	// Still running in inspect until guest exits.
	_, body = te.do(t, http.MethodGet, "/exec/"+created.ID+"/json", nil)
	var ei execInspectResponse
	_ = json.Unmarshal(body, &ei)
	if !ei.Running {
		t.Fatalf("expected still running after detach: %+v", ei)
	}

	// Let guest exit; inspect should update to Running:false, ExitCode:7.
	close(release)
	waitFor(t, func() bool {
		_, b := te.do(t, http.MethodGet, "/exec/"+created.ID+"/json", nil)
		var st execInspectResponse
		_ = json.Unmarshal(b, &st)
		return !st.Running && st.ExitCode != nil && *st.ExitCode == 7
	})
}

func TestExecDetachedAndExitCodeMapping(t *testing.T) {
	te, ctl := newControlTestEngine(t)
	cid := createAndStartContainer(t, te, "gvxd-execdet", nil)

	// 1. Detached exec runs non-detached in rungv and tracks Running -> ExitCode.
	release := make(chan struct{})
	var detachOpt bool
	ctl.execFn = func(ctx context.Context, o sandbox.ExecOptions) (sandbox.ExecProcess, error) {
		detachOpt = o.Detach
		return newFakeExecProcOpts(99, false, true, func(p *fakeExecProc) (int, error) {
			_, _ = io.WriteString(p.stdoutW, "background output\n")
			<-release
			return 5, nil
		}), nil
	}
	_, body := te.do(t, http.MethodPost, "/containers/"+cid+"/exec", map[string]any{"Cmd": []string{"sleep", "1"}})
	var created struct{ ID string }
	_ = json.Unmarshal(body, &created)
	if st, b := te.do(t, http.MethodPost, "/exec/"+created.ID+"/start", map[string]any{"Detach": true}); st != http.StatusOK || len(b) != 0 {
		t.Fatalf("detached start = %d %q", st, b)
	}
	if detachOpt {
		t.Fatal("detached exec should run without rungv -d so exit code is tracked")
	}
	close(release)
	waitFor(t, func() bool {
		_, b := te.do(t, http.MethodGet, "/exec/"+created.ID+"/json", nil)
		var st execInspectResponse
		_ = json.Unmarshal(b, &st)
		return !st.Running && st.ExitCode != nil && *st.ExitCode == 5
	})

	// 2. Exit code mapping: 127 (not found, pid=0), 126 (limit exceeded, pid=0), 137 (ended after start, pid>0).
	cases := []struct {
		name     string
		pid      int
		err      error
		wantCode int
		wantMsg  string
	}{
		{
			name:     "not_found_127",
			pid:      0,
			err:      &rungv.Error{Stderr: "rungv exec: NOT_FOUND: executable \"nope\" not found"},
			wantCode: 127,
			wantMsg:  "OCI runtime exec failed:",
		},
		{
			name:     "limit_exceeded_126",
			pid:      0,
			err:      &rungv.Error{Stderr: "rungv exec: LIMIT_EXCEEDED: too many exec sessions"},
			wantCode: 126,
			wantMsg:  "LIMIT_EXCEEDED",
		},
		{
			name:     "died_after_start_137",
			pid:      123,
			err:      &rungv.Error{Stderr: "rungv exec: SANDBOX_EXITING: sandbox exited"},
			wantCode: 137,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctl.execFn = func(ctx context.Context, o sandbox.ExecOptions) (sandbox.ExecProcess, error) {
				return newFakeExecProcOpts(tc.pid, false, true, func(p *fakeExecProc) (int, error) {
					return -1, tc.err
				}), nil
			}
			_, b := te.do(t, http.MethodPost, "/containers/"+cid+"/exec", map[string]any{
				"AttachStdout": true,
				"AttachStderr": true,
				"Cmd":          []string{"nope"},
			})
			var c struct{ ID string }
			_ = json.Unmarshal(b, &c)
			conn, br, _, _ := dialHijack(t, te, "/v1.52/exec/"+c.ID+"/start", map[string]any{}, true)
			_, stderr := readMuxFrames(t, br)
			conn.Close()
			if tc.wantMsg != "" && !strings.Contains(stderr, tc.wantMsg) {
				t.Errorf("stderr = %q, want %q", stderr, tc.wantMsg)
			}
			_, ib := te.do(t, http.MethodGet, "/exec/"+c.ID+"/json", nil)
			var ei execInspectResponse
			_ = json.Unmarshal(ib, &ei)
			if ei.ExitCode == nil || *ei.ExitCode != tc.wantCode {
				t.Errorf("exitCode = %v, want %d", ei.ExitCode, tc.wantCode)
			}
		})
	}
}

func TestExecNetinitBlanksGVXNetAndExplicitUser(t *testing.T) {
	te, ctl := newControlTestEngine(t)
	if st, b := te.do(t, http.MethodPost, "/networks/create", map[string]any{"Name": "gvxd-execnet"}); st != http.StatusCreated {
		t.Fatalf("network create = %d %s", st, b)
	}
	cid := createAndStartContainer(t, te, "gvxd-netctr", map[string]any{
		"Image":      "alpine:3.20",
		"User":       "1000:1000",
		"HostConfig": map[string]any{"NetworkMode": "gvxd-execnet"},
	})
	var gotOpts sandbox.ExecOptions
	ctl.execFn = func(ctx context.Context, o sandbox.ExecOptions) (sandbox.ExecProcess, error) {
		gotOpts = o
		return newFakeExecProcOpts(12, false, true, func(p *fakeExecProc) (int, error) { return 0, nil }), nil
	}
	_, body := te.do(t, http.MethodPost, "/containers/"+cid+"/exec", map[string]any{
		"AttachStdout": true,
		"Cmd":          []string{"env"},
	})
	var created struct{ ID string }
	_ = json.Unmarshal(body, &created)
	if st, b := te.do(t, http.MethodPost, "/exec/"+created.ID+"/start", map[string]any{"Detach": true}); st != http.StatusOK {
		t.Fatalf("start = %d %s", st, b)
	}
	if !slices.Contains(gotOpts.Env, guestcfg.EnvVar+"=") {
		t.Errorf("expected %s= to blank WireGuard key, got env %v", guestcfg.EnvVar, gotOpts.Env)
	}
	if gotOpts.User != "1000:1000" {
		t.Errorf("User = %q, want 1000:1000", gotOpts.User)
	}
}

func TestExecErrorsAndOldRunAndV019(t *testing.T) {
	te, _ := newControlTestEngine(t)

	// Empty Cmd -> 400.
	cid := createAndStartContainer(t, te, "gvxd-execerr", nil)
	if st, _ := te.do(t, http.MethodPost, "/containers/"+cid+"/exec", map[string]any{"Cmd": []string{}}); st != http.StatusBadRequest {
		t.Fatalf("empty cmd = %d, want 400", st)
	}

	// Paused -> 409 at create and at start (before hijack).
	_, body := te.do(t, http.MethodPost, "/containers/"+cid+"/exec", map[string]any{"Cmd": []string{"true"}})
	var prePaused struct{ ID string }
	_ = json.Unmarshal(body, &prePaused)
	if st, b := te.do(t, http.MethodPost, "/containers/"+cid+"/pause", nil); st != http.StatusNoContent {
		t.Fatalf("pause = %d %s", st, b)
	}
	if st, b := te.do(t, http.MethodPost, "/containers/"+cid+"/exec", map[string]any{"Cmd": []string{"true"}}); st != http.StatusConflict || !strings.Contains(string(b), "paused") {
		t.Fatalf("exec create paused = %d %s", st, b)
	}
	if st, b := te.do(t, http.MethodPost, "/exec/"+prePaused.ID+"/start", map[string]any{}); st != http.StatusConflict || !strings.Contains(string(b), "paused") {
		t.Fatalf("exec start paused = %d %s", st, b)
	}
	_, _ = te.do(t, http.MethodPost, "/containers/"+cid+"/unpause", nil)

	// Stopped container -> 409.
	_, _ = te.do(t, http.MethodPost, "/containers/"+cid+"/stop?t=0", nil)
	if st, b := te.do(t, http.MethodPost, "/containers/"+cid+"/exec", map[string]any{"Cmd": []string{"true"}}); st != http.StatusConflict || !strings.Contains(string(b), "not running") {
		t.Fatalf("exec stopped = %d %s", st, b)
	}

	// Old run (Features.Control true, but container started without control socket) -> 409 "restart it to use exec".
	te.fake.mu.Lock()
	te.fake.features = rungv.Features{Control: false}
	te.fake.mu.Unlock()
	oldID := createAndStartContainer(t, te, "gvxd-oldrun", nil)
	te.fake.mu.Lock()
	te.fake.features = rungv.Features{Control: true}
	te.fake.mu.Unlock()
	if st, b := te.do(t, http.MethodPost, "/containers/"+oldID+"/exec", map[string]any{"Cmd": []string{"true"}}); st != http.StatusConflict || !strings.Contains(string(b), "restart it to use exec") {
		t.Fatalf("old run exec = %d %s", st, b)
	}
}

// TestDockerAPIExecAgainstEngine exercises the UI's Terminal tab path
// (dockerapi.Client.Exec + Resize + Wait + ExecDetached) against the
// httptest engine.
func TestDockerAPIExecAgainstEngine(t *testing.T) {
	te, ctl := newControlTestEngine(t)
	cid := createAndStartContainer(t, te, "gvxd-uiterminal", nil)

	var (
		mu       sync.Mutex
		lastProc *fakeExecProc
		detached []string
	)
	ctl.execFn = func(ctx context.Context, o sandbox.ExecOptions) (sandbox.ExecProcess, error) {
		if !o.TTY {
			mu.Lock()
			detached = slices.Clone(o.Argv)
			mu.Unlock()
			return newFakeExecProcOpts(202, false, true, func(p *fakeExecProc) (int, error) {
				return 0, nil
			}), nil
		}
		p := newFakeExecProcOpts(201, o.Interactive, false, func(p *fakeExecProc) (int, error) {
			_, _ = io.WriteString(p.stdoutW, "ready\r\n")
			buf := make([]byte, 32)
			n, _ := p.stdinR.Read(buf)
			_, _ = io.WriteString(p.stdoutW, "echo:"+string(buf[:n]))
			return 9, nil
		})
		mu.Lock()
		lastProc = p
		mu.Unlock()
		return p, nil
	}

	client, err := dockerapi.New(te.srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	ex, err := client.Exec(ctx, cid, []string{"/bin/sh"}, []string{"TERM=xterm-256color"}, 80, 24)
	if err != nil {
		t.Fatalf("dockerapi.Exec: %v", err)
	}
	defer ex.Close()

	// Read banner.
	buf := make([]byte, 7)
	if _, err := io.ReadFull(ex, buf); err != nil || string(buf) != "ready\r\n" {
		t.Fatalf("banner = %q err=%v", buf, err)
	}
	if err := ex.Resize(100, 32, 0, 0); err != nil {
		t.Fatalf("Resize: %v", err)
	}
	mu.Lock()
	lp := lastProc
	mu.Unlock()
	lp.mu.Lock()
	gotRows, gotCols := lp.rows, lp.cols
	lp.mu.Unlock()
	if gotRows != 32 || gotCols != 100 {
		t.Fatalf("Resize got %dx%d, want 32x100", gotRows, gotCols)
	}

	_, _ = io.WriteString(ex, "ping")
	rest, _ := io.ReadAll(ex)
	if string(rest) != "echo:ping" {
		t.Fatalf("output = %q, want echo:ping", rest)
	}
	if code := ex.Wait(); code != 9 {
		t.Fatalf("Wait() = %d, want 9", code)
	}

	// ExecDetached (used by term.Swept sweep).
	if err := client.ExecDetached(ctx, cid, []string{"sh", "-c", "kill -HUP 1"}); err != nil {
		t.Fatalf("ExecDetached: %v", err)
	}
	mu.Lock()
	gotDetached := slices.Clone(detached)
	mu.Unlock()
	if !slices.Equal(gotDetached, []string{"sh", "-c", "kill -HUP 1"}) {
		t.Fatalf("detached argv = %v", gotDetached)
	}
}

func TestExecPrivilegedInheritance(t *testing.T) {
	te, ctl := newControlTestEngine(t)
	normalID := createAndStartContainer(t, te, "gvxd-priv-norm", nil)
	privID := createAndStartContainer(t, te, "gvxd-priv-yes", map[string]any{
		"HostConfig": map[string]any{"Privileged": true},
	})

	var lastPriv *bool
	ctl.execFn = func(ctx context.Context, o sandbox.ExecOptions) (sandbox.ExecProcess, error) {
		lastPriv = o.Privileged
		return newFakeExecProcOpts(10, false, true, func(p *fakeExecProc) (int, error) { return 0, nil }), nil
	}
	runExec := func(cid string, priv bool) {
		_, b := te.do(t, http.MethodPost, "/containers/"+cid+"/exec", map[string]any{
			"Cmd":        []string{"id"},
			"Privileged": priv,
		})
		var c struct{ ID string }
		_ = json.Unmarshal(b, &c)
		st, sb := te.do(t, http.MethodPost, "/exec/"+c.ID+"/start", map[string]any{"Detach": true})
		if st != http.StatusOK {
			t.Fatalf("start = %d %s", st, sb)
		}
	}

	// Normal container + non-privileged exec -> nil (rungv default).
	runExec(normalID, false)
	if lastPriv != nil {
		t.Fatalf("normal+false: Privileged = %v, want nil", *lastPriv)
	}
	// Normal container + privileged exec -> *true.
	runExec(normalID, true)
	if lastPriv == nil || !*lastPriv {
		t.Fatalf("normal+true: Privileged = %v, want *true", lastPriv)
	}
	// Privileged container + non-privileged exec -> *true.
	runExec(privID, false)
	if lastPriv == nil || !*lastPriv {
		t.Fatalf("priv+false: Privileged = %v, want *true", lastPriv)
	}
}

func TestExecClosesProcessAcrossAllPaths(t *testing.T) {
	te, ctl := newControlTestEngine(t)
	cid := createAndStartContainer(t, te, "gvxd-fdleak", nil)

	var (
		mu    sync.Mutex
		procs []*fakeExecProc
	)
	ctl.execFn = func(ctx context.Context, o sandbox.ExecOptions) (sandbox.ExecProcess, error) {
		p := newFakeExecProcOpts(300, o.Interactive, !o.TTY, func(p *fakeExecProc) (int, error) {
			_, _ = io.WriteString(p.stdoutW, "ok\n")
			return 0, nil
		})
		mu.Lock()
		procs = append(procs, p)
		mu.Unlock()
		return p, nil
	}

	// 15 sequential attached execs + 5 detached + 5 no-streams.
	for range 15 {
		_, b := te.do(t, http.MethodPost, "/containers/"+cid+"/exec", map[string]any{
			"AttachStdout": true,
			"Cmd":          []string{"echo", "ok"},
		})
		var c struct{ ID string }
		_ = json.Unmarshal(b, &c)
		conn, br, st, _ := dialHijack(t, te, "/v1.52/exec/"+c.ID+"/start", map[string]any{}, true)
		if st != http.StatusSwitchingProtocols {
			t.Fatalf("status = %d", st)
		}
		_, _ = readMuxFrames(t, br)
		conn.Close()
	}
	for range 5 {
		_, b := te.do(t, http.MethodPost, "/containers/"+cid+"/exec", map[string]any{
			"Cmd": []string{"echo", "ok"},
		})
		var c struct{ ID string }
		_ = json.Unmarshal(b, &c)
		// No streams attached + Detach:false -> 200 immediately (detached).
		if st, sb := te.do(t, http.MethodPost, "/exec/"+c.ID+"/start", map[string]any{"Detach": false}); st != http.StatusOK || len(sb) != 0 {
			t.Fatalf("no-streams start = %d %q", st, sb)
		}
	}
	for range 5 {
		_, b := te.do(t, http.MethodPost, "/containers/"+cid+"/exec", map[string]any{
			"AttachStdout": true,
			"Cmd":          []string{"echo", "ok"},
		})
		var c struct{ ID string }
		_ = json.Unmarshal(b, &c)
		if st, sb := te.do(t, http.MethodPost, "/exec/"+c.ID+"/start", map[string]any{"Detach": true}); st != http.StatusOK || len(sb) != 0 {
			t.Fatalf("detached start = %d %q", st, sb)
		}
	}

	waitFor(t, func() bool {
		mu.Lock()
		defer mu.Unlock()
		if len(procs) != 25 {
			return false
		}
		for _, p := range procs {
			p.mu.Lock()
			closed := p.closed
			p.mu.Unlock()
			if !closed {
				return false
			}
		}
		return true
	})
}

func TestExecPeerDisconnectKeepsRunningAndTracksExit(t *testing.T) {
	te, ctl := newControlTestEngine(t)
	cid := createAndStartContainer(t, te, "gvxd-peerdisc", nil)

	release := make(chan struct{})
	var procRef *fakeExecProc
	ctl.execFn = func(ctx context.Context, o sandbox.ExecOptions) (sandbox.ExecProcess, error) {
		p := newFakeExecProcOpts(404, o.Interactive, !o.TTY, func(p *fakeExecProc) (int, error) {
			_, _ = io.WriteString(p.stdoutW, "started\n")
			<-release
			_, _ = io.WriteString(p.stdoutW, "finished\n")
			return 11, nil
		})
		procRef = p
		return p, nil
	}

	_, b := te.do(t, http.MethodPost, "/containers/"+cid+"/exec", map[string]any{
		"AttachStdin":  true,
		"AttachStdout": true,
		"Tty":          true,
		"Cmd":          []string{"sh"},
	})
	var created struct{ ID string }
	_ = json.Unmarshal(b, &created)

	conn, br, st, _ := dialHijack(t, te, "/v1.52/exec/"+created.ID+"/start", map[string]any{"Tty": true}, true)
	if st != http.StatusSwitchingProtocols {
		t.Fatalf("status = %d", st)
	}
	buf := make([]byte, 8)
	if _, err := io.ReadFull(br, buf); err != nil {
		t.Fatal(err)
	}
	// Abruptly disconnect client without sending detach keys.
	_ = conn.Close()
	time.Sleep(100 * time.Millisecond)

	// Exec must NOT be closed or interrupted; inspect still shows Running:true.
	procRef.mu.Lock()
	closedEarly := procRef.closed
	procRef.mu.Unlock()
	if closedEarly {
		t.Fatal("peer disconnect closed ExecProcess prematurely")
	}
	_, ib := te.do(t, http.MethodGet, "/exec/"+created.ID+"/json", nil)
	var ei execInspectResponse
	_ = json.Unmarshal(ib, &ei)
	if !ei.Running || ei.ExitCode != nil {
		t.Fatalf("after peer disconnect inspect = %+v, want Running=true", ei)
	}

	// Let guest command exit; inspect records ExitCode=11 and proc is closed.
	close(release)
	waitFor(t, func() bool {
		_, b := te.do(t, http.MethodGet, "/exec/"+created.ID+"/json", nil)
		var st execInspectResponse
		_ = json.Unmarshal(b, &st)
		procRef.mu.Lock()
		closed := procRef.closed
		procRef.mu.Unlock()
		return !st.Running && st.ExitCode != nil && *st.ExitCode == 11 && closed
	})
}

func TestExecStartRaceWithRemoveClosesProc(t *testing.T) {
	te, ctl := newControlTestEngine(t)
	cid := createAndStartContainer(t, te, "gvxd-race-rm", nil)

	var procRef *fakeExecProc
	ctl.execFn = func(ctx context.Context, o sandbox.ExecOptions) (sandbox.ExecProcess, error) {
		// Simulate concurrent container rm -f dropping exec records while ctl.Exec runs.
		te.dropExecsForContainer(cid)
		p := newFakeExecProcOpts(505, false, true, func(p *fakeExecProc) (int, error) { return 0, nil })
		procRef = p
		return p, nil
	}

	_, b := te.do(t, http.MethodPost, "/containers/"+cid+"/exec", map[string]any{
		"AttachStdout": true,
		"Cmd":          []string{"true"},
	})
	var created struct{ ID string }
	_ = json.Unmarshal(b, &created)

	st, body := te.do(t, http.MethodPost, "/exec/"+created.ID+"/start", map[string]any{"Detach": true})
	if st != http.StatusNotFound {
		t.Fatalf("start after concurrent drop = %d %s, want 404", st, body)
	}
	waitFor(t, func() bool {
		procRef.mu.Lock()
		defer procRef.mu.Unlock()
		return procRef.closed
	})
}
