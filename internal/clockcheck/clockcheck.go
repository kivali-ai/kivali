// Package clockcheck validates the local system clock against public
// HTTP time references. Kivali buckets every message into a UTC date
// directory (messages/<YYYY-MM-DD>/) computed from time.Now().UTC(),
// so a skewed host clock silently writes messages into the wrong
// bucket. The check exists to surface that skew loudly — a banner
// in the UI when drift exceeds an hour, a field on /healthz, and a
// log line at boot — so the operator notices before audit-trail
// damage accumulates.
//
// The package intentionally does NOT refuse to boot or block writes
// on drift. A solo-founder workflow needs the pod to come up and
// keep running through transient network failures; the loud warning
// is sufficient for a single human operator who watches their own
// pod logs.
package clockcheck

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"time"

	"github.com/kivali-ai/kivali/internal/clock"
)

// DefaultRefs is the prioritized list of public HTTPS endpoints whose
// Date response header is read as the time reference. Listed in fall-
// back order: a single host being unreachable doesn't fail the check.
//
// All three are CDN-grade endpoints used by their operators as
// always-available probes (Google's /generate_204 is the Android
// captive-portal probe; Cloudflare's /cdn-cgi/trace returns a tiny
// text payload; Apple's gsa.apple.com responds to HEAD with the same
// Date header). Each returns within a few hundred ms typically.
var DefaultRefs = []string{
	"https://www.google.com/generate_204",
	"https://www.cloudflare.com/cdn-cgi/trace",
	"https://gsa.apple.com",
}

// HTTPClient is the interface the package needs from an HTTP client.
// Defaults to a *http.Client with a short timeout; overridable for tests.
type HTTPClient interface {
	Do(*http.Request) (*http.Response, error)
}

// Result is a single completed clock check. Drift is positive when the
// public reference is ahead of the local clock (i.e., local clock is
// behind), negative when local is ahead. CheckedAt is the local
// timestamp at which the check completed. Ref is the URL the Date
// header came from. Err is non-nil only when ALL refs failed; a
// successful Result has Err == nil even if some refs failed (the first
// successful one wins).
type Result struct {
	Drift     time.Duration
	CheckedAt time.Time
	Ref       string
	Err       error
}

// Check makes a single HEAD request to refURL and returns the drift
// between the response's Date header and time.Now(). Returns an error
// if the request fails, the response carries no Date header, or the
// Date can't be parsed.
//
// nowFn defaults to time.Now if nil; provided for testability.
func Check(ctx context.Context, client HTTPClient, refURL string, nowFn func() time.Time) (time.Duration, error) {
	if nowFn == nil {
		nowFn = time.Now
	}
	if client == nil {
		client = &http.Client{Timeout: 10 * time.Second}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodHead, refURL, nil)
	if err != nil {
		return 0, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return 0, err
	}
	defer func() { _ = resp.Body.Close() }()
	dh := resp.Header.Get("Date")
	if dh == "" {
		return 0, fmt.Errorf("no Date header from %s", refURL)
	}
	remote, err := http.ParseTime(dh)
	if err != nil {
		return 0, fmt.Errorf("parse Date %q from %s: %w", dh, refURL, err)
	}
	return remote.Sub(nowFn()), nil
}

// CheckMany walks refs in order, returning the drift from the first
// reference that responded successfully. Returns an aggregated error
// only if every ref failed.
func CheckMany(ctx context.Context, client HTTPClient, refs []string, nowFn func() time.Time) Result {
	if nowFn == nil {
		nowFn = time.Now
	}
	var errs []error
	for _, ref := range refs {
		drift, err := Check(ctx, client, ref, nowFn)
		if err == nil {
			return Result{Drift: drift, CheckedAt: nowFn(), Ref: ref}
		}
		errs = append(errs, err)
	}
	return Result{
		CheckedAt: nowFn(),
		Err:       fmt.Errorf("all %d refs failed: %w", len(refs), errors.Join(errs...)),
	}
}

// Watcher runs CheckMany periodically and exposes the latest Result
// behind a mutex. The result is consumed by:
//   - /healthz (drift seconds + age)
//   - the log line each check writes (so operators see drift in pod logs)
//
// A nil Watcher is usable: Latest() returns a zero Result, and
// /healthz treats the zero value as "no check yet" rather than
// "drift = 0". This keeps the rest of the system from having to guard
// against a nil pointer when clockcheck is disabled (tests, dev).
type Watcher struct {
	refs     []string
	interval time.Duration
	client   HTTPClient
	logf     func(string, ...any)
	nowFn    func() time.Time

	// clk drives the tick interval. The package's own Now seam is
	// nowFn, threaded through Check/CheckMany as a plain func; clk
	// covers the ticker, which
	// is what a test would otherwise have to wait six hours for.
	clk clock.Clock

	mu     sync.RWMutex
	latest Result
}

// Config configures a Watcher. Refs defaults to DefaultRefs; Interval
// defaults to 6 hours; Logf defaults to a no-op.
type Config struct {
	Refs     []string
	Interval time.Duration
	Client   HTTPClient
	Logf     func(string, ...any)

	// NowFn overrides time.Now (testing only).
	NowFn func() time.Time

	// Clock drives the tick interval. Nil means the real clock.
	Clock clock.Clock
}

// NewWatcher constructs a Watcher with the given config. The watcher
// is NOT started; call Run(ctx) to begin the periodic loop.
func NewWatcher(cfg Config) *Watcher {
	w := &Watcher{
		refs:     cfg.Refs,
		interval: cfg.Interval,
		client:   cfg.Client,
		logf:     cfg.Logf,
		nowFn:    cfg.NowFn,
		clk:      cfg.Clock,
	}
	if w.clk == nil {
		w.clk = clock.New()
	}
	if len(w.refs) == 0 {
		w.refs = DefaultRefs
	}
	if w.interval <= 0 {
		w.interval = 6 * time.Hour
	}
	if w.logf == nil {
		w.logf = func(string, ...any) {}
	}
	return w
}

// Run does an immediate check, stores the result, and then ticks every
// w.interval until ctx is cancelled. Each tick's result replaces the
// previous one. Errors are logged but don't terminate the loop —
// transient network failure shouldn't permanently disable the check.
//
// Intended to run in a dedicated goroutine: `go watcher.Run(ctx)`.
func (w *Watcher) Run(ctx context.Context) {
	w.tick(ctx)
	if w.interval <= 0 {
		return
	}
	t := w.clk.NewTicker(w.interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C():
			w.tick(ctx)
		}
	}
}

func (w *Watcher) tick(ctx context.Context) {
	res := CheckMany(ctx, w.client, w.refs, w.nowFn)
	w.mu.Lock()
	w.latest = res
	w.mu.Unlock()
	switch {
	case res.Err != nil:
		w.logf("clockcheck: failed to fetch any reference: %v", res.Err)
	case absDuration(res.Drift) >= time.Hour:
		w.logf("clockcheck: WARN large drift %s vs %s — host clock is wrong; messages will bucket by the wrong UTC date", res.Drift.Round(time.Second), res.Ref)
	case absDuration(res.Drift) >= 5*time.Minute:
		w.logf("clockcheck: drift %s vs %s (above 5m, below 1h)", res.Drift.Round(time.Second), res.Ref)
	default:
		w.logf("clockcheck: drift %s vs %s — within tolerance", res.Drift.Round(time.Second), res.Ref)
	}
}

// Latest returns the most recent Result. The zero Result (CheckedAt
// is zero) means no check has completed yet. Safe to call concurrently
// with Run.
func (w *Watcher) Latest() Result {
	if w == nil {
		return Result{}
	}
	w.mu.RLock()
	defer w.mu.RUnlock()
	return w.latest
}

// SetLatest replaces the stored Result. Intended for tests and
// dev-mode previews that want to assert a specific drift state
// (e.g. "render the banner when the watcher reports 2h drift") without
// spinning up an HTTP fake. Production code paths should never call
// this; the watcher's own Run() loop populates Latest from real
// network probes.
func (w *Watcher) SetLatest(r Result) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.latest = r
}

// AbsDrift is a UI helper: returns the absolute drift, or zero when
// the Result is the zero value (no check yet) or carries an error.
// Templates can compare AbsDrift > threshold without nil-checking.
func (r Result) AbsDrift() time.Duration {
	if r.CheckedAt.IsZero() || r.Err != nil {
		return 0
	}
	return absDuration(r.Drift)
}

func absDuration(d time.Duration) time.Duration {
	if d < 0 {
		return -d
	}
	return d
}
