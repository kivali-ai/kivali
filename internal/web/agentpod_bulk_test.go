package web

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/kivali-ai/kivali/internal/clock"
	"github.com/kivali-ai/kivali/internal/files"
	"github.com/kivali-ai/kivali/internal/store"
)

// syncCheckingAgentPod records, at the moment each pod is provisioned,
// whether the directories that pod mounts by subPath already exist.
type syncCheckingAgentPod struct {
	mu      sync.Mutex
	root    string
	missing map[string][]string
}

func (f *syncCheckingAgentPod) Provision(_ context.Context, slug string) error {
	agentRoot := filepath.Join(f.root, "agents", slug)
	storage := files.StorageRoot(agentRoot)
	want := []string{
		filepath.Join(agentRoot, "attachments"), filepath.Join(agentRoot, "chats"),
		// The published trees mounted at artifacts/public and
		// artifacts/shared.
		files.PublishedRoot(f.root), files.PublishedDir(f.root, slug),
	}
	for _, d := range append([]string{"artifacts"}, files.CoreManagedDirs...) {
		want = append(want, filepath.Join(storage, filepath.FromSlash(d)))
	}
	var missing []string
	for _, p := range want {
		if info, err := os.Lstat(p); err != nil || !info.IsDir() {
			missing = append(missing, p)
		}
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.missing[slug] = missing
	return nil
}

func (f *syncCheckingAgentPod) Destroy(context.Context, string) error { return nil }

// Every subPath an agent's pod mounts exists before the pod is
// created: kubelet makes a missing one as root, and core could then
// never write into it. BulkProvisionAgentPods runs the agent's Sync
// first even when the tree lost a farm since the last one.
func TestBulkProvisionSyncsBeforeEachPod(t *testing.T) {
	srv := newTestServer(t)
	for _, slug := range []string{"chief-of-staff", "alice"} {
		if err := srv.Store.CreateAgent(store.Agent{Slug: slug, Role: slug, ReportsTo: "ceo"}, "role"); err != nil {
			t.Fatal(err)
		}
	}
	// A farm dir gone missing.
	aliceStorage := files.StorageRoot(filepath.Join(srv.Store.Root(), "agents", "alice"))
	if err := os.RemoveAll(filepath.Join(aliceStorage, "episodes")); err != nil {
		t.Fatal(err)
	}

	pod := &syncCheckingAgentPod{root: srv.Store.Root(), missing: map[string][]string{}}
	srv.AgentPod = pod
	srv.BulkProvisionAgentPods(context.Background())

	for _, slug := range []string{"chief-of-staff", "alice"} {
		missing, provisioned := pod.missing[slug]
		if !provisioned {
			t.Errorf("%s: not provisioned", slug)
			continue
		}
		if len(missing) != 0 {
			t.Errorf("%s: provisioned before these existed: %v", slug, missing)
		}
	}
}

// flakyAgentPod fails each slug's Provision until it has been asked
// failures[slug] times, counting every attempt.
type flakyAgentPod struct {
	mu       sync.Mutex
	failures map[string]int
	attempts map[string]int
}

func (f *flakyAgentPod) Provision(_ context.Context, slug string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.attempts[slug]++
	if f.attempts[slug] <= f.failures[slug] {
		return errors.New("context deadline exceeded")
	}
	return nil
}

func (f *flakyAgentPod) Destroy(context.Context, string) error { return nil }

func (f *flakyAgentPod) attemptsOf(slug string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.attempts[slug]
}

// An agent whose Provision failed is provisioned again after a backoff
// that doubles each time, while the agents that succeeded are left
// alone; the sweep returns once every pod exists.
func TestBulkProvisionRetriesFailedPods(t *testing.T) {
	srv := newTestServer(t)
	clk := clock.NewFake()
	srv.Clock = clk
	for _, slug := range []string{"cos", "alice"} {
		if err := srv.Store.CreateAgent(store.Agent{Slug: slug, Role: slug, ReportsTo: "ceo"}, "role"); err != nil {
			t.Fatal(err)
		}
	}
	pod := &flakyAgentPod{failures: map[string]int{"alice": 2}, attempts: map[string]int{}}
	srv.AgentPod = pod

	done := make(chan struct{})
	go func() {
		defer close(done)
		srv.BulkProvisionAgentPods(context.Background())
	}()

	// The first wave failed alice; the retry waits bulkProvisionBackoff.
	clk.BlockUntil(1)
	if got := pod.attemptsOf("alice"); got != 1 {
		t.Fatalf("alice attempts after the first wave: %d, want 1", got)
	}
	clk.Advance(bulkProvisionBackoff - time.Second)
	if got := pod.attemptsOf("alice"); got != 1 {
		t.Fatalf("alice retried before the backoff ran out: %d attempts", got)
	}
	clk.Advance(time.Second)

	// The second attempt fails too; the next wait is twice as long.
	clk.BlockUntil(1)
	if got := pod.attemptsOf("alice"); got != 2 {
		t.Fatalf("alice attempts after the first retry: %d, want 2", got)
	}
	clk.Advance(2*bulkProvisionBackoff - time.Second)
	if got := pod.attemptsOf("alice"); got != 2 {
		t.Fatalf("alice retried before the doubled backoff ran out: %d attempts", got)
	}
	clk.Advance(time.Second)

	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("BulkProvisionAgentPods did not return once every pod was provisioned")
	}
	if got := pod.attemptsOf("alice"); got != 3 {
		t.Errorf("alice attempts: %d, want 3", got)
	}
	if got := pod.attemptsOf("cos"); got != 1 {
		t.Errorf("cos was provisioned %d times, want once: a retry must only touch the agents that failed", got)
	}
}

// The retries stop with the context: a sweep whose deadline passes
// while it waits to try again returns instead of provisioning on.
func TestBulkProvisionStopsRetryingWhenContextEnds(t *testing.T) {
	srv := newTestServer(t)
	clk := clock.NewFake()
	srv.Clock = clk
	if err := srv.Store.CreateAgent(store.Agent{Slug: "alice", Role: "alice", ReportsTo: "ceo"}, "role"); err != nil {
		t.Fatal(err)
	}
	pod := &flakyAgentPod{failures: map[string]int{"alice": 1 << 20}, attempts: map[string]int{}}
	srv.AgentPod = pod

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		srv.BulkProvisionAgentPods(ctx)
	}()
	clk.BlockUntil(1)
	cancel()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("BulkProvisionAgentPods did not return when its context ended")
	}
	if got := pod.attemptsOf("alice"); got != 1 {
		t.Errorf("alice attempts: %d, want 1", got)
	}
}
