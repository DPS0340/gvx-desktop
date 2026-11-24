package ui

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/ironpark/ggui"

	"github.com/ironpark/gvx-desktop/internal/dockercli"
	"github.com/ironpark/gvx-desktop/internal/rungv"
)

// RungvPathState describes whether the active rungv binary is reachable from
// the user's terminal PATH (either via ~/.gvx/bin or an existing PATH entry).
type RungvPathState struct {
	EnginePath string // active rungv binary path
	BinDir     string // ~/.gvx/bin
	Profile    string // shell profile path (or "Windows User PATH")
	InBinDir   bool   // active rungv is at or linked/synced into ~/.gvx/bin/rungv
	BinInPATH  bool   // ~/.gvx/bin is on PATH or in user's shell profile / registry
	Managed    bool   // ~/.gvx/bin PATH entry was written by GVX Desktop
	OnPATH     bool   // exec.LookPath("rungv") already resolves to EnginePath
	PATHBinary string // path resolved by exec.LookPath("rungv"), if any
	Busy       bool   // PATH update in progress
}

// Equal makes RungvPathState comparable for UI signals.
func (s RungvPathState) Equal(o RungvPathState) bool { return s == o }

// lookPathRungv resolves rungv on the process PATH; tests may override it.
var lookPathRungv = exec.LookPath

func sameBinaryFile(a, b string) bool {
	a, b = strings.TrimSpace(a), strings.TrimSpace(b)
	if a == "" || b == "" {
		return false
	}
	if runtime.GOOS == "windows" {
		if strings.EqualFold(filepath.Clean(a), filepath.Clean(b)) {
			return true
		}
	} else if filepath.Clean(a) == filepath.Clean(b) {
		return true
	}
	sa, errA := os.Stat(a)
	sb, errB := os.Stat(b)
	return errA == nil && errB == nil && os.SameFile(sa, sb)
}

func probeRungvPathState(enginePath string) RungvPathState {
	ps := dockercli.PathStatus()
	st := RungvPathState{
		EnginePath: enginePath,
		BinDir:     ps.Dir,
		Profile:    ps.Profile,
		BinInPATH:  ps.Active(),
		Managed:    ps.Managed,
	}
	if enginePath != "" {
		st.InBinDir = dockercli.BinaryLinkedInBinDir(enginePath, "rungv")
		if p, err := lookPathRungv(rungv.BinaryName()); err == nil && p != "" {
			st.PATHBinary = p
			st.OnPATH = sameBinaryFile(p, enginePath)
		}
	}
	return st
}

// refreshRungvPathState re-reads rungv's ~/.gvx/bin and terminal PATH status.
func (m *Model) refreshRungvPathState() {
	if m.Offline {
		return
	}
	enginePath := ggui.Peek(m.Engine).Path
	var st RungvPathState
	m.async(func(ctx context.Context) error {
		st = probeRungvPathState(enginePath)
		return nil
	}, func(error) {
		st.Busy = ggui.Peek(m.RungvPath).Busy
		m.RungvPath.Set(st)
	})
}

// AddRungvToPATH ensures the active rungv binary is exposed inside ~/.gvx/bin
// and appends ~/.gvx/bin to the user's terminal PATH (shell profile on Unix,
// HKCU\Environment\Path on Windows).
func (m *Model) AddRungvToPATH() {
	if m.Offline || ggui.Peek(m.RungvPath).Busy {
		return
	}
	prev := ggui.Peek(m.RungvPath)
	enginePath := ggui.Peek(m.Engine).Path
	dockerSetting := ggui.Peek(m.Settings).DockerPath
	m.RungvPath.Update(func(s RungvPathState) RungvPathState { s.Busy = true; return s })
	var (
		st       RungvPathState
		dockerSt BundledDockerState
	)
	m.async(func(ctx context.Context) error {
		if enginePath != "" && !dockercli.BinaryLinkedInBinDir(enginePath, "rungv") {
			if _, err := dockercli.SyncBinaryToBinDir(enginePath, "rungv"); err != nil {
				return err
			}
		}
		if _, err := dockercli.AddToPath(); err != nil {
			return err
		}
		st = probeRungvPathState(enginePath)
		dockerSt = probeBundledDockerState(dockerSetting)
		return nil
	}, func(err error) {
		if err != nil {
			m.RungvPath.Update(func(s RungvPathState) RungvPathState { s.Busy = false; return s })
			m.toastError(tr("Could not add ~/.gvx/bin to PATH"), err)
			return
		}
		m.RungvPath.Set(st)
		if dockerSt.Available || ggui.Peek(m.BundledDocker).Available {
			m.BundledDocker.Set(dockerSt)
		}
		if prev.BinInPATH {
			m.toastOK(tr("Linked rungv in ~/.gvx/bin"), tr("Available in terminals via %s.", tildePath(st.BinDir)))
		} else {
			m.toastOK(tr("Added ~/.gvx/bin to PATH"), tr("Updated %s. Open a new terminal to use rungv.", tildePath(st.Profile)))
		}
	})
}

// RemoveRungvFromPATH removes the GVX Desktop-managed ~/.gvx/bin PATH entry
// from the user's shell profile or Windows User PATH.
func (m *Model) RemoveRungvFromPATH() {
	if m.Offline || ggui.Peek(m.RungvPath).Busy {
		return
	}
	enginePath := ggui.Peek(m.Engine).Path
	dockerSetting := ggui.Peek(m.Settings).DockerPath
	m.RungvPath.Update(func(s RungvPathState) RungvPathState { s.Busy = true; return s })
	var (
		st       RungvPathState
		dockerSt BundledDockerState
	)
	m.async(func(ctx context.Context) error {
		if _, err := dockercli.RemoveFromPath(); err != nil {
			return err
		}
		st = probeRungvPathState(enginePath)
		dockerSt = probeBundledDockerState(dockerSetting)
		return nil
	}, func(err error) {
		if err != nil {
			m.RungvPath.Update(func(s RungvPathState) RungvPathState { s.Busy = false; return s })
			m.toastError(tr("Could not remove ~/.gvx/bin from PATH"), err)
			return
		}
		m.RungvPath.Set(st)
		if dockerSt.Available || ggui.Peek(m.BundledDocker).Available {
			m.BundledDocker.Set(dockerSt)
		}
		m.toastOK(tr("Removed ~/.gvx/bin from PATH"), tr("Updated %s.", tildePath(st.Profile)))
	})
}
