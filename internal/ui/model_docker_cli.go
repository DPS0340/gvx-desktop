package ui

import (
	"context"
	"os"
	"strings"

	"github.com/ironpark/ggui"

	"github.com/ironpark/gvx-desktop/internal/dockercli"
	"github.com/ironpark/gvx-desktop/internal/rungv"
)

// BundledDockerState describes the bundled Docker CLI (docker, docker-buildx,
// docker-compose) shipped inside the application package and its ~/.gvx/bin
// terminal PATH status.
type BundledDockerState struct {
	Available  bool   // bundled docker binary is present in this build
	HasBuildx  bool   // bundled docker-buildx plugin is present
	HasCompose bool   // bundled docker-compose plugin is present
	Using      bool   // currently resolved docker CLI is the bundled one
	CLIPath    string // ~/.gvx/bin/docker or direct bundle path
	BinDir     string // ~/.gvx/bin
	Profile    string // shell profile path (or "Windows User PATH")
	InPATH     bool   // ~/.gvx/bin is already on PATH / shell profile
	Managed    bool   // PATH entry was written by GVX Desktop
	Busy       bool   // PATH update in progress
}

// Equal makes BundledDockerState comparable for UI signals.
func (s BundledDockerState) Equal(o BundledDockerState) bool { return s == o }

// ToolsList formats the bundled CLI components present (e.g. "docker, buildx, compose").
func (s BundledDockerState) ToolsList() string {
	parts := []string{"docker"}
	if s.HasBuildx {
		parts = append(parts, "buildx")
	}
	if s.HasCompose {
		parts = append(parts, "compose")
	}
	return strings.Join(parts, ", ")
}

func probeBundledDockerState(setting string) BundledDockerState {
	b := dockercli.Bundled()
	if !b.Available() {
		return BundledDockerState{}
	}
	ps := dockercli.PathStatus()
	cli, ok := findCLI("docker", setting)
	using := ok && dockercli.IsBundledPath(cli)
	cliPath := b.Docker
	if managed, err := dockercli.ManagedDockerPath(); err == nil {
		if st, err := os.Stat(managed); err == nil && !st.IsDir() {
			cliPath = managed
		}
	}
	return BundledDockerState{
		Available:  true,
		HasBuildx:  b.Buildx != "",
		HasCompose: b.Compose != "",
		Using:      using,
		CLIPath:    cliPath,
		BinDir:     ps.Dir,
		Profile:    ps.Profile,
		InPATH:     ps.Active(),
		Managed:    ps.Managed,
	}
}

// syncBundledDocker links the bundled Docker CLI + plugins into ~/.gvx/bin and
// ~/.gvx/cli-plugins when present, registers cliPluginsExtraDirs when the
// bundled CLI is active (no external system docker installed), and updates
// Model.BundledDocker.
func (m *Model) syncBundledDocker() {
	if m.Offline {
		return
	}
	setting := ggui.Peek(m.Settings).DockerPath
	var st BundledDockerState
	m.async(func(ctx context.Context) error {
		b := dockercli.Bundled()
		if !b.Available() {
			return nil
		}
		registerPlugins := false
		if strings.TrimSpace(setting) == "" {
			if p, err := rungv.FindTool("docker"); err != nil || dockercli.IsBundledPath(p) {
				registerPlugins = true
			}
		} else if dockercli.IsBundledPath(rungv.ExpandHome(setting)) {
			registerPlugins = true
		}
		_, _ = dockercli.Sync(registerPlugins)
		st = probeBundledDockerState(setting)
		return nil
	}, func(error) {
		m.BundledDocker.Set(st)
		if st.Available && st.Using {
			m.CheckLink(m.DockerLink)
		}
	})
}

// refreshBundledDockerState re-reads bundled CLI availability and PATH state.
func (m *Model) refreshBundledDockerState() {
	if m.Offline {
		return
	}
	setting := ggui.Peek(m.Settings).DockerPath
	var st BundledDockerState
	m.async(func(ctx context.Context) error {
		st = probeBundledDockerState(setting)
		return nil
	}, func(error) {
		st.Busy = ggui.Peek(m.BundledDocker).Busy
		m.BundledDocker.Set(st)
	})
}

// AddBundledDockerToPATH links the bundled Docker CLI + plugins and appends
// ~/.gvx/bin to the user's terminal PATH (at the end, so any system-installed
// docker retains precedence).
func (m *Model) AddBundledDockerToPATH() {
	if m.Offline || ggui.Peek(m.BundledDocker).Busy {
		return
	}
	setting := ggui.Peek(m.Settings).DockerPath
	m.BundledDocker.Update(func(s BundledDockerState) BundledDockerState { s.Busy = true; return s })
	var st BundledDockerState
	m.async(func(ctx context.Context) error {
		if _, err := dockercli.Sync(true); err != nil {
			return err
		}
		if _, err := dockercli.AddToPath(); err != nil {
			return err
		}
		st = probeBundledDockerState(setting)
		return nil
	}, func(err error) {
		if err != nil {
			m.BundledDocker.Update(func(s BundledDockerState) BundledDockerState { s.Busy = false; return s })
			m.toastError(tr("Could not add ~/.gvx/bin to PATH"), err)
			return
		}
		m.BundledDocker.Set(st)
		m.refreshRungvPathState()
		m.toastOK(tr("Added ~/.gvx/bin to PATH"), tr("Updated %s. Open a new terminal to use docker.", tildePath(st.Profile)))
		m.CheckLink(m.DockerLink)
	})
}

// RemoveBundledDockerFromPATH removes the GVX Desktop ~/.gvx/bin PATH entry.
func (m *Model) RemoveBundledDockerFromPATH() {
	if m.Offline || ggui.Peek(m.BundledDocker).Busy {
		return
	}
	setting := ggui.Peek(m.Settings).DockerPath
	m.BundledDocker.Update(func(s BundledDockerState) BundledDockerState { s.Busy = true; return s })
	var st BundledDockerState
	m.async(func(ctx context.Context) error {
		if _, err := dockercli.RemoveFromPath(); err != nil {
			return err
		}
		st = probeBundledDockerState(setting)
		return nil
	}, func(err error) {
		if err != nil {
			m.BundledDocker.Update(func(s BundledDockerState) BundledDockerState { s.Busy = false; return s })
			m.toastError(tr("Could not remove ~/.gvx/bin from PATH"), err)
			return
		}
		m.BundledDocker.Set(st)
		m.refreshRungvPathState()
		m.toastOK(tr("Removed ~/.gvx/bin from PATH"), tr("Updated %s.", tildePath(st.Profile)))
	})
}
