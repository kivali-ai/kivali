package guestapi

import (
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// fakeGuest is a sysfs and a dev directory laid out as the guest's
// kernel lays them out, with a PTP clock the test sets.
type fakeGuest struct {
	t        *testing.T
	sys, dev string
	ptp      time.Time
	ptpErr   error
	ptpRead  []string
}

func newFakeGuest(t *testing.T) *fakeGuest {
	root := t.TempDir()
	return &fakeGuest{t: t, sys: filepath.Join(root, "sys"), dev: filepath.Join(root, "dev")}
}

func (g *fakeGuest) write(rel, content string) {
	g.t.Helper()
	p := filepath.Join(g.sys, rel)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		g.t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		g.t.Fatal(err)
	}
}

func (g *fakeGuest) rtc(t time.Time) {
	g.write("class/rtc/rtc0/since_epoch", strconv.FormatInt(t.Unix(), 10)+"\n")
}

func (g *fakeGuest) clock() hostClock {
	return hostClock{sys: g.sys, dev: g.dev, readPTP: func(path string) (time.Time, error) {
		g.ptpRead = append(g.ptpRead, path)
		return g.ptp, g.ptpErr
	}}
}

// Why this test exists: Hyper-V's hardware clock holds the host's local
// time, as Windows keeps its own, so a guest that took it for UTC set
// its clock seven hours back on a computer in California, and every
// handoff token the supervisor minted was refused as minted by a clock
// hours ahead. On Hyper-V the host's clock must be read from Hyper-V's
// time sync, never from the hardware clock, even with the time sync
// missing; on Virtualization.framework the hardware clock holds UTC
// and is read.
func TestHostClockReadsHyperVTimeSyncNotItsHardwareClock(t *testing.T) {
	utc := time.Date(2026, 10, 29, 23, 13, 59, 0, time.UTC)
	local := utc.Add(-7 * time.Hour) // what Hyper-V's rtc_cmos shows

	g := newFakeGuest(t)
	g.rtc(local)
	g.write("bus/vmbus/devices/242ff919-07db-4180-9c2e-b86cb68c8c55/uevent", "")
	g.write("class/ptp/ptp0/clock_name", "hyperv\n")
	g.ptp = utc
	got, err := g.clock().read()
	if err != nil || !got.Equal(utc) {
		t.Fatalf("Hyper-V: read %v, %v; want %v", got, err, utc)
	}
	if len(g.ptpRead) != 1 || g.ptpRead[0] != filepath.Join(g.dev, "ptp0") {
		t.Errorf("read PTP devices %q", g.ptpRead)
	}

	// The PTP clock before Hyper-V has sent the host's time reads near
	// 1601: refused, not set.
	g.ptp = time.Date(1601, 1, 1, 0, 0, 5, 0, time.UTC)
	if _, err := g.clock().read(); err == nil || !strings.Contains(err.Error(), "has not sent the host's time yet") {
		t.Fatalf("PTP before the first sample: err %v", err)
	}
	g.ptp, g.ptpErr = time.Time{}, errors.New("operation not supported")
	if _, err := g.clock().read(); err == nil || !strings.Contains(err.Error(), "operation not supported") {
		t.Fatalf("PTP unreadable: err %v", err)
	}

	// Hyper-V without its time sync (another PTP clock only): the
	// hardware clock is still not read.
	g = newFakeGuest(t)
	g.rtc(local)
	g.write("bus/vmbus/devices/242ff919-07db-4180-9c2e-b86cb68c8c55/uevent", "")
	g.write("class/ptp/ptp0/clock_name", "some nic\n")
	if got, err := g.clock().read(); err == nil || !strings.Contains(err.Error(), "local time") {
		t.Fatalf("Hyper-V with no time sync: read %v, %v", got, err)
	}
	if len(g.ptpRead) != 0 {
		t.Errorf("read PTP devices %q", g.ptpRead)
	}

	// Virtualization.framework: the VMBus is built in with no devices
	// on it, and the hardware clock holds UTC.
	g = newFakeGuest(t)
	g.rtc(utc)
	if err := os.MkdirAll(filepath.Join(g.sys, "bus", "vmbus", "devices"), 0o755); err != nil {
		t.Fatal(err)
	}
	if got, err := g.clock().read(); err != nil || !got.Equal(utc) {
		t.Fatalf("Virtualization.framework: read %v, %v; want %v", got, err, utc)
	}

	// No hardware clock at all.
	g = newFakeGuest(t)
	if _, err := g.clock().read(); err == nil || !strings.Contains(err.Error(), "no hardware clock") {
		t.Fatalf("no clock: err %v", err)
	}
}
