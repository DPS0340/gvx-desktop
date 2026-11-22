package ui

import (
	"errors"
	"sync"

	"github.com/ironpark/ggui"
	"github.com/ironpark/ggui/icons/lucide"
	"github.com/ironpark/ggui/ui"
	"github.com/ironpark/ggui/ui/icons"
	"github.com/ironpark/ggui/widget"

	"github.com/ironpark/gvx-desktop/internal/term"
)

// The Containers and Pods pages run a shell in the chosen container in a
// tab of its details. The shell starts as the tab opens, and is ended,
// with whatever it runs, as the tab closes: another tab or page is chosen,
// the container stops or goes away, or the app quits. A shell that ends
// on its own leaves its screen as it was, with a way to start another.

// shellTarget is what a terminal tab runs a shell in.
type shellTarget struct {
	// Key names the shell: one runs while the key stays, and a new key
	// starts a new one. Empty, none runs.
	Key string
	// TargetID identifies the sandbox or container ("sandbox:<id>" or
	// "container:<id>") so pausing it can close and sweep active shells
	// before freezing the guest.
	TargetID string
	// Why and WhyText say why no shell can run, when one cannot.
	Why, WhyText string
	// Start starts the shell on a terminal cols by rows, off the UI thread.
	// An unavailable error says why it cannot.
	Start func(cols, rows uint16) (term.Program, error)
}

func (t shellTarget) Equal(o shellTarget) bool {
	return t.Key == o.Key && t.TargetID == o.TargetID && t.Why == o.Why && t.WhyText == o.WhyText
}

// unavailable is why a shell cannot start, as a terminal tab says it.
type unavailable struct{ title, text string }

func (u unavailable) Error() string { return u.title }

// terminalHeight is how tall a terminal tab's screen is.
const terminalHeight = 420

// shellRun is a shell to run: its target, and how many times it was
// restarted.
type shellRun struct {
	target   shellTarget
	restarts int
}

func (r shellRun) Equal(o shellRun) bool { return r.target.Equal(o.target) && r.restarts == o.restarts }

// terminalTab is a tab's terminal: a shell in target, with tools beside
// what it runs and a way to start it again.
func terminalTab(m *Model, target ggui.Readable[shellTarget], caption string, tools ...ggui.Widget) ggui.Widget {
	restarts := ggui.State(0)
	restart := func() { restarts.Set(ggui.Peek(restarts) + 1) }
	run := ggui.Combine(target, restarts, func(t shellTarget, n int) shellRun { return shellRun{t, n} }).WithEqual(shellRun.Equal)
	body := ggui.View(run, func(r shellRun) ggui.Widget {
		t := r.target
		switch {
		case t.Why != "":
			return terminalEmpty(t.Why, t.WhyText)
		case t.Key == "" || t.Start == nil:
			return widget.Box()
		}
		return shellView(m, t, restart)
	})
	bar := append([]ggui.Widget{widget.Expanded(ui.Caption(caption).Size(12)),
		tip(iconButton(lucide.RotateCcw, tr("Restart the shell"), restart), tr("Restart"))}, tools...)
	return widget.Column(widget.Row(bar...).Gap(6).Align(widget.AlignCenter), body).Gap(10).Stretch()
}

func terminalEmpty(title, text string) ggui.Widget {
	return ui.Empty(title, text).Media(glyph(lucide.SquareTerminal, 32).Color(colorGray)).Outline()
}

// shellState is where a shell is: starting, failed, running, or exited.
type shellState struct {
	s      *term.Session
	err    error
	exited bool
	code   int
}

func (a shellState) Equal(b shellState) bool {
	return a.s == b.s && a.exited == b.exited && a.code == b.code && a.err == b.err
}

// shellView starts a shell in t and shows it, ending it when it is
// disposed of.
func shellView(m *Model, t shellTarget, restart func()) ggui.Widget {
	st := ggui.State(shellState{}).WithEqual(shellState.Equal)
	var (
		sess     *term.Session
		disposed bool // both belong to the UI thread
	)
	ggui.OnCleanup(func() {
		disposed = true
		if sess != nil {
			m.closeTerminal(sess)
		}
	})
	start := t.Start
	go func() {
		prog, err := start(80, 24)
		m.post(func() {
			if disposed {
				if prog != nil {
					go func() { prog.Kill(); _ = prog.Close(); prog.Wait() }()
				}
				return
			}
			if err != nil {
				st.Set(shellState{err: err})
				return
			}
			s, err := term.New(prog, term.Options{Cols: 80, Rows: 24, Wake: m.repaint})
			if err != nil {
				st.Set(shellState{err: err})
				return
			}
			sess = s
			m.openTerminal(s, t.TargetID)
			st.Set(shellState{s: s})
			go func() {
				<-s.Done()
				_, code := s.Exited()
				m.post(func() {
					if !disposed {
						st.Set(shellState{s: s, exited: true, code: code})
					}
				})
			}()
		})
	}()

	screen := ggui.View(ggui.Map(st, func(s shellState) shellState { return shellState{s: s.s, err: s.err} }).WithEqual(shellState.Equal), func(s shellState) ggui.Widget {
		var u unavailable
		switch {
		case errors.As(s.err, &u):
			return terminalEmpty(u.title, u.text)
		case s.err != nil:
			return widget.Column(
				ui.Alert(tr("Could not start a shell"), s.err.Error()).Destructive().Icon(icons.Alert),
				widget.Row(ui.Button(tr("Retry"), restart).Outline().Size(ui.ButtonSmall)),
			).Gap(10).Stretch()
		case s.s == nil:
			return widget.Box(widget.Center(widget.Row(ui.Spinner().Size(14), ui.Caption(tr("Starting a shell…"))).Gap(8).Align(widget.AlignCenter))).
				Height(terminalHeight).Radius(8).Border(1, colorBorder)
		}
		return widget.Box(newTerminalView(s.s, true)).Height(terminalHeight).Radius(8).Border(1, colorBorder).Clip()
	})
	ended := ggui.View(ggui.Map(st, func(s shellState) shellState { return shellState{exited: s.exited, code: s.code} }).WithEqual(shellState.Equal), func(s shellState) ggui.Widget {
		if !s.exited {
			return widget.Box()
		}
		text := tr("The shell ended.")
		if s.code >= 0 {
			text = tr("The shell ended (exit %d).", s.code)
		}
		return widget.Row(
			glyph(lucide.SquareTerminal, 14).Color(colorGray),
			widget.Expanded(ui.Caption(text).Size(12)),
			ui.Button(tr("Restart"), restart).Outline().Size(ui.ButtonSmall).Icon(glyph(lucide.RotateCcw, 14)),
		).Gap(8).Align(widget.AlignCenter)
	})
	return widget.Column(screen, ended).Gap(8).Stretch()
}

// liveTerminals are the shells running in the app's terminals, which the
// app ends as it quits, plus marks whose sweep was skipped because the
// target was already paused (swept on the next resume/unpause).
type liveTerminals struct {
	mu            sync.Mutex
	set           map[*term.Session]string
	pendingSweeps map[string][]string
}

// openTerminal notes a shell started for targetID.
func (m *Model) openTerminal(s *term.Session, targetID string) {
	m.terms.mu.Lock()
	defer m.terms.mu.Unlock()
	if m.terms.set == nil {
		m.terms.set = map[*term.Session]string{}
	}
	m.terms.set[s] = targetID
}

// closeTerminal ends a shell, in the background: ending one in a container
// asks its engine to.
func (m *Model) closeTerminal(s *term.Session) {
	go func() {
		s.Close()
		m.terms.mu.Lock()
		delete(m.terms.set, s)
		m.terms.mu.Unlock()
	}()
}

// closeTerminalsFor closes every active shell targeting targetID and waits for
// them (and their sweeps) to finish. Call off the UI thread before pausing the
// target so sweeps run while the guest can still exec.
func (m *Model) closeTerminalsFor(targetID string) {
	if targetID == "" {
		return
	}
	m.terms.mu.Lock()
	var list []*term.Session
	for s, tid := range m.terms.set {
		if tid == targetID {
			list = append(list, s)
			delete(m.terms.set, s)
		}
	}
	m.terms.mu.Unlock()
	var wg sync.WaitGroup
	for _, s := range list {
		wg.Go(func() { s.Close() })
	}
	wg.Wait()
}

// recordPendingSweep remembers a shell mark whose sweep failed because
// targetID was paused, so it can be swept once targetID resumes.
func (m *Model) recordPendingSweep(targetID, mark string) {
	if targetID == "" || mark == "" {
		return
	}
	m.terms.mu.Lock()
	defer m.terms.mu.Unlock()
	if m.terms.pendingSweeps == nil {
		m.terms.pendingSweeps = map[string][]string{}
	}
	m.terms.pendingSweeps[targetID] = append(m.terms.pendingSweeps[targetID], mark)
}

// takePendingSweeps pops and returns any deferred sweep marks for targetID.
func (m *Model) takePendingSweeps(targetID string) []string {
	if targetID == "" {
		return nil
	}
	m.terms.mu.Lock()
	defer m.terms.mu.Unlock()
	marks := m.terms.pendingSweeps[targetID]
	delete(m.terms.pendingSweeps, targetID)
	return marks
}

// CloseTerminals ends every shell the app's terminals run, and waits for
// them to end. The app calls it as it quits.
func (m *Model) CloseTerminals() {
	m.terms.mu.Lock()
	list := make([]*term.Session, 0, len(m.terms.set))
	for s := range m.terms.set {
		list = append(list, s)
	}
	m.terms.mu.Unlock()
	var wg sync.WaitGroup
	for _, s := range list {
		wg.Go(func() { s.Close() })
	}
	wg.Wait()
}

// repaint asks for the window to be painted again, from any goroutine, for
// output a terminal took in, which no signal records: work posted alone
// repaints only what its signals reach.
func (m *Model) repaint() {
	if app, ok := m.host.(interface{ Windows() []*ggui.Window }); ok {
		for _, w := range app.Windows() {
			w.Repaint()
		}
		return
	}
	m.post(func() {}) // a probe paints each frame whole
}
