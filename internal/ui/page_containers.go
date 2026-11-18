package ui

import (
	"fmt"
	"image/color"
	"strings"
	"time"

	"github.com/ironpark/ggui"
	"github.com/ironpark/ggui/icons/lucide"
	"github.com/ironpark/ggui/ui"
	"github.com/ironpark/ggui/ui/icons"
	"github.com/ironpark/ggui/widget"

	"github.com/ironpark/gvx-desktop/internal/dockerapi"
	"github.com/ironpark/gvx-desktop/internal/recipes"
	"github.com/ironpark/gvx-desktop/internal/sandbox"
)

// The states of the Containers page, past the engine's: dockerd did not
// answer, it has yet to, it has no containers, or it has some.
const (
	containersFailed = iota + 1
	containersLoading
	containersNone
	containersListed
)

// containersPage lists the Docker Engine's containers as the Sandboxes page
// lists sandboxes: a table to filter, and the details of the chosen one
// under it. It tells what to do while the engine is not running.
func containersPage(m *Model) ggui.Widget {
	listing := m.DockerContainers
	summary := ui.CaptionOf(ggui.Combine(m.ContainersOpen, listing, func(open bool, l Listing[dockerapi.Container]) string {
		if !open || !l.Loaded || l.Err != "" {
			return tr("Containers run inside the Docker Engine sandbox.")
		}
		running := 0
		for _, c := range l.Items {
			if c.State == "running" {
				running++
			}
		}
		return tr("%d running · %d total. Containers run inside the Docker Engine sandbox.", running, len(l.Items))
	}))

	state := ggui.Combine(m.ContainersAccess, listing, func(a ContainersAccess, l Listing[dockerapi.Container]) int {
		switch {
		case a != ContainersOpen:
			return 0
		case l.Err != "":
			return containersFailed
		case !l.Loaded:
			return containersLoading
		case len(l.Items) == 0:
			return containersNone
		}
		return containersListed
	})
	body := ggui.View(state, func(s int) ggui.Widget {
		empty := func(title, text string) *ui.EmptyWidget {
			return ui.Empty(title, text).Media(glyph(lucide.Boxes, 32).Color(colorGray)).Outline()
		}
		switch s {
		case 0:
			return ggui.View(ggui.Combine(m.ContainersAccess, recipeRow(m, recipes.Docker), func(a ContainersAccess, r Row) engineGate {
				return engineGate{a, r.State.Status == sandbox.Stopping}
			}), func(g engineGate) ggui.Widget { return engineNotRunning(m, g) })
		case containersFailed:
			return ui.Alert(tr("Could not reach dockerd"), ggui.Peek(listing).Err).Destructive().Icon(icons.Alert)
		case containersLoading:
			return widget.Row(ui.Spinner(), ui.Caption(tr("Asking dockerd…"))).Gap(8)
		case containersNone:
			return empty(tr("No containers"), tr("Run one with the docker CLI: docker run -d -p 9000:80 nginx:alpine"))
		}
		return widget.Column(
			tableCard(containersTable(m)),
			containerDetail(m),
		).Gap(16).Stretch()
	})

	return page(
		pageHeader(tr("Containers"), summary,
			tip(iconButton(lucide.RefreshCw, tr("Refresh containers"), func() {
				m.LoadDocker()
				m.LoadContainerLogs()
			}).BindDisabled(ggui.Not(m.ContainersOpen)), tr("Refresh")),
			ui.Button("Docker Engine", func() { m.Go(PageDocker) }).Outline().Icon(glyph(lucide.Container, 16)),
		),
		body,
	)
}

// engineGate is what the Containers page says of an engine that is not
// running: how far it is from running, and whether it is stopping.
type engineGate struct {
	access   ContainersAccess
	stopping bool
}

// engineNotRunning stands in for the containers while the engine does not
// run, with what would start it.
func engineNotRunning(m *Model, g engineGate) ggui.Widget {
	media := glyph(lucide.Boxes, 32).Color(colorGray)
	start := func(label string) *ui.ButtonWidget {
		return ui.Button(label, func() { m.CreateFromRecipe(recipes.Docker) }).Icon(glyph(lucide.Play, 16))
	}
	engine := ui.Button(tr("Open Docker page"), func() { m.Go(PageDocker) }).Outline()
	switch g.access {
	case ContainersNoEngine:
		return ui.Empty(tr("The Docker Engine is not set up"),
			tr("Set it up to run containers with the docker CLI on this machine. They show here while it runs.")).
			Media(media).Action(widget.Row(start(tr("Set up and start")), engine).Gap(8)).Outline()
	case ContainersStarting:
		return ui.Empty(tr("The Docker Engine is starting"), tr("Its containers show here once dockerd is ready.")).
			Media(ui.Spinner().Size(28).Color(colorAmber)).Action(engine).Outline()
	}
	return ui.Empty(tr("The Docker Engine is not running"), tr("Start it to see and manage its containers here.")).
		Media(media).Action(widget.Row(start(tr("Start Docker Engine")).Disabled(g.stopping), engine).Gap(8)).Outline()
}

// containersTable is the engine's containers, filtered by the query.
func containersTable(m *Model) ggui.Widget {
	all := ggui.Map(m.DockerContainers, func(l Listing[dockerapi.Container]) []dockerapi.Container { return l.Items }).WithEqual(nil)
	visible := ggui.Combine(all, m.ContainerQuery, filterContainers).WithEqual(nil)
	count := ggui.Combine(visible, all, func(v, a []dockerapi.Container) string { return showing(len(v), len(a)) })
	selected := ggui.Bind(ggui.Map(m.Container, func(c dockerapi.Container) string { return c.ID }).Get, m.SelectContainer)
	// As tall as its rows up to six, as the sandboxes' table is.
	return ui.Table(visible, func(c dockerapi.Container) string { return c.ID },
		ui.Col(tr("Status"), func(r ggui.Readable[dockerapi.Container]) ggui.Widget {
			return ggui.View(r, containerStatusLabel)
		}).W(150),
		ui.Col(tr("Name"), func(r ggui.Readable[dockerapi.Container]) ggui.Widget {
			return widget.Column(
				widget.TextOf(ggui.Map(r, dockerapi.Container.Name)).Size(13).Weight(ggui.WeightMedium).NoWrap().Ellipsis(),
				ui.CaptionOf(ggui.Map(r, dockerapi.Container.ShortID)).Size(11).NoWrap(),
			).Gap(1)
		}).Grow(2),
		ui.Col(tr("Image"), func(r ggui.Readable[dockerapi.Container]) ggui.Widget {
			return ui.CaptionOf(ggui.Map(r, func(c dockerapi.Container) string { return c.Image })).Size(12).NoWrap().Ellipsis()
		}).Grow(2),
		ui.Col(tr("Ports"), func(r ggui.Readable[dockerapi.Container]) ggui.Widget {
			return ui.CaptionOf(ggui.Map(r, containerPortsSummary)).Size(12).NoWrap().Ellipsis()
		}).Grow(2),
		ui.Col(tr("Created"), func(r ggui.Readable[dockerapi.Container]) ggui.Widget {
			return ui.CaptionOf(ggui.Combine(r, m.Now, func(c dockerapi.Container, now time.Time) string {
				if c.Created == 0 {
					return "—"
				}
				return since(time.Unix(c.Created, 0), now)
			})).Size(12).NoWrap()
		}).W(130),
		ui.Col("", func(r ggui.Readable[dockerapi.Container]) ggui.Widget {
			return ggui.View(r, func(c dockerapi.Container) ggui.Widget {
				return widget.Row(containerQuickAction(m, c), containerMenu(m, c)).Gap(2).Justify(widget.JustifyEnd)
			})
		}).W(84).Identified("actions"),
	).BindSelected(selected).RowName(dockerapi.Container.Name).RowHeight(48).MaxRows(6).
		Toolbar(filterBar(m.ContainerQuery, tr("Filter by name or image"), count))
}

// containerStatus is a container's state as a color, words, and whether it
// is on its way to another.
func containerStatus(c dockerapi.Container) (col color.Color, text string, changing bool) {
	switch c.State {
	case "running":
		return colorGreen, tr("Running"), false
	case "restarting":
		return colorAmber, tr("Restarting"), true
	case "removing":
		return colorAmber, tr("Removing"), true
	case "paused":
		return colorAmber, tr("Paused"), false
	case "created":
		return colorGray, tr("Not started"), false
	case "dead":
		return colorRed, tr("Dead"), false
	case "exited":
		if code, ok := c.ExitCode(); ok && code != 0 {
			return colorRed, tr("Failed · exit %d", code), false
		}
		return colorBlue, tr("Exited"), false
	}
	return colorGray, c.State, false
}

// containerStatusLabel is a container's state as the sandboxes' is shown,
// with the engine's own words for it in a tip.
func containerStatusLabel(c dockerapi.Container) ggui.Widget {
	col, text, changing := containerStatus(c)
	var mark ggui.Widget = widget.Box(dot(col, 8)).Pad(2)
	if changing {
		mark = ui.Spinner().Size(12).Color(col)
	}
	label := widget.Row(mark, widget.Text(text).Size(13).NoWrap()).Gap(8).Align(widget.AlignCenter)
	if c.Status == "" {
		return label
	}
	return tip(label, c.Status)
}

// containerPorts are a container's ports, each once: docker lists a port
// published on IPv4 and IPv6 twice.
func containerPorts(c dockerapi.Container) []dockerapi.Port {
	var out []dockerapi.Port
	seen := map[dockerapi.Port]bool{}
	for _, p := range c.Ports {
		p.IP = ""
		if !seen[p] {
			seen[p] = true
			out = append(out, p)
		}
	}
	return out
}

// containerPortsSummary is the published ports, host first, as the
// sandboxes' table writes them.
func containerPortsSummary(c dockerapi.Container) string {
	var parts []string
	ports := containerPorts(c)
	for i, p := range ports {
		if i == 3 {
			parts = append(parts, fmt.Sprintf("+%d", len(ports)-3))
			break
		}
		if p.PublicPort != 0 {
			parts = append(parts, fmt.Sprintf("%d→%d", p.PublicPort, p.PrivatePort))
		} else {
			parts = append(parts, fmt.Sprintf("%d/%s", p.PrivatePort, p.Type))
		}
	}
	if len(parts) == 0 {
		return "—"
	}
	return strings.Join(parts, ", ")
}

// containerQuickAction starts a stopped container and stops a running one.
func containerQuickAction(m *Model, c dockerapi.Container) ggui.Widget {
	if c.State == "running" || c.State == "restarting" {
		return tip(iconButton(lucide.Square, tr("Stop %s", c.Name()), func() { m.DockerAction("stop", c) }), tr("Stop"))
	}
	return tip(iconButton(lucide.Play, tr("Start %s", c.Name()), func() { m.DockerAction("start", c) }), tr("Start"))
}

// containerMenu is the rest of what can be done to a container.
func containerMenu(m *Model, c dockerapi.Container) ggui.Widget {
	name := c.Name()
	entries := []ggui.Widget{
		ui.MenuItem(tr("Logs"), func() { m.ShowContainerLogs(c.ID) }),
	}
	if c.State == "running" {
		entries = append(entries, ui.MenuItem(tr("Restart"), func() { m.DockerAction("restart", c) }))
	}
	entries = append(entries,
		ui.MenuItem(tr("Copy ID"), func() { m.Copy(c.ID, c.ShortID()) }),
		ui.MenuItem(tr("Copy docker logs command"), func() { m.Copy("docker logs -f "+name, "docker logs") }),
		ui.MenuItem(tr("Copy docker exec command"), func() { m.Copy("docker exec -it "+name+" sh", "docker exec") }),
		ui.MenuDivider(),
		ui.MenuItem(tr("Remove"), func() { m.AskRemoveContainer(c) }).Destructive(),
	)
	return ui.MenuOf(glyph(lucide.Ellipsis, 16), entries...).Name(tr("More actions for %s", name))
}

// ---- Detail -------------------------------------------------------------

// containerDetail is the chosen container: its state and actions, and tabs
// with its logs and how to reach it.
func containerDetail(m *Model) ggui.Widget {
	id := ggui.Map(m.Container, func(c dockerapi.Container) string { return c.ID })
	return entering(id, func(string) ggui.Widget {
		header := ggui.View(m.Container, func(c dockerapi.Container) ggui.Widget { return containerHeader(m, c) })
		tabs := ui.Tabs(m.ContainerDetail,
			ui.Tab(tr("Logs"), containerLogsView(m)),
			ui.Tab(tr("Connect"), ggui.View(m.Container, func(c dockerapi.Container) ggui.Widget { return containerConnect(m, c) })),
		).Line().Name(tr("Container details"))
		return ui.Card(widget.Column(header, tabs).Gap(12).Stretch())
	})
}

func containerHeader(m *Model, c dockerapi.Container) ggui.Widget {
	var actions []ggui.Widget
	if c.State == "running" || c.State == "restarting" {
		actions = append(actions,
			ui.Button(tr("Stop"), func() { m.DockerAction("stop", c) }).Outline().Icon(glyph(lucide.Square, 14)),
			ui.Button(tr("Restart"), func() { m.DockerAction("restart", c) }).Outline().Icon(glyph(lucide.RotateCcw, 14)),
		)
	} else {
		actions = append(actions, ui.Button(tr("Start"), func() { m.DockerAction("start", c) }).Icon(glyph(lucide.Play, 14)))
	}
	actions = append(actions, containerMenu(m, c))
	meta := ui.CaptionOf(ggui.Map(m.Now, func(now time.Time) string {
		parts := []string{c.Image, c.ShortID()}
		if c.Created != 0 {
			parts = append(parts, tr("created %s", since(time.Unix(c.Created, 0), now)))
		}
		if p := c.Project(); p != "" {
			parts = append(parts, "compose: "+p)
		}
		return strings.Join(parts, " · ")
	})).Size(12)
	head := widget.Column(
		widget.Row(ui.Title(c.Name()).Size(20), containerStatusLabel(c)).Gap(12).Align(widget.AlignCenter),
		meta,
	).Gap(4)
	return widget.Responsive(680,
		widget.Row(widget.Expanded(head), widget.Row(actions...).Gap(8)).Gap(16).Align(widget.AlignCenter),
		widget.Column(head, widget.Wrap(actions...).Gap(8)).Gap(12).Stretch(),
	)
}

// containerLogsView shows the last lines the chosen container wrote, read
// again as often as the containers are.
func containerLogsView(m *Model) ggui.Widget {
	lines := ggui.Map(m.ContainerLogs, func(l ContainerLogs) []sandbox.LogLine { return l.Lines }).WithEqual(nil)
	status := ggui.View(ggui.Map(m.ContainerLogs, func(l ContainerLogs) string {
		switch {
		case l.Err != "":
			return l.Err
		case !l.Loaded:
			return "…"
		}
		return ""
	}), func(s string) ggui.Widget {
		switch s {
		case "":
			return ui.Caption(tr("The last %d lines, as docker logs shows them.", containerLogTail)).Size(12)
		case "…":
			return widget.Row(ui.Spinner().Size(12), ui.Caption(tr("Reading logs…")).Size(12)).Gap(8).Align(widget.AlignCenter)
		}
		return ui.Alert(tr("Could not read the logs"), s).Destructive().Icon(icons.Alert)
	})
	viewer := logViewer(m, lines, tr("The container has written nothing yet."),
		tip(iconButton(lucide.RefreshCw, tr("Refresh logs"), m.LoadContainerLogs), tr("Refresh")))
	return widget.Column(viewer, status).Gap(8).Stretch()
}

// containerConnect tells how to reach what the container serves, and the
// commands that reach the container itself.
func containerConnect(m *Model, c dockerapi.Container) ggui.Widget {
	parts := []ggui.Widget{sectionTitle(tr("Published ports"), tr("Published container ports must be inside 9000–9019, the range the sandbox opened at start."))}
	ports := containerPorts(c)
	inside := false
	for _, p := range ports {
		if p.PublicPort == 0 {
			inside = true
			continue
		}
		host := fmt.Sprintf("127.0.0.1:%d", p.PublicPort)
		label := fmt.Sprintf("%s → %d/%s", host, p.PrivatePort, p.Type)
		mark := glyph(lucide.Plug, 14).Color(colorGray)
		var act ggui.Widget = iconButton(lucide.Copy, tr("Copy address"), func() { m.Copy(host, host) })
		if p.Type == "tcp" {
			url := "http://" + host
			if c.State == "running" {
				mark = glyph(lucide.Plug, 14).Color(colorGreen)
			}
			act = widget.Row(
				ui.Button(tr("Open"), func() { m.Open(url) }).Outline().Size(ui.ButtonExtraSmall).Icon(glyph(lucide.ExternalLink, 12)).Disabled(c.State != "running"),
				iconButton(lucide.Copy, tr("Copy address"), func() { m.Copy(url, url) }),
			).Gap(4)
		}
		parts = append(parts, widget.Row(mark, widget.Expanded(ui.Mono(label).Size(12)), act).Gap(10).Align(widget.AlignCenter))
	}
	if inside {
		for _, p := range ports {
			if p.PublicPort == 0 {
				parts = append(parts, widget.Row(glyph(lucide.Plug, 14).Color(colorGray), ui.Mono(fmt.Sprintf("%d/%s", p.PrivatePort, p.Type)).Size(12)).Gap(10))
			}
		}
		parts = append(parts, ui.Caption(tr("A port without a host port is reachable only from other containers.")))
	}
	if len(ports) == 0 {
		parts = append(parts, ui.Caption(tr("No ports are published. Publish one with docker run -p 9000:80.")))
	}
	name := c.Name()
	parts = append(parts,
		sectionTitle(tr("Commands"), tr("For a terminal where docker uses the gvx context.")),
		copyable(m, "docker logs -f "+name, "docker logs"),
		copyable(m, "docker exec -it "+name+" sh", "docker exec"),
		copyable(m, "docker inspect "+name, "docker inspect"),
	)
	return widget.Padding(widget.Column(parts...).Gap(10).Stretch(), 8, 0)
}
