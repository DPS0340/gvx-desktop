//go:build !darwin && !windows

package autostart

import (
	"os"
	"path/filepath"
	"strings"
)

// A desktop entry in the autostart directory is started by the desktop
// at each login, as the XDG autostart specification has it.
func entryPath() (string, error) {
	dir, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "autostart", "gvx-desktop.desktop"), nil
}

func quote(arg string) string {
	if strings.ContainsAny(arg, " \t\"'\\$`") {
		return `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`, "$", `\$`, "`", "\\`").Replace(arg) + `"`
	}
	return arg
}

func register(cmd []string) error {
	path, err := entryPath()
	if err != nil {
		return err
	}
	parts := make([]string, len(cmd))
	for i, arg := range cmd {
		parts[i] = quote(arg)
	}
	entry := "[Desktop Entry]\nType=Application\nName=GVX Desktop\nExec=" + strings.Join(parts, " ") +
		"\nX-GNOME-Autostart-enabled=true\nX-GVX-Command=" + strings.Join(cmd, "\t") + "\n"
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, []byte(entry), 0o644)
}

func unregister() error {
	path, err := entryPath()
	if err != nil {
		return err
	}
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

func registered() (string, bool) {
	path, err := entryPath()
	if err != nil {
		return "", false
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return "", false
	}
	for _, line := range strings.Split(string(b), "\n") {
		if cmd, ok := strings.CutPrefix(line, "X-GVX-Command="); ok {
			return strings.ReplaceAll(cmd, "\t", "\x00"), true
		}
	}
	return "", true
}
