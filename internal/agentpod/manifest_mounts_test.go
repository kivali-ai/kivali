package agentpod

import (
	"reflect"
	"testing"
)

// The per-agent PVC is split: the agent container (claude CLI,
// credentials) and the dev-shell (run_shell) each mount their own
// subPath at /scratch, and only the init container mounts the whole
// volume. A shared writable directory between the two would let the
// shell rewrite the CLI's mcp-config or the read cache.
func TestPodManifestScratchSplit(t *testing.T) {
	c := testConfig()
	c.FilesSubPath = func(slug string) string { return "agents/" + slug + "/files" }
	spec := c.podManifest("alice")["spec"].(map[string]any)

	scratchMounts := func(ct map[string]any) []map[string]any {
		var out []map[string]any
		for _, m := range ct["volumeMounts"].([]map[string]any) {
			if m["name"] == "scratch" {
				out = append(out, m)
			}
		}
		return out
	}
	byName := map[string]map[string]any{}
	for _, ct := range spec["containers"].([]any) {
		m := ct.(map[string]any)
		byName[m["name"].(string)] = m
	}
	for name, want := range map[string]string{"agent": ScratchAgentSubPath, "dev-shell": ScratchShellSubPath} {
		got := scratchMounts(byName[name])
		if len(got) != 1 || got[0]["mountPath"] != "/scratch" || got[0]["subPath"] != want {
			t.Errorf("%s scratch mounts = %v, want one at /scratch from subPath %s", name, got, want)
		}
	}
	if ScratchAgentSubPath == ScratchShellSubPath {
		t.Fatal("the two containers must not mount the same half")
	}

	inits := spec["initContainers"].([]any)
	if len(inits) != 1 {
		t.Fatalf("initContainers = %v, want one", inits)
	}
	ic := inits[0].(map[string]any)
	if ic["image"] != c.Image {
		t.Errorf("init image = %v, want %v", ic["image"], c.Image)
	}
	if !reflect.DeepEqual(ic["command"], []string{"kivali"}) ||
		!reflect.DeepEqual(ic["args"], []string{"prepare-scratch", "--root=" + ScratchVolumeMountPath}) {
		t.Errorf("init command = %v %v", ic["command"], ic["args"])
	}
	want := []map[string]any{{"name": "scratch", "mountPath": ScratchVolumeMountPath}}
	if !reflect.DeepEqual(ic["volumeMounts"], want) {
		t.Errorf("init mounts = %v, want only the whole scratch volume", ic["volumeMounts"])
	}
}

// The dev-shell's mounts, in order. /files/artifacts is a RW mount of
// its own so that `mv /files/artifacts ...` fails with EBUSY instead of
// carrying the read-only public/ and shared/ mounts away; it precedes
// the nested mounts inside it, and /files precedes everything below
// it. The published trees come from public/ on the data volume, not
// from the agent's tree.
func TestPodManifestDevShellMountOrder(t *testing.T) {
	c := testConfig()
	c.FilesSubPath = func(slug string) string { return "agents/" + slug + "/files" }
	spec := c.podManifest("alice")["spec"].(map[string]any)
	dev := spec["containers"].([]any)[1].(map[string]any)

	type mount struct {
		path, subPath string
		ro            bool
	}
	var got []mount
	for _, m := range dev["volumeMounts"].([]map[string]any) {
		sp, _ := m["subPath"].(string)
		ro, _ := m["readOnly"].(bool)
		got = append(got, mount{m["mountPath"].(string), sp, ro})
	}
	want := []mount{
		{"/run/kivali-dev-shell", "", false},
		{"/scratch", ScratchShellSubPath, false},
		{"/files", "agents/alice/files", false},
		{"/files/artifacts", "agents/alice/files/artifacts", false},
		{"/files/project", "agents/alice/files/project", true},
		{"/files/past-chats", "agents/alice/files/past-chats", true},
		{"/files/episodes", "agents/alice/files/episodes", true},
		{"/files/skills", "agents/alice/files/skills", true},
		{"/files/attachments", "agents/alice/files/attachments", true},
		{"/files/artifacts/public", "public/alice", true},
		{"/files/artifacts/shared", "public", true},
		{"/data/attachments", "agents/alice/attachments", true},
		{"/data/skills", "skills", true},
		{"/data/project_files", "project_files", true},
		{"/data/agents/alice/chats", "agents/alice/chats", true},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("dev-shell mounts =\n%v\nwant\n%v", got, want)
	}
}

// Every directory between /files and a core-managed subtree becomes a
// mount point, derived from CoreManagedDirs so a new nested subtree
// (artifacts/public/) brings its parent along.
func TestCoreManagedParents(t *testing.T) {
	if got := coreManagedParents(); !reflect.DeepEqual(got, []string{"artifacts"}) {
		t.Errorf("coreManagedParents() = %v, want [artifacts]", got)
	}
}
