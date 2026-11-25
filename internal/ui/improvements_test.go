package ui

import (
	"slices"
	"strings"
	"testing"

	"github.com/ironpark/ggui"

	"github.com/ironpark/gvx-desktop/internal/appupdate"
	"github.com/ironpark/gvx-desktop/internal/recipes"
	"github.com/ironpark/gvx-desktop/internal/rungv"
	"github.com/ironpark/gvx-desktop/internal/sandbox"
	"github.com/ironpark/gvx-desktop/internal/store"
)

func TestAskRestartToUpdateCountsEngineContainers(t *testing.T) {
	m, _ := appUpdateModel(t)
	m.Offline = false
	fake := &fakeSandboxEngine{running: 2}
	m.sandboxEngine = fake
	m.dockerProxy.SetLocal(fake.Handler())
	m.AppUpdate.Set(AppUpdate{Tag: "v0.6.0", Install: appupdate.Install{Method: appupdate.Bundle}, Ready: true})

	m.AskRestartToUpdate()
	if !ggui.Peek(m.ConfirmOpen) {
		t.Fatal("expected confirmation dialog")
	}
	if ggui.Peek(m.ConfirmSafe) {
		t.Error("expected destructive confirmation when sandbox-engine containers are running")
	}
	if !strings.Contains(ggui.Peek(m.ConfirmText), "2 sandboxes are running") {
		t.Errorf("confirm text = %q, want mention of 2 running sandboxes", ggui.Peek(m.ConfirmText))
	}
}

func TestDraftLiveDuplicateAndSaveAndRestartButton(t *testing.T) {
	t.Cleanup(func() { SetLanguage(LangEnglish) })
	dir := t.TempDir()
	st := store.Store{Dir: dir}
	set := store.DefaultSettings()
	set.Language = LangEnglish
	_ = st.SaveSettings(set)
	web := sandbox.Spec{ID: "sb-web", Name: "web", Image: "nginx:alpine"}
	db := sandbox.Spec{ID: "sb-db", Name: "db", Image: "postgres:17-alpine"}
	_ = st.SaveSandboxes([]sandbox.Spec{web, db})

	var m *Model
	p := ggui.ProbeBuilder(func() ggui.Widget { return ggui.Provide(ggui.ReducedMotionKey, true, Build(m)) }, ggui.Sz(1280, 840))
	p.Setup(func() {
		m = NewModel(st, dir)
		m.Offline = true
		m.Attach(p)
	})
	defer p.Close()
	p.Frame()

	// Creating a new sandbox with an existing name flags DraftError live.
	m.OpenEditor(sandbox.Spec{Name: "web", Image: "alpine:3.20"})
	p.Frame()
	if got := ggui.Peek(m.DraftError); !strings.Contains(got, "another sandbox is called web") {
		t.Fatalf("DraftError = %q, want duplicate name error", got)
	}
	if btn, ok := p.Semantics().Find(ggui.RoleButton, "Save and start"); !ok || !btn.Disabled {
		t.Errorf("new sandbox editor missing disabled Save and start button:\n%s", p.Semantics())
	}

	// Editing a running sandbox offers "Save and restart".
	m.States.Set(map[string]sandbox.State{web.ID: {Status: sandbox.Running}})
	m.EditSandbox(web.ID)
	p.Frame()
	if got := ggui.Peek(m.DraftError); got != "" {
		t.Fatalf("editing own name produced DraftError = %q", got)
	}
	if _, ok := p.Find("Save and restart"); !ok {
		t.Errorf("running sandbox editor missing Save and restart button:\n%s", p.Semantics())
	}
}

func TestSandboxesStatusFilterAndVolumesFilter(t *testing.T) {
	t.Cleanup(func() { SetLanguage(LangEnglish) })
	dir := t.TempDir()
	st := store.Store{Dir: dir}
	set := store.DefaultSettings()
	set.Language = LangEnglish
	_ = st.SaveSettings(set)
	web := sandbox.Spec{ID: "sb-1", Name: "web", Image: "nginx:alpine"}
	db := sandbox.Spec{
		ID: "sb-2", Name: "postgres", Image: "postgres:17-alpine",
		Mounts: []sandbox.Mount{{Type: sandbox.MountVolume, Source: "pgdata", Target: "/var/lib/postgresql/data"}},
	}
	_ = st.SaveSandboxes([]sandbox.Spec{web, db})

	var m *Model
	p := ggui.ProbeBuilder(func() ggui.Widget { return ggui.Provide(ggui.ReducedMotionKey, true, Build(m)) }, ggui.Sz(1280, 840))
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

	m.States.Set(map[string]sandbox.State{
		web.ID: {Status: sandbox.Running},
		db.ID:  {Status: sandbox.Stopped},
	})
	m.Go(PageSandboxes)
	p.Frame()
	if !shows("web") || !shows("postgres") {
		t.Fatalf("expected both sandboxes under All:\n%s", p.Semantics())
	}

	m.SandboxStatus.Set("running")
	p.Frame()
	if !shows("web") || shows("postgres") {
		t.Errorf("Running filter did not hide stopped sandbox:\n%s", p.Semantics())
	}

	m.SandboxStatus.Set("stopped")
	p.Frame()
	if shows("web") || !shows("postgres") {
		t.Errorf("Stopped filter did not isolate stopped sandbox:\n%s", p.Semantics())
	}

	// Volumes page shows sandbox user alongside In use and filters by sandbox name.
	m.Volumes.Set(Listing[rungv.Volume]{Loaded: true, Items: []rungv.Volume{
		{Name: "pgdata", Driver: "ext4lite", Size: "64MB", InUse: true, Mountpoint: "/tmp/pgdata.ext4"},
		{Name: "scratch", Driver: "hostfs", Size: "1MB", InUse: false},
	}})
	m.Go(PageVolumes)
	p.Frame()
	if !shows("In use") || !shows("postgres") || !shows("scratch") {
		t.Fatalf("volumes table missing expected rows or user:\n%s", p.Semantics())
	}

	m.VolumeQuery.Set("postgres")
	p.Frame()
	if !shows("pgdata") || shows("scratch") {
		t.Errorf("volume filter by sandbox name failed:\n%s", p.Semantics())
	}
}

func TestSettingsFieldsSyncWithModelUpdates(t *testing.T) {
	t.Cleanup(func() { SetLanguage(LangEnglish) })
	dir := t.TempDir()
	st := store.Store{Dir: dir}
	set := store.DefaultSettings()
	set.Language = LangEnglish
	_ = st.SaveSettings(set)

	var m *Model
	p := ggui.ProbeBuilder(func() ggui.Widget { return ggui.Provide(ggui.ReducedMotionKey, true, Build(m)) }, ggui.Sz(1280, 2000))
	p.Setup(func() {
		m = NewModel(st, dir)
		m.Offline = true
		m.Attach(p)
	})
	defer p.Close()
	p.Frame()

	m.Go(PageSettings)
	p.Frame()

	// Updating RungvPath externally (as InstallRungv does) syncs the TextField.
	m.UpdateSettings(func(s *store.Settings) { s.RungvPath = "/custom/bin/rungvx" })
	p.Frame()
	found := false
	for _, n := range p.Semantics().All() {
		if n.Name == "rungvx path" && n.Value == "/custom/bin/rungvx" {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("rungvx path field did not sync after UpdateSettings:\n%s", p.Semantics())
	}
}

func TestCommandPaletteHasRecipesAndSandboxes(t *testing.T) {
	t.Cleanup(func() { SetLanguage(LangEnglish) })
	dir := t.TempDir()
	st := store.Store{Dir: dir}
	set := store.DefaultSettings()
	set.Language = LangEnglish
	_ = st.SaveSettings(set)
	_ = st.SaveSandboxes([]sandbox.Spec{{ID: "sb-1", Name: "my-api", Image: "nginx:alpine"}})

	var m *Model
	p := ggui.ProbeBuilder(func() ggui.Widget { return ggui.Provide(ggui.ReducedMotionKey, true, Build(m)) }, ggui.Sz(1280, 840))
	p.Setup(func() {
		m = NewModel(st, dir)
		m.Offline = true
		m.Attach(p)
	})
	defer p.Close()
	p.Frame()

	m.PaletteOpen.Set(true)
	p.Frame()
	names := map[string]bool{}
	for _, n := range p.Semantics().Nodes(ggui.RoleMenuItem) {
		names[n.Name] = true
	}
	for _, want := range []string{"Docker", "Open Docker", "Kubernetes", "Toggle sidebar", "my-api"} {
		if !names[want] {
			t.Errorf("command palette missing %q (have: %v)", want, names)
		}
	}
}

func TestSidebarDockerAndKubernetesSubmenus(t *testing.T) {
	SetLanguage(LangEnglish)
	dir := t.TempDir()
	st := store.Store{Dir: dir}
	set := store.DefaultSettings()
	set.Language = LangEnglish
	set.SandboxEngine = false // test both engines stopped first
	_ = st.SaveSettings(set)
	dind := recipes.DockerEngine()
	k3s := recipes.KubernetesNode()
	_ = st.SaveSandboxes([]sandbox.Spec{dind, k3s})

	var m *Model
	p := ggui.ProbeBuilder(func() ggui.Widget { return ggui.Provide(ggui.ReducedMotionKey, true, Build(m)) }, ggui.Sz(1280, 840))
	p.Setup(func() {
		m = NewModel(st, dir)
		m.Offline = true
		m.Attach(p)
	})
	defer p.Close()
	p.Frame()

	tabs := func() []string {
		var out []string
		for _, n := range p.Semantics().Nodes(ggui.RoleTab) {
			out = append(out, n.Name)
		}
		return out
	}

	// While both engines are stopped, Docker and Kubernetes appear without
	// separate subheader rows and without their runtime sub-items.
	stopped := tabs()
	for _, want := range []string{"Docker", "Kubernetes"} {
		if !slices.Contains(stopped, want) {
			t.Errorf("stopped sidebar missing %q (have: %v)", want, stopped)
		}
	}
	for _, unwanted := range []string{"Docker Engine", "Nodes", "Networks", "Containers", "Pods", "Workloads", "Services"} {
		if slices.Contains(stopped, unwanted) {
			t.Errorf("stopped sidebar unexpectedly shows %q (have: %v)", unwanted, stopped)
		}
	}

	// While both engines run, Networks & Containers nest under Docker and
	// Pods, Workloads & Services nest under Kubernetes.
	m.States.Set(map[string]sandbox.State{
		dind.ID: {Status: sandbox.Running},
		k3s.ID:  {Status: sandbox.Running},
	})
	p.Frame()
	running := tabs()
	idx := func(name string) int { return slices.Index(running, name) }
	if !(idx("Docker") >= 0 && idx("Docker") < idx("Networks") && idx("Networks") < idx("Containers") && idx("Containers") < idx("Kubernetes")) {
		t.Errorf("unexpected Docker submenu order: %v", running)
	}
	if !(idx("Kubernetes") >= 0 && idx("Kubernetes") < idx("Pods") && idx("Pods") < idx("Workloads") && idx("Workloads") < idx("Services")) {
		t.Errorf("unexpected Kubernetes submenu order: %v", running)
	}

	// Folding Docker hides Networks and Containers while keeping Docker clickable.
	clickRole := func(role ggui.Role, name string, xOffset float64) {
		t.Helper()
		var rect ggui.Rect
		found := false
		for _, n := range p.Semantics().Nodes(role) {
			if n.Name == name {
				rect, found = n.Rect, true
				break
			}
		}
		if !found {
			t.Fatalf("%v %q not found in semantics:\n%s", role, name, p.Semantics())
		}
		x := rect.Origin.X + rect.Size.W/2
		if xOffset >= 0 {
			x = rect.Origin.X + xOffset
		}
		p.Click(ggui.Pt(x, rect.Origin.Y+rect.Size.H/2))
		p.Frame()
	}
	clickRole(ggui.RoleDisclosure, "Fold Docker", -1)
	if ggui.Peek(m.DockerNavOpen) {
		t.Fatal("expected Docker submenu to fold after clicking chevron")
	}
	if ggui.Peek(m.Page) == PageDocker {
		t.Fatal("clicking fold chevron should not navigate to Docker page")
	}
	folded := tabs()
	if slices.Contains(folded, "Networks") || slices.Contains(folded, "Containers") {
		t.Errorf("folded Docker should hide sub-items (have: %v)", folded)
	}
	// Clicking Docker itself still navigates to PageDocker.
	clickRole(ggui.RoleTab, "Docker", 24)
	if ggui.Peek(m.Page) != PageDocker {
		t.Fatalf("clicking Docker tab navigated to %q, want %q", ggui.Peek(m.Page), PageDocker)
	}

	// Unfolding Docker restores Networks and Containers.
	clickRole(ggui.RoleDisclosure, "Unfold Docker", -1)
	if !ggui.Peek(m.DockerNavOpen) {
		t.Fatal("expected Docker submenu to unfold")
	}
	if restored := tabs(); !slices.Contains(restored, "Networks") || !slices.Contains(restored, "Containers") {
		t.Errorf("unfolded Docker missing sub-items (have: %v)", restored)
	}

	// Folding Kubernetes hides Pods/Workloads/Services; navigating directly
	// to PagePods auto-expands Kubernetes again.
	clickRole(ggui.RoleDisclosure, "Fold Kubernetes", -1)
	if ggui.Peek(m.KubeNavOpen) {
		t.Fatal("expected Kubernetes submenu to fold")
	}
	if kFolded := tabs(); slices.Contains(kFolded, "Pods") || slices.Contains(kFolded, "Workloads") || slices.Contains(kFolded, "Services") {
		t.Errorf("folded Kubernetes should hide sub-items (have: %v)", kFolded)
	}
	m.Go(PagePods)
	p.Frame()
	if !ggui.Peek(m.KubeNavOpen) {
		t.Fatal("navigating to PagePods should auto-expand Kubernetes submenu")
	}
}
