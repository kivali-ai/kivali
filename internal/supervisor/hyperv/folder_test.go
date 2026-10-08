//go:build windows

package hyperv

import (
	"testing"

	"golang.org/x/sys/windows"

	"github.com/kivali-ai/kivali/internal/supervisor/broker"
)

func TestPlainPath(t *testing.T) {
	for in, want := range map[string]string{
		`\\?\C:\Program Files\Kivali\vm`: `C:\Program Files\Kivali\vm`,
		`C:\Program Files\Kivali\vm`:     `C:\Program Files\Kivali\vm`,
		`\\?\UNC\server\share\vm`:        `\\?\UNC\server\share\vm`,
		`\\?\Volume{1234}\x`:             `\\?\Volume{1234}\x`,
		`\\server\share`:                 `\\server\share`,
		`\\?\C:`:                         `\\?\C:`,
	} {
		if got := plainPath(in); got != want {
			t.Errorf("plainPath(%q) = %q, want %q", in, got, want)
		}
	}
}

// A fresh directory under the profile has no entry for the broker's
// account or the Virtual Machines group; after the grant it has both,
// its inherited entries are still there, and a second grant adds
// nothing.
func TestGrantBrokerFolder(t *testing.T) {
	dir := t.TempDir()
	brokerSID, err := windows.StringToSid(broker.ServiceSID)
	if err != nil {
		t.Fatal(err)
	}
	machinesSID, err := windows.StringToSid(VirtualMachinesSID)
	if err != nil {
		t.Fatal(err)
	}
	read := func() (*windows.ACL, int, int) {
		sd, err := windows.GetNamedSecurityInfo(dir, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION)
		if err != nil {
			t.Fatal(err)
		}
		dacl, _, err := sd.DACL()
		if err != nil {
			t.Fatal(err)
		}
		n, inherited := 0, 0
		for {
			var ace *windows.ACCESS_ALLOWED_ACE
			if windows.GetAce(dacl, uint32(n), &ace) != nil {
				break
			}
			if ace.Header.AceFlags&windows.INHERITED_ACE != 0 {
				inherited++
			}
			n++
		}
		return dacl, n, inherited
	}
	before, n0, i0 := read()
	if grants(before, brokerSID, brokerFolderRights) || grants(before, machinesSID, machinesFolderRights) {
		t.Skip("the temp directory already grants one of the accounts (an unusual profile)")
	}
	if err := grantBrokerFolder(dir); err != nil {
		t.Fatalf("grantBrokerFolder: %v", err)
	}
	after, n1, i1 := read()
	if !grants(after, brokerSID, brokerFolderRights) {
		t.Error("after the grant the directory does not grant the broker's account")
	}
	if !grants(after, machinesSID, machinesFolderRights) {
		t.Error("after the grant the directory does not grant the Virtual Machines group")
	}
	if n1 != n0+2 {
		t.Errorf("the grant added %d entries, want 2", n1-n0)
	}
	if i1 != i0 {
		t.Errorf("the grant changed the inherited entries from %d to %d", i0, i1)
	}
	if err := grantBrokerFolder(dir); err != nil {
		t.Fatalf("second grantBrokerFolder: %v", err)
	}
	if _, n2, _ := read(); n2 != n1 {
		t.Errorf("a second grant changed the entry count from %d to %d", n1, n2)
	}
	if err := grantBrokerFolder(dir + `\absent`); err != nil {
		t.Errorf("a directory that does not exist: %v", err)
	}
}
