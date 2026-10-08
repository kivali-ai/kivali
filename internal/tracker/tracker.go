// Package tracker applies a change to the assignment tracker end to end:
// the rules, the file, and the wakes the change owes, routed as
// assignment_event messages through the same pipe every other message
// takes. See docs/developers/assignments.md.
//
// The order inside one change is the whole design of this package:
//
//  1. Under the assignment lock: read every assignment, apply the rules, write
//     the one file that changed, and write the wakes it owes as a
//     pending record. The lock is released here; nothing below needs
//     it, and the wake path may spawn an agent's turn.
//  2. Pull any queued wake the change has made moot (an assignment
//     still waiting for release when the assignment is dropped or handed
//     to someone else).
//  3. Route the wakes: an agent's change queues for the CEO to release,
//     the CEO's own change delivers at once, and a change that
//     concerns the CEO lands in their inbox. Each wake that lands is
//     struck from the pending record; the record is deleted when none
//     remain, and whatever a crash leaves behind is routed at boot.
package tracker

import (
	"context"
	"fmt"
	"log"
	"path/filepath"
	"strings"
	"time"

	"github.com/kivali-ai/kivali/internal/assignments"
	"github.com/kivali-ai/kivali/internal/messaging"
	"github.com/kivali-ai/kivali/internal/store"
)

// Service is the tracker bound to one store and messenger. Hooks are
// the web layer's delivery callbacks, used for the CEO's instant
// deliveries; nil hooks fall back to a plain chat append with no
// wake, which is what tests want.
type Service struct {
	Store     *store.FSStore
	Messenger *messaging.Messenger
	Hooks     messaging.DeliveryHooks
	// Now stamps changes. Defaults to the wall clock.
	Now func() time.Time
}

// New binds a Service.
func New(s *store.FSStore, m *messaging.Messenger, hooks messaging.DeliveryHooks) *Service {
	return &Service{Store: s, Messenger: m, Hooks: hooks}
}

func (t *Service) now() time.Time {
	if t.Now != nil {
		return t.Now().UTC()
	}
	return time.Now().UTC()
}

// active reports whether slug can be woken: the CEO always, otherwise
// an active agent.
func (t *Service) active(slug string) bool {
	if slug == assignments.CEO {
		return true
	}
	_, err := t.Store.GetAgent(slug)
	return err == nil
}

// Set reads every assignment. Reads take no lock: each file is written
// atomically and a change touches exactly one, so any snapshot is a
// consistent one.
func (t *Service) Set() (*assignments.Set, error) {
	return t.Store.ReadAssignmentSet()
}

// Create files an assignment. Every action returns the Change so the caller
// can say who was woken.
func (t *Service) Create(ctx context.Context, by string, in assignments.CreateInput) (assignments.Change, error) {
	return t.apply(ctx, by, func(r assignments.Rules) (assignments.Change, error) { return r.Create(by, in) })
}

// Update amends an assignment.
func (t *Service) Update(ctx context.Context, by string, id int, in assignments.UpdateInput) (assignments.Change, error) {
	return t.apply(ctx, by, func(r assignments.Rules) (assignments.Change, error) { return r.Update(id, by, in) })
}

// Close ends an assignment.
func (t *Service) Close(ctx context.Context, by string, id int, res assignments.Resolution, outcome string) (assignments.Change, error) {
	return t.apply(ctx, by, func(r assignments.Rules) (assignments.Change, error) { return r.Close(id, by, res, outcome) })
}

// Reopen returns a closed assignment to open.
func (t *Service) Reopen(ctx context.Context, by string, id int, note string) (assignments.Change, error) {
	return t.apply(ctx, by, func(r assignments.Rules) (assignments.Change, error) { return r.Reopen(id, by, note) })
}

// TransferCreator moves an open assignment's creatorship, as the CEO.
func (t *Service) TransferCreator(ctx context.Context, id int, to, note string) (assignments.Change, error) {
	return t.apply(ctx, assignments.CEO, func(r assignments.Rules) (assignments.Change, error) {
		return r.TransferCreator(id, assignments.CEO, to, note)
	})
}

// ReassignAll moves every open assignment `from` holds to `to`, and makes
// `to` the creator of every open assignment `from` filed, as the CEO, with
// one note on each. The offboard path calls it: an archived agent
// cannot be woken, so its work goes to whoever it reported to — and so
// do its asks, because the creator is who hears when an assignment closes
// and the only one besides the CEO who can amend, reassign or reopen
// it. Returns how many assignments moved either way.
func (t *Service) ReassignAll(ctx context.Context, from, to, note string) (int, error) {
	set, err := t.Set()
	if err != nil {
		return 0, err
	}
	n := 0
	for _, iss := range set.Query(assignments.Filter{Assignee: from}) {
		assignee := to
		if _, err := t.Update(ctx, assignments.CEO, iss.ID, assignments.UpdateInput{Assignee: &assignee, Note: note}); err != nil {
			return n, fmt.Errorf("reassign %s: %w", iss.Ref(), err)
		}
		n++
	}
	for _, iss := range set.Query(assignments.Filter{Creator: from}) {
		if _, err := t.TransferCreator(ctx, iss.ID, to, note); err != nil {
			return n, fmt.Errorf("transfer creator of %s: %w", iss.Ref(), err)
		}
		n++
	}
	return n, nil
}

// apply runs one change: rules and file under the lock, then the
// queue prune and the routing without it.
func (t *Service) apply(ctx context.Context, by string, fn func(assignments.Rules) (assignments.Change, error)) (assignments.Change, error) {
	t.Store.LockAssignments()
	set, err := t.Store.ReadAssignmentSet()
	if err != nil {
		t.Store.UnlockAssignments()
		return assignments.Change{}, err
	}
	ch, err := fn(assignments.Rules{Set: set, Active: t.active, Now: t.now()})
	if err != nil {
		t.Store.UnlockAssignments()
		return assignments.Change{}, err
	}
	if ch.PriorText != "" {
		if _, err := t.Store.SnapshotAssignmentText(ch.Assignment.ID, ch.PriorText); err != nil {
			t.Store.UnlockAssignments()
			return assignments.Change{}, fmt.Errorf("snapshot prior text of %s: %w", ch.Assignment.Ref(), err)
		}
	}
	if err := t.Store.WriteAssignment(ch.Assignment); err != nil {
		t.Store.UnlockAssignments()
		return assignments.Change{}, err
	}
	for _, other := range ch.Touched {
		// A flag on another record (the parent's nudge). The change
		// itself is on disk; a failure here loses one advisory line,
		// not the change, so it is logged and the ack stands.
		if err := t.Store.WriteAssignment(other); err != nil {
			log.Printf("tracker: write %s touched by the change to %s: %v", other.Ref(), ch.Assignment.Ref(), err)
		}
	}
	var pending *store.PendingWakes
	if len(ch.Wakes) > 0 {
		p := store.PendingWakes{Seq: ch.Entries[0].Seq, Assignment: ch.Assignment.ID, By: by, At: ch.Entries[0].TS, Wakes: ch.Wakes}
		if err := t.Store.WritePendingWakes(p); err != nil {
			t.Store.UnlockAssignments()
			return assignments.Change{}, fmt.Errorf("%s was written but its wakes could not be recorded: %w", ch.Assignment.Ref(), err)
		}
		pending = &p
	}
	t.Store.UnlockAssignments()

	unseen := t.pruneQueued(ch)
	if pending != nil {
		// An agent whose assignment was still queued never knew they
		// held the assignment, so "stop work on it" (or "it was closed")
		// would only puzzle them — unless the same wake also tells them
		// something else of theirs is now ready.
		if len(unseen) > 0 {
			kept := pending.Wakes[:0]
			for _, w := range pending.Wakes {
				if unseen[w.To] && len(w.Flips) == 0 {
					continue
				}
				kept = append(kept, w)
			}
			pending.Wakes = kept
		}
		if err := t.route(ctx, *pending, false); err != nil {
			// The change is on disk and the wakes are recorded; boot
			// will route what is left. The caller's ack still stands.
			log.Printf("tracker: route wakes for %s (seq %d): %v", ch.Assignment.Ref(), pending.Seq, err)
		}
	}
	return ch, nil
}

// pruneQueued pulls wakes the change has made moot out of the release
// queue: every queued event about an assignment that just closed (done or
// dropped, an old assignment or amendment is history either way), and
// the assignment still queued for an agent the assignment was just taken
// from. Nobody is woken for work that no longer exists. Returns the
// agents whose queued assignment was pulled, so the caller can spare
// them a "stop work" for work they never saw.
func (t *Service) pruneQueued(ch assignments.Change) map[string]bool {
	var recipients []string
	all := false
	for _, e := range ch.Entries {
		switch {
		case e.Op == assignments.OpClosed:
			all = true
		case e.Op == assignments.OpAssigned && e.From != "":
			recipients = append(recipients, e.From)
		}
	}
	if !all && len(recipients) == 0 {
		return nil
	}
	s := t.Store
	s.LockMessageQueue()
	defer s.UnlockMessageQueue()
	q, err := s.ReadMessageQueue()
	if err != nil {
		log.Printf("tracker: prune queued wakes for %s: %v", ch.Assignment.Ref(), err)
		return nil
	}
	var pulled []string
	unseen := map[string]bool{}
	for slug, rt := range q.Agents {
		if !all && !containsSlug(recipients, slug) {
			continue
		}
		kept := rt.Inbox[:0]
		for _, rel := range rt.Inbox {
			if !strings.Contains(filepath.Base(rel), "-"+string(store.MsgAssignmentEvent)+"-") {
				kept = append(kept, rel)
				continue
			}
			abs := filepath.Join(s.Root(), rel)
			m, err := s.ReadMessage(abs)
			if err != nil || m.Assignment == nil || m.Assignment.ID != ch.Assignment.ID {
				kept = append(kept, rel)
				continue
			}
			if m.Assignment.Op == string(assignments.OpCreated) || m.Assignment.Op == string(assignments.OpAssigned) || m.Assignment.Op == string(assignments.OpReopened) {
				unseen[slug] = true
			}
			pulled = append(pulled, abs)
		}
		rt.Inbox = kept
		q.Agents[slug] = rt
	}
	if len(pulled) == 0 {
		return nil
	}
	if err := s.WriteMessageQueue(q); err != nil {
		log.Printf("tracker: prune queued wakes for %s: %v", ch.Assignment.Ref(), err)
		return nil
	}
	for _, abs := range pulled {
		if _, err := s.MoveToRedacted(abs); err != nil {
			log.Printf("tracker: relocate pruned wake %s: %v", abs, err)
		}
	}
	return unseen
}

// route delivers a pending record's wakes and records what landed.
// replay is true when the record is one a crash left behind: a wake
// that already reached its recipient is then skipped rather than
// delivered twice.
func (t *Service) route(ctx context.Context, p store.PendingWakes, replay bool) error {
	var left []assignments.Wake
	var firstErr error
	for _, w := range p.Wakes {
		if !t.active(w.To) {
			log.Printf("tracker: wake for %s to %s dropped: not an active agent", assignments.Ref(p.Assignment), w.To)
			continue
		}
		if err := t.deliver(ctx, p, w, replay); err != nil {
			left = append(left, w)
			if firstErr == nil {
				firstErr = fmt.Errorf("wake %s to %s: %w", assignments.Ref(p.Assignment), w.To, err)
			}
		}
	}
	if len(left) == 0 {
		if err := t.Store.DeletePendingWakes(p.Seq); err != nil {
			return err
		}
		return firstErr
	}
	p.Wakes = left
	if err := t.Store.WritePendingWakes(p); err != nil {
		return err
	}
	return firstErr
}

// deliver writes one wake as an assignment_event and sends it the way its
// endpoints dictate. The message date is the change's timestamp, so a
// replay writes the same path; the queue dedupes by path on its own,
// and the two instant paths ask the recipient's chat first.
func (t *Service) deliver(ctx context.Context, p store.PendingWakes, w assignments.Wake, replay bool) error {
	msg := store.Message{
		Type:       store.MsgAssignmentEvent,
		Title:      w.Title,
		From:       p.By,
		To:         store.Recipients{w.To},
		Date:       p.At,
		Assignment: &store.AssignmentRef{ID: p.Assignment, Seq: p.Seq, Op: string(w.Op)},
		Body:       w.Body,
		Quiet:      w.Quiet,
	}
	path, err := t.Store.WriteMessage(msg)
	if err != nil {
		return err
	}
	msg.Path = path
	rel, err := filepath.Rel(t.Store.Root(), path)
	if err != nil {
		return err
	}
	rel = filepath.ToSlash(rel)
	if replay {
		if done, err := t.Store.ChatHasMessageRef(w.To, rel); err != nil {
			return err
		} else if done {
			log.Printf("tracker: wake for %s to %s was already delivered before the restart; not repeating it", assignments.Ref(p.Assignment), w.To)
			return nil
		}
	}
	if p.By == assignments.CEO && w.To != assignments.CEO {
		// The CEO's own change reaches the agent now, like a CEO reply
		// to any message: the release gate paces agents, not the CEO.
		if !t.Messenger.DeliverToAgent(ctx, w.To, msg, rel, t.Hooks) {
			return fmt.Errorf("instant delivery to %s failed", w.To)
		}
		return nil
	}
	out, err := t.Messenger.Route(ctx, msg)
	if err != nil {
		return err
	}
	for _, warn := range out.Warnings {
		log.Printf("tracker: route %s: %s", rel, warn)
	}
	return nil
}

// Reconcile routes whatever a crash left pending. Called at boot.
func (t *Service) Reconcile(ctx context.Context) error {
	pending, err := t.Store.ListPendingWakes()
	if err != nil {
		return err
	}
	for _, p := range pending {
		if err := t.route(ctx, p, true); err != nil {
			return err
		}
		log.Printf("tracker: routed %d pending wake(s) for %s left from before boot", len(p.Wakes), assignments.Ref(p.Assignment))
	}
	return nil
}

func containsSlug(slugs []string, s string) bool {
	for _, x := range slugs {
		if x == s {
			return true
		}
	}
	return false
}
