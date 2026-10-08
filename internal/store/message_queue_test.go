package store

import "testing"

func TestMessageQueueEmpty(t *testing.T) {
	s := mustStore(t)
	ts, err := s.ReadMessageQueue()
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if ts.Agents == nil {
		t.Error("Agents map nil")
	}
}

func TestMessageQueueRoundtrip(t *testing.T) {
	s := mustStore(t)
	ts := MessageQueue{
		Agents: map[string]AgentQueue{
			"alice": {Status: "ready", Inbox: []string{"messages/2026-04-18/x.md"}},
			"bob":   {Status: "paused"},
		},
	}
	if err := s.WriteMessageQueue(ts); err != nil {
		t.Fatalf("Write: %v", err)
	}
	got, err := s.ReadMessageQueue()
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if len(got.Agents) != 2 {
		t.Fatalf("agents = %d", len(got.Agents))
	}
	if got.Agents["alice"].Status != "ready" {
		t.Errorf("alice.Status = %q", got.Agents["alice"].Status)
	}
	if len(got.Agents["alice"].Inbox) != 1 {
		t.Errorf("alice.Inbox = %v", got.Agents["alice"].Inbox)
	}
	if got.Agents["bob"].Status != "paused" {
		t.Errorf("bob.Status = %q", got.Agents["bob"].Status)
	}
}

// TestMessageQueueUnknownFieldIgnored: a message_queue.json carrying a
// key nothing reads still parses.
func TestMessageQueueUnknownFieldIgnored(t *testing.T) {
	s := mustStore(t)
	const body = `{"unknown_field":7,"agents":{"alice":{"status":"ready","inbox":[]}}}`
	if err := writeAtomic(s.path("message_queue.json"), []byte(body), 0o644); err != nil {
		t.Fatalf("seed: %v", err)
	}
	got, err := s.ReadMessageQueue()
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if got.Agents["alice"].Status != "ready" {
		t.Errorf("alice.Status = %q (an unknown key shouldn't break parsing)", got.Agents["alice"].Status)
	}
}
