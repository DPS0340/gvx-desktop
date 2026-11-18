package ui

import (
	"os"
	"path/filepath"
	goruntime "runtime"

	"github.com/ironpark/ggui"

	"github.com/ironpark/gvx-desktop/internal/store"
)

// HiddenFlag names the flag, without its dash, that a start at login
// passes the app, which then starts in the tray without showing its window.
const HiddenFlag = "hidden"

// The registration of a start at login is ggui's, named for the app's id:
// the bundle identifier, the id ggui build links in, or the one main gives
// SingleInstance, which are all io.github.ironpark.gvx-desktop. A binary
// built otherwise has the last only once SingleInstance has run, so the
// registration is read no sooner than Start.

// loginCommand is what a start at login runs, the program running now and
// its flag, as the settings remember it.
func loginCommand() string {
	exe, err := os.Executable()
	if err != nil {
		return ""
	}
	if resolved, err := filepath.EvalSymlinks(exe); err == nil {
		exe = resolved
	}
	return exe + " -" + HiddenFlag
}

// SetAutostart registers the app to start when the user logs in, or takes
// the registration away. The system's record is what is true, so the
// switch follows it rather than the settings.
func (m *Model) SetAutostart(on bool) {
	if err := m.registerLogin(on); err != nil {
		m.toastError(tr("Could not change what starts at login"), err)
	}
	m.readAutostart()
}

// registerLogin registers the start at login, or takes it away, and
// remembers what it registered.
func (m *Model) registerLogin(on bool) error {
	if err := ggui.SetOpenAtLogin(on, "-"+HiddenFlag); err != nil {
		return err
	}
	cmd := ""
	if on {
		cmd = loginCommand()
	}
	if ggui.Peek(m.Settings).LoginCommand != cmd {
		m.UpdateSettings(func(s *store.Settings) { s.LoginCommand = cmd })
	}
	return nil
}

// readAutostart has the switch show whether the system will start the app.
func (m *Model) readAutostart() {
	on, _ := ggui.OpenAtLogin()
	m.Autostart.Set(on)
}

// refreshAutostart registers the start at login again when it was
// registered from another place: the registration names the program by its
// path, which moving or updating the app elsewhere leaves behind. One that
// is up to date is not written again, which macOS could take for a new
// login item and tell the user of.
func (m *Model) refreshAutostart() {
	on, _ := ggui.OpenAtLogin()
	if removeOldAutostartEntry() || on && ggui.Peek(m.Settings).LoginCommand != loginCommand() {
		_ = m.registerLogin(true)
	}
	m.readAutostart()
}

// removeOldAutostartEntry removes the XDG autostart entry that builds of
// the app before it used ggui's registration wrote, which was named for the
// program, gvx-desktop.desktop, where ggui names it for the app's id, and
// reports whether there was one. The launch agent and the Run value were
// named for the id already, so ggui finds those and writes them over.
func removeOldAutostartEntry() bool {
	if goruntime.GOOS == "darwin" || goruntime.GOOS == "windows" {
		return false
	}
	dir, err := os.UserConfigDir()
	if err != nil {
		return false
	}
	return os.Remove(filepath.Join(dir, "autostart", "gvx-desktop.desktop")) == nil
}
