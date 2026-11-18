package ui

import (
	"context"
	"errors"
	"runtime"

	"github.com/ironpark/ggui"

	"github.com/ironpark/gvx-desktop/internal/rungv"
)

// Update is what the app knows of gvx's releases, and of the one it is
// installing.
type Update struct {
	Tag        string // the newest release, v0.17.1…; empty until one is known
	Page       string // where its notes are
	Checking   bool
	Err        string // why the last check failed
	Installing bool
	Percent    int // of the download, while installing
}

// Available reports a release newer than the rungv of e.
func (u Update) Available(e Engine) bool {
	return u.Tag != "" && e.Path != "" && e.Checked && rungv.Newer(u.Tag, e.Version.Version)
}

// CheckUpdate asks for the newest release of gvx. A quiet check, as the one
// at launch, says nothing of its own; one the user asked for tells them
// what it found.
func (m *Model) CheckUpdate(quiet bool) {
	if u := ggui.Peek(m.Update); u.Checking || u.Installing {
		return
	}
	m.Update.Update(func(u Update) Update { u.Checking = true; return u })
	var rel rungv.Release
	m.async(func(ctx context.Context) error {
		var err error
		rel, err = rungv.Latest(ctx)
		return err
	}, func(err error) {
		if err != nil {
			m.Update.Update(func(u Update) Update { u.Checking, u.Err = false, err.Error(); return u })
			if !quiet {
				m.toastError(tr("Could not check for a new rungv"), err)
			}
			return
		}
		m.release = rel
		u := Update{Tag: rel.Tag, Page: rel.Page}
		m.Update.Set(u)
		if quiet {
			return
		}
		switch e := ggui.Peek(m.Engine); {
		case e.Path == "":
			m.toastOK(tr("rungv %s can be installed", shortVersion(rel.Tag)), "")
		case u.Available(e):
			m.toastOK(tr("rungv %s is available", shortVersion(rel.Tag)), tr("This computer has %s.", shortVersion(e.Version.Version)))
		default:
			m.toastOK(tr("rungv is up to date"), shortVersion(e.Version.Version))
		}
	})
}

// InstallRungv downloads the newest rungv and puts it in place of the one
// in use, or in ~/.gvx/bin when there is none or it is not this app's to
// replace. Sandboxes that are running keep the rungv they started with.
func (m *Model) InstallRungv() {
	if u := ggui.Peek(m.Update); u.Checking || u.Installing {
		return
	}
	m.Update.Update(func(u Update) Update { u.Installing, u.Percent, u.Err = true, 0, ""; return u })
	current := ggui.Peek(m.Engine).Path
	rel := m.release
	var target string
	m.async(func(ctx context.Context) error {
		if rel.Tag == "" {
			var err error
			if rel, err = rungv.Latest(ctx); err != nil {
				return err
			}
		}
		var err error
		if target, _, err = rungv.InstallTarget(current); err != nil {
			return err
		}
		shown := -1
		err = rungv.Install(ctx, rel, target, func(done, total int64) {
			if total <= 0 {
				return
			}
			if p := int(done * 100 / total); p != shown {
				shown = p
				m.post(func() { m.Update.Update(func(u Update) Update { u.Percent = p; return u }) })
			}
		})
		if err != nil {
			return err
		}
		if runtime.GOOS == "darwin" {
			// A rungv without the entitlement cannot reach the hypervisor.
			if err := rungv.Sign(ctx, target); err != nil {
				return errors.New(tr("rungv was installed, but signing it failed: ") + err.Error())
			}
		}
		return nil
	}, func(err error) {
		m.Update.Update(func(u Update) Update {
			u.Installing, u.Percent = false, 0
			if rel.Tag != "" {
				u.Tag, u.Page = rel.Tag, rel.Page
			}
			return u
		})
		if rel.Tag != "" {
			m.release = rel
		}
		if err != nil {
			m.toastError(tr("Could not install rungv"), err)
			if target != "" {
				m.DetectEngine(nil)
			}
			return
		}
		// Another rungv found before this one would hide it: name it.
		set := ggui.Peek(m.Settings)
		if found, _ := rungv.Locate(set.RungvPath); found != target {
			m.UpdateSettings(func(s *storeSettings) { s.RungvPath = target })
		}
		m.toastOK(tr("rungv %s installed", shortVersion(rel.Tag)), target)
		m.DetectEngine(nil)
	})
}
