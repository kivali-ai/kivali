package host

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// These run against the real host of whichever OS runs them: the macOS
// suite here, the Windows suite on the Windows runner.

func TestLockServeIsExclusive(t *testing.T) {
	h := Default()
	dir := t.TempDir()
	release, err := h.LockServe(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.LockServe(dir); !errors.Is(err, ErrServeRunning) {
		t.Fatalf("second lock: %v", err)
	}
	release()
	again, err := h.LockServe(dir)
	if err != nil {
		t.Fatalf("after release: %v", err)
	}
	again()
}

func TestListenDialRoundTrip(t *testing.T) {
	h := Default()
	dir := t.TempDir()
	ln, err := h.Listen(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ln.Close() }()
	if runtime.GOOS != "windows" {
		if fi, err := os.Stat(h.Endpoint(dir)); err != nil || fi.Mode().Perm() != 0o600 {
			t.Fatalf("socket mode %v %v", fi, err)
		}
	}
	served := make(chan error, 1)
	go func() {
		c, err := ln.Accept()
		if err != nil {
			served <- err
			return
		}
		defer func() { _ = c.Close() }()
		b := make([]byte, 5)
		if _, err := io.ReadFull(c, b); err != nil {
			served <- err
			return
		}
		_, err = c.Write(append([]byte("re:"), b...))
		served <- err
	}()
	c, err := h.Dial(context.Background(), dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()
	if _, err := c.Write([]byte("hello")); err != nil {
		t.Fatal(err)
	}
	got := make([]byte, 8)
	if _, err := io.ReadFull(c, got); err != nil || string(got) != "re:hello" {
		t.Fatalf("got %q, %v", got, err)
	}
	if err := <-served; err != nil {
		t.Fatal(err)
	}
	// One serve owns the endpoint: a second listener is refused while
	// the first answers.
	if second, err := h.Listen(dir); err == nil {
		_ = second.Close()
		t.Fatal("second listener on a live endpoint")
	}
}

func TestDialWithNoServer(t *testing.T) {
	if c, err := Default().Dial(context.Background(), t.TempDir()); err == nil {
		_ = c.Close()
		t.Fatal("dialled an endpoint nobody serves")
	}
}

func TestSnapshotCopiesAFile(t *testing.T) {
	h := Default()
	dir := t.TempDir()
	src, dst := filepath.Join(dir, "disk"), filepath.Join(dir, "disk.snap")
	body := bytes.Repeat([]byte("kivali "), 1000)
	if err := os.WriteFile(src, body, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := h.Snapshot(src, dst); err != nil {
		t.Fatal(err)
	}
	if got, err := os.ReadFile(dst); err != nil || !bytes.Equal(got, body) {
		t.Fatalf("snapshot differs (%d bytes, %v)", len(got), err)
	}
	// The snapshot is independent of its source.
	if err := os.WriteFile(src, []byte("changed"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(dst); !bytes.Equal(got, body) {
		t.Fatal("snapshot changed with its source")
	}
	if err := h.Snapshot(src, dst); err == nil {
		t.Fatal("snapshot over an existing file")
	}
}

func TestSyncDir(t *testing.T) {
	dir := t.TempDir()
	if err := os.Rename(writeTemp(t, dir), filepath.Join(dir, "renamed")); err != nil {
		t.Fatal(err)
	}
	if err := Default().SyncDir(dir); err != nil {
		t.Fatal(err)
	}
	if err := Default().SyncDir(filepath.Join(dir, "missing")); err == nil {
		t.Fatal("synced a missing directory")
	}
}

func TestRenameReplaces(t *testing.T) {
	dir := t.TempDir()
	dst := filepath.Join(dir, "local.json")
	if err := os.WriteFile(dst, []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}
	src := filepath.Join(dir, "new")
	if err := os.WriteFile(src, []byte("new"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := Default().Rename(src, dst); err != nil {
		t.Fatal(err)
	}
	if b, err := os.ReadFile(dst); err != nil || string(b) != "new" {
		t.Fatalf("after rename: %q %v", b, err)
	}
	if _, err := os.Stat(src); err == nil {
		t.Fatal("source still exists")
	}
	if err := Default().Rename(src, dst); err == nil {
		t.Fatal("renamed a missing file")
	}
}

func writeTemp(t *testing.T, dir string) string {
	t.Helper()
	p := filepath.Join(dir, "f")
	if err := os.WriteFile(p, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestExcludeFromBackup(t *testing.T) {
	if err := Default().ExcludeFromBackup(writeTemp(t, t.TempDir())); err != nil {
		t.Fatal(err)
	}
}

func TestFreeBytes(t *testing.T) {
	n, err := Default().FreeBytes(t.TempDir())
	if err != nil || n == 0 {
		t.Fatalf("free bytes %d, %v", n, err)
	}
}

func TestFileUsage(t *testing.T) {
	p := writeTemp(t, t.TempDir())
	fi, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	used, size, err := Default().FileUsage(p)
	if err != nil || size != fi.Size() || used < 0 {
		t.Fatalf("usage %d/%d, %v (size %d)", used, size, err, fi.Size())
	}
	// A sparse file allocates less than its size.
	sparse := filepath.Join(t.TempDir(), "sparse")
	f, err := os.Create(sparse)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Truncate(64 << 20); err != nil {
		t.Fatal(err)
	}
	_ = f.Close()
	used, size, err = Default().FileUsage(sparse)
	if err != nil || size != 64<<20 || used > size {
		t.Fatalf("sparse usage %d/%d, %v", used, size, err)
	}
	if _, _, err := Default().FileUsage(filepath.Join(t.TempDir(), "missing")); err == nil {
		t.Fatal("no error for a missing file")
	}
}

func TestAddrInUse(t *testing.T) {
	h := Default()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = l.Close() }()
	_, err = net.Listen("tcp", l.Addr().String())
	if err == nil || !h.AddrInUse(err) {
		t.Fatalf("bind to a held port: %v, AddrInUse %v", err, h.AddrInUse(err))
	}
	if h.AddrInUse(nil) || h.AddrInUse(errors.New("other")) {
		t.Fatal("AddrInUse of an unrelated error")
	}
}

func TestDefaultConfigDir(t *testing.T) {
	d, err := Default().DefaultConfigDir()
	if err != nil || !filepath.IsAbs(d) || !strings.EqualFold(filepath.Base(d), "kivali") {
		t.Fatalf("config dir %q, %v", d, err)
	}
}
