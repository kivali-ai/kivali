package web

import (
	"net/http"
	"testing"
)

// Why this test exists: Needs you is what the owner must act on now. A
// switched-off integration is called by no one, and a connection whose
// integration now runs a version connected with a token has no sign-in
// to renew; a sign-in row for either would ask the owner for a sign-in
// nothing uses.
func TestSignInNeedsYouRowOnlyWhereASignInIsUsed(t *testing.T) {
	srv, vendor, _ := signInServer(t)
	signInBundle(t, srv, vendor)
	signedIn(t, srv, "crm", vendor.Issue())
	srv.markSignInFailed("crm", "c1", "the vendor refused the sign-in (invalid_grant)")
	const row = "integration_requirement:crm/sign_in"
	if _, ok := requirementNeeds(t, srv)[row]; !ok {
		t.Fatalf("no sign-in row: %v", requirementNeeds(t, srv))
	}
	if rr := intPost(t, srv, intBase+"/crm/disable"); rr.Code != http.StatusOK {
		t.Fatalf("disable: %d", rr.Code)
	}
	if got := requirementNeeds(t, srv); len(got) != 0 {
		t.Errorf("switched off: %v", got)
	}
	if rr := intPost(t, srv, intBase+"/crm/enable"); rr.Code != http.StatusOK {
		t.Fatalf("enable: %d", rr.Code)
	}
	// A token version in use in its place.
	if rr := intUpload(t, srv, intBundle(t, "crm", "1.1.0")); rr.Code != http.StatusCreated {
		t.Fatalf("upload: %d %s", rr.Code, rr.Body.String())
	}
	if rr := intPost(t, srv, intBase+"/crm/versions/1.1.0/approve"); rr.Code != http.StatusOK {
		t.Fatalf("approve: %d %s", rr.Code, rr.Body.String())
	}
	if _, ok := requirementNeeds(t, srv)[row]; ok {
		t.Error("a token version in use still has a sign-in row")
	}
}
