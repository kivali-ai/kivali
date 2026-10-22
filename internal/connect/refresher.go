package connect

import (
	"context"
	"sync"
)

// Refresher runs one refresh at a time per key (an integration's name
// and a connection's id): a caller that asks while one is in flight
// for the same key waits for it and gets its outcome, so two calls
// that find the same token about to expire send the vendor one
// refresh, and a vendor that rotates refresh tokens never sees the old
// one twice. The zero value is ready to use.
type Refresher struct {
	mu      sync.Mutex
	flights map[string]*flight
	// onJoin, when set, runs as a caller joins a run in flight. A test
	// seam.
	onJoin func(key string)
}

type flight struct {
	done chan struct{}
	tok  Tokens
	err  error
}

// Do runs fn for key unless a run for key is in flight, in which case
// it waits for that run's outcome. fn runs with ctx's values but not
// its cancellation: a refresh the vendor may already have answered,
// rotating the refresh token, must be stored even when the caller that
// started it has gone. A caller whose ctx ends while it waits gets
// ctx's error; the run goes on.
func (r *Refresher) Do(ctx context.Context, key string, fn func(context.Context) (Tokens, error)) (Tokens, error) {
	r.mu.Lock()
	if r.flights == nil {
		r.flights = map[string]*flight{}
	}
	f := r.flights[key]
	if f == nil {
		f = &flight{done: make(chan struct{})}
		r.flights[key] = f
		go func() {
			f.tok, f.err = fn(context.WithoutCancel(ctx))
			r.mu.Lock()
			delete(r.flights, key)
			r.mu.Unlock()
			close(f.done)
		}()
	} else if r.onJoin != nil {
		r.onJoin(key)
	}
	r.mu.Unlock()
	select {
	case <-f.done:
		return f.tok, f.err
	case <-ctx.Done():
		return Tokens{}, ctx.Err()
	}
}
