package web

import (
	"cmp"
	"errors"
	"net/http"
	"sort"
	"strings"

	"github.com/kivali-ai/kivali/internal/agent"
	"github.com/kivali-ai/kivali/internal/store"
	"github.com/kivali-ai/kivali/internal/web/apitypes"
)

// handleAPIAgents serves GET /api/v1/agents: active agents in tree
// order with their live state, then archived agents by slug.
func (s *Server) handleAPIAgents(w http.ResponseWriter, _ *http.Request) {
	actives, err := s.Store.ListActiveAgents()
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, "the team could not be read", whoDevelopers)
		return
	}
	archived, err := s.Store.ListArchivedAgents()
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, "the archived team could not be read", whoDevelopers)
		return
	}
	working := s.workingAgents()
	resp := apitypes.AgentsResponse{
		Agents:   make([]apitypes.AgentSummary, 0, len(actives)),
		Archived: make([]apitypes.AgentSummary, 0, len(archived)),
	}
	for _, n := range agentTreeOrder(actives) {
		resp.Agents = append(resp.Agents, s.agentSummary(n.Agent, s.agentLivenessFor(n, working)))
	}
	sort.Slice(archived, func(i, j int) bool { return archived[i].Slug < archived[j].Slug })
	for _, a := range archived {
		resp.Archived = append(resp.Archived, s.archivedAgentSummary(a))
	}
	writeJSON(w, http.StatusOK, resp)
}

// handleAPIAgent serves GET /api/v1/agents/{slug}: the summary plus
// what the chat header, composer and tabs need. An archived agent is
// served too, marked archived. The CEO is not an agent here.
func (s *Server) handleAPIAgent(w http.ResponseWriter, r *http.Request) {
	slug := r.PathValue("slug")
	if !plainSlug(slug) || slug == agent.CEOSlug {
		writeAPIError(w, http.StatusNotFound, "no agent is called "+slug, "whoever sent you the link")
		return
	}
	a, err := s.Store.GetAgent(slug)
	archived := false
	if errors.Is(err, store.ErrNotFound) {
		a, err = s.Store.GetArchivedAgent(slug)
		archived = true
	}
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeAPIError(w, http.StatusNotFound, "no agent is called "+slug, "whoever sent you the link")
			return
		}
		writeAPIError(w, http.StatusInternalServerError, "the agent could not be read", whoDevelopers)
		return
	}

	var summary apitypes.AgentSummary
	if archived {
		summary = s.archivedAgentSummary(a)
	} else {
		summary, err = s.activeAgentSummary(slug)
		if err != nil {
			writeAPIError(w, http.StatusInternalServerError, "the team could not be read", whoDevelopers)
			return
		}
	}

	role, err := s.Store.ReadRole(slug)
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		writeAPIError(w, http.StatusInternalServerError, "the agent's role could not be read", whoDevelopers)
		return
	}
	hist, err := s.Store.ReadChatHistory(slug)
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		writeAPIError(w, http.StatusInternalServerError, "the agent's chat could not be read", whoDevelopers)
		return
	}
	pastChats, err := s.Store.ListArchivedChats(slug)
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, "the agent's past chats could not be read", whoDevelopers)
		return
	}

	// The same math as the snapshot's context_pct (contextFillForHistory)
	// and the chat API's: no history reads as empty.
	cw, _ := s.Store.ReadContextWindow(slug)
	stats := s.computeChatFillStats(a.Model, s.AgentModel, hist, cw.ContextTokens)
	fill := apitypes.ContextFill{Limit: stats.ContextLimit}
	if len(hist) > 0 && stats.ContextLimit > 0 {
		fill.Pct, fill.Tokens = stats.FillPct, stats.TokenEst
	}

	writeJSON(w, http.StatusOK, apitypes.AgentDetail{
		AgentSummary: summary,
		RoleLine:     roleLine(role, a),
		Models:       s.modelOptions(summary.Model, a.Effort),
		Context:      fill,
		Counts: apitypes.AgentCounts{
			Background: s.SubagentService.OutstandingSubagents(slug),
			PastChats:  len(pastChats),
		},
		Archived: archived,
	})
}

// plainSlug reports whether a {slug} path value is one the store may
// be asked about. The mux keeps an encoded slash inside one segment
// and hands it to PathValue decoded, and the store joins the slug
// straight into a path under agents/, so "_archived/x" would read an
// archived agent as active and "../x" would leave the directory. A
// slug is one segment with no separators, not a dotfile, and not the
// archive directory.
func plainSlug(slug string) bool {
	return slug != "" &&
		!strings.ContainsAny(slug, `/\`) &&
		!strings.HasPrefix(slug, ".") &&
		!strings.HasPrefix(slug, "_")
}

// activeAgentSummary is one active agent's summary, with the depth and
// state the full list would give it.
func (s *Server) activeAgentSummary(slug string) (apitypes.AgentSummary, error) {
	actives, err := s.Store.ListActiveAgents()
	if err != nil {
		return apitypes.AgentSummary{}, err
	}
	working := s.workingAgents()
	for _, n := range agentTreeOrder(actives) {
		if n.Agent.Slug == slug {
			return s.agentSummary(n.Agent, s.agentLivenessFor(n, working)), nil
		}
	}
	return apitypes.AgentSummary{}, store.ErrNotFound
}

// agentSummary joins an active agent's record with its liveness.
// Model and effort are what the agent runs on: its own pin, else the
// fleet default.
func (s *Server) agentSummary(a store.Agent, l agentLiveness) apitypes.AgentSummary {
	return apitypes.AgentSummary{
		Slug:       a.Slug,
		Name:       l.Name,
		RoleTitle:  l.RoleTitle,
		Icon:       l.Icon,
		ReportsTo:  l.ReportsTo,
		Depth:      l.Depth,
		Model:      cmp.Or(a.Model, s.AgentModel),
		ModelLabel: s.modelLabel(cmp.Or(a.Model, s.AgentModel)),
		Effort:     s.effectiveEffort(cmp.Or(a.Model, s.AgentModel), a.Effort),
		ContextPct: l.ContextPct,
		State:      l.State,
		Created:    a.CreatedAt,
	}
}

// archivedAgentSummary is an archived agent's summary: it has no place
// in the tree and nothing running, so depth 0 and state idle.
func (s *Server) archivedAgentSummary(a store.Agent) apitypes.AgentSummary {
	return apitypes.AgentSummary{
		Slug:       a.Slug,
		Name:       agentDisplayName(a),
		RoleTitle:  a.Role,
		Icon:       a.Icon,
		ReportsTo:  cmp.Or(a.ReportsTo, agent.CEOSlug),
		Model:      cmp.Or(a.Model, s.AgentModel),
		ModelLabel: s.modelLabel(cmp.Or(a.Model, s.AgentModel)),
		Effort:     s.effectiveEffort(cmp.Or(a.Model, s.AgentModel), a.Effort),
		State:      apitypes.AgentStateIdle,
		Created:    a.CreatedAt,
		ArchivedAt: a.ArchivedAt,
	}
}

// roleLine is the one line under the agent's name: the first non-blank
// line of role.md with any markdown heading marks removed, else the
// role title, else the slug.
func roleLine(role string, a store.Agent) string {
	for _, line := range strings.Split(role, "\n") {
		line = strings.TrimSpace(strings.TrimLeft(strings.TrimSpace(line), "#"))
		if line != "" {
			return line
		}
	}
	return agentDisplayName(a)
}
