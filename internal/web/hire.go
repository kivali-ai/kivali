package web

import (
	"log"
)

// drainSlugState tears down every in-memory hold on slug — chat hub,
// agent-pod turn state, pending rotations, latest rotations, and
// buffered deliveries — and asks the runtime to cancel any in-flight
// turn. Idempotent and safe when there's nothing in flight; the
// offboard flow always calls it whether or not the agent was active.
//
// Called by respondAsCEO immediately before Runtime.ApplyOffboard.
// Order matters: drain BEFORE the archive renames the agent directory.
// Without the drain, an in-flight agent-pod chat turn keeps POSTing
// TurnEvents into the per-slug agentpodTurns map, and the rename moves
// chat.jsonl out from under those handlers — writes either fail with
// ErrNotFound or land in the renamed _archived path, neither of which
// is what the caller intended. The drain closes the chat hub, drops
// the in-flight turn state, and tells the runtime to cancel via
// EventCancelTurn so it stops producing events before kubelet sends
// SIGTERM.
func (s *Server) drainSlugState(slug string) {
	// Cancel the in-flight turn first so the runtime stops producing
	// events. publishCancelTurn is a no-op when no turn is registered.
	s.publishCancelTurn(slug)
	s.streamMu.Lock()
	// Retire the pending bubbles on the page before the hub closes;
	// the messages go with the agent. The hub leaves the map under the
	// lock and closes after it, once the queued pending_deleted events
	// are out.
	hub := s.chatHubs[slug]
	s.dropPendingLocked(slug, hub)
	if hub != nil {
		hub.markCompleted()
		delete(s.chatHubs, slug)
	}
	delete(s.agentpodTurns, slug)
	delete(s.latestRotations, slug)
	s.streamMu.Unlock()
	if hub != nil {
		hub.flushOrdered()
		hub.close()
	}
	// Clear the on-disk rotation marker if present. Outside the lock
	// because this is filesystem I/O.
	if err := s.Store.ClearPendingRotation(slug); err != nil {
		log.Printf("offboard %s: clear pending-rotation marker: %v", slug, err)
	}
	s.NotifyOrgState()
}
