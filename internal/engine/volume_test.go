package engine

// Owner: workstream C.

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"
)

func TestVolumesCRUDIsolationAndPersistence(t *testing.T) {
	dir := t.TempDir()
	fake := newFakeRunner()
	// Pre-populate a rungv volume that belongs to a user sandbox, NOT the engine.
	if err := fake.CreateVolume(context.Background(), "user-sandbox-vol", "ext4lite"); err != nil {
		t.Fatal(err)
	}

	te := reopenTestEngine(t, dir, fake)

	// Listing must NOT include user-sandbox-vol.
	st, body := te.do(t, http.MethodGet, "/volumes", nil)
	if st != http.StatusOK {
		t.Fatalf("GET /volumes = %d %s", st, body)
	}
	var listResp struct {
		Volumes  []dockerVolume `json:"Volumes"`
		Warnings []string       `json:"Warnings"`
	}
	if err := json.Unmarshal(body, &listResp); err != nil {
		t.Fatal(err)
	}
	if len(listResp.Volumes) != 0 {
		t.Fatalf("expected 0 engine volumes, got %+v", listResp.Volumes)
	}

	// Inspect / Delete of non-engine volume → 404.
	if st, _ := te.do(t, http.MethodGet, "/volumes/user-sandbox-vol", nil); st != http.StatusNotFound {
		t.Fatalf("inspect non-engine vol = %d, want 404", st)
	}
	if st, _ := te.do(t, http.MethodDelete, "/volumes/user-sandbox-vol", nil); st != http.StatusNotFound {
		t.Fatalf("delete non-engine vol = %d, want 404", st)
	}
	// Seam removeVolume on non-engine volume → 403.
	if err := te.removeVolume(context.Background(), "user-sandbox-vol", false); statusOf(err) != http.StatusForbidden {
		t.Fatalf("removeVolume(non-engine) status = %d (%v), want 403", statusOf(err), err)
	}
	// Seam ensureVolume uses user-sandbox-vol WITHOUT recording it.
	if err := te.ensureVolume(context.Background(), "user-sandbox-vol", nil, false); err != nil {
		t.Fatalf("ensureVolume(user-sandbox-vol) = %v", err)
	}
	if st, _ := te.do(t, http.MethodGet, "/volumes/user-sandbox-vol", nil); st != http.StatusNotFound {
		t.Fatalf("user-sandbox-vol should still not be recorded as engine volume")
	}

	// Bad driver → 400.
	if st, _ := te.do(t, http.MethodPost, "/volumes/create", map[string]any{"Name": "bad", "Driver": "nfs"}); st != http.StatusBadRequest {
		t.Fatalf("bad driver = %d, want 400", st)
	}

	// Create named volume with labels and hostfs opt.
	st, body = te.do(t, http.MethodPost, "/volumes/create", map[string]any{
		"Name":       "gvxd-data",
		"Driver":     "local",
		"DriverOpts": map[string]string{"type": "hostfs"},
		"Labels":     map[string]string{"com.docker.compose.project": "demo"},
	})
	if st != http.StatusCreated {
		t.Fatalf("create = %d %s", st, body)
	}
	var v dockerVolume
	if err := json.Unmarshal(body, &v); err != nil {
		t.Fatal(err)
	}
	if v.Name != "gvxd-data" || v.Driver != "local" || v.Labels["com.docker.compose.project"] != "demo" {
		t.Fatalf("unexpected volume: %+v", v)
	}
	if fake.volumes["gvxd-data"].Driver != "hostfs" {
		t.Errorf("rungv driver = %q, want hostfs", fake.volumes["gvxd-data"].Driver)
	}

	// Creating existing engine volume returns it (idempotent).
	if st, _ := te.do(t, http.MethodPost, "/volumes/create", map[string]any{"Name": "gvxd-data"}); st != http.StatusCreated {
		t.Fatalf("re-create existing = %d, want 201", st)
	}

	// Creating with empty name generates 64-hex anonymous volume.
	st, body = te.do(t, http.MethodPost, "/volumes/create", map[string]any{})
	if st != http.StatusCreated {
		t.Fatalf("create anon = %d %s", st, body)
	}
	var anon dockerVolume
	_ = json.Unmarshal(body, &anon)
	if len(anon.Name) != 64 {
		t.Fatalf("anon name len = %d (%q), want 64", len(anon.Name), anon.Name)
	}

	// Reopen engine: volumes.json preserves gvxd-data and labels.
	te.srv.Close()
	_ = te.Close(time.Second)
	te2 := reopenTestEngine(t, dir, fake)
	q := url.QueryEscape(`{"label":{"com.docker.compose.project=demo":true}}`)
	st, body = te2.do(t, http.MethodGet, "/volumes?filters="+q, nil)
	if st != http.StatusOK {
		t.Fatalf("GET /volumes after reopen = %d %s", st, body)
	}
	listResp.Volumes = nil
	_ = json.Unmarshal(body, &listResp)
	if len(listResp.Volumes) != 1 || listResp.Volumes[0].Name != "gvxd-data" {
		t.Fatalf("filtered volumes after reopen = %+v", listResp.Volumes)
	}
}

func TestVolumeInUseAndPrune(t *testing.T) {
	te := newTestEngine(t)
	te.fake.addImage("alpine:3.20", ImageConfig{Cmd: []string{"/bin/sh"}})
	// Non-engine volume that prune must NEVER touch.
	_ = te.fake.CreateVolume(context.Background(), "other-sandbox-vol", "ext4lite")

	// Create one used named volume, one unused named volume, one unused anonymous volume.
	if st, b := te.do(t, http.MethodPost, "/volumes/create", map[string]any{"Name": "gvxd-used"}); st != http.StatusCreated {
		t.Fatalf("create used = %d %s", st, b)
	}
	if st, b := te.do(t, http.MethodPost, "/volumes/create", map[string]any{"Name": "gvxd-unused"}); st != http.StatusCreated {
		t.Fatalf("create unused = %d %s", st, b)
	}
	if err := te.ensureVolume(context.Background(), strings.Repeat("a", 64), nil, true); err != nil {
		t.Fatal(err)
	}

	// Mount gvxd-used in a container.
	if st, b := te.do(t, http.MethodPost, "/containers/create?name=gvxd-volctr", map[string]any{
		"Image": "alpine:3.20",
		"HostConfig": map[string]any{
			"Binds": []string{"gvxd-used:/mnt"},
		},
	}); st != http.StatusCreated {
		t.Fatalf("create container = %d %s", st, b)
	}

	// DELETE in-use volume → 409.
	st, body := te.do(t, http.MethodDelete, "/volumes/gvxd-used", nil)
	if st != http.StatusConflict || !strings.Contains(string(body), "volume is in use") {
		t.Fatalf("delete in-use = %d %s, want 409", st, body)
	}

	// Default prune removes only anonymous unused volumes.
	st, body = te.do(t, http.MethodPost, "/volumes/prune", nil)
	if st != http.StatusOK {
		t.Fatalf("prune = %d %s", st, body)
	}
	var pr struct {
		VolumesDeleted []string `json:"VolumesDeleted"`
	}
	_ = json.Unmarshal(body, &pr)
	if len(pr.VolumesDeleted) != 1 || pr.VolumesDeleted[0] != strings.Repeat("a", 64) {
		t.Fatalf("default prune deleted %v, want only anon volume", pr.VolumesDeleted)
	}

	// Prune with all=true deletes gvxd-unused, keeps gvxd-used and other-sandbox-vol.
	q := url.QueryEscape(`{"all":{"true":true}}`)
	st, body = te.do(t, http.MethodPost, "/volumes/prune?filters="+q, nil)
	if st != http.StatusOK {
		t.Fatalf("prune all = %d %s", st, body)
	}
	pr.VolumesDeleted = nil
	_ = json.Unmarshal(body, &pr)
	if len(pr.VolumesDeleted) != 1 || pr.VolumesDeleted[0] != "gvxd-unused" {
		t.Fatalf("prune all deleted %v, want [gvxd-unused]", pr.VolumesDeleted)
	}
	if _, ok := te.fake.volumes["other-sandbox-vol"]; !ok {
		t.Fatal("prune deleted a non-engine rungv volume!")
	}
	if _, ok := te.fake.volumes["gvxd-used"]; !ok {
		t.Fatal("prune deleted an in-use volume!")
	}
}
