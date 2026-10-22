package integrationpod

import (
	"strings"

	"github.com/kivali-ai/kivali/internal/kube"
)

// The integration pod's manifest: a plain map[string]any, like the
// agent and browser pods', so only the fields that matter are set and
// the API server defaults the rest.
//
// The load-bearing properties of the shape:
//
//  1. No new image. An init container from the kivali image copies the
//     kivali binary into an emptyDir; the one container runs the
//     agent-shell image (which has python3) with that binary as its
//     command: `kivali integration-shim`, configured entirely by env
//     (EnvName, EnvVersion, EnvToken, EnvCoreUDS, EnvBundle), and
//     carrying the bundle's digest (EnvBundleDigest) so the spec hash
//     moves with the files it runs.
//  2. It dials integration.sock, in the integration subdirectory of the
//     hostPath UDS dir, the only part of that directory it mounts,
//     read-only; core.sock and the browser pods' socket stay out of its
//     reach. The hostPath is why it carries the agent pod's required
//     podAffinity to the Kivali web pod's node.
//  3. The version's bundle is a read-only subPath of the data PVC at
//     /bundle; nothing else of the PVC is mounted. /tmp is an emptyDir
//     and the server's HOME.
//  4. No Secret, no secret value: the shim asks core for the
//     connection's values over the socket when it starts the server.
//     The token in the env is per pod and dies with it; no label
//     carries it.
//  5. No Service, no ports, no service-account token. Its only network
//     path is the egress proxy (the chart's network policy), where its
//     scoped credential reaches the hosts its manifest names and no
//     other.
//  6. restartPolicy Always: the shim exits when its server dies, and
//     kubelet starts it again.
//  7. Both containers run as the images' user with every capability
//     dropped, except when the version's manifest declares packages
//     (kivali.packages): then the integration container starts as root
//     with what apt-get and dpkg need (installSecurityContext), and the
//     shim installs them in a root child and leaves root for good
//     before anything of the bundle runs (Shim.Software).

const (
	// agentShellImageName is the image the server runs in: the agent
	// shell's, released at the kivali image's tag, which carries python3.
	agentShellImageName = "kivali-agent-shell"

	// podUID is the images' kivali user (and group).
	podUID = 65532

	// terminationGraceSeconds is how long kubelet waits after SIGTERM:
	// the shim gives its server five seconds to exit.
	terminationGraceSeconds = 10

	// shimDir holds the kivali binary the init container copies.
	shimDir = "/shim"
	// shimBinary is the binary's path in the kivali image.
	shimBinary = "/usr/local/bin/kivali"

	// shimSize and tmpSize bound the two emptyDirs.
	shimSize = "256Mi"
	tmpSize  = "256Mi"

	// Resource defaults for a config that leaves them empty.
	defaultCPURequest    = "50m"
	defaultMemoryRequest = "128Mi"
	defaultCPULimit      = "1"
	defaultMemoryLimit   = "1Gi"
)

// ShimCommand is the integration container's command.
var ShimCommand = []string{shimDir + "/kivali", "integration-shim"}

// AgentShellImageFor is the image the server runs in, released beside
// the kivali image: same registry and tag, named kivali-agent-shell.
//
//	"ghcr.io/kivali-ai/kivali:v1.2.0" → "ghcr.io/kivali-ai/kivali-agent-shell:v1.2.0"
func AgentShellImageFor(kivaliImage string) string {
	return kube.SiblingImage(kivaliImage, agentShellImageName)
}

// integrationUDSDir is the subdirectory of the UDS dir that holds
// integration.sock.
func integrationUDSDir(udsDir string) string {
	return strings.TrimRight(udsDir, "/") + "/" + UDSSubdir
}

// BundleSubPath is the data PVC subPath of a stored version's bundle,
// as internal/store keeps it.
func BundleSubPath(name, version string) string {
	return "integrations/" + name + "/versions/" + version + "/bundle"
}

// securityContext is both containers': the images' user, no
// privilege escalation, every capability dropped.
func securityContext() map[string]any {
	return map[string]any{
		"runAsUser":                podUID,
		"runAsGroup":               podUID,
		"runAsNonRoot":             true,
		"allowPrivilegeEscalation": false,
		"capabilities":             map[string]any{"drop": []string{"ALL"}},
	}
}

// installCapabilities are the integration container's capabilities
// while it installs software, the rest dropped: what apt-get and dpkg
// need as root for ordinary packages (no SETFCAP, so a package whose
// maintainer script runs setcap may fail). CHOWN, DAC_OVERRIDE, FOWNER
// and FSETID for dpkg to
// unpack files with their owners and modes (setuid bits included) and
// for apt-get to use its _apt-owned download directories; SETUID and
// SETGID for apt-get to fetch as _apt, and for the shim to become the
// images' user; KILL for the install to stop what a package's scripts
// left running, whoever runs it; SETPCAP for the shim to empty its
// bounding set as it leaves root.
var installCapabilities = []string{"CHOWN", "DAC_OVERRIDE", "FOWNER", "FSETID", "KILL", "SETGID", "SETPCAP", "SETUID"}

// installSecurityContext is the integration container's when the
// version installs software: it starts as root, still with no
// privilege escalation, and with installCapabilities alone. The shim
// runs apt-get in a root child, then drops to podUID with no
// capability and an empty bounding set before it starts the server.
func installSecurityContext() map[string]any {
	return map[string]any{
		"runAsUser":                0,
		"runAsGroup":               0,
		"runAsNonRoot":             false,
		"allowPrivilegeEscalation": false,
		"capabilities":             map[string]any{"drop": []string{"ALL"}, "add": installCapabilities},
	}
}

// podManifest is the pod for name at version, whose bundle's files
// have digest, with token in its environment. software says the
// version's manifest declares packages, which the integration container
// installs as root when it starts.
// c.ProxyURL is the proxy by IP (Provisioner.config).
func (c ProvisionerConfig) podManifest(name, version, digest, token string, software bool) map[string]any {
	udsDir := integrationUDSDir(c.UDSDir)
	env := []map[string]any{
		{"name": EnvName, "value": name},
		{"name": EnvVersion, "value": version},
		{"name": EnvToken, "value": token},
		{"name": EnvCoreUDS, "value": udsDir + "/" + SocketName},
		{"name": EnvBundle, "value": DefaultBundle},
		{"name": EnvBundleDigest, "value": digest},
	}
	if c.ProxyURL != "" {
		// Bare: the shim draws the credential the server's own
		// requests carry, so the spec holds none.
		for _, k := range []string{"HTTP_PROXY", "HTTPS_PROXY", "http_proxy", "https_proxy"} {
			env = append(env, map[string]any{"name": k, "value": c.ProxyURL})
		}
	}

	shim := map[string]any{
		"name":            "shim",
		"image":           c.Image,
		"imagePullPolicy": c.ImagePullPolicy,
		"command":         []string{"cp", shimBinary, shimDir + "/kivali"},
		"volumeMounts": []map[string]any{
			{"name": "shim", "mountPath": shimDir},
		},
		"securityContext": securityContext(),
		"resources": map[string]any{
			"requests": map[string]any{"cpu": "10m", "memory": "16Mi"},
			"limits":   map[string]any{"cpu": "200m", "memory": "128Mi"},
		},
	}
	containerSecurity := securityContext()
	if software {
		containerSecurity = installSecurityContext()
	}
	container := map[string]any{
		"name":            "integration",
		"image":           AgentShellImageFor(c.Image),
		"imagePullPolicy": c.ImagePullPolicy,
		"command":         ShimCommand,
		"env":             env,
		"volumeMounts": []map[string]any{
			{"name": "shim", "mountPath": shimDir, "readOnly": true},
			{"name": "uds", "mountPath": udsDir, "readOnly": true},
			{"name": "bundle", "mountPath": DefaultBundle, "subPath": BundleSubPath(name, version), "readOnly": true},
			{"name": "tmp", "mountPath": "/tmp"},
		},
		"securityContext": containerSecurity,
		"resources": map[string]any{
			"requests": map[string]any{
				"cpu":    orDefault(c.CPURequest, defaultCPURequest),
				"memory": orDefault(c.MemoryRequest, defaultMemoryRequest),
			},
			"limits": map[string]any{
				"cpu":    orDefault(c.CPULimit, defaultCPULimit),
				"memory": orDefault(c.MemoryLimit, defaultMemoryLimit),
			},
		},
	}

	spec := map[string]any{
		"automountServiceAccountToken":  false,
		"restartPolicy":                 "Always",
		"terminationGracePeriodSeconds": terminationGraceSeconds,
		"initContainers":                []any{shim},
		"containers":                    []any{container},
		"volumes": []map[string]any{
			{"name": "shim", "emptyDir": map[string]any{"sizeLimit": shimSize}},
			// Directory, not DirectoryOrCreate: core makes it, owned by
			// its own user, before any pod starts; one kubelet made
			// would be root's and unusable.
			{"name": "uds", "hostPath": map[string]any{"path": udsDir, "type": "Directory"}},
			{"name": "bundle", "persistentVolumeClaim": map[string]any{"claimName": c.DataClaimName, "readOnly": true}},
			{"name": "tmp", "emptyDir": map[string]any{"sizeLimit": tmpSize}},
		},
		// Same-node affinity to Kivali web, required: the hostPath UDS
		// dir exists only on its node, and the data PVC is
		// ReadWriteOnce.
		"affinity": map[string]any{
			"podAffinity": map[string]any{
				"requiredDuringSchedulingIgnoredDuringExecution": []map[string]any{
					{
						"labelSelector": map[string]any{"matchLabels": c.WebSelector},
						"topologyKey":   "kubernetes.io/hostname",
					},
				},
			},
		},
	}
	if c.ServiceAccountName != "" {
		spec["serviceAccountName"] = c.ServiceAccountName
	}

	labels := map[string]string{
		"app":         AppLabel,
		"integration": name,
	}
	manifest := map[string]any{
		"apiVersion": "v1",
		"kind":       "Pod",
		"metadata": map[string]any{
			"name":      PodName(name),
			"namespace": c.Namespace,
			"labels":    labels,
		},
		"spec": spec,
	}
	// The spec hash lets Start tell a live pod built from another
	// manifest (another image, version or token) from one it can keep.
	labels[kube.SpecHashLabel] = kube.SpecHash(manifest)
	return manifest
}

func orDefault(v, def string) string {
	if strings.TrimSpace(v) == "" {
		return def
	}
	return v
}
