package integration

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// needPython skips a test that runs the smoke test's driver when the
// host has no python3 or bash; CI's runners and the agent shell have
// both.
func needPython(t *testing.T) {
	t.Helper()
	for _, bin := range []string{"python3", "bash"} {
		if _, err := exec.LookPath(bin); err != nil {
			t.Skipf("no %s on PATH", bin)
		}
	}
}

// runSmoke runs m's smoke test in dir the way an agent's shell runs
// it, with proxy variables in the shell's own environment.
func runSmoke(t *testing.T, m *Manifest, dir string) SmokeRun {
	t.Helper()
	cmd := exec.Command("bash", "-c", SmokeCommand(m))
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "HTTPS_PROXY=http://agent:secret@10.0.0.1:3128", "http_proxy=http://agent:secret@10.0.0.1:3128")
	var out, errOut bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errOut
	run := SmokeRun{}
	if err := cmd.Run(); err != nil {
		var ee *exec.ExitError
		if !errors.As(err, &ee) {
			t.Fatal(err)
		}
		run.ExitCode = ee.ExitCode()
	}
	run.Stdout, run.Stderr = out.String(), errOut.String()
	return run
}

// readEcho is the echo bundle copied to a temp directory with files
// replaced, and its manifest.
func readEcho(t *testing.T, files map[string]string) (*Manifest, string) {
	t.Helper()
	all := map[string]string{ManifestName: echoManifestBytes(t)}
	main, err := os.ReadFile(filepath.Join("testdata", "echo", "server", "main.py"))
	if err != nil {
		t.Fatal(err)
	}
	all["server/main.py"] = string(main)
	for p, b := range files {
		all[p] = b
	}
	dir := writeTree(t, all)
	m, err := ParseManifest([]byte(all[ManifestName]))
	if err != nil {
		t.Fatal(err)
	}
	return m, dir
}

func texts(f Findings) string {
	var out []string
	for _, x := range f {
		out = append(out, string(x.Level)+": "+x.Text)
	}
	return strings.Join(out, "\n")
}

// Why this test exists: the smoke test is what tells an agent, before
// the owner sees a version, that its server starts where it will run
// and lists the actions its manifest promises. The echo bundle does
// both, so it has nothing to say and names the actions it listed; and
// the server runs with none of the shell's proxy variables, so a server
// checked here has no network. A finding here would be a smoke test
// that refuses a good bundle; a proxy variable reaching the server
// would be a check that lets the server reach the internet.
func TestSmokeTestOfTheEchoBundle(t *testing.T) {
	needPython(t)
	m, dir := readEcho(t, nil)
	f, listed := SmokeFindings(m, runSmoke(t, m, dir))
	if len(f) != 0 {
		t.Errorf("findings:\n%s", texts(f))
	}
	if strings.Join(listed, ",") != "echo,list_colors,whoami" {
		t.Errorf("listed = %v", listed)
	}
	if _, err := os.Stat(filepath.Join(dir, "server", "__pycache__")); err == nil {
		t.Error("the smoke test wrote bytecode into the bundle")
	}

	// A server that prints its environment's names and stops.
	m, dir = readEcho(t, map[string]string{"server/main.py": "import os, sys\nsys.stderr.write(' '.join(sorted(os.environ)) + '\\n')\nsys.exit(3)\n"})
	f, _ = SmokeFindings(m, runSmoke(t, m, dir))
	got := texts(f)
	if !strings.HasPrefix(got, "error: the server ended with status 3 before it answered initialize; its error output ends: ") ||
		!strings.Contains(got, "ECHO_TOKEN") || !strings.Contains(got, "HOME") || strings.Contains(strings.ToUpper(got), "PROXY") {
		t.Errorf("findings = %s", got)
	}
}

// Why this test exists: each way a server fails at its start is one
// the pod would fail the same way, and the agent must hear which, with
// the server's own words where it has any, so it can fix the bundle
// before publishing rather than learn from a version that never runs.
// A declared action the server does not list is an error, since the
// owner would approve an action agents never get; one it lists that
// the manifest does not declare is a warning, since agents never get it.
func TestSmokeTestNamesWhatIsWrong(t *testing.T) {
	needPython(t)
	listTwo := strings.Replace(mainPy(t), `{"name": "whoami", "description": "Name the connected account.", "inputSchema": {"type": "object"}},`,
		`{"name": "drop_tables", "inputSchema": {"type": "array"}},`, 1)
	for _, tc := range []struct {
		name  string
		main  string
		want  []string
		clean bool
	}{
		{
			name: "a traceback",
			main: "import nosuchmodule\n",
			want: []string{"error: the server ended with status 1 before it answered initialize; its error output ends: ", "ModuleNotFoundError"},
		},
		{
			name: "a line that is not JSON-RPC",
			main: "import sys\nsys.stdin.readline()\nprint('hello')\nsys.stdout.flush()\nsys.stdin.readline()\n",
			want: []string{"error: the server wrote a line that is not JSON-RPC before it answered initialize, and Kivali stops a server that does: hello"},
		},
		{
			name: "an initialize error",
			main: "import sys, json\nm = json.loads(sys.stdin.readline())\nprint(json.dumps({'jsonrpc': '2.0', 'id': m['id'], 'error': {'code': -32000, 'message': 'no token'}}))\n",
			want: []string{"error: the server answered initialize with an error: no token"},
		},
		{
			name: "the wrong actions",
			main: listTwo,
			want: []string{
				"error: kivali.verify names whoami, which the server does not list; Test connection calls it",
				"warning: the server lists drop_tables, which tools does not declare; agents are never given it",
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m, dir := readEcho(t, map[string]string{"server/main.py": tc.main})
			f, _ := SmokeFindings(m, runSmoke(t, m, dir))
			got := texts(f)
			for _, w := range tc.want {
				if !strings.Contains(got, w) {
					t.Errorf("findings:\n%s\nwant %q", got, w)
				}
			}
		})
	}

	// A command that is not there.
	m, dir := readEcho(t, nil)
	m.Server.MCPConfig.Command = "python-that-is-not-there"
	f, _ := SmokeFindings(m, runSmoke(t, m, dir))
	if got := texts(f); !strings.HasPrefix(got, "error: the server did not start (python-that-is-not-there server/main.py): ") {
		t.Errorf("findings = %s", got)
	}
}

// Why this test exists: Kivali asks a setup bundle's server for its
// requirements whenever the owner reads its page, and a server that
// does not answer kivali/requirements, or answers in another shape,
// shows the owner one unmet row instead of its health. The smoke test
// is the agent's one chance to see that before publishing, so it must
// ask, with no connection, as at the server's first start, and say
// what is wrong. A bundle whose connection is not a setup one is never
// asked, since its server need not know the request.
func TestSmokeTestAsksASetupBundleForItsRequirements(t *testing.T) {
	needPython(t)
	var raw map[string]any
	if err := json.Unmarshal([]byte(echoManifestBytes(t)), &raw); err != nil {
		t.Fatal(err)
	}
	raw["user_config"] = map[string]any{}
	raw["kivali"].(map[string]any)["connection"] = map[string]any{"method": "setup", "setup": map[string]any{
		"summary": "Installs echo.", "hosts": []any{"example.com"},
		"fields": map[string]any{"key": map[string]any{"words": "A key.", "secret": true, "setup_only": true}},
		"keeps":  map[string]any{"token": "The token"},
	}}
	b, err := json.Marshal(raw)
	if err != nil {
		t.Fatal(err)
	}
	withRows := func(rows string) string {
		return strings.Replace(mainPy(t), `    if method == "ping":`, "    if method == \"kivali/requirements\":\n        return "+rows+", None\n    if method == \"ping\":", 1)
	}
	for _, tc := range []struct{ name, main, want string }{
		{"rows", withRows(`{"requirements": [{"id": "token", "title": "Token", "words": "No token is kept yet.", "met": False, "fix": "setup"}]}`), ""},
		{"no answer", mainPy(t), "error: the server answered kivali/requirements with an error: method not found: kivali/requirements"},
		{"another shape", withRows(`{"rows": []}`), `error: the server's kivali/requirements answer is not {"requirements": [...]} of rows with an id, a title, words and met; Kivali shows none of its rows`},
		{"a row without met", withRows(`{"requirements": [{"id": "token", "title": "Token", "words": "?"}]}`), `error: the server's kivali/requirements answer is not {"requirements": [...]}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m, dir := readEcho(t, map[string]string{ManifestName: string(b), "server/main.py": tc.main})
			f, _ := SmokeFindings(m, runSmoke(t, m, dir))
			if got := texts(f); (tc.want == "" && got != "") || !strings.HasPrefix(got, tc.want) {
				t.Errorf("findings = %q, want %q", got, tc.want)
			}
		})
	}
}

func mainPy(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", "echo", "server", "main.py"))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// Why this test exists: the smoke test's verdict is read from one line
// the driver prints, and what the shell gives back when the driver did
// not finish is not the bundle's fault. A shell that timed out, or
// output that is not the driver's, is a warning that the start was not
// checked, never an error that would keep a good bundle from being
// published; a server that did not answer in time names the deadline;
// the declared actions are checked by name, with the access form's
// listing tool worded for the form.
func TestSmokeFindingsReadsTheDriversLine(t *testing.T) {
	m, err := ParseManifest([]byte(echoManifestBytes(t)))
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name string
		run  SmokeRun
		want string
	}{
		{"timed out", SmokeRun{TimedOut: true}, "warning: the server's start could not be checked: the check did not finish within 30 seconds"},
		{"no driver", SmokeRun{Stderr: "bash: python3: command not found\n", ExitCode: 127}, "warning: the server's start could not be checked: bash: python3: command not found"},
		{"no answer", SmokeRun{Stdout: `{"started": true, "initialize": null, "timed_out": true, "stderr": ""}`}, "error: the server did not answer initialize within 20 seconds"},
		{"no list", SmokeRun{Stdout: "noise\n" + `{"started": true, "initialize": {"jsonrpc": "2.0", "id": 1, "result": {}}, "tools_list": {"jsonrpc": "2.0", "id": 2, "result": {"tools": [{"name": "echo"}, {"name": "whoami"}]}}}`},
			"error: kivali.access.choices.colors names list_colors, which the server does not list; the access form lists its options with it"},
		{"undeclared", SmokeRun{Stdout: `{"started": true, "initialize": {"jsonrpc": "2.0", "id": 1, "result": {}}, "tools_list": {"jsonrpc": "2.0", "id": 2, "result": {}}}`}, "error: the server's tools/list answer has no list of tools"},
	} {
		f, _ := SmokeFindings(m, tc.run)
		if got := texts(f); got != tc.want {
			t.Errorf("%s: findings = %q, want %q", tc.name, got, tc.want)
		}
	}
	m.Tools = append(m.Tools, Tool{Name: "extra"})
	f, listed := SmokeFindings(m, SmokeRun{Stdout: `{"started": true, "initialize": {"jsonrpc": "2.0", "id": 1, "result": {}}, "tools_list": {"jsonrpc": "2.0", "id": 2, "result": {"tools": [{"name": "whoami"}, {"name": "list_colors"}, {"name": "echo"}]}}}`})
	if got := texts(f); got != "error: tools declares extra, which the server does not list; agents are given only the actions it lists" {
		t.Errorf("findings = %q", got)
	}
	if !slices.Equal(listed, []string{"echo", "list_colors", "whoami"}) {
		t.Errorf("listed = %v", listed)
	}
}
