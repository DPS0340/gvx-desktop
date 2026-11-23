package ui

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/ironpark/ggui"

	"github.com/ironpark/gvx-desktop/internal/dockerapi"
	"github.com/ironpark/gvx-desktop/internal/recipes"
	"github.com/ironpark/gvx-desktop/internal/rungv"
	"github.com/ironpark/gvx-desktop/internal/sandbox"
	"github.com/ironpark/gvx-desktop/internal/store"
)

func TestParseAndFormatBandwidthBPS(t *testing.T) {
	for _, tc := range []struct {
		in      string
		want    uint64
		wantFmt string
	}{
		{"", 0, ""},
		{"   ", 0, ""},
		{"0", 0, ""},
		{"0m", 0, ""},
		{"500k", 500_000, "500k"},
		{"100m", 100_000_000, "100m"},
		{"100Mbps", 100_000_000, "100m"},
		{"100 Mbit/s", 100_000_000, "100m"},
		{"1g", 1_000_000_000, "1g"},
		{"1.5g", 1_500_000_000, "1.5g"},
		{"2.5m", 2_500_000, "2.5m"},
		{"12345", 12_345, "12345"},
		{"1t", 1_000_000_000_000, "1000g"},
	} {
		got, err := parseBandwidthBPS(tc.in)
		if err != nil {
			t.Errorf("parseBandwidthBPS(%q) unexpected error: %v", tc.in, err)
			continue
		}
		if got != tc.want {
			t.Errorf("parseBandwidthBPS(%q) = %d, want %d", tc.in, got, tc.want)
		}
		formatted := formatBandwidthBPS(got)
		if formatted != tc.wantFmt {
			t.Errorf("formatBandwidthBPS(%d) = %q, want %q", got, formatted, tc.wantFmt)
		}
		reparsed, err := parseBandwidthBPS(formatted)
		if err != nil || reparsed != got {
			t.Errorf("round-trip %q -> %q -> (%d, %v), want %d", tc.in, formatted, reparsed, err, got)
		}
	}

	for _, bad := range []string{"-10m", "abc", "10x", "1.2.3m", ".", "99999999999999999999g"} {
		if got, err := parseBandwidthBPS(bad); err == nil {
			t.Errorf("parseBandwidthBPS(%q) = %d, want error", bad, got)
		}
	}
}

func TestDraftRoundTripV021(t *testing.T) {
	spec := sandbox.Spec{
		ID:               "v21",
		Name:             "app",
		Image:            "nginx:alpine",
		Ports:            []string{"127.0.0.1:8080:80"},
		UnixForwards:     []string{"/var/run/app.sock=~/.gvx/app.sock,uid=1000,mode=0600"},
		NetTXBPS:         100_000_000,
		NetRXBPS:         1_500_000_000,
		NATPolicyFile:    "~/nat-policy.yaml",
		DynamicPortAddrs: []string{"127.0.0.1"},
		AutoPublishAddr:  "127.0.0.1",
	}
	d := DraftFrom(spec)
	if d.NetTX != "100m" || d.NetRX != "1.5g" || d.NATPolicyFile != "~/nat-policy.yaml" || d.UnixForwards != spec.UnixForwards[0] {
		t.Fatalf("DraftFrom unexpected fields: %+v", d)
	}
	got, err := d.Spec()
	if err != nil {
		t.Fatalf("Draft.Spec() error: %v", err)
	}
	if !got.Equal(spec) {
		t.Errorf("round-trip mismatch\n got  %+v\n want %+v", got, spec)
	}

	dBad := d
	dBad.NetTX = "bogus"
	if _, err := dBad.Spec(); err == nil {
		t.Error("expected error for invalid NetTX")
	}
	dBad = d
	dBad.NetRX = "-5m"
	if _, err := dBad.Spec(); err == nil {
		t.Error("expected error for invalid NetRX")
	}
}

func TestWrapCommandV021BoolFlags(t *testing.T) {
	got := wrapCommand([]string{
		"run",
		"--dynamic-ports",
		"--allow-bind-attach",
		"-keep-blobs",
		"--keep-blobs",
		"nginx:alpine",
	})
	for _, want := range []string{
		"\n  --dynamic-ports \\",
		"\n  --allow-bind-attach \\",
		"\n  -keep-blobs \\",
		"\n  --keep-blobs \\",
		"\n  nginx:alpine",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("wrapCommand missing %q in:\n%s", want, got)
		}
	}
}

func TestEditorV021LiveActionAndSaveDraft(t *testing.T) {
	SetLanguage(LangEnglish)
	base := sandbox.Spec{
		ID:     "w1",
		Name:   "web",
		Image:  "nginx:alpine",
		Memory: "512m",
		CPUs:   "1",
		Ports:  []string{"127.0.0.1:8080:80"},
		Mounts: []sandbox.Mount{{Type: sandbox.MountBind, Source: "/tmp/old", Target: "/old"}},
	}
	v21State := sandbox.State{
		Status:       sandbox.Running,
		Control:      true,
		DynamicPorts: true,
		BindAttach:   true,
		NetQoS:       true,
		NATPolicy:    true,
	}

	// v0.21 caption mentions live bandwidth, NAT policy, loopback ports and mounts.
	act := editorActionOf(v21State, base, true, base)
	if got := editorCaption(act); !strings.Contains(got, "without a restart") || !strings.Contains(got, "NAT policy") {
		t.Errorf("v0.21 editorCaption = %q", got)
	}
	if got := editorPrimaryLabel(act); got != "Save" {
		t.Errorf("v0.21 zero diff label = %q, want Save", got)
	}

	// Live edits for ports, bind mounts, bandwidth, and NAT policy all produce "Save and apply".
	edited := base.Clone()
	edited.Ports = []string{"127.0.0.1:8080:80", "127.0.0.1:8443:443"}
	edited.Mounts = []sandbox.Mount{{Type: sandbox.MountBind, Source: "/tmp/new", Target: "/new"}}
	edited.NetTXBPS = 100_000_000
	edited.NetRXBPS = 500_000_000
	edited.NATPolicyFile = "~/nat-policy.yaml"
	act = editorActionOf(v21State, base, true, edited)
	if got := editorPrimaryLabel(act); got != "Save and apply" {
		t.Errorf("v0.21 live diff label = %q, want Save and apply", got)
	}

	// End-to-end SaveDraft with applyLiveFn hook.
	dir := t.TempDir()
	st := store.Store{Dir: dir}
	if err := st.SaveSandboxes([]sandbox.Spec{base}); err != nil {
		t.Fatal(err)
	}
	set := store.DefaultSettings()
	set.Language = LangEnglish
	_ = st.SaveSettings(set)

	var m *Model
	p := ggui.ProbeBuilder(func() ggui.Widget { return ggui.Provide(ggui.ReducedMotionKey, true, Build(m)) }, ggui.Sz(1280, 1200))
	p.Setup(func() {
		m = NewModel(st, dir)
		m.Offline = true
		m.Attach(p)
		m.States.Set(map[string]sandbox.State{base.ID: v21State})
	})
	defer p.Close()
	p.Frame()

	var mu sync.Mutex
	var diffs []sandbox.LiveDiff
	m.applyLiveFn = func(_ context.Context, id string, diff sandbox.LiveDiff) error {
		mu.Lock()
		defer mu.Unlock()
		diffs = append(diffs, diff)
		return nil
	}

	m.EditSandbox(base.ID)
	p.Frame()

	shows := func(substr string) bool {
		for _, n := range p.Semantics().All() {
			if strings.Contains(n.Name, substr) {
				return true
			}
		}
		return false
	}
	for _, want := range []string{"Unix socket forwards", "Upload limit (TX)", "Download limit (RX)", "NAT firewall policy"} {
		if !shows(want) {
			t.Errorf("editor NAT view missing %q", want)
		}
	}

	// Switching network to "none" hides NAT-only bandwidth & policy controls.
	m.Draft.Update(func(d Draft) Draft { d.Network = "none"; return d })
	p.Frame()
	if shows("Upload limit (TX)") || shows("NAT firewall policy") {
		t.Errorf("editor with network=none should hide NAT bandwidth/policy fields")
	}

	// Switch back to NAT and apply v0.21 live changes.
	m.Draft.Update(func(d Draft) Draft {
		d.Network = "nat"
		d.Ports = "127.0.0.1:8080:80\n127.0.0.1:8443:443"
		d.Binds = "/tmp/new:/new"
		d.NetTX = "100m"
		d.NetRX = "500m"
		d.NATPolicyFile = "~/nat-policy.yaml"
		return d
	})
	p.Frame()
	if !shows("Save and apply") {
		t.Errorf("expected Save and apply button for v0.21 live diff")
	}

	if !m.SaveDraft(true) {
		t.Fatal("SaveDraft(true) failed")
	}
	settle(t, p, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(diffs) == 1
	}, "v0.21 SaveDraft applyLiveFn")

	mu.Lock()
	d := diffs[0]
	mu.Unlock()
	if !slices.Equal(d.AddPorts, []string{"127.0.0.1:8443:443"}) || len(d.RemovePorts) != 0 {
		t.Errorf("ports diff = (+%v -%v)", d.AddPorts, d.RemovePorts)
	}
	if len(d.AddMounts) != 1 || d.AddMounts[0].Target != "/new" || !slices.Equal(d.RemoveMounts, []string{"/old"}) {
		t.Errorf("mounts diff = (+%+v -%v)", d.AddMounts, d.RemoveMounts)
	}
	if d.Update.NetTXBPS == nil || *d.Update.NetTXBPS != 100_000_000 || d.Update.NetRXBPS == nil || *d.Update.NetRXBPS != 500_000_000 {
		t.Errorf("bandwidth diff = (tx=%v rx=%v)", d.Update.NetTXBPS, d.Update.NetRXBPS)
	}
	if d.NATPolicyFile != "~/nat-policy.yaml" {
		t.Errorf("NATPolicyFile diff = %q", d.NATPolicyFile)
	}
}

func TestSetSharedFoldersLiveAndFallback(t *testing.T) {
	SetLanguage(LangEnglish)
	dir := t.TempDir()
	shareA := filepath.Join(dir, "shareA")
	shareB := filepath.Join(dir, "shareB")
	if err := os.MkdirAll(shareA, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(shareB, 0o755); err != nil {
		t.Fatal(err)
	}
	guestA, err := recipes.GuestPath(shareA)
	if err != nil {
		t.Fatalf("GuestPath(shareA): %v", err)
	}
	guestB, err := recipes.GuestPath(shareB)
	if err != nil {
		t.Fatalf("GuestPath(shareB): %v", err)
	}

	st := store.Store{Dir: dir}
	dockerSpec := recipes.WithSharedFolders(recipes.DockerEngine(), []string{shareA})
	if err := st.SaveSandboxes([]sandbox.Spec{dockerSpec}); err != nil {
		t.Fatal(err)
	}
	set := store.DefaultSettings()
	set.Language = LangEnglish
	set.DockerSharedFolders = []string{shareA}
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

	var mu sync.Mutex
	var diffs []sandbox.LiveDiff
	var liveErr error
	m.applyLiveFn = func(_ context.Context, id string, diff sandbox.LiveDiff) error {
		mu.Lock()
		defer mu.Unlock()
		diffs = append(diffs, diff)
		return liveErr
	}

	// 1. Running with BindAttach == true -> applies live mount diff without restart.
	m.States.Set(map[string]sandbox.State{
		dockerSpec.ID: {Status: sandbox.Running, Control: true, BindAttach: true},
	})
	m.SetSharedFolders([]string{shareA, shareB})
	settle(t, p, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(diffs) == 1
	}, "live shared folder add")
	mu.Lock()
	addDiff := diffs[0]
	mu.Unlock()
	if len(addDiff.AddMounts) != 1 || addDiff.AddMounts[0].Target != guestB || len(addDiff.RemoveMounts) != 0 {
		t.Errorf("add shared folder diff = (+%+v -%v), want +%s", addDiff.AddMounts, addDiff.RemoveMounts, guestB)
	}

	// 2. Removing shareA with BindAttach == true -> detaches guestA live.
	m.States.Set(map[string]sandbox.State{
		dockerSpec.ID: {Status: sandbox.Running, Control: true, BindAttach: true},
	})
	m.SetSharedFolders([]string{shareB})
	settle(t, p, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(diffs) == 2
	}, "live shared folder remove")
	mu.Lock()
	rmDiff := diffs[1]
	mu.Unlock()
	if len(rmDiff.AddMounts) != 0 || !slices.Equal(rmDiff.RemoveMounts, []string{guestA}) {
		t.Errorf("remove shared folder diff = (+%+v -%v), want -%s", rmDiff.AddMounts, rmDiff.RemoveMounts, guestA)
	}

	// 3. Error during live apply keeps saved folders and does not panic.
	m.States.Set(map[string]sandbox.State{
		dockerSpec.ID: {Status: sandbox.Running, Control: true, BindAttach: true},
	})
	mu.Lock()
	liveErr = errors.New("mount busy")
	mu.Unlock()
	m.SetSharedFolders([]string{shareA, shareB})
	settle(t, p, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(diffs) == 3
	}, "live shared folder error")
	if got := m.SharedFolders(); !slices.Equal(got, []string{shareA, shareB}) {
		t.Errorf("shared folders should stay saved on error: %v", got)
	}

	// 4. Running with BindAttach == false && LiveMounts == true (Windows rpcfs mount serve)
	//    -> still applies live mount diff without restart!
	mu.Lock()
	liveErr = nil
	mu.Unlock()
	m.States.Set(map[string]sandbox.State{
		dockerSpec.ID: {Status: sandbox.Running, Control: true, BindAttach: false, LiveMounts: true},
	})
	m.SetSharedFolders([]string{shareA})
	settle(t, p, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(diffs) == 4
	}, "live shared folder remove via LiveMounts")
	mu.Lock()
	winDiff := diffs[3]
	mu.Unlock()
	if len(winDiff.AddMounts) != 0 || !slices.Equal(winDiff.RemoveMounts, []string{guestB}) {
		t.Errorf("LiveMounts-only diff = (+%+v -%v), want -%s", winDiff.AddMounts, winDiff.RemoveMounts, guestB)
	}

	// 5. Running with BindAttach == false && LiveMounts == false -> does NOT call applyLiveFn, shows restart toast.
	m.States.Set(map[string]sandbox.State{
		dockerSpec.ID: {Status: sandbox.Running, Control: true, BindAttach: false, LiveMounts: false},
	})
	m.SetSharedFolders([]string{shareA, shareB})
	p.Frame()
	mu.Lock()
	gotCalls := len(diffs)
	mu.Unlock()
	if gotCalls != 4 {
		t.Errorf("BindAttach=false && LiveMounts=false should not call applyLiveFn, got %d calls", gotCalls)
	}
	foundToast := false
	for _, n := range p.Semantics().All() {
		if strings.Contains(n.Name, "Restart the Docker Engine to share the change") {
			foundToast = true
			break
		}
	}
	if !foundToast {
		t.Errorf("BindAttach=false && LiveMounts=false expected restart toast")
	}
}

func TestLabsAndSettingsV021Notes(t *testing.T) {
	SetLanguage(LangEnglish)
	dir := t.TempDir()
	st := store.Store{Dir: dir}
	set := store.DefaultSettings()
	set.Language = LangEnglish
	_ = st.SaveSettings(set)

	var m *Model
	p := ggui.ProbeBuilder(func() ggui.Widget { return ggui.Provide(ggui.ReducedMotionKey, true, Build(m)) }, ggui.Sz(1280, 2200))
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

	// Docker page in default sandbox engine mode: v0.20 vs v0.21 capabilities note.
	const v21SandboxNote = "Supports docker run, exec, cp, pause, top, update, stats (with BLOCK I/O), diff, export, commit, build (BuildKit and Compose), push, offline save, networks, and host Unix socket bind mounts (including /var/run/docker.sock)."
	m.RungvFeatures.Set(rungv.Features{Control: true})
	m.Go(PageDocker)
	p.Frame()
	if shows(v21SandboxNote) {
		t.Error("v0.20 features should not show v0.21 sandbox engine capabilities note")
	}
	m.RungvFeatures.Set(rungv.Features{Control: true, KeepBlobs: true})
	p.Frame()
	if !shows(v21SandboxNote) {
		t.Errorf("v0.21 features missing Docker sandbox engine capabilities note:\n%s", p.Semantics())
	}

	// Docker page in compatibility mode (#235): BLOCK I/O note reacts to KeepBlobs.
	m.SetSandboxEngine(false)
	m.RungvFeatures.Set(rungv.Features{Control: true})
	m.Go(PageDocker)
	p.Frame()
	if !shows("BLOCK I/O reads 0B because the guest has no io controller.") {
		t.Errorf("v0.20 Docker compatibility page missing legacy BLOCK I/O note")
	}
	m.RungvFeatures.Set(rungv.Features{Control: true, KeepBlobs: true})
	p.Frame()
	if !shows("docker run -m and --cpus are enforced, and docker stats reports per-container BLOCK I/O.") {
		t.Errorf("v0.21 Docker compatibility page missing updated BLOCK I/O note")
	}
	if shows("BLOCK I/O reads 0B") {
		t.Errorf("v0.21 Docker compatibility page still shows outdated BLOCK I/O 0B note")
	}

	// Settings page (#231/#232/#233/#234): shared folders caption reacts to BindAttach || KeepBlobs,
	// and Docker settings expose LAN port publishing and SSH agent forwarding switches.
	m.RungvFeatures.Set(rungv.Features{Control: true})
	m.Go(PageSettings)
	p.Frame()
	if !shows("The engine sees a change when it is restarted.") {
		t.Errorf("v0.20 Settings missing restart caption")
	}
	if !shows("Publish container ports on local network (0.0.0.0)") {
		t.Errorf("Settings missing LAN port publishing switch")
	}
	if !shows("Forward host SSH agent") {
		t.Errorf("Settings missing host SSH agent switch")
	}
	m.RungvFeatures.Set(rungv.Features{Control: true, KeepBlobs: true})
	p.Frame()
	if !shows("Changes apply live to a running engine without restarting.") {
		t.Errorf("v0.21 KeepBlobs Settings missing live shared folders caption")
	}
	m.RungvFeatures.Set(rungv.Features{Control: true, BindAttach: true})
	p.Frame()
	if !shows("Changes apply live to a running engine without restarting.") {
		t.Errorf("v0.21 BindAttach Settings missing live shared folders caption")
	}
}

func TestUnsharedBindErrorAndContainerCopyLiveMounts(t *testing.T) {
	SetLanguage(LangEnglish)
	const p = "/Volumes/work/data"

	legacyErr := unsharedBindError(p, false)
	if legacyErr.Error() != "/Volumes/work/data is not in a folder shared with the Docker Engine. Share it in GVX Desktop → Settings → Docker, restart the engine, and run this again." {
		t.Errorf("legacy unsharedBindError = %q", legacyErr)
	}
	liveErr := unsharedBindError(p, true)
	if liveErr.Error() != "/Volumes/work/data is not in a folder shared with the Docker Engine. Share it in GVX Desktop → Settings → Docker and run this again." {
		t.Errorf("live unsharedBindError = %q", liveErr)
	}

	legacyWhy := unsharedWhy(p, false)
	if legacyWhy != "/Volumes/work/data is not in a folder shared with the Docker Engine, so the container sees an empty folder there. Share it in Settings → Docker, restart the engine, and create the container again." {
		t.Errorf("legacy unsharedWhy = %q", legacyWhy)
	}
	liveWhy := unsharedWhy(p, true)
	if liveWhy != "/Volumes/work/data is not in a folder shared with the Docker Engine, so the container sees an empty folder there. Share it in Settings → Docker and create the container again." {
		t.Errorf("live unsharedWhy = %q", liveWhy)
	}

	// checkContainerCreate uses live-mount copy when Docker Engine state has LiveMounts or BindAttach.
	dir := t.TempDir()
	st := store.Store{Dir: dir}
	set := store.DefaultSettings()
	set.Language = LangEnglish
	set.SandboxEngine = false
	_ = st.SaveSettings(set)
	dockerSpec := recipes.WithSharedFolders(recipes.DockerEngine(), []string{"/Users/me"})
	_ = st.SaveSandboxes([]sandbox.Spec{dockerSpec})

	var m *Model
	probe := ggui.ProbeBuilder(func() ggui.Widget { return ggui.Provide(ggui.ReducedMotionKey, true, Build(m)) }, ggui.Sz(1200, 1200))
	probe.Setup(func() {
		m = NewModel(st, dir)
		m.Offline = true
		m.Attach(probe)
	})
	defer probe.Close()
	probe.Frame()

	body := []byte(`{"HostConfig":{"Binds":["/Volumes/work/data:/data"]}}`)
	m.States.Set(map[string]sandbox.State{
		dockerSpec.ID: {Status: sandbox.Running, Control: true, BindAttach: false, LiveMounts: false},
	})
	probe.Frame()
	if err := m.checkContainerCreate(nil, body); err == nil || !strings.Contains(err.Error(), "restart the engine") {
		t.Errorf("non-live checkContainerCreate = %v, want restart message", err)
	}

	m.States.Set(map[string]sandbox.State{
		dockerSpec.ID: {Status: sandbox.Running, Control: true, BindAttach: false, LiveMounts: true},
	})
	probe.Frame()
	if err := m.checkContainerCreate(nil, body); err == nil || strings.Contains(err.Error(), "restart the engine") {
		t.Errorf("LiveMounts=true checkContainerCreate = %v, should omit restart", err)
	}

	// Containers page Connect tab unshared bind caption reacts to LiveMounts/KeepBlobs.
	m.DockerContainers.Set(Listing[dockerapi.Container]{
		Loaded: true,
		Items: []dockerapi.Container{{
			ID:     "c1",
			Names:  []string{"/web"},
			Image:  "nginx:alpine",
			State:  "running",
			Mounts: []dockerapi.Mount{{Type: "bind", Source: p, Destination: "/data", RW: true}},
		}},
	})
	m.States.Set(map[string]sandbox.State{
		dockerSpec.ID: {Status: sandbox.Running, Control: true, BindAttach: false, LiveMounts: false},
	})
	m.RungvFeatures.Set(rungv.Features{Control: true})
	m.Go(PageContainers)
	m.ContainerDetail.Set(1)
	probe.Frame()
	shows := func(substr string) bool {
		for _, n := range probe.Semantics().All() {
			if strings.Contains(n.Name, substr) {
				return true
			}
		}
		return false
	}
	if !shows("Share it in Settings → Docker, restart the engine, and create the container again.") {
		t.Errorf("Containers page v0.20 missing legacy unshared caption")
	}
	m.States.Set(map[string]sandbox.State{
		dockerSpec.ID: {Status: sandbox.Running, Control: true, BindAttach: false, LiveMounts: true},
	})
	probe.Frame()
	if !shows("Share it in Settings → Docker and create the container again.") {
		t.Errorf("Containers page LiveMounts=true missing live unshared caption")
	}
}

func TestDockerExposeLANAndSSHAgentSettingsAndWiring(t *testing.T) {
	SetLanguage(LangEnglish)
	dir := t.TempDir()
	st := store.Store{Dir: dir}
	set := store.DefaultSettings()
	set.Language = LangEnglish
	_ = st.SaveSettings(set)
	dockerSpec := recipes.DockerEngine()
	_ = st.SaveSandboxes([]sandbox.Spec{dockerSpec})

	var m *Model
	p := ggui.ProbeBuilder(func() ggui.Widget { return ggui.Provide(ggui.ReducedMotionKey, true, Build(m)) }, ggui.Sz(1200, 800))
	p.Setup(func() {
		m = NewModel(st, dir)
		m.Offline = true
		m.Attach(p)
	})
	defer p.Close()
	p.Frame()

	// Defaults are opt-in (false).
	if ggui.Peek(m.DockerExposeLAN) || m.dockerExposeLAN.Load() {
		t.Fatal("DockerExposeLAN should default to false")
	}
	if ggui.Peek(m.DockerSSHAgent) || m.dockerSSHAgent.Load() || m.sandboxSSHAuthSock() != "" {
		t.Fatal("DockerSSHAgent should default to false")
	}

	// Enable LAN publishing while Docker Engine is running -> persists, updates spec, toasts restart.
	m.States.Set(map[string]sandbox.State{dockerSpec.ID: {Status: sandbox.Running}})
	p.Frame()
	m.SetDockerExposeLAN(true)
	p.Frame()

	if !ggui.Peek(m.DockerExposeLAN) || !m.dockerExposeLAN.Load() {
		t.Fatal("DockerExposeLAN was not enabled")
	}
	savedSet, err := st.Settings()
	if err != nil || !savedSet.DockerExposeLAN {
		t.Fatalf("persisted Settings.DockerExposeLAN = %v (err=%v)", savedSet.DockerExposeLAN, err)
	}
	savedSpecs, err := st.Sandboxes()
	if err != nil || len(savedSpecs) != 1 {
		t.Fatalf("saved sandboxes: %v (%v)", savedSpecs, err)
	}
	if savedSpecs[0].AutoPublishAddr != "0.0.0.0" || !slices.Equal(savedSpecs[0].DynamicPortAddrs, []string{"0.0.0.0"}) {
		t.Errorf("Docker spec after LAN enable = (addr=%q, dyn=%v)", savedSpecs[0].AutoPublishAddr, savedSpecs[0].DynamicPortAddrs)
	}

	// Disable LAN publishing -> clears AutoPublishAddr and DynamicPortAddrs.
	m.SetDockerExposeLAN(false)
	p.Frame()
	if ggui.Peek(m.DockerExposeLAN) || m.dockerExposeLAN.Load() {
		t.Fatal("DockerExposeLAN was not disabled")
	}
	savedSpecs, _ = st.Sandboxes()
	if savedSpecs[0].AutoPublishAddr != "" || len(savedSpecs[0].DynamicPortAddrs) != 0 {
		t.Errorf("Docker spec after LAN disable = (addr=%q, dyn=%v)", savedSpecs[0].AutoPublishAddr, savedSpecs[0].DynamicPortAddrs)
	}

	// SSH agent forwarding with SSH_AUTH_SOCK empty -> persists setting, warns, does not add empty forward.
	t.Setenv("SSH_AUTH_SOCK", "")
	m.SetDockerSSHAgent(true)
	p.Frame()
	if !ggui.Peek(m.DockerSSHAgent) || !m.dockerSSHAgent.Load() {
		t.Fatal("DockerSSHAgent was not enabled")
	}
	if m.sandboxSSHAuthSock() != "" {
		t.Errorf("sandboxSSHAuthSock with empty env = %q, want empty", m.sandboxSSHAuthSock())
	}
	savedSpecs, _ = st.Sandboxes()
	if recipes.HasSSHAgentForward(savedSpecs[0]) {
		t.Errorf("expected no SSH forward when SSH_AUTH_SOCK is empty: %v", savedSpecs[0].UnixForwards)
	}
	foundWarn := false
	for _, n := range p.Semantics().All() {
		if strings.Contains(n.Name, "SSH_AUTH_SOCK is not set") {
			foundWarn = true
			break
		}
	}
	if !foundWarn {
		t.Errorf("expected SSH_AUTH_SOCK warning toast")
	}

	// Set SSH_AUTH_SOCK and enable -> adds forward and exposes via engine.Config SSHAuthSock callback.
	sock1 := "/tmp/agent-1.sock"
	t.Setenv("SSH_AUTH_SOCK", sock1)
	m.SetDockerSSHAgent(true)
	p.Frame()
	if got := m.sandboxSSHAuthSock(); got != sock1 {
		t.Errorf("sandboxSSHAuthSock() = %q, want %q", got, sock1)
	}
	savedSpecs, _ = st.Sandboxes()
	wantFwd := recipes.SSHAuthSocketGuestPath + "=" + sock1
	if !slices.Equal(savedSpecs[0].UnixForwards, []string{wantFwd}) {
		t.Errorf("UnixForwards = %v, want [%s]", savedSpecs[0].UnixForwards, wantFwd)
	}

	// Simulate host reboot rotating SSH_AUTH_SOCK -> syncDockerRecipeSpec refreshes the spec before start.
	sock2 := "/tmp/agent-2.sock"
	t.Setenv("SSH_AUTH_SOCK", sock2)
	refreshed, changed := m.syncDockerRecipeSpec(savedSpecs[0])
	if !changed {
		t.Fatal("expected syncDockerRecipeSpec to detect rotated SSH_AUTH_SOCK")
	}
	wantFwd2 := recipes.SSHAuthSocketGuestPath + "=" + sock2
	if !slices.Equal(refreshed.UnixForwards, []string{wantFwd2}) {
		t.Errorf("refreshed UnixForwards = %v, want [%s]", refreshed.UnixForwards, wantFwd2)
	}

	// Turn SSH agent off -> removes forward and engine callback returns "".
	m.SetDockerSSHAgent(false)
	p.Frame()
	if got := m.sandboxSSHAuthSock(); got != "" {
		t.Errorf("sandboxSSHAuthSock() when off = %q, want empty", got)
	}
	savedSpecs, _ = st.Sandboxes()
	if recipes.HasSSHAgentForward(savedSpecs[0]) {
		t.Errorf("UnixForwards after disable = %v, want none", savedSpecs[0].UnixForwards)
	}

	// CreateFromRecipe applies both DockerExposeLAN and DockerSSHAgent on new Docker Engine specs.
	m.saveSpecs(nil)
	t.Setenv("SSH_AUTH_SOCK", sock2)
	m.SetDockerExposeLAN(true)
	m.SetDockerSSHAgent(true)
	m.CreateFromRecipe(recipes.Docker)
	p.Frame()
	created, _ := st.Sandboxes()
	if len(created) != 1 {
		t.Fatalf("created sandboxes = %d, want 1", len(created))
	}
	if created[0].AutoPublishAddr != "0.0.0.0" || !slices.Equal(created[0].DynamicPortAddrs, []string{"0.0.0.0"}) {
		t.Errorf("CreateFromRecipe LAN = (addr=%q, dyn=%v)", created[0].AutoPublishAddr, created[0].DynamicPortAddrs)
	}
	if !slices.Equal(created[0].UnixForwards, []string{wantFwd2}) {
		t.Errorf("CreateFromRecipe SSH = %v, want [%s]", created[0].UnixForwards, wantFwd2)
	}

	// Verify JSON persistence keys and NewModel hydration from disk.
	rawJSON, err := os.ReadFile(filepath.Join(dir, "settings.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(rawJSON), `"docker_expose_lan": true`) || !strings.Contains(string(rawJSON), `"docker_ssh_agent": true`) {
		t.Errorf("settings.json missing expected snake_case keys:\n%s", rawJSON)
	}
	var m2 *Model
	p2 := ggui.ProbeBuilder(func() ggui.Widget { return ggui.Provide(ggui.ReducedMotionKey, true, Build(m2)) }, ggui.Sz(900, 700))
	p2.Setup(func() {
		m2 = NewModel(st, dir)
		m2.Offline = true
		m2.Attach(p2)
	})
	p2.Frame()
	if !ggui.Peek(m2.DockerExposeLAN) || !m2.dockerExposeLAN.Load() {
		t.Error("reloaded Model did not hydrate DockerExposeLAN")
	}
	if !ggui.Peek(m2.DockerSSHAgent) || !m2.dockerSSHAgent.Load() || m2.sandboxSSHAuthSock() != sock2 {
		t.Errorf("reloaded Model did not hydrate DockerSSHAgent: sock=%q", m2.sandboxSSHAuthSock())
	}
	p2.Close()

	// Race test: concurrent engine.Config callback reads while toggling settings.
	var wg sync.WaitGroup
	stop := make(chan struct{})
	for range 4 {
		wg.Go(func() {
			for {
				select {
				case <-stop:
					return
				default:
					_ = m.dockerExposeLAN.Load()
					_ = m.sandboxSSHAuthSock()
				}
			}
		})
	}
	for i := range 20 {
		on := i%2 == 0
		m.SetDockerExposeLAN(on)
		m.SetDockerSSHAgent(on)
	}
	close(stop)
	wg.Wait()
}

func TestDraftAutoPublishAndDynamicPortAddrs(t *testing.T) {
	SetLanguage(LangEnglish)
	base := sandbox.Spec{
		ID:               "p1",
		Name:             "web",
		Image:            "nginx:alpine",
		ExtraArgs:        "-auto-publish",
		AutoPublishAddr:  "0.0.0.0",
		DynamicPortAddrs: []string{"0.0.0.0", "192.168.1.10", "::1"},
	}

	// Full round-trip via DraftFrom -> Spec.
	d := DraftFrom(base)
	if d.AutoPublishAddr != "0.0.0.0" {
		t.Fatalf("DraftFrom AutoPublishAddr = %q", d.AutoPublishAddr)
	}
	if d.DynamicPortAddrs != "0.0.0.0\n192.168.1.10\n::1" {
		t.Fatalf("DraftFrom DynamicPortAddrs = %q", d.DynamicPortAddrs)
	}
	got, err := d.Spec()
	if err != nil {
		t.Fatalf("Draft.Spec() error: %v", err)
	}
	if !got.Equal(base) {
		t.Errorf("round-trip mismatch\n got  %+v\n want %+v", got, base)
	}
	if err := got.Validate(); err != nil {
		t.Errorf("Spec.Validate() error: %v", err)
	}

	// Comma- and newline-separated input with whitespace and comment lines parses cleanly.
	dMixed := d
	dMixed.DynamicPortAddrs = " 0.0.0.0, 192.168.1.10 \n # comment\n ::1 , "
	gotMixed, err := dMixed.Spec()
	if err != nil {
		t.Fatalf("comma/newline DynamicPortAddrs error: %v", err)
	}
	if !slices.Equal(gotMixed.DynamicPortAddrs, []string{"0.0.0.0", "192.168.1.10", "::1"}) {
		t.Errorf("parsed DynamicPortAddrs = %v", gotMixed.DynamicPortAddrs)
	}

	// Empty values are allowed.
	dEmpty := d
	dEmpty.AutoPublishAddr = "   "
	dEmpty.DynamicPortAddrs = "\n , \n"
	gotEmpty, err := dEmpty.Spec()
	if err != nil {
		t.Fatalf("empty addrs error: %v", err)
	}
	if gotEmpty.AutoPublishAddr != "" || len(gotEmpty.DynamicPortAddrs) != 0 {
		t.Errorf("empty addrs = (%q, %v)", gotEmpty.AutoPublishAddr, gotEmpty.DynamicPortAddrs)
	}

	// Invalid IP literals produce clear validation errors.
	for _, bad := range []string{"not-an-ip", "0.0.0.0:80", "fe80::1%en0", "localhost"} {
		dBadAuto := d
		dBadAuto.AutoPublishAddr = bad
		if _, err := dBadAuto.Spec(); err == nil {
			t.Errorf("AutoPublishAddr=%q expected error", bad)
		}
		dBadDyn := d
		dBadDyn.DynamicPortAddrs = "0.0.0.0, " + bad
		if _, err := dBadDyn.Spec(); err == nil {
			t.Errorf("DynamicPortAddrs with %q expected error", bad)
		}
	}

	// Editor sheet renders both fields in NAT mode and hides them in network=none.
	dir := t.TempDir()
	st := store.Store{Dir: dir}
	_ = st.SaveSandboxes([]sandbox.Spec{base})
	set := store.DefaultSettings()
	set.Language = LangEnglish
	_ = st.SaveSettings(set)

	var m *Model
	p := ggui.ProbeBuilder(func() ggui.Widget { return ggui.Provide(ggui.ReducedMotionKey, true, Build(m)) }, ggui.Sz(1280, 1400))
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
	m.EditSandbox(base.ID)
	p.Frame()
	if !shows("Auto-publish bind address") || !shows("Allowed live port addresses") {
		t.Errorf("editor NAT view missing Auto-publish / DynamicPortAddrs fields")
	}
	m.Draft.Update(func(x Draft) Draft { x.Network = "none"; return x })
	p.Frame()
	if shows("Auto-publish bind address") || shows("Allowed live port addresses") {
		t.Errorf("editor network=none should hide Auto-publish / DynamicPortAddrs fields")
	}
}
