package builtinskills_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/kivali-ai/kivali/internal/integration"
)

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

// skillRuns are the shell commands of the fenced block that follows the
// line after in the skill's file: each python3 command, with its
// continuation lines joined, and the block's assignments before it. The
// block's cd is left out (the runs start in the skill's directory) and
// its port 8765 is port.
func skillRuns(t *testing.T, dir, file, after string, port int) []string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, file))
	if err != nil {
		t.Fatal(err)
	}
	doc := string(b)
	at := strings.Index(doc, after)
	if at < 0 {
		t.Fatalf("%s has no %q", file, after)
	}
	block := doc[at:]
	open := strings.Index(block, "```\n")
	if open < 0 {
		t.Fatalf("no block follows %q", after)
	}
	block = block[open+4:]
	block = block[:strings.Index(block, "```")]
	block = strings.ReplaceAll(block, "\\\n", " ")
	var prelude, runs []string
	for _, line := range strings.Split(block, "\n") {
		line = strings.TrimSpace(strings.ReplaceAll(line, "8765", strconv.Itoa(port)))
		switch {
		case line == "", strings.HasPrefix(line, "#"), strings.HasPrefix(line, "cd "):
		case strings.HasPrefix(line, "python3 "):
			runs = append(runs, strings.Join(append(slices.Clone(prelude), line), "\n"))
		default:
			prelude = append(prelude, line)
		}
	}
	return runs
}

// freePort is a loopback port nothing listens on, for a stub.
func freePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := l.Addr().(*net.TCPAddr).Port
	if err := l.Close(); err != nil {
		t.Fatal(err)
	}
	return port
}

// runShell runs script with bash in dir, with proxy variables in its
// environment as an agent's shell has them, and returns what it printed
// and its exit status.
func runShell(t *testing.T, dir, script string) (string, int) {
	t.Helper()
	cmd := exec.Command("bash", "-c", script)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "HTTPS_PROXY=http://agent:secret@10.0.0.1:3128", "http_proxy=http://agent:secret@10.0.0.1:3128")
	out, err := cmd.CombinedOutput()
	if err != nil {
		var ee *exec.ExitError
		if !errors.As(err, &ee) {
			t.Fatal(err)
		}
		return string(out), ee.ExitCode()
	}
	return string(out), 0
}

// Why this test exists: the skill's example runs of template-setup are
// how the developer first sees a setup flow, and every setup bundle
// starts from them: try.py against scripts/deals_stub.py, word for word
// as setup.md gives them. Each run is one the owner meets: the install,
// its stages as progress and what it keeps; a key the service refuses
// answered on the Form, then one it takes; the token only on the app's
// page, a Wait checked until it is shown, the owner leaving it, the
// install's connection forgotten and a second run taking up the app the
// first made rather than making another; an installed integration's
// requirements, an unmet one's Fix run as its own flow, and an uninstall
// asked for twice; and the requirements with no connection, as
// integration_validate asks. If the commands, the stub, try.py or the
// template stopped agreeing, the developer would be shown a run that
// fails, or one Kivali does not reproduce.
func TestTheSkillsSetupRunsDoWhatItSays(t *testing.T) {
	needPython(t)
	dir := writeSkill(t)
	runs := skillRuns(t, dir, "setup.md", "Run it against the stub before you change it:", freePort(t))
	if len(runs) != 5 {
		t.Fatalf("the block holds %d runs, want 5:\n%s", len(runs), strings.Join(runs, "\n\n"))
	}
	run := func(i int) string {
		t.Helper()
		out, code := runShell(t, dir, runs[i])
		if code != 0 {
			t.Errorf("run %d exited %d:\n%s\n%s", i+1, code, runs[i], out)
		}
		return out
	}

	out := run(0)
	wantIn(t, out,
		"A Form, in",
		"asks for: setup_key",
		"progress: Checking the setup key (1 of 4)",
		"progress: Reading the app's token (4 of 4)",
		"Kivali keeps app_id, token in the connection's Secret, and starts the server again with it, as the pod does.",
		"the connection is made, and the integration is installed.",
		"No problems.",
	)
	if strings.Contains(out, "sk-test-1") || strings.Contains(out, "app-token-0001") {
		t.Errorf("a secret reached the output:\n%s", out)
	}

	wantIn(t, run(1),
		"Deals did not take that setup key; each one works once. Paste a new one.",
		"the connection is made",
		"No problems.",
	)

	out = run(2)
	wantIn(t, out,
		"A Wait, in",
		"page: https://app.deals.example/apps/app_1\n",
		"Kivali keeps app_id in the connection's Secret",
		"The same Wait, in",
		"The setup did not finish, so Kivali forgets the connection it made (app_id)",
		"asks for: token",
		"Kivali keeps token in the connection's Secret",
		"the connection is made",
		"PUT /v1/apps/app_1/scopes -> 200",
		"No problems.",
	)
	if n := strings.Count(out, "POST /v1/apps -> 201"); n != 1 {
		t.Errorf("the runs made %d apps in Deals, want 1:\n%s", n, out)
	}

	wantIn(t, run(3),
		"met: App token: The app's token works, in Acme.",
		"not met: Scopes: This version needs deals.read, which the app was not built with.",
		"Fix runs the flow rescope: try it with the request setup rescope",
		"setup rescope, as Kivali runs it:",
		"progress: Updating the app's scopes (2 of 3)",
		"the Fix is made.",
		"met: Scopes: The app has every scope this version needs.",
		"the app was removed",
		"Deals answered 401 to removing the app; there is nothing left to undo",
		"No problems.",
	)

	wantIn(t, run(4), "not met: App token: No app token is kept yet.", "Fix runs the flow setup")
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

	// A kept value whose keep is declared "secret": false (template-setup's
	// app_id) is shown as it is, as Kivali shows it; a secret one (the
	// token) is not. Were try.py to scrub both, a developer would avoid
	// naming a page by the app's id that Kivali shows whole.
	shown := map[string]any{"step": map[string]any{
		"wait": map[string]any{"words": "Open app-12345 and show tok-abcdef.", "url": "https://app.deals.example/apps/app-12345"},
		"keep": map[string]any{"app_id": "app-12345", "token": "tok-abcdef"},
	}}
	out, _ = try(t, []map[string]any{shown}, "--every", "0", "--checks", "0")
	wantIn(t, out, "Open app-12345 and show [secret].", "page: https://app.deals.example/apps/app-12345\n")

	away := map[string]any{"step": map[string]any{"go": map[string]any{"url": "https://app.deals.example/settings", "no_return": true}}}
	out, code = try(t, []map[string]any{away, done})
	if code != 0 {
		t.Errorf("a Go with no return: exit %d", code)
	}
	wantIn(t, out, "calls setup again at once", `"went": true`, `"team": "Kivali"`)
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
