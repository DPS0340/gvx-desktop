//go:build e2e

package e2e

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
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
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/ironpark/ggui"
	uitheme "github.com/ironpark/ggui/ui/theme"

	"github.com/ironpark/gvx-desktop/internal/engine"
	"github.com/ironpark/gvx-desktop/internal/ext"
	"github.com/ironpark/gvx-desktop/internal/keychain"
	"github.com/ironpark/gvx-desktop/internal/recipes"
	"github.com/ironpark/gvx-desktop/internal/rungv"
	"github.com/ironpark/gvx-desktop/internal/sandbox"
	"github.com/ironpark/gvx-desktop/internal/store"
	"github.com/ironpark/gvx-desktop/internal/ui"
	"github.com/ironpark/gvx-desktop/internal/uiblock"
	"github.com/ironpark/gvx-desktop/pkg/extsdk"
)

// =============================================================================
// Shared E2E-4 Helpers (all prefixed with e2e4 to prevent package e2e collisions)
// =============================================================================

type e2e4BuildRecord struct {
	Req        ext.BuildRequest
	ContextTar []byte
}

// e2e4ImageBuilder implements landed ext.ImageBuilder using ext.PackBuildContext
// so BuildKit context tar filtering (.dockerignore, .git exclusion) is exercised.
type e2e4ImageBuilder struct {
	mu       sync.Mutex
	Calls    []e2e4BuildRecord
	LogLines []string
	BuildErr error
}

func (b *e2e4ImageBuilder) Build(ctx context.Context, req ext.BuildRequest, onLine func(string)) (string, error) {
	var buf bytes.Buffer
	if req.ContextDir != "" {
		if err := ext.PackBuildContext(ctx, req.ContextDir, req.Dockerfile, &buf); err != nil {
			return "", err
		}
	}
	b.mu.Lock()
	b.Calls = append(b.Calls, e2e4BuildRecord{
		Req:        req,
		ContextTar: buf.Bytes(),
	})
	lines := append([]string(nil), b.LogLines...)
	buildErr := b.BuildErr
	idx := len(b.Calls)
	b.mu.Unlock()

	for _, line := range lines {
		if onLine != nil {
			onLine(line)
		}
	}
	if buildErr != nil {
		return "", buildErr
	}
	return fmt.Sprintf("sha256:e2e4built%055d", idx), nil
}

func (b *e2e4ImageBuilder) SnapshotCalls() []e2e4BuildRecord {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := make([]e2e4BuildRecord, len(b.Calls))
	for i, c := range b.Calls {
		c.ContextTar = append([]byte(nil), c.ContextTar...)
		out[i] = c
	}
	return out
}

// e2e4NewManager creates an isolated *ext.Manager with landed ext.ImageBuilder
// compatibility and registers cleanup.
func e2e4NewManager(t *testing.T, builder ext.ImageBuilder, opts ...ManagerOption) (*ext.Manager, *TestHarness) {
	t.Helper()
	o := &testManagerOptions{
		hostInfo: ext.HostInfo{
			AppVersion: "0.10.1-e2e4",
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
		o.dataDir = newShortTempDataDir(t)
	}
	var fakeKC *keychain.Fake
	kc := o.keychain
	if !o.keychainSet {
		fakeKC = newFakeKeychain(t)
		kc = fakeKC
	} else if kc == nil {
		t.Setenv(keychain.EnvVar, "file")
	} else if f, ok := kc.(*keychain.Fake); ok {
		fakeKC = f
	}
	if builder == nil {
		builder = &e2e4ImageBuilder{}
	}
	h := &TestHarness{
		DataDir:       o.dataDir,
		Keychain:      fakeKC,
		UIHost:        NewMockUIHost(),
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
		ImageBuilder:      builder,
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

func e2e4CreateBinaryExt(t *testing.T, root, id, manifestTOML, goSrc string) string {
	t.Helper()
	binPath := buildFixtureSDKBinary(t, goSrc)
	binData, err := os.ReadFile(binPath)
	if err != nil {
		t.Fatalf("read fixture binary: %v", err)
	}
	extDir := filepath.Join(root, id)
	writeExtFixture(t, extDir, manifestTOML, nil)
	if err := os.MkdirAll(filepath.Join(extDir, "bin"), 0o700); err != nil {
		t.Fatalf("mkdir bin: %v", err)
	}
	if err := os.WriteFile(filepath.Join(extDir, "bin", "ext"), binData, 0o755); err != nil {
		t.Fatalf("write dev_entrypoint: %v", err)
	}
	return extDir
}

func e2e4BuildLinuxSDKBinary(t *testing.T, sourceGoCode string) string {
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
	binPath := filepath.Join(buildDir, "ext-linux")
	if out, err := runGo(t, buildDir, []string{"GOOS=linux", "GOARCH=" + runtime.GOARCH}, "build", "-o", binPath, "."); err != nil {
		t.Fatalf("build linux SDK binary failed: %v\n%s", err, out)
	}
	return binPath
}

func e2e4FindExtInfo(mgr *ext.Manager, id string) (ext.ExtensionInfo, bool) {
	for _, info := range mgr.List() {
		if info.ID == id {
			return info, true
		}
	}
	return ext.ExtensionInfo{}, false
}

func e2e4WaitExtState(t *testing.T, mgr *ext.Manager, id string, timeout time.Duration, pred func(ext.ExtensionInfo) bool) ext.ExtensionInfo {
	t.Helper()
	deadline := time.Now().Add(timeout)
	var last ext.ExtensionInfo
	var found bool
	for time.Now().Before(deadline) {
		last, found = e2e4FindExtInfo(mgr, id)
		if found && pred(last) {
			return last
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatalf("timed out after %v waiting for %s state (found=%v last=%+v)", timeout, id, found, last)
	return last
}

func e2e4StartProbeUI(t *testing.T, dataDir string, devMode bool, sz ggui.Size) (*ggui.Probe, *ui.Model) {
	t.Helper()
	mgr, _ := newTestManager(t, WithDataDir(dataDir), WithDeveloperMode(devMode))
	mgrCtx, mgrCancel := context.WithCancel(context.Background())
	t.Cleanup(mgrCancel)
	if err := mgr.Start(mgrCtx); err != nil {
		t.Fatalf("mgr.Start: %v", err)
	}
	return e2e4StartProbeUIWithManager(t, dataDir, mgr, devMode, sz)
}

func e2e4StartProbeUIWithManager(t *testing.T, dataDir string, mgr *ext.Manager, devMode bool, sz ggui.Size) (*ggui.Probe, *ui.Model) {
	t.Helper()
	st := newTestStore(t, dataDir)
	set, err := st.Settings()
	if err != nil {
		set = store.DefaultSettings()
	}
	set.Language = ui.LangEnglish
	set.NoUpdateCheck = true
	set.DeveloperMode = devMode
	if err := st.SaveSettings(set); err != nil {
		t.Fatalf("SaveSettings: %v", err)
	}

	var m *ui.Model
	p := ggui.ProbeBuilder(func() ggui.Widget {
		return ggui.Provide(ggui.ReducedMotionKey, true, ui.Build(m))
	}, sz)
	p.Setup(func() {
		preset := uitheme.Preset{Base: uitheme.BaseZinc, Accent: uitheme.AccentBlue}
		uitheme.Bind(ggui.Const(false), preset.Dark(), preset.Light())
		m = ui.NewModel(st, dataDir)
		m.Offline = true
		m.Attach(p)
		if mgr != nil {
			m.SetExtensionManager(mgr)
		}
	})
	t.Cleanup(func() {
		m.CloseExtensions(8 * time.Second)
		m.CloseTerminals()
		m.CloseForwards()
		m.CloseDockerProxy()
		m.CloseLocalProxy()
		m.CloseMCPServer()
		m.CloseLocalRegistry()
		p.Close()
	})
	p.Frame()
	return p, m
}

func e2e4SemContains(sem *ggui.SemTree, needle string) bool {
	if sem == nil {
		return false
	}
	if strings.Contains(sem.String(), needle) {
		return true
	}
	for _, node := range sem.All() {
		if strings.Contains(node.Name, needle) || strings.Contains(node.Value, needle) || strings.Contains(node.Description, needle) {
			return true
		}
	}
	return false
}

func e2e4WaitSemantics(t *testing.T, p *ggui.Probe, timeout time.Duration, pred func(*ggui.SemTree) bool) *ggui.SemTree {
	t.Helper()
	deadline := time.Now().Add(timeout)
	var last *ggui.SemTree
	for time.Now().Before(deadline) {
		p.Frame()
		last = p.Semantics()
		if pred(last) {
			return last
		}
		time.Sleep(25 * time.Millisecond)
	}
	dump := "<nil>"
	if last != nil {
		dump = last.String()
	}
	t.Fatalf("timed out after %v waiting for semantics condition:\n%s", timeout, dump)
	return last
}

func e2e4RandomHexToken(t *testing.T) string {
	t.Helper()
	var buf [32]byte
	if _, err := rand.Read(buf[:]); err != nil {
		t.Fatalf("rand.Read: %v", err)
	}
	return hex.EncodeToString(buf[:])
}

// e2e4FakeEngineRunner implements engine.Runner for KeepImages / prune tests.
type e2e4FakeEngineRunner struct {
	mu          sync.Mutex
	images      []rungv.Image
	inspects    map[string]engine.ImageInspect
	removedRefs []string
	eventFn     func(sandbox.Event)
	outputFn    func(string, sandbox.Output)
}

func e2e4NewFakeEngineRunner() *e2e4FakeEngineRunner {
	return &e2e4FakeEngineRunner{
		inspects: make(map[string]engine.ImageInspect),
	}
}

func (r *e2e4FakeEngineRunner) SeedImage(id, repo, tag string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.images = append(r.images, rungv.Image{
		ID:         id,
		Repository: repo,
		Tag:        tag,
		Size:       "10 MB",
		CreatedAt:  time.Now().Add(-time.Hour).UTC().Format(time.RFC3339),
	})
	ref := repo + ":" + tag
	r.inspects[id] = engine.ImageInspect{
		ID:           id,
		RepoTags:     []string{ref},
		Created:      time.Now().Add(-time.Hour).UTC(),
		Architecture: runtime.GOARCH,
		Os:           "linux",
		Size:         10 << 20,
	}
	r.inspects[ref] = r.inspects[id]
}

func (r *e2e4FakeEngineRunner) Removed() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.removedRefs...)
}

func (r *e2e4FakeEngineRunner) RemainingRefs() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]string, 0, len(r.images))
	for _, img := range r.images {
		out = append(out, img.Repository+":"+img.Tag)
	}
	return out
}

func (r *e2e4FakeEngineRunner) SetHandlers(event func(sandbox.Event), output func(string, sandbox.Output)) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.eventFn = event
	r.outputFn = output
}

func (r *e2e4FakeEngineRunner) Start(sandbox.Spec) error              { return nil }
func (r *e2e4FakeEngineRunner) Stop(string, time.Duration) error      { return nil }
func (r *e2e4FakeEngineRunner) Signal(string, syscall.Signal) error   { return nil }
func (r *e2e4FakeEngineRunner) State(string) sandbox.State            { return sandbox.State{} }
func (r *e2e4FakeEngineRunner) Stdin(string) io.Writer                { return nil }
func (r *e2e4FakeEngineRunner) CloseStdin(string) error               { return nil }
func (r *e2e4FakeEngineRunner) Resize(string, uint16, uint16) error   { return nil }
func (r *e2e4FakeEngineRunner) Forget(string)                         {}
func (r *e2e4FakeEngineRunner) Features() rungv.Features              { return rungv.Features{} }
func (r *e2e4FakeEngineRunner) Control(string) sandbox.Control        { return nil }
func (r *e2e4FakeEngineRunner) Pause(context.Context, string) error   { return sandbox.ErrNoControl }
func (r *e2e4FakeEngineRunner) Unpause(context.Context, string) error { return sandbox.ErrNoControl }
func (r *e2e4FakeEngineRunner) Shutdown(string, string, time.Duration) error {
	return nil
}
func (r *e2e4FakeEngineRunner) Update(context.Context, string, rungv.Update) (rungv.Resources, error) {
	return rungv.Resources{}, nil
}
func (r *e2e4FakeEngineRunner) Images(context.Context) ([]rungv.Image, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]rungv.Image(nil), r.images...), nil
}
func (r *e2e4FakeEngineRunner) InspectImages(_ context.Context, refs ...string) ([]engine.ImageInspect, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []engine.ImageInspect
	for _, ref := range refs {
		if ins, ok := r.inspects[ref]; ok {
			out = append(out, ins)
		}
	}
	return out, nil
}
func (r *e2e4FakeEngineRunner) Pull(context.Context, string, string, func(string)) error {
	return nil
}
func (r *e2e4FakeEngineRunner) RemoveImage(_ context.Context, ref string, _ bool) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.removedRefs = append(r.removedRefs, ref)
	kept := r.images[:0]
	for _, img := range r.images {
		full := img.Repository + ":" + img.Tag
		if img.ID == ref || full == ref {
			delete(r.inspects, img.ID)
			delete(r.inspects, full)
			continue
		}
		kept = append(kept, img)
	}
	r.images = kept
	return nil
}
func (r *e2e4FakeEngineRunner) TagImage(context.Context, string, string) error { return nil }
func (r *e2e4FakeEngineRunner) LoadImages(context.Context, io.Reader, func(string)) error {
	return nil
}
func (r *e2e4FakeEngineRunner) SaveImages(context.Context, io.Writer, ...string) error {
	return nil
}
func (r *e2e4FakeEngineRunner) PruneImages(context.Context) (string, error) { return "", nil }
func (r *e2e4FakeEngineRunner) Volumes(context.Context) ([]rungv.Volume, error) {
	return nil, nil
}
func (r *e2e4FakeEngineRunner) CreateVolume(context.Context, string, string) error { return nil }
func (r *e2e4FakeEngineRunner) RemoveVolume(context.Context, string) error         { return nil }
func (r *e2e4FakeEngineRunner) DiskUsage(context.Context) ([]rungv.DiskUsage, error) {
	return nil, nil
}
func (r *e2e4FakeEngineRunner) Info(context.Context) (rungv.Info, error) {
	return rungv.Info{OS: "linux", Architecture: runtime.GOARCH}, nil
}

// e2e4NewRungvSupervisor creates a *sandbox.Supervisor with Client wired so
// launch and Checkpoint never dereference a nil Client().
func e2e4NewRungvSupervisor(t *testing.T, supDataDir, binPath string) *sandbox.Supervisor {
	t.Helper()
	cacheDir := filepath.Join(supDataDir, "cache")
	sup := &sandbox.Supervisor{
		Client: func() *rungv.Client {
			p := binPath
			if p == "" {
				p, _ = rungv.Locate("")
			}
			return &rungv.Client{Path: p, CacheDir: cacheDir}
		},
		DataDir: supDataDir,
	}
	t.Cleanup(func() { sup.StopAll(5 * time.Second) })
	return sup
}

// e2e4NonReservedDir returns a canonicalized, symlink-resolved directory path
// that satisfies internal/ext/grants.go validateAllowedFolder on macOS & Linux.
func e2e4NonReservedDir(t *testing.T) string {
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
	custom, err := os.MkdirTemp(home, "gvx-e2e4-folder-*")
	if err != nil {
		t.Fatalf("os.MkdirTemp in home: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(custom) })
	if resolved, err := filepath.EvalSymlinks(custom); err == nil {
		custom = resolved
	}
	return custom
}

// e2e4InitBareGitRepoWithExec initializes a local bare Git repo with
// executable mode 100755 on execPaths, returning ("file://...", HEAD SHA).
func e2e4InitBareGitRepoWithExec(t *testing.T, files map[string]string, execPaths ...string) (string, string) {
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

// e2e4ServeGitHTTPS serves bareDir over httptest.NewTLSServer + git http-backend
// and sets GIT_SSL_CAINFO so InstallGit uses https:// cleanly.
func e2e4ServeGitHTTPS(t *testing.T, bareDir string) string {
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
	var caBuf bytes.Buffer
	if prev := os.Getenv("GIT_SSL_CAINFO"); prev != "" {
		if prevPEM, err := os.ReadFile(prev); err == nil {
			caBuf.Write(prevPEM)
			if len(prevPEM) > 0 && prevPEM[len(prevPEM)-1] != '\n' {
				caBuf.WriteByte('\n')
			}
		}
	}
	caBuf.Write(pem.EncodeToMemory(&pem.Block{
		Type:  "CERTIFICATE",
		Bytes: cert.Raw,
	}))
	caFile := filepath.Join(t.TempDir(), "git-ca.pem")
	if err := os.WriteFile(caFile, caBuf.Bytes(), 0o600); err != nil {
		t.Fatalf("write CA pem: %v", err)
	}
	t.Setenv("GIT_SSL_CAINFO", caFile)
	return srv.URL + "/" + repoBase
}

func e2e4FreeLoopbackPort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("net.Listen: %v", err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	_ = ln.Close()
	return port
}

// =============================================================================
// Tier 3 — 24 Pairwise Cross-Feature Combination Tests
// =============================================================================

func TestT3_01_SDKUIBlockManagerSSERoundtrip(t *testing.T) {
	dataDir := newShortTempDataDir(t)
	mgr, _ := newTestManager(t, WithDataDir(dataDir), WithDeveloperMode(true))

	src := `package main

import (
	"context"
	"fmt"
	"sync/atomic"

	"github.com/ironpark/gvx-desktop/pkg/extsdk"
)

func main() {
	var count atomic.Int64
	app := extsdk.New()
	app.View("main", extsdk.View{
		Render: func(ctx *extsdk.ViewContext, ui *extsdk.UI) extsdk.Node {
			return ui.Column(
				ui.Text(fmt.Sprintf("count=%d", count.Load()), extsdk.Key("counter")),
				ui.Button("Increment", extsdk.Key("inc"), extsdk.OnTap("inc")),
			).Fill(true)
		},
		On: extsdk.Handlers{
			"inc": func(ctx *extsdk.EventContext) error {
				count.Add(1)
				ctx.NotifyChanged()
				return nil
			},
		},
	})
	_ = app.Run(context.Background())
}
`
	extDir := e2e4CreateBinaryExt(t, t.TempDir(), "t3-sse", binaryManifestTOML("t3-sse", "0.1.0", "bin/ext"), src)
	if _, err := mgr.InstallLocal(extDir); err != nil {
		t.Fatalf("InstallLocal: %v", err)
	}
	if err := mgr.AllowAndBuild("t3-sse"); err != nil {
		t.Fatalf("AllowAndBuild: %v", err)
	}
	e2e4WaitExtState(t, mgr, "t3-sse", 10*time.Second, func(i ext.ExtensionInfo) bool {
		return i.State == ext.StateRunning
	})

	subCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	updates, err := mgr.SubscribeView(subCtx, "t3-sse", "main", "en")
	if err != nil {
		t.Fatalf("SubscribeView: %v", err)
	}

	var curTree *uiblock.Node
	select {
	case upd := <-updates:
		if upd.Tree == nil {
			t.Fatalf("expected initial full tree, got %+v", upd)
		}
		curTree = upd.Tree
	case <-subCtx.Done():
		t.Fatal("timed out waiting for initial SSE tree")
	}

	if n := uiblock.NodeOf(curTree, "/counter"); n == nil || n.Text != "count=0" {
		t.Fatalf("initial counter = %+v, want count=0", n)
	}

	if err := mgr.SendViewEvent("t3-sse", "main", uiblock.Event{
		Node:   "/inc",
		Kind:   uiblock.EventTap,
		Locale: "en",
	}); err != nil {
		t.Fatalf("SendViewEvent: %v", err)
	}

	for {
		select {
		case upd := <-updates:
			if upd.Tree != nil {
				curTree = upd.Tree
			} else if len(upd.Ops) > 0 {
				next, err := uiblock.Apply(curTree, upd.Ops)
				if err != nil {
					t.Fatalf("uiblock.Apply: %v", err)
				}
				curTree = next
			}
			if n := uiblock.NodeOf(curTree, "/counter"); n != nil && n.Text == "count=1" {
				return
			}
		case <-subCtx.Done():
			t.Fatalf("timed out waiting for count=1 after tap; last tree=%+v", curTree)
		}
	}
}

func TestT3_02_SDKUIBlockFormPreservationFollow(t *testing.T) {
	dataDir := newShortTempDataDir(t)
	mgr, _ := newTestManager(t, WithDataDir(dataDir), WithDeveloperMode(true))

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
	tick := 0
	given := "init"

	app := extsdk.New()
	app.View("main", extsdk.View{
		Render: func(ctx *extsdk.ViewContext, ui *extsdk.UI) extsdk.Node {
			mu.Lock()
			t := tick
			g := given
			mu.Unlock()
			return ui.Column(
				ui.Text(fmt.Sprintf("tick-%d", t), extsdk.Key("status")),
				ui.Input(g, extsdk.Key("token"), extsdk.Placeholder("Token Field")),
				ui.Button("Overwrite", extsdk.Key("overwrite"), extsdk.OnTap("overwrite")),
			).Fill(true)
		},
		On: extsdk.Handlers{
			"overwrite": func(ctx *extsdk.EventContext) error {
				mu.Lock()
				given = "server-reset"
				mu.Unlock()
				ctx.NotifyChanged()
				return nil
			},
		},
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		tk := time.NewTicker(50 * time.Millisecond)
		defer tk.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-tk.C:
				mu.Lock()
				tick++
				mu.Unlock()
				app.NotifyChanged("main")
			}
		}
	}()
	_ = app.Run(ctx)
}
`
	extDir := e2e4CreateBinaryExt(t, t.TempDir(), "t3-form", binaryManifestTOML("t3-form", "0.1.0", "bin/ext"), src)
	if _, err := mgr.InstallLocal(extDir); err != nil {
		t.Fatalf("InstallLocal: %v", err)
	}
	if err := mgr.AllowAndBuild("t3-form"); err != nil {
		t.Fatalf("AllowAndBuild: %v", err)
	}
	_ = mgr.Close(3 * time.Second)

	p, m := e2e4StartProbeUI(t, dataDir, true, ggui.Sz(1280, 840))
	m.Go("ext:t3-form/main")

	e2e4WaitSemantics(t, p, 10*time.Second, func(sem *ggui.SemTree) bool {
		return e2e4SemContains(sem, "tick-") && e2e4SemContains(sem, "Overwrite")
	})

	tf, ok := p.FindRole(ggui.RoleTextField, "Token Field")
	if !ok {
		t.Fatalf("Token Field not found in semantics:\n%s", p.Semantics())
	}
	p.Click(tf.Center())
	p.Text("-draft")

	// Let several live SSE Follow updates pass while user text must remain preserved.
	deadline := time.Now().Add(400 * time.Millisecond)
	for time.Now().Before(deadline) {
		p.Frame()
		time.Sleep(40 * time.Millisecond)
	}
	sem := p.Semantics()
	node, found := sem.Find(ggui.RoleTextField, "Token Field")
	if !found || !strings.Contains(node.Value, "-draft") {
		t.Fatalf("expected user-typed form draft preserved across Follow updates, got %+v (found=%v)", node, found)
	}

	// Triggering server change of `given` must overwrite the field.
	p.Tap("Overwrite")
	e2e4WaitSemantics(t, p, 5*time.Second, func(s *ggui.SemTree) bool {
		n, ok := s.Find(ggui.RoleTextField, "Token Field")
		return ok && n.Value == "server-reset"
	})
}

func TestT3_03_SDKGesturePickFolderSandboxRestart(t *testing.T) {
	binPath, _, _ := requireRungv(t)
	dataDir := newShortTempDataDir(t)
	sup := e2e4NewRungvSupervisor(t, filepath.Join(dataDir, "sbx"), binPath)

	eng := engine.New(engine.Config{
		Dir:    filepath.Join(dataDir, "eng"),
		Runner: engine.NewRunner(func() *rungv.Client { return &rungv.Client{Path: binPath} }, filepath.Join(dataDir, "eng-run")),
	})
	if err := eng.Open(context.Background()); err != nil {
		t.Fatalf("eng.Open: %v", err)
	}
	t.Cleanup(func() { _ = eng.Close(5 * time.Second) })

	mgr, h := e2e4NewManager(t, ext.NewEngineImageBuilder(eng.Handler), WithDataDir(dataDir), WithSupervisor(sup))

	approvedDir := e2e4NonReservedDir(t)
	if err := os.WriteFile(filepath.Join(approvedDir, "sentinel.txt"), []byte("hello-from-host-folder"), 0o644); err != nil {
		t.Fatalf("write sentinel.txt: %v", err)
	}
	h.UIHost.PickFolderResult = approvedDir

	src := `package main

import (
	"context"
	"os"
	"path/filepath"
	"sync"

	"github.com/ironpark/gvx-desktop/pkg/extsdk"
)

func main() {
	var mu sync.Mutex
	picked := ""
	app := extsdk.New()
	app.View("main", extsdk.View{
		Render: func(ctx *extsdk.ViewContext, ui *extsdk.UI) extsdk.Node {
			mu.Lock()
			p := picked
			mu.Unlock()
			if p == "" {
				if stored, ok, err := ctx.Host.StorageGet(ctx, "picked"); err == nil && ok && stored != "" {
					mu.Lock()
					picked = stored
					mu.Unlock()
					p = stored
				}
			}
			content := "none"
			if p != "" {
				if b, err := os.ReadFile(filepath.Join(p, "sentinel.txt")); err == nil {
					content = string(b)
				} else {
					content = "err:" + err.Error()
				}
			}
			return ui.Column(
				ui.Text("folder="+content, extsdk.Key("status")),
				ui.Button("Pick Folder", extsdk.Key("pick"), extsdk.OnTap("pick")),
			).Fill(true)
		},
		On: extsdk.Handlers{
			"pick": func(ctx *extsdk.EventContext) error {
				dir, ok, err := ctx.Host.PickFolder(ctx)
				if err != nil {
					return err
				}
				if ok {
					if err := ctx.Host.StoragePut(ctx, "picked", dir); err != nil {
						return err
					}
					mu.Lock()
					picked = dir
					mu.Unlock()
					ctx.NotifyChanged()
				}
				return nil
			},
		},
	})
	_ = app.Run(context.Background())
}
`
	linuxBin := e2e4BuildLinuxSDKBinary(t, src)
	binBytes, err := os.ReadFile(linuxBin)
	if err != nil {
		t.Fatalf("read linux bin: %v", err)
	}
	extDir := filepath.Join(t.TempDir(), "t3-sbx-pick")
	manifest := minimalManifestTOML("t3-sbx-pick", "0.1.0", "ui:pick-folder", "storage")
	writeExtFixture(t, extDir, manifest, map[string]string{
		"Dockerfile": "FROM alpine:3.20\nCOPY ext /usr/local/bin/ext\nEXPOSE 18900\nENTRYPOINT [\"/usr/local/bin/ext\"]\n",
		"ext":        string(binBytes),
	})
	if err := os.Chmod(filepath.Join(extDir, "ext"), 0o755); err != nil {
		t.Fatalf("chmod ext: %v", err)
	}

	if _, err := mgr.InstallLocal(extDir); err != nil {
		t.Fatalf("InstallLocal: %v", err)
	}
	if err := mgr.AllowAndBuild("t3-sbx-pick"); err != nil {
		t.Fatalf("AllowAndBuild: %v", err)
	}
	e2e4WaitExtState(t, mgr, "t3-sbx-pick", 25*time.Second, func(i ext.ExtensionInfo) bool {
		return i.State == ext.StateRunning
	})

	if err := mgr.SendViewEvent("t3-sbx-pick", "main", uiblock.Event{Node: "/pick", Kind: uiblock.EventTap}); err != nil {
		t.Fatalf("SendViewEvent(pick): %v", err)
	}
	info := e2e4WaitExtState(t, mgr, "t3-sbx-pick", 25*time.Second, func(i ext.ExtensionInfo) bool {
		return i.State == ext.StateRunning && len(i.AllowedFolders) > 0
	})
	if len(info.AllowedFolders) == 0 {
		t.Fatalf("expected AllowedFolders populated after PickFolder")
	}

	subCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	ch, err := mgr.SubscribeView(subCtx, "t3-sbx-pick", "main", "en")
	if err != nil {
		t.Fatalf("SubscribeView after restart: %v", err)
	}
	var curTree *uiblock.Node
	var lastStatus *uiblock.Node
	for {
		select {
		case upd, ok := <-ch:
			if !ok {
				t.Fatalf("SubscribeView closed before mounted sentinel text arrived (last status=%+v)", lastStatus)
			}
			if upd.Tree != nil {
				curTree = upd.Tree
			} else if len(upd.Ops) > 0 && curTree != nil {
				if next, err := uiblock.Apply(curTree, upd.Ops); err == nil {
					curTree = next
				}
			}
			if curTree != nil {
				if n := uiblock.NodeOf(curTree, "/status"); n != nil {
					lastStatus = n
					if strings.Contains(n.Text, "hello-from-host-folder") {
						return
					}
				}
			}
		case <-subCtx.Done():
			t.Fatalf("timed out waiting for restarted sandbox mount sentinel text (last status=%+v)", lastStatus)
		}
	}
}

func TestT3_04_SDKSecretsSettingsMaskingAndChanged(t *testing.T) {
	dataDir := newShortTempDataDir(t)
	mgr, h := newTestManager(t, WithDataDir(dataDir), WithDeveloperMode(true))

	src := `package main

import (
	"context"
	"fmt"
	"sync"

	"github.com/ironpark/gvx-desktop/pkg/extsdk"
)

func main() {
	var mu sync.Mutex
	summary := "unset"
	app := extsdk.New()
	app.View("main", extsdk.View{
		Render: func(ctx *extsdk.ViewContext, ui *extsdk.UI) extsdk.Node {
			mu.Lock()
			s := summary
			mu.Unlock()
			return ui.Column(ui.Text(s, extsdk.Key("summary")))
		},
	})
	app.OnSettingsChanged(func(ctx *extsdk.Context, _ map[string]any) error {
		set, err := ctx.Host.Settings(ctx)
		if err != nil {
			return err
		}
		plainSecret, ok, err := ctx.Host.SecretGet(ctx, "setting:api_token")
		if err != nil {
			return err
		}
		mu.Lock()
		summary = fmt.Sprintf("url=%s|masked=%v|rawLen=%d|secret=%s|ok=%v",
			set.String("api_url"), set.IsSet("api_token"), len(set.String("api_token")), plainSecret, ok)
		mu.Unlock()
		ctx.NotifyChanged("main")
		return nil
	})
	_ = app.Run(context.Background())
}
`
	manifest := binaryManifestTOML("t3-sec", "0.1.0", "bin/ext", "secrets") + `
[contributes.settings]
api_url = { type = "string", label = "API URL", default = "https://default.invalid" }
api_token = { type = "secret", label = "API Token" }
`
	extDir := e2e4CreateBinaryExt(t, t.TempDir(), "t3-sec", manifest, src)
	if _, err := mgr.InstallLocal(extDir); err != nil {
		t.Fatalf("InstallLocal: %v", err)
	}
	if err := mgr.AllowAndBuild("t3-sec"); err != nil {
		t.Fatalf("AllowAndBuild: %v", err)
	}
	e2e4WaitExtState(t, mgr, "t3-sec", 10*time.Second, func(i ext.ExtensionInfo) bool {
		return i.State == ext.StateRunning
	})

	if err := mgr.SetSetting("t3-sec", "api_url", "https://api.example.invalid"); err != nil {
		t.Fatalf("SetSetting(api_url): %v", err)
	}
	if err := mgr.SetSetting("t3-sec", "api_token", "tok-super-secret"); err != nil {
		t.Fatalf("SetSetting(api_token): %v", err)
	}

	subCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	ch, err := mgr.SubscribeView(subCtx, "t3-sec", "main", "en")
	if err != nil {
		t.Fatalf("SubscribeView: %v", err)
	}
	want := "url=https://api.example.invalid|masked=true|rawLen=0|secret=tok-super-secret|ok=true"
	var curTree *uiblock.Node
	for {
		select {
		case upd := <-ch:
			if upd.Tree != nil {
				curTree = upd.Tree
			} else if len(upd.Ops) > 0 && curTree != nil {
				next, err := uiblock.Apply(curTree, upd.Ops)
				if err != nil {
					t.Fatalf("uiblock.Apply: %v", err)
				}
				curTree = next
			}
			if curTree != nil {
				if n := uiblock.NodeOf(curTree, "/summary"); n != nil && n.Text == want {
					goto checkDisk
				}
			}
		case <-subCtx.Done():
			t.Fatalf("timed out waiting for %q (last=%+v)", want, curTree)
		}
	}

checkDisk:
	rawSettings, err := os.ReadFile(ext.SettingsPath(dataDir, "t3-sec"))
	if err != nil {
		t.Fatalf("read settings.json: %v", err)
	}
	if strings.Contains(string(rawSettings), "tok-super-secret") {
		t.Fatalf("secret leaked into settings.json: %s", string(rawSettings))
	}
	foundInKC := false
	for _, entry := range h.Keychain.Entries() {
		if entry[1] == "setting:api_token" {
			foundInKC = true
		}
	}
	if !foundInKC {
		t.Fatalf("expected setting:api_token in keychain.Fake entries, got %+v", h.Keychain.Entries())
	}
}

func TestT3_05_SDKStoragePersistenceDisableEnableIdle(t *testing.T) {
	dataDir := newShortTempDataDir(t)
	mgr, _ := newTestManager(t, WithDataDir(dataDir), WithDeveloperMode(true))
	mgrCtx, mgrCancel := context.WithCancel(context.Background())
	defer mgrCancel()
	if err := mgr.Start(mgrCtx); err != nil {
		t.Fatalf("mgr.Start: %v", err)
	}

	src := `package main

import (
	"context"

	"github.com/ironpark/gvx-desktop/pkg/extsdk"
)

func main() {
	app := extsdk.New()
	app.Command("save", func(ctx *extsdk.CommandContext) error {
		return ctx.Host.StorageSet(ctx, "persist_key", "v1-persisted")
	})
	app.View("main", extsdk.View{
		Render: func(ctx *extsdk.ViewContext, ui *extsdk.UI) extsdk.Node {
			val, ok, err := ctx.Host.StorageGet(ctx, "persist_key")
			if err != nil {
				val = "err:" + err.Error()
			} else if !ok {
				val = "missing"
			}
			return ui.Column(ui.Text(val, extsdk.Key("kv")))
		},
	})
	_ = app.Run(context.Background())
}
`
	manifest := strings.Replace(
		binaryManifestTOML("t3-kv", "0.1.0", "bin/ext", "storage"),
		"dev_entrypoint = \"bin/ext\"",
		"dev_entrypoint = \"bin/ext\"\nidle_timeout_sec = 1",
		1,
	) + "\n[[contributes.command]]\nid = \"save\"\ntitle = \"Save KV\"\n"
	extDir := e2e4CreateBinaryExt(t, t.TempDir(), "t3-kv", manifest, src)
	if _, err := mgr.InstallLocal(extDir); err != nil {
		t.Fatalf("InstallLocal: %v", err)
	}
	if err := mgr.AllowAndBuild("t3-kv"); err != nil {
		t.Fatalf("AllowAndBuild: %v", err)
	}
	e2e4WaitExtState(t, mgr, "t3-kv", 10*time.Second, func(i ext.ExtensionInfo) bool {
		return i.State == ext.StateRunning
	})

	if err := mgr.RunCommand("t3-kv", "save", "en"); err != nil {
		t.Fatalf("RunCommand(save): %v", err)
	}

	// Wait for idle auto-stop.
	e2e4WaitExtState(t, mgr, "t3-kv", 10*time.Second, func(i ext.ExtensionInfo) bool {
		return !i.State.IsActive()
	})

	// Re-subscribing view wakes extension and reads persisted KV value.
	subCtx1, cancel1 := context.WithTimeout(context.Background(), 10*time.Second)
	ch1, err := mgr.SubscribeView(subCtx1, "t3-kv", "main", "en")
	if err != nil {
		cancel1()
		t.Fatalf("SubscribeView wake: %v", err)
	}
	select {
	case upd := <-ch1:
		if n := uiblock.NodeOf(upd.Tree, "/kv"); n == nil || n.Text != "v1-persisted" {
			cancel1()
			t.Fatalf("after idle wake, kv = %+v, want v1-persisted", n)
		}
	case <-subCtx1.Done():
		cancel1()
		t.Fatal("timed out waiting for tree on idle wake")
	}
	cancel1()

	// Disable -> Enable preserves KV storage across lifecycle stop/start.
	if err := mgr.Disable("t3-kv"); err != nil {
		t.Fatalf("Disable: %v", err)
	}
	e2e4WaitExtState(t, mgr, "t3-kv", 5*time.Second, func(i ext.ExtensionInfo) bool {
		return i.State == ext.StateDisabled
	})
	if err := mgr.Enable("t3-kv"); err != nil {
		t.Fatalf("Enable: %v", err)
	}
	e2e4WaitExtState(t, mgr, "t3-kv", 10*time.Second, func(i ext.ExtensionInfo) bool {
		return i.State == ext.StateRunning
	})

	subCtx2, cancel2 := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel2()
	ch2, err := mgr.SubscribeView(subCtx2, "t3-kv", "main", "en")
	if err != nil {
		t.Fatalf("SubscribeView after Enable: %v", err)
	}
	select {
	case upd := <-ch2:
		if n := uiblock.NodeOf(upd.Tree, "/kv"); n == nil || n.Text != "v1-persisted" {
			t.Fatalf("after Disable/Enable, kv = %+v, want v1-persisted", n)
		}
	case <-subCtx2.Done():
		t.Fatal("timed out waiting for tree after Enable")
	}
}

func TestT3_06_DockerScopeAllowedFoldersAndLabelInjection(t *testing.T) {
	dataDir := newShortTempDataDir(t)
	mgr, h := newTestManager(t, WithDataDir(dataDir), WithDeveloperMode(true))

	approvedDir := e2e4NonReservedDir(t)
	h.UIHost.PickFolderResult = approvedDir

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

	"github.com/ironpark/gvx-desktop/pkg/extsdk"
)

func dockerPost(ctx context.Context, payload map[string]any) (int, string) {
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
	body, _ := json.Marshal(payload)
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, base+"/containers/create", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	resp, err := (&http.Client{Transport: tr}).Do(req)
	if err != nil {
		return 0, err.Error()
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(raw)
}

func main() {
	app := extsdk.New()
	app.View("main", extsdk.View{
		Render: func(ctx *extsdk.ViewContext, ui *extsdk.UI) extsdk.Node {
			return ui.Column(ui.Button("Pick", extsdk.Key("pick"), extsdk.OnTap("pick")))
		},
		On: extsdk.Handlers{
			"pick": func(ctx *extsdk.EventContext) error {
				dir, ok, err := ctx.Host.PickFolder(ctx)
				if err != nil || !ok {
					return err
				}
				return ctx.Host.StorageSet(ctx, "dir", dir)
			},
		},
	})
	app.Command("create-allowed", func(ctx *extsdk.CommandContext) error {
		dir, _, _ := ctx.Host.StorageGet(ctx, "dir")
		code, body := dockerPost(ctx, map[string]any{
			"Image": "alpine:3.20",
			"HostConfig": map[string]any{
				"Binds": []string{dir + ":/work"},
			},
		})
		if code != 201 {
			return fmt.Errorf("create-allowed status=%d body=%s", code, body)
		}
		return nil
	})
	app.Command("create-files", func(ctx *extsdk.CommandContext) error {
		code, body := dockerPost(ctx, map[string]any{
			"Image": "alpine:3.20",
			"HostConfig": map[string]any{
				"Mounts": []map[string]any{
					{"Type": "bind", "Source": ctx.DataDir, "Target": "/extdata"},
				},
			},
		})
		if code != 201 {
			return fmt.Errorf("create-files status=%d body=%s", code, body)
		}
		return nil
	})
	app.Command("create-etc", func(ctx *extsdk.CommandContext) error {
		code, body := dockerPost(ctx, map[string]any{
			"Image": "alpine:3.20",
			"HostConfig": map[string]any{
				"Binds": []string{"/etc:/hostetc"},
			},
		})
		if code < 400 {
			return fmt.Errorf("expected /etc bind rejection, got %d: %s", code, body)
		}
		return nil
	})
	app.Command("create-spoof", func(ctx *extsdk.CommandContext) error {
		code, body := dockerPost(ctx, map[string]any{
			"Image":  "alpine:3.20",
			"Labels": map[string]any{"io.gvx.extension": "victim-ext"},
		})
		if code < 400 {
			return fmt.Errorf("expected label spoof rejection, got %d: %s", code, body)
		}
		return nil
	})
	_ = app.Run(context.Background())
}
`
	manifest := binaryManifestTOML("t3-dock", "0.1.0", "bin/ext", "docker", "ui:pick-folder", "storage") + `
[[contributes.command]]
id = "create-allowed"
title = "Create Allowed"
[[contributes.command]]
id = "create-files"
title = "Create Files"
[[contributes.command]]
id = "create-etc"
title = "Create Etc"
[[contributes.command]]
id = "create-spoof"
title = "Create Spoof"
`
	extDir := e2e4CreateBinaryExt(t, t.TempDir(), "t3-dock", manifest, src)
	if _, err := mgr.InstallLocal(extDir); err != nil {
		t.Fatalf("InstallLocal: %v", err)
	}
	if err := mgr.AllowAndBuild("t3-dock"); err != nil {
		t.Fatalf("AllowAndBuild: %v", err)
	}
	e2e4WaitExtState(t, mgr, "t3-dock", 10*time.Second, func(i ext.ExtensionInfo) bool {
		return i.State == ext.StateRunning
	})

	if err := mgr.SendViewEvent("t3-dock", "main", uiblock.Event{Node: "/pick", Kind: uiblock.EventTap}); err != nil {
		t.Fatalf("SendViewEvent(pick): %v", err)
	}
	for _, cmd := range []string{"create-allowed", "create-files", "create-etc", "create-spoof"} {
		if err := mgr.RunCommand("t3-dock", cmd, "en"); err != nil {
			t.Fatalf("RunCommand(%s): %v", cmd, err)
		}
	}

	var createReqs []DockerRequest
	for _, r := range h.DockerBackend.SnapshotRequests() {
		if r.Method == http.MethodPost && strings.HasSuffix(r.Path, "/containers/create") {
			createReqs = append(createReqs, r)
		} else if !(r.Method == http.MethodGet && strings.HasPrefix(r.Path, "/images/") && strings.HasSuffix(r.Path, "/json")) {
			t.Fatalf("unexpected forwarded Docker request: %+v", r)
		}
	}
	if len(createReqs) != 2 {
		t.Fatalf("expected exactly 2 forwarded /containers/create requests (blocked /etc and spoof), got %d: %+v", len(createReqs), createReqs)
	}
	for i, r := range createReqs {
		labels, _ := r.CreatePayload["Labels"].(map[string]any)
		if labels["io.gvx.extension"] != "t3-dock" {
			t.Fatalf("req[%d] missing injected label io.gvx.extension=t3-dock: %+v", i, r.CreatePayload)
		}
	}
}

func TestT3_07_DockerScopeKeepImagesPruneProtection(t *testing.T) {
	dataDir := newShortTempDataDir(t)
	builder := &e2e4ImageBuilder{}
	mgr, _ := e2e4NewManager(t, builder, WithDataDir(dataDir))

	localRoot := t.TempDir()
	keptDir := createLocalExtensionDir(t, localRoot, "t3-kept", minimalManifestTOML("t3-kept", "0.1.0", "docker"), nil)
	rmDir := createLocalExtensionDir(t, localRoot, "t3-removed", minimalManifestTOML("t3-removed", "0.1.0", "docker"), nil)

	if _, err := mgr.InstallLocal(keptDir); err != nil {
		t.Fatalf("InstallLocal(kept): %v", err)
	}
	_ = mgr.AllowAndBuild("t3-kept")
	if len(builder.SnapshotCalls()) != 1 {
		t.Fatalf("expected 1 ImageBuilder call for t3-kept, got %d", len(builder.SnapshotCalls()))
	}
	if _, err := mgr.InstallLocal(rmDir); err != nil {
		t.Fatalf("InstallLocal(removed): %v", err)
	}
	_ = mgr.AllowAndBuild("t3-removed")
	if len(builder.SnapshotCalls()) != 2 {
		t.Fatalf("expected 2 ImageBuilder calls after t3-removed, got %d", len(builder.SnapshotCalls()))
	}
	if err := mgr.Uninstall("t3-removed", true); err != nil {
		t.Fatalf("Uninstall(removed): %v", err)
	}

	keptTag := ext.ExtensionImageTag("t3-kept", "0.1.0", "")
	removedTag := ext.ExtensionImageTag("t3-removed", "0.1.0", "")

	runner := e2e4NewFakeEngineRunner()
	runner.SeedImage("sha256:"+strings.Repeat("1", 64), "gvx-ext/t3-kept", "0.1.0-local")
	runner.SeedImage("sha256:"+strings.Repeat("2", 64), "gvx-ext/t3-removed", "0.1.0-local")
	runner.SeedImage("sha256:"+strings.Repeat("3", 64), "unused/junk", "1.0")

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
		t.Fatalf("image prune HTTP %d: %s", rec.code, rec.body.String())
	}

	rem := runner.RemainingRefs()
	if len(rem) != 1 || rem[0] != keptTag {
		t.Fatalf("remaining images = %v, want [%s] (removedTag=%s)", rem, keptTag, removedTag)
	}
}

type e2e4MemRespRecorder struct {
	header http.Header
	code   int
	body   bytes.Buffer
}

func (r *e2e4MemRespRecorder) Header() http.Header { return r.header }
func (r *e2e4MemRespRecorder) WriteHeader(c int)   { r.code = c }
func (r *e2e4MemRespRecorder) Write(p []byte) (int, error) {
	if r.code == 0 {
		r.code = http.StatusOK
	}
	return r.body.Write(p)
}

func TestT3_08_GitInstallApprovalBuildSandboxPageExt(t *testing.T) {
	dataDir := newShortTempDataDir(t)
	builder := &e2e4ImageBuilder{LogLines: []string{"step 1: FROM alpine:3.20"}}
	mgr, _ := e2e4NewManager(t, builder, WithDataDir(dataDir))

	bareURL, sha := initBareGitRepo(t, map[string]string{
		"extension.toml": minimalManifestTOML("t3-gitbuild", "0.1.0", "storage"),
		"Dockerfile":     "FROM alpine:3.20\nCMD [\"/bin/sh\"]\n",
		".dockerignore":  "ignored.txt\n",
		"ignored.txt":    "should-not-pack",
	})
	httpsURL := e2e4ServeGitHTTPS(t, strings.TrimPrefix(bareURL, "file://"))

	info, err := mgr.InstallGit(httpsURL, "main")
	if err != nil {
		t.Fatalf("InstallGit: %v", err)
	}
	if info.State != ext.StateWaiting {
		t.Fatalf("expected pre-build StateWaiting, got %s", info.State)
	}
	if calls := builder.SnapshotCalls(); len(calls) != 0 {
		t.Fatalf("expected 0 builds before approval, got %d", len(calls))
	}

	_ = mgr.AllowAndBuild("t3-gitbuild")
	calls := builder.SnapshotCalls()
	if len(calls) != 1 {
		t.Fatalf("expected 1 build after AllowAndBuild, got %d", len(calls))
	}
	wantTag := ext.ExtensionImageTag("t3-gitbuild", "0.1.0", sha)
	if len(calls[0].Req.Tags) == 0 || calls[0].Req.Tags[0] != wantTag {
		t.Fatalf("build tags = %v, want %q", calls[0].Req.Tags, wantTag)
	}
	if calls[0].Req.Labels["io.gvx.extension"] != "t3-gitbuild" {
		t.Fatalf("build labels = %+v", calls[0].Req.Labels)
	}
	if bytes.Contains(calls[0].ContextTar, []byte("should-not-pack")) {
		t.Fatal("build context tar included .dockerignore file")
	}
	_ = mgr.Close(3 * time.Second)

	// Verify PageExtension routes/renders cleanly in ui.Model (no panic, shows extension card).
	p, m := e2e4StartProbeUI(t, dataDir, false, ggui.Sz(1280, 840))
	m.Go("ext:t3-gitbuild/main")
	sem := e2e4WaitSemantics(t, p, 5*time.Second, func(sem *ggui.SemTree) bool {
		return e2e4SemContains(sem, "t3-gitbuild") &&
			e2e4SemContains(sem, "Main") &&
			!e2e4SemContains(sem, "is not installed") &&
			!e2e4SemContains(sem, "Extension not found")
	})
	if e2e4SemContains(sem, "is not installed") || e2e4SemContains(sem, "Extension not found") {
		t.Fatalf("PageExtension rendered not-installed fallback:\n%s", sem)
	}

	t.Run("rungv_sandbox_live", func(t *testing.T) {
		binPath, _, _ := requireRungv(t)
		sbxDataDir := newShortTempDataDir(t)
		sup := e2e4NewRungvSupervisor(t, filepath.Join(sbxDataDir, "sbx"), binPath)

		eng := engine.New(engine.Config{
			Dir:    filepath.Join(sbxDataDir, "eng"),
			Runner: engine.NewRunner(func() *rungv.Client { return &rungv.Client{Path: binPath} }, filepath.Join(sbxDataDir, "eng-run")),
		})
		if err := eng.Open(context.Background()); err != nil {
			t.Fatalf("eng.Open: %v", err)
		}
		t.Cleanup(func() { _ = eng.Close(5 * time.Second) })

		src := `package main
import ("context"; "github.com/ironpark/gvx-desktop/pkg/extsdk")
func main() {
	app := extsdk.New()
	app.View("main", extsdk.View{Render: func(c *extsdk.ViewContext, ui *extsdk.UI) extsdk.Node {
		return ui.Column(ui.Text("rungv-git-ok", extsdk.Key("ok")))
	}})
	_ = app.Run(context.Background())
}`
		linuxBin := e2e4BuildLinuxSDKBinary(t, src)
		binBytes, _ := os.ReadFile(linuxBin)
		gitURL, _ := initBareGitRepo(t, map[string]string{
			"extension.toml": minimalManifestTOML("t3-git-live", "0.1.0"),
			"Dockerfile":     "FROM alpine:3.20\nCOPY ext /usr/local/bin/ext\nRUN chmod +x /usr/local/bin/ext\nEXPOSE 18900\nENTRYPOINT [\"/usr/local/bin/ext\"]\n",
			"ext":            string(binBytes),
		})
		liveHTTPS := e2e4ServeGitHTTPS(t, strings.TrimPrefix(gitURL, "file://"))
		sbxMgr, _ := e2e4NewManager(t, ext.NewEngineImageBuilder(eng.Handler), WithDataDir(sbxDataDir), WithSupervisor(sup))
		if _, err := sbxMgr.InstallGit(liveHTTPS, "main"); err != nil {
			t.Fatalf("InstallGit(live): %v", err)
		}
		if err := sbxMgr.AllowAndBuild("t3-git-live"); err != nil {
			t.Fatalf("AllowAndBuild(live): %v", err)
		}
		e2e4WaitExtState(t, sbxMgr, "t3-git-live", 25*time.Second, func(i ext.ExtensionInfo) bool {
			return i.State == ext.StateRunning
		})
	})
}

func TestT3_09_GitUpdatePermissionEscalationReapprove(t *testing.T) {
	dataDir := newShortTempDataDir(t)
	mgr, _ := newTestManager(t, WithDataDir(dataDir), WithDeveloperMode(true))

	src := `package main
import ("context"; "github.com/ironpark/gvx-desktop/pkg/extsdk")
func main() {
	app := extsdk.New()
	app.View("main", extsdk.View{Render: func(c *extsdk.ViewContext, ui *extsdk.UI) extsdk.Node {
		return ui.Column(ui.Text("git-escalate-ok", extsdk.Key("ok")))
	}})
	_ = app.Run(context.Background())
}`
	binData, err := os.ReadFile(buildFixtureSDKBinary(t, src))
	if err != nil {
		t.Fatalf("read fixture binary: %v", err)
	}
	bareURL, _ := e2e4InitBareGitRepoWithExec(t, map[string]string{
		"extension.toml": binaryManifestTOML("t3-escalate", "0.1.0", "bin/ext", "storage"),
		"bin/ext":        string(binData),
	}, "bin/ext")
	httpsURL := e2e4ServeGitHTTPS(t, strings.TrimPrefix(bareURL, "file://"))

	if _, err := mgr.InstallGit(httpsURL, "main"); err != nil {
		t.Fatalf("InstallGit v1: %v", err)
	}
	if err := mgr.AllowAndBuild("t3-escalate"); err != nil {
		t.Fatalf("AllowAndBuild v1: %v", err)
	}
	e2e4WaitExtState(t, mgr, "t3-escalate", 10*time.Second, func(i ext.ExtensionInfo) bool {
		return i.State == ext.StateRunning
	})

	workDir := filepath.Join(t.TempDir(), "work")
	commitToBareRepo(t, bareURL, workDir, map[string]string{
		"extension.toml": binaryManifestTOML("t3-escalate", "0.1.1", "bin/ext", "storage", "docker"),
	})

	updated, err := mgr.InstallGit(httpsURL, "main")
	if err != nil {
		t.Fatalf("InstallGit update: %v", err)
	}
	if updated.State != ext.StateWaiting {
		t.Fatalf("expected StateWaiting on permission escalation, got %s", updated.State)
	}
	if updated.State.IsActive() {
		t.Fatal("updated extension must stop before re-approval")
	}
	if !strings.Contains(strings.Join(updated.Asked, ","), "docker") {
		t.Fatalf("expected docker in Asked permissions, got %v", updated.Asked)
	}
	if err := mgr.Enable("t3-escalate"); err == nil {
		t.Fatal("expected Enable to fail while escalated permission is unapproved")
	}
	if err := mgr.AllowAndBuild("t3-escalate"); err != nil {
		t.Fatalf("AllowAndBuild v2: %v", err)
	}
	e2e4WaitExtState(t, mgr, "t3-escalate", 10*time.Second, func(i ext.ExtensionInfo) bool {
		return i.State == ext.StateRunning
	})
	if info, ok := e2e4FindExtInfo(mgr, "t3-escalate"); !ok || len(info.Asked) != 0 {
		t.Fatalf("expected zero Asked permissions after re-approval, got %+v", info)
	}
}

func TestT3_10_LocalInstallDevModeBinaryBadgeLogs(t *testing.T) {
	dataDir := newShortTempDataDir(t)
	src := `package main

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/ironpark/gvx-desktop/pkg/extsdk"
)

func main() {
	line := strings.Repeat("log-payload-0123456789abcdef", 64)
	for i := 0; i < 40; i++ {
		fmt.Fprintf(os.Stderr, "[%02d] %s\n", i, line)
	}
	app := extsdk.New()
	app.View("main", extsdk.View{
		Render: func(ctx *extsdk.ViewContext, ui *extsdk.UI) extsdk.Node {
			return ui.Column(ui.Text("binary-ui-ready", extsdk.Key("ready")))
		},
	})
	_ = app.Run(context.Background())
}
`
	extDir := e2e4CreateBinaryExt(t, t.TempDir(), "t3-devmode", binaryManifestTOML("t3-devmode", "0.1.0", "bin/ext"), src)

	// DeveloperMode = false must refuse starting binary runtime.
	mgrOff, _ := newTestManager(t, WithDataDir(dataDir), WithDeveloperMode(false))
	if _, err := mgrOff.InstallLocal(extDir); err != nil {
		t.Fatalf("InstallLocal: %v", err)
	}
	if err := mgrOff.AllowAndBuild("t3-devmode"); err == nil {
		info, _ := e2e4FindExtInfo(mgrOff, "t3-devmode")
		if info.State == ext.StateRunning {
			t.Fatal("binary runtime must not run when DeveloperMode is false")
		}
	}
	_ = mgrOff.Close(3 * time.Second)

	// Re-open with DeveloperMode = true; binary runs with Unsandboxed=true and logs written.
	mgrOn, _ := newTestManager(t, WithDataDir(dataDir), WithDeveloperMode(true))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	_ = mgrOn.Start(ctx)
	_ = mgrOn.AllowAndBuild("t3-devmode")
	info := e2e4WaitExtState(t, mgrOn, "t3-devmode", 10*time.Second, func(i ext.ExtensionInfo) bool {
		return i.State == ext.StateRunning
	})
	if !info.Unsandboxed {
		t.Fatalf("expected Unsandboxed=true for binary runtime, got %+v", info)
	}
	logsDir := ext.LogsDir(dataDir, "t3-devmode")
	entries, err := os.ReadDir(logsDir)
	if err != nil || len(entries) == 0 {
		t.Fatalf("expected rotated/captured log files in %s (err=%v entries=%v)", logsDir, err, entries)
	}
	_ = mgrOn.Close(3 * time.Second)

	// UI renders Dev · Unsandboxed indicator on PageExtension.
	p, m := e2e4StartProbeUI(t, dataDir, true, ggui.Sz(1280, 840))
	m.Go("ext:t3-devmode/main")
	e2e4WaitSemantics(t, p, 10*time.Second, func(sem *ggui.SemTree) bool {
		return e2e4SemContains(sem, "binary-ui-ready") && e2e4SemContains(sem, "Unsandboxed")
	})
}

func TestT3_11_SandboxNetworkNoneVsNatAndHostInfo(t *testing.T) {
	dataDir := newShortTempDataDir(t)
	mgr, _ := newTestManager(t,
		WithDataDir(dataDir),
		WithDeveloperMode(true),
		WithHostInfo(ext.HostInfo{
			AppVersion: "0.10.1-e2e4",
			OS:         runtime.GOOS,
			Arch:       runtime.GOARCH,
			Locale:     "ko",
			Theme:      "light",
			DockerMode: "sandbox",
		}),
	)

	binSrc := `package main

import (
	"context"
	"fmt"

	"github.com/ironpark/gvx-desktop/pkg/extsdk"
)

func main() {
	app := extsdk.New()
	app.View("main", extsdk.View{
		Render: func(ctx *extsdk.ViewContext, ui *extsdk.UI) extsdk.Node {
			hi, err := ctx.Host.Info(ctx)
			if err != nil {
				return ui.Column(ui.Text("err:"+err.Error(), extsdk.Key("info")))
			}
			return ui.Column(ui.Text(fmt.Sprintf("%s|%s|%s", hi.AppVersion, hi.Locale, hi.Theme), extsdk.Key("info")))
		},
	})
	_ = app.Run(context.Background())
}
`
	extDir := e2e4CreateBinaryExt(t, t.TempDir(), "t3-hostinfo", binaryManifestTOML("t3-hostinfo", "0.1.0", "bin/ext"), binSrc)
	if _, err := mgr.InstallLocal(extDir); err != nil {
		t.Fatalf("InstallLocal: %v", err)
	}
	if err := mgr.AllowAndBuild("t3-hostinfo"); err != nil {
		t.Fatalf("AllowAndBuild: %v", err)
	}
	e2e4WaitExtState(t, mgr, "t3-hostinfo", 10*time.Second, func(i ext.ExtensionInfo) bool {
		return i.State == ext.StateRunning
	})
	subCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	ch, err := mgr.SubscribeView(subCtx, "t3-hostinfo", "main", "ko")
	if err != nil {
		t.Fatalf("SubscribeView: %v", err)
	}
	select {
	case upd := <-ch:
		if n := uiblock.NodeOf(upd.Tree, "/info"); n == nil || n.Text != "0.10.1-e2e4|ko|light" {
			t.Fatalf("HostInfo text = %+v, want 0.10.1-e2e4|ko|light", n)
		}
	case <-subCtx.Done():
		t.Fatal("timed out waiting for HostInfo view")
	}

	t.Run("rungv_net_none_vs_nat", func(t *testing.T) {
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

		ln, err := net.Listen("tcp", "0.0.0.0:0")
		if err != nil {
			t.Fatalf("listen: %v", err)
		}
		defer ln.Close()
		go func() {
			for {
				c, err := ln.Accept()
				if err != nil {
					return
				}
				_ = c.Close()
			}
		}()

		hostPort := ln.Addr().(*net.TCPAddr).Port
		guestTarget := fmt.Sprintf("10.0.2.2:%d", hostPort)
		probeSrc := fmt.Sprintf(`package main
import ("context"; "fmt"; "net"; "time"; "github.com/ironpark/gvx-desktop/pkg/extsdk")
func main() {
	app := extsdk.New()
	app.View("main", extsdk.View{Render: func(c *extsdk.ViewContext, ui *extsdk.UI) extsdk.Node {
		hi, herr := c.Host.Info(c)
		conn, derr := net.DialTimeout("tcp", %q, 800*time.Millisecond)
		dialOK := derr == nil
		if conn != nil { _ = conn.Close() }
		return ui.Column(ui.Text(fmt.Sprintf("host=%%v|dial=%%v|ver=%%s", herr == nil, dialOK, hi.AppVersion), extsdk.Key("net")))
	}})
	_ = app.Run(context.Background())
}`, guestTarget)

		linuxBin := e2e4BuildLinuxSDKBinary(t, probeSrc)
		binBytes, _ := os.ReadFile(linuxBin)
		sbxMgr, _ := e2e4NewManager(t, ext.NewEngineImageBuilder(eng.Handler), WithDataDir(sbxDir), WithSupervisor(sup))

		for _, tc := range []struct {
			id       string
			perms    []string
			wantDial bool
		}{
			{"net-none", nil, false},
			{"net-nat", []string{"network"}, true},
		} {
			dir := filepath.Join(t.TempDir(), tc.id)
			writeExtFixture(t, dir, minimalManifestTOML(tc.id, "0.1.0", tc.perms...), map[string]string{
				"Dockerfile": "FROM alpine:3.20\nCOPY ext /usr/local/bin/ext\nRUN chmod +x /usr/local/bin/ext\nEXPOSE 18900\nENTRYPOINT [\"/usr/local/bin/ext\"]\n",
				"ext":        string(binBytes),
			})
			if _, err := sbxMgr.InstallLocal(dir); err != nil {
				t.Fatalf("InstallLocal(%s): %v", tc.id, err)
			}
			if err := sbxMgr.AllowAndBuild(tc.id); err != nil {
				t.Fatalf("AllowAndBuild(%s): %v", tc.id, err)
			}
			e2e4WaitExtState(t, sbxMgr, tc.id, 25*time.Second, func(i ext.ExtensionInfo) bool {
				return i.State == ext.StateRunning
			})
			sCtx, sCancel := context.WithTimeout(context.Background(), 10*time.Second)
			vch, err := sbxMgr.SubscribeView(sCtx, tc.id, "main", "en")
			if err != nil {
				sCancel()
				t.Fatalf("SubscribeView(%s): %v", tc.id, err)
			}
			upd := <-vch
			sCancel()
			n := uiblock.NodeOf(upd.Tree, "/net")
			wantStr := fmt.Sprintf("host=true|dial=%v|", tc.wantDial)
			if n == nil || !strings.HasPrefix(n.Text, wantStr) {
				t.Fatalf("%s network probe = %+v, want prefix %q", tc.id, n, wantStr)
			}
		}
	})
}

func TestT3_12_SandboxesSnapshotsIsolationAndDockerSock(t *testing.T) {
	dataDir := newShortTempDataDir(t)
	st := newTestStore(t, dataDir)
	userSpec := sandbox.Spec{
		ID:    "user-sbx-1",
		Name:  "user-sbx-1",
		Image: "alpine:3.20",
	}
	if err := st.SaveSandboxes([]sandbox.Spec{userSpec}); err != nil {
		t.Fatalf("SaveSandboxes: %v", err)
	}
	sup := e2e4NewRungvSupervisor(t, filepath.Join(dataDir, "sbx"), "")
	mgr, _ := newTestManager(t, WithDataDir(dataDir), WithDeveloperMode(true), WithSupervisor(sup))

	srcB := `package main

import (
	"context"
	"fmt"

	"github.com/ironpark/gvx-desktop/pkg/extsdk"
)

func main() {
	app := extsdk.New()
	app.Command("probe", func(ctx *extsdk.CommandContext) error {
		set, err := ctx.Host.Settings(ctx)
		if err != nil {
			return fmt.Errorf("settings: %w", err)
		}
		targetID := set.String("target_sbx")
		if targetID == "" {
			targetID = "user-sbx-1"
		}
		list, err := ctx.Host.SandboxList(ctx, true)
		if err != nil {
			return fmt.Errorf("list: %w", err)
		}
		for _, item := range list {
			if item.ID == targetID {
				return fmt.Errorf("foreign sandbox %s leaked into owned list %+v", targetID, list)
			}
		}
		if _, err := ctx.Host.SandboxStop(ctx, targetID); !extsdk.IsPermissionDenied(err) {
			return fmt.Errorf("expected 403 stopping unowned sandbox %s, got %v", targetID, err)
		}
		if _, err := ctx.Host.SnapshotCreate(ctx, targetID, extsdk.CreateSnapshotRequest{Name: "s1"}); !extsdk.IsPermissionDenied(err) {
			return fmt.Errorf("expected 403 snapshotting unowned sandbox %s, got %v", targetID, err)
		}
		if err := ctx.Host.SandboxDelete(ctx, targetID); !extsdk.IsPermissionDenied(err) {
			return fmt.Errorf("expected 403 deleting unowned sandbox %s, got %v", targetID, err)
		}
		if targetID == "user-sbx-1" {
			if _, err := ctx.Host.SandboxStart(ctx, targetID); !extsdk.IsPermissionDenied(err) {
				return fmt.Errorf("expected 403 starting user sandbox, got %v", err)
			}
			if _, err := ctx.Host.SandboxCreate(ctx, extsdk.CreateSandboxRequest{
				Name:            "sock-test",
				Image:           "alpine:3.20",
				MountDockerSock: true,
			}); !extsdk.IsPermissionDenied(err) {
				return fmt.Errorf("expected 403 for MountDockerSock without docker perm, got %v", err)
			}
		}
		return nil
	})
	_ = app.Run(context.Background())
}
`
	manifestB := binaryManifestTOML("t3-sbx-b", "0.1.0", "bin/ext", "sandboxes", "snapshots") + `
[[contributes.command]]
id = "probe"
title = "Probe"

[contributes.settings]
target_sbx = { type = "string", label = "Target Sandbox", default = "user-sbx-1" }
`
	extBDir := e2e4CreateBinaryExt(t, t.TempDir(), "t3-sbx-b", manifestB, srcB)
	if _, err := mgr.InstallLocal(extBDir); err != nil {
		t.Fatalf("InstallLocal(b): %v", err)
	}
	if err := mgr.AllowAndBuild("t3-sbx-b"); err != nil {
		t.Fatalf("AllowAndBuild(b): %v", err)
	}
	e2e4WaitExtState(t, mgr, "t3-sbx-b", 10*time.Second, func(i ext.ExtensionInfo) bool {
		return i.State == ext.StateRunning
	})
	if err := mgr.RunCommand("t3-sbx-b", "probe", "en"); err != nil {
		t.Fatalf("RunCommand(probe): %v", err)
	}

	t.Run("rungv_owned_snapshot_isolation", func(t *testing.T) {
		requireRungv(t)
		srcA := `package main
import ("context"; "fmt"; "os"; "path/filepath"; "github.com/ironpark/gvx-desktop/pkg/extsdk")
func main() {
	app := extsdk.New()
	app.Command("create", func(ctx *extsdk.CommandContext) error {
		sb, err := ctx.Host.SandboxCreate(ctx, extsdk.CreateSandboxRequest{
			Name: "ext-a-sbx", Image: "alpine:3.20", Command: "sleep 30", Ephemeral: true,
		})
		if err != nil { return err }
		if err := os.WriteFile(filepath.Join(ctx.DataDir, "sb_id.txt"), []byte(sb.ID), 0o600); err != nil {
			return err
		}
		if err := ctx.Host.StorageSet(ctx, "sb_id", sb.ID); err != nil { return err }
		if _, err := ctx.Host.SnapshotCreate(ctx, sb.ID, extsdk.CreateSnapshotRequest{Name: "cp1", LeaveRunning: true}); err != nil {
			return fmt.Errorf("snap: %w", err)
		}
		return nil
	})
	_ = app.Run(context.Background())
}`
		manifestA := binaryManifestTOML("t3-sbx-a", "0.1.0", "bin/ext", "sandboxes", "snapshots", "storage", "docker") +
			"\n[[contributes.command]]\nid = \"create\"\ntitle = \"Create\"\n"
		extADir := e2e4CreateBinaryExt(t, t.TempDir(), "t3-sbx-a", manifestA, srcA)
		if _, err := mgr.InstallLocal(extADir); err != nil {
			t.Fatalf("InstallLocal(a): %v", err)
		}
		if err := mgr.AllowAndBuild("t3-sbx-a"); err != nil {
			t.Fatalf("AllowAndBuild(a): %v", err)
		}
		e2e4WaitExtState(t, mgr, "t3-sbx-a", 10*time.Second, func(i ext.ExtensionInfo) bool {
			return i.State == ext.StateRunning
		})
		if err := mgr.RunCommand("t3-sbx-a", "create", "en"); err != nil {
			t.Fatalf("RunCommand(a create): %v", err)
		}
		sbIDBytes, err := os.ReadFile(filepath.Join(ext.FilesDir(dataDir, "t3-sbx-a"), "sb_id.txt"))
		if err != nil || strings.TrimSpace(string(sbIDBytes)) == "" {
			t.Fatalf("read ext-a sandbox ID: %v", err)
		}
		extASbxID := strings.TrimSpace(string(sbIDBytes))
		if err := mgr.SetSetting("t3-sbx-b", "target_sbx", extASbxID); err != nil {
			t.Fatalf("SetSetting(target_sbx): %v", err)
		}
		if err := mgr.RunCommand("t3-sbx-b", "probe", "en"); err != nil {
			t.Fatalf("ext-b cross-extension probe against %s failed: %v", extASbxID, err)
		}
	})
}

func TestT3_13_LocalProxyWebUIPageExtRevoke(t *testing.T) {
	dataDir := newShortTempDataDir(t)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()
	port := ln.Addr().(*net.TCPAddr).Port
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "web-ui-ok")
	})}
	go func() { _ = srv.Serve(ln) }()
	defer srv.Close()

	mgr, _ := newTestManager(t, WithDataDir(dataDir), WithDeveloperMode(true))
	src := `package main
import ("context"; "github.com/ironpark/gvx-desktop/pkg/extsdk")
func main() {
	app := extsdk.New()
	app.View("main", extsdk.View{Render: func(c *extsdk.ViewContext, ui *extsdk.UI) extsdk.Node {
		return ui.Column(ui.Text("webui-view-ready"))
	}})
	_ = app.Run(context.Background())
}`
	manifest := binaryManifestTOML("t3-web", "0.1.0", "bin/ext", "localproxy") + fmt.Sprintf(`
[contributes.web_ui]
port = %d
path = "/ui"
title = "Web Dashboard"
`, port)
	extDir := e2e4CreateBinaryExt(t, t.TempDir(), "t3-web", manifest, src)
	if _, err := mgr.InstallLocal(extDir); err != nil {
		t.Fatalf("InstallLocal: %v", err)
	}
	if err := mgr.AllowAndBuild("t3-web"); err != nil {
		t.Fatalf("AllowAndBuild: %v", err)
	}
	e2e4WaitExtState(t, mgr, "t3-web", 10*time.Second, func(i ext.ExtensionInfo) bool {
		return i.State == ext.StateRunning
	})

	routes := mgr.LocalProxyRoutes()
	foundRoute := false
	for _, rt := range routes {
		if strings.Contains(rt.Host, "t3-web") && strings.HasSuffix(rt.Host, ".localhost") {
			foundRoute = true
		}
	}
	if !foundRoute {
		t.Fatalf("expected localproxy route for t3-web, got %+v", routes)
	}
	_ = mgr.Close(3 * time.Second)

	p, m := e2e4StartProbeUI(t, dataDir, true, ggui.Sz(1280, 840))
	m.SetLocalProxyPorts(e2e4FreeLoopbackPort(t), e2e4FreeLoopbackPort(t))
	m.SetLocalProxy(true)
	m.Go("ext:t3-web/main")
	e2e4WaitSemantics(t, p, 10*time.Second, func(sem *ggui.SemTree) bool {
		return e2e4SemContains(sem, "webui-view-ready") && e2e4SemContains(sem, "Open in Browser")
	})
	m.CloseExtensions(5 * time.Second)

	// Re-open manager and verify RevokePermissions stops extension and removes route.
	mgr2, _ := newTestManager(t, WithDataDir(dataDir), WithDeveloperMode(true))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	_ = mgr2.Start(ctx)
	e2e4WaitExtState(t, mgr2, "t3-web", 10*time.Second, func(i ext.ExtensionInfo) bool {
		return i.State == ext.StateRunning
	})
	if err := mgr2.RevokePermissions("t3-web"); err != nil {
		t.Fatalf("RevokePermissions: %v", err)
	}
	e2e4WaitExtState(t, mgr2, "t3-web", 5*time.Second, func(i ext.ExtensionInfo) bool {
		return !i.State.IsActive()
	})
	for _, rt := range mgr2.LocalProxyRoutes() {
		if strings.Contains(rt.Host, "t3-web") {
			t.Fatalf("expected t3-web localproxy route removed after RevokePermissions, got %+v", rt)
		}
	}
}

func TestT3_14_CommandPaletteGestureOpenURLCopy(t *testing.T) {
	dataDir := newShortTempDataDir(t)
	mgr, h := newTestManager(t, WithDataDir(dataDir), WithDeveloperMode(true))

	src := `package main

import (
	"context"
	"fmt"

	"github.com/ironpark/gvx-desktop/pkg/extsdk"
)

func main() {
	app := extsdk.New()
	app.View("main", extsdk.View{Render: func(c *extsdk.ViewContext, ui *extsdk.UI) extsdk.Node {
		return ui.Column(ui.Text("cmd-ready"))
	}})
	app.Command("open-docs", func(ctx *extsdk.CommandContext) error {
		if err := ctx.Host.OpenURL(ctx, "https://example.invalid/docs"); err != nil {
			return err
		}
		return ctx.Host.Copy(ctx, "docs-copied")
	})
	app.Command("bad-url", func(ctx *extsdk.CommandContext) error {
		if err := ctx.Host.OpenURL(ctx, "javascript:alert(1)"); err == nil {
			return fmt.Errorf("expected javascript: URL to be rejected")
		}
		// Strip gesture and verify rejection.
		noGesture := context.Background()
		if err := ctx.Host.OpenURL(noGesture, "https://example.invalid/no-gesture"); !extsdk.IsGestureRequired(err) {
			return fmt.Errorf("expected gesture_required, got %v", err)
		}
		return nil
	})
	_ = app.Run(context.Background())
}
`
	manifest := binaryManifestTOML("t3-cmd", "0.1.0", "bin/ext", "ui:open-url") + `
[[contributes.command]]
id = "open-docs"
title = "T3 Open Docs"
[[contributes.command]]
id = "bad-url"
title = "T3 Bad URL"
`
	extDir := e2e4CreateBinaryExt(t, t.TempDir(), "t3-cmd", manifest, src)
	if _, err := mgr.InstallLocal(extDir); err != nil {
		t.Fatalf("InstallLocal: %v", err)
	}
	if err := mgr.AllowAndBuild("t3-cmd"); err != nil {
		t.Fatalf("AllowAndBuild: %v", err)
	}
	e2e4WaitExtState(t, mgr, "t3-cmd", 10*time.Second, func(i ext.ExtensionInfo) bool {
		return i.State == ext.StateRunning
	})

	if err := mgr.RunCommand("t3-cmd", "open-docs", "en"); err != nil {
		t.Fatalf("RunCommand(open-docs): %v", err)
	}
	if err := mgr.RunCommand("t3-cmd", "bad-url", "en"); err != nil {
		t.Fatalf("RunCommand(bad-url): %v", err)
	}
	if urls := h.UIHost.SnapshotOpenURLs(); len(urls) != 1 || urls[0] != "https://example.invalid/docs" {
		t.Fatalf("OpenURLs = %v, want [https://example.invalid/docs]", urls)
	}
	if copied := h.UIHost.SnapshotCopied(); len(copied) != 1 || copied[0] != "docs-copied" {
		t.Fatalf("Copied = %v, want [docs-copied]", copied)
	}
	_ = mgr.Close(3 * time.Second)

	// Verify command appears in ⌘K palette in UI.
	p, m := e2e4StartProbeUI(t, dataDir, true, ggui.Sz(1280, 840))
	e2e4WaitSemantics(t, p, 10*time.Second, func(sem *ggui.SemTree) bool {
		return e2e4SemContains(sem, "t3-cmd") || e2e4SemContains(sem, "Main")
	})
	m.PaletteQuery.Set("")
	m.PaletteOpen.Set(true)
	e2e4WaitSemantics(t, p, 5*time.Second, func(sem *ggui.SemTree) bool {
		_, ok := sem.Find(ggui.RoleMenuItem, "T3 Open Docs")
		return ok
	})
}

func TestT3_15_ChromeToolbarLiftToolbarDialogProvenance(t *testing.T) {
	dataDir := newShortTempDataDir(t)
	mgr, _ := newTestManager(t, WithDataDir(dataDir), WithDeveloperMode(true))

	src := `package main
import ("context"; "github.com/ironpark/gvx-desktop/pkg/extsdk")
func main() {
	app := extsdk.New()
	app.View("main", extsdk.View{
		Chrome: "toolbar",
		Render: func(c *extsdk.ViewContext, ui *extsdk.UI) extsdk.Node {
			return ui.Column(
				ui.Toolbar(
					ui.Button("Lifted Refresh", extsdk.Key("refresh"), extsdk.OnTap("refresh")),
				),
				ui.Text("body-content"),
				ui.Dialog("Confirm Action", ui.Text("Proceed with operation?")).Variant("danger"),
			).Fill(true)
		},
		On: extsdk.Handlers{"refresh": func(ctx *extsdk.EventContext) error { return nil }},
	})
	_ = app.Run(context.Background())
}`
	manifest := `schema_version = 1
id = "t3-chrome"
version = "0.1.0"
publisher = "acme"
name = "Chrome Ext"
description = "Toolbar & dialog provenance"
icon = "box"

[runtime]
type = "binary"
dev_entrypoint = "bin/ext"

[[contributes.view]]
id = "main"
title = "Main"
icon = "box"
chrome = "toolbar"
`
	extDir := e2e4CreateBinaryExt(t, t.TempDir(), "t3-chrome", manifest, src)
	if _, err := mgr.InstallLocal(extDir); err != nil {
		t.Fatalf("InstallLocal: %v", err)
	}
	if err := mgr.AllowAndBuild("t3-chrome"); err != nil {
		t.Fatalf("AllowAndBuild: %v", err)
	}
	_ = mgr.Close(3 * time.Second)

	p, m := e2e4StartProbeUI(t, dataDir, true, ggui.Sz(1280, 840))
	m.Go("ext:t3-chrome/main")
	sem := e2e4WaitSemantics(t, p, 10*time.Second, func(s *ggui.SemTree) bool {
		return e2e4SemContains(s, "Lifted Refresh") && e2e4SemContains(s, "Confirm Action")
	})
	if !e2e4SemContains(sem, "t3-chrome asks") {
		t.Fatalf("expected dialog provenance 't3-chrome asks' in semantics:\n%s", sem)
	}
}

func TestT3_16_ToastRateLimitBadgeSidebarChanged(t *testing.T) {
	dataDir := newShortTempDataDir(t)
	mgr, h := newTestManager(t, WithDataDir(dataDir), WithDeveloperMode(true))

	src := `package main

import (
	"context"
	"fmt"

	"github.com/ironpark/gvx-desktop/pkg/extsdk"
)

func main() {
	app := extsdk.New()
	app.View("main", extsdk.View{Render: func(c *extsdk.ViewContext, ui *extsdk.UI) extsdk.Node {
		return ui.Column(ui.Text("badge-ready"))
	}})
	app.Command("burst", func(ctx *extsdk.CommandContext) error {
		for i := 0; i < 12; i++ {
			if err := ctx.Host.Toast(ctx, "info", fmt.Sprintf("toast-%d", i)); err != nil {
				return fmt.Errorf("toast %d failed: %w", i, err)
			}
		}
		if err := ctx.Host.Toast(ctx, "info", "toast-12"); !extsdk.IsRateLimited(err) {
			return fmt.Errorf("expected 13th toast rate limited (429), got %v", err)
		}
		return ctx.Host.SetBadge(ctx, extsdk.BadgeRequest{Text: "7", Tone: "warning", Tooltip: "7 alerts"})
	})
	_ = app.Run(context.Background())
}
`
	manifest := binaryManifestTOML("t3-badge", "0.1.0", "bin/ext") +
		"\n[[contributes.command]]\nid = \"burst\"\ntitle = \"Burst\"\n"
	extDir := e2e4CreateBinaryExt(t, t.TempDir(), "t3-badge", manifest, src)
	if _, err := mgr.InstallLocal(extDir); err != nil {
		t.Fatalf("InstallLocal: %v", err)
	}
	if err := mgr.AllowAndBuild("t3-badge"); err != nil {
		t.Fatalf("AllowAndBuild: %v", err)
	}
	e2e4WaitExtState(t, mgr, "t3-badge", 10*time.Second, func(i ext.ExtensionInfo) bool {
		return i.State == ext.StateRunning
	})

	since := h.UIHost.DrainChanged()
	if err := mgr.RunCommand("t3-badge", "burst", "en"); err != nil {
		t.Fatalf("RunCommand(burst): %v", err)
	}
	h.UIHost.WaitChangedAfter(t, since, 5*time.Second)

	if toasts := h.UIHost.SnapshotToasts(); len(toasts) != 12 {
		t.Fatalf("toasts len = %d, want 12", len(toasts))
	}
	info, ok := e2e4FindExtInfo(mgr, "t3-badge")
	if !ok || info.Badge.Text != "7" {
		t.Fatalf("expected ExtensionInfo.Badge.Text == '7', got %+v", info)
	}
	_ = mgr.Close(3 * time.Second)

	p, _ := e2e4StartProbeUI(t, dataDir, true, ggui.Sz(1280, 840))
	e2e4WaitSemantics(t, p, 10*time.Second, func(sem *ggui.SemTree) bool {
		return e2e4SemContains(sem, "t3-badge") || e2e4SemContains(sem, "Main")
	})
}

func TestT3_17_FileFallbackToKeychainMigrationAndPurge(t *testing.T) {
	dataDir := newShortTempDataDir(t)
	src := `package main

import (
	"context"
	"fmt"

	"github.com/ironpark/gvx-desktop/pkg/extsdk"
)

func main() {
	app := extsdk.New()
	app.Command("put", func(ctx *extsdk.CommandContext) error {
		return ctx.Host.SecretSet(ctx, "api_tok", "s3cr3t-value")
	})
	app.Command("check", func(ctx *extsdk.CommandContext) error {
		v, ok, err := ctx.Host.SecretGet(ctx, "api_tok")
		if err != nil || !ok || v != "s3cr3t-value" {
			return fmt.Errorf("SecretGet = (%q, %v, %v)", v, ok, err)
		}
		return nil
	})
	_ = app.Run(context.Background())
}
`
	manifest := binaryManifestTOML("t3-mig", "0.1.0", "bin/ext", "secrets") + `
[[contributes.command]]
id = "put"
title = "Put"
[[contributes.command]]
id = "check"
title = "Check"
`
	extDir := e2e4CreateBinaryExt(t, t.TempDir(), "t3-mig", manifest, src)

	// Phase 1: Keychain unavailable -> 0600 secrets.json fallback.
	mgr1, _ := newTestManager(t, WithDataDir(dataDir), WithKeychain(nil, "Keychain unavailable"), WithDeveloperMode(true))
	if _, err := mgr1.InstallLocal(extDir); err != nil {
		t.Fatalf("InstallLocal: %v", err)
	}
	if err := mgr1.AllowAndBuild("t3-mig"); err != nil {
		t.Fatalf("AllowAndBuild: %v", err)
	}
	e2e4WaitExtState(t, mgr1, "t3-mig", 10*time.Second, func(i ext.ExtensionInfo) bool {
		return i.State == ext.StateRunning
	})
	if err := mgr1.RunCommand("t3-mig", "put", "en"); err != nil {
		t.Fatalf("RunCommand(put): %v", err)
	}
	secFile := ext.SecretsFilePath(dataDir, "t3-mig")
	assertFilePerm(t, secFile, 0o600)
	_ = mgr1.Close(3 * time.Second)

	// Phase 2: Keychain returns -> auto-migrates secrets.json into fakeKC and deletes secrets.json.
	fakeKC := newFakeKeychain(t)
	mgr2, _ := newTestManager(t, WithDataDir(dataDir), WithKeychain(fakeKC, ""), WithDeveloperMode(true))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	_ = mgr2.Start(ctx)
	e2e4WaitExtState(t, mgr2, "t3-mig", 10*time.Second, func(i ext.ExtensionInfo) bool {
		return i.State == ext.StateRunning
	})
	if err := mgr2.RunCommand("t3-mig", "check", "en"); err != nil {
		t.Fatalf("RunCommand(check): %v", err)
	}
	if _, err := os.Stat(secFile); !os.IsNotExist(err) {
		t.Fatalf("expected fallback %s to be deleted after migration, err=%v", secFile, err)
	}
	if len(fakeKC.Entries()) == 0 {
		t.Fatal("expected migrated secret inside fakeKC")
	}

	// Phase 3: Uninstall(purge=true) removes all entries from keychain and data dir.
	if err := mgr2.Uninstall("t3-mig", true); err != nil {
		t.Fatalf("Uninstall(purge=true): %v", err)
	}
	if len(fakeKC.Entries()) != 0 {
		t.Fatalf("expected keychain purged after Uninstall(true), got %+v", fakeKC.Entries())
	}
	if _, err := os.Stat(ext.ExtensionDataDir(dataDir, "t3-mig")); !os.IsNotExist(err) {
		t.Fatalf("expected extension-data removed after purge, err=%v", err)
	}
}

func TestT3_18_LongDataDirSunPathFallbackSandboxIPC(t *testing.T) {
	longParent := filepath.Join(t.TempDir(), strings.Repeat("nested-dir-", 12))
	if err := os.MkdirAll(longParent, 0o700); err != nil {
		t.Fatalf("mkdir longParent: %v", err)
	}
	if len(longParent) <= 120 {
		t.Fatalf("expected >120 char dataDir, got %d", len(longParent))
	}

	sockDir, err := ext.SocketDir(longParent)
	if err != nil {
		t.Fatalf("ext.SocketDir: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(sockDir) })
	assertDirPerm(t, sockDir, 0o700)

	paths := ext.NewPaths(longParent)
	sockPath, err := paths.SocketPath("h", "t3-long-ipc")
	if err != nil {
		t.Fatalf("Paths.SocketPath: %v", err)
	}
	if len(sockPath) > ext.MaxUnixSocketPathLen {
		t.Fatalf("socket path %q len %d exceeds %d", sockPath, len(sockPath), ext.MaxUnixSocketPathLen)
	}

	mgr, _ := newTestManager(t, WithDataDir(longParent), WithDeveloperMode(true))
	src := `package main
import ("context"; "github.com/ironpark/gvx-desktop/pkg/extsdk")
func main() {
	app := extsdk.New()
	app.View("main", extsdk.View{Render: func(c *extsdk.ViewContext, ui *extsdk.UI) extsdk.Node {
		_, err := c.Host.Info(c)
		if err != nil { return ui.Column(ui.Text("err:"+err.Error(), extsdk.Key("st"))) }
		return ui.Column(ui.Text("long-ipc-ok", extsdk.Key("st")))
	}})
	_ = app.Run(context.Background())
}`
	extDir := e2e4CreateBinaryExt(t, t.TempDir(), "t3-long-ipc", binaryManifestTOML("t3-long-ipc", "0.1.0", "bin/ext"), src)
	if _, err := mgr.InstallLocal(extDir); err != nil {
		t.Fatalf("InstallLocal: %v", err)
	}
	if err := mgr.AllowAndBuild("t3-long-ipc"); err != nil {
		t.Fatalf("AllowAndBuild: %v", err)
	}
	e2e4WaitExtState(t, mgr, "t3-long-ipc", 10*time.Second, func(i ext.ExtensionInfo) bool {
		return i.State == ext.StateRunning
	})

	t.Run("rungv_long_datadir_unix_forward", func(t *testing.T) {
		binPath, _, _ := requireRungv(t)
		sup := e2e4NewRungvSupervisor(t, filepath.Join(longParent, "sbx"), binPath)
		eng := engine.New(engine.Config{
			Dir:    filepath.Join(longParent, "eng"),
			Runner: engine.NewRunner(func() *rungv.Client { return &rungv.Client{Path: binPath} }, filepath.Join(longParent, "eng-run")),
		})
		if err := eng.Open(context.Background()); err != nil {
			t.Fatalf("eng.Open: %v", err)
		}
		t.Cleanup(func() { _ = eng.Close(5 * time.Second) })

		linuxBin := e2e4BuildLinuxSDKBinary(t, src)
		binBytes, _ := os.ReadFile(linuxBin)
		sbxDir := filepath.Join(t.TempDir(), "t3-long-sbx")
		writeExtFixture(t, sbxDir, minimalManifestTOML("t3-long-sbx", "0.1.0"), map[string]string{
			"Dockerfile": "FROM alpine:3.20\nCOPY ext /usr/local/bin/ext\nRUN chmod +x /usr/local/bin/ext\nEXPOSE 18900\nENTRYPOINT [\"/usr/local/bin/ext\"]\n",
			"ext":        string(binBytes),
		})
		sbxMgr, _ := e2e4NewManager(t, ext.NewEngineImageBuilder(eng.Handler), WithDataDir(longParent), WithSupervisor(sup))
		if _, err := sbxMgr.InstallLocal(sbxDir); err != nil {
			t.Fatalf("InstallLocal: %v", err)
		}
		if err := sbxMgr.AllowAndBuild("t3-long-sbx"); err != nil {
			t.Fatalf("AllowAndBuild: %v", err)
		}
		e2e4WaitExtState(t, sbxMgr, "t3-long-sbx", 25*time.Second, func(i ext.ExtensionInfo) bool {
			return i.State == ext.StateRunning
		})
	})
}

func TestT3_19_LoopbackTCPBearerFallbackSDKRoundtrip(t *testing.T) {
	hostToken := e2e4RandomHexToken(t)
	extToken := e2e4RandomHexToken(t)

	hostLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen host tcp: %v", err)
	}
	defer hostLn.Close()

	hostMux := http.NewServeMux()
	hostMux.HandleFunc("GET /v1/host/info", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+hostToken {
			http.Error(w, `{"code":"unauthorized"}`, http.StatusUnauthorized)
			return
		}
		_ = json.NewEncoder(w).Encode(extsdk.HostInfo{
			AppVersion: "0.10.1-win",
			OS:         "windows",
			Arch:       "amd64",
			Locale:     "en",
			Theme:      "dark",
			DockerMode: "dind",
		})
	})
	hostSrv := &http.Server{Handler: hostMux}
	go func() { _ = hostSrv.Serve(hostLn) }()
	defer hostSrv.Close()

	extLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen ext tcp: %v", err)
	}
	extAddr := extLn.Addr().String()

	app := extsdk.New()
	var count atomic.Int64
	app.View("main", extsdk.View{
		Render: func(ctx *extsdk.ViewContext, ui *extsdk.UI) extsdk.Node {
			hi, err := ctx.Host.Info(ctx)
			label := fmt.Sprintf("%s:%d", hi.OS, count.Load())
			if err != nil {
				label = "err:" + err.Error()
			}
			return ui.Column(
				ui.Text(label, extsdk.Key("lbl")),
				ui.Button("Bump", extsdk.Key("bump"), extsdk.OnTap("bump")),
			)
		},
		On: extsdk.Handlers{
			"bump": func(ctx *extsdk.EventContext) error {
				count.Add(1)
				ctx.NotifyChanged()
				return nil
			},
		},
	})

	runCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	errCh := make(chan error, 1)
	go func() {
		errCh <- app.RunWithConfig(runCtx, extsdk.Config{
			ExtID:      "t3-tcp",
			HostAPI:    "http://" + hostLn.Addr().String(),
			HostToken:  hostToken,
			ExtToken:   extToken,
			Listener:   extLn,
			ListenAddr: extAddr,
		})
	}()

	// Verify 401 without Bearer on both Host and Ext TCP endpoints.
	code, _, _ := httpDoJSON(t, nil, http.MethodGet, "http://"+hostLn.Addr().String()+"/v1/host/info", "", "", nil, nil)
	if code != http.StatusUnauthorized {
		t.Fatalf("host unauth status = %d, want 401", code)
	}
	code, _, _ = httpDoJSON(t, nil, http.MethodGet, "http://"+extAddr+"/v1/health", "wrong-token", "", nil, nil)
	if code != http.StatusUnauthorized {
		t.Fatalf("ext bad-token status = %d, want 401", code)
	}

	// Open SSE stream over TCP loopback with Bearer token.
	sseReq, _ := http.NewRequestWithContext(runCtx, http.MethodGet, "http://"+extAddr+"/v1/views/main/stream", nil)
	sseReq.Header.Set("Authorization", "Bearer "+extToken)
	resp, err := http.DefaultClient.Do(sseReq)
	if err != nil {
		t.Fatalf("SSE connect: %v", err)
	}
	defer resp.Body.Close()
	sseCh := make(chan SSEEvent, 16)
	go readSSEEvents(runCtx, resp, sseCh)

	ev := awaitSSEEvent(t, sseCh, 5*time.Second, func(e SSEEvent) bool { return e.Event == "tree" })
	if !strings.Contains(ev.Data, "windows:0") {
		t.Fatalf("expected windows:0 in TCP loopback SSE tree, got %s", ev.Data)
	}

	code, _, _ = httpDoJSON(t, nil, http.MethodPost, "http://"+extAddr+"/v1/views/main/event", extToken, "", map[string]any{
		"node_id": "/bump",
		"kind":    "tap",
	}, nil)
	if code != http.StatusOK {
		t.Fatalf("event HTTP %d", code)
	}
	awaitSSEEvent(t, sseCh, 5*time.Second, func(e SSEEvent) bool {
		return strings.Contains(e.Data, "windows:1")
	})

	code, _, _ = httpDoJSON(t, nil, http.MethodPost, "http://"+extAddr+"/v1/shutdown", extToken, "", nil, nil)
	if code != http.StatusOK {
		t.Fatalf("shutdown HTTP %d", code)
	}
	select {
	case err := <-errCh:
		if err != nil {
			t.Fatalf("RunWithConfig returned error: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for TCP SDK shutdown")
	}
}

func TestT3_20_IdleTimeoutAutoStopStreamHoldAndWake(t *testing.T) {
	dataDir := newShortTempDataDir(t)
	mgr, _ := newTestManager(t, WithDataDir(dataDir), WithDeveloperMode(true))
	mgrCtx, mgrCancel := context.WithCancel(context.Background())
	defer mgrCancel()
	if err := mgr.Start(mgrCtx); err != nil {
		t.Fatalf("mgr.Start: %v", err)
	}

	src := `package main
import ("context"; "github.com/ironpark/gvx-desktop/pkg/extsdk")
func main() {
	app := extsdk.New()
	app.View("main", extsdk.View{Render: func(c *extsdk.ViewContext, ui *extsdk.UI) extsdk.Node {
		return ui.Column(ui.Text("idle-awake", extsdk.Key("st")))
	}})
	_ = app.Run(context.Background())
}`
	manifest := strings.Replace(
		binaryManifestTOML("t3-idle", "0.1.0", "bin/ext"),
		"dev_entrypoint = \"bin/ext\"",
		"dev_entrypoint = \"bin/ext\"\nidle_timeout_sec = 1",
		1,
	)
	extDir := e2e4CreateBinaryExt(t, t.TempDir(), "t3-idle", manifest, src)
	if _, err := mgr.InstallLocal(extDir); err != nil {
		t.Fatalf("InstallLocal: %v", err)
	}
	if err := mgr.AllowAndBuild("t3-idle"); err != nil {
		t.Fatalf("AllowAndBuild: %v", err)
	}
	e2e4WaitExtState(t, mgr, "t3-idle", 10*time.Second, func(i ext.ExtensionInfo) bool {
		return i.State == ext.StateRunning
	})

	// Active SSE view prevents idle auto-stop beyond idle_timeout_sec=1s.
	subCtx, cancel := context.WithCancel(context.Background())
	ch, err := mgr.SubscribeView(subCtx, "t3-idle", "main", "en")
	if err != nil {
		cancel()
		t.Fatalf("SubscribeView: %v", err)
	}
	<-ch
	time.Sleep(1600 * time.Millisecond)
	if info, ok := e2e4FindExtInfo(mgr, "t3-idle"); !ok || info.State != ext.StateRunning {
		cancel()
		t.Fatalf("expected active SSE stream to hold extension running, got %+v", info)
	}

	// Closing SSE stream allows idle auto-stop.
	cancel()
	e2e4WaitExtState(t, mgr, "t3-idle", 10*time.Second, func(i ext.ExtensionInfo) bool {
		return !i.State.IsActive()
	})

	// Opening new SubscribeView wakes extension back to StateRunning.
	wakeCtx, wakeCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer wakeCancel()
	ch2, err := mgr.SubscribeView(wakeCtx, "t3-idle", "main", "en")
	if err != nil {
		t.Fatalf("SubscribeView wake: %v", err)
	}
	select {
	case upd := <-ch2:
		if n := uiblock.NodeOf(upd.Tree, "/st"); n == nil || n.Text != "idle-awake" {
			t.Fatalf("wake tree = %+v", upd)
		}
	case <-wakeCtx.Done():
		t.Fatal("timed out waiting for wake view tree")
	}
}

func TestT3_21_CrashBackoffFailedCardAndRebuildRecovery(t *testing.T) {
	dataDir := newShortTempDataDir(t)
	mgr, _ := newTestManager(t, WithDataDir(dataDir), WithDeveloperMode(true))

	crashSrc := `package main
import ("fmt"; "os")
func main() {
	fmt.Fprintln(os.Stderr, "fatal startup boom")
	os.Exit(1)
}`
	extDir := e2e4CreateBinaryExt(t, t.TempDir(), "t3-crash", binaryManifestTOML("t3-crash", "0.1.0", "bin/ext"), crashSrc)
	if _, err := mgr.InstallLocal(extDir); err != nil {
		t.Fatalf("InstallLocal: %v", err)
	}
	_ = mgr.AllowAndBuild("t3-crash")

	e2e4WaitExtState(t, mgr, "t3-crash", 20*time.Second, func(i ext.ExtensionInfo) bool {
		return i.State == ext.StateFailed
	})
	_ = mgr.Close(3 * time.Second)

	// Verify PageExtension shows error/logs card safely without crashing UI.
	p, m := e2e4StartProbeUI(t, dataDir, true, ggui.Sz(1280, 840))
	m.Go("ext:t3-crash/main")
	e2e4WaitSemantics(t, p, 5*time.Second, func(sem *ggui.SemTree) bool {
		return e2e4SemContains(sem, "Reload") || e2e4SemContains(sem, "Logs") || e2e4SemContains(sem, "failed")
	})
	m.CloseExtensions(5 * time.Second)

	// Replace with healthy binary and recover via Rebuild/Enable.
	goodSrc := `package main
import ("context"; "github.com/ironpark/gvx-desktop/pkg/extsdk")
func main() {
	app := extsdk.New()
	app.View("main", extsdk.View{Render: func(c *extsdk.ViewContext, ui *extsdk.UI) extsdk.Node {
		return ui.Column(ui.Text("recovered-ok", extsdk.Key("ok")))
	}})
	_ = app.Run(context.Background())
}`
	goodBin := buildFixtureSDKBinary(t, goodSrc)
	goodBytes, _ := os.ReadFile(goodBin)
	if err := os.WriteFile(filepath.Join(extDir, "bin", "ext"), goodBytes, 0o755); err != nil {
		t.Fatalf("overwrite healthy binary: %v", err)
	}

	mgr2, _ := newTestManager(t, WithDataDir(dataDir), WithDeveloperMode(true))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	_ = mgr2.Start(ctx)
	if err := mgr2.Rebuild("t3-crash"); err != nil {
		t.Fatalf("Rebuild: %v", err)
	}
	e2e4WaitExtState(t, mgr2, "t3-crash", 10*time.Second, func(i ext.ExtensionInfo) bool {
		return i.State == ext.StateRunning
	})
}

func TestT3_22_SpoofingRejectionAndPrintableSanitization(t *testing.T) {
	// 1. Manifest spoofing rejection (bidi RLO U+202E, ZWSP U+200B, ESC \x1b).
	for _, bad := range []string{"bad\u202ename", "bad\u200bname", "bad\x1bname"} {
		badManifest := strings.Replace(minimalManifestTOML("t3-spoof", "0.1.0"), `name = "Test Extension t3-spoof"`, fmt.Sprintf("name = %q", bad), 1)
		if _, err := ext.ParseManifest([]byte(badManifest)); err == nil {
			t.Fatalf("expected ParseManifest to reject spoofing char in %q", bad)
		}
	}

	// 2. uiblock.Read spoofing rejection + host resilience on bad extension tree.
	badJSON := []byte(`{"kind":"column","children":[{"kind":"text","text":"evil\u202etext"}]}`)
	if _, err := uiblock.Read(badJSON); err == nil {
		t.Fatal("expected uiblock.Read to reject RLO U+202E")
	}

	dataDir := newShortTempDataDir(t)
	mgr, _ := newTestManager(t, WithDataDir(dataDir), WithDeveloperMode(true))
	src := `package main
import ("context"; "fmt"; "os"; "github.com/ironpark/gvx-desktop/pkg/extsdk")
func main() {
	fmt.Fprintln(os.Stderr, "raw-esc:\x1b[31mred\x1b[0m\u202e\tkept")
	app := extsdk.New()
	app.View("main", extsdk.View{Render: func(c *extsdk.ViewContext, ui *extsdk.UI) extsdk.Node {
		return ui.Column(ui.Text("bad\u202etree"))
	}})
	_ = app.Run(context.Background())
}`
	extDir := e2e4CreateBinaryExt(t, t.TempDir(), "t3-badtree", binaryManifestTOML("t3-badtree", "0.1.0", "bin/ext"), src)
	if _, err := mgr.InstallLocal(extDir); err != nil {
		t.Fatalf("InstallLocal: %v", err)
	}
	if err := mgr.AllowAndBuild("t3-badtree"); err != nil {
		t.Fatalf("AllowAndBuild: %v", err)
	}
	e2e4WaitExtState(t, mgr, "t3-badtree", 10*time.Second, func(i ext.ExtensionInfo) bool {
		return i.State == ext.StateRunning
	})

	subCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if ch, err := mgr.SubscribeView(subCtx, "t3-badtree", "main", "en"); err == nil {
		select {
		case upd := <-ch:
			if upd.Tree != nil && upd.Error == "" {
				t.Fatalf("expected spoofed tree to be rejected with error, got %+v", upd)
			}
		case <-subCtx.Done():
		}
	}

	// 3. Printable escapes spoofing/control chars while preserving \n and \t.
	sanitized := uiblock.Printable("ok\tline\n\x1b[31m\u202e\u200b")
	if strings.ContainsRune(sanitized, '\x1b') || strings.ContainsRune(sanitized, '\u202e') || strings.ContainsRune(sanitized, '\u200b') {
		t.Fatalf("Printable left raw spoofing runes: %q", sanitized)
	}
	if !strings.Contains(sanitized, `\x1b`) || !strings.Contains(sanitized, `\u202e`) || !strings.Contains(sanitized, "\t") || !strings.Contains(sanitized, "\n") {
		t.Fatalf("Printable output unexpected: %q", sanitized)
	}
}

func TestT3_23_MainShutdownCloseExtensionsStoppingEphemeral(t *testing.T) {
	// Part (a): AST check of main.go shutdown ordering.
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, filepath.Join(repoRoot(t), "main.go"), nil, 0)
	if err != nil {
		t.Fatalf("parse main.go: %v", err)
	}
	var callOrder []string
	for _, decl := range file.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Name.Name != "run" {
			continue
		}
		ast.Inspect(fn.Body, func(n ast.Node) bool {
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
				callOrder = append(callOrder, sel.Sel.Name)
			}
			return true
		})
	}
	idxTerms, idxFwds, idxExts, idxStop := -1, -1, -1, -1
	for i, name := range callOrder {
		switch name {
		case "CloseTerminals":
			idxTerms = i
		case "CloseForwards":
			idxFwds = i
		case "CloseExtensions":
			idxExts = i
		case "StopAll", "CloseSandboxEngine":
			if idxStop == -1 {
				idxStop = i
			}
		}
	}
	if idxExts == -1 || idxExts < idxTerms || idxExts < idxFwds || (idxStop != -1 && idxExts > idxStop) {
		t.Fatalf("main.go shutdown order invalid (want CloseTerminals/CloseForwards -> CloseExtensions -> StopAll/CloseSandboxEngine): %v", callOrder)
	}

	// Part (b): Runtime app.stopping SSE delivery + ephemeral sandbox cleanup + CloseExtensions clean shutdown.
	dataDir := newShortTempDataDir(t)
	src := `package main

import (
	"context"
	"os"
	"path/filepath"
	"sync"

	"github.com/ironpark/gvx-desktop/pkg/extsdk"
)

func main() {
	var once sync.Once
	var mu sync.Mutex
	ephLabel := "pending"

	app := extsdk.New()
	app.View("main", extsdk.View{Render: func(c *extsdk.ViewContext, ui *extsdk.UI) extsdk.Node {
		once.Do(func() {
			start := false
			sb, err := c.Host.SandboxCreate(c, extsdk.CreateSandboxRequest{
				Name:      "t3-eph-sbx",
				Image:     "alpine:3.20",
				Ephemeral: true,
				Start:     &start,
			})
			mu.Lock()
			if err != nil {
				ephLabel = "err:" + err.Error()
			} else {
				ephLabel = sb.ID
			}
			mu.Unlock()
		})
		mu.Lock()
		lbl := ephLabel
		mu.Unlock()
		return ui.Column(ui.Text("ready|eph="+lbl, extsdk.Key("st")))
	}})
	app.OnEvent(func(ctx *extsdk.Context, ev extsdk.HostEvent) {
		switch ev.Type {
		case "host.theme":
			_ = os.WriteFile(filepath.Join(ctx.DataDir, "events-ready"), []byte("1"), 0o600)
		case "app.stopping":
			_ = os.WriteFile(filepath.Join(ctx.DataDir, "saw-stopping"), []byte("1"), 0o600)
		}
	})
	_ = app.Run(context.Background())
}
`
	extDir := e2e4CreateBinaryExt(t, t.TempDir(), "t3-stop", binaryManifestTOML("t3-stop", "0.1.0", "bin/ext", "sandboxes"), src)
	sup := &sandbox.Supervisor{DataDir: dataDir}
	mgr, _ := newTestManager(t, WithDataDir(dataDir), WithDeveloperMode(true), WithSupervisor(sup))
	mgrCtx, mgrCancel := context.WithCancel(context.Background())
	defer mgrCancel()
	if err := mgr.Start(mgrCtx); err != nil {
		t.Fatalf("mgr.Start: %v", err)
	}
	if _, err := mgr.InstallLocal(extDir); err != nil {
		t.Fatalf("InstallLocal: %v", err)
	}
	if err := mgr.AllowAndBuild("t3-stop"); err != nil {
		t.Fatalf("AllowAndBuild: %v", err)
	}

	p, m := e2e4StartProbeUIWithManager(t, dataDir, mgr, true, ggui.Sz(1000, 700))
	m.Go("ext:t3-stop/main")
	e2e4WaitSemantics(t, p, 10*time.Second, func(sem *ggui.SemTree) bool {
		return e2e4SemContains(sem, "ready|eph=") && !e2e4SemContains(sem, "ready|eph=pending") && !e2e4SemContains(sem, "ready|eph=err:")
	})

	st := newTestStore(t, dataDir)
	beforeSpecs, err := st.Sandboxes()
	if err != nil {
		t.Fatalf("st.Sandboxes before CloseExtensions: %v", err)
	}
	if len(beforeSpecs) != 1 || beforeSpecs[0].Name != "t3-eph-sbx" {
		t.Fatalf("expected 1 ephemeral sandbox before CloseExtensions, got %+v", beforeSpecs)
	}

	// Deterministic handshake confirming GET /v1/events SSE stream is active before CloseExtensions.
	eventsReadyMarker := filepath.Join(ext.FilesDir(dataDir, "t3-stop"), "events-ready")
	readyDeadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(readyDeadline) {
		mgr.NotifyHostEvent("host.theme", map[string]string{"theme": "warmup"})
		if _, err := os.Stat(eventsReadyMarker); err == nil {
			break
		}
		time.Sleep(30 * time.Millisecond)
	}
	if _, err := os.Stat(eventsReadyMarker); err != nil {
		t.Fatalf("extension did not establish /v1/events subscription (%s): %v", eventsReadyMarker, err)
	}

	m.CloseExtensions(8 * time.Second)
	// Idempotent second call must return immediately without panic.
	m.CloseExtensions(8 * time.Second)

	marker := filepath.Join(ext.FilesDir(dataDir, "t3-stop"), "saw-stopping")
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("expected extension to receive app.stopping SSE event before exit (%s): %v", marker, err)
	}
	afterSpecs, err := st.Sandboxes()
	if err != nil {
		t.Fatalf("st.Sandboxes after CloseExtensions: %v", err)
	}
	if len(afterSpecs) != 0 {
		t.Fatalf("expected ephemeral sandbox cleaned up by CloseExtensions, remaining=%+v", afterSpecs)
	}
}

func TestT3_24_OpenTerminalGestureRoutingAndCloseTerminals(t *testing.T) {
	dataDir := newShortTempDataDir(t)
	st := newTestStore(t, dataDir)
	if err := st.SaveSandboxes([]sandbox.Spec{
		{
			ID:     "sb-1",
			Name:   "sb-1",
			Image:  "alpine:3.20",
			Labels: map[string]string{"io.gvx.extension": "t3-term"},
		},
		{
			ID:    "foreign-sb",
			Name:  "foreign-sb",
			Image: "alpine:3.20",
		},
	}); err != nil {
		t.Fatalf("SaveSandboxes: %v", err)
	}
	mgr, h := newTestManager(t, WithDataDir(dataDir), WithDeveloperMode(true))
	h.DockerBackend.SeedContainer("c-1", "t3-term")
	h.DockerBackend.SeedContainer("foreign-c", "other-ext")

	src := `package main

import (
	"context"
	"fmt"

	"github.com/ironpark/gvx-desktop/pkg/extsdk"
)

func main() {
	app := extsdk.New()
	app.View("main", extsdk.View{
		Render: func(c *extsdk.ViewContext, ui *extsdk.UI) extsdk.Node {
			return ui.Column(ui.Button("Open Terms", extsdk.Key("term"), extsdk.OnTap("open")))
		},
		On: extsdk.Handlers{
			"open": func(ctx *extsdk.EventContext) error {
				if err := ctx.Host.OpenTerminal(ctx, extsdk.OpenTerminalRequest{
					TargetType: "container", TargetID: "c-1", Shell: "/bin/sh",
				}); err != nil {
					return err
				}
				if err := ctx.Host.OpenTerminal(ctx, extsdk.OpenTerminalRequest{
					TargetType: "sandbox", TargetID: "sb-1", Shell: "/bin/sh",
				}); err != nil {
					return err
				}
				if err := ctx.Host.OpenTerminal(ctx, extsdk.OpenTerminalRequest{
					TargetType: "container", TargetID: "foreign-c", Shell: "/bin/sh",
				}); !extsdk.IsPermissionDenied(err) {
					return fmt.Errorf("expected permission_denied for foreign container, got %v", err)
				}
				if err := ctx.Host.OpenTerminal(ctx, extsdk.OpenTerminalRequest{
					TargetType: "sandbox", TargetID: "foreign-sb", Shell: "/bin/sh",
				}); !extsdk.IsPermissionDenied(err) {
					return fmt.Errorf("expected permission_denied for foreign sandbox, got %v", err)
				}
				if err := ctx.Host.OpenTerminal(context.Background(), extsdk.OpenTerminalRequest{
					TargetType: "sandbox", TargetID: "sb-no-gesture",
				}); !extsdk.IsGestureRequired(err) {
					return fmt.Errorf("expected gesture_required without gesture, got %v", err)
				}
				return nil
			},
		},
	})
	_ = app.Run(context.Background())
}
`
	extDir := e2e4CreateBinaryExt(t, t.TempDir(), "t3-term", binaryManifestTOML("t3-term", "0.1.0", "bin/ext", "ui:terminal", "docker", "sandboxes"), src)
	if _, err := mgr.InstallLocal(extDir); err != nil {
		t.Fatalf("InstallLocal: %v", err)
	}
	if err := mgr.AllowAndBuild("t3-term"); err != nil {
		t.Fatalf("AllowAndBuild: %v", err)
	}
	e2e4WaitExtState(t, mgr, "t3-term", 10*time.Second, func(i ext.ExtensionInfo) bool {
		return i.State == ext.StateRunning
	})

	if err := mgr.SendViewEvent("t3-term", "main", uiblock.Event{Node: "/term", Kind: uiblock.EventTap}); err != nil {
		t.Fatalf("SendViewEvent(open): %v", err)
	}
	terms := h.UIHost.SnapshotTerminals()
	if len(terms) != 2 || terms[0].TargetType != "container" || terms[0].TargetID != "c-1" || terms[1].TargetType != "sandbox" || terms[1].TargetID != "sb-1" {
		t.Fatalf("unexpected terminals recorded: %+v", terms)
	}
	_ = mgr.Close(3 * time.Second)

	// Verify ui.Model ShowSandbox / ShowContainer routing + CloseTerminals -> CloseExtensions order.
	p, m := e2e4StartProbeUI(t, dataDir, true, ggui.Sz(1280, 840))
	m.ShowSandbox("sb-1", 0)
	if got := ggui.Peek(m.Page); got != ui.PageSandboxes {
		t.Fatalf("ShowSandbox page = %q, want %q", got, ui.PageSandboxes)
	}
	m.CloseTerminals()
	m.CloseExtensions(8 * time.Second)
	p.Frame()
}
