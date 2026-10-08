package web

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kivali-ai/kivali/internal/agent"
	"github.com/kivali-ai/kivali/internal/agentpod"
	"github.com/kivali-ai/kivali/internal/messaging"
	"github.com/kivali-ai/kivali/internal/store"
)

// startPublishServer wires a Server with the share-file + two-phase
// publish endpoints on a Unix socket. Messenger is needed for the
// publish path's routing leg; tests inspect Store state to verify
// persistence either way.
func startPublishServer(t *testing.T) (sockPath string, srv *Server, cleanup func()) {
	t.Helper()
	srv = newTestServer(t)
	if err := srv.Store.CreateAgent(store.Agent{Slug: "alice", Role: "Analyst", ReportsTo: "ceo"}, "k"); err != nil {
		t.Fatalf("seed alice: %v", err)
	}
	if err := srv.Store.CreateAgent(store.Agent{Slug: "bob", Role: "Researcher", ReportsTo: "ceo"}, "k"); err != nil {
		t.Fatalf("seed bob: %v", err)
	}
	srv.Runtime = agent.NewRuntime(srv.Store, agent.RuntimeDefaults{})
	srv.Messenger = messaging.New(srv.Store, srv.Runtime)

	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/agent/{slug}/share-file/dispatch", srv.handleAgentpodShareFileDispatch)
	mux.HandleFunc("POST /v1/agent/{slug}/publish/stage", srv.handleAgentpodPublishStage)
	mux.HandleFunc("POST /v1/agent/{slug}/publish/commit", srv.handleAgentpodPublishCommit)
	dir, err := os.MkdirTemp("/tmp", "wos-")
	if err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	sockPath = filepath.Join(dir, "s")
	ln, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	httpSrv := &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	go func() {
		if err := httpSrv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			t.Logf("serve: %v", err)
		}
	}()
	cleanup = func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = httpSrv.Shutdown(ctx)
	}
	return sockPath, srv, cleanup
}

// TestAgentpodShareFileDispatchSingleByProjectPath: a share_file
// referencing one project file by /files/project/ path round-trips
// through the dispatch endpoint, lands a file_shared row in alice's
// chat.jsonl carrying the SHA, and surfaces the rendered ack body.
func TestAgentpodShareFileDispatchSingleByProjectPath(t *testing.T) {
	sockPath, srv, cleanup := startPublishServer(t)
	defer cleanup()
	c := agentpod.NewClient(sockPath, "alice")
	ctx := context.Background()

	pf, err := srv.Store.AddProjectFile("data.csv", strings.NewReader("a,b\n1,2\n"))
	if err != nil {
		t.Fatalf("seed project file: %v", err)
	}

	resp, err := c.ShareFileDispatch(ctx, agentpod.ShareFileDispatchRequest{
		Files:   []agentpod.ShareFileEntry{{Path: "/files/project/data.csv"}},
		Caption: "see this",
	})
	if err != nil {
		t.Fatalf("ShareFileDispatch: %v", err)
	}
	if resp.IsError {
		t.Fatalf("dispatch IsError=true: %s", resp.Body)
	}
	if !strings.Contains(resp.Body, "data.csv") || !strings.Contains(resp.Body, "(sha=") {
		t.Errorf("ack body should list data.csv with sha=...: %q", resp.Body)
	}
	if !strings.Contains(resp.Body, "Caption: see this") {
		t.Errorf("ack body should carry caption: %q", resp.Body)
	}

	hist, err := srv.Store.ReadChatHistory("alice")
	if err != nil {
		t.Fatalf("ReadChatHistory: %v", err)
	}
	if len(hist) != 1 {
		t.Fatalf("history len = %d, want 1", len(hist))
	}
	row := hist[0]
	if row.Kind != "file_shared" {
		t.Errorf("Kind = %q, want file_shared", row.Kind)
	}
	if len(row.Attachments) != 1 || row.Attachments[0].SHA != pf.SHA {
		t.Errorf("attachments = %+v, want one with SHA %q", row.Attachments, pf.SHA)
	}
}

// TestAgentpodShareFileDispatchMulti is the multi-file headline:
// two distinct project files share through one call land in ONE
// chat bubble, ordered, both attachments preserved.
func TestAgentpodShareFileDispatchMulti(t *testing.T) {
	sockPath, srv, cleanup := startPublishServer(t)
	defer cleanup()
	c := agentpod.NewClient(sockPath, "alice")
	ctx := context.Background()

	pf1, err := srv.Store.AddProjectFile("first.csv", strings.NewReader("a,b\n1,2\n"))
	if err != nil {
		t.Fatalf("seed first: %v", err)
	}
	pf2, err := srv.Store.AddProjectFile("second.md", strings.NewReader("# notes\n"))
	if err != nil {
		t.Fatalf("seed second: %v", err)
	}

	resp, err := c.ShareFileDispatch(ctx, agentpod.ShareFileDispatchRequest{
		Files: []agentpod.ShareFileEntry{
			{Path: "/files/project/first.csv"},
			{Path: "/files/project/second.md", Name: "notes.md"},
		},
	})
	if err != nil {
		t.Fatalf("ShareFileDispatch: %v", err)
	}
	if resp.IsError {
		t.Fatalf("dispatch IsError=true: %s", resp.Body)
	}
	// Ack body lists both lines in input order.
	if !strings.Contains(resp.Body, "- first.csv (sha=") {
		t.Errorf("ack missing first.csv line: %q", resp.Body)
	}
	if !strings.Contains(resp.Body, "- notes.md (sha=") {
		t.Errorf("ack missing notes.md line (display_name override should win): %q", resp.Body)
	}

	hist, err := srv.Store.ReadChatHistory("alice")
	if err != nil {
		t.Fatalf("ReadChatHistory: %v", err)
	}
	if len(hist) != 1 {
		t.Fatalf("history len = %d, want 1 (multi-file share lands in ONE bubble)", len(hist))
	}
	row := hist[0]
	if len(row.Attachments) != 2 {
		t.Fatalf("attachments len = %d, want 2", len(row.Attachments))
	}
	if row.Attachments[0].SHA != pf1.SHA || row.Attachments[0].Name != "first.csv" {
		t.Errorf("att[0] = %+v, want sha=%q name=first.csv", row.Attachments[0], pf1.SHA)
	}
	if row.Attachments[1].SHA != pf2.SHA || row.Attachments[1].Name != "notes.md" {
		t.Errorf("att[1] = %+v, want sha=%q name=notes.md (display_name override)", row.Attachments[1], pf2.SHA)
	}
}

// TestAgentpodShareFileDispatchUnreachablePath: a path that doesn't
// resolve surfaces as IsError=true with no chat row written.
func TestAgentpodShareFileDispatchUnreachablePath(t *testing.T) {
	sockPath, srv, cleanup := startPublishServer(t)
	defer cleanup()
	c := agentpod.NewClient(sockPath, "alice")
	ctx := context.Background()

	resp, err := c.ShareFileDispatch(ctx, agentpod.ShareFileDispatchRequest{
		Files: []agentpod.ShareFileEntry{{Path: "/files/project/nonexistent.csv"}},
	})
	if err != nil {
		t.Fatalf("ShareFileDispatch: %v", err)
	}
	if !resp.IsError {
		t.Errorf("expected IsError=true for unknown path, got %+v", resp)
	}
	hist, _ := srv.Store.ReadChatHistory("alice")
	if len(hist) != 0 {
		t.Errorf("history should be empty on resolution failure, got %d rows", len(hist))
	}
}

// TestAgentpodPublishStageThenCommit: a publish_notice round-trips
// through stage + commit, lands a message on disk, the recipient's
// inbox is updated, and the commit response acknowledges the publish.
func TestAgentpodPublishStageThenCommit(t *testing.T) {
	sockPath, srv, cleanup := startPublishServer(t)
	defer cleanup()
	c := agentpod.NewClient(sockPath, "alice")
	ctx := context.Background()

	stage, err := c.StagePublish(ctx, "publish_notice",
		json.RawMessage(`{"to":["bob"],"title":"Q3 analysis","body":"Focus on APAC."}`))
	if err != nil {
		t.Fatalf("StagePublish: %v", err)
	}
	if stage.IsError || stage.StageID == "" {
		t.Fatalf("stage IsError=%v body=%q stageID=%q", stage.IsError, stage.Body, stage.StageID)
	}
	// No message should exist yet — stage doesn't commit.
	msgs, _ := srv.Store.ListMessages(store.MessageFilter{Type: store.MsgNotice, From: "alice"})
	if len(msgs) != 0 {
		t.Fatalf("stage should not write a message, got %d", len(msgs))
	}

	commit, err := c.CommitPublish(ctx, stage.StageID)
	if err != nil {
		t.Fatalf("CommitPublish: %v", err)
	}
	if commit.IsError {
		t.Fatalf("commit IsError=true: %s", commit.Body)
	}
	if !strings.Contains(commit.Body, "publish_notice published") {
		t.Errorf("ack body = %q, want it to confirm publish", commit.Body)
	}

	msgs, err = srv.Store.ListMessages(store.MessageFilter{Type: store.MsgNotice, From: "alice"})
	if err != nil {
		t.Fatalf("ListMessages: %v", err)
	}
	if len(msgs) != 1 {
		t.Fatalf("want 1 message, got %d", len(msgs))
	}
	got := msgs[0]
	if got.To.Primary() != "bob" || got.Title != "Q3 analysis" {
		t.Errorf("unexpected message: %+v", got)
	}

	q, err := srv.Store.ReadMessageQueue()
	if err != nil {
		t.Fatalf("ReadMessageQueue: %v", err)
	}
	if len(q.Agents["bob"].Inbox) != 1 {
		t.Errorf("bob inbox = %v, want one entry", q.Agents["bob"].Inbox)
	}
	if !q.HasCommittedStageID(stage.StageID) {
		t.Errorf("CommittedStageIDs missing %q after commit: %v", stage.StageID, q.CommittedStageIDs)
	}
}

// TestAgentpodPublishStageUnknownAttachmentSHA: invalid SHA in the
// attachments list rejects at stage with IsError=true, no staged
// record on disk, no follow-up commit possible.
func TestAgentpodPublishStageUnknownAttachmentSHA(t *testing.T) {
	sockPath, srv, cleanup := startPublishServer(t)
	defer cleanup()
	c := agentpod.NewClient(sockPath, "alice")
	ctx := context.Background()

	stage, err := c.StagePublish(ctx, "publish_notice",
		json.RawMessage(`{"to":["bob"],"title":"Process","body":"see attached","attachments":[{"name":"phantom.csv","sha":"0000000000000000000000000000000000000000000000000000000000000000"}]}`))
	if err != nil {
		t.Fatalf("StagePublish: %v", err)
	}
	if !stage.IsError {
		t.Fatalf("expected IsError=true for unreachable SHA, got %+v", stage)
	}
	if stage.StageID != "" {
		t.Errorf("expected empty StageID on stage error, got %q", stage.StageID)
	}
	msgs, _ := srv.Store.ListMessages(store.MessageFilter{Type: store.MsgNotice, From: "alice"})
	if len(msgs) != 0 {
		t.Errorf("no message should be persisted on attachment failure, got %d", len(msgs))
	}
}

// TestAgentpodPublishCommitIdempotent: replaying CommitPublish with
// the same StageID after a successful commit returns a 200 response
// with a "duplicate commit suppressed" body, NO new message, and NO
// second inbox entry. This is the property that makes the bridge's
// transport retry safe.
func TestAgentpodPublishCommitIdempotent(t *testing.T) {
	sockPath, srv, cleanup := startPublishServer(t)
	defer cleanup()
	c := agentpod.NewClient(sockPath, "alice")
	ctx := context.Background()

	stage, err := c.StagePublish(ctx, "publish_notice",
		json.RawMessage(`{"to":["bob"],"title":"Heartbeat","body":"All good."}`))
	if err != nil || stage.IsError {
		t.Fatalf("StagePublish: err=%v isErr=%v body=%q", err, stage.IsError, stage.Body)
	}

	first, err := c.CommitPublish(ctx, stage.StageID)
	if err != nil || first.IsError {
		t.Fatalf("first commit: err=%v isErr=%v body=%q", err, first.IsError, first.Body)
	}

	// Replay — server deletes the staged record after a successful
	// commit, so to exercise the CommittedStageIDs short-circuit we
	// rewrite the record (simulating "core died between commit and
	// response; bridge retries with the same stage_id"). The queue
	// must NOT pick up a second inbox entry on the retry.
	committed := replayStagedMessage(t, srv, "alice")
	rec := store.StagedPublish{
		StageID:   stage.StageID,
		Tool:      "publish_notice",
		From:      "alice",
		Message:   committed,
		CreatedAt: committed.Date,
	}
	if werr := srv.Store.WriteStagedPublish(rec); werr != nil {
		t.Fatalf("re-write staged publish for replay: %v", werr)
	}

	second, err := c.CommitPublish(ctx, stage.StageID)
	if err != nil {
		t.Fatalf("second commit: %v", err)
	}
	if second.IsError {
		t.Fatalf("second commit IsError=true: %s", second.Body)
	}
	if !strings.Contains(second.Body, "duplicate commit suppressed") {
		t.Errorf("second commit body = %q, want it to mention duplicate suppression", second.Body)
	}

	msgs, _ := srv.Store.ListMessages(store.MessageFilter{Type: store.MsgNotice, From: "alice"})
	if len(msgs) != 1 {
		t.Errorf("want exactly 1 notice after idempotent replay, got %d", len(msgs))
	}
	q, _ := srv.Store.ReadMessageQueue()
	if got := len(q.Agents["bob"].Inbox); got != 1 {
		t.Errorf("bob inbox = %d entries, want 1 (no duplicate)", got)
	}
}

// TestAgentpodPublishCommitNotFound: committing with a stage_id that
// was never staged (or expired) returns ErrCommitStageNotFound. The
// bridge translates this to a definitive "not committed" outcome
// rather than retrying forever.
func TestAgentpodPublishCommitNotFound(t *testing.T) {
	sockPath, _, cleanup := startPublishServer(t)
	defer cleanup()
	c := agentpod.NewClient(sockPath, "alice")
	ctx := context.Background()

	_, err := c.CommitPublish(ctx, "0123456789abcdef")
	if !errors.Is(err, agentpod.ErrCommitStageNotFound) {
		t.Fatalf("commit with unknown stage_id: err = %v, want ErrCommitStageNotFound", err)
	}
}

// replayStagedMessage rebuilds a staged-publish message for the
// idempotency test by reading what alice's previous commit wrote
// to disk. Avoids depending on the precise time-stamp the server
// pinned at stage time.
func replayStagedMessage(t *testing.T, srv *Server, from string) store.Message {
	t.Helper()
	msgs, err := srv.Store.ListMessages(store.MessageFilter{From: from})
	if err != nil || len(msgs) == 0 {
		t.Fatalf("replayStagedMessage: no message found for %s: err=%v", from, err)
	}
	return msgs[len(msgs)-1]
}
