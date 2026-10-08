//go:build prompteval

package web

// Prompt evals for the two memory prompts — the episode digest and the
// rotation reconcile. Not unit tests: they call the real model and
// judge its output structurally (must-contain / must-not-contain), so
// a prompt change can be measured instead of eyeballed. The cases are
// the golden set from the Durable Agent Memory design: supersession,
// volatile status vs durable lesson, rule → principles, provenance,
// and episode title specificity.
//
// Run with:
//
//	go test -tags prompteval ./internal/web/ -run Eval -v
//
// The evals bill the claude CLI's own sign-in, and are
// skipped when it is not signed in. -v matters: every case logs the model's full
// output, and reading those is half the value. KIVALI_EVAL_MODEL
// overrides the reconcile model (default claude-sonnet-5); the episode
// eval always uses the writer's own model.

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/kivali-ai/kivali/internal/claudeagent"
	"github.com/kivali-ai/kivali/internal/provider"
	"github.com/kivali-ai/kivali/internal/store"
)

func evalClient(t *testing.T) provider.Client {
	t.Helper()
	// The Kivali MCP server the per-call path always registers is
	// pointed at a no-op binary: the evals never call a tool.
	d := claudeagent.New(claudeagent.Options{
		ClaudeBinary: "claude",
		KivaliBinary: "false",
		SessionDir:   t.TempDir(),
	})
	if st, err := d.Credentials().Status(context.Background()); err != nil || !st.Present {
		t.Skipf("the claude CLI is not signed in (%v); prompt evals need a real model", err)
	}
	return d
}

func evalModel() string {
	if m := os.Getenv("KIVALI_EVAL_MODEL"); m != "" {
		return m
	}
	return "claude-sonnet-5"
}

func user(text string) store.ChatMessage {
	return store.ChatMessage{Role: store.RoleReceived, Kind: "direct_chat", Content: text}
}

// said is the agent's own line. Not named after the speaker: `agent`
// is an import name elsewhere in this package.
func said(text string) store.ChatMessage {
	return store.ChatMessage{Role: store.RoleSent, Kind: "direct_chat", Content: text}
}
func toolCall(name, input string) store.ChatMessage {
	return store.ChatMessage{Role: store.RoleSent, Kind: "tool_use", ToolName: name, ToolInput: input, ToolUseID: "tu"}
}
func toolResult(text string) store.ChatMessage {
	return store.ChatMessage{Role: store.RoleReceived, Kind: "tool_result", Content: text, ToolUseID: "tu"}
}

func mustContainAll(t *testing.T, label, got string, wants []string) {
	t.Helper()
	low := strings.ToLower(got)
	for _, w := range wants {
		if !strings.Contains(low, strings.ToLower(w)) {
			t.Errorf("%s: missing %q", label, w)
		}
	}
}

func mustContainNone(t *testing.T, label, got string, wants []string) {
	t.Helper()
	low := strings.ToLower(got)
	for _, w := range wants {
		if strings.Contains(low, strings.ToLower(w)) {
			t.Errorf("%s: still contains %q", label, w)
		}
	}
}

// --- episode digest -----------------------------------------------------

type episodeCase struct {
	name         string
	msgs         []store.ChatMessage
	titleMust    []string
	titleMustNot []string
	bodyMust     []string
}

var episodeCases = []episodeCase{
	{
		name: "egress investigation gets a specific title and keeps identifiers",
		msgs: []store.ChatMessage{
			user("I'm getting reports that a change to the egress allow list did not take immediate effect in agent pods. Can you investigate?"),
			toolCall("run_shell", `{"command":"grep -rn copyDeadline cmd/egress-proxy/main.go"}`),
			toolResult("cmd/egress-proxy/main.go:43:\tcopyDeadline = 10 * time.Minute\ncmd/egress-proxy/main.go:134:\t_ = client.SetDeadline(time.Now().Add(copyDeadline))"),
			said("Additions propagate in about 1.1s. Removals don't: handleConnect at cmd/egress-proxy/main.go:101 checks the allowlist once at tunnel setup, and copyDeadline = 10 * time.Minute bounds how long an already-open CONNECT tunnel keeps working after a host is revoked. No test covers reload-while-tunnel-open."),
			user("Ok thanks, we'll fix it later."),
		},
		titleMust:    []string{"egress"},
		titleMustNot: []string{"debugging session", "investigation session", "status update", "discussion"},
		bodyMust:     []string{"copyDeadline", "cmd/egress-proxy/main.go"},
	},
	{
		name: "hiring chat names the role",
		msgs: []store.ChatMessage{
			user("Draft a role for a recruiter agent that owns sourcing and first-round screens. Reports to you."),
			toolCall("file_create", `{"path":"/files/artifacts/private/role-recruiter.md","content":"# Recruiter\n..."}`),
			toolResult("created"),
			said("Drafted the recruiter role at /files/artifacts/private/role-recruiter.md: owns sourcing, first-round screens, and the candidate pipeline doc; reports to chief-of-staff. Ready for propose_hire when you approve."),
		},
		titleMust: []string{"recruiter"},
		bodyMust:  []string{"role-recruiter.md"},
	},
	{
		name: "long chat keeps the conclusion through elision",
		msgs: func() []store.ChatMessage {
			var m []store.ChatMessage
			m = append(m, user("Let's work through the database upgrade options."))
			for i := 0; i < 300; i++ {
				m = append(m, said(fmt.Sprintf("Option note %d: %s", i, strings.Repeat("considering tradeoffs ", 20))))
			}
			m = append(m, user("Decision?"))
			m = append(m, said("Decided: migrate to Postgres 16 on 2026-10-01, with a dry run on the dev cluster the week before."))
			return m
		}(),
		titleMust: []string{"postgres"},
		bodyMust:  []string{"2026-10-01"},
	},
}

func TestEvalEpisodePrompt(t *testing.T) {
	c := evalClient(t)
	for _, tc := range episodeCases {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), episodeTimeout)
			defer cancel()
			ep, err := callEpisodeWith(ctx, c, claudeagent.DefaultSummaryModel, tc.msgs)
			if err != nil {
				t.Fatalf("callEpisodeWith: %v", err)
			}
			t.Logf("title:   %s\ntouched: %s\n%s", ep.title, ep.touched, ep.body)
			if ep.title == "" {
				t.Fatal("no title parsed — the model ignored the output shape")
			}
			if len(ep.title) > 100 {
				t.Errorf("title is %d chars; the prompt asks for at most 80", len(ep.title))
			}
			mustContainAll(t, "title", ep.title, tc.titleMust)
			mustContainNone(t, "title", ep.title, tc.titleMustNot)
			mustContainAll(t, "body", ep.body, tc.bodyMust)
		})
	}
}

// --- rotation reconcile -------------------------------------------------

// The rotation turn edits memory through tools. The eval approximates
// it without tools: the same reconcile prompt, the two current
// documents, the chat, and an instruction to output both documents in
// full. What is being measured is the prompt's rules — supersession,
// provenance, the volatile-status split, the rule → principles move,
// narrative eviction — not the tool mechanics, which the unit tests
// cover.
type reconcileCase struct {
	name              string
	priorHabits       string
	priorMemory       string
	msgs              []store.ChatMessage
	memoryMust        []string
	memoryMustNot     []string
	principlesMust    []string
	principlesMustNot []string
}

const evalTS = "20260918T120000.000000000Z"

var reconcileCases = []reconcileCase{
	{
		name:        "a contradicted fact is replaced, not joined by its successor",
		priorMemory: "- The web service listens on port 8080 [[stated]]\n- Deploys go through scripts/release.sh [[stated]]\n",
		msgs: []store.ChatMessage{
			user("Heads up: we moved the web service to port 9090 last week. Nothing listens on 8080 any more."),
			said("Noted — 9090 it is."),
		},
		memoryMust:    []string{"9090", "[[ep:" + evalTS + "]]", "scripts/release.sh"},
		memoryMustNot: []string{"listens on port 8080"},
	},
	{
		name:        "volatile status is dropped, the lesson attached to it survives",
		priorMemory: "- The TestJSONBodyCap timeout fix is on branch fix-timeout, NOT merged; main is red under -race. Lesson: grep for DATA RACE before blaming product code — a -race deadline miss is a wall-clock assumption. [[ep:20260910T000000.000000000Z]]\n",
		msgs: []store.ChatMessage{
			user("fix-timeout is merged to main as of this morning, race-test is green at f114f38."),
			said("Great, closing that out."),
		},
		memoryMust:    []string{"DATA RACE"},
		memoryMustNot: []string{"NOT merged", "main is red"},
	},
	{
		name:        "a rule filed as a fact moves to principles",
		priorMemory: "- Always run make lint before committing; CI is not a remote linter. [[stated]]\n- The prod namespace is kivali-team [[stated]]\n",
		msgs: []store.ChatMessage{
			user("thanks, all good for today"),
			said("Talk tomorrow."),
		},
		principlesMust: []string{"lint"},
		memoryMust:     []string{"kivali-team"},
		memoryMustNot:  []string{"make lint"},
	},
	{
		name:        "a new fact lands with this rotation's episode id",
		priorMemory: "- The prod namespace is kivali-team [[stated]]\n",
		msgs: []store.ChatMessage{
			user("run the tests please"),
			toolCall("run_shell", `{"command":"go test ./..."}`),
			toolResult("go: version mismatch: GOROOT points at 1.22 but go tool is 1.24"),
			said("GOROOT is stale in this shell; prefixing with GOROOT= fixes it. Tests pass with that."),
			user("good to know"),
		},
		memoryMust: []string{"GOROOT", "[[ep:" + evalTS + "]]"},
	},
	{
		// A story that is neither contradicted nor a rule would
		// otherwise survive every rotation. The episode it cites is
		// the record of what happened; memory keeps the conclusion
		// and the pointer.
		name:        "a narrative entry keeps its conclusion and episode pointer, loses the story",
		priorMemory: "- Release-process exercise, how it went: the CEO asked for a survey of release practices as onboarding; I wrote /files/background/plan.md, dispatched four research subagents, they fanned out to sixteen, and the CEO stopped the run to re-evaluate scope. Nothing was published. What I concluded: our four product lines (web app, mobile app, public API, data pipeline) differ mainly in reversibility, so release gates belong per class, never uniform. [[ep:20260910T000000.000000000Z]]\n- The prod namespace is kivali-team [[stated]]\n",
		msgs: []store.ChatMessage{
			user("nothing new today, please rotate"),
			said("Will do."),
		},
		memoryMust:    []string{"reversibility", "[[ep:20260910T000000.000000000Z]]", "kivali-team"},
		memoryMustNot: []string{"dispatched four", "fanned out to sixteen"},
	},
}

const reconcileEvalOutputShape = `

For this evaluation you have no tools. Instead of calling them, output the two reconciled documents in full, exactly in this shape and nothing else:

## agent_memory_habits.md
<the complete new principles file, or the word EMPTY>

## agent_memory.md
<the complete new agent memory file>`

func TestEvalReconcilePrompt(t *testing.T) {
	c := evalClient(t)
	for _, tc := range reconcileCases {
		t.Run(tc.name, func(t *testing.T) {
			principles := tc.priorHabits
			if strings.TrimSpace(principles) == "" {
				principles = "(empty)"
			}
			prompt := "Your current operating principles:\n```\n" + principles + "\n```\n\n" +
				"Your current agent memory:\n```\n" + tc.priorMemory + "\n```\n\n" +
				"The chat that is about to archive:\n" + renderEpisodeTranscript(tc.msgs)
			req := provider.CompleteRequest{
				Model:     evalModel(),
				MaxTokens: 2000,
				Purpose:   "memory_eval",
				System:    []provider.SystemBlock{{Text: memoryUpdateInstructionFor(evalTS) + reconcileEvalOutputShape}},
				Messages: []provider.Message{{
					Role:    provider.RoleUser,
					Content: []provider.ContentBlock{{Type: provider.ContentText, Text: prompt}},
				}},
			}
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
			defer cancel()
			resp, err := c.Complete(ctx, req)
			if err != nil {
				t.Fatalf("Complete: %v", err)
			}
			out := flattenTextContent(resp.Content)
			t.Logf("model output:\n%s", out)

			_, rest, ok := strings.Cut(out, "## agent_memory_habits.md")
			if !ok {
				t.Fatal("output lacks the principles section")
			}
			newPrinciples, newMemory, ok := strings.Cut(rest, "## agent_memory.md")
			if !ok {
				t.Fatal("output lacks the memory section")
			}
			mustContainAll(t, "memory", newMemory, tc.memoryMust)
			mustContainNone(t, "memory", newMemory, tc.memoryMustNot)
			mustContainAll(t, "principles", newPrinciples, tc.principlesMust)
			mustContainNone(t, "principles", newPrinciples, tc.principlesMustNot)
		})
	}
}
