package web

import (
	"context"
	"errors"
	"net/http"
	"testing"

	"github.com/kivali-ai/kivali/internal/clock"
	"github.com/kivali-ai/kivali/internal/integrationpod"
	"github.com/kivali-ai/kivali/internal/secrets"
	"github.com/kivali-ai/kivali/internal/store"
	"github.com/kivali-ai/kivali/internal/web/apitypes"
)

// The tests here hold a setup bundle's install that ends without being
// done to kivali/uninstall, as the runner's undo sends it
// (setupUndo), over the same fake shim as integration_setup_bundle_test.go.

// keepThenForm is a pod whose first call keeps a token and asks for the
// account, and whose later calls fail in the service's words.
func keepThenForm(n int, _ integrationpod.SetupParams, _ podProgress) (any, *integrationpod.RPCError, bool) {
	if n == 1 {
		return stepOf(map[string]any{"form": map[string]any{"fields": []any{"account"}}, "keep": map[string]any{"token": "half-made-tok"}}), nil, true
	}
	return nil, &integrationpod.RPCError{Code: -32000, Message: "the service is down"}, true
}

// Why this test exists: a setup bundle's install that fails after a
// Keep has made something on the service, and only its server can undo
// it: kivali/uninstall must be sent before the connection goes, as a
// built-in's Uninstall is called. A server between starts (every Keep
// starts it again) is waited for rather than skipped, so the undo does
// not depend on the moment of the failure; one that never comes leaves
// the service's side as it is, and the connection goes all the same.
func TestBundleFailedInstallIsUndone(t *testing.T) {
	srv, pod, _, _, mem := bundleSetupServer(t, keepThenForm, true)
	startSetupFlow(t, srv, "crm")
	pod.next(t)
	waitFlow(t, srv, "crm", apitypes.SetupFlowStateForm)
	orgOK[apitypes.SetupFlow](t, flowDo(t, srv, http.MethodPost, intBase+"/crm/setup/answer", apitypes.SetupFlowAnswer{Fields: map[string]string{"account": "acme"}}))
	pod.next(t)
	waitFlow(t, srv, "crm", apitypes.SetupFlowStateFailed)
	srv.setupAwaitDiscard(context.Background(), "crm")
	if n := pod.sent(integrationpod.MethodUninstall); n != 1 || len(mem.Names()) != 0 {
		t.Errorf("after a failed install: kivali/uninstall sent %d times, secrets %v", n, mem.Names())
	}

	cancelWithServerDown := func() (*Server, *setupPod, *clock.Fake, *secrets.Memory) {
		srv, pod, fs, clk, mem := bundleSetupServer(t, keepThenForm, true)
		startSetupFlow(t, srv, "crm")
		pod.next(t)
		waitFlow(t, srv, "crm", apitypes.SetupFlowStateForm)
		callsEnd(t, currentRun(t, srv, "crm"))
		srv.Integrations.disconnect(fs.link)
		if rr := flowDo(t, srv, http.MethodPost, intBase+"/crm/setup/cancel", nil); rr.Code != http.StatusNoContent {
			t.Fatalf("cancel: %d", rr.Code)
		}
		// The undo's wait for the server.
		clk.BlockUntil(1)
		return srv, pod, clk, mem
	}

	// A server that comes back while the undo waits is asked.
	srv, pod, _, mem = cancelWithServerDown()
	attachShim(t, srv, "crm", pod.answer)
	srv.setupAwaitDiscard(context.Background(), "crm")
	if n := pod.sent(integrationpod.MethodUninstall); n != 1 || len(mem.Names()) != 0 {
		t.Errorf("a server that came back: kivali/uninstall sent %d times, secrets %v", n, mem.Names())
	}

	// One that never comes: nothing is sent, and the connection goes.
	srv, pod, clk, mem := cancelWithServerDown()
	clk.Advance(bundleStartWait)
	srv.setupAwaitDiscard(context.Background(), "crm")
	if _, err := srv.Store.GetIntegrationConnection("crm"); !errors.Is(err, store.ErrNotFound) || len(mem.Names()) != 0 || pod.sent(integrationpod.MethodUninstall) != 0 {
		t.Errorf("a server that never came: %v, secrets %v, sent %d", err, mem.Names(), pod.sent(integrationpod.MethodUninstall))
	}
}

// Why this test exists: an install a stopped Kivali left unfinished is
// discarded at the next start, and a setup bundle's server is the only
// thing that can undo what it made on the service: kivali/uninstall must
// be sent then too, not only for an install this process saw fail.
func TestBundleRestartDiscardSendsUninstall(t *testing.T) {
	srv, pod, _, _, mem := bundleSetupServer(t, keepAndDone, true)
	ctx := context.Background()
	name := secrets.ConnectionSecretName("crm", "c0ffee03")
	if err := mem.Put(ctx, name, map[string]string{"token": "half-made-tok"}); err != nil {
		t.Fatal(err)
	}
	if err := srv.Store.PutIntegrationConnection("crm", store.IntegrationConnection{ID: "c0ffee03", CreatedAt: signInAt, SecretName: name, SecretKeys: []string{"token"}, SetupPending: true}); err != nil {
		t.Fatal(err)
	}
	srv.DiscardUnfinishedSetups()
	srv.setupAwaitDiscard(ctx, "crm")
	if n := pod.sent(integrationpod.MethodUninstall); n != 1 || len(mem.Names()) != 0 {
		t.Errorf("kivali/uninstall sent %d times, secrets %v", n, mem.Names())
	}
	if _, err := srv.Store.GetIntegrationConnection("crm"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("the record = %v", err)
	}
}
