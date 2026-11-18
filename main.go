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
	"syscall"
	"time"

	"github.com/ironpark/ggui"
	"github.com/ironpark/ggui/record"
	gguiruntime "github.com/ironpark/ggui/runtime"
	uitheme "github.com/ironpark/ggui/ui/theme"

	"github.com/ironpark/gvx-desktop/internal/recipes"
	"github.com/ironpark/gvx-desktop/internal/store"
	"github.com/ironpark/gvx-desktop/internal/ui"
)

// version is the app's, which a release build sets with -X main.version.
var version = "dev"

func main() {
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
			Title:          "gvx Desktop",
			Width:          1280,
			Height:         840,
			MinWidth:       880,
			MinHeight:      600,
			Resizable:      true,
			TitlebarHidden: ui.TitlebarHidden,
			TrafficLights:  &ggui.Point{X: 18, Y: 18},
			Inspector:      "f12",
		}
		app = ggui.New(cfg, func() ggui.Widget { return ui.Build(m) })
		m.Attach(app)
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
		app.OnCloseRequest(func() bool {
			set := ggui.Peek(m.Settings)
			if set.HideOnClose && !set.NoTray && trayAvailable() {
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
	// Whatever ended the app, no sandbox outlives it: gvx has no daemon to
	// adopt them.
	m.Supervisor().StopAll(10 * time.Second)
	return runErr
}

// requestQuit closes the app now when nothing runs, stops the sandboxes
// first when the settings say so, and asks otherwise.
func requestQuit(app *ggui.App, m *ui.Model) bool {
	if len(m.Supervisor().Running()) == 0 {
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
	pages := []string{ui.PageDashboard, ui.PageSandboxes, ui.PageContainers, ui.PagePods, ui.PageImages, ui.PageVolumes, ui.PageSnapshots, ui.PageDocker, ui.PageKubernetes}
	for i, p := range pages {
		app.Shortcut(fmt.Sprintf("cmd+%d", i+1), func() { m.Go(p) })
	}
	app.Shortcut("cmd+k", func() { m.PaletteQuery.Set(""); m.PaletteOpen.Set(true) })
	if !ggui.NativeMenu() {
		app.Shortcut("cmd+n", func() { m.NewSandbox("") })
		app.Shortcut("cmd+r", m.RefreshAll)
		app.Shortcut("cmd+,", func() { m.Go(ui.PageSettings) })
	}
}

func installMenus(app *ggui.App, m *ui.Model) {
	app.SetMenu(
		ggui.Menu(ui.T("File"),
			ggui.MenuAction(ui.T("New Sandbox…"), "cmd+n", func() { m.NewSandbox("") }),
			ggui.MenuAction(ui.T("Pull Image…"), "cmd+shift+p", func() { m.OpenPull("") }),
			ggui.MenuAction(ui.T("Create Volume…"), "", func() { m.VolumeOpen.Set(true) }),
			ggui.MenuSeparator(),
			ggui.MenuAction(ui.T("Settings…"), "cmd+,", func() { m.Go(ui.PageSettings) }),
		),
		ggui.EditMenu(ggui.EditActions{}),
		ggui.Menu(ui.T("Sandbox"),
			ggui.MenuAction(ui.T("Start Docker Engine"), "", func() { m.CreateFromRecipe(recipes.Docker); m.Go(ui.PageDocker) }),
			ggui.MenuAction(ui.T("Start Kubernetes"), "", func() { m.CreateFromRecipe(recipes.Kubernetes); m.Go(ui.PageKubernetes) }),
			ggui.MenuSeparator(),
			ggui.MenuAction(ui.T("Stop All"), "", m.StopAll),
		),
		ggui.Menu("Docker", m.RecipeMenu(recipes.Docker, app.ShowFromTray)...),
		ggui.Menu("Kubernetes", m.RecipeMenu(recipes.Kubernetes, app.ShowFromTray)...),
		ggui.Menu(ui.T("View"),
			ggui.MenuAction(ui.T("Command Palette"), "cmd+k", func() { m.PaletteQuery.Set(""); m.PaletteOpen.Set(true) }),
			ggui.MenuAction(ui.T("Refresh"), "cmd+r", m.RefreshAll),
			ggui.MenuSeparator(),
			ggui.MenuAction(ui.T("Dashboard"), "", func() { m.Go(ui.PageDashboard) }),
			ggui.MenuAction(ui.T("Sandboxes"), "", func() { m.Go(ui.PageSandboxes) }),
			ggui.MenuAction(ui.T("Containers"), "", func() { m.Go(ui.PageContainers) }).BindEnabled(m.ContainersOpen),
			ggui.MenuAction(ui.T("Pods"), "", func() { m.Go(ui.PagePods) }).BindEnabled(m.PodsOpen),
			ggui.MenuAction(ui.T("Images"), "", func() { m.Go(ui.PageImages) }),
			ggui.MenuAction(ui.T("Volumes"), "", func() { m.Go(ui.PageVolumes) }),
			ggui.MenuAction(ui.T("Snapshots"), "", func() { m.Go(ui.PageSnapshots) }),
		),
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
	rebuild := func() {
		if tray == nil {
			return
		}
		ui.SetLanguage(ggui.Peek(m.Lang))
		engine, update := ggui.Peek(m.Engine), ggui.Peek(m.Update)
		items := []ggui.MenuItem{
			ggui.MenuAction(ui.T("Open gvx Desktop"), "", show),
			ggui.MenuAction(ui.EngineLine(engine), "", nil).BindEnabled(ggui.Const(false)),
		}
		if update.Available(engine) && !update.Installing {
			items = append(items, ggui.MenuAction(ui.T("Update rungv to %s", update.Name()), "", m.InstallRungv))
		}
		items = append(items, ggui.MenuSeparator())
		rows := ggui.Peek(m.Rows)
		running := 0
		for _, r := range rows {
			id, name := r.Spec.ID, r.Spec.Name
			label := "○ " + name
			actions := []ggui.MenuItem{ggui.MenuAction(ui.T("Start"), "", func() { m.StartSandbox(id) })}
			if r.State.Status.Active() {
				running++
				label = "● " + name
				actions = []ggui.MenuItem{
					ggui.MenuAction(ui.T("Stop"), "", func() { m.StopSandbox(id) }),
					ggui.MenuAction(ui.T("Restart"), "", func() { m.RestartSandbox(id) }),
				}
			}
			actions = append(actions, ggui.MenuAction(ui.T("Show Logs"), "", func() { show(); m.ShowSandbox(id, 0) }))
			items = append(items, ggui.Menu(label, actions...))
		}
		if len(rows) == 0 {
			items = append(items, ggui.MenuAction(ui.T("No sandboxes"), "", nil).BindEnabled(ggui.Const(false)))
		}
		items = append(items,
			ggui.MenuSeparator(),
			ggui.Menu("Docker", m.RecipeMenu(recipes.Docker, show)...),
			ggui.Menu("Kubernetes", m.RecipeMenu(recipes.Kubernetes, show)...),
			ggui.MenuSeparator(),
			ggui.MenuAction(ui.T("New Sandbox…"), "", func() { show(); m.NewSandbox("") }),
			ggui.MenuAction(ui.T("Pull Image…"), "", func() { show(); m.OpenPull("") }),
			ggui.MenuAction(ui.T("Stop All"), "", m.StopAll).BindEnabled(ggui.Const(running > 0)),
			ggui.MenuSeparator(),
			ggui.MenuAction(ui.T("Settings…"), "", func() { show(); m.Go(ui.PageSettings) }),
			ggui.MenuAction(ui.T("Quit gvx Desktop"), "", func() { quit(app, m) }),
		)
		tray.SetMenu(items...)
		if running > 0 {
			tray.SetTitle(fmt.Sprint(running))
			tray.SetTooltip(fmt.Sprintf(ui.T("gvx Desktop · %d running"), running))
		} else {
			tray.SetTitle("")
			tray.SetTooltip("gvx Desktop")
		}
	}
	wanted := ggui.Map(m.Settings, func(s store.Settings) bool { return !s.NoTray })
	ggui.Watch(wanted, func(on bool) {
		switch {
		case on && tray == nil:
			tray = app.AddTray(ggui.TrayConfig{Icon: trayIcon(), Template: true, Tooltip: "gvx Desktop"})
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
	}
	entries := ggui.Map(m.Rows, func(rows []ui.Row) []entry {
		out := make([]entry, len(rows))
		for i, r := range rows {
			out[i] = entry{r.Spec.ID, r.Spec.Name, r.State.Status.Active()}
		}
		return out
	}).WithEqual(slices.Equal[[]entry])
	ggui.Watch(entries, func([]entry) { rebuild() })
	ggui.Watch(m.Menus, func(ui.MenuState) { rebuild() })
	ggui.Watch(m.Engine, func(ui.Engine) { rebuild() })
	ggui.Watch(m.Update, func(ui.Update) { rebuild() })
	ggui.Watch(m.Lang, func(string) { rebuild() })
}

// trayIcon is the app's mark as the tray wants it, one color: the helmet's
// shell, which is the light part of the mark, with the visor open in it.
func trayIcon() image.Image {
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
