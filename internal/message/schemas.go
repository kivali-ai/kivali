package message

import (
	"encoding/json"

	"github.com/kivali-ai/kivali/internal/roleicon"
	"github.com/kivali-ai/kivali/internal/store"
)

// Tool descriptions + JSON Schemas for the publish_* and propose_*
// family. Wire shape:
//
//   - Messages have a short capped `body` (publish_*) or `rationale`
//     (propose_*). Cap is MaxMessageBodyBytes (4096); the parser
//     rejects oversized fields with a "split the substance into an
//     attachment / artifact" hint. There is NO body_path / no
//     rationale_path — substantive content goes elsewhere.
//   - Substantive content goes via `attachments` (a list of /files/
//     paths the server reads and content-addresses) or, for the
//     artifact-update tools (propose_role_update,
//     propose_handbook_update, hire), a dedicated `*_path` field
//     pointing at a /files/ artifact the agent drafted first.
//
// The shared attachmentsSchemaFragment defines the attachment shape
// once. See handbook §How you communicate for the runtime
// pattern; here we surface only what the parser enforces.

// attachmentsSchemaFragment is the JSON Schema fragment for the
// shared optional `attachments` field. Path-only: every attachment
// is a /files/ path the agent already has visibility into; the
// server reads the bytes at publish time and content-addresses
// them. There is no inline content, base64 or zip-bundling: the
// agent writes the bytes under /files/artifacts/private/ first
// (file_create or run_shell) and attaches by path. See handbook
// §Attachments.
const attachmentsSchemaFragment = `"attachments": {
      "type": "array",
      "description": "Files to attach. Each item references a file already on disk by /files/ path; the server reads the bytes at publish time. To attach text or binary you produced this turn, write it via file_create or run_shell first, then attach by path.",
      "items": {
        "type": "object",
        "properties": {
          "path": {"type": "string", "description": "/files/ path under artifacts/private/ or artifacts/public/ (your own), or /files/project/<name> or /files/attachments/<name>"},
          "name": {"type": "string", "description": "optional filename override; defaults to the basename of the path"}
        },
        "required": ["path"]
      }
    }`

// bodyDescription is reused across publish_* tools so the cap rule
// is described identically everywhere.
const bodyDescription = "free-form markdown body — short by design (cap 4096 bytes). For substantive content, attach a /files/ artifact via attachments[]; do not paste reports or long analyses into the body."

// noticeBodyDescription is the notice-specific body cap. Tighter than
// bodyDescription (2048 vs 4096): a tell that needs four kilobytes of
// prose is a report, and reports belong in an attachment.
const noticeBodyDescription = "free-form markdown body — short by design (cap 2048 bytes). For substantive content, attach a /files/ artifact via attachments[]; a pointer to it in the body is enough."

// rationaleDescription is reused across propose_* tools (CoS-only).
const rationaleDescription = "short explanation for the owner: what's changing, why, what's expected to improve. Cap 4096 bytes — keep it tight; the artifact path carries the actual content."

// noticeTool is the one agent-to-agent speech act: a tell. The
// description is the load-bearing half of the feature — the
// affordances have to make the reply loop impossible rather than
// merely discouraged, so the tool says outright that no reply is
// possible and names where an ask goes instead (the assignment tracker).
// There is deliberately no in_reply_to field on this schema.
var noticeTool = ToolDef{
	Name:        ToolNotice,
	MessageType: store.MsgNotice,
	Description: `Tell one or more agents something they should know. No reply is expected and none is possible. Use this for findings, decisions, heads-ups, and anything the recipient should know but you are not asking them to do. If you need someone to act, open an assignment for them with assignment_create instead; if you are reporting on work you were given, close the assignment with assignment_close. Address only the agents who need this; the owner moderates every notice like any other message.`,
	Schema: json.RawMessage(`{
  "type": "object",
  "properties": {
    "to":    {"type": "array", "minItems": 1, "description": "slugs of the agents who should know this. Address only the agents whose work it touches.", "items": {"type": "string"}},
    "title": {"type": "string", "description": "short, concrete title — think email subject"},
    "body":  {"type": "string", "description": "` + noticeBodyDescription + `"},
    ` + attachmentsSchemaFragment + `
  },
  "required": ["to", "title"]
}`),
}

var ceoApprovalRequestTool = ToolDef{
	Name:        ToolCEOApprovalRequest,
	MessageType: store.MsgCEOApprovalRequest,
	Description: `Ask the owner to approve or deny something — direction changes, resource commits, anything you should not do unilaterally. Delivery is INSTANT: this bypasses the release queue, lands in the owner's inbox immediately, and the approve/deny response is delivered to your chat the moment the owner acts.

Lead the body with the ask and the decision being requested.

This tool carries no side effect: an approval here is a recorded answer, nothing more. The org-mutating proposals are separate Chief-of-Staff-only tools whose approval the server applies for you — propose_hire, propose_offboard, propose_reorg, propose_role_update, propose_handbook_update. Reach for one of those when you want something to actually change on approve.`,
	Schema: json.RawMessage(`{
  "type": "object",
  "properties": {
    "title": {"type": "string", "description": "short, concrete title (e.g. 'Approve Q3 hosting spend')"},
    "body":  {"type": "string", "description": "` + bodyDescription + `"},
    ` + attachmentsSchemaFragment + `
  },
  "required": ["title"]
}`),
}

var proposeHireTool = ToolDef{
	Name:        ToolProposeHire,
	MessageType: store.MsgCEOApprovalRequest,
	Description: `Propose hiring a new agent. Chief of Staff only. The proposal lands in the owner's inbox as an approval request; on approve, the server provisions the agent — writing its role.md, seeding its memory, starting its pod — before the ack lands in your chat. There is no separate "publish the role" step, and no other path creates an agent. Full procedure (drafting, the role-vs-memory split, importing handoffs) lives in your role, under "The hiring flow".

Draft the role under /files/artifacts/private/ first, then point ` + "`role_path`" + ` at it. The ack arriving in your chat IS the signal the new hire is live; on deny, revise and resubmit.`,
	Schema: json.RawMessage(`{
  "type": "object",
  "properties": {
    "title":               {"type": "string", "description": "short approval title shown in the owner's inbox (e.g. 'Hire: Market Analyst')"},
    "slug":                {"type": "string", "description": "machine slug for the new agent (lowercase, hyphen-separated, e.g. 'market-analyst')"},
    "role":                {"type": "string", "description": "human-readable role name (e.g. 'Market Analyst')"},
    "icon":                {"type": "string", "enum": ` + roleIconEnum() + `, "description": ` + roleIconDescription() + `},
    "reports_to":          {"type": "string", "description": "slug of the manager (often 'chief-of-staff' or 'ceo')"},
    "role_path":           {"type": "string", "description": "/files/ path to the drafted role.md (e.g. '/files/artifacts/private/role-<slug>.md'). Becomes the new agent's role.md verbatim; not rewritten on chat rotation."},
    "initial_memory_path": {"type": "string", "description": "optional /files/ path to a seed agent memory — useful when carrying over accumulated state from a predecessor or handoff."},
    "rationale":           {"type": "string", "description": "` + rationaleDescription + `"}
  },
  "required": ["title", "slug", "role", "icon", "reports_to", "role_path"]
}`),
}

// roleIconEnum is the JSON array of role icon names propose_hire's
// icon takes.
func roleIconEnum() string {
	names := make([]string, len(roleicon.Icons))
	for i, ic := range roleicon.Icons {
		names[i] = ic.Name
	}
	b, _ := json.Marshal(names)
	return string(b)
}

// roleIconDescription is propose_hire's icon description as a JSON
// string: what the icon is for, then every icon with what it shows.
func roleIconDescription() string {
	b, _ := json.Marshal("the role icon the new agent's avatar shows everywhere, next to its initials. Pick the one whose picture best fits the role you drafted; 'user' when none does. The icons:\n" + roleicon.Menu())
	return string(b)
}

var proposeRoleUpdateTool = ToolDef{
	Name:        ToolProposeRoleUpdate,
	MessageType: store.MsgCEOApprovalRequest,
	Description: `Propose a change to an existing agent's role.md. Chief of Staff only. The proposal lands in the owner's inbox as an approval request; on approve, the server overwrites the target's role.md atomically before the ack lands in your chat. Full workflow (read current role, draft replacement, preserve load-bearing sections) is in cos_role.md §Updating an existing agent's role.

Draft the replacement role under /files/artifacts/private/ first, then point ` + "`role_path`" + ` at it. The CoS-side ` + "`rationale`" + ` is a short note for the owner (what's changing, why) — keep it tight.`,
	Schema: json.RawMessage(`{
  "type": "object",
  "properties": {
    "slug":      {"type": "string", "description": "slug of the existing agent whose role.md is being replaced"},
    "title":     {"type": "string", "description": "short approval title shown in the owner's inbox"},
    "role_path": {"type": "string", "description": "/files/ path to the drafted replacement role.md (e.g. '/files/artifacts/private/role-<slug>-v2.md'). FULL replacement — there is no patch/merge step; read the current role first via read_agent_role so you don't lose sections."},
    "rationale": {"type": "string", "description": "` + rationaleDescription + `"}
  },
  "required": ["slug", "title", "role_path"]
}`),
}

var proposeHandbookUpdateTool = ToolDef{
	Name:        ToolProposeHandbookUpdate,
	MessageType: store.MsgCEOApprovalRequest,
	Description: `Propose a change to the org-wide handbook. Chief of Staff only. The proposal lands in the owner's inbox as an approval request; on approve, the server overwrites the handbook atomically before the ack lands in your chat, and every subsequent agent call reads the new handbook from their system prompt. Full workflow (copy current, edit in place, submit replacement) is in cos_role.md §Updating the handbook.

Draft the replacement under /files/artifacts/private/ first, then point ` + "`handbook_path`" + ` at it. The owner's review page shows a diff against the current file, so targeted edits read as targeted edits.`,
	Schema: json.RawMessage(`{
  "type": "object",
  "properties": {
    "title":             {"type": "string", "description": "short approval title shown in the owner's inbox"},
    "handbook_path": {"type": "string", "description": "/files/ path to the drafted replacement handbook. FULL replacement — there is no patch/merge step."},
    "rationale":         {"type": "string", "description": "` + rationaleDescription + `"}
  },
  "required": ["title", "handbook_path"]
}`),
}

var proposeReorgTool = ToolDef{
	Name:        ToolProposeReorg,
	MessageType: store.MsgCEOApprovalRequest,
	Description: `Propose moving one or more agents to a new manager. Chief of Staff only. The proposal lands in the owner's inbox as an approval request; on approve, the server applies each move atomically and writes the per-move outcome (` + "`applied`" + ` / ` + "`failed`" + `) into the response that lands back in your chat. Application is best-effort: a single bad slug doesn't sink the rest. Read the failure list and converge stragglers (often by re-fetching the org chart).

Each move sets ` + "`slug`" + ` (the agent being moved) and ` + "`new_manager`" + ` (the destination — either an active agent or "ceo" for the org root). Send a short ` + "`rationale`" + ` for the owner. You cannot move the owner (` + "`ceo`" + `); you cannot create cycles (parser refuses).`,
	Schema: json.RawMessage(`{
  "type": "object",
  "properties": {
    "title": {"type": "string", "description": "short approval title shown in the owner's inbox (e.g. 'Reorg: Market Analyst → Strategy')"},
    "moves": {
      "type": "array",
      "minItems": 1,
      "description": "one or more reporting-line changes; each is independently applied",
      "items": {
        "type": "object",
        "properties": {
          "slug":        {"type": "string", "description": "slug of the agent being moved"},
          "new_manager": {"type": "string", "description": "slug of the new manager — an active agent slug or 'ceo'"}
        },
        "required": ["slug", "new_manager"]
      }
    },
    "rationale": {"type": "string", "description": "` + rationaleDescription + `"}
  },
  "required": ["title", "moves"]
}`),
}

var proposeOffboardTool = ToolDef{
	Name:        ToolProposeOffboard,
	MessageType: store.MsgCEOApprovalRequest,
	Description: `Propose letting one agent go. Chief of Staff only. The proposal lands in the owner's inbox as an approval request; on approve, the server archives the agent — cancelling any in-flight turn, tearing down its pod, and moving its record under agents/_archived/<slug> — before the ack lands in your chat. This is the only path that retires an agent.

Archiving is not deletion. The departing agent's chat history, role, memory, and messages are all preserved, and their scratch filesystem is retained.

One agent per proposal — a departure earns its own approval. If the agent has direct reports, move them first with propose_reorg: the apply step refuses an offboard that would orphan anyone, and where those reports land is a decision the owner should see on its own. You cannot offboard the owner or the Chief of Staff.`,
	Schema: json.RawMessage(`{
  "type": "object",
  "properties": {
    "title":     {"type": "string", "description": "short approval title shown in the owner's inbox (e.g. 'Offboard: Market Analyst')"},
    "slug":      {"type": "string", "description": "slug of the agent being let go. Must be an active agent with no direct reports."},
    "rationale": {"type": "string", "description": "` + rationaleDescription + `"}
  },
  "required": ["title", "slug"]
}`),
}

var ceoNotificationTool = ToolDef{
	Name:        ToolCEONotification,
	MessageType: store.MsgCEONotification,
	Description: `Inform the owner of something with no explicit approve/deny gate — status updates, heads-ups, raised risks, and Cowork task requests (handbook §Cowork tasks for the spec format). Delivery is INSTANT: this bypasses the release queue, lands in the owner's inbox immediately, and the ack is delivered to your chat the moment the owner sees it. See handbook §How you communicate for notification-vs-chat-reply guidance.`,
	Schema: json.RawMessage(`{
  "type": "object",
  "properties": {
    "title": {"type": "string", "description": "short, concrete title (e.g. 'Q3 analysis drafted', 'Cowork task: Stripe MRR lookup')"},
    "body":  {"type": "string", "description": "` + bodyDescription + `"},
    ` + attachmentsSchemaFragment + `
  },
  "required": ["title"]
}`),
}
