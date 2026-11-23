package ui

import (
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/ironpark/ggui"

	"github.com/ironpark/gvx-desktop/internal/dockerapi"
	"github.com/ironpark/gvx-desktop/internal/imagefs"
	"github.com/ironpark/gvx-desktop/internal/recipes"
	"github.com/ironpark/gvx-desktop/internal/rungv"
	"github.com/ironpark/gvx-desktop/internal/sandbox"
	"github.com/ironpark/gvx-desktop/internal/store"
)

func TestDockerImageRows(t *testing.T) {
	now := time.Now().Unix()
	imgs := []dockerapi.Image{
		{ID: "sha256:1111222233334444", RepoTags: []string{"app:dev", "localhost:5000/app:v2"}, Size: 100, Created: now},
		{ID: "sha256:aaaabbbbccccdddd", RepoTags: []string{"<none>:<none>"}, Size: 50, Created: now},
		{ID: "sha256:eeeeffff00001111", RepoTags: nil, RepoDigests: []string{"nginx@sha256:9999"}, Size: 75, Created: now},
	}
	rows := dockerImageRows(imgs)
	if len(rows) != 4 {
		t.Fatalf("got %d rows: %+v", len(rows), rows)
	}
	if rows[0].Repository != "app" || rows[0].Tag != "dev" || rows[0].Ref() != "app:dev" || rows[0].Target() != "app:dev" || !rows[0].Pullable() {
		t.Errorf("row 0: %+v", rows[0])
	}
	if rows[1].Repository != "localhost:5000/app" || rows[1].Tag != "v2" || rows[1].Ref() != "localhost:5000/app:v2" {
		t.Errorf("row 1: %+v", rows[1])
	}
	if rows[2].Repository != "<none>" || rows[2].Tag != "<none>" || !rows[2].Dangling || rows[2].Ref() != "aaaabbbbcccc" || rows[2].Target() != "sha256:aaaabbbbccccdddd" || rows[2].Pullable() {
		t.Errorf("row 2: %+v", rows[2])
	}
	if rows[3].Repository != "nginx" || rows[3].Tag != "<none>" || rows[3].Ref() != "nginx@sha256:9999" || rows[3].Target() != "sha256:eeeeffff00001111" || rows[3].Pullable() {
		t.Errorf("row 3: %+v", rows[3])
	}
	if got := dockerImagesTotalSize(imgs); got != 225 {
		t.Errorf("total size = %d, want 225 (no double counting)", got)
	}
}

func TestFilterAndUsersOfDockerImages(t *testing.T) {
	rows := dockerImageRows([]dockerapi.Image{
		{ID: "sha256:1111222233334444", RepoTags: []string{"nginx:alpine"}},
		{ID: "sha256:aaaabbbbccccdddd", RepoTags: []string{"myapp:latest"}},
	})
	if got := filterDockerImages(rows, "NGINX"); len(got) != 1 || got[0].Ref() != "nginx:alpine" {
		t.Errorf("filter by repo: %+v", got)
	}
	if got := filterDockerImages(rows, "aaaabbbb"); len(got) != 1 || got[0].Ref() != "myapp:latest" {
		t.Errorf("filter by id: %+v", got)
	}
	ctrs := []dockerapi.Container{
		{ID: "c1", Names: []string{"/web"}, Image: "nginx:alpine", ImageID: "sha256:1111222233334444"},
		{ID: "c2", Names: []string{"/worker"}, Image: "myapp"},
	}
	if got := dockerImageUsers(ctrs, rows[0]); !slices.Equal(got, []string{"web"}) {
		t.Errorf("users of nginx: %v", got)
	}
	if got := dockerImageUsers(ctrs, rows[1]); !slices.Equal(got, []string{"worker"}) {
		t.Errorf("users of myapp: %v", got)
	}
}

func TestImagesPageDockerTabFollowsEngine(t *testing.T) {
	dir := t.TempDir()
	st := store.Store{Dir: dir}
	docker := recipes.DockerEngine()
	if err := st.SaveSandboxes([]sandbox.Spec{docker}); err != nil {
		t.Fatal(err)
	}
	set := store.DefaultSettings()
	set.Language = LangEnglish
	set.SandboxEngine = false
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

	shows := func(text string) bool {
		for _, n := range p.Semantics().All() {
			if n.Name == text || strings.Contains(n.Name, text) {
				return true
			}
		}
		return false
	}

	m.GoImages(ImagesDocker)
	p.Frame()
	if !shows("The Docker Engine is not running") {
		t.Fatalf("expected stopped gate on Docker images tab:\n%s", p.Semantics())
	}

	m.States.Set(map[string]sandbox.State{docker.ID: {Status: sandbox.Running}})
	m.DockerContainers.Set(Listing[dockerapi.Container]{Loaded: true, Items: []dockerapi.Container{
		{ID: "c1", Names: []string{"/web"}, Image: "myapp:dev", ImageID: "sha256:1a2b3c4d5e6f7081"},
	}})
	m.DockerImages.Set(Listing[dockerapi.Image]{Loaded: true, Items: []dockerapi.Image{
		{ID: "sha256:1a2b3c4d5e6f7081", RepoTags: []string{"myapp:dev"}, Size: 64 << 20, Created: time.Now().Add(-time.Hour).Unix()},
	}})
	p.Frame()
	if !shows("myapp") || !shows("In use") {
		t.Errorf("expected Docker image row and In use badge:\n%s", p.Semantics())
	}
}

func TestSetSourceAndSettingsUI(t *testing.T) {
	dir := t.TempDir()
	st := store.Store{Dir: dir}
	set := store.DefaultSettings()
	set.Language = LangEnglish
	_ = st.SaveSettings(set)

	var m *Model
	p := ggui.ProbeBuilder(func() ggui.Widget { return ggui.Provide(ggui.ReducedMotionKey, true, Build(m)) }, ggui.Sz(1200, 900))
	p.Setup(func() {
		m = NewModel(st, dir)
		m.Offline = true
		m.Attach(p)
	})
	defer p.Close()
	p.Frame()

	m.Go(PageSettings)
	p.Frame()
	if _, ok := p.Find("Source repository"); ok {
		t.Fatalf("source fields shown while on release channel")
	}

	m.SetChannel(ChannelSource)
	p.Frame()
	if _, ok := p.Find("Source repository"); !ok {
		t.Fatalf("source repository field not shown on source channel:\n%s", p.Semantics())
	}
	if _, ok := p.Find("Source branch"); !ok {
		t.Fatalf("source branch field not shown on source channel:\n%s", p.Semantics())
	}

	m.SetSource("ironpark/gvx", "feat/test")
	p.Frame()
	saved, err := st.Settings()
	if err != nil {
		t.Fatal(err)
	}
	if saved.SourceRepo != "ironpark/gvx" || saved.SourceBranch != "feat/test" {
		t.Errorf("saved settings: repo=%q branch=%q", saved.SourceRepo, saved.SourceBranch)
	}
	if u := ggui.Peek(m.Update); u.Branch != "feat/test" || u.Repo != "ironpark/gvx" {
		t.Errorf("update state: %+v", u)
	}
	if _, ok := p.Find("Reset"); !ok {
		t.Errorf("expected Reset button when custom repo/branch is set")
	}

	// Resetting to defaults stores empty strings.
	m.SetSource("https://github.com/gfire-sigs/gvx", "main")
	p.Frame()
	saved, _ = st.Settings()
	if saved.SourceRepo != "" || saved.SourceBranch != "" {
		t.Errorf("expected defaults normalized to empty, got repo=%q branch=%q", saved.SourceRepo, saved.SourceBranch)
	}
}

func TestImageExplorerDialog(t *testing.T) {
	dir := t.TempDir()
	st := store.Store{Dir: dir}
	set := store.DefaultSettings()
	set.Language = LangEnglish
	_ = st.SaveSettings(set)

	var m *Model
	p := ggui.ProbeBuilder(func() ggui.Widget { return ggui.Provide(ggui.ReducedMotionKey, true, Build(m)) }, ggui.Sz(1200, 840))
	p.Setup(func() {
		m = NewModel(st, dir)
		m.Offline = true
		m.Attach(p)
	})
	defer p.Close()
	p.Frame()

	shows := func(text string) bool {
		for _, n := range p.Semantics().All() {
			if n.Name == text || strings.Contains(n.Name, text) {
				return true
			}
		}
		return false
	}

	m.ExploreTitle.Set("nginx:alpine")
	m.ExploreSource.Set("GVX")
	m.ExploreTab.Set(ExploreFiles)
	m.ExplorePath.Set("/etc/nginx")
	m.ExploreLayers.Set([]imagefs.Layer{
		{Index: 1, Digest: "sha256:111122223333", Size: 8 << 20, Command: "ADD alpine.tar.gz /", HasFiles: true},
		{Index: 2, Digest: "sha256:444455556666", Size: 14 << 20, Command: "RUN apk add nginx", HasFiles: true},
	})
	m.ExploreEntries.Set([]imagefs.Entry{
		{Name: "conf.d", Path: "/etc/nginx/conf.d", Kind: imagefs.KindDir, Mode: 0o040755, Layer: 2},
		{Name: "nginx.conf", Path: "/etc/nginx/nginx.conf", Kind: imagefs.KindFile, Mode: 0o100644, Size: 642, Layer: 2},
	})
	m.ExplorePreview.Set(FilePreview{
		HasEntry: true,
		Entry:    imagefs.Entry{Name: "nginx.conf", Path: "/etc/nginx/nginx.conf", Kind: imagefs.KindFile, Mode: 0o100644, Size: 642, Layer: 2},
		Body:     "worker_processes auto;\n",
		Language: "conf",
	})
	m.ExploreOpen.Set(true)
	p.Frame()

	if !shows("nginx:alpine") || !shows("nginx.conf") || !shows("conf.d") {
		t.Fatalf("explorer files tab did not render expected entries:\n%s", p.Semantics())
	}

	m.ExploreTab.Set(ExploreLayers)
	p.Frame()
	if !shows("RUN apk add nginx") || !shows("Browse") {
		t.Fatalf("explorer layers tab did not render expected layer:\n%s", p.Semantics())
	}
}

func TestRowActionMenuOpensInwardWithoutSquishing(t *testing.T) {
	dir := t.TempDir()
	st := store.Store{Dir: dir}
	set := store.DefaultSettings()
	set.Language = LangEnglish
	_ = st.SaveSettings(set)

	var m *Model
	p := ggui.ProbeBuilder(func() ggui.Widget { return ggui.Provide(ggui.ReducedMotionKey, true, Build(m)) }, ggui.Sz(1000, 700))
	p.Setup(func() {
		m = NewModel(st, dir)
		m.Offline = true
		m.Attach(p)
	})
	defer p.Close()
	p.Frame()

	m.Images.Set(Listing[rungv.Image]{Loaded: true, Items: []rungv.Image{
		{Repository: "nginx", Tag: "alpine", ID: "4a3c0f9e2b11", Size: "21.4MB"},
	}})
	m.GoImages(ImagesGVX)
	p.Frame()

	if _, ok := p.Semantics().Find(ggui.RoleMenu, "More actions for nginx:alpine"); !ok {
		t.Fatalf("no action menu button:\n%s", p.Semantics())
	}
	p.Tap("More actions for nginx:alpine")
	p.Frame()

	item, ok := p.Semantics().Find(ggui.RoleMenuItem, "Pull again")
	if !ok {
		t.Fatalf("menu did not open:\n%s", p.Semantics())
	}
	if item.Rect.Size.W < 180 {
		t.Errorf("menu item squished to width %.1f (rect %+v)", item.Rect.Size.W, item.Rect)
	}
	if item.Rect.Origin.X+item.Rect.Size.W > 1000 {
		t.Errorf("menu item overflows right window edge: %+v", item.Rect)
	}
}

func TestFormatSizeAndBytes(t *testing.T) {
	sizeCases := []struct{ in, want string }{
		{"", ""},
		{"0B", "0 B"},
		{"512B", "512 B"},
		{"12.34KB", "12.3 KB"},
		{"12KB", "12 KB"},
		{"9.155MB", "9.2 MB"},
		{"419.4MB", "419.4 MB"},
		{"322MB", "322 MB"},
		{"1.711GB", "1.71 GB"},
		{"2.114GB", "2.11 GB"},
		{"1.82GB", "1.82 GB"},
		{"2.30GB", "2.3 GB"},
		{"2.00GB", "2 GB"},
		{"113.45MB (32%)", "113.5 MB (32%)"},
		{"—", "—"},
	}
	for _, c := range sizeCases {
		if got := formatSize(c.in); got != c.want {
			t.Errorf("formatSize(%q) = %q, want %q", c.in, got, c.want)
		}
	}
	byteCases := []struct {
		in   int64
		want string
	}{
		{0, "0 B"},
		{642, "642 B"},
		{1500, "1.5 KB"},
		{64_000_000, "64 MB"},
		{1_711_276_032, "1.71 GB"},
		{2_000_000_000, "2 GB"},
	}
	for _, c := range byteCases {
		if got := formatBytes(c.in); got != c.want {
			t.Errorf("formatBytes(%d) = %q, want %q", c.in, got, c.want)
		}
	}
}
