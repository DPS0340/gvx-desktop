package engine

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/ironpark/gvx-desktop/internal/rungv"
	"github.com/ironpark/gvx-desktop/internal/sandbox"
)

func makeTestTar(t *testing.T, entries []struct {
	hdr  tar.Header
	body string
}) []byte {
	t.Helper()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for _, e := range entries {
		h := e.hdr
		if h.Typeflag == 0 {
			h.Typeflag = tar.TypeReg
		}
		if h.Typeflag == tar.TypeReg && h.Size == 0 {
			h.Size = int64(len(e.body))
		}
		if h.Mode == 0 {
			h.Mode = 0o644
		}
		if err := tw.WriteHeader(&h); err != nil {
			t.Fatal(err)
		}
		if h.Typeflag == tar.TypeReg && len(e.body) > 0 {
			if _, err := tw.Write([]byte(e.body)); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func gzipBytes(t *testing.T, data []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	if _, err := gz.Write(data); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func TestParseBuildRequestAndBuildctlArgs(t *testing.T) {
	q := url.Values{}
	q.Set("dockerfile", "docker/Dockerfile.prod")
	q.Add("t", "myapp:1")
	q.Add("t", "myapp:latest")
	q.Set("target", "runtime")
	q.Set("nocache", "1")
	q.Set("pull", "true")
	q.Set("platform", "linux/arm64")
	q.Set("buildargs", `{"B":"2","A":"1","EMPTY":null}`)
	q.Set("labels", `{"org.example.v":"1"}`)

	req := httptest.NewRequest(http.MethodPost, "/v1.52/build?"+q.Encode(), nil)
	opts, err := parseBuildRequest(req)
	if err != nil {
		t.Fatalf("parseBuildRequest: %v", err)
	}
	if opts.Dockerfile != "docker/Dockerfile.prod" || opts.Target != "runtime" || !opts.NoCache || !opts.Pull || opts.Platform != "linux/arm64" {
		t.Fatalf("unexpected opts: %+v", opts)
	}
	if !slices.Equal(opts.Tags, []string{"myapp:1", "myapp:latest"}) {
		t.Fatalf("tags = %v", opts.Tags)
	}
	if opts.BuildArgs["A"] != "1" || opts.BuildArgs["B"] != "2" {
		t.Fatalf("buildargs = %v", opts.BuildArgs)
	}
	if _, hasNull := opts.BuildArgs["EMPTY"]; hasNull {
		t.Fatalf("null buildarg should be omitted: %v", opts.BuildArgs)
	}

	args := buildctlArgs("tcp://10.0.2.2:12345", opts)
	wantSub := []string{
		"--addr", "tcp://10.0.2.2:12345",
		"build",
		"--progress=plain",
		"--frontend", "dockerfile.v0",
		"--local", "context=/ctx",
		"--local", "dockerfile=/ctx",
		"--opt", "filename=docker/Dockerfile.prod",
		"--opt", "target=runtime",
		"--opt", "platform=linux/arm64",
		"--opt", "image-resolve-mode=pull",
		"--no-cache",
		"--opt", "build-arg:A=1",
		"--opt", "build-arg:B=2",
		"--opt", "label:org.example.v=1",
		"--output", "type=docker,name=myapp:1,dest=/out/image.tar",
	}
	if !slices.Equal(args, wantSub) {
		t.Fatalf("buildctlArgs =\n  %v\nwant:\n  %v", args, wantSub)
	}

	// Reject dockerfile escaping context, bad buildargs JSON, bad tag, and remote context.
	for _, rawQuery := range []string{
		"dockerfile=../outside",
		"buildargs=not-json",
		"labels=not-json",
		"t=-bad",
		"t=bad,comma",
		"platform=-bad",
	} {
		r := httptest.NewRequest(http.MethodPost, "/build?"+rawQuery, nil)
		if _, err := parseBuildRequest(r); err == nil || statusOf(err) != http.StatusBadRequest {
			t.Errorf("query %q: err=%v (status %d), want 400", rawQuery, err, statusOf(err))
		}
	}
	rRemote := httptest.NewRequest(http.MethodPost, "/build?remote=https://example.com/repo.git", nil)
	if _, err := parseBuildRequest(rRemote); err == nil || statusOf(err) != http.StatusNotImplemented {
		t.Errorf("remote query: err=%v, want 501", err)
	}
}

func TestExtractBuildContextTarAndGzip(t *testing.T) {
	rawTar := makeTestTar(t, []struct {
		hdr  tar.Header
		body string
	}{
		{hdr: tar.Header{Name: "Dockerfile"}, body: "FROM alpine:3.20\n"},
		{hdr: tar.Header{Name: "sub/", Typeflag: tar.TypeDir, Mode: 0o755}},
		{hdr: tar.Header{Name: "sub/app.txt", Mode: 0o644}, body: "hello"},
		{hdr: tar.Header{Name: "sub/link.txt", Typeflag: tar.TypeSymlink, Linkname: "app.txt"}},
	})

	for _, tc := range []struct {
		name string
		data []byte
	}{
		{"tar", rawTar},
		{"gzip", gzipBytes(t, rawTar)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dst := t.TempDir()
			if err := extractBuildContext(bytes.NewReader(tc.data), dst, 1<<20); err != nil {
				t.Fatalf("extractBuildContext(%s): %v", tc.name, err)
			}
			got, err := os.ReadFile(filepath.Join(dst, "Dockerfile"))
			if err != nil || string(got) != "FROM alpine:3.20\n" {
				t.Fatalf("Dockerfile = %q, %v", string(got), err)
			}
			got2, err := os.ReadFile(filepath.Join(dst, "sub", "app.txt"))
			if err != nil || string(got2) != "hello" {
				t.Fatalf("sub/app.txt = %q, %v", string(got2), err)
			}
			if runtime.GOOS != "windows" {
				gotLink, err := os.ReadFile(filepath.Join(dst, "sub", "link.txt"))
				if err != nil || string(gotLink) != "hello" {
					t.Fatalf("sub/link.txt = %q, %v", string(gotLink), err)
				}
			}
		})
	}
}

func TestExtractBuildContextRejectsTraversalAndCap(t *testing.T) {
	badArchives := []struct {
		name    string
		entries []struct {
			hdr  tar.Header
			body string
		}
	}{
		{"parent_escape", []struct {
			hdr  tar.Header
			body string
		}{{hdr: tar.Header{Name: "../escape.txt"}, body: "bad"}}},
		{"nested_escape", []struct {
			hdr  tar.Header
			body string
		}{{hdr: tar.Header{Name: "a/../../escape.txt"}, body: "bad"}}},
		{"abs_path", []struct {
			hdr  tar.Header
			body string
		}{{hdr: tar.Header{Name: "/etc/passwd"}, body: "bad"}}},
		{"abs_symlink", []struct {
			hdr  tar.Header
			body string
		}{{hdr: tar.Header{Name: "s", Typeflag: tar.TypeSymlink, Linkname: "/etc/passwd"}}}},
		{"rel_symlink_escape", []struct {
			hdr  tar.Header
			body string
		}{{hdr: tar.Header{Name: "a/s", Typeflag: tar.TypeSymlink, Linkname: "../../outside"}}}},
		{"hardlink_escape", []struct {
			hdr  tar.Header
			body string
		}{{hdr: tar.Header{Name: "h", Typeflag: tar.TypeLink, Linkname: "../outside"}}}},
	}

	for _, tc := range badArchives {
		t.Run(tc.name, func(t *testing.T) {
			data := makeTestTar(t, tc.entries)
			err := extractBuildContext(bytes.NewReader(data), t.TempDir(), 1<<20)
			if err == nil || statusOf(err) != http.StatusBadRequest {
				t.Fatalf("%s: err=%v, want 400", tc.name, err)
			}
		})
	}

	// Size cap enforced.
	bigTar := makeTestTar(t, []struct {
		hdr  tar.Header
		body string
	}{{hdr: tar.Header{Name: "big.bin"}, body: strings.Repeat("x", 2048)}})
	if err := extractBuildContext(bytes.NewReader(bigTar), t.TempDir(), 512); err == nil {
		t.Fatal("expected size cap error for archive exceeding maxBytes")
	}
}

func TestBuildStreamFramingAndTarImageID(t *testing.T) {
	rec := httptest.NewRecorder()
	sw := newBuildStreamWriter(rec, false)
	lb := &buildLineBuffer{emit: sw.stream}
	lb.write([]byte("#1 step one\r\n#2 step "))
	lb.write([]byte("two\npartial"))
	lb.flush()
	sw.aux("sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef")
	sw.err(2, "RUN failed")

	lines := strings.Split(strings.TrimSpace(rec.Body.String()), "\n")
	if len(lines) != 5 {
		t.Fatalf("got %d JSON frames, want 5:\n%s", len(lines), rec.Body.String())
	}
	var f0, f3, f4 map[string]any
	_ = json.Unmarshal([]byte(lines[0]), &f0)
	_ = json.Unmarshal([]byte(lines[3]), &f3)
	_ = json.Unmarshal([]byte(lines[4]), &f4)
	if f0["stream"] != "#1 step one\n" {
		t.Errorf("frame 0 = %v", f0)
	}
	auxMap, _ := f3["aux"].(map[string]any)
	if auxMap["ID"] != "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef" {
		t.Errorf("frame 3 aux = %v", f3)
	}
	if f4["error"] != "RUN failed" {
		t.Errorf("frame 4 error = %v", f4)
	}

	// extractTarImageID handles both OCI/BuildKit layout and legacy <hex>.json.
	hexID := fakeHex("built-img")
	for _, cfg := range []string{"blobs/sha256/" + hexID, hexID + ".json"} {
		tarPath := filepath.Join(t.TempDir(), "image.tar")
		manifest := `[{"Config":"` + cfg + `","RepoTags":["demo:1"]}]`
		data := makeTestTar(t, []struct {
			hdr  tar.Header
			body string
		}{{hdr: tar.Header{Name: "manifest.json"}, body: manifest}})
		if err := os.WriteFile(tarPath, data, 0o600); err != nil {
			t.Fatal(err)
		}
		got, err := extractTarImageID(tarPath)
		if err != nil || got != "sha256:"+hexID {
			t.Errorf("extractTarImageID(%q) = %q, %v; want sha256:%s", cfg, got, err, hexID)
		}
	}
}

func TestLegacyBuildEndpointTagAfterLoadAndFailure(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uses unix backend socket in fake buildkit")
	}
	dir, err := os.MkdirTemp("", "gvxd-lb-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })

	fake := newFakeRunner()
	fake.addImage(BuilderImage, ImageConfig{})

	var failNext bool
	var capturedArgs []string
	var capturedDockerfile string

	fake.Script = func(p *fakeProc) int {
		if p.Spec.ID == builderSandboxID {
			var sockPath string
			for _, port := range p.Spec.Ports {
				if rest, ok := strings.CutPrefix(port, "unix:"); ok {
					if i := strings.LastIndexByte(rest, ':'); i > 0 {
						sockPath = rest[:i]
					}
				}
			}
			_ = os.Remove(sockPath)
			ln, err := net.Listen("unix", sockPath)
			if err != nil {
				return 1
			}
			defer func() {
				_ = ln.Close()
				_ = os.Remove(sockPath)
			}()
			go func() {
				for {
					c, err := ln.Accept()
					if err != nil {
						return
					}
					_ = c.Close()
				}
			}()
			code := <-p.end
			select {
			case p.end <- code:
			default:
			}
			return code
		}

		// One-shot buildctl sandbox.
		if strings.HasPrefix(p.Spec.ID, builderBuildctlPrefix) {
			args, _ := rungv.SplitArgs(p.Spec.Command)
			capturedArgs = args
			var ctxHost, outHost string
			for _, m := range p.Spec.Mounts {
				if m.Target == "/ctx" {
					ctxHost = m.Source
				}
				if m.Target == "/out" {
					outHost = m.Source
				}
			}
			if b, err := os.ReadFile(filepath.Join(ctxHost, "Dockerfile")); err == nil {
				capturedDockerfile = string(b)
			}
			p.Write(sandbox.Stderr, "#1 [1/1] FROM alpine:3.20\n")
			if failNext {
				p.Write(sandbox.Stderr, "error: RUN false exited with code 1\n")
				return 1
			}
			// fakeRunner.LoadImages reads the archive contents as the image ref.
			if err := os.WriteFile(filepath.Join(outHost, "image.tar"), []byte("app:v1\n"), 0o600); err != nil {
				return 1
			}
			p.Write(sandbox.Stderr, "#1 DONE 0.1s\n")
			return 0
		}
		return 0
	}

	e := New(Config{Dir: dir, Runner: fake, Version: "0.0.0-test", BuilderSocketDir: dir})
	if err := e.Open(context.Background()); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(e.Handler())
	t.Cleanup(func() {
		srv.Close()
		_ = e.Close(time.Second)
	})

	ctxArchive := gzipBytes(t, makeTestTar(t, []struct {
		hdr  tar.Header
		body string
	}{{hdr: tar.Header{Name: "Dockerfile"}, body: "FROM alpine:3.20\nRUN echo ok\n"}}))

	// 1. Successful build with two tags: verify stream, aux ID, and tag-after-load.
	req, err := http.NewRequest(http.MethodPost, srv.URL+"/v1.52/build?t=app:v1&t=app:latest", bytes.NewReader(ctxArchive))
	if err != nil {
		t.Fatal(err)
	}
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("POST /build = %d %s", resp.StatusCode, body)
	}
	if !strings.Contains(capturedDockerfile, "FROM alpine:3.20") {
		t.Fatalf("extracted Dockerfile = %q", capturedDockerfile)
	}
	if !slices.Contains(capturedArgs, "type=docker,name=app:v1,dest=/out/image.tar") {
		t.Fatalf("unexpected buildctl args: %v", capturedArgs)
	}
	if !strings.Contains(string(body), `"stream":"#1 [1/1] FROM alpine:3.20\n"`) {
		t.Errorf("missing progress stream in body:\n%s", body)
	}
	if !strings.Contains(string(body), `"aux":{"ID":"sha256:`) {
		t.Errorf("missing aux ID in body:\n%s", body)
	}
	if !strings.Contains(string(body), "Successfully tagged app:v1") || !strings.Contains(string(body), "Successfully tagged app:latest") {
		t.Errorf("missing Successfully tagged lines:\n%s", body)
	}
	// Both tags must exist in the runner's image store.
	if _, err := fake.InspectImages(context.Background(), "app:v1", "app:latest"); err != nil {
		t.Fatalf("tags missing after load: %v", err)
	}

	// Temp build dirs cleaned up.
	entries, _ := os.ReadDir(filepath.Join(dir, "builds"))
	if len(entries) != 0 {
		t.Errorf("temp build dirs not cleaned up: %v", entries)
	}

	// 2. Failing build streams errorDetail and leaves no new image.
	failNext = true
	req2, _ := http.NewRequest(http.MethodPost, srv.URL+"/v1.52/build?t=broken:1", bytes.NewReader(ctxArchive))
	resp2, err := srv.Client().Do(req2)
	if err != nil {
		t.Fatal(err)
	}
	body2, _ := io.ReadAll(resp2.Body)
	resp2.Body.Close()
	if !strings.Contains(string(body2), `"errorDetail"`) || !strings.Contains(string(body2), "RUN false exited with code 1") {
		t.Fatalf("failed build response missing errorDetail:\n%s", body2)
	}
	if _, err := fake.InspectImages(context.Background(), "broken:1"); err == nil {
		t.Fatal("failed build should not leave image broken:1")
	}
}
