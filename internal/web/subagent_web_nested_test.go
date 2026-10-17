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
	"github.com/kivali-ai/kivali/internal/files"
	"github.com/kivali-ai/kivali/internal/structured"
)

// TestNestedWebTaskFilesAreTheSubLeadsOwn: a sub-lead's web task names
// files in the sub-lead's own /files/, where artifacts/private is the
// sub-lead's and the agent's memory is absent; the upload later reads
// the same view.
func TestNestedWebTaskFilesAreTheSubLeadsOwn(t *testing.T) {
	svc, parentRoot, _, prov := webService(t, &fakeSubagentDriver{respond: func(fakeDriveCall) (string, error) { return "", nil }})
	registerTestJob(svc, "lead1", "alice", 1, subagentJobRunning)
	leadRoot := subagentFilesRoot(svc.Store, "alice", "lead1")
	if err := files.BuildSubagentOverlay(files.SubagentOverlay{Root: leadRoot}); err != nil {
		t.Fatal(err)
	}
	write := func(p, body string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write(filepath.Join(leadRoot, "artifacts", "private", "brief.pdf"), "brief")
	write(filepath.Join(parentRoot, "artifacts", "private", "secret.pdf"), "the agent's")
	write(filepath.Join(parentRoot, "past-chats", "2026-01.md"), "a past chat")

	ctx := context.Background()
	for _, tc := range []struct {
		name, file string
	}{
		{"the agent's private file", "/files/artifacts/private/secret.pdf"},
		{"the agent's past chats", "/files/past-chats/2026-01.md"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := svc.RunNestedBatch(ctx, "alice", "lead1", webArgs(t, map[string]any{"files": []string{tc.file}}))
			if err == nil || !strings.Contains(err.Error(), "is not a file under your /files/") {
				t.Errorf("err = %v, want the file refused", err)
			}
		})
	}
	// The agent's own dispatch does not see the sub-lead's file.
	if _, err := svc.StartBatch(ctx, "alice", webArgs(t, map[string]any{"files": []string{"/files/artifacts/private/brief.pdf"}})); err == nil || !strings.Contains(err.Error(), "is not a file under your /files/") {
		t.Errorf("the agent naming the sub-lead's file: %v", err)
	}
	if len(svc.listJobs("alice")) != 1 {
		t.Fatalf("a refused dispatch registered a job: %+v", svc.listJobs("alice"))
	}

	out, err := svc.RunNestedBatch(ctx, "alice", "lead1", webArgs(t, map[string]any{"files": []string{"/files/artifacts/private/brief.pdf"}}))
	if err != nil {
		t.Fatalf("the sub-lead's own file: %v", err)
	}
	if !strings.Contains(out, "Your 1 worker finished") {
		t.Errorf("nested result:\n%s", out)
	}
	var worker subagentJob
	for _, j := range svc.listJobs("alice") {
		if j.CallerID == "lead1" {
			worker = j
		}
	}
	if worker.ID == "" || worker.Kind != agentpod.SubagentKindWeb || len(worker.Files) != 1 {
		t.Fatalf("worker job = %+v", worker)
	}

	// An upload by a worker of the sub-lead's reads the sub-lead's
	// file; the same path for a task of the agent's own is no file.
	schema, _ := structured.Parse([]byte(quoteSchema))
	svc.registerJob(&subagentJob{ID: "w9", Parent: "alice", Kind: agentpod.SubagentKindWeb, Session: agentpod.OpenWebSession, State: subagentJobRunning, Depth: 2, CallerID: "lead1", Schema: schema, Files: []string{"/files/artifacts/private/brief.pdf"}, done: make(chan struct{})})
	svc.registerJob(&subagentJob{ID: "a9", Parent: "alice", Kind: agentpod.SubagentKindWeb, Session: agentpod.OpenWebSession, State: subagentJobRunning, Depth: 1, Schema: schema, Files: []string{"/files/artifacts/private/brief.pdf"}, done: make(chan struct{})})
	if resp := svc.WebCommand(ctx, "alice", "a9", agentpod.WebCommandRequest{Command: "upload", Args: json.RawMessage(`{"ref":"e1","file":"brief.pdf"}`)}); !resp.IsError || !strings.Contains(resp.Text, "cannot be read") {
		t.Errorf("the agent's task uploading the sub-lead's file: %+v", resp)
	}
	got := make(chan agentpod.WebCommandResponse, 1)
	go func() {
		got <- svc.WebCommand(ctx, "alice", "w9", agentpod.WebCommandRequest{Command: "upload", Args: json.RawMessage(`{"ref":"e1","file":"brief.pdf"}`)})
	}()
	token := <-prov.startedC
	link := newFakeLink()
	b := svc.Browsers.claim("alice", agentpod.OpenWebSession, token, link)
	if b == nil {
		t.Fatal("claim found no waiting browser")
	}
	if err := svc.Browsers.attach(ctx, b); err != nil {
		t.Fatal(err)
	}
	svc.Browsers.onMessage(b, browserpod.Message{Type: browserpod.TypeState, State: browserpod.StateRunning}, nil)
	if file := link.next(t, browserpod.TypeFile); file.msg.Group != "w9" || file.msg.FileName != "brief.pdf" || string(file.body) != "brief" {
		t.Errorf("file frame = %+v %q, want the sub-lead's brief", file.msg, file.body)
	}
	cmd := link.next(t, browserpod.TypeCommand)
	svc.Browsers.onMessage(b, browserpod.Message{Type: browserpod.TypeResult, ID: cmd.msg.ID, OK: true, Text: "Attached brief.pdf"}, nil)
	if resp := <-got; resp.IsError || resp.Text != "Attached brief.pdf" {
		t.Errorf("upload response = %+v", resp)
	}
}
