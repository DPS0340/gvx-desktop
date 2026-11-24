package imagefs

import (
	"archive/tar"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// testdata/gvxcache is a rungv v0.20.1001 layer cache holding one image,
// gvxd-fixture:1, loaded from a docker save archive of two layers:
//
//	layer 0: etc/{a.txt, hl (a hard link of a.txt), motd, sub/x},
//	         usr/bin/tool, lib/link -> ../etc/a.txt, dev/null (1,3),
//	         dev/fifo, data/big.bin (100000 bytes, uid 1000)
//	layer 1: deletes etc/a.txt and usr/bin, makes etc/sub opaque with a y,
//	         adds opt/new
const (
	fixtureCache = "testdata/gvxcache"
	fixtureID    = "sha256:747db5a176c526f732dd8cfbc736d2667b809ddad2e05a75129b8e1a236e89a1"
)

type tarEntry struct {
	hdr  tar.Header
	body []byte
}

func readTar(t *testing.T, write func(tw *tar.Writer) error) ([]string, map[string]tarEntry) {
	t.Helper()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	if err := write(tw); err != nil {
		t.Fatal(err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	var names []string
	m := map[string]tarEntry{}
	tr := tar.NewReader(&buf)
	for {
		h, err := tr.Next()
		if err == io.EOF {
			return names, m
		}
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(tr)
		names = append(names, h.Name)
		m[h.Name] = tarEntry{*h, b}
	}
}

func TestGVXImageLayers(t *testing.T) {
	g, err := OpenGVXImage(fixtureCache, fixtureID)
	if err != nil {
		t.Fatal(err)
	}
	defer g.Close()
	if g.Layers() != 2 {
		t.Fatalf("Layers() = %d, want 2", g.Layers())
	}
	sum := sha256.Sum256(g.Config)
	if got := "sha256:" + hex.EncodeToString(sum[:]); got != fixtureID {
		t.Errorf("the config sums to %s, want the image ID", got)
	}

	names, l0 := readTar(t, func(tw *tar.Writer) error { return g.WriteLayer(0, tw, false) })
	sorted := slices.Sorted(slices.Values(names))
	want := []string{
		"data/", "data/big.bin", "dev/", "dev/fifo", "dev/null", "etc/", "etc/a.txt", "etc/hl",
		"etc/motd", "etc/sub/", "etc/sub/x", "lib/", "lib/link", "usr/", "usr/bin/", "usr/bin/tool",
	}
	if !slices.Equal(sorted, want) {
		t.Fatalf("layer 0:\n got %q\nwant %q", sorted, want)
	}
	// Each directory comes before what it holds.
	seen := map[string]bool{}
	for _, n := range names {
		if d := parentDir(n); d != "" && !seen[d] {
			t.Errorf("%s comes before its directory %s", n, d)
		}
		seen[n] = true
	}
	bigSum := sha256.Sum256(l0["data/big.bin"].body)
	checks := map[string]func(e tarEntry) bool{
		"etc/a.txt":    func(e tarEntry) bool { return string(e.body) == "alpha\n" && e.hdr.Mode == 0o644 },
		"etc/hl":       func(e tarEntry) bool { return e.hdr.Typeflag == tar.TypeLink && e.hdr.Linkname == "etc/a.txt" },
		"usr/bin/tool": func(e tarEntry) bool { return e.hdr.Mode == 0o755 && string(e.body) == "#!/bin/sh\necho tool\n" },
		"lib/link":     func(e tarEntry) bool { return e.hdr.Typeflag == tar.TypeSymlink && e.hdr.Linkname == "../etc/a.txt" },
		"dev/null": func(e tarEntry) bool {
			return e.hdr.Typeflag == tar.TypeChar && e.hdr.Devmajor == 1 && e.hdr.Devminor == 3 && e.hdr.Mode == 0o666
		},
		"dev/fifo": func(e tarEntry) bool { return e.hdr.Typeflag == tar.TypeFifo },
		"data/": func(e tarEntry) bool {
			return e.hdr.Typeflag == tar.TypeDir && e.hdr.Uid == 1000 && e.hdr.Gid == 1000
		},
		// 100000 bytes of Python's random.Random(42), across 25 blocks.
		"data/big.bin": func(e tarEntry) bool {
			return e.hdr.Uid == 1000 && hex.EncodeToString(bigSum[:]) == "0517269eedaf6f50e7a2f9a1c23f1a54e2e0ab94f6175145f7129af241feb22e"
		},
	}
	for name, ok := range checks {
		if e, found := l0[name]; !found || !ok(e) {
			t.Errorf("layer 0 %s: %+v %q", name, e.hdr, e.body)
		}
	}

	names1, l1 := readTar(t, func(tw *tar.Writer) error { return g.WriteLayer(1, tw, false) })
	sorted1 := slices.Sorted(slices.Values(names1))
	want1 := []string{"etc/", "etc/.wh.a.txt", "etc/sub/", "etc/sub/.wh..wh..opq", "etc/sub/y", "opt/", "opt/new", "usr/", "usr/.wh.bin"}
	if !slices.Equal(sorted1, want1) {
		t.Fatalf("layer 1:\n got %q\nwant %q", sorted1, want1)
	}
	if string(l1["opt/new"].body) != "new\n" {
		t.Errorf("opt/new = %q", l1["opt/new"].body)
	}

	// Headers only: the same tree, no content.
	_, h0 := readTar(t, func(tw *tar.Writer) error { return g.WriteLayer(0, tw, true) })
	if e := h0["data/big.bin"]; e.hdr.Size != 0 || len(e.body) != 0 {
		t.Errorf("headers only wrote %d bytes of data/big.bin", len(e.body))
	}
	if len(h0) != len(l0) {
		t.Errorf("headers only wrote %d entries, want %d", len(h0), len(l0))
	}

	if err := g.WriteLayer(2, tar.NewWriter(io.Discard), false); err == nil {
		t.Error("WriteLayer(2) of a 2-layer image succeeded")
	}
}

func parentDir(name string) string {
	n := name
	if len(n) > 0 && n[len(n)-1] == '/' {
		n = n[:len(n)-1]
	}
	for i := len(n) - 1; i >= 0; i-- {
		if n[i] == '/' {
			return n[:i+1]
		}
	}
	return ""
}

func TestOpenGVXImageMissing(t *testing.T) {
	if _, err := OpenGVXImage(fixtureCache, "sha256:"+string(bytes.Repeat([]byte("0"), 64))); err == nil {
		t.Error("an image the cache lacks opened")
	}
	if _, err := OpenGVXImage("", fixtureID); err == nil {
		t.Error("an empty cache directory opened")
	}
}

// TestReadRange reads a file whole and in pieces across block boundaries.
func TestReadRange(t *testing.T) {
	g, err := OpenGVXImage(fixtureCache, fixtureID)
	if err != nil {
		t.Fatal(err)
	}
	defer g.Close()
	img := g.layers[0]
	ino, ok, err := img.lookup("/data/big.bin")
	if err != nil || !ok {
		t.Fatalf("lookup: %v %v", ok, err)
	}
	whole, err := img.readData(ino, 0)
	if err != nil || len(whole) != 100000 {
		t.Fatalf("readData: %d bytes, %v", len(whole), err)
	}
	for _, r := range [][2]uint64{{0, 1}, {4095, 2}, {4096, 4096}, {99999, 1}, {12345, 54321}, {98304, 1696}} {
		got, err := img.readRange(ino, r[0], r[1])
		if err != nil || !bytes.Equal(got, whole[r[0]:r[0]+r[1]]) {
			t.Errorf("readRange(%d, %d): %v", r[0], r[1], err)
		}
	}
	if _, err := img.readRange(ino, 99999, 2); err == nil {
		t.Error("a read past the end succeeded")
	}
}

func TestGVXImageExists(t *testing.T) {
	g, err := OpenGVXImage(fixtureCache, fixtureID)
	if err != nil {
		t.Fatal(err)
	}
	defer g.Close()
	for name, want := range map[string]bool{
		"etc": true, "etc/hl": true, "etc/motd": true, "opt/new": true, "data/big.bin": true,
		"etc/a.txt": false,                                         // deleted
		"usr":       true, "usr/bin": false, "usr/bin/tool": false, // deleted with what it held
		"etc/sub": true, "etc/sub/y": true, "etc/sub/x": false, // hidden by an opaque directory
		"nope": false, "etc/motd/under": false,
	} {
		got, err := g.Exists(name)
		if err != nil || got != want {
			t.Errorf("Exists(%s) = %v, %v; want %v", name, got, err, want)
		}
	}
}

func TestReadGVXImageHistoryAndWriteEntry(t *testing.T) {
	hist, err := ReadGVXImageHistory(fixtureCache, fixtureID)
	if err != nil {
		t.Fatalf("ReadGVXImageHistory: %v", err)
	}
	if len(hist) != 2 {
		t.Fatalf("len(hist) = %d, want 2 (%+v)", len(hist), hist)
	}
	for i, h := range hist {
		if h.Size <= 0 {
			t.Errorf("hist[%d].Size = %d, want > 0", i, h.Size)
		}
	}

	g, err := OpenGVXImage(fixtureCache, fixtureID)
	if err != nil {
		t.Fatal(err)
	}
	defer g.Close()

	sz, err := g.StatLayer(0, "data/big.bin")
	if err != nil || sz != 100000 {
		t.Fatalf("StatLayer(data/big.bin) = %d, %v; want 100000", sz, err)
	}
	sz, err = g.StatLayer(0, "etc/hl")
	if err != nil || sz != 6 {
		t.Fatalf("StatLayer(etc/hl) = %d, %v; want 6", sz, err)
	}

	// WriteEntry writes hard-linked file as regular file with full content.
	names, ents := readTar(t, func(tw *tar.Writer) error {
		return g.WriteEntry(0, "etc/hl", "hl", tw)
	})
	if len(names) != 1 || names[0] != "hl" {
		t.Fatalf("WriteEntry(etc/hl) names = %v", names)
	}
	if ents["hl"].hdr.Typeflag != tar.TypeReg || string(ents["hl"].body) != "alpha\n" {
		t.Fatalf("WriteEntry(etc/hl) = %+v %q", ents["hl"].hdr, ents["hl"].body)
	}

	// WriteEntry writes symlink with target.
	names, ents = readTar(t, func(tw *tar.Writer) error {
		return g.WriteEntry(0, "lib/link", "link", tw)
	})
	if len(names) != 1 || ents["link"].hdr.Typeflag != tar.TypeSymlink || ents["link"].hdr.Linkname != "../etc/a.txt" {
		t.Fatalf("WriteEntry(lib/link) = %v %+v", names, ents["link"].hdr)
	}
}

func TestResolveGVXLayers(t *testing.T) {
	absCache, err := filepath.Abs(fixtureCache)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		filepath.Join(absCache, "36427c2cf5952cb8e051af94d2c8a104a12565d7f20b79287a2d8a2dc25b6d84.gvx-erofs-v3-b4096.erofs"),
		filepath.Join(absCache, "688af45ba4817240ad535eda8600e73bd05274b29416d43aaf63793928b043aa.gvx-erofs-v3-b4096.erofs"),
	}

	got, err := ResolveGVXLayers(fixtureCache, fixtureID)
	if err != nil {
		t.Fatalf("ResolveGVXLayers(fixture): %v", err)
	}
	if !slices.Equal(got, want) {
		t.Fatalf("ResolveGVXLayers(fixture) =\n got %v\nwant %v", got, want)
	}
	for _, p := range got {
		if !filepath.IsAbs(p) {
			t.Errorf("layer path %q is not absolute", p)
		}
	}

	// Prefix ID without sha256: prefix also resolves.
	short := strings.TrimPrefix(fixtureID, "sha256:")[:12]
	gotShort, err := ResolveGVXLayers(fixtureCache, short)
	if err != nil || !slices.Equal(gotShort, want) {
		t.Fatalf("ResolveGVXLayers(short) = %v, %v; want %v", gotShort, err, want)
	}

	// Synthetic amd64 cached image in a temp cache directory.
	tmp := t.TempDir()
	amdCfg := []byte(`{"architecture":"amd64","os":"linux","config":{"Cmd":["/bin/sh"]}}`)
	cfgSum := sha256.Sum256(amdCfg)
	amdID := "sha256:" + hex.EncodeToString(cfgSum[:])
	l0Hex := strings.Repeat("a", 64)
	l1Hex := strings.Repeat("b", 64)
	l0Path := filepath.Join(tmp, l0Hex+".gvx-erofs-v3-b4096.erofs")
	l1Path := filepath.Join(tmp, l1Hex+".gvx-erofs-v3-b4096.erofs")
	if err := os.WriteFile(l0Path, []byte("erofs-l0"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(l1Path, []byte("erofs-l1"), 0o600); err != nil {
		t.Fatal(err)
	}
	manifest, _ := json.Marshal(map[string]any{
		"schemaVersion": 2,
		"layers": []map[string]any{
			{"digest": "sha256:" + l0Hex, "size": 8},
			{"digest": "sha256:" + l1Hex, "size": 8},
		},
	})
	rec, _ := json.Marshal(gvxImageRecord{
		Version:      2,
		Repository:   "library/alpine",
		Digest:       "sha256:" + strings.Repeat("c", 64),
		Architecture: "amd64",
		Format:       "gvx-erofs-v3-b4096",
		Manifests:    [][]byte{manifest},
		Config:       amdCfg,
	})
	if err := os.WriteFile(filepath.Join(tmp, "image-amd64.json"), rec, 0o600); err != nil {
		t.Fatal(err)
	}
	gotAmd, err := ResolveGVXLayers(tmp, amdID)
	if err != nil {
		t.Fatalf("ResolveGVXLayers(amd64): %v", err)
	}
	wantAmd := []string{l0Path, l1Path}
	if !slices.Equal(gotAmd, wantAmd) {
		t.Fatalf("ResolveGVXLayers(amd64) = %v, want %v", gotAmd, wantAmd)
	}

	// Missing .erofs file returns error.
	if err := os.Remove(l1Path); err != nil {
		t.Fatal(err)
	}
	if _, err := ResolveGVXLayers(tmp, amdID); err == nil {
		t.Error("ResolveGVXLayers succeeded with missing .erofs file")
	}

	// Error cases.
	if _, err := ResolveGVXLayers("", fixtureID); err == nil {
		t.Error("ResolveGVXLayers with empty cacheDir succeeded")
	}
	if _, err := ResolveGVXLayers(fixtureCache, ""); err == nil {
		t.Error("ResolveGVXLayers with empty imageID succeeded")
	}
	if _, err := ResolveGVXLayers(fixtureCache, "sha256:"+strings.Repeat("0", 64)); err == nil {
		t.Error("ResolveGVXLayers with unknown imageID succeeded")
	}
}
