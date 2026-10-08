package store

import (
	"os"
	"testing"
)

func mustStore(t *testing.T) *FSStore {
	t.Helper()
	s, err := New(t.TempDir())
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	return s
}

func isDir(path string) bool {
	fi, err := os.Stat(path)
	return err == nil && fi.IsDir()
}
