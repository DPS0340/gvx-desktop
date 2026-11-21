package engine

// Owner: workstream C.

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"testing"
)

func TestImagesListInspectAndConfigSeam(t *testing.T) {
	te := newTestEngine(t)
	id1 := te.fake.addImage("alpine:3.20", ImageConfig{
		Cmd:    []string{"/bin/sh"},
		Labels: map[string]string{"env": "prod"},
	})
	id2 := te.fake.addImage("ghcr.io/org/app:v1", ImageConfig{
		Cmd:    []string{"/app"},
		Labels: map[string]string{"env": "dev"},
	})

	// Seam: imageConfig resolves exact ref, docker.io/library/ prefix, short ID, and 404s missing.
	ins, err := te.imageConfig(context.Background(), "alpine:3.20")
	if err != nil || ins.ID != id1 {
		t.Fatalf("imageConfig(alpine:3.20) = %v, %v", ins.ID, err)
	}
	ins, err = te.imageConfig(context.Background(), "docker.io/library/alpine:3.20")
	if err != nil || ins.ID != id1 {
		t.Fatalf("imageConfig(docker.io/library/alpine:3.20) = %v, %v", ins.ID, err)
	}
	short := strings.TrimPrefix(id2, "sha256:")[:12]
	ins, err = te.imageConfig(context.Background(), short)
	if err != nil || ins.ID != id2 {
		t.Fatalf("imageConfig(%s) = %v, %v", short, ins.ID, err)
	}
	if _, err := te.imageConfig(context.Background(), "no-such-image:1"); statusOf(err) != http.StatusNotFound {
		t.Fatalf("missing image status = %d (%v), want 404", statusOf(err), err)
	}

	// GET /images/json
	st, body := te.do(t, http.MethodGet, "/images/json", nil)
	if st != http.StatusOK {
		t.Fatalf("GET /images/json = %d %s", st, body)
	}
	var list []imageSummary
	if err := json.Unmarshal(body, &list); err != nil {
		t.Fatal(err)
	}
	if len(list) != 2 {
		t.Fatalf("got %d images, want 2", len(list))
	}

	// Filter by reference and label.
	q := url.QueryEscape(`{"reference":{"alpine":true}}`)
	st, body = te.do(t, http.MethodGet, "/images/json?filters="+q, nil)
	if st != http.StatusOK {
		t.Fatalf("filtered list = %d %s", st, body)
	}
	list = nil
	_ = json.Unmarshal(body, &list)
	if len(list) != 1 || list[0].ID != id1 {
		t.Fatalf("reference filter got %+v, want %s", list, id1)
	}

	q = url.QueryEscape(`{"label":{"env=dev":true}}`)
	_, body = te.do(t, http.MethodGet, "/images/json?filters="+q, nil)
	list = nil
	_ = json.Unmarshal(body, &list)
	if len(list) != 1 || list[0].ID != id2 {
		t.Fatalf("label filter got %+v, want %s", list, id2)
	}

	// Inspect image with slashes in path.
	st, body = te.do(t, http.MethodGet, "/images/ghcr.io/org/app:v1/json", nil)
	if st != http.StatusOK {
		t.Fatalf("inspect slash ref = %d %s", st, body)
	}
	var got imageInspectResponse
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatal(err)
	}
	if got.ID != id2 || got.Architecture != "arm64" {
		t.Fatalf("inspect mismatch: %+v", got)
	}

	// Missing inspect → 404.
	if st, _ := te.do(t, http.MethodGet, "/images/does-not-exist/json", nil); st != http.StatusNotFound {
		t.Fatalf("missing inspect = %d, want 404", st)
	}
}

func TestImagePullStreamAndErrors(t *testing.T) {
	te := newTestEngine(t)

	// Successful pull streams JSON lines.
	st, body := te.do(t, http.MethodPost, "/images/create?fromImage=busybox&tag=1.36", nil)
	if st != http.StatusOK {
		t.Fatalf("pull = %d %s", st, body)
	}
	var lines []map[string]string
	sc := bufio.NewScanner(bytes.NewReader(body))
	for sc.Scan() {
		var m map[string]string
		if err := json.Unmarshal(sc.Bytes(), &m); err != nil {
			t.Fatalf("invalid pull json line %q: %v", sc.Text(), err)
		}
		lines = append(lines, m)
	}
	if len(lines) < 2 {
		t.Fatalf("expected >=2 progress lines, got %+v", lines)
	}
	if lines[0]["id"] != "busybox:1.36" {
		t.Errorf("expected parsed id in first line, got %+v", lines[0])
	}

	// Missing repository fails before streaming with 404.
	st, body = te.do(t, http.MethodPost, "/images/create?fromImage=missing/repo&tag=latest", nil)
	if st != http.StatusNotFound {
		t.Fatalf("missing pull = %d %s, want 404", st, body)
	}

	// fromSrc import → 501.
	st, _ = te.do(t, http.MethodPost, "/images/create?fromSrc=-", nil)
	if st != http.StatusNotImplemented {
		t.Fatalf("fromSrc = %d, want 501", st)
	}
}

func TestImageTagDeleteConflictAndLoadSave(t *testing.T) {
	te := newTestEngine(t)
	te.fake.addImage("alpine:3.20", ImageConfig{Cmd: []string{"/bin/sh"}})

	// Tag
	st, body := te.do(t, http.MethodPost, "/images/alpine:3.20/tag?repo=myrepo/alpine&tag=v1", nil)
	if st != http.StatusCreated {
		t.Fatalf("tag = %d %s", st, body)
	}
	if st, _ := te.do(t, http.MethodGet, "/images/myrepo/alpine:v1/json", nil); st != http.StatusOK {
		t.Fatalf("tagged image inspect = %d, want 200", st)
	}

	// Create a container referencing alpine:3.20 → DELETE without force must 409.
	if st, b := te.do(t, http.MethodPost, "/containers/create?name=gvxd-imguse", map[string]any{
		"Image": "alpine:3.20",
	}); st != http.StatusCreated {
		t.Fatalf("container create = %d %s", st, b)
	}
	st, body = te.do(t, http.MethodDelete, "/images/alpine:3.20", nil)
	if st != http.StatusConflict || !strings.Contains(string(body), "must force") {
		t.Fatalf("delete in-use image = %d %s, want 409 conflict", st, body)
	}

	// Untagging the unused alias myrepo/alpine:v1 works without force.
	st, body = te.do(t, http.MethodDelete, "/images/myrepo/alpine:v1", nil)
	if st != http.StatusOK || !strings.Contains(string(body), "Untagged") {
		t.Fatalf("untag alias = %d %s", st, body)
	}

	// Force delete removes alpine:3.20.
	st, body = te.do(t, http.MethodDelete, "/images/alpine:3.20?force=1", nil)
	if st != http.StatusOK || !strings.Contains(string(body), "Deleted") {
		t.Fatalf("force delete = %d %s", st, body)
	}

	// Load and Save
	st, body = te.do(t, http.MethodPost, "/images/load", "loaded:v1")
	if st != http.StatusOK || !strings.Contains(string(body), "Loaded image: loaded:v1") {
		t.Fatalf("load = %d %s", st, body)
	}
	st, body = te.do(t, http.MethodGet, "/images/loaded:v1/get", nil)
	if st != http.StatusOK || strings.TrimSpace(string(body)) != "loaded:v1" {
		t.Fatalf("get/save = %d %q", st, body)
	}
}

func TestImagePruneKeepRules(t *testing.T) {
	fake := newFakeRunner()
	fake.addImage("used:1", ImageConfig{Cmd: []string{"/bin/sh"}})
	fake.addImage("kept:1", ImageConfig{Cmd: []string{"/bin/sh"}})
	fake.addImage("unused:1", ImageConfig{Cmd: []string{"/bin/sh"}})

	dir := t.TempDir()
	te := reopenTestEngine(t, dir, fake)
	// Attach KeepImages on te.Engine.
	te.Engine.cfg.KeepImages = func() []string { return []string{"docker.io/library/kept:1"} }

	// Container uses used:1.
	if st, b := te.do(t, http.MethodPost, "/containers/create?name=gvxd-keep", map[string]any{
		"Image": "used:1",
	}); st != http.StatusCreated {
		t.Fatalf("create = %d %s", st, b)
	}

	// Default prune (dangling only) leaves all tagged images intact.
	st, body := te.do(t, http.MethodPost, "/images/prune", nil)
	if st != http.StatusOK {
		t.Fatalf("prune dangling = %d %s", st, body)
	}
	if _, err := te.imageConfig(context.Background(), "unused:1"); err != nil {
		t.Fatalf("dangling prune should not remove tagged unused:1")
	}

	// Prune with dangling=false (-a) removes only unused:1, keeping used:1 and kept:1.
	q := url.QueryEscape(`{"dangling":["false"]}`)
	st, body = te.do(t, http.MethodPost, "/images/prune?filters="+q, nil)
	if st != http.StatusOK {
		t.Fatalf("prune all = %d %s", st, body)
	}
	var res struct {
		ImagesDeleted  []map[string]string `json:"ImagesDeleted"`
		SpaceReclaimed int64               `json:"SpaceReclaimed"`
	}
	if err := json.Unmarshal(body, &res); err != nil {
		t.Fatal(err)
	}
	if res.SpaceReclaimed != 1<<20 {
		t.Errorf("SpaceReclaimed = %d, want %d", res.SpaceReclaimed, 1<<20)
	}
	if _, err := te.imageConfig(context.Background(), "unused:1"); statusOf(err) != http.StatusNotFound {
		t.Errorf("unused:1 should have been deleted")
	}
	if _, err := te.imageConfig(context.Background(), "used:1"); err != nil {
		t.Errorf("used:1 should have been kept: %v", err)
	}
	if _, err := te.imageConfig(context.Background(), "kept:1"); err != nil {
		t.Errorf("kept:1 (KeepImages) should have been kept: %v", err)
	}
}

func TestImageDeleteRefusesKeptImages(t *testing.T) {
	te := newTestEngine(t)
	idTag := te.fake.addImage("docker:dind", ImageConfig{Cmd: []string{"dockerd"}})
	idDig := te.fake.addImage("rancher/k3s:v1.31", ImageConfig{Cmd: []string{"k3s"}})
	idPfx := te.fake.addImage("registry:2", ImageConfig{Cmd: []string{"registry"}})

	// Attach a RepoDigest to rancher/k3s so we can keep it by repo@sha256:...
	digest := "sha256:" + strings.Repeat("d", 64)
	te.fake.mu.Lock()
	k3s := te.fake.images["rancher/k3s:v1.31"]
	k3s.RepoDigests = []string{"rancher/k3s@" + digest}
	te.fake.images["rancher/k3s:v1.31"] = k3s
	te.fake.images[idDig] = k3s
	te.fake.mu.Unlock()

	pfx := strings.TrimPrefix(idPfx, "sha256:")[:8]
	te.Engine.cfg.KeepImages = func() []string {
		return []string{
			"docker.io/library/docker:dind",
			"rancher/k3s@" + digest,
			"sha256:" + pfx,
		}
	}

	for _, ref := range []string{"docker:dind", idTag, "rancher/k3s:v1.31", "registry:2"} {
		for _, q := range []string{"", "?force=1"} {
			st, body := te.do(t, http.MethodDelete, "/images/"+ref+q, nil)
			if st != http.StatusConflict || !strings.Contains(string(body), "GVX Desktop sandbox uses it") {
				t.Errorf("DELETE /images/%s%s = %d %s, want 409 mentioning GVX Desktop sandbox", ref, q, st, body)
			}
		}
	}

	// All three kept images must still be present in the cache.
	for _, ref := range []string{"docker:dind", "rancher/k3s:v1.31", "registry:2"} {
		if _, err := te.imageConfig(context.Background(), ref); err != nil {
			t.Errorf("kept image %s was deleted: %v", ref, err)
		}
	}
}

func TestImagePruneKeepImagesAllFormsAndDangling(t *testing.T) {
	te := newTestEngine(t)

	// Add three dangling images: one pinned by RepoDigest, one by ID prefix, one unpinned.
	digPinned := "sha256:" + strings.Repeat("a", 64)
	danglingDigID := "sha256:" + fakeHex("dangling-digest")
	danglingPfxID := "sha256:" + fakeHex("dangling-prefix")
	danglingFreeID := "sha256:" + fakeHex("dangling-free")

	te.fake.mu.Lock()
	addDangling := func(key, id string, digests []string) {
		img := ImageInspect{
			ID:          id,
			RepoTags:    nil,
			RepoDigests: digests,
			Size:        1 << 20,
		}
		te.fake.images[key] = img
		te.fake.images[id] = img
	}
	addDangling("<none>:<none>@1", danglingDigID, []string{"docker.io/library/alpine@" + digPinned})
	addDangling("<none>:<none>@2", danglingPfxID, nil)
	addDangling("<none>:<none>@3", danglingFreeID, nil)
	te.fake.mu.Unlock()

	// Add tagged images: one pinned by RepoDigest, one by 8-char ID prefix, one unused.
	taggedDigID := te.fake.addImage("k3s/server:1", ImageConfig{Cmd: []string{"k3s"}})
	taggedDig := "sha256:" + strings.Repeat("b", 64)
	te.fake.mu.Lock()
	td := te.fake.images["k3s/server:1"]
	td.RepoDigests = []string{"k3s/server@" + taggedDig}
	te.fake.images["k3s/server:1"] = td
	te.fake.images[taggedDigID] = td
	te.fake.mu.Unlock()

	taggedPfxID := te.fake.addImage("keep-pfx:1", ImageConfig{Cmd: []string{"/bin/sh"}})
	te.fake.addImage("drop-me:1", ImageConfig{Cmd: []string{"/bin/sh"}})

	te.Engine.cfg.KeepImages = func() []string {
		return []string{
			"alpine@" + digPinned,
			strings.TrimPrefix(danglingPfxID, "sha256:")[:10],
			"k3s/server@" + taggedDig,
			"sha256:" + strings.TrimPrefix(taggedPfxID, "sha256:")[:8],
		}
	}

	// Plain dangling prune must remove ONLY danglingFreeID and keep digest/prefix-pinned dangling images.
	st, body := te.do(t, http.MethodPost, "/images/prune", nil)
	if st != http.StatusOK {
		t.Fatalf("dangling prune = %d %s", st, body)
	}
	if !strings.Contains(string(body), danglingFreeID) {
		t.Errorf("dangling prune did not delete %s: %s", danglingFreeID, body)
	}
	if strings.Contains(string(body), danglingDigID) || strings.Contains(string(body), danglingPfxID) {
		t.Errorf("dangling prune deleted a KeepImages dangling image: %s", body)
	}
	if _, err := te.imageConfig(context.Background(), danglingDigID); err != nil {
		t.Errorf("digest-pinned dangling image missing after prune: %v", err)
	}
	if _, err := te.imageConfig(context.Background(), danglingPfxID); err != nil {
		t.Errorf("prefix-pinned dangling image missing after prune: %v", err)
	}

	// Prune -a (dangling=false) removes drop-me:1 and keeps all digest/prefix-pinned images.
	q := url.QueryEscape(`{"dangling":["false"]}`)
	st, body = te.do(t, http.MethodPost, "/images/prune?filters="+q, nil)
	if st != http.StatusOK {
		t.Fatalf("prune all = %d %s", st, body)
	}
	if _, err := te.imageConfig(context.Background(), "drop-me:1"); statusOf(err) != http.StatusNotFound {
		t.Errorf("drop-me:1 should have been pruned")
	}
	for _, kept := range []string{danglingDigID, danglingPfxID, "k3s/server:1", "keep-pfx:1"} {
		if _, err := te.imageConfig(context.Background(), kept); err != nil {
			t.Errorf("kept image %s was pruned: %v", kept, err)
		}
	}
}

func TestImageConfigResolvedRef(t *testing.T) {
	te := newTestEngine(t)
	id := te.fake.addImage("alpine:3.20", ImageConfig{Cmd: []string{"/bin/sh"}})

	// Exact ref.
	ins, err := te.imageConfig(context.Background(), "alpine:3.20")
	if err != nil || ins.ResolvedRef != "alpine:3.20" {
		t.Fatalf("exact ref ResolvedRef = %q, err=%v; want alpine:3.20", ins.ResolvedRef, err)
	}
	// Normalized alternative (docker.io/library/ stripped).
	ins, err = te.imageConfig(context.Background(), "docker.io/library/alpine:3.20")
	if err != nil || ins.ResolvedRef != "alpine:3.20" {
		t.Fatalf("alt ref ResolvedRef = %q, err=%v; want alpine:3.20", ins.ResolvedRef, err)
	}
	// Short ID prefix resolves to cached RepoTag so rungv run -pull never accepts it.
	short := strings.TrimPrefix(id, "sha256:")[:12]
	ins, err = te.imageConfig(context.Background(), short)
	if err != nil || ins.ResolvedRef != "alpine:3.20" {
		t.Fatalf("short ID ResolvedRef = %q, err=%v; want alpine:3.20", ins.ResolvedRef, err)
	}
	// Full ID on an untagged digest image resolves to RepoDigests[0].
	digID := "sha256:" + fakeHex("digest-only")
	te.fake.mu.Lock()
	te.fake.images[digID] = ImageInspect{
		ID:          digID,
		RepoDigests: []string{"alpine@sha256:" + strings.Repeat("c", 64)},
	}
	te.fake.mu.Unlock()
	ins, err = te.imageConfig(context.Background(), digID)
	if err != nil || ins.ResolvedRef != "alpine@sha256:"+strings.Repeat("c", 64) {
		t.Fatalf("untagged ID ResolvedRef = %q, err=%v; want RepoDigest", ins.ResolvedRef, err)
	}
}

func TestImageFlagInjectionRejected(t *testing.T) {
	te := newTestEngine(t)
	te.fake.addImage("alpine:3.20", ImageConfig{Cmd: []string{"/bin/sh"}})

	cases := []struct {
		method string
		path   string
	}{
		{http.MethodPost, "/images/create?fromImage=-cache-dir"},
		{http.MethodPost, "/images/create?fromImage=alpine&tag=-f"},
		{http.MethodPost, "/images/create?fromImage=alpine&platform=-bad"},
		{http.MethodGet, "/images/-cache-dir/json"},
		{http.MethodDelete, "/images/-f"},
		{http.MethodPost, "/images/alpine:3.20/tag?repo=-bad"},
		{http.MethodPost, "/images/alpine:3.20/tag?repo=myrepo&tag=-bad"},
		{http.MethodPost, "/images/-bad/tag?repo=myrepo&tag=v1"},
		{http.MethodGet, "/images/-bad/get"},
		{http.MethodGet, "/images/get?names=-bad"},
	}
	for _, tc := range cases {
		st, body := te.do(t, tc.method, tc.path, nil)
		if st != http.StatusBadRequest {
			t.Errorf("%s %s = %d %s, want 400", tc.method, tc.path, st, body)
		}
	}
	if _, err := te.imageConfig(context.Background(), "-cache-dir"); statusOf(err) != http.StatusBadRequest {
		t.Errorf("imageConfig(-cache-dir) status = %d (%v), want 400", statusOf(err), err)
	}
}
