package ext

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ironpark/gvx-desktop/internal/keychain"
	"github.com/ironpark/gvx-desktop/internal/rungv"
	"github.com/ironpark/gvx-desktop/internal/sandbox"
	"github.com/ironpark/gvx-desktop/internal/store"
	"github.com/ironpark/gvx-desktop/pkg/extsdk"
)

type fakeUIHost struct {
	mu           sync.Mutex
	pickPath     string
	pickOK       bool
	pickErr      error
	openedURLs   []string
	copied       []string
	toasts       []string
	terminals    []string
	changedCount int
}

func (f *fakeUIHost) PickFolder(context.Context, string) (string, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.pickPath, f.pickOK, f.pickErr
}

func (f *fakeUIHost) OpenTerminal(targetType, targetID, shell string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.terminals = append(f.terminals, targetType+":"+targetID+":"+shell)
	return nil
}

func (f *fakeUIHost) OpenURL(rawURL string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.openedURLs = append(f.openedURLs, rawURL)
	return nil
}

func (f *fakeUIHost) CopyText(text string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.copied = append(f.copied, text)
	return nil
}

func (f *fakeUIHost) ShowToast(level, message string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.toasts = append(f.toasts, level+":"+message)
}

func (f *fakeUIHost) Changed() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.changedCount++
}

func setupTestHostAPI(t *testing.T, perms []string, uiHost UIHost) (*hostAPIServer, *extsdk.Host, *gestureStore, *GrantStore, string) {
	t.Helper()
	dataDir := t.TempDir()
	extID := "host-test-ext"
	gs, err := LoadGrantStore(GrantsPath(dataDir))
	if err != nil {
		t.Fatalf("LoadGrantStore: %v", err)
	}
	m := &Manifest{
		SchemaVersion: 1,
		ID:            extID,
		Version:       "1.2.3",
		Publisher:     "acme",
		Name:          "Host Test",
		Permissions:   perms,
		Settings: []SettingDecl{
			{Key: "api_url", Type: SettingString, Label: "API URL", Default: "https://default.invalid"},
			{Key: "api_secret", Type: SettingSecret, Label: "Secret"},
		},
	}
	if err := gs.Approve(extID, m, ""); err != nil {
		t.Fatalf("Approve: %v", err)
	}
	vault := NewVault(&keychain.Fake{}, dataDir, extID)
	settings := NewSettingsStore(SettingsPath(dataDir, extID), vault)
	_ = settings.Set(m.Settings[1], "super-secret-value")
	storage := NewStorage(StoragePath(dataDir, extID), DefaultStorageQuota)
	gestures := newGestureStore(nil)
	var sandboxesMu sync.Mutex

	srv := newHostAPIServer(hostAPIConfig{
		extID:       extID,
		dataDir:     dataDir,
		token:       "secret-host-token",
		manifest:    m,
		grants:      gs,
		storage:     storage,
		vault:       vault,
		settings:    settings,
		gestures:    gestures,
		uiHost:      uiHost,
		supervisor:  &sandbox.Supervisor{DataDir: dataDir},
		sandboxesMu: &sandboxesMu,
		hostInfoFn: func() HostInfo {
			return HostInfo{
				AppVersion: "0.10.0-test",
				OS:         "darwin",
				Arch:       "arm64",
				Locale:     "ko",
				Theme:      "light",
				DockerMode: "sandbox",
			}
		},
	})
	ep, _, _, err := srv.Listen()
	if err != nil {
		t.Fatalf("srv.Listen: %v", err)
	}
	t.Cleanup(func() { _ = srv.Close() })

	hostClient, err := extsdk.NewHost(ep, "secret-host-token")
	if err != nil {
		t.Fatalf("NewHost: %v", err)
	}
	return srv, hostClient, gestures, gs, dataDir
}

func TestHostAPI_AuthInfoSettingsStorageSecrets(t *testing.T) {
	ui := &fakeUIHost{}
	srv, host, _, _, _ := setupTestHostAPI(t, []string{PermStorage, PermSecrets}, ui)
	ctx := context.Background()

	// Wrong token -> 401.
	badHost, err := extsdk.NewHost("unix://"+srv.sockPath, "wrong-token")
	if err == nil {
		if _, err := badHost.Info(ctx); err == nil || !strings.Contains(err.Error(), "401") {
			t.Fatalf("expected 401 for wrong token, got %v", err)
		}
	}

	// HostInfo.
	hi, err := host.Info(ctx)
	if err != nil {
		t.Fatalf("Info: %v", err)
	}
	if hi.AppVersion != "0.10.0-test" || hi.Locale != "ko" || hi.Theme != "light" {
		t.Fatalf("unexpected HostInfo: %+v", hi)
	}

	// Settings masks secrets.
	set, err := host.Settings(ctx)
	if err != nil {
		t.Fatalf("Settings: %v", err)
	}
	if set.String("api_url") != "https://default.invalid" || !set.IsSet("api_secret") || set.String("api_secret") != "" {
		t.Fatalf("unexpected settings payload: %+v", set.Raw())
	}

	// Storage CRUD.
	if err := host.StorageSet(ctx, "k/1", "hello"); err != nil {
		t.Fatalf("StorageSet: %v", err)
	}
	v, ok, err := host.StorageGet(ctx, "k/1")
	if err != nil || !ok || v != "hello" {
		t.Fatalf("StorageGet = (%q, %v, %v)", v, ok, err)
	}
	keys, err := host.StorageList(ctx, "k/")
	if err != nil || len(keys) != 1 || keys[0] != "k/1" {
		t.Fatalf("StorageList = (%v, %v)", keys, err)
	}
	if err := host.StorageDelete(ctx, "k/1"); err != nil {
		t.Fatalf("StorageDelete: %v", err)
	}

	// Secrets CRUD + 60/min rate limit.
	if err := host.SecretSet(ctx, "tok", "v1"); err != nil {
		t.Fatalf("SecretSet: %v", err)
	}
	sv, ok, err := host.SecretGet(ctx, "tok")
	if err != nil || !ok || sv != "v1" {
		t.Fatalf("SecretGet = (%q, %v, %v)", sv, ok, err)
	}
	var rateErr error
	for i := 0; i < 65; i++ {
		if _, _, err := host.SecretGet(ctx, "tok"); err != nil {
			rateErr = err
			break
		}
	}
	if !extsdk.IsRateLimited(rateErr) {
		t.Fatalf("expected 429 rate_limited after >60 secret calls, got %v", rateErr)
	}
}

func TestHostAPI_GestureGatingAndUILimits(t *testing.T) {
	ui := &fakeUIHost{pickPath: t.TempDir(), pickOK: true}
	_, host, gestures, gs, _ := setupTestHostAPI(t, []string{PermUIOpenURL, PermUIPickFolder, PermUITerminal}, ui)
	ctx := context.Background()

	// Missing gesture -> 403 gesture_required.
	if err := host.Copy(ctx, "abc"); !extsdk.IsGestureRequired(err) {
		t.Fatalf("expected gesture_required on Copy without gesture, got %v", err)
	}
	if err := host.OpenURL(ctx, "https://example.com"); !extsdk.IsGestureRequired(err) {
		t.Fatalf("expected gesture_required on OpenURL without gesture, got %v", err)
	}

	// Mint gesture, test max 8 intents + immediate revocation + replay rejection.
	gid, err := gestures.mint("host-test-ext")
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	gCtx := extsdk.WithGestureID(ctx, gid)
	for i := 0; i < 8; i++ {
		if err := host.Copy(gCtx, "item"); err != nil {
			t.Fatalf("intent %d failed: %v", i+1, err)
		}
	}
	if err := host.Copy(gCtx, "item-9"); !extsdk.IsGestureRequired(err) {
		t.Fatalf("expected 9th intent to fail with gesture_required, got %v", err)
	}
	gestures.revoke(gid)

	// Fresh gesture for PickFolder, OpenURL validation, and Copy limit.
	gid2, _ := gestures.mint("host-test-ext")
	defer gestures.revoke(gid2)
	gCtx2 := extsdk.WithGestureID(ctx, gid2)

	picked, ok, err := host.PickFolder(gCtx2)
	if err != nil || !ok || picked == "" {
		t.Fatalf("PickFolder = (%q, %v, %v)", picked, ok, err)
	}
	if len(gs.Get("host-test-ext").AllowedFolders) != 1 {
		t.Fatalf("expected allowed_folders updated, got %v", gs.Get("host-test-ext").AllowedFolders)
	}

	for _, badURL := range []string{
		"javascript:alert(1)",
		"file:///etc/passwd",
		"https://user:pass@example.com",
		"https://",
		"https://example.com/" + strings.Repeat("a", 2050),
	} {
		if err := host.OpenURL(gCtx2, badURL); err == nil {
			t.Errorf("expected OpenURL(%q) rejected", badURL)
		}
	}
	if err := host.OpenURL(gCtx2, "https://example.com/ok"); err != nil {
		t.Fatalf("OpenURL valid failed: %v", err)
	}
	if err := host.Copy(gCtx2, strings.Repeat("x", maxCopyBytes+1)); err == nil {
		t.Fatal("expected >64 KiB copy rejected")
	}

	// Toast rate limit (12/min).
	for i := 0; i < 12; i++ {
		if err := host.Toast(ctx, "info", "msg"); err != nil {
			t.Fatalf("toast %d: %v", i, err)
		}
	}
	if err := host.Toast(ctx, "info", "msg-13"); !extsdk.IsRateLimited(err) {
		t.Fatalf("expected 13th toast rate_limited, got %v", err)
	}

	// Revoke PermUIOpenURL -> permission_denied checked BEFORE gesture.
	if err := gs.RevokePermissions("host-test-ext"); err != nil {
		t.Fatalf("RevokePermissions: %v", err)
	}
	if err := host.OpenURL(gCtx2, "https://example.com"); !extsdk.IsPermissionDenied(err) {
		t.Fatalf("expected permission_denied after revoke, got %v", err)
	}
}

func TestHostAPI_SandboxesOwnershipAndDockerSockGate(t *testing.T) {
	ui := &fakeUIHost{}
	// Grant PermSandboxes WITHOUT PermDocker.
	_, host, _, _, dataDir := setupTestHostAPI(t, []string{PermSandboxes}, ui)
	ctx := context.Background()

	// MountDockerSock without PermDocker -> 403 permission_denied.
	startFalse := false
	_, err := host.SandboxCreate(ctx, extsdk.CreateSandboxRequest{
		Name:            "sneaky",
		Image:           "alpine:3.20",
		MountDockerSock: true,
		Start:           &startFalse,
	})
	if !extsdk.IsPermissionDenied(err) {
		t.Fatalf("expected permission_denied for mount_docker_sock without docker perm, got %v", err)
	}

	// Bind mount outside allowed_folders -> 403 permission_denied.
	_, err = host.SandboxCreate(ctx, extsdk.CreateSandboxRequest{
		Name:   "bad-bind",
		Image:  "alpine:3.20",
		Mounts: []extsdk.SandboxMount{{Type: "bind", Source: "/etc", Target: "/host-etc"}},
		Start:  &startFalse,
	})
	if !extsdk.IsPermissionDenied(err) {
		t.Fatalf("expected permission_denied for unapproved sandbox bind mount, got %v", err)
	}

	// Unix socket port-forward in Ports -> 403 permission_denied.
	for _, evilPort := range []string{"unix:/tmp/evil.sock:80", "  unix:/tmp/evil.sock:80"} {
		_, err = host.SandboxCreate(ctx, extsdk.CreateSandboxRequest{
			Name:  "bad-unix-port",
			Image: "alpine:3.20",
			Ports: []string{evilPort},
			Start: &startFalse,
		})
		if !extsdk.IsPermissionDenied(err) {
			t.Fatalf("expected permission_denied for unix: port forward %q, got %v", evilPort, err)
		}
	}

	// Valid owned ephemeral sandbox creation (unstarted) persists to sandboxes.json with Labels.
	sbx, err := host.SandboxCreate(ctx, extsdk.CreateSandboxRequest{
		Name:      "owned-job",
		Image:     "alpine:3.20",
		Ephemeral: true,
		Start:     &startFalse,
	})
	if err != nil {
		t.Fatalf("SandboxCreate: %v", err)
	}
	specs, err := (store.Store{Dir: dataDir}).Sandboxes()
	if err != nil || len(specs) != 1 || specs[0].Labels["io.gvx.extension"] != "host-test-ext" || specs[0].Labels["io.gvx.ephemeral"] != "true" {
		t.Fatalf("unexpected persisted specs: %+v (err=%v)", specs, err)
	}
	owned, err := host.SandboxList(ctx, true)
	if err != nil || len(owned) != 1 || owned[0].ID != sbx.ID {
		t.Fatalf("SandboxList(owned=true) = %+v, %v", owned, err)
	}

	// Unowned sandbox operations -> 403 permission_denied.
	if _, err := host.SandboxStart(ctx, "user-owned-sbx"); !extsdk.IsPermissionDenied(err) {
		t.Fatalf("expected 403 on unowned start, got %v", err)
	}
	if _, err := host.SnapshotList(ctx, "user-owned-sbx"); !extsdk.IsPermissionDenied(err) {
		t.Fatalf("expected 403 on unowned snapshots, got %v", err)
	}
	if err := host.SandboxDelete(ctx, sbx.ID); err != nil {
		t.Fatalf("SandboxDelete(owned): %v", err)
	}
}

func TestHostAPI_EventsSSE(t *testing.T) {
	ui := &fakeUIHost{}
	srv, host, _, _, _ := setupTestHostAPI(t, nil, ui)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	gotCh := make(chan extsdk.HostEvent, 4)
	errCh := make(chan error, 1)
	go func() {
		errCh <- host.SubscribeEvents(ctx, func(ev extsdk.HostEvent) error {
			gotCh <- ev
			cancel()
			return nil
		})
	}()

	// Wait briefly for SSE subscriber to attach, then publish.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		srv.cfg.events.mu.Lock()
		n := len(srv.cfg.events.subs)
		srv.cfg.events.mu.Unlock()
		if n > 0 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	srv.cfg.events.publish("host.theme", map[string]string{"theme": "light"})

	select {
	case ev := <-gotCh:
		var body map[string]string
		_ = json.Unmarshal(ev.Data, &body)
		if ev.Type != "host.theme" || body["theme"] != "light" {
			t.Fatalf("unexpected SSE event: %+v", ev)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for host.theme SSE event")
	}
	if err := <-errCh; err != nil && !errors.Is(err, context.Canceled) {
		t.Fatalf("SubscribeEvents error: %v", err)
	}
}

func TestHostAPI_SettingSecretGetAndGuards(t *testing.T) {
	ctx := context.Background()

	// 1. Missing PermSecrets -> 403 permission_denied.
	_, noPermHost, _, _, _ := setupTestHostAPI(t, []string{PermStorage}, &fakeUIHost{})
	if _, _, err := noPermHost.SecretGet(ctx, "setting:api_secret"); !extsdk.IsPermissionDenied(err) {
		t.Fatalf("expected 403 permission_denied without PermSecrets, got %v", err)
	}

	// 2. With PermSecrets: valid setting:<key> returns value; non-secret/undeclared/unset -> 404;
	// invalid key -> 400; PUT/DELETE still return 400; GET /v1/secrets excludes setting:*; rate limit -> 429.
	srv, host, _, _, _ := setupTestHostAPI(t, []string{PermSecrets}, &fakeUIHost{})

	// Add an unset declared secret setting to manifest for 404 unset testing.
	srv.cfg.manifest.Settings = append(srv.cfg.manifest.Settings, SettingDecl{
		Key:   "optional_secret",
		Type:  SettingSecret,
		Label: "Optional Secret",
	})

	val, ok, err := host.SecretGet(ctx, "setting:api_secret")
	if err != nil || !ok || val != "super-secret-value" {
		t.Fatalf("SecretGet(setting:api_secret) = (%q, %v, %v), want (super-secret-value, true, nil)", val, ok, err)
	}

	// Legacy secret in settings.json is migrated on GET /v1/secrets/setting:<key>.
	srv.cfg.manifest.Settings = append(srv.cfg.manifest.Settings, SettingDecl{
		Key:   "legacy_secret",
		Type:  SettingSecret,
		Label: "Legacy Secret",
	})
	if err := WriteFileAtomic(SettingsPath(srv.cfg.dataDir, srv.cfg.extID), []byte(`{"legacy_secret":"migrated-val"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	srv.cfg.settings.Reset()
	val, ok, err = host.SecretGet(ctx, "setting:legacy_secret")
	if err != nil || !ok || val != "migrated-val" {
		t.Fatalf("SecretGet(setting:legacy_secret) = (%q, %v, %v), want (migrated-val, true, nil)", val, ok, err)
	}

	// Unset declared secret setting -> 404.
	if gotVal, found, err := host.SecretGet(ctx, "setting:optional_secret"); err != nil || found || gotVal != "" {
		t.Fatalf("SecretGet(setting:optional_secret) = (%q, %v, %v), want (\"\", false, nil)", gotVal, found, err)
	}

	// Non-secret setting & undeclared setting -> 404.
	for _, k := range []string{"setting:api_url", "setting:undeclared_key"} {
		if gotVal, found, err := host.SecretGet(ctx, k); err != nil || found || gotVal != "" {
			t.Fatalf("SecretGet(%q) = (%q, %v, %v), want (\"\", false, nil)", k, gotVal, found, err)
		}
	}

	// Invalid key after "setting:" -> 400 bad_request.
	for _, bad := range []string{"setting:", "setting:../etc", "setting:a:b", "setting:a/b"} {
		_, _, err := host.SecretGet(ctx, bad)
		var he *extsdk.HostError
		if !errors.As(err, &he) || he.StatusCode != 400 || he.Code != "bad_request" {
			t.Fatalf("SecretGet(%q) err = %v, want HTTP 400 bad_request", bad, err)
		}
	}

	// PUT and DELETE on setting:<key> must still return 400 bad_request.
	if err := host.SecretPut(ctx, "setting:api_secret", "hacked"); err == nil {
		t.Fatal("SecretPut(setting:api_secret) succeeded, want 400 bad_request")
	} else {
		var he *extsdk.HostError
		if !errors.As(err, &he) || he.StatusCode != 400 || he.Code != "bad_request" {
			t.Fatalf("SecretPut(setting:api_secret) err = %v, want HTTP 400 bad_request", err)
		}
	}
	if err := host.SecretDelete(ctx, "setting:api_secret"); err == nil {
		t.Fatal("SecretDelete(setting:api_secret) succeeded, want 400 bad_request")
	} else {
		var he *extsdk.HostError
		if !errors.As(err, &he) || he.StatusCode != 400 || he.Code != "bad_request" {
			t.Fatalf("SecretDelete(setting:api_secret) err = %v, want HTTP 400 bad_request", err)
		}
	}

	// GET /v1/secrets must not list setting: entries.
	keys, err := host.SecretList(ctx)
	if err != nil {
		t.Fatalf("SecretList: %v", err)
	}
	if len(keys) != 0 {
		t.Fatalf("SecretList = %v, want empty (setting: must not be listed)", keys)
	}

	// Rate limit (60/min) applies to setting:<key> reads as well.
	var rateErr error
	for i := 0; i < 65; i++ {
		if _, _, err := host.SecretGet(ctx, "setting:api_secret"); err != nil {
			rateErr = err
			break
		}
	}
	if !extsdk.IsRateLimited(rateErr) {
		t.Fatalf("expected 429 rate_limited after >60 setting secret reads, got %v", rateErr)
	}
}

func TestHostAPI_RejectsInvalidUTF8AndLoneSurrogates(t *testing.T) {
	ui := &fakeUIHost{}
	srv, host, gestures, _, _ := setupTestHostAPI(t, []string{PermStorage, PermSecrets}, ui)
	ctx := context.Background()
	h := srv.Handler()

	doRaw := func(method, path, gid string, body []byte) int {
		req := httptest.NewRequest(method, path, bytes.NewReader(body))
		req.Header.Set("Authorization", "Bearer secret-host-token")
		req.Header.Set("Content-Type", "application/json")
		if gid != "" {
			req.Header.Set("X-GVX-Gesture-ID", gid)
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec.Code
	}

	gid, err := gestures.mint("host-test-ext")
	if err != nil {
		t.Fatal(err)
	}
	defer gestures.revoke(gid)

	invalidBodies := [][]byte{
		[]byte("{\"value\":\"\xff\xfe\"}"),
		[]byte(`{"value":"\uD800"}`),
		[]byte(`{"value":"\uDFFF"}`),
		[]byte(`{"value":"\uD800\uD800"}`),
		[]byte(`{"value":"\\\uD800"}`),
	}
	for _, body := range invalidBodies {
		if code := doRaw(http.MethodPut, "/v1/storage/k", "", body); code != http.StatusBadRequest {
			t.Errorf("PUT /v1/storage/k body=%q got %d, want 400", body, code)
		}
		if code := doRaw(http.MethodPut, "/v1/secrets/s1", "", body); code != http.StatusBadRequest {
			t.Errorf("PUT /v1/secrets/s1 body=%q got %d, want 400", body, code)
		}
	}
	if code := doRaw(http.MethodPost, "/v1/ui/copy", gid, []byte("{\"text\":\"bad\xff\"}")); code != http.StatusBadRequest {
		t.Errorf("POST /v1/ui/copy raw invalid UTF-8 got %d, want 400", code)
	}
	if code := doRaw(http.MethodPost, "/v1/ui/copy", gid, []byte(`{"text":"\uD800"}`)); code != http.StatusBadRequest {
		t.Errorf("POST /v1/ui/copy lone surrogate got %d, want 400", code)
	}
	if code := doRaw(http.MethodPost, "/v1/ui/toast", "", []byte(`{"level":"info","message":"\uDC00"}`)); code != http.StatusBadRequest {
		t.Errorf("POST /v1/ui/toast lone surrogate got %d, want 400", code)
	}

	// Valid surrogate pair, escaped backslash before \uD800, and explicit \ufffd must succeed.
	validCases := []struct {
		key  string
		body []byte
		want string
	}{
		{key: "pair", body: []byte(`{"value":"\uD83D\uDE00"}`), want: "😀"},
		{key: "escaped", body: []byte(`{"value":"\\uD800"}`), want: `\uD800`},
		{key: "fffd", body: []byte(`{"value":"\ufffd"}`), want: "\ufffd"},
	}
	for _, tc := range validCases {
		if code := doRaw(http.MethodPut, "/v1/storage/"+tc.key, "", tc.body); code != http.StatusOK {
			t.Fatalf("PUT /v1/storage/%s body=%s got %d, want 200", tc.key, tc.body, code)
		}
		got, ok, err := host.StorageGet(ctx, tc.key)
		if err != nil || !ok || got != tc.want {
			t.Fatalf("StorageGet(%s) = (%q, %v, %v), want %q", tc.key, got, ok, err, tc.want)
		}
	}

	// Docker scope container create JSON also rejects raw invalid UTF-8 & lone surrogates.
	for _, bad := range [][]byte{
		[]byte("{\"Image\":\"alpine\",\"Cmd\":[\"\xff\"]}"),
		[]byte(`{"Image":"alpine","Cmd":["\uD800"]}`),
	} {
		if _, status, err := scopeContainerCreatePayload(bad, "host-test-ext", t.TempDir(), nil); err == nil || status != http.StatusBadRequest {
			t.Errorf("scopeContainerCreatePayload(%q) = (%d, %v), want 400", bad, status, err)
		}
	}
}

func TestHostAPI_SandboxDeleteRejectsSnapshotSymlinkEscape(t *testing.T) {
	if runtime.GOOS == "windows" {
		return
	}
	ui := &fakeUIHost{}
	_, host, _, _, dataDir := setupTestHostAPI(t, []string{PermSandboxes}, ui)
	ctx := context.Background()

	startFalse := false
	sbx, err := host.SandboxCreate(ctx, extsdk.CreateSandboxRequest{
		Name:      "owned-snap-sym",
		Image:     "alpine:3.20",
		Ephemeral: true,
		Start:     &startFalse,
	})
	if err != nil {
		t.Fatalf("SandboxCreate: %v", err)
	}

	outside := t.TempDir()
	outsideIndex := filepath.Join(outside, sbx.ID+".json")
	if err := os.WriteFile(outsideIndex, []byte(`{"snap1":"b1"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	extDataDir := ExtensionDataDir(dataDir, "host-test-ext")
	if err := os.MkdirAll(extDataDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(extDataDir, "snapshots")); err != nil {
		t.Fatal(err)
	}

	if err := host.SandboxDelete(ctx, sbx.ID); err == nil {
		t.Fatal("SandboxDelete succeeded through symlinked snapshots dir")
	}
	if _, err := os.Stat(outsideIndex); err != nil {
		t.Fatalf("outside snapshot index %s was deleted through symlink: %v", outsideIndex, err)
	}

	// Replace symlink with real snapshots dir and symlink supervisor snapshots root instead.
	if err := os.Remove(filepath.Join(extDataDir, "snapshots")); err != nil {
		t.Fatal(err)
	}
	outsideSup := t.TempDir()
	outsideSupSbx := filepath.Join(outsideSup, sbx.ID)
	if err := os.MkdirAll(outsideSupSbx, 0o700); err != nil {
		t.Fatal(err)
	}
	outsideSupMarker := filepath.Join(outsideSupSbx, "keep.txt")
	if err := os.WriteFile(outsideSupMarker, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outsideSup, filepath.Join(dataDir, "snapshots")); err != nil {
		t.Fatal(err)
	}
	if err := host.SandboxDelete(ctx, sbx.ID); err == nil {
		t.Fatal("SandboxDelete succeeded through symlinked supervisor snapshots dir")
	}
	if _, err := os.Stat(outsideSupMarker); err != nil {
		t.Fatalf("outside supervisor snapshot file %s was deleted through symlink: %v", outsideSupMarker, err)
	}
}

func TestHostAPI_RestoreExecRetryAndStops(t *testing.T) {
	stateDir := t.TempDir()
	srcFile := filepath.Join(t.TempDir(), "main.go")
	const fakeRungvSrc = `package main

import (
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

func main() {
	if len(os.Args) < 2 {
		os.Exit(2)
	}
	stateDir := os.Getenv("FAKE_RUNGV_STATE_DIR")
	switch os.Args[1] {
	case "run":
		for _, a := range os.Args[2:] {
			if strings.Contains(a, "help") {
				fmt.Println("  -control-token-file")
				fmt.Println("  -control-socket")
				return
			}
		}
		waitStop()
	case "restore":
		for _, a := range os.Args[2:] {
			if strings.Contains(a, "help") {
				return
			}
		}
		modeBytes, _ := os.ReadFile(filepath.Join(stateDir, "restore_mode"))
		if strings.TrimSpace(string(modeBytes)) == "exit_fast" {
			time.Sleep(80 * time.Millisecond)
			os.Exit(1)
		}
		waitStop()
	case "exec":
		var pidFile string
		for i := 2; i < len(os.Args); i++ {
			if os.Args[i] == "--pid-file" && i+1 < len(os.Args) {
				pidFile = os.Args[i+1]
				i++
			}
		}
		cntPath := filepath.Join(stateDir, "exec_count")
		rawCnt, _ := os.ReadFile(cntPath)
		n, _ := strconv.Atoi(strings.TrimSpace(string(rawCnt)))
		n++
		_ = os.WriteFile(cntPath, []byte(strconv.Itoa(n)), 0o600)

		modeBytes, _ := os.ReadFile(filepath.Join(stateDir, "exec_mode"))
		mode := strings.TrimSpace(string(modeBytes))
		switch mode {
		case "retry_then_ok":
			if n <= 2 {
				fmt.Fprintln(os.Stderr, "connect: no such file or directory")
				os.Exit(1)
			}
			if pidFile != "" {
				_ = os.WriteFile(pidFile, []byte("12345\n"), 0o600)
			}
			fmt.Print("restored-ok")
			return
		case "non_unreachable":
			fmt.Fprintln(os.Stderr, "rungv exec: NOT_FOUND: missing command")
			os.Exit(1)
		case "always_unreachable":
			fmt.Fprintln(os.Stderr, "connect: connection refused")
			os.Exit(1)
		default:
			os.Exit(2)
		}
	case "events":
		return
	default:
		return
	}
}

func waitStop() {
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
	done := make(chan struct{})
	go func() {
		_, _ = io.Copy(io.Discard, os.Stdin)
		close(done)
	}()
	select {
	case <-sig:
	case <-done:
	case <-time.After(30 * time.Second):
	}
}
`
	if err := os.WriteFile(srcFile, []byte(fakeRungvSrc), 0o600); err != nil {
		t.Fatal(err)
	}
	exeName := "rungv"
	if runtime.GOOS == "windows" {
		exeName = "rungv.exe"
	}
	fakeRungvBin := filepath.Join(t.TempDir(), exeName)
	if out, err := exec.Command("go", "build", "-o", fakeRungvBin, srcFile).CombinedOutput(); err != nil {
		t.Fatalf("go build fake rungv: %v\n%s", err, out)
	}
	t.Setenv("FAKE_RUNGV_STATE_DIR", stateDir)
	t.Cleanup(func() { rungv.ForgetFeatures(fakeRungvBin) })

	srv, host, _, _, _ := setupTestHostAPI(t, []string{PermSandboxes, PermSnapshots}, &fakeUIHost{})
	supDir := t.TempDir()
	sup := &sandbox.Supervisor{
		DataDir: supDir,
		Client:  func() *rungv.Client { return &rungv.Client{Path: fakeRungvBin} },
	}
	srv.cfg.supervisor = sup
	t.Cleanup(func() { sup.StopAll(2 * time.Second) })

	ctx := context.Background()
	startFalse := false
	ckptTrue := true
	sbx, err := host.SandboxCreate(ctx, extsdk.CreateSandboxRequest{
		Name:       "restore-retry-sbx",
		Image:      "alpine:3.20",
		Checkpoint: &ckptTrue,
		Start:      &startFalse,
	})
	if err != nil {
		t.Fatalf("SandboxCreate: %v", err)
	}

	snapDir := filepath.Join(sup.SnapshotRoot(sbx.ID), "snap-warm")
	if err := os.MkdirAll(snapDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(snapDir, "manifest.json"), []byte(`{"version":1}`), 0o600); err != nil {
		t.Fatal(err)
	}
	srv.saveSnapshotIndex(sbx.ID, map[string]string{"warm": "snap-warm"})

	readExecCount := func() int {
		b, _ := os.ReadFile(filepath.Join(stateDir, "exec_count"))
		n, _ := strconv.Atoi(strings.TrimSpace(string(b)))
		return n
	}
	resetModes := func(restoreMode, execMode string) {
		_ = os.WriteFile(filepath.Join(stateDir, "exec_count"), []byte("0"), 0o600)
		_ = os.WriteFile(filepath.Join(stateDir, "restore_mode"), []byte(restoreMode), 0o600)
		_ = os.WriteFile(filepath.Join(stateDir, "exec_mode"), []byte(execMode), 0o600)
	}

	// 1. Retries when control socket initially returns rungv.IsUnreachable and succeeds once reachable.
	resetModes("stay", "retry_then_ok")
	res, err := host.Restore(ctx, sbx.ID, extsdk.RestoreRequest{
		Snapshot: "warm",
		Exec:     []string{"echo", "ok"},
	})
	if err != nil {
		t.Fatalf("Restore(retry_then_ok): %v", err)
	}
	if res.Exec == nil || res.Exec.ExitCode != 0 || res.Exec.Stdout != "restored-ok" {
		t.Fatalf("unexpected Restore Exec result: %+v", res.Exec)
	}
	if got := readExecCount(); got != 3 {
		t.Fatalf("exec attempt count = %d, want 3", got)
	}

	// 2. Stops immediately on non-unreachable error (no retries).
	resetModes("stay", "non_unreachable")
	if _, err := host.Restore(ctx, sbx.ID, extsdk.RestoreRequest{
		Snapshot: "warm",
		Exec:     []string{"missing"},
	}); err == nil {
		t.Fatal("expected Restore(non_unreachable) to fail")
	}
	if got := readExecCount(); got != 1 {
		t.Fatalf("non-unreachable exec attempt count = %d, want 1 (no retry)", got)
	}

	// 3. Stops immediately on context cancellation without exhausting retry budget.
	resetModes("stay", "always_unreachable")
	cancelCtx, cancel := context.WithTimeout(ctx, 120*time.Millisecond)
	defer cancel()
	startCancel := time.Now()
	_, cancelErr := host.Restore(cancelCtx, sbx.ID, extsdk.RestoreRequest{
		Snapshot: "warm",
		Exec:     []string{"echo", "cancel"},
	})
	cancelElapsed := time.Since(startCancel)
	if cancelErr == nil {
		t.Fatal("expected Restore(cancel) to fail")
	}
	if cancelElapsed > 1500*time.Millisecond {
		t.Fatalf("Restore(cancel) took %v, expected prompt cancellation before full retry budget", cancelElapsed)
	}
	if got := readExecCount(); got > 3 {
		t.Fatalf("Restore(cancel) ran %d attempts, expected prompt stop", got)
	}

	// 4. Stops immediately when the restored sandbox exits during retry backoff.
	resetModes("exit_fast", "always_unreachable")
	startExit := time.Now()
	_, exitErr := host.Restore(ctx, sbx.ID, extsdk.RestoreRequest{
		Snapshot: "warm",
		Exec:     []string{"echo", "dead"},
	})
	exitElapsed := time.Since(startExit)
	if exitErr == nil {
		t.Fatal("expected Restore(exit_fast) to fail")
	}
	if exitElapsed > 1500*time.Millisecond {
		t.Fatalf("Restore(exit_fast) took %v, expected prompt stop when sandbox exited", exitElapsed)
	}
	if got := readExecCount(); got > 3 {
		t.Fatalf("Restore(exit_fast) ran %d attempts, expected stop once sandbox exited", got)
	}
}

func TestHostAPI_ConcurrentSnapshotIndexRMWAndSandboxDelete(t *testing.T) {
	srv, host, _, _, _ := setupTestHostAPI(t, []string{PermSandboxes, PermSnapshots}, &fakeUIHost{})

	st := srv.sandboxStore()
	if err := st.SaveSandboxes([]sandbox.Spec{
		{
			ID:     "sbx-race",
			Name:   "sbx-race",
			Image:  "alpine:3.20",
			Labels: map[string]string{"io.gvx.extension": "host-test-ext"},
		},
		{
			ID:     "sbx-del-race",
			Name:   "sbx-del-race",
			Image:  "alpine:3.20",
			Labels: map[string]string{"io.gvx.extension": "host-test-ext"},
		},
	}); err != nil {
		t.Fatalf("SaveSandboxes: %v", err)
	}

	const nSnaps = 16
	seedSnaps := func(sbxID string) {
		root := srv.cfg.supervisor.SnapshotRoot(sbxID)
		idx := make(map[string]string, nSnaps)
		for i := range nSnaps {
			id := fmt.Sprintf("snap-%02d", i)
			dir := filepath.Join(root, id)
			if err := os.MkdirAll(dir, 0o700); err != nil {
				t.Fatalf("MkdirAll: %v", err)
			}
			if err := os.WriteFile(filepath.Join(dir, "manifest.json"), []byte(`{"version":1}`), 0o600); err != nil {
				t.Fatalf("WriteFile: %v", err)
			}
			idx[id] = id
		}
		srv.saveSnapshotIndex(sbxID, idx)
	}

	seedSnaps("sbx-race")

	var wg sync.WaitGroup
	errs := make(chan error, nSnaps)
	for i := range nSnaps {
		id := fmt.Sprintf("snap-%02d", i)
		wg.Add(1)
		go func(snapID string) {
			defer wg.Done()
			if err := host.SnapshotDelete(context.Background(), "sbx-race", snapID); err != nil {
				errs <- fmt.Errorf("SnapshotDelete(%s): %w", snapID, err)
			}
		}(id)
	}
	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _ = host.SnapshotList(context.Background(), "sbx-race")
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}

	remaining, err := host.SnapshotList(context.Background(), "sbx-race")
	if err != nil {
		t.Fatalf("SnapshotList(sbx-race): %v", err)
	}
	if len(remaining) != 0 {
		t.Fatalf("lost snapshot index updates under concurrent SnapshotDelete: remaining=%+v", remaining)
	}

	// Race SandboxDelete against concurrent SnapshotDelete + SnapshotList on sbx-del-race.
	seedSnaps("sbx-del-race")
	var delWG sync.WaitGroup
	delWG.Add(1)
	go func() {
		defer delWG.Done()
		_ = host.SandboxDelete(context.Background(), "sbx-del-race")
	}()
	for i := range 6 {
		id := fmt.Sprintf("snap-%02d", i)
		delWG.Add(1)
		go func(snapID string) {
			defer delWG.Done()
			_ = host.SnapshotDelete(context.Background(), "sbx-del-race", snapID)
		}(id)
	}
	delWG.Wait()

	if _, err := os.Stat(srv.snapshotIndexPath("sbx-del-race")); !os.IsNotExist(err) {
		t.Fatalf("snapshot index survived SandboxDelete race: stat err=%v", err)
	}
	if _, err := os.Stat(srv.cfg.supervisor.SnapshotRoot("sbx-del-race")); !os.IsNotExist(err) {
		t.Fatalf("snapshot root survived SandboxDelete race: stat err=%v", err)
	}
}

func TestHostAPI_OpenTerminalOwnershipChecks(t *testing.T) {
	dataDir := t.TempDir()
	extID := "term-owner-ext"
	gs, err := LoadGrantStore(GrantsPath(dataDir))
	if err != nil {
		t.Fatalf("LoadGrantStore: %v", err)
	}
	m := &Manifest{
		SchemaVersion: 1,
		ID:            extID,
		Version:       "1.0.0",
		Publisher:     "acme",
		Name:          "Term Owner",
		Permissions:   []string{PermUITerminal, PermDocker, PermSandboxes},
	}
	if err := gs.Approve(extID, m, ""); err != nil {
		t.Fatalf("Approve: %v", err)
	}
	st := store.Store{Dir: dataDir}
	if err := st.SaveSandboxes([]sandbox.Spec{
		{
			ID:     "sb-owned",
			Name:   "sb-owned",
			Image:  "alpine:3.20",
			Labels: map[string]string{"io.gvx.extension": extID},
		},
		{
			ID:     "sb-foreign",
			Name:   "sb-foreign",
			Image:  "alpine:3.20",
			Labels: map[string]string{"io.gvx.extension": "other-ext"},
		},
	}); err != nil {
		t.Fatalf("SaveSandboxes: %v", err)
	}

	uiHost := &fakeUIHost{}
	gestures := newGestureStore(nil)
	var sandboxesMu sync.Mutex
	srv := newHostAPIServer(hostAPIConfig{
		extID:       extID,
		dataDir:     dataDir,
		token:       "tok-term",
		manifest:    m,
		grants:      gs,
		gestures:    gestures,
		uiHost:      uiHost,
		sandboxesMu: &sandboxesMu,
		dockerEngineHandler: func() http.Handler {
			return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/containers/c-owned/json":
					w.Header().Set("Content-Type", "application/json")
					_, _ = w.Write([]byte(`{"Id":"c-owned","Config":{"Labels":{"io.gvx.extension":"term-owner-ext"}}}`))
				case "/containers/c-foreign/json":
					w.Header().Set("Content-Type", "application/json")
					_, _ = w.Write([]byte(`{"Id":"c-foreign","Config":{"Labels":{"io.gvx.extension":"other-ext"}}}`))
				default:
					http.Error(w, `{"message":"no such container"}`, http.StatusNotFound)
				}
			})
		},
	})
	ep, _, _, err := srv.Listen()
	if err != nil {
		t.Fatalf("srv.Listen: %v", err)
	}
	t.Cleanup(func() { _ = srv.Close() })

	hostClient, err := extsdk.NewHost(ep, "tok-term")
	if err != nil {
		t.Fatalf("NewHost: %v", err)
	}
	gCtx := func() context.Context {
		gid, err := gestures.mint(extID)
		if err != nil {
			t.Fatalf("mint gesture: %v", err)
		}
		return extsdk.WithGestureID(context.Background(), gid)
	}

	// Foreign / missing sandbox and container targets must be rejected with permission_denied (403).
	for _, req := range []extsdk.OpenTerminalRequest{
		{TargetType: "sandbox", TargetID: "sb-foreign", Shell: "/bin/sh"},
		{TargetType: "sandbox", TargetID: "sb-missing", Shell: "/bin/sh"},
		{TargetType: "container", TargetID: "c-foreign", Shell: "/bin/sh"},
		{TargetType: "container", TargetID: "c-missing", Shell: "/bin/sh"},
	} {
		err := hostClient.OpenTerminal(gCtx(), req)
		if !extsdk.IsPermissionDenied(err) {
			t.Fatalf("OpenTerminal(%+v): expected permission_denied, got %v", req, err)
		}
	}

	// Traversal / invalid target_id must be rejected.
	if err := hostClient.OpenTerminal(gCtx(), extsdk.OpenTerminalRequest{
		TargetType: "container", TargetID: "../c-owned", Shell: "/bin/sh",
	}); err == nil {
		t.Fatal("expected traversal target_id to be rejected")
	}

	// Owned sandbox and container succeed.
	if err := hostClient.OpenTerminal(gCtx(), extsdk.OpenTerminalRequest{
		TargetType: "sandbox", TargetID: "sb-owned", Shell: "/bin/sh",
	}); err != nil {
		t.Fatalf("OpenTerminal(sb-owned): %v", err)
	}
	if err := hostClient.OpenTerminal(gCtx(), extsdk.OpenTerminalRequest{
		TargetType: "container", TargetID: "c-owned", Shell: "/bin/sh",
	}); err != nil {
		t.Fatalf("OpenTerminal(c-owned): %v", err)
	}
}

func TestHostAPI_OpenTerminalContainerRequiresPermDockerAndBackend(t *testing.T) {
	// 1. Missing PermDocker must reject TargetType="container" with permission_denied even if Docker backend is nil.
	dataDir := t.TempDir()
	extID := "term-nodocker"
	gs, err := LoadGrantStore(GrantsPath(dataDir))
	if err != nil {
		t.Fatalf("LoadGrantStore: %v", err)
	}
	mNoDocker := &Manifest{
		SchemaVersion: 1,
		ID:            extID,
		Version:       "1.0.0",
		Publisher:     "acme",
		Name:          "Term NoDocker",
		Permissions:   []string{PermUITerminal},
	}
	if err := gs.Approve(extID, mNoDocker, ""); err != nil {
		t.Fatalf("Approve: %v", err)
	}
	uiHost := &fakeUIHost{}
	gestures := newGestureStore(nil)
	srv1 := newHostAPIServer(hostAPIConfig{
		extID:    extID,
		dataDir:  dataDir,
		token:    "tok-1",
		manifest: mNoDocker,
		grants:   gs,
		gestures: gestures,
		uiHost:   uiHost,
	})
	ep1, _, _, err := srv1.Listen()
	if err != nil {
		t.Fatalf("srv1.Listen: %v", err)
	}
	defer srv1.Close()

	cl1, err := extsdk.NewHost(ep1, "tok-1")
	if err != nil {
		t.Fatalf("NewHost: %v", err)
	}
	gid1, _ := gestures.mint(extID)
	err = cl1.OpenTerminal(extsdk.WithGestureID(context.Background(), gid1), extsdk.OpenTerminalRequest{
		TargetType: "container",
		TargetID:   "any-host-container",
		Shell:      "/bin/sh",
	})
	if !extsdk.IsPermissionDenied(err) {
		t.Fatalf("expected permission_denied without PermDocker, got %v", err)
	}
	if len(uiHost.terminals) != 0 {
		t.Fatalf("expected 0 OpenTerminal calls without PermDocker, got %+v", uiHost.terminals)
	}

	// 2. PermDocker granted but Docker backend nil must fail closed (unavailable) without calling uiHost.OpenTerminal.
	extID2 := "term-nobackend"
	mWithDocker := &Manifest{
		SchemaVersion: 1,
		ID:            extID2,
		Version:       "1.0.0",
		Publisher:     "acme",
		Name:          "Term NoBackend",
		Permissions:   []string{PermUITerminal, PermDocker},
	}
	if err := gs.Approve(extID2, mWithDocker, ""); err != nil {
		t.Fatalf("Approve 2: %v", err)
	}
	srv2 := newHostAPIServer(hostAPIConfig{
		extID:    extID2,
		dataDir:  dataDir,
		token:    "tok-2",
		manifest: mWithDocker,
		grants:   gs,
		gestures: gestures,
		uiHost:   uiHost,
	})
	ep2, _, _, err := srv2.Listen()
	if err != nil {
		t.Fatalf("srv2.Listen: %v", err)
	}
	defer srv2.Close()

	cl2, err := extsdk.NewHost(ep2, "tok-2")
	if err != nil {
		t.Fatalf("NewHost 2: %v", err)
	}
	gid2, _ := gestures.mint(extID2)
	err = cl2.OpenTerminal(extsdk.WithGestureID(context.Background(), gid2), extsdk.OpenTerminalRequest{
		TargetType: "container",
		TargetID:   "any-host-container",
		Shell:      "/bin/sh",
	})
	if err == nil {
		t.Fatal("expected error when Docker backend is nil")
	}
	if len(uiHost.terminals) != 0 {
		t.Fatalf("expected 0 OpenTerminal calls with nil Docker backend, got %+v", uiHost.terminals)
	}
}

// Keep unused imports clean if needed.
var _ = net.ErrClosed
var _ = os.ErrNotExist
var _ = filepath.Join
var _ = fmt.Sprint

func TestHostAPI_RejectsSandboxMountVolume(t *testing.T) {
	ui := &fakeUIHost{}
	_, host, _, _, dataDir := setupTestHostAPI(t, []string{PermSandboxes, PermStorage}, ui)
	ctx := context.Background()
	startFalse := false

	for _, src := range []string{
		"gvx-buildkit",
		"user-sandbox-vol",
		"victim-ext-vol",
		"/tmp/evil.ext4",
		"../escape",
		"",
	} {
		_, err := host.SandboxCreate(ctx, extsdk.CreateSandboxRequest{
			Name:   "sbx-vol-attack",
			Image:  "alpine:3.20",
			Mounts: []extsdk.SandboxMount{{Type: "volume", Source: src, Target: "/mnt"}},
			Start:  &startFalse,
		})
		if !extsdk.IsPermissionDenied(err) {
			t.Fatalf("MountVolume(%q): expected 403 permission_denied, got %v", src, err)
		}
	}

	specs, err := (store.Store{Dir: dataDir}).Sandboxes()
	if err != nil {
		t.Fatalf("Sandboxes: %v", err)
	}
	if len(specs) != 0 {
		t.Fatalf("expected 0 persisted sandboxes after rejected MountVolume calls, got %+v", specs)
	}
}

func TestHostAPI_SandboxCreateImageIsolation(t *testing.T) {
	ctx := context.Background()
	startFalse := false

	// 1. Even with nil Docker backend, gvx-ext/<other-id> (with or without docker.io/ prefix) is rejected with 403 permission_denied,
	// while alpine:3.20 and gvx-ext/<own-id> are allowed.
	_, noDockerHost, _, _, noDockerDir := setupTestHostAPI(t, []string{PermSandboxes}, &fakeUIHost{})
	for _, foreignRef := range []string{
		"gvx-ext/ext-victim:1.0.0-local",
		"gvx-ext/ext-victim",
		"gvx-ext/ext-victim@sha256:2222222222222222222222222222222222222222222222222222222222222222",
		"docker.io/gvx-ext/ext-victim:1.0.0-local",
		"gvx-ext/",
	} {
		_, err := noDockerHost.SandboxCreate(ctx, extsdk.CreateSandboxRequest{
			Name:  "sbx-steal",
			Image: foreignRef,
			Start: &startFalse,
		})
		if !extsdk.IsPermissionDenied(err) {
			t.Fatalf("nil backend SandboxCreate(%q): expected 403 permission_denied, got %v", foreignRef, err)
		}
	}
	for _, allowedRef := range []string{
		"alpine:3.20",
		"gvx-ext/host-test-ext:1.0.0-local",
		"docker.io/gvx-ext/host-test-ext:1.0.0-local",
	} {
		sbx, err := noDockerHost.SandboxCreate(ctx, extsdk.CreateSandboxRequest{
			Name:  "sbx-ok",
			Image: allowedRef,
			Start: &startFalse,
		})
		if err != nil {
			t.Fatalf("nil backend SandboxCreate(%q): expected success, got %v", allowedRef, err)
		}
		if err := noDockerHost.SandboxDelete(ctx, sbx.ID); err != nil {
			t.Fatalf("SandboxDelete: %v", err)
		}
	}
	if specs, err := (store.Store{Dir: noDockerDir}).Sandboxes(); err != nil || len(specs) != 0 {
		t.Fatalf("expected 0 remaining sandboxes, got %+v (err=%v)", specs, err)
	}

	// 2. With Docker backend wired (dockerEngineHandler & dockerBackendDial), foreign io.gvx.extension images
	// (victim-img:1, sha256:..., gvx-ext/ext-victim:...) are rejected with 403 permission_denied, invalid inspect JSON fails closed (503),
	// and uncached 404 / unlabeled alpine:3.20 / own images succeed.
	images := map[string]string{
		"/images/alpine:3.20/json":  `{"Id":"sha256:1111","Config":{"Labels":{}}}`,
		"/images/victim-img:1/json": `{"Id":"sha256:2222","Config":{"Labels":{"io.gvx.extension":"ext-victim"}}}`,
		"/images/sha256:2222222222222222222222222222222222222222222222222222222222222222/json": `{"Id":"sha256:2222","Config":{"Labels":{"io.gvx.extension":"ext-victim"}}}`,
		"/images/gvx-ext/ext-victim:1.0.0-local/json":                                          `{"Id":"sha256:3333","Config":{"Labels":{"io.gvx.extension":"ext-victim"}}}`,
		"/images/own-img:1/json":                       `{"Id":"sha256:4444","Config":{"Labels":{"io.gvx.extension":"sbx-img-ext"}}}`,
		"/images/gvx-ext/sbx-img-ext:1.0.0-local/json": `{"Id":"sha256:5555","Config":{"Labels":{"io.gvx.extension":"sbx-img-ext"}}}`,
		"/images/corrupt:1/json":                       `{not-valid-json`,
	}
	dockerHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if body, ok := images[r.URL.Path]; ok {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(body))
			return
		}
		http.Error(w, `{"message":"No such image"}`, http.StatusNotFound)
	})
	dockerSrv := httptest.NewServer(dockerHandler)
	defer dockerSrv.Close()

	for _, mode := range []string{"handler", "dial"} {
		dataDir := t.TempDir()
		extID := "sbx-img-ext"
		gs, err := LoadGrantStore(GrantsPath(dataDir))
		if err != nil {
			t.Fatalf("LoadGrantStore: %v", err)
		}
		m := &Manifest{
			SchemaVersion: 1,
			ID:            extID,
			Version:       "1.0.0",
			Publisher:     "acme",
			Name:          "Sandbox Img Isolation",
			Permissions:   []string{PermSandboxes},
		}
		if err := gs.Approve(extID, m, ""); err != nil {
			t.Fatalf("Approve: %v", err)
		}
		var sandboxesMu sync.Mutex
		cfg := hostAPIConfig{
			extID:       extID,
			dataDir:     dataDir,
			token:       "tok-sbx-img",
			manifest:    m,
			grants:      gs,
			gestures:    newGestureStore(nil),
			uiHost:      &fakeUIHost{},
			supervisor:  &sandbox.Supervisor{DataDir: dataDir},
			sandboxesMu: &sandboxesMu,
		}
		if mode == "handler" {
			cfg.dockerEngineHandler = func() http.Handler { return dockerHandler }
		} else {
			cfg.dockerBackendDial = func(c context.Context) (net.Conn, error) {
				return (&net.Dialer{}).DialContext(c, "tcp", strings.TrimPrefix(dockerSrv.URL, "http://"))
			}
		}
		srv := newHostAPIServer(cfg)
		ep, _, _, err := srv.Listen()
		if err != nil {
			t.Fatalf("Listen(%s): %v", mode, err)
		}
		cl, err := extsdk.NewHost(ep, "tok-sbx-img")
		if err != nil {
			_ = srv.Close()
			t.Fatalf("NewHost(%s): %v", mode, err)
		}

		for _, foreignRef := range []string{
			"victim-img:1",
			"victim-img:1@sha256:2222222222222222222222222222222222222222222222222222222222222222",
			"victim-img:1@sha256:9999999999999999999999999999999999999999999999999999999999999999",
			"alpine:3.20@sha256:2222222222222222222222222222222222222222222222222222222222222222",
			"sha256:2222222222222222222222222222222222222222222222222222222222222222",
			"gvx-ext/ext-victim:1.0.0-local",
			"docker.io/gvx-ext/ext-victim:1.0.0-local@sha256:9999999999999999999999999999999999999999999999999999999999999999",
		} {
			_, err := cl.SandboxCreate(ctx, extsdk.CreateSandboxRequest{
				Name:  "sbx-steal",
				Image: foreignRef,
				Start: &startFalse,
			})
			if !extsdk.IsPermissionDenied(err) {
				_ = srv.Close()
				t.Fatalf("%s SandboxCreate(%q): expected 403 permission_denied, got %v", mode, foreignRef, err)
			}
		}

		// Corrupt 200 JSON from backend must fail closed (503).
		if _, err := cl.SandboxCreate(ctx, extsdk.CreateSandboxRequest{
			Name:  "sbx-corrupt",
			Image: "corrupt:1",
			Start: &startFalse,
		}); err == nil || !strings.Contains(err.Error(), "503") {
			_ = srv.Close()
			t.Fatalf("%s SandboxCreate(corrupt:1): expected 503 fail-closed, got %v", mode, err)
		}

		// Allowed: unlabeled alpine:3.20, uncached 404 busybox:latest, owned own-img:1, owned gvx-ext/sbx-img-ext:1.0.0-local.
		for _, okRef := range []string{
			"alpine:3.20",
			"busybox:latest",
			"own-img:1",
			"gvx-ext/sbx-img-ext:1.0.0-local",
		} {
			sbx, err := cl.SandboxCreate(ctx, extsdk.CreateSandboxRequest{
				Name:  "sbx-ok",
				Image: okRef,
				Start: &startFalse,
			})
			if err != nil {
				_ = srv.Close()
				t.Fatalf("%s SandboxCreate(%q): expected success, got %v", mode, okRef, err)
			}
			_ = cl.SandboxDelete(ctx, sbx.ID)
		}
		if specs, err := (store.Store{Dir: dataDir}).Sandboxes(); err != nil || len(specs) != 0 {
			_ = srv.Close()
			t.Fatalf("%s: expected 0 remaining sandboxes, got %+v (err=%v)", mode, specs, err)
		}
		_ = srv.Close()
	}
}

func buildMinimalFakeRungv(t *testing.T) string {
	t.Helper()
	srcFile := filepath.Join(t.TempDir(), "main.go")
	const fakeRungvSrc = `package main

import (
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"
)

func main() {
	if len(os.Args) < 2 {
		os.Exit(2)
	}
	switch os.Args[1] {
	case "run":
		for _, a := range os.Args[2:] {
			if strings.Contains(a, "help") {
				fmt.Println("  -control-token-file")
				fmt.Println("  -control-socket")
				return
			}
		}
		waitStop()
	case "restore":
		for _, a := range os.Args[2:] {
			if strings.Contains(a, "help") {
				return
			}
		}
		waitStop()
	case "events":
		return
	default:
		return
	}
}

func waitStop() {
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
	done := make(chan struct{})
	go func() {
		_, _ = io.Copy(io.Discard, os.Stdin)
		close(done)
	}()
	select {
	case <-sig:
	case <-done:
	case <-time.After(30 * time.Second):
	}
}
`
	if err := os.WriteFile(srcFile, []byte(fakeRungvSrc), 0o600); err != nil {
		t.Fatal(err)
	}
	exeName := "rungv"
	if runtime.GOOS == "windows" {
		exeName = "rungv.exe"
	}
	bin := filepath.Join(t.TempDir(), exeName)
	if out, err := exec.Command("go", "build", "-o", bin, srcFile).CombinedOutput(); err != nil {
		t.Fatalf("go build fake rungv: %v\n%s", err, out)
	}
	t.Cleanup(func() { rungv.ForgetFeatures(bin) })
	return bin
}

func TestHostAPI_RevokedFolderBlocksStartAndRestore(t *testing.T) {
	fakeRungvBin := buildMinimalFakeRungv(t)
	allowedDir := t.TempDir()
	outsideDir := t.TempDir()
	subDir := filepath.Join(allowedDir, "sub")
	if err := os.MkdirAll(subDir, 0o700); err != nil {
		t.Fatal(err)
	}

	srv, host, _, gs, _ := setupTestHostAPI(t, []string{PermSandboxes, PermSnapshots}, &fakeUIHost{})
	supDir := t.TempDir()
	sup := &sandbox.Supervisor{
		DataDir: supDir,
		Client:  func() *rungv.Client { return &rungv.Client{Path: fakeRungvBin} },
	}
	srv.cfg.supervisor = sup
	t.Cleanup(func() { sup.StopAll(2 * time.Second) })

	if _, err := gs.AddAllowedFolder("host-test-ext", allowedDir); err != nil {
		t.Fatalf("AddAllowedFolder: %v", err)
	}

	ctx := context.Background()
	startFalse := false
	ckptTrue := true

	sbxAllowed, err := host.SandboxCreate(ctx, extsdk.CreateSandboxRequest{
		Name:       "sbx-allowed",
		Image:      "alpine:3.20",
		Mounts:     []extsdk.SandboxMount{{Type: "bind", Source: allowedDir, Target: "/work"}},
		Checkpoint: &ckptTrue,
		Start:      &startFalse,
	})
	if err != nil {
		t.Fatalf("SandboxCreate(allowed): %v", err)
	}
	sbxWorkspace, err := host.SandboxCreate(ctx, extsdk.CreateSandboxRequest{
		Name:      "sbx-workspace",
		Image:     "alpine:3.20",
		Workspace: allowedDir,
		Start:     &startFalse,
	})
	if err != nil {
		t.Fatalf("SandboxCreate(workspace): %v", err)
	}
	sbxFiles, err := host.SandboxCreate(ctx, extsdk.CreateSandboxRequest{
		Name:       "sbx-files",
		Image:      "alpine:3.20",
		Mounts:     []extsdk.SandboxMount{{Type: "bind", Source: "/data/sub", Target: "/data-sub"}},
		Checkpoint: &ckptTrue,
		Start:      &startFalse,
	})
	if err != nil {
		t.Fatalf("SandboxCreate(files): %v", err)
	}

	for _, id := range []string{sbxAllowed.ID, sbxFiles.ID} {
		snapDir := filepath.Join(sup.SnapshotRoot(id), "snap-warm")
		if err := os.MkdirAll(snapDir, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(snapDir, "manifest.json"), []byte(`{"version":1}`), 0o600); err != nil {
			t.Fatal(err)
		}
		srv.saveSnapshotIndex(id, map[string]string{"warm": "snap-warm"})
	}

	// Revoke allowedDir.
	removed, err := gs.RemoveAllowedFolder("host-test-ext", allowedDir)
	if err != nil || !removed {
		t.Fatalf("RemoveAllowedFolder = (%v, %v)", removed, err)
	}

	// Start and Restore for sandboxes referencing revoked allowedDir must fail with 403 permission_denied.
	if _, err := host.SandboxStart(ctx, sbxAllowed.ID); !extsdk.IsPermissionDenied(err) {
		t.Fatalf("SandboxStart(sbxAllowed) after revoke: expected 403 permission_denied, got %v", err)
	}
	if _, err := host.SandboxStart(ctx, sbxWorkspace.ID); !extsdk.IsPermissionDenied(err) {
		t.Fatalf("SandboxStart(sbxWorkspace) after revoke: expected 403 permission_denied, got %v", err)
	}
	if _, err := host.Restore(ctx, sbxAllowed.ID, extsdk.RestoreRequest{Snapshot: "warm"}); !extsdk.IsPermissionDenied(err) {
		t.Fatalf("Restore(sbxAllowed) after revoke: expected 403 permission_denied, got %v", err)
	}

	// Sandbox binding extension-data/<id>/files (/data/sub) remains allowed on Start and Restore.
	if info, err := host.SandboxStart(ctx, sbxFiles.ID); err != nil || info.Status == "stopped" {
		t.Fatalf("SandboxStart(sbxFiles) expected success, got info=%+v err=%v", info, err)
	}
	if res, err := host.Restore(ctx, sbxFiles.ID, extsdk.RestoreRequest{Snapshot: "warm"}); err != nil || res.Sandbox.Status == "stopped" {
		t.Fatalf("Restore(sbxFiles) expected success, got res=%+v err=%v", res, err)
	}
	_, _ = host.SandboxStop(ctx, sbxFiles.ID)

	// Re-grant allowedDir, create sandbox on subdir, swap subdir for an escaping symlink -> Start must return 403.
	if _, err := gs.AddAllowedFolder("host-test-ext", allowedDir); err != nil {
		t.Fatalf("re-AddAllowedFolder: %v", err)
	}
	sbxSub, err := host.SandboxCreate(ctx, extsdk.CreateSandboxRequest{
		Name:       "sbx-sub",
		Image:      "alpine:3.20",
		Mounts:     []extsdk.SandboxMount{{Type: "bind", Source: subDir, Target: "/work"}},
		Checkpoint: &ckptTrue,
		Start:      &startFalse,
	})
	if err != nil {
		t.Fatalf("SandboxCreate(sub): %v", err)
	}
	if err := os.RemoveAll(subDir); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outsideDir, subDir); err != nil {
		t.Fatal(err)
	}
	if _, err := host.SandboxStart(ctx, sbxSub.ID); !extsdk.IsPermissionDenied(err) {
		t.Fatalf("SandboxStart after symlink escape swap: expected 403 permission_denied, got %v", err)
	}

	// Snapshot manifest dynamic bind label pointing outside allowed_folders must be rejected on Restore.
	snapEvilDir := filepath.Join(sup.SnapshotRoot(sbxFiles.ID), "snap-evil")
	if err := os.MkdirAll(snapEvilDir, 0o700); err != nil {
		t.Fatal(err)
	}
	evilManifest := fmt.Sprintf(`{"mounts":[{"type":"bind","dynamic":true,"guest_path":"/evil","label":%q}]}`, outsideDir)
	if err := os.WriteFile(filepath.Join(snapEvilDir, "manifest.json"), []byte(evilManifest), 0o600); err != nil {
		t.Fatal(err)
	}
	srv.saveSnapshotIndex(sbxFiles.ID, map[string]string{"warm": "snap-warm", "evil": "snap-evil"})
	if _, err := host.Restore(ctx, sbxFiles.ID, extsdk.RestoreRequest{Snapshot: "evil"}); !extsdk.IsPermissionDenied(err) {
		t.Fatalf("Restore with unapproved dynamic manifest bind label: expected 403 permission_denied, got %v", err)
	}
}

func TestHostAPI_RestoreRequiresPermSandboxes(t *testing.T) {
	fakeRungvBin := buildMinimalFakeRungv(t)
	// Grant PermSnapshots WITHOUT PermSandboxes.
	srv, host, _, _, dataDir := setupTestHostAPI(t, []string{PermSnapshots}, &fakeUIHost{})
	supDir := t.TempDir()
	sup := &sandbox.Supervisor{
		DataDir: supDir,
		Client:  func() *rungv.Client { return &rungv.Client{Path: fakeRungvBin} },
	}
	srv.cfg.supervisor = sup
	t.Cleanup(func() { sup.StopAll(2 * time.Second) })

	spec := sandbox.Spec{
		ID:         "sbx-snap-only",
		Name:       "snaponly",
		Image:      "alpine:3.20",
		Checkpoint: true,
		CreatedAt:  time.Now(),
		Labels:     map[string]string{"io.gvx.extension": "host-test-ext"},
	}
	if err := (store.Store{Dir: dataDir}).SaveSandboxes([]sandbox.Spec{spec}); err != nil {
		t.Fatalf("SaveSandboxes: %v", err)
	}
	snapDir := filepath.Join(sup.SnapshotRoot(spec.ID), "snap-1")
	if err := os.MkdirAll(snapDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(snapDir, "manifest.json"), []byte(`{"version":1}`), 0o600); err != nil {
		t.Fatal(err)
	}
	srv.saveSnapshotIndex(spec.ID, map[string]string{"snap1": "snap-1"})

	ctx := context.Background()
	if _, err := host.Restore(ctx, spec.ID, extsdk.RestoreRequest{Snapshot: "snap1"}); !extsdk.IsPermissionDenied(err) {
		t.Fatalf("Restore without PermSandboxes: expected 403 permission_denied, got %v", err)
	}
	if _, err := host.Restore(ctx, spec.ID, extsdk.RestoreRequest{
		Snapshot: "snap1",
		Exec:     []string{"echo", "pwn"},
	}); !extsdk.IsPermissionDenied(err) {
		t.Fatalf("Restore+Exec without PermSandboxes: expected 403 permission_denied, got %v", err)
	}
	if st := sup.State(spec.ID); st.Status.Active() {
		t.Fatalf("sandbox %s must not have started without PermSandboxes, state=%v", spec.ID, st.Status)
	}
}

func TestHostAPI_SandboxStartAndRestoreRejectDowngradedNetworkAndDocker(t *testing.T) {
	fakeRungvBin := buildMinimalFakeRungv(t)
	srv, host, _, gs, dataDir := setupTestHostAPI(t, []string{PermSandboxes, PermSnapshots, PermNetwork, PermDocker}, &fakeUIHost{})
	supDir := t.TempDir()
	sup := &sandbox.Supervisor{
		DataDir: supDir,
		Client:  func() *rungv.Client { return &rungv.Client{Path: fakeRungvBin} },
	}
	srv.cfg.supervisor = sup
	srv.cfg.dockerSockFn = func() string { return "unix:///tmp/fake-docker.sock" }
	t.Cleanup(func() { sup.StopAll(2 * time.Second) })

	ctx := context.Background()
	startFalse := false
	ckptTrue := true

	sbxBoth, err := host.SandboxCreate(ctx, extsdk.CreateSandboxRequest{
		Name:            "downgrade-both",
		Image:           "alpine:3.20",
		MountDockerSock: true,
		Checkpoint:      &ckptTrue,
		Start:           &startFalse,
	})
	if err != nil {
		t.Fatalf("SandboxCreate(both): %v", err)
	}
	sbxNetOnly, err := host.SandboxCreate(ctx, extsdk.CreateSandboxRequest{
		Name:            "downgrade-net",
		Image:           "alpine:3.20",
		MountDockerSock: false,
		Checkpoint:      &ckptTrue,
		Start:           &startFalse,
	})
	if err != nil {
		t.Fatalf("SandboxCreate(netOnly): %v", err)
	}

	// Also persist a Windows-style DOCKER_HOST-only spec without UnixForwards.
	winSpec := sandbox.Spec{
		ID:         "sbx-win-docker-env",
		Name:       "downgrade-win-docker",
		Image:      "alpine:3.20",
		Network:    "none",
		Env:        []string{"DOCKER_HOST=http://127.0.0.1:2375"},
		Checkpoint: true,
		CreatedAt:  time.Now(),
		Labels:     map[string]string{"io.gvx.extension": "host-test-ext"},
	}
	srv.lockSandboxes()
	specs, err := srv.sandboxStore().Sandboxes()
	if err == nil {
		specs = append(specs, winSpec)
		err = srv.sandboxStore().SaveSandboxes(specs)
	}
	srv.unlockSandboxes()
	if err != nil {
		t.Fatalf("SaveSandboxes(winSpec): %v", err)
	}

	for _, id := range []string{sbxBoth.ID, sbxNetOnly.ID, winSpec.ID} {
		snapDir := filepath.Join(sup.SnapshotRoot(id), "snap-warm")
		if err := os.MkdirAll(snapDir, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(snapDir, "manifest.json"), []byte(`{"version":1}`), 0o600); err != nil {
			t.Fatal(err)
		}
		srv.saveSnapshotIndex(id, map[string]string{"warm": "snap-warm"})
	}

	// Downgrade extension manifest/grants to [sandboxes, snapshots] only (removing network and docker).
	downgraded := &Manifest{
		SchemaVersion: 1,
		ID:            "host-test-ext",
		Version:       "1.2.4",
		Publisher:     "acme",
		Name:          "Host Test",
		Permissions:   []string{PermSandboxes, PermSnapshots},
	}
	srv.cfg.manifest = downgraded
	if err := gs.Approve("host-test-ext", downgraded, ""); err != nil {
		t.Fatalf("Approve(downgraded): %v", err)
	}

	for _, id := range []string{sbxBoth.ID, sbxNetOnly.ID, winSpec.ID} {
		if _, err := host.SandboxStart(ctx, id); !extsdk.IsPermissionDenied(err) {
			t.Fatalf("SandboxStart(%s) after downgrade: expected 403 permission_denied, got %v", id, err)
		}
		if _, err := host.Restore(ctx, id, extsdk.RestoreRequest{Snapshot: "warm"}); !extsdk.IsPermissionDenied(err) {
			t.Fatalf("Restore(%s) after downgrade: expected 403 permission_denied, got %v", id, err)
		}
		if st := sup.State(id); st.Status.Active() {
			t.Fatalf("sandbox %s started unexpectedly after downgrade: %v", id, st.Status)
		}
	}

	// Re-grant only PermNetwork (still without PermDocker): sbxNetOnly succeeds, sbxBoth & winSpec remain 403.
	netOnlyManifest := &Manifest{
		SchemaVersion: 1,
		ID:            "host-test-ext",
		Version:       "1.2.5",
		Publisher:     "acme",
		Name:          "Host Test",
		Permissions:   []string{PermSandboxes, PermSnapshots, PermNetwork},
	}
	srv.cfg.manifest = netOnlyManifest
	if err := gs.Approve("host-test-ext", netOnlyManifest, ""); err != nil {
		t.Fatalf("Approve(netOnly): %v", err)
	}
	for _, id := range []string{sbxBoth.ID, winSpec.ID} {
		if _, err := host.SandboxStart(ctx, id); !extsdk.IsPermissionDenied(err) {
			t.Fatalf("SandboxStart(%s) without PermDocker: expected 403 permission_denied, got %v", id, err)
		}
		if _, err := host.Restore(ctx, id, extsdk.RestoreRequest{Snapshot: "warm"}); !extsdk.IsPermissionDenied(err) {
			t.Fatalf("Restore(%s) without PermDocker: expected 403 permission_denied, got %v", id, err)
		}
	}
	if info, err := host.SandboxStart(ctx, sbxNetOnly.ID); err != nil || info.Status == "stopped" {
		t.Fatalf("SandboxStart(sbxNetOnly) with PermNetwork restored: info=%+v err=%v", info, err)
	}
	_ = dataDir
}
