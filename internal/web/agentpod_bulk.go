package web

import (
	"context"
	"log"
	"sync"

	"github.com/kivali-ai/kivali/internal/agent"
)

// BulkProvisionAgentPods iterates active agents (sans CEO) and fires
// AgentPod.Provision for each in PARALLEL. Per-agent errors are
// logged and other goroutines keep running — one cluster-side
// hiccup must not block the rest.
//
// Parallel because Provision is reconcile-style: when Kivali web boots
// after an agentpod-spec change, every existing
// pod is drift; each Provision call does delete + wait-for-termination
// + recreate, dominated by terminationGracePeriodSeconds (~10s). At
// the prod fleet size (~12 agents) sequential takes minutes; parallel
// takes ~one-pod-time. K8s handles concurrent per-pod operations
// independently — different slugs touch different objects, no cross-
// agent shared state in Provision.
//
// Intended to run once at startup so already-hired agents get their
// pods without a manual re-hire. Hire-time provisioning still
// runs through ApplyHire's normal Provision call; this is the
// catch-up path.
//
// No-op when AgentPod is unwired or the provisioner reports
// !Enabled() (out-of-cluster development).
func (s *Server) BulkProvisionAgentPods(ctx context.Context) {
	if s.AgentPod == nil {
		return
	}
	agents, err := s.Store.ListActiveAgents()
	if err != nil {
		log.Printf("agentpod: list agents: %v", err)
		return
	}
	var wg sync.WaitGroup
	for _, a := range agents {
		if a.Slug == agent.CEOSlug {
			continue
		}
		slug := a.Slug
		wg.Add(1)
		go func() {
			defer wg.Done()
			// Sync before the pod: the pod mounts subPaths of the
			// agent's tree (the /files farms, artifacts/, the per-agent
			// attachments and chats dirs), and kubelet makes a missing
			// one as root, which core — not root — can then never write
			// into. Sync makes each a real directory core owns. A sync
			// that fails still provisions: an existing agent without
			// its pod is worse off than one whose views are stale.
			if s.AgentpodHub != nil {
				s.AgentpodHub.MarkStarting(slug, s.clk().Now())
			}
			if err := s.Store.SyncAgentFilesystem(slug); err != nil {
				log.Printf("agentpod: files sync %s before provisioning: %v", slug, err)
			}
			if err := s.AgentPod.Provision(ctx, slug); err != nil {
				log.Printf("agentpod: provision %s: %v", slug, err)
				return
			}
			log.Printf("agentpod: provisioned %s", slug)
		}()
	}
	wg.Wait()
}
