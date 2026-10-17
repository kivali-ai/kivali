package web

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/kivali-ai/kivali/internal/agentpod"
	"github.com/kivali-ai/kivali/internal/browserpod"
	"github.com/kivali-ai/kivali/internal/structured"
	"github.com/kivali-ai/kivali/internal/web/apitypes"
)

// windowServer serves the browser window route over a subagent
// service with browsers, and returns the service, the server, the pod
// provisioner and the route's base URL.
func windowServer(t *testing.T) (*SubagentService, *Server, *fakeBrowserProv, string) {
	t.Helper()
	svc, _, _, prov := webService(t, &fakeSubagentDriver{})
	srv := &Server{SubagentService: svc, Browsers: svc.Browsers}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v1/agents/{slug}/subagents/{id}/browser", srv.handleBrowserWindow)
	hs := httptest.NewServer(mux)
	t.Cleanup(hs.Close)
	return svc, srv, prov, "ws" + strings.TrimPrefix(hs.URL, "http") + "/api/v1/agents/alice/subagents/"
}

// readNextText is the window's next text message, skipping frames.
func readNextText(t *testing.T, ctx context.Context, c *websocket.Conn) apitypes.BrowserWindowEvent {
	t.Helper()
	for {
		typ, data, err := c.Read(ctx)
		if err != nil {
			t.Fatalf("read: %v", err)
		}
		if typ != websocket.MessageText {
			continue
		}
		var ev apitypes.BrowserWindowEvent
		if err := json.Unmarshal(data, &ev); err != nil {
			t.Fatal(err)
		}
		return ev
	}
}

// TestBrowserWindowChecksThePagesOrigin: a page from another origin is
// refused before the upgrade; the request's own host, the host a proxy
// forwarded, and the deployment's configured external origin are
// accepted.
func TestBrowserWindowChecksThePagesOrigin(t *testing.T) {
	svc, srv, _, base := windowServer(t)
	schema, _ := structured.Parse([]byte(quoteSchema))
	svc.registerJob(&subagentJob{ID: "w2", Parent: "alice", Kind: agentpod.SubagentKindWeb, Session: agentpod.OpenWebSession, State: subagentJobCompleted, Schema: schema, done: make(chan struct{})})
	srv.ExternalURL = "https://kivali.example:8443"
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	host := strings.TrimPrefix(base, "ws://")
	host = host[:strings.Index(host, "/")]

	for _, origin := range []string{"http://evil.example", "https://kivali.example", "http://kivali.example:8443"} {
		_, resp, err := websocket.Dial(ctx, base+"w2/browser", &websocket.DialOptions{HTTPHeader: http.Header{"Origin": {origin}}})
		if err == nil || resp == nil || resp.StatusCode != http.StatusForbidden {
			t.Errorf("origin %s: err=%v status=%v, want 403", origin, err, resp)
		}
	}
	accepted := []http.Header{
		{"Origin": {"http://" + host}},
		{"Origin": {"https://kivali.example:8443"}},
		{"Origin": {"https://kivali.internal"}, "X-Forwarded-Host": {"kivali.internal"}, "X-Forwarded-Proto": {"https"}},
	}
	for _, h := range accepted {
		c, _, err := websocket.Dial(ctx, base+"w2/browser", &websocket.DialOptions{HTTPHeader: h})
		if err != nil {
			t.Errorf("headers %v: %v, want accepted", h, err)
			continue
		}
		if ev := readEvent(t, ctx, c); ev.Type != apitypes.BrowserWindowEventTypeEnd {
			t.Errorf("headers %v: first event %+v", h, ev)
		}
		_ = c.CloseNow()
	}
}

// TestBrowserWindowTellsAQueuedTaskApart: a window on a task that has
// not started yet is ended with that reason, not the ended task's.
func TestBrowserWindowTellsAQueuedTaskApart(t *testing.T) {
	svc, _, _, base := windowServer(t)
	schema, _ := structured.Parse([]byte(quoteSchema))
	svc.registerJob(&subagentJob{ID: "q1", Parent: "alice", Kind: agentpod.SubagentKindWeb, Session: agentpod.OpenWebSession, State: subagentJobQueued, Schema: schema, done: make(chan struct{})})
	svc.registerJob(&subagentJob{ID: "q2", Parent: "alice", Kind: agentpod.SubagentKindWeb, Session: agentpod.OpenWebSession, State: subagentJobQueued, cancelled: true, Schema: schema, done: make(chan struct{})})
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	for id, want := range map[string]string{"q1": "The task has not started yet.", "q2": "The task has ended."} {
		c, _, err := websocket.Dial(ctx, base+id+"/browser", nil)
		if err != nil {
			t.Fatal(err)
		}
		if ev := readEvent(t, ctx, c); ev.Type != apitypes.BrowserWindowEventTypeEnd || ev.Reason != want {
			t.Errorf("%s: event = %+v, want end %q", id, ev, want)
		}
		_ = c.CloseNow()
	}
}

// TestBrowserWindowNoticesAnInputItCannotTake: a person's input on a
// waiting task is answered with a notice when the browser has no page
// to show it on or the paste is over the limit, and reaches the pod
// otherwise; a wait that began while the window waited for its browser
// is heard once the browser is up.
func TestBrowserWindowNoticesAnInputItCannotTake(t *testing.T) {
	svc, _, prov, base := windowServer(t)
	schema, _ := structured.Parse([]byte(quoteSchema))
	// Running, in a site session: the window attaches watch only.
	svc.registerJob(&subagentJob{ID: "s1", Parent: "alice", Kind: agentpod.SubagentKindWeb, Session: "vendor.example", Site: "vendor.example", State: subagentJobRunning, Depth: 1, Schema: schema, done: make(chan struct{})})
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	c, _, err := websocket.Dial(ctx, base+"s1/browser", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.CloseNow() }()
	if ev := readEvent(t, ctx, c); ev.Type != apitypes.BrowserWindowEventTypeState || ev.Browser != apitypes.BrowserStateStarting || ev.Input {
		t.Fatalf("first event = %+v, want starting, watch only", ev)
	}
	// While the browser starts, the task begins to wait for a person.
	svc.updateJob("s1", func(j *subagentJob) { j.State = subagentJobWaiting; j.WaitReason = "sign_in" })
	token := <-prov.startedC
	link := newFakeLink()
	b := svc.Browsers.claim("alice", "vendor.example", token, link)
	if b == nil {
		t.Fatal("claim found no waiting browser")
	}
	if err := svc.Browsers.attach(b); err != nil {
		t.Fatal(err)
	}
	svc.Browsers.onMessage(b, browserpod.Message{Type: browserpod.TypeState, State: browserpod.StateRunning}, nil)
	link.next(t, browserpod.TypeScreencast)
	if ev := readEvent(t, ctx, c); ev.Type != apitypes.BrowserWindowEventTypeState || ev.Browser != apitypes.BrowserStateRunning || ev.Input {
		t.Fatalf("event once running = %+v, want what the window was opened with", ev)
	}
	if ev := readEvent(t, ctx, c); ev.Type != apitypes.BrowserWindowEventTypeState || !ev.Input || ev.Site != "vendor.example" {
		t.Fatalf("event after the wait began = %+v, want input on", ev)
	}

	send := func(in apitypes.BrowserInput) {
		t.Helper()
		body, _ := json.Marshal(apitypes.BrowserWindowInput{Type: apitypes.BrowserWindowInputTypeInput, Input: in})
		if err := c.Write(ctx, websocket.MessageText, body); err != nil {
			t.Fatal(err)
		}
	}
	// No tab is shown yet: the input is not taken, and the page is told.
	send(apitypes.BrowserInput{Kind: "paste", Text: "hello"})
	if ev := readNextText(t, ctx, c); ev.Type != apitypes.BrowserWindowEventTypeNotice || ev.Reason != "The browser has no page to show yet." {
		t.Fatalf("notice without a page = %+v", ev)
	}
	svc.Browsers.onMessage(b, browserpod.Message{Type: browserpod.TypeTab, Group: "s1", Tab: "t1", URL: "https://vendor.example/login", Title: "Sign in"}, nil)
	svc.Browsers.onMessage(b, browserpod.Message{Type: browserpod.TypeFrame, Group: "s1", Tab: "t1", Bytes: 4}, strings.NewReader("jpeg"))
	if ev := readNextText(t, ctx, c); ev.Type != apitypes.BrowserWindowEventTypeTab || ev.Tab == nil || ev.Tab.ID != "t1" {
		t.Fatalf("tab event = %+v", ev)
	}
	// A paste over the limit is refused with the limit named; one within
	// it reaches the pod for the shown tab.
	send(apitypes.BrowserInput{Kind: "paste", Text: strings.Repeat("x", browserInputTextMax+1)})
	if ev := readNextText(t, ctx, c); ev.Type != apitypes.BrowserWindowEventTypeNotice || !strings.Contains(ev.Reason, "256 KB") {
		t.Fatalf("notice for an oversized paste = %+v", ev)
	}
	send(apitypes.BrowserInput{Kind: "paste", Text: "hello"})
	in := link.next(t, browserpod.TypeInput)
	var args browserpod.InputArgs
	if err := json.Unmarshal(in.msg.Args, &args); err != nil || in.msg.Tab != "t1" || in.msg.Group != "s1" || args.Kind != "paste" || args.Text != "hello" {
		t.Errorf("input frame = %+v %s", in.msg, in.msg.Args)
	}
	link.mu.Lock()
	inputs := 0
	for _, fr := range link.frames {
		if fr.msg.Type == browserpod.TypeInput {
			inputs++
		}
	}
	link.mu.Unlock()
	if inputs != 1 {
		t.Errorf("the pod received %d inputs, want only the one within the limit", inputs)
	}
}

// TestSentence: an error's text becomes a line for a person.
func TestSentence(t *testing.T) {
	for in, want := range map[string]string{
		"the browser is not running":              "The browser is not running.",
		"the browser has run for two hours; wait": "The browser has run for two hours; wait.",
		"Already a sentence.":                     "Already a sentence.",
		"":                                        "",
	} {
		if got := sentence(errors.New(in)); got != want {
			t.Errorf("sentence(%q) = %q, want %q", in, got, want)
		}
	}
}
