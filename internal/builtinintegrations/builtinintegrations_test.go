package builtinintegrations_test

import (
	"context"
	"slices"
	"testing"

	"github.com/kivali-ai/kivali/internal/builtinintegrations"
	"github.com/kivali-ai/kivali/internal/builtinintegrations/builtins"
	"github.com/kivali-ai/kivali/internal/builtinintegrations/echotest"
	"github.com/kivali-ai/kivali/internal/integration"
	"github.com/kivali-ai/kivali/internal/integration/integrationtest"
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
	checkRegistry(t, builtins.All(), true)
}

// Why this test exists: the test built-in is what the server's tests
// prove dispatch with. A manifest of its that the check refuses would
// make those tests prove something no real built-in could ship. Each of
// its shapes is held to it: a token connection, a sign-in one, and a
// setup one, alone and handing over to a browser or a device sign-in.
// Echo implements the setup hooks in every shape, so outside its setup
// shape they must do nothing: rows or an Uninstall call there would
// change every other test of a token or sign-in built-in.
func TestEchoPassesTheCheck(t *testing.T) {
	const ep = "https://" + echotest.SetupHost
	token := echotest.New()
	signIn := echotest.New()
	signIn.SignIn = &integration.Connection{
		Method: integration.MethodDevice, DeviceEndpoint: ep + "/device", TokenEndpoint: ep + "/token",
		CredentialKeys: map[string]string{"access_token": "TOKEN"},
	}
	for _, e := range []*echotest.Echo{token, signIn} {
		checkRegistry(t, builtinintegrations.Registry{e}, false)
		rows, err := e.Requirements(context.Background(), builtinintegrations.Connection{})
		if rows != nil || err != nil || e.Uninstall(context.Background(), builtinintegrations.Connection{}) != nil || len(e.Calls()) != 0 {
			t.Errorf("outside setup: rows %v, %v, calls %v", rows, err, e.Calls())
		}
	}
	setup := echotest.New()
	setup.SetupFlow = true
	browser := echotest.New()
	browser.SetupFlow = true
	browser.HandOver = &integration.Connection{
		Method: integration.MethodBrowser, AuthorizationEndpoint: ep + "/authorize", TokenEndpoint: ep + "/token",
		CredentialKeys: map[string]string{"access_token": "TOKEN"},
	}
	device := echotest.New()
	device.SetupFlow = true
	device.HandOver = signIn.SignIn
	for _, tc := range []struct {
		e    *echotest.Echo
		then string
	}{{setup, ""}, {browser, integration.MethodBrowser}, {device, integration.MethodDevice}} {
		checkRegistry(t, builtinintegrations.Registry{tc.e}, false)
		if c := tc.e.Manifest().Kivali.Connection; c.Method != integration.MethodSetup || c.Setup == nil || c.Setup.Then != tc.then {
			t.Errorf("the setup shape's connection = %+v", c)
		}
	}
	if builtins.All().Lookup(echotest.Name) != nil {
		t.Errorf("the test built-in is in the binary's list")
	}
}

// checkShipped holds every shipped bundle of b to what boot and core
// assume of one: files that read as a bundle, a manifest that names the
// bundle's own name and passes an upload's check with no error, and a
// name no other integration that ships with Kivali has (core tells a
// built-in of Go code from a shipped bundle by its name).
func checkShipped(t *testing.T, b builtinintegrations.ShippedBundles, goBuiltins builtinintegrations.Registry) {
	t.Helper()
	seen := map[string]bool{}
	for _, sb := range b {
		bundle, err := integration.ReadDirectory(sb.FS)
		if err != nil {
			t.Errorf("%s: %v", sb.Name, err)
			continue
		}
		if bundle.Manifest.Name != sb.Name {
			t.Errorf("%s: the manifest names %s", sb.Name, bundle.Manifest.Name)
		}
		if f := integration.Validate(bundle.Manifest, bundle.Files); f.HasErrors() {
			t.Errorf("%s: findings: %v", sb.Name, f.Texts())
		}
		if seen[sb.Name] || goBuiltins.Lookup(sb.Name) != nil {
			t.Errorf("%s is registered twice", sb.Name)
		}
		seen[sb.Name] = true
		if !b.Has(sb.Name) || b.ByName()[sb.Name] == nil {
			t.Errorf("%s: Has or ByName does not find it", sb.Name)
		}
	}
}

// Why this test exists: every bundle shipped in the binary is checked
// at boot as an upload is, and one that fails is not installed. A
// bundle that reads wrong, names another integration or shares a name
// with a built-in of Go code would reach every team as an integration
// that never installs, or that core runs the wrong way.
func TestEveryShippedBundlePassesTheCheck(t *testing.T) {
	checkShipped(t, builtins.Shipped(), builtins.All())
}

// Why this test exists: the check above passes only as long as every
// bundle the binary ships does. A test bundle laid out as an embedded
// one is held to it here, so the check is known to read a bundle
// directory and pass a good one whatever the binary ships.
func TestShippedBundleCheckReadsADirectory(t *testing.T) {
	fsys := integrationtest.Dir(t, integrationtest.Manifest("notes", "1.0.0"), nil)
	checkShipped(t, builtinintegrations.ShippedBundles{{Name: "notes", FS: fsys}}, builtins.All())
}
