// Package message defines the standardized inter-agent message types
// that Kivali uses for communication: notice, ceo_approval_request,
// ceo_notification.
//
// Between agents there is one speech act: a notice is a tell.
// Nothing is owed back, and the parser refuses any message whose
// in_reply_to names one. An ask is not a message; it is an assignment
// in the tracker (docs/developers/assignments.md), and the wake the
// tracker sends for it is an assignment_event the tracker writes itself.
//
// For each type, the package provides:
//   - An Anthropic-compatible tool definition (name, description, JSON
//     schema) that agents invoke to publish a message.
//   - A parser that releases a tool_use content block into a store.Message
//     ready to persist.
//
// Structured fields (title, to, from, date, type) are enforced by the
// tool schema; the body is free markdown. High-level "how to write a
// good X" guidance lives in the tool description — not in the schema —
// so agents retain writing freedom.
package message

import (
	"encoding/json"

	"github.com/kivali-ai/kivali/internal/provider"
	"github.com/kivali-ai/kivali/internal/store"
)

// Tool names. Stable; agents address these by string in their tool calls.
const (
	// ToolNotice publishes a `notice`: a tell addressed to one or
	// more agents that closes nothing and opens nothing. It has no
	// in_reply_to field, and the parser refuses any message that
	// names a notice as its reply target — the two halves of "no
	// reply is expected and none is possible".
	ToolNotice             = "publish_notice"
	ToolCEOApprovalRequest = "publish_ceo_approval_request"
	ToolCEONotification    = "publish_ceo_notification"
	// ToolProposeHire generates a ceo_approval_request whose approval,
	// when granted, provisions a new agent. Listed only for the Chief
	// of Staff (gated in ToolsFor); a non-CoS slug calling it is
	// rejected at parse.
	ToolProposeHire = "propose_hire"
	// ToolProposeRoleUpdate generates a ceo_approval_request whose
	// approval, when granted, overwrites an existing agent's role.md
	// atomically. Listed only for the Chief of Staff (gated in
	// ToolsFor); a non-CoS slug calling it is rejected at parse.
	ToolProposeRoleUpdate = "propose_role_update"
	// ToolProposeHandbookUpdate generates a ceo_approval_request
	// whose approval, when granted, overwrites the org-wide
	// handbook atomically. Listed only for the Chief of Staff
	// (gated in ToolsFor); a non-CoS slug calling it is rejected at
	// parse — same defense-in-depth as ToolProposeRoleUpdate.
	ToolProposeHandbookUpdate = "propose_handbook_update"
	// ToolProposeReorg generates a ceo_approval_request that proposes
	// moving one or more agents to a new manager. Listed only for the
	// Chief of Staff (gated in ToolsFor); a non-CoS slug calling it
	// is rejected at parse. Application is best-effort per move so a
	// stale slug in a multi-move proposal doesn't sink the rest.
	ToolProposeReorg = "propose_reorg"
	// ToolProposeOffboard generates a ceo_approval_request that
	// proposes letting one agent go. Listed only for the Chief of
	// Staff (gated in ToolsFor); a non-CoS slug calling it is rejected
	// at parse. On approve the server archives the agent — the only
	// path that does so.
	ToolProposeOffboard = "propose_offboard"
)

// CEO is the special "slug" that represents the human user.
const CEO = "ceo"

// ChiefOfStaff is the slug of the one role allowed to publish the
// org-mutating propose_* proposals. Every parsePropose* gates on it.
const ChiefOfStaff = "chief-of-staff"

// ToolDef bundles everything needed to expose one publish tool to Claude
// and to parse its output back into a store.Message.
type ToolDef struct {
	Name        string
	Description string
	MessageType store.MessageType
	Schema      json.RawMessage
}

// AsClaudeTools converts a slice of message tools into provider.Tool for
// passing to Client.Complete.
func AsClaudeTools(defs []ToolDef) []provider.Tool {
	out := make([]provider.Tool, 0, len(defs))
	for _, d := range defs {
		out = append(out, provider.Tool{
			Name:        d.Name,
			Description: d.Description,
			InputSchema: d.Schema,
		})
	}
	return out
}

// AllTools returns every publish tool exposed to agents. New hires
// flow through `propose_hire`, and the server provisions the new agent
// only when the CEO clicks Approve, so no tool call can create an
// agent without an approval.
//
// Note that the propose_* family is NOT in this list — those are
// listed only for the Chief of Staff (see ToolsFor). Anything that
// doesn't filter on slug should still see only the tools every agent
// has.
func AllTools() []ToolDef {
	return []ToolDef{
		noticeTool,
		ceoApprovalRequestTool,
		ceoNotificationTool,
	}
}

// ToolsFor returns the publish tools available to the given agent. The
// org-mutating propose_* family is visible only to the Chief of Staff
// and is appended when isChiefOfStaff is true. Defense-in-depth
// runtime checks live on the parser side too — every parsePropose*
// refuses a non-CoS caller regardless of what the tool list said.
func ToolsFor(slug string, isChiefOfStaff bool) []ToolDef {
	out := AllTools()
	if isChiefOfStaff {
		out = append(out, ProposeTools()...)
	}
	return out
}

// ProposeTools returns the Chief-of-Staff-only proposals: the five
// tools whose CEO approval the server applies as a side effect
// (provision an agent, archive one, move reporting lines, replace a
// role.md, replace the handbook). Split out from ToolsFor so the
// "what can only CoS do" list has exactly one definition — tests and
// the MCP layer read it instead of restating the set.
func ProposeTools() []ToolDef {
	return []ToolDef{
		proposeHireTool,
		proposeOffboardTool,
		proposeReorgTool,
		proposeRoleUpdateTool,
		proposeHandbookUpdateTool,
	}
}
