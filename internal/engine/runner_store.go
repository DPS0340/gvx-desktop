package engine

// Owner: workstream C (images, volumes, networks, system).

import (
	"context"
	"encoding/json"
	"io"

	"github.com/ironpark/gvx-desktop/internal/rungv"
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

func (r *rungvRunner) InspectImages(ctx context.Context, refs ...string) ([]ImageInspect, error) {
	if len(refs) == 0 {
		return nil, nil
	}
	c, err := r.cli()
	if err != nil {
		return nil, err
	}
	raw, err := c.InspectImagesRaw(ctx, refs...)
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

func (r *rungvRunner) RemoveImage(ctx context.Context, ref string, force bool) error {
	c, err := r.cli()
	if err != nil {
		return err
	}
	return c.RemoveImageSafe(ctx, ref, force)
}

func (r *rungvRunner) TagImage(ctx context.Context, source, target string) error {
	c, err := r.cli()
	if err != nil {
		return err
	}
	return c.TagImage(ctx, source, target)
}

func (r *rungvRunner) LoadImages(ctx context.Context, archive io.Reader, line func(string)) error {
	c, err := r.cli()
	if err != nil {
		return err
	}
	return c.LoadImages(ctx, archive, line)
}

func (r *rungvRunner) SaveImages(ctx context.Context, w io.Writer, refs ...string) error {
	c, err := r.cli()
	if err != nil {
		return err
	}
	return c.SaveImages(ctx, w, refs...)
}

func (r *rungvRunner) PruneImages(ctx context.Context) (string, error) {
	c, err := r.cli()
	if err != nil {
		return "", err
	}
	// Dangling only: never pass all=true, which would delete user sandbox images.
	return c.PruneImages(ctx, false)
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
