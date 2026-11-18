package ui

import (
	"fmt"
	"strings"

	"github.com/ironpark/ggui"
	"github.com/ironpark/ggui/icons/lucide"
	"github.com/ironpark/ggui/ui"
	"github.com/ironpark/ggui/ui/icons"
	"github.com/ironpark/ggui/widget"

	"github.com/ironpark/gvx-desktop/internal/recipes"
	"github.com/ironpark/gvx-desktop/internal/rungv"
	"github.com/ironpark/gvx-desktop/internal/sandbox"
)

func dashboardPage(m *Model) ggui.Widget {
	return page(
		pageHeader(tr("Dashboard"),
			ui.Caption(tr("Linux containers in a single unprivileged process — no VM, no daemon, no root.")),
			ui.Button(tr("Pull image"), func() { m.OpenPull("") }).Outline().Icon(glyph(lucide.Download, 16)),
			ui.Button(tr("New sandbox"), func() { m.NewSandbox("") }).Icon(glyph(lucide.Plus, 16)),
		),
		engineAlert(m),
		metrics(m),
		widget.Responsive(900,
			widget.Row(widget.Flex(recentSandboxes(m), 3), widget.Flex(engineCard(m), 2)).Gap(16).Align(widget.AlignStart),
			widget.Column(recentSandboxes(m), engineCard(m)).Gap(16).Stretch(),
		),
		widget.Column(
			sectionTitle(tr("Quick start"), tr("Presets for the workloads gvx documents. Each runs as one sandbox.")),
			recipeCards(m),
		).Gap(12).Stretch(),
	)
}

// engineAlert explains why the engine cannot run sandboxes, with the fix.
func engineAlert(m *Model) ggui.Widget {
	return ggui.View(m.Engine, func(e Engine) ggui.Widget {
		switch {
		case !e.Checked:
			return widget.Box()
		case e.Path == "":
			return ui.Alert(tr("rungv was not found"),
				tr("gvx Desktop drives the rungv command line. Download it from github.com/gfire-sigs/gvx/releases, put it on your PATH or in ~/.gvx/bin, or choose its location in Settings.")).
				Destructive().Icon(icons.Alert).
				Action(ui.Button(tr("Open Settings"), func() { m.Go(PageSettings) }).Size(ui.ButtonExtraSmall).Outline())
		case e.Err != "":
			return ui.Alert(tr("The engine did not start"), e.Err).Destructive().Icon(icons.Alert).
				Action(ui.Button(tr("Retry"), func() { m.DetectEngine(nil) }).Size(ui.ButtonExtraSmall).Outline())
		case rungv.NeedsSigning(e.Info):
			return ui.Alert(tr("rungv needs the hypervisor entitlement"),
				tr("On macOS, rungv runs guests through Hypervisor.framework and must be signed with com.apple.security.hypervisor. Sign it here, or run: ")+rungv.SignCommand(e.Path)).
				Destructive().Icon(icons.Alert).
				Action(ui.Button(tr("Sign rungv"), m.SignRungv).Size(ui.ButtonExtraSmall))
		case len(e.Info.AvailableBackends) == 0:
			return ui.Alert(tr("No execution backend"), strings.Join(e.Info.BackendWarnings, " ")).Destructive().Icon(icons.Alert)
		case len(e.Info.BackendWarnings) > 0:
			return ui.Alert(tr("Engine warnings"), strings.Join(e.Info.BackendWarnings, " ")).Icon(icons.Info)
		}
		return widget.Box()
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
		metric(tr("Running"), widget.Row(big(running), ui.CaptionOf(ggui.Map(total, func(t string) string { return tr("of %s", t) })).Size(13)).Gap(8).Align(widget.AlignBaseline), lucide.Activity, func() { m.Go(PageSandboxes) }),
		metric(tr("Images"), big(images), lucide.Layers, func() { m.Go(PageImages) }),
		metric(tr("Volumes"), big(volumes), lucide.HardDrive, func() { m.Go(PageVolumes) }),
		metric(tr("Snapshots"), big(ggui.Map(m.Snapshots, func(s []sandbox.Snapshot) string { return itoa(len(s)) })), lucide.Camera, func() { m.Go(PageSnapshots) }),
	}
	return widget.Responsive(760, widget.Grid(4, cards...).Gap(12), widget.Grid(2, cards...).Gap(12))
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
	list := widget.EachKeyed(m.Rows, func(r Row) string { return r.Spec.ID }, func(row widget.EachItem[Row]) ggui.Widget {
		id := row.Item.Spec.ID
		return ggui.View(row.Value, func(r Row) ggui.Widget {
			return ui.Item(r.Spec.Name, r.Spec.Image).
				Media(widget.Box(statusLabel(r.State)).Width(116)).
				Action(sandboxQuickActions(m, r)).
				OnTap(func() { m.ShowSandbox(id, 0) }).Small()
		})
	}).Gap(4).Else(func() ggui.Widget {
		return ui.Empty(tr("No sandboxes yet"), tr("Create one from an image, or start a recipe below.")).
			Media(glyph(lucide.Box, 28).Color(colorGray)).
			Action(ui.Button(tr("New sandbox"), func() { m.NewSandbox("") }).Outline())
	})
	return ui.Card(
		ui.CardHeader(tr("Sandboxes"), tr("Every sandbox is one rungv process supervised by this app.")).
			Action(ui.Button(tr("View all"), func() { m.Go(PageSandboxes) }).Ghost().Size(ui.ButtonSmall)),
		ui.CardContent(list),
	)
}

// sandboxQuickActions are the start/stop buttons of a row.
func sandboxQuickActions(m *Model, r Row) ggui.Widget {
	id := r.Spec.ID
	if r.State.Status.Active() {
		return widget.Row(
			tip(iconButton(lucide.Square, tr("Stop %s", r.Spec.Name), func() { m.StopSandbox(id) }).BindDisabled(ggui.Const(r.State.Status == sandbox.Stopping)), tr("Stop")),
		).Gap(2)
	}
	return tip(iconButton(lucide.Play, tr("Start %s", r.Spec.Name), func() { m.StartSandbox(id) }), tr("Start"))
}

func engineCard(m *Model) ggui.Widget {
	body := ggui.View(m.Engine, func(e Engine) ggui.Widget {
		if !e.Checked {
			return widget.Row(ui.Spinner(), ui.Caption(tr("Looking for rungv…"))).Gap(8)
		}
		if e.Path == "" {
			return ui.Caption(tr("rungv not found."))
		}
		backends := strings.Join(e.Info.AvailableBackends, ", ")
		if backends == "" {
			backends = tr("none")
		}
		return widget.Column(
			kvText(tr("Version"), e.Version.Version),
			kvText(tr("Host"), e.Info.OS+"/"+e.Info.Architecture+fmt.Sprintf(tr(" · %d CPUs"), e.Info.NumCPU)),
			kvText(tr("Default backend"), e.Info.DefaultBackend),
			kvText(tr("Available"), backends),
			kvText(tr("Host lockdown"), e.Info.HostLockdown),
			kvText(tr("Binary"), e.Path),
		).Gap(8).Stretch()
	})
	return ui.Card(
		ui.CardHeader(tr("Engine"), tr("The rungv binary and what this host offers it.")).
			Action(iconButton(lucide.RefreshCw, tr("Refresh"), m.RefreshAll)),
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
			points = append(points, widget.Row(glyph(lucide.Check, 14).Color(colorGreen), ui.Caption(tr(h))).Gap(8).Align(widget.AlignStart))
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
				return widget.Row(statusLabel(sandbox.State{Status: s}), widget.Spacer(),
					ui.Button(tr("Open"), func() { m.Go(page) }).Outline().Size(ui.ButtonSmall)).Align(widget.AlignCenter)
			case s >= 0:
				return widget.Row(ui.Button(tr("Start"), func() { m.CreateFromRecipe(r.Key); m.Go(page) }).Size(ui.ButtonSmall).Icon(glyph(lucide.Play, 14)),
					ui.Button(tr("Details"), func() { m.Go(page) }).Ghost().Size(ui.ButtonSmall)).Gap(8)
			}
			return ui.Button(tr("Set up %s", r.Title), func() { m.CreateFromRecipe(r.Key); m.Go(page) }).Size(ui.ButtonSmall).Icon(glyph(lucide.Play, 14))
		})
		cards = append(cards, ui.Card(widget.Column(
			widget.Row(
				widget.Box(glyph(icon, 20).Color(colorBlue)).Pad(8).Radius(10).Fill(rgbA(0x3b82f6, 0x22)),
				widget.Column(ui.Heading(r.Title).Size(15), ui.Mono(r.Image).Size(11)).Gap(2),
			).Gap(12),
			ui.Caption(tr(r.Summary)),
			widget.Column(points...).Gap(6),
			action,
		).Gap(12).Stretch()))
	}
	return widget.Responsive(760, widget.Grid(2, cards...).Gap(12), widget.Column(cards...).Gap(12).Stretch())
}
