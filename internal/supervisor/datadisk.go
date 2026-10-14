package supervisor

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
)

// migrateSpare is the room a move of the data disk leaves free on the
// volume it moves to, beyond the copy itself: what the guest writes
// first, and the system's own needs.
const migrateSpare = 2 << 30

// migrateLocked moves the data disk from where an earlier version of the
// backend kept it (DataDiskMigrator) to where the backend keeps it now,
// before anything boots: the caller holds s.op, the VM is off and an
// upgrade journal is already recovered (recoverLocked puts an
// interrupted upgrade's snapshot back at the legacy path, so that is the
// disk moved). Nothing at the legacy path is nothing to do.
//
// The legacy file is never changed until its copy is in place: a
// volume short of room for the copy refuses the up at once; the backend
// copies the file and puts the copy in place (ImportDataDisk), or
// reports that an earlier up did (ErrAlreadyImported); only then is the
// legacy file renamed to MigratedSuffix, and it is deleted once the copy
// has booted to READY (dropMigratedLocked). A failure anywhere before
// that leaves it where it was, and the next up starts again.
//
// A legacy disk whose first boot never reached READY is disposable, as
// bootLocked treats one: it is not copied, and bootLocked makes a new
// disk in its place.
//
// TODO: remove migrateLocked, dropMigratedLocked, DataDiskMigrator and
// the broker's POST /v1/vhd/import once installs from before the broker
// kept the data disk have resumed under this one. Nothing else imports
// a disk (a new team's disk is made in place, and a backup is restored
// through the app), and the import is the broker's one read of a disk
// the caller wrote.
func (s *Supervisor) migrateLocked(ctx context.Context, logf Logf) error {
	m, ok := s.o.Backend.(DataDiskMigrator)
	if !ok {
		return nil
	}
	legacy := m.LegacyDataDiskPath(s.o.ConfigDir)
	if legacy == "" {
		return nil
	}
	fi, err := os.Stat(legacy)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("cannot check the data disk %s: %w", legacy, err)
	}
	setStage(ctx, StageUpdatingData)
	defer setStage(ctx, StageStarting)
	if s.State().DataDiskFormatted {
		used, _, err := s.o.Host.FileUsage(legacy)
		if err != nil {
			used = fi.Size()
		}
		dir := existingDir(s.dataDir())
		if free, err := s.o.Host.FreeBytes(dir); err == nil && free < uint64(used)+migrateSpare {
			need := uint64(used) + migrateSpare
			where := dir
			if v := filepath.VolumeName(dir); v != "" {
				where = "drive " + v
			}
			return fmt.Errorf("updating this team's data needs %s free on %s, and only %s is free. "+
				"Free up %s there, then resume the team; nothing was changed",
				gb(need), where, gb(free), gb(need-free))
		}
		logf("updating the team's data: copying the data disk %s (%s) to %s", legacy, gb(uint64(used)), s.dataPath())
		err = m.ImportDataDisk(ctx, s.o.ConfigDir, progressOf(ctx), logf)
		switch {
		case errors.Is(err, ErrAlreadyImported):
			logf("the data disk was already copied to %s", s.dataPath())
		case err != nil:
			return fmt.Errorf("update the team's data: %w; the data disk at %s is unchanged", err, legacy)
		}
	} else {
		logf("the data disk %s never finished its first boot; a new one is made in its place", legacy)
	}
	migrated := legacy + MigratedSuffix
	if err := s.o.Host.Rename(legacy, migrated); err != nil {
		return fmt.Errorf("set aside the data disk %s once copied: %w", legacy, err)
	}
	logf("the data disk is in its new place; %s is kept until the team has started", migrated)
	return nil
}

// dropMigratedLocked deletes the legacy data disk migrateLocked set
// aside, once the disk in its new place has booted to READY: before, it
// is the copy to go back to.
func (s *Supervisor) dropMigratedLocked(logf Logf) {
	m, ok := s.o.Backend.(DataDiskMigrator)
	if !ok {
		return
	}
	legacy := m.LegacyDataDiskPath(s.o.ConfigDir)
	if legacy == "" || !exists(legacy+MigratedSuffix) {
		return
	}
	if err := os.Remove(legacy + MigratedSuffix); err != nil {
		logf("delete %s: %v", legacy+MigratedSuffix, err)
		return
	}
	logf("deleted %s, the data disk's copy from before it moved", legacy+MigratedSuffix)
}

// existingDir is dir, or the nearest directory above it that exists: a
// directory a backend makes on first use measures the volume it will be
// on.
func existingDir(dir string) string {
	for d := dir; ; {
		if fi, err := os.Stat(d); err == nil && fi.IsDir() {
			return d
		}
		parent := filepath.Dir(d)
		if parent == d {
			return dir
		}
		d = parent
	}
}

// gb is n bytes as GB with one decimal, as the desktop app writes sizes.
func gb(n uint64) string { return fmt.Sprintf("%.1f GB", float64(n)/(1<<30)) }

// keeper is the backend's DataDiskKeeper, when the snapshot at p is the
// one it keeps (the backend's own snapshot of its data disk), else nil:
// a journal's snapshot elsewhere (where an earlier version kept the data
// disk) is the Host's to rename or remove.
func (s *Supervisor) keeper(p string) DataDiskKeeper {
	if k, ok := s.o.Backend.(DataDiskKeeper); ok && p == s.snapshotPath() {
		return k
	}
	return nil
}

// takeSnapshot copies the data disk to its snapshot, durably: the
// backend's (DataDiskKeeper) where it keeps the disk, else the Host's
// copy and SyncDir.
func (s *Supervisor) takeSnapshot(ctx context.Context) error {
	snap := s.snapshotPath()
	if k := s.keeper(snap); k != nil {
		return k.SnapshotDataDisk(ctx, s.o.ConfigDir)
	}
	if err := removeIfExists(snap); err != nil {
		return err
	}
	if err := s.o.Host.Snapshot(s.dataPath(), snap); err != nil {
		return err
	}
	// The snapshot must be durable before the journal says it is.
	return s.o.Host.SyncDir(s.dataDir())
}

// removeSnapshot deletes the snapshot at p; nothing there is no error.
func (s *Supervisor) removeSnapshot(ctx context.Context, p string) error {
	if k := s.keeper(p); k != nil {
		return k.DeleteDataDiskSnapshot(ctx, s.o.ConfigDir)
	}
	return removeIfExists(p)
}

// restoreTarget is the data disk the snapshot at p was taken of: where
// an earlier version kept it (DataDiskMigrator) when p is that one's
// snapshot, else the data disk.
func (s *Supervisor) restoreTarget(p string) string {
	if m, ok := s.o.Backend.(DataDiskMigrator); ok {
		if legacy := m.LegacyDataDiskPath(s.o.ConfigDir); legacy != "" && p == legacy+SnapshotSuffix {
			return legacy
		}
	}
	return s.dataPath()
}

// putSnapshotBack replaces the data disk the snapshot at p was taken of
// with it, durably.
func (s *Supervisor) putSnapshotBack(ctx context.Context, p string) error {
	if k := s.keeper(p); k != nil {
		return k.RestoreDataDiskSnapshot(ctx, s.o.ConfigDir)
	}
	return s.o.Host.Rename(p, s.restoreTarget(p))
}
