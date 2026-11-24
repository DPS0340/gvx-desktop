package term

import (
	"bytes"
	"errors"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ironpark/gostty"
	"github.com/ironpark/gostty/input"
)

// Session is a program on a terminal: the program, and the terminal that
// parses its output, answers its queries and encodes what is typed into
// it. Its methods are safe from any goroutine; Frame belongs to whoever
// paints it.
type Session struct {
	prog Program
	wake func() // asks for a paint; see requestPaint

	mu       sync.Mutex // guards everything below, the terminal included
	vt       *gostty.Session
	state    *gostty.RenderState
	gesture  *gostty.Gesture // click counting and drag selection
	dragging bool
	cells    []gostty.RenderCell
	clusters []string
	dirty    []uint16
	hasFrame bool // cells and clusters hold a complete screen
	closed   bool // vt and state are released; nothing may touch them
	exited   bool
	exitCode int
	title    string
	copied   *string // what the program last put on the clipboard, until taken
	colors   Colors
	cols     uint16
	rows     uint16
	cellW    uint16 // a cell in device pixels, zero until a view paints it
	cellH    uint16
	focused  bool // what the program was last told of focus
	// The program is told of a size once a burst of them settles, as a
	// sliding panel resizes the terminal every frame, so it is not made to
	// redraw for each one. sentAt is when it was last told; settle tells
	// it the latest size later.
	sentAt time.Time
	settle *time.Timer

	pending  atomic.Bool // a paint was asked for and has not copied the screen
	done     chan struct{}
	replies  chan []byte
	resizeCh chan struct{}
	closing  sync.Once
}

// Options are how a session's terminal starts.
type Options struct {
	Cols, Rows uint16
	// Scrollback is how many lines scrolled off the top are kept; zero
	// keeps DefaultScrollback.
	Scrollback uint
	// Wake asks for the terminal to be painted again, from any goroutine.
	// Output asks for it in bursts, coalesced until a paint has copied
	// the screen.
	Wake func()
}

// DefaultScrollback is how many lines a terminal keeps above its screen.
const DefaultScrollback = 10000

// New runs prog on a fresh terminal, reading its output until it exits.
// The session owns prog: Close ends it.
func New(prog Program, o Options) (*Session, error) {
	cols, rows := max(o.Cols, 2), max(o.Rows, 2)
	scrollback := o.Scrollback
	if scrollback == 0 {
		scrollback = DefaultScrollback
	}
	vt, err := gostty.New(cols, rows,
		gostty.WithTerminalOptions(gostty.WithScrollbackMaxLines(scrollback)),
		gostty.WithStreamOptions(gostty.WithVersionReport("gvx-desktop", "")),
	)
	if err != nil {
		prog.Kill()
		_ = prog.Close()
		return nil, err
	}
	state, err := gostty.NewRenderState()
	if err != nil {
		vt.Close()
		prog.Kill()
		_ = prog.Close()
		return nil, err
	}
	gesture := vt.Terminal().NewGesture()
	vt.AddGesture(gesture) // closed with the terminal, and before it
	_ = gesture.SetWordBoundaries(wordBoundaries)
	// One click places the anchor a drag grows from, two select a word,
	// three the line.
	gesture.SetBehaviors(gostty.GestureBehaviorCell, gostty.GestureBehaviorWord, gostty.GestureBehaviorLine)
	wake := o.Wake
	if wake == nil {
		wake = func() {}
	}
	s := &Session{
		prog: prog, wake: wake,
		vt: vt, state: state, gesture: gesture,
		cols: cols, rows: rows,
		done:     make(chan struct{}),
		replies:  make(chan []byte, replyQueue),
		resizeCh: make(chan struct{}, 1),
	}
	// A program that selects text itself copies it with OSC 52. The request
	// is served on the UI thread, where the clipboard is; one asking to read
	// the clipboard is refused.
	vt.Stream().OnClipboardWriteRequest(func(req *gostty.ClipboardRequest) {
		if text, ok := clipboardText(req); ok {
			s.copied = &text // under mu, which feeding holds
		}
		_ = req.Allow(false)
	})
	go s.pump()
	go s.writeReplies()
	go s.sendResizes()
	return s, nil
}

// wordBoundaries end a word for double-click selection.
var wordBoundaries = []rune{
	0, ' ', '\t', '\'', '"', '`', '|', ':', ';', ',',
	'(', ')', '[', ']', '{', '}', '<', '>', '$', '│',
}

// outputGrace is how long the terminal's output is read once the program
// has exited: long enough for what it printed last, and no longer, as a
// process it left holding the terminal would otherwise keep the session
// from ever ending.
const outputGrace = time.Second

// pump copies the program's output into the terminal until it ends.
func (s *Session) pump() {
	defer close(s.done)
	defer s.requestPaint()

	// The program is waited for as it exits, not once its terminal's
	// output ends, which a process it left in the background may hold off
	// forever.
	exited := make(chan int, 1)
	go func() {
		code := s.prog.Wait()
		s.prog.EndOutput(outputGrace)
		exited <- code
	}()

	buf := make([]byte, 32*1024)
	for {
		n, err := s.prog.Read(buf)
		if n > 0 {
			s.feed(buf[:n])
		}
		if err != nil {
			break
		}
	}
	code := <-exited

	s.mu.Lock()
	s.exited, s.exitCode = true, code
	if s.settle != nil {
		s.settle.Stop()
	}
	s.mu.Unlock()
}

func (s *Session) feed(chunk []byte) {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return
	}
	// A parse failure must not end the session: the screen falls behind.
	_, _ = s.vt.Write(chunk)
	// This terminal answers the program's queries (device attributes, the
	// cursor's position, size reports): it sees every byte, so each is
	// answered once.
	stream := s.vt.Stream()
	s.queueRepliesLocked(stream)
	for ev, err := range stream.EventValues() {
		if err != nil {
			break
		}
		if ev.Kind == gostty.StreamEventTitleChanged {
			s.title = ev.Title
		}
	}
	s.mu.Unlock()
	s.requestPaint()
}

// replyQueue is how many batches of replies may wait for a program that
// reads none of its input; those after it are dropped, as a program that
// reads nothing would not have seen them anyway.
const replyQueue = 64

// queueRepliesLocked takes what the terminal has answered and queues it
// for writeReplies.
func (s *Session) queueRepliesLocked(stream *gostty.Stream) {
	if !stream.HasReplies() {
		return
	}
	var b bytes.Buffer
	if stream.WriteReplies(&b) != nil || b.Len() == 0 {
		return
	}
	select {
	case s.replies <- b.Bytes():
	default:
	}
}

// writeReplies types the terminal's replies to the program, in the order
// they were made, until the program exits.
func (s *Session) writeReplies() {
	for {
		select {
		case b := <-s.replies:
			_, _ = s.prog.Write(b)
		case <-s.done:
			return
		}
	}
}

// requestPaint asks for the terminal to be painted again, once until a
// paint copies the screen.
func (s *Session) requestPaint() {
	if !s.pending.Swap(true) {
		s.wake()
	}
}

// Done is closed once the program has exited and its output ended.
func (s *Session) Done() <-chan struct{} { return s.done }

// Exited reports whether the program has exited, and its exit code then,
// -1 when it said none.
func (s *Session) Exited() (bool, int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.exited, s.exitCode
}

// Title is what the program last named its window.
func (s *Session) Title() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.title
}

// Close ends the program, and what it started, and releases the
// terminal. It waits for the program's output to end, which the program's
// end ends within outputGrace.
func (s *Session) Close() error {
	var err error
	s.closing.Do(func() {
		s.prog.Kill()
		err = s.prog.Close()
		<-s.done
		s.mu.Lock()
		defer s.mu.Unlock()
		s.closed = true
		if s.settle != nil {
			s.settle.Stop()
		}
		_ = s.state.Close()
		_ = s.vt.Close()
	})
	return err
}

// Write sends p to the program as it is.
func (s *Session) Write(p []byte) (int, error) {
	if s.ended() {
		return 0, errors.New("term: the program has exited")
	}
	return s.prog.Write(p)
}

// ended reports whether the program is gone, or the session closed.
func (s *Session) ended() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.closed || s.exited
}

// Size ---------------------------------------------------------------------

// Resize follows the view's size, cols by rows of cells cellW by cellH
// device pixels: the terminal at once, the program once the size settles.
func (s *Session) Resize(cols, rows, cellW, cellH uint16) {
	cols, rows = max(cols, 2), max(rows, 2)
	s.mu.Lock()
	if s.closed || (cols == s.cols && rows == s.rows && cellW == s.cellW && cellH == s.cellH) {
		s.mu.Unlock()
		return
	}
	s.cols, s.rows, s.cellW, s.cellH = cols, rows, cellW, cellH
	// Through the stream, not the terminal: a program that asked for
	// in-band size reports (mode 2048) is owed one, which only the stream
	// queues.
	stream := s.vt.Stream()
	_ = stream.ResizeCells(cols, rows, uint32(cellW), uint32(cellH))
	s.queueRepliesLocked(stream)
	now := time.Now()
	send := !s.exited && s.settle == nil && now.Sub(s.sentAt) >= resizeSettle
	if send {
		s.sentAt = now
	} else if !s.exited && s.settle == nil {
		s.settle = time.AfterFunc(resizeSettle, s.sendSize)
	}
	s.mu.Unlock()
	if send {
		s.queueResize()
	}
}

// resizeSettle is how long the size must hold before the program hears of
// it again.
const resizeSettle = 120 * time.Millisecond

// queueResize notifies sendResizes to deliver the latest terminal size off the
// UI thread without blocking Paint.
func (s *Session) queueResize() {
	select {
	case s.resizeCh <- struct{}{}:
	default:
	}
}

// sendSize tells the program the size the terminal settled on.
func (s *Session) sendSize() {
	s.mu.Lock()
	s.settle = nil
	s.sentAt = time.Now()
	live := !s.closed && !s.exited
	s.mu.Unlock()
	if live {
		s.queueResize()
	}
}

// sendResizes delivers coalesced terminal size updates to the underlying
// program asynchronously so callers on the UI thread (such as Paint) never
// block on network or PTY resize RPCs.
func (s *Session) sendResizes() {
	for {
		select {
		case <-s.resizeCh:
			s.mu.Lock()
			cols, rows, cellW, cellH := s.cols, s.rows, s.cellW, s.cellH
			live := !s.closed && !s.exited
			s.mu.Unlock()
			if live {
				_ = s.prog.Resize(cols, rows, cellW, cellH)
			}
		case <-s.done:
			return
		}
	}
}

// Size is the terminal's size in cells.
func (s *Session) Size() (cols, rows uint16) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.cols, s.rows
}

// Colours ------------------------------------------------------------------

// Colors are the colours a terminal draws with by default, and answers a
// program asking for them: its foreground, background and sixteen ANSI
// colours, each 0xRRGGBB, and whether they are dark, which a program that
// subscribed (mode 2031) is told when it changes.
type Colors struct {
	Fg, Bg uint32
	ANSI   [16]uint32
	Dark   bool
}

// SetColors makes c the terminal's colours. It runs on every paint, so it
// is cheap when nothing changed.
func (s *Session) SetColors(c Colors) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed || c == s.colors {
		return
	}
	was := s.colors
	s.colors = c
	vt := s.vt.Terminal()
	for i, rgb := range c.ANSI {
		_ = vt.SetDefaultPaletteColor(uint8(i), gostty.RGBFromUint32(rgb))
	}
	vt.ResetPalette()
	vt.SetDefaultForegroundColor(gostty.RGBFromUint32(c.Fg))
	vt.SetDefaultBackgroundColor(gostty.RGBFromUint32(c.Bg))
	// A cell keeps the palette index it was written with, but nothing on
	// screen changed, so the old render state would hand back the colours
	// it resolved before. A fresh one resolves them again.
	if fresh, err := gostty.NewRenderState(); err == nil {
		_ = s.state.Close()
		s.state = fresh
		s.hasFrame = false
	}
	if was == (Colors{}) || was.Dark != c.Dark {
		scheme := gostty.ColorSchemeLight
		if c.Dark {
			scheme = gostty.ColorSchemeDark
		}
		stream := s.vt.Stream()
		stream.ColorSchemeChanged(scheme)
		s.queueRepliesLocked(stream)
	}
}

// Frame --------------------------------------------------------------------

// Frame is what one paint reads. Its slices are reused by the next frame,
// so they are valid only during the paint that asked for them.
type Frame struct {
	Cells      []gostty.RenderCell
	Clusters   []string // a cell's grapheme cluster, where it has more than one codepoint
	Cols, Rows int
	Colors     gostty.RenderColors
	Cursor     gostty.RenderCursor
	Exited     bool
	ExitCode   int
}

// TextAt is a cell's text: its grapheme cluster when it has one, otherwise
// its codepoint.
func (f Frame) TextAt(i int) string {
	if i < 0 || i >= len(f.Cells) {
		return ""
	}
	if s := f.Clusters[i]; s != "" {
		return s
	}
	return string(f.Cells[i].Codepoint)
}

// Frame copies the screen for one paint.
func (s *Session) Frame() (Frame, error) {
	s.pending.Store(false)
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return Frame{}, errors.New("term: closed")
	}
	if err := s.state.Update(s.vt.Terminal()); err != nil {
		return Frame{}, err
	}
	cols, rows := int(s.state.Cols()), int(s.state.Rows())
	n := cols * rows
	if len(s.cells) != n {
		s.cells = slices.Grow(s.cells[:0], n)[:n]
		s.clusters = slices.Grow(s.clusters[:0], n)[:n]
		s.hasFrame = false
	}

	// Only what changed since the last paint is copied out of the native
	// state: a frame drawn for a cursor blink need not touch it at all.
	dirty := s.state.Dirty()
	full := !s.hasFrame || dirty == gostty.RenderDirtyFull
	if !full && dirty == gostty.RenderDirtyPartial {
		if cap(s.dirty) < rows {
			s.dirty = make([]uint16, rows)
		}
		dst := s.dirty[:rows]
		count, err := s.state.DirtyRows(dst)
		if err != nil {
			return Frame{}, err
		}
		changed := int(count)
		// Copying the whole screen is cheaper than many row copies.
		full = changed*2 >= rows
		if !full {
			for _, y := range dst[:changed] {
				if int(y) >= rows {
					continue
				}
				row := s.cells[int(y)*cols : (int(y)+1)*cols]
				if _, err := s.state.RowCells(y, row); err != nil {
					return Frame{}, err
				}
				s.refreshClustersLocked(int(y), int(y)+1, cols)
			}
		}
	}
	if full {
		if _, err := s.state.Cells(s.cells); err != nil {
			return Frame{}, err
		}
		s.refreshClustersLocked(0, rows, cols)
	}
	s.hasFrame = true
	s.state.Clean()
	return Frame{
		Cells: s.cells, Clusters: s.clusters, Cols: cols, Rows: rows,
		Colors: s.state.Colors(), Cursor: s.state.Cursor(),
		Exited: s.exited, ExitCode: s.exitCode,
	}, nil
}

// refreshClustersLocked rebuilds the grapheme clusters of rows [y0, y1)
// from the cells already copied for them.
func (s *Session) refreshClustersLocked(y0, y1, cols int) {
	if cols <= 0 {
		return
	}
	var buf [32]rune
	for y := y0; y < y1; y++ {
		base := y * cols
		for x := range cols {
			i := base + x
			cell := s.cells[i]
			s.clusters[i] = ""
			if !cell.Flags.HasGrapheme || cell.Codepoint <= ' ' {
				continue
			}
			if k, err := s.state.Graphemes(uint16(x), uint16(y), buf[:]); err == nil && k > 1 {
				s.clusters[i] = string(buf[:min(k, uint(len(buf)))])
			}
		}
	}
}

// Text is the terminal's text, scrollback included, without its styling.
func (s *Session) Text() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return ""
	}
	text, _ := s.vt.FormatString(gostty.FormatOptions{Format: gostty.FormatterFormatPlain})
	return text
}

// Input --------------------------------------------------------------------

// Key encodes one key event, which typed text, if any, with the
// terminal's current modes and sends it.
func (s *Session) Key(ev input.KeyEvent, text string) {
	var buf bytes.Buffer
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return
	}
	err := input.EncodeKey(&buf, s.vt.Terminal(), ev, text)
	if err == nil && buf.Len() > 0 {
		// Typing snaps the viewport back to the bottom, as every terminal does.
		s.vt.Terminal().ScrollViewport(gostty.ScrollViewportBottom())
	}
	s.mu.Unlock()
	if err == nil && buf.Len() > 0 {
		_, _ = s.Write(buf.Bytes())
		s.requestPaint()
	}
}

// Paste sends text the way the program asked for it: bracketed when it
// enabled bracketed paste, plain otherwise, its line breaks and all. Only
// a paste's own end marker, which would let the rest run as if typed, is
// refused.
func (s *Session) Paste(text string) {
	if text == "" || strings.Contains(text, "\x1b[201~") {
		return
	}
	var buf bytes.Buffer
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return
	}
	err := input.EncodePaste(&buf, s.vt.Terminal(), []byte(text))
	s.vt.Terminal().ScrollViewport(gostty.ScrollViewportBottom())
	s.mu.Unlock()
	if err == nil {
		_, _ = s.Write(buf.Bytes())
		s.requestPaint()
	}
}

// Focus tells a program that asked for focus events (mode 1004) that its
// terminal gained or lost the keyboard.
func (s *Session) Focus(on bool) {
	var buf bytes.Buffer
	s.mu.Lock()
	if s.closed || on == s.focused {
		s.mu.Unlock()
		return
	}
	s.focused = on
	ev := input.FocusEventLost
	if on {
		ev = input.FocusEventGained
	}
	if s.vt.Terminal().ModeEnabled(gostty.ModeFocusEvent) {
		_ = input.EncodeFocus(&buf, ev)
	}
	s.mu.Unlock()
	if buf.Len() > 0 {
		_, _ = s.Write(buf.Bytes())
	}
}

// Edit sends a word or line edit at a prompt as a Unix shell's readline
// reads it, and reports whether it did: not on the alternate screen,
// where a full screen program reads its keys as they are.
func (s *Session) Edit(e EditKey) bool {
	s.mu.Lock()
	if s.closed || e == EditNone || s.vt.Terminal().ActiveScreenKey() == gostty.ScreenKeyAlternate {
		s.mu.Unlock()
		return false
	}
	s.vt.Terminal().ScrollViewport(gostty.ScrollViewportBottom())
	s.mu.Unlock()
	_, _ = s.Write([]byte(e.bytes()))
	return true
}

// NeedsKeys reports whether keys that type text must be sent as keys, with
// the text they typed, rather than as the text alone: when the program
// asked, through the Kitty keyboard protocol, to hear of their releases or
// of every key as a code.
func (s *Session) NeedsKeys() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return false
	}
	return keyFlags(s.vt.Terminal()).needsKeys()
}

// Scrolling and the mouse --------------------------------------------------

// Wheel scrolls the scrollback by lines, up for more than zero, or on the
// alternate screen, where there is none, sends the arrows a pager
// understands.
func (s *Session) Wheel(lines int) {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return
	}
	alt := s.vt.Terminal().ActiveScreenKey() == gostty.ScreenKeyAlternate
	if !alt {
		s.vt.Terminal().ScrollViewport(gostty.ScrollViewportDelta(-lines))
	}
	s.mu.Unlock()
	if !alt {
		s.requestPaint()
		return
	}
	key := input.KeyArrowUp
	if lines < 0 {
		key, lines = input.KeyArrowDown, -lines
	}
	for range lines {
		s.Key(input.KeyEvent{Action: input.KeyActionPress, Key: key}, "")
	}
}

// MouseReported reports whether the program asked for the mouse (DECSET
// 1000 and friends), in which case presses, drags and the wheel are its.
func (s *Session) MouseReported() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return false
	}
	return s.vt.Terminal().MouseTracking() != gostty.MouseTrackingNone
}

// Mouse encodes n of one mouse event under the program's modes and sends
// them, reporting whether the modes made anything of it.
func (s *Session) Mouse(ev input.MouseEvent, size input.RenderSize, anyPressed bool, n int) bool {
	var buf bytes.Buffer
	s.mu.Lock()
	if s.closed || input.EncodeMouse(&buf, s.vt.Terminal(), ev, size, anyPressed) != nil {
		s.mu.Unlock()
		return false
	}
	s.mu.Unlock()
	if buf.Len() == 0 {
		return false
	}
	_, _ = s.Write(bytes.Repeat(buf.Bytes(), max(n, 1)))
	return true
}

// Selection ----------------------------------------------------------------
//
// The gesture counts clicks, applies the granularity, and works out what a
// drag covers across soft wraps, wide characters and the scrollback; this
// only feeds it the pointer and puts what it hands back on the screen.

// Double-click policy: how close in time and space two presses have to be.
const (
	multiClickInterval = 400 * time.Millisecond
	multiClickCells    = 2
)

// Pointer is a pointer's place in a terminal: the cell under it and the
// pixel position within the grid, which the gesture needs at the edges.
type Pointer struct {
	Col, Row   int
	Xpos, Ypos float64 // Xpos in whole cells' pixels; see WholeCells
	CellW      float64 // whole pixels
	Cols       int
	Height     float64
}

// SelectPress starts a selection at p: a cell, a word or a line, by how
// many clicks came before it.
func (s *Session) SelectPress(p Pointer) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return
	}
	s.gesture.SetGeometry(gostty.GestureGeometry{
		Columns: uint32(p.Cols), CellWidth: uint32(p.CellW), ScreenHeight: uint32(p.Height),
	})
	sel, ok, err := s.gesture.Press(gostty.GesturePressEvent{
		X: uint16(p.Col), Y: uint16(p.Row), Xpos: p.Xpos, Ypos: p.Ypos,
		MaxDistance:      multiClickCells * p.CellW,
		RepeatIntervalNs: uint64(multiClickInterval),
		TimeNs:           time.Now().UnixNano(),
	})
	if err == nil {
		s.dragging = true
		s.applySelectionLocked(sel, ok)
	}
}

// SelectDrag grows the selection to p; rectangle selects the block
// between the corners rather than the flow of text. Held past the top or
// bottom, the view scrolls and the selection grows with it.
func (s *Session) SelectDrag(p Pointer, rectangle bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed || !s.dragging {
		return
	}
	ev := gostty.GestureDragEvent{X: uint16(p.Col), Y: uint16(p.Row), Xpos: p.Xpos, Ypos: p.Ypos, Rectangle: rectangle}
	s.applySelectionLocked(s.gesture.Drag(ev))
	if dir := s.gesture.Autoscroll(); dir != gostty.GestureAutoscrollDirectionNone {
		delta := 1
		if dir == gostty.GestureAutoscrollDirectionUp {
			delta = -1
		}
		s.vt.Terminal().ScrollViewport(gostty.ScrollViewportDelta(delta))
		s.applySelectionLocked(s.gesture.AutoscrollTick(ev))
	}
}

// SelectRelease ends a drag and is the text selected.
func (s *Session) SelectRelease(p Pointer) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed || !s.dragging {
		return ""
	}
	s.dragging = false
	s.gesture.Release(uint16(p.Col), uint16(p.Row))
	return s.selectionLocked()
}

func (s *Session) applySelectionLocked(sel gostty.Selection, ok bool) {
	screen := s.vt.Terminal().ActiveScreen()
	if !ok {
		screen.ClearSelection()
		return
	}
	_, _ = screen.SetSelection(sel)
}

// Selection is the selected text, or "" when nothing is selected.
func (s *Session) Selection() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return ""
	}
	return s.selectionLocked()
}

func (s *Session) selectionLocked() string {
	text, ok, err := s.vt.Terminal().ActiveScreen().SelectionString()
	if err != nil || !ok {
		return ""
	}
	return text
}

// ClearSelection lets go of the selection.
func (s *Session) ClearSelection() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return
	}
	s.vt.Terminal().ActiveScreen().ClearSelection()
	s.gesture.Reset()
	s.dragging = false
}

// SelectAll selects the whole scrollback, not only what is on screen.
func (s *Session) SelectAll() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return
	}
	_, _ = s.vt.Terminal().ActiveScreen().SelectAll()
}

// TakeCopied hands over what the program put on the clipboard (OSC 52)
// since the last call.
func (s *Session) TakeCopied() (string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.copied == nil {
		return "", false
	}
	text := *s.copied
	s.copied = nil
	return text, true
}

// clipboardText is the plain text of a clipboard write, the standard
// clipboard's only.
func clipboardText(req *gostty.ClipboardRequest) (string, bool) {
	if loc, err := req.Location(); err != nil || loc != gostty.ClipboardLocationStandard {
		return "", false
	}
	n, err := req.ContentCount()
	if err != nil {
		return "", false
	}
	for i := range n {
		mime, err := req.ContentMime(i)
		if err != nil || !strings.HasPrefix(mime, "text/plain") {
			continue
		}
		if data, err := req.ContentData(i); err == nil {
			return string(data), true
		}
	}
	return "", false
}
