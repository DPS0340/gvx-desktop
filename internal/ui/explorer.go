package ui

import (
	"fmt"
	"image/color"
	"path"
	"strings"

	"github.com/ironpark/ggui"
	"github.com/ironpark/ggui/icons/lucide"
	"github.com/ironpark/ggui/ui"
	"github.com/ironpark/ggui/ui/icons"
	uitheme "github.com/ironpark/ggui/ui/theme"
	"github.com/ironpark/ggui/widget"

	"github.com/ironpark/gvx-desktop/internal/imagefs"
)

// explorerDialog is the unified image explorer: browse files and preview
// contents, inspect layer history, and read the full configuration JSON.
func explorerDialog(m *Model) ggui.Widget {
	// Release open layer files and cancel background indexing when the dialog
	// closes (via Escape, scrim click or close button).
	ggui.Watch(m.ExploreOpen, func(open bool) {
		if !open {
			m.explorer.close()
		}
	})
	head := widget.Row(
		widget.Flexible(ui.HeadingOf(m.ExploreTitle).Size(17).NoWrap().Ellipsis()),
		ggui.View(m.ExploreSource, func(src string) ggui.Widget {
			if src == "" {
				return widget.Box()
			}
			return ui.Badge(src).Outline()
		}),
		ui.CaptionOf(ggui.Map(m.ExploreLayers, func(ls []imagefs.Layer) string {
			if len(ls) == 0 {
				return ""
			}
			return trn(len(ls), "%d layer", "%d layers")
		})).Size(12).NoWrap(),
	).Gap(8).Align(widget.AlignCenter)

	tabs := ui.Tabs(m.ExploreTab,
		ui.Tab(tr("Files"), widget.Padding(explorerFilesView(m), 10, 0, 0, 0)),
		ui.Tab(tr("Layers"), widget.Padding(explorerLayersView(m), 10, 0, 0, 0)),
		ui.Tab(tr("Inspect"), widget.Padding(
			ui.CodeBlockOf(m.ExploreInspect).Language("json").Height(470).LineNumbers(true),
			10, 0, 0, 0)),
	).Line().Name(tr("Image explorer"))

	return ui.Dialog(m.ExploreOpen, widget.Column(head, tabs).Gap(10).Stretch()).
		Width(960).Name(tr("Image explorer")).CloseButton(true)
}

func explorerFilesView(m *Model) ggui.Widget {
	state := ggui.Combine(m.ExploreLoading, m.ExploreErr, func(loading bool, err string) int {
		switch {
		case loading:
			return 1
		case err != "":
			return 2
		}
		return 0
	})
	return ggui.View(state, func(s int) ggui.Widget {
		switch s {
		case 1:
			msg := ggui.Map(m.ExploreProgress, func(p string) string {
				if p != "" {
					return p
				}
				return tr("Reading image filesystem…")
			})
			return widget.Box(widget.Row(ui.Spinner(), ui.CaptionOf(msg)).Gap(10).Align(widget.AlignCenter)).Height(470)
		case 2:
			return widget.Box(ui.Alert(tr("Could not read image filesystem"), ggui.Peek(m.ExploreErr)).Destructive().Icon(icons.Alert)).Height(470)
		}
		return widget.Column(
			explorerNavBar(m),
			widget.Row(
				explorerDirPane(m),
				widget.Expanded(explorerPreviewPane(m)),
			).Gap(12).Align(widget.AlignStretch),
		).Gap(10).Stretch()
	})
}

func explorerNavBar(m *Model) ggui.Widget {
	atRoot := ggui.Map(m.ExplorePath, func(p string) bool { return p == "" || p == "/" })
	up := tip(iconButton(lucide.ArrowUp, tr("Parent folder"), m.ExploreUp).BindDisabled(atRoot), tr("Parent folder"))

	crumbs := ggui.View(m.ExplorePath, func(p string) ggui.Widget {
		p = imagefs.CleanPath(p)
		items := []ui.BreadcrumbEntry{ui.Crumb("/", func() { m.ExploreOpenDir("/") })}
		if p != "/" {
			parts := strings.Split(strings.Trim(p, "/"), "/")
			cur := ""
			for i, seg := range parts {
				cur += "/" + seg
				target := cur
				var onTap func()
				if i < len(parts)-1 {
					onTap = func() { m.ExploreOpenDir(target) }
				}
				items = append(items, ui.Crumb(seg, onTap))
			}
		}
		return ui.Breadcrumb(items...).Separator("/").Max(5).Name(tr("Path"))
	})

	layerPicker := ggui.View(m.ExploreLayers, func(ls []imagefs.Layer) ggui.Widget {
		var opts []int
		for _, l := range ls {
			if l.HasFiles {
				opts = append(opts, l.Index)
			}
		}
		if len(opts) <= 1 {
			return widget.Box()
		}
		allOpts := append([]int{0}, opts...)
		pick := ggui.Bind(m.ExploreLayer.Get, m.ExploreSetLayer)
		return ui.Select(pick).Options(allOpts).Format(func(v int) string {
			if v == 0 {
				return tr("All layers")
			}
			return tr("Layer %d", v)
		}).Small().MinWidth(120).Name(tr("Layer"))
	})

	filter := widget.Box(ui.TextField(m.ExploreQuery).Placeholder(tr("Filter files…")).Name(tr("Filter files"))).Width(180)
	return widget.Row(up, widget.Expanded(crumbs), layerPicker, filter).Gap(8).Align(widget.AlignCenter)
}

const explorerPaneHeight = 426

func explorerDirPane(m *Model) ggui.Widget {
	visible := ggui.Combine(m.ExploreEntries, m.ExploreQuery, filterExploreEntries).WithEqual(nil)
	showLayer := ggui.Combine(m.ExploreLayers, m.ExploreLayer, func(ls []imagefs.Layer, active int) bool {
		if active != 0 {
			return false
		}
		n := 0
		for _, l := range ls {
			if l.HasFiles {
				n++
			}
		}
		return n > 1
	})
	list := ggui.View(ggui.Combine(visible, showLayer, func(es []imagefs.Entry, sl bool) struct {
		entries []imagefs.Entry
		layer   bool
	} {
		return struct {
			entries []imagefs.Entry
			layer   bool
		}{es, sl}
	}).WithEqual(nil), func(v struct {
		entries []imagefs.Entry
		layer   bool
	}) ggui.Widget {
		if len(v.entries) == 0 {
			return widget.Padding(ui.Caption(tr("Empty folder.")), 16)
		}
		rows := make([]ggui.Widget, len(v.entries))
		for i, e := range v.entries {
			rows[i] = explorerEntryRow(m, e, v.layer)
		}
		return widget.Padding(widget.Column(rows...).Gap(2).Stretch(), 6)
	})
	return widget.Box(widget.Scroll(list).FadeEdges(8)).Width(440).Height(explorerPaneHeight).Radius(8).Border(1, colorGrayA)
}

func explorerEntryRow(m *Model, e imagefs.Entry, showLayer bool) ggui.Widget {
	icon := glyph(lucide.File, 14).Color(colorGray)
	switch e.Kind {
	case imagefs.KindDir:
		icon = glyph(lucide.Folder, 14).Color(colorBlue)
	case imagefs.KindSymlink:
		icon = glyph(lucide.Link2, 14).Color(colorAmber)
	default:
		if imagefs.DetectLanguage(e.Name) != "" {
			icon = glyph(lucide.FileText, 14).Color(colorGray)
		}
	}
	nameText := widget.Text(e.Name).Size(12).NoWrap().Ellipsis()
	if e.Kind == imagefs.KindDir {
		nameText = nameText.Weight(ggui.WeightMedium)
	}
	nameCol := []ggui.Widget{nameText}
	if e.Kind == imagefs.KindSymlink && e.Target != "" {
		nameCol = append(nameCol, ui.Caption("→ "+e.Target).Size(11).NoWrap().Ellipsis())
	}
	sizeStr := "—"
	if e.Kind == imagefs.KindFile {
		sizeStr = humanBytes(e.Size)
	}
	parts := []ggui.Widget{
		icon,
		widget.Expanded(widget.Row(nameCol...).Gap(6).Align(widget.AlignCenter)),
	}
	if showLayer && e.Layer > 0 {
		parts = append(parts, ui.Mono(fmt.Sprintf("L%d", e.Layer)).Size(10).Color(colorGray).NoWrap())
	}
	parts = append(parts,
		ui.Mono(imagefs.FormatMode(e.Mode, e.Kind)).Size(10).Color(colorGray).NoWrap(),
		widget.Box(ui.Caption(sizeStr).Size(11).NoWrap()).Width(62),
	)
	content := widget.Row(parts...).Gap(8).Align(widget.AlignCenter)

	selected := ggui.Map(m.ExploreSelected, func(sel string) bool { return sel != "" && sel == e.Path })
	hover := ggui.State(false)
	fill := ggui.Combine(selected, hover, func(sel, over bool) color.Color {
		if sel || over {
			return uitheme.Var("accent")
		}
		return color.RGBA{}
	})
	return widget.Tap(
		widget.AnimatedBox(content).Pad(5, 8).Radius(6).BindFill(fill).Duration(hoverDuration),
		func() { m.ExploreSelectEntry(e) },
	).OnHover(hover.Set).Cursor(ggui.CursorShapePointer)
}

func explorerPreviewPane(m *Model) ggui.Widget {
	body := ggui.View(m.ExplorePreview, func(p FilePreview) ggui.Widget {
		if !p.HasEntry {
			count := ui.CaptionOf(ggui.Combine(m.ExplorePath, m.ExploreEntries, func(dir string, es []imagefs.Entry) string {
				return fmt.Sprintf("%s · %s", dir, trn(len(es), "%d item", "%d items"))
			})).Size(12)
			return widget.Align(widget.Column(
				glyph(lucide.FileText, 24).Color(colorGray),
				ui.Caption(tr("Select a file to preview its contents.")),
				count,
			).Gap(8).Align(widget.AlignCenter))
		}
		e := p.Entry
		meta := fmt.Sprintf("%s · %d:%d", imagefs.FormatMode(e.Mode, e.Kind), e.UID, e.GID)
		if e.Kind == imagefs.KindFile {
			meta += " · " + humanBytes(e.Size)
		}
		if !e.ModTime.IsZero() {
			meta += " · " + e.ModTime.Local().Format("2006-01-02 15:04")
		}
		header := widget.Column(
			widget.Row(
				widget.Expanded(widget.Text(path.Base(e.Path)).Size(13).Weight(ggui.WeightSemibold).NoWrap().Ellipsis()),
				tip(iconButton(lucide.Copy, tr("Copy path"), func() { m.Copy(e.Path, e.Path) }), tr("Copy path")),
			).Gap(6).Align(widget.AlignCenter),
			ui.Mono(e.Path).Size(11).Color(colorGray).NoWrap().Ellipsis(),
			ui.Caption(meta).Size(11).NoWrap().Ellipsis(),
		).Gap(2).Stretch()

		if p.Loading {
			return widget.Column(
				header,
				ui.Divider(),
				widget.Row(ui.Spinner().Size(14), ui.Caption(tr("Reading file…"))).Gap(8).Align(widget.AlignCenter),
			).Gap(10).Stretch()
		}
		if p.Err != "" {
			return widget.Column(
				header,
				ui.Divider(),
				ui.Alert(tr("Could not read file"), p.Err).Destructive().Icon(icons.Alert),
			).Gap(10).Stretch()
		}
		if e.Kind == imagefs.KindSymlink {
			return widget.Column(
				header,
				ui.Divider(),
				kvText(tr("Target"), e.Target),
				kvText(tr("Resolved"), imagefs.ResolveSymlink(e.Path, e.Target)),
				widget.Row(
					ui.Button(tr("Follow link"), func() { m.ExploreFollowSymlink(e) }).Outline().Size(ui.ButtonSmall).Icon(glyph(lucide.CornerDownRight, 14)),
				),
			).Gap(10).Stretch()
		}
		if e.Kind != imagefs.KindFile {
			return widget.Column(
				header,
				ui.Divider(),
				ui.Caption(tr("Special file (cannot be previewed).")),
			).Gap(10).Stretch()
		}

		parts := []ggui.Widget{header, ui.Divider()}
		if p.Binary || p.Truncated {
			var notices []ggui.Widget
			if p.Binary {
				notices = append(notices, ui.Badge(tr("Binary")).Outline())
			}
			if p.Truncated {
				shown := int64(previewMaxBytes)
				if p.Binary {
					shown = hexPreviewBytes
				}
				notices = append(notices, ui.Caption(tr("Showing first %s of %s.", humanBytes(shown), humanBytes(p.TotalSize))).Size(11))
			}
			parts = append(parts, widget.Row(notices...).Gap(8).Align(widget.AlignCenter))
		}
		if p.Body == "" && !p.Binary {
			parts = append(parts, ui.Caption(tr("Empty file.")))
		} else {
			cb := ui.CodeBlock(p.Body).Height(290).Small().LineNumbers(!p.Binary).CopyButton(true)
			if p.Language != "" {
				cb = cb.Language(p.Language)
			}
			parts = append(parts, cb)
		}
		return widget.Column(parts...).Gap(8).Stretch()
	})
	return widget.Box(widget.Padding(body, 12)).Height(explorerPaneHeight).Radius(8).Border(1, colorGrayA)
}

func explorerLayersView(m *Model) ggui.Widget {
	state := ggui.Combine(m.ExploreLoading, m.ExploreErr, func(loading bool, err string) int {
		switch {
		case loading:
			return 1
		case err != "":
			return 2
		}
		return 0
	})
	return ggui.View(state, func(s int) ggui.Widget {
		switch s {
		case 1:
			return widget.Box(widget.Row(ui.Spinner(), ui.Caption(tr("Reading layers…"))).Gap(10).Align(widget.AlignCenter)).Height(470)
		case 2:
			return widget.Box(ui.Alert(tr("Could not read layers"), ggui.Peek(m.ExploreErr)).Destructive().Icon(icons.Alert)).Height(470)
		}
		return ggui.View(m.ExploreLayers, func(ls []imagefs.Layer) ggui.Widget {
			if len(ls) == 0 {
				return widget.Box(ui.Caption(tr("No layer history available."))).Height(470)
			}
			rows := make([]ggui.Widget, len(ls))
			for i, l := range ls {
				layer := l
				cmd := layer.Command
				if cmd == "" {
					cmd = tr("(no command recorded)")
				}
				sizeText := "0 B"
				if layer.Size > 0 {
					sizeText = humanBytes(layer.Size)
				}
				var act ggui.Widget = widget.Box().Width(74)
				if layer.HasFiles {
					act = ui.Button(tr("Browse"), func() { m.ExploreBrowseLayer(layer.Index) }).Outline().Size(ui.ButtonExtraSmall)
				}
				metaParts := []string{}
				if d := imagefs.ShortDigest(layer.Digest); d != "" && d != "<missing>" {
					metaParts = append(metaParts, d)
				}
				if !layer.Created.IsZero() {
					metaParts = append(metaParts, layer.Created.Local().Format("2006-01-02 15:04"))
				}
				if layer.Empty {
					metaParts = append(metaParts, tr("empty"))
				}
				col := []ggui.Widget{ui.Mono(cmd).Size(12).NoWrap().Ellipsis()}
				if len(metaParts) > 0 {
					col = append(col, ui.Caption(strings.Join(metaParts, " · ")).Size(11).NoWrap())
				}
				rows[i] = widget.Box(widget.Row(
					ui.Badge(fmt.Sprintf("#%d", layer.Index)).Outline(),
					widget.Expanded(widget.Column(col...).Gap(2)),
					widget.Box(ui.Caption(sizeText).Size(12).NoWrap()).Width(72),
					act,
				).Gap(10).Align(widget.AlignCenter)).Pad(8, 10).Radius(6).Border(1, colorGrayA)
			}
			return widget.Box(widget.Scroll(widget.Column(rows...).Gap(6).Stretch()).FadeEdges(8)).Height(470)
		})
	})
}
