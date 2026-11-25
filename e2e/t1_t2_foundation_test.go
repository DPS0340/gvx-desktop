//go:build e2e

package e2e

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ironpark/gvx-desktop/internal/keychain"
	"github.com/ironpark/gvx-desktop/internal/uiblock"
	"github.com/ironpark/gvx-desktop/pkg/extsdk"
)

// =============================================================================
// Feature 1: Git Branch feat/ext & Pure-Go go.mod Dependencies
// =============================================================================

func TestF1_T1_GitBranchFeatExt(t *testing.T) {
	root := repoRoot(t)
	cmd := exec.Command("git", "branch", "--show-current")
	cmd.Dir = root
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git branch --show-current failed: %v\n%s", err, string(out))
	}
	if got := strings.TrimSpace(string(out)); got != "feat/ext" {
		t.Fatalf("current git branch = %q, want %q", got, "feat/ext")
	}
}

func TestF1_T1_CGOZeroBuildAll(t *testing.T) {
	root := repoRoot(t)
	if out, err := runGo(t, root, nil, "build", "./..."); err != nil {
		t.Fatalf("CGO_ENABLED=0 go build ./... failed: %v\n%s", err, out)
	}
}

func TestF1_T1_BurntSushiTomlPresent(t *testing.T) {
	root := repoRoot(t)
	out, err := runGo(t, root, nil, "list", "-m", "-f", "{{.Path}} {{.Version}} {{.Indirect}}", "github.com/BurntSushi/toml")
	if err != nil {
		t.Fatalf("go list -m github.com/BurntSushi/toml failed: %v\n%s", err, out)
	}
	fields := strings.Fields(strings.TrimSpace(out))
	if len(fields) < 3 || fields[0] != "github.com/BurntSushi/toml" {
		t.Fatalf("unexpected go list -m output for github.com/BurntSushi/toml: %q", out)
	}
	if fields[2] != "false" {
		t.Fatalf("github.com/BurntSushi/toml Indirect = %q, want \"false\" (must be a direct dependency once internal/ext imports it)", fields[2])
	}
	modBytes, err := os.ReadFile(filepath.Join(root, "go.mod"))
	if err != nil {
		t.Fatalf("read go.mod: %v", err)
	}
	foundDirect := false
	for _, line := range strings.Split(string(modBytes), "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.Contains(trimmed, "github.com/BurntSushi/toml") {
			if strings.Contains(trimmed, "// indirect") {
				t.Fatalf("go.mod lists github.com/BurntSushi/toml as indirect: %q", trimmed)
			}
			foundDirect = true
		}
	}
	if !foundDirect {
		t.Fatalf("go.mod does not list github.com/BurntSushi/toml as a direct dependency")
	}
}

func TestF1_T1_KeyringPureGoDepsPresent(t *testing.T) {
	root := repoRoot(t)
	modBytes, err := os.ReadFile(filepath.Join(root, "go.mod"))
	if err != nil {
		t.Fatalf("read go.mod: %v", err)
	}
	modText := string(modBytes)
	for _, dep := range []string{
		"github.com/zalando/go-keyring",
		"github.com/godbus/dbus/v5",
		"github.com/ebitengine/purego",
	} {
		if !strings.Contains(modText, dep) {
			t.Fatalf("go.mod missing required pure-Go dependency %q", dep)
		}
	}
	if out, err := runGo(t, root, nil, "list", "./internal/keychain"); err != nil {
		t.Fatalf("CGO_ENABLED=0 go list ./internal/keychain failed: %v\n%s", err, out)
	}
}

func TestF1_T1_ExtsdkStdlibOnlyImports(t *testing.T) {
	root := repoRoot(t)
	out, err := runGo(t, root, nil, "list", "-f", "{{range .Imports}}{{.}}{{\"\\n\"}}{{end}}", "./pkg/extsdk/...")
	if err != nil {
		t.Fatalf("go list ./pkg/extsdk/... failed: %v\n%s", err, out)
	}
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		imp := strings.TrimSpace(line)
		if imp == "" {
			continue
		}
		firstSeg, _, _ := strings.Cut(imp, "/")
		if strings.Contains(firstSeg, ".") {
			t.Fatalf("pkg/extsdk imports non-stdlib package %q", imp)
		}
		if strings.Contains(imp, "internal/") || strings.Contains(imp, "ggui") {
			t.Fatalf("pkg/extsdk imports forbidden package %q", imp)
		}
	}
}

// collectExtensionGoFiles recursively walks all extension packages and globs
// extension-specific internal/ui files.
func collectExtensionGoFiles(t *testing.T, root string, includeTests bool) []string {
	t.Helper()
	var goFiles []string
	for _, dir := range []string{
		filepath.Join(root, "internal", "ext"),
		filepath.Join(root, "internal", "keychain"),
		filepath.Join(root, "internal", "uiblock"),
		filepath.Join(root, "pkg", "extsdk"),
	} {
		if _, err := os.Stat(dir); err != nil {
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			t.Fatalf("stat %s: %v", dir, err)
		}
		err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, walkErr error) error {
			if walkErr != nil {
				return walkErr
			}
			if d.IsDir() || !strings.HasSuffix(d.Name(), ".go") {
				return nil
			}
			if !includeTests && strings.HasSuffix(d.Name(), "_test.go") {
				return nil
			}
			goFiles = append(goFiles, path)
			return nil
		})
		if err != nil {
			t.Fatalf("WalkDir(%s): %v", dir, err)
		}
	}
	for _, pattern := range []string{
		filepath.Join(root, "internal", "ui", "ext_*.go"),
		filepath.Join(root, "internal", "ui", "page_ext*.go"),
		filepath.Join(root, "internal", "ui", "model_ext*.go"),
	} {
		matches, err := filepath.Glob(pattern)
		if err != nil {
			t.Fatalf("Glob(%s): %v", pattern, err)
		}
		for _, m := range matches {
			if !includeTests && strings.HasSuffix(m, "_test.go") {
				continue
			}
			goFiles = append(goFiles, m)
		}
	}
	return goFiles
}

// isDotGvxStorageLiteral reports whether val refers to a ~/.gvx or .gvx/...
// storage path while permitting reverse-DNS identifiers like "io.gvx.extension".
func isDotGvxStorageLiteral(val string) bool {
	return val == ".gvx" ||
		strings.Contains(val, "/.gvx") ||
		strings.Contains(val, `\.gvx`) ||
		strings.HasPrefix(val, ".gvx/") ||
		strings.HasPrefix(val, `.gvx\`)
}

func TestF1_T2_NoDotGvxStorageInExtensionPkgs(t *testing.T) {
	root := repoRoot(t)
	// Intentionally scan ONLY extension packages/files (never internal/rungv,
	// which legitimately locates $HOME/.gvx/bin/rungv).
	goFiles := collectExtensionGoFiles(t, root, false)
	if len(goFiles) == 0 {
		t.Fatalf("expected at least one landed extension package Go file to scan")
	}

	fset := token.NewFileSet()
	for _, path := range goFiles {
		f, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", path, err)
		}
		ast.Inspect(f, func(n ast.Node) bool {
			lit, ok := n.(*ast.BasicLit)
			if !ok || lit.Kind != token.STRING {
				return true
			}
			val, err := strconv.Unquote(lit.Value)
			if err != nil {
				return true
			}
			if isDotGvxStorageLiteral(val) {
				pos := fset.Position(lit.Pos())
				t.Fatalf("extension package %s:%d references .gvx storage path literal %q", path, pos.Line, val)
			}
			return true
		})
	}
}

func TestF1_T2_NoProtobufInUIBlockOrSDK(t *testing.T) {
	root := repoRoot(t)
	targets := []string{"./internal/uiblock/..."}
	if _, err := os.Stat(filepath.Join(root, "pkg", "extsdk")); err == nil {
		targets = append(targets, "./pkg/extsdk/...")
	}
	args := append([]string{"list", "-deps"}, targets...)
	out, err := runGo(t, root, nil, args...)
	if err != nil {
		t.Fatalf("go list -deps failed: %v\n%s", err, out)
	}
	for _, line := range strings.Split(out, "\n") {
		if strings.Contains(strings.ToLower(line), "protobuf") {
			t.Fatalf("unexpected protobuf dependency in uiblock/extsdk: %q", line)
		}
	}
}

func TestF1_T2_NoCGODirectivesInExtPackages(t *testing.T) {
	root := repoRoot(t)
	goFiles := collectExtensionGoFiles(t, root, true)
	if len(goFiles) == 0 {
		t.Fatalf("expected extension package Go files to scan")
	}
	fset := token.NewFileSet()
	for _, path := range goFiles {
		f, err := parser.ParseFile(fset, path, nil, parser.ParseComments)
		if err != nil {
			t.Fatalf("parse %s: %v", path, err)
		}
		for _, imp := range f.Imports {
			if imp.Path.Value == `"C"` {
				t.Fatalf("%s imports \"C\" (CGO forbidden)", path)
			}
		}
		for _, cg := range f.Comments {
			if strings.Contains(cg.Text(), "#cgo") {
				t.Fatalf("%s contains #cgo directive (CGO forbidden)", path)
			}
		}
	}
}

func TestF1_T2_NoOutOfScopePhase3Or4Markers(t *testing.T) {
	root := repoRoot(t)
	forbiddenPaths := []string{
		filepath.Join(root, "gvx-ext-github-actions"),
		filepath.Join(root, "internal", "act"),
		filepath.Join(root, "internal", "engineb"),
		filepath.Join(root, "internal", "warmsnapshot"),
	}
	for _, p := range forbiddenPaths {
		if _, err := os.Stat(p); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("out-of-scope Phase 3/4 directory exists: %s", p)
		}
	}
}

func TestF1_T2_GoModVerifyClean(t *testing.T) {
	root := repoRoot(t)
	if out, err := runGo(t, root, nil, "mod", "verify"); err != nil {
		t.Fatalf("go mod verify failed: %v\n%s", err, out)
	}
	if out, err := runGo(t, root, nil, "vet", "./internal/keychain/...", "./internal/uiblock/...", "./pkg/extsdk/...", "./internal/ext/..."); err != nil {
		t.Fatalf("go vet failed: %v\n%s", err, out)
	}
}

// =============================================================================
// Feature 2: Pure-Go OS Keychain (internal/keychain)
// =============================================================================

func TestF2_T1_FakeSetGetRoundtrip(t *testing.T) {
	fk := newFakeKeychain(t)
	if fk.Name() == "" {
		t.Fatal("Fake.Name() returned empty string")
	}
	cases := [][3]string{
		{"gvx-desktop extension alpha (deadbeef)", "api_token", "tok_12345"},
		{"gvx-desktop extension alpha (deadbeef)", "unicode_secret", "비밀번호-🔐-日本語"},
		{"gvx-desktop extension beta (cafebabe)", "empty_val", ""},
	}
	for _, c := range cases {
		if err := fk.Set(c[0], c[1], c[2]); err != nil {
			t.Fatalf("Set(%q, %q): %v", c[0], c[1], err)
		}
		got, err := fk.Get(c[0], c[1])
		if err != nil {
			t.Fatalf("Get(%q, %q): %v", c[0], c[1], err)
		}
		if got != c[2] {
			t.Fatalf("Get(%q, %q) = %q, want %q", c[0], c[1], got, c[2])
		}
	}
}

func TestF2_T1_FakeDeleteAndEntries(t *testing.T) {
	fk := newFakeKeychain(t)
	_ = fk.Set("svc-b", "k2", "v2")
	_ = fk.Set("svc-a", "k1", "v1")
	_ = fk.Set("svc-a", "k0", "v0")

	wantBefore := [][2]string{{"svc-a", "k0"}, {"svc-a", "k1"}, {"svc-b", "k2"}}
	if got := fk.Entries(); !slices.Equal(got, wantBefore) {
		t.Fatalf("Entries() = %v, want %v", got, wantBefore)
	}
	if err := fk.Delete("svc-a", "k1"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := fk.Get("svc-a", "k1"); !errors.Is(err, keychain.ErrNotFound) {
		t.Fatalf("Get after Delete err = %v, want ErrNotFound", err)
	}
	wantAfter := [][2]string{{"svc-a", "k0"}, {"svc-b", "k2"}}
	if got := fk.Entries(); !slices.Equal(got, wantAfter) {
		t.Fatalf("Entries() after Delete = %v, want %v", got, wantAfter)
	}
}

func TestF2_T1_FakeOverwriteAndCallsCount(t *testing.T) {
	fk := newFakeKeychain(t)
	if fk.Calls != 0 {
		t.Fatalf("initial Calls = %d, want 0", fk.Calls)
	}
	if err := fk.Set("svc", "acct", "first"); err != nil {
		t.Fatalf("Set 1: %v", err)
	}
	if err := fk.Set("svc", "acct", "second"); err != nil {
		t.Fatalf("Set 2: %v", err)
	}
	got, err := fk.Get("svc", "acct")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got != "second" {
		t.Fatalf("Get = %q, want %q", got, "second")
	}
	if err := fk.Delete("svc", "acct"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if fk.Calls != 4 {
		t.Fatalf("Fake.Calls = %d, want 4", fk.Calls)
	}
}

func TestF2_T1_FakeConcurrencySafety(t *testing.T) {
	fk := newFakeKeychain(t)
	const workers = 16
	const ops = 40
	var wg sync.WaitGroup
	wg.Add(workers)
	for w := 0; w < workers; w++ {
		go func(id int) {
			defer wg.Done()
			svc := fmt.Sprintf("svc-%d", id%4)
			acct := fmt.Sprintf("acct-%d", id)
			for i := 0; i < ops; i++ {
				val := fmt.Sprintf("val-%d-%d", id, i)
				if err := fk.Set(svc, acct, val); err != nil {
					t.Errorf("Set: %v", err)
					return
				}
				if _, err := fk.Get(svc, acct); err != nil && !errors.Is(err, keychain.ErrNotFound) {
					t.Errorf("Get: %v", err)
					return
				}
				_ = fk.Entries()
				if i%5 == 0 {
					_ = fk.Delete(svc, acct)
				}
			}
		}(w)
	}
	wg.Wait()
	if fk.Calls < workers*ops*2 {
		t.Fatalf("unexpected low Fake.Calls = %d", fk.Calls)
	}
}

func TestF2_T1_EnvVarFileDisablesOSKeychain(t *testing.T) {
	if keychain.EnvVar != "GVX_KEYCHAIN" {
		t.Fatalf("keychain.EnvVar = %q, want GVX_KEYCHAIN", keychain.EnvVar)
	}
	root := repoRoot(t)
	probeDir := t.TempDir()
	srcPath := filepath.Join(probeDir, "main.go")
	probeSrc := `package main

import (
	"fmt"
	"os"

	"github.com/ironpark/gvx-desktop/internal/keychain"
)

func main() {
	kc, err := keychain.System()
	if kc != nil || err == nil {
		fmt.Fprintf(os.Stderr, "unexpected System() = (%v, %v)\n", kc, err)
		os.Exit(1)
	}
	fmt.Print(err.Error())
}
`
	if err := os.WriteFile(srcPath, []byte(probeSrc), 0o600); err != nil {
		t.Fatalf("write keychainprobe main.go: %v", err)
	}
	overlay := map[string]any{
		"Replace": map[string]string{
			"e2e/keychainprobe/main.go": srcPath,
		},
	}
	overlayBytes, err := json.Marshal(overlay)
	if err != nil {
		t.Fatalf("marshal overlay: %v", err)
	}
	overlayPath := filepath.Join(probeDir, "overlay.json")
	if err := os.WriteFile(overlayPath, overlayBytes, 0o600); err != nil {
		t.Fatalf("write overlay.json: %v", err)
	}
	binPath := filepath.Join(probeDir, "keychainprobe")
	if runtime.GOOS == "windows" {
		binPath += ".exe"
	}
	if out, err := runGo(t, root, nil, "build", "-overlay="+overlayPath, "-o", binPath, "./e2e/keychainprobe"); err != nil {
		t.Fatalf("build keychainprobe via overlay: %v\n%s", err, out)
	}
	for _, val := range []string{"file", "FILE"} {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		cmd := exec.CommandContext(ctx, binPath)
		cmd.Env = append(os.Environ(), keychain.EnvVar+"="+val)
		out, err := cmd.CombinedOutput()
		cancel()
		if err != nil {
			t.Fatalf("keychainprobe (%s=%s) failed: %v\n%s", keychain.EnvVar, val, err, string(out))
		}
		if !strings.Contains(string(out), "GVX_KEYCHAIN=file") {
			t.Fatalf("keychainprobe (%s=%s) output = %q, want substring %q", keychain.EnvVar, val, string(out), "GVX_KEYCHAIN=file")
		}
	}
}

func TestF2_T2_SystemRefusesTestingBinary(t *testing.T) {
	t.Setenv(keychain.EnvVar, "")
	start := time.Now()
	kc, err := keychain.System()
	elapsed := time.Since(start)
	if err == nil || kc != nil {
		t.Fatalf("System() inside go test = (%v, %v), want (nil, error)", kc, err)
	}
	if elapsed > 200*time.Millisecond {
		t.Fatalf("System() took %v inside go test; expected immediate refusal", elapsed)
	}
}

func TestF2_T2_GetMissingReturnsErrNotFound(t *testing.T) {
	fk := newFakeKeychain(t)
	_, err := fk.Get("missing-svc", "missing-acct")
	if !errors.Is(err, keychain.ErrNotFound) {
		t.Fatalf("Get missing err = %v, want ErrNotFound", err)
	}
}

func TestF2_T2_DeleteMissingReturnsErrNotFound(t *testing.T) {
	fk := newFakeKeychain(t)
	err := fk.Delete("missing-svc", "missing-acct")
	if !errors.Is(err, keychain.ErrNotFound) {
		t.Fatalf("Delete missing err = %v, want ErrNotFound", err)
	}
}

func TestF2_T2_FakeFailPropagatesError(t *testing.T) {
	sentinel := errors.New("keychain locked")
	fk := newFakeKeychain(t)
	fk.Fail = sentinel
	if err := fk.Set("s", "a", "v"); !errors.Is(err, sentinel) {
		t.Fatalf("Set err = %v, want %v", err, sentinel)
	}
	if _, err := fk.Get("s", "a"); !errors.Is(err, sentinel) {
		t.Fatalf("Get err = %v, want %v", err, sentinel)
	}
	if err := fk.Delete("s", "a"); !errors.Is(err, sentinel) {
		t.Fatalf("Delete err = %v, want %v", err, sentinel)
	}
}

func TestF2_T2_ErrTimeoutSentinelContract(t *testing.T) {
	if keychain.ErrTimeout == nil {
		t.Fatal("keychain.ErrTimeout is nil")
	}
	if errors.Is(keychain.ErrTimeout, keychain.ErrNotFound) {
		t.Fatal("ErrTimeout must be distinct from ErrNotFound")
	}
	if !strings.Contains(keychain.ErrTimeout.Error(), "20s") {
		t.Fatalf("ErrTimeout.Error() = %q, expected 20s timeout notice", keychain.ErrTimeout.Error())
	}
}

// =============================================================================
// Feature 3: internal/uiblock 35 Node Kinds & Validation
// =============================================================================

func TestF3_T1_ReadAll35Kinds(t *testing.T) {
	if len(uiblock.Kinds) != 35 {
		t.Fatalf("len(uiblock.Kinds) = %d, want 35", len(uiblock.Kinds))
	}
	icons := uiblock.LucideIconNames()
	if len(icons) == 0 {
		t.Fatal("uiblock.LucideIconNames() returned empty slice")
	}
	icon := icons[0]

	raw := fmt.Sprintf(`{
  "kind": "column",
  "fill": true,
  "children": [
    {"kind": "row", "variant": "toolbar", "children": [
      {"kind": "breadcrumb", "options": ["Home", "Actions"]},
      {"kind": "button", "text": "Run", "icon": %q, "variant": "primary", "onTap": ["run"]}
    ]},
    {"kind": "stack", "children": [
      {"kind": "card", "children": [
        {"kind": "text", "text": "Hello", "tone": "accent"},
        {"kind": "icon", "icon": %q},
        {"kind": "pill", "text": "v1", "variant": "soft"},
        {"kind": "chip", "text": "tag", "variant": "outline"}
      ]}
    ]},
    {"kind": "scroll", "variant": "follow", "direction": "vertical", "children": [
      {"kind": "wrap", "children": [
        {"kind": "grid", "columns": 2, "children": [
          {"kind": "collapsible", "text": "Details", "children": [
            {"kind": "markdown", "text": "# Heading\nBody"},
            {"kind": "code", "language": "go", "text": "fmt.Println(\"ok\")"}
          ]},
          {"kind": "list", "children": [
            {"kind": "progress", "progress": 0.5},
            {"kind": "divider"}
          ]}
        ]}
      ]}
    ]},
    {"kind": "split", "direction": "auto", "children": [
      {"kind": "tree", "children": [
        {"kind": "tree_item", "text": "Node A", "check": "off", "onCheck": ["chk"]}
      ]},
      {"kind": "table", "labels": ["Name", "State"], "children": [
        {"kind": "row", "children": [
          {"kind": "text", "text": "job-1"},
          {"kind": "text", "text": "ok"}
        ]}
      ]}
    ]},
    {"kind": "input", "key": "inp", "placeholder": "name"},
    {"kind": "textarea", "key": "ta", "placeholder": "notes"},
    {"kind": "toggle", "key": "tog", "text": "Enable"},
    {"kind": "select", "key": "sel", "options": ["a", "b"], "value": "a"},
    {"kind": "tabs", "key": "tab", "options": ["One", "Two"], "value": "One"},
    {"kind": "checkbox", "key": "cb", "text": "Agree", "check": "mixed"},
    {"kind": "chart", "chart": "line", "series": [{"name": "cpu", "tone": "accent", "values": [1, 2, 3]}]},
    {"kind": "graph", "graphNodes": [{"id": "n1", "label": "Start"}, {"id": "n2", "label": "End"}], "graphEdges": [{"from": "n1", "to": "n2"}]},
    {"kind": "menu", "text": "Actions", "children": [
      {"kind": "menu_item", "text": "Restart", "onTap": ["restart"]}
    ]},
    {"kind": "dialog", "text": "Confirm action?"},
    {"kind": "dag", "rows": [{"id": "build", "text": "Build", "tone": "success"}]}
  ]
}`, icon, icon)

	tree, err := uiblock.Read([]byte(raw))
	if err != nil {
		t.Fatalf("uiblock.Read all 35 kinds failed: %v", err)
	}
	seen := map[string]bool{}
	var walk func(n *uiblock.Node)
	walk = func(n *uiblock.Node) {
		if n == nil {
			return
		}
		seen[n.Kind] = true
		for _, c := range n.Children {
			walk(c)
		}
	}
	walk(tree.Root)
	for _, k := range uiblock.Kinds {
		if !seen[k] {
			t.Fatalf("kind %q missing from parsed 35-kind tree", k)
		}
	}
}

func TestF3_T1_InputPasswordVariant(t *testing.T) {
	if !slices.Contains(uiblock.VariantsOf(uiblock.KindInput), "password") {
		t.Fatalf("VariantsOf(KindInput) = %v, expected password", uiblock.VariantsOf(uiblock.KindInput))
	}
	raw := `{"kind":"column","children":[{"kind":"input","key":"secret","variant":"password","placeholder":"Enter token"}]}`
	tree, err := uiblock.Read([]byte(raw))
	if err != nil {
		t.Fatalf("uiblock.Read password input: %v", err)
	}
	node := uiblock.NodeOf(tree.Root, "/secret")
	if node == nil || node.Variant != "password" {
		t.Fatalf("expected input node with variant=password, got %+v", node)
	}
}

func TestF3_T1_TreeMessageExtraction(t *testing.T) {
	raw := `{
  "kind": "column",
  "children": [
    {"kind": "button", "key": "btn", "text": "Run", "onTap": ["run_job", "ci", 42]},
    {"kind": "toggle", "key": "tog", "text": "Auto", "onChange": ["set_auto"]},
    {"kind": "tree", "children": [
      {"kind": "tree_item", "key": "item", "text": "Leaf", "check": "off", "onCheck": ["toggle_leaf"]}
    ]}
  ]
}`
	tree, err := uiblock.Read([]byte(raw))
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	tapMsg, err := tree.Message(uiblock.Event{Block: "main", Node: "/btn", Kind: uiblock.EventTap})
	if err != nil {
		t.Fatalf("Message(tap): %v", err)
	}
	if tapMsg.Name != "run_job" || len(tapMsg.Args) != 2 {
		t.Fatalf("tap Message = %+v, want name=run_job len(args)=2", tapMsg)
	}

	chgMsg, err := tree.Message(uiblock.Event{Block: "main", Node: "/tog", Kind: uiblock.EventChange, Value: "true"})
	if err != nil {
		t.Fatalf("Message(change toggle): %v", err)
	}
	if chgMsg.Name != "set_auto" || len(chgMsg.Args) != 1 || string(chgMsg.Args[0]) != "true" {
		t.Fatalf("toggle change Message = %+v, want bool true arg", chgMsg)
	}

	chkMsg, err := tree.Message(uiblock.Event{Block: "main", Node: "/#2/item", Kind: uiblock.EventCheck, Value: "on"})
	if err != nil {
		t.Fatalf("Message(check): %v", err)
	}
	if chkMsg.Name != "toggle_leaf" || string(chkMsg.Args[0]) != `"on"` {
		t.Fatalf("check Message = %+v", chkMsg)
	}

	if _, err := tree.Message(uiblock.Event{Block: "main", Node: "/btn", Kind: uiblock.EventChange, Value: "x"}); err == nil {
		t.Fatal("expected error for undeclared change event on button")
	}
}

func TestF3_T1_TreeValuesFiltering(t *testing.T) {
	raw := `{
  "kind": "column",
  "children": [
    {"kind": "input", "key": "inp"},
    {"kind": "textarea", "key": "ta"},
    {"kind": "toggle", "key": "tog"},
    {"kind": "select", "key": "sel", "options": ["a", "b"]},
    {"kind": "tabs", "key": "tab", "options": ["x", "y"]},
    {"kind": "checkbox", "key": "cb"},
    {"kind": "text", "key": "lbl", "text": "Static"}
  ]
}`
	tree, err := uiblock.Read([]byte(raw))
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	vals, err := tree.Values(map[string]string{
		"/inp":   "hello",
		"/ta":    "multiline",
		"/tog":   "true",
		"/sel":   "b",
		"/tab":   "y",
		"/cb":    "on",
		"/stale": "ignored",
	})
	if err != nil {
		t.Fatalf("Values: %v", err)
	}
	if len(vals) != 6 {
		t.Fatalf("len(Values) = %d, want 6 (%v)", len(vals), vals)
	}
	if _, exists := vals["/stale"]; exists {
		t.Fatalf("stale missing node ID should be omitted, got %v", vals)
	}
	if _, err := tree.Values(map[string]string{"/lbl": "bad"}); err == nil {
		t.Fatal("expected error when passing non-input text node ID to Tree.Values")
	}
}

func TestF3_T1_DiffApplyAndFollowCoalescing(t *testing.T) {
	oldTree, err := uiblock.Read([]byte(`{"kind":"column","children":[{"kind":"text","key":"a","text":"one"},{"kind":"text","key":"b","text":"two"}]}`))
	if err != nil {
		t.Fatalf("Read old: %v", err)
	}
	nextTree, err := uiblock.Read([]byte(`{"kind":"column","children":[{"kind":"text","key":"b","text":"two-edited"},{"kind":"text","key":"c","text":"three"}]}`))
	if err != nil {
		t.Fatalf("Read next: %v", err)
	}
	ops := uiblock.Diff(oldTree.Root, nextTree.Root)
	if len(ops) == 0 {
		t.Fatal("Diff returned empty ops for changed trees")
	}
	applied, err := uiblock.Apply(oldTree.Root, ops)
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if !applied.Equal(nextTree.Root) {
		t.Fatalf("Apply(old, Diff(old, next)) != next")
	}

	// Follow coalescing test: rapid burst of 10 changes over 80ms interval.
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	var mu sync.Mutex
	step := 0
	ch := make(chan struct{})
	firstSent := make(chan struct{})
	var once sync.Once
	var updates []uiblock.Update
	done := make(chan error, 1)
	go func() {
		done <- uiblock.Follow(ctx, 80*time.Millisecond, func() uiblock.View {
			mu.Lock()
			defer mu.Unlock()
			txt := fmt.Sprintf("step-%d", step)
			return uiblock.View{
				Tree: &uiblock.Node{
					Kind: uiblock.KindColumn,
					Children: []*uiblock.Node{
						{ID: "/status", Kind: uiblock.KindText, Text: txt},
					},
				},
				Changed: ch,
				Gone:    step >= 10,
			}
		}, func(u uiblock.Update) error {
			updates = append(updates, u)
			once.Do(func() { close(firstSent) })
			return nil
		})
	}()

	<-firstSent
	for i := 1; i <= 10; i++ {
		mu.Lock()
		step = i
		old := ch
		ch = make(chan struct{})
		close(old)
		mu.Unlock()
		time.Sleep(3 * time.Millisecond)
	}

	err = <-done
	if !errors.Is(err, uiblock.ErrGone) {
		t.Fatalf("Follow err = %v, want ErrGone", err)
	}
	if len(updates) < 2 || len(updates) >= 11 {
		t.Fatalf("expected coalesced updates (between 2 and 10), got %d", len(updates))
	}
	if updates[0].Tree == nil || updates[0].Seq != 1 {
		t.Fatalf("first Follow update must carry full Tree and Seq=1, got %+v", updates[0])
	}
	last := updates[len(updates)-1]
	if !last.Gone || len(last.Ops) == 0 {
		t.Fatalf("final Follow update expected Gone=true and Ops, got %+v", last)
	}
}

func TestF3_T2_RejectSizeNodesDepthLimits(t *testing.T) {
	// 1. > 512 KiB JSON
	huge := `{"kind":"column","children":[{"kind":"text","text":"` + strings.Repeat("a", 520<<10) + `"}]}`
	if _, err := uiblock.Read([]byte(huge)); err == nil {
		t.Fatal("expected error for JSON > 512 KiB")
	}

	// 2. > 2000 nodes rejected, exactly 2000 nodes accepted
	buildN := func(n int) []byte {
		children := make([]string, 0, n-1)
		for i := 0; i < n-1; i++ {
			children = append(children, `{"kind":"divider"}`)
		}
		return []byte(`{"kind":"column","children":[` + strings.Join(children, ",") + `]}`)
	}
	if _, err := uiblock.Read(buildN(2000)); err != nil {
		t.Fatalf("expected 2000 nodes to pass, got %v", err)
	}
	if _, err := uiblock.Read(buildN(2001)); err == nil {
		t.Fatal("expected > 2000 nodes to be rejected")
	}

	// 3. Depth 24 accepted, depth 25 rejected
	buildDepth := func(d int) []byte {
		s := `{"kind":"text","text":"leaf"}`
		for i := 1; i < d; i++ {
			s = `{"kind":"column","children":[` + s + `]}`
		}
		return []byte(s)
	}
	if _, err := uiblock.Read(buildDepth(24)); err != nil {
		t.Fatalf("expected depth 24 to pass, got %v", err)
	}
	if _, err := uiblock.Read(buildDepth(25)); err == nil {
		t.Fatal("expected depth 25 to be rejected")
	}
}

func TestF3_T2_RejectUnknownFieldsAndHexColors(t *testing.T) {
	cases := []struct {
		name string
		json string
	}{
		{"unknown_field", `{"kind":"text","text":"hi","bogus":123}`},
		{"hex_tone", `{"kind":"text","text":"hi","tone":"#ff0000"}`},
		{"invalid_tone", `{"kind":"text","text":"hi","tone":"rainbow"}`},
	}
	for _, tc := range cases {
		if _, err := uiblock.Read([]byte(tc.json)); err == nil {
			t.Fatalf("%s: expected uiblock.Read to reject %s", tc.name, tc.json)
		}
	}
}

func TestF3_T2_SpoofingAndPrintableSanitization(t *testing.T) {
	spoofRunes := []rune{0x202E, 0x202A, 0x2066, 0x200B, 0xFEFF, '\x1b', '\x00'}
	for _, r := range spoofRunes {
		if !uiblock.Spoofing(r) {
			t.Fatalf("uiblock.Spoofing(%U) = false, want true", r)
		}
		badTree := fmt.Sprintf(`{"kind":"text","text":"a%cb"}`, r)
		if _, err := uiblock.Read([]byte(badTree)); err == nil {
			t.Fatalf("uiblock.Read accepted spoofing rune %U in text", r)
		}
	}
	// Newline & tab are allowed in non-inline text, but rejected in select options.
	if _, err := uiblock.Read([]byte(`{"kind":"text","text":"line1\n\tline2"}`)); err != nil {
		t.Fatalf("expected newline/tab allowed in KindText, got %v", err)
	}
	if _, err := uiblock.Read([]byte(`{"kind":"select","key":"s","options":["a\nb"]}`)); err == nil {
		t.Fatal("expected newline rejected in select options")
	}

	escaped := uiblock.Printable("ok\n\t\x1b[31m\u202e\u200b\x00")
	if strings.ContainsRune(escaped, '\x1b') || strings.ContainsRune(escaped, 0x202E) || strings.ContainsRune(escaped, 0x200B) || strings.ContainsRune(escaped, 0) {
		t.Fatalf("Printable left unsafe runes unescaped: %q", escaped)
	}
	if !strings.Contains(escaped, "\n\t") || !strings.Contains(escaped, `\x1b`) || !strings.Contains(escaped, `\u202e`) {
		t.Fatalf("Printable output unexpected: %q", escaped)
	}
}

func TestF3_T2_RejectFillOutsideDepth1Layout(t *testing.T) {
	for _, valid := range []string{
		`{"kind":"column","fill":true}`,
		`{"kind":"row","fill":true}`,
		`{"kind":"split","fill":true,"children":[{"kind":"text","text":"a"},{"kind":"text","text":"b"}]}`,
	} {
		if _, err := uiblock.Read([]byte(valid)); err != nil {
			t.Fatalf("expected depth-1 layout fill to pass (%s): %v", valid, err)
		}
	}
	for _, invalid := range []string{
		`{"kind":"card","fill":true}`,
		`{"kind":"column","children":[{"kind":"column","fill":true}]}`,
	} {
		if _, err := uiblock.Read([]byte(invalid)); err == nil {
			t.Fatalf("expected fill outside depth-1 column/row/split to be rejected: %s", invalid)
		}
	}
}

func TestF3_T2_RejectOversizedTextAndInvalidKeys(t *testing.T) {
	if _, err := uiblock.Read([]byte(`{"kind":"text","text":"` + strings.Repeat("x", (8<<10)+1) + `"}`)); err == nil {
		t.Fatal("expected text > 8 KiB to be rejected")
	}
	if _, err := uiblock.Read([]byte(`{"kind":"markdown","text":"` + strings.Repeat("m", (32<<10)+1) + `"}`)); err == nil {
		t.Fatal("expected markdown > 32 KiB to be rejected")
	}
	// Total tree text > 256 KiB across 9 markdown nodes of 30 KiB each = 270 KiB.
	chunk := strings.Repeat("k", 30<<10)
	var nodes []string
	for i := 0; i < 9; i++ {
		nodes = append(nodes, `{"kind":"markdown","text":"`+chunk+`"}`)
	}
	if _, err := uiblock.Read([]byte(`{"kind":"column","children":[` + strings.Join(nodes, ",") + `]}`)); err == nil {
		t.Fatal("expected total tree text > 256 KiB to be rejected")
	}

	for _, badKey := range []string{"a/b", "a#b", strings.Repeat("k", 129)} {
		raw := fmt.Sprintf(`{"kind":"column","children":[{"kind":"text","key":%q,"text":"x"}]}`, badKey)
		if _, err := uiblock.Read([]byte(raw)); err == nil {
			t.Fatalf("expected invalid key %q to be rejected", badKey)
		}
	}

	// Values > 32 KiB rejected.
	tree, err := uiblock.Read([]byte(`{"kind":"column","children":[{"kind":"input","key":"k1"},{"kind":"input","key":"k2"},{"kind":"input","key":"k3"},{"kind":"input","key":"k4"},{"kind":"input","key":"k5"}]}`))
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	oversized := map[string]string{
		"/k1": strings.Repeat("a", 8000),
		"/k2": strings.Repeat("b", 8000),
		"/k3": strings.Repeat("c", 8000),
		"/k4": strings.Repeat("d", 8000),
		"/k5": strings.Repeat("e", 2000),
	}
	if _, err := tree.Values(oversized); err == nil {
		t.Fatal("expected Tree.Values > 32 KiB to be rejected")
	}
}

// =============================================================================
// Feature 4: Go Extension SDK (pkg/extsdk)
// =============================================================================

func TestF4_T1_StdlibOnlyImportGraph(t *testing.T) {
	root := repoRoot(t)
	out, err := runGo(t, root, nil, "list", "-deps", "-f", "{{if not .Standard}}{{.ImportPath}}{{end}}", "./pkg/extsdk/...")
	if err != nil {
		t.Fatalf("go list -deps ./pkg/extsdk/... failed: %v\n%s", err, out)
	}
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		dep := strings.TrimSpace(line)
		if dep == "" || strings.HasPrefix(dep, "github.com/ironpark/gvx-desktop/pkg/extsdk") {
			continue
		}
		t.Fatalf("pkg/extsdk transitively depends on non-stdlib package %q", dep)
	}
}

func TestF4_T1_ViewSSEInitialAndCoalescedPatch(t *testing.T) {
	sockDir := newShortTempDataDir(t)
	extSock := filepath.Join(sockDir, "e-view.sock")
	extToken := "ext-secret-token-123"
	hostSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer hostSrv.Close()

	t.Setenv("GVX_API_VERSION", "1")
	t.Setenv("GVX_EXT_ID", "sdk-sse")
	t.Setenv("GVX_HOST_API", hostSrv.URL)
	t.Setenv("GVX_HOST_TOKEN", "host-secret-token")
	t.Setenv("GVX_EXT_LISTEN", "unix://"+extSock)
	t.Setenv("GVX_EXT_TOKEN", extToken)

	var mu sync.Mutex
	counter := 0
	var lastViewCtx *extsdk.ViewContext

	app := extsdk.New()
	app.View("main", extsdk.View{
		Render: func(ctx *extsdk.ViewContext, ui *extsdk.UI) extsdk.Node {
			mu.Lock()
			lastViewCtx = ctx
			n := counter
			mu.Unlock()
			return ui.Column(
				ui.Text(fmt.Sprintf("count-%d", n), extsdk.Key("status")),
			)
		},
	})

	runCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	runErr := make(chan error, 1)
	go func() { runErr <- app.Run(runCtx) }()

	client := newUnixHTTPClient(extSock)
	waitForSDKHealth(t, client, extToken)

	req, err := http.NewRequestWithContext(runCtx, http.MethodGet, "http://unix/v1/views/main/stream?locale=en", nil)
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+extToken)
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("SSE stream request: %v", err)
	}
	defer resp.Body.Close()

	ch := make(chan SSEEvent, 16)
	go readSSEEvents(runCtx, resp, ch)

	first := awaitSSEEvent(t, ch, 3*time.Second, func(ev SSEEvent) bool {
		return ev.Event == "tree"
	})
	if _, err := uiblock.Read([]byte(first.Data)); err != nil {
		t.Fatalf("initial SSE tree failed uiblock.Read: %v\ndata: %s", err, first.Data)
	}

	// Burst updates to exercise coalescing.
	for i := 1; i <= 5; i++ {
		mu.Lock()
		counter = i
		vctx := lastViewCtx
		mu.Unlock()
		if vctx != nil {
			vctx.NotifyChanged()
		}
		time.Sleep(5 * time.Millisecond)
	}

	upd := awaitSSEEvent(t, ch, 3*time.Second, func(ev SSEEvent) bool {
		return ev.Event == "patch" || ev.Event == "tree"
	})
	if !strings.Contains(upd.Data, "count-") && !strings.Contains(upd.Data, "ops") {
		t.Fatalf("unexpected coalesced SSE update payload: %+v", upd)
	}

	status, _, _ := httpDoJSON(t, client, http.MethodPost, "http://unix/v1/shutdown", extToken, "", nil, nil)
	if status != http.StatusOK {
		t.Fatalf("shutdown status = %d, want 200", status)
	}
	if err := <-runErr; err != nil {
		t.Fatalf("App.Run returned error after /v1/shutdown: %v", err)
	}
}

func TestF4_T1_CommandToolSettingsAndEventHandlers(t *testing.T) {
	sockDir := newShortTempDataDir(t)
	extSock := filepath.Join(sockDir, "e-handlers.sock")
	extToken := "ext-tok-handlers"

	hostEvents := make(chan string, 4)
	hostSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/events" {
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusOK)
			_, _ = fmt.Fprint(w, "event: host.theme\ndata: {\"theme\":\"light\"}\n\n")
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}
			select {
			case <-r.Context().Done():
			case <-time.After(2 * time.Second):
			}
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer hostSrv.Close()

	t.Setenv("GVX_API_VERSION", "1")
	t.Setenv("GVX_EXT_ID", "sdk-handlers")
	t.Setenv("GVX_HOST_API", hostSrv.URL)
	t.Setenv("GVX_HOST_TOKEN", "host-tok")
	t.Setenv("GVX_EXT_LISTEN", "unix://"+extSock)
	t.Setenv("GVX_EXT_TOKEN", extToken)

	var cmdCalled, settingsCalled bool
	var mu sync.Mutex

	app := extsdk.New()
	app.Command("refresh", func(ctx *extsdk.CommandContext) error {
		mu.Lock()
		cmdCalled = true
		mu.Unlock()
		return nil
	})
	app.Tool("echo", func(ctx *extsdk.ToolContext, args map[string]any) (any, error) {
		return map[string]any{"echo": args["msg"]}, nil
	})
	app.OnSettingsChanged(func(ctx *extsdk.Context) error {
		mu.Lock()
		settingsCalled = true
		mu.Unlock()
		return nil
	})
	app.OnEvent(func(ev extsdk.HostEvent) {
		select {
		case hostEvents <- ev.Type:
		default:
		}
	})

	runCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	runErr := make(chan error, 1)
	go func() { runErr <- app.Run(runCtx) }()

	client := newUnixHTTPClient(extSock)
	waitForSDKHealth(t, client, extToken)

	st, _, _ := httpDoJSON(t, client, http.MethodPost, "http://unix/v1/commands/refresh", extToken, "g-cmd-1", map[string]string{"locale": "en"}, nil)
	if st != http.StatusOK {
		t.Fatalf("POST /v1/commands/refresh = %d, want 200", st)
	}

	var toolResp map[string]any
	st, _, _ = httpDoJSON(t, client, http.MethodPost, "http://unix/v1/tools/echo", extToken, "", map[string]any{"args": map[string]any{"msg": "pong"}}, &toolResp)
	if st != http.StatusOK || toolResp["echo"] != "pong" {
		t.Fatalf("POST /v1/tools/echo = (%d, %v), want (200, echo=pong)", st, toolResp)
	}

	st, _, _ = httpDoJSON(t, client, http.MethodPost, "http://unix/v1/settings/changed", extToken, "", map[string]any{}, nil)
	if st != http.StatusOK {
		t.Fatalf("POST /v1/settings/changed = %d, want 200", st)
	}

	mu.Lock()
	okCmd, okSettings := cmdCalled, settingsCalled
	mu.Unlock()
	if !okCmd || !okSettings {
		t.Fatalf("expected Command (%v) and OnSettingsChanged (%v) invoked", okCmd, okSettings)
	}

	select {
	case evType := <-hostEvents:
		if evType != "host.theme" {
			t.Fatalf("received host event %q, want host.theme", evType)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for Host SSE event dispatch to OnEvent")
	}

	_, _, _ = httpDoJSON(t, client, http.MethodPost, "http://unix/v1/shutdown", extToken, "", nil, nil)
	if err := <-runErr; err != nil {
		t.Fatalf("App.Run exit err: %v", err)
	}
}

func TestF4_T1_UIBuildersAll35KindsValid(t *testing.T) {
	ui := &extsdk.UI{}
	root := ui.Column(
		ui.Row(
			ui.Breadcrumb([]string{"Home", "Build"}),
			ui.Button("Run", extsdk.Icon("play"), extsdk.Variant("primary")),
		).Variant("toolbar"),
		ui.Stack(
			ui.Card(
				ui.Text("Status", extsdk.Tone("accent")),
				ui.Icon("box"),
				ui.Pill("v1", extsdk.Variant("soft")),
				ui.Chip("e2e", extsdk.Variant("outline")),
			),
		),
		ui.Scroll(
			ui.Wrap(
				ui.Grid(2,
					ui.Collapsible("More",
						ui.Markdown("# Notes"),
						ui.Code("go", "package main"),
					),
					ui.List(
						ui.Progress(0.75),
						ui.Divider(),
					),
				),
			),
		).Variant("follow"),
		ui.Split("auto",
			ui.Tree(ui.TreeItem("Item", extsdk.Check("off"))),
			ui.Table([]string{"Col"}, ui.Row(ui.Text("val"))),
		),
		ui.Input(extsdk.Key("k_in"), extsdk.Variant("password")),
		ui.Textarea(extsdk.Key("k_ta")),
		ui.Toggle(extsdk.Key("k_tog")),
		ui.Select(extsdk.Key("k_sel"), []string{"a", "b"}, "a"),
		ui.Tabs(extsdk.Key("k_tabs"), []string{"T1", "T2"}, "T1"),
		ui.Checkbox(extsdk.Key("k_cb"), "Check", extsdk.Check("mixed")),
		ui.Chart("line", []extsdk.ChartSeries{{Name: "s", Values: []float64{1, 2}}}),
		ui.Graph([]extsdk.GraphNode{{ID: "a", Label: "A"}}, []extsdk.GraphEdge{{From: "a", To: "a"}}),
		ui.Menu("Menu", ui.MenuItem("Action")),
		ui.Dialog("Confirm?"),
		ui.Dag([]extsdk.DagRow{{ID: "r1", Text: "Build"}}),
	).Fill(true)

	data, err := json.Marshal(root)
	if err != nil {
		t.Fatalf("json.Marshal SDK UI tree: %v", err)
	}
	tree, err := uiblock.Read(data)
	if err != nil {
		t.Fatalf("uiblock.Read on SDK 35-kind builder output failed: %v", err)
	}
	seen := map[string]bool{}
	var walk func(n *uiblock.Node)
	walk = func(n *uiblock.Node) {
		if n == nil {
			return
		}
		seen[n.Kind] = true
		for _, c := range n.Children {
			walk(c)
		}
	}
	walk(tree.Root)
	for _, k := range uiblock.Kinds {
		if !seen[k] {
			t.Fatalf("SDK UI builders missing kind %q", k)
		}
	}
}

func TestF4_T1_HostClientGestureForwardingAndShutdown(t *testing.T) {
	sockDir := newShortTempDataDir(t)
	extSock := filepath.Join(sockDir, "e-host.sock")
	extToken := "ext-tok-host"
	hostToken := "host-tok-expected"

	var mu sync.Mutex
	seenGestures := map[string]string{}
	hostSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+hostToken {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		mu.Lock()
		seenGestures[r.URL.Path] = r.Header.Get("X-GVX-Gesture-ID")
		mu.Unlock()

		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/v1/host/info":
			_, _ = w.Write([]byte(`{"app_version":"0.10.1","os":"darwin","arch":"arm64","locale":"en","theme":"dark","docker_mode":"sandbox","docker_ready":true}`))
		case "/v1/ui/pick-folder":
			_, _ = w.Write([]byte(`{"cancelled":false,"path":"/tmp/project"}`))
		default:
			_, _ = w.Write([]byte(`{}`))
		}
	}))
	defer hostSrv.Close()

	t.Setenv("GVX_API_VERSION", "1")
	t.Setenv("GVX_EXT_ID", "sdk-gesture")
	t.Setenv("GVX_HOST_API", hostSrv.URL)
	t.Setenv("GVX_HOST_TOKEN", hostToken)
	t.Setenv("GVX_EXT_LISTEN", "unix://"+extSock)
	t.Setenv("GVX_EXT_TOKEN", extToken)

	app := extsdk.New()
	app.View("main", extsdk.View{
		Render: func(ctx *extsdk.ViewContext, ui *extsdk.UI) extsdk.Node {
			return ui.Column(ui.Button("Act", extsdk.Key("act"), extsdk.OnTap("act")))
		},
		On: extsdk.Handlers{
			"act": func(ctx *extsdk.EventContext) error {
				if _, err := ctx.Host.Info(ctx); err != nil {
					return err
				}
				if _, _, err := ctx.Host.PickFolder(ctx); err != nil {
					return err
				}
				if err := ctx.Host.OpenURL(ctx, "https://example.invalid"); err != nil {
					return err
				}
				return ctx.Host.Copy(ctx, "copied-text")
			},
		},
	})

	runCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	runErr := make(chan error, 1)
	go func() { runErr <- app.Run(runCtx) }()

	client := newUnixHTTPClient(extSock)
	waitForSDKHealth(t, client, extToken)

	st, _, body := httpDoJSON(t, client, http.MethodPost, "http://unix/v1/views/main/event", extToken, "g-123", map[string]any{
		"node_id": "/act",
		"kind":    "tap",
		"locale":  "en",
	}, nil)
	if st != http.StatusOK {
		t.Fatalf("view event returned %d: %s", st, string(body))
	}

	mu.Lock()
	for _, p := range []string{"/v1/ui/pick-folder", "/v1/ui/open-url", "/v1/ui/copy"} {
		if got := seenGestures[p]; got != "g-123" {
			mu.Unlock()
			t.Fatalf("path %s received X-GVX-Gesture-ID = %q, want g-123", p, got)
		}
	}
	mu.Unlock()

	st, _, _ = httpDoJSON(t, client, http.MethodPost, "http://unix/v1/shutdown", extToken, "", nil, nil)
	if st != http.StatusOK {
		t.Fatalf("shutdown status = %d, want 200", st)
	}
	if err := <-runErr; err != nil {
		t.Fatalf("App.Run returned %v", err)
	}
}

func TestF4_T2_RejectMissingOrWrongBearerToken(t *testing.T) {
	sockDir := newShortTempDataDir(t)
	extSock := filepath.Join(sockDir, "e-auth.sock")
	extToken := "valid-bearer-token"
	hostSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer hostSrv.Close()

	t.Setenv("GVX_API_VERSION", "1")
	t.Setenv("GVX_EXT_ID", "sdk-auth")
	t.Setenv("GVX_HOST_API", hostSrv.URL)
	t.Setenv("GVX_HOST_TOKEN", "host-tok")
	t.Setenv("GVX_EXT_LISTEN", "unix://"+extSock)
	t.Setenv("GVX_EXT_TOKEN", extToken)

	app := extsdk.New()
	app.View("main", extsdk.View{
		Render: func(ctx *extsdk.ViewContext, ui *extsdk.UI) extsdk.Node {
			return ui.Column(ui.Text("ok"))
		},
	})
	runCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = app.Run(runCtx) }()

	client := newUnixHTTPClient(extSock)
	waitForSDKHealth(t, client, extToken)

	endpoints := [][2]string{
		{http.MethodGet, "/v1/health"},
		{http.MethodGet, "/v1/views/main/stream"},
		{http.MethodPost, "/v1/views/main/event"},
		{http.MethodPost, "/v1/commands/cmd"},
		{http.MethodPost, "/v1/tools/tool"},
		{http.MethodPost, "/v1/shutdown"},
	}
	for _, ep := range endpoints {
		for _, badTok := range []string{"", "wrong-token"} {
			st, _, _ := httpDoJSON(t, client, ep[0], "http://unix"+ep[1], badTok, "", nil, nil)
			if st != http.StatusUnauthorized {
				t.Fatalf("%s %s with token %q returned %d, want 401", ep[0], ep[1], badTok, st)
			}
		}
	}
}

func TestF4_T2_HostClientTypedHTTPErrors(t *testing.T) {
	for _, code := range []int{401, 403, 429, 503} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(code)
			_, _ = fmt.Fprintf(w, `{"error":"status-%d"}`, code)
		}))
		host := extsdk.NewHostClient(srv.URL, "tok")
		_, err := host.Info(context.Background())
		srv.Close()
		if err == nil {
			t.Fatalf("expected error for HTTP %d, got nil", code)
		}
		if !strings.Contains(err.Error(), strconv.Itoa(code)) {
			t.Fatalf("error %q does not surface status code %d", err.Error(), code)
		}
	}
}

func TestF4_T2_MissingEnvReturnsStartupError(t *testing.T) {
	t.Setenv("GVX_HOST_API", "")
	t.Setenv("GVX_HOST_TOKEN", "")
	t.Setenv("GVX_EXT_LISTEN", "")
	t.Setenv("GVX_EXT_TOKEN", "")
	app := extsdk.New()
	if err := app.Run(context.Background()); err == nil {
		t.Fatal("expected App.Run with missing GVX_* env vars to return error")
	}
}

func TestF4_T2_SSEDisconnectAndReconnect(t *testing.T) {
	sockDir := newShortTempDataDir(t)
	extSock := filepath.Join(sockDir, "e-reconn.sock")
	extToken := "ext-reconn-tok"
	hostSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer hostSrv.Close()

	t.Setenv("GVX_API_VERSION", "1")
	t.Setenv("GVX_EXT_ID", "sdk-reconn")
	t.Setenv("GVX_HOST_API", hostSrv.URL)
	t.Setenv("GVX_HOST_TOKEN", "host-tok")
	t.Setenv("GVX_EXT_LISTEN", "unix://"+extSock)
	t.Setenv("GVX_EXT_TOKEN", extToken)

	app := extsdk.New()
	app.View("main", extsdk.View{
		Render: func(ctx *extsdk.ViewContext, ui *extsdk.UI) extsdk.Node {
			return ui.Column(ui.Text("alive"))
		},
	})
	runCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = app.Run(runCtx) }()

	client := newUnixHTTPClient(extSock)
	waitForSDKHealth(t, client, extToken)

	// Connect and drop first stream.
	c1, cancel1 := context.WithCancel(runCtx)
	req1, _ := http.NewRequestWithContext(c1, http.MethodGet, "http://unix/v1/views/main/stream", nil)
	req1.Header.Set("Authorization", "Bearer "+extToken)
	resp1, err := client.Do(req1)
	if err != nil {
		t.Fatalf("first SSE stream: %v", err)
	}
	ch1 := make(chan SSEEvent, 4)
	go readSSEEvents(c1, resp1, ch1)
	_ = awaitSSEEvent(t, ch1, 2*time.Second, func(ev SSEEvent) bool { return ev.Event == "tree" })
	cancel1()
	_ = resp1.Body.Close()

	// Reconnect second stream and verify fresh initial tree.
	req2, _ := http.NewRequestWithContext(runCtx, http.MethodGet, "http://unix/v1/views/main/stream", nil)
	req2.Header.Set("Authorization", "Bearer "+extToken)
	resp2, err := client.Do(req2)
	if err != nil {
		t.Fatalf("reconnect SSE stream: %v", err)
	}
	defer resp2.Body.Close()
	ch2 := make(chan SSEEvent, 4)
	go readSSEEvents(runCtx, resp2, ch2)
	ev2 := awaitSSEEvent(t, ch2, 2*time.Second, func(ev SSEEvent) bool { return ev.Event == "tree" })
	if _, err := uiblock.Read([]byte(ev2.Data)); err != nil {
		t.Fatalf("reconnected SSE tree invalid: %v", err)
	}
}

func TestF4_T2_HandlerPanicAndErrorRecovery(t *testing.T) {
	sockDir := newShortTempDataDir(t)
	extSock := filepath.Join(sockDir, "e-panic.sock")
	extToken := "ext-panic-tok"
	hostSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer hostSrv.Close()

	t.Setenv("GVX_API_VERSION", "1")
	t.Setenv("GVX_EXT_ID", "sdk-panic")
	t.Setenv("GVX_HOST_API", hostSrv.URL)
	t.Setenv("GVX_HOST_TOKEN", "host-tok")
	t.Setenv("GVX_EXT_LISTEN", "unix://"+extSock)
	t.Setenv("GVX_EXT_TOKEN", extToken)

	app := extsdk.New()
	app.Command("boom", func(ctx *extsdk.CommandContext) error {
		panic("boom")
	})
	app.Command("fail", func(ctx *extsdk.CommandContext) error {
		return errors.New("intentional handler error")
	})
	runCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = app.Run(runCtx) }()

	client := newUnixHTTPClient(extSock)
	waitForSDKHealth(t, client, extToken)

	st, _, _ := httpDoJSON(t, client, http.MethodPost, "http://unix/v1/commands/boom", extToken, "", map[string]string{}, nil)
	if st != http.StatusInternalServerError {
		t.Fatalf("panicking command returned %d, want 500", st)
	}
	st, _, _ = httpDoJSON(t, client, http.MethodPost, "http://unix/v1/commands/fail", extToken, "", map[string]string{}, nil)
	if st != http.StatusInternalServerError {
		t.Fatalf("failing command returned %d, want 500", st)
	}
	st, _, _ = httpDoJSON(t, client, http.MethodGet, "http://unix/v1/health", extToken, "", nil, nil)
	if st != http.StatusOK {
		t.Fatalf("/v1/health after panic returned %d, want 200", st)
	}
}

func waitForSDKHealth(t *testing.T, client *http.Client, token string) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		req, _ := http.NewRequest(http.MethodGet, "http://unix/v1/health", nil)
		req.Header.Set("Authorization", "Bearer "+token)
		resp, err := client.Do(req)
		if err == nil {
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return
			}
		}
		time.Sleep(15 * time.Millisecond)
	}
	t.Fatal("SDK server failed to become healthy within 3s")
}
