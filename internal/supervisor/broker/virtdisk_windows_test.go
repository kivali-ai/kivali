//go:build windows

package broker

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/Microsoft/go-winio/vhd"
	"golang.org/x/sys/windows"
)

// These make real VHDX files with virtdisk.dll and compact them as the
// broker does, through a winCaller impersonating this account over a
// pipe, and check them as the broker's root disk is checked. They need an elevated test process (GitHub's Windows runners
// are elevated); no Hyper-V. With KIVALI_REQUIRE_ELEVATED set, as CI
// sets it, a process that is not elevated fails them rather than
// skipping them.

func skipUnlessElevated(t *testing.T) {
	t.Helper()
	if !windows.GetCurrentProcessToken().IsElevated() {
		if os.Getenv("KIVALI_REQUIRE_ELEVATED") != "" {
			t.Fatal("KIVALI_REQUIRE_ELEVATED is set, but this test process is not elevated")
		}
		t.Skip("making VHDX files with virtdisk.dll here needs an elevated test process; run go test from an elevated terminal")
	}
}

// createVHDX makes a 64 MiB VHDX at p: dynamic, or fixed (every block
// allocated).
func createVHDX(t *testing.T, p string, fixed bool) {
	t.Helper()
	flags := vhd.CreateVirtualDiskFlagNone
	if fixed {
		flags = vhd.CreateVirtualDiskFlagFullPhysicalAllocation
	}
	params := vhd.CreateVirtualDiskParameters{Version: 2, Version2: vhd.CreateVersion2{
		MaximumSize:      64 << 20,
		BlockSizeInBytes: 2 << 20,
	}}
	h, err := vhd.CreateVirtualDisk(p, vhd.VirtualDiskAccessNone, flags, &params)
	if err != nil {
		t.Fatalf("create %s: %v", p, err)
	}
	if err := syscall.CloseHandle(h); err != nil {
		t.Fatal(err)
	}
}

// pinAsCaller is a caller impersonating this account over a pipe, with
// p pinned as the broker pins the data disk (File for writing), and p's
// canonical path.
func pinAsCaller(t *testing.T, p string) (*winCaller, string) {
	t.Helper()
	server, _ := callerPipe(t)
	c := &winCaller{pipe: windows.Handle(server.Fd()), sid: ourSID(t)}
	t.Cleanup(func() { _ = c.Close() })
	canon, err := c.File(p, AccessWrite)
	if err != nil {
		t.Fatalf("File(%s): %v", p, err)
	}
	return c, canon
}

// A dynamic VHDX is compacted, opened as the impersonated caller, and
// its file's sizes come back.
func TestCompactDynamicVHDX(t *testing.T) {
	skipUnlessElevated(t)
	p := filepath.Join(canonical(t, t.TempDir()), DataDiskName)
	createVHDX(t, p, false)
	c, canon := pinAsCaller(t, p)
	before, after, err := c.CompactVHDX(context.Background(), canon)
	if err != nil {
		t.Fatalf("CompactVHDX: %v", err)
	}
	if before <= 0 || after <= 0 || after > before {
		t.Fatalf("sizes before %d, after %d", before, after)
	}
}

// A compaction whose time is already up does not start.
func TestCompactVHDXCancelled(t *testing.T) {
	skipUnlessElevated(t)
	p := filepath.Join(canonical(t, t.TempDir()), DataDiskName)
	createVHDX(t, p, false)
	c, canon := pinAsCaller(t, p)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, _, err := c.CompactVHDX(ctx, canon); !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
}

// A differencing disk is refused on the virtual disk's own handle, as
// differencing, without its parent being opened: the parent its locator
// names is gone (renamed away), so an open that looked for it would fail
// on that instead. The broker's own read of the locator gives the
// parent's path from the file Windows wrote.
func TestCompactRefusesDifferencingWithoutOpeningTheParent(t *testing.T) {
	skipUnlessElevated(t)
	dir := canonical(t, t.TempDir())
	parent := filepath.Join(dir, "parent.vhdx")
	createVHDX(t, parent, false)
	p := filepath.Join(dir, DataDiskName)
	if err := vhd.CreateDiffVhd(p, parent, 2); err != nil {
		t.Fatalf("create the differencing disk: %v", err)
	}
	got, err := winFiles{}.ParentPath(p)
	if err != nil || !strings.EqualFold(got, parent) {
		t.Fatalf("ParentPath = %q, %v; want %s", got, err, parent)
	}
	if err := os.Rename(parent, filepath.Join(dir, "moved.vhdx")); err != nil {
		t.Fatal(err)
	}
	c, canon := pinAsCaller(t, p)
	_, _, err = c.CompactVHDX(context.Background(), canon)
	if !errors.Is(err, errNotDynamic) || !strings.Contains(err.Error(), "differencing") {
		t.Fatalf("err = %v, want the refusal of a differencing disk", err)
	}
}

// A fixed VHDX is refused on the virtual disk's own handle.
func TestCompactRefusesFixed(t *testing.T) {
	skipUnlessElevated(t)
	p := filepath.Join(canonical(t, t.TempDir()), DataDiskName)
	createVHDX(t, p, true)
	c, canon := pinAsCaller(t, p)
	_, _, err := c.CompactVHDX(context.Background(), canon)
	if !errors.Is(err, errNotDynamic) || !strings.Contains(err.Error(), "fixed") {
		t.Fatalf("err = %v, want the refusal of a fixed disk", err)
	}
}

// A file that is not a VHDX does not open as one.
func TestCompactRefusesAFileThatIsNoVHDX(t *testing.T) {
	skipUnlessElevated(t)
	p := filepath.Join(canonical(t, t.TempDir()), DataDiskName)
	write(t, p, strings.Repeat("x", 4096))
	c, canon := pinAsCaller(t, p)
	if _, _, err := c.CompactVHDX(context.Background(), canon); err == nil {
		t.Fatal("a file that is no VHDX was compacted")
	}
}

// The broker's root disk check accepts a dynamic and a fixed VHDX, and
// gives the canonical path.
func TestRootDiskAcceptsDynamicAndFixed(t *testing.T) {
	skipUnlessElevated(t)
	dir := canonical(t, t.TempDir())
	for _, fixed := range []bool{false, true} {
		p := filepath.Join(dir, "root-dynamic.vhdx")
		if fixed {
			p = filepath.Join(dir, "root-fixed.vhdx")
		}
		createVHDX(t, p, fixed)
		got, err := winFiles{}.RootDisk(p)
		if err != nil || !strings.EqualFold(got, p) {
			t.Errorf("RootDisk(%s) = %q, %v; want %s", p, got, err, p)
		}
	}
}

// A differencing root is refused, as differencing, without its parent
// being opened: the parent its locator names is gone (renamed away), so
// an open that looked for it would fail on that instead.
func TestRootDiskRefusesDifferencingWithoutOpeningTheParent(t *testing.T) {
	skipUnlessElevated(t)
	dir := canonical(t, t.TempDir())
	parent := filepath.Join(dir, "parent.vhdx")
	createVHDX(t, parent, false)
	p := filepath.Join(dir, RootFile)
	if err := vhd.CreateDiffVhd(p, parent, 2); err != nil {
		t.Fatalf("create the differencing disk: %v", err)
	}
	if err := os.Rename(parent, filepath.Join(dir, "moved.vhdx")); err != nil {
		t.Fatal(err)
	}
	_, err := winFiles{}.RootDisk(p)
	if !errors.Is(err, errNotRoot) || !strings.Contains(err.Error(), "differencing") {
		t.Fatalf("err = %v, want the refusal of a differencing disk", err)
	}
}

// A file that is not a VHDX is no root disk.
func TestRootDiskRefusesAFileThatIsNoVHDX(t *testing.T) {
	skipUnlessElevated(t)
	p := filepath.Join(canonical(t, t.TempDir()), RootFile)
	write(t, p, strings.Repeat("x", 4096))
	if _, err := (winFiles{}).RootDisk(p); err == nil {
		t.Fatal("a file that is no VHDX was accepted as the root disk")
	}
}
