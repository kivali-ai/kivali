package guestapi

import (
	"strconv"
	"strings"
)

// DataDiskHealth is what the guest knows of its data disk's filesystem,
// read at each status. Anything but the zero value means the disk has
// been damaged or is failing, and someone should know before more is
// written to it.
type DataDiskHealth struct {
	// ReadOnly: the filesystem is mounted read-only. It is mounted
	// errors=remount-ro, so this is ext4 having met an error: every write
	// fails until the VM restarts and e2fsck runs.
	ReadOnly bool `json:"read_only,omitempty"`
	// Errors is the ext4 error count in the superblock, kept across boots
	// until e2fsck clears it.
	Errors int `json:"errors,omitempty"`
	// FsckRepaired: this boot's e2fsck found and fixed damage, beyond
	// replaying the journal (which an unclean stop alone leaves);
	// FsckDetail is what it said.
	FsckRepaired bool   `json:"fsck_repaired,omitempty"`
	FsckDetail   string `json:"fsck_detail,omitempty"`
}

// Problem is a person's sentence for h, or "" when it is healthy.
func (h DataDiskHealth) Problem() string {
	var p []string
	if h.ReadOnly {
		p = append(p, "the data disk's filesystem met an error and is read-only: nothing can be saved until the VM restarts")
	}
	if h.Errors > 0 {
		p = append(p, "ext4 has recorded "+strconv.Itoa(h.Errors)+" error(s) on the data disk")
	}
	if h.FsckRepaired {
		s := "e2fsck repaired damage on the data disk at this boot"
		if h.FsckDetail != "" {
			s += " (" + h.FsckDetail + ")"
		}
		p = append(p, s)
	}
	return strings.Join(p, "; ")
}
