package web

import (
	"encoding/json"
	"testing"

	"github.com/kivali-ai/kivali/internal/store"
)

// TestOrgSnapshotIncludesMessagesTotal: the web app uses this number
// as its "should I refetch the history?" trigger. Must reflect
// every message file on disk (not just pending or any filtered
// subset) so brand-new mid-release deliveries register.
func TestOrgSnapshotIncludesMessagesTotal(t *testing.T) {
	srv := newTestServer(t)
	if err := srv.Store.CreateAgent(store.Agent{Slug: "chief-of-staff", Role: "CoS", ReportsTo: "ceo"}, "k"); err != nil {
		t.Fatalf("seed cos: %v", err)
	}
	if err := srv.Store.CreateAgent(store.Agent{Slug: "alice", Role: "Analyst", ReportsTo: "chief-of-staff"}, "k"); err != nil {
		t.Fatalf("seed alice: %v", err)
	}
	// Snapshot baseline — there may be CoS hire messages from seeding.
	var before orgSnapshot
	if err := json.Unmarshal(srv.buildOrgSnapshot(), &before); err != nil {
		t.Fatalf("decode before: %v", err)
	}
	// Write a new message; the count should rise by exactly 1.
	msg := store.Message{Type: store.MsgNotice, Title: "T", From: "alice", To: store.Recipients{"chief-of-staff"}, Body: "b"}
	if _, err := srv.Store.WriteMessage(msg); err != nil {
		t.Fatalf("write: %v", err)
	}
	var after orgSnapshot
	if err := json.Unmarshal(srv.buildOrgSnapshot(), &after); err != nil {
		t.Fatalf("decode after: %v", err)
	}
	if after.MessagesTotal != before.MessagesTotal+1 {
		t.Errorf("messages_total = %d, want %d (before=%d)", after.MessagesTotal, before.MessagesTotal+1, before.MessagesTotal)
	}
}
