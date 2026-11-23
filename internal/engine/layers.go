package engine

// Image layers as tar streams: what diff, export and commit read
// (docs/design/sandbox-engine.md §9.4). A layer is an OCI layer tar: a
// ".wh.<name>" entry deletes name from the layers below, a ".wh..wh..opq"
// entry hides everything the layers below hold in its directory.

import (
	"archive/tar"
	"bytes"
	"fmt"
	"io"
	"path"
	"strings"
)

const (
	whiteoutPrefix = ".wh."
	opaqueMarker   = ".wh..wh..opq"
)

// layerName cleans a layer entry's name to a relative path without a
// trailing slash ("etc/hosts"); "" is the root.
func layerName(name string) string {
	name = path.Clean("/" + name)
	return strings.TrimPrefix(name, "/")
}

// implicitOwner marks a tree node no layer has an entry for: a directory
// that only exists as the parent of entries.
const implicitOwner = -1

type treeNode struct {
	children map[string]*treeNode
	owner    int // index of the layer whose entry wins, or implicitOwner
	ord      int // position of that entry in its layer
}

func (n *treeNode) child(name string, create bool) *treeNode {
	c := n.children[name]
	if c == nil && create {
		if n.children == nil {
			n.children = map[string]*treeNode{}
		}
		c = &treeNode{owner: implicitOwner}
		n.children[name] = c
	}
	return c
}

// hideBelow removes what layers below i hold under n. It reports whether
// n still holds something of layer i; n then stays, as a bare parent when
// its own entry was below.
func (n *treeNode) hideBelow(i int) bool {
	n.hideChildrenBelow(i)
	if n.owner >= i {
		return true
	}
	if len(n.children) > 0 {
		n.owner = implicitOwner
		return true
	}
	return false
}

func (n *treeNode) hideChildrenBelow(i int) {
	for name, c := range n.children {
		if !c.hideBelow(i) {
			delete(n.children, name)
		}
	}
}

// layerTree is the merged file system of a stack of layers: which layer's
// entry wins for each path. Apply each layer in order, bottom first; then
// Emit replays them, writing only the winners.
type layerTree struct {
	root treeNode
	// linked lists, by layer, the names a hard link of that layer points to.
	linked map[int]map[string]bool
}

func newLayerTree() *layerTree {
	return &layerTree{root: treeNode{owner: implicitOwner}, linked: map[int]map[string]bool{}}
}

func (t *layerTree) lookup(name string, create bool) *treeNode {
	n := &t.root
	if name == "" {
		return n
	}
	for seg := range strings.SplitSeq(name, "/") {
		if n = n.child(seg, create); n == nil {
			return nil
		}
	}
	return n
}

// Apply merges layer i, read from tr, onto the layers below it.
func (t *layerTree) Apply(i int, tr *tar.Reader) error {
	for ord := 0; ; ord++ {
		hdr, err := tr.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		name := layerName(hdr.Name)
		if name == "" {
			continue
		}
		dir, base := path.Split(name)
		dir = strings.TrimSuffix(dir, "/")
		switch {
		case base == opaqueMarker:
			t.lookup(dir, true).hideChildrenBelow(i)
		case strings.HasPrefix(base, whiteoutPrefix):
			parent := t.lookup(dir, false)
			if parent == nil {
				continue
			}
			target := strings.TrimPrefix(base, whiteoutPrefix)
			if c := parent.children[target]; c != nil && !c.hideBelow(i) {
				delete(parent.children, target)
			}
		default:
			n := t.lookup(name, true)
			n.owner, n.ord = i, ord
			if hdr.Typeflag != tar.TypeDir {
				// A file in place of a directory hides what it held.
				n.hideChildrenBelow(i)
			}
			if hdr.Typeflag == tar.TypeLink {
				if t.linked[i] == nil {
					t.linked[i] = map[string]bool{}
				}
				t.linked[i][layerName(hdr.Linkname)] = true
			}
		}
	}
}

// maxLostLink bounds the content Emit keeps of a hard link's target that a
// later layer replaced or deleted.
const maxLostLink = 256 << 20

// Emit replays layer i from tr, writing to tw the entries that won in the
// merge: what Apply saw of layer i must be what tr reads now.
//
// A hard link whose target lost to a later layer still names the file, as
// in an overlay: the first such link is written as a file with the
// target's content, and the others link to it.
func (t *layerTree) Emit(i int, tr *tar.Reader, tw *tar.Writer) error {
	lost := map[string][]byte{}  // a linked target that did not win → its content
	moved := map[string]string{} // a lost target → the link written in its place
	for ord := 0; ; ord++ {
		hdr, err := tr.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		name := layerName(hdr.Name)
		if name == "" || strings.HasPrefix(path.Base(name), whiteoutPrefix) {
			continue
		}
		if n := t.lookup(name, false); n == nil || n.owner != i || n.ord != ord {
			if t.linked[i][name] && hdr.Typeflag == tar.TypeReg {
				if hdr.Size > maxLostLink {
					return fmt.Errorf("%s: a hard link's target of %d bytes was replaced by a later layer", name, hdr.Size)
				}
				if lost[name], err = io.ReadAll(tr); err != nil {
					return err
				}
			}
			continue
		}
		out := *hdr
		out.Name = name
		var body io.Reader
		switch hdr.Typeflag {
		case tar.TypeDir:
			out.Name += "/"
		case tar.TypeReg:
			body = tr
		case tar.TypeLink:
			target := layerName(hdr.Linkname)
			out.Linkname = target
			if to, ok := moved[target]; ok {
				out.Linkname = to
			} else if b, ok := lost[target]; ok {
				out.Typeflag, out.Linkname, out.Size = tar.TypeReg, "", int64(len(b))
				body = bytes.NewReader(b)
				moved[target] = name
				delete(lost, target)
			}
		}
		if err := tw.WriteHeader(&out); err != nil {
			return err
		}
		if body != nil {
			if _, err := io.Copy(tw, body); err != nil {
				return err
			}
		}
	}
}

// copyLayer copies a layer from tr to tw entry by entry, but what skip
// leaves out.
func copyLayer(tr *tar.Reader, tw *tar.Writer, skip func(name string) bool) error {
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		if skip != nil && skip(layerName(hdr.Name)) {
			continue
		}
		if err := tw.WriteHeader(hdr); err != nil {
			return err
		}
		if _, err := io.Copy(tw, tr); err != nil {
			return err
		}
	}
}

// under reports whether name is dir or below it ("" holds everything).
func under(name, dir string) bool {
	return dir == "" || name == dir || strings.HasPrefix(name, dir+"/")
}
