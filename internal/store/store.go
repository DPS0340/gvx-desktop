// Package store keeps the desktop app's sandboxes and settings as JSON in
// the user's configuration directory.
package store

import (
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"sort"

	"github.com/ironpark/gvx-desktop/internal/sandbox"
)

// Settings are the app's preferences.
type Settings struct {
	RungvPath      string `json:"rungvPath,omitempty"` // empty: locate automatically
	CacheDir       string `json:"cacheDir,omitempty"`  // empty: rungv's default
	Theme          string `json:"theme,omitempty"`     // system (default), light, dark
	Language       string `json:"language,omitempty"`  // system (default), en, ko
	StopOnQuit     bool   `json:"stopOnQuit"`          // stop running sandboxes on quit without asking
	HideOnClose    bool   `json:"hideOnClose"`         // closing the window keeps the app in the tray
	StopTimeout    int    `json:"stopTimeout,omitempty"`
	SidebarNarrow  bool   `json:"sidebarNarrow,omitempty"`
	NoTray         bool   `json:"noTray,omitempty"`         // keep out of the menu bar or notification area
	SearchRegistry string `json:"searchRegistry,omitempty"` // the registry the pull dialog searches: hub (default), quay
	DockerPath     string `json:"dockerPath,omitempty"`     // the docker command; empty: find it
	DockerEndpoint string `json:"dockerEndpoint,omitempty"` // how the app serves the Docker API: socket, tcp; empty: the system's default
	DockerPort     int    `json:"dockerPort,omitempty"`     // the host port of a tcp endpoint; 0: 2375
	K3sServiceLB   bool   `json:"k3sServiceLB,omitempty"`   // the Kubernetes recipe runs k3s's load balancer, ServiceLB
	UpdateChannel  string `json:"updateChannel,omitempty"`  // where rungv is updated from: release (default), source
	SourceRepo     string `json:"sourceRepo,omitempty"`     // git repository for building rungv from source; empty: gfire-sigs/gvx
	SourceBranch   string `json:"sourceBranch,omitempty"`   // branch or tag for building rungv from source; empty: main
	NoUpdateCheck  bool   `json:"noUpdateCheck,omitempty"`  // do not ask GitHub for a newer rungv at launch
	LoginCommand   string `json:"loginCommand,omitempty"`   // what the start at login was registered to run, to register it again when the app moves
	// DockerSharedFolders are the host folders the Docker Engine recipe
	// shares at their own paths: nil for the default, the home folder, and
	// empty for none.
	DockerSharedFolders []string `json:"dockerSharedFolders"`
	// Resume names, by ID, the recipe sandboxes the user left running: the
	// app starts them again when it next starts. Quitting, which stops every
	// sandbox, leaves it as it is; the user stopping one takes it out.
	Resume []string `json:"resume,omitempty"`
	// DockerContextBefore is the docker context that was in use when the app
	// switched docker to gvx, to switch back to.
	DockerContextBefore string `json:"dockerContextBefore,omitempty"`
	// KubeContextBefore is kubectl's current context when the app switched
	// kubectl to gvx-k3s, to switch back to.
	KubeContextBefore string `json:"kubeContextBefore,omitempty"`
}

// Equal makes Settings comparable for signals, which a list in it keeps
// == from doing.
func (s Settings) Equal(o Settings) bool {
	if !slices.Equal(s.Resume, o.Resume) {
		return false
	}
	s.Resume, o.Resume = nil, nil
	return reflect.DeepEqual(s, o) // which tells a nil list of shared folders from an empty one
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
