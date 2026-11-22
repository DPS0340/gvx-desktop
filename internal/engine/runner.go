package engine

// Owner: frozen (architect). Process methods of rungvRunner are in
// runner_proc.go (workstream A), rungv image and volume methods in
// runner_store.go (workstream C).

import (
	"context"
	"io"
	"syscall"
	"time"

	"github.com/ironpark/gvx-desktop/internal/rungv"
	"github.com/ironpark/gvx-desktop/internal/sandbox"
)

// Runner is everything the engine asks of the world: it runs sandboxes, as
// the supervisor does, and drives rungv's image cache and volumes. The
// sandbox of a container has the container's ID as its sandbox.Spec.ID.
type Runner interface {
	// SetHandlers is called once, by New, before any other method. event
	// gets the supervisor's events; output gets what the guests write.
	// Neither may block.
	SetHandlers(event func(sandbox.Event), output func(id string, o sandbox.Output))

	// Start runs spec; it returns once the process is started.
	Start(spec sandbox.Spec) error
	// Stop interrupts the run and kills it after timeout; the run ends as
	// sandbox.Stopped. It returns when the process has ended.
	Stop(id string, timeout time.Duration) error
	// Signal sends sig to the run.
	Signal(id string, sig syscall.Signal) error
	// State is the run's state; a zero State when there is none.
	State(id string) sandbox.State
	// Stdin is the run's standard input, nil without one.
	Stdin(id string) io.Writer
	// CloseStdin ends the run's standard input.
	CloseStdin(id string) error
	// Resize sets the size of the run's terminal.
	Resize(id string, rows, cols uint16) error
	// Forget drops what the runner keeps of an ended run.
	Forget(id string)

	// Images lists the cached images, with full IDs.
	Images(ctx context.Context) ([]rungv.Image, error)
	// InspectImages returns rungv inspect of each ref, in order; an error
	// when one is not cached.
	InspectImages(ctx context.Context, refs ...string) ([]ImageInspect, error)
	// Pull downloads ref for platform ("" for the host's), handing each
	// progress line to line.
	Pull(ctx context.Context, ref, platform string, line func(string)) error
	RemoveImage(ctx context.Context, ref string, force bool) error
	TagImage(ctx context.Context, source, target string) error
	// LoadImages reads a docker save or OCI archive.
	LoadImages(ctx context.Context, archive io.Reader, line func(string)) error
	// SaveImages writes refs as one archive.
	SaveImages(ctx context.Context, w io.Writer, refs ...string) error
	// PruneImages removes the dangling images and says what it freed.
	PruneImages(ctx context.Context) (string, error)

	// Volumes lists rungv's volumes, every one in the cache directory.
	Volumes(ctx context.Context) ([]rungv.Volume, error)
	CreateVolume(ctx context.Context, name, driver string) error
	RemoveVolume(ctx context.Context, name string) error

	DiskUsage(ctx context.Context) ([]rungv.DiskUsage, error)
	Info(ctx context.Context) (rungv.Info, error)
}

// rungvRunner is the real Runner: a supervisor of its own over rungv.
type rungvRunner struct {
	sup    *sandbox.Supervisor
	client func() *rungv.Client
}

// NewRunner returns the real Runner. client gives the app's rungv client,
// whose CacheDir is the image cache and volume store the engine uses; dir
// holds the supervisor's files.
func NewRunner(client func() *rungv.Client, dir string) Runner {
	return &rungvRunner{
		sup:    &sandbox.Supervisor{Client: client, DataDir: dir},
		client: client,
	}
}

// HasRunFlag reports whether the rungv the runner drives takes the run
// flag name.
func (r *rungvRunner) HasRunFlag(name string) bool {
	return r.client().HasRunFlag(context.Background(), name)
}

func (r *rungvRunner) SetHandlers(event func(sandbox.Event), output func(id string, o sandbox.Output)) {
	r.sup.Notify = event
	r.sup.Output = output
}
