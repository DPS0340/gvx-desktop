package ext4lite

// The journal is JBD2 in the subset ext4lite writes: a linear log of
// transactions, each optional revoke blocks, descriptor blocks with the data
// blocks they tag, and a commit block with a CRC-32 of the descriptors and
// data. A replay writes the newest image of each tagged block home, but a
// block a later (or the same) transaction revoked. The reader keeps those
// images in memory instead of writing them.

const (
	jbd2Magic           = 0xC03B3998
	jbd2BlockDescriptor = 1
	jbd2BlockCommit     = 2
	jbd2BlockSuperV2    = 4
	jbd2BlockRevoke     = 5

	jbd2CompatChecksum = 0x1
	jbd2Incompat64bit  = 0x2
	jbd2KnownIncompat  = 0x1 | jbd2Incompat64bit

	jbd2FlagEscape   = 1
	jbd2FlagSameUUID = 2
	jbd2FlagLastTag  = 8

	jbd2CRC32Checksum = 1
	jbd2CRC32Len      = 4
)

var crc32beTable = func() (t [256]uint32) {
	for i := range t {
		c := uint32(i) << 24
		for range 8 {
			if c&0x80000000 != 0 {
				c = c<<1 ^ 0x04C11DB7
			} else {
				c <<= 1
			}
		}
		t[i] = c
	}
	return t
}()

// crc32be is the big-endian CRC-32 of JBD2's checksum: no reflection, no
// final inversion.
func crc32be(crc uint32, p []byte) uint32 {
	for _, b := range p {
		crc = crc<<8 ^ crc32beTable[byte(crc>>24)^b]
	}
	return crc
}

type logTag struct {
	blk    uint64
	pos    uint32
	escape bool
}

type logTxn struct {
	tid     uint32
	tags    []logTag
	revokes []uint64
}

// replay reads the journal (inode 8) and, when its log needs a replay, keeps
// the images the replay would write in v.log.
func (v *Volume) replay(sb []byte) error {
	if got := le.Uint32(sb[0xE0:]); got != journalIno {
		return corrupt("journal inode is %d", got)
	}
	blocks := superBlocks(sb)
	v.blocks = blocks
	v.groups = uint32((blocks + blocksPerGroup - 1) / blocksPerGroup)
	jn, err := v.Stat(journalIno)
	if err != nil {
		return err
	}
	if jn.Mode&ModeType != ModeRegular || jn.Size%blockSize != 0 || jn.flags()&flagExtents == 0 {
		return corrupt("journal inode")
	}
	exts, err := v.extents(jn.raw[iBlock:iBlock+iBlockSize], -1, nil)
	if err != nil {
		return err
	}
	jblocks := uint32(min(jn.Size/blockSize, 1<<31))
	phys := func(pos uint32) (uint64, bool) {
		for _, e := range exts {
			if pos >= e.lblk && pos-e.lblk < e.n {
				return e.pblk + uint64(pos-e.lblk), true
			}
		}
		return 0, false
	}
	read := func(pos uint32) ([]byte, error) {
		p, ok := phys(pos)
		if !ok {
			return nil, corrupt("journal block %d is not mapped", pos)
		}
		return v.block(p)
	}
	holds := func(blk uint64) bool {
		for _, e := range exts {
			if blk >= e.pblk && blk-e.pblk < uint64(e.n) {
				return true
			}
		}
		return false
	}

	js, err := read(0)
	if err != nil {
		return err
	}
	if be.Uint32(js[0:]) != jbd2Magic || be.Uint32(js[4:]) != jbd2BlockSuperV2 || be.Uint32(js[0x0C:]) != blockSize {
		return corrupt("journal superblock")
	}
	maxlen, first := be.Uint32(js[0x10:]), be.Uint32(js[0x14:])
	sequence, start := be.Uint32(js[0x18:]), be.Uint32(js[0x1C:])
	compat, incompat := be.Uint32(js[0x24:]), be.Uint32(js[0x28:])
	if maxlen > jblocks || first < 1 || first >= maxlen || (start != 0 && (start < first || start >= maxlen)) {
		return corrupt("journal geometry")
	}
	if incompat&^jbd2KnownIncompat != 0 || be.Uint32(js[0x2C:]) != 0 {
		return corrupt("journal uses unsupported features %#x", incompat)
	}
	if start == 0 {
		return nil // no log to apply, with the recover flag or without
	}

	txns, err := scanLog(read, start, sequence, first, maxlen, compat, incompat)
	if err != nil {
		return err
	}
	revoked := make(map[uint64]uint32)
	for _, t := range txns {
		for _, b := range t.revokes {
			if t.tid > revoked[b] {
				revoked[b] = t.tid
			}
		}
	}
	st, err := v.f.Stat()
	if err != nil {
		return err
	}
	// A transaction can resize the volume; the superblock image of the last
	// transaction that holds one tells the final size.
	for i := len(txns) - 1; i >= 0; i-- {
		found := false
		for _, tag := range txns[i].tags {
			if tag.blk != 0 {
				continue
			}
			if rt, ok := revoked[0]; ok && rt >= txns[i].tid {
				continue
			}
			d, err := read(tag.pos)
			if err != nil {
				return err
			}
			blocks = superBlocks(d[superOffset:])
			found = true
			break
		}
		if found {
			break
		}
	}
	blocks = min(blocks, uint64(st.Size()/blockSize))
	log := make(map[uint64][]byte)
	for _, t := range txns {
		for _, tag := range t.tags {
			if rt, ok := revoked[tag.blk]; ok && rt >= t.tid {
				continue
			}
			if tag.blk >= blocks || holds(tag.blk) {
				continue
			}
			d, err := read(tag.pos)
			if err != nil {
				return err
			}
			if tag.escape {
				be.PutUint32(d[0:], jbd2Magic)
			}
			log[tag.blk] = d
		}
	}
	v.log = log
	return nil
}

// scanLog reads the committed transactions from start. It stops at the
// first block that is not part of a complete, valid transaction.
func scanLog(read func(uint32) ([]byte, error), start, tid, first, maxlen, compat, incompat uint32) ([]logTxn, error) {
	var out []logTxn
	tagSize, recSize := 8, 4
	if incompat&jbd2Incompat64bit != 0 {
		tagSize, recSize = 12, 8
	}
	pos := start
	crc := ^uint32(0)
	cur := logTxn{tid: tid}
	for steps := uint32(0); steps <= maxlen; steps++ {
		if pos < first || pos >= maxlen {
			return out, nil
		}
		b, err := read(pos)
		if err != nil {
			return out, err
		}
		if be.Uint32(b[0:]) != jbd2Magic || be.Uint32(b[8:]) != tid {
			return out, nil
		}
		switch be.Uint32(b[4:]) {
		case jbd2BlockDescriptor:
			crc = crc32be(crc, b)
			off, count := 12, 0
			for off+tagSize <= blockSize {
				blk := uint64(be.Uint32(b[off:]))
				flags := be.Uint16(b[off+6:])
				if tagSize == 12 {
					blk |= uint64(be.Uint32(b[off+8:])) << 32
				}
				count++
				if pos+uint32(count) >= maxlen {
					return out, nil
				}
				cur.tags = append(cur.tags, logTag{blk: blk, pos: pos + uint32(count), escape: flags&jbd2FlagEscape != 0})
				off += tagSize
				if flags&jbd2FlagSameUUID == 0 {
					off += 16
				}
				if flags&jbd2FlagLastTag != 0 {
					break
				}
			}
			if count == 0 {
				return out, nil
			}
			for k := range count {
				d, err := read(pos + 1 + uint32(k))
				if err != nil {
					return out, err
				}
				crc = crc32be(crc, d)
			}
			pos += 1 + uint32(count)
		case jbd2BlockRevoke:
			n := be.Uint32(b[12:])
			if n < 16 || n > blockSize {
				return out, nil
			}
			for off := 16; off+recSize <= int(n); off += recSize {
				if recSize == 8 {
					cur.revokes = append(cur.revokes, be.Uint64(b[off:]))
				} else {
					cur.revokes = append(cur.revokes, uint64(be.Uint32(b[off:])))
				}
			}
			pos++
		case jbd2BlockCommit:
			if compat&jbd2CompatChecksum != 0 && b[12] != 0 {
				if b[12] != jbd2CRC32Checksum || b[13] != jbd2CRC32Len || be.Uint32(b[16:]) != crc {
					return out, nil
				}
			}
			out = append(out, cur)
			tid++
			cur = logTxn{tid: tid}
			crc = ^uint32(0)
			pos++
		default:
			return out, nil
		}
	}
	return out, nil
}
