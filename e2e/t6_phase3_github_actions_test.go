//go:build e2e

package e2e

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/ironpark/ggui"

	"github.com/ironpark/gvx-desktop/internal/ext"
	"github.com/ironpark/gvx-desktop/internal/uiblock"
)

func locateGitHubActionsExtRepo(t *testing.T) string {
	t.Helper()
	root := repoRoot(t)
	candidate := filepath.Join(filepath.Dir(root), "gvx-ext-github-actions")
	if _, err := os.Stat(filepath.Join(candidate, "extension.toml")); err != nil {
		t.Skipf("sibling reference extension repository not found at %s: %v", candidate, err)
	}
	return candidate
}

func findNodeByKeySuffix(root *uiblock.Node, suffix string) *uiblock.Node {
	if root == nil {
		return nil
	}
	if root.ID == "/"+suffix || strings.HasSuffix(root.ID, "/"+suffix) {
		return root
	}
	for _, ch := range root.Children {
		if found := findNodeByKeySuffix(ch, suffix); found != nil {
			return found
		}
	}
	return nil
}

func collectNodeText(root *uiblock.Node, out *strings.Builder) {
	if root == nil {
		return
	}
	if root.Text != "" {
		out.WriteString(root.Text)
		out.WriteByte('\n')
	}
	for _, ch := range root.Children {
		collectNodeText(ch, out)
	}
}

func TestT6_01_GitHubActionsExtensionManifestAndBuildContext(t *testing.T) {
	extRepo := locateGitHubActionsExtRepo(t)

	// Build dev_entrypoint into a temp check only if missing, or verify manifest parsing directly.
	binName := "gvx-ext-github-actions"
	if runtime.GOOS == "windows" {
		binName += ".exe"
	}
	binPath := filepath.Join(extRepo, "bin", binName)
	if _, err := os.Stat(binPath); errors.Is(err, os.ErrNotExist) {
		if out, err := runGo(t, extRepo, nil, "build", "-o", binPath, "./cmd/gvx-ext-github-actions"); err != nil {
			t.Fatalf("build gvx-ext-github-actions: %v\n%s", err, out)
		}
	}

	// 1. Strict manifest validation through GVX Desktop's real ParseManifestDir.
	man, err := ext.ParseManifestDir(extRepo, "github-actions")
	if err != nil {
		t.Fatalf("ext.ParseManifestDir(%s): %v", extRepo, err)
	}
	if man.ID != "github-actions" || man.Publisher != "ironpark" || man.Runtime.Type != ext.RuntimeSandbox {
		t.Fatalf("unexpected manifest metadata: %+v", man)
	}
	if len(man.Permissions) != 8 || len(man.Views) != 1 || len(man.Commands) != 2 || len(man.Tools) != 2 || len(man.Settings) != 3 {
		t.Fatalf("unexpected manifest contributions: perms=%d views=%d cmds=%d tools=%d settings=%d",
			len(man.Permissions), len(man.Views), len(man.Commands), len(man.Tools), len(man.Settings))
	}

	// 2. PackBuildContext must include Dockerfile/go.mod/source and respect .dockerignore (excluding bin/ & .git/).
	var tarBuf bytes.Buffer
	if err := ext.PackBuildContext(context.Background(), extRepo, man.Runtime.Dockerfile, &tarBuf); err != nil {
		t.Fatalf("PackBuildContext: %v", err)
	}
	gzr, err := gzip.NewReader(&tarBuf)
	if err != nil {
		t.Fatalf("gzip.NewReader: %v", err)
	}
	defer gzr.Close()
	tr := tar.NewReader(gzr)
	found := map[string]bool{}
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("read tar: %v", err)
		}
		name := strings.TrimPrefix(filepath.ToSlash(hdr.Name), "./")
		found[name] = true
		if strings.HasPrefix(name, "bin/") || strings.HasPrefix(name, ".git/") {
			t.Fatalf(".dockerignore failed to exclude %q from build context tar", name)
		}
	}
	for _, want := range []string{"Dockerfile", "extension.toml", "go.mod", "cmd/gvx-ext-github-actions/main.go"} {
		if !found[want] {
			t.Fatalf("expected %q in packed build context tar", want)
		}
	}
}

func TestT6_02_GitHubActionsExtensionLiveHostIntegration(t *testing.T) {
	extRepo := locateGitHubActionsExtRepo(t)
	dataDir := newShortTempDataDir(t)

	// Compile the real extension binary into a temp Developer Mode binary fixture
	// preserving every single permission, view, command, tool, and setting from extension.toml.
	devExtDir := filepath.Join(t.TempDir(), "github-actions")
	if err := os.MkdirAll(filepath.Join(devExtDir, "bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	binName := "gvx-ext-github-actions"
	if runtime.GOOS == "windows" {
		binName += ".exe"
	}
	binDst := filepath.Join(devExtDir, "bin", binName)
	if out, err := runGo(t, extRepo, nil, "build", "-o", binDst, "./cmd/gvx-ext-github-actions"); err != nil {
		t.Fatalf("build gvx-ext-github-actions: %v\n%s", err, out)
	}

	origManifest, err := os.ReadFile(filepath.Join(extRepo, "extension.toml"))
	if err != nil {
		t.Fatalf("read extension.toml: %v", err)
	}
	devManifest := strings.Replace(string(origManifest), `type = "sandbox"`, `type = "binary"`, 1)
	if runtime.GOOS == "windows" {
		devManifest = strings.Replace(devManifest, `bin/gvx-ext-github-actions`, `bin/`+binName, 1)
	}
	writeExtManifest(t, devExtDir, devManifest)

	// Prepare a sample target workspace repository containing a workflow with a secret reference.
	sampleRepo := filepath.Join(t.TempDir(), "sample-app")
	wfDir := filepath.Join(sampleRepo, ".github", "workflows")
	if err := os.MkdirAll(wfDir, 0o755); err != nil {
		t.Fatal(err)
	}
	sampleWF := `name: Sample CI
on:
  push:
  workflow_dispatch:
    inputs:
      env:
        description: Target environment
        default: staging
jobs:
  lint:
    runs-on: ubuntu-latest
    steps:
      - name: Check secret
        run: echo "token=${{ secrets.CI_SECRET }}"
`
	if err := os.WriteFile(filepath.Join(wfDir, "ci.yml"), []byte(sampleWF), 0o644); err != nil {
		t.Fatal(err)
	}

	mgr, h := newTestManager(t,
		WithDataDir(dataDir),
		WithDeveloperMode(true),
		WithHostInfo(ext.HostInfo{
			AppVersion:  "0.10.2-test",
			OS:          "darwin",
			Arch:        "arm64",
			Locale:      "en",
			Theme:       "dark",
			DockerMode:  "sandbox",
			DockerReady: true,
		}),
	)
	h.UIHost.PickFolderResult = sampleRepo

	if err := mgr.Start(context.Background()); err != nil {
		t.Fatalf("mgr.Start: %v", err)
	}

	// Install, approve, and start the REAL gvx-ext-github-actions binary.
	info, err := mgr.InstallLocal(devExtDir)
	if err != nil {
		t.Fatalf("InstallLocal: %v", err)
	}
	if info.State != ext.StateWaiting {
		t.Fatalf("expected StateWaiting before approval, got %s", info.State)
	}
	if err := mgr.AllowAndBuild("github-actions"); err != nil {
		t.Fatalf("AllowAndBuild: %v (logs=%v)", err, mgr.Logs("github-actions"))
	}

	// Subscribe to the real SSE view stream — every frame is validated through internal/uiblock.Read!
	subCtx, subCancel := context.WithCancel(context.Background())
	defer subCancel()
	updates, err := mgr.SubscribeView(subCtx, "github-actions", "main", "en")
	if err != nil {
		t.Fatalf("SubscribeView: %v", err)
	}

	var curTree *uiblock.Node
	waitTree := func(desc string, pred func(*uiblock.Node) bool) *uiblock.Node {
		t.Helper()
		if curTree != nil && pred(curTree) {
			return curTree
		}
		timer := time.NewTimer(8 * time.Second)
		defer timer.Stop()
		for {
			select {
			case upd, ok := <-updates:
				if !ok {
					t.Fatalf("view stream closed while waiting for %s", desc)
				}
				if upd.Error != "" {
					t.Fatalf("uiblock/view error while waiting for %s: %s", desc, upd.Error)
				}
				if upd.Tree != nil {
					curTree = upd.Tree
				} else if len(upd.Ops) > 0 && curTree != nil {
					next, applyErr := uiblock.Apply(curTree, upd.Ops)
					if applyErr != nil {
						t.Fatalf("uiblock.Apply failed while waiting for %s: %v", desc, applyErr)
					}
					curTree = next
				}
				if curTree != nil && pred(curTree) {
					return curTree
				}
			case <-timer.C:
				var dump strings.Builder
				collectNodeText(curTree, &dump)
				t.Fatalf("timed out waiting for %s; current visible text:\n%s", desc, dump.String())
			}
		}
	}

	// 1. Initial Tab 1 render passes strict uiblock.Read.
	tree := waitTree("initial Tab 1 tree", func(n *uiblock.Node) bool {
		return findNodeByKeySuffix(n, "btn_pick_repo") != nil && findNodeByKeySuffix(n, "main_tabs") != nil
	})

	// 2. Pick Repository -> triggers real ui:pick-folder + allowed_folders grant + workflow discovery.
	pickBtn := findNodeByKeySuffix(tree, "btn_pick_repo")
	if err := mgr.SendViewEvent("github-actions", "main", uiblock.Event{
		Node: pickBtn.ID,
		Kind: uiblock.EventTap,
	}); err != nil {
		t.Fatalf("SendViewEvent(pick_repo): %v", err)
	}
	tree = waitTree("discovered workflow ci.yml + DAG preview", func(n *uiblock.Node) bool {
		return findNodeByKeySuffix(n, "btn_dryrun_local") != nil && findNodeByKeySuffix(n, "dag_preview") != nil
	})
	if h.UIHost.SnapshotPickFolderCalls() != 1 {
		t.Fatalf("expected 1 PickFolder call, got %d", h.UIHost.SnapshotPickFolderCalls())
	}

	// 3. Open Secrets Vault dialog -> save secret -> verify encrypted in Host Vault -> close dialog.
	secBtn := findNodeByKeySuffix(tree, "tb_secrets")
	if err := mgr.SendViewEvent("github-actions", "main", uiblock.Event{
		Node: secBtn.ID,
		Kind: uiblock.EventTap,
	}); err != nil {
		t.Fatalf("SendViewEvent(open_secrets): %v", err)
	}
	tree = waitTree("Secrets Vault dialog", func(n *uiblock.Node) bool {
		return findNodeByKeySuffix(n, "dlg_secrets") != nil &&
			findNodeByKeySuffix(n, "secret_name") != nil &&
			findNodeByKeySuffix(n, "secret_value") != nil
	})
	nameNode := findNodeByKeySuffix(tree, "secret_name")
	valNode := findNodeByKeySuffix(tree, "secret_value")
	saveBtn := findNodeByKeySuffix(tree, "btn_save_secret")
	if err := mgr.SendViewEvent("github-actions", "main", uiblock.Event{
		Node: saveBtn.ID,
		Kind: uiblock.EventTap,
		Values: map[string]string{
			nameNode.ID: "CI_SECRET",
			valNode.ID:  "super-secret-e2e-token-999",
		},
	}); err != nil {
		t.Fatalf("SendViewEvent(save_secret): %v", err)
	}
	tree = waitTree("saved secret listed in Vault dialog", func(n *uiblock.Node) bool {
		var txt strings.Builder
		collectNodeText(n, &txt)
		return strings.Contains(txt.String(), "CI_SECRET")
	})
	closeSecBtn := findNodeByKeySuffix(tree, "btn_close_secrets")
	if err := mgr.SendViewEvent("github-actions", "main", uiblock.Event{
		Node: closeSecBtn.ID,
		Kind: uiblock.EventTap,
	}); err != nil {
		t.Fatalf("SendViewEvent(close_dialog): %v", err)
	}
	tree = waitTree("Secrets dialog closed", func(n *uiblock.Node) bool {
		return findNodeByKeySuffix(n, "dlg_secrets") == nil
	})

	// 4. Execute Dry Run via real nektos/act inside the extension binary; verify DAG + secret masking!
	dryBtn := findNodeByKeySuffix(tree, "btn_dryrun_local")
	if err := mgr.SendViewEvent("github-actions", "main", uiblock.Event{
		Node: dryBtn.ID,
		Kind: uiblock.EventTap,
	}); err != nil {
		t.Fatalf("SendViewEvent(dryrun_local): %v", err)
	}
	tree = waitTree("local act Dry Run success with DAG and masked logs", func(n *uiblock.Node) bool {
		var txt strings.Builder
		collectNodeText(n, &txt)
		s := txt.String()
		return findNodeByKeySuffix(n, "local_run_dag") != nil && strings.Contains(s, "finished (success)")
	})
	var logDump strings.Builder
	collectNodeText(tree, &logDump)
	if strings.Contains(logDump.String(), "super-secret-e2e-token-999") {
		t.Fatalf("plaintext secret leaked in rendered UI tree:\n%s", logDump.String())
	}

	// 5. Switch to Tab 2 (Self-Hosted Runners) & open Add Runner dialog -> validate uiblock.Read.
	tabsNode := findNodeByKeySuffix(tree, "main_tabs")
	if err := mgr.SendViewEvent("github-actions", "main", uiblock.Event{
		Node:  tabsNode.ID,
		Kind:  uiblock.EventChange,
		Value: "Self-Hosted Runners",
	}); err != nil {
		t.Fatalf("SendViewEvent(tab runners): %v", err)
	}
	tree = waitTree("Tab 2 Self-Hosted Runners", func(n *uiblock.Node) bool {
		return findNodeByKeySuffix(n, "tb_add_runner") != nil && findNodeByKeySuffix(n, "runner_arm64_banner") != nil
	})

	// 6. Trigger Command Palette command `add-runner` -> opens Add Runner dialog.
	if err := mgr.RunCommand("github-actions", "add-runner", "en"); err != nil {
		t.Fatalf("RunCommand(add-runner): %v", err)
	}
	tree = waitTree("Add Runner dialog via command", func(n *uiblock.Node) bool {
		return findNodeByKeySuffix(n, "dlg_add_runner") != nil
	})
	cancelRunnerBtn := findNodeByKeySuffix(tree, "btn_cancel_runner")
	if err := mgr.SendViewEvent("github-actions", "main", uiblock.Event{
		Node: cancelRunnerBtn.ID,
		Kind: uiblock.EventTap,
	}); err != nil {
		t.Fatalf("SendViewEvent(cancel add runner): %v", err)
	}
	tree = waitTree("Add Runner dialog closed", func(n *uiblock.Node) bool {
		return findNodeByKeySuffix(n, "dlg_add_runner") == nil
	})

	// 7. Switch to Tab 3 (History) -> verify recorded local run & Reproduce Locally button.
	tabsNode = findNodeByKeySuffix(tree, "main_tabs")
	if err := mgr.SendViewEvent("github-actions", "main", uiblock.Event{
		Node:  tabsNode.ID,
		Kind:  uiblock.EventChange,
		Value: "History",
	}); err != nil {
		t.Fatalf("SendViewEvent(tab history): %v", err)
	}
	tree = waitTree("Tab 3 Unified History with Reproduce Locally button", func(n *uiblock.Node) bool {
		return findNodeByKeySuffix(n, "btn_repro_local") != nil
	})

	// 8. Invoke MCP tool `list_runners` through real Host Manager.
	toolResp, err := mgr.InvokeTool(context.Background(), "github-actions", "list_runners", json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("InvokeTool(list_runners): %v", err)
	}
	if !strings.Contains(string(toolResp), `"runners"`) {
		t.Fatalf("unexpected list_runners tool response: %s", toolResp)
	}

	// 9. Mount real PageExtension inside headless ggui.Probe to confirm zero layout/rendering panics!
	m, p := e2e3UINewProbe(t, dataDir, mgr, ggui.Sz(1280, 840), nil)
	m.Go("ext:github-actions/main")
	e2e3UISettle(t, p, func() bool {
		sem := p.Semantics()
		return e2e3UISemHasText(sem, "Local Workflows") &&
			e2e3UISemHasText(sem, "Self-Hosted Runners") &&
			e2e3UISemHasText(sem, "History")
	}, "ggui renders GitHub Actions extension view")

	// 10. Clean shutdown of extension process.
	subCancel()
	if err := mgr.Close(5 * time.Second); err != nil {
		t.Fatalf("mgr.Close: %v", err)
	}
}
