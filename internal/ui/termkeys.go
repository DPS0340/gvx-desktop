package ui

import (
	"image/color"
	"runtime"

	"github.com/ironpark/ggui"
	uitheme "github.com/ironpark/ggui/ui/theme"
	"github.com/ironpark/ggwd"
	"github.com/ironpark/gostty/input"

	"github.com/ironpark/gvx-desktop/internal/term"
)

// terminalKey names a ggui key in the terminal's key set. Both speak W3C key
// codes -- ggfx's String is the code with the letters' "Key" prefix dropped
// -- so the table is a translation rather than a list kept in step by hand.
func terminalKey(k ggui.KeyboardKey) (input.Key, bool) {
	if int(k) >= 0 && int(k) < len(keyTable) {
		named := keyTable[k]
		return named, named != input.KeyUnidentified
	}
	return input.KeyUnidentified, false
}

var keyTable = func() []input.Key {
	table := make([]input.Key, ggwd.KeyMax+1)
	for k := ggwd.Key(0); k <= ggwd.KeyMax; k++ {
		name := k.String()
		if len(name) == 1 && name[0] >= 'A' && name[0] <= 'Z' {
			name = "Key" + name
		}
		if named, ok := input.KeyFromW3C(name); ok {
			table[k] = named
		}
	}
	return table
}()

// altMakesText reports whether Option/Alt produces characters, as it does on
// a Mac keyboard: there an Alt+letter arrives as text and is sent as such.
var altMakesText = runtime.GOOS == "darwin"

func keyMods(m ggui.Mods) input.KeyMods {
	return input.KeyMods{Shift: m.Shift, Ctrl: m.Ctrl, Alt: m.Alt, Super: m.Meta}
}

// The terminal's clipboard chords: ⌘V, ⌘C and ⌘A on macOS, and with
// Ctrl+Shift elsewhere, since the bare Ctrl chords belong to the program.
func isPaste(ev ggui.KeyEvent) bool     { return terminalChord(ev, ggui.KeyV) }
func isCopy(ev ggui.KeyEvent) bool      { return terminalChord(ev, ggui.KeyC) }
func isSelectAll(ev ggui.KeyEvent) bool { return terminalChord(ev, ggui.KeyA) }

func terminalChord(ev ggui.KeyEvent, key ggui.KeyboardKey) bool {
	if ev.Kind != ggui.KeyPress || ev.Key != key || ev.Mods.Alt {
		return false
	}
	if runtime.GOOS == "darwin" {
		return ev.Mods.Meta && !ev.Mods.Ctrl
	}
	return ev.Mods.Ctrl && ev.Mods.Shift
}

// editKeyOf is the edit at a prompt ev asks for with this system's keys,
// typed on goos's keyboard: ⌥ and ⌘ with the arrows and Delete on a Mac,
// Ctrl with them elsewhere.
func editKeyOf(ev ggui.KeyEvent, goos string) term.EditKey {
	m := ev.Mods
	if m.Shift {
		return term.EditNone // selects, where a program selects
	}
	word, line := m.Ctrl && !m.Alt && !m.Meta, false
	if goos == "darwin" {
		word = m.Alt && !m.Ctrl && !m.Meta
		line = m.Meta && !m.Ctrl && !m.Alt
	}
	switch {
	case word && ev.Key == ggui.KeyArrowLeft:
		return term.EditWordLeft
	case word && ev.Key == ggui.KeyArrowRight:
		return term.EditWordRight
	case word && ev.Key == ggui.KeyBackspace:
		return term.EditWordDelete
	case line && ev.Key == ggui.KeyArrowLeft:
		return term.EditLineStart
	case line && ev.Key == ggui.KeyArrowRight:
		return term.EditLineEnd
	case line && ev.Key == ggui.KeyBackspace:
		return term.EditLineDelete
	}
	return term.EditNone
}

// termPalette is what a terminal is painted with: the theme's ground and
// text, and sixteen ANSI colours that read on it, mako's.
type termPalette struct {
	Fg, Bg    color.RGBA
	Raised    color.RGBA // under an IME's composition
	Selection color.RGBA // under selected text
	ANSI      [16]uint32
	Dark      bool
}

var (
	darkANSI = [16]uint32{
		0x141716, 0xE07070, 0x5DBA80, 0xD4A44A, 0x6A9DE0, 0xB07AD0, 0x4AABB8, 0xD4D4D8,
		0x434645, 0xE89090, 0x7ECF9A, 0xE0BE6E, 0x8AB4E8, 0xC49AE0, 0x6EC2CC, 0xF0F0F2,
	}
	lightANSI = [16]uint32{
		0x1A1A1E, 0xDC2626, 0x16A34A, 0xCA8A04, 0x2563EB, 0x9333EA, 0x0891B2, 0xD4D4D8,
		0x3F3F46, 0xEF4444, 0x22C55E, 0xF59E0B, 0x3B82F6, 0xA855F7, 0x06B6D4, 0xFAFAFA,
	}
	darkTerminal = termPalette{
		Fg: hexRGBA(0xFAFAFA), Bg: hexRGBA(0x09090B), Raised: hexRGBA(0x27272A),
		Selection: hexRGBA(0x1E3A5F), ANSI: darkANSI, Dark: true,
	}
)

// terminalPalette is the terminal's palette in theme th: its page's ground
// and text, the selection tinted with its primary colour, and the ANSI
// colours of a light or a dark theme as its ground is.
func terminalPalette(th *uitheme.Theme) termPalette {
	bg, fg := rgbaOf(th.Bg), rgbaOf(th.Fg)
	p := termPalette{Fg: fg, Bg: bg, Raised: rgbaOf(th.Muted), ANSI: lightANSI}
	if lum(bg) < lum(fg) {
		p.Dark, p.ANSI = true, darkANSI
	}
	// A cell's text keeps its colour when selected, so the selection is a
	// tint of the ground, not the primary colour itself.
	p.Selection = mixRGBA(bg, rgbaOf(th.Primary), 0.28)
	return p
}

// colors are the palette as the session's terminal takes it.
func (p termPalette) colors() term.Colors {
	return term.Colors{Fg: rgbUint(p.Fg), Bg: rgbUint(p.Bg), ANSI: p.ANSI, Dark: p.Dark}
}

func hexRGBA(v uint32) color.RGBA {
	return color.RGBA{R: uint8(v >> 16), G: uint8(v >> 8), B: uint8(v), A: 0xff}
}

func rgbUint(c color.RGBA) uint32 { return uint32(c.R)<<16 | uint32(c.G)<<8 | uint32(c.B) }

// rgbaOf is c, opaque.
func rgbaOf(c color.Color) color.RGBA {
	if c == nil {
		return color.RGBA{A: 0xff}
	}
	r, g, b, _ := c.RGBA()
	return color.RGBA{R: uint8(r >> 8), G: uint8(g >> 8), B: uint8(b >> 8), A: 0xff}
}

func lum(c color.RGBA) int { return 299*int(c.R) + 587*int(c.G) + 114*int(c.B) }

// mixRGBA is a moved toward b by k.
func mixRGBA(a, b color.RGBA, k float64) color.RGBA {
	m := func(x, y uint8) uint8 { return uint8(float64(x) + (float64(y)-float64(x))*k + 0.5) }
	return color.RGBA{R: m(a.R, b.R), G: m(a.G, b.G), B: m(a.B, b.B), A: 0xff}
}
