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
		// Drawn as the window it is on a Mac: no title bar, the window
		// buttons over the sidebar.
		Frame:          record.FrameMac,
		TitlebarHidden: true,
		Start:          tourStart,
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
			const look, glance = 1700 * time.Millisecond, 1000 * time.Millisecond
			// press glides the cursor to a point of the window and clicks
			// there: the tour goes through the app as a person would, by
			// its sidebar and its buttons.
			press := func(x, y float64, then time.Duration) {
				r.MoveTo(ggui.Pt(x, y), 550*time.Millisecond)
				r.Click()
				r.Hold(then)
			}
			// The sidebar's rows, top to bottom.
			const side = 70.0
			const (
				dashboard, sandboxes, containers, pods, images = 127.0, 163.0, 199.0, 235.0, 271.0
				docker, kubernetes, settings                   = 423.0, 459.0, 746.0
			)

			r.Hold(look) // the dashboard
			press(side, sandboxes, glance)
			press(520, 314, glance)                 // the docker-engine row
			press(364, 554, look)                   // its Connect tab
			press(1195, 107, 2200*time.Millisecond) // New sandbox
			r.Key("escape")
			r.Hold(glance / 2)

			press(side, containers, glance)
			press(437, 495, look) // the Terminal tab of the container chosen
			press(side, pods, look)

			press(side, images, glance)
			r.MoveTo(ggui.Pt(1183, 107), 550*time.Millisecond) // Pull image
			r.Then(2400*time.Millisecond, func() {
				m.OpenPull("nginx")
				m.SearchFor.Set("nginx")
				m.SearchResults.Set(demoSearch())
				m.SearchTagsOf.Set("nginx")
				m.SearchTags.Set([]string{"stable-alpine", "mainline-alpine", "alpine", "1.31", "1.31-alpine", "stable", "latest", "bookworm"})
			})
			r.Then(glance/2, func() { m.PullOpen.Set(false) })

			press(side, docker, look)
			press(side, kubernetes, look)
			press(side, settings, glance)
			r.MoveTo(ggui.Pt(1254, 26), 550*time.Millisecond) // the theme button
			r.Then(look, func() { dark.Set(false) })
			press(460, 26, look) // the command palette
			r.Key("escape")
			r.Hold(glance / 2)
			press(side, dashboard, look)
			r.MoveTo(ggui.Pt(1254, 26), 550*time.Millisecond)
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
