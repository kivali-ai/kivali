package store

import (
	"testing"
	"time"
)

// A quiet received entry (a hold, a pure receipt) never starts a turn
// by itself: every history walker steps over it, so a broadcast wake
// finds nothing owed, while a real unanswered message beneath it still
// spawns.
func TestQuietReceivedAsksForNoTurn(t *testing.T) {
	ts := func(n int) time.Time { return time.Date(2026, 9, 22, 10, 0, n, 0, time.UTC) }
	recv := func(n int) ChatMessage { return ChatMessage{Role: RoleReceived, Kind: "inbox_delivery", TS: ts(n)} }
	quiet := func(n int) ChatMessage {
		return ChatMessage{Role: RoleReceived, Kind: "inbox_delivery", TS: ts(n), Quiet: true}
	}
	sent := func(n int) ChatMessage { return ChatMessage{Role: RoleSent, TS: ts(n)} }

	cases := []struct {
		name string
		hist []ChatMessage
		want SpawnVerdict
		owed bool
		last time.Time
	}{
		{"lone quiet", []ChatMessage{quiet(0)}, SpawnIdle, false, time.Time{}},
		{"answered, then quiet", []ChatMessage{recv(0), sent(1), quiet(2)}, SpawnIdle, false, ts(0)},
		{"quiet, then real", []ChatMessage{quiet(0), recv(1)}, SpawnNow, true, ts(1)},
		{"real under a quiet tail", []ChatMessage{sent(0), recv(1), quiet(2)}, SpawnNow, true, ts(1)},
		{"two quiet after an answer", []ChatMessage{recv(0), sent(1), quiet(2), quiet(3)}, SpawnIdle, false, ts(0)},
		// HasUnansweredReceived has never known about the Stop marker
		// (it reads it as a received entry); only the gate's verdict
		// and the timestamp matter here.
		{"stop marker survives a quiet tail", []ChatMessage{recv(0), sent(1), {Role: RoleReceived, Kind: KindUserInterruption, TS: ts(2)}, quiet(3)}, SpawnHoldForCEO, true, ts(0)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := SpawnDecision(tc.hist); got != tc.want {
				t.Errorf("SpawnDecision = %v, want %v", got, tc.want)
			}
			if got := HasUnansweredReceived(tc.hist); got != tc.owed {
				t.Errorf("HasUnansweredReceived = %v, want %v", got, tc.owed)
			}
			if got := LastRealReceivedTS(tc.hist); !got.Equal(tc.last) {
				t.Errorf("LastRealReceivedTS = %v, want %v", got, tc.last)
			}
		})
	}
}
