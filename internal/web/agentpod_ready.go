package web

import (
	"encoding/json"
	"log"
	"net/http"
)

// agentPodsReadiness is GET /readyz/agents's body: how many agents have
// a pod (every active agent but the CEO), and how many of those pods'
// runtimes are connected to core and taking events on the spec this
// server provisioned them with. The supervisor's team update waits for
// the two to match before it commits (internal/supervisor, waitAgents).
type agentPodsReadiness struct {
	AgentPods int `json:"agent_pods"`
	Connected int `json:"connected"`
}

// markPodProvisioned records that this process has provisioned slug's
// pod: it now runs on the current spec, created, recreated or left
// running because it already matched.
func (s *Server) markPodProvisioned(slug string) {
	s.podsProvisionedMu.Lock()
	defer s.podsProvisionedMu.Unlock()
	if s.podsProvisioned == nil {
		s.podsProvisioned = map[string]bool{}
	}
	s.podsProvisioned[slug] = true
}

// agentPodsReadiness counts the active agents that have a pod, and those
// whose pod this process has provisioned and whose runtime has
// subscribed to its events since the pod was last created or recreated.
// A runtime that subscribed before a recreate is the old pod's and does
// not count. Without agent pods (no provisioner, or one that is not in a
// cluster) there are none to count.
func (s *Server) agentPodsReadiness() (agentPodsReadiness, error) {
	var r agentPodsReadiness
	if s.AgentPod == nil || s.AgentpodHub == nil {
		return r, nil
	}
	if e, ok := s.AgentPod.(interface{ Enabled() bool }); ok && !e.Enabled() {
		return r, nil
	}
	slugs, err := s.activeAgentPodSlugs(nil)
	if err != nil {
		return r, err
	}
	s.podsProvisionedMu.Lock()
	provisioned := make(map[string]bool, len(slugs))
	for _, slug := range slugs {
		provisioned[slug] = s.podsProvisioned[slug]
	}
	s.podsProvisionedMu.Unlock()
	r.AgentPods = len(slugs)
	for _, slug := range slugs {
		if provisioned[slug] && s.AgentpodHub.ConnectedSincePodStart(slug) {
			r.Connected++
		}
	}
	return r, nil
}

// handleReadyzAgents answers 200 once every agent pod's runtime is
// connected (agentPodsReadiness), 503 until then; the body carries the
// two counts either way. Unauthenticated like /readyz, so it names no
// agent. Not a probe: an agent pod that never connects leaves the server
// itself serving.
func (s *Server) handleReadyzAgents(w http.ResponseWriter, _ *http.Request) {
	r, err := s.agentPodsReadiness()
	if err != nil {
		log.Printf("readyz/agents: %v", err)
		http.Error(w, "agents unreadable", http.StatusServiceUnavailable)
		return
	}
	code := http.StatusOK
	if r.Connected < r.AgentPods {
		code = http.StatusServiceUnavailable
	}
	w.Header().Set("content-type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(r)
}
