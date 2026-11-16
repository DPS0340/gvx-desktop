// Package store keeps the desktop app's sandboxes and settings as JSON in
// the user's configuration directory.
package store

import (
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"sort"

	"github.com/ironpark/gvx-desktop/internal/sandbox"
)

// Settings are the app's preferences.
type Settings struct {
	RungvPath     string `json:"rungvPath,omitempty"` // empty: locate automatically
	CacheDir      string `json:"cacheDir,omitempty"`  // empty: rungv's default
	Theme         string `json:"theme,omitempty"`     // system (default), light, dark
	StopOnQuit    bool   `json:"stopOnQuit"`          // stop running sandboxes on quit without asking
	HideOnClose   bool   `json:"hideOnClose"`         // closing the window keeps the app in the tray
	StopTimeout   int    `json:"stopTimeout,omitempty"`
	SidebarNarrow bool   `json:"sidebarNarrow,omitempty"`
}

// DefaultSettings are the settings of a first launch.
func DefaultSettings() Settings {
	return Settings{Theme: "system", StopOnQuit: true, HideOnClose: true, StopTimeout: 15}
}

// Store reads and writes the files under Dir.
type Store struct {
	Dir string
}

// DefaultDir is gvx-desktop under the user's configuration directory.
func DefaultDir() (string, error) {
	base, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(base, "gvx-desktop"), nil
}

func (s Store) path(name string) string { return filepath.Join(s.Dir, name) }

func (s Store) read(name string, v any) error {
	b, err := os.ReadFile(s.path(name))
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	return json.Unmarshal(b, v)
}

// write replaces a file atomically.
func (s Store) write(name string, v any) error {
	if err := os.MkdirAll(s.Dir, 0o700); err != nil {
		return err
	}
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(s.Dir, name+".*")
	if err != nil {
		return err
	}
	if _, err := tmp.Write(append(b, '\n')); err != nil {
		tmp.Close()
		os.Remove(tmp.Name())
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmp.Name())
		return err
	}
	return os.Rename(tmp.Name(), s.path(name))
}

// Sandboxes reads the saved sandboxes, oldest first.
func (s Store) Sandboxes() ([]sandbox.Spec, error) {
	var specs []sandbox.Spec
	if err := s.read("sandboxes.json", &specs); err != nil {
		return nil, err
	}
	sort.SliceStable(specs, func(i, j int) bool { return specs[i].CreatedAt.Before(specs[j].CreatedAt) })
	return specs, nil
}

// SaveSandboxes writes the sandboxes.
func (s Store) SaveSandboxes(specs []sandbox.Spec) error {
	if specs == nil {
		specs = []sandbox.Spec{}
	}
	return s.write("sandboxes.json", specs)
}

// Settings reads the settings, filling defaults.
func (s Store) Settings() (Settings, error) {
	set := DefaultSettings()
	err := s.read("settings.json", &set)
	if set.StopTimeout <= 0 {
		set.StopTimeout = 15
	}
	return set, err
}

// SaveSettings writes the settings.
func (s Store) SaveSettings(set Settings) error { return s.write("settings.json", set) }
