package ui

import (
	"fmt"
	"strings"

	"github.com/ironpark/ggui"
	"github.com/ironpark/ggui/icons/lucide"
	"github.com/ironpark/ggui/ui"
	"github.com/ironpark/ggui/ui/icons"

	"github.com/ironpark/gvx-desktop/internal/recipes"
	"github.com/ironpark/gvx-desktop/internal/rungv"
	"github.com/ironpark/gvx-desktop/internal/sandbox"
)

func dashboardPage(m *Model) ggui.Widget {
	return page(
		pageHeader("Dashboard",
			ui.Caption("Linux containers in a single unprivileged process — no VM, no daemon, no root."),
			ui.Button("Pull image", func() { m.OpenPull("") }).Outline().Icon(glyph(lucide.Download, 16)),
			ui.Button("New sandbox", func() { m.NewSandbox("") }).Icon(glyph(lucide.Plus, 16)),
		),
		engineAlert(m),
		metrics(m),
		ggui.Responsive(900,
			ggui.Row(ggui.Flex(recentSandboxes(m), 3), ggui.Flex(engineCard(m), 2)).Gap(16).Align(ggui.AlignStart),
			ggui.Column(recentSandboxes(m), engineCard(m)).Gap(16).Stretch(),
		),
		ggui.Column(
			sectionTitle("Quick start", "Presets for the workloads gvx documents. Each runs as one sandbox."),
			recipeCards(m),
		).Gap(12).Stretch(),
	)
}

// engineAlert explains why the engine cannot run sandboxes, with the fix.
func engineAlert(m *Model) ggui.Widget {
	return ggui.View(m.Engine, func(e Engine) ggui.Widget {
		switch {
		case !e.Checked:
			return ggui.Box()
		case e.Path == "":
			return ui.Alert("rungv was not found",
				"gvx Desktop drives the rungv command line. Download it from github.com/gfire-sigs/gvx/releases, put it on your PATH or in ~/.gvx/bin, or choose its location in Settings.").
				Destructive().Icon(icons.Alert).
				Action(ui.Button("Open Settings", func() { m.Go(PageSettings) }).Size(ui.ButtonExtraSmall).Outline())
		case e.Err != "":
			return ui.Alert("The engine did not start", e.Err).Destructive().Icon(icons.Alert).
				Action(ui.Button("Retry", func() { m.DetectEngine(nil) }).Size(ui.ButtonExtraSmall).Outline())
		case rungv.NeedsSigning(e.Info):
			return ui.Alert("rungv needs the hypervisor entitlement",
				"On macOS, rungv runs guests through Hypervisor.framework and must be signed with com.apple.security.hypervisor. Sign it here, or run: "+rungv.SignCommand(e.Path)).
				Destructive().Icon(icons.Alert).
				Action(ui.Button("Sign rungv", m.SignRungv).Size(ui.ButtonExtraSmall))
		case len(e.Info.AvailableBackends) == 0:
			return ui.Alert("No execution backend", strings.Join(e.Info.BackendWarnings, " ")).Destructive().Icon(icons.Alert)
		case len(e.Info.BackendWarnings) > 0:
			return ui.Alert("Engine warnings", strings.Join(e.Info.BackendWarnings, " ")).Icon(icons.Info)
		}
		return ggui.Box()
	})
}

func metrics(m *Model) ggui.Widget {
	big := func(r ggui.Readable[string]) ggui.Widget { return ui.TitleOf(r).Size(28) }
	running := ggui.Map(m.Running, func(n int) string { return itoa(n) })
	total := ggui.Map(m.Specs, func(s []sandbox.Spec) string { return itoa(len(s)) })
	images := ggui.Combine(m.Images, m.Disk, func(l Listing[rungv.Image], d []rungv.DiskUsage) string {
		return countWithSize(len(l.Items), d, "Images")
	})
	volumes := ggui.Combine(m.Volumes, m.Disk, func(l Listing[rungv.Volume], d []rungv.DiskUsage) string {
		return countWithSize(len(l.Items), d, "Local Volumes")
	})
	cards := []ggui.Widget{
		metric("Running", ggui.Row(big(running), ui.CaptionOf(ggui.Map(total, func(t string) string { return "of " + t })).Size(13)).Gap(8).Align(ggui.AlignBaseline), lucide.Activity, func() { m.Go(PageSandboxes) }),
		metric("Images", big(images), lucide.Layers, func() { m.Go(PageImages) }),
		metric("Volumes", big(volumes), lucide.HardDrive, func() { m.Go(PageVolumes) }),
		metric("Snapshots", big(ggui.Map(m.Snapshots, func(s []sandbox.Snapshot) string { return itoa(len(s)) })), lucide.Camera, func() { m.Go(PageSnapshots) }),
	}
	return ggui.Responsive(760, ggui.Grid(4, cards...).Gap(12), ggui.Grid(2, cards...).Gap(12))
}

func countWithSize(n int, rows []rungv.DiskUsage, kind string) string {
	for _, r := range rows {
		if r.Type == kind && r.Size != "" && r.Size != "0B" {
			return fmt.Sprintf("%d · %s", n, r.Size)
		}
	}
	return itoa(n)
}

func recentSandboxes(m *Model) ggui.Widget {
	list := ggui.EachKeyed(m.Rows, func(r Row) string { return r.Spec.ID }, func(row ggui.EachItem[Row]) ggui.Widget {
		id := row.Item.Spec.ID
		return ggui.View(row.Value, func(r Row) ggui.Widget {
			return ui.Item(r.Spec.Name, r.Spec.Image).
				Media(ggui.Box(statusLabel(r.State)).Width(116)).
				Action(sandboxQuickActions(m, r)).
				OnTap(func() { m.ShowSandbox(id, 0) }).Small()
		})
	}).Gap(4).Else(func() ggui.Widget {
		return ui.Empty("No sandboxes yet", "Create one from an image, or start a recipe below.").
			Media(glyph(lucide.Box, 28).Color(colorGray)).
			Action(ui.Button("New sandbox", func() { m.NewSandbox("") }).Outline())
	})
	return ui.Card(
		ui.CardHeader("Sandboxes", "Every sandbox is one rungv process supervised by this app.").
			Action(ui.Button("View all", func() { m.Go(PageSandboxes) }).Ghost().Size(ui.ButtonSmall)),
		ui.CardContent(list),
	)
}

// sandboxQuickActions are the start/stop buttons of a row.
func sandboxQuickActions(m *Model, r Row) ggui.Widget {
	id := r.Spec.ID
	if r.State.Status.Active() {
		return ggui.Row(
			tip(iconButton(lucide.Square, "Stop "+r.Spec.Name, func() { m.StopSandbox(id) }).BindDisabled(ggui.Const(r.State.Status == sandbox.Stopping)), "Stop"),
		).Gap(2)
	}
	return tip(iconButton(lucide.Play, "Start "+r.Spec.Name, func() { m.StartSandbox(id) }), "Start")
}

func engineCard(m *Model) ggui.Widget {
	body := ggui.View(m.Engine, func(e Engine) ggui.Widget {
		if !e.Checked {
			return ggui.Row(ui.Spinner(), ui.Caption("Looking for rungv…")).Gap(8)
		}
		if e.Path == "" {
			return ui.Caption("rungv not found.")
		}
		backends := strings.Join(e.Info.AvailableBackends, ", ")
		if backends == "" {
			backends = "none"
		}
		return ggui.Column(
			kvText("Version", e.Version.Version),
			kvText("Host", e.Info.OS+"/"+e.Info.Architecture+fmt.Sprintf(" · %d CPUs", e.Info.NumCPU)),
			kvText("Default backend", e.Info.DefaultBackend),
			kvText("Available", backends),
			kvText("Host lockdown", e.Info.HostLockdown),
			kvText("Binary", e.Path),
		).Gap(8).Stretch()
	})
	return ui.Card(
		ui.CardHeader("Engine", "The rungv binary and what this host offers it.").
			Action(iconButton(lucide.RefreshCw, "Refresh", m.RefreshAll)),
		ui.CardContent(body),
	)
}

func recipeCards(m *Model) ggui.Widget {
	var cards []ggui.Widget
	for _, r := range recipes.All() {
		r := r
		icon := lucide.Container
		page := PageDocker
		if r.Key == recipes.Kubernetes {
			icon, page = lucide.Ship, PageKubernetes
		}
		var points []ggui.Widget
		for _, h := range r.Highlights {
			points = append(points, ggui.Row(glyph(lucide.Check, 14).Color(colorGreen), ui.Caption(h)).Gap(8).Align(ggui.AlignStart))
		}
		state := ggui.Map(m.Rows, func(rows []Row) sandbox.Status {
			if row, ok := m.RecipeSandbox(rows, r.Key); ok {
				return row.State.Status
			}
			return -1
		})
		action := ggui.View(state, func(s sandbox.Status) ggui.Widget {
			switch {
			case s.Active():
				return ggui.Row(statusLabel(sandbox.State{Status: s}), ggui.Spacer(),
					ui.Button("Open", func() { m.Go(page) }).Outline().Size(ui.ButtonSmall)).Align(ggui.AlignCenter)
			case s >= 0:
				return ggui.Row(ui.Button("Start", func() { m.CreateFromRecipe(r.Key); m.Go(page) }).Size(ui.ButtonSmall).Icon(glyph(lucide.Play, 14)),
					ui.Button("Details", func() { m.Go(page) }).Ghost().Size(ui.ButtonSmall)).Gap(8)
			}
			return ui.Button("Set up "+r.Title, func() { m.CreateFromRecipe(r.Key); m.Go(page) }).Size(ui.ButtonSmall).Icon(glyph(lucide.Play, 14))
		})
		cards = append(cards, ui.Card(ggui.Column(
			ggui.Row(
				ggui.Box(glyph(icon, 20).Color(colorBlue)).Pad(8).Radius(10).Fill(rgbA(0x3b82f6, 0x22)),
				ggui.Column(ui.Heading(r.Title).Size(15), ui.Mono(r.Image).Size(11)).Gap(2),
			).Gap(12),
			ui.Caption(r.Summary),
			ggui.Column(points...).Gap(6),
			action,
		).Gap(12).Stretch()))
	}
	return ggui.Responsive(760, ggui.Grid(2, cards...).Gap(12), ggui.Column(cards...).Gap(12).Stretch())
}
