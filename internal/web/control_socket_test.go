package web

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/kivali-ai/kivali/internal/agent"
	"github.com/kivali-ai/kivali/internal/controlclient"
	"github.com/kivali-ai/kivali/internal/messaging"
	"github.com/kivali-ai/kivali/internal/store"
)

// shortTempSocketPath returns a Unix-socket path under /tmp short
// enough to satisfy macOS's ~104-byte sun_path limit. t.TempDir()
// on macOS roots under /var/folders/.../T/<long-test-name>/, which
// blows past the limit for any nontrivial test name. Cleanup is
// handled via t.Cleanup so we don't leak sockets across test runs.
func shortTempSocketPath(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "wos-")
	if err != nil {
		t.Fatalf("mkdir /tmp: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return filepath.Join(dir, fmt.Sprintf("c%d.sock", time.Now().UnixNano()%1_000_000))
}

// TestControlSocketRouteMessageRoundTrip exercises the end-to-end
// flow that replaces the in-subprocess engine: a controlclient POST
// over the Unix socket lands a published task in the recipient's
// release-state inbox, AND the post-write Store hook fires NotifyOrgState
// so /org/stream subscribers see the update without an explicit
// notify call from the handler.
func TestControlSocketRouteMessageRoundTrip(t *testing.T) {
	srv := newTestServer(t)
	if err := srv.Store.CreateAgent(store.Agent{Slug: "alice", Role: "Analyst", ReportsTo: "ceo"}, "k"); err != nil {
		t.Fatalf("seed alice: %v", err)
	}
	if err := srv.Store.CreateAgent(store.Agent{Slug: "bob", Role: "Researcher", ReportsTo: "ceo"}, "k"); err != nil {
		t.Fatalf("seed bob: %v", err)
	}
	srv.Runtime = agent.NewRuntime(srv.Store, agent.RuntimeDefaults{})
	srv.Messenger = messaging.New(srv.Store, srv.Runtime)

	socketPath := shortTempSocketPath(t)
	stop, err := srv.StartControlSocket(socketPath)
	if err != nil {
		t.Fatalf("StartControlSocket: %v", err)
	}
	defer stop()

	// Subscribe to org-hub before the POST so we can observe the
	// notify firing via the Store hook.
	_, snaps := srv.orgHub.subscribe()
	defer srv.orgHub.unsubscribe(snaps)

	// Author a task message on disk (publish flow's WriteMessage
	// equivalent) so RouteMessage has a real path to reference.
	msg := store.Message{
		Type: store.MsgNotice, Title: "Test", From: "alice", To: store.Recipients{"bob"},
		Body: "hello",
	}
	abs, err := srv.Store.WriteMessage(msg)
	if err != nil {
		t.Fatalf("WriteMessage: %v", err)
	}
	msg.Path = abs

	// POST through the client — same code path the MCP subprocess
	// runs in production.
	// alice is the bound caller — controlclient stamps the slug
	// header on every request so the server can override msg.From.
	// The body still has From=alice so we won't notice the override
	// until the no-spoofing test below.
	client := controlclient.New(socketPath, "alice")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := client.RouteMessage(ctx, msg); err != nil {
		t.Fatalf("RouteMessage: %v", err)
	}

	// message queue should now show bob with one pending entry.
	ts, err := srv.Store.ReadMessageQueue()
	if err != nil {
		t.Fatalf("ReadMessageQueue: %v", err)
	}
	rt := ts.Agents["bob"]
	if len(rt.Inbox) != 1 {
		t.Fatalf("bob.Inbox = %v, want one entry", rt.Inbox)
	}

	// The Store post-write hook should have pinged the notifier;
	// after the debounce window a snapshot lands on our subscriber
	// carrying the queued message in inbox.pending_paths — the field
	// the inbox client diffs against to refetch the For-agents list.
	select {
	case body := <-snaps:
		var snap orgSnapshot
		if err := json.Unmarshal(body, &snap); err != nil {
			t.Fatalf("snapshot decode: %v", err)
		}
		if len(snap.Inbox.PendingPaths) != 1 {
			t.Errorf("snapshot pending_paths = %v, want one entry; full snap: %s", snap.Inbox.PendingPaths, body)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Store post-write hook did not result in a snapshot publish")
	}
}

// TestStoreHookFiresOnEngineWrite is a finer-grained test for the
// Store hook in isolation: anyone calling Store.WriteMessageQueue (web,
// engine, future callers) gets the hook fired exactly once per
// successful write.
func TestStoreHookFiresOnEngineWrite(t *testing.T) {
	srv := newTestServer(t)
	if err := srv.Store.CreateAgent(store.Agent{Slug: "alice", Role: "Analyst", ReportsTo: "ceo"}, "k"); err != nil {
		t.Fatalf("seed: %v", err)
	}
	srv.Runtime = agent.NewRuntime(srv.Store, agent.RuntimeDefaults{})
	srv.Messenger = messaging.New(srv.Store, srv.Runtime)

	// Subscribe; this gives us a channel that fires on each publish.
	_, snaps := srv.orgHub.subscribe()
	defer srv.orgHub.unsubscribe(snaps)

	// Direct engine write — no s.writeMessageQueue wrapper, no
	// explicit notify in the test. The Store hook is the only
	// mechanism that should fire NotifyOrgState here.
	msg := store.Message{
		Type: store.MsgNotice, Title: "From Engine", From: "ceo", To: store.Recipients{"alice"},
		Body: "x",
	}
	abs, err := srv.Store.WriteMessage(msg)
	if err != nil {
		t.Fatalf("WriteMessage: %v", err)
	}
	msg.Path = abs
	if _, err := srv.Messenger.Route(context.Background(), msg); err != nil {
		t.Fatalf("Messenger.Route: %v", err)
	}

	select {
	case <-snaps:
		// Good: the hook fired and the snapshot landed.
	case <-time.After(2 * time.Second):
		t.Fatal("Messenger.Route write did not fire the post-write hook")
	}
}
