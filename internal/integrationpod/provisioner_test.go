package integrationpod

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/kivali-ai/kivali/internal/kube"
)

const podsPath = "/api/v1/namespaces/kivali/pods"

// fakeKube is the part of the Kubernetes API the provisioner uses:
// get, list by label selector, create and delete of Pods. A deleted
// pod is gone at once.
type fakeKube struct {
	mu      sync.Mutex
	objects map[string]map[string]any
	creates int
	deletes int
}

func (f *fakeKube) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	notFound := func() {
		w.WriteHeader(http.StatusNotFound)
		_, _ = io.WriteString(w, `{"kind":"Status","reason":"NotFound","code":404}`)
	}
	switch {
	case r.Method == http.MethodGet && r.URL.Path == podsPath:
		k, v, _ := strings.Cut(r.URL.Query().Get("labelSelector"), "=")
		items := []any{}
		for _, obj := range f.objects {
			labels, _ := obj["metadata"].(map[string]any)["labels"].(map[string]any)
			if labels[k] == v {
				items = append(items, obj)
			}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"items": items})
	case r.Method == http.MethodGet:
		obj, ok := f.objects[r.URL.Path]
		if !ok {
			notFound()
			return
		}
		_ = json.NewEncoder(w).Encode(obj)
	case r.Method == http.MethodPost:
		var obj map[string]any
		_ = json.NewDecoder(r.Body).Decode(&obj)
		obj["status"] = map[string]any{"phase": "Pending"}
		f.objects[r.URL.Path+"/"+obj["metadata"].(map[string]any)["name"].(string)] = obj
		f.creates++
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(obj)
	case r.Method == http.MethodDelete:
		if _, ok := f.objects[r.URL.Path]; !ok {
			notFound()
			return
		}
		delete(f.objects, r.URL.Path)
		f.deletes++
		_, _ = io.WriteString(w, `{}`)
	}
}

func (f *fakeKube) counts() (int, int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	c, d := f.creates, f.deletes
	f.creates, f.deletes = 0, 0
	return c, d
}

func (f *fakeKube) setStatus(name string, status map[string]any) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.objects[podsPath+"/"+name]["status"] = status
}

func testProvisioner(t *testing.T) (*Provisioner, *fakeKube) {
	t.Helper()
	fk := &fakeKube{objects: map[string]map[string]any{}}
	srv := httptest.NewServer(fk)
	t.Cleanup(srv.Close)
	return &Provisioner{
		cfg:    testConfig(),
		kube:   kube.NewForServer(srv.URL, srv.Client(), "kivali"),
		logger: t.Logf,
	}, fk
}

// Start creates a missing pod, leaves a matching live one alone, and
// replaces one built for another version or token, or one that has
// finished. Leaving a matching pod alone is what keeps an integration
// up across a reconcile; replacing a drifted one is how a new version
// reaches its pod.
func TestProvisionerStart(t *testing.T) {
	p, fk := testProvisioner(t)
	ctx := context.Background()
	if err := p.Start(ctx, "crm", "1.0.0", "d1", "tok-1", false); err != nil {
		t.Fatal(err)
	}
	if c, d := fk.counts(); c != 1 || d != 0 {
		t.Fatalf("first Start: creates=%d deletes=%d", c, d)
	}
	fk.setStatus("integrationpod-crm", map[string]any{"phase": "Running"})
	if err := p.Start(ctx, "crm", "1.0.0", "d1", "tok-1", false); err != nil {
		t.Fatal(err)
	}
	if c, d := fk.counts(); c != 0 || d != 0 {
		t.Errorf("matching Start: creates=%d deletes=%d", c, d)
	}
	for _, step := range []struct{ version, token string }{{"1.1.0", "tok-1"}, {"1.1.0", "tok-2"}} {
		if err := p.Start(ctx, "crm", step.version, "d1", step.token, false); err != nil {
			t.Fatal(err)
		}
		if c, d := fk.counts(); c != 1 || d != 1 {
			t.Errorf("Start for %+v: creates=%d deletes=%d, want 1 and 1", step, c, d)
		}
	}
	fk.setStatus("integrationpod-crm", map[string]any{"phase": "Failed"})
	if err := p.Start(ctx, "crm", "1.1.0", "d1", "tok-2", false); err != nil {
		t.Fatal(err)
	}
	if c, d := fk.counts(); c != 1 || d != 1 {
		t.Errorf("Start over a finished pod: creates=%d deletes=%d", c, d)
	}
}

// Token gives core, after a restart, the token of a pod built exactly
// as it would build it, and nothing for a pod built for another
// version, from other files or by another configuration. Without it every restart of the
// server would replace every integration pod; with a looser check core
// would take on a pod that runs a version no longer approved.
func TestProvisionerToken(t *testing.T) {
	p, _ := testProvisioner(t)
	ctx := context.Background()
	if tok, err := p.Token(ctx, "crm", "1.0.0", "d1", false); tok != "" || err != nil {
		t.Fatalf("no pod: %q, %v", tok, err)
	}
	if err := p.Start(ctx, "crm", "1.0.0", "d1", "tok-1", false); err != nil {
		t.Fatal(err)
	}
	if tok, err := p.Token(ctx, "crm", "1.0.0", "d1", false); tok != "tok-1" || err != nil {
		t.Errorf("matching pod: %q, %v", tok, err)
	}
	if tok, _ := p.Token(ctx, "crm", "1.1.0", "d1", false); tok != "" {
		t.Errorf("another version: %q", tok)
	}
	// Other files under the same version: a pod still running the
	// files from before is not taken on.
	if tok, _ := p.Token(ctx, "crm", "1.0.0", "d2", false); tok != "" {
		t.Errorf("other files: %q", tok)
	}
	// A pod built without software is not the pod for a version that
	// installs some: taken on, it would run without root and never
	// install it.
	if tok, _ := p.Token(ctx, "crm", "1.0.0", "d1", true); tok != "" {
		t.Errorf("software: %q", tok)
	}
	other := *p
	other.cfg.Image = "ghcr.io/kivali-ai/kivali:v9.9.9"
	if tok, _ := other.Token(ctx, "crm", "1.0.0", "d1", false); tok != "" {
		t.Errorf("another image: %q", tok)
	}
}

// Stop, List and Unschedulable: Stop deletes the pod and is fine with
// none; List names only integration pods, by integration; Unschedulable
// reads the scheduler's message. List is how core finds, at boot, the
// pods of integrations that were removed while it was down.
func TestProvisionerStopListUnschedulable(t *testing.T) {
	p, fk := testProvisioner(t)
	ctx := context.Background()
	for _, n := range []string{"crm", "blog"} {
		if err := p.Start(ctx, n, "1.0.0", "d1", "tok", false); err != nil {
			t.Fatal(err)
		}
	}
	fk.mu.Lock()
	fk.objects[podsPath+"/agentpod-alice"] = map[string]any{"metadata": map[string]any{"name": "agentpod-alice", "labels": map[string]any{"app": "kivali-agentpod"}}}
	fk.mu.Unlock()
	names, err := p.List(ctx)
	if err != nil || strings.Join(names, ",") != "blog,crm" {
		t.Fatalf("List = %v, %v", names, err)
	}
	const refusal = "0/1 nodes are available: 1 Insufficient memory."
	fk.setStatus("integrationpod-crm", map[string]any{"phase": "Pending", "conditions": []any{
		map[string]any{"type": "PodScheduled", "status": "False", "reason": "Unschedulable", "message": refusal},
	}})
	if msg, err := p.Unschedulable(ctx, "crm"); msg != refusal || err != nil {
		t.Errorf("Unschedulable = %q, %v", msg, err)
	}
	if err := p.Stop(ctx, "crm"); err != nil {
		t.Fatal(err)
	}
	if err := p.Stop(ctx, "crm"); err != nil {
		t.Errorf("second Stop: %v", err)
	}
	if names, _ := p.List(ctx); strings.Join(names, ",") != "blog" {
		t.Errorf("List after Stop = %v", names)
	}
}

// Out of a cluster the provisioner does nothing, and says so by
// Enabled. A missing piece of config is refused at construction, so a
// pod can never be built without its bundle or affinity.
func TestProvisionerConfig(t *testing.T) {
	p, err := NewProvisioner(testConfig())
	if err != nil {
		t.Fatal(err)
	}
	if p.Enabled() {
		t.Skip("running in a cluster")
	}
	ctx := context.Background()
	if err := p.Start(ctx, "crm", "1.0.0", "d1", "t", false); err != nil {
		t.Error(err)
	}
	if names, err := p.List(ctx); names != nil || err != nil {
		t.Errorf("List = %v, %v", names, err)
	}
	for name, mut := range map[string]func(*ProvisionerConfig){
		"image":  func(c *ProvisionerConfig) { c.Image = "" },
		"claim":  func(c *ProvisionerConfig) { c.DataClaimName = "" },
		"uds":    func(c *ProvisionerConfig) { c.UDSDir = "" },
		"select": func(c *ProvisionerConfig) { c.WebSelector = nil },
	} {
		cfg := testConfig()
		mut(&cfg)
		if _, err := NewProvisioner(cfg); err == nil {
			t.Errorf("no %s: accepted", name)
		}
	}
	bad := testConfig()
	bad.ProxyURL = "http://user:pw@proxy:3128"
	fp, _ := testProvisioner(t)
	fp.cfg = bad
	if err := fp.Start(ctx, "crm", "1.0.0", "d1", "t", false); err == nil || errors.Is(err, kube.ErrNotFound) {
		t.Errorf("a proxy URL with credentials: %v", err)
	}
}
