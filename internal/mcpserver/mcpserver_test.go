package mcpserver

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type mockBackend struct {
	mu sync.Mutex

	sandboxes  map[string]SandboxInfo
	snapshots  []SnapshotInfo
	containers []ContainerInfo
	images     []ImageInfo
	routes     []RouteInfo

	lastCreateReq    CreateSandboxRequest
	lastExecTarget   string
	lastExecCmd      []string
	lastExecWorkdir  string
	lastExecEnv      []string
	lastLogTail      int
	lastLeaveRunning *bool
	lastListAll      *bool
	lastImageSource  string
	lastSnapFilter   string

	execResult    ExecResult
	sandboxLogs   string
	containerLogs string

	failTool map[string]error
}

func newMockBackend() *mockBackend {
	now := time.Date(2026, 11, 24, 9, 0, 0, 0, time.UTC)
	return &mockBackend{
		sandboxes: map[string]SandboxInfo{
			"sb-1": {
				ID:         "sb-1",
				Name:       "alpha",
				Image:      "python:3.12-slim",
				Status:     "running",
				Checkpoint: true,
				Ports:      []string{"8080:80"},
				Recipe:     "python-web",
				LocalURL:   "https://alpha.localhost",
			},
		},
		snapshots: []SnapshotInfo{
			{
				SandboxID:   "sb-1",
				SandboxName: "alpha",
				Name:        "snap-init",
				Dir:         "/var/lib/gvx/snapshots/sb-1/snap-init",
				Size:        4096,
				CreatedAt:   now,
			},
		},
		containers: []ContainerInfo{
			{
				ID:       "abcdef1234567890",
				ShortID:  "abcdef123456",
				Name:     "web",
				Image:    "nginx:alpine",
				State:    "running",
				Status:   "Up 10 minutes",
				Ports:    []string{"8081:80"},
				Project:  "demo",
				Service:  "web",
				LocalURL: "https://web.localhost",
			},
			{
				ID:      "9876543210fedcba",
				ShortID: "9876543210fe",
				Name:    "worker",
				Image:   "busybox:latest",
				State:   "exited",
				Status:  "Exited (0) 2 minutes ago",
			},
		},
		images: []ImageInfo{
			{
				Source:     "cached",
				Repository: "python",
				Tag:        "3.12-slim",
				ID:         "sha256:111111",
				Size:       50_000_000,
				SizeText:   "50.0 MB",
				Platforms:  []string{"linux/arm64"},
			},
			{
				Source:     "registry",
				Repository: "local/app",
				Tag:        "v1",
				Digest:     "sha256:222222",
				Size:       12_000_000,
				SizeText:   "12.0 MB",
				Platforms:  []string{"linux/arm64", "linux/amd64"},
			},
		},
		routes: []RouteInfo{
			{
				Host:     "alpha.localhost",
				Target:   "127.0.0.1:8080",
				Kind:     "sandbox",
				Name:     "alpha",
				HTTPURL:  "http://alpha.localhost",
				HTTPSURL: "https://alpha.localhost",
			},
		},
		execResult: ExecResult{
			ExitCode: 0,
			Stdout:   "hello from sandbox\n",
			Stderr:   "",
		},
		sandboxLogs:   "line 1\nline 2\n",
		containerLogs: "nginx ready\n",
		failTool:      make(map[string]error),
	}
}

func (m *mockBackend) findSandboxLocked(target string) (string, SandboxInfo, bool) {
	if sb, ok := m.sandboxes[target]; ok {
		return target, sb, true
	}
	for id, sb := range m.sandboxes {
		if sb.Name == target {
			return id, sb, true
		}
	}
	return "", SandboxInfo{}, false
}

func (m *mockBackend) ListSandboxes(_ context.Context) ([]SandboxInfo, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.failTool["list_sandboxes"]; err != nil {
		return nil, err
	}
	out := make([]SandboxInfo, 0, len(m.sandboxes))
	for _, sb := range m.sandboxes {
		out = append(out, sb)
	}
	return out, nil
}

func (m *mockBackend) CreateSandbox(_ context.Context, req CreateSandboxRequest) (SandboxInfo, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.failTool["create_sandbox"]; err != nil {
		return SandboxInfo{}, err
	}
	m.lastCreateReq = req
	name := req.Name
	if name == "" {
		name = fmt.Sprintf("sb-%d", len(m.sandboxes)+1)
	}
	id := fmt.Sprintf("sb-%d", len(m.sandboxes)+1)
	status := "stopped"
	if req.ShouldStart() {
		status = "running"
	}
	sb := SandboxInfo{
		ID:         id,
		Name:       name,
		Image:      req.Image,
		Status:     status,
		Checkpoint: req.CheckpointEnabled(),
		Ports:      slices.Clone(req.Ports),
	}
	m.sandboxes[id] = sb
	return sb, nil
}

func (m *mockBackend) StartSandbox(_ context.Context, target string) (SandboxInfo, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.failTool["start_sandbox"]; err != nil {
		return SandboxInfo{}, err
	}
	id, sb, ok := m.findSandboxLocked(target)
	if !ok {
		return SandboxInfo{}, fmt.Errorf("sandbox %q not found", target)
	}
	sb.Status = "running"
	m.sandboxes[id] = sb
	return sb, nil
}

func (m *mockBackend) StopSandbox(_ context.Context, target string) (SandboxInfo, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.failTool["stop_sandbox"]; err != nil {
		return SandboxInfo{}, err
	}
	id, sb, ok := m.findSandboxLocked(target)
	if !ok {
		return SandboxInfo{}, fmt.Errorf("sandbox %q not found", target)
	}
	sb.Status = "stopped"
	m.sandboxes[id] = sb
	return sb, nil
}

func (m *mockBackend) DeleteSandbox(_ context.Context, target string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.failTool["delete_sandbox"]; err != nil {
		return err
	}
	id, _, ok := m.findSandboxLocked(target)
	if !ok {
		return fmt.Errorf("sandbox %q not found", target)
	}
	delete(m.sandboxes, id)
	return nil
}

func (m *mockBackend) ExecInSandbox(_ context.Context, target string, command []string, workdir string, env []string) (ExecResult, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.failTool["exec_in_sandbox"]; err != nil {
		return ExecResult{}, err
	}
	if _, _, ok := m.findSandboxLocked(target); !ok {
		return ExecResult{}, fmt.Errorf("sandbox %q not found", target)
	}
	m.lastExecTarget = target
	m.lastExecCmd = slices.Clone(command)
	m.lastExecWorkdir = workdir
	m.lastExecEnv = slices.Clone(env)
	return m.execResult, nil
}

func (m *mockBackend) SandboxLogs(_ context.Context, target string, tail int) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.failTool["sandbox_logs"]; err != nil {
		return "", err
	}
	if _, _, ok := m.findSandboxLocked(target); !ok {
		return "", fmt.Errorf("sandbox %q not found", target)
	}
	m.lastLogTail = tail
	return m.sandboxLogs, nil
}

func (m *mockBackend) CreateSnapshot(_ context.Context, target string, leaveRunning bool) (SnapshotInfo, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.failTool["create_snapshot"]; err != nil {
		return SnapshotInfo{}, err
	}
	id, sb, ok := m.findSandboxLocked(target)
	if !ok {
		return SnapshotInfo{}, fmt.Errorf("sandbox %q not found", target)
	}
	lr := leaveRunning
	m.lastLeaveRunning = &lr
	snap := SnapshotInfo{
		SandboxID:   id,
		SandboxName: sb.Name,
		Name:        fmt.Sprintf("snap-%d", len(m.snapshots)+1),
		Dir:         fmt.Sprintf("/var/lib/gvx/snapshots/%s/snap-%d", id, len(m.snapshots)+1),
		Size:        8192,
		CreatedAt:   time.Now().UTC().Truncate(time.Second),
	}
	m.snapshots = append(m.snapshots, snap)
	return snap, nil
}

func (m *mockBackend) ListSnapshots(_ context.Context, target string) ([]SnapshotInfo, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.failTool["list_snapshots"]; err != nil {
		return nil, err
	}
	m.lastSnapFilter = target
	if target == "" {
		return slices.Clone(m.snapshots), nil
	}
	var out []SnapshotInfo
	for _, s := range m.snapshots {
		if s.SandboxID == target || s.SandboxName == target {
			out = append(out, s)
		}
	}
	return out, nil
}

func (m *mockBackend) RestoreSnapshot(_ context.Context, target string, snapshot string) (SandboxInfo, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.failTool["restore_snapshot"]; err != nil {
		return SandboxInfo{}, err
	}
	id, sb, ok := m.findSandboxLocked(target)
	if !ok {
		return SandboxInfo{}, fmt.Errorf("sandbox %q not found", target)
	}
	found := false
	for _, s := range m.snapshots {
		if s.SandboxID == id && s.Name == snapshot {
			found = true
			break
		}
	}
	if !found {
		return SandboxInfo{}, fmt.Errorf("snapshot %q not found", snapshot)
	}
	sb.Status = "running"
	m.sandboxes[id] = sb
	return sb, nil
}

func (m *mockBackend) ListContainers(_ context.Context, all bool) ([]ContainerInfo, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.failTool["list_containers"]; err != nil {
		return nil, err
	}
	v := all
	m.lastListAll = &v
	if all {
		return slices.Clone(m.containers), nil
	}
	var out []ContainerInfo
	for _, c := range m.containers {
		if c.State == "running" {
			out = append(out, c)
		}
	}
	return out, nil
}

func (m *mockBackend) ContainerLogs(_ context.Context, target string, tail int) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.failTool["container_logs"]; err != nil {
		return "", err
	}
	m.lastLogTail = tail
	for _, c := range m.containers {
		if c.ID == target || c.ShortID == target || c.Name == target {
			return m.containerLogs, nil
		}
	}
	return "", fmt.Errorf("container %q not found", target)
}

func (m *mockBackend) ListImages(_ context.Context, source string) ([]ImageInfo, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.failTool["list_images"]; err != nil {
		return nil, err
	}
	m.lastImageSource = source
	if source == "all" {
		return slices.Clone(m.images), nil
	}
	var out []ImageInfo
	for _, img := range m.images {
		if img.Source == source {
			out = append(out, img)
		}
	}
	return out, nil
}

func (m *mockBackend) ListRoutes(_ context.Context) ([]RouteInfo, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.failTool["list_routes"]; err != nil {
		return nil, err
	}
	return slices.Clone(m.routes), nil
}

func startTestServer(t *testing.T, cfg Config) (*Server, *mcp.ClientSession) {
	t.Helper()
	if cfg.Port == 0 {
		cfg.Port = -1
	}
	srv := New(cfg)
	if err := srv.Start(); err != nil {
		t.Fatalf("Start() failed: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := srv.Stop(ctx); err != nil {
			t.Errorf("Stop() failed: %v", err)
		}
	})

	client := mcp.NewClient(&mcp.Implementation{Name: "test-client", Version: "1.0.0"}, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	session, err := client.Connect(ctx, &mcp.StreamableClientTransport{
		Endpoint: srv.EndpointURL(),
	}, nil)
	if err != nil {
		t.Fatalf("client.Connect() failed: %v", err)
	}
	t.Cleanup(func() {
		_ = session.Close()
	})
	return srv, session
}

func decodeStructured[T any](t *testing.T, res *mcp.CallToolResult) T {
	t.Helper()
	if res == nil {
		t.Fatalf("CallToolResult is nil")
	}
	if res.IsError {
		t.Fatalf("expected success tool result, got error: %+v", res.Content)
	}
	raw, err := json.Marshal(res.StructuredContent)
	if err != nil {
		t.Fatalf("marshal structured content: %v", err)
	}
	var out T
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("unmarshal structured content into %T: %v (raw=%s)", out, err, string(raw))
	}
	return out
}

func TestEndToEndAllTools(t *testing.T) {
	mb := newMockBackend()
	_, session := startTestServer(t, Config{
		Port:    -1,
		Version: "1.2.3",
		Backend: mb,
	})

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// 1. Verify ListTools returns all 14 tools with descriptions and schemas.
	listRes, err := session.ListTools(ctx, nil)
	if err != nil {
		t.Fatalf("ListTools failed: %v", err)
	}
	wantNames := ToolNames()
	if len(wantNames) != 14 {
		t.Fatalf("ToolNames() returned %d tools, want 14", len(wantNames))
	}
	if len(ToolSummaries()) != 14 {
		t.Fatalf("ToolSummaries() returned %d tools, want 14", len(ToolSummaries()))
	}
	gotNames := make([]string, 0, len(listRes.Tools))
	for _, tool := range listRes.Tools {
		gotNames = append(gotNames, tool.Name)
		if strings.TrimSpace(tool.Description) == "" {
			t.Errorf("tool %q has empty description", tool.Name)
		}
		if tool.InputSchema == nil {
			t.Errorf("tool %q has nil InputSchema", tool.Name)
		}
		if tool.OutputSchema == nil {
			t.Errorf("tool %q has nil OutputSchema", tool.Name)
		}
		if tool.Annotations == nil {
			t.Errorf("tool %q has nil Annotations", tool.Name)
		}
	}
	slices.Sort(gotNames)
	sortedWant := slices.Clone(wantNames)
	slices.Sort(sortedWant)
	if !slices.Equal(gotNames, sortedWant) {
		t.Fatalf("listed tools = %v, want %v", gotNames, sortedWant)
	}

	// 2. list_sandboxes
	res, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "list_sandboxes"})
	if err != nil {
		t.Fatalf("list_sandboxes: %v", err)
	}
	sbList := decodeStructured[listSandboxesOutput](t, res)
	if sbList.Count != 1 || sbList.Sandboxes[0].Name != "alpha" {
		t.Fatalf("unexpected list_sandboxes output: %+v", sbList)
	}

	// 3. create_sandbox (defaults: checkpoint=true, start=true)
	res, err = session.CallTool(ctx, &mcp.CallToolParams{
		Name: "create_sandbox",
		Arguments: map[string]any{
			"name":  "beta",
			"image": "node:22-alpine",
			"ports": []string{"3000:3000"},
		},
	})
	if err != nil {
		t.Fatalf("create_sandbox: %v", err)
	}
	created := decodeStructured[SandboxInfo](t, res)
	if created.Name != "beta" || created.Status != "running" || !created.Checkpoint {
		t.Fatalf("unexpected create_sandbox output: %+v", created)
	}
	mb.mu.Lock()
	if mb.lastCreateReq.Checkpoint == nil || !*mb.lastCreateReq.Checkpoint {
		t.Errorf("expected checkpoint default=true, got %v", mb.lastCreateReq.Checkpoint)
	}
	if mb.lastCreateReq.Start == nil || !*mb.lastCreateReq.Start {
		t.Errorf("expected start default=true, got %v", mb.lastCreateReq.Start)
	}
	mb.mu.Unlock()

	// 4. stop_sandbox
	res, err = session.CallTool(ctx, &mcp.CallToolParams{
		Name:      "stop_sandbox",
		Arguments: map[string]any{"target": "beta"},
	})
	if err != nil {
		t.Fatalf("stop_sandbox: %v", err)
	}
	stopped := decodeStructured[SandboxInfo](t, res)
	if stopped.Status != "stopped" {
		t.Fatalf("expected stopped status, got %+v", stopped)
	}

	// 5. start_sandbox
	res, err = session.CallTool(ctx, &mcp.CallToolParams{
		Name:      "start_sandbox",
		Arguments: map[string]any{"target": "beta"},
	})
	if err != nil {
		t.Fatalf("start_sandbox: %v", err)
	}
	started := decodeStructured[SandboxInfo](t, res)
	if started.Status != "running" {
		t.Fatalf("expected running status, got %+v", started)
	}

	// 6. exec_in_sandbox
	res, err = session.CallTool(ctx, &mcp.CallToolParams{
		Name: "exec_in_sandbox",
		Arguments: map[string]any{
			"target":  "alpha",
			"command": []string{"sh", "-c", "echo hi"},
			"workdir": "/workspace",
			"env":     []string{"FOO=bar"},
		},
	})
	if err != nil {
		t.Fatalf("exec_in_sandbox: %v", err)
	}
	execOut := decodeStructured[ExecResult](t, res)
	if execOut.ExitCode != 0 || execOut.Stdout != "hello from sandbox\n" {
		t.Fatalf("unexpected exec_in_sandbox output: %+v", execOut)
	}
	mb.mu.Lock()
	if mb.lastExecWorkdir != "/workspace" || !slices.Equal(mb.lastExecEnv, []string{"FOO=bar"}) {
		t.Errorf("unexpected exec args: workdir=%q env=%v", mb.lastExecWorkdir, mb.lastExecEnv)
	}
	mb.mu.Unlock()

	// 7. sandbox_logs (default tail=100, and cap at 2000)
	res, err = session.CallTool(ctx, &mcp.CallToolParams{
		Name:      "sandbox_logs",
		Arguments: map[string]any{"target": "alpha"},
	})
	if err != nil {
		t.Fatalf("sandbox_logs: %v", err)
	}
	sbLogs := decodeStructured[logsOutput](t, res)
	if sbLogs.Tail != DefaultLogTail || sbLogs.Logs != "line 1\nline 2\n" || sbLogs.Truncated {
		t.Fatalf("unexpected sandbox_logs output: %+v", sbLogs)
	}
	res, err = session.CallTool(ctx, &mcp.CallToolParams{
		Name:      "sandbox_logs",
		Arguments: map[string]any{"target": "alpha", "tail": 99999},
	})
	if err != nil {
		t.Fatalf("sandbox_logs capped: %v", err)
	}
	sbLogs = decodeStructured[logsOutput](t, res)
	if sbLogs.Tail != MaxLogTail {
		t.Fatalf("expected capped tail=%d, got %d", MaxLogTail, sbLogs.Tail)
	}

	// 8. create_snapshot (defaults leave_running=true)
	res, err = session.CallTool(ctx, &mcp.CallToolParams{
		Name:      "create_snapshot",
		Arguments: map[string]any{"target": "alpha"},
	})
	if err != nil {
		t.Fatalf("create_snapshot: %v", err)
	}
	snap := decodeStructured[SnapshotInfo](t, res)
	if snap.SandboxID != "sb-1" || snap.Name == "" {
		t.Fatalf("unexpected create_snapshot output: %+v", snap)
	}
	mb.mu.Lock()
	if mb.lastLeaveRunning == nil || !*mb.lastLeaveRunning {
		t.Errorf("expected leave_running default=true, got %v", mb.lastLeaveRunning)
	}
	mb.mu.Unlock()

	// 9. list_snapshots
	res, err = session.CallTool(ctx, &mcp.CallToolParams{
		Name:      "list_snapshots",
		Arguments: map[string]any{"target": "alpha"},
	})
	if err != nil {
		t.Fatalf("list_snapshots: %v", err)
	}
	snapList := decodeStructured[listSnapshotsOutput](t, res)
	if snapList.Count != 2 {
		t.Fatalf("expected 2 snapshots, got %+v", snapList)
	}

	// 10. restore_snapshot
	res, err = session.CallTool(ctx, &mcp.CallToolParams{
		Name: "restore_snapshot",
		Arguments: map[string]any{
			"target":   "alpha",
			"snapshot": snap.Name,
		},
	})
	if err != nil {
		t.Fatalf("restore_snapshot: %v", err)
	}
	restored := decodeStructured[SandboxInfo](t, res)
	if restored.ID != "sb-1" || restored.Status != "running" {
		t.Fatalf("unexpected restore_snapshot output: %+v", restored)
	}

	// 11. delete_sandbox
	res, err = session.CallTool(ctx, &mcp.CallToolParams{
		Name:      "delete_sandbox",
		Arguments: map[string]any{"target": "beta"},
	})
	if err != nil {
		t.Fatalf("delete_sandbox: %v", err)
	}
	delOut := decodeStructured[deleteSandboxOutput](t, res)
	if !delOut.Deleted || delOut.Target != "beta" {
		t.Fatalf("unexpected delete_sandbox output: %+v", delOut)
	}

	// 12. list_containers (default all=true, and explicit all=false)
	res, err = session.CallTool(ctx, &mcp.CallToolParams{Name: "list_containers"})
	if err != nil {
		t.Fatalf("list_containers: %v", err)
	}
	cList := decodeStructured[listContainersOutput](t, res)
	if cList.Count != 2 {
		t.Fatalf("expected 2 containers with default all=true, got %+v", cList)
	}
	res, err = session.CallTool(ctx, &mcp.CallToolParams{
		Name:      "list_containers",
		Arguments: map[string]any{"all": false},
	})
	if err != nil {
		t.Fatalf("list_containers(all=false): %v", err)
	}
	cList = decodeStructured[listContainersOutput](t, res)
	if cList.Count != 1 || cList.Containers[0].Name != "web" {
		t.Fatalf("expected 1 running container, got %+v", cList)
	}

	// 13. container_logs
	res, err = session.CallTool(ctx, &mcp.CallToolParams{
		Name:      "container_logs",
		Arguments: map[string]any{"target": "web", "tail": 50},
	})
	if err != nil {
		t.Fatalf("container_logs: %v", err)
	}
	cLogs := decodeStructured[logsOutput](t, res)
	if cLogs.Tail != 50 || cLogs.Logs != "nginx ready\n" || cLogs.Truncated {
		t.Fatalf("unexpected container_logs output: %+v", cLogs)
	}

	// 14. list_images (default "all", "cached", "registry")
	res, err = session.CallTool(ctx, &mcp.CallToolParams{Name: "list_images"})
	if err != nil {
		t.Fatalf("list_images: %v", err)
	}
	imgList := decodeStructured[listImagesOutput](t, res)
	if imgList.Count != 2 {
		t.Fatalf("expected 2 images for source=all, got %+v", imgList)
	}
	res, err = session.CallTool(ctx, &mcp.CallToolParams{
		Name:      "list_images",
		Arguments: map[string]any{"source": "registry"},
	})
	if err != nil {
		t.Fatalf("list_images(registry): %v", err)
	}
	imgList = decodeStructured[listImagesOutput](t, res)
	if imgList.Count != 1 || imgList.Images[0].Source != "registry" {
		t.Fatalf("expected 1 registry image, got %+v", imgList)
	}

	// 15. list_routes
	res, err = session.CallTool(ctx, &mcp.CallToolParams{Name: "list_routes"})
	if err != nil {
		t.Fatalf("list_routes: %v", err)
	}
	routeList := decodeStructured[listRoutesOutput](t, res)
	if routeList.Count != 1 || routeList.Routes[0].Host != "alpha.localhost" {
		t.Fatalf("unexpected list_routes output: %+v", routeList)
	}
}

func TestOutputTruncation(t *testing.T) {
	mb := newMockBackend()
	// Mix ASCII and 3-byte UTF-8 runes ('한') crossing the 64 KiB boundary.
	huge := strings.Repeat("abc한", (MaxOutputBytes/6)+500)
	mb.execResult = ExecResult{
		ExitCode: 0,
		Stdout:   huge,
		Stderr:   huge,
	}
	mb.sandboxLogs = huge
	mb.containerLogs = huge

	_, session := startTestServer(t, Config{Port: -1, Backend: mb})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// exec_in_sandbox stdout & stderr
	res, err := session.CallTool(ctx, &mcp.CallToolParams{
		Name: "exec_in_sandbox",
		Arguments: map[string]any{
			"target":  "alpha",
			"command": []string{"cat", "big.log"},
		},
	})
	if err != nil {
		t.Fatalf("exec_in_sandbox: %v", err)
	}
	execOut := decodeStructured[ExecResult](t, res)
	for name, out := range map[string]string{"stdout": execOut.Stdout, "stderr": execOut.Stderr} {
		if len(out) > MaxOutputBytes {
			t.Errorf("%s length %d exceeds MaxOutputBytes %d", name, len(out), MaxOutputBytes)
		}
		if !strings.HasSuffix(out, truncatedMarker) {
			t.Errorf("%s missing truncation marker suffix", name)
		}
		if !utf8.ValidString(out) {
			t.Errorf("%s is not valid UTF-8 after truncation", name)
		}
	}

	// sandbox_logs
	res, err = session.CallTool(ctx, &mcp.CallToolParams{
		Name:      "sandbox_logs",
		Arguments: map[string]any{"target": "alpha"},
	})
	if err != nil {
		t.Fatalf("sandbox_logs: %v", err)
	}
	sbLogs := decodeStructured[logsOutput](t, res)
	if !sbLogs.Truncated || len(sbLogs.Logs) > MaxOutputBytes || !strings.HasSuffix(sbLogs.Logs, truncatedMarker) || !utf8.ValidString(sbLogs.Logs) {
		t.Fatalf("sandbox_logs truncation failed: truncated=%v len=%d validUTF8=%v", sbLogs.Truncated, len(sbLogs.Logs), utf8.ValidString(sbLogs.Logs))
	}

	// container_logs
	res, err = session.CallTool(ctx, &mcp.CallToolParams{
		Name:      "container_logs",
		Arguments: map[string]any{"target": "web"},
	})
	if err != nil {
		t.Fatalf("container_logs: %v", err)
	}
	cLogs := decodeStructured[logsOutput](t, res)
	if !cLogs.Truncated || len(cLogs.Logs) > MaxOutputBytes || !strings.HasSuffix(cLogs.Logs, truncatedMarker) || !utf8.ValidString(cLogs.Logs) {
		t.Fatalf("container_logs truncation failed: truncated=%v len=%d validUTF8=%v", cLogs.Truncated, len(cLogs.Logs), utf8.ValidString(cLogs.Logs))
	}
}

func TestErrorsStatsAndActivityCallback(t *testing.T) {
	mb := newMockBackend()
	mb.failTool["start_sandbox"] = errors.New("vm daemon offline")

	var actMu sync.Mutex
	var events []Stats
	var srvRef *Server

	srv, session := startTestServer(t, Config{
		Port:    -1,
		Backend: mb,
		OnActivity: func(st Stats) {
			// Verify callback runs outside locks by calling srv.Stats() re-entrantly.
			if srvRef != nil {
				_ = srvRef.Stats()
			}
			actMu.Lock()
			events = append(events, st)
			actMu.Unlock()
		},
	})
	srvRef = srv

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// Successful call first.
	res, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "list_routes"})
	if err != nil {
		t.Fatalf("list_routes: %v", err)
	}
	if res.IsError {
		t.Fatalf("expected success, got error: %+v", res.Content)
	}
	s1 := srv.Stats()
	if s1.CallCount != 1 || s1.LastTool != "list_routes" || s1.LastErr != "" || s1.LastCalledAt.IsZero() {
		t.Fatalf("unexpected stats after success: %+v", s1)
	}

	// Backend failure propagates as tool error and updates Stats.LastErr.
	res, err = session.CallTool(ctx, &mcp.CallToolParams{
		Name:      "start_sandbox",
		Arguments: map[string]any{"target": "alpha"},
	})
	if err != nil {
		t.Fatalf("start_sandbox transport error: %v", err)
	}
	if !res.IsError {
		t.Fatalf("expected IsError=true on backend failure")
	}
	s2 := srv.Stats()
	if s2.CallCount != 2 || s2.LastTool != "start_sandbox" || !strings.Contains(s2.LastErr, "vm daemon offline") {
		t.Fatalf("unexpected stats after error: %+v", s2)
	}
	if s1.Equal(s2) {
		t.Fatalf("expected s1 != s2")
	}
	if !s2.Equal(srv.Stats()) {
		t.Fatalf("expected Stats.Equal to return true for identical snapshots")
	}

	// Invalid list_images source returns tool error cleanly.
	res, err = session.CallTool(ctx, &mcp.CallToolParams{
		Name:      "list_images",
		Arguments: map[string]any{"source": "bogus"},
	})
	if err != nil {
		t.Fatalf("list_images invalid source transport error: %v", err)
	}
	if !res.IsError {
		t.Fatalf("expected IsError=true for invalid source")
	}

	actMu.Lock()
	gotEvents := len(events)
	actMu.Unlock()
	if gotEvents != 3 {
		t.Fatalf("expected 3 OnActivity callbacks, got %d", gotEvents)
	}
}

func TestNilBackendReturnsCleanError(t *testing.T) {
	srv, session := startTestServer(t, Config{Port: -1, Backend: nil})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	res, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "list_sandboxes"})
	if err != nil {
		t.Fatalf("unexpected protocol error: %v", err)
	}
	if !res.IsError {
		t.Fatalf("expected IsError=true when Backend is nil")
	}
	st := srv.Stats()
	if st.CallCount != 1 || st.LastTool != "list_sandboxes" || !strings.Contains(st.LastErr, "backend is not configured") {
		t.Fatalf("unexpected stats for nil backend: %+v", st)
	}
}

func TestHealthzAndSecurityProtections(t *testing.T) {
	srv, _ := startTestServer(t, Config{
		Port:    -1,
		Version: "0.9.0-test",
		Backend: newMockBackend(),
	})

	healthURL := fmt.Sprintf("http://%s/healthz", srv.Addr())
	resp, err := http.Get(healthURL)
	if err != nil {
		t.Fatalf("GET /healthz failed: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /healthz status = %d, want 200", resp.StatusCode)
	}
	var payload struct {
		Status  string `json:"status"`
		Server  string `json:"server"`
		Version string `json:"version"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		t.Fatalf("decode /healthz: %v", err)
	}
	if payload.Status != "ok" || payload.Server != "gvx-desktop" || payload.Version != "0.9.0-test" {
		t.Fatalf("unexpected /healthz payload: %+v", payload)
	}

	// Non-GET/HEAD on /healthz returns 405.
	postResp, err := http.Post(healthURL, "application/json", strings.NewReader("{}"))
	if err != nil {
		t.Fatalf("POST /healthz failed: %v", err)
	}
	_ = postResp.Body.Close()
	if postResp.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("POST /healthz status = %d, want 405", postResp.StatusCode)
	}

	// DNS rebinding protection: non-localhost Host header rejected with 403 Forbidden.
	req, err := http.NewRequest(http.MethodPost, srv.EndpointURL(), strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"initialize"}`))
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	req.Host = "evil.example.com"
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	rebindResp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("DNS rebinding test request failed: %v", err)
	}
	_, _ = io.ReadAll(rebindResp.Body)
	_ = rebindResp.Body.Close()
	if rebindResp.StatusCode != http.StatusForbidden {
		t.Fatalf("non-localhost Host status = %d, want 403 Forbidden", rebindResp.StatusCode)
	}

	// Cross-origin protection: cross-site browser POST rejected with 403 Forbidden.
	corsReq, err := http.NewRequest(http.MethodPost, srv.EndpointURL(), strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"initialize"}`))
	if err != nil {
		t.Fatalf("NewRequest cors: %v", err)
	}
	corsReq.Header.Set("Content-Type", "application/json")
	corsReq.Header.Set("Accept", "application/json, text/event-stream")
	corsReq.Header.Set("Origin", "https://evil.example.com")
	corsReq.Header.Set("Sec-Fetch-Site", "cross-site")
	corsResp, err := http.DefaultClient.Do(corsReq)
	if err != nil {
		t.Fatalf("cross-origin test request failed: %v", err)
	}
	_, _ = io.ReadAll(corsResp.Body)
	_ = corsResp.Body.Close()
	if corsResp.StatusCode != http.StatusForbidden {
		t.Fatalf("cross-origin POST status = %d, want 403 Forbidden", corsResp.StatusCode)
	}
}

func TestLifecycleIdempotentAndConcurrentCalls(t *testing.T) {
	srv := New(Config{
		Port:    -1,
		Backend: newMockBackend(),
	})

	if srv.Running() || srv.Addr() != "" || srv.Port() != 0 {
		t.Fatalf("unexpected stopped state: running=%v addr=%q port=%d", srv.Running(), srv.Addr(), srv.Port())
	}
	// Stop before Start is a safe no-op.
	if err := srv.Stop(context.Background()); err != nil {
		t.Fatalf("Stop before Start failed: %v", err)
	}

	// Start is idempotent.
	if err := srv.Start(); err != nil {
		t.Fatalf("Start #1 failed: %v", err)
	}
	addr1 := srv.Addr()
	port1 := srv.Port()
	if !srv.Running() || !strings.HasPrefix(addr1, "127.0.0.1:") || port1 <= 0 {
		t.Fatalf("unexpected running state: running=%v addr=%q port=%d", srv.Running(), addr1, port1)
	}
	if err := srv.Start(); err != nil {
		t.Fatalf("Start #2 (idempotent) failed: %v", err)
	}
	if srv.Addr() != addr1 {
		t.Fatalf("idempotent Start changed Addr from %q to %q", addr1, srv.Addr())
	}

	// Connect multiple clients and invoke tools concurrently under -race.
	const workers = 6
	const callsPerWorker = 5
	var wg sync.WaitGroup
	errCh := make(chan error, workers*callsPerWorker)

	for i := range workers {
		wg.Add(1)
		go func(workerID int) {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			client := mcp.NewClient(&mcp.Implementation{
				Name:    fmt.Sprintf("worker-%d", workerID),
				Version: "1.0.0",
			}, nil)
			session, err := client.Connect(ctx, &mcp.StreamableClientTransport{
				Endpoint: srv.EndpointURL(),
			}, nil)
			if err != nil {
				errCh <- fmt.Errorf("worker %d connect: %w", workerID, err)
				return
			}
			defer session.Close()

			for j := range callsPerWorker {
				var name string
				switch j % 3 {
				case 0:
					name = "list_sandboxes"
				case 1:
					name = "list_containers"
				case 2:
					name = "list_routes"
				}
				res, err := session.CallTool(ctx, &mcp.CallToolParams{Name: name})
				if err != nil {
					errCh <- fmt.Errorf("worker %d call %s: %w", workerID, name, err)
					return
				}
				if res.IsError {
					errCh <- fmt.Errorf("worker %d call %s returned IsError", workerID, name)
					return
				}
			}
		}(i)
	}
	wg.Wait()
	close(errCh)
	for err := range errCh {
		t.Errorf("%v", err)
	}

	if got := srv.Stats().CallCount; got != workers*callsPerWorker {
		t.Fatalf("CallCount = %d, want %d", got, workers*callsPerWorker)
	}

	// Stop is idempotent and releases the bound address.
	stopCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := srv.Stop(stopCtx); err != nil {
		t.Fatalf("Stop #1 failed: %v", err)
	}
	if srv.Running() || srv.Addr() != "" {
		t.Fatalf("expected stopped state after Stop: running=%v addr=%q", srv.Running(), srv.Addr())
	}
	if err := srv.Stop(context.Background()); err != nil {
		t.Fatalf("Stop #2 (idempotent) failed: %v", err)
	}

	// Restarting after Stop works cleanly, and Stop handles an open SSE session without hanging.
	if err := srv.Start(); err != nil {
		t.Fatalf("restart Start() failed: %v", err)
	}
	ctx, cCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cCancel()
	client := mcp.NewClient(&mcp.Implementation{Name: "open-sse-client", Version: "1.0.0"}, nil)
	openSess, err := client.Connect(ctx, &mcp.StreamableClientTransport{Endpoint: srv.EndpointURL()}, nil)
	if err != nil {
		t.Fatalf("connect after restart failed: %v", err)
	}
	defer openSess.Close()

	stopCtx2, sCancel2 := context.WithTimeout(context.Background(), 3*time.Second)
	defer sCancel2()
	if err := srv.Stop(stopCtx2); err != nil {
		t.Fatalf("Stop with open client session failed: %v", err)
	}
}

func TestConfigHelpersAndDefaultPort(t *testing.T) {
	srv := New(Config{})
	if srv.Port() != DefaultPort {
		t.Fatalf("default Port() = %d, want %d", srv.Port(), DefaultPort)
	}
	wantEndpoint := fmt.Sprintf("http://127.0.0.1:%d/mcp", DefaultPort)
	if srv.EndpointURL() != wantEndpoint {
		t.Fatalf("default EndpointURL() = %q, want %q", srv.EndpointURL(), wantEndpoint)
	}

	cfgJSON := ClientConfigJSON("")
	var parsed struct {
		MCPServers map[string]struct {
			URL string `json:"url"`
		} `json:"mcpServers"`
	}
	if err := json.Unmarshal([]byte(cfgJSON), &parsed); err != nil {
		t.Fatalf("ClientConfigJSON produced invalid JSON: %v\n%s", err, cfgJSON)
	}
	if parsed.MCPServers["gvx-desktop"].URL != wantEndpoint {
		t.Fatalf("ClientConfigJSON url = %q, want %q", parsed.MCPServers["gvx-desktop"].URL, wantEndpoint)
	}

	custom := "http://127.0.0.1:19999/mcp"
	if got := ClaudeAddCommand(custom); got != "claude mcp add --transport http gvx-desktop "+custom {
		t.Fatalf("ClaudeAddCommand = %q", got)
	}
	if got := ClaudeAddCommand(""); got != "claude mcp add --transport http gvx-desktop "+wantEndpoint {
		t.Fatalf("ClaudeAddCommand default = %q", got)
	}
}
