// Command gvx-desktop is a desktop app for gvx: it runs Linux containers
// as unprivileged gvx sandboxes, with no VM and no daemon, and manages
// their images, volumes and snapshots.
package main

import (
	"errors"
	"flag"
	"fmt"
	"image"
	"image/color"
	"log"
	"math"
	"os"
	"os/signal"
	"runtime"
	"syscall"
	"time"

	"github.com/ironpark/ggui"
	gguiruntime "github.com/ironpark/ggui/runtime"
	uitheme "github.com/ironpark/ggui/ui/theme"

	"github.com/ironpark/gvx-desktop/internal/recipes"
	"github.com/ironpark/gvx-desktop/internal/store"
	"github.com/ironpark/gvx-desktop/internal/ui"
)

func main() {
	dataDir := flag.String("data-dir", "", "directory for sandboxes, settings and snapshots (default: the user config dir)")
	renderDir := flag.String("render-dir", "", "render the screens to PNG files in this directory, then exit")
	renderLang := flag.String("render-lang", "en", "the language -render-dir renders in: en or ko")
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
	if err := run(st, dir); err != nil {
		log.Fatal(err)
	}
}

func run(st store.Store, dir string) error {
	var (
		app *ggui.App
		m   *ui.Model
	)
	dispose := ggui.Root(func() {
		m = ui.NewModel(st, dir)
		cfg := ggui.Config{
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
			installTray(app, m)
			// The menu bar is worded as it is built, so a change of
			// language builds it again.
			ggui.Watch(m.Lang, func(lang string) {
				ui.SetLanguage(lang)
				installMenus(app, m)
			})
		})
		installShortcuts(app, m)
		installMenus(app, m)

		ui.SetQuit(app.Close)
		app.OnCloseRequest(func() bool {
			set := ggui.Peek(m.Settings)
			if set.HideOnClose && trayAvailable() {
				app.Hide()
				return false
			}
			return requestQuit(app, m)
		})
	})
	defer dispose()

	err := app.SingleInstance("io.github.ironpark.gvx-desktop", func(gguiruntime.Launch) {
		app.Show()
		app.Focus()
	})
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
	app.Show()
	app.Focus()
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
	pages := []string{ui.PageDashboard, ui.PageSandboxes, ui.PageImages, ui.PageVolumes, ui.PageSnapshots, ui.PageDocker, ui.PageKubernetes}
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
			ggui.MenuAction(ui.T("Stop All"), "", func() { m.StopAllAndThen(func() {}) }),
		),
		ggui.Menu(ui.T("View"),
			ggui.MenuAction(ui.T("Command Palette"), "cmd+k", func() { m.PaletteQuery.Set(""); m.PaletteOpen.Set(true) }),
			ggui.MenuAction(ui.T("Refresh"), "cmd+r", m.RefreshAll),
			ggui.MenuSeparator(),
			ggui.MenuAction(ui.T("Dashboard"), "", func() { m.Go(ui.PageDashboard) }),
			ggui.MenuAction(ui.T("Sandboxes"), "", func() { m.Go(ui.PageSandboxes) }),
			ggui.MenuAction(ui.T("Images"), "", func() { m.Go(ui.PageImages) }),
			ggui.MenuAction(ui.T("Volumes"), "", func() { m.Go(ui.PageVolumes) }),
			ggui.MenuAction(ui.T("Snapshots"), "", func() { m.Go(ui.PageSnapshots) }),
		),
	)
}

// installTray puts gvx in the menu bar (macOS) or notification area
// (Windows): the app keeps sandboxes running with its window hidden.
func installTray(app *ggui.App, m *ui.Model) {
	if !trayAvailable() {
		return
	}
	tray := app.AddTray(ggui.TrayConfig{Icon: trayIcon(), Template: true, Tooltip: "gvx Desktop"})
	show := func() { app.Show(); app.Focus() }
	ggui.Watch(m.Rows, func(rows []ui.Row) {
		ui.SetLanguage(m.Lang.Get()) // read here, so the menu follows the language too
		items := []ggui.MenuItem{ggui.MenuAction(ui.T("Open gvx Desktop"), "", show), ggui.MenuSeparator()}
		running := 0
		for _, r := range rows {
			id, name := r.Spec.ID, r.Spec.Name
			st := r.State.Status
			if st.Active() {
				running++
			}
			label := "○ " + name
			action := ggui.MenuAction(ui.T("Start"), "", func() { m.StartSandbox(id) })
			if st.Active() {
				label = "● " + name
				action = ggui.MenuAction(ui.T("Stop"), "", func() { m.StopSandbox(id) })
			}
			items = append(items, ggui.Menu(label,
				action,
				ggui.MenuAction(ui.T("Show Logs"), "", func() { show(); m.ShowSandbox(id, 0) }),
			))
		}
		if len(rows) == 0 {
			items = append(items, ggui.MenuAction(ui.T("No sandboxes"), "", nil).BindEnabled(ggui.Const(false)))
		}
		items = append(items,
			ggui.MenuSeparator(),
			ggui.MenuAction(ui.T("New Sandbox…"), "", func() { show(); m.NewSandbox("") }),
			ggui.MenuAction(ui.T("Stop All"), "", func() { m.StopAllAndThen(func() {}) }).BindEnabled(ggui.Const(running > 0)),
			ggui.MenuSeparator(),
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
	})
}

// trayIcon draws a monochrome isometric box, the gvx mark, for the tray.
func trayIcon() image.Image {
	const size = 36 // 18pt at 2x
	img := image.NewNRGBA(image.Rect(0, 0, size, size))
	c := float64(size) / 2
	r := float64(size)*0.42 - 1
	// The hexagon's six corners, pointy side up.
	var pts [6][2]float64
	for i := range pts {
		a := math.Pi/3*float64(i) - math.Pi/2
		pts[i] = [2]float64{c + r*math.Cos(a), c + r*math.Sin(a)}
	}
	line := func(x0, y0, x1, y1, w float64) {
		for y := 0; y < size; y++ {
			for x := 0; x < size; x++ {
				px, py := float64(x)+0.5, float64(y)+0.5
				dx, dy := x1-x0, y1-y0
				t := math.Max(0, math.Min(1, ((px-x0)*dx+(py-y0)*dy)/(dx*dx+dy*dy)))
				d := math.Hypot(px-(x0+t*dx), py-(y0+t*dy))
				a := math.Max(0, math.Min(1, w/2+0.5-d))
				if a > 0 {
					old := float64(img.NRGBAAt(x, y).A) / 255
					img.SetNRGBA(x, y, color.NRGBA{0, 0, 0, uint8(255 * math.Max(old, a))})
				}
			}
		}
	}
	const w = 2.6
	for i := range pts {
		j := (i + 1) % 6
		line(pts[i][0], pts[i][1], pts[j][0], pts[j][1], w)
	}
	// The three inner edges meeting at the centre.
	line(c, c, pts[1][0], pts[1][1], w)
	line(c, c, pts[3][0], pts[3][1], w)
	line(c, c, pts[5][0], pts[5][1], w)
	return img
}
