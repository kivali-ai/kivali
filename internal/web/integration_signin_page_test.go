package web

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/kivali-ai/kivali/internal/builtinintegrations"
	"github.com/kivali-ai/kivali/internal/builtinintegrations/echotest"
	"github.com/kivali-ai/kivali/internal/connect/connecttest"
	"github.com/kivali-ai/kivali/internal/web/apitypes"
)

// Why this test exists: a refresh the vendor refused leaves agents'
// calls failing until the owner signs in again, and nothing else tells
// the owner. Home's Needs you must carry the unmet sign-in requirement
// as a row, built from the connection as it stands: core's title naming
// the integration, the vendor's refusal in the body, from Kivali, at
// the refusal's time, opening the integration's page. The row leaves
// once the connection is signed in again, and once it is deleted.
func TestSignInNeedsYouRow(t *testing.T) {
	srv, vendor, clk := signInServer(t)
	signInBundle(t, srv, vendor)
	vendor.Set(func(k *connecttest.Knobs) { k.AccessTTL = time.Hour; k.GitHubStyle = true })
	signedIn(t, srv, "crm", vendor.Issue())
	vendor.Set(func(k *connecttest.Knobs) { k.RefreshError = "bad_refresh_token" })
	clk.Advance(2 * time.Hour)
	if _, rpcErr := srv.Integrations.credential(context.Background(), "crm"); rpcErr == nil {
		t.Fatal("the refresh went through")
	}
	findRow := func() *apitypes.NeedItem {
		if row, ok := requirementNeeds(t, srv)["integration_requirement:crm/sign_in"]; ok {
			return &row
		}
		return nil
	}
	row := findRow()
	if row == nil {
		t.Fatal("no sign-in row")
	}
	want := "The service no longer accepts the sign-in: the vendor refused the sign-in (bad_refresh_token): refused by the test. Agents cannot use Test crm until you sign in again."
	if row.Title != "Test crm: Sign-in" || row.BodyMD != want ||
		row.From != (apitypes.PersonRef{Slug: "kivali", Name: "Kivali"}) || !row.At.Equal(clk.Now()) ||
		row.ReviewPath == nil || *row.ReviewPath != "/org/integrations/crm" || row.RawURL != nil {
		t.Errorf("row = %+v", row)
	}
	page := orgOK[apitypes.IntegrationPage](t, orgGet(t, srv, intBase+"/crm"))
	si := page.Connection.SignIn
	if si == nil || si.SignedIn || si.Error == nil || si.Method != "" || si.ExpiresAt != nil {
		t.Errorf("sign-in %+v", si)
	}

	// Signed in again: the record's error goes with the new sign-in.
	vendor.Set(func(k *connecttest.Knobs) { k.RefreshError = "" })
	signedIn(t, srv, "crm", vendor.Issue())
	if row := findRow(); row != nil {
		t.Errorf("signed in again, the row stays: %+v", row)
	}
	srv.markSignInFailed("crm", "c1", "the vendor refused the sign-in (invalid_grant)")
	if findRow() == nil {
		t.Fatal("no row after a second refusal")
	}
	if rr := intDelete(t, srv, intBase+"/crm"); rr.Code != http.StatusNoContent {
		t.Fatalf("delete: %d", rr.Code)
	}
	if row := findRow(); row != nil {
		t.Errorf("deleted, the row stays: %+v", row)
	}
}

// Why this test exists: the page is where the owner sees a sign-in
// connection stand. It must say how they may sign in (the manifest's
// method first), that it is signed in, how and when, and when the
// token is next refreshed; draw the form with the client id and secret
// core adds; and carry the manifest's scopes, method and setup notes,
// on the install screen too. A token connection's page carries none of
// the sign-in state.
func TestSignInConnectionOnThePage(t *testing.T) {
	srv, vendor, _ := signInServer(t)
	signInBundle(t, srv, vendor)
	vendor.Set(func(k *connecttest.Knobs) { k.AccessTTL = time.Hour })
	tok := vendor.Issue()
	signedIn(t, srv, "crm", tok)

	page := orgOK[apitypes.IntegrationPage](t, orgGet(t, srv, intBase+"/crm"))
	si := page.Connection.SignIn
	if si == nil || !si.SignedIn || si.Method != "device" || si.SignedInAt == nil || !si.SignedInAt.Equal(signInAt) ||
		si.ExpiresAt == nil || !si.ExpiresAt.Equal(tok.ExpiresAt) || si.Error != nil || strings.Join(si.Ways, ",") != "device" {
		t.Errorf("sign-in = %+v", si)
	}
	if page.ConnectionMethod != "device" ||
		strings.Join(page.Scopes, ",") != "repo" || page.SetupNotesMD != signInSetupNotes {
		t.Errorf("method %q, scopes %v, notes %q", page.ConnectionMethod, page.Scopes, page.SetupNotesMD)
	}
	var keys []string
	for _, f := range page.ConnectionFields {
		keys = append(keys, f.Key)
		if f.Key == "client_secret" && (!f.Sensitive || !f.Set || f.Value != nil) {
			t.Errorf("client secret field = %+v", f)
		}
		if f.Key == "client_id" && (f.Value == nil || *f.Value != "fake-client-id") {
			t.Errorf("client id field = %+v", f)
		}
	}
	if got := strings.Join(keys[len(keys)-2:], ","); got != "client_id,client_secret" || len(keys) != 4 {
		t.Errorf("form = %v", keys)
	}
	v := orgOK[apitypes.IntegrationVersion](t, orgGet(t, srv, intBase+"/crm/versions/1.0.0"))
	if v.ConnectionMethod != "device" || strings.Join(v.Scopes, ",") != "repo" || v.SetupNotesMD != signInSetupNotes {
		t.Errorf("install screen = %+v", v)
	}

	intConnected(t, srv, "hubspot")
	token := orgOK[apitypes.IntegrationPage](t, orgGet(t, srv, intBase+"/hubspot"))
	if token.Connection.SignIn != nil || len(token.Scopes) != 0 || token.ConnectionMethod != "token" || token.SetupNotesMD != "" {
		t.Errorf("a token connection's page = %+v", token)
	}
}

// Why this test exists: a sign-in connection that needs the owner to
// sign in again cannot be used, so the list must count its unmet
// sign-in requirement until the owner signs in again, not read every
// requirement met with every call failing.
func TestSignInRequirementOnTheList(t *testing.T) {
	srv, vendor, _ := signInServer(t)
	echo := echotest.New()
	c := signInConnection(vendor)
	echo.SignIn = &c
	srv.BuiltinIntegrations = builtinintegrations.Registry{echo}
	withIntegrationRegistry(srv, nil)
	if err := srv.Store.InstallBuiltinIntegrations(srv.BuiltinIntegrations.Manifests(), signInAt); err != nil {
		t.Fatal(err)
	}
	signedIn(t, srv, echotest.Name, vendor.Issue())
	if sum := echoSummary(t, srv); sum.State != apitypes.IntegrationStateConnected || sum.Needs != 0 {
		t.Errorf("signed in: %+v", sum)
	}
	srv.markSignInFailed(echotest.Name, "c1", "the vendor refused the sign-in (invalid_grant)")
	if sum := echoSummary(t, srv); sum.State != apitypes.IntegrationStateConnected || sum.Needs != 1 {
		t.Errorf("needs a new sign-in: %+v", sum)
	}
}
