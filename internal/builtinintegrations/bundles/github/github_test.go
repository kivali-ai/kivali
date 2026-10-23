package github

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kivali-ai/kivali/internal/gitrepo"
	"github.com/kivali-ai/kivali/internal/integration"
)

// readBundle is the embedded bundle as boot reads it.
func readBundle(t *testing.T) *integration.Bundle {
	t.Helper()
	b, err := integration.ReadDirectory(Bundle().FS)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// Why this test exists: boot installs a shipped bundle only when it
// passes the check an upload passes, so a manifest the check refuses
// reaches every team as a GitHub integration that never installs. It
// also pins what the rest of Kivali relies on: a device sign-in with
// both of GitHub's endpoints and the token handed over as GH_TOKEN, gh
// installed in the pod, and an access form whose repositories field is
// required, so access without one is refused at save rather than
// granting nothing silently.
func TestManifestPassesTheUploadCheck(t *testing.T) {
	b := readBundle(t)
	f := integration.Validate(b.Manifest, b.Files)
	if f.HasErrors() {
		t.Fatalf("findings: %v", f.Texts())
	}
	// A warning would show on every team's install screen.
	if w := f.Warnings(); len(w) != 0 {
		t.Errorf("warnings: %v", w)
	}
	m := b.Manifest
	if m.Name != Name || m.Version != "1.0.0" || m.Author.Name != "Kivali" {
		t.Errorf("name, version, author = %s %s %s", m.Name, m.Version, m.Author.Name)
	}
	c := m.Kivali.Connection
	if c.Method != "device" || c.DeviceEndpoint != "https://github.com/login/device/code" ||
		c.AuthorizationEndpoint != "https://github.com/login/oauth/authorize" ||
		c.TokenEndpoint != "https://github.com/login/oauth/access_token" ||
		c.CredentialKeys["access_token"] != "GH_TOKEN" || len(c.Scopes) != 0 {
		t.Errorf("connection = %+v", c)
	}
	// Both ways are offered, a code first, and the install screen names
	// github.com as where the sign-in goes.
	if !slices.Equal(c.Ways(), []string{"device", "browser"}) || !slices.Equal(c.SignInHosts(), []string{"github.com"}) {
		t.Errorf("ways %v, sign-in hosts %v", c.Ways(), c.SignInHosts())
	}
	if !slices.Equal(m.Kivali.Packages, []string{"gh"}) || !slices.Equal(m.Kivali.Hosts, []string{"api.github.com", "github.com"}) {
		t.Errorf("packages %v, hosts %v", m.Kivali.Packages, m.Kivali.Hosts)
	}
	if len(m.Server.MCPConfig.Env) != 0 {
		t.Errorf("mcp_config.env = %v: the token comes from kivali/credential on each call", m.Server.MCPConfig.Env)
	}
	if !strings.Contains(m.Kivali.SetupNotes, "Enable Device Flow") {
		t.Errorf("setup notes lack the device flow step")
	}
	if err := integration.ValidateAccess(m, json.RawMessage(`{"repositories":["acme/*"],"write":false}`)); err != nil {
		t.Errorf("good access refused: %v", err)
	}
	if err := integration.ValidateAccess(m, json.RawMessage(`{"write":true}`)); err == nil {
		t.Error("access without repositories accepted")
	}
}

// Why this test exists: the setup notes send the owner to GitHub's
// new-app form with Kivali's settings in the address, and the owner
// leaves them as they are. A parameter lost in an edit would register
// an app that cannot do what agents need (a permission missing), sends
// GitHub's events nowhere (a webhook on), asks for OAuth at install, can
// be installed on any account, or does not know an address the team
// signs in at, and nothing would say so until a call failed. The
// personal link and the organization address must carry the same
// settings, the organization's under its own path.
func TestSetupNotesPreFillTheApp(t *testing.T) {
	notes := readBundle(t).Manifest.Kivali.SetupNotes
	link := regexp.MustCompile(`\]\((https://github\.com/settings/apps/new\?[^)\s]+)\)`).FindStringSubmatch(notes)
	org := regexp.MustCompile("(?s)```Organization address\n(https://[^\n]+)\n```").FindStringSubmatch(notes)
	if link == nil || org == nil {
		t.Fatalf("notes lack the personal link or the organization address:\n%s", notes)
	}
	want := url.Values{
		"description":              {"Lets Kivali agents work with issues and pull requests."},
		"url":                      {"https://kivali.ai"},
		"callback_urls[]":          {"{{redirect_url}}", "http://127.0.0.1/connect/callback", "{{external_redirect_url}}"},
		"request_oauth_on_install": {"false"},
		"public":                   {"false"},
		"webhook_active":           {"false"},
		"contents":                 {"read"},
		"issues":                   {"write"},
		"metadata":                 {"read"},
		"pull_requests":            {"write"},
	}
	for _, tc := range []struct{ raw, path string }{
		{link[1], "/settings/apps/new"},
		{org[1], "/organizations/ORGANIZATION/settings/apps/new"},
	} {
		u, err := url.Parse(tc.raw)
		if err != nil {
			t.Fatalf("%s: %v", tc.raw, err)
		}
		if u.Host != "github.com" || u.Path != tc.path || !reflect.DeepEqual(u.Query(), want) {
			t.Errorf("%s:\n path %s, query %v\nwant %s, %v", tc.raw, u.Path, u.Query(), tc.path, want)
		}
	}
}

// needPython skips a test that runs the server when the host has no
// python3 or bash; CI's runners have both.
func needPython(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("no bash on PATH")
	}
	py, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("no python3 on PATH")
	}
	py, err = filepath.Abs(py)
	if err != nil {
		t.Fatal(err)
	}
	return py
}

// copyBundle writes the embedded bundle to dir, as the pod's volume
// holds it.
func copyBundle(t *testing.T, dir string) {
	t.Helper()
	err := fs.WalkDir(Bundle().FS, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		dst := filepath.Join(dir, filepath.FromSlash(p))
		if d.IsDir() {
			return os.MkdirAll(dst, 0o755)
		}
		b, err := fs.ReadFile(Bundle().FS, p)
		if err != nil {
			return err
		}
		return os.WriteFile(dst, b, 0o444)
	})
	if err != nil {
		t.Fatal(err)
	}
}

// Why this test exists: the smoke test is what Kivali's own check of a
// bundle runs, the server started with no network and no connection,
// asked to initialize and list its actions. A server that ran gh or
// asked for the token at start, or that listed other actions than the
// manifest declares, would fail it here before it failed in every
// team's pod.
func TestServerPassesTheSmokeTest(t *testing.T) {
	needPython(t)
	dir := t.TempDir()
	copyBundle(t, dir)
	m := readBundle(t).Manifest
	cmd := exec.Command("bash", "-c", integration.SmokeCommand(m))
	cmd.Dir = dir
	cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + dir}
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
	f, listed := integration.SmokeFindings(m, run)
	if len(f) != 0 {
		t.Fatalf("findings: %v\nstderr: %s", f.Texts(), run.Stderr)
	}
	if len(listed) != len(m.Tools) {
		t.Errorf("listed %v", listed)
	}
}

// fakeGHScript is gh as the tests run it: it appends its argv, its
// environment and what came on its input to gh-calls.jsonl beside its
// bin directory, and answers by the first rule in gh-rules.json whose
// argv is a prefix of its own.
const fakeGHScript = `import json, os, sys
root = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
argv = sys.argv[1:]
stdin = sys.stdin.read()
with open(os.path.join(root, "gh-calls.jsonl"), "a") as f:
    f.write(json.dumps({"argv": argv, "env": dict(os.environ), "stdin": stdin}) + "\n")
with open(os.path.join(root, "gh-rules.json")) as f:
    rules = json.load(f)
for rule in rules:
    if argv[: len(rule["argv"])] == rule["argv"]:
        sys.stdout.write(rule.get("stdout", ""))
        sys.stderr.write(rule.get("stderr", ""))
        sys.exit(rule.get("code", 0))
sys.stderr.write("fake gh: no answer for %r\n" % argv)
sys.exit(1)
`

// ghRule is one canned answer of the fake gh.
type ghRule struct {
	Argv   []string `json:"argv"`
	Stdout string   `json:"stdout,omitempty"`
	Stderr string   `json:"stderr,omitempty"`
	Code   int      `json:"code,omitempty"`
}

// ghCall is one run of the fake gh, as it recorded it.
type ghCall struct {
	Argv  []string          `json:"argv"`
	Env   map[string]string `json:"env"`
	Stdin string            `json:"stdin"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type rpcMessage struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

// credentialAnswer is what the fake core answers the server's nth
// kivali/credential (from 1): a result, or an error.
type credentialAnswer func(n int) (any, *rpcError)

// tokens answers each kivali/credential with the next token, as core
// answers a sign-in connection: in env and fields under GH_TOKEN, with
// the client id beside it in fields.
func tokens(list ...string) credentialAnswer {
	return func(n int) (any, *rpcError) {
		tok := list[min(n, len(list))-1]
		return map[string]any{
			"env":    map[string]string{"GH_TOKEN": tok},
			"fields": map[string]string{"GH_TOKEN": tok, "client_id": "Iv23liTest"},
		}, nil
	}
}

// server is the GitHub server started as its pod starts it, with the
// test as core on the other side of its standard streams: it answers
// kivali/credential and keeps kivali/log, as the shim passes them on.
type server struct {
	t    *testing.T
	dir  string
	in   io.WriteCloser
	cred credentialAnswer

	wmu sync.Mutex
	mu  sync.Mutex
	// credentials is how many kivali/credential requests came.
	credentials int
	logs        []string
	// notJSON is every line on the protocol that is not JSON-RPC.
	notJSON []string
	waiting map[string]chan rpcMessage
	seq     int
	stderr  bytes.Buffer
}

// startServer starts the server with a fake gh first on its PATH.
func startServer(t *testing.T, cred credentialAnswer) *server {
	t.Helper()
	py := needPython(t)
	dir := t.TempDir()
	bundle := filepath.Join(dir, "bundle")
	copyBundle(t, bundle)
	bin := filepath.Join(dir, "bin")
	if err := os.MkdirAll(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bin, "gh"), []byte("#!"+py+" -I\n"+fakeGHScript), 0o755); err != nil {
		t.Fatal(err)
	}
	s := &server{t: t, dir: dir, cred: cred, waiting: map[string]chan rpcMessage{}}
	s.setGH()
	cmd := exec.Command(py, "-I", "server/main.py")
	cmd.Dir = bundle
	cmd.Env = []string{
		"PATH=" + bin + string(os.PathListSeparator) + "/usr/bin" + string(os.PathListSeparator) + "/bin",
		"HOME=" + dir,
		"HTTPS_PROXY=http://integration-github:scoped@10.43.0.12:3128",
		// A variable of the server's own that gh is not given.
		"KIVALI_TEST_SERVER_ONLY=1",
	}
	in, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	out, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	cmd.Stderr = &lockedWriter{mu: &s.mu, w: &s.stderr}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	s.in = in
	read := make(chan struct{})
	go func() {
		defer close(read)
		s.read(out)
	}()
	t.Cleanup(func() {
		_ = in.Close()
		<-read
		_ = cmd.Wait()
		s.mu.Lock()
		defer s.mu.Unlock()
		if len(s.notJSON) > 0 {
			t.Errorf("the server wrote lines that are not JSON-RPC, and Kivali stops a server that does: %q", s.notJSON)
		}
	})
	s.request("initialize", map[string]any{"protocolVersion": "2025-06-18", "capabilities": map[string]any{}, "clientInfo": map[string]any{"name": "kivali", "version": "test"}})
	s.write(map[string]any{"jsonrpc": "2.0", "method": "notifications/initialized"})
	return s
}

type lockedWriter struct {
	mu *sync.Mutex
	w  io.Writer
}

func (l *lockedWriter) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.w.Write(p)
}

func (s *server) write(msg any) {
	b, err := json.Marshal(msg)
	if err != nil {
		s.t.Fatal(err)
	}
	s.wmu.Lock()
	defer s.wmu.Unlock()
	_, _ = s.in.Write(append(b, '\n'))
}

func (s *server) read(out io.Reader) {
	sc := bufio.NewScanner(out)
	sc.Buffer(make([]byte, 64<<10), 16<<20)
	for sc.Scan() {
		var m rpcMessage
		if err := json.Unmarshal(sc.Bytes(), &m); err != nil || m.JSONRPC != "2.0" {
			s.mu.Lock()
			s.notJSON = append(s.notJSON, sc.Text())
			s.mu.Unlock()
			continue
		}
		switch m.Method {
		case "":
			s.mu.Lock()
			ch := s.waiting[string(m.ID)]
			delete(s.waiting, string(m.ID))
			s.mu.Unlock()
			if ch != nil {
				ch <- m
			}
		case "kivali/credential":
			s.mu.Lock()
			s.credentials++
			n := s.credentials
			s.mu.Unlock()
			result, rerr := s.cred(n)
			reply := map[string]any{"jsonrpc": "2.0", "id": m.ID}
			if rerr != nil {
				reply["error"] = rerr
			} else {
				reply["result"] = result
			}
			s.write(reply)
		case "kivali/log":
			var p struct {
				Message string `json:"message"`
			}
			_ = json.Unmarshal(m.Params, &p)
			s.mu.Lock()
			s.logs = append(s.logs, p.Message)
			s.mu.Unlock()
		}
	}
}

// request sends one of core's requests and waits for its answer.
func (s *server) request(method string, params any) json.RawMessage {
	s.t.Helper()
	s.mu.Lock()
	s.seq++
	id, _ := json.Marshal(s.seq)
	ch := make(chan rpcMessage, 1)
	s.waiting[string(id)] = ch
	s.mu.Unlock()
	s.write(map[string]any{"jsonrpc": "2.0", "id": json.RawMessage(id), "method": method, "params": params})
	select {
	case m := <-ch:
		if m.Error != nil {
			s.t.Fatalf("%s: error %d %s", method, m.Error.Code, m.Error.Message)
		}
		return m.Result
	case <-time.After(30 * time.Second):
		s.mu.Lock()
		defer s.mu.Unlock()
		s.t.Fatalf("%s: no answer; the server's error output:\n%s", method, s.stderr.String())
		return nil
	}
}

// owner is the access of the owner's own request, which carries no
// _meta.kivali.
var owner map[string]any

func meta(access map[string]any) map[string]any {
	if access == nil {
		return nil
	}
	return map[string]any{"kivali": map[string]any{"agent": "dana", "access": access}}
}

// call is one tools/call, by the owner (access nil) or by an agent
// with access, and the text and isError of its answer.
func (s *server) call(name string, args map[string]any, access map[string]any) (string, bool) {
	s.t.Helper()
	params := map[string]any{"name": name, "arguments": args}
	if m := meta(access); m != nil {
		params["_meta"] = m
	}
	var res struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
		IsError bool `json:"isError"`
	}
	if err := json.Unmarshal(s.request("tools/call", params), &res); err != nil {
		s.t.Fatal(err)
	}
	if len(res.Content) != 1 || res.Content[0].Type != "text" {
		s.t.Fatalf("%s: content %+v", name, res.Content)
	}
	return res.Content[0].Text, res.IsError
}

// toolNames is the names tools/list answers, for the owner or for an
// agent with access.
func (s *server) toolNames(access map[string]any) []string {
	s.t.Helper()
	params := map[string]any{}
	if m := meta(access); m != nil {
		params["_meta"] = m
	}
	var res struct {
		Tools []struct {
			Name        string          `json:"name"`
			InputSchema json.RawMessage `json:"inputSchema"`
		} `json:"tools"`
	}
	if err := json.Unmarshal(s.request("tools/list", params), &res); err != nil {
		s.t.Fatal(err)
	}
	var names []string
	for _, tool := range res.Tools {
		names = append(names, tool.Name)
	}
	return names
}

// setGH replaces the fake gh's answers.
func (s *server) setGH(rules ...ghRule) {
	s.t.Helper()
	if rules == nil {
		rules = []ghRule{}
	}
	b, _ := json.Marshal(rules)
	if err := os.WriteFile(filepath.Join(s.dir, "gh-rules.json"), b, 0o644); err != nil {
		s.t.Fatal(err)
	}
}

// ghCalls is every run of the fake gh so far.
func (s *server) ghCalls() []ghCall {
	s.t.Helper()
	b, err := os.ReadFile(filepath.Join(s.dir, "gh-calls.jsonl"))
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		s.t.Fatal(err)
	}
	var out []ghCall
	for _, line := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		var c ghCall
		if err := json.Unmarshal([]byte(line), &c); err != nil {
			s.t.Fatal(err)
		}
		out = append(out, c)
	}
	return out
}

func (s *server) credentialCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.credentials
}

// errorOutput is the server's error output and log lines so far.
func (s *server) errorOutput() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.stderr.String() + strings.Join(s.logs, "\n")
}

const repoJSON = `{"name":"widgets","owner":{"login":"acme"},"url":"https://github.com/acme/widgets"}`

// Why this test exists: core renews a sign-in's token while the server
// runs, without restarting it, so a server that read the token once
// (at start, or on its first call) would go on calling GitHub with an
// expired one, and every agent's call would fail an hour or so after
// the owner signed in. The token must also reach gh only through its
// environment: on its command line it would be readable by anything
// that lists processes, and in a gh login it would sit on disk past a
// sign-out. And gh must get the settings that keep it to github.com,
// off prompts and through the pod's proxy, and nothing else of the
// server's environment or the connection's other values.
func TestServerTakesTheTokenFromKivaliOnEveryCall(t *testing.T) {
	s := startServer(t, tokens("ghu_first0000", "ghu_second000"))
	if got := s.toolNames(owner); len(got) == 0 {
		t.Fatal("no tools")
	}
	if n := s.credentialCount(); n != 0 {
		t.Errorf("asked for the token %d times before any call", n)
	}
	s.setGH(ghRule{Argv: []string{"repo", "view"}, Stdout: repoJSON + "\n"})
	for i := 0; i < 2; i++ {
		text, isErr := s.call("get_repository", map[string]any{"repository": "acme/widgets"}, owner)
		if isErr || text != repoJSON {
			t.Fatalf("call %d: %v %s", i, isErr, text)
		}
	}
	if n := s.credentialCount(); n != 2 {
		t.Errorf("asked for the token %d times for 2 calls", n)
	}
	calls := s.ghCalls()
	if len(calls) != 2 {
		t.Fatalf("gh ran %d times", len(calls))
	}
	for i, want := range []string{"ghu_first0000", "ghu_second000"} {
		c := calls[i]
		if c.Env["GH_TOKEN"] != want {
			t.Errorf("run %d: GH_TOKEN %q, want %q", i, c.Env["GH_TOKEN"], want)
		}
		if !slices.Equal(c.Argv, []string{"repo", "view", "acme/widgets", "--json=name,owner,description,defaultBranchRef,isPrivate,url,updatedAt"}) {
			t.Errorf("run %d: argv %q", i, c.Argv)
		}
		for _, a := range c.Argv {
			if strings.Contains(a, "ghu_") {
				t.Errorf("run %d: the token is on gh's command line: %q", i, c.Argv)
			}
		}
		for k, v := range map[string]string{
			"GH_HOST": "github.com", "GH_CONFIG_DIR": "/tmp/gh", "GH_NO_UPDATE_NOTIFIER": "1",
			"GH_PROMPT_DISABLED": "1", "NO_COLOR": "1", "HOME": s.dir,
			"HTTPS_PROXY": "http://integration-github:scoped@10.43.0.12:3128",
		} {
			if c.Env[k] != v {
				t.Errorf("run %d: %s=%q, want %q", i, k, c.Env[k], v)
			}
		}
		for _, k := range []string{"KIVALI_TEST_SERVER_ONLY", "client_id"} {
			if _, ok := c.Env[k]; ok {
				t.Errorf("run %d: gh's environment holds %s", i, k)
			}
		}
	}
	if out := s.errorOutput(); strings.Contains(out, "ghu_") {
		t.Errorf("the token is in the server's log: %s", out)
	}
}

// Why this test exists: the owner gives an agent repositories as
// owner/name or patterns, by the rules the Git integration reads them
// by, and the page and the docs describe those rules once for both. A
// server that matched differently would let an agent read or write a
// repository its access does not name, or refuse one it does. Each
// case is held to gitrepo.Match itself, so the two cannot drift; a
// refused call never runs gh.
func TestAccessHoldsCallsToTheRepositoryPatterns(t *testing.T) {
	s := startServer(t, tokens("ghu_token00000"))
	s.setGH(ghRule{Argv: []string{"repo", "view"}, Stdout: repoJSON})
	cases := []struct {
		patterns []string
		repo     string
	}{
		{[]string{"acme/*"}, "acme/widgets"},
		{[]string{"acme/*"}, "ACME/Widgets"},
		{[]string{"acme/*"}, "other/widgets"},
		{[]string{"acme/widgets"}, "acme/widgets2"},
		{[]string{"acme/widgets"}, "Acme/Widgets"},
		{[]string{"*/widgets"}, "acme/widgets"},
		{[]string{"acme/w*s"}, "acme/widgets"},
		{[]string{"acme/w*s"}, "acme/wid"},
		{[]string{"Acme/Widgets.git"}, "acme/widgets"},
		{[]string{"/acme/widgets/"}, "acme/widgets"},
		{[]string{"acme/?idgets"}, "acme/widgets"},
		{[]string{"acme/[w]idgets"}, "acme/widgets"},
		{[]string{`acme/\widgets`}, "acme/widgets"},
		{[]string{"*"}, "acme/widgets"},
		{[]string{"*/*"}, "acme/widgets"},
		{[]string{"acme*"}, "acme/widgets"},
		{[]string{"acme/*.js"}, "acme/site.js"},
		{[]string{"acme/*.js"}, "acme/site.jsx"},
		{[]string{}, "acme/widgets"},
		{[]string{"other/x", "acme/*"}, "acme/widgets"},
	}
	ran := 0
	for _, c := range cases {
		access := map[string]any{"repositories": c.patterns, "write": true}
		want := gitrepo.Match(c.patterns, strings.ToLower(c.repo))
		text, isErr := s.call("get_repository", map[string]any{"repository": c.repo}, access)
		if want {
			ran++
			if isErr {
				t.Errorf("%v %s: refused: %s", c.patterns, c.repo, text)
			}
		} else if !isErr || text != "Your access to GitHub does not include "+c.repo+"; ask the owner." {
			t.Errorf("%v %s: %v %s", c.patterns, c.repo, isErr, text)
		}
		if got := len(s.ghCalls()); got != ran {
			t.Fatalf("%v %s: gh ran %d times, want %d", c.patterns, c.repo, got, ran)
		}
	}

	// A repository not written as owner/name is refused before any
	// pattern is read: gh would read a value starting with - as an
	// option, and a path of more segments is not a GitHub repository.
	for _, repo := range []string{"acme", "acme/widgets/extra", "-R/x", "acme/..", "acme/wid gets", "acme/widgets;id", "acme/widgets\n", ""} {
		text, isErr := s.call("get_repository", map[string]any{"repository": repo}, map[string]any{"repositories": []string{"*/*"}})
		if !isErr || (!strings.Contains(text, "is not owner/name") && !strings.Contains(text, "Name the repository")) {
			t.Errorf("%q: %v %s", repo, isErr, text)
		}
	}
	if got := len(s.ghCalls()); got != ran {
		t.Errorf("gh ran for a repository that is not owner/name")
	}

	// Spaces and tabs around a repository are a slip a model makes, and
	// the Git integration takes them: the repository is the one inside.
	text, isErr := s.call("get_repository", map[string]any{"repository": " acme/widgets\t"}, map[string]any{"repositories": []string{"acme/*"}})
	calls := s.ghCalls()
	if isErr || len(calls) != ran+1 || calls[ran].Argv[2] != "acme/widgets" {
		t.Errorf("spaces around it: %v %s, gh runs %d", isErr, text, len(calls))
	}
}

// writeTools are the actions that create something on GitHub.
var writeTools = []string{"create_issue", "comment", "create_pull_request"}

// Why this test exists: an agent whose access does not allow writing
// must neither see the actions that create issues, comments and pull
// requests nor get through by calling one by name, and its call must
// not even fetch the token. The owner's list (Test connection, the
// check at install) holds every action the manifest declares, or the
// check refuses the bundle; whoami is the owner's alone.
func TestWriteActionsNeedWrite(t *testing.T) {
	s := startServer(t, tokens("ghu_token00000"))
	m := readBundle(t).Manifest
	var declared []string
	for _, tool := range m.Tools {
		declared = append(declared, tool.Name)
	}
	if got := s.toolNames(owner); !slices.Equal(got, declared) {
		t.Errorf("owner's list %v, want %v", got, declared)
	}
	reader := map[string]any{"repositories": []string{"acme/*"}, "write": false}
	readOnly := s.toolNames(reader)
	for _, name := range append(slices.Clone(writeTools), "whoami") {
		if slices.Contains(readOnly, name) {
			t.Errorf("a read-only agent's list holds %s", name)
		}
	}
	if !slices.Contains(readOnly, "list_issues") || !slices.Contains(readOnly, "compare") {
		t.Errorf("a read-only agent's list %v", readOnly)
	}
	// Access set before a version had the switch holds none: off.
	if got := s.toolNames(map[string]any{"repositories": []string{"acme/*"}}); slices.Contains(got, "create_issue") {
		t.Errorf("access without the switch lists create_issue")
	}
	writer := s.toolNames(map[string]any{"repositories": []string{"acme/*"}, "write": true})
	for _, name := range writeTools {
		if !slices.Contains(writer, name) {
			t.Errorf("a writing agent's list lacks %s", name)
		}
	}
	for _, name := range writeTools {
		text, isErr := s.call(name, map[string]any{"repository": "acme/widgets", "title": "t", "number": 1, "body": "b", "head": "h", "base": "main"}, reader)
		if !isErr || text != "Your access to GitHub does not include creating issues, comments or pull requests; ask the owner." {
			t.Errorf("%s: %v %s", name, isErr, text)
		}
	}
	if n, runs := s.credentialCount(), len(s.ghCalls()); n != 0 || runs != 0 {
		t.Errorf("refused calls asked for the token %d times and ran gh %d times", n, runs)
	}
}

// Why this test exists: whoami is the manifest's kivali.verify, what
// Test connection and the check after a sign-in show the owner. Its
// words are how the owner knows which GitHub account Kivali signed in
// as.
func TestWhoamiNamesTheAccount(t *testing.T) {
	s := startServer(t, tokens("ghu_token00000"))
	s.setGH(ghRule{Argv: []string{"api", "user"}, Stdout: `{"login":"octocat","id":1}`})
	text, isErr := s.call("whoami", map[string]any{}, owner)
	if isErr || text != "Connected as @octocat" {
		t.Errorf("%v %s", isErr, text)
	}
	if calls := s.ghCalls(); len(calls) != 1 || !slices.Equal(calls[0].Argv, []string{"api", "user"}) {
		t.Errorf("gh runs %+v", calls)
	}
}

// Why this test exists: while the connection needs the owner to sign
// in, or GitHub did not answer core's renewal, core answers
// kivali/credential with an error rather than a token. The agent must
// read that in words that say what to do (ask the owner), not a
// failure it cannot act on, and gh must not run with no token, which
// would make it try a login of its own.
func TestCredentialRefusalsReachTheAgentInWords(t *testing.T) {
	for _, c := range []struct {
		code int
		want string
	}{
		{-32001, "GitHub is not connected; ask the owner."},
		{-32002, "GitHub's connection needs the owner to sign in again; ask the owner."},
		{-32003, "GitHub did not answer when Kivali renewed the connection's sign-in; try again later, or ask the owner if it goes on."},
	} {
		s := startServer(t, func(int) (any, *rpcError) {
			return nil, &rpcError{Code: c.code, Message: "refused"}
		})
		s.setGH(ghRule{Argv: []string{}, Stdout: "{}"})
		for _, call := range []struct {
			name   string
			args   map[string]any
			access map[string]any
		}{
			{"whoami", map[string]any{}, owner},
			{"list_issues", map[string]any{"repository": "acme/widgets"}, map[string]any{"repositories": []string{"acme/*"}}},
			// It lists owner by owner, skipping one gh cannot list; a
			// sign-in error still refuses the whole call.
			{"list_repositories", map[string]any{}, map[string]any{"repositories": []string{"acme/*"}}},
		} {
			text, isErr := s.call(call.name, call.args, call.access)
			if !isErr || text != c.want {
				t.Errorf("%d %s: %v %s", c.code, call.name, isErr, text)
			}
		}
		if runs := len(s.ghCalls()); runs != 0 {
			t.Errorf("%d: gh ran %d times", c.code, runs)
		}
	}

	// An answer with no token in env is no connection either.
	s := startServer(t, func(int) (any, *rpcError) {
		return map[string]any{"env": map[string]string{}, "fields": map[string]string{"client_id": "Iv23liTest"}}, nil
	})
	if text, isErr := s.call("whoami", map[string]any{}, owner); !isErr || text != "GitHub is not connected; ask the owner." {
		t.Errorf("no token: %v %s", isErr, text)
	}
}

// Why this test exists: gh exits non-zero for a repository that does
// not exist, a permission the GitHub App lacks, or an app not installed
// on the repository's owner. The agent must read GitHub's reason, from
// gh's error output, marked as an error, or it reports a failed call as
// done.
func TestGhFailureIsAnErrorWithItsReason(t *testing.T) {
	s := startServer(t, tokens("ghu_token00000"))
	s.setGH(ghRule{
		Argv:   []string{"issue", "list"},
		Stderr: "GraphQL: Could not resolve to a Repository with the name 'acme/nope'. (repository)\n",
		Code:   1,
	})
	text, isErr := s.call("list_issues", map[string]any{"repository": "acme/nope"}, map[string]any{"repositories": []string{"acme/*"}})
	if !isErr || text != "gh exited with status 1: GraphQL: Could not resolve to a Repository with the name 'acme/nope'. (repository)" {
		t.Errorf("%v %s", isErr, text)
	}
}

// Why this test exists: an issue's thread and a pull request's diff
// can run to megabytes, and an answer that size fills the agent's
// context with one call. The answer keeps the latest 20 comments, the
// first 20,000 characters of a body and 50,000 of a diff, and says
// when it cut something, so the agent knows there is more.
func TestLongAnswersAreClipped(t *testing.T) {
	s := startServer(t, tokens("ghu_token00000"))
	var comments []map[string]any
	for i := 1; i <= 25; i++ {
		comments = append(comments, map[string]any{"author": map[string]any{"login": "u"}, "body": "comment " + string(rune('a'+i-1))})
	}
	issue, _ := json.Marshal(map[string]any{"number": 7, "title": "Long", "body": strings.Repeat("x", 30000), "comments": comments})
	pr, _ := json.Marshal(map[string]any{"number": 8, "title": "Big", "body": "short"})
	s.setGH(
		ghRule{Argv: []string{"issue", "view"}, Stdout: string(issue)},
		ghRule{Argv: []string{"pr", "view"}, Stdout: string(pr)},
		ghRule{Argv: []string{"pr", "diff"}, Stdout: strings.Repeat("d", 60000)},
	)
	agent := map[string]any{"repositories": []string{"acme/*"}}

	text, isErr := s.call("get_issue", map[string]any{"repository": "acme/widgets", "number": 7}, agent)
	if isErr {
		t.Fatal(text)
	}
	var gotIssue struct {
		Body     string `json:"body"`
		Comments []struct {
			Body string `json:"body"`
		} `json:"comments"`
		Note string `json:"note"`
	}
	if err := json.Unmarshal([]byte(text), &gotIssue); err != nil {
		t.Fatal(err)
	}
	if len(gotIssue.Body) != 20000 || len(gotIssue.Comments) != 20 || gotIssue.Comments[0].Body != "comment f" || gotIssue.Comments[19].Body != "comment y" {
		t.Errorf("body %d, comments %d", len(gotIssue.Body), len(gotIssue.Comments))
	}
	if !strings.Contains(gotIssue.Note, "first 20000 characters") || !strings.Contains(gotIssue.Note, "latest 20 of its 25 comments") {
		t.Errorf("note %q", gotIssue.Note)
	}

	text, isErr = s.call("get_pull_request", map[string]any{"repository": "acme/widgets", "number": 8, "diff": true}, agent)
	if isErr {
		t.Fatal(text)
	}
	var gotPR struct {
		Body string `json:"body"`
		Diff string `json:"diff"`
		Note string `json:"note"`
	}
	if err := json.Unmarshal([]byte(text), &gotPR); err != nil {
		t.Fatal(err)
	}
	if gotPR.Body != "short" || len(gotPR.Diff) != 50000 || !strings.Contains(gotPR.Note, "diff is cut to its first 50000 characters") {
		t.Errorf("body %q, diff %d, note %q", gotPR.Body, len(gotPR.Diff), gotPR.Note)
	}
	// Without diff, gh pr diff does not run.
	before := len(s.ghCalls())
	if text, isErr = s.call("get_pull_request", map[string]any{"repository": "acme/widgets", "number": 8}, agent); isErr || strings.Contains(text, `"diff"`) {
		t.Errorf("%v %s", isErr, text)
	}
	if got := len(s.ghCalls()) - before; got != 1 {
		t.Errorf("gh ran %d times without diff", got)
	}
}

// Why this test exists: an issue and a pull request share their
// numbers on GitHub, but gh comments on each with its own command, and
// the wrong one fails. The server asks GitHub which the number is
// first. A comment's text goes to gh on its input, never on the
// command line, where a body starting with - would read as an option.
func TestCommentFindsWhetherTheNumberIsAPullRequest(t *testing.T) {
	s := startServer(t, tokens("ghu_token00000"))
	s.setGH(
		ghRule{Argv: []string{"api", "repos/acme/widgets/issues/7"}, Stdout: `{"number":7,"pull_request":{"url":"https://api.github.com/repos/acme/widgets/pulls/7"}}`},
		ghRule{Argv: []string{"api", "repos/acme/widgets/issues/8"}, Stdout: `{"number":8}`},
		ghRule{Argv: []string{"pr", "comment"}, Stdout: "https://github.com/acme/widgets/pull/7#issuecomment-1\n"},
		ghRule{Argv: []string{"issue", "comment"}, Stdout: "https://github.com/acme/widgets/issues/8#issuecomment-2\n"},
	)
	writer := map[string]any{"repositories": []string{"acme/widgets"}, "write": true}
	text, isErr := s.call("comment", map[string]any{"repository": "acme/widgets", "number": 7, "body": "--looks good"}, writer)
	if isErr || text != "https://github.com/acme/widgets/pull/7#issuecomment-1" {
		t.Errorf("pull request: %v %s", isErr, text)
	}
	text, isErr = s.call("comment", map[string]any{"repository": "acme/widgets", "number": 8, "body": "On it."}, writer)
	if isErr || text != "https://github.com/acme/widgets/issues/8#issuecomment-2" {
		t.Errorf("issue: %v %s", isErr, text)
	}
	calls := s.ghCalls()
	if len(calls) != 4 {
		t.Fatalf("gh ran %d times", len(calls))
	}
	if !slices.Equal(calls[1].Argv, []string{"pr", "comment", "7", "--repo=acme/widgets", "--body-file=-"}) || calls[1].Stdin != "--looks good" {
		t.Errorf("pr comment: %q %q", calls[1].Argv, calls[1].Stdin)
	}
	if !slices.Equal(calls[3].Argv, []string{"issue", "comment", "8", "--repo=acme/widgets", "--body-file=-"}) || calls[3].Stdin != "On it." {
		t.Errorf("issue comment: %q %q", calls[3].Argv, calls[3].Stdin)
	}
}

// Why this test exists: create_pull_request is the one action that
// names branches, and the one an agent pairs with a Git push. Its
// branches reach gh as values of their flags, its body on gh's input,
// and a branch that gh would read as an option, or that is not a
// branch name, is refused before gh runs.
func TestCreatePullRequest(t *testing.T) {
	s := startServer(t, tokens("ghu_token00000"))
	s.setGH(ghRule{Argv: []string{"pr", "create"}, Stdout: "https://github.com/acme/widgets/pull/9\n"})
	writer := map[string]any{"repositories": []string{"acme/*"}, "write": true}
	text, isErr := s.call("create_pull_request", map[string]any{
		"repository": "acme/widgets", "title": "Fix the build", "head": "agent/fix-1", "base": "main", "body": "Fixes #3.", "draft": true,
	}, writer)
	if isErr || text != "https://github.com/acme/widgets/pull/9" {
		t.Fatalf("%v %s", isErr, text)
	}
	calls := s.ghCalls()
	want := []string{"pr", "create", "--repo=acme/widgets", "--head=agent/fix-1", "--base=main", "--title=Fix the build", "--body-file=-", "--draft"}
	if len(calls) != 1 || !slices.Equal(calls[0].Argv, want) || calls[0].Stdin != "Fixes #3." {
		t.Errorf("gh runs %+v", calls)
	}
	for _, head := range []string{"-x", "a..b", "a b", "owner:branch", "refs/heads/", "main\n", ""} {
		text, isErr := s.call("create_pull_request", map[string]any{"repository": "acme/widgets", "title": "t", "head": head, "base": "main"}, writer)
		if !isErr || !strings.Contains(text, "branch") {
			t.Errorf("head %q: %v %s", head, isErr, text)
		}
	}
	if len(s.ghCalls()) != 1 {
		t.Error("gh ran for a refused branch")
	}
}

// Why this test exists: list_repositories is how an agent learns which
// repositories it may use. It asks GitHub for each owner its patterns
// name and keeps only what the patterns match, so an agent given
// acme/w* is not shown acme/api, which its calls would be refused.
func TestListRepositoriesKeepsToThePatterns(t *testing.T) {
	s := startServer(t, tokens("ghu_token00000"))
	s.setGH(
		ghRule{Argv: []string{"repo", "list", "acme"}, Stdout: `[{"nameWithOwner":"acme/widgets","isPrivate":true,"updatedAt":"2026-10-01T00:00:00Z","description":"The widgets"},{"nameWithOwner":"acme/web","isPrivate":false,"updatedAt":"2026-10-02T00:00:00Z"},{"nameWithOwner":"acme/api","isPrivate":true,"updatedAt":"2026-10-03T00:00:00Z"}]`},
		ghRule{Argv: []string{"repo", "list", "other"}, Stdout: `[{"nameWithOwner":"Other/Tool","isPrivate":false,"updatedAt":"2026-10-04T00:00:00Z"},{"nameWithOwner":"other/x","isPrivate":false,"updatedAt":"2026-10-05T00:00:00Z"}]`},
	)
	text, isErr := s.call("list_repositories", map[string]any{}, map[string]any{"repositories": []string{"acme/w*", "other/tool", "*/everything"}})
	want := "acme/widgets (private, updated 2026-10-01T00:00:00Z): The widgets\nacme/web (public, updated 2026-10-02T00:00:00Z)\nOther/Tool (public, updated 2026-10-04T00:00:00Z)"
	if isErr || text != want {
		t.Errorf("%v\n%s", isErr, text)
	}
	calls := s.ghCalls()
	if len(calls) != 2 || !slices.Equal(calls[0].Argv, []string{"repo", "list", "acme", "--limit=100", "--json=nameWithOwner,description,isPrivate,updatedAt"}) {
		t.Errorf("gh runs %+v", calls)
	}
}

// Why this test exists: gh turns list_issues' search into a GitHub
// search with repo:<the repository> added, and GitHub ORs repo:
// qualifiers and honours org:, user: and owner:, so a search naming
// another place would hand an agent the issues of any repository the
// account and the app can see, past its access. Such a search is
// refused before gh runs; and whatever gh answers, only the
// repository's own issues reach the agent. list_pull_requests takes
// no search at all.
func TestListIssuesStaysInTheRepository(t *testing.T) {
	s := startServer(t, tokens("ghu_token00000"))
	agent := map[string]any{"repositories": []string{"acme/widgets"}}
	for _, search := range []string{"repo:acme/secret", "crash org:acme", "-repo:acme/widgets repo:x/y", "(repo:x/y)", `"user:someone"`, "OWNER:acme", "bug\tRepo:x/y"} {
		text, isErr := s.call("list_issues", map[string]any{"repository": "acme/widgets", "search": search}, agent)
		if !isErr || text != "search cannot name repositories, organizations or users; the repository argument does that." {
			t.Errorf("%q: %v %s", search, isErr, text)
		}
	}
	if runs := len(s.ghCalls()); runs != 0 {
		t.Fatalf("gh ran %d times for refused searches", runs)
	}

	s.setGH(ghRule{Argv: []string{"issue", "list"}, Stdout: `[` +
		`{"number":1,"title":"Ours","url":"https://github.com/acme/widgets/issues/1"},` +
		`{"number":2,"title":"Ours, capitals","url":"https://github.com/ACME/Widgets/issues/2"},` +
		`{"number":3,"title":"Theirs","url":"https://github.com/acme/secret/issues/3"},` +
		`{"number":4,"title":"Prefix","url":"https://github.com/acme/widgets-private/issues/4"}]`})
	text, isErr := s.call("list_issues", map[string]any{"repository": "acme/widgets", "search": "crash in:title"}, agent)
	if isErr {
		t.Fatal(text)
	}
	var got []struct {
		Number int `json:"number"`
	}
	if err := json.Unmarshal([]byte(text), &got); err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].Number != 1 || got[1].Number != 2 {
		t.Errorf("kept %s", text)
	}
	if calls := s.ghCalls(); len(calls) != 1 || !slices.Contains(calls[0].Argv, "--search=crash in:title") {
		t.Errorf("gh runs %+v", calls)
	}

	params := map[string]any{"_meta": meta(agent)}
	var list struct {
		Tools []struct {
			Name        string `json:"name"`
			InputSchema struct {
				Properties map[string]any `json:"properties"`
			} `json:"inputSchema"`
		} `json:"tools"`
	}
	if err := json.Unmarshal(s.request("tools/list", params), &list); err != nil {
		t.Fatal(err)
	}
	for _, tool := range list.Tools {
		if _, ok := tool.InputSchema.Properties["search"]; ok && tool.Name != "list_issues" {
			t.Errorf("%s takes a search", tool.Name)
		}
	}
}

// Why this test exists: an answer gh gives can be any size, and one
// that fills the agent's context in a call leaves it nothing to work
// with. Every answer is cut at 60,000 characters with a note, whatever
// the action's own limits.
func TestEveryAnswerIsBounded(t *testing.T) {
	s := startServer(t, tokens("ghu_token00000"))
	s.setGH(ghRule{Argv: []string{"repo", "view"}, Stdout: strings.Repeat("r", 70000)})
	text, isErr := s.call("get_repository", map[string]any{"repository": "acme/widgets"}, owner)
	note := "\n[The answer is cut to its first 60000 characters.]"
	if isErr || text != strings.Repeat("r", 60000)+note {
		t.Errorf("%v, %d characters, ends %q", isErr, len(text), text[max(0, len(text)-60):])
	}
}

// Why this test exists: an agent's access may name several owners, and
// one of them can be a wrong name or an account the GitHub App is not
// installed on. The repositories of the others must still be listed,
// with the owner gh could not list named, rather than the whole list
// failing on one.
func TestListRepositoriesSkipsAnOwnerItCannotList(t *testing.T) {
	s := startServer(t, tokens("ghu_token00000"))
	s.setGH(
		ghRule{Argv: []string{"repo", "list", "acme"}, Stdout: `[{"nameWithOwner":"acme/widgets","isPrivate":true,"updatedAt":"2026-10-01T00:00:00Z"}]`},
		ghRule{Argv: []string{"repo", "list", "nobody"}, Stderr: "GraphQL: Could not resolve to a RepositoryOwner with the login of 'nobody'.\n", Code: 1},
	)
	text, isErr := s.call("list_repositories", map[string]any{}, map[string]any{"repositories": []string{"nobody/*", "acme/*"}})
	want := "acme/widgets (private, updated 2026-10-01T00:00:00Z)\nGitHub did not list the repositories of nobody: the name may be wrong, or the GitHub App not installed there."
	if isErr || text != want {
		t.Errorf("%v\n%s", isErr, text)
	}
}

// Why this test exists: GitHub answers 401 when the sign-in no longer
// works and 403 when the GitHub App lacks a permission or is not
// installed on the repository's owner. Neither is the agent's to fix,
// so its words end by telling it to ask the owner.
func TestGitHubRefusalsTellTheAgentToAskTheOwner(t *testing.T) {
	s := startServer(t, tokens("ghu_token00000"))
	agent := map[string]any{"repositories": []string{"acme/*"}}
	for _, c := range []struct{ stderr, want string }{
		{"HTTP 401: Bad credentials (https://api.github.com/user)", "GitHub refused the connection's sign-in (HTTP 401): HTTP 401: Bad credentials (https://api.github.com/user). Ask the owner."},
		{"HTTP 403: Resource not accessible by integration (https://api.github.com/repos/acme/widgets/branches)", "GitHub refused this (HTTP 403): HTTP 403: Resource not accessible by integration (https://api.github.com/repos/acme/widgets/branches). The GitHub App may lack the permission, or not be installed on the repository's owner; ask the owner."},
	} {
		s.setGH(ghRule{Argv: []string{"api"}, Stderr: c.stderr + "\n", Code: 1})
		text, isErr := s.call("list_branches", map[string]any{"repository": "acme/widgets"}, agent)
		if !isErr || text != c.want {
			t.Errorf("%v %s", isErr, text)
		}
	}
}

// Why this test exists: list_branches and compare call GitHub's REST
// API through gh api, with paths the server builds from the agent's
// arguments. A path built wrong reads another resource or none; these
// pin both, and what each answer keeps (no patches).
func TestBranchesAndCompareCallTheirAPIPaths(t *testing.T) {
	s := startServer(t, tokens("ghu_token00000"))
	s.setGH(
		ghRule{Argv: []string{"api", "repos/acme/widgets/branches?per_page=5"}, Stdout: `[{"name":"main","commit":{"sha":"aaa"}},{"name":"agent/fix-1","commit":{"sha":"bbb"}}]`},
		ghRule{Argv: []string{"api", "repos/acme/widgets/compare/main...agent/fix-1"}, Stdout: `{"status":"ahead","ahead_by":1,"behind_by":0,` +
			`"commits":[{"sha":"bbb","commit":{"message":"Fix the build\n\nLonger text","author":{"name":"Dana"}},"author":{"login":"dana"}}],` +
			`"files":[{"filename":"a.go","status":"modified","additions":3,"deletions":1,"patch":"@@ a patch"}]}`},
	)
	agent := map[string]any{"repositories": []string{"acme/*"}}
	text, isErr := s.call("list_branches", map[string]any{"repository": "acme/widgets", "limit": 5}, agent)
	if isErr || text != "main aaa\nagent/fix-1 bbb" {
		t.Errorf("branches: %v %s", isErr, text)
	}
	text, isErr = s.call("compare", map[string]any{"repository": "acme/widgets", "base": "main", "head": "agent/fix-1"}, agent)
	want := `{"status":"ahead","ahead_by":1,"behind_by":0,"commits":[{"sha":"bbb","title":"Fix the build","author":"dana"}],"files":[{"name":"a.go","status":"modified","additions":3,"deletions":1}]}`
	if isErr || text != want {
		t.Errorf("compare: %v %s", isErr, text)
	}
	calls := s.ghCalls()
	if len(calls) != 2 || !slices.Equal(calls[0].Argv, []string{"api", "repos/acme/widgets/branches?per_page=5"}) ||
		!slices.Equal(calls[1].Argv, []string{"api", "repos/acme/widgets/compare/main...agent/fix-1"}) {
		t.Errorf("gh runs %+v", calls)
	}
}
