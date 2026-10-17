// Package web (web_sessions.go) — an agent's web sessions as the agent
// sees them: names and states, never contents.
//
// GET /v1/agent/{slug}/web/sessions and POST /v1/agent/{slug}/web/sessions/reset
// on the agent socket serve subagent_web_sessions and
// subagent_web_session_reset. Core renders the text, so a session is
// described in one place.

package web

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/kivali-ai/kivali/internal/agentpod"
	"github.com/kivali-ai/kivali/internal/web/apitypes"
)

// handleAgentpodWebSessions lists the bound agent's web sessions.
func (s *Server) handleAgentpodWebSessions(w http.ResponseWriter, r *http.Request) {
	slug := r.PathValue("slug")
	if !checkSlugHeader(w, r, slug) {
		return
	}
	if s.SubagentService == nil {
		http.Error(w, "subagent service not configured", http.StatusServiceUnavailable)
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	_, _ = w.Write([]byte(s.SubagentService.WebSessionsText(slug)))
}

// handleAgentpodWebSessionReset forgets one of the bound agent's web
// sessions.
func (s *Server) handleAgentpodWebSessionReset(w http.ResponseWriter, r *http.Request) {
	slug := r.PathValue("slug")
	if !checkSlugHeader(w, r, slug) {
		return
	}
	if s.SubagentService == nil {
		http.Error(w, "subagent service not configured", http.StatusServiceUnavailable)
		return
	}
	var body struct {
		Session string `json:"session"`
	}
	r.Body = http.MaxBytesReader(w, r.Body, 4<<10)
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "decode: "+err.Error(), http.StatusBadRequest)
		return
	}
	out, err := s.SubagentService.ResetWebSession(slug, body.Session)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	_, _ = w.Write([]byte(out))
}

// WebSessionsText renders an agent's web sessions for the model: the
// open session, then each site session with its site, when it was last
// used, whether its browser runs and how many tasks are on it. Never
// what a session holds.
func (s *SubagentService) WebSessionsText(slug string) string {
	if s.Browsers == nil || !s.Browsers.Enabled() {
		return "Web tasks are not available in this deployment, so you have no web sessions."
	}
	tasksOn := map[string]int{}
	for _, j := range s.listJobs(slug) {
		if j.Kind == agentpod.SubagentKindWeb && !j.terminal() {
			tasksOn[j.Session]++
		}
	}
	state := func(session string) string {
		switch s.Browsers.StateFor(slug, session) {
		case apitypes.BrowserStateRunning:
			return "browser running"
		case apitypes.BrowserStateStarting:
			return "browser starting"
		default:
			return "no browser running"
		}
	}
	tasks := func(session string) string {
		n := tasksOn[session]
		if n == 0 {
			return ""
		}
		return fmt.Sprintf(" · %d task%s on it", n, pluralS(n))
	}
	var b strings.Builder
	fmt.Fprintf(&b, "open — signed in nowhere, nothing kept between tasks · %s%s\n", state(agentpod.OpenWebSession), tasks(agentpod.OpenWebSession))
	sessions, err := s.Store.ListBrowserSessions(slug)
	if err != nil {
		b.WriteString("\nYour site sessions could not be read: " + err.Error() + "\n")
		return b.String()
	}
	var sites []string
	for _, bs := range sessions {
		if bs.Name != agentpod.OpenWebSession {
			sites = append(sites, bs.Name)
		}
	}
	if len(sites) == 0 {
		b.WriteString("\nNo site sessions yet. A web task that names a site as its session (vendor.com, say) creates one, locked to that site, which keeps its sign-ins between tasks.\n")
		return b.String()
	}
	sort.Strings(sites)
	now := s.clk().Now()
	b.WriteString("\nSITE SESSIONS (each locked to its site and its subdomains; sign-ins are kept between tasks):\n")
	for _, bs := range sessions {
		if bs.Name == agentpod.OpenWebSession {
			continue
		}
		used := "never used"
		if !bs.LastUsed.IsZero() {
			used = "last used " + roundDuration(now.Sub(bs.LastUsed)) + " ago"
		}
		fmt.Fprintf(&b, "  %s — %s · %s%s\n", bs.Name, used, state(bs.Name), tasks(bs.Name))
	}
	b.WriteString("\nsubagent_web_session_reset forgets a session: its browser stops, its tasks are cancelled and its sign-ins are deleted.\n")
	return b.String()
}

// webResetCancelWait bounds how long a reset waits for the session's
// cancelled tasks to end before it ends the browser under them.
const webResetCancelWait = 15 * time.Second

// ResetWebSession forgets one of an agent's sessions: every web task
// on it is cancelled and ends, its browser then ends without saving,
// and what the store holds for it is deleted. The tasks go first so
// their windows close with the task's own reason, and their tabs are
// released, before the browser is pulled from under them. A cancelled
// task delivers no result, as after Stop; the text, what the model
// reads, names the tasks it called off.
func (s *SubagentService) ResetWebSession(slug, session string) (string, error) {
	name := strings.TrimSpace(session)
	if name != agentpod.OpenWebSession {
		site, err := webSessionSite(name)
		if err != nil {
			return "", fmt.Errorf("session %q: %v", session, err)
		}
		name = site
	}
	var cancelled []subagentJob
	for _, j := range s.listJobs(slug) {
		if j.Kind == agentpod.SubagentKindWeb && j.Session == name && !j.terminal() {
			if s.CancelSubagent(slug, j.ID) {
				cancelled = append(cancelled, j)
			}
		}
	}
	if len(cancelled) > 0 {
		limit := s.clk().NewTimer(webResetCancelWait)
		defer limit.Stop()
	await:
		for _, j := range cancelled {
			select {
			case <-j.done:
			case <-limit.C():
				break await
			}
		}
	}
	if s.Browsers != nil {
		if err := s.Browsers.Reset(slug, name); err != nil {
			return "", fmt.Errorf("reset %s: %w", name, err)
		}
	} else if err := s.Store.DeleteBrowserSession(slug, name); err != nil {
		return "", fmt.Errorf("reset %s: %w", name, err)
	}
	tasks := fmt.Sprintf("%d task%s cancelled", len(cancelled), pluralS(len(cancelled)))
	if len(cancelled) > 0 {
		ids := make([]string, 0, len(cancelled))
		for _, j := range cancelled {
			ids = append(ids, j.ID)
		}
		tasks += " (" + strings.Join(ids, ", ") + "; none of them will send you a result)"
	}
	if name == agentpod.OpenWebSession {
		return fmt.Sprintf("The open session was reset: its browser stopped and %s. It keeps nothing between tasks anyway.", tasks), nil
	}
	return fmt.Sprintf("Session %s was reset: its browser stopped, %s, and its saved sign-ins deleted. The next web task that names it starts afresh, signed in nowhere.", name, tasks), nil
}
