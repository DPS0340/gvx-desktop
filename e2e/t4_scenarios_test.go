//go:build e2e

package e2e

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/ironpark/ggui"

	"github.com/ironpark/gvx-desktop/internal/engine"
	"github.com/ironpark/gvx-desktop/internal/ext"
	"github.com/ironpark/gvx-desktop/internal/rungv"
	"github.com/ironpark/gvx-desktop/internal/sandbox"
	"github.com/ironpark/gvx-desktop/internal/uiblock"
)

// =============================================================================
// Tier 4 — 12 Real-World End-to-End Application Workload Scenarios
// =============================================================================

func TestT4_01_GitSourceExtensionFullLifecycle(t *testing.T) {
	dataDir := newShortTempDataDir(t)
	builder := &e2e4ImageBuilder{LogLines: []string{"building git source extension"}}
	mgr, _ := e2e4NewManager(t, builder, WithDataDir(dataDir))

	bareURL, sha := initBareGitRepo(t, map[string]string{
		"extension.toml": minimalManifestTOML("t4-git-app", "0.1.0", "ui:pick-folder", "storage"),
		"Dockerfile":     "FROM alpine:3.20\nCMD [\"/bin/sh\"]\n",
		"README.md":      "# T4 Git App\n",
	})
	httpsURL := e2e4ServeGitHTTPS(t, strings.TrimPrefix(bareURL, "file://"))

	// 1. InstallGit -> StateWaiting (pre-build approval gate; zero builds yet).
	info, err := mgr.InstallGit(httpsURL, "main")
	if err != nil {
		t.Fatalf("InstallGit: %v", err)
	}
	if info.State != ext.StateWaiting {
		t.Fatalf("expected StateWaiting after InstallGit, got %s", info.State)
	}
	if len(builder.SnapshotCalls()) != 0 {
		t.Fatal("expected zero builds before AllowAndBuild")
	}
	srcSpec, err := ext.LoadSource(ext.SourcePath(dataDir, "t4-git-app"))
	if err != nil || srcSpec.Commit != sha {
		t.Fatalf("source.json = (%+v, %v), want commit %q", srcSpec, err, sha)
	}

	// 2. AllowAndBuild -> packs context and builds tagged image gvx-ext/t4-git-app:0.1.0-<sha8>.
	_ = mgr.AllowAndBuild("t4-git-app")
	calls := builder.SnapshotCalls()
	wantTag := ext.ExtensionImageTag("t4-git-app", "0.1.0", sha)
	if len(calls) != 1 || len(calls[0].Req.Tags) == 0 || calls[0].Req.Tags[0] != wantTag {
		t.Fatalf("unexpected build calls: %+v (want tag %q)", calls, wantTag)
	}
	_ = mgr.Close(3 * time.Second)

	// 3. Verify PageExtension renders in desktop UI.
	p, m := e2e4StartProbeUI(t, dataDir, false, ggui.Sz(1280, 840))
	m.Go("ext:t4-git-app/main")
	sem := e2e4WaitSemantics(t, p, 5*time.Second, func(sem *ggui.SemTree) bool {
		return e2e4SemContains(sem, "t4-git-app") &&
			e2e4SemContains(sem, "Main") &&
			!e2e4SemContains(sem, "is not installed") &&
			!e2e4SemContains(sem, "Extension not found")
	})
	if !e2e4SemContains(sem, "t4-git-app") || !e2e4SemContains(sem, "Main") ||
		e2e4SemContains(sem, "is not installed") || e2e4SemContains(sem, "Extension not found") {
		t.Fatalf("PageExtension did not render expected extension view:\n%s", sem)
	}
	m.CloseExtensions(8 * time.Second)

	// 4. Full rungv sandbox execution -> SSE view -> gesture pick-folder -> sandbox mount restart -> CloseExtensions.
	t.Run("rungv_full_lifecycle", func(t *testing.T) {
		binPath, _, _ := requireRungv(t)
		sbxDir := newShortTempDataDir(t)
		sup := e2e4NewRungvSupervisor(t, filepath.Join(sbxDir, "sbx"), binPath)
		eng := engine.New(engine.Config{
			Dir:    filepath.Join(sbxDir, "eng"),
			Runner: engine.NewRunner(func() *rungv.Client { return &rungv.Client{Path: binPath} }, filepath.Join(sbxDir, "eng-run")),
		})
		if err := eng.Open(context.Background()); err != nil {
			t.Fatalf("eng.Open: %v", err)
		}
		t.Cleanup(func() { _ = eng.Close(5 * time.Second) })

		projectDir := e2e4NonReservedDir(t)
		if err := os.WriteFile(filepath.Join(projectDir, "repo.txt"), []byte("git-lifecycle-mounted"), 0o644); err != nil {
			t.Fatalf("write repo.txt: %v", err)
		}

		sdkSrc := `package main
import ("context"; "os"; "path/filepath"; "sync"; "github.com/ironpark/gvx-desktop/pkg/extsdk")
func main() {
	var mu sync.Mutex
	folder := ""
	app := extsdk.New()
	app.View("main", extsdk.View{
		Render: func(ctx *extsdk.ViewContext, ui *extsdk.UI) extsdk.Node {
			mu.Lock()
			f := folder
			mu.Unlock()
			if f == "" {
				if stored, ok, err := ctx.Host.StorageGet(ctx, "folder"); err == nil && ok && stored != "" {
					mu.Lock()
					folder = stored
					mu.Unlock()
					f = stored
				}
			}
			msg := "waiting-folder"
			if f != "" {
				if b, err := os.ReadFile(filepath.Join(f, "repo.txt")); err == nil {
					msg = string(b)
				}
			}
			return ui.Column(
				ui.Text(msg, extsdk.Key("msg")),
				ui.Button("Select Repo", extsdk.Key("pick"), extsdk.OnTap("pick")),
			)
		},
		On: extsdk.Handlers{
			"pick": func(ctx *extsdk.EventContext) error {
				dir, ok, err := ctx.Host.PickFolder(ctx)
				if err != nil || !ok { return err }
				if err := ctx.Host.StorageSet(ctx, "folder", dir); err != nil {
					return err
				}
				mu.Lock(); folder = dir; mu.Unlock()
				ctx.NotifyChanged()
				return nil
			},
		},
	})
	_ = app.Run(context.Background())
}`
		linuxBin := e2e4BuildLinuxSDKBinary(t, sdkSrc)
		binBytes, _ := os.ReadFile(linuxBin)
		liveRepo, _ := initBareGitRepo(t, map[string]string{
			"extension.toml": minimalManifestTOML("t4-git-live", "0.1.0", "ui:pick-folder", "storage"),
			"Dockerfile":     "FROM alpine:3.20\nCOPY ext /usr/local/bin/ext\nRUN chmod +x /usr/local/bin/ext\nEXPOSE 18900\nENTRYPOINT [\"/usr/local/bin/ext\"]\n",
			"ext":            string(binBytes),
		})
		liveHTTPS := e2e4ServeGitHTTPS(t, strings.TrimPrefix(liveRepo, "file://"))
		sbxMgr, h := e2e4NewManager(t, ext.NewEngineImageBuilder(eng.Handler), WithDataDir(sbxDir), WithSupervisor(sup))
		h.UIHost.PickFolderResult = projectDir
		if _, err := sbxMgr.InstallGit(liveHTTPS, "main"); err != nil {
			t.Fatalf("InstallGit(live): %v", err)
		}
		if err := sbxMgr.AllowAndBuild("t4-git-live"); err != nil {
			t.Fatalf("AllowAndBuild(live): %v", err)
		}
		e2e4WaitExtState(t, sbxMgr, "t4-git-live", 25*time.Second, func(i ext.ExtensionInfo) bool {
			return i.State == ext.StateRunning
		})
		if err := sbxMgr.SendViewEvent("t4-git-live", "main", uiblock.Event{Node: "/pick", Kind: uiblock.EventTap}); err != nil {
			t.Fatalf("SendViewEvent(pick): %v", err)
		}
		e2e4WaitExtState(t, sbxMgr, "t4-git-live", 25*time.Second, func(i ext.ExtensionInfo) bool {
			return i.State == ext.StateRunning && len(i.AllowedFolders) > 0
		})
		subCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		ch, err := sbxMgr.SubscribeView(subCtx, "t4-git-live", "main", "en")
		if err != nil {
			t.Fatalf("SubscribeView: %v", err)
		}
		var curTree *uiblock.Node
		var lastMsg *uiblock.Node
		foundMounted := false
		for !foundMounted {
			select {
			case upd, ok := <-ch:
				if !ok {
					t.Fatalf("SubscribeView closed before mounted repo.txt content (last msg=%+v)", lastMsg)
				}
				if upd.Tree != nil {
					curTree = upd.Tree
				} else if len(upd.Ops) > 0 && curTree != nil {
					if next, err := uiblock.Apply(curTree, upd.Ops); err == nil {
						curTree = next
					}
				}
				if curTree != nil {
					if n := uiblock.NodeOf(curTree, "/msg"); n != nil {
						lastMsg = n
						if n.Text == "git-lifecycle-mounted" {
							foundMounted = true
						}
					}
				}
			case <-subCtx.Done():
				t.Fatalf("timed out waiting for mounted repo.txt content (last msg=%+v)", lastMsg)
			}
		}
		if err := sbxMgr.Close(8 * time.Second); err != nil {
			t.Fatalf("sbxMgr.Close: %v", err)
		}
	})
}

func TestT4_02_ContainerInspectorScopedDocker(t *testing.T) {
	dataDir := newShortTempDataDir(t)
	mgr, h := newTestManager(t, WithDataDir(dataDir), WithDeveloperMode(true))

	projectDir := e2e4NonReservedDir(t)
	h.UIHost.PickFolderResult = projectDir

	src := `package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"

	"github.com/ironpark/gvx-desktop/pkg/extsdk"
)

func createContainer(ctx context.Context, binds []string) (int, string, string) {
	ep := os.Getenv("GVX_DOCKER_HOST")
	tr := &http.Transport{}
	base := ep
	if strings.HasPrefix(ep, "unix://") {
		sock := strings.TrimPrefix(ep, "unix://")
		tr.DialContext = func(c context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(c, "unix", sock)
		}
		base = "http://docker"
	}
	body, _ := json.Marshal(map[string]any{
		"Image":      "alpine:3.20",
		"HostConfig": map[string]any{"Binds": binds},
	})
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, base+"/containers/create", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	resp, err := (&http.Client{Transport: tr}).Do(req)
	if err != nil {
		return 0, "", err.Error()
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	var out struct{ Id string }
	_ = json.Unmarshal(raw, &out)
	return resp.StatusCode, out.Id, string(raw)
}

func main() {
	var mu sync.Mutex
	containerID := ""
	folder := ""
	app := extsdk.New()
	app.View("main", extsdk.View{
		Render: func(c *extsdk.ViewContext, ui *extsdk.UI) extsdk.Node {
			mu.Lock()
			cid := containerID
			mu.Unlock()
			return ui.Column(
				ui.Text("cid="+cid, extsdk.Key("cid")),
				ui.Button("Pick Folder", extsdk.Key("pick"), extsdk.OnTap("pick")),
				ui.Button("Run Inspector", extsdk.Key("run"), extsdk.OnTap("run")),
				ui.Button("Open Shell", extsdk.Key("shell"), extsdk.OnTap("shell")),
			)
		},
		On: extsdk.Handlers{
			"pick": func(ctx *extsdk.EventContext) error {
				dir, ok, err := ctx.Host.PickFolder(ctx)
				if err != nil || !ok { return err }
				mu.Lock(); folder = dir; mu.Unlock()
				return nil
			},
			"run": func(ctx *extsdk.EventContext) error {
				mu.Lock(); f := folder; mu.Unlock()
				// Approved bind must succeed.
				code, cid, raw := createContainer(ctx, []string{f + ":/workspace"})
				if code != 201 || cid == "" {
					return fmt.Errorf("approved bind failed: %d %s", code, raw)
				}
				// Unapproved /etc bind must fail.
				badCode, _, badRaw := createContainer(ctx, []string{"/etc:/etc"})
				if badCode < 400 {
					return fmt.Errorf("expected /etc bind rejected, got %d %s", badCode, badRaw)
				}
				mu.Lock(); containerID = cid; mu.Unlock()
				if err := ctx.Host.SetBadge(ctx, extsdk.BadgeRequest{Text: "1", Tone: "success"}); err != nil {
					return err
				}
				ctx.NotifyChanged()
				return nil
			},
			"shell": func(ctx *extsdk.EventContext) error {
				mu.Lock(); cid := containerID; mu.Unlock()
				return ctx.Host.OpenTerminal(ctx, extsdk.OpenTerminalRequest{
					TargetType: "container", TargetID: cid, Shell: "/bin/sh",
				})
			},
		},
	})
	_ = app.Run(context.Background())
}
`
	extDir := e2e4CreateBinaryExt(t, t.TempDir(), "t4-inspector", binaryManifestTOML("t4-inspector", "0.1.0", "bin/ext", "docker", "ui:pick-folder", "ui:terminal"), src)
	if _, err := mgr.InstallLocal(extDir); err != nil {
		t.Fatalf("InstallLocal: %v", err)
	}
	if err := mgr.AllowAndBuild("t4-inspector"); err != nil {
		t.Fatalf("AllowAndBuild: %v", err)
	}
	e2e4WaitExtState(t, mgr, "t4-inspector", 10*time.Second, func(i ext.ExtensionInfo) bool {
		return i.State == ext.StateRunning
	})

	if err := mgr.SendViewEvent("t4-inspector", "main", uiblock.Event{Node: "/pick", Kind: uiblock.EventTap}); err != nil {
		t.Fatalf("pick: %v", err)
	}
	if err := mgr.SendViewEvent("t4-inspector", "main", uiblock.Event{Node: "/run", Kind: uiblock.EventTap}); err != nil {
		t.Fatalf("run: %v", err)
	}
	if err := mgr.SendViewEvent("t4-inspector", "main", uiblock.Event{Node: "/shell", Kind: uiblock.EventTap}); err != nil {
		t.Fatalf("shell: %v", err)
	}

	var createReqs []DockerRequest
	for _, r := range h.DockerBackend.SnapshotRequests() {
		if r.Method == http.MethodPost && strings.HasSuffix(r.Path, "/containers/create") {
			createReqs = append(createReqs, r)
		}
	}
	if len(createReqs) != 1 {
		t.Fatalf("expected exactly 1 proxied Docker container create (/etc must be blocked), got %d", len(createReqs))
	}
	labels, _ := createReqs[0].CreatePayload["Labels"].(map[string]any)
	if labels["io.gvx.extension"] != "t4-inspector" {
		t.Fatalf("missing io.gvx.extension label: %+v", createReqs[0].CreatePayload)
	}
	info, _ := e2e4FindExtInfo(mgr, "t4-inspector")
	if info.Badge.Text != "1" {
		t.Fatalf("expected sidebar badge '1', got %+v", info.Badge)
	}
	terms := h.UIHost.SnapshotTerminals()
	if len(terms) != 1 || terms[0].TargetType != "container" || terms[0].TargetID != "mock-container-1" {
		t.Fatalf("expected container terminal call, got %+v", terms)
	}
}

func TestT4_03_CIRunnerSandboxOrchestrator(t *testing.T) {
	dataDir := newShortTempDataDir(t)
	st := newTestStore(t, dataDir)
	foreignSpec := sandbox.Spec{ID: "foreign-sbx", Name: "foreign-sbx", Image: "alpine:3.20"}
	if err := st.SaveSandboxes([]sandbox.Spec{foreignSpec}); err != nil {
		t.Fatalf("SaveSandboxes: %v", err)
	}
	sup := e2e4NewRungvSupervisor(t, filepath.Join(dataDir, "sbx"), "")
	mgr, _ := newTestManager(t, WithDataDir(dataDir), WithDeveloperMode(true), WithSupervisor(sup))

	otherSrc := `package main
import ("context"; "fmt"; "github.com/ironpark/gvx-desktop/pkg/extsdk")
func main() {
	app := extsdk.New()
	app.Command("try-steal", func(ctx *extsdk.CommandContext) error {
		set, err := ctx.Host.Settings(ctx)
		if err != nil {
			return fmt.Errorf("settings: %w", err)
		}
		targetID := set.String("target_sbx")
		if targetID == "" {
			targetID = "foreign-sbx"
		}
		list, err := ctx.Host.SandboxList(ctx, true)
		if err != nil {
			return fmt.Errorf("list: %w", err)
		}
		for _, item := range list {
			if item.ID == targetID {
				return fmt.Errorf("foreign sandbox %s leaked into owned list", targetID)
			}
		}
		if _, err := ctx.Host.SandboxStop(ctx, targetID); !extsdk.IsPermissionDenied(err) {
			return fmt.Errorf("expected 403 on %s stop, got %v", targetID, err)
		}
		if err := ctx.Host.SandboxDelete(ctx, targetID); !extsdk.IsPermissionDenied(err) {
			return fmt.Errorf("expected 403 on %s delete, got %v", targetID, err)
		}
		if _, err := ctx.Host.SnapshotCreate(ctx, targetID, extsdk.CreateSnapshotRequest{Name: "steal"}); !extsdk.IsPermissionDenied(err) {
			return fmt.Errorf("expected 403 on %s snapshot, got %v", targetID, err)
		}
		return nil
	})
	_ = app.Run(context.Background())
}`
	otherManifest := binaryManifestTOML("t4-ci-other", "0.1.0", "bin/ext", "sandboxes", "snapshots") + `
[[contributes.command]]
id = "try-steal"
title = "Try Steal"

[contributes.settings]
target_sbx = { type = "string", label = "Target Sandbox", default = "foreign-sbx" }
`
	otherDir := e2e4CreateBinaryExt(t, t.TempDir(), "t4-ci-other", otherManifest, otherSrc)
	if _, err := mgr.InstallLocal(otherDir); err != nil {
		t.Fatalf("InstallLocal(other): %v", err)
	}
	if err := mgr.AllowAndBuild("t4-ci-other"); err != nil {
		t.Fatalf("AllowAndBuild(other): %v", err)
	}
	e2e4WaitExtState(t, mgr, "t4-ci-other", 10*time.Second, func(i ext.ExtensionInfo) bool {
		return i.State == ext.StateRunning
	})
	if err := mgr.RunCommand("t4-ci-other", "try-steal", "en"); err != nil {
		t.Fatalf("RunCommand(try-steal): %v", err)
	}

	t.Run("rungv_ci_runner_ephemeral_snapshot", func(t *testing.T) {
		requireRungv(t)
		ciSrc := `package main
import ("context"; "fmt"; "os"; "path/filepath"; "github.com/ironpark/gvx-desktop/pkg/extsdk")
func main() {
	app := extsdk.New()
	app.Command("run-ci", func(ctx *extsdk.CommandContext) error {
		sb, err := ctx.Host.SandboxCreate(ctx, extsdk.CreateSandboxRequest{
			Name: "ci-job-1", Image: "alpine:3.20", Command: "sh -c 'echo ci-boot; sleep 30'", Ephemeral: true,
		})
		if err != nil { return err }
		if err := os.WriteFile(filepath.Join(ctx.DataDir, "ci_sb_id.txt"), []byte(sb.ID), 0o600); err != nil {
			return err
		}
		if err := ctx.Host.StorageSet(ctx, "last_sb", sb.ID); err != nil { return err }
		if _, err := ctx.Host.SnapshotCreate(ctx, sb.ID, extsdk.CreateSnapshotRequest{Name: "warm", LeaveRunning: true}); err != nil {
			return fmt.Errorf("snapshot: %w", err)
		}
		res, err := ctx.Host.Restore(ctx, sb.ID, extsdk.RestoreRequest{Snapshot: "warm", Exec: []string{"echo", "restored-job"}})
		if err != nil { return fmt.Errorf("restore: %w", err) }
		if res.Exec == nil || res.Exec.ExitCode != 0 {
			return fmt.Errorf("restore exec failed: %+v", res)
		}
		return nil
	})
	_ = app.Run(context.Background())
}`
		ciManifest := binaryManifestTOML("t4-ci-runner", "0.1.0", "bin/ext", "sandboxes", "snapshots", "storage") +
			"\n[[contributes.command]]\nid = \"run-ci\"\ntitle = \"Run CI\"\n"
		ciDir := e2e4CreateBinaryExt(t, t.TempDir(), "t4-ci-runner", ciManifest, ciSrc)
		if _, err := mgr.InstallLocal(ciDir); err != nil {
			t.Fatalf("InstallLocal(ci): %v", err)
		}
		if err := mgr.AllowAndBuild("t4-ci-runner"); err != nil {
			t.Fatalf("AllowAndBuild(ci): %v", err)
		}
		e2e4WaitExtState(t, mgr, "t4-ci-runner", 10*time.Second, func(i ext.ExtensionInfo) bool {
			return i.State == ext.StateRunning
		})
		if err := mgr.RunCommand("t4-ci-runner", "run-ci", "en"); err != nil {
			t.Fatalf("RunCommand(run-ci): %v", err)
		}
		ciIDBytes, err := os.ReadFile(filepath.Join(ext.FilesDir(dataDir, "t4-ci-runner"), "ci_sb_id.txt"))
		if err != nil || strings.TrimSpace(string(ciIDBytes)) == "" {
			t.Fatalf("read ci_sb_id.txt: %v", err)
		}
		ciID := strings.TrimSpace(string(ciIDBytes))
		if err := mgr.SetSetting("t4-ci-other", "target_sbx", ciID); err != nil {
			t.Fatalf("SetSetting(target_sbx): %v", err)
		}
		if err := mgr.RunCommand("t4-ci-other", "try-steal", "en"); err != nil {
			t.Fatalf("cross-extension isolation check against %s failed: %v", ciID, err)
		}
		if err := mgr.Close(8 * time.Second); err != nil {
			t.Fatalf("mgr.Close: %v", err)
		}
		specs, _ := st.Sandboxes()
		hasForeign := false
		for _, sp := range specs {
			if sp.Name == "ci-job-1" {
				t.Fatalf("ephemeral CI sandbox %s was not cleaned up on shutdown", sp.ID)
			}
			if sp.ID == "foreign-sbx" {
				hasForeign = true
			}
		}
		if !hasForeign {
			t.Fatal("non-ephemeral foreign-sbx was unexpectedly removed")
		}
	})
}

func TestT4_04_SecretBackedAPIClientKeychainLifecycle(t *testing.T) {
	dataDir := newShortTempDataDir(t)
	src := `package main

import (
	"context"
	"fmt"

	"github.com/ironpark/gvx-desktop/pkg/extsdk"
)

func main() {
	app := extsdk.New()
	app.Command("seed", func(ctx *extsdk.CommandContext) error {
		set, err := ctx.Host.Settings(ctx)
		if err != nil { return err }
		if !set.IsSet("api_key") || set.String("api_key") != "" {
			return fmt.Errorf("expected masked api_key in settings, got %+v", set.Raw())
		}
		return ctx.Host.SecretSet(ctx, "runtime_tok", "rt-secret-999")
	})
	app.Command("verify", func(ctx *extsdk.CommandContext) error {
		k1, ok1, err := ctx.Host.SecretGet(ctx, "setting:api_key")
		if err != nil || !ok1 || k1 != "api-key-123" {
			return fmt.Errorf("setting:api_key = (%q, %v, %v)", k1, ok1, err)
		}
		k2, ok2, err := ctx.Host.SecretGet(ctx, "runtime_tok")
		if err != nil || !ok2 || k2 != "rt-secret-999" {
			return fmt.Errorf("runtime_tok = (%q, %v, %v)", k2, ok2, err)
		}
		return nil
	})
	_ = app.Run(context.Background())
}
`
	manifest := binaryManifestTOML("t4-secrets", "0.1.0", "bin/ext", "secrets") + `
[contributes.settings]
api_key = { type = "secret", label = "API Key" }

[[contributes.command]]
id = "seed"
title = "Seed"
[[contributes.command]]
id = "verify"
title = "Verify"
`
	extDir := e2e4CreateBinaryExt(t, t.TempDir(), "t4-secrets", manifest, src)

	// Phase A: GVX_KEYCHAIN=file fallback -> 0600 secrets.json.
	mgrA, _ := newTestManager(t, WithDataDir(dataDir), WithKeychain(nil, "Keychain disabled"), WithDeveloperMode(true))
	if _, err := mgrA.InstallLocal(extDir); err != nil {
		t.Fatalf("InstallLocal: %v", err)
	}
	if err := mgrA.AllowAndBuild("t4-secrets"); err != nil {
		t.Fatalf("AllowAndBuild: %v", err)
	}
	e2e4WaitExtState(t, mgrA, "t4-secrets", 10*time.Second, func(i ext.ExtensionInfo) bool {
		return i.State == ext.StateRunning
	})
	if err := mgrA.SetSetting("t4-secrets", "api_key", "api-key-123"); err != nil {
		t.Fatalf("SetSetting(api_key): %v", err)
	}
	if err := mgrA.RunCommand("t4-secrets", "seed", "en"); err != nil {
		t.Fatalf("RunCommand(seed): %v", err)
	}
	secFile := ext.SecretsFilePath(dataDir, "t4-secrets")
	assertFilePerm(t, secFile, 0o600)
	_ = mgrA.Close(3 * time.Second)

	// Phase B: Restart with keychain.Fake -> migrates both secrets and deletes secrets.json.
	fakeKC := newFakeKeychain(t)
	mgrB, _ := newTestManager(t, WithDataDir(dataDir), WithKeychain(fakeKC, ""), WithDeveloperMode(true))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	_ = mgrB.Start(ctx)
	e2e4WaitExtState(t, mgrB, "t4-secrets", 10*time.Second, func(i ext.ExtensionInfo) bool {
		return i.State == ext.StateRunning
	})
	if err := mgrB.RunCommand("t4-secrets", "verify", "en"); err != nil {
		t.Fatalf("RunCommand(verify): %v", err)
	}
	if _, err := os.Stat(secFile); !os.IsNotExist(err) {
		t.Fatalf("expected secrets.json deleted after keychain migration, err=%v", err)
	}
	if len(fakeKC.Entries()) != 2 {
		t.Fatalf("expected 2 migrated entries in keychain.Fake, got %+v", fakeKC.Entries())
	}

	// Phase C: Uninstall(purge=true) scrubs all keychain entries.
	if err := mgrB.Uninstall("t4-secrets", true); err != nil {
		t.Fatalf("Uninstall(true): %v", err)
	}
	if len(fakeKC.Entries()) != 0 {
		t.Fatalf("expected empty keychain after purge, got %+v", fakeKC.Entries())
	}
}

func TestT4_05_DeveloperModeLocalBinaryWorkflow(t *testing.T) {
	dataDir := newShortTempDataDir(t)
	src := `package main

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/ironpark/gvx-desktop/pkg/extsdk"
)

func main() {
	var mu sync.Mutex
	ticks := 0
	cmdRan := false
	app := extsdk.New()
	app.View("main", extsdk.View{
		Render: func(c *extsdk.ViewContext, ui *extsdk.UI) extsdk.Node {
			mu.Lock()
			tk := ticks
			cr := cmdRan
			mu.Unlock()
			return ui.Column(
				ui.Text(fmt.Sprintf("stream=%d|cmd=%v", tk, cr), extsdk.Key("st")),
				ui.Input("base", extsdk.Key("note"), extsdk.Placeholder("Workflow Note")),
			)
		},
	})
	app.Command("dev-cmd", func(ctx *extsdk.CommandContext) error {
		mu.Lock(); cmdRan = true; mu.Unlock()
		ctx.NotifyChanged("main")
		return nil
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		t := time.NewTicker(50 * time.Millisecond)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				mu.Lock(); ticks++; mu.Unlock()
				app.NotifyChanged("main")
			}
		}
	}()
	_ = app.Run(ctx)
}
`
	manifest := binaryManifestTOML("t4-dev", "0.1.0", "bin/ext") +
		"\n[[contributes.command]]\nid = \"dev-cmd\"\ntitle = \"T4 Dev Command\"\n"
	extDir := e2e4CreateBinaryExt(t, t.TempDir(), "t4-dev", manifest, src)

	mgr, _ := newTestManager(t, WithDataDir(dataDir), WithDeveloperMode(true))
	if _, err := mgr.InstallLocal(extDir); err != nil {
		t.Fatalf("InstallLocal: %v", err)
	}
	if err := mgr.AllowAndBuild("t4-dev"); err != nil {
		t.Fatalf("AllowAndBuild: %v", err)
	}
	_ = mgr.Close(3 * time.Second)

	p, m := e2e4StartProbeUI(t, dataDir, true, ggui.Sz(1280, 840))
	m.Go("ext:t4-dev/main")
	e2e4WaitSemantics(t, p, 10*time.Second, func(sem *ggui.SemTree) bool {
		return e2e4SemContains(sem, "stream=") && e2e4SemContains(sem, "Unsandboxed")
	})

	// Type in blockForm while SSE Follow ticks run.
	tf, ok := p.FindRole(ggui.RoleTextField, "Workflow Note")
	if !ok {
		t.Fatalf("Workflow Note field missing:\n%s", p.Semantics())
	}
	p.Click(tf.Center())
	p.Text("-live")
	time.Sleep(250 * time.Millisecond)
	p.Frame()
	if node, ok := p.Semantics().Find(ggui.RoleTextField, "Workflow Note"); !ok || !strings.Contains(node.Value, "-live") {
		t.Fatalf("blockForm lost typed draft across Follow ticks: %+v", node)
	}

	// Open ⌘K palette and execute contributed command.
	m.PaletteQuery.Set("T4 Dev Command")
	m.PaletteOpen.Set(true)
	e2e4WaitSemantics(t, p, 5*time.Second, func(sem *ggui.SemTree) bool {
		_, found := sem.Find(ggui.RoleMenuItem, "T4 Dev Command")
		return found
	})
	p.Tap("T4 Dev Command")
	e2e4WaitSemantics(t, p, 5*time.Second, func(sem *ggui.SemTree) bool {
		return e2e4SemContains(sem, "|cmd=true")
	})
	m.CloseExtensions(5 * time.Second)

	// Disabling DeveloperMode blocks binary extension start.
	mgrOff, _ := newTestManager(t, WithDataDir(dataDir), WithDeveloperMode(false))
	ctxOff, cancelOff := context.WithCancel(context.Background())
	defer cancelOff()
	_ = mgrOff.Start(ctxOff)
	if err := mgrOff.Enable("t4-dev"); err == nil {
		t.Fatal("expected Enable(t4-dev) to fail when DeveloperMode is false")
	}
	info, ok := e2e4FindExtInfo(mgrOff, "t4-dev")
	if !ok {
		t.Fatal("expected t4-dev in extension list")
	}
	if info.State == ext.StateRunning {
		t.Fatalf("binary runtime must be blocked when DeveloperMode is false, got %s", info.State)
	}
}

func TestT4_06_PrebuiltOCIImageWebDashboard(t *testing.T) {
	dataDir := newShortTempDataDir(t)
	mgr, h := newTestManager(t, WithDataDir(dataDir), WithDeveloperMode(true))

	// 1. InstallImage -> StateWaiting -> AllowAndBuild pulls via ImagePuller & KeepImages protects from prune.
	const imageRef = "ghcr.io/acme/web-dash:1.0.0"
	imgInfo, err := mgr.InstallImage(imageRef)
	if err != nil {
		t.Fatalf("InstallImage: %v", err)
	}
	if imgInfo.State != ext.StateWaiting {
		t.Fatalf("expected StateWaiting before approval, got %s", imgInfo.State)
	}
	if len(h.ImagePuller.SnapshotPulledRefs()) != 0 {
		t.Fatal("ImagePuller.Pull must not run before user approval")
	}

	_ = mgr.AllowAndBuild(imgInfo.ID)
	pulled := h.ImagePuller.SnapshotPulledRefs()
	if !slices.Contains(pulled, imageRef) {
		t.Fatalf("pulled refs = %v, want %q", pulled, imageRef)
	}
	if !slices.Contains(mgr.KeepImages(), imageRef) {
		t.Fatalf("KeepImages = %v, expected %q", mgr.KeepImages(), imageRef)
	}

	runner := e2e4NewFakeEngineRunner()
	runner.SeedImage("sha256:"+strings.Repeat("a", 64), "ghcr.io/acme/web-dash", "1.0.0")
	runner.SeedImage("sha256:"+strings.Repeat("b", 64), "stale/other", "0.1")
	eng := engine.New(engine.Config{
		Dir:        filepath.Join(dataDir, "eng"),
		Runner:     runner,
		KeepImages: mgr.KeepImages,
	})
	if err := eng.Open(context.Background()); err != nil {
		t.Fatalf("eng.Open: %v", err)
	}
	defer func() { _ = eng.Close(3 * time.Second) }()

	req, _ := http.NewRequest(http.MethodPost, `/v1.52/images/prune?filters={"dangling":["false"]}`, nil)
	rec := &e2e4MemRespRecorder{header: make(http.Header)}
	eng.Handler().ServeHTTP(rec, req)
	if rec.code != http.StatusOK {
		t.Fatalf("prune HTTP %d: %s", rec.code, rec.body.String())
	}
	if rem := runner.RemainingRefs(); len(rem) != 1 || rem[0] != imageRef {
		t.Fatalf("expected pulled extension image preserved by KeepImages, remaining=%v", rem)
	}

	// 2. Verify active LocalProxyRoutes + RevokePermissions route removal on a running web_ui extension.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()
	webPort := ln.Addr().(*net.TCPAddr).Port
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "dashboard-ok")
	})}
	go func() { _ = srv.Serve(ln) }()
	defer srv.Close()

	webSrc := `package main
import ("context"; "github.com/ironpark/gvx-desktop/pkg/extsdk")
func main() {
	app := extsdk.New()
	app.View("main", extsdk.View{Render: func(c *extsdk.ViewContext, ui *extsdk.UI) extsdk.Node {
		return ui.Column(ui.Text("dash-running"))
	}})
	_ = app.Run(context.Background())
}`
	webManifest := binaryManifestTOML("t4-webdash", "1.0.0", "bin/ext", "localproxy", "network") + fmt.Sprintf(`
[contributes.web_ui]
port = %d
path = "/dash"
title = "Open Dashboard"
`, webPort)
	webDir := e2e4CreateBinaryExt(t, t.TempDir(), "t4-webdash", webManifest, webSrc)
	if _, err := mgr.InstallLocal(webDir); err != nil {
		t.Fatalf("InstallLocal(webdash): %v", err)
	}
	if err := mgr.AllowAndBuild("t4-webdash"); err != nil {
		t.Fatalf("AllowAndBuild(webdash): %v", err)
	}
	e2e4WaitExtState(t, mgr, "t4-webdash", 10*time.Second, func(i ext.ExtensionInfo) bool {
		return i.State == ext.StateRunning
	})

	routes := mgr.LocalProxyRoutes()
	hasRoute := false
	for _, r := range routes {
		if strings.Contains(r.Host, "t4-webdash") && strings.HasSuffix(r.Host, ".localhost") {
			hasRoute = true
		}
	}
	if !hasRoute {
		t.Fatalf("expected active localproxy route for t4-webdash, got %+v", routes)
	}

	if err := mgr.RevokePermissions("t4-webdash"); err != nil {
		t.Fatalf("RevokePermissions: %v", err)
	}
	e2e4WaitExtState(t, mgr, "t4-webdash", 5*time.Second, func(i ext.ExtensionInfo) bool {
		return !i.State.IsActive()
	})
	for _, r := range mgr.LocalProxyRoutes() {
		if strings.Contains(r.Host, "t4-webdash") {
			t.Fatalf("expected localproxy route removed after RevokePermissions, got %+v", r)
		}
	}

	// 3. Live rungv sandbox web_ui + localproxy + RevokePermissions stop verification.
	t.Run("rungv_oci_web_ui", func(t *testing.T) {
		binPath, _, _ := requireRungv(t)
		sbxDir := newShortTempDataDir(t)
		sup := e2e4NewRungvSupervisor(t, filepath.Join(sbxDir, "sbx"), binPath)
		sbxEng := engine.New(engine.Config{
			Dir:    filepath.Join(sbxDir, "eng"),
			Runner: engine.NewRunner(func() *rungv.Client { return &rungv.Client{Path: binPath} }, filepath.Join(sbxDir, "eng-run")),
		})
		if err := sbxEng.Open(context.Background()); err != nil {
			t.Fatalf("sbxEng.Open: %v", err)
		}
		t.Cleanup(func() { _ = sbxEng.Close(5 * time.Second) })

		sbxSrc := `package main
import ("context"; "net/http"; "github.com/ironpark/gvx-desktop/pkg/extsdk")
func main() {
	go func() {
		_ = http.ListenAndServe("127.0.0.1:8080", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_, _ = w.Write([]byte("sbx-dash"))
		}))
	}()
	app := extsdk.New()
	app.View("main", extsdk.View{Render: func(c *extsdk.ViewContext, ui *extsdk.UI) extsdk.Node {
		return ui.Column(ui.Text("sbx-webdash-ok"))
	}})
	_ = app.Run(context.Background())
}`
		linuxBin := e2e4BuildLinuxSDKBinary(t, sbxSrc)
		binBytes, _ := os.ReadFile(linuxBin)
		sbxManifest := minimalManifestTOML("t4-webdash-sbx", "1.0.0", "localproxy", "network") + `
[contributes.web_ui]
port = 8080
path = "/dash"
title = "Sandbox Dashboard"
`
		sbxExtDir := filepath.Join(t.TempDir(), "t4-webdash-sbx")
		writeExtFixture(t, sbxExtDir, sbxManifest, map[string]string{
			"Dockerfile": "FROM alpine:3.20\nCOPY ext /usr/local/bin/ext\nRUN chmod +x /usr/local/bin/ext\nEXPOSE 18900\nENTRYPOINT [\"/usr/local/bin/ext\"]\n",
			"ext":        string(binBytes),
		})
		sbxMgr, _ := e2e4NewManager(t, ext.NewEngineImageBuilder(sbxEng.Handler), WithDataDir(sbxDir), WithSupervisor(sup))
		if _, err := sbxMgr.InstallLocal(sbxExtDir); err != nil {
			t.Fatalf("InstallLocal(sbx): %v", err)
		}
		if err := sbxMgr.AllowAndBuild("t4-webdash-sbx"); err != nil {
			t.Fatalf("AllowAndBuild(sbx): %v", err)
		}
		e2e4WaitExtState(t, sbxMgr, "t4-webdash-sbx", 25*time.Second, func(i ext.ExtensionInfo) bool {
			return i.State == ext.StateRunning
		})
		liveRoute := false
		for _, r := range sbxMgr.LocalProxyRoutes() {
			if strings.Contains(r.Host, "t4-webdash-sbx") {
				liveRoute = true
			}
		}
		if !liveRoute {
			t.Fatalf("expected active localproxy route for sandbox t4-webdash-sbx, got %+v", sbxMgr.LocalProxyRoutes())
		}
		if err := sbxMgr.RevokePermissions("t4-webdash-sbx"); err != nil {
			t.Fatalf("RevokePermissions(sbx): %v", err)
		}
		e2e4WaitExtState(t, sbxMgr, "t4-webdash-sbx", 10*time.Second, func(i ext.ExtensionInfo) bool {
			return !i.State.IsActive()
		})
		for _, r := range sbxMgr.LocalProxyRoutes() {
			if strings.Contains(r.Host, "t4-webdash-sbx") {
				t.Fatalf("expected sandbox localproxy route removed after RevokePermissions, got %+v", r)
			}
		}
	})
}

func TestT4_07_MultiExtensionIsolationAndCrashResilience(t *testing.T) {
	dataDir := newShortTempDataDir(t)
	mgr, h := newTestManager(t, WithDataDir(dataDir), WithDeveloperMode(true))

	alphaSrc := `package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"strings"

	"github.com/ironpark/gvx-desktop/pkg/extsdk"
)

func main() {
	app := extsdk.New()
	app.View("main", extsdk.View{Render: func(c *extsdk.ViewContext, ui *extsdk.UI) extsdk.Node {
		return ui.Column(ui.Text("alpha-alive", extsdk.Key("st")))
	}})
	app.Command("write-isolated", func(ctx *extsdk.CommandContext) error {
		if err := ctx.Host.StorageSet(ctx, "shared_key", "alpha-val"); err != nil {
			return err
		}
		if err := ctx.Host.SecretSet(ctx, "shared_sec", "alpha-sec"); err != nil {
			return err
		}
		// Spoofing ext-beta's label in Docker proxy must be rejected.
		ep := os.Getenv("GVX_DOCKER_HOST")
		tr := &http.Transport{}
		base := ep
		if strings.HasPrefix(ep, "unix://") {
			sock := strings.TrimPrefix(ep, "unix://")
			tr.DialContext = func(c context.Context, _, _ string) (net.Conn, error) {
				return (&net.Dialer{}).DialContext(c, "unix", sock)
			}
			base = "http://docker"
		}
		body, _ := json.Marshal(map[string]any{
			"Image":  "alpine:3.20",
			"Labels": map[string]any{"io.gvx.extension": "t4-beta"},
		})
		req, _ := http.NewRequestWithContext(ctx, http.MethodPost, base+"/containers/create", bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		resp, err := (&http.Client{Transport: tr}).Do(req)
		if err != nil { return err }
		_ = resp.Body.Close()
		if resp.StatusCode < 400 {
			return fmt.Errorf("expected label spoof of t4-beta rejected, got %d", resp.StatusCode)
		}
		return nil
	})
	app.Command("ping", func(ctx *extsdk.CommandContext) error { return nil })
	_ = app.Run(context.Background())
}
`
	betaCrashSrc := `package main
import "os"
func main() { os.Exit(1) }
`
	alphaManifest := binaryManifestTOML("t4-alpha", "0.1.0", "bin/ext", "storage", "secrets", "docker") + `
[[contributes.command]]
id = "write-isolated"
title = "Write"
[[contributes.command]]
id = "ping"
title = "Ping"
`
	alphaDir := e2e4CreateBinaryExt(t, t.TempDir(), "t4-alpha", alphaManifest, alphaSrc)
	betaDir := e2e4CreateBinaryExt(t, t.TempDir(), "t4-beta", binaryManifestTOML("t4-beta", "0.1.0", "bin/ext", "storage", "secrets"), betaCrashSrc)

	if _, err := mgr.InstallLocal(alphaDir); err != nil {
		t.Fatalf("InstallLocal(alpha): %v", err)
	}
	if err := mgr.AllowAndBuild("t4-alpha"); err != nil {
		t.Fatalf("AllowAndBuild(alpha): %v", err)
	}
	e2e4WaitExtState(t, mgr, "t4-alpha", 10*time.Second, func(i ext.ExtensionInfo) bool {
		return i.State == ext.StateRunning
	})
	if err := mgr.RunCommand("t4-alpha", "write-isolated", "en"); err != nil {
		t.Fatalf("RunCommand(write-isolated): %v", err)
	}

	// Verify isolated keychain service namespace and data dir.
	wantSvc := ext.KeychainService(dataDir, "t4-alpha")
	svcFound := false
	for _, e := range h.Keychain.Entries() {
		if e[0] == wantSvc {
			svcFound = true
		}
	}
	if !svcFound {
		t.Fatalf("expected keychain service %q, entries=%+v", wantSvc, h.Keychain.Entries())
	}

	// Crash t4-beta 3 times into StateFailed and verify t4-alpha stays completely unaffected.
	if _, err := mgr.InstallLocal(betaDir); err != nil {
		t.Fatalf("InstallLocal(beta): %v", err)
	}
	_ = mgr.AllowAndBuild("t4-beta")
	e2e4WaitExtState(t, mgr, "t4-beta", 20*time.Second, func(i ext.ExtensionInfo) bool {
		return i.State == ext.StateFailed
	})

	alphaInfo, ok := e2e4FindExtInfo(mgr, "t4-alpha")
	if !ok || alphaInfo.State != ext.StateRunning {
		t.Fatalf("t4-alpha disturbed by t4-beta crashes: %+v", alphaInfo)
	}
	if err := mgr.RunCommand("t4-alpha", "ping", "en"); err != nil {
		t.Fatalf("t4-alpha unresponsive after t4-beta failed: %v", err)
	}
}

func TestT4_08_SupplyChainUpdateEscalationDefense(t *testing.T) {
	dataDir := newShortTempDataDir(t)
	mgr, h := newTestManager(t, WithDataDir(dataDir), WithDeveloperMode(true))

	src := `package main
import ("context"; "github.com/ironpark/gvx-desktop/pkg/extsdk")
func main() {
	app := extsdk.New()
	app.View("main", extsdk.View{Render: func(c *extsdk.ViewContext, ui *extsdk.UI) extsdk.Node {
		return ui.Column(ui.Text("supply-ok", extsdk.Key("ok")))
	}})
	_ = app.Run(context.Background())
}`
	binData, err := os.ReadFile(buildFixtureSDKBinary(t, src))
	if err != nil {
		t.Fatalf("read fixture binary: %v", err)
	}
	bareURL, _ := e2e4InitBareGitRepoWithExec(t, map[string]string{
		"extension.toml": binaryManifestTOML("t4-supply", "1.0.0", "bin/ext", "storage"),
		"bin/ext":        string(binData),
	}, "bin/ext")
	httpsURL := e2e4ServeGitHTTPS(t, strings.TrimPrefix(bareURL, "file://"))

	if _, err := mgr.InstallGit(httpsURL, "main"); err != nil {
		t.Fatalf("InstallGit v1: %v", err)
	}
	if err := mgr.AllowAndBuild("t4-supply"); err != nil {
		t.Fatalf("AllowAndBuild v1: %v", err)
	}
	e2e4WaitExtState(t, mgr, "t4-supply", 10*time.Second, func(i ext.ExtensionInfo) bool {
		return i.State == ext.StateRunning
	})

	// Supply-chain update attempts to silently add high-risk "docker" permission.
	workDir := filepath.Join(t.TempDir(), "work")
	commitToBareRepo(t, bareURL, workDir, map[string]string{
		"extension.toml": binaryManifestTOML("t4-supply", "1.0.1", "bin/ext", "storage", "docker"),
	})

	info, err := mgr.InstallGit(httpsURL, "main")
	if err != nil {
		t.Fatalf("InstallGit update: %v", err)
	}
	if info.State != ext.StateWaiting {
		t.Fatalf("expected StateWaiting after escalated update, got %s", info.State)
	}
	if info.State.IsActive() {
		t.Fatal("escalated update must stop running extension immediately")
	}
	if !strings.Contains(strings.Join(info.Asked, ","), "docker") {
		t.Fatalf("expected docker in Asked diff summary, got %v", info.Asked)
	}
	if err := mgr.Enable("t4-supply"); err == nil {
		t.Fatal("Enable must be refused while new docker permission is unapproved")
	}

	// User explicitly denies escalated update -> stays stopped, zero new builds/starts triggered.
	buildsBeforeDeny := len(h.ImageBuilder.SnapshotCalls())
	if err := mgr.Deny("t4-supply"); err != nil {
		t.Fatalf("Deny: %v", err)
	}
	afterDeny, _ := e2e4FindExtInfo(mgr, "t4-supply")
	if afterDeny.State.IsActive() {
		t.Fatalf("denied extension must stay stopped, got %s", afterDeny.State)
	}
	if len(h.ImageBuilder.SnapshotCalls()) != buildsBeforeDeny {
		t.Fatal("Deny must not trigger a container build")
	}

	// Subsequent explicit approval restarts extension to StateRunning.
	if err := mgr.AllowAndBuild("t4-supply"); err != nil {
		t.Fatalf("AllowAndBuild after explicit approval: %v", err)
	}
	e2e4WaitExtState(t, mgr, "t4-supply", 10*time.Second, func(i ext.ExtensionInfo) bool {
		return i.State == ext.StateRunning && len(i.Asked) == 0
	})
}

func TestT4_09_RichDeclarativeDashboard35KindsStress(t *testing.T) {
	dataDir := newShortTempDataDir(t)
	mgr, _ := newTestManager(t, WithDataDir(dataDir), WithDeveloperMode(true))

	src := `package main

import (
	"context"
	"sync"

	"github.com/ironpark/gvx-desktop/pkg/extsdk"
)

func main() {
	var mu sync.Mutex
	tab := "Overview"
	submitted := ""
	app := extsdk.New()
	app.View("main", extsdk.View{
		Chrome: "toolbar",
		Render: func(c *extsdk.ViewContext, ui *extsdk.UI) extsdk.Node {
			mu.Lock()
			curTab := tab
			sub := submitted
			mu.Unlock()
			return ui.Column(
				ui.Toolbar(
					ui.Tabs([]string{"Overview", "Graphs"}, curTab, extsdk.Key("tabs"), extsdk.OnChange("tab")),
					ui.Button("Submit Form", extsdk.Key("submit"), extsdk.Variant("primary"), extsdk.OnTap("submit")),
				),
				ui.Split("auto",
					ui.Scroll(
						ui.Card(
							ui.Breadcrumb([]string{"Dash", curTab}),
							ui.Input("", extsdk.Key("secret_in"), extsdk.Variant("password"), extsdk.Placeholder("Secret Password")),
							ui.Textarea("notes", extsdk.Key("notes")),
							ui.Toggle(true, "Enabled", extsdk.Key("tog")),
							ui.Checkbox("Acknowledge", true, extsdk.Key("chk")),
							ui.Select([]string{"fast", "safe"}, "safe", extsdk.Key("mode")),
							ui.Text("submitted="+sub, extsdk.Key("sub")),
							ui.Divider(),
							ui.Progress(0.75),
							ui.Spinner(),
							ui.Pill("live", extsdk.Tone("success")),
							ui.Chip("v1", extsdk.Icon("tag")),
						),
						ui.Markdown("# Report\nAll **systems** nominal.", extsdk.Key("md")),
						ui.Code("go", "fmt.Println(\"ok\")", extsdk.Key("code")),
					).Grow(1),
					ui.Column(
						ui.Chart("line", []extsdk.ChartSeries{{Name: "CPU", Tone: "accent", Values: []float64{10, 40, 30}}}, extsdk.Labels("t1", "t2", "t3")),
						ui.Chart("bar", []extsdk.ChartSeries{{Name: "Jobs", Tone: "success", Values: []float64{2, 5}}}),
						ui.Chart("pie", []extsdk.ChartSeries{{Name: "Share", Values: []float64{60, 40}}}, extsdk.Labels("Pass", "Fail")),
						ui.Dag([]extsdk.DagRow{
							{ID: "build", Text: "Build", Tone: "success"},
							{ID: "test", Parents: []string{"build"}, Text: "Test", Tone: "accent"},
						}),
						ui.Graph(
							[]extsdk.GraphNode{{ID: "a", Label: "Host"}, {ID: "b", Label: "Ext"}},
							[]extsdk.GraphEdge{{From: "a", To: "b", Label: "IPC"}},
							extsdk.MaxHeight(200),
						),
						ui.Table([]string{"Job", "State"}, ui.Row(ui.Text("lint"), ui.Text("pass"))),
						ui.Tree(ui.TreeItem("Root", ui.TreeItem("Child"))),
						ui.Collapsible("Details", ui.Text("inner")),
						ui.Wrap(ui.Icon("activity"), ui.List(ui.Text("item-1"))),
						ui.Grid(2, ui.Text("g1"), ui.Text("g2")),
						ui.Stack(ui.Text("layered")),
					),
				).Ratio(0.5).Grow(1),
				ui.ContextMenu("ctx", ui.MenuItem("Inspect", extsdk.OnTap("inspect"))),
				ui.Dialog("Review Dashboard", ui.Text("Confirm settings?")),
			).Fill(true)
		},
		On: extsdk.Handlers{
			"tab": func(ctx *extsdk.EventContext, val string) error {
				mu.Lock(); tab = val; mu.Unlock()
				ctx.NotifyChanged()
				return nil
			},
			"submit": func(ctx *extsdk.EventContext) error {
				mu.Lock(); submitted = ctx.FormValue("secret_in") + ":" + ctx.FormValue("mode"); mu.Unlock()
				ctx.NotifyChanged()
				return nil
			},
			"inspect": func(ctx *extsdk.EventContext) error { return nil },
		},
	})
	_ = app.Run(context.Background())
}
`
	manifest := strings.Replace(
		binaryManifestTOML("t4-rich", "0.1.0", "bin/ext"),
		"title = \"Main\"",
		"title = \"Main\"\nchrome = \"toolbar\"",
		1,
	)
	extDir := e2e4CreateBinaryExt(t, t.TempDir(), "t4-rich", manifest, src)
	if _, err := mgr.InstallLocal(extDir); err != nil {
		t.Fatalf("InstallLocal: %v", err)
	}
	if err := mgr.AllowAndBuild("t4-rich"); err != nil {
		t.Fatalf("AllowAndBuild: %v", err)
	}
	e2e4WaitExtState(t, mgr, "t4-rich", 10*time.Second, func(i ext.ExtensionInfo) bool {
		return i.State == ext.StateRunning
	})

	// Verify live view tree (Tree.Values, Tap capability, Diff/Apply, SendViewEvent) and wire uiblock.Read/Tree.Message round-trip.
	subCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	ch, err := mgr.SubscribeView(subCtx, "t4-rich", "main", "en")
	if err != nil {
		cancel()
		t.Fatalf("SubscribeView: %v", err)
	}
	upd1, ok := <-ch
	if !ok || upd1.Tree == nil || upd1.Error != "" {
		cancel()
		t.Fatalf("SubscribeView initial update = (%+v, ok=%v), want non-nil Tree and empty Error", upd1, ok)
	}
	tree1 := &uiblock.Tree{Root: upd1.Tree}
	var findKind func(n *uiblock.Node, kind string) string
	findKind = func(n *uiblock.Node, kind string) string {
		if n == nil {
			return ""
		}
		if n.Kind == kind {
			return n.ID
		}
		for _, c := range n.Children {
			if got := findKind(c, kind); got != "" {
				return got
			}
		}
		return ""
	}
	inputID := findKind(tree1.Root, uiblock.KindInput)
	if inputID == "" {
		cancel()
		t.Fatal("missing input node in rich tree")
	}
	vals, err := tree1.Values(map[string]string{inputID: "pw123"})
	if err != nil || vals[inputID] != "pw123" {
		cancel()
		t.Fatalf("Tree.Values = (%+v, %v), want %s=pw123", vals, err, inputID)
	}
	submitID := findKind(tree1.Root, uiblock.KindButton)
	if submitID == "" {
		cancel()
		t.Fatal("missing button node in rich tree")
	}
	submitNode := uiblock.NodeOf(tree1.Root, submitID)
	if submitNode == nil || !submitNode.Tap {
		cancel()
		t.Fatalf("submit node %q missing Tap capability: %+v", submitID, submitNode)
	}
	if err := mgr.SendViewEvent("t4-rich", "main", uiblock.Event{
		Node:   submitID,
		Kind:   uiblock.EventTap,
		Values: vals,
		Locale: "en",
	}); err != nil {
		cancel()
		t.Fatalf("SendViewEvent(submit): %v", err)
	}
	wireTree, err := uiblock.Read([]byte(`{"kind":"column","children":[{"kind":"input","key":"secret_in","variant":"password","placeholder":"Secret Password"},{"kind":"button","key":"submit","text":"Submit Form","variant":"primary","onTap":["submit"]}]}`))
	if err != nil {
		cancel()
		t.Fatalf("uiblock.Read(wire): %v", err)
	}
	if msg, err := wireTree.Message(uiblock.Event{Node: "/submit", Kind: uiblock.EventTap}); err != nil || msg.Name != "submit" {
		cancel()
		t.Fatalf("Tree.Message(submit) = (%+v, %v), want Name=submit", msg, err)
	}
	if ops := uiblock.Diff(tree1.Root, tree1.Root); len(ops) != 0 {
		cancel()
		t.Fatalf("expected 0 diff ops on identical rich tree, got %d", len(ops))
	}
	if applied, err := uiblock.Apply(tree1.Root, nil); err != nil || !applied.Equal(tree1.Root) {
		cancel()
		t.Fatalf("uiblock.Apply(nil): err=%v", err)
	}
	cancel()
	_ = mgr.Close(3 * time.Second)

	// Render in ggui.ProbeBuilder at both wide (>=1000px) and narrow (<640px) widths.
	p, m := e2e4StartProbeUI(t, dataDir, true, ggui.Sz(1280, 840))
	m.Go("ext:t4-rich/main")
	e2e4WaitSemantics(t, p, 10*time.Second, func(sem *ggui.SemTree) bool {
		return e2e4SemContains(sem, "Submit Form") && e2e4SemContains(sem, "Review Dashboard")
	})

	// Enter password into password-variant input and verify Submit Form roundtrip.
	pwField, ok := p.FindRole(ggui.RoleTextField, "Secret Password")
	if !ok {
		t.Fatalf("Secret Password input not found:\n%s", p.Semantics())
	}
	p.Click(pwField.Center())
	p.Text("hunter2")
	p.Tap("Submit Form")
	e2e4WaitSemantics(t, p, 5*time.Second, func(sem *ggui.SemTree) bool {
		return e2e4SemContains(sem, "submitted=hunter2:safe")
	})

	// Resize below 640px split("auto") breakpoint — must re-layout cleanly without panic.
	p.Resize(ggui.Sz(520, 760))
	p.Frame()
}

func TestT4_10_IdleAutoStopAndTransparentWakeOnDemand(t *testing.T) {
	dataDir := newShortTempDataDir(t)
	mgr, _ := newTestManager(t, WithDataDir(dataDir), WithDeveloperMode(true))
	mgrCtx, mgrCancel := context.WithCancel(context.Background())
	defer mgrCancel()
	if err := mgr.Start(mgrCtx); err != nil {
		t.Fatalf("mgr.Start: %v", err)
	}

	residentSrc := `package main
import ("context"; "github.com/ironpark/gvx-desktop/pkg/extsdk")
func main() {
	app := extsdk.New()
	app.View("main", extsdk.View{Render: func(c *extsdk.ViewContext, ui *extsdk.UI) extsdk.Node {
		return ui.Column(ui.Text("resident-running"))
	}})
	_ = app.Run(context.Background())
}`
	lazySrc := `package main
import ("context"; "github.com/ironpark/gvx-desktop/pkg/extsdk")
func main() {
	app := extsdk.New()
	app.View("main", extsdk.View{Render: func(c *extsdk.ViewContext, ui *extsdk.UI) extsdk.Node {
		return ui.Column(ui.Text("lazy-awake", extsdk.Key("st")))
	}})
	app.Command("wake-cmd", func(ctx *extsdk.CommandContext) error { return nil })
	_ = app.Run(context.Background())
}`
	lazyManifest := strings.Replace(
		binaryManifestTOML("t4-lazy", "0.1.0", "bin/ext"),
		"dev_entrypoint = \"bin/ext\"",
		"dev_entrypoint = \"bin/ext\"\nidle_timeout_sec = 1",
		1,
	) + "\n[[contributes.command]]\nid = \"wake-cmd\"\ntitle = \"Wake\"\n"

	resDir := e2e4CreateBinaryExt(t, t.TempDir(), "t4-resident", binaryManifestTOML("t4-resident", "0.1.0", "bin/ext"), residentSrc)
	lazyDir := e2e4CreateBinaryExt(t, t.TempDir(), "t4-lazy", lazyManifest, lazySrc)

	for _, pair := range []struct{ id, dir string }{
		{"t4-resident", resDir},
		{"t4-lazy", lazyDir},
	} {
		if _, err := mgr.InstallLocal(pair.dir); err != nil {
			t.Fatalf("InstallLocal(%s): %v", pair.id, err)
		}
		if err := mgr.AllowAndBuild(pair.id); err != nil {
			t.Fatalf("AllowAndBuild(%s): %v", pair.id, err)
		}
		e2e4WaitExtState(t, mgr, pair.id, 10*time.Second, func(i ext.ExtensionInfo) bool {
			return i.State == ext.StateRunning
		})
	}

	// Active view subscription holds t4-lazy alive beyond 1.5s.
	subCtx, cancel := context.WithCancel(context.Background())
	ch, err := mgr.SubscribeView(subCtx, "t4-lazy", "main", "en")
	if err != nil {
		cancel()
		t.Fatalf("SubscribeView: %v", err)
	}
	<-ch
	time.Sleep(1500 * time.Millisecond)
	if info, _ := e2e4FindExtInfo(mgr, "t4-lazy"); info.State != ext.StateRunning {
		cancel()
		t.Fatalf("expected t4-lazy running while view subscribed, got %s", info.State)
	}

	// Closing view triggers idle stop of t4-lazy while t4-resident remains running.
	cancel()
	e2e4WaitExtState(t, mgr, "t4-lazy", 10*time.Second, func(i ext.ExtensionInfo) bool {
		return !i.State.IsActive()
	})
	if resInfo, _ := e2e4FindExtInfo(mgr, "t4-resident"); resInfo.State != ext.StateRunning {
		t.Fatalf("expected t4-resident still running, got %s", resInfo.State)
	}

	// Invoking command wakes t4-lazy transparently.
	if err := mgr.RunCommand("t4-lazy", "wake-cmd", "en"); err != nil {
		t.Fatalf("RunCommand(wake-cmd): %v", err)
	}
	e2e4WaitExtState(t, mgr, "t4-lazy", 10*time.Second, func(i ext.ExtensionInfo) bool {
		return i.State == ext.StateRunning
	})
}

func TestT4_11_HostEventPropagationAndSettingsSync(t *testing.T) {
	dataDir := newShortTempDataDir(t)
	mgr, h := newTestManager(t, WithDataDir(dataDir), WithDeveloperMode(true))

	src := `package main

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"

	"github.com/ironpark/gvx-desktop/pkg/extsdk"
)

func main() {
	var mu sync.Mutex
	theme := "dark"
	locale := "en"
	dockerState := "ready"
	endpoint := "init"

	app := extsdk.New()
	app.View("main", extsdk.View{
		Render: func(c *extsdk.ViewContext, ui *extsdk.UI) extsdk.Node {
			mu.Lock()
			out := fmt.Sprintf("theme=%s|locale=%s|docker=%s|ep=%s", theme, locale, dockerState, endpoint)
			mu.Unlock()
			return ui.Column(ui.Text(out, extsdk.Key("sync")))
		},
	})
	app.OnEvent(func(ctx *extsdk.Context, ev extsdk.HostEvent) {
		var m map[string]any
		_ = json.Unmarshal(ev.Data, &m)
		mu.Lock()
		switch ev.Type {
		case "host.theme":
			if v, _ := m["theme"].(string); v != "" { theme = v }
		case "host.locale":
			if v, _ := m["locale"].(string); v != "" { locale = v }
		case "docker.state":
			if v, _ := m["state"].(string); v != "" { dockerState = v }
		}
		mu.Unlock()
		ctx.NotifyChanged("main")
	})
	app.OnSettingsChanged(func(ctx *extsdk.Context, _ map[string]any) error {
		set, err := ctx.Host.Settings(ctx)
		if err != nil { return err }
		mu.Lock()
		endpoint = set.String("endpoint")
		mu.Unlock()
		ctx.NotifyChanged("main")
		return nil
	})
	_ = app.Run(context.Background())
}
`
	manifest := binaryManifestTOML("t4-sync", "0.1.0", "bin/ext") + `
[contributes.settings]
endpoint = { type = "string", label = "Endpoint", default = "init" }
`
	extDir := e2e4CreateBinaryExt(t, t.TempDir(), "t4-sync", manifest, src)
	if _, err := mgr.InstallLocal(extDir); err != nil {
		t.Fatalf("InstallLocal: %v", err)
	}
	if err := mgr.AllowAndBuild("t4-sync"); err != nil {
		t.Fatalf("AllowAndBuild: %v", err)
	}
	e2e4WaitExtState(t, mgr, "t4-sync", 10*time.Second, func(i ext.ExtensionInfo) bool {
		return i.State == ext.StateRunning
	})

	subCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	ch, err := mgr.SubscribeView(subCtx, "t4-sync", "main", "en")
	if err != nil {
		t.Fatalf("SubscribeView: %v", err)
	}
	cur := <-ch

	// Synchronize on GET /v1/events subscription before emitting the 4 real changes.
	warmupDone := false
	warmupTicker := time.NewTicker(30 * time.Millisecond)
	defer warmupTicker.Stop()
	mgr.NotifyHostEvent("host.theme", map[string]string{"theme": "warmup"})
	for !warmupDone {
		if n := uiblock.NodeOf(cur.Tree, "/sync"); n != nil && strings.HasPrefix(n.Text, "theme=warmup|") {
			warmupDone = true
			break
		}
		select {
		case upd := <-ch:
			if upd.Tree != nil {
				cur = upd
			} else if len(upd.Ops) > 0 && cur.Tree != nil {
				next, err := uiblock.Apply(cur.Tree, upd.Ops)
				if err != nil {
					t.Fatalf("uiblock.Apply(warmup): %v", err)
				}
				cur.Tree = next
			}
		case <-warmupTicker.C:
			mgr.NotifyHostEvent("host.theme", map[string]string{"theme": "warmup"})
		case <-subCtx.Done():
			t.Fatalf("timed out waiting for GET /v1/events subscription warmup; last=%+v", cur.Tree)
		}
	}

	// Drive all 4 host state / settings changes now that SSE events stream is confirmed active.
	h.HostInfo.Theme = "light"
	mgr.NotifyHostEvent("host.theme", map[string]string{"theme": "light"})
	h.HostInfo.Locale = "ko"
	mgr.NotifyHostEvent("host.locale", map[string]string{"locale": "ko"})
	h.DockerBackend.SetUnavailable(true)
	mgr.NotifyHostEvent("docker.state", map[string]string{"state": "stopped"})
	if err := mgr.SetSetting("t4-sync", "endpoint", "https://synced.invalid"); err != nil {
		t.Fatalf("SetSetting: %v", err)
	}

	want := "theme=light|locale=ko|docker=stopped|ep=https://synced.invalid"
	for {
		if n := uiblock.NodeOf(cur.Tree, "/sync"); n != nil && n.Text == want {
			return
		}
		select {
		case upd := <-ch:
			if upd.Tree != nil {
				cur = upd
			} else if len(upd.Ops) > 0 && cur.Tree != nil {
				next, err := uiblock.Apply(cur.Tree, upd.Ops)
				if err != nil {
					t.Fatalf("uiblock.Apply: %v", err)
				}
				cur.Tree = next
			}
		case <-subCtx.Done():
			t.Fatalf("timed out waiting for %q; last=%+v", want, cur.Tree)
		}
	}
}

func TestT4_12_GracefulVsHungShutdownAndDataRetention(t *testing.T) {
	dataDir := newShortTempDataDir(t)
	sup := e2e4NewRungvSupervisor(t, filepath.Join(dataDir, "sbx"), "")
	mgr, h := newTestManager(t, WithDataDir(dataDir), WithDeveloperMode(true), WithSupervisor(sup))

	coopSrc := `package main
import ("context"; "os"; "path/filepath"; "github.com/ironpark/gvx-desktop/pkg/extsdk")
func main() {
	app := extsdk.New()
	app.Command("seed", func(ctx *extsdk.CommandContext) error {
		if err := ctx.Host.StorageSet(ctx, "k", "keep-me"); err != nil { return err }
		if err := ctx.Host.SecretSet(ctx, "s", "coop-secret"); err != nil { return err }
		start := false
		_, err := ctx.Host.SandboxCreate(ctx, extsdk.CreateSandboxRequest{
			Name:      "t4-eph-sbx",
			Image:     "alpine:3.20",
			Ephemeral: true,
			Start:     &start,
		})
		return err
	})
	app.OnEvent(func(ctx *extsdk.Context, ev extsdk.HostEvent) {
		switch ev.Type {
		case "host.theme":
			_ = os.WriteFile(filepath.Join(ctx.DataDir, "events-ready"), []byte("1"), 0o600)
		case "app.stopping":
			_ = os.WriteFile(filepath.Join(ctx.DataDir, "clean-stop"), []byte("ok"), 0o600)
		}
	})
	_ = app.Run(context.Background())
}`
	// Hung extension ignores SIGTERM, seeds a secret, and blocks in /v1/shutdown to force SIGKILL within budget.
	hungSrc := `package main
import (
	"context"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/ironpark/gvx-desktop/pkg/extsdk"
)
func main() {
	signal.Ignore(syscall.SIGTERM)
	if h, err := extsdk.NewHost(os.Getenv("GVX_HOST_API"), os.Getenv("GVX_HOST_TOKEN")); err == nil {
		_ = h.SecretSet(context.Background(), "hung_sec", "hung-secret-val")
	}
	listen := os.Getenv("GVX_EXT_LISTEN")
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/health", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("{\"status\":\"ok\",\"version\":\"0.1.0\"}"))
	})
	mux.HandleFunc("POST /v1/shutdown", func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(30 * time.Second)
	})
	var ln net.Listener
	var err error
	if strings.HasPrefix(listen, "unix://") {
		sock := strings.TrimPrefix(listen, "unix://")
		_ = os.Remove(sock)
		ln, err = net.Listen("unix", sock)
	} else {
		ln, err = net.Listen("tcp", listen)
	}
	if err != nil { os.Exit(2) }
	_ = (&http.Server{Handler: mux}).Serve(ln)
}`

	coopManifest := binaryManifestTOML("t4-coop", "0.1.0", "bin/ext", "storage", "secrets", "sandboxes") +
		"\n[[contributes.command]]\nid = \"seed\"\ntitle = \"Seed\"\n"
	coopDir := e2e4CreateBinaryExt(t, t.TempDir(), "t4-coop", coopManifest, coopSrc)
	hungDir := e2e4CreateBinaryExt(t, t.TempDir(), "t4-hung", binaryManifestTOML("t4-hung", "0.1.0", "bin/ext", "secrets"), hungSrc)

	if _, err := mgr.InstallLocal(coopDir); err != nil {
		t.Fatalf("InstallLocal(coop): %v", err)
	}
	if err := mgr.AllowAndBuild("t4-coop"); err != nil {
		t.Fatalf("AllowAndBuild(coop): %v", err)
	}
	e2e4WaitExtState(t, mgr, "t4-coop", 10*time.Second, func(i ext.ExtensionInfo) bool {
		return i.State == ext.StateRunning
	})
	if err := mgr.RunCommand("t4-coop", "seed", "en"); err != nil {
		t.Fatalf("RunCommand(seed): %v", err)
	}

	st := newTestStore(t, dataDir)
	beforeSpecs, err := st.Sandboxes()
	if err != nil {
		t.Fatalf("st.Sandboxes before Close: %v", err)
	}
	if len(beforeSpecs) != 1 || beforeSpecs[0].Name != "t4-eph-sbx" {
		t.Fatalf("expected 1 ephemeral sandbox before Close, got %+v", beforeSpecs)
	}

	if _, err := mgr.InstallLocal(hungDir); err != nil {
		t.Fatalf("InstallLocal(hung): %v", err)
	}
	if err := mgr.AllowAndBuild("t4-hung"); err != nil {
		t.Fatalf("AllowAndBuild(hung): %v", err)
	}
	e2e4WaitExtState(t, mgr, "t4-hung", 10*time.Second, func(i ext.ExtensionInfo) bool {
		return i.State == ext.StateRunning
	})

	// Confirm cooperative extension's GET /v1/events subscription is active before Close(8s).
	eventsReadyMarker := filepath.Join(ext.FilesDir(dataDir, "t4-coop"), "events-ready")
	readyDeadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(readyDeadline) {
		mgr.NotifyHostEvent("host.theme", map[string]string{"theme": "warmup"})
		if _, err := os.Stat(eventsReadyMarker); err == nil {
			break
		}
		time.Sleep(30 * time.Millisecond)
	}

	start := time.Now()
	if err := mgr.Close(8 * time.Second); err != nil {
		t.Fatalf("mgr.Close(8s): %v", err)
	}
	elapsed := time.Since(start)
	if elapsed > 10*time.Second {
		t.Fatalf("Close(8s) exceeded bounded budget: took %v", elapsed)
	}
	if _, err := os.Stat(filepath.Join(ext.FilesDir(dataDir, "t4-coop"), "clean-stop")); err != nil {
		t.Fatalf("cooperative extension did not record app.stopping: %v", err)
	}
	afterSpecs, err := st.Sandboxes()
	if err != nil {
		t.Fatalf("st.Sandboxes after Close: %v", err)
	}
	if len(afterSpecs) != 0 {
		t.Fatalf("expected ephemeral sandbox t4-eph-sbx cleaned up on Close(8s), remaining=%+v", afterSpecs)
	}

	// Re-open manager to test Uninstall data retention vs purge.
	mgr2, h2 := newTestManager(t, WithDataDir(dataDir), WithKeychain(h.Keychain, ""), WithDeveloperMode(true))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	_ = mgr2.Start(ctx)

	if err := mgr2.Uninstall("t4-coop", false); err != nil {
		t.Fatalf("Uninstall(coop, purge=false): %v", err)
	}
	if _, err := os.Stat(ext.StoragePath(dataDir, "t4-coop")); err != nil {
		t.Fatalf("expected extension-data/t4-coop/storage.json retained when purge=false: %v", err)
	}

	if err := mgr2.Uninstall("t4-hung", true); err != nil {
		t.Fatalf("Uninstall(hung, purge=true): %v", err)
	}
	if _, err := os.Stat(ext.ExtensionDataDir(dataDir, "t4-hung")); !os.IsNotExist(err) {
		t.Fatalf("expected extension-data/t4-hung deleted when purge=true, err=%v", err)
	}
	// Keychain entries for purged t4-hung should be gone while t4-coop's remain.
	for _, entry := range h2.Keychain.Entries() {
		if strings.Contains(entry[0], "t4-hung") {
			t.Fatalf("purged extension secret leaked in keychain: %+v", entry)
		}
	}
}
