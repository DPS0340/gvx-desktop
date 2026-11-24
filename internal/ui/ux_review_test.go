package ui

import (
	"reflect"
	"strings"
	"testing"

	"github.com/ironpark/ggui"
	"github.com/ironpark/ggui/inspect"
	"github.com/ironpark/ggui/ui"
	uitheme "github.com/ironpark/ggui/ui/theme"

	"github.com/ironpark/gvx-desktop/internal/dockerapi"
	"github.com/ironpark/gvx-desktop/internal/recipes"
	"github.com/ironpark/gvx-desktop/internal/sandbox"
	"github.com/ironpark/gvx-desktop/internal/store"
)

func TestContainerStatusHealthcheck(t *testing.T) {
	t.Cleanup(func() { SetLanguage(LangEnglish) })
	SetLanguage(LangEnglish)

	cases := []struct {
		name  string
		c     dockerapi.Container
		color interface{}
		want  string
		pulse bool
	}{
		{"plain running", dockerapi.Container{State: "running", Status: "Up 2 minutes"}, colorGreen, "Running", false},
		{"healthy", dockerapi.Container{State: "running", Status: "Up 2 minutes (healthy)"}, colorGreen, "Healthy", false},
		{"unhealthy", dockerapi.Container{State: "running", Status: "Up 2 minutes (unhealthy)"}, colorRed, "Unhealthy", false},
		{"health starting", dockerapi.Container{State: "running", Status: "Up 3 seconds (health: starting)"}, colorAmber, "Starting", true},
		{"paused", dockerapi.Container{State: "paused", Status: "Up 2 minutes (Paused)"}, colorAmber, "Paused", false},
	}
	for _, tc := range cases {
		col, text, pulse := containerStatus(tc.c)
		if col != tc.color || text != tc.want || pulse != tc.pulse {
			t.Errorf("%s: got (%v, %q, %v), want (%v, %q, %v)", tc.name, col, text, pulse, tc.color, tc.want, tc.pulse)
		}
	}
}

func TestContainerRowsFilteredPreservesProjectCounts(t *testing.T) {
	list := composeContainers()
	if got := rowsText(containerRowsFiltered(list, "running", "", nil)); got != "[blog 1/1],blog-db-1,[shop 1/2],shop-web-1" {
		t.Errorf("running filter: got %s", got)
	}
	if got := rowsText(containerRowsFiltered(list, "stopped", "", nil)); got != "migrate,[shop 1/2],shop-redis-1" {
		t.Errorf("stopped filter: got %s", got)
	}
}

func TestDraftSpecOmitsHiddenNATFields(t *testing.T) {
	d := Draft{
		Name:             "api",
		Image:            "alpine:3.20",
		Network:          "none",
		NetTX:            "100m",
		NetRX:            "50m",
		NATPolicyFile:    "/tmp/nat.yaml",
		DynamicPortAddrs: "0.0.0.0",
		AutoPublish:      true,
		AutoPublishAddr:  "0.0.0.0",
		ExtraArgs:        "-auto-publish",
	}
	spec, err := d.Spec()
	if err != nil {
		t.Fatalf("Spec() with Network=none failed: %v", err)
	}
	if spec.NetTXBPS != 0 || spec.NetRXBPS != 0 || spec.NATPolicyFile != "" || len(spec.DynamicPortAddrs) != 0 || spec.AutoPublishAddr != "" {
		t.Errorf("hidden NAT fields leaked into spec: %+v", spec)
	}
	if sandbox.HasAutoPublish(spec) {
		t.Errorf("auto-publish leaked into spec when Network=none: %+v", spec.ExtraArgs)
	}

	// With Network=nat and AutoPublish=false, -auto-publish is stripped while NAT fields stay.
	d.Network = "nat"
	d.AutoPublish = false
	d.AutoPublishAddr = ""
	spec, err = d.Spec()
	if err != nil {
		t.Fatalf("Spec() with Network=nat AutoPublish=false failed: %v", err)
	}
	if spec.AutoPublishAddr != "" || sandbox.HasAutoPublish(spec) {
		t.Errorf("AutoPublishAddr/flag leaked when AutoPublish=false: addr=%q extra=%v", spec.AutoPublishAddr, spec.ExtraArgs)
	}
	if spec.NetTXBPS == 0 || spec.NetRXBPS == 0 || spec.NATPolicyFile != "/tmp/nat.yaml" {
		t.Errorf("NAT fields should be preserved when Network=nat: %+v", spec)
	}

	// With Network=nat and AutoPublish=true, both -auto-publish and AutoPublishAddr are set.
	d.AutoPublish = true
	d.AutoPublishAddr = "0.0.0.0"
	spec, err = d.Spec()
	if err != nil {
		t.Fatalf("Spec() with Network=nat AutoPublish=true failed: %v", err)
	}
	if !sandbox.HasAutoPublish(spec) || spec.AutoPublishAddr != "0.0.0.0" {
		t.Errorf("expected -auto-publish and AutoPublishAddr=0.0.0.0, got addr=%q extra=%v", spec.AutoPublishAddr, spec.ExtraArgs)
	}
}

func TestIsValidVolumeName(t *testing.T) {
	valid := []string{"pgdata", "my_vol-1.0", "0data", "A_B"}
	for _, s := range valid {
		if !isValidVolumeName(s) {
			t.Errorf("isValidVolumeName(%q) = false, want true", s)
		}
	}
	invalid := []string{"", "   ", "-leading", ".hidden", "has space", "a/b", `a\b`, "a:b", "bad@name"}
	for _, s := range invalid {
		if isValidVolumeName(s) {
			t.Errorf("isValidVolumeName(%q) = true, want false", s)
		}
	}
}

func TestNormalizeImageRef(t *testing.T) {
	want := "nginx:latest"
	for _, raw := range []string{"nginx", "nginx:latest", "library/nginx", "docker.io/library/nginx:latest", "index.docker.io/library/nginx"} {
		if got := normalizeImageRef(raw); got != want {
			t.Errorf("normalizeImageRef(%q) = %q, want %q", raw, got, want)
		}
	}
	if got := normalizeImageRef("ghcr.io/org/app"); got != "ghcr.io/org/app:latest" {
		t.Errorf("ghcr ref: got %q", got)
	}
	if got := normalizeImageRef("ghcr.io/org/app:1.2"); got != "ghcr.io/org/app:1.2" {
		t.Errorf("ghcr tagged ref: got %q", got)
	}
}

func TestStopAllForUpdateDoesNotSetQuitting(t *testing.T) {
	dir := t.TempDir()
	st := store.Store{Dir: dir}
	var m *Model
	p := ggui.ProbeBuilder(func() ggui.Widget { return Build(m) }, ggui.Sz(1000, 700))
	p.Setup(func() {
		m = NewModel(st, dir)
		m.Offline = true
		m.Attach(p)
	})
	defer p.Close()
	p.Frame()

	called := false
	m.StopAllForUpdate(func() { called = true })
	settle(t, p, func() bool { return called }, "StopAllForUpdate callback")
	if m.quitting {
		t.Fatal("StopAllForUpdate must not set m.quitting = true")
	}
}

func TestSwitchSandboxEngineConfirmsWhenContainersRunning(t *testing.T) {
	t.Cleanup(func() { SetLanguage(LangEnglish) })
	SetLanguage(LangEnglish)
	dir := t.TempDir()
	st := store.Store{Dir: dir}
	set := store.DefaultSettings()
	set.Language = LangEnglish
	set.SandboxEngine = true
	_ = st.SaveSettings(set)

	var m *Model
	p := ggui.ProbeBuilder(func() ggui.Widget { return Build(m) }, ggui.Sz(1000, 700))
	p.Setup(func() {
		m = NewModel(st, dir)
		m.Offline = true
		m.Attach(p)
	})
	defer p.Close()
	p.Frame()

	// With running containers, switching off asks for confirmation first.
	m.EngineRunning.Set(2)
	m.SwitchSandboxEngine(false)
	p.Frame()
	if !ggui.Peek(m.ConfirmOpen) {
		t.Fatal("expected confirmation dialog when switching off sandbox engine with running containers")
	}
	if !ggui.Peek(m.Settings).SandboxEngine {
		t.Fatal("SandboxEngine should stay true until confirmed")
	}
	if title := ggui.Peek(m.ConfirmTitle); !strings.Contains(title, "compatibility mode") {
		t.Errorf("unexpected confirm title: %q", title)
	}

	// Confirming applies the switch.
	m.ConfirmOpen.Set(false)
	m.RunConfirmed()
	p.Frame()
	if ggui.Peek(m.Settings).SandboxEngine {
		t.Fatal("SandboxEngine should be false after confirming")
	}
}

func TestCloseEditorConfirmsOnlyWhenDirty(t *testing.T) {
	t.Cleanup(func() { SetLanguage(LangEnglish) })
	SetLanguage(LangEnglish)
	dir := t.TempDir()
	st := store.Store{Dir: dir}
	var m *Model
	p := ggui.ProbeBuilder(func() ggui.Widget { return Build(m) }, ggui.Sz(1000, 700))
	p.Setup(func() {
		m = NewModel(st, dir)
		m.Offline = true
		m.Attach(p)
	})
	defer p.Close()
	p.Frame()

	// Clean editor closes immediately.
	m.NewSandbox("nginx:alpine")
	p.Frame()
	m.CloseEditor()
	if ggui.Peek(m.EditorOpen) {
		t.Fatal("clean editor did not close immediately")
	}
	if ggui.Peek(m.ConfirmOpen) {
		t.Fatal("clean editor should not open confirm dialog")
	}

	// Dirty editor asks for confirmation.
	m.NewSandbox("nginx:alpine")
	m.Draft.Update(func(d Draft) Draft { d.Memory = "1g"; return d })
	p.Frame()
	m.CloseEditor()
	if !ggui.Peek(m.ConfirmOpen) || !ggui.Peek(m.EditorOpen) {
		t.Fatalf("dirty editor should open confirm dialog and stay open: confirm=%v editor=%v", ggui.Peek(m.ConfirmOpen), ggui.Peek(m.EditorOpen))
	}
	m.ConfirmOpen.Set(false)
	m.RunConfirmed()
	if ggui.Peek(m.EditorOpen) {
		t.Fatal("confirming discard should close editor")
	}
}

func TestQuitHeadingAndSummary(t *testing.T) {
	t.Cleanup(func() { SetLanguage(LangEnglish) })
	SetLanguage(LangEnglish)

	if h := quitHeading(2, 0); h != "Stop running sandboxes?" {
		t.Errorf("sandboxes only heading: %q", h)
	}
	if h := quitHeading(0, 3); h != "Stop running containers?" {
		t.Errorf("containers only heading: %q", h)
	}
	if h := quitHeading(1, 2); h != "Stop running sandboxes and containers?" {
		t.Errorf("both heading: %q", h)
	}
	if s := quitSummary(0, 2); !strings.Contains(s, "2 containers are running") {
		t.Errorf("containers only summary: %q", s)
	}
	if s := quitSummary(1, 2); !strings.Contains(s, "1 sandbox and 2 containers") {
		t.Errorf("both summary: %q", s)
	}
}

func TestSidebarHoverDoesNotBleedIntoSnapshots(t *testing.T) {
	t.Cleanup(func() { SetLanguage(LangEnglish) })
	SetLanguage(LangEnglish)
	dir := t.TempDir()
	st := store.Store{Dir: dir}
	set := store.DefaultSettings()
	set.Language = LangEnglish
	set.SandboxEngine = true
	_ = st.SaveSettings(set)
	k3s := recipes.KubernetesNode()
	_ = st.SaveSandboxes([]sandbox.Spec{k3s})

	var m *Model
	p := ggui.ProbeBuilder(func() ggui.Widget {
		return ggui.Provide(ggui.ReducedMotionKey, true, Build(m))
	}, ggui.Sz(1280, 840))
	p.Setup(func() {
		preset := uitheme.Preset{Base: uitheme.BaseZinc, Accent: uitheme.AccentBlue}
		uitheme.Bind(ggui.Const(false), preset.Dark(), preset.Light())
		m = NewModel(st, dir)
		m.Offline = true
		m.Attach(p)
		m.States.Set(map[string]sandbox.State{k3s.ID: {Status: sandbox.Running}})
	})
	defer p.Close()
	var sb *ui.SidebarWidget
	rows := map[string]*sidebarTabRow{}
	p.OnInspect(func(f *inspect.Frame) {
		for _, n := range f.Nodes {
			if w, ok := n.Instance.(*ui.SidebarWidget); ok {
				sb = w
			}
			if r, ok := n.Instance.(*sidebarTabRow); ok {
				rows[r.key] = r
			}
		}
	})
	p.Frame()

	if sb == nil {
		t.Fatal("missing *ui.SidebarWidget in inspected tree")
	}
	sidebarHover := func() int64 {
		return reflect.ValueOf(sb).Elem().FieldByName("hover").Int()
	}

	findTab := func(label string) ggui.Found {
		t.Helper()
		f, ok := p.FindRole(ggui.RoleTab, label)
		if !ok {
			t.Fatalf("missing sidebar tab %q:\n%s", label, p.Semantics())
		}
		return f
	}
	assertOnlyCustomHovered := func(wantKey string) {
		t.Helper()
		p.Frame()
		if got := sidebarHover(); got != -1 {
			t.Errorf("when hovering %q: SidebarWidget.hover=%d, want -1 (must not bleed into Snapshots)", wantKey, got)
		}
		for k, r := range rows {
			if want := k == wantKey; r.hover != want {
				t.Errorf("when hovering %q: row %q hover=%v, want %v", wantKey, k, r.hover, want)
			}
		}
	}

	// Hovering Snapshots itself hovers a native SidebarItem (not any custom row).
	p.Move(findTab("Snapshots").Center())
	p.Frame()
	snapHover := sidebarHover()
	if snapHover < 0 {
		t.Fatalf("expected Snapshots to set SidebarWidget.hover >= 0, got %d", snapHover)
	}
	for k, r := range rows {
		if r.hover {
			t.Errorf("when hovering Snapshots: custom row %q unexpectedly hovered", k)
		}
	}

	// Hovering Docker, Kubernetes, or any nested sub-item must highlight only
	// that custom row and NEVER bleed hover onto Snapshots.
	for _, tc := range []struct {
		label string
		key   string
	}{
		{"Docker", PageDocker},
		{"Networks", PageNetworks},
		{"Containers", PageContainers},
		{"Kubernetes", PageKubernetes},
		{"Pods", PagePods},
		{"Workloads", PageWorkloads},
		{"Services", PageServices},
	} {
		p.Move(findTab(tc.label).Center())
		assertOnlyCustomHovered(tc.key)
	}

	// Hovering Image registry hovers a native SidebarItem distinct from Snapshots.
	p.Move(findTab("Image registry").Center())
	p.Frame()
	if got := sidebarHover(); got < 0 || got == snapHover {
		t.Errorf("when hovering Image registry: SidebarWidget.hover=%d, want >=0 and != %d", got, snapHover)
	}
}
