package ui

import (
	"image/color"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/ironpark/ggui"
	"github.com/ironpark/ggui/icons/lucide"
	"github.com/ironpark/ggui/ui"
	"github.com/ironpark/ggui/ui/icons"
	uitheme "github.com/ironpark/ggui/ui/theme"
	"github.com/ironpark/ggui/widget"

	"github.com/ironpark/gvx-desktop/internal/rungv"
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

// statusText is a sandbox's status in words.
func statusText(st sandbox.State) string {
	if st.Status == sandbox.Failed {
		return tr("Failed · exit %d", st.ExitCode)
	}
	return tr(st.Status.String())
}

// statusMark is a sandbox's status as a colored dot, which turns while the
// status is changing.
func statusMark(st sandbox.State) ggui.Widget {
	if st.Status == sandbox.Starting || st.Status == sandbox.Stopping {
		return ui.Spinner().Size(12).Color(statusColor(st.Status))
	}
	return widget.Box(dot(statusColor(st.Status), 8)).Pad(2)
}

// statusLabel is the mark and the words together.
func statusLabel(st sandbox.State) ggui.Widget {
	return widget.Row(statusMark(st), widget.Text(statusText(st)).Size(13).NoWrap()).Gap(8).Align(widget.AlignCenter)
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

// entering builds a subtree for key's value. When the value changes, what
// is there fades away fast, and only then does the next come up into
// place: two screens are never drawn over one another.
func entering[K comparable](key ggui.Readable[K], build func(K) ggui.Widget) ggui.Widget {
	return ggui.Component(func() ggui.Widget {
		shown := ggui.State(ggui.Peek(key))
		opacity := ggui.Tween(1.0, leaveDuration).Easing(ggui.EaseStandardAccelerate)
		leaving := false
		ggui.Watch(key, func(k K) {
			if k == ggui.Peek(shown) {
				// Back to what is still there: stay.
				leaving = false
				opacity.Set(1)
				return
			}
			leaving = true
			opacity.Set(0)
		})
		ggui.Watch(opacity, func(v float64) {
			if !leaving || v > 0 {
				return
			}
			leaving = false
			shown.Set(ggui.Peek(key))
			opacity.Jump(1) // the next one's own entrance fades it in
		})
		return widget.Opacity(ggui.Key(shown, func(k K) ggui.Widget {
			return widget.Transition(build(k)).Fade().Slide(0, 8).Duration(pageDuration).Easing(ggui.EaseStandardDecelerate)
		}), 1).BindOpacity(opacity)
	})
}

// headed puts what may or may not be there under a page's header, so that
// nothing leaves no gap behind.
func headed(header ggui.Widget, under ...ggui.Widget) ggui.Widget {
	return widget.Column(append([]ggui.Widget{header}, under...)...).Stretch()
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
	if onTap == nil {
		return ui.Card(body)
	}
	// A card that goes somewhere answers the pointer: its surface warms
	// to the accent while the pointer is over it.
	hover := ggui.State(false)
	fill := ggui.Map(hover, func(over bool) color.Color {
		if over {
			return uitheme.Var("accent").Alpha(.6)
		}
		return color.RGBA{}
	})
	surface := widget.AnimatedBox(body).Pad(16).Radius(cardRadius - 1).BindFill(fill).Duration(hoverDuration)
	return widget.Tap(ui.Card(surface).Pad(0), onTap).OnHover(hover.Set).Cursor(ggui.CursorShapePointer)
}

// tableRow is the height of a row that holds buttons, which the default
// row of one line of text would squeeze.
const tableRow = 44

// cardRadius is the corner of a card under the stock theme.
const cardRadius = 14

// tableCard is a card around a table, with the bars given above it, such
// as the field that filters it. It clips what it holds to its corners: a
// row's highlight is square, and the last row's would cover them.
func tableCard(table ggui.Widget, bars ...ggui.Widget) ggui.Widget {
	const r = cardRadius - 1
	var body []ggui.Widget
	for _, bar := range bars {
		body = append(body, bar, ui.Divider())
	}
	body = append(body, table)
	return ui.Card(widget.ClipPath(widget.Column(body...).Stretch(), func(b ggui.Rect) *ggui.Path {
		x0, y0, x1, y1 := b.Origin.X, b.Origin.Y, b.Origin.X+b.Size.W, b.Origin.Y+b.Size.H
		p := &ggui.Path{}
		p.MoveTo(x0+r, y0)
		p.LineTo(x1-r, y0)
		p.QuadTo(x1, y0, x1, y0+r)
		p.LineTo(x1, y1-r)
		p.QuadTo(x1, y1, x1-r, y1)
		p.LineTo(x0+r, y1)
		p.QuadTo(x0, y1, x0, y1-r)
		p.LineTo(x0, y0+r)
		p.QuadTo(x0, y0, x0+r, y0)
		p.Close()
		return p
	})).Pad(1)
}

// filterBar heads a table with the field that filters it: what is typed
// in query chooses the rows, and count says how many are showing.
func filterBar(query ggui.Binding[string], placeholder string, count ggui.Readable[string]) ggui.Widget {
	clear := ggui.If(ggui.Map(query, func(q string) bool { return q != "" }), func() ggui.Widget {
		return iconButton(lucide.X, tr("Clear the filter"), func() { query.Set("") })
	})
	return widget.Box(widget.Row(
		glyph(lucide.Search, 15).Color(colorGray),
		widget.Expanded(widget.TextInput(query).Placeholder(placeholder).Name(placeholder)),
		clear,
		ui.CaptionOf(count).Size(12).NoWrap(),
	).Gap(10).Align(widget.AlignCenter)).Pad(0, 12).Height(tableRow)
}

// showing words how many of total rows a filter leaves.
func showing(shown, total int) string {
	if shown == total {
		return tr("%d items", total)
	}
	return tr("%d of %d", shown, total)
}

// buildStamp is the ten digits gvx ends a version with, the day and hour
// it was cut from: v0.16.10022026110900 is v0.16.1002 of 2026-11-09.
var buildStamp = regexp.MustCompile(`^(v?\d+\.\d+\.\d+?)\d{10}$`)

// shortVersion is a version of gvx without its build stamp, for the places
// that have no room for it.
func shortVersion(v string) string {
	if m := buildStamp.FindStringSubmatch(v); m != nil {
		return m[1]
	}
	return v
}

// tildePath writes a path under the user's home directory from ~.
func tildePath(p string) string {
	if home, err := rungv.HomeDir(); err == nil && home != "" && strings.HasPrefix(p, home+string(os.PathSeparator)) {
		return "~" + p[len(home):]
	}
	return p
}

// since says how long ago t was, in the largest unit that fits.
func since(t, now time.Time) string {
	days := int(now.Sub(t).Hours() / 24)
	switch {
	case days < 1:
		return ago(t, now)
	case days < 14:
		return tr("%d days ago", days)
	case days < 60:
		return tr("%d weeks ago", days/7)
	case days < 730:
		return tr("%d months ago", days/30)
	}
	return tr("%d years ago", days/365)
}

// sectionTitle heads a group inside a card.
func sectionTitle(title, help string) ggui.Widget {
	if help == "" {
		return ui.Heading(title).Size(15)
	}
	return widget.Column(ui.Heading(title).Size(15), ui.Caption(help)).Gap(2)
}

// abreast lays two widgets side by side, sharing the width as their
// weights say, and makes both as tall as the taller. A Row stretches its
// children only to a height it is given, and a page that scrolls gives
// none.
func abreast(left, right ggui.Widget, leftWeight, rightWeight, gap float64) ggui.Widget {
	return &abreastWidget{left: left, right: right, lw: leftWeight, rw: rightWeight, gap: gap}
}

type abreastWidget struct {
	left, right ggui.Widget
	lw, rw, gap float64
	split       float64 // the left one's width, as laid out
}

func (a *abreastWidget) Layout(c ggui.Constraints, env ggui.Env) ggui.Size {
	room := max(0, c.MaxW-a.gap)
	a.split = room * a.lw / (a.lw + a.rw)
	widths := [2]float64{a.split, room - a.split}
	kids := [2]ggui.Widget{a.left, a.right}
	var tall float64
	for i, k := range kids {
		tall = max(tall, k.Layout(ggui.Constraints{MinW: widths[i], MaxW: widths[i], MaxH: c.MaxH}, env).H)
	}
	for i, k := range kids {
		k.Layout(ggui.Constraints{MinW: widths[i], MaxW: widths[i], MinH: tall, MaxH: tall}, env)
	}
	return ggui.Sz(c.MaxW, tall)
}

func (a *abreastWidget) Paint(dst *ggui.Canvas, r ggui.Rect) {
	dst.Paint(a.left, ggui.Rct(r.Origin, ggui.Sz(a.split, r.Size.H)))
	x := a.split + a.gap
	dst.Paint(a.right, ggui.Rct(ggui.Pt(r.Origin.X+x, r.Origin.Y), ggui.Sz(r.Size.W-x, r.Size.H)))
}

// tailPath shortens a long path to its end, which names the file: the
// directories it drops from the front are told by an ellipsis.
func tailPath(p string, most int) string {
	p = tildePath(p)
	if len([]rune(p)) <= most {
		return p
	}
	parts := strings.Split(filepath.ToSlash(p), "/")
	out := parts[len(parts)-1]
	for i := len(parts) - 2; i >= 0; i-- {
		if next := parts[i] + "/" + out; len([]rune(next))+2 <= most {
			out = next
		} else {
			break
		}
	}
	return "…/" + out
}

// hoverRow is a row of a list that goes somewhere when tapped: its
// surface warms to the accent while the pointer is over it.
func hoverRow(content ggui.Widget, onTap func()) ggui.Widget {
	hover := ggui.State(false)
	fill := ggui.Map(hover, func(over bool) color.Color {
		if over {
			return uitheme.Var("accent")
		}
		return color.RGBA{}
	})
	return widget.Tap(widget.AnimatedBox(content).Pad(6, 10).Radius(8).BindFill(fill).Duration(hoverDuration), onTap).
		OnHover(hover.Set).Cursor(ggui.CursorShapePointer)
}
