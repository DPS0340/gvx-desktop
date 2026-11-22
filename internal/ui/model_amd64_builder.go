package ui

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/ironpark/ggui"

	"github.com/ironpark/gvx-desktop-vm/vmassets"
	"github.com/ironpark/gvx-desktop-vm/vmclient"
	"github.com/ironpark/gvx-desktop-vm/vmhelper"
	"github.com/ironpark/gvx-desktop/internal/engine"
	"github.com/ironpark/gvx-desktop/internal/store"
)

// Amd64CheckState is the result of running the VM helper's requirements check
// (or Rosetta installation) for the experimental amd64 builder.
type Amd64CheckState struct {
	Checked    bool
	Checking   bool
	Installing bool
	Result     vmhelper.CheckResult
	Err        string // failure running the helper itself
}

// Equal makes Amd64CheckState comparable for signals.
func (s Amd64CheckState) Equal(o Amd64CheckState) bool { return s == o }

// OK reports whether all host requirements for the amd64 builder are met.
func (s Amd64CheckState) OK() bool {
	if !s.Checked || s.Err != "" || !s.Result.OK() {
		return false
	}
	if s.Result.MacOS != "" && !macOSAtLeast13(s.Result.MacOS) {
		return false
	}
	return true
}

func macOSAtLeast13(version string) bool {
	if version == "" {
		return false
	}
	majorStr, _, _ := strings.Cut(strings.TrimSpace(version), ".")
	major, err := strconv.Atoi(majorStr)
	return err == nil && major >= 13
}

type vmHelperRunner func(ctx context.Context, subcmd string) (vmhelper.CheckResult, error)

// defaultRunVMHelper invokes this executable in VM-helper mode so the app
// process never loads Virtualization.framework directly.
func defaultRunVMHelper(ctx context.Context, subcmd string) (vmhelper.CheckResult, error) {
	if runtime.GOOS != "darwin" {
		return vmhelper.CheckResult{Rosetta: vmhelper.RosettaNotSupported}, nil
	}
	exe, err := os.Executable()
	if err != nil {
		return vmhelper.CheckResult{}, err
	}
	cmd := exec.CommandContext(ctx, exe, vmhelper.Arg, subcmd)
	out, err := cmd.Output()
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			if msg := strings.TrimSpace(string(ee.Stderr)); msg != "" {
				return vmhelper.CheckResult{}, errors.New(msg)
			}
		}
		return vmhelper.CheckResult{}, err
	}
	var res vmhelper.CheckResult
	if err := json.Unmarshal(out, &res); err != nil {
		return vmhelper.CheckResult{}, err
	}
	return res, nil
}

// CheckAmd64 runs `<exe> __vm-helper check` in the background to check Apple
// silicon, macOS 13+, virtualization entitlement, and Rosetta availability.
func (m *Model) CheckAmd64() {
	if m.Offline {
		return
	}
	cur := ggui.Peek(m.Amd64Check)
	if cur.Checking || cur.Installing {
		return
	}
	m.Amd64Check.Update(func(c Amd64CheckState) Amd64CheckState {
		c.Checking = true
		return c
	})
	run := m.runVMHelper
	var res vmhelper.CheckResult
	m.async(func(ctx context.Context) error {
		ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
		defer cancel()
		var err error
		res, err = run(ctx, vmhelper.CmdCheck)
		return err
	}, func(err error) {
		next := Amd64CheckState{Checked: true, Result: res}
		if err != nil {
			next.Err = firstLine(err.Error())
		}
		m.Amd64Check.Set(next)
	})
}

// InstallRosetta runs `<exe> __vm-helper install-rosetta` so macOS shows its
// Rosetta installation prompt, then refreshes Amd64Check with the result.
func (m *Model) InstallRosetta() {
	if m.Offline {
		return
	}
	cur := ggui.Peek(m.Amd64Check)
	if cur.Installing || cur.Checking {
		return
	}
	m.Amd64Check.Update(func(c Amd64CheckState) Amd64CheckState {
		c.Installing = true
		c.Err = ""
		return c
	})
	run := m.runVMHelper
	var res vmhelper.CheckResult
	m.async(func(ctx context.Context) error {
		ctx, cancel := context.WithTimeout(ctx, 10*time.Minute)
		defer cancel()
		var err error
		res, err = run(ctx, vmhelper.CmdInstallRosetta)
		return err
	}, func(err error) {
		next := Amd64CheckState{Checked: true, Result: res}
		if err != nil {
			next.Err = firstLine(err.Error())
			m.toastError(tr("Rosetta installation failed"), err)
		} else if res.Err != "" {
			m.toastError(tr("Rosetta installation failed"), errors.New(res.Err))
		} else if res.Rosetta == vmhelper.RosettaInstalled {
			m.toastOK(tr("Rosetta installed"), tr("You can now turn on amd64 builds."))
		}
		m.Amd64Check.Set(next)
	})
}

// amd64Config builds the engine's Amd64Config pointing at this executable in
// helper mode and resolving VM assets from the user's cache directory.
func (m *Model) amd64Config() (*engine.Amd64Config, error) {
	exe, err := os.Executable()
	if err != nil {
		return nil, err
	}
	return &engine.Amd64Config{
		Launcher: vmclient.Launcher{
			Path: exe,
			Args: []string{vmhelper.Arg, vmhelper.CmdServe},
			Log: func(line string) {
				log.Printf("vm-helper: %s", line)
			},
		},
		Assets: func(ctx context.Context, progress func(done, total int64)) (vmassets.Dir, error) {
			cacheDir, err := vmassets.DefaultCacheDir()
			if err != nil {
				return vmassets.Dir{}, err
			}
			return vmassets.Resolve(ctx, vmassets.Options{
				CacheDir: cacheDir,
				Progress: progress,
			})
		},
	}, nil
}

func (m *Model) refreshAmd64State() {
	if m.Offline || m.sandboxEngine == nil {
		return
	}
	m.Amd64State.Set(m.sandboxEngine.Amd64State())
}

func (m *Model) syncAmd64BuildxNode(on bool) {
	if m.syncAmd64NodeHook != nil {
		m.syncAmd64NodeHook(on)
		return
	}
	m.syncAmd64Node(on)
}

// SetAmd64Builds turns the experimental amd64 builder VM on or off. Turning it
// on requires both the sandbox engine and passing host requirements; turning it
// off is always allowed.
func (m *Model) SetAmd64Builds(on bool) {
	set := ggui.Peek(m.Settings)
	if set.Amd64Builds == on {
		return
	}
	if on && (!set.SandboxEngine || !ggui.Peek(m.Amd64Check).OK()) {
		return
	}
	m.UpdateSettings(func(s *store.Settings) { s.Amd64Builds = on })
	if m.Offline || m.sandboxEngine == nil || !ggui.Peek(m.Settings).SandboxEngine {
		return
	}
	m.applyAmd64Config(on)
}

func (m *Model) applyAmd64Config(on bool) {
	if m.Offline || m.sandboxEngine == nil {
		return
	}
	m.amd64Gen++
	gen := m.amd64Gen
	eng := m.sandboxEngine
	var cfg *engine.Amd64Config
	if on {
		var err error
		cfg, err = m.amd64Config()
		if err != nil {
			m.toastError(tr("Could not configure amd64 builds"), err)
			return
		}
	}
	m.async(func(context.Context) error {
		return eng.SetAmd64(cfg)
	}, func(err error) {
		if m.amd64Gen != gen {
			return
		}
		m.refreshAmd64State()
		if err != nil {
			m.toastError(tr("Could not configure amd64 builds"), err)
			return
		}
		set := ggui.Peek(m.Settings)
		m.syncAmd64BuildxNode(on && set.SandboxEngine && set.Amd64Builds)
	})
}

// AskResetAmd64Disk confirms and deletes the amd64 builder's persistent cache
// disk, stopping the VM first if it is running.
func (m *Model) AskResetAmd64Disk() {
	if m.Offline || m.sandboxEngine == nil || ggui.Peek(m.Amd64Resetting) {
		return
	}
	m.Confirm(
		tr("Reset amd64 build cache?"),
		tr("Stops the amd64 builder VM if it is running and deletes its persistent disk image. The next amd64 build starts with an empty cache."),
		tr("Reset build cache"),
		func() {
			if m.sandboxEngine == nil || ggui.Peek(m.Amd64Resetting) {
				return
			}
			eng := m.sandboxEngine
			m.Amd64Resetting.Set(true)
			m.async(func(ctx context.Context) error {
				ctx, cancel := context.WithTimeout(ctx, time.Minute)
				defer cancel()
				return eng.ResetAmd64Disk(ctx)
			}, func(err error) {
				m.Amd64Resetting.Set(false)
				m.refreshAmd64State()
				if err != nil {
					m.toastError(tr("Could not reset amd64 build cache"), err)
					return
				}
				m.toastOK(tr("amd64 build cache reset"), "")
			})
		},
	)
}
