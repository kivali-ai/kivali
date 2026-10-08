package host

import (
	"io"
	"os"
	"path/filepath"

	"golang.org/x/sys/unix"
)

// DefaultConfigDir is $XDG_DATA_HOME/kivali, or ~/.local/share/kivali.
func (OS) DefaultConfigDir() (string, error) {
	if d := os.Getenv("XDG_DATA_HOME"); filepath.IsAbs(d) {
		return filepath.Join(d, "kivali"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".local", "share", "kivali"), nil
}

// Snapshot copies src to dst and syncs it; it fails if dst exists. The
// kernel's copy_file_range (which io.Copy uses between files) shares
// extents on file systems that can and keeps holes on most others.
func (OS) Snapshot(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer func() { _ = in.Close() }()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()
		_ = os.Remove(dst)
		return err
	}
	if err := out.Sync(); err != nil {
		_ = out.Close()
		return err
	}
	return out.Close()
}

// SyncDir makes a directory's entries durable.
func (OS) SyncDir(dir string) error {
	f, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	return f.Sync()
}

// ExcludeFromBackup is a no-op: Linux has no system-wide backup with an
// exclusion attribute.
func (OS) ExcludeFromBackup(string) error { return nil }

// FreeBytes is the space in dir's file system available to this user.
func (OS) FreeBytes(dir string) (uint64, error) {
	var st unix.Statfs_t
	if err := unix.Statfs(dir, &st); err != nil {
		return 0, err
	}
	return st.Bavail * uint64(st.Bsize), nil
}
