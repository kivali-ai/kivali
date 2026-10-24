package builtinskills_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/kivali-ai/kivali/internal/builtinskills"
	"github.com/kivali-ai/kivali/internal/integration"
	"github.com/kivali-ai/kivali/internal/integrationpod"
)

const buildAnIntegration = "build-an-integration"

// needPython skips a test that runs the skill's Python when the host
// has no python3 or bash; CI's runners and the agent shell have both.
func needPython(t *testing.T) {
	t.Helper()
	for _, bin := range []string{"python3", "bash"} {
		if _, err := exec.LookPath(bin); err != nil {
			t.Skipf("no %s on PATH", bin)
		}
	}
}

// writeSkill writes the embedded build-an-integration skill, what
// agents read under /files/skills/, to a temp directory.
func writeSkill(t *testing.T) string {
	t.Helper()
	files := builtinskills.Files(buildAnIntegration)
	if len(files) == 0 {
		t.Fatalf("%s is not a built-in skill", buildAnIntegration)
	}
	dir := t.TempDir()
	for _, f := range files {
		p := filepath.Join(dir, filepath.FromSlash(f.Path))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, f.Data, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

// edit replaces old, which must occur once, in the file at p.
func edit(t *testing.T, p, old, new string) {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	if n := strings.Count(string(b), old); n != 1 {
		t.Fatalf("%s holds %q %d times, not once", p, old, n)
	}
	if err := os.WriteFile(p, []byte(strings.Replace(string(b), old, new, 1)), 0o644); err != nil {
		t.Fatal(err)
	}
}

// notesStub is the imaginary notes service the template's server
// calls, on this machine, and its address.
func notesStub(t *testing.T) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reply := func(code int, v any) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(code)
			_ = json.NewEncoder(w).Encode(v)
		}
		if r.Header.Get("Authorization") != "Bearer test-token" {
			reply(http.StatusUnauthorized, map[string]string{"error": "bad token"})
			return
		}
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v1/me":
			reply(http.StatusOK, map[string]string{"name": "Ops bot", "email": "ops@acme.example"})
		case r.Method == http.MethodGet && r.URL.Path == "/v1/notebooks":
			reply(http.StatusOK, map[string]any{"notebooks": []map[string]string{{"name": "Ideas"}, {"name": "Ops"}}})
		case r.Method == http.MethodGet && r.URL.Path == "/v1/notebooks/Ideas/notes":
			reply(http.StatusOK, map[string]any{"notes": []map[string]string{{"id": "n1", "title": "Launch", "body": "Plan the launch", "updated_at": "2026-10-01"}}})
		case r.Method == http.MethodPost && r.URL.Path == "/v1/notebooks/Ideas/notes":
			reply(http.StatusCreated, map[string]string{"id": "n2", "title": "Ship it"})
		default:
			reply(http.StatusNotFound, map[string]string{"error": "no such path"})
		}
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

// runTry runs the skill's try.py from dir, with proxy variables in its
// environment as an agent's shell has them, and returns what it printed
// and its exit status.
func runTry(t *testing.T, dir string, args ...string) (string, int) {
	t.Helper()
	cmd := exec.Command("python3", append([]string{filepath.Join(dir, "scripts", "try.py")}, args...)...)
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

func wantIn(t *testing.T, out string, wants ...string) {
	t.Helper()
	for _, w := range wants {
		if !strings.Contains(out, w) {
			t.Errorf("the output does not say %q:\n%s", w, out)
		}
	}
}

// Why this test exists: the skill tells the Integrations engineer to
// start every bundle by copying its template. A template the bundle
// check refuses or warns about, or whose server the smoke test cannot
// start or does not list every action of, hands each integration built
// from it a failure the engineer did not write, found at
// integration_validate at best. The copy checked is the embedded one,
// which is what agents read under /files/skills/.
func TestBuildAnIntegrationTemplateIsABundleTheCheckAccepts(t *testing.T) {
	dir := filepath.Join(writeSkill(t), "template")
	b, err := integration.ReadDirectory(os.DirFS(dir))
	if err != nil {
		t.Fatal(err)
	}
	if f := integration.Validate(b.Manifest, b.Files); len(f) > 0 {
		t.Errorf("the check's findings on the template:\n%s", strings.Join(f.Texts(), "\n"))
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
		t.Errorf("the smoke test's findings on the template:\n%s", strings.Join(f.Texts(), "\n"))
	}
	slices.Sort(listed)
	if want := []string{"add_note", "find_notes", "list_notebooks", "whoami"}; !slices.Equal(listed, want) {
		t.Errorf("the template's server listed %v, want %v", listed, want)
	}
}

// Why this test exists: try.py is how the engineer sees, before
// publishing, what the owner's Test connection and access form and an
// agent's calls will meet, and the template is the access handling
// every bundle starts from. If try.py stopped driving the server as the
// pod and core do (no proxy variables, so the stub on this machine
// answers; core's _meta on an agent's requests; core's refusal of an
// action missing from the agent's list), or the template stopped
// holding an agent to its access or stopped answering before it is
// connected, the engineer would publish on a clean run that Kivali
// does not reproduce.
func TestTryCallsTheTemplateAsKivaliDoes(t *testing.T) {
	needPython(t)
	dir := writeSkill(t)
	api := "NOTES_API=" + notesStub(t)

	out, code := runTry(t, dir, "--env", api, "--field", "token=test-token", "template", "list", "verify", "choices", "notebooks")
	if code != 0 {
		t.Errorf("as the owner: exit %d", code)
	}
	wantIn(t, out,
		"Verified, in",
		"The owner reads: Signed in as Ops bot (ops@acme.example)",
		"The form offers, in",
		"Ideas, Ops",
		"No problems.",
	)

	out, code = runTry(t, dir, "--env", api, "--field", "token=test-token",
		"--agent", "dana", "--access", `{"notebooks":["Ideas"],"add":false}`, "template",
		"list",
		"call", "find_notes", `{"notebook":"Ideas"}`,
		"call", "find_notes", `{"notebook":"Ops"}`,
		"call", "add_note", `{"notebook":"Ideas","title":"x"}`)
	if code != 0 {
		t.Errorf("as an agent: exit %d", code)
	}
	wantIn(t, out,
		"  notes_find_notes\n",
		"Through the Notes integration; the owner decides what you may do with it.",
		`"enum":["Ideas"]`,
		`"title":"Launch"`,
		"Your access does not include the notebook Ops. Ask the owner if your work needs it.",
		"Kivali refuses it before it reaches the server: Your access to the Notes integration does not include add_note.",
		"No problems.",
	)
	if strings.Contains(out, "notes_add_note") {
		t.Errorf("an agent whose access does not allow adding was listed add_note:\n%s", out)
	}

	out, code = runTry(t, dir, "--agent", "dana", "--access", `{"notebooks":["Ideas"]}`, "template", "call", "find_notes", `{"notebook":"Ideas"}`)
	if code != 0 {
		t.Errorf("before it is connected: exit %d", code)
	}
	wantIn(t, out, "The Notes integration is not connected yet; the owner connects it on its page in Kivali.")
}

// Why this test exists: a line on the output that is not JSON-RPC makes
// the shim stop the server, and an id written back as text is never
// matched by core, so the call fails after 60 seconds. Neither shows in
// integration_validate unless it happens at start, and the skill sends
// the engineer to try.py to find them. If try.py stopped reporting them
// or stopped exiting 1, a bundle that dies on its first agent call
// would look clean. The template's print, which its first lines send to
// the log, is the case that must stay clean. An action whose input
// schema is not an object's is left out of the agent's list by core
// and its calls refused, which only core's own log says; try.py has to
// refuse the call too, or it reaches the server and looks fine.
func TestTryReportsWhatKivaliStopsOrNeverMatches(t *testing.T) {
	needPython(t)
	access := []string{"--agent", "dana", "--access", `{"notebooks":["Ideas"]}`, "template", "call", "find_notes", `{"notebook":"Ideas"}`}

	dir := writeSkill(t)
	edit(t, filepath.Join(dir, "template", "server", "main.py"), "def find_notes(args, access):\n",
		"def find_notes(args, access):\n    print(\"searching\", flush=True)\n")
	out, code := runTry(t, dir, access...)
	if code != 0 {
		t.Errorf("a print in an action: exit %d", code)
	}
	wantIn(t, out, "log stderr: searching", "No problems.")

	dir = writeSkill(t)
	edit(t, filepath.Join(dir, "template", "server", "main.py"), "def find_notes(args, access):\n",
		"def find_notes(args, access):\n    PROTOCOL.write(\"searching\\n\")\n    PROTOCOL.flush()\n")
	out, code = runTry(t, dir, access...)
	if code != 1 {
		t.Errorf("a line that is not JSON-RPC: exit %d, want 1", code)
	}
	wantIn(t, out, "problem: the server wrote a line that is not JSON-RPC; Kivali stops the server there. The line: searching")

	dir = writeSkill(t)
	edit(t, filepath.Join(dir, "template", "server", "main.py"), `out = {"jsonrpc": "2.0", "id": msg["id"]}`, `out = {"jsonrpc": "2.0", "id": str(msg["id"])}`)
	out, code = runTry(t, dir, "template", "list")
	if code != 1 {
		t.Errorf("an id written back as text: exit %d, want 1", code)
	}
	wantIn(t, out, `problem: the server answered request 1 with the id "1"`)

	dir = writeSkill(t)
	edit(t, filepath.Join(dir, "template", "server", "main.py"), "def find_schema(access):\n    return {\n        \"type\": \"object\",",
		"def find_schema(access):\n    return {\n        \"type\": \"array\",")
	out, code = runTry(t, dir, access...)
	if code != 1 {
		t.Errorf("an input schema that is not an object's: exit %d, want 1", code)
	}
	wantIn(t, out,
		"problem: the agent's list gives find_notes an inputSchema that is not an object's",
		"Kivali refuses it before it reaches the server: Your access to the Notes integration does not include find_notes.",
	)
}

// Why this test exists: a sign-in or registered bundle reads its token
// from kivali/credential on every call, and the answer comes back on the
// server's input among Kivali's own requests. The template's reading
// loop must hand that answer to the call waiting for it: one that drops
// a message without a method leaves every call of a bundle built on it
// waiting until Kivali gives up at 60 seconds. The template is changed
// as the skill says a sign-in bundle changes it (token() reads
// credential()), and an agent's call must then be answered at once with
// the service's data.
func TestTheTemplateReadsItsTokenFromKivali(t *testing.T) {
	needPython(t)
	dir := writeSkill(t)
	edit(t, filepath.Join(dir, "template", "server", "main.py"),
		"    if not TOKEN:\n        raise Refusal(\"The %s integration is not connected yet; the owner connects it on its page in Kivali.\" % SERVICE)\n    return TOKEN\n",
		"    return credential()[\"env\"][\"NOTES_TOKEN\"]\n")
	out, code := runTry(t, dir, "--env", "NOTES_API="+notesStub(t), "--field", "token=test-token",
		"--agent", "dana", "--access", `{"notebooks":["Ideas"]}`, "template", "call", "find_notes", `{"notebook":"Ideas"}`)
	if code != 0 {
		t.Errorf("exit %d", code)
	}
	wantIn(t, out, "note: the server asked for the connection's values", `"title":"Launch"`, "No problems.")
}

// Why this test exists: try.py and the templates copy two facts from
// the shim, the MCP protocol version its initialize names and the
// longest line it reads from the server. A shim that moved on without
// them would leave try.py passing a server the pod stops, or a template
// answering with a version Kivali no longer asks for.
func TestTryAndTheTemplateMatchTheShim(t *testing.T) {
	files := map[string]string{}
	for _, f := range builtinskills.Files(buildAnIntegration) {
		files[f.Path] = string(f.Data)
	}
	version := `PROTOCOL_VERSION = "` + integrationpod.ProtocolVersion + `"`
	for _, p := range []string{"scripts/try.py", "template/server/main.py", "template-setup/server/main.py"} {
		if !strings.Contains(files[p], version) {
			t.Errorf("%s does not hold %s", p, version)
		}
	}
	if integrationpod.MaxLineBytes != 16<<20 || !strings.Contains(files["scripts/try.py"], "MAX_LINE = 16 << 20") {
		t.Errorf("the shim reads lines of up to %d bytes; try.py's MAX_LINE must say the same", integrationpod.MaxLineBytes)
	}
}

// skillDocs are the skill's Markdown files an agent reads, by name.
var skillDocs = []string{"SKILL.md", "setup.md"}

// readSkillDocs is each of skillDocs in dir, by name.
func readSkillDocs(t *testing.T, dir string) map[string]string {
	t.Helper()
	out := map[string]string{}
	for _, name := range skillDocs {
		b, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			t.Fatal(err)
		}
		out[name] = string(b)
	}
	return out
}

// jsonBlocks are the bodies of the fenced json blocks in doc, indented
// ones (inside a list) among them.
func jsonBlocks(doc string) []string {
	var out []string
	lines := strings.Split(doc, "\n")
	for i := 0; i < len(lines); i++ {
		if strings.TrimSpace(lines[i]) != "```json" {
			continue
		}
		var body []string
		for i++; i < len(lines) && strings.TrimSpace(lines[i]) != "```"; i++ {
			body = append(body, lines[i])
		}
		out = append(out, strings.Join(body, "\n"))
	}
	return out
}

// kivaliKeys are the keys of a manifest's kivali section a block may
// give.
var kivaliKeys = []string{"connection", "hosts", "packages", "access", "verify", "setup_notes"}

// Why this test exists: the skill teaches each way a bundle connects by
// a manifest block the developer copies, and the check is what decides
// whether a bundle written from one is stored. A block the check
// refuses (a key a rule forbids, a field a shape lacks) hands every
// bundle built from it a refusal at integration_validate, or teaches
// the developer a rule wrong. So every json block of SKILL.md and
// setup.md that holds a "connection" is put into the token template's
// manifest (its connection fields and their placeholders taken out,
// since a setup connection has none) in place of the kivali keys it
// gives, and must pass integration.Validate as a bundle with no
// finding. A block may give only kivali keys: one that gives a
// top-level key would be put in the wrong place and pass for the wrong
// reason. And the blocks must cover every way the skill teaches, so
// none goes unchecked by being left out.
func TestTheSkillsManifestBlocksPassTheCheck(t *testing.T) {
	dir := writeSkill(t)
	tmpl, err := integration.ReadDirectory(os.DirFS(filepath.Join(dir, "template")))
	if err != nil {
		t.Fatal(err)
	}
	ways := map[string]bool{}
	for name, doc := range readSkillDocs(t, dir) {
		for _, block := range jsonBlocks(doc) {
			if !strings.Contains(block, `"connection"`) {
				continue
			}
			var frag map[string]any
			if err := json.Unmarshal([]byte("{"+block+"}"), &frag); err != nil {
				t.Errorf("a manifest block of %s is not JSON once inside braces: %v\n%s", name, err, block)
				continue
			}
			var m map[string]any
			if err := json.Unmarshal(tmpl.ManifestBytes(), &m); err != nil {
				t.Fatal(err)
			}
			m["user_config"] = map[string]any{}
			env := m["server"].(map[string]any)["mcp_config"].(map[string]any)["env"].(map[string]any)
			for k, v := range env {
				if strings.Contains(v.(string), "${") {
					delete(env, k)
				}
			}
			k := m["kivali"].(map[string]any)
			for key, v := range frag {
				if !slices.Contains(kivaliKeys, key) {
					t.Errorf("a manifest block of %s gives %s, which is not a key of the kivali section:\n%s", name, key, block)
				}
				k[key] = v
			}
			raw, err := json.Marshal(m)
			if err != nil {
				t.Fatal(err)
			}
			mf, err := integration.ParseManifest(raw)
			if err != nil {
				t.Errorf("a manifest block of %s does not parse: %v\n%s", name, err, block)
				continue
			}
			if f := integration.Validate(mf, tmpl.Files); len(f) > 0 {
				t.Errorf("the check's findings on a manifest block of %s:\n%s\n%s", name, strings.Join(f.Texts(), "\n"), block)
			}
			c := mf.Kivali.Connection
			way := c.Method
			if c.Registered() {
				way += " registered"
			}
			if c.Setup != nil && c.Setup.Then != "" {
				way += " then " + c.Setup.Then
			}
			ways[way] = true
		}
	}
	for _, want := range []string{"token", "device", "browser", "browser registered", "setup", "setup then browser"} {
		if !ways[want] {
			t.Errorf("the skill has no manifest block for a %s connection; it has %v", want, ways)
		}
	}
}

// Why this test exists: the skill shows the exact JSON of what Kivali
// sends a setup bundle and what the server answers, and the developer
// writes the server from it. Core reads those answers strictly
// (integrationpod.ParseSetupResult, ParseRequirements): an example
// with a key a shape lacks, or a value of another type, teaches a
// server Kivali fails every flow of, or whose requirements read as one
// unmet row. So every JSON value in the json blocks of SKILL.md and
// setup.md that is a kivali/setup request's params, a step (bare or as
// a result), a requirements result or a progress notification is read
// as core reads it, and each kind must appear at least once.
func TestTheSkillsWireExamplesReadAsCoreReadsThem(t *testing.T) {
	dir := writeSkill(t)
	strict := func(raw []byte, v any) error {
		dec := json.NewDecoder(bytes.NewReader(raw))
		dec.DisallowUnknownFields()
		return dec.Decode(v)
	}
	seen := map[string]int{}
	for name, doc := range readSkillDocs(t, dir) {
		for _, block := range jsonBlocks(doc) {
			if strings.Contains(block, `"connection"`) {
				continue
			}
			dec := json.NewDecoder(strings.NewReader(block))
			for dec.More() {
				var raw json.RawMessage
				if err := dec.Decode(&raw); err != nil {
					t.Errorf("a json block of %s is not JSON: %v\n%s", name, err, block)
					break
				}
				var msg struct {
					Method string          `json:"method"`
					Params json.RawMessage `json:"params"`
					Result json.RawMessage `json:"result"`
					Step   json.RawMessage `json:"step"`
				}
				if err := json.Unmarshal(raw, &msg); err != nil {
					t.Errorf("a json value of %s is not an object: %v\n%s", name, err, raw)
					continue
				}
				var err error
				kind := ""
				switch {
				case msg.Method == integrationpod.MethodSetup:
					kind = "setup params"
					var p integrationpod.SetupParams
					err = strict(msg.Params, &p)
				case msg.Method == integrationpod.MethodProgress:
					kind = "progress"
					var p integrationpod.ProgressParams
					err = strict(msg.Params, &p)
				case msg.Step != nil:
					kind = "step"
					_, err = integrationpod.ParseSetupResult(raw)
				case bytes.Contains(msg.Result, []byte(`"step"`)):
					kind = "step"
					_, err = integrationpod.ParseSetupResult(msg.Result)
				case bytes.Contains(msg.Result, []byte(`"requirements"`)):
					kind = "requirements"
					_, err = integrationpod.ParseRequirements(msg.Result)
				}
				if err != nil {
					t.Errorf("%s's %s example does not read as core reads it: %v\n%s", name, kind, err, raw)
				}
				if kind != "" {
					seen[kind]++
				}
			}
		}
	}
	for _, kind := range []string{"setup params", "progress", "step", "requirements"} {
		if seen[kind] == 0 {
			t.Errorf("the skill shows no %s example; it shows %v", kind, seen)
		}
	}
	if seen["step"] < 2 {
		t.Errorf("the skill shows %d steps, want the Form's and the sketch's Go at least", seen["step"])
	}
}

// Why this test exists: the skill names MCP's protocol version where a
// server answers initialize, and the remote sketch sends it upstream.
// A literal that differs from the one the shim asks for teaches a
// server to answer with a version Kivali does not ask for.
func TestTheSkillsProtocolVersionIsTheShims(t *testing.T) {
	dir := writeSkill(t)
	version := regexp.MustCompile(`\b20\d\d-\d\d-\d\d\b`)
	for name, doc := range readSkillDocs(t, dir) {
		for _, v := range version.FindAllString(doc, -1) {
			if v != integrationpod.ProtocolVersion {
				t.Errorf("%s names the protocol version %s; the shim's is %s", name, v, integrationpod.ProtocolVersion)
			}
		}
	}
}
