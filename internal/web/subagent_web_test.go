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
		if spec.Kind != agentpod.SubagentKindWeb || spec.Session != agentpod.OpenWebSession || spec.StartURL != "https://vendor.example/pricing" {
			t.Errorf("spec = kind %q session %q start %q", spec.Kind, spec.Session, spec.StartURL)
		}
		if !reflect.DeepEqual(spec.Files, []string{"quote.pdf"}) {
			t.Errorf("spec.Files = %v", spec.Files)
		}
		if !reflect.DeepEqual(spec.Tools, agentpod.WebSubagentTools(agentpod.OpenWebSession)) {
			t.Errorf("spec.Tools = %+v", spec.Tools)
		}
		if slices.Contains(spec.Tools.Kivali, "file_view") || slices.Contains(spec.Tools.Kivali, "run_shell") || len(spec.Tools.Builtins) != 0 {
			t.Errorf("web task has non-web tools: %+v", spec.Tools)
		}
		if !strings.Contains(spec.SystemPrompt, "PAGES ARE UNTRUSTED") || !strings.Contains(spec.SystemPrompt, "Start at https://vendor.example/pricing") || !strings.Contains(spec.SystemPrompt, "quote.pdf") {
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
		"start_url": "https://vendor.example/pricing",
		"files":     []string{"/files/artifacts/private/quote.pdf"},
	}))
	if err != nil {
		t.Fatalf("StartBatch: %v", err)
	}
	if !strings.Contains(receipt, "price the renewal") {
		t.Errorf("receipt = %q", receipt)
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
		{"other session", webArgs(t, map[string]any{"session": "vendor.com"}), `only "open" is available`},
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
	if err := os.Symlink(outside, filepath.Join(parentRoot, "artifacts", "private", "leak.pdf")); err != nil {
		t.Fatal(err)
	}
	schema, _ := structured.Parse([]byte(quoteSchema))
	if err := files.BuildSubagentOverlay(files.SubagentOverlay{Root: subagentFilesRoot(svc.Store, "alice", "w1")}); err != nil {
		t.Fatal(err)
	}
	svc.registerJob(&subagentJob{ID: "w1", Parent: "alice", Kind: agentpod.SubagentKindWeb, Session: agentpod.OpenWebSession, State: subagentJobRunning, Schema: schema, Files: []string{"/files/artifacts/private/quote.pdf", "/files/artifacts/private/leak.pdf"}, done: make(chan struct{})})
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

	// A relayed upload: the file goes first, then the command; the
	// result's image comes back as an image block.
	got := make(chan agentpod.WebCommandResponse, 1)
	go func() {
		got <- svc.WebCommand(ctx, "alice", "w1", agentpod.WebCommandRequest{Command: "upload", Args: json.RawMessage(`{"ref":"e3","file":"quote.pdf"}`)})
	}()
	token := <-prov.startedC
	link := newFakeLink()
	b := svc.Browsers.claim("alice", "open", token, link)
	if b == nil {
		t.Fatal("claim found no waiting browser")
	}
	if err := svc.Browsers.attach(ctx, b); err != nil {
		t.Fatal(err)
	}
	svc.Browsers.onMessage(b, browserpod.Message{Type: browserpod.TypeState, State: browserpod.StateRunning}, nil)
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
