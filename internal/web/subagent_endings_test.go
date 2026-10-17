package web

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kivali-ai/kivali/internal/clock"
	"github.com/kivali-ai/kivali/internal/structured"
)

// awaitCancel blocks until ctx is cancelled; the deadline is a deadlock
// diagnostic.
func awaitCancel(t *testing.T, what string, ctx context.Context) {
	t.Helper()
	select {
	case <-ctx.Done():
	case <-time.After(10 * time.Second):
		t.Fatalf("%s was not cancelled", what)
	}
}

// TestSubmitResultIsRefusedOnceTheEndingIsDecided: a submit_result
// that lands after structuredEnding has read the job's answer is
// refused, not recorded on a job whose body is already on its way.
func TestSubmitResultIsRefusedOnceTheEndingIsDecided(t *testing.T) {
	svc, _, _, _ := newSubagentTestService(t, &fakeSubagentDriver{})
	schema, _ := structured.Parse([]byte(quoteSchema))
	job := &subagentJob{ID: "j1", Parent: "alice", Depth: 1, State: subagentJobRunning, Schema: schema, done: make(chan struct{})}
	svc.registerJob(job)
	body, err := svc.structuredEnding(job, nil)
	if err == nil || !strings.Contains(body, "without calling submit_result") {
		t.Fatalf("ending = %q, %v", body, err)
	}
	text, isErr := svc.SubmitResult("alice", "j1", json.RawMessage(`{"outcome":"found","result":{"vendor":"Acme","price":1}}`))
	if !isErr || !strings.Contains(text, "no longer running") {
		t.Errorf("a late result: %q (error %v)", text, isErr)
	}
	if j, _ := svc.lookupJob("j1"); j.Result != "" {
		t.Error("a late result was recorded")
	}
}

// TestEndingASubLeadCancelsItsWorkers: the run core ends after an
// accepted result, or after the attempt cap, takes the sub-lead's
// workers with it: they are cancelled, the sub-lead is not, since its
// outcome is decided and delivered.
func TestEndingASubLeadCancelsItsWorkers(t *testing.T) {
	for _, way := range []string{"accepted", "refused"} {
		t.Run(way, func(t *testing.T) {
			svc, _, _, _ := newSubagentTestService(t, &fakeSubagentDriver{})
			clk := clock.NewFake()
			svc.Clock = clk
			schema, _ := structured.Parse([]byte(quoteSchema))
			leadCtx, leadCancel := context.WithCancel(context.Background())
			workerCtx, workerCancel := context.WithCancel(context.Background())
			otherCtx, otherCancel := context.WithCancel(context.Background())
			defer otherCancel()
			svc.registerJob(&subagentJob{ID: "lead", Parent: "alice", Depth: 1, State: subagentJobRunning, Schema: schema, cancel: leadCancel, done: make(chan struct{})})
			svc.registerJob(&subagentJob{ID: "w1", Parent: "alice", Depth: 2, CallerID: "lead", State: subagentJobRunning, cancel: workerCancel, done: make(chan struct{})})
			// Another tier-1 task of alice's is not the lead's.
			svc.registerJob(&subagentJob{ID: "other", Parent: "alice", Depth: 1, State: subagentJobRunning, cancel: otherCancel, done: make(chan struct{})})
			switch way {
			case "accepted":
				if text, isErr := svc.SubmitResult("alice", "lead", json.RawMessage(`{"outcome":"found","result":{"vendor":"Acme","price":1}}`)); isErr {
					t.Fatalf("valid result refused: %s", text)
				}
				clk.BlockUntil(1)
				clk.Advance(subagentAcceptGrace)
			case "refused":
				for i := 0; i < subagentResultAttempts; i++ {
					svc.SubmitResult("alice", "lead", json.RawMessage(`{"outcome":"found","result":{"vendor":"Acme"}}`))
				}
			}
			awaitCancel(t, "the sub-lead's run", leadCtx)
			awaitCancel(t, "the worker", workerCtx)
			lead, _ := svc.lookupJob("lead")
			worker, _ := svc.lookupJob("w1")
			if lead.cancelled || !worker.cancelled {
				t.Errorf("cancelled: lead=%v worker=%v, want the worker alone", lead.cancelled, worker.cancelled)
			}
			if other, _ := svc.lookupJob("other"); other.cancelled || otherCtx.Err() != nil {
				t.Error("an unrelated task was cancelled")
			}
		})
	}
}

// TestRegularFileUnder: a file is the task's when it is a regular file
// reached through no link but core's own; a planted link to a
// directory elsewhere, a link at the name, and a path that climbs out
// are not.
func TestRegularFileUnder(t *testing.T) {
	base := filepath.Join(t.TempDir(), "private")
	elsewhere := filepath.Join(t.TempDir(), "elsewhere")
	mustWrite := func(path string) {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	mustWrite(filepath.Join(base, "real.txt"))
	mustWrite(filepath.Join(base, "sub", "deep.txt"))
	mustWrite(filepath.Join(elsewhere, "secret.txt"))
	if err := os.Symlink(elsewhere, filepath.Join(base, "vault")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(base, "real.txt"), filepath.Join(base, "alias.txt")); err != nil {
		t.Fatal(err)
	}
	// base itself reached through a link, as background/ is: fine.
	linkedBase := filepath.Join(t.TempDir(), "background")
	if err := os.Symlink(base, linkedBase); err != nil {
		t.Fatal(err)
	}
	for rel, want := range map[string]bool{
		"real.txt": true, "sub/deep.txt": true, "vault/secret.txt": false, "alias.txt": false,
		"../../" + filepath.Base(filepath.Dir(elsewhere)) + "/elsewhere/secret.txt": false,
		"sub": false, ".": false, "missing.txt": false, elsewhere + "/secret.txt": false,
	} {
		if got := regularFileUnder(base, filepath.FromSlash(rel)); got != want {
			t.Errorf("regularFileUnder(base, %q) = %v, want %v", rel, got, want)
		}
	}
	if !regularFileUnder(linkedBase, filepath.FromSlash("sub/deep.txt")) || regularFileUnder(linkedBase, filepath.FromSlash("vault/secret.txt")) {
		t.Error("a base reached through core's link is judged differently")
	}
}
