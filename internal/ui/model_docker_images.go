package ui

import (
	"context"
	"runtime"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/ironpark/ggui"
	"github.com/ironpark/ggui/ui"

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
	Arch       string
	Created    int64
	Size       int64
	Dangling   bool
}

// Key identifies a row uniquely even when one image carries several tags or
// when two architectures share a tag.
func (r DockerImageRow) Key() string {
	if r.Arch != "" {
		return r.ID + "|" + r.Repository + "|" + r.Tag + "|" + r.Arch
	}
	return r.ID + "|" + r.Repository + "|" + r.Tag
}

// ForeignArch reports whether the image's architecture differs from this host's
// native sandbox architecture.
func (r DockerImageRow) ForeignArch() bool {
	return r.Arch != "" && r.Arch != runtime.GOARCH
}

// Platform returns "linux/<arch>" for foreign-arch rows and "" otherwise.
// Passing it to platform-scoped engine endpoints ensures name-based actions
// (delete, tag, inspect) target the foreign namespace even when the same
// repo:tag also exists for the host architecture.
func (r DockerImageRow) Platform() string {
	if r.ForeignArch() {
		return "linux/" + r.Arch
	}
	return ""
}

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
		arch := img.Architecture()
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
					Arch:       arch,
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
			Arch:       arch,
			Created:    img.Created,
			Size:       img.Size,
			Dangling:   true,
		})
	}
	return out
}

// filterDockerImages keeps rows whose repository, tag, architecture or ID matches q.
func filterDockerImages(rows []DockerImageRow, q string) []DockerImageRow {
	q = strings.ToLower(strings.TrimSpace(q))
	if q == "" {
		return rows
	}
	var out []DockerImageRow
	for _, r := range rows {
		if strings.Contains(strings.ToLower(r.Repository+":"+r.Tag+" "+r.Arch+" "+r.ShortID+" "+r.ID), q) {
			out = append(out, r)
		}
	}
	return out
}

// dockerImageUsers returns the names of containers referencing row. A
// foreign-arch row never matches containers by repo:tag because the same tag
// may also exist for the host architecture that containers actually run.
func dockerImageUsers(containers []dockerapi.Container, row DockerImageRow) []string {
	var names []string
	ref := row.Ref()
	for _, c := range containers {
		if c.ImageID != "" && row.ID != "" {
			if c.ImageID == row.ID {
				names = append(names, c.Name())
			}
			continue
		}
		if c.Image == row.ID || c.Image == row.ShortID {
			names = append(names, c.Name())
			continue
		}
		if row.ForeignArch() {
			continue
		}
		if c.Image == ref || (row.Tag == "latest" && row.Repository != "<none>" && c.Image == row.Repository) {
			names = append(names, c.Name())
		}
	}
	return names
}

// sandboxImageUsers returns the names of user sandboxes referencing row.
// Sandboxes run only native host-architecture images, so foreign-arch rows
// never match.
func sandboxImageUsers(rows []Row, row DockerImageRow) []string {
	if row.ForeignArch() {
		return nil
	}
	var names []string
	ref := row.Ref()
	for _, r := range rows {
		if sandboxUsesImage(r.Spec.Image, row, ref) {
			names = append(names, r.Spec.Name)
		}
	}
	return names
}

func sandboxUsesImage(img string, row DockerImageRow, ref string) bool {
	if img == "" {
		return false
	}
	if img == ref || img == row.ID || img == row.ShortID {
		return true
	}
	if row.Tag == "latest" && row.Repository != "<none>" && img == row.Repository {
		return true
	}
	norm := func(s string) string {
		s = strings.TrimPrefix(s, "docker.io/")
		s = strings.TrimPrefix(s, "library/")
		return s
	}
	nImg := norm(img)
	if nImg == norm(ref) {
		return true
	}
	return row.Tag == "latest" && row.Repository != "<none>" && nImg == norm(row.Repository)
}

// dockerImageAllUsers returns container users of row, plus user sandbox users
// when the shared rungv image cache is active in sandbox-engine mode.
func dockerImageAllUsers(containers []dockerapi.Container, rows []Row, sandboxEngine bool, row DockerImageRow) []string {
	users := dockerImageUsers(containers, row)
	if !sandboxEngine {
		return users
	}
	for _, sb := range sandboxImageUsers(rows, row) {
		if !slices.Contains(users, sb) {
			users = append(users, sb)
		}
	}
	return users
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

// LoadDockerImages reads the images stored inside the Docker Engine sandbox
// or served by the experimental sandbox engine. Asked for while a listing
// runs, it lists them again after it.
func (m *Model) LoadDockerImages() {
	if m.Offline {
		return
	}
	if ggui.Peek(m.ContainersAccess) != ContainersOpen {
		m.DockerImages.Set(Listing[dockerapi.Image]{})
		return
	}
	if ggui.Peek(m.Settings).SandboxEngine {
		if pe := ggui.Peek(m.DockerProxyErr); pe != "" {
			m.DockerImages.Set(Listing[dockerapi.Image]{Loaded: true, Err: pe})
			return
		}
	}
	if m.dockerImagesLoading {
		m.dockerImagesAgain = true
		return
	}
	m.dockerImagesLoading = true
	host := m.dockerBackendHost()
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
		if ggui.Peek(m.ContainersAccess) != ContainersOpen {
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
// In sandbox-engine mode (shared rungv image cache), images referenced by user
// sandboxes are refused rather than force-deleted.
func (m *Model) AskRemoveDockerImage(img DockerImageRow) {
	ref := img.Ref()
	if ggui.Peek(m.Settings).SandboxEngine {
		if sbs := sandboxImageUsers(ggui.Peek(m.Rows), img); len(sbs) > 0 {
			m.Toasts.Push(ui.Toast(
				tr("Cannot remove %s", ref),
				tr("Used by sandbox %s. Remove it from the GVX Sandboxes tab instead if you want to re-pull it later.", strings.Join(sbs, ", ")),
			).Warning().Duration(8 * time.Second))
			return
		}
	}
	users := dockerImageUsers(ggui.Peek(m.DockerContainers).Items, img)
	text := tr("The image is removed from the Docker Engine (%s).", formatBytes(img.Size))
	if len(users) > 0 {
		text = tr("Used by %s. Force removing untags or deletes the image from the Docker Engine (%s).", strings.Join(users, ", "), formatBytes(img.Size))
	}
	m.Confirm(tr("Remove %s?", ref), text, tr("Remove image"), func() {
		host := m.dockerBackendHost()
		m.async(func(ctx context.Context) error {
			c, err := dockerapi.New(host)
			if err != nil {
				return err
			}
			defer c.Close()
			ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
			defer cancel()
			return c.RemoveImagePlatform(ctx, img.Target(), true, img.Platform())
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
			host := m.dockerBackendHost()
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

// registryPath is an image's repository without the registry it names, if
// it names one: the first part of a path names a registry when it has a
// dot or a port in it, or is localhost, as docker reads references.
func registryPath(repo string) string {
	first, rest, ok := strings.Cut(repo, "/")
	if ok && (strings.ContainsAny(first, ".:") || first == "localhost") {
		return rest
	}
	return repo
}

// RegistryRef is the reference an image is pushed to the image registry
// recipe as, which the cluster's pods pull it by: its repository and tag
// under the registry as sandboxes name it.
func RegistryRef(img DockerImageRow) string {
	return recipes.RegistryHost() + "/" + registryPath(img.Repository) + ":" + img.Tag
}

// PushToRegistry tags an image of the Docker Engine for the image registry
// recipe and pushes it there, for the cluster to run. While the registry is
// not running it offers to start it instead.
func (m *Model) PushToRegistry(img DockerImageRow) {
	if !img.Pullable() {
		return
	}
	row, ok := m.RecipeSandbox(ggui.Peek(m.Rows), recipes.Registry)
	switch {
	case !ok:
		m.Offer(tr("Set up the image registry?"),
			tr("Images reach the cluster through a registry on this computer, which runs in a sandbox of its own. Set it up and start it, then push again once it is ready."),
			tr("Set up and start"), func() { m.CreateFromRecipe(recipes.Registry) })
		return
	case row.State.Status == sandbox.Starting:
		m.Toasts.Push(ui.Toast(tr("The image registry is starting"), tr("Push again once it is ready.")).Info())
		return
	case row.State.Status == sandbox.Running && row.State.Paused:
		m.Offer(tr("Resume the image registry?"),
			tr("Images reach the cluster through the registry, which is paused. Resume it, then push again."),
			tr("Resume"), func() { m.ResumeSandbox(row.Spec.ID) })
		return
	case row.State.Status != sandbox.Running:
		m.Offer(tr("Start the image registry?"),
			tr("Images reach the cluster through the registry, which is not running. Start it, then push again once it is ready."),
			tr("Start"), func() { m.StartSandbox(row.Spec.ID) })
		return
	}
	ref := RegistryRef(img)
	repo, tag := splitRepoTag(ref)
	title := tr("Pushing %s", img.Ref())
	toast := m.Toasts.Push(ui.Toast(title, tr("To %s, through the Docker Engine…", recipes.RegistryHost())).Loading())
	// The last line of the push's progress shows in its toast.
	var mu sync.Mutex
	var last string
	posted := false
	show := func() {
		mu.Lock()
		line := last
		posted = false
		mu.Unlock()
		m.Toasts.Update(toast, ui.Toast(title, line).Loading())
	}
	host := m.dockerBackendHost()
	m.async(func(ctx context.Context) error {
		c, err := dockerapi.New(host)
		if err != nil {
			return err
		}
		defer c.Close()
		if err := c.TagPlatform(ctx, img.Target(), repo, tag, img.Platform()); err != nil {
			return err
		}
		return c.PushImage(ctx, ref, func(line string) {
			mu.Lock()
			last = line
			again := !posted
			posted = true
			mu.Unlock()
			if again {
				m.post(show)
			}
		})
	}, func(err error) {
		if err != nil {
			m.Toasts.Update(toast, ui.Toast(tr("Could not push %s", img.Ref()), err.Error()).Error().Duration(10*time.Second))
		} else {
			m.Toasts.Update(toast, ui.Toast(tr("Pushed %s", img.Ref()), tr("Pods run it as %s.", ref)).Success().Duration(12*time.Second).
				Action(tr("Copy reference"), func() { m.Copy(ref, ref) }))
		}
		m.LoadDockerImages()
	})
}
