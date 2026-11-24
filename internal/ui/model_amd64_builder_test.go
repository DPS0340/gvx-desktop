package ui

import (
	"context"
	"net/http"
	"net/http/httptest"
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ironpark/ggui"

	"github.com/ironpark/gvx-desktop-vm/vmhelper"
	"github.com/ironpark/gvx-desktop/internal/dockerapi"
	"github.com/ironpark/gvx-desktop/internal/engine"
	"github.com/ironpark/gvx-desktop/internal/recipes"
	"github.com/ironpark/gvx-desktop/internal/sandbox"
	"github.com/ironpark/gvx-desktop/internal/store"
)

func TestAmd64BuilderSwitchGatingAndRequirementsUI(t *testing.T) {
	prevPlatform := amd64BuilderPlatform
	amd64BuilderPlatform = func() bool { return true }
	defer func() { amd64BuilderPlatform = prevPlatform }()

	dir := t.TempDir()
	st := store.Store{Dir: dir}
	set := store.DefaultSettings()
	set.Language = LangEnglish
	set.SandboxEngine = false
	_ = st.SaveSettings(set)

	var m *Model
	p := ggui.ProbeBuilder(func() ggui.Widget { return ggui.Provide(ggui.ReducedMotionKey, true, Build(m)) }, ggui.Sz(1200, 2200))
	p.Setup(func() {
		m = NewModel(st, dir)
		m.Offline = true
		m.Attach(p)
	})
	defer p.Close()
	p.Frame()

	shows := func(substr string) bool {
		for _, n := range p.Semantics().All() {
			if strings.Contains(n.Name, substr) {
				return true
			}
		}
		return false
	}
	switchDisabled := func() bool {
		n, ok := p.Semantics().Find(ggui.RoleSwitch, "amd64 builds with Rosetta")
		if !ok {
			t.Fatalf("amd64 builds with Rosetta switch not found:\n%s", p.Semantics())
		}
		return n.Disabled
	}

	m.Go(PageLabs)
	m.Amd64Check.Set(Amd64CheckState{
		Checked: true,
		Result: vmhelper.CheckResult{
			AppleSilicon:   true,
			MacOS:          "15.4",
			Virtualization: true,
			Rosetta:        vmhelper.RosettaInstalled,
		},
	})
	p.Frame()

	// 1. Sandbox engine off (compatibility mode): switch is disabled with caption.
	if !switchDisabled() {
		t.Error("switch should be disabled while SandboxEngine is off")
	}
	if !shows("Requires the default sandbox engine. Switch off compatibility mode in Settings → Docker.") {
		t.Errorf("missing compatibility mode caption:\n%s", p.Semantics())
	}
	m.SetAmd64Builds(true)
	if ggui.Peek(m.Settings).Amd64Builds {
		t.Error("SetAmd64Builds(true) should be ignored while SandboxEngine is off")
	}

	// 2. Sandbox engine on, Rosetta missing: switch stays disabled and offers Install Rosetta + CLI fallback.
	m.SetSandboxEngine(true)
	m.Amd64Check.Set(Amd64CheckState{
		Checked: true,
		Result: vmhelper.CheckResult{
			AppleSilicon:   true,
			MacOS:          "15.4",
			Virtualization: true,
			Rosetta:        vmhelper.RosettaNotInstalled,
		},
	})
	p.Frame()
	if !switchDisabled() {
		t.Error("switch should be disabled when Rosetta is not installed")
	}
	if !shows("Install Rosetta") {
		t.Errorf("missing Install Rosetta button:\n%s", p.Semantics())
	}
	if !shows("softwareupdate --install-rosetta") {
		t.Errorf("missing softwareupdate --install-rosetta fallback:\n%s", p.Semantics())
	}
	m.SetAmd64Builds(true)
	if ggui.Peek(m.Settings).Amd64Builds {
		t.Error("SetAmd64Builds(true) should be ignored when Rosetta is missing")
	}

	// 3. Not Apple silicon: switch disabled and feature marked unavailable.
	m.Amd64Check.Set(Amd64CheckState{
		Checked: true,
		Result: vmhelper.CheckResult{
			AppleSilicon: false,
			Rosetta:      vmhelper.RosettaNotSupported,
		},
	})
	p.Frame()
	if !switchDisabled() {
		t.Error("switch should be disabled without Apple silicon")
	}
	if !shows("Not available on this computer") {
		t.Errorf("missing unavailable message:\n%s", p.Semantics())
	}

	// 4. macOS < 13: switch disabled and version requirement shown.
	m.Amd64Check.Set(Amd64CheckState{
		Checked: true,
		Result: vmhelper.CheckResult{
			AppleSilicon:   true,
			MacOS:          "12.6",
			Virtualization: true,
			Rosetta:        vmhelper.RosettaNotSupported,
		},
	})
	p.Frame()
	if !switchDisabled() {
		t.Error("switch should be disabled on macOS < 13")
	}
	if !shows("macOS 13 or later is required") {
		t.Errorf("missing macOS 13 requirement text:\n%s", p.Semantics())
	}

	// 5. All requirements met: switch enabled; turning on shows status, download progress, disk size, buildx node.
	m.Amd64Check.Set(Amd64CheckState{
		Checked: true,
		Result: vmhelper.CheckResult{
			AppleSilicon:   true,
			MacOS:          "15.4",
			Virtualization: true,
			Rosetta:        vmhelper.RosettaInstalled,
		},
	})
	p.Frame()
	if switchDisabled() {
		t.Error("switch should be enabled when SandboxEngine is on and requirements pass")
	}

	m.SetAmd64Builds(true)
	m.Amd64State.Set(engine.Amd64State{
		Enabled:   true,
		Status:    engine.BuilderRunning,
		Assets:    "1",
		DiskBytes: 512_000_000,
	})
	m.BuildxStatus.Set(BuildxStatus{Checked: true, Ready: true, Selected: true, Amd64Node: true})
	p.Frame()
	for _, want := range []string{
		"amd64 builder: running",
		"VM assets: v1 downloaded",
		"Build cache disk: 512 MB",
		"Reset amd64 build cache",
		"docker buildx sends linux/amd64 builds to the amd64 node.",
	} {
		if !shows(want) {
			t.Errorf("Settings missing %q:\n%s", want, p.Semantics())
		}
	}

	// Download progress and error rendering.
	m.Amd64State.Set(engine.Amd64State{
		Enabled:     true,
		Status:      engine.BuilderStarting,
		Downloading: true,
		Done:        23_000_000,
		Total:       50_000_000,
		Err:         "assets not published yet",
	})
	p.Frame()
	for _, want := range []string{
		"amd64 builder: starting…",
		"Downloading the VM, 23 MB of 50 MB",
		"amd64 builder error: assets not published yet",
	} {
		if !shows(want) {
			t.Errorf("Settings missing %q:\n%s", want, p.Semantics())
		}
	}
}

func TestAmd64BuilderToggleWiringAndLifecycle(t *testing.T) {
	dir := t.TempDir()
	st := store.Store{Dir: dir}
	set := store.DefaultSettings()
	set.Language = LangEnglish
	set.SandboxEngine = true
	_ = st.SaveSettings(set)

	var m *Model
	p := ggui.ProbeBuilder(func() ggui.Widget { return ggui.Provide(ggui.ReducedMotionKey, true, Build(m)) }, ggui.Sz(1000, 700))
	p.Setup(func() {
		m = NewModel(st, dir)
		m.Attach(p)
	})
	defer p.Close()
	p.Frame()

	fake := &fakeSandboxEngine{}
	m.sandboxEngine = fake
	var syncMu sync.Mutex
	var syncCalls []bool
	m.syncAmd64NodeHook = func(on bool) {
		syncMu.Lock()
		syncCalls = append(syncCalls, on)
		syncMu.Unlock()
	}
	m.Amd64Check.Set(Amd64CheckState{
		Checked: true,
		Result: vmhelper.CheckResult{
			AppleSilicon:   true,
			MacOS:          "15.4",
			Virtualization: true,
			Rosetta:        vmhelper.RosettaInstalled,
		},
	})

	waitForSync := func(wantLen int) {
		t.Helper()
		deadline := time.Now().Add(3 * time.Second)
		for time.Now().Before(deadline) {
			syncMu.Lock()
			n := len(syncCalls)
			syncMu.Unlock()
			if n >= wantLen {
				return
			}
			p.Frame()
			time.Sleep(10 * time.Millisecond)
		}
		t.Fatalf("timed out waiting for %d syncAmd64Node call(s), got %v", wantLen, syncCalls)
	}

	// Toggle ON: SetAmd64(non-nil) then syncAmd64Node(true).
	m.SetAmd64Builds(true)
	waitForSync(1)

	fake.mu.Lock()
	if len(fake.amd64Configs) != 1 || fake.amd64Configs[0] == nil {
		t.Fatalf("after toggle on: amd64Configs = %+v, want 1 non-nil config", fake.amd64Configs)
	}
	cfg := fake.amd64Configs[0]
	fake.mu.Unlock()
	if cfg.Launcher.Path == "" || !slices.Equal(cfg.Launcher.Args, []string{vmhelper.Arg, vmhelper.CmdServe}) {
		t.Errorf("launcher = %+v, want executable with [%s %s]", cfg.Launcher, vmhelper.Arg, vmhelper.CmdServe)
	}
	if cfg.Assets == nil {
		t.Error("Amd64Config.Assets is nil")
	}
	if saved, _ := st.Settings(); !saved.Amd64Builds {
		t.Error("Amd64Builds was not saved")
	}

	// Reset disk opens confirm dialog and calls ResetAmd64Disk on confirm.
	m.AskResetAmd64Disk()
	p.Frame()
	if !ggui.Peek(m.ConfirmOpen) {
		t.Fatal("AskResetAmd64Disk did not open confirmation dialog")
	}
	m.RunConfirmed()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		fake.mu.Lock()
		n := fake.resetDisks
		fake.mu.Unlock()
		if n >= 1 {
			break
		}
		p.Frame()
		time.Sleep(10 * time.Millisecond)
	}
	fake.mu.Lock()
	if fake.resetDisks != 1 {
		t.Fatalf("resetDisks = %d, want 1", fake.resetDisks)
	}
	fake.mu.Unlock()

	// Turning sandbox engine OFF leaves Settings.Amd64Builds ON and calls syncAmd64Node(false).
	m.SetSandboxEngine(false)
	waitForSync(2)
	if !ggui.Peek(m.Settings).Amd64Builds {
		t.Error("turning SandboxEngine off cleared Settings.Amd64Builds")
	}

	// Turning sandbox engine back ON with Amd64Builds already ON calls SetAmd64(non-nil) and syncAmd64Node(true).
	m.SetSandboxEngine(true)
	waitForSync(3)

	// Toggle Amd64Builds OFF: SetAmd64(nil) then syncAmd64Node(false).
	m.SetAmd64Builds(false)
	waitForSync(4)

	syncMu.Lock()
	gotSync := slices.Clone(syncCalls)
	syncMu.Unlock()
	wantSync := []bool{true, false, true, false}
	if !slices.Equal(gotSync, wantSync) {
		t.Fatalf("syncAmd64Node calls = %v, want %v", gotSync, wantSync)
	}
	fake.mu.Lock()
	lastCfg := fake.amd64Configs[len(fake.amd64Configs)-1]
	fake.mu.Unlock()
	if lastCfg != nil {
		t.Fatalf("last SetAmd64 after toggle off = %+v, want nil", lastCfg)
	}
}

func TestAmd64HelperCheckAndInstallRosetta(t *testing.T) {
	dir := t.TempDir()
	st := store.Store{Dir: dir}
	set := store.DefaultSettings()
	set.Language = LangEnglish
	_ = st.SaveSettings(set)

	var m *Model
	p := ggui.ProbeBuilder(func() ggui.Widget { return ggui.Provide(ggui.ReducedMotionKey, true, Build(m)) }, ggui.Sz(1000, 700))
	p.Setup(func() {
		m = NewModel(st, dir)
		m.Attach(p)
	})
	defer p.Close()
	p.Frame()

	var mu sync.Mutex
	var subcmds []string
	m.runVMHelper = func(_ context.Context, subcmd string) (vmhelper.CheckResult, error) {
		mu.Lock()
		subcmds = append(subcmds, subcmd)
		mu.Unlock()
		if subcmd == vmhelper.CmdInstallRosetta {
			return vmhelper.CheckResult{
				AppleSilicon:   true,
				MacOS:          "15.4",
				Virtualization: true,
				Rosetta:        vmhelper.RosettaInstalled,
			}, nil
		}
		return vmhelper.CheckResult{
			AppleSilicon:   true,
			MacOS:          "15.4",
			Virtualization: true,
			Rosetta:        vmhelper.RosettaNotInstalled,
		}, nil
	}

	m.CheckAmd64()
	deadline := time.Now().Add(3 * time.Second)
	for !ggui.Peek(m.Amd64Check).Checked && time.Now().Before(deadline) {
		p.Frame()
		time.Sleep(10 * time.Millisecond)
	}
	if c := ggui.Peek(m.Amd64Check); !c.Checked || c.OK() || c.Result.Rosetta != vmhelper.RosettaNotInstalled {
		t.Fatalf("after CheckAmd64: %+v", c)
	}

	m.InstallRosetta()
	deadline = time.Now().Add(3 * time.Second)
	for !ggui.Peek(m.Amd64Check).OK() && time.Now().Before(deadline) {
		p.Frame()
		time.Sleep(10 * time.Millisecond)
	}
	if !ggui.Peek(m.Amd64Check).OK() {
		t.Fatalf("after InstallRosetta: %+v, want OK", ggui.Peek(m.Amd64Check))
	}
	mu.Lock()
	got := slices.Clone(subcmds)
	mu.Unlock()
	if !slices.Equal(got, []string{vmhelper.CmdCheck, vmhelper.CmdInstallRosetta}) {
		t.Fatalf("helper subcommands = %v, want [%s %s]", got, vmhelper.CmdCheck, vmhelper.CmdInstallRosetta)
	}
}

func TestForeignArchImageBadgeAndNoRunAction(t *testing.T) {
	foreign := "amd64"
	if runtime.GOARCH == "amd64" {
		foreign = "arm64"
	}

	dir := t.TempDir()
	st := store.Store{Dir: dir}
	set := store.DefaultSettings()
	set.Language = LangEnglish
	set.SandboxEngine = true
	_ = st.SaveSettings(set)

	var m *Model
	p := ggui.ProbeBuilder(func() ggui.Widget { return ggui.Provide(ggui.ReducedMotionKey, true, Build(m)) }, ggui.Sz(1200, 800))
	p.Setup(func() {
		m = NewModel(st, dir)
		m.Offline = true
		m.Attach(p)
	})
	defer p.Close()
	p.Frame()

	m.DockerImages.Set(Listing[dockerapi.Image]{
		Loaded: true,
		Items: []dockerapi.Image{
			{
				ID: "sha256:aaaa111122223333", RepoTags: []string{"native-app:1"}, Size: 10 << 20, Created: time.Now().Unix(),
				Manifests: []dockerapi.ImageManifest{{
					ID: "sha256:aaaa", Available: true, Kind: "image",
					ImageData: &dockerapi.ImageManifestData{Platform: dockerapi.ImagePlatform{Architecture: runtime.GOARCH, OS: "linux"}},
				}},
			},
			{
				ID: "sha256:bbbb111122223333", RepoTags: []string{"foreign-app:1"}, Size: 20 << 20, Created: time.Now().Unix(),
				Manifests: []dockerapi.ImageManifest{{
					ID: "sha256:bbbb", Available: true, Kind: "image",
					ImageData: &dockerapi.ImageManifestData{Platform: dockerapi.ImagePlatform{Architecture: foreign, OS: "linux"}},
				}},
			},
		},
	})
	m.GoImages(ImagesDocker)
	p.Frame()

	// Foreign architecture badge is shown.
	foundBadge := false
	for _, n := range p.Semantics().All() {
		if n.Name == foreign {
			foundBadge = true
			break
		}
	}
	if !foundBadge {
		t.Fatalf("expected %q badge on foreign-arch image row:\n%s", foreign, p.Semantics())
	}

	// Foreign-arch row's More menu does NOT offer "Copy docker run command".
	p.Tap("More actions for foreign-app:1")
	p.Frame()
	if _, ok := p.Semantics().Find(ggui.RoleMenuItem, "Copy docker run command"); ok {
		t.Errorf("foreign-arch image menu unexpectedly offered Copy docker run command:\n%s", p.Semantics())
	}
	if _, ok := p.Semantics().Find(ggui.RoleMenuItem, "Inspect"); !ok {
		t.Errorf("foreign-arch image menu did not open:\n%s", p.Semantics())
	}
}

func TestForeignArchSameTagNotMarkedInUseAndRemovable(t *testing.T) {
	foreign := "amd64"
	if runtime.GOARCH == "amd64" {
		foreign = "arm64"
	}

	dir := t.TempDir()
	st := store.Store{Dir: dir}
	set := store.DefaultSettings()
	set.Language = LangEnglish
	set.SandboxEngine = true
	_ = st.SaveSettings(set)
	sb := sandbox.Spec{ID: "sb1", Name: "my-app", Image: "app:latest"}
	_ = st.SaveSandboxes([]sandbox.Spec{sb})

	var m *Model
	p := ggui.ProbeBuilder(func() ggui.Widget { return ggui.Provide(ggui.ReducedMotionKey, true, Build(m)) }, ggui.Sz(1200, 800))
	p.Setup(func() {
		m = NewModel(st, dir)
		m.Offline = true
		m.Attach(p)
	})
	defer p.Close()
	p.Frame()

	nativeRow := DockerImageRow{
		ID: "sha256:1111222233334444", ShortID: "111122223333",
		Repository: "app", Tag: "latest", Arch: runtime.GOARCH, Size: 50 << 20,
	}
	foreignRow := DockerImageRow{
		ID: "sha256:9999888877776666", ShortID: "999988887777",
		Repository: "app", Tag: "latest", Arch: foreign, Size: 60 << 20,
	}

	ctrs := []dockerapi.Container{
		{ID: "c1", Names: []string{"/web"}, Image: "app:latest", ImageID: nativeRow.ID},
		{ID: "c2", Names: []string{"/worker"}, Image: "app"},
	}

	// Native row matches both containers and the user sandbox; foreign row matches none.
	if got := dockerImageUsers(ctrs, nativeRow); !slices.Equal(got, []string{"web", "worker"}) {
		t.Errorf("dockerImageUsers(native) = %v, want [web worker]", got)
	}
	if got := dockerImageUsers(ctrs, foreignRow); len(got) != 0 {
		t.Errorf("dockerImageUsers(foreign) = %v, want empty", got)
	}
	if got := sandboxImageUsers(ggui.Peek(m.Rows), nativeRow); !slices.Equal(got, []string{"my-app"}) {
		t.Errorf("sandboxImageUsers(native) = %v, want [my-app]", got)
	}
	if got := sandboxImageUsers(ggui.Peek(m.Rows), foreignRow); len(got) != 0 {
		t.Errorf("sandboxImageUsers(foreign) = %v, want empty", got)
	}
	if got := dockerImageAllUsers(ctrs, ggui.Peek(m.Rows), true, foreignRow); len(got) != 0 {
		t.Errorf("dockerImageAllUsers(foreign) = %v, want empty", got)
	}

	// AskRemoveDockerImage refuses the native row (used by sandbox) but allows removing the foreign row.
	m.AskRemoveDockerImage(nativeRow)
	p.Frame()
	if ggui.Peek(m.ConfirmOpen) {
		t.Fatal("AskRemoveDockerImage opened confirm dialog for native row used by sandbox")
	}

	m.AskRemoveDockerImage(foreignRow)
	p.Frame()
	if !ggui.Peek(m.ConfirmOpen) {
		t.Fatal("AskRemoveDockerImage did not open confirm dialog for foreign-arch row sharing tag with native sandbox image")
	}
}

func TestForeignArchRemoveAndPushUsePlatform(t *testing.T) {
	foreign := "amd64"
	if runtime.GOARCH == "amd64" {
		foreign = "arm64"
	}
	wantPlatform := "linux/" + foreign

	var mu sync.Mutex
	var calls []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		calls = append(calls, r.Method+" "+r.URL.RequestURI())
		mu.Unlock()
		switch {
		case r.Method == http.MethodDelete && strings.HasPrefix(r.URL.Path, "/images/"):
			_, _ = w.Write([]byte(`[{"Untagged":"app:latest"}]`))
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/tag"):
			w.WriteHeader(http.StatusCreated)
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/push"):
			_, _ = w.Write([]byte(`{"status":"Pushed"}` + "\n"))
		case r.Method == http.MethodGet && r.URL.Path == "/images/json":
			_, _ = w.Write([]byte(`[]`))
		case r.Method == http.MethodGet && r.URL.Path == "/version":
			_, _ = w.Write([]byte(`{"Version":"29.0"}`))
		case r.Method == http.MethodGet && r.URL.Path == "/containers/json":
			_, _ = w.Write([]byte(`[]`))
		default:
			w.WriteHeader(http.StatusOK)
		}
	}))
	defer srv.Close()

	dir := t.TempDir()
	st := store.Store{Dir: dir}
	set := store.DefaultSettings()
	set.Language = LangEnglish
	set.SandboxEngine = true
	_ = st.SaveSettings(set)
	reg := recipes.ImageRegistry()
	reg.ID = "reg1"
	_ = st.SaveSandboxes([]sandbox.Spec{reg})

	var m *Model
	p := ggui.ProbeBuilder(func() ggui.Widget { return ggui.Provide(ggui.ReducedMotionKey, true, Build(m)) }, ggui.Sz(1000, 700))
	p.Setup(func() {
		m = NewModel(st, dir)
		m.dockerBackend = "tcp://" + strings.TrimPrefix(srv.URL, "http://")
		m.Attach(p)
	})
	defer p.Close()
	p.Frame()

	m.States.Set(map[string]sandbox.State{reg.ID: {Status: sandbox.Running}})
	p.Frame()

	waitForCall := func(match func(string) bool, desc string) {
		t.Helper()
		deadline := time.Now().Add(3 * time.Second)
		for time.Now().Before(deadline) {
			mu.Lock()
			for _, c := range calls {
				if match(c) {
					mu.Unlock()
					return
				}
			}
			mu.Unlock()
			p.Frame()
			time.Sleep(10 * time.Millisecond)
		}
		mu.Lock()
		got := slices.Clone(calls)
		mu.Unlock()
		t.Fatalf("timed out waiting for %s; calls=%v", desc, got)
	}

	nativeRow := DockerImageRow{
		ID: "sha256:1111222233334444", ShortID: "111122223333",
		Repository: "app", Tag: "latest", Arch: runtime.GOARCH, Size: 50 << 20,
	}
	foreignRow := DockerImageRow{
		ID: "sha256:9999888877776666", ShortID: "999988887777",
		Repository: "app", Tag: "latest", Arch: foreign, Size: 60 << 20,
	}

	// Remove foreign row sends platform=linux/<foreign>.
	m.AskRemoveDockerImage(foreignRow)
	p.Frame()
	m.RunConfirmed()
	wantDel := "DELETE /images/app:latest?force=1&platform=" + strings.ReplaceAll(wantPlatform, "/", "%2F")
	waitForCall(func(c string) bool { return c == wantDel }, wantDel)

	// Remove native row omits platform.
	m.AskRemoveDockerImage(nativeRow)
	p.Frame()
	m.RunConfirmed()
	wantNativeDel := "DELETE /images/app:latest?force=1"
	waitForCall(func(c string) bool { return c == wantNativeDel }, wantNativeDel)

	// PushToRegistry on foreign row tags with platform=linux/<foreign>.
	m.PushToRegistry(foreignRow)
	wantTag := "POST /images/app:latest/tag?platform=" + strings.ReplaceAll(wantPlatform, "/", "%2F") + "&"
	waitForCall(func(c string) bool { return strings.HasPrefix(c, wantTag) }, wantTag)
}
