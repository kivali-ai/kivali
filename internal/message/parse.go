package message

import (
	"encoding/json"
	"errors"
	"fmt"
	"path"
	"strings"
	"time"

	"github.com/kivali-ai/kivali/internal/provider"
	"github.com/kivali-ai/kivali/internal/roleicon"
	"github.com/kivali-ai/kivali/internal/store"
)

// ParseContext is the caller-supplied context needed to release a tool_use
// block into a persistable message. From is the slug of the calling
// agent; Now is the timestamp to stamp on the message (zero value →
// time.Now()).
//
// ResolveBodyPath is invoked when a publish_* tool supplies a *_path
// field instead of an inline body. The callback reads the file from
// the sender's /files/ view and returns its contents. The caller
// usually builds it from a files.Backend. When nil, path refs are
// rejected at parse time.
type ParseContext struct {
	From            string
	Now             time.Time
	ResolveBodyPath func(path string) (string, error)
}

// MaxMessageBodyBytes caps every model-authored "message" field
// (publish_*'s body, propose_*'s rationale). The intent is force a
// distinction between *messages* (short, capped, inline) and
// *artifacts* (long, by /files/ path, capped only by the agent's
// disk quota). Enforced at parse time; oversized fields surface as
// a clear "this is a message, not a report — split the substance
// into an attachment / artifact" error so the model recovers in
// the same turn.
const MaxMessageBodyBytes = 4096

// MaxNoticeBodyBytes is the tighter cap on a notice body. A notice is
// a tell: the recipient owes nothing back, so the body only has to
// carry enough for them to know the thing and find the substance. The
// existing "substance goes in an attachment" pattern applies, just
// from half the runway.
const MaxNoticeBodyBytes = 2048

// validateMessageField caps a model-authored message field at
// MaxMessageBodyBytes. trimmed = strings.TrimSpace(value) so a
// field full of whitespace counts as empty (matches the rest of
// the parser's "is it set" semantics). Returns the trimmed value
// on success.
func validateMessageField(fieldName, value string) (string, error) {
	return validateMessageFieldCapped(fieldName, value, MaxMessageBodyBytes)
}

// validateMessageFieldCapped is validateMessageField with an explicit
// cap, so `notice` can enforce MaxNoticeBodyBytes while every other
// field keeps MaxMessageBodyBytes and the error text stays identical
// in shape.
func validateMessageFieldCapped(fieldName, value string, capBytes int) (string, error) {
	if len(value) > capBytes {
		return "", fmt.Errorf("%s is %d bytes; cap is %d. %s is a short message — substantive content goes in an attachment (`attachments: [{path: \"/files/artifacts/private/...\"}]`) or, for propose_*, the artifact path field",
			fieldName, len(value), capBytes, fieldName)
	}
	return value, nil
}

// resolveArtifactPath reads the bytes at pathRef via
// ctx.ResolveBodyPath. Used for artifact fields (role_path,
// handbook_path, initial_memory_path) — required path refs with
// no inline form. fieldName is used in error messages so the agent
// knows which input went wrong.
func (ctx ParseContext) resolveArtifactPath(fieldName, pathRef string) (string, error) {
	if strings.TrimSpace(pathRef) == "" {
		return "", fmt.Errorf("%s is required (path to /files/...)", fieldName)
	}
	if ctx.ResolveBodyPath == nil {
		return "", fmt.Errorf("%s: path refs not available in this context", fieldName)
	}
	body, err := ctx.ResolveBodyPath(pathRef)
	if err != nil {
		return "", fmt.Errorf("%s %q: %w", fieldName, pathRef, err)
	}
	return body, nil
}

// resolveOptionalArtifactPath is the optional-field variant: empty
// pathRef returns ("", nil) instead of an error. Used for
// initial_memory_path on hire (an agent can be seeded without any
// initial memory).
func (ctx ParseContext) resolveOptionalArtifactPath(fieldName, pathRef string) (string, error) {
	if strings.TrimSpace(pathRef) == "" {
		return "", nil
	}
	return ctx.resolveArtifactPath(fieldName, pathRef)
}

// ParsedAttachment is a fully-materialized attachment emitted by an
// agent alongside a message. Path-only after the attachments
// collapse: the agent referenced a file under /files/ — their own
// artifacts/{private,public}, a project file, or an attachment in
// their chat history. The engine ingests via
// agent.ResolveShareFilePath, which content-addresses the bytes and
// returns the resulting SHA. The model never sees SHAs in the
// attachment input — content addressing is server-side plumbing.
type ParsedAttachment struct {
	Name string
	Path string
}

// ParseResult bundles everything a parse produces. Attachments is
// non-empty only for message types
// that carry an `attachments` field in their schema (notice,
// ceo_approval_request, ceo_notification).
type ParseResult struct {
	Message     store.Message
	Attachments []ParsedAttachment
}

// ErrUnknownTool is returned when a tool_use block carries a name that
// isn't one of the publish / propose tools.
var ErrUnknownTool = errors.New("message: unknown tool")

// Parse converts a tool_use content block into a ParseResult ready for
// persistence by the caller.
func Parse(block provider.ContentBlock, ctx ParseContext) (ParseResult, error) {
	if block.Type != provider.ContentToolUse {
		return ParseResult{}, fmt.Errorf("message: expected tool_use block, got %q", block.Type)
	}
	if ctx.Now.IsZero() {
		ctx.Now = time.Now().UTC()
	}
	switch block.ToolName {
	case ToolNotice:
		return parseNotice(block.ToolInput, ctx)
	case ToolCEOApprovalRequest:
		return parseCEOApprovalRequest(block.ToolInput, ctx)
	case ToolCEONotification:
		return parseCEONotification(block.ToolInput, ctx)
	case ToolProposeRoleUpdate:
		return parseProposeRoleUpdate(block.ToolInput, ctx)
	case ToolProposeHandbookUpdate:
		return parseProposeHandbookUpdate(block.ToolInput, ctx)
	case ToolProposeReorg:
		return parseProposeReorg(block.ToolInput, ctx)
	case ToolProposeHire:
		return parseProposeHire(block.ToolInput, ctx)
	case ToolProposeOffboard:
		return parseProposeOffboard(block.ToolInput, ctx)
	}
	return ParseResult{}, fmt.Errorf("%w: %q", ErrUnknownTool, block.ToolName)
}

type attachmentInput struct {
	Path string `json:"path"`
	Name string `json:"name"`
}

// noticeInput is the on-wire shape of publish_notice. `to` is a list —
// one notice can address several agents — and there is deliberately no
// in_reply_to field: strictUnmarshal rejects unknown fields, so a
// model that tries to thread a notice gets a parse error naming the
// field rather than a silently-dropped value.
type noticeInput struct {
	To          []string          `json:"to"`
	Title       string            `json:"title"`
	Body        string            `json:"body"`
	Attachments []attachmentInput `json:"attachments,omitempty"`
}

func parseNotice(raw json.RawMessage, ctx ParseContext) (ParseResult, error) {
	var in noticeInput
	if err := strictUnmarshal(raw, &in); err != nil {
		return ParseResult{}, fmt.Errorf("notice: %w", err)
	}
	if err := requireFields(map[string]string{"title": in.Title}); err != nil {
		return ParseResult{}, fmt.Errorf("notice: %w", err)
	}
	to, err := parseNoticeRecipients(in.To)
	if err != nil {
		return ParseResult{}, fmt.Errorf("notice: %w", err)
	}
	body, err := validateMessageFieldCapped("body", in.Body, MaxNoticeBodyBytes)
	if err != nil {
		return ParseResult{}, fmt.Errorf("notice: %w", err)
	}
	atts, err := parseAttachments(in.Attachments)
	if err != nil {
		return ParseResult{}, fmt.Errorf("notice: %w", err)
	}
	return ParseResult{
		Message: store.Message{
			Type:  store.MsgNotice,
			Title: in.Title,
			From:  ctx.From,
			To:    to,
			Date:  ctx.Now,
			Body:  body,
		},
		Attachments: atts,
	}, nil
}

// parseNoticeRecipients normalizes, validates, and de-duplicates the
// recipient list. Order is the order the sender wrote, minus repeats —
// deterministic because the filename and the "awaiting delivery to A,
// B" rendering both derive from it.
func parseNoticeRecipients(raw []string) (store.Recipients, error) {
	if len(raw) == 0 {
		return nil, fmt.Errorf("to: at least one recipient is required")
	}
	out := make(store.Recipients, 0, len(raw))
	seen := map[string]bool{}
	for i, r := range raw {
		slug := normalizeSlug(r)
		if slug == "" {
			return nil, fmt.Errorf("to[%d] is empty", i)
		}
		if err := ValidateSlug(slug); err != nil {
			return nil, fmt.Errorf("to[%d]: %w", i, err)
		}
		if slug == CEO {
			return nil, fmt.Errorf("to[%d]: %q is reserved — use publish_ceo_notification to tell the owner something", i, CEO)
		}
		if seen[slug] {
			continue
		}
		seen[slug] = true
		out = append(out, slug)
	}
	return out, nil
}

// noticeAskPhrases are the deterministic markers of an ask hiding in a
// notice body. Matching is plain lowercase substring containment — no
// model call at publish time, now or later.
var noticeAskPhrases = []string{"please", "can you", "could you", "would you", "need you to", "let me know", "get back to me"}

// NoticeAskLint returns a warning to append to the publish tool result
// when a notice body reads like an ask, or "" when it doesn't. It is a
// nudge, not a rejection: the CEO moderates every notice, and a
// question mark in a body is sometimes just prose. What the warning
// buys is a signal at the moment the model can still choose the right
// tool.
//
// Deterministic string matching only, by operator rule: no runtime LLM
// calls outside agent turns.
func NoticeAskLint(m store.Message) string {
	if m.Type != store.MsgNotice {
		return ""
	}
	body := strings.ToLower(m.Body)
	reason := ""
	if strings.Contains(body, "?") {
		reason = "it asks a question"
	}
	for _, phrase := range noticeAskPhrases {
		if strings.Contains(body, phrase) {
			reason = fmt.Sprintf("it contains %q", phrase)
			break
		}
	}
	if reason == "" {
		return ""
	}
	return fmt.Sprintf("NOTE: this notice reads like an ask — %s. Nobody can reply to a notice, so an ask inside one goes unanswered. If you need someone to act, open an assignment with assignment_create for the one agent who owns it. If you need nothing back, ignore this note.", reason)
}

// hireInput is the set of hire fields `propose_hire` carries.
type hireInput struct {
	Slug              string `json:"slug"`
	Role              string `json:"role"`
	Icon              string `json:"icon"`
	ReportsTo         string `json:"reports_to"`
	RolePath          string `json:"role_path"`
	InitialMemoryPath string `json:"initial_memory_path"`
}

// ceoInput is the shared shape of CEO-bound publish tool inputs:
// title + capped inline body + optional attachments. `to` is implicit
// (always CEO).
type ceoInput struct {
	Title       string            `json:"title"`
	Body        string            `json:"body"`
	Attachments []attachmentInput `json:"attachments,omitempty"`
}

func parseCEOApprovalRequest(raw json.RawMessage, ctx ParseContext) (ParseResult, error) {
	return parseCEOBound(raw, ctx, "ceo_approval_request", store.MsgCEOApprovalRequest)
}

func parseCEONotification(raw json.RawMessage, ctx ParseContext) (ParseResult, error) {
	return parseCEOBound(raw, ctx, "ceo_notification", store.MsgCEONotification)
}

func parseCEOBound(raw json.RawMessage, ctx ParseContext, label string, mt store.MessageType) (ParseResult, error) {
	var in ceoInput
	if err := strictUnmarshal(raw, &in); err != nil {
		return ParseResult{}, fmt.Errorf("%s: %w", label, err)
	}
	if err := requireFields(map[string]string{"title": in.Title}); err != nil {
		return ParseResult{}, fmt.Errorf("%s: %w", label, err)
	}
	body, err := validateMessageField("body", in.Body)
	if err != nil {
		return ParseResult{}, fmt.Errorf("%s: %w", label, err)
	}
	atts, err := parseAttachments(in.Attachments)
	if err != nil {
		return ParseResult{}, fmt.Errorf("%s: %w", label, err)
	}
	return ParseResult{
		Message: store.Message{
			Type:  mt,
			Title: in.Title,
			From:  ctx.From,
			To:    store.Recipients{CEO},
			Date:  ctx.Now,
			Body:  body,
		},
		Attachments: atts,
	}, nil
}

// parseProposeHire turns a `propose_hire` call into a
// ceo_approval_request carrying a Hire sub-object. The CEO approving
// it is what provisions the agent (see agent.Runtime.ApplyHire) — this
// only stages the proposal.
func parseProposeHire(raw json.RawMessage, ctx ParseContext) (ParseResult, error) {
	// Defense-in-depth: the tool list omits this tool for non-CoS
	// agents, but reject at parse too. Mirrors parseProposeReorg.
	if ctx.From != ChiefOfStaff {
		return ParseResult{}, fmt.Errorf("propose_hire: only the Chief of Staff may propose hires (caller: %q)", ctx.From)
	}
	var in proposeHireInput
	if err := strictUnmarshal(raw, &in); err != nil {
		return ParseResult{}, fmt.Errorf("propose_hire: %w", err)
	}
	if err := requireFields(map[string]string{"title": in.Title}); err != nil {
		return ParseResult{}, fmt.Errorf("propose_hire: %w", err)
	}
	hire, err := parseHire(in.hireInput, ctx)
	if err != nil {
		return ParseResult{}, fmt.Errorf("propose_hire: %w", err)
	}
	if hire.Slug == CEO || hire.Slug == ChiefOfStaff {
		return ParseResult{}, fmt.Errorf("propose_hire: slug %q is reserved", hire.Slug)
	}
	rationale, err := validateMessageField("rationale", in.Rationale)
	if err != nil {
		return ParseResult{}, fmt.Errorf("propose_hire: %w", err)
	}
	return ParseResult{
		Message: store.Message{
			Type:  store.MsgCEOApprovalRequest,
			Title: in.Title,
			From:  ctx.From,
			To:    store.Recipients{CEO},
			Date:  ctx.Now,
			Body:  rationale,
			Hire:  hire,
		},
	}, nil
}

// proposeHireInput is the on-wire shape of the `propose_hire` tool:
// the hire fields themselves plus the CEO-facing title and rationale
// every propose_* tool carries.
type proposeHireInput struct {
	hireInput
	Title     string `json:"title"`
	Rationale string `json:"rationale"`
}

// proposeOffboardInput is the on-wire shape of the `propose_offboard`
// tool. One slug, deliberately — see store.Offboard.
type proposeOffboardInput struct {
	Title     string `json:"title"`
	Slug      string `json:"slug"`
	Rationale string `json:"rationale"`
}

// parseProposeOffboard turns a `propose_offboard` call into a
// ceo_approval_request carrying an Offboard sub-object. The CEO
// approving it is what archives the agent (see
// agent.Runtime.ApplyOffboard) — this only stages the proposal.
//
// The "has no direct reports" precondition is deliberately NOT checked
// here: the parser has no store. It is enforced at publish time as a
// friendly early warning (mcp.StorePublisher) and at apply time as the
// real gate, which is the one that matters — the org chart can change
// between proposing and approving.
func parseProposeOffboard(raw json.RawMessage, ctx ParseContext) (ParseResult, error) {
	if ctx.From != ChiefOfStaff {
		return ParseResult{}, fmt.Errorf("propose_offboard: only the Chief of Staff may propose offboarding (caller: %q)", ctx.From)
	}
	var in proposeOffboardInput
	if err := strictUnmarshal(raw, &in); err != nil {
		return ParseResult{}, fmt.Errorf("propose_offboard: %w", err)
	}
	if err := requireFields(map[string]string{"title": in.Title, "slug": in.Slug}); err != nil {
		return ParseResult{}, fmt.Errorf("propose_offboard: %w", err)
	}
	slug := normalizeSlug(in.Slug)
	if err := ValidateSlug(slug); err != nil {
		return ParseResult{}, fmt.Errorf("propose_offboard: slug: %w", err)
	}
	// The CEO is the human root, and the Chief of Staff is the role
	// that would have to propose its own replacement.
	if slug == CEO {
		return ParseResult{}, fmt.Errorf("propose_offboard: cannot offboard %q (the owner is the human at the root of the org)", CEO)
	}
	if slug == ChiefOfStaff {
		return ParseResult{}, fmt.Errorf("propose_offboard: cannot offboard %q (it is load-bearing — it is the only role that can propose org changes, including this one)", ChiefOfStaff)
	}
	rationale, err := validateMessageField("rationale", in.Rationale)
	if err != nil {
		return ParseResult{}, fmt.Errorf("propose_offboard: %w", err)
	}
	return ParseResult{
		Message: store.Message{
			Type:     store.MsgCEOApprovalRequest,
			Title:    in.Title,
			From:     ctx.From,
			To:       store.Recipients{CEO},
			Date:     ctx.Now,
			Body:     rationale,
			Offboard: &store.Offboard{Slug: slug, Reason: rationale},
		},
	}, nil
}

func parseHire(in hireInput, ctx ParseContext) (*store.Hire, error) {
	slug := normalizeSlug(in.Slug)
	reportsTo := normalizeSlug(in.ReportsTo)
	if err := requireFields(map[string]string{
		"slug":       slug,
		"role":       in.Role,
		"icon":       in.Icon,
		"reports_to": reportsTo,
	}); err != nil {
		return nil, err
	}
	if err := roleicon.Check(in.Icon); err != nil {
		return nil, fmt.Errorf("icon: %w", err)
	}
	if err := ValidateSlug(slug); err != nil {
		return nil, fmt.Errorf("slug: %w", err)
	}
	if err := ValidateSlug(reportsTo); err != nil {
		return nil, fmt.Errorf("reports_to: %w", err)
	}
	body, err := ctx.resolveArtifactPath("role_path", in.RolePath)
	if err != nil {
		return nil, err
	}
	memory, err := ctx.resolveOptionalArtifactPath("initial_memory_path", in.InitialMemoryPath)
	if err != nil {
		return nil, err
	}
	return &store.Hire{
		Slug:               slug,
		Role:               in.Role,
		Icon:               in.Icon,
		ReportsTo:          reportsTo,
		Body:               body,
		InitialAgentMemory: memory,
	}, nil
}

// proposeRoleUpdateInput is the on-wire shape of the
// `propose_role_update` tool. Distinct from hireInput because the
// fields it carries are different — there's no Role / ReportsTo /
// initial_agent_memory split, and "title" + "rationale" replace the
// usual approval request body fields. The tool generates a
// ceo_approval_request with a RoleUpdate sub-object so the CEO inbox
// applies the change atomically on approve.
type proposeRoleUpdateInput struct {
	Slug      string `json:"slug"`
	Title     string `json:"title"`
	RolePath  string `json:"role_path"`
	Rationale string `json:"rationale"`
}

func parseProposeRoleUpdate(raw json.RawMessage, ctx ParseContext) (ParseResult, error) {
	// Defense-in-depth: the tool list omits this tool for non-CoS
	// agents, but the parser rejects too. Any non-CoS slug calling
	// it (e.g. a stale tool list, an SDK client out of sync, a model
	// hallucinating the tool) gets a clear refusal at the boundary
	// instead of writing an unauthorized approval request.
	if ctx.From != ChiefOfStaff {
		return ParseResult{}, fmt.Errorf("propose_role_update: only the Chief of Staff may propose role updates (caller: %q)", ctx.From)
	}
	var in proposeRoleUpdateInput
	if err := strictUnmarshal(raw, &in); err != nil {
		return ParseResult{}, fmt.Errorf("propose_role_update: %w", err)
	}
	slug := normalizeSlug(in.Slug)
	if err := requireFields(map[string]string{
		"slug":  slug,
		"title": in.Title,
	}); err != nil {
		return ParseResult{}, fmt.Errorf("propose_role_update: %w", err)
	}
	if err := ValidateSlug(slug); err != nil {
		return ParseResult{}, fmt.Errorf("propose_role_update: slug: %w", err)
	}
	if slug == CEO {
		return ParseResult{}, fmt.Errorf("propose_role_update: slug %q is reserved", CEO)
	}
	body, err := ctx.resolveArtifactPath("role_path", in.RolePath)
	if err != nil {
		return ParseResult{}, fmt.Errorf("propose_role_update: %w", err)
	}
	if strings.TrimSpace(body) == "" {
		return ParseResult{}, fmt.Errorf("propose_role_update: role_path resolves to empty content; role.md must be non-empty")
	}
	rationale, err := validateMessageField("rationale", in.Rationale)
	if err != nil {
		return ParseResult{}, fmt.Errorf("propose_role_update: %w", err)
	}
	return ParseResult{
		Message: store.Message{
			Type:  store.MsgCEOApprovalRequest,
			Title: in.Title,
			From:  ctx.From,
			To:    store.Recipients{CEO},
			Date:  ctx.Now,
			Body:  rationale,
			RoleUpdate: &store.RoleUpdate{
				Slug: slug,
				Body: body,
			},
		},
	}, nil
}

// proposeHandbookUpdateInput is the JSON shape of a CoS call to
// the `propose_handbook_update` tool. Mirrors proposeRoleUpdateInput
// minus the slug — the handbook is a singleton.
type proposeHandbookUpdateInput struct {
	Title        string `json:"title"`
	HandbookPath string `json:"handbook_path"`
	Rationale    string `json:"rationale"`
}

func parseProposeHandbookUpdate(raw json.RawMessage, ctx ParseContext) (ParseResult, error) {
	const tool = ToolProposeHandbookUpdate
	// Defense-in-depth: the tool list omits this tool for non-CoS
	// agents, but the parser rejects too. Mirrors parseProposeRoleUpdate.
	if ctx.From != ChiefOfStaff {
		return ParseResult{}, fmt.Errorf("%s: only the Chief of Staff may propose handbook updates (caller: %q)", tool, ctx.From)
	}
	var in proposeHandbookUpdateInput
	if err := strictUnmarshal(raw, &in); err != nil {
		return ParseResult{}, fmt.Errorf("%s: %w", tool, err)
	}
	if err := requireFields(map[string]string{
		"title": in.Title,
	}); err != nil {
		return ParseResult{}, fmt.Errorf("%s: %w", tool, err)
	}
	const field = "handbook_path"
	body, err := ctx.resolveArtifactPath(field, in.HandbookPath)
	if err != nil {
		return ParseResult{}, fmt.Errorf("%s: %w", tool, err)
	}
	if strings.TrimSpace(body) == "" {
		return ParseResult{}, fmt.Errorf("%s: %s resolves to empty content; the handbook must be non-empty", tool, field)
	}
	rationale, err := validateMessageField("rationale", in.Rationale)
	if err != nil {
		return ParseResult{}, fmt.Errorf("%s: %w", tool, err)
	}
	return ParseResult{
		Message: store.Message{
			Type:  store.MsgCEOApprovalRequest,
			Title: in.Title,
			From:  ctx.From,
			To:    store.Recipients{CEO},
			Date:  ctx.Now,
			Body:  rationale,
			HandbookUpdate: &store.HandbookUpdate{
				Body: body,
			},
		},
	}, nil
}

// proposeReorgInput is the on-wire shape of the `propose_reorg` tool.
// Mirrors the propose_role_update / propose_handbook_update shape
// minus a single body — reorg payload is structured (a list of moves),
// not free markdown.
type proposeReorgInput struct {
	Title     string             `json:"title"`
	Moves     []proposeReorgMove `json:"moves"`
	Rationale string             `json:"rationale"`
}

type proposeReorgMove struct {
	Slug       string `json:"slug"`
	NewManager string `json:"new_manager"`
}

func parseProposeReorg(raw json.RawMessage, ctx ParseContext) (ParseResult, error) {
	// Defense-in-depth: tool list omits this tool for non-CoS agents,
	// but reject at parse too. Mirrors parseProposeRoleUpdate.
	if ctx.From != ChiefOfStaff {
		return ParseResult{}, fmt.Errorf("propose_reorg: only the Chief of Staff may propose reorgs (caller: %q)", ctx.From)
	}
	var in proposeReorgInput
	if err := strictUnmarshal(raw, &in); err != nil {
		return ParseResult{}, fmt.Errorf("propose_reorg: %w", err)
	}
	if err := requireFields(map[string]string{"title": in.Title}); err != nil {
		return ParseResult{}, fmt.Errorf("propose_reorg: %w", err)
	}
	if len(in.Moves) == 0 {
		return ParseResult{}, fmt.Errorf("propose_reorg: moves: at least one move required")
	}
	moves := make([]store.ReorgMove, 0, len(in.Moves))
	seen := map[string]int{} // slug → first index, used to detect duplicate targets
	for i, m := range in.Moves {
		slug := normalizeSlug(m.Slug)
		newManager := normalizeSlug(m.NewManager)
		if slug == "" {
			return ParseResult{}, fmt.Errorf("propose_reorg: moves[%d].slug is required", i)
		}
		if newManager == "" {
			return ParseResult{}, fmt.Errorf("propose_reorg: moves[%d].new_manager is required", i)
		}
		if err := ValidateSlug(slug); err != nil {
			return ParseResult{}, fmt.Errorf("propose_reorg: moves[%d].slug: %w", i, err)
		}
		if err := ValidateSlug(newManager); err != nil {
			return ParseResult{}, fmt.Errorf("propose_reorg: moves[%d].new_manager: %w", i, err)
		}
		if slug == CEO {
			return ParseResult{}, fmt.Errorf("propose_reorg: moves[%d]: cannot move %q (the owner is the root)", i, CEO)
		}
		if slug == newManager {
			return ParseResult{}, fmt.Errorf("propose_reorg: moves[%d]: cannot reparent %q under itself", i, slug)
		}
		if prev, dup := seen[slug]; dup {
			return ParseResult{}, fmt.Errorf("propose_reorg: moves[%d]: %q already moved at moves[%d]; one destination per slug", i, slug, prev)
		}
		seen[slug] = i
		moves = append(moves, store.ReorgMove{Slug: slug, NewManager: newManager})
	}
	rationale, err := validateMessageField("rationale", in.Rationale)
	if err != nil {
		return ParseResult{}, fmt.Errorf("propose_reorg: %w", err)
	}
	return ParseResult{
		Message: store.Message{
			Type:  store.MsgCEOApprovalRequest,
			Title: in.Title,
			From:  ctx.From,
			To:    store.Recipients{CEO},
			Date:  ctx.Now,
			Body:  rationale,
			Reorg: &store.Reorg{Moves: moves},
		},
	}, nil
}

func parseAttachments(raw []attachmentInput) ([]ParsedAttachment, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	out := make([]ParsedAttachment, 0, len(raw))
	for i, a := range raw {
		pa, err := materializeAttachment(i, a)
		if err != nil {
			return nil, err
		}
		out = append(out, pa)
	}
	return out, nil
}

// materializeAttachment validates that the attachment carries a
// path. Every attachment is a /files/ path the agent has visibility into,
// and the dispatcher (which has the store + agent slug) ingests +
// access-checks via agent.ResolveShareFilePath. Name defaults to
// the path basename when caller doesn't override.
func materializeAttachment(i int, a attachmentInput) (ParsedAttachment, error) {
	cleanPath := strings.TrimSpace(a.Path)
	if cleanPath == "" {
		return ParsedAttachment{}, fmt.Errorf("attachment %d: path is required (attachments are by path only — write the bytes under /files/artifacts/private/ first and reference that path)", i)
	}
	name := strings.TrimSpace(a.Name)
	if name == "" {
		name = path.Base(strings.TrimSuffix(cleanPath, "/"))
	}
	return ParsedAttachment{Name: name, Path: cleanPath}, nil
}

func strictUnmarshal(raw json.RawMessage, v any) error {
	if len(raw) == 0 {
		return errors.New("empty tool input")
	}
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return err
	}
	return nil
}

func requireFields(fields map[string]string) error {
	var missing []string
	for name, v := range fields {
		if strings.TrimSpace(v) == "" {
			missing = append(missing, name)
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf("missing required fields: %v", missing)
	}
	return nil
}

func normalizeSlug(s string) string {
	return strings.ToLower(strings.TrimSpace(s))
}

// ValidateSlug enforces the slug format (lowercase letters, digits,
// hyphens; no underscore prefix; non-empty). Used at message-parse
// time AND defensively at hire time so a future code path that
// constructs a Hire bypassing the parser still gets the check.
func ValidateSlug(s string) error {
	if s == "" {
		return errors.New("empty slug")
	}
	if strings.HasPrefix(s, "_") {
		return fmt.Errorf("slug %q: underscore prefix reserved", s)
	}
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z':
		case r >= '0' && r <= '9':
		case r == '-':
		default:
			return fmt.Errorf("slug %q: only lowercase, digits, and hyphens allowed", s)
		}
	}
	return nil
}
