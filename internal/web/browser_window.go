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
	"net/url"
	"strings"
	"sync/atomic"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/coder/websocket"

	"github.com/kivali-ai/kivali/internal/agentpod"
	"github.com/kivali-ai/kivali/internal/web/apitypes"
)

// browserWindowWriteTimeout bounds one write to the page.
const browserWindowWriteTimeout = 5 * time.Second

// browserWindowReadLimit bounds one message from the page: an input
// event, at most a pasted text (itself bounded by browserInputTextMax,
// which Input refuses beyond with a notice).
const browserWindowReadLimit = 1 << 20

// windowAcceptOptions are the WebSocket options for a browser window.
// The library accepts a page whose Origin host is the request's Host;
// a proxy that rewrites Host is covered by the origin it forwards
// (requestScheme and requestHost, as the API's same-origin check reads
// them) and by the deployment's configured external origin. A pattern
// with a scheme is matched against the whole origin.
func (s *Server) windowAcceptOptions(r *http.Request) *websocket.AcceptOptions {
	opts := &websocket.AcceptOptions{CompressionMode: websocket.CompressionDisabled}
	if h := requestHost(r); h != "" && !strings.EqualFold(h, r.Host) {
		opts.OriginPatterns = append(opts.OriginPatterns, requestScheme(r)+"://"+h)
	}
	if u, err := url.Parse(s.ExternalURL); err == nil && u.Scheme != "" && u.Host != "" {
		opts.OriginPatterns = append(opts.OriginPatterns, u.Scheme+"://"+u.Host)
	}
	return opts
}

// sentence is err's text as a line for the person at the window: a
// capital to start, a full stop to end.
func sentence(err error) string {
	s := strings.TrimSpace(err.Error())
	if s == "" {
		return ""
	}
	r, size := utf8.DecodeRuneInString(s)
	s = string(unicode.ToUpper(r)) + s[size:]
	if !strings.HasSuffix(s, ".") {
		s += "."
	}
	return s
}

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
	c, err := websocket.Accept(w, r, s.windowAcceptOptions(r))
	if err != nil {
		return
	}
	defer func() { _ = c.CloseNow() }()
	c.SetReadLimit(browserWindowReadLimit)
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()

	// write sends one event to the page. Writes come from this
	// goroutine and the reader's; the connection serialises them.
	write := func(ev apitypes.BrowserWindowEvent) bool {
		body, err := json.Marshal(ev)
		if err != nil {
			return false
		}
		wctx, wcancel := context.WithTimeout(ctx, browserWindowWriteTimeout)
		defer wcancel()
		return c.Write(wctx, websocket.MessageText, body) == nil
	}
	// The page's messages: input for the shown tab, taken only while
	// the task waits for a person; reading is also how the socket's
	// close is seen. The watcher exists only once the browser is up. An
	// input the browser could not take is reported to the page as a
	// notice, so it can say why nothing happened.
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
			if err := s.Browsers.Input(w, in.Input); err != nil {
				write(apitypes.BrowserWindowEvent{Type: apitypes.BrowserWindowEventTypeNotice, Reason: sentence(err)})
			}
		}
	}()
	end := func(reason string) {
		write(apitypes.BrowserWindowEvent{Type: apitypes.BrowserWindowEventTypeEnd, Reason: reason})
		_ = c.Close(websocket.StatusNormalClosure, "")
	}
	switch {
	case j.State == subagentJobQueued && !j.cancelled:
		end("The task has not started yet.")
		return
	case (j.State != subagentJobRunning && j.State != subagentJobWaiting) || j.cancelled || j.endReason != "":
		end("The task has ended.")
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
		end(sentence(err))
		return
	}
	defer s.Browsers.Unwatch(wt)
	watcher.Store(wt)
	// Watch may have waited minutes for the browser; a wait for a
	// person that began meanwhile was broadcast only to the windows
	// then attached. The window hears the current answer.
	if now := s.SubagentService.windowTakesInput(id); now != input {
		wt.event(apitypes.BrowserWindowEvent{Type: apitypes.BrowserWindowEventTypeState, Browser: apitypes.BrowserStateRunning, Input: now, Site: j.Site})
	}
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
