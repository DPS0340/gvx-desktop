// Package autostart registers the app to start when the user logs in, the
// way each system has for a program that asks no more of it: a launch
// agent on macOS, a value under the Run key on Windows, a desktop entry in
// the autostart directory elsewhere.
package autostart

import (
	"os"
	"path/filepath"
	"strings"
)

// ID names the registration, as the app's bundle identifier does the app.
const ID = "io.github.ironpark.gvx-desktop"

// HiddenFlag is the argument a registered start passes the app, which
// then starts in the tray without showing its window.
const HiddenFlag = "-hidden"

// command is what a login runs: the app as it is running now, hidden.
// Inside a macOS bundle that is the bundle, opened as the Finder would,
// so that the system knows the app by it.
func command() ([]string, error) {
	exe, err := os.Executable()
	if err != nil {
		return nil, err
	}
	if resolved, err := filepath.EvalSymlinks(exe); err == nil {
		exe = resolved
	}
	if bundle, ok := bundleOf(exe); ok {
		return []string{"/usr/bin/open", "-a", bundle, "--args", HiddenFlag}, nil
	}
	return []string{exe, HiddenFlag}, nil
}

// bundleOf is the .app a macOS executable lies in.
func bundleOf(exe string) (string, bool) {
	const inside = ".app/Contents/MacOS/"
	if i := strings.Index(exe, inside); i >= 0 {
		return exe[:i+len(".app")], true
	}
	return "", false
}

// Enabled reports whether the app is registered to start at login.
func Enabled() bool {
	_, ok := registered()
	return ok
}

// Set registers the app to start at login, or takes the registration away.
func Set(on bool) error {
	if !on {
		return unregister()
	}
	cmd, err := command()
	if err != nil {
		return err
	}
	return register(cmd)
}

// Refresh writes the registration again when it names another program
// than the one running, as it does once the app has been moved or
// updated in another place. It does nothing when the app is not
// registered.
func Refresh() error {
	was, ok := registered()
	if !ok {
		return nil
	}
	cmd, err := command()
	if err != nil {
		return err
	}
	if was == strings.Join(cmd, "\x00") {
		return nil
	}
	return register(cmd)
}
