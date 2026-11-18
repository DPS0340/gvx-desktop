package main

import (
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/ironpark/ggfx"
	"github.com/ironpark/ggui"
	uitheme "github.com/ironpark/ggui/ui/theme"

	"github.com/ironpark/gvx-desktop/internal/registry"
	"github.com/ironpark/gvx-desktop/internal/rungv"
	"github.com/ironpark/gvx-desktop/internal/store"
	"github.com/ironpark/gvx-desktop/internal/ui"
)

// tourFrame is the time between two frames of the tour: 15 a second.
const tourFrame = time.Second / 15

// renderTour walks through the app with demo data, as renderScreens draws
// it, and writes what the window shows as numbered PNG frames in out: its
// motion included, since the frames are a tourFrame apart. The README's
// animation is made from them (see assets/tour.sh).
func renderTour(lang, out string) error {
	if err := os.MkdirAll(out, 0o755); err != nil {
		return err
	}
	tmp, err := os.MkdirTemp("", "gvxd-tour")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmp)
	return offscreen(func() (bool, error) { return true, tour(tmp, out, lang) })
}

func tour(dir, out, lang string) error {
	now := time.Date(2026, 11, 16, 15, 30, 0, 0, time.Local)
	restore := ggui.SetClock(func() time.Time { return now })
	defer restore()

	rungv.HomeDir = func() (string, error) { return demoHome, nil }
	st := store.Store{Dir: dir}
	if err := st.SaveSandboxes(demoSpecs()); err != nil {
		return err
	}
	set := store.DefaultSettings()
	set.Theme, set.Language = "dark", lang
	_ = st.SaveSettings(set)

	const w, h = 1280, 800
	var m *ui.Model
	dark := ggui.State(true)
	p := ggui.ProbeBuilder(func() ggui.Widget { return ui.Build(m) }, ggui.Sz(w, h))
	p.Setup(func() {
		m = ui.NewModel(st, dir)
		m.Offline = true
		m.Attach(p)
		demoState(m, now)
		preset := uitheme.Preset{Base: uitheme.BaseZinc, Accent: uitheme.AccentBlue}
		uitheme.Bind(dark, preset.Dark(), preset.Light())
	})
	defer p.Close()
	p.Frame()

	img := ggfx.NewImage(w, h)
	defer img.Deallocate()
	frame := 0
	var failed error
	// hold shows the window for d, a frame at a time.
	hold := func(d time.Duration) {
		for ; d > 0 && failed == nil; d -= tourFrame {
			p.Advance(tourFrame)
			img.Fill(ggui.Untrack(uitheme.Use).Bg)
			p.Draw(img)
			failed = savePNG(filepath.Join(out, fmt.Sprintf("%04d.png", frame)), img)
			frame++
		}
	}
	// then does a step of the tour and shows what follows from it.
	then := func(d time.Duration, step func()) {
		step()
		p.Frame()
		hold(d)
	}
	const look, glance = 1800 * time.Millisecond, 1100 * time.Millisecond

	hold(look) // the dashboard
	then(look, func() { m.Page.Set(ui.PageSandboxes) })
	then(glance, func() { m.Select(demoIDs[1]) })
	then(look, func() { m.Detail.Set(1) })
	then(2400*time.Millisecond, func() { m.NewSandbox("postgres:17-alpine") })
	then(glance/2, func() { m.EditorOpen.Set(false) })
	then(look, func() { m.Page.Set(ui.PageImages) })
	then(2600*time.Millisecond, func() {
		m.OpenPull("nginx")
		m.SearchFor.Set("nginx")
		m.SearchResults.Set(demoSearch())
		m.SearchTagsOf.Set("nginx")
		m.SearchTags.Set([]string{"stable-alpine", "mainline-alpine", "alpine", "1.31", "1.31-alpine", "stable", "latest", "bookworm"})
	})
	then(glance/2, func() { m.PullOpen.Set(false) })
	then(look, func() { m.Page.Set(ui.PageDocker) })
	then(look, func() { m.Page.Set(ui.PageKubernetes) })
	then(look, func() { m.Page.Set(ui.PageSettings) })
	then(look, func() { dark.Set(false) })
	then(look, func() { m.PaletteOpen.Set(true) })
	then(glance/2, func() { m.PaletteOpen.Set(false) })
	then(look, func() { m.Page.Set(ui.PageDashboard) })
	then(glance, func() { dark.Set(true) })
	return failed
}

// demoHome stands for the home directory in the pictures.
const demoHome = "/Users/me"

// demoSearch is what a registry would answer a search for nginx.
func demoSearch() []registry.Result {
	return []registry.Result{
		{Name: "nginx", Description: "Official build of Nginx.", Stars: 21422, Pulls: 13593185359, Official: true},
		{Name: "nginx/nginx-ingress", Description: "NGINX and NGINX Plus Ingress Controllers for Kubernetes", Stars: 124, Pulls: 1088795042},
		{Name: "nginx/nginx-prometheus-exporter", Description: "NGINX Prometheus Exporter for NGINX and NGINX Plus", Stars: 53, Pulls: 92683082},
		{Name: "bitnami/nginx", Description: "Bitnami Secure Image for nginx", Stars: 208, Pulls: 310412345},
		{Name: "nginxinc/nginx-unprivileged", Description: "Unprivileged NGINX Dockerfiles", Stars: 189, Pulls: 912000111},
		{Name: "linuxserver/nginx", Description: "An Nginx container, brought to you by LinuxServer.io.", Stars: 231, Pulls: 43000000},
	}
}
