package browserpod

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"

	"github.com/kivali-ai/kivali/internal/kube"
)

// The browser pod's manifest: a plain map[string]any, like the agent
// pod's, so only the fields that matter are set and the API server
// defaults the rest.
//
// The load-bearing properties of the shape:
//
//  1. One container, `kivali browser` under dumb-init (the image's
//     init, which reaps Chromium's helper processes when they are
//     orphaned), from the kivali-browser image, configured entirely by
//     env (EnvSlug, EnvSession, EnvToken, EnvCoreToken, EnvCoreUDS). It reaches the
//     web directly, like any pod; the chart's network policy keeps it
//     off the cluster and private networks.
//  2. It dials browser.sock, core's socket for browser pods, in the
//     browser subdirectory of the hostPath UDS dir, the only part of
//     that directory it mounts, read-only; core.sock and the agent
//     routes stay out of its reach. The hostPath is why it carries the
//     agent pod's required podAffinity to the Kivali web pod's node.
//     It runs as the image's user with every capability dropped.
//  3. No Service, no ports, no service-account token: the pod never
//     listens and has no business with the Kubernetes API.
//  4. No persistent volume. The profile travels over the WebSocket to
//     core; the pod keeps it, and downloads, in an emptyDir at /tmp.
//     /dev/shm is a memory-backed emptyDir (Chromium's shared memory).
//  5. restartPolicy Never: core recreates a browser, kubelet must not.
//     terminationGracePeriodSeconds 60 leaves time for a clean Chromium
//     close and the profile's upload.

const (
	// AppLabel is the app label on every browser pod; StopAll deletes
	// by it.
	AppLabel = "kivali-browserpod"

	// browserImageName is the browser image's base name, released at
	// the server image's tag (BrowserImageFor).
	browserImageName = "kivali-browser"

	// browserUID is the image's kivali user (and group).
	browserUID = 65532

	// terminationGraceSeconds is how long kubelet waits after SIGTERM:
	// a clean Chromium close plus the profile's upload to core.
	terminationGraceSeconds = 60

	// shmSize and tmpSize bound the two emptyDirs: Chromium's shared
	// memory, and /tmp, which holds the unpacked profile and downloads.
	shmSize = "1Gi"
	tmpSize = "4Gi"

	// Resource defaults for a config that leaves them empty.
	defaultCPURequest    = "100m"
	defaultMemoryRequest = "512Mi"
	defaultCPULimit      = "2"
	defaultMemoryLimit   = "2Gi"

	// podPrefix starts every browser pod's name.
	podPrefix = "browserpod-"
	// maxPodName is the longest pod name PodName returns: a DNS label.
	// maxLabelValue is the longest label value Kubernetes takes.
	maxPodName    = 63
	maxLabelValue = 63
)

// browserCommand runs the pod's one process under dumb-init, the
// image's init: with `kivali browser` as PID 1, Chromium's helper
// processes would go unreaped once orphaned.
var browserCommand = []string{"/usr/bin/dumb-init", "--", "kivali", "browser"}

// BrowserImageFor returns the browser image released alongside the
// server image kivaliImage: same registry and tag, named kivali-browser.
//
//	"ghcr.io/kivali-ai/kivali:v1.2.0" → "ghcr.io/kivali-ai/kivali-browser:v1.2.0"
//	"kivali:dev"                      → "kivali-browser:dev"
func BrowserImageFor(kivaliImage string) string {
	return kube.SiblingImage(kivaliImage, browserImageName)
}

// SessionHash is the 12 hex characters of sha256(session) that name a
// session in its pod's name and session label. A session name is free
// text; its hash is always a valid name and label value.
func SessionHash(session string) string {
	h := sha256.Sum256([]byte(session))
	return hex.EncodeToString(h[:6])
}

// PodName is the pod for slug's browser session:
// browserpod-<slug>-<SessionHash(session)>. A slug too long for the 63
// characters of a pod name is shortened (shorten), so two long slugs
// with one head still name two pods; the session hash is never
// shortened.
func PodName(slug, session string) string {
	hash := SessionHash(session)
	return podPrefix + shorten(slug, maxPodName-len(podPrefix)-1-len(hash)) + "-" + hash
}

// shorten fits s into max characters: s itself when it fits, else its
// head, a dash and the first six hex characters of sha256(s), so two
// long values with one head stay apart.
func shorten(s string, max int) string {
	if len(s) <= max {
		return s
	}
	sum := sha256.Sum256([]byte(s))
	tail := "-" + hex.EncodeToString(sum[:3])
	return strings.TrimRight(s[:max-len(tail)], "-") + tail
}

// browserUDSDir is the subdirectory of the UDS dir that holds
// browser.sock: the only part of that directory a browser pod mounts,
// read-only. core.sock, the agent pods' socket, stays out of the pod.
func browserUDSDir(udsDir string) string {
	return strings.TrimRight(udsDir, "/") + "/" + UDSSubdir
}

// podManifest is the pod for slug's browser session, its token and the
// token core proves itself with in the environment. Both are per pod
// and die with it.
func (c ProvisionerConfig) podManifest(slug, session, token, coreToken string) map[string]any {
	udsDir := browserUDSDir(c.UDSDir)
	env := []map[string]any{
		{"name": EnvSlug, "value": slug},
		{"name": EnvSession, "value": session},
		{"name": EnvToken, "value": token},
		{"name": EnvCoreToken, "value": coreToken},
		{"name": EnvCoreUDS, "value": udsDir + "/" + SocketName},
	}

	container := map[string]any{
		"name":            "browser",
		"image":           c.Image,
		"imagePullPolicy": c.ImagePullPolicy,
		"command":         browserCommand,
		"env":             env,
		"volumeMounts": []map[string]any{
			{"name": "uds", "mountPath": udsDir, "readOnly": true},
			{"name": "shm", "mountPath": "/dev/shm"},
			{"name": "tmp", "mountPath": "/tmp"},
		},
		// No capabilities: Chromium's own sandbox needs none. Its
		// seccomp profile is left to the cluster's default, because a
		// profile that blocks user namespaces blocks that sandbox and
		// Chromium falls back to running pages without it (chrome.go).
		"securityContext": map[string]any{
			"runAsUser":                browserUID,
			"runAsGroup":               browserUID,
			"runAsNonRoot":             true,
			"allowPrivilegeEscalation": false,
			"capabilities":             map[string]any{"drop": []string{"ALL"}},
		},
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
		"restartPolicy":                 "Never",
		"terminationGracePeriodSeconds": terminationGraceSeconds,
		"containers":                    []any{container},
		"volumes": []map[string]any{
			// Directory, not DirectoryOrCreate: core makes it, owned by
			// its own user, before any pod starts; one kubelet made
			// would be root's and unusable.
			{"name": "uds", "hostPath": map[string]any{"path": udsDir, "type": "Directory"}},
			{"name": "shm", "emptyDir": map[string]any{"medium": "Memory", "sizeLimit": shmSize}},
			{"name": "tmp", "emptyDir": map[string]any{"sizeLimit": tmpSize}},
		},
		// Same-node affinity to Kivali web, required: the hostPath UDS
		// dir exists only on its node, so a pod scheduled elsewhere
		// could never dial core.
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
		"app":     AppLabel,
		"agent":   shorten(slug, maxLabelValue),
		"session": SessionHash(session),
	}
	manifest := map[string]any{
		"apiVersion": "v1",
		"kind":       "Pod",
		"metadata": map[string]any{
			"name":      PodName(slug, session),
			"namespace": c.Namespace,
			"labels":    labels,
		},
		"spec": spec,
	}
	// The spec hash lets Start tell a live pod built from an older
	// manifest (another image, another token) from one it can keep.
	labels[kube.SpecHashLabel] = kube.SpecHash(manifest)
	return manifest
}

func orDefault(v, def string) string {
	if strings.TrimSpace(v) == "" {
		return def
	}
	return v
}
