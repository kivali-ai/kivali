// Package broker is the Kivali broker: the small privileged service
// that manages Hyper-V virtual machines on behalf of ordinary accounts
// (docs/developers/supervisor.md, "The broker"). `kivali-supervisor broker`
// runs it as a Windows service (its own virtual account), or in the foreground of
// an elevated terminal with --console, serving HTTP/1.1 on the named
// pipe \\.\pipe\kivali-broker.
//
// It is an elevation boundary and is written to be reviewed as one.
// Every request is checked as its caller: the server impersonates the
// pipe client for every file it names, so it opens nothing the caller
// could not open; paths are made canonical from an open handle and
// refused if any component is a link; the VM's name, MAC and console
// pipe must be the ones derived from the caller's config directory; a
// VM is touched only by the account whose SID its notes carry; and
// inputs reach PowerShell only as fields of a JSON object on stdin,
// read by a fixed, embedded script.
//
// What compiles everywhere (and is vetted on every OS): the wire types
// and the client (this file, client.go), the naming rules and request
// validation, and the policy itself (policy.go), which talks to the
// Windows-only parts through the Caller, Files and Runner interfaces.
// The impersonation, the canonical paths, the pipe, the PowerShell
// runner and the service are in the _windows files.
package broker

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
)

// PipeName is the broker's named pipe.
const PipeName = `\\.\pipe\kivali-broker`

// ServiceName is the broker's Windows service (and event log source).
const ServiceName = "kivali-broker"

// The VM's states as Hyper-V names them (Microsoft.HyperV.PowerShell.VMState).
const (
	StateOff     = "Off"
	StateRunning = "Running"
)

// DataDiskName is the one VHD name POST /v1/vhd creates: the data disk.
const DataDiskName = "data.vhdx"

// VMDirName is the directory under the config directory holding the
// VM's configuration and the root disk's differencing child. The broker
// makes and owns it (the caller can only read it), creates the data
// disk in it before moving it into place, and removes it with the VM.
const VMDirName = "vm"

// ChildDiskName is the root disk's differencing child in the VM
// directory, whose parent is the release's root.vhdx.
const ChildDiskName = "root-child.vhdx"

// DefaultSwitch is the Hyper-V switch every VM is connected to.
const DefaultSwitch = "Default Switch"

// VsockServiceKey is the registry key that registers the guest agent's
// vsock port (1024) as a Hyper-V socket service.
const VsockServiceKey = `HKLM:\SOFTWARE\Microsoft\Windows NT\CurrentVersion\Virtualization\GuestCommunicationServices\00000400-facb-11e6-bd58-64006a7986d3`

// Limits the broker enforces before anything runs.
const (
	MinCPUs      = 1
	MaxCPUs      = 64
	MinMemoryMB  = 1024 // Dynamic Memory's minimum is 1 GiB
	MaxMemoryMB  = 256 << 10
	MinDiskBytes = 1 << 30
	MaxDiskBytes = 4 << 40
)

// VMRequest is the body of POST /v1/vm: the VM's whole configuration.
type VMRequest struct {
	Name      string `json:"name"`
	ConfigDir string `json:"config_dir"`
	RootVHDX  string `json:"root_vhdx"`
	DataVHDX  string `json:"data_vhdx"`
	CPUs      uint   `json:"cpus"`
	MemoryMB  uint64 `json:"memory_mb"`
	// MAC is 12 hex digits without separators, as Hyper-V writes it
	// (024B566A4388).
	MAC     string `json:"mac"`
	ComPipe string `json:"com_pipe"`
}

// VMState answers POST /v1/vm, GET /v1/vm/{name} and start.
type VMState struct {
	ID               string `json:"id"`
	State            string `json:"state"`
	MemoryAssignedMB uint64 `json:"memory_assigned_mb,omitempty"`
	// Deferred names what a POST /v1/vm to a running VM left for its next
	// configuration while off (only memory and CPUs ever are).
	Deferred []string `json:"deferred,omitempty"`
}

// StopRequest is the body of POST /v1/vm/{name}/stop.
type StopRequest struct {
	// Graceful stops through the shutdown integration service; otherwise
	// the VM is turned off.
	Graceful bool `json:"graceful"`
}

// VHDRequest is the body of POST /v1/vhd: the data disk to make at Path
// (<config>\data.vhdx, where nothing may be yet). The broker creates it
// in its own VM directory, <config>\vm, renames it to Path and then
// grants the caller full control of it; it never creates a file at a
// path in the caller's directory, where a link could send the creation
// elsewhere.
type VHDRequest struct {
	Path      string `json:"path"`
	SizeBytes int64  `json:"size_bytes"`
}

// VHDResult answers POST /v1/vhd with the disk's canonical path.
type VHDResult struct {
	Path string `json:"path"`
}

// Setup answers GET and POST /v1/setup.
type Setup struct {
	// HyperV: the Hyper-V module answers (the role is enabled and its
	// management service runs).
	HyperV bool `json:"hyperv"`
	// DefaultSwitch: Hyper-V's Default Switch exists.
	DefaultSwitch bool `json:"default_switch"`
	// VsockService: the guest agent's port is registered as a Hyper-V
	// socket service.
	VsockService bool `json:"vsock_service"`
}

// Ready reports whether everything the broker needs is in place.
func (s Setup) Ready() bool { return s.HyperV && s.DefaultSwitch && s.VsockService }

// VMName is the VM of a config directory key: kivali-<key>.
func VMName(key string) string { return "kivali-" + key }

// ComPipe is the console (COM1) pipe of a config directory key.
func ComPipe(key string) string { return `\\.\pipe\kivali-` + key + "-com1" }

// MAC is the guest NIC's address of a config directory key: the locally
// administered prefix 02:4b:56 and the key's first three bytes, as 12
// hex digits.
func MAC(key string) string { return "024B56" + strings.ToUpper(key[:6]) }

var (
	nameRE = regexp.MustCompile(`^kivali-[0-9a-f]{8}$`)
	macRE  = regexp.MustCompile(`^[0-9A-Fa-f]{12}$`)
	sidRE  = regexp.MustCompile(`^S-1-[0-9]+(-[0-9]+)+$`)
	keyRE  = regexp.MustCompile(`^[0-9a-f]{8}$`)
)

// ValidName checks a VM name: kivali- and 8 lowercase hex digits.
func ValidName(name string) error {
	if !nameRE.MatchString(name) {
		return fmt.Errorf("the VM name %q is not kivali- and 8 hex digits", name)
	}
	return nil
}

// KeyOf is the config directory key in a valid VM name.
func KeyOf(name string) string { return strings.TrimPrefix(name, "kivali-") }

// ValidMAC checks a MAC: 12 hex digits, a unicast, locally administered
// address.
func ValidMAC(mac string) error {
	if !macRE.MatchString(mac) {
		return fmt.Errorf("the MAC %q is not 12 hex digits", mac)
	}
	var first byte
	_, _ = fmt.Sscanf(mac[:2], "%02x", &first)
	if first&0x01 != 0 {
		return fmt.Errorf("the MAC %s is a multicast address", mac)
	}
	if first&0x02 == 0 {
		return fmt.Errorf("the MAC %s is not locally administered", mac)
	}
	return nil
}

// ValidSID checks the string form of a SID.
func ValidSID(sid string) error {
	if !sidRE.MatchString(sid) {
		return fmt.Errorf("%q is not a SID", sid)
	}
	return nil
}

// localAbs checks p is an absolute path on a local drive (C:\...),
// never a UNC, device or \\?\ path, with no . or .. component and no
// characters Windows reserves.
func localAbs(what, p string) error {
	if len(p) < 3 || !isLetter(p[0]) || p[1] != ':' || p[2] != '\\' {
		return fmt.Errorf("%s %q is not an absolute path on a local drive", what, p)
	}
	if strings.ContainsAny(p[2:], `/:*?"<>|`) || strings.IndexFunc(p, func(r rune) bool { return r < 0x20 }) >= 0 {
		return fmt.Errorf("%s %q has characters a path cannot hold", what, p)
	}
	for _, part := range strings.Split(p[3:], `\`) {
		if part == "." || part == ".." || (part == "" && p[3:] != "") {
			return fmt.Errorf("%s %q is not a clean path", what, p)
		}
	}
	return nil
}

func isLetter(c byte) bool { return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' }

// Under reports whether p lies strictly inside dir. Both are canonical
// Windows paths (backslashes, no . or ..); the comparison ignores case,
// as NTFS does. It is pure, so every OS tests it.
func Under(dir, p string) bool {
	d := strings.TrimRight(dir, `\`)
	if len(p) <= len(d)+1 || !strings.EqualFold(p[:len(d)], d) || p[len(d)] != '\\' {
		return false
	}
	for _, part := range strings.Split(p[len(d)+1:], `\`) {
		if part == "" || part == "." || part == ".." {
			return false
		}
	}
	return true
}

// winJoin joins Windows path elements whatever OS compiles it.
func winJoin(dir string, elem ...string) string {
	return strings.TrimRight(dir, `\`) + `\` + strings.Join(elem, `\`)
}

// winDir is a Windows path's directory whatever OS compiles it.
func winDir(p string) string {
	i := strings.LastIndexByte(p, '\\')
	if i <= 2 { // C:\x
		return p[:i+1]
	}
	return p[:i]
}

// winBase is a Windows path's last element whatever OS compiles it.
func winBase(p string) string { return p[strings.LastIndexByte(p, '\\')+1:] }

// hasExt reports whether p ends in ext, ignoring case.
func hasExt(p, ext string) bool {
	return len(p) > len(ext) && strings.EqualFold(p[len(p)-len(ext):], ext)
}

// Validate checks a VM request's form before anything is opened or run:
// the name, the MAC, the CPUs and memory, and that every path is an
// absolute local path of the right kind. Whether the name, MAC and pipe
// belong to the config directory is checked once it is canonical.
func (r VMRequest) Validate() error {
	if err := ValidName(r.Name); err != nil {
		return err
	}
	if err := ValidMAC(r.MAC); err != nil {
		return err
	}
	if r.CPUs < MinCPUs || r.CPUs > MaxCPUs {
		return fmt.Errorf("cpus %d is outside %d to %d", r.CPUs, MinCPUs, MaxCPUs)
	}
	if r.MemoryMB < MinMemoryMB || r.MemoryMB > MaxMemoryMB {
		return fmt.Errorf("memory_mb %d is outside %d to %d", r.MemoryMB, MinMemoryMB, MaxMemoryMB)
	}
	if r.MemoryMB%2 != 0 {
		return fmt.Errorf("memory_mb %d is not a multiple of 2 MiB, as Hyper-V needs", r.MemoryMB)
	}
	for _, f := range []struct{ what, p string }{{"config_dir", r.ConfigDir}, {"root_vhdx", r.RootVHDX}, {"data_vhdx", r.DataVHDX}} {
		if err := localAbs(f.what, f.p); err != nil {
			return err
		}
	}
	if !hasExt(r.RootVHDX, ".vhdx") || !hasExt(r.DataVHDX, ".vhdx") {
		return errors.New("root_vhdx and data_vhdx must be .vhdx files")
	}
	if !strings.HasPrefix(r.ComPipe, `\\.\pipe\kivali-`) || !strings.HasSuffix(r.ComPipe, "-com1") ||
		!keyRE.MatchString(strings.TrimSuffix(strings.TrimPrefix(r.ComPipe, `\\.\pipe\kivali-`), "-com1")) {
		return fmt.Errorf(`com_pipe %q is not \\.\pipe\kivali-<key>-com1`, r.ComPipe)
	}
	return nil
}

// Validate checks a VHD request's form: an absolute local path named
// data.vhdx and a size Hyper-V accepts (whole MiB).
func (r VHDRequest) Validate() error {
	if err := localAbs("path", r.Path); err != nil {
		return err
	}
	if !strings.EqualFold(winBase(r.Path), DataDiskName) {
		return fmt.Errorf("path %q: the broker creates only %s", r.Path, DataDiskName)
	}
	if r.SizeBytes < MinDiskBytes || r.SizeBytes > MaxDiskBytes {
		return fmt.Errorf("size_bytes %d is outside %d to %d", r.SizeBytes, int64(MinDiskBytes), int64(MaxDiskBytes))
	}
	if r.SizeBytes%(1<<20) != 0 {
		return fmt.Errorf("size_bytes %d is not a whole number of MiB", r.SizeBytes)
	}
	return nil
}

// OwnerPrefix starts the line of a VM's notes that names its owner.
const OwnerPrefix = "kivali-owner="

// OwnerNotes is what the broker writes into a VM's notes.
func OwnerNotes(sid string) string {
	return "Managed by the Kivali broker; do not edit.\n" + OwnerPrefix + sid
}

// OwnerOf reads the owner's SID from a VM's notes. A VM whose notes
// name no owner, or more than one, has none.
func OwnerOf(notes string) (string, bool) {
	owner := ""
	for _, line := range strings.Split(notes, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, OwnerPrefix) {
			continue
		}
		sid := strings.TrimPrefix(line, OwnerPrefix)
		if ValidSID(sid) != nil || (owner != "" && owner != sid) {
			return "", false
		}
		owner = sid
	}
	return owner, owner != ""
}
