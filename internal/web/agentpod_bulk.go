package web

import (
	"context"
	"errors"
	"fmt"
	"log"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/kivali-ai/kivali/internal/agent"
	"github.com/kivali-ai/kivali/internal/store"
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
// Each agent's startup hold (HoldTurnsForStartup) is released once its
// pod is provisioned, and every hold left when this returns, so no
// agent's turns stay held past the startup pass.
//
// No-op when AgentPod is unwired or the provisioner reports
// !Enabled() (out-of-cluster development).
func (s *Server) BulkProvisionAgentPods(ctx context.Context) {
	defer s.releaseStartupHolds()
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
			// The server has just started, so no pod is subscribed yet:
			// every agent is starting until its pod dials in, also the
			// ones whose pods keep running.
			if s.AgentpodHub != nil {
				s.AgentpodHub.MarkStarting(slug, s.clk().Now())
			}
			if err := s.ProvisionAgentPod(ctx, slug); err != nil {
				log.Printf("agentpod: provision %s: %v", slug, err)
				mu.Lock()
				failed = append(failed, slug)
				mu.Unlock()
				return
			}
			log.Printf("agentpod: provisioned %s", slug)
			s.releaseStartupHold(slug)
		}()
	}
	wg.Wait()
	slices.Sort(failed)
	return failed
}

// ProvisionAgentPod reconciles one agent's pod with its current spec:
// a pod whose spec changed (its software, say) is recreated, one that
// matches is left alone. Blocks until the pod is created.
//
// Sync before the pod: the pod mounts subPaths of the agent's tree
// (the /files farms, artifacts/, the per-agent attachments and chats
// dirs), and kubelet makes a missing one as root, which core — not
// root — can then never write into. Sync makes each a real directory
// core owns. A sync that fails still provisions: an existing agent
// without its pod is worse off than one whose views are stale.
//
// The agent is marked starting only when its pod is about to be
// created or recreated (podStartNotifier), so reconciling every agent
// after an org-wide change leaves the ones whose pods keep running as
// they were. An AgentPod that cannot say marks it every time. From
// that point a turn the old pod's runtime loses is Kivali's doing, a
// Planned disruption (markRestartPlanned). ProvisionAgentPod does not
// wait for the agent to be idle; ReprovisionAgentPod does.
//
// Calls for one slug run one at a time: two settings saves in quick
// succession, an approval landing during a settings reprovision, or the
// boot pass racing either. A later call waits for the earlier one, then
// builds the spec from the lists as they are by then, so it sees the
// earlier call's pod and the later change as drift and applies it. An
// offboard holds the same lock while it archives the agent and deletes
// its pod (lockProvision), so a provision either finishes before the
// archive, and the offboard then deletes its pod, or starts after it
// and finds the agent gone: it neither syncs (which would remake
// agents/<slug>) nor creates a pod.
//
// No-op when AgentPod is unwired, or the agent is not active.
func (s *Server) ProvisionAgentPod(ctx context.Context, slug string) error {
	if s.AgentPod == nil {
		return nil
	}
	defer s.lockProvision(slug)()
	if _, err := s.Store.GetAgent(slug); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			log.Printf("agentpod: %s is not an active agent; no pod", slug)
			return nil
		}
		return fmt.Errorf("read agent: %w", err)
	}
	if err := s.Store.SyncAgentFilesystem(slug); err != nil {
		log.Printf("agentpod: files sync %s before provisioning: %v", slug, err)
	}
	starting := func() {
		s.markRestartPlanned(slug)
		if s.AgentpodHub != nil {
			s.AgentpodHub.MarkStarting(slug, s.clk().Now())
		}
	}
	if n, ok := s.AgentPod.(podStartNotifier); ok {
		return n.ProvisionNotify(ctx, slug, starting)
	}
	starting()
	return s.AgentPod.Provision(ctx, slug)
}

// lockProvision takes slug's provision lock and returns its unlock:
// ProvisionAgentPod and the offboard's archive and pod teardown run one
// at a time for an agent.
func (s *Server) lockProvision(slug string) (unlock func()) {
	mu, _ := s.provisionLocks.LoadOrStore(slug, &sync.Mutex{})
	mu.(*sync.Mutex).Lock()
	return mu.(*sync.Mutex).Unlock
}

// agentProvisioner is the agent.Provisioner the hire path uses: the
// server's ProvisionAgentPod, so a hire takes the same per-agent lock as
// every other provision.
type agentProvisioner struct{ s *Server }

func (p agentProvisioner) Provision(ctx context.Context, slug string) error {
	return p.s.ProvisionAgentPod(ctx, slug)
}

// podStartNotifier is an AgentPodLifecycle that reports when it is
// about to create an agent's pod: *agentpod.Provisioner.
type podStartNotifier interface {
	ProvisionNotify(ctx context.Context, slug string, starting func()) error
}

// ReprovisionAgentPodsAsync runs ReprovisionAgentPod for each slug in
// the background, in parallel, and returns at once: a settings change
// that alters pod specs answers the request before the pods have
// restarted, and an agent at work restarts once its work has ended.
// The CEO has no pod and is skipped.
func (s *Server) ReprovisionAgentPodsAsync(slugs []string) {
	if s.AgentPod == nil {
		return
	}
	for _, slug := range slugs {
		if slug == agent.CEOSlug {
			continue
		}
		go func() {
			if _, err := s.ReprovisionAgentPod(context.Background(), slug, nil); err != nil {
				log.Printf("agentpod: reprovision %s: %v", slug, err)
			}
		}()
	}
}
