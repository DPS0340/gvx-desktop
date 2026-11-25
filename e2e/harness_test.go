//go:build e2e

package e2e

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/http"
	"net/http/cgi"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ironpark/gvx-desktop/internal/ext"
	"github.com/ironpark/gvx-desktop/internal/keychain"
	"github.com/ironpark/gvx-desktop/internal/rungv"
	"github.com/ironpark/gvx-desktop/internal/sandbox"
	"github.com/ironpark/gvx-desktop/internal/store"
)

// --- Repo & Toolchain Helpers ---

// repoRoot walks upward from the current working directory until go.mod for
// github.com/ironpark/gvx-desktop is found.
func repoRoot(t *testing.T) string {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("os.Getwd: %v", err)
	}
	dir := wd
	for {
		modPath := filepath.Join(dir, "go.mod")
		if b, err := os.ReadFile(modPath); err == nil {
			if strings.Contains(string(b), "module github.com/ironpark/gvx-desktop") {
				return dir
			}
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatalf("could not locate repository root from %s", wd)
		}
		dir = parent
	}
}

// runGo runs `go <args...>` in dir with CGO_ENABLED=0 and optional extra
// environment variables, returning combined stdout/stderr.
func runGo(t *testing.T, dir string, extraEnv []string, args ...string) (string, error) {
	t.Helper()
	cmd := exec.Command("go", args...)
	cmd.Dir = dir
	env := append(os.Environ(), "CGO_ENABLED=0")
	env = append(env, extraEnv...)
	cmd.Env = env
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// requireRungv probes rungv.Locate("") and verifies runtime capabilities.
// It skips the test with an explicit diagnostic reason only when rungv or its
// hypervisor/Unix-forward features are unavailable in the host environment.
func requireRungv(t *testing.T) (binPath string, info rungv.Info, feat rungv.Features) {
	t.Helper()
	binPath, err := rungv.Locate("")
	if err != nil {
		t.Skipf("rungv unavailable: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	client := &rungv.Client{Path: binPath}
	info, err = client.Info(ctx)
	if err != nil {
		t.Skipf("rungv info failed (%s): %v", binPath, err)
	}
	if len(info.AvailableBackends) == 0 {
		t.Skipf("rungv at %s reports no available backends", binPath)
	}
	feat = client.Features(ctx)
	if !feat.UnixForward {
		t.Skipf("rungv at %s lacks --unix-forward support", binPath)
	}
	return binPath, info, feat
}

// --- Isolation, Storage & Permission Helpers ---

// registerNoDotGVXCleanup snapshots whether ~/.gvx existed before the test and
// verifies at cleanup that it was not created if it was absent before.
func registerNoDotGVXCleanup(t *testing.T) {
	t.Helper()
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		gvxPath := filepath.Join(home, ".gvx")
		_, beforeErr := os.Stat(gvxPath)
		beforeMissing := errors.Is(beforeErr, os.ErrNotExist)
		t.Cleanup(func() {
			if beforeMissing {
				assertNoDotGVX(t, home)
			}
		})
	}
}

// newTempDataDir creates an isolated temporary data directory, forces
// GVX_KEYCHAIN=file for defense-in-depth, and verifies ~/.gvx is untouched.
func newTempDataDir(t *testing.T) string {
	t.Helper()
	t.Setenv(keychain.EnvVar, "file")
	dir := t.TempDir()
	if resolved, err := filepath.EvalSymlinks(dir); err == nil {
		dir = resolved
	}
	registerNoDotGVXCleanup(t)
	return dir
}

// newShortTempDataDir creates a short temporary directory suitable for Unix
// domain socket tests that must stay below sun_path limits.
func newShortTempDataDir(t *testing.T) string {
	t.Helper()
	t.Setenv(keychain.EnvVar, "file")
	dir, err := os.MkdirTemp("", "gvx-e2e-*")
	if err != nil {
		t.Fatalf("os.MkdirTemp: %v", err)
	}
	if resolved, err := filepath.EvalSymlinks(dir); err == nil {
		dir = resolved
	}
	if runtime.GOOS != "windows" && len(dir) > 70 {
		if shortDir, err := os.MkdirTemp("/tmp", "gvx-e2e-*"); err == nil {
			_ = os.RemoveAll(dir)
			if resolved, err := filepath.EvalSymlinks(shortDir); err == nil {
				shortDir = resolved
			}
			dir = shortDir
		}
	}
	t.Cleanup(func() {
		_ = os.RemoveAll(dir)
	})
	registerNoDotGVXCleanup(t)
	return dir
}

// newFakeKeychain returns an in-memory *keychain.Fake and enforces
// GVX_KEYCHAIN=file so tests never reach the OS keychain.
func newFakeKeychain(t *testing.T) *keychain.Fake {
	t.Helper()
	t.Setenv(keychain.EnvVar, "file")
	return &keychain.Fake{}
}

// newTestStore returns a store.Store backed by dataDir.
func newTestStore(t *testing.T, dataDir string) store.Store {
	t.Helper()
	return store.Store{Dir: dataDir}
}

// assertNoDotGVX asserts that <home>/.gvx does not exist.
func assertNoDotGVX(t *testing.T, home string) {
	t.Helper()
	dotGVX := filepath.Join(home, ".gvx")
	if _, err := os.Stat(dotGVX); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("unexpected .gvx directory/file at %s (err=%v)", dotGVX, err)
	}
}

// assertDirPerm verifies that path exists, is a directory, and has permission
// mode want on Unix platforms.
func assertDirPerm(t *testing.T, path string, want fs.FileMode) {
	t.Helper()
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatalf("os.Stat(%s): %v", path, err)
	}
	if !fi.IsDir() {
		t.Fatalf("expected %s to be a directory", path)
	}
	if runtime.GOOS != "windows" && fi.Mode().Perm() != want {
		t.Fatalf("dir %s mode = %04o, want %04o", path, fi.Mode().Perm(), want)
	}
}

// assertFilePerm verifies that path exists, is a regular file, and has
// permission mode want on Unix platforms.
func assertFilePerm(t *testing.T, path string, want fs.FileMode) {
	t.Helper()
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatalf("os.Stat(%s): %v", path, err)
	}
	if fi.IsDir() {
		t.Fatalf("expected %s to be a file, got directory", path)
	}
	if runtime.GOOS != "windows" && fi.Mode().Perm() != want {
		t.Fatalf("file %s mode = %04o, want %04o", path, fi.Mode().Perm(), want)
	}
}

// --- Manifest & Fixture Helpers ---

// writeExtManifest writes extension.toml inside extDir with 0600 permissions.
func writeExtManifest(t *testing.T, extDir, tomlBody string) string {
	t.Helper()
	if err := os.MkdirAll(extDir, 0o700); err != nil {
		t.Fatalf("os.MkdirAll(%s): %v", extDir, err)
	}
	p := filepath.Join(extDir, "extension.toml")
	if err := os.WriteFile(p, []byte(tomlBody), 0o600); err != nil {
		t.Fatalf("os.WriteFile(%s): %v", p, err)
	}
	return p
}

// writeExtFixture writes extension.toml and arbitrary relative files into dir.
func writeExtFixture(t *testing.T, dir, manifestTOML string, files map[string]string) string {
	t.Helper()
	if manifestTOML != "" {
		writeExtManifest(t, dir, manifestTOML)
	} else if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("os.MkdirAll(%s): %v", dir, err)
	}
	for rel, content := range files {
		full := filepath.Join(dir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(full), 0o700); err != nil {
			t.Fatalf("os.MkdirAll(%s): %v", filepath.Dir(full), err)
		}
		mode := fs.FileMode(0o644)
		if strings.HasPrefix(rel, "bin/") || strings.HasSuffix(rel, ".sh") {
			mode = 0o755
		}
		if err := os.WriteFile(full, []byte(content), mode); err != nil {
			t.Fatalf("os.WriteFile(%s): %v", full, err)
		}
	}
	return dir
}

// minimalManifestTOML builds a valid schema_version=1 sandbox extension.toml.
func minimalManifestTOML(id, version string, perms ...string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "schema_version = 1\n")
	fmt.Fprintf(&b, "id = %q\n", id)
	fmt.Fprintf(&b, "version = %q\n", version)
	fmt.Fprintf(&b, "publisher = \"acme\"\n")
	fmt.Fprintf(&b, "name = \"Test Extension %s\"\n", id)
	fmt.Fprintf(&b, "description = \"E2E test extension\"\n")
	fmt.Fprintf(&b, "icon = \"box\"\n")
	if len(perms) > 0 {
		quoted := make([]string, len(perms))
		for i, p := range perms {
			quoted[i] = fmt.Sprintf("%q", p)
		}
		fmt.Fprintf(&b, "permissions = [%s]\n", strings.Join(quoted, ", "))
	}
	fmt.Fprintf(&b, "\n[runtime]\n")
	fmt.Fprintf(&b, "type = \"sandbox\"\n")
	fmt.Fprintf(&b, "dockerfile = \"Dockerfile\"\n")
	fmt.Fprintf(&b, "context = \".\"\n")
	fmt.Fprintf(&b, "port = 18900\n")
	fmt.Fprintf(&b, "\n[[contributes.view]]\n")
	fmt.Fprintf(&b, "id = \"main\"\n")
	fmt.Fprintf(&b, "title = \"Main\"\n")
	fmt.Fprintf(&b, "icon = \"box\"\n")
	return b.String()
}

// binaryManifestTOML builds a valid Developer Mode binary runtime extension.toml.
func binaryManifestTOML(id, version, entrypoint string, perms ...string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "schema_version = 1\n")
	fmt.Fprintf(&b, "id = %q\n", id)
	fmt.Fprintf(&b, "version = %q\n", version)
	fmt.Fprintf(&b, "publisher = \"acme\"\n")
	fmt.Fprintf(&b, "name = \"Binary Extension %s\"\n", id)
	fmt.Fprintf(&b, "description = \"Developer mode E2E extension\"\n")
	fmt.Fprintf(&b, "icon = \"terminal\"\n")
	if len(perms) > 0 {
		quoted := make([]string, len(perms))
		for i, p := range perms {
			quoted[i] = fmt.Sprintf("%q", p)
		}
		fmt.Fprintf(&b, "permissions = [%s]\n", strings.Join(quoted, ", "))
	}
	fmt.Fprintf(&b, "\n[runtime]\n")
	fmt.Fprintf(&b, "type = \"binary\"\n")
	fmt.Fprintf(&b, "dev_entrypoint = %q\n", entrypoint)
	fmt.Fprintf(&b, "\n[[contributes.view]]\n")
	fmt.Fprintf(&b, "id = \"main\"\n")
	fmt.Fprintf(&b, "title = \"Main\"\n")
	fmt.Fprintf(&b, "icon = \"terminal\"\n")
	return b.String()
}

// fullManifestTOML builds a valid manifest exercising all runtime fields,
// view/command/tool/settings (all 5 types), and web_ui contributions.
func fullManifestTOML(id string) string {
	return fmt.Sprintf(`schema_version = 1
id = %q
version = "1.2.3-rc.1"
publisher = "acme"
name = "Full Fixture"
description = "Full-featured E2E fixture extension"
icon = "workflow"
permissions = ["storage", "secrets", "docker", "localproxy", "network"]

[runtime]
type = "sandbox"
dockerfile = "docker/Dockerfile"
context = "."
target = "runner"
image = "ghcr.io/acme/%s:1.2.3-rc.1"
port = 18950
memory = "512m"
cpus = "1"
idle_timeout_sec = 60
dev_entrypoint = "bin/ext"

[[contributes.view]]
id = "main"
title = "Overview"
icon = "workflow"
chrome = "toolbar"

[[contributes.view]]
id = "logs"
title = "Logs"
icon = "terminal"
chrome = "auto"

[[contributes.command]]
id = "refresh"
title = "Fixture: Refresh"

[[contributes.tool]]
id = "inspect_items"
description = "Inspect items managed by the fixture."
[contributes.tool.params]
filter = { type = "string", description = "Filter query", required = false }
limit = { type = "number", description = "Maximum results", required = false }
verbose = { type = "boolean", description = "Include details", required = false }

[contributes.settings]
api_url = { type = "string", label = "API URL", description = "Endpoint URL", default = "https://example.invalid" }
api_token = { type = "secret", label = "API Token", description = "Secret credential" }
enabled = { type = "boolean", label = "Enabled", default = true }
concurrency = { type = "number", label = "Concurrency", default = 2 }
mode = { type = "select", label = "Mode", options = ["fast", "safe"], default = "safe" }

[contributes.web_ui]
port = 8080
path = "/ui"
`, id, id)
}

// createLocalExtensionDir creates <root>/<id> containing extension.toml, a
// default Dockerfile (unless provided in extraFiles), and extraFiles.
func createLocalExtensionDir(t *testing.T, root, id, manifestTOML string, extraFiles map[string]string) string {
	t.Helper()
	extDir := filepath.Join(root, id)
	merged := map[string]string{
		"Dockerfile": "FROM alpine:3.20\nCMD [\"/bin/sh\"]\n",
	}
	for k, v := range extraFiles {
		merged[k] = v
	}
	return writeExtFixture(t, extDir, manifestTOML, merged)
}

// initBareGitRepo initializes a local Git repository with files, commits them,
// clones it as a bare repository, and returns (file:// URL, HEAD commit SHA).
func initBareGitRepo(t *testing.T, files map[string]string) (bareURL string, headSHA string) {
	t.Helper()
	base := t.TempDir()
	workDir := filepath.Join(base, "work")
	bareDir := filepath.Join(base, "remote.git")
	if err := os.MkdirAll(workDir, 0o755); err != nil {
		t.Fatalf("os.MkdirAll: %v", err)
	}
	runGit(t, workDir, "init", "-b", "main")
	runGit(t, workDir, "config", "user.name", "E2E Bot")
	runGit(t, workDir, "config", "user.email", "e2e@example.invalid")

	for rel, content := range files {
		full := filepath.Join(workDir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatalf("os.MkdirAll: %v", err)
		}
		if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
			t.Fatalf("os.WriteFile: %v", err)
		}
	}
	runGit(t, workDir, "add", "-A")
	runGit(t, workDir, "commit", "-m", "initial commit")
	sha := strings.TrimSpace(runGit(t, workDir, "rev-parse", "HEAD"))
	runGit(t, base, "clone", "--bare", workDir, bareDir)
	return "file://" + filepath.ToSlash(bareDir), sha
}

// serveGitHTTPS starts an httptest TLS server backing bareDir with
// git http-backend, writes the TLS CA certificate to a temp PEM file,
// configures GIT_SSL_CAINFO via t.Setenv, and returns the https:// URL.
func serveGitHTTPS(t *testing.T, bareDir string) string {
	t.Helper()
	gitBin, err := exec.LookPath("git")
	if err != nil {
		t.Fatalf("git binary not found: %v", err)
	}
	realBare := bareDir
	if resolved, err := filepath.EvalSymlinks(bareDir); err == nil {
		realBare = resolved
	}
	projectRoot := filepath.Dir(realBare)
	repoBase := filepath.Base(realBare)

	handler := &cgi.Handler{
		Path: gitBin,
		Args: []string{"http-backend"},
		Dir:  projectRoot,
		Env: []string{
			"GIT_PROJECT_ROOT=" + projectRoot,
			"GIT_HTTP_EXPORT_ALL=1",
		},
	}
	srv := httptest.NewTLSServer(handler)
	t.Cleanup(srv.Close)

	cert := srv.Certificate()
	if cert == nil {
		t.Fatal("httptest TLS server has nil certificate")
	}
	caPEM := pem.EncodeToMemory(&pem.Block{
		Type:  "CERTIFICATE",
		Bytes: cert.Raw,
	})
	caFile := filepath.Join(t.TempDir(), "git-ca.pem")
	if err := os.WriteFile(caFile, caPEM, 0o600); err != nil {
		t.Fatalf("write CA pem: %v", err)
	}
	t.Setenv("GIT_SSL_CAINFO", caFile)
	return srv.URL + "/" + repoBase
}

// commitToBareRepo updates files in workDir (cloning bareURL first if workDir is
// empty), commits, and pushes to bareURL, returning the new HEAD SHA.
func commitToBareRepo(t *testing.T, bareURL, workDir string, files map[string]string) string {
	t.Helper()
	if _, err := os.Stat(filepath.Join(workDir, ".git")); errors.Is(err, os.ErrNotExist) {
		runGit(t, filepath.Dir(workDir), "clone", bareURL, workDir)
		runGit(t, workDir, "config", "user.name", "E2E Bot")
		runGit(t, workDir, "config", "user.email", "e2e@example.invalid")
	}
	for rel, content := range files {
		full := filepath.Join(workDir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatalf("os.MkdirAll: %v", err)
		}
		if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
			t.Fatalf("os.WriteFile: %v", err)
		}
	}
	runGit(t, workDir, "add", "-A")
	runGit(t, workDir, "commit", "--allow-empty", "-m", "update commit")
	runGit(t, workDir, "push", "origin", "HEAD")
	return strings.TrimSpace(runGit(t, workDir, "rev-parse", "HEAD"))
}

func runGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s in %s failed: %v\n%s", strings.Join(args, " "), dir, err, string(out))
	}
	return string(out)
}

// buildFixtureSDKBinary compiles a Go source program using pkg/extsdk from this
// repository into an executable in t.TempDir() and returns the binary path.
func buildFixtureSDKBinary(t *testing.T, sourceGoCode string) string {
	t.Helper()
	root := repoRoot(t)
	buildDir := t.TempDir()
	goMod := fmt.Sprintf("module example.com/e2efixture\n\ngo 1.27.0\n\nrequire github.com/ironpark/gvx-desktop v0.0.0\n\nreplace github.com/ironpark/gvx-desktop => %s\n", root)
	if err := os.WriteFile(filepath.Join(buildDir, "go.mod"), []byte(goMod), 0o644); err != nil {
		t.Fatalf("write go.mod: %v", err)
	}
	if err := os.WriteFile(filepath.Join(buildDir, "main.go"), []byte(sourceGoCode), 0o644); err != nil {
		t.Fatalf("write main.go: %v", err)
	}
	binName := "ext-fixture"
	if runtime.GOOS == "windows" {
		binName += ".exe"
	}
	binPath := filepath.Join(buildDir, binName)
	if out, err := runGo(t, buildDir, nil, "build", "-o", binPath, "."); err != nil {
		t.Fatalf("buildFixtureSDKBinary failed: %v\n%s", err, out)
	}
	return binPath
}

// --- Mock Doubles for ext.Config & Manager Factory ---

// TerminalCall records a UIHost.OpenTerminal invocation.
type TerminalCall struct {
	TargetType string
	TargetID   string
	Shell      string
}

// ToastCall records a UIHost.ShowToast invocation.
type ToastCall struct {
	Level   string
	Message string
}

// MockUIHost is a thread-safe test double for ext.UIHost callbacks.
type MockUIHost struct {
	mu sync.Mutex

	PickFolderResult    string
	PickFolderCancelled bool
	PickFolderErr       error
	PickFolderCalls     int

	Terminals []TerminalCall
	OpenURLs  []string
	Copied    []string
	Toasts    []ToastCall
	Changes   int

	changeCh chan struct{}
	notifyCh chan struct{}
}

// NewMockUIHost creates an initialized MockUIHost.
func NewMockUIHost() *MockUIHost {
	return &MockUIHost{
		changeCh: make(chan struct{}, 64),
	}
}

func (m *MockUIHost) PickFolder(ctx context.Context, extID string) (string, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.PickFolderCalls++
	if m.PickFolderErr != nil {
		return "", false, m.PickFolderErr
	}
	if m.PickFolderCancelled {
		return "", false, nil
	}
	return m.PickFolderResult, m.PickFolderResult != "", nil
}

func (m *MockUIHost) OpenTerminal(targetType, targetID, shell string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.Terminals = append(m.Terminals, TerminalCall{
		TargetType: targetType,
		TargetID:   targetID,
		Shell:      shell,
	})
	return nil
}

func (m *MockUIHost) OpenURL(rawURL string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.OpenURLs = append(m.OpenURLs, rawURL)
	return nil
}

func (m *MockUIHost) CopyText(text string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.Copied = append(m.Copied, text)
	return nil
}

func (m *MockUIHost) ShowToast(level, message string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.Toasts = append(m.Toasts, ToastCall{Level: level, Message: message})
}

func (m *MockUIHost) Changed() {
	m.mu.Lock()
	m.Changes++
	if m.notifyCh != nil {
		close(m.notifyCh)
		m.notifyCh = nil
	}
	m.mu.Unlock()
	select {
	case m.changeCh <- struct{}{}:
	default:
	}
}

// SnapshotPickFolderCalls returns the recorded PickFolder call count safely.
func (m *MockUIHost) SnapshotPickFolderCalls() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.PickFolderCalls
}

// SnapshotTerminals returns a copy of recorded OpenTerminal calls safely.
func (m *MockUIHost) SnapshotTerminals() []TerminalCall {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]TerminalCall(nil), m.Terminals...)
}

// SnapshotOpenURLs returns a copy of recorded OpenURL calls safely.
func (m *MockUIHost) SnapshotOpenURLs() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]string(nil), m.OpenURLs...)
}

// SnapshotCopied returns a copy of recorded CopyText calls safely.
func (m *MockUIHost) SnapshotCopied() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]string(nil), m.Copied...)
}

// SnapshotToasts returns a copy of recorded ShowToast calls safely.
func (m *MockUIHost) SnapshotToasts() []ToastCall {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]ToastCall(nil), m.Toasts...)
}

// SnapshotChanges returns the current Changed() counter safely.
func (m *MockUIHost) SnapshotChanges() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.Changes
}

// DrainChanged drains buffered tokens from changeCh and returns the current
// Changed() count so callers can subsequently pass it to WaitChangedAfter.
func (m *MockUIHost) DrainChanged() int {
	for {
		select {
		case <-m.changeCh:
		default:
			return m.SnapshotChanges()
		}
	}
}

// WaitChanged waits up to timeout for at least one Changed() notification.
func (m *MockUIHost) WaitChanged(t *testing.T, timeout time.Duration) {
	t.Helper()
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case <-m.changeCh:
	case <-timer.C:
		t.Fatalf("timed out after %v waiting for MockUIHost.Changed", timeout)
	}
}

// WaitChangedAfter waits up to timeout for m.Changes to exceed since, returning
// the new count.
func (m *MockUIHost) WaitChangedAfter(t *testing.T, since int, timeout time.Duration) int {
	t.Helper()
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	for {
		m.mu.Lock()
		if m.Changes > since {
			cur := m.Changes
			m.mu.Unlock()
			return cur
		}
		if m.notifyCh == nil {
			m.notifyCh = make(chan struct{})
		}
		ch := m.notifyCh
		m.mu.Unlock()
		select {
		case <-ch:
		case <-timer.C:
			t.Fatalf("timed out after %v waiting for MockUIHost.Changed > %d", timeout, since)
			return 0
		}
	}
}

// BuildCall records one invocation of MockImageBuilder.
type BuildCall struct {
	Request    ext.BuildRequest
	Tag        string
	Dockerfile string
	Target     string
	Platform   string
	ContextTar []byte
}

// MockImageBuilder records BuildKit build requests and tar contexts.
type MockImageBuilder struct {
	mu       sync.Mutex
	Calls    []BuildCall
	LogLines []string
	ImageID  string
	BuildErr error
}

func (b *MockImageBuilder) Build(ctx context.Context, req ext.BuildRequest, logf func(string)) (string, error) {
	var buf bytes.Buffer
	if req.ContextDir != "" {
		if err := ext.PackBuildContext(ctx, req.ContextDir, req.Dockerfile, &buf); err != nil {
			return "", err
		}
	}
	tag := ""
	if len(req.Tags) > 0 {
		tag = req.Tags[0]
	}
	b.mu.Lock()
	b.Calls = append(b.Calls, BuildCall{
		Request:    req,
		Tag:        tag,
		Dockerfile: req.Dockerfile,
		Target:     req.Target,
		Platform:   req.Platform,
		ContextTar: buf.Bytes(),
	})
	lines := append([]string(nil), b.LogLines...)
	imgID := b.ImageID
	if imgID == "" {
		imgID = "sha256:mockbuiltimage"
	}
	buildErr := b.BuildErr
	b.mu.Unlock()
	for _, line := range lines {
		if logf != nil {
			logf(line)
		}
	}
	if buildErr != nil {
		return "", buildErr
	}
	return imgID, nil
}

// SnapshotCalls returns a deep copy of recorded Build calls safely.
func (b *MockImageBuilder) SnapshotCalls() []BuildCall {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := make([]BuildCall, len(b.Calls))
	for i, c := range b.Calls {
		c.ContextTar = append([]byte(nil), c.ContextTar...)
		out[i] = c
	}
	return out
}

// MockImagePuller records prebuilt OCI image pull requests.
type MockImagePuller struct {
	mu         sync.Mutex
	PulledRefs []string
	LogLines   []string
	PullErr    error
}

func (p *MockImagePuller) Pull(ctx context.Context, ref string, logf func(string)) error {
	p.mu.Lock()
	p.PulledRefs = append(p.PulledRefs, ref)
	lines := append([]string(nil), p.LogLines...)
	pullErr := p.PullErr
	p.mu.Unlock()
	for _, line := range lines {
		if logf != nil {
			logf(line)
		}
	}
	return pullErr
}

// SnapshotPulledRefs returns a copy of recorded Pull refs safely.
func (p *MockImagePuller) SnapshotPulledRefs() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.PulledRefs...)
}

// DockerRequest records one HTTP request received by MockDockerBackend.
type DockerRequest struct {
	Method        string
	Path          string
	Header        http.Header
	RawBody       []byte
	CreatePayload map[string]any
}

type mockDockerNetwork struct {
	ID     string            `json:"Id"`
	Name   string            `json:"Name"`
	Driver string            `json:"Driver"`
	Labels map[string]string `json:"Labels"`
}

// MockDockerBackend runs a local httptest.Server mimicking Docker Engine API
// responses and recording every request for opaque-box proxy inspection.
type MockDockerBackend struct {
	Server *httptest.Server

	mu          sync.Mutex
	Requests    []DockerRequest
	Unavailable bool
	DialErr     error
	containerN  int
	execN       int
	networkN    int
	containers  map[string]map[string]string
	execs       map[string]string
	networks    map[string]*mockDockerNetwork
}

// SeedContainer pre-registers a container with io.gvx.extension=extID in MockDockerBackend.
func (b *MockDockerBackend) SeedContainer(id, extID string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.containers == nil {
		b.containers = make(map[string]map[string]string)
	}
	labels := map[string]string{}
	if extID != "" {
		labels["io.gvx.extension"] = extID
	}
	b.containers[id] = labels
}

// SeedNetwork pre-registers a network with io.gvx.extension=extID in MockDockerBackend.
func (b *MockDockerBackend) SeedNetwork(id, name, extID string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.networks == nil {
		b.networks = make(map[string]*mockDockerNetwork)
	}
	labels := map[string]string{}
	if extID != "" {
		labels["io.gvx.extension"] = extID
	}
	b.networks[id] = &mockDockerNetwork{
		ID:     id,
		Name:   name,
		Driver: "bridge",
		Labels: labels,
	}
}

func (b *MockDockerBackend) findNetworkLocked(ref string) *mockDockerNetwork {
	if n, ok := b.networks[ref]; ok {
		return n
	}
	for _, n := range b.networks {
		if n.Name == ref {
			return n
		}
	}
	return nil
}

// NewMockDockerBackend starts a MockDockerBackend server and registers Cleanup.
func NewMockDockerBackend(t *testing.T) *MockDockerBackend {
	t.Helper()
	b := &MockDockerBackend{
		containers: make(map[string]map[string]string),
		execs:      make(map[string]string),
		networks: map[string]*mockDockerNetwork{
			"bridge-id": {ID: "bridge-id", Name: "bridge", Driver: "bridge", Labels: map[string]string{}},
			"host-id":   {ID: "host-id", Name: "host", Driver: "host", Labels: map[string]string{}},
			"none-id":   {ID: "none-id", Name: "none", Driver: "null", Labels: map[string]string{}},
		},
	}
	b.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		rec := DockerRequest{
			Method:  r.Method,
			Path:    r.URL.Path,
			Header:  r.Header.Clone(),
			RawBody: raw,
		}
		if len(raw) > 0 && strings.Contains(r.URL.Path, "/containers/create") {
			var parsed map[string]any
			if err := json.Unmarshal(raw, &parsed); err == nil {
				rec.CreatePayload = parsed
			}
		}

		b.mu.Lock()
		b.Requests = append(b.Requests, rec)
		pathClean := r.URL.Path
		if strings.HasPrefix(pathClean, "/v") {
			if idx := strings.IndexByte(pathClean[2:], '/'); idx > 0 {
				pathClean = pathClean[2+idx:]
			}
		}

		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && pathClean == "/_ping":
			b.mu.Unlock()
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte("OK"))

		case r.Method == http.MethodPost && pathClean == "/containers/create":
			b.containerN++
			id := fmt.Sprintf("mock-container-%d", b.containerN)
			labels := map[string]string{}
			if rawLabels, ok := rec.CreatePayload["Labels"].(map[string]any); ok {
				for k, v := range rawLabels {
					if vs, ok := v.(string); ok {
						labels[k] = vs
					}
				}
			}
			b.containers[id] = labels
			b.mu.Unlock()
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(map[string]any{"Id": id, "Warnings": []string{}})

		case r.Method == http.MethodGet && pathClean == "/containers/json":
			items := make([]map[string]any, 0, len(b.containers))
			for id, labels := range b.containers {
				items = append(items, map[string]any{
					"Id":     id,
					"Labels": labels,
				})
			}
			b.mu.Unlock()
			w.WriteHeader(http.StatusOK)
			_ = json.NewEncoder(w).Encode(items)

		case r.Method == http.MethodGet && strings.HasPrefix(pathClean, "/containers/") && strings.HasSuffix(pathClean, "/json"):
			cid := strings.TrimSuffix(strings.TrimPrefix(pathClean, "/containers/"), "/json")
			labels, ok := b.containers[cid]
			b.mu.Unlock()
			if !ok {
				w.WriteHeader(http.StatusNotFound)
				_ = json.NewEncoder(w).Encode(map[string]string{"message": "No such container: " + cid})
				return
			}
			w.WriteHeader(http.StatusOK)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"Id":     cid,
				"Config": map[string]any{"Labels": labels},
			})

		case r.Method == http.MethodPost && strings.HasPrefix(pathClean, "/containers/") && strings.HasSuffix(pathClean, "/exec"):
			cid := strings.TrimSuffix(strings.TrimPrefix(pathClean, "/containers/"), "/exec")
			_, ok := b.containers[cid]
			if !ok {
				b.mu.Unlock()
				w.WriteHeader(http.StatusNotFound)
				_ = json.NewEncoder(w).Encode(map[string]string{"message": "No such container: " + cid})
				return
			}
			b.execN++
			eid := fmt.Sprintf("mock-exec-%d", b.execN)
			b.execs[eid] = cid
			b.mu.Unlock()
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(map[string]string{"Id": eid})

		case r.Method == http.MethodGet && strings.HasPrefix(pathClean, "/exec/") && strings.HasSuffix(pathClean, "/json"):
			eid := strings.TrimSuffix(strings.TrimPrefix(pathClean, "/exec/"), "/json")
			cid, ok := b.execs[eid]
			b.mu.Unlock()
			if !ok {
				w.WriteHeader(http.StatusNotFound)
				_ = json.NewEncoder(w).Encode(map[string]string{"message": "No such exec instance: " + eid})
				return
			}
			w.WriteHeader(http.StatusOK)
			_ = json.NewEncoder(w).Encode(map[string]string{"ID": eid, "ContainerID": cid})

		case r.Method == http.MethodDelete && strings.HasPrefix(pathClean, "/containers/") && !strings.Contains(strings.TrimPrefix(pathClean, "/containers/"), "/"):
			cid := strings.TrimPrefix(pathClean, "/containers/")
			_, ok := b.containers[cid]
			if ok {
				delete(b.containers, cid)
			}
			b.mu.Unlock()
			if !ok {
				w.WriteHeader(http.StatusNotFound)
				_ = json.NewEncoder(w).Encode(map[string]string{"message": "No such container: " + cid})
				return
			}
			w.WriteHeader(http.StatusNoContent)

		case r.Method == http.MethodPost && pathClean == "/networks/create":
			var req struct {
				Name   string            `json:"Name"`
				Driver string            `json:"Driver"`
				Labels map[string]string `json:"Labels"`
			}
			_ = json.Unmarshal(raw, &req)
			b.networkN++
			id := fmt.Sprintf("mock-net-%d", b.networkN)
			drv := req.Driver
			if drv == "" {
				drv = "bridge"
			}
			labels := map[string]string{}
			for k, v := range req.Labels {
				labels[k] = v
			}
			b.networks[id] = &mockDockerNetwork{ID: id, Name: req.Name, Driver: drv, Labels: labels}
			b.mu.Unlock()
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(map[string]string{"Id": id, "Warning": ""})

		case r.Method == http.MethodGet && pathClean == "/networks":
			items := make([]*mockDockerNetwork, 0, len(b.networks))
			for _, n := range b.networks {
				items = append(items, n)
			}
			b.mu.Unlock()
			w.WriteHeader(http.StatusOK)
			_ = json.NewEncoder(w).Encode(items)

		case r.Method == http.MethodGet && strings.HasPrefix(pathClean, "/networks/") && !strings.Contains(strings.TrimPrefix(pathClean, "/networks/"), "/"):
			nid := strings.TrimPrefix(pathClean, "/networks/")
			n := b.findNetworkLocked(nid)
			b.mu.Unlock()
			if n == nil {
				w.WriteHeader(http.StatusNotFound)
				_ = json.NewEncoder(w).Encode(map[string]string{"message": "network " + nid + " not found"})
				return
			}
			w.WriteHeader(http.StatusOK)
			_ = json.NewEncoder(w).Encode(n)

		case r.Method == http.MethodPost && strings.HasPrefix(pathClean, "/networks/") && (strings.HasSuffix(pathClean, "/connect") || strings.HasSuffix(pathClean, "/disconnect")):
			rest := strings.TrimPrefix(pathClean, "/networks/")
			nid, _, _ := strings.Cut(rest, "/")
			n := b.findNetworkLocked(nid)
			b.mu.Unlock()
			if n == nil {
				w.WriteHeader(http.StatusNotFound)
				_ = json.NewEncoder(w).Encode(map[string]string{"message": "network " + nid + " not found"})
				return
			}
			w.WriteHeader(http.StatusOK)

		case r.Method == http.MethodDelete && strings.HasPrefix(pathClean, "/networks/") && !strings.Contains(strings.TrimPrefix(pathClean, "/networks/"), "/"):
			nid := strings.TrimPrefix(pathClean, "/networks/")
			n := b.findNetworkLocked(nid)
			if n != nil {
				delete(b.networks, n.ID)
			}
			b.mu.Unlock()
			if n == nil {
				w.WriteHeader(http.StatusNotFound)
				_ = json.NewEncoder(w).Encode(map[string]string{"message": "network " + nid + " not found"})
				return
			}
			w.WriteHeader(http.StatusNoContent)

		default:
			b.mu.Unlock()
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte("{}"))
		}
	}))
	t.Cleanup(b.Server.Close)
	return b
}

// SetUnavailable toggles simulated Docker Engine downtime.
func (b *MockDockerBackend) SetUnavailable(down bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.Unavailable = down
}

// Dial connects to the MockDockerBackend HTTP server or returns an error when
// marked unavailable.
func (b *MockDockerBackend) Dial(ctx context.Context) (net.Conn, error) {
	b.mu.Lock()
	down := b.Unavailable
	dialErr := b.DialErr
	b.mu.Unlock()
	if down {
		if dialErr != nil {
			return nil, dialErr
		}
		return nil, errors.New("mock docker backend stopped")
	}
	addr := strings.TrimPrefix(b.Server.URL, "http://")
	return (&net.Dialer{}).DialContext(ctx, "tcp", addr)
}

// SnapshotRequests returns a copy of recorded Docker backend requests.
func (b *MockDockerBackend) SnapshotRequests() []DockerRequest {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]DockerRequest(nil), b.Requests...)
}

type testManagerOptions struct {
	dataDir       string
	keychain      keychain.Keychain
	keychainNote  string
	keychainSet   bool
	developerMode bool
	hostInfo      ext.HostInfo
	supervisor    *sandbox.Supervisor
}

// ManagerOption customizes newTestManager.
type ManagerOption func(*testManagerOptions)

// WithDataDir sets a specific data directory for the test manager.
func WithDataDir(dir string) ManagerOption {
	return func(o *testManagerOptions) { o.dataDir = dir }
}

// WithKeychain injects a custom keychain implementation. Passing nil preserves
// cfg.Keychain = nil to simulate an unavailable OS keychain.
func WithKeychain(kc keychain.Keychain, note string) ManagerOption {
	return func(o *testManagerOptions) {
		o.keychain = kc
		o.keychainNote = note
		o.keychainSet = true
	}
}

// WithDeveloperMode configures the initial DeveloperMode flag on TestHarness.
func WithDeveloperMode(enabled bool) ManagerOption {
	return func(o *testManagerOptions) { o.developerMode = enabled }
}

// WithHostInfo overrides the default HostInfo returned to extensions.
func WithHostInfo(info ext.HostInfo) ManagerOption {
	return func(o *testManagerOptions) { o.hostInfo = info }
}

// WithSupervisor injects a sandbox.Supervisor instance.
func WithSupervisor(sup *sandbox.Supervisor) ManagerOption {
	return func(o *testManagerOptions) { o.supervisor = sup }
}

// TestHarness bundles an ext.Manager and its isolated doubles for E2E tests.
type TestHarness struct {
	DataDir       string
	Keychain      *keychain.Fake
	UIHost        *MockUIHost
	ImageBuilder  *MockImageBuilder
	ImagePuller   *MockImagePuller
	DockerBackend *MockDockerBackend
	DeveloperMode bool
	HostInfo      ext.HostInfo
}

// newTestManager constructs an isolated *ext.Manager with fake keychain,
// temporary data directory, and mock host/builder/puller/Docker doubles.
func newTestManager(t *testing.T, opts ...ManagerOption) (*ext.Manager, *TestHarness) {
	t.Helper()
	o := &testManagerOptions{
		hostInfo: ext.HostInfo{
			AppVersion: "0.10.1-e2e",
			OS:         runtime.GOOS,
			Arch:       runtime.GOARCH,
			Locale:     "en",
			Theme:      "dark",
			DockerMode: "sandbox",
		},
	}
	for _, opt := range opts {
		if opt != nil {
			opt(o)
		}
	}
	if o.dataDir == "" {
		o.dataDir = newTempDataDir(t)
	}
	var fakeKC *keychain.Fake
	kc := o.keychain
	if !o.keychainSet {
		fakeKC = newFakeKeychain(t)
		kc = fakeKC
	} else if kc == nil {
		// Preserve explicit nil keychain (simulating unavailable/file fallback)
		// while still forcing GVX_KEYCHAIN=file for defense-in-depth.
		t.Setenv(keychain.EnvVar, "file")
	} else if f, ok := kc.(*keychain.Fake); ok {
		fakeKC = f
	}
	h := &TestHarness{
		DataDir:       o.dataDir,
		Keychain:      fakeKC,
		UIHost:        NewMockUIHost(),
		ImageBuilder:  &MockImageBuilder{},
		ImagePuller:   &MockImagePuller{},
		DockerBackend: NewMockDockerBackend(t),
		DeveloperMode: o.developerMode,
		HostInfo:      o.hostInfo,
	}
	cfg := ext.Config{
		DataDir:           h.DataDir,
		Keychain:          kc,
		KeychainNote:      o.keychainNote,
		DeveloperMode:     func() bool { return h.DeveloperMode },
		HostInfo:          func() ext.HostInfo { return h.HostInfo },
		Supervisor:        o.supervisor,
		DockerBackendDial: h.DockerBackend.Dial,
		ImageBuilder:      h.ImageBuilder,
		ImagePuller:       h.ImagePuller,
		UIHost:            h.UIHost,
	}
	mgr, err := ext.NewManager(cfg)
	if err != nil {
		t.Fatalf("ext.NewManager: %v", err)
	}
	t.Cleanup(func() {
		_ = mgr.Close(5 * time.Second)
	})
	return mgr, h
}

// --- HTTP / Bearer / Gesture / SSE Helpers ---

// httpDoJSON issues an HTTP request with optional JSON body, Bearer token, and
// X-GVX-Gesture-ID header, decoding any JSON response into out if non-nil.
func httpDoJSON(t *testing.T, client *http.Client, method, url, bearer, gestureID string, reqBody any, out any) (int, http.Header, []byte) {
	t.Helper()
	if client == nil {
		client = http.DefaultClient
	}
	var bodyReader io.Reader
	if reqBody != nil {
		b, err := json.Marshal(reqBody)
		if err != nil {
			t.Fatalf("json.Marshal request: %v", err)
		}
		bodyReader = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, url, bodyReader)
	if err != nil {
		t.Fatalf("http.NewRequest(%s %s): %v", method, url, err)
	}
	if reqBody != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	if gestureID != "" {
		req.Header.Set("X-GVX-Gesture-ID", gestureID)
	}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("client.Do(%s %s): %v", method, url, err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("io.ReadAll: %v", err)
	}
	if out != nil && len(raw) > 0 {
		if err := json.Unmarshal(raw, out); err != nil {
			t.Fatalf("json.Unmarshal response (%d): %v\nbody: %s", resp.StatusCode, err, string(raw))
		}
	}
	return resp.StatusCode, resp.Header, raw
}

// newUnixHTTPClient creates an *http.Client that dials sockPath over Unix socket.
func newUnixHTTPClient(sockPath string) *http.Client {
	return &http.Client{
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				return (&net.Dialer{}).DialContext(ctx, "unix", sockPath)
			},
		},
	}
}

// SSEEvent represents a single parsed Server-Sent Event frame.
type SSEEvent struct {
	Event string
	Data  string
	ID    string
}

// readSSEEvents scans an HTTP SSE response stream until EOF or ctx cancellation,
// sending parsed events to ch.
func readSSEEvents(ctx context.Context, resp *http.Response, ch chan<- SSEEvent) {
	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 0, 64*1024), 1<<20)
	var cur SSEEvent
	var dataLines []string
	flush := func() bool {
		if cur.Event == "" && cur.ID == "" && len(dataLines) == 0 {
			return true
		}
		cur.Data = strings.Join(dataLines, "\n")
		select {
		case ch <- cur:
			cur = SSEEvent{}
			dataLines = nil
			return true
		case <-ctx.Done():
			return false
		}
	}
	for scanner.Scan() {
		select {
		case <-ctx.Done():
			return
		default:
		}
		line := scanner.Text()
		if line == "" {
			if !flush() {
				return
			}
			continue
		}
		if strings.HasPrefix(line, ":") {
			continue
		}
		field, val, _ := strings.Cut(line, ":")
		val = strings.TrimPrefix(val, " ")
		switch field {
		case "event":
			cur.Event = val
		case "data":
			dataLines = append(dataLines, val)
		case "id":
			cur.ID = val
		}
	}
	_ = flush()
}

// awaitSSEEvent waits up to timeout for an SSEEvent matching pred (or any event
// if pred is nil).
func awaitSSEEvent(t *testing.T, ch <-chan SSEEvent, timeout time.Duration, pred func(SSEEvent) bool) SSEEvent {
	t.Helper()
	deadline := time.NewTimer(timeout)
	defer deadline.Stop()
	for {
		select {
		case ev, ok := <-ch:
			if !ok {
				t.Fatal("SSE channel closed before matching event arrived")
			}
			if pred == nil || pred(ev) {
				return ev
			}
		case <-deadline.C:
			t.Fatalf("timed out after %v waiting for matching SSE event", timeout)
		}
	}
}
