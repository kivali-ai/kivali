package web

import (
	"context"
	"testing"
)

// waitForWaiter returns a hook that closes a channel each time a
// caller parks on the waiter list, plus the channel. Tests use it to
// know a goroutine is genuinely blocked in acquire rather than merely
// not scheduled yet — the difference between a deterministic test and
// a sleep.
func waitForWaiter() (func(string, int), chan struct{}) {
	parked := make(chan struct{}, 16)
	return func(string, int) { parked <- struct{}{} }, parked
}

// TestAdmissionBoundsTotalConcurrency proves the pod-wide ceiling
// holds: the (total+1)-th job blocks rather than forking another CLI
// into a cgroup that cannot hold it.
func TestAdmissionBoundsTotalConcurrency(t *testing.T) {
	a := newSubagentAdmission(3, 2)
	hook, parked := waitForWaiter()
	a.onWait = hook
	ctx := context.Background()

	// Two tier-1 (the tier-1 ceiling) and one nested fills the pod.
	for i := 0; i < 2; i++ {
		if err := a.acquire(ctx, "alice", 1); err != nil {
			t.Fatalf("tier-1 acquire %d: %v", i, err)
		}
	}
	if err := a.acquire(ctx, "alice", 2); err != nil {
		t.Fatalf("nested acquire: %v", err)
	}
	if got := a.inFlight("alice"); got != 3 {
		t.Fatalf("inFlight = %d, want 3", got)
	}

	got := make(chan struct{})
	go func() {
		_ = a.acquire(ctx, "alice", 2)
		close(got)
	}()
	<-parked // the fourth job is genuinely queued

	select {
	case <-got:
		t.Fatal("acquire returned past the pod ceiling")
	default:
	}

	a.release("alice", 2)
	<-got // freeing a slot lets it through
}

// TestAdmissionTier1CannotFillThePod is the deadlock guard. A sub-lead
// blocks on its workers, so if tier-1 could occupy every slot the pod
// would wedge forever: no worker could start, so no sub-lead could
// finish, so no slot would ever free.
func TestAdmissionTier1CannotFillThePod(t *testing.T) {
	a := newSubagentAdmission(4, 2)
	hook, parked := waitForWaiter()
	a.onWait = hook
	ctx := context.Background()

	for i := 0; i < 2; i++ {
		if err := a.acquire(ctx, "alice", 1); err != nil {
			t.Fatalf("tier-1 acquire %d: %v", i, err)
		}
	}

	// A third tier-1 job must wait even though the pod has two free
	// slots — those are reserved for the workers of the sub-leads
	// already running.
	blocked := make(chan struct{})
	go func() {
		_ = a.acquire(ctx, "alice", 1)
		close(blocked)
	}()
	<-parked
	select {
	case <-blocked:
		t.Fatal("tier-1 acquire exceeded its ceiling")
	default:
	}

	// Nested work, meanwhile, gets in immediately. This is the
	// property that guarantees forward progress.
	for i := 0; i < 2; i++ {
		if err := a.acquire(ctx, "alice", 2); err != nil {
			t.Fatalf("nested acquire %d should not block: %v", i, err)
		}
	}
	if got := a.inFlight("alice"); got != 4 {
		t.Fatalf("inFlight = %d, want 4", got)
	}
}

// TestAdmissionClampsTier1BelowTotal guards the invariant by
// construction: a caller that passes equal values gets a usable gate,
// not a pod that deadlocks the first time a subagent delegates.
func TestAdmissionClampsTier1BelowTotal(t *testing.T) {
	a := newSubagentAdmission(4, 4)
	if a.tier1 >= a.total {
		t.Fatalf("tier1 = %d, total = %d: tier1 must stay strictly below total", a.tier1, a.total)
	}
}

// TestAdmissionCancelledWaitConsumesNothing proves a job cancelled
// while queued costs no slot. Without this, "dispatch five, change
// your mind" would leak the pod's capacity one cancellation at a time.
func TestAdmissionCancelledWaitConsumesNothing(t *testing.T) {
	a := newSubagentAdmission(1, 1)
	hook, parked := waitForWaiter()
	a.onWait = hook

	if err := a.acquire(context.Background(), "alice", 1); err != nil {
		t.Fatalf("first acquire: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	errc := make(chan error, 1)
	go func() { errc <- a.acquire(ctx, "alice", 2) }()
	<-parked
	cancel()

	if err := <-errc; err == nil {
		t.Fatal("cancelled acquire returned nil error")
	}
	if got := a.inFlight("alice"); got != 1 {
		t.Fatalf("inFlight = %d after cancelled wait, want 1", got)
	}
	// And the abandoned waiter must not be left on the list, where a
	// later release would close an orphaned channel.
	a.mu.Lock()
	n := len(a.waiters["alice"])
	a.mu.Unlock()
	if n != 0 {
		t.Fatalf("waiters = %d after cancel, want 0", n)
	}
}

// TestAdmissionIsPerParent confirms the budget is per agent pod. Two
// agents each get their own pod, so one agent's fan-out must not
// throttle another's.
func TestAdmissionIsPerParent(t *testing.T) {
	a := newSubagentAdmission(2, 1)
	ctx := context.Background()
	if err := a.acquire(ctx, "alice", 1); err != nil {
		t.Fatalf("alice acquire: %v", err)
	}
	if err := a.acquire(ctx, "bob", 1); err != nil {
		t.Fatalf("bob acquire should not be gated by alice: %v", err)
	}
	if got := a.inFlight("alice"); got != 1 {
		t.Fatalf("alice inFlight = %d, want 1", got)
	}
	if got := a.inFlight("bob"); got != 1 {
		t.Fatalf("bob inFlight = %d, want 1", got)
	}
}
