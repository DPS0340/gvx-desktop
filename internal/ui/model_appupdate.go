package ui

import (
	"context"
	"os"
	"strings"
	"time"

	"github.com/ironpark/ggui"
	"github.com/ironpark/ggui/ui"

	"github.com/ironpark/gvx-desktop/internal/appupdate"
	"github.com/ironpark/gvx-desktop/internal/store"
)

// The app looks for a newer version of itself a while after it starts,
// out of the way of what starting does, and once a day after that, by the
// wall clock, which goes on while the computer sleeps.
const (
	appCheckDelay = 30 * time.Second
	appCheckEvery = 24 * time.Hour
)

// appDownloadTimeout bounds downloading an update of the app.
const appDownloadTimeout = 30 * time.Minute

// AppUpdate is what the app knows of its own newest release, and of the
// update it is getting.
type AppUpdate struct {
	Tag  string // the newest release, v0.6.0…; empty until it is known
	Page string // its notes

	// Install is how the app was installed, which says how it is updated,
	// and Command, for an install a package manager keeps, what updates it
	// from a terminal.
	Install appupdate.Install
	Command string

	Checking    bool
	Err         string // why the last check failed
	Downloading bool
	Percent     int  // of the download
	Ready       bool // downloaded and checked: a restart puts it in place
}

// Available reports a release newer than the app running. A development
// build is offered none.
func (u AppUpdate) Available() bool { return appupdate.Newer(u.Tag, AppVersion) }

// Name is the newest release as people say it: 0.6.0.
func (u AppUpdate) Name() string { return appupdate.Plain(u.Tag) }

// page is where the newest release's notes are, or the releases when
// there is none yet.
func (u AppUpdate) page() string {
	if u.Page != "" {
		return u.Page
	}
	return appupdate.ReleasesURL
}

// Offered reports that the app tells of an update by itself, in the
// banner and the tray: one is available that the user did not skip.
func (u AppUpdate) Offered(s store.Settings) bool { return u.Available() && u.Tag != s.SkipAppVersion }

// startAppUpdates removes what an earlier update left, and looks for a
// newer app a while after the app starts, and again once a day, while the
// settings let it.
func (m *Model) startAppUpdates() {
	if m.Offline {
		return
	}
	go cleanupAppUpdate(ggui.Peek(m.AppUpdate).Install)
	go func() {
		time.Sleep(appCheckDelay)
		t := time.NewTicker(time.Hour)
		defer t.Stop()
		for {
			m.post(m.checkAppUpdateDaily)
			<-t.C
		}
	}()
}

// cleanupAppUpdate removes what an update left beside the app. Windows lets
// go of the program an update replaced only once the app that ran it has
// ended, which may be a moment after this one started.
func cleanupAppUpdate(in appupdate.Install) {
	_ = appupdate.Cleanup(in)
	for i := 0; i < 15 && !appupdate.RemoveReplaced(in); i++ {
		time.Sleep(2 * time.Second)
	}
}

// checkAppUpdateDaily looks for a newer app when it has not looked for a
// day, and the settings let it.
func (m *Model) checkAppUpdateDaily() {
	if ggui.Peek(m.Settings).NoAppUpdateCheck || !appupdate.Comparable(AppVersion) {
		return
	}
	if !m.appChecked.IsZero() && time.Now().Round(0).Sub(m.appChecked) < appCheckEvery {
		return
	}
	m.CheckAppUpdate(true)
}

// CheckAppUpdate asks GitHub for the app's newest release. A quiet check,
// as the app makes by itself, says nothing of its own; one the user asked
// for tells them what it found, and shows again a version they skipped.
func (m *Model) CheckAppUpdate(quiet bool) {
	if u := ggui.Peek(m.AppUpdate); u.Checking || u.Downloading || m.Offline {
		return
	}
	m.appChecked = time.Now().Round(0) // the wall clock alone
	m.AppUpdate.Update(func(u AppUpdate) AppUpdate { u.Checking = true; return u })
	var rel appupdate.Release
	m.async(func(ctx context.Context) error {
		var err error
		rel, err = appupdate.Latest(ctx)
		return err
	}, func(err error) {
		if err != nil {
			m.AppUpdate.Update(func(u AppUpdate) AppUpdate { u.Checking, u.Err = false, err.Error(); return u })
			if !quiet {
				m.toastError(tr("Could not check for updates"), err)
			}
			return
		}
		m.appRelease = rel
		if s := m.appStaged; s != nil && s.Tag != rel.Tag {
			// What was downloaded is not the newest any more.
			m.appStaged = nil
			go s.Discard()
		}
		m.AppUpdate.Update(func(u AppUpdate) AppUpdate {
			u.Checking, u.Err = false, ""
			u.Tag, u.Page = rel.Tag, rel.Page
			u.Command = u.Install.Command(rel)
			u.Ready = m.appStaged != nil
			return u
		})
		if quiet {
			return
		}
		u := ggui.Peek(m.AppUpdate)
		switch {
		case !appupdate.Comparable(AppVersion):
			m.toastOK(tr("GVX Desktop %s is the newest release", u.Name()), tr("This build (%s) is not a release, and does not update itself.", AppVersion))
		case u.Available():
			if ggui.Peek(m.Settings).SkipAppVersion == u.Tag {
				m.UpdateSettings(func(s *store.Settings) { s.SkipAppVersion = "" })
			}
			m.Toasts.Push(ui.Toast(tr("GVX Desktop %s is available", u.Name()), tr("This is version %s.", AppVersion)).Success().
				Duration(10*time.Second).Action(appUpdateVerb(u), m.UpdateApp))
		default:
			m.toastOK(tr("GVX Desktop is up to date"), tr("%s is the newest version.", AppVersion))
		}
	})
}

// UpdateApp does what updates the app as it was installed: it downloads
// the new version, or once that is done asks to restart into it; copies
// the command a package manager updates it with; or opens the release
// page.
func (m *Model) UpdateApp() {
	u := ggui.Peek(m.AppUpdate)
	if u.Checking || u.Downloading || !u.Available() {
		return
	}
	switch {
	case u.Install.Method.SelfUpdates() && u.Ready:
		m.AskRestartToUpdate()
	case u.Install.Method.SelfUpdates():
		m.downloadAppUpdate()
	case u.Command != "":
		m.Copy(u.Command, appCommandHint(u.Install.Method))
	default:
		m.Open(u.page())
	}
}

// downloadAppUpdate downloads the newest release beside the app, checked
// against its checksum, for a restart to put in place.
func (m *Model) downloadAppUpdate() {
	u := ggui.Peek(m.AppUpdate)
	rel, in, name := m.appRelease, u.Install, u.Name()
	m.AppUpdate.Update(func(u AppUpdate) AppUpdate { u.Downloading, u.Percent, u.Ready = true, 0, false; return u })
	title := tr("Downloading GVX Desktop %s", name)
	toast := m.Toasts.Push(ui.Toast(title, tr("It goes in place when the app restarts.")).Loading())
	var staged *appupdate.Staged
	work := func(ctx context.Context) error {
		shown := -1
		var err error
		staged, err = appupdate.Stage(ctx, rel, in, func(done, total int64) {
			if total <= 0 {
				return
			}
			if p := int(done * 100 / total); p != shown {
				shown = p
				m.post(func() {
					m.AppUpdate.Update(func(u AppUpdate) AppUpdate { u.Percent = p; return u })
					m.Toasts.Update(toast, ui.Toast(title, tr("%d%% downloaded. It goes in place when the app restarts.", p)).Loading())
				})
			}
		})
		return err
	}
	done := func(err error) {
		m.AppUpdate.Update(func(u AppUpdate) AppUpdate { u.Downloading, u.Percent, u.Ready = false, 0, err == nil; return u })
		if err != nil {
			m.Toasts.Update(toast, ui.Toast(tr("Could not update GVX Desktop"), err.Error()).Error().Duration(10*time.Second))
			return
		}
		m.appStaged = staged
		m.Toasts.Update(toast, ui.Toast(tr("GVX Desktop %s is ready", name), tr("Restart the app to finish the update.")).Success().
			Duration(12*time.Second).Action(tr("Restart"), m.AskRestartToUpdate))
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), appDownloadTimeout)
		defer cancel()
		err := work(ctx)
		m.post(func() { done(err) })
	}()
}

// AskRestartToUpdate asks before the app quits to put the downloaded
// update in place, saying what that does to the sandboxes running.
func (m *Model) AskRestartToUpdate() {
	u := ggui.Peek(m.AppUpdate)
	if !u.Ready {
		return
	}
	text := tr("GVX Desktop quits, puts %s in place, and opens again.", u.Name())
	if n := ggui.Peek(m.Running); n > 0 {
		text = trn(n, "%d sandbox is running and will be stopped: sandboxes live only while the app runs.",
			"%d sandboxes are running and will be stopped: sandboxes live only while the app runs.") + " " + text
		m.Confirm(tr("Restart to update?"), text, tr("Restart to update"), m.restartToUpdate)
		return
	}
	m.Offer(tr("Restart to update?"), text, tr("Restart to update"), m.restartToUpdate)
}

// restartToUpdate readies the update to go in, and quits as the quit
// dialog does, stopping the sandboxes first; main finishes the update
// once the app has ended, with FinishUpdate.
func (m *Model) restartToUpdate() {
	s := m.appStaged
	if s == nil {
		return
	}
	args := relaunchArgs(os.Args[1:])
	m.async(func(context.Context) error { return s.Prepare(args) }, func(err error) {
		if err != nil {
			m.toastError(tr("Could not put the update in place"), err)
			return
		}
		m.appStaged, m.appRestart = nil, s
		if len(m.sup.Running()) > 0 {
			m.StopAllAndThen(func() { quitApp() })
			return
		}
		quitApp()
	})
}

// FinishUpdate starts the updated app when the user restarted to update,
// and otherwise removes an update downloaded and not put in place. main
// calls it last, once the app has ended everything it ran.
func (m *Model) FinishUpdate() error {
	if s := m.appRestart; s != nil {
		return s.Finish()
	}
	if s := m.appStaged; s != nil {
		return s.Discard()
	}
	return nil
}

// SkipAppVersion stops the app from telling of its newest release, until
// a newer one comes or the user checks for updates.
func (m *Model) SkipAppVersion() {
	u := ggui.Peek(m.AppUpdate)
	if u.Tag == "" {
		return
	}
	m.UpdateSettings(func(s *store.Settings) { s.SkipAppVersion = u.Tag })
	m.Toasts.Push(ui.Toast(tr("Skipped GVX Desktop %s", u.Name()), tr("Check for updates in Settings to see it again.")).Info())
}

// relaunchArgs are the arguments the updated app starts with: this run's,
// but for the flag a start at login passes, as the user is there.
func relaunchArgs(args []string) []string {
	var out []string
	for _, a := range args {
		if name, _, _ := strings.Cut(strings.TrimLeft(a, "-"), "="); strings.HasPrefix(a, "-") && name == HiddenFlag {
			continue
		}
		out = append(out, a)
	}
	return out
}

// appUpdateVerb is what the button that updates the app says.
func appUpdateVerb(u AppUpdate) string {
	switch {
	case u.Install.Method.SelfUpdates() && u.Ready:
		return tr("Restart to update")
	case u.Install.Method.SelfUpdates():
		return tr("Update")
	case u.Command != "":
		return tr("Copy command")
	}
	return tr("Open the release page")
}

// appCommandHint says where to run the command a package manager updates
// the app with.
func appCommandHint(m appupdate.Method) string {
	switch m {
	case appupdate.Homebrew:
		return tr("Run it in a terminal, then open the app again.")
	case appupdate.Chocolatey:
		return tr("Quit the app, then run it in a terminal opened as administrator.")
	}
	return tr("Run it in a terminal, then open the app again.")
}

// appUpdateText says how the newest release gets onto this computer, as
// the app was installed.
func appUpdateText(u AppUpdate) string {
	this := tr("This is version %s.", AppVersion) + " "
	switch u.Install.Method {
	case appupdate.Bundle, appupdate.Program:
		if u.Ready {
			return this + tr("The update is downloaded: it goes in place when the app restarts.")
		}
		return this + tr("It downloads now and goes in place when the app restarts.")
	case appupdate.Homebrew:
		return this + tr("Homebrew installed this copy: run %s in a terminal, then open the app again.", u.Command)
	case appupdate.Chocolatey:
		return this + tr("Chocolatey installed this copy: quit the app and update it from a terminal opened as administrator.")
	case appupdate.Deb, appupdate.RPM:
		if u.Command == "" {
			return this + tr("The system's package manager installed this copy: install the new package from the release page.")
		}
		return this + tr("The system's package manager installed this copy: the command downloads the new package, checks it and installs it.")
	}
	return this + tr("This copy cannot replace itself where it is: download the new version from the release page.")
}
