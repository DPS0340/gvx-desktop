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
	head := widget.Padding(widget.Row(
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
	).Gap(8).Align(widget.AlignCenter), 0, 32, 0, 0)

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
	type viewState struct {
		loading bool
		err     string
		note    string
	}
	state := ggui.Derived(func() viewState {
		return viewState{
			loading: m.ExploreLoading.Get(),
			err:     m.ExploreErr.Get(),
			note:    m.ExploreFilesNote.Get(),
		}
	})
	return ggui.View(state, func(s viewState) ggui.Widget {
		switch {
		case s.loading:
			msg := ggui.Map(m.ExploreProgress, func(p string) string {
				if p != "" {
					return p
				}
				return tr("Reading image filesystem…")
			})
			return widget.Box(widget.Row(ui.Spinner(), ui.CaptionOf(msg)).Gap(10).Align(widget.AlignCenter)).Height(470)
		case s.note != "":
			return widget.Box(ui.Empty(
				tr("Files unavailable"),
				s.note,
			).Media(glyph(lucide.FolderSearch, 32).Color(colorGray)).Outline()).Height(470)
		case s.err != "":
			return widget.Box(ui.Alert(tr("Could not read image filesystem"), s.err).Destructive().Icon(icons.Alert)).Height(470)
		}
		dirErr := ggui.View(m.ExploreDirErr, func(err string) ggui.Widget {
			if err == "" {
				return widget.Box()
			}
			return ui.Alert(tr("Could not open folder"), err).Destructive().Icon(icons.Alert).
				Action(ui.Button(tr("Retry"), func() { m.ExploreOpenDir(ggui.Peek(m.ExplorePath)) }).Outline().Size(ui.ButtonExtraSmall))
		})
		return widget.Column(
			explorerNavBar(m),
			dirErr,
			widget.Box(
				ui.Resizable(m.ExploreSplit, explorerDirPane(m), explorerPreviewPane(m)).
					WithHandle().MinSizes(280, 280).Name(tr("Image explorer")),
			).Height(explorerPaneHeight).Radius(8).Border(1, colorBorder).Clip(),
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
			var cur strings.Builder
			for i, seg := range parts {
				cur.WriteString("/" + seg)
				target := cur.String()
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

	clearQuery := ggui.If(ggui.Map(m.ExploreQuery, func(q string) bool { return q != "" }), func() ggui.Widget {
		return iconButton(lucide.X, tr("Clear the filter"), func() { m.ExploreQuery.Set("") })
	})
	filter := widget.Row(
		widget.Box(ui.TextField(m.ExploreQuery).Placeholder(tr("Filter files…")).Name(tr("Filter files"))).Width(180),
		clearQuery,
	).Gap(4).Align(widget.AlignCenter)
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
	type dirListState struct {
		entries  []imagefs.Entry
		layer    bool
		hasQuery bool
	}
	list := ggui.View(ggui.Derived(func() dirListState {
		return dirListState{
			entries:  visible.Get(),
			layer:    showLayer.Get(),
			hasQuery: strings.TrimSpace(m.ExploreQuery.Get()) != "",
		}
	}).WithEqual(nil), func(v dirListState) ggui.Widget {
		if len(v.entries) == 0 {
			if v.hasQuery {
				return widget.Padding(ui.Caption(tr("No matching files.")), 16)
			}
			return widget.Padding(ui.Caption(tr("Empty folder.")), 16)
		}
		rows := make([]ggui.Widget, len(v.entries))
		for i, e := range v.entries {
			rows[i] = explorerEntryRow(m, e, v.layer)
		}
		return widget.Padding(widget.Column(rows...).Gap(2).Stretch(), 6)
	})
	return widget.Scroll(list).FadeEdges(8)
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
	nameCol := []ggui.Widget{widget.Flexible(nameText)}
	if e.Kind == imagefs.KindSymlink && e.Target != "" {
		nameCol = append(nameCol, widget.Flexible(ui.Caption("→ "+e.Target).Size(11).NoWrap().Ellipsis()))
	}
	sizeStr := "—"
	if e.Kind == imagefs.KindFile {
		sizeStr = formatBytes(e.Size)
	}
	parts := []ggui.Widget{
		icon,
		widget.Expanded(widget.Row(nameCol...).Gap(6).Align(widget.AlignCenter)),
	}
	if showLayer && e.Layer > 0 {
		parts = append(parts, tip(ui.Mono(fmt.Sprintf("L%d", e.Layer)).Size(10).Color(colorGray).NoWrap(), tr("Layer %d", e.Layer)))
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
			count := ui.CaptionOf(ggui.Derived(func() string {
				dir := m.ExplorePath.Get()
				all := m.ExploreEntries.Get()
				q := m.ExploreQuery.Get()
				if strings.TrimSpace(q) != "" {
					shown := filterExploreEntries(all, q)
					return fmt.Sprintf("%s · %s", dir, showing(len(shown), len(all)))
				}
				return fmt.Sprintf("%s · %s", dir, trn(len(all), "%d item", "%d items"))
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
			meta += " · " + formatBytes(e.Size)
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
				notices = append(notices, ui.Caption(tr("Showing first %s of %s.", formatBytes(shown), formatBytes(p.TotalSize))).Size(11))
			}
			parts = append(parts, widget.Row(notices...).Gap(8).Align(widget.AlignCenter))
		}
		if p.Body == "" && !p.Binary {
			parts = append(parts, ui.Caption(tr("Empty file.")))
		} else {
			cb := ui.CodeBlock(p.Body).Small().LineNumbers(!p.Binary).CopyButton(true)
			if p.Language != "" {
				cb = cb.Language(p.Language)
			}
			parts = append(parts, widget.Expanded(cb))
		}
		return widget.Column(parts...).Gap(8).Stretch()
	})
	return widget.Padding(body, 12)
}

func explorerLayersView(m *Model) ggui.Widget {
	type layerState struct {
		loading bool
		err     string
	}
	state := ggui.Derived(func() layerState {
		return layerState{
			loading: m.ExploreLoading.Get(),
			err:     m.ExploreErr.Get(),
		}
	})
	return ggui.View(state, func(s layerState) ggui.Widget {
		switch {
		case s.loading:
			return widget.Box(widget.Row(ui.Spinner(), ui.Caption(tr("Reading layers…"))).Gap(10).Align(widget.AlignCenter)).Height(470)
		case s.err != "":
			return widget.Box(ui.Alert(tr("Could not read layers"), s.err).Destructive().Icon(icons.Alert)).Height(470)
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
					sizeText = formatBytes(layer.Size)
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
				col := []ggui.Widget{tip(ui.Mono(cmd).Size(12).NoWrap().Ellipsis(), cmd)}
				if len(metaParts) > 0 {
					col = append(col, ui.Caption(strings.Join(metaParts, " · ")).Size(11).NoWrap())
				}
				rows[i] = widget.Box(widget.Row(
					ui.Badge(fmt.Sprintf("#%d", layer.Index)).Outline(),
					widget.Expanded(widget.Column(col...).Gap(2)),
					widget.Box(ui.Caption(sizeText).Size(12).NoWrap()).Width(72),
					act,
				).Gap(10).Align(widget.AlignCenter)).Pad(8, 10).Radius(6).Border(1, colorBorder)
			}
			return widget.Box(widget.Scroll(widget.Column(rows...).Gap(6).Stretch()).FadeEdges(8)).Height(470)
		})
	})
}
