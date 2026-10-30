package guestapi

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"golang.org/x/sys/unix"
)

// NewClockSync is the clock sync of the Kivali Desktop guest: the
// host's clock through hostClock, the system clock through
// clock_settime(CLOCK_REALTIME).
func NewClockSync(logf func(format string, args ...any)) *ClockSync {
	h := hostClock{sys: "/sys", dev: "/dev", readPTP: readPTP}
	return &ClockSync{
		ReadHost: h.read,
		Now:      time.Now,
		Set: func(t time.Time) error {
			ts := unix.NsecToTimespec(t.UnixNano())
			return unix.ClockSettime(unix.CLOCK_REALTIME, &ts)
		},
		Logf: logf,
	}
}

// hostClock reads the host's clock as the hypervisor shows it to the
// guest. On Virtualization.framework that is the hardware clock
// (rtc-pl031), which holds UTC. On Hyper-V the hardware clock
// (rtc_cmos) holds the host's local time, as Windows keeps its own, so
// it is never read there: the host's UTC time comes from Hyper-V's time
// sync, which hv_utils shows as a PTP clock named "hyperv".
type hostClock struct {
	// sys and dev are where sysfs and devtmpfs are mounted.
	sys, dev string
	// readPTP reads the PTP clock at a device path.
	readPTP func(path string) (time.Time, error)
}

// hypervPTPName is the clock_name hv_utils gives Hyper-V's time sync.
const hypervPTPName = "hyperv"

// earliestHostTime is a time before any the host's clock shows: Hyper-V's
// PTP clock reads near the year 1601 until the host has sent its time.
var earliestHostTime = time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)

func (h hostClock) read() (time.Time, error) {
	if ptp := h.hypervPTP(); ptp != "" {
		t, err := h.readPTP(ptp)
		if err != nil {
			return time.Time{}, fmt.Errorf("read the time sync clock %s: %w", ptp, err)
		}
		if t.Before(earliestHostTime) {
			return time.Time{}, fmt.Errorf("the time sync clock %s reads %s: Hyper-V has not sent the host's time yet", ptp, t.UTC().Format(time.RFC3339))
		}
		return t, nil
	}
	if h.onHyperV() {
		return time.Time{}, errors.New("no PTP clock named hyperv, Hyper-V's time sync (is hv_utils loaded, and the VM's time synchronization service on?); the hardware clock holds the host's local time, not UTC, so it is not read")
	}
	return readRTC(filepath.Join(h.sys, "class", "rtc", "rtc0", "since_epoch"))
}

// hypervPTP is the device path of the PTP clock named hypervPTPName,
// or "" when there is none.
func (h hostClock) hypervPTP() string {
	names, _ := filepath.Glob(filepath.Join(h.sys, "class", "ptp", "ptp*", "clock_name"))
	for _, n := range names {
		b, err := os.ReadFile(n)
		if err == nil && strings.TrimSpace(string(b)) == hypervPTPName {
			return filepath.Join(h.dev, filepath.Base(filepath.Dir(n)))
		}
	}
	return ""
}

// onHyperV is whether the guest has a device on Hyper-V's VMBus. The
// bus itself is built into the kernel on both hypervisors; only Hyper-V
// puts devices on it.
func (h hostClock) onHyperV() bool {
	entries, err := os.ReadDir(filepath.Join(h.sys, "bus", "vmbus", "devices"))
	return err == nil && len(entries) > 0
}

// readPTP reads a PTP clock through its character device: the kernel's
// dynamic POSIX clock for an open descriptor (FD_TO_CLOCKID).
func readPTP(path string) (time.Time, error) {
	f, err := os.Open(path)
	if err != nil {
		return time.Time{}, err
	}
	defer func() { _ = f.Close() }()
	var ts unix.Timespec
	if err := unix.ClockGettime(int32((^int(f.Fd()))<<3|3), &ts); err != nil {
		return time.Time{}, fmt.Errorf("clock_gettime: %w", err)
	}
	return time.Unix(ts.Unix()), nil
}

// readRTC reads the hardware clock from a since_epoch file.
func readRTC(path string) (time.Time, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return time.Time{}, fmt.Errorf("the guest has no hardware clock (%s)", path)
		}
		return time.Time{}, err
	}
	sec, err := strconv.ParseInt(strings.TrimSpace(string(b)), 10, 64)
	if err != nil {
		return time.Time{}, fmt.Errorf("%s: %w", path, err)
	}
	return time.Unix(sec, 0), nil
}
