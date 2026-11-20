package ui

import (
	"context"
	"strings"
	"time"

	"github.com/ironpark/ggui"

	"github.com/ironpark/gvx-desktop/internal/dockerapi"
	"github.com/ironpark/gvx-desktop/internal/recipes"
	"github.com/ironpark/gvx-desktop/internal/sandbox"
)

// The views of the Images page: gvx's own EROFS layer cache for sandboxes,
// and the images stored inside the Docker Engine sandbox.
const (
	ImagesGVX = iota
	ImagesDocker
)

// The targets of the pull dialog: gvx's image cache, or the Docker Engine.
const (
	PullTargetGVX = iota
	PullTargetDocker
)

// DockerImageRow is one row in the Docker images table: a single tag of an
// image, or one dangling/untagged image.
type DockerImageRow struct {
	ID         string
	ShortID    string
	Repository string
	Tag        string
	Digest     string
	Created    int64
	Size       int64
	Dangling   bool
}

// Key identifies a row uniquely even when one image carries several tags.
func (r DockerImageRow) Key() string { return r.ID + "|" + r.Repository + "|" + r.Tag }

// Ref is how the image is named for display and copying: repo:tag when tagged,
// repo@digest when untagged with a digest, or the short ID for a dangling one.
func (r DockerImageRow) Ref() string {
	if r.Pullable() {
		return r.Repository + ":" + r.Tag
	}
	if r.Repository != "" && r.Repository != "<none>" && r.Digest != "" {
		return r.Repository + "@" + r.Digest
	}
	return r.ShortID
}

// Target is what Docker Engine API image endpoints expect: repo:tag so
// removing untags only that tag when an image has several, or the full ID
// for an untagged or dangling image.
func (r DockerImageRow) Target() string {
	if r.Pullable() {
		return r.Repository + ":" + r.Tag
	}
	return r.ID
}

// Pullable reports whether the row has a repository and tag that can be pulled.
func (r DockerImageRow) Pullable() bool {
	return !r.Dangling && r.Repository != "" && r.Repository != "<none>" && r.Tag != "" && r.Tag != "<none>"
}

func splitRepoTag(s string) (repo, tag string) {
	if i := strings.LastIndex(s, ":"); i > strings.LastIndex(s, "/") {
		return s[:i], s[i+1:]
	}
	return s, "<none>"
}

// dockerImageRows expands the engine's images into one row per tag, as
// docker images prints them, keeping dangling and untagged images as one row.
func dockerImageRows(images []dockerapi.Image) []DockerImageRow {
	var out []DockerImageRow
	for _, img := range images {
		short := img.ShortID()
		var tags []string
		for _, rt := range img.RepoTags {
			if rt != "" && rt != "<none>:<none>" {
				tags = append(tags, rt)
			}
		}
		if len(tags) > 0 {
			for _, rt := range tags {
				repo, tag := splitRepoTag(rt)
				out = append(out, DockerImageRow{
					ID:         img.ID,
					ShortID:    short,
					Repository: repo,
					Tag:        tag,
					Created:    img.Created,
					Size:       img.Size,
				})
			}
			continue
		}
		repo, digest := "<none>", ""
		for _, rd := range img.RepoDigests {
			if r, d, ok := strings.Cut(rd, "@"); ok && r != "" && r != "<none>" {
				repo, digest = r, d
				break
			}
		}
		out = append(out, DockerImageRow{
			ID:         img.ID,
			ShortID:    short,
			Repository: repo,
			Tag:        "<none>",
			Digest:     digest,
			Created:    img.Created,
			Size:       img.Size,
			Dangling:   true,
		})
	}
	return out
}

// filterDockerImages keeps rows whose repository, tag or ID matches q.
func filterDockerImages(rows []DockerImageRow, q string) []DockerImageRow {
	q = strings.ToLower(strings.TrimSpace(q))
	if q == "" {
		return rows
	}
	var out []DockerImageRow
	for _, r := range rows {
		if strings.Contains(strings.ToLower(r.Repository+":"+r.Tag+" "+r.ShortID+" "+r.ID), q) {
			out = append(out, r)
		}
	}
	return out
}

// dockerImageUsers returns the names of containers referencing row.
func dockerImageUsers(containers []dockerapi.Container, row DockerImageRow) []string {
	var names []string
	ref := row.Ref()
	for _, c := range containers {
		if c.ImageID != "" && c.ImageID == row.ID ||
			c.Image == ref ||
			c.Image == row.ID ||
			c.Image == row.ShortID ||
			(row.Tag == "latest" && row.Repository != "<none>" && c.Image == row.Repository) {
			names = append(names, c.Name())
		}
	}
	return names
}

// dockerImagesTotalSize sums the sizes of unique Docker images.
func dockerImagesTotalSize(images []dockerapi.Image) int64 {
	var total int64
	for _, img := range images {
		total += img.Size
	}
	return total
}

// GoImages opens the Images page on a view (ImagesGVX or ImagesDocker).
func (m *Model) GoImages(view int) {
	m.ImageView.Set(view)
	m.Go(PageImages)
}

// LoadDockerImages reads the images stored inside the Docker Engine sandbox.
// Asked for while a listing runs, it lists them again after it.
func (m *Model) LoadDockerImages() {
	if m.Offline {
		return
	}
	row, ok := m.RecipeSandbox(ggui.Peek(m.Rows), recipes.Docker)
	if !ok || row.State.Status != sandbox.Running {
		m.DockerImages.Set(Listing[dockerapi.Image]{})
		return
	}
	if m.dockerImagesLoading {
		m.dockerImagesAgain = true
		return
	}
	m.dockerImagesLoading = true
	host := m.dockerHost()
	var items []dockerapi.Image
	m.DockerImages.Update(func(l Listing[dockerapi.Image]) Listing[dockerapi.Image] { l.Loading = true; return l })
	m.async(func(ctx context.Context) error {
		c, err := dockerapi.New(host)
		if err != nil {
			return err
		}
		defer c.Close()
		ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		defer cancel()
		items, err = c.Images(ctx)
		return err
	}, func(err error) {
		m.dockerImagesLoading = false
		if m.dockerImagesAgain {
			m.dockerImagesAgain = false
			m.LoadDockerImages()
			return
		}
		if row, ok := m.RecipeSandbox(ggui.Peek(m.Rows), recipes.Docker); !ok || row.State.Status != sandbox.Running {
			return
		}
		l := Listing[dockerapi.Image]{Items: items, Loaded: true}
		if err != nil {
			l.Err = err.Error()
		}
		m.DockerImages.Set(l)
	})
}

// OpenDockerPull shows the pull dialog targeting the Docker Engine.
func (m *Model) OpenDockerPull(ref string) {
	if ggui.Peek(m.Pulling) {
		m.PullOpen.Set(true)
		return
	}
	m.PullTarget.Set(PullTargetDocker)
	m.PullRef.Set(ref)
	m.PullLog.Set("")
	m.PullOpen.Set(true)
}

// InspectDockerImage opens the image explorer on the Inspect tab for a Docker image.
func (m *Model) InspectDockerImage(img DockerImageRow) {
	m.ExploreDockerImage(img, ExploreInspect)
}

// AskRemoveDockerImage confirms and removes a Docker image (or untags one tag).
func (m *Model) AskRemoveDockerImage(img DockerImageRow) {
	ref := img.Ref()
	users := dockerImageUsers(ggui.Peek(m.DockerContainers).Items, img)
	text := tr("The image is removed from the Docker Engine (%s).", formatBytes(img.Size))
	if len(users) > 0 {
		text = tr("Used by %s. Force removing untags or deletes the image from the Docker Engine (%s).", strings.Join(users, ", "), formatBytes(img.Size))
	}
	m.Confirm(tr("Remove %s?", ref), text, tr("Remove image"), func() {
		host := m.dockerHost()
		m.async(func(ctx context.Context) error {
			c, err := dockerapi.New(host)
			if err != nil {
				return err
			}
			defer c.Close()
			ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
			defer cancel()
			return c.RemoveImage(ctx, img.Target(), true)
		}, func(err error) {
			if err != nil {
				m.toastError(tr("Could not remove %s", ref), err)
			} else {
				m.toastOK(tr("Removed %s", ref), "")
			}
			m.LoadDockerImages()
			m.LoadDocker()
		})
	})
}

// AskPruneDockerImages removes dangling images from the Docker Engine.
func (m *Model) AskPruneDockerImages() {
	m.Confirm(tr("Remove unused Docker images?"),
		tr("Dangling images in the Docker Engine are deleted."),
		tr("Prune images"), func() {
			host := m.dockerHost()
			var res dockerapi.PruneResult
			m.async(func(ctx context.Context) error {
				c, err := dockerapi.New(host)
				if err != nil {
					return err
				}
				defer c.Close()
				ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
				defer cancel()
				res, err = c.PruneImages(ctx, false)
				return err
			}, func(err error) {
				if err != nil {
					m.toastError(tr("Prune failed"), err)
				} else {
					m.toastOK(tr("Docker images pruned"), tr("Reclaimed %s.", formatBytes(res.SpaceReclaimed)))
				}
				m.LoadDockerImages()
			})
		})
}
