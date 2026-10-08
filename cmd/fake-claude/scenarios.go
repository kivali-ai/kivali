package main

import (
	"fmt"
	"regexp"
	"strings"
	"time"
)

// Scenario names. A user message may pick one for its own turn with a
// `[fake:<name>]` tag; otherwise FAKE_CLAUDE_SCENARIO decides.
const (
	scenarioReply    = "reply"
	scenarioSlow     = "slow"
	scenarioFold     = "fold"
	scenarioError    = "error"
	scenarioSubagent = "subagent"
)

var scenarioNames = []string{scenarioReply, scenarioSlow, scenarioFold, scenarioError, scenarioSubagent}

func knownScenario(s string) bool {
	for _, n := range scenarioNames {
		if n == s {
			return true
		}
	}
	return false
}

var scenarioTag = regexp.MustCompile(`\[fake:([a-z]+)\]`)

// scenarioFor picks the scenario for one turn from the newest message
// in its prompt (see inbound.newest); the last known tag in it wins.
func (e *engine) scenarioFor(text string) string {
	m := scenarioTag.FindAllStringSubmatch(text, -1)
	for i := len(m) - 1; i >= 0; i-- {
		if knownScenario(m[i][1]) {
			return m[i][1]
		}
	}
	return e.cfg.scenario
}

// The scripted words. Specs assert on these, so they are constants.
const (
	replyThinking  = "The CEO wants the mail switch status. Check the test-reminder log first."
	replyDelta1    = "I'll "
	replyDelta2    = "check the reminders "
	replyDelta3    = "today."
	replyToolPath  = "/files/project/mail-switch.md"
	replyToolOut   = "Test reminders: 500 of 500 sent; 6 bounced on old addresses."
	replyFinal     = "All 500 test reminders went out. Six bounced on old addresses, and Test runner is cleaning the list this afternoon."
	foldAckPrefix  = "Got your note: "
	errorLead      = "Checking the mail log."
	errorDetail    = `API Error: 529 {"type":"error","error":{"type":"overloaded_error","message":"Overloaded"}}`
	subagentTask   = "Summarise the test-reminder log"
	subagentFinal  = "I've asked a subagent to summarise the test-reminder log; its answer will come back as a message."
	utilityFiles   = "A short project note for the Plainsong team."
	utilityEpisode = "title: Mail switch status for Riverside Food Bank\n" +
		"touched: engineering-lead, test-runner, mail-switch.md\n" +
		"---\n" +
		"Asked: The CEO asked for the state of the mail switch.\n" +
		"Did: The agent read the test-reminder log and checked the bounces.\n" +
		"Concluded: All 500 test reminders went out; six bounced on old addresses.\n" +
		"Left open: Cleaning the address list with Test runner."
)

// utilityReply answers the one-shot background calls core makes with no
// agent behind them (project-file summaries, episode records). They share the chat scenarios' wire but want a
// single plain answer in the shape their parsers expect, so they are
// recognised by the opening words of their fixed system prompts.
func utilityReply(system string) (string, bool) {
	switch {
	case strings.HasPrefix(system, "You are summarizing project files"):
		return utilityFiles, true
	case strings.HasPrefix(system, "You are writing the episode record"):
		return utilityEpisode, true
	}
	return "", false
}

// runTurn plays one turn for msg.
func (e *engine) runTurn(msg inbound) {
	e.turn++
	e.lastText = ""
	ts := &turnState{}
	if msg.UUID != "" {
		ts.ledger = append(ts.ledger, msg.UUID)
	}
	e.lifecycle(msg.UUID, "started")
	e.initFrame()

	if text, ok := utilityReply(e.cfg.systemPrompt); ok {
		e.text(e.newCall(ts), text)
		e.result(ts, false)
		return
	}

	newest := msg.newest()
	scenario := e.scenarioFor(newest)
	e.tracef("turn %d scenario=%s newest=%q", e.turn, scenario, newest)
	switch scenario {
	case scenarioSlow:
		e.playReply(ts, e.cfg.pause, false)
	case scenarioFold:
		e.playReply(ts, 0, true)
	case scenarioError:
		e.playError(ts)
	case scenarioSubagent:
		e.playSubagent(ts)
	default:
		e.playReply(ts, 0, false)
	}
}

// playReply is the ordinary turn: a thinking block, three text deltas,
// one harmless tool round, a closing sentence. pause > 0 waits between
// deltas (the `slow` scenario); waitFold holds after the first delta
// until a second message arrives (the `fold` scenario). Either wait
// ends at once on an interrupt, which ends the turn.
func (e *engine) playReply(ts *turnState, pause time.Duration, waitFold bool) {
	callA := e.newCall(ts)
	e.thinking(callA, replyThinking)
	e.text(callA, replyDelta1)
	if waitFold {
		if e.pause(ts, e.cfg.foldWait, true) || e.pause(ts, e.cfg.foldHold, false) {
			e.result(ts, false)
			return
		}
	}
	for _, d := range []string{replyDelta2, replyDelta3} {
		if e.pause(ts, pause, false) {
			e.result(ts, false)
			return
		}
		e.text(callA, d)
	}
	toolID := fmt.Sprintf("toolu_fake_%03d_01", e.turn)
	e.toolUse(callA, toolID, "mcp__kivali__file_view", map[string]any{"path": replyToolPath})
	e.toolResult(toolID, replyToolOut, false)
	folded := e.seam(ts)
	if ts.interrupted {
		e.result(ts, false)
		return
	}
	callB := e.newCall(ts)
	for _, f := range folded {
		e.text(callB, foldAckPrefix+quote(f.text())+"\n\n")
	}
	e.text(callB, replyFinal)
	e.result(ts, false)
}

// playError is a turn whose second model call fails: some text, then
// the CLI's close-out message under the "<synthetic>" model id carrying
// the API error, then an is_error result with no result text — the
// shape the runner reads as stop reason "error" and core turns into
// the kind:turn-error marker.
func (e *engine) playError(ts *turnState) {
	callA := e.newCall(ts)
	e.text(callA, errorLead)
	e.assistant(e.newCall(ts), "<synthetic>", map[string]any{"type": "text", "text": errorDetail})
	e.result(ts, true)
}

// playSubagent dispatches one subagent through the Kivali MCP server
// named in --mcp-config, as a model calling mcp__kivali__subagent
// would, and reports the receipt as the tool result. When the server
// cannot be reached (no --mcp-config, or it will not start) the tool
// result says so as an error, which is what the real CLI shows the
// model when an MCP server is down.
func (e *engine) playSubagent(ts *turnState) {
	callA := e.newCall(ts)
	e.thinking(callA, "This needs a focused read of the test-reminder log. Hand it to a subagent.")
	toolID := fmt.Sprintf("toolu_fake_%03d_01", e.turn)
	input := map[string]any{"tasks": []map[string]any{{
		"description": subagentTask,
		"prompt":      "Read /files/project/mail-switch.md and summarise which reminders bounced and why, in three bullets.",
		"model":       "claude-haiku-4-5",
		"effort":      "low",
	}}}
	e.toolUse(callA, toolID, "mcp__kivali__subagent", input)
	out, isErr := callMCPTool(e.cfg.mcpConfig, "subagent", input)
	e.toolResult(toolID, out, isErr)
	e.seam(ts)
	if ts.interrupted {
		e.result(ts, false)
		return
	}
	e.text(e.newCall(ts), subagentFinal)
	e.result(ts, false)
}

func quote(s string) string {
	s = strings.TrimSpace(scenarioTag.ReplaceAllString(s, ""))
	if r := []rune(s); len(r) > 80 {
		s = string(r[:80]) + "…"
	}
	return "“" + s + "”"
}
