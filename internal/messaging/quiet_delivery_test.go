package messaging

import (
	"context"
	"testing"
	"time"

	"github.com/kivali-ai/kivali/internal/store"
)

func noticeTo(from, to string, quiet bool) store.Message {
	return store.Message{
		Type:  store.MsgNotice,
		Title: "FYI",
		From:  from,
		To:    store.Recipients{to},
		Date:  time.Date(2026, 9, 22, 10, 0, 0, 0, time.UTC),
		Body:  "For when you next look.",
		Quiet: quiet,
	}
}

// A message that declares itself quiet lands in the transcript without
// a wake: it is read with whatever wakes the recipient next, or folds
// into a turn they are already running. The chat entry carries the
// mark, so the spawn gate agrees on every later broadcast wake. The
// same message without the declaration wakes as usual.
func TestQuietMessageIsDeliveredWithoutAWake(t *testing.T) {
	m := newTestMessenger(t)
	mustCreateAgent(t, m.Store, "bob")
	rec := &recordingHooks{store: m.Store}
	ctx := context.Background()

	if !m.DeliverToAgent(ctx, "bob", noticeTo("ceo", "bob", true), "messages/2026-09-22/quiet.md", rec.hooks()) {
		t.Fatal("delivery should succeed")
	}
	if len(rec.calls) != 0 {
		t.Fatalf("a quiet message woke %v", rec.calls)
	}
	hist, _ := m.Store.ReadChatHistory("bob")
	if len(hist) != 1 || !hist[0].Quiet || hist[0].Kind != "inbox_delivery" {
		t.Fatalf("quiet entry = %+v", hist)
	}
	if store.SpawnDecision(hist) != store.SpawnIdle {
		t.Fatal("a lone quiet message asks for a turn")
	}

	if !m.DeliverToAgent(ctx, "bob", noticeTo("ceo", "bob", false), "messages/2026-09-22/loud.md", rec.hooks()) {
		t.Fatal("delivery should succeed")
	}
	if len(rec.calls) != 1 || rec.calls[0] != "bob" {
		t.Fatalf("an ordinary message woke %v", rec.calls)
	}
	hist, _ = m.Store.ReadChatHistory("bob")
	if len(hist) != 2 || hist[1].Quiet || store.SpawnDecision(hist) != store.SpawnNow {
		t.Fatalf("ordinary entry = %+v", hist)
	}
}

// The declaration rides the file through the release path: an agent's
// quiet message waits for release like any other, and lands quiet when
// the CEO releases it, so the broadcast wake that follows a release
// finds nothing owed.
func TestReleasedQuietMessageStaysQuiet(t *testing.T) {
	m := newTestMessenger(t)
	mustCreateAgent(t, m.Store, "alice")
	mustCreateAgent(t, m.Store, "bob")
	rec := &recordingHooks{store: m.Store}
	ctx := context.Background()

	msg := noticeTo("alice", "bob", true)
	path, err := m.Store.WriteMessage(msg)
	if err != nil {
		t.Fatal(err)
	}
	msg.Path = path
	if _, err := m.Route(ctx, msg); err != nil {
		t.Fatal(err)
	}
	if hist, _ := m.Store.ReadChatHistory("bob"); len(hist) != 0 {
		t.Fatalf("an agent's quiet message reached bob before release: %+v", hist)
	}
	if _, err := m.ReleaseAll(ctx, nil, rec.hooks()); err != nil {
		t.Fatal(err)
	}
	hist, _ := m.Store.ReadChatHistory("bob")
	if len(hist) != 1 || !hist[0].Quiet {
		t.Fatalf("released quiet message = %+v", hist)
	}
	if store.SpawnDecision(hist) != store.SpawnIdle {
		t.Fatal("a released quiet message asks for a turn")
	}
}
