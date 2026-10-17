package browserpod

import (
	"encoding/json"
	"reflect"
	"regexp"
	"strings"
	"testing"

	"github.com/kivali-ai/kivali/internal/kube"
)

func testConfig() ProvisionerConfig {
	c := DefaultsForNamespace("kivali-test")
	c.Image = "ghcr.io/kivali-ai/kivali-browser:test"
	return c
}

// roundTrip is the manifest as the API server sees it: JSON.
func roundTrip(t *testing.T, m map[string]any) map[string]any {
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

func podSpec(t *testing.T, pod map[string]any) map[string]any {
	t.Helper()
	return roundTrip(t, pod)["spec"].(map[string]any)
}

func onlyContainer(t *testing.T, spec map[string]any) map[string]any {
	t.Helper()
	cs := spec["containers"].([]any)
	if len(cs) != 1 {
		t.Fatalf("want one container, got %d", len(cs))
	}
	return cs[0].(map[string]any)
}

func envOf(c map[string]any) map[string]string {
	out := map[string]string{}
	for _, e := range c["env"].([]any) {
		m := e.(map[string]any)
		out[m["name"].(string)] = m["value"].(string)
	}
	return out
}

func labelsOf(pod map[string]any) map[string]string {
	return pod["metadata"].(map[string]any)["labels"].(map[string]string)
}

// The pod never listens, never talks to the Kubernetes API, keeps
// nothing on a persistent volume, and kubelet never restarts it.
func TestPodManifestShape(t *testing.T) {
	c := testConfig()
	pod := c.podManifest("market-analyst", "research", "tok-1")

	meta := pod["metadata"].(map[string]any)
	if got, want := meta["name"], PodName("market-analyst", "research"); got != want {
		t.Errorf("name = %v, want %v", got, want)
	}
	if meta["namespace"] != "kivali-test" {
		t.Errorf("namespace = %v", meta["namespace"])
	}
	labels := labelsOf(pod)
	if labels["app"] != "kivali-browserpod" || labels["agent"] != "market-analyst" || labels["session"] != SessionHash("research") {
		t.Errorf("labels = %v", labels)
	}
	if labels[kube.SpecHashLabel] == "" {
		t.Error("no spec-hash label")
	}

	spec := podSpec(t, pod)
	if spec["automountServiceAccountToken"] != false {
		t.Errorf("automountServiceAccountToken = %v, want false", spec["automountServiceAccountToken"])
	}
	if spec["restartPolicy"] != "Never" {
		t.Errorf("restartPolicy = %v, want Never", spec["restartPolicy"])
	}
	if spec["terminationGracePeriodSeconds"] != float64(60) {
		t.Errorf("terminationGracePeriodSeconds = %v, want 60", spec["terminationGracePeriodSeconds"])
	}
	if _, ok := spec["serviceAccountName"]; ok {
		t.Error("serviceAccountName set without a configured one")
	}

	ctr := onlyContainer(t, spec)
	if ctr["name"] != "browser" || ctr["image"] != "ghcr.io/kivali-ai/kivali-browser:test" || ctr["imagePullPolicy"] != "IfNotPresent" {
		t.Errorf("container = %v %v %v", ctr["name"], ctr["image"], ctr["imagePullPolicy"])
	}
	// Under dumb-init, the image's init, so Chromium's orphaned helpers
	// are reaped; `kivali browser` itself is never PID 1.
	if !reflect.DeepEqual(ctr["command"], []any{"/usr/bin/dumb-init", "--", "kivali", "browser"}) {
		t.Errorf("command = %v", ctr["command"])
	}
	if _, ok := ctr["ports"]; ok {
		t.Errorf("container exposes ports: %v", ctr["ports"])
	}
	wantSec := map[string]any{
		"runAsUser": float64(65532), "runAsGroup": float64(65532), "runAsNonRoot": true,
		"allowPrivilegeEscalation": false,
		"capabilities":             map[string]any{"drop": []any{"ALL"}},
	}
	if !reflect.DeepEqual(ctr["securityContext"], wantSec) {
		t.Errorf("securityContext = %v", ctr["securityContext"])
	}
	wantRes := map[string]any{
		"requests": map[string]any{"cpu": "100m", "memory": "512Mi"},
		"limits":   map[string]any{"cpu": "2", "memory": "2Gi"},
	}
	if !reflect.DeepEqual(ctr["resources"], wantRes) {
		t.Errorf("resources = %v", ctr["resources"])
	}

	// Mounts: the browser subdirectory of the UDS dir, read-only and
	// never the directory with core.sock in it, memory-backed /dev/shm,
	// and /tmp. No PVC.
	mounts := map[string]string{}
	for _, m := range ctr["volumeMounts"].([]any) {
		mm := m.(map[string]any)
		mounts[mm["mountPath"].(string)] = mm["name"].(string)
		if mm["name"] == "uds" && mm["readOnly"] != true {
			t.Errorf("uds mount is writable: %v", mm)
		}
	}
	if want := map[string]string{"/var/run/kivali/uds/browser": "uds", "/dev/shm": "shm", "/tmp": "tmp"}; !reflect.DeepEqual(mounts, want) {
		t.Errorf("mounts = %v, want %v", mounts, want)
	}
	vols := map[string]map[string]any{}
	for _, v := range spec["volumes"].([]any) {
		vm := v.(map[string]any)
		if _, ok := vm["persistentVolumeClaim"]; ok {
			t.Errorf("volume %v is a PVC", vm["name"])
		}
		vols[vm["name"].(string)] = vm
	}
	if want := map[string]any{"path": "/var/run/kivali/uds/browser", "type": "Directory"}; !reflect.DeepEqual(vols["uds"]["hostPath"], want) {
		t.Errorf("uds volume = %v", vols["uds"])
	}
	if want := map[string]any{"medium": "Memory", "sizeLimit": "1Gi"}; !reflect.DeepEqual(vols["shm"]["emptyDir"], want) {
		t.Errorf("shm volume = %v", vols["shm"])
	}
	if want := map[string]any{"sizeLimit": "4Gi"}; !reflect.DeepEqual(vols["tmp"]["emptyDir"], want) {
		t.Errorf("tmp volume = %v", vols["tmp"])
	}

	// Required same-node affinity to the web pod.
	terms := spec["affinity"].(map[string]any)["podAffinity"].(map[string]any)["requiredDuringSchedulingIgnoredDuringExecution"].([]any)
	if len(terms) != 1 {
		t.Fatalf("affinity terms = %v", terms)
	}
	term := terms[0].(map[string]any)
	if term["topologyKey"] != "kubernetes.io/hostname" {
		t.Errorf("topologyKey = %v", term["topologyKey"])
	}
	if want := map[string]any{"matchLabels": map[string]any{"app": "kivali"}}; !reflect.DeepEqual(term["labelSelector"], want) {
		t.Errorf("labelSelector = %v", term["labelSelector"])
	}
}

// The pod is configured by the protocol's env names, and nothing else:
// no proxy, since browser pods reach the web directly.
func TestPodManifestEnv(t *testing.T) {
	c := testConfig()
	env := envOf(onlyContainer(t, podSpec(t, c.podManifest("alice", "s 1", "tok"))))
	want := map[string]string{
		"KIVALI_BROWSER_SLUG":     "alice",
		"KIVALI_BROWSER_SESSION":  "s 1",
		"KIVALI_BROWSER_TOKEN":    "tok",
		"KIVALI_BROWSER_CORE_UDS": "/var/run/kivali/uds/browser/browser.sock",
	}
	if !reflect.DeepEqual(env, want) {
		t.Errorf("env = %v, want %v", env, want)
	}
}

func TestPodManifestResourcesFromConfig(t *testing.T) {
	c := testConfig()
	c.CPURequest, c.MemoryRequest, c.CPULimit, c.MemoryLimit = "1", "1Gi", "4", "8Gi"
	c.ServiceAccountName = "kivali-browser"
	spec := podSpec(t, c.podManifest("alice", "s", "tok"))
	want := map[string]any{
		"requests": map[string]any{"cpu": "1", "memory": "1Gi"},
		"limits":   map[string]any{"cpu": "4", "memory": "8Gi"},
	}
	if got := onlyContainer(t, spec)["resources"]; !reflect.DeepEqual(got, want) {
		t.Errorf("resources = %v", got)
	}
	if spec["serviceAccountName"] != "kivali-browser" || spec["automountServiceAccountToken"] != false {
		t.Errorf("service account = %v, automount = %v", spec["serviceAccountName"], spec["automountServiceAccountToken"])
	}
}

func TestPodName(t *testing.T) {
	if got, want := PodName("alice", "research"), "browserpod-alice-"+SessionHash("research"); got != want {
		t.Errorf("PodName = %q, want %q", got, want)
	}
	// The first 12 hex characters of sha256("research").
	if got := SessionHash("research"); got != "66f62d1807d3" {
		t.Errorf("SessionHash(research) = %q, want 66f62d1807d3", got)
	}
	if PodName("alice", "a") == PodName("alice", "b") {
		t.Error("two sessions share a pod name")
	}

	// A long slug is shortened; the hash is kept whole.
	long := strings.Repeat("abcdefghij", 8)
	name := PodName(long, "research")
	if len(name) > 63 {
		t.Errorf("PodName(long) = %q, %d chars", name, len(name))
	}
	if !strings.HasSuffix(name, "-"+SessionHash("research")) || !strings.HasPrefix(name, "browserpod-abcdefghij") {
		t.Errorf("PodName(long) = %q", name)
	}
	// Shortening never leaves a hyphen before the separator.
	name = PodName(strings.Repeat("a", 38)+"-bbbb", "s")
	if strings.Contains(name, "--") || len(name) > 63 {
		t.Errorf("PodName(hyphen at cut) = %q", name)
	}
	// Two long slugs with one head are two pods: the cut carries a hash
	// of the whole slug.
	head := strings.Repeat("a", 60)
	one, two := PodName(head+"-one", "s"), PodName(head+"-two", "s")
	if one == two || len(one) > 63 || len(two) > 63 {
		t.Errorf("PodName(long, one) = %q, PodName(long, two) = %q", one, two)
	}
	if again := PodName(head+"-one", "s"); again != one {
		t.Errorf("PodName is not stable: %q then %q", one, again)
	}
	for _, n := range []string{one, two} {
		if !regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`).MatchString(n) {
			t.Errorf("PodName(long) = %q is not a DNS label", n)
		}
	}
}

// The agent label fits a label value and stays apart for long slugs
// with one head; a slug that fits is the label as it is.
func TestPodManifestAgentLabel(t *testing.T) {
	c := testConfig()
	if got := labelsOf(c.podManifest("alice", "s", "tok"))["agent"]; got != "alice" {
		t.Errorf("agent label = %q", got)
	}
	head := strings.Repeat("a", 70)
	one, two := labelsOf(c.podManifest(head+"-one", "s", "tok"))["agent"], labelsOf(c.podManifest(head+"-two", "s", "tok"))["agent"]
	if one == two || len(one) > 63 || len(two) > 63 || !strings.HasPrefix(one, strings.Repeat("a", 50)) {
		t.Errorf("agent labels for long slugs: %q, %q", one, two)
	}
	for _, v := range []string{one, two} {
		if !regexp.MustCompile(`^[A-Za-z0-9]([-A-Za-z0-9_.]*[A-Za-z0-9])?$`).MatchString(v) {
			t.Errorf("agent label %q is not a label value", v)
		}
	}
}

// The spec hash is stable for the same inputs and changes with the
// token or the image, so Start replaces a pod built for another.
func TestPodManifestSpecHash(t *testing.T) {
	c := testConfig()
	base := labelsOf(c.podManifest("alice", "s", "tok"))[kube.SpecHashLabel]
	if again := labelsOf(c.podManifest("alice", "s", "tok"))[kube.SpecHashLabel]; again != base {
		t.Errorf("hash unstable: %q then %q", base, again)
	}
	if other := labelsOf(c.podManifest("alice", "s", "tok2"))[kube.SpecHashLabel]; other == base {
		t.Error("hash unchanged by a new token")
	}
	c2 := c
	c2.Image = "ghcr.io/kivali-ai/kivali-browser:other"
	if other := labelsOf(c2.podManifest("alice", "s", "tok"))[kube.SpecHashLabel]; other == base {
		t.Error("hash unchanged by a new image")
	}
}

func TestBrowserImageFor(t *testing.T) {
	cases := map[string]string{
		"kivali:dev":                      "kivali-browser:dev",
		"ghcr.io/kivali-ai/kivali:v1.2.0": "ghcr.io/kivali-ai/kivali-browser:v1.2.0",
		"localhost:5000/kivali":           "localhost:5000/kivali-browser:latest",
	}
	for in, want := range cases {
		if got := BrowserImageFor(in); got != want {
			t.Errorf("BrowserImageFor(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestConfigValidate(t *testing.T) {
	if err := testConfig().validate(); err != nil {
		t.Fatalf("defaults with an image: %v", err)
	}
	for name, mut := range map[string]func(*ProvisionerConfig){
		"namespace": func(c *ProvisionerConfig) { c.Namespace = "" },
		"image":     func(c *ProvisionerConfig) { c.Image = " " },
		"uds":       func(c *ProvisionerConfig) { c.UDSDir = "" },
		"selector":  func(c *ProvisionerConfig) { c.WebSelector = nil },
	} {
		c := testConfig()
		mut(&c)
		if err := c.validate(); err == nil {
			t.Errorf("missing %s validated", name)
		}
	}
}
