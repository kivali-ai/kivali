//go:build !linux

package agentshell

import "os"

// HostCreds is the process's own credentials. The agent-shell runs in
// a Linux container; elsewhere (a developer's machine, where it never
// runs as root) it reads the IDs and refuses to change them.
type HostCreds struct{}

// IDs reports the real and effective IDs; the saved and filesystem
// ones are taken to equal the effective ones.
func (HostCreds) IDs() (IDs, error) {
	groups, err := os.Getgroups()
	if err != nil {
		return IDs{}, err
	}
	return IDs{
		RUID: os.Getuid(), EUID: os.Geteuid(), SUID: os.Geteuid(), FSUID: os.Geteuid(),
		RGID: os.Getgid(), EGID: os.Getegid(), SGID: os.Getegid(), FSGID: os.Getegid(),
		Groups: groups,
	}, nil
}

// Caps reports empty sets: capabilities are a Linux notion.
func (HostCreds) Caps() (Caps, error) { return Caps{}, nil }

// CapLastCap refuses: see HostCreds.
func (HostCreds) CapLastCap() (int, error) { return 0, ErrNoPrivilegeDrop }

// DropBoundingCap refuses: see HostCreds.
func (HostCreds) DropBoundingCap(int) error { return ErrNoPrivilegeDrop }

// SetNoNewPrivs refuses: see HostCreds.
func (HostCreds) SetNoNewPrivs() error { return ErrNoPrivilegeDrop }

// SetKeepCaps refuses: see HostCreds.
func (HostCreds) SetKeepCaps(bool) error { return ErrNoPrivilegeDrop }

// Setresgid refuses: see HostCreds.
func (HostCreds) Setresgid(int, int, int) error { return ErrNoPrivilegeDrop }

// Setgroups refuses: see HostCreds.
func (HostCreds) Setgroups([]int) error { return ErrNoPrivilegeDrop }

// Setresuid refuses: see HostCreds.
func (HostCreds) Setresuid(int, int, int) error { return ErrNoPrivilegeDrop }

// Capset refuses: see HostCreds.
func (HostCreds) Capset(Caps) error { return ErrNoPrivilegeDrop }

// Setfsuid refuses: see HostCreds.
func (HostCreds) Setfsuid(int) error { return ErrNoPrivilegeDrop }
