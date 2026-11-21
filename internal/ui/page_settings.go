package ui

import (
	"runtime"
	"slices"
	"strconv"
	"strings"

	"github.com/ironpark/ggui"
	"github.com/ironpark/ggui/icons/lucide"
	"github.com/ironpark/ggui/ui"
	"github.com/ironpark/ggui/ui/icons"
	"github.com/ironpark/ggui/widget"

	"github.com/ironpark/gvx-desktop/internal/engine"
	"github.com/ironpark/gvx-desktop/internal/recipes"
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
		if l := m.Settings.Get().Language; l != LangSystem {
			if _, ok := languageOf(l); ok {
				return l
			}
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
		headed(pageHeader(tr("Settings"), ui.Caption(tr("Where rungv lives and how the app treats running sandboxes."))), engineAlert(m), appUpdateAlert(m)),
		settingsGroup(lucide.Palette, tr("General"),
			settingRow(tr("Appearance"), tr("Light, dark, or as the system is."),
				ui.ToggleGroup[string](theme).Options([]string{"system", "light", "dark"}).Format(themeName).Icon(themeIcon).Outline().Spacing(0).Name(tr("Appearance"))),
			settingRow(tr("Language"), tr("The language the app is worded in."),
				ui.Select(language).Options(append([]string{LangSystem}, LanguageTags()...)).Format(languageName).MinWidth(180).Name(tr("Language"))),
			settingRow(tr("Start at login"),
				tr("Starts the app when you log in, in the tray without its window when the tray icon is shown. Sandboxes set to start with the app start then."),
				ui.Switch(ggui.Bind(m.Autostart.Get, m.SetAutostart), "")),
		),
		appUpdateSettings(m),
	}
	if runtime.GOOS == "darwin" || runtime.GOOS == "windows" {
		groups = append(groups, settingsGroup(lucide.PanelTop, tr("Tray"),
			settingRow(tr("Show GVX Desktop in the %s", trayPlace()),
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
		kubeSettings(m),
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
		settingsGroup(lucide.Download, tr("rungv updates"),
			widget.Column(
				settingRow(tr("Channel"),
					tr("Releases are gvx's published builds. From source clones a git repository and builds rungv on this computer, which needs git and Go."),
					ui.ToggleGroup[string](channel).Options([]string{ChannelRelease, ChannelSource}).Format(channelName).Outline().Spacing(0).
						BindDisabled(ggui.Map(m.Update, func(u Update) bool { return u.Installing })).Name(tr("Channel"))),
				sourceFields(m),
			).Gap(12).Stretch(),
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
			widget.Row(ui.Heading("GVX Desktop").Size(16), ui.Badge(AppVersion).Outline()).Gap(8).Align(widget.AlignCenter),
			ui.Caption(tr("gvx runs unmodified Linux containers on gVisor's user-space kernel, ported to macOS, Windows and Linux.")),
			widget.Padding(widget.Row(
				ui.Button(tr("gvx on GitHub"), func() { m.Open("https://github.com/gfire-sigs/gvx") }).Outline().Size(ui.ButtonSmall).Icon(glyph(lucide.ExternalLink, 14)),
				ui.Button(tr("Releases"), func() { m.Open(rungv.ReleasesURL) }).Outline().Size(ui.ButtonSmall),
				ui.Button(tr("App data folder"), func() { m.Open(m.store.Dir) }).Ghost().Size(ui.ButtonSmall),
			).Gap(8), 6, 0, 0, 0),
		).Gap(4)),
	).Gap(16).Align(widget.AlignCenter))
}

// sourceFields lets the user choose which git repository and branch are cloned
// and built when the From source channel is selected.
func sourceFields(m *Model) ggui.Widget {
	isSource := ggui.Map(m.Settings, func(s storeSettings) bool { return s.UpdateChannel == ChannelSource })
	return ggui.If(isSource, func() ggui.Widget {
		set := ggui.Peek(m.Settings)
		repo := ggui.State(set.SourceRepo)
		branch := ggui.State(set.SourceBranch)
		ggui.Watch(m.Settings, func(s storeSettings) {
			if ggui.Peek(repo) != s.SourceRepo {
				repo.Set(s.SourceRepo)
			}
			if ggui.Peek(branch) != s.SourceBranch {
				branch.Set(s.SourceBranch)
			}
		})
		installing := ggui.Map(m.Update, func(u Update) bool { return u.Installing })
		custom := ggui.Map(m.Settings, func(s storeSettings) bool { return s.SourceRepo != "" || s.SourceBranch != "" })
		commit := func(string) { m.SetSource(ggui.Peek(repo), ggui.Peek(branch)) }
		reset := ggui.If(custom, func() ggui.Widget {
			return ui.Button(tr("Reset"), func() {
				repo.Set("")
				branch.Set("")
				m.SetSource("", "")
			}).Outline().Size(ui.ButtonSmall).BindDisabled(installing)
		})
		return widget.Column(
			widget.Row(
				widget.Expanded(widget.Column(
					ui.Caption(tr("Repository")).Size(12),
					ui.TextField(repo).Placeholder(rungv.SourceURL).Name(tr("Source repository")).
						OnCommit(commit).BindDisabled(installing),
				).Gap(4).Stretch()),
				widget.Box(widget.Column(
					ui.Caption(tr("Branch")).Size(12),
					ui.TextField(branch).Placeholder(rungv.SourceBranch).Name(tr("Source branch")).
						OnCommit(commit).BindDisabled(installing),
				).Gap(4).Stretch()).Width(180),
				widget.Padding(reset, 18, 0, 0, 0),
			).Gap(8).Align(widget.AlignStart),
			ui.Caption(tr("Git URL, GitHub owner/repo, or a local checkout. Leave empty for gfire-sigs/gvx (main).")),
		).Gap(6).Stretch()
	})
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
		branch := u.BranchName()
		var text string
		var action ggui.Widget = widget.Box()
		install := tr("Install rungv")
		if u.Source {
			install = tr("Build and install")
		}
		switch {
		case u.Installing && u.Source:
			text = tr("Building rungv from %s", branch)
			action = installButton(m, u, install).Size(ui.ButtonSmall)
		case u.Installing:
			text = tr("Installing rungv %s", u.Name())
			action = installButton(m, u, install).Size(ui.ButtonSmall)
		case u.Checking:
			text = tr("Checking for the newest…")
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
				text = tr("%s is %d commits past this computer's %s.", branch, u.Ahead, engineVersion(e))
			case u.Source:
				text = tr("%s is at %s; this computer has %s.", branch, u.Name(), engineVersion(e))
			case u.Newer(e):
				text = tr("rungv %s is available; this computer has %s.", u.Name(), engineVersion(e))
			default:
				text = tr("The newest release is rungv %s; this computer has %s.", u.Name(), engineVersion(e))
			}
			action = installButton(m, u, updateVerb(u, e)).Size(ui.ButtonSmall)
		case u.Source:
			text = tr("rungv is up to date with %s (%s).", branch, u.Name())
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
		return tr("From source")
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

// dockerSettings chooses the docker command the app runs and the endpoint the
// app's Docker API proxy listens on.
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
		m.Settings.Get()
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
	host := ui.CaptionOf(ggui.Map(m.DockerEndpoint, func(h string) string { return tr("DOCKER_HOST is %s.", h) }))
	proxyErr := ggui.View(ggui.Combine(m.DockerEndpoint, m.DockerProxyErr, func(h, err string) [2]string { return [2]string{h, err} }), func(v [2]string) ggui.Widget {
		if v[1] == "" {
			return widget.Box()
		}
		return widget.Row(
			glyph(lucide.TriangleExclamationPoint, 14).Color(colorAmber),
			widget.Expanded(ui.Caption(tr("Could not listen on %s: %s", v[0], v[1]))),
		).Gap(8).Align(widget.AlignStart)
	})

	return settingsGroup(lucide.Container, "Docker",
		settingField(tr("docker path"), tr("The docker command the app sets a context up with. Leave empty to look where a terminal would."), widget.Column(
			widget.Row(
				widget.Expanded(ui.TextField(path).Placeholder(tr("Find automatically (PATH, Homebrew, Docker Desktop)")).Name(tr("docker path")).OnCommit(setPath)),
				ui.Button(tr("Browse…"), func() { browseFile(m, path, setPath) }).Outline(),
			).Gap(8),
			found,
		).Gap(6).Stretch()),
		widget.Column(
			widget.Row(
				widget.Expanded(widget.Column(
					widget.Text(tr("Engine API")).Size(14).Weight(ggui.WeightMedium),
					ui.CaptionOf(ggui.Map(m.Settings, func(s storeSettings) string {
						if s.SandboxEngine {
							return tr("How docker reaches GVX Desktop's sandbox engine. Changes take effect immediately.")
						}
						return tr("How the Docker Engine is reached. Changes take effect immediately.")
					})),
				).Gap(2)),
				widget.Row(
					ggui.If(overTCP, func() ggui.Widget {
						return widget.Box(ui.TextField(port).Name(tr("Docker API port")).OnCommit(setPort)).Width(84)
					}),
					ui.ToggleGroup(endpoint).Options([]string{DockerSocketEndpoint, DockerTCPEndpoint}).Format(dockerEndpointName).Outline().Spacing(0).Name(tr("Engine API")),
				).Gap(8).Align(widget.AlignCenter),
			).Gap(24).Align(widget.AlignCenter),
			ui.Caption(tr("A Docker CLI on Windows reaches a Unix socket only from version 29 on. TCP on this computer's loopback address works with any version, on every system.")),
			host,
			proxyErr,
		).Gap(6).Stretch(),
		sharedFoldersField(m),
		sandboxEngineSettings(m),
	)
}

// sandboxEngineSettings is the Experimental subsection under Settings → Docker.
func sandboxEngineSettings(m *Model) ggui.Widget {
	enabled := ggui.Bind(func() bool { return m.Settings.Get().SandboxEngine }, m.SetSandboxEngine)
	dockerRunning := ggui.Combine(m.Settings, m.Rows, func(s storeSettings, rows []Row) bool {
		if !s.SandboxEngine {
			return false
		}
		r, ok := m.RecipeSandbox(rows, recipes.Docker)
		return ok && r.State.Status.Active()
	})
	runningNote := ggui.If(dockerRunning, func() ggui.Widget {
		return widget.Row(
			glyph(lucide.Info, 14).Color(colorAmber),
			widget.Expanded(ui.Caption(tr("Your Docker Engine sandbox is still running, but docker no longer reaches it while this mode is on. Stop it manually if you do not need it."))),
		).Gap(8).Align(widget.AlignStart)
	})
	builderStatus := ggui.If(ggui.Map(m.Settings, func(s storeSettings) bool { return s.SandboxEngine }), func() ggui.Widget {
		return sandboxBuilderStatus(m)
	})
	return widget.Column(
		widget.Row(
			glyph(lucide.FlaskConical, 14).Color(colorAmber),
			widget.Text(tr("Experimental")).Size(14).Weight(ggui.WeightMedium),
		).Gap(6).Align(widget.AlignCenter),
		settingRow(tr("Run containers as sandboxes, without a Docker Engine"),
			tr("GVX Desktop answers the Docker API directly and runs each container as its own gvx sandbox. Takes effect immediately; turning it off stops those containers."),
			ui.Switch(enabled, "")),
		ui.Caption(tr("Works: docker run (-d, -i, -t, --rm), build (BuildKit and Compose), ps, logs, stop, start, rm, ports on 127.0.0.1, volumes, bind mounts, pull/images, and single-service Compose.")),
		ui.Caption(tr("Not yet supported: exec, cp, inter-container networking, and Windows terminals.")),
		ui.Caption(tr("Turning this on routes docker build through buildx (docker buildx install) and selects the gvx-desktop builder for the gvx context and engine endpoint; turning it off puts both back.")),
		builderStatus,
		runningNote,
	).Gap(6).Stretch()
}

// sandboxBuilderStatus shows the lazy BuildKit builder state (idle/running)
// and the buildx gvx-desktop builder status line in Settings → Docker.
func sandboxBuilderStatus(m *Model) ggui.Widget {
	type viewState struct {
		builder engine.BuilderState
		bx      BuildxStatus
	}
	combined := ggui.Combine(m.BuilderState, m.BuildxStatus, func(b engine.BuilderState, bx BuildxStatus) viewState {
		return viewState{builder: b, bx: bx}
	})
	return ggui.View(combined, func(v viewState) ggui.Widget {
		var rows []ggui.Widget

		// 1. Builder sandbox state (idle / starting / running).
		var stateDot ggui.Widget
		var stateLabel string
		switch v.builder.Status {
		case engine.BuilderRunning:
			stateDot = dot(colorGreen, 7)
			stateLabel = tr("Image builder: running")
		case engine.BuilderStarting:
			stateDot = dot(colorAmber, 7)
			stateLabel = tr("Image builder: starting…")
		default:
			stateDot = dot(colorGray, 7)
			stateLabel = tr("Image builder: idle")
		}
		rows = append(rows, widget.Row(
			stateDot,
			widget.Text(stateLabel).Size(12).Weight(ggui.WeightMedium),
			widget.Expanded(ui.Caption(tr("Starts automatically on the first build and stops when idle.")).Size(12)),
		).Gap(8).Align(widget.AlignCenter))
		if v.builder.Err != "" {
			rows = append(rows, widget.Row(
				glyph(lucide.TriangleExclamationPoint, 13).Color(colorAmber),
				widget.Expanded(ui.Caption(tr("Image builder error: %s", v.builder.Err)).Size(12)),
			).Gap(8).Align(widget.AlignStart))
		}

		// 2. "Use the gvx-desktop builder" status line.
		bx := v.bx
		switch {
		case bx.Selected:
			rows = append(rows, widget.Row(
				glyph(lucide.CircleCheck, 14).Color(colorGreen),
				widget.Expanded(ui.Caption(tr("docker build uses the gvx-desktop builder for the gvx context.")).Size(12)),
			).Gap(8).Align(widget.AlignCenter))
		case bx.NoCLI:
			rows = append(rows, widget.Row(
				glyph(lucide.Info, 14).Color(colorAmber),
				widget.Expanded(ui.Caption(tr("docker was not found. Install the Docker CLI with the buildx plugin to use docker build.")).Size(12)),
			).Gap(8).Align(widget.AlignStart))
		case bx.NoBuildx:
			rows = append(rows, widget.Row(
				glyph(lucide.Info, 14).Color(colorAmber),
				widget.Expanded(ui.Caption(tr("The docker buildx plugin was not found. Install docker-buildx to use docker build with the gvx-desktop builder.")).Size(12)),
			).Gap(8).Align(widget.AlignStart))
		case bx.NoCtx:
			rows = append(rows, widget.Row(
				glyph(lucide.Info, 14).Color(colorBlue),
				widget.Expanded(ui.Caption(tr("The gvx-desktop builder is ready. Set up the gvx context, or run: docker buildx use gvx-desktop")).Size(12)),
				ui.Button(tr("Use the gvx-desktop builder"), m.UseSandboxBuilder).Outline().Size(ui.ButtonExtraSmall).Disabled(bx.Busy),
			).Gap(8).Align(widget.AlignCenter))
		case bx.Err != "":
			rows = append(rows, widget.Row(
				glyph(lucide.TriangleExclamationPoint, 14).Color(colorAmber),
				widget.Expanded(ui.Caption(tr("Could not configure the gvx-desktop builder: %s", bx.Err)).Size(12)),
				ui.Button(tr("Use the gvx-desktop builder"), m.UseSandboxBuilder).Outline().Size(ui.ButtonExtraSmall).Disabled(bx.Busy),
			).Gap(8).Align(widget.AlignCenter))
		default:
			rows = append(rows, widget.Row(
				glyph(lucide.Wrench, 14).Color(colorGray),
				widget.Expanded(ui.Caption(tr("Use the gvx-desktop builder")).Size(12)),
				ui.Button(tr("Use the gvx-desktop builder"), m.UseSandboxBuilder).Outline().Size(ui.ButtonExtraSmall).Disabled(bx.Busy),
			).Gap(8).Align(widget.AlignCenter))
		}

		return widget.Box(widget.Column(rows...).Gap(6).Stretch()).Pad(10).Radius(8).Border(1, colorGrayA)
	})
}

// sharedFoldersField lists the folders the Docker Engine shares at their
// own paths, with a button that removes each and one that adds another.
// In sandbox-engine mode it shows a dimmed note instead because rungv mounts
// host bind paths directly.
func sharedFoldersField(m *Model) ggui.Widget {
	return ggui.View(ggui.Map(m.Settings, func(s storeSettings) bool { return s.SandboxEngine }), func(se bool) ggui.Widget {
		if se {
			return widget.Column(
				widget.Text(tr("Shared folders")).Size(14).Weight(ggui.WeightMedium).Color(colorGray),
				ui.Caption(tr("Applies only to the Docker Engine sandbox. In sandbox-engine mode, bind mounts access host paths directly.")),
			).Gap(4).Stretch()
		}
		folders := ggui.Combine(m.Rows, m.Settings, m.sharedFoldersOf).WithEqual(slices.Equal[[]string])
		pick := ggui.State("")
		list := ggui.View(folders, func(list []string) ggui.Widget {
			add := ui.Button(tr("Add folder…"), func() { browseFolder(m, pick, m.ShareFolder) }).Outline().Size(ui.ButtonSmall).Icon(glyph(lucide.FolderPlus, 14))
			if len(list) == 0 {
				home := rungv.ExpandHome("~")
				return widget.Column(
					widget.Row(glyph(lucide.TriangleExclamationPoint, 14).Color(colorAmber),
						widget.Expanded(ui.Caption(tr("No folder is shared: bind mounts of folders on this computer are rejected."))),
					).Gap(8).Align(widget.AlignStart),
					widget.Row(ui.Button(tr("Share your home folder"), func() { m.ShareFolder(home) }).Size(ui.ButtonSmall), add).Gap(8),
				).Gap(8).Stretch()
			}
			rows := make([]ggui.Widget, 0, len(list)+1)
			for _, f := range list {
				rows = append(rows, widget.Row(
					glyph(lucide.Folder, 14).Color(colorGray),
					widget.Expanded(ui.Mono(tildePath(f)).Size(12).NoWrap().Ellipsis()),
					tip(iconButton(lucide.X, tr("Stop sharing %s", f), func() { m.UnshareFolder(f) }), tr("Stop sharing")),
				).Gap(8).Align(widget.AlignCenter))
			}
			rows = append(rows, widget.Row(add))
			return widget.Column(rows...).Gap(6).Stretch()
		})
		help := []ggui.Widget{list, ui.Caption(tr("The Docker Engine sees these folders at the same paths, so docker run -v and the bind mounts of a compose file work with what is in them. rungv keeps the owners of files in a .gvx_meta file at the top of each. The engine sees a change when it is restarted."))}
		if runtime.GOOS == "windows" {
			help = append(help, widget.Row(glyph(lucide.Info, 14).Color(colorBlue),
				widget.Expanded(ui.Caption(tr("A folder such as C:\\Users\\me is at /c/Users/me in the engine. Paths written the Windows way in docker run -v and in compose files are converted on the way there."))),
			).Gap(8).Align(widget.AlignStart))
		}
		return widget.Column(
			widget.Text(tr("Shared folders")).Size(14).Weight(ggui.WeightMedium),
			widget.Column(help...).Gap(6).Stretch(),
		).Gap(6).Stretch()
	})
}

// kubeSettings chooses what the Kubernetes recipe's cluster runs.
func kubeSettings(m *Model) ggui.Widget {
	serviceLB := ggui.Bind(func() bool {
		m.Settings.Get() // follows the setting; the sandbox's own command is what counts
		m.Rows.Get()
		return m.ServiceLB()
	}, m.SetServiceLB)
	return settingsGroup(lucide.ShipWheel, "Kubernetes",
		settingRow(tr("Service load balancer"),
			tr("Runs k3s's ServiceLB, so a LoadBalancer service gets an address instead of staying pending. The node's ports 80 and 443 reach this computer at 127.0.0.1:%d and :%d. A running cluster changes when it is restarted.",
				recipes.ServiceLBHTTPPort, recipes.ServiceLBHTTPSPort),
			ui.Switch(serviceLB, "")),
	)
}

// dockerEndpointName words an endpoint of the Docker Engine's API.
func dockerEndpointName(v string) string {
	if v == DockerTCPEndpoint {
		return "TCP"
	}
	return tr("Unix socket")
}
