package agentpod

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"sort"
	"strings"

	"github.com/kivali-ai/kivali/internal/files"
)

// Manifest builders for the per-agent Pod + PVC. Plain map[string]any
// nested objects rather than typed client-go structs so we set only
// the fields that matter and let the API server default the rest.
//
// Five load-bearing properties of the shape:
//
//  1. Container command is `kivali agent --slug=<slug> --uds=<sock>`.
//  2. The agent container's mounts: its own half of the per-agent PVC
//     at /scratch (the dev-shell mounts the other half there; see
//     scratch.go), hostPath UDS dir at /var/run/kivali/uds, the
//     dev-shell socket dir, and the Kivali PVC's provider home subdir
//     (HomeSubPath, from the provider's Credentials: claude-home) at
//     /data/<HomeSubPath>, so the embedded CLI sees the same
//     credentials the operator signed in with.
//  3. No Service. Agent pods own no listener; communication is
//     outbound-only over UDS.
//  4. No exposed ports. Belt-and-suspenders for #3.
//  5. podAffinity required: same node as Kivali web. The hostPath
//     UDS dir only exists on Kivali web's node.
//
// automountServiceAccountToken stays false (the agent pod has no
// business talking to k8s API) and the egress proxy env vars are
// kept (claude CLI's outbound HTTPS).

const (
	agentpodSocketName = "core.sock"
	// devShellSocketDir is mounted as an emptyDir into both the
	// agent and dev-shell containers; the daemon listens on a
	// socket inside it. Deliberately NOT under /scratch (which
	// agents browse via run_shell): putting the socket on a path
	// agents have no reason to enumerate keeps the dispatch
	// machinery invisible from inside the shell. emptyDir is
	// pod-local + ephemeral — perfect for an in-pod IPC channel.
	devShellSocketDir  = "/run/kivali-dev-shell"
	devShellSocketName = "sock"
)

// DevShellSocketPath returns the in-pod absolute path of the
// dev-shell sidecar's UDS. Used by mcp_cmd.go's wiring + by the
// daemon binary's --socket flag default. Kept as a function (not a
// const referencing the manifest) so tests can override per-fixture.
func DevShellSocketPath() string { return devShellSocketDir + "/" + devShellSocketName }

// dataVolumeName is the pod volume backed by the web pod's data PVC.
const dataVolumeName = "kivali-data"

// devShellSockVolumeName is the emptyDir volume that holds the
// daemon's UDS. Mounted into both containers at devShellSocketDir.
const devShellSockVolumeName = "dev-shell-sock"

// devShellImageName is the second image's base name. Both images
// (the main image + kivali-dev-shell) ship at the same tag; a release is
// one atomic set, so deriving the dev-shell image from the
// agentpod image's registry + tag is the only correct shape.
const devShellImageName = "kivali-dev-shell"

// DevShellImageFor returns the dev-shell sidecar image to pair with
// agentpodImage. Both are released in lock-step at the same tag, so
// we keep the registry + tag and swap the name.
//
// Examples:
//
//	"kivali:v0.8.3"                  → "kivali-dev-shell:v0.8.3"
//	"ghcr.io/kivali-ai/kivali:v0.8.3"    → "ghcr.io/kivali-ai/kivali-dev-shell:v0.8.3"
//	"kivali:dev"                     → "kivali-dev-shell:dev"
//
// An agentpodImage without a tag is treated as ":latest" — matches
// docker's default. An empty agentpodImage returns empty (the
// validator catches it upstream).
func DevShellImageFor(agentpodImage string) string {
	if agentpodImage == "" {
		return ""
	}
	// Split off the tag (last colon AFTER any registry-port colon).
	// "registry:5000/name:v1" → name="registry:5000/name", tag="v1".
	// strings.LastIndex gets the right colon because the path
	// separator "/" sits between the port colon and any tag colon.
	tag := "latest"
	name := agentpodImage
	if i := strings.LastIndex(agentpodImage, ":"); i > strings.LastIndex(agentpodImage, "/") {
		name = agentpodImage[:i]
		tag = agentpodImage[i+1:]
	}
	// Replace the last "/"-separated segment with devShellImageName.
	if i := strings.LastIndex(name, "/"); i >= 0 {
		return name[:i+1] + devShellImageName + ":" + tag
	}
	return devShellImageName + ":" + tag
}

// filesMounts returns the agent's /files mount (RW), then a RW mount
// of every directory between /files and a nested mount (today just
// artifacts/), then a nested read-only subPath mount over each subtree
// core manages (files.CoreManagedDirs): the project/, skills/,
// attachments/, past-chats/ and episodes/ symlink farms, and the two
// published trees, public/<slug> at /files/artifacts/public and public/
// itself at /files/artifacts/shared, where every agent's published
// tree, archived ones included, appears by owner. Only the dev-shell
// container mounts them.
//
// The farms are core's: the file_* tools refuse writes there by path
// prefix, but run_shell is bash, and without these mounts it could
// replace a farm link with its own — pointing wherever it liked — or
// replace the whole directory with a link, which core's next Sync would
// then write through. A mount point cannot be written into, renamed or
// removed from inside the pod. The published trees are core's too:
// artifact_publish is the only way into them.
//
// A mount point cannot be renamed, but the directory above one can,
// and the mount goes with it: `mv /files/artifacts /files/junk` would
// carry the public/ and shared/ mounts away, leaving writable
// directories where the agent expects its published files. So every
// directory between /files and a nested mount is a mount point itself
// (RW; it is the agent's), and rename(2) of it fails with EBUSY. The
// farms directly under /files need nothing more: /files is a mount
// point already. The RW parents are derived from CoreManagedDirs.
//
// A move between such a directory and the rest of /files crosses a
// mount, so rename(2) answers EXDEV: `mv` copies instead, and
// file_rename falls back to copy-and-delete for a file.
//
// Each subPath exists before the pod starts: core's files.Sync makes
// every CoreManagedDirs entry (and so each parent) a real directory in
// the agent's tree, replacing any link the agent left there, and
// public/<slug> a
// real directory on the data volume, and core runs an agent's Sync
// before it creates the agent's pod. (Kubelet would create a missing
// one, as root, which core then cannot write; and it refuses a subPath
// with a symlink in it, which is the other reason Sync replaces any
// link it finds there.) A parent precedes what is mounted inside it in
// the list.
func filesMounts(filesSubPath, slug string) []map[string]any {
	out := []map[string]any{{
		"name":      dataVolumeName,
		"mountPath": "/files",
		"subPath":   filesSubPath,
	}}
	for _, d := range coreManagedParents() {
		out = append(out, map[string]any{
			"name":      dataVolumeName,
			"mountPath": "/files/" + d,
			"subPath":   filesSubPath + "/" + d,
		})
	}
	for _, d := range files.CoreManagedDirs {
		out = append(out, map[string]any{
			"name":      dataVolumeName,
			"mountPath": "/files/" + d,
			"subPath":   coreManagedSubPath(filesSubPath, slug, d),
			"readOnly":  true,
		})
	}
	return out
}

// coreManagedSubPath is the data-volume subPath mounted at /files/<d>:
// a farm is the directory of that name in the agent's own tree; the
// published mount points are the published trees, the agent's own and
// everyone's.
func coreManagedSubPath(filesSubPath, slug, d string) string {
	switch d {
	case files.PublishedOwnDir:
		return files.PublishedDirName + "/" + slug
	case files.PublishedPeersDir:
		return files.PublishedDirName
	}
	return filesSubPath + "/" + d
}

// coreManagedParents lists, shallowest first, every proper ancestor
// of a files.CoreManagedDirs entry below /files that is not itself a
// core-managed subtree: the directories filesMounts must make mount
// points.
func coreManagedParents() []string {
	managed := map[string]bool{}
	for _, d := range files.CoreManagedDirs {
		managed[d] = true
	}
	seen := map[string]bool{}
	var out []string
	for _, d := range files.CoreManagedDirs {
		parts := strings.Split(d, "/")
		for i := 1; i < len(parts); i++ {
			p := strings.Join(parts[:i], "/")
			if managed[p] || seen[p] {
				continue
			}
			seen[p] = true
			out = append(out, p)
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		return strings.Count(out[i], "/") < strings.Count(out[j], "/")
	})
	return out
}

func (c ProvisionerConfig) pvcManifest(slug string) map[string]any {
	spec := map[string]any{
		"accessModes": []string{"ReadWriteOnce"},
		"resources": map[string]any{
			"requests": map[string]any{"storage": c.ScratchSize},
		},
	}
	if c.StorageClassName != "" {
		spec["storageClassName"] = c.StorageClassName
	}
	return map[string]any{
		"apiVersion": "v1",
		"kind":       "PersistentVolumeClaim",
		"metadata": map[string]any{
			"name":      PVCName(slug),
			"namespace": c.Namespace,
			"labels": map[string]string{
				"app":   "kivali-agentpod",
				"agent": slug,
			},
		},
		"spec": spec,
	}
}

// PodManifest is slug's agent pod as Provision would create it. For a
// driver's own tests: it pins the names the provider's Credentials put
// on the pod, which this package's tests only see through a mock.
func (c ProvisionerConfig) PodManifest(slug string) map[string]any {
	return c.podManifest(slug)
}

func (c ProvisionerConfig) podManifest(slug string) map[string]any {
	envs := []map[string]any{}
	if c.ProxyURL != "" {
		// Both upper-case and lower-case variants so every common
		// CLI tool picks it up without per-tool configuration.
		for _, k := range []string{"HTTP_PROXY", "HTTPS_PROXY", "http_proxy", "https_proxy"} {
			envs = append(envs, map[string]any{"name": k, "value": c.ProxyURL})
		}
	}
	// Agent-container-only env: the `kivali mcp` subprocess inside
	// this container picks up DEV_SHELL_SOCKET and routes run_shell
	// and the file_* tools through the sidecar's UDS; this container
	// has no /files to run them against.
	// The sidecar's own envs are built separately below — it
	// doesn't need this var.
	agentEnvs := append([]map[string]any{}, envs...)
	agentEnvs = append(agentEnvs, map[string]any{
		"name":  "DEV_SHELL_SOCKET",
		"value": DevShellSocketPath(),
	})
	cpuReq := c.CPURequest
	if cpuReq == "" {
		cpuReq = "50m"
	}
	memReq := c.MemoryRequest
	if memReq == "" {
		memReq = "128Mi"
	}
	cpuLim := c.CPULimit
	if cpuLim == "" {
		cpuLim = "2"
	}
	memLim := c.MemoryLimit
	if memLim == "" {
		// 4Gi, not 2Gi: the pod holds the parent's long-lived `claude`
		// CLI plus every concurrently running subagent CLI, each a
		// Node process in the several-hundred-MiB range with its own
		// `kivali mcp` child. SubagentMaxBatch is 5, so a single
		// batched dispatch already puts six CLIs in this cgroup —
		// enough to push a 2Gi limit into OOM-kill territory, and the
		// kernel picks its victim by RSS, which is usually the PARENT.
		// Losing the durable agent because an ephemeral one got greedy
		// is the failure this headroom exists to prevent.
		memLim = "4Gi"
	}

	socketPath := c.UDSDir + "/" + agentpodSocketName

	mounts := []map[string]any{
		// The agent runtime's half of the per-agent PVC: CLI session
		// files, subagent run dirs (their mcp-config.json), the read
		// cache. The dev-shell mounts the other half at the same path
		// and cannot reach this one; see scratch.go.
		{"name": "scratch", "mountPath": "/scratch", "subPath": ScratchAgentSubPath},
		// UDS mount: read-write because the agent's Client opens
		// connections to the socket file (and stat'ing it on
		// startup). The socket file itself is created + owned by
		// the Kivali web pod; the agent pod is a client.
		{"name": "uds", "mountPath": c.UDSDir},
		// Dev-shell IPC: emptyDir shared with the sidecar so the
		// agent runtime can dial the daemon's UDS. Distinct from
		// /scratch so agents don't see the dispatch machinery in
		// the working directory of their shell commands.
		{"name": devShellSockVolumeName, "mountPath": devShellSocketDir},
	}
	volumes := []map[string]any{
		{
			"name": "scratch",
			"persistentVolumeClaim": map[string]any{
				"claimName": PVCName(slug),
			},
		},
		{
			"name": "uds",
			"hostPath": map[string]any{
				"path": c.UDSDir,
				"type": "DirectoryOrCreate",
			},
		},
		{
			// emptyDir for the dev-shell IPC socket. Pod-local +
			// ephemeral; lives only as long as the pod. Both the
			// agent and dev-shell containers mount it at
			// devShellSocketDir, so the daemon can listen and the
			// runtime can dial without exposing the path on a PVC
			// agents browse.
			"name":     devShellSockVolumeName,
			"emptyDir": map[string]any{},
		},
	}
	// Provider credentials (HomeSubPath; claude-home for the Claude
	// CLI). The Kivali PVC's claude-home subdir
	// holds whatever the CLI's sign-in in the server container wrote —
	// .claude/.credentials.json (a subscription's OAuth), .claude.json,
	// .claude/settings.json (the env block of a Bedrock or Vertex
	// sign-in, or of a sign-in setup, which the supervisor writes),
	// and any cloud credential files kept under that HOME — then read by
	// every claude subprocess. Mounting it into
	// agent pods gives every agent's claude the same HOME, and so the
	// same sign-in. Per-session subdirs
	// (.claude/projects/, .claude/sessions/) are session-id-keyed
	// so N parallel agents don't collide; .credentials.json refresh
	// is idempotent (the new token replaces the old). Same-node
	// scheduling (podAffinity below) lets RWO PVCs be mounted by
	// multiple pods. Mount is omitted when DataClaimName is
	// empty — fixtures that don't exercise the CLI stay minimal.
	//
	// It is the agent container's only mount of the Kivali PVC. The
	// agent's /files/ and the /data mounts its farms resolve through
	// belong to the dev-shell container alone, where run_shell and the
	// file_* tools execute: nothing an agent can write, or plant a
	// symlink in, is mounted beside the credentials.
	if c.DataClaimName != "" {
		mounts = append(mounts, map[string]any{
			"name":      c.HomeSubPath,
			"mountPath": "/data/" + c.HomeSubPath,
			"subPath":   c.HomeSubPath,
		})
		// Two volumes name the one PVC: the provider home for the agent
		// container, kivali-data for the dev-shell's subPath mounts.
		volumes = append(volumes, map[string]any{
			"name": c.HomeSubPath,
			"persistentVolumeClaim": map[string]any{
				"claimName": c.DataClaimName,
			},
		})
		if c.FilesSubPath != nil && c.FilesSubPath(slug) != "" {
			volumes = append(volumes, map[string]any{
				"name": dataVolumeName,
				"persistentVolumeClaim": map[string]any{
					"claimName": c.DataClaimName,
				},
			})
		}
	}

	container := map[string]any{
		"name":            "agent",
		"image":           c.Image,
		"imagePullPolicy": c.ImagePullPolicy,
		"command":         []string{"kivali"},
		"args": []string{
			"agent",
			"--slug=" + slug,
			"--uds=" + socketPath,
		},
		"env": agentEnvs,
		"resources": map[string]any{
			"requests": map[string]any{"cpu": cpuReq, "memory": memReq},
			"limits":   map[string]any{"cpu": cpuLim, "memory": memLim},
		},
		"volumeMounts": mounts,
	}

	// dev-shell sidecar — see docs/developers/files-and-publishing.md. It runs
	// the dev tooling (python, jq, git, etc.) the slim Kivali
	// runtime image deliberately omits, and it is where run_shell and
	// every file_* tool execute: the one container that mounts the
	// agent's /files/ and the /data mounts its farms resolve through.
	// UDS at /run/kivali-dev-shell/sock — the agent runtime's MCP
	// server dials it for every run_shell and file_* call. Always
	// present: the image identifier is derived from c.Image at the
	// same tag, so a release ships both images atomically and there's
	// no "is this configured" branch to test.
	devMounts := []map[string]any{
		// Shared IPC: the daemon listens on a socket file inside
		// this dir; the agent runtime mounts the same emptyDir at
		// the same path and dials.
		{"name": devShellSockVolumeName, "mountPath": devShellSocketDir},
		// /scratch is the shell's half of the per-agent PVC (the
		// agent container's half is not mounted here; see
		// scratch.go). The dev-shell uses it as the persistence
		// root: $HOME = /scratch/home so pip --user installs,
		// ~/.bashrc edits, pyenv state, etc. survive pod restart +
		// chat rotation. Mount is RW (default).
		{"name": "scratch", "mountPath": "/scratch", "subPath": ScratchShellSubPath},
	}
	if c.DataClaimName != "" && c.FilesSubPath != nil {
		filesSubPath := c.FilesSubPath(slug)
		if filesSubPath != "" {
			// The agent's /files (RW: artifacts/, notes, etc.), with
			// nested RO mounts over the core-managed subtrees — see
			// filesMounts. subPath chosen per-slug at provision time
			// (FilesSubPath).
			devMounts = append(devMounts, filesMounts(filesSubPath, slug)...)
			devMounts = append(devMounts,
				// Targets for the /files/ symlink farms, read-only.
				// Every subtree under /files/ that's symlinked
				// elsewhere on the PVC needs the link's target mounted
				// here, or `ls /files/<sub>/...` and file_view hit
				// ENOENT. They are also the only places outside
				// /files/ the file_* tools follow a link into (the
				// daemon's --agent names them: files.PodReadRoots).
				//
				//   /data/attachments        ← /files/attachments/
				//   /data/skills             ← /files/skills/
				//   /data/project_files      ← /files/project/
				//   /data/agents/<slug>/chats ← /files/past-chats/
				//
				// Skills + project_files are globally shared (org-wide
				// RO library); attachments + chats are per-agent
				// subPaths so cross-agent isolation holds: an in-pod
				// `ls /data/attachments/` can never enumerate other
				// agents' SHAs (see SyncAgentAttachmentLinks).
				map[string]any{
					"name":      dataVolumeName,
					"mountPath": "/data/attachments",
					"subPath":   "agents/" + slug + "/attachments",
					"readOnly":  true,
				},
				map[string]any{
					"name":      dataVolumeName,
					"mountPath": "/data/skills",
					"subPath":   "skills",
					"readOnly":  true,
				},
				map[string]any{
					"name":      dataVolumeName,
					"mountPath": "/data/project_files",
					"subPath":   "project_files",
					"readOnly":  true,
				},
				map[string]any{
					"name":      dataVolumeName,
					"mountPath": "/data/agents/" + slug + "/chats",
					"subPath":   "agents/" + slug + "/chats",
					"readOnly":  true,
				},
			)
		}
	}
	// HOME on the per-agent PVC + BASH_ENV pointing at an editable
	// shim file is the persistence model. The Dockerfile defaults
	// these (ENV HOME=/scratch/home / BASH_ENV=/scratch/home/.shell_env);
	// we set them explicitly here so a future image-default change
	// can't silently break persistence.
	devEnvs := append([]map[string]any{}, envs...)
	devEnvs = append(devEnvs,
		map[string]any{"name": "HOME", "value": "/scratch/home"},
		map[string]any{"name": "BASH_ENV", "value": "/scratch/home/.shell_env"},
	)

	devContainer := map[string]any{
		"name":            "dev-shell",
		"image":           DevShellImageFor(c.Image),
		"imagePullPolicy": c.ImagePullPolicy,
		// The daemon binary defaults its own --socket and --root
		// to /run/kivali-dev-shell/sock + /files; pass them
		// explicitly anyway so a future flag-default change in
		// cmd/dev-shell doesn't silently change behavior here.
		// --agent names the per-agent /data mounts above.
		"args": []string{
			"--socket=" + DevShellSocketPath(),
			"--root=/files",
			"--agent=" + slug,
		},
		"env": devEnvs,
		"resources": map[string]any{
			// Dev-shell is sized for build/run workloads and is
			// independent of the runtime container. Bigger CPU
			// burst (tests, conversions, any toolchain an operator
			// bakes in); same memory ceiling — kubelet
			// kills a runaway command via cgroup without taking
			// the agent runtime down with it.
			"requests": map[string]any{"cpu": "100m", "memory": "256Mi"},
			"limits":   map[string]any{"cpu": "4", "memory": "4Gi"},
		},
		"volumeMounts": devMounts,
	}
	containers := []any{container, devContainer}

	// The one container that mounts the whole per-agent volume. It
	// runs to completion before either container above starts, so it
	// is also what makes their two subPaths exist, as real directories
	// owned by the pod's user (kubelet would make a missing one as
	// root). See PrepareScratch.
	initContainer := map[string]any{
		"name":            "prepare-scratch",
		"image":           c.Image,
		"imagePullPolicy": c.ImagePullPolicy,
		"command":         []string{"kivali"},
		"args":            []string{"prepare-scratch", "--root=" + ScratchVolumeMountPath},
		"resources": map[string]any{
			"requests": map[string]any{"cpu": "10m", "memory": "32Mi"},
			"limits":   map[string]any{"cpu": "500m", "memory": "128Mi"},
		},
		"volumeMounts": []map[string]any{
			{"name": "scratch", "mountPath": ScratchVolumeMountPath},
		},
	}

	spec := map[string]any{
		// No business talking to the k8s API.
		"automountServiceAccountToken":  false,
		"initContainers":                []any{initContainer},
		"containers":                    containers,
		"volumes":                       volumes,
		"restartPolicy":                 "Always",
		"terminationGracePeriodSeconds": 10,
		// Same-node affinity to Kivali web. Required (not preferred)
		// because the hostPath UDS dir only exists on Kivali web's
		// node — scheduling elsewhere produces a pod that can never
		// dial core.
		"affinity": map[string]any{
			"podAffinity": map[string]any{
				"requiredDuringSchedulingIgnoredDuringExecution": []map[string]any{
					{
						"labelSelector": map[string]any{
							"matchLabels": c.WebSelector,
						},
						"topologyKey": "kubernetes.io/hostname",
					},
				},
			},
		},
	}
	if c.ServiceAccountName != "" {
		spec["serviceAccountName"] = c.ServiceAccountName
	}

	manifest := map[string]any{
		"apiVersion": "v1",
		"kind":       "Pod",
		"metadata": map[string]any{
			"name":      PodName(slug),
			"namespace": c.Namespace,
			"labels": map[string]string{
				"app":   "kivali-agentpod",
				"agent": slug,
			},
		},
		"spec": spec,
	}
	// Stamp a stable hash of the spec content as a label so a future
	// Provision call can detect drift — agent pods are long-lived and
	// don't auto-update with the Kivali image. Without this, every
	// agentpod-spec change (mounts, env, command) would leave a stale
	// fleet running on the previous spec until manually deleted. With
	// the label, Kivali web on boot recreates any pod whose label
	// doesn't match the current manifest. See provisioner.Provision.
	manifest["metadata"].(map[string]any)["labels"].(map[string]string)[specHashLabel] = manifestSpecHash(manifest)
	return manifest
}

// specHashLabel keys the per-pod label that records the hash of the
// manifest the pod was created from. The kivali- prefix keeps it from
// colliding with other labels; it has no domain prefix because no
// domain is shared by every install.
const specHashLabel = "kivali-spec-hash"

// manifestSpecHash returns a stable 12-hex-char hash of the manifest
// content. The hash MUST be computed on a manifest that does not
// already carry the spec-hash label (chicken-and-egg) — so callers
// only ever invoke this on a freshly-built manifest from podManifest
// before injection.
//
// json.Marshal of map[string]any emits keys in sorted order so the
// hash is deterministic across builds and Go versions; podManifest
// itself is pure (no randomness, no time), so the same input
// produces the same hash.
func manifestSpecHash(manifest map[string]any) string {
	b, err := json.Marshal(manifest)
	if err != nil {
		// Should never happen — manifest is a plain map of basic types
		// produced by podManifest. If it does, return empty so drift
		// detection re-runs (recreate-once semantics) rather than
		// panicking the Kivali boot path.
		return ""
	}
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:6])
}
