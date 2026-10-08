package store

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

var autoT0 = time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)

// A fresh store has never had the slider touched: auto-release is off.
func TestAutoReleaseDefaultsOff(t *testing.T) {
	s := mustStore(t)
	if got := s.ReadAutoRelease(); got.Enabled {
		t.Fatalf("fresh store reads %+v, want off", got)
	}
	if sched := s.ReadAutoReleaseSchedule(); len(sched) != 0 || sched.Active() {
		t.Fatalf("fresh store schedule %+v, want empty and inactive", sched)
	}
}

func TestAutoReleaseRoundTrip(t *testing.T) {
	s := mustStore(t)
	for i, want := range []AutoRelease{
		{Enabled: true, Delay: 2 * time.Minute, Since: autoT0},
		{Enabled: true, Delay: 0, Since: autoT0.Add(time.Minute)},
		{Enabled: false, Since: autoT0.Add(2 * time.Minute)},
		{Enabled: true, Delay: 5 * time.Minute},
	} {
		if err := s.WriteAutoRelease(want); err != nil {
			t.Fatalf("write %+v: %v", want, err)
		}
		got := s.ReadAutoRelease()
		if got.Enabled != want.Enabled || (want.Enabled && got.Delay != want.Delay) || !got.Since.Equal(want.Since) {
			t.Fatalf("step %d: read back %+v, want %+v", i, got, want)
		}
	}
}

// A setting governs the messages sent from its Since up to the next
// setting's; a message older than every setting is off.
func TestAutoReleaseScheduleGovernsBySendTime(t *testing.T) {
	s := mustStore(t)
	t1, t2 := autoT0, autoT0.Add(time.Minute)
	if err := s.WriteAutoRelease(AutoRelease{Enabled: true, Delay: 2 * time.Minute, Since: t1}); err != nil {
		t.Fatal(err)
	}
	if err := s.WriteAutoRelease(AutoRelease{Enabled: true, Delay: 5 * time.Minute, Since: t2}); err != nil {
		t.Fatal(err)
	}
	sched := s.ReadAutoReleaseSchedule()
	if len(sched) != 2 || !sched.Active() {
		t.Fatalf("schedule %+v, want both settings in force", sched)
	}
	for _, c := range []struct {
		at   time.Time
		want AutoRelease
	}{
		{t1.Add(-time.Second), AutoRelease{}},
		{t1, sched[0]},
		{t2.Add(-time.Nanosecond), sched[0]},
		{t2, sched[1]},
		{t2.Add(time.Hour), sched[1]},
	} {
		if got := sched.At(c.at); got != c.want {
			t.Errorf("At(%s) = %+v, want %+v", c.at.Format(time.RFC3339Nano), got, c.want)
		}
	}
	if cur := sched.Current(); cur.Delay != 5*time.Minute || !cur.Since.Equal(t2) {
		t.Errorf("Current = %+v, want the 5m setting since t2", cur)
	}
}

// Off cancels: it is the only setting kept, so a message sent under an
// earlier delay is no longer on the clock.
func TestAutoReleaseOffCancelsEarlierSettings(t *testing.T) {
	s := mustStore(t)
	if err := s.WriteAutoRelease(AutoRelease{Enabled: true, Delay: 2 * time.Minute, Since: autoT0}); err != nil {
		t.Fatal(err)
	}
	if err := s.WriteAutoRelease(AutoRelease{Since: autoT0.Add(time.Minute)}); err != nil {
		t.Fatal(err)
	}
	sched := s.ReadAutoReleaseSchedule()
	if len(sched) != 1 || sched.Active() {
		t.Fatalf("schedule after off %+v, want just the off setting", sched)
	}
	if got := sched.At(autoT0.Add(30 * time.Second)); got.Enabled {
		t.Fatalf("a message sent under the cancelled delay reads %+v, want off", got)
	}
}

// A setting is dropped once the next one has taken over and every
// deadline it could have set has passed; until then it stays, since a
// message sent under it may still be counting down.
func TestAutoReleaseSettlesDeadSettings(t *testing.T) {
	s := mustStore(t)
	t1 := autoT0
	t2 := t1.Add(time.Minute)
	t3 := t2.Add(3 * time.Minute) // past t2+2m: the 2m setting is dead
	for _, a := range []AutoRelease{
		{Enabled: true, Delay: 2 * time.Minute, Since: t1},
		{Enabled: true, Delay: 5 * time.Minute, Since: t2},
	} {
		if err := s.WriteAutoRelease(a); err != nil {
			t.Fatal(err)
		}
	}
	if sched := s.ReadAutoReleaseSchedule(); len(sched) != 2 {
		t.Fatalf("schedule %+v, want the 2m setting kept while a message under it may be due", sched)
	}
	if err := s.WriteAutoRelease(AutoRelease{Enabled: true, Since: t3}); err != nil {
		t.Fatal(err)
	}
	sched := s.ReadAutoReleaseSchedule()
	if len(sched) != 2 || sched[0].Delay != 5*time.Minute || sched[1].Delay != 0 {
		t.Fatalf("schedule %+v, want the 2m setting dropped and the 5m one kept", sched)
	}
	// Off before a delay is dead at once: a message under it is off
	// with or without it.
	if err := s.WriteAutoRelease(AutoRelease{Since: t3.Add(time.Minute)}); err != nil {
		t.Fatal(err)
	}
	if err := s.WriteAutoRelease(AutoRelease{Enabled: true, Delay: time.Minute, Since: t3.Add(2 * time.Minute)}); err != nil {
		t.Fatal(err)
	}
	if sched := s.ReadAutoReleaseSchedule(); len(sched) != 1 || sched[0].Delay != time.Minute {
		t.Fatalf("schedule %+v, want only the 1m setting", sched)
	}
}

// Writing the setting that already holds is not a move: its Since
// stays, so the messages sent under it keep their deadlines.
func TestAutoReleaseRewritingTheSameSettingKeepsSince(t *testing.T) {
	s := mustStore(t)
	if err := s.WriteAutoRelease(AutoRelease{Enabled: true, Delay: 2 * time.Minute, Since: autoT0}); err != nil {
		t.Fatal(err)
	}
	if err := s.WriteAutoRelease(AutoRelease{Enabled: true, Delay: 2 * time.Minute, Since: autoT0.Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	got := s.ReadAutoRelease()
	if !got.Since.Equal(autoT0) {
		t.Fatalf("Since moved to %s, want %s", got.Since, autoT0)
	}
}

// A file from before settings were dated has always held: every
// message, however old, is under it.
func TestAutoReleaseUndatedFileHasAlwaysHeld(t *testing.T) {
	s := mustStore(t)
	if err := os.WriteFile(filepath.Join(s.Root(), autoReleaseFile), []byte(`{"delay": "2m0s"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	sched := s.ReadAutoReleaseSchedule()
	if got := sched.At(autoT0.AddDate(-10, 0, 0)); !got.Enabled || got.Delay != 2*time.Minute {
		t.Fatalf("At(long ago) = %+v, want the undated 2m setting", got)
	}
	// The first dated move keeps it for what was sent before the move.
	if err := s.WriteAutoRelease(AutoRelease{Enabled: true, Delay: 5 * time.Minute, Since: autoT0}); err != nil {
		t.Fatal(err)
	}
	sched = s.ReadAutoReleaseSchedule()
	if got := sched.At(autoT0.Add(-time.Second)); got.Delay != 2*time.Minute {
		t.Fatalf("At(before the move) = %+v, want 2m", got)
	}
	if got := sched.At(autoT0); got.Delay != 5*time.Minute {
		t.Fatalf("At(the move) = %+v, want 5m", got)
	}
}

// The file is meant to be readable and editable by hand, so the delay
// is stored as a duration string, "off" is the spelling of off, and
// the time a setting took effect is RFC 3339.
func TestAutoReleaseFileIsHumanReadable(t *testing.T) {
	s := mustStore(t)
	if err := s.WriteAutoRelease(AutoRelease{Enabled: true, Delay: 2 * time.Minute, Since: autoT0}); err != nil {
		t.Fatal(err)
	}
	if err := s.WriteAutoRelease(AutoRelease{Enabled: true, Delay: 5 * time.Minute, Since: autoT0.Add(time.Minute)}); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(filepath.Join(s.Root(), autoReleaseFile))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"\"delay\": \"5m0s\"",
		"\"since\": \"2026-01-01T12:01:00Z\"",
		"\"earlier\": [",
		"\"delay\": \"2m0s\"",
		"\"since\": \"2026-01-01T12:00:00Z\"",
	} {
		if !strings.Contains(string(b), want) {
			t.Errorf("file %q does not contain %q", b, want)
		}
	}
	if err := s.WriteAutoRelease(AutoRelease{}); err != nil {
		t.Fatal(err)
	}
	b, _ = os.ReadFile(filepath.Join(s.Root(), autoReleaseFile))
	if want := "\"delay\": \"off\""; !strings.Contains(string(b), want) {
		t.Fatalf("file %q does not contain %q", b, want)
	}
	if strings.Contains(string(b), "earlier") {
		t.Fatalf("off kept earlier settings: %q", b)
	}
}

// A damaged or nonsensical file must fail closed to off: the failure
// mode of a lost setting is the CEO releasing by hand, never an
// unattended queue draining itself. An earlier setting that will not
// parse takes the whole file down with it.
func TestAutoReleaseUnreadableReadsOff(t *testing.T) {
	s := mustStore(t)
	for _, body := range []string{
		"{not json",
		`{"delay":"soon"}`,
		`{"delay":"-1m"}`,
		`{"delay":"2m0s","since":"yesterday"}`,
		`{"delay":"2m0s","earlier":[{"delay":"soon"}]}`,
	} {
		if err := os.WriteFile(filepath.Join(s.Root(), autoReleaseFile), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		if got := s.ReadAutoRelease(); got.Enabled {
			t.Fatalf("file %q reads %+v, want off", body, got)
		}
		if sched := s.ReadAutoReleaseSchedule(); sched.Active() {
			t.Fatalf("file %q reads schedule %+v, want nothing in force", body, sched)
		}
	}
}

func TestAutoReleaseRefusesNegativeDelay(t *testing.T) {
	s := mustStore(t)
	if err := s.WriteAutoRelease(AutoRelease{Enabled: true, Delay: -time.Second}); err == nil {
		t.Fatal("negative delay was accepted")
	}
}
