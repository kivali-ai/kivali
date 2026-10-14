//go:build windows

package broker

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unsafe"

	"golang.org/x/sys/windows"
)

// These run the broker's own disk operations on real files: the copy an
// import and a snapshot make, the checks and the compaction of the copy,
// the renames of a restore, and the VM directory's DACL. The ones that
// make a VHDX or a VM directory need an elevated test process (see
// skipUnlessElevated).

// pinnedAsCaller is the caller's view of p, a file this account owns:
// pinned through a pipe this account is the client of, as an import
// pins the data disk it copies, and read through the pinning handle.
func pinnedAsCaller(t *testing.T, p string) (io.ReaderAt, int64) {
	t.Helper()
	server, _ := callerPipe(t)
	c := &winCaller{pipe: windows.Handle(server.Fd()), sid: ourSID(t)}
	t.Cleanup(func() { _ = c.Close() })
	canon, err := c.File(p)
	if err != nil {
		t.Fatalf("File(%s): %v", p, err)
	}
	r, size, err := c.Contents(canon)
	if err != nil {
		t.Fatalf("Contents(%s): %v", p, err)
	}
	return r, size
}

// An import's copy of a dynamic VHDX, read through the caller's handle,
// is a VHDX virtdisk opens without parents as a dynamic disk and
// compacts; its file is the source's, byte for byte, and not sparse
// (the source's file has no holes, and Hyper-V refuses a sparse virtual
// disk on older builds); the meter ends at the bytes copied.
func TestImportCopyOfADynamicDisk(t *testing.T) {
	skipUnlessElevated(t)
	dir := canonical(t, t.TempDir())
	src := filepath.Join(dir, "legacy.vhdx")
	createVHDX(t, src, false)
	r, size := pinnedAsCaller(t, src)
	dst := filepath.Join(dir, importName)
	m := &Meter{}
	if err := (winFiles{}).CopyFile(context.Background(), r, size, dst, m); err != nil {
		t.Fatalf("CopyFile: %v", err)
	}
	if done, total := m.Get(); done != size || total != size {
		t.Fatalf("meter = %d of %d, want %d of %d", done, total, size, size)
	}
	a, _ := os.ReadFile(src)
	b, _ := os.ReadFile(dst)
	if !bytes.Equal(a, b) {
		t.Fatal("the copy differs from the source")
	}
	if attrs := fileAttrs(t, dst); attrs&windows.FILE_ATTRIBUTE_SPARSE_FILE != 0 {
		t.Fatal("the copy of a file with no holes is sparse")
	}
	if err := (winFiles{}).CheckDynamicVHDX(dst); err != nil {
		t.Fatalf("CheckDynamicVHDX: %v", err)
	}
	if _, _, err := (winFiles{}).CompactVHDX(context.Background(), dst, nil); err != nil {
		t.Fatalf("CompactVHDX of the copy: %v", err)
	}
}

// A sparse source's holes stay holes: only its allocated ranges are
// read and written, into a sparse copy that reads the same.
func TestCopyKeepsASparseFileSparse(t *testing.T) {
	dir := canonical(t, t.TempDir())
	src := filepath.Join(dir, "sparse")
	f, err := os.Create(src)
	if err != nil {
		t.Fatal(err)
	}
	var n uint32
	if err := windows.DeviceIoControl(windows.Handle(f.Fd()), windows.FSCTL_SET_SPARSE, nil, 0, nil, 0, &n, nil); err != nil {
		t.Skipf("FSCTL_SET_SPARSE: %v", err)
	}
	const size = 64 << 20
	if err := f.Truncate(size); err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteAt([]byte("head"), 0); err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteAt([]byte("tail"), size-4); err != nil {
		t.Fatal(err)
	}
	_ = f.Close()
	r, got := pinnedAsCaller(t, src)
	dst := filepath.Join(dir, "copy")
	m := &Meter{}
	if err := (winFiles{}).CopyFile(context.Background(), r, got, dst, m); err != nil {
		t.Fatalf("CopyFile: %v", err)
	}
	if fileAttrs(t, dst)&windows.FILE_ATTRIBUTE_SPARSE_FILE == 0 {
		t.Fatal("the copy of a sparse file is not sparse")
	}
	if done, total := m.Get(); done != total || total >= size {
		t.Fatalf("meter = %d of %d: the holes were copied", done, total)
	}
	fi, err := (winFiles{}).Stat(dst)
	if err != nil || fi.Size != size || fi.Used >= size/2 {
		t.Fatalf("Stat(copy) = %+v, %v", fi, err)
	}
	a, _ := os.ReadFile(src)
	b, _ := os.ReadFile(dst)
	if !bytes.Equal(a, b) {
		t.Fatal("the copy reads differently from the source")
	}
}

// observed reads through r and notes the meter's progress at each read.
type observed struct {
	r    io.ReaderAt
	m    *Meter
	seen []int64
}

func (o *observed) ReadAt(b []byte, off int64) (int, error) {
	done, _ := o.m.Get()
	o.seen = append(o.seen, done)
	return o.r.ReadAt(b, off)
}

// The meter moves chunk by chunk as the copy goes; a copy whose ctx has
// ended stops and deletes what it wrote; a copy never lands on anything
// already at its destination.
func TestCopyProgressCancellationAndExisting(t *testing.T) {
	dir := canonical(t, t.TempDir())
	src := filepath.Join(dir, "src")
	content := bytes.Repeat([]byte("0123456789abcdef"), (3*copyChunk+copyChunk/2)/16)
	if err := os.WriteFile(src, content, 0o600); err != nil {
		t.Fatal(err)
	}
	r, size := pinnedAsCaller(t, src)
	m := &Meter{}
	o := &observed{r: r, m: m}
	dst := filepath.Join(dir, "dst")
	if err := (winFiles{}).CopyFile(context.Background(), o, size, dst, m); err != nil {
		t.Fatalf("CopyFile: %v", err)
	}
	want := []int64{0, copyChunk, 2 * copyChunk, 3 * copyChunk}
	if len(o.seen) != len(want) {
		t.Fatalf("progress seen at each read = %v, want %v", o.seen, want)
	}
	for i := range want {
		if o.seen[i] != want[i] {
			t.Fatalf("progress seen at each read = %v, want %v", o.seen, want)
		}
	}

	if err := (winFiles{}).CopyFile(context.Background(), r, size, dst, nil); err == nil {
		t.Fatal("a copy landed on an existing file")
	}
	if b, _ := os.ReadFile(dst); !bytes.Equal(b, content) {
		t.Fatal("the existing file changed")
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	stopped := filepath.Join(dir, "stopped")
	if err := (winFiles{}).CopyFile(ctx, r, size, stopped, nil); !errors.Is(err, context.Canceled) {
		t.Fatalf("a cancelled copy: err = %v", err)
	}
	if _, err := os.Lstat(stopped); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the cancelled copy was left behind (%v)", err)
	}
}

// The copy is checked again in the VM directory: a differencing disk is
// refused, as differencing, without its parent being opened, both
// through the caller's handle (dynamicNoParent, as the import checks its
// source) and by CheckDynamicVHDX.
func TestCheckRefusesDifferencingWithoutOpeningTheParent(t *testing.T) {
	skipUnlessElevated(t)
	p := diffWithoutParent(t, canonical(t, t.TempDir()), DataDiskName)
	r, size := pinnedAsCaller(t, p)
	if err := dynamicNoParent(r, size); !errors.Is(err, errNotDynamic) {
		t.Fatalf("dynamicNoParent = %v, want the refusal", err)
	}
	err := winFiles{}.CheckDynamicVHDX(p)
	if !errors.Is(err, errNotDynamic) || !strings.Contains(err.Error(), "differencing") {
		t.Fatalf("CheckDynamicVHDX = %v, want the refusal of a differencing disk", err)
	}
}

// A restore's rename replaces the data disk with the snapshot; an
// import's never replaces anything (ErrExists).
func TestRenameReplacesOnlyWhenAsked(t *testing.T) {
	dir := canonical(t, t.TempDir())
	data, snap := filepath.Join(dir, DataDiskName), filepath.Join(dir, SnapshotName)
	write(t, data, "half-applied")
	write(t, snap, "before")
	if err := (winFiles{}).Rename(snap, data, false); !errors.Is(err, ErrExists) {
		t.Fatalf("Rename without replace = %v, want ErrExists", err)
	}
	if b, _ := os.ReadFile(data); string(b) != "half-applied" {
		t.Fatalf("data = %q", b)
	}
	if err := (winFiles{}).Rename(snap, data, true); err != nil {
		t.Fatalf("Rename with replace: %v", err)
	}
	if b, _ := os.ReadFile(data); string(b) != "before" {
		t.Fatalf("data = %q", b)
	}
	if _, err := os.Lstat(snap); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the snapshot is still there (%v)", err)
	}
	if _, err := (winFiles{}).Stat(snap); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("Stat of a missing file = %v, want ErrNotExist", err)
	}
}

// The rights a SID's own allow entries give on what is open as h (its
// inherit-only entries apply to what is made inside, not to it), with
// generic rights mapped to the file's; and every SID any allow entry
// names.
func rightsOf(t *testing.T, h windows.Handle, sid string) (rights uint32, sids []string) {
	t.Helper()
	sd, err := windows.GetSecurityInfo(h, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		t.Fatal(err)
	}
	dacl, _, err := sd.DACL()
	if err != nil {
		t.Fatal(err)
	}
	for i := uint16(0); i < dacl.AceCount; i++ {
		var ace *windows.ACCESS_ALLOWED_ACE
		if err := windows.GetAce(dacl, uint32(i), &ace); err != nil {
			t.Fatal(err)
		}
		if ace.Header.AceType != windows.ACCESS_ALLOWED_ACE_TYPE {
			continue
		}
		s := (*windows.SID)(unsafe.Pointer(&ace.SidStart)).String()
		sids = append(sids, s)
		if ace.Header.AceFlags&windows.INHERIT_ONLY_ACE != 0 || !strings.EqualFold(s, sid) {
			continue
		}
		mask := uint32(ace.Mask)
		if mask&windows.GENERIC_ALL != 0 {
			mask |= windows.STANDARD_RIGHTS_REQUIRED | windows.SYNCHRONIZE | 0x1ff // FILE_ALL_ACCESS
		}
		if mask&windows.GENERIC_READ != 0 {
			mask |= windows.FILE_GENERIC_READ
		}
		if mask&windows.GENERIC_WRITE != 0 {
			mask |= windows.FILE_GENERIC_WRITE
		}
		if mask&windows.GENERIC_EXECUTE != 0 {
			mask |= windows.FILE_GENERIC_EXECUTE
		}
		rights |= mask
	}
	return rights, sids
}

// The VM directory, and a disk the broker makes in it, let the account
// it was made for read and nothing more: no writing, appending, adding
// files, deleting, nor changing the DACL or the owner; and no entry
// names a group that account could be in. A second account is refused
// the directory, both to use and to remove; the first may remove it.
// The accounts are synthetic SIDs, which no lookup resolves: the DACL
// is all there is to go on.
func TestVMDirIsTheFirstAccountsAndReadOnlyToIt(t *testing.T) {
	skipUnlessElevated(t)
	const (
		first  = "S-1-5-21-4000000001-4000000002-4000000003-1001"
		second = "S-1-5-21-4000000001-4000000002-4000000003-1002"
		key    = "0123abcd"
	)
	f := winFiles{Root: filepath.Join(canonical(t, t.TempDir()), "vm")}
	dir, release, err := f.VMDir(key, first)
	if err != nil {
		t.Fatalf("VMDir: %v", err)
	}
	disk := filepath.Join(dir, DataDiskName)
	err = f.CopyFile(context.Background(), bytes.NewReader([]byte("disk")), 4, disk, nil)
	release()
	if err != nil {
		t.Fatalf("CopyFile: %v", err)
	}
	// FILE_DELETE_CHILD is 0x40.
	const forbidden = windows.FILE_WRITE_DATA | windows.FILE_APPEND_DATA | windows.FILE_WRITE_EA | windows.FILE_WRITE_ATTRIBUTES |
		0x40 | windows.DELETE | windows.WRITE_DAC | windows.WRITE_OWNER
	for _, p := range []struct {
		what string
		path string
		dir  bool
	}{{"the VM directory", dir, true}, {"the data disk", disk, false}} {
		h, err := windows.CreateFile(windows.StringToUTF16Ptr(p.path), windows.READ_CONTROL,
			windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE, nil,
			windows.OPEN_EXISTING, windows.FILE_FLAG_BACKUP_SEMANTICS|windows.FILE_FLAG_OPEN_REPARSE_POINT, 0)
		if err != nil {
			t.Fatalf("open %s: %v", p.path, err)
		}
		rights, sids := rightsOf(t, h, first)
		_ = windows.CloseHandle(h)
		if rights&windows.FILE_READ_DATA == 0 {
			t.Errorf("%s: the account cannot read it (rights %#x)", p.what, rights)
		}
		if rights&forbidden != 0 {
			t.Errorf("%s: the account has %#x of the rights it must not have (rights %#x)", p.what, rights&forbidden, rights)
		}
		for _, s := range sids {
			switch {
			case strings.EqualFold(s, LocalSystemSID), strings.EqualFold(s, AdministratorsSID), strings.EqualFold(s, ServiceSID), strings.EqualFold(s, first):
			default:
				t.Errorf("%s: an entry grants %s", p.what, s)
			}
		}
	}

	if _, release, err := f.VMDir(key, second); !errors.Is(err, errOtherAccount) {
		if err == nil {
			release()
		}
		t.Fatalf("VMDir for a second account: err = %v, want errOtherAccount", err)
	}
	if err := f.RemoveVMDir(dir, second); err == nil {
		t.Fatal("a second account removed the first's VM directory")
	}
	mustExist(t, disk)
	// The first account's still opens, and it may remove it.
	_, release, err = f.VMDir(key, first)
	if err != nil {
		t.Fatalf("VMDir again for the first account: %v", err)
	}
	release()
	if err := f.RemoveVMDir(dir, first); err != nil {
		t.Fatalf("RemoveVMDir for the first account: %v", err)
	}
	if _, err := os.Lstat(dir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the VM directory is still there (%v)", err)
	}
}

func fileAttrs(t *testing.T, p string) uint32 {
	t.Helper()
	attrs, err := windows.GetFileAttributes(windows.StringToUTF16Ptr(p))
	if err != nil {
		t.Fatal(err)
	}
	return attrs
}
