package autostart

import (
	"encoding/xml"
	"os"
	"path/filepath"
	"strings"
)

// A launch agent in the user's Library is loaded at each login. Writing
// the file is enough for the next one; it is not loaded now, which would
// start the app a second time.
func agentPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, "Library", "LaunchAgents", ID+".plist"), nil
}

func register(cmd []string) error {
	path, err := agentPath()
	if err != nil {
		return err
	}
	var b strings.Builder
	b.WriteString(xml.Header)
	b.WriteString(`<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">` + "\n")
	b.WriteString("<plist version=\"1.0\">\n<dict>\n\t<key>Label</key>\n\t<string>" + ID + "</string>\n\t<key>ProgramArguments</key>\n\t<array>\n")
	for _, arg := range cmd {
		b.WriteString("\t\t<string>")
		if err := xml.EscapeText(&b, []byte(arg)); err != nil {
			return err
		}
		b.WriteString("</string>\n")
	}
	b.WriteString("\t</array>\n\t<key>RunAtLoad</key>\n\t<true/>\n\t<key>ProcessType</key>\n\t<string>Interactive</string>\n</dict>\n</plist>\n")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, []byte(b.String()), 0o644)
}

func unregister() error {
	path, err := agentPath()
	if err != nil {
		return err
	}
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

// registered returns the command the agent runs, its arguments joined by
// NUL, and whether there is an agent.
func registered() (string, bool) {
	path, err := agentPath()
	if err != nil {
		return "", false
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return "", false
	}
	var doc struct {
		Args []string `xml:"dict>array>string"`
	}
	if err := xml.Unmarshal(b, &doc); err != nil {
		return "", true
	}
	return strings.Join(doc.Args, "\x00"), true
}
