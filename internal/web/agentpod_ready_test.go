package web

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/kivali-ai/kivali/internal/store"
)

// getReadyzAgents answers GET /readyz/agents through the server's
// handler: the status code and the body.
func getReadyzAgents(t *testing.T, srv *Server) (int, string) {
	t.Helper()
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/readyz/agents", nil))
	return rec.Code, strings.TrimSpace(rec.Body.String())
}

// The supervisor's team update commits on GET /readyz/agents answering
// 200, so it must answer 200 only once every agent pod's runtime is
// connected on the spec this server provisioned: a runtime that dialed
// in before its pod was recreated is the old pod's, and counting it
// would commit an update whose new pods never came up. A pod this
// server found already matching keeps its runtime, which counts. The
// body is the wire shape internal/supervisor's waitAgents reads.
func TestReadyzAgentsCountsOnlyRuntimesOnTheProvisionedPod(t *testing.T) {
	srv := newTestServer(t)
	for _, slug := range []string{"alice", "bob"} {
		if err := srv.Store.CreateAgent(store.Agent{Slug: slug, Role: slug, ReportsTo: "ceo"}, "role"); err != nil {
			t.Fatal(err)
		}
	}
	srv.AgentpodHub = newAgentpodHub()
	srv.AgentPod = &notifyingAgentPod{recreate: true}

	// Both pods still run from before this server started, and dial in.
	oldAlice := srv.AgentpodHub.SubscribeRuntime("alice", "old-alice")
	srv.AgentpodHub.SubscribeRuntime("bob", "bob")
	if code, body := getReadyzAgents(t, srv); code != http.StatusServiceUnavailable || body != `{"agent_pods":2,"connected":0}` {
		t.Fatalf("before any provision: %d %s", code, body)
	}

	// alice's pod drifted and is recreated; bob's matches and stays.
	if err := srv.ProvisionAgentPod(context.Background(), "alice"); err != nil {
		t.Fatal(err)
	}
	srv.AgentPod = &notifyingAgentPod{recreate: false}
	if err := srv.ProvisionAgentPod(context.Background(), "bob"); err != nil {
		t.Fatal(err)
	}
	if code, body := getReadyzAgents(t, srv); code != http.StatusServiceUnavailable || body != `{"agent_pods":2,"connected":1}` {
		t.Fatalf("alice's old runtime still subscribed: %d %s", code, body)
	}

	srv.AgentpodHub.Unsubscribe(oldAlice)
	srv.AgentpodHub.SubscribeRuntime("alice", "new-alice")
	if code, body := getReadyzAgents(t, srv); code != http.StatusOK || body != `{"agent_pods":2,"connected":2}` {
		t.Fatalf("both connected: %d %s", code, body)
	}
}

// A team whose only agent is the CEO, which runs in the server and has
// no pod, has no agent pods to wait for: the update must be able to
// commit at once rather than wait out its timeout and roll back.
func TestReadyzAgentsWithNoAgentPods(t *testing.T) {
	srv := newTestServer(t)
	srv.AgentpodHub = newAgentpodHub()
	srv.AgentPod = &notifyingAgentPod{}
	if err := srv.Store.CreateAgent(store.Agent{Slug: "ceo", Role: "ceo"}, "role"); err != nil {
		t.Fatal(err)
	}
	if code, body := getReadyzAgents(t, srv); code != http.StatusOK || body != `{"agent_pods":0,"connected":0}` {
		t.Fatalf("%d %s", code, body)
	}
}
