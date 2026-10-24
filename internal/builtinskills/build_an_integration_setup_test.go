package builtinskills_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/kivali-ai/kivali/internal/integration"
)

// dealsStub is the imaginary Deals API template-setup's server calls,
// on this machine: a setup key traded once for a session, an app made,
// rescoped and installed with it, the app's token read back (or, with
// hidden, only shown on the app's page, the second look finding it
// shown), and the app's own token answering /v1/me with its scopes.
type dealsStub struct {
	url string

	mu          sync.Mutex
	hidden      bool
	looks       int
	used        map[string]bool
	scopes      []string
	uninstalled bool
}

const dealsToken = "app-token-0001"

func newDealsStub(t *testing.T) *dealsStub {
	t.Helper()
	d := &dealsStub{used: map[string]bool{}, scopes: []string{"deals.read"}}
	srv := httptest.NewServer(http.HandlerFunc(d.serve))
	t.Cleanup(srv.Close)
	d.url = srv.URL
	return d
}

func (d *dealsStub) set(edit func(d *dealsStub)) {
	d.mu.Lock()
	defer d.mu.Unlock()
	edit(d)
}

func (d *dealsStub) serve(w http.ResponseWriter, r *http.Request) {
	d.mu.Lock()
	defer d.mu.Unlock()
	reply := func(code int, v any) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(code)
		_ = json.NewEncoder(w).Encode(v)
	}
	var body struct {
		Key    string   `json:"key"`
		Scopes []string `json:"scopes"`
	}
	_ = json.NewDecoder(r.Body).Decode(&body)
	auth := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	session := strings.HasPrefix(auth, "session-")
	switch {
	case r.Method == http.MethodPost && r.URL.Path == "/v1/setup-keys/exchange":
		if body.Key == "" || d.used[body.Key] {
			reply(http.StatusUnauthorized, map[string]string{"error": "the key was used"})
			return
		}
		d.used[body.Key] = true
		reply(http.StatusOK, map[string]string{"access_token": "session-" + body.Key})
	case session && r.Method == http.MethodPost && r.URL.Path == "/v1/apps":
		d.scopes = body.Scopes
		reply(http.StatusCreated, map[string]string{"id": "app_1"})
	case session && r.Method == http.MethodPut && r.URL.Path == "/v1/apps/app_1/scopes":
		d.scopes = body.Scopes
		reply(http.StatusOK, map[string]any{})
	case session && r.Method == http.MethodPost && r.URL.Path == "/v1/apps/app_1/install":
		reply(http.StatusOK, map[string]any{})
	case session && r.Method == http.MethodGet && r.URL.Path == "/v1/apps/app_1/token":
		if d.hidden {
			reply(http.StatusNotFound, map[string]string{"error": "the token is on the app's page"})
			return
		}
		reply(http.StatusOK, map[string]string{"token": dealsToken})
	case session && r.Method == http.MethodGet && r.URL.Path == "/v1/apps/app_1":
		d.looks++
		reply(http.StatusOK, map[string]bool{"token_shown": d.looks >= 2})
	case auth == dealsToken && r.Method == http.MethodGet && r.URL.Path == "/v1/me":
		reply(http.StatusOK, map[string]any{"name": "Acme", "scopes": d.scopes})
	case auth == dealsToken && r.Method == http.MethodDelete && r.URL.Path == "/v1/apps/self":
		d.uninstalled = true
		w.WriteHeader(http.StatusNoContent)
	default:
		reply(http.StatusUnauthorized, map[string]string{"error": "no"})
	}
}

// Why this test exists: the skill gives the Kivali Developer
// template-setup as the bundle to start a setup flow from. One the
// check refuses, or whose server the smoke test cannot start, does not
// list its actions or does not answer kivali/requirements with no
// connection, hands each integration built from it a failure the
// developer did not write. The copy checked is the embedded one.
func TestBuildAnIntegrationSetupTemplateIsABundleTheCheckAccepts(t *testing.T) {
	dir := filepath.Join(writeSkill(t), "template-setup")
	b, err := integration.ReadDirectory(os.DirFS(dir))
	if err != nil {
		t.Fatal(err)
	}
	if f := integration.Validate(b.Manifest, b.Files); len(f) > 0 {
		t.Errorf("the check's findings on template-setup:\n%s", strings.Join(f.Texts(), "\n"))
	}
	if b.Manifest.Kivali.Connection.Method != integration.MethodSetup {
		t.Fatalf("template-setup's connection is %s", b.Manifest.Kivali.Connection.Method)
	}

	needPython(t)
	cmd := exec.Command("bash", "-c", integration.SmokeCommand(b.Manifest))
	cmd.Dir = dir
	var out, errOut bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errOut
	run := integration.SmokeRun{}
	if err := cmd.Run(); err != nil {
		var ee *exec.ExitError
		if !errors.As(err, &ee) {
			t.Fatal(err)
		}
		run.ExitCode = ee.ExitCode()
	}
	run.Stdout, run.Stderr = out.String(), errOut.String()
	f, listed := integration.SmokeFindings(b.Manifest, run)
	if len(f) > 0 {
		t.Errorf("the smoke test's findings on template-setup:\n%s", strings.Join(f.Texts(), "\n"))
	}
	if want := []string{"find_deals", "whoami"}; !slices.Equal(listed, want) {
		t.Errorf("template-setup's server listed %v, want %v", listed, want)
	}
}

// Why this test exists: try.py's setup mode is how the developer sees,
// before publishing, the flow the owner will take, and template-setup
// is the flow every setup bundle starts from. Each run here is one the
// owner meets: the key traded and the app built in one step with its
// stages as progress; what the step keeps restarting the server with
// it, as the pod does; the fallback where the token is shown only on
// the app's page, a Wait checked until it is shown and then a Form; the
// requirements, an unmet one's Fix run as its own flow; and uninstall.
// If try.py stopped calling as Kivali's runner does, or the template
// stopped finishing its flow, the developer would publish on a run
// Kivali does not reproduce.
func TestTryRunsTheSetupTemplatesFlow(t *testing.T) {
	needPython(t)
	dir := writeSkill(t)
	d := newDealsStub(t)
	api := "DEALS_API=" + d.url

	out, code := runTry(t, dir, "--env", api, "template-setup", "setup", "--answer", "setup_key=sk-test-0001")
	if code != 0 {
		t.Errorf("setup: exit %d", code)
	}
	wantIn(t, out,
		"A Form, in",
		"asks for: setup_key",
		"progress: Checking the setup key (1 of 4)",
		"progress: Reading the app's token (4 of 4)",
		"Kivali keeps app_id, token in the connection's Secret, and starts the server again with it, as the pod does.",
		"the connection is made, and the integration is installed.",
		"No problems.",
	)
	if strings.Contains(out, "sk-test-0001") || strings.Contains(out, dealsToken) {
		t.Errorf("a secret reached the output:\n%s", out)
	}

	d.set(func(d *dealsStub) { d.hidden = true })
	out, code = runTry(t, dir, "--env", api, "--every", "0", "template-setup", "setup", "--answer", "setup_key=sk-test-0002", "--answer", "token="+dealsToken)
	if code != 0 {
		t.Errorf("the token on the app's page: exit %d", code)
	}
	wantIn(t, out,
		"A Wait, in",
		"page: https://app.deals.example/apps/app_1",
		"Kivali keeps app_id in the connection's Secret",
		"The same Wait, in",
		"asks for: token",
		"Kivali keeps token in the connection's Secret",
		"the connection is made",
		"No problems.",
	)

	out, code = runTry(t, dir, "--env", api, "template-setup", "requirements")
	if code != 0 {
		t.Errorf("requirements with no connection: exit %d", code)
	}
	wantIn(t, out, "not met: App token: No app token is kept yet.", "Fix runs the flow setup")

	d.set(func(d *dealsStub) { d.scopes = nil })
	out, code = runTry(t, dir, "--env", api, "--field", "token="+dealsToken, "--field", "app_id=app_1",
		"template-setup", "requirements", "setup", "rescope", "requirements", "--answer", "setup_key=sk-test-0003")
	if code != 0 {
		t.Errorf("the rescope Fix: exit %d", code)
	}
	wantIn(t, out,
		"met: App token: The app's token works, in Acme.",
		"not met: Scopes: This version needs deals.read, which the app was not built with.",
		"Fix runs the flow rescope: try it with the request setup rescope",
		"setup rescope, as Kivali runs it:",
		"progress: Updating the app's scopes (2 of 3)",
		"the Fix is made.",
		"met: Scopes: The app has every scope this version needs.",
		"No problems.",
	)

	out, code = runTry(t, dir, "--env", api, "--field", "token="+dealsToken, "template-setup", "uninstall")
	if code != 0 {
		t.Errorf("uninstall: exit %d", code)
	}
	wantIn(t, out, "Done, in", "Kivali then forgets the connection's Secret and every agent's access.")
	d.mu.Lock()
	defer d.mu.Unlock()
	if !d.uninstalled {
		t.Error("the app was not removed")
	}
}

// scriptedSetupServer is a server whose kivali/setup answers are the
// results in TRY_STEPS, one per call (the last repeated), that writes
// each answer it was given to its error output, and whose
// kivali/requirements answer is TRY_ROWS.
const scriptedSetupServer = `import json, os, sys
steps = json.loads(os.environ["TRY_STEPS"])
n = 0
for line in sys.stdin:
    msg = json.loads(line)
    if "id" not in msg or "method" not in msg:
        continue
    m = msg["method"]
    if m == "initialize":
        res = {"protocolVersion": msg["params"]["protocolVersion"], "capabilities": {"tools": {}}, "serverInfo": {"name": "s", "version": "1"}}
    elif m == "tools/list":
        res = {"tools": [{"name": "find_deals", "inputSchema": {"type": "object"}}, {"name": "whoami", "inputSchema": {"type": "object"}}]}
    elif m == "kivali/setup":
        sys.stderr.write("answer: " + json.dumps(msg["params"]["answer"], sort_keys=True) + "\n")
        sys.stderr.flush()
        res = steps[min(n, len(steps) - 1)]
        n += 1
    elif m == "kivali/requirements":
        res = json.loads(os.environ.get("TRY_ROWS", '{"requirements": []}'))
    else:
        res = {}
    print(json.dumps({"jsonrpc": "2.0", "id": msg["id"], "result": res}), flush=True)
`

// Why this test exists: try.py holds each step to the manifest's
// declarations as Kivali's runner does, so a step Kivali would fail the
// flow on is a problem before publishing, not a failed install. A Go to
// a host the manifest does not declare, a key none of the step's
// shapes has, a key setup does not declare and a return with a state
// the flow never gave are each problems with exit 1; a Go that posts
// and comes back reaches the next call with its query and the state
// Kivali gave its callback, and one that does not come back is called
// again at once with went.
func TestTryHoldsASetupStepToTheManifest(t *testing.T) {
	needPython(t)
	try := func(t *testing.T, steps []map[string]any, args ...string) (string, int) {
		t.Helper()
		dir := writeSkill(t)
		if err := os.WriteFile(filepath.Join(dir, "template-setup", "server", "main.py"), []byte(scriptedSetupServer), 0o644); err != nil {
			t.Fatal(err)
		}
		b, err := json.Marshal(steps)
		if err != nil {
			t.Fatal(err)
		}
		return runTry(t, dir, append([]string{"--env", "TRY_STEPS=" + string(b), "template-setup", "setup"}, args...)...)
	}
	done := map[string]any{"step": map[string]any{"done": true}}
	for _, tc := range []struct {
		name string
		step map[string]any
		want string
	}{
		{"host", map[string]any{"go": map[string]any{"url": "https://evil.example/login"}}, "problem: Kivali fails the flow: Deals's setup tried to send your browser to evil.example, which its manifest does not declare"},
		{"plain http", map[string]any{"go": map[string]any{"url": "http://app.deals.example/new"}}, "problem: Kivali fails the flow: Deals's setup tried to send your browser to app.deals.example"},
		{"shape", map[string]any{"go": map[string]any{"url": "https://app.deals.example/new", "noreturn": true}}, `problem: Kivali fails the flow: Deals's setup answered with a step Kivali cannot read: step: unknown field "noreturn"`},
		{"keep", map[string]any{"done": true, "keep": map[string]any{"password": "hunter22"}}, "problem: Kivali fails the flow: Deals's setup tried to keep password, which its manifest does not declare"},
		{"field", map[string]any{"form": map[string]any{"fields": []any{"password"}}}, "problem: Kivali fails the flow: Deals's setup asked for password, which its manifest does not declare"},
		{"long address", map[string]any{"go": map[string]any{"url": "https://app.deals.example/" + strings.Repeat("a", 4096)}}, "problem: Kivali fails the flow: Deals's setup named an address longer than 4096 characters"},
		{"too much state", map[string]any{"done": true, "state": map[string]any{"blob": strings.Repeat("x", 64<<10)}}, "problem: Kivali fails the flow: Deals's setup gave more to keep and carry than Kivali holds for a step (64 KiB)"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out, code := try(t, []map[string]any{{"step": tc.step}})
			if code != 1 {
				t.Errorf("exit %d, want 1", code)
			}
			wantIn(t, out, tc.want)
		})
	}

	post := map[string]any{"step": map[string]any{"go": map[string]any{"words": "Approve it.", "url": "https://app.deals.example/new", "post": map[string]any{"manifest": "{}"}}}}
	out, code := try(t, []map[string]any{post, done}, "--return", "code=abc")
	if code != 0 {
		t.Errorf("a Go that posts and comes back: exit %d", code)
	}
	wantIn(t, out,
		"the browser posts a form to https://app.deals.example/new",
		"post manifest: {}",
		"the return has no state",
		`"return": {"code": ["abc"], "state": ["[secret]"]}`,
		"the connection is made",
	)

	out, code = try(t, []map[string]any{post, done}, "--return", "code=abc&state=not-the-flows")
	if code != 1 {
		t.Errorf("a return with another state: exit %d, want 1", code)
	}
	wantIn(t, out, "problem: the return's state is none the flow gave")

	away := map[string]any{"step": map[string]any{"go": map[string]any{"url": "https://app.deals.example/settings", "no_return": true}}}
	out, code = try(t, []map[string]any{away, done})
	if code != 0 {
		t.Errorf("a Go with no return: exit %d", code)
	}
	wantIn(t, out, "calls setup again at once", `"went": true`)
}

// Why this test exists: try.py reads requirement rows as Kivali keeps
// them, so a row Kivali leaves out or cannot read is a problem before
// publishing rather than a row the owner never sees. A row lacking its
// met is one Kivali cannot read at all; a fix that is not a flow's name
// leaves its row out; and past 20 rows Kivali shows none of the rest,
// so no integration floods Needs you.
func TestTryHoldsRequirementRowsToWhatKivaliKeeps(t *testing.T) {
	needPython(t)
	rows := func(t *testing.T, res map[string]any) (string, int) {
		t.Helper()
		dir := writeSkill(t)
		if err := os.WriteFile(filepath.Join(dir, "template-setup", "server", "main.py"), []byte(scriptedSetupServer), 0o644); err != nil {
			t.Fatal(err)
		}
		b, err := json.Marshal(res)
		if err != nil {
			t.Fatal(err)
		}
		return runTry(t, dir, "--env", "TRY_STEPS=[]", "--env", "TRY_ROWS="+string(b), "template-setup", "requirements")
	}
	row := func(id string) map[string]any {
		return map[string]any{"id": id, "title": "Row " + id, "words": "Fine.", "met": true}
	}
	out, code := rows(t, map[string]any{"requirements": []any{map[string]any{"id": "a", "title": "A", "words": "No met."}}})
	if code != 1 {
		t.Errorf("a row without met: exit %d, want 1", code)
	}
	wantIn(t, out, "problem: kivali/requirements answered rows Kivali cannot read")

	bad := row("scopes")
	bad["fix"] = "re scope"
	out, code = rows(t, map[string]any{"requirements": []any{row("token"), bad}})
	if code != 1 {
		t.Errorf("a fix that is not a flow's name: exit %d, want 1", code)
	}
	wantIn(t, out, `problem: the row "scopes" is left out: its fix "re scope" is not a flow's name`, "met: Row token: Fine.")

	var many []any
	for i := range 21 {
		many = append(many, row(fmt.Sprintf("r%d", i)))
	}
	out, code = rows(t, map[string]any{"requirements": many})
	if code != 1 {
		t.Errorf("21 rows: exit %d, want 1", code)
	}
	wantIn(t, out, `problem: the integration gave more than 20 rows; Kivali shows the first 20 and leaves the row "r20" and those after it out`, "met: Row r19: Fine.")
}
