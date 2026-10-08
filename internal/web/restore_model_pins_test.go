package web

import (
	"net/http"
	"testing"

	"github.com/kivali-ai/kivali/internal/provider"
	"github.com/kivali-ai/kivali/internal/store"
)

// TestRestoreMovesModelPinsAlongTheirLineage pins the third place the
// upgrade rule applies. A backup carries agent.yaml verbatim, pinned
// against the provider of whatever binary wrote it, and a restore does
// not restart the process — so without its own pass a restored agent
// would run on a model this binary's picker does not offer until the
// next boot, and the detail page would show it as a legacy pin the
// operator never chose. The restore has to end the way boot does.
func TestRestoreMovesModelPinsAlongTheirLineage(t *testing.T) {
	const retired = provider.MockModelRetired
	want := provider.MockProvider{}.Current(retired)
	if want == retired {
		t.Fatalf("Current(%q) = %q; the test needs a retired pin with somewhere to go", retired, want)
	}

	src := newTestServer(t)
	if err := src.Store.CreateAgent(store.Agent{Slug: "alice", Role: "Analyst", ReportsTo: "ceo", Model: retired}, "k"); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if err := src.Store.CreateAgent(store.Agent{Slug: "bob", Role: "Analyst", ReportsTo: "ceo"}, "k"); err != nil {
		t.Fatalf("seed: %v", err)
	}
	dlRR := backupZip(t, src)
	if dlRR.Code != http.StatusOK {
		t.Fatalf("backup code = %d", dlRR.Code)
	}

	dst := newTestServer(t)
	upRR := restoreZip(t, dst, "backup.zip", dlRR.Body.Bytes())
	if upRR.Code != http.StatusOK {
		t.Fatalf("restore code = %d, body = %s", upRR.Code, upRR.Body.String())
	}

	alice, err := dst.Store.GetAgent("alice")
	if err != nil {
		t.Fatalf("restored alice: %v", err)
	}
	if alice.Model != want {
		t.Errorf("alice runs on %q after restore, want %q (the retired pin's current row)", alice.Model, want)
	}
	// The pin the source wrote is untouched there — the pass is the
	// restore's, not the backup's.
	if a, _ := src.Store.GetAgent("alice"); a.Model != retired {
		t.Errorf("source alice = %q, want the original %q", a.Model, retired)
	}
	// An inheriting agent stays inheriting; the fleet default is
	// config's to resolve.
	if bob, _ := dst.Store.GetAgent("bob"); bob.Model != "" {
		t.Errorf("bob = %q after restore, want the empty inherit pin", bob.Model)
	}
}
