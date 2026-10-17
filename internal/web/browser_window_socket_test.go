package web

import (
	"context"
	"encoding/json"
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

// readEvent is the window's next text message as an event.
func readEvent(t *testing.T, ctx context.Context, c *websocket.Conn) apitypes.BrowserWindowEvent {
	t.Helper()
	typ, data, err := c.Read(ctx)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if typ != websocket.MessageText {
		t.Fatalf("a binary message where an event was expected: %q", data)
	}
	var ev apitypes.BrowserWindowEvent
	if err := json.Unmarshal(data, &ev); err != nil {
		t.Fatal(err)
	}
	return ev
}

// TestBrowserWindowOverTheWire runs the window route end to end over a
// real WebSocket: an unknown task is 404, an ended task gets its end
// at once, and a running task's window is told the browser is
// starting, hears its state and the shown tab once it runs, receives
// frames as binary messages after the events they follow, and is ended
// with a reason when the task ends.
func TestBrowserWindowOverTheWire(t *testing.T) {
	svc, _, _, prov := webService(t, &fakeSubagentDriver{})
	schema, _ := structured.Parse([]byte(quoteSchema))
	svc.registerJob(&subagentJob{ID: "w1", Parent: "alice", Kind: agentpod.SubagentKindWeb, Session: agentpod.OpenWebSession, State: subagentJobRunning, Schema: schema, done: make(chan struct{})})
	svc.registerJob(&subagentJob{ID: "w2", Parent: "alice", Kind: agentpod.SubagentKindWeb, Session: agentpod.OpenWebSession, State: "done", Schema: schema, done: make(chan struct{})})
	srv := &Server{Store: svc.Store, Provider: svc.Provider, SubagentService: svc, Browsers: svc.Browsers}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v1/agents/{slug}/subagents/{id}/browser", srv.handleBrowserWindow)
	hs := httptest.NewServer(mux)
	defer hs.Close()
	base := "ws" + strings.TrimPrefix(hs.URL, "http") + "/api/v1/agents/alice/subagents/"
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	if _, resp, err := websocket.Dial(ctx, base+"nope/browser", nil); err == nil || resp == nil || resp.StatusCode != http.StatusNotFound {
		t.Fatalf("unknown task: err=%v resp=%v", err, resp)
	}
	ended, _, err := websocket.Dial(ctx, base+"w2/browser", nil)
	if err != nil {
		t.Fatal(err)
	}
	if ev := readEvent(t, ctx, ended); ev.Type != apitypes.BrowserWindowEventTypeEnd || ev.Reason != "The task has ended." {
		t.Fatalf("ended task's event = %+v", ev)
	}
	_ = ended.CloseNow()

	c, _, err := websocket.Dial(ctx, base+"w1/browser", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.CloseNow() }()
	if ev := readEvent(t, ctx, c); ev.Type != apitypes.BrowserWindowEventTypeState || ev.Browser != apitypes.BrowserStateStarting {
		t.Fatalf("first event = %+v, want starting", ev)
	}
	// The window started the browser; play its pod.
	token := <-prov.startedC
	link := newFakeLink()
	b := svc.Browsers.claim("alice", agentpod.OpenWebSession, token, link)
	if b == nil {
		t.Fatal("claim found no waiting browser")
	}
	if err := svc.Browsers.attach(context.Background(), b); err != nil {
		t.Fatal(err)
	}
	// This cluster would not let Chromium start its sandbox: the pod says so, and so do the window and the task's header.
	svc.Browsers.onMessage(b, browserpod.Message{Type: browserpod.TypeState, State: browserpod.StateRunning, Detail: "sandbox: off"}, nil)
	if on := link.next(t, browserpod.TypeScreencast); on.msg.Group != "w1" || on.msg.Tab != "" || !on.msg.OK {
		t.Fatalf("screencast switch = %+v", on.msg)
	}
	if ev := readEvent(t, ctx, c); ev.Type != apitypes.BrowserWindowEventTypeState || ev.Browser != apitypes.BrowserStateRunning || !ev.SandboxOff {
		t.Fatalf("event once running = %+v, want the sandbox off", ev)
	}
	if meta := srv.subagentMetaView("alice", "alice", "w1", nil); meta.Browser != apitypes.BrowserStateRunning || !meta.BrowserSandboxOff {
		t.Errorf("task header = browser %q, sandbox off %v; want running with the sandbox off", meta.Browser, meta.BrowserSandboxOff)
	}
	if meta := srv.subagentMetaView("alice", "alice", "w2", nil); meta.Browser != "" || meta.BrowserSandboxOff {
		t.Errorf("ended task's header = browser %q, sandbox off %v; want neither", meta.Browser, meta.BrowserSandboxOff)
	}
	svc.Browsers.onMessage(b, browserpod.Message{Type: browserpod.TypeTab, Group: "w1", Tab: "t1", URL: "https://vendor.example/", Title: "Vendor"}, nil)
	svc.Browsers.onMessage(b, browserpod.Message{Type: browserpod.TypeFrame, Group: "w1", Tab: "t1", Bytes: 9}, strings.NewReader("jpegbytes"))
	if ev := readEvent(t, ctx, c); ev.Type != apitypes.BrowserWindowEventTypeTab || ev.Tab == nil || ev.Tab.ID != "t1" || ev.Tab.URL != "https://vendor.example/" || ev.Tab.Title != "Vendor" {
		t.Fatalf("tab event = %+v", ev)
	}
	typ, data, err := c.Read(ctx)
	if err != nil || typ != websocket.MessageBinary || string(data) != "jpegbytes" {
		t.Fatalf("frame = %v %q, %v", typ, data, err)
	}

	svc.Browsers.ReleaseGroup("alice", agentpod.OpenWebSession, "w1")
	if ev := readEvent(t, ctx, c); ev.Type != apitypes.BrowserWindowEventTypeEnd || ev.Reason != "The task has ended." {
		t.Fatalf("event after the task ended = %+v", ev)
	}
	if _, _, err := c.Read(ctx); err == nil {
		t.Error("the window stayed open after its end")
	}
}
