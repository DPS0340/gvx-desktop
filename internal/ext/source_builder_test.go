package ext

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestParseGitSourceAndRejections(t *testing.T) {
	valid := []struct {
		in      string
		wantURL string
		wantRef string
	}{
		{"ironpark/gvx-ext-actions", "https://github.com/ironpark/gvx-ext-actions", ""},
		{"ironpark/gvx-ext-actions@v0.1.0", "https://github.com/ironpark/gvx-ext-actions", "v0.1.0"},
		{"https://github.com/ironpark/gvx-ext-actions", "https://github.com/ironpark/gvx-ext-actions", ""},
		{"https://github.com/ironpark/gvx-ext-actions.git@main", "https://github.com/ironpark/gvx-ext-actions.git", "main"},
	}
	for _, tc := range valid {
		u, r, err := ParseGitSource(tc.in)
		if err != nil {
			t.Errorf("ParseGitSource(%q) unexpected error: %v", tc.in, err)
			continue
		}
		if u != tc.wantURL || r != tc.wantRef {
			t.Errorf("ParseGitSource(%q) = (%q, %q), want (%q, %q)", tc.in, u, r, tc.wantURL, tc.wantRef)
		}
	}

	invalid := []string{
		"",
		"-upload-pack=evil",
		"http://github.com/a/b",
		"file:///tmp/repo",
		"ssh://git@github.com/a/b",
		"git@github.com:a/b",
		"https://user:pass@github.com/a/b",
		"owner/repo@",
		"owner/repo@-branch",
		"owner/repo@../main",
		"https://github.com/owner/repo?foo=1",
		"owner/repo?foo=1",
		"owner/repo#frag",
		"owner/repo\x1b",
		"owner/repo\u202e",
		"owner/-repo",
		"-owner/repo",
		"owner/..",
		"owner/repo@v1\x1b",
		"owner/repo@v1#frag",
		"owner/repo@v1?q=1",
	}
	for _, raw := range invalid {
		if _, _, err := ParseGitSource(raw); err == nil {
			t.Errorf("ParseGitSource(%q) = nil, want error", raw)
		}
	}

	if _, err := AcquireGit(context.Background(), t.TempDir(), "file:///tmp/repo", "", nil); err == nil {
		t.Fatal("AcquireGit accepted file:// URL")
	}
}

func TestStageGitInstallAndUpdateCheckWithLocalBareRepo(t *testing.T) {
	gitBin, err := exec.LookPath("git")
	if err != nil {
		t.Skip("git binary unavailable")
	}

	tmp := t.TempDir()
	bareRepo := filepath.Join(tmp, "remote.git")
	workRepo := filepath.Join(tmp, "work")
	dataDir := filepath.Join(tmp, "data")

	runGit := func(dir string, args ...string) string {
		t.Helper()
		cmd := exec.Command(gitBin, args...)
		if dir != "" {
			cmd.Dir = dir
		}
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v failed: %v\n%s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}

	runGit("", "init", "--bare", bareRepo)
	runGit("", "clone", bareRepo, workRepo)
	runGit(workRepo, "config", "user.email", "test@example.com")
	runGit(workRepo, "config", "user.name", "Test")
	runGit(workRepo, "checkout", "-b", "main")

	if err := os.WriteFile(filepath.Join(workRepo, ManifestName), []byte(`
schema_version = 1
id = "git-demo"
version = "0.1.0"
publisher = "acme"
name = "Git Demo"
`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workRepo, "Dockerfile"), []byte("FROM scratch\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(workRepo, "add", ".")
	runGit(workRepo, "commit", "-m", "initial")
	runGit(workRepo, "push", "-u", "origin", "main")
	sha1 := runGit(workRepo, "rev-parse", "HEAD")

	repoURL := "file://" + filepath.ToSlash(bareRepo)
	if runtime.GOOS == "windows" {
		repoURL = "file:///" + filepath.ToSlash(bareRepo)
	}

	m, spec, err := stageGitInstall(context.Background(), dataDir, repoURL, "main", true, nil)
	if err != nil {
		t.Fatalf("stageGitInstall: %v", err)
	}
	if m.ID != "git-demo" || spec.Kind != SourceGit || spec.Commit != sha1 || spec.ImageID != "" {
		t.Fatalf("unexpected staged git result: m=%+v spec=%+v", m, spec)
	}
	if _, err := os.Stat(SourceCheckoutDir(dataDir, "git-demo")); err != nil {
		t.Fatalf("expected checkout src/ dir: %v", err)
	}

	// No update initially.
	remoteSHA, hasUpdate, err := checkGitUpdate(context.Background(), repoURL, "main", sha1)
	if err != nil || hasUpdate || remoteSHA != sha1 {
		t.Fatalf("checkGitUpdate (same) = (%q, %v, %v), want (%q, false, nil)", remoteSHA, hasUpdate, err, sha1)
	}

	// Push a second commit and confirm update detection.
	if err := os.WriteFile(filepath.Join(workRepo, "README.md"), []byte("updated\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(workRepo, "add", "README.md")
	runGit(workRepo, "commit", "-m", "second")
	runGit(workRepo, "push", "origin", "main")
	sha2 := runGit(workRepo, "rev-parse", "HEAD")

	remoteSHA2, hasUpdate2, err := checkGitUpdate(context.Background(), repoURL, "main", sha1)
	if err != nil || !hasUpdate2 || remoteSHA2 != sha2 {
		t.Fatalf("checkGitUpdate (new) = (%q, %v, %v), want (%q, true, nil)", remoteSHA2, hasUpdate2, err, sha2)
	}
}

func TestStageLocalAndAcquireImage(t *testing.T) {
	dataDir := t.TempDir()
	localDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(localDir, ManifestName), []byte(`
schema_version = 1
id = "local-demo"
version = "0.2.0"
publisher = "acme"
name = "Local Demo"
`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(localDir, "Dockerfile"), []byte("FROM scratch\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	m, spec, err := StageLocalInstall(dataDir, localDir)
	if err != nil {
		t.Fatalf("StageLocalInstall: %v", err)
	}
	if m.ID != "local-demo" || spec.Kind != SourceLocal || spec.LocalPath == "" {
		t.Fatalf("unexpected local spec: %+v", spec)
	}
	if _, err := os.Stat(SourceCheckoutDir(dataDir, "local-demo")); !os.IsNotExist(err) {
		t.Fatal("local install should NOT create src/ checkout directory")
	}

	// AcquireImage with fake ImagePuller.
	puller := &fakeImagePuller{}
	if err := AcquireImage(context.Background(), puller, "ghcr.io/acme/ext:1.0.0", nil); err != nil {
		t.Fatalf("AcquireImage: %v", err)
	}
	if puller.pulled != "ghcr.io/acme/ext:1.0.0" {
		t.Fatalf("pulled = %q", puller.pulled)
	}
	if err := AcquireImage(context.Background(), puller, "--bad-ref", nil); err == nil {
		t.Fatal("AcquireImage accepted option-like ref")
	}
}

type fakeImagePuller struct {
	pulled string
	err    error
}

func (f *fakeImagePuller) Pull(_ context.Context, ref string, line func(string)) error {
	f.pulled = ref
	if line != nil {
		line("pulling " + ref)
	}
	return f.err
}

func TestPackBuildContextDockerignoreGitSymlinkAndSizeLimit(t *testing.T) {
	ctxDir := t.TempDir()
	outside := t.TempDir()

	if err := os.MkdirAll(filepath.Join(ctxDir, ".git", "objects"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ctxDir, ".git", "HEAD"), []byte("ref: refs/heads/main\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(ctxDir, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(ctxDir, "build"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ctxDir, "Dockerfile"), []byte("FROM scratch\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ctxDir, "app.go"), []byte("package main\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ctxDir, "debug.log"), []byte("drop\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ctxDir, "important.log"), []byte("keep\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ctxDir, "build", "out.bin"), []byte("drop\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ctxDir, "sub", "secret.txt"), []byte("drop\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ctxDir, "sub", "keep.txt"), []byte("keep\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ctxDir, ".dockerignore"), []byte(`
# comment
*.log
!important.log
build/
**/secret.txt
!sub/keep.txt
`), 0o644); err != nil {
		t.Fatal(err)
	}

	var buf bytes.Buffer
	if err := PackBuildContext(context.Background(), ctxDir, "Dockerfile", &buf); err != nil {
		t.Fatalf("PackBuildContext: %v", err)
	}

	gz, err := gzip.NewReader(&buf)
	if err != nil {
		t.Fatal(err)
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	var names []string
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		names = append(names, hdr.Name)
	}
	has := func(name string) bool {
		for _, n := range names {
			if n == name {
				return true
			}
		}
		return false
	}
	if !has("Dockerfile") || !has("app.go") || !has("important.log") || !has("sub/keep.txt") {
		t.Fatalf("expected files missing in packed tar: %v", names)
	}
	if has(".git") || has(".git/HEAD") || has("debug.log") || has("build/out.bin") || has("sub/secret.txt") {
		t.Fatalf("ignored/.git files leaked into packed tar: %v", names)
	}

	// Symlink escape must be rejected.
	if runtime.GOOS != "windows" {
		if err := os.WriteFile(filepath.Join(outside, "host-secret"), []byte("nope"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(filepath.Join(outside, "host-secret"), filepath.Join(ctxDir, "leak")); err != nil {
			t.Fatal(err)
		}
		if err := PackBuildContext(context.Background(), ctxDir, "Dockerfile", io.Discard); err == nil {
			t.Fatal("PackBuildContext should reject escaping symlink")
		}
		_ = os.Remove(filepath.Join(ctxDir, "leak"))
	}

	// 512 MiB context size cap (sparse file).
	hugePath := filepath.Join(ctxDir, "huge.bin")
	f, err := os.Create(hugePath)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Truncate(MaxBuildContextBytes + 1); err != nil {
		f.Close()
		t.Skipf("sparse truncate unsupported: %v", err)
	}
	f.Close()
	if err := PackBuildContext(context.Background(), ctxDir, "Dockerfile", io.Discard); err == nil {
		t.Fatal("PackBuildContext should reject >512 MiB context")
	}
}

func TestEngineImageBuilderStreamingAndErrors(t *testing.T) {
	srcDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(srcDir, "Dockerfile"), []byte("FROM scratch\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	m, err := ParseManifest([]byte(`
schema_version = 1
id = "demo-build"
version = "1.2.3"
publisher = "acme"
name = "Demo Build"

[runtime]
target = "final"
`))
	if err != nil {
		t.Fatal(err)
	}

	var gotPath, gotDF, gotTarget, gotPlatform, gotTag, gotLabels string
	var tarFiles int

	mockHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		q := r.URL.Query()
		gotDF = q.Get("dockerfile")
		gotTarget = q.Get("target")
		gotPlatform = q.Get("platform")
		gotTag = q.Get("t")
		gotLabels = q.Get("labels")

		gz, err := gzip.NewReader(r.Body)
		if err != nil {
			http.Error(w, `{"message":"bad gzip"}`, http.StatusBadRequest)
			return
		}
		defer gz.Close()
		tr := tar.NewReader(gz)
		for {
			_, err := tr.Next()
			if errors.Is(err, io.EOF) {
				break
			}
			if err != nil {
				http.Error(w, `{"message":"bad tar"}`, http.StatusBadRequest)
				return
			}
			tarFiles++
		}

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		fl, _ := w.(http.Flusher)
		_, _ = io.WriteString(w, `{"stream":"Step 1/1 : FROM scratch\n"}`+"\n")
		if fl != nil {
			fl.Flush()
		}
		_, _ = io.WriteString(w, `{"aux":{"ID":"sha256:deadbeef1234"}}`+"\n")
		if fl != nil {
			fl.Flush()
		}
	})

	builder := NewEngineImageBuilder(func() http.Handler { return mockHandler })
	var lines []string
	res, err := BuildFromDir(context.Background(), builder, srcDir, m, "0123456789abcdef", func(line string) {
		lines = append(lines, line)
	})
	if err != nil {
		t.Fatalf("BuildFromDir: %v", err)
	}
	if gotPath != "/v1.52/build" || gotDF != "Dockerfile" || gotTarget != "final" || gotPlatform != "linux/"+runtime.GOARCH {
		t.Fatalf("unexpected build query: path=%q df=%q target=%q platform=%q", gotPath, gotDF, gotTarget, gotPlatform)
	}
	if gotTag != "gvx-ext/demo-build:1.2.3-01234567" {
		t.Fatalf("tag = %q", gotTag)
	}
	if !strings.Contains(gotLabels, `"io.gvx.extension":"demo-build"`) {
		t.Fatalf("labels = %q", gotLabels)
	}
	if tarFiles == 0 {
		t.Fatal("expected tar entries in request body")
	}
	if res.ImageID != "sha256:deadbeef1234" || len(lines) != 1 || lines[0] != "Step 1/1 : FROM scratch" {
		t.Fatalf("unexpected build result: res=%+v lines=%v", res, lines)
	}

	// ErrorDetail in NDJSON stream must fail Build.
	errBuilder := NewEngineImageBuilder(func() http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_, _ = io.Copy(io.Discard, r.Body)
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"errorDetail":{"code":1,"message":"RUN failed"},"error":"RUN failed"}`+"\n")
		})
	})
	if _, err := BuildFromDir(context.Background(), errBuilder, srcDir, m, "", nil); err == nil || !strings.Contains(err.Error(), "RUN failed") {
		t.Fatalf("expected RUN failed error, got %v", err)
	}

	// HTTP 503 JSON error must fail cleanly.
	unavailable := NewEngineImageBuilder(func() http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = io.WriteString(w, `{"message":"engine is not running"}`)
		})
	})
	if _, err := BuildFromDir(context.Background(), unavailable, srcDir, m, "", nil); err == nil || !strings.Contains(err.Error(), "engine is not running") {
		t.Fatalf("expected 503 engine error, got %v", err)
	}
}

func TestPackBuildContextNormalizesInternalAbsSymlink(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX symlinks not tested on windows")
	}
	ctxDir := t.TempDir()
	// Resolve ctxDir first so macOS /var -> /private/var matches resolved paths cleanly.
	realCtx, err := filepath.EvalSymlinks(ctxDir)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(realCtx, "Dockerfile"), []byte("FROM scratch\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(realCtx, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(realCtx, "dir"), 0o755); err != nil {
		t.Fatal(err)
	}
	targetFile := filepath.Join(realCtx, "sub", "target.txt")
	if err := os.WriteFile(targetFile, []byte("hello\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	linkPath := filepath.Join(realCtx, "dir", "link.txt")
	if err := os.Symlink(targetFile, linkPath); err != nil {
		t.Fatal(err)
	}

	var buf bytes.Buffer
	if err := PackBuildContext(context.Background(), realCtx, "Dockerfile", &buf); err != nil {
		t.Fatalf("PackBuildContext: %v", err)
	}
	gz, err := gzip.NewReader(&buf)
	if err != nil {
		t.Fatal(err)
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	found := false
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if hdr.Name == "dir/link.txt" {
			found = true
			if hdr.Typeflag != tar.TypeSymlink {
				t.Fatalf("expected symlink tar entry, got type %v", hdr.Typeflag)
			}
			if filepath.IsAbs(hdr.Linkname) || strings.HasPrefix(hdr.Linkname, "/") {
				t.Fatalf("expected normalized relative Linkname, got %q", hdr.Linkname)
			}
			if hdr.Linkname != "../sub/target.txt" {
				t.Fatalf("Linkname = %q, want ../sub/target.txt", hdr.Linkname)
			}
		}
	}
	if !found {
		t.Fatal("dir/link.txt missing from packed tar")
	}
}

func TestEngineImageBuilderCancellationMalformedJSONAndMissingAuxID(t *testing.T) {
	srcDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(srcDir, "Dockerfile"), []byte("FROM scratch\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Write enough incompressible data so gzip + io.PipeWriter blocks if r.Body isn't drained.
	big := bytes.Repeat([]byte("0123456789abcdef"), 16*1024)
	for i := range big {
		big[i] = byte((i*31 + 7) % 251)
	}
	if err := os.WriteFile(filepath.Join(srcDir, "payload.bin"), big, 0o644); err != nil {
		t.Fatal(err)
	}

	// 1. Context cancellation when HTTP 200 handler stops reading r.Body and waits on ctx.Done().
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	cancelBuilder := NewEngineImageBuilder(func() http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			if fl, ok := w.(http.Flusher); ok {
				fl.Flush()
			}
			<-r.Context().Done()
		})
	})
	done := make(chan error, 1)
	go func() {
		_, err := cancelBuilder.Build(ctx, BuildRequest{ContextDir: srcDir, Dockerfile: "Dockerfile"}, nil)
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("expected error on cancelled build")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("EngineImageBuilder.Build hung on context cancellation")
	}

	// 2. Early HTTP 200 exit without draining r.Body must not deadlock.
	earlyBuilder := NewEngineImageBuilder(func() http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(w, `{"aux":{"ID":"sha256:earlyok"}}`+"\n")
		})
	})
	id, err := earlyBuilder.Build(context.Background(), BuildRequest{ContextDir: srcDir, Dockerfile: "Dockerfile"}, nil)
	if err != nil || id != "sha256:earlyok" {
		t.Fatalf("early 200 exit = (%q, %v), want (sha256:earlyok, nil)", id, err)
	}

	// 3. Malformed JSON on HTTP 200 must return an error.
	badJSONBuilder := NewEngineImageBuilder(func() http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_, _ = io.Copy(io.Discard, r.Body)
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, "{not json\n")
		})
	})
	if _, err := badJSONBuilder.Build(context.Background(), BuildRequest{ContextDir: srcDir, Dockerfile: "Dockerfile"}, nil); err == nil {
		t.Fatal("expected error on malformed build stream JSON")
	}

	// 4. HTTP 200 stream completing without aux.ID must return an error.
	noAuxBuilder := NewEngineImageBuilder(func() http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_, _ = io.Copy(io.Discard, r.Body)
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"stream":"done\n"}`+"\n")
		})
	})
	if _, err := noAuxBuilder.Build(context.Background(), BuildRequest{ContextDir: srcDir, Dockerfile: "Dockerfile"}, nil); err == nil {
		t.Fatal("expected error when HTTP 200 build stream has no aux.ID")
	}
}

type captureBuildReqBuilder struct {
	gotReq      BuildRequest
	contextBody map[string]string
}

func (c *captureBuildReqBuilder) Build(_ context.Context, req BuildRequest, _ func(string)) (string, error) {
	c.gotReq = req
	c.contextBody = make(map[string]string)
	err := filepath.WalkDir(req.ContextDir, func(p string, d os.DirEntry, walkErr error) error {
		if walkErr != nil || d.IsDir() {
			return walkErr
		}
		rel, err := filepath.Rel(req.ContextDir, p)
		if err != nil {
			return err
		}
		raw, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		c.contextBody[filepath.ToSlash(rel)] = string(raw)
		return nil
	})
	if err != nil {
		return "", err
	}
	return "sha256:captured", nil
}

func TestBuildFromDir_RepoRootDockerfileWithSubdirContext(t *testing.T) {
	repoDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(repoDir, "docker"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(repoDir, "sub", "ctx"), 0o700); err != nil {
		t.Fatal(err)
	}
	dfContent := "FROM alpine:3.20\nCOPY app.txt /app.txt\n"
	if err := os.WriteFile(filepath.Join(repoDir, "docker", "Dockerfile"), []byte(dfContent), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repoDir, "sub", "ctx", "app.txt"), []byte("hello-ctx"), 0o600); err != nil {
		t.Fatal(err)
	}
	// Existing Dockerfile inside sub/ctx should NOT be overwritten; BuildFromDir should stage a non-colliding filename.
	if err := os.WriteFile(filepath.Join(repoDir, "sub", "ctx", "Dockerfile"), []byte("FROM scratch\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repoDir, "unrelated.secret"), []byte("do-not-leak"), 0o600); err != nil {
		t.Fatal(err)
	}

	manifestTOML := `
schema_version = 1
id = "ctx-split"
version = "0.1.0"
publisher = "acme"
name = "Split Context"
[runtime]
type = "sandbox"
dockerfile = "docker/Dockerfile"
context = "sub/ctx"
`
	if err := os.WriteFile(filepath.Join(repoDir, ManifestName), []byte(manifestTOML), 0o600); err != nil {
		t.Fatal(err)
	}
	m, err := ParseManifestDir(repoDir, "ctx-split")
	if err != nil {
		t.Fatalf("ParseManifestDir: %v", err)
	}

	capBuilder := &captureBuildReqBuilder{}
	res, err := BuildFromDir(context.Background(), capBuilder, repoDir, m, "", nil)
	if err != nil {
		t.Fatalf("BuildFromDir: %v", err)
	}
	if res.ImageID != "sha256:captured" {
		t.Fatalf("ImageID = %q, want sha256:captured", res.ImageID)
	}
	if capBuilder.contextBody["app.txt"] != "hello-ctx" {
		t.Fatalf("missing sub/ctx file in staged context: %+v", capBuilder.contextBody)
	}
	if _, leaked := capBuilder.contextBody["unrelated.secret"]; leaked {
		t.Fatalf("unrelated repo-root file leaked into staged context: %+v", capBuilder.contextBody)
	}
	if capBuilder.contextBody[capBuilder.gotReq.Dockerfile] != dfContent {
		t.Fatalf("staged dockerfile %q body = %q, want %q", capBuilder.gotReq.Dockerfile, capBuilder.contextBody[capBuilder.gotReq.Dockerfile], dfContent)
	}
	if capBuilder.contextBody["Dockerfile"] != "FROM scratch\n" {
		t.Fatalf("existing sub/ctx/Dockerfile was overwritten: %q", capBuilder.contextBody["Dockerfile"])
	}
}
