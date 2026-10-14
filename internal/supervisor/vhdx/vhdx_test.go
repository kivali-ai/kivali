package vhdx

import (
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"runtime"
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

// sparseFile is a file of size bytes that begins with b and is zeros
// after it, as a sparse file the caller made would read.
type sparseFile struct {
	b    []byte
	size int64
}

func (s sparseFile) ReadAt(p []byte, off int64) (int, error) {
	if off < 0 || off >= s.size {
		return 0, io.EOF
	}
	n := len(p)
	if int64(n) > s.size-off {
		n = int(s.size - off)
	}
	clear(p[:n])
	if off < int64(len(s.b)) {
		copy(p[:n], s.b[off:])
	}
	if n < len(p) {
		return n, io.EOF
	}
	return n, nil
}

// allocated is the bytes the heap gave out while fn ran.
func allocated(fn func()) uint64 {
	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	fn()
	runtime.ReadMemStats(&after)
	return after.TotalAlloc - before.TotalAlloc
}

// A virtual size over MaxVirtualSize is refused before the table is
// read: a file the caller made, sparse and a few GiB long, cannot make
// the reader allocate its BAT region's length.
func TestReadVHDXRefusesAVirtualSizeOverTheLimit(t *testing.T) {
	for _, vs := range []uint64{MaxVirtualSize + 1<<20, 400 << 40, 1 << 50, 1<<64 - 1<<20} {
		v := Spec{BlockSize: 1 << 20, SectorSize: 512, VirtualSize: vs, BATLength: 1<<32 - 1<<20}
		f := sparseFile{b: v.Bytes(), size: 2<<20 + 1<<32}
		var err error
		if n := allocated(func() { _, err = Read(f, f.size) }); n > 1<<20 {
			t.Errorf("virtual size %d: Read allocated %d bytes", vs, n)
		}
		if err == nil || !strings.Contains(err.Error(), "virtual disk size") {
			t.Errorf("virtual size %d: %v, want a refusal", vs, err)
		}
	}
}

// The largest disk Read reads, in the smallest blocks, has 32 MiB of
// table: Read reads it a chunk at a time and allocates little.
func TestReadVHDXLargestDiskAllocatesLittle(t *testing.T) {
	// 1 MiB blocks and 512-byte sectors: a chunk is 4096 payload
	// entries, so 4 Mi blocks take 1023 sector bitmap entries too.
	const blocks = MaxVirtualSize >> 20
	const tableBytes = (blocks + (blocks-1)/4096) * vhdxBATEntrySize
	v := Spec{BlockSize: 1 << 20, SectorSize: 512, VirtualSize: MaxVirtualSize, BATLength: tableBytes,
		Entries: [][2]uint64{{BlockFullyPresent, 3}, {BlockUnmapped, 4}}}
	f := sparseFile{b: v.Bytes(), size: 2<<20 + tableBytes}
	var l Layout
	var err error
	if n := allocated(func() { l, err = Read(f, f.size) }); n > 4<<20 {
		t.Errorf("Read allocated %d bytes for a table of %d", n, tableBytes)
	}
	if err != nil || l.Blocks != blocks || l.Present != 1 || l.Discarded != 1 {
		t.Fatalf("layout = %+v, err = %v", l, err)
	}
	// One entry short of the table is refused.
	v.BATLength = tableBytes - vhdxBATEntrySize
	f = sparseFile{b: v.Bytes(), size: 2<<20 + tableBytes}
	if _, err := Read(f, f.size); err == nil || !strings.Contains(err.Error(), "too short") {
		t.Errorf("a table one entry short: %v", err)
	}
}

// The table read a chunk at a time counts as the table read whole, with
// the chunks ending anywhere against the sector bitmap entries.
func TestReadVHDXInChunks(t *testing.T) {
	v := Spec{BlockSize: 256 << 20, SectorSize: 512, VirtualSize: 40 * 256 << 20}
	for n := range 40 {
		switch n % 4 {
		case 0, 1:
			v.Entries = append(v.Entries, [2]uint64{BlockFullyPresent, uint64(3 + n*256)})
		case 2:
			v.Entries = append(v.Entries, [2]uint64{BlockUnmapped, uint64(3 + n*256)})
		default:
			v.Entries = append(v.Entries, [2]uint64{BlockNotPresent, 0})
		}
	}
	b := v.Bytes()
	defer func(n int) { batChunkEntries = n }(batChunkEntries)
	for _, chunk := range []int{1, 2, 3, 15, 16, 17, 18, 41, 42, 1 << 17} {
		batChunkEntries = chunk
		l, err := Read(bytes.NewReader(b), int64(len(b)))
		if err != nil || l.Blocks != 40 || l.Present != 20 || l.Discarded != 10 {
			t.Errorf("chunks of %d entries: layout = %+v, err = %v", chunk, l, err)
		}
	}
}

// ParentPath reads a differencing disk's absolute_win32_path from its
// locator, whatever else the locator holds and in whatever order, and
// drops a \\?\ prefix.
func TestParentPath(t *testing.T) {
	for _, tc := range []struct {
		locator [][2]string
		want    string
	}{
		{[][2]string{
			{"parent_linkage", "{83a2c1e0-5b1c-4c9e-9f2a-0d1e2f3a4b5c}"},
			{"relative_path", `..\..\Program Files\Kivali\vm\root.vhdx`},
			{"volume_path", `\\?\Volume{0b2c3d4e-0000-0000-0000-100000000000}\Program Files\Kivali\vm\root.vhdx`},
			{"absolute_win32_path", `C:\Program Files\Kivali\vm\root.vhdx`},
		}, `C:\Program Files\Kivali\vm\root.vhdx`},
		{[][2]string{{"absolute_win32_path", `\\?\C:\Users\Public\root.vhdx`}}, `C:\Users\Public\root.vhdx`},
		{[][2]string{{"absolute_win32_path", `\\attacker\share\p.vhdx`}}, `\\attacker\share\p.vhdx`},
	} {
		b := Spec{BlockSize: 2 << 20, SectorSize: 512, VirtualSize: 64 << 20, Flags: FlagHasParent, ParentLocator: tc.locator}.Bytes()
		got, err := ParentPath(bytes.NewReader(b), int64(len(b)))
		if err != nil || got != tc.want {
			t.Errorf("ParentPath = %q, %v; want %q", got, err, tc.want)
		}
	}
}

func TestParentPathRefusals(t *testing.T) {
	base := Spec{BlockSize: 2 << 20, SectorSize: 512, VirtualSize: 64 << 20, Flags: FlagHasParent}
	noParent := Spec{BlockSize: 2 << 20, SectorSize: 512, VirtualSize: 64 << 20,
		ParentLocator: [][2]string{{"absolute_win32_path", `C:\root.vhdx`}}}
	noAbsolute := base
	noAbsolute.ParentLocator = [][2]string{{"relative_path", `..\root.vhdx`}}
	twice := base
	twice.ParentLocator = [][2]string{{"absolute_win32_path", `C:\a.vhdx`}, {"absolute_win32_path", `C:\b.vhdx`}}
	for _, tc := range []struct {
		what string
		b    []byte
		want string
	}{
		{"no parent", noParent.Bytes(), "not a differencing disk"},
		{"no locator", base.Bytes(), "parent locator"},
		{"no absolute path", noAbsolute.Bytes(), "absolute_win32_path"},
		{"a key twice", twice.Bytes(), "twice"},
		{"not a VHDX", make([]byte, 3<<20), "not a VHDX"},
	} {
		if _, err := ParentPath(bytes.NewReader(tc.b), int64(len(tc.b))); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: err = %v, want %q", tc.what, err, tc.want)
		}
	}
}

// A locator whose entries point outside it, or that claims more
// entries than it holds, is refused rather than read past.
func TestParseParentLocatorBounds(t *testing.T) {
	good := parentLocator([][2]string{{"absolute_win32_path", `C:\root.vhdx`}})
	if m, err := parseParentLocator(good); err != nil || m["absolute_win32_path"] != `C:\root.vhdx` {
		t.Fatalf("parse = %v, %v", m, err)
	}
	for _, tc := range []struct {
		what  string
		patch func(b []byte)
	}{
		{"key past the end", func(b []byte) { b[vhdxParentLocatorHeaderSize] = 0xff; b[vhdxParentLocatorHeaderSize+1] = 0xff }},
		{"odd value length", func(b []byte) { b[vhdxParentLocatorHeaderSize+10]-- }},
		{"too many entries", func(b []byte) { b[18] = 0xff }},
		{"wrong type", func(b []byte) { b[0]++ }},
	} {
		b := append([]byte(nil), good...)
		tc.patch(b)
		if _, err := parseParentLocator(b); err == nil {
			t.Errorf("%s: accepted", tc.what)
		}
	}
	if _, err := parseParentLocator(good[:10]); err == nil {
		t.Error("a truncated header was accepted")
	}
}
