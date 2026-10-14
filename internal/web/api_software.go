package web

import (
	"errors"
	"fmt"
	"net/http"

	"github.com/kivali-ai/kivali/internal/store"
	"github.com/kivali-ai/kivali/internal/web/apitypes"
)

// The software lists: Debian packages installed into agents' shells
// when their pods start. The org-wide list reaches every agent, an
// agent's own list only that agent. A write adds or removes names on
// the list as it stands when the write lands, so two pages open at once
// cannot undo each other's changes, and re-provisions the pods it
// reaches in the background, so the answer comes back before the
// shells have restarted.

// wireAPISoftwareRoutes mounts the software routes on the API mux.
func (s *Server) wireAPISoftwareRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/v1/org/software", s.handleAPIOrgSoftware)
	mux.HandleFunc("POST /api/v1/org/software", s.handleAPIOrgSoftwareAdd)
	mux.HandleFunc("DELETE /api/v1/org/software/{name}", s.handleAPIOrgSoftwareRemove)
	mux.HandleFunc("GET /api/v1/agents/{slug}/software", s.handleAPIAgentSoftware)
	mux.HandleFunc("POST /api/v1/agents/{slug}/software", s.handleAPIAgentSoftwareAdd)
	mux.HandleFunc("DELETE /api/v1/agents/{slug}/software/{name}", s.handleAPIAgentSoftwareRemove)
}

// softwareNames normalizes a request's package list. A name that is not
// a Debian package name, or no name at all, is a 400; the error is
// written.
func softwareNames(w http.ResponseWriter, packages []string) ([]string, bool) {
	clean, err := store.NormalizeSoftware(packages)
	if err != nil {
		var msg string
		if errors.Is(err, store.ErrInvalidSoftware) {
			msg = fmt.Sprintf("%v. Use the package's name as Debian lists it, for example ffmpeg or poppler-utils: lowercase letters, digits, and the characters + - .", err)
		} else {
			msg = err.Error()
		}
		writeAPIError(w, http.StatusBadRequest, msg, whoYou)
		return nil, false
	}
	if len(clean) == 0 {
		writeAPIError(w, http.StatusBadRequest, "name at least one package to add", whoYou)
		return nil, false
	}
	return clean, true
}

func (s *Server) handleAPIOrgSoftware(w http.ResponseWriter, _ *http.Request) {
	list, err := s.Store.ReadOrgSoftware()
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, "the software list could not be read", whoDevelopers)
		return
	}
	writeJSON(w, http.StatusOK, orgSoftware(list))
}

func (s *Server) handleAPIOrgSoftwareAdd(w http.ResponseWriter, r *http.Request) {
	var req apitypes.SoftwareAdd
	if !decodeJSONOrError(w, r, &req) {
		return
	}
	clean, ok := softwareNames(w, req.Packages)
	if !ok {
		return
	}
	list, err := s.Store.AddOrgSoftware(clean)
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, "the software list could not be saved", whoDevelopers)
		return
	}
	s.reprovisionEveryAgent()
	writeJSON(w, http.StatusOK, orgSoftware(list))
}

func (s *Server) handleAPIOrgSoftwareRemove(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	list, err := s.Store.RemoveOrgSoftware(name)
	switch {
	case errors.Is(err, store.ErrSoftwareNotListed):
		writeAPIError(w, http.StatusNotFound, name+" is not on the software list", whoNoOne)
		return
	case err != nil:
		writeAPIError(w, http.StatusInternalServerError, "the software list could not be saved", whoDevelopers)
		return
	}
	s.reprovisionEveryAgent()
	writeJSON(w, http.StatusOK, orgSoftware(list))
}

// reprovisionEveryAgent re-provisions every active agent's pod in the
// background, after an org-wide list change.
func (s *Server) reprovisionEveryAgent() {
	actives, err := s.Store.ListActiveAgents()
	if err != nil {
		return
	}
	slugs := make([]string, 0, len(actives))
	for _, a := range actives {
		slugs = append(slugs, a.Slug)
	}
	s.ReprovisionAgentPodsAsync(slugs)
}

// softwareSlug resolves {slug} to a live agent with a shell. Archived
// agents are not found, as for the documents; the CEO has no shell.
// Writes the error itself.
func (s *Server) softwareSlug(w http.ResponseWriter, r *http.Request) (string, bool) {
	slug, ok := agentWorkSlug(w, r)
	if !ok {
		return "", false
	}
	if _, err := s.Store.GetAgent(slug); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeAgentNotFound(w, slug)
			return "", false
		}
		writeAPIError(w, http.StatusInternalServerError, "the agent could not be read", whoDevelopers)
		return "", false
	}
	return slug, true
}

func writeAgentNotFound(w http.ResponseWriter, slug string) {
	writeAPIError(w, http.StatusNotFound, "no agent on the team is called "+slug, "whoever sent you the link")
}

func (s *Server) writeAgentSoftware(w http.ResponseWriter, slug string) {
	org, err := s.Store.ReadOrgSoftware()
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, "the software list could not be read", whoDevelopers)
		return
	}
	a, err := s.Store.GetAgent(slug)
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, "the agent could not be read", whoDevelopers)
		return
	}
	own := store.CleanSoftware(a.Software)
	writeJSON(w, http.StatusOK, apitypes.AgentSoftware{
		Org:     nonNilStrings(org),
		Own:     own,
		Invalid: store.InvalidSoftware(store.CleanSoftware(append(append([]string{}, org...), own...))),
	})
}

// orgSoftware is the org-wide list as the API answers it.
func orgSoftware(list []string) apitypes.OrgSoftware {
	return apitypes.OrgSoftware{Packages: nonNilStrings(list), Invalid: store.InvalidSoftware(list)}
}

func (s *Server) handleAPIAgentSoftware(w http.ResponseWriter, r *http.Request) {
	slug, ok := s.softwareSlug(w, r)
	if !ok {
		return
	}
	s.writeAgentSoftware(w, slug)
}

func (s *Server) handleAPIAgentSoftwareAdd(w http.ResponseWriter, r *http.Request) {
	slug, ok := s.softwareSlug(w, r)
	if !ok {
		return
	}
	var req apitypes.SoftwareAdd
	if !decodeJSONOrError(w, r, &req) {
		return
	}
	clean, ok := softwareNames(w, req.Packages)
	if !ok {
		return
	}
	switch err := s.Store.AddAgentSoftware(slug, clean); {
	case errors.Is(err, store.ErrNotFound):
		writeAgentNotFound(w, slug)
		return
	case err != nil:
		writeAPIError(w, http.StatusInternalServerError, "the software list could not be saved", whoDevelopers)
		return
	}
	s.ReprovisionAgentPodsAsync([]string{slug})
	s.writeAgentSoftware(w, slug)
}

func (s *Server) handleAPIAgentSoftwareRemove(w http.ResponseWriter, r *http.Request) {
	slug, ok := s.softwareSlug(w, r)
	if !ok {
		return
	}
	name := r.PathValue("name")
	switch err := s.Store.RemoveAgentSoftware(slug, name); {
	case errors.Is(err, store.ErrNotFound):
		writeAgentNotFound(w, slug)
		return
	case errors.Is(err, store.ErrSoftwareNotListed):
		writeAPIError(w, http.StatusNotFound, name+" is not on this agent's software list", whoNoOne)
		return
	case err != nil:
		writeAPIError(w, http.StatusInternalServerError, "the software list could not be saved", whoDevelopers)
		return
	}
	s.ReprovisionAgentPodsAsync([]string{slug})
	s.writeAgentSoftware(w, slug)
}

// nonNilStrings is list, or an empty list for nil: the wire never
// carries null for a list.
func nonNilStrings(list []string) []string {
	if list == nil {
		return []string{}
	}
	return list
}
