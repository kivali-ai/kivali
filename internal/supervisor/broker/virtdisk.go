package broker

import (
	"errors"

	"github.com/Microsoft/go-winio/pkg/guid"
)

// The parts of virtdisk.h the broker's compaction and root disk check
// use
// (virtdisk_windows.go), laid out as the C structures are on 64-bit
// Windows (amd64 and arm64 alike: BOOL, ULONG and DWORD are 4 bytes, a
// GUID 16 with 4-byte alignment, and a ULONGLONG 8 with 8-byte
// alignment). Padding is spelled out, so the offsets do not depend on
// Go's own alignment rules; virtdisk_test.go checks them.

// virtualStorageType is VIRTUAL_STORAGE_TYPE.
type virtualStorageType struct {
	DeviceID uint32
	VendorID guid.GUID
}

// openVirtualDiskParameters is OPEN_VIRTUAL_DISK_PARAMETERS with
// Version 2's fields (GetInfoOnly, ReadOnly, ResiliencyGuid) at their
// offsets, sized as the whole structure (Version 3's SnapshotId ends
// the union).
type openVirtualDiskParameters struct {
	Version        uint32
	GetInfoOnly    int32 // BOOL
	ReadOnly       int32 // BOOL
	ResiliencyGUID guid.GUID
	SnapshotID     guid.GUID // Version 3 only; zero
}

// getVirtualDiskInfo is GET_VIRTUAL_DISK_INFO: the version (which
// member is asked for) and the union, which holds a ULONGLONG and so
// starts at offset 8; its largest member (Size: two ULONGLONGs and two
// ULONGs) makes it 24 bytes.
type getVirtualDiskInfo struct {
	Version uint32
	_       uint32
	Union   [24]byte
}

// compactVirtualDiskParameters is COMPACT_VIRTUAL_DISK_PARAMETERS
// (Version 1: one reserved ULONG).
type compactVirtualDiskParameters struct {
	Version  uint32
	Reserved uint32
}

// virtualDiskProgress is VIRTUAL_DISK_PROGRESS.
type virtualDiskProgress struct {
	OperationStatus uint32
	_               uint32
	CurrentValue    uint64
	CompletionValue uint64
}

// virtualStorageTypeVendorMicrosoft is
// VIRTUAL_STORAGE_TYPE_VENDOR_MICROSOFT.
var virtualStorageTypeVendorMicrosoft = guid.GUID{
	Data1: 0xec984aec, Data2: 0xa0f9, Data3: 0x47e9,
	Data4: [8]byte{0x90, 0x1f, 0x71, 0x41, 0x5a, 0x66, 0x34, 0x5b},
}

// errNotDynamic is the refusal of a disk that is not a dynamic VHDX:
// the data disk the backend made is one, and nothing else is compacted.
var errNotDynamic = errors.New("not a dynamic VHDX")
