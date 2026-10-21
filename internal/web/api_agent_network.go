package web

import (
	"errors"
	"net/http"

	"github.com/kivali-ai/kivali/internal/store"
	"github.com/kivali-ai/kivali/internal/web/apitypes"
)

// An agent's own network settings, on its About tab under Security:
// the hosts its shell commands reach on top of the team's list, and the
// switch that lifts its network limits. A write changes the agent's
// agent.yaml and the egress proxy's agents file (store), which the
// proxy hot-reloads, so it applies to the agent's next request with no
// pod restart. Every answer is the agent's settings as they now stand,
// with the team's list beside them.

// wireAPIAgentNetworkRoutes mounts the agent network routes on the API mux.
func (s *Server) wireAPIAgentNetworkRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/v1/agents/{slug}/network", s.handleAPIAgentNetwork)
	mux.HandleFunc("POST /api/v1/agents/{slug}/network", s.handleAPIAgentNetworkAdd)
	mux.HandleFunc("DELETE /api/v1/agents/{slug}/network/{host}", s.handleAPIAgentNetworkRemove)
	mux.HandleFunc("POST /api/v1/agents/{slug}/network/exempt", s.handleAPIAgentNetworkExempt)
}

// writeAgentNetwork answers with slug's settings and the team's list.
func (s *Server) writeAgentNetwork(w http.ResponseWriter, slug string) {
	team, err := s.networkHosts()
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, "the team's network list could not be read", whoDevelopers)
		return
	}
	a, err := s.Store.GetAgent(slug)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeAgentNotFound(w, slug)
			return
		}
		writeAPIError(w, http.StatusInternalServerError, "the agent could not be read", whoDevelopers)
		return
	}
	writeJSON(w, http.StatusOK, apitypes.AgentNetwork{
		Team:   team,
		Own:    store.CleanEgressHosts(a.NetworkHosts),
		Exempt: a.NetworkExempt,
	})
}

// writeAgentNetworkError answers a write that failed for a reason other
// than the request's host: the agent is gone, or the save did not
// complete.
func writeAgentNetworkError(w http.ResponseWriter, slug string, err error) {
	if errors.Is(err, store.ErrNotFound) {
		writeAgentNotFound(w, slug)
		return
	}
	writeAPIError(w, http.StatusInternalServerError, "the agent's network settings could not be saved", whoDevelopers)
}

func (s *Server) handleAPIAgentNetwork(w http.ResponseWriter, r *http.Request) {
	slug, ok := s.softwareSlug(w, r)
	if !ok {
		return
	}
	s.writeAgentNetwork(w, slug)
}

func (s *Server) handleAPIAgentNetworkAdd(w http.ResponseWriter, r *http.Request) {
	slug, ok := s.softwareSlug(w, r)
	if !ok {
		return
	}
	var req apitypes.NetworkAdd
	if !decodeJSONOrError(w, r, &req) {
		return
	}
	if _, err := s.Store.AddAgentNetworkHost(slug, req.Host); err != nil {
		switch {
		case errors.Is(err, store.ErrInvalidEgressHost):
			writeInvalidHost(w, req.Host)
		case errors.Is(err, store.ErrNetworkHostListed):
			writeAPIError(w, http.StatusConflict, store.NormalizeEgressHost(req.Host)+" is already on this agent's network list", whoNoOne)
		default:
			writeAgentNetworkError(w, slug, err)
		}
		return
	}
	s.writeAgentNetwork(w, slug)
}

func (s *Server) handleAPIAgentNetworkRemove(w http.ResponseWriter, r *http.Request) {
	slug, ok := s.softwareSlug(w, r)
	if !ok {
		return
	}
	host := store.NormalizeEgressHost(r.PathValue("host"))
	if _, err := s.Store.RemoveAgentNetworkHost(slug, host); err != nil {
		if errors.Is(err, store.ErrNetworkHostNotListed) {
			writeAPIError(w, http.StatusNotFound, host+" is not on this agent's network list", whoNoOne)
			return
		}
		writeAgentNetworkError(w, slug, err)
		return
	}
	s.writeAgentNetwork(w, slug)
}

func (s *Server) handleAPIAgentNetworkExempt(w http.ResponseWriter, r *http.Request) {
	slug, ok := s.softwareSlug(w, r)
	if !ok {
		return
	}
	var req apitypes.AgentNetworkExempt
	if !decodeJSONOrError(w, r, &req) {
		return
	}
	if err := s.Store.SetAgentNetworkExempt(slug, req.Exempt); err != nil {
		writeAgentNetworkError(w, slug, err)
		return
	}
	s.writeAgentNetwork(w, slug)
}
