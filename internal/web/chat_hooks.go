package web

import (
	"github.com/kivali-ai/kivali/internal/messaging"
	"github.com/kivali-ai/kivali/internal/tracker"
)

// deliveryHooks bundles the per-server callbacks the messaging
// layer needs when delivering a published message into an agent's
// chat.jsonl: routing through the mid-flight buffer
// (deliverToAgent) and waking the recipient
// (spawnChatLoopIfIdle). Constructed fresh per call so any of the
// underlying server fields can swap without restart races.
func (s *Server) deliveryHooks() messaging.DeliveryHooks {
	return messaging.DeliveryHooks{
		DeliverToAgent: s.deliverToAgent,
		WakeAgent: func(slug string) bool {
			// CEO-paced delivery: tag the spawn as "release" (the usage
			// row's purpose). Direct-chat posts use "chat".
			return s.spawnChatLoopIfIdle(slug, "release")
		},
	}
}

// tracker returns the assignment tracker, building it on first use. nil
// when the server has no Messenger to route wakes through.
func (s *Server) tracker() *tracker.Service {
	s.trackerOnce.Do(func() {
		if s.Tracker == nil && s.Messenger != nil && s.Store != nil {
			s.Tracker = tracker.New(s.Store, s.Messenger, s.deliveryHooks())
		}
	})
	return s.Tracker
}
