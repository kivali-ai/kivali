package kube

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestSiblingImage(t *testing.T) {
	cases := map[string]string{
		"kivali:v0.8.3":                   "kivali-browser:v0.8.3",
		"ghcr.io/kivali-ai/kivali:v0.8.3": "ghcr.io/kivali-ai/kivali-browser:v0.8.3",
		"registry:5000/kivali":            "registry:5000/kivali-browser:latest",
		"registry:5000/team/kivali:dev":   "registry:5000/team/kivali-browser:dev",
		"kivali":                          "kivali-browser:latest",
		"":                                "",
	}
	for in, want := range cases {
		if got := SiblingImage(in, "kivali-browser"); got != want {
			t.Errorf("SiblingImage(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestSpecHashStableAndSensitive(t *testing.T) {
	a := map[string]any{"spec": map[string]any{"image": "x:1", "n": 1}}
	b := map[string]any{"spec": map[string]any{"n": 1, "image": "x:1"}}
	c := map[string]any{"spec": map[string]any{"image": "x:2", "n": 1}}
	if SpecHash(a) != SpecHash(b) {
		t.Error("same content hashed differently")
	}
	if SpecHash(a) == SpecHash(c) {
		t.Error("different content hashed the same")
	}
	if len(SpecHash(a)) != 12 {
		t.Errorf("hash %q is not 12 hex chars", SpecHash(a))
	}
}

// The client reads pods by name and by label selector, and maps a 404
// to ErrNotFound.
func TestGetAndListPods(t *testing.T) {
	var gotSelector string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/namespaces/ns/pods":
			gotSelector = r.URL.Query().Get("labelSelector")
			_ = json.NewEncoder(w).Encode(map[string]any{"items": []any{
				map[string]any{
					"metadata": map[string]any{"name": "a", "labels": map[string]string{SpecHashLabel: "h1"}},
					"status":   map[string]any{"phase": "Running"},
				},
				map[string]any{
					"metadata": map[string]any{"name": "b"},
					"status":   map[string]any{"phase": "Failed"},
				},
			}})
		case "/api/v1/namespaces/ns/pods/a":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"metadata": map[string]any{"name": "a", "labels": map[string]string{SpecHashLabel: "h1"}},
				"status":   map[string]any{"phase": "Succeeded"},
			})
		default:
			w.WriteHeader(http.StatusNotFound)
			_, _ = io.WriteString(w, `{"kind":"Status","reason":"NotFound","code":404}`)
		}
	}))
	t.Cleanup(srv.Close)
	k := NewForServer(srv.URL, srv.Client(), "ns")
	ctx := context.Background()

	pods, err := k.ListPods(ctx, "app=kivali-browserpod")
	if err != nil {
		t.Fatal(err)
	}
	if gotSelector != "app=kivali-browserpod" {
		t.Errorf("selector = %q", gotSelector)
	}
	if len(pods) != 2 || pods[0].Name != "a" || pods[0].SpecHash() != "h1" || pods[1].SpecHash() != "" || !pods[1].Finished() || pods[0].Finished() {
		t.Errorf("pods = %+v", pods)
	}

	pod, err := k.GetPod(ctx, "a")
	if err != nil || pod.Phase != "Succeeded" || !pod.Finished() {
		t.Errorf("GetPod(a) = %+v, %v", pod, err)
	}
	if _, err := k.GetPod(ctx, "missing"); !errors.Is(err, ErrNotFound) {
		t.Errorf("GetPod(missing) err = %v, want ErrNotFound", err)
	}
	if _, err := k.GetPodSpecHash(ctx, "missing"); !errors.Is(err, ErrNotFound) {
		t.Errorf("GetPodSpecHash(missing) err = %v, want ErrNotFound", err)
	}
}

// TestPodReadsTheSchedulersRefusal guards how a browser pod that cannot
// be placed is told apart: without the PodScheduled condition's message
// a start short of memory waits out its whole timeout and ends with no
// reason. A condition that is True, or False for another reason, is no
// refusal.
func TestPodReadsTheSchedulersRefusal(t *testing.T) {
	const msg = "0/1 nodes are available: 1 Insufficient memory. preemption: 0/1 nodes are available: 1 No preemption victims found for incoming pod."
	conditions := map[string][]any{
		"refused": {map[string]any{"type": "PodScheduled", "status": "False", "reason": "Unschedulable", "message": msg}},
		"placed":  {map[string]any{"type": "PodScheduled", "status": "True"}},
		"gated":   {map[string]any{"type": "PodScheduled", "status": "False", "reason": "SchedulingGated", "message": "gated"}},
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		name := strings.TrimPrefix(r.URL.Path, "/api/v1/namespaces/ns/pods/")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"metadata": map[string]any{"name": name},
			"status":   map[string]any{"phase": "Pending", "conditions": conditions[name]},
		})
	}))
	t.Cleanup(srv.Close)
	k := NewForServer(srv.URL, srv.Client(), "ns")
	for name, want := range map[string]string{"refused": msg, "placed": "", "gated": ""} {
		pod, err := k.GetPod(context.Background(), name)
		if err != nil || pod.Unschedulable != want {
			t.Errorf("GetPod(%s).Unschedulable = %q, %v; want %q", name, pod.Unschedulable, err, want)
		}
	}
}
