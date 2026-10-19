package web

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kivali-ai/kivali/internal/agentpod"
	"github.com/kivali-ai/kivali/internal/egresscred"
	"github.com/kivali-ai/kivali/internal/store"
)

func egressCredentialRequest(slug, body string) *http.Request {
	req := httptest.NewRequest(http.MethodPost, agentpod.EgressCredentialPath, strings.NewReader(body))
	if slug != "" {
		req.Header.Set(agentpod.SlugHeader, slug)
	}
	return req
}

func udsMux(srv *Server) *http.ServeMux {
	mux := http.NewServeMux()
	srv.registerUDSRoutes(mux)
	return mux
}

func TestEgressCredentialRoute(t *testing.T) {
	srv := newTestServer(t)
	if err := srv.Store.CreateAgent(store.Agent{Slug: "alice", Role: "Analyst", ReportsTo: "chief-of-staff"}, "k"); err != nil {
		t.Fatal(err)
	}
	mux := udsMux(srv)
	hash := egresscred.Hash("secret")

	for _, tc := range []struct {
		name, slug, body string
		want             int
	}{
		{"no slug header", "", `{"sha256":"` + hash + `"}`, http.StatusForbidden},
		{"not a slug", "../alice", `{"sha256":"` + hash + `"}`, http.StatusForbidden},
		{"no such agent", "bob", `{"sha256":"` + hash + `"}`, http.StatusNotFound},
		{"malformed hash", "alice", `{"sha256":"ABC"}`, http.StatusBadRequest},
		{"upper-case hash", "alice", `{"sha256":"` + strings.ToUpper(hash) + `"}`, http.StatusBadRequest},
		{"not JSON", "alice", `nope`, http.StatusBadRequest},
	} {
		rr := httptest.NewRecorder()
		mux.ServeHTTP(rr, egressCredentialRequest(tc.slug, tc.body))
		if rr.Code != tc.want {
			t.Errorf("%s: %d %s, want %d", tc.name, rr.Code, rr.Body.String(), tc.want)
		}
	}
	if got, _ := srv.Store.EgressAgentCredentials(); len(got) != 0 {
		t.Fatalf("a refused registration was recorded: %v", got)
	}

	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, egressCredentialRequest("alice", `{"sha256":"`+hash+`"}`))
	if rr.Code != http.StatusNoContent {
		t.Fatalf("good registration: %d %s", rr.Code, rr.Body.String())
	}
	if got, _ := srv.Store.EgressAgentCredentials(); got["alice"] != hash || len(got) != 1 {
		t.Fatalf("credentials = %v", got)
	}
}

// Both sides of the wire: the runtime's registration, over a real
// core.sock, lands in the proxy's agents file as the hash of the
// secret in the credentialed proxy URL the runtime hands its CLIs.
func TestEgressCredentialRoundTrip(t *testing.T) {
	srv := newTestServer(t)
	if err := srv.Store.CreateAgent(store.Agent{Slug: "alice", Role: "Analyst", ReportsTo: "chief-of-staff"}, "k"); err != nil {
		t.Fatal(err)
	}
	agentsCopy := filepath.Join(t.TempDir(), "agents.json")
	srv.Store.SetEgressAgentsSyncPath(agentsCopy)

	dir, err := os.MkdirTemp("/tmp", "eg-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	sock := filepath.Join(dir, "s")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	hs := &http.Server{Handler: udsMux(srv), ReadHeaderTimeout: 5 * time.Second}
	go func() {
		if err := hs.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			t.Logf("serve: %v", err)
		}
	}()
	t.Cleanup(func() { _ = hs.Close() })

	cred, err := agentpod.SetUpEgressCredential(context.Background(), agentpod.NewClient(sock, "alice"), "http://10.43.0.12:3128")
	if err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(agentsCopy)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), `"alice": "`+cred.Hash+`"`) {
		t.Fatalf("the proxy's agents file = %s, want alice's hash %s", b, cred.Hash)
	}
	secret := strings.TrimPrefix(strings.SplitN(cred.URL, "@", 2)[0], "http://alice:")
	if egresscred.Hash(secret) != cred.Hash || strings.Contains(string(b), secret) {
		t.Fatalf("the file holds the secret or not its hash: %s", b)
	}
}
