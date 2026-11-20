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
		headed(pageHeader(tr("Dashboard"),
			ui.Caption(tr("Linux containers in a single unprivileged process — no VM, no daemon, no root.")),
			ui.Button(tr("Pull image"), func() { m.OpenPull("") }).Outline().Icon(glyph(lucide.Download, 16)),
			ui.Button(tr("New sandbox"), func() { m.NewSandbox("") }).Icon(glyph(lucide.Plus, 16)),
		), engineAlert(m)),
		metrics(m),
		widget.Responsive(900,
			widget.Row(widget.Flex(recentSandboxes(m), 3), widget.Flex(engineCard(m), 2)).Gap(16).Stretch(),
			widget.Column(recentSandboxes(m), engineCard(m)).Gap(16).Stretch(),
		),
		widget.Column(
			sectionTitle(tr("Quick start"), tr("Presets for the workloads gvx documents. Each runs as one sandbox.")),
			recipeCards(m),
		).Gap(12).Stretch(),
	)
}

// engineAlert explains why the engine cannot run sandboxes, with the fix,
// and says when there is a newer rungv to have.
func engineAlert(m *Model) ggui.Widget {
	type state struct {
		e Engine
		u Update
	}
	both := ggui.Combine(m.Engine, m.Update, func(e Engine, u Update) state { return state{e, u} })
	return ggui.View(both, func(s state) ggui.Widget {
		e, u := s.e, s.u
		shown := func(a ggui.Widget) ggui.Widget { return widget.Padding(a, 16, 0, 0, 0) }
		switch {
		case !e.Checked:
			return widget.Box()
		case e.Path == "":
			return shown(noticeBanner(lucide.TriangleExclamationPoint, 0xef4444, true,
				tr("rungv was not found"),
				tr("GVX Desktop drives the rungv command line. Install the newest release into ~/.gvx/bin here, or put your own on your PATH or choose its location in Settings."),
				installButton(m, u, tr("Install rungv")),
				ui.Button(tr("Open Settings"), func() { m.Go(PageSettings) }).Outline(),
			))
		case e.Err != "":
			return shown(noticeBanner(lucide.TriangleExclamationPoint, 0xef4444, true,
				tr("The engine did not start"), e.Err,
				ui.Button(tr("Retry"), func() { m.DetectEngine(nil) }).Outline().Icon(glyph(lucide.RefreshCw, 15)),
			))
		case rungv.NeedsSigning(e.Info):
			return shown(noticeBanner(lucide.TriangleExclamationPoint, 0xef4444, true,
				tr("rungv needs the hypervisor entitlement"),
				tr("On macOS, rungv runs guests through Hypervisor.framework and must be signed with com.apple.security.hypervisor. Sign it here, or run: ")+rungv.SignCommand(e.Path),
				ui.Button(tr("Sign rungv"), m.SignRungv),
			))
		case len(e.Info.AvailableBackends) == 0:
			return shown(noticeBanner(lucide.TriangleExclamationPoint, 0xef4444, true,
				tr("No execution backend"), strings.Join(e.Info.BackendWarnings, " ")))
		case len(e.Info.BackendWarnings) > 0:
			return shown(noticeBanner(lucide.Info, 0xf59e0b, false,
				tr("Engine warnings"), strings.Join(e.Info.BackendWarnings, " ")))
		case u.Available(e):
			acts := []ggui.Widget{installButton(m, u, updateVerb(u, e))}
			if u.Page != "" {
				acts = append(acts, ui.Button(updateNotes(u), func() { m.Open(u.Page) }).Outline().Icon(glyph(lucide.ExternalLink, 15)))
			}
			return shown(noticeBanner(lucide.Download, 0x3b82f6, false,
				updateTitle(u, e),
				tr("This computer has %s. Sandboxes that are running keep the rungv they started with.", engineVersion(e)),
				acts...,
			))
		}
		return widget.Box()
	})
}

// noticeBanner draws an inline status or update banner with an icon badge,
// readable title/description, and full-sized action buttons vertically
// centered beside the text (or wrapped cleanly beneath on narrow windows).
func noticeBanner(icon *icons.Glyph, accent uint32, destructive bool, title, desc string, actions ...ggui.Widget) ggui.Widget {
	badge := widget.Box(glyph(icon, 18).Color(rgb(accent))).Pad(10).Radius(10).Fill(rgbA(accent, 0x22))
	heading := widget.Text(title).Size(14).Weight(ggui.WeightSemibold)
	if destructive {
		heading = heading.Color(colorRed)
	}
	textParts := []ggui.Widget{heading}
	if desc != "" {
		textParts = append(textParts, ui.Caption(desc).Size(13))
	}
	info := widget.Row(
		badge,
		widget.Expanded(widget.Column(textParts...).Gap(3)),
	).Gap(14).Align(widget.AlignCenter)

	var content ggui.Widget = info
	if len(actions) > 0 {
		wide := widget.Row(
			widget.Expanded(info),
			widget.Row(actions...).Gap(8).Align(widget.AlignCenter),
		).Gap(16).Align(widget.AlignCenter)
		narrow := widget.Column(
			info,
			widget.Wrap(actions...).Gap(8),
		).Gap(12).Stretch()
		content = widget.Responsive(760, wide, narrow)
	}
	return widget.Box(content).Pad(14, 16).Radius(10).Fill(rgbA(accent, 0x10)).Border(1, rgbA(accent, 0x38))
}

// installButton installs the newest rungv, and says how far the download
// is while it does.
func installButton(m *Model, u Update, label string) *ui.ButtonWidget {
	if u.Installing {
		doing := tr("Downloading… %d%%", u.Percent)
		if u.Source {
			doing = u.Phase
		}
		return ui.ButtonOf(widget.Row(ui.Spinner().Size(12), widget.Text(doing)).Gap(6).Align(widget.AlignCenter), func() {}).
			Name(label).Disabled(true)
	}
	return ui.Button(label, m.InstallRungv).Icon(glyph(lucide.Download, 14))
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
		if r.Type == kind && r.Size != "" && r.Size != "0B" && r.Size != "0 B" {
			return fmt.Sprintf("%d · %s", n, formatSize(r.Size))
		}
	}
	return itoa(n)
}

func recentSandboxes(m *Model) ggui.Widget {
	list := widget.EachKeyed(m.Rows, func(r Row) string { return r.Spec.ID }, func(row widget.EachItem[Row]) ggui.Widget {
		id := row.Item.Spec.ID
		return ggui.View(row.Value, func(r Row) ggui.Widget {
			// One line: the name leads with its image after it, and the
			// status stands by the button that changes it.
			return hoverRow(widget.Row(
				statusMark(r.State),
				widget.Text(r.Spec.Name).Size(14).Weight(ggui.WeightMedium).NoWrap(),
				widget.Expanded(ui.Caption(r.Spec.Image).Size(13).NoWrap().Ellipsis()),
				sandboxUsageBrief(m, id),
				ui.Caption(statusText(r.State)).Size(12).NoWrap(),
				sandboxQuickActions(m, r),
			).Gap(10).Align(widget.AlignCenter), func() { m.ShowSandbox(id, 0) })
		})
	}).Gap(2).Else(func() ggui.Widget {
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

// sandboxUsageBrief is what a running sandbox uses, CPU and memory, as
// short as the dashboard's rows have room for; nothing while it is not
// known.
func sandboxUsageBrief(m *Model, id string) ggui.Widget {
	text := ggui.Map(m.SandboxUsage, func(all map[string]Usage) string {
		u, ok := all[id]
		if !ok {
			return ""
		}
		v := usageOf(u, true)
		return v.CPU + " · " + v.Mem
	})
	return ggui.View(text, func(s string) ggui.Widget {
		if s == "" {
			return widget.Box()
		}
		return ui.Mono(s).Size(11).Color(colorGray).NoWrap()
	})
}

// sandboxQuickActions are the start, restart and stop buttons of a row.
func sandboxQuickActions(m *Model, r Row) ggui.Widget {
	id := r.Spec.ID
	if r.State.Status.Active() {
		return widget.Row(
			tip(iconButton(lucide.RotateCcw, tr("Restart %s", r.Spec.Name), func() { m.RestartSandbox(id) }).BindDisabled(ggui.Const(r.State.Status != sandbox.Running)), tr("Restart")),
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
		// The default backend first, then the others the host offers.
		backends := e.Info.DefaultBackend
		for _, b := range e.Info.AvailableBackends {
			if b != e.Info.DefaultBackend {
				backends += ", " + b
			}
		}
		if backends == "" {
			backends = tr("none")
		}
		return widget.Column(
			kvText(tr("Version"), engineVersion(e)),
			kvText(tr("Host"), e.Info.OS+"/"+e.Info.Architecture+fmt.Sprintf(tr(" · %d CPUs"), e.Info.NumCPU)),
			kvText(tr("Backend"), backends),
			kvText(tr("Host lockdown"), e.Info.HostLockdown),
			kv(tr("Binary"), tip(widget.Text(tailPath(e.Path, 34)).Size(13).NoWrap().Ellipsis(), tildePath(e.Path))),
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
		icon := recipeIcon(r.Key)
		open := func() { m.OpenRecipe(r.Key) }
		summary, highlights := recipeCopy(r)
		var points []ggui.Widget
		for _, h := range highlights {
			points = append(points, widget.Row(glyph(lucide.Check, 14).Color(colorGreen), ui.Caption(h)).Gap(8).Align(widget.AlignStart))
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
					ui.Button(tr("Open"), open).Outline().Size(ui.ButtonSmall)).Align(widget.AlignCenter)
			case s >= 0:
				return widget.Row(ui.Button(tr("Start"), func() { m.CreateFromRecipe(r.Key); open() }).Size(ui.ButtonSmall).Icon(glyph(lucide.Play, 14)),
					ui.Button(tr("Details"), open).Ghost().Size(ui.ButtonSmall)).Gap(8)
			}
			return ui.Button(tr("Set up %s", recipeTitle(r)), func() { m.CreateFromRecipe(r.Key); open() }).Size(ui.ButtonSmall).Icon(glyph(lucide.Play, 14))
		})
		cards = append(cards, ui.Card(widget.Column(
			widget.Row(
				widget.Box(glyph(icon, 20).Color(colorBlue)).Pad(8).Radius(10).Fill(rgbA(0x3b82f6, 0x22)),
				widget.Column(ui.Heading(recipeTitle(r)).Size(15), ui.Mono(r.Image).Size(11)).Gap(2),
			).Gap(12),
			ui.Caption(summary),
			widget.Column(points...).Gap(6),
			action,
		).Gap(12).Stretch()))
	}
	return widget.Responsive(1100, widget.Grid(3, cards...).Gap(12),
		widget.Responsive(760, widget.Grid(2, cards...).Gap(12), widget.Column(cards...).Gap(12).Stretch()))
}

// recipeIcon pictures a recipe.
func recipeIcon(key string) *icons.Glyph {
	switch key {
	case recipes.Kubernetes:
		return lucide.ShipWheel
	case recipes.Registry:
		return lucide.Warehouse
	}
	return lucide.Container
}

// recipeTitle is a recipe's name, in the app's words where it has some.
func recipeTitle(r recipes.Recipe) string {
	if r.Key == recipes.Registry {
		return tr("Image registry")
	}
	return r.Title
}

func recipeCopy(r recipes.Recipe) (summary string, highlights []string) {
	switch r.Key {
	case recipes.Docker:
		return tr("Run dockerd without a VM or root, and drive it with the docker CLI on this machine."), []string{
			tr("API on ~/.gvx/docker.sock (docker context \"gvx\")"),
			tr("Images and containers persist in the gvx-docker volume"),
			tr("Published container ports reach this computer under the same number"),
			tr("Your home folder is shared, so compose bind mounts work"),
		}
	case recipes.Kubernetes:
		return tr("A single-node k3s cluster with a kubeconfig written to ~/.gvx/k3s."), []string{
			tr("API server on 127.0.0.1:6443"),
			tr("NodePorts 30000–30019 reachable from the host"),
			tr("Optional load balancer on 127.0.0.1:8080 and :8443"),
			tr("Cluster state persists in the k3s volume"),
		}
	case recipes.Registry:
		return tr("A registry on this computer: push images from the Docker Engine, and run them in the cluster."), []string{
			tr("Push to host.docker.internal:5001 from the Docker Engine"),
			tr("Pods pull localhost:5001/NAME:TAG and host.docker.internal:5001/NAME:TAG"),
			tr("Images persist in the gvx-registry volume"),
		}
	}
	return r.Summary, r.Highlights
}

// updateTitle heads the notice that the rungv of e is not its channel's
// newest.
func updateTitle(u Update, e Engine) string {
	switch {
	case u.Source && u.Ahead > 0:
		if u.Subject != "" {
			return tr("%d new commits on %s: %s", u.Ahead, u.BranchName(), u.Subject)
		}
		return tr("%d new commits on %s", u.Ahead, u.BranchName())
	case u.Source:
		if u.Subject != "" {
			return tr("%s is at %s: %s", u.BranchName(), u.Name(), u.Subject)
		}
		return tr("%s is at %s", u.BranchName(), u.Name())
	case u.Newer(e):
		return tr("rungv %s is available", u.Name())
	}
	return tr("The newest release is rungv %s", u.Name())
}

// updateVerb is what the button that installs it says.
func updateVerb(u Update, e Engine) string {
	switch {
	case u.Source:
		return tr("Build and install")
	case u.Newer(e):
		return tr("Update")
	}
	return tr("Install the release")
}

// updateNotes is what the button that tells more of it says.
func updateNotes(u Update) string {
	if u.Source {
		return tr("Commits")
	}
	return tr("Release notes")
}
