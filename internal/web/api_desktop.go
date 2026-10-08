package web

import (
	"net/http"

	"github.com/kivali-ai/kivali/internal/auth"
	"github.com/kivali-ai/kivali/internal/web/apitypes"
)

// GET /api/v1/desktop/facts: what Kivali Desktop shows about a team it
// cannot see inside (docs/developers/desktop-app.md, "The team API"): the tray's
// "3 agents working", Settings' "Signed in as …", and the delete
// dialog's "6 agents and what they remember · 1,204 files". The app
// reads it with the session cookie of the team's own window, so it
// answers as the person signed in there, behind the same middleware as
// every other API route. The web app does not read it, so its shape is
// declared here rather than in apitypes (which tygo turns into the web
// app's types).

// DesktopFacts is the answer.
type DesktopFacts struct {
	// Email is the signed-in person's, from the session.
	Email string `json:"email"`
	// Agents counts the hired agents, the person's own "ceo" seat
	// excluded: the agents the web app lists.
	Agents int `json:"agents"`
	// Working counts the agents the web app's Home calls working: a
	// turn running, or background work still out (Readouts.Working).
	Working int `json:"working"`
	// Files counts the team's project files: what the web app's Files
	// page lists (the person's uploads, GET /api/v1/org/files), not the
	// agents' private workspaces.
	Files int `json:"files"`
}

func (s *Server) wireAPIDesktopRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/v1/desktop/facts", s.handleAPIDesktopFacts)
}

func (s *Server) handleAPIDesktopFacts(w http.ResponseWriter, r *http.Request) {
	agents := s.agentLivenessList()
	files, err := s.Store.ListProjectFiles()
	if err != nil {
		writeRefusalAPI(w, err)
		return
	}
	writeJSON(w, http.StatusOK, DesktopFacts{
		Email:   auth.UserFromContext(r.Context()),
		Agents:  len(agents),
		Working: countWorking(agents),
		Files:   len(files),
	})
}

// countWorking is the snapshot's Readouts.Working rule (org_state.go):
// running, or waiting on background tasks.
func countWorking(agents []agentLiveness) int {
	n := 0
	for _, a := range agents {
		if a.State == apitypes.AgentStateRunning || a.WaitingTasks > 0 {
			n++
		}
	}
	return n
}
