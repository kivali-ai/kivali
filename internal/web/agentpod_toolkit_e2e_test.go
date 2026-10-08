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
	"github.com/kivali-ai/kivali/internal/devshell"
	"github.com/kivali-ai/kivali/internal/files"
	"github.com/kivali-ai/kivali/internal/mcp"
	"github.com/kivali-ai/kivali/internal/messaging"
	"github.com/kivali-ai/kivali/internal/store"
)

// startFullAgentpodServer wires a Server with EVERY dispatch endpoint
// the FullAgentToolkit path exercises. The smoke tests below build a
// toolkit against the resulting Client and call tool handlers directly
// to verify the full chain works.
func startFullAgentpodServer(t *testing.T) (sockPath string, srv *Server, cleanup func()) {
	t.Helper()
	srv = newTestServer(t)
	if err := srv.Store.CreateAgent(store.Agent{Slug: "alice", Role: "Analyst", ReportsTo: "chief-of-staff"}, "# Analyst\n"); err != nil {
		t.Fatalf("seed alice: %v", err)
	}
	if err := srv.Store.CreateAgent(store.Agent{Slug: "bob", Role: "Researcher", ReportsTo: "chief-of-staff"}, "# Bob\n"); err != nil {
		t.Fatalf("seed bob: %v", err)
	}
	if err := srv.Store.CreateAgent(store.Agent{Slug: "chief-of-staff", Role: "CoS", ReportsTo: "ceo"}, "# CoS\n"); err != nil {
		t.Fatalf("seed CoS: %v", err)
	}
	srv.Runtime = agent.NewRuntime(srv.Store, agent.RuntimeDefaults{})
	srv.Messenger = messaging.New(srv.Store, srv.Runtime)

	mux := http.NewServeMux()
	// Mirror the route registrations in control_socket.go; only the
	// endpoints the agent-pod toolkit actually hits.
	mux.HandleFunc("/v1/agent/{slug}/role", srv.handleAgentpodRole)
	mux.HandleFunc("/v1/agent/{slug}/memory", srv.handleAgentpodMemory)
	mux.HandleFunc("POST /v1/agent/{slug}/memory/dispatch", srv.handleAgentpodMemoryDispatch)
	mux.HandleFunc("POST /v1/agent/{slug}/share-file/dispatch", srv.handleAgentpodShareFileDispatch)
	mux.HandleFunc("POST /v1/agent/{slug}/artifact/dispatch", srv.handleAgentpodArtifactDispatch)
	mux.HandleFunc("POST /v1/agent/{slug}/publish/stage", srv.handleAgentpodPublishStage)
	mux.HandleFunc("POST /v1/agent/{slug}/publish/commit", srv.handleAgentpodPublishCommit)
	mux.HandleFunc("POST /v1/agent/{slug}/state/dispatch", srv.handleAgentpodStateDispatch)
	mux.HandleFunc("GET /v1/agent/{slug}/past-chats", srv.handleAgentpodPastChatsList)
	mux.HandleFunc("GET /v1/agent/{slug}/past-chats/{ts}", srv.handleAgentpodPastChatsRead)
	mux.HandleFunc("GET /v1/project-files", srv.handleAgentpodProjectFiles)

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

// startDevShellDaemon serves the dev-shell daemon's handler on a UDS,
// rooted at root, and returns the socket path. /tmp keeps the path
// under darwin's sun_path limit.
func startDevShellDaemon(t *testing.T, root string) string {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "wds-")
	if err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	sock := filepath.Join(dir, "s")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	httpSrv := &http.Server{Handler: devshell.NewHandler(devshell.ServerConfig{Root: root}), ReadHeaderTimeout: 5 * time.Second}
	go func() {
		if err := httpSrv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			t.Logf("serve: %v", err)
		}
	}()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = httpSrv.Shutdown(ctx)
	})
	return sock
}

// findTool is a tiny lookup helper for the assertions below.
func findTool(t *testing.T, tools []mcp.Tool, name string) mcp.Tool {
	t.Helper()
	for _, tl := range tools {
		if tl.Name == name {
			return tl
		}
	}
	t.Fatalf("tool %q missing from toolkit; have: %v", name, toolNames(tools))
	return mcp.Tool{}
}

func toolNames(tools []mcp.Tool) []string {
	names := make([]string, 0, len(tools))
	for _, tl := range tools {
		names = append(names, tl.Name)
	}
	return names
}

// TestAgentpodToolkitAgentMemoryAppendRoundTrips: the in-pod toolkit
// calls agent_memory_append; the bytes route through the dispatch
// endpoint and land in the authoritative store. Smoke test for the
// full chain.
func TestAgentpodToolkitAgentMemoryAppendRoundTrips(t *testing.T) {
	sockPath, srv, cleanup := startFullAgentpodServer(t)
	defer cleanup()
	c := agentpod.NewClient(sockPath, "alice")
	tk := mcp.FullAgentToolkit(mcp.FullAgentDeps{
		Client: c,
		Files:  mcp.NewLocalFilesDispatcher(&files.Backend{Root: t.TempDir()}),
	})

	// Seed memory so append concatenates rather than seeding from empty.
	if err := c.WriteMemory(context.Background(), "existing\n"); err != nil {
		t.Fatalf("seed memory: %v", err)
	}

	tool := findTool(t, tk.Tools, "agent_memory_append")
	res, err := tool.Handler(context.Background(), json.RawMessage(`{"text":"new note"}`))
	if err != nil {
		t.Fatalf("Handler: %v", err)
	}
	if res.IsError {
		t.Fatalf("agent_memory_append IsError=true: %v", res.Content)
	}

	got, err := srv.Store.ReadAgentMemory("alice")
	if err != nil {
		t.Fatalf("ReadAgentMemory: %v", err)
	}
	if !strings.Contains(got, "existing") || !strings.Contains(got, "new note") {
		t.Errorf("memory after append = %q, want both lines", got)
	}
}

// TestAgentpodToolkitArtifactPublishRoundTrips: artifact_publish and
// artifact_unpublish from the in-pod toolkits reach core over the UDS;
// core copies from the caller's workspace (a subagent's view for a
// subagent) into the parent's published tree and answers with the
// node the index built.
func TestAgentpodToolkitArtifactPublishRoundTrips(t *testing.T) {
	sockPath, srv, cleanup := startFullAgentpodServer(t)
	defer cleanup()
	root := srv.Store.Root()
	storage := files.StorageRoot(filepath.Join(root, "agents", "alice"))
	writeFile(t, filepath.Join(storage, "artifacts", "private", "specs", "api.md"), "---\nid: api\n---\nthe API\n")
	c := agentpod.NewClient(sockPath, "alice")
	tk := mcp.FullAgentToolkit(mcp.FullAgentDeps{
		Client: c,
		Files:  mcp.NewLocalFilesDispatcher(&files.Backend{Root: storage}),
	})
	res, err := findTool(t, tk.Tools, "artifact_publish").Handler(context.Background(), json.RawMessage(`{"source":"/files/artifacts/private/specs/api.md"}`))
	if err != nil || res.IsError {
		t.Fatalf("artifact_publish: %v %v", err, res.Content)
	}
	if body := strings.Join(res.Content, ""); !strings.Contains(body, "specs/api.md → node alice/api v1, current") {
		t.Errorf("reply does not report the node:\n%s", body)
	}
	if b, err := os.ReadFile(filepath.Join(files.PublishedDir(root, "alice"), "specs", "api.md")); err != nil || !strings.Contains(string(b), "the API") {
		t.Errorf("published copy: %q, %v", b, err)
	}

	// A subagent's toolkit publishes from its own view into alice's area.
	overlay := filepath.Join(storage, "subagents", "sa-1")
	if err := files.BuildSubagentOverlay(files.SubagentOverlay{Root: overlay}); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(overlay, "artifacts", "private", "found.md"), "a finding")
	sub := mcp.SubagentToolkit(mcp.SubagentDeps{
		Client:     agentpod.NewClient(sockPath, "alice"),
		SubagentID: "sa-1",
		Files:      mcp.NewLocalFilesDispatcher(&files.Backend{Root: overlay}),
	})
	res, err = findTool(t, sub.Tools, "artifact_publish").Handler(context.Background(), json.RawMessage(`{"source":"/files/artifacts/private/found.md","dest":"notes/found.md"}`))
	if err != nil || res.IsError {
		t.Fatalf("subagent artifact_publish: %v %v", err, res.Content)
	}
	if b, err := os.ReadFile(filepath.Join(files.PublishedDir(root, "alice"), "notes", "found.md")); err != nil || string(b) != "a finding" {
		t.Errorf("subagent's published copy: %q, %v", b, err)
	}

	res, err = findTool(t, tk.Tools, "artifact_unpublish").Handler(context.Background(), json.RawMessage(`{"path":"specs"}`))
	if err != nil || res.IsError {
		t.Fatalf("artifact_unpublish: %v %v", err, res.Content)
	}
	if body := strings.Join(res.Content, ""); !strings.Contains(body, "Removed from the graph: alice/api.") {
		t.Errorf("unpublish reply:\n%s", body)
	}
	// A refusal comes back as a tool error, not a transport failure.
	res, err = findTool(t, tk.Tools, "artifact_publish").Handler(context.Background(), json.RawMessage(`{"source":"/files/project/x.md"}`))
	if err != nil || !res.IsError {
		t.Errorf("publishing from project/ = %v, %v; want a tool error", res, err)
	}
}

// TestAgentpodToolkitGetOrgChartRoundTrips: get_org_chart through
// the in-pod toolkit lands at the state/dispatch endpoint and
// returns a timestamped chart with the seeded agents.
func TestAgentpodToolkitGetOrgChartRoundTrips(t *testing.T) {
	sockPath, _, cleanup := startFullAgentpodServer(t)
	defer cleanup()
	c := agentpod.NewClient(sockPath, "alice")
	tk := mcp.FullAgentToolkit(mcp.FullAgentDeps{
		Client: c,
		Files:  mcp.NewLocalFilesDispatcher(&files.Backend{Root: t.TempDir()}),
	})

	tool := findTool(t, tk.Tools, "get_org_chart")
	res, err := tool.Handler(context.Background(), json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("Handler: %v", err)
	}
	if res.IsError {
		t.Fatalf("get_org_chart IsError=true: %v", res.Content)
	}
	body := res.Content[0]
	if !strings.Contains(body, "as of ") {
		t.Errorf("missing snapshot timestamp: %q", body)
	}
	if !strings.Contains(body, "alice") {
		t.Errorf("missing alice in chart: %q", body)
	}
	if !strings.Contains(body, "chief-of-staff") {
		t.Errorf("missing CoS in chart: %q", body)
	}
}

// TestAgentpodToolkitPublishNoticeRoundTrips: a publish through the
// in-pod toolkit lands a message on disk via the publish/dispatch
// endpoint.
func TestAgentpodToolkitPublishNoticeRoundTrips(t *testing.T) {
	sockPath, srv, cleanup := startFullAgentpodServer(t)
	defer cleanup()
	c := agentpod.NewClient(sockPath, "alice")
	tk := mcp.FullAgentToolkit(mcp.FullAgentDeps{
		Client: c,
		Files:  mcp.NewLocalFilesDispatcher(&files.Backend{Root: t.TempDir()}),
	})

	tool := findTool(t, tk.Tools, "publish_notice")
	res, err := tool.Handler(context.Background(),
		json.RawMessage(`{"to":["bob"],"title":"Q3","body":"Focus."}`))
	if err != nil {
		t.Fatalf("Handler: %v", err)
	}
	if res.IsError {
		t.Fatalf("publish IsError=true: %v", res.Content)
	}

	msgs, err := srv.Store.ListMessages(store.MessageFilter{Type: store.MsgNotice, From: "alice"})
	if err != nil {
		t.Fatalf("ListMessages: %v", err)
	}
	if len(msgs) != 1 || msgs[0].Title != "Q3" {
		t.Errorf("unexpected message persistence: %+v", msgs)
	}
}

// TestAgentpodToolkitFileViewRoundTrips: file_view in the full-agent
// toolkit goes to the dev-shell daemon (as it does in a pod, where the
// agent container mounts no /files) and returns the content the
// daemon read from the agent's tree.
func TestAgentpodToolkitFileViewRoundTrips(t *testing.T) {
	sockPath, srv, cleanup := startFullAgentpodServer(t)
	defer cleanup()
	filesRoot := filepath.Join(srv.Store.Root(), "agents", "alice", "memory")
	devSock := startDevShellDaemon(t, filesRoot)
	c := agentpod.NewClient(sockPath, "alice")
	tk := mcp.FullAgentToolkit(mcp.FullAgentDeps{
		Client: c,
		Files:  mcp.NewSidecarFilesDispatcher(&devshell.SidecarFiles{SocketPath: devSock}),
	})

	// Seed an artifact in alice's writable area so file_view returns
	// real bytes.
	root := filepath.Join(filesRoot, "artifacts", "private")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(root, "note.md"), []byte("# the note body\n"), 0o644); err != nil {
		t.Fatalf("seed: %v", err)
	}

	tool := findTool(t, tk.Tools, "file_view")
	res, err := tool.Handler(context.Background(),
		json.RawMessage(`{"path":"/files/artifacts/private/note.md"}`))
	if err != nil {
		t.Fatalf("Handler: %v", err)
	}
	if res.IsError {
		t.Fatalf("file_view IsError=true: %v", res.Content)
	}
	body := res.Content[0]
	if !strings.Contains(body, "the note body") {
		t.Errorf("file_view body missing content: %q", body)
	}
}

// TestAgentpodToolkitListsExpectedCoSTools: the CoS-bound toolkit
// exposes the canonical full-agent surface plus the CoS-only tools.
// Regression target if a tool is accidentally dropped or the gating
// drifts.
func TestAgentpodToolkitListsExpectedCoSTools(t *testing.T) {
	sockPath, _, cleanup := startFullAgentpodServer(t)
	defer cleanup()
	c := agentpod.NewClient(sockPath, "chief-of-staff")
	tk := mcp.FullAgentToolkit(mcp.FullAgentDeps{
		Client:         c,
		IsChiefOfStaff: true,
		Files:          mcp.NewLocalFilesDispatcher(&files.Backend{Root: t.TempDir()}),
	})

	got := map[string]bool{}
	for _, n := range toolNames(tk.Tools) {
		got[n] = true
	}

	mustHave := []string{
		// File ops
		"file_view", "file_create", "file_str_replace",
		"file_insert", "file_delete", "file_rename", "file_copy",
		// Agent memory
		"agent_memory_view", "agent_memory_append", "agent_memory_str_replace",
		// State lookups
		"get_org_chart", "list_skills",
		// Knowledge graph
		"graph_query", "graph_node",
		// Assignment tracker
		"assignment_create", "assignment_update", "assignment_close",
		"assignment_reopen", "assignment_list", "assignment_view",
		// Habits
		"agent_memory_habits_view", "agent_memory_habits_append", "agent_memory_habits_str_replace",
		// Search & browse
		"search_past_chats", "list_project_files",
		// Publish (universal)
		"publish_notice",
		"publish_ceo_approval_request", "publish_ceo_notification",
		// CoS-only
		"read_agent_role",
		"propose_role_update", "propose_handbook_update", "propose_reorg",
	}
	for _, n := range mustHave {
		if !got[n] {
			t.Errorf("toolkit missing expected tool %q; have %v", n, toolNames(tk.Tools))
		}
	}
}
