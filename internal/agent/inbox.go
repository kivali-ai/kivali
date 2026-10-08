package agent

import (
	"fmt"
	"strings"

	"github.com/kivali-ai/kivali/internal/store"
)

// InboxView is the structured, presentation-ready form of an inbox
// delivery. It's derived from a store.Message (the authoritative on-
// disk record) and consumed by:
//
//   - RenderInboxBody, which releases it into the plain-text the
//     agent's Claude call sees in its user message,
//   - the agent-detail HTML template, which renders it as a
//     first-class chat bubble (icon, title, badges, body).
//
// Both paths use the same View so a schema change to Message only
// has to update this one derivation — the template can't drift away
// from the text the model sees.
type InboxView struct {
	// Type is the raw message type — "notice", the assignment event's
	// stored type string (store.MsgAssignmentEvent),
	// "ceo_approval_response", etc. Kept as a string key so the
	// template can switch on it for CSS classes.
	Type string
	// Icon is a single-glyph (or glyph-sequence) that labels the
	// type at-a-glance in the UI.
	Icon string
	// Label is the human-readable type name with decision folded
	// in — "Notice", "Assignment", "Approved", "Denied", "Acknowledged".
	Label string
	// Decision captures approval outcome as a CSS-class-safe string
	// when the type is ceo_approval_response: "approved", "denied",
	// "". Empty for all other types.
	Decision string
	// From is the slug of the sender; the UI flips sender/recipient
	// perspective per agent page, the model sees it as-is.
	From string
	// To is the message's recipient list — one slug for every type
	// but a notice, which may address several agents at once. The
	// chat bubble renders it so a recipient can see who else was told
	// the same thing without opening the message file.
	To []string
	// Title is the message's human title.
	Title string
	// Body is the message body (trimmed), with a default filled in
	// for empty CEO-reply bodies so the agent never sees a
	// bare-title signal.
	Body string
	// Approved is non-nil on ceo_approval_response. The template
	// uses it to render the decision pill.
	Approved *bool
	// InReplyTo is the relative path of the original request this
	// message responds to, when applicable.
	InReplyTo string
	// Path is the relative path of THIS message on disk
	// (e.g. "messages/2026-04-27/<ts>-<type>-<from>--to--<to>.md").
	// Surfaced in the model-facing paraphrase for the types a reply
	// may name. Optional; empty omits the "at <path>" phrasing.
	Path string
	// Hire is non-nil on two message types:
	//   - ceo_approval_request: the requester is proposing a hire.
	//   - ceo_approval_response: the CEO approved a hire and the
	//     server provisioned the agent as part of the approval.
	// The paraphrase uses this to state explicitly that the new
	// agent is live, so the recipient doesn't misread the approval
	// as "plan acknowledged, now go execute the hire yourself."
	Hire *store.Hire
	// RoleUpdate is non-nil on the same two message types when the
	// approval is a propose_role_update proposal — used by the
	// paraphrase to state that the role.md was overwritten on
	// approve, mirroring the Hire treatment.
	RoleUpdate *store.RoleUpdate
	// HandbookUpdate is non-nil on the same two message types
	// when the approval is a propose_handbook_update proposal.
	// Used by the paraphrase to state that the handbook was
	// overwritten on approve, mirroring RoleUpdate.
	HandbookUpdate *store.HandbookUpdate
	// Offboard is non-nil on the same two message types when the
	// approval is a propose_offboard proposal. The paraphrase uses it
	// to state that the agent is already archived — without that, CoS
	// reads a bare "I approved your request" and may go looking for
	// a way to archive the agent itself.
	Offboard *store.Offboard
	// Assignment is non-nil on an assignment event: the assignment the
	// wake is about. The UI links the bubble and the inbox card to
	// /assignments/<id>.
	Assignment *store.AssignmentRef
	// Attachments are the message-level attachments, rendered as
	// chips both in the text body for the model and as a link row
	// in the UI.
	Attachments []store.MessageAttachment
}

// BuildInboxView derives the structured view from a persisted
// message. One source of truth for both the model-facing text
// rendering and the UI.
//
// relPath is the relative path of the message on disk (the same
// string stored as ChatMessage.MessageRef). Callers that have it
// should pass it; tests and the UI path pass "" when the path
// isn't relevant to the rendering they need.
func BuildInboxView(d store.Message, relPath string) InboxView {
	v := InboxView{
		Type:           string(d.Type),
		From:           d.From,
		To:             d.To,
		Title:          d.Title,
		Body:           strings.TrimRight(d.Body, "\n"),
		Approved:       d.Approved,
		InReplyTo:      d.InReplyTo,
		Path:           relPath,
		Hire:           d.Hire,
		RoleUpdate:     d.RoleUpdate,
		HandbookUpdate: d.HandbookUpdate,
		Offboard:       d.Offboard,
		Assignment:     d.Assignment,
		Attachments:    d.Attachments,
	}
	if d.Type == store.MsgCEOApprovalResponse {
		if d.Approved != nil && *d.Approved {
			v.Decision = "approved"
		} else if d.Approved != nil && !*d.Approved {
			v.Decision = "denied"
		}
	}
	v.Icon = iconForType(d.Type, v.Decision)
	v.Label = labelForType(d.Type, v.Decision)
	// Note: v.Body stays exactly what the CEO typed (or empty
	// when they clicked through without a message). The old
	// "fill empty bodies with 'Approved.' / 'Denied.'" default
	// is gone — the UI bubble uses the decision pill + icon,
	// and the model-facing paraphrase prepends an explicit
	// decision statement via assembleBody().
	return v
}

func iconForType(t store.MessageType, decision string) string {
	switch t {
	case store.MsgNotice:
		return "🔔"
	case store.MsgAssignmentEvent:
		return "🗂"
	case store.MsgCEOApprovalRequest:
		return "🗳"
	case store.MsgCEOApprovalResponse:
		switch decision {
		case "approved":
			return "✅"
		case "denied":
			return "⛔"
		default:
			return "🗳"
		}
	case store.MsgCEONotification:
		return "📣"
	case store.MsgCEONotificationAck:
		return "💬"
	default:
		return "📨"
	}
}

func labelForType(t store.MessageType, decision string) string {
	switch t {
	case store.MsgNotice:
		return "Notice"
	case store.MsgAssignmentEvent:
		return "Assignment"
	case store.MsgCEOApprovalRequest:
		return "Approval request"
	case store.MsgCEOApprovalResponse:
		switch decision {
		case "approved":
			return "Approved"
		case "denied":
			return "Denied"
		default:
			return "Approval response"
		}
	case store.MsgCEONotification:
		return "Notification"
	case store.MsgCEONotificationAck:
		return "Acknowledged"
	default:
		return string(t)
	}
}

// RenderText formats the view as the plain-text user message the
// agent's Claude call sees. Emits a natural-language paraphrase
// rather than a structured `[INBOX …]` preamble.
//
// Why: an inbox delivery is rendered into the model's conversation
// as a user-role message, structurally indistinguishable from a
// direct CEO chat. When the preamble was a machine-shaped header,
// models sometimes confabulated — pattern-matching the header
// against the original user kickoff and re-interpreting routed
// events as new directives. A sentence of natural-language narration
// ("X sent you a task request titled 'Y' …. Their message follows.")
// reads as clearly-something-that-happened-to-me and avoids that
// failure mode across model families without relying on XML hinting.
//
// The on-disk record (the Message file referenced by
// ChatMessage.MessageRef) keeps every structured field — the UI
// renders its first-class bubble from those fields via BuildInboxView.
// Only the model-facing text changes shape here.
//
// The body is included as a quote after the lead. For CEO reply
// types the decision is always made explicit at the start of the
// quote — "I approved your request." / "I denied your hire of X
// (slug ...). The agent has been provisioned and is now live." —
// regardless of whether the CEO typed a custom message. A custom
// message is appended as a second paragraph. This makes every
// CEO reply read as reported speech with the decision foregrounded,
// which closes the class of "model saw Approved but didn't realize
// the side-effect fired" misreads without needing prompt-level
// guardrails in the handbook.
func (v InboxView) RenderText() string {
	lead := v.naturalLead()
	body := v.assembleBody()
	out := lead
	if body != "" {
		out += "\n\n" + body
	}
	if len(v.Attachments) > 0 {
		names := make([]string, 0, len(v.Attachments))
		for _, a := range v.Attachments {
			names = append(names, a.Name)
		}
		var suffix string
		if len(names) == 1 {
			suffix = "1 attachment: " + names[0]
		} else {
			suffix = fmt.Sprintf("%d attachments: %s", len(names), strings.Join(names, ", "))
		}
		out += "\n\n(" + suffix + ")"
	}
	return out
}

// assembleBody returns the quoted-message body. For CEO reply types
// it prepends a decision statement so the outcome is always in the
// speech act itself; any typed CEO message is appended as a second
// paragraph. For other types it's just the raw body.
func (v InboxView) assembleBody() string {
	if !store.MessageType(v.Type).IsCEOReply() {
		return v.Body
	}
	decision := v.decisionSpeech()
	if decision == "" {
		return v.Body
	}
	if v.Body == "" {
		return decision
	}
	return decision + "\n\n" + v.Body
}

// decisionSpeech renders the CEO's decision as a first-person
// statement — the thing the CEO is "saying" when their response
// lands in the recipient's chat. For hire approvals it includes
// the slug/role/reports_to so the recipient has the exact
// provisioning facts in the quote and doesn't have to cross-
// reference the org chart to figure out what happened.
func (v InboxView) decisionSpeech() string {
	switch store.MessageType(v.Type) {
	case store.MsgCEOApprovalResponse:
		switch v.Decision {
		case "approved":
			if v.Hire != nil {
				role := v.Hire.Role
				if role == "" {
					role = v.Hire.Slug
				}
				return fmt.Sprintf(
					"I approved your hire of %s (slug `%s`, reports to `%s`). The agent has been provisioned and is now live.",
					role, v.Hire.Slug, v.Hire.ReportsTo,
				)
			}
			if v.RoleUpdate != nil {
				return fmt.Sprintf(
					"I approved your role update for `%s`. The agent's role.md has been overwritten and is now live.",
					v.RoleUpdate.Slug,
				)
			}
			if v.HandbookUpdate != nil {
				return "I approved your handbook update. The org-wide handbook has been overwritten and is now live for every agent."
			}
			if v.Offboard != nil {
				return fmt.Sprintf(
					"I approved letting `%s` go. They have been archived and are no longer an active agent — stop routing work to them. Their chat history, role, and memory are preserved in the archive.",
					v.Offboard.Slug,
				)
			}
			return "I approved your request."
		case "denied":
			if v.Hire != nil {
				role := v.Hire.Role
				if role == "" {
					role = v.Hire.Slug
				}
				return fmt.Sprintf("I denied your hire of %s (slug `%s`).", role, v.Hire.Slug)
			}
			if v.RoleUpdate != nil {
				return fmt.Sprintf("I denied your role update for `%s`.", v.RoleUpdate.Slug)
			}
			if v.HandbookUpdate != nil {
				return "I denied your handbook update."
			}
			if v.Offboard != nil {
				return fmt.Sprintf("I denied your proposal to let `%s` go. They remain an active agent.", v.Offboard.Slug)
			}
			return "I denied your request."
		}
	case store.MsgCEONotificationAck:
		return "I acknowledge your notification."
	}
	return ""
}

// naturalLead composes the one-sentence preamble that introduces
// the body. Deterministic templating keyed on message type — no
// LLM call; the paraphrase stays stable across replays and caches
// cleanly because the resulting ChatMessage.Content is fixed once
// persisted.
//
// Three kinds of information matter to the model downstream:
//   - who sent it and what kind of thing it is (notice, assignment
//     event, approval response…),
//   - the title so the model can correlate back to its own
//     outgoing history by name rather than by filename,
//   - a path reference, on the types a reply may name, so the
//     model can cite the message without guessing at the filename.
//
// CEO-reply types fold the decision (approved / denied /
// acknowledged) into the verb itself so the outcome reads at a
// glance; the original-request path is included so the recipient
// can cross-reference.
func (v InboxView) naturalLead() string {
	titled := ""
	if strings.TrimSpace(v.Title) != "" {
		titled = fmt.Sprintf(" titled %q", v.Title)
	}
	atPath := ""
	if v.Path != "" {
		atPath = fmt.Sprintf(" (at %s)", v.Path)
	}
	sender := v.From
	if sender == "" {
		sender = "An unknown sender"
	}

	switch store.MessageType(v.Type) {
	case store.MsgNotice:
		// A notice is a tell. The two sentences after the title are
		// the whole feature: they run at exactly the moment the model
		// is deciding what to do next, and they name the one legal
		// follow-up (open an assignment) so the reflex to reply has
		// somewhere else to go. Keep this framing verbatim.
		//
		// No "(at <path>)" here: that path exists so a recipient can
		// cite the message in a future in_reply_to, and a notice is
		// something nothing may point at. Offering the handle would
		// only lead the model to a parse error. The raw file is still
		// one click away in the UI via the delivery's MessageRef.
		return fmt.Sprintf("%s sent you a notice%s. No reply is expected. If it requires action from someone, open an assignment for the owner.",
			sender, titled)

	case store.MsgAssignmentEvent:
		// The tracker speaking. The body already names the actor,
		// the assignment and what happened; the lead only says where
		// to act, because a reply is not it. No "(at <path>)": nothing
		// may point at an assignment event.
		id := ""
		if v.Assignment != nil {
			id = fmt.Sprintf(" %s", assignmentRefString(v.Assignment.ID))
		}
		return fmt.Sprintf("The assignment tracker reports a change to assignment%s, made by %s. Act on the assignment itself with the assignment_* tools; nothing is owed back on this message.",
			id, sender)

	case store.MsgCEOApprovalResponse:
		// Neutral "replied and said:" frame. The decision itself
		// (approved / denied, plus hire provisioning facts) lives
		// in the quoted body via decisionSpeech(), so the model
		// always sees the outcome inside the quote — no defensive
		// language needed in the system prompt or handbook.
		return ceoReplyLead("approval request", stripDecisionPrefix(v.Title), v.InReplyTo)

	case store.MsgCEONotificationAck:
		return ceoReplyLead("notification", stripDecisionPrefix(v.Title), v.InReplyTo)

	case store.MsgCEOApprovalRequest, store.MsgCEONotification:
		// These types are addressed to the CEO, so they don't
		// arrive as an inbox_delivery on an agent's chat; rendered
		// defensively all the same.
		return fmt.Sprintf("%s sent the owner a %s%s%s. Content follows.",
			sender, v.Label, titled, atPath)

	default:
		return fmt.Sprintf("%s sent you a %s%s%s. Content follows.",
			sender, v.Type, titled, atPath)
	}
}

// assignmentRefString renders an assignment id the way the tracker
// prints it.
func assignmentRefString(id int) string { return fmt.Sprintf("#%d", id) }

// stripDecisionPrefix returns the original request title from a CEO
// reply's "Approved: X" / "Denied: X" / "Acknowledged: X" title
// prefix, so the paraphrase can reference the original ask by its
// own title (easier for the model to correlate than a filename).
func stripDecisionPrefix(title string) string {
	for _, p := range []string{"Approved: ", "Denied: ", "Acknowledged: "} {
		if strings.HasPrefix(title, p) {
			return strings.TrimPrefix(title, p)
		}
	}
	return title
}

// ceoReplyLead composes the reported-speech frame for a CEO reply:
// "The CEO replied to your <kind> titled 'X' (at <path>) and said:".
// Neutral verb; the decision itself lives in the quoted body via
// decisionSpeech() so the model always sees the outcome in the CEO's
// "voice" rather than as a system-authored label.
//
// Correlates back to the original request via its title (easier
// for the model than matching filenames) and includes the path
// so the recipient can cite this exact response if they need to
// reference it later.
func ceoReplyLead(kind, origTitle, inReplyTo string) string {
	titled := ""
	if strings.TrimSpace(origTitle) != "" {
		titled = fmt.Sprintf(" titled %q", origTitle)
	}
	lead := fmt.Sprintf("The owner replied to your %s%s", kind, titled)
	if inReplyTo != "" {
		lead += " (at " + inReplyTo + ")"
	}
	lead += " and said:"
	return lead
}
