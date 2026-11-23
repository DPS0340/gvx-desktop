//go:build !windows

package dockercli

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
)

func shellProfile(home string) (profile, line string, isFish bool) {
	sh := strings.ToLower(filepath.Base(strings.TrimSpace(getenv("SHELL"))))
	switch sh {
	case "zsh":
		return filepath.Join(home, ".zshrc"), `export PATH="$PATH:$HOME/.gvx/bin"`, false
	case "fish":
		return filepath.Join(home, ".config", "fish", "conf.d", "gvx.fish"), `fish_add_path -a "$HOME/.gvx/bin"`, true
	case "bash":
		if goos() == "darwin" {
			return filepath.Join(home, ".bash_profile"), `export PATH="$PATH:$HOME/.gvx/bin"`, false
		}
		return filepath.Join(home, ".bashrc"), `export PATH="$PATH:$HOME/.gvx/bin"`, false
	default:
		if goos() == "darwin" {
			return filepath.Join(home, ".zshrc"), `export PATH="$PATH:$HOME/.gvx/bin"`, false
		}
		return filepath.Join(home, ".profile"), `export PATH="$PATH:$HOME/.gvx/bin"`, false
	}
}

// PathStatus reports whether ~/.gvx/bin is already on PATH or configured in the
// user's shell profile.
func PathStatus() PathState {
	dir, err := BinDir()
	if err != nil {
		return PathState{}
	}
	home, err := userHomeDir()
	if err != nil {
		return PathState{Dir: dir}
	}
	profile, _, _ := shellProfile(home)
	st := PathState{
		Dir:     dir,
		Profile: profile,
		InEnv:   pathListContains(getenv("PATH"), dir, os.PathListSeparator),
	}
	if data, err := os.ReadFile(profile); err == nil {
		text := string(data)
		st.Managed = strings.Contains(text, pathMarker) && profileMentionsBin(text, dir)
		st.InProfile = st.Managed || profileMentionsBin(text, dir)
	}
	return st
}

func profileMentionsBin(text, dir string) bool {
	return strings.Contains(text, ".gvx/bin") || (dir != "" && strings.Contains(text, dir))
}

// AddToPath appends ~/.gvx/bin to the user's shell profile (idempotently, at
// the end of PATH so any system-installed docker keeps precedence).
func AddToPath() (PathState, error) {
	home, err := userHomeDir()
	if err != nil {
		return PathState{}, err
	}
	profile, line, _ := shellProfile(home)
	var existing string
	if b, err := os.ReadFile(profile); err == nil {
		existing = string(b)
	} else if !errors.Is(err, os.ErrNotExist) {
		return PathStatus(), err
	}
	if dir, err := BinDir(); err == nil && profileMentionsBin(existing, dir) {
		return PathStatus(), nil
	}
	if err := os.MkdirAll(filepath.Dir(profile), 0o755); err != nil {
		return PathStatus(), err
	}
	var b strings.Builder
	b.WriteString(existing)
	if len(existing) > 0 && !strings.HasSuffix(existing, "\n") {
		b.WriteByte('\n')
	}
	if len(existing) > 0 {
		b.WriteByte('\n')
	}
	b.WriteString(pathMarker)
	b.WriteByte('\n')
	b.WriteString(line)
	b.WriteByte('\n')
	if err := os.WriteFile(profile, []byte(b.String()), 0o644); err != nil {
		return PathStatus(), err
	}
	return PathStatus(), nil
}

// RemoveFromPath removes the GVX Desktop PATH entry previously added by
// AddToPath from the user's shell profile.
func RemoveFromPath() (PathState, error) {
	home, err := userHomeDir()
	if err != nil {
		return PathState{}, err
	}
	profile, _, isFish := shellProfile(home)
	data, err := os.ReadFile(profile)
	if errors.Is(err, os.ErrNotExist) {
		return PathStatus(), nil
	}
	if err != nil {
		return PathStatus(), err
	}
	lines := strings.Split(string(data), "\n")
	out := make([]string, 0, len(lines))
	for i := 0; i < len(lines); i++ {
		if strings.TrimSpace(lines[i]) == pathMarker && i+1 < len(lines) && strings.Contains(lines[i+1], ".gvx/bin") {
			// Also drop one preceding blank separator line if we added one.
			if len(out) > 0 && out[len(out)-1] == "" {
				out = out[:len(out)-1]
			}
			i++ // skip the PATH line too
			continue
		}
		out = append(out, lines[i])
	}
	joined := strings.TrimSpace(strings.Join(out, "\n"))
	if isFish && joined == "" {
		_ = os.Remove(profile)
		return PathStatus(), nil
	}
	if joined != "" {
		joined += "\n"
	}
	if err := os.WriteFile(profile, []byte(joined), 0o644); err != nil {
		return PathStatus(), err
	}
	return PathStatus(), nil
}
