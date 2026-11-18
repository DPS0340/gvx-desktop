package term

import (
	"bytes"
	"strings"

	"github.com/ironpark/gostty"
	"github.com/ironpark/gostty/input"
)

// The keys that move and delete by word or line at a prompt are not the
// same on every system: a Mac's are ⌥ and ⌘ with the arrows and Delete,
// which a terminal sends as nothing a shell binds. On the main screen,
// where a shell takes lines, such a key is sent as a Unix shell's
// readline reads that edit: the shells here run in Linux containers,
// whatever the system the app runs on.

// EditKey is an edit at a prompt.
type EditKey int

const (
	EditNone EditKey = iota
	EditWordLeft
	EditWordRight
	EditWordDelete // the word before the cursor
	EditLineStart
	EditLineEnd
	EditLineDelete // the line before the cursor
)

// bytes is what readline's emacs keys read as e.
func (e EditKey) bytes() string {
	switch e {
	case EditWordLeft:
		return "\x1bb"
	case EditWordRight:
		return "\x1bf"
	case EditWordDelete:
		return "\x1b\x7f"
	case EditLineStart:
		return "\x01"
	case EditLineEnd:
		return "\x05"
	case EditLineDelete:
		return "\x15"
	}
	return ""
}

// kittyFlags are the progressive enhancements of the Kitty keyboard
// protocol a program turned on, CSI > flags u, on the terminal's active
// screen.
type kittyFlags uint8

const (
	kittyDisambiguate   kittyFlags = 1 << iota // escape codes for keys a legacy one confuses
	kittyEventTypes                            // repeats and releases too
	kittyAlternateKeys                         // the shifted key and the base layout's too
	kittyAllKeysAsCodes                        // text keys as escape codes as well
)

// needsKeys reports whether keys that type text must be sent as keys, with
// the text they typed, rather than as the text alone: when the program
// hears of their releases or asked for every key as a code.
func (f kittyFlags) needsKeys() bool { return f&(kittyEventTypes|kittyAllKeysAsCodes) != 0 }

// keyFlags reads the flags term encodes keys by. ghostty does not say
// them, and a query written to the terminal would land in the middle of
// whatever the program printed last, so they are read off how it encodes
// keys that tell each flag apart; encoding changes nothing.
func keyFlags(term *gostty.Terminal) kittyFlags {
	encode := func(ev input.KeyEvent, text string) string {
		var b bytes.Buffer
		if input.EncodeKey(&b, term, ev, text) != nil {
			return ""
		}
		return b.String()
	}
	a := input.KeyEvent{Action: input.KeyActionPress, Key: input.KeyKeyA, UnshiftedCodepoint: 'a'}
	var f kittyFlags
	if encode(input.KeyEvent{Action: input.KeyActionPress, Key: input.KeyEscape}, "") != "\x1b" {
		f |= kittyDisambiguate
	}
	release := a
	release.Action = input.KeyActionRelease
	if encode(release, "") != "" {
		f |= kittyEventTypes
	}
	shifted := a
	shifted.Mods = input.KeyMods{Shift: true, Ctrl: true}
	if strings.Contains(encode(shifted, "A"), "97:65") {
		f |= kittyAlternateKeys
	}
	if strings.HasPrefix(encode(a, "a"), "\x1b[") {
		f |= kittyAllKeysAsCodes
	}
	return f
}
