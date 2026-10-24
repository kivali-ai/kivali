package web

import (
	"testing"
)

// Why this test exists: a refresh refused while the owner signs out
// (the record's sign-in cleared under it) must not write an error onto
// a connection that has no sign-in: the owner would be told the
// service refused a sign-in they just ended, rather than that the
// connection is not signed in.
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
	if row := requirementNeeds(t, srv)["integration_requirement:crm/sign_in"]; row.BodyMD != "Not signed in yet." {
		t.Errorf("sign-in row of a signed-out connection = %+v", row)
	}
}
