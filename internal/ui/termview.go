package ui

import (
	"image/color"
	"math"
	"runtime"
	"time"
	"unicode/utf8"

	"github.com/ironpark/ggui"
	uitheme "github.com/ironpark/ggui/ui/theme"
	"github.com/ironpark/ggui/widget"
	"github.com/ironpark/gostty"
	"github.com/ironpark/gostty/input"

	"github.com/ironpark/gvx-desktop/internal/term"
	"github.com/ironpark/gvx-desktop/internal/ui/boxdraw"
)

// terminalView draws a terminal session's screen and sends it keyboard,
// mouse and wheel input. It is mako's terminal view, without what mako's
// agents need and a shell in a container does not: images, links, files
// dropped, find. It claims Tab and the Ctrl and Alt chords for the
// program, so a shell completes and Ctrl+C interrupts; ⌘ chords stay with
// the app.
//
// Text reaches it through a hidden TextInput, which drives the platform
// IME: a syllable is shown at the cursor while it is composed and sent
// once committed. The macOS Korean IME composes without marked text: it
// commits each jamo and rewrites the syllable before the caret as the next
// joins it, so the last Hangul syllable committed is held, shown as it is
// composed, until the next key shows it is done; see overHeldSyllable.
type terminalView struct {
	s         *term.Session
	autoFocus bool // take the keyboard on first paint, as a tab just opened does

	focused  bool
	wheelAcc float64   // lines turned, not yet scrolled; see turnLines
	wheelAt  time.Time // the last scroll
	wheelDy  float64   // its turn, kept for the way it went
	blink    time.Time // when the cursor last started a lit phase

	ime     *widget.TextInputWidget
	imeText *ggui.StateValue[string]
	// held are keys pressed while the IME composes, which it may take for
	// itself; see settleHeld.
	held []input.KeyEvent
	// down are the keys sent as keys and not yet let go of: a press of one
	// is a repeat, and its release is reported to a program that asks.
	down map[ggui.KeyboardKey]input.KeyEvent
	// typed are the keys that type text, pressed while the program wants
	// them as keys, waiting for the text they typed; pairing is whether it
	// does, as of this frame's tick. See pairTyped.
	typed   []typedKey
	pairing bool
	// tickText is the editor's text before this frame's IME edits.
	tickText string
	// sent is how much of the editor's text has gone to the program. What
	// was sent stays until nothing can be rewritten, as the IME finds the
	// syllable it rewrites by its place in the text.
	sent int

	// Mouse reporting: a press the program took owns the drag and the
	// release that follow it.
	grabbed   bool
	grabBtn   input.MouseButton
	lastCell  [2]int
	dragMods  ggui.Mods
	selecting bool
	lastDrag  term.Pointer // where a selecting drag last was
	scrolled  time.Time    // when a held drag last scrolled the view

	// From the last layout and paint, in logical pixels.
	pal          termPalette
	focusRing    color.Color
	origin       ggui.Point // the grid's top-left
	rect         ggui.Rect  // the whole widget
	cellW, cellH float64
	cols, rows   int
	runBuf       []byte // the text of a run of cells, kept between paints
	paintMemo    paintMemo
}

const (
	termPad        = 10
	termFontSize   = 12.5
	termLineHeight = 1.35
	cursorRate     = 530 * time.Millisecond
	// wheelLines is how many lines a notch of a mouse wheel scrolls.
	wheelLines = 3
)

func newTerminalView(s *term.Session, autoFocus bool) *terminalView {
	text := ggui.State("")
	return &terminalView{
		s: s, autoFocus: autoFocus,
		ime: widget.TextInput(text), imeText: text,
		pal: darkTerminal, lastCell: [2]int{-1, -1},
	}
}

func (t *terminalView) Layout(c ggui.Constraints, env ggui.Env) ggui.Size {
	th := uitheme.Of(env)
	t.pal = terminalPalette(th)
	t.focusRing = th.Ring
	// The editor is never painted as itself, but PaintCustom wants it laid
	// out first.
	t.ime.Layout(ggui.Loose(c.Max()), env)
	return c.Max()
}

func (t *terminalView) Paint(dst *ggui.Canvas, r ggui.Rect) {
	t.s.SetColors(t.pal.colors())
	t.rect = r
	dst.FillRect(r, t.pal.Bg)
	dst.HitKey(r, t)
	dst.HitPointer(r, t)
	dst.HitCursor(r, ggui.CursorShapeText)
	if t.autoFocus {
		t.autoFocus = false
		dst.RequestFocus(t)
	}
	// What the program copied, with OSC 52.
	if text, ok := t.s.TakeCopied(); ok {
		ggui.CurrentClipboard().Write(text)
	}

	// The platform's mono font and its italic; both fall back to the system
	// fonts for what they lack.
	font := ggui.DefaultMonoFont()
	_, italic := ggui.SystemItalicFonts()
	t.cellW = cellWidth(dst.TextWidth("M", font, termFontSize), dst.Scale())
	t.cellH = math.Ceil(termFontSize * termLineHeight)
	if t.cellW <= 0 {
		return
	}
	t.origin = ggui.Pt(r.Origin.X+termPad, r.Origin.Y+termPad/2)
	t.cols = int((r.Size.W - 2*termPad) / t.cellW)
	t.rows = int((r.Size.H - termPad) / t.cellH)
	if t.cols < 2 || t.rows < 2 {
		return
	}
	// A cell in device pixels, as a program sizes what it draws.
	t.s.Resize(uint16(t.cols), uint16(t.rows), uint16(math.Round(t.cellW*dst.Scale())), uint16(math.Round(t.cellH*dst.Scale())))

	f, err := t.s.Frame()
	if err != nil {
		return
	}
	g := grid{origin: t.origin, cellW: t.cellW, cellH: t.cellH, size: termFontSize, font: font, italic: italic, pal: t.pal, scale: dst.Scale(), runBuf: &t.runBuf, memo: &t.paintMemo}
	clip := dst.Clip(r)
	g.paintBackgrounds(clip, f)
	g.paintText(clip, f)

	caret := g.cellRect(int(f.Cursor.X), int(f.Cursor.Y), 1)
	if !f.Exited {
		// Painted as a widget of its own, so that its blink, which asks
		// for a frame now and then, repaints the cursor alone.
		clip.Paint(ggui.FromFuncs(nil, func(dst *ggui.Canvas, _ ggui.Rect) { t.paintCursor(dst, g, f) }), ggui.Rect{})
	}
	t.ime.PaintCustom(clip, r, func(dst *ggui.Canvas, _ ggui.Rect, st widget.TextInputState) ggui.Rect {
		// A composition, or a syllable held for one; what was sent already
		// shows on the screen.
		if unsent := st.Text[min(t.sent, len(st.Text)):]; unsent != "" {
			g.paintPreedit(dst, caret.Origin, unsent)
		}
		return caret
	})
	if t.focused && t.focusRing != nil {
		strokeRect(dst, r, t.focusRing)
	}
}

// paintCursor draws the cursor in the shape the program asked for, lit or
// dark in its blink. Unfocused, it is an outline that does not blink.
func (t *terminalView) paintCursor(dst *ggui.Canvas, g grid, f term.Frame) {
	c := f.Cursor
	if !c.Visible || !c.ViewportHasValue || int(c.X) >= f.Cols || int(c.Y) >= f.Rows {
		return
	}
	col, width := int(c.X), 1
	if c.WideTail {
		col, width = col-1, 2
	}
	idx := int(c.Y)*f.Cols + col
	if idx >= 0 && idx < len(f.Cells) && f.Cells[idx].Flags.Wide == gostty.CellWidthWide {
		width = 2
	}
	r := g.cellRect(col, int(c.Y), width)
	if !t.focused {
		strokeRect(dst, r, t.pal.Fg)
		return
	}
	if c.Blinking {
		now := ggui.FrameTime()
		if t.blink.IsZero() {
			t.blink = now
		}
		phase := now.Sub(t.blink) / cursorRate
		ggui.WakeAt(t.blink.Add((phase + 1) * cursorRate))
		if phase%2 == 1 {
			return
		}
	}
	switch c.Style {
	case gostty.CursorStyleBar:
		dst.FillRect(ggui.Rect{Origin: r.Origin, Size: ggui.Sz(2, r.Size.H)}, t.pal.Fg)
	case gostty.CursorStyleUnderline:
		dst.FillRect(ggui.Rect{Origin: ggui.Pt(r.Origin.X, r.Origin.Y+r.Size.H-2), Size: ggui.Sz(r.Size.W, 2)}, t.pal.Fg)
	case gostty.CursorStyleBlockHollow:
		strokeRect(dst, r, t.pal.Fg)
	default:
		dst.FillRect(r, t.pal.Fg)
		// The glyph under a block cursor is redrawn in the background colour,
		// except while the program reads a password.
		if idx >= 0 && idx < len(f.Cells) && !c.PasswordInput && !skipGlyph(f.Cells[idx]) {
			g.cellGlyph(dst, f.Cells[idx], f.TextAt(idx), r, t.pal.Bg)
		}
	}
}

// Keyboard --------------------------------------------------------------------

// HandleKey implements ggui.KeyHandler.
func (t *terminalView) HandleKey(ev ggui.KeyEvent) {
	switch ev.Kind {
	case ggui.KeyFocus, ggui.KeyBlur:
		t.focused = ev.Kind == ggui.KeyFocus
		t.s.Focus(t.focused)
		t.blink = time.Time{}
		t.ime.HandleKey(ev)
		t.flushText(true)
		t.releaseAll()
	case ggui.KeyText:
		// The editor commits typed text itself where there is no platform
		// IME, and leaves it to the IME where there is, so text is sent from
		// one place either way.
		t.ime.HandleKey(ev)
		t.pairTyped()
		t.flushText(false)
	case ggui.KeyPress:
		t.blink = time.Time{}
		if t.chord(ev) {
			return
		}
		// A word or line edit goes as the shell reads it, after the text
		// typed before it.
		if e := editKeyOf(ev, runtime.GOOS); e != term.EditNone && !t.ime.EditingState().Composing {
			t.flushText(true)
			if t.s.Edit(e) {
				t.s.ClearSelection()
				return
			}
		}
		if ev.Mods.Meta && runtime.GOOS == "darwin" {
			return // the other ⌘ chords are the app's
		}
		code, ok := terminalKey(ev.Key)
		if !ok || code.Modifier() {
			return
		}
		unshifted, _ := code.Codepoint()
		key := input.KeyEvent{
			Action:             input.KeyActionPress,
			Key:                code,
			Mods:               keyMods(ev.Mods),
			UnshiftedCodepoint: unshifted,
		}
		// A printable key arrives again as text, already through the layout,
		// dead keys and the IME; it is described as a key only when a
		// modifier took its text away, or with the text it typed when the
		// program wants keys that type as keys.
		if code.Printable() && code != input.KeyTab && !ev.Mods.Ctrl && (!ev.Mods.Alt || altMakesText) {
			if t.pairing && !t.ime.EditingState().Composing {
				t.typed = append(t.typed, typedKey{ev.Key, key})
				t.pairTyped()
			}
			return
		}
		if t.ime.EditingState().Composing {
			t.held = append(t.held, key)
			return
		}
		if t.overHeldSyllable(key) {
			return
		}
		t.s.ClearSelection()
		t.sendKey(ev.Key, key, "")
	}
}

// HandleKeyRelease implements ggui.KeyReleaseHandler: a key sent as one is
// reported let go of, to a program that asks.
func (t *terminalView) HandleKeyRelease(ev ggui.KeyEvent) {
	key, ok := t.down[ev.Key]
	if !ok {
		return
	}
	delete(t.down, ev.Key)
	key.Action, key.Mods = input.KeyActionRelease, keyMods(ev.Mods)
	t.s.Key(key, "")
}

// releaseAll lets go of the keys held as focus comes or goes, whose
// releases would go elsewhere.
func (t *terminalView) releaseAll() {
	for k, key := range t.down {
		delete(t.down, k)
		key.Action = input.KeyActionRelease
		t.s.Key(key, "")
	}
}

// sendKey sends key k, which typed text, if any: as a repeat while it is
// held, as a press otherwise.
func (t *terminalView) sendKey(k ggui.KeyboardKey, key input.KeyEvent, text string) {
	if _, held := t.down[k]; held {
		key.Action = input.KeyActionRepeat
	}
	t.s.Key(key, text)
	if t.down == nil {
		t.down = map[ggui.KeyboardKey]input.KeyEvent{}
	}
	key.Action = input.KeyActionPress
	t.down[k] = key
}

// typedKey is a key that typed text, for pairTyped.
type typedKey struct {
	k   ggui.KeyboardKey
	key input.KeyEvent
}

// pairTyped sends the keys in typed with the text they typed, which the
// editor holds unsent: before its press, where the platform IME put it
// there in this frame's tick, or after, as the KeyText that follows. The
// last keys typed the last characters, so they are matched from the end;
// the text before them, the IME's own, goes as text. A syllable the IME
// may still rewrite is no key's, and nor is Hangul: it is the IME's.
func (t *terminalView) pairTyped() {
	if len(t.typed) == 0 || t.ime.EditingState().Composing {
		return
	}
	text := ggui.Peek(t.imeText)
	t.sent = min(t.sent, len(text))
	end := len(text) - len(t.rewritable())
	if end <= t.sent {
		return
	}
	unsent := []rune(text[t.sent:end])
	n := 0
	for n < len(t.typed) && n < len(unsent) && !isHangul(unsent[len(unsent)-1-n]) {
		n++
	}
	if n == 0 {
		return
	}
	keys := t.typed[len(t.typed)-n:]
	t.typed = t.typed[:0]
	t.s.ClearSelection()
	if plain := string(unsent[:len(unsent)-n]); plain != "" {
		t.s.Key(input.KeyEvent{Action: input.KeyActionPress, Key: input.KeyUnidentified}, plain)
	}
	for i, tk := range keys {
		r := unsent[len(unsent)-n+i]
		key := tk.key
		if key.Mods.Shift && r != key.UnshiftedCodepoint {
			key.ConsumedMods.Shift = true
		}
		if key.Mods.Alt && altMakesText && r > 0x7e {
			key.ConsumedMods.Alt = true
		}
		t.sendKey(tk.k, key, string(r))
	}
	t.sent = end
}

func isHangul(r rune) bool {
	return (r >= 0x1100 && r <= 0x11ff) || (r >= 0x3131 && r <= 0x318e) || (r >= 0xac00 && r <= 0xd7a3)
}

// settleHeld decides the keys pressed while the IME composed, once it has
// seen them. One that ended the composition with a commit, as Enter or an
// arrow does, goes to the program after the committed text, so the last
// syllable is not typed after the Enter that ended it. One the IME kept
// composing through, or that took the composition back to nothing, as
// Backspace does, was the IME's own and goes nowhere.
func (t *terminalView) settleHeld() {
	defer func() { t.held = t.held[:0] }()
	if len(t.held) == 0 || t.ime.EditingState().Composing {
		return
	}
	committed := ggui.Peek(t.imeText) != ""
	t.flushText(true)
	if committed {
		t.sendKeys(t.held)
	}
}

// overHeldSyllable handles a key pressed over a syllable held for the IME,
// and reports whether there was one. The key reaches here only when the
// IME passed it on. Backspace takes the syllable back, unless the IME
// rewrote it in the same keystroke; any other key sends the syllable
// first, so Enter runs the line it ends.
func (t *terminalView) overHeldSyllable(key input.KeyEvent) bool {
	rw := t.rewritable()
	if rw == "" {
		return false
	}
	if key.Key == input.KeyBackspace {
		if text := ggui.Peek(t.imeText); text == t.tickText {
			t.imeText.Set(text[:len(text)-len(rw)])
		}
		return true
	}
	t.flushText(true)
	t.sendKeys([]input.KeyEvent{key})
	return true
}

func (t *terminalView) sendKeys(keys []input.KeyEvent) {
	for _, key := range keys {
		t.s.ClearSelection()
		t.s.Key(key, "")
	}
}

// HandleTick implements ggui.TickHandler: it runs the platform IME while
// focused, and sends whatever the IME committed.
//
// While the program wants keys that type text as keys, the text is kept a
// frame for the presses that typed it, which come after the tick; what no
// press took goes at the next tick, as text.
func (t *terminalView) HandleTick() bool {
	t.typed = t.typed[:0] // presses whose text never came, as a dead key's
	if t.pairing {
		t.flushText(false)
	}
	t.tickText = ggui.Peek(t.imeText)
	handled := t.ime.HandleTick()
	t.settleHeld()
	t.pairing = !handled && t.s.NeedsKeys()
	if t.pairing && ggui.Peek(t.imeText) != "" {
		ggui.WakeAt(ggui.Now()) // the next tick sends what no press takes
	} else {
		t.flushText(false)
	}
	t.autoscroll()
	return handled
}

// autoscrollRate is how often a drag held above or below the grid scrolls
// the view by a row.
const autoscrollRate = 50 * time.Millisecond

// autoscroll keeps a selecting drag held past the top or bottom of the grid
// scrolling, as it would were the pointer moving.
func (t *terminalView) autoscroll() {
	pc := t.lastDrag
	if !t.selecting || (pc.Ypos >= 0 && pc.Ypos < pc.Height) {
		return
	}
	now := ggui.FrameTime()
	if now.Sub(t.scrolled) >= autoscrollRate {
		t.scrolled = now
		t.s.SelectDrag(pc, t.dragMods.Alt)
	}
	ggui.WakeAt(t.scrolled.Add(autoscrollRate))
}

// flushText sends text the editor holds once nothing is being composed,
// but for a syllable the IME may still rewrite unless all is set, and
// clears the editor once all of it is sent.
func (t *terminalView) flushText(all bool) {
	text := ggui.Peek(t.imeText)
	if t.ime.EditingState().Composing {
		return
	}
	t.sent = min(t.sent, len(text))
	end := len(text)
	if !all {
		end -= len(t.rewritable())
	}
	if end > t.sent {
		t.s.ClearSelection()
		t.s.Key(input.KeyEvent{Action: input.KeyActionPress, Key: input.KeyUnidentified}, text[t.sent:end])
		t.sent = end
	}
	if t.sent == len(text) && text != "" {
		t.imeText.Set("")
		t.sent = 0
	}
}

// holdsSyllables is whether the IME rewrites syllables it committed, as
// the macOS Korean one does; elsewhere they are composed in marked text.
var holdsSyllables = runtime.GOOS == "darwin"

// rewritable is the Hangul syllable or jamo the editor's text ends in,
// which the Korean IME may still rewrite as the next jamo joins it, or "".
func (t *terminalView) rewritable() string {
	if !holdsSyllables {
		return ""
	}
	text := ggui.Peek(t.imeText)
	r, n := utf8.DecodeLastRuneInString(text)
	if isHangul(r) {
		return text[len(text)-n:]
	}
	return ""
}

// chord runs the terminal's own chords -- paste, copy, select all -- and
// reports whether ev was one.
func (t *terminalView) chord(ev ggui.KeyEvent) bool {
	switch {
	case isPaste(ev):
		t.flushText(true)
		t.s.ClearSelection()
		t.s.Paste(ggui.CurrentClipboard().Read())
	case isCopy(ev):
		if text := t.s.Selection(); text != "" {
			ggui.CurrentClipboard().Write(text)
		}
	case isSelectAll(ev):
		t.s.SelectAll()
	default:
		return false
	}
	return true
}

// ConsumesKey implements ggui.KeyConsumer: every bare key is the program's,
// Escape included, so a dialog behind the terminal does not close on it.
func (t *terminalView) ConsumesKey(ev ggui.KeyEvent) bool { return ev.Kind == ggui.KeyPress }

// ClaimsChord implements ggui.ChordClaimer: Tab, the Ctrl and Alt chords and
// the terminal's own chords reach it ahead of focus traversal and any
// shortcut, so a shell completes on Tab and Ctrl+C interrupts. Copy is
// claimed only while something is selected, so ⌘C otherwise stays the app's.
func (t *terminalView) ClaimsChord(ev ggui.KeyEvent) bool {
	switch {
	case ev.Key == ggui.KeyTab, isPaste(ev), isSelectAll(ev):
		return true
	case isCopy(ev):
		return t.s.Selection() != ""
	case editKeyOf(ev, runtime.GOOS) != term.EditNone:
		return true
	}
	if runtime.GOOS == "darwin" && ev.Mods.Meta {
		return false
	}
	return ev.Mods.Ctrl || ev.Mods.Alt
}

// Pointer ---------------------------------------------------------------------

// HandlePointer implements ggui.PointerHandler. A program that asked for the
// mouse gets presses, drags, motion and the wheel; Shift takes them back for
// selection, as in every terminal. Otherwise a drag selects, two clicks a
// word and three a line, and the wheel scrolls the scrollback.
func (t *terminalView) HandlePointer(ev ggui.PointerEvent) bool {
	pc := t.cellAt(ev.Pos)
	switch ev.Kind {
	case ggui.PointerDown:
		t.dragMods = ev.Mods
		if !ev.Mods.Shift && t.s.MouseReported() {
			btn := mouseButton(ev.Button)
			if t.reportMouse(input.MouseActionPress, btn, true, ev.Pos, ev.Mods, true) {
				t.grabbed, t.grabBtn = true, btn
				return true
			}
		}
		if ev.Button == ggui.MouseButtonLeft {
			t.selecting = true
			t.s.SelectPress(pc)
		}
		return true
	case ggui.PointerDrag:
		if t.grabbed {
			if t.moved(pc) {
				t.reportMouse(input.MouseActionMotion, t.grabBtn, true, ev.Pos, t.dragMods, true)
			}
			return true
		}
		if t.selecting {
			t.lastDrag = pc
			t.s.SelectDrag(pc, t.dragMods.Alt)
		}
		return true
	case ggui.PointerUp:
		if t.grabbed {
			t.grabbed = false
			t.reportMouse(input.MouseActionRelease, t.grabBtn, true, ev.Pos, ev.Mods, false)
			return true
		}
		if t.selecting {
			t.selecting = false
			t.s.SelectRelease(pc)
		}
		return true
	case ggui.PointerMove:
		// Motion without a button is reported only in any-event mode, which
		// the encoder decides from the program's modes. A drag's moves come
		// as PointerDrag too, which reports them with the button held, so
		// they are not reported here as well, buttonless and first.
		if !t.grabbed && !t.selecting && t.s.MouseReported() && t.moved(pc) {
			t.reportMouse(input.MouseActionMotion, input.MouseButtonUnknown, false, ev.Pos, ggui.Mods{}, false)
		}
		return false
	case ggui.PointerScroll:
		dy := t.scrollLines(ev)
		lines := t.turnLines(dy, time.Now())
		if lines == 0 {
			return true
		}
		if !ev.Mods.Shift && t.s.MouseReported() {
			btn := input.MouseButtonFour // up
			n := lines
			if n < 0 {
				btn, n = input.MouseButtonFive, -n
			}
			mev, size := t.mouseEvent(input.MouseActionPress, btn, true, ev.Pos, ev.Mods)
			t.s.Mouse(mev, size, false, n)
			return true
		}
		t.s.Wheel(lines)
		return true
	}
	return false
}

// scrollLines is the lines a scroll event turns: a trackpad's pixels by
// the cell's height, a wheel's notches wheelLines each.
func (t *terminalView) scrollLines(ev ggui.PointerEvent) float64 {
	if ev.ScrollPixels && t.cellH > 0 {
		return ev.Scroll.Y / t.cellH
	}
	return ev.Scroll.Y * wheelLines
}

// A trackpad scrolls by pixels, a cell's worth over a few events, and the
// wheel's lines are counted from them. A scroll moves at its first event,
// though less than a line has turned, owing the rest of that line: waiting
// a line out made every scroll start a beat late. What is left of a scroll
// the other way, or one past wheelGap ago, is let go of, so it neither
// holds a new one back nor adds to it.
const wheelGap = 150 * time.Millisecond

// turnLines is the lines a scroll of dy at now turns, counted from those
// before it.
func (t *terminalView) turnLines(dy float64, now time.Time) int {
	fresh := now.Sub(t.wheelAt) > wheelGap || t.wheelDy*dy < 0
	t.wheelAt, t.wheelDy = now, dy
	if fresh {
		t.wheelAcc = 0
	}
	t.wheelAcc += dy
	lines := int(t.wheelAcc)
	if fresh && lines == 0 && dy != 0 {
		lines = int(math.Copysign(1, dy)) // at once, owing the rest of the line
	}
	t.wheelAcc -= float64(lines)
	return lines
}

// moved reports whether the pointer entered another cell since the last
// report: the protocol has nothing finer.
func (t *terminalView) moved(pc term.Pointer) bool {
	if pc.Col == t.lastCell[0] && pc.Row == t.lastCell[1] {
		return false
	}
	t.lastCell = [2]int{pc.Col, pc.Row}
	return true
}

func (t *terminalView) reportMouse(action input.MouseAction, btn input.MouseButton, hasButton bool, pos ggui.Point, mods ggui.Mods, anyPressed bool) bool {
	ev, size := t.mouseEvent(action, btn, hasButton, pos, mods)
	return t.s.Mouse(ev, size, anyPressed, 1)
}

// mouseEvent is a mouse event at pos, in the terms the program's modes
// encode it in.
func (t *terminalView) mouseEvent(action input.MouseAction, btn input.MouseButton, hasButton bool, pos ggui.Point, mods ggui.Mods) (input.MouseEvent, input.RenderSize) {
	x, cw := wholeCells(pos.X-t.origin.X, t.cellW)
	ev := input.MouseEvent{
		Action: action, Button: btn, HasButton: hasButton, Mods: keyMods(mods),
		X: float32(x + termPad), Y: float32(pos.Y - t.rect.Origin.Y),
	}
	size := input.RenderSize{
		ScreenWidth: uint32(t.rect.Size.W), ScreenHeight: uint32(t.rect.Size.H),
		CellWidth: uint32(cw), CellHeight: uint32(t.cellH),
		PaddingLeft: termPad, PaddingTop: termPad / 2,
	}
	return ev, size
}

// cellAt maps a window position onto the grid, clamped to it.
func (t *terminalView) cellAt(p ggui.Point) term.Pointer {
	x, y := p.X-t.origin.X, p.Y-t.origin.Y
	col, row := 0, 0
	if t.cellW > 0 && t.cellH > 0 {
		col = min(max(int(x/t.cellW), 0), max(t.cols-1, 0))
		row = min(max(int(y/t.cellH), 0), max(t.rows-1, 0))
	}
	xpos, cw := wholeCells(x, t.cellW)
	return term.Pointer{Col: col, Row: row, Xpos: xpos, Ypos: y, CellW: cw, Cols: t.cols, Height: float64(t.rows) * t.cellH}
}

// wholeCells maps x, from the grid's left edge, into cells of a whole
// width, which is what libghostty measures in: a cell is whole in device
// pixels, so on a display of two per point it may be 8.5 points wide.
// Where the pointer sits within a cell, which decides whether a drag takes
// it, survives the rounding.
func wholeCells(x, cellW float64) (xpos, width float64) {
	width = max(math.Round(cellW), 1)
	if cellW <= 0 {
		return x, width
	}
	return x / cellW * width, width
}

func mouseButton(b ggui.MouseButton) input.MouseButton {
	switch b {
	case ggui.MouseButtonRight:
		return input.MouseButtonRight
	case ggui.MouseButtonMiddle:
		return input.MouseButtonMiddle
	}
	return input.MouseButtonLeft
}

// Grid painting -----------------------------------------------------------------

// grid is the geometry and style one paint draws a screen with.
type grid struct {
	origin       ggui.Point
	cellW, cellH float64
	size         float64
	font, italic *ggui.Font // italic is nil where there is none
	pal          termPalette
	scale        float64 // device pixels per logical pixel
	runBuf       *[]byte
	memo         *paintMemo
}

// cellWidth is a cell as wide as the font's advance, rounded to whole device
// pixels as Ghostty does, so that every column starts on a pixel and fills
// in neighbouring cells meet. Glyphs are placed on the cells, not at the
// font's own advance, so a row does not drift from the grid.
func cellWidth(advance, scale float64) float64 {
	if advance <= 0 || scale <= 0 {
		return advance
	}
	return max(math.Round(advance*scale), 1) / scale
}

// cellRect is the rect of width cells at (col, row), its edges on device
// pixels. A cell's right edge is its neighbour's left, rounded the same
// way, so fills in adjacent cells meet without a seam or an overlap.
func (g grid) cellRect(col, row, width int) ggui.Rect {
	x0, x1 := g.snap(g.origin.X+float64(col)*g.cellW), g.snap(g.origin.X+float64(col+width)*g.cellW)
	y0, y1 := g.snap(g.origin.Y+float64(row)*g.cellH), g.snap(g.origin.Y+float64(row+1)*g.cellH)
	return ggui.Rect{Origin: ggui.Pt(x0, y0), Size: ggui.Sz(x1-x0, y1-y0)}
}

func (g grid) snap(v float64) float64 { return boxdraw.Snap(g.scale, v) }

// textTop is how far below a cell's top its text is drawn, centring the
// font's line in the cell.
func (g grid) textTop() float64 { return (g.cellH - g.size*1.2) / 2 }

func (g grid) paintBackgrounds(dst *ggui.Canvas, f term.Frame) {
	for row := 0; row < f.Rows; row++ {
		base := row * f.Cols
		if base+f.Cols > len(f.Cells) {
			break
		}
		line := f.Cells[base : base+f.Cols]
		// Backgrounds in runs: a row is usually one colour.
		for start := 0; start < f.Cols; {
			bg, def := g.background(line[start], f.Colors)
			end := start + 1
			for end < f.Cols {
				if b, _ := g.background(line[end], f.Colors); b != bg {
					break
				}
				end++
			}
			if !def {
				dst.FillRect(g.cellRect(start, row, end-start), g.memo.color(bg))
			}
			start = end
		}
	}
}

func (g grid) paintText(dst *ggui.Canvas, f term.Frame) {
	for row := 0; row < f.Rows; row++ {
		base := row * f.Cols
		if base+f.Cols > len(f.Cells) {
			break
		}
		line := f.Cells[base : base+f.Cols]

		// Glyphs in runs of one style. A run breaks at anything that is not
		// plain ASCII, which is drawn in its own cell so a wide character or
		// a fallback font's advance cannot push the rest of the row off grid.
		for start := 0; start < f.Cols; {
			cell := line[start]
			if skipGlyph(cell) {
				start++
				continue
			}
			fg := g.foreground(cell, f.Colors)
			rect := g.cellRect(start, row, 1)
			if f.Clusters[base+start] != "" || cell.Codepoint > 0x7e {
				g.cellGlyph(dst, cell, f.Clusters[base+start], rect, fg)
				g.decorate(dst, cell, start, row, fg)
				start++
				continue
			}
			end := start + 1
			for end < f.Cols && sameRun(cell, line[end], f.Clusters[base+end], fg, g.foreground(line[end], f.Colors)) {
				end++
			}
			b := (*g.runBuf)[:0]
			for _, c := range line[start:end] {
				if c.Codepoint <= ' ' || c.Flags.Invisible {
					b = append(b, ' ')
				} else {
					b = append(b, byte(c.Codepoint)) // the run is ASCII
				}
			}
			*g.runBuf = b
			g.glyph(dst, g.memo.run(b), rect.Origin, fg, cell.Flags.Bold, cell.Flags.Italic)
			for i := start; i < end; i++ {
				g.decorate(dst, line[i], i, row, fg)
			}
			start = end
		}
	}
}

// sameRun reports whether next can be drawn in the same call as the run
// that cell started: plain ASCII, or a blank, in the same style.
func sameRun(cell, next gostty.RenderCell, cluster string, fg, nextFg color.RGBA) bool {
	if next.Codepoint > 0x7e || cluster != "" || next.Flags.Invisible {
		return false
	}
	if next.Codepoint <= ' ' {
		return true // a blank draws nothing, whatever its colour
	}
	return next.Flags.Bold == cell.Flags.Bold && next.Flags.Italic == cell.Flags.Italic && nextFg == fg &&
		next.Flags.Underline == cell.Flags.Underline && next.Flags.Strikethrough == cell.Flags.Strikethrough
}

// background is a cell's fill, and whether it is the default the widget
// already painted. A selected cell takes the selection colour.
func (g grid) background(c gostty.RenderCell, colors gostty.RenderColors) (color.RGBA, bool) {
	if c.Flags.Selected {
		return g.pal.Selection, false
	}
	if c.Bg == colors.Background {
		return g.pal.Bg, true
	}
	return color.RGBA{R: c.Bg.R(), G: c.Bg.G(), B: c.Bg.B(), A: 0xff}, false
}

func (g grid) foreground(c gostty.RenderCell, colors gostty.RenderColors) color.RGBA {
	fg := g.pal.Fg
	if c.Fg != colors.Foreground {
		fg = color.RGBA{R: c.Fg.R(), G: c.Fg.G(), B: c.Fg.B(), A: 0xff}
	}
	if c.Flags.Faint {
		bg := g.pal.Bg
		fg = color.RGBA{R: uint8((int(fg.R) + int(bg.R)) / 2), G: uint8((int(fg.G) + int(bg.G)) / 2), B: uint8((int(fg.B) + int(bg.B)) / 2), A: 0xff}
	}
	return fg
}

// cellGlyph draws one cell's character: as shapes if it is a box or block
// character, else as text, its grapheme cluster when it has one.
func (g grid) cellGlyph(dst *ggui.Canvas, c gostty.RenderCell, cluster string, rect ggui.Rect, fg color.RGBA) {
	if boxdraw.Draw(dst, c.Codepoint, rect, g.memo.color(fg)) {
		return
	}
	text := cluster
	if text == "" {
		text = g.memo.rune(c.Codepoint)
	}
	g.glyph(dst, text, rect.Origin, fg, c.Flags.Bold, c.Flags.Italic)
}

// glyph draws text from a cell's top-left in the mono face, a grapheme to a
// cell: bold along its weight axis, italic in the platform's italic mono
// where there is one.
func (g grid) glyph(dst *ggui.Canvas, s string, at ggui.Point, fg color.RGBA, bold, italic bool) {
	font := g.font
	if italic && g.italic != nil {
		font = g.italic
	}
	st := ggui.TextStyle{Font: font, Size: g.size, Color: g.memo.color(fg)}
	if bold {
		st.Weight = ggui.WeightBold
	}
	dst.DrawTextCells(s, st, ggui.Pt(at.X, at.Y+g.textTop()), g.cellW)
}

func (g grid) decorate(dst *ggui.Canvas, c gostty.RenderCell, col, row int, fg color.RGBA) {
	width := 1
	if c.Flags.Wide == gostty.CellWidthWide {
		width = 2
	}
	r := g.cellRect(col, row, width)
	if c.Flags.Underline != gostty.UnderlineNone {
		dst.FillRect(ggui.Rect{Origin: ggui.Pt(r.Origin.X, r.Origin.Y+r.Size.H-3), Size: ggui.Sz(r.Size.W, 1)}, g.memo.color(fg))
	}
	if c.Flags.Strikethrough {
		dst.FillRect(ggui.Rect{Origin: ggui.Pt(r.Origin.X, r.Origin.Y+r.Size.H/2), Size: ggui.Sz(r.Size.W, 1)}, g.memo.color(fg))
	}
}

// paintPreedit shows the IME's composition at the cursor: on a raised
// surface, underlined, so it reads as not yet sent.
func (g grid) paintPreedit(dst *ggui.Canvas, at ggui.Point, text string) {
	x := at.X
	for _, r := range text {
		w := g.cellW
		if n, err := gostty.CodepointWidth(r); err == nil && n == 2 {
			w *= 2
		}
		dst.FillRect(ggui.Rect{Origin: ggui.Pt(x, at.Y), Size: ggui.Sz(w, g.cellH)}, g.pal.Raised)
		dst.DrawTextStyle(string(r), ggui.TextStyle{Font: g.font, Size: g.size, Color: g.pal.Fg}, ggui.Pt(x, at.Y+g.textTop()))
		dst.FillRect(ggui.Rect{Origin: ggui.Pt(x, at.Y+g.cellH-2), Size: ggui.Sz(w, 1)}, g.pal.Fg)
		x += w
	}
}

// skipGlyph reports a cell with nothing to draw: a blank, the spacer behind
// a wide character, or hidden text.
func skipGlyph(c gostty.RenderCell) bool {
	return c.Codepoint <= ' ' || c.Flags.Invisible ||
		c.Flags.Wide == gostty.CellWidthSpacerTail || c.Flags.Wide == gostty.CellWidthSpacerHead
}

func strokeRect(dst *ggui.Canvas, r ggui.Rect, c color.Color) {
	x, y, w, h := r.Origin.X, r.Origin.Y, r.Size.W, r.Size.H
	dst.FillRect(ggui.Rect{Origin: ggui.Pt(x, y), Size: ggui.Sz(w, 1)}, c)
	dst.FillRect(ggui.Rect{Origin: ggui.Pt(x, y+h-1), Size: ggui.Sz(w, 1)}, c)
	dst.FillRect(ggui.Rect{Origin: ggui.Pt(x, y), Size: ggui.Sz(1, h)}, c)
	dst.FillRect(ggui.Rect{Origin: ggui.Pt(x+w-1, y), Size: ggui.Sz(1, h)}, c)
}

// paintMemo is what a terminal's paint keeps from paint to paint so as not
// to make it again for every cell: its colors boxed as color.Color, which
// a color.RGBA passed as one is each time, the text of its runs of ASCII,
// and its characters as strings. A screen paints mostly what it painted
// last time, so nearly every lookup finds what it needs. It belongs to the
// UI goroutine.
type paintMemo struct {
	lastRGBA  color.RGBA
	lastColor color.Color
	colors    map[color.RGBA]color.Color
	runs      map[string]string
	runes     map[rune]string
}

// mostMemoized is how many of each a paintMemo keeps before starting
// over: enough for a screen of runs, little next to the screen itself.
const mostMemoized = 4096

// color returns c boxed, boxing it only the first time.
func (m *paintMemo) color(c color.RGBA) color.Color {
	if m.lastColor != nil && c == m.lastRGBA {
		return m.lastColor
	}
	boxed, ok := m.colors[c]
	if !ok {
		if m.colors == nil || len(m.colors) >= mostMemoized {
			m.colors = make(map[color.RGBA]color.Color)
		}
		boxed = c
		m.colors[c] = boxed
	}
	m.lastRGBA, m.lastColor = c, boxed
	return boxed
}

// run returns b as a string, which the text caches may keep as a key, so
// it is never b's bytes: the same string as last time when b reads the same.
func (m *paintMemo) run(b []byte) string {
	if s, ok := m.runs[string(b)]; ok {
		return s
	}
	if m.runs == nil || len(m.runs) >= mostMemoized {
		m.runs = make(map[string]string)
	}
	s := string(b)
	m.runs[s] = s
	return s
}

// rune returns r as a string, made once.
func (m *paintMemo) rune(r rune) string {
	if s, ok := m.runes[r]; ok {
		return s
	}
	if m.runes == nil || len(m.runes) >= mostMemoized {
		m.runes = make(map[rune]string)
	}
	s := string(r)
	m.runes[r] = s
	return s
}
