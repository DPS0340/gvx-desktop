package ui

import (
	"runtime"
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
	noTray := ggui.Map(m.Settings, func(s storeSettings) bool { return s.NoTray })

	groups := []ggui.Widget{
		headed(pageHeader(tr("Settings"), ui.Caption(tr("Where rungv lives and how the app treats running sandboxes."))), engineAlert(m)),
		settingsGroup(lucide.Palette, tr("General"),
			settingRow(tr("Appearance"), tr("Light, dark, or as the system is."),
				ui.ToggleGroup[string](theme).Options([]string{"system", "light", "dark"}).Format(themeName).Outline().Name(tr("Appearance"))),
			settingRow(tr("Language"), tr("The language the app is worded in."),
				ui.ToggleGroup[string](language).Options([]string{LangSystem, LangEnglish, LangKorean}).Format(languageName).Outline().Name(tr("Language"))),
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

// releaseRow says which rungv gvx has released last, and installs it.
func releaseRow(m *Model) ggui.Widget {
	type state struct {
		e Engine
		u Update
	}
	both := ggui.Combine(m.Engine, m.Update, func(e Engine, u Update) state { return state{e, u} })
	return ggui.View(both, func(s state) ggui.Widget {
		e, u := s.e, s.u
		var text string
		var action ggui.Widget = widget.Box()
		switch {
		case u.Installing:
			text = tr("Installing rungv %s", shortVersion(u.Tag))
			action = installButton(m, u, tr("Install rungv")).Size(ui.ButtonSmall)
		case u.Checking:
			text = tr("Asking GitHub for the newest release…")
		case u.Tag == "" && u.Err != "":
			text = tr("The newest release could not be read: %s", u.Err)
		case u.Tag == "":
			text = tr("The newest release has not been looked up.")
		case e.Path == "":
			text = tr("rungv %s can be installed into ~/.gvx/bin.", shortVersion(u.Tag))
			action = installButton(m, u, tr("Install rungv")).Size(ui.ButtonSmall)
		case u.Available(e):
			text = tr("rungv %s is available; this computer has %s.", shortVersion(u.Tag), shortVersion(e.Version.Version))
			action = installButton(m, u, tr("Update")).Size(ui.ButtonSmall)
		default:
			text = tr("rungv is up to date: %s is the newest release.", shortVersion(u.Tag))
		}
		return widget.Row(
			action,
			ui.Button(tr("Check for updates"), func() { m.CheckUpdate(false) }).Outline().Size(ui.ButtonSmall).
				Disabled(u.Checking || u.Installing),
			widget.Expanded(ui.Caption(text)),
		).Gap(10).Align(widget.AlignCenter)
	})
}
