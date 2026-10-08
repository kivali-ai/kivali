package supervisor

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// DestroyRequest is the body of POST /v1/destroy.
type DestroyRequest struct {
	// Exit also ends `serve` once the org is deleted.
	Exit bool `json:"exit,omitempty"`
}

// DestroyResult is the answer to a destroy.
type DestroyResult struct {
	// FreedBytes is what the deleted files occupied (allocated bytes).
	FreedBytes int64 `json:"freed_bytes"`
}

// destroyRefusal refuses a destroy while an upgrade runs or its journal
// is open: the journal's recovery needs the snapshot and the disk.
func (s *Supervisor) destroyRefusal() error {
	if j := s.State().Upgrade; j != nil {
		return fmt.Errorf("an upgrade journal from %s to %s is open at step %s; run `up` to recover it before deleting the org", j.From, j.To, j.Step)
	}
	if s.busy() == "upgrade" {
		return errors.New("an upgrade is running; delete the org once it has finished")
	}
	return nil
}

// Destroy deletes the local org: it cancels a running up, install or
// load-images as Down does, stops the VM cleanly, removes the VM itself
// where the backend's outlives serve (MachineRemover), and removes every
// file the supervisor made in the config directory (the data disk, its
// snapshot, downloads/, local.json, the logs), except the RPC endpoint
// and serve.lock it is still serving on. The directory itself and any
// file the supervisor does not know stay. ctx bounds only the wait for
// the operation lock; once the lock is taken the destroy runs to the end.
func (s *Supervisor) Destroy(ctx context.Context, logf Logf) (DestroyResult, error) {
	setStage(ctx, StageDeleting)
	logf = s.tee(ctx, logf)
	if err := s.destroyRefusal(); err != nil {
		return DestroyResult{}, err
	}
	if s.cancelRunning(logf) {
		ctx = context.WithoutCancel(ctx)
	}
	ctx, end, err := s.begin(ctx, "destroy", false)
	if err != nil {
		return DestroyResult{}, fmt.Errorf("stopped waiting: %w", err)
	}
	defer end()
	ctx = context.WithoutCancel(ctx)
	if err := s.destroyRefusal(); err != nil {
		return DestroyResult{}, err
	}
	if m, _ := s.current(); m != nil {
		if err := s.stopLocked(ctx, logf); err != nil {
			// The stop's own bound hard-stops the VM: it is down either way.
			logf("stop: %v", err)
		}
	}
	// A VM that outlives serve (MachineRemover) goes with the org, even
	// one this serve never booted. It is removed while its disks are
	// still there to identify it, and a failure leaves only the VM: the
	// files are deleted all the same, and the line says what is left.
	if r, ok := s.o.Backend.(MachineRemover); ok {
		logf("removing the VM")
		if err := r.RemoveMachine(s.o.ConfigDir); err != nil {
			logf("remove the VM: %v", err)
		}
	}

	var res DestroyResult
	var failed []string
	remove := func(p string, fatal bool) {
		n, err := removeCounting(s.o.Host, p)
		res.FreedBytes += n
		if err != nil {
			logf("delete %s: %v", p, err)
			if fatal {
				failed = append(failed, p)
			}
		}
	}
	dir := s.o.ConfigDir
	logf("deleting the data disk %s", s.dataPath())
	remove(s.dataPath(), true)
	if exists(s.snapshotPath()) {
		logf("deleting the upgrade snapshot %s", s.snapshotPath())
		remove(s.snapshotPath(), true)
	}
	if d := s.dataDir(); filepath.Clean(d) != filepath.Clean(dir) {
		_ = os.Remove(d) // only if empty: the backend's own directory
	}
	if exists(filepath.Join(dir, "downloads")) {
		logf("deleting the downloaded releases")
		remove(filepath.Join(dir, "downloads"), true)
	}
	logf("deleting local.json and the logs")
	remove(filepath.Join(dir, "local.json"), true)
	// writeFileAtomic's and BackupToFile's temporary files, if a crash
	// left any.
	for _, pat := range []string{".tmp-local.json-*", ".kivali-backup-*"} {
		ms, _ := filepath.Glob(filepath.Join(dir, pat))
		for _, m := range ms {
			remove(m, false)
		}
	}
	logs := filepath.Join(dir, "logs")
	for _, f := range []string{"console.log", "console.log.1", "supervisor.log"} {
		// serve's own log may still be open (on Windows it cannot go).
		remove(filepath.Join(logs, f), false)
	}
	_ = os.Remove(logs) // only if empty
	s.mu.Lock()
	s.state = defaultState()
	s.mu.Unlock()
	if len(failed) > 0 {
		return res, fmt.Errorf("could not delete %s", strings.Join(failed, ", "))
	}
	logf("the org is deleted (%d MiB freed)", res.FreedBytes>>20)
	return res, nil
}

// removeCounting removes p (a file, or a directory and everything in
// it) and returns the allocated bytes its files held. A missing p is no
// error.
func removeCounting(h Host, p string) (int64, error) {
	var n int64
	err := filepath.WalkDir(p, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.Type().IsRegular() {
			if used, _, err := h.FileUsage(path); err == nil {
				n += used
			}
		}
		return nil
	})
	if errors.Is(err, fs.ErrNotExist) {
		return 0, nil
	}
	if err := os.RemoveAll(p); err != nil {
		return 0, err
	}
	return n, nil
}
