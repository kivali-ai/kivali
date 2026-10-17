package web

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kivali-ai/kivali/internal/agentpod"
	"github.com/kivali-ai/kivali/internal/clock"
	"github.com/kivali-ai/kivali/internal/structured"
)

// ctxDriver is a SubagentDriver whose run honours its context, the way
// the real one does: the respond function sees the cancel core sends.
type ctxDriver struct {
	respond func(ctx context.Context, call fakeDriveCall) (string, error)
}

func (d ctxDriver) DriveSubagent(ctx context.Context, parent, turnID, subagentID string, spec agentpod.SubagentSpec) (string, error) {
	return d.respond(ctx, fakeDriveCall{Parent: parent, TurnID: turnID, SubagentID: subagentID, Spec: spec})
}

// awaitJobDone blocks until the job has reached a terminal state: a
// real signal from finishJob, not a wait long enough to probably do.
func awaitJobDone(t *testing.T, svc *SubagentService, id string) subagentJob {
	t.Helper()
	svc.jobsMu.Lock()
	j, ok := svc.jobs[id]
	svc.jobsMu.Unlock()
	if !ok {
		t.Fatalf("job %s not registered", id)
	}
	<-j.done
	snap, _ := svc.lookupJob(id)
	return snap
}

// TestStopSuppressesAStructuredResult: a task called off by Stop or
// subagent_cancel delivers nothing, whether the cancel lands before or
// after an accepted result, and refuses a submission that arrives in
// the cancel window. A delivered result would wake the stopped agent.
func TestStopSuppressesAStructuredResult(t *testing.T) {
	var svc *SubagentService
	accepted := make(chan string)
	release := make(chan struct{})
	driver := ctxDriver{respond: func(ctx context.Context, call fakeDriveCall) (string, error) {
		if _, isErr := svc.SubmitResult("alice", call.SubagentID, json.RawMessage(`{"outcome":"not_found"}`)); isErr {
			t.Error("valid result refused before the cancel")
		}
		accepted <- call.SubagentID
		<-release
		<-ctx.Done()
		return "", ctx.Err()
	}}
	var del *deliveryRecorder
	svc, _, _, _, del = newSubagentTestServiceWithDeliveries(t, driver)
	if _, err := svc.StartBatch(context.Background(), "alice", structuredArgs(t, quoteSchema)); err != nil {
		t.Fatal(err)
	}
	id := <-accepted
	if !svc.CancelSubagent("alice", id) {
		t.Fatal("CancelSubagent found nothing to cancel")
	}
	// A submission in the cancel window is refused.
	if text, isErr := svc.SubmitResult("alice", id, json.RawMessage(`{"outcome":"not_found"}`)); !isErr || !strings.Contains(text, "no longer running") {
		t.Errorf("submission after the cancel: %q (error %v)", text, isErr)
	}
	close(release)
	job := awaitJobDone(t, svc, id)
	if job.State != subagentJobCancelled {
		t.Errorf("state = %s, want cancelled", job.State)
	}
	del.mu.Lock()
	delivered := len(del.msgs)
	del.mu.Unlock()
	if delivered != 0 {
		t.Errorf("%d results delivered after a Stop", delivered)
	}
}

// TestAcceptedResultEndsTheRun: once a result is accepted core ends
// the run after a short grace, so a subagent cannot keep spending or
// rewriting the files it named; the accepted result is delivered as
// completed.
func TestAcceptedResultEndsTheRun(t *testing.T) {
	var svc *SubagentService
	clk := clock.NewFake()
	driver := ctxDriver{respond: func(ctx context.Context, call fakeDriveCall) (string, error) {
		if _, isErr := svc.SubmitResult("alice", call.SubagentID, json.RawMessage(`{"outcome":"found","result":{"vendor":"Acme","price":1}}`)); isErr {
			t.Error("valid result refused")
		}
		// The run goes on until core ends it.
		<-ctx.Done()
		return "", errSubagentCancelled
	}}
	var del *deliveryRecorder
	svc, _, _, _, del = newSubagentTestServiceWithDeliveries(t, driver)
	svc.Clock = clk
	if _, err := svc.StartBatch(context.Background(), "alice", structuredArgs(t, quoteSchema)); err != nil {
		t.Fatal(err)
	}
	// Two waiters: the activity watcher's ticker and the grace timer.
	clk.BlockUntil(2)
	clk.Advance(subagentAcceptGrace)
	body := del.waitFor(1)[0].Content
	if !strings.Contains(body, ") finished.") || !strings.Contains(body, `"outcome": "found"`) {
		t.Errorf("delivered body:\n%s", body)
	}
	if jobs := svc.listJobs("alice"); len(jobs) != 1 || jobs[0].State != subagentJobCompleted {
		t.Errorf("job = %+v", jobs)
	}
}

// TestStructuredFilesAreMappedToTheCallersView: a result names files
// as the subagent sees them; core checks each is a regular file the
// task made and delivers the caller's path for it.
func TestStructuredFilesAreMappedToTheCallersView(t *testing.T) {
	var svc *SubagentService
	var parentRoot string
	driver := &fakeSubagentDriver{respond: func(req fakeDriveCall) (string, error) {
		overlay := filepath.Join(parentRoot, "subagents", req.SubagentID)
		if err := os.WriteFile(filepath.Join(overlay, "artifacts", "private", "quote.pdf"), []byte("pdf"), 0o644); err != nil {
			t.Error(err)
		}
		if err := os.WriteFile(filepath.Join(parentRoot, "background", "notes.md"), []byte("notes"), 0o644); err != nil {
			t.Error(err)
		}
		if err := os.Symlink("/etc/hostname", filepath.Join(overlay, "artifacts", "private", "link.txt")); err != nil {
			t.Error(err)
		}
		// A link to a directory elsewhere, planted in the task's own
		// tree: what it reaches is no file of the task's either.
		elsewhere := t.TempDir()
		if err := os.WriteFile(filepath.Join(elsewhere, "secret.txt"), []byte("secret"), 0o600); err != nil {
			t.Error(err)
		}
		if err := os.Symlink(elsewhere, filepath.Join(overlay, "artifacts", "private", "vault")); err != nil {
			t.Error(err)
		}
		// The task's rules, probed directly: each probe would cost the
		// task an attempt through SubmitResult.
		rules := svc.subagentFileRules("alice", req.SubagentID, agentpod.SubagentKindGeneral)
		schema, _ := structured.Parse([]byte(quoteSchema))
		cases := map[string]string{
			`["/files/artifacts/private/ghost.pdf"]`:                                  "not a file this task made",
			`["/files/artifacts/private/link.txt"]`:                                   "not a file this task made",
			`["/files/artifacts/private/vault/secret.txt"]`:                           "not a file this task made",
			`["/files/artifacts/private"]`:                                            "outside the directories",
			`["/files/subagents/` + req.SubagentID + `/artifacts/private/quote.pdf"]`: "outside the directories",
			`["/files/project/data.csv"]`:                                             "outside the directories",
		}
		for files, want := range cases {
			_, errs := structured.Validate(schema, json.RawMessage(`{"outcome":"not_found","files":`+files+`}`), rules)
			if len(errs) == 0 || !strings.Contains(strings.Join(errs, "\n"), want) {
				t.Errorf("files %s: %v, want a refusal naming %q", files, errs, want)
			}
		}
		if text, isErr := svc.SubmitResult("alice", req.SubagentID, json.RawMessage(`{"outcome":"not_found","files":["/files/artifacts/private/quote.pdf","/files/background/notes.md"]}`)); isErr {
			t.Errorf("real files refused: %s", text)
		}
		return "", nil
	}}
	var del *deliveryRecorder
	svc, _, parentRoot, _, del = newSubagentTestServiceWithDeliveries(t, driver)
	if _, err := svc.StartBatch(context.Background(), "alice", structuredArgs(t, quoteSchema)); err != nil {
		t.Fatal(err)
	}
	body := del.waitFor(1)[0].Content
	id := svc.listJobs("alice")[0].ID
	if !strings.Contains(body, `"/files/subagents/`+id+`/artifacts/private/quote.pdf"`) || !strings.Contains(body, `"/files/background/notes.md"`) {
		t.Errorf("delivered files not in the caller's view:\n%s", body)
	}
	if strings.Contains(body, `"/files/artifacts/private/quote.pdf"`) {
		t.Errorf("delivered body carries the subagent's path:\n%s", body)
	}
}
