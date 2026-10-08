package web

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"testing"

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
