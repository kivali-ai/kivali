package web

import (
	"context"
	"net/http"
	"sync"
	"testing"

	"github.com/kivali-ai/kivali/internal/agent"
	"github.com/kivali-ai/kivali/internal/store"
)

// fakeAgentPodLifecycle is a recording stand-in for the agent-pod
// provisioner. Captures Provision / Destroy slugs so tests can assert
// the parallel-call wiring.
type fakeAgentPodLifecycle struct {
	mu        sync.Mutex
	provision []string
	destroy   []string
	provErr   error
	destErr   error
}

func (f *fakeAgentPodLifecycle) Provision(_ context.Context, slug string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.provision = append(f.provision, slug)
	return f.provErr
}

func (f *fakeAgentPodLifecycle) Destroy(_ context.Context, slug string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.destroy = append(f.destroy, slug)
	return f.destErr
}

func (f *fakeAgentPodLifecycle) provisionedSlugs() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.provision...)
}

func (f *fakeAgentPodLifecycle) destroyedSlugs() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.destroy...)
}

// TestApplyHireProvisionsAgentPod locks in the wiring: when
// Runtime.AgentPod is set, ApplyHire fires its Provision so a fresh
// hire gets its pod without a separate manual step.
func TestApplyHireProvisionsAgentPod(t *testing.T) {
	srv := newTestServer(t)
	if err := srv.Store.CreateAgent(store.Agent{Slug: "chief-of-staff", Role: "CoS", ReportsTo: "ceo"}, "k"); err != nil {
		t.Fatalf("seed CoS: %v", err)
	}
	rt := agent.NewRuntime(srv.Store, agent.RuntimeDefaults{AgentModel: "mock-large-0"})
	pod := &fakeAgentPodLifecycle{}
	rt.AgentPod = pod

	if _, err := rt.ApplyHire(store.Hire{
		Slug: "alice", Role: "Analyst", ReportsTo: "chief-of-staff", Body: "do the analysis",
	}); err != nil {
		t.Fatalf("ApplyHire: %v", err)
	}
	if got := pod.provisionedSlugs(); len(got) != 1 || got[0] != "alice" {
		t.Errorf("AgentPod.Provision calls = %v, want [alice]", got)
	}
}

// TestApplyHireSucceedsWithoutAgentPod: with Runtime.AgentPod nil,
// ApplyHire still completes.
func TestApplyHireSucceedsWithoutAgentPod(t *testing.T) {
	srv := newTestServer(t)
	if err := srv.Store.CreateAgent(store.Agent{Slug: "chief-of-staff", Role: "CoS", ReportsTo: "ceo"}, "k"); err != nil {
		t.Fatalf("seed CoS: %v", err)
	}
	rt := agent.NewRuntime(srv.Store, agent.RuntimeDefaults{AgentModel: "mock-large-0"})
	// AgentPod intentionally unset.

	if _, err := rt.ApplyHire(store.Hire{
		Slug: "alice", Role: "Analyst", ReportsTo: "chief-of-staff", Body: "do the analysis",
	}); err != nil {
		t.Fatalf("ApplyHire: %v", err)
	}
	if _, err := srv.Store.GetAgent("alice"); err != nil {
		t.Errorf("alice should exist post-hire: %v", err)
	}
}

// TestApplyHireAgentPodErrorIsWarning asserts that an AgentPod
// Provision failure surfaces as a warning, not a hire-blocking
// error. Keeps
// hires unblocked when the agent-pod cluster path has a transient
// hiccup.
func TestApplyHireAgentPodErrorIsWarning(t *testing.T) {
	srv := newTestServer(t)
	if err := srv.Store.CreateAgent(store.Agent{Slug: "chief-of-staff", Role: "CoS", ReportsTo: "ceo"}, "k"); err != nil {
		t.Fatalf("seed CoS: %v", err)
	}
	rt := agent.NewRuntime(srv.Store, agent.RuntimeDefaults{AgentModel: "mock-large-0"})
	rt.AgentPod = &fakeAgentPodLifecycle{provErr: context.Canceled}

	warn, err := rt.ApplyHire(store.Hire{
		Slug: "alice", Role: "Analyst", ReportsTo: "chief-of-staff", Body: "x",
	})
	if err != nil {
		t.Fatalf("ApplyHire returned error, want warning: %v", err)
	}
	if warn == "" {
		t.Errorf("expected non-empty warning")
	}
	if _, err := srv.Store.GetAgent("alice"); err != nil {
		t.Errorf("alice should exist post-hire even on agentpod warning: %v", err)
	}
}

// TestOffboardDestroysAgentPod locks in cleanup parity for the
// offboard approval: when Server.AgentPod is set, approving an
// offboard tears the pod down in the same operation that archives the
// agent. This moved off the Fire button but the pod still has to go —
// otherwise an archived agent leaves a running pod behind.
func TestOffboardDestroysAgentPod(t *testing.T) {
	srv, _ := newTurnServer(t)
	_ = srv.Store.CreateAgent(store.Agent{Slug: "chief-of-staff", Role: "CoS"}, "# role\n")
	if err := srv.Store.CreateAgent(store.Agent{Slug: "alice", Role: "Analyst", ReportsTo: "chief-of-staff"}, "k"); err != nil {
		t.Fatalf("seed alice: %v", err)
	}
	pod := &fakeAgentPodLifecycle{}
	srv.AgentPod = pod

	rel := seedOffboardProposal(t, srv, "alice")
	if rr := approve(t, srv, rel); rr.Code != http.StatusOK {
		t.Fatalf("code = %d, body = %s", rr.Code, rr.Body.String())
	}
	if got := pod.destroyedSlugs(); len(got) != 1 || got[0] != "alice" {
		t.Errorf("AgentPod.Destroy calls = %v, want [alice]", got)
	}
}

// A refused offboard must not destroy the pod: the agent is still
// active, so tearing down its pod would take a live agent offline
// without archiving it — the worst of both outcomes.
func TestRefusedOffboardLeavesAgentPodAlone(t *testing.T) {
	srv, _ := newTurnServer(t)
	_ = srv.Store.CreateAgent(store.Agent{Slug: "chief-of-staff", Role: "CoS"}, "# role\n")
	_ = srv.Store.CreateAgent(store.Agent{Slug: "manager", Role: "Lead", ReportsTo: "chief-of-staff"}, "k")
	_ = srv.Store.CreateAgent(store.Agent{Slug: "bob", Role: "IC", ReportsTo: "manager"}, "k")
	pod := &fakeAgentPodLifecycle{}
	srv.AgentPod = pod

	rel := seedOffboardProposal(t, srv, "manager")
	if rr := approve(t, srv, rel); rr.Code != http.StatusConflict {
		t.Fatalf("code = %d, want 409", rr.Code)
	}
	if got := pod.destroyedSlugs(); len(got) != 0 {
		t.Errorf("AgentPod.Destroy calls = %v, want none on a refused offboard", got)
	}
}

// TestApplyReorgMovesReportingLine locks the happy path: a clean move
// flips ReportsTo on disk, lands in the Applied list, and produces no
// failures.
func TestApplyReorgMovesReportingLine(t *testing.T) {
	srv := newTestServer(t)
	for _, a := range []store.Agent{
		{Slug: "chief-of-staff", Role: "CoS", ReportsTo: "ceo"},
		{Slug: "alice", Role: "Analyst", ReportsTo: "chief-of-staff"},
		{Slug: "bob", Role: "Lead", ReportsTo: "ceo"},
	} {
		if err := srv.Store.CreateAgent(a, "x"); err != nil {
			t.Fatalf("seed %s: %v", a.Slug, err)
		}
	}
	rt := agent.NewRuntime(srv.Store, agent.RuntimeDefaults{})

	got, err := rt.ApplyReorg(store.Reorg{Moves: []store.ReorgMove{
		{Slug: "alice", NewManager: "bob"},
	}})
	if err != nil {
		t.Fatalf("ApplyReorg: %v", err)
	}
	if len(got.Applied) != 1 || got.Applied[0].Slug != "alice" {
		t.Errorf("Applied = %+v, want [alice→bob]", got.Applied)
	}
	if len(got.Failed) != 0 {
		t.Errorf("Failed = %+v, want none", got.Failed)
	}
	a, _ := srv.Store.GetAgent("alice")
	if a.ReportsTo != "bob" {
		t.Errorf("alice.ReportsTo = %q, want %q", a.ReportsTo, "bob")
	}
}

// TestApplyReorgIsBestEffort: one bad move doesn't sink the rest; the
// good move lands and the bad one shows up in Failed with a reason.
func TestApplyReorgIsBestEffort(t *testing.T) {
	srv := newTestServer(t)
	for _, a := range []store.Agent{
		{Slug: "chief-of-staff", Role: "CoS", ReportsTo: "ceo"},
		{Slug: "alice", Role: "Analyst", ReportsTo: "chief-of-staff"},
		{Slug: "bob", Role: "Lead", ReportsTo: "ceo"},
	} {
		if err := srv.Store.CreateAgent(a, "x"); err != nil {
			t.Fatalf("seed %s: %v", a.Slug, err)
		}
	}
	rt := agent.NewRuntime(srv.Store, agent.RuntimeDefaults{})

	got, err := rt.ApplyReorg(store.Reorg{Moves: []store.ReorgMove{
		{Slug: "alice", NewManager: "bob"},     // good
		{Slug: "ghost", NewManager: "bob"},     // unknown target
		{Slug: "alice", NewManager: "missing"}, // unknown manager
	}})
	if err != nil {
		t.Fatalf("ApplyReorg: %v", err)
	}
	if len(got.Applied) != 1 || got.Applied[0].Slug != "alice" {
		t.Errorf("Applied = %+v, want one [alice→bob]", got.Applied)
	}
	if len(got.Failed) != 2 {
		t.Errorf("Failed = %+v, want 2 entries", got.Failed)
	}
	a, _ := srv.Store.GetAgent("alice")
	if a.ReportsTo != "bob" {
		t.Errorf("alice.ReportsTo = %q, want %q (good move should still land)", a.ReportsTo, "bob")
	}
}

// TestApplyReorgRefusesCycle: moving a manager under one of their own
// reports would create a cycle and must be refused (caught at apply
// time, not parse — parse only knows the proposal, not the existing
// chain).
func TestApplyReorgRefusesCycle(t *testing.T) {
	srv := newTestServer(t)
	for _, a := range []store.Agent{
		{Slug: "chief-of-staff", Role: "CoS", ReportsTo: "ceo"},
		{Slug: "alice", Role: "Lead", ReportsTo: "ceo"},
		{Slug: "bob", Role: "Analyst", ReportsTo: "alice"},
	} {
		if err := srv.Store.CreateAgent(a, "x"); err != nil {
			t.Fatalf("seed %s: %v", a.Slug, err)
		}
	}
	rt := agent.NewRuntime(srv.Store, agent.RuntimeDefaults{})

	// Putting alice under bob would loop alice → bob → alice.
	got, err := rt.ApplyReorg(store.Reorg{Moves: []store.ReorgMove{
		{Slug: "alice", NewManager: "bob"},
	}})
	if err != nil {
		t.Fatalf("ApplyReorg: %v", err)
	}
	if len(got.Applied) != 0 {
		t.Errorf("cycle move must not apply: %+v", got.Applied)
	}
	if len(got.Failed) != 1 {
		t.Fatalf("Failed = %+v, want 1", got.Failed)
	}
	a, _ := srv.Store.GetAgent("alice")
	if a.ReportsTo != "ceo" {
		t.Errorf("alice.ReportsTo = %q, want unchanged %q", a.ReportsTo, "ceo")
	}
}

// TestApplyReorgIdempotent: a move whose new_manager already equals the
// current ReportsTo lands as a no-op success, not a failure. CoS may
// re-propose stragglers based on a stale view; we don't want that to
// look like an error.
func TestApplyReorgIdempotent(t *testing.T) {
	srv := newTestServer(t)
	for _, a := range []store.Agent{
		{Slug: "chief-of-staff", Role: "CoS", ReportsTo: "ceo"},
		{Slug: "alice", Role: "Analyst", ReportsTo: "chief-of-staff"},
	} {
		if err := srv.Store.CreateAgent(a, "x"); err != nil {
			t.Fatalf("seed %s: %v", a.Slug, err)
		}
	}
	rt := agent.NewRuntime(srv.Store, agent.RuntimeDefaults{})

	got, err := rt.ApplyReorg(store.Reorg{Moves: []store.ReorgMove{
		{Slug: "alice", NewManager: "chief-of-staff"},
	}})
	if err != nil {
		t.Fatalf("ApplyReorg: %v", err)
	}
	if len(got.Applied) != 1 || len(got.Failed) != 0 {
		t.Errorf("no-op should be Applied; got %+v", got)
	}
}

// TestOffboardDestroyAgentPodErrorDoesNotBlock asserts that an
// AgentPod Destroy failure is logged but does not fail the approval.
// By the time Destroy runs the agent is already archived, so 5xx-ing
// here would show the CEO an error for an action that succeeded and
// would skip the approval response CoS is waiting on. A leaked pod is
// cleanable by hand; a lost response is not.
func TestOffboardDestroyAgentPodErrorDoesNotBlock(t *testing.T) {
	srv, _ := newTurnServer(t)
	_ = srv.Store.CreateAgent(store.Agent{Slug: "chief-of-staff", Role: "CoS"}, "# role\n")
	if err := srv.Store.CreateAgent(store.Agent{Slug: "alice", Role: "Analyst", ReportsTo: "chief-of-staff"}, "k"); err != nil {
		t.Fatalf("seed alice: %v", err)
	}
	srv.AgentPod = &fakeAgentPodLifecycle{destErr: context.Canceled}

	rel := seedOffboardProposal(t, srv, "alice")
	rr := approve(t, srv, rel)
	if rr.Code != http.StatusOK {
		t.Errorf("code = %d, body = %s — offboard must not block on destroy error", rr.Code, rr.Body.String())
	}
	if _, err := srv.Store.GetArchivedAgent("alice"); err != nil {
		t.Errorf("alice should be archived even on agentpod destroy error: %v", err)
	}
	resps, _ := srv.Store.ListMessages(store.MessageFilter{Type: store.MsgCEOApprovalResponse})
	if len(resps) != 1 {
		t.Errorf("the approval response must still be written; got %d", len(resps))
	}
}
