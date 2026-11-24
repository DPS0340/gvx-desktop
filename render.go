package main

import (
	"fmt"
	"image/png"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/ironpark/ggfx"
	"github.com/ironpark/ggui"
	"github.com/ironpark/ggui/fonts/gofont"
	uitheme "github.com/ironpark/ggui/ui/theme"
	"github.com/ironpark/ggwd"

	"github.com/ironpark/gvx-desktop-vm/vmhelper"
	"github.com/ironpark/gvx-desktop/internal/appupdate"
	"github.com/ironpark/gvx-desktop/internal/dockerapi"
	"github.com/ironpark/gvx-desktop/internal/engine"
	"github.com/ironpark/gvx-desktop/internal/imagefs"
	"github.com/ironpark/gvx-desktop/internal/kubeapi"
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

// live reports whether what the shot shows comes in from goroutines, as a
// terminal's made-up shell prints: its frames wait a moment each.
func (s shot) live() bool {
	return strings.HasSuffix(s.name, "-terminal") || s.name == "containers-sandbox-engine"
}

// tall reports whether the shot is drawn taller than the window, to show a
// long page whole.
func (s shot) tall() bool {
	return strings.HasSuffix(s.name, "-whole") ||
		s.name == "labs-sandbox-engine" ||
		s.name == "labs-amd64-builder" ||
		s.name == "labs-amd64-requirements"
}

func renderAll(tmp, out, lang string) error {
	now := time.Date(2026, 11, 16, 15, 30, 0, 0, time.Local)
	restore := ggui.SetClock(func() time.Time { return now })
	defer restore()

	rungv.HomeDir = func() (string, error) { return demoHome, nil }
	// A release's version, which the update of the app is measured against.
	defer func(v string) { ui.AppVersion = v }(ui.AppVersion)
	ui.AppVersion = "0.5.1"
	shots := []shot{
		{"dashboard", func(m *ui.Model) { m.Page.Set(ui.PageDashboard) }},
		// The whole page, down to the recipes.
		{"dashboard-whole", func(m *ui.Model) { m.Page.Set(ui.PageDashboard) }},
		{"sandboxes-logs", func(m *ui.Model) { m.Page.Set(ui.PageSandboxes); m.Detail.Set(0) }},
		{"sandboxes-connect", func(m *ui.Model) { m.Page.Set(ui.PageSandboxes); m.Detail.Set(1) }},
		{"sandboxes-config", func(m *ui.Model) { m.Page.Set(ui.PageSandboxes); m.Detail.Set(2) }},
		{"sandboxes-terminal", func(m *ui.Model) { m.Page.Set(ui.PageSandboxes); m.Detail.Set(ui.SandboxTerminalTab) }},
		{"sandboxes-paused", func(m *ui.Model) { m.ShowSandbox(demoIDs[4], 0) }},
		{"containers-logs", func(m *ui.Model) { m.Go(ui.PageContainers); m.ContainerDetail.Set(0) }},
		{"containers-connect", func(m *ui.Model) { m.Go(ui.PageContainers); m.ContainerDetail.Set(1) }},
		// A terminal's shell is a made-up one: the model starts no process.
		{"containers-terminal", func(m *ui.Model) { m.Go(ui.PageContainers); m.ContainerDetail.Set(2) }},
		// A Compose project folded away, and one filtered to one of its
		// containers.
		// A container whose bind mount is of a folder the engine does not share.
		{"containers-unshared", func(m *ui.Model) {
			m.UpdateSettings(func(s *store.Settings) { s.SandboxEngine = false })
			m.Go(ui.PageContainers)
			m.SelectContainer(demoContainers()[2].ID)
			m.ContainerDetail.Set(1)
		}},
		{"containers-folded", func(m *ui.Model) { m.Go(ui.PageContainers); m.ToggleProject("shop") }},
		// Something on this computer has the port a container publishes.
		{"containers-port-busy", func(m *ui.Model) {
			m.UpdateSettings(func(s *store.Settings) { s.SandboxEngine = false })
			m.Go(ui.PageContainers)
			states := maps.Clone(ggui.Peek(m.States))
			st := states[demoIDs[1]]
			st.Ports = slices.DeleteFunc(slices.Clone(st.Ports), func(p sandbox.PublishedPort) bool { return p.Host == "127.0.0.1:9000" })
			st.Busy = []sandbox.PublishedPort{{Host: "127.0.0.1:9000", Guest: 9000, Proto: "tcp", Source: "auto"}}
			states[demoIDs[1]] = st
			m.States.Set(states)
			m.ContainerDetail.Set(1)
		}},
		// The engine's ports: its API socket, the range opened at start, one
		// -auto-publish published as a container listened, and one this
		// computer has.
		{"sandboxes-ports-auto-whole", func(m *ui.Model) {
			states := maps.Clone(ggui.Peek(m.States))
			st := states[demoIDs[1]]
			st.Ports = append(slices.Clone(st.Ports), sandbox.PublishedPort{Host: "127.0.0.1:3000", Guest: 3000, Proto: "tcp", Source: "auto"})
			st.Busy = []sandbox.PublishedPort{{Host: "127.0.0.1:5432", Guest: 5432, Proto: "tcp", Source: "auto"}}
			states[demoIDs[1]] = st
			m.States.Set(states)
			m.ShowSandbox(demoIDs[1], 1)
		}},
		// A container on two networks, with its addresses and names.
		{"containers-networks", func(m *ui.Model) {
			m.Go(ui.PageContainers)
			m.SelectContainer(demoContainers()[1].ID)
			m.ContainerDetail.Set(1)
		}},
		{"networks", func(m *ui.Model) { m.ShowNetwork("shop_default") }},
		// The default bridge in the sandbox engine, where containers do not
		// reach each other.
		{"networks-bridge-sandbox-engine", func(m *ui.Model) {
			stopEngine(m)
			m.UpdateSettings(func(s *store.Settings) { s.SandboxEngine = true })
			m.DockerContainers.Set(ui.Listing[dockerapi.Container]{Loaded: true, Items: demoContainers()})
			m.DockerNetworks.Set(ui.Listing[dockerapi.Network]{Loaded: true, Items: demoNetworks()})
			m.ShowNetwork("bridge")
		}},
		{"networks-remove", func(m *ui.Model) { m.ShowNetwork("scratch"); m.AskRemoveNetwork(demoNetworks()[5]) }},
		{"containers-filtered", func(m *ui.Model) { m.Go(ui.PageContainers); m.ContainerQuery.Set("redis") }},
		{"containers-remove-project", func(m *ui.Model) {
			m.Go(ui.PageContainers)
			ctrs := demoContainers()
			m.AskRemoveProject("shop", ctrs[:2])
		}},
		// The engine stopped while its containers were on screen: the page
		// says so, and the sidebar's item is greyed out.
		{"containers-stopped", func(m *ui.Model) {
			m.UpdateSettings(func(s *store.Settings) { s.SandboxEngine = false })
			m.Go(ui.PageContainers)
			stopEngine(m)
		}},
		// The engine paused while its containers were on screen: the page
		// offers Resume instead of polling the frozen VM.
		{"containers-paused-engine", func(m *ui.Model) {
			m.UpdateSettings(func(s *store.Settings) { s.SandboxEngine = false })
			m.Go(ui.PageContainers)
			states := maps.Clone(ggui.Peek(m.States))
			st := states[demoIDs[1]]
			st.Paused = true
			states[demoIDs[1]] = st
			m.States.Set(states)
		}},
		// Experimental sandbox engine mode: Containers page stays open with
		// the Docker Engine sandbox stopped, and Terminal runs an interactive
		// shell in the container via rungv exec.
		{"containers-sandbox-engine", func(m *ui.Model) {
			stopEngine(m)
			m.UpdateSettings(func(s *store.Settings) { s.SandboxEngine = true })
			m.DockerContainers.Set(ui.Listing[dockerapi.Container]{Loaded: true, Items: demoContainers()})
			m.ContainerLogs.Set(demoContainerLogs(ggui.Peek(m.Now)))
			m.Go(ui.PageContainers)
			m.ContainerDetail.Set(2)
		}},
		{"pods-logs", func(m *ui.Model) { m.Go(ui.PagePods); m.PodDetail.Set(0) }},
		{"pods-info", func(m *ui.Model) { m.Go(ui.PagePods); m.PodDetail.Set(1) }},
		{"pods-terminal", func(m *ui.Model) { m.Go(ui.PagePods); m.PodDetail.Set(2) }},
		// The system's pods shown, the first of them chosen.
		{"pods-system", func(m *ui.Model) {
			m.Go(ui.PagePods)
			m.SystemPods.Set(true)
			m.SelectPod("kube-system/local-path-provisioner-5cf85fd84d-x7k2p")
			m.PodDetail.Set(1)
		}},
		// k3s is up and its API server has yet to answer.
		{"pods-starting", func(m *ui.Model) {
			m.Go(ui.PagePods)
			m.PodsWaiting.Set(true)
			m.Pods.Set(ui.Listing[kubeapi.Pod]{})
		}},
		// The cluster stopped while its pods were on screen.
		{"pods-stopped", func(m *ui.Model) {
			m.Go(ui.PagePods)
			stopCluster(m)
		}},
		// The pods of one workload, asked for from its row.
		{"pods-of-workload", func(m *ui.Model) { m.ShowWorkloadPods(demoWorkloads(ggui.Peek(m.Now))[1]) }},
		{"pods-forward", func(m *ui.Model) {
			m.GoKube(ui.KubePods)
			m.AskForwardPod(ggui.Peek(m.Pod))
		}},
		{"workloads", func(m *ui.Model) { m.GoKube(ui.KubeWorkloads) }},
		{"workloads-system", func(m *ui.Model) { m.GoKube(ui.KubeWorkloads); m.SystemPods.Set(true) }},
		{"workloads-scale", func(m *ui.Model) {
			m.GoKube(ui.KubeWorkloads)
			m.AskScale(demoWorkloads(ggui.Peek(m.Now))[1])
		}},
		{"services", func(m *ui.Model) {
			m.GoKube(ui.KubeServices)
			m.Forwards.Set(demoForwards())
		}},
		{"services-system", func(m *ui.Model) { m.GoKube(ui.KubeServices); m.SystemPods.Set(true) }},
		// The cluster runs ServiceLB, and a LoadBalancer service has its
		// ports 80 and 443 on this computer.
		{"services-loadbalancer", func(m *ui.Model) {
			specs := slices.Clone(ggui.Peek(m.Specs))
			for i, s := range specs {
				if s.Recipe == recipes.Kubernetes {
					specs[i] = recipes.WithServiceLB(s, true)
				}
			}
			m.Specs.Set(specs)
			m.Services.Set(ui.Listing[kubeapi.Service]{Loaded: true, Items: append(demoServices(ggui.Peek(m.Now)), demoLoadBalancer(ggui.Peek(m.Now)))})
			m.GoKube(ui.KubeServices)
		}},
		{"services-forward", func(m *ui.Model) {
			m.GoKube(ui.KubeServices)
			m.Forwards.Set(demoForwards())
			m.AskForwardService(demoServices(ggui.Peek(m.Now))[3])
		}},
		{"images", func(m *ui.Model) { m.GoImages(ui.ImagesGVX) }},
		{"images-docker", func(m *ui.Model) { m.GoImages(ui.ImagesDocker) }},
		// Pushing an image to the cluster's registry while it is stopped.
		{"images-docker-push", func(m *ui.Model) {
			m.GoImages(ui.ImagesDocker)
			states := maps.Clone(ggui.Peek(m.States))
			states[demoIDs[4]] = sandbox.State{Status: sandbox.Stopped, ExitedAt: ggui.Peek(m.Now).Add(-time.Hour)}
			m.States.Set(states)
			m.PushToRegistry(ui.DockerImageRow{ID: "sha256:1a2b3c4d5e6f708192a3", Repository: "app", Tag: "dev"})
		}},
		{"sandboxes-registry", func(m *ui.Model) { m.ShowSandbox(demoIDs[4], 1) }},
		{"volumes", func(m *ui.Model) { m.Page.Set(ui.PageVolumes) }},
		{"snapshots", func(m *ui.Model) { m.Page.Set(ui.PageSnapshots) }},
		{"explorer", func(m *ui.Model) {
			m.GoImages(ui.ImagesGVX)
			m.ExploreTitle.Set("nginx:alpine")
			m.ExploreSource.Set("GVX")
			m.ExploreTab.Set(ui.ExploreFiles)
			m.ExplorePath.Set("/etc/nginx")
			m.ExploreLayers.Set([]imagefs.Layer{
				{Index: 1, Digest: "sha256:111122223333", Size: 8 << 20, Command: "ADD alpine-minirootfs.tar.gz /", HasFiles: true},
				{Index: 2, Digest: "sha256:444455556666", Size: 14 << 20, Command: "RUN apk add --no-cache nginx", HasFiles: true},
			})
			m.ExploreEntries.Set([]imagefs.Entry{
				{Name: "conf.d", Path: "/etc/nginx/conf.d", Kind: imagefs.KindDir, Mode: 0o040755, Layer: 2},
				{Name: "modules", Path: "/etc/nginx/modules", Kind: imagefs.KindSymlink, Target: "/usr/lib/nginx/modules", Mode: 0o120777, Layer: 2},
				{Name: "mime.types", Path: "/etc/nginx/mime.types", Kind: imagefs.KindFile, Mode: 0o100644, Size: 5349, Layer: 2},
				{Name: "nginx.conf", Path: "/etc/nginx/nginx.conf", Kind: imagefs.KindFile, Mode: 0o100644, Size: 642, Layer: 2},
			})
			m.ExploreSelected.Set("/etc/nginx/nginx.conf")
			m.ExplorePreview.Set(ui.FilePreview{
				HasEntry: true,
				Entry:    imagefs.Entry{Name: "nginx.conf", Path: "/etc/nginx/nginx.conf", Kind: imagefs.KindFile, Mode: 0o100644, Size: 642, Layer: 2},
				Body:     "user nginx;\nworker_processes auto;\n\nerror_log /var/log/nginx/error.log notice;\npid       /var/run/nginx.pid;\n\nevents {\n    worker_connections 1024;\n}\n",
				Language: "conf",
			})
			m.ExploreOpen.Set(true)
		}},
		{"docker", func(m *ui.Model) {
			m.BuildxStatus.Set(ui.BuildxStatus{Checked: true, Ready: true, Selected: true})
			m.BundledDocker.Set(ui.BundledDockerState{
				Available:  true,
				HasBuildx:  true,
				HasCompose: true,
				Using:      true,
				CLIPath:    demoHome + "/.gvx/bin/docker",
				BinDir:     demoHome + "/.gvx/bin",
				Profile:    demoHome + "/.zshrc",
				InPATH:     false,
			})
			m.Page.Set(ui.PageDocker)
		}},
		{"docker-compat", func(m *ui.Model) {
			m.UpdateSettings(func(s *store.Settings) { s.SandboxEngine = false })
			m.Page.Set(ui.PageDocker)
		}},
		{"kubernetes", func(m *ui.Model) { m.Page.Set(ui.PageKubernetes); m.Forwards.Set(demoForwards()) }},
		// A cluster of three nodes: the server and two agents, one of them
		// still joining.
		{"kubernetes-nodes", func(m *ui.Model) { demoNodes(m, now); m.Page.Set(ui.PageKubernetes) }},
		{"settings", func(m *ui.Model) { m.Page.Set(ui.PageSettings) }},
		// What is still experimental, under its own item above Settings.
		{"labs-whole", func(m *ui.Model) { m.Page.Set(ui.PageLabs) }},
		// The Docker Engine's daemon.json, as it starts when there is none.
		{"settings-daemon-json", func(m *ui.Model) {
			m.UpdateSettings(func(s *store.Settings) { s.SandboxEngine = false })
			m.Page.Set(ui.PageSettings)
			m.OpenDockerConfig()
		}},
		// The whole page, down to the recipes' settings.
		{"settings-whole", func(m *ui.Model) { m.Page.Set(ui.PageSettings) }},
		// The engine shares no folder.
		{"settings-unshared-whole", func(m *ui.Model) {
			m.UpdateSettings(func(s *store.Settings) { s.SandboxEngine = false })
			m.SetSharedFolders(nil)
			m.Page.Set(ui.PageSettings)
		}},
		// Labs with the sandbox engine on.
		{"labs-sandbox-engine", func(m *ui.Model) {
			m.UpdateSettings(func(s *store.Settings) { s.SandboxEngine = true })
			m.BuildxStatus.Set(ui.BuildxStatus{Checked: true, Ready: true, Selected: true})
			m.Page.Set(ui.PageLabs)
		}},
		// Experimental amd64 builder switch on, with VM status, disk size and buildx node.
		{"labs-amd64-builder", func(m *ui.Model) {
			m.UpdateSettings(func(s *store.Settings) {
				s.SandboxEngine = true
				s.Amd64Builds = true
			})
			m.BuildxStatus.Set(ui.BuildxStatus{Checked: true, Ready: true, Selected: true, Amd64Node: true})
			m.Amd64State.Set(engine.Amd64State{
				Enabled:   true,
				Status:    engine.BuilderRunning,
				Assets:    "1",
				DiskBytes: 1_420_000_000,
			})
			m.Page.Set(ui.PageLabs)
		}},
		// Experimental amd64 builder with Rosetta missing and Install Rosetta offered.
		{"labs-amd64-requirements", func(m *ui.Model) {
			m.UpdateSettings(func(s *store.Settings) { s.SandboxEngine = true })
			m.BuildxStatus.Set(ui.BuildxStatus{Checked: true, Ready: true, Selected: true})
			m.Amd64Check.Set(ui.Amd64CheckState{
				Checked: true,
				Result: vmhelper.CheckResult{
					AppleSilicon:   true,
					MacOS:          "15.4",
					Virtualization: true,
					Rosetta:        vmhelper.RosettaNotInstalled,
				},
			})
			m.Page.Set(ui.PageLabs)
		}},
		{"settings-source", func(m *ui.Model) {
			m.UpdateSettings(func(s *store.Settings) {
				s.UpdateChannel = ui.ChannelSource
				s.SourceRepo = "ironpark/gvx"
				s.SourceBranch = "feat/test"
			})
			m.Update.Set(ui.Update{Source: true, Repo: "ironpark/gvx", Branch: "feat/test", Commit: "e410633f24cf", Subject: "feat: test branch", Ahead: 2})
			m.Page.Set(ui.PageSettings)
		}},
		// A newer GVX Desktop: the banner offers it, downloading, downloaded,
		// and the question before the restart that puts it in place.
		{"dashboard-app-update", func(m *ui.Model) { demoAppUpdate(m, appupdate.Bundle, nil) }},
		{"dashboard-app-update-downloading", func(m *ui.Model) {
			demoAppUpdate(m, appupdate.Bundle, func(u *ui.AppUpdate) { u.Downloading, u.Percent = true, 42 })
		}},
		{"dashboard-app-update-restart", func(m *ui.Model) {
			demoAppUpdate(m, appupdate.Bundle, func(u *ui.AppUpdate) { u.Ready = true })
			m.AskRestartToUpdate()
		}},
		// The settings' group of the app's updates, as each install has it.
		{"settings-app-update-whole", func(m *ui.Model) {
			demoAppUpdate(m, appupdate.Bundle, func(u *ui.AppUpdate) { u.Ready = true })
			m.Page.Set(ui.PageSettings)
		}},
		{"settings-app-update-homebrew-whole", func(m *ui.Model) {
			demoAppUpdate(m, appupdate.Homebrew, nil)
			m.Page.Set(ui.PageSettings)
		}},
		{"settings-app-update-deb-whole", func(m *ui.Model) {
			demoAppUpdate(m, appupdate.Deb, nil)
			m.Page.Set(ui.PageSettings)
		}},
		{"settings-app-update-current-whole", func(m *ui.Model) {
			m.AppUpdate.Set(ui.AppUpdate{Tag: "v0.5.1", Page: appupdate.ReleasesURL, Install: appupdate.Install{Method: appupdate.Bundle}})
			m.Page.Set(ui.PageSettings)
		}},
		{"editor", func(m *ui.Model) { m.Page.Set(ui.PageSandboxes); m.NewSandbox("postgres:17-alpine") }},
		{"editor-live", func(m *ui.Model) {
			m.Page.Set(ui.PageSandboxes)
			m.EditSandbox(demoIDs[0])
			m.Draft.Update(func(d ui.Draft) ui.Draft {
				d.Memory = "512m"
				d.CPUs = "2"
				return d
			})
		}},
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
	const w = 1280
	for _, dark := range []bool{false, true} {
		mode := "light"
		if dark {
			mode = "dark"
		}
		for _, s := range shots {
			h := 840
			if s.tall() {
				h = 2000
			}
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
				// A probe has no system fonts: without a mono font of its
				// own the terminal would space Go Regular out over a grid.
				ggui.SetDefaultMonoFont(gofont.Mono())
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
				if s.live() {
					time.Sleep(50 * time.Millisecond)
				}
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

// demoAppUpdate has GVX Desktop 0.6.0 out, for the app installed by
// method, as change has the update.
func demoAppUpdate(m *ui.Model, method appupdate.Method, change func(*ui.AppUpdate)) {
	const page = appupdate.ReleasesURL + "/tag/v0.6.0"
	u := ui.AppUpdate{Tag: "v0.6.0", Page: page, Install: appupdate.Install{Method: method, Path: "/Applications/GVX Desktop.app"}}
	switch method {
	case appupdate.Homebrew:
		u.Command = u.Install.CommandForVersion("v0.6.0")
	case appupdate.Deb:
		u.Install.Path = "/usr/bin/gvx-desktop"
		dl := appupdate.ReleasesURL + "/download/v0.6.0/gvx-desktop_0.6.0_amd64.deb"
		u.Command = `cd "$(mktemp -d)" && curl -fL -O ` + dl + ` -O ` + dl + `.sha256 && sha256sum -c gvx-desktop_0.6.0_amd64.deb.sha256 && sudo apt install ./gvx-desktop_0.6.0_amd64.deb`
	}
	if change != nil {
		change(&u)
	}
	m.AppUpdate.Set(u)
}

var demoIDs = []string{"a1b2c3d4", "d0c4e7f1", "c8s9k3e2", "f00dfeed", "e9r5b001"}

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
	reg := recipes.ImageRegistry()
	reg.ID, reg.CreatedAt = demoIDs[4], t.Add(4*time.Hour)
	return []sandbox.Spec{web, docker, k3s, pg, reg}
}

// demoNodes grows the demo cluster by two agents.
func demoNodes(m *ui.Model, now time.Time) {
	specs := slices.Clone(ggui.Peek(m.Specs))
	var server sandbox.Spec
	for i, s := range specs {
		if s.Recipe == recipes.Kubernetes {
			specs[i] = recipes.WithNodeNetwork(s)
			server = specs[i]
		}
	}
	a1 := recipes.KubernetesAgentNode(server, 1, "K10demo::server:demo")
	a2 := recipes.KubernetesAgentNode(server, 2, "K10demo::server:demo")
	a1.ID, a2.ID = "a9e47001", "a9e47002"
	m.Specs.Set(append(specs, a1, a2))
	states := maps.Clone(ggui.Peek(m.States))
	states[a1.ID] = sandbox.State{Status: sandbox.Running, PID: 41510, StartedAt: now.Add(-2 * time.Hour), Platform: "hvf", Control: true}
	states[a2.ID] = sandbox.State{Status: sandbox.Running, PID: 41633, StartedAt: now.Add(-20 * time.Second), Platform: "hvf", Control: true}
	m.States.Set(states)
	node := func(name string, ready, server bool) kubeapi.Node {
		var n kubeapi.Node
		n.Metadata.Name = name
		if server {
			n.Metadata.Labels = map[string]string{"node-role.kubernetes.io/control-plane": "true"}
		}
		st := "False"
		if ready {
			st = "True"
		}
		n.Status.Conditions = []kubeapi.Condition{{Type: "Ready", Status: st}}
		n.Status.NodeInfo.KubeletVersion = "v1.31.4+k3s1"
		return n
	}
	m.KubeNodes.Set([]kubeapi.Node{node("k3s", true, true), node(a1.Name, true, false)})
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
	m.RungvFeatures.Set(rungv.Features{Control: true})
	m.States.Set(map[string]sandbox.State{
		demoIDs[0]: {Status: sandbox.Running, PID: 41234, StartedAt: now.Add(-42 * time.Minute), Platform: "hvf", Control: true,
			Ports: []sandbox.PublishedPort{{Host: "127.0.0.1:8080", Guest: 80, Proto: "tcp"}}},
		demoIDs[1]: {Status: sandbox.Running, PID: 41301, StartedAt: now.Add(-3*time.Hour - 12*time.Minute), Platform: "hvf", Control: true,
			Ports: []sandbox.PublishedPort{{Host: "/Users/me/.gvx/engine/dockerd.sock", Guest: 2375, Proto: "tcp"},
				{Host: "127.0.0.1:9000", Guest: 9000, Proto: "tcp"}, {Host: "127.0.0.1:9001", Guest: 9001, Proto: "tcp"}}},
		demoIDs[2]: {Status: sandbox.Running, PID: 41377, StartedAt: now.Add(-2*time.Hour - 10*time.Minute), Platform: "hvf", Control: true,
			Ports: []sandbox.PublishedPort{{Host: "127.0.0.1:6443", Guest: 6443, Proto: "tcp"}, {Host: "127.0.0.1:30000", Guest: 30000, Proto: "tcp"}}},
		demoIDs[4]: {Status: sandbox.Running, PID: 41420, StartedAt: now.Add(-50 * time.Minute), Platform: "hvf", Control: true, Paused: true,
			Ports: []sandbox.PublishedPort{{Host: "127.0.0.1:5001", Guest: 5000, Proto: "tcp"}}},
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
	m.BundledDocker.Set(ui.BundledDockerState{
		Available:  true,
		HasBuildx:  true,
		HasCompose: true,
		Using:      false,
		CLIPath:    demoHome + "/.gvx/bin/docker",
		BinDir:     demoHome + "/.gvx/bin",
		Profile:    demoHome + "/.zshrc",
		InPATH:     false,
	})
	m.KubeLink.State.Set(ui.CLIContext{Checked: true, CLI: "/opt/homebrew/bin/kubectl", Current: "kind-dev", Contexts: []string{"kind-dev"}})
	m.DockerVersion.Set(dockerapi.Version{Version: "29.9.3", APIVersion: "1.56", OS: "linux", Arch: "arm64", KernelVersion: "4.19.0-gvisor"})
	m.Amd64Check.Set(ui.Amd64CheckState{
		Checked: true,
		Result: vmhelper.CheckResult{
			AppleSilicon:   true,
			MacOS:          "15.4",
			Virtualization: true,
			Rosetta:        vmhelper.RosettaInstalled,
		},
	})
	m.DockerContainers.Set(ui.Listing[dockerapi.Container]{Loaded: true, Items: demoContainers()})
	m.DockerImages.Set(ui.Listing[dockerapi.Image]{Loaded: true, Items: demoDockerImages(now)})
	m.DockerNetworks.Set(ui.Listing[dockerapi.Network]{Loaded: true, Items: demoNetworks()})
	m.ContainerLogs.Set(demoContainerLogs(now))
	m.KubeconfigReady.Set(true)
	m.Pods.Set(ui.Listing[kubeapi.Pod]{Loaded: true, Items: demoPods(now)})
	m.Workloads.Set(ui.Listing[kubeapi.Workload]{Loaded: true, Items: demoWorkloads(now)})
	m.Services.Set(ui.Listing[kubeapi.Service]{Loaded: true, Items: demoServices(now)})
	m.SelectPod("default/web-7d4b9c8f6d-2m9qz")
	m.PodLogs.Set(demoPodLogs(now))
	demoUsage(m)
}

// demoUsage is what the demo's running sandboxes, containers and pods use,
// as the pages sample it while they show them. api's pod crashes, so
// metrics-server has no sample of it.
func demoUsage(m *ui.Model) {
	const mib = 1 << 20
	m.SandboxUsage.Set(map[string]ui.Usage{
		demoIDs[4]: {CPU: 0.1, HasCPU: true, Memory: 14 * mib},
		demoIDs[0]: {CPU: 0.8, HasCPU: true, Memory: 46 * mib},
		demoIDs[1]: {CPU: 12.4, HasCPU: true, Memory: 612 * mib},
		demoIDs[2]: {CPU: 38.6, HasCPU: true, Memory: 1516 * mib},
	})
	ctrs := demoContainers()
	m.ContainerUsage.Set(map[string]ui.Usage{
		ctrs[0].ID: {CPU: 0.3, HasCPU: true, Memory: 8 * mib, Limit: 7845 * mib},
		ctrs[1].ID: {CPU: 1.2, HasCPU: true, Memory: 9 * mib, Limit: 7845 * mib},
	})
	use := func(milli float64, mem int64) kubeapi.Usage { return kubeapi.Usage{MilliCPU: milli, Memory: mem * mib} }
	pod := func(ctrs ...any) ui.PodUse {
		u := ui.PodUse{Containers: map[string]kubeapi.Usage{}}
		for i := 0; i < len(ctrs); i += 2 {
			c := ctrs[i+1].(kubeapi.Usage)
			u.Containers[ctrs[i].(string)] = c
			u.Total = u.Total.Add(c)
		}
		return u
	}
	m.PodUsage.Set(map[string]ui.PodUse{
		"default/web-7d4b9c8f6d-2m9qz":                        pod("web", use(2, 6), "exporter", use(1, 10)),
		"default/web-7d4b9c8f6d-hv5tc":                        pod("web", use(1, 5), "exporter", use(1, 9)),
		"shop/redis-0":                                        pod("redis", use(3, 8)),
		"kube-system/coredns-ccb96694c-7xk2m":                 pod("coredns", use(4, 18)),
		"kube-system/local-path-provisioner-5cf85fd84d-x7k2p": pod("local-path-provisioner", use(1, 10)),
		"kube-system/metrics-server-5985cbc9d7-q4n8w":         pod("metrics-server", use(6, 25)),
	})
}

// stopCluster stops the demo's k3s cluster, as its sandbox and the model
// would tell it.
func stopCluster(m *ui.Model) {
	states := maps.Clone(ggui.Peek(m.States))
	states[demoIDs[2]] = sandbox.State{Status: sandbox.Stopped, ExitedAt: ggui.Peek(m.Now).Add(-time.Minute)}
	m.States.Set(states)
	m.Pods.Set(ui.Listing[kubeapi.Pod]{})
	m.Workloads.Set(ui.Listing[kubeapi.Workload]{})
	m.Services.Set(ui.Listing[kubeapi.Service]{})
	m.PodLogs.Set(ui.ContainerLogs{})
}

// demoPods are the demo cluster's: k3s's own in kube-system (traefik and
// servicelb are off in the recipe), a Deployment of two replicas, one
// that crashes, a Job that is done, and a StatefulSet.
func demoPods(now time.Time) []kubeapi.Pod {
	up := now.Add(-2 * time.Hour)
	running := kubeapi.ContainerState{Running: &kubeapi.Running{Started: up}}
	ready := []kubeapi.Condition{{Type: "Initialized", Status: "True"}, {Type: "Ready", Status: "True"}, {Type: "PodScheduled", Status: "True"}}
	ctr := func(name, image string, ports ...kubeapi.ContainerPort) kubeapi.Container {
		return kubeapi.Container{Name: name, Image: image, Ports: ports}
	}
	port := func(n int, name string) kubeapi.ContainerPort {
		return kubeapi.ContainerPort{Name: name, Port: n, Protocol: "TCP"}
	}
	pod := func(ns, name, kind, owner, hash string, created time.Time, ip string, spec []kubeapi.Container, st kubeapi.PodStatus) kubeapi.Pod {
		labels := map[string]string{}
		if hash != "" {
			labels["pod-template-hash"] = hash
		}
		p := kubeapi.Pod{
			Metadata: kubeapi.Meta{Namespace: ns, Name: name, Created: created, Labels: labels},
			Spec:     kubeapi.PodSpec{Node: "k3s", Containers: spec},
			Status:   st,
		}
		if kind != "" {
			p.Metadata.Owners = []kubeapi.OwnerRef{{Kind: kind, Name: owner, Controller: true}}
		}
		p.Status.PodIP = ip
		return p
	}
	ok := func(names ...string) kubeapi.PodStatus {
		st := kubeapi.PodStatus{Phase: "Running", Conditions: ready}
		for _, n := range names {
			st.Containers = append(st.Containers, kubeapi.ContainerStatus{Name: n, Ready: true, State: running})
		}
		return st
	}
	cluster := up.Add(-3 * 24 * time.Hour)
	sys := func(name, owner, hash, image, ip string) kubeapi.Pod {
		p := pod("kube-system", name, "ReplicaSet", owner+"-"+hash, hash, cluster, ip, []kubeapi.Container{ctr(owner, image)}, ok(owner))
		p.Metadata.Labels["k8s-app"] = strings.Replace(owner, "coredns", "kube-dns", 1)
		return p
	}
	web := func(name, ip string) kubeapi.Pod {
		p := pod("default", name, "ReplicaSet", "web-7d4b9c8f6d", "7d4b9c8f6d", up.Add(-20*time.Minute), ip,
			[]kubeapi.Container{ctr("web", "nginx:alpine", port(80, "http")), ctr("exporter", "nginx/nginx-prometheus-exporter:1.4", port(9113, "metrics"))}, ok("web", "exporter"))
		p.Metadata.Labels["app"] = "web"
		return p
	}
	api := pod("default", "api-6f8d5b7c9-r2xwl", "ReplicaSet", "api-6f8d5b7c9", "6f8d5b7c9", now.Add(-25*time.Minute), "10.42.0.17",
		[]kubeapi.Container{ctr("api", "ghcr.io/acme/api:dev", port(8080, "http"))}, kubeapi.PodStatus{Phase: "Running",
			Conditions: []kubeapi.Condition{{Type: "Ready", Status: "False", Reason: "ContainersNotReady"}},
			Containers: []kubeapi.ContainerStatus{{Name: "api", Restarts: 6,
				State: kubeapi.ContainerState{Waiting: &kubeapi.Waiting{Reason: "CrashLoopBackOff",
					Message: "back-off 5m0s restarting failed container=api pod=api-6f8d5b7c9-r2xwl_default"}},
				Last: kubeapi.ContainerState{Terminated: &kubeapi.Terminated{Reason: "Error", ExitCode: 1, Finished: now.Add(-3 * time.Minute)}}}}})
	api.Metadata.Labels["app"] = "api"
	migrate := pod("default", "migrate-db-8xq4v", "Job", "migrate-db", "", up.Add(-15*time.Minute), "",
		[]kubeapi.Container{ctr("migrate", "app:dev")}, kubeapi.PodStatus{Phase: "Succeeded",
			Containers: []kubeapi.ContainerStatus{{Name: "migrate",
				State: kubeapi.ContainerState{Terminated: &kubeapi.Terminated{Reason: "Completed", Finished: up.Add(-14 * time.Minute)}}}}})
	redis := pod("shop", "redis-0", "StatefulSet", "redis", "", up.Add(-30*time.Minute), "10.42.0.9",
		[]kubeapi.Container{ctr("redis", "redis:7-alpine", port(6379, "redis"))}, ok("redis"))
	redis.Metadata.Labels["app"] = "redis"
	return []kubeapi.Pod{
		api, migrate, web("web-7d4b9c8f6d-2m9qz", "10.42.0.14"), web("web-7d4b9c8f6d-hv5tc", "10.42.0.15"), redis,
		sys("coredns-ccb96694c-7xk2m", "coredns", "ccb96694c", "rancher/mirrored-coredns-coredns:1.12.0", "10.42.0.2"),
		sys("local-path-provisioner-5cf85fd84d-x7k2p", "local-path-provisioner", "5cf85fd84d", "rancher/local-path-provisioner:v0.0.30", "10.42.0.3"),
		sys("metrics-server-5985cbc9d7-q4n8w", "metrics-server", "5985cbc9d7", "rancher/mirrored-metrics-server:v0.7.2", "10.42.0.4"),
	}
}

// demoWorkloads are what keeps the demo's pods: web's Deployment of two,
// api's of one that crashes, redis's StatefulSet, the Job migrate-db ran
// to completion, and k3s's own Deployments in kube-system.
func demoWorkloads(now time.Time) []kubeapi.Workload {
	up := now.Add(-2 * time.Hour)
	cluster := up.Add(-3 * 24 * time.Hour)
	w := func(kind, ns, name string, created time.Time, desired, ready int, images ...string) kubeapi.Workload {
		return kubeapi.Workload{Kind: kind, Metadata: kubeapi.Meta{Namespace: ns, Name: name, Created: created},
			Desired: desired, Ready: ready, UpToDate: desired, Available: ready, Images: images}
	}
	migrate := w(kubeapi.KindJob, "default", "migrate-db", up.Add(-15*time.Minute), 1, 1, "app:dev")
	migrate.UpToDate, migrate.Available, migrate.JobState = 0, 0, "Complete"
	sys := func(name, image string) kubeapi.Workload {
		return w(kubeapi.KindDeployment, "kube-system", name, cluster, 1, 1, image)
	}
	return []kubeapi.Workload{
		w(kubeapi.KindDeployment, "default", "api", now.Add(-25*time.Minute), 1, 0, "ghcr.io/acme/api:dev"),
		w(kubeapi.KindDeployment, "default", "web", up.Add(-20*time.Minute), 2, 2, "nginx:alpine", "nginx/nginx-prometheus-exporter:1.4"),
		migrate,
		w(kubeapi.KindStatefulSet, "shop", "redis", up.Add(-30*time.Minute), 1, 1, "redis:7-alpine"),
		sys("coredns", "rancher/mirrored-coredns-coredns:1.12.0"),
		sys("local-path-provisioner", "rancher/local-path-provisioner:v0.0.30"),
		sys("metrics-server", "rancher/mirrored-metrics-server:v0.7.2"),
	}
}

// demoServices lead to the demo's pods: the API server's own, web's
// NodePort in the range the sandbox publishes, api's outside it, redis's
// ClusterIP, and k3s's own in kube-system. traefik and servicelb are off
// in the recipe, so no LoadBalancer has an address.
func demoServices(now time.Time) []kubeapi.Service {
	up := now.Add(-2 * time.Hour)
	cluster := up.Add(-3 * 24 * time.Hour)
	tcp := func(port int, target kubeapi.IntOrString, node int) kubeapi.ServicePort {
		return kubeapi.ServicePort{Protocol: "TCP", Port: port, TargetPort: target, NodePort: node}
	}
	num := func(n int) kubeapi.IntOrString { return kubeapi.IntOrString{Int: n} }
	named := func(s string) kubeapi.IntOrString { return kubeapi.IntOrString{Str: s} }
	svc := func(ns, name, typ, ip string, created time.Time, sel map[string]string, ports ...kubeapi.ServicePort) kubeapi.Service {
		return kubeapi.Service{Metadata: kubeapi.Meta{Namespace: ns, Name: name, Created: created},
			Spec: kubeapi.ServiceSpec{Type: typ, ClusterIP: ip, Selector: sel, Ports: ports}}
	}
	dns := tcp(53, num(53), 0)
	dns.Name, dns.Protocol = "dns", "UDP"
	return []kubeapi.Service{
		svc("default", "api", "NodePort", "10.43.88.21", now.Add(-25*time.Minute), map[string]string{"app": "api"}, tcp(8080, named("http"), 30080)),
		svc("default", "kubernetes", "ClusterIP", "10.43.0.1", cluster, nil, tcp(443, num(6443), 0)),
		svc("default", "web", "NodePort", "10.43.12.7", up.Add(-20*time.Minute), map[string]string{"app": "web"},
			tcp(80, named("http"), 30000), tcp(9113, named("metrics"), 30001)),
		svc("shop", "redis", "ClusterIP", "10.43.150.3", up.Add(-30*time.Minute), map[string]string{"app": "redis"}, tcp(6379, num(6379), 0)),
		svc("kube-system", "kube-dns", "ClusterIP", "10.43.0.10", cluster, map[string]string{"k8s-app": "kube-dns"},
			dns, tcp(53, num(53), 0), tcp(9153, num(9153), 0)),
		svc("kube-system", "metrics-server", "ClusterIP", "10.43.40.88", cluster, map[string]string{"k8s-app": "metrics-server"}, tcp(443, named("https"), 0)),
	}
}

// demoLoadBalancer is a LoadBalancer service ServiceLB has given the
// node's address, on ports 80 and 443.
func demoLoadBalancer(now time.Time) kubeapi.Service {
	s := kubeapi.Service{Metadata: kubeapi.Meta{Namespace: "shop", Name: "storefront", Created: now.Add(-40 * time.Minute)},
		Spec: kubeapi.ServiceSpec{Type: "LoadBalancer", ClusterIP: "10.43.77.5", Selector: map[string]string{"app": "web"}, Ports: []kubeapi.ServicePort{
			{Name: "http", Protocol: "TCP", Port: 80, TargetPort: kubeapi.IntOrString{Str: "http"}, NodePort: 31380},
			{Name: "https", Protocol: "TCP", Port: 443, TargetPort: kubeapi.IntOrString{Int: 8443}, NodePort: 31443},
		}}}
	s.Status.LoadBalancer.Ingress = append(s.Status.LoadBalancer.Ingress, struct {
		IP       string `json:"ip,omitempty"`
		Hostname string `json:"hostname,omitempty"`
	}{IP: "10.0.2.15"})
	return s
}

// demoForwards are the port forwards the demo runs: one to a web pod, one
// to redis's service whose last connection failed.
func demoForwards() []ui.PortForward {
	return []ui.PortForward{
		{ID: 1, Target: kubeapi.Target{Namespace: "default", Pod: "web-7d4b9c8f6d-2m9qz", Port: 80}, Local: 8080, Served: 12},
		{ID: 2, Target: kubeapi.Target{Namespace: "shop", Service: "redis", Port: 6379}, Local: 6379, Served: 3,
			Err: "error forwarding port 6379 to pod redis-0, uid : failed to connect to localhost:6379 inside namespace: connection refused"},
	}
}

// demoPodLogs is what the web container of the demo's first web pod wrote.
func demoPodLogs(now time.Time) ui.ContainerLogs {
	text := []string{
		"/docker-entrypoint.sh: Configuration complete; ready for start up",
		"2026/11/16 06:10:02 [notice] 1#1: nginx/1.31.8",
		"2026/11/16 06:10:02 [notice] 1#1: start worker processes",
		`10.42.0.1 - - [16/Nov/2026:06:12:44 +0000] "GET / HTTP/1.1" 200 615 "-" "kube-probe/1.31" "-"`,
		`10.42.0.17 - - [16/Nov/2026:06:14:09 +0000] "GET /api/health HTTP/1.1" 502 157 "-" "curl/8.7.1" "-"`,
		`10.42.0.1 - - [16/Nov/2026:06:15:44 +0000] "GET / HTTP/1.1" 200 615 "-" "kube-probe/1.31" "-"`,
		`127.0.0.1 - - [16/Nov/2026:06:18:30 +0000] "GET /stub_status HTTP/1.1" 200 97 "-" "NGINX-Prometheus-Exporter/v1.4.0" "-"`,
		`10.42.0.1 - - [16/Nov/2026:06:18:44 +0000] "GET / HTTP/1.1" 200 615 "-" "kube-probe/1.31" "-"`,
	}
	l := ui.ContainerLogs{ID: "default/web-7d4b9c8f6d-2m9qz/web", Loaded: true}
	start := now.Add(-20 * time.Minute)
	for i, t := range text {
		l.Lines = append(l.Lines, sandbox.LogLine{Seq: int64(i + 1), Time: start.Add(time.Duration(i) * 2 * time.Minute), Stream: sandbox.Stdout, Text: t})
	}
	return l
}

// stopEngine stops the demo's Docker Engine, as its sandbox and the model
// would tell it.
func stopEngine(m *ui.Model) {
	states := maps.Clone(ggui.Peek(m.States))
	states[demoIDs[1]] = sandbox.State{Status: sandbox.Stopped, ExitedAt: ggui.Peek(m.Now).Add(-time.Minute)}
	m.States.Set(states)
	m.DockerContainers.Set(ui.Listing[dockerapi.Container]{})
	m.DockerImages.Set(ui.Listing[dockerapi.Image]{})
	m.DockerNetworks.Set(ui.Listing[dockerapi.Network]{})
	m.ContainerLogs.Set(ui.ContainerLogs{})
}

// demoDockerImages are the images inside the demo Docker Engine sandbox.
func demoDockerImages(now time.Time) []dockerapi.Image {
	return []dockerapi.Image{
		{ID: "sha256:4a3c0f9e2b118a7d6c5e", RepoTags: []string{"nginx:alpine"}, Size: 48 * 1024 * 1024, Created: now.Add(-6 * 24 * time.Hour).Unix()},
		{ID: "sha256:8b7c6d5e4f3a2b109876", RepoTags: []string{"redis:alpine"}, Size: 42 * 1024 * 1024, Created: now.Add(-10 * 24 * time.Hour).Unix()},
		{ID: "sha256:1a2b3c4d5e6f708192a3", RepoTags: []string{"app:dev", "app:latest"}, Size: 164 * 1024 * 1024, Created: now.Add(-3 * time.Hour).Unix()},
		{
			ID: "sha256:7c6b5a4938271605abcd", RepoTags: []string{"ghcr.io/acme/api:amd64"}, Size: 118 * 1024 * 1024, Created: now.Add(-2 * time.Hour).Unix(),
			Manifests: []dockerapi.ImageManifest{{
				ID: "sha256:7c6b5a4938271605abcd", Available: true, Kind: "image",
				ImageData: &dockerapi.ImageManifestData{Platform: dockerapi.ImagePlatform{Architecture: "amd64", OS: "linux"}},
			}},
		},
		{ID: "sha256:9f8e7d6c5b4a3210fedc", RepoTags: []string{"<none>:<none>"}, Size: 27 * 1024 * 1024, Created: now.Add(-2 * 24 * time.Hour).Unix()},
	}
}

// demoContainerLogs is what the demo's shop-web-1 wrote.
func demoContainerLogs(now time.Time) ui.ContainerLogs {
	text := []string{
		"/docker-entrypoint.sh: /docker-entrypoint.d/ is not empty, will attempt to perform configuration",
		"/docker-entrypoint.sh: Launching /docker-entrypoint.d/10-listen-on-ipv6-by-default.sh",
		"10-listen-on-ipv6-by-default.sh: info: Enabled listen on IPv6 in /etc/nginx/conf.d/default.conf",
		"/docker-entrypoint.sh: Configuration complete; ready for start up",
		"2026/11/16 04:18:02 [notice] 1#1: nginx/1.31.8",
		"2026/11/16 04:18:02 [notice] 1#1: start worker processes",
		`172.18.0.1 - - [16/Nov/2026:06:12:44 +0000] "GET / HTTP/1.1" 200 615 "-" "Mozilla/5.0" "-"`,
		`172.18.0.1 - - [16/Nov/2026:06:12:44 +0000] "GET /favicon.ico HTTP/1.1" 404 153 "-" "Mozilla/5.0" "-"`,
		`2026/11/16 06:12:44 [error] 29#29: *2 open() "/usr/share/nginx/html/favicon.ico" failed (2: No such file or directory)`,
		`172.18.0.1 - - [16/Nov/2026:06:20:03 +0000] "GET /cart HTTP/1.1" 200 1204 "-" "curl/8.7.1" "-"`,
	}
	l := ui.ContainerLogs{ID: demoContainers()[0].ID, Loaded: true}
	start := now.Add(-2 * time.Hour)
	for i, t := range text {
		stream := sandbox.Stdout
		if strings.Contains(t, "[error]") || strings.Contains(t, "[notice]") {
			stream = sandbox.Stderr
		}
		l.Lines = append(l.Lines, sandbox.LogLine{Seq: int64(i + 1), Time: start.Add(time.Duration(i) * 7 * time.Minute), Stream: stream, Text: t})
	}
	return l
}

// demoContainers are the demo Docker Engine's.
func demoContainers() []dockerapi.Container {
	created := time.Date(2026, 11, 16, 13, 18, 0, 0, time.Local).Unix()
	ctrs := []dockerapi.Container{
		{ID: "3f2a9c1b7d4e5f60", Names: []string{"/shop-web-1"}, Image: "nginx:alpine", State: "running", Status: "Up 2 hours", Created: created,
			Ports:  []dockerapi.Port{{IP: "0.0.0.0", PrivatePort: 80, PublicPort: 9000, Type: "tcp"}, {IP: "::", PrivatePort: 80, PublicPort: 9000, Type: "tcp"}},
			Labels: map[string]string{"com.docker.compose.project": "shop", "com.docker.compose.service": "web"},
			Mounts: []dockerapi.Mount{{Type: "bind", Source: "/Users/me/shop/site", Destination: "/usr/share/nginx/html", RW: true}}},
		{ID: "8b7c6d5e4f3a2b10", Names: []string{"/shop-redis-1"}, Image: "redis:alpine", State: "running", Status: "Up 2 hours", Created: created,
			Ports:  []dockerapi.Port{{PrivatePort: 6379, Type: "tcp"}},
			Labels: map[string]string{"com.docker.compose.project": "shop", "com.docker.compose.service": "redis"}},
		// A bind mount of a folder the engine does not share.
		{ID: "1a2b3c4d5e6f7081", Names: []string{"/migrate"}, Image: "app:dev", State: "exited", Status: "Exited (0) 2 hours ago", Created: created - 60,
			Mounts: []dockerapi.Mount{{Type: "bind", Source: "/Volumes/work/migrations", Destination: "/migrations", RW: true}}},
	}
	ctrs[0].NetworkSettings.Networks = map[string]dockerapi.Endpoint{
		"shop_default": {NetworkID: demoShopNet, IPAddress: "10.89.1.2", IPPrefixLen: 24, Gateway: "10.89.1.1", Aliases: []string{"web", "shop-web-1"}, DNSNames: []string{"shop-web-1", "web", "3f2a9c1b7d4e"}},
	}
	ctrs[1].NetworkSettings.Networks = map[string]dockerapi.Endpoint{
		"shop_default": {NetworkID: demoShopNet, IPAddress: "10.89.1.3", IPPrefixLen: 24, Gateway: "10.89.1.1", Aliases: []string{"redis", "shop-redis-1"}, DNSNames: []string{"shop-redis-1", "redis", "8b7c6d5e4f3a"}},
		"shop_cache":   {NetworkID: demoCacheNet, IPAddress: "10.89.2.2", IPPrefixLen: 24, Gateway: "10.89.2.1", DNSNames: []string{"shop-redis-1", "8b7c6d5e4f3a"}},
	}
	ctrs[2].NetworkSettings.Networks = map[string]dockerapi.Endpoint{
		"bridge": {IPAddress: "172.17.0.4", IPPrefixLen: 16, Gateway: "172.17.0.1"},
	}
	return ctrs
}

const (
	demoShopNet  = "9c1e4b7a2d5f80316e2b9a4c7d0f1e3a5b8c6d2e4f7a9b1c3d5e7f9a1b3c5d7e"
	demoCacheNet = "4d2f8a6c1e3b5907d4c2a8e6f1b3d5c79e1a3c5e7b9d1f3a5c7e9b1d3f5a7c9e"
)

func demoNetCreated(t time.Time) string { return t.Format(time.RFC3339Nano) }

// demoNetworks are the demo engine's networks: the built-in ones, the shop
// project's and an internal one of its, and one no container is on.
func demoNetworks() []dockerapi.Network {
	mk := func(name, id, driver, created, subnet, gateway string, members map[string]dockerapi.NetworkMember, labels map[string]string) dockerapi.Network {
		n := dockerapi.Network{Name: name, ID: id, Driver: driver, Created: created, Scope: "local", Containers: members, Labels: labels}
		if subnet != "" {
			n.IPAM.Config = append(n.IPAM.Config, struct {
				Subnet  string `json:"Subnet"`
				Gateway string `json:"Gateway"`
			}{subnet, gateway})
		}
		return n
	}
	shop := map[string]string{"com.docker.compose.project": "shop", "com.docker.compose.network": "default"}
	nets := []dockerapi.Network{
		mk("bridge", "b1d2e3f4a5b6c7d8e9f0a1b2c3d4e5f6a7b8c9d0e1f2a3b4c5d6e7f8a9b0c1d2", "bridge", "", "172.17.0.0/16", "172.17.0.1",
			map[string]dockerapi.NetworkMember{"1a2b3c4d5e6f7081": {Name: "migrate", IPv4Address: "172.17.0.4/16"}}, nil),
		mk("host", "c2d3e4f5a6b7c8d9e0f1a2b3c4d5e6f7a8b9c0d1e2f3a4b5c6d7e8f9a0b1c2d3", "host", "", "", "", nil, nil),
		mk("none", "d3e4f5a6b7c8d9e0f1a2b3c4d5e6f7a8b9c0d1e2f3a4b5c6d7e8f9a0b1c2d3e4", "null", "", "", "", nil, nil),
		mk("shop_default", demoShopNet, "bridge", demoNetCreated(time.Date(2026, 11, 16, 13, 18, 0, 0, time.Local)), "10.89.1.0/24", "10.89.1.1", map[string]dockerapi.NetworkMember{
			"3f2a9c1b7d4e5f60": {Name: "shop-web-1", IPv4Address: "10.89.1.2/24"},
			"8b7c6d5e4f3a2b10": {Name: "shop-redis-1", IPv4Address: "10.89.1.3/24"},
		}, shop),
		mk("shop_cache", demoCacheNet, "bridge", demoNetCreated(time.Date(2026, 11, 16, 13, 18, 0, 0, time.Local)), "10.89.2.0/24", "10.89.2.1", map[string]dockerapi.NetworkMember{
			"8b7c6d5e4f3a2b10": {Name: "shop-redis-1", IPv4Address: "10.89.2.2/24"},
		}, map[string]string{"com.docker.compose.project": "shop", "com.docker.compose.network": "cache"}),
		mk("scratch", "e4f5a6b7c8d9e0f1a2b3c4d5e6f7a8b9c0d1e2f3a4b5c6d7e8f9a0b1c2d3e4f5", "bridge", demoNetCreated(time.Date(2026, 11, 15, 9, 2, 0, 0, time.Local)), "10.89.3.0/24", "10.89.3.1", nil, nil),
	}
	nets[4].Internal = true
	return nets
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
