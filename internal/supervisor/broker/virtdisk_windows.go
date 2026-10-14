//go:build windows

package broker

import (
	"context"
	"encoding/binary"
	"fmt"
	"runtime"
	"unsafe"

	"github.com/Microsoft/go-winio/pkg/guid"
	"golang.org/x/sys/windows"
)

// The virtual disk API (virtdisk.dll, from the system directory only).
// The broker compacts the data disk through it, on one handle opened
// with OPEN_VIRTUAL_DISK_FLAG_NO_PARENTS, so no parent a locator names
// is ever opened, whatever the file's bytes say.
var (
	modVirtdisk                         = windows.NewLazySystemDLL("virtdisk.dll")
	procOpenVirtualDisk                 = modVirtdisk.NewProc("OpenVirtualDisk")
	procGetVirtualDiskInformation       = modVirtdisk.NewProc("GetVirtualDiskInformation")
	procCompactVirtualDisk              = modVirtdisk.NewProc("CompactVirtualDisk")
	procGetVirtualDiskOperationProgress = modVirtdisk.NewProc("GetVirtualDiskOperationProgress")
)

// findVirtdisk loads virtdisk.dll and finds the functions the broker
// calls, so a missing one is an error rather than a panic in Call.
func findVirtdisk() error {
	for _, p := range []*windows.LazyProc{procOpenVirtualDisk, procGetVirtualDiskInformation, procCompactVirtualDisk, procGetVirtualDiskOperationProgress} {
		if err := p.Find(); err != nil {
			return fmt.Errorf("virtdisk.dll: %w", err)
		}
	}
	return nil
}

// openVHDXNoParents opens the file at p as a VHDX (the storage type is
// named, not detected, so any other format fails to open) with
// OPEN_VIRTUAL_DISK_FLAG_NO_PARENTS: the file alone, never a parent its
// locator names. A Version 2 open takes VIRTUAL_DISK_ACCESS_NONE and,
// with neither GetInfoOnly nor ReadOnly, gives a handle that can
// compact. The open is checked with the token of the thread that makes
// it.
func openVHDXNoParents(p string) (windows.Handle, error) {
	if err := findVirtdisk(); err != nil {
		return 0, err
	}
	name, err := windows.UTF16PtrFromString(p)
	if err != nil {
		return 0, err
	}
	vst := virtualStorageType{DeviceID: virtualStorageTypeDeviceVHDX, VendorID: virtualStorageTypeVendorMicrosoft}
	params := openVirtualDiskParameters{Version: openVirtualDiskVersion2}
	var h windows.Handle
	r, _, _ := procOpenVirtualDisk.Call(
		uintptr(unsafe.Pointer(&vst)),
		uintptr(unsafe.Pointer(name)),
		virtualDiskAccessNone,
		openVirtualDiskFlagNoParents,
		uintptr(unsafe.Pointer(&params)),
		uintptr(unsafe.Pointer(&h)))
	if r != 0 {
		return 0, windows.Errno(r)
	}
	return h, nil
}

// diskInfo asks GetVirtualDiskInformation for one member (version)
// and returns the union it filled.
func diskInfo(h windows.Handle, version uint32) ([24]byte, error) {
	info := getVirtualDiskInfo{Version: version}
	size := uint32(unsafe.Sizeof(info))
	r, _, _ := procGetVirtualDiskInformation.Call(
		uintptr(h),
		uintptr(unsafe.Pointer(&size)),
		uintptr(unsafe.Pointer(&info)),
		0)
	if r != 0 {
		return [24]byte{}, windows.Errno(r)
	}
	return info.Union, nil
}

// checkDynamicVHDX refuses, through the open handle, anything but a
// Microsoft VHDX whose provider subtype is dynamic: a differencing disk
// (which the handle, opened without parents, could not compact as a
// whole anyway) and a fixed one are not the data disk the backend made.
func checkDynamicVHDX(h windows.Handle) error {
	u, err := diskInfo(h, getVirtualDiskInfoVirtualStorageType)
	if err != nil {
		return fmt.Errorf("read the disk's storage type: %w", err)
	}
	device := binary.LittleEndian.Uint32(u[:4])
	vendor := guid.FromWindowsArray([16]byte(u[4:20]))
	if device != virtualStorageTypeDeviceVHDX || vendor != virtualStorageTypeVendorMicrosoft {
		return fmt.Errorf("%w: its storage type is device %d of vendor %s", errNotDynamic, device, vendor)
	}
	u, err = diskInfo(h, getVirtualDiskInfoProviderSubtype)
	if err != nil {
		return fmt.Errorf("read the disk's subtype: %w", err)
	}
	if subtype := binary.LittleEndian.Uint32(u[:4]); subtype != providerSubtypeDynamic {
		return fmt.Errorf("%w: %s", errNotDynamic, subtypeName(subtype))
	}
	return nil
}

// compactVirtualDisk compacts the disk open as h with
// COMPACT_VIRTUAL_DISK_FLAG_NO_ZERO_SCAN, as Optimize-VHD -Mode Quick
// does a disk that is not mounted: the blocks the allocation table
// marks unused (the guest's discards) are given back and the rest moved
// down, without reading every block for zeros. It runs as an overlapped operation and waits
// for it to end; when ctx ends first the operation is cancelled
// (CancelIoEx) and still waited for, since virtdisk writes its outcome
// into the OVERLAPPED, and a compaction cut short is left to the VHDX's
// own log.
func compactVirtualDisk(ctx context.Context, h windows.Handle) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	ev, err := windows.CreateEvent(nil, 1, 0, nil)
	if err != nil {
		return fmt.Errorf("create an event: %w", err)
	}
	defer func() { _ = windows.CloseHandle(ev) }()
	ov := &windows.Overlapped{HEvent: ev}
	params := &compactVirtualDiskParameters{Version: compactVirtualDiskVersion1}
	r, _, _ := procCompactVirtualDisk.Call(
		uintptr(h),
		compactVirtualDiskFlagNoZeroScan,
		uintptr(unsafe.Pointer(params)),
		uintptr(unsafe.Pointer(ov)))
	switch windows.Errno(r) {
	case 0:
		return nil
	case windows.ERROR_IO_PENDING:
	default:
		return windows.Errno(r)
	}
	done := make(chan struct{})
	watched := make(chan struct{})
	go func() {
		defer close(watched)
		select {
		case <-ctx.Done():
			_ = windows.CancelIoEx(h, ov)
		case <-done:
		}
	}()
	_, werr := windows.WaitForSingleObject(ev, windows.INFINITE)
	close(done)
	<-watched
	if werr != nil {
		return fmt.Errorf("wait for the compaction: %w", werr)
	}
	var prog virtualDiskProgress
	r, _, _ = procGetVirtualDiskOperationProgress.Call(
		uintptr(h),
		uintptr(unsafe.Pointer(ov)),
		uintptr(unsafe.Pointer(&prog)))
	runtime.KeepAlive(params)
	runtime.KeepAlive(ov)
	if r != 0 {
		return fmt.Errorf("read the compaction's outcome: %w", windows.Errno(r))
	}
	switch st := windows.Errno(prog.OperationStatus); {
	case st == 0:
		return nil
	case st == windows.ERROR_OPERATION_ABORTED && ctx.Err() != nil:
		return fmt.Errorf("the compaction was stopped: %w", ctx.Err())
	default:
		return st
	}
}

// virtdisk.h values.
const (
	// VIRTUAL_STORAGE_TYPE_DEVICE_VHDX.
	virtualStorageTypeDeviceVHDX = 3
	// VIRTUAL_DISK_ACCESS_NONE: the only access a Version 2 open takes;
	// the handle may then do anything but what GetInfoOnly or ReadOnly
	// rule out.
	virtualDiskAccessNone = 0
	// OPEN_VIRTUAL_DISK_FLAG_NO_PARENTS: open the file alone, without
	// opening any parent its locator names.
	openVirtualDiskFlagNoParents = 0x1
	// OPEN_VIRTUAL_DISK_VERSION_2.
	openVirtualDiskVersion2 = 2
	// GET_VIRTUAL_DISK_INFO_VIRTUAL_STORAGE_TYPE and
	// GET_VIRTUAL_DISK_INFO_PROVIDER_SUBTYPE.
	getVirtualDiskInfoVirtualStorageType = 6
	getVirtualDiskInfoProviderSubtype    = 7
	// COMPACT_VIRTUAL_DISK_VERSION_1.
	compactVirtualDiskVersion1 = 1
	// COMPACT_VIRTUAL_DISK_FLAG_NO_ZERO_SCAN: reclaim the blocks the
	// disk's own allocation table marks unused (the guest's discards)
	// and move the rest down, without reading every block for zeros.
	compactVirtualDiskFlagNoZeroScan = 0x1
)

// The provider subtypes GET_VIRTUAL_DISK_INFO_PROVIDER_SUBTYPE answers.
const (
	providerSubtypeFixed        = 2
	providerSubtypeDynamic      = 3
	providerSubtypeDifferencing = 4
)

// subtypeName names a provider subtype for a refusal.
func subtypeName(subtype uint32) string {
	switch subtype {
	case providerSubtypeFixed:
		return "a fixed disk"
	case providerSubtypeDynamic:
		return "a dynamic disk"
	case providerSubtypeDifferencing:
		return "a differencing disk"
	}
	return "a disk of an unknown kind"
}
