package engine

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/ironpark/gvx-desktop/internal/imagefs"
)

// The fixtures come from the packages that read them:
//
//   - ../imagefs/testdata/gvxcache is a rungv layer cache with one image of
//     two layers (see its TestGVXImageLayers): etc/{hl, motd, sub/y},
//     lib/link, dev/{null, fifo}, data/big.bin and opt/new, after layer 1
//     deleted etc/a.txt (hl was its hard link) and usr/bin.
//   - ../ext4lite/testdata/layer.ext4.gz is a root file system volume that
//     rungv wrote for an alpine run (see its TestWriteLayer): it added
//     root/..., changed etc/motd, deleted etc/issue and usr/share/apk,
//     replaced etc/apk, and holds rungv's etc/hosts, etc/hostname,
//     etc/resolv.conf and an empty workspace/.
const (
	fixtureCache   = "../imagefs/testdata/gvxcache"
	fixtureImageID = "sha256:747db5a176c526f732dd8cfbc736d2667b809ddad2e05a75129b8e1a236e89a1"
	fixtureVolume  = "../ext4lite/testdata/layer.ext4.gz"
)

// newFixtureEngine is a v0.20 engine whose image gvxd-fixture:1 is the
// cache fixture's.
func newFixtureEngine(t *testing.T) (*testEngine, *fakeControl) {
	t.Helper()
	te, ctl := newControlTestEngine(t)
	te.fake.mu.Lock()
	te.fake.layerCacheDir = fixtureCache
	img := ImageInspect{ID: fixtureImageID, RepoTags: []string{"gvxd-fixture:1"}, Architecture: "arm64", Os: "linux",
		Config: ImageConfig{Cmd: []string{"/usr/bin/tool"}, Env: []string{"PATH=/usr/bin:/bin", "A=1"}, WorkingDir: "/data"}}
	te.fake.images["gvxd-fixture:1"] = img
	te.fake.images[fixtureImageID] = img
	te.fake.mu.Unlock()
	return te, ctl
}

// createFixture creates a container of gvxd-fixture:1 and, with volume,
// gives it the volume fixture as its root file system, as a stopped
// container that ran has.
func createFixture(t *testing.T, te *testEngine, name string, volume bool) Container {
	t.Helper()
	st, created, raw := createTestContainer(t, te, name, map[string]any{"Image": "gvxd-fixture:1", "Env": []string{"B=2"}})
	if st != http.StatusCreated {
		t.Fatalf("create: %d %s", st, raw)
	}
	c, err := te.lookup(created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if volume {
		ungzipTo(t, fixtureVolume, c.Spec.RootfsVolume)
	}
	return c
}

func ungzipTo(t *testing.T, src, dst string) {
	t.Helper()
	f, err := os.Open(src)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	zr, err := gzip.NewReader(f)
	if err != nil {
		t.Fatal(err)
	}
	w, err := os.Create(dst)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.Copy(w, zr); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
}

type testEntry struct {
	hdr  tar.Header
	body []byte
}

// tarOf builds a tar of entries: name, then "dir", "wh" (empty file),
// "link:<target>", or the content.
func tarOf(t *testing.T, entries ...[2]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for _, e := range entries {
		h := &tar.Header{Name: e[0], Mode: 0o644, ModTime: time.Unix(1700000000, 0)}
		var body []byte
		switch {
		case e[1] == "dir":
			h.Typeflag, h.Mode = tar.TypeDir, 0o755
		case strings.HasPrefix(e[1], "link:"):
			h.Typeflag, h.Linkname = tar.TypeLink, strings.TrimPrefix(e[1], "link:")
		default:
			h.Typeflag = tar.TypeReg
			if e[1] != "wh" {
				body = []byte(e[1])
			}
			h.Size = int64(len(body))
		}
		if err := tw.WriteHeader(h); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write(body); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func readTarEntries(t *testing.T, b []byte) ([]string, map[string]testEntry) {
	t.Helper()
	var names []string
	m := map[string]testEntry{}
	tr := tar.NewReader(bytes.NewReader(b))
	for {
		h, err := tr.Next()
		if err == io.EOF {
			return names, m
		}
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(tr)
		if _, dup := m[h.Name]; dup {
			t.Errorf("%s is in the archive twice", h.Name)
		}
		names = append(names, h.Name)
		m[h.Name] = testEntry{*h, body}
	}
}

// Exists reports whether the merged file system has name ("etc/hosts").
func (t *layerTree) Exists(name string) bool {
	return t.lookup(layerName(name), false) != nil
}

func TestLayerTreeMerge(t *testing.T) {
	layers := [][]byte{
		tarOf(t,
			[2]string{"a/", "dir"}, [2]string{"a/x", "x0"}, [2]string{"a/y", "y0"},
			[2]string{"b/", "dir"}, [2]string{"b/c/", "dir"}, [2]string{"b/c/deep", "d"},
			[2]string{"f", "file"}, [2]string{"g/", "dir"}, [2]string{"g/in", "in"},
			[2]string{"h", "shared"}, [2]string{"h2", "link:h"}, [2]string{"h3", "link:h"},
		),
		tarOf(t,
			[2]string{"a/.wh.x", "wh"},                                  // delete a file
			[2]string{"b/.wh..wh..opq", "wh"}, [2]string{"b/new", "n1"}, // hide b's lower content
			[2]string{"g", "now a file"},                         // a file replaces a directory
			[2]string{"h", "replaced"},                           // h2 and h3 keep the old content
			[2]string{"deep/er/than/before", "implicit parents"}, // no directory entries
		),
		tarOf(t, [2]string{".wh.f", "wh"}, [2]string{"a/y", "y2"}, [2]string{"b/.wh.new", "wh"}),
	}
	tree := newLayerTree()
	for i, l := range layers {
		if err := tree.Apply(i, tar.NewReader(bytes.NewReader(l))); err != nil {
			t.Fatal(err)
		}
	}
	for name, want := range map[string]bool{
		"a": true, "a/x": false, "a/y": true, "b": true, "b/c": false, "b/c/deep": false, "b/new": false,
		"f": false, "g": true, "g/in": false, "h": true, "h2": true, "deep/er/than/before": true, "deep/er": true,
	} {
		if got := tree.Exists(name); got != want {
			t.Errorf("Exists(%s) = %v, want %v", name, got, want)
		}
	}

	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for i, l := range layers {
		if err := tree.Emit(i, tar.NewReader(bytes.NewReader(l)), tw); err != nil {
			t.Fatal(err)
		}
	}
	tw.Close()
	names, m := readTarEntries(t, buf.Bytes())
	slices.Sort(names)
	want := []string{"a/", "a/y", "b/", "deep/er/than/before", "g", "h", "h2", "h3"}
	if !slices.Equal(names, want) {
		t.Fatalf("merged:\n got %q\nwant %q", names, want)
	}
	for name, body := range map[string]string{"a/y": "y2", "g": "now a file", "h": "replaced", "h2": "shared"} {
		if got := string(m[name].body); got != body {
			t.Errorf("%s = %q, want %q", name, got, body)
		}
	}
	if e := m["h3"]; e.hdr.Typeflag != tar.TypeLink || e.hdr.Linkname != "h2" {
		t.Errorf("h3 = %+v, want a link to h2, which holds the replaced h's content", e.hdr)
	}
}

func TestFilterUpper(t *testing.T) {
	in := tarOf(t,
		[2]string{"etc/", "dir"}, [2]string{"etc/hosts", "h"}, [2]string{"etc/hostname", "n"}, [2]string{"etc/resolv.conf", "r"},
		[2]string{"workspace/", "dir"}, [2]string{"workspace/.wh..wh..opq", "wh"},
		[2]string{"usr/", "dir"}, [2]string{"usr/bin/", "dir"}, [2]string{"usr/bin/.wh..wh..opq", "wh"},
		[2]string{"root/", "dir"}, [2]string{"root/.wh..wh..opq", "wh"}, [2]string{"root/kept", "k"},
		[2]string{"tmp/", "dir"}, [2]string{"tmp/sub/", "dir"},
		[2]string{"opt/", "dir"}, [2]string{"opt/.wh..wh..opq", "wh"},
	)
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	if err := filterUpper(bytes.NewReader(in), tw); err != nil {
		t.Fatal(err)
	}
	tw.Close()
	names, _ := readTarEntries(t, buf.Bytes())
	want := []string{
		"root/", "root/.wh..wh..opq", "root/kept", // a runtime directory with something else in it
		"tmp/", "tmp/sub/", // so is an empty directory below one
		"opt/", "opt/.wh..wh..opq", // not a runtime directory
	}
	if !slices.Equal(names, want) {
		t.Errorf("filtered:\n got %q\nwant %q", names, want)
	}
}

func TestRootfsVolumes(t *testing.T) {
	te, _ := newControlTestEngine(t)
	st, c1, raw := createTestContainer(t, te, "gvxd-rfs", map[string]any{"Image": "alpine:3.20"})
	if st != http.StatusCreated {
		t.Fatalf("create: %d %s", st, raw)
	}
	got, _ := te.lookup(c1.ID)
	if want := filepath.Join(te.dir, "rootfs", c1.ID+".ext4"); got.Spec.RootfsVolume != want {
		t.Fatalf("RootfsVolume = %q, want %q", got.Spec.RootfsVolume, want)
	}
	_, ro, _ := createTestContainer(t, te, "gvxd-ro", map[string]any{"Image": "alpine:3.20", "HostConfig": map[string]any{"ReadonlyRootfs": true}})
	if c, _ := te.lookup(ro.ID); c.Spec.RootfsVolume != "" {
		t.Errorf("a read-only container got the volume %q", c.Spec.RootfsVolume)
	}

	// rm removes the volume.
	if err := os.WriteFile(got.Spec.RootfsVolume, []byte("volume"), 0o600); err != nil {
		t.Fatal(err)
	}
	if st, raw := te.do(t, http.MethodDelete, "/containers/"+c1.ID, nil); st != http.StatusNoContent {
		t.Fatalf("rm: %d %s", st, raw)
	}
	if _, err := os.Stat(got.Spec.RootfsVolume); !os.IsNotExist(err) {
		t.Errorf("the volume outlived its container: %v", err)
	}

	// Open removes volumes no container has, and keeps the others'.
	_, c2, _ := createTestContainer(t, te, "gvxd-keep", map[string]any{"Image": "alpine:3.20"})
	kept, _ := te.lookup(c2.ID)
	orphan := filepath.Join(te.dir, "rootfs", strings.Repeat("ab", 32)+".ext4")
	for _, p := range []string{kept.Spec.RootfsVolume, orphan} {
		if err := os.WriteFile(p, []byte("volume"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	te.srv.Close()
	_ = te.Close(time.Second)
	reopenTestEngine(t, te.dir, te.fake)
	if _, err := os.Stat(orphan); !os.IsNotExist(err) {
		t.Errorf("an orphan volume survived Open: %v", err)
	}
	if _, err := os.Stat(kept.Spec.RootfsVolume); err != nil {
		t.Errorf("a container's volume did not survive Open: %v", err)
	}

	// Without rungv's control (v0.19), containers keep their writes in memory.
	te19 := newTestEngine(t)
	te19.fake.addImage("alpine:3.20", ImageConfig{Cmd: []string{"sh"}})
	_, c3, _ := createTestContainer(t, te19, "gvxd-v19", map[string]any{"Image": "alpine:3.20"})
	if c, _ := te19.lookup(c3.ID); c.Spec.RootfsVolume != "" {
		t.Errorf("a v0.19 container got the volume %q", c.Spec.RootfsVolume)
	}
	for _, p := range []string{"/containers/" + c3.ID + "/changes", "/containers/" + c3.ID + "/export"} {
		if st, _ := te19.do(t, http.MethodGet, p, nil); st != http.StatusNotImplemented {
			t.Errorf("GET %s on v0.19 = %d, want 501", p, st)
		}
	}
}

func changesMap(t *testing.T, raw []byte) map[string]int {
	t.Helper()
	var list []change
	if err := json.Unmarshal(raw, &list); err != nil {
		t.Fatalf("%v: %s", err, raw)
	}
	m := map[string]int{}
	for _, c := range list {
		m[c.Path] = c.Kind
	}
	return m
}

// TestChangesStopped reads a stopped container's changes from its volume.
func TestChangesStopped(t *testing.T) {
	te, _ := newFixtureEngine(t)
	c := createFixture(t, te, "gvxd-diff", true)
	st, raw := te.do(t, http.MethodGet, "/containers/"+c.ID+"/changes", nil)
	if st != http.StatusOK {
		t.Fatalf("changes: %d %s", st, raw)
	}
	m := changesMap(t, raw)
	want := map[string]int{
		"/etc": changeModified, "/etc/motd": changeModified, "/etc/issue": changeDeleted,
		"/etc/apk": changeAdded, "/etc/apk/world": changeAdded,
		"/root": changeAdded, "/root/new.txt": changeAdded, "/root/seq.txt": changeAdded,
		"/usr": changeModified, "/usr/share": changeAdded, "/usr/share/apk": changeDeleted,
	}
	for p, k := range want {
		if got, ok := m[p]; !ok || got != k {
			t.Errorf("%s: kind %d (listed %v), want %d", p, got, ok, k)
		}
	}
	for _, p := range []string{"/etc/hosts", "/etc/hostname", "/etc/resolv.conf", "/workspace", "/etc/apk/.wh..wh..opq"} {
		if _, ok := m[p]; ok {
			t.Errorf("%s is listed", p)
		}
	}
	var list []change
	_ = json.Unmarshal(raw, &list)
	if !slices.IsSortedFunc(list, func(a, b change) int { return strings.Compare(a.Path, b.Path) }) {
		t.Error("the changes are not in path order")
	}

	// A container that never started has none; one whose changes were not
	// kept cannot say.
	fresh := createFixture(t, te, "gvxd-fresh", false)
	if st, raw := te.do(t, http.MethodGet, "/containers/"+fresh.ID+"/changes", nil); st != http.StatusOK || strings.TrimSpace(string(raw)) != "[]" {
		t.Errorf("changes of a new container: %d %s", st, raw)
	}
	te.ctr.mu.Lock()
	te.ctr.byID[fresh.ID].Spec.RootfsVolume = ""
	te.ctr.mu.Unlock()
	if st, raw := te.do(t, http.MethodGet, "/containers/"+fresh.ID+"/changes", nil); st != http.StatusConflict || !strings.Contains(string(raw), "were not kept") {
		t.Errorf("changes of a container without a volume: %d %s", st, raw)
	}
}

// TestChangesRunning reads a running container's changes from rungv's
// export of its upper layer.
func TestChangesRunning(t *testing.T) {
	te, ctl := newFixtureEngine(t)
	upper := tarOf(t,
		[2]string{"etc/", "dir"}, [2]string{"etc/hosts", "h"}, [2]string{"etc/.wh.hl", "wh"}, [2]string{"etc/added", "a"},
		[2]string{"data/", "dir"}, [2]string{"data/big.bin", "smaller"},
		[2]string{"workspace/", "dir"}, [2]string{"workspace/.wh..wh..opq", "wh"},
	)
	ctl.exportFn = func(ctx context.Context, path string, upperOnly bool) (io.ReadCloser, error) {
		return io.NopCloser(bytes.NewReader(upper)), nil
	}
	id := createAndStartContainer(t, te, "gvxd-diff-run", map[string]any{"Image": "gvxd-fixture:1"})
	st, raw := te.do(t, http.MethodGet, "/containers/"+id+"/changes", nil)
	if st != http.StatusOK {
		t.Fatalf("changes: %d %s", st, raw)
	}
	m := changesMap(t, raw)
	want := map[string]int{"/etc": changeModified, "/etc/hl": changeDeleted, "/etc/added": changeAdded, "/data": changeModified, "/data/big.bin": changeModified}
	if len(m) != len(want) {
		t.Errorf("changes = %v, want %v", m, want)
	}
	for p, k := range want {
		if m[p] != k {
			t.Errorf("%s: kind %d, want %d", p, m[p], k)
		}
	}
	if len(ctl.exportCalls) != 1 || !ctl.exportCalls[0].UpperOnly || ctl.exportCalls[0].Path != "/" {
		t.Errorf("export calls = %+v, want one of / with upperOnly", ctl.exportCalls)
	}
}

// TestExportStopped merges a stopped container's image with its changes.
func TestExportStopped(t *testing.T) {
	te, _ := newFixtureEngine(t)
	c := createFixture(t, te, "gvxd-export", true)
	st, raw := te.do(t, http.MethodGet, "/containers/"+c.ID+"/export", nil)
	if st != http.StatusOK {
		t.Fatalf("export: %d %s", st, raw)
	}
	names, m := readTarEntries(t, raw)
	set := map[string]bool{}
	for _, n := range names {
		set[n] = true
	}
	for _, n := range []string{
		"data/", "data/big.bin", "dev/null", "dev/fifo", "lib/link", "etc/hl", "etc/sub/y", "opt/new", // the image's
		"root/new.txt", "root/seq.txt", "etc/apk/world", "usr/share/", // the changes
	} {
		if !set[n] {
			t.Errorf("%s is missing", n)
		}
	}
	for _, n := range []string{
		"etc/a.txt", "etc/sub/x", "usr/bin/", "usr/bin/tool", // the image deleted them
		"etc/hosts", "workspace/", // rungv's
	} {
		if set[n] {
			t.Errorf("%s is in the export", n)
		}
	}
	for _, n := range names {
		if strings.Contains(n, ".wh.") {
			t.Errorf("a whiteout is in the export: %s", n)
		}
	}
	if got := string(m["etc/motd"].body); !strings.HasSuffix(got, "x\n") {
		t.Errorf("etc/motd is the image's, not the changed one: %q", got)
	}
	if e := m["etc/hl"]; e.hdr.Typeflag != tar.TypeReg || string(e.body) != "alpha\n" {
		t.Errorf("etc/hl, a link of the deleted etc/a.txt, = %+v %q; want a file with its content", e.hdr, e.body)
	}
	if e := m["data/big.bin"]; len(e.body) != 100000 || e.hdr.Uid != 1000 {
		t.Errorf("data/big.bin: %d bytes, uid %d", len(e.body), e.hdr.Uid)
	}
}

func TestExportRunning(t *testing.T) {
	te, ctl := newFixtureEngine(t)
	full := tarOf(t,
		[2]string{"etc/", "dir"}, [2]string{"etc/motd", "m"},
		[2]string{"data/", "dir"}, [2]string{"data/in-volume", "v"}, [2]string{"data/sub/", "dir"},
	)
	ctl.exportFn = func(ctx context.Context, path string, upperOnly bool) (io.ReadCloser, error) {
		return io.NopCloser(bytes.NewReader(full)), nil
	}
	id := createAndStartContainer(t, te, "gvxd-export-run", map[string]any{
		"Image": "gvxd-fixture:1", "HostConfig": map[string]any{"Binds": []string{"gvxd-vol:/data"}},
	})
	st, raw := te.do(t, http.MethodGet, "/containers/"+id+"/export", nil)
	if st != http.StatusOK {
		t.Fatalf("export: %d %s", st, raw)
	}
	names, _ := readTarEntries(t, raw)
	if want := []string{"etc/", "etc/motd", "data/"}; !slices.Equal(names, want) {
		t.Errorf("export = %q, want %q (the volume's content left out)", names, want)
	}
	if len(ctl.exportCalls) != 1 || ctl.exportCalls[0].UpperOnly {
		t.Errorf("export calls = %+v", ctl.exportCalls)
	}
}

func commitPath(id string, q url.Values) string {
	q.Set("container", id)
	return "/commit?" + q.Encode()
}

// loadedImage reads the last archive the fake loaded, an OCI image layout.
func loadedImage(t *testing.T, te *testEngine) (index, manifest, config map[string]any, layer map[string]testEntry, raw map[string][]byte) {
	t.Helper()
	te.fake.mu.Lock()
	if len(te.fake.loaded) == 0 {
		te.fake.mu.Unlock()
		t.Fatal("nothing was loaded")
	}
	b := te.fake.loaded[len(te.fake.loaded)-1]
	te.fake.mu.Unlock()
	_, files := readTarEntries(t, b)
	raw = map[string][]byte{}
	for n, e := range files {
		raw[n] = e.body
	}
	blob := func(d any) []byte {
		name := "blobs/sha256/" + strings.TrimPrefix(d.(string), "sha256:")
		body, ok := raw[name]
		if !ok {
			t.Fatalf("the archive lacks %s", name)
		}
		sum := sha256.Sum256(body)
		if "sha256:"+hex.EncodeToString(sum[:]) != d {
			t.Fatalf("%s does not match its digest", name)
		}
		return body
	}
	mustJSONTo := func(b []byte, v any) {
		if err := json.Unmarshal(b, v); err != nil {
			t.Fatal(err)
		}
	}
	mustJSONTo(raw["index.json"], &index)
	desc := index["manifests"].([]any)[0].(map[string]any)
	mustJSONTo(blob(desc["digest"]), &manifest)
	mustJSONTo(blob(manifest["config"].(map[string]any)["digest"]), &config)
	layers := manifest["layers"].([]any)
	_, layer = readTarEntries(t, blob(layers[len(layers)-1].(map[string]any)["digest"]))
	return index, manifest, config, layer, raw
}

func TestCommitStopped(t *testing.T) {
	te, _ := newFixtureEngine(t)
	c := createFixture(t, te, "gvxd-commit", true)
	q := url.Values{"repo": {"gvxd-new"}, "tag": {"1"}, "comment": {"a comment"}, "author": {"someone"},
		"changes": {`CMD ["echo","hi"]`, "ENV C=3\nLABEL made=commit"}}
	st, raw := te.do(t, http.MethodPost, commitPath(c.ID, q), nil)
	if st != http.StatusCreated {
		t.Fatalf("commit: %d %s", st, raw)
	}
	var resp commitResponse
	_ = json.Unmarshal(raw, &resp)

	index, man, cfg, layer, files := loadedImage(t, te)
	if got := string(files["oci-layout"]); got != `{"imageLayoutVersion":"1.0.0"}` {
		t.Errorf("oci-layout = %q", got)
	}
	cfgDesc := man["config"].(map[string]any)
	if resp.ID != cfgDesc["digest"] {
		t.Errorf("Id = %s, want the config's digest %s", resp.ID, cfgDesc["digest"])
	}
	desc := index["manifests"].([]any)[0].(map[string]any)
	if name := desc["annotations"].(map[string]any)["io.containerd.image.name"]; name != "gvxd-new:1" {
		t.Errorf("the image's name = %v", name)
	}
	img, err := imagefs.OpenGVXImage(fixtureCache, fixtureImageID)
	if err != nil {
		t.Fatal(err)
	}
	baseDescs := img.LayerDescriptors
	img.Close()
	layers := man["layers"].([]any)
	if len(layers) != 3 {
		t.Fatalf("layers = %v, want the image's two and the new one", layers)
	}
	for i, d := range baseDescs {
		var want map[string]any
		_ = json.Unmarshal(d, &want)
		if layers[i].(map[string]any)["digest"] != want["digest"] {
			t.Errorf("layer %d = %v, want the cache's descriptor %v", i, layers[i], want)
		}
		if _, ok := files["blobs/sha256/"+strings.TrimPrefix(want["digest"].(string), "sha256:")]; ok {
			t.Errorf("the archive holds the image's layer %d; rungv has it", i)
		}
	}
	newDesc := layers[2].(map[string]any)
	if newDesc["mediaType"] != "application/vnd.oci.image.layer.v1.tar" {
		t.Errorf("the new layer's media type = %v", newDesc["mediaType"])
	}

	diffIDs := cfg["rootfs"].(map[string]any)["diff_ids"].([]any)
	if len(diffIDs) != 3 || diffIDs[2] != newDesc["digest"] {
		t.Errorf("diff_ids = %v; the last must be the uncompressed new layer's digest %v", diffIDs, newDesc["digest"])
	}
	if h := cfg["history"].([]any); len(h) != 3 || h[2].(map[string]any)["comment"] != "a comment" || h[2].(map[string]any)["author"] != "someone" {
		t.Errorf("history = %v", h)
	}
	if cfg["author"] != "someone" || cfg["x-unknown-field"] == nil || cfg["architecture"] != "arm64" {
		t.Errorf("config keeps the image's fields and adds the author: %v", cfg)
	}
	run := cfg["config"].(map[string]any)
	if cmd := run["Cmd"].([]any); len(cmd) != 2 || cmd[0] != "echo" {
		t.Errorf("Cmd = %v", cmd)
	}
	env := run["Env"].([]any)
	for _, kv := range []string{"C=3", "B=2", "A=1"} {
		if !slices.Contains(env, any(kv)) {
			t.Errorf("Env %v lacks %s", env, kv)
		}
	}
	if run["Labels"].(map[string]any)["made"] != "commit" {
		t.Errorf("Labels = %v", run["Labels"])
	}

	if _, ok := layer["root/seq.txt"]; !ok {
		t.Error("the new layer lacks root/seq.txt")
	}
	if _, ok := layer["etc/.wh.issue"]; !ok {
		t.Error("the new layer lacks the whiteout of etc/issue")
	}
	for _, n := range []string{"etc/hosts", "etc/hostname", "etc/resolv.conf", "workspace/"} {
		if _, ok := layer[n]; ok {
			t.Errorf("the new layer holds rungv's %s", n)
		}
	}
	if _, err := te.imageConfig(context.Background(), "gvxd-new:1"); err != nil {
		t.Errorf("the committed image is not listed: %v", err)
	}
}

func TestCommitRunningPauses(t *testing.T) {
	te, ctl := newFixtureEngine(t)
	id := createAndStartContainer(t, te, "gvxd-commit-run", map[string]any{"Image": "gvxd-fixture:1"})
	ctl.pausedNow = func() bool { return te.runner.State(id).Paused }
	ctl.exportFn = func(ctx context.Context, path string, upperOnly bool) (io.ReadCloser, error) {
		return io.NopCloser(bytes.NewReader(tarOf(t, [2]string{"opt/", "dir"}, [2]string{"opt/more", "m"}))), nil
	}
	if st, raw := te.do(t, http.MethodPost, commitPath(id, url.Values{}), nil); st != http.StatusCreated {
		t.Fatalf("commit: %d %s", st, raw)
	}
	if len(ctl.exportCalls) != 1 || !ctl.exportCalls[0].Paused || !ctl.exportCalls[0].UpperOnly {
		t.Errorf("export calls = %+v, want one of the upper layer while paused", ctl.exportCalls)
	}
	if te.runner.State(id).Paused {
		t.Error("the container stayed paused")
	}
	index, _, _, layer, _ := loadedImage(t, te)
	if desc := index["manifests"].([]any)[0].(map[string]any); desc["annotations"] != nil {
		t.Errorf("an untagged commit has the annotations %v", desc["annotations"])
	}
	if string(layer["opt/more"].body) != "m" {
		t.Errorf("layer = %v", layer)
	}

	if st, _ := te.do(t, http.MethodPost, commitPath(id, url.Values{"pause": {"0"}}), nil); st != http.StatusCreated {
		t.Fatal("commit with pause=0 failed")
	}
	if ctl.exportCalls[1].Paused {
		t.Error("pause=0 paused the container")
	}

	for _, q := range []url.Values{{"tag": {"x"}}, {"repo": {"Upper"}}, {"changes": {"RUN rm -rf /"}}} {
		if st, raw := te.do(t, http.MethodPost, commitPath(id, q), nil); st != http.StatusBadRequest {
			t.Errorf("commit %v = %d %s, want 400", q, st, raw)
		}
	}
	if st, _ := te.do(t, http.MethodPost, commitPath("nope", url.Values{}), nil); st != http.StatusNotFound {
		t.Errorf("commit of a missing container = %d, want 404", st)
	}
}

func TestApplyChanges(t *testing.T) {
	cfg := ContainerConfig{Cmd: []string{"old"}, WorkingDir: "/srv", Env: []string{"A=1"}}
	err := applyChanges(&cfg, []string{
		"ENTRYPOINT /run.sh",
		`ENV A=2 B="two words" C=it\'s`,
		"ENV D old form value",
		`LABEL "x y"=1 z='q r'`,
		"EXPOSE 80 53/udp",
		`VOLUME ["/a", "/b"]`,
		"VOLUME /c",
		"WORKDIR sub",
		"USER app:app",
		"STOPSIGNAL SIGINT",
		"# a comment",
	})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(cfg.Entrypoint, []string{"/bin/sh", "-c", "/run.sh"}) || cfg.Cmd != nil {
		t.Errorf("Entrypoint %q, Cmd %q: ENTRYPOINT in shell form, and it clears CMD", cfg.Entrypoint, cfg.Cmd)
	}
	if !slices.Equal(cfg.Env, []string{"A=2", "B=two words", "C=it's", "D=old form value"}) {
		t.Errorf("Env = %q", cfg.Env)
	}
	if cfg.Labels["x y"] != "1" || cfg.Labels["z"] != "q r" {
		t.Errorf("Labels = %q", cfg.Labels)
	}
	for _, p := range []string{"80/tcp", "53/udp"} {
		if _, ok := cfg.ExposedPorts[p]; !ok {
			t.Errorf("ExposedPorts lacks %s", p)
		}
	}
	if len(cfg.Volumes) != 3 || cfg.WorkingDir != "/srv/sub" || cfg.User != "app:app" || cfg.StopSignal != "SIGINT" {
		t.Errorf("Volumes %v, WorkingDir %q, User %q, StopSignal %q", cfg.Volumes, cfg.WorkingDir, cfg.User, cfg.StopSignal)
	}

	cfg = ContainerConfig{}
	if err := applyChanges(&cfg, []string{`CMD ["a", "b"]`, `ENTRYPOINT ["e"]`}); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(cfg.Cmd, []string{"a", "b"}) {
		t.Errorf("an ENTRYPOINT after a CMD of the same commit cleared it: %q", cfg.Cmd)
	}
	for _, bad := range []string{"RUN true", "FROM x", `ENV "unterminated`, "LABEL novalue"} {
		if err := applyChanges(&ContainerConfig{}, []string{bad}); err == nil {
			t.Errorf("%q was accepted", bad)
		}
	}
}

func TestMergeCommitConfig(t *testing.T) {
	from := ContainerConfig{
		User: "u", Env: []string{"A=1", "B=1"}, Cmd: []string{"c"}, Entrypoint: []string{"e"},
		WorkingDir: "/w", Labels: map[string]string{"l": "1", "m": "1"}, StopSignal: "SIGTERM",
		ExposedPorts: map[string]struct{}{"80/tcp": {}}, Volumes: map[string]struct{}{"/v": {}},
	}
	cfg := ContainerConfig{Env: []string{"B=2"}, Labels: map[string]string{"m": "2"}, ExposedPorts: map[string]struct{}{"81/tcp": {}}}
	mergeCommitConfig(&cfg, from)
	if !slices.Equal(cfg.Env, []string{"B=2", "A=1"}) || cfg.Labels["m"] != "2" || cfg.Labels["l"] != "1" {
		t.Errorf("Env %q, Labels %v", cfg.Env, cfg.Labels)
	}
	if len(cfg.ExposedPorts) != 2 || cfg.User != "u" || cfg.WorkingDir != "/w" || cfg.StopSignal != "SIGTERM" {
		t.Errorf("merged = %+v", cfg)
	}
	if !slices.Equal(cfg.Cmd, []string{"c"}) || !slices.Equal(cfg.Entrypoint, []string{"e"}) {
		t.Errorf("Cmd %q, Entrypoint %q", cfg.Cmd, cfg.Entrypoint)
	}
	own := ContainerConfig{Cmd: []string{"mine"}}
	mergeCommitConfig(&own, from)
	if !slices.Equal(own.Cmd, []string{"mine"}) || own.Entrypoint != nil {
		t.Errorf("a config with a command took the container's: Cmd %q, Entrypoint %q", own.Cmd, own.Entrypoint)
	}
}

func TestCommitTarget(t *testing.T) {
	for _, c := range []struct{ repo, tag, want string }{
		{"", "", ""}, {"app", "", "app:latest"}, {"app", "v1", "app:v1"}, {"app:v2", "", "app:v2"},
		{"localhost:5001/app", "", "localhost:5001/app:latest"}, {"localhost:5001/app:v3", "", "localhost:5001/app:v3"},
	} {
		if got, err := commitTarget(c.repo, c.tag); err != nil || got != c.want {
			t.Errorf("commitTarget(%q, %q) = %q, %v; want %q", c.repo, c.tag, got, err, c.want)
		}
	}
	for _, c := range [][2]string{{"", "v1"}, {"App", ""}, {"app", "a:b"}, {"-x", ""}} {
		if _, err := commitTarget(c[0], c[1]); err == nil {
			t.Errorf("commitTarget(%q, %q) succeeded", c[0], c[1])
		}
	}
}
