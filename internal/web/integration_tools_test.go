package web

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/kivali-ai/kivali/internal/agentpod"
	"github.com/kivali-ai/kivali/internal/integrationpod"
	"github.com/kivali-ai/kivali/internal/integrationpod/fakeintegration"
	"github.com/kivali-ai/kivali/internal/mcp"
	"github.com/kivali-ai/kivali/internal/store"
	"github.com/kivali-ai/kivali/internal/web/apitypes"
)

// fakeIntegrationAnswer runs the fake integration's server
// (fakeintegration, what cmd/fake-integration runs) in process on
// pipes and answers a fake shim's requests through it, one at a time:
// the server a pod would run, without the pod.
func fakeIntegrationAnswer(t *testing.T) shimAnswer {
	t.Helper()
	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	done := make(chan struct{})
	go func() {
		defer close(done)
		fakeintegration.Run(inR, outW, io.Discard, func(string) string { return "" }, nil)
	}()
	t.Cleanup(func() {
		_ = outR.Close()
		_ = inW.Close()
		<-done
	})
	sc := bufio.NewScanner(outR)
	sc.Buffer(make([]byte, 64<<10), 16<<20)
	var mu sync.Mutex
	return func(m integrationpod.Message) (any, *integrationpod.RPCError, bool) {
		mu.Lock()
		defer mu.Unlock()
		line, _ := json.Marshal(m)
		if _, err := inW.Write(append(line, '\n')); err != nil || !sc.Scan() {
			return nil, nil, false
		}
		var resp integrationpod.Message
		if json.Unmarshal(sc.Bytes(), &resp) != nil {
			return nil, nil, false
		}
		if resp.Error != nil {
			return nil, resp.Error, true
		}
		return resp.Result, nil, true
	}
}

// fakeIntegrationManifest is the fixture manifest for the fake
// integration: its three actions, and an access form of one list
// field, colors.
func fakeIntegrationManifest(t *testing.T) map[string]any {
	t.Helper()
	return integrationManifestWith(t, "fake", func(m map[string]any) {
		m["tools"] = []any{
			map[string]any{"name": "echo", "description": "Return the arguments."},
			map[string]any{"name": "whoami", "description": "Name the connected account."},
			map[string]any{"name": "list_colors", "description": "List the colors."},
		}
		m["kivali"].(map[string]any)["access"] = map[string]any{
			"schema": map[string]any{
				"type":       "object",
				"properties": map[string]any{"colors": map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "title": "Colors"}},
			},
		}
	})
}

// serveCoreUDS serves srv's core.sock routes on a socket of the
// test's, as an agent pod reaches them, and returns its path.
func serveCoreUDS(t *testing.T, srv *Server) string {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "kvi-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	sock := filepath.Join(dir, "s")
	stop, err := srv.StartAgentpodSocket(sock)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(stop)
	return sock
}

func putIntegrationAccess(t *testing.T, srv *Server, name, slug, access string) {
	t.Helper()
	if rr := orgJSON(t, srv, http.MethodPut, intBase+"/"+name+"/access/"+slug, apitypes.AccessWrite{Access: []byte(access)}); rr.Code != http.StatusOK {
		t.Fatalf("access %s: %d %s", slug, rr.Code, rr.Body.String())
	}
}

// requestsOf is what the fake shim's server was sent of method.
func (fs *fakeShim) requestsOf(method string) []integrationpod.Message {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	var out []integrationpod.Message
	for _, m := range fs.requests {
		if m.Method == method {
			out = append(out, m)
		}
	}
	return out
}

// metaOf is the _meta.kivali a request carried, as JSON; empty for
// none.
func metaOf(m integrationpod.Message) string {
	var p struct {
		Meta struct {
			Kivali json.RawMessage `json:"kivali"`
		} `json:"_meta"`
	}
	_ = json.Unmarshal(m.Params, &p)
	return string(p.Meta.Kivali)
}

// Why this test exists: it is the whole path an agent's call takes,
// with the fake integration's real server behind a fake shim and the
// agent's side on the real core.sock client and MCP tools. dana, with
// access, lists the integration's actions (asked for with her stamp)
// and calls one; the call reaches the server stamped with her slug and
// her access, which the fake echoes back, and is in the audit. lee,
// without access, lists nothing, and his call is refused by core before
// the integration sees it, with an audit row saying why. Without core's
// check an agent without access could call any action whose name it
// guessed; without the stamp the integration could not hold an agent
// to its access.
func TestAgentCallsAnIntegrationThroughCore(t *testing.T) {
	srv := newTestServer(t)
	withIntegrationRegistry(srv, newFakeIntegrationProv())
	intTeam(t, srv, "dana", "lee")
	intUploadApprove(t, srv, fakeIntegrationManifest(t))
	if rr := orgJSON(t, srv, http.MethodPut, intBase+"/fake/connection", apitypes.ConnectionWrite{Fields: map[string]string{"token": "s3cret-fake"}}); rr.Code != http.StatusOK {
		t.Fatalf("connect: %d %s", rr.Code, rr.Body.String())
	}
	putIntegrationAccess(t, srv, "fake", "dana", `{"colors":["red"]}`)
	sock := serveCoreUDS(t, srv)
	dana := agentpod.NewClient(sock, "dana")
	lee := agentpod.NewClient(sock, "lee")
	ctx := context.Background()

	// Not running yet: the agent's tab says so, and there is nothing
	// to call.
	tab := orgOK[apitypes.AgentIntegrations](t, orgGet(t, srv, "/api/v1/agents/dana/integrations"))
	if len(tab.Connected) != 1 || tab.Connected[0].Status != apitypes.IntegrationStatusNotRunning {
		t.Errorf("tab before the shim = %+v", tab.Connected)
	}
	if list, err := dana.IntegrationTools(ctx); err != nil || len(list.Tools) != 0 {
		t.Fatalf("tools while not running = %+v, %v", list, err)
	}

	fs := attachShim(t, srv, "fake", fakeIntegrationAnswer(t))
	tab = orgOK[apitypes.AgentIntegrations](t, orgGet(t, srv, "/api/v1/agents/dana/integrations"))
	if tab.Connected[0].Status != apitypes.IntegrationStatusConnected {
		t.Errorf("tab while running = %+v", tab.Connected)
	}
	list, err := dana.IntegrationTools(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, tl := range list.Tools {
		names = append(names, tl.Name)
	}
	if strings.Join(names, ",") != "fake_echo,fake_list_colors,fake_whoami" {
		t.Errorf("dana's actions = %v", names)
	}
	if d := list.Tools[0].Description; d != "Return the arguments. Through the Test fake integration; the owner decides what you may do with it." {
		t.Errorf("description = %q", d)
	}
	if s := string(list.Tools[0].InputSchema); s != `{"type":"object"}` {
		t.Errorf("schema = %s", s)
	}
	const danaStamp = `{"access":{"colors":["red"]},"agent":"dana"}`
	lists := fs.requestsOf("tools/list")
	if len(lists) != 2 || metaOf(lists[0]) != "" || metaOf(lists[1]) != danaStamp {
		t.Errorf("tools/list sent = %d, stamps %q", len(lists), []string{metaOf(lists[0]), metaOf(lists[len(lists)-1])})
	}
	if l, err := lee.IntegrationTools(ctx); err != nil || len(l.Tools) != 0 {
		t.Errorf("lee's actions = %+v, %v", l, err)
	}

	tools := mcp.IntegrationTools(dana, nil, list.Tools, nil)
	var echo mcp.Tool
	for _, tl := range tools {
		if tl.Name == "fake_echo" {
			echo = tl
		}
	}
	res, err := echo.Handler(ctx, json.RawMessage(`{"text":"hi"}`))
	if err != nil {
		t.Fatal(err)
	}
	if res.IsError || len(res.Content) != 1 || res.Content[0] != `{"arguments":{"text":"hi"},"kivali":`+danaStamp+`}` {
		t.Errorf("echo = %+v", res)
	}
	rows, _ := srv.Store.ReadIntegrationAuditTail("fake", 10)
	if len(rows) != 1 || rows[0].Agent != "dana" || rows[0].Action != "echo" || !rows[0].Allowed || rows[0].Reason != "" {
		t.Errorf("audit after dana's call = %+v", rows)
	}

	resp, err := lee.IntegrationCall(ctx, agentpod.IntegrationCallRequest{Name: "fake_echo", Arguments: json.RawMessage(`{}`)})
	if err != nil {
		t.Fatal(err)
	}
	if !resp.IsError || len(resp.Content) != 1 || resp.Content[0] != "You have no access to the Test fake integration. Ask the owner if your work needs it." {
		t.Errorf("lee's call = %+v", resp)
	}
	rows, _ = srv.Store.ReadIntegrationAuditTail("fake", 10)
	if len(rows) != 2 || rows[0].Agent != "lee" || rows[0].Action != "echo" || rows[0].Allowed || rows[0].Reason != "no access" {
		t.Errorf("audit after lee's call = %+v", rows)
	}
	if calls := fs.requestsOf("tools/call"); len(calls) != 1 || metaOf(calls[0]) != danaStamp {
		t.Errorf("the integration was sent %d calls", len(calls))
	}

	// The slug in the path is the one the header binds: lee's process
	// cannot call as dana.
	req := httptest.NewRequest(http.MethodPost, "/v1/agent/dana"+agentpod.IntegrationCallPath, strings.NewReader(`{"name":"fake_echo"}`))
	req.Header.Set(agentpod.SlugHeader, "lee")
	rr := httptest.NewRecorder()
	udsMux(srv).ServeHTTP(rr, req)
	if rr.Code != http.StatusForbidden {
		t.Errorf("a call for another agent's slug = %d", rr.Code)
	}
}

// crmAnswer answers as a server that lists whoami and list_blogs for
// every agent, create_post only for access that may write, and an
// action its manifest does not declare (drop_tables). whoami answers
// with the connection's token in its text, list_blogs with its own
// error, and create_post with an image and a line of text.
func crmAnswer(m integrationpod.Message) (any, *integrationpod.RPCError, bool) {
	switch m.Method {
	case "ping":
		return map[string]any{}, nil, true
	case "tools/list":
		var p struct {
			Meta struct {
				Kivali struct {
					Access map[string]any `json:"access"`
				} `json:"kivali"`
			} `json:"_meta"`
		}
		_ = json.Unmarshal(m.Params, &p)
		tools := []any{
			map[string]any{"name": "whoami", "description": "Name the connected account."},
			map[string]any{"name": "list_blogs", "inputSchema": map[string]any{"type": "object", "properties": map[string]any{}}},
			map[string]any{"name": "drop_tables"},
		}
		if p.Meta.Kivali.Access["write"] == true {
			tools = append(tools, map[string]any{"name": "create_post", "inputSchema": map[string]any{"type": "object", "properties": map[string]any{"title": map[string]any{"type": "string"}}}})
		}
		return map[string]any{"tools": tools}, nil, true
	case "tools/call":
		var p struct {
			Name string `json:"name"`
		}
		_ = json.Unmarshal(m.Params, &p)
		switch p.Name {
		case "whoami":
			return toolTextResult("signed in with s3cret-crm", false), nil, true
		case "list_blogs":
			return toolTextResult("the service is down", true), nil, true
		case "create_post":
			return map[string]any{"content": []any{
				map[string]any{"type": "image", "data": "AAAA", "mimeType": "image/png"},
				map[string]any{"type": "text", "text": "posted"},
			}}, nil, true
		}
		return toolTextResult("no tool "+p.Name, true), nil, true
	}
	return nil, &integrationpod.RPCError{Code: integrationpod.CodeMethodNotFound, Message: "method not found"}, true
}

// callAs is slug's call of name through core, as its MCP server sends
// it.
func callAs(t *testing.T, srv *Server, slug, name, args string) agentpod.IntegrationCallResponse {
	t.Helper()
	return srv.callAgentIntegration(context.Background(), slug, name, json.RawMessage(args))
}

// Why this test exists: what an agent may call is what its access lets
// the integration list for it, among the actions the owner saw on the
// install screen, and nothing else; and what comes back is the
// integration's text with the connection's secrets replaced. Each
// refusal names its cause and the owner, is in the audit, and never
// reaches the integration. A gap here would let an agent call an action
// its access leaves out, or one the owner never approved, or hand the
// model a token a server echoed.
func TestAgentIntegrationCallsAreHeldToTheAccess(t *testing.T) {
	srv := newTestServer(t)
	reg, clk := withIntegrationRegistry(srv, newFakeIntegrationProv())
	intTeam(t, srv, "dana")
	intConnected(t, srv, "crm")
	putIntegrationAccess(t, srv, "crm", "dana", `{"blogs":["Marketing"]}`)
	fs := attachShim(t, srv, "crm", crmAnswer)
	ctx := context.Background()

	tools, err := srv.agentIntegrationTools(ctx, "dana")
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, tl := range tools {
		names = append(names, tl.Name)
	}
	// drop_tables is not in the manifest; create_post is not in the
	// list for access that may not write.
	if strings.Join(names, ",") != "crm_list_blogs,crm_whoami" {
		t.Errorf("actions = %v", names)
	}
	if d := tools[1].Description; d != "Name the connected account. Through the Test crm integration; the owner decides what you may do with it." {
		t.Errorf("description = %q", d)
	}
	if d := tools[0].Description; d != "Through the Test crm integration; the owner decides what you may do with it." {
		t.Errorf("description of an action with none = %q", d)
	}

	steps := []struct {
		name, args, text string
		isError          bool
		allowed          bool
		reason           string
	}{
		{"crm_whoami", `{}`, "signed in with [secret]", false, true, ""},
		{"crm_list_blogs", `{}`, "the service is down", true, true, "the service is down"},
		{"crm_create_post", `{"title":"x"}`, "Your access to the Test crm integration does not include create_post. Ask the owner if your work needs it.", true, false, "the agent's access does not include it"},
		{"crm_drop_tables", `{}`, "Your access to the Test crm integration does not include drop_tables. Ask the owner if your work needs it.", true, false, "the agent's access does not include it"},
		{"crm_whoami", `{not json`, "The arguments are not JSON.", true, false, "the arguments are not JSON"},
	}
	for _, st := range steps {
		resp := callAs(t, srv, "dana", st.name, st.args)
		if resp.IsError != st.isError || len(resp.Content) != 1 || resp.Content[0] != st.text {
			t.Errorf("%s = %+v", st.name, resp)
		}
		rows, _ := srv.Store.ReadIntegrationAuditTail("crm", 1)
		if len(rows) != 1 || rows[0].Agent != "dana" || rows[0].Allowed != st.allowed || rows[0].Reason != st.reason {
			t.Errorf("%s: audit = %+v", st.name, rows)
		}
	}
	if calls := fs.requestsOf("tools/call"); len(calls) != 2 {
		t.Errorf("the integration was sent %d calls; only the two allowed should reach it", len(calls))
	}
	assertSecretNotOnDisk(t, srv, "s3cret-crm")

	// Access that may write: the integration lists create_post for it,
	// and its answer's image is named, not passed on.
	putIntegrationAccess(t, srv, "crm", "dana", `{"blogs":["Marketing"],"write":true}`)
	resp := callAs(t, srv, "dana", "crm_create_post", `{"title":"x"}`)
	if resp.IsError || strings.Join(resp.Content, "|") != "posted|The answer also held content Kivali does not pass on (image)." {
		t.Errorf("create_post = %+v", resp)
	}

	// The server's own error, and no answer at all.
	fs.setAnswer(func(m integrationpod.Message) (any, *integrationpod.RPCError, bool) {
		if m.Method == "tools/call" {
			return nil, &integrationpod.RPCError{Code: -32000, Message: "broken"}, true
		}
		return crmAnswer(m)
	})
	if resp := callAs(t, srv, "dana", "crm_whoami", `{}`); !resp.IsError || resp.Content[0] != "The Test crm integration answered with an error: broken" {
		t.Errorf("an error response = %+v", resp)
	}
	fs.setAnswer(func(m integrationpod.Message) (any, *integrationpod.RPCError, bool) {
		if m.Method == "tools/call" {
			return nil, nil, false
		}
		return crmAnswer(m)
	})
	done := make(chan agentpod.IntegrationCallResponse, 1)
	go func() { done <- callAs(t, srv, "dana", "crm_whoami", `{}`) }()
	clk.BlockUntil(1)
	clk.Advance(integrationCallTimeout)
	if resp := <-done; !resp.IsError || resp.Content[0] != "The Test crm integration did not answer within 60 seconds; try again later or tell the owner." {
		t.Errorf("no answer = %+v", resp)
	}
	fs.setAnswer(crmAnswer)

	// Access that no longer applies, the switch, a name no integration
	// has, and an integration not running.
	if err := srv.Store.SetIntegrationAccess("crm", store.IntegrationAccess{Slug: "dana", ConnectionID: "gone", Access: map[string]any{"blogs": []any{"M"}}}); err != nil {
		t.Fatal(err)
	}
	if resp := callAs(t, srv, "dana", "crm_whoami", `{}`); resp.Content[0] != "Your access to the Test crm integration no longer applies (it was set for a connection that is no longer there). Ask the owner to set it again." {
		t.Errorf("invalid access = %+v", resp)
	}
	putIntegrationAccess(t, srv, "crm", "dana", `{"blogs":["Marketing"]}`)
	intPost(t, srv, intBase+"/crm/disable")
	if resp := callAs(t, srv, "dana", "crm_whoami", `{}`); resp.Content[0] != "The Test crm integration is switched off. Ask the owner if your work needs it." {
		t.Errorf("off = %+v", resp)
	}
	rows, _ := srv.Store.ReadIntegrationAuditTail("crm", 1)
	if rows[0].Reason != "the integration is off" || rows[0].Allowed {
		t.Errorf("audit when off = %+v", rows)
	}
	intPost(t, srv, intBase+"/crm/enable")
	if resp := callAs(t, srv, "dana", "nothing_here", `{}`); resp.Content[0] != "No integration has an action named nothing_here." {
		t.Errorf("unknown name = %+v", resp)
	}
	reg.disconnect(fs.link)
	if resp := callAs(t, srv, "dana", "crm_whoami", `{}`); resp.Content[0] != "The Test crm integration is not running right now; try again later or tell the owner." {
		t.Errorf("not running = %+v", resp)
	}
	if tools, _ := srv.agentIntegrationTools(ctx, "dana"); len(tools) != 0 {
		t.Errorf("actions while not running = %+v", tools)
	}
}

// Why this test exists: an agent's access is checked against the
// version in use on every call, so approving a version whose access
// form refuses the agent's access, or rolling back to one, takes the
// integration away from it at once: its calls are refused with the
// form's reason and an audit row, never reach the integration, and its
// list has none of the integration's actions. Rolling back to a version
// whose form fits gives them back. Routing that read the access row
// without its validity would let an agent act with access the version
// running cannot express.
func TestAgentIntegrationAccessFollowsTheVersionInUse(t *testing.T) {
	srv := newTestServer(t)
	withIntegrationRegistry(srv, newFakeIntegrationProv())
	intTeam(t, srv, "dana")
	intConnected(t, srv, "crm")
	putIntegrationAccess(t, srv, "crm", "dana", `{"blogs":["Marketing"],"region":"eu"}`)
	ctx := context.Background()
	attachShim(t, srv, "crm", crmAnswer)
	if resp := callAs(t, srv, "dana", "crm_whoami", `{}`); resp.IsError {
		t.Fatalf("under 1.0.0 = %+v", resp)
	}

	if rr := intUpload(t, srv, crmVersion2(t)); rr.Code != http.StatusCreated {
		t.Fatal(rr.Body.String())
	}
	intPost(t, srv, intBase+"/crm/versions/2.0.0/approve")
	fs := attachShim(t, srv, "crm", crmAnswer)
	resp := callAs(t, srv, "dana", "crm_whoami", `{}`)
	if !resp.IsError || len(resp.Content) != 1 || resp.Content[0] != "Your access to the Test crm integration no longer applies (Region must be one of us, apac). Ask the owner to set it again." {
		t.Errorf("under 2.0.0 = %+v", resp)
	}
	rows, _ := srv.Store.ReadIntegrationAuditTail("crm", 1)
	if len(rows) != 1 || rows[0].Agent != "dana" || rows[0].Allowed || rows[0].Reason != "the access no longer applies: Region must be one of us, apac" {
		t.Errorf("audit under 2.0.0 = %+v", rows)
	}
	if calls := fs.requestsOf("tools/call"); len(calls) != 0 {
		t.Errorf("a refused call reached the integration: %d", len(calls))
	}
	if tools, err := srv.agentIntegrationTools(ctx, "dana"); err != nil || len(tools) != 0 {
		t.Errorf("actions under 2.0.0 = %+v, %v", tools, err)
	}

	intPost(t, srv, intBase+"/crm/versions/1.0.0/rollback")
	attachShim(t, srv, "crm", crmAnswer)
	if resp := callAs(t, srv, "dana", "crm_whoami", `{}`); resp.IsError || resp.Content[0] != "signed in with [secret]" {
		t.Errorf("rolled back to 1.0.0 = %+v", resp)
	}
	if tools, err := srv.agentIntegrationTools(ctx, "dana"); err != nil || len(tools) != 2 {
		t.Errorf("actions after the roll back = %+v, %v", tools, err)
	}
}

// Why this test exists: every turn lists the agent's actions, and a
// turn must not cost a round trip to every integration. The list for
// an agent is asked for once and kept, and asked for again only when
// it could differ: the agent's access changed, the connection changed
// (the server restarts with new values), or the shim connected again.
// Kept past one of those, an agent would keep the actions of access the
// owner took away.
func TestAgentIntegrationListIsKeptUntilItCouldDiffer(t *testing.T) {
	srv := newTestServer(t)
	withIntegrationRegistry(srv, newFakeIntegrationProv())
	intTeam(t, srv, "dana")
	intConnected(t, srv, "crm")
	putIntegrationAccess(t, srv, "crm", "dana", `{"blogs":["Marketing"]}`)
	fs := attachShim(t, srv, "crm", crmAnswer)
	ctx := context.Background()
	asked := func() int {
		n := 0
		for _, m := range fs.requestsOf("tools/list") {
			if metaOf(m) != "" {
				n++
			}
		}
		return n
	}
	list := func() {
		t.Helper()
		if _, err := srv.agentIntegrationTools(ctx, "dana"); err != nil {
			t.Fatal(err)
		}
	}
	list()
	list()
	if n := asked(); n != 1 {
		t.Errorf("two lists asked %d times", n)
	}
	putIntegrationAccess(t, srv, "crm", "dana", `{"blogs":["Marketing"],"write":true}`)
	list()
	if n := asked(); n != 2 {
		t.Errorf("after an access change: asked %d times", n)
	}
	orgJSON(t, srv, http.MethodPut, intBase+"/crm/connection", apitypes.ConnectionWrite{Fields: map[string]string{"token": "s3cret-crm-2"}})
	list()
	if n := asked(); n != 3 {
		t.Errorf("after a connection change: asked %d times", n)
	}
	again := attachShim(t, srv, "crm", crmAnswer)
	fs = again
	list()
	if n := asked(); n != 1 {
		t.Errorf("a new link asked %d times", n)
	}
}

// Why this test exists: the turn request is how the agent's actions
// reach the CLI's --allowedTools and how the runner sees a grant change
// (each marked Integration). An action left out of the request would be
// one the CLI refuses; one not marked would never respawn the runner
// when it is taken away.
func TestChatTurnCarriesTheAgentsIntegrationActions(t *testing.T) {
	srv := newChatServer(t)
	withIntegrationRegistry(srv, newFakeIntegrationProv())
	intTeam(t, srv, "alice")
	intConnected(t, srv, "crm")
	putIntegrationAccess(t, srv, "crm", "alice", `{"blogs":["Marketing"]}`)
	attachShim(t, srv, "crm", crmAnswer)
	fake := installFakeAgentPod(t, srv, "alice")
	req := sendMessage(t, srv, fake, "hi")
	var marked []string
	for _, tl := range req.Tools {
		if tl.Integration {
			marked = append(marked, tl.Name)
			if !strings.HasSuffix(tl.Description, "the owner decides what you may do with it.") || len(tl.InputSchema) == 0 {
				t.Errorf("%s = %+v", tl.Name, tl)
			}
		} else if strings.HasPrefix(tl.Name, "crm_") {
			t.Errorf("%s is not marked as an integration action", tl.Name)
		}
	}
	if strings.Join(marked, ",") != "crm_list_blogs,crm_whoami" {
		t.Errorf("integration actions in the turn = %v", marked)
	}
}

// Why this test exists: an action whose input schema is not an
// object's cannot be given to a model, and handing it on would put a
// tool in the CLI's list that the model's API refuses. It is left out,
// and the integration's other actions stay.
func TestAgentIntegrationActionWithABadSchemaIsLeftOut(t *testing.T) {
	srv := newTestServer(t)
	withIntegrationRegistry(srv, newFakeIntegrationProv())
	intTeam(t, srv, "dana")
	intConnected(t, srv, "crm")
	putIntegrationAccess(t, srv, "crm", "dana", `{"blogs":["Marketing"]}`)
	attachShim(t, srv, "crm", func(m integrationpod.Message) (any, *integrationpod.RPCError, bool) {
		if m.Method == "tools/list" {
			return map[string]any{"tools": []any{
				map[string]any{"name": "whoami", "inputSchema": map[string]any{"type": "array"}},
				map[string]any{"name": "list_blogs", "inputSchema": "nope"},
				map[string]any{"name": "create_post"},
			}}, nil, true
		}
		return crmAnswer(m)
	})
	tools, err := srv.agentIntegrationTools(context.Background(), "dana")
	if err != nil {
		t.Fatal(err)
	}
	if len(tools) != 1 || tools[0].Name != "crm_create_post" || string(tools[0].InputSchema) != `{"type":"object"}` {
		t.Errorf("actions = %+v", tools)
	}
}
