package web

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kivali-ai/kivali/internal/agentpod"
	"github.com/kivali-ai/kivali/internal/browserpod"
	"github.com/kivali-ai/kivali/internal/structured"
)

// TestWebUploadFilesAreCheckedBeforeTheyAreRead: a file over the upload
// limit is refused at dispatch from its size alone, and nothing of an
// upload task's files is copied anywhere: only their digests are kept.
func TestWebUploadFilesAreCheckedBeforeTheyAreRead(t *testing.T) {
	svc, parentRoot, _, _ := webService(t, &fakeSubagentDriver{})
	endTasksAtCleanup(t, svc)
	big := filepath.Join(parentRoot, "artifacts", "private", "big.bin")
	f, err := os.Create(big)
	if err != nil {
		t.Fatal(err)
	}
	// Sparse: its size says 51 MB, and reading it would take a while.
	if err := f.Truncate(maxWebUploadBytes + 1<<20); err != nil {
		t.Fatal(err)
	}
	_ = f.Close()
	if _, err := svc.StartBatch(context.Background(), "alice", webArgs(t, map[string]any{"start_url": "https://vendor.com/", "files": []string{"/files/artifacts/private/big.bin"}})); err == nil || !strings.Contains(err.Error(), "is 51 MB; a task may upload files up to 50 MB") {
		t.Errorf("a file over the limit: %v", err)
	}
	if err := os.WriteFile(filepath.Join(parentRoot, "artifacts", "private", "quote.pdf"), []byte("pdf"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.StartBatch(context.Background(), "alice", webArgs(t, map[string]any{"start_url": "https://vendor.com/", "files": []string{"/files/artifacts/private/quote.pdf"}})); err != nil {
		t.Fatal(err)
	}
	if entries, _ := os.ReadDir(filepath.Join(svc.Store.Root(), "attachments")); len(entries) != 0 {
		t.Errorf("dispatch kept %d attachment(s)", len(entries))
	}
	if j := onlyJob(t, svc); len(j.ApprovedFiles) != 1 || j.ApprovedFiles[0].Size != 3 || len(j.ApprovedFiles[0].SHA) != 64 {
		t.Errorf("approved files = %+v", j.ApprovedFiles)
	}
}

// TestWebUploadsAwaitingApprovalAreCapped: at most five of an agent's
// upload tasks wait for a person at once; past that a dispatch is
// refused with what to do, and an answer makes room again.
func TestWebUploadsAwaitingApprovalAreCapped(t *testing.T) {
	srv, svc, _, _ := webNeedsServer(t, &fakeSubagentDriver{}, nil)
	six := make([]map[string]any, 6)
	for i := range six {
		six[i] = map[string]any{"description": "upload", "prompt": "p", "kind": "web", "schema": json.RawMessage(quoteSchema),
			"start_url": "https://vendor.com/", "files": []string{"/files/artifacts/private/quote.pdf"}}
	}
	batch := func(tasks []map[string]any) error {
		args, _ := json.Marshal(map[string]any{"tasks": tasks})
		_, err := svc.StartBatch(context.Background(), "alice", args)
		return err
	}
	if err := batch(six[:3]); err != nil {
		t.Fatal(err)
	}
	if err := batch(six[:3]); err == nil || !strings.Contains(err.Error(), "you have 3 web tasks waiting for a person to approve their uploads, and at most 5 may wait at once") {
		t.Errorf("three more: %v", err)
	}
	if err := batch(six[:2]); err != nil {
		t.Fatal(err)
	}
	if err := batch(six[:1]); err == nil || !strings.Contains(err.Error(), "you have 5 web tasks waiting for a person to approve their uploads") {
		t.Errorf("a sixth: %v", err)
	}
	// A task without files is not held back.
	if _, err := svc.StartBatch(context.Background(), "alice", webArgs(t, nil)); err != nil {
		t.Errorf("a task without files: %v", err)
	}
	j := svc.awaitingApprovalJobs()[0]
	if code, _, body := answerNeed(t, srv, "deny", webUploadNeedPrefix+"alice/"+j.ID, "no"); code != 200 {
		t.Fatalf("deny: %d %s", code, body)
	}
	<-j.done
	if err := batch(six[:1]); err != nil {
		t.Errorf("after an answer: %v", err)
	}
}

// TestWebURLFreePool: the free allowances of a task's URLs come out of
// a 1 KB pool for the whole task; once it is spent, every byte of a
// URL's host, path and query counts.
func TestWebURLFreePool(t *testing.T) {
	svc, _, _, _ := webService(t, &fakeSubagentDriver{})
	schema, _ := structured.Parse([]byte(quoteSchema))
	svc.registerJob(&subagentJob{ID: "p1", Parent: "alice", Kind: agentpod.SubagentKindWeb, Session: agentpod.OpenWebSession, State: subagentJobRunning, Schema: schema, Depth: 1, done: make(chan struct{})})
	j, _ := svc.lookupJob("p1")
	// 9 bytes of host and 61 of path: free while the pool lasts.
	u := "https://x.example/" + strings.Repeat("a", 60)
	cost, waived := webURLCharge(u)
	if cost != 0 || waived != 70 {
		t.Fatalf("webURLCharge = %d, %d", cost, waived)
	}
	nav := func() {
		t.Helper()
		args, _ := json.Marshal(browserpod.NavigateArgs{URL: u})
		if r := svc.chargeWebAction(j, browserpod.CommandNavigate, args, func() string { return "x.example" }); r != "" {
			t.Fatalf("refused: %s", r)
		}
	}
	for i := 0; i < webURLFreePool/70; i++ { // 14 navigations, 980 bytes of the pool
		nav()
	}
	if got := spentToday(svc); got != 0 {
		t.Fatalf("spent while the pool lasted = %d", got)
	}
	nav() // 44 bytes left in the pool; 26 count
	if got := spentToday(svc); got != 70-(webURLFreePool-14*70) {
		t.Errorf("spent as the pool ran out = %d", got)
	}
	nav() // the pool is spent: all 70 count
	if got := spentToday(svc); got != 70-(webURLFreePool-14*70)+70 {
		t.Errorf("spent after the pool = %d", got)
	}
	// Another task has a pool of its own.
	svc.registerJob(&subagentJob{ID: "p2", Parent: "alice", Kind: agentpod.SubagentKindWeb, Session: agentpod.OpenWebSession, State: subagentJobRunning, Schema: schema, Depth: 1, done: make(chan struct{})})
	j2, _ := svc.lookupJob("p2")
	args, _ := json.Marshal(browserpod.NavigateArgs{URL: u})
	before := spentToday(svc)
	if r := svc.chargeWebAction(j2, browserpod.CommandNavigate, args, func() string { return "" }); r != "" || spentToday(svc) != before {
		t.Errorf("another task's first URL: refusal %q, spent %d → %d", r, before, spentToday(svc))
	}
}
