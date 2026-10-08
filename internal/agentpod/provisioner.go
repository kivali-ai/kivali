package agentpod

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/kivali-ai/kivali/internal/provider"
)

// ProvisionerConfig controls per-agent Pod + PVC provisioning. The
// shape mirrors the Kivali web pod's deployment: same Namespace,
// same Image (the Kivali binary has every subcommand including
// `kivali agent`), same SA token discipline (automount disabled).
//
// PodAffinity is the load-bearing field — agent pods must be
// scheduled on the same node as Kivali web so the hostPath UDS dir
// is visible to both. Without this, the UDS socket file simply
// doesn't exist on the agent pod's node and the runtime fails to
// dial core.
type ProvisionerConfig struct {
	// Namespace where agent pods live. Must match the Kivali web
	// pod's namespace (RBAC is namespace-scoped).
	Namespace string

	// Image is the container image for the agent pod (typically the
	// same Kivali image used by web). The container runs
	// `kivali agent --slug=<slug> --uds=<socket>`. The dev-shell
	// sidecar image is derived from this — see DevShellImageFor.
	Image string

	// ImagePullPolicy (IfNotPresent for kind, Always for prod).
	ImagePullPolicy string

	// StorageClassName for the per-agent PVC. Empty → cluster default.
	StorageClassName string

	// ScratchSize is the PVC capacity request (e.g. "2Gi"). Holds
	// /scratch — ephemeral working space + optional cache. Survives
	// pod restarts; recreating from scratch is by-design safe.
	ScratchSize string

	// ServiceAccountName to mount in the agent pod. Empty → "default".
	// Token automount is disabled regardless; the SA name only matters
	// when external admission webhooks key on it.
	ServiceAccountName string

	// UDSDir is the hostPath directory holding core.sock (mode 0700).
	// Mounted into both Kivali web and every agent pod. Default is
	// the design-doc canonical "/var/run/kivali/uds".
	UDSDir string

	// WebSelector is the matchLabels expression that
	// identifies the Kivali web pod for podAffinity. The agent pod
	// REQUIRES same-node scheduling with a pod matching these
	// labels. The default (DefaultsForNamespace) matches the web pod's
	// label in the chart's Deployment (charts/kivali/templates/deployment.yaml).
	WebSelector map[string]string

	// ProxyURL, when non-empty, is written into each agent pod's env
	// as HTTP_PROXY / HTTPS_PROXY / http_proxy / https_proxy so the
	// claude CLI's outbound HTTPS routes through the egress proxy.
	ProxyURL string

	// CPURequest / MemoryRequest / CPULimit / MemoryLimit override
	// the resource requests + limits on the container. Empty → use
	// agentpod defaults (50m/128Mi req, 2/4Gi limit). Memory limit
	// is what protects against runaway CLI growth — kubelet kills
	// via cgroup. The 4Gi default already accounts for the parent
	// CLI plus a full SubagentMaxBatch fan-out; override it upward
	// only when a measured OOM says so.
	CPURequest    string
	MemoryRequest string
	CPULimit      string
	MemoryLimit   string

	// DataClaimName is the PVC that holds /data on Kivali web,
	// mounted into agent pods at a subPath so the embedded `claude`
	// CLI sees the same `~/.claude/` (credentials + settings) the
	// operator signed in to via `kubectl exec -it deploy/kivali -- claude`.
	// Same PVC across all agents is by design — every agent's claude
	// subprocess shares this dir; per-session subdirs are
	// session-id-keyed so concurrent writes don't collide.
	// Default "kivali-data" matches the deployment manifests; empty
	// disables the mount (test fixtures that don't need credentials).
	DataClaimName string
	// HomeSubPath is the subdirectory of DataClaimName the provider
	// keeps its state in (Credentials.HomeDir: "claude-home" for the
	// Claude CLI). The mount surfaces it at /data/<HomeSubPath> in the
	// agent pod, which is where the image points the CLI's HOME, so it
	// resolves to a real, writable location with the same credentials
	// the operator signed in. Required when DataClaimName is set; see
	// UseCredentials.
	HomeSubPath string

	// FilesSubPath, when non-nil, returns the Kivali-PVC-relative
	// subPath that should be mounted RW at /files in the agent pod's
	// dev-shell container: agents/<slug>/memory (files.StorageRoot).
	//
	// When nil, the /files mount + the matching RO globals are all
	// omitted — fixtures + out-of-cluster invocations stay minimal.
	// In production this MUST be set; run_shell and the file_* tools
	// depend on /files/ + /data/project_files + /data/skills + the per-agent
	// /data/attachments mount + the per-agent /data/agents/<slug>/chats
	// mount being live.
	FilesSubPath func(slug string) string
}

// DefaultsForNamespace returns a ProvisionerConfig with sensible
// defaults for the given Kubernetes namespace. Callers typically
// overlay Image + UDSDir from environment / configmap.
func DefaultsForNamespace(namespace string) ProvisionerConfig {
	return ProvisionerConfig{
		Namespace:       namespace,
		ImagePullPolicy: "IfNotPresent",
		ScratchSize:     "2Gi",
		UDSDir:          "/var/run/kivali/uds",
		WebSelector:     map[string]string{"app": "kivali"},
		DataClaimName:   "kivali-data",
	}
}

// UseCredentials takes the home directory an agent pod mounts from the
// provider itself (Credentials.HomeDir: claude-home for the Claude CLI).
// The provider's sign-in lives there, so every agent bills the way the
// server does.
func (c *ProvisionerConfig) UseCredentials(creds provider.Credentials) {
	c.HomeSubPath = creds.HomeDir()
}

func (c ProvisionerConfig) validate() error {
	if strings.TrimSpace(c.Namespace) == "" {
		return fmt.Errorf("agentpod: namespace required")
	}
	if strings.TrimSpace(c.Image) == "" {
		return fmt.Errorf("agentpod: image required")
	}
	if strings.TrimSpace(c.UDSDir) == "" {
		return fmt.Errorf("agentpod: UDSDir required")
	}
	if len(c.WebSelector) == 0 {
		return fmt.Errorf("agentpod: WebSelector required for same-node affinity")
	}
	if c.DataClaimName != "" && strings.TrimSpace(c.HomeSubPath) == "" {
		return fmt.Errorf("agentpod: HomeSubPath required when DataClaimName is set")
	}
	return nil
}

// Provisioner manages the per-agent Pod + PVC lifecycle. Produces
// agent pods bound to core via hostPath UDS, with podAffinity
// enforcing same-node scheduling with the Kivali web pod.
// Implements the agent.Provisioner interface — `Provision` is the
// method web's hire path calls; `Destroy` is the fire path.
//
// Out-of-cluster (developer laptop) Provisioner.Enabled() reports
// false and lifecycle methods are no-ops so callers can stay
// simple.
type Provisioner struct {
	cfg    ProvisionerConfig
	kube   *kubeClient
	logger func(format string, args ...any)
}

// ProvisionerOption configures a Provisioner. Functional options keep
// New's signature narrow.
type ProvisionerOption func(*Provisioner)

// WithProvisionerLogger attaches a logger. Default discards messages.
func WithProvisionerLogger(fn func(format string, args ...any)) ProvisionerOption {
	return func(p *Provisioner) { p.logger = fn }
}

// NewProvisioner returns a Provisioner. The k8s client is
// best-effort: out-of-cluster environments still get a usable
// Provisioner whose lifecycle methods are no-ops and Enabled()
// reports false.
func NewProvisioner(cfg ProvisionerConfig, opts ...ProvisionerOption) (*Provisioner, error) {
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	p := &Provisioner{
		cfg:    cfg,
		logger: func(string, ...any) {},
	}
	for _, o := range opts {
		o(p)
	}
	k, err := newKubeClient(cfg.Namespace)
	if err == nil {
		p.kube = k
	} else {
		p.logger("agentpod: kube client unavailable (%v); running as no-op", err)
	}
	return p, nil
}

// Enabled reports whether real provisioning is possible (in-cluster
// kube client present).
func (p *Provisioner) Enabled() bool { return p != nil && p.kube != nil }

// Provision creates or reconciles the per-agent PVC + Pod. The Pod
// reconcile is spec-diff aware: existing pods whose spec-hash label
// matches the current manifest are left alone (idempotent for the
// no-change case), but any drift — different image, mounts, env,
// command — triggers a delete + wait-for-termination + recreate so
// the live pod always reflects the Kivali web binary that's
// supervising it.
//
// Without this, every agentpod-spec change would leave a stale
// fleet running on the previous spec until manually deleted. Kivali web
// manages those pods; if they need a new configuration, this is
// where it gets applied. See manifest.go::specHashLabel for the
// label format.
//
// PVCs are NOT diffed — they're truly immutable from our perspective
// (resizing a bound PVC is a separate, careful operation; deleting
// destroys data). pvcManifest stays ensure-only.
//
// Order: PVC first (Pod references it), then Pod. No Service.
func (p *Provisioner) Provision(ctx context.Context, slug string) error {
	if !p.Enabled() {
		p.logger("agentpod: Provision(%q): no-op (not in-cluster)", slug)
		return nil
	}
	if err := p.kube.ensureResource(ctx, p.kube.pvcPath(PVCName(slug)), p.kube.pvcsPath(), p.cfg.pvcManifest(slug)); err != nil {
		return fmt.Errorf("pvc: %w", err)
	}
	manifest := p.cfg.podManifest(slug)
	desired := manifest["metadata"].(map[string]any)["labels"].(map[string]string)[specHashLabel]
	existing, err := p.kube.getPodSpecHash(ctx, PodName(slug))
	switch {
	case errors.Is(err, ErrNotFound):
		// Pod doesn't exist; fall through to create.
	case err != nil:
		return fmt.Errorf("pod: read spec-hash: %w", err)
	case existing == desired:
		// Live pod already matches the current manifest; no-op.
		return nil
	default:
		// Drift (or a pod with no spec-hash label) — recreate.
		p.logger("agentpod: %s pod spec drift (existing=%q desired=%q); recreating", slug, existing, desired)
		if derr := p.kube.deleteResource(ctx, p.kube.podPath(PodName(slug))); derr != nil && !errors.Is(derr, ErrNotFound) {
			return fmt.Errorf("pod: delete stale: %w", derr)
		}
		// terminationGracePeriodSeconds=10 in the manifest; allow ~6x
		// for image-pull / volume detach / scheduler hesitation.
		if werr := p.kube.waitForPodGone(ctx, PodName(slug), 60*time.Second); werr != nil {
			return fmt.Errorf("pod: wait for termination: %w", werr)
		}
	}
	if err := p.kube.ensureResource(ctx, p.kube.podPath(PodName(slug)), p.kube.podsPath(), manifest); err != nil {
		return fmt.Errorf("pod: %w", err)
	}
	return nil
}

// Destroy deletes the Pod; the PVC is intentionally preserved so
// the agent's /scratch survives archive/fire (forensics). Safe to
// call on a never-provisioned slug.
func (p *Provisioner) Destroy(ctx context.Context, slug string) error {
	if !p.Enabled() {
		return nil
	}
	if err := p.kube.deleteResource(ctx, p.kube.podPath(PodName(slug))); err != nil && !errors.Is(err, ErrNotFound) {
		return fmt.Errorf("pod: %w", err)
	}
	return nil
}

// WaitReady blocks until the agent pod is Running + containers
// Ready, or the context deadline / timeout elapses. 500ms polling
// interval.
func (p *Provisioner) WaitReady(ctx context.Context, slug string, timeout time.Duration) error {
	if !p.Enabled() {
		return nil
	}
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if err := ctx.Err(); err != nil {
			return err
		}
		ready, err := p.kube.getPodReady(ctx, PodName(slug))
		if err != nil {
			if errors.Is(err, ErrNotFound) {
				return err
			}
			// Transient: keep polling.
		}
		if ready {
			return nil
		}
		time.Sleep(500 * time.Millisecond)
	}
	return fmt.Errorf("agent pod %q not ready within %s", slug, timeout)
}

// PodName returns the canonical Pod name for an agent slug. The
// "agentpod-" prefix is what cluster operators grep for when
// auditing per-agent resources.
func PodName(slug string) string { return "agentpod-" + slug }

// PVCName returns the canonical PVC name; kept distinct so the PVC
// can be left behind on Destroy.
func PVCName(slug string) string { return "agentpod-" + slug + "-scratch" }
