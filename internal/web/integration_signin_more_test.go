package web

import (
	"net/http"
	"testing"

	"github.com/kivali-ai/kivali/internal/web/apitypes"
)

// Why this test exists: a refresh refused while the owner signs out
// (the record's sign-in cleared under it) must not write an error onto
// a connection that has no sign-in: the owner would be told to sign in
// again to a connection they just signed out of, by a Needs you row
// nothing clears but a delete.
func TestSignInRefusalAfterSignOutLeavesTheRecord(t *testing.T) {
	srv, vendor, _ := signInServer(t)
	signInBundle(t, srv, vendor)
	rec := signedIn(t, srv, "crm", vendor.Issue())
	rec.SignIn = nil
	if err := srv.Store.PutIntegrationConnection("crm", rec); err != nil {
		t.Fatal(err)
	}
	srv.markSignInFailed("crm", "c1", "the vendor refused the sign-in (invalid_grant)")
	got, _ := connectionSecret(t, srv, "crm")
	if got.SignIn != nil {
		t.Errorf("sign-in = %+v, want none", got.SignIn)
	}
	home := decodeAPI[apitypes.Home](t, homeDo(t, srv, http.MethodGet, "/api/v1/home", "", nil, nil))
	for _, n := range home.Needs {
		if n.Kind == apitypes.NeedKindIntegrationSignIn {
			t.Errorf("a row for a signed-out connection: %+v", n)
		}
	}
}
