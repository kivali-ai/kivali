package web

import (
	"context"
	"errors"
	"net/url"
	"testing"

	"github.com/kivali-ai/kivali/internal/secrets"
	"github.com/kivali-ai/kivali/internal/store"
	"github.com/kivali-ai/kivali/internal/web/apitypes"
)

// The tests here hold what a restart does to an install under way.
// Flows live in memory, so the connection an install's Keep wrote is
// marked unfinished (store.IntegrationConnection.SetupPending) until
// the flow is done, and one found so marked with no flow behind it is
// discarded (DiscardUnfinishedSetups).

// Why this test exists: Kivali stopping in the middle of an install
// loses the flow but not the connection its Keep wrote, so without the
// mark the integration would read installed with half a connection
// after the restart, its requirements failing on values setup never
// kept. The mark is set with the first Keep and cleared once the flow
// is done, so a finished install survives a restart whole.
func TestInstallMarksItsConnectionUntilDone(t *testing.T) {
	srv, e, clk := setupServer(t, nil)
	srv.Secrets = secrets.NewMemory()
	startSetupFlow(t, srv, "echo")
	_, cookie := echoGo(t, srv, "acme")
	if rec, err := srv.Store.GetIntegrationConnection("echo"); err != nil || !rec.SetupPending || rec.SetupTakenOver {
		t.Fatalf("after the first Keep: %+v %v", rec, err)
	}
	echoBack(t, srv, e, cookie, url.Values{"code": {"c0de-p1"}})
	waitFlow(t, srv, "echo", apitypes.SetupFlowStateWait)
	if page := echoPage(t, srv); page.Summary.State != apitypes.IntegrationStateSettingUp {
		t.Errorf("while the install runs = %s", page.Summary.State)
	}
	e.SetInstalled(true)
	echoCheck(t, srv, e, clk)
	waitFlow(t, srv, "echo", apitypes.SetupFlowStateDone)
	if rec, err := srv.Store.GetIntegrationConnection("echo"); err != nil || rec.SetupPending {
		t.Errorf("after Done: %+v %v", rec, err)
	}
	if page := echoPage(t, srv); page.Summary.State != apitypes.IntegrationStateInstalled {
		t.Errorf("after Done = %s", page.Summary.State)
	}
	// A restart finds nothing to discard.
	srv.DiscardUnfinishedSetups()
	srv.setupAwaitDiscard(context.Background(), "echo")
	if _, err := srv.Store.GetIntegrationConnection("echo"); err != nil {
		t.Errorf("a finished install's connection was discarded: %v", err)
	}
}

// Why this test exists: after a restart mid-install the connection is
// still marked, and no flow of the new process is behind it. It must
// read available, not installed, until it is discarded, and the discard
// must be the runner's (the integration asked to undo what it made on
// the service, then the Secret and the record gone), or the owner would
// be left with a connection nobody finished and no way to install
// again.
func TestRestartDiscardsAnUnfinishedInstall(t *testing.T) {
	srv, e, _ := setupServer(t, nil)
	mem := secrets.NewMemory()
	srv.Secrets = mem
	startSetupFlow(t, srv, "echo")
	_, cookie := echoGo(t, srv, "acme")
	echoBack(t, srv, e, cookie, url.Values{"code": {"c0de-p2"}})
	waitFlow(t, srv, "echo", apitypes.SetupFlowStateWait)
	made, _ := srv.Store.GetIntegrationConnection("echo")

	// The process stops: its flows are lost, and nothing is discarded.
	forgetFlowsAsARestartDoes(srv)
	if page := echoPage(t, srv); page.Summary.State != apitypes.IntegrationStateAvailable || page.Flow != nil {
		t.Errorf("after the restart = %s, flow %+v", page.Summary.State, page.Flow)
	}

	srv.DiscardUnfinishedSetups()
	srv.setupAwaitDiscard(context.Background(), "echo")
	if _, err := srv.Store.GetIntegrationConnection("echo"); !errors.Is(err, store.ErrNotFound) || len(mem.Names()) != 0 {
		t.Errorf("after the discard: %v, secrets %v", err, mem.Names())
	}
	if u := e.callsOf("uninstall"); len(u) != 1 || u[0].Conn.ID != made.ID || u[0].Conn.Fields["token"] != "tok-c0de-p2" {
		t.Errorf("uninstall calls = %+v", u)
	}
	if page := echoPage(t, srv); page.Summary.State != apitypes.IntegrationStateAvailable {
		t.Errorf("after the discard = %s", page.Summary.State)
	}
	// The owner installs again from nothing.
	startSetupFlow(t, srv, "echo")
	waitFlow(t, srv, "echo", apitypes.SetupFlowStateForm)
}

// Why this test exists: a store as a restart finds it (no flow in
// memory) may hold an unfinished install that took over a record an
// earlier version made, beside agents' access to it. Discarding that
// one must empty the record and keep the access, as the runner does for
// a take-over that fails; deleting it would lose what the owner set up
// with the earlier version. A record with no mark is the owner's and is
// left be.
func TestRestartKeepsTheAccessOfATakenOverRecord(t *testing.T) {
	srv, _, _ := setupServer(t, nil)
	mem := secrets.NewMemory()
	srv.Secrets = mem
	intTeam(t, srv, "dana")
	ctx := context.Background()
	name := secrets.ConnectionSecretName("echo", "c0ffee01")
	if err := mem.Put(ctx, name, map[string]string{"token": "half-made-token"}); err != nil {
		t.Fatal(err)
	}
	rec := store.IntegrationConnection{ID: "c0ffee01", CreatedAt: signInAt, SecretName: name, SecretKeys: []string{"token"}, SetupPending: true, SetupTakenOver: true}
	if err := srv.Store.PutIntegrationConnection("echo", rec); err != nil {
		t.Fatal(err)
	}
	if err := srv.Store.SetIntegrationAccess("echo", store.IntegrationAccess{Slug: "dana", SetAt: signInAt, SetBy: "ceo"}); err != nil {
		t.Fatal(err)
	}
	srv.DiscardUnfinishedSetups()
	srv.setupAwaitDiscard(ctx, "echo")
	got, err := srv.Store.GetIntegrationConnection("echo")
	if err != nil || got.ID != rec.ID || got.SetupPending || len(got.SecretKeys) != 0 || got.SecretName != "" || len(mem.Names()) != 0 {
		t.Errorf("the taken-over record = %+v %v, secrets %v", got, err, mem.Names())
	}
	if rows, _ := srv.Store.ListIntegrationAccess("echo"); len(rows) != 1 || rows[0].Slug != "dana" {
		t.Errorf("access = %+v", rows)
	}

	// The owner's own connection, with no mark, is not touched.
	srv2, _, _ := setupServer(t, nil)
	mem2 := secrets.NewMemory()
	srv2.Secrets = mem2
	name2 := secrets.ConnectionSecretName("echo", "c0ffee02")
	if err := mem2.Put(ctx, name2, map[string]string{"token": "finished-token", "account": "acme"}); err != nil {
		t.Fatal(err)
	}
	if err := srv2.Store.PutIntegrationConnection("echo", store.IntegrationConnection{ID: "c0ffee02", CreatedAt: signInAt, SecretName: name2, SecretKeys: []string{"account", "token"}}); err != nil {
		t.Fatal(err)
	}
	srv2.DiscardUnfinishedSetups()
	srv2.setupAwaitDiscard(ctx, "echo")
	if got, err := srv2.Store.GetIntegrationConnection("echo"); err != nil || got.ID != "c0ffee02" || len(mem2.Names()) != 1 {
		t.Errorf("a finished connection = %+v %v, secrets %v", got, err, mem2.Names())
	}
}

// forgetFlowsAsARestartDoes drops srv's flows from memory as a restart
// does: no flow is cancelled, ended or discarded, its goroutines
// stopped.
func forgetFlowsAsARestartDoes(srv *Server) {
	t := &srv.setupRuns
	t.mu.Lock()
	var runs []*setupRun
	for id, run := range t.runs {
		run.madeConn = ""
		run.cancel()
		delete(t.runs, id)
		runs = append(runs, run)
	}
	t.mu.Unlock()
	for _, run := range runs {
		run.calling.Wait()
	}
}
