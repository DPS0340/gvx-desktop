package ext

import (
	"archive/tar"
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"unicode"
)

// MaxBuildContextBytes enforces the 512 MiB uncompressed build context cap
// matching internal/engine/build.go.
const MaxBuildContextBytes = 512 << 20

// ExtensionImageTag returns the local OCI tag for an extension build:
// gvx-ext/<id>:<version>-<shortsha> (or "local" when commitSHA is empty).
func ExtensionImageTag(id, version, commitSHA string) string {
	short := "local"
	cleaned := strings.Map(func(r rune) rune {
		if (r >= 'a' && r <= 'f') || (r >= '0' && r <= '9') {
			return r
		}
		if r >= 'A' && r <= 'F' {
			return unicode.ToLower(r)
		}
		return -1
	}, strings.TrimSpace(commitSHA))
	if len(cleaned) >= 8 {
		short = cleaned[:8]
	} else if len(cleaned) > 0 {
		short = cleaned
	}
	ver := strings.Map(func(r rune) rune {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '.' || r == '_' || r == '-' {
			return r
		}
		return '-'
	}, version)
	if ver == "" {
		ver = "0.0.0"
	}
	return fmt.Sprintf("gvx-ext/%s:%s-%s", id, ver, short)
}

type ignoreRule struct {
	pattern string
	negate  bool
	dirOnly bool
}

type dockerignoreMatcher struct {
	rules       []ignoreRule
	hasNegation bool
}

func parseDockerignore(data []byte) *dockerignoreMatcher {
	m := &dockerignoreMatcher{}
	scanner := bufio.NewScanner(bytes.NewReader(data))
	for scanner.Scan() {
		line := strings.TrimSuffix(scanner.Text(), "\r")
		if strings.HasPrefix(line, "#") {
			continue
		}
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		rule := ignoreRule{}
		if strings.HasPrefix(line, "!") {
			rule.negate = true
			m.hasNegation = true
			line = strings.TrimSpace(line[1:])
			if line == "" {
				continue
			}
		}
		if strings.HasSuffix(line, "/") {
			rule.dirOnly = true
			line = strings.TrimRight(line, "/")
		}
		line = strings.TrimLeft(line, "/")
		line = strings.TrimPrefix(line, "./")
		if line == "" {
			continue
		}
		rule.pattern = path.Clean(line)
		if rule.pattern == "." {
			continue
		}
		m.rules = append(m.rules, rule)
	}
	return m
}

func (m *dockerignoreMatcher) ignored(relSlash string, isDir bool) bool {
	if relSlash == ".git" || strings.HasPrefix(relSlash, ".git/") {
		return true
	}
	if m == nil || len(m.rules) == 0 {
		return false
	}
	ignored := false
	for _, r := range m.rules {
		if matchDockerignoreRule(r, relSlash, isDir) {
			ignored = !r.negate
		}
	}
	return ignored
}

func matchDockerignoreRule(r ignoreRule, relSlash string, isDir bool) bool {
	segs := strings.Split(relSlash, "/")
	patSegs := strings.Split(r.pattern, "/")
	if r.dirOnly {
		if isDir && matchSegments(patSegs, segs) {
			return true
		}
		// Any descendant inside a matched directory is also matched by a dir-only rule.
		for i := 1; i < len(segs); i++ {
			if matchSegments(patSegs, segs[:i]) {
				return true
			}
		}
		return false
	}
	if matchSegments(patSegs, segs) {
		return true
	}
	// Files/subdirs under an ignored directory pattern are also matched.
	for i := 1; i < len(segs); i++ {
		if matchSegments(patSegs, segs[:i]) {
			return true
		}
	}
	return false
}

func matchSegments(pat, name []string) bool {
	if len(pat) == 0 {
		return len(name) == 0
	}
	if pat[0] == "**" {
		// "**" matches zero or more path segments.
		for i := 0; i <= len(name); i++ {
			if matchSegments(pat[1:], name[i:]) {
				return true
			}
		}
		return false
	}
	if len(name) == 0 {
		return false
	}
	ok, err := path.Match(pat[0], name[0])
	if err != nil || !ok {
		return false
	}
	return matchSegments(pat[1:], name[1:])
}

// PackBuildContext archives contextDir into a gzipped tar stream written to w.
// It excludes .git/ unconditionally, honors .dockerignore (including ! negations),
// rejects symlinks that resolve outside contextDir, and enforces MaxBuildContextBytes.
func PackBuildContext(ctx context.Context, contextDir, dockerfileRel string, w io.Writer) error {
	realCtx, err := ResolveWithinRoot(contextDir, ".", true)
	if err != nil {
		return fmt.Errorf("build context: %w", err)
	}
	if dockerfileRel != "" {
		dfPath, err := ResolveWithinRoot(realCtx, dockerfileRel, false)
		if err != nil {
			return fmt.Errorf("dockerfile %q: %w", dockerfileRel, err)
		}
		info, err := os.Stat(dfPath)
		if err != nil {
			return fmt.Errorf("dockerfile %q: %w", dockerfileRel, err)
		}
		if info.IsDir() {
			return fmt.Errorf("dockerfile %q is a directory", dockerfileRel)
		}
	}

	var matcher *dockerignoreMatcher
	ignorePath := filepath.Join(realCtx, ".dockerignore")
	if _, err := os.Lstat(ignorePath); err == nil {
		resolvedIgnore, err := ResolveWithinRoot(realCtx, ".dockerignore", false)
		if err != nil {
			return fmt.Errorf(".dockerignore: %w", err)
		}
		rawIgnore, err := os.ReadFile(resolvedIgnore)
		if err != nil {
			return fmt.Errorf("read .dockerignore: %w", err)
		}
		matcher = parseDockerignore(rawIgnore)
	}

	gz := gzip.NewWriter(w)
	tw := tar.NewWriter(gz)

	var totalBytes int64
	walkErr := filepath.WalkDir(realCtx, func(fullPath string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if fullPath == realCtx {
			return nil
		}
		relOS, err := filepath.Rel(realCtx, fullPath)
		if err != nil {
			return err
		}
		relSlash := filepath.ToSlash(relOS)

		// Always exclude .git directory and contents unconditionally.
		if relSlash == ".git" || strings.HasPrefix(relSlash, ".git/") {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}

		isDir := d.IsDir()
		if matcher != nil && matcher.ignored(relSlash, isDir) {
			// If there are no negation rules, we can skip descending into ignored dirs.
			if isDir && !matcher.hasNegation {
				return filepath.SkipDir
			}
			return nil
		}

		info, err := d.Info()
		if err != nil {
			return err
		}
		mode := info.Mode()

		var linkTarget string
		if mode&os.ModeSymlink != 0 {
			resolved, err := filepath.EvalSymlinks(fullPath)
			if err != nil {
				return fmt.Errorf("build context symlink %q cannot be resolved: %w", relSlash, err)
			}
			if !isWithinDir(realCtx, resolved) {
				return fmt.Errorf("build context symlink %q escapes context directory", relSlash)
			}
			linkTarget, err = os.Readlink(fullPath)
			if err != nil {
				return err
			}
			if filepath.IsAbs(linkTarget) || strings.HasPrefix(linkTarget, "/") || strings.HasPrefix(linkTarget, "\\") {
				relTarget, err := filepath.Rel(filepath.Dir(fullPath), resolved)
				if err != nil {
					return fmt.Errorf("normalize build context symlink %q: %w", relSlash, err)
				}
				relSlashTarget := filepath.ToSlash(relTarget)
				if filepath.IsAbs(relTarget) || strings.HasPrefix(relSlashTarget, "/") || strings.HasPrefix(relSlashTarget, "\\") {
					return fmt.Errorf("build context symlink %q normalized target %q is invalid", relSlash, relSlashTarget)
				}
				linkTarget = relSlashTarget
			}
		} else if !mode.IsRegular() && !mode.IsDir() {
			return fmt.Errorf("build context unsupported file type at %q", relSlash)
		}

		if mode.IsRegular() {
			totalBytes += info.Size()
			if totalBytes > MaxBuildContextBytes {
				return fmt.Errorf("build context exceeds %d MiB limit", MaxBuildContextBytes>>20)
			}
		}

		hdr, err := tar.FileInfoHeader(info, linkTarget)
		if err != nil {
			return err
		}
		hdr.Name = relSlash
		if isDir && !strings.HasSuffix(hdr.Name, "/") {
			hdr.Name += "/"
		}
		if err := tw.WriteHeader(hdr); err != nil {
			return err
		}
		if mode.IsRegular() {
			f, err := os.Open(fullPath)
			if err != nil {
				return err
			}
			copyErr := copyNWithContext(ctx, tw, f, info.Size())
			closeErr := f.Close()
			if copyErr != nil {
				return copyErr
			}
			if closeErr != nil {
				return closeErr
			}
		}
		return nil
	})

	if walkErr != nil {
		_ = tw.Close()
		_ = gz.Close()
		return walkErr
	}
	if err := tw.Close(); err != nil {
		_ = gz.Close()
		return err
	}
	return gz.Close()
}

func copyNWithContext(ctx context.Context, dst io.Writer, src io.Reader, n int64) error {
	buf := make([]byte, 32*1024)
	remaining := n
	for remaining > 0 {
		if err := ctx.Err(); err != nil {
			return err
		}
		chunk := len(buf)
		if int64(chunk) > remaining {
			chunk = int(remaining)
		}
		nr, rerr := src.Read(buf[:chunk])
		if nr > 0 {
			nw, werr := dst.Write(buf[:nr])
			if werr != nil {
				return werr
			}
			if nw != nr {
				return io.ErrShortWrite
			}
			remaining -= int64(nr)
		}
		if rerr != nil {
			if errors.Is(rerr, io.EOF) {
				break
			}
			return rerr
		}
	}
	return nil
}

// EngineImageBuilder implements ImageBuilder by streaming a BuildKit tar context
// to internal/engine's POST /v1.52/build HTTP handler.
type EngineImageBuilder struct {
	handlerFn func() http.Handler
}

// NewEngineImageBuilder creates an EngineImageBuilder backed by handlerFn (e.g. engine.Handler).
func NewEngineImageBuilder(handlerFn func() http.Handler) *EngineImageBuilder {
	return &EngineImageBuilder{handlerFn: handlerFn}
}

// Build packs req.ContextDir, sends POST /v1.52/build to the engine handler,
// streams Docker NDJSON progress lines to onLine, and returns the built image ID.
func (b *EngineImageBuilder) Build(ctx context.Context, req BuildRequest, onLine func(string)) (string, error) {
	if b == nil || b.handlerFn == nil {
		return "", errors.New("image builder is not configured")
	}
	handler := b.handlerFn()
	if handler == nil {
		return "", errors.New("sandbox engine handler is unavailable")
	}

	dockerfile := req.Dockerfile
	if dockerfile == "" {
		dockerfile = "Dockerfile"
	}
	platform := req.Platform
	if platform == "" {
		platform = "linux/" + runtime.GOARCH
	}

	q := url.Values{}
	q.Set("dockerfile", dockerfile)
	q.Set("platform", platform)
	if req.Target != "" {
		q.Set("target", req.Target)
	}
	for _, tag := range req.Tags {
		if tag != "" {
			q.Add("t", tag)
		}
	}
	if req.NoCache {
		q.Set("nocache", "1")
	}
	if req.Pull {
		q.Set("pull", "1")
	}
	if len(req.BuildArgs) > 0 {
		raw, err := json.Marshal(req.BuildArgs)
		if err != nil {
			return "", err
		}
		q.Set("buildargs", string(raw))
	}
	if len(req.Labels) > 0 {
		raw, err := json.Marshal(req.Labels)
		if err != nil {
			return "", err
		}
		q.Set("labels", string(raw))
	}

	reqPR, reqPW := io.Pipe()
	packErrCh := make(chan error, 1)
	go func() {
		err := PackBuildContext(ctx, req.ContextDir, dockerfile, reqPW)
		if err != nil {
			_ = reqPW.CloseWithError(err)
		} else {
			_ = reqPW.Close()
		}
		packErrCh <- err
	}()

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, "/v1.52/build?"+q.Encode(), reqPR)
	if err != nil {
		_ = reqPR.Close()
		<-packErrCh
		return "", err
	}
	httpReq.Header.Set("Content-Type", "application/x-tar")

	respWriter := newStreamResponseWriter()
	buildDone := make(chan struct{})
	defer close(buildDone)
	go func() {
		select {
		case <-ctx.Done():
			_ = reqPR.CloseWithError(ctx.Err())
			_ = respWriter.reader.CloseWithError(ctx.Err())
		case <-buildDone:
		}
	}()

	go func() {
		defer respWriter.finish()
		handler.ServeHTTP(respWriter, httpReq)
	}()

	status, err := respWriter.awaitStatus(ctx)
	if err != nil {
		_ = reqPR.CloseWithError(err)
		_ = respWriter.reader.CloseWithError(err)
		<-packErrCh
		return "", err
	}
	if status >= 400 {
		body, _ := io.ReadAll(io.LimitReader(respWriter.reader, 64<<10))
		_ = respWriter.reader.Close()
		_ = reqPR.Close()
		packErr := <-packErrCh
		if ctxErr := ctx.Err(); ctxErr != nil {
			return "", ctxErr
		}
		var errPayload struct {
			Message string `json:"message"`
		}
		if json.Unmarshal(body, &errPayload) == nil && errPayload.Message != "" {
			if packErr != nil && !errors.Is(packErr, io.ErrClosedPipe) {
				return "", fmt.Errorf("%s (%v)", errPayload.Message, packErr)
			}
			return "", errors.New(errPayload.Message)
		}
		if packErr != nil && !errors.Is(packErr, io.ErrClosedPipe) {
			return "", fmt.Errorf("build failed with HTTP %d (%v)", status, packErr)
		}
		msg := strings.TrimSpace(string(body))
		if msg == "" {
			msg = http.StatusText(status)
		}
		return "", fmt.Errorf("build failed with HTTP %d: %s", status, msg)
	}

	var imageID string
	var streamErr error
	scanner := bufio.NewScanner(respWriter.reader)
	scanner.Buffer(make([]byte, 64*1024), 1024*1024)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		var frame struct {
			Stream string `json:"stream"`
			Aux    struct {
				ID string `json:"ID"`
			} `json:"aux"`
			Error       string `json:"error"`
			ErrorDetail struct {
				Message string `json:"message"`
			} `json:"errorDetail"`
		}
		if err := json.Unmarshal([]byte(line), &frame); err != nil {
			preview := line
			if len(preview) > 200 {
				preview = preview[:200] + "..."
			}
			streamErr = fmt.Errorf("malformed build stream JSON %q: %w", preview, err)
			break
		}
		if frame.Stream != "" && onLine != nil {
			for _, part := range strings.Split(strings.TrimRight(frame.Stream, "\r\n"), "\n") {
				onLine(part)
			}
		}
		if frame.Aux.ID != "" {
			imageID = frame.Aux.ID
		}
		if frame.ErrorDetail.Message != "" {
			streamErr = errors.New(frame.ErrorDetail.Message)
		} else if frame.Error != "" {
			streamErr = errors.New(frame.Error)
		}
	}
	scanErr := scanner.Err()
	_ = respWriter.reader.Close()
	_ = reqPR.Close()
	packErr := <-packErrCh

	if ctxErr := ctx.Err(); ctxErr != nil {
		return "", ctxErr
	}
	if packErr != nil && !errors.Is(packErr, io.ErrClosedPipe) {
		return "", packErr
	}
	if streamErr != nil {
		return "", streamErr
	}
	if scanErr != nil {
		return "", scanErr
	}
	if imageID == "" {
		return "", errors.New("build stream completed without an image ID")
	}
	return imageID, nil
}

// BuildFromDir builds an extension's sandbox image from sourceDir using builder,
// tagging it with ExtensionImageTag(m.ID, m.Version, commitSHA) and labeling
// io.gvx.extension=<id>.
func BuildFromDir(ctx context.Context, builder ImageBuilder, sourceDir string, m *Manifest, commitSHA string, onLine func(string)) (BuildResult, error) {
	if builder == nil {
		return BuildResult{}, errors.New("image builder is not configured")
	}
	if m == nil {
		return BuildResult{}, errors.New("manifest is nil")
	}
	if m.Runtime.Type != RuntimeSandbox {
		return BuildResult{}, fmt.Errorf("cannot build image for runtime.type %q", m.Runtime.Type)
	}
	if m.Runtime.Dockerfile == "" {
		return BuildResult{}, errors.New("manifest has no runtime.dockerfile for source build")
	}

	ctxRel := m.Runtime.Context
	if ctxRel == "" {
		ctxRel = "."
	}
	ctxDir, err := ResolveWithinRoot(sourceDir, ctxRel, true)
	if err != nil {
		return BuildResult{}, fmt.Errorf("runtime.context: %w", err)
	}
	dfPath, err := ResolveWithinRoot(sourceDir, m.Runtime.Dockerfile, false)
	if err != nil {
		return BuildResult{}, fmt.Errorf("runtime.dockerfile: %w", err)
	}
	dfInfo, err := os.Stat(dfPath)
	if err != nil {
		return BuildResult{}, fmt.Errorf("runtime.dockerfile: %w", err)
	}
	if dfInfo.IsDir() {
		return BuildResult{}, fmt.Errorf("runtime.dockerfile %q is a directory", m.Runtime.Dockerfile)
	}

	dockerfile := m.Runtime.Dockerfile
	buildCtxDir := ctxDir
	if ctxRel != "." {
		if strings.HasPrefix(dockerfile, ctxRel+"/") {
			dockerfile = strings.TrimPrefix(dockerfile, ctxRel+"/")
		} else {
			stageCtx, sErr := os.MkdirTemp("", "gvx-ext-ctx-*")
			if sErr != nil {
				return BuildResult{}, fmt.Errorf("stage build context: %w", sErr)
			}
			defer func() { _ = os.RemoveAll(stageCtx) }()
			if err := stageBuildContextDir(ctx, ctxDir, stageCtx); err != nil {
				return BuildResult{}, err
			}
			dfName := filepath.Base(dfPath)
			if dfName == "" || dfName == "." || dfName == ".." {
				dfName = ".gvx-ext.Dockerfile"
			}
			if _, lerr := os.Lstat(filepath.Join(stageCtx, dfName)); lerr == nil {
				for i := 0; ; i++ {
					cand := ".gvx-ext.Dockerfile"
					if i > 0 {
						cand = fmt.Sprintf(".gvx-ext-%d.Dockerfile", i)
					}
					if _, lerr := os.Lstat(filepath.Join(stageCtx, cand)); errors.Is(lerr, os.ErrNotExist) {
						dfName = cand
						break
					}
				}
			}
			if err := copyRegularFile(dfPath, filepath.Join(stageCtx, dfName), 0o600); err != nil {
				return BuildResult{}, fmt.Errorf("stage dockerfile %q: %w", m.Runtime.Dockerfile, err)
			}
			buildCtxDir = stageCtx
			dockerfile = dfName
		}
	}

	tag := ExtensionImageTag(m.ID, m.Version, commitSHA)
	req := BuildRequest{
		Dockerfile: dockerfile,
		ContextDir: buildCtxDir,
		Target:     m.Runtime.Target,
		Platform:   "linux/" + runtime.GOARCH,
		Tags:       []string{tag},
		Labels: map[string]string{
			"io.gvx.extension": m.ID,
		},
	}
	imageID, err := builder.Build(ctx, req, onLine)
	if err != nil {
		return BuildResult{}, err
	}
	return BuildResult{
		ImageID: imageID,
		Tags:    []string{tag},
	}, nil
}

func stageBuildContextDir(ctx context.Context, srcRoot, dstRoot string) error {
	var totalBytes int64
	return filepath.WalkDir(srcRoot, func(fullPath string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if fullPath == srcRoot {
			return nil
		}
		relOS, err := filepath.Rel(srcRoot, fullPath)
		if err != nil {
			return err
		}
		relSlash := filepath.ToSlash(relOS)
		if relSlash == ".git" || strings.HasPrefix(relSlash, ".git/") {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		mode := info.Mode()
		dstPath := filepath.Join(dstRoot, relOS)

		if mode&os.ModeSymlink != 0 {
			resolved, err := filepath.EvalSymlinks(fullPath)
			if err != nil {
				return fmt.Errorf("build context symlink %q cannot be resolved: %w", relSlash, err)
			}
			if !isWithinDir(srcRoot, resolved) {
				return fmt.Errorf("build context symlink %q escapes context directory", relSlash)
			}
			linkTarget, err := os.Readlink(fullPath)
			if err != nil {
				return err
			}
			if filepath.IsAbs(linkTarget) || strings.HasPrefix(linkTarget, "/") || strings.HasPrefix(linkTarget, "\\") {
				relTarget, err := filepath.Rel(filepath.Dir(fullPath), resolved)
				if err != nil {
					return fmt.Errorf("normalize build context symlink %q: %w", relSlash, err)
				}
				linkTarget = relTarget
			}
			if err := os.MkdirAll(filepath.Dir(dstPath), 0o700); err != nil {
				return err
			}
			return os.Symlink(linkTarget, dstPath)
		}
		if mode.IsDir() {
			return os.MkdirAll(dstPath, 0o700)
		}
		if !mode.IsRegular() {
			return fmt.Errorf("build context unsupported file type at %q", relSlash)
		}
		totalBytes += info.Size()
		if totalBytes > MaxBuildContextBytes {
			return fmt.Errorf("build context exceeds %d MiB limit", MaxBuildContextBytes>>20)
		}
		if err := os.MkdirAll(filepath.Dir(dstPath), 0o700); err != nil {
			return err
		}
		return copyRegularFile(fullPath, dstPath, mode.Perm())
	})
}

func copyRegularFile(srcPath, dstPath string, perm fs.FileMode) error {
	if perm == 0 {
		perm = 0o600
	}
	in, err := os.Open(srcPath)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dstPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, perm)
	if err != nil {
		return err
	}
	_, copyErr := io.Copy(out, in)
	closeErr := out.Close()
	if copyErr != nil {
		return copyErr
	}
	return closeErr
}

// streamResponseWriter bridges an http.Handler's streaming writes/Flush calls
// into an io.PipeReader without buffering the entire response in memory.
type streamResponseWriter struct {
	mu      sync.Mutex
	header  http.Header
	status  int
	wrote   bool
	statusC chan int
	reader  *io.PipeReader
	writer  *io.PipeWriter
}

func newStreamResponseWriter() *streamResponseWriter {
	pr, pw := io.Pipe()
	return &streamResponseWriter{
		header:  make(http.Header),
		statusC: make(chan int, 1),
		reader:  pr,
		writer:  pw,
	}
}

func (w *streamResponseWriter) Header() http.Header {
	return w.header
}

func (w *streamResponseWriter) WriteHeader(code int) {
	w.mu.Lock()
	if !w.wrote {
		w.wrote = true
		w.status = code
		w.statusC <- code
	}
	w.mu.Unlock()
}

func (w *streamResponseWriter) Write(p []byte) (int, error) {
	w.WriteHeader(http.StatusOK)
	return w.writer.Write(p)
}

func (w *streamResponseWriter) Flush() {
	w.WriteHeader(http.StatusOK)
}

func (w *streamResponseWriter) finish() {
	w.WriteHeader(http.StatusOK)
	_ = w.writer.Close()
}

func (w *streamResponseWriter) awaitStatus(ctx context.Context) (int, error) {
	select {
	case code := <-w.statusC:
		return code, nil
	case <-ctx.Done():
		return 0, ctx.Err()
	}
}
