package ui

import (
	"context"
	"errors"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/ironpark/ggui"

	"github.com/ironpark/gvx-desktop/internal/rungv"
)

// The channels rungv is updated from: the releases of gvx, or the head of
// its main branch, built here from source.
const (
	ChannelRelease = "release"
	ChannelSource  = "source"
)

// buildTimeout bounds a build from source, which compiles gVisor the first
// time and takes minutes.
const buildTimeout = 45 * time.Minute

// Update is what the app knows of the newest gvx on its channel, and of
// the one it is installing.
type Update struct {
	Source bool // the channel: main built from source, not a release

	Tag  string // the newest release, v0.17.1…; empty until one is known
	Page string // where its notes are

	Commit  string // the head of main; empty until it is known
	Subject string // the first line of its message
	Ahead   int    // commits main is past the rungv in use; -1 when unknown

	Checking   bool
	Err        string // why the last check failed
	Installing bool
	Percent    int    // of the download, while installing a release
	Phase      string // what a build from source is doing
}

// Known reports that the newest gvx of the channel has been looked up.
func (u Update) Known() bool {
	if u.Source {
		return u.Commit != ""
	}
	return u.Tag != ""
}

// Name is what the newest gvx of the channel is called.
func (u Update) Name() string {
	if u.Source {
		return commitName(u.Commit)
	}
	return shortVersion(u.Tag)
}

// commitName names a build of main by its commit.
func commitName(sha string) string {
	if len(sha) > 7 {
		sha = sha[:7]
	}
	return rungv.SourceBranch + "@" + sha
}

// Available reports a gvx newer than the rungv of e.
func (u Update) Available(e Engine) bool {
	if !e.Checked || e.Path == "" || !u.Known() {
		return false
	}
	if u.Source {
		return e.Version.Commit != u.Commit && u.Ahead != 0
	}
	return rungv.Newer(u.Tag, e.Version.Version)
}

// Channel is the channel the settings choose.
func (m *Model) Channel() string {
	if ggui.Peek(m.Settings).UpdateChannel == ChannelSource {
		return ChannelSource
	}
	return ChannelRelease
}

// SetChannel changes the channel, forgets what was known of the other, and
// looks the new one up.
func (m *Model) SetChannel(channel string) {
	if channel == m.Channel() || ggui.Peek(m.Update).Installing {
		return
	}
	m.UpdateSettings(func(s *storeSettings) { s.UpdateChannel = channel })
	m.Update.Set(Update{Source: channel == ChannelSource})
	m.BuildLog.Set("")
	m.CheckUpdate(true)
}

// CheckUpdate asks for the newest gvx of the channel. A quiet check, as
// the one at launch, says nothing of its own; one the user asked for tells
// them what it found.
func (m *Model) CheckUpdate(quiet bool) {
	if u := ggui.Peek(m.Update); u.Checking || u.Installing || m.Offline {
		return
	}
	source := m.Channel() == ChannelSource
	installed := ggui.Peek(m.Engine).Version.Commit
	m.Update.Update(func(u Update) Update { u.Source, u.Checking = source, true; return u })
	var rel rungv.Release
	var head rungv.Head
	m.async(func(ctx context.Context) error {
		var err error
		if source {
			head, err = rungv.SourceHead(ctx, installed)
		} else {
			rel, err = rungv.Latest(ctx)
		}
		return err
	}, func(err error) {
		if source != (m.Channel() == ChannelSource) {
			return // the channel changed while this was asked
		}
		if err != nil {
			m.Update.Update(func(u Update) Update { u.Checking, u.Err = false, err.Error(); return u })
			if !quiet {
				m.toastError(tr("Could not check for a new rungv"), err)
			}
			return
		}
		u := Update{Source: source, Tag: rel.Tag, Page: rel.Page, Commit: head.SHA, Subject: head.Subject, Ahead: head.Ahead}
		if source {
			u.Page = rungv.SourceURL + "/commits/" + rungv.SourceBranch
		} else {
			m.release = rel
		}
		m.Update.Set(u)
		if quiet {
			return
		}
		switch e := ggui.Peek(m.Engine); {
		case e.Path == "":
			m.toastOK(tr("rungv %s can be installed", u.Name()), "")
		case u.Available(e):
			m.toastOK(tr("rungv %s is available", u.Name()), tr("This computer has %s.", engineVersion(e)))
		default:
			m.toastOK(tr("rungv is up to date"), engineVersion(e))
		}
	})
}

// InstallRungv installs the newest rungv of the channel — a release
// downloaded, or main built from source — in place of the one in use, or
// in ~/.gvx/bin when there is none or it is not this app's to replace.
// Sandboxes that are running keep the rungv they started with.
func (m *Model) InstallRungv() {
	if u := ggui.Peek(m.Update); u.Checking || u.Installing {
		return
	}
	source := m.Channel() == ChannelSource
	m.Update.Update(func(u Update) Update {
		u.Source, u.Installing, u.Percent, u.Phase, u.Err = source, true, 0, "", ""
		return u
	})
	current := ggui.Peek(m.Engine).Path
	rel := m.release
	var target, built string
	work := func(ctx context.Context) error {
		var err error
		if target, _, err = rungv.InstallTarget(current); err != nil {
			return err
		}
		if source {
			built, err = m.buildFromSource(ctx, target)
		} else {
			if rel.Tag == "" {
				if rel, err = rungv.Latest(ctx); err != nil {
					return err
				}
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
		}
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
	}
	done := func(err error) {
		m.Update.Update(func(u Update) Update {
			u.Installing, u.Percent, u.Phase = false, 0, ""
			switch {
			case source && built != "":
				u.Commit, u.Ahead = built, 0
			case !source && rel.Tag != "":
				u.Tag, u.Page = rel.Tag, rel.Page
			}
			return u
		})
		if !source && rel.Tag != "" {
			m.release = rel
		}
		if err != nil {
			if errors.Is(err, rungv.ErrNoToolchain) {
				err = errors.New(tr("Building from source needs git and Go on this computer: ") + err.Error())
			}
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
		name := shortVersion(rel.Tag)
		if source {
			name = commitName(built)
			m.BuildLog.Set("")
		}
		m.toastOK(tr("rungv %s installed", name), target)
		m.DetectEngine(nil)
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), buildTimeout)
		defer cancel()
		err := work(ctx)
		m.post(func() { done(err) })
	}()
}

// buildFromSource builds main into target, telling the screen what it is
// doing and keeping the end of what git and Go print in BuildLog.
func (m *Model) buildFromSource(ctx context.Context, target string) (string, error) {
	var tail []string
	var last time.Time
	flush := func() {
		text := strings.Join(tail, "\n")
		m.post(func() { m.BuildLog.Set(text) })
	}
	phase := func(p rungv.Phase) {
		text := ""
		switch p {
		case rungv.PhaseFetch:
			text = tr("Fetching the source…")
		case rungv.PhaseBuild:
			text = tr("Building… this takes minutes")
		case rungv.PhaseInstall:
			text = tr("Installing…")
		}
		m.post(func() { m.Update.Update(func(u Update) Update { u.Phase = text; return u }) })
	}
	line := func(s string) {
		if strings.TrimSpace(s) == "" {
			return
		}
		tail = append(tail, s)
		if len(tail) > 200 {
			tail = tail[len(tail)-200:]
		}
		// git prints progress many times a second; the screen need not.
		if now := time.Now(); now.Sub(last) > 150*time.Millisecond {
			last = now
			flush()
		}
	}
	commit, err := rungv.BuildFromSource(ctx, filepath.Join(m.store.Dir, "gvx-src"), target, phase, line)
	if err != nil {
		tail = append(tail, "error: "+err.Error())
	}
	flush()
	return commit, err
}

// isSourceBuild reports a version as Go gives a build between tags, which
// ends in the time and the hash of its commit: a build from source, not a
// release.
func isSourceBuild(version string) bool {
	i := strings.LastIndexByte(version, '-')
	return i > 0 && len(version)-i-1 == 12 && strings.Count(version, "-") >= 2
}

// engineVersion is the version of the rungv in use as people say it: a
// release's without its build stamp, a build from source by its commit.
func engineVersion(e Engine) string {
	if isSourceBuild(e.Version.Version) && e.Version.Commit != "" {
		return commitName(e.Version.Commit)
	}
	return shortVersion(e.Version.Version)
}
