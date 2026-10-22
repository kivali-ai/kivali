package agentshell

import (
	"fmt"
	"os"
)

// ProtectSocketDir sets the sticky bit on dir, the socket's directory:
// an emptyDir both containers mount, which every uid may write. With it
// set, only an entry's owner (or root) may remove or replace the entry,
// so a command, which runs as 65532, cannot move the daemon's socket
// (65533's) aside and listen in its place to read what the agent's MCP
// server sends the daemon: a git job's token among it. The daemon calls
// it as root, before it drops, when it owns the directory.
func ProtectSocketDir(dir string) error {
	fi, err := os.Stat(dir)
	if err != nil {
		return err
	}
	if !fi.IsDir() {
		return fmt.Errorf("%s is not a directory", dir)
	}
	if fi.Mode()&os.ModeSticky != 0 {
		return nil
	}
	return os.Chmod(dir, fi.Mode().Perm()|os.ModeSticky)
}
