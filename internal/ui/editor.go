package ui

import (
	"image/color"
	"slices"

	"github.com/ironpark/ggui"
	"github.com/ironpark/ggui/icons/lucide"
	"github.com/ironpark/ggui/runtime"
	"github.com/ironpark/ggui/ui"
	"github.com/ironpark/ggui/ui/icons"
	"github.com/ironpark/ggui/widget"

	"github.com/ironpark/gvx-desktop/internal/rungv"
	"github.com/ironpark/gvx-desktop/internal/sandbox"
)

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
			return tr("New sandbox")
		}
		return tr("Edit %s", x.Name)
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
	}).WithEqual(slices.Equal[[]string])
	imageChips := ggui.View(cached, func(refs []string) ggui.Widget {
		if len(refs) == 0 {
			return widget.Box()
		}
		var chips []ggui.Widget
		for _, ref := range refs {
			chips = append(chips, ui.Badge(ref).Outline().OnTap(func() {
				d.Update(func(x Draft) Draft {
					x.Image = ref
					if x.Name == "" {
						x.Name = uniqueName(ggui.Peek(m.Specs), sandbox.SuggestName(ref))
					}
					return x
				})
			}))
		}
		return widget.Column(ui.Caption(tr("Cached images")).Size(11), widget.Wrap(chips...).Gap(6)).Gap(6)
	})

	general := widget.Column(
		ui.Field(tr("Image"), ui.TextField(str(func(x *Draft) *string { return &x.Image })).Placeholder("nginx:alpine").Name(tr("Image"))).
			Help(tr("Any OCI image for linux/%s. A missing image is pulled at start.", goarch())),
		imageChips,
		ui.Field(tr("Name"), ui.TextField(str(func(x *Draft) *string { return &x.Name })).Placeholder("web").Name(tr("Sandbox name"))),
		ui.Field(tr("Command"), ui.TextField(str(func(x *Draft) *string { return &x.Command })).Placeholder(tr("Image default")).Name(tr("Command"))).
			Help(tr("Replaces the image's CMD and keeps its ENTRYPOINT. Quote words as in a shell.")),
	).Gap(14).Stretch()

	ports := widget.Column(
		ui.Field(tr("Published ports"), ui.TextField(str(func(x *Draft) *string { return &x.Ports })).Multiline().Lines(3).
			Placeholder("8080:80\n127.0.0.1:9000-9019:9000-9019\nunix:~/.gvx/app.sock:3000").Name(tr("Published ports"))).
			Help(tr("One per line: [IP:][HOST:]GUEST[/udp]. Bound before the guest starts and fixed for the run: publish every port the guest will use, including ports of nested containers.")),
	).Gap(14).Stretch()

	workspace := str(func(x *Draft) *string { return &x.Workspace })
	storage := widget.Column(
		ui.Field(tr("Volumes"), ui.TextField(str(func(x *Draft) *string { return &x.Volumes })).Multiline().Lines(2).
			Placeholder("pgdata:/var/lib/postgresql/data\n/disk/cache.ext4:/cache").Name(tr("Volumes"))).
			Help(tr("NAME:/guest/path[:ro]. Each named volume is one journaled ext4 image that persists; a source with a slash is an .ext4 file at that path.")),
		ui.Field(tr("Bind mounts"), ui.TextField(str(func(x *Draft) *string { return &x.Binds })).Multiline().Lines(2).
			Placeholder("~/src:/src\n~/app.conf:/etc/app.conf:ro").Name(tr("Bind mounts"))).
			Help(tr("HOST:/guest/path[:ro]. A host directory or one file. The sandbox may only reach what is mounted.")),
		ui.Field(tr("Workspace"), widget.Row(
			widget.Expanded(ui.TextField(workspace).Placeholder(tr("Folder mounted at /workspace")).Name(tr("Workspace folder"))),
			ui.Button(tr("Browse…"), func() { browseFolder(m, workspace, nil) }).Outline(),
		).Gap(8)),
		ui.Switch(flag(func(x *Draft) *bool { return &x.WorkspaceRO }), tr("Mount the workspace read-only")).Small(),
		ui.Field(tr("Persistent root file system"), ui.TextField(str(func(x *Draft) *string { return &x.RootfsVolume })).Placeholder(tr("Volume name, or empty for memory")).Name(tr("Root file system volume"))).
			Help(tr("Writes outside mounts go to memory and vanish at exit unless a root volume keeps them.")),
		ui.Field(tr("Memory mounts (tmpfs)"), ui.TextField(str(func(x *Draft) *string { return &x.Tmpfs })).Multiline().Lines(1).
			Placeholder("/tmp:size=256m").Name(tr("tmpfs mounts"))),
	).Gap(14).Stretch()

	resources := widget.Row(
		widget.Expanded(ui.Field(tr("Memory limit"), ui.TextField(str(func(x *Draft) *string { return &x.Memory })).Placeholder(tr("Host RAM")).Name(tr("Memory limit"))).Help(tr("512m, 2g. The guest is killed (exit 137) when it passes the limit."))),
		widget.Expanded(ui.Field(tr("CPU limit"), ui.TextField(str(func(x *Draft) *string { return &x.CPUs })).Placeholder(tr("All CPUs")).Name(tr("CPU limit"))).Help(tr("1.5 CPUs. A hard cap on Windows only."))),
	).Gap(12).Align(widget.AlignStart)

	env := ui.Field(tr("Environment"), ui.TextField(str(func(x *Draft) *string { return &x.Env })).Multiline().Lines(3).
		Placeholder("KEY=value\nHTTP_PROXY").Name(tr("Environment variables"))).
		Help(tr("KEY=value per line. KEY alone copies the variable from this machine."))

	network := d.Field(func(x *Draft) *string { return &x.Network })
	isWG := ggui.Map(network, func(n string) bool { return n == "wg" })
	netSec := widget.Column(
		ui.Field(tr("Network"), ui.ToggleGroup(network).Options([]string{"nat", "none", "wg"}).Format(func(s string) string {
			switch s {
			case "nat":
				return "NAT"
			case "none":
				return tr("None")
			}
			return "WireGuard"
		}).Outline().Name(tr("Network mode"))).Help(tr("NAT relays guest TCP/UDP through host sockets. WireGuard routes everything through a tunnel and needs a config.")),
		ggui.If(isWG, func() ggui.Widget {
			wg := str(func(x *Draft) *string { return &x.WGConf })
			return ui.Field(tr("WireGuard config"), widget.Row(
				widget.Expanded(ui.TextField(wg).Placeholder("~/sandbox-wg.conf").Name(tr("WireGuard config"))),
				ui.Button(tr("Browse…"), func() { browseFile(m, wg, nil) }).Outline(),
			).Gap(8))
		}),
		ui.Switch(flag(func(x *Draft) *bool { return &x.IsolateLAN }), tr("Block access to the local network")).Small(),
		ui.Switch(flag(func(x *Draft) *bool { return &x.Privileged }), tr("Privileged inside the sandbox (raw sockets, iptables, setuid) — needed by dockerd")).Small(),
		ui.Switch(flag(func(x *Draft) *bool { return &x.HostCACerts }), tr("Trust this machine's root certificates (TLS-inspecting proxies)")).Small(),
		ui.Field(tr("Execution backend"), ui.Select(d.Field(func(x *Draft) *string { return &x.Backend })).
			Options([]string{"auto", "hvf", "whpx", "systrap"}).Name(tr("Execution backend"))).
			Help(tr("auto picks hvf on macOS, whpx on Windows and systrap on Linux. Use whpx for untrusted code on Windows.")),
	).Gap(14).Stretch()

	advancedOpen := ggui.State(false)
	advanced := widget.Column(
		widget.Row(
			widget.Expanded(ui.Field(tr("Hostname"), ui.TextField(str(func(x *Draft) *string { return &x.Hostname })).Placeholder("gvx").Name(tr("Hostname")))),
			widget.Expanded(ui.Field(tr("User"), ui.TextField(str(func(x *Draft) *string { return &x.User })).Placeholder(tr("Image default")).Name(tr("User")))),
		).Gap(12),
		widget.Row(
			widget.Expanded(ui.Field(tr("Working directory"), ui.TextField(str(func(x *Draft) *string { return &x.Workdir })).Placeholder(tr("Image default")).Name(tr("Working directory")))),
			widget.Expanded(ui.Field(tr("Pull policy"), ui.Select(d.Field(func(x *Draft) *string { return &x.Pull })).Options([]string{"missing", "always", "never"}).Name(tr("Pull policy")))),
		).Gap(12),
		ui.Field(tr("Entrypoint"), ui.TextField(str(func(x *Draft) *string { return &x.Entrypoint })).Placeholder(tr("Image default")).Name(tr("Entrypoint"))),
		ui.Switch(flag(func(x *Draft) *bool { return &x.SetEntrypoint }), tr("Override the entrypoint even when empty (clears it)")).Small(),
		ui.Field(tr("Ready when the log shows"), ui.TextField(str(func(x *Draft) *string { return &x.ReadyPattern })).Placeholder(tr("Ready as soon as it starts")).Name(tr("Ready pattern"))).
			Help(tr("The sandbox shows Starting until a line contains this text.")),
		ui.Field(tr("Extra rungv flags"), ui.TextField(str(func(x *Draft) *string { return &x.ExtraArgs })).Placeholder("--tune net.nat.mtu=9000").Name(tr("Extra flags"))),
	).Gap(14).Stretch()

	lifecycle := widget.Column(
		ui.Switch(flag(func(x *Draft) *bool { return &x.Checkpoint }), tr("Allow snapshots (checkpoint and restore)")),
		ui.Switch(flag(func(x *Draft) *bool { return &x.AutoStart }), tr("Start when GVX Desktop starts")),
	).Gap(10).Stretch()

	section := func(icon *icons.Glyph, title string, body ggui.Widget) ggui.Widget {
		return widget.Column(
			widget.Row(glyph(icon, 16).Color(colorBlue), ui.Heading(title).Size(14)).Gap(8),
			body,
		).Gap(12).Stretch()
	}

	errorBanner := ggui.View(m.DraftError, func(msg string) ggui.Widget {
		if msg == "" {
			return widget.Box()
		}
		return widget.Padding(ui.Alert(tr("Needs attention"), msg).Destructive().Icon(icons.Alert), 6, 0, 2, 0)
	})

	form := widget.Column(
		section(lucide.Package, tr("Image & command"), general),
		ui.Divider(),
		section(lucide.Plug, tr("Ports"), ports),
		ui.Divider(),
		section(lucide.HardDrive, tr("Storage"), storage),
		ui.Divider(),
		section(lucide.Cpu, tr("Resources"), resources),
		ui.Divider(),
		section(lucide.SlidersHorizontal, tr("Environment"), env),
		ui.Divider(),
		section(lucide.ShieldCheck, tr("Network & security"), netSec),
		ui.Divider(),
		section(lucide.Camera, tr("Lifecycle"), lifecycle),
		ui.Collapsible(advancedOpen, tr("Advanced"), widget.Padding(advanced, 12, 0, 0, 0)),
		ui.Divider(),
		ui.CodeBlockOf(m.DraftLine).Header(tr("This runs")).Language("sh").Small().CopyButton(true),
	).Gap(18).Stretch()

	invalid := ggui.Map(m.DraftError, func(s string) bool { return s != "" })
	primaryAction := ggui.View(m.DraftRunning, func(running bool) ggui.Widget {
		if running {
			return ui.Button(tr("Save and restart"), func() { m.SaveDraft(true) }).BindDisabled(invalid).Icon(glyph(lucide.RotateCcw, 14))
		}
		return ui.Button(tr("Save and start"), func() { m.SaveDraft(true) }).BindDisabled(invalid).Icon(glyph(lucide.Play, 14))
	})
	// The sheet stacks its own footer under the content; the form scrolls
	// in the room it has and the validation status + actions stay pinned
	// at the bottom edge.
	actions := widget.Row(
		ui.Button(tr("Cancel"), func() { m.EditorOpen.Set(false) }).Ghost(),
		widget.Spacer(),
		ui.Button(tr("Save"), func() { m.SaveDraft(false) }).Outline().BindDisabled(invalid),
		primaryAction,
	).Gap(8).Align(widget.AlignCenter)
	return ui.Sheet(m.EditorOpen, widget.Column(
		ui.TitleOf(title).Size(20),
		ui.Caption(tr("Ports and mounts are opened before the guest starts and stay fixed while it runs.")),
		widget.Expanded(widget.Scroll(widget.Padding(form, 16, 12, 24, 0)).FadeEdges(12)),
		errorBanner,
		ui.Divider(),
		widget.Padding(actions, 12, 0, 0, 0),
	).Gap(6).Stretch()).Right().Size(600).Name(tr("Sandbox editor"))
}

// browseFolder asks for a folder and writes it to target.
func browseFolder(m *Model, target ggui.Binding[string], then func(string)) {
	host := ggui.UseHost()
	start := rungv.ExpandHome(ggui.Peek[string](target))
	ggui.Async(func() (string, error) {
		return host.Dialogs().PickFolder(runtime.FileDialog{Title: tr("Choose a folder"), Directory: start})
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
		return host.Dialogs().OpenFile(runtime.FileDialog{Title: tr("Choose a file")})
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
