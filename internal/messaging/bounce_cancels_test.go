package messaging

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kivali-ai/kivali/internal/agent"
	"github.com/kivali-ai/kivali/internal/store"
)

// TestBouncedMessageLeavesTheLiveLedger: a bounce cancels the message —
// no recipient got it and none ever will. It must not be rediscoverable
// by any messages/ scan, or it resurfaces as work that is still in
// flight.
func TestBouncedMessageLeavesTheLiveLedger(t *testing.T) {
	m := newTestMessenger(t)
	mustCreateAgent(t, m.Store, "alice")
	mustCreateAgent(t, m.Store, "bob")
	rec := &recordingHooks{store: m.Store}

	rel := seedQueued(t, m, "alice", "bob", "do the thing")
	if err := m.Bounce(context.Background(), rel, "wrong owner", rec.hooks()); err != nil {
		t.Fatalf("Bounce: %v", err)
	}

	all, err := m.Store.ListMessages(store.MessageFilter{Type: store.MsgNotice})
	if err != nil {
		t.Fatalf("ListMessages: %v", err)
	}
	for _, d := range all {
		if strings.HasSuffix(d.Path, filepath.Base(rel)) {
			t.Errorf("bounced message %s is still in the live ledger at %s", rel, d.Path)
		}
	}
}

// TestBounceIsATellNotAnAnswer: the CEO's bounce is a parentless notice
// to the sender. It never names the bounced message in in_reply_to —
// a notice is something nothing may point at, and the ledger must obey
// the rule agents are held to.
func TestBounceIsATellNotAnAnswer(t *testing.T) {
	m := newTestMessenger(t)
	mustCreateAgent(t, m.Store, "alice")
	mustCreateAgent(t, m.Store, "bob")
	rec := &recordingHooks{store: m.Store}

	rel := seedQueued(t, m, "alice", "bob", "do the thing")
	if err := m.Bounce(context.Background(), rel, "not good enough", rec.hooks()); err != nil {
		t.Fatalf("Bounce: %v", err)
	}

	bounces, err := m.Store.ListMessages(store.MessageFilter{From: agent.CEOSlug})
	if err != nil {
		t.Fatalf("ListMessages: %v", err)
	}
	if len(bounces) != 1 {
		t.Fatalf("want one CEO-authored bounce, got %d", len(bounces))
	}
	if bounces[0].Type != store.MsgNotice {
		t.Errorf("bounce type = %s, want notice", bounces[0].Type)
	}
	if bounces[0].InReplyTo != "" {
		t.Errorf("bounce names %q in in_reply_to; a tell points at nothing", bounces[0].InReplyTo)
	}
}
