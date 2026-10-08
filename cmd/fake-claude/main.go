// Command fake-claude stands in for the Claude Code CLI (`claude -p
// --input-format stream-json --output-format stream-json`) in the
// end-to-end suite and in local demos. It is installed on PATH under
// the name `claude`, so the agent runtime's claudeagent runner spawns
// it exactly as it spawns the real CLI.
//
// It speaks the subset of the stream-json protocol the runner parses
// (see internal/claudeagent/protocol.go): system/init with the fold
// capabilities, assistant messages carrying thinking, text and tool_use
// blocks, user messages carrying tool_result blocks, command_lifecycle
// frames for uuid-stamped inbound messages, control_response for the
// interrupt control_request, and a result frame with usage, modelUsage
// and the user_message_uuids ledger.
//
// What each turn does is a scripted scenario, never a model. The
// scenario comes from, in order: a `[fake:<name>]` tag in the user
// message (so one spec can pick a scenario per turn against a shared
// server), FAKE_CLAUDE_SCENARIO, then "reply". See scenarios.go.
//
// Everything is deterministic: ids, text, token counts and costs are
// fixed functions of the turn number. The only clocks are the pauses
// the `slow` and `fold` scenarios take so a Stop or a second message
// can arrive mid-turn; those are timers raced against stdin, so an
// interrupt ends a pause at once.
package main

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/kivali-ai/kivali/internal/claudeagent"
)

// authStatus is what `claude auth status --json` prints: signed in to a
// Claude subscription, in the real CLI's shape.
const authStatus = `{
  "loggedIn": true,
  "authMethod": "claude.ai",
  "apiProvider": "firstParty",
  "email": "fake-claude@example.com",
  "orgName": "Fake Claude",
  "subscriptionType": "max"
}`

func main() {
	if len(os.Args) >= 3 && os.Args[1] == "auth" && os.Args[2] == "status" {
		fmt.Println(authStatus)
		return
	}
	cfg, err := parseArgs(os.Args[1:], os.Getenv)
	if err != nil {
		fmt.Fprintln(os.Stderr, "fake-claude:", err)
		os.Exit(1)
	}
	if cfg.version {
		fmt.Println("2.1.281 (fake-claude)")
		return
	}
	out := bufio.NewWriter(os.Stdout)
	var trace io.Writer = os.Stderr
	if os.Getenv("FAKE_CLAUDE_TRACE") == "0" {
		trace = nil
	}
	e := newEngine(cfg, os.Stdin, out, trace)
	e.run()
	_ = out.Flush()
}

// config is everything the engine needs from argv and the environment.
type config struct {
	model        string
	resumeID     string
	systemPrompt string
	mcpConfig    string
	scenario     string
	version      bool

	// pause is how long `slow` waits between deltas; foldWait is how
	// long `fold` waits for a second message after its first delta, and
	// foldHold how long it then keeps that message queued before the
	// tool seam consumes it (so the page can show it pending).
	pause    time.Duration
	foldWait time.Duration
	foldHold time.Duration
}

// knownModel reports whether the real CLI would accept id for --model:
// every id the Kivali catalog prices (with or without a [1m] suffix or
// a dated snapshot suffix) plus the CLI's family aliases. Reading the
// catalog rather than a copy of it keeps the fake in step with every
// model pin the runner can send.
func knownModel(id string) bool {
	switch id {
	case "opus", "sonnet", "haiku", "fable", "default", "opusplan":
		return true
	}
	_, ok := claudeagent.New(claudeagent.Options{}).Resolve(id)
	return ok
}

// parseArgs reads the flags the runner passes and ignores the rest, the
// way the real CLI tolerates flags a given build does not use. Flags
// that take a value are listed so their value is not mistaken for a
// flag of its own.
func parseArgs(args []string, getenv func(string) string) (config, error) {
	cfg := config{
		scenario: getenv("FAKE_CLAUDE_SCENARIO"),
		pause:    envDuration(getenv, "FAKE_CLAUDE_PAUSE_MS", 4*time.Second),
		foldWait: envDuration(getenv, "FAKE_CLAUDE_FOLD_WAIT_MS", 30*time.Second),
		foldHold: envDuration(getenv, "FAKE_CLAUDE_FOLD_HOLD_MS", 2*time.Second),
	}
	if cfg.scenario == "" {
		cfg.scenario = scenarioReply
	}
	if !knownScenario(cfg.scenario) {
		return cfg, fmt.Errorf("unknown FAKE_CLAUDE_SCENARIO %q (want one of %s)", cfg.scenario, strings.Join(scenarioNames, ", "))
	}
	takesValue := map[string]bool{
		"--tools": true, "--output-format": true, "--input-format": true,
		"--mcp-config": true, "--resume": true, "--model": true, "--effort": true,
		"--system-prompt": true, "--append-system-prompt": true, "--allowedTools": true,
		"--disallowedTools": true, "--permission-mode": true, "--max-turns": true,
		"--add-dir": true, "--settings": true, "--fallback-model": true,
		"--session-id": true, "--agents": true,
	}
	for i := 0; i < len(args); i++ {
		a := args[i]
		name, inline, hasInline := strings.Cut(a, "=")
		value := func() string {
			if hasInline {
				return inline
			}
			if i+1 < len(args) {
				i++
				return args[i]
			}
			return ""
		}
		switch {
		case name == "--version" || name == "-v":
			cfg.version = true
		case name == "--model":
			cfg.model = value()
		case name == "--resume":
			cfg.resumeID = value()
		case name == "--system-prompt":
			cfg.systemPrompt = value()
		case name == "--mcp-config":
			cfg.mcpConfig = value()
		case takesValue[name]:
			_ = value()
		}
	}
	if cfg.model != "" && !knownModel(cfg.model) {
		// The real CLI's wording, so a log reader recognises it.
		return cfg, fmt.Errorf("model %q is not available: there's an issue with the selected model (%s). It may not exist or you may not have access to it", cfg.model, cfg.model)
	}
	if cfg.model == "" {
		cfg.model = claudeagent.DefaultAgentModel
	}
	return cfg, nil
}

func envDuration(getenv func(string) string, key string, def time.Duration) time.Duration {
	v := getenv(key)
	if v == "" {
		return def
	}
	ms, err := strconv.Atoi(v)
	if err != nil || ms < 0 {
		return def
	}
	return time.Duration(ms) * time.Millisecond
}
