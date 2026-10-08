//go:build darwin && cgo

// Command boottest boots the Kivali Desktop VM image on macOS
// Virtualization.framework and checks the guest reaches
// "KIVALI-VM READY", then shuts it down cleanly.
//
// It attaches the kernel, initramfs, the read-only root disk
// (/dev/vda) and a data disk (/dev/vdb; a fresh sparse file unless
// -keep-data), a NAT network for outbound traffic, an entropy device,
// a vsock device, and a virtio console whose output is copied to
// <run>/console.log. The boot that follows creating the data disk
// carries kivali.format-data=1; later boots do not. On READY it writes
// KIVALI-POWEROFF to the console's input, which the guest answers with
// a clean shutdown, and exits 0 once the VM has stopped after
// reporting the data disk unmounted. Anything else dumps the console
// tail and exits 1.
//
// With -trim-check it first checks, through the guest agent, that the
// data disk gives deleted space back to the host: it writes and deletes
// a file, runs the guest's trim, and requires data.img's allocated
// bytes to fall by most of the file. -trim-crash-after hard-stops the
// VM that long into that trim instead (fault injection).
//
// The binary needs the com.apple.security.virtualization entitlement;
// vm/Makefile ad-hoc signs it with vm/boottest/entitlements.plist.
package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/Code-Hex/vz/v3"

	"github.com/kivali-ai/kivali/internal/supervisor/guestapi"
	kvz "github.com/kivali-ai/kivali/internal/supervisor/vz"
)

const (
	readyLine    = "KIVALI-VM READY"
	fatalPrefix  = "KIVALI-VM FATAL"
	cleanLine    = "KIVALI-VM: shutdown: data disk unmounted cleanly"
	poweroffVerb = "KIVALI-POWEROFF\n"
	baseCmdline  = "console=hvc0 cgroup_no_v1=all panic=10 kivali.selftest=1"
	formatFlag   = "kivali.format-data=1"
	crashEvent   = "crash"
)

type options struct {
	dir, run    string
	keepData    bool
	formatData  bool
	dataSizeGiB int64
	cpus        uint
	memMiB      uint64
	timeout     time.Duration
	crashOn     string
	trimCheck   bool
	trimCrash   time.Duration

	integrityCycles    int
	integrityMinRun    time.Duration
	integrityMaxRun    time.Duration
	integrityCorpusMiB int
	integritySeed      uint64

	vzCaching, vzSync string
}

func main() {
	var o options
	flag.StringVar(&o.dir, "dir", "build/out", "directory holding Image, initramfs.gz and root.squashfs")
	flag.StringVar(&o.run, "run", "build/run", "directory for console.log and data.img")
	flag.BoolVar(&o.keepData, "keep-data", false, "reuse <run>/data.img if it exists (tests a later boot)")
	flag.BoolVar(&o.formatData, "format-data", false, "pass "+formatFlag+" even when reusing the data disk")
	flag.Int64Var(&o.dataSizeGiB, "data-size-gib", 64, "size of a new sparse data disk, GiB")
	flag.UintVar(&o.cpus, "cpus", 2, "virtual CPUs")
	flag.Uint64Var(&o.memMiB, "memory-mib", 4096, "memory, MiB")
	flag.DurationVar(&o.timeout, "timeout", 5*time.Minute, "how long to wait for "+readyLine)
	flag.StringVar(&o.crashOn, "crash-on", "", "fault injection: hard-stop the VM as soon as a console line contains this text, then exit 0")
	flag.BoolVar(&o.trimCheck, "trim-check", false, "after READY, check that a deleted file's space leaves data.img once the guest trims")
	flag.DurationVar(&o.trimCrash, "trim-crash-after", 0, "fault injection: with -trim-check, hard-stop the VM this long after the trim starts, then exit 0")
	flag.IntVar(&o.integrityCycles, "integrity-cycles", 0, "crash test: this many boots on one data disk, each verifying the integrity oracle and (but the last) cutting the power at a random moment of its run (see integrity.go)")
	flag.DurationVar(&o.integrityMinRun, "integrity-min-run", 5*time.Second, "with -integrity-cycles, the shortest run before a power cut")
	flag.DurationVar(&o.integrityMaxRun, "integrity-max-run", 60*time.Second, "with -integrity-cycles, the longest run before a power cut")
	flag.IntVar(&o.integrityCorpusMiB, "integrity-corpus-mib", 256, "with -integrity-cycles, the oracle's corpus of data written once and checked every boot, MiB")
	flag.Uint64Var(&o.integritySeed, "integrity-seed", 0, "with -integrity-cycles, the seed of the power-cut times (0: from the clock; printed, to repeat a run)")
	flag.StringVar(&o.vzCaching, "vz-caching", "", "experiment only: the data disk's caching mode instead of the supervisor's (automatic, cached, uncached)")
	flag.StringVar(&o.vzSync, "vz-sync", "", "experiment only: the data disk's synchronization mode instead of the supervisor's (full, fsync, none)")
	flag.Parse()

	var err error
	if o.integrityCycles > 0 {
		if o.integrityMaxRun < o.integrityMinRun {
			fmt.Fprintln(os.Stderr, "boottest: -integrity-max-run is shorter than -integrity-min-run")
			os.Exit(2)
		}
		err = integrityCycles(o)
	} else {
		err = boot(o)
	}
	switch {
	case errors.Is(err, errFaultInjected):
		fmt.Println("boottest: DONE (fault injected; not a pass)")
	case err != nil:
		fmt.Fprintf(os.Stderr, "boottest: FAIL: %v\n", err)
		os.Exit(1)
	default:
		fmt.Println("boottest: PASS")
	}
}

var errFaultInjected = errors.New("fault injected")

// guestMAC is a fixed, locally administered (0x02 bit set), unicast
// address for the guest's NIC.
var guestMAC = net.HardwareAddr{0x02, 0x4b, 0x56, 0x00, 0x00, 0x01}

func boot(o options) error {
	if err := os.MkdirAll(o.run, 0o755); err != nil {
		return err
	}
	consolePath := filepath.Join(o.run, "console.log")
	dataPath := filepath.Join(o.run, "data.img")
	created, err := prepareDataDisk(dataPath, o.keepData, o.dataSizeGiB<<30)
	if err != nil {
		return err
	}
	cmdline := baseCmdline
	if created || o.formatData {
		cmdline += " " + formatFlag
	}

	logFile, err := os.Create(consolePath)
	if err != nil {
		return err
	}
	defer func() { _ = logFile.Close() }()

	// Console: the guest reads hostToGuestR and writes guestToHostW.
	hostToGuestR, hostToGuestW, err := os.Pipe()
	if err != nil {
		return err
	}
	guestToHostR, guestToHostW, err := os.Pipe()
	if err != nil {
		return err
	}

	vm, err := newVM(o, cmdline, dataPath, hostToGuestR, guestToHostW)
	if err != nil {
		return err
	}

	events := make(chan string, 16)
	go watchConsole(guestToHostR, logFile, events, o.crashOn)

	start := time.Now()
	if err := vm.Start(); err != nil {
		return fmt.Errorf("start: %w", err)
	}
	fmt.Printf("boottest: VM started (%d CPUs, %d MiB, data disk %s, console %s)\n", o.cpus, o.memMiB, dataPath, consolePath)
	fmt.Printf("boottest: kernel command line: %s\n", cmdline)

	fail := func(err error) error {
		if vm.CanStop() {
			_ = vm.Stop()
		}
		dumpTail(consolePath, 80)
		return err
	}

	deadline := time.After(o.timeout)
	states := vm.StateChangedNotify()
wait:
	for {
		select {
		case ev := <-events:
			switch ev {
			case readyLine:
				fmt.Printf("boottest: %s after %s\n", readyLine, time.Since(start).Round(time.Second))
				break wait
			case fatalPrefix:
				return fail(errors.New("guest reported a fatal error"))
			case crashEvent:
				if err := vm.Stop(); err != nil {
					return fmt.Errorf("hard stop: %w", err)
				}
				fmt.Printf("boottest: hard-stopped the VM on a line containing %q after %s\n", o.crashOn, time.Since(start).Round(time.Millisecond))
				return errFaultInjected
			}
		case st := <-states:
			if st == vz.VirtualMachineStateStopped || st == vz.VirtualMachineStateError {
				return fail(fmt.Errorf("VM stopped before %s (state %v)", readyLine, st))
			}
		case <-deadline:
			return fail(fmt.Errorf("no %s within %s", readyLine, o.timeout))
		}
	}

	if o.trimCheck {
		err := trimCheck(vm, dataPath, o.trimCrash)
		if errors.Is(err, errFaultInjected) {
			fmt.Printf("boottest: hard-stopped the VM %s into the trim\n", o.trimCrash)
			return err
		}
		if err != nil {
			return fail(fmt.Errorf("trim check: %w", err))
		}
	}

	// Clean shutdown through the guest. requestStop cannot be used:
	// see vm/README.md (no gpio-keys in the guest kernel).
	if _, err := io.WriteString(hostToGuestW, poweroffVerb); err != nil {
		return fail(fmt.Errorf("write poweroff verb: %w", err))
	}
	// The guest gives Kivali pods up to 60 s, k3s 30 s and stragglers
	// 10 s; two minutes covers all of it.
	stopDeadline := time.After(2 * time.Minute)
	sawClean := false
	for {
		select {
		case ev := <-events:
			if ev == cleanLine {
				sawClean = true
			}
		case st := <-states:
			if st == vz.VirtualMachineStateStopped {
				// The state change can overtake the last console bytes.
				// Virtualization.framework keeps the console pipe open,
				// so there is no EOF to wait for; give the reader a
				// moment instead.
				if !sawClean {
					sawClean = drainFor(events, cleanLine, 500*time.Millisecond)
				}
				if !sawClean {
					return fail(errors.New("VM stopped without unmounting the data disk cleanly"))
				}
				fmt.Printf("boottest: VM stopped cleanly after %s\n", time.Since(start).Round(time.Second))
				return nil
			}
			if st == vz.VirtualMachineStateError {
				return fail(errors.New("VM entered the error state during shutdown"))
			}
		case <-stopDeadline:
			return fail(errors.New("VM did not stop within two minutes of " + strings.TrimSpace(poweroffVerb)))
		}
	}
}

// trimCheckMiB is the size of the file trimCheck writes and deletes.
const trimCheckMiB = 2048

// trimCheck trims, writes trimCheckMiB of random data to the data disk,
// deletes it, runs the guest's trim again, and requires data.img's allocated
// bytes to have grown by most of the file and then shrunk by most of
// it. With crashAfter > 0 it hard-stops the VM that long after starting
// the trim and returns errFaultInjected.
func trimCheck(vm *vz.VirtualMachine, dataPath string, crashAfter time.Duration) error {
	c := &guestapi.Client{Dial: func(context.Context) (net.Conn, error) {
		devs := vm.SocketDevices()
		if len(devs) == 0 {
			return nil, errors.New("no vsock device")
		}
		return devs[0].Connect(guestapi.Port)
	}}
	run := func(argv ...string) error {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		defer cancel()
		var out strings.Builder
		code, err := c.Exec(ctx, guestapi.ExecSpec{Argv: argv}, nil, &out, &out)
		if err != nil {
			return fmt.Errorf("%v: %w", argv, err)
		}
		if code != 0 {
			return fmt.Errorf("%v exited %d: %s", argv, code, strings.TrimSpace(out.String()))
		}
		return nil
	}
	allocated := func() (int64, error) {
		var st syscall.Stat_t
		if err := syscall.Stat(dataPath, &st); err != nil {
			return 0, err
		}
		return st.Blocks * 512, nil
	}
	const mib = 1 << 20
	file := guestapi.DataDir + "/trim-check"

	// Trim first: space an earlier, interrupted run left allocated would
	// otherwise be reused by the write below without growing data.img.
	if err := run("/usr/libexec/kivali/trim", "--once"); err != nil {
		return err
	}
	before, err := allocated()
	if err != nil {
		return err
	}
	if err := run("sh", "-c", fmt.Sprintf("dd if=/dev/urandom of=%s bs=1M count=%d 2>/dev/null && sync", file, trimCheckMiB)); err != nil {
		return err
	}
	written, err := allocated()
	if err != nil {
		return err
	}
	fmt.Printf("boottest: trim check: data.img %d MiB allocated, %d MiB after writing %d MiB\n", before/mib, written/mib, trimCheckMiB)
	if written-before < trimCheckMiB*mib*3/4 {
		return fmt.Errorf("writing %d MiB grew data.img by only %d MiB", trimCheckMiB, (written-before)/mib)
	}
	if err := run("sh", "-c", "rm "+file+" && sync"); err != nil {
		return err
	}
	if crashAfter > 0 {
		go func() { _ = run("/usr/libexec/kivali/trim", "--once") }()
		time.Sleep(crashAfter)
		if err := vm.Stop(); err != nil {
			return fmt.Errorf("hard stop: %w", err)
		}
		return errFaultInjected
	}
	if err := run("/usr/libexec/kivali/trim", "--once"); err != nil {
		return err
	}
	trimmed, err := allocated()
	if err != nil {
		return err
	}
	fmt.Printf("boottest: trim check: data.img %d MiB allocated after deleting the file and trimming\n", trimmed/mib)
	if written-trimmed < trimCheckMiB*mib*3/4 {
		return fmt.Errorf("trimming returned only %d MiB of the deleted %d MiB", (written-trimmed)/mib, trimCheckMiB)
	}
	return nil
}

// drainFor reports whether want arrives on events within d.
func drainFor(events <-chan string, want string, d time.Duration) bool {
	timer := time.NewTimer(d)
	defer timer.Stop()
	for {
		select {
		case ev := <-events:
			if ev == want {
				return true
			}
		case <-timer.C:
			return false
		}
	}
}

// prepareDataDisk creates a sparse, all-zero raw disk, or keeps an
// existing one with -keep-data. It reports whether it created the file.
func prepareDataDisk(path string, keep bool, size int64) (bool, error) {
	if keep {
		if _, err := os.Stat(path); err == nil {
			fmt.Printf("boottest: reusing data disk %s\n", path)
			return false, nil
		}
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return false, err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR|os.O_EXCL, 0o600)
	if err != nil {
		return false, err
	}
	if err := f.Truncate(size); err != nil {
		_ = f.Close()
		return false, err
	}
	return true, f.Close()
}

func newVM(o options, cmdline, dataPath string, consoleIn, consoleOut *os.File) (*vz.VirtualMachine, error) {
	boot, err := vz.NewLinuxBootLoader(
		filepath.Join(o.dir, "Image"),
		vz.WithInitrd(filepath.Join(o.dir, "initramfs.gz")),
		vz.WithCommandLine(cmdline),
	)
	if err != nil {
		return nil, fmt.Errorf("boot loader: %w", err)
	}
	cfg, err := vz.NewVirtualMachineConfiguration(boot, o.cpus, o.memMiB<<20)
	if err != nil {
		return nil, err
	}

	serial, err := vz.NewFileHandleSerialPortAttachment(consoleIn, consoleOut)
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
	}{
		{filepath.Join(o.dir, "root.squashfs"), true},
		{dataPath, false},
	} {
		// The supervisor's own attachment (caching and sync modes), so
		// this test boots the disk configuration that ships.
		att, err := diskAttachment(o, d.path, d.readOnly)
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

	// NAT: outbound only. It cannot forward ports to the host; the
	// supervisor step replaces it with a vsock-backed user-mode stack.
	nat, err := vz.NewNATNetworkDeviceAttachment()
	if err != nil {
		return nil, err
	}
	nic, err := vz.NewVirtioNetworkDeviceConfiguration(nat)
	if err != nil {
		return nil, err
	}
	// A fixed MAC keeps the DHCP lease, and so the node address k3s
	// persists, the same on every boot; a changed IP leaves k3s
	// reconnecting to the old one ("no route to host").
	mac, err := vz.NewMACAddress(guestMAC)
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
	return vz.NewVirtualMachine(cfg)
}

// watchConsole copies the guest console to the log, echoes progress
// lines to stdout, and reports marker lines on events.
func watchConsole(r io.Reader, log io.Writer, events chan<- string, crashOn string) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	for sc.Scan() {
		line := strings.TrimRight(sc.Text(), "\r")
		_, _ = fmt.Fprintln(log, line)
		if crashOn != "" && strings.Contains(line, crashOn) {
			fmt.Println("  guest| " + line)
			events <- crashEvent
			continue
		}
		if !strings.Contains(line, "KIVALI-VM") {
			continue
		}
		fmt.Println("  guest| " + line)
		switch {
		case line == readyLine:
			events <- readyLine
		case strings.HasPrefix(line, fatalPrefix):
			events <- fatalPrefix
		case line == cleanLine:
			events <- cleanLine
		}
	}
}

func dumpTail(path string, n int) {
	b, err := os.ReadFile(path)
	if err != nil {
		fmt.Fprintf(os.Stderr, "boottest: cannot read console log: %v\n", err)
		return
	}
	lines := strings.Split(strings.TrimRight(string(b), "\n"), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	fmt.Fprintf(os.Stderr, "boottest: last %d console lines (%s):\n", len(lines), path)
	for _, l := range lines {
		fmt.Fprintln(os.Stderr, "  "+l)
	}
}

// diskAttachment is the supervisor's attachment (kvz.DiskAttachment),
// or, for an experiment (-vz-caching, -vz-sync), the data disk with
// other modes: the negative control of the integrity test runs the
// framework's automatic caching, the mode Kivali moved away from.
func diskAttachment(o options, path string, readOnly bool) (*vz.DiskImageStorageDeviceAttachment, error) {
	if readOnly || (o.vzCaching == "" && o.vzSync == "") {
		return kvz.DiskAttachment(path, readOnly)
	}
	caching, sync := kvz.DiskCaching, kvz.DiskSync
	switch o.vzCaching {
	case "":
	case "automatic":
		caching = vz.DiskImageCachingModeAutomatic
	case "cached":
		caching = vz.DiskImageCachingModeCached
	case "uncached":
		caching = vz.DiskImageCachingModeUncached
	default:
		return nil, fmt.Errorf("-vz-caching %q: want automatic, cached or uncached", o.vzCaching)
	}
	switch o.vzSync {
	case "":
	case "full":
		sync = vz.DiskImageSynchronizationModeFull
	case "fsync":
		sync = vz.DiskImageSynchronizationModeFsync
	case "none":
		sync = vz.DiskImageSynchronizationModeNone
	default:
		return nil, fmt.Errorf("-vz-sync %q: want full, fsync or none", o.vzSync)
	}
	fmt.Printf("boottest: EXPERIMENT: data disk caching %q, synchronization %q (not what Kivali ships)\n", o.vzCaching, o.vzSync)
	return vz.NewDiskImageStorageDeviceAttachmentWithCacheAndSync(path, readOnly, caching, sync)
}
