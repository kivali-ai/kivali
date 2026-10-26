package web

import (
	"context"
	"net/http"
	"slices"
	"testing"
	"time"

	"github.com/kivali-ai/kivali/internal/builtinintegrations/echotest"
	"github.com/kivali-ai/kivali/internal/connect"
	"github.com/kivali-ai/kivali/internal/connect/connecttest"
	"github.com/kivali-ai/kivali/internal/integration"
	"github.com/kivali-ai/kivali/internal/secrets"
	"github.com/kivali-ai/kivali/internal/store"
	"github.com/kivali-ai/kivali/internal/web/apitypes"
)

// Why this test exists: setupMisfit decides which connections read
// available and are taken over, values gone, by the next install. A
// rule too wide would wipe a working installation's values on a Fix
// or read a healthy one as needing setup again; one too narrow would
// leave an earlier version's connection reading installed, its calls
// failing with nothing on Needs you. The rows under noClient guard a
// setup whose keeps lack the app's client id and secret (GitHub's)
// against a connection that holds them beside its keeps, as a GitHub
// setup that kept them left it: read as a misfit, its agents' calls
// would be refused and the owner told to set GitHub up again, making a
// second app on GitHub, over two values nothing reads.
func TestSetupMisfit(t *testing.T) {
	keeps := integration.SetupKeeps{{Key: "app_id"}, {Key: "slug"}, {Key: "client_id"}, {Key: "client_secret"}, {Key: "private_key"}}
	setup := &integration.Setup{Keeps: keeps}
	handOver := &integration.Setup{Keeps: keeps, Then: integration.MethodBrowser}
	noClient := &integration.Setup{Keeps: integration.SetupKeeps{{Key: "app_id"}, {Key: "slug"}, {Key: "private_key"}, {Key: "installation_id"}}}
	signedIn := &store.ConnectionSignIn{Method: "browser"}
	for _, c := range []struct {
		name  string
		setup *integration.Setup
		rec   store.IntegrationConnection
		want  bool
	}{
		{"a 1.0.0 record: a client id in the record and a sign-in", setup,
			store.IntegrationConnection{Fields: map[string]string{"client_id": "Iv1old"}, SecretKeys: []string{"client_secret"}, SignIn: signedIn}, true},
		{"the five keeps", setup, store.IntegrationConnection{SecretKeys: []string{"app_id", "client_id", "client_secret", "private_key", "slug"}}, false},
		{"keeps and a key outside them", setup, store.IntegrationConnection{SecretKeys: []string{"app_id", "token"}}, true},
		{"a sign-in after a hand-over", handOver, store.IntegrationConnection{SecretKeys: []string{"client_id", "client_secret"}, SignIn: signedIn}, false},
		{"a sign-in with no hand-over", setup, store.IntegrationConnection{SecretKeys: []string{"client_id"}, SignIn: signedIn}, true},
		{"an empty record", setup, store.IntegrationConnection{}, true},
		{"keeps and the app's client beside them", noClient,
			store.IntegrationConnection{SecretKeys: []string{"app_id", "client_id", "client_secret", "installation_id", "private_key", "slug"}}, false},
		{"the app's client alone", noClient, store.IntegrationConnection{SecretKeys: []string{"client_id", "client_secret"}}, true},
		{"keeps, the app's client and a key outside them", noClient, store.IntegrationConnection{SecretKeys: []string{"app_id", "client_secret", "token"}}, true},
		{"a 1.0.0 record, for keeps without a client", noClient,
			store.IntegrationConnection{Fields: map[string]string{"client_id": "Iv1old"}, SecretKeys: []string{"client_secret"}, SignIn: signedIn}, true},
		{"a 1.0.0 record not signed in, for keeps without a client", noClient,
			store.IntegrationConnection{Fields: map[string]string{"client_id": "Iv1old"}, SecretKeys: []string{"client_secret"}}, true},
		{"no setup, a 1.0.0 record", nil,
			store.IntegrationConnection{Fields: map[string]string{"client_id": "Iv1old"}, SecretKeys: []string{"client_secret"}, SignIn: signedIn}, false},
		{"no setup, an empty record", nil, store.IntegrationConnection{}, false},
	} {
		if got := setupMisfit(c.setup, c.rec); got != c.want {
			t.Errorf("%s: misfit = %v, want %v", c.name, got, c.want)
		}
	}
}

// echoSignedInEarlier makes echo's version 1.0.0 a sign-in against
// vendor, superseded by the setup echo srv carries (2.0.0), and leaves
// a connection 1.0.0 signed in with, its tokens issued by vendor, and
// dana's access.
func echoSignedInEarlier(t *testing.T, srv *Server, mem *secrets.Memory, vendor *connecttest.FakeVendor) (store.IntegrationConnection, connect.Tokens) {
	t.Helper()
	v1 := echotest.New()
	v1.Version = "1.0.0"
	c := signInConnection(vendor)
	v1.SignIn = &c
	boot := signInAt.Add(-time.Hour)
	if err := srv.Store.InstallBuiltinIntegrations([]*integration.Manifest{v1.Manifest()}, boot); err != nil {
		t.Fatal(err)
	}
	tok := vendor.Issue()
	rec := store.IntegrationConnection{
		ID: "c0ffee02", CreatedAt: boot, Fields: map[string]string{integration.FieldClientID: connecttest.ClientID},
		SecretKeys: []string{integration.FieldClientSecret}, SecretName: secrets.ConnectionSecretName("echo", "c0ffee02"),
		SignIn: &store.ConnectionSignIn{Method: "device", SignedInAt: boot},
	}
	if err := mem.Put(context.Background(), rec.SecretName, map[string]string{
		integration.FieldClientSecret: connecttest.ClientSecret, connect.SecretKeyAccessToken: tok.Access, connect.SecretKeyRefreshToken: tok.Refresh,
	}); err != nil {
		t.Fatal(err)
	}
	if err := srv.Store.PutIntegrationConnection("echo", rec); err != nil {
		t.Fatal(err)
	}
	intTeam(t, srv, "dana")
	if err := srv.Store.SetIntegrationAccess("echo", store.IntegrationAccess{Slug: "dana", SetAt: boot, SetBy: "ceo", Access: map[string]any{}}); err != nil {
		t.Fatal(err)
	}
	if err := srv.Store.InstallBuiltinIntegrations(srv.BuiltinIntegrations.Manifests(), signInAt); err != nil {
		t.Fatal(err)
	}
	return rec, tok
}

// echoMisfitServer is a server whose echo is a setup integration at
// 2.0.0, its 1.0.0 a sign-in against a fake vendor that issued the
// connection's tokens.
func echoMisfitServer(t *testing.T) (*Server, *setupEcho, *secrets.Memory, *connecttest.FakeVendor, connect.Tokens) {
	t.Helper()
	srv, e, clk := setupServerWith(t, nil, func(e *echotest.Echo) { e.Version = "2.0.0" })
	mem := secrets.NewMemory()
	srv.Secrets = mem
	vendor := connecttest.New(t, clk)
	srv.Connect = &connect.Engine{Clock: clk, HTTP: vendor.Server.Client()}
	_, tok := echoSignedInEarlier(t, srv, mem, vendor)
	return srv, e, mem, vendor, tok
}

// runnerSettled waits for every goroutine of the setup runner, the
// revocations it starts among them, failing after failsafeWait.
func runnerSettled(t *testing.T, srv *Server) {
	t.Helper()
	done := make(chan struct{})
	go func() {
		srv.setupRuns.calling.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(failsafeWait):
		t.Fatal("the setup runner did not settle")
	}
}

// Why this test exists: a sign-in an earlier version's connection held
// stays good with the vendor unless Kivali revokes it, and the install
// that takes the connection over drops the tokens that could. When the
// earlier version signed in by the standard way, with a revocation
// endpoint, core must revoke as it revokes any sign-in, without the
// integration doing anything of its own.
func TestInstallOverAnEarlierSignInRevokesIt(t *testing.T) {
	srv, e, _, vendor, tok := echoMisfitServer(t)
	if page := echoPage(t, srv); page.Summary.State != apitypes.IntegrationStateInstalled || !page.CanDisconnect {
		t.Fatalf("before = %s, can disconnect %v", page.Summary.State, page.CanUninstall)
	}
	installEcho(t, srv, e, "acme")
	runnerSettled(t, srv)
	if got := vendor.Revoked(); !slices.Contains(got, tok.Refresh) || !slices.Contains(got, tok.Access) {
		t.Errorf("revoked = %v", got)
	}
	if page := echoPage(t, srv); page.Summary.State != apitypes.IntegrationStateConnected || len(page.Access) != 1 {
		t.Errorf("after = %s, access %+v", page.Summary.State, page.Access)
	}
}

// Why this test exists: the owner may drop an earlier version's
// connection rather than set it up again. Disconnect must be offered,
// remove the connection and every agent's access, and revoke the
// sign-in it held, which this version's manifest knows nothing of.
func TestDisconnectAnEarlierSignInRevokesIt(t *testing.T) {
	srv, _, mem, vendor, tok := echoMisfitServer(t)
	if rr := intDelete(t, srv, intBase+"/echo/connection"); rr.Code != http.StatusOK {
		t.Fatalf("disconnect = %d %s", rr.Code, rr.Body.String())
	}
	runnerSettled(t, srv)
	if got := vendor.Revoked(); !slices.Contains(got, tok.Refresh) || !slices.Contains(got, tok.Access) {
		t.Errorf("revoked = %v", got)
	}
	page := echoPage(t, srv)
	if page.Summary.State != apitypes.IntegrationStateInstalled || len(page.Access) != 0 || len(page.Requirements) != 0 || len(mem.Names()) != 0 {
		t.Errorf("after = %s, access %+v, rows %+v, secrets %v", page.Summary.State, page.Access, page.Requirements, mem.Names())
	}
}
