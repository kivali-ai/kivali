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

// A connection's Secret is written with PUT and, when the API has no
// Secret of that name, created with POST; read back with its data
// decoded; and deleted, with a missing one no error. A PutSecret that
// only PUT would fail every first connection with a 404; one that
// sent the data unencoded, or as stringData, would be refused or read
// back wrong.
func TestSecretsPutGetDelete(t *testing.T) {
	stored := map[string]map[string]any{}
	var verbs []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		verbs = append(verbs, r.Method+" "+r.URL.Path)
		const base = "/api/v1/namespaces/ns/secrets"
		name := strings.TrimPrefix(strings.TrimPrefix(r.URL.Path, base), "/")
		notFound := func() {
			w.WriteHeader(http.StatusNotFound)
			_, _ = io.WriteString(w, `{"kind":"Status","reason":"NotFound","code":404}`)
		}
		switch {
		case r.Method == http.MethodPost && name == "":
			var obj map[string]any
			_ = json.NewDecoder(r.Body).Decode(&obj)
			n := obj["metadata"].(map[string]any)["name"].(string)
			stored[n] = obj
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(obj)
		case r.Method == http.MethodPut:
			if _, ok := stored[name]; !ok {
				notFound()
				return
			}
			var obj map[string]any
			_ = json.NewDecoder(r.Body).Decode(&obj)
			stored[name] = obj
			_ = json.NewEncoder(w).Encode(obj)
		case r.Method == http.MethodGet:
			obj, ok := stored[name]
			if !ok {
				notFound()
				return
			}
			_ = json.NewEncoder(w).Encode(obj)
		case r.Method == http.MethodDelete:
			if _, ok := stored[name]; !ok {
				notFound()
				return
			}
			delete(stored, name)
			_, _ = io.WriteString(w, `{"kind":"Status","status":"Success"}`)
		default:
			w.WriteHeader(http.StatusMethodNotAllowed)
		}
	}))
	t.Cleanup(srv.Close)
	k := NewForServer(srv.URL, srv.Client(), "ns")
	ctx := context.Background()
	labels := map[string]string{"app": "kivali", "kivali.ai/integration": "crm"}

	if _, err := k.GetSecret(ctx, "s1"); !errors.Is(err, ErrNotFound) {
		t.Errorf("GetSecret(missing) = %v, want ErrNotFound", err)
	}
	if err := k.PutSecret(ctx, "s1", labels, map[string][]byte{"token": []byte("first")}); err != nil {
		t.Fatal(err)
	}
	if err := k.PutSecret(ctx, "s1", labels, map[string][]byte{"token": []byte("second")}); err != nil {
		t.Fatal(err)
	}
	want := []string{"GET /api/v1/namespaces/ns/secrets/s1", "PUT /api/v1/namespaces/ns/secrets/s1", "POST /api/v1/namespaces/ns/secrets", "PUT /api/v1/namespaces/ns/secrets/s1"}
	if strings.Join(verbs, ",") != strings.Join(want, ",") {
		t.Errorf("verbs = %v, want %v", verbs, want)
	}
	obj := stored["s1"]
	if obj["type"] != "Opaque" || obj["data"].(map[string]any)["token"] != "c2Vjb25k" {
		t.Errorf("stored = %v", obj)
	}
	if got := obj["metadata"].(map[string]any)["labels"].(map[string]any); got["kivali.ai/integration"] != "crm" || got["app"] != "kivali" {
		t.Errorf("labels = %v", got)
	}
	data, err := k.GetSecret(ctx, "s1")
	if err != nil || string(data["token"]) != "second" {
		t.Errorf("GetSecret = %q, %v", data, err)
	}
	if err := k.DeleteSecret(ctx, "s1"); err != nil {
		t.Fatal(err)
	}
	if err := k.DeleteSecret(ctx, "s1"); err != nil {
		t.Errorf("deleting a missing Secret: %v", err)
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
