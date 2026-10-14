//go:build windows

package broker

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"math/rand/v2"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"testing"

	"github.com/Microsoft/go-winio/vhd"
	"golang.org/x/sys/windows"

	"github.com/kivali-ai/kivali/internal/supervisor/vhdx"
)

// These compact a dynamic VHDX that holds data, as the data disk does
// after the guest's fstrim: the disk is attached (no drive letter, no
// volume), blocks are written to it raw, whole blocks in the middle are
// discarded (a trim of each, which vhdmp records as an unmapped block
// that keeps its place in the file), and the disk is detached. Then the
// broker's compaction runs on it, and the blocks still written are read
// back through a read-only attach and checked against what was
// written. Like the rest of virtdisk_windows_test.go they need an
// elevated test process, and fail without one under
// KIVALI_REQUIRE_ELEVATED.

// testBlock is the size of a test disk's blocks: the data disk's.
const testBlock = 32 << 20

// createDynamicVHDX makes a dynamic VHDX at p of size bytes, in blocks
// of testBlock.
func createDynamicVHDX(t *testing.T, p string, size uint64) {
	t.Helper()
	params := vhd.CreateVirtualDiskParameters{Version: 2, Version2: vhd.CreateVersion2{
		MaximumSize:      size,
		BlockSizeInBytes: testBlock,
	}}
	h, err := vhd.CreateVirtualDisk(p, vhd.VirtualDiskAccessNone, vhd.CreateVirtualDiskFlagNone, &params)
	if err != nil {
		t.Fatalf("create %s: %v", p, err)
	}
	if err := syscall.CloseHandle(h); err != nil {
		t.Fatal(err)
	}
}

// attach attaches the VHDX at p with no drive letter, read-only or not,
// and returns its disk device (\\.\PhysicalDriveN) and a function that
// detaches it; the test's cleanup detaches it too.
func attach(t *testing.T, p string, readOnly bool) (string, func()) {
	t.Helper()
	h, err := vhd.OpenVirtualDiskWithParameters(p, vhd.VirtualDiskAccessNone, vhd.OpenVirtualDiskFlagNone,
		&vhd.OpenVirtualDiskParameters{Version: 2, Version2: vhd.OpenVersion2{ReadOnly: readOnly}})
	if err != nil {
		t.Fatalf("open %s: %v", p, err)
	}
	flags := vhd.AttachVirtualDiskFlagNoDriveLetter
	if readOnly {
		flags |= vhd.AttachVirtualDiskFlagReadOnly
	}
	if err := vhd.AttachVirtualDisk(h, flags, &vhd.AttachVirtualDiskParameters{Version: 2}); err != nil {
		_ = syscall.CloseHandle(h)
		t.Fatalf("attach %s: %v", p, err)
	}
	var once sync.Once
	detach := func() {
		once.Do(func() {
			_ = vhd.DetachVirtualDisk(h)
			_ = syscall.CloseHandle(h)
		})
	}
	t.Cleanup(detach)
	dev, err := vhd.GetVirtualDiskPhysicalPath(h)
	if err != nil {
		detach()
		t.Fatalf("the attached disk's device: %v", err)
	}
	return dev, detach
}

// blockData is what block n of a test disk holds: bytes of its own,
// never zeros.
func blockData(n int) []byte {
	var seed [32]byte
	binary.LittleEndian.PutUint64(seed[:], uint64(n)+1)
	b := make([]byte, testBlock)
	_, _ = rand.NewChaCha8(seed).Read(b)
	return b
}

// ioctlStorageManageDataSetAttributes is
// IOCTL_STORAGE_MANAGE_DATA_SET_ATTRIBUTES.
const ioctlStorageManageDataSetAttributes = 0x2d9404

// trim discards length bytes at offset of the disk open as f
// (DeviceDsmAction_Trim): DEVICE_MANAGE_DATA_SET_ATTRIBUTES (seven
// DWORDs, 28 bytes), then one DEVICE_DATA_SET_RANGE at offset 32.
func trim(f *os.File, offset, length int64) error {
	in := make([]byte, 48)
	binary.LittleEndian.PutUint32(in[0:], 28) // Size
	binary.LittleEndian.PutUint32(in[4:], 1)  // Action: DeviceDsmAction_Trim
	binary.LittleEndian.PutUint32(in[20:], 32)
	binary.LittleEndian.PutUint32(in[24:], 16)
	binary.LittleEndian.PutUint64(in[32:], uint64(offset))
	binary.LittleEndian.PutUint64(in[40:], uint64(length))
	var n uint32
	return windows.DeviceIoControl(windows.Handle(f.Fd()), ioctlStorageManageDataSetAttributes, &in[0], uint32(len(in)), nil, 0, &n, nil)
}

// dataDisk makes a dynamic VHDX at p of size bytes holding blockData in
// each block of written, then discards each block of discarded, and
// returns the checksum of each block that still holds data and how many
// blocks are present (those, and any Windows wrote itself). It checks,
// through package vhdx, that each discarded block kept its place in the
// file: the space a compaction gives back.
func dataDisk(t *testing.T, p string, size uint64, written, discarded []int) (map[int][32]byte, int) {
	t.Helper()
	createDynamicVHDX(t, p, size)
	dev, detach := attach(t, p, false)
	f, err := os.OpenFile(dev, os.O_RDWR, 0)
	if err != nil {
		t.Fatalf("open %s: %v", dev, err)
	}
	sums := map[int][32]byte{}
	for _, n := range written {
		b := blockData(n)
		if _, err := f.WriteAt(b, int64(n)*testBlock); err != nil {
			_ = f.Close()
			t.Fatalf("write block %d to %s: %v", n, dev, err)
		}
		sums[n] = sha256.Sum256(b)
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		t.Fatalf("flush %s: %v", dev, err)
	}
	for _, n := range discarded {
		if err := trim(f, int64(n)*testBlock, testBlock); err != nil {
			_ = f.Close()
			t.Fatalf("discard block %d of %s: %v", n, dev, err)
		}
		delete(sums, n)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	detach()
	l, err := vhdx.ReadFile(p)
	if err != nil {
		t.Fatalf("read the disk's block table: %v", err)
	}
	if l.Discarded < len(discarded) || l.Present < len(sums) {
		t.Fatalf("after the writes and discards: %d blocks present, %d discarded keeping their space; want %d and %d",
			l.Present, l.Discarded, len(sums), len(discarded))
	}
	return sums, l.Present
}

// checkBlocks reads each block of sums back from the VHDX at p, attached
// read-only, and compares it with what was written.
func checkBlocks(t *testing.T, p string, sums map[int][32]byte) {
	t.Helper()
	dev, detach := attach(t, p, true)
	defer detach()
	f, err := os.Open(dev)
	if err != nil {
		t.Fatalf("open %s: %v", dev, err)
	}
	defer func() { _ = f.Close() }()
	b := make([]byte, testBlock)
	for n, want := range sums {
		if _, err := f.ReadAt(b, int64(n)*testBlock); err != nil {
			t.Fatalf("read block %d: %v", n, err)
		}
		if sha256.Sum256(b) != want {
			t.Errorf("block %d changed", n)
		}
	}
}

// openable checks that the VHDX at p opens as the broker opens it (no
// parents, its log replayed) and is still a dynamic disk.
func openable(t *testing.T, p string) {
	t.Helper()
	h, err := openVHDXNoParents(p, false)
	if err != nil {
		t.Fatalf("open %s as a VHDX: %v", p, err)
	}
	defer func() { _ = windows.CloseHandle(h) }()
	if err := checkDynamicVHDX(h); err != nil {
		t.Fatalf("%s: %v", p, err)
	}
}

// meterProbe is a Meter whose attached hook records that the compaction
// went asynchronous (ERROR_IO_PENDING, its reader attached) and what its
// progress read then, and then calls then, if set.
type meterProbe struct {
	Meter
	pending             bool
	seenDone, seenTotal int64
}

func newMeterProbe(then func()) *meterProbe {
	m := &meterProbe{}
	m.attached = func() {
		m.pending = true
		m.seenDone, m.seenTotal = m.Get()
		if then != nil {
			then()
		}
	}
	return m
}

func (m *meterProbe) detached(t *testing.T) {
	t.Helper()
	m.mu.Lock()
	attached := m.read != nil
	m.mu.Unlock()
	if attached {
		t.Fatal("the compaction left its progress reader on the meter")
	}
}

// Discarded blocks in the middle of a dynamic VHDX that holds data are
// given back: the compaction runs asynchronously (its progress read
// through the meter while it runs), the file shrinks by the discarded
// blocks, none is left, and every block still written reads back as it
// was written.
func TestCompactGivesBackDiscardedBlocks(t *testing.T) {
	skipUnlessElevated(t)
	p := filepath.Join(canonical(t, t.TempDir()), DataDiskName)
	written := []int{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12}
	discarded := []int{2, 4, 6, 8, 10}
	sums, present := dataDisk(t, p, 1<<30, written, discarded)

	m := newMeterProbe(nil)
	before, after, err := winFiles{}.CompactVHDX(context.Background(), p, &m.Meter)
	if err != nil {
		t.Fatalf("CompactVHDX: %v", err)
	}
	if !m.pending {
		t.Fatal("CompactVirtualDisk answered at once: the wait, the progress reader and the cancel were not used")
	}
	if m.seenDone < 0 || m.seenTotal < 0 || m.seenDone > m.seenTotal {
		t.Errorf("progress read while it ran: %d of %d", m.seenDone, m.seenTotal)
	}
	m.detached(t)
	if before-after < int64(len(discarded)-1)*testBlock {
		t.Fatalf("the file went from %d to %d bytes; want about %d fewer", before, after, int64(len(discarded))*testBlock)
	}
	l, err := vhdx.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	if l.Discarded != 0 || l.Present != present {
		t.Fatalf("after the compaction: %d blocks present, %d discarded; want %d and 0", l.Present, l.Discarded, present)
	}
	openable(t, p)
	checkBlocks(t, p, sums)
}

// A compaction cancelled while it runs (cancelled from the meter as its
// reader is attached, so CancelIoEx reaches the operation in flight)
// leaves a disk that opens as a dynamic VHDX, with every block still
// written as it was; a compaction after it gives everything back. The
// first may have finished before the cancel reached it: either way the
// disk must be whole.
func TestCompactCancelledWhileItRuns(t *testing.T) {
	skipUnlessElevated(t)
	p := filepath.Join(canonical(t, t.TempDir()), DataDiskName)
	var written, discarded []int
	for n := 1; n <= 40; n++ {
		written = append(written, n)
		if n%2 == 1 {
			discarded = append(discarded, n)
		}
	}
	sums, present := dataDisk(t, p, 2<<30, written, discarded)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	m := newMeterProbe(cancel)
	_, _, err := winFiles{}.CompactVHDX(ctx, p, &m.Meter)
	if !m.pending {
		t.Fatal("CompactVirtualDisk answered at once: there was nothing in flight to cancel")
	}
	m.detached(t)
	switch {
	case errors.Is(err, context.Canceled):
		t.Logf("the compaction was cut short (progress %d of %d as it was cancelled)", m.seenDone, m.seenTotal)
	case err == nil:
		t.Log("the compaction finished before the cancel reached it")
	default:
		t.Fatalf("CompactVHDX: %v, want it cancelled or finished", err)
	}
	openable(t, p)
	checkBlocks(t, p, sums)
	if _, err := vhdx.ReadFile(p); err != nil {
		t.Fatalf("read the block table after the cancel: %v", err)
	}

	before, after, err := winFiles{}.CompactVHDX(context.Background(), p, nil)
	if err != nil {
		t.Fatalf("the compaction after the cancel: %v", err)
	}
	l, err := vhdx.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	if l.Discarded != 0 || l.Present != present || after > before {
		t.Fatalf("after the second compaction: %d present, %d discarded, %d to %d bytes", l.Present, l.Discarded, before, after)
	}
	openable(t, p)
	checkBlocks(t, p, sums)
}
