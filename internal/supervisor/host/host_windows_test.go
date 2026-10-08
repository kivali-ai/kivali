package host

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

// A sparse file with a hole and an allocated tail snapshots to a file
// with the same bytes that is sparse too, with only the tail allocated.
func TestSnapshotKeepsASparseFileSparse(t *testing.T) {
	dir := t.TempDir()
	src, dst := filepath.Join(dir, "disk"), filepath.Join(dir, "disk.snap")
	const size = 64 << 20
	tail := bytes.Repeat([]byte("kivali"), 10000)
	f, err := os.Create(src)
	if err != nil {
		t.Fatal(err)
	}
	if err := setSparse(f); err != nil {
		t.Fatal(err)
	}
	if err := f.Truncate(size); err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteAt(tail, size-int64(len(tail))); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	if err := Default().Snapshot(src, dst); err != nil {
		t.Fatal(err)
	}
	want, _ := os.ReadFile(src)
	got, err := os.ReadFile(dst)
	if err != nil || !bytes.Equal(got, want) {
		t.Fatalf("snapshot differs (%d bytes, %v)", len(got), err)
	}
	if !isSparse(dst) {
		t.Fatal("snapshot is not sparse")
	}
	out, err := os.Open(dst)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = out.Close() }()
	ranges, err := allocatedRanges(out, size)
	if err != nil {
		t.Fatal(err)
	}
	var allocated int64
	for _, r := range ranges {
		allocated += r.Length
	}
	if allocated == 0 || allocated >= size/2 {
		t.Fatalf("snapshot allocates %d of %d bytes", allocated, size)
	}
}
