package store

import (
	"testing"
	"time"
)

// Pin the gate semantics directly so a future refactor to the
// "what counts as a real received" rule can't quietly break the
// spawn-suppression invariant the web layer depends on.
func TestHasUnansweredReceived(t *testing.T) {
	ts := func(n int) time.Time { return time.Date(2026, 4, 26, 10, 0, n, 0, time.UTC) }

	cases := []struct {
		name string
		hist []ChatMessage
		want bool
	}{
		{
			name: "empty history",
			hist: nil,
			want: false,
		},
		{
			name: "lone received with no reply yet",
			hist: []ChatMessage{
				{Role: RoleReceived, Kind: "inbox_delivery", TS: ts(0)},
			},
			want: true,
		},
		{
			name: "received then sent — already answered",
			hist: []ChatMessage{
				{Role: RoleReceived, Kind: "inbox_delivery", TS: ts(0)},
				{Role: RoleSent, Kind: "", TS: ts(1)},
			},
			want: false,
		},
		{
			name: "received, sent, then a fresh received — owes a reply",
			hist: []ChatMessage{
				{Role: RoleReceived, Kind: "inbox_delivery", TS: ts(0)},
				{Role: RoleSent, Kind: "", TS: ts(1)},
				{Role: RoleReceived, Kind: "inbox_delivery", TS: ts(2)},
			},
			want: true,
		},
		{
			name: "received, sent, then a tool_result tail — tool plumbing is not real input",
			hist: []ChatMessage{
				{Role: RoleReceived, Kind: "inbox_delivery", TS: ts(0)},
				{Role: RoleSent, Kind: "", TS: ts(1)},
				{Role: RoleReceived, Kind: "tool_result", TS: ts(2)},
			},
			want: false,
		},
		{
			name: "received, tool_use, tool_result, sent — answered",
			hist: []ChatMessage{
				{Role: RoleReceived, Kind: "inbox_delivery", TS: ts(0)},
				{Role: RoleSent, Kind: "tool_use", TS: ts(1)},
				{Role: RoleReceived, Kind: "tool_result", TS: ts(2)},
				{Role: RoleSent, Kind: "", TS: ts(3)},
			},
			want: false,
		},
		{
			name: "rotation_prompt as the trailing received entry",
			hist: []ChatMessage{
				{Role: RoleReceived, Kind: "rotation_prompt", TS: ts(0)},
			},
			want: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := HasUnansweredReceived(tc.hist); got != tc.want {
				t.Errorf("HasUnansweredReceived(%s) = %v, want %v", tc.name, got, tc.want)
			}
		})
	}
}

// TestSpawnDecision pins the chat-loop spawn gate's full state
// machine: the basic "is something owed?" rule plus the two interrupt
// markers (user-interruption from Stop, runtime-disruption from
// boot-detected kills). This is the single source of truth for "should
// the agent be re-spawned right now?" and a regression here would be
// either a Stop that doesn't actually stop, or an OOM'd agent that
// never resumes.
func TestSpawnDecision(t *testing.T) {
	ts := func(n int) time.Time { return time.Date(2026, 4, 28, 10, 0, n, 0, time.UTC) }

	cases := []struct {
		name string
		hist []ChatMessage
		want SpawnVerdict
	}{
		{
			name: "empty history → idle",
			hist: nil,
			want: SpawnIdle,
		},
		{
			name: "received not yet answered → spawn",
			hist: []ChatMessage{
				{Role: RoleReceived, Kind: "direct_chat", TS: ts(0)},
			},
			want: SpawnNow,
		},
		{
			name: "received then sent → idle",
			hist: []ChatMessage{
				{Role: RoleReceived, Kind: "direct_chat", TS: ts(0)},
				{Role: RoleSent, Kind: "", TS: ts(1)},
			},
			want: SpawnIdle,
		},
		{
			// The wake note never wakes anyone: a turn that died after
			// the note was appended must not respawn on it.
			name: "wake note after the reply → idle",
			hist: []ChatMessage{
				{Role: RoleReceived, Kind: "direct_chat", TS: ts(0)},
				{Role: RoleSent, Kind: "", TS: ts(1)},
				{Role: RoleReceived, Kind: KindWakeUpdate, TS: ts(2)},
			},
			want: SpawnIdle,
		},
		{
			name: "received then wake note → spawn (the message is still owed)",
			hist: []ChatMessage{
				{Role: RoleReceived, Kind: "direct_chat", TS: ts(0)},
				{Role: RoleReceived, Kind: KindWakeUpdate, TS: ts(1)},
			},
			want: SpawnNow,
		},
		{
			name: "user-interruption alone → hold for CEO",
			hist: []ChatMessage{
				{Role: RoleReceived, Kind: "direct_chat", TS: ts(0)},
				{Role: RoleSent, Kind: "", TS: ts(1)},
				{Role: RoleReceived, Kind: KindUserInterruption, TS: ts(2)},
			},
			want: SpawnHoldForCEO,
		},
		{
			name: "user-interruption then CEO redirect → spawn (interrupt cleared by message)",
			hist: []ChatMessage{
				{Role: RoleReceived, Kind: "direct_chat", TS: ts(0)},
				{Role: RoleSent, Kind: "", TS: ts(1)},
				{Role: RoleReceived, Kind: KindUserInterruption, TS: ts(2)},
				{Role: RoleReceived, Kind: "direct_chat", TS: ts(3)},
			},
			want: SpawnNow,
		},
		{
			// A turn that stopped on an error is held, not retried: a
			// usage limit or an expired login would fail again at once.
			name: "turn-error alone → hold on error",
			hist: []ChatMessage{
				{Role: RoleReceived, Kind: "direct_chat", TS: ts(0)},
				{Role: RoleSent, Kind: "tool_use", TS: ts(1)},
				{Role: RoleReceived, Kind: KindTurnError, TS: ts(2)},
			},
			want: SpawnHoldOnError,
		},
		{
			// The dying turn's trailing rows may land after the marker
			// (a tool_result the pod posted late, the partial text
			// flushed at teardown). They do not clear the hold.
			name: "turn-error then trailing sent text → still hold",
			hist: []ChatMessage{
				{Role: RoleReceived, Kind: "direct_chat", TS: ts(0)},
				{Role: RoleReceived, Kind: KindTurnError, TS: ts(1)},
				{Role: RoleSent, Kind: "direct_chat", Content: "partial", TS: ts(2)},
			},
			want: SpawnHoldOnError,
		},
		{
			name: "turn-error then a new received → spawn (same release rule as Stop)",
			hist: []ChatMessage{
				{Role: RoleReceived, Kind: "direct_chat", TS: ts(0)},
				{Role: RoleReceived, Kind: KindTurnError, TS: ts(1)},
				{Role: RoleReceived, Kind: "inbox_delivery", TS: ts(2)},
			},
			want: SpawnNow,
		},
		{
			name: "turn-error then a wake note → still hold (the note is plumbing)",
			hist: []ChatMessage{
				{Role: RoleReceived, Kind: "direct_chat", TS: ts(0)},
				{Role: RoleReceived, Kind: KindTurnError, TS: ts(1)},
				{Role: RoleReceived, Kind: KindWakeUpdate, TS: ts(2)},
			},
			want: SpawnHoldOnError,
		},
		{
			name: "user-interruption then turn-error → hold for CEO (intent outranks the fact)",
			hist: []ChatMessage{
				{Role: RoleReceived, Kind: "direct_chat", TS: ts(0)},
				{Role: RoleReceived, Kind: KindUserInterruption, TS: ts(1)},
				{Role: RoleReceived, Kind: KindTurnError, TS: ts(2)},
			},
			want: SpawnHoldForCEO,
		},
		{
			name: "turn-error with no received at all → hold on error",
			hist: []ChatMessage{
				{Role: RoleReceived, Kind: KindTurnError, TS: ts(0)},
			},
			want: SpawnHoldOnError,
		},
		{
			name: "runtime-disruption alone after agent reply → spawn (resume)",
			hist: []ChatMessage{
				{Role: RoleReceived, Kind: "direct_chat", TS: ts(0)},
				{Role: RoleSent, Kind: "", TS: ts(1)},
				{Role: RoleReceived, Kind: KindRuntimeDisruption, TS: ts(2)},
			},
			want: SpawnNow,
		},
		{
			// The resume happened and was answered: a RoleSent newer than
			// the disruption marker. Nothing is owed. Before this case was
			// pinned, the walk returned SpawnNow forever here, and every
			// release-all broadcast respawned the agent to answer the
			// trailer again.
			name: "runtime-disruption then the resumed reply → idle",
			hist: []ChatMessage{
				{Role: RoleReceived, Kind: "direct_chat", TS: ts(0)},
				{Role: RoleSent, Kind: "", TS: ts(1)},
				{Role: RoleReceived, Kind: KindRuntimeDisruption, TS: ts(2)},
				{Role: RoleReceived, Kind: KindWakeUpdate, TS: ts(3)},
				{Role: RoleSent, Kind: "", Content: "resumed", TS: ts(4)},
			},
			want: SpawnIdle,
		},
		{
			name: "runtime-disruption resumed with only tool activity → still spawn",
			hist: []ChatMessage{
				{Role: RoleReceived, Kind: "direct_chat", TS: ts(0)},
				{Role: RoleSent, Kind: "", TS: ts(1)},
				{Role: RoleReceived, Kind: KindRuntimeDisruption, TS: ts(2)},
				{Role: RoleSent, Kind: "tool_use", TS: ts(3)},
			},
			want: SpawnNow,
		},
		{
			name: "runtime-disruption mid-tool then nothing → spawn (resume)",
			hist: []ChatMessage{
				{Role: RoleReceived, Kind: "direct_chat", TS: ts(0)},
				{Role: RoleSent, Kind: "tool_use", TS: ts(1)},
				{Role: RoleReceived, Kind: KindRuntimeDisruption, TS: ts(2)},
			},
			want: SpawnNow,
		},
		{
			name: "user-interruption THEN runtime-disruption → hold (CEO intent persists)",
			hist: []ChatMessage{
				{Role: RoleReceived, Kind: "direct_chat", TS: ts(0)},
				{Role: RoleSent, Kind: "", TS: ts(1)},
				{Role: RoleReceived, Kind: KindUserInterruption, TS: ts(2)},
				{Role: RoleReceived, Kind: KindRuntimeDisruption, TS: ts(3)},
			},
			want: SpawnHoldForCEO,
		},
		{
			name: "user-interruption then runtime-disruption then CEO redirect → spawn",
			hist: []ChatMessage{
				{Role: RoleReceived, Kind: "direct_chat", TS: ts(0)},
				{Role: RoleSent, Kind: "", TS: ts(1)},
				{Role: RoleReceived, Kind: KindUserInterruption, TS: ts(2)},
				{Role: RoleReceived, Kind: KindRuntimeDisruption, TS: ts(3)},
				{Role: RoleReceived, Kind: "direct_chat", TS: ts(4)},
			},
			want: SpawnNow,
		},
		{
			name: "received then user-interruption (CEO sends then immediately stops) → hold",
			hist: []ChatMessage{
				{Role: RoleReceived, Kind: "direct_chat", TS: ts(0)},
				{Role: RoleReceived, Kind: KindUserInterruption, TS: ts(1)},
			},
			want: SpawnHoldForCEO,
		},
		{
			name: "tool plumbing trail past a real reply doesn't change verdict",
			hist: []ChatMessage{
				{Role: RoleReceived, Kind: "direct_chat", TS: ts(0)},
				{Role: RoleSent, Kind: "", TS: ts(1)},
				{Role: RoleSent, Kind: "tool_use", TS: ts(2)},
				{Role: RoleReceived, Kind: "tool_result", TS: ts(3)},
			},
			want: SpawnIdle,
		},
		{
			name: "fresh received after tool plumbing → spawn",
			hist: []ChatMessage{
				{Role: RoleSent, Kind: "tool_use", TS: ts(0)},
				{Role: RoleReceived, Kind: "tool_result", TS: ts(1)},
				{Role: RoleSent, Kind: "", TS: ts(2)},
				{Role: RoleReceived, Kind: "direct_chat", TS: ts(3)},
			},
			want: SpawnNow,
		},
		{
			// Sync-write of the marker (handleAgentStop now writes
			// at click time, not on the post-loop defer): the
			// cancelled subagent's tool_result lands AFTER the
			// marker. The marker is no longer at the tail; the walk
			// must still hold for CEO.
			name: "sync-write: RR then marker then tool pair → hold",
			hist: []ChatMessage{
				{Role: RoleReceived, Kind: "direct_chat", TS: ts(0)},
				{Role: RoleReceived, Kind: KindUserInterruption, TS: ts(1)},
				{Role: RoleSent, Kind: "tool_use", TS: ts(2)},
				{Role: RoleReceived, Kind: "tool_result", TS: ts(3)},
			},
			want: SpawnHoldForCEO,
		},
		{
			// Sync-write where the parent CLI emitted a partial
			// assistant text (e.g., "Subagent cancelled — retrying")
			// before being killed. The RoleSent at the tail is
			// post-marker; the walk must still hold.
			name: "sync-write: RR then marker then partial RS → hold",
			hist: []ChatMessage{
				{Role: RoleReceived, Kind: "direct_chat", TS: ts(0)},
				{Role: RoleReceived, Kind: KindUserInterruption, TS: ts(1)},
				{Role: RoleSent, Kind: "", Content: "partial", TS: ts(2)},
			},
			want: SpawnHoldForCEO,
		},
		{
			// Marker with no preceding RoleReceived at all (CEO
			// pressed Stop during a cold-start spawn that hadn't
			// loaded any received entry yet — pathological but
			// worth pinning).
			name: "marker alone with no RoleReceived → hold",
			hist: []ChatMessage{
				{Role: RoleReceived, Kind: KindUserInterruption, TS: ts(0)},
			},
			want: SpawnHoldForCEO,
		},
		{
			// Marker followed by partial agent activity, no RR.
			name: "marker then RS only (no RR) → hold",
			hist: []ChatMessage{
				{Role: RoleReceived, Kind: KindUserInterruption, TS: ts(0)},
				{Role: RoleSent, Kind: "", Content: "partial", TS: ts(1)},
			},
			want: SpawnHoldForCEO,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := SpawnDecision(tc.hist); got != tc.want {
				t.Errorf("SpawnDecision = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestLastRealReceivedTSSkipsSystemBoundaries pins the L2 fix: the
// "did something newer arrive than spawnTS?" check does NOT count
// user-interruption or runtime-disruption entries as fresh input.
// Without this, the post-loop spawnFollowupIfNewerReceived would fire
// (and immediately bail at the spawn gate) every time we wrote one of
// these markers — wasted work and noisy log lines.
func TestLastRealReceivedTSSkipsSystemBoundaries(t *testing.T) {
	ts := func(n int) time.Time { return time.Date(2026, 4, 28, 10, 0, n, 0, time.UTC) }

	cases := []struct {
		name string
		hist []ChatMessage
		want time.Time
	}{
		{
			name: "user-interruption alone returns zero",
			hist: []ChatMessage{
				{Role: RoleReceived, Kind: KindUserInterruption, TS: ts(5)},
			},
			want: time.Time{},
		},
		{
			name: "runtime-disruption alone returns zero",
			hist: []ChatMessage{
				{Role: RoleReceived, Kind: KindRuntimeDisruption, TS: ts(5)},
			},
			want: time.Time{},
		},
		{
			name: "real received then system marker → real received TS",
			hist: []ChatMessage{
				{Role: RoleReceived, Kind: "direct_chat", TS: ts(2)},
				{Role: RoleReceived, Kind: KindUserInterruption, TS: ts(5)},
			},
			want: ts(2),
		},
		{
			name: "real received past mixed markers and tool plumbing",
			hist: []ChatMessage{
				{Role: RoleReceived, Kind: "direct_chat", TS: ts(1)},
				{Role: RoleSent, Kind: "tool_use", TS: ts(2)},
				{Role: RoleReceived, Kind: "tool_result", TS: ts(3)},
				{Role: RoleSent, TS: ts(4)},
				{Role: RoleReceived, Kind: KindRuntimeDisruption, TS: ts(5)},
			},
			want: ts(1),
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := LastRealReceivedTS(tc.hist)
			if !got.Equal(tc.want) {
				t.Errorf("LastRealReceivedTS = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestConsecutiveRuntimeDisruptions pins the circuit-breaker count.
// Trailing runtime-disruption entries (modulo tool plumbing) count;
// any productive entry — agent reply, CEO direct chat, anything
// non-tool, non-system — resets the count to zero.
func TestConsecutiveRuntimeDisruptions(t *testing.T) {
	ts := func(n int) time.Time { return time.Date(2026, 4, 28, 10, 0, n, 0, time.UTC) }

	cases := []struct {
		name string
		hist []ChatMessage
		want int
	}{
		{
			name: "empty",
			hist: nil,
			want: 0,
		},
		{
			name: "one disruption at the tail",
			hist: []ChatMessage{
				{Role: RoleSent, TS: ts(0)},
				{Role: RoleReceived, Kind: KindRuntimeDisruption, TS: ts(1)},
			},
			want: 1,
		},
		{
			name: "three consecutive disruptions",
			hist: []ChatMessage{
				{Role: RoleReceived, Kind: KindRuntimeDisruption, TS: ts(0)},
				{Role: RoleReceived, Kind: KindRuntimeDisruption, TS: ts(1)},
				{Role: RoleReceived, Kind: KindRuntimeDisruption, TS: ts(2)},
			},
			want: 3,
		},
		{
			name: "disruption then agent reply resets count",
			hist: []ChatMessage{
				{Role: RoleReceived, Kind: KindRuntimeDisruption, TS: ts(0)},
				{Role: RoleSent, TS: ts(1)},
			},
			want: 0,
		},
		{
			name: "disruption then CEO direct chat resets count",
			hist: []ChatMessage{
				{Role: RoleReceived, Kind: KindRuntimeDisruption, TS: ts(0)},
				{Role: RoleReceived, Kind: "direct_chat", TS: ts(1)},
			},
			want: 0,
		},
		{
			name: "tool plumbing at tail is skipped, disruption underneath still counts",
			hist: []ChatMessage{
				{Role: RoleReceived, Kind: KindRuntimeDisruption, TS: ts(0)},
				{Role: RoleSent, Kind: "tool_use", TS: ts(1)},
				{Role: RoleReceived, Kind: "tool_result", TS: ts(2)},
			},
			want: 1,
		},
		{
			name: "user-interruption at tail breaks the run (it's not a runtime-disruption)",
			hist: []ChatMessage{
				{Role: RoleReceived, Kind: KindRuntimeDisruption, TS: ts(0)},
				{Role: RoleReceived, Kind: KindUserInterruption, TS: ts(1)},
			},
			want: 0,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := ConsecutiveRuntimeDisruptions(tc.hist); got != tc.want {
				t.Errorf("ConsecutiveRuntimeDisruptions = %d, want %d", got, tc.want)
			}
		})
	}
}
