//go:build !linux

package agentshell

import "os"

// HostCreds is the process's own credentials. The agent-shell runs in
// a Linux container; elsewhere (a developer's machine, where it never
// runs as root) it reads the IDs and refuses to change them.
type HostCreds struct{}

// IDs reports the real and effective IDs; the saved ones are taken to
// equal the effective ones.
func (HostCreds) IDs() (IDs, error) {
	groups, err := os.Getgroups()
	if err != nil {
		return IDs{}, err
	}
	return IDs{
		RUID: os.Getuid(), EUID: os.Geteuid(), SUID: os.Geteuid(),
		RGID: os.Getgid(), EGID: os.Getegid(), SGID: os.Getegid(),
		Groups: groups,
	}, nil
}

// Setgroups refuses: see HostCreds.
func (HostCreds) Setgroups([]int) error { return ErrNoPrivilegeDrop }

// Setgid refuses: see HostCreds.
func (HostCreds) Setgid(int) error { return ErrNoPrivilegeDrop }

// Setuid refuses: see HostCreds.
func (HostCreds) Setuid(int) error { return ErrNoPrivilegeDrop }

// Caps reports empty sets: capabilities are a Linux notion.
func (HostCreds) Caps() (Caps, error) { return Caps{}, nil }
