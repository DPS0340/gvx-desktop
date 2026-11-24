package engine

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/ironpark/gvx-desktop/internal/imagefs"
	"github.com/ironpark/gvx-desktop/internal/registry"
)

func TestImageHistoryCachedAndFallback(t *testing.T) {
	te, _ := newFixtureEngine(t)

	// Cached image reads real OCI history & layer sizes from rungv's cache.
	st, body := te.do(t, http.MethodGet, "/images/gvxd-fixture:1/history", nil)
	if st != http.StatusOK {
		t.Fatalf("GET /images/gvxd-fixture:1/history = %d %s", st, body)
	}
	var hist []imageHistoryItem
	if err := json.Unmarshal(body, &hist); err != nil {
		t.Fatal(err)
	}
	if len(hist) != 2 {
		t.Fatalf("len(hist) = %d, want 2 (%+v)", len(hist), hist)
	}
	if hist[0].ID != fixtureImageID {
		t.Errorf("hist[0].ID = %q, want %q", hist[0].ID, fixtureImageID)
	}
	if !slices.Contains(hist[0].Tags, "gvxd-fixture:1") {
		t.Errorf("hist[0].Tags = %v, want gvxd-fixture:1", hist[0].Tags)
	}
	if hist[1].ID != "<missing>" || len(hist[1].Tags) != 0 {
		t.Errorf("hist[1] = %+v, want <missing> without tags", hist[1])
	}
	if hist[0].Size <= 0 || hist[1].Size <= 0 {
		t.Errorf("expected positive layer sizes, got %+v", hist)
	}

	// Fallback image without cache record still returns 200 with ID & tags.
	fallbackID := te.fake.addImage("fallback:1", ImageConfig{Cmd: []string{"/bin/sh"}})
	st, body = te.do(t, http.MethodGet, "/images/fallback:1/history", nil)
	if st != http.StatusOK {
		t.Fatalf("fallback history = %d %s", st, body)
	}
	hist = nil
	if err := json.Unmarshal(body, &hist); err != nil {
		t.Fatal(err)
	}
	if len(hist) != 1 || hist[0].ID != fallbackID || !slices.Contains(hist[0].Tags, "fallback:1") {
		t.Fatalf("fallback hist = %+v", hist)
	}

	// Missing image -> 404.
	if st, _ := te.do(t, http.MethodGet, "/images/no-such-img:1/history", nil); st != http.StatusNotFound {
		t.Fatalf("missing history = %d, want 404", st)
	}
}

func TestImageImportTarGzipBzip2URLAndErrors(t *testing.T) {
	te := newTestEngine(t)

	rawTar := tarOf(t,
		[2]string{"etc/", "dir"},
		[2]string{"etc/hello.txt", "hello-import\n"},
	)

	// 1. Plain tar with repo, tag, message, and --change instructions.
	q := url.Values{
		"fromSrc": {"-"},
		"repo":    {"imported/app"},
		"tag":     {"v1"},
		"message": {"initial import"},
		"changes": {
			"ENV IMPORTED=yes",
			`CMD ["/bin/sh", "-c", "cat /etc/hello.txt"]`,
			"HEALTHCHECK --interval=5s CMD true",
		},
	}
	st, body := te.do(t, http.MethodPost, "/images/create?"+q.Encode(), rawTar)
	if st != http.StatusOK {
		t.Fatalf("import plain tar = %d %s", st, body)
	}
	var statusFrame struct {
		Status string `json:"status"`
	}
	if err := json.Unmarshal(bytes.TrimSpace(body), &statusFrame); err != nil {
		t.Fatalf("decode import response %q: %v", body, err)
	}
	if !strings.HasPrefix(statusFrame.Status, "sha256:") {
		t.Fatalf("import status = %q, want sha256:...", statusFrame.Status)
	}

	// Inspect imported image to verify tag and applied changes.
	st, insBody := te.do(t, http.MethodGet, "/images/imported/app:v1/json", nil)
	if st != http.StatusOK {
		t.Fatalf("inspect imported = %d %s", st, insBody)
	}
	var ins imageInspectResponse
	if err := json.Unmarshal(insBody, &ins); err != nil {
		t.Fatal(err)
	}
	if ins.ID != statusFrame.Status {
		t.Errorf("inspected ID = %q, want %q", ins.ID, statusFrame.Status)
	}
	if !slices.Contains(ins.Config.Env, "IMPORTED=yes") {
		t.Errorf("imported Env = %v, want IMPORTED=yes", ins.Config.Env)
	}
	if !slices.Equal(ins.Config.Cmd, []string{"/bin/sh", "-c", "cat /etc/hello.txt"}) {
		t.Errorf("imported Cmd = %v", ins.Config.Cmd)
	}
	hc, err := parseHealthConfig(ins.Config.Healthcheck)
	if err != nil || !hc.enabled() {
		t.Errorf("imported Healthcheck = %s (err=%v), want enabled", ins.Config.Healthcheck, err)
	}

	// 2. Gzip-compressed tar.
	var gzBuf bytes.Buffer
	gw := gzip.NewWriter(&gzBuf)
	_, _ = gw.Write(rawTar)
	_ = gw.Close()
	st, body = te.do(t, http.MethodPost, "/images/create?fromSrc=-&repo=imported/gz&tag=v1", gzBuf.Bytes())
	if st != http.StatusOK || !strings.Contains(string(body), "sha256:") {
		t.Fatalf("import gzip = %d %s", st, body)
	}

	// 3. Bzip2-compressed tar.
	bz2Tar, err := hex.DecodeString("425a6839314159265359f05793440000717b80ca90002040037580008073469e5008082000543490d34190d1883d4d3ca09289a1a0d34003407dd5835082972108b65179356fa102181169cca3389cc119d8438b463b241b373983791ad0eb1650ea8abf26ab122203f177245385090f05793440")
	if err != nil {
		t.Fatal(err)
	}
	st, body = te.do(t, http.MethodPost, "/images/create?fromSrc=-&repo=imported/bz2&tag=v1", bz2Tar)
	if st != http.StatusOK || !strings.Contains(string(body), "sha256:") {
		t.Fatalf("import bzip2 = %d %s", st, body)
	}

	// 4. Remote HTTP URL import.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(gzBuf.Bytes())
	}))
	defer srv.Close()
	st, body = te.do(t, http.MethodPost, "/images/create?fromSrc="+url.QueryEscape(srv.URL+"/rootfs.tar.gz")+"&repo=imported/url", nil)
	if st != http.StatusOK || !strings.Contains(string(body), "sha256:") {
		t.Fatalf("import url = %d %s", st, body)
	}

	// 5. Validation errors -> 400.
	for name, tc := range map[string]struct {
		query string
		body  any
	}{
		"empty":      {"fromSrc=-", nil},
		"not_tar":    {"fromSrc=-", []byte("definitely not a tar file")},
		"xz":         {"fromSrc=-", []byte{0xfd, '7', 'z', 'X', 'Z', 0x00, 1, 2}},
		"local_path": {"fromSrc=/etc/passwd", nil},
		"bad_change": {"fromSrc=-&changes=RUN+ls", rawTar},
		"bad_repo":   {"fromSrc=-&repo=UpperCase", rawTar},
	} {
		st, b := te.do(t, http.MethodPost, "/images/create?"+tc.query, tc.body)
		if st != http.StatusBadRequest {
			t.Errorf("import error case %s = %d %s, want 400", name, st, b)
		}
	}
}

func TestImageSearchFiltersAndQuay(t *testing.T) {
	hubSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"results":[
			{"repo_name":"community/nginx","short_description":"community build","star_count":50,"is_official":false},
			{"repo_name":"nginx","short_description":"Official build of Nginx.","star_count":20000,"is_official":true},
			{"repo_name":"bitnami/nginx","short_description":"Bitnami nginx","star_count":300,"is_official":false}
		]}`))
	}))
	defer hubSrv.Close()
	quaySrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"results":[
			{"kind":"repository","name":"prometheus","namespace":{"name":"prometheus"},"description":"# Prometheus monitoring","is_public":true,"stars":420}
		]}`))
	}))
	defer quaySrv.Close()
	t.Cleanup(registry.Endpoints(hubSrv.URL, quaySrv.URL))

	te := newTestEngine(t)

	// Basic search with limit.
	st, body := te.do(t, http.MethodGet, "/images/search?term=nginx&limit=2", nil)
	if st != http.StatusOK {
		t.Fatalf("search = %d %s", st, body)
	}
	var items []imageSearchItem
	if err := json.Unmarshal(body, &items); err != nil {
		t.Fatal(err)
	}
	if len(items) != 2 || items[0].Name != "nginx" || !items[0].IsOfficial || items[0].StarCount != 20000 {
		t.Fatalf("search items = %+v", items)
	}

	// Filter is-official=true & stars=100.
	fq := url.QueryEscape(`{"is-official":["true"],"stars":["100"]}`)
	st, body = te.do(t, http.MethodGet, "/images/search?term=nginx&filters="+fq, nil)
	if st != http.StatusOK {
		t.Fatalf("filtered search = %d %s", st, body)
	}
	items = nil
	_ = json.Unmarshal(body, &items)
	if len(items) != 1 || items[0].Name != "nginx" {
		t.Fatalf("filtered items = %+v, want [nginx]", items)
	}

	// Quay prefix search.
	st, body = te.do(t, http.MethodGet, "/images/search?term=quay.io/prometheus", nil)
	if st != http.StatusOK {
		t.Fatalf("quay search = %d %s", st, body)
	}
	items = nil
	_ = json.Unmarshal(body, &items)
	if len(items) != 1 || items[0].Name != "quay.io/prometheus/prometheus" || items[0].StarCount != 420 {
		t.Fatalf("quay items = %+v", items)
	}

	// Validation errors -> 400.
	for _, p := range []string{
		"/images/search",
		"/images/search?term=nginx&limit=0",
		"/images/search?term=nginx&limit=101",
		"/images/search?term=nginx&filters=" + url.QueryEscape(`{"stars":["-1"]}`),
	} {
		if st, b := te.do(t, http.MethodGet, p, nil); st != http.StatusBadRequest {
			t.Errorf("GET %s = %d %s, want 400", p, st, b)
		}
	}
}

func TestStoppedAndCreatedContainerArchiveAndOpenDocker(t *testing.T) {
	te, _ := newFixtureEngine(t)

	// 1. Created (never started) container: reads directly from merged image layers.
	created := createFixture(t, te, "gvxd-created-cp", false)

	// HEAD single regular file.
	req, _ := http.NewRequest(http.MethodHead, te.srv.URL+"/v1.52/containers/"+created.ID+"/archive?path=/data/big.bin", nil)
	res, err := te.srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("HEAD created /data/big.bin = %d", res.StatusCode)
	}
	var ps archivePathStat
	dec, err := base64.StdEncoding.DecodeString(res.Header.Get(archivePathStatHeader))
	if err != nil {
		t.Fatalf("decode stat header: %v", err)
	}
	_ = json.Unmarshal(dec, &ps)
	if ps.Name != "big.bin" || ps.Size != 100000 || os.FileMode(ps.Mode).IsDir() {
		t.Fatalf("HEAD stat = %+v", ps)
	}

	// GET hard link whose original target (etc/a.txt) was deleted in layer 1.
	st, body := te.do(t, http.MethodGet, "/containers/"+created.ID+"/archive?path=/etc/hl", nil)
	if st != http.StatusOK {
		t.Fatalf("GET /etc/hl = %d %s", st, body)
	}
	names, ents := readTarEntries(t, body)
	if len(names) != 1 || names[0] != "hl" || ents["hl"].hdr.Typeflag != tar.TypeReg || string(ents["hl"].body) != "alpha\n" {
		t.Fatalf("GET /etc/hl tar = %v %+v", names, ents["hl"])
	}

	// GET deleted file (whiteout) -> 404.
	if st, _ := te.do(t, http.MethodGet, "/containers/"+created.ID+"/archive?path=/etc/a.txt", nil); st != http.StatusNotFound {
		t.Fatalf("GET whiteout /etc/a.txt = %d, want 404", st)
	}

	// GET symlink without trailing slash returns symlink itself.
	st, body = te.do(t, http.MethodGet, "/containers/"+created.ID+"/archive?path=/lib/link", nil)
	if st != http.StatusOK {
		t.Fatalf("GET /lib/link = %d %s", st, body)
	}
	names, ents = readTarEntries(t, body)
	if len(names) != 1 || ents["link"].hdr.Typeflag != tar.TypeSymlink || ents["link"].hdr.Linkname != "../etc/a.txt" {
		t.Fatalf("GET /lib/link = %v %+v", names, ents["link"])
	}

	// GET opaque directory subtree (/etc/sub): starts with sub/ and contains only y (not hidden x).
	st, body = te.do(t, http.MethodGet, "/containers/"+created.ID+"/archive?path=/etc/sub", nil)
	if st != http.StatusOK {
		t.Fatalf("GET /etc/sub = %d %s", st, body)
	}
	names, ents = readTarEntries(t, body)
	if len(names) == 0 || names[0] != "sub/" {
		t.Fatalf("GET /etc/sub first entry = %v, want sub/", names)
	}
	if _, ok := ents["sub/y"]; !ok {
		t.Fatalf("GET /etc/sub missing sub/y: %v", names)
	}
	if _, ok := ents["sub/x"]; ok {
		t.Fatalf("GET /etc/sub leaked hidden sub/x: %v", names)
	}

	// 2. Stopped container with ext4lite upper layer (modified etc/motd, added root/...).
	stopped := createFixture(t, te, "gvxd-stopped-cp", true)

	// GET upper-modified /etc/motd (layer.ext4 appends "\nx\n" to Alpine's motd).
	st, body = te.do(t, http.MethodGet, "/containers/"+stopped.ID+"/archive?path=/etc/motd", nil)
	if st != http.StatusOK {
		t.Fatalf("GET stopped /etc/motd = %d %s", st, body)
	}
	names, ents = readTarEntries(t, body)
	if len(names) != 1 || names[0] != "motd" || !strings.Contains(string(ents["motd"].body), "Welcome to Alpine!") || !strings.HasSuffix(string(ents["motd"].body), "\nx\n") {
		t.Fatalf("stopped /etc/motd = %v %q", names, ents["motd"].body)
	}

	// GET directory /etc spanning both image layers and upper layer.
	st, body = te.do(t, http.MethodGet, "/containers/"+stopped.ID+"/archive?path=/etc", nil)
	if st != http.StatusOK {
		t.Fatalf("GET stopped /etc = %d %s", st, body)
	}
	names, ents = readTarEntries(t, body)
	if len(names) == 0 || names[0] != "etc/" {
		t.Fatalf("stopped /etc first entry = %v, want etc/", names)
	}
	if !strings.Contains(string(ents["etc/motd"].body), "Welcome to Alpine!") || !strings.HasSuffix(string(ents["etc/motd"].body), "\nx\n") {
		t.Errorf("stopped /etc/motd not from upper: %q", ents["etc/motd"].body)
	}
	if string(ents["etc/hl"].body) != "alpha\n" {
		t.Errorf("stopped /etc/hl not promoted from layer 0: %+v %q", ents["etc/hl"].hdr, ents["etc/hl"].body)
	}

	// 3. End-to-end compatibility with GVX Desktop's imagefs.OpenDocker!
	host := "tcp://" + strings.TrimPrefix(te.srv.URL, "http://")
	dfs, err := imagefs.OpenDocker(context.Background(), host, "gvxd-fixture:1", nil)
	if err != nil {
		t.Fatalf("imagefs.OpenDocker against sandbox engine: %v", err)
	}
	defer dfs.Close()
	if len(dfs.Layers()) != 2 {
		t.Errorf("OpenDocker layers = %d, want 2", len(dfs.Layers()))
	}
	fc, err := dfs.ReadFile(context.Background(), "/opt/new", 0, 1024)
	if err != nil || string(fc.Data) != "new\n" {
		t.Errorf("OpenDocker ReadFile(/opt/new) = %q, %v; want new\\n", fc.Data, err)
	}
}

func TestVolumesFromInheritanceAndPrecedence(t *testing.T) {
	te := newTestEngine(t)
	te.fake.addImage("alpine:3.20", ImageConfig{
		Cmd:     []string{"/bin/sh"},
		Volumes: map[string]struct{}{"/anon": {}},
	})
	bindDir := t.TempDir()
	overrideDir := t.TempDir()

	// Create source container with named volume, bind mount, tmpfs, and image anonymous volume.
	st, srcResp, raw := createTestContainer(t, te, "gvxd-vf-src", map[string]any{
		"Image": "alpine:3.20",
		"HostConfig": map[string]any{
			"Binds": []string{
				"gvxd-vf-vol:/shared",
				bindDir + ":/hostdata",
			},
			"Tmpfs": map[string]string{"/cache": "size=16m"},
		},
	})
	if st != http.StatusCreated {
		t.Fatalf("create src = %d %s", st, raw)
	}
	src, err := te.lookup(srcResp.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(src.AnonymousVolumes) != 1 {
		t.Fatalf("src.AnonymousVolumes = %v, want 1", src.AnonymousVolumes)
	}
	srcAnon := src.AnonymousVolumes[0]

	// Create consumer with VolumesFrom: ["gvxd-vf-src:ro"] and explicit bind overriding /hostdata.
	st, dstResp, raw := createTestContainer(t, te, "gvxd-vf-dst", map[string]any{
		"Image": "alpine:3.20",
		"HostConfig": map[string]any{
			"Binds":       []string{overrideDir + ":/hostdata"},
			"VolumesFrom": []string{"gvxd-vf-src:ro"},
		},
	})
	if st != http.StatusCreated {
		t.Fatalf("create dst = %d %s", st, raw)
	}
	dst, err := te.lookup(dstResp.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(dst.AnonymousVolumes) != 0 {
		t.Errorf("dst should not own inherited anonymous volumes: %v", dst.AnonymousVolumes)
	}

	byDst := map[string]MountPoint{}
	for _, m := range dst.Mounts {
		byDst[m.Destination] = m
	}
	if m, ok := byDst["/shared"]; !ok || m.Name != "gvxd-vf-vol" || m.RW {
		t.Errorf("inherited /shared = %+v (ok=%v), want ro gvxd-vf-vol", m, ok)
	}
	if m, ok := byDst["/anon"]; !ok || m.Name != srcAnon || m.RW {
		t.Errorf("inherited /anon = %+v (ok=%v), want ro %s", m, ok, srcAnon)
	}
	if m, ok := byDst["/hostdata"]; !ok || m.Source != overrideDir || !m.RW {
		t.Errorf("explicit /hostdata should override VolumesFrom: %+v", m)
	}
	if m, ok := byDst["/cache"]; !ok || m.Type != "tmpfs" || m.RW {
		t.Errorf("inherited /cache = %+v, want ro tmpfs", m)
	}

	// Removing dst with v=1 must NOT delete src's anonymous volume.
	if st, b := te.do(t, http.MethodDelete, "/containers/"+dst.ID+"?v=1", nil); st != http.StatusNoContent {
		t.Fatalf("delete dst = %d %s", st, b)
	}
	if st, _ := te.do(t, http.MethodGet, "/volumes/"+srcAnon, nil); st != http.StatusOK {
		t.Fatalf("src anonymous volume %s was deleted when consumer was removed (%d)", srcAnon, st)
	}

	// Validation errors.
	if st, _ := te.do(t, http.MethodPost, "/containers/create", map[string]any{
		"Image":      "alpine:3.20",
		"HostConfig": map[string]any{"VolumesFrom": []string{"no-such-container"}},
	}); st != http.StatusNotFound {
		t.Errorf("missing volumes-from = %d, want 404", st)
	}
	if st, _ := te.do(t, http.MethodPost, "/containers/create", map[string]any{
		"Image":      "alpine:3.20",
		"HostConfig": map[string]any{"VolumesFrom": []string{"gvxd-vf-src:invalid"}},
	}); st != http.StatusBadRequest {
		t.Errorf("bad volumes-from mode = %d, want 400", st)
	}
}

func TestContainerSizesBeforeSinceFiltersAndUpdateCPU(t *testing.T) {
	te, _ := newFixtureEngine(t)

	c1 := createFixture(t, te, "gvxd-ord-1", true)
	c2 := createFixture(t, te, "gvxd-ord-2", false)
	c3 := createFixture(t, te, "gvxd-ord-3", false)

	// Write a non-empty file at c2's rootfs path to verify size calculation + prune reclaim.
	if err := os.MkdirAll(filepath.Dir(c2.Spec.RootfsVolume), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(c2.Spec.RootfsVolume, bytes.Repeat([]byte("A"), 16384), 0o600); err != nil {
		t.Fatal(err)
	}
	wantC1 := allocatedFileBytes(c1.Spec.RootfsVolume)
	wantC2 := allocatedFileBytes(c2.Spec.RootfsVolume)
	if wantC1 <= 0 || wantC2 <= 0 {
		t.Fatalf("expected allocated bytes > 0, got c1=%d c2=%d", wantC1, wantC2)
	}

	// GET /containers/json?all=1&size=1
	st, body := te.do(t, http.MethodGet, "/containers/json?all=1&size=1", nil)
	if st != http.StatusOK {
		t.Fatalf("list size=1 = %d %s", st, body)
	}
	var list []containerListItem
	_ = json.Unmarshal(body, &list)
	byID := map[string]containerListItem{}
	for _, it := range list {
		byID[it.ID] = it
	}
	if byID[c1.ID].SizeRw != wantC1 || byID[c2.ID].SizeRw != wantC2 || byID[c3.ID].SizeRw != 0 {
		t.Errorf("list sizes = c1:%d c2:%d c3:%d, want %d %d 0", byID[c1.ID].SizeRw, byID[c2.ID].SizeRw, byID[c3.ID].SizeRw, wantC1, wantC2)
	}

	// GET /containers/{id}/json?size=1
	st, body = te.do(t, http.MethodGet, "/containers/"+c1.ID+"/json?size=1", nil)
	if st != http.StatusOK {
		t.Fatalf("inspect size=1 = %d %s", st, body)
	}
	var ins containerInspectResponse
	_ = json.Unmarshal(body, &ins)
	if ins.SizeRw == nil || *ins.SizeRw != wantC1 || ins.SizeRootFs == nil || *ins.SizeRootFs < wantC1 {
		t.Errorf("inspect sizes = rw:%v rootfs:%v, want %d", ins.SizeRw, ins.SizeRootFs, wantC1)
	}

	// GET /system/df reports non-zero ContainerUsage.TotalSize and Reclaimable.
	st, body = te.do(t, http.MethodGet, "/system/df", nil)
	if st != http.StatusOK {
		t.Fatalf("system/df = %d %s", st, body)
	}
	var df struct {
		ContainerUsage struct {
			TotalSize   int64 `json:"TotalSize"`
			Reclaimable int64 `json:"Reclaimable"`
		} `json:"ContainerUsage"`
	}
	_ = json.Unmarshal(body, &df)
	if df.ContainerUsage.TotalSize != wantC1+wantC2 || df.ContainerUsage.Reclaimable != wantC1+wantC2 {
		t.Errorf("system/df ContainerUsage = %+v, want %d", df.ContainerUsage, wantC1+wantC2)
	}

	// Filters: before=gvxd-ord-2 -> only c1; since=gvxd-ord-2 -> only c3.
	fq := url.QueryEscape(`{"before":["gvxd-ord-2"]}`)
	st, body = te.do(t, http.MethodGet, "/containers/json?all=1&filters="+fq, nil)
	if st != http.StatusOK {
		t.Fatalf("before filter = %d %s", st, body)
	}
	list = nil
	_ = json.Unmarshal(body, &list)
	if len(list) != 1 || list[0].ID != c1.ID {
		t.Errorf("before=gvxd-ord-2 = %+v, want [%s]", list, c1.ID)
	}

	fq = url.QueryEscape(`{"since":["gvxd-ord-2"]}`)
	st, body = te.do(t, http.MethodGet, "/containers/json?all=1&filters="+fq, nil)
	if st != http.StatusOK {
		t.Fatalf("since filter = %d %s", st, body)
	}
	list = nil
	_ = json.Unmarshal(body, &list)
	if len(list) != 1 || list[0].ID != c3.ID {
		t.Errorf("since=gvxd-ord-2 = %+v, want [%s]", list, c3.ID)
	}

	// Unknown before anchor -> 404.
	fq = url.QueryEscape(`{"before":["no-such-ctr"]}`)
	if st, _ := te.do(t, http.MethodGet, "/containers/json?all=1&filters="+fq, nil); st != http.StatusNotFound {
		t.Errorf("unknown before anchor = %d, want 404", st)
	}

	// containerUpdate switching between NanoCpus and CpuQuota/CpuPeriod stays consistent.
	if st, b := te.do(t, http.MethodPost, "/containers/"+c3.ID+"/update", map[string]any{
		"CpuQuota":  50000,
		"CpuPeriod": 100000,
	}); st != http.StatusOK {
		t.Fatalf("update quota = %d %s", st, b)
	}
	if st, b := te.do(t, http.MethodPost, "/containers/"+c3.ID+"/update", map[string]any{
		"CpuPeriod": 50000,
	}); st != http.StatusOK {
		t.Fatalf("update period = %d %s", st, b)
	}
	gotC3, _ := te.lookup(c3.ID)
	if gotC3.Spec.CPUs != "1" {
		t.Errorf("after period=50000 with quota=50000, Spec.CPUs = %q, want 1", gotC3.Spec.CPUs)
	}
	if st, b := te.do(t, http.MethodPost, "/containers/"+c3.ID+"/update", map[string]any{
		"NanoCpus": 2_000_000_000,
	}); st != http.StatusOK {
		t.Fatalf("update nanocpus = %d %s", st, b)
	}
	gotC3, _ = te.lookup(c3.ID)
	if gotC3.HostConfig.CpuQuota != 0 || gotC3.HostConfig.CpuPeriod != 0 || gotC3.Spec.CPUs != "2" {
		t.Errorf("after NanoCpus update: quota=%d period=%d cpus=%q", gotC3.HostConfig.CpuQuota, gotC3.HostConfig.CpuPeriod, gotC3.Spec.CPUs)
	}

	// POST /containers/prune reports SpaceReclaimed >= wantC1 + wantC2.
	st, body = te.do(t, http.MethodPost, "/containers/prune", nil)
	if st != http.StatusOK {
		t.Fatalf("prune = %d %s", st, body)
	}
	var pruned containerPruneResponse
	_ = json.Unmarshal(body, &pruned)
	if len(pruned.ContainersDeleted) != 3 || pruned.SpaceReclaimed != uint64(wantC1+wantC2) {
		t.Errorf("prune = %+v, want 3 deleted and reclaimed=%d", pruned, wantC1+wantC2)
	}
}
