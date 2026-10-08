package mcp

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kivali-ai/kivali/internal/store"
)

// noticeStore seeds a store with the given active agents.
func noticeStore(t *testing.T, slugs ...string) *store.FSStore {
	t.Helper()
	s, err := store.New(t.TempDir())
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	for _, slug := range slugs {
		if err := s.CreateAgent(store.Agent{Slug: slug, Role: "R", ReportsTo: "ceo"}, "role"); err != nil {
			t.Fatalf("create %s: %v", slug, err)
		}
	}
	return s
}

// queueAll is a minimal ApplyRoute that mirrors messaging's fan-out:
// one inbox pointer per recipient. Keeps the mcp package free of a
// dependency on messaging while still exercising the multi-pointer
// shape the publish ack and pending-work rendering assume.
func queueAll(root string) func(context.Context, *store.MessageQueue, store.Message) error {
	return func(_ context.Context, q *store.MessageQueue, msg store.Message) error {
		if q.Agents == nil {
			q.Agents = map[string]store.AgentQueue{}
		}
		rel, err := filepath.Rel(root, msg.Path)
		if err != nil {
			return err
		}
		for _, to := range msg.To {
			rt := q.Agents[to]
			rt.Inbox = append(rt.Inbox, rel)
			q.Agents[to] = rt
		}
		return nil
	}
}

// TestPublishNoticeRoundtrip: a publish_notice call lands one message
// on disk carrying every recipient, and the ack names them all.
func TestPublishNoticeRoundtrip(t *testing.T) {
	s := noticeStore(t, "market-analyst", "alice", "bob")
	d := NewStorePublishDispatcher(StorePublishDeps{
		Store: s, From: "market-analyst", ApplyRoute: queueAll(s.Root()),
	})

	body, isErr, err := d.DispatchPublish(context.Background(), "publish_notice",
		json.RawMessage(`{"to":["alice","bob"],"title":"Pricing changed","body":"List price moved to $49."}`))
	if err != nil || isErr {
		t.Fatalf("DispatchPublish: err=%v isErr=%v body=%q", err, isErr, body)
	}
	for _, want := range []string{"alice", "bob", "Pricing changed"} {
		if !strings.Contains(body, want) {
			t.Errorf("ack should mention %q; got %q", want, body)
		}
	}

	msgs, err := s.ListMessages(store.MessageFilter{Type: store.MsgNotice})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(msgs) != 1 {
		t.Fatalf("want exactly one notice on disk; got %d", len(msgs))
	}
	if strings.Join(msgs[0].To, ",") != "alice,bob" {
		t.Errorf("to = %v, want [alice bob]", msgs[0].To)
	}
	q, _ := s.ReadMessageQueue()
	for _, slug := range []string{"alice", "bob"} {
		if len(q.Agents[slug].Inbox) != 1 {
			t.Errorf("%s should have one queued pointer; got %v", slug, q.Agents[slug].Inbox)
		}
	}
}

// TestPublishNoticeFastFailsOnUnknownRecipient: every named slug is
// checked, and all the misses come back in one round-trip.
func TestPublishNoticeFastFailsOnUnknownRecipient(t *testing.T) {
	s := noticeStore(t, "market-analyst", "alice")
	d := NewStorePublishDispatcher(StorePublishDeps{Store: s, From: "market-analyst"})

	body, isErr, err := d.DispatchPublish(context.Background(), "publish_notice",
		json.RawMessage(`{"to":["alice","ghost","phantom"],"title":"T","body":"b"}`))
	if err != nil {
		t.Fatalf("DispatchPublish: %v", err)
	}
	if !isErr {
		t.Fatalf("a notice naming unknown slugs should fail fast; got success %q", body)
	}
	for _, want := range []string{"ghost", "phantom"} {
		if !strings.Contains(body, want) {
			t.Errorf("error should name %q so one retry fixes both; got %q", want, body)
		}
	}
	msgs, _ := s.ListMessages(store.MessageFilter{From: "market-analyst"})
	if len(msgs) != 0 {
		t.Errorf("nothing should be persisted on fast-fail; got %d", len(msgs))
	}
}

// TestPublishNoticeAppendsAskWarning: the soft lint rides on a
// successful publish. The message still lands — the CEO moderates —
// but the model sees the nudge while it can still pick the right tool.
func TestPublishNoticeAppendsAskWarning(t *testing.T) {
	s := noticeStore(t, "market-analyst", "alice")
	d := NewStorePublishDispatcher(StorePublishDeps{
		Store: s, From: "market-analyst", ApplyRoute: queueAll(s.Root()),
	})

	body, isErr, err := d.DispatchPublish(context.Background(), "publish_notice",
		json.RawMessage(`{"to":["alice"],"title":"Pricing","body":"Price moved. Please refresh the deck."}`))
	if err != nil || isErr {
		t.Fatalf("the lint must not fail the publish: err=%v isErr=%v body=%q", err, isErr, body)
	}
	if !strings.Contains(body, "reads like an ask") {
		t.Errorf("ack should carry the ask warning; got %q", body)
	}
	if !strings.Contains(body, "assignment_create") {
		t.Errorf("warning should name the tracker; got %q", body)
	}
	if msgs, _ := s.ListMessages(store.MessageFilter{Type: store.MsgNotice}); len(msgs) != 1 {
		t.Errorf("the notice should still have been published; got %d", len(msgs))
	}

	// A plain tell gets no warning.
	clean, _, _ := d.DispatchPublish(context.Background(), "publish_notice",
		json.RawMessage(`{"to":["alice"],"title":"Pricing","body":"List price moved to $49 today."}`))
	if strings.Contains(clean, "reads like an ask") {
		t.Errorf("a plain tell should not be flagged; got %q", clean)
	}
}
