package ui

import (
	"fmt"
	"strings"

	"github.com/ironpark/ggui"
	"github.com/ironpark/ggui/icons/lucide"
	"github.com/ironpark/ggui/ui"
	"github.com/ironpark/ggui/ui/icons"

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
		s := fmt.Sprintf("%d cached images", len(l.Items))
		for _, r := range d {
			if r.Type == "Images" {
				s += fmt.Sprintf(" · %s on disk · %s reclaimable", r.Size, r.Reclaimable)
			}
		}
		return s + ". Layers are compiled to read-only EROFS files, never extracted onto the host."
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
		ui.Col("Repository", func(r ggui.Readable[rungv.Image]) ggui.Widget {
			return ggui.Row(
				glyph(lucide.Layers, 16).Color(colorGray),
				ggui.TextOf(ggui.Map(r, func(i rungv.Image) string { return i.Repository })).Size(13).Weight(ggui.WeightMedium).NoWrap().Ellipsis(),
			).Gap(10)
		}).Grow(3),
		ui.TextCol("Tag", func(i rungv.Image) string { return i.Tag }).Grow(1),
		ui.Col("Image ID", func(r ggui.Readable[rungv.Image]) ggui.Widget {
			return ui.MonoOf(ggui.Map(r, func(i rungv.Image) string { return i.ID })).Size(12)
		}).W(130),
		ui.TextCol("Created", func(i rungv.Image) string { return i.CreatedSince }).W(130),
		ui.TextCol("Size", func(i rungv.Image) string { return i.Size }).W(90).Right(),
		ui.Col("", func(r ggui.Readable[rungv.Image]) ggui.Widget {
			return ggui.View(r, func(img rungv.Image) ggui.Widget {
				ref := img.Ref()
				return ggui.Row(
					tip(iconButton(lucide.Play, "Run "+ref, func() { m.NewSandbox(ref) }), "Run in a new sandbox"),
					ui.MenuOf(glyph(lucide.Ellipsis, 16),
						ui.MenuItem("Run…", func() { m.NewSandbox(ref) }),
						ui.MenuItem("Inspect", func() { m.InspectImage(ref) }),
						ui.MenuItem("Pull again", func() { m.OpenPull(ref); m.Pull() }),
						ui.MenuItem("Copy reference", func() { m.Copy(ref, ref) }),
						ui.MenuDivider(),
						ui.MenuItem("Remove", func() {
							if users := used(ref); len(users) > 0 {
								m.Confirm("Remove "+ref+"?", "Used by "+strings.Join(users, ", ")+". They pull it again at their next start.", "Remove image", func() { m.AskRemoveImage(img) })
								return
							}
							m.AskRemoveImage(img)
						}).Destructive(),
					).Name("More actions for "+ref),
				).Gap(2).Justify(ggui.JustifyEnd)
			})
		}).W(84).Identified("actions"),
	).BindSelected(m.SelectedImage).RowName(rungv.Image.Ref).OnSelect(func(img rungv.Image) { m.InspectImage(img.Ref()) })

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
			return ui.Empty("No images cached", "Pull an image, or start a sandbox: a missing image is pulled on first run.").
				Media(glyph(lucide.Layers, 32).Color(colorGray)).
				Action(ui.Button("Pull an image", func() { m.OpenPull("") }).Icon(glyph(lucide.Download, 16))).Outline()
		case 2:
			return ui.Alert("Could not list images", ggui.Peek(m.Images).Err).Destructive().Icon(icons.Alert)
		case 3:
			return ggui.Row(ui.Spinner(), ui.Caption("Reading the image cache…")).Gap(8)
		}
		return ggui.Column(
			ggui.Box(ui.TextField(m.ImageQuery).Placeholder("Filter images").Name("Filter images")).MaxWidth(320),
			ui.Card(table).Pad(0),
		).Gap(12).Stretch()
	})
	return page(
		pageHeader("Images", summary,
			tip(iconButton(lucide.RefreshCw, "Refresh images", m.LoadImages), "Refresh"),
			ui.Button("Prune", m.AskPruneImages).Outline().Icon(glyph(lucide.Eraser, 16)),
			ui.Button("Pull image", func() { m.OpenPull("") }).Icon(glyph(lucide.Download, 16)),
		),
		body,
	)
}

// ---- Volumes ------------------------------------------------------------

func volumesPage(m *Model) ggui.Widget {
	summary := ui.CaptionOf(ggui.Combine(m.Volumes, m.Disk, func(l Listing[rungv.Volume], d []rungv.DiskUsage) string {
		s := fmt.Sprintf("%d volumes", len(l.Items))
		for _, r := range d {
			if r.Type == "Local Volumes" {
				s += " · " + r.Size + " on disk"
			}
		}
		return s + ". Each ext4lite volume is one journaled ext4 image file that grows on demand."
	}))
	table := ui.Table(ggui.Map(m.Volumes, func(l Listing[rungv.Volume]) []rungv.Volume { return l.Items }).WithEqual(nil),
		func(v rungv.Volume) string { return v.Name },
		ui.Col("Name", func(r ggui.Readable[rungv.Volume]) ggui.Widget {
			return ggui.Row(
				glyph(lucide.HardDrive, 16).Color(colorGray),
				ggui.TextOf(ggui.Map(r, func(v rungv.Volume) string { return v.Name })).Size(13).Weight(ggui.WeightMedium).NoWrap().Ellipsis(),
			).Gap(10)
		}).Grow(2),
		ui.TextCol("Driver", func(v rungv.Volume) string { return v.Driver }).W(100),
		ui.Col("Status", func(r ggui.Readable[rungv.Volume]) ggui.Widget {
			return ggui.View(ggui.Combine(r, m.Rows, func(v rungv.Volume, rows []Row) string {
				users := VolumeUsers(rows, v.Name)
				if v.InUse {
					return "in use"
				}
				if len(users) > 0 {
					return "used by " + strings.Join(users, ", ")
				}
				return ""
			}), func(s string) ggui.Widget {
				switch {
				case s == "in use":
					return ggui.Row(dot(colorGreen, 7), ggui.Text("In use").Size(12)).Gap(6)
				case s != "":
					return ui.Caption(s).Size(12).NoWrap().Ellipsis()
				}
				return ui.Caption("unused").Size(12)
			})
		}).Grow(2),
		ui.TextCol("Size", func(v rungv.Volume) string { return v.Size }).W(90).Right(),
		ui.TextCol("Created", func(v rungv.Volume) string { return shortDate(v.CreatedAt) }).W(140),
		ui.Col("", func(r ggui.Readable[rungv.Volume]) ggui.Widget {
			return ggui.View(r, func(v rungv.Volume) ggui.Widget {
				return ggui.Row(
					tip(iconButton(lucide.FolderOpen, "Reveal "+v.Name, func() { m.Open(v.Mountpoint) }), "Reveal in file manager"),
					tip(iconButton(lucide.Trash, "Delete "+v.Name, func() { m.AskRemoveVolume(v) }).Disabled(v.InUse), "Delete"),
				).Gap(2).Justify(ggui.JustifyEnd)
			})
		}).W(84).Identified("actions"),
	).BindSelected(m.SelectedVolume).RowName(func(v rungv.Volume) string { return v.Name })

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
			return ui.Empty("No volumes", "A sandbox creates a named volume the first time it mounts one, or create one here.").
				Media(glyph(lucide.HardDrive, 32).Color(colorGray)).
				Action(ui.Button("Create volume", func() { m.VolumeOpen.Set(true) }).Icon(glyph(lucide.Plus, 16))).Outline()
		case 2:
			return ui.Alert("Could not list volumes", ggui.Peek(m.Volumes).Err).Destructive().Icon(icons.Alert)
		case 3:
			return ggui.Row(ui.Spinner(), ui.Caption("Reading volumes…")).Gap(8)
		}
		return ggui.Column(
			ui.Card(table).Pad(0),
			ui.Caption("A volume can be mounted by one sandbox at a time: its image file is locked while in use. Linux can mount it directly with mount -o loop."),
		).Gap(12).Stretch()
	})
	return page(
		pageHeader("Volumes", summary,
			tip(iconButton(lucide.RefreshCw, "Refresh volumes", m.LoadVolumes), "Refresh"),
			ui.Button("Prune", m.AskPruneVolumes).Outline().Icon(glyph(lucide.Eraser, 16)),
			ui.Button("Create volume", func() { m.VolumeOpen.Set(true) }).Icon(glyph(lucide.Plus, 16)),
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
		return fmt.Sprintf("%d snapshots · %s. Checkpoint a running sandbox and resume it later, in milliseconds.", len(s), humanBytes(total))
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
			return ui.Caption("Snapshots are on for: " + strings.Join(names, ", ") + ". Take one from the sandbox's Snapshots tab.")
		}
		return ui.Alert("No sandbox has snapshots turned on",
			"Turn on \"Allow snapshots\" in a sandbox's settings: gvx opens the bundle directory and a control socket before the guest starts.").Icon(icons.Info)
	})
	return page(
		pageHeader("Snapshots", summary,
			tip(iconButton(lucide.RefreshCw, "Refresh snapshots", m.LoadSnapshots), "Refresh"),
		),
		hint,
		snapshotList(m, m.Snapshots, true),
	)
}
