package messaging

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kivali-ai/kivali/internal/store"
)

// writeNotice persists a notice addressed to several agents and
// returns its store-relative path. Routing is left to the caller so a
// test can exercise RouteProduced directly.
func writeNotice(t *testing.T, m *Messenger, from string, to ...string) string {
	t.Helper()
	abs, err := m.Store.WriteMessage(store.Message{
		Type:  store.MsgNotice,
		Title: "Pricing changed",
		From:  from,
		To:    store.Recipients(to),
		Date:  time.Now().UTC(),
		Body:  "List price moved to $49.",
	})
	if err != nil {
		t.Fatalf("write notice: %v", err)
	}
	rel, err := filepath.Rel(m.Store.Root(), abs)
	if err != nil {
		t.Fatalf("relpath: %v", err)
	}
	return rel
}

func inboxOf(t *testing.T, m *Messenger, slug string) []string {
	t.Helper()
	ts, err := m.Store.ReadMessageQueue()
	if err != nil {
		t.Fatalf("read queue: %v", err)
	}
	return ts.Agents[slug].Inbox
}

// routeNotice writes a notice and routes it, returning its relPath.
func routeNotice(t *testing.T, m *Messenger, from string, to ...string) string {
	t.Helper()
	rel := writeNotice(t, m, from, to...)
	msg, err := m.Store.ReadMessage(filepath.Join(m.Store.Root(), rel))
	if err != nil {
		t.Fatalf("read back notice: %v", err)
	}
	msg.Path = filepath.Join(m.Store.Root(), rel)
	if _, err := m.Route(context.Background(), msg); err != nil {
		t.Fatalf("route: %v", err)
	}
	return rel
}

// TestNoticeFansOutOnePointerPerRecipient: one ledger file, one inbox
// pointer per addressee. That split is what makes per-recipient
// moderation possible at all.
func TestNoticeFansOutOnePointerPerRecipient(t *testing.T) {
	m := newTestMessenger(t)
	for _, slug := range []string{"chief-of-staff", "alice", "bob", "carol"} {
		mustCreateAgent(t, m.Store, slug)
	}

	rel := routeNotice(t, m, "chief-of-staff", "alice", "bob", "carol")

	for _, slug := range []string{"alice", "bob", "carol"} {
		got := inboxOf(t, m, slug)
		if len(got) != 1 || got[0] != rel {
			t.Errorf("%s inbox = %v, want [%s]", slug, got, rel)
		}
	}
	msgs, err := m.Store.ListMessages(store.MessageFilter{Type: store.MsgNotice})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(msgs) != 1 {
		t.Errorf("a notice to three agents must be ONE message on disk; got %d", len(msgs))
	}
	if len(msgs[0].To) != 3 {
		t.Errorf("to = %v, want three recipients", msgs[0].To)
	}
}

// TestNoticeReleasesToEveryRecipientAtOnce: the CEO makes one decision
// about a notice and every addressee gets it. One card in the inbox,
// one release, three deliveries — each carrying the full body.
func TestNoticeReleasesToEveryRecipientAtOnce(t *testing.T) {
	m := newTestMessenger(t)
	for _, slug := range []string{"chief-of-staff", "alice", "bob", "carol"} {
		mustCreateAgent(t, m.Store, slug)
	}
	rec := &recordingHooks{store: m.Store}
	rel := routeNotice(t, m, "chief-of-staff", "alice", "bob", "carol")

	delivered, notFound, warnings, err := m.ReleaseSelected(
		context.Background(),
		[]string{rel},
		nil,
		rec.hooks(),
	)
	if err != nil {
		t.Fatalf("ReleaseSelected: %v", err)
	}
	if len(warnings) != 0 || len(notFound) != 0 {
		t.Errorf("unexpected warnings=%v notFound=%v", warnings, notFound)
	}
	// One message asked about, one message reported — not one entry
	// per delivery, or the CEO's UI would think three cards vanished.
	if len(delivered) != 1 || delivered[0] != rel {
		t.Errorf("delivered = %v, want exactly [%s]", delivered, rel)
	}

	for _, slug := range []string{"alice", "bob", "carol"} {
		if q := inboxOf(t, m, slug); len(q) != 0 {
			t.Errorf("%s inbox should be drained; got %v", slug, q)
		}
		hist, _ := m.Store.ReadChatHistory(slug)
		if len(hist) != 1 {
			t.Fatalf("%s should have exactly one delivery; got %d entries", slug, len(hist))
		}
		if !strings.Contains(hist[0].Content, "List price moved to $49.") {
			t.Errorf("%s delivery should carry the full body; got %q", slug, hist[0].Content)
		}
	}
}

// TestNoticeBounceCancelsTheWholeMessage: bouncing a notice cancels it
// for everyone and returns one message to the sender naming all of
// them. A bounce that left some recipients still queued would tell the
// sender their message didn't go through while it was on its way.
func TestNoticeBounceCancelsTheWholeMessage(t *testing.T) {
	m := newTestMessenger(t)
	for _, slug := range []string{"chief-of-staff", "alice", "bob"} {
		mustCreateAgent(t, m.Store, slug)
	}
	rec := &recordingHooks{store: m.Store}
	rel := routeNotice(t, m, "chief-of-staff", "alice", "bob")

	if err := m.Bounce(context.Background(), rel, "pricing isn't theirs to track", rec.hooks()); err != nil {
		t.Fatalf("Bounce: %v", err)
	}
	for _, slug := range []string{"alice", "bob"} {
		if q := inboxOf(t, m, slug); len(q) != 0 {
			t.Errorf("%s's pointer should be gone after a bounce; got %v", slug, q)
		}
		if h, _ := m.Store.ReadChatHistory(slug); len(h) != 0 {
			t.Errorf("%s must not have received a bounced notice; got %+v", slug, h)
		}
	}
	hist, _ := m.Store.ReadChatHistory("chief-of-staff")
	if len(hist) != 1 {
		t.Fatalf("sender should have received exactly one bounce; got %d entries", len(hist))
	}
	body := hist[0].Content
	if !strings.Contains(body, "Your notice to alice, bob did not go through") {
		t.Errorf("bounce should name every cancelled recipient; got %q", body)
	}
	// The bounce is itself a tell, not an answer: nothing may name a
	// notice as its reply target, server-authored messages included.
	bounces, err := m.Store.ListMessages(store.MessageFilter{From: "ceo"})
	if err != nil {
		t.Fatalf("list ceo messages: %v", err)
	}
	if len(bounces) != 1 {
		t.Fatalf("want one CEO-authored bounce; got %d", len(bounces))
	}
	if bounces[0].Type != store.MsgNotice {
		t.Errorf("bounce of a notice should itself be a notice; got %s", bounces[0].Type)
	}
	if bounces[0].InReplyTo != "" {
		t.Errorf("nothing may point at a notice; bounce in_reply_to = %q", bounces[0].InReplyTo)
	}
}

// TestNoticeDeadLettersOnlyTheMissingRecipient: an archived addressee
// dead-letters on its own; the reachable ones still get queued.
func TestNoticeDeadLettersOnlyTheMissingRecipient(t *testing.T) {
	m := newTestMessenger(t)
	for _, slug := range []string{"chief-of-staff", "alice"} {
		mustCreateAgent(t, m.Store, slug)
	}

	rel := routeNotice(t, m, "chief-of-staff", "alice", "ghost")

	if q := inboxOf(t, m, "alice"); len(q) != 1 || q[0] != rel {
		t.Errorf("alice should still be queued; got %v", q)
	}
	// The sender gets a dead-letter naming the unreachable slug.
	sender := inboxOf(t, m, "chief-of-staff")
	if len(sender) != 1 {
		t.Fatalf("sender should have one dead-letter queued; got %v", sender)
	}
	dl, err := m.Store.ReadMessage(filepath.Join(m.Store.Root(), sender[0]))
	if err != nil {
		t.Fatalf("read dead-letter: %v", err)
	}
	if !strings.Contains(dl.Body, "ghost") {
		t.Errorf("dead-letter should name the missing recipient; got %q", dl.Body)
	}
	if strings.Contains(dl.Body, `to "alice"`) {
		t.Errorf("dead-letter must not implicate the reachable recipient; got %q", dl.Body)
	}
	if dl.Type != store.MsgNotice || dl.InReplyTo != "" {
		t.Errorf("dead-letter for a notice must be a parentless notice; got type=%s in_reply_to=%q", dl.Type, dl.InReplyTo)
	}
}
