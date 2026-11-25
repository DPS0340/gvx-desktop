package rungv

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
)

func TestNewer(t *testing.T) {
	for _, c := range []struct {
		a, b string
		want bool
	}{
		{"v0.17.10002026110900", "v0.16.10022026110900", true},
		{"v0.16.10022026110900", "v0.16.10012026110900", true},
		{"v0.17.1", "v0.17.1", false},
		{"v0.9.10", "v0.17.2", false},
		{"v0.17.2", "v0.9.10", true},
		{"v1.0", "v1.0.0", false},
		{"v1.0.1", "v1.0", true},
		{"v0.17.1", "(devel)", true},
		{"(devel)", "v0.17.1", false},
		{"v0.17.1", "", true},
		{"v0.17.2-rc1", "v0.17.1", true},
	} {
		if got := Newer(c.a, c.b); got != c.want {
			t.Errorf("Newer(%q, %q) = %v", c.a, c.b, got)
		}
	}
}

func TestAssetNames(t *testing.T) {
	if got := AssetNames("v1.2.3", "darwin", "arm64"); !slices.Equal(got, []string{"rungvx-v1.2.3-darwin-arm64.tar.gz", "rungv-v1.2.3-darwin-arm64.tar.gz"}) {
		t.Error(got)
	}
	if got := AssetNames("v1.2.3", "windows", "amd64"); !slices.Equal(got, []string{"rungvx-v1.2.3-windows-amd64.zip", "rungv-v1.2.3-windows-amd64.zip"}) {
		t.Error(got)
	}
}

// archiveOf packs body as the binary of a release archive for this system,
// named as the archive's program (rungvx or rungv) names it.
func archiveOf(t *testing.T, name string, body []byte) []byte {
	t.Helper()
	dir := strings.TrimSuffix(strings.TrimSuffix(name, ".tar.gz"), ".zip")
	prog, _, _ := strings.Cut(name, "-")
	binary := exeName(prog)
	var buf bytes.Buffer
	if strings.HasSuffix(name, ".zip") {
		zw := zip.NewWriter(&buf)
		w, _ := zw.Create(dir + "/LICENSE")
		w.Write([]byte("license"))
		w, _ = zw.Create(dir + "/" + binary)
		w.Write(body)
		zw.Close()
		return buf.Bytes()
	}
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for _, f := range []struct {
		name string
		data []byte
	}{{dir + "/LICENSE", []byte("license")}, {dir + "/" + binary, body}} {
		tw.WriteHeader(&tar.Header{Name: f.name, Mode: 0o755, Size: int64(len(f.data)), Typeflag: tar.TypeReg})
		tw.Write(f.data)
	}
	tw.Close()
	gz.Close()
	return buf.Bytes()
}

// serve stands in for GitHub with one release whose archive holds body and
// whose sums file says sum, or the archive's own when sum is empty. The
// archive is named for prog: rungvx since gvx v0.23, rungv before.
func serve(t *testing.T, prog string, body []byte, sum string) {
	t.Helper()
	const tag = "v9.8.7"
	var name string
	for _, n := range AssetNames(tag, runtime.GOOS, runtime.GOARCH) {
		if strings.HasPrefix(n, prog+"-") {
			name = n
		}
	}
	archive := archiveOf(t, name, body)
	if sum == "" {
		s := sha256.Sum256(archive)
		sum = hex.EncodeToString(s[:])
	}
	mux := http.NewServeMux()
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	mux.HandleFunc("/latest", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(Release{Tag: tag, Page: srv.URL + "/page", Assets: []Asset{
			{Name: name, URL: srv.URL + "/archive", Size: int64(len(archive))},
			{Name: "SHA256SUMS", URL: srv.URL + "/sums"},
		}})
	})
	mux.HandleFunc("/archive", func(w http.ResponseWriter, r *http.Request) { w.Write(archive) })
	mux.HandleFunc("/sums", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("0000  other.tar.gz\n" + sum + "  " + name + "\n"))
	})
	old := latestURL
	latestURL = srv.URL + "/latest"
	t.Cleanup(func() { latestURL = old })
}

func TestInstallPutsTheReleasesBinaryInPlace(t *testing.T) {
	for _, prog := range programs {
		t.Run(prog, func(t *testing.T) { testInstallPutsTheReleasesBinaryInPlace(t, prog) })
	}
}

// testInstallPutsTheReleasesBinaryInPlace installs from a release whose
// archive and binary are named for prog, over a rungv the app installed.
func testInstallPutsTheReleasesBinaryInPlace(t *testing.T, prog string) {
	serve(t, prog, []byte("the new rungv"), "")
	rel, err := Latest(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if rel.Tag != "v9.8.7" {
		t.Fatalf("tag %q", rel.Tag)
	}
	target := filepath.Join(t.TempDir(), "bin", BinaryName())
	os.MkdirAll(filepath.Dir(target), 0o755)
	os.WriteFile(target, []byte("the old rungv"), 0o755)

	var last, total int64
	if err := Install(context.Background(), rel, target, func(d, n int64) { last, total = d, n }); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(target)
	if string(got) != "the new rungv" {
		t.Errorf("installed %q", got)
	}
	if st, _ := os.Stat(target); runtime.GOOS != "windows" && st.Mode()&0o111 == 0 {
		t.Errorf("mode %v is not executable", st.Mode())
	}
	if last == 0 || last != total {
		t.Errorf("progress ended at %d of %d", last, total)
	}
	left, _ := os.ReadDir(filepath.Dir(target))
	for _, e := range left {
		if e.Name() != BinaryName() && e.Name() != BinaryName()+".old" {
			t.Errorf("left %s behind", e.Name())
		}
	}
}

func TestInstallRefusesADownloadThatDoesNotMatchItsChecksum(t *testing.T) {
	serve(t, programX, []byte("tampered"), strings.Repeat("ab", 32))
	rel, err := Latest(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(t.TempDir(), BinaryName())
	os.WriteFile(target, []byte("the old rungv"), 0o755)
	err = Install(context.Background(), rel, target, nil)
	if err == nil || !strings.Contains(err.Error(), "checksum") {
		t.Fatalf("err = %v", err)
	}
	if got, _ := os.ReadFile(target); string(got) != "the old rungv" {
		t.Errorf("the old binary was replaced by %q", got)
	}
}

func TestInstallTarget(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	fallback := filepath.Join(home, ".gvx", "bin", BinaryName())

	if got, replaces, _ := InstallTarget(""); got != fallback || replaces {
		t.Errorf("no rungv: %q, %v", got, replaces)
	}
	dir := t.TempDir()
	plain := filepath.Join(dir, BinaryName())
	os.WriteFile(plain, nil, 0o755)
	if got, replaces, _ := InstallTarget(plain); got != plain || !replaces {
		t.Errorf("a plain file: %q, %v", got, replaces)
	}
	if runtime.GOOS != "windows" {
		link := filepath.Join(dir, "link")
		os.Symlink(plain, link)
		if got, replaces, _ := InstallTarget(link); got != fallback || replaces {
			t.Errorf("a link: %q, %v", got, replaces)
		}
	}
}

func TestInstallRefusesAReleaseWithoutThisSystemsBuild(t *testing.T) {
	rel := Release{Tag: "v9.8.7", Assets: []Asset{
		{Name: "rungvx-v9.8.7-plan9-mips.tar.gz"},
		{Name: "SHA256SUMS"},
	}}
	err := Install(context.Background(), rel, filepath.Join(t.TempDir(), BinaryName()), nil)
	if !errors.Is(err, ErrNoBuild) {
		t.Fatalf("err = %v", err)
	}
}

func TestLocateFindsRungvx(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("PATH", t.TempDir())
	t.Setenv("GVX_RUNGV", "")
	bin := filepath.Join(home, ".gvx", "bin")
	os.MkdirAll(bin, 0o755)

	x := filepath.Join(bin, exeName(programX))
	os.WriteFile(x, nil, 0o755)
	if got, err := Locate(""); err != nil || got != x {
		t.Errorf("only rungvx: %q, %v", got, err)
	}
	// The app's own rungv comes first, so a rungvx put beside it does not
	// change the binary in use.
	own := filepath.Join(bin, BinaryName())
	os.WriteFile(own, nil, 0o755)
	if got, err := Locate(""); err != nil || got != own {
		t.Errorf("both: %q, %v", got, err)
	}
}

func TestSourcePackage(t *testing.T) {
	dir := t.TempDir()
	os.MkdirAll(filepath.Join(dir, "cmd", "rungv"), 0o755)
	if got := sourcePackage(dir); got != "./cmd/rungv" {
		t.Errorf("before v0.23: %q", got)
	}
	os.MkdirAll(filepath.Join(dir, "cmd", "rungvx"), 0o755)
	if got := sourcePackage(dir); got != "./cmd/rungvx" {
		t.Errorf("since v0.23: %q", got)
	}
}
