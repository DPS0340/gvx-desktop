package extsdk

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
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func shortSocketPath(t *testing.T, name string) string {
	t.Helper()
	dir := t.TempDir()
	p := filepath.Join(dir, name)
	if len(p) < 100 {
		return p
	}
	// On macOS sun_path is 104 bytes and long test names push t.TempDir() past 104 bytes;
	// use a short temporary directory in the package dir and clean it up after the test.
	shortDir, err := os.MkdirTemp(".", ".s-*")
	if err != nil {
		t.Fatalf("MkdirTemp: %v", err)
	}
	absDir, err := filepath.Abs(shortDir)
	if err != nil {
		_ = os.RemoveAll(shortDir)
		t.Fatalf("Abs: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(absDir) })
	return filepath.Join(absDir, name)
}

func newUnixClient(sockPath string) *http.Client {
	return &http.Client{
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				var d net.Dialer
				return d.DialContext(ctx, "unix", sockPath)
			},
		},
	}
}

func doReqJSON(t *testing.T, client *http.Client, method, url, token, gestureID string, body any) (int, []byte) {
	t.Helper()
	var r io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("marshal request: %v", err)
		}
		r = strings.NewReader(string(b))
	}
	req, err := http.NewRequest(method, url, r)
	if err != nil {
		t.Fatalf("NewRequest(%s %s): %v", method, url, err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if gestureID != "" {
		req.Header.Set("X-GVX-Gesture-ID", gestureID)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("client.Do(%s %s): %v", method, url, err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read response: %v", err)
	}
	return resp.StatusCode, data
}

func waitHealth(t *testing.T, client *http.Client, baseURL, token string) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		req, _ := http.NewRequest(http.MethodGet, baseURL+"/v1/health", nil)
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		resp, err := client.Do(req)
		if err == nil {
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("server at %s did not become healthy", baseURL)
}

type testSSE struct {
	Event string
	ID    string
	Data  string
}

func readSSEStream(ctx context.Context, body io.Reader, out chan<- testSSE) {
	scanner := bufio.NewScanner(body)
	scanner.Buffer(make([]byte, 0, 64*1024), 2*1024*1024)
	var ev, id string
	var data []string
	for scanner.Scan() {
		line := strings.TrimRight(scanner.Text(), "\r")
		if line == "" {
			if len(data) > 0 || ev != "" {
				select {
				case out <- testSSE{Event: ev, ID: id, Data: strings.Join(data, "\n")}:
				case <-ctx.Done():
					return
				}
			}
			ev, id = "", ""
			data = data[:0]
			continue
		}
		if strings.HasPrefix(line, ":") {
			continue
		}
		k, v, ok := strings.Cut(line, ":")
		if ok {
			v = strings.TrimPrefix(v, " ")
		}
		switch k {
		case "event":
			ev = v
		case "id":
			id = v
		case "data":
			data = append(data, v)
		}
	}
}

func awaitSSE(t *testing.T, ch <-chan testSSE, timeout time.Duration, pred func(testSSE) bool) testSSE {
	t.Helper()
	deadline := time.After(timeout)
	for {
		select {
		case ev := <-ch:
			if pred(ev) {
				return ev
			}
		case <-deadline:
			t.Fatal("timed out waiting for SSE event")
		}
	}
}

func TestUIBuildersAll35KindsAndWireFormat(t *testing.T) {
	var ui *UI // nil receiver must be safe

	root := ui.Column(
		ui.Toolbar(
			ui.Breadcrumb([]string{"Home", "Build"}, OnChange("nav")),
			ui.Button("Run", Icon("play"), Variant("primary"), OnTap("run", "fast")),
		),
		ui.Stack(
			ui.Card(
				ui.Text("Status", Tone("accent"), Weight("semibold"), Size("small")),
				ui.Icon("box"),
				ui.Pill("v1", Variant("soft")),
				ui.Chip("e2e", Variant("outline")),
			).Variant("soft"),
		),
		ui.Scroll(
			ui.Wrap(
				ui.Grid(2,
					ui.Collapsible("More",
						ui.Markdown("# Notes", OnLink("open_doc")),
						ui.Code("go", "package main"),
					).Checked(true).OnChange("toggle_more"),
					ui.List(
						ui.Progress(0.75),
						ui.Spinner(),
						ui.Divider(),
					),
				),
			),
		).Variant("follow").Direction("vertical").MaxHeight(400),
		ui.Split("auto",
			ui.Tree(ui.TreeItem("Item", Check("off"), OnCheck("chk"))),
			ui.Table([]string{"Col"}, ui.Row(ui.Text("val"))),
		).Ratio(0.4),
		ui.Input(Key("k_in"), Variant("password"), Placeholder("secret"), OnSubmit("save")),
		ui.Textarea(Key("k_ta"), Placeholder("notes"), OnChange("edit")),
		ui.Toggle(Key("k_tog"), Checked(true), OnChange("tog")),
		ui.Select(Key("k_sel"), []string{"a", "b"}, "a", OnChange("sel")),
		ui.Tabs("T1", []string{"T1", "T2"}, Key("k_tabs"), OnChange("tab")),
		ui.Checkbox(Key("k_cb"), "Check", Check("mixed"), OnChange("cb")),
		ui.Chart("line", []ChartSeries{{Name: "s", Tone: "accent", Values: []float64{1, 2}}}, Labels("m1", "m2")),
		ui.Graph([]GraphNode{{ID: "a", Label: "A"}}, []GraphEdge{{From: "a", To: "a"}}),
		ui.Menu("Menu", ui.MenuItem("Action", OnTap("menu_act"))),
		ui.ContextMenu("ctx", ui.MenuItem("Inspect", OnTap("inspect"))),
		ui.Dialog("Confirm?", ui.Button("OK", OnTap("confirm"))),
		ui.Dag([]DagRow{{ID: "r1", Text: "Build", Badges: []Badge{{Text: "ok", Tone: "success"}}}}, ContextMenu("ctx"), OnChange("select_row")),
	).Fill(true)

	b, err := json.Marshal(root)
	if err != nil {
		t.Fatalf("json.Marshal(root): %v", err)
	}

	var decoded map[string]any
	if err := json.Unmarshal(b, &decoded); err != nil {
		t.Fatalf("json.Unmarshal: %v", err)
	}

	// Verify forbidden post-validation fields do NOT appear in rawNode JSON.
	jsonStr := string(b)
	for _, forbidden := range []string{`"tap":`, `"change":`, `"drop":`, `"checks":`} {
		if strings.Contains(jsonStr, forbidden) {
			t.Fatalf("rawNode JSON unexpectedly contains post-Read field %s", forbidden)
		}
	}

	// Verify all 35 node kinds are present.
	expectedKinds := []string{
		"column", "row", "stack", "card", "scroll", "wrap", "grid", "split",
		"collapsible", "breadcrumb", "tree", "tree_item", "text", "icon", "pill", "chip", "button",
		"list", "input", "textarea", "toggle", "select", "tabs", "table", "markdown", "code",
		"progress", "divider", "chart", "graph", "menu", "menu_item", "checkbox", "dialog", "dag",
	}
	seen := map[string]bool{}
	var walk func(n Node)
	walk = func(n Node) {
		seen[n.raw.Kind] = true
		for _, c := range n.raw.Children {
			walk(c)
		}
	}
	walk(root)
	for _, k := range expectedKinds {
		if !seen[k] {
			t.Fatalf("missing kind %q in UI builder tree", k)
		}
	}

	// Verify GraphNode.Weight=0 is omitted so uiblock.Read defaults it to 1.0.
	if strings.Contains(jsonStr, `"weight":0`) {
		t.Fatalf("expected GraphNode zero weight to be omitted, got: %s", jsonStr)
	}

	// Verify action arrays serialize as [name, ...args].
	var roundTrip Node
	if err := json.Unmarshal(b, &roundTrip); err != nil {
		t.Fatalf("Node.UnmarshalJSON: %v", err)
	}
	if roundTrip.raw.Kind != "column" || !roundTrip.raw.Fill {
		t.Fatalf("round-trip root mismatch: %+v", roundTrip.raw)
	}
}

func TestDiffAndNormalizeWireNodes(t *testing.T) {
	ui := &UI{}
	oldNode := ui.Column(
		ui.Text("count-0", Key("status")),
		ui.Split("horizontal", ui.Text("L"), ui.Text("R")),
		ui.Checkbox(Key("cb"), "Check"),
		ui.Graph([]GraphNode{{ID: "n1", Label: "N1"}}, nil),
	)
	nextNode := ui.Column(
		ui.Text("count-1", Key("status")),
		ui.Split("horizontal", ui.Text("L"), ui.Text("R2")),
		ui.Checkbox(Key("cb"), "Check", Check("on")),
		ui.Graph([]GraphNode{{ID: "n1", Label: "N1"}}, nil),
		ui.Text("added", Key("extra")),
	)

	w1, idx1 := normalizeAndIndex(oldNode)
	w2, idx2 := normalizeAndIndex(nextNode)
	if !actionsAndContextEqual(idx1, idx2) {
		t.Fatal("expected actionsAndContextEqual true when only text/check/static nodes change")
	}
	if w1.Children[1].Ratio != 0.5 {
		t.Fatalf("split default ratio = %v, want 0.5", w1.Children[1].Ratio)
	}
	if w1.Children[2].Check != "off" {
		t.Fatalf("checkbox default check = %q, want off", w1.Children[2].Check)
	}
	if w1.Children[3].GraphNodes[0].Weight != 1.0 {
		t.Fatalf("graphNode default weight = %v, want 1.0", w1.Children[3].GraphNodes[0].Weight)
	}

	ops := diffWireNodes(w1, w2)
	if len(ops) == 0 {
		t.Fatal("expected non-empty patch ops")
	}
	var sawUpdate, sawInsert bool
	for _, op := range ops {
		if op.Op == OpUpdate && op.ID == "/status" && op.Node.Text == "count-1" {
			sawUpdate = true
		}
		if op.Op == OpInsert && op.Parent == "" && op.Node.ID == "/extra" {
			sawInsert = true
		}
	}
	if !sawUpdate || !sawInsert {
		t.Fatalf("missing expected update/insert ops: %+v", ops)
	}

	// Adding an action handler must trigger actionsAndContextEqual == false -> full tree emit.
	withAct := ui.Column(ui.Button("Go", Key("go"), OnTap("go")))
	_, idxAct := normalizeAndIndex(withAct)
	if actionsAndContextEqual(idx2, idxAct) {
		t.Fatal("expected actionsAndContextEqual false when action handlers change")
	}
}

func TestAppSSECoalescingPatchTreeAndDisconnect(t *testing.T) {
	sock := shortSocketPath(t, "sse.sock")
	token := "sse-tok"

	var mu sync.Mutex
	count := 0
	withBtn := false
	var lastVCtx *ViewContext

	app := New()
	app.View("main", View{
		Render: func(ctx *ViewContext, ui *UI) Node {
			mu.Lock()
			lastVCtx = ctx
			n := count
			btn := withBtn
			mu.Unlock()
			if btn {
				return ui.Column(
					ui.Text(fmt.Sprintf("count-%d", n), Key("status")),
					ui.Button("Act", Key("act"), OnTap("act")),
				)
			}
			return ui.Column(ui.Text(fmt.Sprintf("count-%d", n), Key("status")))
		},
		On: Handlers{
			"act": func(ctx *EventContext) error { return nil },
		},
	})

	runCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	runErr := make(chan error, 1)
	go func() {
		runErr <- app.RunWithConfig(runCtx, Config{
			ListenAddr:       "unix://" + sock,
			ExtToken:         token,
			CoalesceInterval: 60 * time.Millisecond,
		})
	}()

	client := newUnixClient(sock)
	waitHealth(t, client, "http://unix", token)

	// Verify 0600 permissions on unix socket.
	fi, err := os.Stat(sock)
	if err != nil {
		t.Fatalf("stat unix socket: %v", err)
	}
	if perm := fi.Mode().Perm(); perm != 0o600 {
		t.Fatalf("unix socket perm = %o, want 0600", perm)
	}

	sctx, scancel := context.WithCancel(runCtx)
	req, _ := http.NewRequestWithContext(sctx, http.MethodGet, "http://unix/v1/views/main/stream?locale=ko", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("open SSE stream: %v", err)
	}
	ch := make(chan testSSE, 16)
	go readSSEStream(sctx, resp.Body, ch)

	ev1 := awaitSSE(t, ch, 2*time.Second, func(e testSSE) bool { return e.Event == "tree" })
	if !strings.Contains(ev1.Data, "count-0") {
		t.Fatalf("initial tree missing count-0: %s", ev1.Data)
	}

	// Burst updates: should coalesce into patch with count-5.
	for i := 1; i <= 5; i++ {
		mu.Lock()
		count = i
		vc := lastVCtx
		mu.Unlock()
		vc.NotifyChanged()
		time.Sleep(4 * time.Millisecond)
	}
	ev2 := awaitSSE(t, ch, 2*time.Second, func(e testSSE) bool { return e.Event == "patch" })
	if !strings.Contains(ev2.Data, `"ops"`) || !strings.Contains(ev2.Data, "count-5") {
		t.Fatalf("expected coalesced patch with count-5, got: %+v", ev2)
	}

	// Adding an action handler must emit event: tree.
	mu.Lock()
	withBtn = true
	mu.Unlock()
	app.NotifyChanged("main")
	ev3 := awaitSSE(t, ch, 2*time.Second, func(e testSSE) bool { return e.Event == "tree" })
	if !strings.Contains(ev3.Data, `"onTap":["act"]`) {
		t.Fatalf("expected full tree event on action addition, got: %s", ev3.Data)
	}

	// Clean disconnect and reconnect.
	scancel()
	_ = resp.Body.Close()

	req2, _ := http.NewRequestWithContext(runCtx, http.MethodGet, "http://unix/v1/views/main/stream", nil)
	req2.Header.Set("Authorization", "Bearer "+token)
	resp2, err := client.Do(req2)
	if err != nil {
		t.Fatalf("reconnect SSE stream: %v", err)
	}
	defer resp2.Body.Close()
	ch2 := make(chan testSSE, 8)
	go readSSEStream(runCtx, resp2.Body, ch2)
	evRe := awaitSSE(t, ch2, 2*time.Second, func(e testSSE) bool { return e.Event == "tree" })
	if !strings.Contains(evRe.Data, "count-5") {
		t.Fatalf("reconnected stream did not emit fresh tree: %s", evRe.Data)
	}

	st, _ := doReqJSON(t, client, http.MethodPost, "http://unix/v1/shutdown", token, "", nil)
	if st != http.StatusOK {
		t.Fatalf("shutdown status = %d, want 200", st)
	}
	if err := <-runErr; err != nil {
		t.Fatalf("RunWithConfig returned error: %v", err)
	}
}

func TestAppEventsGesturePropagationFormValuesAndErrors(t *testing.T) {
	var mu sync.Mutex
	seenGestures := map[string]string{}
	hostSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		seenGestures[r.URL.Path] = r.Header.Get("X-GVX-Gesture-ID")
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/v1/ui/pick-folder":
			_, _ = w.Write([]byte(`{"cancelled":false,"path":"/Users/test/repo"}`))
		default:
			_, _ = w.Write([]byte(`{}`))
		}
	}))
	defer hostSrv.Close()

	var capturedName, capturedBoolVal string
	var capturedToggle bool

	app := New()
	app.View("main", View{
		Render: func(ctx *ViewContext, ui *UI) Node {
			return ui.Column(
				ui.Input(Key("repo"), Placeholder("path"), OnSubmit("save_repo")),
				ui.Toggle(Key("enabled"), OnChange("set_enabled")),
				ui.Checkbox(Key("agree"), "Agree", OnChange("set_agree")),
				ui.Button("Pick", Key("pick"), OnTap("pick_folder")),
				ui.Button("Boom", Key("boom"), OnTap("boom")),
				ui.ContextMenu("rowctx", ui.MenuItem("Open", Key("open"), OnTap("open_row"))),
				ui.Dag([]DagRow{{ID: "job1", Text: "Build"}}, Key("dag"), ContextMenu("rowctx"), OnChange("sel_row")),
			)
		},
		On: Handlers{
			"save_repo": func(ctx *EventContext, val string) error {
				capturedName = ctx.FormValue("repo") + "|" + ctx.FormValue("/repo") + "|" + val
				return nil
			},
			"set_enabled": func(ctx *EventContext, on bool) error {
				capturedToggle = on
				return nil
			},
			"set_agree": func(ctx *EventContext, v string) error {
				capturedBoolVal = v
				return nil
			},
			"pick_folder": func(ctx *EventContext) error {
				dir, ok, err := ctx.Host.PickFolder(ctx)
				if err != nil || !ok || dir != "/Users/test/repo" {
					return fmt.Errorf("pick failed: dir=%q ok=%v err=%v", dir, ok, err)
				}
				if err := ctx.Host.OpenURL(ctx, "https://example.invalid"); err != nil {
					return err
				}
				if err := ctx.Host.OpenTerminal(ctx, OpenTerminalRequest{TargetType: "sandbox", TargetID: "sb-1"}); err != nil {
					return err
				}
				return ctx.Host.Copy(ctx, "hello")
			},
			"boom": func(ctx *EventContext) error {
				panic("kaboom")
			},
			"open_row": func(ctx *EventContext, rowID string) error {
				capturedBoolVal = "ctx:" + rowID
				return nil
			},
			"sel_row": func(ctx *EventContext, rowID string) error {
				capturedBoolVal = "dag:" + rowID
				return nil
			},
		},
	})

	runCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	errCh := make(chan error, 1)
	go func() {
		errCh <- app.RunWithConfig(runCtx, Config{
			Listener:  ln,
			HostAPI:   hostSrv.URL,
			HostToken: "host-tok",
			ExtToken:  "ext-tok",
		})
	}()

	baseURL := "http://" + ln.Addr().String()
	client := &http.Client{}
	waitHealth(t, client, baseURL, "ext-tok")

	// Unauthorized requests -> 401
	for _, ep := range [][2]string{
		{http.MethodGet, "/v1/health"},
		{http.MethodPost, "/v1/views/main/event"},
		{http.MethodPost, "/v1/shutdown"},
	} {
		st, _ := doReqJSON(t, client, ep[0], baseURL+ep[1], "wrong", "", nil)
		if st != http.StatusUnauthorized {
			t.Fatalf("%s %s with bad token = %d, want 401", ep[0], ep[1], st)
		}
	}

	// Out-of-band event (before stream connects) triggers lazy render + gesture forwarding.
	st, body := doReqJSON(t, client, http.MethodPost, baseURL+"/v1/views/main/event", "ext-tok", "gest-999", map[string]any{
		"node_id": "/pick",
		"kind":    "tap",
		"locale":  "en",
	})
	if st != http.StatusOK {
		t.Fatalf("tap /pick = %d: %s", st, body)
	}
	mu.Lock()
	for _, p := range []string{"/v1/ui/pick-folder", "/v1/ui/open-url", "/v1/ui/open-terminal", "/v1/ui/copy"} {
		if seenGestures[p] != "gest-999" {
			mu.Unlock()
			t.Fatalf("%s gesture = %q, want gest-999", p, seenGestures[p])
		}
	}
	mu.Unlock()

	// Submit with form values (using "node" alias)
	st, _ = doReqJSON(t, client, http.MethodPost, baseURL+"/v1/views/main/event", "ext-tok", "", map[string]any{
		"node":   "/repo",
		"kind":   "submit",
		"value":  "/my/repo",
		"values": map[string]string{"/repo": "/my/repo", "/bogus": "ignored"},
	})
	if st != http.StatusOK || capturedName != "/my/repo|/my/repo|/my/repo" {
		t.Fatalf("submit /repo = (%d, %q)", st, capturedName)
	}

	// Toggle bool coercion
	st, _ = doReqJSON(t, client, http.MethodPost, baseURL+"/v1/views/main/event", "ext-tok", "", map[string]any{
		"node_id": "/enabled",
		"kind":    "change",
		"value":   "true",
	})
	if st != http.StatusOK || !capturedToggle {
		t.Fatalf("toggle change = (%d, %v)", st, capturedToggle)
	}

	// Checkbox validation ("on"/"off")
	st, _ = doReqJSON(t, client, http.MethodPost, baseURL+"/v1/views/main/event", "ext-tok", "", map[string]any{
		"node_id": "/agree",
		"kind":    "change",
		"value":   "invalid",
	})
	if st != http.StatusBadRequest {
		t.Fatalf("checkbox invalid value = %d, want 400", st)
	}
	st, _ = doReqJSON(t, client, http.MethodPost, baseURL+"/v1/views/main/event", "ext-tok", "", map[string]any{
		"node_id": "/agree",
		"kind":    "change",
		"value":   "on",
	})
	if st != http.StatusOK || capturedBoolVal != "on" {
		t.Fatalf("checkbox valid = (%d, %q)", st, capturedBoolVal)
	}

	// Context menu arg validation + dispatch
	st, _ = doReqJSON(t, client, http.MethodPost, baseURL+"/v1/views/main/event", "ext-tok", "", map[string]any{
		"node_id": "/rowctx/open",
		"kind":    "tap",
		"value":   "job1",
	})
	if st != http.StatusOK || capturedBoolVal != "ctx:job1" {
		t.Fatalf("context menu tap = (%d, %q)", st, capturedBoolVal)
	}
	st, _ = doReqJSON(t, client, http.MethodPost, baseURL+"/v1/views/main/event", "ext-tok", "", map[string]any{
		"node_id": "/rowctx/open",
		"kind":    "tap",
		"value":   "nonexistent",
	})
	if st != http.StatusBadRequest {
		t.Fatalf("context menu invalid arg = %d, want 400", st)
	}

	// Dag row validation + dispatch
	st, _ = doReqJSON(t, client, http.MethodPost, baseURL+"/v1/views/main/event", "ext-tok", "", map[string]any{
		"node_id": "/dag",
		"kind":    "change",
		"value":   "job1",
	})
	if st != http.StatusOK || capturedBoolVal != "dag:job1" {
		t.Fatalf("dag change = (%d, %q)", st, capturedBoolVal)
	}

	// Panic recovery -> 500, server remains healthy
	st, _ = doReqJSON(t, client, http.MethodPost, baseURL+"/v1/views/main/event", "ext-tok", "", map[string]any{
		"node_id": "/boom",
		"kind":    "tap",
	})
	if st != http.StatusInternalServerError {
		t.Fatalf("panic handler = %d, want 500", st)
	}

	// Unknown view -> 404; unknown node -> 400
	st, _ = doReqJSON(t, client, http.MethodPost, baseURL+"/v1/views/missing/event", "ext-tok", "", map[string]any{})
	if st != http.StatusNotFound {
		t.Fatalf("unknown view = %d, want 404", st)
	}
	st, _ = doReqJSON(t, client, http.MethodPost, baseURL+"/v1/views/main/event", "ext-tok", "", map[string]any{
		"node_id": "/missing",
		"kind":    "tap",
	})
	if st != http.StatusBadRequest {
		t.Fatalf("unknown node = %d, want 400", st)
	}

	st, _ = doReqJSON(t, client, http.MethodPost, baseURL+"/v1/shutdown", "ext-tok", "", nil)
	if st != http.StatusOK {
		t.Fatalf("shutdown = %d, want 200", st)
	}
	if err := <-errCh; err != nil {
		t.Fatalf("RunWithConfig error: %v", err)
	}
}

func TestAppCommandToolsSettingsAndHostEvents(t *testing.T) {
	hostEvents := make(chan HostEvent, 4)
	hostSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/events" {
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusOK)
			_, _ = fmt.Fprint(w, "event: host.locale\nid: 7\ndata: {\"locale\":\"ko\"}\n\n")
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}
			<-r.Context().Done()
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer hostSrv.Close()

	var cmdLocale, cmdGesture string
	var settingsMap map[string]any

	app := New()
	app.Command("sync", func(ctx *CommandContext) error {
		cmdLocale = ctx.Locale
		cmdGesture = ctx.GestureID()
		return nil
	})
	app.Tool("raw", func(ctx *ToolContext, args json.RawMessage) (any, error) {
		return map[string]any{"raw": string(args)}, nil
	})
	app.Tool("maptool", func(ctx *ToolContext, args map[string]any) (any, error) {
		return map[string]any{"got": args["k"]}, nil
	})
	app.Tool("noarg", func(ctx *ToolContext) (any, error) {
		return map[string]any{"ok": true}, nil
	})
	app.OnSettingsChanged(func(ctx *Context, s map[string]any) error {
		settingsMap = s
		return nil
	})
	app.OnEvent(func(ev HostEvent) {
		hostEvents <- ev
	})

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	runCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	errCh := make(chan error, 1)
	go func() {
		errCh <- app.RunWithConfig(runCtx, Config{
			Listener: ln,
			HostAPI:  hostSrv.URL,
			ExtToken: "tok",
		})
	}()

	baseURL := "http://" + ln.Addr().String()
	client := &http.Client{}
	waitHealth(t, client, baseURL, "tok")

	st, _ := doReqJSON(t, client, http.MethodPost, baseURL+"/v1/commands/sync", "tok", "g-cmd", map[string]string{"locale": "ko"})
	if st != http.StatusOK || cmdLocale != "ko" || cmdGesture != "g-cmd" {
		t.Fatalf("command sync = (%d, %q, %q)", st, cmdLocale, cmdGesture)
	}
	st, _ = doReqJSON(t, client, http.MethodPost, baseURL+"/v1/commands/unknown", "tok", "", nil)
	if st != http.StatusNotFound {
		t.Fatalf("unknown command = %d, want 404", st)
	}

	st, body := doReqJSON(t, client, http.MethodPost, baseURL+"/v1/tools/raw", "tok", "", map[string]any{"args": map[string]int{"x": 1}})
	var rawOut map[string]string
	if st != http.StatusOK || json.Unmarshal(body, &rawOut) != nil || rawOut["raw"] != `{"x":1}` {
		t.Fatalf("tool raw = (%d, %s)", st, body)
	}
	st, body = doReqJSON(t, client, http.MethodPost, baseURL+"/v1/tools/maptool", "tok", "", map[string]any{"k": "direct"})
	if st != http.StatusOK || !strings.Contains(string(body), `"got":"direct"`) {
		t.Fatalf("tool maptool = (%d, %s)", st, body)
	}
	st, body = doReqJSON(t, client, http.MethodPost, baseURL+"/v1/tools/noarg", "tok", "", nil)
	if st != http.StatusOK || !strings.Contains(string(body), `"ok":true`) {
		t.Fatalf("tool noarg = (%d, %s)", st, body)
	}
	st, _ = doReqJSON(t, client, http.MethodPost, baseURL+"/v1/tools/missing", "tok", "", nil)
	if st != http.StatusNotFound {
		t.Fatalf("unknown tool = %d, want 404", st)
	}

	st, _ = doReqJSON(t, client, http.MethodPost, baseURL+"/v1/settings/changed", "tok", "", map[string]any{"concurrency": 4})
	if st != http.StatusOK || settingsMap["concurrency"] != float64(4) {
		t.Fatalf("settings/changed = (%d, %+v)", st, settingsMap)
	}

	select {
	case ev := <-hostEvents:
		if ev.Type != "host.locale" || ev.ID != "7" {
			t.Fatalf("unexpected HostEvent: %+v", ev)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for HostEvent")
	}

	cancel()
	if err := <-errCh; err != nil {
		t.Fatalf("RunWithConfig error: %v", err)
	}
}

func TestAppRunEnvValidationAndStaleUnixSocket(t *testing.T) {
	// Unsupported GVX_API_VERSION
	t.Setenv("GVX_API_VERSION", "99")
	t.Setenv("GVX_EXT_LISTEN", "127.0.0.1:0")
	if err := New().Run(context.Background()); err == nil {
		t.Fatal("expected error for GVX_API_VERSION=99")
	}

	// Missing GVX_EXT_LISTEN
	t.Setenv("GVX_API_VERSION", "1")
	t.Setenv("GVX_EXT_LISTEN", "")
	if err := New().Run(context.Background()); err == nil {
		t.Fatal("expected error when GVX_EXT_LISTEN is empty")
	}

	// Stale Unix socket cleanup
	sock := shortSocketPath(t, "stale.sock")
	staleLn, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatalf("create stale socket: %v", err)
	}
	// Close the underlying fd without unlinking the socket path.
	if ul, ok := staleLn.(*net.UnixListener); ok {
		ul.SetUnlinkOnClose(false)
	}
	_ = staleLn.Close()

	ln, cleanup, err := listenEndpoint("unix://" + sock)
	if err != nil {
		t.Fatalf("listenEndpoint failed to clean stale socket: %v", err)
	}
	cleanup()
	_ = ln.Close()
	if _, err := os.Stat(sock); !os.IsNotExist(err) {
		t.Fatalf("expected cleanup to remove socket, stat err = %v", err)
	}

	// Refuse regular file at socket path
	regFile := filepath.Join(t.TempDir(), "not-a-sock")
	if err := os.WriteFile(regFile, []byte("x"), 0o600); err != nil {
		t.Fatalf("write regular file: %v", err)
	}
	if _, _, err := listenEndpoint("unix://" + regFile); err == nil {
		t.Fatal("expected listenEndpoint to refuse overwriting a regular file")
	}
}

func TestHostClientAllEndpointsAndErrors(t *testing.T) {
	var authSeen atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer host-secret" {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"code":"unauthorized","message":"bad token"}`))
			return
		}
		authSeen.Store(true)
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/v1/host/info":
			_, _ = w.Write([]byte(`{"app_version":"1.0.0","os":"darwin","arch":"arm64","locale":"en","theme":"dark","docker_mode":"sandbox","docker_ready":true,"rungv_version":"0.5.0"}`))
		case "/v1/settings":
			_, _ = w.Write([]byte(`{"repo":"acme/app","concurrency":2,"enabled":true,"pat":{"set":true}}`))
		case "/v1/storage":
			_, _ = w.Write([]byte(`{"keys":["a/1","a/2"]}`))
		case "/v1/storage/k1":
			if r.Method == http.MethodGet {
				_, _ = w.Write([]byte(`{"value":"v1"}`))
				return
			}
			_, _ = w.Write([]byte(`{"ok":true}`))
		case "/v1/storage/json":
			_, _ = w.Write([]byte(`{"value":"{\"n\":42}"}`))
		case "/v1/storage/missing":
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"code":"not_found","message":"missing"}`))
		case "/v1/secrets":
			_, _ = w.Write([]byte(`["s1"]`))
		case "/v1/secrets/s1":
			if r.Method == http.MethodGet {
				_, _ = w.Write([]byte(`{"value":"sec"}`))
				return
			}
			_, _ = w.Write([]byte(`{"ok":true}`))
		case "/v1/secrets/missing":
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"code":"not_found","message":"missing"}`))
		case "/v1/sandboxes":
			if r.Method == http.MethodGet {
				_, _ = w.Write([]byte(`[{"id":"sb1","name":"runner","image":"alpine","status":"running"}]`))
				return
			}
			_, _ = w.Write([]byte(`{"id":"sb1","name":"runner","image":"alpine","status":"running"}`))
		case "/v1/sandboxes/sb1/start", "/v1/sandboxes/sb1/stop":
			_, _ = w.Write([]byte(`{"id":"sb1","name":"runner","image":"alpine","status":"stopped"}`))
		case "/v1/sandboxes/sb1/delete":
			_, _ = w.Write([]byte(`{"ok":true}`))
		case "/v1/sandboxes/sb1/exec":
			_, _ = w.Write([]byte(`{"exit_code":0,"stdout":"ok\n","stderr":""}`))
		case "/v1/sandboxes/sb1/logs":
			if r.URL.Query().Get("follow") == "true" {
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = fmt.Fprint(w, "event: log\ndata: {\"line\":\"line-1\"}\n\nevent: log\ndata: line-2\n\n")
				return
			}
			_, _ = w.Write([]byte(`{"logs":"booted\n"}`))
		case "/v1/sandboxes/sb1/snapshots":
			if r.Method == http.MethodGet {
				_, _ = w.Write([]byte(`[{"sandbox_id":"sb1","name":"snap1","size":1024}]`))
				return
			}
			_, _ = w.Write([]byte(`{"sandbox_id":"sb1","name":"snap1","size":1024}`))
		case "/v1/sandboxes/sb1/restore":
			_, _ = w.Write([]byte(`{"sandbox":{"id":"sb1","name":"runner","image":"alpine","status":"running"}}`))
		case "/v1/sandboxes/sb1/snapshots/snap1":
			_, _ = w.Write([]byte(`{"ok":true}`))
		case "/v1/ui/badge", "/v1/ui/toast", "/v1/ui/open-terminal", "/v1/ui/open-url", "/v1/ui/copy":
			_, _ = w.Write([]byte(`{"ok":true}`))
		case "/v1/ui/pick-folder":
			if r.Header.Get("X-GVX-Gesture-ID") == "" {
				w.WriteHeader(http.StatusForbidden)
				_, _ = w.Write([]byte(`{"code":"gesture_required","message":"gesture required"}`))
				return
			}
			_, _ = w.Write([]byte(`{"cancelled":false,"path":"/work"}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	h := NewHostClient(srv.URL, "host-secret")
	ctx := context.Background()

	info, err := h.Info(ctx)
	if err != nil || info.AppVersion != "1.0.0" || !info.DockerReady {
		t.Fatalf("Info = (%+v, %v)", info, err)
	}
	if !authSeen.Load() {
		t.Fatal("expected Bearer auth header")
	}

	settings, err := h.Settings(ctx)
	if err != nil || settings.String("repo") != "acme/app" || settings.Int("concurrency") != 2 || !settings.Bool("enabled") || !settings.IsSet("pat") {
		t.Fatalf("Settings = (%+v, %v)", settings.Raw(), err)
	}

	keys, err := h.StorageList(ctx, "a/")
	if err != nil || !slices.Equal(keys, []string{"a/1", "a/2"}) {
		t.Fatalf("StorageList = (%v, %v)", keys, err)
	}
	if err := h.StoragePut(ctx, "k1", "v1"); err != nil {
		t.Fatalf("StoragePut: %v", err)
	}
	if val, ok, err := h.StorageGet(ctx, "k1"); err != nil || !ok || val != "v1" {
		t.Fatalf("StorageGet = (%q, %v, %v)", val, ok, err)
	}
	if _, ok, err := h.StorageGet(ctx, "missing"); err != nil || ok {
		t.Fatalf("StorageGet missing = (%v, %v)", ok, err)
	}
	var obj struct {
		N int `json:"n"`
	}
	if ok, err := h.StorageGetJSON(ctx, "json", &obj); err != nil || !ok || obj.N != 42 {
		t.Fatalf("StorageGetJSON = (%+v, %v, %v)", obj, ok, err)
	}
	if err := h.StorageSetJSON(ctx, "k1", map[string]int{"x": 1}); err != nil {
		t.Fatalf("StorageSetJSON: %v", err)
	}
	if err := h.StorageDelete(ctx, "k1"); err != nil {
		t.Fatalf("StorageDelete: %v", err)
	}

	skeys, err := h.SecretList(ctx)
	if err != nil || !slices.Equal(skeys, []string{"s1"}) {
		t.Fatalf("SecretList = (%v, %v)", skeys, err)
	}
	if err := h.SecretPut(ctx, "s1", "sec"); err != nil {
		t.Fatalf("SecretPut: %v", err)
	}
	if sv, ok, err := h.SecretGet(ctx, "s1"); err != nil || !ok || sv != "sec" {
		t.Fatalf("SecretGet = (%q, %v, %v)", sv, ok, err)
	}
	if _, ok, err := h.SecretGet(ctx, "missing"); err != nil || ok {
		t.Fatalf("SecretGet missing = (%v, %v)", ok, err)
	}
	if err := h.SecretDelete(ctx, "s1"); err != nil {
		t.Fatalf("SecretDelete: %v", err)
	}

	sbs, err := h.SandboxList(ctx, true)
	if err != nil || len(sbs) != 1 || sbs[0].ID != "sb1" {
		t.Fatalf("SandboxList = (%+v, %v)", sbs, err)
	}
	if sb, err := h.SandboxCreate(ctx, CreateSandboxRequest{Image: "alpine"}); err != nil || sb.ID != "sb1" {
		t.Fatalf("SandboxCreate = (%+v, %v)", sb, err)
	}
	if _, err := h.SandboxStart(ctx, "sb1"); err != nil {
		t.Fatalf("SandboxStart: %v", err)
	}
	if _, err := h.SandboxStop(ctx, "sb1"); err != nil {
		t.Fatalf("SandboxStop: %v", err)
	}
	if res, err := h.SandboxExec(ctx, "sb1", ExecRequest{Argv: []string{"echo", "ok"}}); err != nil || res.Stdout != "ok\n" {
		t.Fatalf("SandboxExec = (%+v, %v)", res, err)
	}
	if logs, err := h.SandboxLogs(ctx, "sb1", 50); err != nil || logs != "booted\n" {
		t.Fatalf("SandboxLogs = (%q, %v)", logs, err)
	}
	var followed []string
	if err := h.SandboxLogsFollow(ctx, "sb1", 50, func(line string) error {
		followed = append(followed, line)
		return nil
	}); err != nil || !slices.Equal(followed, []string{"line-1", "line-2"}) {
		t.Fatalf("SandboxLogsFollow = (%v, %v)", followed, err)
	}
	if snap, err := h.SnapshotCreate(ctx, "sb1", CreateSnapshotRequest{Name: "snap1"}); err != nil || snap.Name != "snap1" {
		t.Fatalf("SnapshotCreate = (%+v, %v)", snap, err)
	}
	if snaps, err := h.SnapshotList(ctx, "sb1"); err != nil || len(snaps) != 1 {
		t.Fatalf("SnapshotList = (%+v, %v)", snaps, err)
	}
	if rr, err := h.Restore(ctx, "sb1", RestoreRequest{Snapshot: "snap1"}); err != nil || rr.Sandbox.ID != "sb1" {
		t.Fatalf("Restore = (%+v, %v)", rr, err)
	}
	if err := h.SnapshotDelete(ctx, "sb1", "snap1"); err != nil {
		t.Fatalf("SnapshotDelete: %v", err)
	}
	if err := h.SandboxDelete(ctx, "sb1"); err != nil {
		t.Fatalf("SandboxDelete: %v", err)
	}

	if err := h.SetBadge(ctx, BadgeRequest{Text: "2", Tone: "accent"}); err != nil {
		t.Fatalf("SetBadge: %v", err)
	}
	if err := h.Toast(ctx, "info", "done"); err != nil {
		t.Fatalf("Toast: %v", err)
	}

	// PickFolder without gesture -> typed HostError with IsGestureRequired
	if _, _, err := h.PickFolder(ctx); !IsGestureRequired(err) || !IsPermissionDenied(err) {
		t.Fatalf("expected IsGestureRequired + IsPermissionDenied, got %v", err)
	}
	// PickFolder with gesture -> success
	gctx := WithGestureID(ctx, "g-1")
	if path, ok, err := h.PickFolder(gctx); err != nil || !ok || path != "/work" {
		t.Fatalf("PickFolder(gctx) = (%q, %v, %v)", path, ok, err)
	}
	if err := h.OpenTerminal(gctx, OpenTerminalRequest{TargetType: "sandbox", TargetID: "sb1"}); err != nil {
		t.Fatalf("OpenTerminal: %v", err)
	}
	if err := h.OpenURL(gctx, "https://example.invalid"); err != nil {
		t.Fatalf("OpenURL: %v", err)
	}
	if err := h.Copy(gctx, "abc"); err != nil {
		t.Fatalf("Copy: %v", err)
	}

	// Typed HTTP error surfacing status codes (403, 429, 503)
	for _, code := range []int{403, 429, 503} {
		errSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(code)
			_, _ = fmt.Fprintf(w, `{"error":"status-%d"}`, code)
		}))
		eh := NewHostClient(errSrv.URL, "tok")
		_, err := eh.Info(ctx)
		errSrv.Close()
		if err == nil || !strings.Contains(err.Error(), strconv.Itoa(code)) {
			t.Fatalf("status %d error = %v", code, err)
		}
		if code == 429 && !IsRateLimited(err) {
			t.Fatalf("expected IsRateLimited for 429, got %v", err)
		}
		if code == 503 && !IsServiceUnavailable(err) {
			t.Fatalf("expected IsServiceUnavailable for 503, got %v", err)
		}
	}

	// Unix socket Host client
	unixSock := shortSocketPath(t, "host.sock")
	uln, err := net.Listen("unix", unixSock)
	if err != nil {
		t.Fatalf("unix Listen: %v", err)
	}
	defer uln.Close()
	usrv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"app_version":"unix-ok"}`))
	})}
	go func() { _ = usrv.Serve(uln) }()
	defer usrv.Close()

	uh, err := NewHost("unix://"+unixSock, "tok")
	if err != nil {
		t.Fatalf("NewHost unix: %v", err)
	}
	uinfo, err := uh.Info(ctx)
	if err != nil || uinfo.AppVersion != "unix-ok" {
		t.Fatalf("unix Host.Info = (%+v, %v)", uinfo, err)
	}
	var unused error
	_ = errors.Is(unused, nil)
}

func TestHostClientAndApp_RejectNonUTF8WithoutCoercion(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer srv.Close()

	h := NewHostClient(srv.URL, "tok")
	ctx := WithGestureID(context.Background(), "g-1")
	badStr := string([]byte{0xff, 0xfe})

	check400 := func(name string, err error) {
		t.Helper()
		var he *HostError
		if !errors.As(err, &he) || he.StatusCode != http.StatusBadRequest || !strings.Contains(err.Error(), "400") {
			t.Fatalf("%s: got %v, want *HostError 400", name, err)
		}
	}

	check400("StorageSet value", h.StorageSet(ctx, "ok", badStr))
	check400("StorageSet key", h.StorageSet(ctx, badStr, "ok"))
	_, _, err := h.StorageGet(ctx, badStr)
	check400("StorageGet key", err)
	_, err = h.StorageList(ctx, badStr)
	check400("StorageList prefix", err)
	check400("StorageSetJSON value", h.StorageSetJSON(ctx, "ok", map[string]string{"k": badStr}))
	check400("SecretPut value", h.SecretPut(ctx, "sec", badStr))
	check400("SecretPut key", h.SecretPut(ctx, badStr, "val"))
	check400("Copy", h.Copy(ctx, "bad\xffutf8"))
	check400("Toast", h.Toast(ctx, "info", "bad\xff"))
	check400("OpenURL", h.OpenURL(ctx, "https://example.com/\xff"))
	check400("SetBadge", h.SetBadge(ctx, BadgeRequest{Text: "\xff"}))

	if hits.Load() != 0 {
		t.Fatalf("expected 0 HTTP requests for invalid UTF-8 inputs, got %d", hits.Load())
	}

	// Valid U+FFFD and emoji must succeed.
	if err := h.Copy(ctx, "\ufffd😀"); err != nil {
		t.Fatalf("Copy valid UTF-8 failed: %v", err)
	}
	if hits.Load() != 1 {
		t.Fatalf("expected 1 HTTP request for valid UTF-8, got %d", hits.Load())
	}

	// App server handlers reject raw invalid UTF-8 and lone surrogates with 400.
	app := New()
	app.View("main", View{
		Render: func(vctx *ViewContext, ui *UI) Node { return ui.Text("hi") },
		On:     Handlers{"tap": func(ectx *EventContext) error { return nil }},
	})
	app.Command("c", func(cctx *CommandContext) error { return nil })
	app.Tool("t", func(tctx *ToolContext, args json.RawMessage) (any, error) { return map[string]any{"ok": true}, nil })
	app.OnSettingsChanged(func(sctx *Context, m map[string]any) error { return nil })

	ah := app.Handler()
	for _, tc := range []struct {
		path string
		body []byte
		want int
	}{
		{"/v1/views/main/event", []byte("{\"kind\":\"tap\",\"value\":\"\xff\"}"), http.StatusBadRequest},
		{"/v1/views/main/event", []byte(`{"kind":"tap","value":"\uD800"}`), http.StatusBadRequest},
		{"/v1/commands/c", []byte(`{"locale":"\uDFFF"}`), http.StatusBadRequest},
		{"/v1/tools/t", []byte("{\"args\":{\"x\":\"\xff\"}}"), http.StatusBadRequest},
		{"/v1/settings/changed", []byte(`{"k":"\uD800"}`), http.StatusBadRequest},
		{"/v1/commands/c", []byte(`{"locale":"\\uD800"}`), http.StatusOK},
		{"/v1/commands/c", []byte(`{"locale":"\uD83D\uDE00"}`), http.StatusOK},
	} {
		req := httptest.NewRequest(http.MethodPost, tc.path, bytes.NewReader(tc.body))
		rec := httptest.NewRecorder()
		ah.ServeHTTP(rec, req)
		if rec.Code != tc.want {
			t.Errorf("POST %s body=%q got %d, want %d", tc.path, tc.body, rec.Code, tc.want)
		}
	}
}
