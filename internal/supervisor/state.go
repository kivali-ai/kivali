package supervisor

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"time"
)

// DefaultFeed is the release feed of the public repository.
const DefaultFeed = "https://github.com/kivali-ai/kivali/releases/latest/download/release.json"

// Defaults for a new local org.
const (
	DefaultMemoryMB = 4096
	DefaultCPUs     = 4
	DefaultPort     = 8080
	// DataDiskSize is the data disk's size: it grows to this.
	DataDiskSize = 64 << 30
	// SnapshotSuffix makes the upgrade snapshot's name from the data
	// disk's, next to it.
	SnapshotSuffix = ".upgrade"
)

// State is local.json, the supervisor's own record of the local org
// (docs/developers/desktop-app.md, "Picker and persistence").
type State struct {
	// VMImage is the root disk's version as the guest reported it at
	// the last boot.
	VMImage string `json:"vm_image"`
	// Kivali is the installed chart's version ("" until installed).
	Kivali string `json:"kivali"`
	// DataDiskFormatted turns true only once the first boot on the disk
	// reported READY; until then the disk is disposable.
	DataDiskFormatted bool   `json:"data_disk_formatted"`
	MemoryMB          uint64 `json:"memory_mb"`
	CPUs              uint   `json:"cpus"`
	// Port is the host loopback port the org is forwarded to; 0 until
	// the first forward chose one (DefaultPort, or the shell's hint).
	Port        int        `json:"port"`
	Feed        string     `json:"feed"`
	LastCheck   *time.Time `json:"last_check"`
	LastCheckOK bool       `json:"last_check_ok"`
	// Latest is what the last successful check found.
	Latest *CheckResult `json:"latest,omitempty"`
	// Upgrade is the upgrade journal, nil when no upgrade is under way.
	Upgrade *Journal `json:"upgrade"`
}

// Upgrade journal steps, in order (docs/developers/supervisor.md).
const (
	StepPrepared     = "prepared"     // images imported, chart fetched; nothing touched
	StepQuiescing    = "quiescing"    // deleting agent pods, stopping the VM
	StepSnapshotting = "snapshotting" // cloning the data disk
	StepSnapshotted  = "snapshotted"  // the clone is complete
	StepApplying     = "applying"     // VM started on the new chart; waiting for it
	StepRollingBack  = "rolling-back" // putting the snapshot back
	StepCommitted    = "committed"    // the new release is up; only cleanup is left
)

// Journal is an upgrade in progress.
type Journal struct {
	From             string    `json:"from"`
	To               string    `json:"to"`
	Step             string    `json:"step"`
	Snapshot         string    `json:"snapshot"`
	SnapshotComplete bool      `json:"snapshot_complete"`
	Started          time.Time `json:"started"`
	Error            string    `json:"error,omitempty"`
}

func defaultState() State {
	return State{
		MemoryMB: DefaultMemoryMB,
		CPUs:     DefaultCPUs,
		Feed:     DefaultFeed,
	}
}

// port is the port the forward listens on: the recorded one, else
// DefaultPort.
func (st State) port() int {
	if st.Port == 0 {
		return DefaultPort
	}
	return st.Port
}

// LoadState reads local.json from dir. A missing file yields the
// defaults; if the data disk at dataDisk (the backend's path; "" when
// there is no backend) is nevertheless present it is treated as
// formatted, because the guest refuses (FATAL) a disk that is not a
// Kivali disk but a reformat would destroy one that is.
func LoadState(dir, dataDisk string) (State, error) {
	st := defaultState()
	b, err := os.ReadFile(filepath.Join(dir, "local.json"))
	if errors.Is(err, fs.ErrNotExist) {
		if dataDisk != "" {
			if _, err := os.Stat(dataDisk); err == nil {
				st.DataDiskFormatted = true
			}
		}
		return st, nil
	}
	if err != nil {
		return st, err
	}
	if err := json.Unmarshal(b, &st); err != nil {
		return st, fmt.Errorf("local.json: %w", err)
	}
	if st.MemoryMB == 0 {
		st.MemoryMB = DefaultMemoryMB
	}
	if st.CPUs == 0 {
		st.CPUs = DefaultCPUs
	}
	if st.Feed == "" {
		st.Feed = DefaultFeed
	}
	return st, nil
}

// SaveState writes local.json atomically and durably, owner-only: the
// journal steps in it must be on the disk before the step they record
// begins (a SnapshotComplete lost in a crash would make recovery delete
// the only good copy of the data disk).
func SaveState(h Host, dir string, st State) error {
	b, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}
	return writeFileAtomic(h, filepath.Join(dir, "local.json"), append(b, '\n'), 0o600)
}

// writeFileAtomic replaces p with b through a synced temporary file and
// the Host's durable Rename, which replaces an existing file on every OS
// (MoveFileEx on Windows). On Windows mode only sets or clears the read-only bit, and
// a read-only p could not be replaced: keep the owner's write bit in
// mode (every caller passes 0o600).
func writeFileAtomic(h Host, p string, b []byte, mode os.FileMode) (err error) {
	f, err := os.CreateTemp(filepath.Dir(p), ".tmp-"+filepath.Base(p)+"-*")
	if err != nil {
		return err
	}
	defer func() {
		if err != nil {
			_ = f.Close()
			_ = os.Remove(f.Name())
		}
	}()
	if _, err = f.Write(b); err != nil {
		return err
	}
	if err = f.Chmod(mode); err != nil {
		return err
	}
	if err = f.Sync(); err != nil {
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	return h.Rename(f.Name(), p)
}
