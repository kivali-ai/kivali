package googleworkspace

import (
	"context"
	"net/http"
	"strings"
	"testing"
)

// callLabels is list_labels for an agent that may read mail.
func callLabels(ctx context.Context, t *testing.T, g *GoogleWorkspace) string {
	t.Helper()
	res, err := g.Call(ctx, agent(map[string]any{"gmail_read": true}), testConn(), "list_labels", nil)
	if err != nil || res == nil || !res.IsError {
		t.Fatalf("want an error result, got %+v, %v", res, err)
	}
	return res.Content[0]
}

// Why this test exists: a call whose context is already over (the
// agent's turn was cancelled) must not reach Google, and must answer as
// a timeout does rather than as a Google failure with a cause of its
// own.
func TestCallWithCancelledContext(t *testing.T) {
	f, g := newFakeGoogle(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if got := callLabels(ctx, t, g); got != "Google did not answer in time; try again later." {
		t.Errorf("words = %q", got)
	}
	if got := f.requests(); len(got) != 0 {
		t.Errorf("a cancelled call reached Google: %v", got)
	}
}

// Why this test exists: the client reads at most 16 MiB of an answer
// and only JSON, so a hostile or broken origin cannot fill the server's
// memory. An answer past the cap, or one that is not JSON, must reach
// the agent saying Kivali does not read it, never telling it to try
// again (it would answer the same), and a 5xx must say to try later.
func TestUnreadableAnswers(t *testing.T) {
	f, g := newFakeGoogle(t)
	path := "/gmail/v1/users/me/labels"

	f.failWith(path, 200, strings.Repeat(" ", maxAnswerBytes+1))
	if got := callLabels(context.Background(), t, g); got != "Google's answer was larger than Kivali reads." {
		t.Errorf("an answer over the cap: %q", got)
	}

	f.failWith(path, 200, "<html>not json</html>")
	if got := callLabels(context.Background(), t, g); got != "Google's answer was not one Kivali reads." {
		t.Errorf("an answer that is not JSON: %q", got)
	}

	f.failWith(path, 503, `{"error":{"code":503,"message":"The service is unavailable."}}`)
	if got := callLabels(context.Background(), t, g); got != "Google answered 503: The service is unavailable. Try again later." {
		t.Errorf("a 5xx: %q", got)
	}
}

// Why this test exists: the client holds the access token, so a
// redirect must never carry it somewhere else. A redirect to another
// origin must be refused before that origin is asked, and a redirect of
// anything but a read must be refused by the client's own rule.
func TestRefusedRedirects(t *testing.T) {
	f, g := newFakeGoogle(t)
	f.redirectTo("/gmail/v1/users/me/labels", f.docs.URL+"/gmail/v1/users/me/labels")
	got := callLabels(context.Background(), t, g)
	if !strings.Contains(got, "it redirected off its API") || strings.Contains(got, "ya29") {
		t.Errorf("a redirect to another origin: %q", got)
	}
	for _, r := range f.requests() {
		if strings.HasPrefix(r, "docs ") {
			t.Errorf("the other origin was asked: %v", f.requests())
		}
	}

	req, err := http.NewRequest(http.MethodPost, f.gmail.URL+"/x", nil)
	if err != nil {
		t.Fatal(err)
	}
	next, _ := http.NewRequest(http.MethodGet, f.gmail.URL+"/y", nil)
	if err := g.httpClient().CheckRedirect(next, []*http.Request{req}); err == nil || !strings.Contains(err.Error(), "not a read") {
		t.Errorf("a redirected POST: %v", err)
	}
}
