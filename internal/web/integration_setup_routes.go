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
	"log"
	"net/http"
	"net/url"
	"strings"

	"github.com/kivali-ai/kivali/internal/auth"
	"github.com/kivali-ai/kivali/internal/builtinintegrations"
	"github.com/kivali-ai/kivali/internal/web/apitypes"
)

// handleAPISetupStart starts the integration's flow (SetupFlowStart),
// ending one under way, and answers with its state: setup, which
// installs it (switching it on when nothing is installed yet) or makes
// its connection again; sign_in, the standard sign-in again; or a flow
// one of its own requirements names as its Fix.
func (s *Server) handleAPISetupStart(w http.ResponseWriter, r *http.Request) {
	var req apitypes.SetupFlowStart
	if !decodeJSONOrError(w, r, &req) {
		return
	}
	flow := req.Flow
	// A connection a failed install made goes before another starts,
	// so the new one never reads it.
	s.setupAwaitDiscard(r.Context(), r.PathValue("name"))
	// A Fix the integration names is asked of it before the lock, so a
	// slow integration holds up no other integration write.
	named := false
	if flow != builtinintegrations.FlowSetup && flow != flowSignIn {
		v, ok := s.integrationOr404(w, r)
		if !ok {
			return
		}
		named = s.fixNamed(r.Context(), v, flow)
	}
	// Under the lock a switch-off or a delete takes, so the checks hold
	// for the flow this starts.
	defer s.lockIntegrations()()
	v, ok := s.integrationOr404(w, r)
	if !ok {
		return
	}
	display := v.displayName()
	// What a flow other than installing works on: the connection. One an
	// earlier version made that does not fit this version's setup, and
	// one an install left unfinished, are installed over (keepSetupValues
	// takes them over).
	installed := v.conn != nil && !v.connectionMisfit() && !v.conn.rec.SetupPending
	switch {
	case v.discarding:
		// A failed install's connection started going after the wait
		// above: a flow started now would keep into the record that
		// discard is about to delete.
		writeAPIError(w, http.StatusConflict, errSetupUndoing, whoYou)
		return
	case v.staleBuiltin():
		writeAPIError(w, http.StatusConflict, builtinRefusal(display, builtinNotIncluded), whoYou)
		return
	case v.it.CurrentVersion == "":
		writeAPIError(w, http.StatusConflict, "approve a version of "+display+" first", whoYou)
		return
	case flow == flowSignIn && !v.signIn():
		writeAPIError(w, http.StatusConflict, display+" is not connected by signing in", whoYou)
		return
	case flow != builtinintegrations.FlowSetup && flow != flowSignIn && !named && installed && v.it.Enabled && !v.running && s.isSetupBundle(v):
		writeAPIError(w, http.StatusConflict, errBundleNotRunning(display), whoBundle)
		return
	case flow != builtinintegrations.FlowSetup && flow != flowSignIn && !named:
		writeAPIError(w, http.StatusConflict, errFlowNotOffered(flow, display), whoLink)
		return
	case flow != builtinintegrations.FlowSetup && !installed:
		writeAPIError(w, http.StatusConflict, "install "+display+" first", whoYou)
		return
	case !v.it.Enabled && installed:
		writeAPIError(w, http.StatusConflict, "switch "+display+" on first", whoYou)
		return
	}
	var spec setupSpec
	var werr *connWriteError
	switch flow {
	case builtinintegrations.FlowSetup:
		spec, werr = s.setupSpecFor(v, r)
	case flowSignIn:
		spec, werr = s.signInSpecFor(v, r)
	default:
		spec, werr = s.fixSpecFor(v, flow)
	}
	if werr != nil {
		writeAPIError(w, werr.status, werr.words, werr.who)
		return
	}
	// Installing what is not installed switches it on: an integration
	// with no connection has nothing to keep off.
	if !v.it.Enabled {
		if err := s.Store.SetIntegrationEnabled(v.it.Name, true); err != nil {
			log.Printf("integration %s: switch it on to install it: %v", v.it.Name, err)
			writeAPIError(w, http.StatusInternalServerError, "the integration's switch could not be saved", whoServer)
			return
		}
		s.Integrations.Changed()
	}
	writeJSON(w, http.StatusOK, s.setupStart(v.it.Name, display, flow, spec, signInRedirectURL(r), !installed))
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

// secretReturnKey reports whether a callback's query key names a value
// the flow's words are scrubbed of: code or state, or a key whose name
// holds token, secret, password or key, ignoring case.
func secretReturnKey(k string) bool {
	k = strings.ToLower(k)
	if k == "code" || k == "state" {
		return true
	}
	for _, w := range []string{"token", "secret", "password", "key"} {
		if strings.Contains(k, w) {
			return true
		}
	}
	return false
}

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
	// it where the callback never looks. Why the browser is turned away
	// is logged: the flow's id is not a secret, and the addresses are
	// Kivali's own.
	switch reached := signInRedirectURL(r); {
	case next == "" || run == nil:
		s.setupUnlock()
		log.Printf("auth handoff: next names no setup flow (it ended, ran out or Kivali restarted)")
		return ""
	case run.state != apitypes.SetupFlowStateGo:
		s.setupUnlock()
		log.Printf("auth handoff: next names a setup flow of %s that is not waiting for a browser (%s)", run.name, run.state)
		return ""
	case reached != run.callback:
		s.setupUnlock()
		log.Printf("auth handoff: the browser reached Kivali at %s while %s's setup flow was started at %s", setupURLWords(reached), run.name, setupURLWords(run.callback))
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

// handoffRefused is the Desktop handoff's refusal (auth.HandoffRefused):
// the system browser came with a token the server refused, so it never
// left for the service, and nothing but the handoff's log line would
// say so; the owner would see the flow wait on its Go step until it
// ran out, and the browser the home page. When next names a flow
// waiting on a Go step, the flow ends with the refusal's words (the
// team's clock stands hours from this computer's, the link was opened
// too late), which its page in the team's window shows with Try again,
// and the browser is sent to that page. A replayed token, the browser
// loading the handoff again after it passed, ends nothing: the flow is
// that browser's, under way at the service; the browser goes to the
// flow's page. Nothing is signed in and no cookie set either way. next
// comes from a request whose token failed: it can end only a flow it
// names by its 32 random hex digits, and only while that flow waits
// for a browser.
func (s *Server) handoffRefused(_ *http.Request, next string, why auth.HandoffRefusal) string {
	t := &s.setupRuns
	t.mu.Lock()
	t.pruneLocked(s.clk().Now())
	run := t.runs[next]
	if run == nil || run.state != apitypes.SetupFlowStateGo {
		s.setupUnlock()
		return ""
	}
	name, id := run.name, run.id
	if why.Cause != auth.HandoffRefusedReplayed {
		s.setupFailLocked(run, errHandoffRefused(why.Words()))
	}
	s.setupUnlock()
	return setupReturnPath(name, id)
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
	// A driver that echoes a code, a state or a key that came back never
	// shows it. Other values (setup_action=install) are ordinary words.
	for k, vs := range q {
		if secretReturnKey(k) {
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
