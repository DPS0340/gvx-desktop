package ui

import (
	"runtime"
	"strconv"
	"strings"

	"github.com/ironpark/ggui"
	"github.com/ironpark/ggui/icons/lucide"
	"github.com/ironpark/ggui/ui"
	"github.com/ironpark/ggui/ui/icons"
	"github.com/ironpark/ggui/widget"

	"github.com/ironpark/gvx-desktop/internal/rungv"
)

// settingRow is one setting: what it is and what it does at the left, its
// control at the right.
func settingRow(title, help string, control ggui.Widget) ggui.Widget {
	text := []ggui.Widget{widget.Text(title).Size(14).Weight(ggui.WeightMedium)}
	if help != "" {
		text = append(text, ui.Caption(help))
	}
	return widget.Row(widget.Expanded(widget.Column(text...).Gap(2)), control).Gap(24).Align(widget.AlignCenter)
}

// settingField is a setting whose control wants the width of the card.
func settingField(title, help string, control ggui.Widget) ggui.Widget {
	return widget.Column(
		widget.Text(title).Size(14).Weight(ggui.WeightMedium),
		control,
		ui.Caption(help),
	).Gap(6).Stretch()
}

// settingsGroup is a titled card of settings with a rule between each two.
func settingsGroup(icon *icons.Glyph, title string, rows ...ggui.Widget) ggui.Widget {
	body := make([]ggui.Widget, 0, 2*len(rows))
	for i, r := range rows {
		if i > 0 {
			body = append(body, ui.Divider())
		}
		body = append(body, r)
	}
	return widget.Column(
		widget.Row(glyph(icon, 15).Color(colorGray), ui.Heading(title).Size(14)).Gap(8).Align(widget.AlignCenter),
		ui.Card(widget.Column(body...).Gap(14).Stretch()),
	).Gap(10).Stretch()
}

// trayPlace is where the system keeps the app's tray icon.
func trayPlace() string {
	if runtime.GOOS == "darwin" {
		return tr("menu bar")
	}
	return tr("notification area")
}

func settingsPage(m *Model) ggui.Widget {
	set := ggui.Peek(m.Settings)
	path := ggui.State(set.RungvPath)
	cache := ggui.State(set.CacheDir)
	flag := func(get func(storeSettings) bool, put func(*storeSettings, bool)) ggui.Binding[bool] {
		return ggui.Bind(func() bool { return get(m.Settings.Get()) }, func(v bool) { m.UpdateSettings(func(s *storeSettings) { put(s, v) }) })
	}
	theme := ggui.Bind(func() string { return m.Settings.Get().Theme }, func(v string) { m.UpdateSettings(func(s *storeSettings) { s.Theme = v }) })
	language := ggui.Bind(func() string {
		if l := m.Settings.Get().Language; l == LangEnglish || l == LangKorean {
			return l
		}
		return LangSystem
	}, func(v string) { m.UpdateSettings(func(s *storeSettings) { s.Language = v }) })
	tray := flag(func(s storeSettings) bool { return !s.NoTray }, func(s *storeSettings, v bool) { s.NoTray = !v })
	hideOnClose := flag(func(s storeSettings) bool { return s.HideOnClose }, func(s *storeSettings, v bool) { s.HideOnClose = v })
	stopOnQuit := flag(func(s storeSettings) bool { return s.StopOnQuit }, func(s *storeSettings, v bool) { s.StopOnQuit = v })
	checkUpdates := flag(func(s storeSettings) bool { return !s.NoUpdateCheck }, func(s *storeSettings, v bool) { s.NoUpdateCheck = !v })
	timeout := ggui.Bind(func() float64 { return float64(m.Settings.Get().StopTimeout) }, func(v float64) {
		m.UpdateSettings(func(s *storeSettings) { s.StopTimeout = max(1, int(v)) })
	})
	channel := ggui.Bind(func() string {
		if m.Settings.Get().UpdateChannel == ChannelSource {
			return ChannelSource
		}
		return ChannelRelease
	}, m.SetChannel)
	noTray := ggui.Map(m.Settings, func(s storeSettings) bool { return s.NoTray })

	groups := []ggui.Widget{
		headed(pageHeader(tr("Settings"), ui.Caption(tr("Where rungv lives and how the app treats running sandboxes."))), engineAlert(m)),
		settingsGroup(lucide.Palette, tr("General"),
			settingRow(tr("Appearance"), tr("Light, dark, or as the system is."),
				ui.ToggleGroup[string](theme).Options([]string{"system", "light", "dark"}).Format(themeName).Icon(themeIcon).Outline().Spacing(0).Name(tr("Appearance"))),
			settingRow(tr("Language"), tr("The language the app is worded in."),
				ui.ToggleGroup[string](language).Options([]string{LangSystem, LangEnglish, LangKorean}).Format(languageName).Outline().Spacing(0).Name(tr("Language"))),
			settingRow(tr("Start at login"),
				tr("Starts the app when you log in, in the tray without its window when the tray icon is shown. Sandboxes set to start with the app start then."),
				ui.Switch(ggui.Bind(m.Autostart.Get, m.SetAutostart), "")),
		),
	}
	if runtime.GOOS == "darwin" || runtime.GOOS == "windows" {
		groups = append(groups, settingsGroup(lucide.PanelTop, tr("Tray"),
			settingRow(tr("Show gvx Desktop in the %s", trayPlace()),
				tr("An icon with the sandboxes and what they are doing, to start and stop them without the window."),
				ui.Switch(tray, "")),
			settingRow(tr("Keep running when the window closes"),
				tr("Closing the window leaves the app and its sandboxes running behind the icon. Without the icon, closing the window quits."),
				ui.Switch(hideOnClose, "").BindDisabled(noTray)),
		))
	}
	groups = append(groups,
		settingsGroup(lucide.Box, tr("Sandboxes"),
			settingRow(tr("Stop running sandboxes on quit without asking"),
				tr("gvx has no daemon: sandboxes run as long as this app does."),
				ui.Switch(stopOnQuit, "")),
			settingRow(tr("Stop timeout (seconds)"),
				tr("How long a sandbox gets to exit after an interrupt before it is killed."),
				widget.Box(ui.NumberInput(timeout).Name(tr("Stop timeout"))).Width(120)),
		),
		dockerSettings(m),
		settingsGroup(lucide.Cpu, tr("Engine"),
			settingField(tr("rungv path"), tr("Leave empty to search $GVX_RUNGV, PATH, ~/.gvx/bin and the app's own folder."), widget.Row(
				widget.Expanded(ui.TextField(path).Placeholder(tr("Find automatically (PATH, ~/.gvx/bin, next to the app)")).Name(tr("rungv path")).
					OnCommit(func(v string) { m.UpdateSettings(func(s *storeSettings) { s.RungvPath = strings.TrimSpace(v) }) })),
				ui.Button(tr("Browse…"), func() {
					browseFile(m, path, func(v string) { m.UpdateSettings(func(s *storeSettings) { s.RungvPath = v }) })
				}).Outline(),
			).Gap(8)),
			settingField(tr("Layer cache directory"), tr("Passed as -cache-dir. Named volumes live in a volumes folder beside it."), widget.Row(
				widget.Expanded(ui.TextField(cache).Placeholder(tr("rungv's default")).Name(tr("Layer cache directory")).
					OnCommit(func(v string) { m.UpdateSettings(func(s *storeSettings) { s.CacheDir = strings.TrimSpace(v) }) })),
				ui.Button(tr("Browse…"), func() {
					browseFolder(m, cache, func(v string) { m.UpdateSettings(func(s *storeSettings) { s.CacheDir = v }) })
				}).Outline(),
			).Gap(8)),
			engineFacts(m),
		),
		settingsGroup(lucide.Download, tr("Updates"),
			settingRow(tr("Channel"),
				tr("Releases are gvx's published builds. main is its newest commit, cloned and built on this computer, which needs git and Go."),
				ui.ToggleGroup[string](channel).Options([]string{ChannelRelease, ChannelSource}).Format(channelName).Outline().Spacing(0).
					BindDisabled(ggui.Map(m.Update, func(u Update) bool { return u.Installing })).Name(tr("Channel"))),
			releaseRow(m),
			settingRow(tr("Look for a newer rungv when the app starts"),
				tr("Asks GitHub for the newest release of gvx. Nothing is installed until you say so."),
				ui.Switch(checkUpdates, "")),
		),
		aboutCard(m),
	)
	return page(groups...)
}

// engineFacts is what the rungv in use says of itself, with the buttons
// that look again and sign it.
func engineFacts(m *Model) ggui.Widget {
	return ggui.View(m.Engine, func(e Engine) ggui.Widget {
		again := ui.Button(tr("Check again"), func() { m.DetectEngine(nil) }).Outline().Size(ui.ButtonSmall).Icon(glyph(lucide.RefreshCw, 14))
		if !e.Checked || e.Path == "" {
			if e.Err != "" {
				return widget.Column(ui.Alert(tr("Engine unavailable"), e.Err).Destructive().Icon(icons.Alert), widget.Row(again)).Gap(10).Stretch()
			}
			return widget.Row(again)
		}
		actions := []ggui.Widget{again}
		if e.Info.OS == "darwin" {
			actions = append(actions, tip(ui.Button(tr("Sign rungv"), m.SignRungv).Outline().Size(ui.ButtonSmall),
				tr("Ad-hoc signs rungv with the com.apple.security.hypervisor entitlement for the hvf engine.")))
		}
		return widget.Column(
			kvText(tr("Binary"), e.Path),
			kvText(tr("Version"), strings.TrimSpace(e.Version.Version+" "+e.Version.Commit)),
			kvText(tr("Built with"), e.Version.Go),
			kvText(tr("Backends"), tr("%s (default %s)", strings.Join(e.Info.AvailableBackends, ", "), e.Info.DefaultBackend)),
			kvText(tr("Host lockdown"), e.Info.HostLockdown),
			kvText(tr("Layer cache"), e.Info.LayerCacheDir),
			kvText(tr("Volumes"), e.Info.VolumeDir),
			widget.Padding(widget.Row(actions...).Gap(8), 6, 0, 0, 0),
		).Gap(8).Stretch()
	})
}

// aboutCard names the app and its version, and where to read more.
func aboutCard(m *Model) ggui.Widget {
	return ui.Card(widget.Row(
		widget.Image(brandMark()).Size(56, 56),
		widget.Expanded(widget.Column(
			widget.Row(ui.Heading("gvx Desktop").Size(16), ui.Badge(AppVersion).Outline()).Gap(8).Align(widget.AlignCenter),
			ui.Caption(tr("gvx runs unmodified Linux containers on gVisor's user-space kernel, ported to macOS, Windows and Linux.")),
			widget.Padding(widget.Row(
				ui.Button(tr("gvx on GitHub"), func() { m.Open("https://github.com/gfire-sigs/gvx") }).Outline().Size(ui.ButtonSmall).Icon(glyph(lucide.ExternalLink, 14)),
				ui.Button(tr("Releases"), func() { m.Open(rungv.ReleasesURL) }).Outline().Size(ui.ButtonSmall),
				ui.Button(tr("App data folder"), func() { m.Open(m.store.Dir) }).Ghost().Size(ui.ButtonSmall),
			).Gap(8), 6, 0, 0, 0),
		).Gap(4)),
	).Gap(16).Align(widget.AlignCenter))
}

// releaseRow says what the channel's newest gvx is, and installs it.
func releaseRow(m *Model) ggui.Widget {
	type state struct {
		e Engine
		u Update
	}
	both := ggui.Combine(m.Engine, m.Update, func(e Engine, u Update) state { return state{e, u} })
	row := ggui.View(both, func(s state) ggui.Widget {
		e, u := s.e, s.u
		var text string
		var action ggui.Widget = widget.Box()
		install := tr("Install rungv")
		if u.Source {
			install = tr("Build and install")
		}
		switch {
		case u.Installing && u.Source:
			text = tr("Building rungv from main")
			action = installButton(m, u, install).Size(ui.ButtonSmall)
		case u.Installing:
			text = tr("Installing rungv %s", u.Name())
			action = installButton(m, u, install).Size(ui.ButtonSmall)
		case u.Checking:
			text = tr("Asking GitHub for the newest…")
		case !u.Known() && u.Err != "":
			text = tr("The newest could not be read: %s", u.Err)
		case !u.Known():
			text = tr("The newest has not been looked up.")
		case e.Path == "":
			text = tr("rungv %s can be installed into ~/.gvx/bin.", u.Name())
			action = installButton(m, u, install).Size(ui.ButtonSmall)
		case u.Available(e):
			switch {
			case u.Source && u.Ahead > 0:
				text = tr("main is %d commits past this computer's %s.", u.Ahead, engineVersion(e))
			case u.Source:
				text = tr("main is at %s; this computer has %s.", u.Name(), engineVersion(e))
			case u.Newer(e):
				text = tr("rungv %s is available; this computer has %s.", u.Name(), engineVersion(e))
			default:
				text = tr("The newest release is rungv %s; this computer has %s.", u.Name(), engineVersion(e))
			}
			action = installButton(m, u, updateVerb(u, e)).Size(ui.ButtonSmall)
		case u.Source:
			text = tr("rungv is up to date with main (%s).", u.Name())
			action = ui.Button(tr("Build again"), m.InstallRungv).Outline().Size(ui.ButtonSmall)
		default:
			text = tr("rungv is up to date: %s is the newest release.", u.Name())
		}
		return widget.Row(
			action,
			ui.Button(tr("Check for updates"), func() { m.CheckUpdate(false) }).Outline().Size(ui.ButtonSmall).
				Disabled(u.Checking || u.Installing),
			widget.Expanded(ui.Caption(text)),
		).Gap(10).Align(widget.AlignCenter)
	})
	// What a build printed stays while it runs, and after it failed.
	log := ggui.If(ggui.Map(m.BuildLog, func(s string) bool { return s != "" }), func() ggui.Widget {
		return ui.CodeBlockOf(m.BuildLog).Height(160).Small().CopyButton(true)
	})
	return widget.Column(row, log).Gap(10).Stretch()
}

// channelName words a channel.
func channelName(c string) string {
	if c == ChannelSource {
		return tr("main, built here")
	}
	return tr("Releases")
}

// themeIcon pictures an appearance: a screen for the system's, the sun
// and the moon for light and dark.
func themeIcon(theme string) *icons.Widget {
	switch theme {
	case "light":
		return glyph(lucide.Sun, 14)
	case "dark":
		return glyph(lucide.Moon, 14)
	}
	return glyph(lucide.Monitor, 14)
}

// dockerSettings chooses the docker command the app runs and how the
// Docker Engine recipe publishes its API.
func dockerSettings(m *Model) ggui.Widget {
	path := ggui.State(ggui.Peek(m.Settings).DockerPath)
	setPath := func(v string) { m.UpdateSettings(func(s *storeSettings) { s.DockerPath = strings.TrimSpace(v) }) }
	found := ui.CaptionOf(ggui.Map(m.Settings, func(s storeSettings) string {
		p, ok := m.DockerCLI()
		switch {
		case ok:
			return tr("Using %s.", tildePath(p))
		case strings.TrimSpace(s.DockerPath) != "":
			return tr("Nothing is at that path.")
		}
		return tr("docker was not found. Install the Docker CLI alone; no local engine is needed.")
	}))

	endpoint := ggui.Bind(func() string {
		m.Settings.Get() // follows the setting; the sandbox's own port is what counts
		m.Rows.Get()
		if m.DockerAPI().TCP {
			return DockerTCPEndpoint
		}
		return DockerSocketEndpoint
	}, func(v string) {
		api := m.DockerAPI()
		api.TCP = v == DockerTCPEndpoint
		m.SetDockerAPI(api)
	})
	port := ggui.State(strconv.Itoa(m.DockerAPI().Port))
	setPort := func(v string) {
		n, err := strconv.Atoi(strings.TrimSpace(v))
		if err != nil || n < 1 || n > 65535 {
			port.Set(strconv.Itoa(m.DockerAPI().Port)) // not a port: put back what holds
			return
		}
		api := m.DockerAPI()
		api.Port = n
		m.SetDockerAPI(api)
	}
	overTCP := ggui.Map(endpoint, func(v string) bool { return v == DockerTCPEndpoint })
	host := ui.CaptionOf(ggui.Map(endpoint, func(string) string { return tr("DOCKER_HOST is %s.", m.DockerHost()) }))

	return settingsGroup(lucide.Container, "Docker",
		settingField(tr("docker path"), tr("The docker command the app sets a context up with. Leave empty to look where a terminal would."), widget.Column(
			widget.Row(
				widget.Expanded(ui.TextField(path).Placeholder(tr("Find automatically (PATH, Homebrew, Docker Desktop)")).Name(tr("docker path")).OnCommit(setPath)),
				ui.Button(tr("Browse…"), func() { browseFile(m, path, setPath) }).Outline(),
			).Gap(8),
			found,
		).Gap(6).Stretch()),
		widget.Column(
			settingRow(tr("Engine API"),
				tr("How the Docker Engine recipe is reached. A running engine moves when it is restarted."),
				widget.Row(
					ggui.If(overTCP, func() ggui.Widget {
						return widget.Box(ui.TextField(port).Name(tr("Docker API port")).OnCommit(setPort)).Width(84)
					}),
					ui.ToggleGroup(endpoint).Options([]string{DockerSocketEndpoint, DockerTCPEndpoint}).Format(dockerEndpointName).Outline().Spacing(0).Name(tr("Engine API")),
				).Gap(8).Align(widget.AlignCenter)),
			ui.Caption(tr("A Docker CLI on Windows reaches a Unix socket only from version 29 on. TCP on this computer's loopback address works with any version, on every system.")),
			host,
		).Gap(6).Stretch(),
	)
}

// dockerEndpointName words an endpoint of the Docker Engine's API.
func dockerEndpointName(v string) string {
	if v == DockerTCPEndpoint {
		return "TCP"
	}
	return tr("Unix socket")
}
