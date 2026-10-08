package store

import (
	"encoding/json"
	"errors"
	"os"
	"time"
)

// AutoRelease is one setting of the inbox page's auto-release slider:
// whether a message queued while it was in effect releases on its own,
// how long after it was sent, and since when the setting has held.
//
// Off is the default and is what a missing or unreadable file reads
// as. Losing the file returns the org to CEO-paced delivery, never to
// an unattended one.
type AutoRelease struct {
	Enabled bool
	// Delay is how long after a message's send time (Message.Date) it
	// releases. Zero releases it as soon as the scheduler sees it.
	// Meaningless when Enabled is false.
	Delay time.Duration
	// Since is the moment the setting took effect — the slider move
	// that made it. It decides which messages the setting governs:
	// those sent at or after it, up to the next setting's Since. Zero
	// means always: a file from before settings were dated, or a
	// setting written without a time.
	Since time.Time
}

// AutoReleaseSchedule is every slider setting that still governs
// something, oldest first; the last is the current one. A message is
// governed by the newest setting whose Since is not after its send
// time, and by none — off — when it predates them all.
//
// Why a schedule rather than one value: moving the slider must not
// re-time what is already queued. Sliding from Off to 2m must not
// release, at once, everything that piled up while the slider was
// off, when holding it was the whole point of Off. So a setting
// applies to what is sent under it. What was queued before keeps the
// deadline it had, or stays for the CEO to release by hand if it had
// none.
type AutoReleaseSchedule []AutoRelease

// Current is the setting the slider shows: the newest one. Off when
// the schedule is empty.
func (h AutoReleaseSchedule) Current() AutoRelease {
	if len(h) == 0 {
		return AutoRelease{}
	}
	return h[len(h)-1]
}

// At is the setting governing a message sent at t: the newest whose
// Since is not after t. Off when every setting is newer than t.
func (h AutoReleaseSchedule) At(t time.Time) AutoRelease {
	for i := len(h) - 1; i >= 0; i-- {
		if !h[i].Since.After(t) {
			return h[i]
		}
	}
	return AutoRelease{}
}

// Active reports whether any setting can put a message on the clock.
// False means no queued message has a deadline, whatever its date.
func (h AutoReleaseSchedule) Active() bool {
	for _, a := range h {
		if a.Enabled {
			return true
		}
	}
	return false
}

// autoReleaseFile is the on-disk setting. A file of its own rather
// than a field on message_queue.json: the queue is rewritten on every
// route and release under its own lock, and a slider position has no
// business inside that critical section.
const autoReleaseFile = "auto_release.json"

// autoReleaseSettings is the on-disk shape: the current setting at the
// top level, and under "earlier" the settings before it that still
// govern something queued, oldest first. Delay is a Go duration string
// ("2m0s") or "off" and Since is RFC 3339, so the file reads and
// edits by hand; a file with only "delay" is one from before settings
// were dated and reads as a setting that has always held.
type autoReleaseSettings struct {
	Delay   string               `json:"delay"`
	Since   string               `json:"since,omitempty"`
	Earlier []autoReleaseSetting `json:"earlier,omitempty"`
}

type autoReleaseSetting struct {
	Delay string `json:"delay"`
	Since string `json:"since,omitempty"`
}

// autoReleaseOff is the on-disk spelling of a disabled setting.
const autoReleaseOff = "off"

// ReadAutoRelease returns the current setting. Any problem reading or
// parsing the file reads as off (see AutoRelease).
func (s *FSStore) ReadAutoRelease() AutoRelease {
	return s.ReadAutoReleaseSchedule().Current()
}

// ReadAutoReleaseSchedule returns every setting still in force, oldest
// first. Any problem reading or parsing the file — the current setting
// or an earlier one — reads as an empty schedule, which is off for
// every message.
func (s *FSStore) ReadAutoReleaseSchedule() AutoReleaseSchedule {
	b, err := os.ReadFile(s.path(autoReleaseFile))
	if err != nil {
		return nil
	}
	var cfg autoReleaseSettings
	if err := json.Unmarshal(b, &cfg); err != nil {
		return nil
	}
	out := make(AutoReleaseSchedule, 0, len(cfg.Earlier)+1)
	for _, e := range cfg.Earlier {
		a, ok := parseAutoRelease(e)
		if !ok {
			return nil
		}
		out = append(out, a)
	}
	a, ok := parseAutoRelease(autoReleaseSetting{Delay: cfg.Delay, Since: cfg.Since})
	if !ok {
		return nil
	}
	return append(out, a)
}

func parseAutoRelease(e autoReleaseSetting) (AutoRelease, bool) {
	var a AutoRelease
	if e.Since != "" {
		t, err := time.Parse(time.RFC3339Nano, e.Since)
		if err != nil {
			return AutoRelease{}, false
		}
		a.Since = t
	}
	if e.Delay == autoReleaseOff {
		return a, true
	}
	d, err := time.ParseDuration(e.Delay)
	if err != nil || d < 0 {
		return AutoRelease{}, false
	}
	a.Enabled, a.Delay = true, d
	return a, true
}

func formatAutoRelease(a AutoRelease) autoReleaseSetting {
	e := autoReleaseSetting{Delay: autoReleaseOff}
	if a.Enabled {
		e.Delay = a.Delay.String()
	}
	if !a.Since.IsZero() {
		e.Since = a.Since.UTC().Format(time.RFC3339Nano)
	}
	return e
}

// WriteAutoRelease records a slider move: a takes effect at a.Since,
// for the messages sent from then on. A negative delay is refused;
// there is no meaning to "release before it was sent".
//
// Off cancels. An off setting is the only one kept, so every earlier
// deadline is gone and everything queued is the CEO's to release
// again. A delay is added after the settings before it, which keep
// governing the messages sent under them, except for the ones nothing
// can still be under: a setting is dropped once the next one has
// taken over and every deadline it could have set has passed. Writing
// the setting that already holds changes nothing, so its Since stays
// where it was.
func (s *FSStore) WriteAutoRelease(a AutoRelease) error {
	if a.Enabled && a.Delay < 0 {
		return errors.New("auto-release: delay must not be negative")
	}
	sched := AutoReleaseSchedule{a}
	if a.Enabled {
		prev := s.ReadAutoReleaseSchedule()
		if cur := prev.Current(); len(prev) > 0 && cur.Enabled && cur.Delay == a.Delay {
			return nil
		}
		sched = append(prev, a).settle(a.Since)
	}
	cfg := autoReleaseSettings{Earlier: make([]autoReleaseSetting, 0, len(sched)-1)}
	for _, e := range sched[:len(sched)-1] {
		cfg.Earlier = append(cfg.Earlier, formatAutoRelease(e))
	}
	cur := formatAutoRelease(sched.Current())
	cfg.Delay, cfg.Since = cur.Delay, cur.Since
	body, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	return writeAtomic(s.path(autoReleaseFile), append(body, '\n'), 0o644)
}

// settle drops the oldest settings nothing can still be under, as of
// now. The oldest is dead when it is off (a message it governs reads
// as off with or without it, since off is what predating every
// setting means), or when the setting after it has taken over and
// every deadline it could have set — its delay past the takeover — is
// behind now. A setting the next one supersedes outright, because that
// next one is undated and so has always held, is dead too. The current
// setting is never dropped.
func (h AutoReleaseSchedule) settle(now time.Time) AutoReleaseSchedule {
	for len(h) > 1 {
		oldest, next := h[0], h[1]
		dead := !oldest.Enabled || next.Since.IsZero() ||
			!now.Before(next.Since.Add(oldest.Delay))
		if !dead {
			break
		}
		h = h[1:]
	}
	return h
}
