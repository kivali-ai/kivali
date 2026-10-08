package store

import "time"

// Kind values for system-role chat entries that mark turn boundaries.
//
//   - KindUserInterruption: written by the chat-stop handler when the
//     CEO presses Stop. Tells the next spawning agent (and the
//     handbook) that the prior turn was halted on purpose.
//   - KindRuntimeDisruption: written at Kivali boot when an active-turn
//     marker file is found without a matching turn-end (subprocess died:
//     OOM, segfault, pod restart). Tells the next spawning agent that
//     the prior turn was halted by infrastructure, not by the CEO.
//   - KindTurnError: written by the chat-turn failure path when the
//     turn stopped on an error the runtime could not recover from and
//     that will not clear on its own: the model call failed (a usage
//     limit, an expired login, an API error), the transport failed, or
//     the agent pod went away mid-turn. Unlike a runtime disruption the
//     agent is NOT resumed automatically — a second attempt would hit
//     the same wall — so the gate holds it until something new arrives,
//     exactly as it holds after Stop. Content carries the error detail
//     so the CEO reads the cause on the page and the agent reads it on
//     its next wake.
//
// None is "tool plumbing"; all interact with the spawn gate (see
// SpawnDecision below) and the handbook's "When you're interrupted"
// section.
const (
	KindUserInterruption  = "user-interruption"
	KindRuntimeDisruption = "runtime-disruption"
	KindTurnError         = "turn-error"
)

// KindPausedToDeliver marks the seam where the CEO pressed "Send now"
// on a message waiting behind a running turn: core ended the turn
// early, kept what it had written, and delivered the waiting messages
// right after this row. Unlike a Stop marker it does NOT hold the
// agent — the messages that follow it are the reason for the next
// turn — so the spawn gate walks past it. It is a system boundary for
// LastRealReceivedTS, and the model reads it as a bracketed notice
// before the delivered text.
const KindPausedToDeliver = "paused-to-deliver"

// KindWakeUpdate marks the note the runtime appends at wake, behind
// the deliveries that woke the agent: what changed in the knowledge
// graph since the agent's last turn, the agent's own index findings,
// whether the handbook, its role or the skills moved, and the
// assignments the agent holds. It is RoleReceived text the model reads with
// the deliveries that woke it, but it is NOT something the agent owes
// a reply to, so the spawn gate treats it as plumbing: it never causes
// a wake and a turn that dies before answering does not respawn on
// it. See docs/developers/knowledge-graph.md §"The wake note".
const KindWakeUpdate = "wake_update"

// KindSubagentResult marks a background subagent job's outcome,
// delivered to the parent as a RoleReceived entry when the job
// finishes.
//
// It is a received message rather than a tool_result because a
// dispatched job has no pending tool call to satisfy — the parent's
// subagent call returned a receipt immediately and its turn may have
// ended long before the work did. Being RoleReceived is also what
// makes the spawn gate treat it as something the agent owes a
// response to, which is how an idle parent gets woken by a result.
const KindSubagentResult = "subagent_result"

// SpawnVerdict is the result of the spawn gate's walk over chat
// history. Three outcomes; spawnChatLoopIfIdle fires only on Spawn.
type SpawnVerdict int

const (
	// SpawnIdle: nothing to do — the agent has already responded to
	// everything in their chat and there's no pending interrupt or
	// runtime-disruption to act on.
	SpawnIdle SpawnVerdict = iota
	// SpawnNow: spawn a turn. Either there's an unanswered RoleReceived
	// entry (normal incoming message), or there's a pending
	// runtime-disruption entry the agent should resume from.
	SpawnNow
	// SpawnHoldForCEO: the agent was interrupted by the CEO (Stop) and
	// has not yet been redirected. Hold; the next CEO direct message
	// will land in chat.jsonl AFTER the user-interruption entry and
	// flip the verdict to SpawnNow on the next call.
	SpawnHoldForCEO
	// SpawnHoldOnError: the agent's last turn stopped on an error
	// (KindTurnError) and nothing has arrived since. Hold, with the
	// same release rule as SpawnHoldForCEO: the next received entry
	// lands after the marker and flips the verdict to SpawnNow. Kept
	// distinct from SpawnHoldForCEO so the sidebar can say "stopped on
	// an error" rather than "waiting for the CEO".
	SpawnHoldOnError
)

// SpawnDecision walks chat history backwards (most-recent first),
// skipping tool plumbing, and returns whether a fresh spawn should
// fire. The decision honors the three "system" chat entries:
//
//   - KindUserInterruption: the CEO pressed Stop. Held until the CEO
//     sends a fresh RoleReceived — that redirect appears after the
//     marker in chat.jsonl and clears the hold on the walk's first hit.
//   - KindTurnError: the turn stopped on an error that will not clear
//     by itself. Held exactly like an interrupt — the next received
//     entry after the marker releases it — but reported as
//     SpawnHoldOnError so the UI can name the cause. An interrupt
//     sitting anywhere after the same received entry still wins.
//   - KindRuntimeDisruption: the runtime killed the subprocess
//     mid-turn. The agent should resume from where it was — unless a
//     user-interruption sits between the disruption and the latest
//     unanswered message, in which case the CEO's halt wins.
//
// Walking back, three flags collect: whether we've seen a
// user-interrupt marker, a runtime-disrupt marker, and the agent's
// own RoleSent activity. The decision fires on the first
// RoleReceived (= most recent received message): if there's a
// marker between it and the tail, hold; if RoleSent appears between
// it and the tail (no marker), the agent already replied → idle. If
// no RoleReceived at all is reached, fall back to flag-only verdicts
// for the all-system-markers case.
//
// The marker is allowed to appear ANYWHERE between the most-recent
// RoleReceived and the tail (handleAgentStop writes synchronously at
// click time, so trailing tool_use/tool_result/RoleSent entries from
// the dying turn may land after the marker).
//
// A Quiet received entry (a hold, a pure receipt) is walked past like
// plumbing: it is read with whatever next wakes the agent and never
// starts a turn by itself.
//
// Edge case: stop-then-crash (no message in between) → walk
// encounters runtime-disruption, then user-interruption, then
// RoleSent. User-interruption "wins" — the CEO's halt intent
// persists across the crash, and we hold until they redirect.
func SpawnDecision(hist []ChatMessage) SpawnVerdict {
	sawUserInterrupt := false
	sawTurnError := false
	sawRuntimeDisrupt := false
	sawSentSinceReceived := false
	for i := len(hist) - 1; i >= 0; i-- {
		m := hist[i]
		if isToolPlumbing(m.Kind) || isQuiet(m) {
			continue
		}
		switch m.Kind {
		case KindPausedToDeliver:
			// A pause is not a halt: the delivered messages after it
			// are what the agent owes. Walked past, sets no flag.
			continue
		case KindUserInterruption:
			sawUserInterrupt = true
			continue
		case KindTurnError:
			// Same hold as an interrupt: the turn stopped and must not
			// be retried until something new arrives. Trailing tool
			// rows from the dying turn may land after it, so it is
			// collected as a flag rather than decided on the spot.
			sawTurnError = true
			continue
		case KindRuntimeDisruption:
			// A disruption asks for a resume only until the agent
			// speaks again. Walking backwards, a RoleSent already seen
			// is newer than this marker: the resume happened and was
			// answered, so the marker is history, not a pending wake.
			// Without this, every broadcast wake respawned an agent
			// whose resume had long since finished.
			if !sawSentSinceReceived {
				sawRuntimeDisrupt = true
			}
			continue
		}
		if m.Role == RoleSent {
			sawSentSinceReceived = true
			continue
		}
		// First RoleReceived (= most-recent unanswered candidate).
		// Interrupt wins over both prior agent activity and disruption;
		// an error hold comes next, for the same reason — the CEO's
		// halt is intent, the error is a fact, and both outrank "there
		// is mail".
		if sawUserInterrupt {
			return SpawnHoldForCEO
		}
		if sawTurnError {
			return SpawnHoldOnError
		}
		if sawSentSinceReceived {
			// Agent already replied to this message. Resume on
			// runtime-disruption, otherwise idle.
			if sawRuntimeDisrupt {
				return SpawnNow
			}
			return SpawnIdle
		}
		return SpawnNow
	}
	// No RoleReceived in history (or only system markers / RoleSent
	// activity).
	if sawUserInterrupt {
		return SpawnHoldForCEO
	}
	if sawTurnError {
		return SpawnHoldOnError
	}
	if sawRuntimeDisrupt {
		return SpawnNow
	}
	return SpawnIdle
}

// LastRealReceivedTS returns the timestamp of the most recent
// received chat entry that represents fresh input the agent is
// expected to respond to: direct_chat, inbox_delivery, rotation_prompt,
// ceo_inbox (for the CEO's own chat). Returns zero time when no such
// entry exists.
//
// Skipped:
//   - Tool plumbing (tool_use, tool_result, doc_published): the
//     agent's own in-flight thinking, not fresh input. Otherwise
//     mid-response tool_results would fool follow-up spawns into
//     firing after every tool call.
//   - System boundary markers (user-interruption, runtime-disruption):
//     state about the agent's prior turn, not new mail. The spawn
//     gate (SpawnDecision) consumes these specially; a Stop click
//     is NOT a "newer received arrived" signal that should fire a
//     defensive follow-up loop.
//   - Quiet entries (a hold, a pure receipt): read with the next
//     wake, never a reason for one.
//
// Shared here (rather than per-package) so every layer agrees on
// what counts as "real" received content. See
// docs/developers/context-serialization.md §4 for the invariant this feeds.
func LastRealReceivedTS(hist []ChatMessage) time.Time {
	for i := len(hist) - 1; i >= 0; i-- {
		m := hist[i]
		if m.Role != RoleReceived {
			continue
		}
		if isToolPlumbing(m.Kind) || isQuiet(m) {
			continue
		}
		if isSystemBoundary(m.Kind) {
			continue
		}
		return m.TS
	}
	return time.Time{}
}

// isQuiet reports whether a received entry asks for no turn of its
// own. See ChatMessage.Quiet.
func isQuiet(m ChatMessage) bool { return m.Quiet && m.Role == RoleReceived }

// isSystemBoundary reports whether a chat-message Kind is a system
// marker about turn boundaries (user-interruption, runtime-disruption,
// turn-error, paused-to-deliver) rather than fresh input or tool
// plumbing. Centralized so callers agree on the rule.
func isSystemBoundary(kind string) bool {
	switch kind {
	case KindUserInterruption, KindRuntimeDisruption, KindTurnError, KindPausedToDeliver:
		return true
	}
	return false
}

// HasUnansweredReceived reports whether the agent owes a response
// according to chat state — i.e., the last "real" (non-tool-plumbing)
// entry in chat history is a RoleReceived. This is the file-order
// dual of LastRealReceivedTS's TS check: where TS-comparison answers
// "did something newer arrive than what I processed last spawn?",
// this answers "is the conversation currently in a state where a
// reply is expected?".
//
// The two gates serve different purposes:
//   - Use HasUnansweredReceived to decide whether a fresh spawn
//     (release-all wake, CEO message, ack delivery) should fire a
//     Claude call at all. False = the agent already replied to
//     everything; spawning would just confuse the model with stale
//     context and bloat chat history with a spurious reply.
//   - Use LastRealReceivedTS for the post-loop race check: did a
//     mid-loop delivery slip past the buffer.
//
// Why file-order works here even though tool flows interleave:
// tool_use / tool_result / doc_published are skipped, so a partial
// tool stream that ends mid-call still surfaces the assistant_text
// (RoleSent) that requested the tool — the agent's own output, so
// no fresh response is "owed." The buffer flush appends mid-flight
// deliveries strictly to the file tail at loop end, so a real
// "you have new mail" state always shows as a RoleReceived at the
// end of file order.
func HasUnansweredReceived(hist []ChatMessage) bool {
	for i := len(hist) - 1; i >= 0; i-- {
		m := hist[i]
		if isToolPlumbing(m.Kind) || isQuiet(m) {
			continue
		}
		return m.Role == RoleReceived
	}
	return false
}

// ConsecutiveRuntimeDisruptions counts how many KindRuntimeDisruption
// entries trail at the very end of chat history, ignoring tool
// plumbing in between. Used as a crash-loop circuit breaker: if the
// agent has died and resumed N times in a row with no productive
// activity between attempts, something is wrong (memory leak, poison
// content, broken downstream) and we should stop auto-respawning.
//
// The count resets to 0 the moment any non-tool, non-system entry
// lands in chat — the next agent response, the next CEO direct
// message, anything. So a stuck agent that the operator manually
// re-engages with (e.g., a CEO direct chat) automatically clears
// the breaker.
func ConsecutiveRuntimeDisruptions(hist []ChatMessage) int {
	n := 0
	for i := len(hist) - 1; i >= 0; i-- {
		m := hist[i]
		if isToolPlumbing(m.Kind) {
			continue
		}
		if m.Kind == KindRuntimeDisruption {
			n++
			continue
		}
		break
	}
	return n
}

// isToolPlumbing reports whether a chat-message Kind is something
// other than a piece of the turn-by-turn dialogue: the agent's own
// in-flight tool execution, or the runtime's wake note that rides
// along with a wake without being owed a reply. Centralized so every
// "what counts as real" caller agrees on the rule.
func isToolPlumbing(kind string) bool {
	switch kind {
	case "tool_use", "tool_result", "doc_published", KindWakeUpdate:
		return true
	}
	return false
}
