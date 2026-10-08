package web

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kivali-ai/kivali/internal/agentpod"
	"github.com/kivali-ai/kivali/internal/mcp"
	"github.com/kivali-ai/kivali/internal/store"
)

// startStateDispatchServer wires the unified state-tool dispatch
// endpoint on a Unix socket.
func startStateDispatchServer(t *testing.T) (sockPath string, srv *Server, cleanup func()) {
	t.Helper()
	srv = newTestServer(t)
	if err := srv.Store.CreateAgent(store.Agent{Slug: "chief-of-staff", Role: "CoS", ReportsTo: "ceo"}, "# CoS\n"); err != nil {
		t.Fatalf("seed CoS: %v", err)
	}
	if err := srv.Store.CreateAgent(store.Agent{Slug: "alice", Role: "Analyst", ReportsTo: "chief-of-staff"}, "# Analyst\n\nPolish numbers.\n"); err != nil {
		t.Fatalf("seed alice: %v", err)
	}
	if err := srv.Store.CreateAgent(store.Agent{Slug: "bob", Role: "Researcher", ReportsTo: "chief-of-staff"}, "# Bob\n"); err != nil {
		t.Fatalf("seed bob: %v", err)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/agent/{slug}/state/dispatch", srv.handleAgentpodStateDispatch)
	dir, err := os.MkdirTemp("/tmp", "wos-")
	if err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	sockPath = filepath.Join(dir, "s")
	ln, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	httpSrv := &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	go func() {
		if err := httpSrv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			t.Logf("serve: %v", err)
		}
	}()
	cleanup = func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = httpSrv.Shutdown(ctx)
	}
	return sockPath, srv, cleanup
}

// TestAgentpodStateDispatchOrgChart: get_org_chart through the unified
// dispatch endpoint returns the same timestamped snapshot the in-process
// handler would.
func TestAgentpodStateDispatchOrgChart(t *testing.T) {
	sockPath, _, cleanup := startStateDispatchServer(t)
	defer cleanup()
	c := agentpod.NewClient(sockPath, "alice")
	ctx := context.Background()

	resp, err := c.StateDispatch(ctx, mcp.GetOrgChartToolName, json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("StateDispatch: %v", err)
	}
	if resp.IsError {
		t.Fatalf("dispatch IsError=true: %s", resp.Body)
	}
	if !strings.Contains(resp.Body, "as of ") {
		t.Errorf("missing snapshot timestamp: %q", resp.Body)
	}
	if !strings.Contains(resp.Body, "chief-of-staff") {
		t.Errorf("missing CoS in chart: %q", resp.Body)
	}
}

// TestAgentpodStateDispatchAssignmentToolsWithoutAMessenger: the assignment
// tools ride the same endpoint; on a server with no Messenger to
// route wakes through they answer "not configured" rather than
// failing the request.
func TestAgentpodStateDispatchAssignmentToolsWithoutAMessenger(t *testing.T) {
	sockPath, _, cleanup := startStateDispatchServer(t)
	defer cleanup()
	c := agentpod.NewClient(sockPath, "alice")
	resp, err := c.StateDispatch(context.Background(), mcp.AssignmentListToolName, json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("StateDispatch: %v", err)
	}
	if !resp.IsError || !strings.Contains(resp.Body, "not configured") {
		t.Fatalf("assignment_list without a tracker: IsError=%v body=%q", resp.IsError, resp.Body)
	}
}

// TestAgentpodStateDispatchReadAgentRoleCoSGate: a non-CoS slug is
// refused server-side even if it asks for the tool.
func TestAgentpodStateDispatchReadAgentRoleCoSGate(t *testing.T) {
	sockPath, _, cleanup := startStateDispatchServer(t)
	defer cleanup()
	c := agentpod.NewClient(sockPath, "alice")
	ctx := context.Background()

	resp, err := c.StateDispatch(ctx, mcp.ReadAgentRoleToolName,
		json.RawMessage(`{"slug":"bob"}`))
	if err != nil {
		t.Fatalf("StateDispatch: %v", err)
	}
	if !resp.IsError {
		t.Fatalf("expected CoS-only refusal, got success: %q", resp.Body)
	}
	if !strings.Contains(resp.Body, "Chief of Staff") {
		t.Errorf("error should explain the CoS gate: %q", resp.Body)
	}
}

// TestAgentpodStateDispatchReadAgentRoleAsCoS: as chief-of-staff,
// reading another agent's role returns the body.
func TestAgentpodStateDispatchReadAgentRoleAsCoS(t *testing.T) {
	sockPath, _, cleanup := startStateDispatchServer(t)
	defer cleanup()
	c := agentpod.NewClient(sockPath, "chief-of-staff")
	ctx := context.Background()

	resp, err := c.StateDispatch(ctx, mcp.ReadAgentRoleToolName,
		json.RawMessage(`{"slug":"alice"}`))
	if err != nil {
		t.Fatalf("StateDispatch: %v", err)
	}
	if resp.IsError {
		t.Fatalf("expected role to load: %s", resp.Body)
	}
	if !strings.Contains(resp.Body, "Polish numbers.") {
		t.Errorf("role body not surfaced: %q", resp.Body)
	}
}

// TestAgentpodStateDispatchUnknownTool: unknown tool surfaces as
// IsError, not a transport error.
func TestAgentpodStateDispatchUnknownTool(t *testing.T) {
	sockPath, _, cleanup := startStateDispatchServer(t)
	defer cleanup()
	c := agentpod.NewClient(sockPath, "alice")
	ctx := context.Background()

	resp, err := c.StateDispatch(ctx, "state_nope", json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("StateDispatch: %v", err)
	}
	if !resp.IsError {
		t.Errorf("expected IsError for unknown tool, got %+v", resp)
	}
}
