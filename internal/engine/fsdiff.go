package engine

// diff (/containers/{id}/changes) and export (docs/design/sandbox-engine.md
// §9.4). A container's changes are its root file system's upper layer
// (rootfs.go); its image is read from rungv's cache, layer by layer
// (imagefs.GVXImage), as rungv save cannot give an image that was loaded
// or built.

import (
	"archive/tar"
	"context"
	"errors"
	"io"
	"net/http"
	"os"
	"path"
	"slices"
	"strings"

	"github.com/ironpark/gvx-desktop/internal/imagefs"
)

// openImage opens the image with ID id in rungv's cache.
func (e *Engine) openImage(ctx context.Context, id string) (*imagefs.GVXImage, error) {
	info, err := e.runner.Info(ctx)
	if err != nil {
		return nil, err
	}
	img, err := imagefs.OpenGVXImage(info.LayerCacheDir, id)
	if err != nil {
		return nil, errConflict("The image of the container, %s, is not in the image cache: %v", shortHex(id), err)
	}
	return img, nil
}

// tarPipe runs write in a goroutine and returns what it writes, as a tar
// stream. Closing the reader stops write.
func tarPipe(write func(tw *tar.Writer) error) io.ReadCloser {
	pr, pw := io.Pipe()
	go func() {
		tw := tar.NewWriter(pw)
		err := write(tw)
		if err == nil {
			err = tw.Close()
		}
		pw.CloseWithError(err)
	}()
	return pr
}

// applyImage merges every layer of img into t, bottom first.
func applyImage(t *layerTree, img *imagefs.GVXImage) error {
	for i := range img.Layers() {
		rc := tarPipe(func(tw *tar.Writer) error { return img.WriteLayer(i, tw, true) })
		err := t.Apply(i, tar.NewReader(rc))
		rc.Close()
		if err != nil {
			return err
		}
	}
	return nil
}

// change is one entry of docker diff.
type change struct {
	Path string `json:"Path"`
	Kind int    `json:"Kind"` // 0 changed, 1 added, 2 deleted
}

const (
	changeModified = 0
	changeAdded    = 1
	changeDeleted  = 2
)

// changesOf lists the changes an upper layer makes to a file system:
// a whiteout deletes, an entry that exists below changes, any other adds.
func changesOf(upper io.Reader, exists func(name string) (bool, error)) ([]change, error) {
	out := []change{}
	tr := tar.NewReader(upper)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, err
		}
		name := layerName(hdr.Name)
		if name == "" {
			continue
		}
		dir, base := path.Split(name)
		switch {
		case base == opaqueMarker:
		case strings.HasPrefix(base, whiteoutPrefix):
			out = append(out, change{"/" + dir + strings.TrimPrefix(base, whiteoutPrefix), changeDeleted})
		default:
			below, err := exists(name)
			if err != nil {
				return nil, err
			}
			kind := changeAdded
			if below {
				kind = changeModified
			}
			out = append(out, change{"/" + name, kind})
		}
	}
	slices.SortFunc(out, func(a, b change) int { return strings.Compare(a.Path, b.Path) })
	return out, nil
}

func (e *Engine) containerChanges(w http.ResponseWriter, r *http.Request) {
	if !e.runner.Features().Control {
		writeError(w, r, errNotSupported("Listing the changes of a container (it needs rungv 0.20)"))
		return
	}
	c, err := e.lookup(r.PathValue("id"))
	if err != nil {
		writeError(w, r, err)
		return
	}
	img, err := e.openImage(r.Context(), c.ImageID)
	if err != nil {
		writeError(w, r, err)
		return
	}
	defer img.Close()
	var out []change
	err = e.withUpper(r.Context(), c, false, true, "list its changes", func(upper io.Reader) error {
		var err error
		out, err = changesOf(upper, img.Exists)
		return err
	})
	if err != nil {
		writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

// mountSkip leaves out what c's mounts hold, as docker export does: their
// mount points stay, empty.
func mountSkip(c Container) func(name string) bool {
	var dsts []string // each with a trailing slash
	for _, m := range c.Mounts {
		if d := layerName(m.Destination); d != "" {
			dsts = append(dsts, d+"/")
		}
	}
	return func(name string) bool {
		for _, d := range dsts {
			if strings.HasPrefix(name, d) {
				return true
			}
		}
		return false
	}
}

func (e *Engine) containerExport(w http.ResponseWriter, r *http.Request) {
	if !e.runner.Features().Control {
		writeError(w, r, errNotSupported("Exporting a container (it needs rungv 0.20)"))
		return
	}
	c, err := e.lookup(r.PathValue("id"))
	if err != nil {
		writeError(w, r, err)
		return
	}
	out := &tarStreamWriter{w: w}
	if c.State.Running() || c.State.Status == StatusPaused {
		err = e.exportRunning(r.Context(), c, out)
	} else {
		err = e.exportStopped(r.Context(), c, out)
	}
	if err != nil {
		if !out.started {
			writeError(w, r, err)
			return
		}
		e.logf("engine: export %s: %v", c.ID[:12], err)
		panic(http.ErrAbortHandler) // the client must not take a cut archive for a whole one
	}
	if !out.started { // an empty file system still is a tar
		w.Header().Set("Content-Type", "application/x-tar")
		w.WriteHeader(http.StatusOK)
	}
}

// exportRunning streams a running container's file system, as rungv's
// export reads it in the guest, without its mounts' contents.
func (e *Engine) exportRunning(ctx context.Context, c Container, w io.Writer) error {
	ctl := e.runner.Control(c.ID)
	if ctl == nil {
		return errConflict("Container %s was started without a control socket; restart it to export it", c.Name)
	}
	rc, err := ctl.Export(ctx, "/", false)
	if err != nil {
		return err
	}
	defer rc.Close()
	tw := tar.NewWriter(w)
	if err := copyLayer(tar.NewReader(rc), tw, mountSkip(c)); err != nil {
		return err
	}
	return tw.Close()
}

// exportStopped streams a stopped container's file system: its image's
// layers merged with its own changes.
func (e *Engine) exportStopped(ctx context.Context, c Container, w io.Writer) error {
	img, err := e.openImage(ctx, c.ImageID)
	if err != nil {
		return err
	}
	defer img.Close()

	// The changes are read twice, to merge and to write; keep them in a
	// file so the container cannot start in between and change them.
	upper, err := e.tempFile("export-")
	if err != nil {
		return err
	}
	defer func() { upper.Close(); os.Remove(upper.Name()) }()
	if err := e.withUpper(ctx, c, false, false, "export it", func(r io.Reader) error {
		_, err := io.Copy(upper, r)
		return err
	}); err != nil {
		return err
	}

	tree := newLayerTree()
	if err := applyImage(tree, img); err != nil {
		return err
	}
	top := img.Layers()
	readUpper := func() (*tar.Reader, error) {
		if _, err := upper.Seek(0, io.SeekStart); err != nil {
			return nil, err
		}
		return tar.NewReader(upper), nil
	}
	tr, err := readUpper()
	if err != nil {
		return err
	}
	if err := tree.Apply(top, tr); err != nil {
		return err
	}

	tw := tar.NewWriter(w)
	for i := range top {
		rc := tarPipe(func(tw *tar.Writer) error { return img.WriteLayer(i, tw, false) })
		err := tree.Emit(i, tar.NewReader(rc), tw)
		rc.Close()
		if err != nil {
			return err
		}
	}
	if tr, err = readUpper(); err != nil {
		return err
	}
	if err := tree.Emit(top, tr, tw); err != nil {
		return err
	}
	return tw.Close()
}

// tempFile makes a file in the engine's tmp directory, which Open empties.
func (e *Engine) tempFile(pattern string) (*os.File, error) {
	dir := e.tmpDir()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	return os.CreateTemp(dir, pattern)
}

func (e *Engine) tmpDir() string {
	if e.cfg.Dir == "" {
		return os.TempDir()
	}
	return e.path("tmp")
}

// sweepTmp removes what an earlier run left in the tmp directory.
func (e *Engine) sweepTmp() {
	if e.cfg.Dir == "" {
		return
	}
	if err := os.RemoveAll(e.tmpDir()); err != nil && !errors.Is(err, os.ErrNotExist) {
		e.logf("engine: empty %s: %v", e.tmpDir(), err)
	}
}
