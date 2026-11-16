package ui

import (
	"fmt"
	"image/color"
	"runtime"
	"strings"

	"github.com/ironpark/ggui"
	"github.com/ironpark/ggui/icons/lucide"
	"github.com/ironpark/ggui/ui"
	"github.com/ironpark/ggui/ui/icons"

	"github.com/ironpark/gvx-desktop/internal/recipes"
	"github.com/ironpark/gvx-desktop/internal/rungv"
	"github.com/ironpark/gvx-desktop/internal/sandbox"
	"github.com/ironpark/gvx-desktop/internal/store"
)

// TitlebarHidden says whether the window draws its own title bar region:
// on macOS the content runs under a clear title bar, with the traffic
// lights over the sidebar's header.
var TitlebarHidden = runtime.GOOS == "darwin"

// Build is the root of the window.
func Build(m *Model) ggui.Widget {
	pages := ggui.View(m.Page, func(p string) ggui.Widget {
		switch p {
		case PageSandboxes:
			return sandboxesPage(m)
		case PageImages:
			return imagesPage(m)
		case PageVolumes:
			return volumesPage(m)
		case PageSnapshots:
			return snapshotsPage(m)
		case PageDocker:
			return dockerPage(m)
		case PageKubernetes:
			return kubernetesPage(m)
		case PageSettings:
			return settingsPage(m)
		}
		return dashboardPage(m)
	})
	main := ggui.Column(
		topBar(m),
		ui.Divider(),
		ggui.Expanded(pages),
		ui.Divider(),
		statusBar(m),
	).Stretch()

	return ggui.Stack(
		ggui.Row(sidebar(m), ggui.Expanded(main)).Align(ggui.AlignStretch),
		editorSheet(m),
		confirmDialog(m),
		pullDialog(m),
		inspectDialog(m),
		volumeDialog(m),
		quitDialog(m),
		palette(m),
		m.Toasts,
	).Expand()
}

// dragRegion moves the window when pressed, standing in for the hidden
// title bar.
func dragRegion(child ggui.Widget) ggui.Widget {
	if !TitlebarHidden {
		return child
	}
	return ggui.Pointer(child).OnDown(func(ggui.PointerEvent) {
		if w := ggui.UseWindow(); w != nil {
			w.StartDrag()
		}
	})
}

func sidebar(m *Model) ggui.Widget {
	nav := ggui.Bind(m.Page.Get, m.Go)
	running := ggui.View(m.Running, func(n int) ggui.Widget {
		if n == 0 {
			return ggui.Box()
		}
		return ui.Badge(itoa(n))
	})
	item := func(key, label string, g *icons.Glyph) ui.SidebarEntry {
		return ui.SidebarItem(key, label).Leading(glyph(g, 16)).Tooltip(label)
	}
	brandTop := 12.0
	if TitlebarHidden {
		brandTop = 40 // room for the traffic lights
	}
	brand := dragRegion(ggui.Padding(ggui.Row(
		ggui.Box(glyph(lucide.Boxes, 18).Color(rgb(0xffffff))).Pad(6).Radius(8).Fill(colorBlue),
		ggui.Column(
			ggui.Text("gvx Desktop").Size(15).Weight(ggui.WeightSemibold).NoWrap(),
			ui.CaptionOf(ggui.Map(m.Engine, engineShort)).Size(11).NoWrap(),
		).Gap(1),
	).Gap(10), brandTop, 12, 8, 12))

	return ui.Sidebar(nav,
		item(PageDashboard, "Dashboard", lucide.LayoutDashboard),
		item(PageSandboxes, "Sandboxes", lucide.Box).Action(running),
		item(PageImages, "Images", lucide.Layers),
		item(PageVolumes, "Volumes", lucide.HardDrive),
		item(PageSnapshots, "Snapshots", lucide.Camera),
		ui.SidebarSection("Recipes"),
		item(PageDocker, "Docker Engine", lucide.Container),
		item(PageKubernetes, "Kubernetes", lucide.Ship),
	).Header(brand).
		FooterItems(item(PageSettings, "Settings", lucide.Settings)).
		Width(232).
		Collapsible(ui.SidebarCollapseIcon).BindCollapsed(m.SidebarNarrow).
		Shortcut("cmd+b").Name("Navigation")
}

func rgb(v uint32) color.RGBA {
	return color.RGBA{uint8(v >> 16), uint8(v >> 8), uint8(v), 0xff}
}

type storeSettings = store.Settings

func engineShort(e Engine) string {
	switch {
	case e.Loading && !e.Checked:
		return "Checking engine…"
	case e.Err != "":
		return "Engine unavailable"
	case len(e.Info.AvailableBackends) == 0:
		return "No backend"
	}
	v := e.Version.Version
	if v == "" {
		v = "rungv"
	}
	return v + " · " + e.Info.DefaultBackend
}

func topBar(m *Model) ggui.Widget {
	search := ui.ButtonOf(ggui.Row(
		glyph(lucide.Search, 14).Color(colorGray),
		ui.Caption("Search or run a command…"),
		ggui.Spacer(),
		ui.KbdGroup(ui.Kbd(modKey()), ui.Kbd("K")),
	).Gap(8).Align(ggui.AlignCenter), func() { m.PaletteQuery.Set(""); m.PaletteOpen.Set(true) }).
		Outline().Name("Command palette").Size(ui.ButtonSmall)

	engine := ggui.View(m.Engine, func(e Engine) ggui.Widget {
		c, text := colorGreen, "Engine running"
		switch {
		case e.Loading && !e.Checked:
			c, text = colorAmber, "Checking engine"
		case e.Err != "":
			c, text = colorRed, "Engine unavailable"
		case len(e.Info.AvailableBackends) == 0:
			c, text = colorRed, "No backend"
		}
		pill := ui.ButtonOf(ggui.Row(dot(c, 8), ggui.Text(text).Size(12)).Gap(8), func() { m.Go(PageSettings) }).
			Ghost().Size(ui.ButtonSmall).Name(text)
		return tip(pill, engineTooltip(e))
	})
	theme := ggui.View(m.Settings, func(s storeSettings) ggui.Widget {
		g, next, label := lucide.Monitor, "light", "Theme: system"
		switch s.Theme {
		case "light":
			g, next, label = lucide.Sun, "dark", "Theme: light"
		case "dark":
			g, next, label = lucide.Moon, "system", "Theme: dark"
		}
		return tip(iconButton(g, label, func() { m.UpdateSettings(func(s *storeSettings) { s.Theme = next }) }), label)
	})
	left := 12.0
	if TitlebarHidden {
		left = 8
	}
	return ggui.Box(ggui.Row(
		ui.SidebarTrigger(m.SidebarNarrow),
		dragRegion(ggui.Box().Size(16, 52)),
		ggui.Box(search).Width(340),
		ggui.Expanded(dragRegion(ggui.Box().Height(52))),
		engine,
		theme,
	).Gap(4).Align(ggui.AlignCenter)).Pad(0, 12, 0, left).Height(52)
}

func modKey() string {
	if runtime.GOOS == "darwin" {
		return "⌘"
	}
	return "Ctrl"
}

func engineTooltip(e Engine) string {
	if e.Err != "" {
		return e.Err
	}
	if !e.Checked {
		return "Looking for rungv…"
	}
	return fmt.Sprintf("%s · backends: %s · lockdown: %s", e.Path, strings.Join(e.Info.AvailableBackends, ", "), e.Info.HostLockdown)
}

func statusBar(m *Model) ggui.Widget {
	engine := ui.CaptionOf(ggui.Map(m.Engine, func(e Engine) string {
		if e.Err != "" || !e.Checked {
			return engineShort(e)
		}
		return fmt.Sprintf("rungv %s · %s · %s lockdown", e.Version.Version, e.Info.DefaultBackend, e.Info.HostLockdown)
	})).Size(11).NoWrap()
	running := ui.CaptionOf(ggui.Combine(m.Running, m.Specs, func(n int, specs []sandbox.Spec) string {
		return fmt.Sprintf("%d of %d sandboxes running", n, len(specs))
	})).Size(11).NoWrap()
	disk := ui.CaptionOf(ggui.Map(m.Disk, func(rows []rungv.DiskUsage) string {
		var parts []string
		for _, r := range rows {
			name := r.Type
			if name == "Local Volumes" {
				name = "Volumes"
			}
			parts = append(parts, name+" "+r.Size)
		}
		return strings.Join(parts, " · ")
	})).Size(11).NoWrap()
	sep := func() ggui.Widget { return ggui.Box().Size(1, 12).Fill(colorGray) }
	return ggui.Box(ggui.Row(
		ggui.View(m.Engine, func(e Engine) ggui.Widget {
			c := colorGreen
			if !e.Ready() {
				c = colorRed
				if !e.Checked {
					c = colorAmber
				}
			}
			return dot(c, 6)
		}),
		engine, sep(), running, ggui.Spacer(), disk,
	).Gap(10).Align(ggui.AlignCenter)).Pad(0, 14).Height(28)
}

// ---- Dialogs ------------------------------------------------------------

func confirmDialog(m *Model) ggui.Widget {
	return ui.Dialog(m.ConfirmOpen, ggui.Column(
		ui.HeadingOf(m.ConfirmTitle).Size(17),
		ui.CaptionOf(m.ConfirmText),
		ggui.Row(
			ui.Button("Cancel", func() { m.ConfirmOpen.Set(false) }).Outline(),
			ui.ButtonOf(ggui.TextOf(m.ConfirmLabel), func() {
				m.ConfirmOpen.Set(false)
				m.RunConfirmed()
			}).Destructive().BindName(m.ConfirmLabel),
		).Gap(8).Justify(ggui.JustifyEnd),
	).Gap(14).Stretch()).Width(440).Name("Confirm")
}

func pullDialog(m *Model) ggui.Widget {
	field := ui.TextField(m.PullRef).Placeholder("alpine:3.20, nginx:alpine, ghcr.io/org/app:1.0").
		Name("Image reference").OnSubmit(func(string) { m.Pull() }).BindDisabled(m.Pulling)
	suggestions := ggui.Wrap(
		suggestion(m, "alpine:3.20"), suggestion(m, "nginx:alpine"), suggestion(m, "python:3.12-alpine"),
		suggestion(m, "postgres:17-alpine"), suggestion(m, "redis:alpine"), suggestion(m, "docker:dind"),
	).Gap(6)
	logs := ggui.If(ggui.Map(m.PullLog, func(s string) bool { return s != "" }), func() ggui.Widget {
		return ui.CodeBlockOf(m.PullLog).Height(180).Small().CopyButton(false)
	})
	return ui.Dialog(m.PullOpen, ggui.Column(
		ui.Field("Image", field).Help("Images run natively on this CPU (linux/"+goarch()+"). Layers are compiled to read-only EROFS files once and cached."),
		suggestions,
		logs,
	).Gap(14).Stretch()).Title("Pull an image").Width(560).Footer(
		ui.Button("Close", func() { m.PullOpen.Set(false) }).Outline(),
		ui.ButtonOf(ggui.If(m.Pulling, func() ggui.Widget {
			return ggui.Row(ui.Spinner().Size(14), ggui.Text("Pulling…")).Gap(8)
		}).Else(func() ggui.Widget { return ggui.Text("Pull") }), m.Pull).
			Name("Pull").BindDisabled(ggui.Or(m.Pulling, ggui.Map(m.PullRef, func(s string) bool { return strings.TrimSpace(s) == "" }))),
	)
}

func goarch() string {
	if runtime.GOARCH == "amd64" {
		return "amd64"
	}
	return runtime.GOARCH
}

func suggestion(m *Model, ref string) ggui.Widget {
	return ui.Badge(ref).Outline().OnTap(func() { m.PullRef.Set(ref) })
}

func inspectDialog(m *Model) ggui.Widget {
	return ui.Dialog(m.InspectOpen, ggui.Column(
		ui.HeadingOf(m.InspectTitle).Size(16),
		ui.CodeBlockOf(m.InspectBody).Language("json").Height(420).LineNumbers(true),
	).Gap(12).Stretch()).Width(720).Name("Image details").CloseButton(true)
}

func volumeDialog(m *Model) ggui.Widget {
	invalid := ggui.Map(m.VolumeName, func(s string) bool {
		s = strings.TrimSpace(s)
		return s == "" || (sandbox.Mount{Type: sandbox.MountVolume, Source: s, Target: "/x"}).Target == "" || strings.ContainsAny(s, " /\\:")
	})
	return ui.Dialog(m.VolumeOpen, ggui.Column(
		ui.Field("Name", ui.TextField(m.VolumeName).Placeholder("pgdata").Name("Volume name").OnSubmit(func(string) { m.CreateVolume() })).
			Help("Letters, digits, '-', '_' and '.'. Mount it in a sandbox as name:/guest/path."),
		ui.Field("Driver", ui.ToggleGroup(m.VolumeDriver).Options([]string{"ext4lite", "hostfs"}).Outline().Name("Volume driver")).
			Help("ext4lite keeps a journaled ext4 image in one file: real POSIX semantics, fast. hostfs keeps plain host files, slower."),
	).Gap(14).Stretch()).Title("Create a volume").Width(460).Footer(
		ui.Button("Cancel", func() { m.VolumeOpen.Set(false) }).Outline(),
		ui.Button("Create", m.CreateVolume).BindDisabled(invalid),
	)
}

func quitDialog(m *Model) ggui.Widget {
	return ui.Dialog(m.QuitOpen, ggui.Column(
		ui.Heading("Stop running sandboxes?").Size(17),
		ui.CaptionOf(ggui.Map(m.Running, func(n int) string {
			return fmt.Sprintf("%d sandbox(es) are running. gvx has no daemon: sandboxes live only while gvx Desktop runs.", n)
		})),
		ggui.Row(
			ui.Button("Cancel", func() { m.QuitOpen.Set(false) }).Outline(),
			ui.Button("Stop and quit", func() {
				m.QuitOpen.Set(false)
				m.StopAllAndThen(func() { quitApp() })
			}).Destructive(),
		).Gap(8).Justify(ggui.JustifyEnd),
	).Gap(14).Stretch()).Width(460).Name("Quit")
}

// quitApp ends the app; main sets it.
var quitApp = func() {}

// SetQuit installs the function that ends the app.
func SetQuit(fn func()) { quitApp = fn }

func palette(m *Model) ggui.Widget {
	close := func(fn func()) func() {
		return func() { m.PaletteOpen.Set(false); fn() }
	}
	cmd := ui.Command(m.PaletteQuery,
		ui.CommandItem("New sandbox", close(func() { m.NewSandbox("") })).Group("Sandboxes").Shortcut("cmd+n").Keywords("create run"),
		ui.CommandItem("Stop all sandboxes", close(func() { m.StopAllAndThen(func() {}) })).Group("Sandboxes"),
		ui.CommandItem("Pull image", close(func() { m.OpenPull("") })).Group("Images").Keywords("download"),
		ui.CommandItem("Start Docker Engine", close(func() { m.CreateFromRecipe(recipes.Docker); m.Go(PageDocker) })).Group("Recipes").Keywords("dind dockerd"),
		ui.CommandItem("Start Kubernetes (k3s)", close(func() { m.CreateFromRecipe(recipes.Kubernetes); m.Go(PageKubernetes) })).Group("Recipes").Keywords("k8s cluster kubectl"),
		ui.CommandItem("Create volume", close(func() { m.VolumeOpen.Set(true) })).Group("Volumes"),
		ui.CommandItem("Dashboard", close(func() { m.Go(PageDashboard) })).Group("Go to").Shortcut("cmd+1"),
		ui.CommandItem("Sandboxes", close(func() { m.Go(PageSandboxes) })).Group("Go to").Shortcut("cmd+2"),
		ui.CommandItem("Images", close(func() { m.Go(PageImages) })).Group("Go to").Shortcut("cmd+3"),
		ui.CommandItem("Volumes", close(func() { m.Go(PageVolumes) })).Group("Go to").Shortcut("cmd+4"),
		ui.CommandItem("Snapshots", close(func() { m.Go(PageSnapshots) })).Group("Go to").Shortcut("cmd+5"),
		ui.CommandItem("Settings", close(func() { m.Go(PageSettings) })).Group("Go to").Shortcut("cmd+,"),
		ui.CommandItem("Refresh", close(m.RefreshAll)).Group("App").Shortcut("cmd+r"),
		ui.CommandItem("Toggle dark mode", close(func() {
			m.UpdateSettings(func(s *storeSettings) {
				if s.Theme == "dark" {
					s.Theme = "light"
				} else {
					s.Theme = "dark"
				}
			})
		})).Group("App").Keywords("theme appearance"),
	).Height(300).StableHeight().Hints()
	return ui.CommandDialog(m.PaletteOpen, cmd)
}

// RefreshAll reloads everything rungv reports.
func (m *Model) RefreshAll() {
	m.DetectEngine(nil)
	m.LoadDocker()
	m.CheckKubeconfig()
	m.refreshStates()
}
