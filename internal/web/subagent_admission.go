// Package web (subagent_admission.go) — how many subagent CLIs may be
// alive in one agent pod at once.
//
// Every subagent is a `claude` process forked inside the PARENT's
// agent pod, so the pod's memory limit is shared by the parent's
// long-lived CLI and every subagent running underneath it.
// SubagentMaxBatch caps tasks per CALL, not concurrent jobs, and
// dispatch returns immediately — so without admission an agent could
// call subagent() five times in one turn and put fifteen CLIs in a
// cgroup sized for a handful.
//
// Depth is not the fix. Depth decides the SHAPE of delegation; this
// decides its COST, and the two are independent. With admission in
// place a deep tree is safe: it queues instead of multiplying.
//
// THE DEADLOCK THIS AVOIDS. A sub-lead occupies a slot and then blocks
// waiting for its own workers to get slots (nested dispatch is
// synchronous — a subagent is a one-shot process that cannot be woken,
// so it must block). If every slot were held by a blocked sub-lead,
// no worker could ever start and the pod would wedge permanently.
//
// The answer is a ceiling on tier-1 admissions that is strictly below the
// total. Tier 1 can never occupy every slot, so a worker can always
// get in, which lets some sub-lead finish, which frees a slot. Progress
// is guaranteed without any deadlock detection, timeout, or preemption.

package web

import (
	"context"
	"sync"
)

// Pod-wide concurrency budget. These two numbers, the agent-pod
// MemoryLimit (4Gi), and subagentMaxOldSpaceMB are ONE budget — a
// change to any of them should be a change to all of them.
//
// At the ceiling a pod holds the parent CLI plus six subagent CLIs,
// each with its own `kivali mcp` child.
const (
	// subagentPodConcurrency is the total number of subagent CLIs
	// that may run at once in a single agent pod.
	subagentPodConcurrency = 6

	// subagentTier1Concurrency caps how many of those may be tier-1
	// (dispatched by the durable agent itself). MUST stay strictly
	// below subagentPodConcurrency — the gap is what guarantees a
	// nested worker can always be admitted. See the deadlock note
	// above; setting these equal reintroduces it.
	subagentTier1Concurrency = 4
)

// subagentAdmission bounds concurrent subagent CLIs per parent. Per
// parent is per pod: each durable agent has exactly one agent pod, and
// every subagent it owns — at any depth — forks inside that pod.
type subagentAdmission struct {
	mu    sync.Mutex
	total int
	tier1 int

	inUse   map[string]int
	atTier1 map[string]int
	waiters map[string][]chan struct{}

	// onWait, when set, is called after a caller has parked itself on
	// the waiter list and before it blocks. Tests use it to observe
	// that a goroutine really is queued rather than sleeping and
	// hoping. Always nil in production.
	onWait func(parent string, depth int)
}

func newSubagentAdmission(total, tier1 int) *subagentAdmission {
	if total < 1 {
		total = 1
	}
	// Clamp rather than reject: a misconfiguration should cost
	// throughput, not correctness. tier1 == total is the deadlock
	// case, so the clamp is strict.
	if tier1 < 1 {
		tier1 = 1
	}
	if tier1 >= total {
		tier1 = total - 1
		if tier1 < 1 {
			tier1 = 1
		}
	}
	return &subagentAdmission{
		total:   total,
		tier1:   tier1,
		inUse:   map[string]int{},
		atTier1: map[string]int{},
		waiters: map[string][]chan struct{}{},
	}
}

// admitLocked reports whether a job at this depth may start right now.
// Callers hold a.mu.
func (a *subagentAdmission) admitLocked(parent string, depth int) bool {
	if a.inUse[parent] >= a.total {
		return false
	}
	// Only tier 1 is rationed. Deeper tiers take any free slot, which
	// is precisely what keeps a blocked sub-lead from wedging the pod.
	if depth <= 1 && a.atTier1[parent] >= a.tier1 {
		return false
	}
	return true
}

// acquire blocks until a slot is free for a job at the given depth, or
// until ctx is cancelled. A cancelled wait returns ctx.Err() and
// consumes nothing, so a job cancelled while queued costs no slot.
func (a *subagentAdmission) acquire(ctx context.Context, parent string, depth int) error {
	for {
		// Check the context first so a job cancelled before it ever
		// queued fails as cancelled rather than starting.
		if err := ctx.Err(); err != nil {
			return err
		}
		a.mu.Lock()
		if a.admitLocked(parent, depth) {
			a.inUse[parent]++
			if depth <= 1 {
				a.atTier1[parent]++
			}
			a.mu.Unlock()
			return nil
		}
		ch := make(chan struct{})
		a.waiters[parent] = append(a.waiters[parent], ch)
		a.mu.Unlock()

		if a.onWait != nil {
			a.onWait(parent, depth)
		}

		select {
		case <-ctx.Done():
			a.dropWaiter(parent, ch)
			return ctx.Err()
		case <-ch:
			// A slot was released. Loop and re-test: the freed slot
			// may not be one this depth is allowed to take, and other
			// waiters are racing for it too.
		}
	}
}

// release returns a slot and wakes everyone waiting on this parent.
// Waking all of them rather than handing off to one keeps the tier
// rules in a single place (admitLocked) — a targeted handoff would
// have to re-implement them to pick the right waiter, and at these
// sizes the re-check is free.
func (a *subagentAdmission) release(parent string, depth int) {
	a.mu.Lock()
	if a.inUse[parent] > 0 {
		a.inUse[parent]--
	}
	if depth <= 1 && a.atTier1[parent] > 0 {
		a.atTier1[parent]--
	}
	waiters := a.waiters[parent]
	delete(a.waiters, parent)
	a.mu.Unlock()
	for _, ch := range waiters {
		close(ch)
	}
}

// dropWaiter removes a cancelled waiter's channel. Safe to call for a
// channel that release already took off the list.
func (a *subagentAdmission) dropWaiter(parent string, ch chan struct{}) {
	a.mu.Lock()
	defer a.mu.Unlock()
	list := a.waiters[parent]
	for i, c := range list {
		if c == ch {
			a.waiters[parent] = append(list[:i], list[i+1:]...)
			return
		}
	}
}

// inFlight reports how many slots parent currently holds. Test and
// diagnostics helper.
func (a *subagentAdmission) inFlight(parent string) int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.inUse[parent]
}
