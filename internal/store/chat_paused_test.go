package store

import (
	"testing"
	"time"
)

// The paused-to-deliver marker sits between a turn Send now cut short
// and the messages it delivered. It must never hold the agent (those
// messages are what the next turn answers) and never count as fresh
// input itself.
func TestPausedToDeliverMarkerNeverHolds(t *testing.T) {
	ts := func(n int) time.Time { return time.Date(2026, 9, 28, 10, 0, n, 0, time.UTC) }
	recv := func(n int) ChatMessage { return ChatMessage{Role: RoleReceived, Kind: "direct_chat", TS: ts(n)} }
	sent := func(n int) ChatMessage { return ChatMessage{Role: RoleSent, Kind: "direct_chat", TS: ts(n)} }
	paused := func(n int) ChatMessage {
		return ChatMessage{Role: RoleReceived, Kind: KindPausedToDeliver, Content: "Paused to deliver your message", TS: ts(n)}
	}

	cases := []struct {
		name string
		hist []ChatMessage
		want SpawnVerdict
		last time.Time
	}{
		// The shape the flush writes: partial reply, marker, message.
		{"reply, marker, message", []ChatMessage{recv(0), sent(1), paused(2), recv(3)}, SpawnNow, ts(3)},
		// The follow-up answered it: nothing owed, and the marker does
		// not reopen anything.
		{"answered after the pause", []ChatMessage{recv(0), sent(1), paused(2), recv(3), sent(4)}, SpawnIdle, ts(3)},
		// A lone marker (never written by core, but a walker must not
		// hold on one) reads as nothing new.
		{"marker at the tail", []ChatMessage{recv(0), sent(1), paused(2)}, SpawnIdle, ts(0)},
		// A Stop marker still wins when it is the one after the message.
		{"stop after the pause", []ChatMessage{sent(0), paused(1), recv(2), {Role: RoleReceived, Kind: KindUserInterruption, TS: ts(3)}}, SpawnHoldForCEO, ts(2)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := SpawnDecision(tc.hist); got != tc.want {
				t.Errorf("SpawnDecision = %v, want %v", got, tc.want)
			}
			if got := LastRealReceivedTS(tc.hist); !got.Equal(tc.last) {
				t.Errorf("LastRealReceivedTS = %v, want %v", got, tc.last)
			}
		})
	}
	if !isSystemBoundary(KindPausedToDeliver) {
		t.Errorf("isSystemBoundary(%q) = false", KindPausedToDeliver)
	}
}
