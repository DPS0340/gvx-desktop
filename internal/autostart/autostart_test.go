//go:build !windows

package autostart

import (
	"os"
	"strings"
	"testing"
)

// The registration lives under the home directory, so a made-up home
// keeps the test out of the real one.
func TestSetRegistersAndTakesAway(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", home+"/.config")

	if Enabled() {
		t.Fatal("enabled in an empty home")
	}
	if err := Set(true); err != nil {
		t.Fatal(err)
	}
	was, ok := registered()
	if !ok {
		t.Fatal("not registered after Set(true)")
	}
	exe, _ := os.Executable()
	if args := strings.Split(was, "\x00"); args[len(args)-1] != HiddenFlag || !strings.Contains(was, exe[strings.LastIndex(exe, "/")+1:]) {
		t.Errorf("registered %q, which is not this program started hidden", args)
	}
	if err := Refresh(); err != nil {
		t.Fatal(err)
	}
	if again, _ := registered(); again != was {
		t.Errorf("Refresh changed an up-to-date registration: %q", again)
	}
	// A registration for a program that has moved is written again.
	if err := register([]string{"/old/place/gvx-desktop", HiddenFlag}); err != nil {
		t.Fatal(err)
	}
	if err := Refresh(); err != nil {
		t.Fatal(err)
	}
	if now, _ := registered(); now != was {
		t.Errorf("Refresh left %q", now)
	}
	if err := Set(false); err != nil {
		t.Fatal(err)
	}
	if Enabled() {
		t.Error("enabled after Set(false)")
	}
	if err := Set(false); err != nil {
		t.Errorf("taking away what is not there: %v", err)
	}
}

func TestBundleOf(t *testing.T) {
	if b, ok := bundleOf("/Applications/GVX Desktop.app/Contents/MacOS/gvx-desktop"); !ok || b != "/Applications/GVX Desktop.app" {
		t.Errorf("%q %v", b, ok)
	}
	if _, ok := bundleOf("/usr/local/bin/gvx-desktop"); ok {
		t.Error("a bare program is in no bundle")
	}
}
