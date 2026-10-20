package web

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/kivali-ai/kivali/internal/agentpod"
	"github.com/kivali-ai/kivali/internal/browserpod"
	"github.com/kivali-ai/kivali/internal/files"
	"github.com/kivali-ai/kivali/internal/structured"
)

// webArgs is a dispatch of one web task with the quote schema.
func webArgs(t *testing.T, extra map[string]any) json.RawMessage {
	t.Helper()
	task := map[string]any{"description": "price the renewal", "prompt": "find the price", "kind": "web", "schema": json.RawMessage(quoteSchema)}
	for k, v := range extra {
		task[k] = v
	}
	args, err := json.Marshal(map[string]any{"tasks": []map[string]any{task}})
	if err != nil {
		t.Fatal(err)
	}
	return args
}

// webService is a subagent service with a browser registry whose
// provisioner is enabled, so web tasks pass validation.
func webService(t *testing.T, driver SubagentDriver) (*SubagentService, string, *deliveryRecorder, *fakeBrowserProv) {
	t.Helper()
	svc, st, parentRoot, _, del := newSubagentTestServiceWithDeliveries(t, driver)
	prov := newFakeBrowserProv()
	svc.Browsers = newBrowserRegistry(st, prov, svc.clk(), 3, t.Logf, func(slug, id string) string { return subagentFilesRoot(st, slug, id) })
	svc.Browsers.markBooted()
	return svc, parentRoot, del, prov
}

// TestWebTaskDispatchBuildsTheWebSpec: a web task's spec carries the
// kind, the session, the start URL, the file names, the web toolkit
// and a prompt about untrusted pages; its delivered body carries core's
// facts about the run.
func TestWebTaskDispatchBuildsTheWebSpec(t *testing.T) {
	var svc *SubagentService
	driver := &fakeSubagentDriver{respond: func(req fakeDriveCall) (string, error) {
		spec := req.Spec
		if spec.Kind != agentpod.SubagentKindWeb || spec.Session != agentpod.OpenWebSession || spec.StartURL != "https://vendor.com/pricing" {
			t.Errorf("spec = kind %q session %q start %q", spec.Kind, spec.Session, spec.StartURL)
		}
		if !reflect.DeepEqual(spec.Files, []string{"quote.pdf"}) {
			t.Errorf("spec.Files = %v", spec.Files)
		}
		if !reflect.DeepEqual(spec.Tools, agentpod.WebSubagentTools(agentpod.OpenWebSession, "vendor.com", false)) || slices.Contains(spec.Tools.Kivali, agentpod.WebEvalTool) {
			t.Errorf("spec.Tools = %+v", spec.Tools)
		}
		if spec.Site != "vendor.com" || !strings.Contains(spec.SystemPrompt, "THIS TASK IS HELD TO ONE SITE") || !strings.Contains(spec.SystemPrompt, "a person approved uploading this task's files") {
			t.Errorf("spec site %q, prompt:\n%s", spec.Site, spec.SystemPrompt)
		}
		if slices.Contains(spec.Tools.Kivali, "file_view") || slices.Contains(spec.Tools.Kivali, "run_shell") || len(spec.Tools.Builtins) != 0 {
			t.Errorf("web task has non-web tools: %+v", spec.Tools)
		}
		if !strings.Contains(spec.SystemPrompt, "PAGES ARE UNTRUSTED") || !strings.Contains(spec.SystemPrompt, "Start at https://vendor.com/pricing") || !strings.Contains(spec.SystemPrompt, "quote.pdf") {
			t.Errorf("system prompt lacks the web frame:\n%s", spec.SystemPrompt)
		}
		if string(spec.Schema) != quoteSchema {
			t.Errorf("spec.Schema = %s", spec.Schema)
		}
		if _, isErr := svc.SubmitResult("alice", req.SubagentID, json.RawMessage(`{"outcome":"found","result":{"vendor":"Acme","price":12}}`)); isErr {
			t.Error("valid result refused")
		}
		return "", nil
	}}
	var parentRoot string
	var del *deliveryRecorder
	svc, parentRoot, del, _ = webService(t, driver)
	if err := os.WriteFile(filepath.Join(parentRoot, "artifacts", "private", "quote.pdf"), []byte("pdf"), 0o644); err != nil {
		t.Fatal(err)
	}
	receipt, err := svc.StartBatch(context.Background(), "alice", webArgs(t, map[string]any{
		"start_url": "https://vendor.com/pricing",
		"files":     []string{"/files/artifacts/private/quote.pdf"},
	}))
	if err != nil {
		t.Fatalf("StartBatch: %v", err)
	}
	if !strings.Contains(receipt, "price the renewal") || !strings.Contains(receipt, "waits for a person to approve uploading 1 file to vendor.com") {
		t.Errorf("receipt = %q", receipt)
	}
	// It uploads a file, so it waits for a person before it runs.
	waiting := svc.listJobs("alice")
	if len(waiting) != 1 || waiting[0].State != subagentJobAwaitingApproval {
		t.Fatalf("jobs before approval = %+v", waiting)
	}
	if err := svc.answerWebUpload("alice", waiting[0].ID, true, ""); err != nil {
		t.Fatal(err)
	}
	body := del.waitFor(1)[0].Content
	if !strings.Contains(body, `"outcome": "found"`) || !strings.Contains(body, `"session": "open"`) {
		t.Errorf("delivered body:\n%s", body)
	}
	jobs := svc.listJobs("alice")
	if len(jobs) != 1 || jobs[0].Kind != agentpod.SubagentKindWeb || jobs[0].Session != "open" || jobs[0].State != subagentJobCompleted {
		t.Errorf("job = %+v", jobs)
	}
	meta, err := readSubagentMeta(subagentMetaPath(svc.Store.Root(), "alice", jobs[0].ID))
	if err != nil || meta.Kind != agentpod.SubagentKindWeb || meta.Session != "open" {
		t.Errorf("meta = %+v, %v", meta, err)
	}
}

// TestWebTaskDispatchIsRefusedWhenItCannotRun: each web-only rule is
// checked at dispatch with an error naming the task and the fix, and a
// general task may not carry web fields.
func TestWebTaskDispatchIsRefusedWhenItCannotRun(t *testing.T) {
	svc, parentRoot, _, prov := webService(t, &fakeSubagentDriver{})
	// A link under /files/ to a file outside the parent's tree is no
	// file of the parent's, whatever its name says.
	outside := filepath.Join(t.TempDir(), "credentials.json")
	if err := os.WriteFile(outside, []byte("secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(parentRoot, "artifacts", "private", "leak.pdf")); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name string
		args json.RawMessage
		want string
	}{
		{"no schema", mustArgs(t, map[string]any{"description": "d", "prompt": "p", "kind": "web"}), "a web task needs a schema"},
		{"bad site", webArgs(t, map[string]any{"session": "co.uk"}), "is not a site"},
		{"bad start url", webArgs(t, map[string]any{"start_url": "ftp://x"}), "http or https URL"},
		{"missing file", webArgs(t, map[string]any{"files": []string{"/files/artifacts/private/ghost.pdf"}}), "is not a file under your /files/"},
		{"link out of the tree", webArgs(t, map[string]any{"files": []string{"/files/artifacts/private/leak.pdf"}}), "is not a file under your /files/"},
		{"relative file", webArgs(t, map[string]any{"files": []string{"artifacts/private/x.pdf"}}), "absolute, clean path"},
		{"bad kind", mustArgs(t, map[string]any{"description": "d", "prompt": "p", "kind": "browser"}), `kind "browser"`},
		{"general with session", mustArgs(t, map[string]any{"description": "d", "prompt": "p", "session": "open"}), "set kind to \"web\""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := svc.StartBatch(context.Background(), "alice", tc.args)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("err = %v, want %q", err, tc.want)
			}
		})
	}
	if len(svc.listJobs("alice")) != 0 {
		t.Error("a refused dispatch registered a job")
	}
	// Without browsers, no web task at all.
	prov.enabled = false
	if _, err := svc.StartBatch(context.Background(), "alice", webArgs(t, nil)); err == nil || !strings.Contains(err.Error(), "not available in this deployment") {
		t.Errorf("without browsers: %v", err)
	}
}

func mustArgs(t *testing.T, task map[string]any) json.RawMessage {
	t.Helper()
	args, err := json.Marshal(map[string]any{"tasks": []map[string]any{task}})
	if err != nil {
		t.Fatal(err)
	}
	return args
}

// TestWebCommandChecksBeforeRelaying: core refuses what the task's
// session or state does not allow, with text the model can act on, and
// relays the rest under the task's group, handing an upload's bytes to
// the pod first.
func TestWebCommandChecksBeforeRelaying(t *testing.T) {
	svc, parentRoot, _, prov := webService(t, &fakeSubagentDriver{})
	if err := os.WriteFile(filepath.Join(parentRoot, "artifacts", "private", "quote.pdf"), []byte("pdf"), 0o644); err != nil {
		t.Fatal(err)
	}
	// A declared file that turned into a link out of the tree after
	// dispatch is read the way file_view would read it: not at all.
	outside := filepath.Join(t.TempDir(), "credentials.json")
	if err := os.WriteFile(outside, []byte("secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	leak := filepath.Join(parentRoot, "artifacts", "private", "leak.pdf")
	if err := os.WriteFile(leak, []byte("leak"), 0o644); err != nil {
		t.Fatal(err)
	}
	taskFiles := []string{"/files/artifacts/private/quote.pdf", "/files/artifacts/private/leak.pdf"}
	snaps, err := svc.digestWebFiles("alice", taskFiles)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(leak); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, leak); err != nil {
		t.Fatal(err)
	}
	schema, _ := structured.Parse([]byte(quoteSchema))
	if err := files.BuildSubagentOverlay(files.SubagentOverlay{Root: subagentFilesRoot(svc.Store, "alice", "w1")}); err != nil {
		t.Fatal(err)
	}
	svc.registerJob(&subagentJob{ID: "w1", Parent: "alice", Kind: agentpod.SubagentKindWeb, Session: agentpod.OpenWebSession, LockSite: "vendor.com", State: subagentJobRunning, Schema: schema, Files: taskFiles, ApprovedFiles: snaps, done: make(chan struct{})})
	svc.registerJob(&subagentJob{ID: "g1", Parent: "alice", Kind: agentpod.SubagentKindGeneral, State: subagentJobRunning, done: make(chan struct{})})
	svc.registerJob(&subagentJob{ID: "s1", Parent: "alice", Kind: agentpod.SubagentKindWeb, Session: "vendor.com", State: subagentJobRunning, Schema: schema, done: make(chan struct{})})
	ctx := context.Background()
	refusals := []struct {
		name, id string
		req      agentpod.WebCommandRequest
		want     string
	}{
		{"unknown task", "nope", agentpod.WebCommandRequest{Command: "snapshot"}, "no running web task"},
		{"general task", "g1", agentpod.WebCommandRequest{Command: "snapshot"}, "not a web task"},
		{"unknown command", "w1", agentpod.WebCommandRequest{Command: "hack"}, "unknown web command"},
		{"bad navigate", "w1", agentpod.WebCommandRequest{Command: "navigate", Args: json.RawMessage(`{"url":"file:///etc/passwd"}`)}, "http or https"},
		{"eval off open", "s1", agentpod.WebCommandRequest{Command: "eval", Args: json.RawMessage(`{"script":"1"}`)}, "not available in this session"},
		{"upload unknown file", "w1", agentpod.WebCommandRequest{Command: "upload", Args: json.RawMessage(`{"ref":"e1","file":"secret.pdf"}`)}, "not one of this task's files"},
		{"upload a link out of the tree", "w1", agentpod.WebCommandRequest{Command: "upload", Args: json.RawMessage(`{"ref":"e1","file":"leak.pdf"}`)}, "cannot be read"},
	}
	for _, tc := range refusals {
		t.Run(tc.name, func(t *testing.T) {
			resp := svc.WebCommand(ctx, "alice", tc.id, tc.req)
			if !resp.IsError || !strings.Contains(resp.Text, tc.want) {
				t.Errorf("response = %+v, want a refusal naming %q", resp, tc.want)
			}
		})
	}
	if len(prov.started) != 0 {
		t.Fatal("a refused command started a browser")
	}

	// A relayed upload, on a page of the task's site: the file goes
	// first, then the command; the result's image comes back as an
	// image block.
	got := make(chan agentpod.WebCommandResponse, 1)
	go func() {
		got <- svc.WebCommand(ctx, "alice", "w1", agentpod.WebCommandRequest{Command: "navigate", Args: json.RawMessage(`{"url":"https://vendor.com/upload"}`)})
	}()
	token := <-prov.startedC
	link := newFakeLink()
	b := svc.Browsers.claim("alice", "open", token, link)
	if b == nil {
		t.Fatal("claim found no waiting browser")
	}
	if err := svc.Browsers.attach(b); err != nil {
		t.Fatal(err)
	}
	svc.Browsers.onMessage(b, browserpod.Message{Type: browserpod.TypeState, State: browserpod.StateRunning}, nil)
	nav := link.next(t, browserpod.TypeCommand)
	svc.Browsers.onMessage(b, browserpod.Message{Type: browserpod.TypeTab, Group: "w1", Tab: "t1", URL: "about:blank"}, nil)
	svc.Browsers.onMessage(b, browserpod.Message{Type: browserpod.TypeNavigated, Group: "w1", Tab: "t1", URL: "https://vendor.com/upload"}, nil)
	svc.Browsers.onMessage(b, browserpod.Message{Type: browserpod.TypeResult, ID: nav.msg.ID, OK: true, Text: "Loaded"}, nil)
	if r := <-got; r.IsError {
		t.Fatalf("navigate: %+v", r)
	}
	go func() {
		got <- svc.WebCommand(ctx, "alice", "w1", agentpod.WebCommandRequest{Command: "upload", Args: json.RawMessage(`{"ref":"e3","file":"quote.pdf"}`)})
	}()
	file := link.next(t, browserpod.TypeFile)
	if file.msg.Group != "w1" || file.msg.FileName != "quote.pdf" || string(file.body) != "pdf" {
		t.Errorf("file frame = %+v %q", file.msg, file.body)
	}
	cmd := link.next(t, browserpod.TypeCommand)
	if cmd.msg.Name != browserpod.CommandUpload || cmd.msg.Group != "w1" {
		t.Errorf("command frame = %+v", cmd.msg)
	}
	svc.Browsers.onMessage(b, browserpod.Message{Type: browserpod.TypeResult, ID: cmd.msg.ID, OK: true, Text: "Attached quote.pdf", Image: &browserpod.Image{MIME: "image/png", Data: []byte{1, 2, 3}}}, nil)
	resp := <-got
	if resp.IsError || resp.Text != "Attached quote.pdf" || resp.Image == nil || resp.Image.MIME != "image/png" || len(resp.Image.Data) != 3 {
		t.Errorf("relayed response = %+v", resp)
	}
	// A pod refusal is the model's to read, as an error.
	go func() {
		got <- svc.WebCommand(ctx, "alice", "w1", agentpod.WebCommandRequest{Command: "click", Args: json.RawMessage(`{"ref":"e9"}`)})
	}()
	cmd = link.next(t, browserpod.TypeCommand)
	svc.Browsers.onMessage(b, browserpod.Message{Type: browserpod.TypeResult, ID: cmd.msg.ID, OK: false, Error: "no element e9; take a new snapshot", Code: browserpod.CodeNotFound}, nil)
	if resp := <-got; !resp.IsError || !strings.Contains(resp.Text, "take a new snapshot") {
		t.Errorf("refused response = %+v", resp)
	}
	// Downloads the browser reported join the task's result files.
	svc.Browsers.onMessage(b, browserpod.Message{Type: browserpod.TypeDownload, Group: "w1", FileName: "quote-signed.pdf", Bytes: 3}, strings.NewReader("abc"))
	j, _ := svc.lookupJob("w1")
	j.StartedAt = svc.clk().Now()
	body := svc.finishWebJob(&j, structured.Result{Outcome: structured.OutcomeNotFound}.Body())
	if !strings.Contains(body, `"/files/subagents/w1/untrusted-downloads/quote-signed.pdf"`) || !strings.Contains(body, `"session": "open"`) {
		t.Errorf("finished body:\n%s", body)
	}
	if _, err := os.Stat(filepath.Join(subagentFilesRoot(svc.Store, "alice", "w1"), files.UntrustedDownloadsDir, "quote-signed.pdf")); err != nil {
		t.Errorf("download not on disk: %v", err)
	}
}

// TestWebTaskEndsSayingTheTeamIsShortOfMemory guards what the caller
// and the person read when a web task's browser cannot be placed for
// want of memory: "the browser could not start" alone gave neither the
// cause nor where to raise the team's memory, so the task failed again
// on every retry. The end reason names both.
func TestWebTaskEndsSayingTheTeamIsShortOfMemory(t *testing.T) {
	var svc *SubagentService
	driver := &fakeSubagentDriver{respond: func(req fakeDriveCall) (string, error) {
		resp := svc.WebCommand(context.Background(), "alice", req.SubagentID, agentpod.WebCommandRequest{Command: "navigate", Args: json.RawMessage(`{"url":"https://github.com/"}`)})
		if !resp.IsError || !strings.Contains(resp.Text, "This task has ended") || !strings.Contains(resp.Text, "not have enough memory") {
			t.Errorf("tool result = %+v, want the end and the memory shortage", resp)
		}
		return "", context.Canceled
	}}
	var del *deliveryRecorder
	var prov *fakeBrowserProv
	svc, _, del, prov = webService(t, driver)
	if _, err := svc.StartBatch(context.Background(), "alice", webArgs(t, nil)); err != nil {
		t.Fatalf("StartBatch: %v", err)
	}
	<-prov.startedC
	prov.mu.Lock()
	prov.unschedulable = "0/1 nodes are available: 1 Insufficient memory."
	prov.mu.Unlock()
	svc.Browsers.mu.Lock()
	b := svc.Browsers.browsers["alice/"+agentpod.OpenWebSession]
	svc.Browsers.mu.Unlock()
	svc.Browsers.checkScheduling(b)

	body := del.waitFor(1)[0].Content
	if !strings.Contains(body, `"outcome": "failed"`) || !strings.Contains(body, "does not have enough memory") || !strings.Contains(body, "Kivali Desktop (Settings, the team's This Mac or This PC tab)") || !strings.Contains(body, "whoever runs this Kivali server") {
		t.Errorf("delivered body:\n%s", body)
	}
}

// TestWebTaskEndsFailedWhenItsBrowserCannotStart guards what a caller
// reads when a web task's browser never comes up. Left to the
// subagent, a Chromium crash at start came back as blocked on
// not_reachable, which reads as the site being down. Core ends the task
// instead: the caller reads outcome failed and core's reason, and the
// pod's detail stays in the tool result, out of the caller's context.
func TestWebTaskEndsFailedWhenItsBrowserCannotStart(t *testing.T) {
	const detail = "chromium exited at start: exit status 13: Remote debugging pipe file descriptors are not open."
	var svc *SubagentService
	driver := &fakeSubagentDriver{respond: func(req fakeDriveCall) (string, error) {
		resp := svc.WebCommand(context.Background(), "alice", req.SubagentID, agentpod.WebCommandRequest{Command: "navigate", Args: json.RawMessage(`{"url":"https://github.com/"}`)})
		if !resp.IsError || !strings.Contains(resp.Text, "This task has ended") || !strings.Contains(resp.Text, detail) {
			t.Errorf("tool result = %+v, want the end and what stopped the browser", resp)
		}
		j, _ := svc.lookupJob(req.SubagentID)
		if j.endReason != browserStartFailed {
			t.Errorf("endReason = %q", j.endReason)
		}
		// The task is over: another command and a result are refused.
		if resp := svc.WebCommand(context.Background(), "alice", req.SubagentID, agentpod.WebCommandRequest{Command: "snapshot"}); !resp.IsError || !strings.Contains(resp.Text, "no longer running") {
			t.Errorf("a command after the end = %+v", resp)
		}
		if text, isErr := svc.SubmitResult("alice", req.SubagentID, json.RawMessage(`{"outcome":"blocked","blocked_on":"not_reachable"}`)); !isErr {
			t.Errorf("a result after the end was accepted: %q", text)
		}
		// Core cancelled the run; the pod reports it as such.
		return "", context.Canceled
	}}
	var del *deliveryRecorder
	var prov *fakeBrowserProv
	svc, _, del, prov = webService(t, driver)
	if _, err := svc.StartBatch(context.Background(), "alice", webArgs(t, nil)); err != nil {
		t.Fatalf("StartBatch: %v", err)
	}
	// Play the pod: it connects, then Chromium dies before it runs.
	token := <-prov.startedC
	link := newFakeLink()
	b := svc.Browsers.claim("alice", agentpod.OpenWebSession, token, link)
	if b == nil {
		t.Fatal("claim found no waiting browser")
	}
	if err := svc.Browsers.attach(b); err != nil {
		t.Fatal(err)
	}
	svc.Browsers.onMessage(b, browserpod.Message{Type: browserpod.TypeState, State: browserpod.StateStopped, Detail: detail}, nil)

	body := del.waitFor(1)[0].Content
	if !strings.Contains(body, `"outcome": "failed"`) || !strings.Contains(body, `"error": "the browser could not start"`) {
		t.Errorf("delivered body:\n%s", body)
	}
	if strings.Contains(body, "exit status") || strings.Contains(body, "not_reachable") {
		t.Errorf("the pod's detail or the subagent's guess reached the caller:\n%s", body)
	}
	jobs := svc.listJobs("alice")
	if len(jobs) != 1 || jobs[0].State != subagentJobFailed {
		t.Errorf("job = %+v", jobs)
	}
}
