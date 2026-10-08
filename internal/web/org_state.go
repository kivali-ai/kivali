package web

import (
	"cmp"
	"encoding/json"
	"sort"
	"time"

	"github.com/kivali-ai/kivali/internal/agent"
	"github.com/kivali-ai/kivali/internal/store"
	"github.com/kivali-ai/kivali/internal/web/apitypes"
)

// The /org/stream wire shape lives in apitypes so the SSE stream and
// GET /api/v1/snapshot share ONE definition, and tygo can derive the
// web app's TypeScript from it. The aliases keep this package's names.
//
// Per-agent lifecycle (the web app reads SnapshotAgent.State, which
// agentState derives from these flags; only waiting_tasks is on the
// wire beside it):
//
//   - Working: a turn is streaming, there is a hub to attach to.
//   - WaitingTasks: background subagent jobs this agent still owns. A
//     SEPARATE channel from Working on purpose: a client reconciles
//     against a running state by opening the agent SSE, and an agent
//     parked on background work has no stream to open — folding the
//     two would loop the client through open/204/teardown. agentState
//     ORs them; every "can I attach to a turn" gate reads Working alone.
//   - Quarantined: stopped after repeated failures — crashed and
//     respawned chatHubQuarantineThreshold times in a row with nothing
//     productive between (store.ConsecutiveRuntimeDisruptions); the
//     spawn gate refuses to start it until the CEO re-engages it.
//   - Interrupted: the newest meaningful entry is a user-interruption
//     with no CEO redirect after it (store.SpawnHoldForCEO).
//   - NeedsHelp: the last turn stopped on an error and nothing has been
//     received since (store.SpawnHoldOnError); the dot goes red.
//   - Disconnected: no agent-pod runtime is subscribed to this slug's
//     events stream (fresh hire, pod crash, dropped connection). Only
//     set when AgentpodHub is wired.
//
// Stable JSON: orgHub dedupes on byte equality, so every list marshals
// in a deterministic order — agents in tree order, paths sorted.
type (
	orgSnapshot     = apitypes.OrgSnapshot
	agentLiveness   = apitypes.SnapshotAgent
	inboxLiveness   = apitypes.SnapshotInbox
	releaseLiveness = apitypes.SnapshotRelease
)

// buildOrgSnapshot reads current state and returns a marshalled
// snapshot. Called from every NotifyOrgState call site, so it stays
// cheap: one workingAgents() walk, one ListActiveAgents read, one
// message-queue read, one chat-history read per active agent, one
// CountMessages call. The assignment set and the usage window are
// cached until their next write (assignmentSetCached, spendReadouts).
//
// Notify cadence is event-driven (chat hubs open/close, runtime agents
// start/stop, MCP enqueues), not periodic — the steady state is "no
// calls at all."
func (s *Server) buildOrgSnapshot() []byte {
	body, _ := json.Marshal(s.orgSnapshotValue())
	return body
}

// orgSnapshotValue is buildOrgSnapshot before marshalling.
func (s *Server) orgSnapshotValue() orgSnapshot {
	now := s.clk().Now()
	agents := s.agentLivenessList()

	// Flat list of every queued message path, de-duplicated: Home
	// refetches when this list changes, and a notice queued for three
	// agents is one queue row, so it must count once here too.
	q, _ := s.Store.ReadMessageQueue()
	pendingPaths := pendingPathsOf(q)
	if pendingPaths == nil {
		pendingPaths = []string{}
	}

	messagesTotal := 0
	if n, err := s.Store.CountMessages(); err == nil {
		messagesTotal = n
	}

	ts := s.queueStatus()

	// CEO Needs paths: Home refetches whenever a new ceo_inbox entry
	// lands (or one resolves), which changes this list. The same
	// cached inbox read queueStatus counted from (ceoNeeds).
	_, ceoPaths := s.ceoNeeds()

	// Read the version BEFORE the set: a write landing in between
	// leaves the cached set tagged one version behind, never ahead, so
	// the next snapshot rereads it.
	assignmentsVersion := s.assignmentsVersion.Load()
	names := make(map[string]string, len(agents))
	for _, a := range agents {
		names[a.Slug] = a.Name
	}
	person := func(slug string) apitypes.PersonRef { return personNamed(names, slug) }
	goals, blocked, closedWeek := goalsAndAssignmentReadouts(s.assignmentSetCached(assignmentsVersion), now, person)

	working := 0
	for _, a := range agents {
		if a.State == apitypes.AgentStateRunning || a.WaitingTasks > 0 {
			working++
		}
	}
	spendToday, spend7d := s.spendReadouts(now)

	return orgSnapshot{
		Agents: agents,
		Inbox: inboxLiveness{
			Unactioned:   ts.InboxUnactioned,
			PendingPaths: pendingPaths,
			CEOPaths:     ceoPaths,
			AutoRelease:  apitypes.AutoRelease(autoReleaseKey(s.Store.ReadAutoRelease())),
		},
		Release: releaseLiveness{
			Running:      ts.Running,
			ActiveAgents: ts.ActiveAgents,
			PendingDocs:  ts.PendingDocs,
		},
		MessagesTotal:      messagesTotal,
		AssignmentsVersion: assignmentsVersion,
		Goals:              goals,
		Readouts: apitypes.Readouts{
			Working:    working,
			Blocked:    blocked,
			ClosedWeek: closedWeek,
			SpendToday: spendToday,
			Spend7d:    spend7d,
		},
	}
}

// agentLivenessList is every active non-CEO agent's liveness, in tree
// order. Shared by the snapshot and GET /api/v1/agents so the two
// cannot disagree about an agent's state or context fill.
func (s *Server) agentLivenessList() []agentLiveness {
	working := s.workingAgents()
	actives, _ := s.Store.ListActiveAgents()
	out := make([]agentLiveness, 0, len(actives))
	for _, n := range agentTreeOrder(actives) {
		out = append(out, s.agentLivenessFor(n, working))
	}
	return out
}

// lifecycle is the per-agent liveness the server derives
// SnapshotAgent.State from; see the flag list at the top of this file.
type lifecycle struct {
	Working      bool
	WaitingTasks int
	Quarantined  bool
	Interrupted  bool
	NeedsHelp    bool
	Disconnected bool
}

// agentLivenessFor builds one agent's record. The context fill and
// every gate-derived flag come from its chat history, read only when it
// changed since the last snapshot (chatFactsFor).
func (s *Server) agentLivenessFor(n agentTreeNode, working map[string]bool) agentLiveness {
	a := n.Agent
	facts := s.chatFactsFor(a)
	lc := lifecycle{
		Working:      working[a.Slug],
		WaitingTasks: s.SubagentService.OutstandingSubagents(a.Slug),
		Quarantined:  facts.disruptions >= chatHubQuarantineThreshold,
		Interrupted:  facts.verdict == store.SpawnHoldForCEO,
		NeedsHelp:    facts.verdict == store.SpawnHoldOnError,
		// Pod presence: "no subscriber" means the agent pod hasn't
		// dialed the events SSE yet (fresh hire, restart in flight,
		// crash). A pod provisioned moments ago is starting, not
		// disconnected (podStartGrace). Only meaningful in production
		// where AgentpodHub is wired; tests without it leave
		// Disconnected false rather than false-positive every agent.
		Disconnected: s.AgentpodHub != nil && s.AgentpodHub.SubscriberCount(a.Slug) == 0 &&
			!s.AgentpodHub.Starting(a.Slug, s.clk().Now()),
	}
	return agentLiveness{
		Slug:         a.Slug,
		Name:         agentDisplayName(a),
		RoleTitle:    a.Role,
		Icon:         a.Icon,
		ReportsTo:    cmp.Or(a.ReportsTo, agent.CEOSlug),
		Depth:        n.Depth,
		State:        agentState(lc),
		ContextPct:   facts.fillPct,
		WaitingTasks: lc.WaitingTasks,
	}
}

// agentDisplayName is the label the sidebar tree shows for an agent:
// its role title, or its slug when it has none.
func agentDisplayName(a store.Agent) string {
	return cmp.Or(a.Role, a.Slug)
}

// agentState folds the lifecycle flags into the one state the new app
// shows. Precedence, most urgent first: a quarantined agent (stopped
// after repeated failures) will not wake at all; one whose last turn
// stopped on an error needs the CEO's help; a running turn or
// background work is real activity even while the pod link is down
// (background tasks run in core); an interrupted agent is waiting on
// the CEO's redirect; a missing pod is only worth saying when nothing
// else is.
func agentState(l lifecycle) apitypes.AgentState {
	switch {
	case l.Quarantined:
		return apitypes.AgentStateQuarantined
	case l.NeedsHelp:
		return apitypes.AgentStateNeedsHelp
	case l.Working:
		return apitypes.AgentStateRunning
	case l.WaitingTasks > 0, l.Interrupted:
		return apitypes.AgentStateWaiting
	case l.Disconnected:
		return apitypes.AgentStateDisconnected
	}
	return apitypes.AgentStateIdle
}

// agentTreeNode is one agent at its depth in the reporting tree.
type agentTreeNode struct {
	Agent store.Agent
	Depth int
}

// agentTreeOrder walks the reporting lines depth-first from the CEO,
// siblings by slug, and returns every non-CEO agent with its depth
// (the CEO's direct reports are depth 1) — the order the sidebar tree
// draws. An agent whose manager is not in the list (archived, or a
// reporting cycle) is not dropped: it and its reports are appended as
// further roots at depth 1, by slug, so every agent appears once.
func agentTreeOrder(actives []store.Agent) []agentTreeNode {
	bySlug := map[string]store.Agent{}
	children := map[string][]string{}
	for _, a := range actives {
		if a.Slug == agent.CEOSlug {
			continue
		}
		bySlug[a.Slug] = a
		parent := cmp.Or(a.ReportsTo, agent.CEOSlug)
		children[parent] = append(children[parent], a.Slug)
	}
	for _, kids := range children {
		sort.Strings(kids)
	}
	out := make([]agentTreeNode, 0, len(bySlug))
	seen := map[string]bool{}
	var walk func(slug string, depth int)
	walk = func(slug string, depth int) {
		if seen[slug] {
			return
		}
		seen[slug] = true
		out = append(out, agentTreeNode{Agent: bySlug[slug], Depth: depth})
		for _, k := range children[slug] {
			walk(k, depth+1)
		}
	}
	for _, k := range children[agent.CEOSlug] {
		walk(k, 1)
	}
	if len(out) < len(bySlug) {
		rest := make([]string, 0, len(bySlug)-len(out))
		for slug := range bySlug {
			if !seen[slug] {
				rest = append(rest, slug)
			}
		}
		sort.Strings(rest)
		for _, slug := range rest {
			walk(slug, 1)
		}
	}
	return out
}

// pendingPathsOf flattens a MessageQueue into the sorted, de-duplicated
// message-path list the snapshot publishes. The single definition of
// that set: buildOrgSnapshot and queueStatus both count the result of
// this function, so Home's queue and the snapshot's pending count
// cannot disagree.
//
// De-duplicated because a notice queued to three agents is one row
// for the CEO to decide on, and one document to count.
func pendingPathsOf(q store.MessageQueue) []string {
	seen := map[string]bool{}
	var paths []string
	for _, rt := range q.Agents {
		for _, p := range rt.Inbox {
			if seen[p] {
				continue
			}
			seen[p] = true
			paths = append(paths, p)
		}
	}
	sort.Strings(paths)
	return paths
}

// contextFillForHistory returns the context-fill bucket and percentage
// for an agent from its (pre-read) chat history against the model's
// window. Empty bucket and 0% when there is no history (clients treat
// absent as "no fill to show") or the window is unknown.
//
// Takes the history rather than reading it because its caller
// (buildOrgSnapshot) already holds it for the gate-derived flags; one
// read serves the fill and the flags. The math is computeChatFillStats,
// the same the chat API and the chat "done" event use, so the
// snapshot and the chat agree: the last turn's real window occupancy
// when there is one, else the
// chars/4 transcript estimate. The only extra cost over the history
// read is one small sidecar file per agent, so nothing is cached.
func (s *Server) contextFillForHistory(a store.Agent, hist []store.ChatMessage) (bucket string, pct int) {
	if len(hist) == 0 {
		return "", 0
	}
	cw, _ := s.Store.ReadContextWindow(a.Slug)
	stats := s.computeChatFillStats(a.Model, s.AgentModel, hist, cw.ContextTokens)
	if stats.ContextLimit <= 0 {
		return "", 0
	}
	return stats.FillBucket, stats.FillPct
}

// orgNotifyDebounce is the trailing-edge debounce window for snapshot
// publishes. Multiple NotifyOrgState calls inside this window coalesce
// into one snapshot rebuild + publish — the wire-dedup downstream
// catches the redundant bytes anyway, this saves the CPU of N×walk-
// every-agent-history.
//
// Tuned for the Release-all worst case: an N-agent kick fires N×2
// markRunning notifies (one per agent start, one per finish) plus
// hub start/stop notifies, all within ~tens of ms. 30ms collects the
// burst without making the user-visible UI feel laggy.
const orgNotifyDebounce = 30 * time.Millisecond

// NotifyOrgState wakes the snapshot-publisher goroutine. Non-blocking;
// safe from any goroutine. Multiple calls within orgNotifyDebounce
// coalesce — the goroutine sleeps that long before rebuilding +
// publishing, so a burst becomes a single snapshot push.
func (s *Server) NotifyOrgState() {
	if s.orgHub == nil || s.notifyCh == nil {
		return
	}
	select {
	case s.notifyCh <- struct{}{}:
	default:
		// A notify is already pending; the in-flight goroutine will
		// pick up our state change in its rebuild. No-op send.
	}
}

// runOrgNotifier is the snapshot-publisher loop. Started once in
// NewServer; lives for the process lifetime. Drains notifyCh, sleeps
// for the debounce window (during which more notifies coalesce into
// the buffered channel slot), then rebuilds the snapshot and pushes
// it to /org/stream subscribers via orgHub.publish (which itself
// dedupes on byte equality).
//
// Two-stage de-duplication:
//   - Producer side (here): collapse a burst into one snapshot rebuild.
//     Saves CPU.
//   - Consumer side (orgHub.publish): collapse byte-equal snapshots
//     into a no-op. Saves wire bytes + subscriber wakes.
//
// Either alone would be insufficient: without the producer-side
// debounce we'd rebuild N times during a Release-all even if the
// bytes happen to land identical; without consumer-side dedup an
// "actually nothing changed" notify (e.g. defensive belt-and-suspenders
// from a caller) would still wake every subscriber.
func (s *Server) runOrgNotifier() {
	for range s.notifyCh {
		// Trailing-edge debounce: sleep, THEN rebuild. Anything
		// that lands during the sleep window is coalesced via the
		// buffered-1 channel — it'll either be picked up in this
		// rebuild (it landed before we built) or trigger the next
		// loop iteration (it landed after the channel drained).
		time.Sleep(orgNotifyDebounce)
		s.orgHub.publish(s.buildOrgSnapshot())
	}
}
