package web

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kivali-ai/kivali/internal/files"
)

// TestNestedWebTaskCannotNameFiles: a sub-lead's web task names no
// files, its own or the agent's: an upload waits for a person's
// approval, which a sub-lead's dispatch cannot wait for. A file is
// still looked for in the caller's own view, so the agent's dispatch
// does not see a file of the sub-lead's.
func TestNestedWebTaskCannotNameFiles(t *testing.T) {
	svc, parentRoot, _, _ := webService(t, &fakeSubagentDriver{respond: func(fakeDriveCall) (string, error) { return "", nil }})
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
		{"the sub-lead's own file", "/files/artifacts/private/brief.pdf"},
		{"the agent's private file", "/files/artifacts/private/secret.pdf"},
		{"the agent's past chats", "/files/past-chats/2026-01.md"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := svc.RunNestedBatch(ctx, "alice", "lead1", webArgs(t, map[string]any{"files": []string{tc.file}, "start_url": "https://vendor.com/"}))
			if err == nil || !strings.Contains(err.Error(), "dispatched by a subagent cannot upload files") {
				t.Errorf("err = %v, want the upload refused", err)
			}
		})
	}
	// The agent's own dispatch does not see the sub-lead's file.
	if _, err := svc.StartBatch(ctx, "alice", webArgs(t, map[string]any{"files": []string{"/files/artifacts/private/brief.pdf"}, "start_url": "https://vendor.com/"})); err == nil || !strings.Contains(err.Error(), "is not a file under your /files/") {
		t.Errorf("the agent naming the sub-lead's file: %v", err)
	}
	if len(svc.listJobs("alice")) != 1 {
		t.Fatalf("a refused dispatch registered a job: %+v", svc.listJobs("alice"))
	}
}
