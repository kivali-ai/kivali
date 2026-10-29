package web

import (
	"context"
	"log"

	"github.com/kivali-ai/kivali/internal/agent"
	"github.com/kivali-ai/kivali/internal/agentpod"
	"github.com/kivali-ai/kivali/internal/store"
)

// Restarting an agent's pod, and the turns a pod takes with it.
//
// Kivali recreates an agent's pod when its spec changes: at boot for a
// new image, and when the agent's software changes. A turn running on
// the old pod dies with it. So:
//
//   - A software change waits for the agent to be idle (no turn running
//     or starting, no background task outstanding) and holds new turns
//     while the pod is recreated (ReprovisionAgentPod). Work that
//     arrives meanwhile stays owed in chat.jsonl, and the new pod's
//     subscription starts it.
//   - At boot, turns are held for every agent until the startup pass
//     has provisioned its pod (HoldTurnsForStartup), so the subscribe
//     wake of a pod the pass is about to recreate starts nothing on it.
//   - A turn whose runtime went away anyway is ended as a runtime
//     disruption, and the agent resumes from it on its next pod: when
//     the runtime reports it is stopping (agentpod.FailedReasonRuntimeStopped),
//     when a different runtime subscribes for the agent (adoptRuntime),
//     or when no runtime of its own is back within the disconnect
//     grace (handleAgentpodDisconnect). A pod Kivali recreated makes
//     the row Planned, which the crash-loop breaker does not count.

// Core-only failure reasons for a turn whose runtime went away without
// finishing it; finishFailedChatTurn writes a runtime-disruption row
// for each.
const (
	// failedReasonRuntimeRestarted: Kivali recreated the agent's pod
	// while the turn ran. The row is Planned.
	failedReasonRuntimeRestarted = "runtime-restarted"
	// failedReasonRuntimeLost: the runtime went away for a reason Kivali
	// did not cause (a crash, an eviction).
	failedReasonRuntimeLost = "runtime-lost"
)

// plannedDisruptionText is the row a turn interrupted by Kivali's own
// pod restart leaves for the agent to read.
const plannedDisruptionText = "Your runtime restarted mid-task to apply a change to it, such as your shell's software. Resume your prior work as Kivali OS says under When you're interrupted."

// lostRuntimeText is the row a turn whose runtime went away leaves for
// the agent to read.
const lostRuntimeText = "Your runtime stopped mid-task, before the turn finished. Resume your prior work as Kivali OS says under When you're interrupted."

// agentIdleLocked reports whether slug has no turn running or starting
// and no subagent turn in flight. streamMu is held.
func (s *Server) agentIdleLocked(slug string) bool {
	if h := s.chatHubs[slug]; h != nil && !h.isCompleted() {
		return false
	}
	for _, st := range s.agentpodSubagentTurns {
		if st != nil && st.slug == slug {
			return false
		}
	}
	return true
}

// turnsHeldLocked reports whether no new turn may start for slug.
// streamMu is held.
func (s *Server) turnsHeldLocked(slug string) bool {
	return s.turnHolds[slug] > 0
}

// holdTurnsLocked adds a hold on slug's turns. streamMu is held.
func (s *Server) holdTurnsLocked(slug string) {
	if s.turnHolds == nil {
		s.turnHolds = map[string]int{}
	}
	s.turnHolds[slug]++
}

// releaseTurns drops one hold on slug's turns. When it was the last,
// the turn a spawn was refused meanwhile starts now if the agent's pod
// is subscribed; one that is not starts it when it subscribes.
func (s *Server) releaseTurns(slug string) {
	s.streamMu.Lock()
	if s.turnHolds[slug] > 0 {
		s.turnHolds[slug]--
	}
	held := s.turnHolds[slug] > 0
	if !held {
		delete(s.turnHolds, slug)
	}
	s.streamMu.Unlock()
	if !held && s.AgentpodHub != nil && s.AgentpodHub.SubscriberCount(slug) > 0 {
		s.spawnChatLoopIfIdle(slug, "release")
	}
}

// holdIfIdle holds slug's turns and reports true when the agent is
// idle: no turn running or starting, and no background task
// outstanding. Otherwise it holds nothing and reports false.
func (s *Server) holdIfIdle(slug string) bool {
	// A background task is started only by a running turn, so with
	// none outstanding before the turn check, none can start after it.
	if s.SubagentService.OutstandingSubagents(slug) > 0 {
		return false
	}
	s.streamMu.Lock()
	if !s.agentIdleLocked(slug) {
		s.streamMu.Unlock()
		return false
	}
	s.holdTurnsLocked(slug)
	s.streamMu.Unlock()
	// A turn that ended between the two checks may have dispatched
	// one.
	if s.SubagentService.OutstandingSubagents(slug) > 0 {
		s.releaseTurns(slug)
		return false
	}
	return true
}

// ReprovisionAgentPod reconciles slug's pod with its current spec (a
// changed software list, say) without cutting a turn short. When the
// agent is idle it provisions now, with new turns held until the pod is
// recreated, and returns the provision's error. When the agent is
// working it returns deferred: the provision runs once the agent's turn
// and background tasks have ended (runOwedRestart), and turns are held
// from then until the pod is recreated. A deferred provision that fails
// is reported to onFailed (nil for none), on its own goroutine.
func (s *Server) ReprovisionAgentPod(ctx context.Context, slug string, onFailed func(error)) (deferred bool, err error) {
	if s.AgentPod == nil || slug == agent.CEOSlug {
		return false, nil
	}
	if s.holdIfIdle(slug) {
		defer s.releaseTurns(slug)
		return false, s.ProvisionAgentPod(ctx, slug)
	}
	s.streamMu.Lock()
	if s.restartsOwed == nil {
		s.restartsOwed = map[string]*owedRestart{}
	}
	owed := s.restartsOwed[slug]
	if owed == nil {
		owed = &owedRestart{}
		s.restartsOwed[slug] = owed
	}
	if onFailed != nil {
		owed.onFailed = append(owed.onFailed, onFailed)
	}
	s.streamMu.Unlock()
	// The agent may have gone idle since the check.
	s.runOwedRestart(slug)
	return true, nil
}

// owedRestart is a reprovision waiting for its agent to be idle, with
// whoever is to hear if it fails.
type owedRestart struct {
	onFailed []func(error)
}

// runOwedRestart starts slug's owed reprovision if the agent is idle
// now, holding its turns until the pod is recreated. Called wherever an
// agent can become idle: a turn's end, a background task's end, and
// every spawn, so an agent that ends one turn and would start the next
// at once restarts in between.
func (s *Server) runOwedRestart(slug string) {
	s.streamMu.Lock()
	owed := s.restartsOwed[slug] != nil
	s.streamMu.Unlock()
	if !owed || !s.holdIfIdle(slug) {
		return
	}
	s.streamMu.Lock()
	restart := s.restartsOwed[slug]
	delete(s.restartsOwed, slug)
	s.streamMu.Unlock()
	if restart == nil {
		// Another caller took it between the two reads.
		s.releaseTurns(slug)
		return
	}
	go func() {
		// A failure is reported while the turns are still held, so what
		// it delivers is in chat.jsonl before the next turn starts.
		if err := s.ProvisionAgentPod(context.Background(), slug); err != nil {
			log.Printf("agentpod: reprovision %s: %v", slug, err)
			for _, f := range restart.onFailed {
				f(err)
			}
		}
		s.releaseTurns(slug)
		if s.owedRestartDone != nil {
			s.owedRestartDone(slug)
		}
	}()
}

// runOwedRestarts runs runOwedRestart for every agent that owes one.
func (s *Server) runOwedRestarts() {
	s.streamMu.Lock()
	slugs := make([]string, 0, len(s.restartsOwed))
	for slug := range s.restartsOwed {
		slugs = append(slugs, slug)
	}
	s.streamMu.Unlock()
	for _, slug := range slugs {
		s.runOwedRestart(slug)
	}
}

// markRestartPlanned records that Kivali is about to delete or create
// slug's pod: a turn its runtime loses from now until a new runtime
// subscribes ends as a Planned disruption.
func (s *Server) markRestartPlanned(slug string) {
	s.streamMu.Lock()
	if s.plannedRestarts == nil {
		s.plannedRestarts = map[string]bool{}
	}
	s.plannedRestarts[slug] = true
	s.streamMu.Unlock()
}

// restartPlanned reports markRestartPlanned's record for slug.
func (s *Server) restartPlanned(slug string) bool {
	s.streamMu.Lock()
	defer s.streamMu.Unlock()
	return s.plannedRestarts[slug]
}

// HoldTurnsForStartup holds every active agent's turns until
// BulkProvisionAgentPods has provisioned its pod (or gives up). Called
// before the agent-pod socket opens: a pod still running from before
// the restart dials in at once, and its subscribe wake must not start a
// turn on a pod the startup pass is about to recreate for a new spec.
func (s *Server) HoldTurnsForStartup() {
	slugs, err := s.activeAgentPodSlugs(nil)
	if err != nil {
		log.Printf("agentpod: list agents: %v", err)
		return
	}
	s.streamMu.Lock()
	defer s.streamMu.Unlock()
	if s.startupHolds == nil {
		s.startupHolds = map[string]bool{}
	}
	for _, slug := range slugs {
		if !s.startupHolds[slug] {
			s.startupHolds[slug] = true
			s.holdTurnsLocked(slug)
		}
	}
}

// startupHeld reports whether slug's turns are still held for the
// startup pass (HoldTurnsForStartup): its pod may be recreated yet.
func (s *Server) startupHeld(slug string) bool {
	s.streamMu.Lock()
	defer s.streamMu.Unlock()
	return s.startupHolds[slug]
}

// releaseStartupHold drops slug's startup hold, if it has one.
func (s *Server) releaseStartupHold(slug string) {
	s.streamMu.Lock()
	held := s.startupHolds[slug]
	delete(s.startupHolds, slug)
	s.streamMu.Unlock()
	if held {
		s.releaseTurns(slug)
	}
}

// releaseStartupHolds drops every startup hold left.
func (s *Server) releaseStartupHolds() {
	s.streamMu.Lock()
	slugs := make([]string, 0, len(s.startupHolds))
	for slug := range s.startupHolds {
		slugs = append(slugs, slug)
	}
	s.streamMu.Unlock()
	for _, slug := range slugs {
		s.releaseStartupHold(slug)
	}
}

// adoptRuntime records that runtimeID has subscribed for slug, and ends
// the turns a different runtime was running: one process per pod, and
// one pod per agent, so a new runtime means the old one is gone and its
// turns with it. The parent turn ends as a runtime disruption (Planned
// when Kivali recreated the pod), which the subscribe wake that follows
// resumes on this runtime; a subagent turn fails, which its parent reads
// as the task's result.
//
// A turn published to a runtime that sent no id (one older than the
// header) ends too once no such runtime is subscribed. A subscriber
// that sends no id is never assumed to replace another. Any
// subscription other than a reconnect of the same runtime clears the
// planned-restart record: the restart it was for is over.
func (s *Server) adoptRuntime(slug, runtimeID string) {
	idlessGone := s.AgentpodHub == nil || !s.AgentpodHub.HasRuntime(slug, "")
	replaced := func(st *agentpodTurnState) bool {
		if runtimeID == "" {
			return false
		}
		if st.runtime == "" {
			return idlessGone
		}
		return st.runtime != runtimeID
	}
	s.streamMu.Lock()
	if s.runtimes == nil {
		s.runtimes = map[string]string{}
	}
	prev, known := s.runtimes[slug]
	s.runtimes[slug] = runtimeID
	planned := s.plannedRestarts[slug]
	if !known || runtimeID == "" || prev != runtimeID {
		delete(s.plannedRestarts, slug)
	}
	var parent *agentpodTurnState
	if st := s.agentpodTurns[slug]; st != nil && replaced(st) {
		parent = st
	}
	var subagents []*agentpodTurnState
	for _, st := range s.agentpodSubagentTurns {
		if st != nil && st.slug == slug && replaced(st) {
			subagents = append(subagents, st)
		}
	}
	s.streamMu.Unlock()
	for _, st := range subagents {
		log.Printf("agentpod subagent turn %s/%s: runtime %s replaced by %s; failing it", slug, st.turnID, st.runtime, runtimeID)
		s.onAgentpodFailed(st, agentpod.TurnEvent{
			Kind:         agentpod.TurnEventFailed,
			FailedReason: agentpod.FailedReasonOtherError,
			FailedDetail: "the agent runtime this task ran in stopped before it finished",
		})
	}
	if parent != nil {
		log.Printf("agentpod turn %s/%s: runtime %s replaced by %s; ending it", slug, parent.turnID, parent.runtime, runtimeID)
		s.endLostTurn(parent, planned)
	}
}

// endLostTurn ends a parent chat turn whose runtime went away without
// finishing it: cancelled when the CEO had pressed Stop, a Planned
// disruption when Kivali recreated the pod, a runtime disruption
// otherwise.
func (s *Server) endLostTurn(st *agentpodTurnState, planned bool) {
	reason, detail := failedReasonRuntimeLost, "the agent runtime stopped before the turn finished"
	switch {
	case st.hub != nil && st.hub.peekInterruptRequested():
		reason, detail = agentpod.FailedReasonCancelled, "agent runtime gone; the owner had pressed Stop"
	case planned:
		reason, detail = failedReasonRuntimeRestarted, "the agent restarted to apply a change and picks up where it left off"
	}
	s.onAgentpodFailed(st, agentpod.TurnEvent{
		Kind:         agentpod.TurnEventFailed,
		FailedReason: reason,
		FailedDetail: detail,
	})
}

// finalizeRuntimeDisruption writes a kind:runtime-disruption row with
// content, Planned as given, and paints it on hub (nil skips the live
// emit).
func (s *Server) finalizeRuntimeDisruption(slug string, hub *chatHub, content string, planned bool) {
	msg := store.ChatMessage{
		Role:    store.RoleReceived,
		Kind:    store.KindRuntimeDisruption,
		Content: content,
		TS:      s.clk().Now().UTC(),
		Planned: planned,
	}
	if err := s.Store.AppendChatMessage(slug, msg); err != nil {
		log.Printf("chat %s: append runtime disruption: %v", slug, err)
		return
	}
	emitChatMarker(hub, msg)
}
