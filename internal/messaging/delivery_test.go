package messaging

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kivali-ai/kivali/internal/agent"
	"github.com/kivali-ai/kivali/internal/store"
)

// recordingHooks captures DeliveryHooks invocations: every wake call
// against WakeAgent appends to .calls so tests can assert on them.
// DeliverToAgent writes directly to the store (the test harness
// has no real chat-hub busy-state to gate on).
//
// WakeAgent runs SyncAgentFilesystem before recording the call. In
// production the attachments farm is kept by the edge that lands a
// row (AppendChatMessage); the messaging package triggers no syncs of
// its own, so this test wiring is what makes post-delivery filesystem
// state visible.
type recordingHooks struct {
	store *store.FSStore
	calls []string
}

func (r *recordingHooks) hooks() DeliveryHooks {
	return DeliveryHooks{
		DeliverToAgent: func(slug string, msg store.ChatMessage) error {
			return r.store.AppendChatMessage(slug, msg)
		},
		WakeAgent: func(slug string) bool {
			_ = r.store.SyncAgentFilesystem(slug)
			r.calls = append(r.calls, slug)
			return true
		},
	}
}

func newTestMessenger(t *testing.T) *Messenger {
	t.Helper()
	s, err := store.New(t.TempDir())
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	rt := agent.NewRuntime(s, agent.RuntimeDefaults{})
	return New(s, rt)
}

func mustCreateAgent(t *testing.T, s *store.FSStore, slug string) {
	t.Helper()
	if err := s.CreateAgent(store.Agent{Slug: slug, Role: "R", ReportsTo: "ceo"}, "seed"); err != nil {
		t.Fatalf("CreateAgent(%s): %v", slug, err)
	}
}

// Pure ceo_notification_ack receipts — no body, no attachments — must
// not wake the recipient's chat loop. Waking on a pure receipt drives
// the model to re-interpret earlier context and confabulate work.
func TestDeliverToAgentSkipsWakeOnPureAck(t *testing.T) {
	m := newTestMessenger(t)
	mustCreateAgent(t, m.Store, "bob")
	rec := &recordingHooks{store: m.Store}

	msg := store.Message{
		Type:  store.MsgCEONotificationAck,
		Title: "Acknowledged: FYI X",
		From:  "ceo",
		To:    store.Recipients{"bob"},
		Body:  "",
	}
	if !m.DeliverToAgent(context.Background(), "bob", msg, "messages/2026-04-18/ack.md", rec.hooks()) {
		t.Fatal("delivery should succeed")
	}
	if len(rec.calls) != 0 {
		t.Errorf("pure ack should not wake chat loop; got calls=%v", rec.calls)
	}
}

// Acks with a body or attachment DO wake the recipient — the CEO may
// be relaying something actionable (a Cowork response, a note the
// agent should act on).
func TestDeliverToAgentWakesOnAckWithBody(t *testing.T) {
	m := newTestMessenger(t)
	mustCreateAgent(t, m.Store, "bob")
	rec := &recordingHooks{store: m.Store}

	msg := store.Message{
		Type:  store.MsgCEONotificationAck,
		Title: "Acknowledged: FYI X",
		From:  "ceo",
		To:    store.Recipients{"bob"},
		Body:  "saw it; please also check Y",
	}
	_ = m.DeliverToAgent(context.Background(), "bob", msg, "messages/2026-04-18/ack.md", rec.hooks())
	if len(rec.calls) != 1 || rec.calls[0] != "bob" {
		t.Errorf("ack with body should wake chat loop; got calls=%v", rec.calls)
	}
}

func TestDeliverToAgentWakesOnAckWithAttachment(t *testing.T) {
	m := newTestMessenger(t)
	mustCreateAgent(t, m.Store, "bob")
	rec := &recordingHooks{store: m.Store}

	msg := store.Message{
		Type:        store.MsgCEONotificationAck,
		Title:       "Acknowledged: FYI X",
		From:        "ceo",
		To:          store.Recipients{"bob"},
		Attachments: []store.MessageAttachment{{Name: "cowork-output.md", SHA: "abc"}},
	}
	_ = m.DeliverToAgent(context.Background(), "bob", msg, "messages/2026-04-18/ack.md", rec.hooks())
	if len(rec.calls) != 1 || rec.calls[0] != "bob" {
		t.Errorf("ack with attachment should wake chat loop; got calls=%v", rec.calls)
	}
}

// CEO-attached files on approve/deny must appear in the recipient's
// /files/attachments/ view by the time the recipient next runs an
// inference. This test exercises that end-state through
// DeliverToAgent → WakeAgent (which the test hook wires to
// SyncAgentFilesystem).
func TestDeliverToAgentSyncsMemoryOnAttachment(t *testing.T) {
	m := newTestMessenger(t)
	mustCreateAgent(t, m.Store, "bob")

	// Seed a real attachment blob so the sync has something to link.
	att, err := m.Store.AddAttachment(context.Background(), "notes.md", strings.NewReader("hello"))
	if err != nil {
		t.Fatalf("seed attachment: %v", err)
	}

	msg := store.Message{
		Type:        store.MsgCEOApprovalResponse,
		Title:       "Denied: X",
		From:        "ceo",
		To:          store.Recipients{"bob"},
		Body:        "see attached",
		Attachments: []store.MessageAttachment{{SHA: att.SHA, Name: "notes.md"}},
	}
	rec := &recordingHooks{store: m.Store}
	_ = m.DeliverToAgent(context.Background(), "bob", msg, "messages/2026-04-18/resp.md", rec.hooks())

	// /files/attachments/notes.md (or similar) must now be
	// reachable from bob's memory view.
	memRoot := filepath.Join(m.Store.Root(), "agents", "bob", "memory", "attachments")
	entries, err := os.ReadDir(memRoot)
	if err != nil {
		t.Fatalf("memory/attachments dir should exist: %v", err)
	}
	var found bool
	for _, e := range entries {
		if strings.Contains(e.Name(), "notes") {
			found = true
			break
		}
	}
	if !found {
		var names []string
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Errorf("recipient's /files/attachments/ missing the linked file; entries=%v", names)
	}
}

// Approval responses (approve/deny) always wake the recipient even
// with an empty body — the decision is itself the action signal
// (e.g. CoS proceeds after an approve). The server provisions on
// its own for hire approvals; other approvals are pure signal.
func TestDeliverToAgentWakesOnApprovalResponseEmptyBody(t *testing.T) {
	m := newTestMessenger(t)
	mustCreateAgent(t, m.Store, "bob")
	rec := &recordingHooks{store: m.Store}

	approved := true
	msg := store.Message{
		Type:     store.MsgCEOApprovalResponse,
		Title:    "Approved: Hire X",
		From:     "ceo",
		To:       store.Recipients{"bob"},
		Approved: &approved,
	}
	_ = m.DeliverToAgent(context.Background(), "bob", msg, "messages/2026-04-18/resp.md", rec.hooks())
	if len(rec.calls) != 1 || rec.calls[0] != "bob" {
		t.Errorf("approval response should wake chat loop; got calls=%v", rec.calls)
	}
}
