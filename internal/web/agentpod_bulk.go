package web

import (
	"context"
	"log"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/kivali-ai/kivali/internal/agent"
)

// bulkProvisionBackoff is how long BulkProvisionAgentPods waits before
// provisioning again the agents whose last attempt failed; each wait
// doubles the last, up to bulkProvisionBackoffMax. Right after the node
// boots, the API server's service address can go unanswered for tens of
// seconds while kube-proxy and CoreDNS come up, so the first wave can
// lose most of the fleet to the kube client's timeout; the retries
// bring those agents up within the caller's deadline instead of leaving
// them without a pod until the next server start.
const (
	bulkProvisionBackoff    = 5 * time.Second
	bulkProvisionBackoffMax = time.Minute
)

// BulkProvisionAgentPods provisions the pod of every active agent (sans
// CEO): a first wave in PARALLEL, then the agents whose Provision failed
// again after each bulkProvisionBackoff wait, until every pod exists or
// ctx ends. Each retry takes only the failed agents that are still
// active. Per-agent errors are logged and the other goroutines keep
// running — one cluster-side hiccup must not block the rest.
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
// catch-up path. It returns only once every agent has a pod or ctx
// ends: the callers wake recovered agents next, and a wake needs the
// agent's pod listening.
//
// No-op when AgentPod is unwired or the provisioner reports
// !Enabled() (out-of-cluster development).
func (s *Server) BulkProvisionAgentPods(ctx context.Context) {
	if s.AgentPod == nil {
		return
	}
	slugs, err := s.activeAgentPodSlugs(nil)
	if err != nil {
		log.Printf("agentpod: list agents: %v", err)
		return
	}
	wait := bulkProvisionBackoff
	for len(slugs) > 0 {
		failed := s.provisionAgentPods(ctx, slugs)
		if len(failed) == 0 {
			return
		}
		if ctx.Err() != nil {
			log.Printf("agentpod: no pod for %s (%v)", strings.Join(failed, ", "), ctx.Err())
			return
		}
		log.Printf("agentpod: %d of %d pods not provisioned; trying again in %s", len(failed), len(slugs), wait)
		timer := s.clk().NewTimer(wait)
		select {
		case <-timer.C():
		case <-ctx.Done():
			timer.Stop()
			log.Printf("agentpod: no pod for %s (%v)", strings.Join(failed, ", "), ctx.Err())
			return
		}
		// An agent offboarded during the wait is not provisioned
		// again: its pod would be recreated, and the sync would
		// remake agents/<slug> without an agent.yaml.
		slugs, err = s.activeAgentPodSlugs(failed)
		if err != nil {
			log.Printf("agentpod: list agents: %v", err)
			return
		}
		wait = min(2*wait, bulkProvisionBackoffMax)
	}
}

// activeAgentPodSlugs returns the slugs of the active agents that have
// a pod (every one but the CEO), sorted. A non-nil only narrows them to the
// slugs it holds.
func (s *Server) activeAgentPodSlugs(only []string) ([]string, error) {
	agents, err := s.Store.ListActiveAgents()
	if err != nil {
		return nil, err
	}
	var slugs []string
	for _, a := range agents {
		if a.Slug == agent.CEOSlug {
			continue
		}
		if only != nil && !slices.Contains(only, a.Slug) {
			continue
		}
		slugs = append(slugs, a.Slug)
	}
	return slugs, nil
}

// provisionAgentPods provisions the pods of slugs in parallel and
// returns, sorted, the slugs whose Provision failed.
func (s *Server) provisionAgentPods(ctx context.Context, slugs []string) []string {
	var (
		wg     sync.WaitGroup
		mu     sync.Mutex
		failed []string
	)
	for _, slug := range slugs {
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
				mu.Lock()
				failed = append(failed, slug)
				mu.Unlock()
				return
			}
			log.Printf("agentpod: provisioned %s", slug)
		}()
	}
	wg.Wait()
	slices.Sort(failed)
	return failed
}
