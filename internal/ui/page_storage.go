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

	"github.com/ironpark/gvx-desktop/internal/dockerapi"
	"github.com/ironpark/gvx-desktop/internal/recipes"
	"github.com/ironpark/gvx-desktop/internal/rungv"
	"github.com/ironpark/gvx-desktop/internal/sandbox"
)

// ---- Images -------------------------------------------------------------

func imagesPage(m *Model) ggui.Widget {
	header := ggui.View(m.ImageView, func(view int) ggui.Widget {
		if view == ImagesDocker {
			summary := ui.CaptionOf(ggui.Derived(func() string {
				open, l, se := m.ContainersOpen.Get(), m.DockerImages.Get(), m.Settings.Get().SandboxEngine
				if !open || !l.Loaded || l.Err != "" {
					if se {
						return tr("Images shared with gvx's image cache.")
					}
					return tr("Images built or pulled inside the Docker Engine sandbox.")
				}
				if se {
					return tr("%d images · %s. Shared with gvx's image cache.", len(l.Items), formatBytes(dockerImagesTotalSize(l.Items)))
				}
				return tr("%d images · %s. Images built or pulled inside the Docker Engine sandbox.", len(l.Items), formatBytes(dockerImagesTotalSize(l.Items)))
			}))
			return pageHeader(tr("Images"), summary,
				tip(iconButton(lucide.RefreshCw, tr("Refresh Docker images"), m.LoadDockerImages).BindDisabled(ggui.Not(m.ContainersOpen)), tr("Refresh")),
				ui.Button(tr("Prune"), m.AskPruneDockerImages).Outline().Icon(glyph(lucide.Eraser, 16)).BindDisabled(ggui.Not(m.ContainersOpen)),
				ui.Button(tr("Pull image"), func() { m.OpenDockerPull("") }).Icon(glyph(lucide.Download, 16)).BindDisabled(ggui.Not(m.ContainersOpen)),
			)
		}
		summary := ui.CaptionOf(ggui.Combine(m.Images, m.Disk, func(l Listing[rungv.Image], d []rungv.DiskUsage) string {
			var s strings.Builder
			s.WriteString(fmt.Sprintf(tr("%d cached images"), len(l.Items)))
			for _, r := range d {
				if r.Type == "Images" {
					s.WriteString(fmt.Sprintf(tr(" · %s on disk · %s reclaimable"), formatSize(r.Size), formatSize(r.Reclaimable)))
				}
			}
			return s.String() + tr(". Layers are compiled to read-only EROFS files, never extracted onto the host.")
		}))
		return pageHeader(tr("Images"), summary,
			tip(iconButton(lucide.RefreshCw, tr("Refresh images"), m.LoadImages), tr("Refresh")),
			ui.Button(tr("Prune"), m.AskPruneImages).Outline().Icon(glyph(lucide.Eraser, 16)),
			ui.Button(tr("Pull image"), func() { m.OpenPull("") }).Icon(glyph(lucide.Download, 16)),
		)
	})
	tabs := ui.Tabs(m.ImageView,
		ui.Tab(tr("GVX Sandboxes"), widget.Padding(gvxImagesView(m), 12, 0, 0, 0)).
			Header(tabHeader(lucide.Box, tr("GVX Sandboxes"))),
		ui.Tab("Docker Engine", widget.Padding(dockerImagesView(m), 12, 0, 0, 0)).
			Header(tabHeader(lucide.Container, "Docker Engine")),
	).Name(tr("Image sources"))
	return page(header, tabs)
}

func tabHeader(icon *icons.Glyph, label string) ggui.Widget {
	return widget.Row(glyph(icon, 14), widget.Text(label).NoWrap()).Gap(6).Align(widget.AlignCenter)
}

func gvxImagesView(m *Model) ggui.Widget {
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
		ui.Col(tr("Tag"), func(r ggui.Readable[rungv.Image]) ggui.Widget {
			return ui.MonoOf(ggui.Map(r, func(i rungv.Image) string { return i.Tag })).Size(12).NoWrap().Ellipsis()
		}).Grow(1),
		ui.Col(tr("Image ID"), func(r ggui.Readable[rungv.Image]) ggui.Widget {
			return ui.MonoOf(ggui.Map(r, func(i rungv.Image) string { return i.ID })).Size(12)
		}).W(130),
		ui.Col(tr("Created"), func(r ggui.Readable[rungv.Image]) ggui.Widget {
			return ui.CaptionOf(ggui.Combine(r, m.Now, imageAge)).Size(12).NoWrap()
		}).W(130),
		ui.TextCol(tr("Size"), func(i rungv.Image) string { return formatSize(i.Size) }).W(96).Right(),
		ui.Col("", func(r ggui.Readable[rungv.Image]) ggui.Widget {
			return ggui.View(r, func(img rungv.Image) ggui.Widget {
				ref := img.Ref()
				return widget.Row(
					tip(iconButton(lucide.FolderSearch, tr("Browse files of %s", ref), func() { m.ExploreGVXImage(img, ExploreFiles) }), tr("Browse files")),
					tip(iconButton(lucide.Play, tr("Run %s", ref), func() { m.NewSandbox(ref) }), tr("Run in a new sandbox")),
					moreMenu(tr("More actions for %s", ref),
						ui.MenuItem(tr("Browse files"), func() { m.ExploreGVXImage(img, ExploreFiles) }),
						ui.MenuItem(tr("Run…"), func() { m.NewSandbox(ref) }),
						ui.MenuItem(tr("Inspect"), func() { m.ExploreGVXImage(img, ExploreInspect) }),
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
					),
				).Gap(2).Justify(widget.JustifyEnd)
			})
		}).W(112).Identified("actions"),
	).BindSelected(m.SelectedImage).RowName(rungv.Image.Ref).RowHeight(tableRow).OnSelect(func(img rungv.Image) { m.ExploreGVXImage(img, ExploreFiles) })

	return ggui.View(ggui.Map(m.Images, func(l Listing[rungv.Image]) int {
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
}

// dockerImagesGate words the Docker Engine tab of Images while the engine does
// not run.
func dockerImagesGate() gateWords {
	return gateWords{
		recipe: recipes.Docker, page: PageDocker, icon: lucide.Layers,
		openPage: tr("Open Docker page"),
		notSetUp: [2]string{tr("The Docker Engine is not set up"),
			tr("Set it up to build and pull images with the docker CLI. They show here while it runs.")},
		starting: [2]string{tr("The Docker Engine is starting"), tr("Its images show here once dockerd is ready.")},
		stopped:  [2]string{tr("The Docker Engine is not running"), tr("Start it to see and manage its images here.")},
		start:    tr("Start Docker Engine"),
	}
}

func dockerImagesView(m *Model) ggui.Widget {
	listing := m.DockerImages
	all := ggui.Map(listing, func(l Listing[dockerapi.Image]) []DockerImageRow {
		return dockerImageRows(l.Items)
	}).WithEqual(nil)
	visible := ggui.Combine(all, m.DockerImageQuery, filterDockerImages).WithEqual(nil)

	table := ui.Table(visible, DockerImageRow.Key,
		ui.Col(tr("Repository"), func(r ggui.Readable[DockerImageRow]) ggui.Widget {
			type repoCell struct {
				repo    string
				users   string
				arch    string
				foreign bool
			}
			return ggui.View(ggui.Derived(func() repoCell {
				img := r.Get()
				ctrs := m.DockerContainers.Get()
				users := dockerImageAllUsers(ctrs.Items, m.Rows.Get(), m.Settings.Get().SandboxEngine, img)
				return repoCell{
					repo:    img.Repository,
					users:   strings.Join(users, ", "),
					arch:    img.Arch,
					foreign: img.ForeignArch(),
				}
			}), func(info repoCell) ggui.Widget {
				parts := []ggui.Widget{
					glyph(lucide.Layers, 16).Color(colorGray),
					widget.Flexible(widget.Text(info.repo).Size(13).Weight(ggui.WeightMedium).NoWrap().Ellipsis()),
				}
				if info.foreign {
					parts = append(parts, tip(ui.Badge(info.arch).Outline(), tr("Built for linux/%s; cannot be run in a sandbox yet.", info.arch)))
				}
				if info.users != "" {
					parts = append(parts, tip(ui.Badge(tr("In use")).Outline(), tr("Used by %s", info.users)))
				}
				return widget.Row(parts...).Gap(8).Align(widget.AlignCenter)
			})
		}).Grow(3),
		ui.Col(tr("Tag"), func(r ggui.Readable[DockerImageRow]) ggui.Widget {
			return ui.MonoOf(ggui.Map(r, func(i DockerImageRow) string { return i.Tag })).Size(12).NoWrap().Ellipsis()
		}).Grow(1),
		ui.Col(tr("Image ID"), func(r ggui.Readable[DockerImageRow]) ggui.Widget {
			return ui.MonoOf(ggui.Map(r, func(i DockerImageRow) string { return i.ShortID })).Size(12)
		}).W(130),
		ui.Col(tr("Created"), func(r ggui.Readable[DockerImageRow]) ggui.Widget {
			return ui.CaptionOf(ggui.Combine(r, m.Now, func(i DockerImageRow, now time.Time) string {
				if i.Created == 0 {
					return "—"
				}
				return since(time.Unix(i.Created, 0), now)
			})).Size(12).NoWrap()
		}).W(130),
		ui.TextCol(tr("Size"), func(i DockerImageRow) string { return formatBytes(i.Size) }).W(96).Right(),
		ui.Col("", func(r ggui.Readable[DockerImageRow]) ggui.Widget {
			return ggui.View(r, func(img DockerImageRow) ggui.Widget {
				ref := img.Ref()
				entries := []ggui.Widget{
					ui.MenuItem(tr("Browse files"), func() { m.ExploreDockerImage(img, ExploreFiles) }),
					ui.MenuItem(tr("Inspect"), func() { m.ExploreDockerImage(img, ExploreInspect) }),
					ui.MenuItem(tr("Pull again"), func() { m.OpenDockerPull(ref); m.Pull() }).Disabled(!img.Pullable()),
					ui.MenuItem(tr("Push to cluster registry"), func() { m.PushToRegistry(img) }).Disabled(!img.Pullable()),
					ui.MenuItem(tr("Copy reference"), func() { m.Copy(ref, ref) }),
					ui.MenuItem(tr("Copy ID"), func() { m.Copy(img.ID, img.ShortID) }),
				}
				if !img.ForeignArch() {
					entries = append(entries, ui.MenuItem(tr("Copy docker run command"), func() { m.Copy("docker run --rm -it "+ref, "docker run") }))
				}
				entries = append(entries,
					ui.MenuDivider(),
					ui.MenuItem(tr("Remove"), func() { m.AskRemoveDockerImage(img) }).Destructive(),
				)
				return widget.Row(
					tip(iconButton(lucide.FolderSearch, tr("Browse files of %s", ref), func() { m.ExploreDockerImage(img, ExploreFiles) }), tr("Browse files")),
					tip(iconButton(lucide.Trash, tr("Remove %s", ref), func() { m.AskRemoveDockerImage(img) }), tr("Remove")),
					moreMenu(tr("More actions for %s", ref), entries...),
				).Gap(2).Justify(widget.JustifyEnd)
			})
		}).W(112).Identified("actions"),
	).BindSelected(m.SelectedDockerImage).RowName(DockerImageRow.Ref).RowHeight(tableRow).OnSelect(func(img DockerImageRow) { m.ExploreDockerImage(img, ExploreFiles) })

	state := ggui.Combine(m.ContainersAccess, listing, func(a ContainersAccess, l Listing[dockerapi.Image]) int {
		switch {
		case a != ContainersOpen:
			return 0
		case l.Err != "":
			return 2
		case !l.Loaded:
			return 3
		case len(l.Items) == 0:
			return 1
		}
		return 4
	})
	return ggui.View(state, func(s int) ggui.Widget {
		switch s {
		case 0:
			stage := ggui.Map(m.ContainersAccess, func(a ContainersAccess) int { return int(a) })
			return ggui.View(gateOf(m, recipes.Docker, stage), func(g engineGate) ggui.Widget { return engineNotRunning(m, g, dockerImagesGate()) })
		case 1:
			return ui.Empty(tr("No Docker images"), tr("Pull an image here, or build one with the docker CLI: docker build -t myapp .")).
				Media(glyph(lucide.Layers, 32).Color(colorGray)).
				Action(ui.Button(tr("Pull an image"), func() { m.OpenDockerPull("") }).Icon(glyph(lucide.Download, 16))).Outline()
		case 2:
			return ui.Alert(tr("Could not list Docker images"), ggui.Peek(listing).Err).Destructive().Icon(icons.Alert)
		case 3:
			msg := tr("Asking dockerd…")
			if ggui.Peek(m.Settings).SandboxEngine {
				msg = tr("Loading images…")
			}
			return widget.Row(ui.Spinner(), ui.Caption(msg)).Gap(8)
		}
		count := ggui.Combine(visible, all, func(v, a []DockerImageRow) string { return showing(len(v), len(a)) })
		return tableCard(table.Toolbar(filterBar(m.DockerImageQuery, tr("Filter Docker images"), count)))
	})
}

// ---- Volumes ------------------------------------------------------------

func volumesPage(m *Model) ggui.Widget {
	summary := ui.CaptionOf(ggui.Combine(m.Volumes, m.Disk, func(l Listing[rungv.Volume], d []rungv.DiskUsage) string {
		var s strings.Builder
		s.WriteString(fmt.Sprintf(tr("%d volumes"), len(l.Items)))
		for _, r := range d {
			if r.Type == "Local Volumes" {
				s.WriteString(tr(" · %s on disk", formatSize(r.Size)))
			}
		}
		return s.String() + tr(". Each ext4lite volume is one journaled ext4 image file that grows on demand.")
	}))
	all := ggui.Map(m.Volumes, func(l Listing[rungv.Volume]) []rungv.Volume { return l.Items }).WithEqual(nil)
	visible := ggui.Derived(func() []rungv.Volume {
		items := all.Get()
		q := strings.ToLower(strings.TrimSpace(m.VolumeQuery.Get()))
		if q == "" {
			return items
		}
		rows := m.Rows.Get()
		engUsers := m.EngineVolumeUsers.Get()
		var out []rungv.Volume
		for _, v := range items {
			users := strings.Join(volumeUsersWithEngine(rows, engUsers, v.Name), " ")
			if strings.Contains(strings.ToLower(v.Name+" "+v.Driver+" "+users), q) {
				out = append(out, v)
			}
		}
		return out
	}).WithEqual(nil)
	count := ggui.Combine(visible, all, func(v, a []rungv.Volume) string { return showing(len(v), len(a)) })
	type volStatus struct {
		inUse bool
		users string
	}
	table := ui.Table(visible,
		func(v rungv.Volume) string { return v.Name },
		ui.Col(tr("Name"), func(r ggui.Readable[rungv.Volume]) ggui.Widget {
			return widget.Row(
				glyph(lucide.HardDrive, 16).Color(colorGray),
				widget.TextOf(ggui.Map(r, func(v rungv.Volume) string { return v.Name })).Size(13).Weight(ggui.WeightMedium).NoWrap().Ellipsis(),
			).Gap(10)
		}).Grow(2),
		ui.TextCol(tr("Driver"), func(v rungv.Volume) string { return v.Driver }).W(100),
		ui.Col(tr("Status"), func(r ggui.Readable[rungv.Volume]) ggui.Widget {
			return ggui.View(ggui.Derived(func() volStatus {
				v := r.Get()
				users := volumeUsersWithEngine(m.Rows.Get(), m.EngineVolumeUsers.Get(), v.Name)
				return volStatus{inUse: v.InUse, users: strings.Join(users, ", ")}
			}), func(st volStatus) ggui.Widget {
				switch {
				case st.inUse && st.users != "":
					row := widget.Row(
						dot(colorGreen, 7),
						widget.Text(tr("In use")).Size(12).NoWrap(),
						widget.Flexible(ui.Caption("· "+st.users).Size(12).NoWrap().Ellipsis()),
					).Gap(6).Align(widget.AlignCenter)
					return tip(row, tr("Used by %s", st.users))
				case st.inUse:
					return widget.Row(dot(colorGreen, 7), widget.Text(tr("In use")).Size(12)).Gap(6).Align(widget.AlignCenter)
				case st.users != "":
					return tip(ui.Caption(tr("used by ")+st.users).Size(12).NoWrap().Ellipsis(), tr("Used by %s", st.users))
				}
				return ui.Caption(tr("unused")).Size(12)
			})
		}).Grow(2),
		ui.TextCol(tr("Size"), func(v rungv.Volume) string { return formatSize(v.Size) }).W(96).Right(),
		ui.TextCol(tr("Created"), func(v rungv.Volume) string { return shortDate(v.CreatedAt) }).W(140),
		ui.Col("", func(r ggui.Readable[rungv.Volume]) ggui.Widget {
			return ggui.View(r, func(v rungv.Volume) ggui.Widget {
				menuItems := []ggui.Widget{
					ui.MenuItem(tr("Reveal in file manager"), func() { m.Open(v.Mountpoint) }).Disabled(v.Mountpoint == ""),
					ui.MenuItem(tr("Copy name"), func() { m.Copy(v.Name, v.Name) }),
				}
				if v.Mountpoint != "" {
					menuItems = append(menuItems, ui.MenuItem(tr("Copy path"), func() { m.Copy(v.Mountpoint, v.Mountpoint) }))
				}
				menuItems = append(menuItems,
					ui.MenuDivider(),
					ui.MenuItem(tr("Delete"), func() { m.AskRemoveVolume(v) }).Destructive().Disabled(v.InUse),
				)
				return widget.Row(
					tip(iconButton(lucide.FolderOpen, tr("Reveal %s", v.Name), func() { m.Open(v.Mountpoint) }).Disabled(v.Mountpoint == ""), tr("Reveal in file manager")),
					tip(iconButton(lucide.Trash, tr("Delete %s", v.Name), func() { m.AskRemoveVolume(v) }).Disabled(v.InUse), tr("Delete")),
					moreMenu(tr("More actions for %s", v.Name), menuItems...),
				).Gap(2).Justify(widget.JustifyEnd)
			})
		}).W(112).Identified("actions"),
	).BindSelected(m.SelectedVolume).RowName(func(v rungv.Volume) string { return v.Name }).RowHeight(tableRow).
		Toolbar(filterBar(m.VolumeQuery, tr("Filter by name, driver or sandbox"), count))

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
		return fmt.Sprintf(tr("%d snapshots · %s. Checkpoint a running sandbox and resume it later, in milliseconds."), len(s), formatBytes(total))
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
	visible := ggui.Derived(func() []sandbox.Snapshot {
		all := m.Snapshots.Get()
		q := strings.ToLower(strings.TrimSpace(m.SnapshotQuery.Get()))
		if q == "" {
			return all
		}
		var out []sandbox.Snapshot
		for _, s := range all {
			sbName := ""
			if spec, ok := m.spec(s.SandboxID); ok {
				sbName = spec.Name
			}
			if strings.Contains(strings.ToLower(sbName+" "+s.Name+" "+s.Dir), q) {
				out = append(out, s)
			}
		}
		return out
	}).WithEqual(nil)
	count := ggui.Combine(visible, m.Snapshots, func(v, all []sandbox.Snapshot) string { return showing(len(v), len(all)) })
	hasSnaps := ggui.Map(m.Snapshots, func(s []sandbox.Snapshot) bool { return len(s) > 0 })
	body := ggui.If(hasSnaps, func() ggui.Widget {
		return widget.Column(
			ui.Card(filterBar(m.SnapshotQuery, tr("Filter snapshots"), count)).Pad(6, 10),
			snapshotList(m, visible, true),
		).Gap(12).Stretch()
	}).Else(func() ggui.Widget {
		return ui.Empty(tr("No snapshots yet"), tr("Checkpoint a running sandbox from its Snapshots tab to resume it later in milliseconds.")).
			Media(glyph(lucide.Camera, 32).Color(colorGray)).
			Action(ui.Button(tr("Open Sandboxes"), func() { m.Go(PageSandboxes) }).Outline()).Outline()
	})
	return page(
		pageHeader(tr("Snapshots"), summary,
			tip(iconButton(lucide.RefreshCw, tr("Refresh snapshots"), m.LoadSnapshots), tr("Refresh")),
		),
		hint,
		body,
	)
}

// imageAge says how long ago an image was made, in the app's language;
// rungv's own words for it are English.
func imageAge(i rungv.Image, now time.Time) string {
	t, err := time.Parse("2006-01-02 15:04:05 -0700 MST", i.CreatedAt)
	if err != nil {
		return i.CreatedSince
	}
	return since(t, now)
}
