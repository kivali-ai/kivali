package web

import (
	"context"
	"encoding/json"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kivali-ai/kivali/internal/agentpod"
	"github.com/kivali-ai/kivali/internal/builtinintegrations"
	"github.com/kivali-ai/kivali/internal/builtinintegrations/echotest"
	"github.com/kivali-ai/kivali/internal/clock"
	"github.com/kivali-ai/kivali/internal/integration"
	"github.com/kivali-ai/kivali/internal/integration/integrationtest"
	"github.com/kivali-ai/kivali/internal/integrationpod"
	"github.com/kivali-ai/kivali/internal/store"
	"github.com/kivali-ai/kivali/internal/web/apitypes"
)

// shippedAt is when the tests' boot installs a shipped bundle.
var shippedAt = time.Date(2026, 10, 21, 9, 0, 0, 0, time.UTC)

// shippedNotes is the fake integration's server laid out as the binary
// embeds a bundle, as notes at version: its three tools, whoami as Test
// connection and list_colors listing the blogs field's options, with
// main.py as its server's code (the shim runs the test binary in its
// place).
func shippedNotes(t *testing.T, version, main string) fs.FS {
	t.Helper()
	m := fakeServerManifest("notes")
	m["version"] = version
	return integrationtest.Dir(t, m, map[string]integrationtest.File{"server/main.py": {Body: main}})
}

// installShipped makes fsys srv's shipped notes bundle and installs it
// as boot does, returning the names boot wrote again.
func installShipped(t *testing.T, srv *Server, fsys fs.FS) []string {
	t.Helper()
	srv.ShippedBundles = builtinintegrations.ShippedBundles{{Name: "notes", FS: fsys}}
	re, err := srv.Store.InstallShippedBundles(srv.ShippedBundles.ByName(), shippedAt)
	if err != nil {
		t.Fatal(err)
	}
	return re
}

func connectNotes(t *testing.T, srv *Server, token string) {
	t.Helper()
	rr := orgJSON(t, srv, http.MethodPut, intBase+"/notes/connection", apitypes.ConnectionWrite{Fields: map[string]string{"token": token, "portal": "acme"}})
	if rr.Code != http.StatusOK {
		t.Fatalf("connect: %d %s", rr.Code, rr.Body.String())
	}
}

// Why this test exists: a shipped bundle is built in but runs as an
// uploaded bundle does, end to end with core's socket, the real shim
// and the fake server run from the files boot wrote from the binary.
// Its status must say not running until its shim connects (a built-in
// of Go code never does, and a page that said connected would offer a
// Test connection that cannot run); reconcile must start its pod (a
// built-in record skipped there would never run); and once the shim
// connects, Test connection, the access form's options, an agent's
// list and an agent's call must all reach the server, with the pod's
// scoped proxy credential admitted to the hosts its manifest lists.
func TestShippedBundleRunsInItsOwnPod(t *testing.T) {
	srv := newTestServer(t)
	syncPath := filepath.Join(t.TempDir(), "agents.json")
	srv.Store.SetEgressAgentsSyncPath(syncPath)
	prov := newFakeIntegrationProv()
	reg, _ := withIntegrationRegistry(srv, prov)
	installShipped(t, srv, shippedNotes(t, "1.0.0", integrationtest.MainPy))
	intTeam(t, srv, "dana")
	connectNotes(t, srv, "s3cret-1")
	putIntegrationAccess(t, srv, "notes", "dana", `{"blogs":["red"]}`)

	page := orgOK[apitypes.IntegrationPage](t, orgGet(t, srv, intBase+"/notes"))
	if page.Summary.Status != apitypes.IntegrationStatusNotRunning || !page.Summary.Builtin || page.CanRemove ||
		strings.Join(page.Hosts, ",") != "api.example.com" {
		t.Errorf("page before the shim = %+v, hosts %v, can remove %v", page.Summary, page.Hosts, page.CanRemove)
	}
	if e := assertAPIError(t, intPost(t, srv, intBase+"/notes/connection/verify"), http.StatusConflict); e.Error != errNotRunning || e.Who != whoDevelopers {
		t.Errorf("verify before the shim = %+v", e)
	}
	if e := assertAPIError(t, orgGet(t, srv, intBase+"/notes/choices/blogs"), http.StatusConflict); e.Error != errNotRunning {
		t.Errorf("choices before the shim = %+v", e)
	}
	if res := callAs(t, srv, "dana", "notes_echo", `{}`); !res.IsError || len(res.Content) != 1 || res.Content[0] != refuseNotRunning("Test notes") {
		t.Errorf("a call before the shim = %+v", res)
	}

	connected := make(chan string, 4)
	reg.onConnect = func(name string) { connected <- name }
	if err := reg.Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	if calls := prov.takeCalls(); strings.Join(calls, ",") != "start notes 1.0.0" {
		t.Errorf("pods = %v", calls)
	}
	e, _ := entryOf(reg, "notes")
	sock, _ := integrationSocket(t, srv)
	bundle, err := srv.Store.IntegrationBundleDir("notes", "1.0.0")
	if err != nil {
		t.Fatal(err)
	}
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	shim := &integrationpod.Shim{
		Name: "notes", Version: "1.0.0", Token: e.token,
		CoreUDS: sock, Bundle: bundle,
		ProxyURL: "http://10.43.0.12:3128",
		Command:  exe,
		Path:     os.Getenv("PATH"),
		Clock:    clock.NewFake(),
		Logf:     t.Logf,
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan int, 1)
	go func() { done <- shim.Run(ctx) }()
	t.Cleanup(func() {
		cancel()
		<-done
	})
	select {
	case <-connected:
	case <-time.After(failsafeWait):
		t.Fatal("the shim did not connect")
	}
	reg.checks.Wait()

	page = orgOK[apitypes.IntegrationPage](t, intPost(t, srv, intBase+"/notes/connection/verify"))
	if page.Summary.Status != apitypes.IntegrationStatusConnected || page.Connection == nil || page.Connection.Verified == nil ||
		!*page.Connection.Verified || *page.Connection.VerifyMessage != "fake account "+integrationSecretPlaceholder {
		t.Fatalf("verify = %+v %+v", page.Summary, page.Connection)
	}
	if got := orgOK[apitypes.AccessChoices](t, orgGet(t, srv, intBase+"/notes/choices/blogs")); strings.Join(got.Options, ",") != "red,green" {
		t.Errorf("options = %v", got.Options)
	}
	var file store.EgressAgents
	b, _ := os.ReadFile(syncPath)
	_ = json.Unmarshal(b, &file)
	if got := file.Scoped[integrationpod.ProxyUser("notes")]; len(got.Hash) != 64 || strings.Join(got.Hosts, ",") != "api.example.com" {
		t.Errorf("scoped credential = %+v", got)
	}

	dana := agentpod.NewClient(serveCoreUDS(t, srv), "dana")
	list, err := dana.IntegrationTools(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, tl := range list.Tools {
		names = append(names, tl.Name)
	}
	if strings.Join(names, ",") != "notes_echo,notes_list_colors,notes_whoami" {
		t.Errorf("dana's actions = %v", names)
	}
	res := callAs(t, srv, "dana", "notes_echo", `{"text":"hi"}`)
	if res.IsError || len(res.Content) != 1 || !strings.Contains(res.Content[0], `"text":"hi"`) || !strings.Contains(res.Content[0], `"agent":"dana"`) {
		t.Errorf("dana's call = %+v", res)
	}
	rows, _ := srv.Store.ReadIntegrationAuditTail("notes", 10)
	if len(rows) == 0 || rows[0].Agent != "dana" || rows[0].Action != "echo" || !rows[0].Allowed {
		t.Errorf("audit = %+v", rows)
	}
}

// Why this test exists: a shipped bundle is built in, whatever runs
// it. A removal, an upload or a publish under its name, or an approve,
// a reject or a roll back of one of its versions that went through
// would make the data directory disagree with the files the binary
// carries; each must be refused in a built-in's words. And its install
// screen must show what a bundle's shows (its runtime, the hosts it
// reaches and its size), since the owner never approves it and that
// screen is the only place to read what it reaches, while a built-in
// of Go code shows none of these.
func TestShippedBundleRefusalsAndInstallScreen(t *testing.T) {
	srv := newTestServer(t)
	withIntegrationRegistry(srv, nil)
	installShipped(t, srv, shippedNotes(t, "1.0.0", integrationtest.MainPy))
	installShipped(t, srv, shippedNotes(t, "1.1.0", integrationtest.MainPy))

	if e := assertAPIError(t, intDelete(t, srv, intBase+"/notes"), http.StatusConflict); e.Error != builtinRefusal("Test notes", builtinRemove) {
		t.Errorf("remove = %+v", e)
	}
	if e := assertAPIError(t, intUpload(t, srv, intBundle(t, "notes", "2.0.0")), http.StatusConflict); e.Error != builtinRefusal("Test notes", builtinUpload) {
		t.Errorf("upload = %+v", e)
	}
	for path, move := range map[string]builtinMove{
		"/notes/versions/1.0.0/rollback": builtinRollBack,
		"/notes/versions/1.0.0/approve":  builtinApprove,
		"/notes/versions/1.1.0/reject":   builtinReject,
	} {
		if e := assertAPIError(t, intPost(t, srv, intBase+path), http.StatusConflict); e.Error != builtinRefusal("Test notes", move) {
			t.Errorf("%s = %+v", path, e)
		}
	}
	m, err := integration.ParseManifest(integrationtest.ManifestJSON(t, integrationtest.Manifest("notes", "3.0.0")))
	if err != nil {
		t.Fatal(err)
	}
	if why := srv.publishRefusal(m); !strings.Contains(why, "ships with Kivali") {
		t.Errorf("publish refusal = %q", why)
	}

	v := orgOK[apitypes.IntegrationVersion](t, orgGet(t, srv, intBase+"/notes/versions/1.1.0"))
	if !v.Builtin || !v.Shipped || !v.Current || v.Runtime != "python" || strings.Join(v.Hosts, ",") != "api.example.com" ||
		v.BundleBytes == 0 || v.FileCount != 2 || v.PublishedBy.Name != "Kivali" || v.NotesMD != store.BuiltinIntegrationNotes {
		t.Errorf("install screen = %+v", v)
	}
	page := orgOK[apitypes.IntegrationPage](t, orgGet(t, srv, intBase+"/notes"))
	if !page.Summary.Builtin || page.CanRemove || page.Summary.Version != "1.1.0" || page.Summary.PendingVersion != nil {
		t.Errorf("page = %+v, can remove %v", page.Summary, page.CanRemove)
	}

	// A built-in of Go code and an upload are not shipped bundles.
	srv.BuiltinIntegrations = builtinintegrations.Registry{echotest.New()}
	if err := srv.Store.InstallBuiltinIntegrations(srv.BuiltinIntegrations.Manifests(), shippedAt); err != nil {
		t.Fatal(err)
	}
	if v := orgOK[apitypes.IntegrationVersion](t, orgGet(t, srv, intBase+"/echo/versions/1.0.0")); !v.Builtin || v.Shipped {
		t.Errorf("a built-in of Go code's install screen = %+v", v)
	}
	intInstalled(t, srv, "crm", "1.0.0")
	if v := orgOK[apitypes.IntegrationVersion](t, orgGet(t, srv, intBase+"/crm/versions/1.0.0")); v.Builtin || v.Shipped {
		t.Errorf("an upload's install screen = %+v", v)
	}
}

// Why this test exists: a shipped bundle's record outlives the binary
// that carried it (a Kivali that dropped it, a backup restored from
// one that had it). Nothing may run it then: reconcile must stop its
// pod, its page must say it is not in this Kivali rather than that
// Kivali is starting it, Test connection and its options must say
// why they cannot run, agents' calls must be refused, and the owner
// must be able to remove it, or it would stay on the team for good.
func TestShippedBundleNotInThisKivali(t *testing.T) {
	srv := newTestServer(t)
	prov := newFakeIntegrationProv()
	reg, _ := withIntegrationRegistry(srv, prov)
	installShipped(t, srv, shippedNotes(t, "1.0.0", integrationtest.MainPy))
	intTeam(t, srv, "dana")
	connectNotes(t, srv, "s3cret-1")
	putIntegrationAccess(t, srv, "notes", "dana", `{"blogs":["red"]}`)
	if err := reg.Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	prov.takeCalls()

	srv.ShippedBundles = builtinintegrations.ShippedBundles{}
	if err := reg.Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	if calls := prov.takeCalls(); strings.Join(calls, ",") != "stop notes" {
		t.Errorf("pods = %v", calls)
	}
	page := orgOK[apitypes.IntegrationPage](t, orgGet(t, srv, intBase+"/notes"))
	if page.Summary.Status != apitypes.IntegrationStatusNotIncluded || !page.Summary.Builtin || !page.CanRemove {
		t.Errorf("page = %+v, can remove %v", page.Summary, page.CanRemove)
	}
	why := builtinRefusal("Test notes", builtinNotIncluded)
	if e := assertAPIError(t, intPost(t, srv, intBase+"/notes/connection/verify"), http.StatusConflict); e.Error != why {
		t.Errorf("verify = %+v", e)
	}
	if e := assertAPIError(t, orgGet(t, srv, intBase+"/notes/choices/blogs"), http.StatusConflict); e.Error != why {
		t.Errorf("choices = %+v", e)
	}
	if res := callAs(t, srv, "dana", "notes_echo", `{}`); !res.IsError || len(res.Content) != 1 || res.Content[0] != refuseNotRunning("Test notes") {
		t.Errorf("a call = %+v", res)
	}
	// What the stored version is does not change with the binary.
	if v := orgOK[apitypes.IntegrationVersion](t, orgGet(t, srv, intBase+"/notes/versions/1.0.0")); !v.Builtin || !v.Shipped {
		t.Errorf("install screen = %+v", v)
	}
	if rr := intDelete(t, srv, intBase+"/notes"); rr.Code != http.StatusNoContent {
		t.Fatalf("remove: %d %s", rr.Code, rr.Body.String())
	}
	if _, err := srv.Store.GetIntegration("notes"); err == nil {
		t.Errorf("the record is still there")
	}
}

// Why this test exists: a backup holds a shipped bundle's record,
// versions and files as the binary that wrote it had them, or none
// from a Kivali before it shipped. The restore must write this
// binary's version and files over them, keeping the owner's switch, and
// a pod must not go on with files the restore replaced under the same
// version; otherwise the restored team runs code other than the code
// this Kivali carries until the next restart.
func TestRestoreInstallsShippedBundles(t *testing.T) {
	src := newTestServer(t)
	installShipped(t, src, shippedNotes(t, "1.0.0", "print('one')\n"))
	if rr := intPost(t, src, intBase+"/notes/disable"); rr.Code != http.StatusOK {
		t.Fatalf("disable: %d", rr.Code)
	}
	zipBytes := backupZip(t, src).Body.Bytes()

	dst := newTestServer(t)
	dst.ShippedBundles = builtinintegrations.ShippedBundles{{Name: "notes", FS: shippedNotes(t, "1.0.0", "print('two')\n")}}
	if rr := restoreZip(t, dst, "backup.zip", zipBytes); rr.Code != http.StatusOK {
		t.Fatalf("restore: %d %s", rr.Code, rr.Body.String())
	}
	it, err := dst.Store.GetIntegration("notes")
	if err != nil {
		t.Fatal(err)
	}
	if !it.Builtin || it.Enabled || it.CurrentVersion != "1.0.0" {
		t.Errorf("after the restore = %+v", it)
	}
	dir, _ := dst.Store.IntegrationBundleDir("notes", "1.0.0")
	if b, err := os.ReadFile(filepath.Join(dir, "server", "main.py")); err != nil || string(b) != "print('two')\n" {
		t.Errorf("restored server = %q, %v", b, err)
	}
	if e, ok := entryOf(dst.Integrations, "notes"); !ok || !e.replace {
		t.Errorf("the pod is not renewed: %+v, %v", e, ok)
	}

	// From a Kivali that shipped none, the restore installs it.
	empty := backupZip(t, newTestServer(t)).Body.Bytes()
	dst2 := newTestServer(t)
	dst2.ShippedBundles = builtinintegrations.ShippedBundles{{Name: "notes", FS: shippedNotes(t, "1.1.0", integrationtest.MainPy)}}
	if rr := restoreZip(t, dst2, "backup.zip", empty); rr.Code != http.StatusOK {
		t.Fatalf("restore: %d %s", rr.Code, rr.Body.String())
	}
	if it, err := dst2.Store.GetIntegration("notes"); err != nil || !it.Builtin || !it.Enabled || it.CurrentVersion != "1.1.0" {
		t.Errorf("after a restore of a backup without it = %+v, %v", it, err)
	}
}

// Why this test exists: pods outlive core, and at boot core takes on a
// pod built as it would build it for the version in use. When boot
// wrote a shipped bundle's files again under the same version, that
// pod runs the files from before; Renew must make the next reconcile
// start a new pod with a new token instead of taking that one on, or
// a fix shipped under the same version would never run.
func TestRenewReplacesAPodBootWouldTakeOn(t *testing.T) {
	srv := newTestServer(t)
	prov := newFakeIntegrationProv()
	reg, _ := withIntegrationRegistry(srv, prov)
	installShipped(t, srv, shippedNotes(t, "1.0.0", integrationtest.MainPy))
	prov.pods["notes"] = fakeIntegrationPod{version: "1.0.0", token: "from-before"}

	reg.Renew("notes")
	if err := reg.reconcile(context.Background(), true); err != nil {
		t.Fatal(err)
	}
	e, _ := entryOf(reg, "notes")
	if e.token == "from-before" || e.token == "" || e.replace {
		t.Errorf("entry = %+v", e)
	}
	if pod := prov.pod("notes"); pod.token != e.token {
		t.Errorf("pod = %+v, entry token %q", pod, e.token)
	}
	if calls := prov.takeCalls(); strings.Join(calls, ",") != "start notes 1.0.0" {
		t.Errorf("pods = %v", calls)
	}

	// Without Renew, the pod from before is taken on.
	srv2 := newTestServer(t)
	prov2 := newFakeIntegrationProv()
	reg2, _ := withIntegrationRegistry(srv2, prov2)
	installShipped(t, srv2, shippedNotes(t, "1.0.0", integrationtest.MainPy))
	prov2.pods["notes"] = fakeIntegrationPod{version: "1.0.0", token: "from-before"}
	if err := reg2.reconcile(context.Background(), true); err != nil {
		t.Fatal(err)
	}
	if e, _ := entryOf(reg2, "notes"); e.token != "from-before" {
		t.Errorf("the pod from before was not taken on: %+v", e)
	}
}
