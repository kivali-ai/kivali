//go:build windows

package broker

import (
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"golang.org/x/sys/windows"
)

// These run as the person running the tests, not as LocalSystem: they
// exercise the file checks and the link-safe deletion, and every
// refusal an ordinary account can arrange. Making the VM directory as
// the broker and deleting one it made are the spike's.

// canonical is dir's final path, so the canonical comparisons hold
// whatever form the temporary directory's path takes.
func canonical(t *testing.T, dir string) string {
	t.Helper()
	h, canon, err := openDir(dir, windows.FILE_LIST_DIRECTORY)
	if err != nil {
		t.Fatal(err)
	}
	_ = windows.CloseHandle(h)
	return canon
}

// junction makes link a junction to the directory target, which an
// ordinary account may do (a symbolic link needs a privilege).
func junction(t *testing.T, link, target string) {
	t.Helper()
	if out, err := exec.Command("cmd.exe", "/c", "mklink", "/J", link, target).CombinedOutput(); err != nil {
		t.Skipf("mklink /J: %v: %s", err, out)
	}
}

func write(t *testing.T, p, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func mustExist(t *testing.T, p string) {
	t.Helper()
	if _, err := os.Lstat(p); err != nil {
		t.Errorf("%s: %v", p, err)
	}
}

func ourSID(t *testing.T) string {
	t.Helper()
	u, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		t.Fatal(err)
	}
	return u.User.Sid.String()
}

// removeTree deletes a directory's whole tree through handles, and a
// junction inside it as the junction: what it points at, outside the
// tree, is untouched.
func TestRemoveTreeDeletesLinksNotTheirTargets(t *testing.T) {
	base := canonical(t, t.TempDir())
	outside := filepath.Join(base, "outside")
	write(t, filepath.Join(outside, "keep.txt"), "keep")
	vm := filepath.Join(base, "vm")
	write(t, filepath.Join(vm, "Virtual Machines", "id.vmcx"), "config")
	write(t, filepath.Join(vm, "root-child.vhdx"), "child")
	write(t, filepath.Join(vm, "sub", "deep", "f"), "f")
	if err := os.Chmod(filepath.Join(vm, "sub", "deep", "f"), 0o400); err != nil { // read-only
		t.Fatal(err)
	}
	junction(t, filepath.Join(vm, "link"), outside)
	junction(t, filepath.Join(vm, "sub", "link"), outside)

	h, canon, err := openDir(vm, windows.DELETE|windows.FILE_LIST_DIRECTORY|windows.FILE_READ_ATTRIBUTES)
	if err != nil {
		t.Fatal(err)
	}
	err = removeTree(canon, h)
	_ = windows.CloseHandle(h)
	if err != nil {
		t.Fatalf("removeTree: %v", err)
	}
	if _, err := os.Lstat(vm); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("%s is still there (%v)", vm, err)
	}
	mustExist(t, filepath.Join(outside, "keep.txt"))
}

// RemoveVMDir refuses, before deleting anything, a junction in the VM
// directory's place, a path that runs through one, and a directory the
// caller made rather than the broker.
func TestRemoveVMDirRefusals(t *testing.T) {
	base := canonical(t, t.TempDir())
	real := filepath.Join(base, "real")
	write(t, filepath.Join(real, "vm", "f"), "f")
	write(t, filepath.Join(real, "f"), "f")
	junction(t, filepath.Join(base, "j"), real)

	for _, tc := range []struct {
		why  string
		dir  string
		want error
	}{
		{"a junction in its place", filepath.Join(base, "j"), errIsReparse},
		{"a junction along its path", filepath.Join(base, "j", "vm"), errReparseOnPath},
		{"a directory the caller made", filepath.Join(real, "vm"), errNotBrokerDir},
	} {
		err := (winFiles{}).RemoveVMDir(tc.dir, "")
		if !errors.Is(err, tc.want) {
			t.Errorf("%s: err = %v, want %v", tc.why, err, tc.want)
		}
	}
	mustExist(t, filepath.Join(real, "f"))
	mustExist(t, filepath.Join(real, "vm", "f"))
	mustExist(t, filepath.Join(base, "j"))
}

// VMDir refuses a root the caller made first (the broker would create
// files under it as itself), and an ordinary account cannot make one
// the broker's: naming the Administrators group as owner is refused to
// it, so nothing is created.
func TestVMDirOnlyTheBrokers(t *testing.T) {
	if windows.GetCurrentProcessToken().IsElevated() {
		t.Skip("the test process is elevated, and may name the Administrators group as owner")
	}
	sid := ourSID(t)
	const key = "0123abcd"

	made := canonical(t, t.TempDir())
	if _, release, err := (winFiles{Root: made}).VMDir(key, sid); !errors.Is(err, errNotBrokerDir) {
		if err == nil {
			release()
		}
		t.Fatalf("a root the caller made: err = %v", err)
	}
	if _, err := os.Lstat(filepath.Join(made, key)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("a VM directory was made under the caller's root (%v)", err)
	}

	fresh := filepath.Join(canonical(t, t.TempDir()), "vm")
	_, release, err := (winFiles{Root: fresh}).VMDir(key, sid)
	if err == nil {
		release()
		t.Fatal("an ordinary account made a VM directory the broker would trust")
	}
	t.Logf("as this account: %v", err)
	if _, err := os.Lstat(fresh); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("a root was left behind (%v)", err)
	}
}

// Contents reads a file File pinned, through the pinning handle, at any
// offset, and nothing File did not pin.
func TestContentsReadsThePinnedFile(t *testing.T) {
	server, _ := callerPipe(t)
	c := &winCaller{pipe: windows.Handle(server.Fd()), sid: ourSID(t)}
	defer func() { _ = c.Close() }()
	base := canonical(t, t.TempDir())
	p := filepath.Join(base, DataDiskName)
	write(t, p, "0123456789")
	canon, err := c.File(p)
	if err != nil {
		t.Fatalf("File: %v", err)
	}
	f, size, err := c.Contents(canon)
	if err != nil || size != 10 {
		t.Fatalf("Contents: size %d, %v", size, err)
	}
	b := make([]byte, 4)
	if n, err := f.ReadAt(b, 3); n != 4 || err != nil || string(b) != "3456" {
		t.Fatalf("ReadAt(3) = %d %q, %v", n, b, err)
	}
	if n, err := f.ReadAt(b, 8); n != 2 || !errors.Is(err, io.EOF) {
		t.Fatalf("ReadAt(8) = %d, %v", n, err)
	}
	other := filepath.Join(base, "other.vhdx")
	write(t, other, "x")
	if _, _, err := c.Contents(other); err == nil {
		t.Fatal("Contents read a file File did not pin")
	}
}
