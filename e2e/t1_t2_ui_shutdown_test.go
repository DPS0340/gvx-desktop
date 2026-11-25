//go:build e2e

package e2e

import (
	"context"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"net"
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

	"github.com/ironpark/ggui"
	"github.com/ironpark/ggui/inspect"
	gguiui "github.com/ironpark/ggui/ui"
	uitheme "github.com/ironpark/ggui/ui/theme"

	"github.com/ironpark/gvx-desktop/internal/ext"
	"github.com/ironpark/gvx-desktop/internal/keychain"
	"github.com/ironpark/gvx-desktop/internal/localproxy"
	"github.com/ironpark/gvx-desktop/internal/recipes"
	"github.com/ironpark/gvx-desktop/internal/rungv"
	"github.com/ironpark/gvx-desktop/internal/sandbox"
	"github.com/ironpark/gvx-desktop/internal/ui"
	"github.com/ironpark/gvx-desktop/internal/uiblock"
	"github.com/ironpark/gvx-desktop/pkg/extsdk"
)

// =============================================================================
// File-Local Helpers (All prefixed with e2e3UI to avoid symbol collisions)
// =============================================================================

func e2e3UINewProbe(t *testing.T, dataDir string, mgr *ext.Manager, size ggui.Size, setupFn func(*ui.Model)) (*ui.Model, *ggui.Probe) {
	t.Helper()
	t.Cleanup(func() { ui.SetLanguage(ui.LangEnglish) })
	ui.SetLanguage(ui.LangEnglish)

	st := newTestStore(t, dataDir)
	if cur, err := st.Settings(); err == nil && cur.Language == "" {
		cur.Language = ui.LangEnglish
		_ = st.SaveSettings(cur)
	}
	var m *ui.Model
	p := ggui.ProbeBuilder(func() ggui.Widget {
		return ggui.Provide(ggui.ReducedMotionKey, true, ui.Build(m))
	}, size)
	p.Setup(func() {
		preset := uitheme.Preset{Base: uitheme.BaseZinc, Accent: uitheme.AccentBlue}
		uitheme.Bind(ggui.Const(false), preset.Dark(), preset.Light())
		m = ui.NewModel(st, dataDir)
		m.Offline = true
		m.Attach(p)
		if mgr != nil {
			m.SetExtensionManager(mgr)
		}
		if setupFn != nil {
			setupFn(m)
		}
	})
	p.Frame()
	if m == nil {
		t.Fatal("Model not initialized after initial Frame")
	}
	return m, p
}

func e2e3UINewBlockProbe(t *testing.T, wFn func() ggui.Widget, size ggui.Size) *ggui.Probe {
	t.Helper()
	t.Cleanup(func() { ui.SetLanguage(ui.LangEnglish) })
	ui.SetLanguage(ui.LangEnglish)

	p := ggui.ProbeBuilder(func() ggui.Widget {
		return ggui.Provide(ggui.ReducedMotionKey, true, wFn())
	}, size)
	p.Setup(func() {
		preset := uitheme.Preset{Base: uitheme.BaseZinc, Accent: uitheme.AccentBlue}
		uitheme.Bind(ggui.Const(false), preset.Dark(), preset.Light())
	})
	return p
}

func e2e3UISettle(t *testing.T, p *ggui.Probe, cond func() bool, what string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		p.Frame()
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for UI condition (%s); semantics:\n%s", what, p.Semantics())
}

func e2e3UISemHasText(sem *ggui.SemTree, substr string) bool {
	if sem == nil {
		return false
	}
	for _, n := range sem.All() {
		if strings.Contains(n.Name, substr) || strings.Contains(n.Value, substr) || strings.Contains(n.Description, substr) {
			return true
		}
	}
	return false
}

func e2e3UIFindSemNode(sem *ggui.SemTree, role ggui.Role, nameSubstr string) (ggui.SemNode, bool) {
	if sem == nil {
		return ggui.SemNode{}, false
	}
	for _, n := range sem.All() {
		if (role == "" || n.Role == role) && strings.Contains(n.Name, nameSubstr) {
			return n, true
		}
	}
	return ggui.SemNode{}, false
}

func e2e3UIFindSemExact(sem *ggui.SemTree, role ggui.Role, name string) (ggui.SemNode, bool) {
	if sem == nil {
		return ggui.SemNode{}, false
	}
	for _, n := range sem.All() {
		if (role == "" || n.Role == role) && n.Name == name {
			return n, true
		}
	}
	return ggui.SemNode{}, false
}

func e2e3UITapSem(t *testing.T, p *ggui.Probe, role ggui.Role, nameSubstr string) {
	t.Helper()
	sem := p.Semantics()
	n, ok := e2e3UIFindSemNode(sem, role, nameSubstr)
	if !ok {
		t.Fatalf("missing semantic node role=%q name~=%q\n%s", role, nameSubstr, sem)
	}
	if !n.Offscreen && n.Rect.Size.W > 0 && n.Rect.Size.H > 0 {
		center := ggui.Pt(n.Rect.Origin.X+n.Rect.Size.W/2, n.Rect.Origin.Y+n.Rect.Size.H/2)
		p.Click(center)
		return
	}
	p.PerformAction(n.ID, ggui.Action{Kind: ggui.ActionPress})
}

func e2e3UISetTextSem(t *testing.T, p *ggui.Probe, nameSubstr, value string) {
	t.Helper()
	sem := p.Semantics()
	n, ok := e2e3UIFindSemNode(sem, ggui.RoleTextField, nameSubstr)
	if !ok {
		t.Fatalf("missing text field %q\n%s", nameSubstr, sem)
	}
	p.PerformAction(n.ID, ggui.Action{Kind: ggui.ActionFocus})
	if !n.Offscreen && n.Rect.Size.W > 0 && n.Rect.Size.H > 0 {
		p.Click(ggui.Pt(n.Rect.Origin.X+n.Rect.Size.W/2, n.Rect.Origin.Y+n.Rect.Size.H/2))
	}
	p.PerformAction(n.ID, ggui.Action{Kind: ggui.ActionSetValue, Text: value})
	p.Frame()
}

func e2e3UIInstallLocalBinary(t *testing.T, mgr *ext.Manager, id, goSource, extraTOML string, perms ...string) string {
	t.Helper()
	binPath := buildFixtureSDKBinary(t, goSource)
	binData, err := os.ReadFile(binPath)
	if err != nil {
		t.Fatalf("read fixture binary: %v", err)
	}
	if strings.TrimSpace(extraTOML) == "" {
		extraTOML = e2e3UIMinimalViewTOML
	}
	manifest := e2e3UIManifestBinary(id, "Binary Extension "+id, extraTOML, perms...)
	dir := filepath.Join(t.TempDir(), id)
	writeExtFixture(t, dir, manifest, nil)
	if err := os.MkdirAll(filepath.Join(dir, "bin"), 0o700); err != nil {
		t.Fatalf("mkdir bin: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "bin", "ext"), binData, 0o755); err != nil {
		t.Fatalf("write dev_entrypoint: %v", err)
	}
	if _, err := mgr.InstallLocal(dir); err != nil {
		t.Fatalf("InstallLocal(%s): %v", id, err)
	}
	if err := mgr.AllowAndBuild(id); err != nil {
		t.Fatalf("AllowAndBuild(%s): %v", id, err)
	}
	return dir
}

func e2e3UIFindExtInfo(mgr *ext.Manager, id string) (ext.ExtensionInfo, bool) {
	for _, info := range mgr.List() {
		if info.ID == id {
			return info, true
		}
	}
	return ext.ExtensionInfo{}, false
}

func e2e3UIWaitExtState(t *testing.T, mgr *ext.Manager, id string, want ext.ExtensionState, timeout time.Duration) ext.ExtensionInfo {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if info, ok := e2e3UIFindExtInfo(mgr, id); ok && info.State == want {
			return info
		}
		time.Sleep(20 * time.Millisecond)
	}
	info, _ := e2e3UIFindExtInfo(mgr, id)
	t.Fatalf("timed out waiting for %s state=%q (last=%+v)", id, want, info)
	return ext.ExtensionInfo{}
}

func e2e3UIManifestBinary(id, name string, extraTOML string, perms ...string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "schema_version = 1\n")
	fmt.Fprintf(&b, "id = %q\n", id)
	fmt.Fprintf(&b, "version = \"0.1.0\"\n")
	fmt.Fprintf(&b, "publisher = \"acme\"\n")
	fmt.Fprintf(&b, "name = %q\n", name)
	fmt.Fprintf(&b, "description = \"E2E UI fixture\"\n")
	fmt.Fprintf(&b, "icon = \"box\"\n")
	if len(perms) > 0 {
		quoted := make([]string, len(perms))
		for i, p := range perms {
			quoted[i] = fmt.Sprintf("%q", p)
		}
		fmt.Fprintf(&b, "permissions = [%s]\n", strings.Join(quoted, ", "))
	}
	fmt.Fprintf(&b, "\n[runtime]\n")
	fmt.Fprintf(&b, "type = \"binary\"\n")
	fmt.Fprintf(&b, "dev_entrypoint = \"bin/ext\"\n")
	if extraTOML != "" {
		b.WriteString("\n")
		b.WriteString(extraTOML)
		b.WriteString("\n")
	}
	return b.String()
}

func e2e3UIInspectWidgetRectByLabel(t *testing.T, p *ggui.Probe, labelSubstr string) (ggui.Rect, bool) {
	t.Helper()
	var found ggui.Rect
	var ok bool
	p.OnInspect(func(f *inspect.Frame) {
		f.DescribeAll()
		for _, n := range f.Nodes {
			if strings.Contains(n.Label, labelSubstr) {
				found = n.Rect
				ok = true
				return
			}
		}
	})
	p.Frame()
	if ok {
		return found, true
	}
	sem := p.Semantics()
	if sn, sOK := e2e3UIFindSemNode(sem, "", labelSubstr); sOK {
		return sn.Full, true
	}
	return ggui.Rect{}, false
}

const e2e3UIMinimalViewTOML = `
[[contributes.view]]
id = "main"
title = "Main"
icon = "box"
`

type e2e3UIBlockingImageBuilder struct {
	started chan struct{}
	release chan struct{}
	once    sync.Once
}

func (b *e2e3UIBlockingImageBuilder) Build(ctx context.Context, req ext.BuildRequest, onLine func(string)) (string, error) {
	if onLine != nil {
		onLine("Step 1/2 : FROM alpine:3.20")
	}
	b.once.Do(func() {
		close(b.started)
	})
	select {
	case <-b.release:
		return "sha256:e2e3building", nil
	case <-ctx.Done():
		return "", ctx.Err()
	}
}

func e2e3UIFreePort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen free tcp port: %v", err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	_ = ln.Close()
	return port
}

func e2e3UIServeGitHTTPS(t *testing.T, bareDir string) string {
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

func e2e3UINonReservedDir(t *testing.T) string {
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
	custom, err := os.MkdirTemp(home, "gvx-e2e3-ui-*")
	if err != nil {
		t.Fatalf("os.MkdirTemp in home: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(custom) })
	if resolved, err := filepath.EvalSymlinks(custom); err == nil {
		custom = resolved
	}
	return custom
}

func e2e3UIBuildLinuxSDKBinary(t *testing.T, sourceGoCode string) string {
	t.Helper()
	root := repoRoot(t)
	buildDir := t.TempDir()
	goMod := fmt.Sprintf("module example.com/e2e3linuxfixture\n\ngo 1.27.0\n\nrequire github.com/ironpark/gvx-desktop v0.0.0\n\nreplace github.com/ironpark/gvx-desktop => %s\n", root)
	if err := os.WriteFile(filepath.Join(buildDir, "go.mod"), []byte(goMod), 0o644); err != nil {
		t.Fatalf("write go.mod: %v", err)
	}
	if err := os.WriteFile(filepath.Join(buildDir, "main.go"), []byte(sourceGoCode), 0o644); err != nil {
		t.Fatalf("write main.go: %v", err)
	}
	binPath := filepath.Join(buildDir, "ext-linux")
	env := []string{"GOOS=linux", "GOARCH=" + runtime.GOARCH}
	if out, err := runGo(t, buildDir, env, "build", "-o", binPath, "."); err != nil {
		t.Fatalf("e2e3UIBuildLinuxSDKBinary failed: %v\n%s", err, out)
	}
	return binPath
}

// =============================================================================
// Feature 21: Native ggui uiblock Renderer & blockForm (5 Tier 1 + 5 Tier 2)
// =============================================================================

func TestF21_T1_RendersAll35KindsWithoutPanic(t *testing.T) {
	const src = `package main

import (
	"context"
	"github.com/ironpark/gvx-desktop/pkg/extsdk"
)

func main() {
	app := extsdk.New()
	app.View("main", extsdk.View{
		Title: "All35",
		Render: func(ctx *extsdk.ViewContext, u *extsdk.UI) extsdk.Node {
			return u.Column(
				u.Toolbar(
					u.Icon("box"),
					u.Pill("Live", extsdk.Tone("success")),
					u.Chip("v1", extsdk.Tone("accent")),
					u.Button("RunAction", extsdk.Key("run"), extsdk.OnTap("noop")),
					u.Menu("MoreMenu",
						u.MenuItem("RefreshItem", extsdk.OnTap("noop")),
						u.Divider(),
					),
				),
				u.Stack(
					u.Card(u.Text("CardContent")),
				),
				u.Scroll(
					u.Wrap(u.Breadcrumb([]string{"Home", "Runs"}, extsdk.Key("crumb"), extsdk.OnChange("noop"))),
					u.Grid(2,
						u.Input("init", extsdk.Key("inp"), extsdk.Placeholder("Search"), extsdk.OnChange("noop")),
						u.Textarea("notes", extsdk.Key("ta"), extsdk.Placeholder("Notes"), extsdk.OnChange("noop")),
						u.Toggle(true, "AutoSync", extsdk.Key("tog"), extsdk.OnChange("noop")),
						u.Select([]string{"fast", "safe"}, "fast", extsdk.Key("sel"), extsdk.OnChange("noop")),
						u.Tabs([]string{"Overview", "History"}, "Overview", extsdk.Key("tabs"), extsdk.OnChange("noop")),
						u.Checkbox("ConfirmBox", true, extsdk.Key("chk"), extsdk.OnChange("noop")),
					),
					u.Split("horizontal",
						u.Collapsible("DetailsGroup",
							u.List(
								u.Markdown("**MarkdownHeading**"),
								u.Code("go", "package main\nfunc main() {}"),
								u.Progress(0.65),
								u.Divider(),
							),
						),
						u.Tree(
							u.TreeItem("TreeNodeRoot", extsdk.Key("ti"), extsdk.Check("off"), extsdk.OnCheck("noop")),
						),
					),
					u.Table([]string{"Name", "Status"},
						u.Row(u.Text("job-1"), u.Text("ok")),
					),
					u.Chart("line", []extsdk.ChartSeries{{Name: "CPU", Values: []float64{10, 20, 15}}}, extsdk.Labels("t1", "t2", "t3")),
					u.Graph(
						[]extsdk.GraphNode{{ID: "a", Label: "Build"}, {ID: "b", Label: "Test"}},
						[]extsdk.GraphEdge{{From: "a", To: "b", Label: "next"}},
						extsdk.MaxHeight(160),
					),
					u.Dag([]extsdk.DagRow{
						{ID: "d1", Text: "Checkout", Icon: "git-branch"},
						{ID: "d2", Parents: []string{"d1"}, Text: "Compile", Icon: "hammer"},
					}, extsdk.MaxHeight(160)),
					u.Dialog("SampleDialog", extsdk.Key("dlg"), extsdk.OnChange("noop"), u.Text("DialogBodyText")),
				),
			).Fill(true)
		},
		On: extsdk.Handlers{
			"noop": func(ctx *extsdk.EventContext) error { return nil },
		},
	})
	_ = app.Run(context.Background())
}
`
	// Verify programmatically via extsdk + uiblock.Read that all 35 kinds are present.
	var u extsdk.UI
	verifyTreeNode := u.Column(
		u.Toolbar(u.Icon("box"), u.Pill("Live"), u.Chip("v1"), u.Button("RunAction", extsdk.OnTap("noop")), u.Menu("MoreMenu", u.MenuItem("RefreshItem", extsdk.OnTap("noop")), u.Divider())),
		u.Stack(u.Card(u.Text("CardContent"))),
		u.Scroll(
			u.Wrap(u.Breadcrumb([]string{"Home"}, extsdk.OnChange("noop"))),
			u.Grid(2, u.Input("a"), u.Textarea("b"), u.Toggle(true, "T"), u.Select([]string{"x"}, "x"), u.Tabs([]string{"y"}, "y"), u.Checkbox("C", true)),
			u.Split("horizontal", u.Collapsible("D", u.List(u.Markdown("m"), u.Code("go", "x"), u.Progress(0.5), u.Divider())), u.Tree(u.TreeItem("ti"))),
			u.Table([]string{"H"}, u.Row(u.Text("v"))),
			u.Chart("line", []extsdk.ChartSeries{{Name: "s", Values: []float64{1}}}),
			u.Graph([]extsdk.GraphNode{{ID: "a", Label: "A"}}, nil),
			u.Dag([]extsdk.DagRow{{ID: "r1", Text: "R"}}),
			u.Dialog("Dlg", u.Text("body")),
		),
	).Fill(true)
	rawBytes, err := json.Marshal(verifyTreeNode)
	if err != nil {
		t.Fatalf("marshal 35-kind tree: %v", err)
	}
	parsedTree, err := uiblock.Read(rawBytes)
	if err != nil {
		t.Fatalf("uiblock.Read(35-kind tree): %v", err)
	}
	seenKinds := map[string]bool{}
	var walk func(*uiblock.Node)
	walk = func(n *uiblock.Node) {
		if n == nil {
			return
		}
		seenKinds[n.Kind] = true
		for _, c := range n.Children {
			walk(c)
		}
	}
	walk(parsedTree.Root)
	for _, k := range uiblock.Kinds {
		if !seenKinds[k] {
			t.Fatalf("35-kind tree missing kind %q", k)
		}
	}

	mgr, h := newTestManager(t, WithDeveloperMode(true))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := mgr.Start(ctx); err != nil {
		t.Fatalf("mgr.Start: %v", err)
	}
	e2e3UIInstallLocalBinary(t, mgr, "f21-all35", src, e2e3UIMinimalViewTOML)

	m, p := e2e3UINewProbe(t, h.DataDir, mgr, ggui.Sz(1280, 840), nil)
	defer p.Close()
	p.Frame()

	m.Go(ui.ExtensionPage("f21-all35", "main"))
	e2e3UISettle(t, p, func() bool {
		return e2e3UISemHasText(p.Semantics(), "CardContent")
	}, "all-35-kinds ExtensionPage render")

	for range 5 {
		p.Advance(50 * time.Millisecond)
		p.Frame()
	}
	sem := p.Semantics()
	for _, want := range []string{"RunAction", "CardContent", "AutoSync", "ConfirmBox", "SampleDialog"} {
		if !e2e3UISemHasText(sem, want) {
			t.Fatalf("expected rendered semantics to contain %q\n%s", want, sem)
		}
	}
}

func TestF21_T1_PasswordInputMaskedField(t *testing.T) {
	const src = `package main

import (
	"context"
	"os"
	"path/filepath"
	"github.com/ironpark/gvx-desktop/pkg/extsdk"
)

func main() {
	app := extsdk.New()
	app.View("main", extsdk.View{
		Render: func(ctx *extsdk.ViewContext, u *extsdk.UI) extsdk.Node {
			return u.Column(
				u.Input("initial-pat-secret",
					extsdk.Key("pat"),
					extsdk.Variant("password"),
					extsdk.Placeholder("API Token"),
					extsdk.OnChange("set_pat"),
				),
				u.Button("SaveToken", extsdk.Key("save"), extsdk.OnTap("save")),
			)
		},
		On: extsdk.Handlers{
			"set_pat": func(ctx *extsdk.EventContext, val string) error { return nil },
			"save": func(ctx *extsdk.EventContext) error {
				_ = os.WriteFile(filepath.Join(os.Getenv("GVX_EXT_DATA_DIR"), "saved_pat.txt"), []byte(ctx.FormValue("/pat")), 0o600)
				return nil
			},
		},
	})
	_ = app.Run(context.Background())
}
`
	mgr, h := newTestManager(t, WithDeveloperMode(true))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := mgr.Start(ctx); err != nil {
		t.Fatalf("mgr.Start: %v", err)
	}
	e2e3UIInstallLocalBinary(t, mgr, "f21-pw", src, e2e3UIMinimalViewTOML)

	m, p := e2e3UINewProbe(t, h.DataDir, mgr, ggui.Sz(1200, 800), nil)
	defer p.Close()
	p.Frame()

	var hasPasswordTextField bool
	p.OnInspect(func(f *inspect.Frame) {
		for _, n := range f.Nodes {
			if _, ok := n.Instance.(*gguiui.TextFieldWidget); ok {
				hasPasswordTextField = true
			}
		}
	})

	m.Go(ui.ExtensionPage("f21-pw", "main"))
	e2e3UISettle(t, p, func() bool {
		return e2e3UISemHasText(p.Semantics(), "SaveToken")
	}, "password view ready")

	sem := p.Semantics()
	pwNode, ok := e2e3UIFindSemNode(sem, ggui.RoleTextField, "API Token")
	if !ok {
		t.Fatalf("missing password TextField in semantics:\n%s", sem)
	}
	if strings.Contains(pwNode.Value, "initial-pat-secret") {
		t.Fatalf("password field exposed initial plaintext value in semantics: %q", pwNode.Value)
	}
	if !hasPasswordTextField {
		t.Fatal("expected *ui.TextFieldWidget instance in inspected widget tree")
	}

	e2e3UISetTextSem(t, p, "API Token", "typed-super-secret-99")
	semAfter := p.Semantics()
	pwNodeAfter, ok := e2e3UIFindSemNode(semAfter, ggui.RoleTextField, "API Token")
	if !ok {
		t.Fatalf("missing password TextField after typing")
	}
	if strings.Contains(pwNodeAfter.Value, "typed-super-secret-99") {
		t.Fatalf("password field exposed typed plaintext value in semantics: %q", pwNodeAfter.Value)
	}

	e2e3UITapSem(t, p, ggui.RoleButton, "SaveToken")
	savedPath := filepath.Join(h.DataDir, "extension-data", "f21-pw", "files", "saved_pat.txt")
	e2e3UISettle(t, p, func() bool {
		b, err := os.ReadFile(savedPath)
		return err == nil && string(b) == "typed-super-secret-99"
	}, "submitted password value persisted by extension")
}

func TestF21_T1_BlockFormPreservesUserEditsAcrossSSE(t *testing.T) {
	const src = `package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync/atomic"
	"github.com/ironpark/gvx-desktop/pkg/extsdk"
)

func main() {
	app := extsdk.New()
	var tick atomic.Int64
	app.View("main", extsdk.View{
		Render: func(ctx *extsdk.ViewContext, u *extsdk.UI) extsdk.Node {
			return u.Column(
				u.Text(fmt.Sprintf("tick-%d", tick.Load()), extsdk.Key("status")),
				u.Input("init-text", extsdk.Key("in"), extsdk.Placeholder("TextInput"), extsdk.OnChange("noop")),
				u.Toggle(false, "ToggleField", extsdk.Key("tog"), extsdk.OnChange("noop")),
				u.Select([]string{"alpha", "beta"}, "alpha", extsdk.Key("sel"), extsdk.OnChange("noop")),
				u.Checkbox("CheckField", false, extsdk.Key("chk"), extsdk.OnChange("noop")),
				u.Tabs([]string{"TabA", "TabB"}, "TabA", extsdk.Key("tabs"), extsdk.OnChange("noop")),
				u.Button("SubmitForm", extsdk.Key("submit"), extsdk.OnTap("submit")),
			)
		},
		On: extsdk.Handlers{
			"noop": func(ctx *extsdk.EventContext) error { return nil },
			"submit": func(ctx *extsdk.EventContext) error {
				b, _ := json.Marshal(ctx.FormValues())
				return os.WriteFile(filepath.Join(os.Getenv("GVX_EXT_DATA_DIR"), "values.json"), b, 0o600)
			},
		},
	})
	app.Command("bump", func(ctx *extsdk.CommandContext) error {
		tick.Add(1)
		app.NotifyChanged("main")
		return nil
	})
	_ = app.Run(context.Background())
}
`
	extraTOML := e2e3UIMinimalViewTOML + `
[[contributes.command]]
id = "bump"
title = "Bump Tick"
`
	mgr, h := newTestManager(t, WithDeveloperMode(true))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := mgr.Start(ctx); err != nil {
		t.Fatalf("mgr.Start: %v", err)
	}
	e2e3UIInstallLocalBinary(t, mgr, "f21-form-keep", src, extraTOML)

	m, p := e2e3UINewProbe(t, h.DataDir, mgr, ggui.Sz(1200, 840), nil)
	defer p.Close()
	p.Frame()

	m.Go(ui.ExtensionPage("f21-form-keep", "main"))
	e2e3UISettle(t, p, func() bool {
		return e2e3UISemHasText(p.Semantics(), "tick-0")
	}, "initial form render")

	// User edits input, toggle, select, checkbox, and tabs.
	e2e3UISetTextSem(t, p, "TextInput", "user-draft-value")
	e2e3UITapSem(t, p, ggui.RoleSwitch, "ToggleField")
	e2e3UITapSem(t, p, ggui.RoleCheckbox, "CheckField")
	e2e3UITapSem(t, p, ggui.RoleTab, "TabB")
	if selNode, ok := e2e3UIFindSemNode(p.Semantics(), ggui.RoleSelect, ""); ok {
		p.PerformAction(selNode.ID, ggui.Action{Kind: ggui.ActionSetValue, Text: "beta"})
	} else if selNode, ok := e2e3UIFindSemNode(p.Semantics(), ggui.RoleCombobox, ""); ok {
		p.PerformAction(selNode.ID, ggui.Action{Kind: ggui.ActionSetValue, Text: "beta"})
	}
	p.Frame()

	// Trigger SSE update changing sibling text ("tick-1") while form field "given" defaults stay identical.
	if err := mgr.RunCommand("f21-form-keep", "bump", "en"); err != nil {
		t.Fatalf("RunCommand(bump): %v", err)
	}
	e2e3UISettle(t, p, func() bool {
		return e2e3UISemHasText(p.Semantics(), "tick-1")
	}, "SSE update tick-1 applied")

	// User input value must still be preserved across the SSE update.
	sem := p.Semantics()
	inNode, ok := e2e3UIFindSemNode(sem, ggui.RoleTextField, "TextInput")
	if !ok || inNode.Value != "user-draft-value" {
		t.Fatalf("expected input value preserved as %q after SSE update, got %+v", "user-draft-value", inNode)
	}

	// Tap SubmitForm and inspect Event.Values received by extension.
	e2e3UITapSem(t, p, ggui.RoleButton, "SubmitForm")
	valPath := filepath.Join(h.DataDir, "extension-data", "f21-form-keep", "files", "values.json")
	e2e3UISettle(t, p, func() bool {
		_, err := os.Stat(valPath)
		return err == nil
	}, "values.json written by extension")

	raw, err := os.ReadFile(valPath)
	if err != nil {
		t.Fatalf("read values.json: %v", err)
	}
	var got map[string]string
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("unmarshal values.json: %v", err)
	}
	if got["/in"] != "user-draft-value" {
		t.Fatalf("Event.Values[/in] = %q, want user-draft-value (all=%v)", got["/in"], got)
	}
	if got["/tog"] != "true" {
		t.Fatalf("Event.Values[/tog] = %q, want true (all=%v)", got["/tog"], got)
	}
	if got["/chk"] != "on" {
		t.Fatalf("Event.Values[/chk] = %q, want on (all=%v)", got["/chk"], got)
	}
	if got["/tabs"] != "TabB" {
		t.Fatalf("Event.Values[/tabs] = %q, want TabB (all=%v)", got["/tabs"], got)
	}
}

func TestF21_T1_BlockFormOverwritesWhenGivenChanges(t *testing.T) {
	const src = `package main

import (
	"context"
	"sync/atomic"
	"github.com/ironpark/gvx-desktop/pkg/extsdk"
)

func main() {
	app := extsdk.New()
	var ver atomic.Int64
	ver.Store(1)
	app.View("main", extsdk.View{
		Render: func(ctx *extsdk.ViewContext, u *extsdk.UI) extsdk.Node {
			val := "v1"
			if ver.Load() >= 2 {
				val = "v2"
			}
			return u.Column(
				u.Input(val, extsdk.Key("cfg"), extsdk.Placeholder("ConfigValue"), extsdk.OnChange("noop")),
			)
		},
		On: extsdk.Handlers{
			"noop": func(ctx *extsdk.EventContext) error { return nil },
		},
	})
	app.Command("reset", func(ctx *extsdk.CommandContext) error {
		ver.Store(2)
		app.NotifyChanged("main")
		return nil
	})
	_ = app.Run(context.Background())
}
`
	extraTOML := e2e3UIMinimalViewTOML + `
[[contributes.command]]
id = "reset"
title = "Reset Given"
`
	mgr, h := newTestManager(t, WithDeveloperMode(true))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := mgr.Start(ctx); err != nil {
		t.Fatalf("mgr.Start: %v", err)
	}
	e2e3UIInstallLocalBinary(t, mgr, "f21-form-overwrite", src, extraTOML)

	m, p := e2e3UINewProbe(t, h.DataDir, mgr, ggui.Sz(1200, 800), nil)
	defer p.Close()
	p.Frame()

	m.Go(ui.ExtensionPage("f21-form-overwrite", "main"))
	e2e3UISettle(t, p, func() bool {
		n, ok := e2e3UIFindSemNode(p.Semantics(), ggui.RoleTextField, "ConfigValue")
		return ok && n.Value == "v1"
	}, "initial given=v1 rendered")

	e2e3UISetTextSem(t, p, "ConfigValue", "user-draft")
	if n, ok := e2e3UIFindSemNode(p.Semantics(), ggui.RoleTextField, "ConfigValue"); !ok || n.Value != "user-draft" {
		t.Fatalf("expected typed draft before SSE update, got %+v", n)
	}

	if err := mgr.RunCommand("f21-form-overwrite", "reset", "en"); err != nil {
		t.Fatalf("RunCommand(reset): %v", err)
	}
	e2e3UISettle(t, p, func() bool {
		n, ok := e2e3UIFindSemNode(p.Semantics(), ggui.RoleTextField, "ConfigValue")
		return ok && n.Value == "v2"
	}, "changed given=v2 overwrote user draft")
}

func TestF21_T1_LiftToolbarExtractsLeadingToolbarRow(t *testing.T) {
	const src = `package main

import (
	"context"
	"github.com/ironpark/gvx-desktop/pkg/extsdk"
)

func main() {
	app := extsdk.New()
	app.View("main", extsdk.View{
		Chrome: "toolbar",
		Render: func(ctx *extsdk.ViewContext, u *extsdk.UI) extsdk.Node {
			return u.Column(
				u.Toolbar(
					u.Button("ToolbarActionBtn", extsdk.Key("tb_btn"), extsdk.OnTap("act")),
				),
				u.Text("BodyAreaMarker"),
			).Fill(true)
		},
		On: extsdk.Handlers{
			"act": func(ctx *extsdk.EventContext) error { return nil },
		},
	})
	_ = app.Run(context.Background())
}
`
	toolbarTOML := `
[[contributes.view]]
id = "main"
title = "ToolbarView"
icon = "box"
chrome = "toolbar"
`
	mgr, h := newTestManager(t, WithDeveloperMode(true))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := mgr.Start(ctx); err != nil {
		t.Fatalf("mgr.Start: %v", err)
	}
	e2e3UIInstallLocalBinary(t, mgr, "f21-toolbar", src, toolbarTOML)

	m, p := e2e3UINewProbe(t, h.DataDir, mgr, ggui.Sz(1280, 840), nil)
	defer p.Close()
	p.Frame()

	m.Go(ui.ExtensionPage("f21-toolbar", "main"))
	e2e3UISettle(t, p, func() bool {
		return e2e3UISemHasText(p.Semantics(), "ToolbarActionBtn") && e2e3UISemHasText(p.Semantics(), "BodyAreaMarker")
	}, "toolbar view rendered")

	tbRect, okTB := e2e3UIInspectWidgetRectByLabel(t, p, "ToolbarActionBtn")
	bodyRect, okBody := e2e3UIInspectWidgetRectByLabel(t, p, "BodyAreaMarker")
	if !okTB || !okBody {
		t.Fatalf("missing rects: tb=%v body=%v\n%s", okTB, okBody, p.Semantics())
	}
	if tbRect.Origin.Y >= bodyRect.Origin.Y {
		t.Fatalf("expected lifted toolbar button (y=%.1f) above body marker (y=%.1f)", tbRect.Origin.Y, bodyRect.Origin.Y)
	}
	var headerRect ggui.Rect
	var headerLabel string
	for _, lbl := range []string{"Reload", "Settings", "Logs"} {
		if r, ok := e2e3UIInspectWidgetRectByLabel(t, p, lbl); ok {
			headerRect = r
			headerLabel = lbl
			break
		}
	}
	if headerLabel == "" {
		t.Fatalf("expected PageExtension header chrome control (Reload/Settings/Logs) in widget tree\n%s", p.Semantics())
	}
	diff := tbRect.Origin.Y - headerRect.Origin.Y
	if diff < -40 || diff > 40 {
		t.Fatalf("lifted toolbar Y=%.1f should share header band with %s Y=%.1f", tbRect.Origin.Y, headerLabel, headerRect.Origin.Y)
	}
}

func TestF21_T2_UnknownIconFallsBackToSquare(t *testing.T) {
	// uiblock.Read blocks unknown icons at parse time, so we feed a direct
	// *uiblock.Node with an unknown icon into ui.RenderUIBlock to verify the
	// renderer's runtime fallback to Lucide "square" without panic.
	p := e2e3UINewBlockProbe(t, func() ggui.Widget {
		root := &uiblock.Node{
			Kind: uiblock.KindColumn,
			Children: []*uiblock.Node{
				{ID: "/ic", Kind: uiblock.KindIcon, Icon: "nonexistent-xyz-icon"},
				{ID: "/btn", Kind: uiblock.KindButton, Text: "FallbackIconBtn", Icon: "also-missing-icon", Tap: true},
			},
		}
		return ui.RenderUIBlock("acme-demo", "main", root, nil)
	}, ggui.Sz(640, 480))
	defer p.Close()

	p.Frame()
	if !e2e3UISemHasText(p.Semantics(), "FallbackIconBtn") {
		t.Fatalf("expected button with unknown icon to still render safely:\n%s", p.Semantics())
	}
}

func TestF21_T2_DialogProvenanceFooter(t *testing.T) {
	const src = `package main

import (
	"context"
	"os"
	"path/filepath"
	"github.com/ironpark/gvx-desktop/pkg/extsdk"
)

func main() {
	app := extsdk.New()
	app.View("main", extsdk.View{
		Render: func(ctx *extsdk.ViewContext, u *extsdk.UI) extsdk.Node {
			return u.Column(
				u.Dialog("Confirm Action",
					extsdk.Key("dlg"),
					extsdk.OnChange("on_dlg"),
					u.Text("Proceed with operation?"),
					u.Button("ApproveOp", extsdk.Key("ok"), extsdk.OnTap("ok")),
				),
			)
		},
		On: extsdk.Handlers{
			"on_dlg": func(ctx *extsdk.EventContext, val string) error {
				return os.WriteFile(filepath.Join(os.Getenv("GVX_EXT_DATA_DIR"), "dlg.txt"), []byte(val), 0o600)
			},
			"ok": func(ctx *extsdk.EventContext) error { return nil },
		},
	})
	_ = app.Run(context.Background())
}
`
	mgr, h := newTestManager(t, WithDeveloperMode(true))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := mgr.Start(ctx); err != nil {
		t.Fatalf("mgr.Start: %v", err)
	}
	e2e3UIInstallLocalBinary(t, mgr, "acme-demo", src, e2e3UIMinimalViewTOML)

	m, p := e2e3UINewProbe(t, h.DataDir, mgr, ggui.Sz(1200, 800), nil)
	defer p.Close()
	p.Frame()

	m.Go(ui.ExtensionPage("acme-demo", "main"))
	e2e3UISettle(t, p, func() bool {
		return e2e3UISemHasText(p.Semantics(), "Confirm Action")
	}, "dialog rendered")

	sem := p.Semantics()
	if !e2e3UISemHasText(sem, "acme-demo asks") {
		t.Fatalf("expected dialog provenance footer %q in semantics:\n%s", "acme-demo asks", sem)
	}

	// Dismissing the dialog via Escape should send change("dismiss") to extension.
	p.Key("escape")
	dlgPath := filepath.Join(h.DataDir, "extension-data", "acme-demo", "files", "dlg.txt")
	e2e3UISettle(t, p, func() bool {
		b, err := os.ReadFile(dlgPath)
		return err == nil && string(b) == "dismiss"
	}, "dialog dismiss event delivered")
}

func TestF21_T2_ScrollFollowAndAutoSplitBreakpoint(t *testing.T) {
	const src = `package main

import (
	"context"
	"fmt"
	"sync/atomic"
	"github.com/ironpark/gvx-desktop/pkg/extsdk"
)

func main() {
	app := extsdk.New()
	var lines atomic.Int64
	lines.Store(2)
	app.View("main", extsdk.View{
		Render: func(ctx *extsdk.ViewContext, u *extsdk.UI) extsdk.Node {
			var logNodes []extsdk.Node
			n := int(lines.Load())
			for i := 1; i <= n; i++ {
				logNodes = append(logNodes, u.Text(fmt.Sprintf("follow-line-%d", i)))
			}
			return u.Column(
				u.Split("auto",
					u.Card(u.Text("LeftAutoPane")),
					u.Scroll(logNodes, extsdk.Variant("follow"), extsdk.MaxHeight(240)),
				),
			).Fill(true)
		},
	})
	app.Command("append", func(ctx *extsdk.CommandContext) error {
		lines.Add(1)
		app.NotifyChanged("main")
		return nil
	})
	_ = app.Run(context.Background())
}
`
	extraTOML := e2e3UIMinimalViewTOML + `
[[contributes.command]]
id = "append"
title = "Append Log"
`
	mgr, h := newTestManager(t, WithDeveloperMode(true))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := mgr.Start(ctx); err != nil {
		t.Fatalf("mgr.Start: %v", err)
	}
	e2e3UIInstallLocalBinary(t, mgr, "f21-split", src, extraTOML)

	m, p := e2e3UINewProbe(t, h.DataDir, mgr, ggui.Sz(1100, 760), nil)
	defer p.Close()
	p.Frame()

	m.Go(ui.ExtensionPage("f21-split", "main"))
	e2e3UISettle(t, p, func() bool {
		return e2e3UISemHasText(p.Semantics(), "LeftAutoPane") && e2e3UISemHasText(p.Semantics(), "follow-line-2")
	}, "auto split + scroll follow initial render")

	// Append line via SSE update while scroll(variant="follow") is active.
	if err := mgr.RunCommand("f21-split", "append", "en"); err != nil {
		t.Fatalf("RunCommand(append): %v", err)
	}
	e2e3UISettle(t, p, func() bool {
		return e2e3UISemHasText(p.Semantics(), "follow-line-3")
	}, "follow scroll received follow-line-3")

	// At >= 640px body width (1100px window), panes sit side-by-side horizontally.
	leftWide, okL := e2e3UIInspectWidgetRectByLabel(t, p, "LeftAutoPane")
	rightWide, okR := e2e3UIInspectWidgetRectByLabel(t, p, "follow-line-1")
	if !okL || !okR {
		t.Fatalf("missing split rects at wide size: left=%v right=%v", okL, okR)
	}
	if leftWide.Origin.X >= rightWide.Origin.X {
		t.Fatalf("expected horizontal split at 1100px: left.X=%.1f right.X=%.1f", leftWide.Origin.X, rightWide.Origin.X)
	}

	// Resize below 640px breakpoint -> panes stack vertically.
	p.Resize(ggui.Sz(500, 760))
	p.Frame()
	leftNarrow, okLN := e2e3UIInspectWidgetRectByLabel(t, p, "LeftAutoPane")
	rightNarrow, okRN := e2e3UIInspectWidgetRectByLabel(t, p, "follow-line-1")
	if !okLN || !okRN {
		t.Fatalf("missing split rects at narrow size: left=%v right=%v", okLN, okRN)
	}
	if leftNarrow.Origin.Y >= rightNarrow.Origin.Y {
		t.Fatalf("expected vertical stack below 640px: left.Y=%.1f right.Y=%.1f", leftNarrow.Origin.Y, rightNarrow.Origin.Y)
	}
}

func TestF21_T2_InvalidOrCorruptNodeResilience(t *testing.T) {
	// Part A: Direct synthetic corrupt *uiblock.Node fed to ui.RenderUIBlock must not panic.
	pBlock := e2e3UINewBlockProbe(t, func() ggui.Widget {
		corrupt := &uiblock.Node{
			Kind: uiblock.KindColumn,
			Children: []*uiblock.Node{
				nil,
				{ID: "/bogus", Kind: "unknown_widget_kind", Text: "Safe"},
				{ID: "/split_bad", Kind: uiblock.KindSplit, Children: []*uiblock.Node{{ID: "/only", Kind: uiblock.KindText, Text: "OnlyOne"}}},
				{ID: "/chart_bad", Kind: uiblock.KindChart, Chart: "nonexistent"},
				{ID: "/ok", Kind: uiblock.KindText, Text: "RecoveredLeaf"},
			},
		}
		return ui.RenderUIBlock("corrupt-ext", "main", corrupt, nil)
	}, ggui.Sz(640, 480))
	defer pBlock.Close()
	pBlock.Frame()
	if !e2e3UISemHasText(pBlock.Semantics(), "RecoveredLeaf") {
		t.Fatalf("expected valid sibling node to render despite corrupt neighbors:\n%s", pBlock.Semantics())
	}

	// Part B: Extension emitting invalid uiblock JSON over SSE stream displays error card without crashing app.
	// Build a pure-Go binary that directly serves malformed uiblock JSON over /v1/views/main/stream.
	const badSSESrc = `package main

import (
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
)

func main() {
	listen := os.Getenv("GVX_EXT_LISTEN")
	tok := os.Getenv("GVX_EXT_TOKEN")
	var ln net.Listener
	var err error
	if strings.HasPrefix(listen, "unix://") {
		p := strings.TrimPrefix(listen, "unix://")
		_ = os.Remove(p)
		ln, err = net.Listen("unix", p)
	} else {
		ln, err = net.Listen("tcp", listen)
	}
	if err != nil {
		os.Exit(2)
	}
	mux := http.NewServeMux()
	auth := func(r *http.Request) bool {
		return r.Header.Get("Authorization") == "Bearer "+tok
	}
	mux.HandleFunc("/v1/health", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(` + "`" + `{"status":"ok","version":"0.1.0"}` + "`" + `))
	})
	mux.HandleFunc("/v1/views/main/stream", func(w http.ResponseWriter, r *http.Request) {
		if !auth(r) {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		fmt.Fprintf(w, "event: tree\ndata: {\"kind\":\"unknown_bad_kind\",\"color\":\"#ff0000\"}\n\n")
		if fl, ok := w.(http.Flusher); ok {
			fl.Flush()
		}
		<-r.Context().Done()
	})
	mux.HandleFunc("/v1/shutdown", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		go func() { os.Exit(0) }()
	})
	srv := &http.Server{Handler: mux}
	go func() {
		sig := make(chan os.Signal, 1)
		signal.Notify(sig, syscall.SIGTERM, os.Interrupt)
		<-sig
		_ = srv.Close()
	}()
	_ = srv.Serve(ln)
}
`
	mgr, h := newTestManager(t, WithDeveloperMode(true))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := mgr.Start(ctx); err != nil {
		t.Fatalf("mgr.Start: %v", err)
	}
	e2e3UIInstallLocalBinary(t, mgr, "f21-bad-sse", badSSESrc, e2e3UIMinimalViewTOML)

	m, p := e2e3UINewProbe(t, h.DataDir, mgr, ggui.Sz(1200, 800), nil)
	defer p.Close()
	p.Frame()

	m.Go(ui.ExtensionPage("f21-bad-sse", "main"))
	e2e3UISettle(t, p, func() bool {
		sem := p.Semantics()
		return e2e3UISemHasText(sem, "Reload") || e2e3UISemHasText(sem, "invalid") || e2e3UISemHasText(sem, "error") || e2e3UISemHasText(sem, "failed")
	}, "error card displayed for invalid SSE tree")
}

func TestF21_T2_LocalsAndGraphDagExcludedFromEventValues(t *testing.T) {
	const src = `package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"github.com/ironpark/gvx-desktop/pkg/extsdk"
)

func main() {
	app := extsdk.New()
	app.View("main", extsdk.View{
		Render: func(ctx *extsdk.ViewContext, u *extsdk.UI) extsdk.Node {
			return u.Column(
				u.Collapsible("LocalCollapsible", extsdk.Key("col"), extsdk.Checked(false),
					u.Text("InsideCollapsible"),
				),
				u.Tree(
					u.TreeItem("LocalTreeItem", extsdk.Key("ti"), extsdk.Check("off"), extsdk.OnCheck("chk")),
				),
				u.Split("horizontal",
					u.Text("LeftSplit"),
					u.Text("RightSplit"),
					extsdk.Key("sp"),
				),
				u.Graph(
					[]extsdk.GraphNode{{ID: "g1", Label: "Node1"}},
					nil,
					extsdk.Key("gr"),
					extsdk.MaxHeight(140),
				),
				u.Dag(
					[]extsdk.DagRow{{ID: "d1", Text: "DagJob"}},
					extsdk.Key("dg"),
					extsdk.MaxHeight(140),
				),
				u.Input("initial-q", extsdk.Key("q"), extsdk.Placeholder("QueryInput"), extsdk.OnChange("noop")),
				u.Button("GoSubmit", extsdk.Key("go"), extsdk.OnTap("go")),
			)
		},
		On: extsdk.Handlers{
			"noop": func(ctx *extsdk.EventContext) error { return nil },
			"chk":  func(ctx *extsdk.EventContext) error { return nil },
			"go": func(ctx *extsdk.EventContext) error {
				b, _ := json.Marshal(ctx.FormValues())
				return os.WriteFile(filepath.Join(os.Getenv("GVX_EXT_DATA_DIR"), "event_values.json"), b, 0o600)
			},
		},
	})
	_ = app.Run(context.Background())
}
`
	mgr, h := newTestManager(t, WithDeveloperMode(true))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := mgr.Start(ctx); err != nil {
		t.Fatalf("mgr.Start: %v", err)
	}
	e2e3UIInstallLocalBinary(t, mgr, "f21-locals", src, e2e3UIMinimalViewTOML)

	m, p := e2e3UINewProbe(t, h.DataDir, mgr, ggui.Sz(1200, 840), nil)
	defer p.Close()
	p.Frame()

	m.Go(ui.ExtensionPage("f21-locals", "main"))
	e2e3UISettle(t, p, func() bool {
		return e2e3UISemHasText(p.Semantics(), "GoSubmit")
	}, "locals/graph/dag view ready")

	// Interact with collapsible, tree_item, and real input field.
	if colNode, ok := e2e3UIFindSemNode(p.Semantics(), "", "LocalCollapsible"); ok {
		p.PerformAction(colNode.ID, ggui.Action{Kind: ggui.ActionExpand})
	}
	if tiNode, ok := e2e3UIFindSemNode(p.Semantics(), ggui.RoleTreeItem, "LocalTreeItem"); ok {
		p.PerformAction(tiNode.ID, ggui.Action{Kind: ggui.ActionPress})
	}
	e2e3UISetTextSem(t, p, "QueryInput", "search-term")
	e2e3UITapSem(t, p, ggui.RoleButton, "GoSubmit")

	valPath := filepath.Join(h.DataDir, "extension-data", "f21-locals", "files", "event_values.json")
	e2e3UISettle(t, p, func() bool {
		_, err := os.Stat(valPath)
		return err == nil
	}, "event_values.json recorded")

	raw, err := os.ReadFile(valPath)
	if err != nil {
		t.Fatalf("read event_values.json: %v", err)
	}
	var got map[string]string
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("unmarshal event_values.json: %v", err)
	}
	if len(got) != 1 || got["/q"] != "search-term" {
		t.Fatalf("expected ONLY /q=search-term in Event.Values (zero locals/graph/dag), got %v", got)
	}
}

// =============================================================================
// Feature 22: Sidebar Extensions Group & PageExtension (5 Tier 1 + 5 Tier 2)
// =============================================================================

func TestF22_T1_SidebarExtensionsGroupVisibleAndViewsListed(t *testing.T) {
	const src = `package main

import (
	"context"
	"github.com/ironpark/gvx-desktop/pkg/extsdk"
)

func main() {
	app := extsdk.New()
	app.View("main", extsdk.View{
		Render: func(ctx *extsdk.ViewContext, u *extsdk.UI) extsdk.Node {
			return u.Column(u.Text("MainViewBody"))
		},
	})
	app.View("logs", extsdk.View{
		Render: func(ctx *extsdk.ViewContext, u *extsdk.UI) extsdk.Node {
			return u.Column(u.Text("LogsViewBody"))
		},
	})
	_ = app.Run(context.Background())
}
`
	viewsTOML := `
[[contributes.view]]
id = "main"
title = "Workflows"
icon = "workflow"

[[contributes.view]]
id = "logs"
title = "Runner Logs"
icon = "terminal"
`
	mgr, h := newTestManager(t, WithDeveloperMode(true))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := mgr.Start(ctx); err != nil {
		t.Fatalf("mgr.Start: %v", err)
	}
	e2e3UIInstallLocalBinary(t, mgr, "f22-sidebar", src, viewsTOML)

	m, p := e2e3UINewProbe(t, h.DataDir, mgr, ggui.Sz(1280, 840), nil)
	defer p.Close()
	e2e3UISettle(t, p, func() bool {
		sem := p.Semantics()
		return e2e3UISemHasText(sem, "Extensions") &&
			e2e3UISemHasText(sem, "Workflows") &&
			e2e3UISemHasText(sem, "Runner Logs")
	}, "sidebar Extensions group and contributed views visible")

	e2e3UITapSem(t, p, ggui.RoleTab, "Runner Logs")
	e2e3UISettle(t, p, func() bool {
		return ggui.Peek(m.Page) == ui.ExtensionPage("f22-sidebar", "logs") &&
			e2e3UISemHasText(p.Semantics(), "LogsViewBody")
	}, "tapping sidebar view navigated to ext:f22-sidebar/logs")
}

func TestF22_T1_LiveBadgeReflectedInSidebar(t *testing.T) {
	const src = `package main

import (
	"context"
	"github.com/ironpark/gvx-desktop/pkg/extsdk"
)

func main() {
	app := extsdk.New()
	app.View("main", extsdk.View{
		Render: func(ctx *extsdk.ViewContext, u *extsdk.UI) extsdk.Node {
			return u.Column(u.Text("BadgeReady"))
		},
	})
	app.Command("set-badge", func(ctx *extsdk.CommandContext) error {
		return ctx.Host.SetBadge(ctx, extsdk.BadgeRequest{
			Text:    "3",
			Tone:    "accent",
			Tooltip: "3 jobs running",
		})
	})
	_ = app.Run(context.Background())
}
`
	extraTOML := e2e3UIMinimalViewTOML + `
[[contributes.command]]
id = "set-badge"
title = "Set Badge"
`
	mgr, h := newTestManager(t, WithDeveloperMode(true))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := mgr.Start(ctx); err != nil {
		t.Fatalf("mgr.Start: %v", err)
	}
	e2e3UIInstallLocalBinary(t, mgr, "f22-badge", src, extraTOML)

	m, p := e2e3UINewProbe(t, h.DataDir, mgr, ggui.Sz(1280, 840), nil)
	defer p.Close()
	p.Frame()
	_ = m

	if err := mgr.RunCommand("f22-badge", "set-badge", "en"); err != nil {
		t.Fatalf("RunCommand(set-badge): %v", err)
	}

	e2e3UISettle(t, p, func() bool {
		return e2e3UISemHasText(p.Semantics(), "3")
	}, "sidebar live badge '3' reflected")
}

func TestF22_T1_PageExtensionRoutingAndHeaderChrome(t *testing.T) {
	const src = `package main

import (
	"context"
	"github.com/ironpark/gvx-desktop/pkg/extsdk"
)

func main() {
	app := extsdk.New()
	app.View("main", extsdk.View{
		Render: func(ctx *extsdk.ViewContext, u *extsdk.UI) extsdk.Node {
			return u.Column(u.Text("ChromeBodyContent"))
		},
	})
	_ = app.Run(context.Background())
}
`
	mgr, h := newTestManager(t, WithDeveloperMode(true))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := mgr.Start(ctx); err != nil {
		t.Fatalf("mgr.Start: %v", err)
	}
	e2e3UIInstallLocalBinary(t, mgr, "f22-chrome", src, e2e3UIMinimalViewTOML)

	m, p := e2e3UINewProbe(t, h.DataDir, mgr, ggui.Sz(1280, 840), nil)
	defer p.Close()
	p.Frame()

	route := ui.ExtensionPage("f22-chrome", "main")
	if route != "ext:f22-chrome/main" {
		t.Fatalf("ExtensionPage = %q, want ext:f22-chrome/main", route)
	}
	m.Go(route)

	e2e3UISettle(t, p, func() bool {
		sem := p.Semantics()
		return e2e3UISemHasText(sem, "ChromeBodyContent") &&
			e2e3UISemHasText(sem, "Binary Extension f22-chrome") &&
			(e2e3UISemHasText(sem, "Reload") || e2e3UISemHasText(sem, "Settings") || e2e3UISemHasText(sem, "Logs"))
	}, "PageExtension routing and header chrome rendered")
}

func TestF22_T1_PageExtensionStateCards(t *testing.T) {
	const src = `package main

import (
	"context"
	"github.com/ironpark/gvx-desktop/pkg/extsdk"
)

func main() {
	app := extsdk.New()
	app.View("main", extsdk.View{
		Render: func(ctx *extsdk.ViewContext, u *extsdk.UI) extsdk.Node {
			return u.Column(u.Text("RunningView"))
		},
	})
	_ = app.Run(context.Background())
}
`
	mgr, h := newTestManager(t, WithDeveloperMode(true))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := mgr.Start(ctx); err != nil {
		t.Fatalf("mgr.Start: %v", err)
	}

	// 1. Waiting state (sandbox ext installed but not approved).
	waitDir := createLocalExtensionDir(t, t.TempDir(), "f22-wait", minimalManifestTOML("f22-wait", "0.1.0", "storage"), nil)
	if _, err := mgr.InstallLocal(waitDir); err != nil {
		t.Fatalf("InstallLocal f22-wait: %v", err)
	}

	// 2. Disabled state.
	e2e3UIInstallLocalBinary(t, mgr, "f22-dis", src, e2e3UIMinimalViewTOML)
	if err := mgr.Disable("f22-dis"); err != nil {
		t.Fatalf("Disable f22-dis: %v", err)
	}

	// 3. Failed state (binary exits immediately -> 3 crashes -> failed).
	failTOML := e2e3UIManifestBinary("f22-fail", "Fail Ext", e2e3UIMinimalViewTOML)
	failDir := filepath.Join(t.TempDir(), "f22-fail")
	writeExtFixture(t, failDir, failTOML, map[string]string{
		"bin/ext": "#!/bin/sh\nexit 1\n",
	})
	if _, err := mgr.InstallLocal(failDir); err != nil {
		t.Fatalf("InstallLocal f22-fail: %v", err)
	}
	_ = mgr.AllowAndBuild("f22-fail")

	m, p := e2e3UINewProbe(t, h.DataDir, mgr, ggui.Sz(1280, 840), nil)
	defer p.Close()
	p.Frame()

	// Verify waiting state card.
	m.Go(ui.ExtensionPage("f22-wait", "main"))
	e2e3UISettle(t, p, func() bool {
		sem := p.Semantics()
		return e2e3UISemHasText(sem, "Allow") || e2e3UISemHasText(sem, "waiting") || e2e3UISemHasText(sem, "Waiting")
	}, "waiting state card rendered")

	// Verify disabled state card.
	m.Go(ui.ExtensionPage("f22-dis", "main"))
	e2e3UISettle(t, p, func() bool {
		sem := p.Semantics()
		return e2e3UISemHasText(sem, "Enable") && (e2e3UISemHasText(sem, "disabled") || e2e3UISemHasText(sem, "Disabled"))
	}, "disabled state card rendered")

	// Verify failed state card.
	e2e3UIWaitExtState(t, mgr, "f22-fail", ext.StateFailed, 8*time.Second)
	m.Go(ui.ExtensionPage("f22-fail", "main"))
	e2e3UISettle(t, p, func() bool {
		sem := p.Semantics()
		return (e2e3UISemHasText(sem, "failed") || e2e3UISemHasText(sem, "Failed")) &&
			(e2e3UISemHasText(sem, "Reload") || e2e3UISemHasText(sem, "Logs"))
	}, "failed state card rendered")

	// 4. Building state card (blocking ImageBuilder pauses in StateBuilding while PageExtension renders).
	builder := &e2e3UIBlockingImageBuilder{
		started: make(chan struct{}),
		release: make(chan struct{}),
	}
	buildDataDir := newTempDataDir(t)
	buildUIHost := NewMockUIHost()
	buildMgr, err := ext.NewManager(ext.Config{
		DataDir:      buildDataDir,
		Keychain:     newFakeKeychain(t),
		UIHost:       buildUIHost,
		ImageBuilder: builder,
	})
	if err != nil {
		t.Fatalf("ext.NewManager(building): %v", err)
	}
	t.Cleanup(func() { _ = buildMgr.Close(3 * time.Second) })
	buildDir := createLocalExtensionDir(t, t.TempDir(), "f22-build", minimalManifestTOML("f22-build", "0.1.0"), nil)
	if _, err := buildMgr.InstallLocal(buildDir); err != nil {
		t.Fatalf("InstallLocal f22-build: %v", err)
	}

	buildM, buildP := e2e3UINewProbe(t, buildDataDir, buildMgr, ggui.Sz(1280, 840), nil)
	defer buildP.Close()
	buildP.Frame()
	buildM.Go(ui.ExtensionPage("f22-build", "main"))

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		_ = buildMgr.AllowAndBuild("f22-build")
	}()
	<-builder.started
	e2e3UISettle(t, buildP, func() bool {
		sem := buildP.Semantics()
		return e2e3UISemHasText(sem, "building") || e2e3UISemHasText(sem, "Building") || e2e3UISemHasText(sem, "Step 1/2")
	}, "building state card rendered")
	close(builder.release)
	wg.Wait()
}

func TestF22_T1_OpenInBrowserLocalProxyWired(t *testing.T) {
	const src = `package main

import (
	"context"
	"github.com/ironpark/gvx-desktop/pkg/extsdk"
)

func main() {
	app := extsdk.New()
	app.View("main", extsdk.View{
		Render: func(ctx *extsdk.ViewContext, u *extsdk.UI) extsdk.Node {
			return u.Column(u.Text("WebUIHostView"))
		},
	})
	_ = app.Run(context.Background())
}
`
	webTOML := e2e3UIMinimalViewTOML + `
[contributes.web_ui]
port = 8080
path = "/ui"
`
	mgr, h := newTestManager(t, WithDeveloperMode(true))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := mgr.Start(ctx); err != nil {
		t.Fatalf("mgr.Start: %v", err)
	}
	e2e3UIInstallLocalBinary(t, mgr, "f22-webui", src, webTOML, ext.PermLocalProxy)

	shimDir := t.TempDir()
	openedFile := filepath.Join(shimDir, "opened_url.txt")
	shimScript := fmt.Sprintf("#!/bin/sh\nprintf '%%s\\n' \"$1\" > %q\n", openedFile)
	for _, binName := range []string{"open", "xdg-open"} {
		if err := os.WriteFile(filepath.Join(shimDir, binName), []byte(shimScript), 0o755); err != nil {
			t.Fatalf("write %s shim: %v", binName, err)
		}
	}
	t.Setenv("PATH", shimDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	httpPort := e2e3UIFreePort(t)
	httpsPort := e2e3UIFreePort(t)
	for httpsPort == httpPort {
		httpsPort = e2e3UIFreePort(t)
	}

	m, p := e2e3UINewProbe(t, h.DataDir, mgr, ggui.Sz(1280, 840), func(mod *ui.Model) {
		mod.SetLocalProxyPorts(httpPort, httpsPort)
		mod.SetLocalProxy(true)
	})
	defer m.CloseLocalProxy()
	defer p.Close()
	p.Frame()

	routes := mgr.LocalProxyRoutes()
	wantHost := localproxy.BuildHost("f22-webui")
	foundRoute := false
	for _, rt := range routes {
		if rt.Host == wantHost {
			foundRoute = true
			break
		}
	}
	if !foundRoute {
		t.Fatalf("expected LocalProxyRoutes() to contain %q, got %+v", wantHost, routes)
	}

	m.Go(ui.ExtensionPage("f22-webui", "main"))
	e2e3UISettle(t, p, func() bool {
		return e2e3UISemHasText(p.Semantics(), "Open in Browser")
	}, "Open in Browser button visible on PageExtension")

	e2e3UITapSem(t, p, ggui.RoleButton, "Open in Browser")
	e2e3UISettle(t, p, func() bool {
		for _, u := range h.UIHost.SnapshotOpenURLs() {
			if strings.Contains(u, wantHost) && strings.Contains(u, "/ui") {
				return true
			}
		}
		if b, err := os.ReadFile(openedFile); err == nil {
			u := string(b)
			return strings.Contains(u, wantHost) && strings.Contains(u, "/ui")
		}
		return false
	}, "Open in Browser opened localproxy URL containing wantHost and /ui")
}

func TestF22_T2_SidebarExtensionsGroupHiddenWhenZeroEnabled(t *testing.T) {
	// Subcase A: 0 extensions installed.
	mgrEmpty, hEmpty := newTestManager(t, WithDeveloperMode(true))
	_, pEmpty := e2e3UINewProbe(t, hEmpty.DataDir, mgrEmpty, ggui.Sz(1280, 840), nil)
	defer pEmpty.Close()
	pEmpty.Frame()
	if e2e3UISemHasText(pEmpty.Semantics(), "Extensions") {
		t.Fatalf("expected sidebar Extensions group hidden with 0 installed extensions:\n%s", pEmpty.Semantics())
	}

	// Subcase B: 1 installed extension, disabled -> Extensions group still hidden.
	const src = `package main

import (
	"context"
	"github.com/ironpark/gvx-desktop/pkg/extsdk"
)

func main() {
	app := extsdk.New()
	app.View("main", extsdk.View{
		Render: func(ctx *extsdk.ViewContext, u *extsdk.UI) extsdk.Node {
			return u.Column(u.Text("HiddenWhenDisabled"))
		},
	})
	_ = app.Run(context.Background())
}
`
	mgrDis, hDis := newTestManager(t, WithDeveloperMode(true))
	e2e3UIInstallLocalBinary(t, mgrDis, "f22-disabled-sb", src, e2e3UIMinimalViewTOML)
	if err := mgrDis.Disable("f22-disabled-sb"); err != nil {
		t.Fatalf("Disable: %v", err)
	}
	_, pDis := e2e3UINewProbe(t, hDis.DataDir, mgrDis, ggui.Sz(1280, 840), nil)
	defer pDis.Close()
	pDis.Frame()
	if e2e3UISemHasText(pDis.Semantics(), "Extensions") {
		t.Fatalf("expected sidebar Extensions group hidden when all extensions disabled:\n%s", pDis.Semantics())
	}
}

func TestF22_T2_NonRunningStatesShowCardNotCrashedView(t *testing.T) {
	failTOML := e2e3UIManifestBinary("f22-crash3", "Crash3 Ext", e2e3UIMinimalViewTOML)
	mgr, h := newTestManager(t, WithDeveloperMode(true))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := mgr.Start(ctx); err != nil {
		t.Fatalf("mgr.Start: %v", err)
	}
	extDir := filepath.Join(t.TempDir(), "f22-crash3")
	writeExtFixture(t, extDir, failTOML, map[string]string{
		"bin/ext": "#!/bin/sh\necho 'fatal boom' >&2\nexit 1\n",
	})
	if _, err := mgr.InstallLocal(extDir); err != nil {
		t.Fatalf("InstallLocal: %v", err)
	}
	_ = mgr.AllowAndBuild("f22-crash3")

	e2e3UIWaitExtState(t, mgr, "f22-crash3", ext.StateFailed, 8*time.Second)

	m, p := e2e3UINewProbe(t, h.DataDir, mgr, ggui.Sz(1280, 840), nil)
	defer p.Close()
	p.Frame()

	m.Go(ui.ExtensionPage("f22-crash3", "main"))
	e2e3UISettle(t, p, func() bool {
		sem := p.Semantics()
		return e2e3UISemHasText(sem, "Crash3 Ext") && (e2e3UISemHasText(sem, "failed") || e2e3UISemHasText(sem, "Failed"))
	}, "crashed extension shows error state card")
}

func TestF22_T2_OpenInBrowserHiddenWithoutLocalProxyPerm(t *testing.T) {
	const src = `package main

import (
	"context"
	"github.com/ironpark/gvx-desktop/pkg/extsdk"
)

func main() {
	app := extsdk.New()
	app.View("main", extsdk.View{
		Render: func(ctx *extsdk.ViewContext, u *extsdk.UI) extsdk.Node {
			return u.Column(u.Text("NoProxyBody"))
		},
	})
	_ = app.Run(context.Background())
}
`
	// Subcase A: valid running extension without [contributes.web_ui] and without localproxy permission.
	mgr, h := newTestManager(t, WithDeveloperMode(true))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := mgr.Start(ctx); err != nil {
		t.Fatalf("mgr.Start: %v", err)
	}
	e2e3UIInstallLocalBinary(t, mgr, "f22-noproxy", src, e2e3UIMinimalViewTOML)

	if routes := mgr.LocalProxyRoutes(); len(routes) != 0 {
		t.Fatalf("expected 0 localproxy routes without localproxy permission, got %+v", routes)
	}

	m, p := e2e3UINewProbe(t, h.DataDir, mgr, ggui.Sz(1280, 840), nil)
	defer p.Close()
	p.Frame()

	m.Go(ui.ExtensionPage("f22-noproxy", "main"))
	e2e3UISettle(t, p, func() bool {
		return e2e3UISemHasText(p.Semantics(), "NoProxyBody")
	}, "view without localproxy rendered")

	if e2e3UISemHasText(p.Semantics(), "Open in Browser") {
		t.Fatalf("Open in Browser must be hidden without localproxy permission:\n%s", p.Semantics())
	}

	// Subcase B: extension declaring [contributes.web_ui] + localproxy permission, then RevokePermissions.
	webTOML := e2e3UIMinimalViewTOML + `
[contributes.web_ui]
port = 8080
path = "/ui"
`
	e2e3UIInstallLocalBinary(t, mgr, "f22-noproxy-revoked", src, webTOML, ext.PermLocalProxy)
	if err := mgr.RevokePermissions("f22-noproxy-revoked"); err != nil {
		t.Fatalf("RevokePermissions: %v", err)
	}
	wantRevokedHost := localproxy.BuildHost("f22-noproxy-revoked")
	for _, rt := range mgr.LocalProxyRoutes() {
		if rt.Host == wantRevokedHost {
			t.Fatalf("expected revoked extension route %q removed from LocalProxyRoutes(), got %+v", wantRevokedHost, mgr.LocalProxyRoutes())
		}
	}

	m.Go(ui.ExtensionPage("f22-noproxy-revoked", "main"))
	e2e3UISettle(t, p, func() bool {
		sem := p.Semantics()
		return e2e3UISemHasText(sem, "Allow") || e2e3UISemHasText(sem, "waiting") || e2e3UISemHasText(sem, "Waiting")
	}, "revoked web_ui extension renders waiting state card")

	if e2e3UISemHasText(p.Semantics(), "Open in Browser") {
		t.Fatalf("Open in Browser must be hidden when localproxy permission is revoked:\n%s", p.Semantics())
	}
}

func TestF22_T2_DevUnsandboxedBadgeShownForBinaryRuntime(t *testing.T) {
	const src = `package main

import (
	"context"
	"github.com/ironpark/gvx-desktop/pkg/extsdk"
)

func main() {
	app := extsdk.New()
	app.View("main", extsdk.View{
		Render: func(ctx *extsdk.ViewContext, u *extsdk.UI) extsdk.Node {
			return u.Column(u.Text("UnsandboxedBody"))
		},
	})
	_ = app.Run(context.Background())
}
`
	mgr, h := newTestManager(t, WithDeveloperMode(true))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := mgr.Start(ctx); err != nil {
		t.Fatalf("mgr.Start: %v", err)
	}
	e2e3UIInstallLocalBinary(t, mgr, "f22-dev-badge", src, e2e3UIMinimalViewTOML)

	info, ok := e2e3UIFindExtInfo(mgr, "f22-dev-badge")
	if !ok || !info.Unsandboxed {
		t.Fatalf("expected binary runtime ExtensionInfo.Unsandboxed == true, got %+v", info)
	}

	m, p := e2e3UINewProbe(t, h.DataDir, mgr, ggui.Sz(1280, 840), nil)
	defer p.Close()
	p.Frame()

	m.Go(ui.ExtensionPage("f22-dev-badge", "main"))
	e2e3UISettle(t, p, func() bool {
		sem := p.Semantics()
		return e2e3UISemHasText(sem, "UnsandboxedBody") &&
			(e2e3UISemHasText(sem, "Dev · Unsandboxed") || e2e3UISemHasText(sem, "Unsandboxed"))
	}, "Dev · Unsandboxed badge shown on PageExtension")
}

func TestF22_T2_NavigatingAwayCancelsSSESubscription(t *testing.T) {
	const sseTrackSrc = `package main

import (
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
)

func main() {
	listen := os.Getenv("GVX_EXT_LISTEN")
	tok := os.Getenv("GVX_EXT_TOKEN")
	dataDir := os.Getenv("GVX_EXT_DATA_DIR")
	var ln net.Listener
	var err error
	if strings.HasPrefix(listen, "unix://") {
		p := strings.TrimPrefix(listen, "unix://")
		_ = os.Remove(p)
		ln, err = net.Listen("unix", p)
	} else {
		ln, err = net.Listen("tcp", listen)
	}
	if err != nil {
		os.Exit(2)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/health", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(` + "`" + `{"status":"ok","version":"0.1.0"}` + "`" + `))
	})
	mux.HandleFunc("/v1/views/main/stream", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+tok {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		_ = os.WriteFile(filepath.Join(dataDir, "sse_opened.txt"), []byte("1"), 0o600)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		fmt.Fprintf(w, "event: tree\ndata: {\"kind\":\"column\",\"children\":[{\"kind\":\"text\",\"text\":\"TrackedSSEView\"}]}\n\n")
		if fl, ok := w.(http.Flusher); ok {
			fl.Flush()
		}
		<-r.Context().Done()
		_ = os.WriteFile(filepath.Join(dataDir, "sse_closed.txt"), []byte("1"), 0o600)
	})
	mux.HandleFunc("/v1/shutdown", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		go func() { os.Exit(0) }()
	})
	srv := &http.Server{Handler: mux}
	go func() {
		sig := make(chan os.Signal, 1)
		signal.Notify(sig, syscall.SIGTERM, os.Interrupt)
		<-sig
		_ = srv.Close()
	}()
	_ = srv.Serve(ln)
}
`
	mgr, h := newTestManager(t, WithDeveloperMode(true))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := mgr.Start(ctx); err != nil {
		t.Fatalf("mgr.Start: %v", err)
	}
	e2e3UIInstallLocalBinary(t, mgr, "f22-sse-cancel", sseTrackSrc, e2e3UIMinimalViewTOML)

	m, p := e2e3UINewProbe(t, h.DataDir, mgr, ggui.Sz(1280, 840), nil)
	defer p.Close()
	p.Frame()

	filesDir := filepath.Join(h.DataDir, "extension-data", "f22-sse-cancel", "files")
	m.Go(ui.ExtensionPage("f22-sse-cancel", "main"))
	e2e3UISettle(t, p, func() bool {
		_, err := os.Stat(filepath.Join(filesDir, "sse_opened.txt"))
		return err == nil && e2e3UISemHasText(p.Semantics(), "TrackedSSEView")
	}, "SSE stream opened")

	// Navigate away from PageExtension -> SSE context must cancel promptly.
	m.Go(ui.PageDashboard)
	e2e3UISettle(t, p, func() bool {
		_, err := os.Stat(filepath.Join(filesDir, "sse_closed.txt"))
		return err == nil
	}, "SSE stream closed after navigating away")
}

// =============================================================================
// Feature 23: Command Palette (⌘K) & Settings ▸ Extensions (5 Tier 1 + 5 Tier 2)
// =============================================================================

func TestF23_T1_CommandPaletteRegistersAndTriggersCommand(t *testing.T) {
	const src = `package main

import (
	"context"
	"github.com/ironpark/gvx-desktop/pkg/extsdk"
)

func main() {
	app := extsdk.New()
	app.View("main", extsdk.View{
		Render: func(ctx *extsdk.ViewContext, u *extsdk.UI) extsdk.Node {
			return u.Column(u.Text("CmdTargetView"))
		},
	})
	app.Command("sync-now", func(ctx *extsdk.CommandContext) error {
		return ctx.Host.Copy(ctx, "copied-from-palette-cmd")
	})
	_ = app.Run(context.Background())
}
`
	extraTOML := e2e3UIMinimalViewTOML + `
[[contributes.command]]
id = "sync-now"
title = "Acme: Sync Now"
`
	mgr, h := newTestManager(t, WithDeveloperMode(true))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := mgr.Start(ctx); err != nil {
		t.Fatalf("mgr.Start: %v", err)
	}
	e2e3UIInstallLocalBinary(t, mgr, "f23-cmd", src, extraTOML)

	m, p := e2e3UINewProbe(t, h.DataDir, mgr, ggui.Sz(1280, 840), nil)
	defer p.Close()
	p.Frame()

	m.PaletteQuery.Set("")
	m.PaletteOpen.Set(true)
	e2e3UISettle(t, p, func() bool {
		return e2e3UISemHasText(p.Semantics(), "Acme: Sync Now")
	}, "extension command visible in ⌘K palette")

	e2e3UITapSem(t, p, "", "Acme: Sync Now")
	e2e3UISettle(t, p, func() bool {
		return slices.Contains(h.UIHost.SnapshotCopied(), "copied-from-palette-cmd")
	}, "command triggered Copy with valid X-GVX-Gesture-ID")
}

func TestF23_T1_SettingsExtensionsListsStatusAndLogs(t *testing.T) {
	const src = `package main

import (
	"context"
	"fmt"
	"os"
	"github.com/ironpark/gvx-desktop/pkg/extsdk"
)

func main() {
	fmt.Fprintln(os.Stderr, "ext-boot-log-marker-42")
	app := extsdk.New()
	app.View("main", extsdk.View{
		Render: func(ctx *extsdk.ViewContext, u *extsdk.UI) extsdk.Node {
			return u.Column(u.Text("SettingsLogView"))
		},
	})
	_ = app.Run(context.Background())
}
`
	mgr, h := newTestManager(t, WithDeveloperMode(true))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := mgr.Start(ctx); err != nil {
		t.Fatalf("mgr.Start: %v", err)
	}
	e2e3UIInstallLocalBinary(t, mgr, "f23-logs", src, e2e3UIMinimalViewTOML)

	m, p := e2e3UINewProbe(t, h.DataDir, mgr, ggui.Sz(1280, 840), nil)
	defer p.Close()
	p.Frame()

	m.Go(ui.PageSettings)
	e2e3UISettle(t, p, func() bool {
		sem := p.Semantics()
		return e2e3UISemHasText(sem, "Extensions") &&
			e2e3UISemHasText(sem, "Binary Extension f23-logs") &&
			e2e3UISemHasText(sem, "0.1.0")
	}, "Settings ▸ Extensions lists extension name and version")

	// Expand/open logs if behind a Logs button, or inspect directly if already visible.
	if !e2e3UISemHasText(p.Semantics(), "ext-boot-log-marker-42") {
		if _, ok := e2e3UIFindSemNode(p.Semantics(), ggui.RoleButton, "Logs"); ok {
			e2e3UITapSem(t, p, ggui.RoleButton, "Logs")
		}
	}
	e2e3UISettle(t, p, func() bool {
		return e2e3UISemHasText(p.Semantics(), "ext-boot-log-marker-42")
	}, "extension stderr log visible in Settings ▸ Extensions")
}

func TestF23_T1_SettingsInstallAndAllowAndBuildTriggers(t *testing.T) {
	mgr, h := newTestManager(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := mgr.Start(ctx); err != nil {
		t.Fatalf("mgr.Start: %v", err)
	}

	m, p := e2e3UINewProbe(t, h.DataDir, mgr, ggui.Sz(1280, 840), nil)
	defer p.Close()
	m.Go(ui.PageSettings)
	p.Frame()

	// 1. InstallLocal -> waiting -> AllowAndBuild -> ImageBuilder invoked.
	localDir := createLocalExtensionDir(t, t.TempDir(), "f23-local-sbx", minimalManifestTOML("f23-local-sbx", "0.1.0"), nil)
	infoLocal, err := mgr.InstallLocal(localDir)
	if err != nil {
		t.Fatalf("InstallLocal: %v", err)
	}
	if infoLocal.State != ext.StateWaiting {
		t.Fatalf("InstallLocal state = %q, want waiting", infoLocal.State)
	}
	e2e3UISettle(t, p, func() bool {
		return e2e3UISemHasText(p.Semantics(), "Test Extension f23-local-sbx")
	}, "local install listed in Settings")

	// AllowAndBuild may fail at container launch without rungv, but MUST run ImageBuilder first.
	_ = mgr.AllowAndBuild("f23-local-sbx")
	if len(h.ImageBuilder.SnapshotCalls()) < 1 {
		t.Fatal("expected MockImageBuilder.Build to be invoked on AllowAndBuild for local extension")
	}

	// 2. InstallGit -> waiting -> AllowAndBuild -> ImageBuilder invoked.
	bareURL, _ := initBareGitRepo(t, map[string]string{
		"extension.toml": minimalManifestTOML("f23-git-sbx", "0.2.0"),
		"Dockerfile":     "FROM alpine:3.20\nCMD [\"/bin/sh\"]\n",
	})
	bareDir := strings.TrimPrefix(bareURL, "file://")
	httpsURL := e2e3UIServeGitHTTPS(t, bareDir)
	infoGit, err := mgr.InstallGit(httpsURL, "main")
	if err != nil {
		t.Fatalf("InstallGit: %v", err)
	}
	if infoGit.State != ext.StateWaiting {
		t.Fatalf("InstallGit state = %q, want waiting", infoGit.State)
	}
	beforeCalls := len(h.ImageBuilder.SnapshotCalls())
	_ = mgr.AllowAndBuild("f23-git-sbx")
	if len(h.ImageBuilder.SnapshotCalls()) <= beforeCalls {
		t.Fatal("expected MockImageBuilder.Build to be invoked on AllowAndBuild for git extension")
	}

	// 3. InstallImage -> waiting -> AllowAndBuild -> ImagePuller invoked.
	infoImg, err := mgr.InstallImage("ghcr.io/acme/prebuilt-ext:0.3.0")
	if err != nil {
		t.Fatalf("InstallImage: %v", err)
	}
	if infoImg.State != ext.StateWaiting {
		t.Fatalf("InstallImage state = %q, want waiting", infoImg.State)
	}
	if refs := h.ImagePuller.SnapshotPulledRefs(); len(refs) != 0 {
		t.Fatalf("InstallImage pulled before approval: %v", refs)
	}
	_ = mgr.AllowAndBuild(infoImg.ID)
	if !slices.Contains(h.ImagePuller.SnapshotPulledRefs(), "ghcr.io/acme/prebuilt-ext:0.3.0") {
		t.Fatalf("expected MockImagePuller.Pull called for ghcr.io/acme/prebuilt-ext:0.3.0, got %v", h.ImagePuller.SnapshotPulledRefs())
	}
	e2e3UISettle(t, p, func() bool {
		sem := p.Semantics()
		return e2e3UISemHasText(sem, "Test Extension f23-git-sbx") &&
			((infoImg.Name != "" && e2e3UISemHasText(sem, infoImg.Name)) || e2e3UISemHasText(sem, infoImg.ID))
	}, "git and image extensions listed in Settings")
}

func TestF23_T1_SettingsPermissionsFoldersAndAutoForm(t *testing.T) {
	const src = `package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"github.com/ironpark/gvx-desktop/pkg/extsdk"
)

func main() {
	app := extsdk.New()
	app.View("main", extsdk.View{
		Render: func(ctx *extsdk.ViewContext, u *extsdk.UI) extsdk.Node {
			return u.Column(
				u.Button("PickFolderBtn", extsdk.Key("pick"), extsdk.OnTap("pick")),
			)
		},
		On: extsdk.Handlers{
			"pick": func(ctx *extsdk.EventContext) error {
				_, _, err := ctx.Host.PickFolder(ctx)
				return err
			},
		},
	})
	app.Command("pick-cmd", func(ctx *extsdk.CommandContext) error {
		_, _, err := ctx.Host.PickFolder(ctx)
		return err
	})
	app.OnSettingsChanged(func(ctx *extsdk.Context, changed map[string]any) error {
		b, _ := json.Marshal(changed)
		return os.WriteFile(filepath.Join(os.Getenv("GVX_EXT_DATA_DIR"), "settings_changed.json"), b, 0o600)
	})
	_ = app.Run(context.Background())
}
`
	settingsTOML := e2e3UIMinimalViewTOML + `
[[contributes.command]]
id = "pick-cmd"
title = "Pick Workspace"

[contributes.settings]
endpoint_url = { type = "string", label = "Endpoint URL", default = "https://initial.invalid" }
api_secret   = { type = "secret", label = "API Secret" }
auto_retry   = { type = "boolean", label = "Auto Retry", default = false }
max_workers  = { type = "number", label = "Max Workers", default = 2 }
run_mode     = { type = "select", label = "Run Mode", options = ["fast", "safe"], default = "safe" }
`
	mgr, h := newTestManager(t, WithDeveloperMode(true))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := mgr.Start(ctx); err != nil {
		t.Fatalf("mgr.Start: %v", err)
	}
	pickedDir := e2e3UINonReservedDir(t)
	h.UIHost.PickFolderResult = pickedDir

	e2e3UIInstallLocalBinary(t, mgr, "f23-settings-form", src, settingsTOML, ext.PermStorage, ext.PermUIPickFolder)

	if err := mgr.RunCommand("f23-settings-form", "pick-cmd", "en"); err != nil {
		t.Fatalf("RunCommand(pick-cmd): %v", err)
	}

	m, p := e2e3UINewProbe(t, h.DataDir, mgr, ggui.Sz(1280, 900), nil)
	defer p.Close()
	p.Frame()

	m.Go(ui.PageSettings)
	e2e3UISettle(t, p, func() bool {
		sem := p.Semantics()
		return e2e3UISemHasText(sem, "Endpoint URL") &&
			e2e3UISemHasText(sem, "API Secret") &&
			e2e3UISemHasText(sem, "Auto Retry") &&
			e2e3UISemHasText(sem, "Max Workers") &&
			e2e3UISemHasText(sem, "Run Mode") &&
			e2e3UISemHasText(sem, filepath.Base(pickedDir))
	}, "all 5 auto settings controls and allowed folder rendered")

	// Update settings via SetSetting and verify settings_changed pushed to running extension.
	if err := mgr.SetSetting("f23-settings-form", "endpoint_url", "https://updated.invalid"); err != nil {
		t.Fatalf("SetSetting(string): %v", err)
	}
	if err := mgr.SetSetting("f23-settings-form", "api_secret", "top-secret-key-val"); err != nil {
		t.Fatalf("SetSetting(secret): %v", err)
	}
	if err := mgr.SetSetting("f23-settings-form", "auto_retry", true); err != nil {
		t.Fatalf("SetSetting(boolean): %v", err)
	}
	if err := mgr.SetSetting("f23-settings-form", "max_workers", 4.0); err != nil {
		t.Fatalf("SetSetting(number): %v", err)
	}
	if err := mgr.SetSetting("f23-settings-form", "run_mode", "fast"); err != nil {
		t.Fatalf("SetSetting(select): %v", err)
	}

	changedFile := filepath.Join(h.DataDir, "extension-data", "f23-settings-form", "files", "settings_changed.json")
	e2e3UISettle(t, p, func() bool {
		_, err := os.Stat(changedFile)
		return err == nil
	}, "OnSettingsChanged invoked on extension")

	// Remove allowed folder and revoke permissions cleanly.
	if err := mgr.RemoveAllowedFolder("f23-settings-form", pickedDir); err != nil {
		t.Fatalf("RemoveAllowedFolder: %v", err)
	}
	if err := mgr.RevokePermissions("f23-settings-form"); err != nil {
		t.Fatalf("RevokePermissions: %v", err)
	}
	info, ok := e2e3UIFindExtInfo(mgr, "f23-settings-form")
	if !ok {
		t.Fatal("missing extension info after revoke")
	}
	if len(info.AllowedFolders) != 0 || len(info.Granted) != 0 {
		t.Fatalf("expected allowed_folders and granted cleared, got %+v", info)
	}
}

func TestF23_T1_SettingsDeveloperModeToggleAndUninstall(t *testing.T) {
	const src = `package main

import (
	"context"
	"github.com/ironpark/gvx-desktop/pkg/extsdk"
)

func main() {
	app := extsdk.New()
	app.View("main", extsdk.View{
		Render: func(ctx *extsdk.ViewContext, u *extsdk.UI) extsdk.Node {
			return u.Column(u.Text("DevUninstallBody"))
		},
	})
	_ = app.Run(context.Background())
}
`
	mgr, h := newTestManager(t, WithDeveloperMode(false))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := mgr.Start(ctx); err != nil {
		t.Fatalf("mgr.Start: %v", err)
	}
	binData, err := os.ReadFile(buildFixtureSDKBinary(t, src))
	if err != nil {
		t.Fatalf("read fixture binary: %v", err)
	}
	extDir := filepath.Join(t.TempDir(), "f23-dev-uninst")
	writeExtFixture(t, extDir, e2e3UIManifestBinary("f23-dev-uninst", "Binary Extension f23-dev-uninst", e2e3UIMinimalViewTOML), nil)
	if err := os.MkdirAll(filepath.Join(extDir, "bin"), 0o700); err != nil {
		t.Fatalf("mkdir bin: %v", err)
	}
	if err := os.WriteFile(filepath.Join(extDir, "bin", "ext"), binData, 0o755); err != nil {
		t.Fatalf("write bin/ext: %v", err)
	}
	if _, err := mgr.InstallLocal(extDir); err != nil {
		t.Fatalf("InstallLocal: %v", err)
	}

	m, p := e2e3UINewProbe(t, h.DataDir, mgr, ggui.Sz(1280, 840), nil)
	defer p.Close()
	m.Go(ui.PageSettings)
	e2e3UISettle(t, p, func() bool {
		return e2e3UISemHasText(p.Semantics(), "Developer Mode") || e2e3UISemHasText(p.Semantics(), "Developer mode")
	}, "Developer Mode toggle visible in Settings ▸ Extensions")

	e2e3UITapSem(t, p, ggui.RoleSwitch, "Developer")
	st := newTestStore(t, h.DataDir)
	e2e3UISettle(t, p, func() bool {
		savedSet, err := st.Settings()
		return err == nil && savedSet.DeveloperMode
	}, "DeveloperMode == true persisted in store.Settings")

	if err := mgr.Uninstall("f23-dev-uninst", false); err != nil {
		t.Fatalf("Uninstall: %v", err)
	}
	e2e3UISettle(t, p, func() bool {
		return !e2e3UISemHasText(p.Semantics(), "Binary Extension f23-dev-uninst")
	}, "uninstalled extension removed from Settings UI")
}

func TestF23_T2_CommandPaletteOmitsNonRunnableCommands(t *testing.T) {
	const src = `package main

import (
	"context"
	"github.com/ironpark/gvx-desktop/pkg/extsdk"
)

func main() {
	app := extsdk.New()
	app.View("main", extsdk.View{
		Render: func(ctx *extsdk.ViewContext, u *extsdk.UI) extsdk.Node {
			return u.Column(u.Text("CmdStateCheck"))
		},
	})
	app.Command("cmd", func(ctx *extsdk.CommandContext) error { return nil })
	_ = app.Run(context.Background())
}
`
	cmdTOML := func(title string) string {
		return e2e3UIMinimalViewTOML + fmt.Sprintf("\n[[contributes.command]]\nid = \"cmd\"\ntitle = %q\n", title)
	}

	mgr, h := newTestManager(t, WithDeveloperMode(true))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := mgr.Start(ctx); err != nil {
		t.Fatalf("mgr.Start: %v", err)
	}

	// 1. Running extension -> command MUST appear.
	e2e3UIInstallLocalBinary(t, mgr, "f23-run", src, cmdTOML("Runnable: ActiveCmd"))
	// 2. Disabled extension -> omitted.
	e2e3UIInstallLocalBinary(t, mgr, "f23-dis", src, cmdTOML("Disabled: HiddenCmd"))
	if err := mgr.Disable("f23-dis"); err != nil {
		t.Fatalf("Disable: %v", err)
	}
	// 3. Waiting extension -> omitted.
	waitDir := createLocalExtensionDir(t, t.TempDir(), "f23-wait", minimalManifestTOML("f23-wait", "0.1.0")+`
[[contributes.command]]
id = "cmd"
title = "Waiting: HiddenCmd"
`, nil)
	if _, err := mgr.InstallLocal(waitDir); err != nil {
		t.Fatalf("InstallLocal f23-wait: %v", err)
	}
	// 4. Failed extension -> omitted.
	failManifest := e2e3UIManifestBinary("f23-fail", "Failed Cmd Ext", cmdTOML("Failed: HiddenCmd"))
	failDir := filepath.Join(t.TempDir(), "f23-fail")
	writeExtFixture(t, failDir, failManifest, map[string]string{
		"bin/ext": "#!/bin/sh\nexit 1\n",
	})
	if _, err := mgr.InstallLocal(failDir); err != nil {
		t.Fatalf("InstallLocal(f23-fail): %v", err)
	}
	if err := mgr.AllowAndBuild("f23-fail"); err == nil {
		t.Fatal("expected AllowAndBuild(f23-fail) to fail for exit-1 script")
	}
	e2e3UIWaitExtState(t, mgr, "f23-fail", ext.StateFailed, 8*time.Second)

	m, p := e2e3UINewProbe(t, h.DataDir, mgr, ggui.Sz(1280, 840), nil)
	defer p.Close()
	p.Frame()

	m.PaletteQuery.Set("")
	m.PaletteOpen.Set(true)
	e2e3UISettle(t, p, func() bool {
		return e2e3UISemHasText(p.Semantics(), "Runnable: ActiveCmd")
	}, "running extension command in palette")

	sem := p.Semantics()
	for _, hidden := range []string{"Disabled: HiddenCmd", "Waiting: HiddenCmd", "Failed: HiddenCmd"} {
		if e2e3UISemHasText(sem, hidden) {
			t.Fatalf("non-runnable command %q leaked into ⌘K palette:\n%s", hidden, sem)
		}
	}
}

func TestF23_T2_KeychainFileFallbackNoticeShown(t *testing.T) {
	t.Setenv(keychain.EnvVar, "file")
	mgr, h := newTestManager(t, WithKeychain(nil, "Secret Service unavailable"), WithDeveloperMode(true))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := mgr.Start(ctx); err != nil {
		t.Fatalf("mgr.Start: %v", err)
	}

	m, p := e2e3UINewProbe(t, h.DataDir, mgr, ggui.Sz(1280, 840), nil)
	defer p.Close()
	m.Go(ui.PageSettings)
	e2e3UISettle(t, p, func() bool {
		sem := p.Semantics()
		return e2e3UISemHasText(sem, "Secret Service unavailable") ||
			(e2e3UISemHasText(sem, "file") && e2e3UISemHasText(sem, "keychain"))
	}, "keychain file fallback notice displayed in Settings ▸ Extensions")
}

func TestF23_T2_SecretSettingNeverPrefilledInPlaintext(t *testing.T) {
	const secretVal = "super-secret-pat-value-98765"
	const src = `package main

import (
	"context"
	"github.com/ironpark/gvx-desktop/pkg/extsdk"
)

func main() {
	app := extsdk.New()
	app.View("main", extsdk.View{
		Render: func(ctx *extsdk.ViewContext, u *extsdk.UI) extsdk.Node {
			return u.Column(u.Text("SecretCheckBody"))
		},
	})
	_ = app.Run(context.Background())
}
`
	secretTOML := e2e3UIMinimalViewTOML + `
[contributes.settings]
github_pat = { type = "secret", label = "GitHub Token", description = "Personal access token" }
`
	mgr, h := newTestManager(t, WithDeveloperMode(true))
	e2e3UIInstallLocalBinary(t, mgr, "f23-secret-mask", src, secretTOML, ext.PermSecrets)
	if err := mgr.SetSetting("f23-secret-mask", "github_pat", secretVal); err != nil {
		t.Fatalf("SetSetting(secret): %v", err)
	}

	m, p := e2e3UINewProbe(t, h.DataDir, mgr, ggui.Sz(1280, 840), nil)
	defer p.Close()
	m.Go(ui.PageSettings)
	e2e3UISettle(t, p, func() bool {
		return e2e3UISemHasText(p.Semantics(), "GitHub Token")
	}, "secret setting row rendered in Settings ▸ Extensions")

	semStr := p.Semantics().String()
	if strings.Contains(semStr, secretVal) {
		t.Fatalf("secret setting value leaked in plaintext into UI semantics:\n%s", semStr)
	}
	settingsJSON, err := os.ReadFile(filepath.Join(h.DataDir, "extension-data", "f23-secret-mask", "settings.json"))
	if err == nil && strings.Contains(string(settingsJSON), secretVal) {
		t.Fatalf("secret setting value leaked into settings.json: %s", string(settingsJSON))
	}
}

func TestF23_T2_DeveloperModeOffBlocksBinaryExtension(t *testing.T) {
	mgr, h := newTestManager(t, WithDeveloperMode(false))
	spawnMarker := filepath.Join(t.TempDir(), "should_not_exist.txt")
	binManifest := e2e3UIManifestBinary("f23-dev-blocked", "Blocked Binary", e2e3UIMinimalViewTOML)
	extDir := filepath.Join(t.TempDir(), "f23-dev-blocked")
	writeExtFixture(t, extDir, binManifest, map[string]string{
		"bin/ext": fmt.Sprintf("#!/bin/sh\ntouch %q\nexit 0\n", spawnMarker),
	})
	if _, err := mgr.InstallLocal(extDir); err != nil {
		t.Fatalf("InstallLocal: %v", err)
	}

	err := mgr.AllowAndBuild("f23-dev-blocked")
	if err == nil {
		t.Fatal("expected AllowAndBuild on binary extension to fail when DeveloperMode == false")
	}
	if _, statErr := os.Stat(spawnMarker); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("binary process should NOT have spawned when DeveloperMode is off (stat err=%v)", statErr)
	}

	m, p := e2e3UINewProbe(t, h.DataDir, mgr, ggui.Sz(1280, 840), nil)
	defer p.Close()
	m.Go(ui.ExtensionPage("f23-dev-blocked", "main"))
	e2e3UISettle(t, p, func() bool {
		sem := p.Semantics()
		return e2e3UISemHasText(sem, "Developer") || e2e3UISemHasText(sem, "Blocked Binary")
	}, "DeveloperMode-blocked binary extension surfaced in UI")
}

func TestF23_T2_UninstallPurgeFalseVsTrue(t *testing.T) {
	const src = `package main

import (
	"context"
	"os"
	"path/filepath"
	"github.com/ironpark/gvx-desktop/pkg/extsdk"
)

func main() {
	app := extsdk.New()
	app.View("main", extsdk.View{
		Render: func(ctx *extsdk.ViewContext, u *extsdk.UI) extsdk.Node {
			return u.Column(u.Text("PurgeTestView"))
		},
	})
	app.Command("seed", func(ctx *extsdk.CommandContext) error {
		if err := ctx.Host.StoragePut(ctx, "k", "v"); err != nil {
			return err
		}
		if err := ctx.Host.SecretPut(ctx, "api_key", "secret-123"); err != nil {
			return err
		}
		return os.WriteFile(filepath.Join(os.Getenv("GVX_EXT_DATA_DIR"), "artifact.txt"), []byte("keep-or-purge"), 0o600)
	})
	_ = app.Run(context.Background())
}
`
	seedTOML := e2e3UIMinimalViewTOML + `
[[contributes.command]]
id = "seed"
title = "Seed"
`
	mgr, h := newTestManager(t, WithDeveloperMode(true))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := mgr.Start(ctx); err != nil {
		t.Fatalf("mgr.Start: %v", err)
	}
	e2e3UIInstallLocalBinary(t, mgr, "f23-keep", src, seedTOML, ext.PermStorage, ext.PermSecrets)
	e2e3UIInstallLocalBinary(t, mgr, "f23-purge", src, seedTOML, ext.PermStorage, ext.PermSecrets)

	if err := mgr.RunCommand("f23-keep", "seed", "en"); err != nil {
		t.Fatalf("seed f23-keep: %v", err)
	}
	if err := mgr.RunCommand("f23-purge", "seed", "en"); err != nil {
		t.Fatalf("seed f23-purge: %v", err)
	}

	// Uninstall(purge=false) removes extensions/<id> but preserves extension-data/<id>.
	if err := mgr.Uninstall("f23-keep", false); err != nil {
		t.Fatalf("Uninstall(f23-keep, false): %v", err)
	}
	if _, err := os.Stat(filepath.Join(h.DataDir, "extensions", "f23-keep")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("expected extensions/f23-keep removed, err=%v", err)
	}
	if _, err := os.Stat(filepath.Join(h.DataDir, "extension-data", "f23-keep", "files", "artifact.txt")); err != nil {
		t.Fatalf("expected extension-data/f23-keep/files/artifact.txt preserved: %v", err)
	}
	if _, err := os.Stat(filepath.Join(h.DataDir, "extension-data", "f23-keep", "storage.json")); err != nil {
		t.Fatalf("expected extension-data/f23-keep/storage.json preserved: %v", err)
	}

	// Uninstall(purge=true) removes extensions/<id>, extension-data/<id>, AND keychain secrets.
	if err := mgr.Uninstall("f23-purge", true); err != nil {
		t.Fatalf("Uninstall(f23-purge, true): %v", err)
	}
	if _, err := os.Stat(filepath.Join(h.DataDir, "extensions", "f23-purge")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("expected extensions/f23-purge removed, err=%v", err)
	}
	if _, err := os.Stat(filepath.Join(h.DataDir, "extension-data", "f23-purge")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("expected extension-data/f23-purge purged, err=%v", err)
	}
	purgeSvc := ext.KeychainService(h.DataDir, "f23-purge")
	for _, entry := range h.Keychain.Entries() {
		if strings.HasPrefix(entry[0], purgeSvc) {
			t.Fatalf("keychain entry %v survived Uninstall(purge=true)", entry)
		}
	}
}

// =============================================================================
// Feature 24: main.go Shutdown Hook (CloseExtensions) (5 Tier 1 + 5 Tier 2)
// =============================================================================

func TestF24_T1_CloseExtensionsSendsShutdownToRunningExts(t *testing.T) {
	const src = `package main

import (
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
)

func main() {
	listen := os.Getenv("GVX_EXT_LISTEN")
	dataDir := os.Getenv("GVX_EXT_DATA_DIR")
	var ln net.Listener
	var err error
	if strings.HasPrefix(listen, "unix://") {
		p := strings.TrimPrefix(listen, "unix://")
		_ = os.Remove(p)
		ln, err = net.Listen("unix", p)
	} else {
		ln, err = net.Listen("tcp", listen)
	}
	if err != nil {
		os.Exit(2)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/health", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(` + "`" + `{"status":"ok","version":"0.1.0"}` + "`" + `))
	})
	mux.HandleFunc("/v1/shutdown", func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			_ = os.WriteFile(filepath.Join(dataDir, "shutdown_post.txt"), []byte(fmt.Sprintf("%s", r.Method)), 0o600)
		}
		w.WriteHeader(http.StatusOK)
		go func() { os.Exit(0) }()
	})
	srv := &http.Server{Handler: mux}
	go func() {
		sig := make(chan os.Signal, 1)
		signal.Notify(sig, syscall.SIGTERM, os.Interrupt)
		<-sig
		_ = srv.Close()
	}()
	_ = srv.Serve(ln)
}
`
	mgr, h := newTestManager(t, WithDeveloperMode(true))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := mgr.Start(ctx); err != nil {
		t.Fatalf("mgr.Start: %v", err)
	}
	e2e3UIInstallLocalBinary(t, mgr, "f24-shut", src, e2e3UIMinimalViewTOML)

	m, p := e2e3UINewProbe(t, h.DataDir, mgr, ggui.Sz(1000, 700), nil)
	defer p.Close()
	p.Frame()

	m.CloseExtensions(8 * time.Second)

	got, err := os.ReadFile(filepath.Join(h.DataDir, "extension-data", "f24-shut", "files", "shutdown_post.txt"))
	if err != nil || string(got) != "POST" {
		t.Fatalf("expected POST /v1/shutdown marker, got (%q, %v)", string(got), err)
	}
}

func TestF24_T1_CloseExtensionsStopsBinaryAndSandboxRuntimes(t *testing.T) {
	const src = `package main

import (
	"context"
	"github.com/ironpark/gvx-desktop/pkg/extsdk"
)

func main() {
	app := extsdk.New()
	app.View("main", extsdk.View{
		Render: func(ctx *extsdk.ViewContext, u *extsdk.UI) extsdk.Node {
			return u.Column(u.Text("ShutdownRuntimeBody"))
		},
	})
	_ = app.Run(context.Background())
}
`
	mgr, h := newTestManager(t, WithDeveloperMode(true))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := mgr.Start(ctx); err != nil {
		t.Fatalf("mgr.Start: %v", err)
	}
	e2e3UIInstallLocalBinary(t, mgr, "f24-bin", src, e2e3UIMinimalViewTOML)

	m, p := e2e3UINewProbe(t, h.DataDir, mgr, ggui.Sz(1000, 700), nil)
	defer p.Close()
	p.Frame()

	m.CloseExtensions(8 * time.Second)
	if info, ok := e2e3UIFindExtInfo(mgr, "f24-bin"); ok && info.State.IsActive() {
		t.Fatalf("expected binary extension stopped after CloseExtensions, got state=%q", info.State)
	}

	// Sandbox runtime verification gated on rungv availability.
	binPath, _, _ := requireRungv(t)
	sbxDir := newShortTempDataDir(t)
	sup := &sandbox.Supervisor{
		Client:  func() *rungv.Client { return &rungv.Client{Path: binPath} },
		DataDir: sbxDir,
		Launch: func(spec sandbox.Spec) (sandbox.Spec, func(), error) {
			if spec.Image == "alpine:3.20" {
				spec.Entrypoint = "/data/ext-bin"
				spec.SetEntrypoint = true
				spec.Command = ""
			}
			return spec, nil, nil
		},
	}
	defer sup.StopAll(5 * time.Second)

	linuxBin := e2e3UIBuildLinuxSDKBinary(t, src)
	linuxBytes, err := os.ReadFile(linuxBin)
	if err != nil {
		t.Fatalf("read linux binary: %v", err)
	}

	sbxMgr, _ := newTestManager(t, WithDataDir(sbxDir), WithSupervisor(sup))
	sbxCtx, sbxCancel := context.WithCancel(context.Background())
	defer sbxCancel()
	if err := sbxMgr.Start(sbxCtx); err != nil {
		t.Fatalf("sbxMgr.Start: %v", err)
	}
	sbxManifest := `schema_version = 1
id = "f24-sbx"
version = "0.1.0"
publisher = "acme"
name = "Shutdown Sandbox Ext"
description = "E2E sandbox shutdown fixture"
icon = "box"

[runtime]
type = "sandbox"
image = "alpine:3.20"
port = 18900

[[contributes.view]]
id = "main"
title = "Main"
icon = "box"
`
	sbxSrcDir := createLocalExtensionDir(t, t.TempDir(), "f24-sbx", sbxManifest, nil)
	if err := ext.EnsureDir(ext.FilesDir(sbxDir, "f24-sbx")); err != nil {
		t.Fatalf("EnsureDir FilesDir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(ext.FilesDir(sbxDir, "f24-sbx"), "ext-bin"), linuxBytes, 0o755); err != nil {
		t.Fatalf("stage linux binary: %v", err)
	}
	if _, err := sbxMgr.InstallLocal(sbxSrcDir); err != nil {
		t.Fatalf("InstallLocal sbx: %v", err)
	}
	if err := sbxMgr.AllowAndBuild("f24-sbx"); err != nil {
		t.Fatalf("AllowAndBuild sbx: %v", err)
	}
	sbxInfo, ok := e2e3UIFindExtInfo(sbxMgr, "f24-sbx")
	if !ok || sbxInfo.State != ext.StateRunning || sbxInfo.Unsandboxed {
		t.Fatalf("expected f24-sbx running and sandboxed, got ok=%v info=%+v", ok, sbxInfo)
	}

	sbxM, sbxP := e2e3UINewProbe(t, sbxDir, sbxMgr, ggui.Sz(1000, 700), nil)
	defer sbxP.Close()
	sbxP.Frame()
	sbxM.CloseExtensions(8 * time.Second)

	if afterInfo, ok := e2e3UIFindExtInfo(sbxMgr, "f24-sbx"); !ok || afterInfo.State.IsActive() {
		t.Fatalf("expected sandbox extension stopped after CloseExtensions, got ok=%v info=%+v", ok, afterInfo)
	}
	if running := sup.Running(); len(running) != 0 {
		t.Fatalf("expected 0 running sandboxes after CloseExtensions, got %v", running)
	}
}

func TestF24_T1_CloseExtensionsCleansEphemeralSandboxes(t *testing.T) {
	const src = `package main

import (
	"context"
	"github.com/ironpark/gvx-desktop/pkg/extsdk"
)

func main() {
	app := extsdk.New()
	app.View("main", extsdk.View{
		Render: func(ctx *extsdk.ViewContext, u *extsdk.UI) extsdk.Node {
			return u.Column(u.Text("EphemeralOwner"))
		},
	})
	app.Command("spawn-ephemeral", func(ctx *extsdk.CommandContext) error {
		start := false
		_, err := ctx.Host.SandboxCreate(ctx, extsdk.CreateSandboxRequest{
			Name:      "eph-job-1",
			Image:     "alpine:3.20",
			Ephemeral: true,
			Start:     &start,
		})
		return err
	})
	_ = app.Run(context.Background())
}
`
	extraTOML := e2e3UIMinimalViewTOML + `
[[contributes.command]]
id = "spawn-ephemeral"
title = "Spawn Ephemeral"
`
	dataDir := newShortTempDataDir(t)
	sup := &sandbox.Supervisor{DataDir: dataDir}
	mgr, _ := newTestManager(t, WithDataDir(dataDir), WithDeveloperMode(true), WithSupervisor(sup))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := mgr.Start(ctx); err != nil {
		t.Fatalf("mgr.Start: %v", err)
	}
	e2e3UIInstallLocalBinary(t, mgr, "f24-eph", src, extraTOML, ext.PermSandboxes)

	if err := mgr.RunCommand("f24-eph", "spawn-ephemeral", "en"); err != nil {
		t.Fatalf("RunCommand(spawn-ephemeral): %v", err)
	}

	st := newTestStore(t, dataDir)
	beforeSpecs, err := st.Sandboxes()
	if err != nil {
		t.Fatalf("st.Sandboxes before close: %v", err)
	}
	if len(beforeSpecs) != 1 {
		t.Fatalf("expected 1 owned ephemeral sandbox before CloseExtensions, got %d", len(beforeSpecs))
	}

	m, p := e2e3UINewProbe(t, dataDir, mgr, ggui.Sz(1000, 700), nil)
	defer p.Close()
	p.Frame()

	m.CloseExtensions(8 * time.Second)

	afterSpecs, err := st.Sandboxes()
	if err != nil {
		t.Fatalf("st.Sandboxes after close: %v", err)
	}
	if len(afterSpecs) != 0 {
		t.Fatalf("expected ephemeral sandbox cleaned up by CloseExtensions, remaining=%+v", afterSpecs)
	}
}

func TestF24_T1_MainGoShutdownHookOrderingAST(t *testing.T) {
	mainPath := filepath.Join(repoRoot(t), "main.go")
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, mainPath, nil, 0)
	if err != nil {
		t.Fatalf("parse main.go: %v", err)
	}

	var runDecl *ast.FuncDecl
	for _, decl := range file.Decls {
		if fd, ok := decl.(*ast.FuncDecl); ok && fd.Name.Name == "run" {
			runDecl = fd
			break
		}
	}
	if runDecl == nil || runDecl.Body == nil {
		t.Fatal("could not locate func run() in main.go")
	}

	callPos := map[string]token.Pos{}
	ast.Inspect(runDecl.Body, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		switch sel.Sel.Name {
		case "CloseTerminals", "CloseForwards", "CloseExtensions", "StopAll", "CloseSandboxEngine":
			if _, exists := callPos[sel.Sel.Name]; !exists {
				callPos[sel.Sel.Name] = call.Pos()
			}
		}
		return true
	})

	for _, req := range []string{"CloseTerminals", "CloseForwards", "CloseExtensions", "StopAll", "CloseSandboxEngine"} {
		if _, ok := callPos[req]; !ok {
			t.Fatalf("main.go run() is missing shutdown call %s (found=%v)", req, callPos)
		}
	}
	if callPos["CloseExtensions"] <= callPos["CloseTerminals"] {
		t.Fatalf("CloseExtensions (line %d) must run after CloseTerminals (line %d)",
			fset.Position(callPos["CloseExtensions"]).Line, fset.Position(callPos["CloseTerminals"]).Line)
	}
	if callPos["CloseExtensions"] <= callPos["CloseForwards"] {
		t.Fatalf("CloseExtensions (line %d) must run after CloseForwards (line %d)",
			fset.Position(callPos["CloseExtensions"]).Line, fset.Position(callPos["CloseForwards"]).Line)
	}
	if callPos["CloseExtensions"] >= callPos["StopAll"] {
		t.Fatalf("CloseExtensions (line %d) must run before Supervisor().StopAll (line %d)",
			fset.Position(callPos["CloseExtensions"]).Line, fset.Position(callPos["StopAll"]).Line)
	}
	if callPos["CloseExtensions"] >= callPos["CloseSandboxEngine"] {
		t.Fatalf("CloseExtensions (line %d) must run before CloseSandboxEngine (line %d)",
			fset.Position(callPos["CloseExtensions"]).Line, fset.Position(callPos["CloseSandboxEngine"]).Line)
	}
}

func TestF24_T1_CloseExtensionsIdempotentAndNilSafe(t *testing.T) {
	// 1. Model with nil extension manager.
	dir1 := newTempDataDir(t)
	mNil, pNil := e2e3UINewProbe(t, dir1, nil, ggui.Sz(800, 600), nil)
	defer pNil.Close()
	pNil.Frame()
	mNil.SetExtensionManager(nil)
	mNil.CloseExtensions(8 * time.Second)
	mNil.CloseExtensions(8 * time.Second)

	// 2. Model with empty extension manager (0 installed extensions), called twice.
	mgr, h := newTestManager(t)
	mEmpty, pEmpty := e2e3UINewProbe(t, h.DataDir, mgr, ggui.Sz(800, 600), nil)
	defer pEmpty.Close()
	pEmpty.Frame()
	mEmpty.CloseExtensions(8 * time.Second)
	mEmpty.CloseExtensions(8 * time.Second)
}

func TestF24_T2_HungExtensionForceKilledWithin8sBudget(t *testing.T) {
	const hungSrc = `package main

import (
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
)

func main() {
	// Deliberately trap/ignore SIGTERM and hang on POST /v1/shutdown so host must SIGKILL.
	signal.Ignore(syscall.SIGTERM, os.Interrupt)
	listen := os.Getenv("GVX_EXT_LISTEN")
	var ln net.Listener
	var err error
	if strings.HasPrefix(listen, "unix://") {
		p := strings.TrimPrefix(listen, "unix://")
		_ = os.Remove(p)
		ln, err = net.Listen("unix", p)
	} else {
		ln, err = net.Listen("tcp", listen)
	}
	if err != nil {
		os.Exit(2)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/health", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(` + "`" + `{"status":"ok","version":"0.1.0"}` + "`" + `))
	})
	mux.HandleFunc("/v1/shutdown", func(w http.ResponseWriter, r *http.Request) {
		select {} // deliberate hang
	})
	_ = (&http.Server{Handler: mux}).Serve(ln)
}
`
	mgr, h := newTestManager(t, WithDeveloperMode(true))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := mgr.Start(ctx); err != nil {
		t.Fatalf("mgr.Start: %v", err)
	}
	e2e3UIInstallLocalBinary(t, mgr, "f24-hung", hungSrc, e2e3UIMinimalViewTOML)

	m, p := e2e3UINewProbe(t, h.DataDir, mgr, ggui.Sz(1000, 700), nil)
	defer p.Close()
	p.Frame()

	start := time.Now()
	m.CloseExtensions(8 * time.Second)
	elapsed := time.Since(start)
	if elapsed > 8500*time.Millisecond {
		t.Fatalf("hung extension CloseExtensions(8s) took %v (> 8.5s budget)", elapsed)
	}
	if info, ok := e2e3UIFindExtInfo(mgr, "f24-hung"); ok && info.State.IsActive() {
		t.Fatalf("hung extension still active after CloseExtensions: %+v", info)
	}
}

func TestF24_T2_AppStoppingSSEEventEmittedBeforeTeardown(t *testing.T) {
	const src = `package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"github.com/ironpark/gvx-desktop/pkg/extsdk"
)

func main() {
	app := extsdk.New()
	app.View("main", extsdk.View{
		Render: func(ctx *extsdk.ViewContext, u *extsdk.UI) extsdk.Node {
			return u.Column(u.Text("StoppingObserver"))
		},
	})
	app.OnEvent(func(ctx *extsdk.Context, ev extsdk.HostEvent) {
		if ev.Type == "app.stopping" {
			f, err := os.OpenFile(filepath.Join(os.Getenv("GVX_EXT_DATA_DIR"), "events_order.log"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
			if err == nil {
				_, _ = f.WriteString("app.stopping\n")
				_ = f.Close()
			}
		}
	})
	_ = app.Run(context.Background())
	_ = strings.TrimSpace("")
}
`
	mgr, h := newTestManager(t, WithDeveloperMode(true))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := mgr.Start(ctx); err != nil {
		t.Fatalf("mgr.Start: %v", err)
	}
	e2e3UIInstallLocalBinary(t, mgr, "f24-stopping-ev", src, e2e3UIMinimalViewTOML)

	m, p := e2e3UINewProbe(t, h.DataDir, mgr, ggui.Sz(1000, 700), nil)
	defer p.Close()
	p.Frame()

	// Brief settle so extension's GET /v1/events SSE subscription is established.
	time.Sleep(150 * time.Millisecond)

	m.CloseExtensions(8 * time.Second)

	logPath := filepath.Join(h.DataDir, "extension-data", "f24-stopping-ev", "files", "events_order.log")
	raw, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("expected app.stopping SSE event recorded before teardown: %v", err)
	}
	if !strings.Contains(string(raw), "app.stopping") {
		t.Fatalf("events_order.log missing app.stopping: %q", string(raw))
	}
}

func TestF24_T2_NonEphemeralSandboxPreservedVsEphemeralCleaned(t *testing.T) {
	const src = `package main

import (
	"context"
	"github.com/ironpark/gvx-desktop/pkg/extsdk"
)

func main() {
	app := extsdk.New()
	app.View("main", extsdk.View{
		Render: func(ctx *extsdk.ViewContext, u *extsdk.UI) extsdk.Node {
			return u.Column(u.Text("MixedSandboxes"))
		},
	})
	app.Command("seed-both", func(ctx *extsdk.CommandContext) error {
		start := false
		if _, err := ctx.Host.SandboxCreate(ctx, extsdk.CreateSandboxRequest{
			Name:      "eph-sbx",
			Image:     "alpine:3.20",
			Ephemeral: true,
			Start:     &start,
		}); err != nil {
			return err
		}
		_, err := ctx.Host.SandboxCreate(ctx, extsdk.CreateSandboxRequest{
			Name:      "persist-sbx",
			Image:     "alpine:3.20",
			Ephemeral: false,
			Start:     &start,
		})
		return err
	})
	_ = app.Run(context.Background())
}
`
	extraTOML := e2e3UIMinimalViewTOML + `
[[contributes.command]]
id = "seed-both"
title = "Seed Both Sandboxes"
`
	dataDir := newShortTempDataDir(t)
	sup := &sandbox.Supervisor{DataDir: dataDir}
	mgr, _ := newTestManager(t, WithDataDir(dataDir), WithDeveloperMode(true), WithSupervisor(sup))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := mgr.Start(ctx); err != nil {
		t.Fatalf("mgr.Start: %v", err)
	}
	e2e3UIInstallLocalBinary(t, mgr, "f24-mixed-sbx", src, extraTOML, ext.PermSandboxes)
	if err := mgr.RunCommand("f24-mixed-sbx", "seed-both", "en"); err != nil {
		t.Fatalf("RunCommand(seed-both): %v", err)
	}

	m, p := e2e3UINewProbe(t, dataDir, mgr, ggui.Sz(1000, 700), nil)
	defer p.Close()
	p.Frame()

	m.CloseExtensions(8 * time.Second)

	specs, err := newTestStore(t, dataDir).Sandboxes()
	if err != nil {
		t.Fatalf("Sandboxes(): %v", err)
	}
	var names []string
	for _, sp := range specs {
		names = append(names, sp.Name)
	}
	if slices.Contains(names, "eph-sbx") {
		t.Fatalf("ephemeral sandbox eph-sbx should be removed on CloseExtensions, got %v", names)
	}
	if !slices.Contains(names, "persist-sbx") {
		t.Fatalf("non-ephemeral sandbox persist-sbx should be preserved on CloseExtensions, got %v", names)
	}
}

func TestF24_T2_ConcurrentCloseExtensionsAndAPIRace(t *testing.T) {
	const src = `package main

import (
	"context"
	"sync/atomic"
	"github.com/ironpark/gvx-desktop/pkg/extsdk"
)

func main() {
	app := extsdk.New()
	var n atomic.Int64
	app.View("main", extsdk.View{
		Render: func(ctx *extsdk.ViewContext, u *extsdk.UI) extsdk.Node {
			return u.Column(
				u.Button("Ping", extsdk.Key("ping"), extsdk.OnTap("ping")),
			)
		},
		On: extsdk.Handlers{
			"ping": func(ctx *extsdk.EventContext) error {
				n.Add(1)
				app.NotifyChanged("main")
				return nil
			},
		},
	})
	app.Command("tick", func(ctx *extsdk.CommandContext) error {
		n.Add(1)
		return nil
	})
	_ = app.Run(context.Background())
}
`
	extraTOML := e2e3UIMinimalViewTOML + `
[[contributes.command]]
id = "tick"
title = "Tick"
`
	mgr, h := newTestManager(t, WithDeveloperMode(true))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := mgr.Start(ctx); err != nil {
		t.Fatalf("mgr.Start: %v", err)
	}
	e2e3UIInstallLocalBinary(t, mgr, "f24-race", src, extraTOML)

	m, p := e2e3UINewProbe(t, h.DataDir, mgr, ggui.Sz(1000, 700), nil)
	defer p.Close()
	p.Frame()

	var wg sync.WaitGroup
	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 10 {
				_ = mgr.RunCommand("f24-race", "tick", "en")
				_ = mgr.List()
			}
		}()
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		_ = mgr.SendViewEvent("f24-race", "main", uiblock.Event{
			Block: "main",
			Node:  "/ping",
			Kind:  uiblock.EventTap,
		})
	}()
	// Concurrently invoke CloseExtensions while API calls are in flight.
	m.CloseExtensions(8 * time.Second)
	wg.Wait()
}

func TestF24_T2_CloseExtensionsWhenAlreadyStoppedReturnsFast(t *testing.T) {
	const src = `package main

import (
	"context"
	"github.com/ironpark/gvx-desktop/pkg/extsdk"
)

func main() {
	app := extsdk.New()
	app.View("main", extsdk.View{
		Render: func(ctx *extsdk.ViewContext, u *extsdk.UI) extsdk.Node {
			return u.Column(u.Text("FastReturnBody"))
		},
	})
	_ = app.Run(context.Background())
}
`
	mgr, h := newTestManager(t, WithDeveloperMode(true))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := mgr.Start(ctx); err != nil {
		t.Fatalf("mgr.Start: %v", err)
	}
	e2e3UIInstallLocalBinary(t, mgr, "f24-fast", src, e2e3UIMinimalViewTOML)
	if err := mgr.Disable("f24-fast"); err != nil {
		t.Fatalf("Disable: %v", err)
	}

	m, p := e2e3UINewProbe(t, h.DataDir, mgr, ggui.Sz(1000, 700), nil)
	defer p.Close()
	p.Frame()

	start := time.Now()
	m.CloseExtensions(8 * time.Second)
	if elapsed := time.Since(start); elapsed >= 200*time.Millisecond {
		t.Fatalf("CloseExtensions when already stopped took %v, want < 200ms", elapsed)
	}

	start2 := time.Now()
	m.CloseExtensions(8 * time.Second)
	if elapsed := time.Since(start2); elapsed >= 200*time.Millisecond {
		t.Fatalf("repeated CloseExtensions took %v, want < 200ms", elapsed)
	}
}
