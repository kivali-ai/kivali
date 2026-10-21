package secrets

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/kivali-ai/kivali/internal/kube"
)

// Memory is what make run and the tests hold connections in. A Get that
// handed out its own map would let a caller change a stored secret by
// editing what it read; a missing name must be ErrNotFound, which the
// connection reads as "reconnect needed".
func TestMemory(t *testing.T) {
	ctx := context.Background()
	m := NewMemory()
	if _, err := m.Get(ctx, "a"); !errors.Is(err, ErrNotFound) {
		t.Errorf("Get(missing) = %v", err)
	}
	in := map[string]string{"token": "t1"}
	if err := m.Put(ctx, "a", in); err != nil {
		t.Fatal(err)
	}
	in["token"] = "changed"
	got, _ := m.Get(ctx, "a")
	got["token"] = "changed again"
	if again, _ := m.Get(ctx, "a"); again["token"] != "t1" {
		t.Errorf("stored value = %q", again["token"])
	}
	if err := m.Delete(ctx, "a"); err != nil {
		t.Fatal(err)
	}
	if err := m.Delete(ctx, "a"); err != nil {
		t.Errorf("Delete(missing) = %v", err)
	}
	if len(m.Names()) != 0 {
		t.Errorf("names = %v", m.Names())
	}
}

// The Secret's name is what the connection record keeps, and the
// label is how an operator finds an integration's Secrets with
// kubectl; a name that parsed back to another integration would label
// it wrong.
func TestConnectionSecretName(t *testing.T) {
	n := ConnectionSecretName("my-crm", "c7f3a1b2")
	if n != "kivali-connection-my-crm-c7f3a1b2" {
		t.Errorf("name = %s", n)
	}
	for name, want := range map[string]string{n: "my-crm", "kivali-connection-a-1": "a", "kivali-secrets": "", "kivali-connection-x": ""} {
		if got := connectionIntegration(name); got != want {
			t.Errorf("connectionIntegration(%s) = %q, want %q", name, got, want)
		}
	}
}

// Kube writes Opaque Secrets labelled with the integration, reads them
// back as text and maps the API's 404 to ErrNotFound, which is what
// tells the page a connection must be entered again.
func TestKube(t *testing.T) {
	stored := map[string]map[string]any{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		name := strings.TrimPrefix(r.URL.Path, "/api/v1/namespaces/ns/secrets")
		name = strings.TrimPrefix(name, "/")
		switch r.Method {
		case http.MethodPut:
			if _, ok := stored[name]; !ok {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			fallthrough
		case http.MethodPost:
			var obj map[string]any
			_ = json.NewDecoder(r.Body).Decode(&obj)
			stored[obj["metadata"].(map[string]any)["name"].(string)] = obj
			_ = json.NewEncoder(w).Encode(obj)
		case http.MethodGet:
			obj, ok := stored[name]
			if !ok {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			_ = json.NewEncoder(w).Encode(obj)
		case http.MethodDelete:
			delete(stored, name)
		}
	}))
	t.Cleanup(srv.Close)
	k := NewKube(kube.NewForServer(srv.URL, srv.Client(), "ns"))
	ctx := context.Background()
	name := ConnectionSecretName("crm", "c1")
	if _, err := k.Get(ctx, name); !errors.Is(err, ErrNotFound) {
		t.Errorf("Get(missing) = %v", err)
	}
	if err := k.Put(ctx, name, map[string]string{"token": "s3cret"}); err != nil {
		t.Fatal(err)
	}
	labels := stored[name]["metadata"].(map[string]any)["labels"].(map[string]any)
	if labels["app"] != "kivali" || labels[IntegrationLabel] != "crm" || stored[name]["type"] != "Opaque" {
		t.Errorf("stored = %v", stored[name])
	}
	got, err := k.Get(ctx, name)
	if err != nil || got["token"] != "s3cret" {
		t.Errorf("Get = %v, %v", got, err)
	}
	if err := k.Delete(ctx, name); err != nil {
		t.Fatal(err)
	}
	if _, ok := stored[name]; ok {
		t.Error("not deleted")
	}
}
