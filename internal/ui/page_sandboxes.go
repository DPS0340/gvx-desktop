package ui

import (
	"fmt"
	"strings"
	"time"

	"github.com/ironpark/ggui"
	"github.com/ironpark/ggui/icons/lucide"
	"github.com/ironpark/ggui/ui"
	"github.com/ironpark/ggui/ui/icons"

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
		return fmt.Sprintf("%d running · %d total. Ports and mounts are fixed when a sandbox starts.", n, len(s))
	}))
	selected := ggui.Bind(m.Selected.Get, m.Select)

	table := ui.Table(visible, func(r Row) string { return r.Spec.ID },
		ui.Col("Status", func(r ggui.Readable[Row]) ggui.Widget {
			return ggui.View(ggui.Map(r, func(r Row) sandbox.State { return r.State }), statusLabel)
		}).W(150),
		ui.Col("Name", func(r ggui.Readable[Row]) ggui.Widget {
			return ggui.Column(
				ggui.TextOf(ggui.Map(r, func(r Row) string { return r.Spec.Name })).Size(13).Weight(ggui.WeightMedium).NoWrap().Ellipsis(),
				ui.CaptionOf(ggui.Map(r, func(r Row) string { return r.Spec.Image })).Size(11).NoWrap().Ellipsis(),
			).Gap(1)
		}).Grow(2),
		ui.Col("Ports", func(r ggui.Readable[Row]) ggui.Widget {
			return ui.CaptionOf(ggui.Map(r, portsSummary)).Size(12).NoWrap().Ellipsis()
		}).Grow(2),
		ui.Col("Uptime", func(r ggui.Readable[Row]) ggui.Widget {
			return ui.CaptionOf(ggui.Combine(r, m.Now, func(r Row, now time.Time) string {
				if r.State.Status.Active() {
					return uptime(r.State.StartedAt, now)
				}
				if !r.State.ExitedAt.IsZero() {
					return "stopped " + ago(r.State.ExitedAt, now)
				}
				return "—"
			})).Size(12).NoWrap()
		}).W(120),
		ui.Col("", func(r ggui.Readable[Row]) ggui.Widget {
			return ggui.View(r, func(r Row) ggui.Widget {
				return ggui.Row(sandboxQuickActions(m, r), rowMenu(m, r)).Gap(2).Justify(ggui.JustifyEnd)
			})
		}).W(84).Identified("actions"),
	).BindSelected(selected).RowName(func(r Row) string { return r.Spec.Name }).RowHeight(48).Height(260)

	empty := ui.Empty("No sandboxes", "A sandbox runs one OCI image in its own unprivileged process. Create one, or start the Docker Engine or Kubernetes recipe.").
		Media(glyph(lucide.Box, 32).Color(colorGray)).
		Action(ggui.Row(
			ui.Button("New sandbox", func() { m.NewSandbox("") }).Icon(glyph(lucide.Plus, 16)),
			ui.Button("Docker Engine", func() { m.CreateFromRecipe(recipes.Docker) }).Outline(),
		).Gap(8)).Outline()

	list := ggui.If(ggui.Map(m.Specs, func(s []sandbox.Spec) bool { return len(s) > 0 }), func() ggui.Widget {
		return ggui.Column(
			ggui.Box(ui.TextField(m.Query).Placeholder("Filter by name or image").Name("Filter sandboxes")).MaxWidth(320),
			ui.Card(table).Pad(0),
			detailPanel(m),
		).Gap(16).Stretch()
	}).Else(func() ggui.Widget { return empty })

	return page(
		pageHeader("Sandboxes", summary,
			ui.Button("New sandbox", func() { m.NewSandbox("") }).Icon(glyph(lucide.Plus, 16)).Shortcut("cmd+n"),
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
		ui.MenuItem("Logs", func() { m.ShowSandbox(id, 0) }),
		ui.MenuItem("Edit…", func() { m.EditSandbox(id) }),
		ui.MenuItem("Duplicate…", func() { m.Duplicate(id) }),
		ui.MenuItem("Copy command line", func() { m.Copy(r.Spec.CommandLine(sandbox.Paths{}), "rungv command line") }),
	}
	if active {
		entries = append(entries, ui.MenuItem("Restart", func() { m.RestartSandbox(id) }))
	}
	entries = append(entries, ui.MenuDivider(), ui.MenuItem("Delete", func() { m.AskDelete(id) }).Destructive())
	return ui.MenuOf(glyph(lucide.Ellipsis, 16), entries...).Name("More actions for " + r.Spec.Name)
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
	return ggui.If(hasSelection, func() ggui.Widget {
		header := ggui.View(current, func(r Row) ggui.Widget { return detailHeader(m, r) })
		tabs := ui.Tabs(m.Detail,
			ui.Tab("Logs", logsView(m, current)),
			ui.Tab("Connect", ggui.View(current, func(r Row) ggui.Widget { return connectView(m, r) })),
			ui.Tab("Configuration", ggui.View(ggui.Map(current, func(r Row) sandbox.Spec { return r.Spec }).WithEqual(sandbox.Spec.Equal), func(s sandbox.Spec) ggui.Widget { return configView(m, s) })),
			ui.Tab("Snapshots", ggui.View(current, func(r Row) ggui.Widget { return sandboxSnapshots(m, r) })),
		).Line().Name("Sandbox details")
		return ui.Card(ggui.Column(header, tabs).Gap(12).Stretch())
	}).Else(func() ggui.Widget {
		return ui.Empty("Select a sandbox", "Its logs, ports and settings show here.").Outline()
	})
}

func detailHeader(m *Model, r Row) ggui.Widget {
	id := r.Spec.ID
	st := r.State
	var actions []ggui.Widget
	switch {
	case st.Status == sandbox.Stopping:
		actions = append(actions, ui.Button("Stopping…", nil).Outline().Disabled(true))
	case st.Status.Active():
		actions = append(actions,
			ui.Button("Stop", func() { m.StopSandbox(id) }).Outline().Icon(glyph(lucide.Square, 14)),
			ui.Button("Restart", func() { m.RestartSandbox(id) }).Outline().Icon(glyph(lucide.RotateCcw, 14)),
		)
	default:
		actions = append(actions, ui.Button("Start", func() { m.StartSandbox(id) }).Icon(glyph(lucide.Play, 14)))
	}
	actions = append(actions,
		ui.Button("Edit", func() { m.EditSandbox(id) }).Outline().Icon(glyph(lucide.Pencil, 14)),
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
		meta = append(meta, "restored from "+shortPath(st.Restored))
	}
	head := ggui.Column(
		ggui.Row(ui.Title(r.Spec.Name).Size(20), statusLabel(st)).Gap(12).Align(ggui.AlignCenter),
		ui.Caption(strings.Join(meta, " · ")).Size(12),
	).Gap(4)
	var problem ggui.Widget = ggui.Box()
	if st.Status == sandbox.Failed {
		desc := st.Error
		if st.Advice != "" {
			desc = st.Advice + "\n" + st.Error
		}
		problem = ui.Alert(fmt.Sprintf("Exited with code %d", st.ExitCode), desc).Destructive().Icon(icons.Alert)
	}
	return ggui.Column(
		ggui.Responsive(680,
			ggui.Row(ggui.Expanded(head), ggui.Row(actions...).Gap(8)).Gap(16).Align(ggui.AlignCenter),
			ggui.Column(head, ggui.Wrap(actions...).Gap(8)).Gap(12).Stretch(),
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
	rows := ggui.EachKeyed(shown, func(l sandbox.LogLine) int64 { return l.Seq }, func(row ggui.EachItem[sandbox.LogLine]) ggui.Widget {
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
		return ggui.Row(
			ui.Mono(l.Time.Format("15:04:05")).Size(11).Color(colorGray).NoWrap(),
			ggui.Expanded(t),
		).Gap(10)
	}).ItemExtent(19).Else(func() ggui.Widget {
		return ggui.Padding(ui.Caption("No output yet. Start the sandbox to see what it writes."), 12)
	})
	scroll := ggui.Scroll(ggui.Padding(rows, 8, 12)).FollowEnd()
	toolbar := ggui.Row(
		ggui.Box(ui.TextField(filter).Placeholder("Filter lines").Name("Filter logs")).Width(260),
		ggui.Spacer(),
		ui.CaptionOf(ggui.Map(shown, func(l []sandbox.LogLine) string { return fmt.Sprintf("%d lines", len(l)) })).Size(12),
		tip(iconButton(lucide.Copy, "Copy logs", func() {
			var b strings.Builder
			for _, l := range ggui.Peek(shown) {
				b.WriteString(l.Text)
				b.WriteByte('\n')
			}
			m.Copy(b.String(), "The visible log lines are on the clipboard.")
		}), "Copy"),
		tip(iconButton(lucide.Eraser, "Clear logs", func() { m.ClearLogs(ggui.Peek(m.Selected)) }), "Clear"),
	).Gap(6).Align(ggui.AlignCenter)
	return ggui.Column(
		toolbar,
		ggui.Box(scroll).Height(380).Radius(8).Border(1, colorGrayA).Fill(rgbA(0x000000, 0x08)),
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
	parts = append(parts, sectionTitle("Published ports", "Bound on the host before the guest starts. A guest port outside these is reachable only inside the sandbox."))
	if r.State.Status.Active() && len(r.State.Ports) > 0 {
		for _, p := range r.State.Ports {
			p := p
			label := fmt.Sprintf("%s → %d/%s", p.Host, p.Guest, p.Proto)
			var act ggui.Widget = ggui.Box()
			if url := p.URL(); url != "" {
				act = ggui.Row(
					ui.Button("Open", func() { m.Open(url) }).Outline().Size(ui.ButtonExtraSmall).Icon(glyph(lucide.ExternalLink, 12)),
					iconButton(lucide.Copy, "Copy address", func() { m.Copy(url, url) }),
				).Gap(4)
			} else {
				act = iconButton(lucide.Copy, "Copy address", func() { m.Copy(p.Host, p.Host) })
			}
			parts = append(parts, ggui.Row(glyph(lucide.Plug, 14).Color(colorGreen), ggui.Expanded(ui.Mono(label).Size(12)), act).Gap(10).Align(ggui.AlignCenter))
		}
	} else if len(r.Spec.Ports) > 0 {
		for _, p := range r.Spec.Ports {
			parts = append(parts, ggui.Row(glyph(lucide.Plug, 14).Color(colorGray), ui.Mono(p).Size(12)).Gap(10))
		}
		parts = append(parts, ui.Caption("Start the sandbox to bind these ports; a port written without a host port gets a free one."))
	} else {
		parts = append(parts, ui.Caption("No ports are published. Edit the sandbox to add some, such as 8080:80."))
	}
	if r.Spec.Workspace != "" {
		parts = append(parts, sectionTitle("Workspace", "Mounted at /workspace in the guest."),
			ggui.Row(ggui.Expanded(ui.Mono(rungv.ExpandHome(r.Spec.Workspace)).Size(12)),
				ui.Button("Reveal", func() { m.Open(rungv.ExpandHome(r.Spec.Workspace)) }).Outline().Size(ui.ButtonExtraSmall)).Gap(8).Align(ggui.AlignCenter))
	}
	return ggui.Padding(ggui.Column(parts...).Gap(10).Stretch(), 8, 0)
}

func dockerConnect(m *Model, r Row) ggui.Widget {
	host := recipes.DockerHost()
	ready := r.State.Status == sandbox.Running
	_, hasCLI := DockerCLI()
	status := "Start the sandbox, then point the docker CLI at it."
	if ready {
		status = "dockerd is listening. The docker CLI on this machine can use it."
	}
	return ggui.Column(
		sectionTitle("Docker CLI", status),
		copyable(m, "docker context create gvx --docker host="+host, "docker context create"),
		copyable(m, "docker context use gvx", "docker context use"),
		copyable(m, "export DOCKER_HOST="+host, "DOCKER_HOST"),
		ggui.Row(
			ui.Button("Create & use context", m.CreateDockerContext).Size(ui.ButtonSmall).Disabled(!hasCLI),
			ui.Button("Open Docker page", func() { m.Go(PageDocker) }).Outline().Size(ui.ButtonSmall),
		).Gap(8),
		ui.Divider(),
	).Gap(8).Stretch()
}

func kubeConnect(m *Model, r Row) ggui.Widget {
	cfg := recipes.Kubeconfig()
	return ggui.Column(
		sectionTitle("kubectl", "The server writes its kubeconfig to the host when it starts."),
		copyable(m, "export KUBECONFIG="+cfg, "KUBECONFIG"),
		copyable(m, "kubectl get nodes", "kubectl get nodes"),
		ggui.Row(
			ui.Button("Reveal kubeconfig", func() { m.Open(cfg) }).Outline().Size(ui.ButtonSmall),
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
				ro = " (read-only)"
			}
			mounts = append(mounts, fmt.Sprintf("%s %s → %s%s", mt.Type, mt.Source, mt.Target, ro))
		}
	}
	if s.Workspace != "" {
		mounts = append(mounts, "workspace "+s.Workspace+" → /workspace")
	}
	if s.RootfsVolume != "" {
		mounts = append(mounts, "root file system → volume "+s.RootfsVolume)
	}
	net := s.Network
	if net == "" {
		net = "nat"
	}
	if s.IsolateLAN {
		net += ", LAN isolated"
	}
	limits := []string{}
	if s.Memory != "" {
		limits = append(limits, "memory "+s.Memory)
	}
	if s.CPUs != "" {
		limits = append(limits, s.CPUs+" CPUs")
	}
	sec := "privileged inside the sandbox"
	if s.Unprivileged {
		sec = "unprivileged"
	}
	if s.HostCACerts {
		sec += ", trusts host root certificates"
	}
	return ggui.Padding(ggui.Column(
		ui.CodeBlock(line).Header("Command line").Language("sh").CopyButton(true).Small(),
		ggui.Column(
			kvText("Image", s.Image),
			kvText("Command", s.Command),
			kvText("Environment", join(s.Env)),
			kvText("Mounts", join(mounts)),
			kvText("Network", net),
			kvText("Limits", strings.Join(limits, ", ")),
			kvText("Security", sec),
			kvText("Snapshots", map[bool]string{true: "enabled (control socket + bundle directory)", false: "off"}[s.Checkpoint]),
			kvText("Start with app", map[bool]string{true: "yes", false: "no"}[s.AutoStart]),
			kvText("Ready when log has", s.ReadyPattern),
		).Gap(8).Stretch(),
	).Gap(14).Stretch(), 8, 0)
}

func sandboxSnapshots(m *Model, r Row) ggui.Widget {
	id := r.Spec.ID
	if !r.Spec.Checkpoint {
		return ggui.Padding(ui.Alert("Snapshots are off for this sandbox",
			"A snapshot needs a control socket and a bundle directory opened before the guest starts. Turn snapshots on, then restart the sandbox.").
			Icon(icons.Info).
			Action(ui.Button("Turn on", func() {
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
	return ggui.Padding(ggui.Column(
		ggui.Row(
			ui.Button("Snapshot and stop", func() { m.Checkpoint(id, false) }).Disabled(!running).Icon(glyph(lucide.Camera, 14)),
			ui.Button("Snapshot, keep running", func() { m.Checkpoint(id, true) }).Outline().Disabled(!running),
		).Gap(8),
		ui.Caption("A snapshot holds the task graph, memory pages, volumes and network namespaces. Restoring needs the same mounts, which this sandbox's settings provide."),
		snapshotList(m, snaps, false),
	).Gap(12).Stretch(), 8, 0)
}

func snapshotList(m *Model, snaps ggui.Readable[[]sandbox.Snapshot], showSandbox bool) ggui.Widget {
	return ggui.EachKeyed(snaps, func(s sandbox.Snapshot) string { return s.Dir }, func(row ggui.EachItem[sandbox.Snapshot]) ggui.Widget {
		s := row.Item
		title := s.Time.Format("Jan 2, 15:04:05")
		desc := humanBytes(s.Size)
		if showSandbox {
			if spec, ok := m.spec(s.SandboxID); ok {
				title = spec.Name + " · " + title
			} else {
				title = "(deleted sandbox) · " + title
			}
		}
		return ui.Item(title, desc+" · "+s.Dir).
			Media(glyph(lucide.Camera, 18).Color(colorBlue)).
			Action(ggui.Row(
				ui.Button("Restore", func() { m.Restore(s) }).Outline().Size(ui.ButtonSmall).Icon(glyph(lucide.RotateCcw, 14)),
				iconButton(lucide.FolderOpen, "Reveal", func() { m.Open(s.Dir) }),
				iconButton(lucide.Trash, "Delete snapshot", func() { m.DeleteSnapshot(s) }),
			).Gap(4).Align(ggui.AlignCenter)).Outline().Small()
	}).Gap(8).Else(func() ggui.Widget {
		return ui.Caption("No snapshots yet.")
	})
}
