package ui

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/ironpark/ggui"

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

	// 4. Running with BindAttach == false -> does NOT call applyLiveFn, shows restart toast.
	m.States.Set(map[string]sandbox.State{
		dockerSpec.ID: {Status: sandbox.Running, Control: true, BindAttach: false},
	})
	m.SetSharedFolders([]string{shareA})
	p.Frame()
	mu.Lock()
	gotCalls := len(diffs)
	mu.Unlock()
	if gotCalls != 3 {
		t.Errorf("BindAttach=false should not call applyLiveFn, got %d calls", gotCalls)
	}
	foundToast := false
	for _, n := range p.Semantics().All() {
		if strings.Contains(n.Name, "Restart the Docker Engine to share the change") {
			foundToast = true
			break
		}
	}
	if !foundToast {
		t.Errorf("BindAttach=false expected restart toast")
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
	p := ggui.ProbeBuilder(func() ggui.Widget { return ggui.Provide(ggui.ReducedMotionKey, true, Build(m)) }, ggui.Sz(1280, 1800))
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

	// Labs page: v0.20 (Control only) vs v0.21 (Control + KeepBlobs).
	const v21LabsNote = "With rungv v0.21+: docker push, offline docker save, and host Unix socket bind mounts (including /var/run/docker.sock)."
	m.RungvFeatures.Set(rungv.Features{Control: true})
	m.Go(PageLabs)
	p.Frame()
	if shows(v21LabsNote) {
		t.Error("v0.20 features should not show v0.21 Labs note")
	}
	m.RungvFeatures.Set(rungv.Features{Control: true, KeepBlobs: true})
	p.Frame()
	if !shows(v21LabsNote) {
		t.Errorf("v0.21 features missing Labs note:\n%s", p.Semantics())
	}
	if !shows("Also diff, export and commit, on stopped containers too.") {
		t.Error("v0.21 Labs page should keep v0.20 notes intact")
	}

	// Settings page: shared folders caption reacts to BindAttach on Unix.
	m.RungvFeatures.Set(rungv.Features{Control: true})
	m.Go(PageSettings)
	p.Frame()
	if !shows("The engine sees a change when it is restarted.") {
		t.Errorf("v0.20 Settings missing restart caption")
	}
	m.RungvFeatures.Set(rungv.Features{Control: true, BindAttach: true})
	p.Frame()
	if runtime.GOOS != "windows" {
		if !shows("Changes apply live to a running engine without restarting.") {
			t.Errorf("v0.21 Settings missing live shared folders caption")
		}
	} else {
		if !shows("The engine sees a change when it is restarted.") {
			t.Errorf("Windows Settings should keep restart caption")
		}
	}
}
