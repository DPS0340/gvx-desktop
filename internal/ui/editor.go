package ui

import (
	"image/color"

	"github.com/ironpark/ggui"
	"github.com/ironpark/ggui/icons/lucide"
	"github.com/ironpark/ggui/runtime"
	"github.com/ironpark/ggui/ui"
	"github.com/ironpark/ggui/ui/icons"

	"github.com/ironpark/gvx-desktop/internal/rungv"
)

var colorGrayA = color.NRGBA{0x80, 0x80, 0x88, 0x40}

func rgbA(v uint32, a uint8) color.NRGBA {
	return color.NRGBA{uint8(v >> 16), uint8(v >> 8), uint8(v), a}
}

// editorSheet is the form that creates and edits a sandbox. Everything in
// it is fixed for a run, so the sheet says so where it matters and shows
// the exact rungv command it will run.
func editorSheet(m *Model) ggui.Widget {
	d := m.Draft
	str := func(sel func(*Draft) *string) ggui.Binding[string] { return d.Field(sel) }
	flag := func(sel func(*Draft) *bool) ggui.Binding[bool] { return d.Field(sel) }

	title := ggui.Map(d, func(x Draft) string {
		if x.ID == "" {
			return "New sandbox"
		}
		return "Edit " + x.Name
	})

	cached := ggui.Map(m.Images, func(l Listing[rungv.Image]) []string {
		var refs []string
		for _, img := range l.Items {
			if img.Repository != "<none>" {
				refs = append(refs, img.Ref())
			}
			if len(refs) == 8 {
				break
			}
		}
		return refs
	}).WithEqual(func(a, b []string) bool { return equalStrings(a, b) })
	imageChips := ggui.View(cached, func(refs []string) ggui.Widget {
		if len(refs) == 0 {
			return ggui.Box()
		}
		var chips []ggui.Widget
		for _, ref := range refs {
			ref := ref
			chips = append(chips, ui.Badge(ref).Outline().OnTap(func() {
				d.Update(func(x Draft) Draft {
					x.Image = ref
					if x.Name == "" {
						x.Name = uniqueName(ggui.Peek(m.Specs), sandboxName(ref))
					}
					return x
				})
			}))
		}
		return ggui.Column(ui.Caption("Cached images").Size(11), ggui.Wrap(chips...).Gap(6)).Gap(6)
	})

	general := ggui.Column(
		ui.Field("Image", ui.TextField(str(func(x *Draft) *string { return &x.Image })).Placeholder("nginx:alpine").Name("Image")).
			Help("Any OCI image for linux/"+goarch()+". A missing image is pulled at start."),
		imageChips,
		ui.Field("Name", ui.TextField(str(func(x *Draft) *string { return &x.Name })).Placeholder("web").Name("Sandbox name")),
		ui.Field("Command", ui.TextField(str(func(x *Draft) *string { return &x.Command })).Placeholder("Image default").Name("Command")).
			Help("Replaces the image's CMD and keeps its ENTRYPOINT. Quote words as in a shell."),
	).Gap(14).Stretch()

	ports := ggui.Column(
		ui.Field("Published ports", ui.TextField(str(func(x *Draft) *string { return &x.Ports })).Multiline().Lines(3).
			Placeholder("8080:80\n127.0.0.1:9000-9019:9000-9019\nunix:~/.gvx/app.sock:3000").Name("Published ports")).
			Help("One per line: [IP:][HOST:]GUEST[/udp]. Bound before the guest starts and fixed for the run: publish every port the guest will use, including ports of nested containers."),
	).Gap(14).Stretch()

	workspace := str(func(x *Draft) *string { return &x.Workspace })
	storage := ggui.Column(
		ui.Field("Volumes", ui.TextField(str(func(x *Draft) *string { return &x.Volumes })).Multiline().Lines(2).
			Placeholder("pgdata:/var/lib/postgresql/data\n/disk/cache.ext4:/cache").Name("Volumes")).
			Help("NAME:/guest/path[:ro]. Each named volume is one journaled ext4 image that persists; a source with a slash is an .ext4 file at that path."),
		ui.Field("Bind mounts", ui.TextField(str(func(x *Draft) *string { return &x.Binds })).Multiline().Lines(2).
			Placeholder("~/src:/src\n~/app.conf:/etc/app.conf:ro").Name("Bind mounts")).
			Help("HOST:/guest/path[:ro]. A host directory or one file. The sandbox may only reach what is mounted."),
		ui.Field("Workspace", ggui.Row(
			ggui.Expanded(ui.TextField(workspace).Placeholder("Folder mounted at /workspace").Name("Workspace folder")),
			ui.Button("Browse…", func() { browseFolder(m, workspace, nil) }).Outline(),
		).Gap(8)),
		ui.Switch(flag(func(x *Draft) *bool { return &x.WorkspaceRO }), "Mount the workspace read-only").Small(),
		ui.Field("Persistent root file system", ui.TextField(str(func(x *Draft) *string { return &x.RootfsVolume })).Placeholder("Volume name, or empty for memory").Name("Root file system volume")).
			Help("Writes outside mounts go to memory and vanish at exit unless a root volume keeps them."),
		ui.Field("Memory mounts (tmpfs)", ui.TextField(str(func(x *Draft) *string { return &x.Tmpfs })).Multiline().Lines(1).
			Placeholder("/tmp:size=256m").Name("tmpfs mounts")),
	).Gap(14).Stretch()

	resources := ggui.Row(
		ggui.Expanded(ui.Field("Memory limit", ui.TextField(str(func(x *Draft) *string { return &x.Memory })).Placeholder("Host RAM").Name("Memory limit")).Help("512m, 2g. The guest is killed (exit 137) when it passes the limit.")),
		ggui.Expanded(ui.Field("CPU limit", ui.TextField(str(func(x *Draft) *string { return &x.CPUs })).Placeholder("All CPUs").Name("CPU limit")).Help("1.5 CPUs. A hard cap on Windows only.")),
	).Gap(12).Align(ggui.AlignStart)

	env := ui.Field("Environment", ui.TextField(str(func(x *Draft) *string { return &x.Env })).Multiline().Lines(3).
		Placeholder("KEY=value\nHTTP_PROXY").Name("Environment variables")).
		Help("KEY=value per line. KEY alone copies the variable from this machine.")

	network := d.Field(func(x *Draft) *string { return &x.Network })
	isWG := ggui.Map(network, func(n string) bool { return n == "wg" })
	netSec := ggui.Column(
		ui.Field("Network", ui.ToggleGroup(network).Options([]string{"nat", "none", "wg"}).Format(func(s string) string {
			switch s {
			case "nat":
				return "NAT"
			case "none":
				return "None"
			}
			return "WireGuard"
		}).Outline().Name("Network mode")).Help("NAT relays guest TCP/UDP through host sockets. WireGuard routes everything through a tunnel and needs a config."),
		ggui.If(isWG, func() ggui.Widget {
			wg := str(func(x *Draft) *string { return &x.WGConf })
			return ui.Field("WireGuard config", ggui.Row(
				ggui.Expanded(ui.TextField(wg).Placeholder("~/sandbox-wg.conf").Name("WireGuard config")),
				ui.Button("Browse…", func() { browseFile(m, wg, nil) }).Outline(),
			).Gap(8))
		}),
		ui.Switch(flag(func(x *Draft) *bool { return &x.IsolateLAN }), "Block access to the local network").Small(),
		ui.Switch(flag(func(x *Draft) *bool { return &x.Privileged }), "Privileged inside the sandbox (raw sockets, iptables, setuid) — needed by dockerd").Small(),
		ui.Switch(flag(func(x *Draft) *bool { return &x.HostCACerts }), "Trust this machine's root certificates (TLS-inspecting proxies)").Small(),
		ui.Field("Execution backend", ui.Select(d.Field(func(x *Draft) *string { return &x.Backend })).
			Options([]string{"auto", "hvf", "whpx", "systrap"}).Name("Execution backend")).
			Help("auto picks hvf on macOS, whpx on Windows and systrap on Linux. Use whpx for untrusted code on Windows."),
	).Gap(14).Stretch()

	advancedOpen := ggui.State(false)
	advanced := ggui.Column(
		ggui.Row(
			ggui.Expanded(ui.Field("Hostname", ui.TextField(str(func(x *Draft) *string { return &x.Hostname })).Placeholder("gvx").Name("Hostname"))),
			ggui.Expanded(ui.Field("User", ui.TextField(str(func(x *Draft) *string { return &x.User })).Placeholder("Image default").Name("User"))),
		).Gap(12),
		ggui.Row(
			ggui.Expanded(ui.Field("Working directory", ui.TextField(str(func(x *Draft) *string { return &x.Workdir })).Placeholder("Image default").Name("Working directory"))),
			ggui.Expanded(ui.Field("Pull policy", ui.Select(d.Field(func(x *Draft) *string { return &x.Pull })).Options([]string{"missing", "always", "never"}).Name("Pull policy"))),
		).Gap(12),
		ui.Field("Entrypoint", ui.TextField(str(func(x *Draft) *string { return &x.Entrypoint })).Placeholder("Image default").Name("Entrypoint")),
		ui.Switch(flag(func(x *Draft) *bool { return &x.SetEntrypoint }), "Override the entrypoint even when empty (clears it)").Small(),
		ui.Field("Ready when the log shows", ui.TextField(str(func(x *Draft) *string { return &x.ReadyPattern })).Placeholder("Ready as soon as it starts").Name("Ready pattern")).
			Help("The sandbox shows Starting until a line contains this text."),
		ui.Field("Extra rungv flags", ui.TextField(str(func(x *Draft) *string { return &x.ExtraArgs })).Placeholder("--tune net.nat.mtu=9000").Name("Extra flags")),
	).Gap(14).Stretch()

	lifecycle := ggui.Column(
		ui.Switch(flag(func(x *Draft) *bool { return &x.Checkpoint }), "Allow snapshots (checkpoint and restore)"),
		ui.Switch(flag(func(x *Draft) *bool { return &x.AutoStart }), "Start when gvx Desktop starts"),
	).Gap(10).Stretch()

	section := func(icon *icons.Glyph, title string, body ggui.Widget) ggui.Widget {
		return ggui.Column(
			ggui.Row(glyph(icon, 16).Color(colorBlue), ui.Heading(title).Size(14)).Gap(8),
			body,
		).Gap(12).Stretch()
	}

	errorBanner := ggui.View(m.DraftError, func(msg string) ggui.Widget {
		if msg == "" {
			return ggui.Box()
		}
		return ui.Alert("Needs attention", msg).Destructive().Icon(icons.Alert)
	})

	form := ggui.Column(
		section(lucide.Package, "Image & command", general),
		ui.Divider(),
		section(lucide.Plug, "Ports", ports),
		ui.Divider(),
		section(lucide.HardDrive, "Storage", storage),
		ui.Divider(),
		section(lucide.Cpu, "Resources", resources),
		ui.Divider(),
		section(lucide.SlidersHorizontal, "Environment", env),
		ui.Divider(),
		section(lucide.ShieldCheck, "Network & security", netSec),
		ui.Divider(),
		section(lucide.Camera, "Lifecycle", lifecycle),
		ui.Collapsible(advancedOpen, "Advanced", ggui.Padding(advanced, 12, 0, 0, 0)),
		ui.Divider(),
		ui.CodeBlockOf(m.DraftLine).Header("This runs").Language("sh").Small().CopyButton(true),
		errorBanner,
	).Gap(18).Stretch()

	invalid := ggui.Map(m.DraftError, func(s string) bool { return s != "" })
	// The sheet stacks its own footer under the content; the form scrolls
	// in the room it has and the actions stay at the bottom edge.
	actions := ggui.Row(
		ui.Button("Cancel", func() { m.EditorOpen.Set(false) }).Ghost(),
		ggui.Spacer(),
		ui.Button("Save", func() { m.SaveDraft(false) }).Outline().BindDisabled(invalid),
		ui.Button("Save and start", func() { m.SaveDraft(true) }).BindDisabled(invalid).Icon(glyph(lucide.Play, 14)),
	).Gap(8).Align(ggui.AlignCenter)
	return ui.Sheet(m.EditorOpen, ggui.Column(
		ui.TitleOf(title).Size(20),
		ui.Caption("Ports and mounts are opened before the guest starts and stay fixed while it runs."),
		ggui.Expanded(ggui.Scroll(ggui.Padding(form, 16, 12, 24, 0)).FadeEdges(12)),
		ui.Divider(),
		ggui.Padding(actions, 12, 0, 0, 0),
	).Gap(6).Stretch()).Right().Size(600).Name("Sandbox editor")
}

func sandboxName(ref string) string {
	n := ref
	for _, sep := range []string{"@", ":"} {
		if i := lastIndex(n, sep); i > 0 && !containsSlashAfter(n, i) {
			n = n[:i]
		}
	}
	if i := lastIndex(n, "/"); i >= 0 {
		n = n[i+1:]
	}
	return n
}

func lastIndex(s, sub string) int {
	for i := len(s) - len(sub); i >= 0; i-- {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}

func containsSlashAfter(s string, i int) bool {
	for _, r := range s[i:] {
		if r == '/' {
			return true
		}
	}
	return false
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// browseFolder asks for a folder and writes it to target.
func browseFolder(m *Model, target ggui.Binding[string], then func(string)) {
	host := ggui.UseHost()
	start := rungv.ExpandHome(ggui.Peek[string](target))
	ggui.Async(func() (string, error) {
		return host.Dialogs().PickFolder(runtime.FileDialog{Title: "Choose a folder", Directory: start})
	}, func(path string, err error) {
		if err != nil || path == "" {
			return
		}
		target.Set(path)
		if then != nil {
			then(path)
		}
	})
}

// browseFile asks for a file and writes it to target.
func browseFile(m *Model, target ggui.Binding[string], then func(string)) {
	host := ggui.UseHost()
	ggui.Async(func() (string, error) {
		return host.Dialogs().OpenFile(runtime.FileDialog{Title: "Choose a file"})
	}, func(path string, err error) {
		if err != nil || path == "" {
			return
		}
		target.Set(path)
		if then != nil {
			then(path)
		}
	})
}
