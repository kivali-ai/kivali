package connect_test

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kivali-ai/kivali/internal/clock"
	"github.com/kivali-ai/kivali/internal/connect"
	"github.com/kivali-ai/kivali/internal/connect/connecttest"
)

func setup(t *testing.T) (*connect.Engine, *connecttest.FakeVendor, *clock.Fake) {
	t.Helper()
	clk := clock.NewFake()
	v := connecttest.New(t, clk)
	return &connect.Engine{Clock: clk, HTTP: &http.Client{Timeout: 30 * time.Second}}, v, clk
}

// Why this test exists: a refresh is how a connection keeps working
// after its access token's short life. If the request were not the
// form-encoded, JSON-asking POST with the client's credentials in the
// body that GitHub and HubSpot take, or expires_in were not read on
// Kivali's clock, every connection would stop working an hour after
// the owner signed in, and the owner would see "sign in again" for no
// reason. Both of a vendor's ways of sending expires_in are read.
func TestRefreshSendsTheClientAndReadsTheExpiry(t *testing.T) {
	for _, quoted := range []bool{false, true} {
		e, v, clk := setup(t)
		v.Set(func(k *connecttest.Knobs) { k.AccessTTL = time.Hour; k.ExpiresInString = quoted })
		old := v.Issue()
		got, err := e.Refresh(context.Background(), v.Endpoints(), v.Client(), old.Refresh)
		if err != nil {
			t.Fatalf("quoted %v: %v", quoted, err)
		}
		if !v.Valid(got.Access) || got.Access == old.Access {
			t.Errorf("quoted %v: access %q is not a new token the vendor issued", quoted, got.Access)
		}
		if want := clk.Now().Add(time.Hour); !got.ExpiresAt.Equal(want) {
			t.Errorf("quoted %v: expires %v, want %v", quoted, got.ExpiresAt, want)
		}
		if got.Scope != "repo" {
			t.Errorf("quoted %v: scope %q", quoted, got.Scope)
		}
		reqs := v.Requests()
		last := reqs[len(reqs)-1]
		if last.Accept != "application/json" || last.Form.Get("client_id") != connecttest.ClientID || last.Form.Get("client_secret") != connecttest.ClientSecret {
			t.Errorf("quoted %v: request = %+v", quoted, last)
		}
	}
}

// Why this test exists: a vendor that rotates refresh tokens accepts
// each one once. If Kivali kept the old one after a rotation, the next
// refresh would be refused and the owner would have to sign in again
// every time the token expired; if it dropped the old one when the
// vendor sends none, the connection would die at the second expiry.
func TestRefreshKeepsOrReplacesTheRefreshToken(t *testing.T) {
	e, v, _ := setup(t)
	v.Set(func(k *connecttest.Knobs) { k.Rotate = true })
	old := v.Issue()
	got, err := e.Refresh(context.Background(), v.Endpoints(), v.Client(), old.Refresh)
	if err != nil {
		t.Fatal(err)
	}
	if got.Refresh == old.Refresh || !v.RefreshAccepted(got.Refresh) || v.RefreshAccepted(old.Refresh) {
		t.Errorf("rotated: refresh %q (old %q)", got.Refresh, old.Refresh)
	}

	v.Set(func(k *connecttest.Knobs) { k.Rotate = false })
	again, err := e.Refresh(context.Background(), v.Endpoints(), v.Client(), got.Refresh)
	if err != nil {
		t.Fatal(err)
	}
	if again.Refresh != got.Refresh || !v.RefreshAccepted(again.Refresh) {
		t.Errorf("not rotated: refresh %q, want the old %q kept", again.Refresh, got.Refresh)
	}
	if !again.ExpiresAt.IsZero() {
		t.Errorf("a token sent with no expires_in expires at %v; it does not expire", again.ExpiresAt)
	}
}

// Why this test exists: a refresh failure either ends the connection
// (the owner must sign in again, and a Needs you row says so) or is a
// passing hiccup (the stored tokens stay and the next call tries
// again). Mixing the two up either nags the owner over a vendor's
// outage or leaves a dead connection that agents keep failing on with
// no one told. GitHub answers its refusals with a 200 and its own
// codes; RFC vendors with a 400; both are read the same way.
func TestRefreshTerminalOrTransient(t *testing.T) {
	cases := []struct {
		name     string
		knobs    func(k *connecttest.Knobs)
		refresh  string
		terminal string
	}{
		{"invalid_grant, RFC style", func(k *connecttest.Knobs) { k.RefreshError = "invalid_grant" }, "", "invalid_grant"},
		{"invalid_grant, GitHub style", func(k *connecttest.Knobs) { k.GitHubStyle = true; k.RefreshError = "invalid_grant" }, "", "invalid_grant"},
		{"unknown refresh token, RFC style", func(*connecttest.Knobs) {}, "not-a-token", "invalid_grant"},
		{"unknown refresh token, GitHub style", func(k *connecttest.Knobs) { k.GitHubStyle = true }, "not-a-token", "bad_refresh_token"},
		{"incorrect_client_credentials", func(k *connecttest.Knobs) { k.GitHubStyle = true; k.RefreshError = "incorrect_client_credentials" }, "", "incorrect_client_credentials"},
		{"invalid_client", func(k *connecttest.Knobs) { k.RefreshError = "invalid_client" }, "", "invalid_client"},
		{"unauthorized_client", func(k *connecttest.Knobs) { k.RefreshError = "unauthorized_client" }, "", "unauthorized_client"},
		{"access_denied", func(k *connecttest.Knobs) { k.RefreshError = "access_denied" }, "", "access_denied"},
		{"a vendor that is down", func(k *connecttest.Knobs) { k.RefreshStatus = http.StatusInternalServerError }, "", ""},
		{"an answer that is not JSON", func(k *connecttest.Knobs) { k.RefreshMalformed = true }, "", ""},
		{"another error code, RFC style", func(k *connecttest.Knobs) { k.RefreshError = "temporarily_unavailable" }, "", ""},
		{"another error code, GitHub style", func(k *connecttest.Knobs) { k.GitHubStyle = true; k.RefreshError = "unsupported_grant_type" }, "", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e, v, _ := setup(t)
			tok := v.Issue()
			v.Set(c.knobs)
			refresh := tok.Refresh
			if c.refresh != "" {
				refresh = c.refresh
			}
			_, err := e.Refresh(context.Background(), v.Endpoints(), v.Client(), refresh)
			if err == nil {
				t.Fatal("refresh went through")
			}
			var term *connect.Terminal
			switch {
			case c.terminal != "" && (!errors.As(err, &term) || term.Code != c.terminal):
				t.Errorf("err = %v, want terminal %s", err, c.terminal)
			case c.terminal == "" && errors.As(err, &term):
				t.Errorf("err = %v is terminal; it is transient", err)
			}
		})
	}
	// A vendor that does not answer at all is transient.
	e, v, _ := setup(t)
	ep := v.Endpoints()
	v.Close()
	_, err := e.Refresh(context.Background(), ep, v.Client(), "r")
	var term *connect.Terminal
	if err == nil || errors.As(err, &term) {
		t.Errorf("no answer: err = %v, want transient", err)
	}
}

// Why this test exists: Disconnect and Delete revoke the tokens where
// the vendor allows, so a removed connection cannot be used by whoever
// later reads an old Secret. The refresh token goes first (most
// vendors revoke the access token with it), each with the client's
// credentials; a manifest with no revocation endpoint sends nothing,
// and a vendor's failure is reported, never swallowed, so the caller
// can log it.
func TestRevoke(t *testing.T) {
	e, v, _ := setup(t)
	tok := v.Issue()
	if err := e.Revoke(context.Background(), v.Endpoints(), v.Client(), tok); err != nil {
		t.Fatal(err)
	}
	if got := v.Revoked(); len(got) != 2 || got[0] != tok.Refresh || got[1] != tok.Access {
		t.Errorf("revoked %v, want the refresh token then the access token", got)
	}
	if v.Valid(tok.Access) || v.RefreshAccepted(tok.Refresh) {
		t.Error("the tokens still work")
	}

	ep := v.Endpoints()
	ep.Revocation = ""
	before := len(v.Requests())
	if err := e.Revoke(context.Background(), ep, v.Client(), v.Issue()); err != nil || len(v.Requests()) != before {
		t.Errorf("no endpoint: err %v, %d requests sent", err, len(v.Requests())-before)
	}

	v.Set(func(k *connecttest.Knobs) { k.RevokeStatus = http.StatusServiceUnavailable })
	if err := e.Revoke(context.Background(), v.Endpoints(), v.Client(), v.Issue()); err == nil {
		t.Error("a failed revocation reported no error")
	}
}

// runPoll runs PollDevice in the background and returns where its
// outcome lands.
func runPoll(ctx context.Context, e *connect.Engine, v *connecttest.FakeVendor, ds *connect.DeviceStart) <-chan struct {
	tok connect.Tokens
	err error
} {
	out := make(chan struct {
		tok connect.Tokens
		err error
	}, 1)
	go func() {
		tok, err := e.PollDevice(ctx, v.Endpoints(), v.Client(), ds)
		out <- struct {
			tok connect.Tokens
			err error
		}{tok, err}
	}()
	return out
}

// Why this test exists: the device flow polls a vendor that tells it
// to wait (authorization_pending) and to wait longer (slow_down, which
// adds five seconds to the interval for good). A poller that ignored
// slow_down would be cut off by GitHub; one that slept would make the
// tests slow and flaky. Every wait is a timer on the injected clock,
// and the code start sends the client id and never the secret, which
// GitHub's device flow does not take.
func TestPollDeviceWaitsAndSlowsDown(t *testing.T) {
	e, v, clk := setup(t)
	v.Set(func(k *connecttest.Knobs) {
		k.GitHubStyle = true
		k.DeviceSlowDown = 1
		k.DevicePending = 2
		k.AccessTTL = 8 * time.Hour
	})
	ds, err := e.StartDevice(context.Background(), v.Endpoints(), v.Client(), []string{"repo", "read:org"})
	if err != nil {
		t.Fatal(err)
	}
	if ds.UserCode != "WDJB-MJHT" || ds.VerificationURI != v.URL+"/activate" || ds.Interval != 5*time.Second || !ds.ExpiresAt.Equal(clk.Now().Add(900*time.Second)) {
		t.Errorf("start = %+v", ds)
	}
	start := v.Requests()[0]
	if start.Form.Get("client_id") != connecttest.ClientID || start.Form.Has("client_secret") || start.Form.Get("scope") != "repo read:org" {
		t.Errorf("device start sent %v", start.Form)
	}
	done := runPoll(context.Background(), e, v, ds)
	// Intervals: 5 s, then slow_down makes it 10 s for the rest.
	for i, want := range []time.Duration{5 * time.Second, 10 * time.Second, 10 * time.Second, 10 * time.Second} {
		clk.BlockUntil(1)
		if d := clk.Deadlines()[0].Sub(clk.Now()); d != want {
			t.Fatalf("poll %d waits %v, want %v", i, d, want)
		}
		clk.Advance(want)
	}
	got := <-done
	if got.err != nil || !v.Valid(got.tok.Access) || got.tok.Refresh == "" || !got.tok.ExpiresAt.Equal(clk.Now().Add(8*time.Hour)) {
		t.Fatalf("poll = %+v, %v", got.tok, got.err)
	}
	for _, r := range v.Requests()[1:] {
		if r.Form.Has("client_secret") || r.Form.Get("client_id") != connecttest.ClientID {
			t.Errorf("a poll sent %v", r.Form)
		}
	}
}

// Why this test exists: a device flow ends without tokens in several
// ways, and the page tells the owner which: the code expired (start
// again), the owner or the vendor said no (the vendor's words). Each
// vendor style must end the loop rather than poll forever, and a code
// past its expiry on the clock must not be polled at all.
func TestPollDeviceEnds(t *testing.T) {
	for _, style := range []bool{false, true} {
		for _, c := range []struct {
			code    string
			expired bool
		}{
			{"access_denied", false}, {"expired_token", true}, {"incorrect_device_code", false},
			{"device_flow_disabled", false}, {"unsupported_grant_type", false},
		} {
			e, v, clk := setup(t)
			v.Set(func(k *connecttest.Knobs) { k.GitHubStyle = style; k.DeviceError = c.code })
			ds, err := e.StartDevice(context.Background(), v.Endpoints(), v.Client(), nil)
			if err != nil {
				t.Fatal(err)
			}
			done := runPoll(context.Background(), e, v, ds)
			clk.BlockUntil(1)
			clk.Advance(ds.Interval)
			got := <-done
			var term *connect.Terminal
			switch {
			case c.expired && !errors.Is(got.err, connect.ErrDeviceExpired):
				t.Errorf("github %v, %s: err = %v, want expired", style, c.code, got.err)
			case !c.expired && (!errors.As(got.err, &term) || term.Code != c.code || term.Description == ""):
				t.Errorf("github %v, %s: err = %v, want terminal with the vendor's words", style, c.code, got.err)
			}
		}
	}

	// The code's own expiry, on the clock: the poll that comes due
	// after it is never sent.
	e, v, clk := setup(t)
	v.Set(func(k *connecttest.Knobs) { k.DevicePending = 1000; k.DeviceExpiresIn = 10 * time.Second })
	ds, err := e.StartDevice(context.Background(), v.Endpoints(), v.Client(), nil)
	if err != nil {
		t.Fatal(err)
	}
	done := runPoll(context.Background(), e, v, ds)
	clk.BlockUntil(1)
	clk.Advance(5 * time.Second)
	clk.BlockUntil(1)
	clk.Advance(5 * time.Second)
	if got := <-done; !errors.Is(got.err, connect.ErrDeviceExpired) {
		t.Errorf("past the code's expiry: err = %v", got.err)
	}
}

// Why this test exists: a device flow the owner abandons (they left the
// page, the connection was deleted) must stop polling at once, not hold
// a goroutine and keep asking the vendor for fifteen minutes.
func TestPollDeviceStopsWithItsContext(t *testing.T) {
	e, v, clk := setup(t)
	v.Set(func(k *connecttest.Knobs) { k.DevicePending = 1000 })
	ds, err := e.StartDevice(context.Background(), v.Endpoints(), v.Client(), nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := runPoll(ctx, e, v, ds)
	clk.BlockUntil(1)
	cancel()
	if got := <-done; !errors.Is(got.err, context.Canceled) {
		t.Errorf("err = %v", got.err)
	}
}

// Why this test exists: browser sign-in sends the owner to the
// vendor's authorization address and trades the code it sends back
// for tokens. The address must carry the client, the exact redirect
// address, the scopes, the state (which ties the callback to the flow
// it started) and a PKCE challenge, and keep the endpoint's own
// parameters and the manifest's extra ones; the exchange must send the
// verifier, the same redirect address and the client's credentials. A
// vendor that checks PKCE refuses a wrong verifier, and a code works
// once.
func TestAuthorizeAndExchangeWithPKCE(t *testing.T) {
	e, v, clk := setup(t)
	v.Set(func(k *connecttest.Knobs) { k.AccessTTL = 30 * time.Minute })
	ep := v.Endpoints()
	ep.Authorization += "?prompt=consent"
	verifier := connect.NewPKCEVerifier()
	if len(verifier) < 43 || len(verifier) > 128 {
		t.Fatalf("verifier of %d characters", len(verifier))
	}
	const redirect = "https://kivali.example.ts.net/connect/callback"
	raw := e.AuthorizeURL(ep, v.Client(), redirect, "state-123", []string{"content", "crm.objects.contacts.read"}, verifier, map[string]string{"optional_scope": "automation"})
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	q := u.Query()
	for k, want := range map[string]string{
		"response_type": "code", "client_id": connecttest.ClientID, "redirect_uri": redirect, "state": "state-123",
		"scope": "content crm.objects.contacts.read", "code_challenge": connect.PKCEChallenge(verifier),
		"code_challenge_method": "S256", "prompt": "consent", "optional_scope": "automation",
	} {
		if q.Get(k) != want {
			t.Errorf("%s = %q, want %q", k, q.Get(k), want)
		}
	}
	if q.Has("client_secret") {
		t.Error("the authorization address carries the client secret")
	}

	// The vendor sends the browser back with a code and the state.
	noFollow := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := noFollow.Get(raw)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	back, err := url.Parse(resp.Header.Get("Location"))
	if err != nil || back.Query().Get("state") != "state-123" {
		t.Fatalf("redirect = %q", resp.Header.Get("Location"))
	}
	code := back.Query().Get("code")

	if _, err := e.Exchange(context.Background(), ep, v.Client(), code, redirect, connect.NewPKCEVerifier()); err == nil {
		t.Error("a wrong verifier was accepted")
	}
	// The code was spent on the refused attempt; a fresh one with the
	// right verifier goes through.
	resp, err = noFollow.Get(raw)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	back, _ = url.Parse(resp.Header.Get("Location"))
	code = back.Query().Get("code")
	tok, err := e.Exchange(context.Background(), ep, v.Client(), code, redirect, verifier)
	if err != nil {
		t.Fatal(err)
	}
	if !v.Valid(tok.Access) || tok.Refresh == "" || !tok.ExpiresAt.Equal(clk.Now().Add(30*time.Minute)) {
		t.Errorf("tokens = %+v", tok)
	}
	reqs := v.Requests()
	last := reqs[len(reqs)-1].Form
	if last.Get("code_verifier") != verifier || last.Get("redirect_uri") != redirect || last.Get("client_secret") != connecttest.ClientSecret {
		t.Errorf("exchange sent %v", last)
	}
	var term *connect.Terminal
	if _, err := e.Exchange(context.Background(), ep, v.Client(), code, redirect, verifier); !errors.As(err, &term) || term.Code != "invalid_grant" {
		t.Errorf("a code used twice: err = %v", err)
	}
}

// Why this test exists: a token is refreshed shortly before it
// expires, not after a call has failed with it, and never when the
// vendor gave no expiry (GitHub's tokens without expiry would
// otherwise be refreshed on every call, or never be usable).
func TestRefreshDue(t *testing.T) {
	now := time.Date(2026, 10, 22, 9, 0, 0, 0, time.UTC)
	for _, c := range []struct {
		exp  time.Time
		want bool
	}{
		{time.Time{}, false},
		{now.Add(time.Hour), false},
		{now.Add(connect.RefreshMargin + time.Second), false},
		{now.Add(connect.RefreshMargin), true},
		{now.Add(time.Second), true},
		{now.Add(-time.Hour), true},
	} {
		if got := connect.RefreshDue(now, c.exp); got != c.want {
			t.Errorf("expires %v: due %v, want %v", c.exp, got, c.want)
		}
	}
}

// Why this test exists: two agents' calls that find the same token
// about to expire must send the vendor one refresh. A vendor that
// rotates refresh tokens accepts each once, so a second refresh with
// the old token would be refused as invalid_grant and the connection
// marked "sign in again" for no fault of the owner's. Another
// connection's refresh is not held up by it.
func TestRefresherRunsOneRefreshPerKey(t *testing.T) {
	var r connect.Refresher
	joined := make(chan string, 8)
	connect.OnJoin(&r, func(key string) { joined <- key })
	var runs atomic.Int32
	release := make(chan struct{})
	started := make(chan struct{})
	fn := func(context.Context) (connect.Tokens, error) {
		runs.Add(1)
		close(started)
		<-release
		return connect.Tokens{Access: "new"}, nil
	}
	results := make(chan connect.Tokens, 5)
	go func() {
		tok, _ := r.Do(context.Background(), "github/a1", fn)
		results <- tok
	}()
	<-started
	for range 4 {
		go func() {
			tok, _ := r.Do(context.Background(), "github/a1", func(context.Context) (connect.Tokens, error) {
				runs.Add(1)
				return connect.Tokens{Access: "a second run"}, nil
			})
			results <- tok
		}()
	}
	for range 4 {
		<-joined
	}
	if tok, err := r.Do(context.Background(), "github/b2", func(context.Context) (connect.Tokens, error) { return connect.Tokens{Access: "b"}, nil }); err != nil || tok.Access != "b" {
		t.Errorf("another key = %+v, %v", tok, err)
	}
	close(release)
	for range 5 {
		if tok := <-results; tok.Access != "new" {
			t.Errorf("a caller got %+v, want the one run's tokens", tok)
		}
	}
	if runs.Load() != 1 {
		t.Errorf("%d runs for five callers at once", runs.Load())
	}
}

// Why this test exists: a refresh the vendor may already have answered
// has rotated the refresh token; if the caller's going away (an agent's
// call cancelled) cancelled the run, the new tokens would never be
// stored and the old refresh token would be refused next time. The
// caller stops waiting; the run goes on with a context that is not
// cancelled, and a caller still waiting gets its outcome.
func TestRefresherRunOutlivesItsCaller(t *testing.T) {
	var r connect.Refresher
	joined := make(chan string, 1)
	connect.OnJoin(&r, func(key string) { joined <- key })
	hold := make(chan struct{})
	inFlight := make(chan struct{})
	ctx, cancel := context.WithCancel(context.Background())
	var runErr error
	left := make(chan error, 1)
	go func() {
		_, err := r.Do(ctx, "github/c3", func(fctx context.Context) (connect.Tokens, error) {
			close(inFlight)
			<-hold
			runErr = fctx.Err()
			return connect.Tokens{Access: "c"}, nil
		})
		left <- err
	}()
	<-inFlight
	waiter := make(chan connect.Tokens, 1)
	go func() {
		tok, _ := r.Do(context.Background(), "github/c3", nil)
		waiter <- tok
	}()
	<-joined
	cancel()
	if err := <-left; !errors.Is(err, context.Canceled) {
		t.Errorf("the caller that left: err = %v", err)
	}
	close(hold)
	if tok := <-waiter; tok.Access != "c" {
		t.Errorf("the caller still waiting got %+v", tok)
	}
	if runErr != nil {
		t.Errorf("the run saw its caller's cancellation: %v", runErr)
	}
}
