package web

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/kivali-ai/kivali/internal/auth"
	"github.com/kivali-ai/kivali/internal/web/apitypes"
)

// Why this test exists: a browser Kivali Desktop handed off is the
// system browser, while the team's window follows the flow. Sent to the
// flow's page as any browser is, the owner lands in the web app with no
// idea where they are. The callback must answer that browser, and only
// that browser, with the page that says the setup goes on in the app;
// when the handoff names the app's listener, the page must tell it, at
// that address and no other, so the app can come to the front; and a
// listener of the wrong shape must be dropped, not fail the flow.
func TestDesktopHandoffReturnsTheBrowserToTheApp(t *testing.T) {
	token := strings.Repeat("a", 43)
	cases := []struct {
		name, query, ret string
	}{
		{"a listener", "&return_port=54321&return_token=" + token, "http://127.0.0.1:54321/connect/" + token},
		{"no listener", "", ""},
		{"a port that is none", "&return_port=0&return_token=" + token, ""},
		{"a token of the wrong shape", "&return_port=54321&return_token=abc", ""},
		{"a port alone", "&return_port=54321", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			srv, _, ts := desktopServer(t)
			base, _ := url.Parse(ts.URL)
			f := startInWebview(t, srv, ts)
			req := httptest.NewRequest(http.MethodGet, auth.HandoffPath+"?t="+url.QueryEscape(handoffToken(t, srv))+"&next="+f.ID+c.query, nil)
			req.Host = base.Host
			first := httptest.NewRecorder()
			srv.Handler().ServeHTTP(first, req)
			if first.Code != http.StatusFound || first.Header().Get("Location") != connectStartPath+"?flow="+f.ID || cookieOf(first, "kivali_connect") == nil {
				t.Fatalf("handoff: %d to %q", first.Code, first.Header().Get("Location"))
			}
			authorize := authorizeOf(t, srv, f)
			q := authorize.Query()
			q.Set("redirect_uri", ts.URL+connectCallbackPath)
			authorize.RawQuery = q.Encode()
			desktopCallback(t, srv, vendorApprove(t, authorize), c.ret, cookieOf(first, "kivali_connect"))
			if f := flowEnds(t, srv); f.State != apitypes.SetupFlowStateDone {
				t.Fatalf("the flow ended %+v %q", f, textOf(f.Error))
			}
		})
	}
	// A browser that left through /connect/start itself, not the
	// handoff, goes back to the flow's page as before.
	t.Run("a browser of its own", func(t *testing.T) {
		srv, _ := browserServer(t)
		authorize, cookie := startBrowser(t, srv)
		signedInThere(t, srv, callback(t, srv, vendorApprove(t, authorize), cookie))
	})
}

// Why this test exists: the page's fonts come from the web build, whose
// file names carry a hash that changes with the bytes. The rules must
// name the files the binary carries, and a binary built without the web
// app (tests, a bare go build) must get a page that still renders, in
// the fallback faces, rather than rules naming files that are not there.
func TestConnectFontFacesComeFromTheBuild(t *testing.T) {
	build := fstest.MapFS{
		"assets/SpaceGrotesk-Medium-lFbtlQH6.woff2": {Data: []byte("f")},
		"assets/IBMPlexSans-Regular-aB3dE4fG.woff2": {Data: []byte("f")},
		"assets/index-Zz9Yy8Xx.js":                  {Data: []byte("j")},
	}
	got := connectFontFaces(build)
	for _, want := range []string{
		`@font-face{font-family:"Space Grotesk";src:url("/assets/SpaceGrotesk-Medium-lFbtlQH6.woff2") format("woff2");font-weight:500;`,
		`@font-face{font-family:"IBM Plex Sans";src:url("/assets/IBMPlexSans-Regular-aB3dE4fG.woff2") format("woff2");font-weight:400;`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("rules = %q, want %q", got, want)
		}
	}
	if strings.Count(got, "@font-face") != 2 || strings.Contains(got, "IBMPlexSans-Medium") {
		t.Errorf("rules = %q name a face the build has not", got)
	}
	if got := connectFontFaces(fstest.MapFS{}); got != "" {
		t.Errorf("without a build: %q", got)
	}
}

// Why this test exists: the return address goes into a script element
// verbatim. It is Kivali's own by construction, but a literal that
// could close the script or the string is a class of bug worth ruling
// out at the one place strings meet scripts.
func TestJSStringEscapesWhatCouldLeaveTheScript(t *testing.T) {
	plain := "http://127.0.0.1:1/connect/a-b_c"
	if got := jsString(plain); got != `"`+plain+`"` {
		t.Errorf("jsString(%q) = %s", plain, got)
	}
	hostile := "</script><b>\"\\\né&"
	got := jsString(hostile)
	inner, ok := strings.CutPrefix(got, `"`)
	inner, ok2 := strings.CutSuffix(inner, `"`)
	// Nine of the runes could leave the string or the script: each is
	// a \u escape; the letters and the slash stay as they are.
	if !ok || !ok2 || strings.ContainsAny(inner, "<>\"&\n\x00é") || strings.Count(inner, "\\u") != 9 || !strings.Contains(inner, "/script") {
		t.Errorf("jsString(%q) = %s", hostile, got)
	}
}
