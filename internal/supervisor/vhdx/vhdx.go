// Package vhdx reads what a VHDX's metadata says: a dynamic disk's
// blocks, for the Hyper-V backend to decide whether the data disk is
// worth compacting and for the broker to refuse any file but a dynamic
// VHDX with no parent before it compacts one; and the parent a
// differencing disk's locator names, for the broker to compare its own
// differencing child's parent without opening it.
package vhdx

import (
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"os"
	"strings"
	"unicode/utf16"

	"github.com/Microsoft/go-winio/pkg/guid"
)

// The VHDX file format (MS-VHDX), as far as counting a dynamic disk's
// blocks needs it. A dynamic VHDX keeps a block allocation table (BAT)
// with one 8-byte entry per payload block of the virtual disk, giving
// the block's state (its low 3 bits) and where in the file its data is
// (bits 20 to 63, in MiB). A block the guest discards (SCSI UNMAP; the
// guest's fstrim) becomes PAYLOAD_BLOCK_UNMAPPED, but its place in the
// file is kept, for the block to reuse; the file shrinks only when the
// blocks at its end are dropped (Hyper-V does that when the VM closes
// the disk) or when it is compacted.
//
// The region table (two copies, at 192 KiB and 256 KiB) locates the BAT
// and the metadata region; the metadata region holds the block size,
// the virtual disk size and the logical sector size, which together
// give the BAT's layout: after every chunkRatio payload entries comes
// one entry for a sector bitmap block (used by differencing disks). A
// differencing disk's metadata also holds its parent locator: UTF-16
// key-value pairs naming the parent (absolute_win32_path, relative_path,
// parent_linkage, ...).
const (
	vhdxSignature          = "vhdxfile"
	vhdxRegionTableOffset  = 192 << 10
	vhdxRegionTableSize    = 64 << 10
	vhdxRegionSignature    = "regi"
	vhdxRegionEntrySize    = 32
	vhdxMaxRegionEntries   = 2047
	vhdxMetadataSignature  = "metadata"
	vhdxMetadataHeaderSize = 32
	vhdxMetadataEntrySize  = 32
	vhdxMaxMetadataEntries = 2047
	vhdxBATEntrySize       = 8
	vhdxMinBlockSize       = 1 << 20
	vhdxMaxBlockSize       = 256 << 20

	vhdxParentLocatorHeaderSize = 20
	vhdxParentLocatorEntrySize  = 12
	vhdxMaxParentLocatorSize    = 64 << 10
)

// MaxVirtualSize is the largest virtual disk Read reads: the largest the
// broker makes (broker.MaxDiskBytes is this). A file whose metadata
// claims more is refused before its block allocation table is read.
const MaxVirtualSize = 4 << 40

// batChunkEntries is how many BAT entries Read reads at a time (1 MiB of
// table), so what it allocates is bounded whatever the table's length:
// a disk of MaxVirtualSize in 1 MiB blocks has 32 MiB of table.
var batChunkEntries = 1 << 17

// Payload block states (BAT entry bits 0 to 2).
const (
	BlockNotPresent       = 0
	BlockZero             = 2
	BlockUnmapped         = 3
	BlockFullyPresent     = 6
	BlockPartiallyPresent = 7
)

// File parameters flags.
const (
	FlagLeaveBlocksAllocated = 1 << 0 // a fixed disk: every block has its space
	FlagHasParent            = 1 << 1 // a differencing disk
)

// The region and metadata item identifiers, in the byte order the file
// stores them (the Windows GUID layout).
var (
	batRegionID         = windowsGUID("2DC27766-F623-4200-9D64-115E9BFD4A08")
	metadataRegionID    = windowsGUID("8B7CA206-4790-4B9A-B8FE-575F050F886E")
	fileParametersID    = windowsGUID("CAA16737-FA36-4D43-B3B6-33F0AA44E76B")
	virtualDiskSizeID   = windowsGUID("2FA54224-CD1B-4876-B211-5DBED83BF4B8")
	logicalSectorSizeID = windowsGUID("8141BF1D-A96F-4709-BA47-F233A8FAAB5F")
	parentLocatorID     = windowsGUID("A8D35F2D-B30B-454D-ABF7-D3D84834AB0C")
	// The locator type a VHDX parent locator's header starts with.
	parentLocatorTypeVHDX = windowsGUID("B04AEFB7-D19E-4A81-B789-25B8E9445913")
)

func windowsGUID(s string) [16]byte {
	g, err := guid.FromString(s)
	if err != nil {
		panic(err)
	}
	return g.ToWindowsArray()
}

var crc32c = crc32.MakeTable(crc32.Castagnoli)

// ErrDifferencing is Read's answer for a differencing disk.
var ErrDifferencing = errors.New("a differencing disk")

// Layout is what a VHDX says about its blocks.
type Layout struct {
	FileSize    int64
	BlockSize   uint32
	VirtualSize uint64
	// Fixed: a fixed disk (LeaveBlocksAllocated), whose blocks all keep
	// their space; nothing of it is ever discarded.
	Fixed bool
	// Blocks is the number of payload blocks; Present how many hold the
	// guest's data; Discarded how many the guest has discarded that
	// still keep their space in the file, which compaction gives back.
	Blocks, Present, Discarded int
}

// DiscardedBytes is the space compaction gives back.
func (l Layout) DiscardedBytes() int64 { return int64(l.Discarded) * int64(l.BlockSize) }

type vhdxRegion struct {
	offset uint64
	length uint32
}

// ReadFile reads the VHDX at path with Read.
func ReadFile(path string) (Layout, error) {
	f, err := os.Open(path)
	if err != nil {
		return Layout{}, err
	}
	defer func() { _ = f.Close() }()
	fi, err := f.Stat()
	if err != nil {
		return Layout{}, err
	}
	l, err := Read(f, fi.Size())
	if err != nil {
		return l, fmt.Errorf("%s: %w", path, err)
	}
	return l, nil
}

// Read reads the VHDX in f, size bytes long, a dynamic or fixed disk
// with no parent, and counts its blocks; a differencing disk (its file
// parameters' HasParent flag set) is an error, and its parent locator is
// never read, and so is a virtual size over MaxVirtualSize. It reads
// only the region table, the metadata and the BAT (a few dozen KiB for
// the data disk, the BAT a chunk at a time).
func Read(f io.ReaderAt, size int64) (Layout, error) {
	l := Layout{FileSize: size}
	var sig [8]byte
	if _, err := f.ReadAt(sig[:], 0); err != nil || string(sig[:]) != vhdxSignature {
		return l, errors.New("not a VHDX file")
	}
	regions, err := readRegionTable(f, l.FileSize)
	if err != nil {
		return l, err
	}
	meta, ok := regions[metadataRegionID]
	if !ok {
		return l, errors.New("no metadata region")
	}
	bat, ok := regions[batRegionID]
	if !ok {
		return l, errors.New("no block allocation table")
	}
	var flags, sectorSize uint32
	l.BlockSize, flags, l.VirtualSize, sectorSize, err = readMetadata(f, meta)
	if err != nil {
		return l, err
	}
	if flags&FlagHasParent != 0 {
		return l, ErrDifferencing
	}
	l.Fixed = flags&FlagLeaveBlocksAllocated != 0
	if l.VirtualSize > MaxVirtualSize {
		return l, fmt.Errorf("the virtual disk size %d is over %d", l.VirtualSize, uint64(MaxVirtualSize))
	}
	l.Blocks = int((l.VirtualSize + uint64(l.BlockSize) - 1) / uint64(l.BlockSize))
	// After every chunkRatio payload entries, one sector bitmap entry.
	chunkRatio := int((uint64(1) << 23) * uint64(sectorSize) / uint64(l.BlockSize))
	if chunkRatio < 1 {
		return l, fmt.Errorf("block size %d and sector size %d make no chunk", l.BlockSize, sectorSize)
	}
	entries := l.Blocks + (l.Blocks-1)/chunkRatio
	if uint64(entries)*vhdxBATEntrySize > uint64(bat.length) {
		return l, fmt.Errorf("the block allocation table (%d bytes) is too short for %d blocks", bat.length, l.Blocks)
	}
	buf := make([]byte, min(entries, batChunkEntries)*vhdxBATEntrySize)
	for start := 0; start < entries; {
		table := buf[:min(entries-start, batChunkEntries)*vhdxBATEntrySize]
		if _, err := f.ReadAt(table, int64(bat.offset)+int64(start)*vhdxBATEntrySize); err != nil {
			return l, fmt.Errorf("read the block allocation table: %w", err)
		}
		for j := 0; j < len(table); j += vhdxBATEntrySize {
			// Entry i is the sector bitmap entry that follows each
			// chunkRatio payload entries when i+1 is a multiple of
			// chunkRatio+1.
			if i := start + j/vhdxBATEntrySize; (i+1)%(chunkRatio+1) == 0 {
				continue
			}
			e := binary.LittleEndian.Uint64(table[j:])
			state, offsetMB := e&7, e>>20
			switch {
			case state == BlockFullyPresent || state == BlockPartiallyPresent:
				l.Present++
			case offsetMB != 0 && !l.Fixed:
				l.Discarded++
			}
		}
		start += len(table) / vhdxBATEntrySize
	}
	return l, nil
}

// readRegionTable reads the first region table whose checksum holds
// (the file keeps two copies) and returns its regions by identifier.
func readRegionTable(f io.ReaderAt, fileSize int64) (map[[16]byte]vhdxRegion, error) {
	var last error
	for _, off := range []int64{vhdxRegionTableOffset, vhdxRegionTableOffset + vhdxRegionTableSize} {
		regions, err := parseRegionTable(f, off, fileSize)
		if err == nil {
			return regions, nil
		}
		last = err
	}
	return nil, last
}

func parseRegionTable(f io.ReaderAt, off, fileSize int64) (map[[16]byte]vhdxRegion, error) {
	table := make([]byte, vhdxRegionTableSize)
	if _, err := f.ReadAt(table, off); err != nil {
		return nil, fmt.Errorf("read the region table: %w", err)
	}
	if string(table[:4]) != vhdxRegionSignature {
		return nil, errors.New("the region table has no signature")
	}
	// The checksum is CRC-32C of the table with its own field zeroed.
	want := binary.LittleEndian.Uint32(table[4:])
	binary.LittleEndian.PutUint32(table[4:], 0)
	if crc32.Checksum(table, crc32c) != want {
		return nil, errors.New("the region table's checksum does not match")
	}
	n := int(binary.LittleEndian.Uint32(table[8:]))
	if n > vhdxMaxRegionEntries {
		return nil, fmt.Errorf("the region table claims %d entries", n)
	}
	regions := make(map[[16]byte]vhdxRegion, n)
	for i := range n {
		e := table[16+i*vhdxRegionEntrySize:]
		var id [16]byte
		copy(id[:], e[:16])
		r := vhdxRegion{offset: binary.LittleEndian.Uint64(e[16:]), length: binary.LittleEndian.Uint32(e[24:])}
		if r.offset > uint64(fileSize) || uint64(r.length) > uint64(fileSize)-r.offset {
			return nil, fmt.Errorf("region %d lies outside the file", i)
		}
		regions[id] = r
	}
	return regions, nil
}

// metadataTable is the metadata region's table of items.
type metadataTable struct {
	f       io.ReaderAt
	region  vhdxRegion
	entries []byte
	n       int
}

func readMetadataTable(f io.ReaderAt, meta vhdxRegion) (metadataTable, error) {
	hdr := make([]byte, vhdxMetadataHeaderSize)
	if _, err := f.ReadAt(hdr, int64(meta.offset)); err != nil {
		return metadataTable{}, fmt.Errorf("read the metadata table: %w", err)
	}
	if string(hdr[:8]) != vhdxMetadataSignature {
		return metadataTable{}, errors.New("the metadata table has no signature")
	}
	n := int(binary.LittleEndian.Uint16(hdr[10:]))
	if n > vhdxMaxMetadataEntries {
		return metadataTable{}, fmt.Errorf("the metadata table claims %d entries", n)
	}
	entries := make([]byte, n*vhdxMetadataEntrySize)
	if _, err := f.ReadAt(entries, int64(meta.offset)+vhdxMetadataHeaderSize); err != nil {
		return metadataTable{}, fmt.Errorf("read the metadata entries: %w", err)
	}
	return metadataTable{f: f, region: meta, entries: entries, n: n}, nil
}

// item reads the metadata item id: its first size bytes or, with size
// 0, all of it, refusing one longer than limit. An item shorter than
// size, or lying outside the region, is an error.
func (m metadataTable) item(id [16]byte, size, limit int) ([]byte, error) {
	for i := range m.n {
		e := m.entries[i*vhdxMetadataEntrySize:]
		if [16]byte(e[:16]) != id {
			continue
		}
		off, length := binary.LittleEndian.Uint32(e[16:]), binary.LittleEndian.Uint32(e[20:])
		if int64(length) < int64(size) || uint64(off) > uint64(m.region.length) || uint64(length) > uint64(m.region.length)-uint64(off) {
			return nil, fmt.Errorf("metadata item %d lies outside its region", i)
		}
		if size == 0 {
			if int64(length) > int64(limit) {
				return nil, fmt.Errorf("metadata item %d is %d bytes long", i, length)
			}
			size = int(length)
		}
		b := make([]byte, size)
		if _, err := m.f.ReadAt(b, int64(m.region.offset)+int64(off)); err != nil {
			return nil, fmt.Errorf("read metadata item %d: %w", i, err)
		}
		return b, nil
	}
	return nil, errors.New("a required metadata item is missing")
}

// readMetadata reads the file parameters (block size and flags), the
// virtual disk size and the logical sector size from the metadata
// region.
func readMetadata(f io.ReaderAt, meta vhdxRegion) (blockSize, flags uint32, virtualSize uint64, sectorSize uint32, err error) {
	m, err := readMetadataTable(f, meta)
	if err != nil {
		return 0, 0, 0, 0, err
	}
	fp, err := m.item(fileParametersID, 8, 0)
	if err != nil {
		return 0, 0, 0, 0, err
	}
	blockSize, flags = binary.LittleEndian.Uint32(fp), binary.LittleEndian.Uint32(fp[4:])
	vs, err := m.item(virtualDiskSizeID, 8, 0)
	if err != nil {
		return 0, 0, 0, 0, err
	}
	virtualSize = binary.LittleEndian.Uint64(vs)
	ss, err := m.item(logicalSectorSizeID, 4, 0)
	if err != nil {
		return 0, 0, 0, 0, err
	}
	sectorSize = binary.LittleEndian.Uint32(ss)
	if blockSize < vhdxMinBlockSize || blockSize > vhdxMaxBlockSize || blockSize&(blockSize-1) != 0 {
		return 0, 0, 0, 0, fmt.Errorf("the block size %d is not a power of two between 1 MiB and 256 MiB", blockSize)
	}
	if sectorSize != 512 && sectorSize != 4096 {
		return 0, 0, 0, 0, fmt.Errorf("the logical sector size %d is neither 512 nor 4096", sectorSize)
	}
	if virtualSize == 0 {
		return 0, 0, 0, 0, errors.New("the virtual disk size is 0")
	}
	return blockSize, flags, virtualSize, sectorSize, nil
}

// ParentPath reads the parent locator of the differencing VHDX in f,
// size bytes long, and returns the path its absolute_win32_path entry
// names, without a \\?\ prefix. It parses the locator's bytes and opens
// nothing: the parent itself is never touched. A disk with no parent is
// an error.
func ParentPath(f io.ReaderAt, size int64) (string, error) {
	var sig [8]byte
	if _, err := f.ReadAt(sig[:], 0); err != nil || string(sig[:]) != vhdxSignature {
		return "", errors.New("not a VHDX file")
	}
	regions, err := readRegionTable(f, size)
	if err != nil {
		return "", err
	}
	meta, ok := regions[metadataRegionID]
	if !ok {
		return "", errors.New("no metadata region")
	}
	m, err := readMetadataTable(f, meta)
	if err != nil {
		return "", err
	}
	fp, err := m.item(fileParametersID, 8, 0)
	if err != nil {
		return "", err
	}
	if binary.LittleEndian.Uint32(fp[4:])&FlagHasParent == 0 {
		return "", errors.New("not a differencing disk")
	}
	loc, err := m.item(parentLocatorID, 0, vhdxMaxParentLocatorSize)
	if err != nil {
		return "", fmt.Errorf("the parent locator: %w", err)
	}
	entries, err := parseParentLocator(loc)
	if err != nil {
		return "", err
	}
	p := entries["absolute_win32_path"]
	if p == "" {
		return "", errors.New("the parent locator has no absolute_win32_path")
	}
	return strings.TrimPrefix(p, `\\?\`), nil
}

// parseParentLocator reads a VHDX parent locator's key-value entries:
// a header (the locator type, 2 reserved bytes, the entry count), then
// per entry the key's and the value's offset (from the locator's start)
// and their lengths in bytes, the strings UTF-16LE.
func parseParentLocator(loc []byte) (map[string]string, error) {
	if len(loc) < vhdxParentLocatorHeaderSize {
		return nil, errors.New("the parent locator is shorter than its header")
	}
	if [16]byte(loc[:16]) != parentLocatorTypeVHDX {
		return nil, errors.New("the parent locator is not of the VHDX type")
	}
	n := int(binary.LittleEndian.Uint16(loc[18:]))
	if vhdxParentLocatorHeaderSize+n*vhdxParentLocatorEntrySize > len(loc) {
		return nil, fmt.Errorf("the parent locator claims %d entries", n)
	}
	str := func(off uint32, length uint16) (string, error) {
		if length%2 != 0 || uint64(off)+uint64(length) > uint64(len(loc)) {
			return "", errors.New("a parent locator entry lies outside the locator")
		}
		u := make([]uint16, length/2)
		for i := range u {
			u[i] = binary.LittleEndian.Uint16(loc[int(off)+2*i:])
		}
		return string(utf16.Decode(u)), nil
	}
	entries := make(map[string]string, n)
	for i := range n {
		e := loc[vhdxParentLocatorHeaderSize+i*vhdxParentLocatorEntrySize:]
		k, err := str(binary.LittleEndian.Uint32(e), binary.LittleEndian.Uint16(e[8:]))
		if err != nil {
			return nil, err
		}
		v, err := str(binary.LittleEndian.Uint32(e[4:]), binary.LittleEndian.Uint16(e[10:]))
		if err != nil {
			return nil, err
		}
		if _, dup := entries[k]; dup {
			return nil, fmt.Errorf("the parent locator names %q twice", k)
		}
		entries[k] = v
	}
	return entries, nil
}
