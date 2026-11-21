// Package ghrelease reads what GitHub publishes of a repository's
// releases: the newest one, its files, and a download of a file checked
// against the SHA-256 the release gives it. rungv is updated from gvx's
// releases with it, and the app from its own.
package ghrelease

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
)

// Asset is one file of a release.
type Asset struct {
	Name string `json:"name"`
	URL  string `json:"browser_download_url"`
	Size int64  `json:"size"`
}

// Release is a published version.
type Release struct {
	Tag    string  `json:"tag_name"` // v0.17.1…
	Page   string  `json:"html_url"`
	Assets []Asset `json:"assets"`
}

// Find returns the file of r called name.
func (r Release) Find(name string) (Asset, bool) {
	for _, a := range r.Assets {
		if a.Name == name {
			return a, true
		}
	}
	return Asset{}, false
}

// Get asks url for what it has, as the app: anything but 200 is an error.
func Get(ctx context.Context, url string) (*http.Response, error) {
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

// Latest reads the release that url, a repository's releases/latest in
// GitHub's API, answers with.
func Latest(ctx context.Context, url string) (Release, error) {
	resp, err := Get(ctx, url)
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

// Checksum reads the SHA-256 that the checksum file at url gives name, in
// lowercase hex. The file is as sha256sum writes it: a SHA256SUMS of many
// files, or the name.sha256 of one, which may also hold the sum alone.
func Checksum(ctx context.Context, url, name string) (string, error) {
	resp, err := Get(ctx, url)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return "", err
	}
	return parseChecksum(b, name)
}

func parseChecksum(b []byte, name string) (string, error) {
	var lines [][]string
	sc := bufio.NewScanner(bytes.NewReader(b))
	for sc.Scan() {
		if f := strings.Fields(sc.Text()); len(f) > 0 {
			lines = append(lines, f)
		}
	}
	isSum := func(s string) bool {
		_, err := hex.DecodeString(s)
		return len(s) == sha256.Size*2 && err == nil
	}
	for _, f := range lines {
		if len(f) == 2 && strings.TrimPrefix(f[1], "*") == name && isSum(f[0]) {
			return strings.ToLower(f[0]), nil
		}
	}
	if len(lines) == 1 && len(lines[0]) == 1 && isSum(lines[0][0]) {
		return strings.ToLower(lines[0][0]), nil
	}
	return "", fmt.Errorf("the checksum file does not list %s", name)
}

// ErrTooLarge reports a download past the size it may have.
var ErrTooLarge = errors.New("the download is larger than it may be")

// Download writes the file a to w, reading no more than limit bytes of it,
// and checks it against want, its SHA-256 in hex. A download that does not
// match is an error, and w then holds what is not to be used. progress,
// when not nil, hears how much has arrived of how much, when that is known.
func Download(ctx context.Context, a Asset, want string, w io.Writer, limit int64, progress func(done, total int64)) error {
	resp, err := Get(ctx, a.URL)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	total := resp.ContentLength
	if total <= 0 {
		total = a.Size
	}
	if total > limit {
		return fmt.Errorf("%w: %s is %d MB, more than %d MB", ErrTooLarge, a.Name, total>>20, limit>>20)
	}
	sum := sha256.New()
	body := io.TeeReader(io.LimitReader(resp.Body, limit+1), sum)
	if progress != nil {
		body = &counter{r: body, total: total, report: progress}
	}
	n, err := io.Copy(w, body)
	if err != nil {
		return err
	}
	if n > limit {
		return fmt.Errorf("%w: %s is more than %d MB", ErrTooLarge, a.Name, limit>>20)
	}
	if got := hex.EncodeToString(sum.Sum(nil)); got != strings.ToLower(want) {
		return fmt.Errorf("%s does not match its checksum: got %s, the release says %s", a.Name, got, want)
	}
	return nil
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

// Writable reports whether this user may make files in dir.
func Writable(dir string) bool {
	f, err := os.CreateTemp(dir, ".gvx-desktop-*")
	if err != nil {
		return false
	}
	f.Close()
	os.Remove(f.Name())
	return true
}
