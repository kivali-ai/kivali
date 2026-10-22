package web

import (
	"context"
	"testing"
	"time"

	"github.com/kivali-ai/kivali/internal/connect/connecttest"
)

// Why this test exists: a vendor whose refresh answer carries no
// expires_in has not said the new token never expires; it has said
// nothing. Read as "never", core would stop refreshing a token that
// does expire, and every call after its real expiry would fail. A
// stored token that had a lifetime keeps it for the refreshed one,
// refresh after refresh, without growing.
func TestSignInRefreshWithoutExpiryKeepsTheLifetime(t *testing.T) {
	srv, vendor, clk := signInServer(t)
	signInBundle(t, srv, vendor)
	vendor.Set(func(k *connecttest.Knobs) { k.AccessTTL = time.Hour })
	signedIn(t, srv, "crm", vendor.Issue())
	vendor.Set(func(k *connecttest.Knobs) { k.AccessTTL = 0 })
	for i := range 3 {
		clk.Advance(time.Hour)
		if _, rpcErr := srv.Integrations.credential(context.Background(), "crm"); rpcErr != nil {
			t.Fatal(rpcErr)
		}
		rec, _ := connectionSecret(t, srv, "crm")
		if want := clk.Now().Add(time.Hour); !rec.SignIn.ExpiresAt.Equal(want) {
			t.Errorf("refresh %d: expires %v, want %v: the stored lifetime kept", i, rec.SignIn.ExpiresAt, want)
		}
	}
	if n := vendor.Refreshes(); n != 3 {
		t.Errorf("%d refreshes, want one per expiry", n)
	}
}
