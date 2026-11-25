package ext

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
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
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/ironpark/gvx-desktop/internal/engine"
	"github.com/ironpark/gvx-desktop/internal/rungv"
	"github.com/ironpark/gvx-desktop/internal/sandbox"
)

func newTestGrantStoreWithDocker(t *testing.T, dataDir, extID string, allowed []string) *GrantStore {
	t.Helper()
	gs, err := LoadGrantStore(GrantsPath(dataDir))
	if err != nil {
		t.Fatalf("LoadGrantStore: %v", err)
	}
	m := &Manifest{
		SchemaVersion: 1,
		ID:            extID,
		Version:       "1.0.0",
		Publisher:     "acme",
		Name:          "Test",
		Permissions:   []string{PermDocker, PermStorage},
	}
	if err := gs.Approve(extID, m, ""); err != nil {
		t.Fatalf("Approve: %v", err)
	}
	for _, dir := range allowed {
		if _, err := gs.AddAllowedFolder(extID, dir); err != nil {
			t.Fatalf("AddAllowedFolder(%q): %v", dir, err)
		}
	}
	return gs
}

func doScopedDockerCreate(t *testing.T, handler http.Handler, path string, body []byte) (int, []byte) {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	return rec.Code, rec.Body.Bytes()
}

func TestDockerScope_LocalHandlerCannotBypassScopingAndInjectsLabel(t *testing.T) {
	dataDir := t.TempDir()
	allowedDir := t.TempDir()
	gs := newTestGrantStoreWithDocker(t, dataDir, "scope-ext", []string{allowedDir})

	var gotPayload map[string]any
	localBackend := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := stripDockerVersion(cleanDockerMatchPath(r.URL.Path))
		if r.Method == http.MethodGet && strings.HasPrefix(p, "/images/") && strings.HasSuffix(p, "/json") {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{}`))
			return
		}
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &gotPayload)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"Id":"ctr-1"}`))
	})

	dp := newScopedDockerProxy("scope-ext", dataDir, gs, []string{PermDocker}, nil, nil, func() http.Handler {
		return localBackend
	})
	defer dp.Close()

	// 1. Plain create on versioned endpoint injects Labels["io.gvx.extension"].
	code, resp := doScopedDockerCreate(t, dp, "/v1.52/containers/create", []byte(`{"Image":"alpine:3.20"}`))
	if code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", code, resp)
	}
	labels, _ := gotPayload["Labels"].(map[string]any)
	if labels["io.gvx.extension"] != "scope-ext" {
		t.Fatalf("expected injected io.gvx.extension=scope-ext, got %+v", labels)
	}

	// 2. Spoofed io.gvx.extension label is rejected with 403.
	code, resp = doScopedDockerCreate(t, dp, "/containers/create", []byte(`{
		"Image":"alpine:3.20",
		"Labels":{"io.gvx.extension":"other-ext"}
	}`))
	if code != http.StatusForbidden {
		t.Fatalf("expected 403 on spoofed label, got %d: %s", code, resp)
	}
}

func TestDockerScope_OversizedPayloadAndMalformedJSONRejected(t *testing.T) {
	dataDir := t.TempDir()
	gs := newTestGrantStoreWithDocker(t, dataDir, "size-ext", nil)

	called := false
	localBackend := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(http.StatusCreated)
	})
	dp := newScopedDockerProxy("size-ext", dataDir, gs, []string{PermDocker}, nil, nil, func() http.Handler {
		return localBackend
	})
	defer dp.Close()

	oversized := bytes.Repeat([]byte("a"), maxDockerCreateBodyBytes+64)
	code, _ := doScopedDockerCreate(t, dp, "/v1.47/containers/create", oversized)
	if code != http.StatusBadRequest {
		t.Fatalf("expected 400 for >4 MiB payload, got %d", code)
	}
	if called {
		t.Fatal("oversized payload must never reach backend")
	}

	code, _ = doScopedDockerCreate(t, dp, "/containers/create", []byte(`{"Image":`))
	if code != http.StatusBadRequest {
		t.Fatalf("expected 400 for malformed JSON, got %d", code)
	}
	if called {
		t.Fatal("malformed JSON must never reach backend")
	}
}

func TestDockerScope_BindMountValidationSymlinksDataAndWindows(t *testing.T) {
	dataDir := t.TempDir()
	allowedDir := t.TempDir()
	outsideDir := t.TempDir()
	gs := newTestGrantStoreWithDocker(t, dataDir, "bind-ext", []string{allowedDir})

	// Create symlink escaping outside allowedDir, a dangling symlink, and a relative dangling symlink inside a symlinked subdir.
	if err := os.Symlink(outsideDir, filepath.Join(allowedDir, "escape")); err != nil {
		t.Fatalf("symlink: %v", err)
	}
	if err := os.Symlink(filepath.Join(outsideDir, "future"), filepath.Join(allowedDir, "dangling")); err != nil {
		t.Fatalf("dangling symlink: %v", err)
	}
	outsideSub := filepath.Join(outsideDir, "sub")
	if err := os.MkdirAll(outsideSub, 0o700); err != nil {
		t.Fatalf("mkdir outside/sub: %v", err)
	}
	if err := os.Symlink(outsideSub, filepath.Join(allowedDir, "dir1")); err != nil {
		t.Fatalf("symlink dir1: %v", err)
	}
	if err := os.Symlink("../secret", filepath.Join(outsideSub, "link")); err != nil {
		t.Fatalf("relative dangling symlink: %v", err)
	}
	realOutside, err := filepath.EvalSymlinks(outsideDir)
	if err != nil {
		t.Fatal(err)
	}
	resolvedLink, err := resolvePathWithAncestor(filepath.Join(allowedDir, "dir1", "link"))
	if err != nil {
		t.Fatalf("resolvePathWithAncestor: %v", err)
	}
	if want := filepath.Join(realOutside, "secret"); resolvedLink != want {
		t.Fatalf("resolvePathWithAncestor(allowed/dir1/link) = %q, want %q", resolvedLink, want)
	}

	var lastReq map[string]any
	volumes := map[string]map[string]string{}
	localBackend := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := stripDockerVersion(cleanDockerMatchPath(r.URL.Path))
		switch {
		case r.Method == http.MethodPost && p == "/volumes/create":
			b, _ := io.ReadAll(r.Body)
			var req struct {
				Name   string            `json:"Name"`
				Labels map[string]string `json:"Labels"`
			}
			_ = json.Unmarshal(b, &req)
			volumes[req.Name] = req.Labels
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(map[string]any{"Name": req.Name, "Labels": req.Labels})
		case r.Method == http.MethodGet && strings.HasPrefix(p, "/volumes/"):
			name := strings.TrimPrefix(p, "/volumes/")
			lbls, ok := volumes[name]
			if !ok {
				http.Error(w, `{"message":"no such volume"}`, http.StatusNotFound)
				return
			}
			w.WriteHeader(http.StatusOK)
			_ = json.NewEncoder(w).Encode(map[string]any{"Name": name, "Labels": lbls})
		case r.Method == http.MethodGet && strings.HasPrefix(p, "/images/") && strings.HasSuffix(p, "/json"):
			http.Error(w, `{"message":"No such image"}`, http.StatusNotFound)
		default:
			b, _ := io.ReadAll(r.Body)
			_ = json.Unmarshal(b, &lastReq)
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"Id":"ok"}`))
		}
	})
	dp := newScopedDockerProxy("bind-ext", dataDir, gs, []string{PermDocker}, nil, nil, func() http.Handler {
		return localBackend
	})
	defer dp.Close()

	// Create owned named volume first, then verify allowed non-existing leaf bind + owned named volume succeed.
	if code, resp := doScopedDockerReq(t, dp, http.MethodPost, "/volumes/create", []byte(`{"Name":"named-cache"}`)); code != http.StatusCreated {
		t.Fatalf("POST /volumes/create(named-cache) = %d: %s", code, resp)
	}
	nonExistAllowed := filepath.Join(allowedDir, "sub", "newdir")
	code, resp := doScopedDockerCreate(t, dp, "/containers/create", []byte(`{
		"Image":"alpine:3.20",
		"HostConfig":{"Binds":["`+filepath.ToSlash(nonExistAllowed)+`:/work:ro","named-cache:/cache"]}
	}`))
	if code != http.StatusCreated {
		t.Fatalf("expected allowed non-existing leaf bind to succeed, got %d: %s", code, resp)
	}

	// Sandbox /data/sub -> rewritten to FilesDir/sub.
	code, resp = doScopedDockerCreate(t, dp, "/containers/create", []byte(`{
		"Image":"alpine:3.20",
		"HostConfig":{"Mounts":[{"Type":"bind","Source":"/data/sub","Target":"/dst"}]}
	}`))
	if code != http.StatusCreated {
		t.Fatalf("expected /data/sub mount to succeed, got %d: %s", code, resp)
	}
	hc := lastReq["HostConfig"].(map[string]any)
	mounts := hc["Mounts"].([]any)
	mappedSrc := mounts[0].(map[string]any)["Source"].(string)
	if !strings.Contains(mappedSrc, "sub") || strings.HasPrefix(mappedSrc, "/data") {
		t.Fatalf("expected /data/sub rewritten to host FilesDir, got %q", mappedSrc)
	}

	// Rejected bind cases.
	badBodies := []struct {
		name string
		body string
	}{
		{"outside_etc", `{"Image":"alpine","HostConfig":{"Binds":["/etc:/host-etc"]}}`},
		{"relative_dot", `{"Image":"alpine","HostConfig":{"Binds":["./local:/work"]}}`},
		{"dotdot_data", `{"Image":"alpine","HostConfig":{"Binds":["/data/../secrets.json:/x"]}}`},
		{"symlink_escape", `{"Image":"alpine","HostConfig":{"Binds":["` + filepath.ToSlash(filepath.Join(allowedDir, "escape", "f")) + `:/x"]}}`},
		{"dangling_escape", `{"Image":"alpine","HostConfig":{"Binds":["` + filepath.ToSlash(filepath.Join(allowedDir, "dangling", "f")) + `:/x"]}}`},
		{"rel_dangling_subdir_escape", `{"Image":"alpine","HostConfig":{"Binds":["` + filepath.ToSlash(filepath.Join(allowedDir, "dir1", "link")) + `:/x"]}}`},
		{"mount_outside", `{"Image":"alpine","HostConfig":{"Mounts":[{"Type":"BIND","Source":"` + filepath.ToSlash(outsideDir) + `","Target":"/x"}]}}`},
	}
	for _, tc := range badBodies {
		code, resp := doScopedDockerCreate(t, dp, "/containers/create", []byte(tc.body))
		if code != http.StatusForbidden {
			t.Errorf("%s: expected 403, got %d (%s)", tc.name, code, resp)
		}
	}

	// Windows drive-letter bind normalization check (works cross-platform via scopeContainerCreatePayload).
	winDataDir := t.TempDir()
	winGS, _ := LoadGrantStore(GrantsPath(winDataDir))
	_ = winGS.Approve("win-ext", &Manifest{SchemaVersion: 1, ID: "win-ext", Version: "1.0.0", Publisher: "acme", Name: "Win", Permissions: []string{PermDocker}}, "")
	winGS.mu.Lock()
	entry := winGS.entries["win-ext"]
	entry.AllowedFolders = []string{`C:\Users\Alice\proj`}
	winGS.entries["win-ext"] = entry
	winGS.mu.Unlock()

	winReq := []byte(`{"Image":"alpine","HostConfig":{"Binds":["C:\\Users\\Alice\\proj\\sub:/work:ro"]}}`)
	rewritten, status, err := scopeContainerCreatePayload(winReq, "win-ext", winDataDir, winGS)
	if err != nil || status != 0 {
		t.Fatalf("expected valid Windows bind under allowed folder, got status=%d err=%v", status, err)
	}
	if !bytes.Contains(rewritten, []byte(`/c/Users/Alice/proj/sub:/work:ro`)) {
		t.Fatalf("expected normalized Windows guest path /c/Users/Alice/proj/sub, got %s", rewritten)
	}

	winBad := []byte(`{"Image":"alpine","HostConfig":{"Binds":["C:\\Users\\Bob\\secret:/work"]}}`)
	if _, status, err := scopeContainerCreatePayload(winBad, "win-ext", winDataDir, winGS); err == nil || status != http.StatusForbidden {
		t.Fatalf("expected 403 for unapproved Windows bind, got status=%d err=%v", status, err)
	}
}

func TestDockerScope_503WhenStoppedAndDynamicRevokeAndSocketPerms(t *testing.T) {
	dataDir := t.TempDir()
	gs := newTestGrantStoreWithDocker(t, dataDir, "status-ext", nil)

	// 1. Both handler=nil and dial=nil -> 503 Docker JSON error.
	dpDown := newScopedDockerProxy("status-ext", dataDir, gs, []string{PermDocker}, nil, nil, nil)
	ep, err := dpDown.Listen()
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	defer dpDown.Close()

	if runtime.GOOS != "windows" {
		sock := dpDown.SocketPath()
		info, err := os.Stat(sock)
		if err != nil {
			t.Fatalf("stat socket: %v", err)
		}
		if info.Mode().Perm() != 0o600 {
			t.Fatalf("expected socket mode 0600, got %04o", info.Mode().Perm())
		}
	}

	client := &http.Client{
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				if strings.HasPrefix(ep, "unix://") {
					return (&net.Dialer{}).DialContext(ctx, "unix", strings.TrimPrefix(ep, "unix://"))
				}
				return (&net.Dialer{}).DialContext(ctx, "tcp", strings.TrimPrefix(ep, "http://"))
			},
		},
	}
	resp, err := client.Get("http://docker/_ping")
	if err != nil {
		t.Fatalf("GET /_ping: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("expected 503 when docker engine stopped, got %d", resp.StatusCode)
	}

	// 2. RevokePermissions immediately returns 403 on subsequent requests.
	if err := gs.RevokePermissions("status-ext"); err != nil {
		t.Fatalf("RevokePermissions: %v", err)
	}
	resp2, err := client.Get("http://docker/_ping")
	if err != nil {
		t.Fatalf("GET /_ping after revoke: %v", err)
	}
	_ = resp2.Body.Close()
	if resp2.StatusCode != http.StatusForbidden {
		t.Fatalf("expected 403 after permission revocation, got %d", resp2.StatusCode)
	}
}

func TestDockerScope_SymlinkTargetDotDotAcrossIntermediateSymlinkRejected(t *testing.T) {
	dataDir := t.TempDir()
	allowedDir := t.TempDir()
	outsideDir := t.TempDir()
	gs := newTestGrantStoreWithDocker(t, dataDir, "sym-dotdot-ext", []string{allowedDir})

	outsideDeepNested := filepath.Join(outsideDir, "deep", "nested")
	if err := os.MkdirAll(outsideDeepNested, 0o700); err != nil {
		t.Fatalf("mkdir outside/deep/nested: %v", err)
	}
	if err := os.Symlink(outsideDeepNested, filepath.Join(allowedDir, "sub_sym")); err != nil {
		t.Fatalf("symlink sub_sym: %v", err)
	}
	// Symlink target uses literal "sub_sym/../secret" so ".." must traverse
	// the resolved target of sub_sym (outside/deep/nested/.. -> outside/deep/secret),
	// NOT lexically collapse to allowed/secret!
	if err := os.Symlink("sub_sym/../secret", filepath.Join(allowedDir, "escape")); err != nil {
		t.Fatalf("symlink escape: %v", err)
	}
	// Also verify dangling nonexist/../sub_sym re-enters Lstat when popping back to existing dir.
	if err := os.Symlink("nonexist/../sub_sym", filepath.Join(allowedDir, "reenter")); err != nil {
		t.Fatalf("symlink reenter: %v", err)
	}

	realOutside, err := filepath.EvalSymlinks(outsideDir)
	if err != nil {
		t.Fatal(err)
	}
	resolved, err := resolvePathWithAncestor(filepath.Join(allowedDir, "escape", "new_child"))
	if err != nil {
		t.Fatalf("resolvePathWithAncestor(escape/new_child): %v", err)
	}
	want := filepath.Join(realOutside, "deep", "secret", "new_child")
	if resolved != want {
		t.Fatalf("resolvePathWithAncestor(allowed/escape/new_child) = %q, want %q", resolved, want)
	}

	resolvedReenter, err := resolvePathWithAncestor(filepath.Join(allowedDir, "reenter", "new_child"))
	if err != nil {
		t.Fatalf("resolvePathWithAncestor(reenter/new_child): %v", err)
	}
	wantReenter := filepath.Join(realOutside, "deep", "nested", "new_child")
	if resolvedReenter != wantReenter {
		t.Fatalf("resolvePathWithAncestor(allowed/reenter/new_child) = %q, want %q", resolvedReenter, wantReenter)
	}

	dp := newScopedDockerProxy("sym-dotdot-ext", dataDir, gs, []string{PermDocker}, nil, nil, func() http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"Id":"unexpected"}`))
		})
	})
	defer dp.Close()

	for _, tc := range []struct {
		name string
		src  string
	}{
		{"escape_new_child", filepath.Join(allowedDir, "escape", "new_child")},
		{"reenter_new_child", filepath.Join(allowedDir, "reenter", "new_child")},
	} {
		body := `{"Image":"alpine:3.20","HostConfig":{"Binds":["` + filepath.ToSlash(tc.src) + `:/work"]}}`
		code, resp := doScopedDockerCreate(t, dp, "/containers/create", []byte(body))
		if code != http.StatusForbidden {
			t.Fatalf("%s: expected 403 Forbidden, got %d (%s)", tc.name, code, resp)
		}
	}
}

func doScopedDockerReq(t *testing.T, dp *scopedDockerProxy, method, path string, body []byte) (int, string) {
	t.Helper()
	var bodyReader io.Reader
	if body != nil {
		bodyReader = bytes.NewReader(body)
	}
	req := httptest.NewRequest(method, "http://docker"+path, bodyReader)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	rec := httptest.NewRecorder()
	dp.ServeHTTP(rec, req)
	return rec.Code, rec.Body.String()
}

func TestDockerScope_CaseInsensitiveJSONAndDuplicateKeysRejected(t *testing.T) {
	dataDir := t.TempDir()
	allowedDir := t.TempDir()
	gs := newTestGrantStoreWithDocker(t, dataDir, "ext-case", []string{allowedDir})

	var lastCreateBody []byte
	dp := newScopedDockerProxy("ext-case", dataDir, gs, []string{PermDocker}, nil, nil, func() http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			p := stripDockerVersion(cleanDockerMatchPath(r.URL.Path))
			if r.Method == http.MethodGet && strings.HasPrefix(p, "/images/") && strings.HasSuffix(p, "/json") {
				http.Error(w, `{"message":"No such image"}`, http.StatusNotFound)
				return
			}
			lastCreateBody, _ = io.ReadAll(r.Body)
			w.WriteHeader(http.StatusCreated)
			_, _ = io.WriteString(w, `{"Id":"ctr-1"}`)
		})
	})

	// 1. Lowercase hostconfig/binds to /etc must be caught and rejected.
	code, resp := doScopedDockerCreate(t, dp, "/containers/create", []byte(`{"image":"alpine:3.20","labels":{"io.gvx.extension":"other"},"hostconfig":{"binds":["/etc:/work"]}}`))
	if code != http.StatusForbidden {
		t.Fatalf("lowercase binds bypass succeeded: %d %s", code, resp)
	}

	// 2. Lowercase mounts/type/source to /etc must be rejected.
	code, resp = doScopedDockerCreate(t, dp, "/containers/create", []byte(`{"image":"alpine:3.20","hostconfig":{"mounts":[{"type":"BIND","source":"/etc","target":"/work"}]}}`))
	if code != http.StatusForbidden {
		t.Fatalf("lowercase mounts bypass succeeded: %d %s", code, resp)
	}

	// 3. Duplicate case-colliding keys at top level, HostConfig, and Mounts must be rejected.
	for _, tc := range []struct {
		name string
		body string
	}{
		{"dup-labels", `{"Image":"alpine:3.20","Labels":{"a":"1"},"labels":{"io.gvx.extension":"evil"}}`},
		{"dup-hostconfig", `{"Image":"alpine:3.20","HostConfig":{},"hostconfig":{"Binds":["/etc:/work"]}}`},
		{"dup-binds", `{"Image":"alpine:3.20","HostConfig":{"Binds":[],"binds":["/etc:/work"]}}`},
		{"dup-mounts", `{"Image":"alpine:3.20","HostConfig":{"Mounts":[],"mounts":[{"Type":"bind","Source":"/etc","Target":"/work"}]}}`},
		{"dup-mount-type", `{"Image":"alpine:3.20","HostConfig":{"Mounts":[{"Type":"volume","type":"bind","Source":"/etc","Target":"/work"}]}}`},
		{"dup-mount-source", `{"Image":"alpine:3.20","HostConfig":{"Mounts":[{"Type":"bind","Source":"` + filepath.ToSlash(allowedDir) + `","source":"/etc","Target":"/work"}]}}`},
	} {
		code, resp := doScopedDockerCreate(t, dp, "/containers/create", []byte(tc.body))
		if code != http.StatusBadRequest && code != http.StatusForbidden {
			t.Fatalf("%s: expected 400/403, got %d (%s)", tc.name, code, resp)
		}
	}

	// 4. Spoofed io.gvx.extension inside lowercase "labels" must be rejected with 403.
	spoofBody := `{"image":"alpine:3.20","labels":{"io.gvx.extension":"spoofed","custom":"ok"},"hostconfig":{"binds":["` + filepath.ToSlash(allowedDir) + `:/work"]}}`
	if code, resp := doScopedDockerCreate(t, dp, "/containers/create", []byte(spoofBody)); code != http.StatusForbidden {
		t.Fatalf("expected 403 Forbidden for lowercase labels spoof, got %d (%s)", code, resp)
	}

	// 5. Legitimate lowercase payload succeeds and is canonically normalized with immutable io.gvx.extension.
	okBody := `{"image":"alpine:3.20","labels":{"custom":"ok"},"hostconfig":{"binds":["` + filepath.ToSlash(allowedDir) + `:/work"]}}`
	code, resp = doScopedDockerCreate(t, dp, "/containers/create", []byte(okBody))
	if code != http.StatusCreated {
		t.Fatalf("expected 201 Created for allowed lowercase bind, got %d (%s)", code, resp)
	}
	var forwarded map[string]any
	if err := json.Unmarshal(lastCreateBody, &forwarded); err != nil {
		t.Fatalf("unmarshal forwarded payload: %v", err)
	}
	if _, hasLowerLabels := forwarded["labels"]; hasLowerLabels {
		t.Fatalf("forwarded payload still contains lowercase 'labels': %s", string(lastCreateBody))
	}
	labels, _ := forwarded["Labels"].(map[string]any)
	if labels["io.gvx.extension"] != "ext-case" || labels["custom"] != "ok" {
		t.Fatalf("unexpected canonical Labels: %#v", labels)
	}
}

func TestDockerScope_ContainerExecOwnershipAndPruneBlocked(t *testing.T) {
	dataDir := t.TempDir()
	gs := newTestGrantStoreWithDocker(t, dataDir, "ext-a", nil)

	containers := map[string]map[string]string{
		"c-owned":   {"io.gvx.extension": "ext-a"},
		"c-foreign": {"io.gvx.extension": "ext-b"},
		"c-host":    {},
	}
	execOwners := map[string]string{
		"e-owned":   "c-owned",
		"e-foreign": "c-foreign",
		"e-host":    "c-host",
	}

	dp := newScopedDockerProxy("ext-a", dataDir, gs, []string{PermDocker}, nil, nil, func() http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			p := strings.TrimPrefix(r.URL.Path, "/v1.47")
			switch {
			case r.Method == http.MethodGet && p == "/containers/json":
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, `[
					{"Id":"c-owned","Labels":{"io.gvx.extension":"ext-a"}},
					{"Id":"c-foreign","Labels":{"io.gvx.extension":"ext-b"}},
					{"Id":"c-host","Labels":{}}
				]`)
			case r.Method == http.MethodGet && strings.HasPrefix(p, "/containers/") && strings.HasSuffix(p, "/json"):
				id := strings.TrimSuffix(strings.TrimPrefix(p, "/containers/"), "/json")
				labels, ok := containers[id]
				if !ok {
					http.Error(w, `{"message":"no such container"}`, http.StatusNotFound)
					return
				}
				w.Header().Set("Content-Type", "application/json")
				_ = json.NewEncoder(w).Encode(map[string]any{
					"Id":     id,
					"Config": map[string]any{"Labels": labels},
				})
			case strings.HasPrefix(p, "/containers/"):
				w.WriteHeader(http.StatusOK)
				_, _ = io.WriteString(w, `{"Id":"e-owned"}`)
			case r.Method == http.MethodGet && strings.HasPrefix(p, "/exec/") && strings.HasSuffix(p, "/json"):
				id := strings.TrimSuffix(strings.TrimPrefix(p, "/exec/"), "/json")
				cid, ok := execOwners[id]
				if !ok {
					http.Error(w, `{"message":"no such exec"}`, http.StatusNotFound)
					return
				}
				_ = json.NewEncoder(w).Encode(map[string]any{"ID": id, "ContainerID": cid})
			case strings.HasPrefix(p, "/exec/"):
				w.WriteHeader(http.StatusOK)
			case r.Method == http.MethodPost && p == "/commit":
				w.WriteHeader(http.StatusCreated)
				_, _ = io.WriteString(w, `{"Id":"sha256:img"}`)
			default:
				w.WriteHeader(http.StatusOK)
			}
		})
	})

	// 1. GET /containers/json must filter down to owned containers only.
	for _, path := range []string{"/containers/json", "/v1.47/containers/json?all=1"} {
		code, resp := doScopedDockerReq(t, dp, http.MethodGet, path, nil)
		if code != http.StatusOK {
			t.Fatalf("%s: expected 200, got %d (%s)", path, code, resp)
		}
		var list []map[string]any
		if err := json.Unmarshal([]byte(resp), &list); err != nil {
			t.Fatalf("%s: decode list: %v", path, err)
		}
		if len(list) != 1 || list[0]["Id"] != "c-owned" {
			t.Fatalf("%s: expected only c-owned, got %+v", path, list)
		}
	}

	// 2. Foreign & host containers must be rejected across endpoints, version prefixes, double slashes, and encoded slashes.
	for _, tc := range []struct {
		method string
		path   string
	}{
		{http.MethodGet, "/containers/c-foreign/json"},
		{http.MethodPost, "/containers/c-foreign/start"},
		{http.MethodDelete, "/containers/c-foreign"},
		{http.MethodGet, "/containers/c-host/archive?path=/etc/passwd"},
		{http.MethodPost, "//v1.47//containers/c-foreign/exec"},
		{http.MethodPost, "/exec/e-foreign/start"},
		{http.MethodGet, "/exec/e-foreign/json"},
		{http.MethodPost, "/exec/e-host/resize"},
		{http.MethodPost, "/commit?container=c-foreign"},
		{http.MethodPost, "/containers/prune"},
		{http.MethodPost, "/images/prune"},
		{http.MethodPost, "/volumes/prune"},
		{http.MethodPost, "/networks/prune"},
		{http.MethodPost, "/v1.47/containers/prune"},
	} {
		code, resp := doScopedDockerReq(t, dp, tc.method, tc.path, nil)
		if code != http.StatusForbidden {
			t.Fatalf("%s %s: expected 403 Forbidden, got %d (%s)", tc.method, tc.path, code, resp)
		}
	}
	if code, resp := doScopedDockerReq(t, dp, http.MethodGet, "/containers/c-owned%2f..%2fc-foreign/json", nil); code != http.StatusBadRequest && code != http.StatusForbidden {
		t.Fatalf("encoded slash bypass: expected 400/403, got %d (%s)", code, resp)
	}

	// 3. Owned container & exec endpoints continue working cleanly.
	for _, tc := range []struct {
		method string
		path   string
		want   int
	}{
		{http.MethodGet, "/containers/c-owned/json", http.StatusOK},
		{http.MethodPost, "/containers/c-owned/start", http.StatusOK},
		{http.MethodPost, "/containers/c-owned/exec", http.StatusOK},
		{http.MethodDelete, "/containers/c-owned", http.StatusOK},
		{http.MethodPost, "/exec/e-owned/start", http.StatusOK},
		{http.MethodPost, "/commit?container=c-owned", http.StatusCreated},
	} {
		code, resp := doScopedDockerReq(t, dp, tc.method, tc.path, nil)
		if code != tc.want {
			t.Fatalf("%s %s: expected %d, got %d (%s)", tc.method, tc.path, tc.want, code, resp)
		}
	}
}

func TestDockerScope_DangerousHostConfigRejected(t *testing.T) {
	dataDir := t.TempDir()
	gs := newTestGrantStoreWithDocker(t, dataDir, "ext-hc", nil)
	dp := newScopedDockerProxy("ext-hc", dataDir, gs, []string{PermDocker}, nil, nil, func() http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			p := stripDockerVersion(cleanDockerMatchPath(r.URL.Path))
			if r.Method == http.MethodGet && strings.HasPrefix(p, "/images/") && strings.HasSuffix(p, "/json") {
				http.Error(w, `{"message":"No such image"}`, http.StatusNotFound)
				return
			}
			w.WriteHeader(http.StatusCreated)
			_, _ = io.WriteString(w, `{"Id":"ok"}`)
		})
	})

	for _, tc := range []struct {
		name string
		body string
	}{
		{"privileged", `{"Image":"alpine:3.20","HostConfig":{"Privileged":true}}`},
		{"privileged-lower", `{"Image":"alpine:3.20","hostconfig":{"privileged":true}}`},
		{"volumes-from", `{"Image":"alpine:3.20","HostConfig":{"VolumesFrom":["other"]}}`},
		{"pid-host", `{"Image":"alpine:3.20","HostConfig":{"PidMode":"host"}}`},
		{"pid-container", `{"Image":"alpine:3.20","HostConfig":{"PidMode":"container:other"}}`},
		{"ipc-host", `{"Image":"alpine:3.20","HostConfig":{"IpcMode":"host"}}`},
		{"uts-host", `{"Image":"alpine:3.20","HostConfig":{"UTSMode":"host"}}`},
		{"userns-host", `{"Image":"alpine:3.20","HostConfig":{"UsernsMode":"host"}}`},
		{"cgroupns-host", `{"Image":"alpine:3.20","HostConfig":{"CgroupnsMode":"host"}}`},
		{"net-host", `{"Image":"alpine:3.20","HostConfig":{"NetworkMode":"host"}}`},
		{"net-container", `{"Image":"alpine:3.20","HostConfig":{"NetworkMode":"container:other"}}`},
		{"cap-add", `{"Image":"alpine:3.20","HostConfig":{"CapAdd":["SYS_ADMIN"]}}`},
		{"security-opt", `{"Image":"alpine:3.20","HostConfig":{"SecurityOpt":["apparmor=unconfined"]}}`},
		{"devices", `{"Image":"alpine:3.20","HostConfig":{"Devices":[{"PathOnHost":"/dev/kmsg","PathInContainer":"/dev/kmsg"}]}}`},
		{"volume-driver-bind-o", `{"Image":"alpine:3.20","HostConfig":{"Mounts":[{"Type":"volume","Target":"/mnt","VolumeOptions":{"DriverConfig":{"Name":"local","Options":{"type":"none","o":"bind","device":"/etc"}}}}]}}`},
		{"volume-driver-bind-case", `{"Image":"alpine:3.20","HostConfig":{"Mounts":[{"Type":"volume","Target":"/mnt","VolumeOptions":{"DriverConfig":{"Name":"local","Options":{"O":"rw,BIND","Device":"/var/run/docker.sock"}}}}]}}`},
	} {
		code, resp := doScopedDockerCreate(t, dp, "/containers/create", []byte(tc.body))
		if code != http.StatusBadRequest && code != http.StatusForbidden {
			t.Fatalf("%s: expected 400/403, got %d (%s)", tc.name, code, resp)
		}
	}

	// Safe NetworkMode=bridge remains allowed.
	code, resp := doScopedDockerCreate(t, dp, "/containers/create", []byte(`{"Image":"alpine:3.20","HostConfig":{"NetworkMode":"bridge"}}`))
	if code != http.StatusCreated {
		t.Fatalf("expected safe NetworkMode=bridge to succeed, got %d (%s)", code, resp)
	}

	// Case-insensitive Windows prefix validation.
	if _, err := validateWindowsBind(`c:\allowed\sub`, dataDir, []string{`C:\Allowed`}); err != nil {
		t.Fatalf("expected case-insensitive Windows bind allow, got %v", err)
	}
	if _, err := validateWindowsBind(`c:\allowed_other\sub`, dataDir, []string{`C:\Allowed`}); err == nil {
		t.Fatal("expected sibling Windows prefix escape to be rejected")
	}
}

func TestDockerScope_UnicodeSimpleFoldBypassRejected(t *testing.T) {
	dataDir := t.TempDir()
	allowedDir := t.TempDir()
	gs := newTestGrantStoreWithDocker(t, dataDir, "ext-fold", []string{allowedDir})

	var lastBody []byte
	backendCalls := 0
	dp := newScopedDockerProxy("ext-fold", dataDir, gs, []string{PermDocker}, nil, nil, func() http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			p := stripDockerVersion(cleanDockerMatchPath(r.URL.Path))
			if r.Method == http.MethodGet && strings.HasPrefix(p, "/images/") && strings.HasSuffix(p, "/json") {
				http.Error(w, `{"message":"No such image"}`, http.StatusNotFound)
				return
			}
			backendCalls++
			lastBody, _ = io.ReadAll(r.Body)
			w.WriteHeader(http.StatusCreated)
			_, _ = io.WriteString(w, `{"Id":"ctr-fold"}`)
		})
	})
	defer dp.Close()

	// 1. Colliding SimpleFold keys (ſ U+017F, K U+212A) alongside canonical keys must fail closed with 400.
	collidingCases := []struct {
		name string
		body string
	}{
		{"hostconfig-long-s", `{"Image":"alpine:3.20","Labels":{},"label\u017f":{"io.gvx.extension":"victim"},"HostConfig":{},"ho\u017ftconfig":{"Binds":["/etc:/host-etc:ro"],"Privileged":true}}`},
		{"hostconfig-only-long-s-dup", `{"Image":"alpine:3.20","HostConfig":{},"ho\u017ftconfig":{"Privileged":true}}`},
		{"labels-only-long-s-dup", `{"Image":"alpine:3.20","Labels":{},"label\u017f":{"io.gvx.extension":"victim"}}`},
		{"label-owner-long-s-dup", `{"Image":"alpine:3.20","Labels":{"io.gvx.extension":"ext-fold","io.gvx.exten\u017fion":"victim"}}`},
		{"networkmode-kelvin-dup", `{"Image":"alpine:3.20","HostConfig":{"NetworkMode":"bridge","Networ\u212aMode":"host"}}`},
		{"binds-nested-long-s-dup", `{"Image":"alpine:3.20","HostConfig":{"Mounts":[],"Mount\u017f":[{"Type":"bind","Source":"/etc","Target":"/work"}]}}`},
	}
	for _, tc := range collidingCases {
		code, resp := doScopedDockerCreate(t, dp, "/containers/create", []byte(tc.body))
		if code != http.StatusBadRequest {
			t.Fatalf("%s: expected 400 Bad Request, got %d (%s)", tc.name, code, resp)
		}
	}
	if backendCalls != 0 {
		t.Fatalf("expected 0 backend calls for colliding SimpleFold payloads, got %d", backendCalls)
	}

	// 2. Standalone SimpleFold keys carrying dangerous options or spoofed labels must fail closed with 403.
	standaloneForbidden := []struct {
		name string
		body string
	}{
		{"standalone-long-s-binds", `{"Image":"alpine:3.20","ho\u017ftconfig":{"Binds":["/etc:/host-etc:ro"]}}`},
		{"standalone-long-s-priv", `{"Image":"alpine:3.20","ho\u017ftconfig":{"Privileged":true}}`},
		{"standalone-long-s-labels", `{"Image":"alpine:3.20","label\u017f":{"io.gvx.extension":"victim"}}`},
		{"standalone-long-s-owner", `{"Image":"alpine:3.20","Labels":{"io.gvx.exten\u017fion":"victim"}}`},
		{"standalone-kelvin-netmode", `{"Image":"alpine:3.20","HostConfig":{"Networ\u212aMode":"host"}}`},
		{"standalone-long-s-netmode-val", `{"Image":"alpine:3.20","HostConfig":{"NetworkMode":"ho\u017ft"}}`},
	}
	for _, tc := range standaloneForbidden {
		code, resp := doScopedDockerCreate(t, dp, "/containers/create", []byte(tc.body))
		if code != http.StatusForbidden {
			t.Fatalf("%s: expected 403 Forbidden, got %d (%s)", tc.name, code, resp)
		}
	}
	if backendCalls != 0 {
		t.Fatalf("expected 0 backend calls for forbidden SimpleFold payloads, got %d", backendCalls)
	}

	// 3. Safe standalone SimpleFold keys are canonicalized so no non-canonical key survives.
	safeBody := `{"Image":"alpine:3.20","label\u017f":{"custom":"ok"},"ho\u017ftconfig":{"Networ\u212aMode":"bridge","bind\u017f":["` + filepath.ToSlash(allowedDir) + `:/work:ro"]}}`
	code, resp := doScopedDockerCreate(t, dp, "/containers/create", []byte(safeBody))
	if code != http.StatusCreated {
		t.Fatalf("expected 201 Created for safe SimpleFold payload, got %d (%s)", code, resp)
	}
	var forwarded map[string]any
	if err := json.Unmarshal(lastBody, &forwarded); err != nil {
		t.Fatalf("unmarshal forwarded: %v", err)
	}
	for _, badKey := range []string{"label\u017f", "ho\u017ftconfig"} {
		if _, ok := forwarded[badKey]; ok {
			t.Fatalf("non-canonical key %q survived in forwarded payload: %s", badKey, string(lastBody))
		}
	}
	hc, _ := forwarded["HostConfig"].(map[string]any)
	for _, badKey := range []string{"Networ\u212aMode", "bind\u017f"} {
		if _, ok := hc[badKey]; ok {
			t.Fatalf("non-canonical HostConfig key %q survived: %s", badKey, string(lastBody))
		}
	}
	if hc["NetworkMode"] != "bridge" {
		t.Fatalf("expected canonical NetworkMode=bridge, got %#v", hc["NetworkMode"])
	}
	labels, _ := forwarded["Labels"].(map[string]any)
	if labels["io.gvx.extension"] != "ext-fold" || labels["custom"] != "ok" {
		t.Fatalf("unexpected canonical Labels: %#v", labels)
	}

	// 4. popCaseInsensitive and lookupCaseInsensitive fail closed if >1 EqualFold keys exist directly.
	dupMap := map[string]any{"HostConfig": 1, "ho\u017ftconfig": 2}
	if _, _, err := popCaseInsensitive(dupMap, "HostConfig"); err == nil {
		t.Fatal("expected popCaseInsensitive to fail on >1 EqualFold keys")
	}
	if len(dupMap) != 0 {
		t.Fatalf("expected popCaseInsensitive to delete all matched keys, got %v", dupMap)
	}
	if _, _, err := lookupCaseInsensitive(map[string]any{"Labels": 1, "label\u017f": 2}, "Labels"); err == nil {
		t.Fatal("expected lookupCaseInsensitive to fail on >1 EqualFold keys")
	}
}

func TestDockerScope_NetworkConnectDisconnectAndVolumeCreateScoped(t *testing.T) {
	dataDir := t.TempDir()
	gs := newTestGrantStoreWithDocker(t, dataDir, "ext-net", nil)

	containers := map[string]map[string]string{
		"c-owned":   {"io.gvx.extension": "ext-net"},
		"c-foreign": {"io.gvx.extension": "ext-other"},
		"c-host":    {},
		"json":      {}, // reserved-name host container must still be scoped
		"create":    {"io.gvx.extension": "ext-other"},
	}
	networks := map[string]map[string]any{
		"bridge":       {"Id": "bridge-id", "Name": "bridge", "Driver": "bridge", "Labels": map[string]string{}},
		"none":         {"Id": "none-id", "Name": "none", "Driver": "null", "Labels": map[string]string{}},
		"host":         {"Id": "host-id", "Name": "host", "Driver": "host", "Labels": map[string]string{}},
		"host-id":      {"Id": "host-id", "Name": "host", "Driver": "host", "Labels": map[string]string{}},
		"ext-net-1":    {"Id": "ext-net-1", "Name": "ext-net-1", "Driver": "bridge", "Labels": map[string]string{"io.gvx.extension": "ext-net"}},
		"attacker-net": {"Id": "attacker-net", "Name": "attacker-net", "Driver": "bridge", "Labels": map[string]string{"io.gvx.extension": "ext-other"}},
		"empty-net":    {}, // empty response must fail closed with 403
	}
	type recordedCall struct {
		method string
		path   string
		body   string
	}
	var calls []recordedCall

	dp := newScopedDockerProxy("ext-net", dataDir, gs, []string{PermDocker}, nil, nil, func() http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			p := stripDockerVersion(cleanDockerMatchPath(r.URL.Path))
			var raw []byte
			if r.Body != nil {
				raw, _ = io.ReadAll(r.Body)
			}
			isReadOnlyCheck := (r.Method == http.MethodGet && strings.HasPrefix(p, "/containers/") && strings.HasSuffix(p, "/json")) ||
				(r.Method == http.MethodGet && strings.HasPrefix(p, "/images/") && strings.HasSuffix(p, "/json")) ||
				(r.Method == http.MethodGet && strings.HasPrefix(p, "/networks/"))
			if !isReadOnlyCheck {
				calls = append(calls, recordedCall{method: r.Method, path: p, body: string(raw)})
			}
			switch {
			case r.Method == http.MethodGet && strings.HasPrefix(p, "/containers/") && strings.HasSuffix(p, "/json"):
				id := strings.TrimSuffix(strings.TrimPrefix(p, "/containers/"), "/json")
				labels, ok := containers[id]
				if !ok {
					http.Error(w, `{"message":"No such container"}`, http.StatusNotFound)
					return
				}
				w.Header().Set("Content-Type", "application/json")
				_ = json.NewEncoder(w).Encode(map[string]any{
					"Id":     id,
					"Config": map[string]any{"Labels": labels},
				})
			case r.Method == http.MethodGet && strings.HasPrefix(p, "/networks/"):
				nid := strings.TrimPrefix(p, "/networks/")
				netObj, ok := networks[nid]
				if !ok {
					http.Error(w, `{"message":"network not found"}`, http.StatusNotFound)
					return
				}
				w.Header().Set("Content-Type", "application/json")
				_ = json.NewEncoder(w).Encode(netObj)
			case r.Method == http.MethodPost && p == "/volumes/create":
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusCreated)
				_, _ = io.WriteString(w, `{"Name":"vol-ok"}`)
			case r.Method == http.MethodPost && p == "/containers/create":
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusCreated)
				_, _ = io.WriteString(w, `{"Id":"c-new"}`)
			case r.Method == http.MethodGet && strings.HasPrefix(p, "/images/") && strings.HasSuffix(p, "/json"):
				http.Error(w, `{"message":"No such image"}`, http.StatusNotFound)
			default:
				w.WriteHeader(http.StatusOK)
			}
		})
	})
	defer dp.Close()

	// 1. Foreign & host containers AND foreign/host/empty networks blocked on /networks/{id}/connect and /disconnect.
	for _, tc := range []struct {
		name string
		path string
		body string
		want int
	}{
		{"connect-foreign", "/networks/attacker-net/connect", `{"Container":"c-foreign"}`, http.StatusForbidden},
		{"connect-owned-to-foreign-net", "/networks/attacker-net/connect", `{"Container":"c-owned"}`, http.StatusForbidden},
		{"disconnect-owned-from-foreign-net", "/networks/attacker-net/disconnect", `{"Container":"c-owned"}`, http.StatusForbidden},
		{"connect-owned-to-host-net", "/networks/host/connect", `{"Container":"c-owned"}`, http.StatusForbidden},
		{"connect-owned-to-HOST-net", "/networks/HOST/connect", `{"Container":"c-owned"}`, http.StatusForbidden},
		{"connect-owned-to-fold-host-net", "/networks/ho\u017ft/connect", `{"Container":"c-owned"}`, http.StatusForbidden},
		{"connect-owned-to-host-id", "/networks/host-id/connect", `{"Container":"c-owned"}`, http.StatusForbidden},
		{"disconnect-owned-from-host-net", "/networks/host/disconnect", `{"Container":"c-owned"}`, http.StatusForbidden},
		{"connect-owned-to-empty-net", "/networks/empty-net/connect", `{"Container":"c-owned"}`, http.StatusForbidden},
		{"connect-host-v152", "/v1.52/networks/bridge/connect", `{"Container":"c-host"}`, http.StatusForbidden},
		{"connect-leading-slash", "/networks/bridge/connect", `{"Container":"/c-foreign"}`, http.StatusForbidden},
		{"disconnect-foreign-force", "/networks/bridge/disconnect", `{"Container":"c-foreign","Force":true}`, http.StatusForbidden},
		{"disconnect-host-lower", "/v1.47/networks/bridge/disconnect", `{"container":"c-host"}`, http.StatusForbidden},
		{"connect-missing-container", "/networks/bridge/connect", `{}`, http.StatusBadRequest},
		{"connect-empty-container", "/networks/bridge/connect", `{"Container":"  "}`, http.StatusBadRequest},
		{"connect-dup-container", "/networks/bridge/connect", `{"Container":"c-owned","container":"c-foreign"}`, http.StatusBadRequest},
		{"connect-traversal-container", "/networks/bridge/connect", `{"Container":"c-owned/../c-foreign"}`, http.StatusBadRequest},
		{"connect-unknown-container", "/networks/bridge/connect", `{"Container":"does-not-exist"}`, http.StatusNotFound},
	} {
		code, resp := doScopedDockerReq(t, dp, http.MethodPost, tc.path, []byte(tc.body))
		if code != tc.want {
			t.Fatalf("%s: expected %d, got %d (%s)", tc.name, tc.want, code, resp)
		}
	}

	// 1b. Host & foreign networks blocked in /containers/create NetworkMode and EndpointsConfig.
	for _, tc := range []struct {
		name string
		body string
	}{
		{"create-netmode-host-id", `{"Image":"alpine:3.20","HostConfig":{"NetworkMode":"host-id"}}`},
		{"create-netmode-foreign", `{"Image":"alpine:3.20","HostConfig":{"NetworkMode":"attacker-net"}}`},
		{"create-endpoints-host", `{"Image":"alpine:3.20","NetworkingConfig":{"EndpointsConfig":{"host":{}}}}`},
		{"create-endpoints-HOST", `{"Image":"alpine:3.20","NetworkingConfig":{"EndpointsConfig":{"HOST":{}}}}`},
		{"create-endpoints-host-id", `{"Image":"alpine:3.20","NetworkingConfig":{"EndpointsConfig":{"host-id":{}}}}`},
		{"create-endpoints-foreign", `{"Image":"alpine:3.20","NetworkingConfig":{"EndpointsConfig":{"attacker-net":{}}}}`},
	} {
		code, resp := doScopedDockerCreate(t, dp, "/containers/create", []byte(tc.body))
		if code != http.StatusForbidden {
			t.Fatalf("%s: expected 403 Forbidden, got %d (%s)", tc.name, code, resp)
		}
	}

	// 2. Host/foreign containers named "json" or "create" cannot bypass /containers/ ownership check.
	for _, tc := range []struct {
		method string
		path   string
	}{
		{http.MethodDelete, "/containers/json"},
		{http.MethodGet, "/containers/json/archive"},
		{http.MethodPost, "/containers/create/start"},
	} {
		code, resp := doScopedDockerReq(t, dp, tc.method, tc.path, nil)
		if code != http.StatusForbidden {
			t.Fatalf("%s %s: expected 403 Forbidden, got %d (%s)", tc.method, tc.path, code, resp)
		}
	}

	// 3. POST /volumes/create rejects dangerous host-bind DriverOpts (including SimpleFold variants).
	for _, tc := range []struct {
		name string
		body string
		want int
	}{
		{"vol-o-bind", `{"Name":"bad1","DriverOpts":{"type":"none","o":"bind","device":"/etc"}}`, http.StatusForbidden},
		{"vol-device-only", `{"Name":"bad2","DriverOpts":{"Device":"/var/run/docker.sock"}}`, http.StatusForbidden},
		{"vol-type-bind", `{"Name":"bad3","DriverOpts":{"type":"BIND"}}`, http.StatusForbidden},
		{"vol-o-rw-bind", `{"Name":"bad4","DriverOpts":{"O":"rw,bind"}}`, http.StatusForbidden},
		{"vol-long-s-driveropts", `{"Name":"bad5","driveropt\u017f":{"o":"bind","device":"/etc"}}`, http.StatusForbidden},
		{"vol-dup-driveropts", `{"Name":"bad6","DriverOpts":{},"driveropts":{"o":"bind"}}`, http.StatusBadRequest},
	} {
		code, resp := doScopedDockerReq(t, dp, http.MethodPost, "/volumes/create", []byte(tc.body))
		if code != tc.want {
			t.Fatalf("%s: expected %d, got %d (%s)", tc.name, tc.want, code, resp)
		}
	}
	if len(calls) != 0 {
		t.Fatalf("expected 0 backend mutations from rejected requests, got %+v", calls)
	}

	// 4. Owned network connect/disconnect and safe volumes/create succeed and forward canonical JSON bodies.
	code, resp := doScopedDockerReq(t, dp, http.MethodPost, "/v1.52/networks/ext-net-1/connect", []byte(`{"container":"c-owned","EndpointConfig":{"Aliases":["app"]}}`))
	if code != http.StatusOK {
		t.Fatalf("owned network connect: expected 200, got %d (%s)", code, resp)
	}
	code, resp = doScopedDockerReq(t, dp, http.MethodPost, "/networks/ext-net-1/disconnect", []byte(`{"Container":"c-owned","Force":false}`))
	if code != http.StatusOK {
		t.Fatalf("owned network disconnect: expected 200, got %d (%s)", code, resp)
	}
	code, resp = doScopedDockerReq(t, dp, http.MethodPost, "/volumes/create", []byte(`{"Name":"vol-ok","DriverOpts":{"type":"hostfs"}}`))
	if code != http.StatusCreated {
		t.Fatalf("safe volumes/create: expected 201, got %d (%s)", code, resp)
	}
	code, resp = doScopedDockerReq(t, dp, http.MethodPost, "/volumes/create", nil)
	if code != http.StatusCreated {
		t.Fatalf("empty body volumes/create: expected 201, got %d (%s)", code, resp)
	}
	if len(calls) != 4 {
		t.Fatalf("expected 4 allowed backend calls, got %d (%+v)", len(calls), calls)
	}
	if !strings.Contains(calls[0].body, `"Container":"c-owned"`) || !strings.Contains(calls[0].body, `"Aliases":["app"]`) {
		t.Fatalf("expected canonical Container & preserved EndpointConfig in forwarded connect body, got %s", calls[0].body)
	}
}

func TestDockerScope_EventsVolumesNetworksImagesBeforeSinceAndCommit(t *testing.T) {
	dataDir := t.TempDir()
	gs := newTestGrantStoreWithDocker(t, dataDir, "ext-a", nil)

	var mu sync.Mutex
	containers := map[string]map[string]string{
		"c-owned":   {"io.gvx.extension": "ext-a"},
		"c-foreign": {"io.gvx.extension": "ext-b"},
		"c-host":    {},
	}
	containerImages := map[string]string{
		"c-owned":   "sha256:img-owned",
		"c-foreign": "sha256:img-shared",
	}
	volumes := map[string]map[string]string{
		"vol-foreign": {"io.gvx.extension": "ext-b"},
		"vol-host":    {},
	}
	networks := map[string]struct {
		id         string
		name       string
		labels     map[string]string
		containers map[string]any
	}{
		"bridge": {
			id:     "bridge-id",
			name:   "bridge",
			labels: map[string]string{},
			containers: map[string]any{
				"c-owned":   map[string]any{"Name": "c-owned", "IPv4Address": "10.0.2.15/24"},
				"c-foreign": map[string]any{"Name": "c-foreign", "IPv4Address": "10.0.2.16/24"},
				"c-host":    map[string]any{"Name": "c-host", "IPv4Address": "10.0.2.17/24"},
			},
		},
		"net-foreign": {
			id:     "net-foreign-id",
			name:   "net-foreign",
			labels: map[string]string{"io.gvx.extension": "ext-b"},
			containers: map[string]any{
				"c-foreign": map[string]any{"Name": "c-foreign", "IPv4Address": "10.89.1.2/24"},
			},
		},
	}
	images := map[string]struct {
		id     string
		labels map[string]string
	}{
		"alpine:3.20":       {id: "sha256:img-shared", labels: map[string]string{}},
		"ext-b-img:1":       {id: "sha256:img-foreign", labels: map[string]string{"io.gvx.extension": "ext-b"}},
		"ext-a-img:1":       {id: "sha256:img-owned", labels: map[string]string{"io.gvx.extension": "ext-a"}},
		"sha256:img-shared": {id: "sha256:img-shared", labels: map[string]string{}},
	}
	eventSubCh := make(chan string, 32)

	backend := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := stripDockerVersion(cleanDockerMatchPath(r.URL.Path))
		switch {
		case r.Method == http.MethodGet && p == "/events":
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			rc := http.NewResponseController(w)
			_ = rc.Flush()
			for {
				select {
				case <-r.Context().Done():
					return
				case line, ok := <-eventSubCh:
					if !ok {
						return
					}
					_, _ = io.WriteString(w, line+"\n")
					_ = rc.Flush()
				}
			}

		case r.Method == http.MethodGet && p == "/containers/json":
			rawF := r.URL.Query().Get("filters")
			if strings.Contains(rawF, "does-not-exist") {
				http.Error(w, `{"message":"No such container: does-not-exist"}`, http.StatusNotFound)
				return
			}
			mu.Lock()
			var list []map[string]any
			for cid, lbls := range containers {
				list = append(list, map[string]any{
					"Id":      cid,
					"ImageID": containerImages[cid],
					"Labels":  lbls,
				})
			}
			mu.Unlock()
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(list)

		case r.Method == http.MethodGet && strings.HasPrefix(p, "/containers/") && strings.HasSuffix(p, "/json"):
			cid := strings.TrimSuffix(strings.TrimPrefix(p, "/containers/"), "/json")
			mu.Lock()
			lbls, ok := containers[cid]
			mu.Unlock()
			if !ok {
				http.Error(w, `{"message":"No such container: `+cid+`"}`, http.StatusNotFound)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{"Id": cid, "Config": map[string]any{"Labels": lbls}})

		case r.Method == http.MethodPost && p == "/volumes/create":
			var req struct {
				Name   string            `json:"Name"`
				Labels map[string]string `json:"Labels"`
			}
			raw, _ := io.ReadAll(r.Body)
			_ = json.Unmarshal(raw, &req)
			if req.Name == "" {
				req.Name = "vol-anon"
			}
			mu.Lock()
			if existing, ok := volumes[req.Name]; ok {
				mu.Unlock()
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusCreated)
				_ = json.NewEncoder(w).Encode(map[string]any{"Name": req.Name, "Labels": existing})
				return
			}
			volumes[req.Name] = req.Labels
			mu.Unlock()
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(map[string]any{"Name": req.Name, "Labels": req.Labels})

		case r.Method == http.MethodGet && p == "/volumes":
			mu.Lock()
			var list []map[string]any
			for name, lbls := range volumes {
				list = append(list, map[string]any{"Name": name, "Labels": lbls})
			}
			mu.Unlock()
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{"Volumes": list, "Warnings": []string{}})

		case r.Method == http.MethodGet && strings.HasPrefix(p, "/volumes/"):
			name := strings.TrimPrefix(p, "/volumes/")
			mu.Lock()
			lbls, ok := volumes[name]
			mu.Unlock()
			if !ok {
				http.Error(w, `{"message":"no such volume"}`, http.StatusNotFound)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{"Name": name, "Labels": lbls})

		case r.Method == http.MethodDelete && strings.HasPrefix(p, "/volumes/"):
			name := strings.TrimPrefix(p, "/volumes/")
			mu.Lock()
			_, ok := volumes[name]
			if ok {
				delete(volumes, name)
			}
			mu.Unlock()
			if !ok {
				http.Error(w, `{"message":"no such volume"}`, http.StatusNotFound)
				return
			}
			w.WriteHeader(http.StatusNoContent)

		case r.Method == http.MethodPost && p == "/networks/create":
			var req struct {
				Name   string            `json:"Name"`
				Labels map[string]string `json:"Labels"`
			}
			raw, _ := io.ReadAll(r.Body)
			_ = json.Unmarshal(raw, &req)
			mu.Lock()
			networks[req.Name] = struct {
				id         string
				name       string
				labels     map[string]string
				containers map[string]any
			}{
				id:         req.Name + "-id",
				name:       req.Name,
				labels:     req.Labels,
				containers: map[string]any{"c-owned": map[string]any{"Name": "c-owned"}},
			}
			mu.Unlock()
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(map[string]any{"Id": req.Name + "-id", "Warning": ""})

		case r.Method == http.MethodGet && p == "/networks":
			mu.Lock()
			var list []map[string]any
			for _, n := range networks {
				ctrs := map[string]any{}
				for k, v := range n.containers {
					ctrs[k] = v
				}
				list = append(list, map[string]any{
					"Id":         n.id,
					"Name":       n.name,
					"Labels":     n.labels,
					"Containers": ctrs,
				})
			}
			mu.Unlock()
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(list)

		case r.Method == http.MethodGet && strings.HasPrefix(p, "/networks/"):
			nid := strings.TrimPrefix(p, "/networks/")
			mu.Lock()
			n, ok := networks[nid]
			mu.Unlock()
			if !ok {
				http.Error(w, `{"message":"network not found"}`, http.StatusNotFound)
				return
			}
			ctrs := map[string]any{}
			for k, v := range n.containers {
				ctrs[k] = v
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"Id":         n.id,
				"Name":       n.name,
				"Labels":     n.labels,
				"Containers": ctrs,
			})

		case r.Method == http.MethodDelete && strings.HasPrefix(p, "/networks/"):
			nid := strings.TrimPrefix(p, "/networks/")
			mu.Lock()
			_, ok := networks[nid]
			if ok {
				delete(networks, nid)
			}
			mu.Unlock()
			if !ok {
				http.Error(w, `{"message":"network not found"}`, http.StatusNotFound)
				return
			}
			w.WriteHeader(http.StatusNoContent)

		case r.Method == http.MethodGet && p == "/images/json":
			rawF := r.URL.Query().Get("filters")
			if strings.Contains(rawF, "missing-img") {
				http.Error(w, `{"message":"No such image: missing-img"}`, http.StatusNotFound)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `[{"Id":"sha256:img-owned","RepoTags":["ext-a-img:1"]}]`)

		case r.Method == http.MethodPost && p == "/images/create":
			if strings.Contains(r.URL.RawQuery, "fromSrc=") {
				repo := r.URL.Query().Get("repo")
				tag := r.URL.Query().Get("tag")
				labels := map[string]string{}
				for _, ch := range r.URL.Query()["changes"] {
					if strings.Contains(ch, "io.gvx.extension=ext-a") {
						labels["io.gvx.extension"] = "ext-a"
					}
				}
				if repo != "" {
					ref := repo + ":latest"
					if tag != "" {
						ref = repo + ":" + tag
					}
					mu.Lock()
					images[ref] = struct {
						id     string
						labels map[string]string
					}{id: "sha256:imported", labels: labels}
					mu.Unlock()
				}
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusOK)
				_, _ = io.WriteString(w, `{"status":"sha256:imported"}`+"\n")
				return
			}
			w.WriteHeader(http.StatusOK)

		case r.Method == http.MethodGet && strings.HasPrefix(p, "/images/") && strings.HasSuffix(p, "/json"):
			ref := strings.TrimSuffix(strings.TrimPrefix(p, "/images/"), "/json")
			mu.Lock()
			img, ok := images[ref]
			mu.Unlock()
			if !ok {
				http.Error(w, `{"message":"No such image"}`, http.StatusNotFound)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"Id":     img.id,
				"Config": map[string]any{"Labels": img.labels},
			})

		case r.Method == http.MethodDelete && strings.HasPrefix(p, "/images/"):
			ref := strings.TrimPrefix(p, "/images/")
			mu.Lock()
			_, ok := images[ref]
			delete(images, ref)
			mu.Unlock()
			if !ok {
				http.Error(w, `{"message":"No such image"}`, http.StatusNotFound)
				return
			}
			w.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(w, `[{"Deleted":"`+ref+`"}]`)

		case r.Method == http.MethodPost && strings.HasPrefix(p, "/images/") && strings.HasSuffix(p, "/tag"):
			w.WriteHeader(http.StatusCreated)

		case r.Method == http.MethodPost && p == "/commit":
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusCreated)
			_, _ = io.WriteString(w, `{"Id":"sha256:committed"}`)

		default:
			w.WriteHeader(http.StatusOK)
		}
	})

	dp := newScopedDockerProxy("ext-a", dataDir, gs, []string{PermDocker}, nil, nil, func() http.Handler {
		return backend
	})
	defer dp.Close()

	// 1. Finding 1: GET /events filters out cross-extension and host events while passing ext-a's own events.
	proxySrv := httptest.NewServer(dp)
	defer proxySrv.Close()
	evCtx, evCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer evCancel()
	evReq, _ := http.NewRequestWithContext(evCtx, http.MethodGet, proxySrv.URL+"/events", nil)
	evResp, err := proxySrv.Client().Do(evReq)
	if err != nil {
		t.Fatalf("GET /events: %v", err)
	}
	defer evResp.Body.Close()
	if evResp.StatusCode != http.StatusOK {
		t.Fatalf("GET /events status=%d", evResp.StatusCode)
	}
	lineCh := make(chan string, 16)
	go func() {
		buf := make([]byte, 0, 4096)
		tmp := make([]byte, 512)
		for {
			n, rerr := evResp.Body.Read(tmp)
			if n > 0 {
				buf = append(buf, tmp[:n]...)
				for {
					idx := bytes.IndexByte(buf, '\n')
					if idx < 0 {
						break
					}
					lineCh <- string(buf[:idx])
					buf = buf[idx+1:]
				}
			}
			if rerr != nil {
				close(lineCh)
				return
			}
		}
	}()

	eventSubCh <- `{"Type":"container","Action":"start","Actor":{"ID":"c-foreign","Attributes":{"io.gvx.extension":"ext-b","secret":"leak-me"}}}`
	eventSubCh <- `{"Type":"container","Action":"start","Actor":{"ID":"c-host","Attributes":{"name":"host-ctr"}}}`
	eventSubCh <- `{"Type":"network","Action":"connect","Actor":{"ID":"bridge-id","Attributes":{"container":"c-foreign","name":"bridge"}}}`
	eventSubCh <- `{"Type":"volume","Action":"create","Actor":{"ID":"vol-foreign","Attributes":{"driver":"local"}}}`
	eventSubCh <- `{"Type":"network","Action":"create","Actor":{"ID":"net-foreign","Attributes":{"name":"net-foreign"}}}`
	eventSubCh <- `{"Type":"image","Action":"pull","Actor":{"ID":"private/secret:latest","Attributes":{"name":"private/secret:latest"}}}`
	eventSubCh <- `{"Type":"container","Action":"start","Actor":{"ID":"c-owned","Attributes":{"io.gvx.extension":"ext-a","name":"mine"}}}`
	eventSubCh <- `{"Type":"network","Action":"connect","Actor":{"ID":"bridge-id","Attributes":{"container":"c-owned","name":"bridge"}}}`

	var gotLines []string
	deadline := time.After(2 * time.Second)
	for len(gotLines) < 2 {
		select {
		case ln, ok := <-lineCh:
			if !ok {
				t.Fatalf("events stream closed early, got %v", gotLines)
			}
			gotLines = append(gotLines, ln)
		case <-deadline:
			t.Fatalf("timed out waiting for filtered events, got %v", gotLines)
		}
	}
	joinedEvents := strings.Join(gotLines, "\n")
	for _, secret := range []string{"c-foreign", "c-host", "leak-me", "vol-foreign", "net-foreign", "private/secret"} {
		if strings.Contains(joinedEvents, secret) {
			t.Fatalf("GET /events leaked %q in %s", secret, joinedEvents)
		}
	}
	if !strings.Contains(joinedEvents, "c-owned") {
		t.Fatalf("expected c-owned events, got %s", joinedEvents)
	}
	evCancel()

	// 2. Finding 2: /volumes scoping & ownership.
	if code, resp := doScopedDockerReq(t, dp, http.MethodPost, "/volumes/create", []byte(`{"Name":"vol-spoof","Labels":{"io.gvx.extension":"ext-b"}}`)); code != http.StatusForbidden {
		t.Fatalf("spoofed volume label: expected 403, got %d (%s)", code, resp)
	}
	if code, resp := doScopedDockerReq(t, dp, http.MethodPost, "/volumes/create", []byte(`{"Name":"vol-foreign"}`)); code != http.StatusForbidden {
		t.Fatalf("existing foreign volume create: expected 403, got %d (%s)", code, resp)
	}
	if code, resp := doScopedDockerReq(t, dp, http.MethodPost, "/volumes/create", []byte(`{"Name":"vol-owned"}`)); code != http.StatusCreated {
		t.Fatalf("owned volume create: expected 201, got %d (%s)", code, resp)
	}
	if code, resp := doScopedDockerReq(t, dp, http.MethodGet, "/volumes", nil); code != http.StatusOK || strings.Contains(resp, "vol-foreign") || strings.Contains(resp, "vol-host") || !strings.Contains(resp, "vol-owned") {
		t.Fatalf("GET /volumes isolation failed: code=%d resp=%s", code, resp)
	}
	if code, resp := doScopedDockerReq(t, dp, http.MethodGet, "/volumes/vol-foreign", nil); code != http.StatusForbidden {
		t.Fatalf("GET /volumes/vol-foreign: expected 403, got %d (%s)", code, resp)
	}
	if code, resp := doScopedDockerReq(t, dp, http.MethodDelete, "/volumes/vol-foreign", nil); code != http.StatusForbidden {
		t.Fatalf("DELETE /volumes/vol-foreign: expected 403, got %d (%s)", code, resp)
	}
	if code, resp := doScopedDockerReq(t, dp, http.MethodDelete, "/volumes/vol-missing", nil); code != http.StatusNotFound {
		t.Fatalf("DELETE /volumes/vol-missing: expected 404, got %d (%s)", code, resp)
	}
	if code, resp := doScopedDockerReq(t, dp, http.MethodDelete, "/volumes/vol-owned", nil); code != http.StatusNoContent {
		t.Fatalf("DELETE /volumes/vol-owned: expected 204, got %d (%s)", code, resp)
	}

	// 3. Finding 2: /networks scoping, container scrubbing, & deletion.
	if code, resp := doScopedDockerReq(t, dp, http.MethodPost, "/networks/create", []byte(`{"Name":"net-spoof","Labels":{"io.gvx.extension":"ext-b"}}`)); code != http.StatusForbidden {
		t.Fatalf("spoofed network label: expected 403, got %d (%s)", code, resp)
	}
	if code, resp := doScopedDockerReq(t, dp, http.MethodPost, "/networks/create", []byte(`{"Name":"net-owned"}`)); code != http.StatusCreated {
		t.Fatalf("owned network create: expected 201, got %d (%s)", code, resp)
	}
	if code, resp := doScopedDockerReq(t, dp, http.MethodGet, "/networks", nil); code != http.StatusOK || strings.Contains(resp, "net-foreign") || strings.Contains(resp, "c-foreign") || strings.Contains(resp, "c-host") || !strings.Contains(resp, "c-owned") || !strings.Contains(resp, "net-owned") {
		t.Fatalf("GET /networks isolation failed: code=%d resp=%s", code, resp)
	}
	if code, resp := doScopedDockerReq(t, dp, http.MethodGet, "/networks/bridge", nil); code != http.StatusOK || strings.Contains(resp, "c-foreign") || strings.Contains(resp, "c-host") || !strings.Contains(resp, "c-owned") {
		t.Fatalf("GET /networks/bridge scrub failed: code=%d resp=%s", code, resp)
	}
	if code, resp := doScopedDockerReq(t, dp, http.MethodGet, "/networks/net-foreign", nil); code != http.StatusForbidden {
		t.Fatalf("GET /networks/net-foreign: expected 403, got %d (%s)", code, resp)
	}
	for _, forbiddenNet := range []string{"bridge", "host", "none", "net-foreign"} {
		if code, resp := doScopedDockerReq(t, dp, http.MethodDelete, "/networks/"+forbiddenNet, nil); code != http.StatusForbidden {
			t.Fatalf("DELETE /networks/%s: expected 403, got %d (%s)", forbiddenNet, code, resp)
		}
	}
	if code, resp := doScopedDockerReq(t, dp, http.MethodDelete, "/networks/net-owned", nil); code != http.StatusNoContent {
		t.Fatalf("DELETE /networks/net-owned: expected 204, got %d (%s)", code, resp)
	}

	// 4. Finding 2: /images deletion & tag protection.
	if code, resp := doScopedDockerReq(t, dp, http.MethodDelete, "/images/alpine:3.20?force=1", nil); code != http.StatusForbidden {
		t.Fatalf("DELETE host image: expected 403, got %d (%s)", code, resp)
	}
	if code, resp := doScopedDockerReq(t, dp, http.MethodDelete, "/images/ext-b-img:1?force=1", nil); code != http.StatusForbidden {
		t.Fatalf("DELETE foreign image: expected 403, got %d (%s)", code, resp)
	}
	if code, resp := doScopedDockerReq(t, dp, http.MethodPost, "/images/alpine:3.20/tag?repo=ext-b-img&tag=1", nil); code != http.StatusForbidden {
		t.Fatalf("tag overwrite foreign image: expected 403, got %d (%s)", code, resp)
	}
	if code, resp := doScopedDockerReq(t, dp, http.MethodDelete, "/images/ext-a-img:1", nil); code != http.StatusOK {
		t.Fatalf("DELETE owned image: expected 200, got %d (%s)", code, resp)
	}

	// 5. Finding 5: GET /containers/json before/since anti-oracle.
	codeForeign, respForeign := doScopedDockerReq(t, dp, http.MethodGet, `/containers/json?all=1&filters=`+`%7B%22before%22%3A%5B%22c-foreign%22%5D%7D`, nil)
	codeMissing, respMissing := doScopedDockerReq(t, dp, http.MethodGet, `/containers/json?all=1&filters=`+`%7B%22before%22%3A%5B%22does-not-exist%22%5D%7D`, nil)
	if codeForeign != http.StatusForbidden || codeMissing != http.StatusForbidden || respForeign != respMissing {
		t.Fatalf("before oracle leaked: foreign=(%d,%s) missing=(%d,%s)", codeForeign, respForeign, codeMissing, respMissing)
	}
	if code, resp := doScopedDockerReq(t, dp, http.MethodGet, `/containers/json?all=1&filters=`+`%7B%22since%22%3A%7B%22c-host%22%3Atrue%7D%7D`, nil); code != http.StatusForbidden {
		t.Fatalf("since host container: expected 403, got %d (%s)", code, resp)
	}
	if code, resp := doScopedDockerReq(t, dp, http.MethodGet, `/containers/json?all=1&filters=`+`%7B%22before%22%3A%5B%22c-owned%22%5D%7D`, nil); code != http.StatusOK {
		t.Fatalf("before owned container: expected 200, got %d (%s)", code, resp)
	}

	// 6. Finding 6: POST /commit multi-value container= and label spoofing rejected (including multiline & backslash escapes).
	for _, tc := range []struct {
		path string
		body string
		want int
	}{
		{"/commit", "", http.StatusBadRequest},
		{"/commit?container=", "", http.StatusBadRequest},
		{"/commit?container=c-owned&container=c-foreign", "", http.StatusBadRequest},
		{"/commit?container=&container=c-foreign", "", http.StatusBadRequest},
		{"/commit?container=c-foreign", "", http.StatusForbidden},
		{"/commit?container=c-owned", `{"Labels":{"io.gvx.extension":"ext-b"}}`, http.StatusForbidden},
		{"/commit?container=c-owned&changes=LABEL%20io.gvx.extension%3Dext-b", "", http.StatusForbidden},
		{"/commit?container=c-owned&changes=ENV%20A%3D1%0ALABEL%20io.gvx.extension%3Dext-b", "", http.StatusForbidden},
		{"/commit?container=c-owned&changes=ENV%20A%3D1%0DLABEL%20io.gvx.extension%3Dext-b", "", http.StatusForbidden},
		{"/commit?container=c-owned&changes=LABEL%20io.%5Cgvx.extension%3Dext-b", "", http.StatusForbidden},
		{"/commit?container=c-owned&changes=LABEL%20%22io.gvx.extension%22%3Dext-b", "", http.StatusForbidden},
		{"/commit?container=c-owned&changes=LABEL%20io.gvx.extension%20ext-b", "", http.StatusForbidden},
		{"/commit?container=c-owned&changes=LABEL%20a%3D1%20io.gvx.exten%C5%BFion%3Dext-b", "", http.StatusForbidden},
		{"/commit?container=c-owned&changes=LABEL%20%22unterminated", "", http.StatusBadRequest},
		{"/commit?container=c-owned&repo=ext-b-img&tag=1", "", http.StatusForbidden},
		{"/commit?container=c-owned&changes=ENV%20A%3D1%0ALABEL%20app.version%3D1.0", "", http.StatusCreated},
		{"/commit?container=c-owned", "", http.StatusCreated},
	} {
		code, resp := doScopedDockerReq(t, dp, http.MethodPost, tc.path, []byte(tc.body))
		if code != tc.want {
			t.Fatalf("POST %s body=%s: expected %d, got %d (%s)", tc.path, tc.body, tc.want, code, resp)
		}
	}

	// 7. POST /images/create (fromSrc) & POST /images/load & GET /images/json before/since anti-oracle.
	for _, tc := range []struct {
		name   string
		method string
		path   string
		want   int
	}{
		{"import-label-spoof", http.MethodPost, "/images/create?fromSrc=-&repo=ext-a-new&tag=1&changes=LABEL%20io.gvx.extension%3Dext-b", http.StatusForbidden},
		{"import-multiline-escape-spoof", http.MethodPost, "/images/create?fromSrc=-&repo=ext-a-new&tag=1&changes=ENV%20A%3D1%0ALABEL%20io.%5Cgvx.extension%3Dext-b", http.StatusForbidden},
		{"import-overwrite-foreign-tag", http.MethodPost, "/images/create?fromSrc=-&repo=ext-b-img&tag=1", http.StatusForbidden},
		{"import-overwrite-host-tag", http.MethodPost, "/images/create?fromSrc=-&repo=alpine&tag=3.20", http.StatusForbidden},
		{"import-owned-ok", http.MethodPost, "/images/create?fromSrc=-&repo=ext-a-new&tag=1", http.StatusOK},
		{"images-load-blocked", http.MethodPost, "/images/load", http.StatusForbidden},
		{"images-json-dup-filters", http.MethodGet, `/images/json?filters=%7B%7D&filters=%7B%7D`, http.StatusBadRequest},
		{"images-json-since-host", http.MethodGet, `/images/json?filters=%7B%22since%22%3A%7B%22alpine%3A3.20%22%3Atrue%7D%7D`, http.StatusForbidden},
		{"images-json-before-owned", http.MethodGet, `/images/json?filters=%7B%22before%22%3A%5B%22ext-a-new%3A1%22%5D%7D`, http.StatusOK},
	} {
		code, resp := doScopedDockerReq(t, dp, tc.method, tc.path, nil)
		if code != tc.want {
			t.Fatalf("%s (%s %s): expected %d, got %d (%s)", tc.name, tc.method, tc.path, tc.want, code, resp)
		}
	}
	// Verify imported image was injected with io.gvx.extension=ext-a and can be inspected/deleted as owned.
	if code, resp := doScopedDockerReq(t, dp, http.MethodDelete, "/images/ext-a-new:1", nil); code != http.StatusOK {
		t.Fatalf("expected imported owned image to be deletable, got %d (%s)", code, resp)
	}
	codeImgForeign, respImgForeign := doScopedDockerReq(t, dp, http.MethodGet, `/images/json?filters=%7B%22before%22%3A%5B%22ext-b-img%3A1%22%5D%7D`, nil)
	codeImgMissing, respImgMissing := doScopedDockerReq(t, dp, http.MethodGet, `/images/json?filters=%7B%22before%22%3A%5B%22missing-img%3A1%22%5D%7D`, nil)
	if codeImgForeign != http.StatusForbidden || codeImgMissing != http.StatusForbidden || respImgForeign != respImgMissing {
		t.Fatalf("GET /images/json before oracle leaked: foreign=(%d,%s) missing=(%d,%s)", codeImgForeign, respImgForeign, codeImgMissing, respImgMissing)
	}
}

type fakeEngineRunner struct {
	mu      sync.Mutex
	volumes map[string]rungv.Volume
	images  map[string]engine.ImageInspect
	started []sandbox.Spec
	eventFn func(sandbox.Event)
	outFn   func(string, sandbox.Output)
}

func newFakeEngineRunner() *fakeEngineRunner {
	return &fakeEngineRunner{
		volumes: make(map[string]rungv.Volume),
		images:  make(map[string]engine.ImageInspect),
	}
}

func (f *fakeEngineRunner) SetHandlers(event func(sandbox.Event), output func(id string, o sandbox.Output)) {
	f.mu.Lock()
	f.eventFn = event
	f.outFn = output
	f.mu.Unlock()
}

func (f *fakeEngineRunner) Start(spec sandbox.Spec) error {
	f.mu.Lock()
	f.started = append(f.started, spec.Clone())
	f.mu.Unlock()
	return nil
}
func (f *fakeEngineRunner) Stop(string, time.Duration) error      { return nil }
func (f *fakeEngineRunner) Signal(string, syscall.Signal) error   { return nil }
func (f *fakeEngineRunner) State(string) sandbox.State            { return sandbox.State{} }
func (f *fakeEngineRunner) Stdin(string) io.Writer                { return nil }
func (f *fakeEngineRunner) CloseStdin(string) error               { return nil }
func (f *fakeEngineRunner) Resize(string, uint16, uint16) error   { return nil }
func (f *fakeEngineRunner) Forget(string)                         {}
func (f *fakeEngineRunner) Features() rungv.Features              { return rungv.Features{KeepBlobs: true} }
func (f *fakeEngineRunner) Control(string) sandbox.Control        { return nil }
func (f *fakeEngineRunner) Pause(context.Context, string) error   { return nil }
func (f *fakeEngineRunner) Unpause(context.Context, string) error { return nil }
func (f *fakeEngineRunner) Update(context.Context, string, rungv.Update) (rungv.Resources, error) {
	return rungv.Resources{}, nil
}
func (f *fakeEngineRunner) Shutdown(string, string, time.Duration) error { return nil }

func (f *fakeEngineRunner) Images(context.Context) ([]rungv.Image, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []rungv.Image
	seen := map[string]bool{}
	for _, img := range f.images {
		if seen[img.ID] {
			continue
		}
		seen[img.ID] = true
		for _, rt := range img.RepoTags {
			repo, tag, _ := strings.Cut(rt, ":")
			out = append(out, rungv.Image{
				ID:         img.ID,
				Repository: repo,
				Tag:        tag,
				CreatedAt:  img.Created.UTC().Format(time.RFC3339),
			})
		}
	}
	return out, nil
}

func (f *fakeEngineRunner) InspectImages(_ context.Context, refs ...string) ([]engine.ImageInspect, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []engine.ImageInspect
	for _, ref := range refs {
		var found *engine.ImageInspect
		if img, ok := f.images[ref]; ok {
			cp := img
			found = &cp
		} else {
			for _, img := range f.images {
				if img.ID == ref {
					cp := img
					found = &cp
					break
				}
				for _, rt := range img.RepoTags {
					if rt == ref || rt == ref+":latest" {
						cp := img
						found = &cp
						break
					}
				}
				if found != nil {
					break
				}
			}
		}
		if found == nil {
			return nil, fmt.Errorf("No such image: %s", ref)
		}
		out = append(out, *found)
	}
	return out, nil
}

func (f *fakeEngineRunner) Pull(_ context.Context, ref, _ string, line func(string)) error {
	if line != nil {
		line("Status: Downloaded newer image for " + ref)
	}
	return nil
}

func (f *fakeEngineRunner) RemoveImage(_ context.Context, ref string, _ bool) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.images[ref]; !ok {
		return fmt.Errorf("No such image: %s", ref)
	}
	delete(f.images, ref)
	return nil
}

func (f *fakeEngineRunner) TagImage(_ context.Context, source, target string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	img, ok := f.images[source]
	if !ok {
		for _, cand := range f.images {
			if cand.ID == source {
				img = cand
				ok = true
				break
			}
		}
	}
	if !ok {
		return fmt.Errorf("No such image: %s", source)
	}
	img.RepoTags = append(append([]string(nil), img.RepoTags...), target)
	f.images[target] = img
	return nil
}

func (f *fakeEngineRunner) LoadImages(context.Context, io.Reader, func(string)) error { return nil }

func (f *fakeEngineRunner) SaveImages(_ context.Context, w io.Writer, refs ...string) error {
	tw := tar.NewWriter(w)
	data := []byte(strings.Join(refs, ","))
	_ = tw.WriteHeader(&tar.Header{Name: "manifest.json", Mode: 0o600, Size: int64(len(data))})
	_, _ = tw.Write(data)
	return tw.Close()
}

func (f *fakeEngineRunner) PruneImages(context.Context) (string, error) { return "", nil }

func (f *fakeEngineRunner) Volumes(context.Context) ([]rungv.Volume, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]rungv.Volume, 0, len(f.volumes))
	for _, v := range f.volumes {
		out = append(out, v)
	}
	return out, nil
}

func (f *fakeEngineRunner) CreateVolume(_ context.Context, name, driver string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.volumes[name]; ok {
		return errors.New("already exists")
	}
	f.volumes[name] = rungv.Volume{
		Name:      name,
		Driver:    driver,
		CreatedAt: time.Now().UTC().Format(time.RFC3339),
	}
	return nil
}

func (f *fakeEngineRunner) RemoveVolume(_ context.Context, name string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.volumes[name]; !ok {
		return errors.New("not found")
	}
	delete(f.volumes, name)
	return nil
}

func (f *fakeEngineRunner) DiskUsage(context.Context) ([]rungv.DiskUsage, error) { return nil, nil }
func (f *fakeEngineRunner) Info(context.Context) (rungv.Info, error)             { return rungv.Info{}, nil }
func (f *fakeEngineRunner) PushImage(_ context.Context, _ rungv.PushOptions, line func(string)) error {
	if line != nil {
		line("Status: pushed")
	}
	return nil
}

func TestDockerScope_RealEngine_NamedVolumesBuildAndImageIsolation(t *testing.T) {
	runner := newFakeEngineRunner()
	// Pre-seed user rungv volume and gvx-buildkit cache volume outside engine volumes.json.
	_ = runner.CreateVolume(context.Background(), "user-sandbox-vol", "ext4lite")
	_ = runner.CreateVolume(context.Background(), "gvx-buildkit", "ext4lite")

	now := time.Now().UTC()
	runner.images["alpine:3.20"] = engine.ImageInspect{
		ID:           "sha256:1111111111111111111111111111111111111111111111111111111111111111",
		RepoTags:     []string{"alpine:3.20"},
		Created:      now,
		Architecture: runtime.GOARCH,
		Os:           "linux",
		Config:       engine.ImageConfig{Cmd: []string{"/bin/sh"}, Labels: map[string]string{}},
	}
	runner.images["victim-img:1"] = engine.ImageInspect{
		ID:           "sha256:2222222222222222222222222222222222222222222222222222222222222222",
		RepoTags:     []string{"victim-img:1"},
		Created:      now,
		Architecture: runtime.GOARCH,
		Os:           "linux",
		Config:       engine.ImageConfig{Cmd: []string{"/bin/sh"}, Env: []string{"VICTIM_SECRET=topsecret"}, Labels: map[string]string{"io.gvx.extension": "ext-victim"}},
	}
	runner.images["gvx-ext/ext-victim:1.0.0-local"] = engine.ImageInspect{
		ID:           "sha256:4444444444444444444444444444444444444444444444444444444444444444",
		RepoTags:     []string{"gvx-ext/ext-victim:1.0.0-local"},
		Created:      now,
		Architecture: runtime.GOARCH,
		Os:           "linux",
		Config:       engine.ImageConfig{Cmd: []string{"/bin/sh"}, Labels: map[string]string{"io.gvx.extension": "ext-victim"}},
	}
	runner.images["atk-img:1"] = engine.ImageInspect{
		ID:           "sha256:3333333333333333333333333333333333333333333333333333333333333333",
		RepoTags:     []string{"atk-img:1"},
		Created:      now,
		Architecture: runtime.GOARCH,
		Os:           "linux",
		Config:       engine.ImageConfig{Cmd: []string{"/bin/sh"}, Labels: map[string]string{"io.gvx.extension": "ext-attacker"}},
	}

	eng := engine.New(engine.Config{
		Dir:    t.TempDir(),
		Runner: runner,
	})
	if err := eng.Open(context.Background()); err != nil {
		t.Fatalf("eng.Open: %v", err)
	}
	t.Cleanup(func() { _ = eng.Close(2 * time.Second) })

	dataDir := t.TempDir()
	gs := newTestGrantStoreWithDocker(t, dataDir, "ext-victim", nil)
	_ = gs.Approve("ext-attacker", &Manifest{
		SchemaVersion: 1,
		ID:            "ext-attacker",
		Version:       "1.0.0",
		Publisher:     "acme",
		Name:          "Attacker",
		Permissions:   []string{PermDocker},
	}, "")

	dpVictim := newScopedDockerProxy("ext-victim", dataDir, gs, []string{PermDocker}, nil, nil, func() http.Handler {
		return eng.Handler()
	})
	defer dpVictim.Close()
	dpAtk := newScopedDockerProxy("ext-attacker", dataDir, gs, []string{PermDocker}, func() bool { return false }, nil, func() http.Handler {
		return eng.Handler()
	})
	defer dpAtk.Close()
	dpAtkNet := newScopedDockerProxy("ext-attacker", dataDir, gs, []string{PermDocker, PermNetwork}, func() bool { return true }, nil, func() http.Handler {
		return eng.Handler()
	})
	defer dpAtkNet.Close()

	// Victim creates an owned volume.
	if code, resp := doScopedDockerReq(t, dpVictim, http.MethodPost, "/volumes/create", []byte(`{"Name":"victim-vol"}`)); code != http.StatusCreated {
		t.Fatalf("victim create vol = %d (%s)", code, resp)
	}

	// Gap 1: Attacker cannot mount victim-vol, user-sandbox-vol, gvx-buildkit, implicit unowned volumes, or unknown mount types.
	for _, tc := range []struct {
		name string
		body string
	}{
		{"binds-victim-vol", `{"Image":"alpine:3.20","HostConfig":{"Binds":["victim-vol:/mnt"]}}`},
		{"binds-user-vol", `{"Image":"alpine:3.20","HostConfig":{"Binds":["user-sandbox-vol:/mnt"]}}`},
		{"binds-buildkit", `{"Image":"alpine:3.20","HostConfig":{"Binds":["gvx-buildkit:/mnt"]}}`},
		{"binds-implicit-new", `{"Image":"alpine:3.20","HostConfig":{"Binds":["stealth-new-vol:/mnt"]}}`},
		{"binds-no-colon", `{"Image":"alpine:3.20","HostConfig":{"Binds":["victim-vol"]}}`},
		{"mounts-victim-vol", `{"Image":"alpine:3.20","HostConfig":{"Mounts":[{"Type":"volume","Source":"victim-vol","Target":"/mnt"}]}}`},
		{"mounts-user-vol", `{"Image":"alpine:3.20","HostConfig":{"Mounts":[{"Type":"VOLUME","Source":"user-sandbox-vol","Target":"/mnt"}]}}`},
		{"mounts-buildkit", `{"Image":"alpine:3.20","HostConfig":{"Mounts":[{"Type":"volume","Source":"gvx-buildkit","Target":"/mnt"}]}}`},
		{"mounts-implicit-new", `{"Image":"alpine:3.20","HostConfig":{"Mounts":[{"Type":"volume","Source":"stealth-mount-vol","Target":"/mnt"}]}}`},
		{"mounts-anon-empty", `{"Image":"alpine:3.20","HostConfig":{"Mounts":[{"Type":"volume","Source":"","Target":"/mnt"}]}}`},
		{"mounts-empty-type-victim", `{"Image":"alpine:3.20","HostConfig":{"Mounts":[{"Source":"victim-vol","Target":"/mnt"}]}}`},
		{"mounts-unknown-type", `{"Image":"alpine:3.20","HostConfig":{"Mounts":[{"Type":"nfs","Source":"victim-vol","Target":"/mnt"}]}}`},
	} {
		code, resp := doScopedDockerCreate(t, dpAtk, "/containers/create", []byte(tc.body))
		if code != http.StatusForbidden {
			t.Fatalf("Gap1 %s: expected 403, got %d (%s)", tc.name, code, resp)
		}
	}
	// Implicit volumes must NOT have been created on the engine.
	for _, implicit := range []string{"stealth-new-vol", "stealth-mount-vol"} {
		req := httptest.NewRequest(http.MethodGet, "/volumes/"+implicit, nil)
		rec := httptest.NewRecorder()
		eng.Handler().ServeHTTP(rec, req)
		if rec.Code != http.StatusNotFound {
			t.Fatalf("implicit volume %q should not exist, got status %d (%s)", implicit, rec.Code, rec.Body.String())
		}
	}
	// gvx-buildkit volume creation is also forbidden directly.
	if code, resp := doScopedDockerReq(t, dpAtk, http.MethodPost, "/volumes/create", []byte(`{"Name":"gvx-buildkit"}`)); code != http.StatusForbidden {
		t.Fatalf("POST /volumes/create gvx-buildkit: expected 403, got %d (%s)", code, resp)
	}
	// Legitimate flow: create owned volume first, then mount via Binds & Mounts.
	if code, resp := doScopedDockerReq(t, dpAtk, http.MethodPost, "/volumes/create", []byte(`{"Name":"atk-vol"}`)); code != http.StatusCreated {
		t.Fatalf("attacker create owned vol = %d (%s)", code, resp)
	}
	if code, resp := doScopedDockerCreate(t, dpAtk, "/containers/create", []byte(`{"Image":"alpine:3.20","HostConfig":{"Binds":["atk-vol:/a"],"Mounts":[{"Type":"volume","Source":"atk-vol","Target":"/b"}]}}`)); code != http.StatusCreated {
		t.Fatalf("attacker create with owned volume = %d (%s)", code, resp)
	}

	// Gap 3 + Router completeness: /build, /session, /grpc, /system/*, /swarm blocked with 403.
	for _, tc := range []struct {
		method string
		path   string
	}{
		{http.MethodPost, "/build?t=victim-img:1"},
		{http.MethodPost, "/v1.52/build?t=victim-img:1"},
		{http.MethodPost, "/build/prune"},
		{http.MethodPost, "/build/cancel"},
		{http.MethodPost, "/session"},
		{http.MethodPost, "/grpc"},
		{http.MethodGet, "/system/df"},
		{http.MethodPost, "/system/prune"},
		{http.MethodGet, "/swarm"},
	} {
		code, resp := doScopedDockerReq(t, dpAtk, tc.method, tc.path, nil)
		if code != http.StatusForbidden {
			t.Fatalf("Gap3 %s %s: expected 403, got %d (%s)", tc.method, tc.path, code, resp)
		}
	}

	// Gap 4: Source image isolation on list, inspect, history, export, tag, push, and fromImage pull.
	if code, resp := doScopedDockerReq(t, dpAtk, http.MethodGet, "/images/json", nil); code != http.StatusOK || strings.Contains(resp, "victim-img") || !strings.Contains(resp, "alpine:3.20") || !strings.Contains(resp, "atk-img:1") {
		t.Fatalf("GET /images/json isolation failed: code=%d resp=%s", code, resp)
	}
	for _, tc := range []struct {
		method string
		path   string
		want   int
	}{
		{http.MethodGet, "/images/victim-img:1/json", http.StatusForbidden},
		{http.MethodGet, "/images/victim-img:1/history", http.StatusForbidden},
		{http.MethodGet, "/images/victim-img:1/get", http.StatusForbidden},
		{http.MethodGet, "/images/get?names=victim-img:1", http.StatusForbidden},
		{http.MethodGet, "/images/get?names=alpine:3.20&names=victim-img:1", http.StatusForbidden},
		{http.MethodPost, "/images/victim-img:1/tag?repo=atk-stolen&tag=1", http.StatusForbidden},
		{http.MethodPost, "/images/victim-img:1/push", http.StatusForbidden},
		{http.MethodPost, "/images/alpine:3.20/tag?repo=victim-img&tag=1", http.StatusForbidden},
		{http.MethodPost, "/images/create?fromImage=victim-img&tag=1", http.StatusForbidden},
		{http.MethodPost, "/images/create?fromImage=victim-img:1", http.StatusForbidden},
		{http.MethodPost, "/images/create?fromImage=../../etc/passwd", http.StatusForbidden},
		{http.MethodGet, "/images/does-not-exist:1/json", http.StatusNotFound},
		// Shared unlabeled base image & owned image remain accessible.
		{http.MethodGet, "/images/alpine:3.20/json", http.StatusOK},
		{http.MethodGet, "/images/alpine:3.20/history", http.StatusOK},
		{http.MethodGet, "/images/alpine:3.20/get", http.StatusOK},
		{http.MethodGet, "/images/get?names=alpine:3.20", http.StatusOK},
		{http.MethodPost, "/images/alpine:3.20/tag?repo=atk-from-base&tag=1", http.StatusCreated},
		{http.MethodPost, "/images/create?fromImage=alpine&tag=3.20", http.StatusForbidden},
		{http.MethodGet, "/images/search?term=alpine", http.StatusForbidden},
		{http.MethodGet, "/images/atk-img:1/json", http.StatusOK},
		{http.MethodGet, "/images/atk-img:1/history", http.StatusOK},
		{http.MethodGet, "/images/atk-img:1/get", http.StatusOK},
		{http.MethodPost, "/images/atk-img:1/tag?repo=atk-alias&tag=1", http.StatusCreated},
		{http.MethodPost, "/images/atk-img:1/push", http.StatusForbidden},
	} {
		code, resp := doScopedDockerReq(t, dpAtk, tc.method, tc.path, nil)
		if code != tc.want {
			t.Fatalf("Gap4 %s %s: expected %d, got %d (%s)", tc.method, tc.path, tc.want, code, resp)
		}
	}
	for _, tc := range []struct {
		method string
		path   string
		want   int
	}{
		{http.MethodPost, "/images/create?fromImage=victim-img&tag=1", http.StatusForbidden},
		{http.MethodPost, "/images/create?fromImage=victim-img:1@sha256:2222222222222222222222222222222222222222222222222222222222222222", http.StatusForbidden},
		{http.MethodPost, "/images/create?fromImage=../../etc/passwd", http.StatusBadRequest},
		{http.MethodPost, "/images/victim-img:1/push", http.StatusForbidden},
		{http.MethodPost, "/images/create?fromImage=alpine&tag=3.20", http.StatusOK},
		{http.MethodPost, "/images/atk-img:1/push", http.StatusOK},
	} {
		code, resp := doScopedDockerReq(t, dpAtkNet, tc.method, tc.path, nil)
		if code != tc.want {
			t.Fatalf("Gap4(net) %s %s: expected %d, got %d (%s)", tc.method, tc.path, tc.want, code, resp)
		}
	}

	// Gap 4b (R5 Finding 1): POST /containers/create rejects foreign images by tag, sha256 ID, gvx-ext/<victim>,
	// lowercase/SimpleFold Image key, and duplicate platform params, while allowing alpine:3.20, atk-img:1, and returning 404 for uncached images.
	for _, tc := range []struct {
		name string
		path string
		body string
		want int
	}{
		{"victim-tag", "/containers/create", `{"Image":"victim-img:1"}`, http.StatusForbidden},
		{"victim-lower-key", "/containers/create", `{"image":"victim-img:1"}`, http.StatusForbidden},
		{"victim-sha256", "/containers/create", `{"Image":"sha256:2222222222222222222222222222222222222222222222222222222222222222"}`, http.StatusForbidden},
		{"victim-gvx-ext", "/containers/create", `{"Image":"gvx-ext/ext-victim:1.0.0-local"}`, http.StatusForbidden},
		{"dup-platform", "/containers/create?platform=linux/arm64&platform=linux/amd64", `{"Image":"alpine:3.20"}`, http.StatusBadRequest},
		{"uncached-404", "/containers/create", `{"Image":"does-not-exist:1"}`, http.StatusNotFound},
		{"shared-alpine", "/containers/create", `{"Image":"alpine:3.20"}`, http.StatusCreated},
		{"owned-atk", "/containers/create", `{"Image":"atk-img:1"}`, http.StatusCreated},
	} {
		code, resp := doScopedDockerCreate(t, dpAtk, tc.path, []byte(tc.body))
		if code != tc.want {
			t.Fatalf("Gap4b %s: expected %d, got %d (%s)", tc.name, tc.want, code, resp)
		}
	}
}

type fakeMultiArchEngineRunner struct {
	*fakeEngineRunner
	foreignArch   string
	foreignImages map[string]engine.ImageInspect
	tagCalls      []string
}

func newFakeMultiArchEngineRunner() *fakeMultiArchEngineRunner {
	foreign := "amd64"
	if runtime.GOARCH == "amd64" {
		foreign = "arm64"
	}
	return &fakeMultiArchEngineRunner{
		fakeEngineRunner: newFakeEngineRunner(),
		foreignArch:      foreign,
		foreignImages:    make(map[string]engine.ImageInspect),
	}
}

func (f *fakeMultiArchEngineRunner) mapForArch(arch string) map[string]engine.ImageInspect {
	if arch == f.foreignArch {
		return f.foreignImages
	}
	return f.images
}

func (f *fakeMultiArchEngineRunner) ImagesArch(_ context.Context, arch string) ([]rungv.Image, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	m := f.mapForArch(arch)
	var out []rungv.Image
	seen := map[string]bool{}
	for _, img := range m {
		if seen[img.ID] {
			continue
		}
		seen[img.ID] = true
		for _, rt := range img.RepoTags {
			repo, tag, _ := strings.Cut(rt, ":")
			out = append(out, rungv.Image{
				ID:         img.ID,
				Repository: repo,
				Tag:        tag,
				CreatedAt:  img.Created.UTC().Format(time.RFC3339),
			})
		}
	}
	return out, nil
}

func (f *fakeMultiArchEngineRunner) InspectImagesArch(_ context.Context, arch string, refs ...string) ([]engine.ImageInspect, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	m := f.mapForArch(arch)
	var out []engine.ImageInspect
	for _, ref := range refs {
		var found *engine.ImageInspect
		if img, ok := m[ref]; ok {
			cp := img
			found = &cp
		} else {
			for _, img := range m {
				if img.ID == ref {
					cp := img
					found = &cp
					break
				}
				for _, rt := range img.RepoTags {
					if rt == ref || rt == ref+":latest" {
						cp := img
						found = &cp
						break
					}
				}
				if found != nil {
					break
				}
			}
		}
		if found == nil {
			return nil, fmt.Errorf("No such image: %s", ref)
		}
		out = append(out, *found)
	}
	return out, nil
}

func (f *fakeMultiArchEngineRunner) RemoveImageArch(_ context.Context, arch, ref string, _ bool) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	m := f.mapForArch(arch)
	if _, ok := m[ref]; !ok {
		return fmt.Errorf("No such image: %s", ref)
	}
	delete(m, ref)
	return nil
}

func (f *fakeMultiArchEngineRunner) TagImageArch(_ context.Context, arch, source, target string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.tagCalls = append(f.tagCalls, fmt.Sprintf("tag:%s:%s->%s", arch, source, target))
	m := f.mapForArch(arch)
	img, ok := m[source]
	if !ok {
		for _, cand := range m {
			if cand.ID == source {
				img = cand
				ok = true
				break
			}
		}
	}
	if !ok {
		return fmt.Errorf("No such image: %s", source)
	}
	img.RepoTags = append(append([]string(nil), img.RepoTags...), target)
	m[target] = img
	return nil
}

func (f *fakeMultiArchEngineRunner) LoadImagesArch(context.Context, string, io.Reader, func(string)) error {
	return nil
}

func (f *fakeMultiArchEngineRunner) SaveImagesArch(ctx context.Context, _ string, w io.Writer, refs ...string) error {
	return f.SaveImages(ctx, w, refs...)
}

func (f *fakeMultiArchEngineRunner) PruneImagesArch(context.Context, string) (string, error) {
	return "", nil
}

func TestDockerScope_SSRFNetworkPermCrossArchTagReservedNSAndDigest(t *testing.T) {
	runner := newFakeMultiArchEngineRunner()
	now := time.Now().UTC()

	// Host arch images.
	runner.images["alpine:3.20"] = engine.ImageInspect{
		ID:           "sha256:1111111111111111111111111111111111111111111111111111111111111111",
		RepoTags:     []string{"alpine:3.20"},
		Created:      now,
		Architecture: runtime.GOARCH,
		Os:           "linux",
		Config:       engine.ImageConfig{Cmd: []string{"/bin/sh"}, Labels: map[string]string{}},
	}
	runner.images["victim-img:1"] = engine.ImageInspect{
		ID:           "sha256:2222222222222222222222222222222222222222222222222222222222222222",
		RepoTags:     []string{"victim-img:1"},
		Created:      now,
		Architecture: runtime.GOARCH,
		Os:           "linux",
		Config:       engine.ImageConfig{Cmd: []string{"/bin/sh"}, Labels: map[string]string{"io.gvx.extension": "ext-victim"}},
	}
	runner.images["atk-img:1"] = engine.ImageInspect{
		ID:           "sha256:3333333333333333333333333333333333333333333333333333333333333333",
		RepoTags:     []string{"atk-img:1"},
		Created:      now,
		Architecture: runtime.GOARCH,
		Os:           "linux",
		Config:       engine.ImageConfig{Cmd: []string{"/bin/sh"}, Labels: map[string]string{"io.gvx.extension": "ext-attacker"}},
	}
	runner.images["shared:1"] = engine.ImageInspect{
		ID:           "sha256:aaaa000000000000000000000000000000000000000000000000000000000001",
		RepoTags:     []string{"shared:1"},
		Created:      now,
		Architecture: runtime.GOARCH,
		Os:           "linux",
		Config:       engine.ImageConfig{Cmd: []string{"/bin/sh"}, Labels: map[string]string{"io.gvx.extension": "ext-attacker"}},
	}

	// Foreign arch images: atk-foreign:1 owned by ext-attacker, shared:1 owned by ext-victim.
	runner.foreignImages["atk-foreign:1"] = engine.ImageInspect{
		ID:           "sha256:bbbb000000000000000000000000000000000000000000000000000000000001",
		RepoTags:     []string{"atk-foreign:1"},
		Created:      now,
		Architecture: runner.foreignArch,
		Os:           "linux",
		Config:       engine.ImageConfig{Cmd: []string{"/bin/sh"}, Labels: map[string]string{"io.gvx.extension": "ext-attacker"}},
	}
	runner.foreignImages["shared:1"] = engine.ImageInspect{
		ID:           "sha256:bbbb000000000000000000000000000000000000000000000000000000000002",
		RepoTags:     []string{"shared:1"},
		Created:      now,
		Architecture: runner.foreignArch,
		Os:           "linux",
		Config:       engine.ImageConfig{Cmd: []string{"/bin/sh"}, Env: []string{"SECRET=foreign-victim"}, Labels: map[string]string{"io.gvx.extension": "ext-victim"}},
	}

	eng := engine.New(engine.Config{
		Dir:    t.TempDir(),
		Runner: runner,
	})
	if err := eng.Open(context.Background()); err != nil {
		t.Fatalf("eng.Open: %v", err)
	}
	t.Cleanup(func() { _ = eng.Close(2 * time.Second) })

	dataDir := t.TempDir()
	gs := newTestGrantStoreWithDocker(t, dataDir, "ext-victim", nil)
	_ = gs.Approve("ext-attacker", &Manifest{
		SchemaVersion: 1,
		ID:            "ext-attacker",
		Version:       "1.0.0",
		Publisher:     "acme",
		Name:          "Attacker",
		Permissions:   []string{PermDocker},
	}, "")

	dpNoNet := newScopedDockerProxy("ext-attacker", dataDir, gs, []string{PermDocker}, func() bool { return false }, nil, func() http.Handler {
		return eng.Handler()
	})
	defer dpNoNet.Close()

	dpWithNet := newScopedDockerProxy("ext-attacker", dataDir, gs, []string{PermDocker, PermNetwork}, func() bool { return true }, nil, func() http.Handler {
		return eng.Handler()
	})
	defer dpWithNet.Close()

	// Build a minimal valid tar archive for fromSrc=- tests.
	var tarBuf bytes.Buffer
	tw := tar.NewWriter(&tarBuf)
	_ = tw.WriteHeader(&tar.Header{Name: "hello.txt", Mode: 0o644, Size: 2})
	_, _ = tw.Write([]byte("hi"))
	_ = tw.Close()
	validTar := tarBuf.Bytes()

	// Gap 1: fromSrc SSRF blocked (hitCount == 0) both without and with PermNetwork;
	// registry endpoints gated on PermNetwork.
	var ssrfHits atomic.Int32
	ssrfSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ssrfHits.Add(1)
		w.Header().Set("Content-Type", "application/x-tar")
		_, _ = w.Write(validTar)
	}))
	defer ssrfSrv.Close()

	ssrfPath := "/images/create?fromSrc=" + url.QueryEscape(ssrfSrv.URL+"/internal-secret") + "&repo=atk-ssrf&tag=1"
	for name, dp := range map[string]*scopedDockerProxy{"noNet": dpNoNet, "withNet": dpWithNet} {
		code, resp := doScopedDockerReq(t, dp, http.MethodPost, ssrfPath, nil)
		if code != http.StatusForbidden {
			t.Fatalf("Gap1 SSRF (%s): expected 403, got %d (%s)", name, code, resp)
		}
	}
	if ssrfHits.Load() != 0 {
		t.Fatalf("Gap1 SSRF: expected 0 hits to internal server, got %d", ssrfHits.Load())
	}

	// Without PermNetwork -> 403 on fromImage, search, push; fromSrc=- succeeds (local tar stream).
	for _, tc := range []struct {
		method string
		path   string
		body   []byte
		want   int
	}{
		{http.MethodPost, "/images/create?fromImage=alpine&tag=3.20", nil, http.StatusForbidden},
		{http.MethodGet, "/images/search?term=alpine", nil, http.StatusForbidden},
		{http.MethodPost, "/images/atk-img:1/push", nil, http.StatusForbidden},
		{http.MethodPost, "/images/create?fromSrc=-&repo=atk-local-import&tag=1", validTar, http.StatusOK},
	} {
		code, resp := doScopedDockerReq(t, dpNoNet, tc.method, tc.path, tc.body)
		if code != tc.want {
			t.Fatalf("Gap1 noNet %s %s: expected %d, got %d (%s)", tc.method, tc.path, tc.want, code, resp)
		}
	}
	// With PermNetwork -> 200 on fromImage, search, push.
	for _, tc := range []struct {
		method string
		path   string
		want   int
	}{
		{http.MethodPost, "/images/create?fromImage=alpine&tag=3.20", http.StatusOK},
		{http.MethodGet, "/images/search?term=alpine", http.StatusOK},
		{http.MethodPost, "/images/atk-img:1/push", http.StatusOK},
	} {
		code, resp := doScopedDockerReq(t, dpWithNet, tc.method, tc.path, nil)
		if code != tc.want {
			t.Fatalf("Gap1 withNet %s %s: expected %d, got %d (%s)", tc.method, tc.path, tc.want, code, resp)
		}
	}

	// Gap 2: Cross-arch tag overwrite blocked against real multi-arch ImageArchRunner.
	for _, tc := range []struct {
		name string
		path string
		want int
	}{
		{"tag-foreign-by-name", "/images/atk-foreign:1/tag?repo=shared&tag=1", http.StatusForbidden},
		{"tag-foreign-by-digest", "/images/sha256:bbbb000000000000000000000000000000000000000000000000000000000001/tag?repo=shared&tag=1", http.StatusForbidden},
		{"tag-foreign-own-new", "/images/atk-foreign:1/tag?repo=atk-foreign-alias&tag=1", http.StatusCreated},
	} {
		code, resp := doScopedDockerReq(t, dpNoNet, http.MethodPost, tc.path, nil)
		if code != tc.want {
			t.Fatalf("Gap2 %s: expected %d, got %d (%s)", tc.name, tc.want, code, resp)
		}
	}
	runner.mu.Lock()
	foreignShared := runner.foreignImages["shared:1"]
	runner.mu.Unlock()
	if foreignShared.Config.Labels["io.gvx.extension"] != "ext-victim" || foreignShared.ID != "sha256:bbbb000000000000000000000000000000000000000000000000000000000002" {
		t.Fatalf("Gap2: foreign shared:1 was mutated: %+v", foreignShared)
	}

	// Gap 2b: POST /commit cross-arch protection + Gap 3 /commit into reserved namespace.
	commitBackend := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := stripDockerVersion(cleanDockerMatchPath(r.URL.Path))
		switch {
		case r.Method == http.MethodGet && p == "/containers/c-foreign-arch/json":
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"Id":     "c-foreign-arch",
				"Image":  "sha256:bbbb000000000000000000000000000000000000000000000000000000000001",
				"Config": map[string]any{"Labels": map[string]string{"io.gvx.extension": "ext-attacker"}},
			})
		case r.Method == http.MethodGet && strings.HasPrefix(p, "/images/") && strings.HasSuffix(p, "/json"):
			eng.Handler().ServeHTTP(w, r)
		case r.Method == http.MethodPost && p == "/commit":
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusCreated)
			_, _ = io.WriteString(w, `{"Id":"sha256:committed"}`)
		default:
			eng.Handler().ServeHTTP(w, r)
		}
	})
	dpCommit := newScopedDockerProxy("ext-attacker", dataDir, gs, []string{PermDocker, PermNetwork}, func() bool { return true }, nil, func() http.Handler {
		return commitBackend
	})
	defer dpCommit.Close()

	if code, resp := doScopedDockerReq(t, dpCommit, http.MethodPost, "/commit?container=c-foreign-arch&repo=shared&tag=1", nil); code != http.StatusForbidden {
		t.Fatalf("Gap2b cross-arch commit overwrite expected 403, got %d (%s)", code, resp)
	}
	if code, resp := doScopedDockerReq(t, dpCommit, http.MethodPost, "/commit?container=c-foreign-arch&repo=gvx-ext/ext-victim&tag=1.0.0", nil); code != http.StatusForbidden {
		t.Fatalf("Gap3 commit reserved namespace expected 403, got %d (%s)", code, resp)
	}
	if code, resp := doScopedDockerReq(t, dpCommit, http.MethodPost, "/commit?container=c-foreign-arch&repo=atk-foreign-commit&tag=1", nil); code != http.StatusCreated {
		t.Fatalf("Gap2b owned foreign-arch commit expected 201, got %d (%s)", code, resp)
	}

	// Gap 3: Reserved gvx-ext/<victim> namespace poisoning blocked even when not on disk.
	for _, tc := range []struct {
		name   string
		method string
		path   string
		body   []byte
		want   int
	}{
		{"tag-gvx-ext-victim", http.MethodPost, "/images/atk-img:1/tag?repo=gvx-ext/ext-victim&tag=9.9.9", nil, http.StatusForbidden},
		{"tag-dockerio-gvx-ext-victim", http.MethodPost, "/images/atk-img:1/tag?repo=docker.io/gvx-ext/ext-victim&tag=1.0.0", nil, http.StatusForbidden},
		{"import-gvx-ext-victim", http.MethodPost, "/images/create?fromSrc=-&repo=gvx-ext/ext-victim&tag=1.0.0", validTar, http.StatusForbidden},
		{"pull-gvx-ext-victim", http.MethodPost, "/images/create?fromImage=gvx-ext/ext-victim&tag=1.0.0", nil, http.StatusForbidden},
		{"pull-dockerio-gvx-ext-victim", http.MethodPost, "/images/create?fromImage=docker.io/gvx-ext/ext-victim:1.0.0", nil, http.StatusForbidden},
		{"create-gvx-ext-victim", http.MethodPost, "/containers/create", []byte(`{"Image":"gvx-ext/ext-victim:9.9.9"}`), http.StatusForbidden},
		{"create-dockerio-gvx-ext-victim", http.MethodPost, "/containers/create", []byte(`{"Image":"docker.io/gvx-ext/ext-victim:9.9.9"}`), http.StatusForbidden},
		// Own namespace remains allowed.
		{"tag-own-gvx-ext", http.MethodPost, "/images/atk-img:1/tag?repo=gvx-ext/ext-attacker&tag=9.9.9", nil, http.StatusCreated},
	} {
		code, resp := doScopedDockerReq(t, dpWithNet, tc.method, tc.path, tc.body)
		if code != tc.want {
			t.Fatalf("Gap3 %s: expected %d, got %d (%s)", tc.name, tc.want, code, resp)
		}
	}

	// Gap 4: Digest-qualified ref normalization (victim-img:1@sha256:... and alpine:3.20@sha256:<victim-digest>).
	for _, tc := range []struct {
		name   string
		method string
		path   string
		body   []byte
		want   int
	}{
		{"pull-victim-exact-digest", http.MethodPost, "/images/create?fromImage=victim-img:1@sha256:2222222222222222222222222222222222222222222222222222222222222222", nil, http.StatusForbidden},
		{"pull-victim-other-digest", http.MethodPost, "/images/create?fromImage=victim-img:1@sha256:9999999999999999999999999999999999999999999999999999999999999999", nil, http.StatusForbidden},
		{"pull-alpine-victim-digest", http.MethodPost, "/images/create?fromImage=alpine:3.20@sha256:2222222222222222222222222222222222222222222222222222222222222222", nil, http.StatusForbidden},
		{"inspect-victim-digest-ref", http.MethodGet, "/images/victim-img:1@sha256:2222222222222222222222222222222222222222222222222222222222222222/json", nil, http.StatusForbidden},
		{"create-victim-exact-digest", http.MethodPost, "/containers/create", []byte(`{"Image":"victim-img:1@sha256:2222222222222222222222222222222222222222222222222222222222222222"}`), http.StatusForbidden},
		{"create-victim-other-digest", http.MethodPost, "/containers/create", []byte(`{"Image":"victim-img:1@sha256:9999999999999999999999999999999999999999999999999999999999999999"}`), http.StatusForbidden},
		{"create-alpine-victim-digest", http.MethodPost, "/containers/create", []byte(`{"Image":"alpine:3.20@sha256:2222222222222222222222222222222222222222222222222222222222222222"}`), http.StatusForbidden},
	} {
		code, resp := doScopedDockerReq(t, dpWithNet, tc.method, tc.path, tc.body)
		if code != tc.want {
			t.Fatalf("Gap4 %s: expected %d, got %d (%s)", tc.name, tc.want, code, resp)
		}
	}
}

func TestDockerScope_CommitContinuationOnbuildAndLabelAppend(t *testing.T) {
	dataDir := t.TempDir()
	gs := newTestGrantStoreWithDocker(t, dataDir, "ext-a", nil)

	var (
		mu             sync.Mutex
		commitHits     int
		lastChanges    []string
		lastBodyLabels map[string]any
	)
	backend := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := stripDockerVersion(cleanDockerMatchPath(r.URL.Path))
		switch {
		case r.Method == http.MethodGet && p == "/containers/c-owned/json":
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"Id":     "c-owned",
				"Image":  "alpine:3.20",
				"Config": map[string]any{"Labels": map[string]string{"io.gvx.extension": "ext-a"}},
			})
		case r.Method == http.MethodGet && p == "/images/alpine:3.20/json":
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"Id":     "sha256:alpine",
				"Config": map[string]any{"Labels": map[string]string{}},
			})
		case r.Method == http.MethodPost && p == "/commit":
			raw, _ := io.ReadAll(r.Body)
			var bodyObj map[string]any
			if len(bytes.TrimSpace(raw)) > 0 {
				_ = json.Unmarshal(raw, &bodyObj)
			}
			mu.Lock()
			commitHits++
			lastChanges = append([]string(nil), r.URL.Query()["changes"]...)
			if lbls, ok := bodyObj["Labels"].(map[string]any); ok {
				lastBodyLabels = lbls
			} else {
				lastBodyLabels = nil
			}
			mu.Unlock()
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"Id":      "sha256:committed",
				"changes": r.URL.Query()["changes"],
			})
		case r.Method == http.MethodPost && p == "/images/create":
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(w, `{"status":"sha256:imported"}`+"\n")
		default:
			w.WriteHeader(http.StatusOK)
		}
	})

	dp := newScopedDockerProxy("ext-a", dataDir, gs, []string{PermDocker}, func() bool { return false }, nil, func() http.Handler {
		return backend
	})
	defer dp.Close()

	var tarBuf bytes.Buffer
	tw := tar.NewWriter(&tarBuf)
	_ = tw.WriteHeader(&tar.Header{Name: "hello.txt", Mode: 0o644, Size: 2})
	_, _ = tw.Write([]byte("hi"))
	_ = tw.Close()
	validTar := tarBuf.Bytes()

	for _, tc := range []struct {
		name    string
		changes []string
		want    int
	}{
		{"backslash_label_continuation", []string{"LABEL a=b \\\nio.gvx.extension=ext-victim"}, http.StatusForbidden},
		{"backslash_label_crlf", []string{"LABEL a=b \\\r\nio.gvx.extension=ext-victim"}, http.StatusForbidden},
		{"backslash_label_comment_inside", []string{"LABEL a=b \\\n# comment\nio.gvx.extension=ext-victim"}, http.StatusForbidden},
		{"backslash_split_keyword", []string{"LAB\\\nEL io.gvx.extension=ext-victim"}, http.StatusForbidden},
		{"multi_changes_continuation", []string{"LABEL a=b \\", "io.gvx.extension=ext-victim"}, http.StatusForbidden},
		{"onbuild_label", []string{"ONBUILD LABEL io.gvx.extension=ext-victim"}, http.StatusForbidden},
		{"onbuild_tab_label", []string{"onbuild\tLABEL a=1 io.gvx.extension=ext-victim"}, http.StatusForbidden},
		{"onbuild_backslash_label", []string{"ONBUILD \\\nLABEL io.gvx.extension=ext-victim"}, http.StatusForbidden},
		{"escape_directive_backtick", []string{"# escape=`\nLABEL a=b `\nio.gvx.extension=ext-victim"}, http.StatusForbidden},
		{"backslash_env_then_label", []string{"ENV A=1 \\\nLABEL io.gvx.extension=ext-victim"}, http.StatusForbidden},
		{"dangling_continuation", []string{"LABEL a=b \\"}, http.StatusBadRequest},
		{"empty_onbuild", []string{"ONBUILD"}, http.StatusBadRequest},
	} {
		q := url.Values{}
		q.Set("container", "c-owned")
		for _, ch := range tc.changes {
			q.Add("changes", ch)
		}
		code, resp := doScopedDockerReq(t, dp, http.MethodPost, "/commit?"+q.Encode(), nil)
		if code != tc.want {
			t.Fatalf("POST /commit %s: expected %d, got %d (%s)", tc.name, tc.want, code, resp)
		}

		// Also verify POST /images/create?fromSrc=- rejects the same changes payloads.
		iq := url.Values{}
		iq.Set("fromSrc", "-")
		iq.Set("repo", "ext-a-img")
		iq.Set("tag", "1")
		for _, ch := range tc.changes {
			iq.Add("changes", ch)
		}
		icode, iresp := doScopedDockerReq(t, dp, http.MethodPost, "/images/create?"+iq.Encode(), validTar)
		if icode != tc.want {
			t.Fatalf("POST /images/create %s: expected %d, got %d (%s)", tc.name, tc.want, icode, iresp)
		}
	}

	mu.Lock()
	hitsBeforeValid := commitHits
	mu.Unlock()
	if hitsBeforeValid != 0 {
		t.Fatalf("expected 0 backend /commit hits for rejected payloads, got %d", hitsBeforeValid)
	}

	// Valid commit without body must append LABEL io.gvx.extension=ext-a to changes.
	code, resp := doScopedDockerReq(t, dp, http.MethodPost, "/commit?container=c-owned&changes="+url.QueryEscape("ENV A=1"), nil)
	if code != http.StatusCreated {
		t.Fatalf("valid commit (no body): expected 201, got %d (%s)", code, resp)
	}
	mu.Lock()
	gotChanges := append([]string(nil), lastChanges...)
	mu.Unlock()
	if len(gotChanges) != 2 || gotChanges[0] != "ENV A=1" || gotChanges[1] != "LABEL io.gvx.extension=ext-a" {
		t.Fatalf("expected appended LABEL io.gvx.extension=ext-a in changes, got %#v", gotChanges)
	}

	// Valid commit with JSON body must enforce Labels["io.gvx.extension"] AND append LABEL io.gvx.extension=ext-a.
	code, resp = doScopedDockerReq(t, dp, http.MethodPost, "/commit?container=c-owned&changes="+url.QueryEscape("ONBUILD ENV B=2\nLABEL app.version=1.0"), []byte(`{"Comment":"test"}`))
	if code != http.StatusCreated {
		t.Fatalf("valid commit (with body): expected 201, got %d (%s)", code, resp)
	}
	mu.Lock()
	gotChanges = append([]string(nil), lastChanges...)
	gotLabels := lastBodyLabels
	mu.Unlock()
	if len(gotChanges) < 2 || gotChanges[len(gotChanges)-1] != "LABEL io.gvx.extension=ext-a" {
		t.Fatalf("expected trailing LABEL io.gvx.extension=ext-a in changes, got %#v", gotChanges)
	}
	if gotLabels == nil || gotLabels["io.gvx.extension"] != "ext-a" {
		t.Fatalf("expected Labels[io.gvx.extension]=ext-a in JSON body, got %#v", gotLabels)
	}
}

func TestDockerScope_BOMDirectiveAndJSONOnBuildBypasses(t *testing.T) {
	dataDir := t.TempDir()
	gs := newTestGrantStoreWithDocker(t, dataDir, "ext-a", nil)

	var (
		mu          sync.Mutex
		backendHits int
	)
	backend := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := stripDockerVersion(cleanDockerMatchPath(r.URL.Path))
		switch {
		case r.Method == http.MethodGet && p == "/containers/c-owned/json":
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"Id":     "c-owned",
				"Image":  "alpine:3.20",
				"Config": map[string]any{"Labels": map[string]string{"io.gvx.extension": "ext-a"}},
			})
		case r.Method == http.MethodGet && p == "/images/alpine:3.20/json":
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"Id":     "sha256:alpine",
				"Config": map[string]any{"Labels": map[string]string{}},
			})
		case r.Method == http.MethodPost && p == "/commit":
			mu.Lock()
			backendHits++
			mu.Unlock()
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusCreated)
			_, _ = io.WriteString(w, `{"Id":"sha256:committed"}`)
		case r.Method == http.MethodPost && p == "/containers/create":
			mu.Lock()
			backendHits++
			mu.Unlock()
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusCreated)
			_, _ = io.WriteString(w, `{"Id":"ctr-new"}`)
		case r.Method == http.MethodPost && p == "/images/create":
			mu.Lock()
			backendHits++
			mu.Unlock()
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(w, `{"status":"sha256:imported"}`+"\n")
		default:
			w.WriteHeader(http.StatusOK)
		}
	})

	dp := newScopedDockerProxy("ext-a", dataDir, gs, []string{PermDocker}, func() bool { return false }, nil, func() http.Handler {
		return backend
	})
	defer dp.Close()

	var tarBuf bytes.Buffer
	tw := tar.NewWriter(&tarBuf)
	_ = tw.WriteHeader(&tar.Header{Name: "hello.txt", Mode: 0o644, Size: 2})
	_, _ = tw.Write([]byte("hi"))
	_ = tw.Close()
	validTar := tarBuf.Bytes()

	// 1. UTF-8 BOM before # escape=` or LABEL/ONBUILD on POST /commit and POST /images/create?fromSrc=-.
	bomChanges := []struct {
		name    string
		changes []string
		want    int
	}{
		{"bom_escape_onbuild", []string{"\ufeff# escape=`\nONBUILD LABEL a=1 `\nio.gvx.extension=ext-victim"}, http.StatusForbidden},
		{"bom_space_escape_label", []string{"\ufeff  # escape=`\nLABEL a=1 `\nio.gvx.extension=ext-victim"}, http.StatusForbidden},
		{"space_bom_escape", []string{"  \ufeff# escape=`\nLABEL a=1 `\nio.gvx.extension=ext-victim"}, http.StatusForbidden},
		{"second_change_bom_escape", []string{"", "\ufeff# escape=`\nLABEL a=1 `\nio.gvx.extension=ext-victim"}, http.StatusForbidden},
		{"bom_direct_label", []string{"\ufeffLABEL io.gvx.extension=ext-victim"}, http.StatusForbidden},
		{"bom_direct_onbuild_label", []string{"\ufeffONBUILD LABEL io.gvx.extension=ext-victim"}, http.StatusForbidden},
	}
	for _, tc := range bomChanges {
		cq := url.Values{}
		cq.Set("container", "c-owned")
		for _, ch := range tc.changes {
			cq.Add("changes", ch)
		}
		if code, resp := doScopedDockerReq(t, dp, http.MethodPost, "/commit?"+cq.Encode(), nil); code != tc.want {
			t.Fatalf("POST /commit %s: expected %d, got %d (%s)", tc.name, tc.want, code, resp)
		}

		iq := url.Values{}
		iq.Set("fromSrc", "-")
		iq.Set("repo", "ext-a-img")
		iq.Set("tag", "1")
		for _, ch := range tc.changes {
			iq.Add("changes", ch)
		}
		if code, resp := doScopedDockerReq(t, dp, http.MethodPost, "/images/create?"+iq.Encode(), validTar); code != tc.want {
			t.Fatalf("POST /images/create %s: expected %d, got %d (%s)", tc.name, tc.want, code, resp)
		}
	}

	// 2. JSON OnBuild enforcement on both POST /commit and POST /containers/create.
	onBuildRejected := []struct {
		name string
		body string
		want int
	}{
		{"canonical_onbuild_label", `{"Image":"alpine:3.20","OnBuild":["LABEL io.gvx.extension=ext-victim"]}`, http.StatusForbidden},
		{"lowercase_onbuild", `{"Image":"alpine:3.20","onbuild":["LABEL io.gvx.extension=ext-victim"]}`, http.StatusForbidden},
		{"lowercase_label_cmd", `{"Image":"alpine:3.20","OnBuild":["label io.gvx.extension=ext-victim"]}`, http.StatusForbidden},
		{"nested_onbuild_cmd", `{"Image":"alpine:3.20","OnBuild":["ONBUILD LABEL io.gvx.extension=ext-victim"]}`, http.StatusForbidden},
		{"quoted_onbuild_label", `{"Image":"alpine:3.20","OnBuild":["LABEL \"io.gvx.extension\"=\"ext-victim\""]}`, http.StatusForbidden},
		{"bom_escape_inside_onbuild", `{"Image":"alpine:3.20","OnBuild":["\ufeff# escape=` + "`" + `\nLABEL a=1 ` + "`" + `\nio.gvx.extension=ext-victim"]}`, http.StatusForbidden},
		{"split_continuation_onbuild", `{"Image":"alpine:3.20","OnBuild":["LABEL a=1 \\","io.gvx.extension=ext-victim"]}`, http.StatusForbidden},
		{"dup_case_colliding_onbuild", `{"Image":"alpine:3.20","OnBuild":[],"onbuild":["LABEL io.gvx.extension=ext-victim"]}`, http.StatusBadRequest},
		{"onbuild_string_type", `{"Image":"alpine:3.20","OnBuild":"LABEL io.gvx.extension=ext-victim"}`, http.StatusBadRequest},
		{"onbuild_number_type", `{"Image":"alpine:3.20","OnBuild":123}`, http.StatusBadRequest},
		{"onbuild_non_string_element", `{"Image":"alpine:3.20","OnBuild":["ENV A=1",123]}`, http.StatusBadRequest},
		{"onbuild_null_element", `{"Image":"alpine:3.20","OnBuild":[null]}`, http.StatusBadRequest},
	}
	for _, tc := range onBuildRejected {
		if code, resp := doScopedDockerReq(t, dp, http.MethodPost, "/commit?container=c-owned", []byte(tc.body)); code != tc.want {
			t.Fatalf("POST /commit %s: expected %d, got %d (%s)", tc.name, tc.want, code, resp)
		}
		if code, resp := doScopedDockerCreate(t, dp, "/containers/create", []byte(tc.body)); code != tc.want {
			t.Fatalf("POST /containers/create %s: expected %d, got %d (%s)", tc.name, tc.want, code, resp)
		}
	}

	mu.Lock()
	hitsBeforeValid := backendHits
	mu.Unlock()
	if hitsBeforeValid != 0 {
		t.Fatalf("expected 0 backend hits for rejected BOM/OnBuild payloads, got %d", hitsBeforeValid)
	}

	// 3. Valid OnBuild payloads (null, empty array, safe instructions) succeed.
	for _, tc := range []struct {
		name string
		body string
	}{
		{"onbuild_null", `{"Image":"alpine:3.20","OnBuild":null}`},
		{"onbuild_empty", `{"Image":"alpine:3.20","onbuild":[]}`},
		{"onbuild_safe", `{"Image":"alpine:3.20","onbuild":["ENV APP_ENV=prod","LABEL com.example.tier=web"]}`},
	} {
		if code, resp := doScopedDockerReq(t, dp, http.MethodPost, "/commit?container=c-owned", []byte(tc.body)); code != http.StatusCreated {
			t.Fatalf("POST /commit %s: expected 201, got %d (%s)", tc.name, code, resp)
		}
		if code, resp := doScopedDockerCreate(t, dp, "/containers/create", []byte(tc.body)); code != http.StatusCreated {
			t.Fatalf("POST /containers/create %s: expected 201, got %d (%s)", tc.name, code, resp)
		}
	}
}

func TestDockerScope_RevokedFolderBlocksContainerStartRestartUnpause(t *testing.T) {
	dataDir := t.TempDir()
	allowedDir := t.TempDir()
	outsideDir := t.TempDir()
	gs := newTestGrantStoreWithDocker(t, dataDir, "scope-ext", []string{allowedDir})

	type mockCtr struct {
		labels map[string]string
		binds  []string
		hcMnts []map[string]any
		mnts   []map[string]any
	}
	var (
		mu             sync.Mutex
		nextID         int
		containers     = map[string]mockCtr{}
		lifecycleCalls []string
		forceInspect   string // "", "500", "corrupt"
	)

	backend := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := stripDockerVersion(cleanDockerMatchPath(r.URL.Path))
		switch {
		case r.Method == http.MethodGet && strings.HasPrefix(p, "/images/") && strings.HasSuffix(p, "/json"):
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"Id":"sha256:alpine","Config":{"Labels":{}}}`))

		case r.Method == http.MethodPost && p == "/containers/create":
			raw, _ := io.ReadAll(r.Body)
			var req struct {
				Labels     map[string]string `json:"Labels"`
				HostConfig struct {
					Binds  []string         `json:"Binds"`
					Mounts []map[string]any `json:"Mounts"`
				} `json:"HostConfig"`
			}
			_ = json.Unmarshal(raw, &req)
			mu.Lock()
			nextID++
			cid := fmt.Sprintf("ctr-%d", nextID)
			var topMnts []map[string]any
			for _, m := range req.HostConfig.Mounts {
				topMnts = append(topMnts, map[string]any{
					"Type":        m["Type"],
					"Source":      m["Source"],
					"Destination": m["Target"],
				})
			}
			containers[cid] = mockCtr{
				labels: req.Labels,
				binds:  req.HostConfig.Binds,
				hcMnts: req.HostConfig.Mounts,
				mnts:   topMnts,
			}
			mu.Unlock()
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(map[string]string{"Id": cid})

		case r.Method == http.MethodGet && strings.HasPrefix(p, "/containers/") && strings.HasSuffix(p, "/json"):
			cid := strings.TrimSuffix(strings.TrimPrefix(p, "/containers/"), "/json")
			mu.Lock()
			fi := forceInspect
			c, ok := containers[cid]
			mu.Unlock()
			if fi == "500" {
				http.Error(w, `{"message":"backend inspect error"}`, http.StatusInternalServerError)
				return
			}
			if fi == "corrupt" {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusOK)
				_, _ = w.Write([]byte(`{corrupt`))
				return
			}
			if !ok {
				http.Error(w, `{"message":"No such container"}`, http.StatusNotFound)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"Id":     cid,
				"Image":  "alpine:3.20",
				"Config": map[string]any{"Labels": c.labels},
				"HostConfig": map[string]any{
					"Binds":  c.binds,
					"Mounts": c.hcMnts,
				},
				"Mounts": c.mnts,
			})

		case r.Method == http.MethodPost && strings.HasPrefix(p, "/containers/"):
			mu.Lock()
			lifecycleCalls = append(lifecycleCalls, p)
			mu.Unlock()
			w.WriteHeader(http.StatusNoContent)

		default:
			w.WriteHeader(http.StatusOK)
		}
	})

	dp := newScopedDockerProxy("scope-ext", dataDir, gs, []string{PermDocker}, nil, nil, func() http.Handler {
		return backend
	})
	defer dp.Close()

	createCtr := func(body string) string {
		code, resp := doScopedDockerCreate(t, dp, "/containers/create", []byte(body))
		if code != http.StatusCreated {
			t.Fatalf("create failed: %d (%s)", code, resp)
		}
		var out struct {
			ID string `json:"Id"`
		}
		_ = json.Unmarshal(resp, &out)
		return out.ID
	}

	ctrBinds := createCtr(fmt.Sprintf(`{"Image":"alpine:3.20","HostConfig":{"Binds":[%q]}}`, allowedDir+":/work:rw"))
	ctrMounts := createCtr(fmt.Sprintf(`{"Image":"alpine:3.20","HostConfig":{"Mounts":[{"Type":"bind","Source":%q,"Target":"/work"}]}}`, allowedDir))
	ctrData := createCtr(`{"Image":"alpine:3.20","HostConfig":{"Binds":["/data/sub:/data-sub:rw"]}}`)

	// Also seed a container that only reports the bind in top-level Mounts (simulating inspect output).
	ctrTopMounts := "ctr-top-mounts"
	mu.Lock()
	containers[ctrTopMounts] = mockCtr{
		labels: map[string]string{"io.gvx.extension": "scope-ext"},
		mnts: []map[string]any{
			{"Type": "bind", "Source": allowedDir, "Destination": "/work"},
		},
	}
	mu.Unlock()

	// Before revocation, start/restart/unpause succeed with 204.
	for _, action := range []string{"start", "restart", "unpause"} {
		if code, resp := doScopedDockerReq(t, dp, http.MethodPost, "/containers/"+ctrBinds+"/"+action, nil); code != http.StatusNoContent {
			t.Fatalf("before revoke %s: expected 204, got %d (%s)", action, code, resp)
		}
	}

	// Revoke allowedDir.
	removed, err := gs.RemoveAllowedFolder("scope-ext", allowedDir)
	if err != nil || !removed {
		t.Fatalf("RemoveAllowedFolder = (%v, %v)", removed, err)
	}

	mu.Lock()
	lifecycleCalls = nil
	mu.Unlock()

	// After revocation, start/restart/unpause on all containers referencing allowedDir must return 403.
	for _, cid := range []string{ctrBinds, ctrMounts, ctrTopMounts} {
		for _, action := range []string{"start", "restart", "unpause"} {
			for _, prefix := range []string{"", "/v1.52"} {
				p := prefix + "/containers/" + cid + "/" + action
				code, resp := doScopedDockerReq(t, dp, http.MethodPost, p, nil)
				if code != http.StatusForbidden {
					t.Fatalf("after revoke POST %s: expected 403, got %d (%s)", p, code, resp)
				}
			}
		}
	}

	mu.Lock()
	callsAfterBlock := len(lifecycleCalls)
	mu.Unlock()
	if callsAfterBlock != 0 {
		t.Fatalf("blocked lifecycle calls leaked to backend: %v", lifecycleCalls)
	}

	// Container binding extension-data/<id>/files (/data/sub) continues to start/restart/unpause cleanly.
	for _, action := range []string{"start", "restart", "unpause"} {
		if code, resp := doScopedDockerReq(t, dp, http.MethodPost, "/containers/"+ctrData+"/"+action, nil); code != http.StatusNoContent {
			t.Fatalf("ctrData %s: expected 204, got %d (%s)", action, code, resp)
		}
	}

	// Inspect errors (500 / corrupt JSON) must fail closed on start/restart/unpause.
	for _, mode := range []string{"500", "corrupt"} {
		mu.Lock()
		forceInspect = mode
		lifecycleCalls = nil
		mu.Unlock()
		for _, action := range []string{"start", "restart", "unpause"} {
			code, resp := doScopedDockerReq(t, dp, http.MethodPost, "/containers/"+ctrData+"/"+action, nil)
			if code >= 200 && code < 300 {
				t.Fatalf("inspect=%s %s must fail closed, got %d (%s)", mode, action, code, resp)
			}
		}
		mu.Lock()
		leaked := len(lifecycleCalls)
		mu.Unlock()
		if leaked != 0 {
			t.Fatalf("inspect=%s leaked lifecycle calls to backend: %v", mode, lifecycleCalls)
		}
	}
	_ = outsideDir
}

func TestDockerScope_ExecPrivilegedRejected(t *testing.T) {
	dataDir := t.TempDir()
	gs := newTestGrantStoreWithDocker(t, dataDir, "exec-ext", nil)

	var (
		mu             sync.Mutex
		backendBodies  []string
		backendHeaders []http.Header
	)
	backend := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := stripDockerVersion(cleanDockerMatchPath(r.URL.Path))
		switch {
		case r.Method == http.MethodGet && p == "/containers/ctr-owned/json":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"Id":"ctr-owned","Image":"alpine:3.20","Config":{"Labels":{"io.gvx.extension":"exec-ext"}}}`))
		case r.Method == http.MethodPost && p == "/containers/ctr-owned/exec":
			b, _ := io.ReadAll(r.Body)
			mu.Lock()
			backendBodies = append(backendBodies, string(b))
			backendHeaders = append(backendHeaders, r.Header.Clone())
			mu.Unlock()
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"Id":"exec-1"}`))
		default:
			http.Error(w, `{"message":"not found"}`, http.StatusNotFound)
		}
	})

	dp := newScopedDockerProxy("exec-ext", dataDir, gs, []string{PermDocker}, nil, nil, func() http.Handler {
		return backend
	})
	defer dp.Close()

	// 1. Privileged: true (various casings / Unicode SimpleFold) must return 403 and never reach backend.
	for _, tc := range []struct {
		name     string
		body     string
		wantCode int
	}{
		{"upper-true", `{"Cmd":["id"],"Privileged":true}`, http.StatusForbidden},
		{"lower-true", `{"Cmd":["id"],"privileged":true}`, http.StatusForbidden},
		{"mixed-true", `{"Cmd":["id"],"pRiViLeGeD":true}`, http.StatusForbidden},
		{"dup-case-keys", `{"Cmd":["id"],"Privileged":false,"privileged":true}`, http.StatusBadRequest},
		{"non-bool-string", `{"Cmd":["id"],"Privileged":"true"}`, http.StatusBadRequest},
		{"non-bool-number", `{"Cmd":["id"],"Privileged":1}`, http.StatusBadRequest},
		{"non-object-null", `null`, http.StatusBadRequest},
		{"malformed-json", `{"Cmd":["id"`, http.StatusBadRequest},
	} {
		for _, prefix := range []string{"", "/v1.52"} {
			p := prefix + "/containers/ctr-owned/exec"
			code, resp := doScopedDockerReq(t, dp, http.MethodPost, p, []byte(tc.body))
			if code != tc.wantCode {
				t.Fatalf("%s POST %s: expected %d, got %d (%s)", tc.name, p, tc.wantCode, code, resp)
			}
		}
	}

	// 2. Upgrade requests on POST /containers/{id}/exec must be rejected with 400.
	upgReq := httptest.NewRequest(http.MethodPost, "/containers/ctr-owned/exec", strings.NewReader(`{"Cmd":["id"]}`))
	upgReq.Header.Set("Connection", "Upgrade")
	upgReq.Header.Set("Upgrade", "websocket")
	upgRec := httptest.NewRecorder()
	dp.ServeHTTP(upgRec, upgReq)
	if upgRec.Code != http.StatusBadRequest {
		t.Fatalf("upgrade on /exec expected 400, got %d (%s)", upgRec.Code, upgRec.Body.String())
	}

	mu.Lock()
	leaked := len(backendBodies)
	mu.Unlock()
	if leaked != 0 {
		t.Fatalf("rejected exec payloads leaked to backend: %v", backendBodies)
	}

	// 3. Privileged: false (any valid single key) and omitted Privileged succeed and normalize cleanly.
	for _, body := range []string{
		`{"Cmd":["id"],"Privileged":false}`,
		`{"Cmd":["id"],"privileged":false}`,
		`{"Cmd":["id"]}`,
	} {
		code, resp := doScopedDockerReq(t, dp, http.MethodPost, "/v1.52/containers/ctr-owned/exec", []byte(body))
		if code != http.StatusCreated {
			t.Fatalf("valid exec %s: expected 201, got %d (%s)", body, code, resp)
		}
	}

	mu.Lock()
	bodies := append([]string(nil), backendBodies...)
	hdrs := append([]http.Header(nil), backendHeaders...)
	mu.Unlock()
	if len(bodies) != 3 {
		t.Fatalf("expected 3 backend exec calls, got %d", len(bodies))
	}
	for i, raw := range bodies {
		var parsed map[string]any
		if err := json.Unmarshal([]byte(raw), &parsed); err != nil {
			t.Fatalf("backend body %d invalid JSON %q: %v", i, raw, err)
		}
		if priv, ok := parsed["Privileged"]; ok && priv != false {
			t.Fatalf("backend body %d unexpected Privileged=%v", i, priv)
		}
		if _, ok := parsed["privileged"]; ok {
			t.Fatalf("backend body %d kept non-canonical lowercase privileged: %s", i, raw)
		}
		_ = hdrs[i]
	}
}
