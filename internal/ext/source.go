package ext

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/ironpark/gvx-desktop/internal/rungv"
	"github.com/ironpark/gvx-desktop/internal/uiblock"
)

var commitSHARe = regexp.MustCompile(`^[0-9a-fA-F]{40}$`)

// LoadSource reads a persisted source.json file.
func LoadSource(path string) (SourceSpec, error) {
	return LoadSourceWithin(filepath.Dir(path), path)
}

// LoadSourceWithin reads a persisted source.json file after verifying no symlinks under root.
func LoadSourceWithin(root, path string) (SourceSpec, error) {
	b, err := ReadFileNoSymlinkWithin(root, path)
	if err != nil {
		return SourceSpec{}, err
	}
	var spec SourceSpec
	if err := json.Unmarshal(b, &spec); err != nil {
		return SourceSpec{}, fmt.Errorf("parse %s: %w", filepath.Base(path), err)
	}
	return spec, nil
}

// SaveSource writes spec atomically to path with 0600 permissions.
func SaveSource(path string, spec SourceSpec) error {
	return SaveSourceWithin(filepath.Dir(path), path, spec)
}

// SaveSourceWithin writes spec atomically to path under root with 0600 permissions.
func SaveSourceWithin(root, path string, spec SourceSpec) error {
	b, err := json.MarshalIndent(spec, "", "  ")
	if err != nil {
		return err
	}
	return WriteFileAtomicWithin(root, path, append(b, '\n'), 0o600)
}

// ParseGitSource parses a Git extension source string into a canonical HTTPS
// clone URL and optional ref (branch, tag, or commit SHA).
// Accepted forms:
//   - "owner/repo" or "owner/repo@ref" (expands to https://github.com/owner/repo)
//   - "https://host/path" or "https://host/path@ref"
//
// Rejects http://, file://, ssh://, git://, ext::, leading '-', and invalid refs.
func ParseGitSource(raw string) (repoURL string, ref string, err error) {
	s := strings.TrimSpace(raw)
	if s == "" {
		return "", "", errors.New("git source is empty")
	}
	if !utf8.ValidString(s) || strings.ContainsFunc(s, hasUnsafeGitRune) {
		return "", "", fmt.Errorf("git source %q contains invalid whitespace, control, or spoofing characters", raw)
	}
	if strings.HasPrefix(s, "-") {
		return "", "", fmt.Errorf("git source %q must not start with '-'", raw)
	}

	if strings.Contains(s, "://") {
		u, err := url.Parse(s)
		if err != nil {
			return "", "", fmt.Errorf("invalid git URL %q: %w", raw, err)
		}
		if !strings.EqualFold(u.Scheme, "https") {
			return "", "", fmt.Errorf("unsupported git URL scheme %q: only https:// or owner/repo is allowed", u.Scheme)
		}
		if u.User != nil {
			return "", "", errors.New("git URL must not include embedded credentials")
		}
		if u.Host == "" || strings.HasPrefix(u.Host, "-") {
			return "", "", fmt.Errorf("invalid git URL host in %q", raw)
		}
		if u.RawQuery != "" || u.Fragment != "" {
			return "", "", fmt.Errorf("git URL %q must not contain query parameters or fragments", raw)
		}
		cleanPath := strings.TrimSuffix(u.Path, "/")
		if cleanPath == "" || cleanPath == "/" {
			return "", "", fmt.Errorf("git URL %q is missing repository path", raw)
		}
		if idx := strings.LastIndexByte(cleanPath, '@'); idx >= 0 {
			ref = cleanPath[idx+1:]
			cleanPath = cleanPath[:idx]
			if err := validateGitRef(ref); err != nil {
				return "", "", err
			}
			if cleanPath == "" || cleanPath == "/" {
				return "", "", fmt.Errorf("git URL %q is missing repository path", raw)
			}
		}
		u.Scheme = "https"
		u.Path = cleanPath
		return u.String(), ref, nil
	}

	// Reject scp-style or other schemes like git@github.com:owner/repo or ext::...
	if strings.ContainsRune(s, ':') || strings.ContainsRune(s, '\\') {
		return "", "", fmt.Errorf("unsupported git source %q: use https:// or owner/repo", raw)
	}

	slug := s
	if idx := strings.LastIndexByte(s, '@'); idx >= 0 {
		slug = s[:idx]
		ref = s[idx+1:]
		if err := validateGitRef(ref); err != nil {
			return "", "", err
		}
	}
	if !isGitHubSlug(slug) {
		return "", "", fmt.Errorf("invalid repository slug %q: expected owner/repo or https:// URL", raw)
	}
	return "https://github.com/" + slug, ref, nil
}

func hasUnsafeGitRune(r rune) bool {
	return r < 0x20 || r == 0x7f || unicode.IsSpace(r) || uiblock.Spoofing(r)
}

func isGitHubSlug(s string) bool {
	if s == "" || strings.ContainsFunc(s, hasUnsafeGitRune) || strings.ContainsAny(s, ":\\@?#") {
		return false
	}
	owner, repo, ok := strings.Cut(s, "/")
	if !ok || !validGitHubSlugPart(owner) || !validGitHubSlugPart(repo) {
		return false
	}
	return true
}

func validGitHubSlugPart(part string) bool {
	if part == "" || len(part) > 100 || part == "." || part == ".." || strings.Contains(part, "..") {
		return false
	}
	if strings.HasPrefix(part, "-") || strings.HasPrefix(part, ".") || strings.HasPrefix(part, "~") || strings.HasSuffix(part, ".") {
		return false
	}
	for i := 0; i < len(part); i++ {
		c := part[i]
		if !((c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') || c == '-' || c == '_' || c == '.') {
			return false
		}
	}
	return true
}

func validateGitRef(ref string) error {
	if ref == "" {
		return errors.New("git ref after '@' is empty")
	}
	if !utf8.ValidString(ref) || strings.ContainsFunc(ref, hasUnsafeGitRune) {
		return fmt.Errorf("invalid git ref %q", ref)
	}
	if strings.HasPrefix(ref, "-") || strings.HasPrefix(ref, "/") || strings.HasSuffix(ref, "/") || strings.HasSuffix(ref, ".") || strings.HasSuffix(ref, ".lock") {
		return fmt.Errorf("invalid git ref %q", ref)
	}
	if strings.Contains(ref, "..") || strings.Contains(ref, "@{") || strings.ContainsAny(ref, " \t\r\n\x00~^:?*#[\\") {
		return fmt.Errorf("invalid git ref %q", ref)
	}
	return nil
}

// AcquireGit shallow-clones repoURL (at optional ref) into destDir and returns
// the resolved HEAD commit SHA. It NEVER invokes a container build.
func AcquireGit(ctx context.Context, destDir, repoURL, ref string, onLog func(string)) (string, error) {
	u, err := url.Parse(repoURL)
	if err != nil || !strings.EqualFold(u.Scheme, "https") || u.Host == "" || u.User != nil {
		return "", fmt.Errorf("refusing non-https git repository %q", repoURL)
	}
	if ref != "" {
		if err := validateGitRef(ref); err != nil {
			return "", err
		}
	}
	return acquireGit(ctx, destDir, repoURL, ref, true, onLog)
}

func acquireGit(ctx context.Context, destDir, repoURL, ref string, restrictFile bool, onLog func(string)) (string, error) {
	gitBin, err := rungv.FindTool("git")
	if err != nil {
		return "", fmt.Errorf("git not found: %w", err)
	}
	if err := os.RemoveAll(destDir); err != nil {
		return "", fmt.Errorf("prepare clone destination: %w", err)
	}
	if err := EnsureDir(filepath.Dir(destDir)); err != nil {
		return "", err
	}

	cleanupOnErr := true
	defer func() {
		if cleanupOnErr {
			_ = os.RemoveAll(destDir)
		}
	}()

	runGit := func(subcmd string, args ...string) ([]byte, error) {
		cmd := rungv.ToolCommand(ctx, gitBin, args...)
		cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
		out, err := cmd.CombinedOutput()
		if onLog != nil {
			for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
				if line != "" {
					onLog(line)
				}
			}
		}
		if err != nil {
			msg := strings.TrimSpace(string(out))
			if msg != "" {
				return out, fmt.Errorf("git %s: %s: %w", subcmd, msg, err)
			}
			return out, fmt.Errorf("git %s: %w", subcmd, err)
		}
		return out, nil
	}

	if ref != "" && commitSHARe.MatchString(ref) {
		if _, err := runGit("init", "init", "--", destDir); err != nil {
			return "", err
		}
		if _, err := runGit("remote", "-C", destDir, "remote", "add", "origin", "--", repoURL); err != nil {
			return "", err
		}
		fetchArgs := []string{"-C", destDir}
		if restrictFile {
			fetchArgs = append(fetchArgs, "-c", "protocol.file.allow=never")
		}
		fetchArgs = append(fetchArgs, "fetch", "--depth", "1", "origin", ref)
		if _, err := runGit("fetch", fetchArgs...); err != nil {
			return "", err
		}
		if _, err := runGit("checkout", "-C", destDir, "checkout", "--detach", "FETCH_HEAD"); err != nil {
			return "", err
		}
	} else {
		args := []string{}
		if restrictFile {
			args = append(args, "-c", "protocol.file.allow=never")
		}
		args = append(args, "clone", "--depth", "1")
		if ref != "" {
			args = append(args, "--branch", ref)
		}
		args = append(args, "--", repoURL, destDir)
		if _, err := runGit("clone", args...); err != nil {
			return "", err
		}
	}

	revCmd := rungv.ToolCommand(ctx, gitBin, "-C", destDir, "rev-parse", "HEAD")
	revCmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
	revOut, err := revCmd.Output()
	if err != nil {
		if exit, ok := errors.AsType[*exec.ExitError](err); ok && len(exit.Stderr) > 0 {
			return "", fmt.Errorf("git rev-parse HEAD: %s", strings.TrimSpace(string(exit.Stderr)))
		}
		return "", fmt.Errorf("git rev-parse HEAD: %w", err)
	}
	sha := strings.TrimSpace(string(revOut))
	if sha == "" {
		return "", errors.New("git rev-parse HEAD returned empty SHA")
	}
	cleanupOnErr = false
	return sha, nil
}

// CheckGitUpdate queries remote refs via `git ls-remote` and reports whether
// the remote commit differs from currentCommit.
func CheckGitUpdate(ctx context.Context, repoURL, ref, currentCommit string) (remoteSHA string, hasUpdate bool, err error) {
	u, err := url.Parse(repoURL)
	if err != nil || !strings.EqualFold(u.Scheme, "https") || u.Host == "" || u.User != nil {
		return "", false, fmt.Errorf("refusing non-https git repository %q", repoURL)
	}
	if ref != "" {
		if err := validateGitRef(ref); err != nil {
			return "", false, err
		}
	}
	return checkGitUpdate(ctx, repoURL, ref, currentCommit)
}

func checkGitUpdate(ctx context.Context, repoURL, ref, currentCommit string) (string, bool, error) {
	gitBin, err := rungv.FindTool("git")
	if err != nil {
		return "", false, fmt.Errorf("git not found: %w", err)
	}

	var queryRefs []string
	if ref == "" {
		queryRefs = []string{"HEAD"}
	} else {
		queryRefs = []string{"refs/heads/" + ref, "refs/tags/" + ref + "^{}", "refs/tags/" + ref, ref}
	}
	args := []string{}
	if !strings.HasPrefix(repoURL, "file://") {
		args = append(args, "-c", "protocol.file.allow=never")
	}
	args = append(args, "ls-remote", "--", repoURL)
	args = append(args, queryRefs...)
	cmd := rungv.ToolCommand(ctx, gitBin, args...)
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
	out, err := cmd.Output()
	if err != nil {
		if exit, ok := errors.AsType[*exec.ExitError](err); ok && len(exit.Stderr) > 0 {
			return "", false, fmt.Errorf("git ls-remote: %s", strings.TrimSpace(string(exit.Stderr)))
		}
		return "", false, fmt.Errorf("git ls-remote: %w", err)
	}

	byRef := map[string]string{}
	var firstSHA string
	for _, line := range strings.Split(string(out), "\n") {
		fields := strings.Fields(line)
		if len(fields) >= 2 && fields[0] != "" {
			if firstSHA == "" {
				firstSHA = fields[0]
			}
			byRef[fields[1]] = fields[0]
		}
	}

	var sha string
	for _, q := range queryRefs {
		if v := byRef[q]; v != "" {
			sha = v
			break
		}
	}
	if sha == "" {
		sha = firstSHA
	}
	if sha == "" {
		return "", false, fmt.Errorf("ref %q not found in %s", ref, repoURL)
	}
	return sha, sha != currentCommit, nil
}

// StageGitInstall shallow-clones a Git extension source into a staging directory
// under dataDir, validates extension.toml, moves the checkout into extensions/<id>/src,
// writes the manifest snapshot and source.json, and returns without auto-building.
func StageGitInstall(ctx context.Context, dataDir, rawSource string, onLog func(string)) (*Manifest, SourceSpec, error) {
	repoURL, ref, err := ParseGitSource(rawSource)
	if err != nil {
		return nil, SourceSpec{}, err
	}
	return stageGitInstall(ctx, dataDir, repoURL, ref, false, onLog)
}

type preparedGitInstall struct {
	man           *Manifest
	manifestBytes []byte
	spec          SourceSpec
	stageDir      string
}

func (p *preparedGitInstall) cleanup(dataDir string) {
	if p != nil && p.stageDir != "" {
		_ = removeAllNoSymlinkWithin(dataDir, p.stageDir)
		p.stageDir = ""
	}
}

func prepareGitInstall(ctx context.Context, dataDir, repoURL, ref string, allowLocal bool, onLog func(string)) (*preparedGitInstall, error) {
	if err := EnsureDirWithin(dataDir, ExtensionsDir(dataDir)); err != nil {
		return nil, err
	}
	var nonce [6]byte
	_, _ = rand.Read(nonce[:])
	stageDir := filepath.Join(ExtensionsDir(dataDir), ".staging-"+hex.EncodeToString(nonce[:]))
	cleanup := true
	defer func() {
		if cleanup {
			_ = removeAllNoSymlinkWithin(dataDir, stageDir)
		}
	}()

	var sha string
	var err error
	if allowLocal {
		sha, err = acquireGit(ctx, stageDir, repoURL, ref, false, onLog)
	} else {
		sha, err = AcquireGit(ctx, stageDir, repoURL, ref, onLog)
	}
	if err != nil {
		return nil, err
	}

	m, err := ParseManifestDir(stageDir, "")
	if err != nil {
		return nil, err
	}
	manifestBytes, err := os.ReadFile(filepath.Join(stageDir, ManifestName))
	if err != nil {
		return nil, err
	}
	cleanup = false
	return &preparedGitInstall{
		man:           m,
		manifestBytes: manifestBytes,
		spec: SourceSpec{
			Kind:   SourceGit,
			Repo:   repoURL,
			Ref:    ref,
			Commit: sha,
		},
		stageDir: stageDir,
	}, nil
}

func commitGitInstall(dataDir string, p *preparedGitInstall) error {
	if p == nil || p.man == nil {
		return errors.New("nil prepared git install")
	}
	extDir := ExtensionDir(dataDir, p.man.ID)
	if err := EnsureDirWithin(dataDir, extDir); err != nil {
		return err
	}
	srcDir := SourceCheckoutDir(dataDir, p.man.ID)
	if err := removeAllNoSymlinkWithin(dataDir, srcDir); err != nil {
		return err
	}
	if err := os.Rename(p.stageDir, srcDir); err != nil {
		return fmt.Errorf("move checkout into %s: %w", srcDir, err)
	}
	p.stageDir = ""
	if err := WriteFileAtomicWithin(dataDir, ManifestSnapshotPath(dataDir, p.man.ID), p.manifestBytes, 0o600); err != nil {
		return err
	}
	return SaveSourceWithin(dataDir, SourcePath(dataDir, p.man.ID), p.spec)
}

func stageGitInstall(ctx context.Context, dataDir, repoURL, ref string, allowLocal bool, onLog func(string)) (*Manifest, SourceSpec, error) {
	prep, err := prepareGitInstall(ctx, dataDir, repoURL, ref, allowLocal, onLog)
	if err != nil {
		return nil, SourceSpec{}, err
	}
	defer prep.cleanup(dataDir)
	if err := commitGitInstall(dataDir, prep); err != nil {
		return nil, SourceSpec{}, err
	}
	return prep.man, prep.spec, nil
}

// AcquireLocal validates a local extension directory without creating a src/ checkout
// and without triggering any build.
func AcquireLocal(localDir string) (absDir string, m *Manifest, err error) {
	trimmed := strings.TrimSpace(localDir)
	if trimmed == "" {
		return "", nil, errors.New("local extension path is empty")
	}
	absDir, err = filepath.Abs(trimmed)
	if err != nil {
		return "", nil, fmt.Errorf("resolve local path %q: %w", localDir, err)
	}
	info, err := os.Stat(absDir)
	if err != nil {
		return "", nil, fmt.Errorf("inspect local path %q: %w", localDir, err)
	}
	if !info.IsDir() {
		return "", nil, fmt.Errorf("local path %q is not a directory", localDir)
	}
	realDir, err := filepath.EvalSymlinks(absDir)
	if err != nil {
		return "", nil, fmt.Errorf("resolve local path %q: %w", localDir, err)
	}
	m, err = ParseManifestDir(realDir, "")
	if err != nil {
		return "", nil, err
	}
	return realDir, m, nil
}

type preparedLocalInstall struct {
	man           *Manifest
	manifestBytes []byte
	spec          SourceSpec
}

func prepareLocalInstall(localDir string) (preparedLocalInstall, error) {
	absDir, m, err := AcquireLocal(localDir)
	if err != nil {
		return preparedLocalInstall{}, err
	}
	manifestBytes, err := os.ReadFile(filepath.Join(absDir, ManifestName))
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return preparedLocalInstall{}, err
	}
	if err != nil {
		return preparedLocalInstall{}, err
	}
	return preparedLocalInstall{
		man:           m,
		manifestBytes: manifestBytes,
		spec: SourceSpec{
			Kind:      SourceLocal,
			LocalPath: absDir,
		},
	}, nil
}

func commitLocalInstall(dataDir string, p preparedLocalInstall) error {
	if err := WriteFileAtomicWithin(dataDir, ManifestSnapshotPath(dataDir, p.man.ID), p.manifestBytes, 0o600); err != nil {
		return err
	}
	return SaveSourceWithin(dataDir, SourcePath(dataDir, p.man.ID), p.spec)
}

// StageLocalInstall validates a local extension directory and persists its manifest
// snapshot and source.json under dataDir (without creating src/ or auto-building).
func StageLocalInstall(dataDir, localDir string) (*Manifest, SourceSpec, error) {
	prep, err := prepareLocalInstall(localDir)
	if err != nil {
		return nil, SourceSpec{}, err
	}
	if err := commitLocalInstall(dataDir, prep); err != nil {
		return nil, SourceSpec{}, err
	}
	return prep.man, prep.spec, nil
}

// AcquireImage validates imageRef and pulls it using puller (invoked only after user approval).
func AcquireImage(ctx context.Context, puller ImagePuller, imageRef string, onLog func(string)) error {
	if err := ValidateImageRef(imageRef); err != nil {
		return err
	}
	if puller == nil {
		return errors.New("image puller is not configured")
	}
	return puller.Pull(ctx, imageRef, onLog)
}

// ValidateImageRef checks that an OCI image reference is non-empty and free of
// option-injection prefixes or control/whitespace bytes.
func ValidateImageRef(ref string) error {
	if ref == "" || strings.HasPrefix(ref, "-") {
		return fmt.Errorf("invalid image reference %q", ref)
	}
	if !utf8.ValidString(ref) {
		return fmt.Errorf("invalid image reference %q", ref)
	}
	for i := 0; i < len(ref); i++ {
		if ref[i] <= 0x20 || ref[i] == 0x7f {
			return fmt.Errorf("invalid image reference %q", ref)
		}
	}
	return nil
}
