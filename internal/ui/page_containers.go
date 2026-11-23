package ui

import (
	"fmt"
	"image/color"
	"slices"
	"strings"
	"time"

	"github.com/ironpark/ggui"
	"github.com/ironpark/ggui/icons/lucide"
	"github.com/ironpark/ggui/ui"
	"github.com/ironpark/ggui/ui/icons"
	"github.com/ironpark/ggui/widget"

	"github.com/ironpark/gvx-desktop/internal/dockerapi"
	"github.com/ironpark/gvx-desktop/internal/recipes"
	"github.com/ironpark/gvx-desktop/internal/rungv"
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
	summary := ui.CaptionOf(ggui.Derived(func() string {
		open, l, se := m.ContainersOpen.Get(), listing.Get(), m.Settings.Get().SandboxEngine
		if !open || !l.Loaded || l.Err != "" {
			if se {
				return tr("Containers run directly as gvx sandboxes.")
			}
			return tr("Containers run inside the Docker Engine sandbox.")
		}
		running := 0
		for _, c := range l.Items {
			if c.State == "running" {
				running++
			}
		}
		if se {
			return tr("%d running · %d total. Containers run directly as gvx sandboxes.", running, len(l.Items))
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
			return ui.Empty(title, text).Media(glyph(lucide.Container, 32).Color(colorGray)).Outline()
		}
		se := ggui.Peek(m.Settings).SandboxEngine
		switch s {
		case 0:
			stage := ggui.Map(m.ContainersAccess, func(a ContainersAccess) int { return int(a) })
			return ggui.View(gateOf(m, recipes.Docker, stage), func(g engineGate) ggui.Widget { return engineNotRunning(m, g, dockerGate()) })
		case containersFailed:
			title := tr("Could not reach dockerd")
			if se {
				title = tr("Could not reach the sandbox engine")
			}
			return ui.Alert(title, ggui.Peek(listing).Err).Destructive().Icon(icons.Alert)
		case containersLoading:
			msg := tr("Asking dockerd…")
			if se {
				msg = tr("Loading containers…")
			}
			return widget.Row(ui.Spinner(), ui.Caption(msg)).Gap(8)
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
			ui.Button("Docker Engine", func() { m.Go(PageDocker) }).Outline().Icon(glyph(dockerIcon, 16)),
		),
		body,
	)
}

// dockerGate words the Containers page while the engine does not run.
func dockerGate() gateWords {
	return gateWords{
		recipe: recipes.Docker, page: PageDocker, icon: lucide.Container,
		openPage: tr("Open Docker page"),
		notSetUp: [2]string{tr("The Docker Engine is not set up"),
			tr("Set it up to run containers with the docker CLI on this machine. They show here while it runs.")},
		starting: [2]string{tr("The Docker Engine is starting"), tr("Its containers show here once dockerd is ready.")},
		stopped:  [2]string{tr("The Docker Engine is not running"), tr("Start it to see and manage its containers here.")},
		paused:   [2]string{tr("The Docker Engine is paused"), tr("Resume it to see and manage its containers here.")},
		start:    tr("Start Docker Engine"),
		resume:   tr("Resume Docker Engine"),
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
	engine := engineSpecOf(m)
	liveMounts := dockerLiveMountsOf(m)
	// As tall as its rows up to six, as the sandboxes' table is.
	return ui.Table(rows, containerRow.key,
		ui.Col(tr("Status"), cell(func(r ggui.Readable[dockerapi.Container]) ggui.Widget {
			return ggui.View(r, containerStatusLabel)
		}, func(r ggui.Readable[containerRow]) ggui.Widget {
			return ggui.View(ggui.Map(r, func(row containerRow) []dockerapi.Container { return row.Members }).WithEqual(nil), projectStatusLabel)
		})).W(150),
		ui.Col(tr("Name"), cell(func(r ggui.Readable[dockerapi.Container]) ggui.Widget {
			unshared := ggui.Derived(func() unsharedView {
				if m.Settings.Get().SandboxEngine {
					return unsharedView{}
				}
				return unsharedView{
					binds: unsharedBinds(r.Get(), engine.Get()),
					live:  liveMounts.Get(),
				}
			}).WithEqual(unsharedView.equal)
			name := widget.Row(
				widget.Flexible(widget.Column(
					widget.TextOf(ggui.Map(r, dockerapi.Container.Name)).Size(13).Weight(ggui.WeightMedium).NoWrap().Ellipsis(),
					ui.CaptionOf(ggui.Map(r, dockerapi.Container.ShortID)).Size(11).NoWrap(),
				).Gap(1)),
				ggui.View(unshared, unsharedMark),
			).Gap(6).Align(widget.AlignCenter)
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
	).BindSelected(selected).RowName(containerRow.name).RowHeight(48).MaxRows(5).
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
	return moreMenu(tr("More actions for %s", p),
		ui.MenuItem(tr("Start all"), func() { m.ProjectAction("start", p, members) }).Disabled(len(projectTargets("start", members)) == 0),
		ui.MenuItem(tr("Stop all"), func() { m.ProjectAction("stop", p, members) }).Disabled(len(projectTargets("stop", members)) == 0),
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

// containerPauseAction returns "pause", "unpause", or "" for container c.
func containerPauseAction(c dockerapi.Container, sandboxEngine, canControl bool) string {
	if sandboxEngine && !canControl {
		return ""
	}
	switch c.State {
	case "running":
		return "pause"
	case "paused":
		return "unpause"
	}
	return ""
}

// containerQuickAction starts a stopped container, resumes a paused one, and
// stops a running one.
func containerQuickAction(m *Model, c dockerapi.Container) ggui.Widget {
	if c.State == "paused" {
		if !ggui.Peek(m.Settings).SandboxEngine || m.canControlContainer(c.ID) {
			return tip(iconButton(lucide.Play, tr("Resume"), func() { m.DockerAction("unpause", c) }), tr("Resume"))
		}
		return tip(iconButton(lucide.Square, tr("Stop %s", c.Name()), func() { m.DockerAction("stop", c) }), tr("Stop"))
	}
	if c.State == "running" || c.State == "restarting" {
		return tip(iconButton(lucide.Square, tr("Stop %s", c.Name()), func() { m.DockerAction("stop", c) }), tr("Stop"))
	}
	return tip(iconButton(lucide.Play, tr("Start %s", c.Name()), func() { m.DockerAction("start", c) }), tr("Start"))
}

// containerMenu is the rest of what can be done to a container.
func containerMenu(m *Model, c dockerapi.Container) ggui.Widget {
	name := c.Name()
	se := ggui.Peek(m.Settings).SandboxEngine
	entries := []ggui.Widget{
		ui.MenuItem(tr("Logs"), func() { m.ShowContainerLogs(c.ID) }),
	}
	switch containerPauseAction(c, se, m.canControlContainer(c.ID)) {
	case "pause":
		entries = append(entries, ui.MenuItem(tr("Pause"), func() { m.DockerAction("pause", c) }))
	case "unpause":
		entries = append(entries, ui.MenuItem(tr("Resume"), func() { m.DockerAction("unpause", c) }))
	}
	if c.State == "running" {
		entries = append(entries, ui.MenuItem(tr("Restart"), func() { m.DockerAction("restart", c) }))
	}
	entries = append(entries,
		ui.MenuItem(tr("Copy ID"), func() { m.Copy(c.ID, c.ShortID()) }),
		ui.MenuItem(tr("Copy docker logs command"), func() { m.Copy("docker logs -f "+name, "docker logs") }),
	)
	if !se || m.peekEngineFeatures().Control {
		entries = append(entries, ui.MenuItem(tr("Copy docker exec command"), func() { m.Copy("docker exec -it "+name+" sh", "docker exec") }))
	}
	entries = append(entries,
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
	se := ggui.Peek(m.Settings).SandboxEngine
	var actions []ggui.Widget
	switch {
	case c.State == "paused":
		if !se || m.canControlContainer(c.ID) {
			actions = append(actions, ui.Button(tr("Resume"), func() { m.DockerAction("unpause", c) }).Icon(glyph(lucide.Play, 14)))
		}
		actions = append(actions, ui.Button(tr("Stop"), func() { m.DockerAction("stop", c) }).Outline().Icon(glyph(lucide.Square, 14)))
	case c.State == "running" || c.State == "restarting":
		actions = append(actions,
			ui.Button(tr("Stop"), func() { m.DockerAction("stop", c) }).Outline().Icon(glyph(lucide.Square, 14)),
			ui.Button(tr("Restart"), func() { m.DockerAction("restart", c) }).Outline().Icon(glyph(lucide.RotateCcw, 14)),
		)
	default:
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
	return terminalTab(m, m.containerShell(), tr("A shell in the container, as docker exec -it runs one."),
		tip(iconButton(lucide.Copy, tr("Copy docker exec command"), func() {
			if name := ggui.Peek(m.Container).Name(); name != "" {
				m.Copy("docker exec -it "+name+" sh", "docker exec")
			}
		}), tr("Copy")))
}

// engineSpecOf follows the spec of the Docker Engine's sandbox.
func engineSpecOf(m *Model) ggui.Readable[sandbox.Spec] {
	return ggui.Map(m.Rows, func(rows []Row) sandbox.Spec {
		r, _ := m.RecipeSandbox(rows, recipes.Docker)
		return r.Spec
	}).WithEqual(sandbox.Spec.Equal)
}

// dockerLiveMountsOf reports whether shared-folder changes apply live to the
// Docker Engine without restarting (v0.21+ bind attach or Windows mount serve).
func dockerLiveMountsOf(m *Model) ggui.Readable[bool] {
	row := recipeRow(m, recipes.Docker)
	return ggui.Combine(row, m.RungvFeatures, func(r Row, f rungv.Features) bool {
		return r.State.BindAttach || r.State.LiveMounts || f.BindAttach || f.KeepBlobs
	})
}

type unsharedView struct {
	binds []dockerapi.Mount
	live  bool
}

func (u unsharedView) equal(o unsharedView) bool {
	return u.live == o.live && slices.Equal(u.binds, o.binds)
}

// unsharedBinds are a container's bind mounts that look like folders of
// this computer the engine does not share, which dockerd made empty
// folders in the sandbox.
func unsharedBinds(c dockerapi.Container, engine sandbox.Spec) []dockerapi.Mount {
	var out []dockerapi.Mount
	for _, b := range c.Binds() {
		if recipes.UnsharedBind(b.Source, engine) {
			out = append(out, b)
		}
	}
	return out
}

// unsharedWhy says what a bind mount of a folder the engine does not share
// does, and what to do.
func unsharedWhy(src string, liveMounts bool) string {
	if liveMounts {
		return tr("%s is not in a folder shared with the Docker Engine, so the container sees an empty folder there. Share it in Settings → Docker and create the container again.", src)
	}
	return tr("%s is not in a folder shared with the Docker Engine, so the container sees an empty folder there. Share it in Settings → Docker, restart the engine, and create the container again.", src)
}

// unsharedMark warns of a container's bind mounts of folders the engine
// does not share, or is nothing.
func unsharedMark(v unsharedView) ggui.Widget {
	if len(v.binds) == 0 {
		return widget.Box()
	}
	return tip(glyph(lucide.TriangleExclamationPoint, 13).Color(colorAmber), unsharedWhy(v.binds[0].Source, v.live))
}

// containerBinds lists a container's bind mounts, warning of those of
// folders the engine does not share (in Docker Engine sandbox mode).
func containerBinds(c dockerapi.Container, engine sandbox.Spec, sandboxEngine bool, liveMounts ggui.Readable[bool]) []ggui.Widget {
	binds := c.Binds()
	if len(binds) == 0 {
		return nil
	}
	var unshared []dockerapi.Mount
	desc := tr("Paths in the Docker Engine's sandbox, mounted into the container.")
	if sandboxEngine {
		desc = tr("Host paths mounted directly into the container's sandbox.")
	} else {
		unshared = unsharedBinds(c, engine)
	}
	parts := []ggui.Widget{sectionTitle(tr("Bind mounts"), desc)}
	for _, b := range binds {
		label := b.Source + " → " + b.Destination
		if !b.RW {
			label += " (ro)"
		}
		mark := glyph(lucide.Folder, 14).Color(colorGray)
		row := []ggui.Widget{mark, widget.Expanded(ui.Mono(label).Size(12).NoWrap().Ellipsis())}
		if slices.Contains(unshared, b) {
			src := b.Source
			row[0] = glyph(lucide.TriangleExclamationPoint, 14).Color(colorAmber)
			parts = append(parts, widget.Row(row...).Gap(10).Align(widget.AlignCenter),
				ui.CaptionOf(ggui.Map(liveMounts, func(live bool) string { return unsharedWhy(src, live) })))
			continue
		}
		parts = append(parts, widget.Row(row...).Gap(10).Align(widget.AlignCenter))
	}
	return parts
}

// containerConnect tells how to reach what the container serves, and the
// commands that reach the container itself.
func containerConnect(m *Model, c dockerapi.Container) ggui.Widget {
	parts := []ggui.Widget{sectionTitle(tr("Published ports"), publishedPortsNote(m))}
	se := ggui.Peek(m.Settings).SandboxEngine
	engine, _ := m.RecipeSandbox(ggui.Peek(m.Rows), recipes.Docker)
	hostPorts := engineHostPorts(m, se, engine)
	ports := containerPorts(c)
	inside := false
	for _, p := range ports {
		if p.PublicPort == 0 {
			inside = true
			continue
		}
		host := fmt.Sprintf("127.0.0.1:%d", p.PublicPort)
		label := fmt.Sprintf("%s → %d/%s", host, p.PrivatePort, p.Type)
		reach := portReached
		if c.State == "running" {
			reach = hostPorts.reach(p)
		}
		open := c.State == "running" && reach != portBusy
		mark := glyph(lucide.Plug, 14).Color(colorGray)
		var text ggui.Widget = ui.Mono(label).Size(12)
		switch reach {
		case portBusy:
			mark = glyph(lucide.TriangleExclamationPoint, 14).Color(colorAmber)
			text = widget.Column(text, ui.Caption(tr("Something on this computer has port %d, so this one does not reach it.", p.PublicPort)).Size(11)).Gap(1)
		case portWaiting:
			text = widget.Column(text, ui.Caption(tr("Not published on this computer yet: the engine publishes it once the container listens on it.")).Size(11)).Gap(1)
		}
		var act ggui.Widget = iconButton(lucide.Copy, tr("Copy address"), func() { m.Copy(host, host) })
		if p.Type == "tcp" {
			url := "http://" + host
			if open && reach == portReached {
				mark = glyph(lucide.Plug, 14).Color(colorGreen)
			}
			act = widget.Row(
				ui.Button(tr("Open"), func() { m.Open(url) }).Outline().Size(ui.ButtonExtraSmall).Icon(glyph(lucide.ExternalLink, 12)).Disabled(!open),
				iconButton(lucide.Copy, tr("Copy address"), func() { m.Copy(url, url) }),
			).Gap(4)
		}
		parts = append(parts, widget.Row(mark, widget.Expanded(text), act).Gap(10).Align(widget.AlignCenter))
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
	parts = append(parts, containerNetworks(m, c, se)...)
	parts = append(parts, containerBinds(c, engine.Spec, se, dockerLiveMountsOf(m))...)
	name := c.Name()
	parts = append(parts,
		sectionTitle(tr("Commands"), tr("For a terminal where docker uses the gvx context.")),
		copyable(m, "docker logs -f "+name, "docker logs"),
	)
	if !se || m.peekEngineFeatures().Control {
		parts = append(parts, copyable(m, "docker exec -it "+name+" sh", "docker exec"))
	}
	parts = append(parts, copyable(m, "docker inspect "+name, "docker inspect"))
	return widget.Padding(widget.Column(parts...).Gap(10).Stretch(), 8, 0)
}

// containerNetworks lists the networks a container is on, with its
// address on each and the names other containers reach it by there.
func containerNetworks(m *Model, c dockerapi.Container, se bool) []ggui.Widget {
	nets := c.Networks()
	if len(nets) == 0 {
		return nil
	}
	parts := []ggui.Widget{sectionTitle(tr("Networks"), tr("Other containers on a network reach this one at its address there, or by name."))}
	for _, n := range nets {
		var names []string
		for _, nm := range n.DNSNames {
			if nm != c.Name() && nm != c.ShortID() {
				names = append(names, nm)
			}
		}
		if len(names) == 0 {
			names = slices.DeleteFunc(slices.Clone(n.Aliases), func(a string) bool { return a == c.Name() })
		}
		caption := tr("Reached as %s", strings.Join(append([]string{c.Name()}, names...), ", "))
		switch {
		case se && (n.Name == "bridge" || n.Name == "host"):
			caption = tr("A network of its own; other containers cannot reach it here.")
		case n.Name == "bridge":
			caption = tr("Reached by address only on the default bridge.")
		case n.Name == "host" || n.Name == "none":
			caption = ""
		}
		label := []ggui.Widget{widget.Text(n.Name).Size(13).Weight(ggui.WeightMedium).NoWrap().Ellipsis()}
		if caption != "" {
			label = append(label, ui.Caption(caption).Size(11).NoWrap().Ellipsis())
		}
		row := []ggui.Widget{glyph(lucide.Network, 14).Color(colorGray), widget.Expanded(widget.Column(label...).Gap(1))}
		addr := n.IPAddress
		if se && (n.Name == "bridge" || n.Name == "host") {
			addr = ""
		}
		if addr != "" {
			row = append(row, ui.Mono(addr).Size(12).NoWrap(), iconButton(lucide.Copy, tr("Copy address"), func() { m.Copy(addr, addr) }))
		}
		name := n.Name
		row = append(row, tip(iconButton(lucide.ArrowRight, tr("Show %s", name), func() { m.ShowNetwork(name) }), tr("Show network")))
		parts = append(parts, widget.Row(row...).Gap(10).Align(widget.AlignCenter))
	}
	return parts
}

// portReach is whether a container's published port reaches this computer,
// as the Docker Engine's sandbox publishes it.
type portReach int

const (
	portReached portReach = iota // it does, or the sandbox does not say
	portBusy                     // something on this computer has it
	portWaiting                  // not published yet
)

// hostPorts are the ports the Docker Engine's sandbox has on this computer,
// and those it could not take; known is false when it tells nothing of
// them: the sandbox engine, or the engine not running.
type hostPorts struct {
	known       bool
	auto        bool // -auto-publish publishes ports as containers listen
	ports, busy []sandbox.PublishedPort
}

func engineHostPorts(m *Model, se bool, engine Row) hostPorts {
	if se || engine.State.Status != sandbox.Running {
		return hostPorts{}
	}
	spec, ok := m.activeSpec(engine.Spec.ID)
	return hostPorts{known: true, auto: ok && sandbox.HasAutoPublish(spec), ports: engine.State.Ports, busy: engine.State.Busy}
}

// reach finds a container's published port among them: it is the same
// number there, whether -auto-publish published it or the range opened at
// start. Without -auto-publish only that range reaches this computer.
func (h hostPorts) reach(p dockerapi.Port) portReach {
	match := func(q sandbox.PublishedPort) bool { return q.Proto == p.Type && q.HostPort() == int(p.PublicPort) }
	switch {
	case !h.known || p.PublicPort == 0 || slices.ContainsFunc(h.ports, match):
		return portReached
	case slices.ContainsFunc(h.busy, match):
		return portBusy
	case h.auto:
		return portWaiting
	}
	return portReached
}

// publishedPortsNote tells where a container's published port is reached:
// under its own number when the engine's sandbox publishes ports as they
// come, and inside the range it opened at start otherwise.
func publishedPortsNote(m *Model) string {
	if ggui.Peek(m.Settings).SandboxEngine {
		return tr("Published container ports reach this computer on 127.0.0.1.")
	}
	if engine, ok := m.RecipeSandbox(ggui.Peek(m.Rows), recipes.Docker); ok && !sandbox.HasAutoPublish(engine.Spec) {
		return tr("Published container ports must be inside 9000–9019, the range the sandbox opened at start.")
	}
	return tr("A published container port reaches this computer at 127.0.0.1 under the same number, unless something here has that port already.")
}
