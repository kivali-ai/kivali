package web

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kivali-ai/kivali/internal/mcp"
	"github.com/kivali-ai/kivali/internal/provider"
	"github.com/kivali-ai/kivali/internal/store"
	"github.com/kivali-ai/kivali/internal/web/apitypes"
)

// transcriptServer is alice (reporting to you) with two peers, bob and
// carol, on a fresh store.
func transcriptServer(t *testing.T) *Server {
	t.Helper()
	srv := newTestServer(t)
	for _, a := range []store.Agent{
		{Slug: "alice", Role: "Analyst", ReportsTo: "ceo"},
		{Slug: "bob", Role: "Builder", ReportsTo: "ceo"},
		{Slug: "carol", Role: "Critic", ReportsTo: "ceo"},
	} {
		if err := srv.Store.CreateAgent(a, "role"); err != nil {
			t.Fatalf("seed %s: %v", a.Slug, err)
		}
	}
	return srv
}

// writeMsg persists m and returns its path relative to the store root,
// the form chat rows reference it by.
func writeMsg(t *testing.T, srv *Server, m store.Message) string {
	t.Helper()
	if m.Date.IsZero() {
		m.Date = tsAt(0)
	}
	abs, err := srv.Store.WriteMessage(m)
	if err != nil {
		t.Fatalf("write message: %v", err)
	}
	rel, err := filepath.Rel(srv.Store.Root(), abs)
	if err != nil {
		t.Fatal(err)
	}
	return rel
}

// ts is a fixed instant plus n seconds.
func tsAt(n int) time.Time {
	return time.Date(2026, 9, 28, 12, 0, n, 0, time.UTC)
}

func msAt(n int) int64 { return tsAt(n).UnixMilli() }

// writeSubagentMetaFile writes meta.json for one of alice's tasks the
// way SubagentService.writeSubagentMeta does.
func writeSubagentMetaFile(t *testing.T, srv *Server, id string, meta subagentMeta) {
	t.Helper()
	path := subagentMetaPath(srv.Store.Root(), "alice", id)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	body, _ := json.Marshal(meta)
	if err := os.WriteFile(path, body, 0o644); err != nil {
		t.Fatal(err)
	}
}

func deref[T any](t *testing.T, name string, p *T) T {
	t.Helper()
	if p == nil {
		t.Fatalf("%s is absent", name)
	}
	return *p
}

// One fixture per stored kind: the rows it maps to and what they carry.
func TestTranscriptRowsByKind(t *testing.T) {
	opus := provider.MockModelLarge
	cases := []struct {
		name  string
		setup func(t *testing.T, srv *Server) []store.ChatMessage
		check func(t *testing.T, rows []apitypes.TranscriptRow)
	}{
		{
			name: "your message",
			setup: func(t *testing.T, srv *Server) []store.ChatMessage {
				att, err := srv.Store.AddAttachmentFromText("quote.txt", "twelve bytes")
				if err != nil {
					t.Fatal(err)
				}
				return []store.ChatMessage{{Role: store.RoleReceived, Kind: "direct_chat", Content: "Ship it", TS: tsAt(1),
					Attachments: []store.MessageAttachment{{SHA: att.SHA, Name: "quote.txt"}}}}
			},
			check: func(t *testing.T, rows []apitypes.TranscriptRow) {
				r := oneRow(t, rows, apitypes.TranscriptKindMessage)
				if deref(t, "role", r.Role) != apitypes.MessageRoleSent || *r.From != (apitypes.ChatParty{Kind: apitypes.PartyKindPerson, Slug: "ceo", Name: "You"}) {
					t.Errorf("role/from = %v/%v, want sent from you", *r.Role, *r.From)
				}
				if deref(t, "body_md", r.BodyMD) != "Ship it" || r.TS != msAt(1) || deref(t, "pending", r.Pending) {
					t.Errorf("row = %+v", r)
				}
				atts := deref(t, "attachments", r.Attachments)
				if len(atts) != 1 || atts[0].Name != "quote.txt" || atts[0].SizeBytes != 12 || atts[0].URL == "" {
					t.Errorf("attachments = %+v", atts)
				}
				if r.Model != nil || r.SourceKind != nil {
					t.Errorf("a message you wrote has model %v, source_kind %v", r.Model, r.SourceKind)
				}
			},
		},
		{
			name: "agent reply with model and effort",
			setup: func(*testing.T, *Server) []store.ChatMessage {
				return []store.ChatMessage{{Role: store.RoleSent, Kind: "direct_chat", Content: "Done", TS: tsAt(2), Model: opus, Effort: "high"}}
			},
			check: func(t *testing.T, rows []apitypes.TranscriptRow) {
				r := oneRow(t, rows, apitypes.TranscriptKindMessage)
				if *r.Role != apitypes.MessageRoleReceived || r.From.Slug != "alice" || r.From.Name != "Analyst" || r.From.Kind != apitypes.PartyKindAgent {
					t.Errorf("role/from = %v/%+v", *r.Role, *r.From)
				}
				if deref(t, "model", r.Model) != "Mock Large" || deref(t, "effort", r.Effort) != "high" || r.IsError {
					t.Errorf("model/effort = %v/%v", r.Model, r.Effort)
				}
			},
		},
		{
			name: "other kinds are messages that name their kind",
			setup: func(*testing.T, *Server) []store.ChatMessage {
				return []store.ChatMessage{{Role: store.RoleReceived, Kind: "shell_captured", Content: "out", Quiet: true}}
			},
			check: func(t *testing.T, rows []apitypes.TranscriptRow) {
				r := oneRow(t, rows, apitypes.TranscriptKindMessage)
				if deref(t, "source_kind", r.SourceKind) != "shell_captured" || !r.Quiet {
					t.Errorf("source_kind/quiet = %v/%v", r.SourceKind, r.Quiet)
				}
			},
		},
		{
			name: "assignment event delivery",
			setup: func(t *testing.T, srv *Server) []store.ChatMessage {
				ref := writeMsg(t, srv, store.Message{Type: store.MsgAssignmentEvent, Title: "Assigned #7", From: "bob",
					To: store.Recipients{"alice"}, Body: "Please take it.\n", Assignment: &store.AssignmentRef{ID: 7}, InReplyTo: "messages/x.md"})
				return []store.ChatMessage{{Role: store.RoleReceived, Kind: "inbox_delivery", MessageRef: ref, Content: "text the model saw", TS: tsAt(3)}}
			},
			check: func(t *testing.T, rows []apitypes.TranscriptRow) {
				r := oneRow(t, rows, apitypes.TranscriptKindDelivery)
				if deref(t, "title", r.Title) != "Assigned #7" || deref(t, "body_md", r.BodyMD) != "Please take it." {
					t.Errorf("title/body = %q/%q", *r.Title, *r.BodyMD)
				}
				if deref(t, "kind_badge", r.KindBadge) != "assignment update" || deref(t, "message_type", r.MessageType) != "assignment_event" {
					t.Errorf("badge/type = %v/%v", *r.KindBadge, *r.MessageType)
				}
				if deref(t, "assignment_ref", r.AssignmentRef).ID != 7 {
					t.Errorf("assignment_ref = %+v", r.AssignmentRef)
				}
				if r.From.Slug != "bob" || r.From.Name != "Builder" || deref(t, "replies_to", r.RepliesTo).Slug != "bob" {
					t.Errorf("from/replies_to = %+v/%+v", r.From, r.RepliesTo)
				}
				if deref(t, "delivered_to", r.DeliveredTo).Slug != "alice" || len(deref(t, "also_to", r.AlsoTo)) != 0 {
					t.Errorf("delivered_to/also_to = %+v/%+v", r.DeliveredTo, r.AlsoTo)
				}
				if deref(t, "in_reply_to", r.InReplyTo) != "/messages/x.md" || deref(t, "raw_url", r.RawURL) == "" {
					t.Errorf("in_reply_to/raw = %v/%v", r.InReplyTo, r.RawURL)
				}
			},
		},
		{
			name: "notice to several agents",
			setup: func(t *testing.T, srv *Server) []store.ChatMessage {
				ref := writeMsg(t, srv, store.Message{Type: store.MsgNotice, Title: "Heads up", From: "bob", To: store.Recipients{"alice", "carol"}, Body: "b"})
				return []store.ChatMessage{{Role: store.RoleReceived, Kind: "inbox_delivery", MessageRef: ref}}
			},
			check: func(t *testing.T, rows []apitypes.TranscriptRow) {
				r := oneRow(t, rows, apitypes.TranscriptKindDelivery)
				also := deref(t, "also_to", r.AlsoTo)
				if *r.KindBadge != "notice" || len(also) != 1 || also[0] != (apitypes.PersonRef{Slug: "carol", Name: "Critic"}) || r.AssignmentRef != nil {
					t.Errorf("badge/also_to/assignment = %v/%+v/%v", *r.KindBadge, also, r.AssignmentRef)
				}
			},
		},
		{
			name: "delivery whose message is gone keeps the text the model saw",
			setup: func(*testing.T, *Server) []store.ChatMessage {
				return []store.ChatMessage{{Role: store.RoleReceived, Kind: "inbox_delivery", MessageRef: "messages/gone.md", Content: "what it read"}}
			},
			check: func(t *testing.T, rows []apitypes.TranscriptRow) {
				r := oneRow(t, rows, apitypes.TranscriptKindDelivery)
				if *r.BodyMD != "what it read" || *r.Title != "" || r.From != nil {
					t.Errorf("row = %+v", r)
				}
			},
		},
		{
			name: "published to you is a ceo_queue row",
			setup: func(t *testing.T, srv *Server) []store.ChatMessage {
				ref := writeMsg(t, srv, store.Message{Type: store.MsgCEONotification, Title: "Budget", From: "alice", To: store.Recipients{"ceo"}, Body: "We are over.\n"})
				return []store.ChatMessage{{Role: store.RoleSent, Kind: "doc_published", ToolUseID: "tu-p", MessageRef: ref, TS: tsAt(4)}}
			},
			check: func(t *testing.T, rows []apitypes.TranscriptRow) {
				r := oneRow(t, rows, apitypes.TranscriptKindCEOQueue)
				if *r.Title != "Budget" || *r.BodyMD != "We are over." || deref(t, "path", r.Path) == "" || deref(t, "resolved", r.Resolved) {
					t.Errorf("row = %+v", r)
				}
				if r.From.Slug != "alice" || *r.MessageType != "ceo_notification" {
					t.Errorf("from/type = %+v/%v", r.From, *r.MessageType)
				}
			},
		},
		{
			name: "published to you and answered",
			setup: func(t *testing.T, srv *Server) []store.ChatMessage {
				ref := writeMsg(t, srv, store.Message{Type: store.MsgCEOApprovalRequest, Title: "Hire", From: "alice", To: store.Recipients{"ceo"}})
				yes := true
				reply := writeMsg(t, srv, store.Message{Type: store.MsgCEOApprovalResponse, Title: "Re: Hire", From: "ceo", To: store.Recipients{"alice"}, InReplyTo: ref, Approved: &yes, Date: tsAt(9)})
				if err := srv.Store.CreateAgent(store.Agent{Slug: "ceo"}, ""); err != nil {
					t.Fatal(err)
				}
				if err := srv.Store.AppendChatMessage("ceo", store.ChatMessage{Role: store.RoleSent, Kind: "ceo_reply", MessageRef: reply, ReplyToMessageRef: ref}); err != nil {
					t.Fatal(err)
				}
				return []store.ChatMessage{{Role: store.RoleSent, Kind: "doc_published", MessageRef: ref}}
			},
			check: func(t *testing.T, rows []apitypes.TranscriptRow) {
				if r := oneRow(t, rows, apitypes.TranscriptKindCEOQueue); !deref(t, "resolved", r.Resolved) {
					t.Error("resolved = false after your reply")
				}
			},
		},
		{
			name: "published to you and answered by a reply that names it",
			setup: func(t *testing.T, srv *Server) []store.ChatMessage {
				ref := writeMsg(t, srv, store.Message{Type: store.MsgCEOApprovalRequest, Title: "Spend", From: "alice", To: store.Recipients{"ceo"}})
				if err := srv.Store.CreateAgent(store.Agent{Slug: "ceo"}, ""); err != nil {
					t.Fatal(err)
				}
				// The reply's own file is gone; the chat row still names
				// what it answered.
				if err := srv.Store.AppendChatMessage("ceo", store.ChatMessage{Role: store.RoleSent, Kind: "ceo_reply", MessageRef: "messages/gone.md", ReplyToMessageRef: ref}); err != nil {
					t.Fatal(err)
				}
				return []store.ChatMessage{{Role: store.RoleSent, Kind: "doc_published", MessageRef: ref}}
			},
			check: func(t *testing.T, rows []apitypes.TranscriptRow) {
				if r := oneRow(t, rows, apitypes.TranscriptKindCEOQueue); !deref(t, "resolved", r.Resolved) {
					t.Error("resolved = false after a reply that names the request")
				}
			},
		},
		{
			name: "published to a peer is a chip, and finishes its call",
			setup: func(t *testing.T, srv *Server) []store.ChatMessage {
				ref := writeMsg(t, srv, store.Message{Type: store.MsgNotice, Title: "FYI", From: "alice", To: store.Recipients{"bob"},
					Body: "The build is green.\n", Assignment: &store.AssignmentRef{ID: 7},
					Attachments: []store.MessageAttachment{{SHA: "nope", Name: "log.txt"}}})
				return []store.ChatMessage{
					{Role: store.RoleSent, Kind: "tool_use", ToolUseID: "tu-n", ToolName: "publish_notice", ToolInput: `{}`, TS: tsAt(5)},
					{Role: store.RoleSent, Kind: "doc_published", ToolUseID: "tu-n", MessageRef: ref, Content: "📄 notice", TS: tsAt(6)},
				}
			},
			check: func(t *testing.T, rows []apitypes.TranscriptRow) {
				if len(rows) != 2 {
					t.Fatalf("rows = %d, want the call and the chip", len(rows))
				}
				call, chip := rows[0], rows[1]
				if *call.Status != apitypes.ToolStatusDone || deref(t, "ended_ts", call.EndedTS) != msAt(6) {
					t.Errorf("call status/ended = %v/%v", *call.Status, call.EndedTS)
				}
				to := deref(t, "to", chip.To)
				if chip.Kind != apitypes.TranscriptKindDocPublished || *chip.Title != "FYI" || len(to) != 1 || to[0].Slug != "bob" || *chip.ToolUseID != "tu-n" {
					t.Errorf("chip = %+v", chip)
				}
				atts := deref(t, "attachments", chip.Attachments)
				if deref(t, "body_md", chip.BodyMD) != "The build is green." || deref(t, "message_type", chip.MessageType) != "notice" ||
					len(atts) != 1 || atts[0].Name != "log.txt" || atts[0].URL != "/attachments/nope" {
					t.Errorf("chip body/type/attachments = %v/%v/%+v", chip.BodyMD, chip.MessageType, atts)
				}
				if chip.AssignmentRef == nil || chip.AssignmentRef.ID != 7 {
					t.Errorf("assignment_ref = %+v, want #7", chip.AssignmentRef)
				}
				// The stored file is a raw link, never the row's page link.
				if raw := deref(t, "raw_url", chip.RawURL); !strings.HasPrefix(raw, "/messages/") || !strings.HasSuffix(raw, ".md") {
					t.Errorf("raw_url = %q", raw)
				}
			},
		},
		{
			name: "published to a peer whose file is gone keeps the title the agent gave",
			setup: func(*testing.T, *Server) []store.ChatMessage {
				return []store.ChatMessage{{Role: store.RoleSent, Kind: "doc_published", ToolUseID: "tu-g", MessageRef: "messages/gone.md", Content: "Weekly notes"}}
			},
			check: func(t *testing.T, rows []apitypes.TranscriptRow) {
				r := oneRow(t, rows, apitypes.TranscriptKindDocPublished)
				if *r.Title != "Weekly notes" || r.BodyMD != nil || r.MessageType != nil || r.AssignmentRef != nil ||
					len(deref(t, "to", r.To)) != 0 || len(deref(t, "attachments", r.Attachments)) != 0 || deref(t, "raw_url", r.RawURL) != "/messages/gone.md" {
					t.Errorf("row = %+v", r)
				}
			},
		},
		{
			name: "file shared",
			setup: func(*testing.T, *Server) []store.ChatMessage {
				return []store.ChatMessage{{Role: store.RoleSent, Kind: "file_shared", Content: "Shared file: a.md",
					Attachments: []store.MessageAttachment{{SHA: "nope", Name: "a.md"}}}}
			},
			check: func(t *testing.T, rows []apitypes.TranscriptRow) {
				r := oneRow(t, rows, apitypes.TranscriptKindFileShared)
				atts := deref(t, "attachments", r.Attachments)
				if *r.BodyMD != "Shared file: a.md" || r.From.Slug != "alice" || len(atts) != 1 || atts[0].URL != "/attachments/nope" || atts[0].SizeBytes != 0 {
					t.Errorf("row = %+v, attachments %+v", r, atts)
				}
			},
		},
		{
			name: "markers",
			setup: func(*testing.T, *Server) []store.ChatMessage {
				return []store.ChatMessage{
					{Role: store.RoleReceived, Kind: store.KindUserInterruption, Content: "User pressed Stop."},
					{Role: store.RoleReceived, Kind: store.KindRuntimeDisruption},
					{Role: store.RoleReceived, Kind: store.KindTurnError, Content: "usage limit"},
					{Role: store.RoleReceived, Kind: store.KindPausedToDeliver},
				}
			},
			check: func(t *testing.T, rows []apitypes.TranscriptRow) {
				want := []struct {
					kind apitypes.MarkerKind
					text string
				}{
					{apitypes.MarkerKindUserInterruption, "Chat interrupted by Stop"},
					{apitypes.MarkerKindRuntimeDisruption, "Interrupted by a runtime restart"},
					{apitypes.MarkerKindTurnError, "Stopped on an error"},
					{apitypes.MarkerKindPausedToDeliver, "Paused to deliver your message"},
				}
				if len(rows) != len(want) {
					t.Fatalf("rows = %d", len(rows))
				}
				for i, w := range want {
					r := rows[i]
					if r.Kind != apitypes.TranscriptKindMarker || *r.MarkerKind != w.kind || *r.Text != w.text {
						t.Errorf("row %d = %v %v %v, want %v %q", i, r.Kind, *r.MarkerKind, *r.Text, w.kind, w.text)
					}
				}
				if *rows[2].BodyMD != "usage limit" {
					t.Errorf("turn-error detail = %q", *rows[2].BodyMD)
				}
			},
		},
		{
			name: "wake note and rotation prompt",
			setup: func(*testing.T, *Server) []store.ChatMessage {
				return []store.ChatMessage{
					{Role: store.RoleReceived, Kind: store.KindWakeUpdate, Content: "since last"},
					{Role: store.RoleReceived, Kind: "rotation_prompt", Content: "reconcile"},
				}
			},
			check: func(t *testing.T, rows []apitypes.TranscriptRow) {
				want := []apitypes.TranscriptKind{apitypes.TranscriptKindWakeUpdate, apitypes.TranscriptKindRotationPrompt}
				bodies := []string{"since last", "reconcile"}
				if len(rows) != len(want) {
					t.Fatalf("rows = %d", len(rows))
				}
				for i := range want {
					if rows[i].Kind != want[i] || *rows[i].BodyMD != bodies[i] {
						t.Errorf("row %d = %v %q", i, rows[i].Kind, *rows[i].BodyMD)
					}
				}
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := transcriptServer(t)
			rows := srv.transcriptRows("alice", tc.setup(t, srv))
			apitypes.NoNilSlices(t, rows)
			tc.check(t, rows)
		})
	}
}

// oneRow is the only row, which must be of kind.
func oneRow(t *testing.T, rows []apitypes.TranscriptRow, kind apitypes.TranscriptKind) apitypes.TranscriptRow {
	t.Helper()
	if len(rows) != 1 {
		t.Fatalf("rows = %d, want 1: %+v", len(rows), rows)
	}
	if rows[0].Kind != kind {
		t.Fatalf("kind = %s, want %s", rows[0].Kind, kind)
	}
	return rows[0]
}

func TestTranscriptRowsNeverNil(t *testing.T) {
	srv := transcriptServer(t)
	if rows := srv.transcriptRows("alice", nil); rows == nil || len(rows) != 0 {
		t.Errorf("rows = %#v, want empty", rows)
	}
}

// A result folds into its call; a result with no call stands alone; a
// call with no result is running until the transcript moves past it.
func TestTranscriptToolPairing(t *testing.T) {
	use := func(id string, n int) store.ChatMessage {
		return store.ChatMessage{Role: store.RoleSent, Kind: "tool_use", ToolUseID: id, ToolName: "Bash", ToolInput: `{"command":"ls"}`, TS: tsAt(n)}
	}
	result := func(id string, n int, isErr bool) store.ChatMessage {
		return store.ChatMessage{Role: store.RoleReceived, Kind: "tool_result", ToolUseID: id, Content: "out-" + id, IsError: isErr, TS: tsAt(n)}
	}
	cases := []struct {
		name   string
		msgs   []store.ChatMessage
		status []apitypes.ToolStatus
	}{
		{"result folds in", []store.ChatMessage{use("a", 1), result("a", 2, false)}, []apitypes.ToolStatus{apitypes.ToolStatusDone}},
		{"error result", []store.ChatMessage{use("a", 1), result("a", 2, true)}, []apitypes.ToolStatus{apitypes.ToolStatusError}},
		{"parallel calls pair by id", []store.ChatMessage{use("a", 1), use("b", 2), result("b", 3, false), result("a", 4, false)},
			[]apitypes.ToolStatus{apitypes.ToolStatusDone, apitypes.ToolStatusDone}},
		{"no result yet", []store.ChatMessage{use("a", 1)}, []apitypes.ToolStatus{apitypes.ToolStatusRunning}},
		{"orphan result", []store.ChatMessage{result("z", 1, false)}, []apitypes.ToolStatus{apitypes.ToolStatusDone}},
		{"no result, then a reply", []store.ChatMessage{use("a", 1), {Role: store.RoleSent, Kind: "direct_chat", Content: "ok"}},
			[]apitypes.ToolStatus{apitypes.ToolStatusDone}},
		{"no result, then Stop", []store.ChatMessage{use("a", 1), {Role: store.RoleReceived, Kind: store.KindUserInterruption}},
			[]apitypes.ToolStatus{apitypes.ToolStatusError}},
		{"no result, then paused to deliver", []store.ChatMessage{use("a", 1), {Role: store.RoleReceived, Kind: store.KindPausedToDeliver}},
			[]apitypes.ToolStatus{apitypes.ToolStatusError}},
		{"no result, only plumbing after", []store.ChatMessage{use("a", 1), use("b", 2), result("b", 3, false)},
			[]apitypes.ToolStatus{apitypes.ToolStatusRunning, apitypes.ToolStatusDone}},
		// A message that lands mid-turn (a fold) says nothing about the
		// call: the tool is still running while the agent has it.
		{"no result, then a message delivered mid-turn", []store.ChatMessage{use("a", 1),
			{Role: store.RoleReceived, Kind: "direct_chat", Content: "while you work"},
			{Role: store.RoleReceived, Kind: store.KindSubagentResult, Content: "Background task ffff0000 (x) finished."},
			{Role: store.RoleReceived, Kind: "inbox_delivery", MessageRef: "messages/gone.md"}},
			[]apitypes.ToolStatus{apitypes.ToolStatusRunning}},
		{"no result, a fold, then the reply", []store.ChatMessage{use("a", 1),
			{Role: store.RoleReceived, Kind: "direct_chat", Content: "while you work"},
			{Role: store.RoleSent, Kind: "direct_chat", Content: "ok"}},
			[]apitypes.ToolStatus{apitypes.ToolStatusDone}},
		{"no result, then a shared file for another call", []store.ChatMessage{use("a", 1), use("b", 2), result("b", 3, false),
			{Role: store.RoleSent, Kind: "file_shared", Content: "Shared file: a.md"}},
			[]apitypes.ToolStatus{apitypes.ToolStatusRunning, apitypes.ToolStatusDone}},
		{"no result, then a rotation", []store.ChatMessage{use("a", 1), {Role: store.RoleReceived, Kind: "rotation_prompt", Content: "fold"}},
			[]apitypes.ToolStatus{apitypes.ToolStatusDone}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := transcriptServer(t)
			rows := srv.transcriptRows("alice", tc.msgs)
			var got []apitypes.ToolStatus
			for _, r := range rows {
				if r.Kind != apitypes.TranscriptKindToolUse {
					continue
				}
				got = append(got, *r.Status)
			}
			if tc.status == nil {
				tc.status = []apitypes.ToolStatus{apitypes.ToolStatusRunning}
			}
			if len(got) != len(tc.status) {
				t.Fatalf("tool rows = %v, want %v", got, tc.status)
			}
			for i := range got {
				if got[i] != tc.status[i] {
					t.Errorf("tool row %d = %s, want %s", i, got[i], tc.status[i])
				}
			}
		})
	}

	srv := transcriptServer(t)
	rows := srv.transcriptRows("alice", []store.ChatMessage{use("a", 1), result("a", 2, false)})
	r := rows[0]
	if *r.ToolUseID != "a" || *r.Name != "Bash" || *r.Input != `{"command":"ls"}` || deref(t, "output", r.Output) != "out-a" ||
		*r.StartedTS != msAt(1) || deref(t, "ended_ts", r.EndedTS) != msAt(2) {
		t.Errorf("paired row = %+v", r)
	}
	if running := srv.transcriptRows("alice", []store.ChatMessage{use("a", 1)})[0]; running.Output != nil || running.EndedTS != nil {
		t.Errorf("running row has output %v, ended %v", running.Output, running.EndedTS)
	}
}

// A batch reads each task's state off disk. Every result is a row
// where it landed, and its outcome folds into its task when the
// dispatch is in the transcript.
func TestTranscriptSubagents(t *testing.T) {
	srv := transcriptServer(t)
	writeSubagentMetaFile(t, srv, "aaaa1111", subagentMeta{ID: "aaaa1111", Description: "survey", Model: provider.MockModelRetired, Effort: "low", Status: "running"})
	writeSubagentMetaFile(t, srv, "bbbb2222", subagentMeta{ID: "bbbb2222", Description: "price", Status: "errored", Error: "pod went away"})
	writeSubagentMetaFile(t, srv, "cccc3333", subagentMeta{ID: "cccc3333", Description: "old", Status: "completed"})
	if err := srv.Store.AppendSubagentMessage("alice", "aaaa1111", store.ChatMessage{Role: store.RoleSent, Kind: "direct_chat", Content: "Three vendors."}); err != nil {
		t.Fatal(err)
	}
	input := `{"tasks":[{"description":"survey","prompt":"p","model":"mock-large-0","effort":"low"},{"description":"price","prompt":"q"},{"description":"third","prompt":"r"}]}`
	receipt := renderSubagentDispatch("alice", []SubagentTaskInput{{Description: "survey"}, {Description: "price"}}, []string{"aaaa1111", "bbbb2222"})
	msgs := []store.ChatMessage{
		{Role: store.RoleSent, Kind: "tool_use", ToolUseID: "tu-s", ToolName: mcp.SubagentToolName, ToolInput: input, TS: tsAt(1)},
		{Role: store.RoleReceived, Kind: "tool_result", ToolUseID: "tu-s", Content: receipt, TS: tsAt(2)},
		{Role: store.RoleSent, Kind: "direct_chat", Content: "Dispatched.", TS: tsAt(3)},
		{Role: store.RoleReceived, Kind: store.KindSubagentResult, Content: "Background task aaaa1111 (survey) finished.\n\nThree vendors.\n", TS: tsAt(4)},
		{Role: store.RoleReceived, Kind: store.KindSubagentResult, Content: "Background task cccc3333 (old) finished.\n\ndone\n", TS: tsAt(5)},
		{Role: store.RoleReceived, Kind: store.KindSubagentResult, Content: "A report that names no task.", TS: tsAt(6)},
	}
	rows := srv.transcriptRows("alice", msgs)
	apitypes.NoNilSlices(t, rows)
	kinds := []apitypes.TranscriptKind{}
	for _, r := range rows {
		kinds = append(kinds, r.Kind)
	}
	want := []apitypes.TranscriptKind{apitypes.TranscriptKindSubagents, apitypes.TranscriptKindMessage,
		apitypes.TranscriptKindTaskResult, apitypes.TranscriptKindTaskResult, apitypes.TranscriptKindTaskResult}
	if len(kinds) != len(want) {
		t.Fatalf("kinds = %v, want %v", kinds, want)
	}
	for i := range want {
		if kinds[i] != want[i] {
			t.Fatalf("kinds = %v, want %v", kinds, want)
		}
	}

	batch := rows[0]
	if deref(t, "tool_use_id", batch.ToolUseID) != "tu-s" || batch.TS != msAt(1) {
		t.Errorf("batch = %+v", batch)
	}
	tasks := deref(t, "tasks", batch.Tasks)
	if len(tasks) != 3 {
		t.Fatalf("tasks = %+v", tasks)
	}
	survey, price, third := tasks[0], tasks[1], tasks[2]
	if survey.ID != "aaaa1111" || survey.State != apitypes.SubagentTaskStateDone || deref(t, "output_md", survey.OutputMD) != "Three vendors." ||
		survey.Model != "Mock Large 0" || survey.Effort != "low" ||
		deref(t, "url", survey.URL) != "/api/v1/agents/alice/subagents/aaaa1111" {
		t.Errorf("survey (folded from its result) = %+v", survey)
	}
	if price.State != apitypes.SubagentTaskStateErrored || deref(t, "error", price.Error) != "pod went away" || price.Index != 1 {
		t.Errorf("price = %+v", price)
	}
	if third.ID != "" || third.State != apitypes.SubagentTaskStateRunning || third.URL != nil || third.Index != 2 {
		t.Errorf("third (no id yet) = %+v", third)
	}

	// The result is a row of its own at the time it landed, carrying the
	// task as it ended: what the batch's entry now shows, at index 0.
	landed := rows[2]
	landedTasks := deref(t, "tasks", landed.Tasks)
	if landed.TS != msAt(4) || landed.ToolUseID != nil || landed.BodyMD != nil || len(landedTasks) != 1 {
		t.Fatalf("result row = %+v %+v", landed, landedTasks)
	}
	if got := landedTasks[0]; got.Index != 0 || got.ID != "aaaa1111" || got.Title != "survey" || got.State != apitypes.SubagentTaskStateDone ||
		deref(t, "output_md", got.OutputMD) != "Three vendors." || got.Model != survey.Model || got.Effort != "low" ||
		deref(t, "url", got.URL) != "/api/v1/agents/alice/subagents/aaaa1111" {
		t.Errorf("result row's task = %+v", got)
	}

	stray := rows[3]
	strayTasks := deref(t, "tasks", stray.Tasks)
	if stray.TS != msAt(5) || len(strayTasks) != 1 || strayTasks[0].ID != "cccc3333" || strayTasks[0].State != apitypes.SubagentTaskStateDone || strayTasks[0].Title != "old" {
		t.Errorf("stray result row = %+v %+v", stray, strayTasks)
	}

	unnamed := rows[4]
	if len(deref(t, "tasks", unnamed.Tasks)) != 0 || deref(t, "body_md", unnamed.BodyMD) != "A report that names no task." {
		t.Errorf("a report that names no task = %+v", unnamed)
	}
}

// A failed task's report reads as errored whatever meta.json says, and
// carries the reason the task recorded.
func TestTranscriptTaskResultFailed(t *testing.T) {
	srv := transcriptServer(t)
	writeSubagentMetaFile(t, srv, "bbbb2222", subagentMeta{ID: "bbbb2222", Description: "price", Status: "running", Error: "pod went away"})
	rows := srv.transcriptRows("alice", []store.ChatMessage{
		{Role: store.RoleReceived, Kind: store.KindSubagentResult, Content: "Background task bbbb2222 (price) FAILED.\n\nerror: pod went away\n", TS: tsAt(1)},
	})
	task := deref(t, "tasks", oneRow(t, rows, apitypes.TranscriptKindTaskResult).Tasks)[0]
	if task.State != apitypes.SubagentTaskStateErrored || deref(t, "error", task.Error) != "pod went away" || task.OutputMD != nil {
		t.Errorf("failed task = %+v", task)
	}
}

// A task's own transcript: its replies are from the task, its prompt
// from the agent that dispatched it.
func TestSubagentTranscriptRowsParties(t *testing.T) {
	srv := transcriptServer(t)
	rows := srv.subagentTranscriptRows("alice", "aaaa1111", "survey", []store.ChatMessage{
		{Role: store.RoleReceived, Kind: "direct_chat", Content: "go"},
		{Role: store.RoleSent, Kind: "direct_chat", Content: "done"},
	})
	if len(rows) != 2 {
		t.Fatalf("rows = %d", len(rows))
	}
	if p := *rows[0].From; p.Slug != "alice" || p.Kind != apitypes.PartyKindAgent || *rows[0].Role != apitypes.MessageRoleReceived {
		t.Errorf("prompt from = %+v, role %v", p, *rows[0].Role)
	}
	if p := *rows[1].From; p.Slug != "aaaa1111" || p.Name != "survey" {
		t.Errorf("reply from = %+v", p)
	}
}
