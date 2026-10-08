package web

import (
	"log"
	"time"
)

// stagedPublishSweepInterval bounds how often the janitor walks the
// staged_publishes/ dir. The interval is significantly shorter than
// stagedPublishMaxAge so a stale record is reaped within roughly one
// max-age window of its creation.
const stagedPublishSweepInterval = 5 * time.Minute

// stagedPublishMaxAge is the TTL on a staged-publish record. The
// bridge's stage→commit window is sub-second on the happy path; even
// the worst transport flake recovers within a few seconds. 1h is
// 3+ orders of magnitude beyond that, so anything past the cutoff
// is genuinely abandoned (process crashed mid-publish, bridge gave
// up after exhausting retries, agent pod evicted between stage and
// commit, etc.).
const stagedPublishMaxAge = time.Hour

// StartStagedPublishJanitor starts a background goroutine that sweeps
// expired staged-publish records on the configured interval. Returns
// a stop function the caller invokes during shutdown — the goroutine
// drains the in-flight tick (no work in progress is left dangling)
// before returning.
//
// Idempotent on Store == nil (returns a no-op stop) so callers that
// haven't wired the store yet — test fixtures, e.g. — can call this
// unconditionally.
func (s *Server) StartStagedPublishJanitor() func() {
	if s.Store == nil {
		return func() {}
	}
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		ticker := s.clk().NewTicker(stagedPublishSweepInterval)
		defer ticker.Stop()
		for {
			select {
			case <-stop:
				return
			case now := <-ticker.C():
				removed, err := s.Store.SweepStagedPublishes(now, stagedPublishMaxAge)
				if err != nil {
					log.Printf("staged-publish janitor: sweep error (removed %d before failure): %v", removed, err)
				} else if removed > 0 {
					log.Printf("staged-publish janitor: removed %d expired records", removed)
				}
			}
		}
	}()
	return func() {
		close(stop)
		<-done
	}
}
