package engine

// Containers' root file systems (docs/design/sandbox-engine.md §9.4). With
// rungv v0.20, a new container's writes to its root file system go to an
// ext4lite volume of its own, <Dir>/rootfs/<id>.ext4 (-rootfs-volume), not
// to memory: they outlive a stop, as docker's do, and diff, export and
// commit can read them while the container is stopped, with
// internal/ext4lite. While it runs, rungv's export reads them instead.

import (
	"archive/tar"
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/ironpark/gvx-desktop/internal/ext4lite"
	"github.com/ironpark/gvx-desktop/internal/overlay/guestcfg"
	"github.com/ironpark/gvx-desktop/internal/rungv"
)

// rootfsDir holds the containers' root file system volumes.
func (e *Engine) rootfsDir() string { return e.path("rootfs") }

func (e *Engine) rootfsPath(id string) string {
	return filepath.Join(e.rootfsDir(), id+".ext4")
}

// keepsRootfs reports whether a new container gets a root file system
// volume: with rungv's control (v0.20), which reads it while the container
// runs, and when its root file system is writable.
func (e *Engine) keepsRootfs(hc HostConfig) bool {
	return e.runner.Features().Control && !hc.ReadonlyRootfs && e.cfg.Dir != ""
}

// ownsRootfs reports whether p is a volume of the engine's own, the one
// the container with id has.
func (e *Engine) ownsRootfs(id, p string) bool {
	return p != "" && e.cfg.Dir != "" && filepath.Clean(p) == e.rootfsPath(id)
}

// removeRootfs deletes c's root file system volume, once its run ended.
func (e *Engine) removeRootfs(c Container) {
	if !e.ownsRootfs(c.ID, c.Spec.RootfsVolume) {
		return
	}
	if err := os.Remove(c.Spec.RootfsVolume); err != nil && !errors.Is(err, os.ErrNotExist) {
		e.logf("engine: remove the root file system of %s: %v", c.ID[:12], err)
	}
}

// sweepRootfs removes the volumes no container has: those a crash left
// between a container's removal and its volume's.
func (e *Engine) sweepRootfs() {
	ents, err := os.ReadDir(e.rootfsDir())
	if err != nil {
		return
	}
	e.ctr.mu.Lock()
	keep := map[string]bool{}
	for id, c := range e.ctr.byID {
		if e.ownsRootfs(id, c.Spec.RootfsVolume) {
			keep[filepath.Base(c.Spec.RootfsVolume)] = true
		}
	}
	e.ctr.mu.Unlock()
	for _, ent := range ents {
		if ent.Type().IsRegular() && strings.HasSuffix(ent.Name(), ".ext4") && !keep[ent.Name()] {
			_ = os.Remove(filepath.Join(e.rootfsDir(), ent.Name()))
		}
	}
}

// runtimeFiles are what rungv (or older netinit single-file mounts) writes
// into a guest at its start: they stay out of diff and commit, as docker's
// init layer keeps its own out.
var runtimeFiles = map[string]bool{
	"etc/hosts":       true,
	"etc/hostname":    true,
	"etc/resolv.conf": true,
	".gvx-netinit":    true,
}

// runtimeDirs are the directories rungv makes at a guest's start when the
// image has none (including the netinit mountpoint). One the layer holds
// nothing under but runtime files is left out.
var runtimeDirs = map[string]bool{
	"proc": true, "sys": true, "dev": true, "etc": true, "root": true,
	"workspace": true, "tmp": true, "bin": true, "usr": true, "usr/bin": true,
	strings.TrimPrefix(guestcfg.MountDir, "/"): true,
}

// filterUpper copies an upper layer from r to tw without what rungv put
// there itself: runtimeFiles, and the runtimeDirs left with nothing else.
// The layer lists each directory before what it holds, as rungv's export
// and ext4lite.WriteLayer do.
func filterUpper(r io.Reader, tw *tar.Writer) error {
	type held struct {
		name string
		hdrs []*tar.Header // the directory, then its opaque marker
	}
	var pending []held
	flush := func() error {
		for _, p := range pending {
			for _, h := range p.hdrs {
				if err := tw.WriteHeader(h); err != nil {
					return err
				}
			}
		}
		pending = pending[:0]
		return nil
	}
	tr := tar.NewReader(r)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			return nil // a runtime directory still pending held nothing
		}
		if err != nil {
			return err
		}
		name := layerName(hdr.Name)
		if name == "" || runtimeFiles[name] {
			continue
		}
		for len(pending) > 0 && !under(name, pending[len(pending)-1].name) {
			pending = pending[:len(pending)-1] // it held nothing
		}
		if len(pending) > 0 && path.Base(name) == opaqueMarker && path.Dir(name) == pending[len(pending)-1].name {
			top := &pending[len(pending)-1]
			top.hdrs = append(top.hdrs, hdr)
			continue
		}
		if hdr.Typeflag == tar.TypeDir && runtimeDirs[name] {
			pending = append(pending, held{name: name, hdrs: []*tar.Header{hdr}})
			continue
		}
		if err := flush(); err != nil {
			return err
		}
		if err := tw.WriteHeader(hdr); err != nil {
			return err
		}
		if _, err := io.Copy(tw, tr); err != nil {
			return err
		}
	}
}

// errNoRootfs is a stopped container whose root file system changes were
// not kept: one created before the engine kept them, or without rungv's
// control.
func errNoRootfs(c Container, what string) error {
	return errConflict("Container %s is not running, and its file system changes were not kept: it was created before GVX Desktop kept them, or without rungv 0.20. Start it to %s", c.Name, what)
}

// withUpper hands fn the changes c's root file system holds, as a layer
// tar without rungv's runtime files. A running container's come from
// rungv's export, paused for the duration when pause is set (and it is not
// paused already); a stopped one's from its volume, with regular files
// empty when headersOnly is set. A container whose root file system is
// read-only has no changes: fn gets an empty layer.
func (e *Engine) withUpper(ctx context.Context, c Container, pause, headersOnly bool, what string, fn func(r io.Reader) error) error {
	var raw io.ReadCloser
	switch {
	case c.HostConfig.ReadonlyRootfs:
		return fn(emptyLayer())
	case c.State.Running() || c.State.Status == StatusPaused:
		ctl := e.runner.Control(c.ID)
		if ctl == nil {
			return errConflict("Container %s was started without a control socket; restart it to %s", c.Name, what)
		}
		if pause && !e.runner.State(c.ID).Paused {
			if err := e.runner.Pause(ctx, c.ID); err == nil {
				defer func() { _ = e.runner.Unpause(context.WithoutCancel(ctx), c.ID) }()
			} else if controlCode(err) != rungv.CodeAlreadyPaused {
				return err
			}
		}
		rc, err := ctl.Export(ctx, "/", true)
		if err != nil {
			return err
		}
		raw = rc
	case e.ownsRootfs(c.ID, c.Spec.RootfsVolume):
		v, err := ext4lite.Open(c.Spec.RootfsVolume)
		switch {
		case errors.Is(err, os.ErrNotExist):
			return fn(emptyLayer()) // created and never started: nothing changed yet
		case errors.Is(err, ext4lite.ErrInUse):
			return errConflict("Container %s is starting or stopping; try again", c.Name)
		case err != nil:
			return err
		}
		defer v.Close()
		raw = tarPipe(func(tw *tar.Writer) error {
			err := v.WriteLayer(tw, ext4lite.UpperDir, ext4lite.LayerOptions{HeadersOnly: headersOnly})
			if errors.Is(err, ext4lite.ErrNotExist) {
				return nil // the volume of a run that ended before its overlay was set up
			}
			return err
		})
	default:
		return errNoRootfs(c, what)
	}
	defer raw.Close()
	filtered := tarPipe(func(tw *tar.Writer) error { return filterUpper(raw, tw) })
	defer filtered.Close()
	if err := fn(filtered); err != nil {
		return err
	}
	// fn may stop before the end; drain so the producers finish.
	_, err := io.Copy(io.Discard, filtered)
	return err
}

// emptyLayer is a layer tar with nothing in it.
func emptyLayer() io.Reader { return bytes.NewReader(make([]byte, 1024)) }
