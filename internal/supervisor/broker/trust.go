package broker

import (
	"crypto/sha1"
	"encoding/binary"
	"errors"
	"fmt"
	"strings"
	"unicode/utf16"
)

// The well-known SIDs the ownership rule names.
const (
	// LocalSystemSID is LocalSystem, the account an elevated --console
	// broker may run as under a SYSTEM shell, and the account the
	// service ran as before it had an account of its own.
	LocalSystemSID = "S-1-5-18"
	// AdministratorsSID is the built-in Administrators group, which an
	// elevated --console broker names as the owner of what it creates.
	AdministratorsSID = "S-1-5-32-544"
	// HyperVAdministratorsSID is the built-in Hyper-V Administrators
	// group, whose members manage Hyper-V without being administrators;
	// the service's account is made a member at install.
	HyperVAdministratorsSID = "S-1-5-32-578"
)

// ServiceSID is the SID of the broker service's own account, the
// virtual account NT SERVICE\kivali-broker that the service runs as
// (serviceSID; ServiceName). It is the owner of everything the service
// creates, and the identity its Hyper-V rights are granted to.
var ServiceSID = serviceSID(ServiceName)

// serviceSID is the per-service SID Windows derives from a service's
// name: S-1-5-80 followed by the SHA-1 of the upper-cased name in
// UTF-16LE, as five little-endian 32-bit sub-authorities. It needs no
// lookup, so a client can know the broker's SID without the service
// existing, and it is the same on every machine.
func serviceSID(name string) string {
	u := utf16.Encode([]rune(strings.ToUpper(name)))
	b := make([]byte, 2*len(u))
	for i, c := range u {
		binary.LittleEndian.PutUint16(b[2*i:], c)
	}
	sum := sha1.Sum(b)
	s := "S-1-5-80"
	for i := 0; i < 5; i++ {
		s += fmt.Sprintf("-%d", binary.LittleEndian.Uint32(sum[4*i:]))
	}
	return s
}

// ErrNotBroker is Dial's answer when the broker pipe is not the
// broker's: any local account can create the pipe's name before the
// service does, and a client must never talk to that.
var ErrNotBroker = errors.New("the broker pipe is not served by the Kivali broker")

// brokerOwned is the one identity rule of the broker's Windows parts,
// from an object's owner SID: the broker service's own account
// (ServiceSID), LocalSystem, or the Administrators group. The service
// owns what it creates as itself; an elevated --console broker names the
// Administrators group; an ordinary account can make none of the three
// the owner of anything it creates (its own SID is the only user it may
// name, and Administrators only with the group enabled in its token,
// which elevation is). So an object owned by anyone else was not made by
// the broker. It decides two things:
//
//   - whether a client talks to the broker pipe (Dial, before writing
//     anything): the pipe's owner, read through the client's handle;
//   - whether the broker creates files in, or deletes, a VM directory
//     (winFiles): one the caller made first, which the caller can
//     write in, is refused.
//
// It is pure, so every OS tests it; dial_windows.go and fs_windows.go
// read the owners.
func brokerOwned(ownerSID string) bool {
	return strings.EqualFold(ownerSID, ServiceSID) ||
		strings.EqualFold(ownerSID, LocalSystemSID) ||
		strings.EqualFold(ownerSID, AdministratorsSID)
}
