package web

import (
	"context"
	"errors"
	"log"
	"path/filepath"
	"sort"
	"time"

	"github.com/kivali-ai/kivali/internal/store"
)

// autoReleaseDetent is one stop on the Queue's auto-release control.
// Key is the wire spelling (the API's value, the snapshot's
// inbox.auto_release); Label is its words.
type autoReleaseDetent struct {
	Key   string
	Label string
	Delay time.Duration
	Off   bool
}

// autoReleaseDetents is the slider, left to right: release at once,
// four delays, and off. Off is last so the slider reads as "how long
// does a message wait" with "forever" at the far end. The list is the
// single source of truth for the handler's validation and the
// snapshot's spelling — the client never sees a
// duration, only a key.
var autoReleaseDetents = []autoReleaseDetent{
	{Key: "now", Label: "Now", Delay: 0},
	{Key: "30s", Label: "30s", Delay: 30 * time.Second},
	{Key: "2m", Label: "2m", Delay: 2 * time.Minute},
	{Key: "5m", Label: "5m", Delay: 5 * time.Minute},
	{Key: "20m", Label: "20m", Delay: 20 * time.Minute},
	{Key: "off", Label: "Off", Off: true},
}

// autoReleaseDetentByKey looks a detent up by its wire spelling.
func autoReleaseDetentByKey(key string) (autoReleaseDetent, bool) {
	for _, d := range autoReleaseDetents {
		if d.Key == key {
			return d, true
		}
	}
	return autoReleaseDetent{}, false
}

// autoReleaseKey is the detent a stored setting shows as. An exact
// match wins; a delay that is on no detent (the file was edited by
// hand) shows as the nearest one so the slider still has a position,
// while the scheduler keeps using the stored delay as written.
func autoReleaseKey(a store.AutoRelease) string {
	if !a.Enabled {
		return "off"
	}
	best := autoReleaseDetents[0]
	for _, d := range autoReleaseDetents {
		if d.Off {
			continue
		}
		if d.Delay == a.Delay {
			return d.Key
		}
		if absDuration(d.Delay-a.Delay) < absDuration(best.Delay-a.Delay) {
			best = d
		}
	}
	return best.Key
}

func absDuration(d time.Duration) time.Duration {
	if d < 0 {
		return -d
	}
	return d
}

// errUnknownDetent is setAutoRelease's refusal of a key that is not on
// the slider.
var errUnknownDetent = errors.New("unknown auto-release detent")

// setAutoRelease moves the slider to the detent named key: persists
// the setting as of now, wakes the scheduler and republishes the
// snapshot. Serves POST /api/v1/auto-release.
func (s *Server) setAutoRelease(key string) error {
	d, ok := autoReleaseDetentByKey(key)
	if !ok {
		return errUnknownDetent
	}
	a := store.AutoRelease{Enabled: !d.Off, Delay: d.Delay, Since: s.clk().Now()}
	if err := s.Store.WriteAutoRelease(a); err != nil {
		return err
	}
	s.NotifyAutoRelease()
	s.NotifyOrgState()
	return nil
}

// NotifyAutoRelease wakes the auto-release scheduler so it re-reads
// the setting and the queue. Non-blocking; safe from any goroutine;
// a no-op when the scheduler is not running.
func (s *Server) NotifyAutoRelease() {
	if s.autoReleaseCh == nil {
		return
	}
	select {
	case s.autoReleaseCh <- struct{}{}:
	default:
	}
}

// autoReleaseRetry is how long the scheduler waits before looking
// again after a pass it could not complete: the queue would not read,
// or a release failed outright. Nothing else re-arms the timer in
// that state — the next queue write would, but a queue nobody
// touches would otherwise sit with its overdue messages until one
// did.
const autoReleaseRetry = 30 * time.Second

// StartAutoRelease starts the scheduler that releases queued messages
// once their delay has run out. Returns a stop function the caller
// invokes during shutdown; it waits for an in-flight pass to finish
// so no release is cut off half-delivered.
//
// The scheduler needs the engine (Runtime + Messenger) to deliver.
// Without one — test fixtures, a deployment with no backend — it is a
// no-op stop, matching the "no engine configured" gate on every
// release handler.
func (s *Server) StartAutoRelease() func() {
	if s.Store == nil || s.Messenger == nil || s.Runtime == nil {
		return func() {}
	}
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		s.runAutoRelease(stop)
	}()
	return func() {
		close(stop)
		<-done
	}
}

// runAutoRelease is the scheduler loop. One pass at boot — a restart
// must not lose the deadlines that were pending when it went down,
// and those deadlines live in the message dates on disk, so nothing
// is lost as long as a pass runs before the first wait. Then: wait
// for a wake or the next deadline, whichever comes first, and pass
// again.
//
// One timer, re-armed at the earliest deadline after every pass and
// disarmed when there is nothing to wait for. A wake (queue write,
// slider move) does not fire anything by itself; it makes the loop
// look again, and the look is what releases. The slider moving to Off
// is therefore just a pass in which nothing is due any more.
func (s *Server) runAutoRelease(stop <-chan struct{}) {
	timer := s.clk().NewTimer(autoReleaseRetry)
	timer.Stop()
	for {
		if wait, ok := s.autoReleasePass(); ok {
			timer.Reset(wait)
		} else {
			timer.Stop()
		}
		select {
		case <-stop:
			timer.Stop()
			return
		case <-s.autoReleaseCh:
		case <-timer.C():
		}
	}
}

// autoReleasePass releases every queued message whose deadline has
// passed and reports how long until the next one is due. ok is false
// when nothing is waiting on the clock: no setting in force can put a
// message on it, the queue is empty, or everything queued that has a
// deadline is already released by the time the pass ends.
//
// Each message's deadline comes from autoReleaseDue: its send time
// plus the delay the slider held then. A message sent while the
// slider was off has no deadline and is never due here, however long
// it has waited; the CEO releases it.
//
// Pointers for a recipient who no longer exists are skipped rather
// than released: the drain leaves those in place (there is nobody to
// deliver to), and treating them as due would re-run the same no-op
// release on every queue write.
func (s *Server) autoReleasePass() (wait time.Duration, ok bool) {
	sched := s.Store.ReadAutoReleaseSchedule()
	if !sched.Active() {
		return 0, false
	}
	q, err := s.Store.ReadMessageQueue()
	if err != nil {
		log.Printf("auto-release: read queue: %v", err)
		return autoReleaseRetry, true
	}
	now := s.clk().Now()
	var overdue []string
	var next time.Time
	seen := map[string]bool{}
	for slug, rt := range q.Agents {
		if len(rt.Inbox) == 0 {
			continue
		}
		if _, gerr := s.Store.GetAgent(slug); gerr != nil {
			continue
		}
		for _, p := range rt.Inbox {
			if seen[p] {
				continue
			}
			seen[p] = true
			m, rerr := s.Store.ReadMessage(filepath.Join(s.Store.Root(), p))
			if rerr != nil {
				// The drain drops an unreadable pointer the next time
				// anything is released to this agent; there is no
				// deadline to compute for it here.
				log.Printf("auto-release: read %s: %v", p, rerr)
				continue
			}
			deadline, has := autoReleaseDue(sched, m)
			if !has {
				continue
			}
			if deadline.After(now) {
				if next.IsZero() || deadline.Before(next) {
					next = deadline
				}
				continue
			}
			overdue = append(overdue, p)
		}
	}
	if len(overdue) > 0 {
		sort.Strings(overdue)
		_, _, warnings, rerr := s.Messenger.ReleaseSelected(context.Background(), overdue, nil, s.deliveryHooks())
		if rerr != nil {
			log.Printf("auto-release: %v", rerr)
			return autoReleaseRetry, true
		}
		for _, w := range warnings {
			log.Printf("auto-release: %s", w)
		}
		s.NotifyOrgState()
	}
	if next.IsZero() {
		return 0, false
	}
	return next.Sub(now), true
}

// autoReleaseDue is when a queued message releases on its own: its
// send time (Message.Date — a publish routes inside the tool call, so
// that is also when the pointer was queued) plus the delay the slider
// held at that moment. Not ok for a message sent while the slider was
// off, or before it was ever moved: that one waits for the CEO.
//
// A slider move since does not change the answer. The card's
// countdown and the scheduler's timer both come from here, so they
// agree, and neither jumps when the slider does — except to Off,
// which cancels every deadline (see store.WriteAutoRelease).
func autoReleaseDue(sched store.AutoReleaseSchedule, m store.Message) (time.Time, bool) {
	a := sched.At(m.Date)
	if !a.Enabled {
		return time.Time{}, false
	}
	return m.Date.Add(a.Delay), true
}
