//go:build windows

package hyperv

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"strings"
	"unsafe"

	"golang.org/x/sys/windows"

	"github.com/kivali-ai/kivali/internal/supervisor/broker"
)

// plainPath is p without the \\?\ prefix Windows puts on a canonical
// local path (Tauri's resource directory arrives that way), which the
// broker refuses (broker.localAbs) and host.DirKey would hash as a
// different directory. Only a local drive's prefix is removed; a UNC or
// device path is returned as it is.
func plainPath(p string) string {
	if strings.HasPrefix(p, `\\?\`) && len(p) >= 7 && p[5] == ':' && p[6] == '\\' {
		return p[4:]
	}
	return p
}

// What the config directory, the data disk's directory, must grant
// before the broker is asked to attach the disk. Hyper-V does the
// attaching as the broker service's account, and when it finds the
// directory short of either entry it writes the directory a DACL of its
// own: without the entries the directory inherits from the profile, so
// the person who owns it can no longer create a file in it (local.json,
// the logs), and `up` fails right after the VM is ready. Measured by
// trying each entry alone and both: with both present Hyper-V leaves
// the directory as it is.
//
//   - The broker service's account: READ_CONTROL to read the directory's
//     security descriptor, WRITE_DAC to add the virtual machine's own
//     entry to it, and to list and traverse it.
//   - NT VIRTUAL MACHINE\Virtual Machines (S-1-5-83-0, every VM's worker
//     process): read, write-data and append-data on the directory, the
//     entry Hyper-V would add itself. Read alone is not enough.
//
// Neither entry is inherited by what is in the directory; the data disk
// carries its own, which the broker set when it created the disk.
const (
	brokerFolderRights   = windows.READ_CONTROL | windows.WRITE_DAC | windows.FILE_LIST_DIRECTORY | windows.FILE_READ_ATTRIBUTES | windows.FILE_TRAVERSE
	machinesFolderRights = windows.FILE_GENERIC_READ | windows.FILE_WRITE_DATA | windows.FILE_APPEND_DATA
	// VirtualMachinesSID is the group every Hyper-V virtual machine's
	// worker process belongs to.
	VirtualMachinesSID = "S-1-5-83-0"
)

// grantBrokerFolder adds to dir's DACL the entries above that it lacks.
// The config directory is the supervisor's own, under the person's
// profile, where neither account has an entry by default. A directory
// that does not exist is left alone: the broker refuses a disk in it
// anyway.
func grantBrokerFolder(dir string) error {
	if _, err := os.Stat(dir); errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	sd, err := windows.GetNamedSecurityInfo(dir, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		return fmt.Errorf("read %s's permissions: %w", dir, err)
	}
	dacl, _, err := sd.DACL()
	if err != nil {
		return fmt.Errorf("read %s's permissions: %w", dir, err)
	}
	var entries []windows.EXPLICIT_ACCESS
	for _, want := range []struct {
		sid    string
		rights windows.ACCESS_MASK
	}{{broker.ServiceSID, brokerFolderRights}, {VirtualMachinesSID, machinesFolderRights}} {
		sid, err := windows.StringToSid(want.sid)
		if err != nil {
			return fmt.Errorf("SID %s: %w", want.sid, err)
		}
		if grants(dacl, sid, want.rights) {
			continue
		}
		entries = append(entries, windows.EXPLICIT_ACCESS{
			AccessPermissions: want.rights,
			AccessMode:        windows.GRANT_ACCESS,
			Inheritance:       windows.NO_INHERITANCE,
			Trustee: windows.TRUSTEE{
				TrusteeForm:  windows.TRUSTEE_IS_SID,
				TrusteeType:  windows.TRUSTEE_IS_UNKNOWN,
				TrusteeValue: windows.TrusteeValueFromSID(sid),
			},
		})
	}
	if len(entries) == 0 {
		return nil
	}
	merged, err := windows.ACLFromEntries(entries, dacl)
	if err != nil {
		return fmt.Errorf("add the broker to %s's permissions: %w", dir, err)
	}
	err = windows.SetNamedSecurityInfo(dir, windows.SE_FILE_OBJECT,
		windows.DACL_SECURITY_INFORMATION|windows.UNPROTECTED_DACL_SECURITY_INFORMATION, nil, nil, merged, nil)
	if err != nil {
		return fmt.Errorf("add the broker to %s's permissions: %w", dir, err)
	}
	return nil
}

// grants reports whether one access-allowed entry of acl gives sid all
// of rights. GetAce fails past the last entry, which ends the walk.
func grants(acl *windows.ACL, sid *windows.SID, rights windows.ACCESS_MASK) bool {
	if acl == nil {
		return false
	}
	for i := uint32(0); ; i++ {
		var ace *windows.ACCESS_ALLOWED_ACE
		if err := windows.GetAce(acl, i, &ace); err != nil {
			return false
		}
		if ace.Header.AceType != windows.ACCESS_ALLOWED_ACE_TYPE || ace.Mask&rights != rights {
			continue
		}
		if (*windows.SID)(unsafe.Pointer(&ace.SidStart)).Equals(sid) {
			return true
		}
	}
}
