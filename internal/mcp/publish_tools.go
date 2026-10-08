package mcp

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/kivali-ai/kivali/internal/agent"
	"github.com/kivali-ai/kivali/internal/files"
	"github.com/kivali-ai/kivali/internal/message"
	"github.com/kivali-ai/kivali/internal/provider"
	"github.com/kivali-ai/kivali/internal/store"
)

// PublishDispatcher executes one publish_* tool invocation, returning
// the rendered tool_result body + tool-level error flag. Two impls:
//
//   - storePublishDispatcher (this file) — in-process: runs the
//     two-phase pipeline (StorePublisher.StagePublish followed by
//     StorePublisher.CommitPublish) inline. Used by tests where the
//     bridge isn't involved.
//   - agentpodPublishDispatcher (internal/mcp/agentpod_bridge.go) —
//     in-pod MCP subprocess: calls /publish/stage then /publish/commit
//     over UDS, retrying commit on transport failure (the bridge can
//     do this safely because commit is idempotent on StageID).
type PublishDispatcher interface {
	DispatchPublish(ctx context.Context, tool string, raw json.RawMessage) (body string, isError bool, err error)
}

// PublishToolsConfig wires the inter-agent publish tools (notice,
// ceo_approval_request, ceo_notification, plus the CoS-only propose_*
// family) into an MCP server. The toolkit emits one tool definition
// per supported message tool; every handler funnels through
// Dispatcher.
//
// IsChiefOfStaff controls which tool list message.ToolsFor emits — CoS
// additionally gets the org-mutating proposals (message.ProposeTools).
type PublishToolsConfig struct {
	Dispatcher     PublishDispatcher
	From           string
	IsChiefOfStaff bool
}

// PublishTools returns MCP Tool definitions for the publish tool
// family. Tool list shape is taken verbatim from message.ToolsFor so
// the schemas stay in lock-step with the native-API path.
func PublishTools(cfg PublishToolsConfig) []Tool {
	if cfg.Dispatcher == nil {
		return nil
	}
	defs := message.ToolsFor(cfg.From, cfg.IsChiefOfStaff)
	out := make([]Tool, 0, len(defs))
	for _, d := range defs {
		d := d // capture per iteration
		out = append(out, Tool{
			Name:        d.Name,
			Description: d.Description,
			InputSchema: d.Schema,
			Handler:     publishHandler(cfg.Dispatcher, d.Name),
		})
	}
	return out
}

// publishHandler funnels one publish_* invocation through the
// dispatcher and wraps the result as a ToolResult. Tool-level
// failures ride on isError; transport errors surface as the
// handler's error return.
func publishHandler(d PublishDispatcher, toolName string) func(context.Context, json.RawMessage) (*ToolResult, error) {
	return func(ctx context.Context, arguments json.RawMessage) (*ToolResult, error) {
		body, isErr, err := d.DispatchPublish(ctx, toolName, arguments)
		if err != nil {
			return &ToolResult{IsError: true, Content: []string{toolName + ": " + err.Error()}}, nil
		}
		return &ToolResult{IsError: isErr, Content: []string{body}}, nil
	}
}

// StorePublishDeps is the dependency bundle for the in-process
// two-phase publisher. ApplyRoute is allowed to be nil — the message
// is still persisted and the rendered body just doesn't note any
// routing (the "persist-only" mode used by tests that exercise the
// publish parser without a messenger).
type StorePublishDeps struct {
	Store             *store.FSStore
	FilesystemBackend *files.Backend
	From              string
	// ApplyRoute applies routing rules to msg, mutating q in place
	// (typically appending msg.Path to q.Agents[msg.To].Inbox, or
	// dead-letter handling). Called inside CommitPublish while
	// holding LockMessageQueue, so the implementation MUST NOT
	// acquire the queue lock itself; it must operate on the unlocked
	// queue passed in. The web layer wires this to
	// messaging.Messenger.RouteProduced.
	ApplyRoute func(ctx context.Context, q *store.MessageQueue, msg store.Message) error
}

// StorePublisher is the in-process two-phase publish engine. Exposes
// StagePublish + CommitPublish so the agent-pod HTTP handler can
// drive them from distinct endpoints. The in-process DispatchPublish
// path (NewStorePublishDispatcher) calls them back-to-back.
type StorePublisher struct {
	deps StorePublishDeps
}

// NewStorePublisher constructs a two-phase publisher against deps.
// Safe for concurrent use — all serialization happens via the store's
// queue lock (commit phase) and writeAtomic (stage record + message).
func NewStorePublisher(deps StorePublishDeps) *StorePublisher {
	return &StorePublisher{deps: deps}
}

// StagePublish runs phase 1: parse the tool input, run the duplicate-
// role guard, resolve attachments, pin msg.Date, and persist a staged-
// publish record on disk. Returns (stageID, "", false, nil) on
// success; the body is intentionally empty — it's rendered at commit
// time when the queue state is observable under the lock.
//
// On a deterministic input-side failure (parse error, role conflict,
// unreachable attachment), returns ("", body, true, nil) with the
// model-visible error text in body. No staging record is created;
// the bridge surfaces the body directly to the model without retry
// because the inputs aren't going to parse differently next time.
//
// Returns a non-nil error only on transport-class faults (disk write
// failure of the staged record); the caller decides whether to retry.
func (p *StorePublisher) StagePublish(ctx context.Context, toolName string, arguments json.RawMessage) (stageID, body string, isError bool, err error) {
	pctx := message.ParseContext{From: p.deps.From}
	if p.deps.FilesystemBackend != nil {
		pctx.ResolveBodyPath = bodyPathResolver(p.deps.FilesystemBackend)
	}
	cb := provider.ContentBlock{
		Type:      provider.ContentToolUse,
		ToolName:  toolName,
		ToolInput: arguments,
		ToolUseID: "mcp-" + toolName,
	}
	pr, perr := message.Parse(cb, pctx)
	if perr != nil {
		return "", "parse: " + perr.Error(), true, nil
	}
	msg := pr.Message
	if reason := p.checkRecipientSlugs(msg); reason != "" {
		return "", reason, true, nil
	}
	if reason := p.checkOffboardPreconditions(msg); reason != "" {
		return "", reason, true, nil
	}
	for _, pa := range pr.Attachments {
		// Path-only after the attachments collapse: route through the
		// same resolver share_file uses (same access rules — artifacts
		// /private + /public are the agent's own files; project +
		// attachments are referenceable open content). The resolver
		// content-addresses the bytes via AddAttachment and returns
		// the SHA.
		resolved, rerr := agent.ResolveShareFilePath(ctx, p.deps.Store, p.deps.From, pa.Path)
		if rerr != nil {
			return "", "attachment: " + rerr.Error(), true, nil
		}
		msg.Attachments = append(msg.Attachments, store.MessageAttachment{SHA: resolved.SHA, Name: pa.Name})
	}
	// Pin Date NOW so the deterministic filename — and thus the
	// idempotent commit — is locked in at stage time. Without this
	// pin, WriteMessage would set Date on its own and a retry would
	// land in a different filename.
	if msg.Date.IsZero() {
		msg.Date = nowFunc()
	}
	id := newStageID()
	rec := store.StagedPublish{
		StageID:   id,
		Tool:      toolName,
		From:      p.deps.From,
		Message:   msg,
		CreatedAt: msg.Date,
	}
	if werr := p.deps.Store.WriteStagedPublish(rec); werr != nil {
		return "", "", false, fmt.Errorf("stage: %w", werr)
	}
	return id, "", false, nil
}

// CommitPublish runs phase 2: looks up the staged record, writes the
// message file at its deterministic path, atomically appends the
// recipient inbox entry AND records stageID in the queue's
// CommittedStageIDs set under one queue-lock acquisition, then
// deletes the staged record.
//
// Idempotent on stageID: a retry with an already-committed id
// short-circuits inside the queue lock and returns a definitive
// "duplicate commit suppressed" body. The bridge therefore can
// retry commit on any transport failure without risk of double-
// routing.
//
// ErrStagedPublishNotFound surfaces to the caller (HTTP handler
// translates it to 404 so the bridge can re-stage if needed).
// Disk-write errors return as the err result.
func (p *StorePublisher) CommitPublish(ctx context.Context, stageID string) (body string, isError bool, err error) {
	rec, rerr := p.deps.Store.ReadStagedPublish(stageID)
	if rerr != nil {
		return "", false, rerr
	}
	msg := rec.Message

	// WriteMessage is outside the queue lock: it has its own
	// atomicity (writeAtomic via temp+rename) and the filename is
	// deterministic in msg, so a retry writing the same content is
	// a benign no-op rather than a duplicate-file producer.
	path, werr := p.deps.Store.WriteMessage(msg)
	if werr != nil {
		return "", false, fmt.Errorf("write message: %w", werr)
	}
	msg.Path = path

	p.deps.Store.LockMessageQueue()
	defer p.deps.Store.UnlockMessageQueue()

	q, qerr := p.deps.Store.ReadMessageQueue()
	if qerr != nil {
		return "", false, fmt.Errorf("read message queue: %w", qerr)
	}
	if q.HasCommittedStageID(stageID) {
		// A prior commit for this stageID already wrote the queue.
		// The original first-pass body is not preserved; on the rare
		// "core died between commit and response" path the bridge
		// retries and lands here. Return a definitive confirmation
		// so the model sees an unambiguous success — beats the old
		// ambiguous "internal error after write" which is what we're
		// fixing.
		_ = p.deps.Store.DeleteStagedPublish(stageID)
		return fmt.Sprintf("%s already committed: %q → %s (saved to %s) [duplicate commit suppressed]",
			rec.Tool, msg.Title, msg.To, path), false, nil
	}

	routeWarn := ""
	if p.deps.ApplyRoute != nil {
		if aerr := p.deps.ApplyRoute(ctx, &q, msg); aerr != nil {
			// Persistence succeeded; routing is recoverable.
			routeWarn = fmt.Sprintf("\n[warning] route: %v", aerr)
		}
	}
	q.RecordCommittedStageID(stageID)
	if werr := p.deps.Store.WriteMessageQueue(q); werr != nil {
		return "", false, fmt.Errorf("write message queue: %w", werr)
	}

	// Best-effort cleanup of the staged record. Success of commit
	// does not depend on this — janitor will sweep stale records
	// either way.
	_ = p.deps.Store.DeleteStagedPublish(stageID)

	body = fmt.Sprintf("%s published: %q → %s (saved to %s)%s", rec.Tool, msg.Title, msg.To, path, routeWarn)
	// Soft lint on a notice that reads like an ask. A nudge appended
	// to a successful publish, not a rejection: the CEO moderates
	// every notice, and the point is to give the model the signal
	// while it can still file the assignment instead.
	if warn := message.NoticeAskLint(msg); warn != "" {
		body += "\n\n" + warn
	}
	return body, false, nil
}

// checkRecipientSlugs verifies that every agent slug a message names
// — the routing recipients of a notice, the target
// of a propose_role_update, and the slug + new_manager on each
// propose_reorg move — corresponds to an active agent in the org.
// Returns "" if every named slug exists; otherwise a single tool-level
// error string listing the missing slugs (collected so the model gets
// the full picture in one round-trip).
//
// This is a fast-fail layer on top of the delivery-time dead-letter
// (messaging.queueDeadLetter): the dead-letter still handles the rare
// race where a recipient is archived between stage and release, but
// agents publishing to a slug that never existed shouldn't have to
// wait a full release cycle to find out.
//
// "ceo" is always considered valid — it's the pseudo-agent root and
// is the only valid sentinel new_manager value on a reorg move that
// doesn't correspond to an on-disk agent dir.
func (p *StorePublisher) checkRecipientSlugs(msg store.Message) string {
	var missing []string
	check := func(slug, label string) {
		if slug == "" || slug == agent.CEOSlug {
			return
		}
		if _, err := p.deps.Store.GetAgent(slug); err != nil {
			missing = append(missing, fmt.Sprintf("%s %q", label, slug))
		}
	}
	if msg.Type == store.MsgNotice {
		// Every named recipient must exist. A notice can name several;
		// collecting all the misses means one round-trip to fix a
		// broadcast with two bad slugs instead of two.
		for _, to := range msg.To {
			check(to, "recipient")
		}
	}
	if msg.RoleUpdate != nil {
		check(msg.RoleUpdate.Slug, "role_update.slug")
	}
	if msg.Reorg != nil {
		for i, m := range msg.Reorg.Moves {
			check(m.Slug, fmt.Sprintf("moves[%d].slug", i))
			check(m.NewManager, fmt.Sprintf("moves[%d].new_manager", i))
		}
	}
	if msg.Offboard != nil {
		check(msg.Offboard.Slug, "offboard.slug")
	}
	if len(missing) == 0 {
		return ""
	}
	noun := "agent"
	if len(missing) > 1 {
		noun = "agents"
	}
	return fmt.Sprintf("unknown %s: %s — that slug is not an active agent in the org (archived agents cannot receive messages). Use read_org_chart to list active recipients and retry with a valid slug.", noun, strings.Join(missing, ", "))
}

// checkOffboardPreconditions refuses a propose_offboard whose target
// still has direct reports, so CoS learns at publish time rather than
// after the CEO has read the card and clicked Approve. Returns "" for
// every other message type.
//
// This is an early warning, NOT the gate. The binding check is
// agent.Runtime.ApplyOffboard, which runs at approve time — the org
// chart can change in between, and a proposal that was clean when
// written can orphan someone by the time it is acted on.
func (p *StorePublisher) checkOffboardPreconditions(msg store.Message) string {
	if msg.Offboard == nil {
		return ""
	}
	all, err := p.deps.Store.ListActiveAgents()
	if err != nil {
		// Can't tell; let the apply-time gate decide rather than
		// blocking a legitimate proposal on a transient read error.
		return ""
	}
	var reports []string
	for _, a := range all {
		if a.Slug != msg.Offboard.Slug && a.ReportsTo == msg.Offboard.Slug {
			reports = append(reports, a.Slug)
		}
	}
	if len(reports) == 0 {
		return ""
	}
	sort.Strings(reports)
	return fmt.Sprintf("cannot offboard %q: %s still report(s) to them. Offboarding would leave them unattached from the org chart, so move them first with propose_reorg — then re-propose this offboard.",
		msg.Offboard.Slug, strings.Join(reports, ", "))
}

// NewStorePublishDispatcher returns an in-process PublishDispatcher
// that runs StagePublish followed by CommitPublish back-to-back.
// Used by tests that exercise the dispatcher interface without
// involving the agent-pod bridge or HTTP layer.
func NewStorePublishDispatcher(deps StorePublishDeps) PublishDispatcher {
	return storePublishDispatcher{publisher: NewStorePublisher(deps)}
}

type storePublishDispatcher struct {
	publisher *StorePublisher
}

func (d storePublishDispatcher) DispatchPublish(ctx context.Context, toolName string, arguments json.RawMessage) (string, bool, error) {
	stageID, body, isErr, err := d.publisher.StagePublish(ctx, toolName, arguments)
	if err != nil {
		return "", false, err
	}
	if isErr {
		return body, true, nil
	}
	return d.publisher.CommitPublish(ctx, stageID)
}

// nowFunc and newStageIDFunc are overridable in tests; default to
// time.Now().UTC() and a crypto/rand-backed 16-hex-char generator.
// Kept package-level (not on StorePublisher) so the values are
// stable for tests spanning multiple constructors.
var (
	nowFunc        = func() time.Time { return time.Now().UTC() }
	newStageIDFunc = defaultNewStageID
)

func newStageID() string { return newStageIDFunc() }

func defaultNewStageID() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		// crypto/rand failures on Linux are essentially impossible;
		// degrade to a timestamp so the id is at least non-empty
		// and unique-per-call. Same fallback pattern as
		// newAgentpodTurnID.
		return fmt.Sprintf("s%016x", time.Now().UnixNano())
	}
	return hex.EncodeToString(b[:])
}

// bodyPathResolver returns a message.ParseContext.ResolveBodyPath
// callback that reads a referenced artifact from the agent's
// /files/ view. Matches the resolver used by the API path.
func bodyPathResolver(b *files.Backend) func(string) (string, error) {
	return func(modelPath string) (string, error) {
		body, isErr, err := files.Dispatch(b, files.ToolView, json.RawMessage(fmt.Sprintf(`{"path":%q}`, modelPath)))
		if err != nil {
			return "", err
		}
		if isErr {
			return "", fmt.Errorf("%s", body)
		}
		return body, nil
	}
}
