package ui

import (
	"slices"
	"strings"
	"testing"

	"github.com/ironpark/ggui"

	"github.com/ironpark/gvx-desktop/internal/appupdate"
	"github.com/ironpark/gvx-desktop/internal/store"
)

// appUpdateModel is a model whose app is version 0.5.1, with no network.
func appUpdateModel(t *testing.T) (*Model, *ggui.Probe) {
	t.Helper()
	old := AppVersion
	AppVersion = "0.5.1"
	t.Cleanup(func() { AppVersion = old })
	dir := t.TempDir()
	set := store.DefaultSettings()
	set.Language = LangEnglish
	st := store.Store{Dir: dir}
	st.SaveSettings(set)
	t.Cleanup(func() { SetLanguage(LangEnglish) })
	var m *Model
	p := ggui.ProbeBuilder(func() ggui.Widget { return Build(m) }, ggui.Sz(1100, 760))
	p.Setup(func() {
		m = NewModel(st, dir)
		m.Offline = true
		m.Attach(p)
	})
	t.Cleanup(p.Close)
	p.Frame()
	return m, p
}

func TestAppUpdateBanner(t *testing.T) {
	m, p := appUpdateModel(t)
	const title = "GVX Desktop 0.6.0 is available"
	shows := func(want bool, why string) {
		t.Helper()
		if ok := strings.Contains(p.Semantics().String(), title); ok != want {
			t.Errorf("%s: the banner shows: %v", why, ok)
		}
	}
	shows(false, "nothing known")
	m.AppUpdate.Set(AppUpdate{Tag: "v0.6.0", Install: appupdate.Install{Method: appupdate.Bundle}})
	shows(true, "a newer release")
	if _, ok := p.Find("Update"); !ok {
		t.Error("no Update button")
	}
	m.SkipAppVersion()
	shows(false, "skipped")
	if got := ggui.Peek(m.Settings).SkipAppVersion; got != "v0.6.0" {
		t.Errorf("skipped %q", got)
	}
	m.AppUpdate.Set(AppUpdate{Tag: "v0.6.1", Install: appupdate.Install{Method: appupdate.Bundle}})
	if !strings.Contains(p.Semantics().String(), "GVX Desktop 0.6.1 is available") {
		t.Error("a release past the skipped one is not offered")
	}
	m.AppUpdate.Set(AppUpdate{Tag: "v0.5.1", Install: appupdate.Install{Method: appupdate.Bundle}})
	shows(false, "the same version")
	AppVersion = "dev"
	m.AppUpdate.Set(AppUpdate{Tag: "v0.6.0", Install: appupdate.Install{Method: appupdate.Bundle}})
	shows(false, "a development build")
}

func TestAppUpdateActionFollowsTheInstall(t *testing.T) {
	m, p := appUpdateModel(t)
	for _, c := range []struct {
		u    AppUpdate
		verb string
	}{
		{AppUpdate{Install: appupdate.Install{Method: appupdate.Bundle}}, "Update"},
		{AppUpdate{Install: appupdate.Install{Method: appupdate.Program}, Ready: true}, "Restart to update"},
		{AppUpdate{Install: appupdate.Install{Method: appupdate.Homebrew}, Command: "brew upgrade --cask gvx-desktop"}, "Copy command"},
		{AppUpdate{Install: appupdate.Install{Method: appupdate.Deb}}, "Open the release page"}, // no package to name
		{AppUpdate{Install: appupdate.Install{Method: appupdate.Manual}}, "Open the release page"},
	} {
		c.u.Tag = "v0.6.0"
		if got := appUpdateVerb(c.u); got != c.verb {
			t.Errorf("%v: %q", c.u.Install.Method, got)
		}
		m.AppUpdate.Set(c.u)
		p.Frame()
		if _, ok := p.Find(c.verb); !ok {
			t.Errorf("%v: no %q in the window", c.u.Install.Method, c.verb)
		}
	}
	if !strings.Contains(appUpdateText(AppUpdate{Install: appupdate.Install{Method: appupdate.Homebrew}, Command: "brew upgrade --cask gvx-desktop"}), "brew upgrade --cask gvx-desktop") {
		t.Error("Homebrew's banner does not name the command")
	}
}

// The restart that puts the update in place asks first, and says the
// sandboxes running stop; without an update downloaded it does nothing.
func TestAskRestartToUpdate(t *testing.T) {
	m, _ := appUpdateModel(t)
	m.AppUpdate.Set(AppUpdate{Tag: "v0.6.0", Install: appupdate.Install{Method: appupdate.Bundle}})
	m.AskRestartToUpdate()
	if ggui.Peek(m.ConfirmOpen) {
		t.Fatal("asked to restart with nothing downloaded")
	}
	m.AppUpdate.Set(AppUpdate{Tag: "v0.6.0", Install: appupdate.Install{Method: appupdate.Bundle}, Ready: true})
	m.AskRestartToUpdate()
	if !ggui.Peek(m.ConfirmOpen) || ggui.Peek(m.ConfirmLabel) != "Restart to update" || !strings.Contains(ggui.Peek(m.ConfirmText), "0.6.0") {
		t.Errorf("open %v, %q: %q", ggui.Peek(m.ConfirmOpen), ggui.Peek(m.ConfirmLabel), ggui.Peek(m.ConfirmText))
	}
	if !ggui.Peek(m.ConfirmSafe) {
		t.Error("with no sandbox running, restarting destroys nothing")
	}
	// Nothing was staged, so confirming it does not quit.
	quits := 0
	old := quitApp
	quitApp = func() { quits++ }
	t.Cleanup(func() { quitApp = old })
	m.RunConfirmed()
	if quits != 0 {
		t.Error("quit with no update to put in place")
	}
	if err := m.FinishUpdate(); err != nil {
		t.Errorf("FinishUpdate with no update: %v", err)
	}
}

func TestOfflineChecksNothing(t *testing.T) {
	m, _ := appUpdateModel(t)
	m.CheckAppUpdate(false)
	if u := ggui.Peek(m.AppUpdate); u.Checking || u.Tag != "" {
		t.Errorf("an offline model checked: %+v", u)
	}
}

func TestRelaunchArgs(t *testing.T) {
	for _, c := range []struct{ in, want []string }{
		{nil, nil},
		{[]string{"-hidden"}, nil},
		{[]string{"--hidden=true", "-data-dir", "/x"}, []string{"-data-dir", "/x"}},
		{[]string{"-data-dir=/hidden"}, []string{"-data-dir=/hidden"}},
		{[]string{"hidden"}, []string{"hidden"}},
	} {
		if got := relaunchArgs(c.in); !slices.Equal(got, c.want) {
			t.Errorf("relaunchArgs(%q) = %q", c.in, got)
		}
	}
}
