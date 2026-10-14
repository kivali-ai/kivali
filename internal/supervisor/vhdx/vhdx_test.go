package vhdx

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// write writes v's bytes to data.vhdx in a test directory.
func write(t *testing.T, v Spec) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "data.vhdx")
	if err := os.WriteFile(p, v.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// A block the guest discarded keeps its place in the file until the disk
// is compacted; one that never had a place, or whose place the host has
// dropped, costs nothing. The sector bitmap entry after each chunk of
// payload entries is skipped.
func TestReadVHDXCountsDiscardedBlocks(t *testing.T) {
	// 256 MiB blocks with 512-byte sectors: a chunk is 16 payload
	// entries, so 20 blocks put one sector bitmap entry in the middle.
	v := Spec{BlockSize: 256 << 20, SectorSize: 512, VirtualSize: 20 * 256 << 20}
	for n := range 20 {
		switch {
		case n < 10:
			v.Entries = append(v.Entries, [2]uint64{BlockFullyPresent, uint64(3 + n*256)})
		case n < 14:
			v.Entries = append(v.Entries, [2]uint64{BlockUnmapped, uint64(3 + n*256)}) // discarded, space kept
		case n < 16:
			v.Entries = append(v.Entries, [2]uint64{BlockUnmapped, 0}) // discarded, space dropped
		case n == 16:
			v.Entries = append(v.Entries, [2]uint64{BlockZero, 0})
		default:
			v.Entries = append(v.Entries, [2]uint64{BlockNotPresent, 0})
		}
	}
	l, err := ReadFile(write(t, v))
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
	v := Spec{BlockSize: 32 << 20, SectorSize: 512, VirtualSize: 100 << 20,
		Entries: [][2]uint64{{BlockFullyPresent, 3}, {BlockFullyPresent, 35}, {BlockUnmapped, 67}, {BlockNotPresent, 0}}}
	l, err := ReadFile(write(t, v))
	if err != nil {
		t.Fatal(err)
	}
	if l.Blocks != 4 || l.Present != 2 || l.Discarded != 1 {
		t.Fatalf("layout = %+v", l)
	}
}

// A fixed disk keeps every block's space by design: nothing to give back.
func TestReadVHDXFixedDiskHasNothingDiscarded(t *testing.T) {
	v := Spec{BlockSize: 32 << 20, SectorSize: 512, VirtualSize: 64 << 20, Flags: FlagLeaveBlocksAllocated,
		Entries: [][2]uint64{{BlockUnmapped, 3}, {BlockUnmapped, 35}}}
	l, err := ReadFile(write(t, v))
	if err != nil {
		t.Fatal(err)
	}
	if !l.Fixed || l.Discarded != 0 {
		t.Fatalf("layout = %+v", l)
	}
}

func TestReadVHDXSecondRegionTableCopy(t *testing.T) {
	v := Spec{BlockSize: 32 << 20, SectorSize: 512, VirtualSize: 64 << 20, BreakFirstRegionTable: true,
		Entries: [][2]uint64{{BlockFullyPresent, 3}, {BlockUnmapped, 35}}}
	l, err := ReadFile(write(t, v))
	if err != nil {
		t.Fatal(err)
	}
	if l.Present != 1 || l.Discarded != 1 {
		t.Fatalf("layout = %+v", l)
	}
}

func TestReadVHDXRefusals(t *testing.T) {
	diff := Spec{BlockSize: 32 << 20, SectorSize: 512, VirtualSize: 64 << 20, Flags: FlagHasParent}
	if _, err := ReadFile(write(t, diff)); err == nil || !strings.Contains(err.Error(), "differencing") {
		t.Errorf("a differencing disk: %v", err)
	}
	notVHDX := filepath.Join(t.TempDir(), "data.vhdx")
	if err := os.WriteFile(notVHDX, make([]byte, 3<<20), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadFile(notVHDX); err == nil || !strings.Contains(err.Error(), "not a VHDX") {
		t.Errorf("zeros: %v", err)
	}
	if _, err := ReadFile(filepath.Join(t.TempDir(), "missing.vhdx")); err == nil {
		t.Error("a missing file was read")
	}
}

// A real disk, when one is named: KIVALI_TEST_VHDX=<path> go test -run
// RealVHDX -v. Prints what ReadFile sees, to compare with Get-VHD and
// with what Optimize-VHD gives back.
func TestReadRealVHDX(t *testing.T) {
	p := os.Getenv("KIVALI_TEST_VHDX")
	if p == "" {
		t.Skip("KIVALI_TEST_VHDX not set")
	}
	l, err := ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("%s: file %d bytes, block %d, virtual %d, blocks %d, present %d, discarded %d (%d bytes)",
		p, l.FileSize, l.BlockSize, l.VirtualSize, l.Blocks, l.Present, l.Discarded, l.DiscardedBytes())
}

// Read reads through any io.ReaderAt (the broker reads through the
// handle that pins the file) and says which refusal it was.
func TestReadReaderAt(t *testing.T) {
	ok := Spec{BlockSize: 32 << 20, SectorSize: 512, VirtualSize: 64 << 20,
		Entries: [][2]uint64{{BlockFullyPresent, 3}, {BlockUnmapped, 35}}}.Bytes()
	l, err := Read(bytes.NewReader(ok), int64(len(ok)))
	if err != nil || l.Present != 1 || l.Discarded != 1 || l.Fixed {
		t.Fatalf("layout = %+v, err = %v", l, err)
	}
	diff := Spec{BlockSize: 32 << 20, SectorSize: 512, VirtualSize: 64 << 20, Flags: FlagHasParent}.Bytes()
	if _, err := Read(bytes.NewReader(diff), int64(len(diff))); !errors.Is(err, ErrDifferencing) {
		t.Errorf("a differencing disk: %v", err)
	}
	if _, err := Read(bytes.NewReader(ok[:100]), 100); err == nil {
		t.Error("a truncated file was read")
	}
}
