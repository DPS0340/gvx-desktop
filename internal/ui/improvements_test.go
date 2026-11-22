package ui

import (
	"strings"
	"testing"

	"github.com/ironpark/ggui"

	"github.com/ironpark/gvx-desktop/internal/appupdate"
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
	m.UpdateSettings(func(s *store.Settings) { s.RungvPath = "/custom/bin/rungv" })
	p.Frame()
	found := false
	for _, n := range p.Semantics().All() {
		if n.Name == "rungv path" && n.Value == "/custom/bin/rungv" {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("rungv path field did not sync after UpdateSettings:\n%s", p.Semantics())
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
	for _, want := range []string{"Docker Engine", "Kubernetes", "Toggle sidebar", "my-api"} {
		if !names[want] {
			t.Errorf("command palette missing %q (have: %v)", want, names)
		}
	}
}
