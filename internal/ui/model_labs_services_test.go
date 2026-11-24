package ui

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/ironpark/ggui"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/ironpark/gvx-desktop/internal/dockerapi"
	"github.com/ironpark/gvx-desktop/internal/kubeapi"
	"github.com/ironpark/gvx-desktop/internal/localproxy"
	"github.com/ironpark/gvx-desktop/internal/mcpserver"
	"github.com/ironpark/gvx-desktop/internal/recipes"
	"github.com/ironpark/gvx-desktop/internal/rungv"
	"github.com/ironpark/gvx-desktop/internal/sandbox"
	"github.com/ironpark/gvx-desktop/internal/store"
)

func labsTestModel(t *testing.T, initSettings func(*store.Settings), specs ...sandbox.Spec) (*Model, *ggui.Probe, store.Store) {
	t.Helper()
	t.Cleanup(func() { SetLanguage(LangEnglish) })
	SetLanguage(LangEnglish)

	dir := t.TempDir()
	st := store.Store{Dir: dir}
	set := store.DefaultSettings()
	set.Language = LangEnglish
	set.SandboxEngine = false
	if initSettings != nil {
		initSettings(&set)
	}
	if err := st.SaveSettings(set); err != nil {
		t.Fatal(err)
	}
	if len(specs) > 0 {
		if err := st.SaveSandboxes(specs); err != nil {
			t.Fatal(err)
		}
	}

	var m *Model
	p := ggui.ProbeBuilder(func() ggui.Widget {
		return ggui.Provide(ggui.ReducedMotionKey, true, Build(m))
	}, ggui.Sz(1280, 2600))
	p.Setup(func() {
		m = NewModel(st, dir)
		m.localRegPort = -1          // ephemeral loopback port
		m.localProxyTestPorts = true // ephemeral loopback HTTP/HTTPS ports
		m.mcpTestPort = -1           // ephemeral loopback MCP port
		m.Attach(p)
		m.followLocalProxy()
		m.followMCPServer()
	})
	t.Cleanup(p.Close)
	p.Frame()
	return m, p, st
}

func semanticsContains(p *ggui.Probe, substr string) bool {
	for _, n := range p.Semantics().All() {
		if strings.Contains(n.Name, substr) {
			return true
		}
	}
	return false
}

func TestDeriveLocalProxyRoutes(t *testing.T) {
	dind := recipes.DockerEngine()
	k3s := recipes.KubernetesNode()
	reg := recipes.ImageRegistry()

	rows := []Row{
		// Recipe sandboxes must be skipped.
		{
			Spec: dind,
			State: sandbox.State{
				Status: sandbox.Running,
				Ports:  []sandbox.PublishedPort{{Host: "127.0.0.1:2375", Guest: 2375, Proto: "tcp"}},
			},
		},
		{
			Spec: k3s,
			State: sandbox.State{
				Status: sandbox.Running,
				Ports:  []sandbox.PublishedPort{{Host: "127.0.0.1:6443", Guest: 6443, Proto: "tcp"}},
			},
		},
		{
			Spec: reg,
			State: sandbox.State{
				Status: sandbox.Running,
				Ports:  []sandbox.PublishedPort{{Host: "127.0.0.1:5001", Guest: 5000, Proto: "tcp"}},
			},
		},
		// Stopped user sandbox must be skipped.
		{
			Spec: sandbox.Spec{ID: "sb-stop", Name: "stopped-app"},
			State: sandbox.State{
				Status: sandbox.Stopped,
				Ports:  []sandbox.PublishedPort{{Host: "127.0.0.1:8080", Guest: 80, Proto: "tcp"}},
			},
		},
		// Running user sandbox with multiple ports: web port 3000 wins primary host over 22 and 9090;
		// 0.0.0.0 is normalized to 127.0.0.1; UDP and Unix sockets are skipped.
		{
			Spec: sandbox.Spec{ID: "sb-web", Name: "My App"},
			State: sandbox.State{
				Status: sandbox.Running,
				Ports: []sandbox.PublishedPort{
					{Host: "127.0.0.1:2222", Guest: 22, Proto: "tcp"},
					{Host: "0.0.0.0:13000", Guest: 3000, Proto: "tcp"},
					{Host: "127.0.0.1:19090", Guest: 9090, Proto: "tcp"},
					{Host: "127.0.0.1:5353", Guest: 53, Proto: "udp"},
					{Host: "/tmp/app.sock", Guest: 80, Proto: "tcp"},
				},
			},
		},
	}

	containers := []dockerapi.Container{
		// Stopped container skipped.
		{
			ID:    "ctr-stopped",
			Names: []string{"/old-web"},
			State: "exited",
			Ports: []dockerapi.Port{{PublicPort: 8088, PrivatePort: 80, Type: "tcp"}},
		},
		// Standalone container with dual-stack duplicate port 80 and secondary port 9090, plus UDP skipped.
		{
			ID:    "ctr-standalone",
			Names: []string{"/nginx"},
			State: "running",
			Ports: []dockerapi.Port{
				{IP: "0.0.0.0", PublicPort: 18081, PrivatePort: 80, Type: "tcp"},
				{IP: "::", PublicPort: 18081, PrivatePort: 80, Type: "tcp"},
				{IP: "127.0.0.1", PublicPort: 19091, PrivatePort: 9090, Type: "tcp"},
				{IP: "127.0.0.1", PublicPort: 5300, PrivatePort: 53, Type: "udp"},
			},
		},
		// Compose service container: <service>.<project>.localhost + extra port suffix.
		{
			ID:    "ctr-compose",
			Names: []string{"/shop-api-1"},
			State: "running",
			Labels: map[string]string{
				"com.docker.compose.project": "shop",
				"com.docker.compose.service": "api",
			},
			Ports: []dockerapi.Port{
				{PublicPort: 18082, PrivatePort: 8080, Type: "tcp"},
				{PublicPort: 19092, PrivatePort: 9100, Type: "tcp"},
			},
		},
		// Colliding host with sandbox "my-app.localhost": sandbox wins first-seen deterministically.
		{
			ID:    "ctr-collide",
			Names: []string{"/my-app"},
			State: "running",
			Ports: []dockerapi.Port{{PublicPort: 19999, PrivatePort: 80, Type: "tcp"}},
		},
	}

	forwards := []PortForward{
		{ID: 1, Target: kubeapi.Target{Namespace: "default", Service: "frontend", Port: 80}, Local: 20080},
		// Colliding service name on different port gets -8443 suffix.
		{ID: 2, Target: kubeapi.Target{Namespace: "default", Service: "frontend", Port: 8443}, Local: 20443},
		// Pod forward.
		{ID: 3, Target: kubeapi.Target{Namespace: "default", Pod: "worker-0", Port: 8000}, Local: 28000},
	}

	routes := deriveLocalProxyRoutes(rows, containers, forwards)

	byHost := make(map[string]localproxy.Route, len(routes))
	for i, rt := range routes {
		if i > 0 && routes[i-1].Host >= rt.Host {
			t.Errorf("routes not sorted strictly ascending: %q >= %q", routes[i-1].Host, rt.Host)
		}
		byHost[rt.Host] = rt
	}

	want := map[string]localproxy.Route{
		"my-app.localhost":            {Host: "my-app.localhost", Target: "127.0.0.1:13000", Kind: "sandbox", Name: "My App", GuestPort: 3000},
		"my-app-22.localhost":         {Host: "my-app-22.localhost", Target: "127.0.0.1:2222", Kind: "sandbox", Name: "My App", GuestPort: 22},
		"my-app-9090.localhost":       {Host: "my-app-9090.localhost", Target: "127.0.0.1:19090", Kind: "sandbox", Name: "My App", GuestPort: 9090},
		"nginx.localhost":             {Host: "nginx.localhost", Target: "127.0.0.1:18081", Kind: "container", Name: "nginx", GuestPort: 80},
		"nginx-9090.localhost":        {Host: "nginx-9090.localhost", Target: "127.0.0.1:19091", Kind: "container", Name: "nginx", GuestPort: 9090},
		"api.shop.localhost":          {Host: "api.shop.localhost", Target: "127.0.0.1:18082", Kind: "container", Name: "shop-api-1", GuestPort: 8080},
		"api-9100.shop.localhost":     {Host: "api-9100.shop.localhost", Target: "127.0.0.1:19092", Kind: "container", Name: "shop-api-1", GuestPort: 9100},
		"frontend.k8s.localhost":      {Host: "frontend.k8s.localhost", Target: "127.0.0.1:20080", Kind: "k8s", Name: "svc/frontend", GuestPort: 80},
		"frontend-8443.k8s.localhost": {Host: "frontend-8443.k8s.localhost", Target: "127.0.0.1:20443", Kind: "k8s", Name: "svc/frontend", GuestPort: 8443},
		"worker-0.k8s.localhost":      {Host: "worker-0.k8s.localhost", Target: "127.0.0.1:28000", Kind: "k8s", Name: "pod/worker-0", GuestPort: 8000},
	}

	if len(routes) != len(want) {
		t.Fatalf("got %d routes (%+v), want %d", len(routes), routes, len(want))
	}
	for host, exp := range want {
		got, ok := byHost[host]
		if !ok {
			t.Errorf("missing expected route %q", host)
			continue
		}
		if !got.Equal(exp) {
			t.Errorf("route %q = %+v, want %+v", host, got, exp)
		}
	}
}

func TestLocalProxyLifecycleHTTPAndHTTPSAndLabsUI(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Upstream-Proto", r.Header.Get("X-Forwarded-Proto"))
		_, _ = fmt.Fprintf(w, "hello from upstream host=%s", r.Host)
	}))
	defer upstream.Close()
	upstreamAddr := upstream.Listener.Addr().String()

	sb := sandbox.Spec{ID: "sb-1", Name: "webapp", Image: "nginx:alpine"}
	m, p, stStore := labsTestModel(t, func(s *store.Settings) {
		s.LocalProxy = false
	}, sb)

	// Mark sandbox running with published port targeting our httptest.Server.
	m.States.Set(map[string]sandbox.State{
		sb.ID: {
			Status: sandbox.Running,
			Ports:  []sandbox.PublishedPort{{Host: upstreamAddr, Guest: 80, Proto: "tcp"}},
		},
	})
	p.Frame()

	// Enable local proxy and wait for Running state.
	m.SetLocalProxy(true)
	settle(t, p, func() bool {
		st := ggui.Peek(m.LocalProxy)
		return st.Running && st.HTTPAddr != "" && st.HTTPSAddr != "" && len(st.Routes) == 1
	}, "local proxy starts and publishes route")

	st := ggui.Peek(m.LocalProxy)
	if saved, _ := stStore.Settings(); !saved.LocalProxy {
		t.Fatal("expected Settings.LocalProxy=true persisted")
	}
	wantURL := localproxy.FormatURL("https", "webapp.localhost", st.HTTPSPort)
	if gotURL := m.LocalProxyURLFor("sandbox", "webapp"); gotURL != wantURL {
		t.Fatalf("LocalProxyURLFor = %q, want %q", gotURL, wantURL)
	}

	// 1. Real HTTP request through local proxy.
	httpClient := &http.Client{Timeout: 5 * time.Second}
	req, err := http.NewRequest(http.MethodGet, "http://"+st.HTTPAddr+"/", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Host = "webapp.localhost"
	resp, err := httpClient.Do(req)
	if err != nil {
		t.Fatalf("HTTP proxy request failed: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("HTTP proxy status = %d, want 200 (body=%q)", resp.StatusCode, body)
	}
	if !strings.Contains(string(body), "hello from upstream host=webapp.localhost") {
		t.Errorf("unexpected HTTP proxy body: %q", body)
	}
	if proto := resp.Header.Get("X-Upstream-Proto"); proto != "http" {
		t.Errorf("X-Forwarded-Proto = %q, want http", proto)
	}

	// 2. Real HTTPS request verified against the proxy's generated local dev CA.
	caPEM, err := os.ReadFile(st.CAPath)
	if err != nil {
		t.Fatalf("read CA cert %q: %v", st.CAPath, err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(caPEM) {
		t.Fatalf("failed to parse CA cert from %q", st.CAPath)
	}
	httpsClient := &http.Client{
		Timeout: 5 * time.Second,
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
				var d net.Dialer
				return d.DialContext(ctx, network, st.HTTPSAddr)
			},
			TLSClientConfig: &tls.Config{
				RootCAs:    pool,
				ServerName: "webapp.localhost",
				MinVersion: tls.VersionTLS12,
			},
		},
	}
	httpsReq, err := http.NewRequest(http.MethodGet, fmt.Sprintf("https://webapp.localhost:%d/", st.HTTPSPort), nil)
	if err != nil {
		t.Fatal(err)
	}
	httpsResp, err := httpsClient.Do(httpsReq)
	if err != nil {
		t.Fatalf("HTTPS proxy request failed: %v", err)
	}
	httpsBody, _ := io.ReadAll(httpsResp.Body)
	httpsResp.Body.Close()
	if httpsResp.StatusCode != http.StatusOK {
		t.Fatalf("HTTPS proxy status = %d, want 200 (body=%q)", httpsResp.StatusCode, httpsBody)
	}
	if !strings.Contains(string(httpsBody), "hello from upstream host=webapp.localhost") {
		t.Errorf("unexpected HTTPS proxy body: %q", httpsBody)
	}
	if proto := httpsResp.Header.Get("X-Upstream-Proto"); proto != "https" {
		t.Errorf("HTTPS X-Forwarded-Proto = %q, want https", proto)
	}

	// 3. Verify Labs page renders Local Proxy status, CA controls, and active route.
	m.Go(PageLabs)
	p.Frame()
	for _, wantText := range []string{
		"Local HTTPS routing",
		"Route *.localhost domains",
		"Running on *.localhost",
		"Local development CA",
		"Reveal CA",
		"Copy trust command",
		wantURL,
	} {
		if !semanticsContains(p, wantText) {
			t.Errorf("Labs page missing %q:\n%s", wantText, p.Semantics())
		}
	}

	// 4. Disable local proxy and verify clean shutdown.
	m.SetLocalProxy(false)
	settle(t, p, func() bool {
		cur := ggui.Peek(m.LocalProxy)
		return !cur.Enabled && !cur.Running && cur.HTTPAddr == "" && cur.HTTPSAddr == ""
	}, "local proxy stops cleanly")
	if gotURL := m.LocalProxyURLFor("sandbox", "webapp"); gotURL != "" {
		t.Errorf("LocalProxyURLFor after stop = %q, want empty", gotURL)
	}
}

func callMCPToolWithPump(t *testing.T, p *ggui.Probe, sess *mcp.ClientSession, name string, args map[string]any) *mcp.CallToolResult {
	t.Helper()
	type result struct {
		res *mcp.CallToolResult
		err error
	}
	ch := make(chan result, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		res, err := sess.CallTool(ctx, &mcp.CallToolParams{
			Name:      name,
			Arguments: args,
		})
		ch <- result{res: res, err: err}
	}()

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		p.Frame()
		select {
		case r := <-ch:
			if r.err != nil {
				t.Fatalf("CallTool(%q) error: %v", name, r.err)
			}
			if r.res == nil {
				t.Fatalf("CallTool(%q) returned nil result", name)
			}
			if r.res.IsError {
				t.Fatalf("CallTool(%q) returned tool error: %+v", name, r.res.Content)
			}
			return r.res
		default:
			time.Sleep(5 * time.Millisecond)
		}
	}
	t.Fatalf("timed out waiting for CallTool(%q)", name)
	return nil
}

func decodeToolJSON[T any](t *testing.T, res *mcp.CallToolResult) T {
	t.Helper()
	var out T
	raw, err := json.Marshal(res.StructuredContent)
	if err != nil {
		t.Fatalf("marshal StructuredContent: %v", err)
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("unmarshal StructuredContent (%s): %v", raw, err)
	}
	return out
}

func TestMCPServerLifecycleRealClientAndLabsUI(t *testing.T) {
	initial := sandbox.Spec{ID: "sb-init", Name: "devbox", Image: "python:3.12-slim", Checkpoint: true}
	m, p, stStore := labsTestModel(t, func(s *store.Settings) {
		s.LocalProxy = true
		s.MCPServer = false
	}, initial)

	m.States.Set(map[string]sandbox.State{
		initial.ID: {
			Status: sandbox.Running,
			Ports:  []sandbox.PublishedPort{{Host: "127.0.0.1:18888", Guest: 8000, Proto: "tcp"}},
		},
	})
	m.Images.Set(Listing[rungv.Image]{
		Loaded: true,
		Items: []rungv.Image{
			{ID: "img-1", Repository: "python", Tag: "3.12-slim", Size: "120MB"},
		},
	})
	p.Frame()

	// Wait for local proxy to be running with devbox.localhost route.
	settle(t, p, func() bool {
		st := ggui.Peek(m.LocalProxy)
		return st.Running && len(st.Routes) == 1
	}, "local proxy running for MCP list_routes test")

	// Start MCP server.
	m.SetMCPServer(true)
	settle(t, p, func() bool {
		st := ggui.Peek(m.MCPServer)
		return st.Running && st.EndpointURL != ""
	}, "MCP server starts")

	st := ggui.Peek(m.MCPServer)
	if saved, _ := stStore.Settings(); !saved.MCPServer {
		t.Fatal("expected Settings.MCPServer=true persisted")
	}

	// Connect real MCP SDK client over Streamable HTTP.
	client := mcp.NewClient(&mcp.Implementation{Name: "ui-integration-test", Version: "1.0.0"}, nil)
	connCtx, connCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer connCancel()
	sess, err := client.Connect(connCtx, &mcp.StreamableClientTransport{
		Endpoint: st.EndpointURL,
	}, nil)
	if err != nil {
		t.Fatalf("MCP client.Connect(%s) failed: %v", st.EndpointURL, err)
	}
	defer sess.Close()

	// 1. list_sandboxes
	listRes := callMCPToolWithPump(t, p, sess, "list_sandboxes", nil)
	listed := decodeToolJSON[struct {
		Sandboxes []mcpserver.SandboxInfo `json:"sandboxes"`
		Count     int                     `json:"count"`
	}](t, listRes)
	if listed.Count != 1 || listed.Sandboxes[0].Name != "devbox" {
		t.Fatalf("list_sandboxes = %+v, want 1 sandbox 'devbox'", listed)
	}
	if !strings.HasPrefix(listed.Sandboxes[0].LocalURL, "https://devbox.localhost:") {
		t.Errorf("sandbox LocalURL = %q, want https://devbox.localhost:<port>", listed.Sandboxes[0].LocalURL)
	}

	// 2. list_routes
	routesRes := callMCPToolWithPump(t, p, sess, "list_routes", nil)
	routesOut := decodeToolJSON[struct {
		Routes []mcpserver.RouteInfo `json:"routes"`
		Count  int                   `json:"count"`
	}](t, routesRes)
	if routesOut.Count != 1 || routesOut.Routes[0].Host != "devbox.localhost" {
		t.Fatalf("list_routes = %+v, want devbox.localhost", routesOut)
	}

	// 3. list_images
	imgsRes := callMCPToolWithPump(t, p, sess, "list_images", map[string]any{"source": "cached"})
	imgsOut := decodeToolJSON[struct {
		Images []mcpserver.ImageInfo `json:"images"`
		Count  int                   `json:"count"`
	}](t, imgsRes)
	if imgsOut.Count != 1 || imgsOut.Images[0].Repository != "python" {
		t.Fatalf("list_images = %+v, want cached python image", imgsOut)
	}

	// 4. create_sandbox (start: false) exercises callUI -> UI thread signal mutation safely.
	createRes := callMCPToolWithPump(t, p, sess, "create_sandbox", map[string]any{
		"name":  "agent-box",
		"image": "alpine:3.20",
		"start": false,
	})
	created := decodeToolJSON[mcpserver.SandboxInfo](t, createRes)
	if created.Name != "agent-box" || created.Image != "alpine:3.20" || !created.Checkpoint {
		t.Fatalf("create_sandbox = %+v, want agent-box alpine:3.20 checkpoint=true", created)
	}
	if len(ggui.Peek(m.Specs)) != 2 {
		t.Fatalf("Specs count = %d, want 2 after create_sandbox", len(ggui.Peek(m.Specs)))
	}

	// Wait for live MCPServerState activity update.
	settle(t, p, func() bool {
		cur := ggui.Peek(m.MCPServer)
		return cur.CallCount == 4 && cur.LastTool == "create_sandbox"
	}, "MCP live CallCount and LastTool update")

	// Render Labs page and verify MCP status, copy actions, and live activity badge.
	m.Go(PageLabs)
	p.Frame()
	for _, wantText := range []string{
		"AI agent MCP server",
		"Built-in MCP server",
		"Running on " + st.EndpointURL,
		"4 calls · create_sandbox",
		"Copy endpoint",
		"Copy Claude Code command",
		"Copy JSON config",
		"14 tools: sandboxes, exec, logs, snapshots, containers, images, routes.",
	} {
		if !semanticsContains(p, wantText) {
			t.Errorf("Labs page missing %q:\n%s", wantText, p.Semantics())
		}
	}

	// Close client before stopping server so SSE stream tears down cleanly.
	_ = sess.Close()
	m.SetMCPServer(false)
	settle(t, p, func() bool {
		cur := ggui.Peek(m.MCPServer)
		return !cur.Enabled && !cur.Running && cur.Addr == ""
	}, "MCP server stops cleanly")
}

func TestLabsSupportedAndNavigation(t *testing.T) {
	if !LabsSupported() {
		t.Fatal("LabsSupported() = false, want true on all platforms")
	}

	// Simulate non-Apple-silicon platform: Labs still appears in sidebar & palette,
	// shows Local HTTPS routing & AI agent MCP server, and hides Rosetta section.
	prevPlatform := amd64BuilderPlatform
	amd64BuilderPlatform = func() bool { return false }
	defer func() { amd64BuilderPlatform = prevPlatform }()

	m, p, _ := labsTestModel(t, nil)

	// Sidebar includes Labs.
	if !semanticsContains(p, "Labs") {
		t.Fatalf("sidebar missing Labs entry:\n%s", p.Semantics())
	}

	// Command Palette includes Labs.
	m.PaletteQuery.Set("Labs")
	m.PaletteOpen.Set(true)
	p.Frame()
	if !semanticsContains(p, "Labs") {
		t.Fatalf("Command Palette missing Labs entry:\n%s", p.Semantics())
	}
	m.PaletteOpen.Set(false)

	// Navigate to PageLabs: shows both cross-platform Labs services, hides Rosetta.
	m.Go(PageLabs)
	p.Frame()
	if !semanticsContains(p, "Local HTTPS routing") {
		t.Errorf("PageLabs missing Local HTTPS routing:\n%s", p.Semantics())
	}
	if !semanticsContains(p, "AI agent MCP server") {
		t.Errorf("PageLabs missing AI agent MCP server:\n%s", p.Semantics())
	}
	if semanticsContains(p, "Cross-architecture & micro-VMs") || semanticsContains(p, "Micro-VM container runtime") {
		t.Errorf("PageLabs should hide Cross-architecture & micro-VMs on non-darwin/arm64:\n%s", p.Semantics())
	}
}
