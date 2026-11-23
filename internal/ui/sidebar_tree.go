package ui

import (
	"image/color"
	"math"

	"github.com/ironpark/ggui"
	"github.com/ironpark/ggui/icons/lucide"
	"github.com/ironpark/ggui/ui/icons"
	uitheme "github.com/ironpark/ggui/ui/theme"
	"github.com/ironpark/ggui/widget"
)

// sidebarNavItem describes one destination inside a collapsible sidebar group.
type sidebarNavItem struct {
	key    string
	label  string
	icon   *icons.Glyph
	action ggui.Widget
}

// sidebarTreeGroup renders a clickable top-level sidebar destination (Docker
// or Kubernetes) with a disclosure chevron on the right and a compact,
// smoothly folding list of sub-items beside a thin vertical guide line.
type sidebarTreeGroup struct {
	parent  *sidebarTabRow
	foldBtn *sidebarFoldButton
	subs    []*sidebarTabRow
	open    *ggui.StateValue[bool]

	reveal   ggui.Motion
	progress float64
	theme    *uitheme.Theme
	env      ggui.Env
	parentH  float64
	subsH    float64
	subX     float64
	lineX    float64
}

func newSidebarTreeGroup(m *Model, parent sidebarNavItem, open *ggui.StateValue[bool], subs ...sidebarNavItem) *sidebarTreeGroup {
	g := &sidebarTreeGroup{open: open}
	g.foldBtn = newSidebarFoldButton(open, parent.label)
	g.parent = newSidebarTabRow(m, parent, 0, g.foldBtn)
	g.subs = make([]*sidebarTabRow, len(subs))
	for i, s := range subs {
		g.subs[i] = newSidebarTabRow(m, s, 1, s.action)
	}
	return g
}

func (g *sidebarTreeGroup) Layout(c ggui.Constraints, env ggui.Env) ggui.Size {
	t := uitheme.Of(env)
	g.theme = t
	g.env = env
	w := c.MaxW
	if w == ggui.Unbounded {
		w = c.MinW
	}
	if w < c.MinW {
		w = c.MinW
	}

	g.parentH = g.parent.Layout(ggui.Constraints{MinW: w, MaxW: w, MaxH: ggui.Unbounded}, env).H
	g.progress = g.reveal.Easing(sidebarEasing).LayoutToggle(env, g.open.Get(), t.MotionSlow)

	// Align the 1px guide line under the center of the parent's 16px icon
	// (pad 8 + icon/2 8 = 16 -> line at 15..16), with a tight 4px gap before
	// the sub-item pill so depth reads cleanly without wasting width.
	g.lineX = t.Space + t.IconSize/2 - t.BorderWidth
	g.subX = g.lineX + t.BorderWidth + 4

	g.subsH = 0
	if g.progress > 0 && len(g.subs) > 0 {
		subW := max(w-g.subX, 0)
		const (
			subPadY = 2.0
			subGap  = 2.0
		)
		h := 2 * subPadY
		for i, sub := range g.subs {
			if i > 0 {
				h += subGap
			}
			h += sub.Layout(ggui.Constraints{MinW: subW, MaxW: subW, MaxH: ggui.Unbounded}, env).H
		}
		g.subsH = h
	}
	return c.Constrain(ggui.Sz(w, g.parentH+g.subsH*g.progress))
}

func (g *sidebarTreeGroup) Paint(dst *ggui.Canvas, r ggui.Rect) {
	t := g.theme
	if t == nil {
		return
	}
	g.foldBtn.progress = g.progress
	dst.Paint(g.parent, ggui.Rct(r.Origin, ggui.Sz(r.Size.W, g.parentH)))
	if g.progress <= 0 || g.subsH <= 0 {
		return
	}
	visH := g.subsH * g.progress
	block := ggui.Rct(ggui.Pt(r.Origin.X, r.Origin.Y+g.parentH), ggui.Sz(r.Size.W, visH))
	subCanvas := dst
	if g.progress < 1 {
		subCanvas = dst.Clip(block)
		if !ggui.Untrack(g.open.Get) {
			subCanvas = subCanvas.Inert()
		}
	}
	const (
		subPadY = 2.0
		subGap  = 2.0
	)
	lineH := max(g.subsH-2*subPadY, 0)
	if lineH > 0 {
		subCanvas.FillRect(
			ggui.Rct(ggui.Pt(r.Origin.X+g.lineX, block.Origin.Y+subPadY), ggui.Sz(t.BorderWidth, lineH)),
			t.Border,
		)
	}
	subW := max(r.Size.W-g.subX, 0)
	y := block.Origin.Y + subPadY
	for i, sub := range g.subs {
		if i > 0 {
			y += subGap
		}
		subCanvas.Paint(sub, ggui.Rct(ggui.Pt(r.Origin.X+g.subX, y), ggui.Sz(subW, sub.rowH)))
		y += sub.rowH
	}
}

func (g *sidebarTreeGroup) Adopt(prev any) {
	p, ok := prev.(*sidebarTreeGroup)
	if !ok || p == g {
		return
	}
	g.reveal = p.reveal
	g.parent.Adopt(p.parent)
	g.foldBtn.Adopt(p.foldBtn)
	byKey := make(map[string]*sidebarTabRow, len(p.subs))
	for _, s := range p.subs {
		byKey[s.key] = s
	}
	for _, s := range g.subs {
		if old, ok := byKey[s.key]; ok {
			s.Adopt(old)
		}
	}
}

// sidebarTabRow is one clickable row inside a sidebarTreeGroup, styled to
// match ggui's ui.SidebarItem using the sidebar's active theme tokens.
type sidebarTabRow struct {
	m         *Model
	key       string
	labelText string
	depth     int

	icon   *icons.Widget
	label  *widget.TextWidget
	action ggui.Widget

	theme      *uitheme.Theme
	env        ggui.Env
	hover      bool
	lit        ggui.Motion
	litSeen    bool
	rowH       float64
	padL       float64
	gap        float64
	iconSize   ggui.Size
	labelSize  ggui.Size
	actionSize ggui.Size
}

func newSidebarTabRow(m *Model, item sidebarNavItem, depth int, action ggui.Widget) *sidebarTabRow {
	icSize := 16.0
	if depth > 0 {
		icSize = 15.0
	}
	return &sidebarTabRow{
		m:         m,
		key:       item.key,
		labelText: item.label,
		depth:     depth,
		icon:      glyph(item.icon, icSize),
		label:     widget.Text(item.label).NoWrap().Ellipsis(),
		action:    action,
	}
}

func (r *sidebarTabRow) isActive() bool {
	return ggui.Untrack(r.m.Page.Get) == r.key
}

func (r *sidebarTabRow) Layout(c ggui.Constraints, env ggui.Env) ggui.Size {
	t := uitheme.Of(env)
	r.theme = t
	r.env = env
	u := t.Space
	w := c.MaxW
	if w == ggui.Unbounded {
		w = c.MinW
	}
	if w < c.MinW {
		w = c.MinW
	}

	// Read m.Page in Layout so switching pages updates weight and highlight.
	active := r.m.Page.Get() == r.key
	if r.depth == 0 {
		r.rowH = 4 * u
		r.padL = u
		r.gap = u
	} else {
		r.rowH = 3.5 * u
		r.padL = 6
		r.gap = 7
	}

	regular := t.Text.Weight
	if regular == 0 {
		regular = ggui.WeightRegular
	}
	medium := t.Label.Weight
	if medium == 0 {
		medium = ggui.WeightMedium
	}
	weight := regular
	if active && r.depth == 0 {
		weight = medium
	}
	r.label.Style(ggui.TextStyle{Size: t.Text.Size, Weight: weight})
	r.iconSize = r.icon.Layout(ggui.Loose(ggui.Sz(w, r.rowH)), env)

	edge := u / 2
	limit := w - u
	r.actionSize = ggui.Size{}
	if r.action != nil {
		r.actionSize = r.action.Layout(ggui.Loose(ggui.Sz(2.5*u, r.rowH)), env)
		if r.actionSize.W > 0 {
			limit = min(limit, w-edge-r.actionSize.W-edge)
		}
	}
	labelX := r.padL + r.iconSize.W + r.gap
	r.labelSize = r.label.Layout(ggui.Loose(ggui.Sz(max(limit-labelX, 0), r.rowH)), env)
	return c.Constrain(ggui.Sz(w, r.rowH))
}

func (r *sidebarTabRow) Paint(dst *ggui.Canvas, rect ggui.Rect) {
	t := r.theme
	if t == nil {
		return
	}
	dst.Describe(rect, r)
	dst.HitPointer(rect, r)
	dst.HitCursor(rect, ggui.CursorShapePointer)

	lit := r.isActive() || r.hover
	d := r.env.Motion(t.MotionFast)
	if !r.litSeen {
		r.litSeen, d = true, 0
	}
	k := r.lit.Easing(t.Ease.Easing()).Toggle(lit, ggui.FrameTime(), d)
	fg := mixSidebarColor(t.Fg, t.AccentFg, k)
	if k > 0 {
		dst.FillRoundRect(rect, t.RadiusSm, fadeSidebarColor(t.Accent, k))
	}

	r.icon.Color(fg)
	dst.Paint(r.icon, ggui.Rct(
		ggui.Pt(rect.Origin.X+r.padL, rect.Origin.Y+(rect.Size.H-r.iconSize.H)/2),
		r.iconSize,
	))
	r.label.Color(fg)
	labelX := r.padL + r.iconSize.W + r.gap
	dst.Paint(r.label, ggui.Rct(
		ggui.Pt(rect.Origin.X+labelX, rect.Origin.Y+(rect.Size.H-r.labelSize.H)/2),
		r.labelSize,
	))
	if r.action != nil && r.actionSize.W > 0 {
		edge := t.Space / 2
		at := ggui.Rct(
			ggui.Pt(rect.Origin.X+rect.Size.W-edge-r.actionSize.W, rect.Origin.Y+(rect.Size.H-r.actionSize.H)/2),
			r.actionSize,
		)
		dst.Paint(r.action, at)
	}
}

func (r *sidebarTabRow) HandlePointer(ev ggui.PointerEvent) bool {
	switch ev.Kind {
	case ggui.PointerEnter, ggui.PointerMove:
		if !r.hover {
			r.hover = true
			ggui.Invalidate(r.env)
		}
	case ggui.PointerExit:
		if r.hover {
			r.hover = false
			ggui.Invalidate(r.env)
		}
	case ggui.PointerTap:
		if ev.Button == ggui.MouseButtonLeft {
			r.m.Go(r.key)
		}
	case ggui.PointerScroll:
		return false
	}
	return true
}

func (r *sidebarTabRow) Describe() ggui.Node {
	return ggui.Node{
		Role:     ggui.RoleTab,
		Name:     r.labelText,
		Selected: r.isActive(),
		Level:    r.depth + 1,
		Actions:  ggui.ActionPress | ggui.ActionSelect,
	}
}

func (r *sidebarTabRow) Act(a ggui.Action) bool {
	switch a.Kind {
	case ggui.ActionSelect, ggui.ActionPress:
		r.m.Go(r.key)
		return true
	}
	return false
}

func (r *sidebarTabRow) Adopt(prev any) {
	p, ok := prev.(*sidebarTabRow)
	if !ok || p == r {
		return
	}
	r.hover = p.hover
	r.lit = p.lit
	r.litSeen = p.litSeen
}

// sidebarFoldButton is the disclosure chevron on the trailing edge of a
// collapsible parent row. Clicking it folds or unfolds the sub-items without
// navigating away from the current page.
type sidebarFoldButton struct {
	open     *ggui.StateValue[bool]
	name     string
	icon     *icons.Widget
	progress float64
	hover    bool
	theme    *uitheme.Theme
	env      ggui.Env
}

func newSidebarFoldButton(open *ggui.StateValue[bool], name string) *sidebarFoldButton {
	return &sidebarFoldButton{
		open: open,
		name: name,
		icon: glyph(lucide.ChevronRight, 14).Color(colorGray),
	}
}

func (b *sidebarFoldButton) Layout(c ggui.Constraints, env ggui.Env) ggui.Size {
	b.theme = uitheme.Of(env)
	b.env = env
	sz := ggui.Sz(20, 20)
	b.icon.Layout(ggui.Tight(ggui.Sz(14, 14)), env)
	return c.Constrain(sz)
}

func (b *sidebarFoldButton) Paint(dst *ggui.Canvas, rect ggui.Rect) {
	t := b.theme
	if t == nil {
		return
	}
	dst.Describe(rect, b)
	dst.HitPointer(rect, b)
	dst.HitCursor(rect, ggui.CursorShapePointer)
	if b.hover {
		dst.FillRoundRect(rect, 4, fadeSidebarColor(t.Fg, 0.08))
	}
	b.icon.Rotate(b.progress * math.Pi / 2)
	dst.Paint(b.icon, ggui.Rct(
		ggui.Pt(rect.Origin.X+(rect.Size.W-14)/2, rect.Origin.Y+(rect.Size.H-14)/2),
		ggui.Sz(14, 14),
	))
}

func (b *sidebarFoldButton) HandlePointer(ev ggui.PointerEvent) bool {
	switch ev.Kind {
	case ggui.PointerEnter, ggui.PointerMove:
		if !b.hover {
			b.hover = true
			ggui.Invalidate(b.env)
		}
	case ggui.PointerExit:
		if b.hover {
			b.hover = false
			ggui.Invalidate(b.env)
		}
	case ggui.PointerTap:
		if ev.Button == ggui.MouseButtonLeft {
			b.open.Set(!ggui.Peek(b.open))
		}
	case ggui.PointerScroll:
		return false
	}
	return true
}

func (b *sidebarFoldButton) Describe() ggui.Node {
	expanded := ggui.Untrack(b.open.Get)
	label := tr("Unfold %s", b.name)
	act := ggui.ActionPress | ggui.ActionExpand
	if expanded {
		label = tr("Fold %s", b.name)
		act = ggui.ActionPress | ggui.ActionCollapse
	}
	return ggui.Node{
		Role:     ggui.RoleDisclosure,
		Name:     label,
		Expanded: ggui.Expandable(expanded),
		Actions:  act,
	}
}

func (b *sidebarFoldButton) Act(a ggui.Action) bool {
	switch a.Kind {
	case ggui.ActionPress:
		b.open.Set(!ggui.Peek(b.open))
		return true
	case ggui.ActionExpand:
		b.open.Set(true)
		return true
	case ggui.ActionCollapse:
		b.open.Set(false)
		return true
	}
	return false
}

func (b *sidebarFoldButton) Adopt(prev any) {
	p, ok := prev.(*sidebarFoldButton)
	if !ok || p == b {
		return
	}
	b.hover = p.hover
}

func fadeSidebarColor(c color.Color, k float64) color.Color {
	if c == nil || k <= 0 {
		return color.RGBA{}
	}
	if k >= 1 {
		return c
	}
	n := color.NRGBAModel.Convert(c).(color.NRGBA)
	n.A = uint8(math.Round(float64(n.A) * k))
	return n
}

func mixSidebarColor(a, b color.Color, k float64) color.Color {
	if k <= 0 {
		return a
	}
	if k >= 1 {
		return b
	}
	ca := color.NRGBAModel.Convert(a).(color.NRGBA)
	cb := color.NRGBAModel.Convert(b).(color.NRGBA)
	lerp := func(x, y uint8) uint8 { return uint8(math.Round(float64(x) + (float64(y)-float64(x))*k)) }
	return color.NRGBA{
		R: lerp(ca.R, cb.R),
		G: lerp(ca.G, cb.G),
		B: lerp(ca.B, cb.B),
		A: lerp(ca.A, cb.A),
	}
}
