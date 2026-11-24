package ui

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ironpark/ggui"

	"github.com/ironpark/gvx-desktop/internal/engine"
	"github.com/ironpark/gvx-desktop/internal/store"
)

func TestBuildxEndpointForSocket(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"/tmp/gvxd/buildkit.sock", "unix:///tmp/gvxd/buildkit.sock"},
		{"tcp://127.0.0.1:23760", "tcp://127.0.0.1:23760"},
		{"unix:///tmp/already.sock", "unix:///tmp/already.sock"},
		{"npipe:////./pipe/buildkitd", "npipe:////./pipe/buildkitd"},
	}
	for _, tc := range cases {
		if got := buildxEndpointForSocket(tc.in); got != tc.want {
			t.Errorf("buildxEndpointForSocket(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestBuildxParseInspectMultiNode(t *testing.T) {
	raw := `Name:          gvx-desktop
Driver:        remote
Last Activity: 2026-11-22 00:47:15 +0000 UTC

Nodes:
Name:           gvx-desktop0
Endpoint:       unix:///tmp/gvxd/buildkit.sock
Driver Options: default-load="true"
Status:         inactive

Name:           gvx-desktop-amd64
Endpoint:       unix:///tmp/gvxd/buildkit-amd64.sock
Driver Options: default-load="true"
Status:         inactive
Platforms:      linux/amd64*
`
	info := parseBuildxInspect([]byte(raw))
	if info.Name != engine.BuilderName || info.Driver != "remote" {
		t.Fatalf("top info = %+v", info)
	}
	if info.Endpoint != "unix:///tmp/gvxd/buildkit.sock" || !info.DefaultLoad {
		t.Fatalf("node0 summary = (%q, defaultLoad=%v)", info.Endpoint, info.DefaultLoad)
	}
	if len(info.Nodes) != 2 {
		t.Fatalf("len(Nodes) = %d, want 2", len(info.Nodes))
	}
	n1, ok := info.findNode(engine.Amd64NodeName)
	if !ok {
		t.Fatalf("missing node %s", engine.Amd64NodeName)
	}
	if !nodeMatchesAmd64(n1, "unix:///tmp/gvxd/buildkit-amd64.sock") {
		t.Fatalf("nodeMatchesAmd64 = false for %+v", n1)
	}

	// Both platforms pinned must NOT match amd64-only requirement.
	both := buildxNodeInfo{
		Name:           engine.Amd64NodeName,
		Endpoint:       "unix:///tmp/gvxd/buildkit-amd64.sock",
		DefaultLoad:    true,
		Platforms:      []string{"linux/amd64", "linux/arm64"},
		FixedPlatforms: []string{"linux/amd64", "linux/arm64"},
	}
	if nodeMatchesAmd64(both, "unix:///tmp/gvxd/buildkit-amd64.sock") {
		t.Errorf("nodeMatchesAmd64 accepted both platforms; must require linux/amd64 only")
	}
}

func TestBuildxAmd64NodeAppendLeaveAndRecreate(t *testing.T) {
	t.Setenv("DOCKER_CONFIG", t.TempDir())
	t.Setenv("BUILDX_CONFIG", t.TempDir())

	sock0 := "/tmp/gvxd-test/buildkit.sock"
	sock1 := "/tmp/gvxd-test/buildkit-amd64.sock"
	ep0 := "unix://" + sock0
	ep1 := "unix://" + sock1

	t.Run("append_and_leave_args", func(t *testing.T) {
		var calls [][]string
		run := func(_ context.Context, _ string, args ...string) ([]byte, error) {
			calls = append(calls, slices.Clone(args))
			return []byte("gvx-desktop\n"), nil
		}

		present, err := configureAmd64Node(context.Background(), "/usr/local/bin/docker", run, sock1, true)
		if err != nil || !present {
			t.Fatalf("configureAmd64Node(on) = (%v, %v), want (true, nil)", present, err)
		}
		wantAppend := []string{
			"buildx", "create",
			"--append",
			"--name", engine.BuilderName,
			"--node", engine.Amd64NodeName,
			"--platform", engine.Amd64Platform,
			"--driver-opt", "default-load=true",
			ep1,
		}
		if len(calls) != 2 || !slices.Equal(calls[1], wantAppend) {
			t.Fatalf("append calls = %v, want %v", calls, wantAppend)
		}

		calls = nil
		present, err = configureAmd64Node(context.Background(), "/usr/local/bin/docker", run, sock1, false)
		if err != nil || present {
			t.Fatalf("configureAmd64Node(off) = (%v, %v), want (false, nil)", present, err)
		}
		wantLeave := []string{
			"buildx", "create",
			"--leave",
			"--name", engine.BuilderName,
			"--node", engine.Amd64NodeName,
		}
		if len(calls) != 2 || !slices.Equal(calls[1], wantLeave) {
			t.Fatalf("leave calls = %v, want %v", calls, wantLeave)
		}
	})

	t.Run("leave_missing_node_is_not_error", func(t *testing.T) {
		run := func(_ context.Context, _ string, args ...string) ([]byte, error) {
			if strings.Join(args, " ") == "buildx version" {
				return []byte("v0.39.1"), nil
			}
			return nil, os.ErrNotExist
		}
		present, err := configureAmd64Node(context.Background(), "/usr/local/bin/docker", run, sock1, false)
		if err == nil {
			t.Fatalf("expected non-matching error to surface, got nil")
		}
		if present {
			t.Fatalf("present = true on error")
		}

		runNotFound := func(_ context.Context, _ string, args ...string) ([]byte, error) {
			if strings.Join(args, " ") == "buildx version" {
				return []byte("v0.39.1"), nil
			}
			return nil, &testErr{msg: `ERROR: node "gvx-desktop-amd64" not found for gvx-desktop`}
		}
		present, err = configureAmd64Node(context.Background(), "/usr/local/bin/docker", runNotFound, sock1, false)
		if err != nil || present {
			t.Fatalf("leave missing node = (%v, %v), want (false, nil)", present, err)
		}
	})

	// Regression test for review item 3: when feature is off, configureBuildx
	// must --leave a stale amd64 node (instead of keeping it) and must NOT
	// re-append it when recreating a stale builder. When feature is on, it must
	// validate an existing node with nodeMatchesAmd64 and fix a mismatched one.
	t.Run("configureBuildx_leaves_stale_node_when_off_and_validates_when_on", func(t *testing.T) {
		// Case A: node0 valid, stale amd64 node present, feature OFF -> must --leave.
		var calls [][]string
		run := func(_ context.Context, _ string, args ...string) ([]byte, error) {
			calls = append(calls, slices.Clone(args))
			key := strings.Join(args, " ")
			switch {
			case key == "buildx version":
				return []byte("v0.39.1"), nil
			case key == "buildx inspect gvx-desktop":
				return []byte("Name: gvx-desktop\nDriver: remote\n\nNodes:\nName: gvx-desktop0\nEndpoint: " + ep0 + "\nDriver Options: default-load=\"true\"\n\nName: gvx-desktop-amd64\nEndpoint: " + ep1 + "\nDriver Options: default-load=\"true\"\nPlatforms: linux/amd64*\n"), nil
			case strings.HasPrefix(key, "buildx create --leave "):
				return []byte("gvx-desktop\n"), nil
			case key == "buildx install":
				return nil, nil
			case key == "context inspect gvx":
				return nil, os.ErrNotExist
			}
			return nil, os.ErrInvalid
		}
		st, _, _, _ := configureBuildx(context.Background(), "/usr/local/bin/docker", run, sock0, "", "")
		if !st.Ready || st.Amd64Node {
			t.Fatalf("configureBuildx(off with stale node) = %+v, want Ready=true Amd64Node=false", st)
		}
		sawLeave := false
		for _, c := range calls {
			if slices.Contains(c, "--leave") && slices.Contains(c, engine.Amd64NodeName) {
				sawLeave = true
			}
		}
		if !sawLeave {
			t.Fatalf("configureBuildx did not --leave stale amd64 node when off; calls=%v", calls)
		}

		// Case B: node0 stale (recreated), stale amd64 node present, feature OFF -> must NOT re-append.
		calls = nil
		runRecreateOff := func(_ context.Context, _ string, args ...string) ([]byte, error) {
			calls = append(calls, slices.Clone(args))
			key := strings.Join(args, " ")
			switch {
			case key == "buildx version":
				return []byte("v0.39.1"), nil
			case key == "buildx inspect gvx-desktop":
				return []byte("Name: gvx-desktop\nDriver: remote\n\nNodes:\nName: gvx-desktop0\nEndpoint: unix:///stale/old.sock\nDriver Options: default-load=\"true\"\n\nName: gvx-desktop-amd64\nEndpoint: " + ep1 + "\nDriver Options: default-load=\"true\"\nPlatforms: linux/amd64*\n"), nil
			case key == "buildx rm gvx-desktop":
				return nil, nil
			case strings.HasPrefix(key, "buildx create "):
				return []byte("gvx-desktop\n"), nil
			case key == "buildx install":
				return nil, nil
			case key == "context inspect gvx":
				return nil, os.ErrNotExist
			}
			return nil, os.ErrInvalid
		}
		st, _, _, _ = configureBuildx(context.Background(), "/usr/local/bin/docker", runRecreateOff, sock0, "", "")
		if !st.Ready || st.Amd64Node {
			t.Fatalf("configureBuildx(recreate while off) = %+v, want Ready=true Amd64Node=false", st)
		}
		for _, c := range calls {
			if slices.Contains(c, "--append") {
				t.Fatalf("configureBuildx re-appended stale amd64 node while feature is off: %v", c)
			}
		}

		// Case C: feature ON, existing amd64 node has BOTH platforms pinned (invalid) -> must re-append with linux/amd64 only.
		calls = nil
		runMismatchOn := func(_ context.Context, _ string, args ...string) ([]byte, error) {
			calls = append(calls, slices.Clone(args))
			key := strings.Join(args, " ")
			switch {
			case key == "buildx version":
				return []byte("v0.39.1"), nil
			case key == "buildx inspect gvx-desktop":
				return []byte("Name: gvx-desktop\nDriver: remote\n\nNodes:\nName: gvx-desktop0\nEndpoint: " + ep0 + "\nDriver Options: default-load=\"true\"\n\nName: gvx-desktop-amd64\nEndpoint: " + ep1 + "\nDriver Options: default-load=\"true\"\nPlatforms: linux/amd64*, linux/arm64*\n"), nil
			case strings.HasPrefix(key, "buildx create --append "):
				return []byte("gvx-desktop\n"), nil
			case key == "buildx install":
				return nil, nil
			case key == "context inspect gvx":
				return nil, os.ErrNotExist
			}
			return nil, os.ErrInvalid
		}
		st, _, _, _ = configureBuildxWithAmd64(context.Background(), "/usr/local/bin/docker", runMismatchOn, sock0, "", "", true, sock1, false)
		if !st.Ready || !st.Amd64Node {
			t.Fatalf("configureBuildxWithAmd64(mismatch on) = %+v, want Ready+Amd64Node", st)
		}
		sawFixAppend := false
		for _, c := range calls {
			if slices.Contains(c, "--append") && slices.Contains(c, engine.Amd64Platform) {
				sawFixAppend = true
			}
		}
		if !sawFixAppend {
			t.Fatalf("configureBuildxWithAmd64 did not repair mismatched amd64 node; calls=%v", calls)
		}
	})

	// Regression test for review item 5c: probeSafe mode (UseSandboxBuilder)
	// reads BUILDX_CONFIG/instances/gvx-desktop directly and must never call
	// `buildx inspect` or connect to the lazy front socket buildkit-amd64.sock.
	t.Run("probe_safe_does_not_connect_to_amd64_socket", func(t *testing.T) {
		tmpSockDir, err := os.MkdirTemp("", "gvxd-ps-")
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.RemoveAll(tmpSockDir) })
		liveSock0 := filepath.Join(tmpSockDir, "buildkit.sock")
		liveSock1 := filepath.Join(tmpSockDir, "buildkit-amd64.sock")
		ln1, err := net.Listen("unix", liveSock1)
		if err != nil {
			t.Fatal(err)
		}
		defer ln1.Close()

		var probes atomic.Int32
		go func() {
			for {
				c, err := ln1.Accept()
				if err != nil {
					return
				}
				probes.Add(1)
				_ = c.Close()
			}
		}()

		buildxCfg := t.TempDir()
		instDir := filepath.Join(buildxCfg, "instances")
		if err := os.MkdirAll(instDir, 0o700); err != nil {
			t.Fatal(err)
		}
		instJSON := `{"Name":"gvx-desktop","Driver":"remote","Nodes":[` +
			`{"Name":"gvx-desktop0","Endpoint":"unix://` + filepath.ToSlash(liveSock0) + `","Platforms":[],"DriverOpts":{"default-load":"true"}},` +
			`{"Name":"gvx-desktop-amd64","Endpoint":"unix://` + filepath.ToSlash(liveSock1) + `","Platforms":[{"architecture":"amd64","os":"linux"}],"DriverOpts":{"default-load":"true"}}` +
			`]}`
		if err := os.WriteFile(filepath.Join(instDir, engine.BuilderName), []byte(instJSON), 0o600); err != nil {
			t.Fatal(err)
		}
		t.Setenv("BUILDX_CONFIG", buildxCfg)
		t.Setenv("DOCKER_CONFIG", t.TempDir())

		var calls [][]string
		run := func(_ context.Context, _ string, args ...string) ([]byte, error) {
			calls = append(calls, slices.Clone(args))
			key := strings.Join(args, " ")
			switch {
			case key == "buildx version":
				return []byte("v0.39.1"), nil
			case key == "buildx install":
				return nil, nil
			case key == "context inspect gvx":
				return []byte("[{}]"), nil
			case strings.HasPrefix(key, "--context gvx buildx use --default "):
				return nil, nil
			}
			return nil, os.ErrInvalid
		}

		st, _, _, _ := configureBuildxWithAmd64(context.Background(), "/usr/local/bin/docker", run, liveSock0, "", "", true, liveSock1, true)
		if !st.Ready || !st.Selected || !st.Amd64Node {
			t.Fatalf("probeSafe configureBuildxWithAmd64 = %+v, want Ready+Selected+Amd64Node", st)
		}
		for _, c := range calls {
			if slices.Contains(c, "inspect") && slices.Contains(c, "buildx") {
				t.Errorf("probeSafe mode ran buildx inspect: %v", c)
			}
			if slices.Contains(c, "--append") {
				t.Errorf("probeSafe mode unnecessarily ran --append on already-valid node: %v", c)
			}
		}
		if probes.Load() != 0 {
			t.Fatalf("probeSafe mode connected to buildkit-amd64.sock %d time(s), want 0", probes.Load())
		}
	})

	t.Run("real_buildx_append_and_leave", func(t *testing.T) {
		cli, ok := findCLI("docker", "")
		if !ok {
			t.Skip("docker CLI not installed")
		}
		dockerCfg := t.TempDir()
		buildxCfg := t.TempDir()
		candidates := []string{
			"/opt/homebrew/lib/docker/cli-plugins/docker-buildx",
			"/usr/local/lib/docker/cli-plugins/docker-buildx",
		}
		if home, err := os.UserHomeDir(); err == nil && home != "" {
			candidates = append([]string{filepath.Join(home, ".docker", "cli-plugins", "docker-buildx")}, candidates...)
		}
		for _, cand := range candidates {
			if target, err := filepath.EvalSymlinks(cand); err == nil {
				_ = os.MkdirAll(filepath.Join(dockerCfg, "cli-plugins"), 0o700)
				_ = os.Symlink(target, filepath.Join(dockerCfg, "cli-plugins", "docker-buildx"))
				break
			}
		}
		t.Setenv("DOCKER_CONFIG", dockerCfg)
		t.Setenv("BUILDX_CONFIG", buildxCfg)
		if _, err := runCLI(context.Background(), cli, "buildx", "version"); err != nil {
			t.Skipf("buildx not available: %v", err)
		}

		st, _, _, _ := configureBuildx(context.Background(), cli, runCLI, sock0, "", "")
		if !st.Ready {
			t.Fatalf("configureBuildx = %+v", st)
		}
		present, err := configureAmd64Node(context.Background(), cli, runCLI, sock1, true)
		if err != nil || !present {
			t.Fatalf("real append = (%v, %v)", present, err)
		}
		out, err := runCLI(context.Background(), cli, "buildx", "inspect", engine.BuilderName)
		if err != nil {
			t.Fatalf("inspect after append: %v", err)
		}
		info := parseBuildxInspect(out)
		n1, ok := info.findNode(engine.Amd64NodeName)
		if !ok || !nodeMatchesAmd64(n1, ep1) {
			t.Fatalf("inspected node mismatch: ok=%v node=%+v\nraw:\n%s", ok, n1, string(out))
		}

		// readBuildxInstance sees the exact same node without probing.
		diskInfo, ok := readBuildxInstance(engine.BuilderName)
		if !ok {
			t.Fatalf("readBuildxInstance(%s) returned false", engine.BuilderName)
		}
		dn1, ok := diskInfo.findNode(engine.Amd64NodeName)
		if !ok || !nodeMatchesAmd64(dn1, ep1) {
			t.Fatalf("readBuildxInstance node mismatch: ok=%v node=%+v", ok, dn1)
		}

		present, err = configureAmd64Node(context.Background(), cli, runCLI, sock1, false)
		if err != nil || present {
			t.Fatalf("real leave = (%v, %v)", present, err)
		}
		out, err = runCLI(context.Background(), cli, "buildx", "inspect", engine.BuilderName)
		if err != nil {
			t.Fatalf("inspect after leave: %v", err)
		}
		info = parseBuildxInspect(out)
		if _, ok := info.findNode(engine.Amd64NodeName); ok {
			t.Fatalf("amd64 node still present after leave:\n%s", string(out))
		}
		if len(info.Nodes) != 1 {
			t.Fatalf("expected node 0 preserved after leave, got %d nodes:\n%s", len(info.Nodes), string(out))
		}
	})
}

// Regression test for review item 3: fast ON -> OFF toggle in syncAmd64Node
// uses a generation counter and reads coherent Settings under the mutex so the
// node can never end up appended while the feature is off.
func TestBuildxSyncAmd64NodeFastToggleOnOff(t *testing.T) {
	t.Setenv("DOCKER_CONFIG", t.TempDir())
	t.Setenv("BUILDX_CONFIG", t.TempDir())

	dir := t.TempDir()
	st := store.Store{Dir: dir}
	set := store.DefaultSettings()
	set.Language = LangEnglish
	set.SandboxEngine = true
	set.Amd64Builds = true
	// Point DockerPath at an existing executable so findCLI succeeds in tests.
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	set.DockerPath = exe
	_ = st.SaveSettings(set)

	var m *Model
	p := ggui.ProbeBuilder(func() ggui.Widget { return ggui.Provide(ggui.ReducedMotionKey, true, Build(m)) }, ggui.Sz(800, 600))
	p.Setup(func() {
		m = NewModel(st, dir)
		m.Attach(p)
	})
	defer p.Close()
	p.Frame()

	m.sandboxEngine = &fakeSandboxEngine{}
	m.BuildxStatus.Set(BuildxStatus{Checked: true, Ready: true, Selected: true})

	var (
		mu       sync.Mutex
		ops      []string
		leaveRan = make(chan struct{})
	)
	m.runCLI = func(_ context.Context, _ string, args ...string) ([]byte, error) {
		key := strings.Join(args, " ")
		if key == "buildx version" {
			return []byte("v0.39.1"), nil
		}
		mu.Lock()
		if slices.Contains(args, "--append") {
			ops = append(ops, "append")
		} else if slices.Contains(args, "--leave") {
			ops = append(ops, "leave")
			select {
			case <-leaveRan:
			default:
				close(leaveRan)
			}
		}
		mu.Unlock()
		return []byte("gvx-desktop\n"), nil
	}

	// Hold amd64BuildxMu briefly so ON and OFF queue up concurrently, simulating
	// a fast ON -> OFF toggle before the first CLI command acquires the lock.
	amd64BuildxMu.Lock()
	m.syncAmd64Node(true)
	m.UpdateSettings(func(s *store.Settings) { s.Amd64Builds = false })
	m.syncAmd64Node(false)
	amd64BuildxMu.Unlock()

	deadline := time.Now().Add(3 * time.Second)
	for {
		select {
		case <-leaveRan:
			goto doneWait
		default:
		}
		if time.Now().After(deadline) {
			t.Fatal("timed out waiting for --leave to run")
		}
		p.Frame()
		time.Sleep(10 * time.Millisecond)
	}
doneWait:
	// Pump frames so callbacks settle.
	for range 10 {
		p.Frame()
		time.Sleep(10 * time.Millisecond)
	}

	mu.Lock()
	gotOps := slices.Clone(ops)
	mu.Unlock()
	if len(gotOps) == 0 || gotOps[len(gotOps)-1] != "leave" {
		t.Fatalf("final buildx op after fast ON->OFF = %v, want last op to be leave (and no stale append)", gotOps)
	}
	if slices.Contains(gotOps, "append") {
		t.Errorf("stale ON goroutine ran --append after OFF superseded it: ops=%v", gotOps)
	}
	if ggui.Peek(m.BuildxStatus).Amd64Node {
		t.Fatalf("BuildxStatus.Amd64Node = true after fast ON->OFF, want false")
	}
}

type testErr struct{ msg string }

func (e *testErr) Error() string { return e.msg }
