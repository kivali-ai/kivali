package hyperv

import (
	"encoding/binary"
	"hash/crc32"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// testVHDX describes a synthetic VHDX: the parts of the format readVHDX
// reads, with the payload entries given as (state, file offset in MiB)
// pairs.
type testVHDX struct {
	blockSize   uint32
	sectorSize  uint32
	virtualSize uint64
	flags       uint32
	entries     [][2]uint64
	// breakFirstRegionTable corrupts the first copy's checksum.
	breakFirstRegionTable bool
}

// write lays the file out as Hyper-V does: identifier at 0, the two
// region tables at 192 KiB and 256 KiB, the metadata region at 1 MiB and
// the BAT at 2 MiB, 1 MiB each; the file ends at 3 MiB (no payload).
func (v testVHDX) write(t *testing.T) string {
	t.Helper()
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
	binary.LittleEndian.PutUint32(fp, v.blockSize)
	binary.LittleEndian.PutUint32(fp[4:], v.flags)
	item(0, fileParametersID, 64<<10, fp)
	vs := make([]byte, 8)
	binary.LittleEndian.PutUint64(vs, v.virtualSize)
	item(1, virtualDiskSizeID, 64<<10+8, vs)
	ss := make([]byte, 4)
	binary.LittleEndian.PutUint32(ss, v.sectorSize)
	item(2, logicalSectorSizeID, 64<<10+16, ss)

	chunkRatio := int((uint64(1) << 23) * uint64(v.sectorSize) / uint64(v.blockSize))
	bat := b[batOff:]
	for n, e := range v.entries {
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
		if copyNo == 0 && v.breakFirstRegionTable {
			sum++
		}
		binary.LittleEndian.PutUint32(table[4:], sum)
	}
	p := filepath.Join(t.TempDir(), "data.vhdx")
	if err := os.WriteFile(p, b, 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// The payload states a dynamic disk's BAT holds.
const (
	stNotPresent = 0
	stZero       = 2
	stUnmapped   = 3
	stFull       = 6
)

// A block the guest discarded keeps its place in the file until the disk
// is compacted; one that never had a place, or whose place the host has
// dropped, costs nothing. The sector bitmap entry after each chunk of
// payload entries is skipped.
func TestReadVHDXCountsDiscardedBlocks(t *testing.T) {
	// 256 MiB blocks with 512-byte sectors: a chunk is 16 payload
	// entries, so 20 blocks put one sector bitmap entry in the middle.
	v := testVHDX{blockSize: 256 << 20, sectorSize: 512, virtualSize: 20 * 256 << 20}
	for n := range 20 {
		switch {
		case n < 10:
			v.entries = append(v.entries, [2]uint64{stFull, uint64(3 + n*256)})
		case n < 14:
			v.entries = append(v.entries, [2]uint64{stUnmapped, uint64(3 + n*256)}) // discarded, space kept
		case n < 16:
			v.entries = append(v.entries, [2]uint64{stUnmapped, 0}) // discarded, space dropped
		case n == 16:
			v.entries = append(v.entries, [2]uint64{stZero, 0})
		default:
			v.entries = append(v.entries, [2]uint64{stNotPresent, 0})
		}
	}
	l, err := readVHDX(v.write(t))
	if err != nil {
		t.Fatal(err)
	}
	if l.Blocks != 20 || l.Present != 10 || l.Discarded != 4 || l.BlockSize != 256<<20 || l.VirtualSize != 20*256<<20 || l.Fixed {
		t.Fatalf("layout = %+v", l)
	}
	if l.DiscardedBytes() != 4*256<<20 {
		t.Fatalf("DiscardedBytes = %d", l.DiscardedBytes())
	}
	if l.FileSize != 3<<20 {
		t.Fatalf("FileSize = %d", l.FileSize)
	}
}

// A virtual size that is not a whole number of blocks rounds up.
func TestReadVHDXPartialLastBlock(t *testing.T) {
	v := testVHDX{blockSize: 32 << 20, sectorSize: 512, virtualSize: 100 << 20,
		entries: [][2]uint64{{stFull, 3}, {stFull, 35}, {stUnmapped, 67}, {stNotPresent, 0}}}
	l, err := readVHDX(v.write(t))
	if err != nil {
		t.Fatal(err)
	}
	if l.Blocks != 4 || l.Present != 2 || l.Discarded != 1 {
		t.Fatalf("layout = %+v", l)
	}
}

// A fixed disk keeps every block's space by design: nothing to give back.
func TestReadVHDXFixedDiskHasNothingDiscarded(t *testing.T) {
	v := testVHDX{blockSize: 32 << 20, sectorSize: 512, virtualSize: 64 << 20, flags: vhdxLeaveBlocksAllocated,
		entries: [][2]uint64{{stUnmapped, 3}, {stUnmapped, 35}}}
	l, err := readVHDX(v.write(t))
	if err != nil {
		t.Fatal(err)
	}
	if !l.Fixed || l.Discarded != 0 {
		t.Fatalf("layout = %+v", l)
	}
}

func TestReadVHDXSecondRegionTableCopy(t *testing.T) {
	v := testVHDX{blockSize: 32 << 20, sectorSize: 512, virtualSize: 64 << 20, breakFirstRegionTable: true,
		entries: [][2]uint64{{stFull, 3}, {stUnmapped, 35}}}
	l, err := readVHDX(v.write(t))
	if err != nil {
		t.Fatal(err)
	}
	if l.Present != 1 || l.Discarded != 1 {
		t.Fatalf("layout = %+v", l)
	}
}

func TestReadVHDXRefusals(t *testing.T) {
	diff := testVHDX{blockSize: 32 << 20, sectorSize: 512, virtualSize: 64 << 20, flags: vhdxHasParent}
	if _, err := readVHDX(diff.write(t)); err == nil || !strings.Contains(err.Error(), "differencing") {
		t.Errorf("a differencing disk: %v", err)
	}
	notVHDX := filepath.Join(t.TempDir(), "data.vhdx")
	if err := os.WriteFile(notVHDX, make([]byte, 3<<20), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := readVHDX(notVHDX); err == nil || !strings.Contains(err.Error(), "not a VHDX") {
		t.Errorf("zeros: %v", err)
	}
	if _, err := readVHDX(filepath.Join(t.TempDir(), "missing.vhdx")); err == nil {
		t.Error("a missing file was read")
	}
}

// A real disk, when one is named: KIVALI_TEST_VHDX=<path> go test -run
// RealVHDX -v. Prints what readVHDX sees, to compare with Get-VHD and
// with what Optimize-VHD gives back.
func TestReadRealVHDX(t *testing.T) {
	p := os.Getenv("KIVALI_TEST_VHDX")
	if p == "" {
		t.Skip("KIVALI_TEST_VHDX not set")
	}
	l, err := readVHDX(p)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("%s: file %d bytes, block %d, virtual %d, blocks %d, present %d, discarded %d (%d bytes)",
		p, l.FileSize, l.BlockSize, l.VirtualSize, l.Blocks, l.Present, l.Discarded, l.DiscardedBytes())
}
