package agentshell

import (
	"os"
	"path/filepath"
	"testing"
)

// Why this test exists: the socket's directory is writable by every uid
// in the pod, so without the sticky bit a command could replace the
// daemon's socket with one of its own and read the git jobs the agent's
// MCP server sends, the owner's token in them. ProtectSocketDir must
// set the bit and keep the directory's permissions, leave a directory
// that has it as it is, and fail on anything that is not a directory.
func TestProtectSocketDir(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "sock")
	if err := os.Mkdir(dir, 0o777); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0o777); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := ProtectSocketDir(dir); err != nil {
			t.Fatal(err)
		}
		fi, err := os.Stat(dir)
		if err != nil {
			t.Fatal(err)
		}
		if fi.Mode()&os.ModeSticky == 0 || fi.Mode().Perm() != 0o777 {
			t.Fatalf("mode = %v", fi.Mode())
		}
	}
	file := filepath.Join(dir, "f")
	if err := os.WriteFile(file, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if ProtectSocketDir(file) == nil || ProtectSocketDir(filepath.Join(dir, "missing")) == nil {
		t.Error("a file or a missing path was accepted")
	}
}
