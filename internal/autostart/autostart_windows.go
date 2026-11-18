package autostart

import (
	"strings"

	"golang.org/x/sys/windows/registry"
)

// The values of the user's Run key are command lines Windows runs at each
// sign-in.
const runKey = `Software\Microsoft\Windows\CurrentVersion\Run`

func commandLine(cmd []string) string {
	parts := make([]string, len(cmd))
	for i, arg := range cmd {
		if strings.ContainsAny(arg, " \t") {
			arg = `"` + arg + `"`
		}
		parts[i] = arg
	}
	return strings.Join(parts, " ")
}

func register(cmd []string) error {
	k, _, err := registry.CreateKey(registry.CURRENT_USER, runKey, registry.SET_VALUE)
	if err != nil {
		return err
	}
	defer k.Close()
	return k.SetStringValue(ID, commandLine(cmd))
}

func unregister() error {
	k, err := registry.OpenKey(registry.CURRENT_USER, runKey, registry.SET_VALUE)
	if err != nil {
		return nil // no Run key, so nothing registered
	}
	defer k.Close()
	if err := k.DeleteValue(ID); err != nil && err != registry.ErrNotExist {
		return err
	}
	return nil
}

// registered returns what the Run key holds for the app, written as
// Refresh compares it, and whether it holds anything.
func registered() (string, bool) {
	k, err := registry.OpenKey(registry.CURRENT_USER, runKey, registry.QUERY_VALUE)
	if err != nil {
		return "", false
	}
	defer k.Close()
	line, _, err := k.GetStringValue(ID)
	if err != nil {
		return "", false
	}
	// The line was written from a command of two parts: the program, which
	// is quoted when it has a space, and the flag.
	program := strings.TrimSuffix(line, " "+HiddenFlag)
	return strings.Trim(program, `"`) + "\x00" + HiddenFlag, true
}
