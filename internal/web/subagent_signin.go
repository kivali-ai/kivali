// Package web (subagent_signin.go) — a web task waiting for a person
// to sign in.
//
// A web task in a site session, dispatched by an agent itself, may call
// web_wait_for_sign_in when a page needs a sign-in, a second factor, a
// CAPTCHA or a consent only a person can give. Core sets the job to
// subagentJobWaiting, ends the task's run a moment later, and asks the
// person through a web_sign_in need on Home and the task's view. The
// browser window takes input while the task waits. When the person
// closes the window (the last window on the task, after the grace), or
// after signInWaitLimit with nobody coming, the task's run continues:
// a resumed run of the same session with one line from core, or an end
// core writes, outcome blocked, user_did_not_respond. The calling agent
// is told nothing until the final result.

package web

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"time"

	"github.com/kivali-ai/kivali/internal/agentpod"
	"github.com/kivali-ai/kivali/internal/web/apitypes"
)

const (
	// signInWaitLimit is how long a task waits for a person before it
	// ends blocked, user_did_not_respond.
	signInWaitLimit = 24 * time.Hour
	// signInEndGrace is how long after web_wait_for_sign_in core waits
	// before ending the run, so the tool's answer reaches the model.
	signInEndGrace = subagentAcceptGrace
	// signInResumeLine is the one user message a resumed run gets: core's
	// words, never the person's or the page's.
	signInResumeLine = "The person has finished in the browser window. Continue your task from the page as it is now, with your tabs as you left them. If the page still needs a sign-in, call web_wait_for_sign_in again; otherwise do not call it."
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
	entered := false
	s.updateJob(j.ID, func(job *subagentJob) {
		if job.State != subagentJobRunning {
			return
		}
		entered = true
		job.State = subagentJobWaiting
		job.WaitReason = a.Reason
		job.WaitSince = s.clk().Now()
		job.resumed = make(chan struct{})
		job.resumeDone = false
		job.waitTimer = s.clk().NewTimer(signInWaitLimit)
	})
	if !entered {
		return refuse("this task is not running")
	}
	s.patchSubagentMeta(j.Parent, j.ID, map[string]any{"status": subagentJobWaiting, "wait_reason": a.Reason, "site": j.Site})
	s.notifyWorkingChanged()
	if s.Browsers != nil {
		s.Browsers.BroadcastState(j.Parent, j.Session, j.ID, true)
	}
	go s.endRunForWait(j.ID)
	return agentpod.WebCommandResponse{Text: fmt.Sprintf("A person is being asked to handle %s in a window on your tab. Your turn ends here: do nothing more. You will be resumed, with your tabs as they are, once they have closed the window.", webSignInPhrase(a.Reason))}
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
)

// awaitSignIn blocks until the person is done (the task's last window
// closed), the wait limit passes, or the job is cancelled.
func (s *SubagentService) awaitSignIn(ctx context.Context, job *subagentJob) signInOutcome {
	s.jobsMu.Lock()
	resumed, t := job.resumed, job.waitTimer
	s.jobsMu.Unlock()
	defer t.Stop()
	select {
	case <-resumed:
		return signInResumed
	case <-t.C():
		return signInTimedOut
	case <-ctx.Done():
		return signInCancelled
	}
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
func (s *SubagentService) windowsClosed(slug, _ string, group string) {
	s.updateJob(group, func(j *subagentJob) {
		if j.Parent != slug || j.State != subagentJobWaiting || j.resumed == nil || j.resumeDone {
			return
		}
		j.resumeDone = true
		close(j.resumed)
	})
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

// patchSubagentMeta changes the named fields of a task's meta.json,
// leaving the rest as it is; a nil value removes the field. The task's
// stream watches the file, so a change reaches the task's view at once.
func (s *SubagentService) patchSubagentMeta(parent, id string, fields map[string]any) {
	path := subagentMetaPath(s.Store.Root(), parent, id)
	body, err := os.ReadFile(path)
	if err != nil {
		return
	}
	var meta map[string]any
	if json.Unmarshal(body, &meta) != nil {
		return
	}
	for k, v := range fields {
		if v == nil {
			delete(meta, k)
			continue
		}
		meta[k] = v
	}
	out, err := json.MarshalIndent(meta, "", "  ")
	if err != nil {
		return
	}
	_ = os.WriteFile(path, out, 0o644)
}
