// Package web (browser_relay.go) — a web subagent's tool calls on
// their way to its browser.
//
// POST /v1/agent/{slug}/subagents/{id}/web carries one web_* call.
// Core checks that the id is one of the agent's running web tasks,
// that the command is one the task's session allows and that it fits
// the web budget (subagent_web_budget.go), hands an upload's bytes to
// the pod first, and relays the command under the task's tab group,
// with the site the group is held to. The pod's answer, text and an
// optional image, is what the model reads. A browser that could not
// start ends the task, outcome failed.

package web

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"path"
	"strings"

	"github.com/kivali-ai/kivali/internal/agentpod"
	"github.com/kivali-ai/kivali/internal/browserpod"
)

// webCommandMaxBody bounds a web tool call on the wire.
const webCommandMaxBody = 256 << 10

// handleAgentpodWebCommand relays one web tool call from a web
// subagent of the bound slug.
func (s *Server) handleAgentpodWebCommand(w http.ResponseWriter, r *http.Request) {
	slug := r.PathValue("slug")
	if !checkSlugHeader(w, r, slug) {
		return
	}
	if s.SubagentService == nil {
		http.Error(w, "subagent service not configured", http.StatusServiceUnavailable)
		return
	}
	var req agentpod.WebCommandRequest
	r.Body = http.MaxBytesReader(w, r.Body, webCommandMaxBody)
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "decode: "+err.Error(), http.StatusBadRequest)
		return
	}
	writeJSON(w, http.StatusOK, s.SubagentService.WebCommand(r.Context(), slug, r.PathValue("id"), req))
}

// WebCommand runs one web tool call for a running web task of parent's.
// What comes back is the tool result the subagent reads: a refusal by
// core or the pod is text with IsError set, never a transport error,
// so the model learns what to do instead.
func (s *SubagentService) WebCommand(ctx context.Context, parent, id string, req agentpod.WebCommandRequest) agentpod.WebCommandResponse {
	refuse := func(format string, args ...any) agentpod.WebCommandResponse {
		return agentpod.WebCommandResponse{Text: fmt.Sprintf(format, args...), IsError: true}
	}
	if s == nil || s.Browsers == nil {
		return refuse("browsers are not available in this deployment")
	}
	j, ok := s.lookupJob(id)
	switch {
	case !ok || j.Parent != parent:
		return refuse("no running web task %s of yours", id)
	case j.Kind != agentpod.SubagentKindWeb:
		return refuse("task %s is not a web task", id)
	case j.State == subagentJobWaiting:
		return refuse("this task is waiting for a person in the browser window; your turn is over, do nothing more")
	case j.State != subagentJobRunning || j.cancelled || j.endReason != "" || j.Result != "":
		return refuse("this task is no longer running; end your turn")
	}
	command := strings.TrimSpace(req.Command)
	if command == agentpod.WebWaitForSignInCommand {
		// Core's own: nothing goes to the browser.
		return s.waitForSignIn(j, req.Args)
	}
	if !browserpod.KnownCommand(command) {
		return refuse("unknown web command %q", command)
	}
	args := req.Args
	if len(args) == 0 {
		args = json.RawMessage(`{}`)
	}
	switch command {
	case browserpod.CommandEval:
		if j.Session != agentpod.OpenWebSession {
			return refuse("web_eval is not available in this session")
		}
		if j.LockSite != "" {
			return refuse("web_eval is not available to a task that uploads files")
		}
	case browserpod.CommandNavigate:
		var a browserpod.NavigateArgs
		if err := json.Unmarshal(args, &a); err != nil || strings.TrimSpace(a.URL) == "" {
			return refuse("web_navigate needs a url")
		}
		if resp, ok := taskURLAllowed(j, "web_navigate", a.URL); !ok {
			return resp
		}
	case browserpod.CommandTabs:
		var a browserpod.TabsArgs
		if err := json.Unmarshal(args, &a); err != nil {
			return refuse("web_tabs needs an action")
		}
		if a.Action == browserpod.TabsOpen && strings.TrimSpace(a.URL) != "" {
			if resp, ok := taskURLAllowed(j, "web_tabs", a.URL); !ok {
				return resp
			}
		}
	case browserpod.CommandUpload:
		var a browserpod.UploadArgs
		if err := json.Unmarshal(args, &a); err != nil || a.Ref == "" || a.File == "" {
			return refuse("web_upload needs a ref and a file name")
		}
	}
	if refusal := s.chargeWebAction(j, command, args, func() string { return s.webActionSite(j, command, args) }); refusal != "" {
		return refuse("%s", refusal)
	}
	if command == browserpod.CommandUpload {
		var a browserpod.UploadArgs
		_ = json.Unmarshal(args, &a)
		file, body, resp, ok := s.approvedTaskFile(j, a.File)
		if !ok {
			return resp
		}
		// A file goes only onto a page of the site the task is held
		// to: every tab of the task must be on it, whatever the pod's
		// hold has let a tab reach. The pod checks the tab it uploads
		// into as well.
		if off, ok := s.webTabsOnSite(j); !ok {
			if off == webNoPageYet {
				return refuse("web_upload puts a file only on a page of %s, and your tab is at %s. Go to a page of %s first.", j.LockSite, off, j.LockSite)
			}
			// The tab's URL is the page's to set, so it goes between
			// the page content lines, not in core's own sentence.
			return refuse("%s", browserpod.WithPage(fmt.Sprintf("web_upload puts a file only on a page of %s, and a tab of yours is on another page. Go to a page of %s first.", j.LockSite, j.LockSite), browserpod.URLLine(off)))
		}
		if err := s.Browsers.SendFile(ctx, j.Parent, j.Session, j.ID, path.Base(file.Path), bytes.NewReader(body), int64(len(body))); err != nil {
			if isBrowserStartError(err) {
				return s.endForBrowserStart(j, err)
			}
			return refuse("%v", err)
		}
	}
	// Site holds the task's tab group to its site in the pod, the lock
	// a site session's whole browser has: a task that uploads files
	// stays on the site it was approved for.
	res, err := s.Browsers.Command(ctx, parent, j.Session, id, browserpod.Message{Name: command, Args: args, Site: j.LockSite})
	if err != nil {
		if isBrowserStartError(err) {
			return s.endForBrowserStart(j, err)
		}
		return refuse("%v", err)
	}
	out := agentpod.WebCommandResponse{Text: res.Text, IsError: !res.OK}
	if !res.OK && res.Error != "" {
		out.Text = res.Error
	}
	if res.Image != nil && len(res.Image.Data) > 0 {
		out.Image = &agentpod.WebImage{MIME: res.Image.MIME, Data: res.Image.Data}
	}
	return out
}

// browserStartFailed is the reason a web task's caller reads when the
// task's browser could not start. What stopped it is in the task's
// tool result and core's log: the pod's part of that is free text for
// a log line, not for the caller.
const browserStartFailed = "the browser could not start"

// endForBrowserStart ends a web task whose browser could not start, as
// core ends a task at the submit_result attempt cap: the caller gets
// outcome failed with browserStartFailed, or errBrowserNoMemory's words
// when the team had no memory for it, rather than a reason the
// subagent made up for a failure it cannot see. The tool result keeps
// what stopped the browser, so the task's transcript shows it.
func (s *SubagentService) endForBrowserStart(j subagentJob, err error) agentpod.WebCommandResponse {
	var ends []func()
	s.updateJob(j.ID, func(job *subagentJob) {
		if job.endReason == "" && job.Result == "" && job.State == subagentJobRunning && !job.cancelled && !job.closing {
			job.endReason = browserStartFailed
			if errors.Is(err, errBrowserNoMemory) {
				// The cause, and where the owner raises the team's
				// memory: a reason the caller and the person can act on.
				job.endReason = errBrowserNoMemory.Error()
			}
			// The task's run ends, and with it the workers a sub-lead
			// dispatched: nobody is left to read them.
			ends = s.collectEndCancelsLocked(j.Parent, j.ID)
		}
	})
	if len(ends) > 0 {
		log.Printf("subagent %s/%s: ending: %v", j.Parent, j.ID, err)
	}
	for _, end := range ends {
		end()
	}
	return agentpod.WebCommandResponse{Text: fmt.Sprintf("This task has ended without an answer: its browser could not start. End your turn.\n%v", err), IsError: true}
}

// taskURLAllowed checks a URL a task asks its browser to load, for
// tool: http or https, and for a task held to a site (a site session's,
// or the site an upload was approved for) on that site or one of its
// subdomains, the rule the pod applies too. The refusal is the tool
// result the model reads.
func taskURLAllowed(j subagentJob, tool, raw string) (agentpod.WebCommandResponse, bool) {
	refuse := func(text string) (agentpod.WebCommandResponse, bool) {
		return agentpod.WebCommandResponse{Text: text, IsError: true}, false
	}
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return refuse(tool + " takes an http or https URL")
	}
	if site := j.lockSite(); site != "" && !onSite(site, u.Hostname()) {
		if j.Site != "" {
			return refuse(fmt.Sprintf("Blocked: %s is off this session's site, %s. A task in a site session stays on it and its subdomains.", strings.TrimSpace(raw), j.Site))
		}
		return refuse(fmt.Sprintf("Blocked: %s is off %s, the site this task is held to.", strings.TrimSpace(raw), site))
	}
	return agentpod.WebCommandResponse{}, true
}

// lockSite is the site a web task is held to: LockSite, set at
// registration, else its session's site.
func (j subagentJob) lockSite() string {
	if j.LockSite != "" {
		return j.LockSite
	}
	return j.Site
}

// webActionSite is where a command's data would go, for a budget
// notice: the site of the URL it opens, or of the page its tab is on.
func (s *SubagentService) webActionSite(j subagentJob, command string, args json.RawMessage) string {
	var target string
	switch command {
	case browserpod.CommandNavigate:
		var a browserpod.NavigateArgs
		_ = json.Unmarshal(args, &a)
		target = a.URL
	case browserpod.CommandTabs:
		var a browserpod.TabsArgs
		_ = json.Unmarshal(args, &a)
		target = a.URL
	}
	if strings.TrimSpace(target) == "" && s.Browsers != nil {
		target = s.Browsers.GroupURL(j.Parent, j.Session, j.ID)
	}
	u, err := url.Parse(strings.TrimSpace(target))
	if err != nil || u.Hostname() == "" {
		return ""
	}
	if site, err := webURLSite(target); err == nil {
		return site
	}
	return strings.ToLower(u.Hostname())
}

// webNoPageYet is where webTabsOnSite says a tab is when it has none.
const webNoPageYet = "no page yet"

// webTabsOnSite reports whether every tab of a held task is on its
// site, and if not, where one is: a URL, or webNoPageYet.
func (s *SubagentService) webTabsOnSite(j subagentJob) (string, bool) {
	urls := s.Browsers.GroupTabURLs(j.Parent, j.Session, j.ID)
	if j.LockSite == "" || len(urls) == 0 {
		return webNoPageYet, false
	}
	for _, u := range urls {
		if !onWebSite(j.LockSite, u) {
			if u == "" {
				u = webNoPageYet
			}
			return u, false
		}
	}
	return "", true
}

// webApprovedFile is one of a task's files as it was at dispatch: its
// path under the caller's /files/, and the sha256 and size of its
// bytes. Only the digest is kept: a person approves these bytes, and
// the file is read again, and checked against it, whenever it is shown
// or uploaded.
type webApprovedFile struct {
	Path string
	SHA  string
	Size int64
}

// digestWebFiles is the files a task may upload, as they are now, read
// in the agent's own /files/: only a task the agent dispatches itself
// may upload. A file over the upload limit is refused before it is
// read.
func (s *SubagentService) digestWebFiles(parent string, paths []string) ([]webApprovedFile, error) {
	out := make([]webApprovedFile, 0, len(paths))
	for _, p := range paths {
		f, info, err := s.openCallerFile(parent, "", p)
		if err != nil {
			return nil, fmt.Errorf("%q could not be read", p)
		}
		regular, size := info.Mode().IsRegular(), info.Size()
		if !regular || size > maxWebUploadBytes {
			_ = f.Close()
			if !regular {
				return nil, fmt.Errorf("%q is not a file", p)
			}
			return nil, fmt.Errorf("%q is over %d MB, the most a task may upload", p, maxWebUploadBytes>>20)
		}
		h := sha256.New()
		n, err := io.Copy(h, io.LimitReader(f, maxWebUploadBytes+1))
		_ = f.Close()
		if err != nil {
			return nil, fmt.Errorf("%q could not be read", p)
		}
		if n > maxWebUploadBytes {
			return nil, fmt.Errorf("%q is over %d MB, the most a task may upload", p, maxWebUploadBytes>>20)
		}
		out = append(out, webApprovedFile{Path: p, SHA: hex.EncodeToString(h.Sum(nil)), Size: n})
	}
	return out, nil
}

// errWebFileChanged is a file that is no longer the one a person
// approved.
var errWebFileChanged = errors.New("the file has changed since the task was dispatched")

// readApprovedFile reads an approved file from the agent's /files/,
// the way file_view reads it, and returns its bytes only when they are
// the ones approved. The bytes returned are what is shown or sent, so
// nothing can change between the check and their use.
func (s *SubagentService) readApprovedFile(parent string, file webApprovedFile) ([]byte, error) {
	f, info, err := s.openCallerFile(parent, "", file.Path)
	if err != nil || !info.Mode().IsRegular() {
		return nil, errors.New("that file cannot be read")
	}
	defer func() { _ = f.Close() }()
	if info.Size() != file.Size {
		return nil, errWebFileChanged
	}
	body, err := io.ReadAll(io.LimitReader(f, file.Size+1))
	if err != nil {
		return nil, errors.New("that file cannot be read")
	}
	sum := sha256.Sum256(body)
	if int64(len(body)) != file.Size || hex.EncodeToString(sum[:]) != file.SHA {
		return nil, errWebFileChanged
	}
	return body, nil
}

// webFileChanged is what the model reads when a file is not the one a
// person approved.
const webFileChanged = "%q has changed since a person approved this task, so it cannot be uploaded. Carry on without it, or submit what you have."

// webApprovedFileNamed is the task's approved file by the name it is
// uploaded by.
func webApprovedFileNamed(j subagentJob, name string) (webApprovedFile, bool) {
	for _, f := range j.ApprovedFiles {
		if path.Base(f.Path) == name {
			return f, true
		}
	}
	return webApprovedFile{}, false
}

// approvedTaskFile is the task's file by the name it uploads it by,
// with its bytes, when the file in the caller's /files/ is still the
// one a person approved.
func (s *SubagentService) approvedTaskFile(j subagentJob, name string) (webApprovedFile, []byte, agentpod.WebCommandResponse, bool) {
	refuse := func(text string) (webApprovedFile, []byte, agentpod.WebCommandResponse, bool) {
		return webApprovedFile{}, nil, agentpod.WebCommandResponse{Text: text, IsError: true}, false
	}
	file, ok := webApprovedFileNamed(j, name)
	if !ok {
		return refuse(fmt.Sprintf("%q is not one of this task's files (%s)", name, strings.Join(webTaskFileNames(j.Files), ", ")))
	}
	body, err := s.readApprovedFile(j.Parent, file)
	if errors.Is(err, errWebFileChanged) {
		return refuse(fmt.Sprintf(webFileChanged, name))
	}
	if err != nil {
		return refuse(err.Error())
	}
	return file, body, agentpod.WebCommandResponse{}, true
}
