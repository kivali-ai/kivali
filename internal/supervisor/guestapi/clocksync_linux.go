package guestapi

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"golang.org/x/sys/unix"
)

// RTCSinceEpochFile is the kernel's reading of the first hardware
// clock, in seconds since the Unix epoch, read from the device each
// time: Virtualization.framework's pl031 and Hyper-V's RTC both follow
// the host's clock.
const RTCSinceEpochFile = "/sys/class/rtc/rtc0/since_epoch"

// NewClockSync is the clock sync of the Kivali Desktop guest: the
// hardware clock through RTCSinceEpochFile, the system clock through
// clock_settime(CLOCK_REALTIME).
func NewClockSync(logf func(format string, args ...any)) *ClockSync {
	return &ClockSync{
		ReadRTC: func() (time.Time, error) { return readRTC(RTCSinceEpochFile) },
		Now:     time.Now,
		Set: func(t time.Time) error {
			return unix.ClockSettime(unix.CLOCK_REALTIME, &unix.Timespec{Sec: t.Unix()})
		},
		Logf: logf,
	}
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
