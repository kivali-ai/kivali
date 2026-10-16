// Package web (browser_window.go) — the browser window: a person
// watching a web task's browser, live.
//
// GET /api/v1/agents/{slug}/subagents/{id}/browser, upgraded to a
// WebSocket under the session cookie like every API route. Text
// messages are apitypes.BrowserWindowEvent; binary messages are frames,
// one JPEG each, passed through from the pod untouched. Nothing is read
// from the page: the window watches.
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
	"time"

	"github.com/coder/websocket"

	"github.com/kivali-ai/kivali/internal/agentpod"
	"github.com/kivali-ai/kivali/internal/web/apitypes"
)

// browserWindowWriteTimeout bounds one write to the page.
const browserWindowWriteTimeout = 5 * time.Second

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
	defer c.CloseNow()
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	// The page sends nothing; reading is how the socket's close is seen.
	go func() {
		defer cancel()
		for {
			if _, _, err := c.Reader(ctx); err != nil {
				return
			}
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
	if j.State != subagentJobRunning || j.cancelled || j.endReason != "" {
		write(apitypes.BrowserWindowEvent{Type: apitypes.BrowserWindowEventTypeEnd, Reason: "The task has ended."})
		_ = c.Close(websocket.StatusNormalClosure, "")
		return
	}
	// A browser that is not up yet takes a while; the page is told so
	// before the wait.
	if st := s.Browsers.StateFor(slug, j.Session); st != apitypes.BrowserStateRunning {
		if !write(apitypes.BrowserWindowEvent{Type: apitypes.BrowserWindowEventTypeState, Browser: apitypes.BrowserStateStarting}) {
			return
		}
	}
	watcher, err := s.Browsers.Watch(ctx, slug, j.Session, id)
	if err != nil {
		write(apitypes.BrowserWindowEvent{Type: apitypes.BrowserWindowEventTypeEnd, Reason: err.Error()})
		_ = c.Close(websocket.StatusNormalClosure, "")
		return
	}
	defer s.Browsers.Unwatch(watcher)
	for {
		select {
		case <-ctx.Done():
			return
		case frame := <-watcher.Frames():
			wctx, wcancel := context.WithTimeout(ctx, browserWindowWriteTimeout)
			err := c.Write(wctx, websocket.MessageBinary, frame)
			wcancel()
			if err != nil {
				return
			}
		case ev := <-watcher.Events():
			if !write(ev) {
				return
			}
		case <-watcher.Done():
			// The end event is queued before done closes; send what
			// is left, then close cleanly.
			for {
				select {
				case ev := <-watcher.Events():
					if !write(ev) {
						return
					}
					continue
				default:
				}
				break
			}
			_ = c.Close(websocket.StatusNormalClosure, "")
			return
		}
	}
}
