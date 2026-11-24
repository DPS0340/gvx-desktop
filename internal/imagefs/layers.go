package imagefs

import (
	"archive/tar"
	"encoding/json"
	"errors"
	"fmt"
	"path"
	"slices"
	"strings"
)

// GVXImage is a cached gvx image read whole: its config and its compiled
// layers, bottom first, each as an OCI layer tar. The sandbox engine's
// diff and export read a container's image this way, without rungv save,
// which fetches the layers from the registry again and has none for an
// image that was loaded or built.
type GVXImage struct {
	// Config is the image config as the cache holds it: its SHA-256 is the
	// image ID.
	Config []byte
	// LayerDescriptors are the OCI descriptors of the image's layers, as
	// its manifest in the cache lists them: their digests are the keys the
	// cache holds the layers under.
	LayerDescriptors []json.RawMessage
	layers           []*erofsImage
}

// OpenGVXImage opens the cached image whose config digest is imageID.
func OpenGVXImage(cacheDir, imageID string) (*GVXImage, error) {
	if cacheDir == "" {
		return nil, errors.New("layer cache directory is unknown")
	}
	rec, err := findGVXImageRecord(cacheDir, imageID, "")
	if err != nil {
		return nil, err
	}
	if len(rec.Manifests) == 0 {
		return nil, errors.New("cached image has no manifest")
	}
	var man struct {
		Layers []json.RawMessage `json:"layers"`
	}
	if err := json.Unmarshal(rec.Manifests[len(rec.Manifests)-1], &man); err != nil {
		return nil, fmt.Errorf("decode image manifest: %w", err)
	}
	g := &GVXImage{Config: rec.Config, LayerDescriptors: man.Layers}
	for _, raw := range man.Layers {
		var l struct {
			Digest string `json:"digest"`
		}
		if err := json.Unmarshal(raw, &l); err != nil {
			g.Close()
			return nil, fmt.Errorf("decode image manifest layer: %w", err)
		}
		img, _, err := openGVXLayerFile(cacheDir, rec.Format, l.Digest)
		if err != nil {
			g.Close()
			return nil, err
		}
		g.layers = append(g.layers, img)
	}
	return g, nil
}

// Layers is the number of layers.
func (g *GVXImage) Layers() int { return len(g.layers) }

// Close releases the layer files.
func (g *GVXImage) Close() error {
	var first error
	for _, img := range g.layers {
		if err := img.Close(); err != nil && first == nil {
			first = err
		}
	}
	g.layers = nil
	return first
}

// Exists reports whether the merged file system of the layers has name
// ("etc/hosts"): the topmost layer that holds it, or a whiteout of it, or
// that hides what the layers below hold there, decides.
func (g *GVXImage) Exists(name string) (bool, error) {
	name = CleanPath(name)
	for _, img := range slices.Backward(g.layers) {
		ino, ok, err := img.lookup(name)
		if err != nil {
			return false, err
		}
		if ok {
			return !ino.isWhiteout(), nil
		}
		if blocked, err := ancestorBlocksLower(img, name); err != nil || blocked {
			return false, err
		}
	}
	return false, nil
}

// WriteLayer writes layer i (0 is the bottom) to tw as an OCI layer tar: a
// whiteout becomes ".wh.<name>", an opaque directory gets ".wh..wh..opq",
// names are relative, directories end in "/" and come before what they
// hold, and a file with more than one name is written once and linked
// after. With headersOnly, regular files are written with no content and a
// size of 0, for a reader that wants only the tree and symlink targets.
func (g *GVXImage) WriteLayer(i int, tw *tar.Writer, headersOnly bool) error {
	if i < 0 || i >= len(g.layers) {
		return fmt.Errorf("layer %d of %d", i, len(g.layers))
	}
	img := g.layers[i]
	root, err := img.inode(img.rootNid)
	if err != nil {
		return err
	}
	w := erofsTarWriter{img: img, tw: tw, headersOnly: headersOnly, links: map[uint64]string{}}
	return w.dir(root, "", 0)
}

// StatLayer returns the byte size of name in layer i (0 for non-regular files).
func (g *GVXImage) StatLayer(i int, name string) (int64, error) {
	if i < 0 || i >= len(g.layers) {
		return 0, fmt.Errorf("layer %d of %d", i, len(g.layers))
	}
	ino, ok, err := g.layers[i].lookup(CleanPath(name))
	if err != nil {
		return 0, err
	}
	if !ok || ino.isWhiteout() {
		return 0, fmt.Errorf("%s: not found in layer %d", name, i)
	}
	if ino.mode&posixIFMT == posixIFREG {
		return int64(ino.size), nil
	}
	return 0, nil
}

// WriteEntry writes the single entry at name from layer i to tw under outName.
// Unlike WriteLayer, hard-linked regular files are always written as regular
// files with their full content, making single-file archive extraction O(depth).
func (g *GVXImage) WriteEntry(i int, name, outName string, tw *tar.Writer) error {
	if i < 0 || i >= len(g.layers) {
		return fmt.Errorf("layer %d of %d", i, len(g.layers))
	}
	img := g.layers[i]
	ino, ok, err := img.lookup(CleanPath(name))
	if err != nil {
		return err
	}
	if !ok || ino.isWhiteout() {
		return fmt.Errorf("%s: not found in layer %d", name, i)
	}
	hdr := &tar.Header{
		Name:    outName,
		Mode:    int64(ino.mode & 0o7777),
		Uid:     int(ino.uid),
		Gid:     int(ino.gid),
		ModTime: ino.modTime(),
	}
	switch ino.mode & posixIFMT {
	case posixIFDIR:
		hdr.Typeflag = tar.TypeDir
		if !strings.HasSuffix(hdr.Name, "/") {
			hdr.Name += "/"
		}
		return tw.WriteHeader(hdr)
	case posixIFREG:
		hdr.Typeflag = tar.TypeReg
		hdr.Size = int64(ino.size)
		if err := tw.WriteHeader(hdr); err != nil {
			return err
		}
		for off := uint64(0); off < ino.size; {
			n := min(ino.size-off, copyChunk)
			b, err := img.readRange(ino, off, n)
			if err != nil {
				return err
			}
			if _, err := tw.Write(b); err != nil {
				return err
			}
			off += n
		}
		return nil
	case posixIFLNK:
		hdr.Typeflag = tar.TypeSymlink
		target, err := img.readLink(ino)
		if err != nil {
			return err
		}
		hdr.Linkname = target
		return tw.WriteHeader(hdr)
	case posixIFCHR, 0o060000:
		hdr.Typeflag = tar.TypeChar
		if ino.mode&posixIFMT == 0o060000 {
			hdr.Typeflag = tar.TypeBlock
		}
		hdr.Devmajor = int64((ino.rdev & 0xfff00) >> 8)
		hdr.Devminor = int64((ino.rdev & 0xff) | ((ino.rdev >> 12) & 0xfff00))
		return tw.WriteHeader(hdr)
	case 0o010000:
		hdr.Typeflag = tar.TypeFifo
		return tw.WriteHeader(hdr)
	default:
		return nil
	}
}

type erofsTarWriter struct {
	img         *erofsImage
	tw          *tar.Writer
	headersOnly bool
	links       map[uint64]string
}

const (
	maxLayerDepth = 512
	copyChunk     = 1 << 20
)

func (w *erofsTarWriter) dir(ino erofsInode, prefix string, depth int) error {
	if depth > maxLayerDepth {
		return fmt.Errorf("erofs: directory tree deeper than %d", maxLayerDepth)
	}
	ents, err := w.img.readDir(ino)
	if err != nil {
		return err
	}
	for _, d := range ents {
		if strings.ContainsRune(d.name, '/') {
			return fmt.Errorf("erofs: bad name %q", d.name)
		}
		if d.nid == ino.nid {
			continue
		}
		child, err := w.img.inode(d.nid)
		if err != nil {
			return err
		}
		if err := w.entry(child, prefix+d.name, depth); err != nil {
			return err
		}
	}
	return nil
}

func (w *erofsTarWriter) entry(ino erofsInode, name string, depth int) error {
	if ino.isWhiteout() {
		dir, base := path.Split(name)
		return w.tw.WriteHeader(&tar.Header{Typeflag: tar.TypeReg, Name: dir + ".wh." + base, Mode: 0o644, ModTime: ino.modTime()})
	}
	hdr := &tar.Header{
		Name:    name,
		Mode:    int64(ino.mode & 0o7777),
		Uid:     int(ino.uid),
		Gid:     int(ino.gid),
		ModTime: ino.modTime(),
	}
	if ino.kind() != KindDir {
		if first, ok := w.links[ino.nid]; ok {
			hdr.Typeflag, hdr.Linkname = tar.TypeLink, first
			return w.tw.WriteHeader(hdr)
		}
		w.links[ino.nid] = name
	}
	switch ino.mode & posixIFMT {
	case posixIFDIR:
		hdr.Typeflag, hdr.Name = tar.TypeDir, name+"/"
		if err := w.tw.WriteHeader(hdr); err != nil {
			return err
		}
		if w.img.isOpaque(ino) {
			if err := w.tw.WriteHeader(&tar.Header{Typeflag: tar.TypeReg, Name: name + "/.wh..wh..opq", Mode: 0o644, ModTime: ino.modTime()}); err != nil {
				return err
			}
		}
		return w.dir(ino, name+"/", depth+1)
	case posixIFREG:
		hdr.Typeflag = tar.TypeReg
		if !w.headersOnly {
			hdr.Size = int64(ino.size)
		}
		if err := w.tw.WriteHeader(hdr); err != nil {
			return err
		}
		if w.headersOnly {
			return nil
		}
		for off := uint64(0); off < ino.size; {
			n := min(ino.size-off, copyChunk)
			b, err := w.img.readRange(ino, off, n)
			if err != nil {
				return err
			}
			if _, err := w.tw.Write(b); err != nil {
				return err
			}
			off += n
		}
		return nil
	case posixIFLNK:
		hdr.Typeflag = tar.TypeSymlink
		target, err := w.img.readLink(ino)
		if err != nil {
			return err
		}
		hdr.Linkname = target
	case posixIFCHR, 0o060000:
		hdr.Typeflag = tar.TypeChar
		if ino.mode&posixIFMT == 0o060000 {
			hdr.Typeflag = tar.TypeBlock
		}
		// Linux's new_encode_dev.
		hdr.Devmajor = int64((ino.rdev & 0xfff00) >> 8)
		hdr.Devminor = int64((ino.rdev & 0xff) | ((ino.rdev >> 12) & 0xfff00))
	case 0o010000:
		hdr.Typeflag = tar.TypeFifo
	default:
		return nil // a socket: tar has none
	}
	return w.tw.WriteHeader(hdr)
}
