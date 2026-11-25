//go:build e2e

package e2e

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net/http/cgi"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ironpark/gvx-desktop/internal/ext"
	"github.com/ironpark/gvx-desktop/internal/keychain"
	"github.com/ironpark/gvx-desktop/internal/recipes"
	"github.com/ironpark/gvx-desktop/internal/uiblock"
)

const e2e2CoreSDKSourceMinimal = `package main

import (
	"context"

	"github.com/ironpark/gvx-desktop/pkg/extsdk"
)

func main() {
	app := extsdk.New()
	app.View("main", extsdk.View{
		Render: func(ctx *extsdk.ViewContext, ui *extsdk.UI) extsdk.Node {
			return ui.Column(ui.Text("ready"))
		},
	})
	_ = app.Run(context.Background())
}
`

func e2e2CoreFindExtension(mgr *ext.Manager, id string) (ext.ExtensionInfo, bool) {
	for _, item := range mgr.List() {
		if item.ID == id {
			return item, true
		}
	}
	return ext.ExtensionInfo{}, false
}

func e2e2CoreMustExtension(t *testing.T, mgr *ext.Manager, id string) ext.ExtensionInfo {
	t.Helper()
	item, ok := e2e2CoreFindExtension(mgr, id)
	if !ok {
		t.Fatalf("extension %q not found in mgr.List()", id)
	}
	return item
}

func e2e2CoreWriteBinaryExt(t *testing.T, root, id, version string, binData []byte, extraTOML string, perms ...string) string {
	t.Helper()
	if binData == nil {
		binPath := buildFixtureSDKBinary(t, e2e2CoreSDKSourceMinimal)
		var err error
		binData, err = os.ReadFile(binPath)
		if err != nil {
			t.Fatalf("read fixture binary: %v", err)
		}
	}
	extDir := filepath.Join(root, id)
	manifest := binaryManifestTOML(id, version, "bin/ext", perms...) + extraTOML
	writeExtFixture(t, extDir, manifest, nil)
	if err := os.MkdirAll(filepath.Join(extDir, "bin"), 0o700); err != nil {
		t.Fatalf("mkdir bin: %v", err)
	}
	if err := os.WriteFile(filepath.Join(extDir, "bin", "ext"), binData, 0o755); err != nil {
		t.Fatalf("write dev_entrypoint: %v", err)
	}
	return extDir
}

func e2e2CoreInstallAndStartBinary(t *testing.T, mgr *ext.Manager, id, goSrc, extraTOML string, perms ...string) string {
	t.Helper()
	var binData []byte
	if goSrc != "" {
		binPath := buildFixtureSDKBinary(t, goSrc)
		var err error
		binData, err = os.ReadFile(binPath)
		if err != nil {
			t.Fatalf("read compiled binary: %v", err)
		}
	}
	extDir := e2e2CoreWriteBinaryExt(t, t.TempDir(), id, "0.1.0", binData, extraTOML, perms...)
	if _, err := mgr.InstallLocal(extDir); err != nil {
		t.Fatalf("InstallLocal(%s): %v", id, err)
	}
	if err := mgr.AllowAndBuild(id); err != nil {
		t.Fatalf("AllowAndBuild(%s): %v", id, err)
	}
	return extDir
}

func e2e2CoreNonReservedDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if resolved, err := filepath.EvalSymlinks(dir); err == nil {
		dir = resolved
	}
	guest, err := recipes.GuestPath(dir)
	isReserved := err != nil || recipes.ReservedGuestPath(guest)
	if !isReserved && runtime.GOOS == "darwin" && strings.HasPrefix(guest, "/private/") {
		isReserved = recipes.ReservedGuestPath(strings.TrimPrefix(guest, "/private"))
	}
	if !isReserved {
		return dir
	}
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		t.Fatalf("cannot find non-reserved host folder (temp dir %q is reserved): %v", dir, err)
	}
	custom, err := os.MkdirTemp(home, "gvx-e2e-folder-*")
	if err != nil {
		t.Fatalf("os.MkdirTemp in home: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(custom) })
	if resolved, err := filepath.EvalSymlinks(custom); err == nil {
		custom = resolved
	}
	return custom
}

// e2e2ServeGitHTTPS starts an httptest TLS server backing bareDir with
// git http-backend, writes the TLS CA certificate to a temp PEM file, configures
// GIT_SSL_CAINFO via t.Setenv, and returns the https://127.0.0.1:<port>/<repo> URL.
func e2e2ServeGitHTTPS(t *testing.T, bareDir string) string {
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

type e2e2ImageBuilder struct {
	mu          sync.Mutex
	Calls       []ext.BuildRequest
	ContextTars [][]byte
	LogLines    []string
	ImageID     string
	BuildErr    error
}

func (b *e2e2ImageBuilder) Build(ctx context.Context, req ext.BuildRequest, onLine func(string)) (string, error) {
	var buf bytes.Buffer
	if req.ContextDir != "" {
		if err := ext.PackBuildContext(ctx, req.ContextDir, req.Dockerfile, &buf); err != nil {
			return "", err
		}
	}
	b.mu.Lock()
	b.Calls = append(b.Calls, req)
	b.ContextTars = append(b.ContextTars, buf.Bytes())
	lines := append([]string(nil), b.LogLines...)
	imgID := b.ImageID
	if imgID == "" {
		imgID = "sha256:e2e2mockimage0001"
	}
	buildErr := b.BuildErr
	b.mu.Unlock()

	for _, line := range lines {
		if onLine != nil {
			onLine(line)
		}
	}
	if buildErr != nil {
		return "", buildErr
	}
	return imgID, nil
}

func e2e2TarEntries(t *testing.T, targz []byte) map[string]bool {
	t.Helper()
	gz, err := gzip.NewReader(bytes.NewReader(targz))
	if err != nil {
		t.Fatalf("gzip.NewReader: %v", err)
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	entries := map[string]bool{}
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("tar.Next: %v", err)
		}
		entries[strings.TrimSuffix(hdr.Name, "/")] = true
	}
	return entries
}

// =============================================================================
// Feature 9: Permission Catalog & Grant Store
// =============================================================================

func TestF9_T1_ImplicitPermissionsWorkWithoutManifestDeclaration(t *testing.T) {
	src := `package main

import (
	"context"
	"fmt"
	"os"

	"github.com/ironpark/gvx-desktop/pkg/extsdk"
)

func main() {
	app := extsdk.New()
	app.View("main", extsdk.View{
		Render: func(ctx *extsdk.ViewContext, ui *extsdk.UI) extsdk.Node {
			return ui.Column(ui.Text("implicit-view-ok"))
		},
	})
	app.Command("probe", func(ctx *extsdk.CommandContext) error {
		info, err := ctx.Host.Info(ctx)
		if err != nil {
			return fmt.Errorf("host info: %w", err)
		}
		if info.OS == "" || info.AppVersion == "" {
			return fmt.Errorf("unexpected empty host info: %+v", info)
		}
		if _, err := ctx.Host.Settings(ctx); err != nil {
			return fmt.Errorf("host settings: %w", err)
		}
		return ctx.Host.Toast(ctx, "info", "implicit-toast-ok")
	})
	fmt.Fprintln(os.Stderr, "implicit probe booted")
	_ = app.Run(context.Background())
}
`
	mgr, h := newTestManager(t, WithDeveloperMode(true))
	const id = "implicit-perm-ext"
	extraTOML := "\n[[contributes.command]]\nid = \"probe\"\ntitle = \"Probe Implicit\"\n"
	e2e2CoreInstallAndStartBinary(t, mgr, id, src, extraTOML)

	if err := mgr.RunCommand(id, "probe", "en"); err != nil {
		t.Fatalf("RunCommand(probe) failed for zero-perm extension: %v", err)
	}
	toasts := h.UIHost.SnapshotToasts()
	if len(toasts) == 0 || toasts[0].Message != "implicit-toast-ok" {
		t.Fatalf("expected implicit Toast recorded, got %+v", toasts)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	gotTree := false
	subCtx, subCancel := context.WithCancel(ctx)
	defer subCancel()
	ch, err := mgr.SubscribeView(subCtx, id, "main", "en")
	if err != nil {
		t.Fatalf("SubscribeView: %v", err)
	}
	for u := range ch {
		if u.Tree != nil {
			gotTree = true
			subCancel()
			break
		}
	}
	if !gotTree {
		t.Fatal("expected initial view tree from SubscribeView")
	}
}

func TestF9_T1_All10ExplicitPermissionsRecognized(t *testing.T) {
	if len(ext.AllPermissions) != 10 {
		t.Fatalf("expected 10 explicit permissions in catalog, got %d (%v)", len(ext.AllPermissions), ext.AllPermissions)
	}
	for _, p := range ext.AllPermissions {
		risk := ext.PermissionRisk(p)
		if risk != "low" && risk != "medium" && risk != "high" {
			t.Fatalf("unexpected PermissionRisk(%q) = %q", p, risk)
		}
		if ext.PermissionDescription(p) == "" {
			t.Fatalf("PermissionDescription(%q) is empty", p)
		}
	}

	mgr, _ := newTestManager(t, WithDeveloperMode(true))
	const id = "all-perms-ext"
	e2e2CoreInstallAndStartBinary(t, mgr, id, "", "", ext.AllPermissions...)

	info := e2e2CoreMustExtension(t, mgr, id)
	for _, p := range ext.AllPermissions {
		if !slices.Contains(info.Permissions, p) {
			t.Fatalf("ExtensionInfo.Permissions missing %q: %v", p, info.Permissions)
		}
		if !slices.Contains(info.Granted, p) {
			t.Fatalf("ExtensionInfo.Granted missing %q: %v", p, info.Granted)
		}
	}
}

func TestF9_T1_ApprovePersistsExtensionGrantsJSON(t *testing.T) {
	mgr, h := newTestManager(t, WithDeveloperMode(true))
	const id = "persist-grants"
	e2e2CoreInstallAndStartBinary(t, mgr, id, "", "", ext.PermStorage, ext.PermNetwork)

	grantsFile := ext.GrantsPath(h.DataDir)
	assertFilePerm(t, grantsFile, 0o600)

	gs, err := ext.LoadGrantStore(grantsFile)
	if err != nil {
		t.Fatalf("LoadGrantStore: %v", err)
	}
	entry := gs.Get(id)
	if !slices.Equal(entry.Granted, []string{ext.PermStorage, ext.PermNetwork}) {
		t.Fatalf("persisted Granted = %v, want [storage network]", entry.Granted)
	}
	if entry.ApprovedManifestHash == "" || entry.ApprovedVersion != "0.1.0" {
		t.Fatalf("persisted approval metadata incomplete: %+v", entry)
	}

	if err := mgr.Close(5 * time.Second); err != nil {
		t.Fatalf("mgr.Close: %v", err)
	}

	mgr2, _ := newTestManager(t, WithDataDir(h.DataDir), WithDeveloperMode(true))
	if err := mgr2.Start(context.Background()); err != nil {
		t.Fatalf("mgr2.Start: %v", err)
	}
	deadline := time.Now().Add(5 * time.Second)
	var info2 ext.ExtensionInfo
	for {
		info2 = e2e2CoreMustExtension(t, mgr2, id)
		if info2.State == ext.StateRunning || time.Now().After(deadline) {
			break
		}
		time.Sleep(25 * time.Millisecond)
	}
	if info2.State != ext.StateRunning {
		t.Fatalf("reloaded state = %q, want %q (err=%q)", info2.State, ext.StateRunning, info2.Error)
	}
	if !slices.Contains(info2.Granted, ext.PermStorage) || !slices.Contains(info2.Granted, ext.PermNetwork) {
		t.Fatalf("reloaded Granted = %v", info2.Granted)
	}
}

func TestF9_T1_AllowedFoldersPersistenceAndRemoval(t *testing.T) {
	src := `package main

import (
	"context"

	"github.com/ironpark/gvx-desktop/pkg/extsdk"
)

func main() {
	app := extsdk.New()
	app.View("main", extsdk.View{
		Render: func(ctx *extsdk.ViewContext, ui *extsdk.UI) extsdk.Node {
			return ui.Column(
				ui.Button("Pick", extsdk.Key("pick"), extsdk.OnTap("pick")),
			)
		},
		On: extsdk.Handlers{
			"pick": func(ctx *extsdk.EventContext) error {
				_, _, err := ctx.Host.PickFolder(ctx)
				return err
			},
		},
	})
	_ = app.Run(context.Background())
}
`
	mgr, h := newTestManager(t, WithDeveloperMode(true))
	folder := e2e2CoreNonReservedDir(t)
	h.UIHost.PickFolderResult = folder

	const id = "folder-grant-ext"
	e2e2CoreInstallAndStartBinary(t, mgr, id, src, "", ext.PermUIPickFolder)

	if err := mgr.SendViewEvent(id, "main", uiblock.Event{
		Node:   "/pick",
		Kind:   uiblock.EventTap,
		Locale: "en",
	}); err != nil {
		t.Fatalf("SendViewEvent(pick): %v", err)
	}

	info := e2e2CoreMustExtension(t, mgr, id)
	if len(info.AllowedFolders) != 1 || info.AllowedFolders[0] != folder {
		t.Fatalf("AllowedFolders = %v, want [%s]", info.AllowedFolders, folder)
	}

	if err := mgr.Close(5 * time.Second); err != nil {
		t.Fatalf("mgr.Close: %v", err)
	}

	mgr2, _ := newTestManager(t, WithDataDir(h.DataDir), WithDeveloperMode(true))
	if err := mgr2.Start(context.Background()); err != nil {
		t.Fatalf("mgr2.Start: %v", err)
	}
	info2 := e2e2CoreMustExtension(t, mgr2, id)
	if len(info2.AllowedFolders) != 1 || info2.AllowedFolders[0] != folder {
		t.Fatalf("reloaded AllowedFolders = %v, want [%s]", info2.AllowedFolders, folder)
	}

	if err := mgr2.RemoveAllowedFolder(id, folder); err != nil {
		t.Fatalf("RemoveAllowedFolder: %v", err)
	}
	infoAfter := e2e2CoreMustExtension(t, mgr2, id)
	if len(infoAfter.AllowedFolders) != 0 {
		t.Fatalf("expected AllowedFolders empty after removal, got %v", infoAfter.AllowedFolders)
	}
}

func TestF9_T1_RevokePermissionsHaltsAndUpdatesGrants(t *testing.T) {
	const src = `package main

import (
	"context"

	"github.com/ironpark/gvx-desktop/pkg/extsdk"
)

func main() {
	app := extsdk.New()
	app.View("main", extsdk.View{
		Render: func(ctx *extsdk.ViewContext, ui *extsdk.UI) extsdk.Node {
			return ui.Column(
				ui.Button("Pick", extsdk.Key("pick"), extsdk.OnTap("pick")),
			)
		},
		On: extsdk.Handlers{
			"pick": func(ctx *extsdk.EventContext) error {
				_, _, err := ctx.Host.PickFolder(ctx)
				return err
			},
		},
	})
	_ = app.Run(context.Background())
}
`
	mgr, h := newTestManager(t, WithDeveloperMode(true))
	folder := e2e2CoreNonReservedDir(t)
	h.UIHost.PickFolderResult = folder

	const id = "revoke-ext"
	e2e2CoreInstallAndStartBinary(t, mgr, id, src, "", ext.PermStorage, ext.PermUIPickFolder)

	if err := mgr.SendViewEvent(id, "main", uiblock.Event{
		Node:   "/pick",
		Kind:   uiblock.EventTap,
		Locale: "en",
	}); err != nil {
		t.Fatalf("SendViewEvent(pick): %v", err)
	}

	if info := e2e2CoreMustExtension(t, mgr, id); info.State != ext.StateRunning {
		t.Fatalf("pre-revoke state = %q, want running", info.State)
	}

	if err := mgr.RevokePermissions(id); err != nil {
		t.Fatalf("RevokePermissions: %v", err)
	}
	info := e2e2CoreMustExtension(t, mgr, id)
	if info.State != ext.StateWaiting {
		t.Fatalf("post-revoke state = %q, want %q", info.State, ext.StateWaiting)
	}
	if len(info.Granted) != 0 {
		t.Fatalf("expected Granted empty after RevokePermissions, got %v", info.Granted)
	}
	if err := mgr.Enable(id); err == nil {
		t.Fatal("expected Enable to fail while permissions are revoked")
	}

	gs2, err := ext.LoadGrantStore(ext.GrantsPath(h.DataDir))
	if err != nil {
		t.Fatalf("LoadGrantStore reload: %v", err)
	}
	if len(gs2.Get(id).AllowedFolders) != 1 || gs2.Get(id).AllowedFolders[0] != folder {
		t.Fatalf("expected AllowedFolders preserved after revoke, got %v", gs2.Get(id).AllowedFolders)
	}
}

func TestF9_T2_RejectsUnknownPermissionString(t *testing.T) {
	mgr, _ := newTestManager(t, WithDeveloperMode(true))
	unknowns := []string{"admin", "filesystem", "ui:exec", "root", ""}
	for i, perm := range unknowns {
		id := fmt.Sprintf("unknown-perm-%d", i)
		toml := minimalManifestTOML(id, "0.1.0", perm)
		if _, err := ext.ParseManifest([]byte(toml)); err == nil {
			t.Fatalf("ParseManifest unexpectedly accepted unknown permission %q", perm)
		}
		dir := createLocalExtensionDir(t, t.TempDir(), id, toml, nil)
		if _, err := mgr.InstallLocal(dir); err == nil {
			t.Fatalf("InstallLocal unexpectedly accepted unknown permission %q", perm)
		}
	}
}

func TestF9_T2_RejectsImplicitAndDuplicatePermissions(t *testing.T) {
	mgr, _ := newTestManager(t, WithDeveloperMode(true))
	for i, imp := range ext.ImplicitPermissions {
		id := fmt.Sprintf("imp-perm-%d", i)
		toml := minimalManifestTOML(id, "0.1.0", imp)
		if _, err := ext.ParseManifest([]byte(toml)); err == nil {
			t.Fatalf("ParseManifest unexpectedly accepted implicit permission %q", imp)
		}
		dir := createLocalExtensionDir(t, t.TempDir(), id, toml, nil)
		if _, err := mgr.InstallLocal(dir); err == nil {
			t.Fatalf("InstallLocal unexpectedly accepted implicit permission %q", imp)
		}
	}

	dupTOML := minimalManifestTOML("dup-perm-ext", "0.1.0", ext.PermStorage, ext.PermStorage)
	if _, err := ext.ParseManifest([]byte(dupTOML)); err == nil {
		t.Fatal("ParseManifest unexpectedly accepted duplicate permission")
	}
	dupDir := createLocalExtensionDir(t, t.TempDir(), "dup-perm-ext", dupTOML, nil)
	if _, err := mgr.InstallLocal(dupDir); err == nil {
		t.Fatal("InstallLocal unexpectedly accepted duplicate permission")
	}
}

// e2e2InitBareGitRepoWithExec initializes a local bare Git repo where files in
// execPaths are committed with executable mode 100755.
func e2e2InitBareGitRepoWithExec(t *testing.T, files map[string]string, execPaths ...string) (string, string) {
	t.Helper()
	execSet := make(map[string]bool, len(execPaths))
	for _, p := range execPaths {
		execSet[filepath.ToSlash(p)] = true
	}
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
		mode := os.FileMode(0o644)
		if execSet[filepath.ToSlash(rel)] {
			mode = 0o755
		}
		if err := os.WriteFile(full, []byte(content), mode); err != nil {
			t.Fatalf("os.WriteFile: %v", err)
		}
	}
	runGit(t, workDir, "add", "-A")
	for _, p := range execPaths {
		runGit(t, workDir, "update-index", "--chmod=+x", filepath.ToSlash(p))
	}
	runGit(t, workDir, "commit", "-m", "initial commit")
	sha := strings.TrimSpace(runGit(t, workDir, "rev-parse", "HEAD"))
	runGit(t, base, "clone", "--bare", workDir, bareDir)
	return "file://" + filepath.ToSlash(bareDir), sha
}

func TestF9_T2_UpdateAddingNewPermissionHaltsAndMovesToWaiting(t *testing.T) {
	binData, err := os.ReadFile(buildFixtureSDKBinary(t, e2e2CoreSDKSourceMinimal))
	if err != nil {
		t.Fatalf("read fixture binary: %v", err)
	}
	bareURL, _ := e2e2InitBareGitRepoWithExec(t, map[string]string{
		"extension.toml": binaryManifestTOML("git-esc-perm", "0.1.0", "bin/ext", ext.PermStorage),
		"bin/ext":        string(binData),
	}, "bin/ext")
	bareDir := strings.TrimPrefix(bareURL, "file://")
	httpsURL := e2e2ServeGitHTTPS(t, bareDir)

	mgr, _ := newTestManager(t, WithDeveloperMode(true))
	if _, err := mgr.InstallGit(httpsURL, "main"); err != nil {
		t.Fatalf("InstallGit v1: %v", err)
	}
	if err := mgr.AllowAndBuild("git-esc-perm"); err != nil {
		t.Fatalf("AllowAndBuild v1: %v", err)
	}
	if info := e2e2CoreMustExtension(t, mgr, "git-esc-perm"); info.State != ext.StateRunning {
		t.Fatalf("state after v1 start = %q, want running", info.State)
	}

	workDir := filepath.Join(t.TempDir(), "work")
	commitToBareRepo(t, bareURL, workDir, map[string]string{
		"extension.toml": binaryManifestTOML("git-esc-perm", "0.2.0", "bin/ext", ext.PermStorage, ext.PermDocker),
	})

	info2, err := mgr.InstallGit(httpsURL, "main")
	if err != nil {
		t.Fatalf("InstallGit v2: %v", err)
	}
	if info2.State != ext.StateWaiting {
		t.Fatalf("expected StateWaiting after permission escalation, got %q", info2.State)
	}
	if !slices.Contains(info2.Asked, ext.PermDocker) {
		t.Fatalf("expected Asked to contain %q, got %v", ext.PermDocker, info2.Asked)
	}
	if err := mgr.Enable("git-esc-perm"); err == nil {
		t.Fatal("expected Enable to fail before re-approving escalated permission")
	}
}

func TestF9_T2_UpdateChangingCommitSHAOrVersionMovesToWaiting(t *testing.T) {
	binData, err := os.ReadFile(buildFixtureSDKBinary(t, e2e2CoreSDKSourceMinimal))
	if err != nil {
		t.Fatalf("read fixture binary: %v", err)
	}
	bareURL, sha1 := e2e2InitBareGitRepoWithExec(t, map[string]string{
		"extension.toml": binaryManifestTOML("git-esc-sha", "0.1.0", "bin/ext", ext.PermStorage),
		"bin/ext":        string(binData),
	}, "bin/ext")
	bareDir := strings.TrimPrefix(bareURL, "file://")
	httpsURL := e2e2ServeGitHTTPS(t, bareDir)

	mgr, _ := newTestManager(t, WithDeveloperMode(true))
	if _, err := mgr.InstallGit(httpsURL, "main"); err != nil {
		t.Fatalf("InstallGit sha1: %v", err)
	}
	if err := mgr.AllowAndBuild("git-esc-sha"); err != nil {
		t.Fatalf("AllowAndBuild sha1: %v", err)
	}

	// Same permissions, new commit SHA + version.
	workDir := filepath.Join(t.TempDir(), "work")
	sha2 := commitToBareRepo(t, bareURL, workDir, map[string]string{
		"extension.toml": binaryManifestTOML("git-esc-sha", "0.1.1", "bin/ext", ext.PermStorage),
		"CHANGELOG.md":   "bump\n",
	})
	if sha2 == sha1 {
		t.Fatal("expected distinct commit SHA")
	}

	info2, err := mgr.InstallGit(httpsURL, "main")
	if err != nil {
		t.Fatalf("InstallGit sha2: %v", err)
	}
	if info2.State != ext.StateWaiting {
		t.Fatalf("expected StateWaiting after commit/version change, got %q", info2.State)
	}
	if info2.Source.Commit != sha2 {
		t.Fatalf("Source.Commit = %q, want %q", info2.Source.Commit, sha2)
	}
}

func TestF9_T2_DenyPreventsStartAndRecordsDeniedState(t *testing.T) {
	mgr, h := newTestManager(t, WithDeveloperMode(true))
	const id = "deny-ext"
	dir := e2e2CoreWriteBinaryExt(t, t.TempDir(), id, "0.1.0", nil, "", ext.PermStorage, ext.PermNetwork)

	if _, err := mgr.InstallLocal(dir); err != nil {
		t.Fatalf("InstallLocal: %v", err)
	}
	if err := mgr.Deny(id); err != nil {
		t.Fatalf("Deny: %v", err)
	}
	info := e2e2CoreMustExtension(t, mgr, id)
	if info.State.IsActive() {
		t.Fatalf("denied extension should not be active, state=%q", info.State)
	}
	if !slices.Contains(info.Denied, ext.PermStorage) || len(info.Granted) != 0 {
		t.Fatalf("unexpected grant state after Deny: granted=%v denied=%v", info.Granted, info.Denied)
	}
	if err := mgr.Enable(id); err == nil {
		t.Fatal("expected Enable on denied extension to fail")
	}

	gs, err := ext.LoadGrantStore(ext.GrantsPath(h.DataDir))
	if err != nil {
		t.Fatalf("LoadGrantStore: %v", err)
	}
	if gs.Get(id).DeniedManifestHash == "" {
		t.Fatal("expected DeniedManifestHash persisted in extension-grants.json")
	}

	// Denying an already running extension must stop it immediately.
	const id2 = "deny-running-ext"
	e2e2CoreInstallAndStartBinary(t, mgr, id2, "", "", ext.PermStorage)
	if err := mgr.Deny(id2); err != nil {
		t.Fatalf("Deny(running): %v", err)
	}
	if info2 := e2e2CoreMustExtension(t, mgr, id2); info2.State.IsActive() {
		t.Fatalf("running extension remained active after Deny: state=%q", info2.State)
	}
}

// =============================================================================
// Feature 10: KV Storage (storage.json)
// =============================================================================

func TestF10_T1_StoragePutGetRoundtrip(t *testing.T) {
	src := `package main

import (
	"context"
	"fmt"

	"github.com/ironpark/gvx-desktop/pkg/extsdk"
)

type e2eItem struct {
	Name  string ` + "`json:\"name\"`" + `
	Count int    ` + "`json:\"count\"`" + `
}

func main() {
	app := extsdk.New()
	app.View("main", extsdk.View{
		Render: func(ctx *extsdk.ViewContext, ui *extsdk.UI) extsdk.Node {
			return ui.Column(ui.Text("kv"))
		},
	})
	app.Command("roundtrip", func(ctx *extsdk.CommandContext) error {
		if err := ctx.Host.StorageSet(ctx, "plain", "hello-kv"); err != nil {
			return err
		}
		got, ok, err := ctx.Host.StorageGet(ctx, "plain")
		if err != nil || !ok || got != "hello-kv" {
			return fmt.Errorf("StorageGet = (%q, %v, %v)", got, ok, err)
		}
		if err := ctx.Host.StorageSetJSON(ctx, "obj", e2eItem{Name: "alpha", Count: 7}); err != nil {
			return err
		}
		var out e2eItem
		ok, err = ctx.Host.StorageGetJSON(ctx, "obj", &out)
		if err != nil || !ok || out.Name != "alpha" || out.Count != 7 {
			return fmt.Errorf("StorageGetJSON = (%+v, %v, %v)", out, ok, err)
		}
		return nil
	})
	_ = app.Run(context.Background())
}
`
	mgr, h := newTestManager(t, WithDeveloperMode(true))
	const id = "kv-roundtrip"
	e2e2CoreInstallAndStartBinary(t, mgr, id, src, "\n[[contributes.command]]\nid = \"roundtrip\"\ntitle = \"Roundtrip\"\n", ext.PermStorage)

	if err := mgr.RunCommand(id, "roundtrip", "en"); err != nil {
		t.Fatalf("RunCommand(roundtrip): %v", err)
	}
	assertFilePerm(t, ext.StoragePath(h.DataDir, id), 0o600)
}

func TestF10_T1_StoragePrefixListing(t *testing.T) {
	src := `package main

import (
	"context"
	"fmt"
	"slices"

	"github.com/ironpark/gvx-desktop/pkg/extsdk"
)

func main() {
	app := extsdk.New()
	app.View("main", extsdk.View{
		Render: func(ctx *extsdk.ViewContext, ui *extsdk.UI) extsdk.Node {
			return ui.Column(ui.Text("prefix"))
		},
	})
	app.Command("prefix-test", func(ctx *extsdk.CommandContext) error {
		for k, v := range map[string]string{
			"job/1":   "a",
			"job/10":  "b",
			"job/2":   "c",
			"other/x": "d",
		} {
			if err := ctx.Host.StorageSet(ctx, k, v); err != nil {
				return err
			}
		}
		got, err := ctx.Host.StorageList(ctx, "job/")
		if err != nil {
			return err
		}
		want := []string{"job/1", "job/10", "job/2"}
		if !slices.Equal(got, want) {
			return fmt.Errorf("StorageList(job/) = %v, want %v", got, want)
		}
		all, err := ctx.Host.StorageList(ctx, "")
		if err != nil {
			return err
		}
		if len(all) != 4 {
			return fmt.Errorf("StorageList('') len = %d, want 4 (%v)", len(all), all)
		}
		return nil
	})
	_ = app.Run(context.Background())
}
`
	mgr, _ := newTestManager(t, WithDeveloperMode(true))
	const id = "kv-prefix"
	e2e2CoreInstallAndStartBinary(t, mgr, id, src, "\n[[contributes.command]]\nid = \"prefix-test\"\ntitle = \"Prefix\"\n", ext.PermStorage)

	if err := mgr.RunCommand(id, "prefix-test", "en"); err != nil {
		t.Fatalf("RunCommand(prefix-test): %v", err)
	}
}

func TestF10_T1_StorageDeleteAndQuotaReclamation(t *testing.T) {
	src := `package main

import (
	"context"
	"fmt"
	"strings"

	"github.com/ironpark/gvx-desktop/pkg/extsdk"
)

func main() {
	app := extsdk.New()
	app.View("main", extsdk.View{
		Render: func(ctx *extsdk.ViewContext, ui *extsdk.UI) extsdk.Node {
			return ui.Column(ui.Text("delete"))
		},
	})
	app.Command("seed", func(ctx *extsdk.CommandContext) error {
		return ctx.Host.StorageSet(ctx, "blob", strings.Repeat("A", 1024))
	})
	app.Command("del", func(ctx *extsdk.CommandContext) error {
		if err := ctx.Host.StorageDelete(ctx, "blob"); err != nil {
			return err
		}
		_, ok, err := ctx.Host.StorageGet(ctx, "blob")
		if err != nil || ok {
			return fmt.Errorf("after delete StorageGet = (ok=%v, err=%v), want (false, nil)", ok, err)
		}
		return ctx.Host.StorageSet(ctx, "reused", strings.Repeat("B", 512))
	})
	_ = app.Run(context.Background())
}
`
	mgr, _ := newTestManager(t, WithDeveloperMode(true))
	const id = "kv-delete"
	extra := "\n[[contributes.command]]\nid = \"seed\"\ntitle = \"Seed\"\n\n[[contributes.command]]\nid = \"del\"\ntitle = \"Delete\"\n"
	e2e2CoreInstallAndStartBinary(t, mgr, id, src, extra, ext.PermStorage)

	if err := mgr.RunCommand(id, "seed", "en"); err != nil {
		t.Fatalf("RunCommand(seed): %v", err)
	}
	info1 := e2e2CoreMustExtension(t, mgr, id)
	if info1.StorageUsed != len("blob")+1024 {
		t.Fatalf("StorageUsed after seed = %d, want %d", info1.StorageUsed, len("blob")+1024)
	}

	if err := mgr.RunCommand(id, "del", "en"); err != nil {
		t.Fatalf("RunCommand(del): %v", err)
	}
	info2 := e2e2CoreMustExtension(t, mgr, id)
	wantUsed := len("reused") + 512
	if info2.StorageUsed != wantUsed {
		t.Fatalf("StorageUsed after delete+reuse = %d, want %d", info2.StorageUsed, wantUsed)
	}
}

func TestF10_T1_StoragePersistsAcrossManagerRestart(t *testing.T) {
	src := `package main

import (
	"context"
	"fmt"

	"github.com/ironpark/gvx-desktop/pkg/extsdk"
)

func main() {
	app := extsdk.New()
	app.View("main", extsdk.View{
		Render: func(ctx *extsdk.ViewContext, ui *extsdk.UI) extsdk.Node {
			return ui.Column(ui.Text("persist"))
		},
	})
	app.Command("write", func(ctx *extsdk.CommandContext) error {
		return ctx.Host.StorageSet(ctx, "session-key", "persisted-value-42")
	})
	app.Command("verify", func(ctx *extsdk.CommandContext) error {
		val, ok, err := ctx.Host.StorageGet(ctx, "session-key")
		if err != nil || !ok || val != "persisted-value-42" {
			return fmt.Errorf("verify got (%q, %v, %v)", val, ok, err)
		}
		return nil
	})
	_ = app.Run(context.Background())
}
`
	mgr1, h := newTestManager(t, WithDeveloperMode(true))
	const id = "kv-restart"
	extra := "\n[[contributes.command]]\nid = \"write\"\ntitle = \"Write\"\n\n[[contributes.command]]\nid = \"verify\"\ntitle = \"Verify\"\n"
	e2e2CoreInstallAndStartBinary(t, mgr1, id, src, extra, ext.PermStorage)

	if err := mgr1.RunCommand(id, "write", "en"); err != nil {
		t.Fatalf("RunCommand(write): %v", err)
	}
	if err := mgr1.Close(5 * time.Second); err != nil {
		t.Fatalf("mgr1.Close: %v", err)
	}

	mgr2, _ := newTestManager(t, WithDataDir(h.DataDir), WithDeveloperMode(true))
	if err := mgr2.Start(context.Background()); err != nil {
		t.Fatalf("mgr2.Start: %v", err)
	}
	if err := mgr2.RunCommand(id, "verify", "en"); err != nil {
		t.Fatalf("RunCommand(verify) after restart: %v", err)
	}
}

func TestF10_T1_StorageQuotaReporting(t *testing.T) {
	src := `package main

import (
	"context"

	"github.com/ironpark/gvx-desktop/pkg/extsdk"
)

func main() {
	app := extsdk.New()
	app.View("main", extsdk.View{
		Render: func(ctx *extsdk.ViewContext, ui *extsdk.UI) extsdk.Node {
			return ui.Column(ui.Text("quota"))
		},
	})
	app.Command("write-two", func(ctx *extsdk.CommandContext) error {
		if err := ctx.Host.StorageSet(ctx, "k1", "12345"); err != nil {
			return err
		}
		return ctx.Host.StorageSet(ctx, "k2", "abcdef")
	})
	_ = app.Run(context.Background())
}
`
	mgr, _ := newTestManager(t, WithDeveloperMode(true))
	const id = "kv-quota-report"
	e2e2CoreInstallAndStartBinary(t, mgr, id, src, "\n[[contributes.command]]\nid = \"write-two\"\ntitle = \"Write Two\"\n", ext.PermStorage)

	info0 := e2e2CoreMustExtension(t, mgr, id)
	if info0.StorageQuota != ext.DefaultStorageQuota {
		t.Fatalf("StorageQuota = %d, want %d", info0.StorageQuota, ext.DefaultStorageQuota)
	}
	if err := mgr.RunCommand(id, "write-two", "en"); err != nil {
		t.Fatalf("RunCommand(write-two): %v", err)
	}
	info1 := e2e2CoreMustExtension(t, mgr, id)
	wantUsed := len("k1") + len("12345") + len("k2") + len("abcdef")
	if info1.StorageUsed != wantUsed {
		t.Fatalf("StorageUsed = %d, want %d", info1.StorageUsed, wantUsed)
	}
}

func TestF10_T2_RejectsKeyOver128Bytes(t *testing.T) {
	src := `package main

import (
	"context"
	"fmt"
	"strings"

	"github.com/ironpark/gvx-desktop/pkg/extsdk"
)

func main() {
	app := extsdk.New()
	app.View("main", extsdk.View{
		Render: func(ctx *extsdk.ViewContext, ui *extsdk.UI) extsdk.Node {
			return ui.Column(ui.Text("keylen"))
		},
	})
	app.Command("check-keylen", func(ctx *extsdk.CommandContext) error {
		k128 := strings.Repeat("a", 128)
		if err := ctx.Host.StorageSet(ctx, k128, "ok"); err != nil {
			return fmt.Errorf("128B key failed: %w", err)
		}
		k129 := strings.Repeat("b", 129)
		if err := ctx.Host.StorageSet(ctx, k129, "bad"); err == nil {
			return fmt.Errorf("129B key unexpectedly succeeded")
		}
		return nil
	})
	_ = app.Run(context.Background())
}
`
	mgr, h := newTestManager(t, WithDeveloperMode(true))
	const id = "kv-keylen"
	e2e2CoreInstallAndStartBinary(t, mgr, id, src, "\n[[contributes.command]]\nid = \"check-keylen\"\ntitle = \"Check Keylen\"\n", ext.PermStorage)

	if err := mgr.RunCommand(id, "check-keylen", "en"); err != nil {
		t.Fatalf("RunCommand(check-keylen): %v", err)
	}
	raw, err := os.ReadFile(ext.StoragePath(h.DataDir, id))
	if err != nil {
		t.Fatalf("read storage.json: %v", err)
	}
	if strings.Contains(string(raw), strings.Repeat("b", 129)) {
		t.Fatal("129-byte key persisted in storage.json")
	}
}

func TestF10_T2_RejectsInvalidKeyAndNonUTF8Value(t *testing.T) {
	// Direct store boundary checks for invalid UTF-8 keys/values & empty key.
	st := ext.NewStorage(filepath.Join(t.TempDir(), "storage.json"), 0)
	if _, err := st.Set("", "v"); err == nil {
		t.Fatal("Storage.Set accepted empty key")
	}
	if _, err := st.Set("bad\xffkey", "v"); err == nil {
		t.Fatal("Storage.Set accepted invalid UTF-8 key")
	}
	if _, err := st.Set("k", "bad\xffval"); err == nil {
		t.Fatal("Storage.Set accepted invalid UTF-8 value")
	}

	src := `package main

import (
	"bytes"
	"context"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"

	"github.com/ironpark/gvx-desktop/pkg/extsdk"
)

func main() {
	app := extsdk.New()
	app.View("main", extsdk.View{
		Render: func(ctx *extsdk.ViewContext, ui *extsdk.UI) extsdk.Node {
			return ui.Column(ui.Text("invalid-kv"))
		},
	})
	app.Command("bad-inputs", func(ctx *extsdk.CommandContext) error {
		if err := ctx.Host.StorageSet(ctx, "", "v"); err == nil {
			return fmt.Errorf("empty key succeeded")
		}
		if err := ctx.Host.StorageSet(ctx, string([]byte{0xff, 0xfe}), "v"); err == nil {
			return fmt.Errorf("invalid UTF-8 key succeeded")
		}
		if err := ctx.Host.StorageSet(ctx, "ok", string([]byte{0xff, 0xfe})); err == nil {
			return fmt.Errorf("invalid UTF-8 value succeeded")
		}
		// Control characters via Host HTTP endpoint directly.
		hostAPI := os.Getenv("GVX_HOST_API")
		hostTok := os.Getenv("GVX_HOST_TOKEN")
		client := http.DefaultClient
		base := hostAPI
		if strings.HasPrefix(hostAPI, "unix://") {
			sock := strings.TrimPrefix(hostAPI, "unix://")
			client = &http.Client{Transport: &http.Transport{
				DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
					return (&net.Dialer{}).DialContext(ctx, "unix", sock)
				},
			}}
			base = "http://unix"
		}
		for _, badKey := range []string{"bad\x00key", "bad\nkey"} {
			req, _ := http.NewRequestWithContext(ctx, http.MethodPut, base+"/v1/storage/"+url.PathEscape(badKey), bytes.NewReader([]byte(` + "`" + `{"value":"v"}` + "`" + `)))
			req.Header.Set("Authorization", "Bearer "+hostTok)
			req.Header.Set("Content-Type", "application/json")
			resp, err := client.Do(req)
			if err != nil {
				return err
			}
			_ = resp.Body.Close()
			if resp.StatusCode < 400 || resp.StatusCode >= 500 {
				return fmt.Errorf("expected 4xx for control-char key %q, got %d", badKey, resp.StatusCode)
			}
		}
		return nil
	})
	_ = app.Run(context.Background())
}
`
	mgr, _ := newTestManager(t, WithDeveloperMode(true))
	const id = "kv-invalid-utf8"
	e2e2CoreInstallAndStartBinary(t, mgr, id, src, "\n[[contributes.command]]\nid = \"bad-inputs\"\ntitle = \"Bad Inputs\"\n", ext.PermStorage)

	if err := mgr.RunCommand(id, "bad-inputs", "en"); err != nil {
		t.Fatalf("RunCommand(bad-inputs): %v", err)
	}
}

func TestF10_T2_Enforces4MiBQuota(t *testing.T) {
	src := `package main

import (
	"context"
	"fmt"
	"strings"

	"github.com/ironpark/gvx-desktop/pkg/extsdk"
)

func main() {
	app := extsdk.New()
	app.View("main", extsdk.View{
		Render: func(ctx *extsdk.ViewContext, ui *extsdk.UI) extsdk.Node {
			return ui.Column(ui.Text("quota-limit"))
		},
	})
	app.Command("fill", func(ctx *extsdk.CommandContext) error {
		chunk := strings.Repeat("Z", 512*1024)
		for i := 0; i < 7; i++ {
			if err := ctx.Host.StorageSet(ctx, fmt.Sprintf("chunk-%d", i), chunk); err != nil {
				return fmt.Errorf("chunk %d failed prematurely: %w", i, err)
			}
		}
		if err := ctx.Host.StorageSet(ctx, "overflow", strings.Repeat("X", 600*1024)); err == nil {
			return fmt.Errorf("expected quota exceeded error above 4 MiB")
		}
		val, ok, err := ctx.Host.StorageGet(ctx, "chunk-0")
		if err != nil || !ok || len(val) != 512*1024 {
			return fmt.Errorf("existing key corrupted after quota rejection: ok=%v len=%d err=%v", ok, len(val), err)
		}
		return nil
	})
	_ = app.Run(context.Background())
}
`
	mgr, _ := newTestManager(t, WithDeveloperMode(true))
	const id = "kv-4mib-cap"
	e2e2CoreInstallAndStartBinary(t, mgr, id, src, "\n[[contributes.command]]\nid = \"fill\"\ntitle = \"Fill\"\n", ext.PermStorage)

	if err := mgr.RunCommand(id, "fill", "en"); err != nil {
		t.Fatalf("RunCommand(fill): %v", err)
	}
	info := e2e2CoreMustExtension(t, mgr, id)
	if info.StorageUsed > ext.DefaultStorageQuota {
		t.Fatalf("StorageUsed %d exceeded quota %d", info.StorageUsed, ext.DefaultStorageQuota)
	}
}

func TestF10_T2_Returns403WithoutStoragePermission(t *testing.T) {
	src := `package main

import (
	"context"
	"fmt"

	"github.com/ironpark/gvx-desktop/pkg/extsdk"
)

func main() {
	app := extsdk.New()
	app.View("main", extsdk.View{
		Render: func(ctx *extsdk.ViewContext, ui *extsdk.UI) extsdk.Node {
			return ui.Column(ui.Text("no-storage"))
		},
	})
	app.Command("try-storage", func(ctx *extsdk.CommandContext) error {
		if _, err := ctx.Host.StorageList(ctx, ""); !extsdk.IsPermissionDenied(err) {
			return fmt.Errorf("StorageList err = %v, want 403", err)
		}
		if _, _, err := ctx.Host.StorageGet(ctx, "k"); !extsdk.IsPermissionDenied(err) {
			return fmt.Errorf("StorageGet err = %v, want 403", err)
		}
		if err := ctx.Host.StorageSet(ctx, "k", "v"); !extsdk.IsPermissionDenied(err) {
			return fmt.Errorf("StorageSet err = %v, want 403", err)
		}
		if err := ctx.Host.StorageDelete(ctx, "k"); !extsdk.IsPermissionDenied(err) {
			return fmt.Errorf("StorageDelete err = %v, want 403", err)
		}
		return nil
	})
	_ = app.Run(context.Background())
}
`
	mgr, _ := newTestManager(t, WithDeveloperMode(true))
	const id = "no-storage-perm"
	e2e2CoreInstallAndStartBinary(t, mgr, id, src, "\n[[contributes.command]]\nid = \"try-storage\"\ntitle = \"Try Storage\"\n")

	if err := mgr.RunCommand(id, "try-storage", "en"); err != nil {
		t.Fatalf("RunCommand(try-storage): %v", err)
	}
}

func TestF10_T2_PerExtensionStorageIsolation(t *testing.T) {
	src := `package main

import (
	"context"
	"fmt"
	"os"
	"slices"

	"github.com/ironpark/gvx-desktop/pkg/extsdk"
)

func main() {
	app := extsdk.New()
	app.View("main", extsdk.View{
		Render: func(ctx *extsdk.ViewContext, ui *extsdk.UI) extsdk.Node {
			return ui.Column(ui.Text("iso"))
		},
	})
	app.Command("write-own", func(ctx *extsdk.CommandContext) error {
		return ctx.Host.StorageSet(ctx, "shared-key", "val-of-"+os.Getenv("GVX_EXT_ID"))
	})
	app.Command("verify-own", func(ctx *extsdk.CommandContext) error {
		want := "val-of-" + os.Getenv("GVX_EXT_ID")
		got, ok, err := ctx.Host.StorageGet(ctx, "shared-key")
		if err != nil || !ok || got != want {
			return fmt.Errorf("StorageGet = (%q, %v, %v), want %q", got, ok, err, want)
		}
		keys, err := ctx.Host.StorageList(ctx, "")
		if err != nil {
			return err
		}
		if !slices.Equal(keys, []string{"shared-key"}) {
			return fmt.Errorf("StorageList = %v, want [shared-key]", keys)
		}
		return nil
	})
	_ = app.Run(context.Background())
}
`
	mgr, _ := newTestManager(t, WithDeveloperMode(true))
	extra := "\n[[contributes.command]]\nid = \"write-own\"\ntitle = \"Write Own\"\n\n[[contributes.command]]\nid = \"verify-own\"\ntitle = \"Verify Own\"\n"
	e2e2CoreInstallAndStartBinary(t, mgr, "iso-ext-a", src, extra, ext.PermStorage)
	e2e2CoreInstallAndStartBinary(t, mgr, "iso-ext-b", src, extra, ext.PermStorage)

	if err := mgr.RunCommand("iso-ext-a", "write-own", "en"); err != nil {
		t.Fatalf("iso-ext-a write-own: %v", err)
	}
	if err := mgr.RunCommand("iso-ext-b", "write-own", "en"); err != nil {
		t.Fatalf("iso-ext-b write-own: %v", err)
	}
	if err := mgr.RunCommand("iso-ext-a", "verify-own", "en"); err != nil {
		t.Fatalf("iso-ext-a verify-own: %v", err)
	}
	if err := mgr.RunCommand("iso-ext-b", "verify-own", "en"); err != nil {
		t.Fatalf("iso-ext-b verify-own: %v", err)
	}
}

// =============================================================================
// Feature 11: Secrets Vault & Settings
// =============================================================================

func TestF11_T1_SecretCRUDViaFakeKeychain(t *testing.T) {
	src := `package main

import (
	"context"
	"fmt"
	"slices"

	"github.com/ironpark/gvx-desktop/pkg/extsdk"
)

func main() {
	app := extsdk.New()
	app.View("main", extsdk.View{
		Render: func(ctx *extsdk.ViewContext, ui *extsdk.UI) extsdk.Node {
			return ui.Column(ui.Text("secrets"))
		},
	})
	app.Command("crud", func(ctx *extsdk.CommandContext) error {
		if err := ctx.Host.SecretPut(ctx, "api_key", "s3cr3t-value-1"); err != nil {
			return err
		}
		if err := ctx.Host.SecretPut(ctx, "other_tok", "s3cr3t-value-2"); err != nil {
			return err
		}
		val, ok, err := ctx.Host.SecretGet(ctx, "api_key")
		if err != nil || !ok || val != "s3cr3t-value-1" {
			return fmt.Errorf("SecretGet = (%q, %v, %v)", val, ok, err)
		}
		names, err := ctx.Host.SecretList(ctx)
		if err != nil {
			return err
		}
		if !slices.Equal(names, []string{"api_key", "other_tok"}) {
			return fmt.Errorf("SecretList = %v", names)
		}
		return ctx.Host.SecretDelete(ctx, "other_tok")
	})
	_ = app.Run(context.Background())
}
`
	mgr, h := newTestManager(t, WithDeveloperMode(true))
	const id = "secret-crud"
	e2e2CoreInstallAndStartBinary(t, mgr, id, src, "\n[[contributes.command]]\nid = \"crud\"\ntitle = \"CRUD\"\n", ext.PermSecrets)

	if err := mgr.RunCommand(id, "crud", "en"); err != nil {
		t.Fatalf("RunCommand(crud): %v", err)
	}
	svc := ext.KeychainService(h.DataDir, id)
	entries := h.Keychain.Entries()
	if !slices.Contains(entries, [2]string{svc, "secret:api_key"}) {
		t.Fatalf("Fake keychain entries %v missing (%q, %q)", entries, svc, "secret:api_key")
	}
	if got, err := h.Keychain.Get(svc, "secret:api_key"); err != nil || got != "s3cr3t-value-1" {
		t.Fatalf("Fake keychain entry = (%q, %v), want s3cr3t-value-1", got, err)
	}
	if slices.Contains(entries, [2]string{svc, "secret:other_tok"}) {
		t.Fatalf("deleted secret still present in Fake keychain entries: %v", entries)
	}
	if _, err := h.Keychain.Get(svc, "secret:other_tok"); !errors.Is(err, keychain.ErrNotFound) {
		t.Fatalf("deleted secret Get err = %v, want keychain.ErrNotFound", err)
	}

	idxPath := ext.SecretsIndexPath(h.DataDir, id)
	assertFilePerm(t, idxPath, 0o600)
	idxBytes, err := os.ReadFile(idxPath)
	if err != nil {
		t.Fatalf("read secrets-index.json: %v", err)
	}
	if strings.Contains(string(idxBytes), "s3cr3t-value-1") {
		t.Fatal("secrets-index.json must not contain secret values")
	}
}

func TestF11_T1_FileFallback0600WhenKeychainUnavailable(t *testing.T) {
	src := `package main

import (
	"context"
	"fmt"
	"slices"

	"github.com/ironpark/gvx-desktop/pkg/extsdk"
)

func main() {
	app := extsdk.New()
	app.View("main", extsdk.View{
		Render: func(ctx *extsdk.ViewContext, ui *extsdk.UI) extsdk.Node {
			return ui.Column(ui.Text("fallback"))
		},
	})
	app.Command("store", func(ctx *extsdk.CommandContext) error {
		if err := ctx.Host.SecretPut(ctx, "fb_key", "fb-secret-val"); err != nil {
			return err
		}
		got, ok, err := ctx.Host.SecretGet(ctx, "fb_key")
		if err != nil || !ok || got != "fb-secret-val" {
			return fmt.Errorf("SecretGet = (%q, %v, %v)", got, ok, err)
		}
		list, err := ctx.Host.SecretList(ctx)
		if err != nil || !slices.Equal(list, []string{"fb_key"}) {
			return fmt.Errorf("SecretList = (%v, %v)", list, err)
		}
		return nil
	})
	_ = app.Run(context.Background())
}
`
	mgr, h := newTestManager(t, WithKeychain(nil, "OS keychain unavailable"), WithDeveloperMode(true))
	const id = "secret-fallback"
	e2e2CoreInstallAndStartBinary(t, mgr, id, src, "\n[[contributes.command]]\nid = \"store\"\ntitle = \"Store\"\n", ext.PermSecrets)

	if err := mgr.RunCommand(id, "store", "en"); err != nil {
		t.Fatalf("RunCommand(store): %v", err)
	}
	secFile := ext.SecretsFilePath(h.DataDir, id)
	assertFilePerm(t, secFile, 0o600)
	st := ext.SecretStoreStatusFor(nil, "OS keychain unavailable")
	if st.Store != "file" || st.Detail != "OS keychain unavailable" {
		t.Fatalf("unexpected SecretStoreStatusFor: %+v", st)
	}
}

func TestF11_T1_AutoMigratesFallbackFileToKeychain(t *testing.T) {
	src := `package main

import (
	"context"
	"fmt"

	"github.com/ironpark/gvx-desktop/pkg/extsdk"
)

func main() {
	app := extsdk.New()
	app.View("main", extsdk.View{
		Render: func(ctx *extsdk.ViewContext, ui *extsdk.UI) extsdk.Node {
			return ui.Column(ui.Text("migrate"))
		},
	})
	app.Command("write", func(ctx *extsdk.CommandContext) error {
		return ctx.Host.SecretPut(ctx, "api_tok", "migrated-val-123")
	})
	app.Command("read", func(ctx *extsdk.CommandContext) error {
		v, ok, err := ctx.Host.SecretGet(ctx, "api_tok")
		if err != nil || !ok || v != "migrated-val-123" {
			return fmt.Errorf("SecretGet after migration = (%q, %v, %v)", v, ok, err)
		}
		return nil
	})
	_ = app.Run(context.Background())
}
`
	const id = "secret-migrate"
	extra := "\n[[contributes.command]]\nid = \"write\"\ntitle = \"Write\"\n\n[[contributes.command]]\nid = \"read\"\ntitle = \"Read\"\n"

	mgr1, h1 := newTestManager(t, WithKeychain(nil, "file"), WithDeveloperMode(true))
	e2e2CoreInstallAndStartBinary(t, mgr1, id, src, extra, ext.PermSecrets)
	if err := mgr1.RunCommand(id, "write", "en"); err != nil {
		t.Fatalf("RunCommand(write): %v", err)
	}
	secFile := ext.SecretsFilePath(h1.DataDir, id)
	assertFilePerm(t, secFile, 0o600)
	if err := mgr1.Close(5 * time.Second); err != nil {
		t.Fatalf("mgr1.Close: %v", err)
	}

	fakeKC := newFakeKeychain(t)
	mgr2, _ := newTestManager(t, WithDataDir(h1.DataDir), WithKeychain(fakeKC, ""), WithDeveloperMode(true))
	if err := mgr2.Start(context.Background()); err != nil {
		t.Fatalf("mgr2.Start: %v", err)
	}
	if err := mgr2.RunCommand(id, "read", "en"); err != nil {
		t.Fatalf("RunCommand(read): %v", err)
	}
	svc := ext.KeychainService(h1.DataDir, id)
	entries := fakeKC.Entries()
	if !slices.Contains(entries, [2]string{svc, "secret:api_tok"}) {
		t.Fatalf("expected migrated secret in Fake keychain entries, got %v", entries)
	}
	if got, err := fakeKC.Get(svc, "secret:api_tok"); err != nil || got != "migrated-val-123" {
		t.Fatalf("expected migrated secret in Fake keychain, got (%q, %v)", got, err)
	}
	if _, err := os.Stat(secFile); !os.IsNotExist(err) {
		t.Fatalf("expected fallback secrets.json to be removed after migration (err=%v)", err)
	}
}

func TestF11_T1_UninstallPurgeRemovesKeychainEntries(t *testing.T) {
	src := `package main

import (
	"context"
	"os"

	"github.com/ironpark/gvx-desktop/pkg/extsdk"
)

func main() {
	app := extsdk.New()
	app.View("main", extsdk.View{
		Render: func(ctx *extsdk.ViewContext, ui *extsdk.UI) extsdk.Node {
			return ui.Column(ui.Text("purge"))
		},
	})
	app.Command("seed", func(ctx *extsdk.CommandContext) error {
		return ctx.Host.SecretPut(ctx, "tok", "secret-for-"+os.Getenv("GVX_EXT_ID"))
	})
	_ = app.Run(context.Background())
}
`
	fakeKC := newFakeKeychain(t)
	dataDir := newTempDataDir(t)
	mgr, _ := newTestManager(t, WithDataDir(dataDir), WithKeychain(fakeKC, ""), WithDeveloperMode(true))
	extra := "\n[[contributes.command]]\nid = \"seed\"\ntitle = \"Seed\"\n"

	e2e2CoreInstallAndStartBinary(t, mgr, "purge-a", src, extra, ext.PermSecrets)
	e2e2CoreInstallAndStartBinary(t, mgr, "purge-b", src, extra, ext.PermSecrets)
	if err := mgr.RunCommand("purge-a", "seed", "en"); err != nil {
		t.Fatalf("purge-a seed: %v", err)
	}
	if err := mgr.RunCommand("purge-b", "seed", "en"); err != nil {
		t.Fatalf("purge-b seed: %v", err)
	}

	if err := mgr.Uninstall("purge-a", true); err != nil {
		t.Fatalf("Uninstall(purge-a, true): %v", err)
	}

	svcA := ext.KeychainService(dataDir, "purge-a")
	svcB := ext.KeychainService(dataDir, "purge-b")
	entries := fakeKC.Entries()
	for _, e := range entries {
		if e[0] == svcA {
			t.Fatalf("expected purge-a keychain entries purged, got %v", entries)
		}
	}
	if !slices.Contains(entries, [2]string{svcB, "secret:tok"}) {
		t.Fatalf("purge-b keychain entry missing from %v", entries)
	}
	if got, err := fakeKC.Get(svcB, "secret:tok"); err != nil || got != "secret-for-purge-b" {
		t.Fatalf("purge-b keychain entry affected: (%q, %v)", got, err)
	}
	if _, err := os.Stat(ext.ExtensionDataDir(dataDir, "purge-a")); !os.IsNotExist(err) {
		t.Fatalf("expected extension-data/purge-a deleted (err=%v)", err)
	}
}

func TestF11_T1_SettingsAndSecretMasking(t *testing.T) {
	src := `package main

import (
	"context"
	"fmt"

	"github.com/ironpark/gvx-desktop/pkg/extsdk"
)

func main() {
	app := extsdk.New()
	app.View("main", extsdk.View{
		Render: func(ctx *extsdk.ViewContext, ui *extsdk.UI) extsdk.Node {
			return ui.Column(ui.Text("settings"))
		},
	})
	app.Command("inspect", func(ctx *extsdk.CommandContext) error {
		st, err := ctx.Host.Settings(ctx)
		if err != nil {
			return err
		}
		if st.String("endpoint") != "https://api.example.invalid" {
			return fmt.Errorf("endpoint = %q", st.String("endpoint"))
		}
		if !st.Bool("enabled") || st.Int("workers") != 4 || st.String("mode") != "fast" {
			return fmt.Errorf("unexpected typed settings: %v", st.Raw())
		}
		if !st.IsSet("api_token") {
			return fmt.Errorf("expected api_token masked as set=true, got %v", st.Raw()["api_token"])
		}
		m, ok := st.Raw()["api_token"].(map[string]any)
		if !ok || m["set"] != true || len(m) != 1 {
			return fmt.Errorf("secret setting not strictly masked as {set:true}: %v", st.Raw()["api_token"])
		}
		val, found, err := ctx.Host.SecretGet(ctx, "setting:api_token")
		if err != nil || !found || val != "pat-secret-xyz" {
			return fmt.Errorf("SecretGet(setting:api_token) = (%q, %v, %v)", val, found, err)
		}
		return nil
	})
	_ = app.Run(context.Background())
}
`
	extra := `
[[contributes.command]]
id = "inspect"
title = "Inspect"

[contributes.settings]
endpoint = { type = "string", label = "Endpoint", default = "https://example.invalid" }
api_token = { type = "secret", label = "API Token" }
enabled = { type = "boolean", label = "Enabled", default = false }
workers = { type = "number", label = "Workers", default = 1 }
mode = { type = "select", label = "Mode", options = ["fast", "safe"], default = "safe" }
`
	mgr, h := newTestManager(t, WithDeveloperMode(true))
	const id = "settings-mask"
	e2e2CoreInstallAndStartBinary(t, mgr, id, src, extra, ext.PermSecrets)

	for k, v := range map[string]string{
		"endpoint":  "https://api.example.invalid",
		"api_token": "pat-secret-xyz",
		"enabled":   "true",
		"workers":   "4",
		"mode":      "fast",
	} {
		if err := mgr.SetSetting(id, k, v); err != nil {
			t.Fatalf("SetSetting(%s): %v", k, err)
		}
	}

	assertFilePerm(t, ext.SettingsPath(h.DataDir, id), 0o600)
	rawSettings, err := os.ReadFile(ext.SettingsPath(h.DataDir, id))
	if err != nil {
		t.Fatalf("read settings.json: %v", err)
	}
	if strings.Contains(string(rawSettings), "pat-secret-xyz") {
		t.Fatal("secret setting leaked into settings.json")
	}

	info := e2e2CoreMustExtension(t, mgr, id)
	for _, s := range info.Settings {
		if s.Key == "api_token" && (!s.Set || s.Value != "") {
			t.Fatalf("ExtensionInfo.Settings exposed secret value or wrong Set: %+v", s)
		}
	}

	if err := mgr.RunCommand(id, "inspect", "en"); err != nil {
		t.Fatalf("RunCommand(inspect): %v", err)
	}
}

func TestF11_T2_RejectsSecretOver2048BytesOrNonUTF8(t *testing.T) {
	src := `package main

import (
	"context"
	"fmt"
	"strings"

	"github.com/ironpark/gvx-desktop/pkg/extsdk"
)

func main() {
	app := extsdk.New()
	app.View("main", extsdk.View{
		Render: func(ctx *extsdk.ViewContext, ui *extsdk.UI) extsdk.Node {
			return ui.Column(ui.Text("bounds"))
		},
	})
	app.Command("test-bounds", func(ctx *extsdk.CommandContext) error {
		if err := ctx.Host.SecretPut(ctx, "exact", strings.Repeat("S", 2048)); err != nil {
			return fmt.Errorf("2048B secret failed: %w", err)
		}
		if err := ctx.Host.SecretPut(ctx, "toolong", strings.Repeat("S", 2049)); err == nil {
			return fmt.Errorf("2049B secret succeeded")
		}
		if err := ctx.Host.SecretPut(ctx, "badutf8", string([]byte{0xff, 0xfe})); err == nil {
			return fmt.Errorf("invalid UTF-8 secret succeeded")
		}
		return nil
	})
	_ = app.Run(context.Background())
}
`
	mgr, _ := newTestManager(t, WithDeveloperMode(true))
	const id = "secret-bounds"
	e2e2CoreInstallAndStartBinary(t, mgr, id, src, "\n[[contributes.command]]\nid = \"test-bounds\"\ntitle = \"Test Bounds\"\n", ext.PermSecrets)

	if err := mgr.RunCommand(id, "test-bounds", "en"); err != nil {
		t.Fatalf("RunCommand(test-bounds): %v", err)
	}
}

func TestF11_T2_EnforcesMax32SecretsPerExtension(t *testing.T) {
	src := `package main

import (
	"context"
	"fmt"

	"github.com/ironpark/gvx-desktop/pkg/extsdk"
)

func main() {
	app := extsdk.New()
	app.View("main", extsdk.View{
		Render: func(ctx *extsdk.ViewContext, ui *extsdk.UI) extsdk.Node {
			return ui.Column(ui.Text("max32"))
		},
	})
	app.Command("fill32", func(ctx *extsdk.CommandContext) error {
		for i := 0; i < 32; i++ {
			if err := ctx.Host.SecretPut(ctx, fmt.Sprintf("sec_%02d", i), "val"); err != nil {
				return fmt.Errorf("secret %d failed: %w", i, err)
			}
		}
		if err := ctx.Host.SecretPut(ctx, "sec_32", "overflow"); err == nil {
			return fmt.Errorf("33rd secret unexpectedly succeeded")
		}
		return nil
	})
	_ = app.Run(context.Background())
}
`
	extra := `
[[contributes.command]]
id = "fill32"
title = "Fill 32"

[contributes.settings]
cfg_secret = { type = "secret", label = "Config Secret" }
`
	mgr, _ := newTestManager(t, WithDeveloperMode(true))
	const id = "secret-max32"
	e2e2CoreInstallAndStartBinary(t, mgr, id, src, extra, ext.PermSecrets)

	// A setting: secret must not consume any of the 32 runtime secret slots.
	if err := mgr.SetSetting(id, "cfg_secret", "setting-val"); err != nil {
		t.Fatalf("SetSetting(cfg_secret): %v", err)
	}
	if err := mgr.RunCommand(id, "fill32", "en"); err != nil {
		t.Fatalf("RunCommand(fill32): %v", err)
	}
}

func TestF11_T2_Enforces60RPMRateLimit(t *testing.T) {
	src := `package main

import (
	"context"
	"fmt"

	"github.com/ironpark/gvx-desktop/pkg/extsdk"
)

func main() {
	app := extsdk.New()
	app.View("main", extsdk.View{
		Render: func(ctx *extsdk.ViewContext, ui *extsdk.UI) extsdk.Node {
			return ui.Column(ui.Text("ratelimit"))
		},
	})
	app.Command("burst", func(ctx *extsdk.CommandContext) error {
		if err := ctx.Host.SecretPut(ctx, "rate_key", "v"); err != nil {
			return err
		}
		for i := 1; i <= 64; i++ {
			_, _, err := ctx.Host.SecretGet(ctx, "rate_key")
			if extsdk.IsRateLimited(err) {
				return nil
			}
			if err != nil {
				return fmt.Errorf("unexpected non-429 error on call %d: %w", i, err)
			}
		}
		return fmt.Errorf("65 secret calls within 1 minute did not trigger 429 rate limit")
	})
	_ = app.Run(context.Background())
}
`
	mgr, _ := newTestManager(t, WithDeveloperMode(true))
	const id = "secret-ratelimit"
	e2e2CoreInstallAndStartBinary(t, mgr, id, src, "\n[[contributes.command]]\nid = \"burst\"\ntitle = \"Burst\"\n", ext.PermSecrets)

	if err := mgr.RunCommand(id, "burst", "en"); err != nil {
		t.Fatalf("RunCommand(burst): %v", err)
	}
}

func TestF11_T2_Returns403WithoutSecretsPermission(t *testing.T) {
	src := `package main

import (
	"context"
	"fmt"

	"github.com/ironpark/gvx-desktop/pkg/extsdk"
)

func main() {
	app := extsdk.New()
	app.View("main", extsdk.View{
		Render: func(ctx *extsdk.ViewContext, ui *extsdk.UI) extsdk.Node {
			return ui.Column(ui.Text("no-secrets"))
		},
	})
	app.Command("try-secrets", func(ctx *extsdk.CommandContext) error {
		if _, err := ctx.Host.SecretList(ctx); !extsdk.IsPermissionDenied(err) {
			return fmt.Errorf("SecretList err = %v, want 403", err)
		}
		if _, _, err := ctx.Host.SecretGet(ctx, "k"); !extsdk.IsPermissionDenied(err) {
			return fmt.Errorf("SecretGet err = %v, want 403", err)
		}
		if err := ctx.Host.SecretPut(ctx, "k", "v"); !extsdk.IsPermissionDenied(err) {
			return fmt.Errorf("SecretPut err = %v, want 403", err)
		}
		if err := ctx.Host.SecretDelete(ctx, "k"); !extsdk.IsPermissionDenied(err) {
			return fmt.Errorf("SecretDelete err = %v, want 403", err)
		}
		if _, _, err := ctx.Host.SecretGet(ctx, "setting:api_token"); !extsdk.IsPermissionDenied(err) {
			return fmt.Errorf("SecretGet(setting:api_token) err = %v, want 403", err)
		}
		return nil
	})
	_ = app.Run(context.Background())
}
`
	extra := `
[[contributes.command]]
id = "try-secrets"
title = "Try Secrets"

[contributes.settings]
api_token = { type = "secret", label = "API Token" }
`
	mgr, _ := newTestManager(t, WithDeveloperMode(true))
	const id = "no-secrets-perm"
	e2e2CoreInstallAndStartBinary(t, mgr, id, src, extra)
	if err := mgr.SetSetting(id, "api_token", "hidden-token"); err != nil {
		t.Fatalf("SetSetting(api_token): %v", err)
	}
	if err := mgr.RunCommand(id, "try-secrets", "en"); err != nil {
		t.Fatalf("RunCommand(try-secrets): %v", err)
	}
}

func TestF11_T2_SecretsNeverLeakInSettingsJSONLogsOrGetSettings(t *testing.T) {
	const secretSettingVal = "super-secret-setting-xyz-987"
	const secretRuntimeVal = "super-secret-runtime-abc-654"

	src := fmt.Sprintf(`package main

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/ironpark/gvx-desktop/pkg/extsdk"
)

func main() {
	app := extsdk.New()
	app.View("main", extsdk.View{
		Render: func(ctx *extsdk.ViewContext, ui *extsdk.UI) extsdk.Node {
			return ui.Column(ui.Text("no-leak"))
		},
	})
	app.Command("check", func(ctx *extsdk.CommandContext) error {
		if err := ctx.Host.SecretPut(ctx, "runtime_tok", %q); err != nil {
			return err
		}
		st, err := ctx.Host.Settings(ctx)
		if err != nil {
			return err
		}
		b, _ := json.Marshal(st.Raw())
		if strings.Contains(string(b), %q) {
			return fmt.Errorf("GET /v1/settings leaked secret value")
		}
		return nil
	})
	_ = app.Run(context.Background())
}
`, secretRuntimeVal, secretSettingVal)

	extra := `
[[contributes.command]]
id = "check"
title = "Check"

[contributes.settings]
api_key = { type = "secret", label = "API Key" }
`
	mgr, h := newTestManager(t, WithDeveloperMode(true))
	const id = "secret-no-leak"
	e2e2CoreInstallAndStartBinary(t, mgr, id, src, extra, ext.PermSecrets)
	if err := mgr.SetSetting(id, "api_key", secretSettingVal); err != nil {
		t.Fatalf("SetSetting: %v", err)
	}
	if err := mgr.RunCommand(id, "check", "en"); err != nil {
		t.Fatalf("RunCommand(check): %v", err)
	}

	info := e2e2CoreMustExtension(t, mgr, id)
	infoBytes, _ := json.Marshal(info)
	if strings.Contains(string(infoBytes), secretSettingVal) || strings.Contains(string(infoBytes), secretRuntimeVal) {
		t.Fatal("ExtensionInfo leaked secret value")
	}

	for _, path := range []string{
		ext.SettingsPath(h.DataDir, id),
		ext.SecretsIndexPath(h.DataDir, id),
	} {
		b, err := os.ReadFile(path)
		if err == nil {
			if strings.Contains(string(b), secretSettingVal) || strings.Contains(string(b), secretRuntimeVal) {
				t.Fatalf("file %s leaked secret value", path)
			}
		}
	}
	logFiles, _ := filepath.Glob(filepath.Join(ext.LogsDir(h.DataDir, id), "*"))
	for _, lf := range logFiles {
		b, err := os.ReadFile(lf)
		if err == nil && (strings.Contains(string(b), secretSettingVal) || strings.Contains(string(b), secretRuntimeVal)) {
			t.Fatalf("log file %s leaked secret value", lf)
		}
	}
}

// =============================================================================
// Feature 12: 3 Install Sources & BuildKit Builder
// =============================================================================

func TestF12_T1_InstallLocalEntersWaitingWithoutAutoBuild(t *testing.T) {
	mgr, h := newTestManager(t)
	const id = "local-wait-ext"
	localDir := createLocalExtensionDir(t, t.TempDir(), id, minimalManifestTOML(id, "0.1.0", ext.PermStorage), nil)

	info, err := mgr.InstallLocal(localDir)
	if err != nil {
		t.Fatalf("InstallLocal: %v", err)
	}
	if info.State != ext.StateWaiting {
		t.Fatalf("State = %q, want %q", info.State, ext.StateWaiting)
	}
	if len(h.ImageBuilder.SnapshotCalls()) != 0 {
		t.Fatalf("InstallLocal unexpectedly triggered %d image builds", len(h.ImageBuilder.SnapshotCalls()))
	}
	spec, err := ext.LoadSource(ext.SourcePath(h.DataDir, id))
	if err != nil {
		t.Fatalf("LoadSource: %v", err)
	}
	if spec.Kind != ext.SourceLocal || spec.LocalPath == "" {
		t.Fatalf("unexpected SourceSpec: %+v", spec)
	}
}

func TestF12_T1_InstallGitShallowClonesAndEntersWaiting(t *testing.T) {
	const id = "git-wait-ext"
	bareURL, sha := initBareGitRepo(t, map[string]string{
		"extension.toml": minimalManifestTOML(id, "0.1.0", ext.PermStorage),
		"Dockerfile":     "FROM alpine:3.20\nCMD [\"/bin/sh\"]\n",
		"hello.txt":      "from-git\n",
	})
	bareDir := strings.TrimPrefix(bareURL, "file://")
	httpsURL := e2e2ServeGitHTTPS(t, bareDir)

	mgr, h := newTestManager(t)
	info, err := mgr.InstallGit(httpsURL, "main")
	if err != nil {
		t.Fatalf("InstallGit: %v", err)
	}
	if info.State != ext.StateWaiting {
		t.Fatalf("State = %q, want %q", info.State, ext.StateWaiting)
	}
	if len(h.ImageBuilder.SnapshotCalls()) != 0 {
		t.Fatalf("InstallGit unexpectedly triggered %d image builds", len(h.ImageBuilder.SnapshotCalls()))
	}
	srcDir := ext.SourceCheckoutDir(h.DataDir, id)
	if _, err := os.Stat(filepath.Join(srcDir, "hello.txt")); err != nil {
		t.Fatalf("expected cloned file in %s: %v", srcDir, err)
	}
	spec, err := ext.LoadSource(ext.SourcePath(h.DataDir, id))
	if err != nil {
		t.Fatalf("LoadSource: %v", err)
	}
	if spec.Kind != ext.SourceGit || spec.Commit != sha || spec.Ref != "main" {
		t.Fatalf("unexpected git SourceSpec: %+v (want commit %s)", spec, sha)
	}
}

func TestF12_T1_InstallImagePullsOnlyAfterApproval(t *testing.T) {
	mgr, h := newTestManager(t)
	const ref = "ghcr.io/acme/prebuilt-ext:1.2.3"

	info, err := mgr.InstallImage(ref)
	if err != nil {
		t.Fatalf("InstallImage: %v", err)
	}
	if info.State != ext.StateWaiting {
		t.Fatalf("State after InstallImage = %q, want %q", info.State, ext.StateWaiting)
	}
	if refs := h.ImagePuller.SnapshotPulledRefs(); len(refs) != 0 {
		t.Fatalf("InstallImage pulled before approval: %v", refs)
	}

	_ = mgr.AllowAndBuild(info.ID)
	refs := h.ImagePuller.SnapshotPulledRefs()
	if !slices.Contains(refs, ref) {
		t.Fatalf("expected ImagePuller.Pull called with %q after approval, got %v", ref, refs)
	}
}

func TestF12_T1_AllowAndBuildPacksDockerignoreAndCallsImageBuilder(t *testing.T) {
	// Verify landed BuildFromDir + PackBuildContext .dockerignore rules directly.
	const id = "build-ctx-ext"
	const commitSHA = "1234567890abcdef1234567890abcdef12345678"
	srcDir := createLocalExtensionDir(t, t.TempDir(), id, minimalManifestTOML(id, "0.2.0"), map[string]string{
		".dockerignore": "*.log\n!keep.log\ndist/\n",
		"keep.log":      "keep\n",
		"drop.log":      "drop\n",
		"dist/out.bin":  "ignored\n",
		"app/main.go":   "package main\n",
	})
	m, err := ext.ParseManifestDir(srcDir, "")
	if err != nil {
		t.Fatalf("ParseManifestDir: %v", err)
	}

	builder := &e2e2ImageBuilder{ImageID: "sha256:built123"}
	res, err := ext.BuildFromDir(context.Background(), builder, srcDir, m, commitSHA, nil)
	if err != nil {
		t.Fatalf("BuildFromDir: %v", err)
	}
	wantTag := ext.ExtensionImageTag(id, "0.2.0", commitSHA)
	if wantTag != "gvx-ext/build-ctx-ext:0.2.0-12345678" {
		t.Fatalf("ExtensionImageTag = %q", wantTag)
	}
	if !slices.Equal(res.Tags, []string{wantTag}) || len(builder.Calls) != 1 {
		t.Fatalf("unexpected build result=%+v calls=%d", res, len(builder.Calls))
	}
	if builder.Calls[0].Labels["io.gvx.extension"] != id {
		t.Fatalf("missing io.gvx.extension label: %+v", builder.Calls[0].Labels)
	}
	entries := e2e2TarEntries(t, builder.ContextTars[0])
	if !entries["Dockerfile"] || !entries["keep.log"] || !entries["app/main.go"] {
		t.Fatalf("expected Dockerfile/keep.log/app/main.go in tar, got %v", entries)
	}
	if entries["drop.log"] || entries["dist/out.bin"] || entries["dist"] {
		t.Fatalf(".dockerignore failed to exclude drop.log/dist: %v", entries)
	}

	// Also exercise Manager.InstallLocal -> AllowAndBuild -> h.ImageBuilder.
	mgr, h := newTestManager(t)
	if _, err := mgr.InstallLocal(srcDir); err != nil {
		t.Fatalf("InstallLocal: %v", err)
	}
	_ = mgr.AllowAndBuild(id)
	if len(h.ImageBuilder.SnapshotCalls()) == 0 {
		t.Fatal("expected AllowAndBuild to invoke h.ImageBuilder")
	}
}

func TestF12_T1_RebuildReRunsBuildAndUpdatesImageTag(t *testing.T) {
	mgr, h := newTestManager(t)
	const id = "rebuild-ext"
	localDir := createLocalExtensionDir(t, t.TempDir(), id, minimalManifestTOML(id, "0.1.0"), nil)

	if _, err := mgr.InstallLocal(localDir); err != nil {
		t.Fatalf("InstallLocal: %v", err)
	}
	_ = mgr.AllowAndBuild(id)
	calls1 := len(h.ImageBuilder.SnapshotCalls())
	if calls1 == 0 {
		t.Fatal("expected initial AllowAndBuild to invoke ImageBuilder")
	}

	if err := os.WriteFile(filepath.Join(localDir, "extra.txt"), []byte("v2\n"), 0o644); err != nil {
		t.Fatalf("write extra.txt: %v", err)
	}
	_ = mgr.Rebuild(id)
	calls2 := len(h.ImageBuilder.SnapshotCalls())
	if calls2 <= calls1 {
		t.Fatalf("expected Rebuild to invoke ImageBuilder again: before=%d after=%d", calls1, calls2)
	}
}

func TestF12_T2_NeverAutoBuildsBeforeAllowAndBuild(t *testing.T) {
	const idGit = "no-autobuild-git"
	bareURL, _ := initBareGitRepo(t, map[string]string{
		"extension.toml": minimalManifestTOML(idGit, "0.1.0", ext.PermDocker),
		"Dockerfile":     "FROM alpine:3.20\nCMD [\"/bin/sh\"]\n",
	})
	httpsURL := e2e2ServeGitHTTPS(t, strings.TrimPrefix(bareURL, "file://"))

	mgr, h := newTestManager(t)
	if _, err := mgr.InstallGit(httpsURL, "main"); err != nil {
		t.Fatalf("InstallGit: %v", err)
	}
	const idLocal = "no-autobuild-local"
	localDir := createLocalExtensionDir(t, t.TempDir(), idLocal, minimalManifestTOML(idLocal, "0.1.0", ext.PermDocker), nil)
	if _, err := mgr.InstallLocal(localDir); err != nil {
		t.Fatalf("InstallLocal: %v", err)
	}

	_ = mgr.Start(context.Background())
	if err := mgr.Enable(idGit); err == nil {
		t.Fatal("Enable succeeded for unapproved git extension")
	}
	if err := mgr.Enable(idLocal); err == nil {
		t.Fatal("Enable succeeded for unapproved local extension")
	}
	if len(h.ImageBuilder.SnapshotCalls()) != 0 {
		t.Fatalf("expected 0 image builds before approval, got %d", len(h.ImageBuilder.SnapshotCalls()))
	}
	if len(h.ImagePuller.SnapshotPulledRefs()) != 0 {
		t.Fatalf("expected 0 image pulls before approval, got %d", len(h.ImagePuller.SnapshotPulledRefs()))
	}
}

func TestF12_T2_ExcludesDotGitFromBuildContextTar(t *testing.T) {
	dir := t.TempDir()
	writeExtFixture(t, dir, minimalManifestTOML("dotgit-ext", "0.1.0"), map[string]string{
		"Dockerfile":    "FROM alpine:3.20\n",
		".dockerignore": "!.git\n!.git/HEAD\n",
		".git/HEAD":     "ref: refs/heads/main\n",
		".git/config":   "[core]\n\tbare = false\n",
	})

	var buf bytes.Buffer
	if err := ext.PackBuildContext(context.Background(), dir, "Dockerfile", &buf); err != nil {
		t.Fatalf("PackBuildContext: %v", err)
	}
	entries := e2e2TarEntries(t, buf.Bytes())
	if !entries["Dockerfile"] {
		t.Fatal("expected Dockerfile in build context tar")
	}
	for name := range entries {
		if name == ".git" || strings.HasPrefix(name, ".git/") {
			t.Fatalf(".git entry %q leaked into build context tar despite unconditional exclusion", name)
		}
	}
}

func TestF12_T2_RejectsBuildContextOver512MiB(t *testing.T) {
	const id = "huge-ctx-ext"
	dir := createLocalExtensionDir(t, t.TempDir(), id, minimalManifestTOML(id, "0.1.0"), nil)
	bigFile := filepath.Join(dir, "huge.bin")
	f, err := os.Create(bigFile)
	if err != nil {
		t.Fatalf("os.Create: %v", err)
	}
	if err := f.Truncate(ext.MaxBuildContextBytes + 1); err != nil {
		_ = f.Close()
		t.Fatalf("Truncate: %v", err)
	}
	_ = f.Close()

	var buf bytes.Buffer
	if err := ext.PackBuildContext(context.Background(), dir, "Dockerfile", &buf); err == nil {
		t.Fatal("PackBuildContext unexpectedly accepted >512 MiB context")
	}

	m, err := ext.ParseManifestDir(dir, "")
	if err != nil {
		t.Fatalf("ParseManifestDir: %v", err)
	}
	builder := &e2e2ImageBuilder{}
	if _, err := ext.BuildFromDir(context.Background(), builder, dir, m, "", nil); err == nil {
		t.Fatal("BuildFromDir unexpectedly succeeded for >512 MiB context")
	}
	if len(builder.Calls) != 0 {
		t.Fatalf("expected 0 completed build calls on oversized context, got %d", len(builder.Calls))
	}
}

func TestF12_T2_RejectsSymlinkEscapeInBuildContext(t *testing.T) {
	outsideDir := t.TempDir()
	outsideSecret := filepath.Join(outsideDir, "host-secret.txt")
	if err := os.WriteFile(outsideSecret, []byte("secret"), 0o600); err != nil {
		t.Fatalf("write outside file: %v", err)
	}

	const id = "sym-ctx-ext"
	dir := createLocalExtensionDir(t, t.TempDir(), id, minimalManifestTOML(id, "0.1.0"), nil)
	if err := os.Symlink(outsideSecret, filepath.Join(dir, "escape.txt")); err != nil {
		t.Fatalf("os.Symlink: %v", err)
	}

	var buf bytes.Buffer
	if err := ext.PackBuildContext(context.Background(), dir, "Dockerfile", &buf); err == nil {
		t.Fatal("PackBuildContext unexpectedly accepted symlink escaping build context")
	}
}

func TestF12_T2_FailedBuildPreservesLogTailAndErrorState(t *testing.T) {
	const id = "fail-build-ext"
	dir := createLocalExtensionDir(t, t.TempDir(), id, minimalManifestTOML(id, "0.1.0"), nil)
	m, err := ext.ParseManifestDir(dir, "")
	if err != nil {
		t.Fatalf("ParseManifestDir: %v", err)
	}

	builder := &e2e2ImageBuilder{
		LogLines: []string{"Step 1/2 : FROM alpine:3.20", "ERROR boom"},
		BuildErr: errors.New("exit code 1"),
	}
	var capturedLogs []string
	if _, err := ext.BuildFromDir(context.Background(), builder, dir, m, "", func(line string) {
		capturedLogs = append(capturedLogs, line)
	}); err == nil {
		t.Fatal("BuildFromDir expected failure")
	}
	if !slices.Contains(capturedLogs, "ERROR boom") {
		t.Fatalf("expected streamed log tail to contain 'ERROR boom', got %v", capturedLogs)
	}

	mgr, h := newTestManager(t)
	h.ImageBuilder.LogLines = []string{"Step 1/2 : FROM alpine:3.20", "ERROR boom"}
	h.ImageBuilder.BuildErr = errors.New("build failed: ERROR boom")

	if _, err := mgr.InstallLocal(dir); err != nil {
		t.Fatalf("InstallLocal: %v", err)
	}
	if err := mgr.AllowAndBuild(id); err == nil {
		t.Fatal("expected AllowAndBuild to return error when ImageBuilder fails")
	}
	info := e2e2CoreMustExtension(t, mgr, id)
	if info.State == ext.StateRunning {
		t.Fatalf("failed build must not reach StateRunning, got %q", info.State)
	}
	if !strings.Contains(info.Error, "ERROR boom") && !strings.Contains(info.Error, "build failed") {
		t.Fatalf("expected ExtensionInfo.Error to preserve failure detail, got %q", info.Error)
	}
}
