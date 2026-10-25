// Package web (integration_setup.go) — the setup flow runner: every
// connection is made by a flow of steps the owner takes, which core
// runs and draws.
//
// A flow is a setupRun, kept in memory on the Server (setupRuns) under
// a random id, at most one per integration: a new start ends the one
// before. What decides each step is the flow's driver: for a manifest
// whose connection method is setup, a built-in's SetupIntegration.Setup
// or a bundle's server, asked over its pod (integration_setup_bundle.go);
// for token, device and browser, one of core's standard flows
// (integration_setup_standard.go). Core calls the driver off any
// request, one call at a time per flow, under setupCallTimeout on the
// server's clock, first with an empty Answer and then with what each
// step gave back, until it answers Done or an error:
//
//   - Form: core draws the declared fields; POST .../setup/answer hands
//     what the owner entered to the next call as Answer.Form.
//   - Go: the page sends the browser to GET /connect/start?flow=<id>,
//     which sets the flow's cookie and sends it on to the service (a
//     303, or a page whose form posts itself). The service sends it
//     back to GET /connect/callback, matched by the leg's state, and
//     the whole query reaches the next call as Answer.Return. Each leg
//     has a fresh state, so a flow may leave and come back more than
//     once.
//   - Wait: core shows the words and the page, and calls the driver
//     again with Answer.Checked every setupWaitEvery while someone
//     watches the flow (its stream, or a read of its state in the last
//     setupWatchWindow). A driver that ends its own waits (the device
//     flow's poller) is never checked.
//
// Every step is held to the flow's declarations (setupSpec): a Form
// asks only for declared fields, a Go or a Wait names a page only on a
// declared host, and Keep holds only declared keys; a step outside
// them fails the flow before anything of it is kept. Kept values (but
// those of a keep declared not secret, or a form field that is not
// sensitive), the secret values the owner entered, and the State's
// values six characters or longer are scrubbed from every word the flow
// shows, logs or audits. The owner's audit gains a row per flow event, under
// the flow's name: started, returned, done, failed with the words,
// cancelled.
//
// A flow is forgotten setupFlowTTL after its last change or its last
// Wait check (made only while someone watches it), setupFlowMaxAge after
// its start at most, and lost on a restart; deleting a connection, uninstalling the integration,
// switching it off and signing out cancel it. A flow that installs
// (it started with no connection) and fails, is cancelled or is
// forgotten before it is done keeps nothing: the integration is asked
// to undo what it made on the service, best effort (setupUndo), the
// connection its Keep made goes again (setupDiscard), and the
// integration is available again. An install started while another is
// under way ends that one and takes over the connection it made, with
// what it kept.

package web

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/url"
	"reflect"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/kivali-ai/kivali/internal/agent"
	"github.com/kivali-ai/kivali/internal/builtinintegrations"
	"github.com/kivali-ai/kivali/internal/connect"
	"github.com/kivali-ai/kivali/internal/integration"
	"github.com/kivali-ai/kivali/internal/secrets"
	"github.com/kivali-ai/kivali/internal/store"
	"github.com/kivali-ai/kivali/internal/web/apitypes"
)

const (
	// setupFlowTTL is how long a flow is kept after its last change, or
	// its last Wait check made while someone watched it: a step nobody
	// takes in that time ends with it, and a flow that ended is kept
	// that long for the page to read.
	setupFlowTTL = 10 * time.Minute
	// setupFlowMaxAge is how long a flow is kept at most from its start,
	// however it is watched: a Wait left open in a tab ends too.
	setupFlowMaxAge = 24 * time.Hour
	// setupCallTimeout bounds one call of a flow's driver.
	setupCallTimeout = 5 * time.Minute
	// setupWaitEvery is how often a Wait step is checked while someone
	// watches it.
	setupWaitEvery = 3 * time.Second
	// setupWatchWindow is how long a read of a flow's state counts as
	// someone watching it, for a page that polls instead of streaming.
	setupWatchWindow = 10 * time.Second
)

// Words of the runner's own.
const (
	errSetupTimeout = "setup did not finish its step within 5 minutes; start it again"
	errSetupNotKept = "setup made something Kivali could not keep; start it again"
	errSetupStopped = "setup was stopped before it finished; start it again"
	// errSetupNoState: a callback with no state, matched by the flow's
	// cookie alone, for a Go step that did not say its service drops
	// the state (Go.Stateless).
	errSetupNoState = "the service sent the browser back without its state; start it again"
)

// setupDriver decides a flow's steps, as SetupIntegration.Setup does:
// a setup built-in itself, a setup bundle's server (bundleHooks), or
// one of core's standard flows.
type setupDriver interface {
	Setup(ctx context.Context, conn builtinintegrations.Connection, flow string, last builtinintegrations.Answer, p builtinintegrations.Progress) (builtinintegrations.Step, error)
}

// setupBinder is a driver that is told its flow before its first call,
// for what it does outside a call (setupScrub, setupEnd).
type setupBinder interface {
	bind(run *setupRun)
}

// setupSpec is what a flow runs and what it is held to.
type setupSpec struct {
	driver setupDriver
	// fields are what a Form step may ask for, in order.
	fields integration.SetupFields
	// hosts are where a Go step may send the browser, or a Wait step's
	// page may be: host names, or host:port for an endpoint that names
	// a port.
	hosts []string
	// keeps are the keys Keep may hold.
	keeps []string
	// shown are the keys among keeps whose kept values are not scrubbed
	// from the flow's words (integration.ConnectionKeyScrubbed is false:
	// a form field that is not sensitive, a keep declared not secret).
	shown []string
	// setup is the manifest's kivali.connection.setup, for a setup
	// connection's flow; nil for a standard flow.
	setup *integration.Setup
	// keep writes a step's Keep, under lockIntegrations, unless the
	// flow was cancelled.
	// final is true for the flow's last step (Done, with no sign-in
	// after it): the same write clears the connection's unfinished mark.
	keep func(ctx context.Context, run *setupRun, values map[string]string, final bool) error
	// then is the sign-in that takes the flow on once the driver is
	// Done (kivali.connection.setup.then); nil for none.
	then *setupSpec
	// vendorWaitPages is true for the standard device flow, whose Wait
	// page is the one the manifest's own device endpoint answered with
	// (verification_uri), not one the integration's code chose: it is
	// held to https (or loopback http) but not to hosts, since a vendor
	// may put it on a host none of its endpoints is on.
	vendorWaitPages bool
}

// setupFormChecker is a driver that checks a Form's answer before the
// next call: words, answered as a 400, leave the flow on its Form so
// the owner can mend what they entered.
type setupFormChecker interface {
	checkForm(ctx context.Context, values map[string]string) string
}

// setupWaiter is a driver that ends its own Wait steps: once a Wait is
// shown, core calls waiting instead of checking it, and the driver ends
// the step with Server.setupEnd.
type setupWaiter interface {
	waiting(run *setupRun)
}

// setupWaitEnder is a setupWaiter that knows when it ends its Wait at
// the latest (the device code's expiry), for the flow's expires_at.
type setupWaitEnder interface {
	waitEnds() time.Time
}

// setupRun is one flow.
type setupRun struct {
	id, name, flow, display string
	spec                    setupSpec
	ctx                     context.Context
	cancel                  context.CancelFunc
	// runs is the table the flow is in.
	runs *setupRuns
	// calling counts the calls of runSetupCall not yet returned, with
	// what each call keeps or shows.
	calling sync.WaitGroup

	// Guarded by setupRuns.mu.
	state apitypes.SetupFlowState
	// step is the Form, Go or Wait the owner sees.
	step     builtinintegrations.Step
	progress *apitypes.SetupFlowProgress
	err      string
	// changed is the flow's last change: a step shown, a call started,
	// a stage of a call that is not a Wait's check. touched is its last
	// Wait check, made only while someone watches. The flow is kept
	// setupFlowTTL after the later of the two, and setupFlowMaxAge after
	// started at most.
	changed, touched, started time.Time
	// data is the last step's State.
	data map[string]string
	// callback is the callback address as the request that started or
	// last touched the flow reached Kivali: <scheme>://<host>/connect/callback.
	callback string
	// viaDesktop says the browser the Go step left in is the system
	// browser Kivali Desktop handed off (handoffNext), while the team's
	// window follows the flow: the callback answers that browser with a
	// page that says so rather than the flow's page. desktopReturn is
	// the app's listener that page tells, so the app comes to the
	// front, or empty when the handoff named none.
	viaDesktop    bool
	desktopReturn string
	// legs is every state the flow's calls were given in their
	// Callback, the latest last: a callback carrying any of them finds
	// the flow while it lives, since a service may keep an address an
	// earlier step gave it.
	legs []string
	// gen counts the calls and the ends: a call's answer is kept only
	// while it is the latest.
	gen int
	// calls counts the calls of the driver, Answer.Leg.
	calls int
	// checking is true while a Wait's check is in flight: the owner
	// still sees the Wait.
	checking bool
	scrub    []string
	polled   time.Time
	subs     map[chan []byte]struct{}
	// sent is the state last queued for the streams; sentKey the same
	// without its expires_at and seq, which publishLocked compares.
	// seq numbers the states, SetupFlow.Seq: one more each time the
	// state to send differs from the last.
	sent, sentKey []byte
	seq           int
	// install is true for a flow that installs: it started with no
	// connection, or with one an earlier version made that does not
	// fit (setupMisfit), or took over from one that did. madeConn is
	// the connection its Keep made, or emptied and filled again, which
	// goes again when the flow ends without being done; madeKept is
	// true for the second, whose record and access stay, emptied.
	install  bool
	madeConn string
	madeKept bool
}

// setupRuns are the flows, by id. The zero value is ready to use.
//
// What a change owes the outside (its audit rows, its state to the
// streams) is queued under mu with the change and written once mu is
// released (Server.setupUnlock): the store is never written under mu,
// and the rows reach the audit in the order their changes were made.
// A stream is sent a state only once the rows of that state are
// written, so whoever sees a state can read its audit.
type setupRuns struct {
	mu   sync.Mutex
	runs map[string]*setupRun
	// pending are audit rows not yet written; dirty, flows whose state
	// changed since it was last sent to their streams.
	pending []setupAuditRow
	dirty   map[*setupRun]struct{}
	// discard forgets the connection id an install of the integration
	// name made and that ended without being done (Server.setupDiscard),
	// off the caller's locks; with keepRecord, its record and access
	// stay, emptied. discarding holds, by integration, a channel closed
	// once its discard is over.
	discard    func(name, connID string, keepRecord bool)
	discarding map[string]chan struct{}
	// settleMu orders the writes of what is queued.
	settleMu sync.Mutex
	// calling counts every goroutine the runner and its drivers start
	// that may write: each call (runSetupCall) and the driver's own
	// function it runs, each Wait's check loop, and the device flow's
	// poller (signInSetup.waiting). setupDrain waits for them.
	calling sync.WaitGroup
}

// setupDrain cancels every flow and waits for every goroutine of the
// runner to return. A test's teardown calls it, so nothing writes
// under a directory being removed.
func (s *Server) setupDrain() {
	t := &s.setupRuns
	t.mu.Lock()
	for _, run := range t.runs {
		run.cancel()
	}
	t.mu.Unlock()
	t.calling.Wait()
}

// setupAuditRow is one flow event for the owner's audit.
type setupAuditRow struct {
	name, flow, event string
	at                time.Time
	allowed           bool
}

// setupUnlock releases setupRuns.mu and writes what the changes made
// under it owe: the audit rows, in order, then the flows' states to
// their streams. The caller holds mu.
func (s *Server) setupUnlock() {
	r := &s.setupRuns
	r.mu.Unlock()
	r.settleMu.Lock()
	defer r.settleMu.Unlock()
	r.mu.Lock()
	rows := r.pending
	r.pending = nil
	r.mu.Unlock()
	for _, row := range rows {
		s.auditAgentCall(row.name, row.at, agent.CEOSlug, row.flow, row.allowed, row.event)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	for run := range r.dirty {
		run.sendLocked()
	}
	r.dirty = nil
}

// endLocked starts forgetting the connection run made, when run
// installs and ends without being done: a flow that fails or is
// cancelled keeps nothing, and the integration is available again.
func (t *setupRuns) endLocked(run *setupRun) {
	if !run.install || run.madeConn == "" || run.state == apitypes.SetupFlowStateDone || t.discard == nil {
		return
	}
	name, id, kept := run.name, run.madeConn, run.madeKept
	run.madeConn, run.madeKept = "", false
	t.discardLocked(name, id, kept)
}

// discardLocked starts forgetting name's connection id, an install's
// that ended without being done (t.discard, off the caller's locks);
// with kept, its record and access stay, emptied. A start of name waits
// for it (setupAwaitDiscard) and is refused while it runs.
func (t *setupRuns) discardLocked(name, id string, kept bool) {
	done := make(chan struct{})
	if t.discarding == nil {
		t.discarding = map[string]chan struct{}{}
	}
	prev := t.discarding[name]
	t.discarding[name] = done
	// Counted, and run to its end whatever is cancelled meanwhile
	// (setupDrain cancels flows, not this): it writes the store after
	// the flow has ended.
	t.calling.Add(1)
	go func() {
		defer t.calling.Done()
		defer close(done)
		if prev != nil {
			<-prev
		}
		t.discard(name, id, kept)
		t.mu.Lock()
		if t.discarding[name] == done {
			delete(t.discarding, name)
		}
		t.mu.Unlock()
	}()
}

// DiscardUnfinishedSetups forgets every connection an install under
// way made before Kivali stopped: a record still marked unfinished
// (store.IntegrationConnection.SetupPending) with no flow of this
// process behind it, since flows live in memory. Each goes as the
// runner discards an install that ended without being done
// (setupDiscard: the integration asked to undo what it made on the
// service, best effort, then the Secret, the proxy credential, and the
// record and its access, or for one an install took over, the record
// emptied with its access kept), off the caller, logged. Until then the
// integration reads available, and a start of it waits. Boot and a
// restore call it; anything the owner finished carries no mark and is
// left be.
func (s *Server) DiscardUnfinishedSetups() {
	its, err := s.Store.ListIntegrations()
	if err != nil {
		log.Printf("integrations: list them to discard unfinished setups: %v", err)
		return
	}
	t := &s.setupRuns
	for _, it := range its {
		rec, err := s.Store.GetIntegrationConnection(it.Name)
		if err != nil || !rec.SetupPending {
			continue
		}
		t.mu.Lock()
		if t.discard == nil {
			t.discard = s.setupDiscard
		}
		if run := t.currentLocked(it.Name); run != nil && run.madeConn == rec.ID {
			// This process's own install under way: it keeps its connection.
			s.setupUnlock()
			continue
		}
		log.Printf("integration %s: its setup did not finish before Kivali stopped; forgetting the connection it made", it.Name)
		t.discardLocked(it.Name, rec.ID, rec.SetupTakenOver)
		s.setupUnlock()
	}
}

// setupAwaitDiscard waits, while ctx lasts, until no connection a
// failed or cancelled install of name made is still on its way out.
// The caller holds no lock.
func (s *Server) setupAwaitDiscard(ctx context.Context, name string) {
	t := &s.setupRuns
	t.mu.Lock()
	done := t.discarding[name]
	t.mu.Unlock()
	if done == nil {
		return
	}
	select {
	case <-done:
	case <-ctx.Done():
	}
}

// ended reports whether run is done or failed.
func (run *setupRun) ended() bool {
	return run.state == apitypes.SetupFlowStateDone || run.state == apitypes.SetupFlowStateFailed
}

// liveLocked reports whether the call gen of run still counts: run is
// in the table, gen is its latest call and it has not ended.
func (t *setupRuns) liveLocked(run *setupRun, gen int) bool {
	return t.runs[run.id] == run && run.gen == gen && !run.ended()
}

// currentLocked is name's flow, nil for none.
func (t *setupRuns) currentLocked(name string) *setupRun {
	for _, run := range t.runs {
		if run.name == name {
			return run
		}
	}
	return nil
}

// forgetLocked takes run out of the table, cancels it and ends its
// streams. A connection an install made goes with it, unless the
// flow was done.
func (t *setupRuns) forgetLocked(run *setupRun) {
	t.endLocked(run)
	run.cancel()
	delete(t.runs, run.id)
	for ch := range run.subs {
		close(ch)
	}
	run.subs = nil
}

// expiresLocked is when run is forgotten unless it changes, or a Wait
// check is made while someone watches it, before: setupFlowTTL after
// the later of its last change and its last such check, and never
// later than setupFlowMaxAge after it started.
func (run *setupRun) expiresLocked() time.Time {
	last := run.changed
	if run.touched.After(last) {
		last = run.touched
	}
	at := last.Add(setupFlowTTL)
	// A Wait its driver ends itself (the device flow's poller) is kept
	// until the driver ends it: when the code expires.
	if w, ok := run.spec.driver.(setupWaitEnder); ok && run.state == apitypes.SetupFlowStateWait {
		if end := w.waitEnds(); !end.IsZero() {
			at = end
		}
	}
	if ceiling := run.started.Add(setupFlowMaxAge); !run.started.IsZero() && ceiling.Before(at) {
		at = ceiling
	}
	return at
}

// pruneLocked forgets the flows whose time is up (expiresLocked). A
// Wait its driver ends itself (setupWaiter: the device flow's poller,
// which stops at the code's expiry) is left for the driver to end, so
// the page reads why, until setupFlowMaxAge.
func (t *setupRuns) pruneLocked(now time.Time) {
	for _, run := range t.runs {
		_, waits := run.spec.driver.(setupWaiter)
		switch {
		case !run.started.IsZero() && !now.Before(run.started.Add(setupFlowMaxAge)):
			t.forgetLocked(run)
		case waits && run.state == apitypes.SetupFlowStateWait:
		case !now.Before(run.expiresLocked()):
			t.forgetLocked(run)
		}
	}
}

// scrubber replaces run's secrets in words.
func (run *setupRun) scrubber() *strings.Replacer { return newScrubber(run.scrub) }

// addScrubLocked adds values to what run's words are scrubbed of.
func (run *setupRun) addScrubLocked(values ...string) {
	for _, v := range values {
		if len(v) >= scrubMinLen && !slices.Contains(run.scrub, v) {
			run.scrub = append(run.scrub, v)
		}
	}
}

// setupScrub adds values to what run's words are scrubbed of: a
// driver's own secrets (a client secret, a code, a verifier).
func (s *Server) setupScrub(run *setupRun, values ...string) {
	s.setupRuns.mu.Lock()
	defer s.setupRuns.mu.Unlock()
	run.addScrubLocked(values...)
}

// wireLocked is run on the wire, scrubbed.
func (run *setupRun) wireLocked() apitypes.SetupFlow {
	scrub := run.scrubber()
	out := apitypes.SetupFlow{ID: run.id, Flow: run.flow, State: run.state, Seq: run.seq, ExpiresAt: run.expiresLocked()}
	if run.err != "" {
		out.Error = optString(run.err)
	}
	if run.state == apitypes.SetupFlowStateRunning && run.progress != nil {
		p := *run.progress
		// Scrubbed whole, then cut, so no piece of a secret is left.
		p.Words = clipRunes(scrub.Replace(p.Words), signInWordsRunes)
		out.Progress = &p
	}
	// A step's words are scrubbed whole, then cut: a bundle's step is
	// someone else's text, and the page shows only so much. A Wait's
	// code is cut and not scrubbed: the driver chose to show it, for the
	// owner to enter on the service's page.
	words := func(w string) string { return clipRunes(scrub.Replace(w), signInWordsRunes) }
	switch st := run.step; {
	case run.state == apitypes.SetupFlowStateForm && st.Form != nil:
		step := &apitypes.SetupFlowStep{Kind: apitypes.SetupFlowStepKindForm, Words: words(st.Form.Words), Fields: []apitypes.SetupField{}}
		for _, k := range st.Form.Fields {
			if f, ok := run.spec.fields.Field(k); ok {
				step.Fields = append(step.Fields, apitypes.SetupField{
					Key: f.Key, Title: integration.SetupFieldTitle(f), Words: scrub.Replace(f.Words), Secret: f.Secret, SetupOnly: f.SetupOnly,
				})
			}
		}
		out.Step = step
	case run.state == apitypes.SetupFlowStateGo && st.Go != nil:
		out.Step = &apitypes.SetupFlowStep{
			Kind: apitypes.SetupFlowStepKindGo, Words: words(st.Go.Words), Fields: []apitypes.SetupField{},
			StartURL: optString(connectStartPath + "?flow=" + run.id), Host: optString(setupURLHost(st.Go.URL)), NoReturn: st.Go.NoReturn,
		}
	case run.state == apitypes.SetupFlowStateWait && st.Wait != nil:
		out.Step = &apitypes.SetupFlowStep{
			Kind: apitypes.SetupFlowStepKindWait, Words: words(st.Wait.Words), Fields: []apitypes.SetupField{},
			URL: optString(scrub.Replace(st.Wait.URL)), Host: optString(setupURLHost(st.Wait.URL)),
			Code: optString(clipRunes(st.Wait.Code, setupCodeRunes)),
		}
	}
	return out
}

// publishLocked queues run's state for its streams when it changed,
// numbered one past the last (SetupFlow.Seq); setupUnlock sends it. A
// state whose expires_at alone moved (a Wait checked while watched) is
// no change and is not sent: a reader learns the new expiry with the
// next change, or by reading the flow.
func (run *setupRun) publishLocked() {
	f := run.wireLocked()
	f.ExpiresAt, f.Seq = time.Time{}, 0
	key, kerr := json.Marshal(f)
	if kerr != nil || bytes.Equal(key, run.sentKey) {
		return
	}
	run.seq++
	b, err := json.Marshal(run.wireLocked())
	if err != nil {
		run.seq--
		return
	}
	run.sent, run.sentKey = b, key
	if run.runs.dirty == nil {
		run.runs.dirty = map[*setupRun]struct{}{}
	}
	run.runs.dirty[run] = struct{}{}
}

// sendLocked sends run's latest state to its streams. Each stream holds
// one state at most: a slow one gets the newest.
func (run *setupRun) sendLocked() {
	b := run.sent
	for ch := range run.subs {
		select {
		case ch <- b:
		default:
			select {
			case <-ch:
			default:
			}
			select {
			case ch <- b:
			default:
			}
		}
	}
}

// watchedLocked reports whether someone watches run: a stream, or a
// read of its state within setupWatchWindow.
func (run *setupRun) watchedLocked(now time.Time) bool {
	return len(run.subs) > 0 || now.Sub(run.polled) < setupWatchWindow
}

// newSetupFlowID is a fresh flow id: 16 random bytes in hex, as Kivali
// Desktop's connect_flow checks it.
func newSetupFlowID() string { return newSignInFlowID() }

// setupStart begins name's flow, running spec, ending (and auditing as
// cancelled) any flow name had. callback is the callback address of
// the request that started it; install is true when name has no
// connection yet, so the flow installs it. A flow that takes over from
// an install under way installs in its place, and the connection that
// one made is its own. The first call starts at once.
func (s *Server) setupStart(name, display, flow string, spec setupSpec, callback string, install bool) apitypes.SetupFlow {
	ctx, cancel := context.WithCancel(context.Background())
	now := s.clk().Now()
	run := &setupRun{
		id: newSetupFlowID(), name: name, flow: flow, display: display, spec: spec, ctx: ctx, cancel: cancel,
		changed: now, started: now, callback: callback, subs: map[chan []byte]struct{}{}, runs: &s.setupRuns, install: install,
	}
	t := &s.setupRuns
	t.mu.Lock()
	defer s.setupUnlock()
	if t.discard == nil {
		t.discard = s.setupDiscard
	}
	t.pruneLocked(now)
	var replaced *setupRun
	if old := t.currentLocked(name); old != nil {
		if !old.ended() {
			replaced = old
			if old.install {
				run.install, run.madeConn, run.madeKept, old.madeConn, old.madeKept = true, old.madeConn, old.madeKept, "", false
			}
		}
		t.forgetLocked(old)
	}
	if t.runs == nil {
		t.runs = map[string]*setupRun{}
	}
	t.runs[run.id] = run
	if b, ok := spec.driver.(setupBinder); ok {
		b.bind(run)
	}
	// Audited before the first call, whose own rows come after.
	if replaced != nil {
		s.auditSetupLocked(replaced, "cancelled", true)
	}
	s.auditSetupLocked(run, "started", true)
	s.setupCallLocked(run, builtinintegrations.Answer{}, false)
	return run.wireLocked()
}

// auditSetupLocked queues the owner's audit row of run's event, which
// setupUnlock writes.
func (s *Server) auditSetupLocked(run *setupRun, event string, allowed bool) {
	t := &s.setupRuns
	t.pending = append(t.pending, setupAuditRow{name: run.name, flow: run.flow, event: event, at: s.clk().Now(), allowed: allowed})
}

// setupCallLocked starts run's next call with ans. A check keeps the
// Wait the owner sees; any other call shows the flow running. Each call
// but a check is given a fresh state in its Callback; a check, which
// may come every few seconds for as long as a Wait lasts, is given the
// latest again.
func (s *Server) setupCallLocked(run *setupRun, ans builtinintegrations.Answer, check bool) {
	run.gen++
	gen := run.gen
	run.calls++
	ans.Leg = run.calls
	var leg string
	if check && len(run.legs) > 0 {
		leg = run.legs[len(run.legs)-1]
	} else {
		leg = newOAuthState()
		run.legs = append(run.legs, leg)
		run.addScrubLocked(leg)
	}
	if run.data != nil {
		ans.State = make(map[string]string, len(run.data))
		for k, v := range run.data {
			ans.State[k] = v
		}
	}
	if run.callback != "" {
		ans.Callback = run.callback + "?" + url.Values{"state": {leg}}.Encode()
	}
	ans.Addresses = s.setupAddresses(run.callback)
	if check {
		// A check is made only while someone watches the flow: that keeps
		// it, as a change would.
		run.checking = true
		run.touched = s.clk().Now()
	} else {
		run.state, run.step, run.progress = apitypes.SetupFlowStateRunning, builtinintegrations.Step{}, nil
		run.changed = s.clk().Now()
		run.publishLocked()
	}
	run.calling.Add(1)
	s.setupRuns.calling.Add(1)
	go s.runSetupCall(run, gen, ans)
}

// setupAddresses is every address the team signs in at, as
// Answer.Addresses gives them: callback (the bare callback the flow's
// last request reached Kivali at, "" for none) first, then the one at
// the team's external address when it has one and it is another.
func (s *Server) setupAddresses(callback string) []string {
	out := []string{}
	if callback != "" {
		out = append(out, callback)
	}
	if ext := s.externalRedirectURL(); ext != "" && ext != callback {
		out = append(out, ext)
	}
	return out
}

// runSetupCall is one call of run's driver, gen, off any request.
func (s *Server) runSetupCall(run *setupRun, gen int, ans builtinintegrations.Answer) {
	defer s.setupRuns.calling.Done()
	defer run.calling.Done()
	conn := s.setupConnection(run.ctx, run.name)
	if br, err := s.Store.ReadBranding(); err == nil {
		ans.Team = br.CompanyName
	}
	p := &setupProgress{s: s, run: run, gen: gen}
	// The driver's function runs on in its own goroutine when the call
	// is abandoned (cancelled, past its deadline), and may still write.
	s.setupRuns.calling.Add(1)
	step, err := runBuiltin(run.ctx, s.clk(), setupCallTimeout, func(ctx context.Context) (builtinintegrations.Step, error) {
		defer s.setupRuns.calling.Done()
		return run.spec.driver.Setup(ctx, conn, run.flow, ans, p)
	})
	if run.ctx.Err() != nil {
		return
	}
	if errors.Is(err, errIntegrationTimeout) {
		err = errors.New(errSetupTimeout)
	}
	s.setupApply(run, gen, step, err)
}

// setupConnection is name's connection as a driver reads it: its id
// and every value it holds, fields and Secret both, without what core
// keeps of a sign-in (connect.SecretKeys) and the proxy credential's
// secret. Empty with no connection yet; a Secret that cannot be read
// gives the fields alone.
func (s *Server) setupConnection(ctx context.Context, name string) builtinintegrations.Connection {
	conn := builtinintegrations.Connection{Fields: map[string]string{}}
	rec, err := s.Store.GetIntegrationConnection(name)
	if err != nil {
		return conn
	}
	conn.ID = rec.ID
	for k, v := range rec.Fields {
		conn.Fields[k] = v
	}
	if rec.SecretName == "" {
		return conn
	}
	data, err := s.Secrets.Get(ctx, rec.SecretName)
	if err != nil {
		if !errors.Is(err, secrets.ErrNotFound) {
			log.Printf("integration %s: read the connection's Secret for setup: %v", name, err)
		}
		return conn
	}
	for k, v := range data {
		if k == builtinintegrations.ProxySecretKey || slices.Contains(connect.SecretKeys, k) {
			continue
		}
		conn.Fields[k] = v
	}
	return conn
}

// setupApply takes the answer of run's call gen: an error fails the
// flow; a step outside the declarations fails it before anything of it
// is kept; otherwise Keep is written, State carried and the step shown.
func (s *Server) setupApply(run *setupRun, gen int, step builtinintegrations.Step, err error) {
	t := &s.setupRuns
	t.mu.Lock()
	if !t.liveLocked(run, gen) {
		s.setupUnlock()
		return
	}
	if err == nil {
		err = run.spec.check(run.display, step)
	}
	if err != nil {
		s.setupFailLocked(run, err.Error())
		s.setupUnlock()
		return
	}
	// The flow's last step: Done with no sign-in to hand over to.
	final := step.Done && run.spec.then == nil
	s.setupUnlock()
	if len(step.Keep) > 0 {
		// The last step's Keep clears the unfinished mark in the same
		// write, so no moment holds the finished connection still marked.
		if err := run.spec.keep(run.ctx, run, step.Keep, final); err != nil {
			t.mu.Lock()
			if t.liveLocked(run, gen) {
				s.setupFailLocked(run, err.Error())
			}
			s.setupUnlock()
			return
		}
	}
	if final && len(step.Keep) == 0 {
		if err := s.setupFinish(run); err != nil {
			t.mu.Lock()
			if t.liveLocked(run, gen) {
				s.setupFailLocked(run, err.Error())
			}
			s.setupUnlock()
			return
		}
	}
	t.mu.Lock()
	if !t.liveLocked(run, gen) {
		s.setupUnlock()
		return
	}
	for k, v := range step.Keep {
		if !slices.Contains(run.spec.shown, k) {
			run.addScrubLocked(v)
		}
	}
	for _, v := range step.State {
		run.addScrubLocked(v)
	}
	same := run.state == apitypes.SetupFlowStateWait && step.Wait != nil && run.step.Wait != nil &&
		*run.step.Wait == *step.Wait && reflect.DeepEqual(run.data, step.State) && len(step.Keep) == 0
	run.data = step.State
	run.checking = false
	run.progress = nil
	if !same {
		// A check that finds the same Wait is no change: a Wait nobody
		// finishes is forgotten as any other step is.
		run.changed = s.clk().Now()
	}
	run.step = builtinintegrations.Step{Form: step.Form, Go: step.Go, Wait: step.Wait}
	waiter, waits := run.spec.driver.(setupWaiter)
	startWait := false
	switch {
	case step.Done && run.spec.then != nil:
		// Setup kept the app; core's own sign-in takes the flow on, as
		// for a sign-in connection, with a State of its own.
		run.spec, run.data = *run.spec.then, nil
		if b, ok := run.spec.driver.(setupBinder); ok {
			b.bind(run)
		}
		s.setupCallLocked(run, builtinintegrations.Answer{}, false)
		s.setupUnlock()
		return
	case step.Done:
		run.state, run.step = apitypes.SetupFlowStateDone, builtinintegrations.Step{}
		run.cancel()
		s.auditSetupLocked(run, "done", true)
		// What the flow made or mended is read afresh.
		s.requirementsCache.forget(run.name)
	case step.Form != nil:
		run.state = apitypes.SetupFlowStateForm
	case step.Go != nil:
		run.state = apitypes.SetupFlowStateGo
	case step.Wait != nil:
		startWait = run.state != apitypes.SetupFlowStateWait || !waits
		run.state = apitypes.SetupFlowStateWait
	}
	run.publishLocked()
	gen = run.gen
	s.setupUnlock()
	switch {
	case startWait && waits:
		waiter.waiting(run)
	case startWait:
		s.setupRuns.calling.Add(1)
		go s.setupWaitLoop(run, gen)
	}
}

// errSetupNoStep and errSetupManySteps refuse a step that is none, or
// more than one, of Form, Go, Wait and Done.
func errSetupNoStep(display string) error {
	return fmt.Errorf("%s's setup gave no next step", display)
}

func errSetupManySteps(display string) error {
	return fmt.Errorf("%s's setup gave more than one next step", display)
}

// setupFinish marks name's connection finished as run, about to be
// done with a last step that keeps nothing, made or mended it
// (store.IntegrationConnection.SetupPending cleared; a last step that
// keeps clears it in its Keep's own write), under lockIntegrations,
// unless run was cancelled meanwhile:
// from then on it is the integration's installed connection, which a
// restart keeps.
func (s *Server) setupFinish(run *setupRun) error {
	defer s.lockIntegrations()()
	if run.ctx.Err() != nil {
		return errors.New(errSetupStopped)
	}
	rec, err := s.Store.GetIntegrationConnection(run.name)
	switch {
	case errors.Is(err, store.ErrNotFound), errors.Is(err, store.ErrIntegrationConnectionsSeveral):
		// No connection, or several an earlier Kivali left, which no
		// install made: nothing is marked.
		return nil
	case err != nil:
		log.Printf("integration %s: read the connection to finish its setup: %v", run.name, err)
		return errors.New(errSetupNotKept)
	case !rec.SetupPending:
		return nil
	}
	rec.SetupPending, rec.SetupTakenOver = false, false
	if err := s.Store.PutIntegrationConnection(run.name, rec); err != nil {
		log.Printf("integration %s: mark the connection's setup finished: %v", run.name, err)
		return errors.New(errSetupNotKept)
	}
	s.Integrations.Changed()
	return nil
}

// check is why step does not keep to spec, as an error in words for
// the owner, or nil.
func (spec setupSpec) check(display string, step builtinintegrations.Step) error {
	n := 0
	for _, set := range []bool{step.Form != nil, step.Go != nil, step.Wait != nil, step.Done} {
		if set {
			n++
		}
	}
	switch {
	case n == 0:
		return errSetupNoStep(display)
	case n > 1:
		return errSetupManySteps(display)
	}
	for _, k := range sortedFieldKeys(step.Keep) {
		if !slices.Contains(spec.keeps, k) {
			return fmt.Errorf("%s's setup tried to keep %s, which its manifest does not declare", display, k)
		}
	}
	if mapBytes(step.Keep)+mapBytes(step.State) > setupCarryMax {
		return fmt.Errorf("%s's setup gave more to keep and carry than Kivali holds for a step (%d KiB)", display, setupCarryMax>>10)
	}
	switch {
	case step.Go != nil && len(step.Go.URL) > setupURLMax, step.Wait != nil && len(step.Wait.URL) > setupURLMax:
		return fmt.Errorf("%s's setup named an address longer than %d characters", display, setupURLMax)
	case step.Go != nil && valuesBytes(step.Go.Post) > setupCarryMax:
		return fmt.Errorf("%s's setup gave a form to post larger than Kivali sends (%d KiB)", display, setupCarryMax>>10)
	}
	switch {
	case step.Form != nil:
		if len(step.Form.Fields) == 0 {
			return fmt.Errorf("%s's setup asked for a form with no fields", display)
		}
		for _, k := range step.Form.Fields {
			if _, ok := spec.fields.Field(k); !ok {
				return fmt.Errorf("%s's setup asked for %s, which its manifest does not declare", display, k)
			}
		}
	case step.Go != nil:
		if !setupURLAllowed(step.Go.URL, spec.hosts) {
			return fmt.Errorf("%s's setup tried to send your browser to %s, which its manifest does not declare", display, setupURLWords(step.Go.URL))
		}
	case step.Wait != nil && step.Wait.URL != "" && spec.vendorWaitPages:
		if !setupURLSafe(step.Wait.URL) {
			return fmt.Errorf("%s's sign-in named a page at %s, which is not an https address", display, setupURLWords(step.Wait.URL))
		}
	case step.Wait != nil:
		if step.Wait.URL != "" && !setupURLAllowed(step.Wait.URL, spec.hosts) {
			return fmt.Errorf("%s's setup named a page on %s, which its manifest does not declare", display, setupURLWords(step.Wait.URL))
		}
	}
	return nil
}

// Bounds of a step, which for a bundle is someone else's code: the
// longest address a Go or a Wait may name, the most a step may keep and
// carry in its State together, and the most a Go may post; and the
// longest Wait code the page shows.
const (
	setupURLMax    = 4096
	setupCarryMax  = 64 << 10
	setupCodeRunes = 64
)

// mapBytes is the size of m's keys and values.
func mapBytes(m map[string]string) int {
	n := 0
	for k, v := range m {
		n += len(k) + len(v)
	}
	return n
}

// valuesBytes is the size of v's keys and values.
func valuesBytes(v url.Values) int {
	n := 0
	for k, vs := range v {
		n += len(k)
		for _, x := range vs {
			n += len(x)
		}
	}
	return n
}

// setupURLAllowed reports whether a step may name raw: an address
// setupURLSafe takes whose host is among hosts. A host listed with a
// port (a sign-in endpoint's, 127.0.0.1:8443) takes that port alone; a
// bare host name takes no port, so a declared github.com does not let a
// step send the browser to github.com:8443.
func setupURLAllowed(raw string, hosts []string) bool {
	if !setupURLSafe(raw) {
		return false
	}
	u, _ := url.Parse(raw)
	if slices.Contains(hosts, strings.ToLower(u.Host)) {
		return true
	}
	return u.Port() == "" && slices.Contains(hosts, strings.ToLower(u.Hostname()))
}

// setupURLSafe reports whether raw is an address a step may send the
// browser to at all: an absolute https:// one with no user info, or
// plain http:// on loopback (a service run in-process in tests).
func setupURLSafe(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || u.User != nil {
		return false
	}
	switch u.Scheme {
	case "https":
		return true
	case "http":
		h := u.Hostname()
		return h == "localhost" || h == "127.0.0.1" || h == "::1"
	}
	return false
}

// setupURLHost is raw's host, lower-cased; "" for none.
func setupURLHost(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	return strings.ToLower(u.Host)
}

// setupURLWords names raw's host for a refusal, or says the address is
// not one.
func setupURLWords(raw string) string {
	if h := setupURLHost(raw); h != "" {
		return h
	}
	return "an address that is not one"
}

// setupFailLocked ends run failed with words, scrubbed of its secrets
// and then cut to signInWordsRunes, and audits it.
func (s *Server) setupFailLocked(run *setupRun, words string) {
	words = clipRunes(run.scrubber().Replace(words), signInWordsRunes)
	run.state, run.err = apitypes.SetupFlowStateFailed, words
	run.step, run.progress, run.checking = builtinintegrations.Step{}, nil, false
	run.gen++
	run.changed = s.clk().Now()
	run.cancel()
	run.publishLocked()
	s.auditSetupLocked(run, "failed: "+words, false)
	s.setupRuns.endLocked(run)
}

// setupEnd ends run's Wait from its driver (setupWaiter): done when err
// is nil, else failed with err's words. A flow that moved on or ended
// meanwhile is left as it is.
func (s *Server) setupEnd(run *setupRun, err error) {
	t := &s.setupRuns
	t.mu.Lock()
	gen := run.gen
	if !t.liveLocked(run, gen) || run.state != apitypes.SetupFlowStateWait {
		s.setupUnlock()
		return
	}
	s.setupUnlock()
	s.setupApply(run, gen, builtinintegrations.Step{Done: err == nil}, err)
}

// setupWaitLoop checks run's Wait, shown by call gen, every
// setupWaitEvery while someone watches it, until the flow moves on.
func (s *Server) setupWaitLoop(run *setupRun, gen int) {
	defer s.setupRuns.calling.Done()
	t := &s.setupRuns
	for {
		timer := s.clk().NewTimer(setupWaitEvery)
		select {
		case <-run.ctx.Done():
			timer.Stop()
			return
		case <-timer.C():
		}
		t.mu.Lock()
		now := s.clk().Now()
		t.pruneLocked(now)
		if !t.liveLocked(run, gen) || run.state != apitypes.SetupFlowStateWait {
			s.setupUnlock()
			return
		}
		if run.watchedLocked(now) {
			s.setupCallLocked(run, builtinintegrations.Answer{Checked: true}, true)
			s.setupUnlock()
			return
		}
		s.setupUnlock()
	}
}

// setupProgress is a call's Progress: each stage shows on the page
// while the call is the flow's latest.
type setupProgress struct {
	s   *Server
	run *setupRun
	gen int
}

func (p *setupProgress) Stage(words string, done, total int) {
	t := &p.s.setupRuns
	t.mu.Lock()
	defer p.s.setupUnlock()
	if !t.liveLocked(p.run, p.gen) {
		return
	}
	total = max(total, 0)
	done = min(max(done, 0), total)
	// Kept as given: wireLocked scrubs, then cuts.
	p.run.progress = &apitypes.SetupFlowProgress{Words: words, Done: done, Total: total}
	// A stage of a Wait's check is no change: the owner still sees the
	// Wait, and a check is kept alive only by someone watching it
	// (setupRun.touched), however much its driver reports.
	if !p.run.checking {
		p.run.changed = p.s.clk().Now()
	}
	p.run.publishLocked()
}

// setupFlowState is name's flow on the wire; ok is false with none. A read
// counts as watching it for setupWatchWindow.
func (s *Server) setupFlowState(name string) (apitypes.SetupFlow, bool) {
	t := &s.setupRuns
	t.mu.Lock()
	defer s.setupUnlock()
	now := s.clk().Now()
	t.pruneLocked(now)
	run := t.currentLocked(name)
	if run == nil {
		return apitypes.SetupFlow{}, false
	}
	run.polled = now
	return run.wireLocked(), true
}

// setupPeek is name's flow on the wire, nil for none, without counting
// as watching it; whether an install of name is under way; and whether
// a connection a failed or cancelled install made is still on its way
// out.
func (s *Server) setupPeek(name string) (flow *apitypes.SetupFlow, installing, discarding bool) {
	t := &s.setupRuns
	t.mu.Lock()
	defer s.setupUnlock()
	t.pruneLocked(s.clk().Now())
	discarding = t.discarding[name] != nil
	run := t.currentLocked(name)
	if run == nil {
		return nil, false, discarding
	}
	f := run.wireLocked()
	return &f, run.install && !run.ended(), discarding
}

// setupMade records that run's Keep made name's connection id, which
// goes again should run end without being done; kept is true for a
// record an earlier version made that the Keep emptied and filled
// again, which then stays, emptied, with its access. A run that ended
// meanwhile lets it go at once.
func (s *Server) setupMade(run *setupRun, id string, kept bool) {
	t := &s.setupRuns
	t.mu.Lock()
	defer s.setupUnlock()
	if !run.install || run.madeConn != "" {
		return
	}
	run.madeConn, run.madeKept = id, kept
	if t.runs[run.id] != run || run.state == apitypes.SetupFlowStateFailed {
		t.endLocked(run)
	}
}

// setupDiscard forgets name's connection id that an install made and
// that ended without being done, as Uninstall forgets a connection:
// first, where the integration can undo what the install made on the
// service, it is asked to (setupUndo); then its Secret (the sign-in's
// tokens revoked with the service), its scoped proxy credential, its
// record and the access set for it go. With keepRecord (the install
// took over a record an earlier version made), the record and the
// access stay, holding no value, so the integration reads available
// and agents' access returns with the next install. A connection
// already gone is left be.
func (s *Server) setupDiscard(name, id string, keepRecord bool) {
	ctx := context.Background()
	s.setupUndo(ctx, name, id)
	unlock := s.lockIntegrations()
	rec, err := s.Store.GetIntegrationConnectionByID(name, id)
	if err != nil {
		unlock()
		if !errors.Is(err, store.ErrNotFound) {
			log.Printf("integration %s: read the connection a setup made, to forget it: %v", name, err)
		}
		return
	}
	_, m, _ := s.Store.CurrentIntegrationVersion(name)
	var revokes signInRevocations
	if rec.SecretName != "" {
		tokens := s.revocableTokens(ctx, name, m, rec)
		if err := s.Secrets.Delete(ctx, rec.SecretName); err != nil && !errors.Is(err, secrets.ErrNotFound) {
			unlock()
			log.Printf("integration %s: forget the connection a setup made: delete Secret %s: %v", name, rec.SecretName, err)
			return
		}
		revokes = revokes.add(name, m, rec, tokens)
	}
	if err := s.dropBuiltinEgress(name, []store.IntegrationConnection{rec}); err != nil {
		log.Printf("integration %s: drop the connection's proxy credential: %v", name, err)
	}
	if keepRecord {
		empty := store.IntegrationConnection{ID: rec.ID, CreatedAt: rec.CreatedAt}
		if err := s.Store.PutIntegrationConnection(name, empty); err != nil {
			log.Printf("integration %s: empty the connection a setup took over: %v", name, err)
		}
	} else if _, _, err := s.Store.DeleteIntegrationConnectionByID(name, id); err != nil && !errors.Is(err, store.ErrNotFound) {
		log.Printf("integration %s: forget the connection a setup made: %v", name, err)
	}
	unlock()
	s.revokeSignIns(ctx, revokes)
	s.Integrations.ConnectionChanged(name)
	s.Integrations.Changed()
}

// setupUndo asks name's integration to undo, on the service, what an
// install that ended without being done made there, before core
// forgets the connection id it kept: a built-in's Uninstall, or a
// setup bundle's kivali/uninstall, its server waited for bundleStartWait
// at most when it is switched on and not running yet (one that still
// is not, or is switched off, is not asked, and the log says so), with
// the connection as kept so far, which may lack what later steps
// would have kept. It is best effort, under integrationCallTimeout, off
// every lock: a failure, or no answer in time, is logged, scrubbed of
// the connection's secrets, and the connection goes all the same. The
// call is counted in setupRuns.calling, so setupDrain waits for it.
func (s *Server) setupUndo(ctx context.Context, name, id string) {
	v, err := s.loadIntegration(ctx, name)
	if err != nil {
		return
	}
	var u builtinintegrations.Uninstaller
	if b, ok := s.bundleHooksOf(v); ok {
		// A setup bundle's server may be starting (switched on to install
		// it, started again by a Keep, or at boot): it is waited for as a
		// setup call waits for it.
		if !v.it.Enabled || !b.awaitServer(ctx) {
			log.Printf("integration %s: its server is not running, so what the unfinished setup made on the service is left as it is", name)
			return
		}
		u = b
	} else if bu, ok := v.builtin.(builtinintegrations.Uninstaller); ok {
		u = bu
	} else {
		return
	}
	conn, scrub, err := s.uninstallConnection(ctx, name, id)
	switch {
	case errors.Is(err, store.ErrNotFound):
		return
	case err != nil:
		log.Printf("integration %s: read the connection an unfinished setup made, to undo it on the service: %v", name, err)
		return
	}
	s.setupRuns.calling.Add(1)
	_, err = runBuiltin(ctx, s.clk(), integrationCallTimeout, func(ctx context.Context) (struct{}, error) {
		defer s.setupRuns.calling.Done()
		return struct{}{}, u.Uninstall(ctx, conn)
	})
	if err != nil {
		said := err.Error()
		if errors.Is(err, errIntegrationTimeout) {
			said = fmt.Sprintf("it did not answer within %d seconds", int(integrationCallTimeout/time.Second))
		}
		log.Printf("integration %s: undo on the service what an unfinished setup made: %s", name, clipRunes(scrub.Replace(said), signInWordsRunes))
	}
}

// setupSubscribe is name's flow's state now and a channel of its
// states to come, closed when the flow is forgotten; ok is false with
// no flow. The caller unsubscribes.
func (s *Server) setupSubscribe(name string) (run *setupRun, initial []byte, ch chan []byte, ok bool) {
	t := &s.setupRuns
	t.mu.Lock()
	defer s.setupUnlock()
	t.pruneLocked(s.clk().Now())
	run = t.currentLocked(name)
	if run == nil {
		return nil, nil, nil, false
	}
	initial, err := json.Marshal(run.wireLocked())
	if err != nil {
		return nil, nil, nil, false
	}
	ch = make(chan []byte, 1)
	run.subs[ch] = struct{}{}
	return run, initial, ch, true
}

// setupUnsubscribe ends a stream's subscription.
func (s *Server) setupUnsubscribe(run *setupRun, ch chan []byte) {
	t := &s.setupRuns
	t.mu.Lock()
	defer s.setupUnlock()
	delete(run.subs, ch)
}

// setupCancel ends and forgets name's flow, auditing it as cancelled
// when it had not ended: the connection is going, or the integration
// is switched off or removed.
func (s *Server) setupCancel(name string) bool {
	return s.setupCancelRun(name, false)
}

// setupCancelKeep is setupCancel for an owner's write to the
// connection (a save of the form, Sign out): the connection an install
// under way made is the owner's from then on, and is kept, no longer
// marked unfinished. The caller holds lockIntegrations.
func (s *Server) setupCancelKeep(name string) bool {
	ok := s.setupCancelRun(name, true)
	s.adoptConnectionLocked(name)
	return ok
}

// adoptConnectionLocked makes name's connection the owner's: one an
// install marked unfinished (SetupPending) is so no longer, and a
// restart keeps it. The caller holds lockIntegrations.
func (s *Server) adoptConnectionLocked(name string) {
	rec, err := s.Store.GetIntegrationConnection(name)
	if err != nil || !rec.SetupPending {
		return
	}
	rec.SetupPending, rec.SetupTakenOver = false, false
	if err := s.Store.PutIntegrationConnection(name, rec); err != nil {
		log.Printf("integration %s: keep the connection an install made as the owner's: %v", name, err)
	}
}

// setupAdopt makes the connection name's install under way made the
// owner's, as their own save of it does: it is kept whatever becomes of
// the flow, and is no longer marked unfinished. The caller holds
// lockIntegrations.
func (s *Server) setupAdopt(name string) {
	t := &s.setupRuns
	t.mu.Lock()
	if run := t.currentLocked(name); run != nil {
		run.madeConn = ""
	}
	s.setupUnlock()
	s.adoptConnectionLocked(name)
}

// setupCancelRun ends and forgets name's flow; with keep, the
// connection an install made is not forgotten with it.
func (s *Server) setupCancelRun(name string, keep bool) bool {
	t := &s.setupRuns
	t.mu.Lock()
	run := t.currentLocked(name)
	if run == nil {
		s.setupUnlock()
		return false
	}
	if keep {
		run.madeConn = ""
	}
	if !run.ended() {
		s.auditSetupLocked(run, "cancelled", true)
	}
	t.forgetLocked(run)
	s.setupUnlock()
	return true
}

// setupAnswerRefusal is why an answer is refused: status and words.
type setupAnswerRefusal struct {
	status int
	words  string
}

// setupAnswer hands the owner's answer to name's Form to the next
// call. callback is the answering request's callback address.
func (s *Server) setupAnswer(ctx context.Context, name string, values map[string]string, callback string) *setupAnswerRefusal {
	t := &s.setupRuns
	t.mu.Lock()
	t.pruneLocked(s.clk().Now())
	run := t.currentLocked(name)
	if run == nil {
		s.setupUnlock()
		return &setupAnswerRefusal{404, errSetupNoFlow}
	}
	if run.state != apitypes.SetupFlowStateForm || run.step.Form == nil {
		s.setupUnlock()
		return &setupAnswerRefusal{409, "the setup is not asking for a form now"}
	}
	asked := run.step.Form.Fields
	gen, spec := run.gen, run.spec
	s.setupUnlock()
	clean := map[string]string{}
	var secret []string
	for _, k := range sortedFieldKeys(values) {
		if !slices.Contains(asked, k) {
			return &setupAnswerRefusal{400, k + " is not a field this step asks for"}
		}
		v := strings.TrimSpace(values[k])
		clean[k] = v
		if f, _ := spec.fields.Field(k); f.Secret || f.SetupOnly {
			secret = append(secret, v)
		}
	}
	if c, ok := spec.driver.(setupFormChecker); ok {
		if words := c.checkForm(ctx, clean); words != "" {
			return &setupAnswerRefusal{400, words}
		}
	}
	t.mu.Lock()
	defer s.setupUnlock()
	if !t.liveLocked(run, gen) || run.state != apitypes.SetupFlowStateForm {
		return &setupAnswerRefusal{409, "the setup is not asking for a form now"}
	}
	run.addScrubLocked(secret...)
	if callback != "" {
		run.callback = callback
	}
	s.setupCallLocked(run, builtinintegrations.Answer{Form: clean}, false)
	return nil
}

// errSetupUndoing refuses a start while the connection a failed or
// cancelled install made is still being forgotten.
const errSetupUndoing = "Kivali is still undoing the last setup; try again in a moment"

// errSetupNoFlow: an integration with no flow Kivali knows of.
const errSetupNoFlow = "Kivali has no setup under way for this integration, as after a restart; start it again"

// keepSetupValues is a setup integration's Keep: values written into
// the connection's Secret beside what it holds, under
// lockIntegrations, making the connection when there is none. A flow
// cancelled meanwhile keeps nothing.
//
// An install whose connection an earlier version made, which does not
// fit this version's setup (setupMisfit), takes it over at its first
// Keep: every value the record and its Secret held goes (the scoped
// proxy credential's secret aside), the record's id, its access and
// its Secret's name stay, and the sign-in it held is revoked with the
// service once the lock is released (revokeReplacedSignIn).
func (s *Server) keepSetupValues(ctx context.Context, run *setupRun, values map[string]string, final bool) error {
	replaced, err := s.keepSetupValuesLocked(ctx, run, values, final)
	if replaced != nil {
		s.revokeReplacedSignIn(run.name, replaced.rec, replaced.data)
	}
	return err
}

// replacedConnection is a connection an install took over, as it was.
type replacedConnection struct {
	rec  store.IntegrationConnection
	data map[string]string
}

func (s *Server) keepSetupValuesLocked(ctx context.Context, run *setupRun, values map[string]string, final bool) (*replacedConnection, error) {
	defer s.lockIntegrations()()
	if run.ctx.Err() != nil {
		return nil, errors.New(errSetupStopped)
	}
	name := run.name
	rec, err := s.Store.GetIntegrationConnection(name)
	made := errors.Is(err, store.ErrNotFound)
	switch {
	case made:
		rec = store.IntegrationConnection{ID: s.newConnectionID(name), CreatedAt: s.clk().Now()}
	case err != nil:
		log.Printf("integration %s: read the connection to keep what setup made: %v", name, err)
		return nil, errors.New(errSetupNotKept)
	}
	data := map[string]string{}
	if rec.SecretName != "" {
		got, err := s.Secrets.Get(ctx, rec.SecretName)
		switch {
		case err == nil:
			data = got
		case !errors.Is(err, secrets.ErrNotFound):
			log.Printf("integration %s: read the connection's Secret to keep what setup made: %v", name, err)
			return nil, errors.New(errSetupNotKept)
		}
	}
	var replaced *replacedConnection
	if !made && run.install && setupMisfit(run.spec.setup, rec) {
		replaced = &replacedConnection{rec: rec, data: data}
		fresh := map[string]string{}
		if p := data[builtinintegrations.ProxySecretKey]; p != "" {
			fresh[builtinintegrations.ProxySecretKey] = p
		}
		data = fresh
		rec = store.IntegrationConnection{ID: rec.ID, CreatedAt: rec.CreatedAt, SecretName: rec.SecretName}
	}
	keys := slices.Clone(rec.SecretKeys)
	for k, v := range values {
		if v == "" {
			delete(data, k)
			keys = slices.DeleteFunc(keys, func(x string) bool { return x == k })
			continue
		}
		data[k] = v
		if !slices.Contains(keys, k) {
			keys = append(keys, k)
		}
	}
	slices.Sort(keys)
	if rec.SecretName == "" {
		rec.SecretName = secrets.ConnectionSecretName(name, rec.ID)
	}
	if err := s.Secrets.Put(ctx, rec.SecretName, data); err != nil {
		log.Printf("integration %s: write the connection's Secret for setup: %v", name, err)
		return nil, errors.New(errSetupNotKept)
	}
	rec.SecretKeys = keys
	// An install's connection is marked unfinished from its first Keep
	// until the flow is done (setupFinish), so one a stopped Kivali left
	// is discarded at the next start (DiscardUnfinishedSetups). An
	// install over a connection still so marked takes it over.
	adopted := !made && replaced == nil && run.install && rec.SetupPending
	takenOver := replaced != nil || (adopted && rec.SetupTakenOver)
	switch {
	case final:
		// The flow's last step: the connection is finished as this
		// write makes it.
		rec.SetupPending, rec.SetupTakenOver = false, false
	case run.install && (made || replaced != nil || adopted):
		rec.SetupPending, rec.SetupTakenOver = true, takenOver
	}
	if err := s.Store.PutIntegrationConnection(name, rec); err != nil {
		log.Printf("integration %s: write the connection for setup: %v", name, err)
		// The Secret holds the new values alone: an earlier version's
		// sign-in in it is gone either way.
		return replaced, errors.New(errSetupNotKept)
	}
	if made || replaced != nil || adopted {
		s.setupMade(run, rec.ID, takenOver)
	}
	s.Integrations.ConnectionChanged(name)
	s.Integrations.Changed()
	return replaced, nil
}

// revokeReplacedSignIn revokes the sign-in a connection an earlier
// version made held (rec, its Secret's data as it was), when an install
// takes the connection over or Uninstall removes it: off the caller's
// locks, best effort, a failure logged with nothing of the sign-in in
// it. First as core revokes any sign-in (revokeSignIn), with the
// newest superseded version's manifest when its sign-in has a
// revocation endpoint; then, for a vendor whose revocation is its own,
// through the built-in (builtinintegrations.SignInReplacer, a built-in
// hook, not part of a bundle's contract).
func (s *Server) revokeReplacedSignIn(name string, rec store.IntegrationConnection, data map[string]string) {
	client := signInClient(rec, data)
	old := builtinintegrations.SignIn{ClientID: client.ID, ClientSecret: client.Secret,
		AccessToken: data[connect.SecretKeyAccessToken], RefreshToken: data[connect.SecretKeyRefreshToken]}
	if old.AccessToken == "" && old.RefreshToken == "" {
		return
	}
	earlier := s.supersededSignIn(name)
	r, ok := s.BuiltinIntegrations.Lookup(name).(builtinintegrations.SignInReplacer)
	if earlier == nil && !ok {
		return
	}
	scrub := newScrubber([]string{old.ClientSecret, old.AccessToken, old.RefreshToken})
	s.setupRuns.calling.Add(1)
	go func() {
		defer s.setupRuns.calling.Done()
		if earlier != nil {
			s.revokeSignIn(context.Background(), name, earlier, rec, data)
		}
		if !ok {
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), revokeTimeout)
		defer cancel()
		if err := r.RevokeSignIn(ctx, old); err != nil {
			log.Printf("integration %s: revoke the sign-in of the connection setup replaced: %s", name, scrub.Replace(err.Error()))
		}
	}()
}

// supersededSignIn is the manifest of name's newest superseded version
// when its connection is made by signing in and its sign-in has a
// revocation endpoint: the version a connection that no longer fits
// was most likely made by. nil for none.
func (s *Server) supersededSignIn(name string) *integration.Manifest {
	versions, err := s.Store.IntegrationVersions(name)
	if err != nil {
		return nil
	}
	for _, v := range versions {
		if v.State != store.IntegrationVersionSuperseded {
			continue
		}
		_, m, err := s.Store.GetIntegrationVersion(name, v.Version)
		if err != nil || m == nil {
			return nil
		}
		if c := m.Kivali.Connection; c.OAuth() && c.RevocationEndpoint != "" {
			return m
		}
		return nil
	}
	return nil
}
