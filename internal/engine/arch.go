package engine

// Owner: frozen (architect). Images of another architecture than the
// sandboxes run: the amd64 builder loads its images into rungv's cache,
// which keeps each architecture apart (rungv's -arch flag), and the engine
// lists them, inspects, tags, saves and removes them, but does not run
// them. The bodies are in foreign_images.go (workstream C). See
// docs/design/amd64-builder.md, "Images of another architecture".

import (
	"context"
	"io"
	"runtime"

	"github.com/ironpark/gvx-desktop/internal/rungv"
)

// hostImageArch is the architecture the sandboxes run, and the one of
// rungv's default image namespace.
const hostImageArch = runtime.GOARCH

// foreignImageArch is the other architecture rungv keeps images of.
func foreignImageArch() string {
	if hostImageArch == "amd64" {
		return "arm64"
	}
	return "amd64"
}

// foreignRunMessage is what docker run (container create) answers for an
// image of another architecture, with the image's reference and its
// architecture.
const foreignRunMessage = "%s images can be built and pushed, not run, yet: %s is linux/%s and GVX Desktop runs linux/%s containers"

// ImageArchRunner is a Runner whose image cache keeps each architecture's
// images apart, as rungv's does. arch is "amd64" or "arm64"; the host's is
// what the Runner methods without arch do. The engine uses it when the
// Runner has it, and sees only the host's images otherwise.
type ImageArchRunner interface {
	ImagesArch(ctx context.Context, arch string) ([]rungv.Image, error)
	InspectImagesArch(ctx context.Context, arch string, refs ...string) ([]ImageInspect, error)
	RemoveImageArch(ctx context.Context, arch, ref string, force bool) error
	TagImageArch(ctx context.Context, arch, source, target string) error
	LoadImagesArch(ctx context.Context, arch string, archive io.Reader, line func(string)) error
	SaveImagesArch(ctx context.Context, arch string, w io.Writer, refs ...string) error
	PruneImagesArch(ctx context.Context, arch string) (string, error)
}
