package ext4lite

import (
	"archive/tar"
	"fmt"
	"io"
	"path"
	"slices"
	"strings"
)

// UpperDir is where rungv keeps a root file system volume's overlay upper
// layer (-rootfs-volume); its work directory is /work.
const UpperDir = "/upper"

// LayerOptions shape WriteLayer's archive.
type LayerOptions struct {
	// HeadersOnly writes regular files with no content and a size of 0, for
	// a reader that wants only the tree.
	HeadersOnly bool
}

// WriteLayer writes the tree under dir (an overlay upper directory) to tw as
// an OCI image layer: a whiteout (character device 0:0) becomes an empty
// ".wh.<name>", and an opaque directory (trusted.overlay.opaque=y) gets a
// ".wh..wh..opq" entry. Names are relative to dir, directories end in "/",
// and a file with more than one link is written once and linked after.
// Entries are in name order within each directory.
func (v *Volume) WriteLayer(tw *tar.Writer, dir string, o LayerOptions) error {
	root, err := v.Lookup(dir)
	if err != nil {
		return err
	}
	if !root.IsDir() {
		return fmt.Errorf("ext4lite: %s is not a directory", dir)
	}
	w := layerWriter{v: v, tw: tw, o: o, links: make(map[uint32]string)}
	return w.dir(root, "", 0)
}

type layerWriter struct {
	v     *Volume
	tw    *tar.Writer
	o     LayerOptions
	links map[uint32]string
}

const maxTreeDepth = 512

func (w *layerWriter) dir(n *Inode, prefix string, depth int) error {
	if depth > maxTreeDepth {
		return corrupt("directory tree deeper than %d", maxTreeDepth)
	}
	ents, err := w.v.ReadDir(n)
	if err != nil {
		return err
	}
	slices.SortFunc(ents, func(a, b DirEntry) int { return strings.Compare(a.Name, b.Name) })
	for _, e := range ents {
		if strings.ContainsRune(e.Name, '/') || e.Name == "" {
			return corrupt("bad name %q in inode %d", e.Name, n.Ino)
		}
		c, err := w.v.Stat(e.Ino)
		if err != nil {
			return err
		}
		if err := w.entry(c, prefix+e.Name, depth); err != nil {
			return err
		}
	}
	return nil
}

func (w *layerWriter) entry(n *Inode, name string, depth int) error {
	if n.IsWhiteout() {
		dir, base := path.Split(name)
		return w.tw.WriteHeader(&tar.Header{
			Typeflag: tar.TypeReg, Name: dir + ".wh." + base, Mode: 0o644,
			ModTime: n.Mtime, Format: tar.FormatPAX,
		})
	}
	xattrs, err := w.v.Xattrs(n)
	if err != nil {
		return err
	}
	hdr := &tar.Header{
		Name:       name,
		Mode:       int64(n.Mode & 0o7777),
		Uid:        int(n.UID),
		Gid:        int(n.GID),
		ModTime:    n.Mtime,
		AccessTime: n.Atime,
		ChangeTime: n.Ctime,
		Format:     tar.FormatPAX,
	}
	opaque := false
	for _, x := range xattrs {
		switch {
		case x.Name == "trusted.overlay.opaque":
			opaque = string(x.Value) == "y"
		case strings.HasPrefix(x.Name, "trusted.") || strings.HasPrefix(x.Name, "system."):
			// The overlay's own attributes and ACLs stay out, as in docker's layers.
		default:
			if hdr.PAXRecords == nil {
				hdr.PAXRecords = map[string]string{}
			}
			hdr.PAXRecords["SCHILY.xattr."+x.Name] = string(x.Value)
		}
	}

	if !n.IsDir() && n.Links > 1 {
		if first, ok := w.links[n.Ino]; ok {
			hdr.Typeflag, hdr.Linkname = tar.TypeLink, first
			return w.tw.WriteHeader(hdr)
		}
		w.links[n.Ino] = name
	}

	var body io.Reader
	switch n.Mode & ModeType {
	case ModeDir:
		hdr.Typeflag, hdr.Name = tar.TypeDir, name+"/"
	case ModeRegular:
		hdr.Typeflag = tar.TypeReg
		if !w.o.HeadersOnly {
			hdr.Size = int64(n.Size)
			if body, err = w.v.Open(n); err != nil {
				return err
			}
		}
	case ModeSymlink:
		hdr.Typeflag = tar.TypeSymlink
		if hdr.Linkname, err = w.v.Readlink(n); err != nil {
			return err
		}
	case ModeChar:
		hdr.Typeflag, hdr.Devmajor, hdr.Devminor = tar.TypeChar, int64(n.Major), int64(n.Minor)
	case ModeBlock:
		hdr.Typeflag, hdr.Devmajor, hdr.Devminor = tar.TypeBlock, int64(n.Major), int64(n.Minor)
	case ModeFIFO:
		hdr.Typeflag = tar.TypeFifo
	case ModeSocket:
		return nil // tar has no sockets; docker leaves them out too
	default:
		return corrupt("inode %d has the mode %#o", n.Ino, n.Mode)
	}
	if err := w.tw.WriteHeader(hdr); err != nil {
		return err
	}
	if body != nil {
		if _, err := io.CopyN(w.tw, body, hdr.Size); err != nil {
			return err
		}
	}
	if n.IsDir() {
		if opaque {
			if err := w.tw.WriteHeader(&tar.Header{
				Typeflag: tar.TypeReg, Name: name + "/.wh..wh..opq", Mode: 0o644,
				ModTime: n.Mtime, Format: tar.FormatPAX,
			}); err != nil {
				return err
			}
		}
		return w.dir(n, name+"/", depth+1)
	}
	return nil
}
