package main

import (
	"fmt"
	"image/png"
	"os"
	"path/filepath"
	"time"

	"github.com/ironpark/ggfx"
	"github.com/ironpark/ggui"
	uitheme "github.com/ironpark/ggui/ui/theme"
	"github.com/ironpark/ggwd"

	"github.com/ironpark/gvx-desktop/internal/dockerapi"
	"github.com/ironpark/gvx-desktop/internal/recipes"
	"github.com/ironpark/gvx-desktop/internal/rungv"
	"github.com/ironpark/gvx-desktop/internal/sandbox"
	"github.com/ironpark/gvx-desktop/internal/store"
	"github.com/ironpark/gvx-desktop/internal/ui"
)

// renderScreens draws every page with demo data into PNG files, so the
// layout can be reviewed without a running engine or a screen capture.
func renderScreens(lang, out string) error {
	if err := os.MkdirAll(out, 0o755); err != nil {
		return err
	}
	tmp, err := os.MkdirTemp("", "gvxd-render")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmp)
	return offscreen(func() (bool, error) { return true, renderAll(tmp, out, lang) })
}

type shot struct {
	name  string
	setup func(m *ui.Model)
}

func renderAll(tmp, out, lang string) error {
	now := time.Date(2026, 11, 16, 15, 30, 0, 0, time.Local)
	restore := ggui.SetClock(func() time.Time { return now })
	defer restore()

	rungv.HomeDir = func() (string, error) { return demoHome, nil }
	shots := []shot{
		{"dashboard", func(m *ui.Model) { m.Page.Set(ui.PageDashboard) }},
		{"sandboxes-logs", func(m *ui.Model) { m.Page.Set(ui.PageSandboxes); m.Detail.Set(0) }},
		{"sandboxes-connect", func(m *ui.Model) { m.Page.Set(ui.PageSandboxes); m.Detail.Set(1) }},
		{"sandboxes-config", func(m *ui.Model) { m.Page.Set(ui.PageSandboxes); m.Detail.Set(2) }},
		{"images", func(m *ui.Model) { m.Page.Set(ui.PageImages) }},
		{"volumes", func(m *ui.Model) { m.Page.Set(ui.PageVolumes) }},
		{"docker", func(m *ui.Model) { m.Page.Set(ui.PageDocker) }},
		{"kubernetes", func(m *ui.Model) { m.Page.Set(ui.PageKubernetes) }},
		{"settings", func(m *ui.Model) { m.Page.Set(ui.PageSettings) }},
		{"editor", func(m *ui.Model) { m.Page.Set(ui.PageSandboxes); m.NewSandbox("postgres:17-alpine") }},
		{"pull-search", func(m *ui.Model) {
			m.Page.Set(ui.PageImages)
			m.OpenPull("nginx")
			m.SearchFor.Set("nginx")
			m.SearchResults.Set(demoSearch())
			m.SearchTagsOf.Set("nginx")
			m.SearchTags.Set([]string{"stable-alpine", "mainline-alpine", "alpine", "1.31", "1.31-alpine", "stable", "latest", "bookworm"})
		}},
		{"palette", func(m *ui.Model) { m.PaletteOpen.Set(true) }},
		{"sidebar-hidden", func(m *ui.Model) { m.Page.Set(ui.PageImages); m.SidebarNarrow.Set(true) }},
	}
	const w, h = 1280, 840
	for _, dark := range []bool{false, true} {
		mode := "light"
		if dark {
			mode = "dark"
		}
		for _, s := range shots {
			dir := filepath.Join(tmp, mode+"-"+s.name)
			st := store.Store{Dir: dir}
			if err := st.SaveSandboxes(demoSpecs()); err != nil {
				return err
			}
			theme := "light"
			if dark {
				theme = "dark"
			}
			set := store.DefaultSettings()
			set.Theme = theme
			set.Language = lang
			_ = st.SaveSettings(set)

			var m *ui.Model
			p := ggui.ProbeBuilder(func() ggui.Widget {
				return ggui.Provide(ggui.ReducedMotionKey, true, ui.Build(m))
			}, ggui.Sz(w, h))
			p.Setup(func() {
				m = ui.NewModel(st, dir)
				m.Offline = true
				m.Attach(p)
				demoState(m, now)
				preset := uitheme.Preset{Base: uitheme.BaseZinc, Accent: uitheme.AccentBlue}
				uitheme.Bind(ggui.Const(dark), preset.Dark(), preset.Light())
			})
			p.Frame()
			s.setup(m)
			for range 6 {
				p.Frame()
				now = now.Add(200 * time.Millisecond)
			}
			img := ggfx.NewImage(w, h)
			img.Fill(ggui.Untrack(uitheme.Use).Bg)
			p.Draw(img)
			err := savePNG(filepath.Join(out, fmt.Sprintf("%s-%s.png", mode, s.name)), img)
			img.Deallocate()
			p.Close()
			if err != nil {
				return err
			}
		}
	}
	return nil
}

var demoIDs = []string{"a1b2c3d4", "d0c4e7f1", "c8s9k3e2", "f00dfeed"}

func demoSpecs() []sandbox.Spec {
	t := time.Date(2026, 11, 10, 9, 0, 0, 0, time.Local)
	web := sandbox.Spec{ID: demoIDs[0], Name: "web", Image: "nginx:alpine", Ports: []string{"8080:80"},
		Workspace: "~/sites/blog", CreatedAt: t, ReadyPattern: "start worker process"}
	docker := recipes.DockerEngine()
	docker.ID, docker.CreatedAt = demoIDs[1], t.Add(time.Hour)
	k3s := recipes.KubernetesNode()
	k3s.ID, k3s.CreatedAt = demoIDs[2], t.Add(2*time.Hour)
	pg := sandbox.Spec{ID: demoIDs[3], Name: "postgres", Image: "postgres:17-alpine", Ports: []string{"5432:5432"},
		Env:    []string{"POSTGRES_PASSWORD=secret"},
		Mounts: []sandbox.Mount{{Type: sandbox.MountVolume, Source: "pgdata", Target: "/var/lib/postgresql/data"}},
		Memory: "1g", Checkpoint: true, CreatedAt: t.Add(3 * time.Hour)}
	return []sandbox.Spec{web, docker, k3s, pg}
}

func demoState(m *ui.Model, now time.Time) {
	// The uptimes read the model's clock, which starts at the real time.
	m.Now.Set(now)
	m.Update.Set(ui.Update{Tag: "v0.17.10002026110900", Page: rungv.ReleasesURL})
	m.Engine.Set(ui.Engine{
		Path:    "/Users/me/.gvx/bin/rungv",
		Version: rungv.Version{Version: "v0.16.10022026110900", Commit: "a9bfd06", Go: "go1.27.4 darwin/arm64"},
		Info: rungv.Info{OS: "darwin", Architecture: "arm64", NumCPU: 10, DefaultBackend: "hvf",
			AvailableBackends: []string{"hvf"}, HostLockdown: "seatbelt",
			LayerCacheDir: "/Users/me/Library/Caches/gvx/layers", VolumeDir: "/Users/me/Library/Caches/gvx/volumes",
			Images: 5, Volumes: 3},
		Checked: true,
	})
	m.States.Set(map[string]sandbox.State{
		demoIDs[0]: {Status: sandbox.Running, PID: 41234, StartedAt: now.Add(-42 * time.Minute), Platform: "hvf",
			Ports: []sandbox.PublishedPort{{Host: "127.0.0.1:8080", Guest: 80, Proto: "tcp"}}},
		demoIDs[1]: {Status: sandbox.Running, PID: 41301, StartedAt: now.Add(-3*time.Hour - 12*time.Minute), Platform: "hvf",
			Ports: []sandbox.PublishedPort{{Host: "/Users/me/.gvx/docker.sock", Guest: 2375, Proto: "tcp"},
				{Host: "127.0.0.1:9000", Guest: 9000, Proto: "tcp"}, {Host: "127.0.0.1:9001", Guest: 9001, Proto: "tcp"}}},
		demoIDs[2]: {Status: sandbox.Starting, PID: 41377, StartedAt: now.Add(-40 * time.Second), Platform: "hvf"},
		demoIDs[3]: {Status: sandbox.Failed, ExitCode: 1, ExitedAt: now.Add(-5 * time.Minute),
			Error:  "rungv run: listen tcp 127.0.0.1:5432: bind: address already in use",
			Advice: rungv.Diagnose("address already in use", 1)},
	})
	m.Select(demoIDs[0])
	logs := []string{
		"/docker-entrypoint.sh: Configuration complete; ready for start up",
		"2026/11/16 06:24:33 [notice] 1#1: using the \"epoll\" event method",
		"2026/11/16 06:24:33 [notice] 1#1: nginx/1.31.8",
		"2026/11/16 06:24:33 [notice] 1#1: OS: Linux 4.19.0-gvisor",
		"2026/11/16 06:24:33 [notice] 1#1: start worker processes",
		"2026/11/16 06:24:33 [notice] 1#1: start worker process 25",
		`10.0.2.15 - - [16/Nov/2026:06:24:35 +0000] "GET / HTTP/1.1" 200 896 "-" "curl/8.7.1" "-"`,
		`10.0.2.15 - - [16/Nov/2026:06:25:02 +0000] "GET /about HTTP/1.1" 200 1204 "-" "Mozilla/5.0" "-"`,
	}
	var lines []sandbox.LogLine
	add := func(s sandbox.Stream, text string) {
		lines = append(lines, sandbox.LogLine{Seq: int64(len(lines) + 1), Time: now.Add(-42*time.Minute + time.Duration(len(lines))*time.Second), Stream: s, Text: text})
	}
	add(sandbox.System, "── Starting web · 2026-11-16 14:48:00")
	add(sandbox.System, "$ rungv run --name web -p 8080:80 -workspace /Users/me/sites/blog nginx:alpine")
	add(sandbox.Stderr, "rungv: platform=hvf")
	add(sandbox.Stderr, "rungv: publishing 127.0.0.1:8080 -> 80/tcp")
	for _, l := range logs {
		add(sandbox.Stdout, l)
	}
	m.Logs.Set(lines)

	m.Images.Set(ui.Listing[rungv.Image]{Loaded: true, Items: []rungv.Image{
		{Repository: "nginx", Tag: "alpine", ID: "4a3c0f9e2b11", CreatedSince: "2 weeks ago", Size: "21.4MB"},
		{Repository: "docker", Tag: "dind", ID: "9b2e77d1c0aa", CreatedSince: "5 days ago", Size: "142MB"},
		{Repository: "rancher/k3s", Tag: "v1.31.4-k3s1", ID: "e81f3a5c9d02", CreatedSince: "11 months ago", Size: "71.2MB"},
		{Repository: "postgres", Tag: "17-alpine", ID: "1d0c9a8b7e66", CreatedSince: "3 weeks ago", Size: "104MB"},
		{Repository: "alpine", Tag: "3.20", ID: "ab3fe4defd29", CreatedSince: "7 months ago", Size: "9.155MB"},
	}})
	m.Volumes.Set(ui.Listing[rungv.Volume]{Loaded: true, Items: []rungv.Volume{
		{Name: "gvx-docker", Driver: "ext4lite", Size: "1.82GB", InUse: true, CreatedAt: "2026-11-10 10:00:00 +0900 KST", Mountpoint: "/Users/me/Library/Caches/gvx/volumes/gvx-docker.ext4"},
		{Name: "k3s", Driver: "ext4lite", Size: "412MB", InUse: true, CreatedAt: "2026-11-10 11:00:00 +0900 KST"},
		{Name: "pgdata", Driver: "ext4lite", Size: "64MB", CreatedAt: "2026-11-12 09:30:00 +0900 KST"},
	}})
	m.Disk.Set([]rungv.DiskUsage{
		{Type: "Images", TotalCount: 5, Active: 3, Size: "348MB", Reclaimable: "113MB"},
		{Type: "Local Volumes", TotalCount: 3, Active: 2, Size: "2.30GB", Reclaimable: "64MB"},
	})
	// docker has the gvx context and uses another; kubectl has not been set
	// up for the cluster.
	m.DockerLink.State.Set(ui.CLIContext{Checked: true, CLI: "/opt/homebrew/bin/docker", Exists: true, Host: m.DockerHost(),
		Current: "desktop-linux", Contexts: []string{"default", "desktop-linux", ui.DockerContextName}})
	m.KubeLink.State.Set(ui.CLIContext{Checked: true, CLI: "/opt/homebrew/bin/kubectl", Current: "kind-dev", Contexts: []string{"kind-dev"}})
	m.DockerVersion.Set(dockerapi.Version{Version: "29.9.3", APIVersion: "1.56", OS: "linux", Arch: "arm64", KernelVersion: "4.19.0-gvisor"})
	m.DockerContainers.Set(ui.Listing[dockerapi.Container]{Loaded: true, Items: []dockerapi.Container{
		{ID: "3f2a9c1b7d4e5f60", Names: []string{"/shop-web-1"}, Image: "nginx:alpine", State: "running", Status: "Up 2 hours",
			Ports: []dockerapi.Port{{PrivatePort: 80, PublicPort: 9000, Type: "tcp"}}},
		{ID: "8b7c6d5e4f3a2b10", Names: []string{"/shop-redis-1"}, Image: "redis:alpine", State: "running", Status: "Up 2 hours",
			Ports: []dockerapi.Port{{PrivatePort: 6379, Type: "tcp"}}},
		{ID: "1a2b3c4d5e6f7081", Names: []string{"/migrate"}, Image: "app:dev", State: "exited", Status: "Exited (0) 2 hours ago"},
	}})
}

// offscreen runs step on frames of a hidden window: the graphics driver
// draws and reads back pixels only inside a frame.
func offscreen(step func() (bool, error)) error {
	var window *ggwd.Window
	return ggwd.Run(ggwd.HandlerFunc(func(ev ggwd.Event) error {
		switch ev.(type) {
		case ggwd.StartEvent:
			w, err := ggwd.NewWindow(&ggwd.WindowOptions{Title: "render", Width: 64, Height: 64, Hidden: true})
			if err != nil {
				return err
			}
			window = w
		case ggwd.FrameEvent:
			done, err := step()
			if err != nil {
				return err
			}
			if done {
				return ggwd.Termination
			}
			window.RequestFrame()
		}
		return nil
	}), nil)
}

func savePNG(name string, img *ggfx.Image) (err error) {
	f, err := os.Create(name)
	if err != nil {
		return err
	}
	defer func() {
		if cerr := f.Close(); err == nil {
			err = cerr
		}
	}()
	return png.Encode(f, img)
}
