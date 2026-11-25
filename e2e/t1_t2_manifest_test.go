//go:build e2e

package e2e

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/ironpark/gvx-desktop/internal/ext"
	"github.com/ironpark/gvx-desktop/internal/keychain"
	"github.com/ironpark/gvx-desktop/internal/store"
	"github.com/ironpark/gvx-desktop/internal/uiblock"
)

const minimalSDKBinarySource = `package main

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

func createLocalBinaryExtensionDir(t *testing.T, root, id string, binData []byte, extraTOML string, perms ...string) string {
	t.Helper()
	if binData == nil {
		binPath := buildFixtureSDKBinary(t, minimalSDKBinarySource)
		var err error
		binData, err = os.ReadFile(binPath)
		if err != nil {
			t.Fatalf("read fixture binary: %v", err)
		}
	}
	extDir := filepath.Join(root, id)
	manifest := binaryManifestTOML(id, "0.1.0", "bin/ext", perms...) + extraTOML
	writeExtFixture(t, extDir, manifest, nil)
	if err := os.MkdirAll(filepath.Join(extDir, "bin"), 0o700); err != nil {
		t.Fatalf("mkdir bin: %v", err)
	}
	if err := os.WriteFile(filepath.Join(extDir, "bin", "ext"), binData, 0o755); err != nil {
		t.Fatalf("write dev_entrypoint: %v", err)
	}
	return extDir
}

func tomlQuote(s string) string {
	var b strings.Builder
	b.WriteByte('"')
	for _, r := range s {
		switch r {
		case '\\':
			b.WriteString(`\\`)
		case '"':
			b.WriteString(`\"`)
		case '\b':
			b.WriteString(`\b`)
		case '\t':
			b.WriteString(`\t`)
		case '\n':
			b.WriteString(`\n`)
		case '\f':
			b.WriteString(`\f`)
		case '\r':
			b.WriteString(`\r`)
		default:
			if r < 0x20 || r == 0x7f {
				fmt.Fprintf(&b, `\u%04x`, r)
			} else {
				b.WriteRune(r)
			}
		}
	}
	b.WriteByte('"')
	return b.String()
}

// =============================================================================
// Feature 5: Extension Data Dir Layout (store.DefaultDir() / -data-dir)
// =============================================================================

func TestF5_T1_CustomDataDirCreatesGrantsAndLayout(t *testing.T) {
	mgr, h := newTestManager(t, WithDeveloperMode(true))
	extDir := createLocalBinaryExtensionDir(t, t.TempDir(), "alpha-ext", nil, "", "storage")

	if _, err := mgr.InstallLocal(extDir); err != nil {
		t.Fatalf("InstallLocal: %v", err)
	}
	if err := mgr.AllowAndBuild("alpha-ext"); err != nil {
		t.Fatalf("AllowAndBuild: %v", err)
	}

	for _, p := range []string{
		filepath.Join(h.DataDir, "extension-grants.json"),
		filepath.Join(h.DataDir, "extensions", "alpha-ext", "extension.toml"),
		filepath.Join(h.DataDir, "extensions", "alpha-ext", "source.json"),
	} {
		if _, err := os.Stat(p); err != nil {
			t.Fatalf("expected %s to exist: %v", p, err)
		}
	}
}

func TestF5_T1_GitInstallCreatesSrcCheckout(t *testing.T) {
	mgr, h := newTestManager(t)
	bareURL, sha := initBareGitRepo(t, map[string]string{
		"extension.toml": minimalManifestTOML("git-ext", "0.1.0"),
		"Dockerfile":     "FROM alpine:3.20\nCMD [\"/bin/sh\"]\n",
		"README.md":      "# git-ext\n",
	})
	httpsURL := serveGitHTTPS(t, strings.TrimPrefix(bareURL, "file://"))

	info, err := mgr.InstallGit(httpsURL, "main")
	if err != nil {
		t.Fatalf("InstallGit: %v", err)
	}
	if info.ID != "git-ext" {
		t.Fatalf("InstallGit ID = %q, want git-ext", info.ID)
	}
	srcCheckout := filepath.Join(h.DataDir, "extensions", "git-ext", "src")
	if _, err := os.Stat(filepath.Join(srcCheckout, "README.md")); err != nil {
		t.Fatalf("expected git checkout in %s: %v", srcCheckout, err)
	}
	srcMeta, err := os.ReadFile(filepath.Join(h.DataDir, "extensions", "git-ext", "source.json"))
	if err != nil {
		t.Fatalf("read source.json: %v", err)
	}
	if !strings.Contains(string(srcMeta), sha) {
		t.Fatalf("source.json does not record commit SHA %q: %s", sha, string(srcMeta))
	}

	// Local install should NOT create src/ checkout directory.
	localDir := createLocalExtensionDir(t, t.TempDir(), "local-ext", minimalManifestTOML("local-ext", "0.1.0"), nil)
	if _, err := mgr.InstallLocal(localDir); err != nil {
		t.Fatalf("InstallLocal: %v", err)
	}
	if _, err := os.Stat(filepath.Join(h.DataDir, "extensions", "local-ext", "src")); !os.IsNotExist(err) {
		t.Fatalf("InstallLocal should not create src/ directory (err=%v)", err)
	}
}

func TestF5_T1_ExtensionDataSubdirLayout(t *testing.T) {
	binPath := buildFixtureSDKBinary(t, `package main

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
			return ui.Column(ui.Text("ready"))
		},
	})
	app.Command("seed", func(ctx *extsdk.CommandContext) error {
		if err := ctx.Host.StoragePut(ctx, "k", "v"); err != nil {
			return err
		}
		return ctx.Host.SecretPut(ctx, "s", "topsecret")
	})
	fmt.Fprintln(os.Stderr, "fixture started")
	_ = app.Run(context.Background())
}
`)
	mgr, h := newTestManager(t, WithDeveloperMode(true))
	manifest := `schema_version = 1
id = "data-layout"
version = "0.1.0"
publisher = "acme"
name = "Data Layout"
description = "Layout verification"
icon = "box"
permissions = ["storage", "secrets"]

[runtime]
type = "binary"
dev_entrypoint = "bin/ext"

[[contributes.view]]
id = "main"
title = "Main"

[[contributes.command]]
id = "seed"
title = "Seed"

[contributes.settings]
label = { type = "string", label = "Label", default = "init" }
`
	extDir := writeExtFixture(t, filepath.Join(t.TempDir(), "data-layout"), manifest, nil)
	binData, err := os.ReadFile(binPath)
	if err != nil {
		t.Fatalf("read fixture binary: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(extDir, "bin"), 0o755); err != nil {
		t.Fatalf("mkdir bin: %v", err)
	}
	if err := os.WriteFile(filepath.Join(extDir, "bin", "ext"), binData, 0o755); err != nil {
		t.Fatalf("write dev_entrypoint: %v", err)
	}

	if _, err := mgr.InstallLocal(extDir); err != nil {
		t.Fatalf("InstallLocal: %v", err)
	}
	if err := mgr.AllowAndBuild("data-layout"); err != nil {
		t.Fatalf("AllowAndBuild: %v", err)
	}
	if err := mgr.SetSetting("data-layout", "label", "updated"); err != nil {
		t.Fatalf("SetSetting: %v", err)
	}
	if err := mgr.RunCommand("data-layout", "seed", "en"); err != nil {
		t.Fatalf("RunCommand(seed): %v", err)
	}

	base := filepath.Join(h.DataDir, "extension-data", "data-layout")
	for _, rel := range []string{"settings.json", "storage.json", "secrets-index.json", "files", "logs"} {
		if _, err := os.Stat(filepath.Join(base, rel)); err != nil {
			t.Fatalf("expected %s under extension-data: %v", rel, err)
		}
	}
}

func TestF5_T1_DirectoryAndFilePermissions(t *testing.T) {
	binPath := buildFixtureSDKBinary(t, `package main

import (
	"context"

	"github.com/ironpark/gvx-desktop/pkg/extsdk"
)

func main() {
	app := extsdk.New()
	app.View("main", extsdk.View{
		Render: func(ctx *extsdk.ViewContext, ui *extsdk.UI) extsdk.Node {
			return ui.Column(ui.Text("perm-ready"))
		},
	})
	app.Command("seed", func(ctx *extsdk.CommandContext) error {
		if err := ctx.Host.StoragePut(ctx, "k", "v"); err != nil {
			return err
		}
		return ctx.Host.SecretPut(ctx, "s", "topsecret")
	})
	_ = app.Run(context.Background())
}
`)
	binData, err := os.ReadFile(binPath)
	if err != nil {
		t.Fatalf("read fixture binary: %v", err)
	}

	t.Setenv(keychain.EnvVar, "file")
	mgr, h := newTestManager(t, WithKeychain(nil, "unavailable"), WithDeveloperMode(true))
	if h.Keychain != nil {
		t.Fatalf("expected TestHarness.Keychain == nil when WithKeychain(nil, ...) is passed")
	}

	extraTOML := `
[[contributes.command]]
id = "seed"
title = "Seed"

[contributes.settings]
label = { type = "string", label = "Label", default = "init" }
`
	extDir := createLocalBinaryExtensionDir(t, t.TempDir(), "perm-ext", binData, extraTOML, "storage", "secrets")
	if _, err := mgr.InstallLocal(extDir); err != nil {
		t.Fatalf("InstallLocal: %v", err)
	}
	if err := mgr.AllowAndBuild("perm-ext"); err != nil {
		t.Fatalf("AllowAndBuild: %v", err)
	}
	if err := mgr.SetSetting("perm-ext", "label", "updated"); err != nil {
		t.Fatalf("SetSetting: %v", err)
	}
	if err := mgr.RunCommand("perm-ext", "seed", "en"); err != nil {
		t.Fatalf("RunCommand(seed): %v", err)
	}

	assertDirPerm(t, filepath.Join(h.DataDir, "extensions", "perm-ext"), 0o700)
	assertDirPerm(t, filepath.Join(h.DataDir, "extension-data", "perm-ext"), 0o700)
	assertDirPerm(t, filepath.Join(h.DataDir, "extension-data", "perm-ext", "files"), 0o700)
	assertDirPerm(t, filepath.Join(h.DataDir, "extension-data", "perm-ext", "logs"), 0o700)
	assertFilePerm(t, filepath.Join(h.DataDir, "extension-grants.json"), 0o600)
	assertFilePerm(t, filepath.Join(h.DataDir, "extension-data", "perm-ext", "settings.json"), 0o600)
	assertFilePerm(t, filepath.Join(h.DataDir, "extension-data", "perm-ext", "storage.json"), 0o600)
	assertFilePerm(t, filepath.Join(h.DataDir, "extension-data", "perm-ext", "secrets.json"), 0o600)

	// Verify keychain-backed mode writes secrets-index.json with 0600 permissions.
	mgrKC, hKC := newTestManager(t, WithDeveloperMode(true))
	kcDir := createLocalBinaryExtensionDir(t, t.TempDir(), "perm-kc-ext", binData, extraTOML, "storage", "secrets")
	if _, err := mgrKC.InstallLocal(kcDir); err != nil {
		t.Fatalf("InstallLocal(perm-kc-ext): %v", err)
	}
	if err := mgrKC.AllowAndBuild("perm-kc-ext"); err != nil {
		t.Fatalf("AllowAndBuild(perm-kc-ext): %v", err)
	}
	if err := mgrKC.RunCommand("perm-kc-ext", "seed", "en"); err != nil {
		t.Fatalf("RunCommand(perm-kc-ext, seed): %v", err)
	}
	assertFilePerm(t, filepath.Join(hKC.DataDir, "extension-data", "perm-kc-ext", "secrets-index.json"), 0o600)
}

func TestF5_T1_MultiExtensionIsolationUnderSameDataDir(t *testing.T) {
	binPath := buildFixtureSDKBinary(t, minimalSDKBinarySource)
	binData, err := os.ReadFile(binPath)
	if err != nil {
		t.Fatalf("read fixture binary: %v", err)
	}

	mgr, h := newTestManager(t, WithDeveloperMode(true))
	srcRoot := t.TempDir()
	dirA := createLocalBinaryExtensionDir(t, srcRoot, "ext-alpha", binData, "", "storage")
	dirB := createLocalBinaryExtensionDir(t, srcRoot, "ext-beta", binData, "", "secrets")

	if _, err := mgr.InstallLocal(dirA); err != nil {
		t.Fatalf("InstallLocal alpha: %v", err)
	}
	if _, err := mgr.InstallLocal(dirB); err != nil {
		t.Fatalf("InstallLocal beta: %v", err)
	}
	if err := mgr.AllowAndBuild("ext-alpha"); err != nil {
		t.Fatalf("AllowAndBuild alpha: %v", err)
	}

	grantsRaw, err := os.ReadFile(filepath.Join(h.DataDir, "extension-grants.json"))
	if err != nil {
		t.Fatalf("read extension-grants.json: %v", err)
	}
	if !strings.Contains(string(grantsRaw), "ext-alpha") {
		t.Fatalf("extension-grants.json missing ext-alpha: %s", string(grantsRaw))
	}
	for _, id := range []string{"ext-alpha", "ext-beta"} {
		if _, err := os.Stat(filepath.Join(h.DataDir, "extensions", id, "extension.toml")); err != nil {
			t.Fatalf("missing manifest for %s: %v", id, err)
		}
	}
	if _, err := os.Stat(filepath.Join(h.DataDir, "extension-data", "ext-alpha")); err != nil {
		t.Fatalf("expected extension-data/ext-alpha to exist: %v", err)
	}
}

func TestF5_T2_NeverWritesDotGVX(t *testing.T) {
	fakeHome := t.TempDir()
	t.Setenv("HOME", fakeHome)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(fakeHome, ".config"))

	defDir, err := store.DefaultDir()
	if err != nil {
		t.Fatalf("store.DefaultDir: %v", err)
	}
	if strings.Contains(defDir, ".gvx") {
		t.Fatalf("store.DefaultDir() = %q must not use .gvx", defDir)
	}

	mgr, _ := newTestManager(t, WithDeveloperMode(true))
	extDir := createLocalBinaryExtensionDir(t, t.TempDir(), "no-dotgvx", nil, "")
	if _, err := mgr.InstallLocal(extDir); err != nil {
		t.Fatalf("InstallLocal: %v", err)
	}
	if err := mgr.AllowAndBuild("no-dotgvx"); err != nil {
		t.Fatalf("AllowAndBuild: %v", err)
	}
	assertNoDotGVX(t, fakeHome)
}

func TestF5_T2_RejectsInvalidExtIDs(t *testing.T) {
	mgr, h := newTestManager(t)
	fsValidBadIDs := []string{
		"UpperCase",
		"-lead",
		"trail-",
		".hidden",
		strings.Repeat("a", 65),
	}
	for _, id := range fsValidBadIDs {
		manifest := minimalManifestTOML(id, "0.1.0")
		if _, err := ext.ParseManifest([]byte(manifest)); err == nil {
			t.Fatalf("ParseManifest accepted invalid extension ID %q", id)
		}
		dir := filepath.Join(t.TempDir(), id)
		writeExtFixture(t, dir, manifest, map[string]string{"Dockerfile": "FROM alpine:3.20\n"})
		if _, err := mgr.InstallLocal(dir); err == nil {
			t.Fatalf("InstallLocal accepted invalid extension ID %q", id)
		}
	}

	pathOrEmptyIDs := []string{
		"../escape",
		"a/b",
		"",
		`a\b`,
	}
	for _, id := range pathOrEmptyIDs {
		if _, err := ext.ParseManifest([]byte(minimalManifestTOML(id, "0.1.0"))); err == nil {
			t.Fatalf("ParseManifest accepted path/empty extension ID %q", id)
		}
	}
	traversalDir := filepath.Join(t.TempDir(), "escape")
	writeExtFixture(t, traversalDir, minimalManifestTOML("../escape", "0.1.0"), map[string]string{"Dockerfile": "FROM alpine:3.20\n"})
	if _, err := mgr.InstallLocal(traversalDir); err == nil {
		t.Fatal("InstallLocal accepted ../escape manifest ID")
	}
	if _, err := os.Stat(filepath.Join(h.DataDir, "escape")); !os.IsNotExist(err) {
		t.Fatalf("path traversal created directory outside extensions/: %v", err)
	}
}

func TestF5_T2_RejectsSymlinkEscapeInExtensionsAndDataDirs(t *testing.T) {
	// Subcase A: symlinked extensions/<id> -> outside directory.
	outside := t.TempDir()
	secretFile := filepath.Join(outside, "extension.toml")
	if err := os.WriteFile(secretFile, []byte(minimalManifestTOML("sym-ext", "0.1.0")), 0o600); err != nil {
		t.Fatalf("write outside file: %v", err)
	}

	dataDir := newTempDataDir(t)
	extsDir := filepath.Join(dataDir, "extensions")
	if err := os.MkdirAll(extsDir, 0o700); err != nil {
		t.Fatalf("mkdir extensions: %v", err)
	}
	if err := os.Symlink(outside, filepath.Join(extsDir, "sym-ext")); err != nil {
		t.Fatalf("os.Symlink: %v", err)
	}

	mgr, _ := newTestManager(t, WithDataDir(dataDir))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	_ = mgr.Start(ctx)
	for _, item := range mgr.List() {
		if item.ID == "sym-ext" && item.State != "invalid" {
			t.Fatalf("symlinked extensions/<id> should be refused or marked invalid, got state %q", item.State)
		}
	}
	if err := mgr.AllowAndBuild("sym-ext"); err == nil {
		t.Fatal("AllowAndBuild succeeded on symlinked extensions/sym-ext")
	}
	if err := mgr.Enable("sym-ext"); err == nil {
		t.Fatal("Enable succeeded on symlinked extensions/sym-ext")
	}
	if _, err := os.Stat(filepath.Join(outside, "source.json")); !os.IsNotExist(err) {
		t.Fatalf("manager wrote source.json through symlink into outside dir (err=%v)", err)
	}

	binPath := buildFixtureSDKBinary(t, minimalSDKBinarySource)
	binData, err := os.ReadFile(binPath)
	if err != nil {
		t.Fatalf("read fixture binary: %v", err)
	}
	settingsTOML := "\n[contributes.settings]\nkey = { type = \"string\", label = \"Key\", default = \"a\" }\n"

	// Subcase B: symlinked extension-data/<id> -> outsideData.
	dataDir2 := newTempDataDir(t)
	outsideData := t.TempDir()
	extDataRoot := filepath.Join(dataDir2, "extension-data")
	if err := os.MkdirAll(extDataRoot, 0o700); err != nil {
		t.Fatalf("mkdir extension-data: %v", err)
	}
	if err := os.Symlink(outsideData, filepath.Join(extDataRoot, "sym-data")); err != nil {
		t.Fatalf("symlink extension-data/sym-data: %v", err)
	}
	mgr2, _ := newTestManager(t, WithDataDir(dataDir2), WithDeveloperMode(true))
	dirB := createLocalBinaryExtensionDir(t, t.TempDir(), "sym-data", binData, settingsTOML)
	if _, err := mgr2.InstallLocal(dirB); err != nil {
		t.Fatalf("InstallLocal sym-data: %v", err)
	}
	errAllow := mgr2.AllowAndBuild("sym-data")
	errSet := mgr2.SetSetting("sym-data", "key", "b")
	if errAllow == nil && errSet == nil {
		t.Fatal("expected symlinked extension-data/sym-data to reject AllowAndBuild or SetSetting")
	}
	if entries, err := os.ReadDir(outsideData); err == nil && len(entries) > 0 {
		t.Fatalf("wrote %d unexpected entries into symlinked outsideData dir", len(entries))
	}

	// Subcase C: symlinked extension-data/<id>/files -> outsideFiles.
	dataDir3 := newTempDataDir(t)
	outsideFiles := t.TempDir()
	extDataDir3 := filepath.Join(dataDir3, "extension-data", "sym-files")
	if err := os.MkdirAll(extDataDir3, 0o700); err != nil {
		t.Fatalf("mkdir extension-data/sym-files: %v", err)
	}
	if err := os.Symlink(outsideFiles, filepath.Join(extDataDir3, "files")); err != nil {
		t.Fatalf("symlink extension-data/sym-files/files: %v", err)
	}
	mgr3, _ := newTestManager(t, WithDataDir(dataDir3), WithDeveloperMode(true))
	dirC := createLocalBinaryExtensionDir(t, t.TempDir(), "sym-files", binData, "")
	if _, err := mgr3.InstallLocal(dirC); err != nil {
		t.Fatalf("InstallLocal sym-files: %v", err)
	}
	if err := mgr3.AllowAndBuild("sym-files"); err == nil {
		t.Fatal("expected AllowAndBuild to reject symlinked extension-data/sym-files/files")
	}
	if entries, err := os.ReadDir(outsideFiles); err == nil && len(entries) > 0 {
		t.Fatalf("wrote %d unexpected entries into symlinked outsideFiles dir", len(entries))
	}
}

func TestF5_T2_AtomicWritesSurvivePartialTempFiles(t *testing.T) {
	binPath := buildFixtureSDKBinary(t, minimalSDKBinarySource)
	binData, err := os.ReadFile(binPath)
	if err != nil {
		t.Fatalf("read fixture binary: %v", err)
	}

	dataDir := newTempDataDir(t)
	staleTmp := filepath.Join(dataDir, ".extension-grants.json-stale.tmp")
	if err := os.WriteFile(staleTmp, []byte("{corrupt-partial"), 0o600); err != nil {
		t.Fatalf("write stale tmp: %v", err)
	}

	mgr, _ := newTestManager(t, WithDataDir(dataDir), WithDeveloperMode(true))
	srcRoot := t.TempDir()
	var wg sync.WaitGroup
	errCh := make(chan error, 4)
	for i := 0; i < 4; i++ {
		id := fmt.Sprintf("atomic-%d", i)
		extDir := createLocalBinaryExtensionDir(t, srcRoot, id, binData, "", "storage")
		if _, err := mgr.InstallLocal(extDir); err != nil {
			t.Fatalf("InstallLocal %s: %v", id, err)
		}
		wg.Add(1)
		go func(extID string) {
			defer wg.Done()
			if err := mgr.AllowAndBuild(extID); err != nil {
				errCh <- fmt.Errorf("AllowAndBuild(%s): %w", extID, err)
			}
		}(id)
	}
	wg.Wait()
	close(errCh)
	for err := range errCh {
		t.Fatal(err)
	}

	grantsBytes, err := os.ReadFile(filepath.Join(dataDir, "extension-grants.json"))
	if err != nil {
		t.Fatalf("read extension-grants.json: %v", err)
	}
	var parsed any
	if err := json.Unmarshal(grantsBytes, &parsed); err != nil {
		t.Fatalf("extension-grants.json corrupted under concurrent writes: %v\n%s", err, string(grantsBytes))
	}
	for i := 0; i < 4; i++ {
		id := fmt.Sprintf("atomic-%d", i)
		if !strings.Contains(string(grantsBytes), id) {
			t.Fatalf("extension-grants.json missing %q after concurrent AllowAndBuild", id)
		}
	}
}

func TestF5_T2_MissingSubdirsAutoRecreatedSafely(t *testing.T) {
	mgr, h := newTestManager(t, WithDeveloperMode(true))
	extraTOML := "\n[contributes.settings]\nkey = { type = \"string\", label = \"Key\", default = \"a\" }\n"
	extDir := createLocalBinaryExtensionDir(t, t.TempDir(), "recreate-ext", nil, extraTOML)
	if _, err := mgr.InstallLocal(extDir); err != nil {
		t.Fatalf("InstallLocal: %v", err)
	}
	if err := mgr.AllowAndBuild("recreate-ext"); err != nil {
		t.Fatalf("AllowAndBuild: %v", err)
	}
	if err := mgr.Disable("recreate-ext"); err != nil {
		t.Fatalf("Disable: %v", err)
	}
	_ = os.RemoveAll(filepath.Join(h.DataDir, "extension-data", "recreate-ext", "files"))
	_ = os.RemoveAll(filepath.Join(h.DataDir, "extension-data", "recreate-ext", "logs"))

	if err := mgr.SetSetting("recreate-ext", "key", "b"); err != nil {
		t.Fatalf("SetSetting after subdir removal: %v", err)
	}
	if err := mgr.Enable("recreate-ext"); err != nil {
		t.Fatalf("Enable after subdir removal: %v", err)
	}
	assertDirPerm(t, filepath.Join(h.DataDir, "extension-data", "recreate-ext"), 0o700)
	assertDirPerm(t, filepath.Join(h.DataDir, "extension-data", "recreate-ext", "files"), 0o700)
	assertDirPerm(t, filepath.Join(h.DataDir, "extension-data", "recreate-ext", "logs"), 0o700)
}

// =============================================================================
// Feature 6: Strict extension.toml Manifest Parser
// =============================================================================

func TestF6_T1_MinimalValidManifest(t *testing.T) {
	raw := `
schema_version = 1
id = "demo-ext"
version = "0.1.0"
publisher = "acme"
name = "Demo"
description = "Minimal extension"

[runtime]
dockerfile = "Dockerfile"
`
	m, err := ext.ParseManifest([]byte(raw))
	if err != nil {
		t.Fatalf("ParseManifest minimal: %v", err)
	}
	if m.SchemaVersion != 1 || m.ID != "demo-ext" {
		t.Fatalf("unexpected header fields: %+v", m)
	}
	if m.Runtime.Type != "sandbox" || m.Runtime.Port != 18900 || m.Runtime.Context != "." || m.Runtime.Memory != "512m" {
		t.Fatalf("unexpected runtime defaults: %+v", m.Runtime)
	}
}

func TestF6_T1_FullManifestWithRuntimeAndContributes(t *testing.T) {
	m, err := ext.ParseManifest([]byte(fullManifestTOML("full-ext")))
	if err != nil {
		t.Fatalf("ParseManifest full: %v", err)
	}
	if m.Runtime.Target != "runner" || m.Runtime.Port != 18950 || m.Runtime.IdleTimeoutSec != 60 {
		t.Fatalf("unexpected runtime: %+v", m.Runtime)
	}
	if len(m.Views) != 2 || len(m.Commands) != 1 || len(m.Tools) != 1 {
		t.Fatalf("unexpected contributions count: views=%d commands=%d tools=%d", len(m.Views), len(m.Commands), len(m.Tools))
	}
	if len(m.Settings) != 5 {
		t.Fatalf("expected 5 settings, got %d", len(m.Settings))
	}
	if m.WebUI == nil || m.WebUI.Port != 8080 || m.WebUI.Path != "/ui" {
		t.Fatalf("unexpected web_ui: %+v", m.WebUI)
	}
}

func TestF6_T1_SemVerWithPrerelease(t *testing.T) {
	for _, ver := range []string{"0.0.1", "1.2.3-rc.1", "2.10.0-alpha.20261124"} {
		if _, err := ext.ParseManifest([]byte(minimalManifestTOML("semver-ext", ver))); err != nil {
			t.Fatalf("ParseManifest rejected valid SemVer %q: %v", ver, err)
		}
	}
}

func TestF6_T1_SettingDeclarationOrderPreserved(t *testing.T) {
	raw := minimalManifestTOML("order-ext", "0.1.0") + `
[contributes.settings]
z_last = { type = "string", label = "Z" }
a_first = { type = "string", label = "A" }
m_mid = { type = "boolean", label = "M" }
b_two = { type = "number", label = "B" }
y_five = { type = "secret", label = "Y" }
c_six = { type = "select", label = "C", options = ["x", "y"] }
`
	m, err := ext.ParseManifest([]byte(raw))
	if err != nil {
		t.Fatalf("ParseManifest: %v", err)
	}
	want := []string{"z_last", "a_first", "m_mid", "b_two", "y_five", "c_six"}
	if len(m.Settings) != len(want) {
		t.Fatalf("len(Settings) = %d, want %d", len(m.Settings), len(want))
	}
	for i, w := range want {
		if m.Settings[i].Key != w {
			t.Fatalf("setting[%d].Key = %q, want %q (order must match TOML file)", i, m.Settings[i].Key, w)
		}
	}
}

func TestF6_T1_ValidSandboxVsBinaryRuntimes(t *testing.T) {
	cases := []struct {
		name string
		toml string
	}{
		{"sandbox_dockerfile_only", minimalManifestTOML("sbx-df", "0.1.0")},
		{"sandbox_image_only", `schema_version = 1
id = "sbx-img"
version = "0.1.0"
publisher = "acme"
name = "Image Only"
description = "Prebuilt image"
[runtime]
type = "sandbox"
dockerfile = ""
image = "ghcr.io/acme/img:0.1.0"
`},
		{"binary_runtime", binaryManifestTOML("bin-ext", "0.1.0", "bin/ext")},
	}
	for _, tc := range cases {
		if _, err := ext.ParseManifest([]byte(tc.toml)); err != nil {
			t.Fatalf("%s: ParseManifest failed: %v", tc.name, err)
		}
	}
}

func TestF6_T2_RejectsOver64KiBManifest(t *testing.T) {
	base := minimalManifestTOML("size-ext", "0.1.0")
	exact := base + strings.Repeat("#", (64<<10)-len(base))
	if len(exact) != 64<<10 {
		t.Fatalf("setup len = %d", len(exact))
	}
	if _, err := ext.ParseManifest([]byte(exact)); err != nil {
		t.Fatalf("exact 64 KiB manifest should pass, got %v", err)
	}
	over := exact + "#"
	if _, err := ext.ParseManifest([]byte(over)); err == nil {
		t.Fatal("expected manifest > 64 KiB to be rejected")
	}
}

func TestF6_T2_RejectsUnknownKeysTopLevelAndNested(t *testing.T) {
	base := minimalManifestTOML("unk-ext", "0.1.0")
	cases := []struct {
		name string
		toml string
	}{
		{"top_level", base + "\nunknown_key = true\n"},
		{"runtime", strings.Replace(base, "port = 18900", "port = 18900\ngpu = true", 1)},
		{"view", base + "\ncolor = \"#fff\"\n"},
		{"command", base + "\n[[contributes.command]]\nid = \"c1\"\ntitle = \"C1\"\nshortcut = \"cmd+k\"\n"},
		{"tool", base + "\n[[contributes.tool]]\nid = \"t1\"\ndescription = \"d\"\nextra = 1\n"},
		{"settings", base + "\n[contributes.settings.s1]\ntype = \"string\"\nlabel = \"S\"\nunknown = 1\n"},
		{"web_ui", base + "\n[contributes.web_ui]\nport = 8080\npath = \"/\"\nunknown_web = true\n"},
	}
	for _, tc := range cases {
		if _, err := ext.ParseManifest([]byte(tc.toml)); err == nil {
			t.Fatalf("%s: expected unknown TOML key to be rejected", tc.name)
		}
	}
}

func TestF6_T2_RejectsInvalidSchemaVersion(t *testing.T) {
	base := minimalManifestTOML("ver-ext", "0.1.0")
	for _, repl := range []string{
		"schema_version = 0",
		"schema_version = 2",
		`schema_version = "1"`,
		"",
	} {
		toml := strings.Replace(base, "schema_version = 1", repl, 1)
		if _, err := ext.ParseManifest([]byte(toml)); err == nil {
			t.Fatalf("expected invalid schema_version (%q) to be rejected", repl)
		}
	}
}

func TestF6_T2_RejectsReservedIDPrefixAndPublisher(t *testing.T) {
	if _, err := ext.ParseManifest([]byte(minimalManifestTOML("gvx-actions", "0.1.0"))); err == nil {
		t.Fatal("expected reserved gvx- ID prefix to be rejected")
	}
	if _, err := ext.ParseManifest([]byte(minimalManifestTOML("gvx", "0.1.0"))); err == nil {
		t.Fatal("expected reserved gvx ID to be rejected")
	}
	pubGVX := strings.Replace(minimalManifestTOML("custom-ext", "0.1.0"), `publisher = "acme"`, `publisher = "gvx"`, 1)
	if _, err := ext.ParseManifest([]byte(pubGVX)); err == nil {
		t.Fatal("expected reserved publisher=\"gvx\" to be rejected")
	}
}

func TestF6_T2_RejectsInvalidIDFolderMismatchAndBadSemVer(t *testing.T) {
	for _, badID := range []string{"Bad_ID", "-a", "a-", "a..b"} {
		if _, err := ext.ParseManifest([]byte(minimalManifestTOML(badID, "0.1.0"))); err == nil {
			t.Fatalf("expected invalid ID %q to be rejected", badID)
		}
	}
	for _, badVer := range []string{"1.0", "v1.0.0", "01.0.0", "1.0.0+build"} {
		if _, err := ext.ParseManifest([]byte(minimalManifestTOML("bad-ver", badVer))); err == nil {
			t.Fatalf("expected invalid SemVer %q to be rejected", badVer)
		}
	}
	for _, badPub := range []string{"GVX", "a b", ""} {
		s := strings.Replace(minimalManifestTOML("bad-pub", "0.1.0"), `publisher = "acme"`, fmt.Sprintf("publisher = %q", badPub), 1)
		if _, err := ext.ParseManifest([]byte(s)); err == nil {
			t.Fatalf("expected invalid publisher %q to be rejected", badPub)
		}
	}
	// Sandbox missing both dockerfile and image
	noImgOrDF := `schema_version = 1
id = "no-src"
version = "0.1.0"
publisher = "acme"
name = "No Src"
description = "Missing dockerfile and image"
[runtime]
type = "sandbox"
dockerfile = ""
`
	if _, err := ext.ParseManifest([]byte(noImgOrDF)); err == nil {
		t.Fatal("expected sandbox runtime with empty dockerfile and image to be rejected")
	}
	// Port out of 1024..65535
	for _, badPort := range []int{80, 1023, 65536} {
		s := strings.Replace(minimalManifestTOML("bad-port", "0.1.0"), "port = 18900", fmt.Sprintf("port = %d", badPort), 1)
		if _, err := ext.ParseManifest([]byte(s)); err == nil {
			t.Fatalf("expected port %d to be rejected", badPort)
		}
	}
	// Folder mismatch vs manifest ID on ParseManifestDir & Manager discovery
	mismatchDir := filepath.Join(t.TempDir(), "wrong-folder-name")
	writeExtFixture(t, mismatchDir, minimalManifestTOML("expected-id", "0.1.0"), map[string]string{"Dockerfile": "FROM alpine:3.20\n"})
	if _, err := ext.ParseManifestDir(mismatchDir, "wrong-folder-name"); err == nil {
		t.Fatal("expected folder name mismatch with manifest ID to be rejected by ParseManifestDir")
	}
	mgr, h := newTestManager(t)
	installedMismatchDir := ext.ExtensionDir(h.DataDir, "wrong-folder-name")
	writeExtFixture(t, installedMismatchDir, minimalManifestTOML("expected-id", "0.1.0"), map[string]string{"Dockerfile": "FROM alpine:3.20\n"})
	if err := mgr.Start(context.Background()); err != nil {
		t.Fatalf("mgr.Start: %v", err)
	}
	var foundInvalid bool
	for _, info := range mgr.List() {
		if info.ID == "wrong-folder-name" {
			if info.State != ext.StateInvalid {
				t.Fatalf("expected mismatched folder extension state %q, got %q", ext.StateInvalid, info.State)
			}
			foundInvalid = true
		}
	}
	if !foundInvalid {
		t.Fatal("expected mismatched folder extension to be reported in StateInvalid")
	}
}

// =============================================================================
// Feature 7: Path-Traversal, Symlink & Spoofing Validation
// =============================================================================

func TestF7_T1_CleanRelativeRuntimePaths(t *testing.T) {
	sbx := `schema_version = 1
id = "clean-paths"
version = "0.1.0"
publisher = "acme"
name = "Clean Paths"
description = "Nested relative paths"
[runtime]
type = "sandbox"
dockerfile = "docker/Dockerfile"
context = "sub/ctx"
dev_entrypoint = "bin/ext"
`
	if _, err := ext.ParseManifest([]byte(sbx)); err != nil {
		t.Fatalf("ParseManifest clean nested paths: %v", err)
	}
}

func TestF7_T1_CleanUTF8MetadataAndOptions(t *testing.T) {
	raw := `schema_version = 1
id = "utf8-ext"
version = "0.1.0"
publisher = "acme"
name = "GitHub 워크플로우 — 日本語"
description = "Ümlaut friendly description"
[runtime]
dockerfile = "Dockerfile"
[[contributes.view]]
id = "main"
title = "뷰 · ビュー"
[contributes.settings]
region = { type = "select", label = "리전", options = ["서울", "東京", "Frankfurt"], default = "서울" }
`
	if _, err := ext.ParseManifest([]byte(raw)); err != nil {
		t.Fatalf("ParseManifest clean UTF-8: %v", err)
	}
}

func TestF7_T1_NewlineAndTabAllowedInNonInlineText(t *testing.T) {
	for _, raw := range []string{
		`{"kind":"text","text":"line 1\n\tline 2"}`,
		`{"kind":"markdown","text":"# Title\n\n- item\t1"}`,
		`{"kind":"code","language":"go","text":"func f() {\n\treturn\n}"}`,
	} {
		if _, err := uiblock.Read([]byte(raw)); err != nil {
			t.Fatalf("uiblock.Read failed on valid newline/tab text %s: %v", raw, err)
		}
	}
	if got := uiblock.Printable("a\n\tb"); got != "a\n\tb" {
		t.Fatalf("Printable altered newline/tab: %q", got)
	}
}

func TestF7_T1_SymlinkInsideRepoRootAllowed(t *testing.T) {
	mgr, _ := newTestManager(t)
	extDir := filepath.Join(t.TempDir(), "sym-inside")
	writeExtFixture(t, extDir, minimalManifestTOML("sym-inside", "0.1.0"), map[string]string{
		"real.Dockerfile": "FROM alpine:3.20\nCMD [\"/bin/sh\"]\n",
	})
	if err := os.Symlink("real.Dockerfile", filepath.Join(extDir, "Dockerfile")); err != nil {
		t.Fatalf("os.Symlink: %v", err)
	}
	if _, err := mgr.InstallLocal(extDir); err != nil {
		t.Fatalf("InstallLocal rejected benign intra-repo symlink: %v", err)
	}
}

func TestF7_T1_PrintableEscapesSpoofingSafely(t *testing.T) {
	samples := []struct {
		r    rune
		want string
	}{
		{'\x1b', `\x1b`},
		{'\x00', `\x00`},
		{0x202E, `\u202e`},
		{0x200B, `\u200b`},
	}
	for _, s := range samples {
		if !uiblock.Spoofing(s.r) {
			t.Fatalf("Spoofing(%U) = false, want true", s.r)
		}
		got := uiblock.Printable("pre" + string(s.r) + "post")
		if !strings.Contains(got, s.want) {
			t.Fatalf("Printable for %U = %q, want substring %q", s.r, got, s.want)
		}
	}
}

func TestF7_T2_RejectsDotDotTraversalInRuntimePaths(t *testing.T) {
	for _, bad := range []string{"../Dockerfile", "sub/../../Dockerfile", ".."} {
		for _, field := range []string{"dockerfile", "context", "dev_entrypoint"} {
			raw := fmt.Sprintf(`schema_version = 1
id = "dotdot-ext"
version = "0.1.0"
publisher = "acme"
name = "DotDot"
description = "Traversal"
[runtime]
type = "binary"
dockerfile = "Dockerfile"
context = "."
dev_entrypoint = "bin/ext"
`)
			// Replace the targeted field.
			target := field + " = "
			lines := strings.Split(raw, "\n")
			for i, l := range lines {
				if strings.HasPrefix(l, target) {
					lines[i] = fmt.Sprintf("%s = %q", field, bad)
				}
			}
			if _, err := ext.ParseManifest([]byte(strings.Join(lines, "\n"))); err == nil {
				t.Fatalf("expected .. traversal %q in %s to be rejected", bad, field)
			}
		}
	}
}

func TestF7_T2_RejectsAbsoluteAndBackslashPaths(t *testing.T) {
	for _, bad := range []string{"/etc/passwd", `docker\Dockerfile`, `..\evil`, `C:Dockerfile`, "bad\x00path"} {
		for _, field := range []string{"dockerfile", "context", "dev_entrypoint"} {
			raw := fmt.Sprintf(`schema_version = 1
id = "abs-ext"
version = "0.1.0"
publisher = "acme"
name = "Abs"
description = "Abs/backslash"
[runtime]
type = "binary"
dockerfile = "Dockerfile"
context = "."
dev_entrypoint = "bin/ext"
`)
			lines := strings.Split(raw, "\n")
			for i, l := range lines {
				if strings.HasPrefix(l, field+" = ") {
					lines[i] = fmt.Sprintf("%s = %s", field, tomlQuote(bad))
				}
			}
			if _, err := ext.ParseManifest([]byte(strings.Join(lines, "\n"))); err == nil {
				t.Fatalf("expected invalid path %q in %s to be rejected", bad, field)
			}
		}
	}
}

func TestF7_T2_RejectsSymlinkEscapingOutsideRoot(t *testing.T) {
	mgr, _ := newTestManager(t)
	outside := t.TempDir()
	outsideFile := filepath.Join(outside, "secret.txt")
	if err := os.WriteFile(outsideFile, []byte("FROM alpine:3.20\n"), 0o600); err != nil {
		t.Fatalf("write outside file: %v", err)
	}

	// Case 1: Dockerfile symlink escaping outside root.
	dir1 := filepath.Join(t.TempDir(), "esc-df")
	writeExtFixture(t, dir1, minimalManifestTOML("esc-df", "0.1.0"), nil)
	if err := os.Symlink(outsideFile, filepath.Join(dir1, "Dockerfile")); err != nil {
		t.Fatalf("symlink: %v", err)
	}
	if _, err := mgr.InstallLocal(dir1); err == nil {
		t.Fatal("expected symlink escape via Dockerfile to be rejected")
	}

	// Case 2: context symlink escaping outside root.
	dir2 := filepath.Join(t.TempDir(), "esc-ctx")
	m2 := strings.Replace(minimalManifestTOML("esc-ctx", "0.1.0"), `context = "."`, `context = "ctx"`, 1)
	writeExtFixture(t, dir2, m2, map[string]string{"Dockerfile": "FROM alpine:3.20\n"})
	if err := os.Symlink(outside, filepath.Join(dir2, "ctx")); err != nil {
		t.Fatalf("symlink ctx: %v", err)
	}
	if _, err := mgr.InstallLocal(dir2); err == nil {
		t.Fatal("expected symlink escape via context dir to be rejected")
	}

	// Case 3: dev_entrypoint symlink escaping outside root.
	dir3 := filepath.Join(t.TempDir(), "esc-dev")
	writeExtFixture(t, dir3, binaryManifestTOML("esc-dev", "0.1.0", "bin/ext"), nil)
	if err := os.MkdirAll(filepath.Join(dir3, "bin"), 0o700); err != nil {
		t.Fatalf("mkdir bin: %v", err)
	}
	if err := os.Symlink(outsideFile, filepath.Join(dir3, "bin", "ext")); err != nil {
		t.Fatalf("symlink dev_entrypoint: %v", err)
	}
	if _, err := mgr.InstallLocal(dir3); err == nil {
		t.Fatal("expected symlink escape via dev_entrypoint to be rejected")
	}
}

func TestF7_T2_RejectsSpoofingCharsInManifestFields(t *testing.T) {
	spoofs := []string{"\u202e", "\u200b", "\ufeff", "\x1b", "\x00", "\u2066"}
	for _, sp := range spoofs {
		candidates := []string{
			strings.Replace(minimalManifestTOML("sp-ext", "0.1.0"), `name = "Test Extension sp-ext"`, fmt.Sprintf(`name = %s`, tomlQuote("Evil"+sp+"Name")), 1),
			strings.Replace(minimalManifestTOML("sp-ext", "0.1.0"), `description = "E2E test extension"`, fmt.Sprintf(`description = %s`, tomlQuote("Desc"+sp)), 1),
			minimalManifestTOML("sp-ext", "0.1.0") + fmt.Sprintf("\n[[contributes.tool]]\nid = \"t\"\ndescription = %s\n", tomlQuote("Tool"+sp)),
			minimalManifestTOML("sp-ext", "0.1.0") + fmt.Sprintf("\n[[contributes.tool]]\nid = \"t\"\ndescription = \"ok\"\n[contributes.tool.params]\np = { type = \"string\", description = %s }\n", tomlQuote("Param"+sp)),
			minimalManifestTOML("sp-ext", "0.1.0") + fmt.Sprintf("\n[contributes.settings]\nk = { type = \"string\", label = %s }\n", tomlQuote("Lbl"+sp)),
			minimalManifestTOML("sp-ext", "0.1.0") + fmt.Sprintf("\n[contributes.settings]\nk = { type = \"string\", label = \"K\", description = %s }\n", tomlQuote("Desc"+sp)),
		}
		for idx, c := range candidates {
			if _, err := ext.ParseManifest([]byte(c)); err == nil {
				t.Fatalf("case %d accepted spoofing char %q", idx, sp)
			}
		}
	}
}

func TestF7_T2_RejectsSpoofingCharsInOptionsAndViewCommandTitles(t *testing.T) {
	for _, sp := range []string{"\u202e", "\x1b", "\n", "\t"} {
		badView := strings.Replace(minimalManifestTOML("sp2-ext", "0.1.0"), `title = "Main"`, fmt.Sprintf(`title = %s`, tomlQuote("Main"+sp)), 1)
		if _, err := ext.ParseManifest([]byte(badView)); err == nil {
			t.Fatalf("accepted spoofing char %q in view title", sp)
		}
		badCmd := minimalManifestTOML("sp2-ext", "0.1.0") + fmt.Sprintf("\n[[contributes.command]]\nid = \"c\"\ntitle = %s\n", tomlQuote("Cmd"+sp))
		if _, err := ext.ParseManifest([]byte(badCmd)); err == nil {
			t.Fatalf("accepted spoofing char %q in command title", sp)
		}
		badOpt := minimalManifestTOML("sp2-ext", "0.1.0") + fmt.Sprintf("\n[contributes.settings]\nsel = { type = \"select\", label = \"Sel\", options = [%s] }\n", tomlQuote("Opt"+sp))
		if _, err := ext.ParseManifest([]byte(badOpt)); err == nil {
			t.Fatalf("accepted spoofing char %q in setting option", sp)
		}
	}
	if _, err := uiblock.Read([]byte(`{"kind":"text","text":"bad\u202etext"}`)); err == nil {
		t.Fatal("uiblock.Read accepted U+202E in text")
	}
	if _, err := uiblock.Read([]byte(`{"kind":"select","key":"s","options":["bad\nopt"]}`)); err == nil {
		t.Fatal("uiblock.Read accepted newline in select option")
	}
}

// =============================================================================
// Feature 8: Contributions Schema & Bounds
// =============================================================================

func TestF8_T1_MaxViewsWithAutoAndToolbarChrome(t *testing.T) {
	var b strings.Builder
	b.WriteString(`schema_version = 1
id = "max-views"
version = "0.1.0"
publisher = "acme"
name = "Max Views"
description = "8 views boundary"
[runtime]
dockerfile = "Dockerfile"
`)
	chromes := []string{"auto", "toolbar", ""}
	for i := 0; i < 8; i++ {
		fmt.Fprintf(&b, "\n[[contributes.view]]\nid = \"view-%d\"\ntitle = \"View %d\"\n", i, i)
		if c := chromes[i%len(chromes)]; c != "" {
			fmt.Fprintf(&b, "chrome = %q\n", c)
		}
	}
	m, err := ext.ParseManifest([]byte(b.String()))
	if err != nil {
		t.Fatalf("ParseManifest 8 views failed: %v", err)
	}
	if len(m.Views) != 8 {
		t.Fatalf("len(Views) = %d, want 8", len(m.Views))
	}
}

func TestF8_T1_MaxCommandsAndMaxToolsWithMaxParams(t *testing.T) {
	var b strings.Builder
	b.WriteString(minimalManifestTOML("max-cmd-tool", "0.1.0"))
	for i := 0; i < 32; i++ {
		fmt.Fprintf(&b, "\n[[contributes.command]]\nid = \"cmd-%d\"\ntitle = \"Command %d\"\n", i, i)
	}
	paramTypes := []string{"string", "number", "boolean"}
	for i := 0; i < 16; i++ {
		fmt.Fprintf(&b, "\n[[contributes.tool]]\nid = \"tool_%d\"\ndescription = \"Tool %d\"\n", i, i)
		if i == 0 {
			b.WriteString("[contributes.tool.params]\n")
			for p := 0; p < 16; p++ {
				fmt.Fprintf(&b, "param_%d = { type = %q, description = \"Param %d\", required = %v }\n",
					p, paramTypes[p%len(paramTypes)], p, p%2 == 0)
			}
		}
	}
	m, err := ext.ParseManifest([]byte(b.String()))
	if err != nil {
		t.Fatalf("ParseManifest max commands/tools/params failed: %v", err)
	}
	if len(m.Commands) != 32 || len(m.Tools) != 16 || len(m.Tools[0].Params) != 16 {
		t.Fatalf("unexpected counts: cmds=%d tools=%d params=%d",
			len(m.Commands), len(m.Tools), len(m.Tools[0].Params))
	}
}

func TestF8_T1_MaxSettingsCoveringAll5Types(t *testing.T) {
	var b strings.Builder
	b.WriteString(minimalManifestTOML("max-settings", "0.1.0"))
	b.WriteString("\n[contributes.settings]\n")
	for i := 0; i < 32; i++ {
		switch i % 5 {
		case 0:
			fmt.Fprintf(&b, "s_%d = { type = \"string\", label = \"S %d\", default = \"v\" }\n", i, i)
		case 1:
			fmt.Fprintf(&b, "s_%d = { type = \"secret\", label = \"Sec %d\" }\n", i, i)
		case 2:
			fmt.Fprintf(&b, "s_%d = { type = \"boolean\", label = \"Bool %d\", default = true }\n", i, i)
		case 3:
			fmt.Fprintf(&b, "s_%d = { type = \"number\", label = \"Num %d\", default = 42 }\n", i, i)
		case 4:
			fmt.Fprintf(&b, "s_%d = { type = \"select\", label = \"Sel %d\", options = [\"a\", \"b\"], default = \"a\" }\n", i, i)
		}
	}
	m, err := ext.ParseManifest([]byte(b.String()))
	if err != nil {
		t.Fatalf("ParseManifest 32 settings failed: %v", err)
	}
	if len(m.Settings) != 32 {
		t.Fatalf("len(Settings) = %d, want 32", len(m.Settings))
	}
}

func TestF8_T1_OptionalWebUIValidPorts(t *testing.T) {
	for _, port := range []int{ext.MinRuntimePort, 8080, ext.MaxRuntimePort} {
		raw := minimalManifestTOML("webui-ext", "0.1.0", ext.PermLocalProxy) + fmt.Sprintf("\n[contributes.web_ui]\nport = %d\npath = \"/dashboard\"\n", port)
		m, err := ext.ParseManifest([]byte(raw))
		if err != nil {
			t.Fatalf("ParseManifest web_ui port %d failed: %v", port, err)
		}
		if m.WebUI == nil || m.WebUI.Port != port {
			t.Fatalf("WebUI.Port = %+v, want %d", m.WebUI, port)
		}
	}
}

func TestF8_T1_ValidPermissionsWithinMax16(t *testing.T) {
	allPerms := []string{
		"storage", "secrets", "docker", "sandboxes", "snapshots",
		"ui:pick-folder", "ui:terminal", "ui:open-url", "localproxy", "network",
	}
	m, err := ext.ParseManifest([]byte(minimalManifestTOML("all-perms", "0.1.0", allPerms...)))
	if err != nil {
		t.Fatalf("ParseManifest all 10 explicit perms failed: %v", err)
	}
	if len(m.Permissions) != 10 {
		t.Fatalf("len(Permissions) = %d, want 10", len(m.Permissions))
	}
}

func TestF8_T2_RejectsOver16Permissions(t *testing.T) {
	perms := make([]string, 17)
	for i := range perms {
		perms[i] = fmt.Sprintf("perm-%d", i)
	}
	if _, err := ext.ParseManifest([]byte(minimalManifestTOML("too-many-perms", "0.1.0", perms...))); err == nil {
		t.Fatal("expected > 16 permissions to be rejected")
	}
}

func TestF8_T2_RejectsOver8ViewsDuplicateIDsAndInvalidChrome(t *testing.T) {
	// 9 views
	var b strings.Builder
	b.WriteString(`schema_version = 1
id = "bad-views"
version = "0.1.0"
publisher = "acme"
name = "Bad Views"
description = "Too many views"
[runtime]
dockerfile = "Dockerfile"
`)
	for i := 0; i < 9; i++ {
		fmt.Fprintf(&b, "\n[[contributes.view]]\nid = \"v-%d\"\ntitle = \"V %d\"\n", i, i)
	}
	if _, err := ext.ParseManifest([]byte(b.String())); err == nil {
		t.Fatal("expected 9 views (> 8) to be rejected")
	}

	// Duplicate view ID, invalid view ID, empty title, invalid chrome
	base := minimalManifestTOML("bad-v", "0.1.0")
	cases := []string{
		base + "\n[[contributes.view]]\nid = \"main\"\ntitle = \"Dup\"\n",
		strings.Replace(base, `id = "main"`, `id = "My_View"`, 1),
		strings.Replace(base, `title = "Main"`, `title = ""`, 1),
		base + "\n[[contributes.view]]\nid = \"extra\"\ntitle = \"Extra\"\nchrome = \"sidebar\"\n",
	}
	for i, c := range cases {
		if _, err := ext.ParseManifest([]byte(c)); err == nil {
			t.Fatalf("view invalid case %d unexpectedly accepted", i)
		}
	}
}

func TestF8_T2_RejectsOver32CommandsOver16ToolsOver16Params(t *testing.T) {
	base := minimalManifestTOML("bad-ct", "0.1.0")

	// 33 commands
	var cmds strings.Builder
	cmds.WriteString(base)
	for i := 0; i < 33; i++ {
		fmt.Fprintf(&cmds, "\n[[contributes.command]]\nid = \"c-%d\"\ntitle = \"C %d\"\n", i, i)
	}
	if _, err := ext.ParseManifest([]byte(cmds.String())); err == nil {
		t.Fatal("expected 33 commands (> 32) to be rejected")
	}

	// Duplicate command ID
	dupCmd := base + "\n[[contributes.command]]\nid = \"c1\"\ntitle = \"C1\"\n[[contributes.command]]\nid = \"c1\"\ntitle = \"C2\"\n"
	if _, err := ext.ParseManifest([]byte(dupCmd)); err == nil {
		t.Fatal("expected duplicate command ID to be rejected")
	}

	// 17 tools
	var tools strings.Builder
	tools.WriteString(base)
	for i := 0; i < 17; i++ {
		fmt.Fprintf(&tools, "\n[[contributes.tool]]\nid = \"t_%d\"\ndescription = \"T %d\"\n", i, i)
	}
	if _, err := ext.ParseManifest([]byte(tools.String())); err == nil {
		t.Fatal("expected 17 tools (> 16) to be rejected")
	}

	// Invalid param type "object"
	badParamType := base + "\n[[contributes.tool]]\nid = \"t\"\ndescription = \"T\"\n[contributes.tool.params]\np = { type = \"object\", description = \"obj\" }\n"
	if _, err := ext.ParseManifest([]byte(badParamType)); err == nil {
		t.Fatal("expected invalid tool param type \"object\" to be rejected")
	}

	// 17 tool params
	var params strings.Builder
	params.WriteString(base + "\n[[contributes.tool]]\nid = \"t\"\ndescription = \"T\"\n[contributes.tool.params]\n")
	for i := 0; i < 17; i++ {
		fmt.Fprintf(&params, "p_%d = { type = \"string\", description = \"P\" }\n", i)
	}
	if _, err := ext.ParseManifest([]byte(params.String())); err == nil {
		t.Fatal("expected 17 tool params (> 16) to be rejected")
	}
}

func TestF8_T2_RejectsOver32SettingsAndTypeDefaultMismatches(t *testing.T) {
	base := minimalManifestTOML("bad-set", "0.1.0")
	var over strings.Builder
	over.WriteString(base + "\n[contributes.settings]\n")
	for i := 0; i < 33; i++ {
		fmt.Fprintf(&over, "k_%d = { type = \"string\", label = \"K %d\" }\n", i, i)
	}
	if _, err := ext.ParseManifest([]byte(over.String())); err == nil {
		t.Fatal("expected 33 settings (> 32) to be rejected")
	}

	cases := []string{
		base + "\n[contributes.settings]\nk = { type = \"color\", label = \"K\" }\n",
		base + "\n[contributes.settings]\nk = { type = \"select\", label = \"K\" }\n",
		base + "\n[contributes.settings]\nk = { type = \"string\", label = \"K\", options = [\"a\"] }\n",
		base + "\n[contributes.settings]\nk = { type = \"boolean\", label = \"K\", default = \"true\" }\n",
		base + "\n[contributes.settings]\nk = { type = \"number\", label = \"K\", default = \"1\" }\n",
		base + "\n[contributes.settings]\nk = { type = \"select\", label = \"K\", options = [\"a\", \"b\"], default = \"c\" }\n",
		base + "\n[contributes.settings]\nk = { type = \"secret\", label = \"K\", default = \"leak\" }\n",
	}
	for i, c := range cases {
		if _, err := ext.ParseManifest([]byte(c)); err == nil {
			t.Fatalf("setting invalid case %d unexpectedly accepted", i)
		}
	}
}

func TestF8_T2_RejectsInvalidWebUIPorts(t *testing.T) {
	base := minimalManifestTOML("bad-webui", "0.1.0", ext.PermLocalProxy)
	for _, port := range []int{0, -1, 65536} {
		raw := base + fmt.Sprintf("\n[contributes.web_ui]\nport = %d\npath = \"/\"\n", port)
		if _, err := ext.ParseManifest([]byte(raw)); err == nil {
			t.Fatalf("expected invalid web_ui port %d to be rejected", port)
		}
	}
	for _, badPath := range []string{"/../secret", "/ui\u202e"} {
		raw := base + fmt.Sprintf("\n[contributes.web_ui]\nport = 8080\npath = %q\n", badPath)
		if _, err := ext.ParseManifest([]byte(raw)); err == nil {
			t.Fatalf("expected invalid web_ui path %q to be rejected", badPath)
		}
	}
}
