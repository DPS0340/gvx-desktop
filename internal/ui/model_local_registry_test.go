package ui

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/ironpark/ggui"

	"github.com/ironpark/gvx-desktop/internal/dockerapi"
	"github.com/ironpark/gvx-desktop/internal/recipes"
	"github.com/ironpark/gvx-desktop/internal/sandbox"
	"github.com/ironpark/gvx-desktop/internal/store"
)

func localRegTestModel(t *testing.T, initSettings func(*store.Settings), specs ...sandbox.Spec) (*Model, *ggui.Probe, store.Store) {
	t.Helper()
	t.Cleanup(func() { SetLanguage(LangEnglish) })
	SetLanguage(LangEnglish)

	dir := t.TempDir()
	st := store.Store{Dir: dir}
	set := store.DefaultSettings()
	set.Language = LangEnglish
	set.SandboxEngine = false
	if initSettings != nil {
		initSettings(&set)
	}
	if err := st.SaveSettings(set); err != nil {
		t.Fatal(err)
	}
	if len(specs) > 0 {
		if err := st.SaveSandboxes(specs); err != nil {
			t.Fatal(err)
		}
	}

	var m *Model
	p := ggui.ProbeBuilder(func() ggui.Widget {
		return ggui.Provide(ggui.ReducedMotionKey, true, Build(m))
	}, ggui.Sz(1280, 2400))
	p.Setup(func() {
		m = NewModel(st, dir)
		m.localRegPort = -1 // ephemeral loopback port; never bind 5001 in tests
		m.Attach(p)
		m.followLocalRegistry()
	})
	t.Cleanup(p.Close)
	p.Frame()
	return m, p, st
}

func testSHA256(b []byte) string {
	sum := sha256.Sum256(b)
	return "sha256:" + hex.EncodeToString(sum[:])
}

func pushOCIImageToAddr(t *testing.T, addr, repo, tag, osName, arch string, layer []byte) {
	t.Helper()
	base := "http://" + addr
	client := &http.Client{Timeout: 5 * time.Second}

	pushBlob := func(data []byte) string {
		d := testSHA256(data)
		u := fmt.Sprintf("%s/v2/%s/blobs/uploads/?digest=%s", base, repo, url.QueryEscape(d))
		req, err := http.NewRequest(http.MethodPost, u, bytes.NewReader(data))
		if err != nil {
			t.Fatalf("NewRequest blob: %v", err)
		}
		resp, err := client.Do(req)
		if err != nil {
			t.Fatalf("POST blob: %v", err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusCreated {
			body, _ := io.ReadAll(resp.Body)
			t.Fatalf("POST blob status=%d body=%s", resp.StatusCode, body)
		}
		return d
	}

	cfgBytes, _ := json.Marshal(map[string]string{
		"os":           osName,
		"architecture": arch,
	})
	cfgDigest := pushBlob(cfgBytes)
	layerDigest := pushBlob(layer)

	manifestBytes, _ := json.Marshal(map[string]any{
		"schemaVersion": 2,
		"mediaType":     "application/vnd.oci.image.manifest.v1+json",
		"config": map[string]any{
			"mediaType": "application/vnd.oci.image.config.v1+json",
			"digest":    cfgDigest,
			"size":      len(cfgBytes),
		},
		"layers": []map[string]any{{
			"mediaType": "application/vnd.oci.image.layer.v1.tar+gzip",
			"digest":    layerDigest,
			"size":      len(layer),
		}},
	})
	u := fmt.Sprintf("%s/v2/%s/manifests/%s", base, repo, tag)
	req, err := http.NewRequest(http.MethodPut, u, bytes.NewReader(manifestBytes))
	if err != nil {
		t.Fatalf("NewRequest manifest: %v", err)
	}
	req.Header.Set("Content-Type", "application/vnd.oci.image.manifest.v1+json")
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("PUT manifest: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("PUT manifest status=%d body=%s", resp.StatusCode, body)
	}
}

// TestLocalRegistryAutoLifecycle verifies Auto-ON when Sandbox Engine,
// docker:dind, or k3s is active, and Auto-OFF when none of them are active.
func TestLocalRegistryAutoLifecycle(t *testing.T) {
	dind := recipes.DockerEngine()
	k3s := recipes.KubernetesNode()
	m, p, _ := localRegTestModel(t, func(s *store.Settings) {
		s.SandboxEngine = false
		s.LocalRegistry = store.LocalRegistryAuto
	}, dind, k3s)

	waitRunning := func(want bool, desc string) LocalRegistryState {
		t.Helper()
		settle(t, p, func() bool {
			st := ggui.Peek(m.LocalRegistry)
			return st.Running == want && (want == (st.Addr != ""))
		}, desc)
		return ggui.Peek(m.LocalRegistry)
	}

	// Initially: no demand, Auto mode -> stopped.
	st := ggui.Peek(m.LocalRegistry)
	if st.Running || st.AutoDemand || st.Mode != store.LocalRegistryAuto {
		t.Fatalf("initial state = %+v, want stopped auto with no demand", st)
	}

	// 1. Sandbox Engine opening -> Auto-ON.
	m.UpdateSettings(func(s *store.Settings) { s.SandboxEngine = true })
	m.SandboxEngineStatus.Set(SandboxEngineStatus{Opening: true})
	st = waitRunning(true, "auto-on on SandboxEngine Opening")
	if !st.AutoDemand {
		t.Errorf("expected AutoDemand=true while SandboxEngine opening: %+v", st)
	}
	resp, err := http.Get("http://" + st.Addr + "/v2/")
	if err != nil {
		t.Fatalf("GET /v2/ failed: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("GET /v2/ status = %d, want 200", resp.StatusCode)
	}

	// Sandbox Engine ready -> stays ON.
	m.SandboxEngineStatus.Set(SandboxEngineStatus{Ready: true})
	p.Frame()
	if !ggui.Peek(m.LocalRegistry).Running {
		t.Fatal("expected registry to stay running when SandboxEngine becomes Ready")
	}

	// Sandbox Engine off -> Auto-OFF.
	m.UpdateSettings(func(s *store.Settings) { s.SandboxEngine = false })
	m.SandboxEngineStatus.Set(SandboxEngineStatus{})
	st = waitRunning(false, "auto-off when SandboxEngine turns off")
	if st.AutoDemand {
		t.Errorf("expected AutoDemand=false: %+v", st)
	}

	// 2. docker:dind sandbox active -> Auto-ON; stopped -> Auto-OFF.
	m.States.Set(map[string]sandbox.State{dind.ID: {Status: sandbox.Running}})
	st = waitRunning(true, "auto-on on docker:dind active")
	if !st.AutoDemand {
		t.Errorf("expected AutoDemand=true for docker:dind: %+v", st)
	}
	m.States.Set(map[string]sandbox.State{dind.ID: {Status: sandbox.Stopped}})
	waitRunning(false, "auto-off when docker:dind stops")

	// 3. k3s sandbox active -> Auto-ON; stopped -> Auto-OFF.
	m.States.Set(map[string]sandbox.State{k3s.ID: {Status: sandbox.Running}})
	st = waitRunning(true, "auto-on on k3s active")
	if !st.AutoDemand {
		t.Errorf("expected AutoDemand=true for k3s: %+v", st)
	}
	m.States.Set(map[string]sandbox.State{k3s.ID: {Status: sandbox.Stopped}})
	waitRunning(false, "auto-off when k3s stops")
}

// TestLocalRegistryExplicitOnOffAndRetry verifies explicit On and Off modes
// as well as RetryLocalRegistry recovering after a port-in-use bind failure.
func TestLocalRegistryExplicitOnOffAndRetry(t *testing.T) {
	k3s := recipes.KubernetesNode()
	m, p, stStore := localRegTestModel(t, func(s *store.Settings) {
		s.SandboxEngine = false
		s.LocalRegistry = store.LocalRegistryAuto
	}, k3s)

	// Explicit On starts even without any Docker or Kubernetes demand.
	m.SetLocalRegistryMode(store.LocalRegistryOn)
	settle(t, p, func() bool { return ggui.Peek(m.LocalRegistry).Running }, "explicit On start")
	if saved, _ := stStore.Settings(); saved.LocalRegistryMode() != store.LocalRegistryOn {
		t.Fatalf("saved mode = %q, want on", saved.LocalRegistry)
	}

	// Explicit Off stops even when k3s demand appears.
	m.States.Set(map[string]sandbox.State{k3s.ID: {Status: sandbox.Running}})
	m.SetLocalRegistryMode(store.LocalRegistryOff)
	settle(t, p, func() bool { return !ggui.Peek(m.LocalRegistry).Running }, "explicit Off stop")
	if saved, _ := stStore.Settings(); saved.LocalRegistryMode() != store.LocalRegistryOff {
		t.Fatalf("saved mode = %q, want off", saved.LocalRegistry)
	}
	if st := ggui.Peek(m.LocalRegistry); !st.AutoDemand {
		t.Errorf("expected AutoDemand=true while k3s running in Off mode: %+v", st)
	}

	// Switching back to Auto with k3s active starts it again.
	m.SetLocalRegistryMode(store.LocalRegistryAuto)
	settle(t, p, func() bool { return ggui.Peek(m.LocalRegistry).Running }, "auto start with k3s active")

	// RetryLocalRegistry after an ephemeral port conflict.
	blocker, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	busyPort := blocker.Addr().(*net.TCPAddr).Port
	defer blocker.Close()

	dir2 := t.TempDir()
	st2 := store.Store{Dir: dir2}
	set2 := store.DefaultSettings()
	set2.Language = LangEnglish
	set2.SandboxEngine = false
	set2.LocalRegistry = store.LocalRegistryOn
	_ = st2.SaveSettings(set2)

	var m2 *Model
	p2 := ggui.ProbeBuilder(func() ggui.Widget {
		return ggui.Provide(ggui.ReducedMotionKey, true, Build(m2))
	}, ggui.Sz(1000, 700))
	p2.Setup(func() {
		m2 = NewModel(st2, dir2)
		m2.localRegPort = busyPort
		m2.Attach(p2)
		m2.followLocalRegistry()
	})
	defer p2.Close()
	p2.Frame()

	settle(t, p2, func() bool { return ggui.Peek(m2.LocalRegistry).Err != "" }, "port conflict error")
	if ggui.Peek(m2.LocalRegistry).Running {
		t.Fatal("expected Running=false on bind failure")
	}

	_ = blocker.Close()
	m2.RetryLocalRegistry()
	settle(t, p2, func() bool {
		st := ggui.Peek(m2.LocalRegistry)
		return st.Running && st.Err == ""
	}, "retry succeeds after port released")
}

// TestLocalRegistryYieldsToLegacySandbox verifies that the built-in registry
// stops and yields when a legacy registry:2 sandbox becomes active, and resumes
// once the legacy sandbox stops.
func TestLocalRegistryYieldsToLegacySandbox(t *testing.T) {
	legacy := recipes.ImageRegistry()
	m, p, _ := localRegTestModel(t, func(s *store.Settings) {
		s.LocalRegistry = store.LocalRegistryOn
	}, legacy)

	settle(t, p, func() bool { return ggui.Peek(m.LocalRegistry).Running }, "initial running")

	// Legacy registry:2 sandbox becomes active -> built-in yields.
	m.States.Set(map[string]sandbox.State{legacy.ID: {Status: sandbox.Running}})
	settle(t, p, func() bool {
		st := ggui.Peek(m.LocalRegistry)
		return !st.Running && st.LegacyActive && !st.LegacyPaused && st.Addr == ""
	}, "yield to legacy sandbox")

	// Legacy paused -> LegacyPaused=true, still yielded.
	m.States.Set(map[string]sandbox.State{legacy.ID: {Status: sandbox.Running, Paused: true}})
	settle(t, p, func() bool {
		st := ggui.Peek(m.LocalRegistry)
		return !st.Running && st.LegacyActive && st.LegacyPaused
	}, "legacy paused")

	// Legacy stops -> built-in automatically starts again.
	m.States.Set(map[string]sandbox.State{legacy.ID: {Status: sandbox.Stopped}})
	settle(t, p, func() bool {
		st := ggui.Peek(m.LocalRegistry)
		return st.Running && !st.LegacyActive && st.Addr != ""
	}, "resume built-in after legacy stops")
}

// TestLocalRegistryPushRenderDeleteAndPrune pushes a real OCI image to the
// built-in server, renders it in Images → Local Registry, filters and deletes
// it via AskDeleteLocalRegistryImage, and tests AskPruneLocalRegistry.
func TestLocalRegistryPushRenderDeleteAndPrune(t *testing.T) {
	m, p, _ := localRegTestModel(t, func(s *store.Settings) {
		s.LocalRegistry = store.LocalRegistryOn
	})

	settle(t, p, func() bool { return ggui.Peek(m.LocalRegistry).Running }, "server started")
	addr := ggui.Peek(m.LocalRegistry).Addr

	// Push v1, then overwrite v1 with a different layer so v1's old blob becomes reclaimable.
	pushOCIImageToAddr(t, addr, "acme/web", "v1.0", "linux", "arm64", bytes.Repeat([]byte("old-layer-"), 256))
	pushOCIImageToAddr(t, addr, "acme/web", "v1.0", "linux", "arm64", bytes.Repeat([]byte("new-layer-"), 512))

	m.OpenRegistry()
	settle(t, p, func() bool {
		l := ggui.Peek(m.LocalRegistryImages)
		st := ggui.Peek(m.LocalRegistry)
		return l.Loaded && len(l.Items) == 1 && st.ImageCount == 1 && st.TotalBytes > 0 && st.ReclaimableBytes > 0
	}, "local registry loaded pushed image with reclaimable bytes")
	p.Frame()

	if ggui.Peek(m.Page) != PageImages || ggui.Peek(m.ImageView) != ImagesRegistry {
		t.Fatalf("OpenRegistry navigated to page=%q view=%d, want images/%d", ggui.Peek(m.Page), ggui.Peek(m.ImageView), ImagesRegistry)
	}

	shows := func(substr string) bool {
		for _, n := range p.Semantics().All() {
			if strings.Contains(n.Name, substr) {
				return true
			}
		}
		return false
	}

	for _, want := range []string{
		"Local Registry",
		"acme/web",
		"v1.0",
		"linux/arm64",
		recipes.RegistryLocal(),
		recipes.RegistryHost(),
		"Running on " + recipes.RegistryLocal(),
	} {
		if !shows(want) {
			t.Errorf("Images → Local Registry missing %q:\n%s", want, p.Semantics())
		}
	}

	// Prune reclaims the unreferenced old manifest & blob.
	m.AskPruneLocalRegistry()
	p.Frame()
	if !ggui.Peek(m.ConfirmOpen) || ggui.Peek(m.ConfirmTitle) != "Prune local registry?" {
		t.Fatalf("AskPruneLocalRegistry dialog title = %q, want Prune local registry?", ggui.Peek(m.ConfirmTitle))
	}
	m.ConfirmOpen.Set(false)
	m.RunConfirmed()
	settle(t, p, func() bool {
		return !m.isBusy("lreg:prune") && ggui.Peek(m.LocalRegistry).ReclaimableBytes == 0
	}, "prune reclaimed unreferenced bytes")

	// Filter hides non-matching entries and restores on clear.
	m.LocalRegistryQuery.Set("does-not-exist")
	p.Frame()
	if shows("acme/web") {
		t.Error("filter did not hide non-matching image")
	}
	m.LocalRegistryQuery.Set("")
	p.Frame()
	if !shows("acme/web") {
		t.Error("clearing filter did not restore image")
	}

	// Delete image via AskDeleteLocalRegistryImage and verify space is reclaimed.
	entry := ggui.Peek(m.LocalRegistryImages).Items[0]
	m.AskDeleteLocalRegistryImage(entry)
	p.Frame()
	if !ggui.Peek(m.ConfirmOpen) || ggui.Peek(m.ConfirmTitle) != "Delete acme/web:v1.0?" || ggui.Peek(m.ConfirmSafe) {
		t.Fatalf("delete confirm = open:%v title:%q safe:%v", ggui.Peek(m.ConfirmOpen), ggui.Peek(m.ConfirmTitle), ggui.Peek(m.ConfirmSafe))
	}
	m.ConfirmOpen.Set(false)
	m.RunConfirmed()
	settle(t, p, func() bool {
		l := ggui.Peek(m.LocalRegistryImages)
		st := ggui.Peek(m.LocalRegistry)
		return !m.isBusy("lreg:"+entry.Key()) && len(l.Items) == 0 && st.TotalBytes == 0 && st.ImageCount == 0
	}, "delete tag and reclaim space")
	p.Frame()

	if !shows("No images in the local registry") {
		t.Errorf("expected empty state after deletion:\n%s", p.Semantics())
	}

	// Disk listing still works when stopped (mode Off).
	pushOCIImageToAddr(t, addr, "team/api", "dev", "linux", "amd64", []byte("layer-bytes"))
	m.SetLocalRegistryMode(store.LocalRegistryOff)
	settle(t, p, func() bool { return !ggui.Peek(m.LocalRegistry).Running }, "stopped in Off mode")
	m.LoadLocalRegistry()
	settle(t, p, func() bool {
		l := ggui.Peek(m.LocalRegistryImages)
		return l.Loaded && len(l.Items) == 1 && l.Items[0].Key() == "team/api:dev"
	}, "disk listing while stopped")
}

// TestSettingsLocalRegistrySectionRendersAndSwitchesMode verifies the Settings
// Local registry section renders status/storage, switches modes, and opens the
// Images → Local Registry tab.
func TestSettingsLocalRegistrySectionRendersAndSwitchesMode(t *testing.T) {
	m, p, stStore := localRegTestModel(t, func(s *store.Settings) {
		s.LocalRegistry = store.LocalRegistryAuto
	})

	m.Go(PageSettings)
	p.Frame()

	shows := func(substr string) bool {
		for _, n := range p.Semantics().All() {
			if strings.Contains(n.Name, substr) {
				return true
			}
		}
		return false
	}

	for _, want := range []string{
		"Local registry",
		"Built-in OCI registry",
		"Auto",
		"Always on",
		"Off",
		"Open in Images",
	} {
		if !shows(want) {
			t.Errorf("Settings missing %q:\n%s", want, p.Semantics())
		}
	}

	// Switch to Always on via the Settings ToggleGroup option.
	p.Tap("Always on")
	settle(t, p, func() bool {
		st := ggui.Peek(m.LocalRegistry)
		return st.Mode == store.LocalRegistryOn && st.Running
	}, "Settings Always on starts server")
	if saved, _ := stStore.Settings(); saved.LocalRegistryMode() != store.LocalRegistryOn {
		t.Fatalf("saved mode = %q, want on", saved.LocalRegistry)
	}
	p.Frame()
	if !shows("Running on " + recipes.RegistryLocal()) {
		t.Errorf("Settings did not update to Running status:\n%s", p.Semantics())
	}

	// Open in Images button navigates straight to Images → Local Registry.
	p.Tap("Open in Images")
	p.Frame()
	if ggui.Peek(m.Page) != PageImages || ggui.Peek(m.ImageView) != ImagesRegistry {
		t.Fatalf("Open in Images navigated to page=%q view=%d, want images/%d", ggui.Peek(m.Page), ggui.Peek(m.ImageView), ImagesRegistry)
	}
}

// TestSidebarPaletteAndDockerImageMenuCleanup verifies the sidebar no longer
// shows the lonely "Image registry" entry under "Recipes", OpenRegistry opens
// Images → Local Registry, Command Palette offers "Local registry", and the
// Docker images row menu puts "Load into cluster" before "Push to local registry".
func TestSidebarPaletteAndDockerImageMenuCleanup(t *testing.T) {
	t.Cleanup(func() { SetLanguage(LangEnglish) })
	SetLanguage(LangEnglish)

	dir := t.TempDir()
	st := store.Store{Dir: dir}
	set := store.DefaultSettings()
	set.Language = LangEnglish
	set.SandboxEngine = true
	_ = st.SaveSettings(set)

	var m *Model
	p := ggui.ProbeBuilder(func() ggui.Widget {
		return ggui.Provide(ggui.ReducedMotionKey, true, Build(m))
	}, ggui.Sz(1200, 840))
	p.Setup(func() {
		m = NewModel(st, dir)
		m.Offline = true
		m.Attach(p)
	})
	defer p.Close()
	p.Frame()

	// 1. Sidebar has no "Image registry" tab or "Recipes" section header.
	for _, n := range p.Semantics().All() {
		if n.Name == "Image registry" || n.Name == "Recipes" {
			t.Errorf("sidebar unexpectedly still contains %q (role=%v):\n%s", n.Name, n.Role, p.Semantics())
		}
	}

	// 2. OpenRegistry navigates to PageImages + ImagesRegistry.
	m.OpenRegistry()
	p.Frame()
	if ggui.Peek(m.Page) != PageImages || ggui.Peek(m.ImageView) != ImagesRegistry {
		t.Fatalf("OpenRegistry landed on page=%q view=%d, want images/%d", ggui.Peek(m.Page), ggui.Peek(m.ImageView), ImagesRegistry)
	}

	// 3. Command Palette lists "Local registry" and no longer "Start the image registry" or "Image registry".
	m.PaletteOpen.Set(true)
	p.Frame()
	names := map[string]bool{}
	for _, n := range p.Semantics().Nodes(ggui.RoleMenuItem) {
		names[n.Name] = true
	}
	if !names["Local registry"] {
		t.Errorf("command palette missing Local registry (have: %v)", names)
	}
	for _, removed := range []string{"Start the image registry", "Image registry"} {
		if names[removed] {
			t.Errorf("command palette still has legacy item %q", removed)
		}
	}
	m.PaletteOpen.Set(false)
	p.Frame()

	// 4. Docker images row menu puts "Load into cluster" before "Push to local registry".
	m.DockerImages.Set(Listing[dockerapi.Image]{
		Loaded: true,
		Items: []dockerapi.Image{
			{ID: "sha256:1111222233334444", RepoTags: []string{"myapp:dev"}, Size: 10 << 20, Created: time.Now().Unix()},
		},
	})
	m.GoImages(ImagesDocker)
	p.Frame()
	p.Tap("More actions for myapp:dev")
	p.Frame()
	var menuOrder []string
	for _, n := range p.Semantics().Nodes(ggui.RoleMenuItem) {
		menuOrder = append(menuOrder, n.Name)
	}
	loadIdx := slices.Index(menuOrder, "Load into cluster")
	pushIdx := slices.Index(menuOrder, "Push to local registry")
	if loadIdx < 0 || pushIdx < 0 || loadIdx >= pushIdx {
		t.Errorf("Docker image menu order = %v, want Load into cluster before Push to local registry", menuOrder)
	}
}
