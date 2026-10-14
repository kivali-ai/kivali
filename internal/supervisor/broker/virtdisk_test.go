package broker

import (
	"testing"
	"unsafe"
)

// The virtdisk structures have the sizes and offsets virtdisk.h gives
// them on 64-bit Windows, which the broker passes to virtdisk.dll as
// they are.
func TestVirtdiskLayouts(t *testing.T) {
	var (
		vst  virtualStorageType
		open openVirtualDiskParameters
		info getVirtualDiskInfo
		comp compactVirtualDiskParameters
		prog virtualDiskProgress
	)
	for _, c := range []struct {
		what      string
		got, want uintptr
	}{
		{"sizeof(VIRTUAL_STORAGE_TYPE)", unsafe.Sizeof(vst), 20},
		{"VIRTUAL_STORAGE_TYPE.VendorId", unsafe.Offsetof(vst.VendorID), 4},
		{"sizeof(OPEN_VIRTUAL_DISK_PARAMETERS)", unsafe.Sizeof(open), 44},
		{"OPEN_VIRTUAL_DISK_PARAMETERS.Version2.GetInfoOnly", unsafe.Offsetof(open.GetInfoOnly), 4},
		{"OPEN_VIRTUAL_DISK_PARAMETERS.Version2.ReadOnly", unsafe.Offsetof(open.ReadOnly), 8},
		{"OPEN_VIRTUAL_DISK_PARAMETERS.Version2.ResiliencyGuid", unsafe.Offsetof(open.ResiliencyGUID), 12},
		{"OPEN_VIRTUAL_DISK_PARAMETERS.Version3.SnapshotId", unsafe.Offsetof(open.SnapshotID), 28},
		{"sizeof(GET_VIRTUAL_DISK_INFO)", unsafe.Sizeof(info), 32},
		{"GET_VIRTUAL_DISK_INFO's union", unsafe.Offsetof(info.Union), 8},
		{"sizeof(COMPACT_VIRTUAL_DISK_PARAMETERS)", unsafe.Sizeof(comp), 8},
		{"sizeof(VIRTUAL_DISK_PROGRESS)", unsafe.Sizeof(prog), 24},
		{"VIRTUAL_DISK_PROGRESS.CurrentValue", unsafe.Offsetof(prog.CurrentValue), 8},
		{"VIRTUAL_DISK_PROGRESS.CompletionValue", unsafe.Offsetof(prog.CompletionValue), 16},
	} {
		if c.got != c.want {
			t.Errorf("%s = %d, want %d", c.what, c.got, c.want)
		}
	}
	if got := virtualStorageTypeVendorMicrosoft.String(); got != "ec984aec-a0f9-47e9-901f-71415a66345b" {
		t.Errorf("VIRTUAL_STORAGE_TYPE_VENDOR_MICROSOFT = %s", got)
	}
}
