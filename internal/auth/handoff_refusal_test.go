package auth

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// Why this test exists: a refused handoff used to leave one fixed log
// line and a browser on "/", whatever the cause. When Kivali Desktop's
// VM has stood still through the computer's sleep, the team's clock is
// hours behind the supervisor's, every token the supervisor mints
// looks to the server like one that expires too far ahead, and the
// owner's browser lands on the home page with nothing to go on. The
// refusal must say what happened, in a measure a person can act on:
// which clock is off and by about how much, a link opened too late, a
// link used twice, an account not invited, a link not this team's.
func TestHandoffRefusalWords(t *testing.T) {
	// A token's expiry is whole seconds; the measures below are exact
	// only from a whole second.
	now := time.Now().Truncate(time.Second)
	h := handoffAt(t, &now, "")
	owner := "owner@example.com"
	good := mint(t, handoffKey, owner, now.Add(HandoffTTL))
	_, _ = h.redeem(good)
	cases := []struct {
		name, token, cause, words, logged string
	}{
		// The supervisor's clock runs 10 h ahead of the server's: the
		// token expires 10 h 2 min ahead.
		{"team clock behind", mint(t, handoffKey, owner, now.Add(10*time.Hour+HandoffTTL)), HandoffRefusedTooFarAhead,
			"the team's clock is about 10 hours behind this computer's", "too far ahead by 10h0m0s"},
		{"team clock behind by minutes", mint(t, handoffKey, owner, now.Add(4*time.Minute+HandoffTTL)), HandoffRefusedTooFarAhead,
			"the team's clock is about 4 minutes behind this computer's", "too far ahead by 4m0s"},
		// The supervisor's clock runs 1 h behind: the token expired 58 min ago.
		{"team clock ahead", mint(t, handoffKey, owner, now.Add(HandoffTTL-time.Hour)), HandoffRefusedExpired,
			"the team's clock is about 1 hour ahead of this computer's", "expired by 1h0m0s"},
		{"opened late", mint(t, handoffKey, owner, now.Add(-30*time.Second)), HandoffRefusedExpired,
			"the sign-in link was opened too late", "expired by 2m30s"},
		{"replayed", good, HandoffRefusedReplayed, "the sign-in link was already used", "replayed"},
		{"not invited", mint(t, handoffKey, "stranger@example.com", now.Add(HandoffTTL)), HandoffRefusedNotInvited,
			"the account the link signs in is not invited to this team", "not invited"},
		{"another key", mint(t, []byte("another key of 32 bytes........."), owner, now.Add(HandoffTTL)), HandoffRefusedMalformed,
			"the sign-in link was not issued by this team", "malformed"},
		{"empty", "", HandoffRefusedMalformed, "the sign-in link was not issued by this team", "malformed"},
	}
	for _, c := range cases {
		email, why := h.redeem(c.token)
		if email != "" || why == nil {
			t.Fatalf("%s: signed in %q", c.name, email)
		}
		if why.Cause != c.cause || why.Words() != c.words || why.Error() != c.logged {
			t.Errorf("%s: %q %q %q, want %q %q %q", c.name, why.Cause, why.Words(), why.Error(), c.cause, c.words, c.logged)
		}
	}
	if _, why := h.redeem(mint(t, handoffKey, owner, now.Add(HandoffTTL))); why != nil {
		t.Errorf("after the refusals a good token was refused: %v", why)
	}
	for d, want := range map[time.Duration]string{40 * time.Second: "40 seconds", time.Second: "1 second", 90 * time.Second: "2 minutes", time.Minute: "1 minute", 10*time.Hour + 17*time.Second: "10 hours", 29 * time.Minute: "29 minutes"} {
		if got := aboutDuration(d); got != want {
			t.Errorf("aboutDuration(%s) = %q, want %q", d, got, want)
		}
	}
}

// Why this test exists: the refusal hook is how a setup flow learns
// that the browser Kivali Desktop sent it never arrived. It must be
// told of a refused token that names a next, with the cause, and its
// answer must be where the browser goes; it must not be asked for a
// token that passed, for one without a next, or at a host that is not
// loopback (no route there); and whatever it answers, a refused
// request signs nothing in.
func TestHandoffRefusedHookIsToldOfTheNext(t *testing.T) {
	now := time.Now().Truncate(time.Second)
	h := handoffAt(t, &now, "")
	var told []string
	handler := h.WithNext(func(w http.ResponseWriter, r *http.Request, next string) string {
		return "/connect/start?flow=" + next
	}, func(r *http.Request, next string, why HandoffRefusal) string {
		told = append(told, next+": "+why.Cause)
		if next == "known" {
			return "/org/integrations/crm?setup=known"
		}
		return ""
	})
	redeem := func(host, token, next string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, HandoffPath+"?t="+token+"&next="+next, nil)
		req.Host = host
		rr := httptest.NewRecorder()
		handler.ServeHTTP(rr, req)
		return rr
	}
	skewed := mint(t, handoffKey, "owner@example.com", now.Add(10*time.Hour))
	if rr := redeem("127.0.0.1:18081", skewed, "known"); rr.Code != http.StatusFound || rr.Header().Get("Location") != "/org/integrations/crm?setup=known" || cookieNamed(rr, CookieName) != nil {
		t.Fatalf("refused with a known next: %d to %q, cookies %v", rr.Code, rr.Header().Get("Location"), rr.Result().Cookies())
	}
	// The hook answers "": the browser goes to "/".
	if rr := redeem("127.0.0.1:18081", skewed, "unknown"); rr.Code != http.StatusFound || rr.Header().Get("Location") != "/" || cookieNamed(rr, CookieName) != nil {
		t.Fatalf("refused with an unknown next: %d to %q", rr.Code, rr.Header().Get("Location"))
	}
	// A refusal without a next, a token that passes, and a host that is
	// not loopback: not told.
	if rr := redeem("127.0.0.1:18081", skewed, ""); rr.Header().Get("Location") != "/" {
		t.Fatalf("refused without next: to %q", rr.Header().Get("Location"))
	}
	good := mint(t, handoffKey, "owner@example.com", now.Add(HandoffTTL))
	if rr := redeem("127.0.0.1:18081", good, "known"); rr.Header().Get("Location") != "/connect/start?flow=known" || cookieNamed(rr, CookieName) == nil {
		t.Fatalf("passed: to %q", rr.Header().Get("Location"))
	}
	if rr := redeem("kivali.example.com", skewed, "known"); rr.Code != http.StatusNotFound {
		t.Fatalf("non-loopback: %d", rr.Code)
	}
	// The replay of the token that passed is told as such.
	if rr := redeem("127.0.0.1:18081", good, "known"); rr.Header().Get("Location") != "/org/integrations/crm?setup=known" || cookieNamed(rr, CookieName) != nil {
		t.Fatalf("replay: to %q", rr.Header().Get("Location"))
	}
	if got := strings.Join(told, ", "); got != "known: too far ahead, unknown: too far ahead, known: replayed" {
		t.Fatalf("told %q", got)
	}
}
