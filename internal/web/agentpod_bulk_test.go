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
	awaitRetryWait(t, clk, bulkProvisionBackoff)
	if got := pod.attemptsOf("alice"); got != 1 {
		t.Fatalf("alice attempts after the first wave: %d, want 1", got)
	}
	clk.Advance(bulkProvisionBackoff)

	// The second attempt fails too; the next wait is twice as long.
	awaitRetryWait(t, clk, 2*bulkProvisionBackoff)
	if got := pod.attemptsOf("alice"); got != 2 {
		t.Fatalf("alice attempts after the first retry: %d, want 2", got)
	}
	clk.Advance(2 * bulkProvisionBackoff)

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

// awaitRetryWait waits for the sweep to arm its retry timer and fails
// the test unless that timer is due exactly want from now. The timer is
// the only thing that starts the next wave, so a wait of the right
// length is what rules out a retry before the backoff runs out.
func awaitRetryWait(t *testing.T, clk *clock.Fake, want time.Duration) {
	t.Helper()
	clk.BlockUntil(1)
	got := clk.Deadlines()
	if len(got) != 1 {
		t.Fatalf("armed timers: %v, want one retry wait", got)
	}
	if wait := got[0].Sub(clk.Now()); wait != want {
		t.Fatalf("retry wait: %s, want %s", wait, want)
	}
}

// The doubling backoff stops growing at bulkProvisionBackoffMax.
func TestBulkProvisionBackoffIsCapped(t *testing.T) {
	srv := newTestServer(t)
	clk := clock.NewFake()
	srv.Clock = clk
	if err := srv.Store.CreateAgent(store.Agent{Slug: "alice", Role: "alice", ReportsTo: "ceo"}, "role"); err != nil {
		t.Fatal(err)
	}
	waits := []time.Duration{
		bulkProvisionBackoff,
		2 * bulkProvisionBackoff,
		4 * bulkProvisionBackoff,
		8 * bulkProvisionBackoff,
		bulkProvisionBackoffMax,
		bulkProvisionBackoffMax,
	}
	pod := &flakyAgentPod{failures: map[string]int{"alice": len(waits)}, attempts: map[string]int{}}
	srv.AgentPod = pod

	done := make(chan struct{})
	go func() {
		defer close(done)
		srv.BulkProvisionAgentPods(context.Background())
	}()

	for i, want := range waits {
		awaitRetryWait(t, clk, want)
		if got := pod.attemptsOf("alice"); got != i+1 {
			t.Fatalf("alice attempts before wait %d: %d, want %d", i+1, got, i+1)
		}
		clk.Advance(want)
	}
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("BulkProvisionAgentPods did not return once every pod was provisioned")
	}
	if got := pod.attemptsOf("alice"); got != len(waits)+1 {
		t.Errorf("alice attempts: %d, want %d", got, len(waits)+1)
	}
}

// An agent offboarded while the sweep waits to retry its pod is not
// provisioned again, and its directory is not recreated; the agents
// still active are retried as before.
func TestBulkProvisionSkipsAgentsOffboardedDuringBackoff(t *testing.T) {
	srv := newTestServer(t)
	clk := clock.NewFake()
	srv.Clock = clk
	for _, slug := range []string{"alice", "bob"} {
		if err := srv.Store.CreateAgent(store.Agent{Slug: slug, Role: slug, ReportsTo: "ceo"}, "role"); err != nil {
			t.Fatal(err)
		}
	}
	pod := &flakyAgentPod{failures: map[string]int{"alice": 1, "bob": 1}, attempts: map[string]int{}}
	srv.AgentPod = pod

	done := make(chan struct{})
	go func() {
		defer close(done)
		srv.BulkProvisionAgentPods(context.Background())
	}()

	awaitRetryWait(t, clk, bulkProvisionBackoff)
	if err := srv.Store.ArchiveAgent("alice"); err != nil {
		t.Fatal(err)
	}
	clk.Advance(bulkProvisionBackoff)

	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("BulkProvisionAgentPods did not return once every active agent's pod was provisioned")
	}
	if got := pod.attemptsOf("alice"); got != 1 {
		t.Errorf("alice provisioned %d times, want once: she was offboarded before the retry", got)
	}
	if got := pod.attemptsOf("bob"); got != 2 {
		t.Errorf("bob attempts: %d, want 2", got)
	}
	if _, err := os.Stat(filepath.Join(srv.Store.Root(), "agents", "alice")); !os.IsNotExist(err) {
		t.Errorf("agents/alice exists after the sweep (stat err %v); the offboarded agent's tree was recreated", err)
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

// notifyingAgentPod is an AgentPod that reports, like
// *agentpod.Provisioner, whether it is about to create the pod.
type notifyingAgentPod struct{ recreate bool }

func (f *notifyingAgentPod) Provision(context.Context, string) error { return nil }
func (f *notifyingAgentPod) Destroy(context.Context, string) error   { return nil }
func (f *notifyingAgentPod) ProvisionNotify(_ context.Context, _ string, starting func()) error {
	if f.recreate {
		starting()
	}
	return nil
}

// Reconciling an agent whose pod keeps running (an org-wide software
// change re-provisions everyone) does not mark it starting; one whose
// pod is recreated is marked. An AgentPod that cannot say marks it.
func TestProvisionAgentPodMarksStartingOnlyOnRecreate(t *testing.T) {
	cases := []struct {
		name string
		pod  AgentPodLifecycle
		want bool
	}{
		{"spec unchanged", &notifyingAgentPod{recreate: false}, false},
		{"recreated", &notifyingAgentPod{recreate: true}, true},
		{"cannot say", &syncCheckingAgentPod{missing: map[string][]string{}}, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			srv := newTestServer(t)
			if err := srv.Store.CreateAgent(store.Agent{Slug: "alice", Role: "alice", ReportsTo: "ceo"}, "role"); err != nil {
				t.Fatal(err)
			}
			if p, ok := c.pod.(*syncCheckingAgentPod); ok {
				p.root = srv.Store.Root()
			}
			srv.AgentPod = c.pod
			srv.AgentpodHub = newAgentpodHub()
			if err := srv.ProvisionAgentPod(context.Background(), "alice"); err != nil {
				t.Fatal(err)
			}
			if got := srv.AgentpodHub.Starting("alice", srv.clk().Now()); got != c.want {
				t.Errorf("Starting = %v, want %v", got, c.want)
			}
		})
	}
}

// blockingAgentPod reports, on entered, the agent's software list as
// each Provision reads it, then holds the call until release[slug].
type blockingAgentPod struct {
	st      *store.FSStore
	entered chan []string
	release map[string]chan struct{}
}

func (p *blockingAgentPod) Provision(_ context.Context, slug string) error {
	eff, _ := p.st.EffectiveSoftware(slug)
	p.entered <- eff
	<-p.release[slug]
	return nil
}

func (p *blockingAgentPod) Destroy(context.Context, string) error { return nil }

// Two provisions for one agent run one after the other, and the later
// one builds the pod from the list as it stands once the first is done,
// so a change saved while the first was recreating the pod is applied.
func TestProvisionAgentPodSerializesPerSlug(t *testing.T) {
	srv := newTestServer(t)
	for _, slug := range []string{"alice", "bob"} {
		if err := srv.Store.CreateAgent(store.Agent{Slug: slug, Role: slug, ReportsTo: "ceo"}, "role"); err != nil {
			t.Fatal(err)
		}
	}
	pod := &blockingAgentPod{st: srv.Store, entered: make(chan []string), release: map[string]chan struct{}{
		"alice": make(chan struct{}), "bob": make(chan struct{}),
	}}
	srv.AgentPod = pod
	ctx := context.Background()

	first := make(chan error, 1)
	go func() { first <- srv.ProvisionAgentPod(ctx, "alice") }()
	if got := <-pod.entered; len(got) != 0 {
		t.Fatalf("first provision read %v, want an empty list", got)
	}
	// While the first runs, alice's provisions are locked out...
	mu, ok := srv.provisionLocks.Load("alice")
	if !ok {
		t.Fatal("no provision lock for alice")
	}
	if mu.(*sync.Mutex).TryLock() {
		mu.(*sync.Mutex).Unlock()
		t.Fatal("alice's provision lock is free while a provision runs")
	}
	// ...and another agent's are not.
	other := make(chan error, 1)
	go func() { other <- srv.ProvisionAgentPod(ctx, "bob") }()
	<-pod.entered
	pod.release["bob"] <- struct{}{}
	if err := <-other; err != nil {
		t.Fatal(err)
	}

	if err := srv.Store.AddAgentSoftware("alice", []string{"ffmpeg"}); err != nil {
		t.Fatal(err)
	}
	second := make(chan error, 1)
	go func() { second <- srv.ProvisionAgentPod(ctx, "alice") }()
	pod.release["alice"] <- struct{}{}
	if err := <-first; err != nil {
		t.Fatal(err)
	}
	if got := <-pod.entered; len(got) != 1 || got[0] != "ffmpeg" {
		t.Errorf("second provision read %v, want [ffmpeg]", got)
	}
	pod.release["alice"] <- struct{}{}
	if err := <-second; err != nil {
		t.Fatal(err)
	}
}

// The startup pass marks every agent starting, also one whose pod keeps
// running: the server has just started and no pod is subscribed yet.
func TestBulkProvisionMarksUnchangedPodsStarting(t *testing.T) {
	srv := newTestServer(t)
	if err := srv.Store.CreateAgent(store.Agent{Slug: "alice", Role: "alice", ReportsTo: "ceo"}, "role"); err != nil {
		t.Fatal(err)
	}
	srv.AgentPod = &notifyingAgentPod{recreate: false}
	srv.AgentpodHub = newAgentpodHub()
	srv.BulkProvisionAgentPods(context.Background())
	if !srv.AgentpodHub.Starting("alice", srv.clk().Now()) {
		t.Error("Starting = false after the startup pass, want true")
	}
}
