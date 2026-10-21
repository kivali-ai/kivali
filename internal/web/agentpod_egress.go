package web

import (
	"log"
	"net/http"

	"github.com/kivali-ai/kivali/internal/agentpod"
	"github.com/kivali-ai/kivali/internal/egresscred"
	"github.com/kivali-ai/kivali/internal/message"
)

// handleAgentpodEgressCredential serves POST /v1/egress/credential: an
// agent runtime registering the SHA-256 of the egress proxy secrets it
// drew at start (agentpod.SetUpEgressCredential), its own and, when it
// has a shell, the shell's. The agent is the slug header, as on every
// core.sock route; it must name an active agent. The hashes replace the
// agent's earlier ones, a registration without a shell hash dropping
// the earlier shell hash, and reach the proxy through the agents sync
// file. 204 on success, 400 on a malformed hash or a shell hash equal
// to the runtime's, which would give the shell the runtime's open
// access.
func (s *Server) handleAgentpodEgressCredential(w http.ResponseWriter, r *http.Request) {
	slug := agentpod.SlugFromHeader(r.Header)
	if slug == "" || message.ValidateSlug(slug) != nil {
		http.Error(w, "the "+agentpod.SlugHeader+" header must name an agent", http.StatusForbidden)
		return
	}
	if s.Store == nil {
		http.Error(w, "store not configured", http.StatusServiceUnavailable)
		return
	}
	if _, err := s.Store.GetAgent(slug); err != nil {
		http.Error(w, "no active agent "+slug, http.StatusNotFound)
		return
	}
	var req agentpod.EgressCredentialRequest
	if err := decodeJSONBounded(w, r, &req); err != nil {
		http.Error(w, "body: "+err.Error(), http.StatusBadRequest)
		return
	}
	if !egresscred.ValidHash(req.SHA256) {
		http.Error(w, "sha256 must be 64 lowercase hex characters", http.StatusBadRequest)
		return
	}
	if req.ShellSHA256 != "" && !egresscred.ValidHash(req.ShellSHA256) {
		http.Error(w, "shell_sha256 must be 64 lowercase hex characters", http.StatusBadRequest)
		return
	}
	if req.ShellSHA256 == req.SHA256 {
		http.Error(w, "shell_sha256 must differ from sha256", http.StatusBadRequest)
		return
	}
	if err := s.Store.SetEgressAgentCredential(slug, req.SHA256); err != nil {
		log.Printf("egress credential %s: %v", slug, err)
		http.Error(w, "could not record the credential", http.StatusInternalServerError)
		return
	}
	if err := s.Store.SetEgressShellCredential(slug, req.ShellSHA256); err != nil {
		log.Printf("egress shell credential %s: %v", slug, err)
		http.Error(w, "could not record the shell's credential", http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
