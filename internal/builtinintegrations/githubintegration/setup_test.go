package githubintegration

import (
	"context"
	"encoding/json"
	"net/url"
	"strings"
	"testing"

	"github.com/kivali-ai/kivali/internal/builtinintegrations"
	"github.com/kivali-ai/kivali/internal/integration"
)

// The callback core gives a call, with a state of the flow's.
const (
	teamBase     = "https://kivali.acme.example"
	testCallback = teamBase + "/connect/callback?state=st4te0001"
)

// stages records what Setup says it is at.
type stages []string

func (s *stages) Stage(words string, _, _ int) { *s = append(*s, words) }

// setupT is one call of Setup, failing the test on an error.
func (g *GitHub) setupT(t *testing.T, conn builtinintegrations.Connection, flow string, last builtinintegrations.Answer) builtinintegrations.Step {
	t.Helper()
	if last.Callback == "" {
		last.Callback = testCallback
	}
	step, err := g.Setup(context.Background(), conn, flow, last, &stages{})
	if err != nil {
		t.Fatalf("setup %s at %q: %v", flow, last.State["at"], err)
	}
	checkStep(t, step)
	return step
}

// checkStep holds a step to what core does with it: the step is held
// to the manifest's declarations, and core scrubs every kept value and
// every State value of six characters or more from what a step shows.
// A long State value would blank words out of the page, and a Wait
// page holding the slug kept in the flow would show a broken address.
func checkStep(t *testing.T, step builtinintegrations.Step) {
	t.Helper()
	m := New().Manifest()
	setup := m.Kivali.Connection.Setup
	for k, v := range step.State {
		if len(v) >= 6 {
			t.Errorf("State %s = %q: core would scrub it from every word", k, v)
		}
	}
	for k := range step.Keep {
		if !setup.Keeps.Has(k) {
			t.Errorf("keeps %s, which the manifest does not declare", k)
		}
	}
	for _, raw := range []string{goURL(step), waitURL(step)} {
		if raw == "" {
			continue
		}
		if u, err := url.Parse(raw); err != nil || u.Scheme != "https" || u.Host != "github.com" {
			t.Errorf("a page off github.com: %q", raw)
		}
	}
	if w := waitURL(step); strings.Contains(w, testSlug) {
		t.Errorf("a Wait page holding the slug: %q", w)
	}
	if f := step.Form; f != nil {
		for _, k := range f.Fields {
			if _, ok := setup.Fields.Field(k); !ok {
				t.Errorf("asks for %s, which the manifest does not declare", k)
			}
		}
	}
}

func goURL(s builtinintegrations.Step) string {
	if s.Go == nil {
		return ""
	}
	return s.Go.URL
}

func waitURL(s builtinintegrations.Step) string {
	if s.Wait == nil {
		return ""
	}
	return s.Wait.URL
}

// conversionAnswer is GitHub's answer to a manifest's code.
func conversionAnswer() string {
	b, _ := json.Marshal(map[string]any{
		"id": 1234, "slug": testSlug, "name": "Kivali Acme", "client_id": "Iv23liTest", "client_secret": testClientSecret,
		"webhook_secret": "whsec-not-kept-0000", "pem": pkcs1PEM(testKey()), "owner": map[string]any{"login": "acme"},
	})
	return string(b)
}

// madeConn is the connection after the conversion's Keep: the app, not
// yet installed.
func madeConn() builtinintegrations.Connection {
	c := testConn()
	delete(c.Fields, FieldInstallationID)
	return c
}

// Why this test exists: this is GitHub's whole install as the design
// draws it, two clicks on GitHub and nothing copied. The Form asks for
// the account alone; the Go posts the manifest (named for the team,
// private, no webhook, the permissions this version needs, GitHub sent
// back to the flow's callback and the setup address) to the personal
// account's new-app page with the flow's state; the code GitHub sends
// back is traded with no credential, and only the app's five values
// are kept (the webhook secret and the code never); the install Go is
// Stateless, as GitHub's setup address carries no state of Kivali's;
// and the installation kept is the one the app's own JWT lists, never
// the id the browser came back with. Each piece missing is a setup that
// cannot finish, or one a forged return can steer.
func TestSetupInstallsThroughTheManifestFlow(t *testing.T) {
	g, f := newTest(t)
	step := g.setupT(t, builtinintegrations.Connection{Fields: map[string]string{}}, builtinintegrations.FlowSetup, builtinintegrations.Answer{Leg: 1})
	if step.Form == nil || len(step.Form.Fields) != 1 || step.Form.Fields[0] != "account" || len(step.Keep) != 0 {
		t.Fatalf("first step = %+v", step)
	}

	step = g.setupT(t, builtinintegrations.Connection{Fields: map[string]string{}}, builtinintegrations.FlowSetup,
		builtinintegrations.Answer{State: step.State, Form: map[string]string{"account": ""}, Team: "Acme Corp", Leg: 2})
	if step.Go == nil || step.Go.URL != "https://github.com/settings/apps/new?state=st4te0001" || step.Go.NoReturn || step.Go.Stateless || len(step.Keep) != 0 ||
		!strings.Contains(step.Go.Words, "GitHub may ask you to change the name") || strings.Contains(step.Go.Words, "https address") {
		t.Fatalf("create = %+v", step.Go)
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(step.Go.Post.Get("manifest")), &m); err != nil || len(step.Go.Post) != 1 {
		t.Fatalf("manifest %q: %v", step.Go.Post, err)
	}
	want := map[string]any{
		"name": "Kivali Acme Corp", "url": teamBase, "redirect_url": teamBase + "/connect/callback", "setup_url": teamBase + "/connect/callback",
		"setup_on_update": true, "public": false, "hook_attributes": map[string]any{"active": false},
		"default_permissions": map[string]any{"metadata": "read", "contents": "read", "issues": "write", "pull_requests": "write"},
		"default_events":      []any{},
	}
	if gotJSON, wantJSON := mustJSON(t, m), mustJSON(t, want); gotJSON != wantJSON {
		t.Errorf("manifest = %s\nwant %s", gotJSON, wantJSON)
	}
	if len(f.requests()) != 0 {
		t.Errorf("GitHub was called before the app was made: %+v", f.requests())
	}

	f.route("POST /app-manifests/c0de-1/conversions", 201, conversionAnswer())
	pr := &stages{}
	back, err := g.Setup(context.Background(), builtinintegrations.Connection{Fields: map[string]string{}}, builtinintegrations.FlowSetup,
		builtinintegrations.Answer{State: step.State, Return: url.Values{"code": {"c0de-1"}, "state": {"st4te0001"}}, Callback: teamBase + "/connect/callback?state=st4te0002", Leg: 3}, pr)
	if err != nil {
		t.Fatal(err)
	}
	checkStep(t, back)
	if len(*pr) != 1 || (*pr)[0] != "Creating the app on GitHub" {
		t.Errorf("stages = %q", *pr)
	}
	reqs := f.requests()
	if len(reqs) != 1 || reqs[0].Method != "POST" || reqs[0].Header.Get("Authorization") != "" {
		t.Errorf("conversion = %+v", reqs)
	}
	wantKeep := map[string]string{FieldAppID: "1234", FieldSlug: testSlug, FieldClientID: "Iv23liTest", FieldClientSecret: testClientSecret, FieldPrivateKey: pkcs1PEM(testKey())}
	if mustJSON(t, back.Keep) != mustJSON(t, wantKeep) {
		t.Errorf("kept %v", back.Keep)
	}
	for k, v := range back.State {
		if strings.Contains(v, "c0de-1") || strings.Contains(k+v, "whsec") {
			t.Errorf("State carries %s=%s", k, v)
		}
	}
	if back.Go == nil || back.Go.URL != "https://github.com/apps/kivali-acme/installations/new?state=st4te0002" || !back.Go.Stateless || back.Go.NoReturn {
		t.Fatalf("install = %+v", back.Go)
	}

	// The browser comes back with an installation id anyone could have
	// put there; the one the app's JWT lists is kept.
	f.route("GET /app/installations", 200, `[{"id":42,"account":{"login":"acme"}}]`)
	pr = &stages{}
	done, err := g.Setup(context.Background(), madeConn(), builtinintegrations.FlowSetup,
		builtinintegrations.Answer{State: back.State, Return: url.Values{"installation_id": {"999"}, "setup_action": {"install"}}, Callback: testCallback, Leg: 4}, pr)
	if err != nil {
		t.Fatal(err)
	}
	if !done.Done || len(done.Keep) != 1 || done.Keep[FieldInstallationID] != "42" {
		t.Fatalf("done = %+v", done)
	}
	if len(*pr) != 1 || (*pr)[0] != "Waiting for the installation" {
		t.Errorf("stages = %q", *pr)
	}
	if last := f.requests()[len(f.requests())-1]; last.Path != "/app/installations" || last.JWT == nil || last.JWT.ISS != testAppID {
		t.Errorf("listing = %+v", last)
	}
}

func mustJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// Why this test exists: an organization's app is made on the
// organization's own new-app page, or GitHub makes it on the owner's
// personal account; a name that is not an account's is asked for again
// rather than put into GitHub's address. The app's name is the team's
// with Kivali before it, cut to the 34 characters GitHub takes, and
// "Kivali" for a team with no name. A team on plain http is told GitHub
// may refuse its address, and what to do, since GitHub says so on its
// own page and the browser never comes back.
func TestSetupCreatesOnTheAccountNamed(t *testing.T) {
	g, _ := newTest(t)
	at := map[string]string{"at": "form"}
	step := g.setupT(t, builtinintegrations.Connection{}, builtinintegrations.FlowSetup, builtinintegrations.Answer{State: at, Form: map[string]string{"account": " @acme-org "}})
	if step.Go == nil || step.Go.URL != "https://github.com/organizations/acme-org/settings/apps/new?state=st4te0001" {
		t.Fatalf("org = %+v", step.Go)
	}
	for _, bad := range []string{"acme corp", "-acme", "acme/../x", strings.Repeat("a", 40)} {
		step = g.setupT(t, builtinintegrations.Connection{}, builtinintegrations.FlowSetup, builtinintegrations.Answer{State: at, Form: map[string]string{"account": bad}})
		if step.Form == nil || !strings.HasPrefix(step.Form.Words, "That is not the name of a GitHub account") {
			t.Errorf("%q = %+v", bad, step)
		}
	}
	for team, want := range map[string]string{"": "Kivali", "  Acme   Corp ": "Kivali Acme Corp", strings.Repeat("x", 40): "Kivali " + strings.Repeat("x", 27)} {
		if got := appName(team); got != want {
			t.Errorf("appName(%q) = %q, want %q", team, got, want)
		}
	}
	step = g.setupT(t, builtinintegrations.Connection{}, builtinintegrations.FlowSetup,
		builtinintegrations.Answer{State: at, Form: map[string]string{"account": ""}, Callback: "http://127.0.0.1:8080/connect/callback?state=st4te0001"})
	var m appManifest
	_ = json.Unmarshal([]byte(step.Go.Post.Get("manifest")), &m)
	if m.URL != "http://127.0.0.1:8080" || m.SetupURL != "http://127.0.0.1:8080/connect/callback" ||
		!strings.Contains(step.Go.Words, "give the team an https address under Other devices in Kivali Desktop") {
		t.Errorf("http team: %+v %q", m, step.Go.Words)
	}
	if _, err := g.Setup(context.Background(), builtinintegrations.Connection{}, builtinintegrations.FlowSetup,
		builtinintegrations.Answer{State: at, Form: map[string]string{"account": ""}, Callback: ""}, &stages{}); err == nil {
		t.Error("a flow with no callback went to GitHub")
	}
}

// Why this test exists: a failure before GitHub made the app must keep
// nothing and say what GitHub said, so the owner can start again: no
// code, a code GitHub no longer takes (one use, within the hour), and
// an answer missing what a call needs each fail with words, and none
// keeps a value.
func TestSetupConversionFailures(t *testing.T) {
	g, f := newTest(t)
	at := map[string]string{"at": "make"}
	call := func(ret url.Values) (builtinintegrations.Step, error) {
		return g.Setup(context.Background(), builtinintegrations.Connection{}, builtinintegrations.FlowSetup, builtinintegrations.Answer{State: at, Return: ret, Callback: testCallback}, &stages{})
	}
	if step, err := call(url.Values{"state": {"x"}}); err == nil || len(step.Keep) != 0 || err.Error() != "GitHub sent the browser back without the app's code; start again" {
		t.Errorf("no code: %+v %v", step, err)
	}
	if _, err := call(url.Values{"code": {"../app"}}); err == nil || len(f.requests()) != 0 {
		t.Errorf("a code with a path in it reached GitHub: %v", err)
	}
	f.route("POST /app-manifests/old/conversions", 404, `{"message":"Not Found"}`)
	if step, err := call(url.Values{"code": {"old"}}); err == nil || len(step.Keep) != 0 ||
		err.Error() != "GitHub did not take the app's code (HTTP 404): Not Found. A code works once, within an hour; start again" {
		t.Errorf("old code: %v", err)
	}
	f.route("POST /app-manifests/c0de/conversions", 422, `{"message":"Validation Failed","errors":[{"message":"Name is already taken"}]}`)
	if _, err := call(url.Values{"code": {"c0de"}}); err == nil || err.Error() != "GitHub did not make the app (HTTP 422): Validation Failed: Name is already taken. Start again" {
		t.Errorf("refused: %v", err)
	}
	f.route("POST /app-manifests/c0de/conversions", 201, `{"id":1234,"slug":"kivali-acme","client_id":"Iv1","client_secret":"s3cret"}`)
	if step, err := call(url.Values{"code": {"c0de"}}); err == nil || len(step.Keep) != 0 || !strings.Contains(err.Error(), "its private key") {
		t.Errorf("no key: %+v %v", step, err)
	}
}

// Why this test exists: GitHub may list no installation yet when the
// browser comes back (the owner left its page, or an organization's
// owners approve it later), list more than one, or fail for a moment.
// None may finish setup on a guess: no installation sends the owner to
// install it again, an approval to come is waited for and checked, a
// passing failure keeps waiting, the id the browser brought only picks
// among the app's own installations, and a key GitHub refuses ends the
// flow in its words.
func TestSetupWaitsForTheInstallation(t *testing.T) {
	g, f := newTest(t)
	inst := map[string]string{"at": "inst"}
	f.route("GET /app/installations", 200, `[]`)
	step := g.setupT(t, madeConn(), builtinintegrations.FlowSetup, builtinintegrations.Answer{State: inst, Return: url.Values{"installation_id": {"42"}}})
	if step.Go == nil || !step.Go.Stateless || !strings.HasPrefix(step.Go.URL, "https://github.com/apps/kivali-acme/installations/new") || len(step.Keep) != 0 {
		t.Fatalf("not installed = %+v", step)
	}
	step = g.setupT(t, madeConn(), builtinintegrations.FlowSetup, builtinintegrations.Answer{State: inst, Return: url.Values{"setup_action": {"request"}}})
	if step.Wait == nil || step.Wait.URL != "" || !strings.Contains(step.Wait.Words, "An organization's owner may have to approve it") {
		t.Fatalf("awaiting approval = %+v", step)
	}
	wait := step.State
	if again := g.setupT(t, madeConn(), builtinintegrations.FlowSetup, builtinintegrations.Answer{State: wait, Checked: true}); again.Wait == nil || *again.Wait != *step.Wait {
		t.Errorf("a check finding none = %+v", again)
	}
	f.route("GET /app/installations", 502, `{"message":"Bad Gateway"}`)
	if again := g.setupT(t, madeConn(), builtinintegrations.FlowSetup, builtinintegrations.Answer{State: wait, Checked: true}); again.Wait == nil {
		t.Errorf("a passing failure = %+v", again)
	}
	f.route("GET /app/installations", 200, `[{"id":42,"account":{"login":"acme"}}]`)
	if done := g.setupT(t, madeConn(), builtinintegrations.FlowSetup, builtinintegrations.Answer{State: wait, Checked: true}); !done.Done || done.Keep[FieldInstallationID] != "42" {
		t.Errorf("a check finding it = %+v", done)
	}
	f.route("GET /app/installations", 200, `[{"id":42,"account":{"login":"acme"}},{"id":43,"account":{"login":"dana"}}]`)
	if done := g.setupT(t, madeConn(), builtinintegrations.FlowSetup, builtinintegrations.Answer{State: inst, Return: url.Values{"installation_id": {"43"}}}); !done.Done || done.Keep[FieldInstallationID] != "43" {
		t.Errorf("picked = %+v", done)
	}
	if _, err := g.Setup(context.Background(), madeConn(), builtinintegrations.FlowSetup, builtinintegrations.Answer{State: inst, Return: url.Values{"installation_id": {"999"}}}, &stages{}); err == nil {
		t.Error("an id the app does not list picked an installation")
	}
	f.route("GET /app/installations", 401, `{"message":"A JSON web token could not be decoded"}`)
	if _, err := g.Setup(context.Background(), madeConn(), builtinintegrations.FlowSetup, builtinintegrations.Answer{State: wait, Checked: true}, &stages{}); err == nil ||
		!strings.HasPrefix(err.Error(), "GitHub did not list the app's installations: GitHub refused the GitHub App's key (HTTP 401)") {
		t.Errorf("refused key: %v", err)
	}
	if _, err := g.Setup(context.Background(), builtinintegrations.Connection{Fields: map[string]string{}}, builtinintegrations.FlowSetup, builtinintegrations.Answer{State: inst}, &stages{}); err == nil {
		t.Error("a connection with no app listed installations")
	}
}

// Why this test exists: the installed requirement's Fix (install) is
// steps 4 and 5 alone: GitHub's new-installation page for the app setup
// made, then the installation the app's JWT lists kept in place of the
// one removed on GitHub.
func TestInstallFixInstallsAgain(t *testing.T) {
	g, f := newTest(t)
	step := g.setupT(t, testConn(), FlowInstall, builtinintegrations.Answer{Leg: 1})
	if step.Go == nil || !step.Go.Stateless || step.Go.URL != "https://github.com/apps/kivali-acme/installations/new?state=st4te0001" || len(step.Keep) != 0 {
		t.Fatalf("fix = %+v", step)
	}
	f.route("GET /app/installations", 200, `[{"id":77,"account":{"login":"acme"}}]`)
	if done := g.setupT(t, testConn(), FlowInstall, builtinintegrations.Answer{State: step.State, Return: url.Values{}}); !done.Done || done.Keep[FieldInstallationID] != "77" {
		t.Errorf("done = %+v", done)
	}
	if _, err := g.Setup(context.Background(), builtinintegrations.Connection{Fields: map[string]string{}}, FlowInstall, builtinintegrations.Answer{}, &stages{}); err == nil {
		t.Error("a fix with no app went to GitHub")
	}
	if _, err := g.Setup(context.Background(), testConn(), "rename", builtinintegrations.Answer{}, &stages{}); err == nil {
		t.Error("a flow GitHub does not have ran")
	}
}

// installation is GET /app/installations/42's answer.
func installation(selection, perms string, suspended bool) string {
	s := `null`
	if suspended {
		s = `"2026-10-01T00:00:00Z"`
	}
	return `{"id":42,"account":{"login":"acme","type":"Organization"},"repository_selection":"` + selection + `","permissions":` + perms + `,"suspended_at":` + s + `}`
}

const (
	allPerms  = `{"metadata":"read","contents":"read","issues":"write","pull_requests":"write"}`
	readPerms = `{"metadata":"read","contents":"read","issues":"read","pull_requests":"read"}`
	appAnswer = `{"id":1234,"slug":"kivali-acme","name":"Kivali Acme","owner":{"login":"acme","type":"Organization"},"permissions":` + allPerms + `}`
)

// requirementsT is Requirements, failing on an error.
func (g *GitHub) requirementsT(t *testing.T, conn builtinintegrations.Connection) []builtinintegrations.Requirement {
	t.Helper()
	rows, err := g.Requirements(context.Background(), conn)
	if err != nil {
		t.Fatalf("requirements: %v", err)
	}
	for _, r := range rows {
		// Core scrubs the connection's kept values from the words, so a
		// row naming one would read "[secret]".
		for _, kept := range []string{testSlug, testClientSecret, "Iv23liTest"} {
			if strings.Contains(r.Words+r.Title, kept) {
				t.Errorf("%s names %q: %q", r.ID, kept, r.Words)
			}
		}
	}
	return rows
}

func rowsOf(rows []builtinintegrations.Requirement) string {
	var out []string
	for _, r := range rows {
		met := "unmet"
		if r.Met {
			met = "met"
		}
		out = append(out, r.ID+" "+met+" "+r.Fix)
	}
	return strings.Join(out, "|")
}

// Why this test exists: the requirements are an installed GitHub's
// health, and each unmet one is a Needs-you item whose Fix must be the
// flow that mends it: a key GitHub refuses (or values Kivali lacks) is
// set up again, a removed installation installs again, a suspended
// installation or one reaching no repository goes to its settings, and
// missing permissions go to where they are granted. Every row must say
// what is in words that survive core's scrubbing of the kept values,
// and a GitHub failure that names none of them is an error, not a row
// claiming one.
func TestRequirements(t *testing.T) {
	g, f := newTest(t)
	f.route("GET /app", 200, appAnswer)
	f.route("GET /app/installations/42", 200, installation("selected", allPerms, false))
	f.route("GET /installation/repositories", 200, `{"total_count":3,"repositories":[]}`)
	rows := g.requirementsT(t, testConn())
	if got := rowsOf(rows); got != "key met |installed met |repositories met |permissions met " {
		t.Fatalf("rows = %s", got)
	}
	if rows[0].Title != "The app's key works" || rows[0].Words != "GitHub accepts the key of the app Kivali Acme. Uninstalling GitHub here leaves the app on GitHub, for you to delete under https://github.com/organizations/acme/settings/apps." ||
		rows[1].Words != "Installed on acme." || rows[2].Words != "Installed on acme, reaching 3 repositories." || rows[3].Title != "Permissions current" {
		t.Errorf("words = %+v", rows)
	}
	if r := f.requests(); r[0].Path != "/app" || r[0].JWT == nil || r[1].JWT == nil {
		t.Errorf("requests %+v", r)
	}
	f.route("GET /app/installations/42", 200, installation("all", allPerms, false))
	if rows := g.requirementsT(t, testConn()); rows[2].Words != "Installed on acme, reaching all repositories." {
		t.Errorf("all = %+v", rows[2])
	}
	f.route("GET /app/installations/42", 200, installation("selected", allPerms, false))
	f.route("GET /installation/repositories", 200, `{"total_count":0,"repositories":[]}`)
	if got := rowsOf(g.requirementsT(t, testConn())); got != "key met |installed met |repositories unmet settings|permissions met " {
		t.Errorf("no repositories = %s", got)
	}
	f.route("GET /app/installations/42", 200, installation("all", readPerms, true))
	rows = g.requirementsT(t, testConn())
	if got := rowsOf(rows); got != "key met |installed unmet settings|permissions unmet permissions" {
		t.Errorf("suspended, lacking = %s", got)
	}
	if rows[2].Words != "The installation lacks permissions this version needs: issues: write, pull requests: write. Approve them on GitHub." {
		t.Errorf("lacking = %q", rows[2].Words)
	}
	f.route("GET /app/installations/42", 404, `{"message":"Not Found"}`)
	if rows := g.requirementsT(t, testConn()); rowsOf(rows) != "key met |installed unmet install" || rows[1].Words != "The app is no longer installed on GitHub." {
		t.Errorf("removed = %+v", rows)
	}
	f.route("GET /app", 401, `{"message":"A JSON web token could not be decoded"}`)
	if rows := g.requirementsT(t, testConn()); rowsOf(rows) != "key unmet setup" || !strings.HasPrefix(rows[0].Words, "GitHub refused the app's key (HTTP 401)") {
		t.Errorf("refused key = %+v", rows)
	}
	if rows := g.requirementsT(t, madeConn()); rowsOf(rows) != "key unmet setup" {
		t.Errorf("incomplete = %+v", rows)
	}
	f.route("GET /app", 503, `{"message":"Service Unavailable"}`)
	if rows, err := g.Requirements(context.Background(), testConn()); err == nil || len(rows) != 0 {
		t.Errorf("GitHub failing = %+v %v", rows, err)
	}
}

// Why this test exists: the permissions Fix must send the owner where
// the missing permissions are granted. An app made by an earlier
// version does not ask for a permission a later one needs, so that is
// given in the app's own settings first; one it asks for is approved
// in the installation's settings. Then core waits, checking the
// installation, and the Fix is done once it grants every one; one
// already granted is done at once.
func TestPermissionsFix(t *testing.T) {
	g, f := newTest(t)
	f.route("GET /app", 200, `{"slug":"kivali-acme","owner":{"login":"dana","type":"User"},"permissions":`+readPerms+`}`)
	f.route("GET /app/installations/42", 200, installation("selected", readPerms, false))
	step := g.setupT(t, testConn(), FlowPermissions, builtinintegrations.Answer{Leg: 1})
	if step.Go == nil || !step.Go.NoReturn || step.Go.URL != "https://github.com/settings/apps/kivali-acme/permissions" ||
		step.Go.Words != "On GitHub, give the app these permissions and save: issues: write, pull requests: write. Then approve them for its installation, where GitHub asks." {
		t.Fatalf("app lacks = %+v", step.Go)
	}
	f.route("GET /app", 200, appAnswer)
	step = g.setupT(t, testConn(), FlowPermissions, builtinintegrations.Answer{Leg: 1})
	if step.Go == nil || step.Go.URL != "https://github.com/organizations/acme/settings/installations/42" {
		t.Fatalf("installation lacks = %+v", step.Go)
	}
	wait := g.setupT(t, testConn(), FlowPermissions, builtinintegrations.Answer{State: step.State, Went: true})
	if wait.Wait == nil || wait.Wait.URL != "https://github.com/organizations/acme/settings/installations/42" {
		t.Fatalf("wait = %+v", wait)
	}
	f.route("GET /app/installations/42", 200, installation("selected", allPerms, false))
	if done := g.setupT(t, testConn(), FlowPermissions, builtinintegrations.Answer{State: wait.State, Checked: true}); !done.Done || len(done.Keep) != 0 {
		t.Errorf("granted = %+v", done)
	}
	if done := g.setupT(t, testConn(), FlowPermissions, builtinintegrations.Answer{Leg: 1}); !done.Done {
		t.Errorf("already granted = %+v", done)
	}
	// Its checks, every few seconds, never count repositories: that is a
	// token and a request the permissions do not need.
	for _, r := range f.requests() {
		if r.Path == "/installation/repositories" || strings.HasSuffix(r.Path, "/access_tokens") {
			t.Errorf("the permissions Fix asked %s", r.Path)
		}
	}
}

// Why this test exists: the settings Fix mends a suspended installation
// and one reaching no repository, both in the installation's settings
// on GitHub, and is done once GitHub shows it mended.
func TestSettingsFix(t *testing.T) {
	g, f := newTest(t)
	f.route("GET /app/installations/42", 200, installation("all", allPerms, true))
	step := g.setupT(t, testConn(), FlowSettings, builtinintegrations.Answer{Leg: 1})
	if step.Go == nil || !step.Go.NoReturn || step.Go.URL != "https://github.com/organizations/acme/settings/installations/42" {
		t.Fatalf("suspended = %+v", step.Go)
	}
	f.route("GET /app/installations/42", 200, installation("selected", allPerms, false))
	f.route("GET /installation/repositories", 200, `{"total_count":0}`)
	if wait := g.setupT(t, testConn(), FlowSettings, builtinintegrations.Answer{State: step.State, Went: true}); wait.Wait == nil {
		t.Fatalf("no repositories yet = %+v", wait)
	}
	f.route("GET /installation/repositories", 200, `{"total_count":2}`)
	if done := g.setupT(t, testConn(), FlowSettings, builtinintegrations.Answer{State: map[string]string{"at": "wait"}, Checked: true}); !done.Done {
		t.Errorf("mended = %+v", done)
	}
}

// Why this test exists: Uninstall removes the installation on GitHub
// with the app's JWT before core forgets the key. Core may call it
// again after it succeeded, or with values missing, so an installation
// already gone, a key GitHub no longer takes (the app deleted there)
// and a connection that cannot name one are done; any other failure is
// an error that leaves everything as it is. A 401 over the JWT's times
// is this server's clock, not a deleted app: taking it as done would
// forget the key while the installation stays on GitHub.
func TestUninstall(t *testing.T) {
	g, f := newTest(t)
	f.route("DELETE /app/installations/42", 204, ``)
	if err := g.Uninstall(context.Background(), testConn()); err != nil {
		t.Fatal(err)
	}
	if r := f.requests(); len(r) != 1 || r[0].Method != "DELETE" || r[0].JWT == nil || r[0].JWT.ISS != testAppID {
		t.Errorf("requests %+v", r)
	}
	for _, status := range []int{404, 401} {
		f.route("DELETE /app/installations/42", status, `{"message":"gone"}`)
		if err := g.Uninstall(context.Background(), testConn()); err != nil {
			t.Errorf("%d: %v", status, err)
		}
	}
	f.route("DELETE /app/installations/42", 401, `{"message":"'Expiration time' claim ('exp') is too far in the future"}`)
	if err := g.Uninstall(context.Background(), testConn()); err == nil || !strings.Contains(err.Error(), "Check the clock of the computer Kivali runs on") ||
		!strings.Contains(err.Error(), "too far in the future") {
		t.Errorf("a 401 over the JWT's times = %v", err)
	}
	f.route("DELETE /app/installations/42", 500, `{"message":"Server Error"}`)
	if err := g.Uninstall(context.Background(), testConn()); err == nil || err.Error() != "GitHub did not remove the app's installation: GitHub answered 500: Server Error" {
		t.Errorf("failing = %v", err)
	}
	n := len(f.requests())
	if err := g.Uninstall(context.Background(), madeConn()); err != nil || len(f.requests()) != n {
		t.Errorf("incomplete: %v, %d requests", err, len(f.requests())-n)
	}
}

// Why this test exists: an earlier version's connection held a sign-in
// with the owner's own app; once setup replaces it, Kivali revokes the
// app's grant so its tokens stop working. GitHub takes a revocation
// only with a live access token, so an expired one is renewed with the
// refresh token first. With no client to revoke as, nothing is sent,
// and the app's secret never reaches words.
func TestRevokeSignIn(t *testing.T) {
	g, f := newTest(t)
	g.TokenURL = f.srv.URL + "/login/oauth/access_token"
	old := builtinintegrations.SignIn{ClientID: "Iv1old", ClientSecret: "old-secret-123", AccessToken: "ghu_live", RefreshToken: "ghr_refresh"}
	f.route("DELETE /applications/Iv1old/grant", 204, ``)
	if err := g.RevokeSignIn(context.Background(), old); err != nil {
		t.Fatal(err)
	}
	r := f.requests()
	if len(r) != 1 || r[0].Header.Get("Authorization") == "" || !strings.HasPrefix(r[0].Header.Get("Authorization"), "Basic ") || r[0].Body != `{"access_token":"ghu_live"}` {
		t.Fatalf("revoke = %+v", r)
	}

	// An expired token: renewed, then revoked.
	f.routeFunc("DELETE /applications/Iv1old/grant", func(rec recorded) reply {
		if strings.Contains(rec.Body, "ghu_fresh") {
			return reply{Status: 204}
		}
		return reply{Status: 404, Body: `{"message":"Not Found"}`}
	})
	f.route("POST /login/oauth/access_token", 200, `{"access_token":"ghu_fresh","refresh_token":"ghr_next"}`)
	if err := g.RevokeSignIn(context.Background(), old); err != nil {
		t.Fatal(err)
	}
	r = f.requests()
	if len(r) != 4 || r[2].Path != "/login/oauth/access_token" || !strings.Contains(r[2].Body, "grant_type=refresh_token") || !strings.Contains(r[3].Body, "ghu_fresh") {
		t.Errorf("renewed = %+v", r[1:])
	}
	// A refresh token GitHub no longer takes leaves nothing to revoke.
	f.route("POST /login/oauth/access_token", 200, `{"error":"bad_refresh_token"}`)
	if err := g.RevokeSignIn(context.Background(), old); err != nil {
		t.Errorf("dead refresh: %v", err)
	}
	f.route("DELETE /applications/Iv1old/grant", 500, `{"message":"old-secret-123 broke"}`)
	if err := g.RevokeSignIn(context.Background(), old); err == nil || strings.Contains(err.Error(), "old-secret-123") {
		t.Errorf("failing = %v", err)
	}
	n := len(f.requests())
	if err := g.RevokeSignIn(context.Background(), builtinintegrations.SignIn{AccessToken: "ghu_live"}); err != nil || len(f.requests()) != n {
		t.Errorf("no client: %v", err)
	}
}

// Why this test exists: the manifest is the install screen's promise,
// and setup is held to it: the account field says a blank one is the
// personal account, and the summary says what Uninstall leaves on
// GitHub, since the page has nowhere else to say it.
func TestSetupDeclarations(t *testing.T) {
	s := New().Manifest().Kivali.Connection.Setup
	if f, _ := s.Fields.Field("account"); !strings.Contains(f.Words, "blank for your personal account") || integration.SetupFieldTitle(f) != "Account" {
		t.Errorf("account = %+v", f)
	}
	if !strings.Contains(s.Summary, "the app stays on GitHub until you delete it") {
		t.Errorf("summary = %q", s.Summary)
	}
}
