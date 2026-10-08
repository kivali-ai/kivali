//go:build darwin && cgo

// Package vz is the macOS Backend: it boots the Kivali Desktop VM on
// Virtualization.framework (github.com/Code-Hex/vz). The devices match
// vm/README.md's boot contract: kernel and initrd through
// VZLinuxBootLoader with the kernel command line, the read-only root
// disk as /dev/vda, the data disk (a sparse raw file) as /dev/vdb, a
// virtio console carrying the marker lines out and the poweroff verb
// in, a NAT NIC with a fixed MAC, entropy, and vsock for the guest
// agent. The binary using it must carry the
// com.apple.security.virtualization entitlement.
package vz

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/Code-Hex/vz/v3"

	"github.com/kivali-ai/kivali/internal/supervisor"
	"github.com/kivali-ai/kivali/internal/supervisor/guestapi"
)

// The VM image's files (vm/README.md).
const (
	KernelFile  = "Image"
	InitrdFile  = "initramfs.gz"
	RootFile    = "root.squashfs"
	VersionFile = "VERSION"
	// DataDiskFile is the data disk's name in the config directory.
	DataDiskFile = "data.img"
)

// Kernel command line (vm/README.md, "Kernel command line").
const (
	BaseCmdline = "console=hvc0 cgroup_no_v1=all panic=10"
	// FormatFlag is added only on the first boot of a new data disk.
	FormatFlag = "kivali.format-data=1"
)

// How every disk is attached (DiskAttachment). Virtualization.framework's
// default, the automatic caching mode, silently corrupts Linux guests'
// disks on Apple Silicon under mixed I/O (utmapp/UTM#4840; Lima and
// vfkit pin the cached mode for it, Lima "until the corruption issue is
// properly fixed"). Full synchronization turns a guest flush into
// F_FULLFSYNC, through the drive's own cache, so what the guest's ext4
// journal and fsync calls commit survives a host crash or power loss;
// the fsync mode would stop at the drive's volatile cache.
const (
	DiskCaching = vz.DiskImageCachingModeCached
	DiskSync    = vz.DiskImageSynchronizationModeFull
)

// DiskAttachment attaches the raw disk image at path with DiskCaching
// and DiskSync. The supervisor and vm/boottest both attach through it,
// so the boot test exercises the configuration that ships.
func DiskAttachment(path string, readOnly bool) (*vz.DiskImageStorageDeviceAttachment, error) {
	return vz.NewDiskImageStorageDeviceAttachmentWithCacheAndSync(path, readOnly, DiskCaching, DiskSync)
}

// PoweroffVerb, written to the console, makes the guest init stop
// cleanly without the agent.
const PoweroffVerb = "KIVALI-POWEROFF\n"

// GuestMAC is the guest NIC's fixed, locally administered address: DHCP
// then hands out the same address every boot, which k3s needs because
// it persists the node address (vm/README.md).
var GuestMAC = net.HardwareAddr{0x02, 0x4b, 0x56, 0x00, 0x00, 0x01}

// Backend creates Virtualization.framework machines.
type Backend struct{}

var _ supervisor.Backend = Backend{}

// Image checks dir holds the kernel, initramfs and root disk and reads
// the image's version from VERSION ("dev" without one). The guest is
// arm64: Virtualization.framework runs only the host's architecture.
func (Backend) Image(dir string) (supervisor.ImageInfo, error) {
	for _, f := range []string{KernelFile, InitrdFile, RootFile} {
		if _, err := os.Stat(filepath.Join(dir, f)); err != nil {
			return supervisor.ImageInfo{}, fmt.Errorf("%s has no %s (want %s, %s and %s)", dir, f, KernelFile, InitrdFile, RootFile)
		}
	}
	info := supervisor.ImageInfo{Version: "dev", Arch: "arm64"}
	b, err := os.ReadFile(filepath.Join(dir, VersionFile))
	switch {
	case err == nil:
		if v := strings.TrimSpace(string(b)); v != "" {
			info.Version = v
		}
	case !errors.Is(err, fs.ErrNotExist):
		return info, err
	}
	return info, nil
}

// DataDiskPath is data.img in the config directory.
func (Backend) DataDiskPath(configDir string) string {
	return filepath.Join(configDir, DataDiskFile)
}

// CreateDataDisk makes a new sparse, all-zero raw disk of size bytes,
// replacing any file at p.
func (Backend) CreateDataDisk(p string, size int64) error {
	if err := os.Remove(p); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	f, err := os.OpenFile(p, os.O_CREATE|os.O_RDWR|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	if err := f.Truncate(size); err != nil {
		_ = f.Close()
		return err
	}
	return f.Close()
}

// Cmdline is the kernel command line of a boot.
func Cmdline(formatData bool) string {
	if formatData {
		return BaseCmdline + " " + FormatFlag
	}
	return BaseCmdline
}

type machine struct {
	vm        *vz.VirtualMachine
	events    chan supervisor.ConsoleEvent
	done      chan struct{}
	doneOnce  sync.Once
	consoleIn *os.File // host writes, guest reads
	closers   []io.Closer
}

// NewMachine configures (but does not start) a VM.
func (Backend) NewMachine(c supervisor.MachineConfig) (supervisor.Machine, error) {
	cmdline := Cmdline(c.FormatData)
	if c.ConsoleLog != nil {
		_, _ = fmt.Fprintf(c.ConsoleLog, "%s host: kernel command line %q\n", time.Now().UTC().Format("15:04:05.000"), cmdline)
	}
	boot, err := vz.NewLinuxBootLoader(filepath.Join(c.ImageDir, KernelFile),
		vz.WithInitrd(filepath.Join(c.ImageDir, InitrdFile)), vz.WithCommandLine(cmdline))
	if err != nil {
		return nil, fmt.Errorf("boot loader: %w", err)
	}
	cfg, err := vz.NewVirtualMachineConfiguration(boot, c.CPUs, c.MemoryMB<<20)
	if err != nil {
		return nil, err
	}
	m := &machine{events: make(chan supervisor.ConsoleEvent, 64), done: make(chan struct{})}

	// Console: the guest reads hostToGuestR and writes guestToHostW.
	hostToGuestR, hostToGuestW, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	guestToHostR, guestToHostW, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	m.consoleIn = hostToGuestW
	m.closers = []io.Closer{hostToGuestR, hostToGuestW, guestToHostW}
	serial, err := vz.NewFileHandleSerialPortAttachment(hostToGuestR, guestToHostW)
	if err != nil {
		return nil, err
	}
	console, err := vz.NewVirtioConsoleDeviceSerialPortConfiguration(serial)
	if err != nil {
		return nil, err
	}
	cfg.SetSerialPortsVirtualMachineConfiguration([]*vz.VirtioConsoleDeviceSerialPortConfiguration{console})

	// Order matters: the root disk is /dev/vda, the data disk /dev/vdb.
	var disks []vz.StorageDeviceConfiguration
	for _, d := range []struct {
		path     string
		readOnly bool
	}{{filepath.Join(c.ImageDir, RootFile), true}, {c.DataDisk, false}} {
		att, err := DiskAttachment(d.path, d.readOnly)
		if err != nil {
			return nil, fmt.Errorf("disk %s: %w", d.path, err)
		}
		blk, err := vz.NewVirtioBlockDeviceConfiguration(att)
		if err != nil {
			return nil, err
		}
		disks = append(disks, blk)
	}
	cfg.SetStorageDevicesVirtualMachineConfiguration(disks)

	// NAT: outbound only. The org's port reaches the host through the
	// guest agent's vsock proxy, not this NIC.
	nat, err := vz.NewNATNetworkDeviceAttachment()
	if err != nil {
		return nil, err
	}
	nic, err := vz.NewVirtioNetworkDeviceConfiguration(nat)
	if err != nil {
		return nil, err
	}
	mac, err := vz.NewMACAddress(GuestMAC)
	if err != nil {
		return nil, err
	}
	nic.SetMACAddress(mac)
	cfg.SetNetworkDevicesVirtualMachineConfiguration([]*vz.VirtioNetworkDeviceConfiguration{nic})

	entropy, err := vz.NewVirtioEntropyDeviceConfiguration()
	if err != nil {
		return nil, err
	}
	cfg.SetEntropyDevicesVirtualMachineConfiguration([]*vz.VirtioEntropyDeviceConfiguration{entropy})

	vsock, err := vz.NewVirtioSocketDeviceConfiguration()
	if err != nil {
		return nil, err
	}
	cfg.SetSocketDevicesVirtualMachineConfiguration([]vz.SocketDeviceConfiguration{vsock})

	if ok, err := cfg.Validate(); !ok || err != nil {
		return nil, fmt.Errorf("invalid VM configuration: %w", err)
	}
	vm, err := vz.NewVirtualMachine(cfg)
	if err != nil {
		return nil, err
	}
	m.vm = vm
	go supervisor.ScanConsole(guestToHostR, c.ConsoleLog, m.events)
	go m.watch()
	return m, nil
}

func (m *machine) watch() {
	for st := range m.vm.StateChangedNotify() {
		if st == vz.VirtualMachineStateStopped || st == vz.VirtualMachineStateError {
			m.markDone()
			return
		}
	}
}

func (m *machine) markDone() {
	m.doneOnce.Do(func() {
		close(m.done)
		for _, c := range m.closers {
			_ = c.Close()
		}
	})
}

func (m *machine) Start() error { return m.vm.Start() }

func (m *machine) Events() <-chan supervisor.ConsoleEvent { return m.events }

func (m *machine) Done() <-chan struct{} { return m.done }

// DialAgent connects to the guest agent's vsock port. The host side
// needs no context id: a Virtualization.framework socket device reaches
// its one guest.
func (m *machine) DialAgent(ctx context.Context) (net.Conn, error) {
	port := uint32(guestapi.Port)
	devs := m.vm.SocketDevices()
	if len(devs) == 0 {
		return nil, errors.New("the VM has no vsock device")
	}
	type result struct {
		c   net.Conn
		err error
	}
	ch := make(chan result, 1)
	go func() {
		c, err := devs[0].Connect(port)
		ch <- result{c, err}
	}()
	select {
	case r := <-ch:
		if r.err != nil {
			return nil, fmt.Errorf("vsock connect to port %d: %w", port, r.err)
		}
		return r.c, nil
	case <-ctx.Done():
		go func() {
			if r := <-ch; r.c != nil {
				_ = r.c.Close()
			}
		}()
		return nil, ctx.Err()
	}
}

// RequestPoweroff writes the poweroff verb to the guest console.
func (m *machine) RequestPoweroff() error {
	_, err := io.WriteString(m.consoleIn, PoweroffVerb)
	return err
}

func (m *machine) HardStop() error {
	select {
	case <-m.done:
		return nil
	default:
	}
	if !m.vm.CanStop() {
		return errors.New("the VM cannot be stopped in its current state")
	}
	return m.vm.Stop()
}
