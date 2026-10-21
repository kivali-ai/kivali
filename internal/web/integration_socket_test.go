package web

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kivali-ai/kivali/internal/clock"
	"github.com/kivali-ai/kivali/internal/integration/integrationtest"
	"github.com/kivali-ai/kivali/internal/integrationpod"
	"github.com/kivali-ai/kivali/internal/integrationpod/fakeintegration"
	"github.com/kivali-ai/kivali/internal/store"
	"github.com/kivali-ai/kivali/internal/web/apitypes"
)

// TestMain runs the test binary as the fake integration's server when a
// shim starts it: the end-to-end test's bundle puts the marker in the
// server's env, the only environment the shim passes on.
func TestMain(m *testing.M) {
	if os.Getenv(fakeintegration.EnvMarker) == "1" {
		os.Exit(fakeintegration.Main())
	}
	os.Exit(m.Run())
}

// integrationSocket starts srv's integration socket in a short temp
// directory and returns its path and an HTTP client over it.
func integrationSocket(t *testing.T, srv *Server) (string, *http.Client) {
	t.Helper()
	dir, err := os.MkdirTemp("", "isock")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	path := filepath.Join(dir, integrationpod.SocketName)
	stop, err := srv.StartIntegrationSocket(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(stop)
	client := &http.Client{Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		var d net.Dialer
		return d.DialContext(ctx, "unix", path)
	}}}
	return path, client
}

// The socket's two routes and what they refuse. The events stream
// needs the pod's token and the approved version and opens with start;
// a post needs the token, a log line is stored and answered 204, a
// request is answered with its response, and a response reaches the
// call waiting on it. A route that took a post without the token would
// let any pod on the node write another integration's log or read its
// connection.
func TestIntegrationSocketRoutes(t *testing.T) {
	srv := newTestServer(t)
	withIntegrationRegistry(srv, newFakeIntegrationProv())
	reg := srv.Integrations
	intInstalled(t, srv, "crm", "1.0.0")
	if err := reg.Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	e, _ := entryOf(reg, "crm")
	_, client := integrationSocket(t, srv)

	open := func(token, version, hash string) *http.Response {
		t.Helper()
		req, _ := http.NewRequest(http.MethodGet, "http://core/v1/integration/crm/events", nil)
		req.Header.Set(integrationpod.TokenHeader, token)
		req.Header.Set(integrationpod.VersionHeader, version)
		if hash != "" {
			req.Header.Set(integrationpod.ProxyHashHeader, hash)
		}
		resp, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		return resp
	}
	for _, bad := range []struct {
		token, version, hash string
		status               int
	}{
		{"", "1.0.0", "", http.StatusForbidden},
		{e.token, "2.0.0", "", http.StatusForbidden},
		{e.token, "1.0.0", "not-a-hash", http.StatusBadRequest},
	} {
		resp := open(bad.token, bad.version, bad.hash)
		_ = resp.Body.Close()
		if resp.StatusCode != bad.status {
			t.Errorf("open %+v = %d", bad, resp.StatusCode)
		}
	}

	resp := open(e.token, "1.0.0", "")
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK || !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream") {
		t.Fatalf("open = %d %s", resp.StatusCode, resp.Header.Get("Content-Type"))
	}
	events := make(chan integrationpod.Event, 8)
	go func() { _ = integrationpod.ReadSSE(resp.Body, events, nil); close(events) }()
	next := func() integrationpod.Event {
		t.Helper()
		select {
		case ev := <-events:
			return ev
		case <-time.After(failsafeWait):
			t.Fatal("no event")
			return integrationpod.Event{}
		}
	}
	if ev := next(); ev.Type != integrationpod.EventStart {
		t.Fatalf("first event = %s", ev.Type)
	}

	post := func(token string, body string) (int, string) {
		t.Helper()
		req, _ := http.NewRequest(http.MethodPost, "http://core/v1/integration/crm/rpc", strings.NewReader(body))
		req.Header.Set(integrationpod.TokenHeader, token)
		resp, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = resp.Body.Close() }()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b)
	}
	logLine := `{"jsonrpc":"2.0","method":"kivali/log","params":{"level":"info","message":"hello"}}`
	if code, _ := post("nope", logLine); code != http.StatusForbidden {
		t.Errorf("post without the token = %d", code)
	}
	if code, _ := post(e.token, "not json"); code != http.StatusBadRequest {
		t.Errorf("post of junk = %d", code)
	}
	if code, _ := post(e.token, logLine); code != http.StatusNoContent {
		t.Errorf("log post = %d", code)
	}
	if rows, _ := srv.Store.ReadIntegrationLogTail("crm", 5); len(rows) != 1 || rows[0].Message != "hello" {
		t.Errorf("log = %+v", rows)
	}
	code, body := post(e.token, `{"jsonrpc":"2.0","id":"x","method":"kivali/credential"}`)
	var m integrationpod.Message
	if code != http.StatusOK || json.Unmarshal([]byte(body), &m) != nil || m.Error == nil || m.Error.Code != integrationpod.CodeNoConnection || string(m.ID) != `"x"` {
		t.Errorf("credential = %d %s", code, body)
	}

	// The tools/list every connect asks for, answered over the socket.
	ev := next()
	var call integrationpod.Message
	if err := json.Unmarshal(ev.Data, &call); err != nil || ev.Type != integrationpod.EventRPC || call.Method != "tools/list" {
		t.Fatalf("event = %s %s", ev.Type, ev.Data)
	}
	reply, _ := integrationpod.Response(call.ID, map[string]any{"tools": []any{map[string]any{"name": "whoami"}}}, nil)
	if code, _ := post(e.token, string(reply)); code != http.StatusNoContent {
		t.Errorf("response post = %d", code)
	}
	reg.checks.Wait()
	if !bytes.Contains(reg.Tools("crm"), []byte("whoami")) {
		t.Errorf("tools = %s", reg.Tools("crm"))
	}

	// Removal: the stream gets shutdown and ends.
	intDelete(t, srv, intBase+"/crm")
	_ = reg.Reconcile(context.Background())
	if ev := next(); ev.Type != integrationpod.EventShutdown {
		t.Errorf("event = %s", ev.Type)
	}
	if _, ok := <-events; ok {
		t.Error("the stream did not end")
	}
}

// fakeServerManifest is the fixture manifest run by the fake server:
// its three tools, whoami as Test connection, list_colors listing the
// blogs field's options, and the token in FAKE_TOKEN.
func fakeServerManifest(name string) map[string]any {
	m := integrationtest.Manifest(name, "1.0.0")
	m["server"].(map[string]any)["mcp_config"].(map[string]any)["env"] = map[string]any{
		fakeintegration.EnvMarker: "1",
		"FAKE_TOKEN":              "${user_config.token}",
		"FAKE_PRINT_PROXY":        "1",
	}
	m["tools"] = []any{
		map[string]any{"name": "echo", "description": "Return the arguments."},
		map[string]any{"name": "whoami", "description": "Name the connected account."},
		map[string]any{"name": "list_colors", "description": "List the colors."},
	}
	k := m["kivali"].(map[string]any)
	k["verify"] = "whoami"
	k["access"].(map[string]any)["choices"] = map[string]any{"blogs": "list_colors"}
	return m
}

// End to end, with core's socket, the real shim and the fake server: an
// approved, connected integration's shim connects with its pod's token;
// Test connection reaches the server with the connection's secret in
// its env; the access form's options are listed live; the shim's proxy
// credential is registered with the version's hosts and never reaches
// the log; a new connection restarts the server with the new value; and
// switching the integration off shuts the shim down and drops its
// credential. This is the chain slice 3 builds agents' calls on.
func TestIntegrationEndToEnd(t *testing.T) {
	srv := newTestServer(t)
	syncPath := filepath.Join(t.TempDir(), "agents.json")
	srv.Store.SetEgressAgentsSyncPath(syncPath)
	intUploadApprove(t, srv, fakeServerManifest("fakecrm"))
	connect := func(token string) {
		t.Helper()
		page := orgOK[apitypes.IntegrationPage](t, orgJSON(t, srv, http.MethodPut, intBase+"/fakecrm/connection", apitypes.ConnectionWrite{Fields: map[string]string{"token": token, "portal": "acme"}}))
		if page.Connection == nil {
			t.Fatal("no connection")
		}
	}
	connect("s3cret-1")
	reg := srv.Integrations
	connected := make(chan string, 4)
	reg.onConnect = func(name string) { connected <- name }
	if err := reg.Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	e, _ := entryOf(reg, "fakecrm")
	sock, _ := integrationSocket(t, srv)
	bundle, err := srv.Store.IntegrationBundleDir("fakecrm", "1.0.0")
	if err != nil {
		t.Fatal(err)
	}
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	shim := &integrationpod.Shim{
		Name: "fakecrm", Version: "1.0.0", Token: e.token,
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
	exited := false
	t.Cleanup(func() {
		cancel()
		if !exited {
			<-done
		}
	})
	select {
	case <-connected:
	case <-time.After(failsafeWait):
		t.Fatal("the shim did not connect")
	}

	verify := func() apitypes.ConnectionState {
		t.Helper()
		page := orgOK[apitypes.IntegrationPage](t, intPost(t, srv, intBase+"/fakecrm/connection/verify"))
		if page.Summary.Status != apitypes.IntegrationStatusConnected || page.Connection == nil || page.Connection.Verified == nil {
			t.Fatalf("page = %+v %+v", page.Summary, page.Connection)
		}
		return *page.Connection
	}
	if c := verify(); !*c.Verified || *c.VerifyMessage != "fake account s3cret-1" {
		t.Errorf("verify = %+v", c)
	}
	if got := orgOK[apitypes.AccessChoices](t, orgGet(t, srv, intBase+"/fakecrm/choices/blogs")); strings.Join(got.Options, ",") != "red,green" {
		t.Errorf("options = %v", got.Options)
	}
	var file store.EgressAgents
	b, _ := os.ReadFile(syncPath)
	_ = json.Unmarshal(b, &file)
	if got := file.Scoped[integrationpod.ProxyUser("fakecrm")]; len(got.Hash) != 64 || strings.Join(got.Hosts, ",") != "api.example.com" {
		t.Errorf("scoped credential = %+v", got)
	}

	connect("s3cret-2")
	if c := verify(); *c.VerifyMessage != "fake account s3cret-2" {
		t.Errorf("verify after a new connection = %+v", c)
	}
	logs := orgOK[apitypes.IntegrationLogs](t, orgGet(t, srv, intBase+"/fakecrm/logs"))
	started := 0
	for _, l := range logs.Lines {
		if l.Message == "server started" {
			started++
		}
		if strings.Contains(l.Message, "@10.43.0.12") || strings.Contains(l.Message, "s3cret") {
			t.Errorf("the log holds a secret: %q", l.Message)
		}
	}
	if started != 2 {
		t.Errorf("server started %d times: %+v", started, logs.Lines)
	}

	intPost(t, srv, intBase+"/fakecrm/disable")
	if err := reg.Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	select {
	case code := <-done:
		exited = true
		if code != 0 {
			t.Errorf("shim exit = %d", code)
		}
	case <-time.After(failsafeWait):
		t.Fatal("the shim did not shut down")
	}
	if got := srv.Store.EgressScopedCredentials(); len(got) != 0 {
		t.Errorf("scoped credentials after switching off: %v", got)
	}
	if page := orgOK[apitypes.IntegrationPage](t, orgGet(t, srv, intBase+"/fakecrm")); page.Summary.Status != apitypes.IntegrationStatusOff {
		t.Errorf("status = %s", page.Summary.Status)
	}
}
