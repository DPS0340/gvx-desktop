package localreg

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func sha256Digest(b []byte) string {
	sum := sha256.Sum256(b)
	return "sha256:" + hex.EncodeToString(sum[:])
}

func startTestServer(t *testing.T, onChange func()) *Server {
	t.Helper()
	srv := New(Config{
		Dir:      filepath.Join(t.TempDir(), "registry"),
		Port:     -1,
		OnChange: onChange,
	})
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
	return srv
}

func baseURL(s *Server) string {
	return "http://" + s.Addr()
}

func mustPushBlobMonolithic(t *testing.T, client *http.Client, base, repo string, data []byte) string {
	t.Helper()
	d := sha256Digest(data)
	u := fmt.Sprintf("%s/v2/%s/blobs/uploads/?digest=%s", base, repo, url.QueryEscape(d))
	req, err := http.NewRequest(http.MethodPost, u, bytes.NewReader(data))
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("POST blob upload: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("POST blob upload status = %d, want 201; body=%s", resp.StatusCode, body)
	}
	if got := resp.Header.Get("Docker-Content-Digest"); got != d {
		t.Fatalf("Docker-Content-Digest = %q, want %q", got, d)
	}
	if got := resp.Header.Get(apiVersionHeader); got != apiVersionValue {
		t.Fatalf("api version header = %q, want %q", got, apiVersionValue)
	}
	return d
}

func mustPutManifest(t *testing.T, client *http.Client, base, repo, ref, contentType string, raw []byte) string {
	t.Helper()
	u := fmt.Sprintf("%s/v2/%s/manifests/%s", base, repo, ref)
	req, err := http.NewRequest(http.MethodPut, u, bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("PUT manifest: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("PUT manifest %s/%s status = %d, want 201; body=%s", repo, ref, resp.StatusCode, body)
	}
	d := sha256Digest(raw)
	if got := resp.Header.Get("Docker-Content-Digest"); got != d {
		t.Fatalf("Docker-Content-Digest = %q, want %q", got, d)
	}
	if got := resp.Header.Get("Location"); got != "/v2/"+repo+"/manifests/"+d {
		t.Fatalf("Location = %q, want %q", got, "/v2/"+repo+"/manifests/"+d)
	}
	return d
}

func pushSimpleImage(t *testing.T, client *http.Client, base, repo, tag, osName, arch string, layers ...[]byte) (manifestDigest string, manifestBytes, cfgBytes []byte) {
	t.Helper()
	cfgBytes, _ = json.Marshal(ociImageConfig{OS: osName, Architecture: arch})
	cfgDigest := mustPushBlobMonolithic(t, client, base, repo, cfgBytes)

	layerDescs := make([]ociDescriptor, 0, len(layers))
	for _, l := range layers {
		ld := mustPushBlobMonolithic(t, client, base, repo, l)
		layerDescs = append(layerDescs, ociDescriptor{
			MediaType: "application/vnd.oci.image.layer.v1.tar+gzip",
			Digest:    ld,
			Size:      int64(len(l)),
		})
	}
	m := ociManifest{
		SchemaVersion: 2,
		MediaType:     mediaTypeOCIImageManifest,
		Config: &ociDescriptor{
			MediaType: "application/vnd.oci.image.config.v1+json",
			Digest:    cfgDigest,
			Size:      int64(len(cfgBytes)),
		},
		Layers: layerDescs,
	}
	manifestBytes, _ = json.Marshal(m)
	manifestDigest = mustPutManifest(t, client, base, repo, tag, mediaTypeOCIImageManifest, manifestBytes)
	return manifestDigest, manifestBytes, cfgBytes
}

func decodeOCIErrorCode(t *testing.T, resp *http.Response) string {
	t.Helper()
	if got := resp.Header.Get(apiVersionHeader); got != apiVersionValue {
		t.Errorf("error response missing %s header, got %q", apiVersionHeader, got)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Errorf("error response Content-Type = %q, want application/json*", ct)
	}
	var errResp ociErrorResponse
	if err := json.NewDecoder(resp.Body).Decode(&errResp); err != nil {
		t.Fatalf("decode OCI error response: %v", err)
	}
	if len(errResp.Errors) == 0 {
		t.Fatalf("expected at least one OCI error entry")
	}
	return errResp.Errors[0].Code
}

func TestImageEntryMethods(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	e := ImageEntry{
		Repository: "acme/web",
		Tag:        "v1.2",
		Digest:     "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
		MediaType:  mediaTypeOCIImageManifest,
		Size:       1024,
		Platforms:  []string{"linux/amd64", "linux/arm64"},
		UpdatedAt:  now,
	}

	if got := e.Key(); got != "acme/web:v1.2" {
		t.Fatalf("Key() = %q", got)
	}
	if got := e.Ref(""); got != "acme/web:v1.2" {
		t.Fatalf("Ref(\"\") = %q", got)
	}
	if got := e.Ref("127.0.0.1:5001"); got != "127.0.0.1:5001/acme/web:v1.2" {
		t.Fatalf("Ref(host) = %q", got)
	}
	if got := e.Ref("localhost:5001/"); got != "localhost:5001/acme/web:v1.2" {
		t.Fatalf("Ref(host/) = %q", got)
	}
	if got := e.ShortDigest(); got != "0123456789ab" {
		t.Fatalf("ShortDigest() = %q", got)
	}
	if got := (ImageEntry{Digest: "customdigest12345"}).ShortDigest(); got != "customdigest" {
		t.Fatalf("ShortDigest(custom) = %q", got)
	}
	if got := (ImageEntry{Digest: "short"}).ShortDigest(); got != "short" {
		t.Fatalf("ShortDigest(short) = %q", got)
	}

	same := e
	same.UpdatedAt = now.In(time.FixedZone("KST", 9*3600))
	same.Platforms = []string{"linux/amd64", "linux/arm64"}
	if !e.Equal(same) {
		t.Fatalf("Equal() should be true across timezone representations")
	}

	diff := e
	diff.Platforms = []string{"linux/amd64"}
	if e.Equal(diff) {
		t.Fatalf("Equal() should be false when Platforms differ")
	}
}

func TestServerLifecycleAndDefaults(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "reg")

	// Default port configuration when stopped.
	sDefault := New(Config{Dir: dir})
	if sDefault.Port() != DefaultPort {
		t.Fatalf("Port() = %d, want %d", sDefault.Port(), DefaultPort)
	}
	if sDefault.Running() {
		t.Fatalf("Running() should be false before Start")
	}
	if sDefault.Addr() != "" {
		t.Fatalf("Addr() should be empty before Start, got %q", sDefault.Addr())
	}
	if sDefault.Dir() != dir {
		t.Fatalf("Dir() = %q, want %q", sDefault.Dir(), dir)
	}

	// Explicit port configuration when stopped.
	sExplicit := New(Config{Dir: dir, Port: 5099})
	if sExplicit.Port() != 5099 {
		t.Fatalf("Port() = %d, want 5099", sExplicit.Port())
	}

	// Empty Dir fails Start cleanly.
	sNoDir := New(Config{Port: -1})
	if err := sNoDir.Start(); err == nil {
		t.Fatalf("expected error starting with empty Dir")
	}

	// Pre-populate stale file in uploads/ to verify startup crash-recovery sweep.
	staleFile := filepath.Join(dir, "uploads", "stale-crash-upload")
	if err := os.MkdirAll(filepath.Dir(staleFile), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(staleFile, []byte("leftover"), 0o600); err != nil {
		t.Fatal(err)
	}

	srv := New(Config{Dir: dir, Port: -1})
	if srv.Port() != 0 {
		t.Fatalf("ephemeral Port() while stopped = %d, want 0", srv.Port())
	}
	if err := srv.Start(); err != nil {
		t.Fatalf("Start() failed: %v", err)
	}
	if !srv.Running() {
		t.Fatalf("Running() = false after Start")
	}
	port := srv.Port()
	if port <= 0 {
		t.Fatalf("bound Port() = %d, want > 0", port)
	}
	wantAddr := fmt.Sprintf("127.0.0.1:%d", port)
	if srv.Addr() != wantAddr {
		t.Fatalf("Addr() = %q, want %q", srv.Addr(), wantAddr)
	}

	// Stale file should have been swept on Start.
	if _, err := os.Stat(staleFile); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("expected stale upload file to be swept on Start, err=%v", err)
	}

	// Start() is idempotent.
	if err := srv.Start(); err != nil {
		t.Fatalf("second Start() failed: %v", err)
	}
	if srv.Port() != port {
		t.Fatalf("second Start() changed port from %d to %d", port, srv.Port())
	}

	// Ping GET /v2/ and HEAD /v2/.
	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Get(baseURL(srv) + "/v2/")
	if err != nil {
		t.Fatalf("GET /v2/: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || string(body) != "{}" {
		t.Fatalf("GET /v2/ = %d %q, want 200 {}", resp.StatusCode, body)
	}
	if got := resp.Header.Get(apiVersionHeader); got != apiVersionValue {
		t.Fatalf("GET /v2/ api header = %q", got)
	}

	headResp, err := client.Head(baseURL(srv) + "/v2/")
	if err != nil {
		t.Fatalf("HEAD /v2/: %v", err)
	}
	headBody, _ := io.ReadAll(headResp.Body)
	headResp.Body.Close()
	if headResp.StatusCode != http.StatusOK || len(headBody) != 0 {
		t.Fatalf("HEAD /v2/ = %d len=%d", headResp.StatusCode, len(headBody))
	}

	// Initiate an active upload so we can verify Stop cleans it up.
	uResp, err := client.Post(baseURL(srv)+"/v2/cleanup/blobs/uploads/", "", nil)
	if err != nil {
		t.Fatalf("POST upload init: %v", err)
	}
	uUUID := uResp.Header.Get("Docker-Upload-UUID")
	uResp.Body.Close()
	if uResp.StatusCode != http.StatusAccepted || uUUID == "" {
		t.Fatalf("POST upload init = %d uuid=%q", uResp.StatusCode, uUUID)
	}
	activeUploadFile := filepath.Join(dir, "uploads", uUUID)
	if _, err := os.Stat(activeUploadFile); err != nil {
		t.Fatalf("expected active upload file on disk: %v", err)
	}

	// Stop cleans up active uploads and frees the port.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := srv.Stop(ctx); err != nil {
		t.Fatalf("Stop() failed: %v", err)
	}
	if srv.Running() || srv.Addr() != "" {
		t.Fatalf("after Stop: running=%v addr=%q", srv.Running(), srv.Addr())
	}
	if _, err := os.Stat(activeUploadFile); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("expected active upload file cleaned up on Stop, err=%v", err)
	}

	// Stop is idempotent.
	if err := srv.Stop(ctx); err != nil {
		t.Fatalf("second Stop() failed: %v", err)
	}

	// Restart on the same previously-bound port succeeds (proving port was freed).
	srv2 := New(Config{Dir: dir, Port: port})
	if err := srv2.Start(); err != nil {
		t.Fatalf("re-binding freed port %d failed: %v", port, err)
	}
	_ = srv2.Stop(ctx)
}

func TestEmptyNonExistentDir(t *testing.T) {
	missingDir := filepath.Join(t.TempDir(), "does-not-exist-yet")
	srv := New(Config{Dir: missingDir, Port: -1})

	imgs, err := srv.List()
	if err != nil {
		t.Fatalf("List() on missing dir failed: %v", err)
	}
	if len(imgs) != 0 {
		t.Fatalf("List() = %v, want empty", imgs)
	}

	stats, err := srv.DiskUsage()
	if err != nil {
		t.Fatalf("DiskUsage() on missing dir failed: %v", err)
	}
	if stats != (DiskStats{}) {
		t.Fatalf("DiskUsage() = %+v, want zero", stats)
	}

	gcRes, err := srv.GC()
	if err != nil {
		t.Fatalf("GC() on missing dir failed: %v", err)
	}
	if gcRes != (GCResult{}) {
		t.Fatalf("GC() = %+v, want zero", gcRes)
	}

	if _, err := srv.DeleteTag("app", "latest"); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("DeleteTag() on missing dir err = %v, want fs.ErrNotExist", err)
	}

	// Non-mutating queries should not have created the directory.
	if _, err := os.Stat(missingDir); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("expected missingDir to remain absent, err=%v", err)
	}
}

func TestBlobUploadsAllModesAndRangeDownload(t *testing.T) {
	srv := startTestServer(t, nil)
	client := &http.Client{Timeout: 5 * time.Second}
	base := baseURL(srv)

	// 1. Monolithic POST upload + HEAD + GET + Range download.
	monoData := []byte("0123456789abcdefghij")
	monoDigest := mustPushBlobMonolithic(t, client, base, "myapp", monoData)

	headResp, err := client.Head(fmt.Sprintf("%s/v2/myapp/blobs/%s", base, monoDigest))
	if err != nil {
		t.Fatal(err)
	}
	headResp.Body.Close()
	if headResp.StatusCode != http.StatusOK {
		t.Fatalf("HEAD blob = %d, want 200", headResp.StatusCode)
	}
	if headResp.Header.Get("Docker-Content-Digest") != monoDigest {
		t.Fatalf("HEAD digest = %q", headResp.Header.Get("Docker-Content-Digest"))
	}
	if headResp.Header.Get("Content-Type") != "application/octet-stream" {
		t.Fatalf("HEAD Content-Type = %q", headResp.Header.Get("Content-Type"))
	}
	if headResp.ContentLength != int64(len(monoData)) {
		t.Fatalf("HEAD Content-Length = %d, want %d", headResp.ContentLength, len(monoData))
	}

	// Range GET (bytes 5-9 -> "56789").
	rangeReq, _ := http.NewRequest(http.MethodGet, fmt.Sprintf("%s/v2/myapp/blobs/%s", base, monoDigest), nil)
	rangeReq.Header.Set("Range", "bytes=5-9")
	rangeResp, err := client.Do(rangeReq)
	if err != nil {
		t.Fatal(err)
	}
	rangeBody, _ := io.ReadAll(rangeResp.Body)
	rangeResp.Body.Close()
	if rangeResp.StatusCode != http.StatusPartialContent {
		t.Fatalf("Range GET status = %d, want 206", rangeResp.StatusCode)
	}
	if string(rangeBody) != "56789" {
		t.Fatalf("Range GET body = %q, want %q", rangeBody, "56789")
	}

	// Duplicate monolithic upload of the exact same blob succeeds cleanly (201).
	mustPushBlobMonolithic(t, client, base, "myapp", monoData)

	// 2. Empty (0-byte) blob upload.
	emptyDigest := mustPushBlobMonolithic(t, client, base, "myapp", nil)
	emptyResp, err := client.Get(fmt.Sprintf("%s/v2/myapp/blobs/%s", base, emptyDigest))
	if err != nil {
		t.Fatal(err)
	}
	emptyBody, _ := io.ReadAll(emptyResp.Body)
	emptyResp.Body.Close()
	if emptyResp.StatusCode != http.StatusOK || len(emptyBody) != 0 {
		t.Fatalf("GET empty blob = %d len=%d", emptyResp.StatusCode, len(emptyBody))
	}

	// 3. Streamed upload (POST -> PATCH without Content-Range -> PUT empty body).
	streamData := []byte("streamed blob content payload")
	streamDigest := sha256Digest(streamData)
	initResp, err := client.Post(base+"/v2/myapp/blobs/uploads/", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	initResp.Body.Close()
	if initResp.StatusCode != http.StatusAccepted {
		t.Fatalf("POST upload init = %d, want 202", initResp.StatusCode)
	}
	loc := initResp.Header.Get("Location")
	uuid := initResp.Header.Get("Docker-Upload-UUID")
	if initResp.Header.Get("Range") != "0-0" || uuid == "" || loc == "" {
		t.Fatalf("unexpected init headers: loc=%q uuid=%q range=%q", loc, uuid, initResp.Header.Get("Range"))
	}

	patchReq, _ := http.NewRequest(http.MethodPatch, base+loc, bytes.NewReader(streamData))
	patchResp, err := client.Do(patchReq)
	if err != nil {
		t.Fatal(err)
	}
	patchResp.Body.Close()
	if patchResp.StatusCode != http.StatusAccepted {
		t.Fatalf("PATCH streamed = %d, want 202", patchResp.StatusCode)
	}
	wantRange := fmt.Sprintf("0-%d", len(streamData)-1)
	if got := patchResp.Header.Get("Range"); got != wantRange {
		t.Fatalf("PATCH Range = %q, want %q", got, wantRange)
	}

	putReq, _ := http.NewRequest(http.MethodPut, base+loc+"?digest="+url.QueryEscape(streamDigest), nil)
	putResp, err := client.Do(putReq)
	if err != nil {
		t.Fatal(err)
	}
	putResp.Body.Close()
	if putResp.StatusCode != http.StatusCreated {
		t.Fatalf("PUT finish = %d, want 201", putResp.StatusCode)
	}

	getResp, err := client.Get(fmt.Sprintf("%s/v2/myapp/blobs/%s", base, streamDigest))
	if err != nil {
		t.Fatal(err)
	}
	gotStream, _ := io.ReadAll(getResp.Body)
	getResp.Body.Close()
	if !bytes.Equal(gotStream, streamData) {
		t.Fatalf("downloaded streamed blob = %q, want %q", gotStream, streamData)
	}

	// 4. Multi-chunk upload with Content-Range + GET status + 416 mismatch + final PUT chunk.
	c1, c2, c3 := []byte("hello"), []byte("world"), []byte("1234")
	full := append(append(append([]byte{}, c1...), c2...), c3...)
	fullDigest := sha256Digest(full)

	initResp, err = client.Post(base+"/v2/myapp/blobs/uploads/", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	loc = initResp.Header.Get("Location")
	uuid = initResp.Header.Get("Docker-Upload-UUID")
	initResp.Body.Close()

	// GET status when empty -> 204 Range: 0-0.
	stResp, err := client.Get(base + loc)
	if err != nil {
		t.Fatal(err)
	}
	stResp.Body.Close()
	if stResp.StatusCode != http.StatusNoContent || stResp.Header.Get("Range") != "0-0" {
		t.Fatalf("GET empty upload = %d range=%q", stResp.StatusCode, stResp.Header.Get("Range"))
	}

	// PATCH chunk 1 (0-4).
	p1, _ := http.NewRequest(http.MethodPatch, base+loc, bytes.NewReader(c1))
	p1.Header.Set("Content-Range", "0-4")
	r1, err := client.Do(p1)
	if err != nil {
		t.Fatal(err)
	}
	r1.Body.Close()
	if r1.StatusCode != http.StatusAccepted || r1.Header.Get("Range") != "0-4" {
		t.Fatalf("PATCH chunk 1 = %d range=%q", r1.StatusCode, r1.Header.Get("Range"))
	}

	// GET status after chunk 1 -> 204 Range: 0-4.
	stResp, err = client.Get(base + loc)
	if err != nil {
		t.Fatal(err)
	}
	stResp.Body.Close()
	if stResp.StatusCode != http.StatusNoContent || stResp.Header.Get("Range") != "0-4" || stResp.Header.Get("Docker-Upload-UUID") != uuid {
		t.Fatalf("GET upload after chunk 1 = %d range=%q uuid=%q", stResp.StatusCode, stResp.Header.Get("Range"), stResp.Header.Get("Docker-Upload-UUID"))
	}

	// PATCH with out-of-order Content-Range -> 416.
	pBad, _ := http.NewRequest(http.MethodPatch, base+loc, bytes.NewReader(c2))
	pBad.Header.Set("Content-Range", "10-14")
	rBad, err := client.Do(pBad)
	if err != nil {
		t.Fatal(err)
	}
	rBad.Body.Close()
	if rBad.StatusCode != http.StatusRequestedRangeNotSatisfiable {
		t.Fatalf("out-of-order PATCH = %d, want 416", rBad.StatusCode)
	}

	// PATCH chunk 2 (5-9) still succeeds after 416.
	p2, _ := http.NewRequest(http.MethodPatch, base+loc, bytes.NewReader(c2))
	p2.Header.Set("Content-Range", "5-9")
	r2, err := client.Do(p2)
	if err != nil {
		t.Fatal(err)
	}
	r2.Body.Close()
	if r2.StatusCode != http.StatusAccepted || r2.Header.Get("Range") != "0-9" {
		t.Fatalf("PATCH chunk 2 = %d range=%q", r2.StatusCode, r2.Header.Get("Range"))
	}

	// PUT with final chunk c3.
	pFin, _ := http.NewRequest(http.MethodPut, base+loc+"?digest="+url.QueryEscape(fullDigest), bytes.NewReader(c3))
	rFin, err := client.Do(pFin)
	if err != nil {
		t.Fatal(err)
	}
	rFin.Body.Close()
	if rFin.StatusCode != http.StatusCreated {
		t.Fatalf("PUT final chunk = %d, want 201", rFin.StatusCode)
	}

	getResp, err = client.Get(fmt.Sprintf("%s/v2/myapp/blobs/%s", base, fullDigest))
	if err != nil {
		t.Fatal(err)
	}
	gotFull, _ := io.ReadAll(getResp.Body)
	getResp.Body.Close()
	if !bytes.Equal(gotFull, full) {
		t.Fatalf("multi-chunk blob = %q, want %q", gotFull, full)
	}

	// 5. DELETE cancel upload.
	initResp, err = client.Post(base+"/v2/myapp/blobs/uploads/", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	cancelLoc := initResp.Header.Get("Location")
	cancelUUID := initResp.Header.Get("Docker-Upload-UUID")
	initResp.Body.Close()

	delReq, _ := http.NewRequest(http.MethodDelete, base+cancelLoc, nil)
	delResp, err := client.Do(delReq)
	if err != nil {
		t.Fatal(err)
	}
	delResp.Body.Close()
	if delResp.StatusCode != http.StatusNoContent {
		t.Fatalf("DELETE upload = %d, want 204", delResp.StatusCode)
	}
	if _, err := os.Stat(filepath.Join(srv.Dir(), "uploads", cancelUUID)); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("canceled upload temp file still exists: %v", err)
	}
	afterCancel, err := client.Get(base + cancelLoc)
	if err != nil {
		t.Fatal(err)
	}
	code := decodeOCIErrorCode(t, afterCancel)
	afterCancel.Body.Close()
	if afterCancel.StatusCode != http.StatusNotFound || code != "BLOB_UPLOAD_UNKNOWN" {
		t.Fatalf("GET after cancel = %d %q", afterCancel.StatusCode, code)
	}

	// 6. DELETE blob -> 202, then 404 BLOB_UNKNOWN.
	delBlobReq, _ := http.NewRequest(http.MethodDelete, fmt.Sprintf("%s/v2/myapp/blobs/%s", base, fullDigest), nil)
	delBlobResp, err := client.Do(delBlobReq)
	if err != nil {
		t.Fatal(err)
	}
	delBlobResp.Body.Close()
	if delBlobResp.StatusCode != http.StatusAccepted {
		t.Fatalf("DELETE blob = %d, want 202", delBlobResp.StatusCode)
	}
	missingResp, err := client.Get(fmt.Sprintf("%s/v2/myapp/blobs/%s", base, fullDigest))
	if err != nil {
		t.Fatal(err)
	}
	code = decodeOCIErrorCode(t, missingResp)
	missingResp.Body.Close()
	if missingResp.StatusCode != http.StatusNotFound || code != "BLOB_UNKNOWN" {
		t.Fatalf("GET deleted blob = %d %q", missingResp.StatusCode, code)
	}
}

func TestCrossRepoBlobMount(t *testing.T) {
	srv := startTestServer(t, nil)
	client := &http.Client{Timeout: 5 * time.Second}
	base := baseURL(srv)

	blob := []byte("shared cross-repo layer bytes")
	digest := mustPushBlobMonolithic(t, client, base, "team/source", blob)

	// Hit -> 201 Created.
	mountURL := fmt.Sprintf("%s/v2/team/dest/blobs/uploads/?mount=%s&from=team/source", base, url.QueryEscape(digest))
	resp, err := client.Post(mountURL, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("mount hit status = %d, want 201", resp.StatusCode)
	}
	if got := resp.Header.Get("Location"); got != "/v2/team/dest/blobs/"+digest {
		t.Fatalf("mount Location = %q", got)
	}
	if got := resp.Header.Get("Docker-Content-Digest"); got != digest {
		t.Fatalf("mount Docker-Content-Digest = %q", got)
	}

	// Miss -> fallback to 202 Accepted upload init.
	missingData := []byte("not yet present")
	missingDigest := sha256Digest(missingData)
	missURL := fmt.Sprintf("%s/v2/team/dest/blobs/uploads/?mount=%s&from=team/source", base, url.QueryEscape(missingDigest))
	missResp, err := client.Post(missURL, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	missResp.Body.Close()
	if missResp.StatusCode != http.StatusAccepted {
		t.Fatalf("mount miss status = %d, want 202", missResp.StatusCode)
	}
	loc := missResp.Header.Get("Location")
	if loc == "" || missResp.Header.Get("Docker-Upload-UUID") == "" || missResp.Header.Get("Range") != "0-0" {
		t.Fatalf("mount miss missing upload headers: loc=%q uuid=%q range=%q",
			loc, missResp.Header.Get("Docker-Upload-UUID"), missResp.Header.Get("Range"))
	}

	// Completing the fallback upload works.
	finReq, _ := http.NewRequest(http.MethodPut, base+loc+"?digest="+url.QueryEscape(missingDigest), bytes.NewReader(missingData))
	finResp, err := client.Do(finReq)
	if err != nil {
		t.Fatal(err)
	}
	finResp.Body.Close()
	if finResp.StatusCode != http.StatusCreated {
		t.Fatalf("completing mount fallback upload = %d, want 201", finResp.StatusCode)
	}
}

func TestValidationAndErrors(t *testing.T) {
	srv := startTestServer(t, nil)
	client := &http.Client{Timeout: 5 * time.Second}
	base := baseURL(srv)

	// 1. Digest mismatch on monolithic POST -> 400 DIGEST_INVALID.
	wrongDigest := sha256Digest([]byte("other"))
	resp, err := client.Post(fmt.Sprintf("%s/v2/app/blobs/uploads/?digest=%s", base, url.QueryEscape(wrongDigest)), "", bytes.NewReader([]byte("actual")))
	if err != nil {
		t.Fatal(err)
	}
	code := decodeOCIErrorCode(t, resp)
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest || code != "DIGEST_INVALID" {
		t.Fatalf("monolithic mismatch = %d %q, want 400 DIGEST_INVALID", resp.StatusCode, code)
	}

	// 2. Digest mismatch on chunked PUT -> 400 DIGEST_INVALID & cleans up temp file.
	initResp, err := client.Post(base+"/v2/app/blobs/uploads/", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	loc := initResp.Header.Get("Location")
	uuid := initResp.Header.Get("Docker-Upload-UUID")
	initResp.Body.Close()

	putReq, _ := http.NewRequest(http.MethodPut, base+loc+"?digest="+url.QueryEscape(wrongDigest), bytes.NewReader([]byte("actual")))
	putResp, err := client.Do(putReq)
	if err != nil {
		t.Fatal(err)
	}
	code = decodeOCIErrorCode(t, putResp)
	putResp.Body.Close()
	if putResp.StatusCode != http.StatusBadRequest || code != "DIGEST_INVALID" {
		t.Fatalf("chunked mismatch = %d %q, want 400 DIGEST_INVALID", putResp.StatusCode, code)
	}
	if _, err := os.Stat(filepath.Join(srv.Dir(), "uploads", uuid)); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("expected temp upload file removed after mismatch, err=%v", err)
	}

	// 3. Missing config/layer blob on manifest PUT -> 400 BLOB_UNKNOWN.
	missingBlobDigest := sha256Digest([]byte("never uploaded"))
	badManifest, _ := json.Marshal(ociManifest{
		SchemaVersion: 2,
		MediaType:     mediaTypeOCIImageManifest,
		Config:        &ociDescriptor{Digest: missingBlobDigest, Size: 14},
	})
	mReq, _ := http.NewRequest(http.MethodPut, base+"/v2/app/manifests/v1", bytes.NewReader(badManifest))
	mResp, err := client.Do(mReq)
	if err != nil {
		t.Fatal(err)
	}
	code = decodeOCIErrorCode(t, mResp)
	mResp.Body.Close()
	if mResp.StatusCode != http.StatusBadRequest || code != "BLOB_UNKNOWN" {
		t.Fatalf("missing config blob PUT = %d %q, want 400 BLOB_UNKNOWN", mResp.StatusCode, code)
	}

	// Present config, missing layer -> 400 BLOB_UNKNOWN.
	cfgDigest := mustPushBlobMonolithic(t, client, base, "app", []byte(`{"os":"linux","architecture":"amd64"}`))
	badLayerManifest, _ := json.Marshal(ociManifest{
		SchemaVersion: 2,
		MediaType:     mediaTypeOCIImageManifest,
		Config:        &ociDescriptor{Digest: cfgDigest, Size: 37},
		Layers:        []ociDescriptor{{Digest: missingBlobDigest, Size: 14}},
	})
	mReq, _ = http.NewRequest(http.MethodPut, base+"/v2/app/manifests/v1", bytes.NewReader(badLayerManifest))
	mResp, err = client.Do(mReq)
	if err != nil {
		t.Fatal(err)
	}
	code = decodeOCIErrorCode(t, mResp)
	mResp.Body.Close()
	if mResp.StatusCode != http.StatusBadRequest || code != "BLOB_UNKNOWN" {
		t.Fatalf("missing layer blob PUT = %d %q, want 400 BLOB_UNKNOWN", mResp.StatusCode, code)
	}

	// Missing child manifest in index -> 400 BLOB_UNKNOWN.
	badIndex, _ := json.Marshal(ociManifest{
		SchemaVersion: 2,
		MediaType:     "application/vnd.oci.image.index.v1+json",
		Manifests:     []ociDescriptor{{Digest: missingBlobDigest, Size: 14}},
	})
	mReq, _ = http.NewRequest(http.MethodPut, base+"/v2/app/manifests/idx", bytes.NewReader(badIndex))
	mResp, err = client.Do(mReq)
	if err != nil {
		t.Fatal(err)
	}
	code = decodeOCIErrorCode(t, mResp)
	mResp.Body.Close()
	if mResp.StatusCode != http.StatusBadRequest || code != "BLOB_UNKNOWN" {
		t.Fatalf("missing index child PUT = %d %q, want 400 BLOB_UNKNOWN", mResp.StatusCode, code)
	}

	// 4. Digest mismatch on manifest PUT by digest -> 400 DIGEST_INVALID.
	validManifest, _ := json.Marshal(ociManifest{
		SchemaVersion: 2,
		MediaType:     mediaTypeOCIImageManifest,
		Config:        &ociDescriptor{Digest: cfgDigest, Size: 37},
	})
	mReq, _ = http.NewRequest(http.MethodPut, base+"/v2/app/manifests/"+wrongDigest, bytes.NewReader(validManifest))
	mResp, err = client.Do(mReq)
	if err != nil {
		t.Fatal(err)
	}
	code = decodeOCIErrorCode(t, mResp)
	mResp.Body.Close()
	if mResp.StatusCode != http.StatusBadRequest || code != "DIGEST_INVALID" {
		t.Fatalf("manifest digest mismatch = %d %q, want 400 DIGEST_INVALID", mResp.StatusCode, code)
	}

	// 5. Invalid JSON bodies on manifest PUT -> 400 MANIFEST_INVALID.
	for _, body := range []string{"", "not json", "null", "[1,2,3]", `"string"`} {
		mReq, _ = http.NewRequest(http.MethodPut, base+"/v2/app/manifests/badjson", strings.NewReader(body))
		mResp, err = client.Do(mReq)
		if err != nil {
			t.Fatal(err)
		}
		code = decodeOCIErrorCode(t, mResp)
		mResp.Body.Close()
		if mResp.StatusCode != http.StatusBadRequest || code != "MANIFEST_INVALID" {
			t.Fatalf("invalid manifest %q = %d %q, want 400 MANIFEST_INVALID", body, mResp.StatusCode, code)
		}
	}

	// 6. Path traversal & invalid repo/tag/digest/uuid tested directly via Handler()
	// so URL normalization cannot mask raw traversal attempts.
	h := srv.Handler()
	badPaths := []struct {
		method  string
		path    string
		rawPath string
	}{
		{http.MethodGet, "/v2/../etc/manifests/latest", ""},
		{http.MethodGet, "/v2/a/../b/manifests/latest", ""},
		{http.MethodGet, "/v2/a/b/manifests/..", ""},
		{http.MethodGet, "/v2/a/b/manifests/latest", "/v2/a%2f..%2fb/manifests/latest"},
		{http.MethodGet, "/v2/a/b/manifests/latest", "/v2/a/%2e%2e/manifests/latest"},
		{http.MethodGet, "/v2/a\\b/manifests/latest", ""},
		{http.MethodGet, "/v2/UPPER/manifests/latest", ""},
		{http.MethodGet, "/v2/bad..repo/manifests/latest", ""},
		{http.MethodGet, "/v2/-leading/manifests/latest", ""},
		{http.MethodGet, "/v2/trailing-/manifests/latest", ""},
		{http.MethodGet, "/v2/a//b/manifests/latest", ""},
		{http.MethodGet, "/v2/app/manifests/.dot", ""},
		{http.MethodGet, "/v2/app/manifests/-dash", ""},
		{http.MethodGet, "/v2/app/manifests/" + strings.Repeat("a", 129), ""},
		{http.MethodGet, "/v2/app/blobs/not-a-digest", ""},
		{http.MethodGet, "/v2/app/blobs/sha256:short", ""},
		{http.MethodGet, "/v2/app/blobs/sha256:" + strings.Repeat("A", 64), ""},
		{http.MethodGet, "/v2/app/manifests/sha256:bad", ""},
		{http.MethodGet, "/v2/app/blobs/uploads/../../evil", ""},
		{http.MethodGet, "/v2/app/blobs/uploads/not-a-uuid", ""},
	}
	for _, tc := range badPaths {
		req := httptest.NewRequest(tc.method, "http://127.0.0.1"+tc.path, nil)
		if tc.rawPath != "" {
			req.URL.RawPath = tc.rawPath
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("%s path=%q rawPath=%q: status = %d, want 400 (body=%s)",
				tc.method, tc.path, tc.rawPath, rec.Code, rec.Body.String())
		}
		if rec.Header().Get(apiVersionHeader) != apiVersionValue {
			t.Errorf("%s path=%q missing api version header", tc.method, tc.path)
		}
	}
}

func TestReservedSegmentAndDeepRepoNames(t *testing.T) {
	srv := startTestServer(t, nil)
	client := &http.Client{Timeout: 5 * time.Second}
	base := baseURL(srv)

	cases := []struct {
		repo string
		tag  string
	}{
		{"app", "bar"},
		{"app/bar", "latest"},
		{"org/sub/tags/app", "v1.0"},
		{"org/sub/tags/app", "V1.0"}, // tests case-sensitive tag safety on macOS/Windows
		{"a/blobs/b", "v1"},
		{"a/manifests/b", "v1"},
		{"a/blobs/uploads", "v1"},
	}

	digests := make(map[string]string)
	for i, tc := range cases {
		layer := []byte(fmt.Sprintf("layer-%d-%s-%s", i, tc.repo, tc.tag))
		d, _, _ := pushSimpleImage(t, client, base, tc.repo, tc.tag, "linux", "arm64", layer)
		digests[tc.repo+":"+tc.tag] = d
	}

	// Verify GET manifest + tags/list for each.
	for _, tc := range cases {
		resp, err := client.Get(fmt.Sprintf("%s/v2/%s/manifests/%s", base, tc.repo, tc.tag))
		if err != nil {
			t.Fatalf("GET %s:%s: %v", tc.repo, tc.tag, err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("GET %s:%s status = %d, want 200", tc.repo, tc.tag, resp.StatusCode)
		}
		if got := resp.Header.Get("Docker-Content-Digest"); got != digests[tc.repo+":"+tc.tag] {
			t.Fatalf("GET %s:%s digest = %q, want %q", tc.repo, tc.tag, got, digests[tc.repo+":"+tc.tag])
		}
	}

	// Verify tags/list on org/sub/tags/app has both V1.0 and v1.0.
	resp, err := client.Get(base + "/v2/org/sub/tags/app/tags/list")
	if err != nil {
		t.Fatal(err)
	}
	var tl struct {
		Name string   `json:"name"`
		Tags []string `json:"tags"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&tl); err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if tl.Name != "org/sub/tags/app" || !reflect.DeepEqual(tl.Tags, []string{"V1.0", "v1.0"}) {
		t.Fatalf("tags/list = %+v, want name=org/sub/tags/app tags=[V1.0 v1.0]", tl)
	}

	// Verify List() returns all 7 entries.
	imgs, err := srv.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(imgs) != len(cases) {
		t.Fatalf("List() returned %d images, want %d", len(imgs), len(cases))
	}
}

func TestSingleArchManifestAndRawBytePreservation(t *testing.T) {
	srv := startTestServer(t, nil)
	client := &http.Client{Timeout: 5 * time.Second}
	base := baseURL(srv)

	cfgBytes := []byte(`{"architecture":"arm64","os":"linux","variant":"v8"}`)
	layer1 := []byte("layer-one-bytes")
	layer2 := []byte("layer-two-bytes-longer")
	cfgDigest := mustPushBlobMonolithic(t, client, base, "demo/single", cfgBytes)
	l1Digest := mustPushBlobMonolithic(t, client, base, "demo/single", layer1)
	l2Digest := mustPushBlobMonolithic(t, client, base, "demo/single", layer2)

	// Non-canonical JSON with extra whitespace and duplicate layer digest.
	rawManifest := []byte(fmt.Sprintf(`{
  "schemaVersion":   2,
  "mediaType": "application/vnd.docker.distribution.manifest.v2+json",
  "config": {
      "mediaType": "application/vnd.docker.container.image.v1+json",
      "size": %d,
      "digest": %q
  },
  "layers": [
      {"mediaType": "application/vnd.docker.image.rootfs.diff.tar.gzip", "size": %d, "digest": %q},
      {"mediaType": "application/vnd.docker.image.rootfs.diff.tar.gzip", "size": %d, "digest": %q},
      {"mediaType": "application/vnd.docker.image.rootfs.diff.tar.gzip", "size": %d, "digest": %q}
  ]
}
`, len(cfgBytes), cfgDigest, len(layer1), l1Digest, len(layer1), l1Digest, len(layer2), l2Digest))

	// Omit Content-Type header to verify fallback to manifest's JSON mediaType.
	mDigest := mustPutManifest(t, client, base, "demo/single", "v1", "", rawManifest)

	// Verify GET by tag and GET by digest both preserve exact raw bytes.
	for _, ref := range []string{"v1", mDigest} {
		resp, err := client.Get(fmt.Sprintf("%s/v2/demo/single/manifests/%s", base, ref))
		if err != nil {
			t.Fatal(err)
		}
		gotBody, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("GET manifest %s = %d", ref, resp.StatusCode)
		}
		if !bytes.Equal(gotBody, rawManifest) {
			t.Fatalf("GET manifest %s bytes not preserved byte-for-byte", ref)
		}
		if ct := resp.Header.Get("Content-Type"); ct != "application/vnd.docker.distribution.manifest.v2+json" {
			t.Fatalf("Content-Type = %q", ct)
		}
		if d := resp.Header.Get("Docker-Content-Digest"); d != mDigest {
			t.Fatalf("Docker-Content-Digest = %q, want %q", d, mDigest)
		}
	}

	// Verify List() extracts platform from config blob and deduplicates layer1 in Size.
	imgs, err := srv.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(imgs) != 1 {
		t.Fatalf("List() len = %d, want 1", len(imgs))
	}
	img := imgs[0]
	wantSize := int64(len(rawManifest) + len(cfgBytes) + len(layer1) + len(layer2))
	if img.Size != wantSize {
		t.Fatalf("Size = %d, want %d", img.Size, wantSize)
	}
	if !reflect.DeepEqual(img.Platforms, []string{"linux/arm64/v8"}) {
		t.Fatalf("Platforms = %v, want [linux/arm64/v8]", img.Platforms)
	}
	if img.MediaType != "application/vnd.docker.distribution.manifest.v2+json" {
		t.Fatalf("MediaType = %q", img.MediaType)
	}
}

func TestMultiArchOCIIndexAndDockerManifestList(t *testing.T) {
	srv := startTestServer(t, nil)
	client := &http.Client{Timeout: 5 * time.Second}
	base := baseURL(srv)

	repo := "acme/multi"
	sharedLayer := []byte("shared-base-layer-1234567890")
	amdLayer := []byte("amd64-layer-payload")
	armLayer := []byte("arm64-layer-payload-extra")
	attLayer := []byte(`{"predicateType":"https://slsa.dev/provenance/v0.2"}`)

	sharedDigest := mustPushBlobMonolithic(t, client, base, repo, sharedLayer)
	amdLayerDigest := mustPushBlobMonolithic(t, client, base, repo, amdLayer)
	armLayerDigest := mustPushBlobMonolithic(t, client, base, repo, armLayer)
	attLayerDigest := mustPushBlobMonolithic(t, client, base, repo, attLayer)

	amdCfg := []byte(`{"os":"linux","architecture":"amd64"}`)
	armCfg := []byte(`{"os":"linux","architecture":"arm64","variant":"v8"}`)
	attCfg := []byte(`{"os":"unknown","architecture":"unknown"}`)
	amdCfgDigest := mustPushBlobMonolithic(t, client, base, repo, amdCfg)
	armCfgDigest := mustPushBlobMonolithic(t, client, base, repo, armCfg)
	attCfgDigest := mustPushBlobMonolithic(t, client, base, repo, attCfg)

	amdManifest, _ := json.Marshal(ociManifest{
		SchemaVersion: 2,
		MediaType:     mediaTypeOCIImageManifest,
		Config:        &ociDescriptor{Digest: amdCfgDigest, Size: int64(len(amdCfg))},
		Layers: []ociDescriptor{
			{Digest: sharedDigest, Size: int64(len(sharedLayer))},
			{Digest: amdLayerDigest, Size: int64(len(amdLayer))},
		},
	})
	armManifest, _ := json.Marshal(ociManifest{
		SchemaVersion: 2,
		MediaType:     mediaTypeOCIImageManifest,
		Config:        &ociDescriptor{Digest: armCfgDigest, Size: int64(len(armCfg))},
		Layers: []ociDescriptor{
			{Digest: sharedDigest, Size: int64(len(sharedLayer))},
			{Digest: armLayerDigest, Size: int64(len(armLayer))},
		},
	})
	attManifest, _ := json.Marshal(ociManifest{
		SchemaVersion: 2,
		MediaType:     mediaTypeOCIImageManifest,
		Config:        &ociDescriptor{Digest: attCfgDigest, Size: int64(len(attCfg))},
		Layers:        []ociDescriptor{{Digest: attLayerDigest, Size: int64(len(attLayer))}},
	})

	amdDigest := mustPutManifest(t, client, base, repo, sha256Digest(amdManifest), mediaTypeOCIImageManifest, amdManifest)
	armDigest := mustPutManifest(t, client, base, repo, sha256Digest(armManifest), mediaTypeOCIImageManifest, armManifest)
	attDigest := mustPutManifest(t, client, base, repo, sha256Digest(attManifest), mediaTypeOCIImageManifest, attManifest)

	// Push OCI Image Index with amd64, arm64/v8, and unknown/unknown attestation.
	ociIndexMT := "application/vnd.oci.image.index.v1+json"
	indexBytes, _ := json.Marshal(ociManifest{
		SchemaVersion: 2,
		MediaType:     ociIndexMT,
		Manifests: []ociDescriptor{
			{
				MediaType: mediaTypeOCIImageManifest,
				Digest:    armDigest,
				Size:      int64(len(armManifest)),
				Platform:  &ociPlatform{OS: "linux", Architecture: "arm64", Variant: "v8"},
			},
			{
				MediaType: mediaTypeOCIImageManifest,
				Digest:    amdDigest,
				Size:      int64(len(amdManifest)),
				Platform:  &ociPlatform{OS: "linux", Architecture: "amd64"},
			},
			{
				MediaType:   mediaTypeOCIImageManifest,
				Digest:      attDigest,
				Size:        int64(len(attManifest)),
				Platform:    &ociPlatform{OS: "unknown", Architecture: "unknown"},
				Annotations: map[string]string{"vnd.docker.reference.type": "attestation-manifest"},
			},
		},
	})
	mustPutManifest(t, client, base, repo, "oci", ociIndexMT, indexBytes)

	// Also push Docker Manifest List referencing amd64 and arm64.
	dockerListMT := "application/vnd.docker.distribution.manifest.list.v2+json"
	listBytes, _ := json.Marshal(ociManifest{
		SchemaVersion: 2,
		MediaType:     dockerListMT,
		Manifests: []ociDescriptor{
			{
				MediaType: mediaTypeOCIImageManifest,
				Digest:    amdDigest,
				Size:      int64(len(amdManifest)),
				Platform:  &ociPlatform{OS: "linux", Architecture: "amd64"},
			},
			{
				MediaType: mediaTypeOCIImageManifest,
				Digest:    armDigest,
				Size:      int64(len(armManifest)),
				Platform:  &ociPlatform{OS: "linux", Architecture: "arm64", Variant: "v8"},
			},
		},
	})
	mustPutManifest(t, client, base, repo, "docker", dockerListMT, listBytes)

	imgs, err := srv.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(imgs) != 2 {
		t.Fatalf("List() len = %d, want 2", len(imgs))
	}
	byTag := map[string]ImageEntry{}
	for _, img := range imgs {
		byTag[img.Tag] = img
	}

	// OCI index: sorted platforms without unknown/unknown, and total unique size
	// includes index + all 3 child manifests + all 7 unique blobs (sharedLayer once!).
	ociImg := byTag["oci"]
	if !reflect.DeepEqual(ociImg.Platforms, []string{"linux/amd64", "linux/arm64/v8"}) {
		t.Fatalf("oci Platforms = %v, want [linux/amd64 linux/arm64/v8]", ociImg.Platforms)
	}
	wantOCISize := int64(len(indexBytes) + len(amdManifest) + len(armManifest) + len(attManifest) +
		len(sharedLayer) + len(amdLayer) + len(armLayer) + len(attLayer) +
		len(amdCfg) + len(armCfg) + len(attCfg))
	if ociImg.Size != wantOCISize {
		t.Fatalf("oci Size = %d, want %d", ociImg.Size, wantOCISize)
	}

	dockerImg := byTag["docker"]
	if !reflect.DeepEqual(dockerImg.Platforms, []string{"linux/amd64", "linux/arm64/v8"}) {
		t.Fatalf("docker Platforms = %v", dockerImg.Platforms)
	}
	wantDockerSize := int64(len(listBytes) + len(amdManifest) + len(armManifest) +
		len(sharedLayer) + len(amdLayer) + len(armLayer) +
		len(amdCfg) + len(armCfg))
	if dockerImg.Size != wantDockerSize {
		t.Fatalf("docker Size = %d, want %d", dockerImg.Size, wantDockerSize)
	}

	// GC must retain EVERY manifest (including attestation) and EVERY blob!
	gcRes, err := srv.GC()
	if err != nil {
		t.Fatal(err)
	}
	if gcRes.RemovedBlobs != 0 || gcRes.RemovedManifests != 0 {
		t.Fatalf("GC() unexpectedly removed live multi-arch/attestation objects: %+v", gcRes)
	}

	// Attestation manifest and its layer are still downloadable after GC.
	attResp, err := client.Get(fmt.Sprintf("%s/v2/%s/manifests/%s", base, repo, attDigest))
	if err != nil {
		t.Fatal(err)
	}
	attResp.Body.Close()
	if attResp.StatusCode != http.StatusOK {
		t.Fatalf("attestation manifest missing after GC: status=%d", attResp.StatusCode)
	}
}

func parseNextLink(hdr string) string {
	// Parses `</v2/...>; rel="next"`.
	start := strings.Index(hdr, "<")
	end := strings.Index(hdr, ">")
	if start < 0 || end <= start {
		return ""
	}
	return hdr[start+1 : end]
}

func TestCatalogAndTagsListPagination(t *testing.T) {
	srv := startTestServer(t, nil)
	client := &http.Client{Timeout: 5 * time.Second}
	base := baseURL(srv)

	repos := []string{"epsilon", "alpha", "delta", "beta", "gamma"}
	for _, r := range repos {
		pushSimpleImage(t, client, base, r, "v1", "linux", "amd64", []byte("layer-"+r))
	}
	for _, tag := range []string{"v5", "v2", "v4", "v3"} {
		pushSimpleImage(t, client, base, "alpha", tag, "linux", "amd64", []byte("layer-alpha-"+tag))
	}

	// Paginate _catalog with n=2.
	var gotRepos []string
	nextPath := "/v2/_catalog?n=2"
	pages := 0
	for nextPath != "" {
		pages++
		resp, err := client.Get(base + nextPath)
		if err != nil {
			t.Fatal(err)
		}
		var body struct {
			Repositories []string `json:"repositories"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		link := resp.Header.Get("Link")
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("_catalog status = %d", resp.StatusCode)
		}
		gotRepos = append(gotRepos, body.Repositories...)
		nextPath = parseNextLink(link)
	}
	wantRepos := []string{"alpha", "beta", "delta", "epsilon", "gamma"}
	if !reflect.DeepEqual(gotRepos, wantRepos) || pages != 3 {
		t.Fatalf("_catalog pagination: got=%v pages=%d, want=%v pages=3", gotRepos, pages, wantRepos)
	}

	// _catalog?n=0 returns empty slice, no Link.
	resp, err := client.Get(base + "/v2/_catalog?n=0")
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if string(raw) != `{"repositories":[]}` || resp.Header.Get("Link") != "" {
		t.Fatalf("_catalog?n=0 = %s link=%q", raw, resp.Header.Get("Link"))
	}

	// Invalid n -> 400.
	badResp, err := client.Get(base + "/v2/_catalog?n=-1")
	if err != nil {
		t.Fatal(err)
	}
	badResp.Body.Close()
	if badResp.StatusCode != http.StatusBadRequest {
		t.Fatalf("_catalog?n=-1 = %d, want 400", badResp.StatusCode)
	}

	// Paginate alpha/tags/list with n=2.
	var gotTags []string
	nextPath = "/v2/alpha/tags/list?n=2"
	pages = 0
	for nextPath != "" {
		pages++
		resp, err := client.Get(base + nextPath)
		if err != nil {
			t.Fatal(err)
		}
		var body struct {
			Name string   `json:"name"`
			Tags []string `json:"tags"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		link := resp.Header.Get("Link")
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK || body.Name != "alpha" {
			t.Fatalf("tags/list status=%d name=%q", resp.StatusCode, body.Name)
		}
		gotTags = append(gotTags, body.Tags...)
		nextPath = parseNextLink(link)
	}
	wantTags := []string{"v1", "v2", "v3", "v4", "v5"}
	if !reflect.DeepEqual(gotTags, wantTags) || pages != 3 {
		t.Fatalf("tags/list pagination: got=%v pages=%d, want=%v pages=3", gotTags, pages, wantTags)
	}

	// Unknown repo -> 404 NAME_UNKNOWN.
	unkResp, err := client.Get(base + "/v2/unknown/repo/tags/list")
	if err != nil {
		t.Fatal(err)
	}
	code := decodeOCIErrorCode(t, unkResp)
	unkResp.Body.Close()
	if unkResp.StatusCode != http.StatusNotFound || code != "NAME_UNKNOWN" {
		t.Fatalf("unknown tags/list = %d %q, want 404 NAME_UNKNOWN", unkResp.StatusCode, code)
	}
}

func TestDeleteGCUsageStoppedAndPersistence(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "persist-reg")
	srv := New(Config{Dir: dir, Port: -1})
	if err := srv.Start(); err != nil {
		t.Fatal(err)
	}
	client := &http.Client{Timeout: 5 * time.Second}
	base := baseURL(srv)

	shared := []byte("shared-layer-bytes")
	uniq1 := []byte("unique-layer-v1")
	uniq2 := []byte("unique-layer-v2-extra")

	m1Digest, m1Bytes, cfg1Bytes := pushSimpleImage(t, client, base, "app", "v1", "linux", "amd64", shared, uniq1)
	m2Digest, m2Bytes, cfg2Bytes := pushSimpleImage(t, client, base, "app", "v2", "linux", "arm64", shared, uniq2)
	// Also tag m2 as "latest" to verify multiple tags sharing a manifest.
	mustPutManifest(t, client, base, "app", "latest", mediaTypeOCIImageManifest, m2Bytes)

	stats, err := srv.DiskUsage()
	if err != nil {
		t.Fatal(err)
	}
	wantTotal := int64(len(m1Bytes) + len(m2Bytes) + len(cfg1Bytes) + len(cfg2Bytes) + len(shared) + len(uniq1) + len(uniq2))
	if stats.ImageCount != 3 || stats.BlobCount != 5 || stats.TotalBytes != wantTotal || stats.ReclaimableBytes != 0 {
		t.Fatalf("DiskUsage = %+v, want ImageCount=3 BlobCount=5 Total=%d Reclaimable=0", stats, wantTotal)
	}

	// Delete tag v1 via HTTP DELETE -> 202, leaving revision m1 and its exclusive blobs reclaimable.
	delReq, _ := http.NewRequest(http.MethodDelete, base+"/v2/app/manifests/v1", nil)
	delResp, err := client.Do(delReq)
	if err != nil {
		t.Fatal(err)
	}
	delResp.Body.Close()
	if delResp.StatusCode != http.StatusAccepted {
		t.Fatalf("DELETE tag v1 = %d, want 202", delResp.StatusCode)
	}

	// Revision m1 is still fetchable by digest until GC runs.
	revResp, err := client.Get(base + "/v2/app/manifests/" + m1Digest)
	if err != nil {
		t.Fatal(err)
	}
	revResp.Body.Close()
	if revResp.StatusCode != http.StatusOK {
		t.Fatalf("GET unreferenced revision before GC = %d, want 200", revResp.StatusCode)
	}

	stats, err = srv.DiskUsage()
	if err != nil {
		t.Fatal(err)
	}
	wantReclaimable := int64(len(m1Bytes) + len(cfg1Bytes) + len(uniq1))
	if stats.ImageCount != 2 || stats.ReclaimableBytes != wantReclaimable {
		t.Fatalf("DiskUsage before GC = %+v, want ImageCount=2 Reclaimable=%d", stats, wantReclaimable)
	}

	// Start an active upload to verify GC never touches uploads/.
	uResp, err := client.Post(base+"/v2/app/blobs/uploads/", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	uLoc := uResp.Header.Get("Location")
	uResp.Body.Close()
	activeBytes := []byte("in-flight-upload-chunk")
	pReq, _ := http.NewRequest(http.MethodPatch, base+uLoc, bytes.NewReader(activeBytes))
	pResp, err := client.Do(pReq)
	if err != nil {
		t.Fatal(err)
	}
	pResp.Body.Close()

	// Run GC.
	gcRes, err := srv.GC()
	if err != nil {
		t.Fatal(err)
	}
	if gcRes.RemovedManifests != 1 || gcRes.RemovedBlobs != 2 || gcRes.ReclaimedBytes != wantReclaimable {
		t.Fatalf("GC() = %+v, want Manifests=1 Blobs=2 Reclaimed=%d", gcRes, wantReclaimable)
	}

	// In-flight upload is completely untouched and still completable!
	finReq, _ := http.NewRequest(http.MethodPut, base+uLoc+"?digest="+url.QueryEscape(sha256Digest(activeBytes)), nil)
	finResp, err := client.Do(finReq)
	if err != nil {
		t.Fatal(err)
	}
	finResp.Body.Close()
	if finResp.StatusCode != http.StatusCreated {
		t.Fatalf("completing upload after GC = %d, want 201", finResp.StatusCode)
	}

	// Clean up that unreferenced blob so DiskUsage is exact.
	if _, err := srv.GC(); err != nil {
		t.Fatal(err)
	}

	// DELETE by digest removes revision and ALL tags referencing it (push temp repo to verify).
	tmpDigest, tmpManifest, _ := pushSimpleImage(t, client, base, "temp", "t1", "linux", "amd64", []byte("temp-layer"))
	mustPutManifest(t, client, base, "temp", "t2", mediaTypeOCIImageManifest, tmpManifest)
	delDigReq, _ := http.NewRequest(http.MethodDelete, base+"/v2/temp/manifests/"+tmpDigest, nil)
	delDigResp, err := client.Do(delDigReq)
	if err != nil {
		t.Fatal(err)
	}
	delDigResp.Body.Close()
	if delDigResp.StatusCode != http.StatusAccepted {
		t.Fatalf("DELETE by digest = %d, want 202", delDigResp.StatusCode)
	}
	for _, tag := range []string{"t1", "t2"} {
		r, err := client.Get(base + "/v2/temp/manifests/" + tag)
		if err != nil {
			t.Fatal(err)
		}
		r.Body.Close()
		if r.StatusCode != http.StatusNotFound {
			t.Fatalf("tag %s still exists after DELETE by digest: %d", tag, r.StatusCode)
		}
	}
	if _, err := srv.GC(); err != nil {
		t.Fatal(err)
	}

	// Stop server and verify List(), DiskUsage(), DeleteTag(), GC() all work while STOPPED.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := srv.Stop(ctx); err != nil {
		t.Fatal(err)
	}

	imgs, err := srv.List()
	if err != nil {
		t.Fatalf("List() while stopped: %v", err)
	}
	if len(imgs) != 2 {
		t.Fatalf("List() while stopped len = %d, want 2 (v2 & latest)", len(imgs))
	}

	// DeleteTag("app", "latest") while stopped keeps m2 and its blobs because "v2" still points to m2.
	delRes, err := srv.DeleteTag("app", "latest")
	if err != nil {
		t.Fatalf("DeleteTag while stopped: %v", err)
	}
	if delRes.RemovedManifests != 0 || delRes.RemovedBlobs != 0 {
		t.Fatalf("DeleteTag(latest) removed shared objects: %+v", delRes)
	}

	stats, err = srv.DiskUsage()
	if err != nil {
		t.Fatalf("DiskUsage while stopped: %v", err)
	}
	if stats.ImageCount != 1 || stats.BlobCount != 3 || stats.ReclaimableBytes != 0 {
		t.Fatalf("DiskUsage while stopped = %+v, want ImageCount=1 BlobCount=3 Reclaimable=0", stats)
	}

	// Start a brand-new Server instance on the same Dir and confirm persistence.
	srv2 := New(Config{Dir: dir, Port: -1})
	if err := srv2.Start(); err != nil {
		t.Fatalf("srv2.Start(): %v", err)
	}
	defer func() {
		c2, cancel2 := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel2()
		_ = srv2.Stop(c2)
	}()

	imgs2, err := srv2.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(imgs2) != 1 || imgs2[0].Key() != "app:v2" || imgs2[0].Digest != m2Digest {
		t.Fatalf("persisted List() = %+v, want app:v2 %s", imgs2, m2Digest)
	}

	resp, err := client.Get(baseURL(srv2) + "/v2/app/manifests/v2")
	if err != nil {
		t.Fatal(err)
	}
	gotM2, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || !bytes.Equal(gotM2, m2Bytes) {
		t.Fatalf("persisted GET manifest = %d bytesMatch=%v", resp.StatusCode, bytes.Equal(gotM2, m2Bytes))
	}

	// Delete final tag via DeleteTag and verify full cleanup (including empty repo dir pruning).
	finalGC, err := srv2.DeleteTag("app", "v2")
	if err != nil {
		t.Fatal(err)
	}
	if finalGC.RemovedManifests != 1 || finalGC.RemovedBlobs != 3 {
		t.Fatalf("final DeleteTag GC = %+v, want Manifests=1 Blobs=3", finalGC)
	}
	finalStats, err := srv2.DiskUsage()
	if err != nil {
		t.Fatal(err)
	}
	if finalStats != (DiskStats{}) {
		t.Fatalf("final DiskUsage = %+v, want all zero", finalStats)
	}
	repoEntries, err := os.ReadDir(filepath.Join(dir, "repos"))
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		t.Fatal(err)
	}
	if len(repoEntries) != 0 {
		t.Fatalf("expected empty repo dir pruned, found %d entries", len(repoEntries))
	}
}

func TestOnChangeCallbackNoDeadlock(t *testing.T) {
	var changes atomic.Int32
	var srv *Server
	srv = New(Config{
		Dir:  filepath.Join(t.TempDir(), "reg"),
		Port: -1,
		OnChange: func() {
			changes.Add(1)
			// Calling List() and DiskUsage() synchronously inside OnChange must not deadlock!
			if _, err := srv.List(); err != nil {
				t.Errorf("List inside OnChange failed: %v", err)
			}
			if _, err := srv.DiskUsage(); err != nil {
				t.Errorf("DiskUsage inside OnChange failed: %v", err)
			}
		},
	})
	if err := srv.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Stop(ctx)
	}()

	client := &http.Client{Timeout: 5 * time.Second}
	base := baseURL(srv)

	// 1. PUT manifest triggers OnChange.
	pushSimpleImage(t, client, base, "cb", "v1", "linux", "amd64", []byte("layer1"))
	if got := changes.Load(); got != 1 {
		t.Fatalf("changes after PUT = %d, want 1", got)
	}

	// 2. GC with nothing to reclaim does NOT trigger OnChange.
	if _, err := srv.GC(); err != nil {
		t.Fatal(err)
	}
	if got := changes.Load(); got != 1 {
		t.Fatalf("changes after no-op GC = %d, want 1", got)
	}

	// 3. HTTP DELETE manifest triggers OnChange.
	req, _ := http.NewRequest(http.MethodDelete, base+"/v2/cb/manifests/v1", nil)
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if got := changes.Load(); got != 2 {
		t.Fatalf("changes after DELETE manifest = %d, want 2", got)
	}

	// 4. GC reclaiming unreferenced objects triggers OnChange.
	if _, err := srv.GC(); err != nil {
		t.Fatal(err)
	}
	if got := changes.Load(); got != 3 {
		t.Fatalf("changes after reclaiming GC = %d, want 3", got)
	}

	// 5. DeleteTag triggers OnChange.
	pushSimpleImage(t, client, base, "cb", "v2", "linux", "amd64", []byte("layer2"))
	if _, err := srv.DeleteTag("cb", "v2"); err != nil {
		t.Fatal(err)
	}
	if got := changes.Load(); got != 5 {
		t.Fatalf("changes after PUT+DeleteTag = %d, want 5", got)
	}
}

func TestConcurrentPushGCListRace(t *testing.T) {
	var onChangeCount atomic.Int64
	srv := startTestServer(t, func() {
		onChangeCount.Add(1)
	})
	base := baseURL(srv)

	// Seed a baseline tagged image that must survive all concurrent operations.
	seedClient := &http.Client{Timeout: 10 * time.Second}
	seedDigest, _, _ := pushSimpleImage(t, seedClient, base, "baseline/app", "stable", "linux", "arm64", []byte("stable-layer"))

	var wg sync.WaitGroup

	// 6 concurrent pusher goroutines pushing chunked blobs + manifests.
	for g := 0; g < 6; g++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			c := &http.Client{Timeout: 10 * time.Second}
			repo := fmt.Sprintf("team/svc%d", id)
			for iter := 0; iter < 5; iter++ {
				cfg := []byte(fmt.Sprintf(`{"os":"linux","architecture":"amd64","iter":%d,"id":%d}`, iter, id))
				layer := []byte(fmt.Sprintf("payload-%d-%d", id, iter))

				// Chunked upload for layer.
				initResp, err := c.Post(fmt.Sprintf("%s/v2/%s/blobs/uploads/", base, repo), "", nil)
				if err != nil {
					t.Errorf("init upload: %v", err)
					return
				}
				loc := initResp.Header.Get("Location")
				initResp.Body.Close()

				half := len(layer) / 2
				pReq, _ := http.NewRequest(http.MethodPatch, base+loc, bytes.NewReader(layer[:half]))
				pResp, err := c.Do(pReq)
				if err != nil {
					t.Errorf("patch upload: %v", err)
					return
				}
				pResp.Body.Close()

				ld := sha256Digest(layer)
				fReq, _ := http.NewRequest(http.MethodPut, base+loc+"?digest="+url.QueryEscape(ld), bytes.NewReader(layer[half:]))
				fResp, err := c.Do(fReq)
				if err != nil {
					t.Errorf("put upload: %v", err)
					return
				}
				fResp.Body.Close()

				// Monolithic upload for config.
				cd := sha256Digest(cfg)
				cReq, _ := http.NewRequest(http.MethodPost, fmt.Sprintf("%s/v2/%s/blobs/uploads/?digest=%s", base, repo, url.QueryEscape(cd)), bytes.NewReader(cfg))
				cResp, err := c.Do(cReq)
				if err != nil {
					t.Errorf("config upload: %v", err)
					return
				}
				cResp.Body.Close()

				// Push manifest (tolerate 400 BLOB_UNKNOWN only if concurrent GC collected
				// a just-uploaded unreferenced blob right between blob PUT and manifest PUT).
				mBytes, _ := json.Marshal(ociManifest{
					SchemaVersion: 2,
					MediaType:     mediaTypeOCIImageManifest,
					Config:        &ociDescriptor{Digest: cd, Size: int64(len(cfg))},
					Layers:        []ociDescriptor{{Digest: ld, Size: int64(len(layer))}},
				})
				mReq, _ := http.NewRequest(http.MethodPut, fmt.Sprintf("%s/v2/%s/manifests/v%d", base, repo, iter), bytes.NewReader(mBytes))
				mResp, err := c.Do(mReq)
				if err != nil {
					t.Errorf("put manifest: %v", err)
					return
				}
				if mResp.StatusCode != http.StatusCreated && mResp.StatusCode != http.StatusBadRequest {
					t.Errorf("unexpected manifest PUT status: %d", mResp.StatusCode)
				}
				mResp.Body.Close()
			}
		}(g)
	}

	// Concurrent List() + DiskUsage() readers.
	for r := 0; r < 3; r++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 20; i++ {
				if _, err := srv.List(); err != nil {
					t.Errorf("concurrent List: %v", err)
				}
				if _, err := srv.DiskUsage(); err != nil {
					t.Errorf("concurrent DiskUsage: %v", err)
				}
			}
		}()
	}

	// Concurrent GC() runner.
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 15; i++ {
			if _, err := srv.GC(); err != nil {
				t.Errorf("concurrent GC: %v", err)
			}
			time.Sleep(time.Millisecond)
		}
	}()

	wg.Wait()

	// Baseline tagged image MUST still be intact and downloadable.
	resp, err := seedClient.Get(base + "/v2/baseline/app/manifests/stable")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || resp.Header.Get("Docker-Content-Digest") != seedDigest {
		t.Fatalf("baseline image corrupted after concurrent stress: status=%d digest=%q", resp.StatusCode, resp.Header.Get("Docker-Content-Digest"))
	}
	if onChangeCount.Load() == 0 {
		t.Fatalf("expected OnChange to be invoked during concurrent pushes")
	}
}
