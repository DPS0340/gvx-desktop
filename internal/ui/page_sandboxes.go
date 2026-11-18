package ui

import (
	"fmt"
	"strings"
	"time"

	"github.com/ironpark/ggui"
	"github.com/ironpark/ggui/icons/lucide"
	"github.com/ironpark/ggui/ui"
	"github.com/ironpark/ggui/ui/icons"
	"github.com/ironpark/ggui/widget"

	"github.com/ironpark/gvx-desktop/internal/recipes"
	"github.com/ironpark/gvx-desktop/internal/rungv"
	"github.com/ironpark/gvx-desktop/internal/sandbox"
)

func sandboxesPage(m *Model) ggui.Widget {
	visible := ggui.Combine(m.Rows, m.Query, func(rows []Row, q string) []Row {
		q = strings.ToLower(strings.TrimSpace(q))
		if q == "" {
			return rows
		}
		var out []Row
		for _, r := range rows {
			if strings.Contains(strings.ToLower(r.Spec.Name+" "+r.Spec.Image), q) {
				out = append(out, r)
			}
		}
		return out
	})
	summary := ui.CaptionOf(ggui.Combine(m.Running, m.Specs, func(n int, s []sandbox.Spec) string {
		return fmt.Sprintf(tr("%d running · %d total. Ports and mounts are fixed when a sandbox starts."), n, len(s))
	}))
	selected := ggui.Bind(m.Selected.Get, m.Select)

	count := ggui.Combine(visible, m.Rows, func(v, all []Row) string { return showing(len(v), len(all)) })
	// The table is as tall as its rows up to six, and scrolls past that, so
	// the details below stay in reach.
	table := ui.Table(visible, func(r Row) string { return r.Spec.ID },
		ui.Col(tr("Status"), func(r ggui.Readable[Row]) ggui.Widget {
			return ggui.View(ggui.Map(r, func(r Row) sandbox.State { return r.State }), statusLabel)
		}).W(150),
		ui.Col(tr("Name"), func(r ggui.Readable[Row]) ggui.Widget {
			return widget.Column(
				widget.TextOf(ggui.Map(r, func(r Row) string { return r.Spec.Name })).Size(13).Weight(ggui.WeightMedium).NoWrap().Ellipsis(),
				ui.CaptionOf(ggui.Map(r, func(r Row) string { return r.Spec.Image })).Size(11).NoWrap().Ellipsis(),
			).Gap(1)
		}).Grow(2),
		ui.Col(tr("Ports"), func(r ggui.Readable[Row]) ggui.Widget {
			return ui.CaptionOf(ggui.Map(r, portsSummary)).Size(12).NoWrap().Ellipsis()
		}).Grow(2),
		ui.Col(tr("Uptime"), func(r ggui.Readable[Row]) ggui.Widget {
			return ui.CaptionOf(ggui.Combine(r, m.Now, func(r Row, now time.Time) string {
				if r.State.Status.Active() {
					return uptime(r.State.StartedAt, now)
				}
				if !r.State.ExitedAt.IsZero() {
					return tr("stopped %s", ago(r.State.ExitedAt, now))
				}
				return "—"
			})).Size(12).NoWrap()
		}).W(160),
		ui.Col("", func(r ggui.Readable[Row]) ggui.Widget {
			return ggui.View(r, func(r Row) ggui.Widget {
				return widget.Row(sandboxQuickActions(m, r), rowMenu(m, r)).Gap(2).Justify(widget.JustifyEnd)
			})
		}).W(84).Identified("actions"),
	).BindSelected(selected).RowName(func(r Row) string { return r.Spec.Name }).RowHeight(48).MaxRows(6).
		Toolbar(filterBar(m.Query, tr("Filter by name or image"), count))

	empty := ui.Empty(tr("No sandboxes"), tr("A sandbox runs one OCI image in its own unprivileged process. Create one, or start the Docker Engine or Kubernetes recipe.")).
		Media(glyph(lucide.Box, 32).Color(colorGray)).
		Action(widget.Row(
			ui.Button(tr("New sandbox"), func() { m.NewSandbox("") }).Icon(glyph(lucide.Plus, 16)),
			ui.Button("Docker Engine", func() { m.CreateFromRecipe(recipes.Docker) }).Outline(),
		).Gap(8)).Outline()

	list := ggui.If(ggui.Map(m.Specs, func(s []sandbox.Spec) bool { return len(s) > 0 }), func() ggui.Widget {
		return widget.Column(
			tableCard(table),
			detailPanel(m),
		).Gap(16).Stretch()
	}).Else(func() ggui.Widget { return empty })

	return page(
		pageHeader(tr("Sandboxes"), summary,
			ui.Button(tr("New sandbox"), func() { m.NewSandbox("") }).Icon(glyph(lucide.Plus, 16)).Shortcut("cmd+n"),
		),
		list,
	)
}

func portsSummary(r Row) string {
	if len(r.State.Ports) > 0 && r.State.Status.Active() {
		var parts []string
		for i, p := range r.State.Ports {
			if i == 3 {
				parts = append(parts, fmt.Sprintf("+%d", len(r.State.Ports)-3))
				break
			}
			parts = append(parts, fmt.Sprintf("%s→%d", shortHost(p.Host), p.Guest))
		}
		return strings.Join(parts, ", ")
	}
	if len(r.Spec.Ports) == 0 {
		return "—"
	}
	return strings.Join(r.Spec.Ports, ", ")
}

func shortHost(h string) string {
	if strings.HasPrefix(h, "/") || strings.Contains(h, `\`) {
		return "unix:" + h[strings.LastIndexAny(h, `/\`)+1:]
	}
	return strings.TrimPrefix(h, "127.0.0.1:")
}

func rowMenu(m *Model, r Row) ggui.Widget {
	id := r.Spec.ID
	active := r.State.Status.Active()
	entries := []ggui.Widget{
		ui.MenuItem(tr("Logs"), func() { m.ShowSandbox(id, 0) }),
		ui.MenuItem(tr("Edit…"), func() { m.EditSandbox(id) }),
		ui.MenuItem(tr("Duplicate…"), func() { m.Duplicate(id) }),
		ui.MenuItem(tr("Copy command line"), func() { m.Copy(r.Spec.CommandLine(sandbox.Paths{}), tr("rungv command line")) }),
	}
	if active {
		entries = append(entries, ui.MenuItem(tr("Restart"), func() { m.RestartSandbox(id) }))
	}
	entries = append(entries, ui.MenuDivider(), ui.MenuItem(tr("Delete"), func() { m.AskDelete(id) }).Destructive())
	return ui.MenuOf(glyph(lucide.Ellipsis, 16), entries...).Name(tr("More actions for %s", r.Spec.Name))
}

// ---- Detail -------------------------------------------------------------

func detailPanel(m *Model) ggui.Widget {
	current := ggui.Combine(m.Rows, m.Selected, func(rows []Row, id string) Row {
		for _, r := range rows {
			if r.Spec.ID == id {
				return r
			}
		}
		return Row{}
	})
	hasSelection := ggui.Map(current, func(r Row) bool { return r.Spec.ID != "" })
	return entering(m.Selected, func(string) ggui.Widget {
		return ggui.If(hasSelection, func() ggui.Widget {
			header := ggui.View(current, func(r Row) ggui.Widget { return detailHeader(m, r) })
			tabs := ui.Tabs(m.Detail,
				ui.Tab(tr("Logs"), logsView(m, current)),
				ui.Tab(tr("Connect"), ggui.View(current, func(r Row) ggui.Widget { return connectView(m, r) })),
				ui.Tab(tr("Configuration"), ggui.View(ggui.Map(current, func(r Row) sandbox.Spec { return r.Spec }).WithEqual(sandbox.Spec.Equal), func(s sandbox.Spec) ggui.Widget { return configView(m, s) })),
				ui.Tab(tr("Snapshots"), ggui.View(current, func(r Row) ggui.Widget { return sandboxSnapshots(m, r) })),
			).Line().Name(tr("Sandbox details"))
			return ui.Card(widget.Column(header, tabs).Gap(12).Stretch())
		}).Else(func() ggui.Widget {
			return ui.Empty(tr("Select a sandbox"), tr("Its logs, ports and settings show here.")).Outline()
		})
	})
}

func detailHeader(m *Model, r Row) ggui.Widget {
	id := r.Spec.ID
	st := r.State
	var actions []ggui.Widget
	switch {
	case st.Status == sandbox.Stopping:
		actions = append(actions, ui.Button(tr("Stopping…"), nil).Outline().Disabled(true))
	case st.Status.Active():
		actions = append(actions,
			ui.Button(tr("Stop"), func() { m.StopSandbox(id) }).Outline().Icon(glyph(lucide.Square, 14)),
			ui.Button(tr("Restart"), func() { m.RestartSandbox(id) }).Outline().Icon(glyph(lucide.RotateCcw, 14)),
		)
	default:
		actions = append(actions, ui.Button(tr("Start"), func() { m.StartSandbox(id) }).Icon(glyph(lucide.Play, 14)))
	}
	actions = append(actions,
		ui.Button(tr("Edit"), func() { m.EditSandbox(id) }).Outline().Icon(glyph(lucide.Pencil, 14)),
		rowMenu(m, r),
	)
	meta := []string{r.Spec.Image}
	if st.Platform != "" {
		meta = append(meta, st.Platform)
	}
	if st.PID != 0 {
		meta = append(meta, fmt.Sprintf("pid %d", st.PID))
	}
	if st.Restored != "" {
		meta = append(meta, tr("restored from %s", shortPath(st.Restored)))
	}
	head := widget.Column(
		widget.Row(ui.Title(r.Spec.Name).Size(20), statusLabel(st)).Gap(12).Align(widget.AlignCenter),
		ui.Caption(strings.Join(meta, " · ")).Size(12),
	).Gap(4)
	var problem ggui.Widget = widget.Box()
	if st.Status == sandbox.Failed {
		desc := st.Error
		if st.Advice != "" {
			desc = st.Advice + "\n" + st.Error
		}
		problem = ui.Alert(tr("Exited with code %d", st.ExitCode), desc).Destructive().Icon(icons.Alert)
	}
	return widget.Column(
		widget.Responsive(680,
			widget.Row(widget.Expanded(head), widget.Row(actions...).Gap(8)).Gap(16).Align(widget.AlignCenter),
			widget.Column(head, widget.Wrap(actions...).Gap(8)).Gap(12).Stretch(),
		),
		problem,
	).Gap(12).Stretch()
}

func shortPath(p string) string {
	if i := strings.LastIndexAny(p, `/\`); i >= 0 {
		return p[i+1:]
	}
	return p
}

// logsView follows the selected sandbox's output.
func logsView(m *Model, current ggui.Readable[Row]) ggui.Widget {
	filter := ggui.State("")
	shown := ggui.Combine(m.Logs, filter, func(lines []sandbox.LogLine, f string) []sandbox.LogLine {
		f = strings.ToLower(strings.TrimSpace(f))
		if f == "" {
			return lines
		}
		var out []sandbox.LogLine
		for _, l := range lines {
			if strings.Contains(strings.ToLower(l.Text), f) {
				out = append(out, l)
			}
		}
		return out
	}).WithEqual(nil)
	rows := widget.EachKeyed(shown, func(l sandbox.LogLine) int64 { return l.Seq }, func(row widget.EachItem[sandbox.LogLine]) ggui.Widget {
		l := row.Item
		t := ui.Mono(l.Text).Size(12).NoWrap().Ellipsis().Selectable()
		switch l.Stream {
		case sandbox.System:
			t = t.Color(colorBlue)
		case sandbox.Stderr:
			if strings.HasPrefix(l.Text, "rungv:") {
				t = t.Color(colorGray)
			}
		}
		return widget.Row(
			ui.Mono(l.Time.Format("15:04:05")).Size(11).Color(colorGray).NoWrap(),
			widget.Expanded(t),
		).Gap(10)
	}).ItemExtent(19).Else(func() ggui.Widget {
		return widget.Padding(ui.Caption(tr("No output yet. Start the sandbox to see what it writes.")), 12)
	})
	scroll := widget.Scroll(widget.Padding(rows, 8, 12)).FollowEnd()
	toolbar := widget.Row(
		widget.Box(ui.TextField(filter).Placeholder(tr("Filter lines")).Name(tr("Filter logs"))).Width(260),
		widget.Spacer(),
		ui.CaptionOf(ggui.Map(shown, func(l []sandbox.LogLine) string { return fmt.Sprintf(tr("%d lines"), len(l)) })).Size(12),
		tip(iconButton(lucide.Copy, tr("Copy logs"), func() {
			var b strings.Builder
			for _, l := range ggui.Peek(shown) {
				b.WriteString(l.Text)
				b.WriteByte('\n')
			}
			m.Copy(b.String(), tr("The visible log lines are on the clipboard."))
		}), tr("Copy")),
		tip(iconButton(lucide.Eraser, tr("Clear logs"), func() { m.ClearLogs(ggui.Peek(m.Selected)) }), tr("Clear")),
	).Gap(6).Align(widget.AlignCenter)
	return widget.Column(
		toolbar,
		widget.Box(scroll).Height(380).Radius(8).Border(1, colorGrayA).Fill(rgbA(0x000000, 0x08)),
	).Gap(10).Stretch()
}

// connectView tells how to reach what the sandbox serves.
func connectView(m *Model, r Row) ggui.Widget {
	var parts []ggui.Widget
	switch r.Spec.Recipe {
	case recipes.Docker:
		parts = append(parts, dockerConnect(m, r))
	case recipes.Kubernetes:
		parts = append(parts, kubeConnect(m, r))
	}
	parts = append(parts, sectionTitle(tr("Published ports"), tr("Bound on the host before the guest starts. A guest port outside these is reachable only inside the sandbox.")))
	if r.State.Status.Active() && len(r.State.Ports) > 0 {
		for _, p := range r.State.Ports {
			label := fmt.Sprintf("%s → %d/%s", p.Host, p.Guest, p.Proto)
			var act ggui.Widget = widget.Box()
			if url := p.URL(); url != "" {
				act = widget.Row(
					ui.Button(tr("Open"), func() { m.Open(url) }).Outline().Size(ui.ButtonExtraSmall).Icon(glyph(lucide.ExternalLink, 12)),
					iconButton(lucide.Copy, tr("Copy address"), func() { m.Copy(url, url) }),
				).Gap(4)
			} else {
				act = iconButton(lucide.Copy, tr("Copy address"), func() { m.Copy(p.Host, p.Host) })
			}
			parts = append(parts, widget.Row(glyph(lucide.Plug, 14).Color(colorGreen), widget.Expanded(ui.Mono(label).Size(12)), act).Gap(10).Align(widget.AlignCenter))
		}
	} else if len(r.Spec.Ports) > 0 {
		for _, p := range r.Spec.Ports {
			parts = append(parts, widget.Row(glyph(lucide.Plug, 14).Color(colorGray), ui.Mono(p).Size(12)).Gap(10))
		}
		parts = append(parts, ui.Caption(tr("Start the sandbox to bind these ports; a port written without a host port gets a free one.")))
	} else {
		parts = append(parts, ui.Caption(tr("No ports are published. Edit the sandbox to add some, such as 8080:80.")))
	}
	if r.Spec.Workspace != "" {
		parts = append(parts, sectionTitle(tr("Workspace"), tr("Mounted at /workspace in the guest.")),
			widget.Row(widget.Expanded(ui.Mono(rungv.ExpandHome(r.Spec.Workspace)).Size(12)),
				ui.Button(tr("Reveal"), func() { m.Open(rungv.ExpandHome(r.Spec.Workspace)) }).Outline().Size(ui.ButtonExtraSmall)).Gap(8).Align(widget.AlignCenter))
	}
	return widget.Padding(widget.Column(parts...).Gap(10).Stretch(), 8, 0)
}

func dockerConnect(m *Model, r Row) ggui.Widget {
	host := m.DockerHost()
	ready := r.State.Status == sandbox.Running
	_, hasCLI := m.DockerCLI()
	status := tr("Start the sandbox, then point the docker CLI at it.")
	if ready {
		status = tr("dockerd is listening. The docker CLI on this machine can use it.")
	}
	return widget.Column(
		sectionTitle("Docker CLI", status),
		copyable(m, "docker context create gvx --docker host="+host, "docker context create"),
		copyable(m, "docker context use gvx", "docker context use"),
		copyable(m, "export DOCKER_HOST="+host, "DOCKER_HOST"),
		widget.Row(
			ui.Button(tr("Create & use context"), m.CreateDockerContext).Size(ui.ButtonSmall).Disabled(!hasCLI),
			ui.Button(tr("Open Docker page"), func() { m.Go(PageDocker) }).Outline().Size(ui.ButtonSmall),
		).Gap(8),
		ui.Divider(),
	).Gap(8).Stretch()
}

func kubeConnect(m *Model, r Row) ggui.Widget {
	cfg := recipes.Kubeconfig()
	return widget.Column(
		sectionTitle("kubectl", tr("The server writes its kubeconfig to the host when it starts.")),
		copyable(m, "export KUBECONFIG="+cfg, "KUBECONFIG"),
		copyable(m, "kubectl get nodes", "kubectl get nodes"),
		widget.Row(
			ui.Button(tr("Reveal kubeconfig"), func() { m.Open(cfg) }).Outline().Size(ui.ButtonSmall),
		).Gap(8),
		ui.Divider(),
	).Gap(8).Stretch()
}

// configView shows the spec in the words of the rungv command line.
func configView(m *Model, s sandbox.Spec) ggui.Widget {
	line := wrapCommand(s.RunArgs(sandbox.Paths{}))
	join := func(v []string) string {
		if len(v) == 0 {
			return ""
		}
		return strings.Join(v, "\n")
	}
	var mounts []string
	for _, mt := range s.Mounts {
		switch mt.Type {
		case sandbox.MountTmpfs:
			mounts = append(mounts, fmt.Sprintf("tmpfs %s %s", mt.Target, mt.Size))
		default:
			ro := ""
			if mt.ReadOnly {
				ro = tr(" (read-only)")
			}
			mounts = append(mounts, fmt.Sprintf("%s %s → %s%s", mt.Type, mt.Source, mt.Target, ro))
		}
	}
	if s.Workspace != "" {
		mounts = append(mounts, tr("workspace %s → /workspace", s.Workspace))
	}
	if s.RootfsVolume != "" {
		mounts = append(mounts, tr("root file system → volume %s", s.RootfsVolume))
	}
	net := s.Network
	if net == "" {
		net = "nat"
	}
	if s.IsolateLAN {
		net += tr(", LAN isolated")
	}
	limits := []string{}
	if s.Memory != "" {
		limits = append(limits, tr("memory ")+s.Memory)
	}
	if s.CPUs != "" {
		limits = append(limits, s.CPUs+tr(" CPUs"))
	}
	sec := tr("privileged inside the sandbox")
	if s.Unprivileged {
		sec = tr("unprivileged")
	}
	if s.HostCACerts {
		sec += tr(", trusts host root certificates")
	}
	return widget.Padding(widget.Column(
		ui.CodeBlock(line).Header(tr("Command line")).Language("sh").CopyButton(true).Small(),
		widget.Column(
			kvText(tr("Image"), s.Image),
			kvText(tr("Command"), s.Command),
			kvText(tr("Environment"), join(s.Env)),
			kvText(tr("Mounts"), join(mounts)),
			kvText(tr("Network"), net),
			kvText(tr("Limits"), strings.Join(limits, ", ")),
			kvText(tr("Security"), sec),
			kvText(tr("Snapshots"), map[bool]string{true: tr("enabled (control socket + bundle directory)"), false: tr("off")}[s.Checkpoint]),
			kvText(tr("Start with app"), map[bool]string{true: tr("yes"), false: tr("no")}[s.AutoStart]),
			kvText(tr("Ready when log has"), s.ReadyPattern),
		).Gap(8).Stretch(),
	).Gap(14).Stretch(), 8, 0)
}

func sandboxSnapshots(m *Model, r Row) ggui.Widget {
	id := r.Spec.ID
	if !r.Spec.Checkpoint {
		return widget.Padding(ui.Alert(tr("Snapshots are off for this sandbox"),
			tr("A snapshot needs a control socket and a bundle directory opened before the guest starts. Turn snapshots on, then restart the sandbox.")).
			Icon(icons.Info).
			Action(ui.Button(tr("Turn on"), func() {
				spec := r.Spec.Clone()
				spec.Checkpoint = true
				m.OpenEditor(spec)
			}).Size(ui.ButtonExtraSmall).Outline()), 8, 0)
	}
	running := r.State.Status.Active() && r.State.Status != sandbox.Stopping
	snaps := ggui.Map(m.Snapshots, func(all []sandbox.Snapshot) []sandbox.Snapshot {
		var out []sandbox.Snapshot
		for _, s := range all {
			if s.SandboxID == id {
				out = append(out, s)
			}
		}
		return out
	})
	return widget.Padding(widget.Column(
		widget.Row(
			ui.Button(tr("Snapshot and stop"), func() { m.Checkpoint(id, false) }).Disabled(!running).Icon(glyph(lucide.Camera, 14)),
			ui.Button(tr("Snapshot, keep running"), func() { m.Checkpoint(id, true) }).Outline().Disabled(!running),
		).Gap(8),
		ui.Caption(tr("A snapshot holds the task graph, memory pages, volumes and network namespaces. Restoring needs the same mounts, which this sandbox's settings provide.")),
		snapshotList(m, snaps, false),
	).Gap(12).Stretch(), 8, 0)
}

func snapshotList(m *Model, snaps ggui.Readable[[]sandbox.Snapshot], showSandbox bool) ggui.Widget {
	return widget.EachKeyed(snaps, func(s sandbox.Snapshot) string { return s.Dir }, func(row widget.EachItem[sandbox.Snapshot]) ggui.Widget {
		s := row.Item
		title := s.Time.Format(tr("Jan 2, 15:04:05"))
		desc := humanBytes(s.Size)
		if showSandbox {
			if spec, ok := m.spec(s.SandboxID); ok {
				title = spec.Name + " · " + title
			} else {
				title = tr("(deleted sandbox) · ") + title
			}
		}
		return ui.Item(title, desc+" · "+s.Dir).
			Media(glyph(lucide.Camera, 18).Color(colorBlue)).
			Action(widget.Row(
				ui.Button(tr("Restore"), func() { m.Restore(s) }).Outline().Size(ui.ButtonSmall).Icon(glyph(lucide.RotateCcw, 14)),
				iconButton(lucide.FolderOpen, tr("Reveal"), func() { m.Open(s.Dir) }),
				iconButton(lucide.Trash, tr("Delete snapshot"), func() { m.DeleteSnapshot(s) }),
			).Gap(4).Align(widget.AlignCenter)).Outline().Small()
	}).Gap(8).Else(func() ggui.Widget {
		return ui.Caption(tr("No snapshots yet."))
	})
}
