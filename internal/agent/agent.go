// Package agent assembles the Claude call context for a single agent.
//
// This package knows how a Kivali agent's prompt is laid out — which
// pieces are cached, in what order, how inbox deliveries are rendered
// into chat messages, and which tools each agent has access to.
//
// The runtime (internal/release) composes this package on top of
// internal/provider and internal/store.
package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/kivali-ai/kivali/internal/files"
	"github.com/kivali-ai/kivali/internal/graph"
	"github.com/kivali-ai/kivali/internal/message"
	"github.com/kivali-ai/kivali/internal/owner"
	"github.com/kivali-ai/kivali/internal/provider"
	"github.com/kivali-ai/kivali/internal/store"
)

// attachmentReader is the narrow interface BuildRequest needs from the
// store to inline attachment text into outgoing messages. Kept small so
// this package doesn't have to pull the full FSStore into its import
// graph (simpler tests, clearer boundary).
type attachmentReader interface {
	ReadAttachmentText(sha string) (string, error)
	// AttachmentHasTextSidecar reports whether the attachment has a
	// canonical text file that's a SEPARATE file from the original
	// blob (true for PDFs, office docs, zip listings; false for text
	// uploads where canonical IS the blob, and false for images and
	// opaque binaries with no canonical at all). The render path
	// uses this to decide whether to advertise a `.txt` sidecar
	// path next to the original-name path under /files/attachments/.
	AttachmentHasTextSidecar(sha string) bool
	// AttachmentLinkNames returns sha → the filename that SHA is
	// actually reachable under in the agent's /files/attachments/.
	// The render path MUST quote these rather than the original
	// filename: when two distinct blobs arrive under one name, only
	// the first keeps the clean name and the rest are written with a
	// short-SHA prefix. Quoting the original name in that case points
	// the recipient at the wrong blob, and since both files parse
	// cleanly nothing downstream reports a problem.
	AttachmentLinkNames(slug string) (map[string]string, error)
}

// CEOSlug is the slug of the pseudo-agent representing the human user.
// CEO is never sent to Claude; its chat.jsonl is the queue view.
const CEOSlug = "ceo"

// ChiefOfStaffSlug is the slug of the bootstrap Chief of Staff agent.
// A handful of org-management tools (propose_role_update,
// read_agent_role) are gated to this slug — defense-in-depth on top
// of the tool-list filter that already excludes them from non-CoS
// agents.
const ChiefOfStaffSlug = "chief-of-staff"

// Context is everything needed to build a Claude request for one agent
// on one release. The zero value is not usable; callers populate it by
// reading from the store.
type Context struct {
	Agent          store.Agent
	IsChiefOfStaff bool

	Handbook string
	// Owner is what the team calls its person, read from the stored
	// name each turn. BuildRequest renders it as the system prompt's
	// owner section (owner.Term.Section), the only place the name
	// reaches an agent, so a rename lands on the next turn: the
	// runner respawns on system-prompt drift.
	Owner        owner.Term
	OrgChart     string
	ProjectFiles []store.ProjectFile
	Skills       []store.Skill // app-wide skills library (shared across agents)
	Role         string
	AgentMemory  string // optional per-agent semantic memory (agent_memory.md; carries forward through new-chat)
	// AgentHabits is the agent's habits (stored as
	// agent_memory_habits.md): the short list of learned rules of
	// behaviour rendered above semantic memory in the identity block.
	AgentHabits string

	ChatHistory []store.ChatMessage
	Inbox       []store.Message

	// FileText resolves a project-file SHA to its canonical text for
	// inlining into the cached system prompt.
	FileText func(sha string) (string, error)
	// Attachments resolves a chat-message attachment SHA to its text
	// canonical. If nil, attachments are listed by name only.
	Attachments attachmentReader
	// IncludeShellTool injects run_shell into the request's tool list.
	// The runtime sets it true; the direct-chat path leaves it
	// false (chat clears req.Tools anyway).
	IncludeShellTool bool
	// IncludeShareFileTool injects share_file. Set true on the chat
	// path so the agent can surface a file inline mid-conversation;
	// the release path can set it too — the resolver enforces the
	// agent's chat-history access set on the underlying SHA either way.
	IncludeShareFileTool bool
	// IncludeFilesystemTools injects the file_* tool family into the
	// request and sets the context-management beta header. When true,
	// the project-files section renders as a listing only (no inlined
	// content) and a new "Filesystem" block tells the agent where
	// things live.
	IncludeFilesystemTools bool
	// FilesystemAvailable mirrors IncludeFilesystemTools but is also true
	// for test and preview paths that want the listing rendered
	// without registering the tools. Callers
	// usually set both together.
	FilesystemAvailable bool

	// attachmentLinks is sha → the filename that SHA is actually
	// reachable under in this agent's /files/attachments/. Resolved
	// once per BuildRequest from the store and threaded through the
	// render helpers, rather than being a field construction sites
	// have to remember to populate — a construction site that forgot
	// would silently fall back to quoting original filenames, which
	// can open the wrong blob.
	attachmentLinks map[string]string

	Model       string
	Effort      string // reasoning level (claude effort); empty → transport default
	MaxTokens   int
	Temperature float64

	Purpose string // typically "release"
}

// FileTextFn builds a FileText callback that reads canonical text from
// the store, honoring the given context for PDF extraction side-effects.
func FileTextFn(ctx context.Context, s interface {
	ReadCanonicalText(ctx context.Context, sha string) (string, error)
}) func(string) (string, error) {
	return func(sha string) (string, error) {
		return s.ReadCanonicalText(ctx, sha)
	}
}

// BuildRequest produces a provider.CompleteRequest reflecting the context.
// System prompt layers are ordered stable-to-volatile. Layout:
//
//  1. Handbook
//  2. Org chart                            (invalidated on hire/offboard)
//  3. Project files (listing)              (invalidated on upload) — CACHED
//  4. Filesystem (file_*) section (if available)
//  5. Skills section (if any)
//  6. Agent identity: role + agent memory (per-agent) — CACHED
//
// Anthropic's prompt-caching API caps cache_control markers at 4 per
// request. The budget here is:
//
//   - end of shared-stable prefix (project files listing) — caches
//     [handbook + org chart + files listing] as one prefix.
//   - end of system prompt (agent identity) — caches the full system
//     prompt per-agent; invalidates on agent_memory rewrite or role
//     edit.
//   - last tool (in the tools block below) — caches the full tool-defs
//     block across releases.
//   - last message — caches the full conversation so tool-loop
//     iterations read their prior state from cache.
//
// Messages are chat.jsonl replayed + the current inbox rendered as user
// messages (with [INBOX ...] prefixes). Message attachments get their
// text canonicals inlined into the same message when available.
func (c Context) BuildRequest() provider.CompleteRequest {
	// Resolve the real /files/attachments/ filenames once, before any
	// message renders. c is a value receiver, so this only mutates our
	// own copy — which is the one the render helpers below read.
	c.attachmentLinks = c.resolveAttachmentLinks()

	req := provider.CompleteRequest{
		Model:       c.Model,
		Effort:      c.Effort,
		MaxTokens:   c.MaxTokens,
		Temperature: c.Temperature,
		Purpose:     c.Purpose,
		Agent:       c.Agent.Slug,
	}

	// System prompt carries only invariants: the handbook, the
	// filesystem (file_*) help, and the agent's own persona (role.md +
	// agent_memory.md). Mutating state — the org chart, open
	// assignments, project-files listing, and skills — is reached
	// through on-demand MCP tools (get_org_chart, assignment_list) and
	// filesystem listings under /files/. Models interpret
	// system-prompt blocks as invariant background facts, so live
	// state there would hide changes made within the current chat
	// (a newly hired agent would read as "pre-existing" after its own
	// hire approval). Tool_results, by contrast, read
	// as "snapshot I computed at time T" — the right semantics for
	// mutable state.
	req.System = append(req.System, provider.SystemBlock{Text: c.Handbook})
	req.System = append(req.System, provider.SystemBlock{Text: c.Owner.Section()})
	if c.FilesystemAvailable {
		req.System = append(req.System, provider.SystemBlock{Text: c.renderFilesystemSection()})
	}
	req.System = append(req.System, provider.SystemBlock{Text: renderPersonaIdentity(c.Agent, c.Role, c.AgentHabits, c.AgentMemory), Cache: true})

	req.Messages = append(req.Messages, c.chatHistoryToMessages()...)
	for _, d := range c.Inbox {
		// The Inbox slice carries an in-memory pending queue with no
		// relPath, so pass "" — the paraphrase omits the "at <path>"
		// clause when empty.
		body := RenderInboxBody(d, "")
		if att := c.renderAttachmentsInline(d.Attachments); att != "" {
			body = body + "\n\n" + att
		}
		req.Messages = append(req.Messages, provider.Message{
			Role:    provider.RoleUser,
			Content: []provider.ContentBlock{{Type: provider.ContentText, Text: body}},
		})
	}

	tools := message.ToolsFor(c.Agent.Slug, c.IsChiefOfStaff)
	req.Tools = message.AsClaudeTools(tools)
	// The shell tool is injected whenever it's wanted — today just for
	// release execution; direct-chat handlers explicitly clear req.Tools.
	if c.IncludeShellTool {
		req.Tools = append(req.Tools, ShellTool())
	}
	if c.IncludeShareFileTool {
		req.Tools = append(req.Tools, ShareFileTool())
	}
	if c.IncludeFilesystemTools {
		req.Tools = append(req.Tools, FilesystemTools()...)
		// Publishing is how a file leaves the workspace: the published
		// area is read-only to the file_* tools, the MCP server's order.
		req.Tools = append(req.Tools, ArtifactPublishTool(), ArtifactUnpublishTool())
		req.Betas = append(req.Betas, FilesystemBeta)
		// agent_memory_* tools ride with the /files/ tools: both
		// flow from the same "the agent should curate its own state"
		// affordance, and every current caller sets them together.
		req.Tools = append(req.Tools, AgentMemoryTools()...)
	}
	// Live-state lookup tools: exposed to every agent on every call.
	// These replace the org-chart / pending-work system-prompt blocks
	// that models tended to read as invariant background. CoS-only
	// tools (read_agent_role) are gated by IsChiefOfStaff.
	req.Tools = append(req.Tools, StateTools(c.IsChiefOfStaff)...)
	req.Tools = append(req.Tools, GraphTools()...)
	req.Tools = append(req.Tools, AssignmentTools()...)
	// Cache breakpoint on the last tool → every call after the first
	// reads the full tool-definitions block from prompt cache at 10%
	// of input rate. Invalidates only when we change the tool set
	// (hire CoS vs non-CoS, shell tool enable/disable).
	if n := len(req.Tools); n > 0 {
		req.Tools[n-1].Cache = true
	}
	// Cache breakpoint on the last message → within a tool-loop (release
	// or chat), each iteration's prior conversation is served from
	// cache. 5-min ephemeral TTL also covers rapid back-to-back releases.
	if n := len(req.Messages); n > 0 {
		blocks := req.Messages[n-1].Content
		if len(blocks) > 0 {
			blocks[len(blocks)-1].Cache = true
			req.Messages[n-1].Content = blocks
		}
	}
	return req
}

// RenderInboxBody formats a delivered message as the body of the
// user-role chat message the agent sees. Thin wrapper over
// BuildInboxView.RenderText so the UI bubble and the model-facing
// text both derive from the same View — schema changes to Message
// only need to update the one builder.
//
// The rendered text is a natural-language paraphrase: a one-sentence
// lead ("X sent you a task request titled 'Y' (at messages/…)."),
// a blank line, the body verbatim, and a parenthetical attachment
// advertisement. The on-disk Message retains all structured fields;
// only the text projected into the chat history narrates them.
//
// relPath is the relative path of the message being delivered
// (same string stored as ChatMessage.MessageRef). It's surfaced in
// the lead so the recipient can cite this exact path in their own
// follow-up via in_reply_to.
func RenderInboxBody(d store.Message, relPath string) string {
	return BuildInboxView(d, relPath).RenderText()
}

// OwnerLabel is what the org behind s calls its person in a label (see
// owner.Term.Label): their chosen name, else "CEO" or "Owner".
func OwnerLabel(s interface {
	ReadBranding() (store.Branding, error)
}) string {
	br, _ := s.ReadBranding()
	return br.Owner().Label()
}

// OrgChartMarkdown renders a bullet-tree org chart rooted at ceo, the
// person, labelled person (OwnerLabel). Any agents without a valid
// reports_to chain back to ceo are listed under "Unattached" so the
// person notices.
//
// The ceo pseudo-agent is excluded from the children map — it's the
// root of the tree, not a subordinate. Including it would create a
// self-cycle (ceo's reports_to is empty → mapped to "ceo" → ceo is its
// own child) and infinite recursion in writeChildren.
func OrgChartMarkdown(agents []store.Agent, person string) string {
	children := map[string][]store.Agent{}
	for _, a := range agents {
		if a.Slug == CEOSlug {
			continue
		}
		parent := a.ReportsTo
		if parent == "" {
			parent = CEOSlug
		}
		children[parent] = append(children[parent], a)
	}
	var b strings.Builder
	fmt.Fprintf(&b, "- ceo (%s, human)\n", person)
	writeChildren(&b, children, CEOSlug, 1)

	// Surface agents whose ancestry doesn't reach ceo (bad data).
	reached := map[string]bool{CEOSlug: true}
	var markReached func(slug string)
	markReached = func(slug string) {
		for _, c := range children[slug] {
			if reached[c.Slug] {
				continue
			}
			reached[c.Slug] = true
			markReached(c.Slug)
		}
	}
	markReached(CEOSlug)
	var orphans []store.Agent
	for _, a := range agents {
		if !reached[a.Slug] {
			orphans = append(orphans, a)
		}
	}
	if len(orphans) > 0 {
		b.WriteString("\nUnattached (a data problem: not reachable from ceo):\n")
		for _, o := range orphans {
			fmt.Fprintf(&b, "- %s (%s, reports_to=%q)\n", o.Slug, o.Role, o.ReportsTo)
		}
	}
	return b.String()
}

func writeChildren(b *strings.Builder, children map[string][]store.Agent, parent string, depth int) {
	kids := children[parent]
	for _, k := range kids {
		for i := 0; i < depth; i++ {
			b.WriteString("  ")
		}
		fmt.Fprintf(b, "- %s (%s)\n", k.Slug, k.Role)
		writeChildren(b, children, k.Slug, depth+1)
	}
}

// renderFilesystemSection introduces the /files/ directory layout so
// the model knows what's available without having to list everything
// at release start. Short by design — each subtree has a purpose, the
// model can file_view to learn more.
func (c Context) renderFilesystemSection() string {
	return `## Filesystem (/files/)

Distinct from your "Agent memory" block above (which is a curated summary inlined into every prompt and edited only via the agent_memory_* tools), you also have a file-backed virtual filesystem rooted at /files/, edited via the file_* tools:

**Read-only:**
- /files/project/                    — project files uploaded by the owner.
- /files/past-chats/                 — your own archived chat generations.
- /files/episodes/<ts>.md            — one digest per archived generation: what was asked, done, concluded, left open. Grep here before opening a raw transcript, and when an agent-memory entry cites [[ep:<ts>]].
- /files/skills/<name>/              — shared org skills. Each skill is a directory; read /files/skills/<name>/SKILL.md first, then browse the rest of the dir for scripts and assets. Run scripts directly with run_shell (e.g. bash /files/skills/<name>/scripts/foo.sh).
- /files/attachments/                — files attached to recent chat/inbox messages.
- /files/artifacts/public/           — what you have published for other agents to read, visible to them as /files/artifacts/shared/<your-slug>/<name>. Only artifact_publish and artifact_unpublish change it.
- /files/artifacts/shared/<slug>/    — every agent's published artifacts, by owner.

**Read/write — your workspace** (these two, and anything else you create under /files/ outside the read-only list above):
- /files/artifacts/private/  — drafts, working notes, anything only you (and the owner) should see; publish from here. Other agents cannot read it.
- /files/background/        — your background workspace, shared with your subagents and only them: the plan for one fan-out (plan.md) and what its workers produce. Other agents cannot see it.

Use the file_* tools (file_view, file_create, file_str_replace, file_insert, file_delete, file_rename, file_copy) to build and organize artifacts. Most artifacts fit in a single file_create call; for very long messages that would exceed the per-response output budget, draft a skeleton first and extend section-by-section with file_str_replace / file_insert. The tool-loop will call you back with each result, so iterative builds are natural when needed.

**When to use artifacts vs Agent memory:** artifacts hold standalone messages (role drafts, specs, reports, research). Agent memory holds your curated running summary. Don't duplicate — point at the artifact from memory if it matters long-term.

**run_shell shares this filesystem.** The file_* tools and run_shell run in the same sandbox with the same privileges, so they see the same files and neither can write a read-only path. Bash reads /files/foo.csv directly and writes to /files/artifacts/private/out.txt directly — the next file_view sees the result. cwd defaults to /files/; pass a relative cwd to start somewhere else under that root.

**Publishing.** Nothing you write is visible to other agents until you publish it: artifact_publish copies a file or directory from your workspace into /files/artifacts/public/, and artifact_unpublish removes one. To send a file to another agent, publish it and cite /files/artifacts/shared/<your-slug>/<path>, or attach it to a message by its /files/artifacts/private/ path (a file elsewhere in your workspace, such as /files/background/, is copied there first).

` + renderGraphSection()
}

// renderGraphSection is the knowledge-graph paragraph of the
// filesystem help: what a node is, the kinds, and the two read tools.
// It ships in the binary, so it reaches every install on deploy; the
// handbook carries the full rules but is seeded once and needs a
// splice to catch up. Kinds are read from graph.Kinds so this text
// cannot fall behind the allow list.
func renderGraphSection() string {
	kinds := make([]string, 0, len(graph.Kinds))
	for _, k := range graph.Kinds {
		kinds = append(kinds, string(k))
	}
	return `**Knowledge graph.** Every published file (yours under /files/artifacts/public/, peers' under /files/artifacts/shared/<owner>/) and every project file is a node in the org's knowledge graph; agents are nodes too. A markdown file there with YAML front matter carries the node's id (stable across renames; without one, the path is the name), kind (one of: ` + strings.Join(kinds, ", ") + `, or none for authored primary material), about (the node this concerns), status (draft | provisional | current | withdrawn; current when absent; provisional requires a condition), condition, depends_on, supersedes, file (one payload beside it), check, source, evidence and summary. Decisions and requirements are nodes, one per file, a few lines each; a message about one names its id instead of restating it. Find nodes with ` + GraphQueryToolName + ` (by owner, about, kind, status) and inspect one with ` + GraphNodeToolName + ` (edges both ways, versions, the file_view path to its body). Each kind's rule is in ` + GraphQueryToolName + `'s description; the handbook's "The knowledge graph" section, where present, has the longer form. The index checks the rules when you publish: artifact_publish's reply gives each file's node id, version, status and anything the index could not accept. Your next wake reports what changed in the graph since your last turn.`
}

// resolveAttachmentLinks asks the store for this agent's sha → on-disk
// attachment filename map. Best-effort: on any error (or a store that
// doesn't implement the lookup, as in older test fakes) we return nil
// and the render path falls back to the original filename — the same
// behavior as before, which is wrong only in the collision case and
// is the best guess available when the store can't be consulted.
func (c Context) resolveAttachmentLinks() map[string]string {
	if c.Attachments == nil || c.Agent.Slug == "" {
		return nil
	}
	links, err := c.Attachments.AttachmentLinkNames(c.Agent.Slug)
	if err != nil {
		return nil
	}
	return links
}

// attachmentLinkFor returns the /files/attachments/ filename that
// actually resolves to THIS attachment's bytes, given the store's
// sha → on-disk name map.
//
// The store may have written it under a short-SHA prefix because an
// earlier, distinct blob already holds the clean name. Quoting the
// clean name there hands the recipient a valid, parseable, WRONG
// file, and nothing downstream can tell: the read succeeds, the
// structure matches, only the bytes are stale. So the map wins
// whenever it has an entry, and the original filename is only a
// fallback for SHAs the store couldn't resolve.
func attachmentLinkFor(links map[string]string, a store.MessageAttachment) string {
	if link, ok := links[a.SHA]; ok && link != "" {
		return link
	}
	return files.AttachmentLinkName(a.Name)
}

// AttachedFilesLines is the block that tells the model where a
// delivered message's files are: one /files/attachments/ path per
// attachment, and a .txt line beside it when sidecar says the text
// was extracted to a separate file. links is the store's sha → on-disk
// name map (attachmentReader.AttachmentLinkNames); a SHA it lacks
// falls back to the sanitised original name. Empty when there is
// nothing attached.
//
// Exported because the prompt is not the only place a message reaches
// the model: one folded into a running turn goes as text, and it has
// to name its files the same way the prompt would have.
func AttachedFilesLines(atts []store.MessageAttachment, links map[string]string, sidecar func(sha string) bool) string {
	if len(atts) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("--- attached files ---\n")
	for _, a := range atts {
		link := attachmentLinkFor(links, a)
		fmt.Fprintf(&b, "/files/attachments/%s\n", link)
		if sidecar != nil && sidecar(a.SHA) {
			fmt.Fprintf(&b, "/files/attachments/%s.txt (text extracted from %s)\n", link, a.Name)
		}
	}
	return b.String()
}

// renderAttachmentsInline annotates an inbox delivery or received
// chat message with its attachments.
//
// When the file_* tools are available (the common case), we emit a
// list of full /files/attachments/ paths — one per line. Each path
// is the name the file was actually written under (see
// attachmentLink), not necessarily the sender's filename, so it
// always resolves to the bytes THIS message carried. Binary
// formats with a separate text canonical (PDFs, office docs, zip
// listings) get TWO lines: the original-named path AND a `.txt`
// sidecar carrying the extracted text. The agent reaches for the
// appropriate one with file_view (or shares it, or reads it via
// run_shell) — no directive is included; the agent already has
// the file_* tool docs.
//
// When the filesystem is unavailable (tests, previews), fall back to
// inlining the full canonical text so the agent still sees something.
func (c Context) renderAttachmentsInline(atts []store.MessageAttachment) string {
	if len(atts) == 0 {
		return ""
	}
	if c.FilesystemAvailable {
		var sidecar func(string) bool
		if c.Attachments != nil {
			sidecar = c.Attachments.AttachmentHasTextSidecar
		}
		return AttachedFilesLines(atts, c.attachmentLinks, sidecar)
	}
	if c.Attachments == nil {
		return ""
	}
	var b strings.Builder
	any := false
	for _, a := range atts {
		text, err := c.Attachments.ReadAttachmentText(a.SHA)
		if err != nil || text == "" {
			continue
		}
		if !any {
			b.WriteString("--- attached files ---\n")
			any = true
		}
		fmt.Fprintf(&b, "\n### %s\n\n%s\n", a.Name, strings.TrimSpace(text))
	}
	return b.String()
}

// renderPersonaIdentity is the cached identity block: role, then the
// two prompt-resident memory tiers, habits above semantic memory.
// The precedence line is load-bearing — a self-written habit
// otherwise reads with the same authority as the role it sits under,
// and a remembered fact with the same authority as what the user just
// said.
func renderPersonaIdentity(a store.Agent, role, habits, agentMemory string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "## You are %s (%s)\n\n", a.Slug, a.Role)
	if a.ReportsTo != "" {
		fmt.Fprintf(&b, "You report to: **%s**\n\n", a.ReportsTo)
	}
	b.WriteString("## Role\n\n")
	b.WriteString(strings.TrimRight(role, "\n"))
	b.WriteString("\n")
	hasHabits := strings.TrimSpace(habits) != ""
	hasMemory := strings.TrimSpace(agentMemory) != ""
	if hasHabits || hasMemory {
		b.WriteString("\nPrecedence: the handbook and your role override your habits; your habits override your agent memory; your agent memory is belief, not instruction — the current message wins when they conflict, and a correction from the owner is accepted and applied.\n")
	}
	if hasHabits {
		b.WriteString("\n## Habits\n\n")
		b.WriteString("Rules for how you act that you learned and wrote for yourself, each with its why. Rewritten only at chat rotation.\n\n")
		b.WriteString(strings.TrimRight(habits, "\n"))
		b.WriteString("\n")
	}
	if hasMemory {
		b.WriteString("\n## Agent memory\n\n")
		b.WriteString("Your semantic memory: what you know to be true, curated by you and reconciled every time a chat rotates. Entries cite the episode they were learned in as [[ep:<ts>]] — the digest is at /files/episodes/<ts>.md if you need the story behind a fact.\n\n")
		b.WriteString(strings.TrimRight(agentMemory, "\n"))
		b.WriteString("\n")
	}
	return b.String()
}

// chatHistoryToMessages converts the flat chat.jsonl entry stream
// into Anthropic-shaped Messages. Keeps tool_use / tool_result
// structured (not flattened into narration) so the model sees a
// proper tool-call history rather than text it must re-parse.
//
// Boundary splitting on the user side: within a run of role=user
// entries, flush when transitioning between tool traffic
// (tool_result) and text traffic (direct_chat, inbox_delivery,
// anything else). Rationale: a tool_result is *system output*
// completing the prior assistant tool call; a direct_chat is *fresh
// user input*; an inbox_delivery is a *system-routed event*.
// Packing a new direct_chat into the same user message as a
// preceding tool_result reads to the model like commentary on the
// tool_result, which leads the model to treat user directives as
// extensions of a prior flow rather than new directives. Consecutive same-role messages are legal in the
// Messages API; this split is purely a semantic boundary and it's
// limited to the user side — on the assistant side, text
// immediately before a tool_use is the model's reasoning about what
// it's about to call, which IS one logical step and should stay
// merged.
//
// Received messages with text attachments get the canonical text
// inlined after the body so the recipient sees the content rather
// than just the filename. Sent messages don't inline attachments
// (the author already knows what they sent).
func (c Context) chatHistoryToMessages() []provider.Message {
	var out []provider.Message
	var curRole provider.Role
	var curBlocks []provider.ContentBlock
	// curUserBucket tracks whether the current user-role message is
	// holding tool_results or text, so we can split when that
	// changes. Only consulted when curRole == RoleUser; "" means
	// the current message is empty or we're on an assistant run.
	var curUserBucket string
	// pendingNotice accumulates KindUserInterruption / KindRuntimeDisruption
	// content. Attached as a prefix to the next text-bucket user
	// message we add (skipping tool_result-bucket messages — the API
	// rejects mixed text+tool_result content blocks for the model
	// context most callers want). Consumed once and cleared.
	var pendingNotice string
	// dangling is every tool_use id the assistant emitted that has no
	// tool_result yet. The API requires each tool_use to be answered
	// by a tool_result in the very next user message; a turn that died
	// mid-tool (Stop, a runtime disruption) leaves ids unanswered, and
	// the next text the CEO or the runtime appends would otherwise
	// follow the tool_use directly and be rejected. Before a text
	// message opens after such a gap, the ids are closed with a
	// synthetic result that says the call was interrupted.
	var dangling []string

	flush := func() {
		if len(curBlocks) == 0 {
			return
		}
		out = append(out, provider.Message{Role: curRole, Content: curBlocks})
		curBlocks = nil
		curUserBucket = ""
	}
	closeDangling := func() {
		if len(dangling) == 0 {
			return
		}
		blocks := make([]provider.ContentBlock, 0, len(dangling))
		for _, id := range dangling {
			blocks = append(blocks, provider.ContentBlock{
				Type:              provider.ContentToolResult,
				ToolResultID:      id,
				ToolResultContent: "[the turn was interrupted before this tool call returned; no result was recorded]",
				ToolResultIsError: true,
			})
		}
		out = append(out, provider.Message{Role: provider.RoleUser, Content: blocks})
		dangling = nil
	}

	userBucketOf := func(m store.ChatMessage) string {
		if m.Kind == "tool_result" {
			return "tool"
		}
		return "text"
	}

	for _, m := range c.ChatHistory {
		// doc_published and shell_captured entries are side-effect
		// rows the MCP handlers persist so the chat history carries
		// the SHAs that gate share_file / publish_*. The model already
		// sees the same info in the paired tool_result; skip on
		// serialization to avoid redundant tokens.
		if m.Kind == "doc_published" || m.Kind == "shell_captured" {
			continue
		}
		// System boundary markers (user-interruption, runtime-disruption,
		// turn-error, paused-to-deliver): don't project as standalone
		// user-text messages — that would break the API's tool-pair
		// invariant when
		// handleAgentStop's sync-write lands between a tool_use and its
		// tool_result. Defer the marker's content as a pendingNotice and
		// prepend it to the next text-bucket user message we'd construct.
		// That way the model still sees the boundary text in conversation
		// context (and on the SDK path, latestUserMessage carries the
		// prepended notice into stdin — overriding the CLI's "Continue
		// from where you left off" meta-prompt that --resume injects
		// after a kill).
		if m.Kind == store.KindUserInterruption || m.Kind == store.KindRuntimeDisruption || m.Kind == store.KindTurnError || m.Kind == store.KindPausedToDeliver {
			if pendingNotice != "" {
				pendingNotice += "\n"
			}
			pendingNotice += "[" + m.Content + "]"
			continue
		}
		role := provider.RoleUser
		if m.Role == store.RoleSent {
			role = provider.RoleAssistant
		}
		if len(curBlocks) > 0 {
			if role != curRole {
				flush()
			} else if role == provider.RoleUser {
				if b := userBucketOf(m); b != curUserBucket {
					flush()
				}
			}
		}
		// A text-bucket user message about to open while tool_use ids
		// are still unanswered: close them first so the pairing holds.
		if role == provider.RoleUser && len(curBlocks) == 0 && userBucketOf(m) == "text" {
			closeDangling()
		}
		curRole = role
		if role == provider.RoleUser {
			curUserBucket = userBucketOf(m)
		} else {
			curUserBucket = ""
		}

		switch m.Kind {
		case "tool_use":
			input := json.RawMessage(m.ToolInput)
			if len(input) == 0 {
				// Defensive: older entries without ToolInput get an
				// empty-object stub so the block is still valid.
				input = json.RawMessage("{}")
			}
			curBlocks = append(curBlocks, provider.ContentBlock{
				Type:      provider.ContentToolUse,
				ToolUseID: m.ToolUseID,
				ToolName:  m.ToolName,
				ToolInput: input,
			})
			if m.ToolUseID != "" {
				dangling = append(dangling, m.ToolUseID)
			}
		case "tool_result":
			curBlocks = append(curBlocks, provider.ContentBlock{
				Type:              provider.ContentToolResult,
				ToolResultID:      m.ToolUseID,
				ToolResultContent: m.Content,
				ToolResultIsError: m.IsError,
			})
			dangling = removeID(dangling, m.ToolUseID)
		default:
			text := m.Content
			if m.Role == store.RoleReceived {
				if att := c.renderAttachmentsInline(m.Attachments); att != "" {
					text = text + "\n\n" + att
				}
			}
			if pendingNotice != "" && role == provider.RoleUser {
				text = pendingNotice + "\n\n" + text
				pendingNotice = ""
			}
			curBlocks = append(curBlocks, provider.ContentBlock{
				Type: provider.ContentText,
				Text: text,
			})
		}
	}
	flush()
	// A boundary marker with nothing after it is the boot-recovery
	// shape: RecoverInterruptedTurns appends the disruption and spawns
	// at once. Held only as a prefix for text that never comes, the
	// notice would vanish and the SDK path would feed the CLI the last
	// message the agent already answered as its resume prompt. So it
	// becomes the trailing user message itself, after any tool_use it
	// interrupted has been answered.
	if pendingNotice != "" {
		closeDangling()
		out = append(out, provider.Message{Role: provider.RoleUser, Content: []provider.ContentBlock{{Type: provider.ContentText, Text: pendingNotice}}})
	}
	return out
}

// removeID drops one id from a list of dangling tool_use ids.
func removeID(ids []string, id string) []string {
	out := ids[:0]
	for _, x := range ids {
		if x != id {
			out = append(out, x)
		}
	}
	return out
}
