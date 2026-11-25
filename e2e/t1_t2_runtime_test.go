//go:build e2e

package e2e

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ironpark/gvx-desktop/internal/ext"
	"github.com/ironpark/gvx-desktop/internal/recipes"
	"github.com/ironpark/gvx-desktop/internal/rungv"
	"github.com/ironpark/gvx-desktop/internal/sandbox"
	"github.com/ironpark/gvx-desktop/internal/uiblock"
	"github.com/ironpark/gvx-desktop/pkg/extsdk"
)

const e2e2RuntimeSDKSourceMinimal = `package main

import (
	"context"

	"github.com/ironpark/gvx-desktop/pkg/extsdk"
)

func main() {
	app := extsdk.New()
	app.View("main", extsdk.View{
		Render: func(ctx *extsdk.ViewContext, ui *extsdk.UI) extsdk.Node {
			return ui.Column(ui.Text("runtime-ready"))
		},
	})
	_ = app.Run(context.Background())
}
`

func e2e2RuntimeFindExtension(mgr *ext.Manager, id string) (ext.ExtensionInfo, bool) {
	for _, item := range mgr.List() {
		if item.ID == id {
			return item, true
		}
	}
	return ext.ExtensionInfo{}, false
}

func e2e2RuntimeMustExtension(t *testing.T, mgr *ext.Manager, id string) ext.ExtensionInfo {
	t.Helper()
	item, ok := e2e2RuntimeFindExtension(mgr, id)
	if !ok {
		t.Fatalf("extension %q not found in mgr.List()", id)
	}
	return item
}

func e2e2AwaitExtState(t *testing.T, mgr *ext.Manager, id string, want ext.ExtensionState, timeout time.Duration) ext.ExtensionInfo {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		info, ok := e2e2RuntimeFindExtension(mgr, id)
		if ok && info.State == want {
			return info
		}
		if time.Now().After(deadline) {
			if !ok {
				t.Fatalf("timed out after %v waiting for %q to appear with state %q", timeout, id, want)
			}
			t.Fatalf("timed out after %v waiting for %q state %q (last state=%q err=%q)", timeout, id, want, info.State, info.Error)
		}
		time.Sleep(25 * time.Millisecond)
	}
}

func e2e2NewBinaryExt(t *testing.T, mgr *ext.Manager, id, goSrc, extraTOML string, perms ...string) string {
	t.Helper()
	if goSrc == "" {
		goSrc = e2e2RuntimeSDKSourceMinimal
	}
	binPath := buildFixtureSDKBinary(t, goSrc)
	binData, err := os.ReadFile(binPath)
	if err != nil {
		t.Fatalf("read compiled binary: %v", err)
	}
	extDir := filepath.Join(t.TempDir(), id)
	manifest := binaryManifestTOML(id, "0.1.0", "bin/ext", perms...) + extraTOML
	writeExtFixture(t, extDir, manifest, nil)
	if err := os.MkdirAll(filepath.Join(extDir, "bin"), 0o700); err != nil {
		t.Fatalf("mkdir bin: %v", err)
	}
	if err := os.WriteFile(filepath.Join(extDir, "bin", "ext"), binData, 0o755); err != nil {
		t.Fatalf("write dev_entrypoint: %v", err)
	}
	if _, err := mgr.InstallLocal(extDir); err != nil {
		t.Fatalf("InstallLocal(%s): %v", id, err)
	}
	if err := mgr.AllowAndBuild(id); err != nil {
		t.Fatalf("AllowAndBuild(%s): %v", id, err)
	}
	return extDir
}

func e2e2RuntimeNonReservedDir(t *testing.T) string {
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
	custom, err := os.MkdirTemp(home, "gvx-e2e-rt-*")
	if err != nil {
		t.Fatalf("os.MkdirTemp in home: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(custom) })
	if resolved, err := filepath.EvalSymlinks(custom); err == nil {
		custom = resolved
	}
	return custom
}

type e2e2RungvHarness struct {
	Manager    *ext.Manager
	Harness    *TestHarness
	Supervisor *sandbox.Supervisor
	mu         sync.Mutex
	Specs      []sandbox.Spec
}

func (r *e2e2RungvHarness) snapshotSpecs() []sandbox.Spec {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]sandbox.Spec(nil), r.Specs...)
}

func e2e2NewRungvManager(t *testing.T, captureOnly bool) *e2e2RungvHarness {
	t.Helper()
	return e2e2NewRungvManagerWithDir(t, newShortTempDataDir(t), captureOnly)
}

func e2e2NewRungvManagerWithDir(t *testing.T, dir string, captureOnly bool) *e2e2RungvHarness {
	t.Helper()
	binPath, _, _ := requireRungv(t)
	if dir == "" {
		dir = newShortTempDataDir(t)
	}
	rh := &e2e2RungvHarness{}
	sup := &sandbox.Supervisor{
		Client:  func() *rungv.Client { return &rungv.Client{Path: binPath} },
		DataDir: dir,
		Launch: func(spec sandbox.Spec) (sandbox.Spec, func(), error) {
			rh.mu.Lock()
			rh.Specs = append(rh.Specs, spec)
			rh.mu.Unlock()
			if captureOnly {
				return spec, nil, errors.New("e2e2: spec captured")
			}
			if spec.Image == "alpine:3.20" {
				spec.Entrypoint = "/data/ext-bin"
				spec.SetEntrypoint = true
				spec.Command = ""
			}
			return spec, nil, nil
		},
	}
	t.Cleanup(func() {
		sup.StopAll(3 * time.Second)
	})
	mgr, h := newTestManager(t, WithDataDir(dir), WithSupervisor(sup))
	rh.Manager = mgr
	rh.Harness = h
	rh.Supervisor = sup
	return rh
}

func e2e2BuildLinuxSDKBinary(t *testing.T, sourceGoCode string) string {
	t.Helper()
	root := repoRoot(t)
	buildDir := t.TempDir()
	goMod := fmt.Sprintf("module example.com/e2elinuxfixture\n\ngo 1.27.0\n\nrequire github.com/ironpark/gvx-desktop v0.0.0\n\nreplace github.com/ironpark/gvx-desktop => %s\n", root)
	if err := os.WriteFile(filepath.Join(buildDir, "go.mod"), []byte(goMod), 0o644); err != nil {
		t.Fatalf("write go.mod: %v", err)
	}
	if err := os.WriteFile(filepath.Join(buildDir, "main.go"), []byte(sourceGoCode), 0o644); err != nil {
		t.Fatalf("write main.go: %v", err)
	}
	binPath := filepath.Join(buildDir, "ext-linux")
	env := []string{"GOOS=linux", "GOARCH=" + runtime.GOARCH}
	if out, err := runGo(t, buildDir, env, "build", "-o", binPath, "."); err != nil {
		t.Fatalf("e2e2BuildLinuxSDKBinary failed: %v\n%s", err, out)
	}
	return binPath
}

// =============================================================================
// Feature 13: IPC Socket Paths (sun_path & Windows Loopback)
// =============================================================================

func TestF13_T1_ShortSocketDirCreatedWith0700(t *testing.T) {
	dataDir := newTempDataDir(t)
	sockDir, err := ext.SocketDir(dataDir)
	if err != nil {
		t.Fatalf("ext.SocketDir: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(sockDir) })

	if !strings.HasPrefix(filepath.Base(sockDir), "gvxd-ext-") {
		t.Fatalf("unexpected socket dir name %q", filepath.Base(sockDir))
	}
	assertDirPerm(t, sockDir, 0o700)
}

func TestF13_T1_ShortSocketNamesFormatAndLength(t *testing.T) {
	const longID = "very-long-extension-identifier-covering-sixty-four-chars-0123456"
	dataDir := newTempDataDir(t)
	paths := ext.NewPaths(dataDir)
	sockDir, err := paths.SocketDir()
	if err != nil {
		t.Fatalf("paths.SocketDir: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(sockDir) })

	for _, prefix := range []string{"h", "d", "e"} {
		name := ext.ShortSocketName(prefix, longID)
		if !strings.HasPrefix(name, prefix+"-") || !strings.HasSuffix(name, ".sock") || len(name) != 15 {
			t.Fatalf("ShortSocketName(%q, id) = %q (len=%d), want <p>-<8hex>.sock (len 15)", prefix, name, len(name))
		}
		full, err := paths.SocketPath(prefix, longID)
		if err != nil {
			t.Fatalf("SocketPath(%q): %v", prefix, err)
		}
		if runtime.GOOS != "windows" && len(full) > ext.MaxUnixSocketPathLen {
			t.Fatalf("SocketPath(%q) len=%d > MaxUnixSocketPathLen(%d): %s", prefix, len(full), ext.MaxUnixSocketPathLen, full)
		}
	}
}

func TestF13_T1_LongDataDirYieldsShortSocketPath(t *testing.T) {
	base := newTempDataDir(t)
	longDataDir := filepath.Join(base, strings.Repeat("nested-application-support-dir/", 5), "gvx-desktop")
	if len(longDataDir) < 130 {
		t.Fatalf("expected long dataDir >= 130 chars, got %d", len(longDataDir))
	}
	paths := ext.NewPaths(longDataDir)
	sockDir, err := paths.SocketDir()
	if err != nil {
		t.Fatalf("SocketDir for long dataDir: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(sockDir) })

	sockPath, err := paths.SocketPath("d", "github-actions-extension")
	if err != nil {
		t.Fatalf("SocketPath: %v", err)
	}
	if runtime.GOOS != "windows" && len(sockPath) > ext.MaxUnixSocketPathLen {
		t.Fatalf("socket path %q len=%d exceeds %dB sun_path budget", sockPath, len(sockPath), ext.MaxUnixSocketPathLen)
	}
}

func TestF13_T1_CleanupRemovesStaleSocketsOnStopAndUninstall(t *testing.T) {
	dataDir := newShortTempDataDir(t)
	mgr, _ := newTestManager(t, WithDataDir(dataDir), WithDeveloperMode(true))
	const id = "sock-cleanup-ext"
	e2e2NewBinaryExt(t, mgr, id, "", "")

	extSock, err := ext.NewPaths(dataDir).SocketPath("e", id)
	if err != nil {
		t.Fatalf("SocketPath: %v", err)
	}
	if runtime.GOOS != "windows" {
		if _, err := os.Stat(extSock); err != nil {
			t.Fatalf("expected extension socket %s while running: %v", extSock, err)
		}
	}

	if err := mgr.Disable(id); err != nil {
		t.Fatalf("Disable: %v", err)
	}
	if runtime.GOOS != "windows" {
		if _, err := os.Stat(extSock); !os.IsNotExist(err) {
			t.Fatalf("expected stale socket %s removed on Disable (err=%v)", extSock, err)
		}
	}

	if err := mgr.Enable(id); err != nil {
		t.Fatalf("Enable after Disable: %v", err)
	}
	if runtime.GOOS != "windows" {
		if _, err := os.Stat(extSock); err != nil {
			t.Fatalf("expected socket %s recreated on Enable: %v", extSock, err)
		}
	}

	if err := mgr.Uninstall(id, true); err != nil {
		t.Fatalf("Uninstall: %v", err)
	}
	if runtime.GOOS != "windows" {
		if _, err := os.Stat(extSock); !os.IsNotExist(err) {
			t.Fatalf("expected socket %s removed on Uninstall (err=%v)", extSock, err)
		}
	}
}

func TestF13_T1_LoopbackTCPWith256BitBearerAuth(t *testing.T) {
	var rawHostTok, rawExtTok [32]byte
	if _, err := rand.Read(rawHostTok[:]); err != nil {
		t.Fatalf("rand.Read: %v", err)
	}
	if _, err := rand.Read(rawExtTok[:]); err != nil {
		t.Fatalf("rand.Read: %v", err)
	}
	hostToken := hex.EncodeToString(rawHostTok[:])
	extToken := hex.EncodeToString(rawExtTok[:])

	hostSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+hostToken {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/v1/host/info" {
			_, _ = w.Write([]byte(`{"app_version":"0.10.1-tcp","os":"windows","arch":"amd64","locale":"en","theme":"dark","docker_mode":"sandbox","docker_ready":true}`))
			return
		}
		_, _ = w.Write([]byte(`{}`))
	}))
	defer hostSrv.Close()

	app := extsdk.New()
	app.Command("host-ping", func(ctx *extsdk.CommandContext) error {
		info, err := ctx.Host.Info(ctx)
		if err != nil {
			return err
		}
		if info.AppVersion != "0.10.1-tcp" {
			return fmt.Errorf("unexpected AppVersion %q", info.AppVersion)
		}
		return nil
	})

	runCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	runErr := make(chan error, 1)
	go func() {
		runErr <- app.RunWithConfig(runCtx, extsdk.Config{
			ExtID:      "tcp-loopback-ext",
			HostAPI:    hostSrv.URL,
			HostToken:  hostToken,
			ListenAddr: "127.0.0.1:0",
			ExtToken:   extToken,
		})
	}()

	var listenAddr string
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		listenAddr = app.ListenAddr()
		if listenAddr != "" {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if listenAddr == "" {
		t.Fatal("timed out waiting for TCP ListenAddr")
	}
	baseURL := "http://" + listenAddr

	st, _, _ := httpDoJSON(t, nil, http.MethodGet, baseURL+"/v1/health", "", "", nil, nil)
	if st != http.StatusUnauthorized {
		t.Fatalf("unauthenticated GET /v1/health = %d, want 401", st)
	}
	st, _, _ = httpDoJSON(t, nil, http.MethodGet, baseURL+"/v1/health", extToken, "", nil, nil)
	if st != http.StatusOK {
		t.Fatalf("authenticated GET /v1/health = %d, want 200", st)
	}
	st, _, body := httpDoJSON(t, nil, http.MethodPost, baseURL+"/v1/commands/host-ping", extToken, "", map[string]string{"locale": "en"}, nil)
	if st != http.StatusOK {
		t.Fatalf("POST /v1/commands/host-ping = %d: %s", st, string(body))
	}

	st, _, _ = httpDoJSON(t, nil, http.MethodPost, baseURL+"/v1/shutdown", extToken, "", nil, nil)
	if st != http.StatusOK {
		t.Fatalf("POST /v1/shutdown = %d, want 200", st)
	}
	if err := <-runErr; err != nil {
		t.Fatalf("RunWithConfig returned error: %v", err)
	}
}

func TestF13_T2_FallsBackToTmpWhenTempDirTooLong(t *testing.T) {
	if runtime.GOOS == "windows" {
		dataDir := newTempDataDir(t)
		sockDir, err := ext.SocketDir(dataDir)
		if err != nil {
			t.Fatalf("SocketDir on windows: %v", err)
		}
		if sockDir == "" {
			t.Fatal("expected non-empty socket dir")
		}
		return
	}

	longTmp := filepath.Join(t.TempDir(), strings.Repeat("long-tmpdir-seg-", 6))
	if err := os.MkdirAll(longTmp, 0o700); err != nil {
		t.Fatalf("mkdir longTmp: %v", err)
	}
	t.Setenv("TMPDIR", longTmp)

	dataDir := newTempDataDir(t)
	sockDir, err := ext.SocketDir(dataDir)
	if err != nil {
		t.Fatalf("SocketDir with long TMPDIR: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(sockDir) })

	if !strings.HasPrefix(sockDir, "/tmp/gvxd-ext-") {
		t.Fatalf("expected /tmp fallback when TMPDIR is too long, got %q", sockDir)
	}
	sample := filepath.Join(sockDir, ext.ShortSocketName("d", "ext-id"))
	if len(sample) > ext.MaxUnixSocketPathLen {
		t.Fatalf("fallback socket path %q len=%d exceeds %d", sample, len(sample), ext.MaxUnixSocketPathLen)
	}
	assertDirPerm(t, sockDir, 0o700)
}

func TestF13_T2_RejectsNon0700OrSymlinkSocketDir(t *testing.T) {
	if runtime.GOOS == "windows" {
		dataDir := newTempDataDir(t)
		if _, err := ext.SocketDir(dataDir); err != nil {
			t.Fatalf("SocketDir: %v", err)
		}
		return
	}
	dataDir := newTempDataDir(t)
	sockDir, err := ext.SocketDir(dataDir)
	if err != nil {
		t.Fatalf("initial SocketDir: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(sockDir) })

	if err := os.RemoveAll(sockDir); err != nil {
		t.Fatalf("RemoveAll: %v", err)
	}
	if err := os.MkdirAll(sockDir, 0o755); err != nil {
		t.Fatalf("MkdirAll 0755: %v", err)
	}
	if err := os.Chmod(sockDir, 0o755); err != nil {
		t.Fatalf("Chmod 0755: %v", err)
	}
	if _, err := ext.SocketDir(dataDir); err == nil {
		t.Fatal("SocketDir accepted 0755 directory permissions")
	}

	_ = os.RemoveAll(sockDir)
	target := t.TempDir()
	if err := os.Symlink(target, sockDir); err != nil {
		t.Fatalf("Symlink: %v", err)
	}
	if _, err := ext.SocketDir(dataDir); err == nil {
		t.Fatal("SocketDir accepted symlink at socket dir path")
	}
}

func TestF13_T2_DistinctDataDirsYieldDistinctSocketDirs(t *testing.T) {
	dirA := newTempDataDir(t)
	dirB := newTempDataDir(t)
	sockA, err := ext.SocketDir(dirA)
	if err != nil {
		t.Fatalf("SocketDir(A): %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(sockA) })

	sockB, err := ext.SocketDir(dirB)
	if err != nil {
		t.Fatalf("SocketDir(B): %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(sockB) })

	if sockA == sockB {
		t.Fatalf("distinct dataDirs produced identical socketDir %q", sockA)
	}
}

func TestF13_T2_RejectsMissingOrWrongBearerTokenOnHostAndExtIPC(t *testing.T) {
	src := `package main

import (
	"context"
	"os"
	"path/filepath"

	"github.com/ironpark/gvx-desktop/pkg/extsdk"
)

func main() {
	app := extsdk.New()
	app.View("main", extsdk.View{
		Render: func(ctx *extsdk.ViewContext, ui *extsdk.UI) extsdk.Node {
			return ui.Column(ui.Text("ipc-auth"))
		},
	})
	app.Command("record-ipc", func(ctx *extsdk.CommandContext) error {
		dataDir := os.Getenv("GVX_EXT_DATA_DIR")
		body := os.Getenv("GVX_HOST_API") + "\n" + os.Getenv("GVX_EXT_LISTEN")
		return os.WriteFile(filepath.Join(dataDir, "ipc.txt"), []byte(body), 0o600)
	})
	_ = app.Run(context.Background())
}
`
	dataDir := newShortTempDataDir(t)
	mgr, _ := newTestManager(t, WithDataDir(dataDir), WithDeveloperMode(true))
	const id = "ipc-auth-ext"
	e2e2NewBinaryExt(t, mgr, id, src, "\n[[contributes.command]]\nid = \"record-ipc\"\ntitle = \"Record IPC\"\n")
	if err := mgr.RunCommand(id, "record-ipc", "en"); err != nil {
		t.Fatalf("RunCommand(record-ipc): %v", err)
	}

	raw, err := os.ReadFile(filepath.Join(ext.FilesDir(dataDir, id), "ipc.txt"))
	if err != nil {
		t.Fatalf("read ipc.txt: %v", err)
	}
	parts := strings.Split(strings.TrimSpace(string(raw)), "\n")
	if len(parts) != 2 {
		t.Fatalf("unexpected ipc.txt contents: %q", string(raw))
	}
	hostEndpoint, extEndpoint := parts[0], parts[1]

	probeUnauth := func(endpoint, path string) {
		var client *http.Client
		base := endpoint
		if strings.HasPrefix(endpoint, "unix://") {
			client = newUnixHTTPClient(strings.TrimPrefix(endpoint, "unix://"))
			base = "http://unix"
		}
		for _, tok := range []string{"", "wrong-bearer-token-value"} {
			st, _, _ := httpDoJSON(t, client, http.MethodGet, base+path, tok, "", nil, nil)
			if st != http.StatusUnauthorized {
				t.Fatalf("endpoint %s%s with token=%q returned %d, want 401", endpoint, path, tok, st)
			}
		}
	}
	probeUnauth(hostEndpoint, "/v1/host/info")
	probeUnauth(extEndpoint, "/v1/health")
}

func TestF13_T2_ConcurrentExtensionsIsolateIPCPaths(t *testing.T) {
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
			return ui.Column(ui.Text(os.Getenv("GVX_EXT_ID")))
		},
	})
	app.Command("whoami", func(ctx *extsdk.CommandContext) error {
		return ctx.Host.StorageSet(ctx, "id", os.Getenv("GVX_EXT_ID"))
	})
	_ = app.Run(context.Background())
}
`
	dataDir := newShortTempDataDir(t)
	mgr, _ := newTestManager(t, WithDataDir(dataDir), WithDeveloperMode(true))
	extra := "\n[[contributes.command]]\nid = \"whoami\"\ntitle = \"Whoami\"\n"
	ids := []string{"conc-ext-1", "conc-ext-2", "conc-ext-3"}
	for _, id := range ids {
		e2e2NewBinaryExt(t, mgr, id, src, extra, ext.PermStorage)
	}

	var wg sync.WaitGroup
	errs := make(chan error, len(ids))
	for _, id := range ids {
		wg.Add(1)
		go func(extID string) {
			defer wg.Done()
			if err := mgr.RunCommand(extID, "whoami", "en"); err != nil {
				errs <- fmt.Errorf("%s: %w", extID, err)
			}
		}(id)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
	for _, id := range ids {
		info := e2e2RuntimeMustExtension(t, mgr, id)
		if info.State != ext.StateRunning {
			t.Fatalf("%s state = %q, want running", id, info.State)
		}
	}
}

// =============================================================================
// Feature 14: Approval & Runtime State Machine
// =============================================================================

func TestF14_T1_LifecycleWaitingBuildingStartingRunning(t *testing.T) {
	activeStates := map[ext.ExtensionState]bool{
		ext.StateStarting: true,
		ext.StateRunning:  true,
		ext.StateDegraded: true,
	}
	nonRunnableStates := map[ext.ExtensionState]bool{
		ext.StateInvalid:  true,
		ext.StateWaiting:  true,
		ext.StateBuilding: true,
		ext.StateDisabled: true,
		ext.StateFailed:   true,
	}
	allStates := []ext.ExtensionState{
		ext.StateDiscovered, ext.StateInvalid, ext.StateWaiting, ext.StateBuilding,
		ext.StateDisabled, ext.StateStarting, ext.StateRunning, ext.StateDegraded,
		ext.StateCrashed, ext.StateFailed,
	}
	for _, st := range allStates {
		if st.IsActive() != activeStates[st] {
			t.Fatalf("%q.IsActive() = %v, want %v", st, st.IsActive(), activeStates[st])
		}
		if st.CanRun() != !nonRunnableStates[st] {
			t.Fatalf("%q.CanRun() = %v, want %v", st, st.CanRun(), !nonRunnableStates[st])
		}
	}

	mgr, _ := newTestManager(t, WithDeveloperMode(true))
	binPath := buildFixtureSDKBinary(t, e2e2RuntimeSDKSourceMinimal)
	binData, err := os.ReadFile(binPath)
	if err != nil {
		t.Fatalf("read binary: %v", err)
	}
	const id = "lifecycle-ext"
	extDir := filepath.Join(t.TempDir(), id)
	writeExtFixture(t, extDir, binaryManifestTOML(id, "0.1.0", "bin/ext"), nil)
	if err := os.MkdirAll(filepath.Join(extDir, "bin"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(extDir, "bin", "ext"), binData, 0o755); err != nil {
		t.Fatal(err)
	}

	info1, err := mgr.InstallLocal(extDir)
	if err != nil {
		t.Fatalf("InstallLocal: %v", err)
	}
	if info1.State != ext.StateWaiting {
		t.Fatalf("state after InstallLocal = %q, want %q", info1.State, ext.StateWaiting)
	}
	if err := mgr.AllowAndBuild(id); err != nil {
		t.Fatalf("AllowAndBuild: %v", err)
	}
	info2 := e2e2RuntimeMustExtension(t, mgr, id)
	if info2.State != ext.StateRunning {
		t.Fatalf("state after AllowAndBuild = %q, want %q", info2.State, ext.StateRunning)
	}
}

func TestF14_T1_DisableAndEnableTransitions(t *testing.T) {
	mgr, h := newTestManager(t, WithDeveloperMode(true))
	const id = "disable-enable-ext"
	e2e2NewBinaryExt(t, mgr, id, "", "")

	if err := mgr.Disable(id); err != nil {
		t.Fatalf("Disable: %v", err)
	}
	info := e2e2RuntimeMustExtension(t, mgr, id)
	if info.State != ext.StateDisabled {
		t.Fatalf("state after Disable = %q, want %q", info.State, ext.StateDisabled)
	}
	gs, err := ext.LoadGrantStore(ext.GrantsPath(h.DataDir))
	if err != nil || !gs.Get(id).Disabled {
		t.Fatalf("Disabled not persisted in grants (err=%v entry=%+v)", err, gs.Get(id))
	}

	if err := mgr.Enable(id); err != nil {
		t.Fatalf("Enable: %v", err)
	}
	info2 := e2e2RuntimeMustExtension(t, mgr, id)
	if info2.State != ext.StateRunning {
		t.Fatalf("state after Enable = %q, want %q", info2.State, ext.StateRunning)
	}
}

func TestF14_T1_InvalidStateOnBadPersistedManifest(t *testing.T) {
	dataDir := newTempDataDir(t)
	badDir := ext.ExtensionDir(dataDir, "bad-persisted-ext")
	if err := os.MkdirAll(badDir, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	badTOML := "schema_version = 99\nid = \"bad-persisted-ext\"\nunknown_field = true\n"
	if err := os.WriteFile(filepath.Join(badDir, ext.ManifestName), []byte(badTOML), 0o600); err != nil {
		t.Fatalf("write bad manifest: %v", err)
	}

	mgr, _ := newTestManager(t, WithDataDir(dataDir))
	_ = mgr.Start(context.Background())

	info := e2e2RuntimeMustExtension(t, mgr, "bad-persisted-ext")
	if info.State != ext.StateInvalid {
		t.Fatalf("state for corrupt persisted manifest = %q, want %q", info.State, ext.StateInvalid)
	}
	if info.Error == "" {
		t.Fatal("expected non-empty Error on StateInvalid extension")
	}
}

func TestF14_T1_StartingPollsHealthUntil200OK(t *testing.T) {
	src := `package main

import (
	"context"
	"time"

	"github.com/ironpark/gvx-desktop/pkg/extsdk"
)

func main() {
	time.Sleep(450 * time.Millisecond)
	app := extsdk.New()
	app.View("main", extsdk.View{
		Render: func(ctx *extsdk.ViewContext, ui *extsdk.UI) extsdk.Node {
			return ui.Column(ui.Text("slow-boot-ok"))
		},
	})
	_ = app.Run(context.Background())
}
`
	mgr, _ := newTestManager(t, WithDeveloperMode(true))
	const id = "slow-health-ext"
	e2e2NewBinaryExt(t, mgr, id, src, "")

	info := e2e2RuntimeMustExtension(t, mgr, id)
	if info.State != ext.StateRunning {
		t.Fatalf("state after delayed /v1/health startup = %q, want %q", info.State, ext.StateRunning)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	subCtx, subCancel := context.WithCancel(ctx)
	defer subCancel()
	gotView := false
	ch, err := mgr.SubscribeView(subCtx, id, "main", "en")
	if err != nil {
		t.Fatalf("SubscribeView: %v", err)
	}
	for u := range ch {
		if u.Tree != nil {
			gotView = true
			subCancel()
			break
		}
	}
	if !gotView {
		t.Fatal("expected view tree after slow health boot")
	}
}

func TestF14_T1_ChangedCallbackFiresOnStateTransitions(t *testing.T) {
	mgr, h := newTestManager(t, WithDeveloperMode(true))
	binData, err := os.ReadFile(buildFixtureSDKBinary(t, e2e2RuntimeSDKSourceMinimal))
	if err != nil {
		t.Fatalf("read binary: %v", err)
	}
	const id = "changed-events-ext"
	extDir := filepath.Join(t.TempDir(), id)
	writeExtFixture(t, extDir, binaryManifestTOML(id, "0.1.0", "bin/ext"), nil)
	if err := os.MkdirAll(filepath.Join(extDir, "bin"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(extDir, "bin", "ext"), binData, 0o755); err != nil {
		t.Fatal(err)
	}

	since := h.UIHost.DrainChanged()
	if _, err := mgr.InstallLocal(extDir); err != nil {
		t.Fatalf("InstallLocal: %v", err)
	}
	since = h.UIHost.WaitChangedAfter(t, since, 3*time.Second)

	if err := mgr.AllowAndBuild(id); err != nil {
		t.Fatalf("AllowAndBuild: %v", err)
	}
	since = h.UIHost.WaitChangedAfter(t, since, 3*time.Second)

	if err := mgr.Disable(id); err != nil {
		t.Fatalf("Disable: %v", err)
	}
	since = h.UIHost.WaitChangedAfter(t, since, 3*time.Second)

	if err := mgr.Enable(id); err != nil {
		t.Fatalf("Enable: %v", err)
	}
	since = h.UIHost.WaitChangedAfter(t, since, 3*time.Second)

	if err := mgr.Uninstall(id, true); err != nil {
		t.Fatalf("Uninstall: %v", err)
	}
	_ = h.UIHost.WaitChangedAfter(t, since, 3*time.Second)
}

func TestF14_T2_StartingTimesOutAfter15sOnUnhealthyExtension(t *testing.T) {
	// Listen on GVX_EXT_LISTEN but return 503 on /v1/health so startup times out at 15s.
	src := `package main

import (
	"net"
	"net/http"
	"os"
	"strings"
	"time"
)

func main() {
	addr := os.Getenv("GVX_EXT_LISTEN")
	var ln net.Listener
	var err error
	if strings.HasPrefix(addr, "unix://") {
		sock := strings.TrimPrefix(addr, "unix://")
		_ = os.Remove(sock)
		ln, err = net.Listen("unix", sock)
	} else {
		ln, err = net.Listen("tcp", addr)
	}
	if err != nil {
		os.Exit(2)
	}
	srv := &http.Server{
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.Error(w, "unhealthy", http.StatusServiceUnavailable)
		}),
	}
	go func() { _ = srv.Serve(ln) }()
	time.Sleep(60 * time.Second)
}
`
	mgr, _ := newTestManager(t, WithDeveloperMode(true))
	binData, err := os.ReadFile(buildFixtureSDKBinary(t, src))
	if err != nil {
		t.Fatalf("read binary: %v", err)
	}
	const id = "unhealthy-timeout-ext"
	extDir := filepath.Join(t.TempDir(), id)
	writeExtFixture(t, extDir, binaryManifestTOML(id, "0.1.0", "bin/ext"), nil)
	if err := os.MkdirAll(filepath.Join(extDir, "bin"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(extDir, "bin", "ext"), binData, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := mgr.InstallLocal(extDir); err != nil {
		t.Fatalf("InstallLocal: %v", err)
	}

	start := time.Now()
	err = mgr.AllowAndBuild(id)
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("expected AllowAndBuild to fail after 15s healthcheck timeout")
	}
	if elapsed < 14*time.Second {
		t.Fatalf("healthcheck timeout returned too early (%v < 14s): %v", elapsed, err)
	}
	info := e2e2RuntimeMustExtension(t, mgr, id)
	if info.State == ext.StateRunning {
		t.Fatalf("unhealthy extension must not be StateRunning")
	}
}

func TestF14_T2_CrashTriggersBackoffRestart(t *testing.T) {
	// Crash on run #1 shortly after serving health; succeed on run #2.
	src := `package main

import (
	"context"
	"os"
	"path/filepath"
	"time"

	"github.com/ironpark/gvx-desktop/pkg/extsdk"
)

func main() {
	marker := filepath.Join(os.Getenv("GVX_EXT_DATA_DIR"), "crashed-once")
	_, err := os.Stat(marker)
	firstRun := os.IsNotExist(err)
	if firstRun {
		_ = os.WriteFile(marker, []byte("1"), 0o600)
	}
	app := extsdk.New()
	app.View("main", extsdk.View{
		Render: func(ctx *extsdk.ViewContext, ui *extsdk.UI) extsdk.Node {
			return ui.Column(ui.Text("recovered"))
		},
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if firstRun {
		go func() {
			time.Sleep(150 * time.Millisecond)
			os.Exit(1)
		}()
	}
	_ = app.Run(ctx)
}
`
	mgr, _ := newTestManager(t, WithDeveloperMode(true))
	const id = "crash-backoff-ext"
	e2e2NewBinaryExt(t, mgr, id, src, "")

	// Wait for run #1's intentional crash (at 150ms) before awaiting recovery to StateRunning.
	_ = e2e2AwaitExtState(t, mgr, id, ext.StateCrashed, 5*time.Second)

	info := e2e2AwaitExtState(t, mgr, id, ext.StateRunning, 15*time.Second)
	if info.State != ext.StateRunning {
		t.Fatalf("expected crash recovery to StateRunning, got %q", info.State)
	}
	// Ensure it stays running after recovery.
	time.Sleep(300 * time.Millisecond)
	if cur := e2e2RuntimeMustExtension(t, mgr, id); cur.State != ext.StateRunning {
		t.Fatalf("expected recovered extension to stay running, got %q", cur.State)
	}
}

func TestF14_T2_ThreeConsecutiveCrashesHaltInFailedState(t *testing.T) {
	// Always crash shortly after startup so 3 consecutive crashes halt in StateFailed.
	src := `package main

import (
	"context"
	"os"
	"time"

	"github.com/ironpark/gvx-desktop/pkg/extsdk"
)

func main() {
	app := extsdk.New()
	app.View("main", extsdk.View{
		Render: func(ctx *extsdk.ViewContext, ui *extsdk.UI) extsdk.Node {
			return ui.Column(ui.Text("doomed"))
		},
	})
	go func() {
		time.Sleep(80 * time.Millisecond)
		os.Exit(1)
	}()
	_ = app.Run(context.Background())
}
`
	mgr, _ := newTestManager(t, WithDeveloperMode(true))
	binData, err := os.ReadFile(buildFixtureSDKBinary(t, src))
	if err != nil {
		t.Fatalf("read binary: %v", err)
	}
	const id = "three-crash-ext"
	extDir := filepath.Join(t.TempDir(), id)
	writeExtFixture(t, extDir, binaryManifestTOML(id, "0.1.0", "bin/ext"), nil)
	if err := os.MkdirAll(filepath.Join(extDir, "bin"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(extDir, "bin", "ext"), binData, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := mgr.InstallLocal(extDir); err != nil {
		t.Fatalf("InstallLocal: %v", err)
	}
	_ = mgr.AllowAndBuild(id)

	info := e2e2AwaitExtState(t, mgr, id, ext.StateFailed, 25*time.Second)
	if info.State.CanRun() {
		t.Fatalf("StateFailed should report CanRun() == false")
	}
}

func TestF14_T2_RevokePermissionsOrDenyStopsRunningExtensionImmediately(t *testing.T) {
	mgr, _ := newTestManager(t, WithDeveloperMode(true))
	const idRev = "stop-on-revoke"
	const idDeny = "stop-on-deny"

	e2e2NewBinaryExt(t, mgr, idRev, "", "", ext.PermStorage)
	e2e2NewBinaryExt(t, mgr, idDeny, "", "", ext.PermStorage)

	if err := mgr.RevokePermissions(idRev); err != nil {
		t.Fatalf("RevokePermissions: %v", err)
	}
	if info := e2e2RuntimeMustExtension(t, mgr, idRev); info.State.IsActive() || info.State != ext.StateWaiting {
		t.Fatalf("after RevokePermissions state = %q, want waiting", info.State)
	}

	if err := mgr.Deny(idDeny); err != nil {
		t.Fatalf("Deny: %v", err)
	}
	if info := e2e2RuntimeMustExtension(t, mgr, idDeny); info.State.IsActive() {
		t.Fatalf("after Deny state = %q, expected inactive", info.State)
	}
	if err := mgr.Enable(idDeny); err == nil {
		t.Fatal("expected Enable to fail on denied extension")
	}
}

func TestF14_T2_RejectsInvalidStateTransitions(t *testing.T) {
	mgr, _ := newTestManager(t, WithDeveloperMode(true))
	if err := mgr.AllowAndBuild("nonexistent-ext"); err == nil {
		t.Fatal("AllowAndBuild succeeded on nonexistent extension")
	}
	if err := mgr.Enable("nonexistent-ext"); err == nil {
		t.Fatal("Enable succeeded on nonexistent extension")
	}

	const idWait = "invalid-trans-wait"
	waitDir := createLocalExtensionDir(t, t.TempDir(), idWait, minimalManifestTOML(idWait, "0.1.0"), nil)
	if _, err := mgr.InstallLocal(waitDir); err != nil {
		t.Fatalf("InstallLocal: %v", err)
	}
	if err := mgr.Enable(idWait); err == nil {
		t.Fatal("Enable succeeded while extension is in StateWaiting")
	}
	if err := mgr.Rebuild(idWait); err == nil {
		t.Fatal("Rebuild succeeded while extension is in StateWaiting")
	}

	const idBin = "invalid-trans-bin"
	e2e2NewBinaryExt(t, mgr, idBin, "", "")
	if err := mgr.Deny(idBin); err != nil {
		t.Fatalf("Deny: %v", err)
	}
	if err := mgr.Enable(idBin); err == nil {
		t.Fatal("Enable succeeded on denied extension")
	}
	if err := mgr.Rebuild(idBin); err == nil {
		t.Fatal("Rebuild succeeded on denied extension")
	}
}

// =============================================================================
// Feature 15: sandbox (rungv) Runtime
// =============================================================================

func TestF15_T1_DefaultRuntimeIsSandbox(t *testing.T) {
	toml := `schema_version = 1
id = "default-sbx"
version = "0.1.0"
publisher = "acme"
name = "Default Sandbox"

[runtime]
dockerfile = "Dockerfile"
`
	m, err := ext.ParseManifest([]byte(toml))
	if err != nil {
		t.Fatalf("ParseManifest: %v", err)
	}
	if m.Runtime.Type != ext.RuntimeSandbox {
		t.Fatalf("default Runtime.Type = %q, want %q", m.Runtime.Type, ext.RuntimeSandbox)
	}
	if m.Runtime.Port != ext.DefaultRuntimePort {
		t.Fatalf("default Runtime.Port = %d, want %d", m.Runtime.Port, ext.DefaultRuntimePort)
	}

	mgr, _ := newTestManager(t)
	dir := createLocalExtensionDir(t, t.TempDir(), "default-sbx", toml, nil)
	info, err := mgr.InstallLocal(dir)
	if err != nil {
		t.Fatalf("InstallLocal: %v", err)
	}
	if info.RuntimeType != ext.RuntimeSandbox || info.Unsandboxed {
		t.Fatalf("ExtensionInfo RuntimeType=%q Unsandboxed=%v, want sandbox/false", info.RuntimeType, info.Unsandboxed)
	}
}

func TestF15_T1_ConstructsSandboxSpecWithDataAndAllowedFolders(t *testing.T) {
	const id = "sbx-mounts-ext"
	dataDir := newShortTempDataDir(t)
	folder := e2e2RuntimeNonReservedDir(t)
	gs, err := ext.LoadGrantStore(ext.GrantsPath(dataDir))
	if err != nil {
		t.Fatalf("LoadGrantStore: %v", err)
	}
	if _, err := gs.AddAllowedFolder(id, folder); err != nil {
		t.Fatalf("AddAllowedFolder: %v", err)
	}

	rh := e2e2NewRungvManagerWithDir(t, dataDir, true)
	toml := `schema_version = 1
id = "sbx-mounts-ext"
version = "0.1.0"
publisher = "acme"
name = "Sandbox Mounts"

[runtime]
type = "sandbox"
dockerfile = "Dockerfile"
memory = "256m"
cpus = "1"
`
	dir := createLocalExtensionDir(t, t.TempDir(), id, toml, nil)
	if _, err := rh.Manager.InstallLocal(dir); err != nil {
		t.Fatalf("InstallLocal: %v", err)
	}
	_ = rh.Manager.AllowAndBuild(id)

	specs := rh.snapshotSpecs()
	if len(specs) == 0 {
		t.Fatal("expected Supervisor.Start to construct at least one sandbox.Spec")
	}
	spec := specs[len(specs)-1]
	if spec.Memory != "256m" || spec.CPUs != "1" {
		t.Fatalf("unexpected resources in sandbox.Spec: memory=%q cpus=%q", spec.Memory, spec.CPUs)
	}
	wantData := ext.FilesDir(rh.Harness.DataDir, id)
	wantGuestFolder, err := recipes.GuestPath(folder)
	if err != nil {
		t.Fatalf("GuestPath: %v", err)
	}
	var hasData, hasAllowed bool
	for _, m := range spec.Mounts {
		if m.Type == sandbox.MountBind && m.Source == wantData && m.Target == "/data" {
			hasData = true
		}
		if m.Type == sandbox.MountBind && m.Source == folder && m.Target == wantGuestFolder {
			hasAllowed = true
		}
	}
	if !hasData {
		t.Fatalf("sandbox.Spec missing /data bind mount for %s: %+v", wantData, spec.Mounts)
	}
	if !hasAllowed {
		t.Fatalf("sandbox.Spec missing allowed folder mount %s -> %s: %+v", folder, wantGuestFolder, spec.Mounts)
	}
}

func TestF15_T1_NetworkModeGatedByNetworkPermission(t *testing.T) {
	rh := e2e2NewRungvManager(t, true)

	const idNoNet = "sbx-net-none"
	dirNoNet := createLocalExtensionDir(t, t.TempDir(), idNoNet, minimalManifestTOML(idNoNet, "0.1.0"), nil)
	if _, err := rh.Manager.InstallLocal(dirNoNet); err != nil {
		t.Fatalf("InstallLocal(noNet): %v", err)
	}
	_ = rh.Manager.AllowAndBuild(idNoNet)

	specs1 := rh.snapshotSpecs()
	if len(specs1) == 0 {
		t.Fatal("expected sandbox.Spec for no-network extension")
	}
	spec1 := specs1[len(specs1)-1]
	binPath, _, _ := requireRungv(t)
	feat := (&rungv.Client{Path: binPath}).Features(context.Background())
	if feat.NATPolicy {
		if spec1.Network != "nat" || !spec1.IsolateLAN || spec1.NATPolicyFile == "" {
			t.Fatalf("expected NATPolicy deny-egress isolation without network permission, got network=%q isolateLAN=%v policy=%q",
				spec1.Network, spec1.IsolateLAN, spec1.NATPolicyFile)
		}
		rawPol, err := os.ReadFile(spec1.NATPolicyFile)
		if err != nil || !strings.Contains(string(rawPol), `"deny"`) {
			t.Fatalf("expected deny-all egress NAT policy in %q, got (%q, %v)", spec1.NATPolicyFile, string(rawPol), err)
		}
	} else if spec1.Network != "none" {
		t.Fatalf("expected Network='none' without network permission, got %q", spec1.Network)
	}

	const idWithNet = "sbx-net-nat"
	dirWithNet := createLocalExtensionDir(t, t.TempDir(), idWithNet, minimalManifestTOML(idWithNet, "0.1.0", ext.PermNetwork), nil)
	if _, err := rh.Manager.InstallLocal(dirWithNet); err != nil {
		t.Fatalf("InstallLocal(withNet): %v", err)
	}
	_ = rh.Manager.AllowAndBuild(idWithNet)

	specs2 := rh.snapshotSpecs()
	if len(specs2) <= len(specs1) {
		t.Fatal("expected sandbox.Spec for network-permitted extension")
	}
	spec2 := specs2[len(specs2)-1]
	if spec2.Network != "nat" || spec2.IsolateLAN || spec2.NATPolicyFile != "" {
		t.Fatalf("expected unrestricted Network='nat' with network permission, got network=%q isolateLAN=%v policy=%q",
			spec2.Network, spec2.IsolateLAN, spec2.NATPolicyFile)
	}
}

func TestF15_T1_UnixForwardAndExtPortConfigured(t *testing.T) {
	rh := e2e2NewRungvManager(t, true)
	const id = "sbx-unixfwd-ext"
	dir := createLocalExtensionDir(t, t.TempDir(), id, minimalManifestTOML(id, "0.1.0", ext.PermDocker, ext.PermNetwork), nil)
	if _, err := rh.Manager.InstallLocal(dir); err != nil {
		t.Fatalf("InstallLocal: %v", err)
	}
	_ = rh.Manager.AllowAndBuild(id)

	specs := rh.snapshotSpecs()
	if len(specs) == 0 {
		t.Fatal("expected sandbox.Spec")
	}
	spec := specs[len(specs)-1]

	var hasHostFwd, hasDockerFwd, hasExtPort bool
	for _, uf := range spec.UnixForwards {
		if strings.HasPrefix(uf, "/run/gvx/host.sock=") {
			hasHostFwd = true
		}
		if strings.HasPrefix(uf, "/run/gvx/docker.sock=") {
			hasDockerFwd = true
		}
	}
	for _, p := range spec.Ports {
		if strings.HasPrefix(p, "unix:") && strings.HasSuffix(p, ":18900") {
			hasExtPort = true
		}
	}
	if !hasHostFwd || !hasDockerFwd || !hasExtPort {
		t.Fatalf("missing expected forwards/ports: UnixForwards=%v Ports=%v", spec.UnixForwards, spec.Ports)
	}
}

func TestF15_T1_LiveRungvSandboxLifecycle(t *testing.T) {
	rh := e2e2NewRungvManager(t, false)
	linuxBin := e2e2BuildLinuxSDKBinary(t, e2e2RuntimeSDKSourceMinimal)
	binBytes, err := os.ReadFile(linuxBin)
	if err != nil {
		t.Fatalf("read linux binary: %v", err)
	}

	const id = "sbx-live-ext"
	manifest := `schema_version = 1
id = "sbx-live-ext"
version = "0.1.0"
publisher = "acme"
name = "Live Sandbox"
permissions = ["network"]

[runtime]
type = "sandbox"
image = "alpine:3.20"
port = 18900

[[contributes.view]]
id = "main"
title = "Main"
`
	dir := createLocalExtensionDir(t, t.TempDir(), id, manifest, nil)
	if err := ext.EnsureDir(ext.FilesDir(rh.Harness.DataDir, id)); err != nil {
		t.Fatalf("EnsureDir FilesDir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(ext.FilesDir(rh.Harness.DataDir, id), "ext-bin"), binBytes, 0o755); err != nil {
		t.Fatalf("stage linux binary: %v", err)
	}
	if _, err := rh.Manager.InstallLocal(dir); err != nil {
		t.Fatalf("InstallLocal: %v", err)
	}
	if err := rh.Manager.AllowAndBuild(id); err != nil {
		t.Fatalf("AllowAndBuild(live sandbox): %v", err)
	}

	info := e2e2RuntimeMustExtension(t, rh.Manager, id)
	if info.State != ext.StateRunning || info.Unsandboxed {
		t.Fatalf("live sandbox state=%q unsandboxed=%v, want running/false", info.State, info.Unsandboxed)
	}
	if err := rh.Manager.Disable(id); err != nil {
		t.Fatalf("Disable: %v", err)
	}
	if st := e2e2RuntimeMustExtension(t, rh.Manager, id).State; st != ext.StateDisabled {
		t.Fatalf("state after Disable = %q, want disabled", st)
	}
}

func TestF15_T2_FailsClearlyWhenRungvUnavailable(t *testing.T) {
	// newTestManager without WithSupervisor leaves Supervisor == nil.
	mgr, _ := newTestManager(t, WithDeveloperMode(true))
	const id = "sbx-no-rungv"
	dir := createLocalExtensionDir(t, t.TempDir(), id, minimalManifestTOML(id, "0.1.0"), nil)
	if _, err := mgr.InstallLocal(dir); err != nil {
		t.Fatalf("InstallLocal: %v", err)
	}
	err := mgr.AllowAndBuild(id)
	if err == nil {
		t.Fatal("expected AllowAndBuild to fail clearly when Supervisor/rungv is unavailable")
	}
	info := e2e2RuntimeMustExtension(t, mgr, id)
	if info.State == ext.StateRunning || info.Unsandboxed {
		t.Fatalf("sandbox extension must not fall back to unsandboxed running: state=%q unsandboxed=%v", info.State, info.Unsandboxed)
	}
}

func TestF15_T2_NoDockerSocketOrEnvWithoutDockerPermission(t *testing.T) {
	rh := e2e2NewRungvManager(t, true)
	const id = "sbx-nodocker-ext"
	dir := createLocalExtensionDir(t, t.TempDir(), id, minimalManifestTOML(id, "0.1.0", ext.PermStorage), nil)
	if _, err := rh.Manager.InstallLocal(dir); err != nil {
		t.Fatalf("InstallLocal: %v", err)
	}
	_ = rh.Manager.AllowAndBuild(id)

	specs := rh.snapshotSpecs()
	if len(specs) == 0 {
		t.Fatal("expected sandbox.Spec")
	}
	spec := specs[len(specs)-1]
	for _, uf := range spec.UnixForwards {
		if strings.Contains(uf, "docker.sock") {
			t.Fatalf("docker.sock unexpectedly forwarded without docker permission: %v", spec.UnixForwards)
		}
	}
	for _, env := range spec.Env {
		if strings.HasPrefix(env, "GVX_DOCKER_HOST=") {
			t.Fatalf("GVX_DOCKER_HOST unexpectedly injected without docker permission: %v", spec.Env)
		}
	}
}

func TestF15_T2_PickFolderRestartsSandboxWithUpdatedMounts(t *testing.T) {
	rh := e2e2NewRungvManager(t, false)
	picked := e2e2RuntimeNonReservedDir(t)
	rh.Harness.UIHost.PickFolderResult = picked

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
				ui.Button("Pick Folder", extsdk.Key("pick"), extsdk.OnTap("pick")),
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
	linuxBin := e2e2BuildLinuxSDKBinary(t, src)
	binBytes, err := os.ReadFile(linuxBin)
	if err != nil {
		t.Fatalf("read linux binary: %v", err)
	}

	const id = "sbx-pick-restart"
	manifest := `schema_version = 1
id = "sbx-pick-restart"
version = "0.1.0"
publisher = "acme"
name = "Sandbox Pick Restart"
permissions = ["ui:pick-folder", "network"]

[runtime]
type = "sandbox"
image = "alpine:3.20"
port = 18900

[[contributes.view]]
id = "main"
title = "Main"
`
	dir := createLocalExtensionDir(t, t.TempDir(), id, manifest, nil)
	if err := ext.EnsureDir(ext.FilesDir(rh.Harness.DataDir, id)); err != nil {
		t.Fatalf("EnsureDir FilesDir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(ext.FilesDir(rh.Harness.DataDir, id), "ext-bin"), binBytes, 0o755); err != nil {
		t.Fatalf("stage linux binary: %v", err)
	}
	if _, err := rh.Manager.InstallLocal(dir); err != nil {
		t.Fatalf("InstallLocal: %v", err)
	}
	if err := rh.Manager.AllowAndBuild(id); err != nil {
		t.Fatalf("AllowAndBuild: %v", err)
	}
	beforeCount := len(rh.snapshotSpecs())

	if err := rh.Manager.SendViewEvent(id, "main", uiblock.Event{
		Node:   "/pick",
		Kind:   uiblock.EventTap,
		Locale: "en",
	}); err != nil {
		t.Fatalf("SendViewEvent(pick): %v", err)
	}
	if rh.Harness.UIHost.SnapshotPickFolderCalls() == 0 {
		t.Fatal("expected MockUIHost.PickFolder to be invoked")
	}

	deadline := time.Now().Add(5 * time.Second)
	var specs []sandbox.Spec
	for time.Now().Before(deadline) {
		specs = rh.snapshotSpecs()
		if len(specs) > beforeCount {
			break
		}
		time.Sleep(25 * time.Millisecond)
	}
	if len(specs) <= beforeCount {
		t.Fatalf("expected sandbox restart with updated mounts after pick-folder (specs before=%d after=%d)", beforeCount, len(specs))
	}
	latest := specs[len(specs)-1]
	wantGuest, _ := recipes.GuestPath(picked)
	found := false
	for _, m := range latest.Mounts {
		if m.Type == sandbox.MountBind && m.Source == picked && m.Target == wantGuest {
			found = true
		}
	}
	if !found {
		t.Fatalf("restarted sandbox.Spec missing newly picked folder %s: %+v", picked, latest.Mounts)
	}
}

func TestF15_T2_InjectedGVXEnvOmitsHostSecrets(t *testing.T) {
	t.Setenv("AWS_SECRET_ACCESS_KEY", "host-aws-secret-do-not-leak")
	t.Setenv("GITHUB_TOKEN", "host-gh-token-do-not-leak")

	rh := e2e2NewRungvManager(t, true)
	const id = "sbx-env-isolation"
	dir := createLocalExtensionDir(t, t.TempDir(), id, minimalManifestTOML(id, "0.1.0"), nil)
	if _, err := rh.Manager.InstallLocal(dir); err != nil {
		t.Fatalf("InstallLocal: %v", err)
	}
	_ = rh.Manager.AllowAndBuild(id)

	specs := rh.snapshotSpecs()
	if len(specs) == 0 {
		t.Fatal("expected sandbox.Spec")
	}
	spec := specs[len(specs)-1]
	for _, kv := range spec.Env {
		if strings.Contains(kv, "AWS_SECRET_ACCESS_KEY") || strings.Contains(kv, "GITHUB_TOKEN") ||
			strings.Contains(kv, "host-aws-secret") || strings.Contains(kv, "host-gh-token") {
			t.Fatalf("host secret leaked into sandbox.Spec.Env: %q", kv)
		}
	}
}

func TestF15_T2_CleanStopRemovesSandboxAndMounts(t *testing.T) {
	rh := e2e2NewRungvManager(t, false)
	linuxBin := e2e2BuildLinuxSDKBinary(t, e2e2RuntimeSDKSourceMinimal)
	binBytes, err := os.ReadFile(linuxBin)
	if err != nil {
		t.Fatalf("read linux binary: %v", err)
	}

	const id = "sbx-clean-stop"
	manifest := `schema_version = 1
id = "sbx-clean-stop"
version = "0.1.0"
publisher = "acme"
name = "Clean Stop Sandbox"
permissions = ["network"]

[runtime]
type = "sandbox"
image = "alpine:3.20"
port = 18900

[[contributes.view]]
id = "main"
title = "Main"
`
	dir := createLocalExtensionDir(t, t.TempDir(), id, manifest, nil)
	if err := ext.EnsureDir(ext.FilesDir(rh.Harness.DataDir, id)); err != nil {
		t.Fatalf("EnsureDir FilesDir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(ext.FilesDir(rh.Harness.DataDir, id), "ext-bin"), binBytes, 0o755); err != nil {
		t.Fatalf("stage linux binary: %v", err)
	}
	if _, err := rh.Manager.InstallLocal(dir); err != nil {
		t.Fatalf("InstallLocal: %v", err)
	}
	if err := rh.Manager.AllowAndBuild(id); err != nil {
		t.Fatalf("AllowAndBuild: %v", err)
	}
	if len(rh.Supervisor.Running()) == 0 {
		t.Fatal("expected running sandbox before Disable")
	}

	if err := rh.Manager.Disable(id); err != nil {
		t.Fatalf("Disable: %v", err)
	}
	if len(rh.Supervisor.Running()) != 0 {
		t.Fatalf("expected 0 running sandboxes after Disable, got %v", rh.Supervisor.Running())
	}
	sockPath, err := ext.NewPaths(rh.Harness.DataDir).SocketPath("e", id)
	if err == nil && runtime.GOOS != "windows" {
		if _, statErr := os.Stat(sockPath); !os.IsNotExist(statErr) {
			t.Fatalf("expected extension socket %s removed on stop (err=%v)", sockPath, statErr)
		}
	}
}

// =============================================================================
// Feature 16: Developer Mode binary Runtime
// =============================================================================

func TestF16_T1_BinaryRuntimeStartsWhenDeveloperModeEnabled(t *testing.T) {
	mgr, _ := newTestManager(t, WithDeveloperMode(true))
	const id = "bin-dev-ok"
	e2e2NewBinaryExt(t, mgr, id, "", "")

	info := e2e2RuntimeMustExtension(t, mgr, id)
	if info.State != ext.StateRunning {
		t.Fatalf("state = %q, want %q", info.State, ext.StateRunning)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	subCtx, subCancel := context.WithCancel(ctx)
	defer subCancel()
	gotTree := false
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
		t.Fatal("expected view tree from binary runtime")
	}
}

func TestF16_T1_InjectsRequiredGVXEnvVars(t *testing.T) {
	src := `package main

import (
	"context"
	"encoding/json"
	"os"

	"github.com/ironpark/gvx-desktop/pkg/extsdk"
)

func main() {
	app := extsdk.New()
	app.View("main", extsdk.View{
		Render: func(ctx *extsdk.ViewContext, ui *extsdk.UI) extsdk.Node {
			return ui.Column(ui.Text("env"))
		},
	})
	app.Command("dump-env", func(ctx *extsdk.CommandContext) error {
		envMap := map[string]string{}
		for _, k := range []string{
			"GVX_API_VERSION",
			"GVX_EXT_ID",
			"GVX_HOST_API",
			"GVX_HOST_TOKEN",
			"GVX_EXT_LISTEN",
			"GVX_EXT_TOKEN",
			"GVX_EXT_DATA_DIR",
			"GVX_DOCKER_HOST",
		} {
			envMap[k] = os.Getenv(k)
		}
		b, _ := json.Marshal(envMap)
		return ctx.Host.StorageSet(ctx, "env", string(b))
	})
	_ = app.Run(context.Background())
}
`
	mgr, h := newTestManager(t, WithDeveloperMode(true))
	extra := "\n[[contributes.command]]\nid = \"dump-env\"\ntitle = \"Dump Env\"\n"

	const idNoDocker = "bin-env-nodocker"
	e2e2NewBinaryExt(t, mgr, idNoDocker, src, extra, ext.PermStorage)
	if err := mgr.RunCommand(idNoDocker, "dump-env", "en"); err != nil {
		t.Fatalf("RunCommand(nodocker): %v", err)
	}
	stNoDocker := ext.NewStorage(ext.StoragePath(h.DataDir, idNoDocker), 0)
	raw1, ok, err := stNoDocker.Get("env")
	if err != nil || !ok {
		t.Fatalf("read stored env: ok=%v err=%v", ok, err)
	}
	for _, reqKey := range []string{"GVX_API_VERSION", "GVX_EXT_ID", "GVX_HOST_API", "GVX_HOST_TOKEN", "GVX_EXT_LISTEN", "GVX_EXT_TOKEN", "GVX_EXT_DATA_DIR"} {
		if !strings.Contains(raw1, fmt.Sprintf("%q:\"", reqKey)) || strings.Contains(raw1, fmt.Sprintf("%q:\"\"", reqKey)) {
			t.Fatalf("missing required env %s in %s", reqKey, raw1)
		}
	}
	if !strings.Contains(raw1, `"GVX_API_VERSION":"1"`) || !strings.Contains(raw1, idNoDocker) || !strings.Contains(raw1, ext.FilesDir(h.DataDir, idNoDocker)) {
		t.Fatalf("unexpected env values: %s", raw1)
	}
	if !strings.Contains(raw1, `"GVX_DOCKER_HOST":""`) {
		t.Fatalf("GVX_DOCKER_HOST must be empty without docker perm: %s", raw1)
	}

	const idDocker = "bin-env-docker"
	e2e2NewBinaryExt(t, mgr, idDocker, src, extra, ext.PermStorage, ext.PermDocker)
	if err := mgr.RunCommand(idDocker, "dump-env", "en"); err != nil {
		t.Fatalf("RunCommand(docker): %v", err)
	}
	stDocker := ext.NewStorage(ext.StoragePath(h.DataDir, idDocker), 0)
	raw2, ok, err := stDocker.Get("env")
	if err != nil || !ok {
		t.Fatalf("read stored docker env: ok=%v err=%v", ok, err)
	}
	if strings.Contains(raw2, `"GVX_DOCKER_HOST":""`) {
		t.Fatalf("expected non-empty GVX_DOCKER_HOST with docker perm: %s", raw2)
	}
}

func TestF16_T1_MarksUnsandboxedDevFlag(t *testing.T) {
	mgr, _ := newTestManager(t, WithDeveloperMode(true))
	const id = "bin-unsandboxed-flag"
	e2e2NewBinaryExt(t, mgr, id, "", "")

	info := e2e2RuntimeMustExtension(t, mgr, id)
	if info.RuntimeType != ext.RuntimeBinary {
		t.Fatalf("RuntimeType = %q, want %q", info.RuntimeType, ext.RuntimeBinary)
	}
	if !info.Unsandboxed {
		t.Fatal("expected Unsandboxed == true for binary runtime")
	}
}

func TestF16_T1_CapturesStdoutStderrToLogsDir(t *testing.T) {
	src := `package main

import (
	"context"
	"fmt"
	"os"

	"github.com/ironpark/gvx-desktop/pkg/extsdk"
)

func main() {
	fmt.Fprintln(os.Stdout, "E2E_STDOUT_MARKER_12345")
	fmt.Fprintln(os.Stderr, "E2E_STDERR_MARKER_67890")
	app := extsdk.New()
	app.View("main", extsdk.View{
		Render: func(ctx *extsdk.ViewContext, ui *extsdk.UI) extsdk.Node {
			return ui.Column(ui.Text("logs"))
		},
	})
	_ = app.Run(context.Background())
}
`
	mgr, h := newTestManager(t, WithDeveloperMode(true))
	const id = "bin-logs-capture"
	e2e2NewBinaryExt(t, mgr, id, src, "")

	logsDir := ext.LogsDir(h.DataDir, id)
	assertDirPerm(t, logsDir, 0o700)

	deadline := time.Now().Add(3 * time.Second)
	var combined string
	for time.Now().Before(deadline) {
		entries, _ := filepath.Glob(filepath.Join(logsDir, "*"))
		var b strings.Builder
		for _, p := range entries {
			data, _ := os.ReadFile(p)
			b.Write(data)
		}
		combined = b.String()
		if strings.Contains(combined, "E2E_STDOUT_MARKER_12345") && strings.Contains(combined, "E2E_STDERR_MARKER_67890") {
			return
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatalf("expected stdout & stderr markers in %s, got: %q", logsDir, combined)
}

func TestF16_T1_CleanStopViaShutdownAndSIGTERM(t *testing.T) {
	src := `package main

import (
	"context"
	"os"
	"path/filepath"

	"github.com/ironpark/gvx-desktop/pkg/extsdk"
)

func main() {
	app := extsdk.New()
	app.View("main", extsdk.View{
		Render: func(ctx *extsdk.ViewContext, ui *extsdk.UI) extsdk.Node {
			return ui.Column(ui.Text("shutdown"))
		},
	})
	_ = app.Run(context.Background())
	_ = os.WriteFile(filepath.Join(os.Getenv("GVX_EXT_DATA_DIR"), "clean-exit.txt"), []byte("clean"), 0o600)
}
`
	mgr, h := newTestManager(t, WithDeveloperMode(true))
	const id = "bin-clean-shutdown"
	e2e2NewBinaryExt(t, mgr, id, src, "")

	if err := mgr.Disable(id); err != nil {
		t.Fatalf("Disable: %v", err)
	}
	marker := filepath.Join(ext.FilesDir(h.DataDir, id), "clean-exit.txt")
	b, err := os.ReadFile(marker)
	if err != nil || string(b) != "clean" {
		t.Fatalf("expected clean exit marker %s after /v1/shutdown: data=%q err=%v", marker, string(b), err)
	}
}

func TestF16_T2_RefusesStartWhenDeveloperModeFalse(t *testing.T) {
	mgr, _ := newTestManager(t, WithDeveloperMode(false))
	binData, err := os.ReadFile(buildFixtureSDKBinary(t, e2e2RuntimeSDKSourceMinimal))
	if err != nil {
		t.Fatalf("read binary: %v", err)
	}
	const id = "bin-devmode-off"
	extDir := filepath.Join(t.TempDir(), id)
	writeExtFixture(t, extDir, binaryManifestTOML(id, "0.1.0", "bin/ext"), nil)
	if err := os.MkdirAll(filepath.Join(extDir, "bin"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(extDir, "bin", "ext"), binData, 0o755); err != nil {
		t.Fatal(err)
	}

	if _, err := mgr.InstallLocal(extDir); err != nil {
		t.Fatalf("InstallLocal: %v", err)
	}
	if err := mgr.AllowAndBuild(id); err == nil {
		t.Fatal("expected AllowAndBuild to refuse binary runtime when DeveloperMode == false")
	}
	if info := e2e2RuntimeMustExtension(t, mgr, id); info.State == ext.StateRunning {
		t.Fatal("binary extension started despite DeveloperMode == false")
	}
}

func TestF16_T2_RejectsInvalidOrMissingDevEntrypoint(t *testing.T) {
	mgr, _ := newTestManager(t, WithDeveloperMode(true))

	// 1. Missing dev_entrypoint in manifest.
	noEntryTOML := `schema_version = 1
id = "bin-no-entry"
version = "0.1.0"
publisher = "acme"
name = "No Entry"

[runtime]
type = "binary"
`
	if _, err := ext.ParseManifest([]byte(noEntryTOML)); err == nil {
		t.Fatal("ParseManifest accepted binary runtime without dev_entrypoint")
	}

	// 2. Traversal dev_entrypoint.
	travTOML := binaryManifestTOML("bin-trav", "0.1.0", "../outside/bin")
	if _, err := ext.ParseManifest([]byte(travTOML)); err == nil {
		t.Fatal("ParseManifest accepted '..' traversal dev_entrypoint")
	}

	// 3. Non-existent file on disk.
	missingDir := writeExtFixture(t, filepath.Join(t.TempDir(), "bin-missing"), binaryManifestTOML("bin-missing", "0.1.0", "bin/does-not-exist"), nil)
	if _, err := mgr.InstallLocal(missingDir); err == nil {
		_ = mgr.AllowAndBuild("bin-missing")
		if info, ok := e2e2RuntimeFindExtension(mgr, "bin-missing"); ok && info.State == ext.StateRunning {
			t.Fatal("non-existent dev_entrypoint reached StateRunning")
		}
	}

	// 4. Directory instead of file.
	dirEntry := writeExtFixture(t, filepath.Join(t.TempDir(), "bin-isdir"), binaryManifestTOML("bin-isdir", "0.1.0", "bin"), nil)
	_ = os.MkdirAll(filepath.Join(dirEntry, "bin"), 0o755)
	if _, err := mgr.InstallLocal(dirEntry); err == nil {
		if err := mgr.AllowAndBuild("bin-isdir"); err == nil {
			t.Fatal("directory dev_entrypoint unexpectedly succeeded")
		}
	}

	// 5. Symlink escaping extension directory.
	outsideBin := filepath.Join(t.TempDir(), "outside-bin")
	_ = os.WriteFile(outsideBin, []byte("#!/bin/sh\n"), 0o755)
	symDir := writeExtFixture(t, filepath.Join(t.TempDir(), "bin-sym"), binaryManifestTOML("bin-sym", "0.1.0", "bin/ext"), nil)
	_ = os.MkdirAll(filepath.Join(symDir, "bin"), 0o755)
	_ = os.Symlink(outsideBin, filepath.Join(symDir, "bin", "ext"))
	if _, err := mgr.InstallLocal(symDir); err == nil {
		t.Fatal("InstallLocal accepted symlink-escaping dev_entrypoint")
	}
}

func TestF16_T2_TogglingDeveloperModeOffBlocksRestart(t *testing.T) {
	mgr, h := newTestManager(t, WithDeveloperMode(true))
	const id = "bin-toggle-devmode"
	e2e2NewBinaryExt(t, mgr, id, "", "")

	if err := mgr.Disable(id); err != nil {
		t.Fatalf("Disable: %v", err)
	}
	h.DeveloperMode = false
	if err := mgr.Enable(id); err == nil {
		t.Fatal("expected Enable on binary extension to fail after toggling DeveloperMode = false")
	}
	if info := e2e2RuntimeMustExtension(t, mgr, id); info.State == ext.StateRunning {
		t.Fatal("binary extension started while DeveloperMode == false")
	}
}

func TestF16_T2_RotatingLogsCapDiskGrowth(t *testing.T) {
	src := `package main

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/ironpark/gvx-desktop/pkg/extsdk"
)

func main() {
	app := extsdk.New()
	app.View("main", extsdk.View{
		Render: func(ctx *extsdk.ViewContext, ui *extsdk.UI) extsdk.Node {
			return ui.Column(ui.Text("noisy"))
		},
	})
	app.Command("flood-logs", func(ctx *extsdk.CommandContext) error {
		line := strings.Repeat("L", 4096)
		for i := 0; i < 4500; i++ {
			fmt.Fprintln(os.Stderr, line)
		}
		return nil
	})
	_ = app.Run(context.Background())
}
`
	mgr, h := newTestManager(t, WithDeveloperMode(true))
	const id = "bin-log-rotate"
	e2e2NewBinaryExt(t, mgr, id, src, "\n[[contributes.command]]\nid = \"flood-logs\"\ntitle = \"Flood Logs\"\n")

	if err := mgr.RunCommand(id, "flood-logs", "en"); err != nil {
		t.Fatalf("RunCommand(flood-logs): %v", err)
	}
	// ~18 MiB written to stderr; 5 MiB x (1 active + 2 backups) rotation caps disk usage <= 16 MiB.
	time.Sleep(400 * time.Millisecond)
	logsDir := ext.LogsDir(h.DataDir, id)
	entries, err := os.ReadDir(logsDir)
	if err != nil {
		t.Fatalf("ReadDir(%s): %v", logsDir, err)
	}
	if len(entries) > 3 {
		t.Fatalf("expected at most 3 rotated log files (active + 2 backups), got %d", len(entries))
	}
	var totalBytes int64
	for _, ent := range entries {
		if info, err := ent.Info(); err == nil {
			totalBytes += info.Size()
		}
	}
	if totalBytes == 0 {
		t.Fatal("expected non-empty log files")
	}
	const maxExpectedBytes = 16 << 20
	if totalBytes > maxExpectedBytes {
		t.Fatalf("rotating log size = %d bytes exceeded cap %d bytes", totalBytes, maxExpectedBytes)
	}
}

func TestF16_T2_ForceKillsHungBinaryWithSIGKILL(t *testing.T) {
	// Trap SIGTERM and ignore /v1/shutdown so the host must escalate to SIGKILL within 8s.
	src := `package main

import (
	"context"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
)

func main() {
	sigCh := make(chan os.Signal, 4)
	signal.Notify(sigCh, syscall.SIGTERM, os.Interrupt)
	go func() {
		for range sigCh {
			// Intentionally ignore SIGTERM to test SIGKILL escalation.
		}
	}()

	addr := os.Getenv("GVX_EXT_LISTEN")
	var ln net.Listener
	var err error
	if strings.HasPrefix(addr, "unix://") {
		sock := strings.TrimPrefix(addr, "unix://")
		_ = os.Remove(sock)
		ln, err = net.Listen("unix", sock)
	} else {
		ln, err = net.Listen("tcp", addr)
	}
	if err != nil {
		os.Exit(2)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/health", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(` + "`" + `{"status":"ok","version":"0.1.0"}` + "`" + `))
	})
	mux.HandleFunc("POST /v1/shutdown", func(w http.ResponseWriter, r *http.Request) {
		// Hang intentionally until context closes or SIGKILL kills process.
		<-r.Context().Done()
	})
	_ = (&http.Server{Handler: mux}).Serve(ln)
	_ = context.Background()
}
`
	mgr, _ := newTestManager(t, WithDeveloperMode(true))
	const id = "bin-hung-sigkill"
	e2e2NewBinaryExt(t, mgr, id, src, "")

	start := time.Now()
	_ = mgr.Close(8 * time.Second)
	elapsed := time.Since(start)
	if elapsed > 10*time.Second {
		t.Fatalf("mgr.Close exceeded 8s shutdown budget (%v)", elapsed)
	}
}
