package ext4lite

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// The fixtures are volumes rungv v0.20.1001 wrote (-rootfs-volume):
//
//   - layer.ext4 holds the upper layer of an alpine:3.20 run that added,
//     changed, deleted and replaced files (see TestWriteLayer).
//   - crash.ext4 was killed (SIGKILL) while a loop wrote and removed files
//     under /data, so its journal needs a replay. crashDataSum is the
//     SHA-256 of what the guest saw after rungv replayed a copy: the sorted
//     lines "<path> <content>" of every file under /data.
const crashDataSum = "67979cb7074719db34d96e898dd0e443eb78df21a92fd0acddb8ac63f17924e8"

// fixture ungzips testdata/name into a temporary file.
func fixture(t testing.TB, name string) string {
	t.Helper()
	f, err := os.Open(filepath.Join("testdata", name+".gz"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	zr, err := gzip.NewReader(f)
	if err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(t.TempDir(), name)
	w, err := os.Create(out)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.Copy(w, zr); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return out
}

func open(t testing.TB, path string) *Volume {
	t.Helper()
	v, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { v.Close() })
	return v
}

type layerEntry struct {
	hdr  *tar.Header
	body []byte
}

func readLayer(t testing.TB, v *Volume, o LayerOptions) map[string]layerEntry {
	t.Helper()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	if err := v.WriteLayer(tw, UpperDir, o); err != nil {
		t.Fatal(err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	out := map[string]layerEntry{}
	tr := tar.NewReader(&buf)
	for {
		h, err := tr.Next()
		if err == io.EOF {
			return out
		}
		if err != nil {
			t.Fatal(err)
		}
		if _, dup := out[h.Name]; dup {
			t.Fatalf("%s is in the layer twice", h.Name)
		}
		b, _ := io.ReadAll(tr)
		out[h.Name] = layerEntry{h, b}
	}
}

func TestWriteLayer(t *testing.T) {
	v := open(t, fixture(t, "layer.ext4"))
	l := readLayer(t, v, LayerOptions{})

	var names []string
	many := 0
	for n := range l {
		if strings.HasPrefix(n, "root/many/entry-") {
			many++
			continue
		}
		names = append(names, n)
	}
	slices.Sort(names)
	want := []string{
		"etc/", "etc/.wh.issue", "etc/apk/", "etc/apk/.wh..wh..opq", "etc/apk/world",
		"etc/hostname", "etc/hosts", "etc/motd", "etc/resolv.conf",
		"root/", "root/empty", "root/fifo", "root/hard", "root/link", "root/longlink",
		"root/many/", "root/many/.wh..wh..opq", "root/mid", "root/new.txt", "root/seq.txt", "root/small",
		"usr/", "usr/share/", "usr/share/.wh.apk",
		"workspace/", "workspace/.wh..wh..opq",
	}
	if !slices.Equal(names, want) {
		t.Errorf("layer entries:\n got %q\nwant %q", names, want)
	}
	if many != 300 {
		t.Errorf("root/many holds %d entries, want 300 (a directory of several blocks)", many)
	}

	var seq strings.Builder
	for i := 1; i <= 40000; i++ {
		fmt.Fprintf(&seq, "%d\n", i)
	}
	checks := []struct {
		name string
		ok   func(e layerEntry) bool
	}{
		{"root/seq.txt", func(e layerEntry) bool { return string(e.body) == seq.String() }},                                     // 56 blocks of extents
		{"root/mid", func(e layerEntry) bool { return len(e.body) == 150 && strings.HasPrefix(string(e.body), "root:x:0:0:") }}, // inline, past i_block
		{"root/new.txt", func(e layerEntry) bool { return string(e.body) == "hi\n" && e.hdr.Uid == 1234 && e.hdr.Gid == 5678 }},
		{"root/hard", func(e layerEntry) bool { return string(e.body) == "abc" && e.hdr.Mode == 0o4755 }},
		{"root/small", func(e layerEntry) bool { return e.hdr.Typeflag == tar.TypeLink && e.hdr.Linkname == "root/hard" }},
		{"root/link", func(e layerEntry) bool { return e.hdr.Typeflag == tar.TypeSymlink && e.hdr.Linkname == "/root/new.txt" }},
		{"root/longlink", func(e layerEntry) bool {
			return e.hdr.Typeflag == tar.TypeSymlink && e.hdr.Linkname == "/"+strings.Repeat("a", 80)+"/target"
		}},
		{"root/fifo", func(e layerEntry) bool { return e.hdr.Typeflag == tar.TypeFifo }},
		{"root/empty", func(e layerEntry) bool { return e.hdr.Typeflag == tar.TypeReg && e.hdr.Size == 0 }},
		{"root/", func(e layerEntry) bool { return e.hdr.Typeflag == tar.TypeDir && e.hdr.Mode == 0o700 }},
		{"etc/.wh.issue", func(e layerEntry) bool { return e.hdr.Typeflag == tar.TypeReg && e.hdr.Size == 0 }},
		{"etc/apk/world", func(e layerEntry) bool { return string(e.body) == "n\n" }},
	}
	for _, c := range checks {
		e, ok := l[c.name]
		if !ok {
			t.Errorf("%s is missing", c.name)
			continue
		}
		if !c.ok(e) {
			t.Errorf("%s: %+v %q", c.name, *e.hdr, e.body)
		}
	}
}

func TestWriteLayerSkip(t *testing.T) {
	v := open(t, fixture(t, "layer.ext4"))
	l := readLayer(t, v, LayerOptions{Skip: func(name string) bool {
		return name == "etc/hosts" || name == "root/many"
	}})
	for n := range l {
		if n == "etc/hosts" || strings.HasPrefix(n, "root/many/") {
			t.Errorf("%s is in the layer", n)
		}
	}
	if _, ok := l["etc/motd"]; !ok {
		t.Error("etc/motd is missing")
	}
}

func TestLookup(t *testing.T) {
	v := open(t, fixture(t, "layer.ext4"))
	n, err := v.Lookup("/upper/root/many")
	if err != nil || !n.IsDir() {
		t.Fatalf("Lookup = %v, %v", n, err)
	}
	if _, err := v.Lookup("/upper/root/nope"); !errors.Is(err, ErrNotExist) {
		t.Errorf("Lookup of a missing path = %v, want ErrNotExist", err)
	}
	w, err := v.Lookup("/upper/etc/issue")
	if err != nil || !w.IsWhiteout() {
		t.Errorf("etc/issue = %+v, %v; want a whiteout", w, err)
	}
	if _, err := v.Lookup("/work"); err != nil {
		t.Errorf("the overlay work directory: %v", err)
	}
}

// TestJournalReplay reads a volume a crash left with committed transactions
// in its log, and must see what rungv's own replay showed the guest.
func TestJournalReplay(t *testing.T) {
	path := fixture(t, "crash.ext4")
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	v := open(t, path)
	if len(v.log) == 0 {
		t.Fatal("the fixture's log should need a replay")
	}
	l := readLayer(t, v, LayerOptions{})
	var lines []string
	for n, e := range l {
		if strings.HasPrefix(n, "data/") && e.hdr.Typeflag == tar.TypeReg && !strings.HasSuffix(n, ".wh..wh..opq") {
			lines = append(lines, "/"+n+" "+strings.TrimSpace(string(e.body)))
		}
	}
	slices.Sort(lines)
	sum := sha256.Sum256([]byte(strings.Join(lines, "\n") + "\n"))
	if got := hex.EncodeToString(sum[:]); got != crashDataSum {
		t.Errorf("after the replay, /data sums to %s (%d files), want %s", got, len(lines), crashDataSum)
	}
	v.Close()
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Error("reading the volume changed the file")
	}
}

func TestOpenRejectsOtherFiles(t *testing.T) {
	dir := t.TempDir()
	for name, data := range map[string][]byte{
		"empty":  nil,
		"short":  []byte("not a volume"),
		"zeros":  make([]byte, 64<<10),
		"random": func() []byte { b := make([]byte, 64<<10); _, _ = rand.NewChaCha8([32]byte{1}).Read(b); return b }(),
	} {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, data, 0o600); err != nil {
			t.Fatal(err)
		}
		if v, err := Open(p); err == nil {
			v.Close()
			t.Errorf("Open(%s) succeeded", name)
		}
	}
}

// TestDamageDoesNotPanic flips bytes in a volume's metadata and walks it:
// damage must come back as errors.
func TestDamageDoesNotPanic(t *testing.T) {
	src, err := os.ReadFile(fixture(t, "layer.ext4"))
	if err != nil {
		t.Fatal(err)
	}
	rng := rand.New(rand.NewPCG(7, 11))
	p := filepath.Join(t.TempDir(), "damaged.ext4")
	for i := range 200 {
		b := bytes.Clone(src)
		for range 1 + rng.IntN(64) {
			// Group 0's superblock, descriptors, bitmaps, inode table and
			// the first data blocks.
			b[rng.IntN(1<<20)] ^= byte(1 + rng.IntN(255))
		}
		if err := os.WriteFile(p, b, 0o600); err != nil {
			t.Fatal(err)
		}
		func() {
			defer func() {
				if r := recover(); r != nil {
					t.Fatalf("iteration %d panicked: %v", i, r)
				}
			}()
			v, err := Open(p)
			if err != nil {
				return
			}
			defer v.Close()
			walk(v, rootIno, 0)
		}()
	}
}

// walk reads every inode under ino as WriteLayer would, but at most 1 MiB of
// each file: a damaged size can claim terabytes of holes.
func walk(v *Volume, ino uint32, depth int) {
	n, err := v.Stat(ino)
	if err != nil || depth > 64 {
		return
	}
	_, _ = v.Xattrs(n)
	switch n.Mode & ModeType {
	case ModeDir:
		ents, err := v.ReadDir(n)
		if err != nil {
			return
		}
		for _, e := range ents {
			walk(v, e.Ino, depth+1)
		}
	case ModeSymlink:
		_, _ = v.Readlink(n)
	case ModeRegular:
		if r, err := v.Open(n); err == nil {
			_, _ = io.Copy(io.Discard, io.LimitReader(r, 1<<20))
		}
	}
}
