package githubintegration

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

// Why this test exists: GitHub reads the API version from
// X-GitHub-Api-Version and the answer's shape from Accept; a request
// without them gets whatever GitHub's default is that day, and a
// request without a User-Agent is refused. Every request carries them,
// and its credential as a bearer.
func TestRequestsCarryGitHubsHeaders(t *testing.T) {
	g, f := newTest(t)
	f.route("GET /repos/acme/widgets", 200, repoJSON)
	if r := g.callT(t, agent(false, "acme/*"), "get_repository", map[string]any{"repository": "acme/widgets"}); r.isError {
		t.Fatal(r.text)
	}
	for _, q := range f.requests() {
		if q.Header.Get("Accept") != "application/vnd.github+json" || q.Header.Get("X-GitHub-Api-Version") != "2022-11-28" ||
			q.Header.Get("User-Agent") == "" || q.Bearer == "" {
			t.Errorf("%s %s: headers %v", q.Method, q.Path, q.Header)
		}
	}
	if m := f.mints()[0]; m.Header.Get("Content-Type") != "application/json" {
		t.Errorf("mint content type %q", m.Header.Get("Content-Type"))
	}
}

// Why this test exists: GitHub redirects a renamed repository's API
// path on api.github.com, which is followed; a redirect anywhere else
// would carry the call, and its answer, off GitHub's API, so it is
// refused, and the agent reads that GitHub did not answer.
func TestRedirectsStayOnTheAPI(t *testing.T) {
	g, f := newTest(t)
	elsewhere := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("a request reached another host: %s %s", r.Method, r.URL)
	}))
	defer elsewhere.Close()
	f.routeFunc("GET /repos/acme/old", func(recorded) reply {
		return reply{Status: 301, Header: map[string]string{"Location": f.srv.URL + "/repositories/7"}}
	})
	f.route("GET /repositories/7", 200, repoJSON)
	f.routeFunc("GET /repos/acme/away", func(recorded) reply {
		return reply{Status: 302, Header: map[string]string{"Location": elsewhere.URL + "/steal"}}
	})
	reader := agent(false, "acme/*")
	if r := g.callT(t, reader, "get_repository", map[string]any{"repository": "acme/old"}); r.isError || !strings.Contains(r.text, `"repository":"acme/widgets"`) {
		t.Errorf("same origin: %+v", r)
	}
	if r := g.callT(t, reader, "get_repository", map[string]any{"repository": "acme/away"}); !r.isError || !strings.Contains(r.text, "GitHub redirected off its API") {
		t.Errorf("off origin: %+v", r)
	}
}

// Why this test exists: an answer is read into memory before it is
// cut, so one that has no end (a broken proxy, a huge diff) must stop
// at the bound rather than take the server's memory.
func TestAnswersAreReadToABound(t *testing.T) {
	g, f := newTest(t)
	f.route("GET /repos/acme/widgets", 200, `{"description":"`+strings.Repeat("x", maxAnswerBytes)+`"}`)
	r := g.callT(t, agent(false, "acme/*"), "get_repository", map[string]any{"repository": "acme/widgets"})
	if !r.isError || r.text != "GitHub did not answer: its answer is larger than Kivali reads. Try again later." {
		t.Errorf("%v %.200s", r.isError, r.text)
	}
}

// Why this test exists: the API base is a test hook. A plain http one
// would send the app's JWT and tokens in the clear, and another https
// host is one the manifest does not name and whose addresses the
// actions do not read (they keep what is under github.com). Only
// https://api.github.com, or http on loopback for the tests' fake, is
// ever used.
func TestTheAPIBaseIsGitHubsOrLoopback(t *testing.T) {
	for base, ok := range map[string]bool{
		"":                               true,
		"https://api.github.com":         true,
		"https://api.github.com/":        true,
		"https://ghe.acme.io/":           false,
		"https://api.github.com.evil.io": false,
		"http://127.0.0.1:9999":          true,
		"http://localhost:9999":          true,
		"http://api.github.com":          false,
		"http://10.0.0.1":                false,
		"ftp://api.github.com":           false,
		"https://user@x.io":              false,
		"api.github.com":                 false,
	} {
		_, err := (&GitHub{APIBase: base}).base()
		if (err == nil) != ok {
			t.Errorf("%q: %v", base, err)
		}
	}
	g := &GitHub{APIBase: "http://api.github.com"}
	if _, _, err := g.do(context.Background(), request{method: "GET", path: "/app", auth: "x"}); err == nil {
		t.Error("a request went out over plain http")
	}
}

// Why this test exists: Go follows a 301, 302 or 303 after a POST as a
// GET, so a create GitHub redirected would read something and answer
// as though it had written. Only a read is followed.
func TestARedirectedWriteIsNotFollowed(t *testing.T) {
	g, f := newTest(t)
	f.routeFunc("POST /repos/acme/widgets/issues", func(recorded) reply {
		return reply{Status: 302, Header: map[string]string{"Location": f.srv.URL + "/repos/acme/widgets/issues/1"}}
	})
	f.route("GET /repos/acme/widgets/issues/1", 200, `{"html_url":"https://github.com/acme/widgets/issues/1"}`)
	r := g.callT(t, agent(true, "acme/*"), "create_issue", map[string]any{"repository": "acme/widgets", "title": "t"})
	if !r.isError || !strings.Contains(r.text, "GitHub redirected a request that is not a read") {
		t.Errorf("%+v", r)
	}
	for _, q := range f.api() {
		if q.Method == "GET" {
			t.Errorf("the redirect was followed: %s", q.Path)
		}
	}
}

// Why this test exists: an error answer that is not GitHub's JSON (a
// proxy's or a load balancer's page) has no message to quote; the
// agent reads the status in words rather than the page or nothing.
func TestAnErrorWithoutGitHubsWordsNamesItsStatus(t *testing.T) {
	g, f := newTest(t)
	f.route("GET /repos/acme/widgets", 502, `<html>bad gateway</html>`)
	r := g.callT(t, agent(false, "acme/*"), "get_repository", map[string]any{"repository": "acme/widgets"})
	if !r.isError || r.text != "GitHub answered 502: Bad Gateway" {
		t.Errorf("%+v", r)
	}
}

type timeoutError struct{}

func (timeoutError) Error() string   { return "i/o timeout" }
func (timeoutError) Timeout() bool   { return true }
func (timeoutError) Temporary() bool { return true }

// Why this test exists: a call core abandons at its deadline, or a
// request past the client's 30 seconds, must read as GitHub not
// answering in time, with nothing to ask the owner; any other failure
// to reach GitHub names its cause, without the request's credential.
func TestTimeoutsAndCancelsReadAsNotInTime(t *testing.T) {
	g, f := newTest(t)
	f.route("GET /repos/acme/widgets", 200, repoJSON)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	res, err := g.Call(ctx, agent(false, "acme/*"), testConn(), "get_repository", json.RawMessage(`{"repository":"acme/widgets"}`))
	if err != nil || !res.IsError || res.Content[0] != "GitHub did not answer in time; try again later." {
		t.Errorf("cancelled: %+v %v", res, err)
	}
	for _, c := range []struct {
		err   error
		agent string
		owner string
	}{
		{&url.Error{Op: "Get", URL: "https://api.github.com/x", Err: timeoutError{}},
			"GitHub did not answer in time; try again later.", "GitHub did not answer in time; try again later."},
		{context.DeadlineExceeded,
			"GitHub did not answer in time; try again later.", "GitHub did not answer in time; try again later."},
		{&url.Error{Op: "Get", URL: "https://api.github.com/x", Err: errors.New("dial tcp: connection refused by ghs_secret")},
			"GitHub did not answer: dial tcp: connection refused by [credential]. Try again later.",
			"GitHub did not answer: dial tcp: connection refused by [credential]"},
	} {
		e := transportError(context.Background(), c.err, "ghs_secret")
		if got := agentWords(e); got != c.agent {
			t.Errorf("%v: agent %q", c.err, got)
		}
		if got := ownerWords(e); got != c.owner {
			t.Errorf("%v: owner %q", c.err, got)
		}
	}
}
