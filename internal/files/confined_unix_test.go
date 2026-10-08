//go:build unix

package files

import (
	"errors"
	"path/filepath"
	"syscall"
	"testing"
)

// A FIFO is neither followed nor opened: the Lstat sees it is not a
// regular file, and the open (had a swap slipped one in) would not
// block waiting for a writer.
func TestOpenNoFollowRefusesAFIFO(t *testing.T) {
	alice, _, _ := confinedFixture(t)
	if err := syscall.Mkfifo(filepath.Join(alice, "artifacts", "private", "pipe"), 0o644); err != nil {
		t.Skipf("mkfifo: %v", err)
	}
	if _, err := OpenNoFollow(alice, "artifacts/private/pipe"); !errors.Is(err, ErrNotRegular) {
		t.Errorf("err = %v, want ErrNotRegular", err)
	}
}

// The backend's reads refuse a FIFO too, rather than hanging on it.
func TestBackendViewRefusesAFIFO(t *testing.T) {
	b := newBackend(t)
	if err := syscall.Mkfifo(filepath.Join(b.Root, "notes", "pipe"), 0o644); err != nil {
		t.Skipf("mkfifo: %v", err)
	}
	if _, err := b.View("/files/notes/pipe", ViewOptions{}); err == nil {
		t.Error("View of a FIFO succeeded, want an error")
	}
}
