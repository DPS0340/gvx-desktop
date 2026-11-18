package ui

import (
	"image/color"

	"github.com/ironpark/ggui"
	"github.com/ironpark/ggui/icons/lucide"
	"github.com/ironpark/ggui/ui"
	"github.com/ironpark/ggui/ui/icons"
	"github.com/ironpark/ggui/widget"

	"github.com/ironpark/gvx-desktop/internal/sandbox"
)

// Status colors read on both light and dark surfaces.
var (
	colorGreen = color.RGBA{0x22, 0xc5, 0x5e, 0xff}
	colorAmber = color.RGBA{0xf5, 0x9e, 0x0b, 0xff}
	colorRed   = color.RGBA{0xef, 0x44, 0x44, 0xff}
	colorGray  = color.RGBA{0xa1, 0xa1, 0xaa, 0xff}
	colorBlue  = color.RGBA{0x3b, 0x82, 0xf6, 0xff}
)

func statusColor(s sandbox.Status) color.Color {
	switch s {
	case sandbox.Running:
		return colorGreen
	case sandbox.Starting, sandbox.Stopping:
		return colorAmber
	case sandbox.Failed:
		return colorRed
	case sandbox.Exited:
		return colorBlue
	}
	return colorGray
}

func dot(c color.Color, size float64) *widget.BoxWidget {
	return widget.Box().Size(size, size).Radius(size / 2).Fill(c)
}

// statusLabel is a colored dot and the status in words.
func statusLabel(st sandbox.State) ggui.Widget {
	label := tr(st.Status.String())
	if st.Status == sandbox.Failed {
		label = tr("Failed · exit %d", st.ExitCode)
	}
	var lead ggui.Widget = dot(statusColor(st.Status), 8)
	if st.Status == sandbox.Starting || st.Status == sandbox.Stopping {
		lead = ui.Spinner().Size(12).Color(statusColor(st.Status))
	}
	return widget.Row(lead, widget.Text(label).Size(13).NoWrap()).Gap(8)
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		b[i] = '-'
	}
	return string(b[i:])
}

// glyph is a Lucide icon at size.
func glyph(g *icons.Glyph, size float64) *icons.Widget { return g.Icon().Size(size) }

// iconButton is a ghost button holding only an icon.
func iconButton(g *icons.Glyph, name string, onTap func()) *ui.ButtonWidget {
	return ui.ButtonOf(glyph(g, 16), onTap).Name(name).Size(ui.ButtonIconSmall).Ghost()
}

// tip wraps a widget in a tooltip.
func tip(w ggui.Widget, text string) ggui.Widget { return ui.Tooltip(w, text) }

// pageHeader is the title row of a page with its actions at the right.
func pageHeader(title string, subtitle ggui.Widget, actions ...ggui.Widget) ggui.Widget {
	head := widget.Column(ui.Title(title).Size(24), subtitle).Gap(4)
	acts := widget.Row(actions...).Gap(8)
	return widget.Responsive(720,
		widget.Row(widget.Expanded(head), acts).Gap(16).Align(widget.AlignCenter),
		widget.Column(head, acts).Gap(12).Stretch(),
	)
}

// page lays out a scrolling page with a gutter.
func page(children ...ggui.Widget) ggui.Widget {
	body := widget.Column(children...).Gap(20).Stretch()
	return widget.Scroll(widget.Padding(widget.Align(widget.Box(body).MaxWidth(1180)).Top(), 28, 32))
}

// kv is a label and its value in a details list.
func kv(label string, value ggui.Widget) ggui.Widget {
	return widget.Row(
		widget.Box(ui.Caption(label)).Width(150),
		widget.Expanded(value),
	).Gap(12).Align(widget.AlignStart)
}

func kvText(label, value string) ggui.Widget {
	if value == "" {
		value = "—"
	}
	return kv(label, widget.Text(value).Size(13).Selectable())
}

// copyable is monospace text with a copy button.
func copyable(m *Model, text, what string) ggui.Widget {
	return widget.Row(
		widget.Expanded(ui.Mono(text).Size(12).Selectable()),
		iconButton(lucide.Copy, tr("Copy %s", what), func() { m.Copy(text, what) }),
	).Gap(8).Align(widget.AlignCenter)
}

// metric is a count with its label, as the dashboard shows them.
func metric(label string, value ggui.Widget, icon *icons.Glyph, onTap func()) ggui.Widget {
	body := widget.Column(
		widget.Row(ui.Caption(label), widget.Spacer(), glyph(icon, 16).Color(colorGray)).Align(widget.AlignCenter),
		value,
	).Gap(10).Stretch()
	card := ui.Card(body)
	if onTap == nil {
		return card
	}
	return widget.Tap(card, onTap).Cursor(ggui.CursorShapePointer)
}

// sectionTitle heads a group inside a card.
func sectionTitle(title, help string) ggui.Widget {
	if help == "" {
		return ui.Heading(title).Size(15)
	}
	return widget.Column(ui.Heading(title).Size(15), ui.Caption(help)).Gap(2)
}
