package ui

import (
	"fmt"
	"strings"
	"time"

	"github.com/ironpark/ggui"
	"github.com/ironpark/ggui/icons/lucide"
	"github.com/ironpark/ggui/ui"
	"github.com/ironpark/ggui/ui/icons"
	"github.com/ironpark/ggui/widget"

	"github.com/ironpark/gvx-desktop/internal/rungv"
	"github.com/ironpark/gvx-desktop/internal/sandbox"
)

// ---- Images -------------------------------------------------------------

func imagesPage(m *Model) ggui.Widget {
	visible := ggui.Combine(m.Images, m.ImageQuery, func(l Listing[rungv.Image], q string) []rungv.Image {
		q = strings.ToLower(strings.TrimSpace(q))
		var out []rungv.Image
		for _, img := range l.Items {
			if q == "" || strings.Contains(strings.ToLower(img.Repository+":"+img.Tag+" "+img.ID), q) {
				out = append(out, img)
			}
		}
		return out
	}).WithEqual(nil)
	summary := ui.CaptionOf(ggui.Combine(m.Images, m.Disk, func(l Listing[rungv.Image], d []rungv.DiskUsage) string {
		var s strings.Builder
		s.WriteString(fmt.Sprintf(tr("%d cached images"), len(l.Items)))
		for _, r := range d {
			if r.Type == "Images" {
				s.WriteString(fmt.Sprintf(tr(" · %s on disk · %s reclaimable"), r.Size, r.Reclaimable))
			}
		}
		return s.String() + tr(". Layers are compiled to read-only EROFS files, never extracted onto the host.")
	}))
	used := func(ref string) []string {
		var names []string
		for _, r := range ggui.Peek(m.Rows) {
			if r.Spec.Image == ref || r.Spec.Image+":latest" == ref {
				names = append(names, r.Spec.Name)
			}
		}
		return names
	}
	table := ui.Table(visible, rungv.Image.Key,
		ui.Col(tr("Repository"), func(r ggui.Readable[rungv.Image]) ggui.Widget {
			return widget.Row(
				glyph(lucide.Layers, 16).Color(colorGray),
				widget.TextOf(ggui.Map(r, func(i rungv.Image) string { return i.Repository })).Size(13).Weight(ggui.WeightMedium).NoWrap().Ellipsis(),
			).Gap(10)
		}).Grow(3),
		ui.TextCol(tr("Tag"), func(i rungv.Image) string { return i.Tag }).Grow(1),
		ui.Col(tr("Image ID"), func(r ggui.Readable[rungv.Image]) ggui.Widget {
			return ui.MonoOf(ggui.Map(r, func(i rungv.Image) string { return i.ID })).Size(12)
		}).W(130),
		ui.TextCol(tr("Created"), imageAge).W(130),
		ui.TextCol(tr("Size"), func(i rungv.Image) string { return i.Size }).W(90).Right(),
		ui.Col("", func(r ggui.Readable[rungv.Image]) ggui.Widget {
			return ggui.View(r, func(img rungv.Image) ggui.Widget {
				ref := img.Ref()
				return widget.Row(
					tip(iconButton(lucide.Play, tr("Run %s", ref), func() { m.NewSandbox(ref) }), tr("Run in a new sandbox")),
					ui.MenuOf(glyph(lucide.Ellipsis, 16),
						ui.MenuItem(tr("Run…"), func() { m.NewSandbox(ref) }),
						ui.MenuItem(tr("Inspect"), func() { m.InspectImage(ref) }),
						ui.MenuItem(tr("Pull again"), func() { m.OpenPull(ref); m.Pull() }),
						ui.MenuItem(tr("Copy reference"), func() { m.Copy(ref, ref) }),
						ui.MenuDivider(),
						ui.MenuItem(tr("Remove"), func() {
							if users := used(ref); len(users) > 0 {
								m.Confirm(tr("Remove %s?", ref), tr("Used by %s. They pull it again at their next start.", strings.Join(users, ", ")), tr("Remove image"), func() { m.AskRemoveImage(img) })
								return
							}
							m.AskRemoveImage(img)
						}).Destructive(),
					).Name(tr("More actions for %s", ref)),
				).Gap(2).Justify(widget.JustifyEnd)
			})
		}).W(84).Identified("actions"),
	).BindSelected(m.SelectedImage).RowName(rungv.Image.Ref).RowHeight(tableRow).OnSelect(func(img rungv.Image) { m.InspectImage(img.Ref()) })

	body := ggui.View(ggui.Map(m.Images, func(l Listing[rungv.Image]) int {
		switch {
		case l.Err != "":
			return 2
		case l.Loaded && len(l.Items) == 0:
			return 1
		case !l.Loaded:
			return 3
		}
		return 0
	}), func(state int) ggui.Widget {
		switch state {
		case 1:
			return ui.Empty(tr("No images cached"), tr("Pull an image, or start a sandbox: a missing image is pulled on first run.")).
				Media(glyph(lucide.Layers, 32).Color(colorGray)).
				Action(ui.Button(tr("Pull an image"), func() { m.OpenPull("") }).Icon(glyph(lucide.Download, 16))).Outline()
		case 2:
			return ui.Alert(tr("Could not list images"), ggui.Peek(m.Images).Err).Destructive().Icon(icons.Alert)
		case 3:
			return widget.Row(ui.Spinner(), ui.Caption(tr("Reading the image cache…"))).Gap(8)
		}
		count := ggui.Combine(visible, m.Images, func(v []rungv.Image, l Listing[rungv.Image]) string { return showing(len(v), len(l.Items)) })
		return tableCard(table.Toolbar(filterBar(m.ImageQuery, tr("Filter images"), count)))
	})
	return page(
		pageHeader(tr("Images"), summary,
			tip(iconButton(lucide.RefreshCw, tr("Refresh images"), m.LoadImages), tr("Refresh")),
			ui.Button(tr("Prune"), m.AskPruneImages).Outline().Icon(glyph(lucide.Eraser, 16)),
			ui.Button(tr("Pull image"), func() { m.OpenPull("") }).Icon(glyph(lucide.Download, 16)),
		),
		body,
	)
}

// ---- Volumes ------------------------------------------------------------

func volumesPage(m *Model) ggui.Widget {
	summary := ui.CaptionOf(ggui.Combine(m.Volumes, m.Disk, func(l Listing[rungv.Volume], d []rungv.DiskUsage) string {
		var s strings.Builder
		s.WriteString(fmt.Sprintf(tr("%d volumes"), len(l.Items)))
		for _, r := range d {
			if r.Type == "Local Volumes" {
				s.WriteString(tr(" · %s on disk", r.Size))
			}
		}
		return s.String() + tr(". Each ext4lite volume is one journaled ext4 image file that grows on demand.")
	}))
	table := ui.Table(ggui.Map(m.Volumes, func(l Listing[rungv.Volume]) []rungv.Volume { return l.Items }).WithEqual(nil),
		func(v rungv.Volume) string { return v.Name },
		ui.Col(tr("Name"), func(r ggui.Readable[rungv.Volume]) ggui.Widget {
			return widget.Row(
				glyph(lucide.HardDrive, 16).Color(colorGray),
				widget.TextOf(ggui.Map(r, func(v rungv.Volume) string { return v.Name })).Size(13).Weight(ggui.WeightMedium).NoWrap().Ellipsis(),
			).Gap(10)
		}).Grow(2),
		ui.TextCol(tr("Driver"), func(v rungv.Volume) string { return v.Driver }).W(100),
		ui.Col(tr("Status"), func(r ggui.Readable[rungv.Volume]) ggui.Widget {
			return ggui.View(ggui.Combine(r, m.Rows, func(v rungv.Volume, rows []Row) string {
				users := VolumeUsers(rows, v.Name)
				if v.InUse {
					return "in use"
				}
				if len(users) > 0 {
					return tr("used by ") + strings.Join(users, ", ")
				}
				return ""
			}), func(s string) ggui.Widget {
				switch {
				case s == "in use":
					return widget.Row(dot(colorGreen, 7), widget.Text(tr("In use")).Size(12)).Gap(6)
				case s != "":
					return ui.Caption(s).Size(12).NoWrap().Ellipsis()
				}
				return ui.Caption(tr("unused")).Size(12)
			})
		}).Grow(2),
		ui.TextCol(tr("Size"), func(v rungv.Volume) string { return v.Size }).W(90).Right(),
		ui.TextCol(tr("Created"), func(v rungv.Volume) string { return shortDate(v.CreatedAt) }).W(140),
		ui.Col("", func(r ggui.Readable[rungv.Volume]) ggui.Widget {
			return ggui.View(r, func(v rungv.Volume) ggui.Widget {
				return widget.Row(
					tip(iconButton(lucide.FolderOpen, tr("Reveal %s", v.Name), func() { m.Open(v.Mountpoint) }), tr("Reveal in file manager")),
					tip(iconButton(lucide.Trash, tr("Delete %s", v.Name), func() { m.AskRemoveVolume(v) }).Disabled(v.InUse), tr("Delete")),
				).Gap(2).Justify(widget.JustifyEnd)
			})
		}).W(84).Identified("actions"),
	).BindSelected(m.SelectedVolume).RowName(func(v rungv.Volume) string { return v.Name }).RowHeight(tableRow)

	body := ggui.View(ggui.Map(m.Volumes, func(l Listing[rungv.Volume]) int {
		switch {
		case l.Err != "":
			return 2
		case l.Loaded && len(l.Items) == 0:
			return 1
		case !l.Loaded:
			return 3
		}
		return 0
	}), func(state int) ggui.Widget {
		switch state {
		case 1:
			return ui.Empty(tr("No volumes"), tr("A sandbox creates a named volume the first time it mounts one, or create one here.")).
				Media(glyph(lucide.HardDrive, 32).Color(colorGray)).
				Action(ui.Button(tr("Create volume"), func() { m.VolumeOpen.Set(true) }).Icon(glyph(lucide.Plus, 16))).Outline()
		case 2:
			return ui.Alert(tr("Could not list volumes"), ggui.Peek(m.Volumes).Err).Destructive().Icon(icons.Alert)
		case 3:
			return widget.Row(ui.Spinner(), ui.Caption(tr("Reading volumes…"))).Gap(8)
		}
		return widget.Column(
			tableCard(table),
			ui.Caption(tr("A volume can be mounted by one sandbox at a time: its image file is locked while in use. Linux can mount it directly with mount -o loop.")),
		).Gap(12).Stretch()
	})
	return page(
		pageHeader(tr("Volumes"), summary,
			tip(iconButton(lucide.RefreshCw, tr("Refresh volumes"), m.LoadVolumes), tr("Refresh")),
			ui.Button(tr("Prune"), m.AskPruneVolumes).Outline().Icon(glyph(lucide.Eraser, 16)),
			ui.Button(tr("Create volume"), func() { m.VolumeOpen.Set(true) }).Icon(glyph(lucide.Plus, 16)),
		),
		body,
	)
}

func shortDate(s string) string {
	// rungv writes RFC 3339 or "2006-01-02 15:04:05 -0700 MST".
	if len(s) >= 16 {
		return strings.Replace(s[:16], "T", " ", 1)
	}
	return s
}

// ---- Snapshots ----------------------------------------------------------

func snapshotsPage(m *Model) ggui.Widget {
	summary := ui.CaptionOf(ggui.Map(m.Snapshots, func(s []sandbox.Snapshot) string {
		var total int64
		for _, x := range s {
			total += x.Size
		}
		return fmt.Sprintf(tr("%d snapshots · %s. Checkpoint a running sandbox and resume it later, in milliseconds."), len(s), humanBytes(total))
	}))
	enabled := ggui.Map(m.Specs, func(specs []sandbox.Spec) []sandbox.Spec {
		var out []sandbox.Spec
		for _, s := range specs {
			if s.Checkpoint {
				out = append(out, s)
			}
		}
		return out
	}).WithEqual(nil)
	hint := ggui.View(enabled, func(specs []sandbox.Spec) ggui.Widget {
		if len(specs) > 0 {
			names := make([]string, len(specs))
			for i, s := range specs {
				names[i] = s.Name
			}
			return ui.Caption(tr("Snapshots are on for: %s. Take one from the sandbox's Snapshots tab.", strings.Join(names, ", ")))
		}
		return ui.Alert(tr("No sandbox has snapshots turned on"),
			tr("Turn on \"Allow snapshots\" in a sandbox's settings: gvx opens the bundle directory and a control socket before the guest starts.")).Icon(icons.Info)
	})
	return page(
		pageHeader(tr("Snapshots"), summary,
			tip(iconButton(lucide.RefreshCw, tr("Refresh snapshots"), m.LoadSnapshots), tr("Refresh")),
		),
		hint,
		snapshotList(m, m.Snapshots, true),
	)
}

// imageAge says how long ago an image was made, in the app's language;
// rungv's own words for it are English.
func imageAge(i rungv.Image) string {
	t, err := time.Parse("2006-01-02 15:04:05 -0700 MST", i.CreatedAt)
	if err != nil {
		return i.CreatedSince
	}
	return since(t, time.Now())
}
