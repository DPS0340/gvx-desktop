//go:build !windows

package ui

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/ironpark/ggui"

	"github.com/ironpark/gvx-desktop/internal/store"
)

// The start at login is registered to start the app hidden, the switch
// shows what the system holds, a registration from another place is
// written again and an up-to-date one is left alone, and the entry builds
// before ggui's registration wrote on Linux is replaced. The registration
// lives under the home directory, so a made-up home keeps the test out of
// the real one.
func TestStartAtLogin(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	registrations := filepath.Join(home, ".config", "autostart", "*.desktop")
	if runtime.GOOS == "darwin" {
		registrations = filepath.Join(home, "Library", "LaunchAgents", "*.plist")
	}
	registration := func() (string, string) {
		t.Helper()
		found, _ := filepath.Glob(registrations)
		if len(found) != 1 {
			t.Fatalf("registrations %v, want one", found)
		}
		b, err := os.ReadFile(found[0])
		if err != nil {
			t.Fatal(err)
		}
		return found[0], string(b)
	}

	dir := t.TempDir()
	var m *Model
	p := ggui.ProbeBuilder(func() ggui.Widget { return Build(m) }, ggui.Sz(900, 700))
	p.Setup(func() {
		m = NewModel(store.Store{Dir: dir}, dir)
		m.Offline = true
		m.Attach(p)
	})
	defer p.Close()
	p.Frame()

	m.refreshAutostart()
	if ggui.Peek(m.Autostart) {
		t.Fatal("on in an empty home")
	}
	m.SetAutostart(true)
	if !ggui.Peek(m.Autostart) {
		t.Fatal("off after it was turned on")
	}
	exe, _ := os.Executable()
	exe, _ = filepath.EvalSymlinks(exe)
	path, data := registration()
	if !strings.Contains(data, exe) || !strings.Contains(data, "-"+HiddenFlag) {
		t.Errorf("registered\n%s\nwhich does not start %s hidden", data, exe)
	}
	if got := ggui.Peek(m.Settings).LoginCommand; got != loginCommand() {
		t.Errorf("remembered %q", got)
	}

	// Up to date, it is not written again.
	os.WriteFile(path, []byte("left alone"), 0o644)
	m.refreshAutostart()
	if _, data := registration(); data != "left alone" {
		t.Errorf("an up-to-date registration was written again: %s", data)
	}
	// Registered from another place, it is.
	m.UpdateSettings(func(s *store.Settings) { s.LoginCommand = "/old/place/gvx-desktop -hidden" })
	m.refreshAutostart()
	if _, data := registration(); !strings.Contains(data, exe) {
		t.Errorf("a registration from another place was left: %s", data)
	}
	if got := ggui.Peek(m.Settings).LoginCommand; got != loginCommand() || !ggui.Peek(m.Autostart) {
		t.Errorf("after registering again: remembered %q, on %v", got, ggui.Peek(m.Autostart))
	}

	m.SetAutostart(false)
	if ggui.Peek(m.Autostart) || ggui.Peek(m.Settings).LoginCommand != "" {
		t.Errorf("after turning it off: on %v, remembered %q", ggui.Peek(m.Autostart), ggui.Peek(m.Settings).LoginCommand)
	}
	if found, _ := filepath.Glob(registrations); len(found) != 0 {
		t.Errorf("left %v", found)
	}

	if runtime.GOOS == "darwin" {
		return
	}
	old := filepath.Join(home, ".config", "autostart", "gvx-desktop.desktop")
	os.WriteFile(old, []byte("[Desktop Entry]\nExec=/old/gvx-desktop -hidden\n"), 0o644)
	m.refreshAutostart()
	if _, err := os.Stat(old); err == nil {
		t.Error("the entry of an older build was left")
	}
	if _, data := registration(); !strings.Contains(data, exe) || !ggui.Peek(m.Autostart) {
		t.Errorf("the entry of an older build was not replaced: %s", data)
	}
}
