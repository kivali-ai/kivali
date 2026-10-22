package web

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/kivali-ai/kivali/internal/connect/connecttest"
	"github.com/kivali-ai/kivali/internal/integrationpod"
)

// Why this test exists: a refresh that fails for a passing reason
// hands out the stored token only while it is still good. The vendor
// may take seconds to fail, and a token good when the call began may
// have expired by then; handed out, it fails the integration's call
// with a token the vendor no longer takes. Whether it is still good is
// read after the refresh attempt, not before it.
func TestSignInTransientFallbackReadsTheClockAfterTheRefresh(t *testing.T) {
	srv, vendor, clk := signInServer(t)
	signInBundle(t, srv, vendor)
	vendor.Set(func(k *connecttest.Knobs) { k.AccessTTL = time.Hour })
	signedIn(t, srv, "crm", vendor.Issue())
	clk.Advance(time.Hour - 20*time.Second)
	vendor.Set(func(k *connecttest.Knobs) {
		k.RefreshStatus = http.StatusGatewayTimeout
		// The vendor takes 30 seconds to fail.
		k.OnRefresh = func() { clk.Advance(30 * time.Second) }
	})
	if _, rpcErr := srv.Integrations.credential(context.Background(), "crm"); rpcErr == nil || rpcErr.Code != integrationpod.CodeVendorUnavailable {
		t.Errorf("error = %+v, want CodeVendorUnavailable for a token that expired during the refresh", rpcErr)
	}
}
