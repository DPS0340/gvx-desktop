package rungv

import (
	"archive/tar"
	"archive/zip"
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
)

// ReleasesURL is where a person reads about the releases of gvx.
const ReleasesURL = "https://github.com/gfire-sigs/gvx/releases"

// latestURL answers with the newest release of gvx; a test points it at a
// server of its own.
var latestURL = "https://api.github.com/repos/gfire-sigs/gvx/releases/latest"

// maxArchive bounds what Install reads, several times a release's size.
const maxArchive = 512 << 20

// Asset is one file of a release.
type Asset struct {
	Name string `json:"name"`
	URL  string `json:"browser_download_url"`
	Size int64  `json:"size"`
}

// Release is a published version of gvx.
type Release struct {
	Tag    string  `json:"tag_name"` // v0.17.1…
	Page   string  `json:"html_url"`
	Assets []Asset `json:"assets"`
}

// ErrNoBuild reports a release with no rungv for this machine.
var ErrNoBuild = errors.New("the release has no rungv for this system")

func get(ctx context.Context, url string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "gvx-desktop")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		return nil, fmt.Errorf("%s: %s", url, resp.Status)
	}
	return resp, nil
}

// Latest asks GitHub for the newest release of gvx.
func Latest(ctx context.Context) (Release, error) {
	resp, err := get(ctx, latestURL)
	if err != nil {
		return Release{}, err
	}
	defer resp.Body.Close()
	var r Release
	if err := json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(&r); err != nil {
		return Release{}, err
	}
	if r.Tag == "" {
		return Release{}, errors.New("the release has no tag")
	}
	return r, nil
}

// AssetName is the archive of tag that holds rungv for goos and goarch.
func AssetName(tag, goos, goarch string) string {
	ext := ".tar.gz"
	if goos == "windows" {
		ext = ".zip"
	}
	return "rungv-" + tag + "-" + goos + "-" + goarch + ext
}

func (r Release) asset(name string) (Asset, bool) {
	for _, a := range r.Assets {
		if a.Name == name {
			return a, true
		}
	}
	return Asset{}, false
}

// Build is the archive of r for this machine.
func (r Release) Build() (Asset, bool) {
	return r.asset(AssetName(r.Tag, runtime.GOOS, runtime.GOARCH))
}

// Newer reports whether version a is later than b. Versions are compared
// number by number, v0.17.2 after v0.9.10; one that is not numbers, such as
// (devel), is earlier than any that is.
func Newer(a, b string) bool {
	x, okx := versionNumbers(a)
	y, oky := versionNumbers(b)
	if !okx || !oky {
		return okx && !oky
	}
	for i := 0; i < len(x) || i < len(y); i++ {
		var p, q uint64
		if i < len(x) {
			p = x[i]
		}
		if i < len(y) {
			q = y[i]
		}
		if p != q {
			return p > q
		}
	}
	return false
}

func versionNumbers(v string) ([]uint64, bool) {
	v = strings.TrimPrefix(strings.TrimSpace(v), "v")
	if i := strings.IndexAny(v, "-+ "); i >= 0 {
		v = v[:i]
	}
	if v == "" {
		return nil, false
	}
	var out []uint64
	for part := range strings.SplitSeq(v, ".") {
		n, err := strconv.ParseUint(part, 10, 64)
		if err != nil {
			return nil, false
		}
		out = append(out, n)
	}
	return out, true
}

// InstallDir is where Install puts rungv when it has no better place:
// ~/.gvx/bin, one of the places Locate looks.
func InstallDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".gvx", "bin"), nil
}

// InstallTarget is the file an update of the rungv at current should
// replace: current itself when it is a plain file in a directory this user
// may write to, and otherwise, as for a link a package manager keeps or
// when there is no rungv yet, the one in InstallDir. replaces reports the
// first.
func InstallTarget(current string) (target string, replaces bool, err error) {
	if current != "" {
		if st, err := os.Lstat(current); err == nil && st.Mode().IsRegular() && writable(filepath.Dir(current)) {
			return current, true, nil
		}
	}
	dir, err := InstallDir()
	if err != nil {
		return "", false, err
	}
	return filepath.Join(dir, BinaryName()), false, nil
}

func writable(dir string) bool {
	f, err := os.CreateTemp(dir, ".gvx-desktop-*")
	if err != nil {
		return false
	}
	f.Close()
	os.Remove(f.Name())
	return true
}

// Install downloads the rungv of r for this machine, checks it against the
// release's SHA256SUMS, and puts it at target, replacing what is there. A
// rungv that is running keeps running the file it started from. progress,
// when not nil, hears how much of the archive has arrived. On macOS the
// caller still signs the binary, with Sign.
func Install(ctx context.Context, r Release, target string, progress func(done, total int64)) error {
	a, ok := r.Build()
	if !ok {
		return fmt.Errorf("%w (%s/%s)", ErrNoBuild, runtime.GOOS, runtime.GOARCH)
	}
	sums, ok := r.asset("SHA256SUMS")
	if !ok {
		return errors.New("the release has no SHA256SUMS to check the download against")
	}
	want, err := checksum(ctx, sums.URL, a.Name)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		return err
	}
	archive, err := os.CreateTemp(filepath.Dir(target), ".rungv-download-*")
	if err != nil {
		return err
	}
	defer os.Remove(archive.Name())
	defer archive.Close()

	resp, err := get(ctx, a.URL)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	total := resp.ContentLength
	if total <= 0 {
		total = a.Size
	}
	sum := sha256.New()
	body := io.TeeReader(io.LimitReader(resp.Body, maxArchive), sum)
	if progress != nil {
		body = &counter{r: body, total: total, report: progress}
	}
	if _, err := io.Copy(archive, body); err != nil {
		return err
	}
	if got := hex.EncodeToString(sum.Sum(nil)); got != want {
		return fmt.Errorf("%s does not match its checksum: got %s, the release says %s", a.Name, got, want)
	}
	if _, err := archive.Seek(0, io.SeekStart); err != nil {
		return err
	}

	fresh, err := os.CreateTemp(filepath.Dir(target), ".rungv-new-*")
	if err != nil {
		return err
	}
	defer os.Remove(fresh.Name())
	if strings.HasSuffix(a.Name, ".zip") {
		err = unzipBinary(archive, fresh)
	} else {
		err = untarBinary(archive, fresh)
	}
	if cerr := fresh.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return err
	}
	if err := os.Chmod(fresh.Name(), 0o755); err != nil {
		return err
	}
	return replaceFile(fresh.Name(), target)
}

// replaceFile moves fresh onto target. Windows does not let a running
// program's file be written over, but lets it be renamed: the old one steps
// aside first.
func replaceFile(fresh, target string) error {
	if runtime.GOOS == "windows" {
		old := target + ".old"
		os.Remove(old)
		if err := os.Rename(target, old); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	return os.Rename(fresh, target)
}

type counter struct {
	r      io.Reader
	done   int64
	total  int64
	report func(done, total int64)
}

func (c *counter) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.done += int64(n)
	c.report(c.done, c.total)
	return n, err
}

// checksum reads the SHA-256 that the sums file at url gives name.
func checksum(ctx context.Context, url, name string) (string, error) {
	resp, err := get(ctx, url)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return "", err
	}
	sc := bufio.NewScanner(bytes.NewReader(b))
	for sc.Scan() {
		f := strings.Fields(sc.Text())
		if len(f) == 2 && strings.TrimPrefix(f[1], "*") == name && len(f[0]) == sha256.Size*2 {
			return strings.ToLower(f[0]), nil
		}
	}
	return "", fmt.Errorf("SHA256SUMS does not list %s", name)
}

// isBinary reports whether an archive's entry is rungv itself, which lies
// one directory down.
func isBinary(name string) bool { return path.Base(filepath.ToSlash(name)) == BinaryName() }

func untarBinary(archive io.Reader, out io.Writer) error {
	gz, err := gzip.NewReader(archive)
	if err != nil {
		return err
	}
	tr := tar.NewReader(gz)
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return errors.New("the archive holds no rungv")
		}
		if err != nil {
			return err
		}
		if h.Typeflag == tar.TypeReg && isBinary(h.Name) {
			_, err := io.Copy(out, io.LimitReader(tr, maxArchive))
			return err
		}
	}
}

func unzipBinary(archive *os.File, out io.Writer) error {
	st, err := archive.Stat()
	if err != nil {
		return err
	}
	zr, err := zip.NewReader(archive, st.Size())
	if err != nil {
		return err
	}
	for _, f := range zr.File {
		if f.FileInfo().IsDir() || !isBinary(f.Name) {
			continue
		}
		rc, err := f.Open()
		if err != nil {
			return err
		}
		defer rc.Close()
		_, err = io.Copy(out, io.LimitReader(rc, maxArchive))
		return err
	}
	return errors.New("the archive holds no rungv")
}
