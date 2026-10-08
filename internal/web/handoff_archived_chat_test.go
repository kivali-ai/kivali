package web

import (
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kivali-ai/kivali/internal/store"
	"github.com/kivali-ai/kivali/internal/web/apitypes"
)

// TestArchivedChatRendersInboxDelivery: a past chat holding an
// inbox_delivery entry resolves that delivery to its message (title
// and sender), exactly as the live chat does. It began as the fix for
// a past-chat viewer that crashed on the first inbox delivery because
// the archived view never loaded the inbox views the live one did; the
// past-chat API shares the live chat's transcript builder.
func TestArchivedChatRendersInboxDelivery(t *testing.T) {
	srv := newTestServer(t)
	if err := srv.Store.CreateAgent(store.Agent{Slug: "alice", Role: "Analyst", ReportsTo: "chief-of-staff"}, "# role"); err != nil {
		t.Fatalf("seed agent: %v", err)
	}

	// Write a real inbox message to /data/messages/ so the chat
	// entry's MessageRef resolves cleanly — exercises the happy path
	// where loadInboxViews returns a populated map. (Unresolvable
	// refs fall back to zero-value views, which the transcript also
	// tolerates; this test covers the case that breaks when
	// InboxViews is missing entirely.)
	msgAbs, err := srv.Store.WriteMessage(store.Message{
		Type:  store.MsgNotice,
		Title: "draft Q3 memo",
		From:  "chief-of-staff",
		To:    store.Recipients{"alice"},
		Date:  time.Now().UTC(),
		Body:  "please write the Q3 memo by Friday",
	})
	if err != nil {
		t.Fatalf("write msg: %v", err)
	}
	msgRef, _ := filepath.Rel(srv.Store.Root(), msgAbs)

	for _, m := range []store.ChatMessage{
		{Role: store.RoleReceived, Content: "hi"},
		{Role: store.RoleSent, Content: "hello"},
		{
			Role:       store.RoleReceived,
			Content:    "[INBOX task_request from=chief-of-staff] draft Q3 memo",
			Kind:       "inbox_delivery",
			MessageRef: msgRef,
		},
		{Role: store.RoleSent, Content: "on it"},
	} {
		if err := srv.Store.AppendChatMessage("alice", m); err != nil {
			t.Fatalf("append: %v", err)
		}
	}

	ts, err := srv.Store.ArchiveChat("alice")
	if err != nil {
		t.Fatalf("archive: %v", err)
	}

	rr := apiDo(t, srv, http.MethodGet, "/api/v1/agents/alice/chats/"+ts, "", nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("code = %d, body = %s", rr.Code, rr.Body.String())
	}
	d := decodeAPI[apitypes.PastChatDetail](t, rr)
	// The delivery's title comes from the Message, not from the raw
	// chat.jsonl content, so finding it proves the view was resolved.
	var sawTitle bool
	for _, row := range d.Rows {
		if row.Title != nil && *row.Title == "draft Q3 memo" {
			sawTitle = true
		}
	}
	if !sawTitle {
		t.Errorf("no row carries the resolved delivery title; rows = %+v", d.Rows)
	}
	body := rr.Body.String()
	for _, want := range []string{"chief-of-staff", "hello", "on it"} {
		if !strings.Contains(body, want) {
			t.Errorf("archived chat missing %q", want)
		}
	}
}
