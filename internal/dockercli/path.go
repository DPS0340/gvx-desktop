package dockercli

import (
	"path/filepath"
	"strings"
)

const pathMarker = "# Added by GVX Desktop"

// PathState describes whether ~/.gvx/bin is on the user's terminal PATH.
type PathState struct {
	Dir       string // ~/.gvx/bin
	Profile   string // shell profile path on Unix, or "Windows User PATH" on Windows
	InEnv     bool   // Dir is in this process's PATH
	InProfile bool   // Dir is configured in the user's shell profile or registry PATH
	Managed   bool   // entry was written by GVX Desktop and can be cleanly removed
}

// Active reports whether ~/.gvx/bin is already available in terminals (either
// in the current environment or in the user's persistent shell profile / PATH).
func (s PathState) Active() bool { return s.InEnv || s.InProfile }

// Equal makes PathState comparable for UI signals.
func (s PathState) Equal(o PathState) bool { return s == o }

// pathListContains reports whether dir appears in a PATH-like string split by
// sep (os.PathListSeparator).
func pathListContains(pathEnv, dir string, sep byte) bool {
	if pathEnv == "" || dir == "" {
		return false
	}
	cleanDir := filepath.Clean(dir)
	for entry := range strings.SplitSeq(pathEnv, string(sep)) {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}
		if samePath(filepath.Clean(entry), cleanDir) {
			return true
		}
	}
	return false
}

// ManualPathCommand returns a copyable command that appends ~/.gvx/bin to PATH
// in a terminal without overwriting an existing system docker.
func ManualPathCommand() string {
	if goos() == "windows" {
		return `$env:PATH += ";$HOME\.gvx\bin"`
	}
	if strings.Contains(strings.ToLower(filepath.Base(getenv("SHELL"))), "fish") {
		return `fish_add_path -a "$HOME/.gvx/bin"`
	}
	return `export PATH="$PATH:$HOME/.gvx/bin"`
}
