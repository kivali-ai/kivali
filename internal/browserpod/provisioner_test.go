package browserpod

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sort"
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
	creates []string
	deletes []string
}

func (f *fakeKube) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	switch {
	case r.Method == http.MethodGet && r.URL.Path == podsPath:
		sel := r.URL.Query().Get("labelSelector")
		k, v, _ := strings.Cut(sel, "=")
		items := []any{}
		for _, obj := range f.objects {
			labels, _ := obj["metadata"].(map[string]any)["labels"].(map[string]any)
			if sel == "" || labels[k] == v {
				items = append(items, obj)
			}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"items": items})
	case r.Method == http.MethodGet:
		obj, ok := f.objects[r.URL.Path]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			_, _ = io.WriteString(w, `{"kind":"Status","reason":"NotFound","code":404}`)
			return
		}
		_ = json.NewEncoder(w).Encode(obj)
	case r.Method == http.MethodPost:
		var obj map[string]any
		_ = json.NewDecoder(r.Body).Decode(&obj)
		path := r.URL.Path + "/" + obj["metadata"].(map[string]any)["name"].(string)
		obj["status"] = map[string]any{"phase": "Pending"}
		f.objects[path] = obj
		f.creates = append(f.creates, path)
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(obj)
	case r.Method == http.MethodDelete:
		if _, ok := f.objects[r.URL.Path]; !ok {
			w.WriteHeader(http.StatusNotFound)
			_, _ = io.WriteString(w, `{"kind":"Status","reason":"NotFound","code":404}`)
			return
		}
		delete(f.objects, r.URL.Path)
		f.deletes = append(f.deletes, r.URL.Path)
		_, _ = io.WriteString(w, `{}`)
	}
}

func (f *fakeKube) pod(name string) map[string]any {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.objects[podsPath+"/"+name]
}

func (f *fakeKube) put(name string, obj map[string]any) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.objects[podsPath+"/"+name] = obj
}

func (f *fakeKube) setPhase(name, phase string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.objects[podsPath+"/"+name]["status"] = map[string]any{"phase": phase}
}

func (f *fakeKube) reset() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.creates, f.deletes = nil, nil
}

func (f *fakeKube) counts() (int, int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.creates), len(f.deletes)
}

func testProvisioner(t *testing.T) (*Provisioner, *fakeKube) {
	t.Helper()
	fk := &fakeKube{objects: map[string]map[string]any{}}
	srv := httptest.NewServer(fk)
	t.Cleanup(srv.Close)
	cfg := testConfig()
	cfg.Namespace = "kivali"
	return &Provisioner{
		cfg:    cfg,
		kube:   kube.NewForServer(srv.URL, srv.Client(), "kivali"),
		logger: t.Logf,
	}, fk
}

func tokenOf(pod map[string]any) string {
	ctr := pod["spec"].(map[string]any)["containers"].([]any)[0].(map[string]any)
	for _, e := range ctr["env"].([]any) {
		if m := e.(map[string]any); m["name"] == EnvToken {
			return m["value"].(string)
		}
	}
	return ""
}

// Start creates a missing pod, leaves a matching live one alone, and
// replaces one built from another manifest or one that has finished.
func TestStart(t *testing.T) {
	p, fk := testProvisioner(t)
	ctx := context.Background()
	name := PodName("alice", "research")

	if err := p.Start(ctx, "alice", "research", "tok-1"); err != nil {
		t.Fatalf("first Start: %v", err)
	}
	if c, d := fk.counts(); c != 1 || d != 0 {
		t.Fatalf("first Start: creates=%d deletes=%d", c, d)
	}
	if got := tokenOf(fk.pod(name)); got != "tok-1" {
		t.Errorf("token = %q", got)
	}

	// Same manifest, running: left alone.
	fk.setPhase(name, "Running")
	fk.reset()
	if err := p.Start(ctx, "alice", "research", "tok-1"); err != nil {
		t.Fatalf("matching Start: %v", err)
	}
	if c, d := fk.counts(); c != 0 || d != 0 {
		t.Errorf("matching Start: creates=%d deletes=%d, want none", c, d)
	}

	// New token: drift, recreated.
	fk.reset()
	if err := p.Start(ctx, "alice", "research", "tok-2"); err != nil {
		t.Fatalf("drift Start: %v", err)
	}
	if c, d := fk.counts(); c != 1 || d != 1 {
		t.Errorf("drift Start: creates=%d deletes=%d, want 1 and 1", c, d)
	}
	if got := tokenOf(fk.pod(name)); got != "tok-2" {
		t.Errorf("token after drift = %q", got)
	}

	// Same manifest but finished: recreated.
	for _, phase := range []string{"Succeeded", "Failed"} {
		fk.setPhase(name, phase)
		fk.reset()
		if err := p.Start(ctx, "alice", "research", "tok-2"); err != nil {
			t.Fatalf("Start over %s pod: %v", phase, err)
		}
		if c, d := fk.counts(); c != 1 || d != 1 {
			t.Errorf("Start over %s pod: creates=%d deletes=%d, want 1 and 1", phase, c, d)
		}
	}
}

func TestStop(t *testing.T) {
	p, fk := testProvisioner(t)
	ctx := context.Background()
	if err := p.Start(ctx, "alice", "research", "tok"); err != nil {
		t.Fatal(err)
	}
	if err := p.Stop(ctx, "alice", "research"); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if fk.pod(PodName("alice", "research")) != nil {
		t.Error("pod still present after Stop")
	}
	// Stopping a stopped browser is fine.
	if err := p.Stop(ctx, "alice", "research"); err != nil {
		t.Errorf("second Stop: %v", err)
	}
}

// StopAll deletes browser pods and nothing else.
func TestStopAll(t *testing.T) {
	p, fk := testProvisioner(t)
	ctx := context.Background()
	for _, s := range []string{"a", "b"} {
		if err := p.Start(ctx, "alice", s, "tok"); err != nil {
			t.Fatal(err)
		}
	}
	fk.put("agentpod-alice", map[string]any{
		"metadata": map[string]any{"name": "agentpod-alice", "labels": map[string]any{"app": "kivali-agentpod"}},
	})
	n, err := p.StopAll(ctx)
	if err != nil {
		t.Fatalf("StopAll: %v", err)
	}
	if n != 2 {
		t.Errorf("StopAll deleted %d, want 2", n)
	}
	fk.mu.Lock()
	var left []string
	for path := range fk.objects {
		left = append(left, path)
	}
	fk.mu.Unlock()
	sort.Strings(left)
	if len(left) != 1 || left[0] != podsPath+"/agentpod-alice" {
		t.Errorf("left = %v, want only the agent pod", left)
	}
}

// Out-of-cluster the provisioner does nothing and says so.
func TestDisabledProvisioner(t *testing.T) {
	p, err := NewProvisioner(testConfig())
	if err != nil {
		t.Fatal(err)
	}
	if p.Enabled() {
		t.Skip("running in-cluster")
	}
	ctx := context.Background()
	if err := p.Start(ctx, "a", "s", "t"); err != nil {
		t.Error(err)
	}
	if err := p.Stop(ctx, "a", "s"); err != nil {
		t.Error(err)
	}
	if n, err := p.StopAll(ctx); n != 0 || err != nil {
		t.Errorf("StopAll = %d, %v", n, err)
	}
}
