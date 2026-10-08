package agentpod

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// The per-agent PVC is split between the pod's two containers, and
// neither can reach the other's half.
//
// The agent container runs the claude CLI with the org's credentials
// mounted beside it, and it keeps its runtime state on the PVC: the
// CLI's --mcp-config files (the session dir and every subagent's run
// dir), and the read cache of role, memory, skill and attachment
// bytes. The dev-shell container runs whatever the agent's run_shell
// asks for. A shell that could reach the agent's half could rewrite an
// mcp-config (whose "command" the CLI executes) or plant a link in the
// cache, and so run code, or feed bytes, in the container that holds
// the credentials.
//
// So each mounts its own subdirectory of the volume at /scratch, and
// neither mounts the volume's root: ScratchAgentSubPath in the agent
// container, ScratchShellSubPath in the dev-shell, where
// $HOME=/scratch/home. Nothing passes
// between the two through the volume: the dev-shell writes only its
// own home (the BASH_ENV shim included) and whatever run_shell makes,
// and the agent runtime reads only what it wrote itself. They meet at
// the dev-shell socket (an emptyDir) and nowhere else.
//
// PrepareScratch runs in the pod's init container, the only thing that
// mounts the whole volume, before either container starts.
const (
	ScratchAgentSubPath = ".kivali-agent"
	ScratchShellSubPath = ".kivali-shell"
	// ScratchVolumeMountPath is where the init container mounts the
	// whole volume.
	ScratchVolumeMountPath = "/volume"
)

// scratchTrashPrefix names what PrepareScratch sets aside for removal:
// the previous agent half, and anything it could not delete in place.
const scratchTrashPrefix = ".kivali-trash-"

// PrepareScratch lays out the per-agent volume mounted at root. It is
// idempotent, and runs at every pod start:
//
//   - The agent half starts empty, every time. All of it is
//     regenerable: core holds the truth, the cache refills on the next
//     read, the run dirs are per turn.
//   - The shell half is kept: the shell's home (pip --user installs,
//     dotfiles, the BASH_ENV shim) and anything run_shell left in
//     /scratch.
//   - Both halves are real directories: kubelet refuses a subPath with
//     a link in it, and the pod would never start.
//
// Only a failure to establish the two directories is an error; a
// set-aside half that cannot be removed is logged and left at the
// volume's root, which neither container mounts. Any other entry at
// the root is left alone.
func PrepareScratch(root string, logf func(format string, args ...any)) error {
	if logf == nil {
		logf = func(string, ...any) {}
	}
	info, err := os.Stat(root)
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return fmt.Errorf("%s is not a directory", root)
	}
	// Every rename below needs to write the root.
	ensureOwnerRWX(root, info, logf)

	// Set the old agent half aside first, then make a fresh one. A
	// rename within one directory needs no permission on the entry
	// itself, so this works even on a tree the shell made read-only.
	if _, err := os.Lstat(filepath.Join(root, ScratchAgentSubPath)); err == nil {
		if err := setAside(root, ScratchAgentSubPath); err != nil {
			return fmt.Errorf("set aside %s: %w", ScratchAgentSubPath, err)
		}
	} else if !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	if err := os.Mkdir(filepath.Join(root, ScratchAgentSubPath), 0o755); err != nil {
		return fmt.Errorf("make %s: %w", ScratchAgentSubPath, err)
	}

	// The shell half is kept when it is a directory. Anything else by
	// that name (a link, a file) is set aside.
	shell := filepath.Join(root, ScratchShellSubPath)
	sinfo, err := os.Lstat(shell)
	switch {
	case err == nil && sinfo.IsDir():
	case err == nil:
		if err := setAside(root, ScratchShellSubPath); err != nil {
			return fmt.Errorf("set aside %s: %w", ScratchShellSubPath, err)
		}
		fallthrough
	case errors.Is(err, fs.ErrNotExist):
		if err := os.Mkdir(shell, 0o755); err != nil {
			return fmt.Errorf("make %s: %w", ScratchShellSubPath, err)
		}
	default:
		return err
	}

	// Sweep what was set aside, this run's and any an earlier run could
	// not finish.
	return eachEntry(root, func(name string) {
		if !strings.HasPrefix(name, scratchTrashPrefix) {
			return
		}
		if err := removeTrash(filepath.Join(root, name), logf); err != nil {
			logf("scratch: remove %s: %v", name, err)
		}
	})
}

// scratchReadBatch is how many directory entries PrepareScratch holds
// at once. The init container's memory is small, and a directory being
// removed can hold any number of entries.
const scratchReadBatch = 1024

// eachEntry calls fn with the name of each entry of dir, reading the
// directory scratchReadBatch entries at a time. fn may rename or remove
// the entry it is given; an entry added to dir meanwhile may or may not
// be seen.
func eachEntry(dir string, fn func(name string)) error {
	f, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	for {
		entries, err := f.ReadDir(scratchReadBatch)
		for _, e := range entries {
			fn(e.Name())
		}
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
	}
}

// ensureOwnerRWX gives the owner of dir read, write and search on it
// when it lacks them, as far as this process may: the shell could have
// taken them away from anything it made. Root needs none of them, so
// nothing is changed when running as root.
func ensureOwnerRWX(dir string, info fs.FileInfo, logf func(format string, args ...any)) {
	perm := info.Mode().Perm()
	if perm&0o700 == 0o700 || os.Geteuid() == 0 {
		return
	}
	if err := os.Chmod(dir, perm|0o700); err != nil {
		logf("scratch: chmod u+rwx %s: %v", dir, err)
	}
}

// removeTrash removes p and everything beneath it without following a
// link. A directory the shell made unwritable is given u+rwx on the way
// down, which os.RemoveAll does not do. Each directory is read
// scratchReadBatch entries at a time.
func removeTrash(p string, logf func(format string, args ...any)) error {
	if err := os.RemoveAll(p); err == nil {
		return nil
	}
	info, err := os.Lstat(p)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if info.IsDir() {
		ensureOwnerRWX(p, info, logf)
		// Reopened for every batch: the batch just read is removed, so
		// each pass makes progress or fails.
		for {
			f, err := os.Open(p)
			if err != nil {
				return err
			}
			entries, rerr := f.ReadDir(scratchReadBatch)
			_ = f.Close()
			if rerr != nil && !errors.Is(rerr, io.EOF) {
				return rerr
			}
			if len(entries) == 0 {
				break
			}
			for _, e := range entries {
				if err := removeTrash(filepath.Join(p, e.Name()), logf); err != nil {
					return err
				}
			}
		}
	}
	return os.Remove(p)
}

// setAside renames root/name to a fresh trash name in root, for the
// sweep at the end of PrepareScratch to remove.
func setAside(root, name string) error {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return err
	}
	return os.Rename(filepath.Join(root, name), filepath.Join(root, scratchTrashPrefix+hex.EncodeToString(b[:])))
}
