package web

import (
	"net/http"
	"testing"

	"github.com/kivali-ai/kivali/internal/web/apitypes"
)

// Why this test exists: Needs you is what the owner must act on now. A
// switched-off integration is called by no one, and a connection whose
// integration now runs a version connected with a token has no sign-in
// to renew; a row for either would ask the owner for a sign-in nothing
// uses.
func TestSignInNeedsYouRowOnlyWhereASignInIsUsed(t *testing.T) {
	srv, vendor, _ := signInServer(t)
	signInBundle(t, srv, vendor)
	signedIn(t, srv, "crm", vendor.Issue())
	srv.markSignInFailed("crm", "c1", "the vendor refused the sign-in (invalid_grant)")
	rows := func() int {
		home := decodeAPI[apitypes.Home](t, homeDo(t, srv, http.MethodGet, "/api/v1/home", "", nil, nil))
		n := 0
		for _, item := range home.Needs {
			if item.Kind == apitypes.NeedKindIntegrationSignIn {
				n++
			}
		}
		return n
	}
	if n := rows(); n != 1 {
		t.Fatalf("%d rows, want one", n)
	}
	if rr := intPost(t, srv, intBase+"/crm/disable"); rr.Code != http.StatusOK {
		t.Fatalf("disable: %d", rr.Code)
	}
	if n := rows(); n != 0 {
		t.Errorf("switched off: %d rows", n)
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
	if n := rows(); n != 0 {
		t.Errorf("a token version in use: %d rows", n)
	}
}
