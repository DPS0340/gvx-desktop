package imagefs

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"strings"
	"time"
)

// Pure-Go read-only EROFS image reader supporting the uncompressed layouts
// gvx compiles OCI layers into (FlatPlain, FlatInline, ChunkBased) and inline
// xattrs (for trusted.overlay.opaque).

const (
	erofsMagic          = 0xe0f5e1e2
	erofsSuperOffset    = 1024
	erofsSuperSize      = 128
	erofsInodeSlotBits  = 5
	erofsCompactSize    = 32
	erofsExtendedSize   = 64
	erofsDirentSize     = 12
	erofsBlockMapSize   = 4
	erofsChunkIndexSize = 8
	erofsNullAddr       = math.MaxUint32

	erofsLayoutCompact  = 0
	erofsLayoutExtended = 1

	erofsDataFlatPlain  = 0
	erofsDataFlatInline = 2
	erofsDataChunkBased = 4

	erofsChunkBlkBits = 0x001f
	erofsChunkIndexes = 0x0020

	erofsXattrHeaderSize = 12
	erofsXattrEntrySize  = 4
	erofsXattrAlign      = 4
	erofsXattrIndexTrust = 4

	posixIFMT  = 0o170000
	posixIFREG = 0o100000
	posixIFDIR = 0o040000
	posixIFCHR = 0o020000
	posixIFLNK = 0o120000
)

type erofsImage struct {
	file           *os.File
	fileSize       uint64
	blockSizeBits  uint8
	blockSize      uint64
	rootNid        uint64
	metaBlockAddr  uint32
	xattrBlockAddr uint32
	buildTime      uint64
	buildTimeNsec  uint32
}

type erofsInode struct {
	nid         uint64
	format      uint16
	dataLayout  uint16
	mode        uint16
	size        uint64
	uid         uint32
	gid         uint32
	mtime       uint64
	mtimeNsec   uint32
	rdev        uint32
	dataOff     uint64
	idataOff    uint64
	blocks      uint64
	xattrOff    uint64
	xattrIsize  uint32
	chunkFormat uint16
}

type erofsDirent struct {
	name     string
	fileType uint8
	nid      uint64
}

func openEROFS(path string) (*erofsImage, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	st, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, err
	}
	img := &erofsImage{file: f, fileSize: uint64(st.Size())}
	if err := img.readSuper(); err != nil {
		f.Close()
		return nil, err
	}
	return img, nil
}

func (img *erofsImage) Close() error {
	if img.file == nil {
		return nil
	}
	return img.file.Close()
}

func (img *erofsImage) readAt(off, n uint64) ([]byte, error) {
	if n == 0 {
		return nil, nil
	}
	end := off + n
	if off > img.fileSize || end < off || end > img.fileSize {
		return nil, fmt.Errorf("erofs: out of range [%d, %d) in %d-byte image", off, end, img.fileSize)
	}
	buf := make([]byte, n)
	if _, err := img.file.ReadAt(buf, int64(off)); err != nil && !errors.Is(err, io.EOF) {
		return nil, err
	}
	return buf, nil
}

func (img *erofsImage) readSuper() error {
	b, err := img.readAt(erofsSuperOffset, erofsSuperSize)
	if err != nil {
		return fmt.Errorf("erofs: superblock: %w", err)
	}
	magic := binary.LittleEndian.Uint32(b[0:4])
	if magic != erofsMagic {
		return fmt.Errorf("erofs: bad magic 0x%x", magic)
	}
	img.blockSizeBits = b[12]
	if img.blockSizeBits < 9 || img.blockSizeBits > 16 {
		return fmt.Errorf("erofs: unsupported block size bits %d", img.blockSizeBits)
	}
	img.blockSize = 1 << img.blockSizeBits
	img.rootNid = uint64(binary.LittleEndian.Uint16(b[14:16]))
	img.buildTime = binary.LittleEndian.Uint64(b[24:32])
	img.buildTimeNsec = binary.LittleEndian.Uint32(b[32:36])
	img.metaBlockAddr = binary.LittleEndian.Uint32(b[40:44])
	img.xattrBlockAddr = binary.LittleEndian.Uint32(b[44:48])
	return nil
}

func (img *erofsImage) blockOff(addr uint32) uint64 {
	return uint64(addr) << img.blockSizeBits
}

func (img *erofsImage) nidOff(nid uint64) uint64 {
	return img.blockOff(img.metaBlockAddr) + (nid << erofsInodeSlotBits)
}

func erofsXattrIbodySize(icount uint16) uint32 {
	if icount == 0 {
		return 0
	}
	return erofsXattrHeaderSize + erofsXattrAlign*(uint32(icount)-1)
}

func (img *erofsImage) inode(nid uint64) (erofsInode, error) {
	off := img.nidOff(nid)
	hdr, err := img.readAt(off, 2)
	if err != nil {
		return erofsInode{}, err
	}
	format := binary.LittleEndian.Uint16(hdr)
	layout := format & 1
	dataLayout := (format >> 1) & 7

	ino := erofsInode{nid: nid, format: format, dataLayout: dataLayout}
	var rawBlk uint32
	var inoSize uint64
	var xattrCount uint16

	switch layout {
	case erofsLayoutCompact:
		b, err := img.readAt(off, erofsCompactSize)
		if err != nil {
			return erofsInode{}, err
		}
		xattrCount = binary.LittleEndian.Uint16(b[2:4])
		ino.mode = binary.LittleEndian.Uint16(b[4:6])
		ino.size = uint64(binary.LittleEndian.Uint32(b[8:12]))
		rawBlk = binary.LittleEndian.Uint32(b[16:20])
		ino.uid = uint32(binary.LittleEndian.Uint16(b[24:26]))
		ino.gid = uint32(binary.LittleEndian.Uint16(b[26:28]))
		ino.mtime = img.buildTime
		ino.mtimeNsec = img.buildTimeNsec
		inoSize = erofsCompactSize
	case erofsLayoutExtended:
		b, err := img.readAt(off, erofsExtendedSize)
		if err != nil {
			return erofsInode{}, err
		}
		xattrCount = binary.LittleEndian.Uint16(b[2:4])
		ino.mode = binary.LittleEndian.Uint16(b[4:6])
		ino.size = binary.LittleEndian.Uint64(b[8:16])
		rawBlk = binary.LittleEndian.Uint32(b[16:20])
		ino.uid = binary.LittleEndian.Uint32(b[24:28])
		ino.gid = binary.LittleEndian.Uint32(b[28:32])
		ino.mtime = binary.LittleEndian.Uint64(b[32:40])
		ino.mtimeNsec = binary.LittleEndian.Uint32(b[40:44])
		inoSize = erofsExtendedSize
	default:
		return erofsInode{}, fmt.Errorf("erofs: unsupported inode layout %d", layout)
	}

	ino.xattrOff = off + inoSize
	ino.xattrIsize = erofsXattrIbodySize(xattrCount)
	inoEnd := ino.xattrOff + uint64(ino.xattrIsize)
	ino.blocks = (ino.size + img.blockSize - 1) / img.blockSize
	if t := ino.mode & posixIFMT; t == posixIFCHR || t == 0o060000 || t == 0o010000 || t == 0o140000 {
		ino.rdev = rawBlk
	}

	switch dataLayout {
	case erofsDataFlatInline:
		ino.idataOff = inoEnd
		ino.dataOff = img.blockOff(rawBlk)
	case erofsDataFlatPlain:
		ino.dataOff = img.blockOff(rawBlk)
	case erofsDataChunkBased:
		ino.chunkFormat = uint16(rawBlk)
		unit := uint64(erofsBlockMapSize)
		if ino.chunkFormat&erofsChunkIndexes != 0 {
			unit = erofsChunkIndexSize
		}
		ino.dataOff = (inoEnd + unit - 1) &^ (unit - 1)
	default:
		return erofsInode{}, fmt.Errorf("erofs: unsupported data layout %d", dataLayout)
	}
	return ino, nil
}

func (ino erofsInode) kind() Kind {
	switch ino.mode & posixIFMT {
	case posixIFDIR:
		return KindDir
	case posixIFREG:
		return KindFile
	case posixIFLNK:
		return KindSymlink
	default:
		return KindOther
	}
}

func (ino erofsInode) isWhiteout() bool {
	return ino.mode&posixIFMT == posixIFCHR && ino.rdev == 0
}

func (ino erofsInode) modTime() time.Time {
	if ino.mtime == 0 {
		return time.Time{}
	}
	return time.Unix(int64(ino.mtime), int64(ino.mtimeNsec))
}

func (img *erofsImage) isOpaque(ino erofsInode) bool {
	if ino.xattrIsize < erofsXattrHeaderSize {
		return false
	}
	ibody, err := img.readAt(ino.xattrOff, uint64(ino.xattrIsize))
	if err != nil {
		return false
	}
	shared := uint32(ibody[4])
	pos := uint32(erofsXattrHeaderSize) + shared*erofsXattrAlign
	if pos > ino.xattrIsize {
		return false
	}
	for pos+erofsXattrEntrySize <= ino.xattrIsize {
		nameLen := uint32(ibody[pos])
		nameIdx := ibody[pos+1]
		valSize := uint32(binary.LittleEndian.Uint16(ibody[pos+2 : pos+4]))
		entrySize := (erofsXattrEntrySize + nameLen + valSize + erofsXattrAlign - 1) &^ (erofsXattrAlign - 1)
		if pos+entrySize > ino.xattrIsize {
			break
		}
		if nameIdx == erofsXattrIndexTrust {
			name := string(ibody[pos+4 : pos+4+nameLen])
			val := string(ibody[pos+4+nameLen : pos+4+nameLen+valSize])
			if name == "overlay.opaque" && val == "y" {
				return true
			}
		}
		pos += entrySize
	}
	return false
}

func (img *erofsImage) dirBlock(ino erofsInode, idx uint64) (off, size uint64) {
	last := idx == ino.blocks-1
	if last && ino.idataOff != 0 {
		off = ino.idataOff
	} else {
		off = ino.dataOff + idx*img.blockSize
	}
	size = img.blockSize
	if last {
		if tail := ino.size & (img.blockSize - 1); tail != 0 {
			size = tail
		}
	}
	return off, size
}

func (img *erofsImage) readDir(ino erofsInode) ([]erofsDirent, error) {
	if ino.kind() != KindDir {
		return nil, errors.New("not a directory")
	}
	var out []erofsDirent
	for bIdx := uint64(0); bIdx < ino.blocks; bIdx++ {
		off, size := img.dirBlock(ino, bIdx)
		blk, err := img.readAt(off, size)
		if err != nil {
			return nil, err
		}
		if len(blk) < erofsDirentSize {
			continue
		}
		nameOff0 := uint32(binary.LittleEndian.Uint16(blk[8:10]))
		if nameOff0 < erofsDirentSize || uint64(nameOff0) > size {
			return nil, errors.New("erofs: corrupt directory block")
		}
		count := int(nameOff0 / erofsDirentSize)
		for i := range count {
			doff := i * erofsDirentSize
			nid := uint64(binary.LittleEndian.Uint32(blk[doff:doff+4])) |
				(uint64(binary.LittleEndian.Uint32(blk[doff+4:doff+8])) << 32)
			nameStart := uint64(binary.LittleEndian.Uint16(blk[doff+8 : doff+10]))
			ftype := blk[doff+10]
			nameEnd := size
			if i+1 < count {
				nextOff := (i + 1) * erofsDirentSize
				nameEnd = uint64(binary.LittleEndian.Uint16(blk[nextOff+8 : nextOff+10]))
			}
			if nameStart >= nameEnd || nameEnd > size {
				return nil, errors.New("erofs: corrupt dirent name")
			}
			raw := blk[nameStart:nameEnd]
			if i+1 == count {
				if z := bytes.IndexByte(raw, 0); z >= 0 {
					raw = raw[:z]
				}
			}
			name := string(raw)
			if name == "" || name == "." || name == ".." {
				continue
			}
			out = append(out, erofsDirent{name: name, fileType: ftype, nid: nid})
		}
	}
	return out, nil
}

func (img *erofsImage) lookupChild(dir erofsInode, want string) (erofsInode, bool, error) {
	entries, err := img.readDir(dir)
	if err != nil {
		return erofsInode{}, false, err
	}
	for _, d := range entries {
		if d.name == want {
			ino, err := img.inode(d.nid)
			return ino, true, err
		}
	}
	return erofsInode{}, false, nil
}

func (img *erofsImage) lookup(cleanPath string) (erofsInode, bool, error) {
	cur, err := img.inode(img.rootNid)
	if err != nil {
		return erofsInode{}, false, err
	}
	trimmed := strings.Trim(cleanPath, "/")
	if trimmed == "" {
		return cur, true, nil
	}
	for seg := range strings.SplitSeq(trimmed, "/") {
		if cur.kind() != KindDir {
			return erofsInode{}, false, nil
		}
		next, ok, err := img.lookupChild(cur, seg)
		if err != nil || !ok {
			return erofsInode{}, false, err
		}
		cur = next
	}
	return cur, true, nil
}

func (img *erofsImage) readLink(ino erofsInode) (string, error) {
	if ino.kind() != KindSymlink {
		return "", errors.New("not a symlink")
	}
	b, err := img.readData(ino, 4096)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

func (img *erofsImage) readData(ino erofsInode, limit int64) ([]byte, error) {
	toRead := ino.size
	if limit > 0 && toRead > uint64(limit) {
		toRead = uint64(limit)
	}
	if toRead == 0 {
		return nil, nil
	}
	switch ino.dataLayout {
	case erofsDataFlatPlain:
		return img.readAt(ino.dataOff, toRead)
	case erofsDataFlatInline:
		tailOff := ino.size &^ (img.blockSize - 1)
		out := make([]byte, 0, toRead)
		if tailOff > 0 {
			n := min(toRead, tailOff)
			head, err := img.readAt(ino.dataOff, n)
			if err != nil {
				return nil, err
			}
			out = append(out, head...)
		}
		if uint64(len(out)) < toRead {
			rem := toRead - uint64(len(out))
			tail, err := img.readAt(ino.idataOff, rem)
			if err != nil {
				return nil, err
			}
			out = append(out, tail...)
		}
		return out, nil
	case erofsDataChunkBased:
		chunkBits := img.blockSizeBits + uint8(ino.chunkFormat&erofsChunkBlkBits)
		chunkSize := uint64(1) << chunkBits
		unit := uint64(erofsBlockMapSize)
		indexes := ino.chunkFormat&erofsChunkIndexes != 0
		if indexes {
			unit = erofsChunkIndexSize
		}
		out := make([]byte, 0, toRead)
		for pos := uint64(0); pos < toRead; {
			chunkIdx := pos >> chunkBits
			entryOff := ino.dataOff + chunkIdx*unit
			eb, err := img.readAt(entryOff, unit)
			if err != nil {
				return nil, err
			}
			var blkAddr uint32
			if indexes {
				blkAddr = binary.LittleEndian.Uint32(eb[4:8])
			} else {
				blkAddr = binary.LittleEndian.Uint32(eb[0:4])
			}
			chunkOff := pos & (chunkSize - 1)
			n := min(toRead-pos, chunkSize-chunkOff)
			if blkAddr == erofsNullAddr {
				out = append(out, make([]byte, n)...)
			} else {
				part, err := img.readAt(img.blockOff(blkAddr)+chunkOff, n)
				if err != nil {
					return nil, err
				}
				out = append(out, part...)
			}
			pos += n
		}
		return out, nil
	default:
		return nil, fmt.Errorf("erofs: unsupported data layout %d", ino.dataLayout)
	}
}
