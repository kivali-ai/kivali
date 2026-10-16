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
//  1. One container, `kivali browser`, from the kivali-browser image,
//     configured entirely by env (EnvSlug, EnvSession, EnvToken,
//     EnvCoreUDS, EnvProxy).
//  2. It dials core.sock in the hostPath UDS dir, so it carries the
//     agent pod's required podAffinity to the Kivali web pod's node.
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

	// coreSocketName is core's socket in the UDS dir, the one agent
	// pods dial.
	coreSocketName = "core.sock"

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
	maxPodName = 63
)

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
// browserpod-<slug>-<SessionHash(session)>. A long slug is shortened so
// the name stays within 63 characters; the hash is never shortened.
func PodName(slug, session string) string {
	hash := SessionHash(session)
	room := maxPodName - len(podPrefix) - 1 - len(hash)
	if len(slug) > room {
		slug = strings.TrimRight(slug[:room], "-")
	}
	return podPrefix + slug + "-" + hash
}

// podManifest is the pod for slug's browser session, its token in the
// environment. The token is per pod and dies with it.
func (c ProvisionerConfig) podManifest(slug, session, token string) map[string]any {
	env := []map[string]any{
		{"name": EnvSlug, "value": slug},
		{"name": EnvSession, "value": session},
		{"name": EnvToken, "value": token},
		{"name": EnvCoreUDS, "value": strings.TrimRight(c.UDSDir, "/") + "/" + coreSocketName},
	}
	if c.ProxyURL != "" {
		env = append(env, map[string]any{"name": EnvProxy, "value": c.ProxyURL})
	}

	container := map[string]any{
		"name":            "browser",
		"image":           c.Image,
		"imagePullPolicy": c.ImagePullPolicy,
		"command":         []string{"kivali", "browser"},
		"env":             env,
		"volumeMounts": []map[string]any{
			{"name": "uds", "mountPath": c.UDSDir},
			{"name": "shm", "mountPath": "/dev/shm"},
			{"name": "tmp", "mountPath": "/tmp"},
		},
		"securityContext": map[string]any{
			"runAsUser":                browserUID,
			"runAsGroup":               browserUID,
			"allowPrivilegeEscalation": false,
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
			{"name": "uds", "hostPath": map[string]any{"path": c.UDSDir, "type": "DirectoryOrCreate"}},
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
		"agent":   slug,
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
