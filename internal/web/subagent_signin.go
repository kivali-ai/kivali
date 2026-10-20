// Package web (subagent_signin.go) — a web task waiting for a person
// to sign in.
//
// A web task in a site session, dispatched by an agent itself, may call
// web_wait_for_sign_in when a page needs a sign-in, a second factor, a
// CAPTCHA or a consent only a person can give. Core sets the job to
// subagentJobWaiting, ends the task's run a moment later, and asks the
// person through a web_sign_in need on Home and the task's view. The
// browser window takes input while the task waits. When the person
// says they are done (Continue the task in the window, continueTask),
// or closes the window (the last window on the task, after the grace),
// or after signInWaitLimit with nobody coming, the task's run continues:
// a resumed run of the same session with one line from core, or an end
// core writes, outcome blocked, user_did_not_respond. The calling agent
// is told nothing until the final result.
//
// A task may wait signInWaitCap times. The call after that is refused
// and the task ends blocked on what the page still asked for (the
// reason it gave, as a blocked_on the caller knows): a page that needs
// a person a fourth time is not one the task can get through.

package web

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"sort"
	"strings"
	"time"

	"github.com/kivali-ai/kivali/internal/agentpod"
	"github.com/kivali-ai/kivali/internal/structured"
	"github.com/kivali-ai/kivali/internal/web/apitypes"
)

const (
	// signInWaitCap is how many times one task may wait for a person.
	signInWaitCap = 3
	// signInWaitLimit is how long a task waits for a person before it
	// ends blocked, user_did_not_respond.
	signInWaitLimit = 24 * time.Hour
	// signInEndGrace is how long after web_wait_for_sign_in core waits
	// before ending the run, so the tool's answer reaches the model.
	signInEndGrace = subagentAcceptGrace
	// signInResumePodWait is how long a resumed task waits for its
	// agent pod to be connected before its wait for a person goes on;
	// signInResumePodPoll is how often it looks.
	signInResumePodWait = 3 * time.Minute
	signInResumePodPoll = 2 * time.Second
	// signInResumeLine is the one user message a resumed run gets: core's
	// words, never the person's or the page's. signInReopenedLine
	// replaces it when the task's browser was lost during the wait and
	// opened again at the site's root page, %s, and signInBrowserGoneLine
	// when it could not be: then core ended the wait itself.
	signInResumeLine      = "The person has finished in the browser window. Continue your task from the page as it is now, with your tabs as you left them. If the page still needs a sign-in, call web_wait_for_sign_in again; otherwise do not call it."
	signInReopenedLine    = "The person has finished in the browser window. The browser closed while you waited and was opened again, so your earlier tabs are gone: the person was given a new tab at %s. Continue your task from the page as it is now. If the page still needs a sign-in, call web_wait_for_sign_in again; otherwise do not call it."
	signInBrowserGoneLine = "The browser closed while you waited for the person and could not be opened again: your tabs are gone, and the person may not have finished. Decide what to do from here: a web command starts a new browser, or call submit_result with what you have, or with outcome blocked if the page still needs a person."
	// signInPageTextMax bounds a page's title in core's text about it.
	signInPageTextMax = 120
)

// webSignInPhrases spell each reason out, for the person and the model.
var webSignInPhrases = map[apitypes.WebSignInReason]string{
	apitypes.WebSignInReasonSignIn:  "a sign-in",
	apitypes.WebSignInReasonMFA:     "a second factor",
	apitypes.WebSignInReasonCaptcha: "a CAPTCHA",
	apitypes.WebSignInReasonConsent: "a consent",
}

// webSignInPhrase is the reason spelled out, "a sign-in" for one core
// does not know.
func webSignInPhrase(reason string) string {
	if p, ok := webSignInPhrases[apitypes.WebSignInReason(reason)]; ok {
		return p
	}
	return "a sign-in"
}

// webSignInBlockedOn is the blocked_on core writes when a task ends on
// what the page kept asking for: the reason as the wrapper's vocabulary
// names it.
func webSignInBlockedOn(reason string) structured.BlockedOn {
	switch apitypes.WebSignInReason(reason) {
	case apitypes.WebSignInReasonMFA:
		return structured.BlockedMFA
	case apitypes.WebSignInReasonCaptcha:
		return structured.BlockedCaptcha
	case apitypes.WebSignInReasonConsent:
		return structured.BlockedConsent
	default:
		return structured.BlockedLoginRequired
	}
}

// waitForSignIn is web_wait_for_sign_in from a running web task. It
// moves the task to waiting, tells its windows to take input, and ends
// the run after the grace. The text is what the model reads.
func (s *SubagentService) waitForSignIn(j subagentJob, args json.RawMessage) agentpod.WebCommandResponse {
	refuse := func(text string) agentpod.WebCommandResponse {
		return agentpod.WebCommandResponse{Text: text, IsError: true}
	}
	if j.Site == "" {
		return refuse("web_wait_for_sign_in is for a site session; this task runs in the open session, which is never signed in. Submit blocked with blocked_on login_required and let your caller decide.")
	}
	if j.Depth != 1 {
		return refuse("web_wait_for_sign_in is not available to a task dispatched by a subagent. Submit blocked with blocked_on login_required; your caller's agent can arrange the sign-in.")
	}
	var a struct {
		Reason string `json:"reason"`
	}
	if err := json.Unmarshal(args, &a); err != nil || webSignInPhrases[apitypes.WebSignInReason(a.Reason)] == "" {
		return refuse("reason must be one of sign_in, mfa, captcha or consent")
	}
	entered, capped := false, false
	s.updateJob(j.ID, func(job *subagentJob) {
		if job.State != subagentJobRunning {
			return
		}
		if job.waits >= signInWaitCap {
			// The cap: core writes the answer, blocked on what the page
			// still asked for, and ends the run after the grace.
			if job.Result == "" && job.endReason == "" && !job.cancelled && !job.closing {
				job.Result = structured.Blocked(webSignInBlockedOn(a.Reason)).Body()
				capped = true
			}
			return
		}
		entered = true
		job.waits++
		job.reopens = 0
		job.tabs = tabsKept
		job.lost = make(chan struct{})
		job.lostDone = false
		job.State = subagentJobWaiting
		job.WaitReason = a.Reason
		job.WaitSince = s.clk().Now()
		job.resumed = make(chan struct{})
		job.resumeDone = false
		job.waitTimer = s.clk().NewTimer(signInWaitLimit)
	})
	if capped {
		go s.endRunAfterAccept(j.ID)
		return refuse(fmt.Sprintf("This task has asked for a person %d times already, the most a web task may. It is being ended with outcome blocked, blocked_on %s; your caller decides what happens next. Do nothing more.", signInWaitCap, webSignInBlockedOn(a.Reason)))
	}
	if !entered {
		return refuse("this task is not running")
	}
	s.enteredWait(j, a.Reason)
	go s.endRunForWait(j.ID)
	return agentpod.WebCommandResponse{Text: fmt.Sprintf("A person is being asked to handle %s in a window on your tab. Your turn ends here: do nothing more. You will be resumed once they say they are done or close the window, with your tabs as they are; if the browser closes meanwhile, the message that resumes you says what became of them. A task may ask for a person at most %d times.", webSignInPhrase(a.Reason), signInWaitCap)}
}

// enteredWait tells everything that shows a task that it waits for a
// person: its meta.json (the task's view), the org snapshot (Home's
// need), the chip, and its windows, which take input from here.
func (s *SubagentService) enteredWait(j subagentJob, reason string) {
	s.patchSubagentMeta(j.Parent, j.ID, map[string]any{"status": subagentJobWaiting, "wait_reason": reason, "site": j.Site})
	s.notifyWorkingChanged()
	s.emitProgress(j.Parent, j.toolUseID, j.index, j.ID, subagentJobWaiting, "", "")
	if s.Browsers != nil {
		s.Browsers.BroadcastState(j.Parent, j.Session, j.ID, true)
	}
}

// leftWait is the reverse: the task reads as running again while its
// run is brought back, and its windows stop taking input.
func (s *SubagentService) leftWait(j subagentJob) {
	s.patchSubagentMeta(j.Parent, j.ID, map[string]any{"status": subagentJobRunning, "wait_reason": nil})
	s.notifyWorkingChanged()
	s.emitProgress(j.Parent, j.toolUseID, j.index, j.ID, subagentJobRunning, "", "")
	if s.Browsers != nil {
		s.Browsers.BroadcastState(j.Parent, j.Session, j.ID, false)
	}
}

// endRunForWait ends a waiting task's run after the grace. Only the
// run ends; the task waits on.
func (s *SubagentService) endRunForWait(id string) {
	t := s.clk().NewTimer(signInEndGrace)
	defer t.Stop()
	<-t.C()
	var end context.CancelFunc
	s.updateJob(id, func(j *subagentJob) {
		if j.State == subagentJobWaiting {
			end = j.cancelRun
		}
	})
	if end != nil {
		end()
	}
}

// signInOutcome is how a wait ended.
type signInOutcome int

const (
	signInResumed signInOutcome = iota
	signInTimedOut
	signInCancelled
	// signInBrowserLost: the task's browser ended under the wait.
	signInBrowserLost
	// signInBrowserReopened: core opened it again; the wait goes on.
	signInBrowserReopened
)

// waitTabs is what became of a waiting task's tabs.
type waitTabs int

const (
	// tabsKept: the browser ran through the wait, tabs and all.
	tabsKept waitTabs = iota
	// tabsReopened: the browser was lost and opened again at the
	// site's root page.
	tabsReopened
	// tabsGone: the browser was lost and is not back.
	tabsGone
)

// awaitSignIn blocks until the person is done (they said so, or the
// task's last window closed), the wait limit passes, the task's browser
// is lost, or the job is cancelled. The wait timer runs on: a wait a
// resume could not finish (awaitPod) is entered again, and the limit
// still counts from the first call.
func (s *SubagentService) awaitSignIn(ctx context.Context, job *subagentJob) signInOutcome {
	s.jobsMu.Lock()
	resumed, lost, t := job.resumed, job.lost, job.waitTimer
	s.jobsMu.Unlock()
	select {
	case <-resumed:
		return signInResumed
	case <-lost:
		return signInBrowserLost
	case <-t.C():
		return signInTimedOut
	case <-ctx.Done():
		return signInCancelled
	}
}

// browserLost is the registry's report that a site session's running
// browser ended without being asked to. Each task waiting for a person
// on it has lost its tabs, and its wait hears of it, so its browser is
// opened again (reopenForWait). Other tasks are unmoved: a running
// task's next command starts a browser of its own accord.
func (s *SubagentService) browserLost(slug, session string) {
	s.jobsMu.Lock()
	defer s.jobsMu.Unlock()
	for _, j := range s.jobs {
		if j.Parent != slug || j.Kind != agentpod.SubagentKindWeb || j.Session != session || j.State != subagentJobWaiting {
			continue
		}
		j.tabs = tabsGone
		if j.lost != nil && !j.lostDone {
			j.lostDone = true
			close(j.lost)
		}
	}
}

// reopenForWait gives a waiting task whose browser was lost a browser
// again, with a tab at its site's root page, so the person can carry on
// in the window; the task's turn is over, so nothing else would start
// one, and a window would open an empty browser. A wait whose browser
// is back does not count toward signInWaitCap: the person lost their
// work there, and the task may need to ask again.
//
// Only one browser is opened per wait. When none can be (its start
// fails or is refused), or the one opened is lost too, the wait is over
// for good: the task resumes (signInResumed) with signInBrowserGoneLine,
// so Home's need goes and the task decides what to report, rather than
// leaving a person a row whose browser cannot come back.
func (s *SubagentService) reopenForWait(ctx context.Context, job *subagentJob) signInOutcome {
	var j subagentJob
	s.updateJob(job.ID, func(job *subagentJob) {
		// A fresh signal for the next loss.
		job.lost = make(chan struct{})
		job.lostDone = false
		j = *job
	})
	if s.Browsers == nil || j.reopens > 0 {
		log.Printf("subagent %s/%s: the browser was lost again during the wait; the task resumes", j.Parent, j.ID)
		return signInResumed
	}
	if err := s.Browsers.Reopen(ctx, j.Parent, j.Session, j.ID); err != nil {
		if ctx.Err() != nil {
			return signInCancelled
		}
		log.Printf("subagent %s/%s: the browser lost during the wait could not be opened again (%v); the task resumes", j.Parent, j.ID, err)
		return signInResumed
	}
	s.updateJob(j.ID, func(job *subagentJob) {
		job.reopens++
		job.tabs = tabsReopened
		if job.waits > 0 {
			job.waits--
		}
	})
	// The new browser's windows take input, and its group is unlocked
	// while one is on it, as for the browser the wait began on.
	s.Browsers.BroadcastState(j.Parent, j.Session, j.ID, true)
	return signInBrowserReopened
}

// resumeLine is the line a task's resumed run gets, for what became of
// its tabs during the wait, which it then forgets.
func (s *SubagentService) resumeLine(id string) string {
	line := signInResumeLine
	s.updateJob(id, func(j *subagentJob) {
		switch j.tabs {
		case tabsReopened:
			line = fmt.Sprintf(signInReopenedLine, "https://"+j.Site+"/")
		case tabsGone:
			line = signInBrowserGoneLine
		}
		j.tabs = tabsKept
	})
	return line
}

// awaitPod blocks until the parent's agent pod is connected, so a
// resumed run has somewhere to go, up to signInResumePodWait. False
// when none came, or the job was cancelled.
func (s *SubagentService) awaitPod(ctx context.Context, parent string) bool {
	if s.PodConnected == nil || s.PodConnected(parent) {
		return true
	}
	limit := s.clk().NewTimer(signInResumePodWait)
	defer limit.Stop()
	tick := s.clk().NewTicker(signInResumePodPoll)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return false
		case <-limit.C():
			return false
		case <-tick.C():
			if s.PodConnected(parent) {
				return true
			}
		}
	}
}

// reenterWait puts a task whose resume found no pod back to waiting, as
// it was: the same reason, the same WaitSince, a fresh signal for the
// person's next word or the next window to close.
func (s *SubagentService) reenterWait(id string) {
	var j subagentJob
	s.updateJob(id, func(job *subagentJob) {
		job.State = subagentJobWaiting
		job.resumed = make(chan struct{})
		job.resumeDone = false
		job.lost = make(chan struct{})
		job.lostDone = false
		j = *job
	})
	s.enteredWait(j, j.WaitReason)
}

// endWait stops the wait timer once a wait is over for good.
func (s *SubagentService) endWait(id string) {
	s.updateJob(id, func(j *subagentJob) {
		if j.waitTimer != nil {
			j.waitTimer.Stop()
		}
	})
}

// jobWaiting reports whether a job waits for a person.
func (s *SubagentService) jobWaiting(id string) bool {
	waiting := false
	s.updateJob(id, func(j *subagentJob) { waiting = j.State == subagentJobWaiting })
	return waiting
}

// windowTakesInput reports whether a task's browser window may send
// input now: the task waits for a person, in a site session.
func (s *SubagentService) windowTakesInput(id string) bool {
	var ok bool
	s.updateJob(id, func(j *subagentJob) { ok = j.State == subagentJobWaiting && j.Site != "" })
	return ok
}

// windowsClosed is the registry's report that a task's last browser
// window has been gone for the grace: a task waiting for a person
// resumes. Other tasks are unmoved.
//
// The report comes browserWindowGrace after the window went, so one
// that arrives sooner than that after the wait began is about a window
// closed before the task asked for anyone: a watcher who left as the
// task hit the sign-in wall, not a person who has signed in. It is
// ignored, and the task waits for a window opened after the wait began.
func (s *SubagentService) windowsClosed(slug, _ string, group string) {
	now := s.clk().Now()
	s.updateJob(group, func(j *subagentJob) {
		if j.Parent != slug || j.State != subagentJobWaiting || j.resumed == nil || j.resumeDone {
			return
		}
		if now.Sub(j.WaitSince) < browserWindowGrace {
			return
		}
		j.resumeDone = true
		close(j.resumed)
	})
}

// signInContinue is what the person's word that they are done did.
type signInContinue int

const (
	// signInContinued: the task resumes, or had resumed already.
	signInContinued signInContinue = iota
	// signInNotWaiting: the task is not waiting for a person.
	signInNotWaiting
	// signInNoTask: core holds no such task of that agent.
	signInNoTask
)

// continueTask is the person's word, from the task's browser window,
// that they have done what the page asked: a task waiting for them
// resumes at once, as windowsClosed resumes it, without the grace.
// A task already resumed from its wait, and not waiting again, is
// signInContinued too, so a second press changes nothing.
func (s *SubagentService) continueTask(slug, id string) signInContinue {
	out := signInNoTask
	s.updateJob(id, func(j *subagentJob) {
		if j.Parent != slug {
			return
		}
		switch {
		case j.State == subagentJobWaiting && j.resumed != nil:
			if !j.resumeDone {
				j.resumeDone = true
				close(j.resumed)
			}
			out = signInContinued
		case j.State == subagentJobRunning && j.resumeDone:
			out = signInContinued
		default:
			out = signInNotWaiting
		}
	})
	return out
}

// waitingWebJobs are every agent's tasks waiting for a person, for
// Home's needs.
func (s *SubagentService) waitingWebJobs() []subagentJob {
	s.jobsMu.Lock()
	defer s.jobsMu.Unlock()
	var out []subagentJob
	for _, j := range s.jobs {
		if j.State == subagentJobWaiting {
			out = append(out, *j)
		}
	}
	return out
}

// signInWaitKeys names every task waiting for a person as parent/id,
// sorted, for the org snapshot: Home refetches its needs when the list
// changes. Never nil.
func (s *SubagentService) signInWaitKeys() []string {
	out := []string{}
	if s == nil {
		return out
	}
	for _, j := range s.waitingWebJobs() {
		out = append(out, j.Parent+"/"+j.ID)
	}
	sort.Strings(out)
	return out
}

// waitingPage is the page a waiting task's window shows, as the
// browser reported it: its URL and title, empty when core knows none.
// Both are page text; whoever shows them shows them as text.
func (s *SubagentService) waitingPage(j subagentJob) (url, title string) {
	if s.Browsers == nil {
		return "", ""
	}
	url, title = s.Browsers.CurrentPage(j.Parent, j.Session, j.ID)
	return pageText(url), pageText(title)
}

// pageText is a page's URL or title as core quotes it: one line, at
// most signInPageTextMax runes.
func pageText(v string) string {
	v = strings.Join(strings.Fields(v), " ")
	if r := []rune(v); len(r) > signInPageTextMax {
		v = string(r[:signInPageTextMax-1]) + "…"
	}
	return v
}

// patchSubagentMeta changes the named fields of a task's meta.json,
// leaving the rest as it is; a nil value removes the field. A task with
// no meta.json yet is left without one. The task's stream watches the
// file, so a change reaches the task's view at once.
func (s *SubagentService) patchSubagentMeta(parent, id string, fields map[string]any) {
	_ = s.Store.UpdateSubagentMeta(parent, id, func(meta map[string]any) bool {
		if len(meta) == 0 {
			return false
		}
		for k, v := range fields {
			if v == nil {
				delete(meta, k)
				continue
			}
			meta[k] = v
		}
		return true
	})
}
