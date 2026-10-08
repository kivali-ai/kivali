package host

import (
	"crypto/sha256"
	"encoding/hex"
	"path/filepath"
	"strings"
)

// DirKey is the key of a config directory given as an absolute path:
// the first 8 hex characters of the SHA-256 of
// strings.ToLower(filepath.Clean(absDir)). The RPC pipe, the Hyper-V
// VM, its console pipe and its MAC all derive from it, on both sides of
// the broker. It is pure, so every OS tests it.
func DirKey(absDir string) string {
	sum := sha256.Sum256([]byte(strings.ToLower(filepath.Clean(absDir))))
	return hex.EncodeToString(sum[:])[:8]
}

// PipeName is the Windows RPC named pipe of a config directory given as
// an absolute path: `\\.\pipe\kivali-` and its DirKey. The shell
// computes the same name; keep the two in step.
func PipeName(absDir string) string {
	return `\\.\pipe\kivali-` + DirKey(absDir)
}
