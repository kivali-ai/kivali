package agent

import (
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	"github.com/kivali-ai/kivali/internal/provider"
	"github.com/kivali-ai/kivali/internal/store"
)

// TestParseMCPPublishAck pins the regex-shaped ack parser for the
// doc_published event synthesis. If the MCP server (internal/mcp/
// publish_tools.go) ever changes its ack format, this test breaks
// loudly — preventing a silent UX regression where the "📄"
// notice stops carrying its path.
func TestParseMCPPublishAck(t *testing.T) {
	cases := []struct {
		name     string
		toolName string
		input    string
		ack      string
		want     mcpPublishAck
	}{
		{
			name:     "agent_role — canonical ack",
			toolName: "publish_agent_role",
			input:    `{"title":"Role: Chief Scientist","to":"chief-scientist","slug":"chief-scientist","body":"hi"}`,
			ack:      `publish_agent_role published: "Role: Chief Scientist" → chief-scientist (saved to /data/messages/2026-04-18/20260421T120000Z-agent_role-chief-of-staff--to--chief-scientist.md)`,
			want: mcpPublishAck{
				MessageType: "agent_role",
				Title:       "Role: Chief Scientist",
				To:          "chief-scientist",
				Path:        "/data/messages/2026-04-18/20260421T120000Z-agent_role-chief-of-staff--to--chief-scientist.md",
			},
		},
		{
			name:     "notice — doc_type is not a 1:1 of tool name strip",
			toolName: "publish_notice",
			input:    `{"title":"Pricing changed","to":["chief-scientist"],"body":"fyi"}`,
			ack:      `publish_notice published: "Pricing changed" → chief-scientist (saved to messages/2026-04-18/x.md)`,
			want: mcpPublishAck{
				MessageType: "notice",
				Title:       "Pricing changed",
				To:          "chief-scientist",
				Path:        "messages/2026-04-18/x.md",
			},
		},
		{
			name:     "ceo_notification with paren in the title",
			toolName: "publish_ceo_notification",
			input:    `{"title":"Review (urgent)","body":"."}`,
			// The regex uses LastIndex for "(saved to " so a literal
			// paren in the title shouldn't confuse it.
			ack: `publish_ceo_notification published: "Review (urgent)" → ceo (saved to messages/2026-04-18/r.md)`,
			want: mcpPublishAck{
				MessageType: "ceo_notification",
				Title:       "Review (urgent)",
				To:          "ceo",
				Path:        "messages/2026-04-18/r.md",
			},
		},
		{
			name:     "ack missing the path suffix — partial degrade, other fields still populated",
			toolName: "publish_ceo_notification",
			input:    `{"title":"Weekly","body":"."}`,
			ack:      `publish_ceo_notification published: "Weekly" → ceo`,
			want: mcpPublishAck{
				MessageType: "ceo_notification",
				Title:       "Weekly",
				To:          "", // no `to` in the input and no path suffix to anchor the ack's recipient on
				Path:        "", // couldn't parse — leave empty, don't crash
			},
		},
		{
			name:     "tool input is nil — Title defaults empty, To falls back to ack value",
			toolName: "publish_agent_role",
			input:    ``,
			ack:      `publish_agent_role published: "??" → ?? (saved to nowhere.md)`,
			want: mcpPublishAck{
				MessageType: "agent_role",
				Title:       "",
				To:          "??",
				Path:        "nowhere.md",
			},
		},
		{
			name:     "tool name without publish_ prefix — passed through as-is",
			toolName: "file_create",
			input:    `{"title":"x","to":"y"}`,
			ack:      `whatever (saved to z.md)`,
			want: mcpPublishAck{
				MessageType: "file_create", // TrimPrefix is a no-op
				Title:       "x",
				To:          "y",
				Path:        "z.md",
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := parseMCPPublishAck(tc.toolName, json.RawMessage(tc.input), tc.ack)
			if got != tc.want {
				t.Errorf("parseMCPPublishAck mismatch\n  got  %+v\n  want %+v", got, tc.want)
			}
		})
	}
}

// recordingEmitter keeps every Emit for assertions.
type recordingEmitter struct {
	kinds    []string
	payloads []map[string]any
}

func (e *recordingEmitter) Emit(kind string, payload any) {
	e.kinds = append(e.kinds, kind)
	m, _ := payload.(map[string]any)
	e.payloads = append(e.payloads, m)
}
func (e *recordingEmitter) Checkpoint() {}
func (e *recordingEmitter) TrimDeltas() {}

// The synthetic chips carry ts (unix ms, as chat_message does), and
// doc_published's row is stamped with the same instant.
func TestSyntheticChipsCarryTS(t *testing.T) {
	now := time.Date(2026, 9, 28, 14, 3, 0, 0, time.UTC)
	st, err := store.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := st.CreateAgent(store.Agent{Slug: "alice", Role: "Eng", ReportsTo: "ceo"}, "k"); err != nil {
		t.Fatal(err)
	}
	r := &Runtime{Store: st}
	em := &recordingEmitter{}
	r.EmitDocPublishedFromMCPAck("alice", em, provider.StreamEvent{
		ToolUseID: "tu-1", ToolName: "publish_notice",
		ToolInput: json.RawMessage(`{"title":"Pricing","to":"bob"}`),
	}, `publish_notice published: "Pricing" → bob (saved to messages/x.md)`, now)
	EmitFileSharedFromMCPAck(em, provider.StreamEvent{ToolUseID: "tu-2", ToolName: ShareFileToolName},
		"Shared in this conversation:\n- plan.md (sha=abc)", now)

	if len(em.kinds) != 2 || em.kinds[0] != "doc_published" || em.kinds[1] != "file_shared" {
		t.Fatalf("emits = %v, want doc_published, file_shared", em.kinds)
	}
	for i, p := range em.payloads {
		if p["ts"] != now.UnixMilli() {
			t.Errorf("%s ts = %v, want %d", em.kinds[i], p["ts"], now.UnixMilli())
		}
	}
	hist, err := st.ReadChatHistory("alice")
	if err != nil || len(hist) != 1 || !hist[0].TS.Equal(now) {
		t.Fatalf("doc_published row = %+v (err %v), want one row at %v", hist, err, now)
	}
}

// The live doc_published event carries the published message's body,
// files and assignment, so the live sent row opens to the same card the
// transcript draws after a reload. A path that cannot be read leaves
// them out.
func TestDocPublishedCarriesTheMessage(t *testing.T) {
	now := time.Date(2026, 9, 28, 14, 3, 0, 0, time.UTC)
	st, err := store.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := st.CreateAgent(store.Agent{Slug: "alice", Role: "Eng", ReportsTo: "ceo"}, "k"); err != nil {
		t.Fatal(err)
	}
	abs, err := st.WriteMessage(store.Message{
		Type: store.MsgNotice, Title: "FYI", From: "alice", To: store.Recipients{"bob"}, Date: now,
		Body: "The build is green.\n", Assignment: &store.AssignmentRef{ID: 7},
		Attachments: []store.MessageAttachment{{SHA: "abc123", Name: "log.txt"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	rel, err := filepath.Rel(st.Root(), abs)
	if err != nil {
		t.Fatal(err)
	}
	r := &Runtime{Store: st}
	em := &recordingEmitter{}
	tu := provider.StreamEvent{ToolUseID: "tu-1", ToolName: "publish_notice", ToolInput: json.RawMessage(`{"title":"FYI","to":"bob"}`)}
	r.EmitDocPublishedFromMCPAck("alice", em, tu, `publish_notice published: "FYI" → bob (saved to `+rel+`)`, now)
	r.EmitDocPublishedFromMCPAck("alice", em, tu, `publish_notice published: "FYI" → bob (saved to messages/gone.md)`, now)

	if len(em.payloads) != 2 {
		t.Fatalf("emits = %v", em.kinds)
	}
	p := em.payloads[0]
	if p["doc_body"] != "The build is green." || p["doc_path"] != rel || p["doc_assignment"] != 7 {
		t.Errorf("doc_body/doc_path/doc_assignment = %v/%v/%v", p["doc_body"], p["doc_path"], p["doc_assignment"])
	}
	// The recipients travel as the one string the publish call wrote;
	// web/src/state/transcript.test.ts feeds the reducer the same shape.
	if to, ok := p["doc_to"].(string); !ok || to != "bob" {
		t.Errorf("doc_to = %#v, want the string \"bob\"", p["doc_to"])
	}
	files, _ := p["doc_files"].([]map[string]any)
	if len(files) != 1 || files[0]["sha"] != "abc123" || files[0]["name"] != "log.txt" {
		t.Errorf("doc_files = %+v", p["doc_files"])
	}
	gone := em.payloads[1]
	for _, k := range []string{"doc_body", "doc_files", "doc_assignment"} {
		if _, ok := gone[k]; ok {
			t.Errorf("unreadable message: %s = %v, want absent", k, gone[k])
		}
	}
}
