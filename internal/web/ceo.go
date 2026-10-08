package web

import (
	"context"
	"fmt"
	"log"
	"mime/multipart"
	"net/http"
	"path/filepath"
	"strings"
	"time"

	"github.com/kivali-ai/kivali/internal/agent"
	"github.com/kivali-ai/kivali/internal/store"
)

type responseKind int

const (
	responseKindApproval responseKind = iota
	responseKindAck
)

// ceoRefusal is respondAsCEO's failure: the status and a short
// "<stage>: <cause>" text (Msg, what Error returns). The request stays
// in the CEO's inbox on every refusal except the post-write delivery
// ones, which say so.
//
// Words and Who are the same refusal as the API says it: a sentence in
// sentence case without the "<stage>: " prefix, and who can fix
// it. A refusal without Words is worded by writeNeedsError from its
// status.
type ceoRefusal struct {
	Status int
	Msg    string
	Words  string
	Who    string
}

func (e *ceoRefusal) Error() string { return e.Msg }

func refuseCEO(status int, msg string) (ceoResponse, error) {
	return ceoResponse{}, &ceoRefusal{Status: status, Msg: msg}
}

// refuseCEOSaying is refuseCEO with the API's words and who.
func refuseCEOSaying(status int, msg, words, who string) (ceoResponse, error) {
	return ceoResponse{}, &ceoRefusal{Status: status, Msg: msg, Words: words, Who: who}
}

// refuseApply is an approved proposal whose change could not be
// applied. msg keeps the form's wording; the API says what could not
// be applied, the cause without its developer prefixes, and that the
// CEO can deny it or ask the proposer (named by proposer) again.
func refuseApply(what, msg string, err error, proposer string) (ceoResponse, error) {
	words := what + " could not be applied"
	if cause := plainCause(err); cause != "" {
		words += ": " + cause
	}
	return refuseCEOSaying(http.StatusConflict, msg+err.Error(), words,
		"you, by denying it or asking "+proposer+" for a new proposal")
}

// plainCause is err's text without the "stage: " prefixes Go errors
// collect as they are wrapped ("hire: reports_to: unknown agent" is
// "unknown agent"). A prefix is a run of at most two words before
// ": " with no quotes in it, so a quoted name that holds a colon
// stays.
func plainCause(err error) string {
	if err == nil {
		return ""
	}
	s := strings.TrimSpace(err.Error())
	for {
		i := strings.Index(s, ": ")
		if i <= 0 {
			return s
		}
		head := s[:i]
		if strings.ContainsAny(head, "\"'`") || len(strings.Fields(head)) > 2 {
			return s
		}
		s = strings.TrimSpace(s[i+2:])
	}
}

// sentenceCase upper-cases s's first letter.
func sentenceCase(s string) string {
	if s == "" {
		return s
	}
	r := []rune(s)
	return strings.ToUpper(string(r[0])) + string(r[1:])
}

// ceoResponse is what respondAsCEO did: the request it answered and
// the response it wrote and delivered.
type ceoResponse struct {
	Request  store.Message
	Response store.Message
}

// respondAsCEO answers a CEO-inbox request: approve or deny a
// ceo_approval_request (applying a proposal's change first when
// approved), or acknowledge a ceo_notification. Writes the response,
// records it on the CEO's chat, delivers it to the requester and
// republishes the org snapshot, for
// POST /api/v1/needs/approve·deny·ack.
func (s *Server) respondAsCEO(ctx context.Context, kind responseKind, approved *bool, path, message string, files []*multipart.FileHeader) (ceoResponse, error) {
	reqPath, ok := cleanMessagePath(path)
	if !ok {
		return refuseCEOSaying(http.StatusBadRequest, "bad path", "That request path is not valid", whoDevelopers)
	}
	body := strings.TrimSpace(message)

	abs := filepath.Join(s.Store.Root(), reqPath)
	reqMsg, err := s.Store.ReadMessage(abs)
	if err != nil {
		return refuseCEO(http.StatusNotFound, err.Error())
	}
	switch kind {
	case responseKindApproval:
		if reqMsg.Type != store.MsgCEOApprovalRequest {
			return refuseCEOSaying(http.StatusBadRequest, "target is not a ceo_approval_request",
				"That request is not an approval, so it cannot be approved or denied", whoDevelopers)
		}
	case responseKindAck:
		if reqMsg.Type != store.MsgCEONotification {
			return refuseCEOSaying(http.StatusBadRequest, "target is not a ceo_notification",
				"That request is not a notification, so it cannot be acknowledged", whoDevelopers)
		}
	}

	// Refuse the action if the sender of the original request has
	// been archived between publish and now: any response we wrote
	// would be undeliverable, leaving an orphan response file on
	// disk + a ceo_reply chat entry that resolves the inbox item but
	// represents no real outcome. Better to fail loud here so the
	// operator knows the request can't be fulfilled and can manually
	// dismiss it (or restore the agent first).
	//
	// Hire approvals are exempt: the response goes to the proposer
	// (reqMsg.From), and we provision a NEW agent (reqMsg.Hire.Slug).
	// If the proposer was archived between propose and approve, the
	// hire would still create the new agent but the proposer would
	// never see the outcome — same orphan-state class. So the gate
	// applies to all kinds.
	sender, err := s.Store.GetAgent(reqMsg.From)
	if err != nil {
		return refuseCEOSaying(http.StatusConflict,
			fmt.Sprintf("ceo response: original sender %q is no longer an active agent; refusing to write an undeliverable response", reqMsg.From),
			fmt.Sprintf("%s is no longer on the team, so an answer to this request cannot reach anyone", reqMsg.From),
			"no one; the agent that asked has left")
	}
	proposer := agentDisplayName(sender)

	// Optional attachments uploaded with the response (e.g. Cowork
	// output the CEO ran externally).
	var attachments []store.MessageAttachment
	for _, fh := range files {
		att, err := s.ingestCEOResponseAttachment(ctx, fh)
		if err != nil {
			return refuseCEOSaying(http.StatusInternalServerError, "attachment upload failed: "+fh.Filename+": "+err.Error(),
				"The attachment "+fh.Filename+" could not be saved", whoServer)
		}
		attachments = append(attachments, store.MessageAttachment{SHA: att.SHA, Name: fh.Filename})
	}

	respType := store.MsgCEOApprovalResponse
	titlePrefix := ""
	switch {
	case kind == responseKindApproval && approved != nil && *approved:
		titlePrefix = "Approved"
	case kind == responseKindApproval && approved != nil && !*approved:
		titlePrefix = "Denied"
	case kind == responseKindAck:
		respType = store.MsgCEONotificationAck
		titlePrefix = "Acknowledged"
	}
	// Hire proposals: if the approval request carried a `hire`
	// object and the CEO approved, provision the agent BEFORE
	// writing the approval response. That way the approval landing
	// in the requester's chat is the signal that the new agent is
	// live — no separate call, no window where the model could
	// bypass the approval gate.
	//
	// Failure path: return an error to the CEO and do NOT write an
	// approval response. The request stays in the inbox so the CEO
	// can retry or deny with a message.
	var appliedHire *store.Hire
	if kind == responseKindApproval && approved != nil && *approved && reqMsg.Hire != nil {
		if s.Runtime == nil {
			return refuseCEOSaying(http.StatusInternalServerError, "hire approval: no engine configured",
				"This server has no agent runtime, so the proposal cannot be applied", whoServer)
		}
		if s.AgentpodHub != nil {
			s.AgentpodHub.MarkStarting(reqMsg.Hire.Slug, s.clk().Now())
		}
		if _, herr := s.Runtime.ApplyHire(*reqMsg.Hire); herr != nil {
			return refuseApply("The hire", "hire provision: ", herr, proposer)
		}
		// Copy the hire metadata onto the response so downstream
		// rendering (both the model-facing paraphrase and the UI)
		// can say "the agent is now live" with the specific slug
		// and role — a bare "Approved." would leave the recipient
		// guessing whether the side-effect fired, inducing them to
		// re-submit.
		h := *reqMsg.Hire
		// Drop the body + initial_agent_memory — they're already
		// persisted on the agent; duplicating here would bloat
		// every response file for no downstream benefit.
		h.Body = ""
		h.InitialAgentMemory = ""
		appliedHire = &h
	}

	// Role updates: same atomicity rule as Hire — apply role.md
	// overwrite BEFORE writing the approval response, so the ack
	// landing in the proposer's chat IS the signal the change is
	// live. Failure (unknown slug, write error) returns to the CEO;
	// the request stays in inbox for retry/deny.
	var appliedRoleUpdate *store.RoleUpdate
	if kind == responseKindApproval && approved != nil && *approved && reqMsg.RoleUpdate != nil {
		if s.Runtime == nil {
			return refuseCEOSaying(http.StatusInternalServerError, "role_update approval: no engine configured",
				"This server has no agent runtime, so the proposal cannot be applied", whoServer)
		}
		if rerr := s.Runtime.ApplyRoleUpdate(*reqMsg.RoleUpdate); rerr != nil {
			return refuseApply("The role update", "role_update apply: ", rerr, proposer)
		}
		// Mirror the Hire pattern: keep the slug on the response so
		// CoS sees which role was applied, but drop the body — it's
		// already on disk in the agent's role.md and duplicating
		// every replacement role into the response file would bloat
		// the messages directory.
		ru := *reqMsg.RoleUpdate
		ru.Body = ""
		appliedRoleUpdate = &ru
	}

	// Handbook updates: same atomicity rule as RoleUpdate.
	var appliedHandbookUpdate *store.HandbookUpdate
	if kind == responseKindApproval && approved != nil && *approved && reqMsg.HandbookUpdate != nil {
		if s.Runtime == nil {
			return refuseCEOSaying(http.StatusInternalServerError, "handbook_update approval: no engine configured",
				"This server has no agent runtime, so the proposal cannot be applied", whoServer)
		}
		if cerr := s.Runtime.ApplyHandbookUpdate(*reqMsg.HandbookUpdate); cerr != nil {
			return refuseApply("The handbook update", "handbook_update apply: ", cerr, proposer)
		}
		// Drop the body off the response (it's already on disk as the
		// handbook); leave a non-nil pointer so downstream
		// rendering can say "handbook is now live."
		appliedHandbookUpdate = &store.HandbookUpdate{}
	}

	// Offboards: same atomicity rule as Hire — archive the agent
	// BEFORE writing the approval response, so the ack landing in
	// CoS's chat IS the signal the departure is done. Failure (unknown
	// slug, still has direct reports, disk error) returns to the CEO
	// and the request stays in the inbox, which is what lets CoS land
	// a propose_reorg and have the CEO approve this same card after.
	//
	// The drain has to happen before the archive: an in-flight turn
	// keeps POSTing TurnEvents into the per-slug state, and the
	// archive renames chat.jsonl out from under those handlers. We
	// drain first and only then call ApplyOffboard, which is why this
	// block does more than the one-line Apply* calls above it.
	//
	// Draining is destructive and not undone if ApplyOffboard then
	// refuses — the agent's in-flight turn is already cancelled. That
	// is deliberate: the alternative is checking the preconditions,
	// draining, and archiving as three non-atomic steps, where the
	// window between check and archive is exactly when a turn can
	// start. A cancelled turn is recoverable (the CEO or CoS can
	// message the agent again); a half-archived agent is not.
	// Carried on the response for BOTH outcomes, unlike the Apply*
	// fields above: a denial needs to name who was spared just as much
	// as an approval needs to name who left. CoS can have several
	// proposals in flight, and "I denied your request" against a queue
	// of them says nothing. Slug only — the reason lives on the
	// request file this response replies to.
	var respOffboard *store.Offboard
	if kind == responseKindApproval && reqMsg.Offboard != nil {
		respOffboard = &store.Offboard{Slug: reqMsg.Offboard.Slug}
	}
	if kind == responseKindApproval && approved != nil && *approved && reqMsg.Offboard != nil {
		if s.Runtime == nil {
			return refuseCEOSaying(http.StatusInternalServerError, "offboard approval: no engine configured",
				"This server has no agent runtime, so the proposal cannot be applied", whoServer)
		}
		slug := reqMsg.Offboard.Slug
		// Who inherits the departing agent's open assignments: whoever they
		// reported to, read before the archive moves the record.
		inherits := agent.CEOSlug
		if a, aerr := s.Store.GetAgent(slug); aerr == nil && a.ReportsTo != "" {
			inherits = a.ReportsTo
		}
		s.drainSlugState(slug)
		if oerr := s.Runtime.ApplyOffboard(*reqMsg.Offboard); oerr != nil {
			return refuseApply("The offboard", "offboard apply: ", oerr, proposer)
		}
		// An archived agent cannot be woken, so its open assignments move
		// to its manager with a note on each (docs/developers/assignments.md §Rules).
		// Best effort after the archive: the departure is done either
		// way, and a failure here is visible on the Work board rather
		// than fatal.
		if tr := s.tracker(); tr != nil {
			note := fmt.Sprintf("%s was offboarded; this assignment moves to %s", slug, inherits)
			if n, rerr := tr.ReassignAll(ctx, slug, inherits, note); rerr != nil {
				log.Printf("offboard %s: reassign open assignments to %s: %v (%d moved)", slug, inherits, rerr, n)
			}
		}
		// Pod teardown logs and continues on failure. The agent is already archived, so failing the whole
		// approval here would leave the CEO staring at an error for an
		// action that in fact succeeded; a leaked pod is cleanable by
		// hand, a lost approval response is not.
		if s.AgentPod != nil {
			if derr := s.AgentPod.Destroy(ctx, slug); derr != nil {
				log.Printf("offboard %s: destroy agent pod: %v", slug, derr)
			}
		}
	}

	// Reorgs: best-effort per move (CoS converges stragglers from the
	// Failed list). Unlike the other Apply* paths, ApplyReorg never
	// 5xx's the whole approval — partial failure is the design. The
	// proposer-facing summary is appended to the response body so it
	// shows up in chat without an extra round-trip.
	var appliedReorg *store.Reorg
	if kind == responseKindApproval && approved != nil && *approved && reqMsg.Reorg != nil {
		if s.Runtime == nil {
			return refuseCEOSaying(http.StatusInternalServerError, "reorg approval: no engine configured",
				"This server has no agent runtime, so the proposal cannot be applied", whoServer)
		}
		result, rerr := s.Runtime.ApplyReorg(*reqMsg.Reorg)
		if rerr != nil {
			return refuseApply("The reorg", "reorg apply: ", rerr, proposer)
		}
		appliedReorg = &result
		body = appendReorgSummary(body, result)
	}

	resp := store.Message{
		Type:           respType,
		Title:          fmt.Sprintf("%s: %s", titlePrefix, reqMsg.Title),
		From:           agent.CEOSlug,
		To:             store.Recipients{reqMsg.From},
		Date:           time.Now().UTC(),
		InReplyTo:      reqPath,
		Approved:       approved,
		Hire:           appliedHire,
		RoleUpdate:     appliedRoleUpdate,
		HandbookUpdate: appliedHandbookUpdate,
		Reorg:          appliedReorg,
		Offboard:       respOffboard,
		Attachments:    attachments,
		Body:           body,
	}
	respAbs, err := s.Store.WriteMessage(resp)
	if err != nil {
		return refuseCEOSaying(http.StatusInternalServerError, "write response: "+err.Error(),
			"Your answer could not be saved", whoServer)
	}
	respRel, _ := filepath.Rel(s.Store.Root(), respAbs)

	// Record on the CEO's own chat timeline as a sent entry so the
	// inbox view pairs the thread correctly. Surface the error to the
	// CEO instead of swallowing it: pendingCEOInbox() pairs requests
	// to responses by walking these chat entries, so a missing
	// ceo_reply means the inbox count never decrements — exactly the
	// "ack didn't take" behavior users hit if this fails silently.
	ceoContent := resp.Title
	if body != "" {
		ceoContent = resp.Title + "\n\n" + body
	}
	if err := s.Store.AppendChatMessage(agent.CEOSlug, store.ChatMessage{
		Role:              store.RoleSent,
		Content:           ceoContent,
		Kind:              "ceo_reply",
		MessageRef:        respRel,
		ReplyToMessageRef: reqPath,
		Attachments:       attachments,
	}); err != nil {
		log.Printf("ceo reply: record on ceo chat: %v", err)
		return refuseCEOSaying(http.StatusInternalServerError, "ceo reply: record on ceo chat: "+err.Error(),
			"Your answer is saved but could not be recorded as answered, so the request stays in Needs you", whoServer)
	}

	// Instant-deliver to the original sender's chat.jsonl (skip the
	// release-state inbox queue). Happy-path notify rides on
	// DeliverToAgent → spawnChatLoopIfIdle → getOrCreateHub.
	//
	// If delivery fails (no Runtime, recipient archived, underlying
	// disk error), we surface the error rather than falling back to
	// the message queue: the queue is the Messenger's responsibility
	// and any direct mutation here would race Messenger.mu, leaving
	// inbox state corrupt. A response file destined for an unreachable
	// recipient is better as a 5xx the operator can see than as a
	// silent half-state.
	resp.Path = respAbs
	if s.Runtime == nil {
		return refuseCEOSaying(http.StatusInternalServerError, "ceo reply: no runtime configured to deliver response",
			"Your answer is saved, but this server has no agent runtime to deliver it", whoServer)
	}
	if !s.Messenger.DeliverToAgent(ctx, reqMsg.From, resp, respRel, s.deliveryHooks()) {
		return refuseCEOSaying(http.StatusInternalServerError, "ceo reply: could not deliver to "+reqMsg.From+" (agent archived or unreachable)",
			"Your answer is saved but could not be delivered to "+proposer, whoServer)
	}

	// The ack just resolved a CEO-inbox item. Push a fresh /org/stream
	// snapshot so the CEO-inbox count drops without waiting
	// for an unrelated event to wake the publisher. (DeliverToAgent
	// fires NotifyOrgState only on a fresh chat-loop spawn; the
	// already-running path doesn't, leaving the snapshot stale until
	// the next message-queue write.)
	s.NotifyOrgState()

	return ceoResponse{Request: reqMsg, Response: resp}, nil
}

// ingestCEOResponseAttachment writes an uploaded file into the blob
// store and returns its stored metadata.
func (s *Server) ingestCEOResponseAttachment(ctx context.Context, fh *multipart.FileHeader) (store.Attachment, error) {
	src, err := fh.Open()
	if err != nil {
		return store.Attachment{}, err
	}
	defer func() { _ = src.Close() }()
	return s.Store.AddAttachment(ctx, fh.Filename, src)
}

func boolPtr(b bool) *bool { return &b }

// appendReorgSummary tacks a human-readable per-move outcome list onto
// the CEO's optional response body so the proposer sees what landed
// without parsing the structured Reorg sub-object. Pure markdown so it
// renders cleanly in the proposer's chat.
func appendReorgSummary(body string, r store.Reorg) string {
	var b strings.Builder
	if strings.TrimSpace(body) != "" {
		b.WriteString(strings.TrimRight(body, "\n"))
		b.WriteString("\n\n")
	}
	fmt.Fprintf(&b, "Reorg outcome: %d applied, %d failed.\n", len(r.Applied), len(r.Failed))
	for _, m := range r.Applied {
		fmt.Fprintf(&b, "- %s → %s ✓\n", m.Slug, m.NewManager)
	}
	for _, f := range r.Failed {
		fmt.Fprintf(&b, "- %s → %s ✗ (%s)\n", f.Move.Slug, f.Move.NewManager, f.Reason)
	}
	return b.String()
}
