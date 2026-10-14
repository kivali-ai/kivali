package agentpod

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"
)

// agentShellContainer returns the agent-shell container of a manifest.
func agentShellContainer(t *testing.T, pod map[string]any) map[string]any {
	t.Helper()
	for _, c := range pod["spec"].(map[string]any)["containers"].([]any) {
		m := c.(map[string]any)
		if m["name"] == "agent-shell" {
			return m
		}
	}
	t.Fatal("no agent-shell container")
	return nil
}

func specHash(pod map[string]any) string {
	return pod["metadata"].(map[string]any)["labels"].(map[string]string)[specHashLabel]
}

// No software: the agent-shell runs as the image's user, as before,
// with privilege escalation off and no --install.
func TestPodManifestWithoutSoftware(t *testing.T) {
	c := testConfig()
	for _, sw := range [][]string{nil, {}} {
		pod := c.podManifestWithSoftware("alice", sw)
		dev := agentShellContainer(t, pod)
		if want := map[string]any{"allowPrivilegeEscalation": false}; !reflect.DeepEqual(dev["securityContext"], want) {
			t.Errorf("securityContext = %v, want %v", dev["securityContext"], want)
		}
		for _, a := range dev["args"].([]string) {
			if strings.HasPrefix(a, "--install") {
				t.Errorf("args carry %q with no software", a)
			}
		}
		if specHash(pod) != specHash(c.podManifest("alice")) {
			t.Error("an empty list changes the spec hash")
		}
	}
}

// Software: the container starts as root to install it, with the list
// sorted and comma-joined, and privilege escalation still off.
func TestPodManifestWithSoftware(t *testing.T) {
	c := testConfig()
	pod := c.podManifestWithSoftware("alice", []string{"poppler-utils", "ffmpeg", "ffmpeg"})
	dev := agentShellContainer(t, pod)
	want := map[string]any{"allowPrivilegeEscalation": false, "runAsUser": 0, "runAsGroup": 0}
	if !reflect.DeepEqual(dev["securityContext"], want) {
		t.Errorf("securityContext = %v, want %v", dev["securityContext"], want)
	}
	args := dev["args"].([]string)
	if got := args[len(args)-1]; got != "--install=ffmpeg,poppler-utils" {
		t.Errorf("last arg = %q, want --install=ffmpeg,poppler-utils", got)
	}
	// The agent container is untouched: it never runs as root.
	for _, ci := range pod["spec"].(map[string]any)["containers"].([]any) {
		m := ci.(map[string]any)
		if m["name"] == "agent" {
			if _, ok := m["securityContext"]; ok {
				t.Errorf("agent container gained a securityContext: %v", m["securityContext"])
			}
		}
	}
}

// The spec hash changes exactly when the set of software does: order
// and duplicates do not count.
func TestPodManifestSoftwareSpecHash(t *testing.T) {
	c := testConfig()
	none := specHash(c.podManifestWithSoftware("alice", nil))
	ab := specHash(c.podManifestWithSoftware("alice", []string{"ffmpeg", "jq"}))
	ba := specHash(c.podManifestWithSoftware("alice", []string{"jq", "ffmpeg", "jq"}))
	a := specHash(c.podManifestWithSoftware("alice", []string{"ffmpeg"}))
	if ab != ba {
		t.Error("order or duplicates changed the hash")
	}
	if none == ab || ab == a || none == a {
		t.Errorf("hashes do not follow the set: none=%s ab=%s a=%s", none, ab, a)
	}
}

// fakeKube is an in-memory API server for one namespace's pods and
// PVCs: GET, POST and DELETE, with a deleted pod gone at once.
type fakeKube struct {
	mu      sync.Mutex
	objects map[string]map[string]any // path → object
	creates []string
	deletes []string
}

func (f *fakeKube) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	switch r.Method {
	case http.MethodGet:
		obj, ok := f.objects[r.URL.Path]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			_, _ = io.WriteString(w, `{"kind":"Status","reason":"NotFound","code":404}`)
			return
		}
		_ = json.NewEncoder(w).Encode(obj)
	case http.MethodPost:
		var obj map[string]any
		_ = json.NewDecoder(r.Body).Decode(&obj)
		name := obj["metadata"].(map[string]any)["name"].(string)
		path := r.URL.Path + "/" + name
		f.objects[path] = obj
		f.creates = append(f.creates, path)
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(obj)
	case http.MethodDelete:
		delete(f.objects, r.URL.Path)
		f.deletes = append(f.deletes, r.URL.Path)
		_, _ = io.WriteString(w, `{}`)
	}
}

func (f *fakeKube) pod(t *testing.T, slug string) map[string]any {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	obj := f.objects["/api/v1/namespaces/kivali/pods/"+PodName(slug)]
	if obj == nil {
		t.Fatalf("no pod for %s", slug)
	}
	return obj
}

func (f *fakeKube) reset() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.creates, f.deletes = nil, nil
}

// testProvisioner is a Provisioner talking to a fakeKube, its software
// resolved from *software.
func testProvisioner(t *testing.T, software *[]string, resolveErr *error) (*Provisioner, *fakeKube) {
	t.Helper()
	fk := &fakeKube{objects: map[string]map[string]any{}}
	srv := httptest.NewServer(fk)
	t.Cleanup(srv.Close)
	cfg := testConfig()
	cfg.Namespace = "kivali"
	p := &Provisioner{
		cfg:    cfg,
		kube:   &kubeClient{server: srv.URL, http: srv.Client(), namespace: "kivali"},
		logger: t.Logf,
	}
	WithSoftware(func(slug string) ([]string, error) {
		if *resolveErr != nil {
			return nil, *resolveErr
		}
		return *software, nil
	})(p)
	return p, fk
}

func devArgsOf(t *testing.T, pod map[string]any) []string {
	t.Helper()
	dev := agentShellContainer(t, pod)
	var out []string
	for _, a := range dev["args"].([]any) {
		out = append(out, a.(string))
	}
	return out
}

// Provision builds the pod with the slug's software, recreates it when
// the list changes, leaves it alone (and does not report it starting)
// when nothing changed.
func TestProvisionAppliesSoftware(t *testing.T) {
	software := []string{"ffmpeg"}
	var resolveErr error
	p, fk := testProvisioner(t, &software, &resolveErr)
	ctx := context.Background()

	starts := 0
	starting := func() { starts++ }
	if err := p.ProvisionNotify(ctx, "alice", starting); err != nil {
		t.Fatalf("first Provision: %v", err)
	}
	if starts != 1 {
		t.Errorf("creating a missing pod reported starting %d times, want 1", starts)
	}
	if args := devArgsOf(t, fk.pod(t, "alice")); args[len(args)-1] != "--install=ffmpeg" {
		t.Errorf("agent-shell args = %v", args)
	}

	// Unchanged: no delete, no create, not starting.
	fk.reset()
	if err := p.ProvisionNotify(ctx, "alice", starting); err != nil {
		t.Fatalf("no-op Provision: %v", err)
	}
	if starts != 1 || len(fk.creates) != 0 || len(fk.deletes) != 0 {
		t.Errorf("unchanged spec: starts=%d creates=%v deletes=%v", starts, fk.creates, fk.deletes)
	}

	// Changed list: recreated, starting reported.
	software = []string{"ffmpeg", "jq"}
	fk.reset()
	if err := p.ProvisionNotify(ctx, "alice", starting); err != nil {
		t.Fatalf("changed Provision: %v", err)
	}
	if starts != 2 || len(fk.deletes) != 1 || len(fk.creates) != 1 {
		t.Errorf("changed spec: starts=%d creates=%v deletes=%v", starts, fk.creates, fk.deletes)
	}
	if args := devArgsOf(t, fk.pod(t, "alice")); args[len(args)-1] != "--install=ffmpeg,jq" {
		t.Errorf("agent-shell args = %v", args)
	}

	// Emptied: recreated as the image's user.
	software = nil
	if err := p.Provision(ctx, "alice"); err != nil {
		t.Fatalf("emptied Provision: %v", err)
	}
	sc := agentShellContainer(t, fk.pod(t, "alice"))["securityContext"].(map[string]any)
	if _, root := sc["runAsUser"]; root {
		t.Errorf("securityContext = %v, want no runAsUser with no software", sc)
	}
}

// A resolver error fails Provision before anything is touched. A name
// that is not a Debian package name never reaches the pod, and the rest
// of the list still installs.
func TestProvisionSoftwareErrors(t *testing.T) {
	ctx := context.Background()

	software := []string{"ffmpeg"}
	resolveErr := errors.New("disk on fire")
	p, fk := testProvisioner(t, &software, &resolveErr)
	if err := p.Provision(ctx, "alice"); err == nil || !strings.Contains(err.Error(), "disk on fire") {
		t.Errorf("Provision = %v, want the resolver's error", err)
	}
	if len(fk.creates) != 0 {
		t.Errorf("created %v despite the error", fk.creates)
	}

	resolveErr = nil
	software = []string{"ffmpeg", "-oAPT::Get::AllowUnauthenticated=1"}
	if err := p.Provision(ctx, "alice"); err != nil {
		t.Fatalf("Provision with an invalid name on the list = %v, want the pod made without it", err)
	}
	if args := devArgsOf(t, fk.pod(t, "alice")); args[len(args)-1] != "--install=ffmpeg" {
		t.Errorf("last arg = %q, want --install=ffmpeg", args[len(args)-1])
	}
}

// Out of cluster, Provision does nothing but still reports starting,
// as it always has.
func TestProvisionNotifyOutOfCluster(t *testing.T) {
	p := &Provisioner{cfg: testConfig(), logger: func(string, ...any) {}}
	called := false
	if err := p.ProvisionNotify(context.Background(), "alice", func() { called = true }); err != nil {
		t.Fatal(err)
	}
	if !called {
		t.Error("starting not called out of cluster")
	}
}
