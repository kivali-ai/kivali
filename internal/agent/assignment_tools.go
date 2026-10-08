package agent

import (
	"encoding/json"
	"strings"

	"github.com/kivali-ai/kivali/internal/provider"
)

// Tool names for the assignment tracker: four writes and two reads,
// given to every agent and to nobody's subagents. The MCP side
// (internal/mcp/assignment_tools.go) builds its definitions from these
// constants, so the model reads one text whichever transport it runs
// under. See docs/developers/assignments.md §Tools.
const (
	AssignmentCreateToolName = "assignment_create"
	AssignmentUpdateToolName = "assignment_update"
	AssignmentCloseToolName  = "assignment_close"
	AssignmentReopenToolName = "assignment_reopen"
	AssignmentListToolName   = "assignment_list"
	AssignmentViewToolName   = "assignment_view"
)

// AssignmentToolNames lists the six canonical names in the order they
// are registered.
var AssignmentToolNames = []string{
	AssignmentCreateToolName, AssignmentUpdateToolName, AssignmentCloseToolName,
	AssignmentReopenToolName, AssignmentListToolName, AssignmentViewToolName,
}

// IsAssignmentTool reports whether name is one of the tracker's tools.
func IsAssignmentTool(name string) bool {
	for _, n := range AssignmentToolNames {
		if n == name {
			return true
		}
	}
	return false
}

// AssignmentTools returns the native-API definitions of the six
// assignment tools under their canonical names.
func AssignmentTools() []provider.Tool {
	return []provider.Tool{
		{Name: AssignmentCreateToolName, Description: AssignmentCreateDescription, InputSchema: json.RawMessage(AssignmentCreateInputSchema)},
		{Name: AssignmentUpdateToolName, Description: AssignmentUpdateDescription, InputSchema: json.RawMessage(AssignmentUpdateInputSchema)},
		{Name: AssignmentCloseToolName, Description: AssignmentCloseDescription, InputSchema: json.RawMessage(AssignmentCloseInputSchema)},
		{Name: AssignmentReopenToolName, Description: AssignmentReopenDescription, InputSchema: json.RawMessage(AssignmentReopenInputSchema)},
		{Name: AssignmentListToolName, Description: AssignmentListDescription, InputSchema: json.RawMessage(AssignmentListInputSchema)},
		{Name: AssignmentViewToolName, Description: AssignmentViewDescription, InputSchema: json.RawMessage(AssignmentViewInputSchema)},
	}
}

// AssignmentDescription looks up a canonical tool's description by
// name; "" when the name is not an assignment tool.
func AssignmentDescription(name string) string {
	for _, t := range AssignmentTools() {
		if t.Name == name {
			return t.Description
		}
	}
	return ""
}

// AssignmentCreateDescription is assignment_create's model-facing text.
var AssignmentCreateDescription = strings.TrimSpace(`
Open an assignment: one unit of work with one assignee, or one question with one answerer. The title is the ask in a line; the description carries context, constraints and what done looks like (cap 4096 bytes; substance goes in a file you publish with artifact_publish before you open this, cited by the path the assignee reads it at: /files/artifacts/shared/<your-slug>/<path>). assignee defaults to you. Name another agent to hand them work, or "ceo" to ask the owner. parent makes this a part of another assignment: an open part holds up the assignment it is part of until it closes, and only that assignment's creator, its assignee or the owner may open parts under it. An assignment with no parent is a goal. A question is a part of the assignment that raised it, for whoever can answer; the answer arrives as its outcome and you are woken when it closes. blocked_by names assignments in other trees this one waits on. The assignee is woken when the owner releases the event. Work you carry past this turn gets one assignment for you (one per thread of work, not one per turn; opening one for yourself wakes nobody, and every wake ends with the assignments you hold); anything you need from someone else is an assignment for them.

acceptance: the Done-when conditions, the named deliverables this assignment expects from its parts, written when you know what done requires and independent of what has been opened so far. A release lists what it certifies; a launch lists the copy, the design, a review, and the publish step. Leave it empty on work you will do yourself.

satisfies: the Done-when conditions this assignment counts toward, among those declared by the assignment it is part of. Closing this assignment as done is what meets them.`)

// AssignmentCreateInputSchema is assignment_create's JSON schema. The
// property names are the wire schema and stay as they are.
const AssignmentCreateInputSchema = `{
  "type": "object",
  "properties": {
    "title": {"type": "string", "description": "one line, the ask itself (cap 200 bytes)"},
    "description": {"type": "string", "description": "context, constraints, what done looks like; markdown, cap 4096 bytes"},
    "assignee": {"type": "string", "description": "agent slug, or \"ceo\"; defaults to you"},
    "parent": {"type": "integer", "description": "id of the assignment this is part of; an open part holds that assignment up"},
    "blocked_by": {"type": "array", "items": {"type": "integer"}, "description": "ids in other trees this assignment waits on"},
    "acceptance": {"type": "array", "items": {"type": "string"}, "description": "the Done-when conditions: named deliverables this assignment expects from its parts, one line each (cap 200 bytes), unique; progress on it is then conditions met, not parts closed"},
    "satisfies": {"type": "array", "items": {"type": "string"}, "description": "the Done-when conditions this assignment counts toward, among those the assignment it is part of declares, named exactly as declared; needs parent"}
  },
  "required": ["title"]
}`

// AssignmentUpdateDescription is assignment_update's model-facing text.
var AssignmentUpdateDescription = strings.TrimSpace(`
Amend an open assignment. Creator or owner: title, description, assignee, parent (0 makes it a goal), hold. Creator, assignee or owner: add_blocked_by, remove_blocked_by, add_acceptance, remove_acceptance (a condition an open part counts toward stays until that part's satisfies changes). Creator, assignee, the creator or assignee of the assignment it is part of, or the owner: add_satisfies, remove_satisfies. A note is required whenever the change wakes someone: always for a reassign or a hold, and for a title or description change when the assignee is someone else; it is the first thing they read. hold=true pauses the assignment and everything under it: its assignee and the assignee of every open assignment under it are told to stop, at once if they are mid-turn and otherwise with their next wake, since an idle agent has nothing to stop; hold=false resumes them and wakes the same agents to carry on. A hold goes in a call of its own. The assignee cannot reassign an assignment: break it into parts for whoever should do them, or close it as dropped with the reason. A closed assignment takes no edit but assignment_reopen. A cycle through parent and blocked_by is refused. Only what you pass changes.

acceptance: the Done-when conditions, the named deliverables this assignment expects from its parts, written when you know what done requires and independent of what has been opened so far. A release lists what it certifies; a launch lists the copy, the design, a review, and the publish step. Leave it empty on work you will do yourself.

satisfies: the Done-when conditions this assignment counts toward, among those declared by the assignment it is part of. Closing this assignment as done is what meets them.`)

// AssignmentUpdateInputSchema is assignment_update's JSON schema.
const AssignmentUpdateInputSchema = `{
  "type": "object",
  "properties": {
    "id": {"type": "integer", "description": "the assignment"},
    "title": {"type": "string", "description": "new title"},
    "description": {"type": "string", "description": "new description; replaces the whole text"},
    "assignee": {"type": "string", "description": "new assignee slug, or \"ceo\""},
    "parent": {"type": "integer", "description": "id of the assignment it is now part of; 0 makes it a goal"},
    "add_blocked_by": {"type": "array", "items": {"type": "integer"}, "description": "ids this assignment now waits on"},
    "remove_blocked_by": {"type": "array", "items": {"type": "integer"}, "description": "ids it no longer waits on"},
    "hold": {"type": "boolean", "description": "true pauses this assignment and everything under it (everyone working on them is told to stop; an idle agent reads it with their next wake); false resumes them and wakes them. Creator or owner; needs a note; nothing else in the same call"},
    "add_acceptance": {"type": "array", "items": {"type": "string"}, "description": "Done-when conditions this assignment now expects from its parts"},
    "remove_acceptance": {"type": "array", "items": {"type": "string"}, "description": "conditions it no longer expects; refused for one an open part counts toward"},
    "add_satisfies": {"type": "array", "items": {"type": "string"}, "description": "Done-when conditions it now counts toward, among those the assignment it is part of declares, named exactly"},
    "remove_satisfies": {"type": "array", "items": {"type": "string"}, "description": "conditions it no longer counts toward; a move to another assignment removes the old one's names here"},
    "note": {"type": "string", "description": "why; required when the change wakes someone (cap 500 bytes)"}
  },
  "required": ["id"]
}`

// AssignmentCloseDescription is assignment_close's model-facing text.
var AssignmentCloseDescription = strings.TrimSpace(`
End an open assignment. resolution "done": you must be its assignee (or the owner); outcome says what was done and where the result is (cap 2048 bytes). Closing as done is what meets the Done-when condition this assignment counts toward; dropping meets nothing. resolution "dropped": its assignee or creator, the creator or assignee of an assignment it is part of, or the owner; outcome says why, and whoever is woken reads it first. Refused while any part is open: close or drop the parts, each with its own outcome. Closing as done with Done-when conditions still unmet succeeds and is recorded as a warning naming them, here, in the log and in every wake the close sends. Closing wakes the creator, and the assignee of every assignment this one was holding up. Closing is final for you: only the creator or the owner can reopen. If you finished the work but something is still missing, close it and open a new assignment for the remainder. When the owner opened the assignment, the outcome is your report to them; send no notification as well.`)

// AssignmentCloseInputSchema is assignment_close's JSON schema.
const AssignmentCloseInputSchema = `{
  "type": "object",
  "properties": {
    "id": {"type": "integer", "description": "the assignment"},
    "resolution": {"type": "string", "enum": ["done", "dropped"], "description": "done: the work is complete; dropped: it will not be done"},
    "outcome": {"type": "string", "description": "what was done and where the result is, or why it was dropped (cap 2048 bytes)"}
  },
  "required": ["id", "resolution", "outcome"]
}`

// AssignmentReopenDescription is assignment_reopen's model-facing text.
var AssignmentReopenDescription = strings.TrimSpace(`
Return a closed assignment to open. Creator or owner only. note (required) says what is wrong with the outcome; the assignee is woken and reads it first, with the earlier outcome beside it. If the assignee has since left the org, the reopened assignment lands on you: hand it on with assignment_update. Refused while the assignment it is part of is closed: reopen that one first. If you are not the creator and need the work redone, open a new assignment that says what is still missing.`)

// AssignmentReopenInputSchema is assignment_reopen's JSON schema.
const AssignmentReopenInputSchema = `{
  "type": "object",
  "properties": {
    "id": {"type": "integer", "description": "the assignment"},
    "note": {"type": "string", "description": "what is wrong with the outcome (cap 500 bytes)"}
  },
  "required": ["id", "note"]
}`

// AssignmentListDescription is assignment_list's model-facing text.
var AssignmentListDescription = strings.TrimSpace(`
List assignments, one line each: id, state (ready, on hold, blocked by which ids, or closed), title, assignee, creator, what it is part of, what it counts toward, its Done-when progress (conditions met / claimed by an open part / unclaimed) or, with no Done-when conditions, its parts, and what it holds up. With no filters it lists your open assignments. assignee="any" lists everyone's; creator=<slug> lists what someone opened; parent=<id> lists the parts of one assignment; status is open (default), closed or all; ready=true narrows to what can be worked on now. Timestamped snapshot. Open one with assignment_view.`)

// AssignmentListInputSchema is assignment_list's JSON schema.
const AssignmentListInputSchema = `{
  "type": "object",
  "properties": {
    "assignee": {"type": "string", "description": "slug, \"ceo\", or \"any\"; defaults to you"},
    "creator": {"type": "string", "description": "slug or \"ceo\": only assignments this agent opened"},
    "parent": {"type": "integer", "description": "only the parts of this assignment"},
    "status": {"type": "string", "enum": ["open", "closed", "all"], "description": "default open"},
    "ready": {"type": "boolean", "description": "true: only assignments that can be worked on now"}
  }
}`

// AssignmentViewDescription is assignment_view's model-facing text.
var AssignmentViewDescription = strings.TrimSpace(`
Read one assignment in full: its fields and derived state, the description, the outcome when closed, its Done-when conditions with which part met or claims each (an unclaimed condition is work nobody has opened), the condition it counts toward, its parts with their state, what it waits on, what it holds up, and the log of every change with who made it and why. Read it before acting on a wake about it, and again after an amendment.`)

// AssignmentViewInputSchema is assignment_view's JSON schema.
const AssignmentViewInputSchema = `{
  "type": "object",
  "properties": {
    "id": {"type": "integer", "description": "the assignment"}
  },
  "required": ["id"]
}`
