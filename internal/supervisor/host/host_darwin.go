package host

import (
	"os"
	"path/filepath"

	"golang.org/x/sys/unix"
)

// DefaultConfigDir is ~/Library/Application Support/Kivali.
func (OS) DefaultConfigDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, "Library", "Application Support", "Kivali"), nil
}

// Snapshot makes dst an APFS copy-on-write clone of src: instant, and
// free until either file's blocks diverge. It fails if dst exists.
func (OS) Snapshot(src, dst string) error {
	return unix.Clonefile(src, dst, unix.CLONE_NOFOLLOW)
}

// SyncDir makes a directory's entries durable. On macOS fsync only
// reaches the drive's cache; F_FULLFSYNC asks the drive to flush it, so
// a clone or rename recorded in the journal survives a power cut.
func (OS) SyncDir(dir string) error {
	f, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	if _, err := unix.FcntlInt(f.Fd(), unix.F_FULLFSYNC, 0); err != nil {
		return f.Sync()
	}
	return nil
}

// backupExcludeValue is the binary plist of the string
// "com.apple.backupd", the value macOS itself stores in the
// com.apple.metadata:com_apple_backup_excludeItem attribute when an item
// is excluded from Time Machine (NSURLIsExcludedFromBackupKey,
// CSBackupSetItemExcluded, `tmutil addexclusion` without -p).
var backupExcludeValue = []byte{
	0x62, 0x70, 0x6c, 0x69, 0x73, 0x74, 0x30, 0x30, 0x5f, 0x10, 0x11, 0x63,
	0x6f, 0x6d, 0x2e, 0x61, 0x70, 0x70, 0x6c, 0x65, 0x2e, 0x62, 0x61, 0x63,
	0x6b, 0x75, 0x70, 0x64, 0x08, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x01,
	0x01, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x01, 0x00, 0x00, 0x00,
	0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00,
	0x1c,
}

// ExcludeFromBackup marks p as excluded from Time Machine with the
// sticky exclusion attribute: it travels with the item (renames,
// clones), needs no root and no subprocess, unlike `tmutil addexclusion
// -p`, which edits the system-wide path list and needs root.
func (OS) ExcludeFromBackup(p string) error {
	return unix.Setxattr(p, "com.apple.metadata:com_apple_backup_excludeItem", backupExcludeValue, 0)
}

// FreeBytes is the space in dir's file system available to this user.
func (OS) FreeBytes(dir string) (uint64, error) {
	var st unix.Statfs_t
	if err := unix.Statfs(dir, &st); err != nil {
		return 0, err
	}
	return st.Bavail * uint64(st.Bsize), nil
}
