package vhdx

import (
	"encoding/binary"
	"hash/crc32"
)

// Spec describes a synthetic VHDX for tests: the parts of the format
// Read reads, with the payload entries given as (state, file offset in
// MiB) pairs.
type Spec struct {
	BlockSize   uint32
	SectorSize  uint32
	VirtualSize uint64
	Flags       uint32
	Entries     [][2]uint64
	// BreakFirstRegionTable corrupts the first copy's checksum.
	BreakFirstRegionTable bool
}

// Bytes lays the file out as Hyper-V does: identifier at 0, the two
// region tables at 192 KiB and 256 KiB, the metadata region at 1 MiB and
// the BAT at 2 MiB, 1 MiB each; the file ends at 3 MiB (no payload).
func (v Spec) Bytes() []byte {
	const metaOff, batOff, size = 1 << 20, 2 << 20, 3 << 20
	b := make([]byte, size)
	copy(b, vhdxSignature)

	meta := b[metaOff:]
	copy(meta, vhdxMetadataSignature)
	binary.LittleEndian.PutUint16(meta[10:], 3)
	item := func(i int, id [16]byte, off uint32, data []byte) {
		e := meta[vhdxMetadataHeaderSize+i*vhdxMetadataEntrySize:]
		copy(e, id[:])
		binary.LittleEndian.PutUint32(e[16:], off)
		binary.LittleEndian.PutUint32(e[20:], uint32(len(data)))
		copy(meta[off:], data)
	}
	fp := make([]byte, 8)
	binary.LittleEndian.PutUint32(fp, v.BlockSize)
	binary.LittleEndian.PutUint32(fp[4:], v.Flags)
	item(0, fileParametersID, 64<<10, fp)
	vs := make([]byte, 8)
	binary.LittleEndian.PutUint64(vs, v.VirtualSize)
	item(1, virtualDiskSizeID, 64<<10+8, vs)
	ss := make([]byte, 4)
	binary.LittleEndian.PutUint32(ss, v.SectorSize)
	item(2, logicalSectorSizeID, 64<<10+16, ss)

	chunkRatio := int((uint64(1) << 23) * uint64(v.SectorSize) / uint64(v.BlockSize))
	bat := b[batOff:]
	for n, e := range v.Entries {
		i := n + n/chunkRatio
		binary.LittleEndian.PutUint64(bat[i*vhdxBATEntrySize:], e[0]|e[1]<<20)
	}

	for copyNo, off := range []int{vhdxRegionTableOffset, vhdxRegionTableOffset + vhdxRegionTableSize} {
		table := b[off : off+vhdxRegionTableSize]
		copy(table, vhdxRegionSignature)
		binary.LittleEndian.PutUint32(table[8:], 2)
		for i, r := range []struct {
			id     [16]byte
			offset uint64
		}{{metadataRegionID, metaOff}, {batRegionID, batOff}} {
			e := table[16+i*vhdxRegionEntrySize:]
			copy(e, r.id[:])
			binary.LittleEndian.PutUint64(e[16:], r.offset)
			binary.LittleEndian.PutUint32(e[24:], 1<<20)
			binary.LittleEndian.PutUint32(e[28:], 1)
		}
		sum := crc32.Checksum(table, crc32c)
		if copyNo == 0 && v.BreakFirstRegionTable {
			sum++
		}
		binary.LittleEndian.PutUint32(table[4:], sum)
	}
	return b
}
