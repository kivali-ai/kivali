package integrationpod

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/kivali-ai/kivali/internal/kube"
)

// ProvisionerConfig controls integration pod provisioning: one pod per
// approved, switched-on integration, kept running while it is.
type ProvisionerConfig struct {
	// Namespace integration pods live in: the Kivali web pod's.
	Namespace string

	// Image is the kivali image, which the init container copies the
	// shim from; the server runs in AgentShellImageFor(Image). The
	// server sets it from AGENTPOD_IMAGE.
	Image string

	// ImagePullPolicy (IfNotPresent for kind, Always for prod).
	ImagePullPolicy string

	// UDSDir is the hostPath UDS directory core's sockets live under.
	// The pod mounts only its UDSSubdir and dials
	// <UDSDir>/<UDSSubdir>/<SocketName>.
	UDSDir string

	// WebSelector is the matchLabels expression of the Kivali web pod.
	// An integration pod requires a node running a pod that matches it.
	WebSelector map[string]string

	// DataClaimName is the PVC that holds /data on Kivali web; the pod
	// mounts its version's bundle from it, read-only.
	DataClaimName string

	// ProxyURL is the egress proxy, as configured (a host name is
	// resolved to an IP when a pod is built: pods have no DNS). Empty
	// means no proxy.
	ProxyURL string

	// ServiceAccountName to run the pod as. Empty means the namespace's
	// default. Its token is never mounted.
	ServiceAccountName string

	// CPURequest / MemoryRequest / CPULimit / MemoryLimit size the
	// integration container. Empty means 50m / 128Mi requests and
	// 1 / 1Gi limits.
	CPURequest    string
	MemoryRequest string
	CPULimit      string
	MemoryLimit   string
}

// DefaultsForNamespace returns a ProvisionerConfig for namespace with
// the chart's defaults. Callers set Image and ProxyURL.
func DefaultsForNamespace(namespace string) ProvisionerConfig {
	return ProvisionerConfig{
		Namespace:       namespace,
		ImagePullPolicy: "IfNotPresent",
		UDSDir:          "/var/run/kivali/uds",
		WebSelector:     map[string]string{"app": "kivali"},
		DataClaimName:   "kivali-data",
		CPURequest:      defaultCPURequest,
		MemoryRequest:   defaultMemoryRequest,
		CPULimit:        defaultCPULimit,
		MemoryLimit:     defaultMemoryLimit,
	}
}

func (c ProvisionerConfig) validate() error {
	switch {
	case strings.TrimSpace(c.Namespace) == "":
		return fmt.Errorf("integrationpod: namespace required")
	case strings.TrimSpace(c.Image) == "":
		return fmt.Errorf("integrationpod: image required")
	case strings.TrimSpace(c.UDSDir) == "":
		return fmt.Errorf("integrationpod: UDSDir required")
	case len(c.WebSelector) == 0:
		return fmt.Errorf("integrationpod: WebSelector required for same-node affinity")
	case strings.TrimSpace(c.DataClaimName) == "":
		return fmt.Errorf("integrationpod: DataClaimName required for the bundle")
	}
	return nil
}

// goneTimeout bounds the wait for a deleted pod to disappear: the 10 s
// termination grace period, plus room for volume teardown.
const goneTimeout = 60 * time.Second

// Provisioner creates and deletes integration pods. Out-of-cluster,
// where there is no Kubernetes API, Enabled reports false and every
// method does nothing.
type Provisioner struct {
	cfg     ProvisionerConfig
	kube    *kube.Client
	logger  func(format string, args ...any)
	resolve func(ctx context.Context, host string) ([]string, error)
}

// ProvisionerOption configures a Provisioner.
type ProvisionerOption func(*Provisioner)

// WithProvisionerLogger attaches a logger. Default discards messages.
func WithProvisionerLogger(fn func(format string, args ...any)) ProvisionerOption {
	return func(p *Provisioner) { p.logger = fn }
}

// WithResolver replaces the DNS lookup the proxy's host is resolved
// with (net.DefaultResolver.LookupHost). Test seam.
func WithResolver(fn func(ctx context.Context, host string) ([]string, error)) ProvisionerOption {
	return func(p *Provisioner) { p.resolve = fn }
}

// NewProvisioner returns a Provisioner for cfg. Without in-cluster
// credentials it is a no-op Provisioner (Enabled is false).
func NewProvisioner(cfg ProvisionerConfig, opts ...ProvisionerOption) (*Provisioner, error) {
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	p := &Provisioner{cfg: cfg, logger: func(string, ...any) {}}
	for _, o := range opts {
		o(p)
	}
	k, err := kube.New(cfg.Namespace)
	if err != nil {
		p.logger("integrationpod: kube client unavailable (%v); running as no-op", err)
		return p, nil
	}
	p.kube = k
	return p, nil
}

// Enabled reports whether real provisioning is possible (in-cluster
// kube client present).
func (p *Provisioner) Enabled() bool { return p != nil && p.kube != nil }

// manifestFor is the pod for name at version, its bundle's files of
// digest, with token, the proxy by IP; software says the version
// installs packages.
func (p *Provisioner) manifestFor(ctx context.Context, name, version, digest, token string, software bool) (map[string]any, error) {
	cfg := p.cfg
	proxy, err := kube.ProxyByIP(ctx, cfg.ProxyURL, p.resolve)
	if err != nil {
		return nil, err
	}
	cfg.ProxyURL = proxy
	return cfg.podManifest(name, version, digest, token, software), nil
}

func specHashOf(manifest map[string]any) string {
	return manifest["metadata"].(map[string]any)["labels"].(map[string]string)[kube.SpecHashLabel]
}

// Start makes sure the pod for name at version, its bundle's files of
// digest, with token in its environment, is there; software says the
// version's manifest declares packages, which the pod installs as root
// when it starts. A live pod of that name built from the same manifest
// is left running. One built from another manifest (another image,
// version, digest or token), or one that has finished, is deleted and
// created anew once it is gone.
func (p *Provisioner) Start(ctx context.Context, name, version, digest, token string, software bool) error {
	if !p.Enabled() {
		p.logger("integrationpod: Start(%q, %q): no-op (not in-cluster)", name, version)
		return nil
	}
	pod := PodName(name)
	manifest, err := p.manifestFor(ctx, name, version, digest, token, software)
	if err != nil {
		return fmt.Errorf("integration pod: %w", err)
	}
	desired := specHashOf(manifest)
	existing, err := p.kube.GetPod(ctx, pod)
	switch {
	case errors.Is(err, kube.ErrNotFound):
	case err != nil:
		return fmt.Errorf("integration pod: read: %w", err)
	case existing.SpecHash() == desired && !existing.Finished():
		return nil
	default:
		p.logger("integrationpod: %s: replacing pod (phase=%s spec-hash=%q desired=%q)", pod, existing.Phase, existing.SpecHash(), desired)
		if err := p.deleteAndWait(ctx, pod); err != nil {
			return err
		}
	}
	if err := p.kube.EnsureResource(ctx, p.kube.PodPath(pod), p.kube.PodsPath(), manifest); err != nil {
		return fmt.Errorf("integration pod: create: %w", err)
	}
	return nil
}

// Token is the token in the environment of a live pod for name that
// is built exactly as Start would build it for version, digest and
// software with that token, so core, after a restart, can take the pod
// on rather than replace it. Empty when there is no such pod: a pod
// built from other files than the ones stored now is never taken on.
func (p *Provisioner) Token(ctx context.Context, name, version, digest string, software bool) (string, error) {
	if !p.Enabled() {
		return "", nil
	}
	pod, err := p.kube.GetPod(ctx, PodName(name))
	if errors.Is(err, kube.ErrNotFound) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("integration pod: read: %w", err)
	}
	token := pod.Env("integration", EnvToken)
	if token == "" || pod.Finished() {
		return "", nil
	}
	manifest, err := p.manifestFor(ctx, name, version, digest, token, software)
	if err != nil {
		return "", fmt.Errorf("integration pod: %w", err)
	}
	if pod.SpecHash() != specHashOf(manifest) {
		return "", nil
	}
	return token, nil
}

// Stop deletes the pod for name and waits until it is gone. A pod that
// does not exist is already stopped.
func (p *Provisioner) Stop(ctx context.Context, name string) error {
	if !p.Enabled() {
		return nil
	}
	return p.deleteAndWait(ctx, PodName(name))
}

func (p *Provisioner) deleteAndWait(ctx context.Context, pod string) error {
	if err := p.kube.DeleteResource(ctx, p.kube.PodPath(pod)); err != nil && !errors.Is(err, kube.ErrNotFound) {
		return fmt.Errorf("integration pod: delete: %w", err)
	}
	if err := p.kube.WaitForPodGone(ctx, pod, goneTimeout); err != nil {
		return fmt.Errorf("integration pod: wait for termination: %w", err)
	}
	return nil
}

// List is the integrations that have a pod, sorted.
func (p *Provisioner) List(ctx context.Context) ([]string, error) {
	if !p.Enabled() {
		return nil, nil
	}
	pods, err := p.kube.ListPods(ctx, "app="+AppLabel)
	if err != nil {
		return nil, fmt.Errorf("integration pods: list: %w", err)
	}
	var out []string
	for _, pod := range pods {
		if name := nameFromPod(pod.Name); name != "" {
			out = append(out, name)
		}
	}
	sort.Strings(out)
	return out, nil
}

// Unschedulable is the scheduler's message while the pod for name
// cannot be placed on a node ("0/1 nodes are available: 1 Insufficient
// memory. ..."), empty while it can or has been, and when there is no
// such pod.
func (p *Provisioner) Unschedulable(ctx context.Context, name string) (string, error) {
	if !p.Enabled() {
		return "", nil
	}
	pod, err := p.kube.GetPod(ctx, PodName(name))
	if errors.Is(err, kube.ErrNotFound) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("integration pod: read: %w", err)
	}
	return pod.Unschedulable, nil
}
