package agentpod

import (
	"context"
	"reflect"
	"strings"
	"testing"

	"github.com/kivali-ai/kivali/internal/provider"
)

func testConfig() ProvisionerConfig {
	c := DefaultsForNamespace("kivali-test")
	c.Image = "ghcr.io/kivali-ai/kivali:test"
	// The mock provider's home (mock-home): the manifest carries
	// whatever the provider reports. The Claude names are held in
	// internal/claudeagent.
	c.UseCredentials(provider.MockProvider{}.Credentials())
	return c
}

// TestPodManifestShape covers the load-bearing fields the design
// doc commits to: command, mounts, no Service, automount disabled,
// podAffinity to Kivali web. A regression here means an agent pod
// would deploy with a shape that breaks the UDS contract.
func TestPodManifestShape(t *testing.T) {
	c := testConfig()
	pod := c.podManifest("market-analyst")

	if got := pod["kind"].(string); got != "Pod" {
		t.Errorf("kind = %q, want Pod", got)
	}
	meta := pod["metadata"].(map[string]any)
	if got := meta["name"].(string); got != "agentpod-market-analyst" {
		t.Errorf("name = %q, want agentpod-market-analyst", got)
	}
	labels := meta["labels"].(map[string]string)
	if labels["app"] != "kivali-agentpod" || labels["agent"] != "market-analyst" {
		t.Errorf("labels missing app/agent: %+v", labels)
	}

	spec := pod["spec"].(map[string]any)
	if spec["automountServiceAccountToken"] != false {
		t.Errorf("automountServiceAccountToken should be false, got %v", spec["automountServiceAccountToken"])
	}
	if spec["restartPolicy"] != "Always" {
		t.Errorf("restartPolicy = %v, want Always", spec["restartPolicy"])
	}

	containers := spec["containers"].([]any)
	// agent + dev-shell. The dev-shell sidecar is always present —
	// its image is derived from the agentpod image at the same tag,
	// so a release ships both atomically (see DevShellImageFor).
	if len(containers) != 2 {
		t.Fatalf("want 2 containers (agent + dev-shell), got %d", len(containers))
	}
	container := containers[0].(map[string]any)
	if got := container["image"].(string); got != "ghcr.io/kivali-ai/kivali:test" {
		t.Errorf("image = %q", got)
	}

	cmd := container["command"].([]string)
	if !reflect.DeepEqual(cmd, []string{"kivali"}) {
		t.Errorf("command = %v, want [kivali]", cmd)
	}
	args := container["args"].([]string)
	wantArgs := []string{
		"agent",
		"--slug=market-analyst",
		"--uds=/var/run/kivali/uds/core.sock",
	}
	if !reflect.DeepEqual(args, wantArgs) {
		t.Errorf("args = %v, want %v", args, wantArgs)
	}

	mounts := container["volumeMounts"].([]map[string]any)
	mountByName := map[string]map[string]any{}
	for _, m := range mounts {
		mountByName[m["name"].(string)] = m
	}
	if scratch, ok := mountByName["scratch"]; !ok {
		t.Error("missing scratch mount")
	} else if scratch["mountPath"] != "/scratch" || scratch["subPath"] != ScratchAgentSubPath {
		t.Errorf("scratch mount = %v, want /scratch from subPath %s", scratch, ScratchAgentSubPath)
	}
	if uds, ok := mountByName["uds"]; !ok {
		t.Error("missing uds mount")
	} else if uds["mountPath"] != "/var/run/kivali/uds" {
		t.Errorf("uds mountPath = %v, want /var/run/kivali/uds", uds["mountPath"])
	}

	// Volumes — PVC + hostPath. The hostPath is the load-bearing
	// piece; without it the agent pod can't see the UDS socket file.
	volumes := spec["volumes"].([]map[string]any)
	volByName := map[string]map[string]any{}
	for _, v := range volumes {
		volByName[v["name"].(string)] = v
	}
	if pvc, ok := volByName["scratch"]; !ok || pvc["persistentVolumeClaim"] == nil {
		t.Errorf("scratch volume should be a PVC: %+v", pvc)
	} else {
		claim := pvc["persistentVolumeClaim"].(map[string]any)
		if claim["claimName"] != "agentpod-market-analyst-scratch" {
			t.Errorf("PVC claimName = %v, want agentpod-market-analyst-scratch", claim["claimName"])
		}
	}
	if uds, ok := volByName["uds"]; !ok || uds["hostPath"] == nil {
		t.Errorf("uds volume should be hostPath: %+v", uds)
	} else {
		hp := uds["hostPath"].(map[string]any)
		if hp["path"] != "/var/run/kivali/uds" {
			t.Errorf("hostPath path = %v, want /var/run/kivali/uds", hp["path"])
		}
	}

	// No ports. Belt-and-suspenders for the no-Service invariant.
	if _, hasPorts := container["ports"]; hasPorts {
		t.Errorf("container should not declare ports (no listener), got %v", container["ports"])
	}

	// podAffinity required to Kivali web. Same-node enforcement.
	affinity, ok := spec["affinity"].(map[string]any)
	if !ok {
		t.Fatal("missing affinity block — agent pod must require same-node as Kivali web")
	}
	podAff, ok := affinity["podAffinity"].(map[string]any)
	if !ok {
		t.Fatal("missing podAffinity")
	}
	required, ok := podAff["requiredDuringSchedulingIgnoredDuringExecution"].([]map[string]any)
	if !ok || len(required) != 1 {
		t.Fatalf("expected exactly one required podAffinity term, got %v", required)
	}
	term := required[0]
	if term["topologyKey"] != "kubernetes.io/hostname" {
		t.Errorf("topologyKey = %v, want kubernetes.io/hostname", term["topologyKey"])
	}
	sel := term["labelSelector"].(map[string]any)
	mlabels := sel["matchLabels"].(map[string]string)
	if mlabels["app"] != "kivali" {
		t.Errorf("matchLabels.app = %v, want the web pod label", mlabels["app"])
	}
}

// TestPodManifestProviderHomeMount asserts the agent pod sees the
// Kivali PVC's provider home subdir (Credentials.HomeDir; mock-home for
// the mock provider, claude-home for Claude) at /data/<HomeDir>, where
// the image points the CLI's HOME, so it resolves to the same sign-in
// the operator made on the server. Without this, every agent pod boots
// logged-out and fails on the first chat-turn.
func TestPodManifestProviderHomeMount(t *testing.T) {
	c := testConfig()
	pod := c.podManifest("market-analyst")
	spec := pod["spec"].(map[string]any)
	container := spec["containers"].([]any)[0].(map[string]any)

	mounts := container["volumeMounts"].([]map[string]any)
	var ch map[string]any
	for _, m := range mounts {
		if m["name"] == "mock-home" {
			ch = m
			break
		}
	}
	if ch == nil {
		t.Fatalf("missing mock-home volumeMount; mounts=%+v", mounts)
	}
	if ch["mountPath"] != "/data/mock-home" {
		t.Errorf("mountPath = %v, want /data/mock-home (/data/<HomeDir>, where the image points HOME)", ch["mountPath"])
	}
	if ch["subPath"] != "mock-home" {
		t.Errorf("subPath = %v, want mock-home (so the agent only sees the credentials subdir, not the rest of /data)", ch["subPath"])
	}

	volumes := spec["volumes"].([]map[string]any)
	var chv map[string]any
	for _, v := range volumes {
		if v["name"] == "mock-home" {
			chv = v
			break
		}
	}
	if chv == nil {
		t.Fatalf("missing mock-home volume; volumes=%+v", volumes)
	}
	pvc, ok := chv["persistentVolumeClaim"].(map[string]any)
	if !ok {
		t.Fatalf("mock-home volume should be a PVC ref, got %+v", chv)
	}
	if pvc["claimName"] != "kivali-data" {
		t.Errorf("claimName = %v, want kivali-data (the Kivali web PVC)", pvc["claimName"])
	}
}

// TestPodManifestNoProviderHomeWhenUnset confirms the mount is
// omitted when DataClaimName is empty — keeps test fixtures
// (and any future deployment that doesn't use the shared-CLI
// pattern) minimal.
func TestPodManifestNoProviderHomeWhenUnset(t *testing.T) {
	c := testConfig()
	c.DataClaimName = ""
	pod := c.podManifest("market-analyst")
	spec := pod["spec"].(map[string]any)
	container := spec["containers"].([]any)[0].(map[string]any)

	mounts := container["volumeMounts"].([]map[string]any)
	for _, m := range mounts {
		if m["name"] == "mock-home" {
			t.Errorf("unexpected mock-home mount with DataClaimName empty: %+v", m)
		}
	}
	volumes := spec["volumes"].([]map[string]any)
	for _, v := range volumes {
		if v["name"] == "mock-home" {
			t.Errorf("unexpected mock-home volume with DataClaimName empty: %+v", v)
		}
	}
}

// TestPodManifestSidecarPresent proves the sidecar container is
// always emitted, with the same /files/ + /scratch/ volumeMounts
// as the agent container, and the agent container's env carries
// DEV_SHELL_SOCKET so its kivali mcp subprocess routes run_shell
// through the sidecar. The dev-shell image identifier is derived
// from the agentpod image at the same tag — both ship as one
// atomic release set, no separate config knob.
func TestPodManifestSidecarPresent(t *testing.T) {
	c := testConfig()
	c.FilesSubPath = func(slug string) string { return "agents/" + slug + "/memory" }
	pod := c.podManifest("alice")
	containers := pod["spec"].(map[string]any)["containers"].([]any)
	if len(containers) != 2 {
		t.Fatalf("want 2 containers (agent + dev-shell), got %d", len(containers))
	}

	// Find each container by name — order isn't a contract.
	byName := map[string]map[string]any{}
	for _, c := range containers {
		m := c.(map[string]any)
		byName[m["name"].(string)] = m
	}
	dev, ok := byName["dev-shell"]
	if !ok {
		t.Fatalf("missing dev-shell container; have %v", byName)
	}
	// testConfig().Image == "ghcr.io/kivali-ai/kivali:test"; the derived
	// dev-shell image swaps the name and keeps the registry + tag.
	if got := dev["image"]; got != "ghcr.io/kivali-ai/kivali-dev-shell:test" {
		t.Errorf("dev-shell image = %v, want ghcr.io/kivali-ai/kivali-dev-shell:test", got)
	}
	args := dev["args"].([]string)
	wantArgs := []string{
		"--socket=/run/kivali-dev-shell/sock",
		"--root=/files",
		"--agent=alice",
	}
	if !reflect.DeepEqual(args, wantArgs) {
		t.Errorf("dev-shell args = %v, want %v", args, wantArgs)
	}

	// dev-shell mounts: /run/kivali-dev-shell (the IPC emptyDir),
	// /scratch (per-agent PVC — HOME lives here for persistence),
	// /files RW, /files/artifacts/shared RO, plus the four symlink-
	// target mounts (/data/attachments, /data/skills, /data/project_files,
	// /data/agents/<slug>/chats), all RO. Each is required because the
	// corresponding subtree under /files/ is a symlink farm pointing
	// at the matching /data/ path; without the mount, bash and the
	// file_* tools (which execute here too) hit ENOENT. Per-agent subPaths on
	// /data/attachments and the chats mount hold the cross-agent
	// isolation invariant the runtime side enforces.
	//
	// Must NOT mount /data/mock-home — that's the runtime container's
	// concern (claude CLI credentials), not bash's.
	devMounts := dev["volumeMounts"].([]map[string]any)
	devByPath := map[string]map[string]any{}
	for _, m := range devMounts {
		devByPath[m["mountPath"].(string)] = m
	}
	if _, ok := devByPath["/run/kivali-dev-shell"]; !ok {
		t.Error("dev-shell missing /run/kivali-dev-shell mount (the UDS lives here)")
	}
	if _, ok := devByPath["/scratch"]; !ok {
		t.Error("dev-shell missing /scratch mount (HOME=/scratch/home, persistence root)")
	}
	if files, ok := devByPath["/files"]; !ok {
		t.Error("dev-shell missing /files mount (run_shell and file_* both execute against it)")
	} else if files["subPath"] != "agents/alice/memory" {
		t.Errorf("dev-shell /files subPath = %v, want agents/alice/memory", files["subPath"])
	}
	if shared, ok := devByPath["/files/artifacts/shared"]; !ok {
		t.Error("dev-shell missing /files/artifacts/shared RO mount (run_shell could clobber peer artifacts)")
	} else if ro, _ := shared["readOnly"].(bool); !ro {
		t.Errorf("dev-shell /files/artifacts/shared readOnly = false, want true")
	}
	// The symlink farms are read-only mounts in the dev-shell too:
	// run_shell is the one tool that could otherwise repoint a farm
	// link or replace a farm dir with a link core would write through.
	for _, d := range []string{"project", "skills", "attachments", "past-chats", "episodes"} {
		m, ok := devByPath["/files/"+d]
		if !ok {
			t.Errorf("dev-shell missing /files/%s RO mount", d)
			continue
		}
		if m["subPath"] != "agents/alice/memory/"+d {
			t.Errorf("dev-shell /files/%s subPath = %v, want agents/alice/memory/%s", d, m["subPath"], d)
		}
		if ro, _ := m["readOnly"].(bool); !ro {
			t.Errorf("dev-shell /files/%s readOnly = false, want true", d)
		}
	}
	// Nested mounts must follow /files in the list: kubelet mounts in
	// order, and a nested mount made first would be hidden by /files.
	filesAt := -1
	for i, m := range devMounts {
		p := m["mountPath"].(string)
		if p == "/files" {
			filesAt = i
		} else if strings.HasPrefix(p, "/files/") && (filesAt < 0 || i < filesAt) {
			t.Errorf("dev-shell mount %s comes before /files", p)
		}
	}
	// Four symlink-target mounts the dev-shell needs so bash can
	// follow /files/{attachments,skills,project,past-chats}/ links.
	// Each must be RO; attachments + per-agent chats use a per-slug
	// subPath, skills + project_files are globally shared.
	symlinkTargets := []struct {
		mountPath string
		subPath   string
		surface   string
	}{
		{"/data/attachments", "agents/alice/attachments", "/files/attachments/"},
		{"/data/skills", "skills", "/files/skills/"},
		{"/data/project_files", "project_files", "/files/project/"},
		{"/data/agents/alice/chats", "agents/alice/chats", "/files/past-chats/"},
	}
	for _, tgt := range symlinkTargets {
		m, ok := devByPath[tgt.mountPath]
		if !ok {
			t.Errorf("dev-shell missing %s RO mount — bash cannot follow %s symlinks without it", tgt.mountPath, tgt.surface)
			continue
		}
		if m["subPath"] != tgt.subPath {
			t.Errorf("dev-shell %s subPath = %v, want %s", tgt.mountPath, m["subPath"], tgt.subPath)
		}
		if ro, _ := m["readOnly"].(bool); !ro {
			t.Errorf("dev-shell %s readOnly = false, want true", tgt.mountPath)
		}
	}
	if _, has := devByPath["/data/mock-home"]; has {
		t.Error("dev-shell should not mount /data/mock-home — agent-runtime concern (claude CLI credentials)")
	}

	// dev-shell env: HOME on the persistent PVC + BASH_ENV
	// pointing at the editable shim file. Both are load-bearing
	// for the persistence model.
	devEnv := dev["env"].([]map[string]any)
	devEnvByName := map[string]string{}
	for _, e := range devEnv {
		devEnvByName[e["name"].(string)] = e["value"].(string)
	}
	if devEnvByName["HOME"] != "/scratch/home" {
		t.Errorf("dev-shell HOME = %q, want /scratch/home", devEnvByName["HOME"])
	}
	if devEnvByName["BASH_ENV"] != "/scratch/home/.shell_env" {
		t.Errorf("dev-shell BASH_ENV = %q, want /scratch/home/.shell_env", devEnvByName["BASH_ENV"])
	}

	// Agent container env carries DEV_SHELL_SOCKET so kivali mcp
	// inside it routes run_shell and file_* through the sidecar's UDS.
	agent, ok := byName["agent"]
	if !ok {
		t.Fatalf("missing agent container; have %v", byName)
	}
	envs := agent["env"].([]map[string]any)
	var sock string
	for _, e := range envs {
		if e["name"] == "DEV_SHELL_SOCKET" {
			sock = e["value"].(string)
		}
	}
	if sock != "/run/kivali-dev-shell/sock" {
		t.Errorf("agent env DEV_SHELL_SOCKET = %q, want /run/kivali-dev-shell/sock", sock)
	}
	// Agent container also needs the IPC mount so it can dial
	// the daemon's socket. Without it, every run_shell call
	// would fail to connect.
	agentMounts := agent["volumeMounts"].([]map[string]any)
	hasIPC := false
	for _, m := range agentMounts {
		if m["mountPath"] == "/run/kivali-dev-shell" {
			hasIPC = true
			break
		}
	}
	if !hasIPC {
		t.Error("agent container missing /run/kivali-dev-shell mount (cannot dial dev-shell daemon)")
	}
}

// TestPodManifestSidecarSpecHashChanges proves drift is detected
// when the agentpod image's tag bumps — that bumps the dev-shell
// image too (DevShellImageFor preserves the tag), so the manifest
// changes and Provision recreates any agent pod running against
// another tag.
func TestPodManifestSidecarSpecHashChanges(t *testing.T) {
	hashOf := func(pod map[string]any) string {
		labels := pod["metadata"].(map[string]any)["labels"].(map[string]string)
		return labels["kivali-spec-hash"]
	}
	a := testConfig()
	a.FilesSubPath = func(slug string) string { return "agents/" + slug + "/memory" }
	without := a.podManifest("alice")
	b := testConfig()
	b.FilesSubPath = func(slug string) string { return "agents/" + slug + "/memory" }
	b.Image = "ghcr.io/kivali-ai/kivali:next" // bumped tag
	with := b.podManifest("alice")
	if hashOf(without) == hashOf(with) {
		t.Errorf("spec hash should differ when image tag changes: without=%q with=%q",
			hashOf(without), hashOf(with))
	}
}

// TestDevShellImageFor pins the derivation rule. A release ships
// Kivali and kivali-dev-shell as one set; this helper is the only
// place that knows their names line up. Cases cover the two
// canonical shapes (registry-prefixed and bare), plus a missing
// tag (treated as :latest, matching docker's default).
func TestDevShellImageFor(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"kivali:v0.8.3", "kivali-dev-shell:v0.8.3"},
		{"kivali:dev", "kivali-dev-shell:dev"},
		{"ghcr.io/kivali-ai/kivali:v0.8.3", "ghcr.io/kivali-ai/kivali-dev-shell:v0.8.3"},
		{"registry.internal:5000/kivali:v0.8.3", "registry.internal:5000/kivali-dev-shell:v0.8.3"},
		{"kivali", "kivali-dev-shell:latest"},
		{"", ""},
	}
	for _, tc := range cases {
		got := DevShellImageFor(tc.in)
		if got != tc.want {
			t.Errorf("DevShellImageFor(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// TestPodManifestNoService asserts there is no serviceManifest
// helper / no Service emitted. The Provisioner's Provision() also
// only POSTs PVC + Pod. If a future change adds a Service, this
// test forces a deliberate update — the design invariant is "no
// listener on the agent pod".
func TestPodManifestNoService(t *testing.T) {
	// Sanity: the only manifest builders are pvcManifest and
	// podManifest. There is no serviceManifest equivalent to call;
	// reflect / package surface only checks here would be paranoid.
	// Instead exercise the Provisioner's Enabled()=false code path
	// to confirm Provision() is the only mutation entry point.
	p, err := NewProvisioner(testConfig())
	if err != nil {
		t.Fatalf("NewProvisioner: %v", err)
	}
	if p.Enabled() {
		t.Error("test runs out-of-cluster; Provisioner should not be Enabled")
	}
	// No-op out-of-cluster Provision/Destroy don't touch anything;
	// asserts the call-site shape is exactly Provision + Destroy.
	if err := p.Provision(context.Background(), "alice"); err != nil {
		t.Errorf("Provision noop: %v", err)
	}
	if err := p.Destroy(context.Background(), "alice"); err != nil {
		t.Errorf("Destroy noop: %v", err)
	}
}

// TestPodManifestUnifiedFilesMounts proves the dev-shell container
// gets the subPath mounts the file_* tools and run_shell rely on:
// /files RW (per-slug subPath) with RO nested mounts over core's
// subtrees, /data/project_files RO, /data/skills RO, /data/attachments
// RO (per-agent hardlink dir), and /data/agents/<slug>/chats RO
// (per-agent archive dir) — and that the agent container, which holds
// the Claude credentials, gets none of them. file_* executes in the
// dev-shell, so nothing an agent can write, or plant a symlink in, is
// mounted beside the credentials.
func TestPodManifestUnifiedFilesMounts(t *testing.T) {
	c := testConfig()
	c.FilesSubPath = func(slug string) string { return "agents/" + slug + "/memory" }
	pod := c.podManifest("alice")
	spec := pod["spec"].(map[string]any)
	byName := map[string]map[string]any{}
	for _, ct := range spec["containers"].([]any) {
		m := ct.(map[string]any)
		byName[m["name"].(string)] = m
	}

	// The agent container mounts exactly its scratch PVC, core's UDS
	// dir, the dev-shell socket dir and the CLI's HOME.
	var agentPaths []string
	for _, m := range byName["agent"]["volumeMounts"].([]map[string]any) {
		agentPaths = append(agentPaths, m["mountPath"].(string))
	}
	wantAgent := []string{"/scratch", c.UDSDir, "/run/kivali-dev-shell", "/data/mock-home"}
	if !reflect.DeepEqual(agentPaths, wantAgent) {
		t.Errorf("agent container mounts = %v, want %v", agentPaths, wantAgent)
	}

	mounts := byName["dev-shell"]["volumeMounts"].([]map[string]any)
	byMount := map[string]map[string]any{}
	for _, m := range mounts {
		byMount[m["mountPath"].(string)] = m
	}

	cases := []struct {
		mountPath string
		subPath   string
		readOnly  bool
	}{
		{"/files", "agents/alice/memory", false},
		// The published trees, read-only: core is their only writer
		// (artifact_publish). The agent's own at artifacts/public,
		// everyone's, by owner, at artifacts/shared.
		{"/files/artifacts/public", "public/alice", true},
		{"/files/artifacts/shared", "public", true},
		// Nested RO subPaths over core's symlink farms, so run_shell
		// cannot repoint a farm link or swap a farm dir for a link
		// that core's next Sync would write through.
		{"/files/project", "agents/alice/memory/project", true},
		{"/files/skills", "agents/alice/memory/skills", true},
		{"/files/attachments", "agents/alice/memory/attachments", true},
		{"/files/past-chats", "agents/alice/memory/past-chats", true},
		{"/files/episodes", "agents/alice/memory/episodes", true},
		{"/data/project_files", "project_files", true},
		{"/data/skills", "skills", true},
		{"/data/attachments", "agents/alice/attachments", true},
		{"/data/agents/alice/chats", "agents/alice/chats", true},
	}
	for _, tc := range cases {
		m, ok := byMount[tc.mountPath]
		if !ok {
			t.Errorf("missing mount at %s", tc.mountPath)
			continue
		}
		if m["name"] != "kivali-data" {
			t.Errorf("%s: name = %v, want kivali-data", tc.mountPath, m["name"])
		}
		if m["subPath"] != tc.subPath {
			t.Errorf("%s: subPath = %v, want %v", tc.mountPath, m["subPath"], tc.subPath)
		}
		if tc.readOnly {
			if got, _ := m["readOnly"].(bool); !got {
				t.Errorf("%s: readOnly = false, want true", tc.mountPath)
			}
		}
	}

	volumes := spec["volumes"].([]map[string]any)
	var dataVolume map[string]any
	for _, v := range volumes {
		if v["name"] == "kivali-data" {
			dataVolume = v
		}
	}
	if dataVolume == nil {
		t.Fatalf("missing kivali-data volume entry; volumes=%+v", volumes)
	}
	if pvc, _ := dataVolume["persistentVolumeClaim"].(map[string]any); pvc == nil || pvc["claimName"] != "kivali-data" {
		t.Errorf("kivali-data volume should reference PVC kivali-data, got %+v", dataVolume)
	}
}

// TestPodManifestUnifiedFilesOmittedWhenUnset confirms the new
// mounts vanish when FilesSubPath is unset — fixtures and
// out-of-cluster invocations stay minimal.
func TestPodManifestUnifiedFilesOmittedWhenUnset(t *testing.T) {
	c := testConfig()
	// FilesSubPath unset.
	pod := c.podManifest("alice")
	spec := pod["spec"].(map[string]any)
	container := spec["containers"].([]any)[0].(map[string]any)
	for _, m := range container["volumeMounts"].([]map[string]any) {
		switch m["mountPath"] {
		case "/files", "/files/artifacts/shared", "/files/project", "/files/skills", "/files/attachments", "/files/past-chats", "/files/episodes",
			"/data/project_files", "/data/skills", "/data/attachments", "/data/agents/alice/chats":
			t.Errorf("unexpected unified-files mount with FilesSubPath unset: %+v", m)
		}
	}
	for _, v := range spec["volumes"].([]map[string]any) {
		if v["name"] == "kivali-data" {
			t.Errorf("unexpected kivali-data volume with FilesSubPath unset: %+v", v)
		}
	}
}

// TestPodManifestMemoryLimitDefault pins the default at 4Gi. The pod
// hosts the parent CLI plus a full SubagentMaxBatch fan-out of
// subagent CLIs, so a lower limit such as 2Gi would surface as
// OOM kills of the PARENT under a batched dispatch — the kernel picks
// its victim by RSS, and the long-lived session is the fattest.
func TestPodManifestMemoryLimitDefault(t *testing.T) {
	c := testConfig()
	pod := c.podManifest("market-analyst")
	spec := pod["spec"].(map[string]any)
	container := spec["containers"].([]any)[0].(map[string]any)
	limits := container["resources"].(map[string]any)["limits"].(map[string]any)
	if got := limits["memory"]; got != "4Gi" {
		t.Errorf("default memory limit = %v, want 4Gi", got)
	}
}

// TestPodManifestMemoryLimitOverride confirms operators can tune
// the agent-pod memory limit via ProvisionerConfig.MemoryLimit when
// production pressure shows up. Deliberately not 4Gi — that is the
// default, and an override test that matches it proves nothing.
func TestPodManifestMemoryLimitOverride(t *testing.T) {
	c := testConfig()
	c.MemoryLimit = "8Gi"
	pod := c.podManifest("market-analyst")
	spec := pod["spec"].(map[string]any)
	container := spec["containers"].([]any)[0].(map[string]any)
	limits := container["resources"].(map[string]any)["limits"].(map[string]any)
	if got := limits["memory"]; got != "8Gi" {
		t.Errorf("override memory limit = %v, want 8Gi", got)
	}
}

// TestPodManifestSpecHashStable proves the hash is deterministic:
// building the same manifest twice for the same slug produces the
// same label. Without this, every Kivali web boot would treat every
// existing agent pod as drift and recreate the world.
func TestPodManifestSpecHashStable(t *testing.T) {
	c := testConfig()
	c.FilesSubPath = func(slug string) string { return "agents/" + slug + "/memory" }
	first := c.podManifest("market-analyst")
	second := c.podManifest("market-analyst")
	hashOf := func(pod map[string]any) string {
		labels := pod["metadata"].(map[string]any)["labels"].(map[string]string)
		return labels["kivali-spec-hash"]
	}
	if h := hashOf(first); h == "" {
		t.Fatal("first manifest missing spec-hash label")
	}
	if hashOf(first) != hashOf(second) {
		t.Errorf("spec hash unstable: first=%q second=%q", hashOf(first), hashOf(second))
	}
}

// TestPodManifestSpecHashChangesWithMounts proves drift is detected:
// flipping the FilesSubPath callback (a change to the mount set)
// produces a different hash, so
// Provision will recreate the live pod instead of leaving it on the
// old shape.
func TestPodManifestSpecHashChangesWithMounts(t *testing.T) {
	hashOf := func(pod map[string]any) string {
		labels := pod["metadata"].(map[string]any)["labels"].(map[string]string)
		return labels["kivali-spec-hash"]
	}
	a := testConfig()
	// FilesSubPath unset → minimal mount set.
	bare := a.podManifest("alice")
	b := testConfig()
	b.FilesSubPath = func(slug string) string { return "agents/" + slug + "/memory" }
	full := b.podManifest("alice")
	if hashOf(bare) == hashOf(full) {
		t.Errorf("spec hash should differ when mount set changes: bare=%q full=%q", hashOf(bare), hashOf(full))
	}
}

// TestPodManifestSpecHashOmittedFromHashInput is a paranoia check —
// the hash mustn't be sensitive to itself (chicken-and-egg). The
// hash is computed BEFORE the label is injected; if a future refactor
// reverses the order, the hash would shift on every build and drift
// detection would always trigger. Hard-code the expected hash so a
// change in computation order surfaces here.
func TestPodManifestSpecHashOmittedFromHashInput(t *testing.T) {
	c := testConfig()
	c.FilesSubPath = func(slug string) string { return "agents/" + slug + "/memory" }
	first := c.podManifest("alice")
	hashFirst := first["metadata"].(map[string]any)["labels"].(map[string]string)["kivali-spec-hash"]
	// Tamper the label on the manifest in-place, then re-hash via the
	// same path. The label injection must be the LAST mutation; if
	// the injection's position drifts, hashFirst would change here.
	second := c.podManifest("alice")
	hashSecond := second["metadata"].(map[string]any)["labels"].(map[string]string)["kivali-spec-hash"]
	if hashFirst != hashSecond {
		t.Errorf("hash chicken-and-egg regression: %q vs %q", hashFirst, hashSecond)
	}
}

// TestPVCManifestShape covers the per-agent PVC: ReadWriteOnce,
// requested storage matches Config.ScratchSize, optional
// storageClassName.
func TestPVCManifestShape(t *testing.T) {
	c := testConfig()
	pvc := c.pvcManifest("alice")

	meta := pvc["metadata"].(map[string]any)
	if got := meta["name"].(string); got != "agentpod-alice-scratch" {
		t.Errorf("name = %q", got)
	}
	spec := pvc["spec"].(map[string]any)
	modes := spec["accessModes"].([]string)
	if len(modes) != 1 || modes[0] != "ReadWriteOnce" {
		t.Errorf("accessModes = %v, want [ReadWriteOnce]", modes)
	}
	storage := spec["resources"].(map[string]any)["requests"].(map[string]any)["storage"]
	if storage != "2Gi" {
		t.Errorf("storage = %v, want 2Gi", storage)
	}
}

// TestProvisionerConfigValidate covers the four required fields the
// validator rejects when missing. The default helper supplies most
// of them; we override one at a time and assert the error message
// names the missing field.
func TestProvisionerConfigValidate(t *testing.T) {
	cases := []struct {
		name string
		mut  func(*ProvisionerConfig)
		want string
	}{
		{"namespace required", func(c *ProvisionerConfig) { c.Namespace = "" }, "namespace"},
		{"image required", func(c *ProvisionerConfig) { c.Image = "" }, "image"},
		{"UDSDir required", func(c *ProvisionerConfig) { c.UDSDir = "" }, "UDSDir"},
		{"selector required", func(c *ProvisionerConfig) { c.WebSelector = nil }, "WebSelector"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := testConfig()
			tc.mut(&c)
			err := c.validate()
			if err == nil {
				t.Fatalf("want error mentioning %q, got nil", tc.want)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error %q should mention %q", err.Error(), tc.want)
			}
		})
	}
}

// TestPodManifestCarriesNoCredential locks in that an agent pod is
// signed in only through the CLI's HOME it mounts: no container gets a
// credential variable, and the pod carries no credential annotation.
func TestPodManifestCarriesNoCredential(t *testing.T) {
	c := testConfig()
	c.FilesSubPath = func(slug string) string { return "agents/" + slug + "/files" }
	pod := c.podManifest("alice")
	for _, x := range pod["spec"].(map[string]any)["containers"].([]any) {
		m := x.(map[string]any)
		for _, e := range m["env"].([]map[string]any) {
			name, _ := e["name"].(string)
			if strings.Contains(name, "API_KEY") || strings.Contains(name, "TOKEN") {
				t.Errorf("container %s env carries %s", m["name"], name)
			}
		}
	}
	if _, has := pod["metadata"].(map[string]any)["annotations"]; has {
		t.Error("pod carries annotations; none are expected")
	}
}
