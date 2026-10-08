package web

import (
	"errors"
	"log"
	"time"

	"github.com/kivali-ai/kivali/internal/store"
	"github.com/kivali-ai/kivali/internal/web/apitypes"
)

// Pending messages: a CEO direct chat that arrived while the agent was
// mid-turn and waits in pendingDeliveries until the turn takes it (a
// fold) or ends (the flush). The buffer entry IS the pending message;
// nothing about it is persisted, because chat.jsonl means "what the
// model has" and this message is not that yet. See
// docs/developers/context-serialization.md.
//
// The page learns of it from the agent stream (pending_message,
// pending_offered, pending_delivered, pending_deleted), every event
// queued on the hub under streamMu together with the buffer change it
// reports and broadcast after the unlock (chatHub.flushOrdered), so
// the stream's order is the buffer's order. A page that connects
// late seeds from pendingMessagesFor rather than from the replay log,
// which starts only with the current turn's hub.

// pausedToDeliverContent is the text of the kind:paused-to-deliver
// marker. The CEO reads it on the page; the model reads it bracketed
// in front of the messages it introduces.
const pausedToDeliverContent = "Paused to deliver your message"

// pendingRestoreWindow is how long a deleted pending message can be
// put back. Matches the undo toast.
const pendingRestoreWindow = 10 * time.Second

var (
	errPendingNotFound       = errors.New("message is not pending")
	errPendingOffered        = errors.New("already with the agent")
	errPendingExpired        = errors.New("too late to restore")
	errPendingRotating       = errors.New("the agent is saving its memory before a new chat")
	errPendingNothingToPause = errors.New("no turn to pause")
	errPendingPauseFailed    = errors.New("could not reach the agent's pod")
)

// pendingTombstone is a deleted pending message kept for the restore
// window. followers are the ids that were queued behind it at delete
// time; a restore goes back in front of the first still waiting.
type pendingTombstone struct {
	bd        bufferedDelivery
	followers []string
	deletedAt time.Time
}

// tombstonesLocked returns s's tombstones for slug
// (Server.pendingTombstones), creating the maps on first use. Caller
// holds streamMu.
func (s *Server) tombstonesLocked(slug string) map[string]pendingTombstone {
	if s.pendingTombstones == nil {
		s.pendingTombstones = map[string]map[string]pendingTombstone{}
	}
	m := s.pendingTombstones[slug]
	if m == nil {
		m = map[string]pendingTombstone{}
		s.pendingTombstones[slug] = m
	}
	return m
}

// takeTombstoneLocked removes and returns slug's tombstone for id, when
// there is one, forgetting the slug's map once it is empty. Reads
// never create anything. Caller holds streamMu.
func (s *Server) takeTombstoneLocked(slug, id string) (pendingTombstone, bool) {
	t, ok := s.pendingTombstones[slug][id]
	if !ok {
		return pendingTombstone{}, false
	}
	delete(s.pendingTombstones[slug], id)
	if len(s.pendingTombstones[slug]) == 0 {
		delete(s.pendingTombstones, slug)
	}
	return t, true
}

// dropTombstonesLocked forgets every tombstone for slug. Caller holds
// streamMu.
func (s *Server) dropTombstonesLocked(slug string) {
	delete(s.pendingTombstones, slug)
}

// pendingMessageOf is the wire shape of a buffered delivery.
func pendingMessageOf(bd bufferedDelivery) apitypes.PendingMessage {
	atts := make([]apitypes.PendingAttachment, 0, len(bd.msg.Attachments))
	for _, a := range bd.msg.Attachments {
		atts = append(atts, apitypes.PendingAttachment{SHA: a.SHA, Name: a.Name})
	}
	queued := bd.queuedAt
	if queued.IsZero() {
		queued = bd.msg.TS
	}
	return apitypes.PendingMessage{
		ID:          bd.id,
		Text:        bd.msg.Content,
		QueuedAt:    queued.UnixMilli(),
		Attachments: atts,
		Deletable:   !bd.offered,
	}
}

// queuePendingEvent queues one pending_* event for bd on hub, when bd
// is a visible (CEO direct chat) delivery and there is a hub. Invisible
// deliveries were never announced, so they are never retired either.
// Called with streamMu held; the caller runs hub.flushOrdered after
// unlocking.
func queuePendingEvent(hub *chatHub, bd bufferedDelivery, kind string, payload any) {
	if hub == nil || !bd.visible {
		return
	}
	hub.queueOrdered(kind, payload)
}

// pendingMessagesFor is slug's pending messages in the order they will
// be delivered: the visible entries of its buffer. Never nil. The
// chat endpoint returns it so a page that connects mid-turn shows what
// is waiting; the stream events then keep it current.
func (s *Server) pendingMessagesFor(slug string) []apitypes.PendingMessage {
	s.streamMu.Lock()
	defer s.streamMu.Unlock()
	out := make([]apitypes.PendingMessage, 0, len(s.pendingDeliveries[slug]))
	for _, bd := range s.pendingDeliveries[slug] {
		if bd.visible {
			out = append(out, pendingMessageOf(bd))
		}
	}
	return out
}

// findVisibleLocked returns the index of the visible pending message
// id in slug's buffer, or -1. Caller holds streamMu.
func (s *Server) findVisibleLocked(slug, id string) int {
	for i, bd := range s.pendingDeliveries[slug] {
		if bd.id == id && bd.visible {
			return i
		}
	}
	return -1
}

// deletePending takes a pending message back before the agent has it.
// Possible only while it has not been offered to the running turn
// (errPendingOffered after). The message is kept as a tombstone for
// pendingRestoreWindow so restorePending can put it back.
func (s *Server) deletePending(slug, id string) error {
	s.streamMu.Lock()
	idx := s.findVisibleLocked(slug, id)
	if idx < 0 {
		s.streamMu.Unlock()
		return errPendingNotFound
	}
	buf := s.pendingDeliveries[slug]
	bd := buf[idx]
	if bd.offered {
		s.streamMu.Unlock()
		return errPendingOffered
	}
	followers := make([]string, 0, len(buf)-idx-1)
	for _, pd := range buf[idx+1:] {
		followers = append(followers, pd.id)
	}
	rest := append(buf[:idx:idx], buf[idx+1:]...)
	if len(rest) == 0 {
		delete(s.pendingDeliveries, slug)
	} else {
		s.pendingDeliveries[slug] = rest
	}

	now := s.clk().Now()
	tombs := s.tombstonesLocked(slug)
	// Forget tombstones whose window has passed; a restore of one of
	// those after this point answers 404 rather than 410.
	for tid, t := range tombs {
		if now.Sub(t.deletedAt) >= pendingRestoreWindow {
			delete(tombs, tid)
		}
	}
	tombs[id] = pendingTombstone{bd: bd, followers: followers, deletedAt: now}
	hub := s.chatHubs[slug]
	queuePendingEvent(hub, bd, "pending_deleted", apitypes.PendingRef{ID: id})
	s.streamMu.Unlock()
	hub.flushOrdered()
	log.Printf("pending %s: message %s deleted by the CEO", slug, id)
	return nil
}

// restorePending puts a deleted pending message back, within
// pendingRestoreWindow of the delete. Mid-turn it goes back into the
// buffer where it was (in front of whatever was queued behind it and
// is still waiting), is announced again with pending_message, and is
// offered to the running turn again. If the turn has ended meanwhile
// it is delivered like a fresh message — straight into chat.jsonl,
// then a spawn — and the response carries its ts.
func (s *Server) restorePending(slug, id string) (apitypes.PendingRestoreResponse, error) {
	s.streamMu.Lock()
	t, ok := s.takeTombstoneLocked(slug, id)
	if !ok {
		s.streamMu.Unlock()
		return apitypes.PendingRestoreResponse{}, errPendingNotFound
	}
	if s.clk().Now().Sub(t.deletedAt) >= pendingRestoreWindow {
		s.streamMu.Unlock()
		return apitypes.PendingRestoreResponse{}, errPendingExpired
	}
	bd := t.bd
	bd.offered = false
	if !s.midTurnLocked(slug) {
		err := s.Store.AppendChatMessageLinkLater(slug, bd.msg)
		s.streamMu.Unlock()
		if err != nil {
			return apitypes.PendingRestoreResponse{}, err
		}
		s.Store.LinkChatAttachments(slug)
		s.spawnChatLoopIfIdle(slug, "chat")
		ts := bd.msg.TS.UnixMilli()
		return apitypes.PendingRestoreResponse{PendingMessage: pendingMessageOf(bd), DeliveredTS: &ts}, nil
	}
	bd, foldTurnID := s.stageLocked(slug, bd, t.followers)
	hub := s.chatHubs[slug]
	s.streamMu.Unlock()
	hub.flushOrdered()
	s.offerFold(slug, foldTurnID, bd)
	log.Printf("pending %s: message %s restored", slug, id)
	return apitypes.PendingRestoreResponse{PendingMessage: pendingMessageOf(bd)}, nil
}

// sendPendingNow is Send now: end the running turn early so every
// pending message reaches the agent now. It is preemptForDelivery with
// a latch (pausedForDelivery) that finalizeAgentpodTurn reads to keep
// the partial reply and write the paused-to-deliver marker in front of
// the flushed messages; the follow-up spawn then answers them.
//
// Parent turn only, like every delivery preempt: background subagents
// keep running. The flush drains the whole buffer, so Send now on one
// message delivers all of them, in order, under one marker.
func (s *Server) sendPendingNow(slug, id string) error {
	s.streamMu.Lock()
	if s.findVisibleLocked(slug, id) < 0 {
		s.streamMu.Unlock()
		return errPendingNotFound
	}
	// Cutting a rotation's memory reconcile short would break the
	// rotation; the message is delivered first thing in the new chat.
	if s.hasRotationPending(slug) {
		s.streamMu.Unlock()
		return errPendingRotating
	}
	st := s.agentpodTurns[slug]
	if st == nil || s.AgentpodHub == nil {
		s.streamMu.Unlock()
		return errPendingNothingToPause
	}
	st.pausedForDelivery = true
	if st.preemptRequested {
		// A wind-up is already on its way (a missed fold, or a second
		// click); the latch above is all this click adds.
		s.streamMu.Unlock()
		return nil
	}
	st.preemptRequested = true
	turnID := st.turnID
	s.streamMu.Unlock()

	if !s.preemptForDelivery(slug, turnID) {
		s.streamMu.Lock()
		if s.agentpodTurns[slug] == st {
			st.pausedForDelivery = false
			st.preemptRequested = false
		}
		s.streamMu.Unlock()
		return errPendingPauseFailed
	}
	log.Printf("pending %s: Send now on %s; pausing turn %s", slug, id, turnID)
	return nil
}

// dropPendingLocked queues pending_deleted on hub for every visible
// pending message of slug and forgets the buffer and tombstones.
// Offboarding calls it: the agent is going away, and so are the
// messages that were waiting for it. Caller holds streamMu and runs
// hub.flushOrdered after unlocking, before closing the hub.
func (s *Server) dropPendingLocked(slug string, hub *chatHub) {
	for _, bd := range s.pendingDeliveries[slug] {
		queuePendingEvent(hub, bd, "pending_deleted", apitypes.PendingRef{ID: bd.id})
	}
	delete(s.pendingDeliveries, slug)
	s.dropTombstonesLocked(slug)
}

// pausedToDeliverMarker is what finalizeAgentpodTurn writes in front
// of the messages a Send now delivers.
func pausedToDeliverMarker(now time.Time) store.ChatMessage {
	return store.ChatMessage{
		Role:    store.RoleReceived,
		Kind:    store.KindPausedToDeliver,
		Content: pausedToDeliverContent,
		TS:      now.UTC(),
	}
}
