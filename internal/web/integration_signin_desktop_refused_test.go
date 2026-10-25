package web

import (
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/kivali-ai/kivali/internal/auth"
	"github.com/kivali-ai/kivali/internal/web/apitypes"
)

// Why this test exists: when the team's VM stood still through the
// computer's sleep, its clock is hours behind, and the handoff token
// the app minted for the system browser looks to the server like one
// that expires too far ahead. The browser then landed on the home page
// and the flow waited on its Go step for ten minutes, and nothing told
// the owner why GitHub never opened. The refused handoff must end the
// flow in words that name the clocks' disagreement, where the owner is
// looking (the flow's panel in the team's window), send the browser to
// the integration's page, and sign nothing in.
func TestDesktopHandoffRefusedEndsTheFlowInWords(t *testing.T) {
	srv, _, ts := desktopServer(t)
	f := startInWebview(t, srv, ts)
	// The supervisor's clock, the computer's, runs 10 h ahead of the
	// team's.
	skewed, err := auth.NewHandoffToken([]byte(testSessionKey), testOwnerEmail, srv.clk().Now().Add(10*time.Hour+auth.HandoffTTL))
	if err != nil {
		t.Fatal(err)
	}
	system, systemJar, landed := browserJar(t)
	resp, err := system.Get(ts.URL + auth.HandoffPath + "?t=" + url.QueryEscape(skewed) + "&next=" + f.ID)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if *landed == nil || (*landed).Path != "/org/integrations/crm" || (*landed).Query().Get("setup") != f.ID {
		t.Fatalf("the system browser landed on %v (%d)", *landed, resp.StatusCode)
	}
	ended := flowEnds(t, srv)
	if ended.State != apitypes.SetupFlowStateFailed || textOf(ended.Error) != errHandoffRefused("the team's clock is about 10 hours behind this computer's") {
		t.Fatalf("flow = %+v %q", ended, textOf(ended.Error))
	}
	base, _ := url.Parse(ts.URL)
	if hasCookie(systemJar, base, "/", auth.CookieName) || hasCookie(systemJar, base, connectCallbackPath, "kivali_connect") {
		t.Error("a refused handoff left a cookie in the system browser")
	}
	assertNotSignedIn(t, srv)
}

// Why this test exists: a refused token must end only a flow that is
// waiting for the browser it names. One that is not (it finished, or
// it is on a Form) and an id that names no flow are left as they are,
// and the browser goes to "/" as for any refused token: a request whose
// token failed must change nothing it does not name exactly.
func TestDesktopHandoffRefusedEndsOnlyAWaitingFlow(t *testing.T) {
	srv, _, ts := desktopServer(t)
	f := startInWebview(t, srv, ts)
	base, _ := url.Parse(ts.URL)
	expired, err := auth.NewHandoffToken([]byte(testSessionKey), testOwnerEmail, srv.clk().Now().Add(-time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	for _, next := range []string{"", "0000", strings.Repeat("f", 32)} {
		assertHandoffRefused(t, serveHandoff(t, srv, base.Host, expired, next), "next "+next)
	}
	if f := waitFlow(t, srv, "crm", apitypes.SetupFlowStateGo); f.State != apitypes.SetupFlowStateGo {
		t.Fatalf("the flow reads %+v %q", f, textOf(f.Error))
	}
	// The flow came back and finished: a late refusal naming it ends
	// nothing and sends the browser to "/".
	first := serveHandoff(t, srv, base.Host, handoffToken(t, srv), f.ID)
	if first.Code != http.StatusFound || cookieOf(first, "kivali_connect") == nil {
		t.Fatalf("first: %d to %q", first.Code, first.Header().Get("Location"))
	}
	authorize := authorizeOf(t, srv, f)
	q := authorize.Query()
	q.Set("redirect_uri", ts.URL+connectCallbackPath)
	authorize.RawQuery = q.Encode()
	back := vendorApprove(t, authorize)
	signedInThere(t, srv, callback(t, srv, back, cookieOf(first, "kivali_connect")))
	if done := flowEnds(t, srv); done.State != apitypes.SetupFlowStateDone {
		t.Fatalf("flow = %+v %q", done, textOf(done.Error))
	}
	assertHandoffRefused(t, serveHandoff(t, srv, base.Host, expired, f.ID), "a flow that finished")
	if done := flowEnds(t, srv); done.State != apitypes.SetupFlowStateDone {
		t.Fatalf("after the late refusal the flow reads %+v %q", done, textOf(done.Error))
	}
}
