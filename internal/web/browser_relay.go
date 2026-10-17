// Package web (browser_relay.go) — a web subagent's tool calls on
// their way to its browser.
//
// POST /v1/agent/{slug}/subagents/{id}/web carries one web_* call.
// Core checks that the id is one of the agent's running web tasks and
// that the command is one the task's session allows, hands an upload's
// bytes to the pod first, and relays the command under the task's tab
// group. The pod's answer, text and an optional image, is what the
// model reads.

package web

import (
	"context"
	"encoding/json"
	"fmt"
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
	case browserpod.CommandNavigate:
		var a browserpod.NavigateArgs
		if err := json.Unmarshal(args, &a); err != nil || strings.TrimSpace(a.URL) == "" {
			return refuse("web_navigate needs a url")
		}
		u, err := url.Parse(strings.TrimSpace(a.URL))
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			return refuse("web_navigate takes an http or https URL")
		}
	case browserpod.CommandUpload:
		var a browserpod.UploadArgs
		if err := json.Unmarshal(args, &a); err != nil || a.Ref == "" || a.File == "" {
			return refuse("web_upload needs a ref and a file name")
		}
		if resp, ok := s.sendTaskFile(ctx, j, a.File); !ok {
			return resp
		}
	}
	res, err := s.Browsers.Command(ctx, parent, j.Session, id, browserpod.Message{Name: command, Args: args})
	if err != nil {
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

// sendTaskFile hands the pod one of the task's files before an upload:
// the file the task was dispatched with under that name, read from the
// caller's /files/ the way file_view reads it. The caller is the agent,
// or the sub-lead (j.CallerID) whose own view the path was checked in
// at dispatch.
func (s *SubagentService) sendTaskFile(ctx context.Context, j subagentJob, name string) (agentpod.WebCommandResponse, bool) {
	refuse := func(text string) (agentpod.WebCommandResponse, bool) {
		return agentpod.WebCommandResponse{Text: text, IsError: true}, false
	}
	var modelPath string
	for _, p := range j.Files {
		if path.Base(p) == name {
			modelPath = p
			break
		}
	}
	if modelPath == "" {
		return refuse(fmt.Sprintf("%q is not one of this task's files (%s)", name, strings.Join(webTaskFileNames(j.Files), ", ")))
	}
	f, info, err := s.openCallerFile(j.Parent, j.CallerID, modelPath)
	if err != nil || !info.Mode().IsRegular() {
		return refuse("that file cannot be read")
	}
	defer func() { _ = f.Close() }()
	if info.Size() > maxWebUploadBytes {
		return refuse(fmt.Sprintf("%q is %d MB; a task may upload files up to %d MB", name, info.Size()>>20, maxWebUploadBytes>>20))
	}
	if err := s.Browsers.SendFile(ctx, j.Parent, j.Session, j.ID, name, f, info.Size()); err != nil {
		return refuse(err.Error())
	}
	return agentpod.WebCommandResponse{}, true
}
