package engine

import (
	"archive/tar"
	"context"
	"errors"
	"io"
	"net/http"
	"os"
	"path"
	"strings"

	"github.com/ironpark/gvx-desktop/internal/ext4lite"
	"github.com/ironpark/gvx-desktop/internal/imagefs"
	"github.com/ironpark/gvx-desktop/internal/rungv"
)

type stoppedFS struct {
	img  *imagefs.GVXImage
	vol  *ext4lite.Volume // nil when never started or read-only rootfs
	tree *layerTree
	top  int // layer index of upper (== img.Layers())
}

func (s *stoppedFS) Close() {
	if s.vol != nil {
		_ = s.vol.Close()
		s.vol = nil
	}
	if s.img != nil {
		_ = s.img.Close()
		s.img = nil
	}
}

// openStoppedFS opens a stopped/created container's merged rootfs tree using
// headers-only layer scans (zero regular-file payload I/O).
func (e *Engine) openStoppedFS(ctx context.Context, c Container) (*stoppedFS, error) {
	if c.State.Status == StatusRestarting {
		return nil, errConflict("Container %s is restarting, wait until the container is running", c.ID)
	}
	if c.isMicroVM() {
		return nil, errConflict("Container %s is a stopped micro-VM container; start it to copy files from it", c.Name)
	}
	img, err := e.openImage(ctx, c.ImageID)
	if err != nil {
		return nil, err
	}
	var vol *ext4lite.Volume
	switch {
	case c.HostConfig.ReadonlyRootfs:
		// Read-only rootfs has no upper layer writes.
	case e.ownsRootfs(c.ID, c.Spec.RootfsVolume):
		v, err := ext4lite.Open(c.Spec.RootfsVolume)
		switch {
		case errors.Is(err, os.ErrNotExist):
			// Created and never started: empty upper layer.
		case errors.Is(err, ext4lite.ErrInUse):
			_ = img.Close()
			return nil, errConflict("Container %s is starting or stopping; try again", c.Name)
		case err != nil:
			_ = img.Close()
			return nil, err
		default:
			vol = v
		}
	default:
		_ = img.Close()
		return nil, errNoRootfs(c, "copy files from it")
	}

	s := &stoppedFS{
		img:  img,
		vol:  vol,
		tree: newLayerTree(),
		top:  img.Layers(),
	}
	if err := applyImage(s.tree, img); err != nil {
		s.Close()
		return nil, err
	}
	if vol != nil {
		rc := tarPipe(func(tw *tar.Writer) error {
			err := vol.WriteLayer(tw, ext4lite.UpperDir, ext4lite.LayerOptions{HeadersOnly: true})
			if errors.Is(err, ext4lite.ErrNotExist) {
				return nil
			}
			return err
		})
		err := s.tree.Apply(s.top, tar.NewReader(rc))
		_ = rc.Close()
		if err != nil {
			s.Close()
			return nil, err
		}
	}
	return s, nil
}

func (s *stoppedFS) resolveAndStat(c Container, rawPath, guestPath string) (string, *treeNode, rungv.PathStat, error) {
	followLast := strings.HasSuffix(guestPath, "/") || strings.HasSuffix(guestPath, "/.")
	resolved, node, err := s.tree.resolvePath(guestPath, followLast)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) || errors.Is(err, errNotDirectory) {
			return "", nil, rungv.PathStat{}, errNotFound("Could not find the file %s in container %s", rawPath, c.Name)
		}
		if errors.Is(err, errTooManySymlinks) {
			return "", nil, rungv.PathStat{}, errInvalid("%s: %v", rawPath, err)
		}
		return "", nil, rungv.PathStat{}, err
	}
	st, err := s.statNode(resolved, node)
	if err != nil {
		return "", nil, rungv.PathStat{}, err
	}
	return resolved, node, st, nil
}

func (s *stoppedFS) statNode(resolved string, node *treeNode) (rungv.PathStat, error) {
	if resolved == "" {
		return rungv.PathStat{
			Name: "/",
			Mode: uint32(os.ModeDir | 0o755),
		}, nil
	}
	base := path.Base(resolved)
	if node.owner == implicitOwner {
		return rungv.PathStat{
			Name: base,
			Mode: uint32(os.ModeDir | 0o755),
		}, nil
	}
	tf := node.meta.typeflag
	var size int64
	if tf == tar.TypeReg || tf == tar.TypeLink {
		tf = tar.TypeReg
		sz, err := s.lookupSize(node.owner, resolved)
		if err != nil {
			return rungv.PathStat{}, err
		}
		size = sz
	}
	hdr := &tar.Header{
		Typeflag: tf,
		Mode:     node.meta.mode,
		Size:     size,
		ModTime:  node.meta.modTime,
	}
	st := rungv.PathStat{
		Name:  base,
		Size:  size,
		Mode:  uint32(hdr.FileInfo().Mode()),
		Mtime: node.meta.modTime,
	}
	if tf == tar.TypeSymlink {
		st.LinkTarget = node.meta.linkname
	}
	return st, nil
}

func (s *stoppedFS) lookupSize(owner int, relPath string) (int64, error) {
	if owner < s.top {
		return s.img.StatLayer(owner, relPath)
	}
	if s.vol == nil {
		return 0, os.ErrNotExist
	}
	ino, err := s.vol.Lookup(ext4lite.UpperDir + "/" + relPath)
	if err != nil {
		return 0, err
	}
	if ino.Mode&ext4lite.ModeType == ext4lite.ModeRegular {
		return int64(ino.Size), nil
	}
	return 0, nil
}

func (e *Engine) containerArchiveStoppedHead(w http.ResponseWriter, r *http.Request, c Container, rawPath, guestPath string) {
	fs, err := e.openStoppedFS(r.Context(), c)
	if err != nil {
		writeError(w, r, err)
		return
	}
	defer fs.Close()

	_, _, st, err := fs.resolveAndStat(c, rawPath, guestPath)
	if err != nil {
		writeError(w, r, err)
		return
	}
	w.Header().Set(archivePathStatHeader, encodePathStatHeader(st))
	w.WriteHeader(http.StatusOK)
}

func (e *Engine) containerArchiveStoppedGet(w http.ResponseWriter, r *http.Request, c Container, rawPath, guestPath string) {
	fs, err := e.openStoppedFS(r.Context(), c)
	if err != nil {
		writeError(w, r, err)
		return
	}
	defer fs.Close()

	resolved, node, st, err := fs.resolveAndStat(c, rawPath, guestPath)
	if err != nil {
		writeError(w, r, err)
		return
	}

	w.Header().Set(archivePathStatHeader, encodePathStatHeader(st))
	w.Header().Set("Content-Type", "application/x-tar")
	w.WriteHeader(http.StatusOK)

	tw := tar.NewWriter(w)
	if err := fs.writeArchive(tw, resolved, node); err != nil {
		e.logf("engine: archive stopped %s %s: %v", shortHex(c.ID), rawPath, err)
		panic(http.ErrAbortHandler)
	}
	if err := tw.Close(); err != nil {
		e.logf("engine: archive stopped close %s %s: %v", shortHex(c.ID), rawPath, err)
		panic(http.ErrAbortHandler)
	}
}

func (s *stoppedFS) writeArchive(tw *tar.Writer, resolved string, node *treeNode) error {
	// Single non-directory entry: direct O(depth) read from the winning layer.
	if resolved != "" && !node.isDir() {
		outName := path.Base(resolved)
		if node.owner < s.top {
			return s.img.WriteEntry(node.owner, resolved, outName, tw)
		}
		return writeExt4Entry(s.vol, resolved, outName, tw)
	}

	baseName := ""
	if resolved != "" {
		baseName = path.Base(resolved)
		// Emit the top directory entry first with its winning metadata.
		hdr := &tar.Header{
			Typeflag: tar.TypeDir,
			Name:     baseName + "/",
			Mode:     0o755,
		}
		if node.owner != implicitOwner {
			hdr.Mode = node.meta.mode
			hdr.Uid = node.meta.uid
			hdr.Gid = node.meta.gid
			hdr.ModTime = node.meta.modTime
		}
		if err := tw.WriteHeader(hdr); err != nil {
			return err
		}
	}

	for i := range s.top {
		rc := tarPipe(func(lw *tar.Writer) error { return s.img.WriteLayer(i, lw, false) })
		err := s.tree.EmitSubtree(i, tar.NewReader(rc), tw, resolved, baseName)
		_ = rc.Close()
		if err != nil {
			return err
		}
	}
	if s.vol != nil {
		rc := tarPipe(func(lw *tar.Writer) error {
			err := s.vol.WriteLayer(lw, ext4lite.UpperDir, ext4lite.LayerOptions{})
			if errors.Is(err, ext4lite.ErrNotExist) {
				return nil
			}
			return err
		})
		err := s.tree.EmitSubtree(s.top, tar.NewReader(rc), tw, resolved, baseName)
		_ = rc.Close()
		if err != nil {
			return err
		}
	}
	return nil
}

func writeExt4Entry(v *ext4lite.Volume, relPath, outName string, tw *tar.Writer) error {
	if v == nil {
		return os.ErrNotExist
	}
	n, err := v.Lookup(ext4lite.UpperDir + "/" + relPath)
	if err != nil {
		return err
	}
	if n.IsWhiteout() {
		return os.ErrNotExist
	}
	hdr := &tar.Header{
		Name:       outName,
		Mode:       int64(n.Mode & 0o7777),
		Uid:        int(n.UID),
		Gid:        int(n.GID),
		ModTime:    n.Mtime,
		AccessTime: n.Atime,
		ChangeTime: n.Ctime,
		Format:     tar.FormatPAX,
	}
	if xattrs, err := v.Xattrs(n); err == nil {
		for _, x := range xattrs {
			if strings.HasPrefix(x.Name, "trusted.") || strings.HasPrefix(x.Name, "system.") {
				continue
			}
			if hdr.PAXRecords == nil {
				hdr.PAXRecords = map[string]string{}
			}
			hdr.PAXRecords["SCHILY.xattr."+x.Name] = string(x.Value)
		}
	}
	var body io.Reader
	switch n.Mode & ext4lite.ModeType {
	case ext4lite.ModeDir:
		hdr.Typeflag = tar.TypeDir
		if !strings.HasSuffix(hdr.Name, "/") {
			hdr.Name += "/"
		}
	case ext4lite.ModeRegular:
		hdr.Typeflag = tar.TypeReg
		hdr.Size = int64(n.Size)
		body, err = v.Open(n)
		if err != nil {
			return err
		}
	case ext4lite.ModeSymlink:
		hdr.Typeflag = tar.TypeSymlink
		hdr.Linkname, err = v.Readlink(n)
		if err != nil {
			return err
		}
	case ext4lite.ModeChar:
		hdr.Typeflag, hdr.Devmajor, hdr.Devminor = tar.TypeChar, int64(n.Major), int64(n.Minor)
	case ext4lite.ModeBlock:
		hdr.Typeflag, hdr.Devmajor, hdr.Devminor = tar.TypeBlock, int64(n.Major), int64(n.Minor)
	case ext4lite.ModeFIFO:
		hdr.Typeflag = tar.TypeFifo
	default:
		return nil
	}
	if err := tw.WriteHeader(hdr); err != nil {
		return err
	}
	if body != nil {
		_, err = io.CopyN(tw, body, hdr.Size)
		return err
	}
	return nil
}
