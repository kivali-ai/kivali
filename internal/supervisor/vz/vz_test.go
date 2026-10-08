//go:build darwin && cgo

package vz

import (
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/Code-Hex/vz/v3"
)

func TestImage(t *testing.T) {
	dir := t.TempDir()
	if _, err := (Backend{}).Image(dir); err == nil || !strings.Contains(err.Error(), KernelFile) {
		t.Fatalf("empty dir: %v", err)
	}
	for _, f := range []string{KernelFile, InitrdFile, RootFile} {
		if err := os.WriteFile(filepath.Join(dir, f), nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	info, err := (Backend{}).Image(dir)
	if err != nil || info.Version != "dev" || info.Arch != "arm64" {
		t.Fatalf("no VERSION: %+v %v", info, err)
	}
	if err := os.WriteFile(filepath.Join(dir, VersionFile), []byte("0.16.0\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if info, err := (Backend{}).Image(dir); err != nil || info.Version != "0.16.0" {
		t.Fatalf("VERSION: %+v %v", info, err)
	}
}

func TestCmdline(t *testing.T) {
	if Cmdline(false) != BaseCmdline || strings.Contains(Cmdline(false), FormatFlag) {
		t.Fatalf("plain boot: %q", Cmdline(false))
	}
	if Cmdline(true) != BaseCmdline+" "+FormatFlag {
		t.Fatalf("first boot: %q", Cmdline(true))
	}
}

func TestCreateDataDiskIsSparseAndReplaces(t *testing.T) {
	p := (Backend{}).DataDiskPath(t.TempDir())
	if filepath.Base(p) != DataDiskFile {
		t.Fatalf("data disk path %s", p)
	}
	if err := os.WriteFile(p, []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := (Backend{}).CreateDataDisk(p, 1<<30); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(p)
	if err != nil || fi.Size() != 1<<30 || fi.Mode().Perm() != 0o600 {
		t.Fatalf("disk %v %v", fi, err)
	}
	// Sparse: a GiB of size, next to nothing allocated (st_blocks is in
	// 512-byte units).
	if blocks := fi.Sys().(*syscall.Stat_t).Blocks; blocks*512 >= 1<<20 {
		t.Fatalf("data disk not sparse: %d bytes allocated", blocks*512)
	}
	b := make([]byte, 3)
	f, err := os.Open(p)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	if _, err := f.Read(b); err != nil || string(b) != "\x00\x00\x00" {
		t.Fatalf("head %q %v", b, err)
	}
}

// TestDiskAttachmentModes pins the disk caching and synchronization
// modes: the automatic caching mode corrupts guest disks (see
// DiskCaching), and anything short of full synchronization lets a host
// crash lose writes the guest has flushed.
func TestDiskAttachmentModes(t *testing.T) {
	if DiskCaching != vz.DiskImageCachingModeCached {
		t.Fatalf("DiskCaching = %v, want DiskImageCachingModeCached", DiskCaching)
	}
	if DiskSync != vz.DiskImageSynchronizationModeFull {
		t.Fatalf("DiskSync = %v, want DiskImageSynchronizationModeFull", DiskSync)
	}
	p := filepath.Join(t.TempDir(), "disk.img")
	if err := os.WriteFile(p, make([]byte, 1<<20), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, ro := range []bool{true, false} {
		if _, err := DiskAttachment(p, ro); err != nil {
			t.Fatalf("DiskAttachment(readOnly=%v): %v", ro, err)
		}
	}
}
