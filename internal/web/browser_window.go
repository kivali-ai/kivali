// Package web (browser_window.go) — the browser window: a person
// watching a web task's browser, live, and signing in through it when
// the task waits for them.
//
// GET /api/v1/agents/{slug}/subagents/{id}/browser, upgraded to a
// WebSocket under the session cookie like every API route. Text
// messages from core are apitypes.BrowserWindowEvent; binary messages
// are frames, one JPEG each, passed through from the pod untouched.
// Text messages from the page are apitypes.BrowserWindowInput, taken
// only while the task waits for a person in a site session and dropped
// otherwise: a watch-only window stays watch only whatever it sends.
//
// Each window has its own goroutine and its own latest-frame slot in
// the registry, so a slow viewer sees the newest frame when it next
// reads and never holds up the pod or another window; a viewer that
// cannot take a write within browserWindowWriteTimeout is dropped.

package web

import (
	"context"
	"encoding/json"
	"net/http"
	"sync/atomic"
	"time"

	"github.com/coder/websocket"

	"github.com/kivali-ai/kivali/internal/agentpod"
	"github.com/kivali-ai/kivali/internal/web/apitypes"
)

// browserWindowWriteTimeout bounds one write to the page.
const browserWindowWriteTimeout = 5 * time.Second

// browserWindowReadLimit bounds one message from the page: an input
// event, at most a pasted text.
const browserWindowReadLimit = 1 << 20

// handleBrowserWindow serves one browser window.
func (s *Server) handleBrowserWindow(w http.ResponseWriter, r *http.Request) {
	slug, id := r.PathValue("slug"), r.PathValue("id")
	if s.SubagentService == nil || s.Browsers == nil {
		http.Error(w, "browsers are not available in this deployment", http.StatusServiceUnavailable)
		return
	}
	j, ok := s.subagentJobByID(slug, id)
	if !ok || j.Kind != agentpod.SubagentKindWeb {
		http.NotFound(w, r)
		return
	}
	c, err := websocket.Accept(w, r, &websocket.AcceptOptions{CompressionMode: websocket.CompressionDisabled})
	if err != nil {
		return
	}
	defer func() { _ = c.CloseNow() }()
	c.SetReadLimit(browserWindowReadLimit)
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()

	// The page's messages: input for the shown tab, taken only while
	// the task waits for a person; reading is also how the socket's
	// close is seen. The watcher exists only once the browser is up.
	var watcher atomic.Pointer[browserWatcher]
	go func() {
		defer cancel()
		for {
			typ, data, err := c.Read(ctx)
			if err != nil {
				return
			}
			if typ != websocket.MessageText {
				continue
			}
			var in apitypes.BrowserWindowInput
			if json.Unmarshal(data, &in) != nil || in.Type != apitypes.BrowserWindowInputTypeInput {
				continue
			}
			w := watcher.Load()
			if w == nil || !s.SubagentService.windowTakesInput(id) {
				continue
			}
			_ = s.Browsers.Input(w, in.Input)
		}
	}()
	write := func(ev apitypes.BrowserWindowEvent) bool {
		body, err := json.Marshal(ev)
		if err != nil {
			return false
		}
		wctx, wcancel := context.WithTimeout(ctx, browserWindowWriteTimeout)
		defer wcancel()
		return c.Write(wctx, websocket.MessageText, body) == nil
	}
	live := j.State == subagentJobRunning || j.State == subagentJobWaiting
	if !live || j.cancelled || j.endReason != "" {
		write(apitypes.BrowserWindowEvent{Type: apitypes.BrowserWindowEventTypeEnd, Reason: "The task has ended."})
		_ = c.Close(websocket.StatusNormalClosure, "")
		return
	}
	input := j.State == subagentJobWaiting && j.Site != ""
	// A browser that is not up yet takes a while; the page is told so
	// before the wait.
	if st := s.Browsers.StateFor(slug, j.Session); st != apitypes.BrowserStateRunning {
		if !write(apitypes.BrowserWindowEvent{Type: apitypes.BrowserWindowEventTypeState, Browser: apitypes.BrowserStateStarting, Input: input, Site: j.Site}) {
			return
		}
	}
	wt, err := s.Browsers.Watch(ctx, slug, j.Session, id, input)
	if err != nil {
		write(apitypes.BrowserWindowEvent{Type: apitypes.BrowserWindowEventTypeEnd, Reason: err.Error()})
		_ = c.Close(websocket.StatusNormalClosure, "")
		return
	}
	defer s.Browsers.Unwatch(wt)
	watcher.Store(wt)
	// flushEvents sends every queued text message. Events are queued
	// before the frame they describe (a tab change before that tab's
	// first frame), so they go out before any frame, and before the
	// close that ends the window.
	flushEvents := func() bool {
		for {
			select {
			case ev := <-wt.Events():
				if !write(ev) {
					return false
				}
			default:
				return true
			}
		}
	}
	for {
		select {
		case <-ctx.Done():
			return
		case frame := <-wt.Frames():
			if !flushEvents() {
				return
			}
			wctx, wcancel := context.WithTimeout(ctx, browserWindowWriteTimeout)
			err := c.Write(wctx, websocket.MessageBinary, frame)
			wcancel()
			if err != nil {
				return
			}
		case ev := <-wt.Events():
			if !write(ev) {
				return
			}
		case <-wt.Done():
			flushEvents()
			_ = c.Close(websocket.StatusNormalClosure, "")
			return
		}
	}
}
