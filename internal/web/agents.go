package web

import (
	"path/filepath"

	"github.com/kivali-ai/kivali/internal/agent"
	"github.com/kivali-ai/kivali/internal/store"
)

// agentWorking reports whether slug has work outstanding (see the
// Working note in handleAgentDetail) and how many of its background
// tasks are still running. Shared by the agent page and the chat API.
func (s *Server) agentWorking(slug string) (working bool, outstanding int) {
	s.streamMu.Lock()
	hub, hubPresent := s.chatHubs[slug]
	s.streamMu.Unlock()
	// A hub whose turn has sent its `done`, or one that completed and
	// lingers, is NOT streaming — nothing more is coming, and the UI
	// shouldn't show "thinking…" (chatHub.turnEnded says why).
	hubStreaming := hubPresent && hub.liveForPage()
	// The third term is what keeps an agent honest while its
	// background subagents run. Its own turn has ended by then — it
	// dispatched, said what it was doing, and stopped — but work it
	// owns is still burning tokens, and rendering that as "idle"
	// would be a lie the CEO acts on.
	outstanding = s.SubagentService.OutstandingSubagents(slug)
	return hubStreaming || s.turnInFlight(slug) || outstanding > 0, outstanding
}

// fillBucket groups a context-fill percentage into a coarse band
// ("low" | "mid" | "high" | "critical") that the org snapshot and the
// chat API carry, so every client colours context fill the same way.
func fillBucket(pct int) string {
	switch {
	case pct >= 90:
		return "critical"
	case pct >= 75:
		return "high"
	case pct >= 50:
		return "mid"
	default:
		return "low"
	}
}

// chatFillStats is the bundle of context-window numbers the chat API
// and the agent stream's "done" and "chat_fill" events all carry.
// Centralizing the math keeps the live numbers aligned with what the
// next GET would report, so a reload introduces no visible jump.
type chatFillStats struct {
	TokenEst      int
	ContextLimit  int
	LongThresh    int
	FillPct       int
	FillBucket    string
	ResolvedModel string
}

// computeChatFillStats resolves the agent's effective model (per-agent
// override, falling back to the cluster default) and computes the
// ring's tokenEst / fillPct / bucket. Fill ratio is clamped to
// [0, 100] so a runaway estimate can't break the SVG arc math
// downstream. LongThresh is 75% of the window — the "Chat is getting
// long" banner trips above that line.
//
// realContextTokens is the last turn's true single-call window
// occupancy (store.ContextWindowStat.ContextTokens). When > 0 it
// drives the ring directly — it already accounts for the system
// prompt, role, tool/skill/MCP schemas, injected memory, and any
// pulled-in file content, which the chars/4 transcript estimate omits
// entirely. Before any turn has run (cold start, or just-rotated
// chat) it's 0 and we fall back to the chars-over-4 estimate of the
// transcript, which is better than showing nothing.
func (s *Server) computeChatFillStats(agentModel, defaultModel string, hist []store.ChatMessage, realContextTokens int) chatFillStats {
	resolvedModel := agentModel
	if resolvedModel == "" {
		resolvedModel = defaultModel
	}
	ctxWindow := s.modelContextWindow(resolvedModel)
	tokenEst := realContextTokens
	if tokenEst <= 0 {
		tokenEst = estimateChatTokens(hist)
	}
	longThresh := ctxWindow * 3 / 4
	fillPct := 0
	if ctxWindow > 0 {
		fillPct = tokenEst * 100 / ctxWindow
		if fillPct > 100 {
			fillPct = 100
		}
	}
	return chatFillStats{
		TokenEst:      tokenEst,
		ContextLimit:  ctxWindow,
		LongThresh:    longThresh,
		FillPct:       fillPct,
		FillBucket:    fillBucket(fillPct),
		ResolvedModel: resolvedModel,
	}
}

// estimateChatTokens is a rough chars-over-4 approximation of a
// chat's token footprint. Used only for UI heuristics (the "chat is
// getting long, consider rotating" cue) — not for any billing path.
func estimateChatTokens(hist []store.ChatMessage) int {
	total := 0
	for _, m := range hist {
		total += len(m.Content)
		total += len(m.ToolInput)
	}
	return total / 4
}

// toolResultsByID builds a lookup of tool_use_id → tool_result
// ChatMessage so the transcript can fold each tool_use together with
// its result into one row (instead of two separate siblings).
func toolResultsByID(hist []store.ChatMessage) map[string]store.ChatMessage {
	out := make(map[string]store.ChatMessage)
	for _, m := range hist {
		if m.Kind == "tool_result" && m.ToolUseID != "" {
			out[m.ToolUseID] = m
		}
	}
	return out
}

// loadInboxViews reads the persisted message for each
// inbox_delivery entry in the history and returns a map keyed by
// MessageRef. The transcript builds inbox rows from these structured
// views rather than parsing the rendered text, so the rows can't drift
// from what the model actually sees.
//
// Missing / unreadable refs are silently dropped; the transcript then
// falls back to the raw chat entry.
func (s *Server) loadInboxViews(hist []store.ChatMessage) map[string]agent.InboxView {
	out := map[string]agent.InboxView{}
	for _, e := range hist {
		if e.Kind != "inbox_delivery" || e.MessageRef == "" {
			continue
		}
		if _, ok := out[e.MessageRef]; ok {
			continue
		}
		m, err := s.Store.ReadMessage(filepath.Join(s.Store.Root(), e.MessageRef))
		if err != nil {
			continue
		}
		out[e.MessageRef] = agent.BuildInboxView(m, e.MessageRef)
	}
	return out
}
