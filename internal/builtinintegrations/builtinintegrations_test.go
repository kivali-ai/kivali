package builtinintegrations_test

import (
	"context"
	"slices"
	"testing"

	"github.com/kivali-ai/kivali/internal/builtinintegrations"
	"github.com/kivali-ai/kivali/internal/builtinintegrations/echotest"
	"github.com/kivali-ai/kivali/internal/integration"
)

// checkRegistry holds every built-in of r to what boot and core assume
// of one: a manifest that passes the built-in check with no finding,
// a name no other built-in has, and a list of actions the manifest
// declares (strictTools; the test built-in lists one more on purpose).
func checkRegistry(t *testing.T, r builtinintegrations.Registry, strictTools bool) {
	t.Helper()
	seen := map[string]bool{}
	for _, bi := range r {
		m := bi.Manifest()
		if m == nil {
			t.Fatalf("a built-in has no manifest")
		}
		if f := integration.ValidateBuiltin(m); len(f) != 0 {
			t.Errorf("%s: findings: %v", m.Name, f.Texts())
		}
		if seen[m.Name] {
			t.Errorf("%s is registered twice", m.Name)
		}
		seen[m.Name] = true
		if r.Lookup(m.Name) != bi {
			t.Errorf("%s: Lookup finds another", m.Name)
		}
		tools, err := bi.Tools(context.Background(), builtinintegrations.Caller{Access: map[string]any{}})
		if err != nil {
			t.Errorf("%s: Tools: %v", m.Name, err)
		}
		for _, tl := range tools {
			if strictTools && !slices.ContainsFunc(m.Tools, func(d integration.Tool) bool { return d.Name == tl.Name }) {
				t.Errorf("%s: Tools lists %s, which the manifest does not declare", m.Name, tl.Name)
			}
		}
	}
}

// Why this test exists: every built-in compiled into the binary is
// written to the store at boot and drawn by the Org page from its
// manifest. A manifest that fails the check, or two built-ins with one
// name, would put a record on every team that the page cannot draw or
// that core cannot tell apart.
func TestEveryBuiltinPassesTheCheck(t *testing.T) {
	checkRegistry(t, builtinintegrations.All(), true)
}

// Why this test exists: the test built-in is what the server's tests
// prove dispatch with. A manifest of its that the check refuses would
// make those tests prove something no real built-in could ship.
func TestEchoPassesTheCheck(t *testing.T) {
	checkRegistry(t, builtinintegrations.Registry{echotest.New()}, false)
	if builtinintegrations.All().Lookup(echotest.Name) != nil {
		t.Errorf("the test built-in is in the binary's list")
	}
}
