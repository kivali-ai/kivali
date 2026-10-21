package integrationpod

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/kivali-ai/kivali/internal/kube"
)

func testConfig() ProvisionerConfig {
	cfg := DefaultsForNamespace("kivali")
	cfg.Image = "ghcr.io/kivali-ai/kivali:v1.2.0"
	cfg.ProxyURL = "http://10.43.0.12:3128"
	return cfg
}

// asJSON round-trips the manifest through JSON, as the API server sees
// it, so the test reads plain maps and slices.
func asJSON(t *testing.T, m map[string]any) map[string]any {
	t.Helper()
	b, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	var out map[string]any
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

func containerNamed(t *testing.T, spec map[string]any, key, name string) map[string]any {
	t.Helper()
	for _, c := range spec[key].([]any) {
		if c := c.(map[string]any); c["name"] == name {
			return c
		}
	}
	t.Fatalf("no %s %q", key, name)
	return nil
}

// The pod's load-bearing properties. It names no Secret and carries no
// secret value, so the connection never reaches the Kubernetes API by
// way of the pod; the token is in the env and in no label, which
// anyone listing pods reads; every mount of a shared thing is
// read-only, so a server that is taken over cannot write the shim, the
// socket directory or the stored bundle; the images are the kivali
// image for the init container and the agent shell's for the server;
// and nothing about it lets it reach the Kubernetes API or another
// node. This fails when any of those changes.
func TestPodManifestLoadBearingProperties(t *testing.T) {
	const token = "tok-0123456789abcdef"
	m := asJSON(t, testConfig().podManifest("hubspot", "1.2.0", token))
	raw, _ := json.Marshal(m)
	for _, ref := range []string{"secretKeyRef", "secretRef", `"secret":`, "envFrom"} {
		if strings.Contains(string(raw), ref) {
			t.Errorf("the pod refers to a Secret: %s", ref)
		}
	}

	meta := m["metadata"].(map[string]any)
	if meta["name"] != "integrationpod-hubspot" || meta["namespace"] != "kivali" {
		t.Errorf("metadata = %v", meta)
	}
	labels := meta["labels"].(map[string]any)
	if labels["app"] != AppLabel || labels["integration"] != "hubspot" || labels[kube.SpecHashLabel] == "" {
		t.Errorf("labels = %v", labels)
	}
	lb, _ := json.Marshal(labels)
	if strings.Contains(string(lb), token) {
		t.Error("a label carries the token")
	}

	spec := m["spec"].(map[string]any)
	if spec["automountServiceAccountToken"] != false || spec["restartPolicy"] != "Always" || spec["terminationGracePeriodSeconds"] != float64(10) {
		t.Errorf("spec = %v", spec)
	}
	if _, ok := spec["hostNetwork"]; ok {
		t.Error("hostNetwork set")
	}
	aff := spec["affinity"].(map[string]any)["podAffinity"].(map[string]any)["requiredDuringSchedulingIgnoredDuringExecution"].([]any)[0].(map[string]any)
	if aff["topologyKey"] != "kubernetes.io/hostname" {
		t.Errorf("affinity = %v", aff)
	}

	shim := containerNamed(t, spec, "initContainers", "shim")
	if shim["image"] != "ghcr.io/kivali-ai/kivali:v1.2.0" || strings.Join(toStrings(shim["command"]), " ") != "cp /usr/local/bin/kivali /shim/kivali" {
		t.Errorf("init container = %v", shim)
	}
	ctr := containerNamed(t, spec, "containers", "integration")
	if ctr["image"] != "ghcr.io/kivali-ai/kivali-agent-shell:v1.2.0" || strings.Join(toStrings(ctr["command"]), " ") != "/shim/kivali integration-shim" {
		t.Errorf("container = %v %v", ctr["image"], ctr["command"])
	}
	if _, ok := ctr["ports"]; ok {
		t.Error("the container exposes ports")
	}
	for _, c := range []map[string]any{shim, ctr} {
		sc := c["securityContext"].(map[string]any)
		if sc["runAsUser"] != float64(65532) || sc["runAsNonRoot"] != true || sc["allowPrivilegeEscalation"] != false ||
			strings.Join(toStrings(sc["capabilities"].(map[string]any)["drop"]), ",") != "ALL" {
			t.Errorf("%s securityContext = %v", c["name"], sc)
		}
	}

	env := map[string]string{}
	for _, e := range ctr["env"].([]any) {
		e := e.(map[string]any)
		env[e["name"].(string)] = e["value"].(string)
	}
	want := map[string]string{
		EnvName: "hubspot", EnvVersion: "1.2.0", EnvToken: token,
		EnvCoreUDS: "/var/run/kivali/uds/integration/integration.sock", EnvBundle: "/bundle",
		"HTTPS_PROXY": "http://10.43.0.12:3128", "https_proxy": "http://10.43.0.12:3128",
		"HTTP_PROXY": "http://10.43.0.12:3128", "http_proxy": "http://10.43.0.12:3128",
	}
	if len(env) != len(want) {
		t.Errorf("env = %v", env)
	}
	for k, v := range want {
		if env[k] != v {
			t.Errorf("env %s = %q, want %q", k, env[k], v)
		}
	}
	if _, ok := env[EnvCommand]; ok {
		t.Error("the pod sets the test hook")
	}

	mounts := map[string]map[string]any{}
	for _, mt := range ctr["volumeMounts"].([]any) {
		mt := mt.(map[string]any)
		mounts[mt["mountPath"].(string)] = mt
	}
	for path, wantRO := range map[string]bool{"/shim": true, "/var/run/kivali/uds/integration": true, "/bundle": true, "/tmp": false} {
		mt, ok := mounts[path]
		if !ok {
			t.Errorf("no mount at %s", path)
			continue
		}
		if ro, _ := mt["readOnly"].(bool); ro != wantRO {
			t.Errorf("%s readOnly = %v, want %v", path, ro, wantRO)
		}
	}
	if len(mounts) != 4 {
		t.Errorf("mounts = %v", mounts)
	}
	if mounts["/bundle"]["subPath"] != "integrations/hubspot/versions/1.2.0/bundle" {
		t.Errorf("bundle subPath = %v", mounts["/bundle"]["subPath"])
	}
	vols := map[string]map[string]any{}
	for _, v := range spec["volumes"].([]any) {
		v := v.(map[string]any)
		vols[v["name"].(string)] = v
	}
	if hp := vols["uds"]["hostPath"].(map[string]any); hp["path"] != "/var/run/kivali/uds/integration" || hp["type"] != "Directory" {
		t.Errorf("uds volume = %v", hp)
	}
	if pvc := vols["bundle"]["persistentVolumeClaim"].(map[string]any); pvc["claimName"] != "kivali-data" || pvc["readOnly"] != true {
		t.Errorf("bundle volume = %v", pvc)
	}
}

// The spec hash moves with what makes a pod another pod (the version,
// the token, the image) and not between two builds of the same one:
// Start keeps a pod whose hash matches and replaces the rest, so a hash
// that moved on its own would restart every integration at every
// reconcile, and one that did not move would leave an old version
// running.
func TestPodManifestSpecHash(t *testing.T) {
	cfg := testConfig()
	hash := func(c ProvisionerConfig, version, token string) string {
		return specHashOf(c.podManifest("hubspot", version, token))
	}
	base := hash(cfg, "1.2.0", "a")
	if base != hash(cfg, "1.2.0", "a") {
		t.Fatal("two builds of one pod differ")
	}
	other := cfg
	other.Image = "ghcr.io/kivali-ai/kivali:v1.3.0"
	for name, h := range map[string]string{
		"version": hash(cfg, "1.2.1", "a"),
		"token":   hash(cfg, "1.2.0", "b"),
		"image":   hash(other, "1.2.0", "a"),
	} {
		if h == base {
			t.Errorf("a new %s keeps the hash", name)
		}
	}
}

func toStrings(v any) []string {
	var out []string
	for _, s := range v.([]any) {
		out = append(out, s.(string))
	}
	return out
}
