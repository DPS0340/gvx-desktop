// Command gvx-desktop is a desktop app for gvx: it runs Linux containers
// as unprivileged gvx sandboxes, with no VM and no daemon, and manages
// their images, volumes and snapshots.
package main

import (
	"bytes"
	"errors"
	"flag"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"log"
	"os"
	"os/signal"
	"runtime"
	"slices"
	"sync"
	"syscall"
	"time"

	"github.com/ironpark/ggui"
	"github.com/ironpark/ggui/record"
	gguiruntime "github.com/ironpark/ggui/runtime"
	uitheme "github.com/ironpark/ggui/ui/theme"

	"github.com/ironpark/gvx-desktop-vm/vmhelper"
	"github.com/ironpark/gvx-desktop/internal/recipes"
	"github.com/ironpark/gvx-desktop/internal/store"
	"github.com/ironpark/gvx-desktop/internal/ui"
)

// version is the app's, which a release build sets with -X main.version.
var version = "dev"

func main() {
	// The amd64 builder's VM helper is this executable too, started by the
	// app with vmhelper.Arg: a separate process with nothing of the app's.
	if len(os.Args) > 1 && os.Args[1] == vmhelper.Arg {
		os.Exit(vmhelper.Main(os.Args[2:]))
	}
	// ggui record runs the app to draw the README's tour (tour.go), which
	// reads none of the user's data and must not hand off to a copy of the
	// app already running, as SingleInstance would.
	record.Main()
	ui.AppVersion = version
	dataDir := flag.String("data-dir", "", "directory for sandboxes, settings and snapshots (default: the user config dir)")
	renderDir := flag.String("render-dir", "", "render the screens to PNG files in this directory, then exit")
	renderLang := flag.String("render-lang", "en", "the language -render-dir renders in: en, ko, ja, zh-Hans, zh-Hant, de or hi")
	hidden := flag.Bool(ui.HiddenFlag, false, "start in the tray without showing the window, as a start at login does")
	flag.Parse()

	dir := *dataDir
	if dir == "" {
		var err error
		if dir, err = store.DefaultDir(); err != nil {
			log.Fatal(err)
		}
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		log.Fatal(err)
	}
	st := store.Store{Dir: dir}
	if *renderDir != "" {
		if err := renderScreens(*renderLang, *renderDir); err != nil {
			log.Fatal(err)
		}
		return
	}
	if err := run(st, dir, *hidden); err != nil {
		log.Fatal(err)
	}
}

func run(st store.Store, dir string, hidden bool) error {
	var (
		app *ggui.App
		m   *ui.Model
	)
	dispose := ggui.Root(func() {
		m = ui.NewModel(st, dir)
		// A start at login stays out of the way, in the tray, when there
		// is one to open the window from.
		hidden = hidden && trayAvailable() && !ggui.Peek(m.Settings).NoTray
		cfg := ggui.Config{
			Hidden:         hidden,
			Title:          "GVX Desktop",
			Width:          1280,
			Height:         840,
			MinWidth:       880,
			MinHeight:      600,
			Resizable:      true,
			Remember:       "main",
			TitlebarHidden: ui.TitlebarHidden,
			TrafficLights:  &ggui.Point{X: 18, Y: 18},
			Inspector:      "f12",
		}
		app = ggui.New(cfg, func() ggui.Widget { return ui.Build(m) })
		m.Attach(app)
		m.SetMenuSource(app.Menu)
		app.Setup(func() {
			bindTheme(m)
			m.Start()
			if hidden {
				// The window is hidden already; this takes the app out of
				// the Dock too, and ShowFromTray brings the window back.
				app.HideToTray()
			}
			installTray(app, m)
			// The menu bar is worded as it is built, so a change of
			// language builds it again, as does a change of what the
			// Docker and Kubernetes menus tell.
			ggui.Watch(m.Lang, func(lang string) {
				ui.SetLanguage(lang)
				installMenus(app, m)
			})
			ggui.Watch(m.Menus, func(ui.MenuState) { installMenus(app, m) })
		})
		installShortcuts(app, m)
		installMenus(app, m)

		ui.SetQuit(app.Close)
		// On macOS a quit (⌘Q, the Dock's Quit, or the quit Apple event
		// that Homebrew sends before it replaces the app) reaches the window
		// as a close request too, right after OnQuitRequest. Only the close
		// button hides the app to the tray; a quit goes on to quit.
		quitAsked := false
		app.OnQuitRequest(func() { quitAsked = true })
		app.OnCloseRequest(func() bool {
			quitting := quitAsked
			quitAsked = false
			set := ggui.Peek(m.Settings)
			if !quitting && set.HideOnClose && !set.NoTray && trayAvailable() {
				app.HideToTray()
				return false
			}
			return requestQuit(app, m)
		})
	})
	defer dispose()

	// Opening the app again, from another launch or (on macOS) from the
	// Finder or the Dock, brings its window back from the tray.
	app.OnReopen(app.ShowFromTray)
	err := app.SingleInstance("io.github.ironpark.gvx-desktop", func(gguiruntime.Launch) { app.ShowFromTray() })
	if errors.Is(err, gguiruntime.ErrRunning) {
		return nil
	}
	// An interrupt or termination of the app ends it the same way a quit
	// does, so its sandboxes are stopped rather than orphaned.
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-sigs
		app.Quit()
	}()
	runErr := app.Run()
	signal.Stop(sigs)
	// The shells of the terminals end with the app, and what they run in
	// their containers with them; so do the port forwards, and the
	// connections they carry.
	m.CloseTerminals()
	m.CloseForwards()
	m.CloseExtensions(8 * time.Second)
	m.CloseDockerProxy()
	m.CloseExplorer()
	// Whatever ended the app, no sandbox outlives it: gvx has no daemon to
	// adopt them.
	var stopWG sync.WaitGroup
	stopWG.Go(func() { m.Supervisor().StopAll(10 * time.Second) })
	stopWG.Go(func() { m.CloseSandboxEngine(10 * time.Second) })
	stopWG.Wait()
	// Last, an update the user restarted for goes in place and the new app
	// starts; one downloaded and not asked for is removed.
	if err := m.FinishUpdate(); err != nil {
		log.Printf("update: %v", err)
	}
	return runErr
}

// requestQuit closes the app now when nothing runs, stops the sandboxes
// first when the settings say so, and asks otherwise.
func requestQuit(app *ggui.App, m *ui.Model) bool {
	if m.ActiveCount() == 0 {
		return true
	}
	if ggui.Peek(m.Settings).StopOnQuit {
		m.StopAllAndThen(app.Close)
		return false
	}
	app.ShowFromTray()
	return m.PrepareQuit()
}

func quit(app *ggui.App, m *ui.Model) {
	if requestQuit(app, m) {
		app.Close()
	}
}

func trayAvailable() bool { return runtime.GOOS == "darwin" || runtime.GOOS == "windows" }

func bindTheme(m *ui.Model) {
	preset := uitheme.Preset{Base: uitheme.BaseZinc, Accent: uitheme.AccentBlue}
	dark := ggui.Combine(m.Settings, ggui.SystemDark(), func(s store.Settings, system bool) bool {
		switch s.Theme {
		case "dark":
			return true
		case "light":
			return false
		}
		return system
	})
	uitheme.Bind(dark, preset.Dark(), preset.Light())
}

func installShortcuts(app *ggui.App, m *ui.Model) {
	// ⌘1 and on go to the sidebar's pages in its order. Go does not open the
	// Containers page while the Docker Engine is not running, nor the Pods
	// page while the cluster is not.
	pages := []string{
		ui.PageDashboard,
		ui.PageSandboxes,
		ui.PageImages,
		ui.PageVolumes,
		ui.PageSnapshots,
		ui.PageDocker,
		ui.PageContainers,
		ui.PageKubernetes,
		ui.PagePods,
	}
	for i, p := range pages {
		app.Shortcut(fmt.Sprintf("cmd+%d", i+1), func() { m.Go(p) })
	}
	app.Shortcut("cmd+k", func() { m.PaletteQuery.Set(""); m.PaletteOpen.Set(true) })
}

func installMenus(app *ggui.App, m *ui.Model) {
	dockerMenuLabel := ui.T("Start Docker Engine")
	if ggui.Peek(m.Settings).SandboxEngine {
		dockerMenuLabel = ui.T("Open Docker")
	}
	viewItems := []ggui.MenuItem{
		ggui.MenuAction(ui.T("Command Palette"), "cmd+k", func() { m.PaletteQuery.Set(""); m.PaletteOpen.Set(true) }),
		ggui.MenuAction(ui.T("Refresh"), "cmd+r", m.RefreshAll),
		ggui.MenuSeparator(),
		ggui.MenuAction(ui.T("Dashboard"), "cmd+1", func() { m.Go(ui.PageDashboard) }),
		ggui.MenuAction(ui.T("Sandboxes"), "cmd+2", func() { m.Go(ui.PageSandboxes) }),
		ggui.MenuAction(ui.T("Images"), "cmd+3", func() { m.Go(ui.PageImages) }),
		ggui.MenuAction(ui.T("Volumes"), "cmd+4", func() { m.Go(ui.PageVolumes) }),
		ggui.MenuAction(ui.T("Snapshots"), "cmd+5", func() { m.Go(ui.PageSnapshots) }),
		ggui.MenuSeparator(),
		ggui.MenuAction("Docker", "cmd+6", func() { m.Go(ui.PageDocker) }),
		ggui.MenuAction(ui.T("Networks"), "", func() { m.Go(ui.PageNetworks) }).BindEnabled(m.ContainersOpen),
		ggui.MenuAction(ui.T("Containers"), "cmd+7", func() { m.Go(ui.PageContainers) }).BindEnabled(m.ContainersOpen),
		ggui.MenuSeparator(),
		ggui.MenuAction("Kubernetes", "cmd+8", func() { m.Go(ui.PageKubernetes) }),
		ggui.MenuAction(ui.T("Pods"), "cmd+9", func() { m.GoKube(ui.KubePods) }).BindEnabled(m.PodsOpen),
		ggui.MenuAction(ui.T("Workloads"), "", func() { m.GoKube(ui.KubeWorkloads) }).BindEnabled(m.PodsOpen),
		ggui.MenuAction(ui.T("Services"), "", func() { m.GoKube(ui.KubeServices) }).BindEnabled(m.PodsOpen),
		ggui.MenuSeparator(),
		ggui.MenuAction(ui.T("Image registry"), "", m.OpenRegistry),
	}
	if ui.LabsSupported() {
		viewItems = append(viewItems, ggui.MenuAction(ui.T("Labs"), "", func() { m.Go(ui.PageLabs) }))
	}
	viewItems = append(viewItems, ggui.MenuAction(ui.T("Extensions"), "", func() { m.Go(ui.PageExtensions) }).
		BindEnabled(ggui.Map(m.Settings, func(s store.Settings) bool { return s.Extensions })))
	app.SetMenu(
		ggui.Menu(ui.T("File"),
			ggui.MenuAction(ui.T("New Sandbox…"), "cmd+n", func() { m.NewSandbox("") }),
			ggui.MenuAction(ui.T("Pull Image…"), "cmd+shift+p", func() { m.OpenPull("") }),
			ggui.MenuAction(ui.T("Create Volume…"), "", m.OpenVolumeDialog),
			ggui.MenuSeparator(),
			ggui.MenuAction(ui.T("Settings…"), "cmd+,", func() { m.Go(ui.PageSettings) }),
			ggui.MenuAction(ui.T("Check for Updates…"), "", func() { m.CheckAppUpdate(false) }),
		),
		ggui.EditMenu(ggui.EditActions{}),
		ggui.Menu(ui.T("Sandbox"),
			ggui.MenuAction(dockerMenuLabel, "", m.OpenDocker),
			ggui.MenuAction(ui.T("Start Kubernetes"), "", func() { m.CreateFromRecipe(recipes.Kubernetes); m.Go(ui.PageKubernetes) }),
			ggui.MenuSeparator(),
			ggui.MenuAction(ui.T("Stop All"), "", m.StopAll).BindEnabled(ggui.Map(m.TotalRunning, func(n int) bool { return n > 0 })),
		),
		ggui.Menu("Docker", m.RecipeMenu(recipes.Docker, app.ShowFromTray)...),
		ggui.Menu("Kubernetes", m.RecipeMenu(recipes.Kubernetes, app.ShowFromTray)...),
		ggui.Menu(ui.T("View"), viewItems...),
	)
}

// installTray puts gvx in the menu bar (macOS) or notification area
// (Windows), for as long as the settings ask for it: the app keeps
// sandboxes running with its window hidden.
func installTray(app *ggui.App, m *ui.Model) {
	if !trayAvailable() {
		return
	}
	var tray *ggui.Tray
	show := app.ShowFromTray
	type trayUpdate struct {
		name        string
		downloading bool
		ready       bool
	}
	// The newest app the tray offers, as its menu tells it: coarse state
	// (available / downloading / ready) so a download does not rebuild the
	// menu on every percentage tick.
	appUpdate := ggui.Combine(m.AppUpdate, m.Settings, func(u ui.AppUpdate, s store.Settings) trayUpdate {
		if !u.Offered(s) {
			return trayUpdate{}
		}
		return trayUpdate{name: u.Name(), downloading: u.Downloading, ready: u.Ready}
	})
	rebuild := func() {
		if tray == nil {
			return
		}
		ui.SetLanguage(ggui.Peek(m.Lang))
		engine, update := ggui.Peek(m.Engine), ggui.Peek(m.Update)
		items := []ggui.MenuItem{
			ggui.MenuAction(ui.T("Open GVX Desktop"), "", show),
			ggui.MenuAction(ui.EngineLine(engine), "", nil).BindEnabled(ggui.Const(false)),
		}
		if update.Available(engine) && !update.Installing {
			items = append(items, ggui.MenuAction(ui.T("Update rungvx to %s", update.Name()), "", m.InstallRungv))
		}
		if tu := ggui.Peek(appUpdate); tu.name != "" {
			switch {
			case tu.downloading:
				items = append(items, ggui.MenuAction(ui.T("Downloading GVX Desktop %s…", tu.name), "", nil).BindEnabled(ggui.Const(false)))
			case tu.ready:
				items = append(items, ggui.MenuAction(ui.T("Restart to update to %s", tu.name), "", func() { show(); m.UpdateApp() }))
			default:
				items = append(items, ggui.MenuAction(ui.T("Update GVX Desktop to %s", tu.name), "", func() { show(); m.UpdateApp() }))
			}
		}
		items = append(items, ggui.MenuSeparator())
		rows := ggui.Peek(m.Rows)
		running, active := 0, 0
		for _, r := range rows {
			id, name := r.Spec.ID, r.Spec.Name
			label := "○ " + name
			actions := []ggui.MenuItem{ggui.MenuAction(ui.T("Start"), "", func() { m.StartSandbox(id) })}
			if r.State.Status.Active() {
				active++
				label = "● " + name
				if r.State.Paused {
					label += " · " + ui.T("Paused")
				} else {
					running++
				}
				actions = nil
				switch {
				case r.State.Paused:
					actions = append(actions, ggui.MenuAction(ui.T("Resume"), "", func() { m.ResumeSandbox(id) }))
				case r.State.Control:
					actions = append(actions, ggui.MenuAction(ui.T("Pause"), "", func() { m.PauseSandbox(id) }))
				}
				actions = append(actions,
					ggui.MenuAction(ui.T("Stop"), "", func() { m.StopSandbox(id) }),
					ggui.MenuAction(ui.T("Restart"), "", func() { m.RestartSandbox(id) }),
				)
			}
			actions = append(actions, ggui.MenuAction(ui.T("Show Logs"), "", func() { show(); m.ShowSandbox(id, 0) }))
			items = append(items, ggui.Menu(label, actions...))
		}
		engRunning := ggui.Peek(m.EngineRunning)
		if len(rows) == 0 {
			if engRunning > 0 {
				items = append(items, ggui.MenuAction(ui.TN(engRunning, "%d container running", "%d containers running"), "", func() { show(); m.Go(ui.PageContainers) }))
			} else {
				items = append(items, ggui.MenuAction(ui.T("No sandboxes"), "", nil).BindEnabled(ggui.Const(false)))
			}
		}
		totalRunning := running + engRunning
		totalActive := active + engRunning
		items = append(items,
			ggui.MenuSeparator(),
			ggui.Menu("Docker", m.RecipeMenu(recipes.Docker, show)...),
			ggui.Menu("Kubernetes", m.RecipeMenu(recipes.Kubernetes, show)...),
			ggui.MenuSeparator(),
			ggui.MenuAction(ui.T("New Sandbox…"), "", func() { show(); m.NewSandbox("") }),
			ggui.MenuAction(ui.T("Pull Image…"), "", func() { show(); m.OpenPull("") }),
			ggui.MenuAction(ui.T("Stop All"), "", m.StopAll).BindEnabled(ggui.Const(totalActive > 0)),
			ggui.MenuSeparator(),
			ggui.MenuAction(ui.T("Settings…"), "", func() { show(); m.Go(ui.PageSettings) }),
			ggui.MenuAction(ui.T("Quit GVX Desktop"), "", func() { quit(app, m) }),
		)
		tray.SetMenu(items...)
		if totalRunning > 0 {
			tray.SetTitle(fmt.Sprint(totalRunning))
			tray.SetTooltip(fmt.Sprintf(ui.T("GVX Desktop · %d running"), totalRunning))
		} else {
			tray.SetTitle("")
			tray.SetTooltip("GVX Desktop")
		}
	}
	wanted := ggui.Map(m.Settings, func(s store.Settings) bool { return !s.NoTray })
	ggui.Watch(wanted, func(on bool) {
		switch {
		case on && tray == nil:
			icon, template := trayIconConfig()
			tray = app.AddTray(ggui.TrayConfig{Icon: icon, Template: template, Tooltip: "GVX Desktop"})
			rebuild()
		case !on && tray != nil:
			tray.Remove()
			tray = nil
		}
	})
	// The menu tells of all of these, so each builds it again. Of the
	// sandboxes it tells only names and whether they run: a line of a log
	// changes a row, and must not replace a menu that may be open.
	type entry struct {
		id, name string
		active   bool
		control  bool
		paused   bool
	}
	entries := ggui.Map(m.Rows, func(rows []ui.Row) []entry {
		out := make([]entry, len(rows))
		for i, r := range rows {
			out[i] = entry{r.Spec.ID, r.Spec.Name, r.State.Status.Active(), r.State.Control, r.State.Paused}
		}
		return out
	}).WithEqual(slices.Equal[[]entry])
	ggui.Watch(entries, func([]entry) { rebuild() })
	ggui.Watch(m.EngineRunning, func(int) { rebuild() })
	ggui.Watch(m.Menus, func(ui.MenuState) { rebuild() })
	ggui.Watch(m.Engine, func(ui.Engine) { rebuild() })
	ggui.Watch(m.Update, func(ui.Update) { rebuild() })
	ggui.Watch(appUpdate, func(trayUpdate) { rebuild() })
	ggui.Watch(m.Lang, func(string) { rebuild() })
}

// trayIconConfig picks the tray icon image and template flag for the host OS.
// macOS uses a monochrome template silhouette for its menu bar; Windows uses
// the full-color mark in its taskbar notification area.
func trayIconConfig() (image.Image, bool) {
	return trayIconConfigForOS(runtime.GOOS)
}

func trayIconConfigForOS(goos string) (image.Image, bool) {
	if goos == "windows" {
		return trayColorIcon(), false
	}
	return trayTemplateIcon(), true
}

// trayColorIcon decodes the app's full-color mark for non-template trays
// (Windows taskbar). Returning nil falls back to the executable's color icon.
func trayColorIcon() image.Image {
	src, err := png.Decode(bytes.NewReader(ui.MarkPNG()))
	if err != nil {
		return nil
	}
	return src
}

// trayTemplateIcon is the app's mark as the macOS menu bar wants it, one color:
// the helmet's shell, which is the light part of the mark, with the visor open in it.
func trayTemplateIcon() image.Image {
	const size, glyph = 36, 32 // 18pt at 2x, with a point of air around
	src, err := png.Decode(bytes.NewReader(ui.MarkPNG()))
	if err != nil {
		return image.NewNRGBA(image.Rect(0, 0, size, size))
	}
	b := src.Bounds()
	n := b.Dx() / glyph
	img := image.NewNRGBA(image.Rect(0, 0, size, size))
	for y := range glyph {
		for x := range glyph {
			var light float64
			for dy := range n {
				for dx := range n {
					r, g, bl, a := src.At(b.Min.X+x*n+dx, b.Min.Y+y*n+dy).RGBA()
					if a == 0 {
						continue
					}
					if (0.299*float64(r)+0.587*float64(g)+0.114*float64(bl))/float64(a) > 0.8 {
						light += float64(a) / 0xffff
					}
				}
			}
			img.SetNRGBA(x+(size-glyph)/2, y+(size-glyph)/2, color.NRGBA{A: uint8(255 * light / float64(n*n))})
		}
	}
	return img
}
