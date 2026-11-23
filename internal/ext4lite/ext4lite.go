// Package ext4lite reads the .ext4 volumes that rungv writes (its ext4lite
// driver), without a sandbox. It is read-only: a volume whose journal holds
// committed transactions, as a crash leaves it, is read as the replay would
// leave it, with the log applied in memory, and the file is never written.
//
// It reads the profile ext4lite formats (GVXIP-002: 4 KiB blocks, 4096 blocks
// and inodes per group, 256-byte inodes, meta_bg, extents, 64-bit, inline
// data, linear directories, no checksums), and refuses a file system with any
// other incompatible feature.
package ext4lite

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"strings"
	"time"
)

var (
	le = binary.LittleEndian
	be = binary.BigEndian

	// ErrNotExist reports a path the volume does not have.
	ErrNotExist = errors.New("ext4lite: no such file or directory")
	// ErrInUse reports a volume a running sandbox holds.
	ErrInUse = errors.New("ext4lite: volume is in use")
)

// corrupt is damage the reader found.
func corrupt(format string, args ...any) error {
	return fmt.Errorf("ext4lite: damaged volume: "+format, args...)
}

const (
	blockSize      = 4096
	blocksPerGroup = 4096
	inodesPerGroup = 4096
	inodeSize      = 256
	descSize       = 64
	descsPerBlock  = blockSize / descSize
	superOffset    = 1024
	superMagic     = 0xEF53
	rootIno        = 2
	journalIno     = 8

	compatHasJournal = 0x0004

	incompatFiletype   = 0x0002
	incompatRecover    = 0x0004
	incompatMetaBG     = 0x0010
	incompatExtents    = 0x0040
	incompat64bit      = 0x0080
	incompatInlineData = 0x8000
	knownIncompat      = incompatFiletype | incompatRecover | incompatMetaBG | incompatExtents | incompat64bit | incompatInlineData

	flagExtents    = 0x00080000
	flagInlineData = 0x10000000

	iBlock     = 0x28
	iBlockSize = 60
	extMagic   = 0xF30A
	xattrMagic = 0xEA020000
	maxDepth   = 5
	// maxDirSize bounds what ReadDir reads of a directory: ext4lite's
	// directories are linear, and 64 MiB holds about a million entries.
	maxDirSize = 64 << 20
)

// Mode bits of an inode's file type.
const (
	ModeType    = 0xF000
	ModeSocket  = 0xC000
	ModeSymlink = 0xA000
	ModeRegular = 0x8000
	ModeBlock   = 0x6000
	ModeDir     = 0x4000
	ModeChar    = 0x2000
	ModeFIFO    = 0x1000
)

// Volume is an open ext4lite volume.
type Volume struct {
	f      *os.File
	blocks uint64
	// log holds the block images a journal replay would write, by block.
	log    map[uint64][]byte
	groups uint32
	unlock func()
}

// Open opens the volume at path for reading. It takes a shared lock, as
// rungv does for a read-only mount, so a sandbox cannot write the volume
// while it is open, and fails with ErrInUse when a sandbox holds it.
func Open(path string) (*Volume, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	unlock, err := lockShared(f)
	if err != nil {
		f.Close()
		return nil, err
	}
	v := &Volume{f: f, unlock: unlock}
	if err := v.load(); err != nil {
		v.Close()
		return nil, err
	}
	return v, nil
}

// Close releases the volume.
func (v *Volume) Close() error {
	if v.unlock != nil {
		v.unlock()
		v.unlock = nil
	}
	return v.f.Close()
}

func (v *Volume) load() error {
	sb, err := v.super()
	if err != nil {
		return err
	}
	if err := checkSuper(sb); err != nil {
		return err
	}
	if le.Uint32(sb[0x5C:])&compatHasJournal != 0 {
		if err := v.replay(sb); err != nil {
			return err
		}
		// The log can hold block 0.
		if sb, err = v.super(); err != nil {
			return err
		}
		if err := checkSuper(sb); err != nil {
			return err
		}
	}
	v.blocks = uint64(le.Uint32(sb[0x04:])) | uint64(le.Uint32(sb[0x150:]))<<32
	v.groups = uint32((v.blocks + blocksPerGroup - 1) / blocksPerGroup)
	return nil
}

// super reads the 1024 bytes of the superblock.
func (v *Volume) super() ([]byte, error) {
	b, err := v.block(0)
	if err != nil {
		return nil, err
	}
	return b[superOffset : superOffset+1024], nil
}

func checkSuper(sb []byte) error {
	if le.Uint16(sb[0x38:]) != superMagic {
		return corrupt("bad superblock magic")
	}
	if got := le.Uint32(sb[0x18:]); got != 2 {
		return fmt.Errorf("ext4lite: block size %d is not 4096", 1024<<got)
	}
	if le.Uint32(sb[0x20:]) != blocksPerGroup || le.Uint32(sb[0x28:]) != inodesPerGroup || le.Uint16(sb[0x58:]) != inodeSize || le.Uint16(sb[0xFE:]) != descSize {
		return fmt.Errorf("ext4lite: not an ext4lite volume (geometry)")
	}
	if inc := le.Uint32(sb[0x60:]); inc&^knownIncompat != 0 {
		return fmt.Errorf("ext4lite: unsupported features %#x", inc&^knownIncompat)
	}
	if le.Uint32(sb[0x104:]) != 0 {
		return fmt.Errorf("ext4lite: first_meta_bg %d is not supported", le.Uint32(sb[0x104:]))
	}
	return nil
}

// block reads block n, as a journal replay would leave it.
func (v *Volume) block(n uint64) ([]byte, error) {
	if b, ok := v.log[n]; ok {
		return bytes.Clone(b), nil
	}
	b := make([]byte, blockSize)
	if _, err := v.f.ReadAt(b, int64(n)*blockSize); err != nil {
		if errors.Is(err, io.EOF) {
			return nil, corrupt("block %d is past the end of the file", n)
		}
		return nil, err
	}
	return b, nil
}

// hasSuper reports whether group g holds a superblock backup (sparse_super).
func hasSuper(g uint32) bool {
	if g <= 1 {
		return true
	}
	if g&1 == 0 {
		return false
	}
	for _, base := range []uint32{3, 5, 7} {
		n := g
		for n%base == 0 {
			n /= base
		}
		if n == 1 {
			return true
		}
	}
	return false
}

// inodeTable returns the first block of group g's inode table, from its
// group descriptor. With meta_bg, the descriptors of each run of 64 groups
// live in the first group of the run, behind its superblock backup.
func (v *Volume) inodeTable(g uint32) (uint64, error) {
	if g >= v.groups {
		return 0, corrupt("group %d of %d", g, v.groups)
	}
	first := g / descsPerBlock * descsPerBlock
	blk := uint64(first) * blocksPerGroup
	if hasSuper(first) {
		blk++
	}
	b, err := v.block(blk)
	if err != nil {
		return 0, err
	}
	d := b[(g%descsPerBlock)*descSize:]
	return uint64(le.Uint32(d[0x08:])) | uint64(le.Uint32(d[0x28:]))<<32, nil
}

// rawInode reads inode ino's 256 bytes.
func (v *Volume) rawInode(ino uint32) ([]byte, error) {
	if ino == 0 {
		return nil, corrupt("inode 0")
	}
	idx := ino - 1
	table, err := v.inodeTable(idx / inodesPerGroup)
	if err != nil {
		return nil, err
	}
	within := idx % inodesPerGroup
	b, err := v.block(table + uint64(within/(blockSize/inodeSize)))
	if err != nil {
		return nil, err
	}
	off := int(within%(blockSize/inodeSize)) * inodeSize
	return b[off : off+inodeSize], nil
}

// Inode is what an inode says about a file.
type Inode struct {
	Ino   uint32
	Mode  uint32 // file type and permission bits, as st_mode
	UID   uint32
	GID   uint32
	Size  uint64
	Links uint16
	Atime time.Time
	Mtime time.Time
	Ctime time.Time
	// Major and Minor are a device node's numbers.
	Major, Minor uint32

	raw []byte
}

// IsDir reports whether the inode is a directory.
func (n *Inode) IsDir() bool { return n.Mode&ModeType == ModeDir }

// IsWhiteout reports whether the inode is an overlay whiteout: a character
// device 0:0.
func (n *Inode) IsWhiteout() bool {
	return n.Mode&ModeType == ModeChar && n.Major == 0 && n.Minor == 0
}

func (n *Inode) flags() uint32 { return le.Uint32(n.raw[0x20:]) }

// Stat reads inode ino.
func (v *Volume) Stat(ino uint32) (*Inode, error) {
	raw, err := v.rawInode(ino)
	if err != nil {
		return nil, err
	}
	n := &Inode{
		Ino:   ino,
		Mode:  uint32(le.Uint16(raw[0x00:])),
		UID:   uint32(le.Uint16(raw[0x02:])) | uint32(le.Uint16(raw[0x78:]))<<16,
		GID:   uint32(le.Uint16(raw[0x18:])) | uint32(le.Uint16(raw[0x7A:]))<<16,
		Size:  uint64(le.Uint32(raw[0x04:])) | uint64(le.Uint32(raw[0x6C:]))<<32,
		Links: le.Uint16(raw[0x1A:]),
		raw:   raw,
	}
	extra := int(le.Uint16(raw[0x80:]))
	stamp := func(lo, ex int) time.Time {
		sec := int64(int32(le.Uint32(raw[lo:])))
		var nsec int64
		if 128+extra >= ex+4 {
			x := le.Uint32(raw[ex:])
			sec += int64(x&3) << 32
			nsec = int64(x >> 2)
		}
		return time.Unix(sec, nsec)
	}
	n.Atime = stamp(0x08, 0x8C)
	n.Ctime = stamp(0x0C, 0x84)
	n.Mtime = stamp(0x10, 0x88)
	if t := n.Mode & ModeType; t == ModeChar || t == ModeBlock {
		if old := le.Uint32(raw[iBlock:]); old != 0 {
			n.Major, n.Minor = (old>>8)&0xff, old&0xff
		} else {
			x := le.Uint32(raw[iBlock+4:])
			n.Major = (x & 0xfff00) >> 8
			n.Minor = (x & 0xff) | ((x >> 12) & 0xfff00)
		}
	}
	return n, nil
}

// extent is one run of file blocks.
type extent struct {
	lblk   uint32
	n      uint32
	pblk   uint64
	uninit bool
}

// extents reads the extent tree whose root is node (i_block, or a tree
// block).
func (v *Volume) extents(node []byte, depth int, out []extent) ([]extent, error) {
	if le.Uint16(node[0:]) != extMagic {
		return nil, corrupt("bad extent header")
	}
	entries := int(le.Uint16(node[2:]))
	d := int(le.Uint16(node[6:]))
	if d > maxDepth || (depth >= 0 && d != depth) || 12+entries*12 > len(node) {
		return nil, corrupt("bad extent tree")
	}
	for i := range entries {
		e := node[12+i*12:]
		if d == 0 {
			ln := le.Uint16(e[4:])
			x := extent{lblk: le.Uint32(e[0:]), pblk: uint64(le.Uint16(e[6:]))<<32 | uint64(le.Uint32(e[8:]))}
			if ln > 32768 {
				x.uninit = true
				ln -= 32768
			}
			x.n = uint32(ln)
			if x.pblk+uint64(x.n) > v.blocks {
				return nil, corrupt("extent past the end of the volume")
			}
			out = append(out, x)
			continue
		}
		child := uint64(le.Uint16(e[8:]))<<32 | uint64(le.Uint32(e[4:]))
		if child >= v.blocks {
			return nil, corrupt("extent index past the end of the volume")
		}
		b, err := v.block(child)
		if err != nil {
			return nil, err
		}
		if out, err = v.extents(b, d-1, out); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// ibodyXattrs returns the attributes in the inode, behind its extra fields.
func ibodyXattrs(n *Inode) ([]Xattr, error) {
	extra := int(le.Uint16(n.raw[0x80:]))
	start := 128 + extra
	if extra == 0 || start+4 > inodeSize || le.Uint32(n.raw[start:]) != xattrMagic {
		return nil, nil
	}
	region := n.raw[start+4:]
	return parseXattrs(region, 0, region)
}

// Xattr is one extended attribute.
type Xattr struct {
	Name  string // with its prefix, as "user.mime" or "trusted.overlay.opaque"
	Value []byte
}

var xattrPrefixes = map[uint8]string{
	1: "user.",
	2: "system.posix_acl_access",
	3: "system.posix_acl_default",
	4: "trusted.",
	6: "security.",
	7: "system.",
}

// parseXattrs reads the entries that start at region[off]; values are at
// offsets into base.
func parseXattrs(region []byte, off int, base []byte) ([]Xattr, error) {
	var out []Xattr
	for {
		if off+4 > len(region) {
			return nil, corrupt("xattr entries run past their region")
		}
		if le.Uint32(region[off:]) == 0 {
			return out, nil
		}
		if off+16 > len(region) {
			return nil, corrupt("xattr entry runs past its region")
		}
		nameLen := int(region[off])
		index := region[off+1]
		voff := int(le.Uint16(region[off+2:]))
		vsize := int(le.Uint32(region[off+8:]))
		if off+16+nameLen > len(region) || voff+vsize > len(base) || le.Uint32(region[off+4:]) != 0 {
			return nil, corrupt("bad xattr entry")
		}
		name := xattrPrefixes[index] + string(region[off+16:off+16+nameLen])
		out = append(out, Xattr{Name: name, Value: bytes.Clone(base[voff : voff+vsize])})
		off += (16 + nameLen + 3) &^ 3
	}
}

// Xattrs returns the inode's extended attributes, but system.data, which
// holds inline content.
func (v *Volume) Xattrs(n *Inode) ([]Xattr, error) {
	in, err := ibodyXattrs(n)
	if err != nil {
		return nil, err
	}
	var out []Xattr
	for _, x := range in {
		if x.Name != "system.data" {
			out = append(out, x)
		}
	}
	acl := uint64(le.Uint32(n.raw[0x68:])) | uint64(le.Uint16(n.raw[0x76:]))<<32
	if acl == 0 {
		return out, nil
	}
	if acl >= v.blocks {
		return nil, corrupt("xattr block past the end of the volume")
	}
	b, err := v.block(acl)
	if err != nil {
		return nil, err
	}
	if le.Uint32(b[0:]) != xattrMagic {
		return nil, corrupt("bad xattr block")
	}
	blk, err := parseXattrs(b, 32, b)
	if err != nil {
		return nil, err
	}
	return append(out, blk...), nil
}

// inline returns the content of an inline-data inode: i_block, then the
// value of system.data.
func inline(n *Inode) ([]byte, error) {
	if n.Size > iBlockSize+inodeSize {
		return nil, corrupt("inline inode %d has the size %d", n.Ino, n.Size)
	}
	size := int(n.Size)
	out := make([]byte, size)
	copy(out, n.raw[iBlock:iBlock+min(size, iBlockSize)])
	if size > iBlockSize {
		xs, err := ibodyXattrs(n)
		if err != nil {
			return nil, err
		}
		var tail []byte
		for _, x := range xs {
			if x.Name == "system.data" {
				tail = x.Value
			}
		}
		if len(tail) != size-iBlockSize {
			return nil, corrupt("inline inode %d has %d bytes past i_block, want %d", n.Ino, len(tail), size-iBlockSize)
		}
		copy(out[iBlockSize:], tail)
	}
	return out, nil
}

// fileReader reads a file's content from its extents.
type fileReader struct {
	v    *Volume
	exts []extent
	size uint64
	off  uint64
}

func (r *fileReader) Read(p []byte) (int, error) {
	if r.off >= r.size {
		return 0, io.EOF
	}
	lblk := uint32(r.off / blockSize)
	within := int(r.off % blockSize)
	n := int(min(uint64(len(p)), uint64(blockSize-within), r.size-r.off))
	var data []byte
	for _, e := range r.exts {
		if lblk >= e.lblk && lblk-e.lblk < e.n {
			if !e.uninit {
				b, err := r.v.block(e.pblk + uint64(lblk-e.lblk))
				if err != nil {
					return 0, err
				}
				data = b
			}
			break
		}
	}
	if data == nil {
		clear(p[:n]) // a hole or an uninitialized extent reads as zeros
	} else {
		copy(p[:n], data[within:])
	}
	r.off += uint64(n)
	return n, nil
}

// Open returns a reader of a regular file's or a symlink's content.
func (v *Volume) Open(n *Inode) (io.Reader, error) {
	f := n.flags()
	switch {
	case f&flagInlineData != 0:
		b, err := inline(n)
		if err != nil {
			return nil, err
		}
		return bytes.NewReader(b), nil
	case f&flagExtents != 0:
		exts, err := v.extents(n.raw[iBlock:iBlock+iBlockSize], -1, nil)
		if err != nil {
			return nil, err
		}
		return &fileReader{v: v, exts: exts, size: n.Size}, nil
	}
	if n.Size == 0 {
		return bytes.NewReader(nil), nil
	}
	return nil, corrupt("inode %d has neither inline data nor extents", n.Ino)
}

// Readlink returns a symlink's target.
func (v *Volume) Readlink(n *Inode) (string, error) {
	if n.Mode&ModeType != ModeSymlink {
		return "", fmt.Errorf("ext4lite: inode %d is not a symlink", n.Ino)
	}
	// A fast symlink keeps its target in i_block, with neither flag.
	if n.Size < iBlockSize && n.flags()&(flagExtents|flagInlineData) == 0 {
		return string(n.raw[iBlock : iBlock+int(n.Size)]), nil
	}
	r, err := v.Open(n)
	if err != nil {
		return "", err
	}
	b, err := io.ReadAll(io.LimitReader(r, 4096))
	return string(b), err
}

// DirEntry is one entry of a directory.
type DirEntry struct {
	Name string
	Ino  uint32
}

// ReadDir lists a directory, without "." and "..", in on-disk order.
func (v *Volume) ReadDir(n *Inode) ([]DirEntry, error) {
	if !n.IsDir() {
		return nil, fmt.Errorf("ext4lite: inode %d is not a directory", n.Ino)
	}
	var data []byte
	if n.flags()&flagInlineData != 0 {
		b, err := inline(n)
		if err != nil {
			return nil, err
		}
		// An inline directory starts with its parent's inode number.
		if len(b) < 4 {
			return nil, corrupt("inline directory %d", n.Ino)
		}
		data = b[4:]
	} else {
		if n.Size > maxDirSize || n.Size%blockSize != 0 {
			return nil, corrupt("directory %d has the size %d", n.Ino, n.Size)
		}
		r, err := v.Open(n)
		if err != nil {
			return nil, err
		}
		if data, err = io.ReadAll(r); err != nil {
			return nil, err
		}
	}
	var out []DirEntry
	for off := 0; off+8 <= len(data); {
		ino := le.Uint32(data[off:])
		recLen := int(le.Uint16(data[off+4:]))
		nameLen := int(data[off+6])
		if recLen < 8 || off+recLen > len(data) || 8+nameLen > recLen {
			return nil, corrupt("bad directory entry in inode %d", n.Ino)
		}
		if ino != 0 {
			name := string(data[off+8 : off+8+nameLen])
			if name != "." && name != ".." {
				out = append(out, DirEntry{Name: name, Ino: ino})
			}
		}
		off += recLen
	}
	return out, nil
}

// Lookup resolves an absolute path, without following symlinks.
func (v *Volume) Lookup(p string) (*Inode, error) {
	n, err := v.Stat(rootIno)
	if err != nil {
		return nil, err
	}
	for name := range strings.SplitSeq(strings.Trim(path.Clean("/"+p), "/"), "/") {
		if name == "" {
			continue
		}
		ents, err := v.ReadDir(n)
		if err != nil {
			return nil, err
		}
		found := uint32(0)
		for _, e := range ents {
			if e.Name == name {
				found = e.Ino
				break
			}
		}
		if found == 0 {
			return nil, fmt.Errorf("%w: %s", ErrNotExist, p)
		}
		if n, err = v.Stat(found); err != nil {
			return nil, err
		}
	}
	return n, nil
}
