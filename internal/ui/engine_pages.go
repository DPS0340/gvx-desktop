package ui

import (
	"image/color"
	"slices"
	"time"

	"github.com/ironpark/ggui"
	"github.com/ironpark/ggui/icons/lucide"
	"github.com/ironpark/ggui/ui"
	"github.com/ironpark/ggui/ui/icons"
	"github.com/ironpark/ggui/widget"

	"github.com/ironpark/gvx-desktop/internal/sandbox"
)

// What the Containers and the Pods pages have in common: each shows what
// runs inside a recipe's engine (the Docker Engine's containers, the k3s
// cluster's pods), only while that engine's sandbox runs, as a table with
// the details of the chosen row and its logs beneath.

// The stages of a recipe's engine, as ContainersAccess and PodsAccess
// count them: it runs, it is not set up, its sandbox is starting, it is
// stopped/stopping/exited/failed, or it is paused.
const (
	engineOpen = iota
	engineNotSetUp
	engineStarting
	engineStopped
	enginePaused
)

// engineStage reads from the sandboxes how far the recipe's engine is from
// running.
func engineStage(rows []Row, recipe string) int {
	for _, r := range rows {
		if r.Spec.Recipe != recipe {
			continue
		}
		switch {
		case r.State.Status == sandbox.Running && r.State.Paused:
			return enginePaused
		case r.State.Status == sandbox.Running:
			return engineOpen
		case r.State.Status == sandbox.Starting:
			return engineStarting
		}
		return engineStopped
	}
	return engineNotSetUp
}

// engineGate is what a page says of an engine that is not running: how
// far it is from running, whether it is stopping, and its sandbox ID.
type engineGate struct {
	stage    int
	stopping bool
	id       string
}

// gateOf follows a recipe's engine for a page that stands aside while it
// does not run.
func gateOf(m *Model, recipe string, stage ggui.Readable[int]) ggui.Readable[engineGate] {
	return ggui.Combine(stage, recipeRow(m, recipe), func(s int, r Row) engineGate {
		return engineGate{s, r.State.Status == sandbox.Stopping, r.Spec.ID}
	})
}

// gateWords are a page's words for an engine that is not running.
type gateWords struct {
	recipe, page string       // the recipe, and its page
	icon         *icons.Glyph // what the page lists
	openPage     string       // the button that opens the recipe's page
	notSetUp     [2]string    // the title and text while it is not set up
	starting     [2]string    // ... while it starts
	stopped      [2]string    // ... while it is stopped
	paused       [2]string    // ... while it is paused
	start        string       // the button that starts it
	resume       string       // the button that resumes it when paused
}

// engineNotRunning stands in for a page's table while the engine does not
// run, with what would start or resume it.
func engineNotRunning(m *Model, g engineGate, w gateWords) ggui.Widget {
	media := glyph(w.icon, 32).Color(colorGray)
	start := func(label string) *ui.ButtonWidget {
		return ui.Button(label, func() { m.CreateFromRecipe(w.recipe) }).Icon(glyph(lucide.Play, 16))
	}
	open := ui.Button(w.openPage, func() { m.Go(w.page) }).Outline()
	switch g.stage {
	case engineNotSetUp:
		return ui.Empty(w.notSetUp[0], w.notSetUp[1]).
			Media(media).Action(widget.Row(start(tr("Set up and start")), open).Gap(8)).Outline()
	case engineStarting:
		return ui.Empty(w.starting[0], w.starting[1]).
			Media(ui.Spinner().Size(28).Color(colorAmber)).Action(open).Outline()
	case enginePaused:
		resume := ui.Button(w.resume, func() { m.ResumeSandbox(g.id) }).Icon(glyph(lucide.Play, 16))
		return ui.Empty(w.paused[0], w.paused[1]).
			Media(media).Action(widget.Row(resume, open).Gap(8)).Outline()
	}
	return ui.Empty(w.stopped[0], w.stopped[1]).
		Media(media).Action(widget.Row(start(w.start).Disabled(g.stopping), open).Gap(8)).Outline()
}

// stateLabel is a row's state as the sandboxes' is shown: a dot of its
// color, or a spinner while it is changing, and its words, with more in a
// tip when there is more to say.
func stateLabel(col color.Color, text string, changing bool, more string) ggui.Widget {
	var mark ggui.Widget = widget.Box(dot(col, 8)).Pad(2)
	if changing {
		mark = ui.Spinner().Size(12).Color(col)
	}
	label := widget.Row(mark, widget.Text(text).Size(13).NoWrap().Ellipsis()).Gap(8).Align(widget.AlignCenter)
	if more == "" {
		return label
	}
	return tip(label, more)
}

// detailHead is the head of the chosen row's details: its name and state,
// a line about it, and its actions, which go under the rest when the
// card is narrow.
func detailHead(title string, state, meta ggui.Widget, actions ...ggui.Widget) ggui.Widget {
	head := widget.Column(
		widget.Row(widget.Flexible(ui.Title(title).Size(20).NoWrap().Ellipsis()), state).Gap(12).Align(widget.AlignCenter),
		meta,
	).Gap(4)
	return widget.Responsive(680,
		widget.Row(widget.Expanded(head), widget.Row(actions...).Gap(8)).Gap(16).Align(widget.AlignCenter),
		widget.Column(head, widget.Wrap(actions...).Gap(8)).Gap(12).Stretch(),
	)
}

// engineLogsView shows the last lines a container wrote, read from its
// engine: the lines, with what reading them says beneath, caption when
// all went well. empty is said when there are none, and actions join the
// viewer's toolbar.
func engineLogsView(m *Model, logs ggui.Readable[ContainerLogs], caption, empty string, actions ...ggui.Widget) ggui.Widget {
	lines := ggui.Map(logs, func(l ContainerLogs) []sandbox.LogLine { return l.Lines }).WithEqual(nil)
	status := ggui.View(ggui.Map(logs, func(l ContainerLogs) string {
		switch {
		case l.Err != "":
			return l.Err
		case !l.Loaded:
			return "…"
		}
		return ""
	}), func(s string) ggui.Widget {
		switch s {
		case "":
			return ui.Caption(caption).Size(12)
		case "…":
			return widget.Row(ui.Spinner().Size(12), ui.Caption(tr("Reading logs…")).Size(12)).Gap(8).Align(widget.AlignCenter)
		}
		return ui.Alert(tr("Could not read the logs"), s).Destructive().Icon(icons.Alert)
	})
	return widget.Column(logViewer(m, lines, empty, actions...), status).Gap(8).Stretch()
}

// runningBadge is how many of a sidebar item's things run, or nothing for
// none: a small green dot, as the lists mark what runs, and the number in
// the muted color, which reads as a state rather than as news waiting to be
// read, with the words in a tip.
// words says it for n, with trn.
func runningBadge(n ggui.Readable[int], words func(n int) string) ggui.Widget {
	return ggui.View(n, func(n int) ggui.Widget {
		if n == 0 {
			return widget.Box()
		}
		text := itoa(n)
		if n > 99 {
			text = "99+"
		}
		// A status dot and the number in the muted color, as modern sidebars
		// count: no fill to read as news. The sidebar gives an action the room
		// of a badge, 20 points wide, which two digits still fit.
		count := widget.Row(
			dot(colorGreen, 5),
			widget.Text(text).Size(11).Weight(ggui.WeightMedium).Color(colorGray).NoWrap(),
		).Gap(3).Align(widget.AlignCenter)
		return tip(count, words(n))
	})
}

// gatedItem is a sidebar item that leads into a recipe's engine: greyed
// out while the engine is not running, with a mark at its end that says
// why in a tip, as the sidebar tells the item's own Tooltip only on a
// rail of icons; and how many of its things run while it is, if count is
// not nil (runningBadge, with words).
func gatedItem(item ui.SidebarEntry, why string, count ggui.Readable[int], words func(int) string) ui.SidebarEntry {
	if why != "" {
		return item.Disabled(true).Tooltip(why).
			Action(tip(widget.Box(glyph(lucide.PowerOff, 14).Color(colorGray)).Pad(3), why))
	}
	if count == nil {
		return item
	}
	return item.Action(runningBadge(count, words))
}

// poll runs load at once whenever every changes, as when an engine has
// just come up or stopped or a page that shows what it runs opens, and
// tick every so often after, while every is not zero.
func (m *Model) poll(every ggui.Readable[time.Duration], load, tick func()) {
	ggui.Effect(func() ggui.Cleanup {
		d := every.Get()
		m.post(load)
		if d == 0 {
			return nil
		}
		stop := make(chan struct{})
		go func() {
			t := time.NewTicker(d)
			defer t.Stop()
			for {
				select {
				case <-stop:
					return
				case <-t.C:
					m.post(tick)
				}
			}
		}()
		return func() { close(stop) }
	})
}

// followLogs reads a container's logs when they come on screen, and when
// another's take their place: shown is the one on screen, or "" for none.
// With no container left to show, as when the engine stops, the lines go.
func (m *Model) followLogs(shown, id ggui.Readable[string], load func(), logs *ggui.StateValue[ContainerLogs]) {
	ggui.Watch(shown, func(shown string) {
		if shown != "" {
			load()
		}
	})
	ggui.Watch(id, func(id string) {
		if id == "" {
			logs.Set(ContainerLogs{})
		}
	})
}

// recipePageInterval is how often what runs in a recipe's engine is read
// while the engine runs: often while a page shows it, now and then for the
// menus and the sidebar otherwise, and not at all while it is stopped.
func (m *Model) recipePageInterval(recipe string, pages ...string) time.Duration {
	row, ok := m.RecipeSandbox(m.Rows.Get(), recipe)
	if !ok || row.State.Status != sandbox.Running || row.State.Paused {
		return 0
	}
	page := m.Page.Get()
	if slices.Contains(pages, page) {
		return 3 * time.Second
	}
	return 10 * time.Second
}
