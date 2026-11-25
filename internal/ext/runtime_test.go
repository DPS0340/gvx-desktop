package ext

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ironpark/gvx-desktop/internal/rungv"
	"github.com/ironpark/gvx-desktop/internal/sandbox"
	"github.com/ironpark/gvx-desktop/internal/uiblock"
)

func TestRuntimeBinary_GatingTraversalAndHungForceKill(t *testing.T) {
	dataDir := t.TempDir()
	extDir := t.TempDir()
	outsideBin := filepath.Join(t.TempDir(), "outside")
	if err := os.WriteFile(outsideBin, []byte("#!/bin/sh\nexit 0\n"), 0o700); err != nil {
		t.Fatal(err)
	}

	m := &Manifest{
		SchemaVersion: 1,
		ID:            "bin-rt-test",
		Version:       "0.1.0",
		Publisher:     "acme",
		Name:          "Bin Test",
		Runtime: RuntimeDecl{
			Type:          RuntimeBinary,
			DevEntrypoint: "bin/ext",
		},
	}
	gs, _ := LoadGrantStore(GrantsPath(dataDir))
	_ = gs.Approve(m.ID, m, "")

	// 1. DeveloperMode = false must refuse to start.
	rtOff := newBinaryRuntime(binaryRuntimeConfig{
		extID:         m.ID,
		dataDir:       dataDir,
		manifest:      m,
		source:        SourceSpec{Kind: SourceLocal, LocalPath: extDir},
		grants:        gs,
		developerMode: func() bool { return false },
		hostEndpoint:  "http://127.0.0.1:1",
		hostToken:     "htok",
		extToken:      "etok",
	})
	if err := rtOff.Start(context.Background()); err == nil || !strings.Contains(err.Error(), "Developer Mode") {
		t.Fatalf("expected Developer Mode refusal, got %v", err)
	}

	// 2. Traversal dev_entrypoint must be rejected.
	mTrav := *m
	mTrav.Runtime.DevEntrypoint = "../outside"
	rtTrav := newBinaryRuntime(binaryRuntimeConfig{
		extID:         m.ID,
		dataDir:       dataDir,
		manifest:      &mTrav,
		source:        SourceSpec{Kind: SourceLocal, LocalPath: extDir},
		grants:        gs,
		developerMode: func() bool { return true },
		hostEndpoint:  "http://127.0.0.1:1",
		hostToken:     "htok",
		extToken:      "etok",
	})
	if err := rtTrav.Start(context.Background()); err == nil {
		t.Fatal("expected traversal dev_entrypoint to be rejected")
	}

	// 3. Compile a real hung binary that ignores SIGTERM and hangs on /v1/shutdown,
	// verifying log capture and SIGKILL within Stop budget.
	binDir := filepath.Join(extDir, "bin")
	if err := os.MkdirAll(binDir, 0o700); err != nil {
		t.Fatal(err)
	}
	srcFile := filepath.Join(t.TempDir(), "main.go")
	const hungCode = `package main
import (
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
)
func main() {
	signal.Ignore(syscall.SIGTERM, os.Interrupt)
	fmt.Fprintln(os.Stderr, "hung-bin-started")
	listen := os.Getenv("GVX_EXT_LISTEN")
	var ln net.Listener
	var err error
	if strings.HasPrefix(listen, "unix://") {
		p := strings.TrimPrefix(listen, "unix://")
		_ = os.Remove(p)
		ln, err = net.Listen("unix", p)
	} else {
		ln, err = net.Listen("tcp", listen)
	}
	if err != nil {
		os.Exit(2)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/health", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(` + "`" + `{"status":"ok","version":"0.1.0"}` + "`" + `))
	})
	mux.HandleFunc("/v1/shutdown", func(w http.ResponseWriter, r *http.Request) {
		select {}
	})
	_ = http.Serve(ln, mux)
}
`
	if err := os.WriteFile(srcFile, []byte(hungCode), 0o600); err != nil {
		t.Fatal(err)
	}
	exeName := "ext"
	if runtime.GOOS == "windows" {
		exeName = "ext.exe"
	}
	m.Runtime.DevEntrypoint = "bin/" + exeName
	outBin := filepath.Join(binDir, exeName)
	buildCmd := exec.Command("go", "build", "-o", outBin, srcFile)
	if out, err := buildCmd.CombinedOutput(); err != nil {
		t.Fatalf("go build hung fixture: %v\n%s", err, out)
	}

	rt := newBinaryRuntime(binaryRuntimeConfig{
		extID:         m.ID,
		dataDir:       dataDir,
		manifest:      m,
		source:        SourceSpec{Kind: SourceLocal, LocalPath: extDir},
		grants:        gs,
		developerMode: func() bool { return true },
		hostEndpoint:  "http://127.0.0.1:1",
		hostToken:     "htok",
		extToken:      "etok",
	})
	if err := rt.Start(context.Background()); err != nil {
		t.Fatalf("rt.Start: %v", err)
	}

	// Verify log file and ring buffer captured stdout/stderr.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if strings.Contains(strings.Join(rt.Logs(), "\n"), "hung-bin-started") {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if !strings.Contains(strings.Join(rt.Logs(), "\n"), "hung-bin-started") {
		t.Fatalf("expected log line in rt.Logs(), got %v", rt.Logs())
	}

	startStop := time.Now()
	if err := rt.Stop(1200 * time.Millisecond); err != nil {
		t.Fatalf("rt.Stop: %v", err)
	}
	if elapsed := time.Since(startStop); elapsed > 4*time.Second {
		t.Fatalf("hung binary took too long to force kill: %v", elapsed)
	}

	// Also verify default 8s Stop budget skips waitHTTP on failed /v1/shutdown
	// and completes SIGKILL well within 8s.
	rt8 := newBinaryRuntime(binaryRuntimeConfig{
		extID:         m.ID + "-8s",
		dataDir:       dataDir,
		manifest:      m,
		source:        SourceSpec{Kind: SourceLocal, LocalPath: extDir},
		grants:        gs,
		developerMode: func() bool { return true },
		hostEndpoint:  "http://127.0.0.1:1",
		hostToken:     "htok",
		extToken:      "etok",
	})
	if err := rt8.Start(context.Background()); err != nil {
		t.Fatalf("rt8.Start: %v", err)
	}
	start8 := time.Now()
	if err := rt8.Stop(8 * time.Second); err != nil {
		t.Fatalf("rt8.Stop: %v", err)
	}
	if elapsed := time.Since(start8); elapsed > 7800*time.Millisecond {
		t.Fatalf("8s hung binary Stop took %v (> 7.8s)", elapsed)
	}
}

func TestRuntimeSandbox_RequiresSupervisorNeverDegrades(t *testing.T) {
	dataDir := t.TempDir()
	m := &Manifest{
		SchemaVersion: 1,
		ID:            "sbx-req-test",
		Version:       "0.1.0",
		Publisher:     "acme",
		Name:          "Sbx Test",
		Runtime: RuntimeDecl{
			Type:  RuntimeSandbox,
			Image: "alpine:3.20",
			Port:  DefaultRuntimePort,
		},
	}
	gs, _ := LoadGrantStore(GrantsPath(dataDir))
	_ = gs.Approve(m.ID, m, "")

	rt := newSandboxRuntime(sandboxRuntimeConfig{
		extID:      m.ID,
		dataDir:    dataDir,
		manifest:   m,
		source:     SourceSpec{Kind: SourceImage, ImageRef: "alpine:3.20"},
		grants:     gs,
		supervisor: nil,
	})
	err := rt.Start(context.Background())
	if err == nil || !strings.Contains(err.Error(), "supervisor") {
		t.Fatalf("expected explicit supervisor error without binary fallback, got %v", err)
	}
}

func TestExtClient_StreamViewValidationAndPatchReconnect(t *testing.T) {
	var streamCalls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer tok-1" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		switch r.URL.Path {
		case "/v1/views/bad/stream":
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusOK)
			// Invalid tree with RLO spoofing rune U+202E.
			_, _ = fmt.Fprintf(w, "event: tree\nid: 1\ndata: {\"kind\":\"column\",\"children\":[{\"kind\":\"text\",\"text\":\"bad\\u202etree\"}]}\n\n")
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}
			<-r.Context().Done()

		case "/v1/views/patch/stream":
			call := streamCalls.Add(1)
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusOK)
			fl, _ := w.(http.Flusher)
			if call == 1 {
				// Valid initial tree, followed by a malicious patch trying to inject an invalid icon.
				_, _ = fmt.Fprintf(w, "event: tree\nid: 1\ndata: {\"kind\":\"column\",\"children\":[{\"kind\":\"text\",\"key\":\"lbl\",\"text\":\"v1\"}]}\n\n")
				fl.Flush()
				badPatch, _ := json.Marshal(map[string]any{
					"ops": []*uiblock.Op{{
						Op:   uiblock.OpUpdate,
						ID:   "/lbl",
						Node: &uiblock.Node{ID: "/lbl", Kind: uiblock.KindIcon, Icon: "not-a-real-lucide-icon-xyz"},
					}},
				})
				_, _ = fmt.Fprintf(w, "event: patch\nid: 2\ndata: %s\n\n", badPatch)
				fl.Flush()
				<-r.Context().Done()
			} else {
				// On reconnect, serve clean fresh tree!
				_, _ = fmt.Fprintf(w, "event: tree\nid: 1\ndata: {\"kind\":\"column\",\"children\":[{\"kind\":\"text\",\"key\":\"lbl\",\"text\":\"recovered\"}]}\n\n")
				fl.Flush()
				<-r.Context().Done()
			}
		}
	}))
	defer srv.Close()

	c, err := newExtClient(srv.URL, "tok-1")
	if err != nil {
		t.Fatalf("newExtClient: %v", err)
	}

	// 1. Invalid initial tree emits uiblock.Update{Error: ...} without crashing.
	ctx1, cancel1 := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel1()
	gotErr := make(chan uiblock.Update, 1)
	go func() {
		_ = c.StreamView(ctx1, "bad", "en", func(u uiblock.Update) error {
			select {
			case gotErr <- u:
			default:
			}
			cancel1()
			return nil
		})
	}()
	select {
	case u := <-gotErr:
		if u.Error == "" || u.Tree != nil {
			t.Fatalf("expected error update for spoofed tree, got %+v", u)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for error update")
	}

	// 2. Malicious patch triggers reconnect and recovers fresh tree.
	ctx2, cancel2 := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancel2()
	var updates []uiblock.Update
	go func() {
		_ = c.StreamView(ctx2, "patch", "en", func(u uiblock.Update) error {
			updates = append(updates, u)
			if u.Tree != nil && len(u.Tree.Children) > 0 && u.Tree.Children[0].Text == "recovered" {
				cancel2()
			}
			return nil
		})
	}()
	<-ctx2.Done()
	if streamCalls.Load() < 2 {
		t.Fatalf("expected reconnect after invalid patch, streamCalls=%d", streamCalls.Load())
	}
	if len(updates) < 2 || updates[len(updates)-1].Tree == nil || updates[len(updates)-1].Tree.Children[0].Text != "recovered" {
		t.Fatalf("expected recovered tree after patch reconnect, got %+v", updates)
	}
}

func TestLogWriter_RotatesAtMaxSize(t *testing.T) {
	dataDir := t.TempDir()
	lw, err := newExtLogWriter(dataDir, "rot-ext")
	if err != nil {
		t.Fatalf("newExtLogWriter: %v", err)
	}
	lw.maxSize = 256
	for i := 0; i < 30; i++ {
		lw.AddLine(fmt.Sprintf("line-%02d-%s", i, strings.Repeat("x", 32)))
	}
	lw.Close()

	if _, err := os.Stat(filepath.Join(LogsDir(dataDir, "rot-ext"), "extension.log.1")); err != nil {
		t.Fatalf("expected rotated extension.log.1: %v", err)
	}
}

func TestExtClient_StreamViewMarkdownOnLinkPatchNoReconnect(t *testing.T) {
	var streamCalls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer tok-md" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		if r.URL.Path != "/v1/views/md/stream" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		streamCalls.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		fl, _ := w.(http.Flusher)
		_, _ = fmt.Fprintf(w, "event: tree\nid: 1\ndata: {\"kind\":\"column\",\"children\":[{\"kind\":\"markdown\",\"key\":\"doc\",\"text\":\"[one](https://example.com)\",\"onLink\":[\"openLink\"]}]}\n\n")
		fl.Flush()
		patchBytes, _ := json.Marshal(map[string]any{
			"ops": []*uiblock.Op{{
				Op:   uiblock.OpUpdate,
				ID:   "/doc",
				Node: &uiblock.Node{ID: "/doc", Kind: uiblock.KindMarkdown, Text: "[two](https://example.com/2)", Change: true},
			}},
		})
		_, _ = fmt.Fprintf(w, "event: patch\nid: 2\ndata: %s\n\n", patchBytes)
		fl.Flush()
		<-r.Context().Done()
	}))
	defer srv.Close()

	c, err := newExtClient(srv.URL, "tok-md")
	if err != nil {
		t.Fatalf("newExtClient: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	var updates []uiblock.Update
	done := make(chan error, 1)
	go func() {
		done <- c.StreamView(ctx, "md", "en", func(u uiblock.Update) error {
			updates = append(updates, u)
			if len(updates) >= 2 {
				cancel()
			}
			return nil
		})
	}()
	<-done

	if got := streamCalls.Load(); got != 1 {
		t.Fatalf("expected zero reconnects (streamCalls=1), got %d (updates=%+v)", got, updates)
	}
	if len(updates) != 2 || updates[1].Error != "" || updates[1].Tree == nil {
		t.Fatalf("expected valid patched markdown update without error, got %+v", updates)
	}
	if md := updates[1].Tree.Children[0]; md.Text != "[two](https://example.com/2)" || !md.Change {
		t.Fatalf("unexpected patched markdown node: %+v", md)
	}
}

func TestExtClient_StreamViewInputPlaceholderNearMaxTreeText(t *testing.T) {
	// Build a valid tree right below maxTreeText (256 KiB = 262144 B):
	// 8 markdown blocks totaling 261,600 bytes + an input (200B placeholder, empty text)
	// + a textarea (200B placeholder, empty text) => 262,000 bytes total.
	// Without placeholder deduplication in nodeToRawValidate, uiblock.Read would
	// double-count both placeholders to 262,400 > 262,144 and spuriously fail.
	chunk := strings.Repeat("a", 32700) // 8 * 32700 = 261,600 (< maxLongText 32768 each)
	phInput := strings.Repeat("p", 200)
	phArea := strings.Repeat("q", 200)

	initialTree := map[string]any{
		"kind": "column",
		"children": []any{
			map[string]any{"kind": "markdown", "key": "m0", "text": chunk},
			map[string]any{"kind": "markdown", "key": "m1", "text": chunk},
			map[string]any{"kind": "markdown", "key": "m2", "text": chunk},
			map[string]any{"kind": "markdown", "key": "m3", "text": chunk},
			map[string]any{"kind": "markdown", "key": "m4", "text": chunk},
			map[string]any{"kind": "markdown", "key": "m5", "text": chunk},
			map[string]any{"kind": "markdown", "key": "m6", "text": chunk},
			map[string]any{"kind": "markdown", "key": "m7", "text": chunk},
			map[string]any{"kind": "input", "key": "in1", "placeholder": phInput},
			map[string]any{"kind": "textarea", "key": "ta1", "placeholder": phArea},
		},
	}
	initialJSON, err := json.Marshal(initialTree)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := uiblock.Read(initialJSON); err != nil {
		t.Fatalf("expected initial tree to be valid under 256 KiB: %v", err)
	}

	var streamCalls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer tok-ph" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		if r.URL.Path != "/v1/views/form/stream" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		streamCalls.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		fl, _ := w.(http.Flusher)
		_, _ = fmt.Fprintf(w, "event: tree\nid: 1\ndata: %s\n\n", initialJSON)
		fl.Flush()

		updatedAreaPH := strings.Repeat("r", 200)
		patchBytes, _ := json.Marshal(map[string]any{
			"ops": []*uiblock.Op{{
				Op: uiblock.OpUpdate,
				ID: "/ta1",
				Node: &uiblock.Node{
					ID:          "/ta1",
					Kind:        uiblock.KindTextarea,
					Text:        updatedAreaPH,
					Placeholder: updatedAreaPH,
				},
			}},
		})
		_, _ = fmt.Fprintf(w, "event: patch\nid: 2\ndata: %s\n\n", patchBytes)
		fl.Flush()
		<-r.Context().Done()
	}))
	defer srv.Close()

	c, err := newExtClient(srv.URL, "tok-ph")
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	var updates []uiblock.Update
	done := make(chan error, 1)
	go func() {
		done <- c.StreamView(ctx, "form", "en", func(u uiblock.Update) error {
			updates = append(updates, u)
			if len(updates) >= 2 {
				cancel()
			}
			return nil
		})
	}()
	<-done

	if got := streamCalls.Load(); got != 1 {
		t.Fatalf("expected zero reconnects (streamCalls=1), got %d (updates=%+v)", got, updates)
	}
	if len(updates) != 2 || updates[1].Error != "" || updates[1].Tree == nil {
		t.Fatalf("expected valid patched tree near maxTreeText without placeholder double-count error, got %+v", updates)
	}
}

func TestExtClient_StreamViewAndDoJSONRejectNonUTF8AndLoneSurrogates(t *testing.T) {
	var patchCalls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer tok-surr" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		switch r.URL.Path {
		case "/v1/views/surr-patch/stream":
			call := patchCalls.Add(1)
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusOK)
			fl, _ := w.(http.Flusher)
			switch call {
			case 1:
				_, _ = fmt.Fprintf(w, "event: tree\nid: 1\ndata: {\"kind\":\"column\",\"children\":[{\"kind\":\"text\",\"key\":\"lbl\",\"text\":\"v1\"}]}\n\n")
				fl.Flush()
				_, _ = fmt.Fprintf(w, "event: patch\nid: 2\ndata: {\"ops\":[{\"op\":\"update\",\"id\":\"/lbl\",\"node\":{\"id\":\"/lbl\",\"kind\":\"text\",\"text\":\"\\uD800\"}}]}\n\n")
				fl.Flush()
				<-r.Context().Done()
			case 2:
				_, _ = fmt.Fprintf(w, "event: tree\nid: 1\ndata: {\"kind\":\"column\",\"children\":[{\"kind\":\"text\",\"key\":\"lbl\",\"text\":\"v2\"}]}\n\n")
				fl.Flush()
				_, _ = w.Write([]byte("event: patch\nid: 2\ndata: {\"ops\":[{\"op\":\"update\",\"id\":\"/lbl\",\"node\":{\"id\":\"/lbl\",\"kind\":\"text\",\"text\":\"\xff\"}}]}\n\n"))
				fl.Flush()
				<-r.Context().Done()
			default:
				_, _ = fmt.Fprintf(w, "event: tree\nid: 1\ndata: {\"kind\":\"column\",\"children\":[{\"kind\":\"text\",\"key\":\"lbl\",\"text\":\"recovered\"}]}\n\n")
				fl.Flush()
				<-r.Context().Done()
			}
		case "/v1/tools/surr":
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"text":"\uD800"}`))
		case "/v1/health":
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte("{\"status\":\"\xff\"}"))
		}
	}))
	defer srv.Close()

	c, err := newExtClient(srv.URL, "tok-surr")
	if err != nil {
		t.Fatalf("newExtClient: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var updates []uiblock.Update
	done := make(chan error, 1)
	go func() {
		done <- c.StreamView(ctx, "surr-patch", "en", func(u uiblock.Update) error {
			updates = append(updates, u)
			if u.Tree != nil && len(u.Tree.Children) > 0 && u.Tree.Children[0].Text == "recovered" {
				cancel()
			}
			return nil
		})
	}()
	<-done

	if patchCalls.Load() < 3 {
		t.Fatalf("expected reconnects after lone-surrogate and raw non-UTF-8 patches, got %d calls", patchCalls.Load())
	}
	for _, u := range updates {
		if len(u.Ops) > 0 {
			t.Fatalf("unexpected patch update accepted despite lone surrogate / invalid UTF-8: %+v", u)
		}
		if u.Tree != nil && len(u.Tree.Children) > 0 && strings.Contains(u.Tree.Children[0].Text, "\ufffd") {
			t.Fatalf("coerced U+FFFD leaked into tree update: %+v", u.Tree.Children[0])
		}
	}

	if _, err := c.InvokeTool(context.Background(), "surr", nil, "en"); err == nil {
		t.Fatal("InvokeTool accepted lone surrogate in JSON response")
	}
	if err := c.Health(context.Background()); err == nil {
		t.Fatal("Health accepted invalid UTF-8 in JSON response")
	}
}

func TestExtClient_RejectsHTTPRedirects(t *testing.T) {
	var targetHits atomic.Int32
	targetSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		targetHits.Add(1)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"status":"ok","output":"ssrf"}`))
	}))
	defer targetSrv.Close()

	redirectSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, targetSrv.URL+r.URL.Path, http.StatusFound)
	}))
	defer redirectSrv.Close()

	c, err := newExtClient(redirectSrv.URL, "tok-redir")
	if err != nil {
		t.Fatalf("newExtClient: %v", err)
	}

	if err := c.Health(context.Background()); err == nil {
		t.Fatal("Health followed 302 redirect")
	}
	if _, err := c.InvokeTool(context.Background(), "ping", nil, "en"); err == nil {
		t.Fatal("InvokeTool followed 302 redirect")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	var gotStreamErr atomic.Bool
	err = c.StreamView(ctx, "main", "en", func(u uiblock.Update) error {
		if u.Tree != nil || len(u.Ops) > 0 {
			t.Fatalf("unexpected SSE tree/patch from redirect target: %+v", u)
		}
		if u.Error != "" {
			gotStreamErr.Store(true)
			cancel()
		}
		return nil
	})
	if err == nil && !gotStreamErr.Load() {
		t.Fatal("StreamView followed 302 redirect without reporting an error")
	}
	if targetHits.Load() != 0 {
		t.Fatalf("redirect target was contacted %d times; want 0", targetHits.Load())
	}
}

func TestExtClient_StreamViewRejectsNodeIDSpoofingAndMismatch(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/views/main/stream" {
			http.NotFound(w, r)
			return
		}
		fl, _ := w.(http.Flusher)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		n := calls.Add(1)
		switch n {
		case 1:
			// Spoofing rune in Node.ID despite matching op.ID should fail validatePatchedNodeTree.
			_, _ = w.Write([]byte("event: tree\nid: 1\ndata: {\"kind\":\"column\",\"children\":[{\"kind\":\"text\",\"key\":\"lbl\",\"text\":\"v1\"}]}\n\n"))
			fl.Flush()
			_, _ = w.Write([]byte("event: patch\nid: 2\ndata: {\"ops\":[{\"op\":\"insert\",\"parent_id\":\"\",\"index\":1,\"node\":{\"id\":\"/\\u202eevil\",\"kind\":\"text\",\"text\":\"x\"}}]}\n\n"))
			fl.Flush()
			<-r.Context().Done()
		case 2:
			// Forged parent prefix /mismatched op.ID in insert node.
			_, _ = w.Write([]byte("event: tree\nid: 1\ndata: {\"kind\":\"column\",\"children\":[{\"kind\":\"text\",\"key\":\"lbl\",\"text\":\"v2\"}]}\n\n"))
			fl.Flush()
			_, _ = w.Write([]byte("event: patch\nid: 2\ndata: {\"ops\":[{\"op\":\"insert\",\"parent_id\":\"\",\"index\":1,\"node\":{\"id\":\"/forged/nested\",\"kind\":\"text\",\"text\":\"x\"}}]}\n\n"))
			fl.Flush()
			<-r.Context().Done()
		case 3:
			// Duplicate sibling IDs inside patched subtree.
			_, _ = w.Write([]byte("event: tree\nid: 1\ndata: {\"kind\":\"column\",\"children\":[{\"kind\":\"text\",\"key\":\"lbl\",\"text\":\"v3\"}]}\n\n"))
			fl.Flush()
			_, _ = w.Write([]byte("event: patch\nid: 2\ndata: {\"ops\":[{\"op\":\"replace_root\",\"node\":{\"id\":\"\",\"kind\":\"column\",\"children\":[{\"id\":\"/dup\",\"kind\":\"text\",\"key\":\"dup\",\"text\":\"a\"},{\"id\":\"/dup\",\"kind\":\"text\",\"key\":\"dup\",\"text\":\"b\"}]}}]}\n\n"))
			fl.Flush()
			<-r.Context().Done()
		default:
			_, _ = w.Write([]byte("event: tree\nid: 1\ndata: {\"kind\":\"column\",\"children\":[{\"kind\":\"text\",\"key\":\"lbl\",\"text\":\"clean\"}]}\n\n"))
			fl.Flush()
			<-r.Context().Done()
		}
	}))
	defer srv.Close()

	c, err := newExtClient(srv.URL, "tok-id")
	if err != nil {
		t.Fatalf("newExtClient: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var updates []uiblock.Update
	done := make(chan error, 1)
	go func() {
		done <- c.StreamView(ctx, "main", "en", func(u uiblock.Update) error {
			updates = append(updates, u)
			if u.Tree != nil && len(u.Tree.Children) > 0 && u.Tree.Children[0].Text == "clean" {
				cancel()
			}
			return nil
		})
	}()
	<-done

	if calls.Load() < 4 {
		t.Fatalf("expected reconnects after node ID spoofing/prefix/duplicate patches, got %d calls", calls.Load())
	}
	for _, u := range updates {
		if len(u.Ops) > 0 {
			t.Fatalf("spoofed/invalid patch update leaked to caller: %+v", u)
		}
	}
}

func TestRuntimeSandbox_RedactsHostAndExtTokensInLogs(t *testing.T) {
	dataDir := t.TempDir()
	// Keep Unix socket paths short (<104B) on macOS/Linux.
	if len(dataDir) > 40 {
		shortDir, err := os.MkdirTemp("", "gvx-sbx-redact-")
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.RemoveAll(shortDir) })
		dataDir = shortDir
	}

	fakeRungvSrc := filepath.Join(t.TempDir(), "fake_rungv.go")
	const fakeRungvCode = `package main

import (
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
)

func main() {
	args := os.Args[1:]
	for _, a := range args {
		if a == "-help" || a == "--help" {
			fmt.Println("  -unix-forward")
			fmt.Println("  -nat-policy-file")
			return
		}
	}
	if len(args) == 0 || args[0] != "run" {
		return
	}
	var hostTok, extTok, listenSpec string
	for i := 0; i < len(args); i++ {
		if args[i] == "-e" && i+1 < len(args) {
			kv := args[i+1]
			if v, ok := strings.CutPrefix(kv, "GVX_HOST_TOKEN="); ok {
				hostTok = v
			} else if v, ok := strings.CutPrefix(kv, "GVX_EXT_TOKEN="); ok {
				extTok = v
			}
			i++
		} else if args[i] == "-p" && i+1 < len(args) {
			listenSpec = args[i+1]
			i++
		}
	}
	// Deliberately emit a guest stderr line echoing tokens to verify defense-in-depth redaction too.
	fmt.Fprintf(os.Stderr, "guest-debug GVX_HOST_TOKEN=%s raw=%s\n", hostTok, extTok)

	var ln net.Listener
	var err error
	if strings.HasPrefix(listenSpec, "unix:") {
		rest := strings.TrimPrefix(listenSpec, "unix:")
		if idx := strings.LastIndex(rest, ":"); idx > 0 {
			sock := rest[:idx]
			_ = os.Remove(sock)
			ln, err = net.Listen("unix", sock)
		}
	} else {
		parts := strings.Split(listenSpec, ":")
		if len(parts) >= 2 {
			ln, err = net.Listen("tcp", parts[0]+":"+parts[1])
		}
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "listen err:", err)
		os.Exit(2)
	}
	done := make(chan struct{})
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/health", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(` + "`" + `{"status":"ok","version":"0.1.0"}` + "`" + `))
	})
	mux.HandleFunc("/v1/shutdown", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		select {
		case <-done:
		default:
			close(done)
		}
	})
	srv := &http.Server{Handler: mux}
	go func() { _ = srv.Serve(ln) }()
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)
	select {
	case <-done:
	case <-sigCh:
	}
	_ = srv.Close()
}
`
	if err := os.WriteFile(fakeRungvSrc, []byte(fakeRungvCode), 0o600); err != nil {
		t.Fatal(err)
	}
	fakeRungvBin := filepath.Join(t.TempDir(), "rungv")
	if runtime.GOOS == "windows" {
		fakeRungvBin += ".exe"
	}
	if out, err := exec.Command("go", "build", "-o", fakeRungvBin, fakeRungvSrc).CombinedOutput(); err != nil {
		t.Fatalf("build fake rungv: %v\n%s", err, out)
	}
	t.Cleanup(func() { rungv.ForgetFeatures(fakeRungvBin) })

	sup := &sandbox.Supervisor{
		Client:  func() *rungv.Client { return &rungv.Client{Path: fakeRungvBin} },
		DataDir: filepath.Join(dataDir, "sbx-sup"),
	}
	t.Cleanup(func() { sup.StopAll(5 * time.Second) })

	m := &Manifest{
		SchemaVersion: 1,
		ID:            "sbx-leak-test",
		Version:       "0.1.0",
		Publisher:     "acme",
		Name:          "Sbx Leak Test",
		Runtime: RuntimeDecl{
			Type:  RuntimeSandbox,
			Image: "alpine:3.20",
			Port:  DefaultRuntimePort,
		},
	}
	gs, err := LoadGrantStore(GrantsPath(dataDir))
	if err != nil {
		t.Fatal(err)
	}
	if err := gs.Approve(m.ID, m, ""); err != nil {
		t.Fatal(err)
	}

	const (
		hostSecret = "SECRET_HOST_TOKEN_987654321"
		extSecret  = "SECRET_EXT_TOKEN_123456789"
	)
	rt := newSandboxRuntime(sandboxRuntimeConfig{
		extID:        m.ID,
		dataDir:      dataDir,
		manifest:     m,
		source:       SourceSpec{Kind: SourceImage, ImageRef: "alpine:3.20"},
		grants:       gs,
		supervisor:   sup,
		hostEndpoint: "http://127.0.0.1:1",
		hostSockPath: filepath.Join(dataDir, "h.sock"),
		hostToken:    hostSecret,
		extToken:     extSecret,
	})
	if err := rt.Start(context.Background()); err != nil {
		t.Fatalf("rt.Start: %v", err)
	}
	defer func() { _ = rt.Stop(5 * time.Second) }()

	// Wait until watchSandbox drains at least the supervisor command line + guest-debug line.
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		joined := strings.Join(rt.Logs(), "\n")
		if strings.Contains(joined, "GVX_HOST_TOKEN=<redacted>") && strings.Contains(joined, "guest-debug") {
			break
		}
		time.Sleep(25 * time.Millisecond)
	}

	// 1. Supervisor.Logs (System lines) must redact GVX_HOST_TOKEN and GVX_EXT_TOKEN.
	var supSysLines []string
	for _, ln := range sup.Logs(rt.sbxID).Snapshot() {
		if ln.Stream == sandbox.System {
			supSysLines = append(supSysLines, ln.Text)
		}
	}
	supJoined := strings.Join(supSysLines, "\n")
	if strings.Contains(supJoined, hostSecret) || strings.Contains(supJoined, extSecret) {
		t.Fatalf("secret token leaked into Supervisor.Logs System lines:\n%s", supJoined)
	}
	if !strings.Contains(supJoined, "GVX_HOST_TOKEN=<redacted>") || !strings.Contains(supJoined, "GVX_EXT_TOKEN=<redacted>") {
		t.Fatalf("expected redacted tokens in Supervisor.Logs, got:\n%s", supJoined)
	}

	// 2. rt.Logs() must redact both supervisor command line and guest stdout/stderr tokens.
	rtJoined := strings.Join(rt.Logs(), "\n")
	if strings.Contains(rtJoined, hostSecret) || strings.Contains(rtJoined, extSecret) {
		t.Fatalf("secret token leaked into rt.Logs():\n%s", rtJoined)
	}
	if !strings.Contains(rtJoined, "GVX_HOST_TOKEN=<redacted>") || !strings.Contains(rtJoined, "GVX_EXT_TOKEN=<redacted>") {
		t.Fatalf("expected redacted tokens in rt.Logs(), got:\n%s", rtJoined)
	}

	// 3. extension-data/<id>/logs/extension.log on disk must never contain either secret token.
	diskLogBytes, err := os.ReadFile(filepath.Join(LogsDir(dataDir, m.ID), "extension.log"))
	if err != nil {
		t.Fatalf("read extension.log: %v", err)
	}
	diskLog := string(diskLogBytes)
	if strings.Contains(diskLog, hostSecret) || strings.Contains(diskLog, extSecret) {
		t.Fatalf("secret token leaked into extension.log on disk:\n%s", diskLog)
	}
	if !strings.Contains(diskLog, "GVX_HOST_TOKEN=<redacted>") || !strings.Contains(diskLog, "GVX_EXT_TOKEN=<redacted>") {
		t.Fatalf("expected redacted tokens in extension.log, got:\n%s", diskLog)
	}
}
