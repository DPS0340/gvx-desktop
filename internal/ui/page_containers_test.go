package ui

import (
	"strings"
	"testing"
	"time"

	"github.com/ironpark/ggui"

	"github.com/ironpark/gvx-desktop/internal/dockerapi"
	"github.com/ironpark/gvx-desktop/internal/recipes"
	"github.com/ironpark/gvx-desktop/internal/sandbox"
	"github.com/ironpark/gvx-desktop/internal/store"
)

func TestContainersAccess(t *testing.T) {
	docker := recipes.DockerEngine()
	other := sandbox.Spec{ID: "web", Name: "web", Image: "nginx"}
	for _, tc := range []struct {
		name string
		rows []Row
		want ContainersAccess
	}{
		{"no sandboxes", nil, ContainersNoEngine},
		{"no engine", []Row{{Spec: other, State: sandbox.State{Status: sandbox.Running}}}, ContainersNoEngine},
		{"never started", []Row{{Spec: docker}}, ContainersStopped},
		{"starting", []Row{{Spec: docker, State: sandbox.State{Status: sandbox.Starting}}}, ContainersStarting},
		{"running", []Row{{Spec: other}, {Spec: docker, State: sandbox.State{Status: sandbox.Running}}}, ContainersOpen},
		{"stopping", []Row{{Spec: docker, State: sandbox.State{Status: sandbox.Stopping}}}, ContainersStopped},
		{"exited", []Row{{Spec: docker, State: sandbox.State{Status: sandbox.Exited}}}, ContainersStopped},
		{"failed", []Row{{Spec: docker, State: sandbox.State{Status: sandbox.Failed, ExitCode: 1}}}, ContainersStopped},
	} {
		got := containersAccess(tc.rows)
		if got != tc.want {
			t.Errorf("%s: %v, want %v", tc.name, got, tc.want)
		}
		if open := got == ContainersOpen; open != (got.Reason() == "") {
			t.Errorf("%s: open %v with the reason %q", tc.name, open, got.Reason())
		}
	}
}

func TestFilterContainers(t *testing.T) {
	list := []dockerapi.Container{
		{ID: "3f2a9c1b7d4e5f60", Names: []string{"/shop-web-1"}, Image: "nginx:alpine"},
		{ID: "8b7c6d5e4f3a2b10", Names: []string{"/shop-redis-1"}, Image: "redis:alpine"},
		{ID: "1a2b3c4d5e6f7081", Names: []string{"/migrate"}, Image: "app:dev"},
	}
	names := func(l []dockerapi.Container) string {
		var out []string
		for _, c := range l {
			out = append(out, c.Name())
		}
		return strings.Join(out, ",")
	}
	for q, want := range map[string]string{
		"":            "shop-web-1,shop-redis-1,migrate",
		"  SHOP ":     "shop-web-1,shop-redis-1",
		"alpine":      "shop-web-1,shop-redis-1",
		"8b7c6d":      "shop-redis-1",
		"5f60":        "", // past the short ID
		"app:dev":     "migrate",
		"nothing-yet": "",
	} {
		if got := names(filterContainers(list, q)); got != want {
			t.Errorf("%q: %s, want %s", q, got, want)
		}
	}
	if c := chosenContainer(list, "8b7c6d5e4f3a2b10"); c.Name() != "shop-redis-1" {
		t.Errorf("the chosen one: %s", c.Name())
	}
	if c := chosenContainer(list, "gone"); c.Name() != "shop-web-1" {
		t.Errorf("none chosen is the first: %s", c.Name())
	}
}

func TestContainerLogLinesKeepTheirNumbers(t *testing.T) {
	at := time.Date(2026, 11, 16, 6, 0, 0, 0, time.UTC)
	lines := []dockerapi.LogLine{
		{Time: at, Text: "a"},
		{Time: at, Text: "b"}, // the same time
		{Text: "c"},           // none
		{Time: at.Add(time.Second), Text: "d", Stderr: true},
	}
	got := containerLogLines(lines)
	for i := 1; i < len(got); i++ {
		if got[i].Seq <= got[i-1].Seq {
			t.Fatalf("line %d is numbered %d after %d", i, got[i].Seq, got[i-1].Seq)
		}
	}
	if got[3].Stream != sandbox.Stderr || got[0].Stream != sandbox.Stdout {
		t.Errorf("streams %v %v", got[0].Stream, got[3].Stream)
	}
	// With the first line gone from the tail, the others keep theirs.
	again := containerLogLines(lines[1:])
	if again[2].Seq != got[3].Seq {
		t.Errorf("a line was renumbered: %d, was %d", again[2].Seq, got[3].Seq)
	}
}

// The Containers item of the sidebar, the palette's entry and the page
// itself follow the Docker Engine: open while it runs, greyed out with the
// reason in a tip otherwise. A page left open as the engine stops stays,
// and says so.
func TestTheContainersItemFollowsTheEngine(t *testing.T) {
	dir := t.TempDir()
	st := store.Store{Dir: dir}
	docker := recipes.DockerEngine()
	if err := st.SaveSandboxes([]sandbox.Spec{docker}); err != nil {
		t.Fatal(err)
	}
	set := store.DefaultSettings()
	set.Language = LangEnglish
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

	status := func(s sandbox.Status) {
		m.States.Set(map[string]sandbox.State{docker.ID: {Status: s}})
		p.Frame()
	}
	item := func() ggui.SemNode {
		t.Helper()
		n, ok := p.Semantics().Find(ggui.RoleTab, "Containers")
		if !ok {
			t.Fatalf("no Containers item:\n%s", p.Semantics())
		}
		return n
	}
	shows := func(text string) bool {
		for _, n := range p.Semantics().All() {
			if n.Name == text {
				return true
			}
		}
		return false
	}

	for _, s := range []sandbox.Status{sandbox.Stopped, sandbox.Starting, sandbox.Stopping, sandbox.Exited, sandbox.Failed} {
		status(s)
		if !item().Disabled {
			t.Errorf("%v: the item is enabled", s)
		}
		m.Go(PageContainers)
		if ggui.Peek(m.Page) == PageContainers {
			t.Errorf("%v: Go opened the page", s)
			m.Page.Set(PageDashboard)
		}
	}

	// Stopped, its tip tells why.
	status(sandbox.Stopped)
	why := ContainersStopped.Reason()
	n := item()
	p.Move(ggui.Pt(n.Rect.Origin.X+n.Rect.Size.W-12, n.Rect.Origin.Y+n.Rect.Size.H/2))
	for range 30 {
		p.Frame()
	}
	if !shows(why) {
		t.Errorf("no tip saying %q over the item's mark:\n%s", why, p.Semantics())
	}
	p.Move(ggui.Pt(600, 400))

	status(sandbox.Running)
	if item().Disabled {
		t.Fatal("running: the item is disabled")
	}
	m.DockerContainers.Set(Listing[dockerapi.Container]{Loaded: true, Items: []dockerapi.Container{
		{ID: "3f2a9c1b7d4e5f60", Names: []string{"/web"}, Image: "nginx:alpine", State: "running"},
	}})
	m.Go(PageContainers)
	if ggui.Peek(m.Page) != PageContainers {
		t.Fatal("running: Go did not open the page")
	}
	if n := ggui.Peek(m.RunningContainers); n != 1 {
		t.Errorf("the item's badge counts %d running containers", n)
	}
	if !shows("web") {
		t.Errorf("the page does not list the container:\n%s", p.Semantics())
	}

	// The palette's entry is open too, and is greyed out with the item.
	m.PaletteOpen.Set(true)
	entry := func() ggui.SemNode {
		for _, n := range p.Semantics().Nodes(ggui.RoleMenuItem) {
			if n.Name == "Containers" {
				return n
			}
		}
		t.Fatalf("no Containers entry in the palette:\n%s", p.Semantics())
		return ggui.SemNode{}
	}
	if entry().Disabled {
		t.Error("running: the palette's entry is disabled")
	}
	status(sandbox.Stopping)
	if !entry().Disabled {
		t.Error("stopping: the palette's entry is enabled")
	}
	m.PaletteOpen.Set(false)

	// The engine stopped under the page: it stays, and says what to do.
	m.DockerContainers.Set(Listing[dockerapi.Container]{})
	status(sandbox.Stopped)
	if ggui.Peek(m.Page) != PageContainers {
		t.Errorf("the page closed: %s", ggui.Peek(m.Page))
	}
	if !shows("The Docker Engine is not running") || !shows("Start Docker Engine") {
		t.Errorf("the page does not tell the engine stopped:\n%s", p.Semantics())
	}
}
