package mcp

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kivali-ai/kivali/internal/files"
	"github.com/kivali-ai/kivali/internal/store"
)

// TestPublishAttachmentByProjectPath verifies an agent
// can re-attach an existing project file via path, without ever
// touching a SHA. /files/project/<name> resolves to the project
// file's bytes; the dispatcher's content-addressing yields the
// project file's existing SHA (idempotent — no new blob written).
func TestPublishAttachmentByProjectPath(t *testing.T) {
	s, err := store.New(t.TempDir())
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	if err := s.CreateAgent(store.Agent{Slug: "alice", Role: "Analyst", ReportsTo: "chief-of-staff"}, "role"); err != nil {
		t.Fatalf("create alice: %v", err)
	}
	if err := s.CreateAgent(store.Agent{Slug: "bob", Role: "Writer", ReportsTo: "alice"}, "role"); err != nil {
		t.Fatalf("create bob: %v", err)
	}
	pf, err := s.AddProjectFile("data.csv", strings.NewReader("a,b\n1,2\n"))
	if err != nil {
		t.Fatalf("seed project file: %v", err)
	}
	// Provision alice so /files/project/<name> resolves.
	aliceMem := &files.Backend{Root: files.StorageRoot(filepath.Join(s.Root(), "agents", "alice"))}
	if err := s.SyncAgentFilesystem("alice"); err != nil {
		t.Fatalf("sync: %v", err)
	}

	tools := PublishTools(PublishToolsConfig{
		Dispatcher: NewStorePublishDispatcher(StorePublishDeps{Store: s, FilesystemBackend: aliceMem, From: "alice"}),
		From:       "alice",
	})
	c := startServer(t, tools)

	result := c.call(t, "tools/call", map[string]any{
		"name": "publish_notice",
		"arguments": map[string]any{
			"to":    []string{"bob"},
			"title": "Process this dataset",
			"body":  "see attached",
			"attachments": []map[string]any{
				{"name": "renamed.csv", "path": "/files/project/data.csv"},
			},
		},
	})
	var r toolsCallResult
	if err := json.Unmarshal(result, &r); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if r.IsError {
		t.Fatalf("publish errored: %+v", r.Content)
	}

	messages, err := s.ListMessages(store.MessageFilter{Type: store.MsgNotice, From: "alice"})
	if err != nil {
		t.Fatalf("list messages: %v", err)
	}
	if len(messages) != 1 {
		t.Fatalf("want 1 message, got %d", len(messages))
	}
	got := messages[0]
	if len(got.Attachments) != 1 {
		t.Fatalf("want 1 attachment, got %d", len(got.Attachments))
	}
	// Path resolves to the project file's existing SHA — same content,
	// same SHA. No new attachment blob written.
	if got.Attachments[0].SHA != pf.SHA {
		t.Errorf("attachment SHA = %q, want project file SHA %q (path-resolved bytes content-address to existing SHA)",
			got.Attachments[0].SHA, pf.SHA)
	}
	if got.Attachments[0].Name != "renamed.csv" {
		t.Errorf("attachment Name = %q, want agent-supplied display name", got.Attachments[0].Name)
	}
}

// TestPublishAttachmentByPathRejectsUnknown verifies an
// unknown path is rejected with a clear error rather than silently
// attaching something phantom.
func TestPublishAttachmentByPathRejectsUnknown(t *testing.T) {
	s, err := store.New(t.TempDir())
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	if err := s.CreateAgent(store.Agent{Slug: "alice", Role: "Analyst", ReportsTo: "chief-of-staff"}, "role"); err != nil {
		t.Fatalf("create alice: %v", err)
	}
	if err := s.CreateAgent(store.Agent{Slug: "bob", Role: "Writer", ReportsTo: "alice"}, "role"); err != nil {
		t.Fatalf("create bob: %v", err)
	}
	aliceMem := &files.Backend{Root: files.StorageRoot(filepath.Join(s.Root(), "agents", "alice"))}

	tools := PublishTools(PublishToolsConfig{
		Dispatcher: NewStorePublishDispatcher(StorePublishDeps{Store: s, FilesystemBackend: aliceMem, From: "alice"}),
		From:       "alice",
	})
	c := startServer(t, tools)

	result := c.call(t, "tools/call", map[string]any{
		"name": "publish_notice",
		"arguments": map[string]any{
			"to":    []string{"bob"},
			"title": "Process this dataset",
			"body":  "see attached",
			"attachments": []map[string]any{
				{"path": "/files/artifacts/private/never-existed.csv"},
			},
		},
	})
	var r toolsCallResult
	if err := json.Unmarshal(result, &r); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !r.IsError {
		t.Fatalf("expected IsError=true for missing path, got: %+v", r.Content)
	}
	messages, _ := s.ListMessages(store.MessageFilter{Type: store.MsgNotice, From: "alice"})
	if len(messages) != 0 {
		t.Errorf("no message should be persisted on attachment failure, got %d", len(messages))
	}
}

// TestPublishAttachmentByPath: an agent attaches a file from
// /files/artifacts/private/ to a publish_notice via the path field.
// Bytes are content-addressed at publish time; no base64 enters the
// chat history.
func TestPublishAttachmentByPath(t *testing.T) {
	s, err := store.New(t.TempDir())
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	if err := s.CreateAgent(store.Agent{Slug: "alice", Role: "Analyst", ReportsTo: "chief-of-staff"}, "role"); err != nil {
		t.Fatalf("create alice: %v", err)
	}
	if err := s.CreateAgent(store.Agent{Slug: "bob", Role: "Writer", ReportsTo: "alice"}, "role"); err != nil {
		t.Fatalf("create bob: %v", err)
	}
	aliceMem := &files.Backend{Root: files.StorageRoot(filepath.Join(s.Root(), "agents", "alice"))}
	if err := files.Sync(files.BootstrapOptions{AgentRoot: filepath.Join(s.Root(), "agents", "alice")}); err != nil {
		t.Fatalf("files sync: %v", err)
	}
	// Plant the artifact bytes that the publish will reference.
	if _, _, err := files.Dispatch(aliceMem, files.ToolCreate,
		json.RawMessage(`{"path":"/files/artifacts/private/HDP_RC6.tar.gz","file_text":"tarball-bytes"}`)); err != nil {
		t.Fatalf("seed artifact: %v", err)
	}

	tools := PublishTools(PublishToolsConfig{
		Dispatcher: NewStorePublishDispatcher(StorePublishDeps{
			Store:             s,
			FilesystemBackend: aliceMem,
			From:              "alice",
		}),
		From: "alice",
	})
	c := startServer(t, tools)

	result := c.call(t, "tools/call", map[string]any{
		"name": "publish_notice",
		"arguments": map[string]any{
			"to":    []string{"bob"},
			"title": "Review RC6",
			"body":  "see attached",
			"attachments": []map[string]any{
				// name omitted — defaults to basename of path.
				{"path": "/files/artifacts/private/HDP_RC6.tar.gz"},
			},
		},
	})
	var r toolsCallResult
	if err := json.Unmarshal(result, &r); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if r.IsError {
		t.Fatalf("publish_notice with path attachment returned error: %+v", r.Content)
	}

	messages, err := s.ListMessages(store.MessageFilter{Type: store.MsgNotice, From: "alice"})
	if err != nil {
		t.Fatalf("list messages: %v", err)
	}
	if len(messages) != 1 {
		t.Fatalf("want 1 message, got %d", len(messages))
	}
	msg := messages[0]
	if len(msg.Attachments) != 1 {
		t.Fatalf("want 1 attachment, got %d", len(msg.Attachments))
	}
	att := msg.Attachments[0]
	if att.SHA == "" {
		t.Error("attachment SHA empty — bytes weren't content-addressed")
	}
	if att.Name != "HDP_RC6.tar.gz" {
		t.Errorf("attachment Name = %q, want HDP_RC6.tar.gz (default to path basename)", att.Name)
	}
	// Bytes must be in the canonical attachment store.
	if _, gerr := s.GetAttachment(att.SHA); gerr != nil {
		t.Errorf("attachment %s not found in store: %v", att.SHA, gerr)
	}
}

// TestPublishAttachmentByPathRejectsTraversal proves the
// path field can't be used to ingest arbitrary host paths. The
// resolver's escape-protection (files.Backend.Resolve) refuses
// paths that leave the agent's /files/ root.
func TestPublishAttachmentByPathRejectsTraversal(t *testing.T) {
	s, err := store.New(t.TempDir())
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	if err := s.CreateAgent(store.Agent{Slug: "alice", Role: "Analyst", ReportsTo: "chief-of-staff"}, "role"); err != nil {
		t.Fatalf("create alice: %v", err)
	}
	if err := s.CreateAgent(store.Agent{Slug: "bob", Role: "Writer", ReportsTo: "alice"}, "role"); err != nil {
		t.Fatalf("create bob: %v", err)
	}
	aliceMem := &files.Backend{Root: files.StorageRoot(filepath.Join(s.Root(), "agents", "alice"))}

	tools := PublishTools(PublishToolsConfig{
		Dispatcher: NewStorePublishDispatcher(StorePublishDeps{
			Store:             s,
			FilesystemBackend: aliceMem,
			From:              "alice",
		}),
		From: "alice",
	})
	c := startServer(t, tools)

	result := c.call(t, "tools/call", map[string]any{
		"name": "publish_notice",
		"arguments": map[string]any{
			"to":    []string{"bob"},
			"title": "Sneaky",
			"body":  "see attached",
			"attachments": []map[string]any{
				{"name": "passwd", "path": "/files/artifacts/private/../../../etc/passwd"},
			},
		},
	})
	var r toolsCallResult
	_ = json.Unmarshal(result, &r)
	if !r.IsError {
		t.Errorf("traversal attempt should fail; got success: %+v", r.Content)
	}
}

// TestPublishProposeRoleUpdateOnlyForCoS asserts the propose_role_update
// tool appears only in the Chief of Staff's tool list. Defense-in-depth
// for the parser-side gate (parseProposeRoleUpdate also rejects on
// caller slug).
func TestPublishProposeRoleUpdateOnlyForCoS(t *testing.T) {
	s, err := store.New(t.TempDir())
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	cases := []struct {
		from   string
		isCoS  bool
		expect bool
	}{
		{"alice", false, false},
		{"chief-of-staff", true, true},
	}
	for _, tc := range cases {
		got := false
		for _, tl := range PublishTools(PublishToolsConfig{
			Dispatcher:     NewStorePublishDispatcher(StorePublishDeps{Store: s, From: tc.from}),
			From:           tc.from,
			IsChiefOfStaff: tc.isCoS,
		}) {
			if tl.Name == "propose_role_update" {
				got = true
				break
			}
		}
		if got != tc.expect {
			t.Errorf("from=%q IsChiefOfStaff=%v: propose_role_update exposed=%v want=%v", tc.from, tc.isCoS, got, tc.expect)
		}
	}
}

// TestPublishTaskRequestFastFailsOnUnknownRecipient verifies the
// TestPublishProposeRoleUpdateFastFailsOnUnknownTarget verifies that
// a CoS-issued propose_role_update naming a slug that is not an
// active agent fails fast at publish time — the CEO never sees the
// approval request, the CoS gets immediate feedback to use the org
// chart.
func TestPublishProposeRoleUpdateFastFailsOnUnknownTarget(t *testing.T) {
	s, err := store.New(t.TempDir())
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	if err := s.CreateAgent(store.Agent{Slug: "chief-of-staff", Role: "CoS", ReportsTo: "ceo"}, "role"); err != nil {
		t.Fatalf("create chief-of-staff: %v", err)
	}
	cosMem := &files.Backend{Root: files.StorageRoot(filepath.Join(s.Root(), "agents", "chief-of-staff"))}
	if err := files.Sync(files.BootstrapOptions{AgentRoot: filepath.Join(s.Root(), "agents", "chief-of-staff")}); err != nil {
		t.Fatalf("files sync: %v", err)
	}
	// Plant the drafted replacement role.md so role_path resolves —
	// the failure we want to observe is the unknown TARGET slug, not
	// a missing artifact.
	if _, _, err := files.Dispatch(cosMem, files.ToolCreate,
		json.RawMessage(`{"path":"/files/artifacts/private/role-phantom.md","file_text":"# New role\n"}`)); err != nil {
		t.Fatalf("seed role artifact: %v", err)
	}

	tools := PublishTools(PublishToolsConfig{
		Dispatcher: NewStorePublishDispatcher(StorePublishDeps{
			Store:             s,
			FilesystemBackend: cosMem,
			From:              "chief-of-staff",
		}),
		From:           "chief-of-staff",
		IsChiefOfStaff: true,
	})
	c := startServer(t, tools)

	result := c.call(t, "tools/call", map[string]any{
		"name": "propose_role_update",
		"arguments": map[string]any{
			"slug":      "phantom",
			"title":     "Update role: Phantom",
			"role_path": "/files/artifacts/private/role-phantom.md",
			"rationale": "ship it",
		},
	})
	var r toolsCallResult
	if err := json.Unmarshal(result, &r); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !r.IsError {
		t.Fatalf("propose_role_update for unknown target should fail fast; got success: %+v", r.Content)
	}
	if len(r.Content) == 0 || !strings.Contains(r.Content[0].Text, "phantom") {
		t.Errorf("error body should name the missing target slug; got %+v", r.Content)
	}
}

// TestPublishProposeReorgFastFailsOnUnknownSlugs verifies propose_reorg
// rejects the whole proposal when ANY named slug (move target or new
// manager) is not an active agent. Bundles the failures so the CoS
// sees the full picture in one round-trip instead of fixing one slug
// at a time. NewManager == "ceo" is still accepted (the org root).
func TestPublishProposeReorgFastFailsOnUnknownSlugs(t *testing.T) {
	s, err := store.New(t.TempDir())
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	if err := s.CreateAgent(store.Agent{Slug: "chief-of-staff", Role: "CoS", ReportsTo: "ceo"}, "role"); err != nil {
		t.Fatalf("create chief-of-staff: %v", err)
	}
	if err := s.CreateAgent(store.Agent{Slug: "alice", Role: "Analyst", ReportsTo: "ceo"}, "role"); err != nil {
		t.Fatalf("create alice: %v", err)
	}
	// "carol" does not exist (the bad move slug); "void-manager" does
	// not exist (the bad new_manager). The third move parents alice
	// under ceo, which is always valid — proves the check doesn't
	// reject "ceo".

	tools := PublishTools(PublishToolsConfig{
		Dispatcher: NewStorePublishDispatcher(StorePublishDeps{
			Store: s,
			From:  "chief-of-staff",
		}),
		From:           "chief-of-staff",
		IsChiefOfStaff: true,
	})
	c := startServer(t, tools)

	result := c.call(t, "tools/call", map[string]any{
		"name": "propose_reorg",
		"arguments": map[string]any{
			"title": "shuffle",
			"moves": []map[string]any{
				{"slug": "carol", "new_manager": "alice"},
				{"slug": "alice", "new_manager": "void-manager"},
				{"slug": "chief-of-staff", "new_manager": "ceo"},
			},
			"rationale": "reshape",
		},
	})
	var r toolsCallResult
	if err := json.Unmarshal(result, &r); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !r.IsError {
		t.Fatalf("propose_reorg with unknown slugs should fail fast; got success: %+v", r.Content)
	}
	body := ""
	if len(r.Content) > 0 {
		body = r.Content[0].Text
	}
	for _, want := range []string{"carol", "void-manager"} {
		if !strings.Contains(body, want) {
			t.Errorf("error body should name %q; got %q", want, body)
		}
	}
	// The valid moves should NOT appear in the error — only the
	// failing slugs do.
	if strings.Contains(body, `"alice"`) && !strings.Contains(body, "void-manager") {
		// alice is fine on its own; it only appears as the "moves[1].slug"
		// owner of a bad new_manager, which the body cites by index.
		// This branch guards against a regression that names alice as
		// missing.
		t.Errorf("error body wrongly cites alice as missing: %q", body)
	}
}

// TestPublishCEOApprovalRequestSkipsRecipientCheck verifies CEO-bound
// publishes are not subject to the recipient check — they route via
// IsCEOBound() to the CEO inbox directly. Regression guard: the
// switch in checkRecipientSlugs intentionally omits CEO-bound types,
// and the fallthrough behaviour (no check) is what makes
// publish_ceo_notification work in a fresh org where the CoS hasn't
// created any peers yet.
func TestPublishCEOApprovalRequestSkipsRecipientCheck(t *testing.T) {
	s, err := store.New(t.TempDir())
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	if err := s.CreateAgent(store.Agent{Slug: "alice", Role: "Analyst", ReportsTo: "ceo"}, "role"); err != nil {
		t.Fatalf("create alice: %v", err)
	}

	tools := PublishTools(PublishToolsConfig{
		Dispatcher: NewStorePublishDispatcher(StorePublishDeps{Store: s, From: "alice"}),
		From:       "alice",
	})
	c := startServer(t, tools)

	result := c.call(t, "tools/call", map[string]any{
		"name": "publish_ceo_notification",
		"arguments": map[string]any{
			"title": "heads up",
			"body":  "just a note",
		},
	})
	var r toolsCallResult
	if err := json.Unmarshal(result, &r); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if r.IsError {
		t.Fatalf("ceo_notification should not be subject to recipient check; got error: %+v", r.Content)
	}
}
