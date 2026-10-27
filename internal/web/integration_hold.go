// Package web (integration_hold.go) — a built-in's call that waits for
// the owner's approval: a held call.
//
// A built-in's Call answers with Result.Hold for an action that must
// not happen until the owner says so. Core keeps the call in memory
// under an id nobody can guess (heldCalls), writes its audit row
// ("waiting for the owner's approval: <title>") and answers the agent
// at once, not as an error, that it waits and that the outcome comes to
// its chat; the agent's turn carries on. Home lists an integration_hold
// need: the title, each field as the built-in captured it, who it
// reaches, and the taint line (heldCallTaint). A hold the owner could
// not read as Run sends it is refused: one carrying the connection's
// secret, which the owner would read scrubbed, or a Unicode format
// character other than the zero-width joiner, which shows as nothing.
// At most
// maxHeldCallsPerAgent of an agent's calls wait at once; another is
// refused before it is held.
//
// Approve works the integration out again exactly as a call does (the
// agent still on the team, agentIntegrationFor, the hold's Switch still
// on in the agent's access, the agent's listed actions, the
// connection). A refusal there is the owner's answer,
// audited as not run, and the call keeps waiting. Otherwise core runs
// Hold.Run once, with the connection read then, under a call's
// deadline, writes the row of how it ended ("approved: ...") and tells
// the agent in its chat, a held_call message. Deny, heldCallLimit with
// no answer, and the integration disabled, disconnected or uninstalled
// or the agent offboarded end it unrun, with a row and, while the agent
// is on the team, a message; so does a hold made as one of those
// happens. Each held call is answered once; a later answer is a 409. A
// restart of core drops every held call with nothing written:
// the agent was told that no word within a day means it was not done.

package web

import (
	"cmp"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/kivali-ai/kivali/internal/agent"
	"github.com/kivali-ai/kivali/internal/agentpod"
	"github.com/kivali-ai/kivali/internal/builtinintegrations"
	"github.com/kivali-ai/kivali/internal/clock"
	"github.com/kivali-ai/kivali/internal/store"
	"github.com/kivali-ai/kivali/internal/web/apitypes"
)

const (
	// heldCallLimit is how long a held call waits for the owner before
	// it ends unrun.
	heldCallLimit = 24 * time.Hour
	// maxHeldCallsPerAgent bounds an agent's held calls waiting at once.
	maxHeldCallsPerAgent = 5
	// heldCallTaintRows is how much of the integration's audit the
	// taint line reads, newest first, and heldCallTaintWindow how far
	// back.
	heldCallTaintRows   = 500
	heldCallTaintWindow = 24 * time.Hour
	// maxHeldCallNote bounds the note the owner sends with an answer;
	// the agent reads it.
	maxHeldCallNote = 2 << 10
	// A held call's fields are at most maxHeldCallFields, and with
	// their labels at most maxHeldCallBytes: what the owner reads
	// whole before approving.
	maxHeldCallFields = 64
	maxHeldCallBytes  = 256 << 10
	// heldCallTitleMax and heldCallRecipientMax bound the title and
	// each recipient, which are one line each.
	heldCallTitleMax     = 300
	heldCallRecipientMax = 320
	// heldCallAnswerMax bounds what the agent's message keeps of Run's
	// answer, and heldCallOwnerWordsMax what the owner's reply quotes.
	heldCallAnswerMax     = 16 << 10
	heldCallOwnerWordsMax = 300
)

// integrationHoldNeedPrefix is the ID prefix of a held call's row on
// Needs you, which POST /api/v1/needs/approve and /deny take as path.
const integrationHoldNeedPrefix = "integration_hold:"

// heldCallWaiting is the agent's answer to a call core holds.
const heldCallWaiting = "Waiting for the owner's approval. You will hear in your chat whether it was done. If you hear nothing within a day, it was not done."

// reasonTooManyHeld is the audit's reason for a call refused because
// the agent already has maxHeldCallsPerAgent waiting.
const reasonTooManyHeld = "the owner has five of the agent's requests waiting"

func refuseTooManyHeld() string {
	return "The owner has five of your requests waiting for approval. Wait for their answers in your chat before asking for another."
}

// reasonHeldCallSecret is the audit's reason for a hold whose words
// carry the connection's secret: the owner would read "[secret]" where
// Run sends the secret itself, so it is never held.
const reasonHeldCallSecret = "the call carries the connection's secret"

func refuseHeldCallSecret(display string) string {
	return "This request carries the " + display + " integration's secret, so it was not held for the owner's approval."
}

// reasonHeldCallFormat is the audit's reason for a hold whose words
// hold a Unicode format character (category Cf: zero-width spaces and
// non-joiners, direction marks and overrides, the byte order mark),
// which the owner would not see and Run would send. The zero-width
// joiner is allowed (hasFormatRune).
const reasonHeldCallFormat = "the call holds invisible formatting characters"

func refuseHeldCallFormat() string {
	return "This request holds invisible formatting characters (Unicode format characters, such as zero-width spaces or direction marks), which the owner cannot see before approving. Remove them and ask again."
}

// errHeldCallGone is an answer to a held call that is no longer
// waiting.
var errHeldCallGone = &ceoRefusal{
	Status: http.StatusConflict,
	Msg:    "integration_hold is not waiting",
	Words:  "That request is no longer waiting for an answer",
	Who:    whoNoOne,
}

// integrationHold is one call waiting for the owner. Everything but stopped
// and the end's words is set when it is held and never changes.
type integrationHold struct {
	id          string
	agent       string
	integration string
	display     string
	tool        string
	title       string
	fields      []builtinintegrations.HoldField
	recipients  []string
	// needs is the hold's Switch: the access form's switch that must
	// still be on at approval, "" for none.
	needs string
	taint string
	at    time.Time
	run   func(context.Context, builtinintegrations.Connection) (*builtinintegrations.Result, error)

	// answers takes the owner's answers, one at a time, to the
	// goroutine that waits for them (awaitHeldCall); stop, closed, ends
	// the call unrun; ended is closed once the call's end is written
	// and the agent told.
	answers chan heldCallAnswer
	stop    chan struct{}
	ended   chan struct{}

	// stopped, and why: the audit's reason and the agent's words.
	// Under heldCalls.mu.
	stopped               bool
	stopReason, stopWords string
}

// heldCallAnswer is the owner's answer to a held call, and where its
// outcome goes.
type heldCallAnswer struct {
	approved bool
	note     string
	reply    chan heldCallReply
}

type heldCallReply struct {
	out apitypes.NeedActionResponse
	err error
}

// heldCalls is every held call waiting, by id. The zero value is
// ready.
type heldCalls struct {
	mu   sync.Mutex
	byID map[string]*integrationHold
	// last is the date of the last outcome message written, so two
	// outcomes in one instant are two files.
	last time.Time
	// added, when set, is given each held call as add registers it. A
	// test seam, for a call that may end before the test could list it.
	added func(*integrationHold)
}

// add registers h unless its agent already has maxHeldCallsPerAgent
// waiting, which is false.
func (hc *heldCalls) add(h *integrationHold) bool {
	hc.mu.Lock()
	defer hc.mu.Unlock()
	n := 0
	for _, o := range hc.byID {
		if o.agent == h.agent {
			n++
		}
	}
	if n >= maxHeldCallsPerAgent {
		return false
	}
	if hc.byID == nil {
		hc.byID = map[string]*integrationHold{}
	}
	hc.byID[h.id] = h
	if hc.added != nil {
		hc.added(h)
	}
	return true
}

func (hc *heldCalls) remove(id string) {
	hc.mu.Lock()
	delete(hc.byID, id)
	hc.mu.Unlock()
}

// get is the held call id, nil for none.
func (hc *heldCalls) get(id string) *integrationHold {
	hc.mu.Lock()
	defer hc.mu.Unlock()
	return hc.byID[id]
}

// list is every held call waiting, oldest first.
func (hc *heldCalls) list() []*integrationHold {
	hc.mu.Lock()
	out := make([]*integrationHold, 0, len(hc.byID))
	for _, h := range hc.byID {
		out = append(out, h)
	}
	hc.mu.Unlock()
	sort.Slice(out, func(a, b int) bool {
		if out[a].at.Equal(out[b].at) {
			return out[a].id < out[b].id
		}
		return out[a].at.Before(out[b].at)
	})
	return out
}

// needIDs are the IDs of every held call's row on Needs you.
func (hc *heldCalls) needIDs() []string {
	var out []string
	for _, h := range hc.list() {
		out = append(out, integrationHoldNeedPrefix+h.id)
	}
	return out
}

// stopWhere ends, unrun, every held call match picks: reason is the
// audit's, words what the agent is told. It does not wait for them.
func (hc *heldCalls) stopWhere(match func(*integrationHold) bool, reason, words string) {
	hc.mu.Lock()
	defer hc.mu.Unlock()
	for _, h := range hc.byID {
		if !h.stopped && match(h) {
			h.stopped, h.stopReason, h.stopWords = true, reason, words
			close(h.stop)
		}
	}
}

// stoppedWhy is why h was stopped.
func (hc *heldCalls) stoppedWhy(h *integrationHold) (reason, words string) {
	hc.mu.Lock()
	defer hc.mu.Unlock()
	return h.stopReason, h.stopWords
}

// date is the date of an outcome message written at now: later than
// the one before it.
func (hc *heldCalls) date(now time.Time) time.Time {
	hc.mu.Lock()
	defer hc.mu.Unlock()
	now = now.UTC()
	if !now.After(hc.last) {
		now = hc.last.Add(time.Nanosecond)
	}
	hc.last = now
	return now
}

// newHeldCallID is 128 random bits in hex: the row's ID is the only
// thing an answer names, so it must not be guessable.
func newHeldCallID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}

// oneLine is s with its runs of white space, line breaks included, as
// one space, at most n bytes.
func oneLine(s string, n int) string {
	return clipText(strings.Join(strings.Fields(s), " "), n)
}

// holdAgentCall holds slug's call of tool on v, whose built-in
// answered with hold, and answers the agent that it waits. What the
// owner reads is what Run does: a hold whose words carry the
// connection's secret (scrub, the connection's scrubber, would change
// them) or a Unicode format character is refused, never shown altered.
// A hold with nothing to run is the integration's failure; one too
// large for the owner to read, or past the agent's limit, is refused.
// A hold made as its integration is turned off or its agent leaves
// ends at once, as theirs do (heldCallGone).
func (s *Server) holdAgentCall(start time.Time, slug string, v agentIntegration, tool string, hold *builtinintegrations.Hold, scrub *strings.Replacer) agentpod.IntegrationCallResponse {
	title := oneLine(hold.Title, heldCallTitleMax)
	if hold.Run == nil || title == "" {
		s.auditAgentCall(v.name, start, slug, tool, false, "the integration held a call with nothing to run")
		return integrationError(fmt.Sprintf("The %s integration did not finish the call; tell the owner.", v.display))
	}
	words := heldCallWords(hold)
	if slices.ContainsFunc(words, func(w string) bool { return scrub.Replace(w) != w }) {
		s.auditAgentCall(v.name, start, slug, tool, false, reasonHeldCallSecret)
		return integrationError(refuseHeldCallSecret(v.display))
	}
	if slices.ContainsFunc(words, hasFormatRune) {
		s.auditAgentCall(v.name, start, slug, tool, false, reasonHeldCallFormat)
		return integrationError(refuseHeldCallFormat())
	}
	size := 0
	fields := make([]builtinintegrations.HoldField, 0, len(hold.Fields))
	for _, f := range hold.Fields {
		size += len(f.Label) + len(f.Value)
		fields = append(fields, builtinintegrations.HoldField{Label: oneLine(f.Label, heldCallTitleMax), Value: f.Value})
	}
	if len(fields) > maxHeldCallFields || size > maxHeldCallBytes {
		s.auditAgentCall(v.name, start, slug, tool, false, "the call is too large for the owner to read")
		return integrationError(fmt.Sprintf("This call is too large for the owner to read before approving it (at most %d fields and %d KB). Make it shorter.", maxHeldCallFields, maxHeldCallBytes>>10))
	}
	recipients := []string{}
	for _, r := range hold.Recipients {
		if r = oneLine(r, heldCallRecipientMax); r != "" {
			recipients = append(recipients, r)
		}
	}
	id, err := newHeldCallID()
	if err != nil {
		log.Printf("integration %s: %s's held call: %v", v.name, slug, err)
		s.auditAgentCall(v.name, start, slug, tool, true, "the call could not be held")
		return integrationError("Kivali could not hold the call for the owner's approval; try again later or tell the owner.")
	}
	now := s.clk().Now()
	h := &integrationHold{
		id: id, agent: slug, integration: v.name, display: v.display, tool: tool,
		title: title, fields: fields, recipients: recipients, needs: hold.Switch,
		taint: s.heldCallTaint(v.name, slug, now), at: now, run: hold.Run,
		answers: make(chan heldCallAnswer), stop: make(chan struct{}), ended: make(chan struct{}),
	}
	if !s.heldCalls.add(h) {
		s.auditAgentCall(v.name, start, slug, tool, false, reasonTooManyHeld)
		return integrationError(refuseTooManyHeld())
	}
	// Armed before the call answers, so the limit counts from the hold.
	// The row is written before awaitHeldCall runs, so it comes before
	// the row of how the call ended.
	t := s.clk().NewTimer(heldCallLimit)
	s.auditAgentCall(v.name, start, slug, tool, true, "waiting for the owner's approval: "+title)
	go s.awaitHeldCall(h, t)
	// The owner may have turned the integration off, or the agent left,
	// after the call was worked out and before h was added, when ending
	// their held calls passed h by.
	if reason, words := s.heldCallGone(h); reason != "" {
		s.heldCalls.stopWhere(func(o *integrationHold) bool { return o == h }, reason, words)
	}
	s.NotifyOrgState()
	return agentpod.IntegrationCallResponse{Content: []string{heldCallWaiting}}
}

// heldCallWords is every word of hold the owner reads: the title, each
// field's label and value, and the recipients.
func heldCallWords(hold *builtinintegrations.Hold) []string {
	out := make([]string, 0, 1+2*len(hold.Fields)+len(hold.Recipients))
	out = append(out, hold.Title)
	for _, f := range hold.Fields {
		out = append(out, f.Label, f.Value)
	}
	return append(out, hold.Recipients...)
}

// zeroWidthJoiner joins emoji into one (a family, a skin tone on a
// profession): it changes how they are drawn, never what text the owner
// reads, so a hold may carry it.
const zeroWidthJoiner = '\u200d'

// hasFormatRune reports whether w holds a Unicode format character
// (category Cf) other than zeroWidthJoiner, which shows as nothing on
// the owner's screen.
func hasFormatRune(w string) bool {
	return strings.IndexFunc(w, func(r rune) bool { return r != zeroWidthJoiner && unicode.Is(unicode.Cf, r) }) >= 0
}

// heldCallGone is why h may no longer wait, in the words the owner's
// action or the agent's leaving ends a held call with
// (endHeldCallsOf, endAgentHeldCalls): the audit's reason and the
// agent's words. Both are empty while its agent is on the team and its
// integration installed, enabled and connected.
func (s *Server) heldCallGone(h *integrationHold) (reason, words string) {
	if _, err := s.Store.GetAgent(h.agent); errors.Is(err, store.ErrNotFound) {
		return reasonAgentLeft, wordsAgentLeft
	}
	it, err := s.Store.GetIntegration(h.integration)
	switch {
	case errors.Is(err, store.ErrNotFound):
		return "the owner uninstalled the integration", "the owner uninstalled the " + h.display + " integration"
	case err == nil && !it.Enabled:
		return "the owner disabled the integration", "the owner disabled the " + h.display + " integration"
	}
	if _, err := s.Store.GetIntegrationConnection(h.integration); errors.Is(err, store.ErrNotFound) {
		return "the owner disconnected the integration", "the owner disconnected the " + h.display + " integration"
	}
	return "", ""
}

// heldCallTaint is the taint line of a call slug holds on name at now:
// how many messages or documents from outside the owner's domain the
// agent's calls of it returned in the heldCallTaintWindow before, as
// the audit's outside lists name them, and who sent the latest; empty
// when none did.
func (s *Server) heldCallTaint(name, slug string, now time.Time) string {
	rows, err := s.Store.ReadIntegrationAuditTail(name, heldCallTaintRows)
	if err != nil {
		log.Printf("integration %s: %s's held call: read the audit for its taint line: %v", name, slug, err)
		return ""
	}
	since := now.Add(-heldCallTaintWindow)
	n, latest := 0, ""
	for _, r := range rows { // newest first
		if r.Agent != slug || len(r.Outside) == 0 || r.At.Before(since) {
			continue
		}
		if latest == "" {
			latest = r.Outside[len(r.Outside)-1]
		}
		n += len(r.Outside)
	}
	switch n {
	case 0:
		return ""
	case 1:
		return "Before asking, this agent read 1 message or document from outside your domain, from " + latest + "."
	}
	return fmt.Sprintf("Before asking, this agent read %d messages or documents from outside your domain, the latest from %s.", n, latest)
}

// awaitHeldCall waits for h's end: the owner's answers, one at a time,
// the limit, or a stop. An approval Kivali refuses to run leaves it
// waiting.
func (s *Server) awaitHeldCall(h *integrationHold, t clock.Timer) {
	defer close(h.ended)
	defer t.Stop()
	for {
		select {
		case a := <-h.answers:
			out, done, err := s.answerHeldCall(h, a)
			a.reply <- heldCallReply{out, err}
			if done {
				return
			}
		case <-t.C():
			s.endHeldCall(h, s.clk().Now(), false, "nobody answered within a day", nil,
				"Not answered: "+h.title, "Nobody answered within a day, so it was not done.")
			return
		case <-h.stop:
			reason, words := s.heldCalls.stoppedWhy(h)
			s.endHeldCall(h, s.clk().Now(), false, "denied: "+reason, nil,
				"Denied: "+h.title, "It was not done: "+words+".")
			return
		}
	}
}

// answerHeldCall is the owner's answer a to h: what the owner reads
// back, and whether h ended. Its end is written and the agent told
// before it returns.
func (s *Server) answerHeldCall(h *integrationHold, a heldCallAnswer) (apitypes.NeedActionResponse, bool, error) {
	start := s.clk().Now()
	name := cmp.Or(s.agentNames()[h.agent], h.agent)
	link := "/agents/" + url.PathEscape(h.agent)
	told := name + " is told in its chat"
	note := ""
	if a.note != "" {
		note = "\n\nThe owner's note: " + a.note
	}
	if !a.approved {
		reason := "denied"
		if a.note != "" {
			reason += ": " + a.note
		}
		s.endHeldCall(h, start, false, reason, nil, "Denied: "+h.title, "The owner denied it, so it was not done."+note)
		return apitypes.NeedActionResponse{Result: "Denied. " + told, Link: link}, true, nil
	}

	ctx := context.Background()
	v, conn, scrub, why, audit := s.heldCallTarget(ctx, h, name)
	if why != "" {
		s.auditAgentCall(h.integration, start, h.agent, h.tool, false, "approved, not run: "+cmp.Or(audit, why))
		return apitypes.NeedActionResponse{}, false, &ceoRefusal{
			Status: http.StatusConflict,
			Msg:    "integration_hold approve: " + why,
			Words:  why + ", so it was not done. It is still waiting: approve it again once that changes, or deny it",
			Who:    whoYou,
		}
	}
	res, callErr := runBuiltin(ctx, s.clk(), integrationCallTimeout, func(ctx context.Context) (*builtinintegrations.Result, error) {
		return h.run(ctx, conn)
	})
	var refusal *builtinintegrations.Refusal
	if errors.As(callErr, &refusal) {
		words := scrub.Replace(refusal.Text)
		s.endHeldCall(h, start, false, "approved, then refused: "+scrub.Replace(refusal.Reason), nil, "Approved: "+h.title,
			"The owner approved it, but the "+v.display+" integration refused it, and nothing was done:\n\n"+fenced(clipText(words, heldCallAnswerMax))+note)
		return apitypes.NeedActionResponse{Result: "Approved, but " + v.display + " refused it: " + ownerQuote(words) + ". " + told, Link: link}, true, nil
	}
	var resp agentpod.IntegrationCallResponse
	if callErr == nil && res != nil && (res.Hold != nil || res.ShellJob != nil) {
		resp = integrationError(fmt.Sprintf("The %s integration did not finish the call; tell the owner.", v.display))
	} else {
		raw, err := builtinResultJSON(res, callErr)
		resp, _ = integrationCallAnswer(v.display, raw, err)
	}
	for i := range resp.Content {
		resp.Content[i] = scrub.Replace(resp.Content[i])
	}
	answer := strings.Join(resp.Content, "\n\n")
	// The row names what was done in the built-in's words (Run's Audit)
	// or, failing them, by the title the owner approved; never by Run's
	// answer, which may quote what the call read.
	reason := h.title
	var outside []string
	if callErr == nil && res != nil {
		reason = cmp.Or(scrub.Replace(res.Audit), reason)
		outside = auditOutside(res.Outside, scrub)
	}
	quoted := fenced(clipText(answer, heldCallAnswerMax))
	if resp.IsError {
		s.endHeldCall(h, start, true, "approved, then failed: "+reason, outside, "Approved: "+h.title,
			"The owner approved it, and Kivali ran it, but the "+v.display+" integration answered with an error:\n\n"+quoted+note)
		return apitypes.NeedActionResponse{Result: "Approved, but " + v.display + " answered with an error: " + ownerQuote(answer) + ". " + told, Link: link}, true, nil
	}
	s.endHeldCall(h, start, true, "approved: "+reason, outside, "Approved: "+h.title,
		"The owner approved it, and Kivali ran it. The "+v.display+" integration answered:\n\n"+quoted+note)
	return apitypes.NeedActionResponse{Result: "Approved and done. " + told, Link: link}, true, nil
}

// reasonSwitchOff is the audit's reason, after "approved, not run: ",
// for an approval of a held call whose Switch the agent's access no
// longer has on.
const reasonSwitchOff = "the agent's access no longer allows it"

// heldCallTarget is h's integration as a call of it finds it now, with
// the connection read now and its scrubber; or, when it cannot run,
// why, for the owner, and, when the audit says it otherwise, audit: the
// agent gone, the integration refused (as agentIntegrationFor and the
// agent's listed actions refuse a call), the hold's Switch off in the
// agent's access, or no connection to run it with. name is the agent's.
func (s *Server) heldCallTarget(ctx context.Context, h *integrationHold, name string) (v agentIntegration, conn builtinintegrations.Connection, scrub *strings.Replacer, why, audit string) {
	if _, err := s.Store.GetAgent(h.agent); err != nil {
		return v, conn, nil, name + " is no longer on the team", ""
	}
	it, err := s.Store.GetIntegration(h.integration)
	if err != nil {
		return v, conn, nil, h.display + " is no longer on the team", ""
	}
	v, err = s.agentIntegrationFor(it, h.agent)
	switch {
	case err != nil:
		log.Printf("integration %s: %s's access, for a held call: %v", h.integration, h.agent, err)
		return v, conn, nil, "Kivali could not read " + name + "'s access to " + h.display, ""
	case v.refusal != "":
		return v, conn, nil, heldCallRefused(v, name), ""
	case v.builtin == nil:
		return v, conn, nil, v.display + " is not in this version of Kivali", ""
	}
	if h.needs != "" {
		if on, _ := callerOf(h.agent, v.access).Access[h.needs].(bool); !on {
			return v, conn, nil, name + "'s access to " + v.display + " no longer allows it", reasonSwitchOff
		}
	}
	tools, err := s.agentToolsOf(ctx, v, h.agent)
	if err != nil {
		return v, conn, nil, v.display + " did not list " + name + "'s actions (" + integrationErrorWords(err) + ")", ""
	}
	if !slices.ContainsFunc(tools, func(t agentIntegrationTool) bool { return t.Tool == h.tool }) {
		return v, conn, nil, name + "'s access to " + v.display + " no longer includes " + h.tool, ""
	}
	conn, _, secretValues, err := s.builtinConnectionSecrets(ctx, v.name, "")
	switch {
	case errors.Is(err, store.ErrNotFound):
		return v, conn, nil, v.display + " has no connection", ""
	case errors.Is(err, errSignInNeeded):
		return v, conn, nil, v.display + "'s connection needs you to sign in again", ""
	case errors.Is(err, errVendorUnavailable):
		return v, conn, nil, v.display + " did not answer when Kivali renewed the connection's sign-in", ""
	case err != nil:
		log.Printf("integration %s: the connection, for a held call: %v", v.name, err)
		return v, conn, nil, "Kivali could not read " + v.display + "'s connection", ""
	}
	return v, conn, newScrubber(secretValues), "", ""
}

// heldCallRefused is v's refusal of agent name's call in the owner's
// words.
func heldCallRefused(v agentIntegration, name string) string {
	switch {
	case v.reason == reasonNoVersion:
		return v.display + " has no approved version"
	case v.reason == reasonDisabled:
		return v.display + " is disabled"
	case v.reason == reasonNoAccess:
		return name + " has no access to " + v.display
	case strings.HasPrefix(v.reason, reasonInvalidAccess):
		return name + "'s access to " + v.display + " no longer applies (" + strings.TrimPrefix(v.reason, reasonInvalidAccess) + ")"
	case v.reason == errNotRunning:
		return v.display + " is not running"
	case v.reason == reasonSettingUp:
		return v.display + "'s connection is being set up"
	}
	return v.display + " refused " + name + "'s call (" + v.reason + ")"
}

// endHeldCall ends h: its row leaves Needs you, the audit gains the
// row of how it ended (from start, allowed, with reason and the
// senders outside the owner's domain of what Run returned), and the
// agent, while on the team, is told in its chat with title and body.
func (s *Server) endHeldCall(h *integrationHold, start time.Time, allowed bool, reason string, outside []string, title, body string) {
	s.heldCalls.remove(h.id)
	s.auditAgentRow(h.integration, start, h.agent, h.tool, allowed, reason, outside)
	s.tellHeldCall(h, title, body)
	s.NotifyOrgState()
}

// tellHeldCall writes how h ended as a held_call message from the
// owner to its agent and delivers it at once, as the owner's answer to
// a request is delivered. An agent no longer on the team is told
// nothing.
func (s *Server) tellHeldCall(h *integrationHold, title, body string) {
	if _, err := s.Store.GetAgent(h.agent); err != nil {
		return
	}
	if s.Messenger == nil {
		log.Printf("integration %s: %s's held call %q ended, with no messenger to tell it", h.integration, h.agent, h.title)
		return
	}
	msg := store.Message{
		Type:  store.MsgHeldCall,
		Title: title,
		From:  agent.CEOSlug,
		To:    store.Recipients{h.agent},
		Date:  s.heldCalls.date(s.clk().Now()),
		Body:  body,
	}
	abs, err := s.Store.WriteMessage(msg)
	if err != nil {
		log.Printf("integration %s: %s's held call %q: write how it ended: %v", h.integration, h.agent, h.title, err)
		return
	}
	rel, err := filepath.Rel(s.Store.Root(), abs)
	if err != nil {
		log.Printf("integration %s: %s's held call %q: %v", h.integration, h.agent, h.title, err)
		return
	}
	msg.Path = abs
	if !s.Messenger.DeliverToAgent(context.Background(), h.agent, msg, filepath.ToSlash(rel), s.deliveryHooks()) {
		log.Printf("integration %s: %s's held call %q: not delivered", h.integration, h.agent, h.title)
	}
}

// fenced is text as a fenced block, its fence longer than any run of
// backticks in it, so nothing in it closes the block or reads as
// Markdown.
func fenced(text string) string {
	longest, run := 0, 0
	for _, r := range text {
		if r == '`' {
			run++
			longest = max(longest, run)
		} else {
			run = 0
		}
	}
	fence := strings.Repeat("`", max(3, longest+1))
	return fence + "text\n" + strings.TrimRight(text, "\n") + "\n" + fence
}

// ownerQuote is an integration's words as the owner's reply quotes
// them inside a sentence: on one line, at most heldCallOwnerWordsMax
// runes, without a full stop of their own.
func ownerQuote(words string) string {
	return strings.TrimRight(clipRunes(strings.Join(strings.Fields(words), " "), heldCallOwnerWordsMax), ".")
}

// endHeldCallsOf ends, unrun, every call held on the integration name,
// as the owner's action (reason, for the audit) does: words say it to
// the agent ("the owner disabled the Echo integration").
func (s *Server) endHeldCallsOf(name, reason, words string) {
	s.heldCalls.stopWhere(func(h *integrationHold) bool { return h.integration == name }, reason, words)
}

// The audit's reason and the agent's words for a held call ended as
// its agent left the team.
const (
	reasonAgentLeft = "the agent left the team"
	wordsAgentLeft  = "you left the team"
)

// endAgentHeldCalls ends, unrun, every call slug holds, as it leaves
// the team.
func (s *Server) endAgentHeldCalls(slug string) {
	s.heldCalls.stopWhere(func(h *integrationHold) bool { return h.agent == slug }, reasonAgentLeft, wordsAgentLeft)
}

// integrationHoldItems are the Needs-you rows of every held call,
// oldest first. Every word is the built-in's, as it held the call, or
// core's.
func (s *Server) integrationHoldItems() []apitypes.NeedItem {
	holds := s.heldCalls.list()
	if len(holds) == 0 {
		return nil
	}
	names := s.agentNames()
	out := make([]apitypes.NeedItem, 0, len(holds))
	for _, h := range holds {
		fields := make([]apitypes.HoldField, 0, len(h.fields))
		for _, f := range h.fields {
			fields = append(fields, apitypes.HoldField{Label: f.Label, Value: f.Value})
		}
		out = append(out, apitypes.NeedItem{
			ID:          integrationHoldNeedPrefix + h.id,
			Kind:        apitypes.NeedKindIntegrationHold,
			Title:       h.title,
			From:        apitypes.PersonRef{Slug: h.agent, Name: cmp.Or(names[h.agent], h.agent)},
			At:          h.at,
			Attachments: []apitypes.AttachmentRef{},
			Packages:    []apitypes.SoftwarePackage{},
			IntegrationHold: &apitypes.IntegrationHoldRef{
				Integration: h.integration, Display: h.display, Agent: h.agent, Action: h.tool,
				Title: h.title, Fields: fields, Recipients: slices.Clone(h.recipients), Taint: h.taint,
			},
		})
	}
	return out
}

// answerIntegrationHold answers a held call's Needs-you row when path
// names one: approve or deny it, with the owner's note. handled is
// false for any other path. The answer waits for the outcome, Run's
// included, which a call's deadline bounds.
func (s *Server) answerIntegrationHold(kind responseKind, approved *bool, path, message string) (handled bool, out apitypes.NeedActionResponse, err error) {
	id, ok := strings.CutPrefix(path, integrationHoldNeedPrefix)
	if !ok {
		return false, out, nil
	}
	if kind != responseKindApproval || approved == nil {
		return true, out, &ceoRefusal{Status: http.StatusBadRequest, Msg: "integration_hold takes approve or deny",
			Words: "That request is an approval, so it can only be approved or denied", Who: whoDevelopers}
	}
	note := strings.TrimSpace(message)
	if len(note) > maxHeldCallNote {
		return true, out, &ceoRefusal{Status: http.StatusBadRequest, Msg: "integration_hold note over 2 KB",
			Words: "The note is over 2 KB; shorten it", Who: whoYou}
	}
	h := s.heldCalls.get(id)
	if h == nil {
		return true, out, errHeldCallGone
	}
	reply := make(chan heldCallReply, 1)
	select {
	case h.answers <- heldCallAnswer{approved: *approved, note: note, reply: reply}:
	case <-h.ended:
		return true, out, errHeldCallGone
	}
	r := <-reply
	return true, r.out, r.err
}
