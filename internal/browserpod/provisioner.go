package browserpod

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/kivali-ai/kivali/internal/kube"
)

// ProvisionerConfig controls browser pod provisioning: one pod per
// (agent slug, session name), created when core starts that browser and
// deleted when core reaps it.
type ProvisionerConfig struct {
	// Namespace browser pods live in: the Kivali web pod's.
	Namespace string

	// Image is the kivali-browser image. The server sets it from
	// BROWSER_IMAGE, or BrowserImageFor of the agent pod image.
	Image string

	// ImagePullPolicy (IfNotPresent for kind, Always for prod).
	ImagePullPolicy string

	// UDSDir is the hostPath UDS directory core's sockets live under.
	// The pod mounts only its UDSSubdir and dials
	// <UDSDir>/<UDSSubdir>/<SocketName>.
	UDSDir string

	// WebSelector is the matchLabels expression of the Kivali web pod.
	// A browser pod requires a node running a pod that matches it.
	WebSelector map[string]string

	// ServiceAccountName to run the pod as. Empty means the namespace's
	// default. Its token is never mounted.
	ServiceAccountName string

	// CPURequest / MemoryRequest / CPULimit / MemoryLimit size the
	// browser container. Empty means 100m / 512Mi requests and 2 / 2Gi
	// limits.
	CPURequest    string
	MemoryRequest string
	CPULimit      string
	MemoryLimit   string
}

// DefaultsForNamespace returns a ProvisionerConfig for namespace with
// the chart's defaults. Callers set Image.
func DefaultsForNamespace(namespace string) ProvisionerConfig {
	return ProvisionerConfig{
		Namespace:       namespace,
		ImagePullPolicy: "IfNotPresent",
		UDSDir:          "/var/run/kivali/uds",
		WebSelector:     map[string]string{"app": "kivali"},
		CPURequest:      defaultCPURequest,
		MemoryRequest:   defaultMemoryRequest,
		CPULimit:        defaultCPULimit,
		MemoryLimit:     defaultMemoryLimit,
	}
}

func (c ProvisionerConfig) validate() error {
	if strings.TrimSpace(c.Namespace) == "" {
		return fmt.Errorf("browserpod: namespace required")
	}
	if strings.TrimSpace(c.Image) == "" {
		return fmt.Errorf("browserpod: image required")
	}
	if strings.TrimSpace(c.UDSDir) == "" {
		return fmt.Errorf("browserpod: UDSDir required")
	}
	if len(c.WebSelector) == 0 {
		return fmt.Errorf("browserpod: WebSelector required for same-node affinity")
	}
	return nil
}

// goneTimeout bounds the wait for a deleted pod to disappear: the 60 s
// termination grace period, which a clean close and profile upload may
// use in full, plus room for volume teardown.
const goneTimeout = 90 * time.Second

// Provisioner creates and deletes browser pods. Out-of-cluster, where
// there is no Kubernetes API, Enabled reports false and every method
// does nothing.
type Provisioner struct {
	cfg    ProvisionerConfig
	kube   *kube.Client
	logger func(format string, args ...any)
}

// ProvisionerOption configures a Provisioner.
type ProvisionerOption func(*Provisioner)

// WithProvisionerLogger attaches a logger. Default discards messages.
func WithProvisionerLogger(fn func(format string, args ...any)) ProvisionerOption {
	return func(p *Provisioner) { p.logger = fn }
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
		p.logger("browserpod: kube client unavailable (%v); running as no-op", err)
		return p, nil
	}
	p.kube = k
	return p, nil
}

// Enabled reports whether real provisioning is possible (in-cluster
// kube client present).
func (p *Provisioner) Enabled() bool { return p != nil && p.kube != nil }

// Start creates the pod for slug's session with token and coreToken in
// its environment. A live pod of that name built from the same manifest is
// left running. One built from another manifest (another image, another
// token), or one that has finished (Succeeded or Failed: restartPolicy
// is Never, so it will not run again), is deleted and created anew once
// it is gone.
func (p *Provisioner) Start(ctx context.Context, slug, session, token, coreToken string) error {
	if !p.Enabled() {
		p.logger("browserpod: Start(%q, %q): no-op (not in-cluster)", slug, session)
		return nil
	}
	name := PodName(slug, session)
	manifest := p.cfg.podManifest(slug, session, token, coreToken)
	desired := manifest["metadata"].(map[string]any)["labels"].(map[string]string)[kube.SpecHashLabel]
	existing, err := p.kube.GetPod(ctx, name)
	switch {
	case errors.Is(err, kube.ErrNotFound):
	case err != nil:
		return fmt.Errorf("browser pod: read: %w", err)
	case existing.SpecHash() == desired && !existing.Finished():
		return nil
	default:
		p.logger("browserpod: %s: replacing pod (phase=%s spec-hash=%q desired=%q)", name, existing.Phase, existing.SpecHash(), desired)
		if err := p.deleteAndWait(ctx, name); err != nil {
			return err
		}
	}
	if err := p.kube.EnsureResource(ctx, p.kube.PodPath(name), p.kube.PodsPath(), manifest); err != nil {
		return fmt.Errorf("browser pod: create: %w", err)
	}
	return nil
}

// Stop deletes the pod for slug's session and waits until it is gone.
// A pod that does not exist is already stopped.
func (p *Provisioner) Stop(ctx context.Context, slug, session string) error {
	if !p.Enabled() {
		return nil
	}
	return p.deleteAndWait(ctx, PodName(slug, session))
}

func (p *Provisioner) deleteAndWait(ctx context.Context, name string) error {
	if err := p.kube.DeleteResource(ctx, p.kube.PodPath(name)); err != nil && !errors.Is(err, kube.ErrNotFound) {
		return fmt.Errorf("browser pod: delete: %w", err)
	}
	if err := p.kube.WaitForPodGone(ctx, name, goneTimeout); err != nil {
		return fmt.Errorf("browser pod: wait for termination: %w", err)
	}
	return nil
}

// StopAll deletes every browser pod in the namespace and returns how
// many it deleted. Core runs it at boot: no browser outlives the
// server that started it. It does not wait for the pods to be gone.
func (p *Provisioner) StopAll(ctx context.Context) (int, error) {
	if !p.Enabled() {
		return 0, nil
	}
	pods, err := p.kube.ListPods(ctx, "app="+AppLabel)
	if err != nil {
		return 0, fmt.Errorf("browser pods: list: %w", err)
	}
	n := 0
	for _, pod := range pods {
		if err := p.kube.DeleteResource(ctx, p.kube.PodPath(pod.Name)); err != nil && !errors.Is(err, kube.ErrNotFound) {
			return n, fmt.Errorf("browser pod %s: delete: %w", pod.Name, err)
		}
		n++
	}
	return n, nil
}
