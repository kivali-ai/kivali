package clockcheck

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeClient mocks an HTTPClient. responses maps URL → response or
// error; the call counter lets tests assert how many refs were tried.
type fakeClient struct {
	mu    sync.Mutex
	resps map[string]fakeResp
	calls []string
}

type fakeResp struct {
	dateHeader string
	err        error
}

func (f *fakeClient) Do(req *http.Request) (*http.Response, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	url := req.URL.String()
	f.calls = append(f.calls, url)
	r, ok := f.resps[url]
	if !ok {
		return nil, errors.New("no fake response for " + url)
	}
	if r.err != nil {
		return nil, r.err
	}
	h := http.Header{}
	if r.dateHeader != "" {
		h.Set("Date", r.dateHeader)
	}
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     h,
		Body:       http.NoBody,
	}, nil
}

func TestCheckPositiveDrift(t *testing.T) {
	// Local clock is 30s behind. Public Date header reads later than
	// local now → positive drift.
	now := time.Date(2026, 4, 27, 12, 0, 0, 0, time.UTC)
	remote := now.Add(30 * time.Second)
	c := &fakeClient{resps: map[string]fakeResp{
		"https://example/ok": {dateHeader: remote.Format(http.TimeFormat)},
	}}
	got, err := Check(context.Background(), c, "https://example/ok", func() time.Time { return now })
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	// HTTP Date format has 1s granularity, so an exact match isn't
	// guaranteed; assert the seconds match.
	if got.Round(time.Second) != 30*time.Second {
		t.Errorf("drift = %s, want ~30s", got)
	}
}

func TestCheckNegativeDrift(t *testing.T) {
	// Local clock is 5min ahead. Public Date is in the past → negative drift.
	now := time.Date(2026, 4, 27, 12, 5, 0, 0, time.UTC)
	remote := time.Date(2026, 4, 27, 12, 0, 0, 0, time.UTC)
	c := &fakeClient{resps: map[string]fakeResp{
		"https://example/ok": {dateHeader: remote.Format(http.TimeFormat)},
	}}
	got, err := Check(context.Background(), c, "https://example/ok", func() time.Time { return now })
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if got.Round(time.Second) != -5*time.Minute {
		t.Errorf("drift = %s, want -5m", got)
	}
}

func TestCheckNoDateHeader(t *testing.T) {
	c := &fakeClient{resps: map[string]fakeResp{
		"https://example/no-date": {dateHeader: ""},
	}}
	_, err := Check(context.Background(), c, "https://example/no-date", time.Now)
	if err == nil || !strings.Contains(err.Error(), "no Date header") {
		t.Fatalf("expected no-Date-header error, got %v", err)
	}
}

func TestCheckUnparseableDate(t *testing.T) {
	c := &fakeClient{resps: map[string]fakeResp{
		"https://example/bad": {dateHeader: "not a date"},
	}}
	_, err := Check(context.Background(), c, "https://example/bad", time.Now)
	if err == nil || !strings.Contains(err.Error(), "parse Date") {
		t.Fatalf("expected parse error, got %v", err)
	}
}

// TestCheckManyFallsBack: if the first ref fails, the second one is
// tried; the first successful result is returned and subsequent refs
// aren't called.
func TestCheckManyFallsBack(t *testing.T) {
	now := time.Date(2026, 4, 27, 12, 0, 0, 0, time.UTC)
	c := &fakeClient{resps: map[string]fakeResp{
		"https://a/fail": {err: errors.New("boom")},
		"https://b/ok":   {dateHeader: now.Add(2 * time.Second).Format(http.TimeFormat)},
		"https://c/ok":   {dateHeader: now.Format(http.TimeFormat)},
	}}
	res := CheckMany(context.Background(), c, []string{
		"https://a/fail", "https://b/ok", "https://c/ok",
	}, func() time.Time { return now })
	if res.Err != nil {
		t.Fatalf("expected success after fallback, got %v", res.Err)
	}
	if res.Ref != "https://b/ok" {
		t.Errorf("ref = %q, want b/ok (first success wins)", res.Ref)
	}
	if len(c.calls) != 2 {
		t.Errorf("expected exactly 2 ref calls (stop after first success); got %v", c.calls)
	}
}

// TestCheckManyAllFail: when every ref fails, the Result carries an
// error joining all underlying failures (so the operator sees which
// refs were tried and why each rejected).
func TestCheckManyAllFail(t *testing.T) {
	c := &fakeClient{resps: map[string]fakeResp{
		"https://a/fail": {err: errors.New("dns fail")},
		"https://b/fail": {err: errors.New("tls fail")},
	}}
	res := CheckMany(context.Background(), c, []string{
		"https://a/fail", "https://b/fail",
	}, time.Now)
	if res.Err == nil {
		t.Fatal("expected aggregated error")
	}
	if !strings.Contains(res.Err.Error(), "dns fail") || !strings.Contains(res.Err.Error(), "tls fail") {
		t.Errorf("aggregated error should mention all underlying failures; got %v", res.Err)
	}
}

// TestWatcherTickStoresResult: a single tick populates Latest() with
// the drift the fake client emitted.
func TestWatcherTickStoresResult(t *testing.T) {
	now := time.Date(2026, 4, 27, 12, 0, 0, 0, time.UTC)
	c := &fakeClient{resps: map[string]fakeResp{
		"https://x/ok": {dateHeader: now.Add(45 * time.Second).Format(http.TimeFormat)},
	}}
	w := NewWatcher(Config{
		Refs:     []string{"https://x/ok"},
		Interval: time.Hour,
		Client:   c,
		NowFn:    func() time.Time { return now },
	})
	w.tick(context.Background())
	got := w.Latest()
	if got.Err != nil {
		t.Fatalf("Latest err = %v", got.Err)
	}
	if got.Drift.Round(time.Second) != 45*time.Second {
		t.Errorf("drift = %s, want 45s", got.Drift)
	}
	if got.Ref != "https://x/ok" {
		t.Errorf("ref = %q", got.Ref)
	}
	if !got.CheckedAt.Equal(now) {
		t.Errorf("CheckedAt = %v, want %v", got.CheckedAt, now)
	}
}

// TestWatcherLogsDriftThresholds: the tick logs a different message
// at each band (within tolerance / 5m–1h / >=1h / failure). Lock the
// thresholds explicitly so a future refactor doesn't silently drop the
// "WARN large drift" message that drives the UI banner decision.
func TestWatcherLogsDriftThresholds(t *testing.T) {
	cases := []struct {
		name     string
		drift    time.Duration
		wantSub  string
		wantWarn bool
	}{
		{"within tolerance", 30 * time.Second, "within tolerance", false},
		{"medium", 10 * time.Minute, "above 5m, below 1h", false},
		{"large", 2 * time.Hour, "WARN large drift", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			now := time.Date(2026, 4, 27, 12, 0, 0, 0, time.UTC)
			c := &fakeClient{resps: map[string]fakeResp{
				"https://x/ok": {dateHeader: now.Add(tc.drift).Format(http.TimeFormat)},
			}}
			var logs []string
			w := NewWatcher(Config{
				Refs:   []string{"https://x/ok"},
				Client: c,
				NowFn:  func() time.Time { return now },
				Logf:   func(f string, a ...any) { logs = append(logs, f) },
			})
			w.tick(context.Background())
			joined := strings.Join(logs, "\n")
			if !strings.Contains(joined, tc.wantSub) {
				t.Errorf("log missing %q in: %s", tc.wantSub, joined)
			}
		})
	}
}

func TestWatcherLogsAllRefsFailed(t *testing.T) {
	c := &fakeClient{resps: map[string]fakeResp{
		"https://a/fail": {err: errors.New("boom")},
	}}
	var logs []string
	w := NewWatcher(Config{
		Refs:   []string{"https://a/fail"},
		Client: c,
		Logf:   func(f string, a ...any) { logs = append(logs, f) },
	})
	w.tick(context.Background())
	joined := strings.Join(logs, "\n")
	if !strings.Contains(joined, "failed to fetch any reference") {
		t.Errorf("expected failure log, got: %s", joined)
	}
	// Latest still records the failure (so /healthz can report it).
	got := w.Latest()
	if got.Err == nil {
		t.Error("Latest should carry the error after a failed tick")
	}
}

// TestNilWatcherSafe: a nil *Watcher's Latest() returns the zero
// Result. The web layer relies on this so callers never need to
// nil-check before calling Latest().
func TestNilWatcherSafe(t *testing.T) {
	var w *Watcher
	got := w.Latest()
	if got.CheckedAt.IsZero() == false || got.Err != nil || got.Drift != 0 {
		t.Errorf("nil watcher Latest should return zero Result; got %+v", got)
	}
}

// TestResultAbsDrift: zero Result and error Result both report
// AbsDrift = 0 so templates can branch on > threshold without having
// to also test for "no check yet".
func TestResultAbsDrift(t *testing.T) {
	zero := Result{}
	if zero.AbsDrift() != 0 {
		t.Errorf("zero Result AbsDrift = %s, want 0", zero.AbsDrift())
	}
	errRes := Result{CheckedAt: time.Now(), Drift: 2 * time.Hour, Err: errors.New("x")}
	if errRes.AbsDrift() != 0 {
		t.Errorf("error Result AbsDrift = %s, want 0", errRes.AbsDrift())
	}
	pos := Result{CheckedAt: time.Now(), Drift: 30 * time.Minute}
	if pos.AbsDrift() != 30*time.Minute {
		t.Errorf("positive drift AbsDrift = %s, want 30m", pos.AbsDrift())
	}
	neg := Result{CheckedAt: time.Now(), Drift: -45 * time.Minute}
	if neg.AbsDrift() != 45*time.Minute {
		t.Errorf("negative drift AbsDrift = %s, want 45m", neg.AbsDrift())
	}
}
