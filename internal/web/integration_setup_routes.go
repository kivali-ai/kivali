// Package web (integration_setup_routes.go) — the setup runner's routes:
// the page's (start, read, answer, cancel and the stream, under
// /api/v1/org/integrations/{name}/setup) and the browser's
// (GET /connect/start and GET /connect/callback, and the Desktop
// handoff's next).
//
// A Go step leaves Kivali through /connect/start?flow=<id>, which sets
// the flow's cookie in the browser that goes, signed with the session
// codec: a flow comes back only to that browser. A browser holds one
// such cookie, so a Go started in it ends the one its cookie named
// before. The service sends the browser back to /connect/callback,
// behind the session middleware, matched by the leg's state when the
// query has one, else by the cookie alone for a flow on a Go step; the
// cookie must name the flow either way. The whole query goes to the
// flow's next call as Answer.Return, and the browser returns to the
// integration's page with ?setup=<flow id> and nothing else: anyone can
// link the owner to the page with any query, and the page shows only
// what the flow's state says.

package web

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"fmt"
	"html"
	"net/http"
	"net/url"
	"strings"

	"github.com/kivali-ai/kivali/internal/builtinintegrations"
	"github.com/kivali-ai/kivali/internal/web/apitypes"
)

// handleAPISetupStart starts the integration's flow (SetupFlowStart),
// ending one under way, and answers with its state.
func (s *Server) handleAPISetupStart(w http.ResponseWriter, r *http.Request) {
	var req apitypes.SetupFlowStart
	if !decodeJSONOrError(w, r, &req) {
		return
	}
	// Under the lock a switch-off or a delete takes, so the checks hold
	// for the flow this starts.
	defer s.lockIntegrations()()
	v, ok := s.integrationOr404(w, r)
	if !ok {
		return
	}
	switch {
	case v.staleBuiltin():
		writeAPIError(w, http.StatusConflict, builtinRefusal(v.displayName(), builtinNotIncluded), whoYou)
		return
	case v.it.CurrentVersion == "":
		writeAPIError(w, http.StatusConflict, "approve a version of "+v.displayName()+" first", whoYou)
		return
	case !v.it.Enabled:
		writeAPIError(w, http.StatusConflict, "switch "+v.displayName()+" on first", whoYou)
		return
	case req.Flow != builtinintegrations.FlowSetup:
		writeAPIError(w, http.StatusConflict, req.Flow+" is not a flow "+v.displayName()+" has", whoLink)
		return
	}
	spec, werr := s.setupSpecFor(v, r)
	if werr != nil {
		writeAPIError(w, werr.status, werr.words, werr.who)
		return
	}
	writeJSON(w, http.StatusOK, s.setupStart(v.it.Name, v.displayName(), req.Flow, spec, signInRedirectURL(r)))
}

// handleAPISetupGet answers with the integration's flow; 404 with none.
func (s *Server) handleAPISetupGet(w http.ResponseWriter, r *http.Request) {
	v, ok := s.integrationOr404(w, r)
	if !ok {
		return
	}
	flow, ok := s.setupFlowState(v.it.Name)
	if !ok {
		writeAPIError(w, http.StatusNotFound, errSetupNoFlow, whoYou)
		return
	}
	writeJSON(w, http.StatusOK, flow)
}

// handleAPISetupAnswer answers the flow's Form (SetupFlowAnswer) and
// answers with its state.
func (s *Server) handleAPISetupAnswer(w http.ResponseWriter, r *http.Request) {
	var req apitypes.SetupFlowAnswer
	if !decodeJSONOrError(w, r, &req) {
		return
	}
	v, ok := s.integrationOr404(w, r)
	if !ok {
		return
	}
	if refused := s.setupAnswer(r.Context(), v.it.Name, req.Fields, signInRedirectURL(r)); refused != nil {
		writeAPIError(w, refused.status, refused.words, whoYou)
		return
	}
	flow, ok := s.setupFlowState(v.it.Name)
	if !ok {
		writeAPIError(w, http.StatusNotFound, errSetupNoFlow, whoYou)
		return
	}
	writeJSON(w, http.StatusOK, flow)
}

// handleAPISetupCancel ends the integration's flow; 204, or 404 with
// none.
func (s *Server) handleAPISetupCancel(w http.ResponseWriter, r *http.Request) {
	// Under the lock a Keep is written under, so a Keep under way lands
	// before the cancel or, seeing the flow cancelled, not at all.
	defer s.lockIntegrations()()
	v, ok := s.integrationOr404(w, r)
	if !ok {
		return
	}
	if !s.setupCancel(v.it.Name) {
		writeAPIError(w, http.StatusNotFound, errSetupNoFlow, whoYou)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// handleAPISetupStream streams the integration's flow: its state first,
// then each change, as flow events, with a heartbeat comment every
// orgStreamHeartbeat. The stream ends when the flow is forgotten; 404
// with no flow. While it is open, the flow's Wait steps are checked.
func (s *Server) handleAPISetupStream(w http.ResponseWriter, r *http.Request) {
	v, ok := s.integrationOr404(w, r)
	if !ok {
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeAPIError(w, http.StatusInternalServerError, "streaming not supported", whoServer)
		return
	}
	run, initial, ch, ok := s.setupSubscribe(v.it.Name)
	if !ok {
		writeAPIError(w, http.StatusNotFound, errSetupNoFlow, whoYou)
		return
	}
	defer s.setupUnsubscribe(run, ch)
	w.Header().Set("content-type", "text/event-stream")
	w.Header().Set("cache-control", "no-cache")
	w.Header().Set("x-accel-buffering", "no")
	_, _ = fmt.Fprintf(w, "retry: %d\n\n", orgStreamRetryMs)
	writeRawSSE(w, flusher, "flow", initial)
	hb := s.clk().NewTicker(orgStreamHeartbeat)
	defer hb.Stop()
	for {
		select {
		case b, ok := <-ch:
			if !ok {
				return
			}
			writeRawSSE(w, flusher, "flow", b)
		case <-hb.C():
			_, _ = fmt.Fprint(w, ": heartbeat\n\n")
			flusher.Flush()
		case <-r.Context().Done():
			return
		}
	}
}

// setupReturnPath is the integration name's page with ?setup=<id>.
func setupReturnPath(name, id string) string {
	return "/org/integrations/" + url.PathEscape(name) + "?" + url.Values{"setup": {id}}.Encode()
}

// setupReturnElsewhere sends the browser back when no flow is found: to
// the page of the integration the cookie names, with its flow, or to
// the integrations list with ?sign_in_error=start_again.
func setupReturnElsewhere(w http.ResponseWriter, r *http.Request, cookieID, cookieName string) {
	if cookieName != "" && cookieID != "" {
		http.Redirect(w, r, setupReturnPath(cookieName, cookieID), http.StatusSeeOther)
		return
	}
	http.Redirect(w, r, "/org/integrations?"+url.Values{"sign_in_error": {connectStartAgain}}.Encode(), http.StatusSeeOther)
}

// connectStartAgain is the one ?sign_in_error= code: there is no flow
// to read (it took too long, was used or cancelled, or Kivali
// restarted). The integrations list says so in its own words.
const connectStartAgain = "start_again"

// byLegLocked is the flow that issued state to one of its calls, nil
// for none. Every state a flow issued finds it while it lives.
func (t *setupRuns) byLegLocked(state string) *setupRun {
	for _, run := range t.runs {
		for _, leg := range run.legs {
			if subtle.ConstantTimeCompare([]byte(leg), []byte(state)) == 1 {
				return run
			}
		}
	}
	return nil
}

// replaceCookieLocked ends, as replaced, the flow on a Go step that r's
// cookie names, when it is not id: the browser is about to hold id's
// cookie instead, so that flow can no longer come back here.
func (s *Server) replaceCookieLocked(r *http.Request, id string) {
	oldID, _ := s.readConnectCookie(r)
	if oldID == "" || oldID == id {
		return
	}
	if old := s.setupRuns.runs[oldID]; old != nil && old.state == apitypes.SetupFlowStateGo {
		s.setupFailLocked(old, errSignInReplaced)
	}
}

// handleConnectStart is GET /connect/start?flow=<id>, where the page
// sends the browser for a Go step: it sets the flow's cookie for this
// leg and sends the browser on to the step's address, a 303 for a link
// or a page whose form posts the step's values there. A flow not on a
// Go step returns to its page; an id that names no flow, to the
// integrations list. It sends the browser only to an address its flow
// gave, never to one the request names. Kivali Desktop does not load
// it in the team's window; it opens the handoff with next=<id> in the
// system browser, which comes here signed in (handoffNext).
func (s *Server) handleConnectStart(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Referrer-Policy", "no-referrer")
	id := r.URL.Query().Get("flow")
	t := &s.setupRuns
	t.mu.Lock()
	t.pruneLocked(s.clk().Now())
	run := t.runs[id]
	if run == nil || id == "" {
		s.setupUnlock()
		setupReturnElsewhere(w, r, "", "")
		return
	}
	if run.state != apitypes.SetupFlowStateGo || run.step.Go == nil {
		s.setupUnlock()
		http.Redirect(w, r, setupReturnPath(run.name, run.id), http.StatusSeeOther)
		return
	}
	step := *run.step.Go
	callback := run.callback
	if step.NoReturn {
		// Nothing comes back, so no cookie is needed: the flow goes on
		// at once, as the browser leaves.
		s.setupCallLocked(run, builtinintegrations.Answer{Went: true}, false)
	} else {
		s.replaceCookieLocked(r, id)
	}
	s.setupUnlock()
	if !step.NoReturn {
		http.SetCookie(w, s.connectCookie(run.id, run.name, callback))
	}
	if step.Post == nil {
		http.Redirect(w, r, step.URL, http.StatusSeeOther)
		return
	}
	writeAutoPost(w, step.URL, step.Post)
}

// autoPostScript submits the start page's form as it loads.
const autoPostScript = `document.getElementById("go").submit();`

// autoPostScriptHash is autoPostScript's CSP hash: the one script the
// page may run.
var autoPostScriptHash = func() string {
	sum := sha256.Sum256([]byte(autoPostScript))
	return "'sha256-" + base64.StdEncoding.EncodeToString(sum[:]) + "'"
}()

// writeAutoPost answers with a page whose form posts values to target
// and submits itself on load, with a visible button for a browser that
// runs no scripts. It loads nothing, sends no referrer, and may post
// only to target's origin.
func writeAutoPost(w http.ResponseWriter, target string, values url.Values) {
	u, _ := url.Parse(target)
	origin := u.Scheme + "://" + u.Host
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	// form-action names the step's origin alone. Chrome holds the
	// redirects that follow the POST to it as well, so a service that
	// answers the post with a redirect to another origin is stopped by
	// the browser there, on this page's policy.
	w.Header().Set("Content-Security-Policy", "default-src 'none'; script-src "+autoPostScriptHash+"; form-action "+origin+"; base-uri 'none'; frame-ancestors 'none'")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	var b strings.Builder
	b.WriteString(`<!doctype html><html lang="en"><head><meta charset="utf-8"><meta name="referrer" content="no-referrer"><title>Kivali</title></head><body>`)
	b.WriteString(`<form id="go" method="post" action="` + html.EscapeString(target) + `">`)
	for _, k := range sortedValueKeys(values) {
		for _, v := range values[k] {
			b.WriteString(`<input type="hidden" name="` + html.EscapeString(k) + `" value="` + html.EscapeString(v) + `">`)
		}
	}
	b.WriteString(`<p>Kivali is sending you to ` + html.EscapeString(u.Host) + `.</p><button type="submit">Continue</button></form>`)
	b.WriteString(`<script>` + autoPostScript + `</script></body></html>`)
	_, _ = w.Write([]byte(b.String()))
}

// sortedValueKeys is values' keys, sorted.
func sortedValueKeys(values url.Values) []string {
	m := make(map[string]string, len(values))
	for k := range values {
		m[k] = ""
	}
	return sortedFieldKeys(m)
}

// handoffNext is the Desktop handoff's next (auth.Handoff.WithNext):
// once a handoff token has passed, a next that names a flow on a Go
// step whose callback address is the one this request reached Kivali
// at sets that flow's cookie in this browser and sends it to
// /connect/start?flow=<id>, which sends it on to the service, by a
// link or a form that posts itself. The callback then comes to this
// browser, not to the Desktop window that started the flow, and passes
// the cookie check. Any other next answers "", and the handoff is
// refused: no session, a 302 to "/". A handoff token is minted only by
// the team's supervisor and signs the browser in as the owner, so the
// cookie it hands over is no more than the owner already holds.
func (s *Server) handoffNext(w http.ResponseWriter, r *http.Request, next string) string {
	t := &s.setupRuns
	t.mu.Lock()
	t.pruneLocked(s.clk().Now())
	run := t.runs[next]
	// The cookie is host-only and the service sends the browser back to
	// the flow's callback address: a handoff that reached Kivali at
	// another address (localhost for 127.0.0.1, another port) would set
	// it where the callback never looks.
	if next == "" || run == nil || run.state != apitypes.SetupFlowStateGo || signInRedirectURL(r) != run.callback {
		s.setupUnlock()
		return ""
	}
	noReturn := run.step.Go != nil && run.step.Go.NoReturn
	if !noReturn {
		s.replaceCookieLocked(r, run.id)
	}
	name, callback := run.name, run.callback
	s.setupUnlock()
	if !noReturn {
		http.SetCookie(w, s.connectCookie(next, name, callback))
	}
	return connectStartPath + "?" + url.Values{"flow": {next}}.Encode()
}

// handleConnectCallback is GET /connect/callback, where a Go step's
// service sends the owner's browser back. It is a GET that changes
// things, as OAuth has it. What makes it safe is that it runs behind
// the session middleware, that each leg's state is random and good for
// one callback, that the cookie set as the browser left must name the
// flow, and that the flow's driver checks what the query says against
// the service before relying on it. Nothing of the query is logged.
//
// It answers with a 303 to the flow's page with ?setup=<flow id>,
// where the page reads how the flow stands. A callback whose cookie
// names another flow ends this one, as started elsewhere. One that
// finds no flow returns to the page of the integration its cookie
// names, or to the integrations list.
func (s *Server) handleConnectCallback(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Referrer-Policy", "no-referrer")
	q := r.URL.Query()
	cookieID, cookieName := s.readConnectCookie(r)
	t := &s.setupRuns
	t.mu.Lock()
	t.pruneLocked(s.clk().Now())
	var run *setupRun
	state := q.Get("state")
	if state != "" {
		run = t.byLegLocked(state)
	} else if c := t.runs[cookieID]; cookieID != "" && c != nil && c.state == apitypes.SetupFlowStateGo {
		run = c
	}
	if run == nil {
		s.setupUnlock()
		setupReturnElsewhere(w, r, cookieID, cookieName)
		return
	}
	back := setupReturnPath(run.name, run.id)
	switch {
	case run.state != apitypes.SetupFlowStateGo || run.step.Go == nil || run.step.Go.NoReturn:
		// Not waiting for the browser: it ended before its callback came
		// (another flow's Go was started in this browser), or the
		// callback is a replay. Its page reads how it stands.
		s.setupUnlock()
		http.Redirect(w, r, back, http.StatusSeeOther)
		return
	case state == "" && !run.step.Go.Stateless:
		// Matched by the cookie alone, which goes with any top-level GET
		// a page the owner visits makes: only a step that says its
		// service drops the state takes such a return. Nothing runs.
		s.setupFailLocked(run, errSetupNoState)
		s.setupUnlock()
		http.Redirect(w, r, back, http.StatusSeeOther)
		return
	case cookieID != run.id:
		s.setupFailLocked(run, errSignInStartAgain)
		s.setupUnlock()
		http.Redirect(w, r, back, http.StatusSeeOther)
		return
	}
	if cb := signInRedirectURL(r); cb != "" {
		run.callback = cb
	}
	// A driver that echoes what came back never shows it: every value
	// but the service's own words of a refusal (OAuth's error, its
	// description and its page), which are for the owner to read.
	for k, vs := range q {
		if k != "error" && k != "error_description" && k != "error_uri" {
			run.addScrubLocked(vs...)
		}
	}
	s.auditSetupLocked(run, "returned", true)
	s.setupCallLocked(run, builtinintegrations.Answer{Return: q}, false)
	callback := run.callback
	s.setupUnlock()
	// The cookie was for this flow, and goes with this leg. A cookie
	// that names another stays: that flow may still come back here.
	gone := s.connectCookie("", "", callback)
	gone.Value, gone.MaxAge = "", -1
	http.SetCookie(w, gone)
	http.Redirect(w, r, back, http.StatusSeeOther)
}
