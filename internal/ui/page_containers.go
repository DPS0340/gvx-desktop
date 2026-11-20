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
			stage := ggui.Map(m.ContainersAccess, func(a ContainersAccess) int { return int(a) })
			return ggui.View(gateOf(m, recipes.Docker, stage), func(g engineGate) ggui.Widget { return engineNotRunning(m, g, dockerGate()) })
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

// dockerGate words the Containers page while the engine does not run.
func dockerGate() gateWords {
	return gateWords{
		recipe: recipes.Docker, page: PageDocker, icon: lucide.Boxes,
		openPage: tr("Open Docker page"),
		notSetUp: [2]string{tr("The Docker Engine is not set up"),
			tr("Set it up to run containers with the docker CLI on this machine. They show here while it runs.")},
		starting: [2]string{tr("The Docker Engine is starting"), tr("Its containers show here once dockerd is ready.")},
		stopped:  [2]string{tr("The Docker Engine is not running"), tr("Start it to see and manage its containers here.")},
		start:    tr("Start Docker Engine"),
	}
}

// containersTable is the engine's containers, filtered by the query, each
// Compose project's under a head that folds them away.
func containersTable(m *Model) ggui.Widget {
	all := ggui.Map(m.DockerContainers, func(l Listing[dockerapi.Container]) []dockerapi.Container { return l.Items }).WithEqual(nil)
	rows := ggui.Derived(func() []containerRow {
		return containerRows(all.Get(), m.ContainerQuery.Get(), m.ComposeFolded.Get())
	}).WithEqual(nil)
	count := ggui.Combine(all, m.ContainerQuery, func(a []dockerapi.Container, q string) string {
		return showing(len(filterContainers(a, q)), len(a))
	})
	// A head is no container to choose: choosing one leaves the choice.
	selected := ggui.Bind(ggui.Map(m.Container, func(c dockerapi.Container) string { return c.ID }).Get, func(id string) {
		if !strings.HasPrefix(id, composeKey("")) {
			m.SelectContainer(id)
		}
	})
	// cell builds a column's cell for a container's row, or with head for a
	// project's head, which has nothing in the column without one. A row
	// keeps its key, and so whether it is a head.
	cell := func(ctr func(ggui.Readable[dockerapi.Container]) ggui.Widget, head func(ggui.Readable[containerRow]) ggui.Widget) func(ggui.Readable[containerRow]) ggui.Widget {
		return func(r ggui.Readable[containerRow]) ggui.Widget {
			if ggui.Peek(r).Head {
				if head == nil {
					return widget.Box()
				}
				return head(r)
			}
			return ctr(ggui.Map(r, func(row containerRow) dockerapi.Container { return row.C }).WithEqual(dockerapi.Container.Equal))
		}
	}
	// As tall as its rows up to six, as the sandboxes' table is.
	return ui.Table(rows, containerRow.key,
		ui.Col(tr("Status"), cell(func(r ggui.Readable[dockerapi.Container]) ggui.Widget {
			return ggui.View(r, containerStatusLabel)
		}, func(r ggui.Readable[containerRow]) ggui.Widget {
			return ggui.View(ggui.Map(r, func(row containerRow) []dockerapi.Container { return row.Members }).WithEqual(nil), projectStatusLabel)
		})).W(150),
		ui.Col(tr("Name"), cell(func(r ggui.Readable[dockerapi.Container]) ggui.Widget {
			name := widget.Column(
				widget.TextOf(ggui.Map(r, dockerapi.Container.Name)).Size(13).Weight(ggui.WeightMedium).NoWrap().Ellipsis(),
				ui.CaptionOf(ggui.Map(r, dockerapi.Container.ShortID)).Size(11).NoWrap(),
			).Gap(1)
			if ggui.Peek(r).Project() == "" {
				return name
			}
			// A project's container stands in from its head's name.
			return widget.Padding(name, 0, 0, 0, projectIndent)
		}, func(r ggui.Readable[containerRow]) ggui.Widget { return projectName(m, r) })).Grow(2),
		ui.Col(tr("Image"), cell(func(r ggui.Readable[dockerapi.Container]) ggui.Widget {
			return ui.CaptionOf(ggui.Map(r, func(c dockerapi.Container) string { return c.Image })).Size(12).NoWrap().Ellipsis()
		}, nil)).Grow(2),
		ui.Col(tr("Ports"), cell(func(r ggui.Readable[dockerapi.Container]) ggui.Widget {
			return ui.CaptionOf(ggui.Map(r, containerPortsSummary)).Size(12).NoWrap().Ellipsis()
		}, nil)).Grow(2),
		ui.Col(tr("CPU / Memory"), cell(func(r ggui.Readable[dockerapi.Container]) ggui.Widget {
			return usageCell(ggui.Combine(r, m.ContainerUsage, func(c dockerapi.Container, all map[string]Usage) usageView {
				u, ok := all[c.ID]
				return usageOf(u, ok)
			}))
		}, func(r ggui.Readable[containerRow]) ggui.Widget {
			return usageCell(ggui.Combine(r, m.ContainerUsage, func(row containerRow, all map[string]Usage) usageView {
				u, ok := projectUsage(row.Members, all)
				return usageOf(u, ok)
			}))
		})).W(usageWidth),
		ui.Col(tr("Created"), cell(func(r ggui.Readable[dockerapi.Container]) ggui.Widget {
			return ui.CaptionOf(ggui.Combine(r, m.Now, func(c dockerapi.Container, now time.Time) string {
				if c.Created == 0 {
					return "—"
				}
				return since(time.Unix(c.Created, 0), now)
			})).Size(12).NoWrap()
		}, nil)).W(130),
		ui.Col("", cell(func(r ggui.Readable[dockerapi.Container]) ggui.Widget {
			return ggui.View(r, func(c dockerapi.Container) ggui.Widget {
				return widget.Row(containerQuickAction(m, c), containerMenu(m, c)).Gap(2).Justify(widget.JustifyEnd)
			})
		}, func(r ggui.Readable[containerRow]) ggui.Widget {
			return ggui.View(r, func(row containerRow) ggui.Widget {
				return widget.Row(projectQuickAction(m, row), projectMenu(m, row)).Gap(2).Justify(widget.JustifyEnd)
			})
		})).W(84).Identified("actions"),
	).BindSelected(selected).RowName(containerRow.name).RowHeight(48).MaxRows(6).
		Toolbar(filterBar(m.ContainerQuery, tr("Filter by name, image or project"), count))
}

// projectIndent is how far a project's containers stand in from its name.
const projectIndent = 26

// projectName is a Compose project's name in its head, after the button
// that folds its containers away or brings them back.
func projectName(m *Model, r ggui.Readable[containerRow]) ggui.Widget {
	project := ggui.Peek(r).Project
	fold := ggui.View(ggui.Map(r, func(row containerRow) bool { return row.Folded }), func(folded bool) ggui.Widget {
		icon, name := lucide.ChevronDown, tr("Fold %s", project)
		if folded {
			icon, name = lucide.ChevronRight, tr("Unfold %s", project)
		}
		return ui.ButtonOf(glyph(icon, 14), func() { m.ToggleProject(project) }).Name(name).Size(ui.ButtonIconExtraSmall).Ghost()
	})
	services := ui.CaptionOf(ggui.Map(r, func(row containerRow) string {
		return trn(len(row.Members), "Compose · %d container", "Compose · %d containers")
	})).Size(11).NoWrap().Ellipsis()
	return widget.Row(
		fold,
		widget.Flexible(widget.Column(
			widget.Text(project).Size(13).Weight(ggui.WeightSemibold).NoWrap().Ellipsis(),
			services,
		).Gap(1)),
	).Gap(4).Align(widget.AlignCenter)
}

// projectStatusLabel is a project's state: green with all its containers
// running, amber with some, gray with none, and red while one has failed.
func projectStatusLabel(members []dockerapi.Container) ggui.Widget {
	running, total := projectCounts(members)
	col := colorGray
	switch {
	case running == total && total > 0:
		col = colorGreen
	case running > 0:
		col = colorAmber
	}
	for _, c := range members {
		if c2, _, _ := containerStatus(c); c2 == colorRed {
			col = colorRed
		}
	}
	return stateLabel(col, tr("%d of %d running", running, total), false, "")
}

// projectUsage sums what a project's running containers use; ok is false
// while none of theirs is known. Its CPU is known when every one's is.
func projectUsage(members []dockerapi.Container, all map[string]Usage) (u Usage, ok bool) {
	u.HasCPU = true
	for _, c := range members {
		cu, found := all[c.ID]
		if !found {
			continue
		}
		ok = true
		u.CPU += cu.CPU
		u.HasCPU = u.HasCPU && cu.HasCPU
		u.Memory += cu.Memory
	}
	return u, ok
}

// projectQuickAction stops a project with containers running, and starts
// one with none.
func projectQuickAction(m *Model, row containerRow) ggui.Widget {
	if running, _ := projectCounts(row.Members); running > 0 {
		return tip(iconButton(lucide.Square, tr("Stop %s", row.Project), func() { m.ProjectAction("stop", row.Project, row.Members) }), tr("Stop all"))
	}
	return tip(iconButton(lucide.Play, tr("Start %s", row.Project), func() { m.ProjectAction("start", row.Project, row.Members) }), tr("Start all"))
}

// projectMenu is what can be done to a project's containers together.
func projectMenu(m *Model, row containerRow) ggui.Widget {
	p, members := row.Project, row.Members
	running, total := projectCounts(members)
	return moreMenu(tr("More actions for %s", p),
		ui.MenuItem(tr("Start all"), func() { m.ProjectAction("start", p, members) }).Disabled(running == total),
		ui.MenuItem(tr("Stop all"), func() { m.ProjectAction("stop", p, members) }).Disabled(running == 0),
		ui.MenuItem(tr("Restart all"), func() { m.ProjectAction("restart", p, members) }),
		ui.MenuItem(tr("Copy project name"), func() { m.Copy(p, p) }),
		ui.MenuDivider(),
		ui.MenuItem(tr("Remove all…"), func() { m.AskRemoveProject(p, members) }).Destructive(),
	)
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
	return stateLabel(col, text, changing, c.Status)
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
	return moreMenu(tr("More actions for %s", name), entries...)
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
			ui.Tab(tr("Terminal"), containerTerminal(m)),
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
	usage := usageLine(ggui.Map(m.ContainerUsage, func(all map[string]Usage) usageView {
		u, ok := all[c.ID]
		return usageOf(u, ok)
	}))
	state := widget.Row(containerStatusLabel(c), usage).Gap(16).Align(widget.AlignCenter)
	return detailHead(c.Name(), state, meta, actions...)
}

// containerLogsView shows the last lines the chosen container wrote, read
// again as often as the containers are.
func containerLogsView(m *Model) ggui.Widget {
	return engineLogsView(m, m.ContainerLogs,
		tr("The last %d lines, as docker logs shows them.", containerLogTail), tr("The container has written nothing yet."),
		tip(iconButton(lucide.RefreshCw, tr("Refresh logs"), m.LoadContainerLogs), tr("Refresh")))
}

// containerTerminal is a shell in the chosen container, as docker exec -it
// runs one.
func containerTerminal(m *Model) ggui.Widget {
	name := ggui.Peek(m.Container).Name()
	return terminalTab(m, m.containerShell(), tr("A shell in the container, as docker exec -it runs one."),
		tip(iconButton(lucide.Copy, tr("Copy docker exec command"), func() { m.Copy("docker exec -it "+name+" sh", "docker exec") }), tr("Copy")))
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
