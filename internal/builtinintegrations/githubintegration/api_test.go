package githubintegration

import (
	"context"
	"net/http"
	"net/http/httptest"
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

// Why this test exists: the API base is a test hook, and a plain http
// one would send the app's JWT and tokens in the clear. Only https, or
// http on loopback for the tests' fake, is ever used.
func TestTheAPIBaseIsHTTPSOrLoopback(t *testing.T) {
	for base, ok := range map[string]bool{
		"":                       true,
		"https://api.github.com": true,
		"https://ghe.acme.io/":   true,
		"http://127.0.0.1:9999":  true,
		"http://localhost:9999":  true,
		"http://api.github.com":  false,
		"http://10.0.0.1":        false,
		"ftp://api.github.com":   false,
		"https://user@x.io":      false,
		"api.github.com":         false,
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
