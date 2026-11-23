package engine

// Owner: workstream C (images, volumes, networks, system).

import (
	"context"
	"encoding/json"
	"io"

	"github.com/ironpark/gvx-desktop/internal/rungv"
)

var (
	_ ImageArchRunner = (*rungvRunner)(nil)
	_ ImagePushRunner = (*rungvRunner)(nil)
)

func (r *rungvRunner) cli() (*rungv.Client, error) {
	if r.client == nil {
		return nil, rungv.ErrNotFound
	}
	c := r.client()
	if c == nil || c.Path == "" {
		return nil, rungv.ErrNotFound
	}
	return c, nil
}

func (r *rungvRunner) Images(ctx context.Context) ([]rungv.Image, error) {
	c, err := r.cli()
	if err != nil {
		return nil, err
	}
	return c.ImagesFull(ctx)
}

func (r *rungvRunner) ImagesArch(ctx context.Context, arch string) ([]rungv.Image, error) {
	if arch == "" || arch == hostImageArch {
		return r.Images(ctx)
	}
	c, err := r.cli()
	if err != nil {
		return nil, err
	}
	return c.ImagesFullArch(ctx, arch)
}

func (r *rungvRunner) InspectImages(ctx context.Context, refs ...string) ([]ImageInspect, error) {
	return r.InspectImagesArch(ctx, "", refs...)
}

func (r *rungvRunner) InspectImagesArch(ctx context.Context, arch string, refs ...string) ([]ImageInspect, error) {
	if len(refs) == 0 {
		return nil, nil
	}
	c, err := r.cli()
	if err != nil {
		return nil, err
	}
	if arch == hostImageArch {
		arch = ""
	}
	raw, err := c.InspectImagesRawArch(ctx, arch, refs...)
	if err != nil {
		return nil, err
	}
	var out []ImageInspect
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, err
	}
	return out, nil
}

func (r *rungvRunner) Pull(ctx context.Context, ref, platform string, line func(string)) error {
	c, err := r.cli()
	if err != nil {
		return err
	}
	return c.PullPlatform(ctx, ref, platform, line)
}

func (r *rungvRunner) PushImage(ctx context.Context, opts rungv.PushOptions, line func(string)) error {
	c, err := r.cli()
	if err != nil {
		return err
	}
	return c.PushImage(ctx, opts, line)
}

func (r *rungvRunner) RemoveImage(ctx context.Context, ref string, force bool) error {
	c, err := r.cli()
	if err != nil {
		return err
	}
	return c.RemoveImageSafe(ctx, ref, force)
}

func (r *rungvRunner) RemoveImageArch(ctx context.Context, arch, ref string, force bool) error {
	if arch == "" || arch == hostImageArch {
		return r.RemoveImage(ctx, ref, force)
	}
	c, err := r.cli()
	if err != nil {
		return err
	}
	return c.RemoveImageSafeArch(ctx, arch, ref, force)
}

func (r *rungvRunner) TagImage(ctx context.Context, source, target string) error {
	c, err := r.cli()
	if err != nil {
		return err
	}
	return c.TagImage(ctx, source, target)
}

func (r *rungvRunner) TagImageArch(ctx context.Context, arch, source, target string) error {
	if arch == "" || arch == hostImageArch {
		return r.TagImage(ctx, source, target)
	}
	c, err := r.cli()
	if err != nil {
		return err
	}
	return c.TagImageArch(ctx, arch, source, target)
}

func (r *rungvRunner) LoadImages(ctx context.Context, archive io.Reader, line func(string)) error {
	c, err := r.cli()
	if err != nil {
		return err
	}
	return c.LoadImages(ctx, archive, line)
}

func (r *rungvRunner) LoadImagesArch(ctx context.Context, arch string, archive io.Reader, line func(string)) error {
	if arch == "" || arch == hostImageArch {
		return r.LoadImages(ctx, archive, line)
	}
	c, err := r.cli()
	if err != nil {
		return err
	}
	return c.LoadImagesArch(ctx, arch, archive, line)
}

func (r *rungvRunner) SaveImages(ctx context.Context, w io.Writer, refs ...string) error {
	c, err := r.cli()
	if err != nil {
		return err
	}
	return c.SaveImages(ctx, w, refs...)
}

func (r *rungvRunner) SaveImagesArch(ctx context.Context, arch string, w io.Writer, refs ...string) error {
	if arch == "" || arch == hostImageArch {
		return r.SaveImages(ctx, w, refs...)
	}
	c, err := r.cli()
	if err != nil {
		return err
	}
	return c.SaveImagesArch(ctx, arch, w, refs...)
}

func (r *rungvRunner) PruneImages(ctx context.Context) (string, error) {
	c, err := r.cli()
	if err != nil {
		return "", err
	}
	// Dangling only: never pass all=true, which would delete user sandbox images.
	return c.PruneImages(ctx, false)
}

func (r *rungvRunner) PruneImagesArch(ctx context.Context, arch string) (string, error) {
	if arch == "" || arch == hostImageArch {
		return r.PruneImages(ctx)
	}
	c, err := r.cli()
	if err != nil {
		return "", err
	}
	// Dangling only: never pass all=true, which would delete user sandbox images.
	return c.PruneImagesArch(ctx, arch, false)
}

func (r *rungvRunner) Volumes(ctx context.Context) ([]rungv.Volume, error) {
	c, err := r.cli()
	if err != nil {
		return nil, err
	}
	return c.Volumes(ctx)
}

func (r *rungvRunner) CreateVolume(ctx context.Context, name, driver string) error {
	c, err := r.cli()
	if err != nil {
		return err
	}
	return c.CreateVolumeSafe(ctx, name, driver)
}

func (r *rungvRunner) RemoveVolume(ctx context.Context, name string) error {
	c, err := r.cli()
	if err != nil {
		return err
	}
	return c.RemoveVolumeSafe(ctx, name)
}

func (r *rungvRunner) DiskUsage(ctx context.Context) ([]rungv.DiskUsage, error) {
	c, err := r.cli()
	if err != nil {
		return nil, err
	}
	return c.DiskUsage(ctx)
}

func (r *rungvRunner) Info(ctx context.Context) (rungv.Info, error) {
	c, err := r.cli()
	if err != nil {
		return rungv.Info{}, err
	}
	return c.Info(ctx)
}

// RungvVersion returns the rungv binary version string for /version.
func (r *rungvRunner) RungvVersion(ctx context.Context) (string, error) {
	c, err := r.cli()
	if err != nil {
		return "", err
	}
	v, err := c.Version(ctx)
	if err != nil {
		return "", err
	}
	return v.Version, nil
}
