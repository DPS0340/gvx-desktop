package sandbox

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/ironpark/gvx-desktop/internal/rungv"
)

func TestRunArgsAndRestoreArgsV019IdenticalAndToken(t *testing.T) {
	spec := Spec{
		ID:      "abcd1234",
		Name:    "web",
		Image:   "alpine:3.20",
		Command: "sleep 60",
		Memory:  "512m",
		Ports:   []string{"8080:80"},
	}

	// v0.19 without checkpoint: Paths{} -> byte-identical to today.
	gotV19 := spec.RunArgs(Paths{})
	wantV19 := []string{"run", "--name", "web", "--memory", "512m", "-p", "8080:80", "alpine:3.20", "sleep", "60"}
	if !reflect.DeepEqual(gotV19, wantV19) {
		t.Fatalf("v0.19 RunArgs:\n got  %q\n want %q", gotV19, wantV19)
	}

	// v0.19 with checkpoint: only -control-socket and -checkpoint-dir, no token flag.
	gotV19Ckpt := spec.RunArgs(Paths{ControlSocket: "/r/a.sock", CheckpointDir: "/r/ck"})
	wantV19Ckpt := []string{"run", "--name", "web", "--memory", "512m", "-p", "8080:80", "-control-socket", "/r/a.sock", "-checkpoint-dir", "/r/ck", "alpine:3.20", "sleep", "60"}
	if !reflect.DeepEqual(gotV19Ckpt, wantV19Ckpt) {
		t.Fatalf("v0.19 checkpoint RunArgs:\n got  %q\n want %q", gotV19Ckpt, wantV19Ckpt)
	}

	// v0.20 with token (Windows): adds --control-token-file.
	gotV20Tok := spec.RunArgs(Paths{ControlSocket: `C:\r\a.sock`, TokenFile: `C:\r\a.sock.token`, Control: true})
	wantV20Tok := []string{"run", "--name", "web", "--memory", "512m", "-p", "8080:80", "-control-socket", `C:\r\a.sock`, "--control-token-file", `C:\r\a.sock.token`, "alpine:3.20", "sleep", "60"}
	if !reflect.DeepEqual(gotV20Tok, wantV20Tok) {
		t.Fatalf("v0.20 token RunArgs:\n got  %q\n want %q", gotV20Tok, wantV20Tok)
	}

	// RestoreArgs: v0.19 identical, v0.20 adds --control-token-file.
	gotRestoreV19 := spec.RestoreArgs("/ck", Paths{ControlSocket: "/r/a.sock"})
	wantRestoreV19 := []string{"restore", "-checkpoint-dir", "/ck", "-control-socket", "/r/a.sock", "--name", "web", "-p", "8080:80"}
	if !reflect.DeepEqual(gotRestoreV19, wantRestoreV19) {
		t.Fatalf("v0.19 RestoreArgs:\n got  %q\n want %q", gotRestoreV19, wantRestoreV19)
	}
	gotRestoreV20 := spec.RestoreArgs("/ck", Paths{ControlSocket: `C:\r\a.sock`, TokenFile: `C:\r\a.sock.token`, Control: true})
	wantRestoreV20 := []string{"restore", "-checkpoint-dir", "/ck", "-control-socket", `C:\r\a.sock`, "--control-token-file", `C:\r\a.sock.token`, "--name", "web", "-p", "8080:80"}
	if !reflect.DeepEqual(gotRestoreV20, wantRestoreV20) {
		t.Fatalf("v0.20 RestoreArgs:\n got  %q\n want %q", gotRestoreV20, wantRestoreV20)
	}
}

func TestComputePathsTable(t *testing.T) {
	shortID := "abcd1234"
	longID := strings.Repeat("a", 64)
	longDataDir := "/Users/someone/Library/Application Support/gvx-desktop/very/deep/nested/directory/for/testing"
	winLongDataDir := `C:\Users\someone\AppData\Roaming\gvx-desktop\very\deep\nested\directory\that\exceeds\one\hundred\bytes`

	cases := []struct {
		name     string
		env      pathsEnv
		spec     Spec
		feat     rungv.Features
		wantCtrl bool
		wantCkpt bool
		wantSock string
		wantTok  string
		maxLen   int
		wantHash bool
	}{
		{
			name:     "unix v0.19 no checkpoint",
			env:      pathsEnv{goos: "darwin", dataDir: "/data", euid: 501},
			spec:     Spec{ID: shortID},
			feat:     rungv.Features{},
			wantCtrl: false,
		},
		{
			name:     "unix v0.19 checkpoint",
			env:      pathsEnv{goos: "darwin", dataDir: "/data", euid: 501},
			spec:     Spec{ID: shortID, Checkpoint: true},
			feat:     rungv.Features{},
			wantCtrl: false,
			wantCkpt: true,
			wantSock: "/data/run/abcd1234.sock",
		},
		{
			name:     "windows v0.19 checkpoint uses tcp",
			env:      pathsEnv{goos: "windows", dataDir: `C:\data`, tempDir: `C:\Temp`},
			spec:     Spec{ID: shortID, Checkpoint: true},
			feat:     rungv.Features{},
			wantCtrl: false,
			wantCkpt: true,
			wantSock: "127.0.0.1:0",
		},
		{
			name:     "unix v0.20 no checkpoint",
			env:      pathsEnv{goos: "darwin", dataDir: "/data", euid: 501},
			spec:     Spec{ID: shortID},
			feat:     rungv.Features{Control: true},
			wantCtrl: true,
			wantSock: "/data/run/abcd1234.sock",
		},
		{
			name:     "unix v0.20 checkpoint",
			env:      pathsEnv{goos: "linux", dataDir: "/data", euid: 1000},
			spec:     Spec{ID: shortID, Checkpoint: true},
			feat:     rungv.Features{Control: true},
			wantCtrl: true,
			wantCkpt: true,
			wantSock: "/data/run/abcd1234.sock",
		},
		{
			name:     "unix v0.20 64-hex ID under long DataDir uses hashed name and /tmp/gvxd-euid",
			env:      pathsEnv{goos: "darwin", dataDir: longDataDir, euid: 501},
			spec:     Spec{ID: longID},
			feat:     rungv.Features{Control: true},
			wantCtrl: true,
			wantSock: "/tmp/gvxd-501/" + sockName(longID) + ".sock",
			maxLen:   maxSocketPath,
			wantHash: true,
		},
		{
			name:     "windows v0.20 short DataDir uses unix socket and token",
			env:      pathsEnv{goos: "windows", dataDir: `C:\gvxd`, tempDir: `C:\Temp`},
			spec:     Spec{ID: shortID},
			feat:     rungv.Features{Control: true},
			wantCtrl: true,
			wantSock: `C:\gvxd\run\abcd1234.sock`,
			wantTok:  `C:\gvxd\run\abcd1234.sock.token`,
		},
		{
			name:     "windows v0.20 long DataDir falls back to TEMP\\gvxd",
			env:      pathsEnv{goos: "windows", dataDir: winLongDataDir, tempDir: `C:\Temp`},
			spec:     Spec{ID: longID, Checkpoint: true},
			feat:     rungv.Features{Control: true},
			wantCtrl: true,
			wantCkpt: true,
			wantSock: `C:\Temp\gvxd\` + sockName(longID) + ".sock",
			wantTok:  `C:\Temp\gvxd\` + sockName(longID) + ".sock.token",
			maxLen:   maxSocketPath,
			wantHash: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p, err := computePaths(tc.env, tc.spec, tc.feat, "/snap/dir")
			if err != nil {
				t.Fatalf("computePaths: %v", err)
			}
			if p.Control != tc.wantCtrl {
				t.Errorf("Control = %v, want %v", p.Control, tc.wantCtrl)
			}
			if (p.CheckpointDir != "") != tc.wantCkpt {
				t.Errorf("CheckpointDir = %q, want non-empty=%v", p.CheckpointDir, tc.wantCkpt)
			}
			if p.ControlSocket != tc.wantSock {
				t.Errorf("ControlSocket = %q, want %q", p.ControlSocket, tc.wantSock)
			}
			if p.TokenFile != tc.wantTok {
				t.Errorf("TokenFile = %q, want %q", p.TokenFile, tc.wantTok)
			}
			if tc.maxLen > 0 && len(p.ControlSocket) > tc.maxLen {
				t.Errorf("ControlSocket len=%d exceeds %d: %q", len(p.ControlSocket), tc.maxLen, p.ControlSocket)
			}
			if tc.wantHash && len(sockName(tc.spec.ID)) != 16 {
				t.Errorf("sockName(%q) = %q, want 16 hex chars", tc.spec.ID, sockName(tc.spec.ID))
			}
		})
	}
}

func TestEnsurePrivateSocketDirUnix(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("unix permissions test")
	}
	base := t.TempDir()

	// Fresh directory is created 0700 and passes.
	okDir := filepath.Join(base, "ok")
	if err := ensurePrivateSocketDir(okDir); err != nil {
		t.Fatalf("fresh dir: %v", err)
	}
	// Second call on existing valid 0700 dir passes.
	if err := ensurePrivateSocketDir(okDir); err != nil {
		t.Fatalf("existing dir: %v", err)
	}

	// Symlink must fail and error must name the directory.
	symDir := filepath.Join(base, "sym")
	if err := os.Symlink(okDir, symDir); err != nil {
		t.Fatal(err)
	}
	if err := ensurePrivateSocketDir(symDir); err == nil || !strings.Contains(err.Error(), symDir) {
		t.Fatalf("symlink accepted or unnamed error: %v", err)
	}

	// Regular file must fail.
	regPath := filepath.Join(base, "reg")
	if err := os.WriteFile(regPath, nil, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := ensurePrivateSocketDir(regPath); err == nil {
		t.Fatal("regular file accepted as socket dir")
	}

	// Loose permissions (0755) must fail.
	looseDir := filepath.Join(base, "loose")
	if err := os.Mkdir(looseDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(looseDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := ensurePrivateSocketDir(looseDir); err == nil || !strings.Contains(err.Error(), looseDir) {
		t.Fatalf("0755 dir accepted: %v", err)
	}
}

func TestRemoveSocketAndTokenSafeLstat(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("unix lstat test")
	}
	dir := t.TempDir()
	// Regular file must NOT be deleted by removeSocketFile.
	reg := filepath.Join(dir, "keep.txt")
	if err := os.WriteFile(reg, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	removeSocketFile(reg)
	if _, err := os.Stat(reg); err != nil {
		t.Fatalf("removeSocketFile deleted regular file: %v", err)
	}

	// Symlink must NOT be deleted by removeTokenFile.
	sym := filepath.Join(dir, "keep.token")
	if err := os.Symlink(reg, sym); err != nil {
		t.Fatal(err)
	}
	removeTokenFile(sym)
	if _, err := os.Lstat(sym); err != nil {
		t.Fatalf("removeTokenFile deleted symlink: %v", err)
	}

	// Real regular token file IS deleted.
	tok := filepath.Join(dir, "real.token")
	if err := os.WriteFile(tok, []byte("secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	removeTokenFile(tok)
	if _, err := os.Lstat(tok); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("removeTokenFile left regular file: %v", err)
	}

	// Real Unix socket IS deleted.
	sockPath := filepath.Join("/tmp", "gvxd-test-"+NewID()+".sock")
	ln, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Fatalf("listen unix: %v", err)
	}
	_ = ln.Close()
	// Re-bind and unlink manually if Go unlinked on Close; create via net.Listen + SetUnlinkOnClose(false).
	ln2, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Fatalf("listen unix 2: %v", err)
	}
	ln2.(*net.UnixListener).SetUnlinkOnClose(false)
	_ = ln2.Close()
	t.Cleanup(func() { _ = os.Remove(sockPath) })
	removeSocketFile(sockPath)
	if _, err := os.Lstat(sockPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("removeSocketFile left socket: %v", err)
	}
}

func TestLiveUpdateTable(t *testing.T) {
	base := Spec{
		ID:         "12345678",
		Name:       "app",
		Image:      "alpine:3.20",
		Memory:     "512m",
		CPUs:       "1",
		IsolateLAN: false,
		Ports:      []string{"8080:80"},
		Env:        []string{"A=1"},
		CreatedAt:  time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
	}

	t.Run("no change or only AutoStart and CreatedAt", func(t *testing.T) {
		edited := base.Clone()
		edited.AutoStart = true
		edited.CreatedAt = time.Date(2026, 11, 22, 0, 0, 0, 0, time.UTC)
		u, live := LiveUpdate(base, edited)
		if !live || !u.IsZero() {
			t.Fatalf("got (%+v, %v), want (zero, true)", u, live)
		}
	})

	t.Run("memory, cpus, isolateLAN changed", func(t *testing.T) {
		edited := base.Clone()
		edited.Memory = "1g"
		edited.CPUs = "2.5"
		edited.IsolateLAN = true
		u, live := LiveUpdate(base, edited)
		if !live || u.Memory != "1g" || u.CPUs != "2.5" || u.IsolateLAN == nil || !*u.IsolateLAN {
			t.Fatalf("got (%+v, %v)", u, live)
		}
	})

	t.Run("cpus cleared becomes 0", func(t *testing.T) {
		edited := base.Clone()
		edited.CPUs = ""
		u, live := LiveUpdate(base, edited)
		if !live || u.CPUs != "0" {
			t.Fatalf("got (%+v, %v), want CPUs=0 live=true", u, live)
		}
	})

	t.Run("memory cleared is not live", func(t *testing.T) {
		edited := base.Clone()
		edited.Memory = ""
		_, live := LiveUpdate(base, edited)
		if live {
			t.Fatal("clearing Memory should not be live")
		}
	})

	nonLiveEdits := map[string]func(*Spec){
		"ports":      func(s *Spec) { s.Ports = append(s.Ports, "9090:90") },
		"mounts":     func(s *Spec) { s.Mounts = []Mount{{Type: MountTmpfs, Target: "/tmp"}} },
		"image":      func(s *Spec) { s.Image = "alpine:3.21" },
		"env":        func(s *Spec) { s.Env = []string{"A=2"} },
		"name":       func(s *Spec) { s.Name = "other" },
		"network":    func(s *Spec) { s.Network = "none" },
		"user":       func(s *Spec) { s.User = "1000" },
		"workdir":    func(s *Spec) { s.Workdir = "/app" },
		"extraArgs":  func(s *Spec) { s.ExtraArgs = "--tune a=b" },
		"checkpoint": func(s *Spec) { s.Checkpoint = true },
	}
	for name, mutate := range nonLiveEdits {
		t.Run("non-live "+name, func(t *testing.T) {
			edited := base.Clone()
			mutate(&edited)
			if _, live := LiveUpdate(base, edited); live {
				t.Fatalf("change to %s should not be live", name)
			}
		})
	}
}

func TestSupervisorControlLifecycleWithFakeRungv(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell script fake rungv")
	}
	dir := t.TempDir()
	stateFile := filepath.Join(dir, "state")
	stopMarker := filepath.Join(dir, "stopped")
	runArgsFile := filepath.Join(dir, "run.args")

	// Fake v0.20 rungv supporting run -help, run, pause, unpause, update, stop, exec, kill.
	bin := filepath.Join(dir, "rungv")
	t.Cleanup(func() { rungv.ForgetFeatures(bin) })
	script := `#!/bin/sh
if [ "$1" = "run" ] && [ "$2" = "-help" ]; then
  echo "  -control-socket string"
  echo "  -control-token-file string"
  echo "  -dynamic-ports"
  exit 2
fi
cmd="$1"
shift
case "$cmd" in
  run)
    printf '%s\n' "$*" > "` + runArgsFile + `"
    echo "running" > "` + stateFile + `"
    # Wait until stop creates stopMarker or SIGINT/SIGTERM arrives.
    trap 'exit 0' INT TERM
    while [ ! -f "` + stopMarker + `" ]; do
      sleep 0.05
    done
    exit 0
    ;;
  pause)
    if [ "$(cat "` + stateFile + `" 2>/dev/null)" = "paused" ]; then
      echo "rungv pause: ALREADY_PAUSED: already paused" >&2
      exit 1
    fi
    echo "paused" > "` + stateFile + `"
    exit 0
    ;;
  unpause)
    if [ "$(cat "` + stateFile + `" 2>/dev/null)" != "paused" ]; then
      echo "rungv unpause: NOT_PAUSED: not paused" >&2
      exit 1
    fi
    echo "running" > "` + stateFile + `"
    exit 0
    ;;
  update)
    echo '{"memory_bytes":268435456,"cpu_millis":1500,"isolate_lan":true}'
    exit 0
    ;;
  stop)
    touch "` + stopMarker + `"
    exit 0
    ;;
  kill)
    exit 0
    ;;
  exec)
    pidfile=""
    prev=""
    for a in "$@"; do
      if [ "$prev" = "--pid-file" ]; then pidfile="$a"; fi
      prev="$a"
    done
    case "$*" in
      *fail-before-start*)
        echo "rungv exec: NOT_FOUND: executable not found" >&2
        exit 1
        ;;
      *fail-after-start*)
        [ -n "$pidfile" ] && echo "42" > "$pidfile"
        echo "rungv exec: SANDBOX_EXITING: connection closed" >&2
        exit 1
        ;;
      *exit-code-7*)
        [ -n "$pidfile" ] && echo "43" > "$pidfile"
        echo "out-line"
        echo "err-line" >&2
        exit 7
        ;;
      *echo-stdin*)
        [ -n "$pidfile" ] && echo "44" > "$pidfile"
        cat
        exit 0
        ;;
      *sleep-long*)
        [ -n "$pidfile" ] && echo "45" > "$pidfile"
        sleep 30
        exit 0
        ;;
    esac
    [ -n "$pidfile" ] && echo "41" > "$pidfile"
    echo "ok"
    exit 0
    ;;
esac
`
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}

	s, _ := newTestSupervisor(t, &rungv.Client{Path: bin})
	spec := Spec{ID: "fakev20", Name: "gvxd-fake", Image: "alpine:3.20", Memory: "512m"}
	if err := s.Start(spec); err != nil {
		t.Fatalf("Start: %v", err)
	}
	waitFor(t, "running", 5*time.Second, func() bool { return s.State(spec.ID).Status == Running })

	st := s.State(spec.ID)
	if !st.Control {
		t.Fatalf("State.Control = false, want true")
	}
	ctl, ok := s.Control(spec.ID)
	if !ok || !ctl.Target().Valid() {
		t.Fatalf("Control(%s) = (%v, %v)", spec.ID, ctl, ok)
	}
	argv := ctl.ExecArgv(rungv.ExecOptions{Argv: []string{"/bin/sh"}, Interactive: true, TTY: true})
	if len(argv) < 2 || argv[0] != bin || argv[1] != "exec" {
		t.Fatalf("ExecArgv = %q", argv)
	}

	// Non-TTY exec: streams apart, guest exit code 7, PID > 0.
	ep, err := ctl.Exec(context.Background(), ExecOptions{
		ExecOptions: rungv.ExecOptions{Argv: []string{"exit-code-7"}},
	})
	if err != nil {
		t.Fatalf("Exec exit-code-7: %v", err)
	}
	if ep.Stdin() != nil {
		t.Fatal("non-interactive exec returned non-nil Stdin")
	}
	outBytes, _ := io.ReadAll(ep.Stdout())
	errBytes, _ := io.ReadAll(ep.Stderr())
	code, werr := ep.Wait()
	_ = ep.Close()
	if werr != nil || code != 7 || ep.PID() != 43 {
		t.Fatalf("Wait = (%d, %v), PID = %d", code, werr, ep.PID())
	}
	if strings.TrimSpace(string(outBytes)) != "out-line" || strings.TrimSpace(string(errBytes)) != "err-line" {
		t.Fatalf("stdout=%q stderr=%q", outBytes, errBytes)
	}

	// Interactive pipe exec: stdin -> stdout, Close -> EOF.
	epCat, err := ctl.Exec(context.Background(), ExecOptions{
		ExecOptions: rungv.ExecOptions{Argv: []string{"echo-stdin"}, Interactive: true},
	})
	if err != nil {
		t.Fatalf("Exec echo-stdin: %v", err)
	}
	if _, err := epCat.Stdin().Write([]byte("ping-stdin\n")); err != nil {
		t.Fatal(err)
	}
	if err := epCat.Stdin().Close(); err != nil {
		t.Fatal(err)
	}
	catOut, _ := io.ReadAll(epCat.Stdout())
	code, werr = epCat.Wait()
	_ = epCat.Close()
	if werr != nil || code != 0 || string(catOut) != "ping-stdin\n" {
		t.Fatalf("cat: code=%d err=%v out=%q", code, werr, catOut)
	}

	// Exec failing before start -> PID == 0, (-1, *rungv.Error with NOT_FOUND).
	epFail, err := ctl.Exec(context.Background(), ExecOptions{
		ExecOptions: rungv.ExecOptions{Argv: []string{"fail-before-start"}},
	})
	if err != nil {
		t.Fatalf("Exec fail-before-start: %v", err)
	}
	code, werr = epFail.Wait()
	_ = epFail.Close()
	if code != -1 || werr == nil || epFail.PID() != 0 || rungv.ControlCode(werr) != rungv.CodeNotFound {
		t.Fatalf("fail-before-start: code=%d err=%v pid=%d codeStr=%q", code, werr, epFail.PID(), rungv.ControlCode(werr))
	}

	// Exec failing after start with rungv exec: line -> (-1, *rungv.Error).
	epMid, err := ctl.Exec(context.Background(), ExecOptions{
		ExecOptions: rungv.ExecOptions{Argv: []string{"fail-after-start"}},
	})
	if err != nil {
		t.Fatalf("Exec fail-after-start: %v", err)
	}
	code, werr = epMid.Wait()
	_ = epMid.Close()
	if code != -1 || werr == nil || epMid.PID() != 42 || rungv.ControlCode(werr) != rungv.CodeSandboxExiting {
		t.Fatalf("fail-after-start: code=%d err=%v pid=%d codeStr=%q", code, werr, epMid.PID(), rungv.ControlCode(werr))
	}

	// Long-running exec: Started closes with PID, Signal works, Close terminates client cleanly.
	epLong, err := ctl.Exec(context.Background(), ExecOptions{
		ExecOptions: rungv.ExecOptions{Argv: []string{"sleep-long"}},
	})
	if err != nil {
		t.Fatalf("Exec sleep-long: %v", err)
	}
	select {
	case <-epLong.Started():
	case <-time.After(3 * time.Second):
		t.Fatal("Started did not close")
	}
	if epLong.PID() != 45 {
		t.Fatalf("PID = %d, want 45", epLong.PID())
	}
	if err := epLong.Signal(context.Background(), "SIGTERM"); err != nil {
		t.Fatalf("Signal: %v", err)
	}
	if err := epLong.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	// Close is idempotent.
	if err := epLong.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}

	// Pause -> State.Paused true, idempotent on ALREADY_PAUSED, Exec -> ErrPaused, Checkpoint refused.
	if err := s.Pause(context.Background(), spec.ID); err != nil {
		t.Fatalf("Pause: %v", err)
	}
	if !s.State(spec.ID).Paused {
		t.Fatal("State.Paused not true after Pause")
	}
	if err := s.Pause(context.Background(), spec.ID); err != nil {
		t.Fatalf("second Pause (ALREADY_PAUSED) should succeed: %v", err)
	}
	if _, err := ctl.Exec(context.Background(), ExecOptions{ExecOptions: rungv.ExecOptions{Argv: []string{"ok"}}}); !errors.Is(err, ErrPaused) {
		t.Fatalf("Exec while paused = %v, want ErrPaused", err)
	}

	// Unpause -> State.Paused false, idempotent on NOT_PAUSED.
	if err := s.Unpause(context.Background(), spec.ID); err != nil {
		t.Fatalf("Unpause: %v", err)
	}
	if s.State(spec.ID).Paused {
		t.Fatal("State.Paused still true after Unpause")
	}
	if err := s.Unpause(context.Background(), spec.ID); err != nil {
		t.Fatalf("second Unpause (NOT_PAUSED) should succeed: %v", err)
	}

	// Update copies Memory, CPUs, IsolateLAN into ActiveSpec and logs a System line.
	iso := true
	if _, err := s.Update(context.Background(), spec.ID, rungv.Update{Memory: "256m", CPUs: "1.5", IsolateLAN: &iso}); err != nil {
		t.Fatalf("Update: %v", err)
	}
	active, ok := s.ActiveSpec(spec.ID)
	if !ok || active.Memory != "256m" || active.CPUs != "1.5" || !active.IsolateLAN {
		t.Fatalf("ActiveSpec after Update = %+v (ok=%v)", active, ok)
	}
	var sawUpdateLog bool
	for _, l := range s.Logs(spec.ID).Snapshot() {
		if l.Stream == System && strings.Contains(l.Text, "── Updated: memory 256m, cpus 1.5") {
			sawUpdateLog = true
		}
	}
	if !sawUpdateLog {
		t.Fatal("missing System update log line")
	}

	// Pause again and call Shutdown: must unpause, call stop, and end Stopped with Paused=false.
	if err := s.Pause(context.Background(), spec.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.Shutdown(spec.ID, "SIGTERM", 2*time.Second); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
	final := s.State(spec.ID)
	if final.Status != Stopped || final.Paused {
		t.Fatalf("final state after Shutdown = %+v", final)
	}
	if _, ok := s.Control(spec.ID); ok {
		t.Fatal("Control returned true after exit")
	}
}

func TestSupervisorV019FallbackNoControl(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell script fake rungv")
	}
	dir := t.TempDir()
	argsFile := filepath.Join(dir, "args")
	bin := filepath.Join(dir, "rungv")
	t.Cleanup(func() { rungv.ForgetFeatures(bin) })
	script := "#!/bin/sh\nif [ \"$1\" = \"run\" ] && [ \"$2\" = \"-help\" ]; then echo '  -control-socket string'; exit 2; fi\n" +
		"printf '%s\\n' \"$*\" > \"" + argsFile + "\"\nexec sleep 30\n"
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}

	s, _ := newTestSupervisor(t, &rungv.Client{Path: bin})
	spec := Spec{ID: "v19id", Name: "gvxd-v19", Image: "alpine:3.20"}
	if err := s.Start(spec); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "running", 5*time.Second, func() bool { return s.State(spec.ID).Status == Running })

	if st := s.State(spec.ID); st.Control {
		t.Fatalf("v0.19 run had State.Control=true")
	}
	if _, ok := s.Control(spec.ID); ok {
		t.Fatal("v0.19 run returned Control")
	}
	if err := s.Pause(context.Background(), spec.ID); !errors.Is(err, ErrNoControl) {
		t.Fatalf("Pause on v0.19 = %v, want ErrNoControl", err)
	}
	if err := s.Unpause(context.Background(), spec.ID); !errors.Is(err, ErrNoControl) {
		t.Fatalf("Unpause on v0.19 = %v, want ErrNoControl", err)
	}
	if _, err := s.Update(context.Background(), spec.ID, rungv.Update{Memory: "256m"}); !errors.Is(err, ErrNoControl) {
		t.Fatalf("Update on v0.19 = %v, want ErrNoControl", err)
	}
	// Verify no -control-socket or --control-token-file was passed.
	waitFor(t, "args file", 5*time.Second, func() bool {
		_, err := os.Stat(argsFile)
		return err == nil
	})
	rawArgs, _ := os.ReadFile(argsFile)
	if bytes.Contains(rawArgs, []byte("control-socket")) || bytes.Contains(rawArgs, []byte("control-token-file")) {
		t.Fatalf("v0.19 got control flags: %q", rawArgs)
	}

	// Shutdown falls back to Stop.
	if err := s.Shutdown(spec.ID, "SIGTERM", 2*time.Second); err != nil {
		t.Fatalf("Shutdown on v0.19: %v", err)
	}
	if st := s.State(spec.ID); st.Status != Stopped {
		t.Fatalf("state after Shutdown = %+v", st)
	}
}

func TestRedactArgsAndSupervisorLogRedaction(t *testing.T) {
	in := []string{
		"run", "-e", "GVX_NET=wg-private-key-secret", "-e", "GVX_NET=", "-e", "NORMAL=ok",
		"--env=GVX_NET=secret2", "--token", "tok1", "--token=tok2", "-token", "tok3", "-token=tok4", "alpine",
	}
	orig := append([]string(nil), in...)
	got := redactArgs(in)
	if !reflect.DeepEqual(in, orig) {
		t.Fatalf("redactArgs mutated input: %q", in)
	}
	want := []string{
		"run", "-e", "GVX_NET=<redacted>", "-e", "GVX_NET=", "-e", "NORMAL=ok",
		"--env=GVX_NET=<redacted>", "--token", "<redacted>", "--token=<redacted>", "-token", "<redacted>", "-token=<redacted>", "alpine",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("redactArgs:\n got  %q\n want %q", got, want)
	}

	if runtime.GOOS == "windows" {
		return
	}
	dir := t.TempDir()
	argsFile := filepath.Join(dir, "args")
	bin := filepath.Join(dir, "rungv")
	t.Cleanup(func() { rungv.ForgetFeatures(bin) })
	script := "#!/bin/sh\nif [ \"$1\" = \"run\" ] && [ \"$2\" = \"-help\" ]; then echo '  -control-socket string'; exit 2; fi\n" +
		"printf '%s\\n' \"$*\" > \"" + argsFile + "\"\n" +
		"echo 'hello-line'\n" +
		"exec sleep 30\n"
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}

	s, events := newTestSupervisor(t, &rungv.Client{Path: bin})
	spec := Spec{
		ID:      "redact1",
		Name:    "gvxd-redact",
		Image:   "alpine:3.20",
		Env:     []string{"GVX_NET=super-secret-wg-key", "PLAIN=visible"},
		Command: "sleep 30",
	}
	if err := s.Start(spec); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "hello-line", 5*time.Second, func() bool {
		for _, l := range s.Logs(spec.ID).Snapshot() {
			if l.Text == "hello-line" {
				return true
			}
		}
		return false
	})

	// Logs must contain redacted GVX_NET and must NOT contain the secret.
	var sawRedacted bool
	for _, l := range s.Logs(spec.ID).Snapshot() {
		if strings.Contains(l.Text, "super-secret-wg-key") {
			t.Fatalf("secret leaked into log buffer: %q", l.Text)
		}
		if strings.Contains(l.Text, "GVX_NET=<redacted>") {
			sawRedacted = true
		}
	}
	if !sawRedacted {
		t.Fatalf("expected GVX_NET=<redacted> in logs: %+v", s.Logs(spec.ID).Snapshot())
	}

	// Real rungv process still received the unredacted GVX_NET value.
	rawArgs, err := os.ReadFile(argsFile)
	if err != nil || !strings.Contains(string(rawArgs), "GVX_NET=super-secret-wg-key") {
		t.Fatalf("rungv did not receive real GVX_NET: %q err=%v", rawArgs, err)
	}

	// Stop emits EventChanged with populated State (non-zero PID / Stopping),
	// and pump's EventChanged also had populated State (non-zero PID).
	if err := s.Stop(spec.ID, 5*time.Second); err != nil {
		t.Fatal(err)
	}
	var sawPumpPopulated, sawStopPopulated bool
	for len(events) > 0 {
		ev := <-events
		if ev.Kind == EventChanged {
			if ev.State.Status == Running && ev.State.PID > 0 && ev.State.LogSeq > 0 {
				sawPumpPopulated = true
			}
			if ev.State.Status == Stopping && ev.State.PID > 0 {
				sawStopPopulated = true
			}
		}
	}
	if !sawPumpPopulated {
		t.Error("expected EventChanged from pump to carry populated State (Status=Running, PID>0)")
	}
	if !sawStopPopulated {
		t.Error("expected EventChanged from Stop to carry populated State (Status=Stopping, PID>0)")
	}
}
