package ui

import (
	"bytes"
	_ "embed"
	"fmt"
	"image/color"
	"image/png"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/ironpark/ggfx"
	"github.com/ironpark/ggui"
	"github.com/ironpark/ggui/i18n"
	"github.com/ironpark/ggui/icons/lucide"
	"github.com/ironpark/ggui/ui"
	"github.com/ironpark/ggui/ui/icons"
	"github.com/ironpark/ggui/widget"

	"github.com/ironpark/gvx-desktop/internal/recipes"
	"github.com/ironpark/gvx-desktop/internal/registry"
	"github.com/ironpark/gvx-desktop/internal/rungv"
	"github.com/ironpark/gvx-desktop/internal/sandbox"
	"github.com/ironpark/gvx-desktop/internal/store"
)

// TitlebarHidden says whether the window draws its own title bar region:
// on macOS the content runs under a clear title bar, with the traffic
// lights over the sidebar's header.
var TitlebarHidden = runtime.GOOS == "darwin"

// Build is the root of the window. It builds the window again when the
// language changes: the words are chosen as the tree is built, and what
// the user was doing lives in the model, which stays. The tree is given
// the language's Localizer, which sets the language text is shaped in, and
// the words of ggui's own controls.
func Build(m *Model) ggui.Widget {
	return ggui.View(m.Lang, func(lang string) ggui.Widget {
		SetLanguage(lang)
		return i18n.Provide(localizer(lang), ui.ProvideStrings(controlStrings(lang), build(m)))
	})
}

func build(m *Model) ggui.Widget {
	pages := entering(m.Page, func(p string) ggui.Widget {
		switch p {
		case PageSandboxes:
			return sandboxesPage(m)
		case PageContainers:
			return containersPage(m)
		case PagePods:
			return podsPage(m)
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
	// The sidebar runs from the top of the window, with the traffic lights
	// over its header, down to the status bar, which has the window's
	// width; the top bar belongs to the content beside the sidebar and
	// holds its trigger. With the sidebar away the bar reaches the window's
	// edge, and makes room for the lights itself.
	main := widget.Column(
		widget.Expanded(widget.Row(
			sidebar(m),
			widget.Expanded(widget.Column(
				topBar(m),
				ui.Divider(),
				widget.Expanded(pages),
			).Stretch()),
		).Align(widget.AlignStretch)),
		ui.Divider(),
		statusBar(m),
	).Stretch()

	return widget.Stack(
		main,
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
	return widget.Pointer(child).OnDown(func(ggui.PointerEvent) {
		if w := ggui.UseWindow(); w != nil {
			w.StartDrag()
		}
	})
}

// sidebar is built again when the Containers or Pods item is enabled or
// disabled, which a sidebar entry takes only as it is made; the sidebar
// carries its highlight, hover and collapse across.
func sidebar(m *Model) ggui.Widget {
	access := ggui.Combine(m.ContainersAccess, m.PodsAccess, func(c ContainersAccess, p PodsAccess) [2]string {
		return [2]string{c.Reason(), p.Reason()}
	})
	return ggui.View(access, func(why [2]string) ggui.Widget { return sidebarFor(m, why[0], why[1]) })
}

// sidebarFor is the sidebar with the Containers and Pods items greyed out
// for the reasons given, or not for none.
func sidebarFor(m *Model, noContainers, noPods string) ggui.Widget {
	nav := ggui.Bind(m.Page.Get, m.Go)
	item := func(key, label string, g *icons.Glyph) ui.SidebarEntry {
		return ui.SidebarItem(key, label).Leading(glyph(g, 16)).Tooltip(label)
	}
	// Containers and Pods lead into the recipes' engines, and are greyed
	// out while those are not running.
	containers := gatedItem(item(PageContainers, tr("Containers"), lucide.Boxes), noContainers, m.RunningContainers)
	pods := gatedItem(item(PagePods, tr("Pods"), lucide.Hexagon), noPods, m.RunningPods)
	brandTop := 4.0
	if TitlebarHidden {
		brandTop = 40 // room for the traffic lights
	}
	brand := dragRegion(widget.Padding(widget.Row(
		widget.Image(brandMark()).Size(40, 40),
		widget.Column(
			widget.Text("gvx Desktop").Size(15).Weight(ggui.WeightSemibold).NoWrap(),
			ui.CaptionOf(ggui.Map(m.Engine, engineShort)).Size(11).NoWrap(),
		).Gap(1),
	).Gap(10), brandTop, 12, 8, 12))

	return ui.Sidebar(nav,
		item(PageDashboard, tr("Dashboard"), lucide.LayoutDashboard),
		item(PageSandboxes, tr("Sandboxes"), lucide.Box).Action(countBadge(m.Running)),
		containers,
		pods,
		item(PageImages, tr("Images"), lucide.Layers),
		item(PageVolumes, tr("Volumes"), lucide.HardDrive),
		item(PageSnapshots, tr("Snapshots"), lucide.Camera),
		ui.SidebarSection(tr("Recipes")),
		item(PageDocker, "Docker Engine", lucide.Container),
		item(PageKubernetes, "Kubernetes", lucide.ShipWheel),
	).Header(brand).
		FooterItems(item(PageSettings, tr("Settings"), lucide.Settings)).
		Width(232).
		Collapsible(ui.SidebarCollapseOffcanvas).BindCollapsed(m.SidebarNarrow).
		Shortcut("cmd+b").Name(tr("Navigation")).
		Easing(sidebarEasing)
}

//go:embed mark.png
var markPNG []byte

// brandMark is the app's mark, the helmeted gopher of assets/, made into
// a picture the first time the window is built.
var brandMark = sync.OnceValue(func() *ggfx.Image {
	img, err := png.Decode(bytes.NewReader(markPNG))
	if err != nil {
		panic("ui: mark.png: " + err.Error())
	}
	return ggfx.NewImageFromImage(img)
})

// MarkPNG is the app's mark as a PNG, for what draws it outside the window.
func MarkPNG() []byte { return markPNG }

// AppVersion is the version of the app, which main sets.
var AppVersion = "dev"

// EngineLine says in a few words which rungv the app runs sandboxes with,
// or that it has none.
func EngineLine(e Engine) string { return "rungv · " + engineShort(e) }

// ShortVersion is a version of gvx without its build stamp.
func ShortVersion(v string) string { return shortVersion(v) }

// trafficLightsWidth is the room the window's close, minimise and zoom
// buttons take at the start of the top bar, where the title bar is hidden.
const trafficLightsWidth = 68

// The sidebar slides away and back along sidebarEasing: gently off, fast
// through the middle, gently home. sidebarDuration is the theme's
// MotionSlow, which the sidebar itself takes.
var sidebarEasing = ggui.EaseInOutCubic

const sidebarDuration = 200 * time.Millisecond

// pageDuration is how long a page takes to come in, leaveDuration how long
// the one before it took to go, and hoverDuration how long a surface takes
// to answer the pointer.
const (
	pageDuration  = 180 * time.Millisecond
	leaveDuration = 70 * time.Millisecond
	hoverDuration = 120 * time.Millisecond
)

func rgb(v uint32) color.RGBA {
	return color.RGBA{uint8(v >> 16), uint8(v >> 8), uint8(v), 0xff}
}

type storeSettings = store.Settings

func engineShort(e Engine) string {
	switch {
	case e.Loading && !e.Checked:
		return tr("Checking engine…")
	case e.Err != "":
		return tr("Engine unavailable")
	case len(e.Info.AvailableBackends) == 0:
		return tr("No backend")
	}
	v := engineVersion(e)
	if v == "" {
		v = "rungv"
	}
	return v + " · " + e.Info.DefaultBackend
}

func topBar(m *Model) ggui.Widget {
	search := ui.ButtonOf(widget.Row(
		glyph(lucide.Search, 14).Color(colorGray),
		ui.Caption(tr("Search or run a command…")),
		widget.Spacer(),
		ui.KbdGroup(ui.Kbd(modKey()), ui.Kbd("K")),
	).Gap(8).Align(widget.AlignCenter), func() { m.PaletteQuery.Set(""); m.PaletteOpen.Set(true) }).
		Outline().Name(tr("Command palette")).Size(ui.ButtonSmall)

	engine := ggui.View(m.Engine, func(e Engine) ggui.Widget {
		c, text := colorGreen, tr("Engine running")
		switch {
		case e.Loading && !e.Checked:
			c, text = colorAmber, tr("Checking engine")
		case e.Err != "":
			c, text = colorRed, tr("Engine unavailable")
		case len(e.Info.AvailableBackends) == 0:
			c, text = colorRed, tr("No backend")
		}
		pill := ui.ButtonOf(widget.Row(dot(c, 8), widget.Text(text).Size(12)).Gap(8), func() { m.Go(PageSettings) }).
			Ghost().Size(ui.ButtonSmall).Name(text)
		return tip(pill, engineTooltip(e))
	})
	theme := ggui.View(m.Settings, func(s storeSettings) ggui.Widget {
		g, next, label := lucide.Monitor, "light", tr("Theme: system")
		switch s.Theme {
		case "light":
			g, next, label = lucide.Sun, "dark", tr("Theme: light")
		case "dark":
			g, next, label = lucide.Moon, "system", tr("Theme: dark")
		}
		return tip(iconButton(g, label, func() { m.UpdateSettings(func(s *storeSettings) { s.Theme = next }) }), label)
	})
	// With the sidebar away the traffic lights stand over the start of this
	// bar: room for them opens as the sidebar leaves, at its pace.
	var lights ggui.Widget = widget.Box()
	if TitlebarHidden {
		room := ggui.TweenOf(ggui.Map(m.SidebarNarrow, func(away bool) float64 {
			if away {
				return trafficLightsWidth
			}
			return 0
		}), sidebarDuration).Easing(sidebarEasing)
		lights = dragRegion(widget.Box().Height(52).BindWidth(room))
	}
	return widget.Box(widget.Row(
		lights,
		ui.SidebarTrigger(m.SidebarNarrow),
		dragRegion(widget.Box().Size(16, 52)),
		widget.Box(search).Width(340),
		widget.Expanded(dragRegion(widget.Box().Height(52))),
		engine,
		theme,
	).Gap(4).Align(widget.AlignCenter)).Pad(0, 12, 0, 8).Height(52)
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
		return tr("Looking for rungv…")
	}
	return fmt.Sprintf(tr("%s · backends: %s · lockdown: %s"), e.Path, strings.Join(e.Info.AvailableBackends, ", "), e.Info.HostLockdown)
}

func statusBar(m *Model) ggui.Widget {
	engine := ui.CaptionOf(ggui.Map(m.Engine, func(e Engine) string {
		if e.Err != "" || !e.Checked {
			return engineShort(e)
		}
		return fmt.Sprintf(tr("rungv %s · %s · %s lockdown"), engineVersion(e), e.Info.DefaultBackend, e.Info.HostLockdown)
	})).Size(11).NoWrap()
	running := ui.CaptionOf(ggui.Combine(m.Running, m.Specs, func(n int, specs []sandbox.Spec) string {
		return fmt.Sprintf(tr("%d of %d sandboxes running"), n, len(specs))
	})).Size(11).NoWrap()
	disk := ui.CaptionOf(ggui.Map(m.Disk, func(rows []rungv.DiskUsage) string {
		var parts []string
		for _, r := range rows {
			name := r.Type
			if name == "Local Volumes" {
				name = tr("Volumes")
			}
			parts = append(parts, tr(name)+" "+r.Size)
		}
		return strings.Join(parts, " · ")
	})).Size(11).NoWrap()
	sep := func() ggui.Widget { return widget.Box().Size(1, 12).Fill(colorGray) }
	return widget.Box(widget.Row(
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
		engine, sep(), running, widget.Spacer(), disk,
	).Gap(10).Align(widget.AlignCenter)).Pad(0, 14).Height(28)
}

// ---- Dialogs ------------------------------------------------------------

func confirmDialog(m *Model) ggui.Widget {
	return ui.Dialog(m.ConfirmOpen, widget.Column(
		ui.HeadingOf(m.ConfirmTitle).Size(17),
		ui.CaptionOf(m.ConfirmText),
		widget.Row(
			ui.Button(tr("Cancel"), func() { m.ConfirmOpen.Set(false) }).Outline(),
			ui.ButtonOf(widget.TextOf(m.ConfirmLabel), func() {
				m.ConfirmOpen.Set(false)
				m.RunConfirmed()
			}).Destructive().BindName(m.ConfirmLabel),
		).Gap(8).Justify(widget.JustifyEnd),
	).Gap(14).Stretch()).Width(440).Name(tr("Confirm"))
}

func pullDialog(m *Model) ggui.Widget {
	// What is typed is searched for, once the typing pauses.
	ggui.Watch(m.PullRef, func(string) { m.SearchTyped() })
	ggui.Watch(m.PullOpen, func(bool) { m.SearchTyped() })
	ggui.Watch(m.Pulling, func(bool) { m.SearchTyped() })
	ggui.Watch(ggui.Debounced(m.searchWaiting(), searchDelay), m.searchPaused)
	source := ggui.Bind(m.SearchSource, func(v registry.Source) {
		m.UpdateSettings(func(s *storeSettings) { s.SearchRegistry = string(v) })
		m.SearchTyped()
	})

	field := ui.TextField(m.PullRef).Placeholder(tr("Search, or a reference: nginx, ghcr.io/org/app:1.0")).
		Name(tr("Image reference")).OnSubmit(func(string) { m.Pull() }).BindDisabled(m.Pulling)
	searchBar := widget.Row(
		ui.Caption(tr("Search in")).Size(12),
		ui.ToggleGroup[registry.Source](source).Options(registry.Sources()).Format(registry.Source.Title).
			Outline().Size(ui.ButtonExtraSmall).Spacing(0).Name(tr("Registry to search")),
		ggui.If(m.SearchBusy, func() ggui.Widget { return ui.Spinner().Size(12) }),
		widget.Spacer(),
		ui.CaptionOf(ggui.Combine(m.SearchResults, m.SearchFor, func(r []registry.Result, q string) string {
			if q == "" {
				return ""
			}
			return tr("%d for “%s”", len(r), q)
		})).Size(12).NoWrap(),
	).Gap(8).Align(widget.AlignCenter)

	suggestions := widget.Wrap(
		suggestion(m, "alpine:3.20"), suggestion(m, "nginx:alpine"), suggestion(m, "python:3.12-alpine"),
		suggestion(m, "postgres:17-alpine"), suggestion(m, "redis:alpine"), suggestion(m, "docker:dind"),
	).Gap(6)

	// 0: nothing searched, 1: results, 2: none found, 3: the search failed.
	state := ggui.Combine(ggui.Combine(m.SearchResults, m.SearchFor, func(r []registry.Result, q string) int {
		switch {
		case q == "":
			return 0
		case len(r) == 0:
			return 2
		}
		return 1
	}), m.SearchErr, func(s int, err string) int {
		if err != "" {
			return 3
		}
		return s
	})
	found := ggui.View(state, func(s int) ggui.Widget {
		switch s {
		case 1:
			return widget.Box(widget.Scroll(ggui.View(m.SearchResults, func(rs []registry.Result) ggui.Widget {
				rows := make([]ggui.Widget, len(rs))
				for i, r := range rs {
					rows[i] = searchResult(m, r)
				}
				return widget.Column(rows...).Gap(2).Stretch()
			})).FadeEdges(10)).Height(248)
		case 2:
			return ui.CaptionOf(ggui.Map(m.SearchFor, func(q string) string {
				return tr("Nothing found for “%s”. A full reference can still be pulled.", q)
			}))
		case 3:
			return ui.CaptionOf(ggui.Map(m.SearchErr, func(e string) string { return tr("The search failed: %s", e) })).Color(colorRed)
		}
		return suggestions
	})
	tags := ggui.View(ggui.Combine(m.SearchTags, m.SearchTagsBusy, func(t []string, busy bool) int {
		if busy {
			return -1
		}
		return len(t)
	}), func(n int) ggui.Widget {
		switch {
		case n < 0:
			return widget.Row(ui.Spinner().Size(12), ui.Caption(tr("Reading tags…")).Size(12)).Gap(8).Align(widget.AlignCenter)
		case n == 0:
			return widget.Box()
		}
		chips := make([]ggui.Widget, 0, n)
		for _, t := range ggui.Peek(m.SearchTags) {
			chips = append(chips, ui.Badge(t).Outline().OnTap(func() { m.PickTag(t) }))
		}
		return widget.Column(
			ui.CaptionOf(ggui.Map(m.SearchTagsOf, func(ref string) string { return tr("Tags of %s, newest first", ref) })).Size(12),
			widget.Wrap(chips...).Gap(6),
		).Gap(6).Stretch()
	})
	// While a pull runs, its output has the room.
	browse := ggui.If(m.Pulling, func() ggui.Widget { return widget.Box() }).Else(func() ggui.Widget {
		return widget.Column(searchBar, found, tags).Gap(10).Stretch()
	})
	logs := ggui.If(ggui.Map(m.PullLog, func(s string) bool { return s != "" }), func() ggui.Widget {
		return ui.CodeBlockOf(m.PullLog).Height(180).Small().CopyButton(false)
	})
	return ui.Dialog(m.PullOpen, widget.Column(
		ui.Field(tr("Image"), field).Help(tr("Images run natively on this CPU (linux/%s). Layers are compiled to read-only EROFS files once and cached.", goarch())),
		browse,
		logs,
	).Gap(14).Stretch()).Title(tr("Pull an image")).Width(600).Footer(
		ui.Button(tr("Close"), func() { m.PullOpen.Set(false) }).Outline(),
		ui.ButtonOf(ggui.If(m.Pulling, func() ggui.Widget {
			return widget.Row(ui.Spinner().Size(14), widget.Text(tr("Pulling…"))).Gap(8)
		}).Else(func() ggui.Widget { return widget.Text(tr("Pull")) }), m.Pull).
			Name(tr("Pull")).BindDisabled(ggui.Or(m.Pulling, ggui.Map(m.PullRef, func(s string) bool { return strings.TrimSpace(s) == "" }))),
	)
}

// searchResult is one repository a registry search found: choosing it puts
// it in the field.
func searchResult(m *Model, r registry.Result) ggui.Widget {
	title := []ggui.Widget{widget.Text(r.Name).Size(13).Weight(ggui.WeightMedium).NoWrap().Ellipsis()}
	if r.Official {
		title = append(title, ui.Badge(tr("Official")).Outline())
	}
	text := []ggui.Widget{widget.Row(title...).Gap(8).Align(widget.AlignCenter)}
	if r.Description != "" {
		text = append(text, ui.Caption(r.Description).Size(12).NoWrap().Ellipsis())
	}
	var stats []ggui.Widget
	if r.Stars > 0 {
		stats = append(stats, glyph(lucide.Star, 12).Color(colorGray), ui.Caption(compact(int64(r.Stars))).Size(12))
	}
	if r.Pulls > 0 {
		stats = append(stats, glyph(lucide.Download, 12).Color(colorGray), ui.Caption(compact(r.Pulls)).Size(12))
	}
	row := widget.Row(
		widget.Expanded(widget.Column(text...).Gap(2)),
		widget.Row(stats...).Gap(5).Align(widget.AlignCenter),
	).Gap(12).Align(widget.AlignCenter)
	return hoverRow(row, func() { m.PickResult(r) })
}

// compact writes a count in a few characters: 950, 21k, 1.4M, 13B.
func compact(n int64) string {
	switch {
	case n < 1000:
		return fmt.Sprint(n)
	case n < 10_000:
		return fmt.Sprintf("%.1fk", float64(n)/1e3)
	case n < 1_000_000:
		return fmt.Sprintf("%dk", n/1000)
	case n < 10_000_000:
		return fmt.Sprintf("%.1fM", float64(n)/1e6)
	case n < 1_000_000_000:
		return fmt.Sprintf("%dM", n/1_000_000)
	case n < 10_000_000_000:
		return fmt.Sprintf("%.1fB", float64(n)/1e9)
	}
	return fmt.Sprintf("%dB", n/1_000_000_000)
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
	return ui.Dialog(m.InspectOpen, widget.Column(
		ui.HeadingOf(m.InspectTitle).Size(16),
		ui.CodeBlockOf(m.InspectBody).Language("json").Height(420).LineNumbers(true),
	).Gap(12).Stretch()).Width(720).Name(tr("Image details")).CloseButton(true)
}

func volumeDialog(m *Model) ggui.Widget {
	invalid := ggui.Map(m.VolumeName, func(s string) bool {
		s = strings.TrimSpace(s)
		return s == "" || (sandbox.Mount{Type: sandbox.MountVolume, Source: s, Target: "/x"}).Target == "" || strings.ContainsAny(s, " /\\:")
	})
	return ui.Dialog(m.VolumeOpen, widget.Column(
		ui.Field(tr("Name"), ui.TextField(m.VolumeName).Placeholder("pgdata").Name(tr("Volume name")).OnSubmit(func(string) { m.CreateVolume() })).
			Help(tr("Letters, digits, '-', '_' and '.'. Mount it in a sandbox as name:/guest/path.")),
		ui.Field(tr("Driver"), ui.ToggleGroup(m.VolumeDriver).Options([]string{"ext4lite", "hostfs"}).Outline().Name(tr("Volume driver"))).
			Help(tr("ext4lite keeps a journaled ext4 image in one file: real POSIX semantics, fast. hostfs keeps plain host files, slower.")),
	).Gap(14).Stretch()).Title(tr("Create a volume")).Width(460).Footer(
		ui.Button(tr("Cancel"), func() { m.VolumeOpen.Set(false) }).Outline(),
		ui.Button(tr("Create"), m.CreateVolume).BindDisabled(invalid),
	)
}

// quitText says what quitting with n sandboxes running does to them.
func quitText(n int) string {
	return trn(n, "%d sandbox is running. gvx has no daemon: sandboxes live only while gvx Desktop runs.",
		"%d sandboxes are running. gvx has no daemon: sandboxes live only while gvx Desktop runs.")
}

func quitDialog(m *Model) ggui.Widget {
	return ui.Dialog(m.QuitOpen, widget.Column(
		ui.Heading(tr("Stop running sandboxes?")).Size(17),
		ui.CaptionOf(ggui.Map(m.Running, quitText)),
		widget.Row(
			ui.Button(tr("Cancel"), func() { m.QuitOpen.Set(false) }).Outline(),
			ui.Button(tr("Stop and quit"), func() {
				m.QuitOpen.Set(false)
				m.StopAllAndThen(func() { quitApp() })
			}).Destructive(),
		).Gap(8).Justify(widget.JustifyEnd),
	).Gap(14).Stretch()).Width(460).Name(tr("Quit"))
}

// quitApp ends the app; main sets it.
var quitApp = func() {}

// SetQuit installs the function that ends the app.
func SetQuit(fn func()) { quitApp = fn }

func palette(m *Model) ggui.Widget {
	close := func(fn func()) func() {
		return func() { m.PaletteOpen.Set(false); fn() }
	}
	entries := func(containers, pods bool) []ui.CommandEntry {
		return []ui.CommandEntry{
			ui.CommandItem(tr("New sandbox"), close(func() { m.NewSandbox("") })).Group(tr("Sandboxes")).Shortcut("cmd+n").Keywords("create run"),
			ui.CommandItem(tr("Stop all sandboxes"), close(m.StopAll)).Group(tr("Sandboxes")),
			ui.CommandItem(tr("Pull image"), close(func() { m.OpenPull("") })).Group(tr("Images")).Keywords("download"),
			ui.CommandItem(tr("Start Docker Engine"), close(func() { m.CreateFromRecipe(recipes.Docker); m.Go(PageDocker) })).Group(tr("Recipes")).Keywords("dind dockerd"),
			ui.CommandItem(tr("Start Kubernetes (k3s)"), close(func() { m.CreateFromRecipe(recipes.Kubernetes); m.Go(PageKubernetes) })).Group(tr("Recipes")).Keywords("k8s cluster kubectl"),
			ui.CommandItem(tr("Create volume"), close(func() { m.VolumeOpen.Set(true) })).Group(tr("Volumes")),
			ui.CommandItem(tr("Dashboard"), close(func() { m.Go(PageDashboard) })).Group(tr("Go to")).Shortcut("cmd+1"),
			ui.CommandItem(tr("Sandboxes"), close(func() { m.Go(PageSandboxes) })).Group(tr("Go to")).Shortcut("cmd+2"),
			ui.CommandItem(tr("Containers"), close(func() { m.Go(PageContainers) })).Group(tr("Go to")).Shortcut("cmd+3").Keywords("docker ps").Disabled(!containers),
			ui.CommandItem(tr("Pods"), close(func() { m.Go(PagePods) })).Group(tr("Go to")).Shortcut("cmd+4").Keywords("kubectl get pods k8s").Disabled(!pods),
			ui.CommandItem(tr("Images"), close(func() { m.Go(PageImages) })).Group(tr("Go to")).Shortcut("cmd+5"),
			ui.CommandItem(tr("Volumes"), close(func() { m.Go(PageVolumes) })).Group(tr("Go to")).Shortcut("cmd+6"),
			ui.CommandItem(tr("Snapshots"), close(func() { m.Go(PageSnapshots) })).Group(tr("Go to")).Shortcut("cmd+7"),
			ui.CommandItem(tr("Settings"), close(func() { m.Go(PageSettings) })).Group(tr("Go to")).Shortcut("cmd+,"),
			ui.CommandItem(tr("Refresh"), close(m.RefreshAll)).Group(tr("App")).Shortcut("cmd+r"),
			ui.CommandItem(tr("Toggle dark mode"), close(func() {
				m.UpdateSettings(func(s *storeSettings) {
					if s.Theme == "dark" {
						s.Theme = "light"
					} else {
						s.Theme = "dark"
					}
				})
			})).Group(tr("App")).Keywords("theme appearance"),
		}
	}
	cmd := ui.Command(m.PaletteQuery).Height(300).StableHeight().Hints()
	// The Containers and Pods entries are greyed out as the sidebar's
	// items are.
	open := ggui.Combine(m.ContainersOpen, m.PodsOpen, func(c, p bool) [2]bool { return [2]bool{c, p} })
	ggui.Watch(open, func(on [2]bool) { cmd.Entries(entries(on[0], on[1])...) })
	return ui.CommandDialog(m.PaletteOpen, cmd)
}

// RefreshAll reloads everything rungv reports.
func (m *Model) RefreshAll() {
	m.DetectEngine(nil)
	m.LoadDocker()
	m.LoadPods()
	switch ggui.Peek(m.Page) {
	case PageContainers:
		m.LoadContainerLogs()
	case PagePods:
		m.LoadPodLogs()
	}
	m.CheckKubeconfig()
	for _, l := range m.Links() {
		m.CheckLink(l)
	}
	m.refreshStates()
}
