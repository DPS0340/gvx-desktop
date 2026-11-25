//go:build e2e

package e2e

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ironpark/ggui"

	"github.com/ironpark/gvx-desktop/internal/ext"
	"github.com/ironpark/gvx-desktop/internal/rungv"
	"github.com/ironpark/gvx-desktop/internal/sandbox"
	"github.com/ironpark/gvx-desktop/internal/ui"
	"github.com/ironpark/gvx-desktop/internal/uiblock"
)

type t5BlockingBuilder struct {
	started     chan struct{}
	release     chan struct{}
	once        sync.Once
	builtMarker string
}

func (b *t5BlockingBuilder) Build(_ context.Context, req ext.BuildRequest, _ func(string)) (string, error) {
	b.once.Do(func() { close(b.started) })
	<-b.release
	raw, _ := os.ReadFile(filepath.Join(req.ContextDir, "marker.txt"))
	b.builtMarker = strings.TrimSpace(string(raw))
	return "sha256:t5-built", nil
}

func TestT5_01_DockerScopeBypassCrossExtIsolationAndTerminalOwnership(t *testing.T) {
	dataDir := newShortTempDataDir(t)
	st := newTestStore(t, dataDir)
	if err := st.SaveSandboxes([]sandbox.Spec{
		{
			ID:     "t5-foreign-sbx",
			Name:   "t5-foreign-sbx",
			Image:  "alpine:3.20",
			Labels: map[string]string{"io.gvx.extension": "t5-victim"},
		},
	}); err != nil {
		t.Fatalf("SaveSandboxes: %v", err)
	}

	mgr, h := newTestManager(t, WithDataDir(dataDir), WithDeveloperMode(true))
	h.DockerBackend.SeedContainer("t5-host-ctr", "")
	h.DockerBackend.SeedContainer("t5-victim-ctr", "t5-victim")
	h.DockerBackend.SeedNetwork("t5-victim-net-id", "t5-victim-net", "t5-victim")

	attackerSrc := `package main

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

func dockerClient() (*http.Client, string) {
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
	return &http.Client{Transport: tr}, base
}

func doDocker(ctx context.Context, method, path string, body string) (int, string, error) {
	cl, base := dockerClient()
	var r io.Reader
	if body != "" {
		r = bytes.NewBufferString(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, base+path, r)
	if err != nil {
		return 0, "", err
	}
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := cl.Do(req)
	if err != nil {
		return 0, "", err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(raw), nil
}

func main() {
	app := extsdk.New()
	app.View("main", extsdk.View{
		Render: func(c *extsdk.ViewContext, ui *extsdk.UI) extsdk.Node {
			return ui.Column(ui.Button("Attack", extsdk.Key("attack"), extsdk.OnTap("attack")))
		},
		On: extsdk.Handlers{
			"attack": func(ctx *extsdk.EventContext) error {
				// 1. Case-insensitive, Unicode SimpleFold, & duplicate JSON key bypass attempts must all be rejected (400/403).
				payloads := []struct {
					name     string
					body     string
					wantCode int
				}{
					{"lower-binds", ` + "`" + `{"image":"alpine:3.20","labels":{"io.gvx.extension":"t5-victim"},"hostconfig":{"binds":["/etc:/etc"]}}` + "`" + `, http.StatusForbidden},
					{"dup-labels", ` + "`" + `{"Image":"alpine:3.20","Labels":{},"labels":{"io.gvx.extension":"t5-victim"}}` + "`" + `, http.StatusBadRequest},
					{"simplefold-dup-hostconfig-labels", ` + "`" + `{"Image":"alpine:3.20","Labels":{},"label\u017f":{"io.gvx.extension":"t5-victim"},"HostConfig":{},"ho\u017ftconfig":{"Binds":["/etc:/host-etc:ro"],"Privileged":true}}` + "`" + `, http.StatusBadRequest},
					{"simplefold-dup-kelvin-net", ` + "`" + `{"Image":"alpine:3.20","HostConfig":{"NetworkMode":"bridge","Networ\u212aMode":"host"}}` + "`" + `, http.StatusBadRequest},
					{"simplefold-standalone-hostconfig", ` + "`" + `{"Image":"alpine:3.20","ho\u017ftconfig":{"Binds":["/etc:/host-etc:ro"]}}` + "`" + `, http.StatusForbidden},
					{"simplefold-standalone-labels", ` + "`" + `{"Image":"alpine:3.20","label\u017f":{"io.gvx.extension":"t5-victim"}}` + "`" + `, http.StatusForbidden},
					{"privileged", ` + "`" + `{"Image":"alpine:3.20","HostConfig":{"Privileged":true}}` + "`" + `, http.StatusForbidden},
					{"net-host", ` + "`" + `{"Image":"alpine:3.20","HostConfig":{"NetworkMode":"host"}}` + "`" + `, http.StatusForbidden},
					{"pid-host", ` + "`" + `{"Image":"alpine:3.20","HostConfig":{"PidMode":"host"}}` + "`" + `, http.StatusForbidden},
					{"volumes-from", ` + "`" + `{"Image":"alpine:3.20","HostConfig":{"VolumesFrom":["t5-host-ctr"]}}` + "`" + `, http.StatusForbidden},
					{"cap-add", ` + "`" + `{"Image":"alpine:3.20","HostConfig":{"CapAdd":["SYS_ADMIN"]}}` + "`" + `, http.StatusForbidden},
					{"driver-bind", ` + "`" + `{"Image":"alpine:3.20","HostConfig":{"Mounts":[{"Type":"volume","Target":"/m","VolumeOptions":{"DriverConfig":{"Name":"local","Options":{"o":"bind","device":"/etc"}}}}]}}` + "`" + `, http.StatusForbidden},
				}
				for _, tc := range payloads {
					code, raw, err := doDocker(ctx, http.MethodPost, "/containers/create", tc.body)
					if err != nil {
						return fmt.Errorf("%s request error: %w", tc.name, err)
					}
					if code != tc.wantCode {
						return fmt.Errorf("%s expected %d, got %d (%s)", tc.name, tc.wantCode, code, raw)
					}
				}

				// 2. Owned container creation succeeds and GET /containers/json must hide victim & host containers.
				code, raw, err := doDocker(ctx, http.MethodPost, "/containers/create", ` + "`" + `{"image":"alpine:3.20"}` + "`" + `)
				if err != nil || code != http.StatusCreated {
					return fmt.Errorf("owned create failed: code=%d raw=%s err=%v", code, raw, err)
				}
				var created struct{ Id string }
				_ = json.Unmarshal([]byte(raw), &created)
				if created.Id == "" {
					return fmt.Errorf("missing created container ID: %s", raw)
				}

				code, listRaw, err := doDocker(ctx, http.MethodGet, "/v1.47/containers/json?all=1", "")
				if err != nil || code != http.StatusOK {
					return fmt.Errorf("containers/json failed: %d %s %v", code, listRaw, err)
				}
				if strings.Contains(listRaw, "t5-victim-ctr") || strings.Contains(listRaw, "t5-host-ctr") {
					return fmt.Errorf("foreign/host containers leaked in list: %s", listRaw)
				}
				if !strings.Contains(listRaw, created.Id) {
					return fmt.Errorf("owned container %s missing from list: %s", created.Id, listRaw)
				}

				// 3. Cross-extension / host container endpoints, network connect/disconnect, volume DriverOpts bind, & global prune must return 403.
				for _, tc := range []struct {
					m    string
					p    string
					body string
				}{
					{http.MethodGet, "/containers/t5-victim-ctr/json", ""},
					{http.MethodPost, "/containers/t5-victim-ctr/exec", ""},
					{http.MethodDelete, "/containers/t5-host-ctr", ""},
					{http.MethodPost, "/networks/bridge/disconnect", ` + "`" + `{"Container":"t5-host-ctr","Force":true}` + "`" + `},
					{http.MethodPost, "/v1.52/networks/attacker-net/connect", ` + "`" + `{"Container":"t5-victim-ctr"}` + "`" + `},
					{http.MethodPost, "/networks/t5-victim-net/connect", fmt.Sprintf(` + "`" + `{"Container":%q}` + "`" + `, created.Id)},
					{http.MethodPost, "/networks/t5-victim-net/disconnect", fmt.Sprintf(` + "`" + `{"Container":%q}` + "`" + `, created.Id)},
					{http.MethodPost, "/networks/host/connect", fmt.Sprintf(` + "`" + `{"Container":%q}` + "`" + `, created.Id)},
					{http.MethodPost, "/volumes/create", ` + "`" + `{"Name":"t5-bad-vol","DriverOpts":{"type":"none","o":"bind","device":"/etc"}}` + "`" + `},
					{http.MethodPost, "/volumes/create", ` + "`" + `{"Name":"t5-bad-fold-vol","driveropt\u017f":{"Device":"/var/run/docker.sock"}}` + "`" + `},
					{http.MethodPost, "/containers/prune", ""},
					{http.MethodPost, "/images/prune", ""},
				} {
					c, b, e := doDocker(ctx, tc.m, tc.p, tc.body)
					if e != nil || c != http.StatusForbidden {
						return fmt.Errorf("%s %s expected 403, got %d (%s) err=%v", tc.m, tc.p, c, b, e)
					}
				}

				// Create owned network, then owned container network connect/disconnect and safe volume create succeed.
				if c, b, e := doDocker(ctx, http.MethodPost, "/networks/create", ` + "`" + `{"Name":"ext-net"}` + "`" + `); e != nil || c != http.StatusCreated {
					return fmt.Errorf("POST /networks/create expected 201, got %d (%s) err=%v", c, b, e)
				}
				for _, tc := range []struct {
					p    string
					body string
				}{
					{"/v1.52/networks/ext-net/connect", fmt.Sprintf(` + "`" + `{"Container":%q}` + "`" + `, created.Id)},
					{"/networks/ext-net/disconnect", fmt.Sprintf(` + "`" + `{"Container":%q}` + "`" + `, created.Id)},
					{"/volumes/create", ` + "`" + `{"Name":"t5-safe-vol","DriverOpts":{"type":"hostfs"}}` + "`" + `},
				} {
					c, b, e := doDocker(ctx, http.MethodPost, tc.p, tc.body)
					if e != nil || (c != http.StatusOK && c != http.StatusCreated) {
						return fmt.Errorf("POST %s expected 200/201, got %d (%s) err=%v", tc.p, c, b, e)
					}
				}

				// 4. OpenTerminal enforces container & sandbox ownership while allowing owned container.
				if err := ctx.Host.OpenTerminal(ctx, extsdk.OpenTerminalRequest{
					TargetType: "container", TargetID: "t5-victim-ctr", Shell: "/bin/sh",
				}); !extsdk.IsPermissionDenied(err) {
					return fmt.Errorf("expected permission_denied for foreign container terminal, got %v", err)
				}
				if err := ctx.Host.OpenTerminal(ctx, extsdk.OpenTerminalRequest{
					TargetType: "sandbox", TargetID: "t5-foreign-sbx", Shell: "/bin/sh",
				}); !extsdk.IsPermissionDenied(err) {
					return fmt.Errorf("expected permission_denied for foreign sandbox terminal, got %v", err)
				}
				return ctx.Host.OpenTerminal(ctx, extsdk.OpenTerminalRequest{
					TargetType: "container", TargetID: created.Id, Shell: "/bin/sh",
				})
			},
		},
	})
	_ = app.Run(context.Background())
}
`
	extDir := e2e4CreateBinaryExt(t, t.TempDir(), "t5-attacker", binaryManifestTOML("t5-attacker", "0.1.0", "bin/ext", "docker", "sandboxes", "ui:terminal"), attackerSrc)
	if _, err := mgr.InstallLocal(extDir); err != nil {
		t.Fatalf("InstallLocal: %v", err)
	}
	if err := mgr.AllowAndBuild("t5-attacker"); err != nil {
		t.Fatalf("AllowAndBuild: %v", err)
	}
	e2e4WaitExtState(t, mgr, "t5-attacker", 10*time.Second, func(i ext.ExtensionInfo) bool {
		return i.State == ext.StateRunning
	})

	if err := mgr.SendViewEvent("t5-attacker", "main", uiblock.Event{Node: "/attack", Kind: uiblock.EventTap}); err != nil {
		t.Fatalf("SendViewEvent(attack): %v", err)
	}
	terms := h.UIHost.SnapshotTerminals()
	if len(terms) != 1 || terms[0].TargetType != "container" || terms[0].TargetID != "mock-container-1" {
		t.Fatalf("expected 1 owned container terminal recorded, got %+v", terms)
	}
	for _, req := range h.DockerBackend.SnapshotRequests() {
		rawBody := string(req.RawBody)
		if strings.Contains(req.Path, "/networks/") && (strings.Contains(rawBody, "t5-victim-ctr") || strings.Contains(rawBody, "t5-host-ctr")) {
			t.Fatalf("blocked network op leaked to DockerBackend: %s %s %s", req.Method, req.Path, rawBody)
		}
		if strings.HasSuffix(req.Path, "/volumes/create") && strings.Contains(rawBody, "t5-bad") {
			t.Fatalf("blocked volume create leaked to DockerBackend: %s %s %s", req.Method, req.Path, rawBody)
		}
		if strings.HasSuffix(req.Path, "/containers/create") && (strings.Contains(rawBody, "t5-victim") || strings.Contains(rawBody, "/etc")) {
			t.Fatalf("blocked container create leaked to DockerBackend: %s %s %s", req.Method, req.Path, rawBody)
		}
	}
}

func TestT5_02_ConcurrentInstallDuringAllowAndBuildAndDisabledState(t *testing.T) {
	// Part A: Concurrent InstallLocal(v2) must block behind in-flight AllowAndBuild(v1) without mutating disk mid-build.
	dataDir := newShortTempDataDir(t)
	builder := &t5BlockingBuilder{
		started: make(chan struct{}),
		release: make(chan struct{}),
	}
	mgr, _ := e2e4NewManager(t, builder, WithDataDir(dataDir))
	t.Cleanup(func() {
		select {
		case <-builder.release:
		default:
			close(builder.release)
		}
	})

	writeSandboxExt := func(ver, marker string) string {
		dir := t.TempDir()
		writeExtFixture(t, dir, fmt.Sprintf(`schema_version = 1
id = "t5-race"
version = %q
publisher = "acme"
name = "T5 Race"
[runtime]
type = "sandbox"
dockerfile = "Dockerfile"
`, ver), map[string]string{
			"Dockerfile": "FROM alpine:3.20\n",
			"marker.txt": marker,
		})
		return dir
	}

	v1 := writeSandboxExt("0.1.0", "v1")
	v2 := writeSandboxExt("0.2.0", "v2")
	if _, err := mgr.InstallLocal(v1); err != nil {
		t.Fatalf("InstallLocal(v1): %v", err)
	}

	buildDone := make(chan error, 1)
	go func() { buildDone <- mgr.AllowAndBuild("t5-race") }()
	<-builder.started

	installDone := make(chan error, 1)
	go func() {
		_, err := mgr.InstallLocal(v2)
		installDone <- err
	}()

	time.Sleep(80 * time.Millisecond)
	select {
	case err := <-installDone:
		t.Fatalf("InstallLocal(v2) raced ahead of in-flight AllowAndBuild (err=%v)", err)
	default:
	}
	midMan, err := os.ReadFile(ext.ManifestSnapshotPath(dataDir, "t5-race"))
	if err != nil || !strings.Contains(string(midMan), `version = "0.1.0"`) {
		t.Fatalf("manifest snapshot mutated mid-build: got %q err=%v", string(midMan), err)
	}

	close(builder.release)
	_ = <-buildDone
	if err := <-installDone; err != nil {
		t.Fatalf("InstallLocal(v2): %v", err)
	}
	if builder.builtMarker != "v1" {
		t.Fatalf("builder read marker %q, want v1", builder.builtMarker)
	}
	finalMan, err := os.ReadFile(ext.ManifestSnapshotPath(dataDir, "t5-race"))
	if err != nil || !strings.Contains(string(finalMan), `version = "0.2.0"`) {
		t.Fatalf("final manifest snapshot = %q (err=%v), want 0.2.0", string(finalMan), err)
	}

	// Part B: AllowAndBuild on a disabled extension must keep StateDisabled and not launch process.
	mgr2, _ := newTestManager(t, WithDataDir(newShortTempDataDir(t)), WithDeveloperMode(true))
	startedMarker := filepath.Join(t.TempDir(), "started")
	binSrc := fmt.Sprintf(`package main
import ("context"; "os"; "github.com/ironpark/gvx-desktop/pkg/extsdk")
func main() {
	_ = os.WriteFile(%q, []byte("started"), 0o600)
	_ = extsdk.New().Run(context.Background())
}`, startedMarker)
	binDir := e2e4CreateBinaryExt(t, t.TempDir(), "t5-disabled", binaryManifestTOML("t5-disabled", "0.1.0", "bin/ext"), binSrc)
	if _, err := mgr2.InstallLocal(binDir); err != nil {
		t.Fatalf("InstallLocal(t5-disabled): %v", err)
	}
	if err := mgr2.Disable("t5-disabled"); err != nil {
		t.Fatalf("Disable: %v", err)
	}
	if err := mgr2.AllowAndBuild("t5-disabled"); err != nil {
		t.Fatalf("AllowAndBuild(t5-disabled): %v", err)
	}
	info, ok := e2e4FindExtInfo(mgr2, "t5-disabled")
	if !ok || info.State != ext.StateDisabled {
		t.Fatalf("expected StateDisabled after AllowAndBuild, got ok=%v state=%q", ok, info.State)
	}
	if _, err := os.Stat(startedMarker); !os.IsNotExist(err) {
		t.Fatalf("disabled extension executed during AllowAndBuild (stat=%v)", err)
	}
}

func t5TarEntries(t *testing.T, gzTar []byte) map[string]struct{} {
	t.Helper()
	gz, err := gzip.NewReader(bytes.NewReader(gzTar))
	if err != nil {
		t.Fatalf("gzip.NewReader: %v", err)
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	out := map[string]struct{}{}
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			return out
		}
		if err != nil {
			t.Fatalf("tar.Next: %v", err)
		}
		out[hdr.Name] = struct{}{}
	}
}

func TestT5_03_RepoRootDockerfileSubdirContextAndInvalidUTF8Manifest(t *testing.T) {
	dataDir := newShortTempDataDir(t)
	capBuilder := &e2e4ImageBuilder{}
	mgr, _ := e2e4NewManager(t, capBuilder, WithDataDir(dataDir))

	extDir := t.TempDir()
	writeExtFixture(t, extDir, `schema_version = 1
id = "t5-ctx-split"
version = "0.1.0"
publisher = "acme"
name = "T5 Context Split"
[runtime]
type = "sandbox"
dockerfile = "docker/Dockerfile"
context = "sub/ctx"
`, map[string]string{
		"docker/Dockerfile":   "FROM alpine:3.20\nCOPY payload.txt /payload.txt\n",
		"sub/ctx/payload.txt": "ctx-ok",
		"secret.txt":          "should-not-leak",
	})

	if _, err := mgr.InstallLocal(extDir); err != nil {
		t.Fatalf("InstallLocal(t5-ctx-split): %v", err)
	}
	// startLocked will fail because supervisor is nil in e2e4NewManager, AFTER BuildFromDir finishes.
	_ = mgr.AllowAndBuild("t5-ctx-split")
	calls := capBuilder.SnapshotCalls()
	if len(calls) != 1 {
		t.Fatalf("expected 1 Build call, got %d", len(calls))
	}
	files := t5TarEntries(t, calls[0].ContextTar)
	if _, ok := files["payload.txt"]; !ok {
		t.Fatalf("expected sub/ctx/payload.txt at context root, got %v", files)
	}
	if _, ok := files[calls[0].Req.Dockerfile]; !ok {
		t.Fatalf("expected staged dockerfile %q in context tar, got %v", calls[0].Req.Dockerfile, files)
	}
	if _, leaked := files["secret.txt"]; leaked {
		t.Fatalf("repo-root secret.txt leaked into subdir context tar: %v", files)
	}

	// Invalid UTF-8 manifest must be rejected cleanly on InstallLocal.
	badDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(badDir, ext.ManifestName), []byte("schema_version = 1\nid = \"t5-bad\"\nversion = \"0.1.0\"\npublisher = \"acme\"\nname = \"Bad\xffName\"\n[runtime]\ntype = \"sandbox\"\nimage = \"alpine:3.20\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := mgr.InstallLocal(badDir); err == nil {
		t.Fatal("expected InstallLocal to reject invalid UTF-8 manifest")
	}
}

func TestT5_04_UIUninstallDropsStaleViewSessionAndReinstallClean(t *testing.T) {
	dataDir := newShortTempDataDir(t)
	mgr, _ := newTestManager(t, WithDataDir(dataDir), WithDeveloperMode(true))

	viewTOML := binaryManifestTOML("t5-ui-sess", "0.1.0", "bin/ext")
	v1Dir := e2e4CreateBinaryExt(t, t.TempDir(), "t5-ui-sess", viewTOML, `package main
import ("context"; "github.com/ironpark/gvx-desktop/pkg/extsdk")
func main() {
	app := extsdk.New()
	app.View("main", extsdk.View{
		Render: func(c *extsdk.ViewContext, ui *extsdk.UI) extsdk.Node {
			return ui.Column(ui.Text("t5-v1-stale-banner", extsdk.Key("b")))
		},
	})
	_ = app.Run(context.Background())
}`)
	if _, err := mgr.InstallLocal(v1Dir); err != nil {
		t.Fatalf("InstallLocal(v1): %v", err)
	}
	if err := mgr.AllowAndBuild("t5-ui-sess"); err != nil {
		t.Fatalf("AllowAndBuild(v1): %v", err)
	}

	p, m := e2e4StartProbeUIWithManager(t, dataDir, mgr, true, ggui.Sz(1200, 800))
	m.Go(ui.ExtensionPage("t5-ui-sess", "main"))
	e2e4WaitSemantics(t, p, 8*time.Second, func(sem *ggui.SemTree) bool {
		return e2e4SemContains(sem, "t5-v1-stale-banner")
	})

	m.UninstallExtension("t5-ui-sess", true)
	e2e4WaitSemantics(t, p, 8*time.Second, func(sem *ggui.SemTree) bool {
		_, stillInstalled := e2e4FindExtInfo(mgr, "t5-ui-sess")
		return !stillInstalled && !e2e4SemContains(sem, "t5-v1-stale-banner")
	})

	v2Dir := e2e4CreateBinaryExt(t, t.TempDir(), "t5-ui-sess", viewTOML, `package main
import ("context"; "github.com/ironpark/gvx-desktop/pkg/extsdk")
func main() {
	app := extsdk.New()
	app.View("main", extsdk.View{
		Render: func(c *extsdk.ViewContext, ui *extsdk.UI) extsdk.Node {
			return ui.Column(ui.Text("t5-v2-clean-banner", extsdk.Key("b")))
		},
	})
	_ = app.Run(context.Background())
}`)
	if _, err := mgr.InstallLocal(v2Dir); err != nil {
		t.Fatalf("InstallLocal(v2): %v", err)
	}
	if err := mgr.AllowAndBuild("t5-ui-sess"); err != nil {
		t.Fatalf("AllowAndBuild(v2): %v", err)
	}

	m.Go(ui.ExtensionPage("t5-ui-sess", "main"))
	e2e4WaitSemantics(t, p, 8*time.Second, func(sem *ggui.SemTree) bool {
		return e2e4SemContains(sem, "t5-v2-clean-banner") && !e2e4SemContains(sem, "t5-v1-stale-banner")
	})
}

func TestT5_05_DockerEventsTopologyOraclesCommitTerminalAndColdStart(t *testing.T) {
	// Part A: Scoped Docker proxy isolates /events, /volumes, /networks, /images, before/since oracles, and /commit.
	var mu sync.Mutex
	containers := map[string]map[string]string{
		"t5-victim-c": {"io.gvx.extension": "t5-victim"},
		"t5-host-c":   {},
	}
	volumes := map[string]map[string]string{
		"t5-victim-vol": {"io.gvx.extension": "t5-victim"},
		"t5-host-vol":   {},
	}
	networks := map[string]struct {
		id     string
		name   string
		labels map[string]string
		ctrs   map[string]any
	}{
		"bridge": {
			id:     "bridge-id",
			name:   "bridge",
			labels: map[string]string{},
			ctrs: map[string]any{
				"t5-victim-c": map[string]any{"Name": "t5-victim-c", "IPv4Address": "10.0.2.20/24"},
				"t5-host-c":   map[string]any{"Name": "t5-host-c", "IPv4Address": "10.0.2.21/24"},
			},
		},
		"t5-victim-net": {
			id:     "t5-victim-net-id",
			name:   "t5-victim-net",
			labels: map[string]string{"io.gvx.extension": "t5-victim"},
			ctrs: map[string]any{
				"t5-victim-c": map[string]any{"Name": "t5-victim-c", "IPv4Address": "10.89.1.20/24"},
			},
		},
	}
	images := map[string]struct {
		id     string
		labels map[string]string
	}{
		"alpine:3.20":  {id: "sha256:alpine", labels: map[string]string{}},
		"t5-victim:v1": {id: "sha256:victimimg", labels: map[string]string{"io.gvx.extension": "t5-victim"}},
	}
	evCh := make(chan string, 16)
	var ownedCID string

	dockerSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := r.URL.Path
		if len(p) > 3 && strings.HasPrefix(p, "/v") && p[2] >= '0' && p[2] <= '9' {
			if idx := strings.IndexByte(p[2:], '/'); idx > 0 {
				p = p[2+idx:]
			}
		}
		switch {
		case r.Method == http.MethodGet && p == "/events":
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			rc := http.NewResponseController(w)
			_ = rc.Flush()
			for {
				select {
				case <-r.Context().Done():
					return
				case ln, ok := <-evCh:
					if !ok {
						return
					}
					_, _ = io.WriteString(w, ln+"\n")
					_ = rc.Flush()
				}
			}

		case r.Method == http.MethodPost && p == "/containers/create":
			raw, _ := io.ReadAll(r.Body)
			var body struct {
				Labels map[string]string `json:"Labels"`
			}
			_ = json.Unmarshal(raw, &body)
			mu.Lock()
			cid := "t5-attacker-c"
			containers[cid] = body.Labels
			networks["bridge"].ctrs[cid] = map[string]any{"Name": cid, "IPv4Address": "10.0.2.30/24"}
			ownedCID = cid
			mu.Unlock()
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusCreated)
			_, _ = io.WriteString(w, `{"Id":"`+cid+`"}`)

		case r.Method == http.MethodGet && p == "/containers/json":
			f := r.URL.Query().Get("filters")
			if strings.Contains(f, "t5-missing-c") {
				http.Error(w, `{"message":"No such container: t5-missing-c"}`, http.StatusNotFound)
				return
			}
			mu.Lock()
			var list []map[string]any
			for cid, lbls := range containers {
				list = append(list, map[string]any{"Id": cid, "Labels": lbls})
			}
			mu.Unlock()
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(list)

		case r.Method == http.MethodGet && strings.HasPrefix(p, "/containers/") && strings.HasSuffix(p, "/json"):
			cid := strings.TrimSuffix(strings.TrimPrefix(p, "/containers/"), "/json")
			mu.Lock()
			lbls, ok := containers[cid]
			mu.Unlock()
			if !ok {
				http.Error(w, `{"message":"No such container: `+cid+`"}`, http.StatusNotFound)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{"Id": cid, "Config": map[string]any{"Labels": lbls}})

		case r.Method == http.MethodPost && p == "/volumes/create":
			raw, _ := io.ReadAll(r.Body)
			var req struct {
				Name   string            `json:"Name"`
				Labels map[string]string `json:"Labels"`
			}
			_ = json.Unmarshal(raw, &req)
			mu.Lock()
			if existing, ok := volumes[req.Name]; ok {
				mu.Unlock()
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusCreated)
				_ = json.NewEncoder(w).Encode(map[string]any{"Name": req.Name, "Labels": existing})
				return
			}
			volumes[req.Name] = req.Labels
			mu.Unlock()
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(map[string]any{"Name": req.Name, "Labels": req.Labels})

		case r.Method == http.MethodGet && p == "/volumes":
			mu.Lock()
			var list []map[string]any
			for name, lbls := range volumes {
				list = append(list, map[string]any{"Name": name, "Labels": lbls})
			}
			mu.Unlock()
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{"Volumes": list, "Warnings": []string{}})

		case r.Method == http.MethodGet && strings.HasPrefix(p, "/volumes/"):
			name := strings.TrimPrefix(p, "/volumes/")
			mu.Lock()
			lbls, ok := volumes[name]
			mu.Unlock()
			if !ok {
				http.Error(w, `{"message":"no such volume"}`, http.StatusNotFound)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{"Name": name, "Labels": lbls})

		case r.Method == http.MethodDelete && strings.HasPrefix(p, "/volumes/"):
			name := strings.TrimPrefix(p, "/volumes/")
			mu.Lock()
			_, ok := volumes[name]
			delete(volumes, name)
			mu.Unlock()
			if !ok {
				http.Error(w, `{"message":"no such volume"}`, http.StatusNotFound)
				return
			}
			w.WriteHeader(http.StatusNoContent)

		case r.Method == http.MethodPost && p == "/networks/create":
			raw, _ := io.ReadAll(r.Body)
			var req struct {
				Name   string            `json:"Name"`
				Labels map[string]string `json:"Labels"`
			}
			_ = json.Unmarshal(raw, &req)
			mu.Lock()
			networks[req.Name] = struct {
				id     string
				name   string
				labels map[string]string
				ctrs   map[string]any
			}{id: req.Name + "-id", name: req.Name, labels: req.Labels, ctrs: map[string]any{}}
			mu.Unlock()
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(map[string]any{"Id": req.Name + "-id", "Warning": ""})

		case r.Method == http.MethodGet && p == "/networks":
			mu.Lock()
			var list []map[string]any
			for _, n := range networks {
				ctrs := map[string]any{}
				for k, v := range n.ctrs {
					ctrs[k] = v
				}
				list = append(list, map[string]any{"Id": n.id, "Name": n.name, "Labels": n.labels, "Containers": ctrs})
			}
			mu.Unlock()
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(list)

		case r.Method == http.MethodGet && strings.HasPrefix(p, "/networks/"):
			nid := strings.TrimPrefix(p, "/networks/")
			mu.Lock()
			n, ok := networks[nid]
			mu.Unlock()
			if !ok {
				http.Error(w, `{"message":"network not found"}`, http.StatusNotFound)
				return
			}
			ctrs := map[string]any{}
			for k, v := range n.ctrs {
				ctrs[k] = v
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{"Id": n.id, "Name": n.name, "Labels": n.labels, "Containers": ctrs})

		case r.Method == http.MethodDelete && strings.HasPrefix(p, "/networks/"):
			nid := strings.TrimPrefix(p, "/networks/")
			mu.Lock()
			delete(networks, nid)
			mu.Unlock()
			w.WriteHeader(http.StatusNoContent)

		case r.Method == http.MethodGet && p == "/images/json":
			f := r.URL.Query().Get("filters")
			if strings.Contains(f, "t5-missing-img") {
				http.Error(w, `{"message":"No such image: t5-missing-img"}`, http.StatusNotFound)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `[{"Id":"sha256:victimimg","RepoTags":["t5-victim:v1"]}]`)

		case r.Method == http.MethodPost && p == "/images/create":
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(w, `{"status":"sha256:t5imported"}`+"\n")

		case r.Method == http.MethodGet && strings.HasPrefix(p, "/images/") && strings.HasSuffix(p, "/json"):
			ref := strings.TrimSuffix(strings.TrimPrefix(p, "/images/"), "/json")
			mu.Lock()
			img, ok := images[ref]
			mu.Unlock()
			if !ok {
				http.Error(w, `{"message":"No such image"}`, http.StatusNotFound)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{"Id": img.id, "Config": map[string]any{"Labels": img.labels}})

		case r.Method == http.MethodPost && p == "/commit":
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusCreated)
			_, _ = io.WriteString(w, `{"Id":"sha256:t5commit"}`)

		default:
			w.WriteHeader(http.StatusOK)
		}
	}))
	defer dockerSrv.Close()

	dataDir := newShortTempDataDir(t)
	mgr, err := ext.NewManager(ext.Config{
		DataDir:       dataDir,
		Keychain:      newFakeKeychain(t),
		DeveloperMode: func() bool { return true },
		HostInfo:      func() ext.HostInfo { return ext.HostInfo{OS: "darwin", Arch: "arm64", Locale: "en"} },
		DockerBackendDial: func(ctx context.Context) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "tcp", strings.TrimPrefix(dockerSrv.URL, "http://"))
		},
		UIHost: NewMockUIHost(),
	})
	if err != nil {
		t.Fatalf("ext.NewManager: %v", err)
	}
	defer func() { _ = mgr.Close(5 * time.Second) }()

	probeSrc := `package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/ironpark/gvx-desktop/pkg/extsdk"
)

func dockerReq(ctx context.Context, method, p, body string) (int, string, error) {
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
	var r io.Reader
	if body != "" {
		r = bytes.NewBufferString(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, base+p, r)
	if err != nil {
		return 0, "", err
	}
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := (&http.Client{Transport: tr}).Do(req)
	if err != nil {
		return 0, "", err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(raw), nil
}

func main() {
	app := extsdk.New()
	app.View("main", extsdk.View{
		Render: func(c *extsdk.ViewContext, ui *extsdk.UI) extsdk.Node {
			return ui.Column(ui.Button("RunT5", extsdk.Key("run"), extsdk.OnTap("run")))
		},
		On: extsdk.Handlers{
			"run": func(ctx *extsdk.EventContext) error {
				if c, b, e := dockerReq(ctx, http.MethodPost, "/containers/create", ` + "`" + `{"Image":"alpine:3.20"}` + "`" + `); e != nil || c != 201 {
					return fmt.Errorf("create container: %d %s %v", c, b, e)
				}
				if c, b, e := dockerReq(ctx, http.MethodPost, "/volumes/create", ` + "`" + `{"Name":"t5-own-vol"}` + "`" + `); e != nil || c != 201 {
					return fmt.Errorf("create vol: %d %s %v", c, b, e)
				}
				if c, b, e := dockerReq(ctx, http.MethodPost, "/volumes/create", ` + "`" + `{"Name":"t5-victim-vol"}` + "`" + `); e != nil || c != 403 {
					return fmt.Errorf("adopt foreign vol expected 403, got %d %s %v", c, b, e)
				}
				if c, b, e := dockerReq(ctx, http.MethodGet, "/volumes", ""); e != nil || c != 200 || strings.Contains(b, "t5-victim-vol") || strings.Contains(b, "t5-host-vol") || !strings.Contains(b, "t5-own-vol") {
					return fmt.Errorf("GET /volumes leak: %d %s %v", c, b, e)
				}
				if c, b, e := dockerReq(ctx, http.MethodDelete, "/volumes/t5-victim-vol", ""); e != nil || c != 403 {
					return fmt.Errorf("DELETE foreign vol expected 403, got %d %s %v", c, b, e)
				}
				if c, b, e := dockerReq(ctx, http.MethodPost, "/networks/create", ` + "`" + `{"Name":"t5-own-net"}` + "`" + `); e != nil || c != 201 {
					return fmt.Errorf("create net: %d %s %v", c, b, e)
				}
				if c, b, e := dockerReq(ctx, http.MethodGet, "/networks", ""); e != nil || c != 200 || strings.Contains(b, "t5-victim") || strings.Contains(b, "t5-host-c") || !strings.Contains(b, "t5-attacker-c") || !strings.Contains(b, "t5-own-net") {
					return fmt.Errorf("GET /networks leak: %d %s %v", c, b, e)
				}
				if c, b, e := dockerReq(ctx, http.MethodDelete, "/networks/bridge", ""); e != nil || c != 403 {
					return fmt.Errorf("DELETE bridge expected 403, got %d %s %v", c, b, e)
				}
				if c, b, e := dockerReq(ctx, http.MethodDelete, "/networks/t5-victim-net", ""); e != nil || c != 403 {
					return fmt.Errorf("DELETE foreign net expected 403, got %d %s %v", c, b, e)
				}
				if c, b, e := dockerReq(ctx, http.MethodPost, "/networks/t5-victim-net/connect", ` + "`" + `{"Container":"t5-attacker-c"}` + "`" + `); e != nil || c != 403 {
					return fmt.Errorf("connect foreign net expected 403, got %d %s %v", c, b, e)
				}
				if c, b, e := dockerReq(ctx, http.MethodPost, "/networks/t5-victim-net/disconnect", ` + "`" + `{"Container":"t5-attacker-c"}` + "`" + `); e != nil || c != 403 {
					return fmt.Errorf("disconnect foreign net expected 403, got %d %s %v", c, b, e)
				}
				if c, b, e := dockerReq(ctx, http.MethodPost, "/networks/host/connect", ` + "`" + `{"Container":"t5-attacker-c"}` + "`" + `); e != nil || c != 403 {
					return fmt.Errorf("connect host net expected 403, got %d %s %v", c, b, e)
				}
				if c, b, e := dockerReq(ctx, http.MethodPost, "/containers/create", ` + "`" + `{"Image":"alpine:3.20","NetworkingConfig":{"EndpointsConfig":{"host":{}}}}` + "`" + `); e != nil || c != 403 {
					return fmt.Errorf("create EndpointsConfig host expected 403, got %d %s %v", c, b, e)
				}
				if c, b, e := dockerReq(ctx, http.MethodPost, "/containers/create", ` + "`" + `{"Image":"alpine:3.20","NetworkingConfig":{"EndpointsConfig":{"t5-victim-net":{}}}}` + "`" + `); e != nil || c != 403 {
					return fmt.Errorf("create EndpointsConfig foreign net expected 403, got %d %s %v", c, b, e)
				}
				if c, b, e := dockerReq(ctx, http.MethodPost, "/networks/t5-own-net/connect", ` + "`" + `{"Container":"t5-attacker-c"}` + "`" + `); e != nil || c != 200 {
					return fmt.Errorf("connect owned net expected 200, got %d %s %v", c, b, e)
				}
				if c, b, e := dockerReq(ctx, http.MethodDelete, "/images/alpine:3.20?force=1", ""); e != nil || c != 403 {
					return fmt.Errorf("DELETE host image expected 403, got %d %s %v", c, b, e)
				}
				if c, b, e := dockerReq(ctx, http.MethodDelete, "/images/t5-victim:v1?force=1", ""); e != nil || c != 403 {
					return fmt.Errorf("DELETE foreign image expected 403, got %d %s %v", c, b, e)
				}
				ic1, ib1, _ := dockerReq(ctx, http.MethodGet, "/images/json?filters="+url.QueryEscape(` + "`" + `{"before":["t5-victim:v1"]}` + "`" + `), "")
				ic2, ib2, _ := dockerReq(ctx, http.MethodGet, "/images/json?filters="+url.QueryEscape(` + "`" + `{"before":["t5-missing-img:v1"]}` + "`" + `), "")
				if ic1 != 403 || ic2 != 403 || ib1 != ib2 {
					return fmt.Errorf("images/json before oracle: (%d,%s) vs (%d,%s)", ic1, ib1, ic2, ib2)
				}
				if c, b, _ := dockerReq(ctx, http.MethodPost, "/images/create?fromSrc=-&repo=t5-victim&tag=v1", ""); c != 403 {
					return fmt.Errorf("images/create overwrite foreign tag expected 403, got %d %s", c, b)
				}
				if c, b, _ := dockerReq(ctx, http.MethodPost, "/images/create?fromSrc=-&repo=t5-own-img&tag=1&changes="+url.QueryEscape("LABEL io.gvx.extension=t5-victim"), ""); c != 403 {
					return fmt.Errorf("images/create spoofed label expected 403, got %d %s", c, b)
				}
				if c, b, _ := dockerReq(ctx, http.MethodPost, "/images/load", ""); c != 403 {
					return fmt.Errorf("images/load expected 403, got %d %s", c, b)
				}
				c1, b1, _ := dockerReq(ctx, http.MethodGet, "/containers/json?all=1&filters="+url.QueryEscape(` + "`" + `{"before":["t5-victim-c"]}` + "`" + `), "")
				c2, b2, _ := dockerReq(ctx, http.MethodGet, "/containers/json?all=1&filters="+url.QueryEscape(` + "`" + `{"before":["t5-missing-c"]}` + "`" + `), "")
				if c1 != 403 || c2 != 403 || b1 != b2 {
					return fmt.Errorf("before oracle: (%d,%s) vs (%d,%s)", c1, b1, c2, b2)
				}
				if c, b, _ := dockerReq(ctx, http.MethodPost, "/commit?container=t5-attacker-c&container=t5-victim-c", ""); c != 400 {
					return fmt.Errorf("multi-param commit expected 400, got %d %s", c, b)
				}
				if c, b, _ := dockerReq(ctx, http.MethodPost, "/commit?container=&container=t5-victim-c", ""); c != 400 {
					return fmt.Errorf("empty+foreign param commit expected 400, got %d %s", c, b)
				}
				if c, b, _ := dockerReq(ctx, http.MethodPost, "/commit?container=t5-attacker-c&changes="+url.QueryEscape("ENV A=1\nLABEL io.gvx.extension=t5-victim"), ""); c != 403 {
					return fmt.Errorf("multiline commit label spoof expected 403, got %d %s", c, b)
				}
				if c, b, _ := dockerReq(ctx, http.MethodPost, "/commit?container=t5-attacker-c&changes="+url.QueryEscape("LABEL io.\\gvx.extension=t5-victim"), ""); c != 403 {
					return fmt.Errorf("escaped commit label spoof expected 403, got %d %s", c, b)
				}
				if c, b, _ := dockerReq(ctx, http.MethodPost, "/commit?container=t5-attacker-c", ""); c != 201 {
					return fmt.Errorf("owned commit expected 201, got %d %s", c, b)
				}

				// Verify /events stream drops victim/host events and delivers attacker event.
				ep := os.Getenv("GVX_DOCKER_HOST")
				tr := &http.Transport{DialContext: func(c context.Context, _, _ string) (net.Conn, error) {
					return (&net.Dialer{}).DialContext(c, "unix", strings.TrimPrefix(ep, "unix://"))
				}}
				evCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
				defer cancel()
				req, _ := http.NewRequestWithContext(evCtx, http.MethodGet, "http://docker/events", nil)
				resp, err := (&http.Client{Transport: tr}).Do(req)
				if err != nil {
					return err
				}
				defer resp.Body.Close()
				buf := make([]byte, 4096)
				n, _ := resp.Body.Read(buf)
				got := string(buf[:n])
				if strings.Contains(got, "t5-victim-c") || strings.Contains(got, "secret-leak") || !strings.Contains(got, "t5-attacker-c") {
					return fmt.Errorf("GET /events leaked or missed owned event: %q", got)
				}
				return nil
			},
		},
	})
	_ = app.Run(context.Background())
}
`
	extDir := e2e4CreateBinaryExt(t, t.TempDir(), "t5-probe", binaryManifestTOML("t5-probe", "0.1.0", "bin/ext", "docker"), probeSrc)
	if _, err := mgr.InstallLocal(extDir); err != nil {
		t.Fatalf("InstallLocal: %v", err)
	}
	if err := mgr.AllowAndBuild("t5-probe"); err != nil {
		t.Fatalf("AllowAndBuild: %v", err)
	}
	e2e4WaitExtState(t, mgr, "t5-probe", 10*time.Second, func(i ext.ExtensionInfo) bool {
		return i.State == ext.StateRunning
	})

	// Feed events concurrently once the extension calls GET /events.
	go func() {
		time.Sleep(150 * time.Millisecond)
		evCh <- `{"Type":"container","Action":"start","Actor":{"ID":"t5-victim-c","Attributes":{"io.gvx.extension":"t5-victim","secret":"secret-leak"}}}`
		evCh <- `{"Type":"network","Action":"connect","Actor":{"ID":"bridge-id","Attributes":{"container":"t5-victim-c","name":"bridge"}}}`
		evCh <- `{"Type":"volume","Action":"create","Actor":{"ID":"t5-victim-vol"}}`
		evCh <- `{"Type":"container","Action":"start","Actor":{"ID":"t5-attacker-c","Attributes":{"io.gvx.extension":"t5-probe"}}}`
	}()

	if err := mgr.SendViewEvent("t5-probe", "main", uiblock.Event{Node: "/run", Kind: uiblock.EventTap}); err != nil {
		t.Fatalf("SendViewEvent(run): %v", err)
	}
	_ = ownedCID

	// Part B: OpenTerminal(container) fails closed without PermDocker and with nil Docker backend.
	termNoDockerSrc := `package main
import (
	"context"
	"fmt"
	"github.com/ironpark/gvx-desktop/pkg/extsdk"
)
func main() {
	app := extsdk.New()
	app.Command("open", func(ctx *extsdk.CommandContext) error {
		err := ctx.Host.OpenTerminal(ctx, extsdk.OpenTerminalRequest{TargetType: "container", TargetID: "t5-host-c", Shell: "/bin/sh"})
		if err == nil {
			return fmt.Errorf("expected error for container terminal without PermDocker/backend")
		}
		return nil
	})
	_ = app.Run(context.Background())
}`
	cmdSuffix := "\n[[contributes.command]]\nid = \"open\"\ntitle = \"Open\"\n"
	noPermDir := e2e4CreateBinaryExt(t, t.TempDir(), "t5-term-noperm", binaryManifestTOML("t5-term-noperm", "0.1.0", "bin/ext", "ui:terminal")+cmdSuffix, termNoDockerSrc)
	if _, err := mgr.InstallLocal(noPermDir); err != nil {
		t.Fatalf("InstallLocal(t5-term-noperm): %v", err)
	}
	if err := mgr.AllowAndBuild("t5-term-noperm"); err != nil {
		t.Fatalf("AllowAndBuild(t5-term-noperm): %v", err)
	}
	e2e4WaitExtState(t, mgr, "t5-term-noperm", 10*time.Second, func(i ext.ExtensionInfo) bool { return i.State == ext.StateRunning })
	if err := mgr.RunCommand("t5-term-noperm", "open", "en"); err != nil {
		t.Fatalf("RunCommand(t5-term-noperm): %v", err)
	}

	mgrNilDocker, hNil := newTestManager(t, WithDataDir(newShortTempDataDir(t)), WithDeveloperMode(true))
	// Override DockerBackendDial via a fresh Manager with nil DockerBackendDial and nil DockerEngineHandler.
	mgrStrictNil, err := ext.NewManager(ext.Config{
		DataDir:       newShortTempDataDir(t),
		Keychain:      newFakeKeychain(t),
		DeveloperMode: func() bool { return true },
		HostInfo:      func() ext.HostInfo { return ext.HostInfo{OS: "darwin", Arch: "arm64"} },
		UIHost:        hNil.UIHost,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := mgrStrictNil.Start(context.Background()); err != nil {
		t.Fatalf("mgrStrictNil.Start: %v", err)
	}
	defer func() { _ = mgrStrictNil.Close(5 * time.Second) }()
	_ = mgrNilDocker
	nilDir := e2e4CreateBinaryExt(t, t.TempDir(), "t5-term-nil", binaryManifestTOML("t5-term-nil", "0.1.0", "bin/ext", "ui:terminal", "docker")+cmdSuffix, termNoDockerSrc)
	if _, err := mgrStrictNil.InstallLocal(nilDir); err != nil {
		t.Fatalf("InstallLocal(t5-term-nil): %v", err)
	}
	if err := mgrStrictNil.AllowAndBuild("t5-term-nil"); err != nil {
		t.Fatalf("AllowAndBuild(t5-term-nil): %v", err)
	}
	e2e4WaitExtState(t, mgrStrictNil, "t5-term-nil", 10*time.Second, func(i ext.ExtensionInfo) bool { return i.State == ext.StateRunning })
	if err := mgrStrictNil.RunCommand("t5-term-nil", "open", "en"); err != nil {
		t.Fatalf("RunCommand(t5-term-nil): %v", err)
	}
	if terms := hNil.UIHost.SnapshotTerminals(); len(terms) != 0 {
		t.Fatalf("expected 0 terminals opened on nil Docker backend, got %+v", terms)
	}

	// Part C: Short-timeout SubscribeView during cold-start does not poison extension to StateFailed.
	coldDelayMarker := filepath.Join(t.TempDir(), "cold-delay")
	coldSrc := fmt.Sprintf(`package main
import (
	"context"
	"os"
	"time"
	"github.com/ironpark/gvx-desktop/pkg/extsdk"
)
func main() {
	if _, err := os.Stat(%q); err == nil {
		time.Sleep(300 * time.Millisecond)
	}
	app := extsdk.New()
	app.View("main", extsdk.View{Render: func(c *extsdk.ViewContext, ui *extsdk.UI) extsdk.Node {
		return ui.Column(ui.Text("t5-cold-ok", extsdk.Key("ok")))
	}})
	_ = app.Run(context.Background())
}`, coldDelayMarker)
	coldTOML := `schema_version = 1
id = "t5-cold"
version = "0.1.0"
publisher = "acme"
name = "T5 Cold"
[runtime]
type = "binary"
dev_entrypoint = "bin/ext"
idle_timeout_sec = 1
[[contributes.view]]
id = "main"
title = "Main"
`
	coldDir := e2e4CreateBinaryExt(t, t.TempDir(), "t5-cold", coldTOML, coldSrc)
	if _, err := mgrStrictNil.InstallLocal(coldDir); err != nil {
		t.Fatalf("InstallLocal(t5-cold): %v", err)
	}
	if err := mgrStrictNil.AllowAndBuild("t5-cold"); err != nil {
		t.Fatalf("AllowAndBuild(t5-cold): %v", err)
	}
	e2e4WaitExtState(t, mgrStrictNil, "t5-cold", 10*time.Second, func(i ext.ExtensionInfo) bool { return i.State == ext.StateRunning })
	e2e4WaitExtState(t, mgrStrictNil, "t5-cold", 5*time.Second, func(i ext.ExtensionInfo) bool { return i.State == ext.StateDiscovered })

	if err := os.WriteFile(coldDelayMarker, []byte("1"), 0o600); err != nil {
		t.Fatal(err)
	}
	shortCtx, shortCancel := context.WithTimeout(context.Background(), 25*time.Millisecond)
	defer shortCancel()
	if _, err := mgrStrictNil.SubscribeView(shortCtx, "t5-cold", "main", "en"); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("expected DeadlineExceeded on short SubscribeView, got %v", err)
	}
	info := e2e4WaitExtState(t, mgrStrictNil, "t5-cold", 10*time.Second, func(i ext.ExtensionInfo) bool {
		return i.State == ext.StateRunning || i.State == ext.StateFailed
	})
	if info.State != ext.StateRunning {
		t.Fatalf("short SubscribeView poisoned extension to %q: %s", info.State, info.Error)
	}
}

func TestT5_06_DockerNamedVolumesBuildImageLeakAndSandboxMountVolume(t *testing.T) {
	var mu sync.Mutex
	volumes := map[string]map[string]string{
		"t5-victim-vol": {"io.gvx.extension": "t5-victim"},
		"t5-user-vol":   {},
	}
	images := map[string]struct {
		id     string
		labels map[string]string
	}{
		"alpine:3.20":                   {id: "sha256:alpine320", labels: map[string]string{}},
		"t5-victim:v1":                  {id: "sha256:victimv1", labels: map[string]string{"io.gvx.extension": "t5-victim"}},
		"gvx-ext/t5-victim:1.0.0-local": {id: "sha256:victimext", labels: map[string]string{"io.gvx.extension": "t5-victim"}},
		"t5-own:v1":                     {id: "sha256:ownv1", labels: map[string]string{"io.gvx.extension": "t5-probe6"}},
	}

	dockerSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := r.URL.Path
		if len(p) > 3 && strings.HasPrefix(p, "/v") && p[2] >= '0' && p[2] <= '9' {
			if idx := strings.IndexByte(p[2:], '/'); idx > 0 {
				p = p[2+idx:]
			}
		}
		switch {
		case r.Method == http.MethodPost && p == "/volumes/create":
			raw, _ := io.ReadAll(r.Body)
			var req struct {
				Name   string            `json:"Name"`
				Labels map[string]string `json:"Labels"`
			}
			_ = json.Unmarshal(raw, &req)
			mu.Lock()
			if existing, ok := volumes[req.Name]; ok {
				mu.Unlock()
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusCreated)
				_ = json.NewEncoder(w).Encode(map[string]any{"Name": req.Name, "Labels": existing})
				return
			}
			volumes[req.Name] = req.Labels
			mu.Unlock()
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(map[string]any{"Name": req.Name, "Labels": req.Labels})

		case r.Method == http.MethodGet && strings.HasPrefix(p, "/volumes/"):
			name := strings.TrimPrefix(p, "/volumes/")
			mu.Lock()
			lbls, ok := volumes[name]
			mu.Unlock()
			if !ok {
				http.Error(w, `{"message":"no such volume"}`, http.StatusNotFound)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{"Name": name, "Labels": lbls})

		case r.Method == http.MethodPost && p == "/containers/create":
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusCreated)
			_, _ = io.WriteString(w, `{"Id":"t5-c6"}`)

		case r.Method == http.MethodGet && p == "/images/json":
			mu.Lock()
			var list []map[string]any
			for tag, img := range images {
				list = append(list, map[string]any{"Id": img.id, "RepoTags": []string{tag}, "Labels": img.labels})
			}
			mu.Unlock()
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(list)

		case r.Method == http.MethodGet && strings.HasPrefix(p, "/images/") && strings.HasSuffix(p, "/json"):
			ref := strings.TrimSuffix(strings.TrimPrefix(p, "/images/"), "/json")
			mu.Lock()
			img, ok := images[ref]
			mu.Unlock()
			if !ok {
				http.Error(w, `{"message":"No such image"}`, http.StatusNotFound)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{"Id": img.id, "Config": map[string]any{"Labels": img.labels}})

		case r.Method == http.MethodGet && strings.HasPrefix(p, "/images/") && strings.HasSuffix(p, "/history"):
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `[{"Id":"sha256:hist"}]`)

		case r.Method == http.MethodGet && (p == "/images/get" || (strings.HasPrefix(p, "/images/") && strings.HasSuffix(p, "/get"))):
			w.Header().Set("Content-Type", "application/x-tar")
			_, _ = io.WriteString(w, "tar-bytes")

		case r.Method == http.MethodPost && strings.HasPrefix(p, "/images/") && strings.HasSuffix(p, "/tag"):
			w.WriteHeader(http.StatusCreated)

		case r.Method == http.MethodPost && p == "/images/create":
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"status":"Pulled"}`+"\n")

		default:
			w.WriteHeader(http.StatusOK)
		}
	}))
	defer dockerSrv.Close()

	dataDir := newShortTempDataDir(t)
	mgr, err := ext.NewManager(ext.Config{
		DataDir:       dataDir,
		Keychain:      newFakeKeychain(t),
		DeveloperMode: func() bool { return true },
		HostInfo:      func() ext.HostInfo { return ext.HostInfo{OS: "darwin", Arch: "arm64", Locale: "en"} },
		DockerBackendDial: func(ctx context.Context) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "tcp", strings.TrimPrefix(dockerSrv.URL, "http://"))
		},
		UIHost: NewMockUIHost(),
	})
	if err != nil {
		t.Fatalf("ext.NewManager: %v", err)
	}
	defer func() { _ = mgr.Close(5 * time.Second) }()

	probeSrc := `package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strings"

	"github.com/ironpark/gvx-desktop/pkg/extsdk"
)

func dReq(ctx context.Context, method, p, body string) (int, string, error) {
	ep := os.Getenv("GVX_DOCKER_HOST")
	tr := &http.Transport{DialContext: func(c context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(c, "unix", strings.TrimPrefix(ep, "unix://"))
	}}
	var r io.Reader
	if body != "" {
		r = bytes.NewBufferString(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, "http://docker"+p, r)
	if err != nil {
		return 0, "", err
	}
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := (&http.Client{Transport: tr}).Do(req)
	if err != nil {
		return 0, "", err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(raw), nil
}

func main() {
	app := extsdk.New()
	app.View("main", extsdk.View{
		Render: func(c *extsdk.ViewContext, ui *extsdk.UI) extsdk.Node {
			return ui.Column(ui.Button("Run", extsdk.Key("run"), extsdk.OnTap("run")))
		},
		On: extsdk.Handlers{
			"run": func(ctx *extsdk.EventContext) error {
				// Gap 1: Binds & Mounts volume checks.
				for _, tc := range []struct {
					name string
					body string
				}{
					{"binds-victim", ` + "`" + `{"Image":"alpine:3.20","HostConfig":{"Binds":["t5-victim-vol:/m"]}}` + "`" + `},
					{"binds-user", ` + "`" + `{"Image":"alpine:3.20","HostConfig":{"Binds":["t5-user-vol:/m"]}}` + "`" + `},
					{"binds-buildkit", ` + "`" + `{"Image":"alpine:3.20","HostConfig":{"Binds":["gvx-buildkit:/m"]}}` + "`" + `},
					{"binds-implicit", ` + "`" + `{"Image":"alpine:3.20","HostConfig":{"Binds":["stealth-vol:/m"]}}` + "`" + `},
					{"mounts-victim", ` + "`" + `{"Image":"alpine:3.20","HostConfig":{"Mounts":[{"Type":"volume","Source":"t5-victim-vol","Target":"/m"}]}}` + "`" + `},
					{"mounts-user", ` + "`" + `{"Image":"alpine:3.20","HostConfig":{"Mounts":[{"Type":"volume","Source":"t5-user-vol","Target":"/m"}]}}` + "`" + `},
					{"mounts-buildkit", ` + "`" + `{"Image":"alpine:3.20","HostConfig":{"Mounts":[{"Type":"volume","Source":"gvx-buildkit","Target":"/m"}]}}` + "`" + `},
					{"mounts-unknown", ` + "`" + `{"Image":"alpine:3.20","HostConfig":{"Mounts":[{"Type":"weird","Source":"t5-own-vol","Target":"/m"}]}}` + "`" + `},
				} {
					if c, b, e := dReq(ctx, http.MethodPost, "/containers/create", tc.body); e != nil || c != 403 {
						return fmt.Errorf("%s expected 403, got %d (%s) err=%v", tc.name, c, b, e)
					}
				}
				if c, b, e := dReq(ctx, http.MethodPost, "/volumes/create", ` + "`" + `{"Name":"t5-own-vol6"}` + "`" + `); e != nil || c != 201 {
					return fmt.Errorf("create own vol: %d %s %v", c, b, e)
				}
				if c, b, e := dReq(ctx, http.MethodPost, "/containers/create", ` + "`" + `{"Image":"alpine:3.20","HostConfig":{"Binds":["t5-own-vol6:/a"],"Mounts":[{"Type":"volume","Source":"t5-own-vol6","Target":"/b"}]}}` + "`" + `); e != nil || c != 201 {
					return fmt.Errorf("mount own vol: %d %s %v", c, b, e)
				}

				// Gap 2: POST /v1/sandboxes rejects MountVolume.
				startFalse := false
				for _, src := range []string{"gvx-buildkit", "t5-user-vol", "t5-victim-vol"} {
					_, err := ctx.Host.SandboxCreate(ctx, extsdk.CreateSandboxRequest{
						Name:   "sbx-vol",
						Image:  "alpine:3.20",
						Mounts: []extsdk.SandboxMount{{Type: "volume", Source: src, Target: "/mnt"}},
						Start:  &startFalse,
					})
					if !extsdk.IsPermissionDenied(err) {
						return fmt.Errorf("SandboxCreate MountVolume(%s) expected permission_denied, got %v", src, err)
					}
				}

				// Gap 3: POST /build, /session, /grpc, /system/df blocked.
				for _, tc := range []struct{ m, p string }{
					{http.MethodPost, "/build"},
					{http.MethodPost, "/v1.52/build"},
					{http.MethodPost, "/session"},
					{http.MethodPost, "/grpc"},
					{http.MethodGet, "/system/df"},
				} {
					if c, b, e := dReq(ctx, tc.m, tc.p, ""); e != nil || c != 403 {
						return fmt.Errorf("%s %s expected 403, got %d (%s) err=%v", tc.m, tc.p, c, b, e)
					}
				}

				// Gap 4: Source image isolation & fromImage pull check.
				if c, b, e := dReq(ctx, http.MethodGet, "/images/json", ""); e != nil || c != 200 || strings.Contains(b, "t5-victim") || !strings.Contains(b, "alpine:3.20") || !strings.Contains(b, "t5-own:v1") {
					return fmt.Errorf("GET /images/json isolation: %d %s %v", c, b, e)
				}
				for _, tc := range []struct {
					m, p string
					want int
				}{
					{http.MethodGet, "/images/t5-victim:v1/json", 403},
					{http.MethodGet, "/images/t5-victim:v1/history", 403},
					{http.MethodGet, "/images/t5-victim:v1/get", 403},
					{http.MethodGet, "/images/get?names=alpine:3.20&names=t5-victim:v1", 403},
					{http.MethodPost, "/images/t5-victim:v1/tag?repo=atk&tag=1", 403},
					{http.MethodPost, "/images/create?fromImage=t5-victim&tag=v1", 403},
					{http.MethodGet, "/images/alpine:3.20/json", 200},
					{http.MethodGet, "/images/alpine:3.20/history", 200},
					{http.MethodGet, "/images/alpine:3.20/get", 200},
					{http.MethodGet, "/images/get?names=alpine:3.20", 200},
					{http.MethodPost, "/images/alpine:3.20/tag?repo=t5-atk-base&tag=1", 201},
					{http.MethodPost, "/images/create?fromImage=alpine&tag=3.20", 200},
				} {
					if c, b, e := dReq(ctx, tc.m, tc.p, ""); e != nil || c != tc.want {
						return fmt.Errorf("%s %s expected %d, got %d (%s) err=%v", tc.m, tc.p, tc.want, c, b, e)
					}
				}

				// Gap 4b (R5 Finding 1): POST /containers/create and POST /v1/sandboxes reject foreign images.
				for _, tc := range []struct {
					name string
					body string
					want int
				}{
					{"create-victim-tag", ` + "`" + `{"Image":"t5-victim:v1"}` + "`" + `, 403},
					{"create-victim-gvx-ext", ` + "`" + `{"Image":"gvx-ext/t5-victim:1.0.0-local"}` + "`" + `, 403},
					{"create-own-tag", ` + "`" + `{"Image":"t5-own:v1"}` + "`" + `, 201},
					{"create-alpine", ` + "`" + `{"Image":"alpine:3.20"}` + "`" + `, 201},
				} {
					if c, b, e := dReq(ctx, http.MethodPost, "/containers/create", tc.body); e != nil || c != tc.want {
						return fmt.Errorf("%s expected %d, got %d (%s) err=%v", tc.name, tc.want, c, b, e)
					}
				}
				for _, foreignImg := range []string{"t5-victim:v1", "gvx-ext/t5-victim:1.0.0-local"} {
					if _, err := ctx.Host.SandboxCreate(ctx, extsdk.CreateSandboxRequest{
						Name:  "sbx-steal",
						Image: foreignImg,
						Start: &startFalse,
					}); !extsdk.IsPermissionDenied(err) {
						return fmt.Errorf("SandboxCreate(%s) expected permission_denied, got %v", foreignImg, err)
					}
				}
				for _, okImg := range []string{"t5-own:v1", "alpine:3.20"} {
					sb, err := ctx.Host.SandboxCreate(ctx, extsdk.CreateSandboxRequest{
						Name:  "sbx-ok",
						Image: okImg,
						Start: &startFalse,
					})
					if err != nil {
						return fmt.Errorf("SandboxCreate(%s) expected success, got %v", okImg, err)
					}
					_ = ctx.Host.SandboxDelete(ctx, sb.ID)
				}
				return nil
			},
		},
	})
	_ = app.Run(context.Background())
}
`
	extDir := e2e4CreateBinaryExt(t, t.TempDir(), "t5-probe6", binaryManifestTOML("t5-probe6", "0.1.0", "bin/ext", "docker", "sandboxes", "network"), probeSrc)
	if _, err := mgr.InstallLocal(extDir); err != nil {
		t.Fatalf("InstallLocal: %v", err)
	}
	if err := mgr.AllowAndBuild("t5-probe6"); err != nil {
		t.Fatalf("AllowAndBuild: %v", err)
	}
	e2e4WaitExtState(t, mgr, "t5-probe6", 10*time.Second, func(i ext.ExtensionInfo) bool {
		return i.State == ext.StateRunning
	})
	if err := mgr.SendViewEvent("t5-probe6", "main", uiblock.Event{Node: "/run", Kind: uiblock.EventTap}); err != nil {
		t.Fatalf("SendViewEvent(run): %v", err)
	}
}

func TestT5_07_DockerSSRFNetworkPermCrossArchTagReservedNSAndDigest(t *testing.T) {
	foreignArch := "amd64"
	if runtime.GOARCH == "amd64" {
		foreignArch = "arm64"
	}
	hostArch := runtime.GOARCH

	var ssrfHits atomic.Int32
	ssrfCanary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ssrfHits.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer ssrfCanary.Close()

	type imgEntry struct {
		id     string
		arch   string
		labels map[string]string
	}
	var mu sync.Mutex
	hostImgs := map[string]imgEntry{
		"alpine:3.20":  {id: "sha256:1111111111111111111111111111111111111111111111111111111111111111", arch: hostArch, labels: map[string]string{}},
		"t5-victim:v1": {id: "sha256:2222222222222222222222222222222222222222222222222222222222222222", arch: hostArch, labels: map[string]string{"io.gvx.extension": "t5-victim"}},
		"t5-own:v1":    {id: "sha256:3333333333333333333333333333333333333333333333333333333333333333", arch: hostArch, labels: map[string]string{"io.gvx.extension": "t5-probe7-net"}},
		"shared:1":     {id: "sha256:aaaa000000000000000000000000000000000000000000000000000000000001", arch: hostArch, labels: map[string]string{"io.gvx.extension": "t5-probe7-nonet"}},
	}
	foreignImgs := map[string]imgEntry{
		"atk-foreign:1": {id: "sha256:bbbb000000000000000000000000000000000000000000000000000000000001", arch: foreignArch, labels: map[string]string{"io.gvx.extension": "t5-probe7-nonet"}},
		"shared:1":      {id: "sha256:bbbb000000000000000000000000000000000000000000000000000000000002", arch: foreignArch, labels: map[string]string{"io.gvx.extension": "t5-victim"}},
	}

	lookupImg := func(ref, platform string) (imgEntry, bool) {
		mu.Lock()
		defer mu.Unlock()
		findIn := func(m map[string]imgEntry) (imgEntry, bool) {
			if e, ok := m[ref]; ok {
				return e, true
			}
			for _, e := range m {
				if e.id == ref {
					return e, true
				}
			}
			return imgEntry{}, false
		}
		if strings.Contains(platform, foreignArch) {
			return findIn(foreignImgs)
		}
		if strings.Contains(platform, hostArch) {
			return findIn(hostImgs)
		}
		if e, ok := findIn(hostImgs); ok {
			return e, true
		}
		return findIn(foreignImgs)
	}

	var mutatingCrossArch atomic.Int32
	dockerSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := r.URL.Path
		if len(p) > 3 && strings.HasPrefix(p, "/v") && p[2] >= '0' && p[2] <= '9' {
			if idx := strings.IndexByte(p[2:], '/'); idx > 0 {
				p = p[2+idx:]
			}
		}
		switch {
		case r.Method == http.MethodGet && strings.HasPrefix(p, "/images/") && strings.HasSuffix(p, "/json"):
			ref := strings.TrimSuffix(strings.TrimPrefix(p, "/images/"), "/json")
			e, ok := lookupImg(ref, r.URL.Query().Get("platform"))
			if !ok {
				http.Error(w, `{"message":"No such image"}`, http.StatusNotFound)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"Id":           e.id,
				"Os":           "linux",
				"Architecture": e.arch,
				"Config":       map[string]any{"Labels": e.labels},
			})

		case r.Method == http.MethodGet && p == "/containers/c-foreign-arch/json":
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"Id":     "c-foreign-arch",
				"Image":  "sha256:bbbb000000000000000000000000000000000000000000000000000000000001",
				"Config": map[string]any{"Labels": map[string]string{"io.gvx.extension": "t5-probe7-nonet"}},
			})

		case r.Method == http.MethodPost && strings.HasPrefix(p, "/images/") && strings.HasSuffix(p, "/tag"):
			src := strings.TrimSuffix(strings.TrimPrefix(p, "/images/"), "/tag")
			repo := r.URL.Query().Get("repo")
			tag := r.URL.Query().Get("tag")
			if src == "atk-foreign:1" && repo == "shared" && tag == "1" {
				mutatingCrossArch.Add(1)
			}
			w.WriteHeader(http.StatusCreated)

		case r.Method == http.MethodPost && p == "/commit":
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusCreated)
			_, _ = io.WriteString(w, `{"Id":"sha256:committed"}`)

		case r.Method == http.MethodPost && p == "/images/create":
			if fromSrc := r.URL.Query().Get("fromSrc"); fromSrc != "" && fromSrc != "-" {
				ssrfHits.Add(1)
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"status":"ok"}`+"\n")

		case r.Method == http.MethodGet && p == "/images/search":
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `[{"name":"alpine"}]`)

		case r.Method == http.MethodPost && strings.HasPrefix(p, "/images/") && strings.HasSuffix(p, "/push"):
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"status":"pushed"}`+"\n")

		case r.Method == http.MethodPost && p == "/containers/create":
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusCreated)
			_, _ = io.WriteString(w, `{"Id":"ctr-ok"}`)

		default:
			w.WriteHeader(http.StatusOK)
		}
	}))
	defer dockerSrv.Close()

	dataDir := newShortTempDataDir(t)
	mgr, err := ext.NewManager(ext.Config{
		DataDir:       dataDir,
		Keychain:      newFakeKeychain(t),
		DeveloperMode: func() bool { return true },
		HostInfo:      func() ext.HostInfo { return ext.HostInfo{OS: "darwin", Arch: hostArch, Locale: "en"} },
		DockerBackendDial: func(ctx context.Context) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "tcp", strings.TrimPrefix(dockerSrv.URL, "http://"))
		},
		UIHost: NewMockUIHost(),
	})
	if err != nil {
		t.Fatalf("ext.NewManager: %v", err)
	}
	defer func() { _ = mgr.Close(5 * time.Second) }()

	// Extension 1: PermDocker WITHOUT PermNetwork.
	noNetSrc := strings.ReplaceAll(`package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strings"

	"github.com/ironpark/gvx-desktop/pkg/extsdk"
)

func dReq(ctx context.Context, method, p, body string) (int, string, error) {
	ep := os.Getenv("GVX_DOCKER_HOST")
	tr := &http.Transport{DialContext: func(c context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(c, "unix", strings.TrimPrefix(ep, "unix://"))
	}}
	var r io.Reader
	if body != "" {
		r = bytes.NewBufferString(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, "http://docker"+p, r)
	if err != nil {
		return 0, "", err
	}
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := (&http.Client{Transport: tr}).Do(req)
	if err != nil {
		return 0, "", err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(raw), nil
}

func main() {
	app := extsdk.New()
	app.View("main", extsdk.View{
		Render: func(c *extsdk.ViewContext, ui *extsdk.UI) extsdk.Node {
			return ui.Column(ui.Button("Run", extsdk.Key("run"), extsdk.OnTap("run")))
		},
		On: extsdk.Handlers{
			"run": func(ctx *extsdk.EventContext) error {
				for _, tc := range []struct {
					m, p string
					want int
				}{
					{http.MethodPost, "/images/create?fromSrc=__SSRF_URL__&repo=atk&tag=1", 403},
					{http.MethodPost, "/images/create?fromImage=alpine&tag=3.20", 403},
					{http.MethodGet, "/images/search?term=alpine", 403},
					{http.MethodPost, "/images/alpine:3.20/push", 403},
					{http.MethodPost, "/images/atk-foreign:1/tag?repo=shared&tag=1", 403},
					{http.MethodPost, "/images/sha256:bbbb000000000000000000000000000000000000000000000000000000000001/tag?repo=shared&tag=1", 403},
					{http.MethodPost, "/commit?container=c-foreign-arch&repo=shared&tag=1", 403},
					{http.MethodPost, "/images/atk-foreign:1/tag?repo=gvx-ext/t5-victim&tag=9.9.9", 403},
					{http.MethodPost, "/images/create?fromSrc=-&repo=gvx-ext/t5-victim&tag=1.0.0", 403},
					{http.MethodPost, "/commit?container=c-foreign-arch&repo=gvx-ext/t5-victim&tag=1.0.0", 403},
					{http.MethodPost, "/images/atk-foreign:1/tag?repo=atk-foreign-own&tag=1", 201},
					{http.MethodPost, "/images/create?fromSrc=-&repo=atk-local&tag=1", 200},
				} {
					if c, b, e := dReq(ctx, tc.m, tc.p, ""); e != nil || c != tc.want {
						return fmt.Errorf("nonet %s %s expected %d, got %d (%s) err=%v", tc.m, tc.p, tc.want, c, b, e)
					}
				}
				return nil
			},
		},
	})
	_ = app.Run(context.Background())
}
`, "__SSRF_URL__", url.QueryEscape(ssrfCanary.URL+"/secret"))

	noNetDir := e2e4CreateBinaryExt(t, t.TempDir(), "t5-probe7-nonet", binaryManifestTOML("t5-probe7-nonet", "0.1.0", "bin/ext", "docker"), noNetSrc)
	if _, err := mgr.InstallLocal(noNetDir); err != nil {
		t.Fatalf("InstallLocal(nonet): %v", err)
	}
	if err := mgr.AllowAndBuild("t5-probe7-nonet"); err != nil {
		t.Fatalf("AllowAndBuild(nonet): %v", err)
	}
	e2e4WaitExtState(t, mgr, "t5-probe7-nonet", 10*time.Second, func(i ext.ExtensionInfo) bool { return i.State == ext.StateRunning })
	if err := mgr.SendViewEvent("t5-probe7-nonet", "main", uiblock.Event{Node: "/run", Kind: uiblock.EventTap}); err != nil {
		t.Fatalf("SendViewEvent(nonet): %v", err)
	}

	// Extension 2: PermDocker + PermNetwork + PermSandboxes.
	withNetSrc := strings.ReplaceAll(`package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strings"

	"github.com/ironpark/gvx-desktop/pkg/extsdk"
)

func dReq(ctx context.Context, method, p, body string) (int, string, error) {
	ep := os.Getenv("GVX_DOCKER_HOST")
	tr := &http.Transport{DialContext: func(c context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(c, "unix", strings.TrimPrefix(ep, "unix://"))
	}}
	var r io.Reader
	if body != "" {
		r = bytes.NewBufferString(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, "http://docker"+p, r)
	if err != nil {
		return 0, "", err
	}
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := (&http.Client{Transport: tr}).Do(req)
	if err != nil {
		return 0, "", err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(raw), nil
}

func main() {
	app := extsdk.New()
	app.View("main", extsdk.View{
		Render: func(c *extsdk.ViewContext, ui *extsdk.UI) extsdk.Node {
			return ui.Column(ui.Button("Run", extsdk.Key("run"), extsdk.OnTap("run")))
		},
		On: extsdk.Handlers{
			"run": func(ctx *extsdk.EventContext) error {
				for _, tc := range []struct {
					m, p, b string
					want    int
				}{
					{http.MethodPost, "/images/create?fromSrc=__SSRF_URL__&repo=atk&tag=1", "", 403},
					{http.MethodPost, "/images/create?fromImage=gvx-ext/t5-victim&tag=1.0.0", "", 403},
					{http.MethodPost, "/images/create?fromImage=docker.io/gvx-ext/t5-victim:1.0.0", "", 403},
					{http.MethodPost, "/images/create?fromImage=t5-victim:v1@sha256:2222222222222222222222222222222222222222222222222222222222222222", "", 403},
					{http.MethodPost, "/images/create?fromImage=t5-victim:v1@sha256:9999999999999999999999999999999999999999999999999999999999999999", "", 403},
					{http.MethodPost, "/images/create?fromImage=alpine:3.20@sha256:2222222222222222222222222222222222222222222222222222222222222222", "", 403},
					{http.MethodGet, "/images/t5-victim:v1@sha256:2222222222222222222222222222222222222222222222222222222222222222/json", "", 403},
					{http.MethodPost, "/containers/create", `+"`"+`{"Image":"gvx-ext/t5-victim:9.9.9"}`+"`"+`, 403},
					{http.MethodPost, "/containers/create", `+"`"+`{"Image":"t5-victim:v1@sha256:9999999999999999999999999999999999999999999999999999999999999999"}`+"`"+`, 403},
					{http.MethodPost, "/containers/create", `+"`"+`{"Image":"alpine:3.20@sha256:2222222222222222222222222222222222222222222222222222222222222222"}`+"`"+`, 403},
					// Positive registry ops succeed with PermNetwork.
					{http.MethodPost, "/images/create?fromImage=alpine&tag=3.20", "", 200},
					{http.MethodGet, "/images/search?term=alpine", "", 200},
					{http.MethodPost, "/images/t5-own:v1/push", "", 200},
				} {
					if c, b, e := dReq(ctx, tc.m, tc.p, tc.b); e != nil || c != tc.want {
						return fmt.Errorf("withnet %s %s expected %d, got %d (%s) err=%v", tc.m, tc.p, tc.want, c, b, e)
					}
				}
				startFalse := false
				for _, img := range []string{
					"t5-victim:v1@sha256:9999999999999999999999999999999999999999999999999999999999999999",
					"alpine:3.20@sha256:2222222222222222222222222222222222222222222222222222222222222222",
					"docker.io/gvx-ext/t5-victim:9.9.9",
				} {
					if _, err := ctx.Host.SandboxCreate(ctx, extsdk.CreateSandboxRequest{
						Name:  "sbx-deny",
						Image: img,
						Start: &startFalse,
					}); !extsdk.IsPermissionDenied(err) {
						return fmt.Errorf("SandboxCreate(%s) expected permission_denied, got %v", img, err)
					}
				}
				return nil
			},
		},
	})
	_ = app.Run(context.Background())
}
`, "__SSRF_URL__", url.QueryEscape(ssrfCanary.URL+"/secret"))

	withNetDir := e2e4CreateBinaryExt(t, t.TempDir(), "t5-probe7-net", binaryManifestTOML("t5-probe7-net", "0.1.0", "bin/ext", "docker", "network", "sandboxes"), withNetSrc)
	if _, err := mgr.InstallLocal(withNetDir); err != nil {
		t.Fatalf("InstallLocal(withnet): %v", err)
	}
	if err := mgr.AllowAndBuild("t5-probe7-net"); err != nil {
		t.Fatalf("AllowAndBuild(withnet): %v", err)
	}
	e2e4WaitExtState(t, mgr, "t5-probe7-net", 10*time.Second, func(i ext.ExtensionInfo) bool { return i.State == ext.StateRunning })
	if err := mgr.SendViewEvent("t5-probe7-net", "main", uiblock.Event{Node: "/run", Kind: uiblock.EventTap}); err != nil {
		t.Fatalf("SendViewEvent(withnet): %v", err)
	}

	if ssrfHits.Load() != 0 {
		t.Fatalf("SSRF canary hit %d times, want 0", ssrfHits.Load())
	}
	if mutatingCrossArch.Load() != 0 {
		t.Fatalf("cross-arch tag reached backend %d times, want 0", mutatingCrossArch.Load())
	}
}

func TestT5_08_FailedReinstallDoesNotLeaveStaleRunningState(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX directory chmod semantics required")
	}
	if os.Geteuid() == 0 {
		t.Skip("cannot enforce 0500 directory write failure when running as root")
	}

	dataDir := newShortTempDataDir(t)
	mgr, _ := e2e4NewManager(t, nil, WithDataDir(dataDir), WithDeveloperMode(true))
	if err := mgr.Start(context.Background()); err != nil {
		t.Fatalf("mgr.Start: %v", err)
	}

	extSrc := `package main

import (
	"context"

	"github.com/ironpark/gvx-desktop/pkg/extsdk"
)

func main() {
	app := extsdk.New()
	app.View("main", extsdk.View{
		Render: func(c *extsdk.ViewContext, ui *extsdk.UI) extsdk.Node {
			return ui.Column(ui.Button("Ping", extsdk.Key("ping"), extsdk.OnTap("ping")))
		},
		On: extsdk.Handlers{
			"ping": func(ctx *extsdk.EventContext) error {
				return nil
			},
		},
	})
	_ = app.Run(context.Background())
}
`
	v1Dir := e2e4CreateBinaryExt(t, t.TempDir(), "t5-reinstall-state", binaryManifestTOML("t5-reinstall-state", "0.1.0", "bin/ext"), extSrc)
	v2Dir := e2e4CreateBinaryExt(t, t.TempDir(), "t5-reinstall-state", binaryManifestTOML("t5-reinstall-state", "0.2.0", "bin/ext"), extSrc)

	if _, err := mgr.InstallLocal(v1Dir); err != nil {
		t.Fatalf("InstallLocal(v1): %v", err)
	}
	if err := mgr.AllowAndBuild("t5-reinstall-state"); err != nil {
		t.Fatalf("AllowAndBuild(v1): %v", err)
	}
	e2e4WaitExtState(t, mgr, "t5-reinstall-state", 10*time.Second, func(i ext.ExtensionInfo) bool {
		return i.State == ext.StateRunning
	})
	if err := mgr.SendViewEvent("t5-reinstall-state", "main", uiblock.Event{Node: "/ping", Kind: uiblock.EventTap}); err != nil {
		t.Fatalf("SendViewEvent(v1): %v", err)
	}

	instDir := ext.ExtensionDir(dataDir, "t5-reinstall-state")
	t.Cleanup(func() { _ = os.Chmod(instDir, 0o700) })
	if err := os.Chmod(instDir, 0o500); err != nil {
		t.Fatalf("chmod 0500: %v", err)
	}

	if _, err := mgr.InstallLocal(v2Dir); err == nil {
		t.Fatalf("expected InstallLocal(v2) to fail when ExtensionDir is read-only")
	}

	info, ok := e2e4FindExtInfo(mgr, "t5-reinstall-state")
	if !ok {
		t.Fatalf("t5-reinstall-state missing from mgr.List() after failed reinstall")
	}
	if info.State == ext.StateRunning || info.State.IsActive() || info.State == ext.StateBuilding {
		t.Fatalf("failed reinstall left stale active state=%s (err=%q)", info.State, info.Error)
	}
	if info.State != ext.StateFailed {
		t.Fatalf("expected StateFailed after failed reinstall, got %s (err=%q)", info.State, info.Error)
	}
	if info.Error == "" {
		t.Fatalf("expected non-empty ExtensionInfo.Error after failed reinstall")
	}
	if err := mgr.SendViewEvent("t5-reinstall-state", "main", uiblock.Event{Node: "/ping", Kind: uiblock.EventTap}); err == nil {
		t.Fatalf("expected SendViewEvent to fail after runtime teardown on failed reinstall")
	}

	// Restore permissions and confirm lifecycle recovery works cleanly without wedging.
	if err := os.Chmod(instDir, 0o700); err != nil {
		t.Fatalf("chmod 0700: %v", err)
	}
	if err := mgr.AllowAndBuild("t5-reinstall-state"); err != nil {
		t.Fatalf("AllowAndBuild recovery: %v", err)
	}
	e2e4WaitExtState(t, mgr, "t5-reinstall-state", 10*time.Second, func(i ext.ExtensionInfo) bool {
		return i.State == ext.StateRunning
	})
	if err := mgr.SendViewEvent("t5-reinstall-state", "main", uiblock.Event{Node: "/ping", Kind: uiblock.EventTap}); err != nil {
		t.Fatalf("SendViewEvent after recovery: %v", err)
	}
}

func TestT5_09_CommitContinuationOnbuildAndSubscribeViewTightIdleReaper(t *testing.T) {
	// Part A: POST /commit & POST /images/create reject backslash line continuations and ONBUILD label spoofing,
	// and valid commits always append LABEL io.gvx.extension=<extID>.
	var (
		mu             sync.Mutex
		commitHits     int
		lastChanges    []string
		lastBodyLabels map[string]any
	)
	dockerSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := r.URL.Path
		if len(p) > 3 && strings.HasPrefix(p, "/v") && p[2] >= '0' && p[2] <= '9' {
			if idx := strings.IndexByte(p[2:], '/'); idx > 0 {
				p = p[2+idx:]
			}
		}
		switch {
		case r.Method == http.MethodGet && p == "/containers/t5-p9-owned/json":
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"Id":     "t5-p9-owned",
				"Image":  "alpine:3.20",
				"Config": map[string]any{"Labels": map[string]string{"io.gvx.extension": "t5-probe9-docker"}},
			})
		case r.Method == http.MethodGet && p == "/images/alpine:3.20/json":
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"Id":     "sha256:alpine",
				"Config": map[string]any{"Labels": map[string]string{}},
			})
		case r.Method == http.MethodPost && p == "/commit":
			raw, _ := io.ReadAll(r.Body)
			var bodyObj map[string]any
			if len(bytes.TrimSpace(raw)) > 0 {
				_ = json.Unmarshal(raw, &bodyObj)
			}
			mu.Lock()
			commitHits++
			lastChanges = append([]string(nil), r.URL.Query()["changes"]...)
			if lbls, ok := bodyObj["Labels"].(map[string]any); ok {
				lastBodyLabels = lbls
			} else {
				lastBodyLabels = nil
			}
			mu.Unlock()
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusCreated)
			_, _ = io.WriteString(w, `{"Id":"sha256:t5p9commit"}`)
		case r.Method == http.MethodPost && p == "/images/create":
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(w, `{"status":"sha256:t5p9img"}`+"\n")
		default:
			w.WriteHeader(http.StatusOK)
		}
	}))
	defer dockerSrv.Close()

	dataDir := newShortTempDataDir(t)
	mgr, err := ext.NewManager(ext.Config{
		DataDir:       dataDir,
		Keychain:      newFakeKeychain(t),
		DeveloperMode: func() bool { return true },
		HostInfo:      func() ext.HostInfo { return ext.HostInfo{OS: "darwin", Arch: runtime.GOARCH, Locale: "en"} },
		DockerBackendDial: func(ctx context.Context) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "tcp", strings.TrimPrefix(dockerSrv.URL, "http://"))
		},
		UIHost: NewMockUIHost(),
	})
	if err != nil {
		t.Fatalf("ext.NewManager: %v", err)
	}
	if err := mgr.Start(context.Background()); err != nil {
		t.Fatalf("mgr.Start: %v", err)
	}
	defer func() { _ = mgr.Close(5 * time.Second) }()

	dockerExtSrc := `package main

import (
	"archive/tar"
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"

	"github.com/ironpark/gvx-desktop/pkg/extsdk"
)

func dReq(ctx context.Context, method, p string, body []byte) (int, string, error) {
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
	var r io.Reader
	if len(body) > 0 {
		r = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, base+p, r)
	if err != nil {
		return 0, "", err
	}
	if len(body) > 0 && body[0] == '{' {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := (&http.Client{Transport: tr}).Do(req)
	if err != nil {
		return 0, "", err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(raw), nil
}

func main() {
	var tarBuf bytes.Buffer
	tw := tar.NewWriter(&tarBuf)
	_ = tw.WriteHeader(&tar.Header{Name: "a.txt", Mode: 0o644, Size: 1})
	_, _ = tw.Write([]byte("a"))
	_ = tw.Close()
	validTar := tarBuf.Bytes()

	app := extsdk.New()
	app.View("main", extsdk.View{
		Render: func(c *extsdk.ViewContext, ui *extsdk.UI) extsdk.Node {
			return ui.Column(ui.Button("Probe", extsdk.Key("run"), extsdk.OnTap("run")))
		},
		On: extsdk.Handlers{
			"run": func(ctx *extsdk.EventContext) error {
				badChanges := [][]string{
					{"LABEL a=b \\\nio.gvx.extension=t5-victim"},
					{"LABEL a=b \\\r\nio.gvx.extension=t5-victim"},
					{"LABEL a=b \\\n# bypass\nio.gvx.extension=t5-victim"},
					{"LABEL a=b \\", "io.gvx.extension=t5-victim"},
					{"ONBUILD LABEL io.gvx.extension=t5-victim"},
					{"ONBUILD \\\nLABEL io.gvx.extension=t5-victim"},
					{"# escape=` + "`" + `\nLABEL a=b ` + "`" + `\nio.gvx.extension=t5-victim"},
				}
				for _, chList := range badChanges {
					cq := url.Values{}
					cq.Set("container", "t5-p9-owned")
					for _, ch := range chList {
						cq.Add("changes", ch)
					}
					if c, b, e := dReq(ctx, http.MethodPost, "/commit?"+cq.Encode(), nil); e != nil || c != 403 {
						return fmt.Errorf("commit %v expected 403, got %d (%s) err=%v", chList, c, b, e)
					}
					iq := url.Values{}
					iq.Set("fromSrc", "-")
					iq.Set("repo", "t5-probe9-own")
					iq.Set("tag", "1")
					for _, ch := range chList {
						iq.Add("changes", ch)
					}
					if c, b, e := dReq(ctx, http.MethodPost, "/images/create?"+iq.Encode(), validTar); e != nil || c != 403 {
						return fmt.Errorf("images/create %v expected 403, got %d (%s) err=%v", chList, c, b, e)
					}
				}
				if c, b, e := dReq(ctx, http.MethodPost, "/commit?container=t5-p9-owned&changes="+url.QueryEscape("ENV A=1"), nil); e != nil || c != 201 {
					return fmt.Errorf("valid commit (no body) expected 201, got %d (%s) err=%v", c, b, e)
				}
				if c, b, e := dReq(ctx, http.MethodPost, "/commit?container=t5-p9-owned&changes="+url.QueryEscape("LABEL app.v=1"), []byte("{\"Comment\":\"p9\"}")); e != nil || c != 201 {
					return fmt.Errorf("valid commit (with body) expected 201, got %d (%s) err=%v", c, b, e)
				}
				return nil
			},
		},
	})
	_ = app.Run(context.Background())
}
`
	dockerExtDir := e2e4CreateBinaryExt(t, t.TempDir(), "t5-probe9-docker", binaryManifestTOML("t5-probe9-docker", "0.1.0", "bin/ext", "docker"), dockerExtSrc)
	if _, err := mgr.InstallLocal(dockerExtDir); err != nil {
		t.Fatalf("InstallLocal(dockerExt): %v", err)
	}
	if err := mgr.AllowAndBuild("t5-probe9-docker"); err != nil {
		t.Fatalf("AllowAndBuild(dockerExt): %v", err)
	}
	e2e4WaitExtState(t, mgr, "t5-probe9-docker", 10*time.Second, func(i ext.ExtensionInfo) bool { return i.State == ext.StateRunning })
	if err := mgr.SendViewEvent("t5-probe9-docker", "main", uiblock.Event{Node: "/run", Kind: uiblock.EventTap}); err != nil {
		t.Fatalf("SendViewEvent(dockerExt): %v", err)
	}

	mu.Lock()
	gotHits := commitHits
	gotChanges := append([]string(nil), lastChanges...)
	gotBodyLabels := lastBodyLabels
	mu.Unlock()
	if gotHits != 2 {
		t.Fatalf("expected exactly 2 valid /commit backend hits, got %d", gotHits)
	}
	if len(gotChanges) == 0 || gotChanges[len(gotChanges)-1] != "LABEL io.gvx.extension=t5-probe9-docker" {
		t.Fatalf("expected appended LABEL io.gvx.extension=t5-probe9-docker in changes, got %#v", gotChanges)
	}
	if gotBodyLabels == nil || gotBodyLabels["io.gvx.extension"] != "t5-probe9-docker" {
		t.Fatalf("expected enforced Labels[io.gvx.extension]=t5-probe9-docker in commit body, got %#v", gotBodyLabels)
	}

	// Part B: SubscribeView vs tight idle_timeout_sec = 1 reaper does not wedge across repeated cycles.
	idleSrc := `package main

import (
	"context"

	"github.com/ironpark/gvx-desktop/pkg/extsdk"
)

func main() {
	app := extsdk.New()
	app.View("main", extsdk.View{
		Render: func(c *extsdk.ViewContext, ui *extsdk.UI) extsdk.Node {
			return ui.Column(ui.Text("t5-p9-idle-ok"))
		},
	})
	_ = app.Run(context.Background())
}
`
	idleTOML := `schema_version = 1
id = "t5-probe9-idle"
version = "0.1.0"
publisher = "acme"
name = "T5 Probe9 Idle"

[runtime]
type = "binary"
dev_entrypoint = "bin/ext"
idle_timeout_sec = 1

[[contributes.view]]
id = "main"
title = "Main"
`
	if runtime.GOOS == "windows" {
		idleTOML = strings.ReplaceAll(idleTOML, `"bin/ext"`, `"bin/ext.exe"`)
	}
	idleDir := e2e4CreateBinaryExt(t, t.TempDir(), "t5-probe9-idle", idleTOML, idleSrc)
	if _, err := mgr.InstallLocal(idleDir); err != nil {
		t.Fatalf("InstallLocal(idleExt): %v", err)
	}
	if err := mgr.AllowAndBuild("t5-probe9-idle"); err != nil {
		t.Fatalf("AllowAndBuild(idleExt): %v", err)
	}
	e2e4WaitExtState(t, mgr, "t5-probe9-idle", 10*time.Second, func(i ext.ExtensionInfo) bool { return i.State == ext.StateRunning })
	// Let the background idle reaper reap it to StateDiscovered first.
	e2e4WaitExtState(t, mgr, "t5-probe9-idle", 5*time.Second, func(i ext.ExtensionInfo) bool { return i.State == ext.StateDiscovered })

	// Repeatedly subscribe across tight idle-reaper intervals (with concurrent SubscribeView calls).
	for cycle := 0; cycle < 8; cycle++ {
		if cycle%2 == 0 {
			// Sleep right around the 1s idle timeout + 250ms reaper tick boundary.
			time.Sleep(1050 * time.Millisecond)
		}
		var subWG sync.WaitGroup
		errCh := make(chan error, 2)
		for g := 0; g < 2; g++ {
			subWG.Add(1)
			go func(idx int) {
				defer subWG.Done()
				subCtx, subCancel := context.WithTimeout(context.Background(), 4*time.Second)
				defer subCancel()
				ch, err := mgr.SubscribeView(subCtx, "t5-probe9-idle", "main", "en")
				if err != nil {
					errCh <- fmt.Errorf("cycle %d g=%d SubscribeView: %w", cycle, idx, err)
					return
				}
				gotTree := false
				for !gotTree {
					select {
					case upd, ok := <-ch:
						if !ok {
							errCh <- fmt.Errorf("cycle %d g=%d channel closed before tree", cycle, idx)
							return
						}
						if upd.Tree != nil {
							gotTree = true
						}
					case <-subCtx.Done():
						errCh <- fmt.Errorf("cycle %d g=%d SubscribeView wedged before tree", cycle, idx)
						return
					}
				}
				subCancel()
				for range ch {
				}
			}(g)
		}
		subWG.Wait()
		close(errCh)
		for e := range errCh {
			t.Fatal(e)
		}
	}
}

func TestT5_10_DockerScopeBOMOnBuildAndSandboxTokenRedaction(t *testing.T) {
	// Part A: BOM before # escape=` on POST /commit & POST /images/create?fromSrc=-,
	// and JSON OnBuild label spoofing / invalid types on POST /commit & POST /containers/create.
	var (
		mu          sync.Mutex
		backendHits int
	)
	dockerSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := r.URL.Path
		if len(p) > 3 && strings.HasPrefix(p, "/v") && p[2] >= '0' && p[2] <= '9' {
			if idx := strings.IndexByte(p[2:], '/'); idx > 0 {
				p = p[2+idx:]
			}
		}
		switch {
		case r.Method == http.MethodGet && p == "/containers/t5-p10-owned/json":
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"Id":     "t5-p10-owned",
				"Image":  "alpine:3.20",
				"Config": map[string]any{"Labels": map[string]string{"io.gvx.extension": "t5-p10-docker"}},
			})
		case r.Method == http.MethodGet && p == "/images/alpine:3.20/json":
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"Id":     "sha256:alpine",
				"Config": map[string]any{"Labels": map[string]string{}},
			})
		case r.Method == http.MethodPost && p == "/commit":
			mu.Lock()
			backendHits++
			mu.Unlock()
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusCreated)
			_, _ = io.WriteString(w, `{"Id":"sha256:t5p10commit"}`)
		case r.Method == http.MethodPost && p == "/containers/create":
			mu.Lock()
			backendHits++
			mu.Unlock()
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusCreated)
			_, _ = io.WriteString(w, `{"Id":"t5-p10-ctr"}`)
		case r.Method == http.MethodPost && p == "/images/create":
			mu.Lock()
			backendHits++
			mu.Unlock()
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(w, `{"status":"sha256:t5p10img"}`+"\n")
		default:
			w.WriteHeader(http.StatusOK)
		}
	}))
	defer dockerSrv.Close()

	dataDir := newShortTempDataDir(t)
	mgr, err := ext.NewManager(ext.Config{
		DataDir:       dataDir,
		Keychain:      newFakeKeychain(t),
		DeveloperMode: func() bool { return true },
		HostInfo:      func() ext.HostInfo { return ext.HostInfo{OS: "darwin", Arch: runtime.GOARCH, Locale: "en"} },
		DockerBackendDial: func(ctx context.Context) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "tcp", strings.TrimPrefix(dockerSrv.URL, "http://"))
		},
		UIHost: NewMockUIHost(),
	})
	if err != nil {
		t.Fatalf("ext.NewManager: %v", err)
	}
	if err := mgr.Start(context.Background()); err != nil {
		t.Fatalf("mgr.Start: %v", err)
	}
	defer func() { _ = mgr.Close(5 * time.Second) }()

	dockerExtSrc := `package main

import (
	"archive/tar"
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"

	"github.com/ironpark/gvx-desktop/pkg/extsdk"
)

func dReq(ctx context.Context, method, p string, body []byte) (int, string, error) {
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
	var r io.Reader
	if len(body) > 0 {
		r = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, base+p, r)
	if err != nil {
		return 0, "", err
	}
	if len(body) > 0 && body[0] == '{' {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := (&http.Client{Transport: tr}).Do(req)
	if err != nil {
		return 0, "", err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(raw), nil
}

func main() {
	var tarBuf bytes.Buffer
	tw := tar.NewWriter(&tarBuf)
	_ = tw.WriteHeader(&tar.Header{Name: "a.txt", Mode: 0o644, Size: 1})
	_, _ = tw.Write([]byte("a"))
	_ = tw.Close()
	validTar := tarBuf.Bytes()

	app := extsdk.New()
	app.View("main", extsdk.View{
		Render: func(c *extsdk.ViewContext, ui *extsdk.UI) extsdk.Node {
			return ui.Column(ui.Button("Probe10", extsdk.Key("run"), extsdk.OnTap("run")))
		},
		On: extsdk.Handlers{
			"run": func(ctx *extsdk.EventContext) error {
				bomChange := "\ufeff# escape=` + "`" + `\nONBUILD LABEL a=1 ` + "`" + `\nio.gvx.extension=t5-victim"
				cq := url.Values{}
				cq.Set("container", "t5-p10-owned")
				cq.Add("changes", bomChange)
				if c, b, e := dReq(ctx, http.MethodPost, "/commit?"+cq.Encode(), nil); e != nil || c != 403 {
					return fmt.Errorf("commit BOM escape expected 403, got %d (%s) err=%v", c, b, e)
				}
				iq := url.Values{}
				iq.Set("fromSrc", "-")
				iq.Set("repo", "t5-p10-own")
				iq.Set("tag", "1")
				iq.Add("changes", bomChange)
				if c, b, e := dReq(ctx, http.MethodPost, "/images/create?"+iq.Encode(), validTar); e != nil || c != 403 {
					return fmt.Errorf("images/create BOM escape expected 403, got %d (%s) err=%v", c, b, e)
				}

				for _, tc := range []struct {
					name string
					body string
					want int
				}{
					{"onbuild_upper", ` + "`" + `{"Image":"alpine:3.20","OnBuild":["LABEL io.gvx.extension=t5-victim"]}` + "`" + `, 403},
					{"onbuild_lower", ` + "`" + `{"Image":"alpine:3.20","onbuild":["LABEL io.gvx.extension=t5-victim"]}` + "`" + `, 403},
					{"onbuild_lower_label", ` + "`" + `{"Image":"alpine:3.20","OnBuild":["label io.gvx.extension=t5-victim"]}` + "`" + `, 403},
					{"onbuild_nested", ` + "`" + `{"Image":"alpine:3.20","OnBuild":["ONBUILD LABEL io.gvx.extension=t5-victim"]}` + "`" + `, 403},
					{"onbuild_bad_type", ` + "`" + `{"Image":"alpine:3.20","OnBuild":"LABEL io.gvx.extension=t5-victim"}` + "`" + `, 400},
					{"onbuild_bad_elem", ` + "`" + `{"Image":"alpine:3.20","OnBuild":[123]}` + "`" + `, 400},
				} {
					if c, b, e := dReq(ctx, http.MethodPost, "/commit?container=t5-p10-owned", []byte(tc.body)); e != nil || c != tc.want {
						return fmt.Errorf("commit %s expected %d, got %d (%s) err=%v", tc.name, tc.want, c, b, e)
					}
					if c, b, e := dReq(ctx, http.MethodPost, "/containers/create", []byte(tc.body)); e != nil || c != tc.want {
						return fmt.Errorf("create %s expected %d, got %d (%s) err=%v", tc.name, tc.want, c, b, e)
					}
				}
				return nil
			},
		},
	})
	_ = app.Run(context.Background())
}
`
	dockerExtDir := e2e4CreateBinaryExt(t, t.TempDir(), "t5-p10-docker", binaryManifestTOML("t5-p10-docker", "0.1.0", "bin/ext", "docker"), dockerExtSrc)
	if _, err := mgr.InstallLocal(dockerExtDir); err != nil {
		t.Fatalf("InstallLocal(dockerExt): %v", err)
	}
	if err := mgr.AllowAndBuild("t5-p10-docker"); err != nil {
		t.Fatalf("AllowAndBuild(dockerExt): %v", err)
	}
	e2e4WaitExtState(t, mgr, "t5-p10-docker", 10*time.Second, func(i ext.ExtensionInfo) bool { return i.State == ext.StateRunning })
	if err := mgr.SendViewEvent("t5-p10-docker", "main", uiblock.Event{Node: "/run", Kind: uiblock.EventTap}); err != nil {
		t.Fatalf("SendViewEvent(t5-p10-docker): %v", err)
	}
	mu.Lock()
	hits := backendHits
	mu.Unlock()
	if hits != 0 {
		t.Fatalf("expected 0 backend hits for rejected BOM/OnBuild payloads, got %d", hits)
	}

	// Part B: Sandbox extension via Manager + fake rungv binary must redact GVX_HOST_TOKEN and GVX_EXT_TOKEN
	// from Supervisor.Logs, mgr.Logs(id), and extension-data/<id>/logs/extension.log.
	sbxDataDir := newShortTempDataDir(t)
	fakeRungvSrc := filepath.Join(t.TempDir(), "fake_rungv.go")
	const fakeRungvCode = `package main

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
	args := os.Args[1:]
	for _, a := range args {
		if a == "-help" || a == "--help" {
			fmt.Println("  -unix-forward")
			fmt.Println("  -nat-policy-file")
			return
		}
	}
	if len(args) == 0 || args[0] != "run" {
		return
	}
	var hostTok, extTok, listenSpec string
	for i := 0; i < len(args); i++ {
		if args[i] == "-e" && i+1 < len(args) {
			kv := args[i+1]
			if v, ok := strings.CutPrefix(kv, "GVX_HOST_TOKEN="); ok {
				hostTok = v
			} else if v, ok := strings.CutPrefix(kv, "GVX_EXT_TOKEN="); ok {
				extTok = v
			}
			i++
		} else if args[i] == "-p" && i+1 < len(args) {
			listenSpec = args[i+1]
			i++
		}
	}
	fmt.Fprintf(os.Stderr, "t5-p10-guest-line GVX_HOST_TOKEN=%s GVX_EXT_TOKEN=%s\n", hostTok, extTok)

	var ln net.Listener
	var err error
	if strings.HasPrefix(listenSpec, "unix:") {
		rest := strings.TrimPrefix(listenSpec, "unix:")
		if idx := strings.LastIndex(rest, ":"); idx > 0 {
			sock := rest[:idx]
			_ = os.Remove(sock)
			ln, err = net.Listen("unix", sock)
		}
	} else {
		parts := strings.Split(listenSpec, ":")
		if len(parts) >= 2 {
			ln, err = net.Listen("tcp", parts[0]+":"+parts[1])
		}
	}
	if err != nil {
		os.Exit(2)
	}
	done := make(chan struct{})
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/health", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(` + "`" + `{"status":"ok","version":"0.1.0"}` + "`" + `))
	})
	mux.HandleFunc("/v1/shutdown", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		select {
		case <-done:
		default:
			close(done)
		}
	})
	srv := &http.Server{Handler: mux}
	go func() { _ = srv.Serve(ln) }()
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)
	select {
	case <-done:
	case <-sigCh:
	}
	_ = srv.Close()
}
`
	if err := os.WriteFile(fakeRungvSrc, []byte(fakeRungvCode), 0o600); err != nil {
		t.Fatal(err)
	}
	fakeRungvBin := filepath.Join(t.TempDir(), "rungv")
	if runtime.GOOS == "windows" {
		fakeRungvBin += ".exe"
	}
	if out, err := exec.Command("go", "build", "-o", fakeRungvBin, fakeRungvSrc).CombinedOutput(); err != nil {
		t.Fatalf("build fake rungv: %v\n%s", err, out)
	}
	t.Cleanup(func() { rungv.ForgetFeatures(fakeRungvBin) })

	sbxSup := &sandbox.Supervisor{
		Client:  func() *rungv.Client { return &rungv.Client{Path: fakeRungvBin} },
		DataDir: filepath.Join(sbxDataDir, "sup"),
	}
	t.Cleanup(func() { sbxSup.StopAll(5 * time.Second) })
	sbxMgr, _ := newTestManager(t, WithDataDir(sbxDataDir), WithSupervisor(sbxSup))

	sbxDir := t.TempDir()
	writeExtFixture(t, sbxDir, `schema_version = 1
id = "t5-p10-sbx"
version = "0.1.0"
publisher = "acme"
name = "T5 P10 Sbx"

[runtime]
type = "sandbox"
image = "alpine:3.20"
`, nil)
	if _, err := sbxMgr.InstallLocal(sbxDir); err != nil {
		t.Fatalf("InstallLocal(sbx): %v", err)
	}
	if err := sbxMgr.AllowAndBuild("t5-p10-sbx"); err != nil {
		t.Fatalf("AllowAndBuild(sbx): %v", err)
	}
	e2e4WaitExtState(t, sbxMgr, "t5-p10-sbx", 10*time.Second, func(i ext.ExtensionInfo) bool { return i.State == ext.StateRunning })

	// Wait for watchSandbox to flush logs.
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if strings.Contains(strings.Join(sbxMgr.Logs("t5-p10-sbx"), "\n"), "t5-p10-guest-line") {
			break
		}
		time.Sleep(25 * time.Millisecond)
	}

	var supTexts []string
	for _, ln := range sbxSup.Logs("ext-t5-p10-sbx").Snapshot() {
		if ln.Stream == sandbox.System {
			supTexts = append(supTexts, ln.Text)
		}
	}
	supJoined := strings.Join(supTexts, "\n")
	mgrJoined := strings.Join(sbxMgr.Logs("t5-p10-sbx"), "\n")
	diskBytes, err := os.ReadFile(filepath.Join(ext.LogsDir(sbxDataDir, "t5-p10-sbx"), "extension.log"))
	if err != nil {
		t.Fatalf("read extension.log: %v", err)
	}
	diskJoined := string(diskBytes)

	for label, text := range map[string]string{
		"Supervisor.Logs": supJoined,
		"Manager.Logs":    mgrJoined,
		"extension.log":   diskJoined,
	} {
		if !strings.Contains(text, "GVX_HOST_TOKEN=<redacted>") || !strings.Contains(text, "GVX_EXT_TOKEN=<redacted>") {
			t.Fatalf("%s missing redacted token placeholders:\n%s", label, text)
		}
	}
	// Ensure no 64-hex-char bearer token value appears after GVX_HOST_TOKEN= or GVX_EXT_TOKEN=.
	for _, key := range []string{"GVX_HOST_TOKEN=", "GVX_EXT_TOKEN="} {
		for label, text := range map[string]string{
			"Supervisor.Logs": supJoined,
			"Manager.Logs":    mgrJoined,
			"extension.log":   diskJoined,
		} {
			idx := 0
			for {
				pos := strings.Index(text[idx:], key)
				if pos < 0 {
					break
				}
				valStart := idx + pos + len(key)
				if !strings.HasPrefix(text[valStart:], "<redacted>") {
					t.Fatalf("%s leaked unredacted %s value in:\n%s", label, key, text)
				}
				idx = valStart
			}
		}
	}
}

func TestT5_11_RevokedAllowedFoldersAndRestorePermSandboxes(t *testing.T) {
	// Part A: Revoking allowed_folders via Manager.RemoveAllowedFolder stops running
	// extension-owned child sandboxes & Docker containers binding the revoked folder
	// and rejects subsequent SandboxStart, Restore, and Docker start/restart/unpause (403).
	dataDir := newShortTempDataDir(t)
	allowedDir := t.TempDir()
	if resolved, err := filepath.EvalSymlinks(allowedDir); err == nil {
		allowedDir = resolved
	}

	fakeRungvSrc := filepath.Join(t.TempDir(), "fake_rungv_t5_11.go")
	const fakeRungvCode = `package main

import (
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"
)

func main() {
	if len(os.Args) < 2 {
		os.Exit(2)
	}
	switch os.Args[1] {
	case "run":
		for _, a := range os.Args[2:] {
			if strings.Contains(a, "help") {
				fmt.Println("  -control-token-file")
				fmt.Println("  -control-socket")
				return
			}
		}
		waitStop()
	case "restore":
		for _, a := range os.Args[2:] {
			if strings.Contains(a, "help") {
				return
			}
		}
		waitStop()
	default:
		return
	}
}

func waitStop() {
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
	done := make(chan struct{})
	go func() {
		_, _ = io.Copy(io.Discard, os.Stdin)
		close(done)
	}()
	select {
	case <-sig:
	case <-done:
	case <-time.After(30 * time.Second):
	}
}
`
	if err := os.WriteFile(fakeRungvSrc, []byte(fakeRungvCode), 0o600); err != nil {
		t.Fatal(err)
	}
	exeName := "rungv"
	if runtime.GOOS == "windows" {
		exeName = "rungv.exe"
	}
	fakeRungvBin := filepath.Join(t.TempDir(), exeName)
	if out, err := exec.Command("go", "build", "-o", fakeRungvBin, fakeRungvSrc).CombinedOutput(); err != nil {
		t.Fatalf("build fake rungv: %v\n%s", err, out)
	}
	t.Cleanup(func() { rungv.ForgetFeatures(fakeRungvBin) })

	sup := &sandbox.Supervisor{
		DataDir: filepath.Join(dataDir, "sup"),
		Client:  func() *rungv.Client { return &rungv.Client{Path: fakeRungvBin} },
	}
	t.Cleanup(func() { sup.StopAll(3 * time.Second) })

	type mockCtr struct {
		labels  map[string]string
		binds   []string
		running bool
		stopped bool
	}
	var (
		ctrMu      sync.Mutex
		nextCtrID  int
		containers = map[string]*mockCtr{}
	)
	dockerSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := r.URL.Path
		if len(p) > 3 && strings.HasPrefix(p, "/v") && p[2] >= '0' && p[2] <= '9' {
			if idx := strings.IndexByte(p[2:], '/'); idx > 0 {
				p = p[2+idx:]
			}
		}
		switch {
		case r.Method == http.MethodGet && strings.HasPrefix(p, "/images/") && strings.HasSuffix(p, "/json"):
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"Id":"sha256:alpine","Config":{"Labels":{}}}`))
		case r.Method == http.MethodPost && p == "/containers/create":
			var req struct {
				Labels     map[string]string `json:"Labels"`
				HostConfig struct {
					Binds []string `json:"Binds"`
				} `json:"HostConfig"`
			}
			_ = json.NewDecoder(r.Body).Decode(&req)
			ctrMu.Lock()
			nextCtrID++
			cid := fmt.Sprintf("t5-11-ctr-%d", nextCtrID)
			containers[cid] = &mockCtr{
				labels: req.Labels,
				binds:  req.HostConfig.Binds,
			}
			ctrMu.Unlock()
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(map[string]string{"Id": cid})
		case r.Method == http.MethodGet && p == "/containers/json":
			ctrMu.Lock()
			var list []map[string]any
			for cid, c := range containers {
				if c.running || r.URL.Query().Get("all") == "1" {
					list = append(list, map[string]any{"Id": cid, "Labels": c.labels})
				}
			}
			ctrMu.Unlock()
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(list)
		case r.Method == http.MethodGet && strings.HasPrefix(p, "/containers/") && strings.HasSuffix(p, "/json"):
			cid := strings.TrimSuffix(strings.TrimPrefix(p, "/containers/"), "/json")
			ctrMu.Lock()
			c, ok := containers[cid]
			ctrMu.Unlock()
			if !ok {
				http.Error(w, `{"message":"no such container"}`, http.StatusNotFound)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"Id":         cid,
				"Image":      "alpine:3.20",
				"Config":     map[string]any{"Labels": c.labels},
				"HostConfig": map[string]any{"Binds": c.binds},
			})
		case r.Method == http.MethodPost && strings.HasPrefix(p, "/containers/") && strings.HasSuffix(p, "/start"):
			cid := strings.TrimSuffix(strings.TrimPrefix(p, "/containers/"), "/start")
			ctrMu.Lock()
			if c, ok := containers[cid]; ok {
				c.running = true
				c.stopped = false
			}
			ctrMu.Unlock()
			w.WriteHeader(http.StatusNoContent)
		case r.Method == http.MethodPost && strings.HasPrefix(p, "/containers/") && strings.HasSuffix(p, "/stop"):
			cid := strings.TrimSuffix(strings.TrimPrefix(p, "/containers/"), "/stop")
			ctrMu.Lock()
			if c, ok := containers[cid]; ok {
				c.running = false
				c.stopped = true
			}
			ctrMu.Unlock()
			w.WriteHeader(http.StatusNoContent)
		case r.Method == http.MethodPost && strings.HasPrefix(p, "/containers/"):
			w.WriteHeader(http.StatusNoContent)
		default:
			w.WriteHeader(http.StatusOK)
		}
	}))
	defer dockerSrv.Close()

	uiHost := NewMockUIHost()
	uiHost.PickFolderResult = allowedDir
	mgr, err := ext.NewManager(ext.Config{
		DataDir:       dataDir,
		Keychain:      newFakeKeychain(t),
		DeveloperMode: func() bool { return true },
		Supervisor:    sup,
		HostInfo:      func() ext.HostInfo { return ext.HostInfo{OS: runtime.GOOS, Arch: runtime.GOARCH, Locale: "en"} },
		DockerBackendDial: func(ctx context.Context) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "tcp", strings.TrimPrefix(dockerSrv.URL, "http://"))
		},
		UIHost: uiHost,
	})
	if err != nil {
		t.Fatalf("ext.NewManager: %v", err)
	}
	if err := mgr.Start(context.Background()); err != nil {
		t.Fatalf("mgr.Start: %v", err)
	}
	defer func() { _ = mgr.Close(5 * time.Second) }()

	extSrc := `package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/ironpark/gvx-desktop/pkg/extsdk"
)

func dockerDo(ctx context.Context, method, p, body string) (int, string, error) {
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
	var r io.Reader
	if body != "" {
		r = bytes.NewBufferString(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, base+p, r)
	if err != nil {
		return 0, "", err
	}
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := (&http.Client{Transport: tr}).Do(req)
	if err != nil {
		return 0, "", err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(raw), nil
}

func main() {
	app := extsdk.New()
	app.View("main", extsdk.View{
		Render: func(c *extsdk.ViewContext, ui *extsdk.UI) extsdk.Node {
			return ui.Column(ui.Button("Setup", extsdk.Key("setup"), extsdk.OnTap("setup")))
		},
		On: extsdk.Handlers{
			"setup": func(ctx *extsdk.EventContext) error {
				picked, ok, err := ctx.Host.PickFolder(ctx)
				if err != nil || !ok {
					return fmt.Errorf("PickFolder failed: ok=%v err=%v", ok, err)
				}
				startTrue := true
				ckptTrue := true
				sbx, err := ctx.Host.SandboxCreate(ctx, extsdk.CreateSandboxRequest{
					Name:       "t5-11-child",
					Image:      "alpine:3.20",
					Mounts:     []extsdk.SandboxMount{{Type: "bind", Source: picked, Target: "/work"}},
					Checkpoint: &ckptTrue,
					Start:      &startTrue,
				})
				if err != nil {
					return err
				}
				c, b, e := dockerDo(ctx, http.MethodPost, "/containers/create", fmt.Sprintf(` + "`" + `{"Image":"alpine:3.20","HostConfig":{"Binds":[%q]}}` + "`" + `, picked+":/work:rw"))
				if e != nil || c != 201 {
					return fmt.Errorf("create container: %d %s %v", c, b, e)
				}
				var resp struct{ Id string }
				_ = json.Unmarshal([]byte(b), &resp)
				if sc, sb, se := dockerDo(ctx, http.MethodPost, "/containers/"+resp.Id+"/start", ""); se != nil || sc != 204 {
					return fmt.Errorf("start container: %d %s %v", sc, sb, se)
				}
				raw, _ := json.Marshal(map[string]string{"sbx": sbx.ID, "ctr": resp.Id})
				return os.WriteFile(filepath.Join(ctx.DataDir, "ids.json"), raw, 0o600)
			},
		},
	})
	app.Tool("probe_revoked", func(ctx *extsdk.ToolContext, args map[string]any) (any, error) {
		sbxID, _ := args["sbx"].(string)
		ctrID, _ := args["ctr"].(string)
		_, startErr := ctx.Host.SandboxStart(ctx, sbxID)
		_, restoreErr := ctx.Host.Restore(ctx, sbxID, extsdk.RestoreRequest{Snapshot: "warm"})
		cStart, _, _ := dockerDo(ctx, http.MethodPost, "/containers/"+ctrID+"/start", "")
		cRestart, _, _ := dockerDo(ctx, http.MethodPost, "/containers/"+ctrID+"/restart", "")
		cUnpause, _, _ := dockerDo(ctx, http.MethodPost, "/containers/"+ctrID+"/unpause", "")
		return map[string]any{
			"sbx_start_denied":   extsdk.IsPermissionDenied(startErr),
			"sbx_restore_denied": extsdk.IsPermissionDenied(restoreErr),
			"ctr_start_code":     cStart,
			"ctr_restart_code":   cRestart,
			"ctr_unpause_code":   cUnpause,
		}, nil
	})
	_ = app.Run(context.Background())
}
`
	manifest := `schema_version = 1
id = "t5-11-revoke"
version = "0.1.0"
publisher = "acme"
name = "T5 11 Revoke"
permissions = ["sandboxes", "snapshots", "docker", "ui:pick-folder"]

[runtime]
type = "binary"
dev_entrypoint = "bin/ext"

[[contributes.view]]
id = "main"
title = "Main"

[[contributes.tool]]
id = "probe_revoked"
description = "Probe revoked mounts"
`
	if runtime.GOOS == "windows" {
		manifest = strings.ReplaceAll(manifest, `"bin/ext"`, `"bin/ext.exe"`)
	}
	extDir := e2e4CreateBinaryExt(t, t.TempDir(), "t5-11-revoke", manifest, extSrc)
	if _, err := mgr.InstallLocal(extDir); err != nil {
		t.Fatalf("InstallLocal: %v", err)
	}
	if err := mgr.AllowAndBuild("t5-11-revoke"); err != nil {
		t.Fatalf("AllowAndBuild: %v", err)
	}
	e2e4WaitExtState(t, mgr, "t5-11-revoke", 10*time.Second, func(i ext.ExtensionInfo) bool { return i.State == ext.StateRunning })

	if err := mgr.SendViewEvent("t5-11-revoke", "main", uiblock.Event{Node: "/setup", Kind: uiblock.EventTap}); err != nil {
		t.Fatalf("SendViewEvent(setup): %v", err)
	}
	rawIDs, err := os.ReadFile(filepath.Join(ext.FilesDir(dataDir, "t5-11-revoke"), "ids.json"))
	if err != nil {
		t.Fatalf("read ids.json: %v", err)
	}
	var ids struct {
		Sbx string `json:"sbx"`
		Ctr string `json:"ctr"`
	}
	if err := json.Unmarshal(rawIDs, &ids); err != nil {
		t.Fatal(err)
	}

	// Seed snapshot bundle + index for the child sandbox so Restore reaches mount validation.
	snapDir := filepath.Join(sup.SnapshotRoot(ids.Sbx), "snap-warm")
	if err := os.MkdirAll(snapDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(snapDir, "manifest.json"), []byte(`{"version":1}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(ext.ExtensionDataDir(dataDir, "t5-11-revoke"), "snapshots"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ext.ExtensionDataDir(dataDir, "t5-11-revoke"), "snapshots", ids.Sbx+".json"), []byte(`{"warm":"snap-warm"}`), 0o600); err != nil {
		t.Fatal(err)
	}

	if !sup.State(ids.Sbx).Status.Active() {
		t.Fatalf("expected child sandbox active before RemoveAllowedFolder")
	}

	if err := mgr.RemoveAllowedFolder("t5-11-revoke", allowedDir); err != nil {
		t.Fatalf("RemoveAllowedFolder: %v", err)
	}
	if st := sup.State(ids.Sbx).Status; st.Active() {
		t.Fatalf("expected child sandbox stopped after RemoveAllowedFolder, got %v", st)
	}
	ctrMu.Lock()
	ctrStopped := containers[ids.Ctr].stopped && !containers[ids.Ctr].running
	ctrMu.Unlock()
	if !ctrStopped {
		t.Fatalf("expected Docker container stopped after RemoveAllowedFolder")
	}

	args, _ := json.Marshal(ids)
	probeOut, err := mgr.InvokeTool(context.Background(), "t5-11-revoke", "probe_revoked", args)
	if err != nil {
		t.Fatalf("InvokeTool(probe_revoked): %v", err)
	}
	var probe struct {
		SbxStartDenied   bool `json:"sbx_start_denied"`
		SbxRestoreDenied bool `json:"sbx_restore_denied"`
		CtrStartCode     int  `json:"ctr_start_code"`
		CtrRestartCode   int  `json:"ctr_restart_code"`
		CtrUnpauseCode   int  `json:"ctr_unpause_code"`
	}
	if err := json.Unmarshal(probeOut, &probe); err != nil {
		t.Fatal(err)
	}
	if !probe.SbxStartDenied || !probe.SbxRestoreDenied || probe.CtrStartCode != 403 || probe.CtrRestartCode != 403 || probe.CtrUnpauseCode != 403 {
		t.Fatalf("unexpected post-revoke probe results: %+v", probe)
	}

	// Part B: Restore requires PermSandboxes in addition to PermSnapshots.
	dataDir2 := newShortTempDataDir(t)
	sup2 := &sandbox.Supervisor{
		DataDir: filepath.Join(dataDir2, "sup"),
		Client:  func() *rungv.Client { return &rungv.Client{Path: fakeRungvBin} },
	}
	t.Cleanup(func() { sup2.StopAll(3 * time.Second) })
	if err := newTestStore(t, dataDir2).SaveSandboxes([]sandbox.Spec{
		{
			ID:         "t5-11-snap-only-sbx",
			Name:       "t5-11-snap-only-sbx",
			Image:      "alpine:3.20",
			Checkpoint: true,
			Labels:     map[string]string{"io.gvx.extension": "t5-11-snap-only"},
		},
	}); err != nil {
		t.Fatalf("SaveSandboxes: %v", err)
	}
	snapDir2 := filepath.Join(sup2.SnapshotRoot("t5-11-snap-only-sbx"), "snap-only")
	if err := os.MkdirAll(snapDir2, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(snapDir2, "manifest.json"), []byte(`{"version":1}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(ext.ExtensionDataDir(dataDir2, "t5-11-snap-only"), "snapshots"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ext.ExtensionDataDir(dataDir2, "t5-11-snap-only"), "snapshots", "t5-11-snap-only-sbx.json"), []byte(`{"only":"snap-only"}`), 0o600); err != nil {
		t.Fatal(err)
	}

	mgr2, err := ext.NewManager(ext.Config{
		DataDir:       dataDir2,
		Keychain:      newFakeKeychain(t),
		DeveloperMode: func() bool { return true },
		Supervisor:    sup2,
		UIHost:        NewMockUIHost(),
	})
	if err != nil {
		t.Fatalf("NewManager 2: %v", err)
	}
	if err := mgr2.Start(context.Background()); err != nil {
		t.Fatalf("mgr2.Start: %v", err)
	}
	defer func() { _ = mgr2.Close(5 * time.Second) }()

	snapOnlySrc := `package main

import (
	"context"
	"github.com/ironpark/gvx-desktop/pkg/extsdk"
)

func main() {
	app := extsdk.New()
	app.Tool("try_restore", func(ctx *extsdk.ToolContext, args map[string]any) (any, error) {
		_, err := ctx.Host.Restore(ctx, "t5-11-snap-only-sbx", extsdk.RestoreRequest{
			Snapshot: "only",
			Exec:     []string{"echo", "pwn"},
		})
		return map[string]any{
			"denied": extsdk.IsPermissionDenied(err),
			"err":    fmtErr(err),
		}, nil
	})
	_ = app.Run(context.Background())
}

func fmtErr(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}
`
	snapOnlyManifest := `schema_version = 1
id = "t5-11-snap-only"
version = "0.1.0"
publisher = "acme"
name = "T5 11 Snap Only"
permissions = ["snapshots"]

[runtime]
type = "binary"
dev_entrypoint = "bin/ext"

[[contributes.tool]]
id = "try_restore"
description = "Attempt restore without sandboxes perm"
`
	if runtime.GOOS == "windows" {
		snapOnlyManifest = strings.ReplaceAll(snapOnlyManifest, `"bin/ext"`, `"bin/ext.exe"`)
	}
	snapOnlyDir := e2e4CreateBinaryExt(t, t.TempDir(), "t5-11-snap-only", snapOnlyManifest, snapOnlySrc)
	if _, err := mgr2.InstallLocal(snapOnlyDir); err != nil {
		t.Fatalf("InstallLocal(snap-only): %v", err)
	}
	if err := mgr2.AllowAndBuild("t5-11-snap-only"); err != nil {
		t.Fatalf("AllowAndBuild(snap-only): %v", err)
	}
	e2e4WaitExtState(t, mgr2, "t5-11-snap-only", 10*time.Second, func(i ext.ExtensionInfo) bool { return i.State == ext.StateRunning })

	out2, err := mgr2.InvokeTool(context.Background(), "t5-11-snap-only", "try_restore", json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("InvokeTool(try_restore): %v", err)
	}
	var res2 struct {
		Denied bool   `json:"denied"`
		Err    string `json:"err"`
	}
	if err := json.Unmarshal(out2, &res2); err != nil {
		t.Fatal(err)
	}
	if !res2.Denied {
		t.Fatalf("expected Restore without PermSandboxes to return 403 permission_denied, got %+v", res2)
	}
	if st := sup2.State("t5-11-snap-only-sbx").Status; st.Active() {
		t.Fatalf("sandbox must not have started without PermSandboxes, got %v", st)
	}
}

func TestT5_12_UninstallDockerCleanupAndRevocationTOCTOU(t *testing.T) {
	dataDir := newShortTempDataDir(t)
	allowedDir := t.TempDir()
	if resolved, err := filepath.EvalSymlinks(allowedDir); err == nil {
		allowedDir = resolved
	}

	fakeRungvSrc := filepath.Join(t.TempDir(), "fake_rungv_t5_12.go")
	const fakeRungvCode = `package main

import (
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"
)

func main() {
	if len(os.Args) < 2 {
		os.Exit(2)
	}
	switch os.Args[1] {
	case "run":
		for _, a := range os.Args[2:] {
			if strings.Contains(a, "help") {
				fmt.Println("  -control-token-file")
				fmt.Println("  -control-socket")
				return
			}
		}
		waitStop()
	case "restore":
		for _, a := range os.Args[2:] {
			if strings.Contains(a, "help") {
				return
			}
		}
		waitStop()
	default:
		return
	}
}

func waitStop() {
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
	done := make(chan struct{})
	go func() {
		_, _ = io.Copy(io.Discard, os.Stdin)
		close(done)
	}()
	select {
	case <-sig:
	case <-done:
	case <-time.After(30 * time.Second):
	}
}
`
	if err := os.WriteFile(fakeRungvSrc, []byte(fakeRungvCode), 0o600); err != nil {
		t.Fatal(err)
	}
	exeName := "rungv"
	if runtime.GOOS == "windows" {
		exeName = "rungv.exe"
	}
	fakeRungvBin := filepath.Join(t.TempDir(), exeName)
	if out, err := exec.Command("go", "build", "-o", fakeRungvBin, fakeRungvSrc).CombinedOutput(); err != nil {
		t.Fatalf("build fake rungv: %v\n%s", err, out)
	}
	t.Cleanup(func() { rungv.ForgetFeatures(fakeRungvBin) })

	var (
		blockLaunchOnce atomic.Bool
		launchMu        sync.Mutex
		launchEntered   chan struct{}
		launchRelease   chan struct{}
	)
	armLaunchBarrier := func() (<-chan struct{}, func()) {
		launchMu.Lock()
		launchEntered = make(chan struct{})
		launchRelease = make(chan struct{})
		ent := launchEntered
		rel := launchRelease
		launchMu.Unlock()
		blockLaunchOnce.Store(true)
		var once sync.Once
		return ent, func() {
			once.Do(func() { close(rel) })
		}
	}

	sup := &sandbox.Supervisor{
		DataDir: filepath.Join(dataDir, "sup"),
		Client:  func() *rungv.Client { return &rungv.Client{Path: fakeRungvBin} },
		Launch: func(spec sandbox.Spec) (sandbox.Spec, func(), error) {
			if blockLaunchOnce.CompareAndSwap(true, false) {
				launchMu.Lock()
				ent := launchEntered
				rel := launchRelease
				launchMu.Unlock()
				if ent != nil {
					close(ent)
				}
				if rel != nil {
					<-rel
				}
			}
			return spec, nil, nil
		},
	}
	t.Cleanup(func() { sup.StopAll(3 * time.Second) })

	type mockCtr struct {
		labels    map[string]string
		binds     []string
		running   bool
		stopped   bool
		stopCalls int
	}
	var (
		ctrMu      sync.Mutex
		nextCtrID  int
		containers = map[string]*mockCtr{
			"t5-12-foreign-ctr": {labels: map[string]string{"io.gvx.extension": "t5-12-other"}, running: true},
			"t5-12-host-ctr":    {labels: map[string]string{}, running: true},
		}
		blockDockerStartOnce atomic.Bool
		dockerBarrierMu      sync.Mutex
		dockerStartEntered   chan struct{}
		dockerStartRelease   chan struct{}
	)
	armDockerStartBarrier := func() (<-chan struct{}, func()) {
		dockerBarrierMu.Lock()
		dockerStartEntered = make(chan struct{})
		dockerStartRelease = make(chan struct{})
		ent := dockerStartEntered
		rel := dockerStartRelease
		dockerBarrierMu.Unlock()
		blockDockerStartOnce.Store(true)
		var once sync.Once
		return ent, func() {
			once.Do(func() { close(rel) })
		}
	}

	dockerSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := r.URL.Path
		if len(p) > 3 && strings.HasPrefix(p, "/v") && p[2] >= '0' && p[2] <= '9' {
			if idx := strings.IndexByte(p[2:], '/'); idx > 0 {
				p = p[2+idx:]
			}
		}
		switch {
		case r.Method == http.MethodGet && strings.HasPrefix(p, "/images/") && strings.HasSuffix(p, "/json"):
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"Id":"sha256:alpine","Config":{"Labels":{}}}`))
		case r.Method == http.MethodPost && p == "/containers/create":
			var req struct {
				Labels     map[string]string `json:"Labels"`
				HostConfig struct {
					Binds []string `json:"Binds"`
				} `json:"HostConfig"`
			}
			_ = json.NewDecoder(r.Body).Decode(&req)
			ctrMu.Lock()
			nextCtrID++
			cid := fmt.Sprintf("t5-12-ctr-%d", nextCtrID)
			containers[cid] = &mockCtr{
				labels: req.Labels,
				binds:  req.HostConfig.Binds,
			}
			ctrMu.Unlock()
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(map[string]string{"Id": cid})
		case r.Method == http.MethodGet && p == "/containers/json":
			ctrMu.Lock()
			var list []map[string]any
			for cid, c := range containers {
				if c.running || r.URL.Query().Get("all") == "1" {
					list = append(list, map[string]any{"Id": cid, "Labels": c.labels})
				}
			}
			ctrMu.Unlock()
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(list)
		case r.Method == http.MethodGet && strings.HasPrefix(p, "/containers/") && strings.HasSuffix(p, "/json"):
			cid := strings.TrimSuffix(strings.TrimPrefix(p, "/containers/"), "/json")
			ctrMu.Lock()
			c, ok := containers[cid]
			ctrMu.Unlock()
			if !ok {
				http.Error(w, `{"message":"no such container"}`, http.StatusNotFound)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"Id":         cid,
				"Image":      "alpine:3.20",
				"Config":     map[string]any{"Labels": c.labels},
				"HostConfig": map[string]any{"Binds": c.binds},
			})
		case r.Method == http.MethodPost && strings.HasPrefix(p, "/containers/") && strings.HasSuffix(p, "/start"):
			if blockDockerStartOnce.CompareAndSwap(true, false) {
				dockerBarrierMu.Lock()
				ent := dockerStartEntered
				rel := dockerStartRelease
				dockerBarrierMu.Unlock()
				if ent != nil {
					close(ent)
				}
				if rel != nil {
					<-rel
				}
			}
			cid := strings.TrimSuffix(strings.TrimPrefix(p, "/containers/"), "/start")
			ctrMu.Lock()
			if c, ok := containers[cid]; ok {
				c.running = true
				c.stopped = false
			}
			ctrMu.Unlock()
			w.WriteHeader(http.StatusNoContent)
		case r.Method == http.MethodPost && strings.HasPrefix(p, "/containers/") && strings.HasSuffix(p, "/stop"):
			cid := strings.TrimSuffix(strings.TrimPrefix(p, "/containers/"), "/stop")
			ctrMu.Lock()
			if c, ok := containers[cid]; ok {
				c.running = false
				c.stopped = true
				c.stopCalls++
			}
			ctrMu.Unlock()
			w.WriteHeader(http.StatusNoContent)
		default:
			w.WriteHeader(http.StatusOK)
		}
	}))
	defer dockerSrv.Close()

	uiHost := NewMockUIHost()
	uiHost.PickFolderResult = allowedDir
	mgr, err := ext.NewManager(ext.Config{
		DataDir:       dataDir,
		Keychain:      newFakeKeychain(t),
		DeveloperMode: func() bool { return true },
		Supervisor:    sup,
		HostInfo:      func() ext.HostInfo { return ext.HostInfo{OS: runtime.GOOS, Arch: runtime.GOARCH, Locale: "en"} },
		DockerBackendDial: func(ctx context.Context) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "tcp", strings.TrimPrefix(dockerSrv.URL, "http://"))
		},
		UIHost: uiHost,
	})
	if err != nil {
		t.Fatalf("ext.NewManager: %v", err)
	}
	if err := mgr.Start(context.Background()); err != nil {
		t.Fatalf("mgr.Start: %v", err)
	}
	defer func() { _ = mgr.Close(5 * time.Second) }()

	// Part A: Uninstall stops owned running Docker containers and child sandboxes.
	uninstallSrc := `package main

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

func dockerDo(ctx context.Context, method, p, body string) (int, []byte, error) {
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
	var r io.Reader
	if body != "" {
		r = bytes.NewBufferString(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, base+p, r)
	if err != nil {
		return 0, nil, err
	}
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := (&http.Client{Transport: tr}).Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, raw, nil
}

func main() {
	app := extsdk.New()
	app.Tool("spawn_resources", func(ctx *extsdk.ToolContext, _ map[string]any) (any, error) {
		startTrue := true
		sbx, err := ctx.Host.SandboxCreate(ctx, extsdk.CreateSandboxRequest{
			Name:  "t5-12-uninstall-sbx",
			Image: "alpine:3.20",
			Start: &startTrue,
		})
		if err != nil {
			return nil, err
		}
		c, b, e := dockerDo(ctx, http.MethodPost, "/containers/create", ` + "`" + `{"Image":"alpine:3.20"}` + "`" + `)
		if e != nil || c != 201 {
			return nil, fmt.Errorf("create: %d %s %v", c, b, e)
		}
		var res struct{ Id string }
		_ = json.Unmarshal(b, &res)
		if sc, sb, se := dockerDo(ctx, http.MethodPost, "/containers/"+res.Id+"/start", ""); se != nil || sc != 204 {
			return nil, fmt.Errorf("start: %d %s %v", sc, sb, se)
		}
		return map[string]string{"sbx": sbx.ID, "ctr": res.Id}, nil
	})
	_ = app.Run(context.Background())
}
`
	uninstallManifest := `schema_version = 1
id = "t5-12-uninstall"
version = "0.1.0"
publisher = "acme"
name = "T5 12 Uninstall"
permissions = ["docker", "sandboxes"]

[runtime]
type = "binary"
dev_entrypoint = "bin/ext"

[[contributes.tool]]
id = "spawn_resources"
description = "Spawn docker container and child sandbox"
`
	if runtime.GOOS == "windows" {
		uninstallManifest = strings.ReplaceAll(uninstallManifest, `"bin/ext"`, `"bin/ext.exe"`)
	}
	uninstallDir := e2e4CreateBinaryExt(t, t.TempDir(), "t5-12-uninstall", uninstallManifest, uninstallSrc)
	if _, err := mgr.InstallLocal(uninstallDir); err != nil {
		t.Fatalf("InstallLocal(t5-12-uninstall): %v", err)
	}
	if err := mgr.AllowAndBuild("t5-12-uninstall"); err != nil {
		t.Fatalf("AllowAndBuild(t5-12-uninstall): %v", err)
	}
	e2e4WaitExtState(t, mgr, "t5-12-uninstall", 10*time.Second, func(i ext.ExtensionInfo) bool { return i.State == ext.StateRunning })

	rawRes, err := mgr.InvokeTool(context.Background(), "t5-12-uninstall", "spawn_resources", json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("InvokeTool(spawn_resources): %v", err)
	}
	var spawned struct {
		Sbx string `json:"sbx"`
		Ctr string `json:"ctr"`
	}
	if err := json.Unmarshal(rawRes, &spawned); err != nil {
		t.Fatal(err)
	}
	if !sup.State(spawned.Sbx).Status.Active() {
		t.Fatalf("expected child sandbox active before Uninstall")
	}

	if err := mgr.Uninstall("t5-12-uninstall", true); err != nil {
		t.Fatalf("Uninstall(t5-12-uninstall): %v", err)
	}
	if st := sup.State(spawned.Sbx).Status; st.Active() {
		t.Fatalf("expected child sandbox stopped after Uninstall, got %v", st)
	}
	ctrMu.Lock()
	uOwned := containers[spawned.Ctr]
	uForeign := containers["t5-12-foreign-ctr"]
	uHost := containers["t5-12-host-ctr"]
	ctrMu.Unlock()
	if uOwned == nil || uOwned.running || !uOwned.stopped || uOwned.stopCalls == 0 {
		t.Fatalf("expected owned Docker container stopped on Uninstall, got %+v", uOwned)
	}
	if !uForeign.running || uForeign.stopCalls != 0 || !uHost.running || uHost.stopCalls != 0 {
		t.Fatalf("expected foreign/host containers untouched, foreign=%+v host=%+v", uForeign, uHost)
	}
	if _, stillListed := e2e4FindExtInfo(mgr, "t5-12-uninstall"); stillListed {
		t.Fatalf("expected t5-12-uninstall removed from List()")
	}

	// Part B: End-to-end TOCTOU serialization for RemoveAllowedFolder against in-flight Docker /start, SandboxStart, and Restore.
	toctouSrc := `package main

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

func dockerDo(ctx context.Context, method, p, body string) (int, []byte, error) {
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
	var r io.Reader
	if body != "" {
		r = bytes.NewBufferString(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, base+p, r)
	if err != nil {
		return 0, nil, err
	}
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := (&http.Client{Transport: tr}).Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, raw, nil
}

func main() {
	app := extsdk.New()
	app.View("main", extsdk.View{
		Render: func(c *extsdk.ViewContext, ui *extsdk.UI) extsdk.Node {
			return ui.Column(ui.Button("Pick", extsdk.Key("pick"), extsdk.OnTap("pick")))
		},
		On: extsdk.Handlers{
			"pick": func(ctx *extsdk.EventContext) error {
				_, ok, err := ctx.Host.PickFolder(ctx)
				if err != nil || !ok {
					return fmt.Errorf("PickFolder failed: ok=%v err=%v", ok, err)
				}
				return nil
			},
		},
	})
	app.Tool("prep_docker", func(ctx *extsdk.ToolContext, args map[string]any) (any, error) {
		dir, _ := args["dir"].(string)
		c, b, e := dockerDo(ctx, http.MethodPost, "/containers/create", fmt.Sprintf(` + "`" + `{"Image":"alpine:3.20","HostConfig":{"Binds":[%q]}}` + "`" + `, dir+":/work:rw"))
		if e != nil || c != 201 {
			return nil, fmt.Errorf("create: %d %s %v", c, b, e)
		}
		var res struct{ Id string }
		_ = json.Unmarshal(b, &res)
		return map[string]string{"ctr": res.Id}, nil
	})
	app.Tool("start_docker", func(ctx *extsdk.ToolContext, args map[string]any) (any, error) {
		cid, _ := args["ctr"].(string)
		sc, sb, se := dockerDo(ctx, http.MethodPost, "/containers/"+cid+"/start", "")
		return map[string]any{"code": sc, "body": string(sb), "err": fmtErr(se)}, nil
	})
	app.Tool("prep_sbx", func(ctx *extsdk.ToolContext, args map[string]any) (any, error) {
		dir, _ := args["dir"].(string)
		startFalse := false
		ckptTrue := true
		sbx, err := ctx.Host.SandboxCreate(ctx, extsdk.CreateSandboxRequest{
			Name:       "t5-12-toctou-sbx",
			Image:      "alpine:3.20",
			Mounts:     []extsdk.SandboxMount{{Type: "bind", Source: dir, Target: "/work"}},
			Checkpoint: &ckptTrue,
			Start:      &startFalse,
		})
		if err != nil {
			return nil, err
		}
		return map[string]string{"sbx": sbx.ID}, nil
	})
	app.Tool("start_sbx", func(ctx *extsdk.ToolContext, args map[string]any) (any, error) {
		sbxID, _ := args["sbx"].(string)
		_, err := ctx.Host.SandboxStart(ctx, sbxID)
		return map[string]any{"err": fmtErr(err)}, nil
	})
	app.Tool("restore_sbx", func(ctx *extsdk.ToolContext, args map[string]any) (any, error) {
		sbxID, _ := args["sbx"].(string)
		_, err := ctx.Host.Restore(ctx, sbxID, extsdk.RestoreRequest{Snapshot: "warm"})
		return map[string]any{"err": fmtErr(err)}, nil
	})
	_ = app.Run(context.Background())
}

func fmtErr(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}
`
	toctouManifest := `schema_version = 1
id = "t5-12-toctou"
version = "0.1.0"
publisher = "acme"
name = "T5 12 TOCTOU"
permissions = ["docker", "sandboxes", "snapshots", "ui:pick-folder"]

[runtime]
type = "binary"
dev_entrypoint = "bin/ext"

[[contributes.view]]
id = "main"
title = "Main"

[[contributes.tool]]
id = "prep_docker"
description = "Create docker container"

[[contributes.tool]]
id = "start_docker"
description = "Start docker container"

[[contributes.tool]]
id = "prep_sbx"
description = "Create stopped sandbox"

[[contributes.tool]]
id = "start_sbx"
description = "Start sandbox"

[[contributes.tool]]
id = "restore_sbx"
description = "Restore sandbox"
`
	if runtime.GOOS == "windows" {
		toctouManifest = strings.ReplaceAll(toctouManifest, `"bin/ext"`, `"bin/ext.exe"`)
	}
	toctouDir := e2e4CreateBinaryExt(t, t.TempDir(), "t5-12-toctou", toctouManifest, toctouSrc)
	if _, err := mgr.InstallLocal(toctouDir); err != nil {
		t.Fatalf("InstallLocal(t5-12-toctou): %v", err)
	}
	if err := mgr.AllowAndBuild("t5-12-toctou"); err != nil {
		t.Fatalf("AllowAndBuild(t5-12-toctou): %v", err)
	}
	e2e4WaitExtState(t, mgr, "t5-12-toctou", 10*time.Second, func(i ext.ExtensionInfo) bool { return i.State == ext.StateRunning })

	grantFolder := func() {
		t.Helper()
		if err := mgr.SendViewEvent("t5-12-toctou", "main", uiblock.Event{Node: "/pick", Kind: uiblock.EventTap}); err != nil {
			t.Fatalf("SendViewEvent(pick): %v", err)
		}
	}

	// 1. Docker /containers/{id}/start TOCTOU vs RemoveAllowedFolder.
	grantFolder()
	dirArgs, _ := json.Marshal(map[string]string{"dir": allowedDir})
	prepDocRaw, err := mgr.InvokeTool(context.Background(), "t5-12-toctou", "prep_docker", dirArgs)
	if err != nil {
		t.Fatalf("prep_docker: %v", err)
	}
	var docID struct {
		Ctr string `json:"ctr"`
	}
	_ = json.Unmarshal(prepDocRaw, &docID)

	dEnt, dRel := armDockerStartBarrier()
	t.Cleanup(dRel)
	dStartErr := make(chan error, 1)
	go func() {
		a, _ := json.Marshal(map[string]string{"ctr": docID.Ctr})
		_, err := mgr.InvokeTool(context.Background(), "t5-12-toctou", "start_docker", a)
		dStartErr <- err
	}()
	<-dEnt
	remDoc := make(chan error, 1)
	go func() {
		remDoc <- mgr.RemoveAllowedFolder("t5-12-toctou", allowedDir)
	}()
	select {
	case err := <-remDoc:
		t.Fatalf("RemoveAllowedFolder returned while Docker /start held policyMu.RLock (err=%v)", err)
	case <-time.After(80 * time.Millisecond):
	}
	dRel()
	if err := <-remDoc; err != nil {
		t.Fatalf("RemoveAllowedFolder(docker): %v", err)
	}
	if err := <-dStartErr; err != nil {
		t.Fatalf("start_docker tool call err: %v", err)
	}
	ctrMu.Lock()
	cState := containers[docID.Ctr]
	ctrMu.Unlock()
	if cState.running || !cState.stopped {
		t.Fatalf("E2E Docker TOCTOU leak: container %s left running=%v stopped=%v", docID.Ctr, cState.running, cState.stopped)
	}

	// 2. HostAPI SandboxStart TOCTOU vs RemoveAllowedFolder.
	grantFolder()
	prepSbxRaw, err := mgr.InvokeTool(context.Background(), "t5-12-toctou", "prep_sbx", dirArgs)
	if err != nil {
		t.Fatalf("prep_sbx: %v", err)
	}
	var sbxID struct {
		Sbx string `json:"sbx"`
	}
	_ = json.Unmarshal(prepSbxRaw, &sbxID)

	sEnt, sRel := armLaunchBarrier()
	t.Cleanup(sRel)
	sStartErr := make(chan error, 1)
	go func() {
		a, _ := json.Marshal(map[string]string{"sbx": sbxID.Sbx})
		_, err := mgr.InvokeTool(context.Background(), "t5-12-toctou", "start_sbx", a)
		sStartErr <- err
	}()
	<-sEnt
	remSbx := make(chan error, 1)
	go func() {
		remSbx <- mgr.RemoveAllowedFolder("t5-12-toctou", allowedDir)
	}()
	select {
	case err := <-remSbx:
		t.Fatalf("RemoveAllowedFolder returned while SandboxStart held policyMu.RLock (err=%v)", err)
	case <-time.After(80 * time.Millisecond):
	}
	sRel()
	if err := <-remSbx; err != nil {
		t.Fatalf("RemoveAllowedFolder(sbxStart): %v", err)
	}
	if err := <-sStartErr; err != nil {
		t.Fatalf("start_sbx tool call err: %v", err)
	}
	if st := sup.State(sbxID.Sbx).Status; st.Active() {
		t.Fatalf("E2E SandboxStart TOCTOU leak: sandbox %s remained %v", sbxID.Sbx, st)
	}

	// 3. HostAPI SandboxRestore TOCTOU vs RemoveAllowedFolder.
	snapDir := filepath.Join(sup.SnapshotRoot(sbxID.Sbx), "snap-warm")
	if err := os.MkdirAll(snapDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(snapDir, "manifest.json"), []byte(`{"version":1}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(ext.ExtensionDataDir(dataDir, "t5-12-toctou"), "snapshots"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ext.ExtensionDataDir(dataDir, "t5-12-toctou"), "snapshots", sbxID.Sbx+".json"), []byte(`{"warm":"snap-warm"}`), 0o600); err != nil {
		t.Fatal(err)
	}

	grantFolder()
	rEnt, rRel := armLaunchBarrier()
	t.Cleanup(rRel)
	rErr := make(chan error, 1)
	go func() {
		a, _ := json.Marshal(map[string]string{"sbx": sbxID.Sbx})
		_, err := mgr.InvokeTool(context.Background(), "t5-12-toctou", "restore_sbx", a)
		rErr <- err
	}()
	<-rEnt
	remRest := make(chan error, 1)
	go func() {
		remRest <- mgr.RemoveAllowedFolder("t5-12-toctou", allowedDir)
	}()
	select {
	case err := <-remRest:
		t.Fatalf("RemoveAllowedFolder returned while SandboxRestore held policyMu.RLock (err=%v)", err)
	case <-time.After(80 * time.Millisecond):
	}
	rRel()
	if err := <-remRest; err != nil {
		t.Fatalf("RemoveAllowedFolder(sbxRestore): %v", err)
	}
	if err := <-rErr; err != nil {
		t.Fatalf("restore_sbx tool call err: %v", err)
	}
	if st := sup.State(sbxID.Sbx).Status; st.Active() {
		t.Fatalf("E2E SandboxRestore TOCTOU leak: sandbox %s remained %v", sbxID.Sbx, st)
	}
}

func TestT5_13_ExecPrivilegedDowngradedStartRestoreAndCloseEphemeralCleanup(t *testing.T) {
	dataDir := newShortTempDataDir(t)

	// Build a minimal cross-platform fake rungv for Supervisor Start/Restore.
	fakeRungvSrc := filepath.Join(t.TempDir(), "fake_rungv.go")
	const fakeRungvCode = `package main

import (
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"
	"time"
)

func main() {
	if len(os.Args) < 2 {
		return
	}
	switch os.Args[1] {
	case "version":
		fmt.Println("rungv v0.21.0")
	case "run", "restore":
		if len(os.Args) >= 3 && os.Args[2] == "-help" {
			fmt.Println("  --unix-forward value")
			fmt.Println("  -control-token-file string")
			return
		}
		sig := make(chan os.Signal, 1)
		signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
		done := make(chan struct{})
		go func() {
			_, _ = io.Copy(io.Discard, os.Stdin)
			close(done)
		}()
		select {
		case <-sig:
		case <-done:
		case <-time.After(30 * time.Second):
		}
	}
}
`
	if err := os.WriteFile(fakeRungvSrc, []byte(fakeRungvCode), 0o600); err != nil {
		t.Fatal(err)
	}
	exeName := "rungv"
	if runtime.GOOS == "windows" {
		exeName = "rungv.exe"
	}
	fakeRungvBin := filepath.Join(t.TempDir(), exeName)
	if out, err := exec.Command("go", "build", "-o", fakeRungvBin, fakeRungvSrc).CombinedOutput(); err != nil {
		t.Fatalf("build fake rungv: %v\n%s", err, out)
	}
	t.Cleanup(func() { rungv.ForgetFeatures(fakeRungvBin) })

	var (
		slowOnce      atomic.Bool
		launchEntered = make(chan struct{})
	)
	sup := &sandbox.Supervisor{
		DataDir: filepath.Join(dataDir, "sup"),
		Client:  func() *rungv.Client { return &rungv.Client{Path: fakeRungvBin} },
		Launch: func(spec sandbox.Spec) (sandbox.Spec, func(), error) {
			if spec.Name == "t5-13-slow-eph" && slowOnce.CompareAndSwap(true, false) {
				close(launchEntered)
				time.Sleep(2400 * time.Millisecond)
			}
			return spec, nil, nil
		},
	}
	t.Cleanup(func() { sup.StopAll(3 * time.Second) })

	mgr, h := newTestManager(t, WithDataDir(dataDir), WithDeveloperMode(true), WithSupervisor(sup))
	if err := mgr.Start(context.Background()); err != nil {
		t.Fatalf("mgr.Start: %v", err)
	}

	const extSrc = `package main

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

func dockerDo(ctx context.Context, method, p, body string) (int, []byte, error) {
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
	var r io.Reader
	if body != "" {
		r = bytes.NewBufferString(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, base+p, r)
	if err != nil {
		return 0, nil, err
	}
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := (&http.Client{Transport: tr}).Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, raw, nil
}

func main() {
	app := extsdk.New()
	app.Tool("test_exec_priv", func(ctx *extsdk.ToolContext, _ map[string]any) (any, error) {
		c, b, e := dockerDo(ctx, http.MethodPost, "/containers/create", ` + "`" + `{"Image":"alpine:3.20"}` + "`" + `)
		if e != nil || c != http.StatusCreated {
			return nil, fmt.Errorf("create: %d %s %v", c, b, e)
		}
		var cr struct{ Id string }
		_ = json.Unmarshal(b, &cr)

		for _, tc := range []struct {
			name string
			body string
			want int
		}{
			{"upper-true", ` + "`" + `{"Cmd":["id"],"Privileged":true}` + "`" + `, http.StatusForbidden},
			{"lower-true", ` + "`" + `{"Cmd":["id"],"privileged":true}` + "`" + `, http.StatusForbidden},
			{"dup-keys", ` + "`" + `{"Cmd":["id"],"Privileged":false,"privileged":true}` + "`" + `, http.StatusBadRequest},
			{"non-bool", ` + "`" + `{"Cmd":["id"],"Privileged":"true"}` + "`" + `, http.StatusBadRequest},
			{"valid-false", ` + "`" + `{"Cmd":["id"],"Privileged":false}` + "`" + `, http.StatusCreated},
			{"valid-omitted", ` + "`" + `{"Cmd":["id"]}` + "`" + `, http.StatusCreated},
		} {
			sc, sb, se := dockerDo(ctx, http.MethodPost, "/containers/"+cr.Id+"/exec", tc.body)
			if se != nil || sc != tc.want {
				return nil, fmt.Errorf("%s: got status=%d body=%s err=%v, want %d", tc.name, sc, sb, se, tc.want)
			}
		}
		return map[string]any{"ok": true, "ctr": cr.Id}, nil
	})
	app.Tool("create_priv_sbx", func(ctx *extsdk.ToolContext, _ map[string]any) (any, error) {
		startFalse := false
		ckptTrue := true
		sbx, err := ctx.Host.SandboxCreate(ctx, extsdk.CreateSandboxRequest{
			Name:            "t5-13-priv-sbx",
			Image:           "alpine:3.20",
			MountDockerSock: true,
			Checkpoint:      &ckptTrue,
			Start:           &startFalse,
		})
		if err != nil {
			return nil, err
		}
		return map[string]string{"sbx": sbx.ID}, nil
	})
	app.Tool("try_start_restore", func(ctx *extsdk.ToolContext, args map[string]any) (any, error) {
		sbxID, _ := args["sbx"].(string)
		_, startErr := ctx.Host.SandboxStart(ctx, sbxID)
		_, restoreErr := ctx.Host.Restore(ctx, sbxID, extsdk.RestoreRequest{Snapshot: "warm"})
		return map[string]any{
			"start_denied":   extsdk.IsPermissionDenied(startErr),
			"restore_denied": extsdk.IsPermissionDenied(restoreErr),
			"start_err":      fmtErr(startErr),
			"restore_err":    fmtErr(restoreErr),
		}, nil
	})
	app.Tool("spawn_slow_ephemeral", func(ctx *extsdk.ToolContext, _ map[string]any) (any, error) {
		startTrue := true
		return ctx.Host.SandboxCreate(ctx, extsdk.CreateSandboxRequest{
			Name:      "t5-13-slow-eph",
			Image:     "alpine:3.20",
			Ephemeral: true,
			Start:     &startTrue,
		})
	})
	_ = app.Run(context.Background())
}

func fmtErr(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}
`
	extSrcRoot := t.TempDir()
	fullManifest := `schema_version = 1
id = "t5-13-ext"
version = "0.1.0"
publisher = "acme"
name = "T5 13 Ext"
permissions = ["docker", "sandboxes", "snapshots", "network"]

[runtime]
type = "binary"
dev_entrypoint = "bin/ext"

[[contributes.tool]]
id = "test_exec_priv"
description = "Test exec privileged enforcement"

[[contributes.tool]]
id = "create_priv_sbx"
description = "Create persisted sandbox with network+docker"

[[contributes.tool]]
id = "try_start_restore"
description = "Attempt start and restore"

[[contributes.tool]]
id = "spawn_slow_ephemeral"
description = "Spawn slow ephemeral sandbox"
`
	if runtime.GOOS == "windows" {
		fullManifest = strings.ReplaceAll(fullManifest, `"bin/ext"`, `"bin/ext.exe"`)
	}
	extDir := e2e4CreateBinaryExt(t, extSrcRoot, "t5-13-ext", fullManifest, extSrc)
	if _, err := mgr.InstallLocal(extDir); err != nil {
		t.Fatalf("InstallLocal(full): %v", err)
	}
	if err := mgr.AllowAndBuild("t5-13-ext"); err != nil {
		t.Fatalf("AllowAndBuild(full): %v", err)
	}
	e2e4WaitExtState(t, mgr, "t5-13-ext", 10*time.Second, func(i ext.ExtensionInfo) bool { return i.State == ext.StateRunning })

	// 1. Gap 1: Exec privileged rejection on scoped Docker proxy.
	if _, err := mgr.InvokeTool(context.Background(), "t5-13-ext", "test_exec_priv", nil); err != nil {
		t.Fatalf("test_exec_priv failed: %v", err)
	}
	for _, req := range h.DockerBackend.SnapshotRequests() {
		if strings.HasSuffix(req.Path, "/exec") && strings.Contains(strings.ToLower(string(req.RawBody)), `"privileged":true`) {
			t.Fatalf("privileged exec leaked to DockerBackend: %s", string(req.RawBody))
		}
	}

	// 2. Gap 2: Create persisted sandbox with network + docker, seed snapshot, then downgrade manifest.
	sbxRaw, err := mgr.InvokeTool(context.Background(), "t5-13-ext", "create_priv_sbx", nil)
	if err != nil {
		t.Fatalf("create_priv_sbx: %v", err)
	}
	var sbxOut struct {
		Sbx string `json:"sbx"`
	}
	_ = json.Unmarshal(sbxRaw, &sbxOut)
	if sbxOut.Sbx == "" {
		t.Fatalf("missing sbx ID: %s", sbxRaw)
	}
	snapDir := filepath.Join(sup.SnapshotRoot(sbxOut.Sbx), "snap-warm")
	if err := os.MkdirAll(snapDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(snapDir, "manifest.json"), []byte(`{"version":1}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(ext.ExtensionDataDir(dataDir, "t5-13-ext"), "snapshots"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ext.ExtensionDataDir(dataDir, "t5-13-ext"), "snapshots", sbxOut.Sbx+".json"), []byte(`{"warm":"snap-warm"}`), 0o600); err != nil {
		t.Fatal(err)
	}

	downgradedManifest := strings.Replace(fullManifest, `version = "0.1.0"`, `version = "0.2.0"`, 1)
	downgradedManifest = strings.Replace(downgradedManifest, `permissions = ["docker", "sandboxes", "snapshots", "network"]`, `permissions = ["sandboxes", "snapshots"]`, 1)
	if err := os.WriteFile(filepath.Join(extDir, ext.ManifestName), []byte(downgradedManifest), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := mgr.InstallLocal(extDir); err != nil {
		t.Fatalf("InstallLocal(downgraded): %v", err)
	}
	if err := mgr.AllowAndBuild("t5-13-ext"); err != nil {
		t.Fatalf("AllowAndBuild(downgraded): %v", err)
	}
	e2e4WaitExtState(t, mgr, "t5-13-ext", 10*time.Second, func(i ext.ExtensionInfo) bool { return i.State == ext.StateRunning })

	args, _ := json.Marshal(map[string]string{"sbx": sbxOut.Sbx})
	tryRaw, err := mgr.InvokeTool(context.Background(), "t5-13-ext", "try_start_restore", args)
	if err != nil {
		t.Fatalf("try_start_restore: %v", err)
	}
	var tryRes struct {
		StartDenied   bool   `json:"start_denied"`
		RestoreDenied bool   `json:"restore_denied"`
		StartErr      string `json:"start_err"`
		RestoreErr    string `json:"restore_err"`
	}
	_ = json.Unmarshal(tryRaw, &tryRes)
	if !tryRes.StartDenied || !tryRes.RestoreDenied {
		t.Fatalf("expected 403 permission_denied on downgraded Start & Restore, got %+v", tryRes)
	}
	if st := sup.State(sbxOut.Sbx).Status; st.Active() {
		t.Fatalf("downgraded sandbox %s should not be active: %v", sbxOut.Sbx, st)
	}

	// 3. Gap 3: In-flight slow (>2s) ephemeral sandbox create during Manager.Close leaves zero running sandboxes.
	slowOnce.Store(true)
	toolDone := make(chan error, 1)
	go func() {
		_, err := mgr.InvokeTool(context.Background(), "t5-13-ext", "spawn_slow_ephemeral", nil)
		toolDone <- err
	}()
	select {
	case <-launchEntered:
	case err := <-toolDone:
		t.Fatalf("spawn_slow_ephemeral exited before Launch: %v", err)
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for slow ephemeral Launch")
	}

	if err := mgr.Close(5 * time.Second); err != nil {
		t.Fatalf("mgr.Close: %v", err)
	}
	if running := sup.Running(); len(running) != 0 {
		t.Fatalf("Manager.Close left orphaned running ephemeral sandbox(es): %v", running)
	}
	<-toolDone
}
