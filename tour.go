package main

import (
	"os"
	"time"

	"github.com/ironpark/ggui"
	"github.com/ironpark/ggui/record"
	uitheme "github.com/ironpark/ggui/ui/theme"

	"github.com/ironpark/gvx-desktop/internal/registry"
	"github.com/ironpark/gvx-desktop/internal/rungv"
	"github.com/ironpark/gvx-desktop/internal/store"
	"github.com/ironpark/gvx-desktop/internal/ui"
)

// tourStart is the time the tour's clock reads, as renderScreens's does.
var tourStart = time.Date(2026, 11, 16, 15, 30, 0, 0, time.Local)

// The README's animation is a walk through the app with demo data, as
// renderScreens draws it, with the app's motion and nothing captured from
// the screen. ggui record draws it from the repository's root (main calls
// record.Main):
//
//	ggui record -o assets/tour.gif --width 960 --fps 10
//
// It is drawn at twice its size, as a Retina display shows it, and scaled
// down to 960 pixels wide, which reads sharper than drawing it at 1x.
func init() {
	var (
		m    *ui.Model
		dir  string
		dark *ggui.StateValue[bool]
	)
	record.Register(record.Tour{
		Size:  ggui.Sz(1280, 800),
		Scale: 2,
		Start: tourStart,
		Setup: func(p *ggui.Probe) {
			rungv.HomeDir = func() (string, error) { return demoHome, nil }
			var err error
			if dir, err = os.MkdirTemp("", "gvxd-tour"); err != nil {
				panic(err)
			}
			st := store.Store{Dir: dir}
			if err := st.SaveSandboxes(demoSpecs()); err != nil {
				panic(err)
			}
			set := store.DefaultSettings()
			set.Theme, set.Language = "dark", "en"
			_ = st.SaveSettings(set)

			m = ui.NewModel(st, dir)
			m.Offline = true
			m.Attach(p)
			demoState(m, tourStart)
			dark = ggui.State(true)
			preset := uitheme.Preset{Base: uitheme.BaseZinc, Accent: uitheme.AccentBlue}
			uitheme.Bind(dark, preset.Dark(), preset.Light())
		},
		Build: func() ggui.Widget { return ui.Build(m) },
		Script: func(r *record.Recorder) {
			defer os.RemoveAll(dir)
			const look, glance = 1800 * time.Millisecond, 1100 * time.Millisecond

			r.Hold(look) // the dashboard
			r.Then(look, func() { m.Page.Set(ui.PageSandboxes) })
			r.Then(glance, func() { m.Select(demoIDs[1]) })
			r.Then(look, func() { m.Detail.Set(1) })
			r.Then(2400*time.Millisecond, func() { m.NewSandbox("postgres:17-alpine") })
			r.Then(glance/2, func() { m.EditorOpen.Set(false) })
			r.Then(look, func() { m.Page.Set(ui.PageImages) })
			r.Then(2600*time.Millisecond, func() {
				m.OpenPull("nginx")
				m.SearchFor.Set("nginx")
				m.SearchResults.Set(demoSearch())
				m.SearchTagsOf.Set("nginx")
				m.SearchTags.Set([]string{"stable-alpine", "mainline-alpine", "alpine", "1.31", "1.31-alpine", "stable", "latest", "bookworm"})
			})
			r.Then(glance/2, func() { m.PullOpen.Set(false) })
			r.Then(look, func() { m.Page.Set(ui.PageDocker) })
			r.Then(look, func() { m.Page.Set(ui.PageContainers) })
			r.Then(look, func() { m.Page.Set(ui.PagePods) })
			r.Then(look, func() { m.Page.Set(ui.PageKubernetes) })
			r.Then(look, func() { m.Page.Set(ui.PageSettings) })
			r.Then(look, func() { dark.Set(false) })
			r.Then(look, func() { m.PaletteOpen.Set(true) })
			r.Then(glance/2, func() { m.PaletteOpen.Set(false) })
			r.Then(look, func() { m.Page.Set(ui.PageDashboard) })
			r.Then(glance, func() { dark.Set(true) })
		},
	})
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
