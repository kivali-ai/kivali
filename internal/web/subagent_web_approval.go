// Package web (subagent_web_approval.go) — a web task's uploads,
// approved by a person before it starts.
//
// A web task that names files may upload them to a page, so a person
// approves it first. Validation has made sure it has a start URL; the
// task is held to that URL's site for its whole run (LockSite), the
// same lock a site session has, so an approval for vendor.com cannot
// be carried anywhere else. The job registers in
// subagentJobAwaitingApproval with nothing run and no browser started,
// and Home lists a web_upload need: the files (each as a download of
// the bytes approved, checked against their digest), the site, the
// start URL and the task's prompt. At most maxWebAwaitingApproval of an
// agent's tasks wait at once. Approve and the task queues for a slot
// like any other; the approval covers its uploads of those bytes to
// that site, and the web budget counts the task like any other. Deny
// and it ends blocked, upload_denied, with the person's words. After
// webApprovalLimit with no answer it ends blocked,
// user_did_not_respond. The calling agent is told nothing until the
// result. A restart of core leaves a waiting task errored: the
// approval lived in memory (recoverOrphanedSubagents).

package web

import (
	"context"
	"net/http"
	"sort"
	"time"

	"github.com/kivali-ai/kivali/internal/agentpod"
	"github.com/kivali-ai/kivali/internal/structured"
)

// webApprovalLimit is how long a task waits for a person to answer
// before it ends blocked, user_did_not_respond.
const webApprovalLimit = 24 * time.Hour

// webUploadAnswer is a person's answer to a task awaiting approval.
type webUploadAnswer struct {
	approved bool
	reason   string
}

// approvalOutcome is how a wait for approval ended.
type approvalOutcome int

const (
	approvalGranted approvalOutcome = iota
	approvalDenied
	approvalTimedOut
	approvalCancelled
)

// needsApproval reports whether a task is held for a person's approval
// before it starts: a web task that names files.
func (j *subagentJob) needsApproval() bool {
	return j.Kind == agentpod.SubagentKindWeb && len(j.Files) > 0
}

// holdForApproval puts a job being registered into
// subagentJobAwaitingApproval, with its wait limit running from now.
// Called before the job is registered, so nothing sees it otherwise.
func (s *SubagentService) holdForApproval(job *subagentJob) {
	job.State = subagentJobAwaitingApproval
	job.WaitSince = s.clk().Now()
	job.approval = make(chan webUploadAnswer, 1)
	job.waitTimer = s.clk().NewTimer(webApprovalLimit)
}

// awaitApproval blocks until a person answers, the limit passes or the
// job is cancelled. An approved job is queued again for a slot; the
// reason is the person's words on a denial.
func (s *SubagentService) awaitApproval(ctx context.Context, job *subagentJob) (approvalOutcome, string) {
	s.jobsMu.Lock()
	answers, t := job.approval, job.waitTimer
	s.jobsMu.Unlock()
	defer t.Stop()
	var answer webUploadAnswer
	select {
	case answer = <-answers:
	case <-t.C():
		// A person may have answered as the limit passed: the answer
		// taken first stands.
		late := false
		s.updateJob(job.ID, func(j *subagentJob) {
			late = j.approvalDone
			j.approvalDone = true
		})
		if !late {
			return approvalTimedOut, ""
		}
		answer = <-answers
	case <-ctx.Done():
		s.updateJob(job.ID, func(j *subagentJob) { j.approvalDone = true })
		return approvalCancelled, ""
	}
	if !answer.approved {
		return approvalDenied, answer.reason
	}
	s.updateJob(job.ID, func(j *subagentJob) {
		j.State = subagentJobQueued
		j.Approved = true
		j.WaitSince = time.Time{}
	})
	return approvalGranted, ""
}

// errWebNeedGone is an answer to a web need that is no longer waiting.
var errWebNeedGone = &ceoRefusal{
	Status: http.StatusConflict,
	Msg:    "web task is not awaiting approval",
	Words:  "That task is no longer waiting for an answer",
	Who:    whoNoOne,
}

// answerWebUpload is a person's answer to one of parent's tasks
// awaiting approval. Only the first answer counts.
func (s *SubagentService) answerWebUpload(parent, id string, approved bool, reason string) error {
	taken := false
	s.updateJob(id, func(j *subagentJob) {
		if j.Parent != parent || j.State != subagentJobAwaitingApproval || j.approvalDone || j.approval == nil {
			return
		}
		j.approvalDone = true
		taken = true
		j.approval <- webUploadAnswer{approved: approved, reason: reason}
	})
	if !taken {
		return errWebNeedGone
	}
	return nil
}

// webUploadEnding is the result core writes for a task that never
// started: denied, or nobody answered.
func webUploadEnding(outcome approvalOutcome, reason string) string {
	if outcome == approvalDenied {
		return structured.UploadDenied(reason).Body()
	}
	return structured.Blocked(structured.BlockedUserDidNotRespond).Body()
}

// webNeedIDs are the IDs of every web task's rows on Needs you, as
// Home lists them: a task waiting for a sign-in, a task awaiting
// approval, a budget notice nobody has acknowledged. Nil-safe.
func (s *SubagentService) webNeedIDs() []string {
	if s == nil {
		return nil
	}
	var out []string
	s.jobsMu.Lock()
	for _, j := range s.jobs {
		switch j.State {
		case subagentJobWaiting:
			out = append(out, webSignInNeedPrefix+j.Parent+"/"+j.ID)
		case subagentJobAwaitingApproval:
			out = append(out, webUploadNeedPrefix+j.Parent+"/"+j.ID)
		}
	}
	s.jobsMu.Unlock()
	for _, n := range s.webBudgetNotices() {
		out = append(out, webBudgetNeedPrefix+n.Parent+"/"+n.ID)
	}
	return out
}

// awaitingApprovalJobs are every agent's tasks awaiting approval, for
// Home's needs.
func (s *SubagentService) awaitingApprovalJobs() []subagentJob {
	s.jobsMu.Lock()
	defer s.jobsMu.Unlock()
	var out []subagentJob
	for _, j := range s.jobs {
		if j.State == subagentJobAwaitingApproval {
			out = append(out, *j)
		}
	}
	sort.Slice(out, func(a, b int) bool {
		if out[a].WaitSince.Equal(out[b].WaitSince) {
			return out[a].ID < out[b].ID
		}
		return out[a].WaitSince.Before(out[b].WaitSince)
	})
	return out
}
