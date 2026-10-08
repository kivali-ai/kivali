package broker

import (
	"strings"
	"testing"
)

// Who may own the broker pipe a client talks to, and the VM directory
// the broker writes in and deletes from: only owners an ordinary account
// cannot set.
func TestBrokerOwned(t *testing.T) {
	for sid, want := range map[string]bool{
		ServiceSID:                  true,  // the service's own account
		LocalSystemSID:              true,  // a SYSTEM shell's console broker
		AdministratorsSID:           true,  // what an elevated console broker names
		"s-1-5-32-544":              true,  // SIDs compare without case
		"S-1-5-21-111-222-333-1001": false, // an ordinary account: a squatter, or the caller first
		"S-1-5-19":                  false, // LocalService
		"S-1-5-20":                  false, // NetworkService
		"S-1-5-32-545":              false, // Users
		"S-1-5-11":                  false, // Authenticated Users
		"S-1-1-0":                   false, // Everyone
		"S-1-5-18-1":                false, // a prefix is not the SID
		"":                          false,
	} {
		if got := brokerOwned(sid); got != want {
			t.Errorf("brokerOwned(%q) = %v, want %v", sid, got, want)
		}
	}
}

// The service's SID is derived from its name the way Windows derives
// every per-service SID (S-1-5-80 and the SHA-1 of the upper-cased
// name), checked against a service whose SID is documented:
// NT SERVICE\TrustedInstaller.
func TestServiceSID(t *testing.T) {
	if got, want := serviceSID("TrustedInstaller"), "S-1-5-80-956008885-3418522649-1831038044-1853292631-2271478464"; got != want {
		t.Fatalf("serviceSID(TrustedInstaller) = %s, want %s", got, want)
	}
	if ServiceSID != serviceSID("kivali-broker") || !strings.HasPrefix(ServiceSID, "S-1-5-80-") {
		t.Fatalf("ServiceSID = %s", ServiceSID)
	}
}
