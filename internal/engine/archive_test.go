package engine

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/ironpark/gvx-desktop/internal/rungv"
)

func TestArchiveHeadGetPutAndErrors(t *testing.T) {
	te, ctl := newControlTestEngine(t)
	cid := createAndStartContainer(t, te, "gvxd-cp", nil)

	now := time.Date(2026, 11, 22, 12, 0, 0, 0, time.UTC)
	ctl.statFn = func(ctx context.Context, guestPath string) (rungv.PathStat, error) {
		switch guestPath {
		case "/etc/hostname":
			return rungv.PathStat{Name: "hostname", Size: 8, Mode: 0o644, Mtime: now}, nil
		case "/tmp":
			return rungv.PathStat{Name: "tmp", Mode: uint32(os.ModeDir | 0o755), Mtime: now}, nil
		case "/ro":
			return rungv.PathStat{Name: "ro", Mode: uint32(os.ModeDir | 0o755), Mtime: now}, nil
		case "/badlink":
			return rungv.PathStat{Name: "badlink", Mode: uint32(os.ModeSymlink | 0o777), LinkTarget: "/etc/hostname", Mtime: now}, nil
		case "/exists":
			return rungv.PathStat{Name: "exists", Mode: uint32(os.ModeDir | 0o755), Mtime: now}, nil
		default:
			return rungv.PathStat{}, &rungv.Error{Code: 1, Stderr: "rungv cp: NOT_FOUND: guest path does not exist"}
		}
	}
	ctl.copyFromFn = func(ctx context.Context, guestPath string, o rungv.CopyOptions) (io.ReadCloser, error) {
		if guestPath != "/etc/hostname" {
			return nil, &rungv.Error{Code: 1, Stderr: "rungv cp: NOT_FOUND: missing"}
		}
		return io.NopCloser(strings.NewReader("FAKE-TAR-BYTES")), nil
	}
	ctl.copyToFn = func(ctx context.Context, guestDir string, r io.Reader, o rungv.CopyOptions) error {
		switch guestDir {
		case "/ro":
			return &rungv.Error{Code: 1, Stderr: "rungv cp: INTERNAL: open /ro/a: read-only file system"}
		case "/badlink":
			return &rungv.Error{Code: 1, Stderr: "rungv cp: INVALID_ARGUMENT: destination is not a directory"}
		case "/exists":
			return &rungv.Error{Code: 1, Stderr: "rungv cp: ALREADY_EXISTS: cannot overwrite non-directory with directory"}
		}
		return nil
	}

	// HEAD with relative path (resolved relative to /).
	req, _ := http.NewRequest(http.MethodHead, te.srv.URL+"/v1.52/containers/"+cid+"/archive?path=etc/hostname", nil)
	res, err := te.srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("HEAD = %d", res.StatusCode)
	}
	rawHdr := res.Header.Get(archivePathStatHeader)
	dec, err := base64.StdEncoding.DecodeString(rawHdr)
	if err != nil {
		t.Fatalf("decode stat header %q: %v", rawHdr, err)
	}
	var ps archivePathStat
	if err := json.Unmarshal(dec, &ps); err != nil {
		t.Fatal(err)
	}
	if ps.Name != "hostname" || ps.Size != 8 || ps.Mode != 0o644 {
		t.Fatalf("stat = %+v", ps)
	}

	// GET streams tar with header + application/x-tar.
	req, _ = http.NewRequest(http.MethodGet, te.srv.URL+"/v1.52/containers/"+cid+"/archive?path=/etc/hostname", nil)
	res, err = te.srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	gotTar, _ := io.ReadAll(res.Body)
	res.Body.Close()
	if res.StatusCode != http.StatusOK || res.Header.Get("Content-Type") != "application/x-tar" || string(gotTar) != "FAKE-TAR-BYTES" {
		t.Fatalf("GET = %d ct=%q body=%q", res.StatusCode, res.Header.Get("Content-Type"), gotTar)
	}

	// NOT_FOUND -> 404 naming container.
	if st, b := te.do(t, http.MethodGet, "/containers/"+cid+"/archive?path=/missing", nil); st != http.StatusNotFound || !strings.Contains(string(b), "gvxd-cp") {
		t.Fatalf("missing = %d %s", st, b)
	}

	// PUT to non-directory -> 400.
	if st, b := te.do(t, http.MethodPut, "/containers/"+cid+"/archive?path=/etc/hostname", []byte("tar")); st != http.StatusBadRequest || !strings.Contains(string(b), "not a directory") {
		t.Fatalf("put non-dir = %d %s", st, b)
	}

	// PUT plain tar with copyUIDGID & noOverwriteDirNonDir.
	if st, b := te.do(t, http.MethodPut, "/containers/"+cid+"/archive?path=/tmp&copyUIDGID=1&noOverwriteDirNonDir=1", []byte("RAW-TAR")); st != http.StatusOK {
		t.Fatalf("put raw = %d %s", st, b)
	}
	ctl.mu.Lock()
	last := ctl.copyToCalls[len(ctl.copyToCalls)-1]
	ctl.mu.Unlock()
	if last.Dir != "/tmp" || string(last.Data) != "RAW-TAR" || !last.Opts.Archive || !last.Opts.NoOverwriteDir {
		t.Fatalf("copyTo call = %+v", last)
	}

	// PUT with trailing slash (as nektos/act sends for /var/run/act/) cleans the target path.
	if st, b := te.do(t, http.MethodPut, "/containers/"+cid+"/archive?path=/tmp/", []byte("ACT-TAR")); st != http.StatusOK {
		t.Fatalf("put trailing slash = %d %s", st, b)
	}
	ctl.mu.Lock()
	last = ctl.copyToCalls[len(ctl.copyToCalls)-1]
	ctl.mu.Unlock()
	if last.Dir != "/tmp" || string(last.Data) != "ACT-TAR" {
		t.Fatalf("copyTo trailing slash = %+v", last)
	}

	// PUT gzip-compressed tar -> transparently decompressed before CopyTo.
	var gzBuf bytes.Buffer
	gw := gzip.NewWriter(&gzBuf)
	_, _ = gw.Write([]byte("DECOMPRESSED-TAR"))
	_ = gw.Close()
	if st, b := te.do(t, http.MethodPut, "/containers/"+cid+"/archive?path=/tmp", gzBuf.Bytes()); st != http.StatusOK {
		t.Fatalf("put gzip = %d %s", st, b)
	}
	ctl.mu.Lock()
	last = ctl.copyToCalls[len(ctl.copyToCalls)-1]
	ctl.mu.Unlock()
	if string(last.Data) != "DECOMPRESSED-TAR" {
		t.Fatalf("gzip body not decompressed: %q", last.Data)
	}

	// PUT valid bzip2-compressed tar -> transparently decompressed before CopyTo.
	bz2Valid := []byte("BZh91AY&SYe\x96Lu\x00\x00\x01\x9e\x00\x00\x02\x10\x000 T\x10 \x001\x06LA\x00\xd3\xd2R\x06\xc1\xf8\xbb\x92)\xc2\x84\x83,\xb2c\xa8")
	if st, b := te.do(t, http.MethodPut, "/containers/"+cid+"/archive?path=/tmp", bz2Valid); st != http.StatusOK {
		t.Fatalf("put valid bzip2 = %d %s", st, b)
	}
	ctl.mu.Lock()
	last = ctl.copyToCalls[len(ctl.copyToCalls)-1]
	ctl.mu.Unlock()
	if string(last.Data) != "BZIP2-TAR" {
		t.Fatalf("bzip2 body not decompressed: %q", last.Data)
	}

	// PUT corrupt bzip2 / unsupported xz -> 400.
	if st, _ := te.do(t, http.MethodPut, "/containers/"+cid+"/archive?path=/tmp", []byte("BZh91AY&SY")); st != http.StatusBadRequest {
		t.Fatalf("put corrupt bzip2 = %d, want 400", st)
	}
	if st, _ := te.do(t, http.MethodPut, "/containers/"+cid+"/archive?path=/tmp", []byte{0xfd, '7', 'z', 'X', 'Z', 0x00, 1, 2}); st != http.StatusBadRequest {
		t.Fatalf("put xz = %d, want 400", st)
	}

	// PUT read-only -> 403.
	if st, b := te.do(t, http.MethodPut, "/containers/"+cid+"/archive?path=/ro", []byte("RAW")); st != http.StatusForbidden || !strings.Contains(string(b), "read-only") {
		t.Fatalf("put ro = %d %s", st, b)
	}

	// PUT symlink-to-file (INVALID_ARGUMENT) and ALREADY_EXISTS -> 400 (not 500).
	if st, b := te.do(t, http.MethodPut, "/containers/"+cid+"/archive?path=/badlink", []byte("RAW")); st != http.StatusBadRequest {
		t.Fatalf("put badlink = %d %s, want 400", st, b)
	}
	if st, b := te.do(t, http.MethodPut, "/containers/"+cid+"/archive?path=/exists&noOverwriteDirNonDir=1", []byte("RAW")); st != http.StatusBadRequest {
		t.Fatalf("put already_exists = %d %s, want 400", st, b)
	}

	// Paused -> cp succeeds (matching Docker and rungv v0.20).
	_, _ = te.do(t, http.MethodPost, "/containers/"+cid+"/pause", nil)
	if st, b := te.do(t, http.MethodGet, "/containers/"+cid+"/archive?path=/etc/hostname", nil); st != http.StatusOK || string(b) != "FAKE-TAR-BYTES" {
		t.Fatalf("archive paused = %d %q, want 200 FAKE-TAR-BYTES", st, b)
	}
	_, _ = te.do(t, http.MethodPost, "/containers/"+cid+"/unpause", nil)

	// Stopped container: PUT -> 501 (ext4lite is read-only).
	_, _ = te.do(t, http.MethodPost, "/containers/"+cid+"/stop?t=0", nil)
	if st, b := te.do(t, http.MethodPut, "/containers/"+cid+"/archive?path=/tmp", []byte("RAW")); st != http.StatusNotImplemented || !strings.Contains(string(b), "not running") {
		t.Fatalf("archive put stopped = %d %s", st, b)
	}
}
