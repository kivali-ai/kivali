package agent

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/kivali-ai/kivali/internal/message"
	"github.com/kivali-ai/kivali/internal/roleicon"
	"github.com/kivali-ai/kivali/internal/store"
)

// ApplyHire provisions a new agent from a Hire proposal. The
// SINGLE creation path: the only caller is the CEO approval
// handler ([internal/web/ceo.go]) when a ceo_approval_request
// carrying a Hire is approved. The CEO asks the Chief of Staff for
// a role; CoS drafts it and publishes a propose_hire; the CEO's
// approve click lands here. There is no other path to creating an
// agent, and no CEO-side button that skips the proposal.
//
// The new agent is provisioned BEFORE the approval response is
// delivered, so the ack landing in the proposer's chat is itself
// the signal that the new hire is live.
//
// Returns (warning, error). A nil error with a non-empty warning
// means the agent was created but a non-fatal side effect (memory
// sync, agent-pod provision) failed and can be retried later. A
// non-nil error means the agent was NOT created and the caller
// should surface the failure (e.g. refuse to write an approval
// response).
func (r *Runtime) ApplyHire(h store.Hire) (warn string, err error) {
	// Format checks are done at message-parse time too (see
	// internal/message.ValidateSlug), but enforce them here as
	// defense in depth: if any future code path constructs a
	// store.Hire without going through the parser, this is the
	// chokepoint that still gets the check. Slug feeds straight
	// into a directory path (agents/<slug>) — a bad slug like
	// "alice/../bob" would silently land the agent dir in the
	// wrong place after path cleaning.
	if err := message.ValidateSlug(h.Slug); err != nil {
		return "", fmt.Errorf("hire: %w", err)
	}
	if err := message.ValidateSlug(h.ReportsTo); err != nil {
		return "", fmt.Errorf("hire: reports_to: %w", err)
	}
	if h.Role == "" {
		return "", fmt.Errorf("hire: role is required")
	}
	if h.Body == "" {
		return "", fmt.Errorf("hire: body (role content) is required")
	}
	// A hire without an icon applies as an agent drawn with initials.
	if h.Icon != "" {
		if err := roleicon.Check(h.Icon); err != nil {
			return "", fmt.Errorf("hire: icon: %w", err)
		}
	}
	if _, gerr := r.Store.GetAgent(h.Slug); gerr == nil {
		return "", fmt.Errorf("hire: agent %q already exists", h.Slug)
	}
	a := store.Agent{
		Slug:      h.Slug,
		Role:      h.Role,
		Icon:      h.Icon,
		ReportsTo: h.ReportsTo,
		Model:     r.Defaults.AgentModel,
	}
	if cerr := r.Store.CreateAgent(a, h.Body); cerr != nil {
		return "", fmt.Errorf("hire %s: create: %w", h.Slug, cerr)
	}
	if h.InitialAgentMemory != "" {
		if werr := r.Store.WriteAgentMemory(h.Slug, h.InitialAgentMemory); werr != nil {
			return fmt.Sprintf("hire %s: write initial agent memory: %v", h.Slug, werr), nil
		}
	}
	if serr := r.Store.SyncAgentFilesystem(h.Slug); serr != nil {
		return fmt.Sprintf("memory sync %s: %v", h.Slug, serr), nil
	}
	if r.AgentPod != nil {
		if perr := r.AgentPod.Provision(context.Background(), h.Slug); perr != nil {
			// Non-fatal: agent exists in the store, the pod can be
			// re-provisioned manually. Don't fail the hire — the
			// agent runtime will become available once the pod
			// schedules and dials core.
			return fmt.Sprintf("agentpod provision %s: %v", h.Slug, perr), nil
		}
	}
	return "", nil
}

// ApplyRoleUpdate overwrites an existing agent's role.md with body.
// Called by the CEO approval handler when a ceo_approval_request
// carrying a RoleUpdate is approved — atomically replacing the role
// file BEFORE writing the approval response, so the ack landing in
// the proposer's chat is itself the signal that the new role is live.
//
// Refuses to write on missing/unknown slug or empty body. The
// underlying Store.WriteRole is rename-based (atomic at the
// filesystem layer); a crash mid-write leaves the prior file intact.
func (r *Runtime) ApplyRoleUpdate(u store.RoleUpdate) error {
	if u.Slug == "" {
		return fmt.Errorf("role_update: slug is required")
	}
	if strings.TrimSpace(u.Body) == "" {
		return fmt.Errorf("role_update %s: body is required", u.Slug)
	}
	if _, err := r.Store.GetAgent(u.Slug); err != nil {
		return fmt.Errorf("role_update %s: %w", u.Slug, err)
	}
	if err := r.Store.WriteRole(u.Slug, u.Body); err != nil {
		return fmt.Errorf("role_update %s: write role.md: %w", u.Slug, err)
	}
	return nil
}

// ApplyOffboard archives the agent named by an Offboard proposal. The
// SINGLE retirement path: the only caller is the CEO approval handler
// ([internal/web/ceo.go]) when a ceo_approval_request carrying an
// Offboard is approved. There is no CEO-side button: letting someone
// go is a decision with a proposer, a rationale, and an approval on
// the record.
//
// Archiving preserves everything: chat history, role, memory, and
// messages move under agents/_archived/<slug>, and the per-agent PVC
// is deliberately retained so the departing agent's scratch
// filesystem survives for forensics.
//
// The caller is responsible for two things this function cannot do,
// both of which live in the web layer:
//
//   - Draining in-flight state BEFORE calling (chat hub, agent-pod
//     turn state, buffered deliveries) — otherwise a turn still in
//     flight keeps writing into a chat.jsonl that the archive rename
//     has moved out from under it.
//   - Destroying the pod AFTER (Provisioner here has no Destroy;
//     web's AgentPodLifecycle does). This is the one asymmetry with
//     ApplyHire, which provisions inline.
func (r *Runtime) ApplyOffboard(o store.Offboard) error {
	// Defense in depth. The parser enforces all three of these, but
	// this is the chokepoint any future caller passes through, and
	// the slug feeds straight into a directory path.
	if err := message.ValidateSlug(o.Slug); err != nil {
		return fmt.Errorf("offboard: %w", err)
	}
	if o.Slug == CEOSlug {
		return fmt.Errorf("offboard: cannot offboard %q (the owner is the human at the root of the org)", CEOSlug)
	}
	if o.Slug == message.ChiefOfStaff {
		return fmt.Errorf("offboard: cannot offboard %q (it is the only role that can propose org changes, including this one)", message.ChiefOfStaff)
	}
	if _, err := r.Store.GetAgent(o.Slug); err != nil {
		return fmt.Errorf("offboard %s: %w", o.Slug, err)
	}
	// Refuse to orphan anyone. Archiving a manager leaves its reports
	// pointing at a slug that is no longer active, which surfaces in
	// the org chart as "Unattached (data issue)" and silently drops
	// them out of their manager's context. Where those reports should
	// land is a real decision, so it gets its own approval: CoS lands
	// a propose_reorg first, then re-proposes the offboard.
	//
	// Checked here rather than at parse time on purpose — the org
	// chart can change between proposing and approving, so the
	// binding check has to be the one that runs at apply.
	reports, err := r.directReports(o.Slug)
	if err != nil {
		return fmt.Errorf("offboard %s: %w", o.Slug, err)
	}
	if len(reports) > 0 {
		return fmt.Errorf("offboard %s: still has %d direct report(s): %s — move them with propose_reorg first, then re-propose this offboard", o.Slug, len(reports), strings.Join(reports, ", "))
	}
	if err := r.Store.ArchiveAgent(o.Slug); err != nil {
		return fmt.Errorf("offboard %s: archive: %w", o.Slug, err)
	}
	return nil
}

// directReports returns the slugs of every active agent whose
// reports_to names slug, sorted so the refusal message is stable.
func (r *Runtime) directReports(slug string) ([]string, error) {
	all, err := r.Store.ListActiveAgents()
	if err != nil {
		return nil, fmt.Errorf("list agents: %w", err)
	}
	var out []string
	for _, a := range all {
		if a.Slug != slug && a.ReportsTo == slug {
			out = append(out, a.Slug)
		}
	}
	sort.Strings(out)
	return out, nil
}

// ApplyReorg applies a Reorg proposal best-effort: each move is tried
// independently; failures (unknown slug, missing manager, cycle) are
// captured in the returned slices so the caller can both surface the
// outcome to the proposer and keep going. Mutates and returns r.Reorg
// so the caller can drop it onto the approval response with Moves
// stripped and Applied/Failed populated.
//
// Idempotency: a move whose new_manager already equals the current
// reports_to applies as a no-op (counted as applied, not failed) so a
// proposal that races a prior application doesn't surface as an error
// the operator has to triage.
func (r *Runtime) ApplyReorg(in store.Reorg) (store.Reorg, error) {
	out := store.Reorg{}
	if len(in.Moves) == 0 {
		return out, fmt.Errorf("reorg: at least one move required")
	}
	for _, m := range in.Moves {
		if err := r.applyReorgMove(m); err != nil {
			out.Failed = append(out.Failed, store.ReorgFailure{Move: m, Reason: err.Error()})
			continue
		}
		out.Applied = append(out.Applied, m)
	}
	return out, nil
}

// applyReorgMove validates one move and writes ReportsTo. Validation
// errors and disk errors both surface here as a single error string;
// the apply caller treats them uniformly as failed moves.
func (r *Runtime) applyReorgMove(m store.ReorgMove) error {
	if err := message.ValidateSlug(m.Slug); err != nil {
		return fmt.Errorf("slug: %w", err)
	}
	if err := message.ValidateSlug(m.NewManager); err != nil {
		return fmt.Errorf("new_manager: %w", err)
	}
	if m.Slug == CEOSlug {
		return fmt.Errorf("cannot move %q (root of org)", CEOSlug)
	}
	if m.Slug == m.NewManager {
		return fmt.Errorf("cannot reparent %q under itself", m.Slug)
	}
	target, err := r.Store.GetAgent(m.Slug)
	if err != nil {
		return fmt.Errorf("target %q: %w", m.Slug, err)
	}
	if m.NewManager != CEOSlug {
		if _, err := r.Store.GetAgent(m.NewManager); err != nil {
			return fmt.Errorf("new manager %q: %w", m.NewManager, err)
		}
	}
	if target.ReportsTo == m.NewManager {
		// Already reports to the proposed manager — no-op success.
		return nil
	}
	if err := r.checkReorgCycle(m.Slug, m.NewManager); err != nil {
		return err
	}
	if err := r.Store.SetAgentReportsTo(m.Slug, m.NewManager); err != nil {
		return fmt.Errorf("write agent.yaml: %w", err)
	}
	return nil
}

// checkReorgCycle walks up from newManager via ReportsTo and refuses
// the move if it ever hits target. Capped at len(active agents) + 1
// hops so a pre-existing cycle in stored data can't loop forever.
func (r *Runtime) checkReorgCycle(target, newManager string) error {
	cursor := newManager
	all, err := r.Store.ListActiveAgents()
	if err != nil {
		return fmt.Errorf("list agents: %w", err)
	}
	maxHops := len(all) + 1
	for hop := 0; hop < maxHops; hop++ {
		if cursor == "" || cursor == CEOSlug {
			return nil
		}
		if cursor == target {
			return fmt.Errorf("would create a cycle: %q reports up through %q", newManager, target)
		}
		a, err := r.Store.GetAgent(cursor)
		if err != nil {
			// Manager-chain points at a missing slug. Treat as
			// dangling; not our job to fix here. Allowing the move
			// is safer than rejecting on stale state.
			return nil
		}
		cursor = a.ReportsTo
	}
	return fmt.Errorf("cycle detected walking up from %q (chain too long)", newManager)
}

// ApplyHandbookUpdate overwrites the org-wide handbook with
// body. Called by the CEO approval handler when a ceo_approval_request
// carrying a HandbookUpdate is approved — atomically replacing
// the handbook file BEFORE writing the approval response, so the
// ack landing in the proposer's chat is itself the signal that the
// new handbook is live. Mirrors ApplyRoleUpdate.
//
// Refuses to write on empty body. Store.WriteHandbook is
// rename-based (atomic at the filesystem layer); a crash mid-write
// leaves the prior file intact.
func (r *Runtime) ApplyHandbookUpdate(u store.HandbookUpdate) error {
	if strings.TrimSpace(u.Body) == "" {
		return fmt.Errorf("handbook_update: body is required")
	}
	if err := r.Store.WriteHandbook(u.Body); err != nil {
		return fmt.Errorf("handbook_update: write: %w", err)
	}
	return nil
}
