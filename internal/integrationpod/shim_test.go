package integrationpod

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kivali-ai/kivali/internal/clock"
	"github.com/kivali-ai/kivali/internal/egresscred"
	"github.com/kivali-ai/kivali/internal/integration"
	"github.com/kivali-ai/kivali/internal/integrationpod/fakeintegration"
)

// TestMain runs the test binary as the fake integration's server when
// the shim starts it: the test manifests put the marker in the server's
// env, the only environment the shim passes on. Run with InstallArgs
// and installChildEnv set, it is the shim's install child instead
// (installChildMain).
func TestMain(m *testing.M) {
	if os.Getenv(fakeintegration.EnvMarker) == "1" {
		os.Exit(fakeintegration.Main())
	}
	if mode := os.Getenv(installChildEnv); mode != "" && len(os.Args) > len(InstallArgs) &&
		os.Args[1] == InstallArgs[0] && os.Args[2] == InstallArgs[1] {
		os.Exit(installChildMain(mode, os.Args[3:]))
	}
	os.Exit(m.Run())
}

// failsafe bounds a wait on the shim or its server, which are real
// processes; a passing test never reaches it.
const failsafe = 20 * time.Second

const testToken = "pod-token-0123456789"

// fakeCore stands in for core on a temp Unix socket: the events route,
// whose streams the test drives, and the rpc route, which answers the
// connection's values and records every post.
type fakeCore struct {
	t        *testing.T
	sock     string
	manifest *integration.Manifest

	mu sync.Mutex
	// values is the connection, by user_config key; nil is none.
	values map[string]string
	// steps, when set, records each events stream opened ("events") and
	// each log line posted ("log: <message>"), in the order they came.
	steps *steps
	// credCode, when set, answers the connection's values with this
	// error code.
	credCode int
	// refuse is how many event streams to answer 403.
	refuse int
	// bodies is every post as it arrived.
	bodies []string

	streams chan *fakeStream
	refused chan struct{}
	posts   chan Message
}

// fakeStream is one events stream the shim opened.
type fakeStream struct {
	hdr http.Header
	out chan string
	end chan struct{}
}

func (s *fakeStream) send(typ EventType, data string) {
	s.out <- fmt.Sprintf("event: %s\ndata: %s\n\n", typ, data)
}

func newFakeCore(t *testing.T, m *integration.Manifest) *fakeCore {
	t.Helper()
	dir, err := os.MkdirTemp("", "ish")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	c := &fakeCore{
		t: t, sock: filepath.Join(dir, SocketName), manifest: m,
		streams: make(chan *fakeStream, 4),
		refused: make(chan struct{}, 4),
		posts:   make(chan Message, 256),
	}
	ln, err := net.Listen("unix", c.sock)
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/integration/"+m.Name+"/events", c.events)
	mux.HandleFunc("POST /v1/integration/"+m.Name+"/rpc", c.rpc)
	srv := &http.Server{Handler: mux}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })
	return c
}

func (c *fakeCore) setValues(v map[string]string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.values = v
}

func (c *fakeCore) setSteps(s *steps) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.steps = s
}

func (c *fakeCore) record(step string) {
	c.mu.Lock()
	s := c.steps
	c.mu.Unlock()
	s.add(step)
}

func (c *fakeCore) events(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get(TokenHeader) != testToken {
		http.Error(w, "bad token", http.StatusForbidden)
		return
	}
	c.mu.Lock()
	refuse := c.refuse > 0
	if refuse {
		c.refuse--
	}
	c.mu.Unlock()
	if refuse {
		http.Error(w, "no integration is waiting for this connection", http.StatusForbidden)
		c.refused <- struct{}{}
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.WriteHeader(http.StatusOK)
	w.(http.Flusher).Flush()
	st := &fakeStream{hdr: r.Header.Clone(), out: make(chan string, 16), end: make(chan struct{})}
	c.record("events")
	c.streams <- st
	for {
		select {
		case f := <-st.out:
			_, _ = io.WriteString(w, f)
			w.(http.Flusher).Flush()
		case <-st.end:
			return
		case <-r.Context().Done():
			return
		}
	}
}

func (c *fakeCore) rpc(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get(TokenHeader) != testToken {
		http.Error(w, "bad token", http.StatusForbidden)
		return
	}
	body, _ := io.ReadAll(r.Body)
	var m Message
	if err := json.Unmarshal(body, &m); err != nil {
		http.Error(w, "not JSON", http.StatusBadRequest)
		return
	}
	c.mu.Lock()
	c.bodies = append(c.bodies, string(body))
	values, credCode := c.values, c.credCode
	c.mu.Unlock()
	if p, ok := postLog(m); ok {
		c.record("log: " + p.Message)
	}
	c.posts <- m
	if !m.IsRequest() {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	var out []byte
	switch {
	case m.Method == MethodCredential && credCode != 0:
		out, _ = Response(m.ID, nil, &RPCError{Code: credCode, Message: "the connection needs the owner to sign in again"})
	case m.Method == MethodCredential && values == nil:
		out, _ = Response(m.ID, nil, &RPCError{Code: CodeNoConnection, Message: "no connection"})
	case m.Method == MethodCredential:
		out, _ = Response(m.ID, Credential{Env: integration.ServerEnv(c.manifest, values), Fields: values}, nil)
	default:
		out, _ = Response(m.ID, nil, &RPCError{Code: CodeMethodNotFound, Message: "method not found"})
	}
	_, _ = w.Write(out)
}

// stream waits for the shim's next events stream.
func (c *fakeCore) stream() *fakeStream {
	c.t.Helper()
	select {
	case st := <-c.streams:
		return st
	case <-time.After(failsafe):
		c.t.Fatal("the shim opened no events stream")
		return nil
	}
}

// postLog is the params of a log message, or false.
func postLog(m Message) (LogParams, bool) {
	var p LogParams
	if m.Method != MethodLog || json.Unmarshal(m.Params, &p) != nil {
		return p, false
	}
	return p, true
}

// recorder keeps every post a test has read, so it can count them. The
// server's error output and the shim's own lines are posted from two
// goroutines, so a wait finds its post among those already read as
// well as those still to come.
type recorder struct {
	c    *fakeCore
	seen []Message
	// taken marks the posts a wait has returned.
	taken map[int]bool
}

// wait returns the first post not yet returned that matches pred,
// reading more until one does.
func (rc *recorder) wait(what string, pred func(Message) bool) Message {
	rc.c.t.Helper()
	if rc.taken == nil {
		rc.taken = map[int]bool{}
	}
	for i, m := range rc.seen {
		if !rc.taken[i] && pred(m) {
			rc.taken[i] = true
			return m
		}
	}
	for {
		select {
		case m := <-rc.c.posts:
			rc.seen = append(rc.seen, m)
			if pred(m) {
				rc.taken[len(rc.seen)-1] = true
				return m
			}
		case <-time.After(failsafe):
			rc.c.t.Fatalf("no post: %s; saw %d", what, len(rc.seen))
			return Message{}
		}
	}
}

func (rc *recorder) log(contains string) LogParams {
	rc.c.t.Helper()
	m := rc.wait("log "+contains, func(m Message) bool {
		p, ok := postLog(m)
		return ok && strings.Contains(p.Message, contains)
	})
	p, _ := postLog(m)
	return p
}

func (rc *recorder) response(id int) Message {
	rc.c.t.Helper()
	return rc.wait(fmt.Sprintf("response %d", id), func(m Message) bool { return m.IsResponse() && string(m.ID) == fmt.Sprint(id) })
}

func (rc *recorder) count(pred func(Message) bool) int {
	n := 0
	for _, m := range rc.seen {
		if pred(m) {
			n++
		}
	}
	return n
}

// toolText is a tools/call response's first text.
func toolText(t *testing.T, m Message) string {
	t.Helper()
	var res struct {
		Content []struct {
			Text string `json:"text"`
		} `json:"content"`
	}
	if m.Error != nil || json.Unmarshal(m.Result, &res) != nil || len(res.Content) == 0 {
		t.Fatalf("not a tool result: %+v", m)
	}
	return res.Content[0].Text
}

// testManifest is the fake bundle's manifest: python3, as the bundle
// check requires, run under the shim's command override. env joins the
// marker that makes the test binary the fake server.
func testManifest(env map[string]string) *integration.Manifest {
	all := map[string]string{fakeintegration.EnvMarker: "1"}
	for k, v := range env {
		all[k] = v
	}
	return &integration.Manifest{
		ManifestVersion: "0.3",
		Name:            "fake",
		Version:         "1.0.0",
		DisplayName:     "Fake",
		Server: integration.Server{Type: "python", EntryPoint: "server/main.py", MCPConfig: integration.MCPConfig{
			Command: "python3", Args: []string{"server/main.py"}, Env: all,
		}},
		UserConfig: integration.UserConfig{
			{Key: "token", Type: "string", Title: "Token", Sensitive: true},
			{Key: "ask", Type: "string", Title: "Ask"},
		},
		Tools:  []integration.Tool{{Name: "echo"}, {Name: "whoami"}, {Name: "list_colors"}},
		Kivali: integration.Kivali{Connection: integration.Connection{Method: "token"}, Hosts: []string{"api.example.com"}},
	}
}

// shimRun is one Run of a shim: done closes when it returns, with its
// status in code.
type shimRun struct {
	done chan struct{}
	code int
}

// runShim writes the bundle for m and runs a shim on it against c.
// cancel is SIGTERM.
func runShim(t *testing.T, c *fakeCore, m *integration.Manifest) (*clock.Fake, *shimRun, context.CancelFunc) {
	t.Helper()
	bundle := t.TempDir()
	raw, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bundle, integration.ManifestName), raw, 0o644); err != nil {
		t.Fatal(err)
	}
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	return runShimIn(t, c, bundle, m, exe)
}

// runShimWith is runShim with set applied to the shim before it runs.
func runShimWith(t *testing.T, c *fakeCore, m *integration.Manifest, set func(*Shim)) (*clock.Fake, *shimRun, context.CancelFunc) {
	t.Helper()
	bundle := t.TempDir()
	raw, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bundle, integration.ManifestName), raw, 0o644); err != nil {
		t.Fatal(err)
	}
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	return runShimIn(t, c, bundle, m, exe, set)
}

// runShimIn runs a shim on the bundle in dir, built for m's name and
// version, against c, with command replacing mcp_config.command when
// it is not empty.
func runShimIn(t *testing.T, c *fakeCore, bundle string, m *integration.Manifest, command string, set ...func(*Shim)) (*clock.Fake, *shimRun, context.CancelFunc) {
	t.Helper()
	clk := clock.NewFake()
	ctx, cancel := context.WithCancel(context.Background())
	run := &shimRun{done: make(chan struct{})}
	shim := &Shim{
		Name: m.Name, Version: m.Version, Token: testToken,
		CoreUDS: c.sock, Bundle: bundle,
		ProxyURL: "http://10.43.0.12:3128",
		Command:  command,
		Path:     os.Getenv("PATH"),
		Lang:     "C.UTF-8",
		Clock:    clk,
		Logf:     t.Logf,
	}
	for _, f := range set {
		f(shim)
	}
	go func() {
		run.code = shim.Run(ctx)
		close(run.done)
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case <-run.done:
		case <-time.After(failsafe):
			t.Error("the shim did not stop")
		}
	})
	return clk, run, cancel
}

func exitOf(t *testing.T, run *shimRun) int {
	t.Helper()
	select {
	case <-run.done:
		return run.code
	case <-time.After(failsafe):
		t.Fatal("the shim did not exit")
		return -1
	}
}

// The shim end to end, against a fake core and the fake server. Its
// handshake names the version and a proxy hash; on start it asks for
// the connection's values and starts the server with them, in an
// environment of PATH, HOME, LANG, the manifest's env and the
// credentialed proxy and nothing else; the server's error output
// becomes log lines with the proxy credential scrubbed; core's requests
// reach the server and its responses come back; a start with the same
// values leaves the server running and a restart starts it with new
// ones; the server's own request for the values goes to core; shutdown
// ends it. A break anywhere in that chain is an integration that
// cannot be called, holds a stale token, or leaks its proxy credential
// to core.
func TestShimRelaysBetweenCoreAndServer(t *testing.T) {
	m := testManifest(map[string]string{
		"FAKE_TOKEN":          "${user_config.token}",
		"FAKE_ASK_CREDENTIAL": "${user_config.ask}",
		"FAKE_PRINT_ENV":      "1",
		"FAKE_PRINT_PROXY":    "1",
	})
	c := newFakeCore(t, m)
	c.setValues(map[string]string{"token": "s3cret-token"})
	_, done, _ := runShim(t, c, m)
	rc := &recorder{c: c}

	st := c.stream()
	if st.hdr.Get(VersionHeader) != "1.0.0" || !egresscred.ValidHash(st.hdr.Get(ProxyHashHeader)) {
		t.Fatalf("handshake headers = %v", st.hdr)
	}
	st.send(EventStart, "{}")
	rc.wait("the values", func(m Message) bool { return m.IsRequest() && m.Method == MethodCredential })
	if p := rc.log("fake-integration ready"); p.Level != "stderr" {
		t.Errorf("stderr line level = %q", p.Level)
	}
	env := rc.log("env: ")
	if want := "env: FAKE_ASK_CREDENTIAL,FAKE_PRINT_ENV,FAKE_PRINT_PROXY,FAKE_TOKEN,HOME,HTTPS_PROXY,HTTP_PROXY,KIVALI_FAKE_INTEGRATION,LANG,PATH,http_proxy,https_proxy"; env.Message != want {
		t.Errorf("server env = %q, want %q", env.Message, want)
	}
	if p := rc.log("proxy: "); p.Message != "proxy: "+egresscred.Placeholder {
		t.Errorf("proxy line = %q: the credential was not scrubbed", p.Message)
	}
	if p := rc.log("server started"); p.Level != "info" {
		t.Errorf("started level = %q", p.Level)
	}

	st.send(EventRPC, `{"jsonrpc":"2.0","id":7,"method":"tools/call","params":{"name":"whoami","arguments":{}}}`)
	if got := toolText(t, rc.response(7)); got != "fake account s3cret-token" {
		t.Errorf("whoami = %q", got)
	}
	st.send(EventRPC, `{"jsonrpc":"2.0","id":8,"method":"tools/call","params":{"name":"echo","arguments":{"x":1},"_meta":{"kivali":{"agent":"dana"}}}}`)
	if got := toolText(t, rc.response(8)); got != `{"arguments":{"x":1},"kivali":{"agent":"dana"}}` {
		t.Errorf("echo = %q", got)
	}

	// The same values: the server keeps running (no second start before
	// the next answer).
	st.send(EventStart, "{}")
	st.send(EventRPC, `{"jsonrpc":"2.0","id":9,"method":"ping"}`)
	rc.response(9)
	isStarted := func(m Message) bool { p, ok := postLog(m); return ok && p.Message == "server started" }
	if n := rc.count(isStarted); n != 1 {
		t.Errorf("a start with unchanged values started the server again (%d starts)", n)
	}

	// New values: restart, and the server asks core for them itself.
	c.setValues(map[string]string{"token": "s3cret-token", "ask": "1"})
	st.send(EventRestart, "{}")
	rc.log("server started")
	st.send(EventRPC, `{"jsonrpc":"2.0","id":10,"method":"tools/call","params":{"name":"whoami","arguments":{}}}`)
	asked := rc.wait("the server's own request", func(m Message) bool {
		var id string
		return m.Method == MethodCredential && json.Unmarshal(m.ID, &id) == nil && strings.HasPrefix(id, "fake-")
	})
	if !asked.IsRequest() {
		t.Errorf("relayed request = %+v", asked)
	}
	if got := toolText(t, rc.response(10)); got != "env: FAKE_ASK_CREDENTIAL,FAKE_PRINT_ENV,FAKE_PRINT_PROXY,FAKE_TOKEN,KIVALI_FAKE_INTEGRATION; fields: ask,token" {
		t.Errorf("whoami after restart = %q", got)
	}

	st.send(EventShutdown, "{}")
	if code := exitOf(t, done); code != 0 {
		t.Errorf("exit after shutdown = %d", code)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, b := range c.bodies {
		if strings.Contains(b, "@10.43.0.12") || strings.Contains(b, ProxyUser("fake")+":") {
			t.Errorf("core received the proxy credential: %s", b)
		}
	}
}

// A request the server makes that is not for the connection's values
// or the log (sampling, roots, elicitation) is answered by the shim as
// a method that does not exist, and never reaches core: an integration
// must not drive the model or read anything through Kivali. With no
// connection the server starts with empty values. This fails if the
// request is relayed or left unanswered, which would hang the call.
func TestShimAnswersOtherServerRequests(t *testing.T) {
	m := testManifest(map[string]string{"FAKE_ASK": "sampling/createMessage", "FAKE_TOKEN": "${user_config.token}"})
	c := newFakeCore(t, m)
	_, _, _ = runShim(t, c, m)
	rc := &recorder{c: c}
	st := c.stream()
	st.send(EventStart, "{}")
	rc.log("server started")
	st.send(EventRPC, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"whoami","arguments":{}}}`)
	if got := toolText(t, rc.response(1)); got != fmt.Sprintf("asked sampling/createMessage: %d", CodeMethodNotFound) {
		t.Errorf("whoami = %q", got)
	}
	if n := rc.count(func(m Message) bool { return m.Method == "sampling/createMessage" }); n != 0 {
		t.Errorf("the request reached core %d times", n)
	}
}

// Why this test exists: a connection made by signing in whose owner
// has not signed in (or must sign in again) answers the values with
// CodeSignInNeeded, and one whose vendor did not answer a due refresh
// with CodeVendorUnavailable. If the shim treated either as a failure,
// the server would never start and the pod would restart for ever, a
// vendor's outage among the causes; it starts the server with empty
// values, as with no connection, and the server asks for the token at
// call time.
func TestShimStartsTheServerWhenSignInIsNeeded(t *testing.T) {
	for _, code := range []int{CodeSignInNeeded, CodeVendorUnavailable} {
		t.Run(fmt.Sprint(code), func(t *testing.T) {
			m := testManifest(map[string]string{"FAKE_TOKEN": "${user_config.token}"})
			c := newFakeCore(t, m)
			c.mu.Lock()
			c.credCode = code
			c.mu.Unlock()
			_, _, _ = runShim(t, c, m)
			rc := &recorder{c: c}
			st := c.stream()
			st.send(EventStart, "{}")
			rc.log("server started")
			st.send(EventRPC, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"whoami","arguments":{}}}`)
			if got := toolText(t, rc.response(1)); got != "fake account " {
				t.Errorf("whoami = %q, want the server started with no token", got)
			}
		})
	}
}

// A refused stream is opened again after a wait on the clock that
// doubles from 200 ms; the shim never spins on a core that refuses it
// (a pod about to be replaced, or a core that restarted and has not
// taken it on yet). This fails if the retry does not wait, does not
// back off, or never comes.
func TestShimRetriesARefusedStream(t *testing.T) {
	m := testManifest(nil)
	c := newFakeCore(t, m)
	c.refuse = 2
	clk, done, cancel := runShim(t, c, m)
	for _, wait := range []time.Duration{200 * time.Millisecond, 400 * time.Millisecond} {
		select {
		case <-c.refused:
		case <-time.After(failsafe):
			t.Fatal("no refused attempt")
		}
		clk.BlockUntil(1)
		if d := clk.Deadlines(); len(d) != 1 || d[0].Sub(clk.Now()) != wait {
			t.Fatalf("next attempt at %v from now %v, want %v", d, clk.Now(), wait)
		}
		clk.Advance(wait)
	}
	c.stream()
	cancel()
	if code := exitOf(t, done); code != 0 {
		t.Errorf("exit on SIGTERM with no server = %d", code)
	}
}

// A server that does not answer initialize within 20 s on the clock is
// not started, and the shim exits non-zero so kubelet starts it again.
// This fails if the shim waits on a dead server for ever.
func TestShimHandshakeTimeout(t *testing.T) {
	m := testManifest(map[string]string{"FAKE_NO_INITIALIZE": "1"})
	c := newFakeCore(t, m)
	clk, done, _ := runShim(t, c, m)
	rc := &recorder{c: c}
	c.stream().send(EventStart, "{}")
	rc.log("fake-integration ready")
	clk.BlockUntil(1)
	clk.Advance(handshakeTimeout)
	if p := rc.log("the server did not start"); !strings.Contains(p.Message, "did not answer initialize") || p.Level != "error" {
		t.Errorf("log = %+v", p)
	}
	if code := exitOf(t, done); code != 1 {
		t.Errorf("exit = %d, want 1", code)
	}
}

// A line on the server's output that is not JSON-RPC ends the server
// and so the shim, non-zero: what follows a broken line cannot be
// trusted to be a response to anything. This fails if the line is
// skipped or relayed.
func TestShimEndsAServerThatWritesGarbage(t *testing.T) {
	m := testManifest(map[string]string{"FAKE_GARBAGE": "1"})
	c := newFakeCore(t, m)
	_, done, _ := runShim(t, c, m)
	rc := &recorder{c: c}
	c.stream().send(EventStart, "{}")
	if p := rc.log("not JSON-RPC"); p.Level != "error" {
		t.Errorf("log = %+v", p)
	}
	if code := exitOf(t, done); code == 0 {
		t.Error("exit 0 after the server was ended")
	}
}

// The shim exits when its server does, with the server's status, after
// logging it: kubelet then starts the pod's container again. This
// fails if the shim outlives its server or loses the status.
func TestShimExitsWithTheServersStatus(t *testing.T) {
	m := testManifest(map[string]string{"FAKE_EXIT": "3"})
	c := newFakeCore(t, m)
	_, done, _ := runShim(t, c, m)
	rc := &recorder{c: c}
	c.stream().send(EventStart, "{}")
	if p := rc.log("server exited"); p.Message != "server exited: exit status 3" {
		t.Errorf("log = %+v", p)
	}
	if code := exitOf(t, done); code != 3 {
		t.Errorf("exit = %d, want 3", code)
	}
}

// SIGTERM closes the server's input and the shim exits once it has
// gone, with its status. This fails if the shim leaves the server
// running or exits before it.
func TestShimStopsTheServerOnSIGTERM(t *testing.T) {
	m := testManifest(nil)
	c := newFakeCore(t, m)
	_, done, cancel := runShim(t, c, m)
	rc := &recorder{c: c}
	c.stream().send(EventStart, "{}")
	rc.log("server started")
	cancel()
	if code := exitOf(t, done); code != 0 {
		t.Errorf("exit = %d, want 0", code)
	}
}

// ReadSSE reads frames as core writes them, skipping heartbeats, and
// joins multi-line data. This fails if a heartbeat becomes an event or
// a frame is split.
func TestReadSSE(t *testing.T) {
	in := ": ok\n\nevent: start\ndata: {}\n\n: hb\n\nevent: rpc\ndata: {\"a\":1}\ndata: x\n\n"
	out := make(chan Event, 4)
	if err := ReadSSE(strings.NewReader(in), out, nil); err != nil {
		t.Fatal(err)
	}
	close(out)
	var got []string
	for ev := range out {
		got = append(got, string(ev.Type)+"="+string(ev.Data))
	}
	if strings.Join(got, "|") != "start={}|rpc={\"a\":1}\nx" {
		t.Errorf("events = %q", got)
	}
}

// copyDir copies the regular files under src into a new temp
// directory and returns it.
func copyDir(t *testing.T, src string) string {
	t.Helper()
	dst := t.TempDir()
	err := filepath.WalkDir(src, func(p string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		rel, _ := filepath.Rel(src, p)
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		if err := os.MkdirAll(filepath.Join(dst, filepath.Dir(rel)), 0o755); err != nil {
			return err
		}
		return os.WriteFile(filepath.Join(dst, rel), b, 0o644)
	})
	if err != nil {
		t.Fatal(err)
	}
	return dst
}

// Why this test exists: every other shim test runs the Go fake behind
// a command override, which the pod never sets. The echo bundle is a
// real Python server started as a pod starts it, by mcp_config's own
// python3 in the bundle, with the connection's value in its env from
// core's answer. Its whoami names the token it was started with, and
// its echo returns the stamp a call carried and refuses a color the
// stamped access leaves out. Without this, a shim that only worked for
// the test binary would pass.
func TestShimRunsThePythonEchoBundle(t *testing.T) {
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("no python3 on PATH")
	}
	bundle := copyDir(t, filepath.Join("..", "integration", "testdata", "echo"))
	raw, err := os.ReadFile(filepath.Join(bundle, integration.ManifestName))
	if err != nil {
		t.Fatal(err)
	}
	m, err := integration.ParseManifest(raw)
	if err != nil {
		t.Fatal(err)
	}
	c := newFakeCore(t, m)
	c.setValues(map[string]string{"token": "s3cret-echo"})
	_, done, _ := runShimIn(t, c, bundle, m, "")
	rc := &recorder{c: c}
	st := c.stream()
	st.send(EventStart, "{}")
	rc.log("server started")

	st.send(EventRPC, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"whoami","arguments":{}}}`)
	if got := toolText(t, rc.response(1)); got != "echo as s3cret-echo" {
		t.Errorf("whoami = %q", got)
	}
	stamp := `"_meta":{"kivali":{"agent":"dana","access":{"colors":["red"]}}}`
	st.send(EventRPC, `{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"echo","arguments":{"color":"red"},`+stamp+`}}`)
	if got := toolText(t, rc.response(2)); got != `{"arguments": {"color": "red"}, "kivali": {"access": {"colors": ["red"]}, "agent": "dana"}}` {
		t.Errorf("echo red = %q", got)
	}
	st.send(EventRPC, `{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"echo","arguments":{"color":"blue"},`+stamp+`}}`)
	resp := rc.response(3)
	var res struct {
		IsError bool `json:"isError"`
	}
	_ = json.Unmarshal(resp.Result, &res)
	if got := toolText(t, resp); !res.IsError || got != "blue is not one of your colors (red)" {
		t.Errorf("echo blue = %q, isError %v", got, res.IsError)
	}

	st.send(EventShutdown, "{}")
	if code := exitOf(t, done); code != 0 {
		t.Errorf("exit after shutdown = %d", code)
	}
}
