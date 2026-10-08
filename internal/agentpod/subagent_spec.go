package agentpod

import (
	"strconv"
	"strings"

	"github.com/kivali-ai/kivali/internal/provider"
)

// MaxSubagentDepth is how many tiers of subagent may exist below a
// durable agent. 1 is the agent's own subagents; 2 lets one of those
// act as a sub-lead with workers of its own.
//
// Two, not three. Sub-leads are the whole point — they are what turns
// a flat batch into work that has been broken down — and one tier of
// them is enough for any task an agent is plausibly given. A fourth
// tier would multiply the pod's process count again for no structural
// gain, and each tier is a summary of a summary: an error introduced
// at the bottom is laundered through one more retelling before anyone
// who can check it sees it.
const MaxSubagentDepth = 2

// SubagentMaxNestedFanout caps how many workers a sub-lead may
// dispatch in one call. Smaller than the durable agent's batch cap on
// purpose: a sub-lead BLOCKS on its workers (see SubagentSystemPrompt),
// so every one of them holds a slot in the same pod while their lead
// holds one too.
const SubagentMaxNestedFanout = 3

// CanDelegate reports whether a subagent at this depth is allowed to
// dispatch subagents of its own.
func CanDelegate(depth int) bool { return depth < MaxSubagentDepth }

// SubagentTools returns the tool set a subagent at the given depth
// runs with: the kivali MCP tools by bare name, plus the provider-side
// web tools. The subagent tool is present only for a tier that is
// allowed to delegate; a leaf is never shown a tool it would be refused
// for using.
//
// This is policy — which tools a tier gets — and so it lives here, in
// neutral names. How a provider spells them is the driver's business.
func SubagentTools(depth int) provider.ToolSet {
	kivali := []string{
		"file_view",
		"file_create",
		"file_str_replace",
		"file_insert",
		"file_delete",
		"file_rename",
		"file_copy",
		"artifact_publish",
		"artifact_unpublish",
		"list_skills",
		"graph_query",
		"graph_node",
		"run_shell",
	}
	if CanDelegate(depth) {
		kivali = append(kivali, "subagent")
	}
	return provider.ToolSet{
		Kivali:   kivali,
		Builtins: []string{provider.BuiltinWebFetch, provider.BuiltinWebSearch},
	}
}

// SubagentSystemPrompt builds the minimal-by-design system prompt the
// subagent gets. NO handbook, NO parent persona, NO org context
// — just the frame the subagent needs to do focused work and return
// a clean answer.
//
// Post docs/developers/files-and-publishing.md, the subagent inherits a narrowed view
// of the parent's /files/ via a symlink overlay: artifacts/private/
// is the subagent's own scratch, the rest of the namespace mirrors
// what the parent could file_view (project/, skills/, attachments/,
// artifacts/public/, artifacts/shared/, background/). file_view paths
// and shell paths point at the same physical bytes — no
// inputs[]/outputs/ shuffle, no path translation between tools.
func SubagentSystemPrompt(parent, description string, depth int) string {
	var b strings.Builder
	// EVERYTHING FROM HERE TO THE TAIL IS IDENTICAL FOR EVERY SUBAGENT
	// AT THIS TIER, and it needs to stay that way.
	//
	// Prompt caching keys on a shared leading prefix. The caller slug
	// and the task label change on every spawn, so they sit in a tail
	// below: opening with them would put ~1000 static tokens behind
	// text that never repeats, and a batch of five would pay for the
	// whole thing five times.
	//
	// Anything per-spawn added above this line silently un-caches the
	// rest. Add it to the tail instead.
	b.WriteString("You are a subagent completing one focused task for a Kivali agent.\n\n")
	b.WriteString("Your /files/ is a narrowed view of your caller's /files/:\n")
	b.WriteString("- /files/artifacts/private/  — your own scratch + deliverables. Persists for your caller to read after you finish.\n")
	b.WriteString("- /files/artifacts/public/   — your caller's published files (read-only; see artifact_publish).\n")
	b.WriteString("- /files/artifacts/shared/<slug>/ — every agent's published files (read-only).\n")
	b.WriteString("- /files/project/            — shared project files (read-only).\n")
	b.WriteString("- /files/skills/             — shared org skills (read-only).\n")
	b.WriteString("- /files/attachments/        — your caller's attachments by name (read-only).\n")
	b.WriteString("- /files/background/         — SHARED and WRITABLE. Everyone working on this job sees the same directory, including whoever is assembling the final answer.\n\n")
	b.WriteString("Put anything another worker or your caller needs in /files/background/, in your OWN file named for your task. ")
	b.WriteString("If /files/background/plan.md exists, read it for what the whole job is and which part is yours — but do not edit it: ")
	b.WriteString("your caller maintains it, and concurrent writes to a shared file silently lose one side's changes.\n\n")
	b.WriteString("file_view, file_create, etc. address paths as /files/artifacts/private/note.md. ")
	b.WriteString("run_shell runs against the SAME filesystem — `cat /files/project/foo.csv` works directly, ")
	b.WriteString("and `echo hi > /files/artifacts/private/out.txt` is what file_view sees on the next call. ")
	b.WriteString("cwd defaults to /files/; you can pass a cwd to start somewhere else under that root.\n\n")
	if CanDelegate(depth) {
		b.WriteString("You have run_shell, file_*, artifact_publish, artifact_unpublish, list_skills, graph_query, graph_node, WebFetch, WebSearch, and `subagent`.\n")
	} else {
		b.WriteString("You have run_shell, file_*, artifact_publish, artifact_unpublish, list_skills, graph_query, graph_node, WebFetch, WebSearch.\n")
	}
	b.WriteString("graph_query and graph_node search the org's knowledge graph (every public artifact and agent, with kind, status, owner, subject); graph_query about=<id> binding=true finds what binds an object; graph_node <id>@N returns the file at version N.\n")
	b.WriteString("artifact_publish copies a file from your workspace into your caller's published area, where every agent can read it; publish only when your task says to.\n")
	b.WriteString("You do NOT have publish_*, agent_memory_*, share_file, past-chats, or org-state tools — those are caller-only.\n\n")
	b.WriteString(subagentDelegationFrame(depth))
	b.WriteString("IF YOU ARE BLOCKED: you cannot ask anyone anything — one prompt in, nothing can reach you, and you are gone once you answer. ")
	b.WriteString("So either decide (take the most defensible reading and state the assumption in your answer), ")
	b.WriteString("or stop early and make the question your answer — \"I cannot do this without knowing X\" is a useful result, not a failure, ")
	b.WriteString("and your caller can get the answer and re-dispatch in seconds. Never guess silently.\n\n")
	b.WriteString("RESULT CONTRACT: your final assistant message is what gets returned to your caller verbatim. ")
	b.WriteString("Don't write a preamble. Don't summarize what you did. Don't say \"here's the answer:\". ")
	b.WriteString("Just write the answer they need — directly, in full. Files you wrote under /files/artifacts/private/ ")
	b.WriteString("are reachable by your caller via the same path; ")
	b.WriteString("don't re-quote their contents in your final message unless the caller's request specifically asked you to.\n\n")
	b.WriteString("LARGE ANSWERS: if your final answer would run more than a few KB (long tables, full file dumps, multi-section reports), ")
	b.WriteString("write it to /files/artifacts/private/<name>.md and have your final message be a short pointer to that path plus the headline finding. ")
	b.WriteString("The caller's runtime caps tool_results, so a huge inline answer becomes unreadable.\n\n")
	b.WriteString("EDITING FILES EFFICIENTLY: when updating an existing file, use file_str_replace or file_insert with a small diff. ")
	b.WriteString("Reach for file_create only when creating a file from scratch. The reason: the tool input lives in your ")
	b.WriteString("chat history and replays on every later turn — a file_create of a 60 KB body costs 60 KB on every future API ")
	b.WriteString("call, while the equivalent file_str_replace is a few hundred bytes. Consolidate updates from one work-burst ")
	b.WriteString("into one file_str_replace. For a long artifact, start with a small file_create and grow it with ")
	b.WriteString("file_str_replace calls rather than dumping it via one giant file_create. The same logic applies to inline run_shell ")
	b.WriteString("scripts: long heredocs replay forever just like file_create bodies — when the content is shell-derivable (curl, ")
	b.WriteString("filtering, generation), let run_shell produce it directly into the target path instead of staging it inline.\n")

	// The per-spawn tail. Everything above is a stable, cacheable
	// prefix; these two facts are the only things that differ between
	// one subagent and the next, so they go last. Landing at the end
	// also puts them closest to the user prompt that follows.
	b.WriteString("\nYou were spawned by `")
	b.WriteString(parent)
	b.WriteString("`")
	if description != "" {
		b.WriteString(" for this task: ")
		b.WriteString(description)
	}
	b.WriteString(".\n")
	return b.String()
}

// subagentDelegationFrame is the paragraph that makes a subagent's
// tier its role. There is no membership object, no lead/worker type,
// no registry of background jobs — a subagent IS a sub-lead exactly when it is
// shallow enough to delegate, and it learns that from the only place
// it could matter: its own instructions.
//
// The two branches differ in more than tone. A sub-lead's dispatch
// BLOCKS, which is the opposite of the durable agent's contract, and
// the reason is structural rather than stylistic: a subagent is a
// one-shot run (provider.Client.RunSubagent) fed its prompt once. It cannot
// be woken, so there is no later moment at which a result could reach
// it. Ending its turn with work outstanding would orphan that work.
// Saying so plainly is what stops a sub-lead from copying the
// fire-and-forget pattern it may have seen described elsewhere.
func subagentDelegationFrame(depth int) string {
	if !CanDelegate(depth) {
		// A leaf is told it is a leaf. Left unsaid, a model given a
		// large task and no delegation tool tends to either attempt
		// the whole thing badly or report back asking for help it
		// cannot be given.
		return "SCOPE: you do the work yourself. You cannot dispatch subagents from here — you are the last tier. " +
			"If the task is bigger than one agent should take on, do the most valuable part completely, " +
			"then say plainly in your final message what you did not cover and what it would take.\n\n"
	}

	// Everything operational about nested dispatch — that it blocks,
	// that workers see only their prompt, that their answers are raw
	// material — lives in the tool's own description, which is in this
	// subagent's context whenever this paragraph is. Saying it twice
	// costs tokens on every spawn and gives the two copies somewhere
	// to disagree. This says only what the tool description cannot:
	// that splitting is permitted here at all, and when it is worth it.
	return "DELEGATING: you may split this task across up to " +
		strconv.Itoa(SubagentMaxNestedFanout) +
		" workers with `subagent` (see its description — it blocks, and you own reconciling what comes back). " +
		"Worth it when the task has genuinely independent pieces; not worth it for anything you could finish " +
		"in the time it takes to write the instruction.\n\n"
}
