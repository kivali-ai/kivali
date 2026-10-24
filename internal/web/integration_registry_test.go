package web

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kivali-ai/kivali/internal/clock"
	"github.com/kivali-ai/kivali/internal/egresscred"
	"github.com/kivali-ai/kivali/internal/integration/integrationtest"
	"github.com/kivali-ai/kivali/internal/integrationpod"
	"github.com/kivali-ai/kivali/internal/secrets"
	"github.com/kivali-ai/kivali/internal/store"
	"github.com/kivali-ai/kivali/internal/web/apitypes"
)

// failsafeWait bounds a wait on a goroutine of the registry's; a
// passing test never reaches it.
const failsafeWait = 10 * time.Second

// fakeIntegrationProv records the pods it is asked for. A pod is its
// version, token and whether it installs software; Token takes one on
// as integrationpod's does, by version and software.
type fakeIntegrationProv struct {
	mu      sync.Mutex
	pods    map[string]fakeIntegrationPod
	calls   []string
	unsched map[string]string
}

type fakeIntegrationPod struct {
	version, digest, token string
	software               bool
}

func newFakeIntegrationProv() *fakeIntegrationProv {
	return &fakeIntegrationProv{pods: map[string]fakeIntegrationPod{}, unsched: map[string]string{}}
}

func (p *fakeIntegrationProv) Enabled() bool { return true }

func (p *fakeIntegrationProv) Start(_ context.Context, name, version, digest, token string, software bool) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if cur, ok := p.pods[name]; ok && cur == (fakeIntegrationPod{version, digest, token, software}) {
		return nil
	}
	p.pods[name] = fakeIntegrationPod{version, digest, token, software}
	p.calls = append(p.calls, "start "+name+" "+version)
	return nil
}

func (p *fakeIntegrationProv) Stop(_ context.Context, name string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if _, ok := p.pods[name]; ok {
		delete(p.pods, name)
		p.calls = append(p.calls, "stop "+name)
	}
	return nil
}

func (p *fakeIntegrationProv) List(context.Context) ([]string, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	var out []string
	for n := range p.pods {
		out = append(out, n)
	}
	sort.Strings(out)
	return out, nil
}

func (p *fakeIntegrationProv) Token(_ context.Context, name, version, digest string, software bool) (string, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if pod, ok := p.pods[name]; ok && pod.version == version && pod.digest == digest && pod.software == software {
		return pod.token, nil
	}
	return "", nil
}

func (p *fakeIntegrationProv) Unschedulable(_ context.Context, name string) (string, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.unsched[name], nil
}

// takeCalls returns the calls since the last take.
func (p *fakeIntegrationProv) takeCalls() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := p.calls
	p.calls = nil
	return out
}

func (p *fakeIntegrationProv) pod(name string) fakeIntegrationPod {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.pods[name]
}

// withIntegrationRegistry gives srv a registry on a fake clock and
// prov, logging nowhere: its goroutines may outlive the test.
func withIntegrationRegistry(srv *Server, prov IntegrationProvisioner) (*integrationRegistry, *clock.Fake) {
	clk := clock.NewFake()
	srv.Integrations = newIntegrationRegistry(srv.Store, func() secrets.Store { return srv.Secrets }, prov, clk, nil)
	srv.Integrations.signIn = srv.signInAccessToken
	return srv.Integrations, clk
}

// shimAnswer is how a fake shim's server answers a request: a result
// or an error, or ok false for no answer at all.
type shimAnswer func(m integrationpod.Message) (result any, rpcErr *integrationpod.RPCError, ok bool)

func toolTextResult(text string, isError bool) map[string]any {
	return map[string]any{"content": []any{map[string]any{"type": "text", "text": text}}, "isError": isError}
}

// toolTexts answers as a server whose tools return these texts; a
// call of a tool not listed is the tool's own error.
func toolTexts(texts map[string]string) shimAnswer {
	return func(m integrationpod.Message) (any, *integrationpod.RPCError, bool) {
		switch m.Method {
		case "ping":
			return map[string]any{}, nil, true
		case "tools/list":
			return map[string]any{"tools": []any{}}, nil, true
		case "tools/call":
			var p struct {
				Name string `json:"name"`
			}
			_ = json.Unmarshal(m.Params, &p)
			if text, ok := texts[p.Name]; ok {
				return toolTextResult(text, false), nil, true
			}
			return toolTextResult("no tool "+p.Name, true), nil, true
		}
		return nil, &integrationpod.RPCError{Code: integrationpod.CodeMethodNotFound, Message: "method not found"}, true
	}
}

// fakeShim is a connected shim the test drives without a pod: it
// answers core's requests with answer and records every event.
type fakeShim struct {
	reg    *integrationRegistry
	name   string
	link   *integrationLink
	answer shimAnswer
	events chan integrationpod.Event
	// requests records each request core sent.
	mu       sync.Mutex
	requests []integrationpod.Message
}

// attachShim reconciles srv's integrations (an entry and a token for
// each that should run) and connects a fake shim for name, and returns
// once the server's tools/list, which every connect asks for, has been
// answered: answer must answer it.
func attachShim(t *testing.T, srv *Server, name string, answer shimAnswer) *fakeShim {
	t.Helper()
	reg := srv.Integrations
	if err := reg.Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	reg.mu.Lock()
	e := reg.entries[name]
	reg.mu.Unlock()
	if e == nil {
		t.Fatalf("%s should not run", name)
	}
	link, err := reg.connect(name, e.token, e.version, "")
	if err != nil {
		t.Fatal(err)
	}
	fs := &fakeShim{reg: reg, name: name, link: link, answer: answer, events: make(chan integrationpod.Event, 64)}
	go fs.run()
	t.Cleanup(func() { reg.disconnect(link) })
	reg.checks.Wait()
	return fs
}

func (fs *fakeShim) run() {
	for {
		select {
		case ev := <-fs.link.events:
			fs.handle(ev)
		case <-fs.link.closed:
			// What was queued before the close still arrives, as the
			// stream handler writes it.
			for {
				select {
				case ev := <-fs.link.events:
					fs.handle(ev)
				default:
					return
				}
			}
		}
	}
}

func (fs *fakeShim) handle(ev integrationpod.Event) {
	if ev.Type == integrationpod.EventRPC {
		var m integrationpod.Message
		_ = json.Unmarshal(ev.Data, &m)
		fs.mu.Lock()
		fs.requests = append(fs.requests, m)
		answer := fs.answer
		fs.mu.Unlock()
		if m.IsRequest() && answer != nil {
			if res, rpcErr, ok := answer(m); ok {
				out, _ := integrationpod.Response(m.ID, res, rpcErr)
				var resp integrationpod.Message
				_ = json.Unmarshal(out, &resp)
				fs.reg.onMessage(context.Background(), fs.name, resp)
			}
		}
	}
	select {
	case fs.events <- ev:
	default:
	}
}

// setAnswer changes how the fake's server answers from now on.
func (fs *fakeShim) setAnswer(a shimAnswer) {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	fs.answer = a
}

// next waits for the next event of typ.
func (fs *fakeShim) next(t *testing.T, typ integrationpod.EventType) integrationpod.Event {
	t.Helper()
	for {
		select {
		case ev := <-fs.events:
			if ev.Type == typ {
				return ev
			}
		case <-time.After(failsafeWait):
			t.Fatalf("no %s event", typ)
			return integrationpod.Event{}
		}
	}
}

func entryOf(reg *integrationRegistry, name string) (integrationEntry, bool) {
	reg.mu.Lock()
	defer reg.mu.Unlock()
	e, ok := reg.entries[name]
	if !ok {
		return integrationEntry{}, false
	}
	return *e, true
}

// Reconcile gives every approved, switched-on integration a pod and
// takes it away from every other: pending, switched off, removed. A
// new approved version gets a pod of its own with a new token. Without
// this an approval would never run, and a switched-off integration
// would keep running with its connection.
func TestIntegrationReconcileStartsAndStopsPods(t *testing.T) {
	srv := newTestServer(t)
	prov := newFakeIntegrationProv()
	reg, _ := withIntegrationRegistry(srv, prov)
	ctx := context.Background()
	intInstalled(t, srv, "crm", "1.0.0")
	intUpload(t, srv, intBundle(t, "blog", "1.0.0"))
	if err := reg.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	if got := prov.takeCalls(); !reflect.DeepEqual(got, []string{"start crm 1.0.0"}) {
		t.Errorf("first pass = %v", got)
	}
	first := prov.pod("crm").token
	if len(first) != 64 {
		t.Errorf("token %q", first)
	}
	if err := reg.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	if got := prov.takeCalls(); len(got) != 0 || prov.pod("crm").token != first {
		t.Errorf("a pass with nothing changed: %v", got)
	}

	intUpload(t, srv, intBundle(t, "crm", "1.1.0"))
	intPost(t, srv, intBase+"/crm/versions/1.1.0/approve")
	if err := reg.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	if got := prov.takeCalls(); !reflect.DeepEqual(got, []string{"start crm 1.1.0"}) || prov.pod("crm").token == first {
		t.Errorf("after approving 1.1.0: %v, token kept %v", got, prov.pod("crm").token == first)
	}
	// A roll back is a new version in use too: the pod is built again
	// from the version rolled back to, with a token of its own.
	second := prov.pod("crm").token
	if rr := intPost(t, srv, intBase+"/crm/versions/1.0.0/rollback"); rr.Code != http.StatusOK {
		t.Fatalf("roll back: %d %s", rr.Code, rr.Body.String())
	}
	if err := reg.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	if got := prov.takeCalls(); !reflect.DeepEqual(got, []string{"start crm 1.0.0"}) || prov.pod("crm").token == second || prov.pod("crm").token == first {
		t.Errorf("after rolling back to 1.0.0: %v", got)
	}
	if rr := intPost(t, srv, intBase+"/crm/versions/1.1.0/rollback"); rr.Code != http.StatusOK {
		t.Fatalf("roll back to 1.1.0: %d %s", rr.Code, rr.Body.String())
	}
	_ = reg.Reconcile(ctx)
	if got := prov.takeCalls(); !reflect.DeepEqual(got, []string{"start crm 1.1.0"}) {
		t.Errorf("after rolling back to 1.1.0: %v", got)
	}

	intPost(t, srv, intBase+"/crm/disable")
	if err := reg.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	if got := prov.takeCalls(); !reflect.DeepEqual(got, []string{"stop crm"}) {
		t.Errorf("after disable: %v", got)
	}
	if _, ok := entryOf(reg, "crm"); ok {
		t.Error("a switched-off integration keeps its entry")
	}
	intPost(t, srv, intBase+"/crm/enable")
	_ = reg.Reconcile(ctx)
	if got := prov.takeCalls(); !reflect.DeepEqual(got, []string{"start crm 1.1.0"}) {
		t.Errorf("after enable: %v", got)
	}
	intDelete(t, srv, intBase+"/crm")
	_ = reg.Reconcile(ctx)
	if got := prov.takeCalls(); !reflect.DeepEqual(got, []string{"stop crm"}) {
		t.Errorf("after remove: %v", got)
	}
}

// Every change the owner makes asks for a reconcile: approving,
// rejecting, rolling back, the switch, removing and the connection.
// One that did not would leave the pod as it was until the next
// unrelated change.
func TestIntegrationChangesAskForAReconcile(t *testing.T) {
	srv := newTestServer(t)
	reg, _ := withIntegrationRegistry(srv, newFakeIntegrationProv())
	kicked := func() bool {
		select {
		case <-reg.kickCh:
			return true
		default:
			return false
		}
	}
	intUpload(t, srv, intBundle(t, "crm", "1.0.0"))
	intUpload(t, srv, intBundle(t, "crm", "1.1.0"))
	intUpload(t, srv, intBundle(t, "crm", "1.2.0"))
	kicked()
	for _, step := range []struct {
		name string
		do   func()
	}{
		{"approve", func() { intPost(t, srv, intBase+"/crm/versions/1.0.0/approve") }},
		{"reject", func() { intPost(t, srv, intBase+"/crm/versions/1.1.0/reject") }},
		{"approve a later version", func() { intPost(t, srv, intBase+"/crm/versions/1.2.0/approve") }},
		{"roll back", func() { intPost(t, srv, intBase+"/crm/versions/1.0.0/rollback") }},
		{"disable", func() { intPost(t, srv, intBase+"/crm/disable") }},
		{"enable", func() { intPost(t, srv, intBase+"/crm/enable") }},
		{"connect", func() {
			orgJSON(t, srv, "PUT", intBase+"/crm/connection", apitypes.ConnectionWrite{Fields: map[string]string{"token": "t"}})
		}},
		{"uninstall", func() { intDelete(t, srv, intBase+"/crm") }},
	} {
		step.do()
		if !kicked() {
			t.Errorf("%s asked for no reconcile", step.name)
		}
	}
}

// At boot core takes on a pod left running from before, with the
// token in its spec, so its shim reconnects instead of the pod being
// replaced; and it stops the pods and drops the proxy credentials of
// integrations removed while it was down. Without the first, every
// restart of the server would restart every integration; without the
// second, a removed integration would keep running and reaching its
// hosts.
func TestIntegrationBootTakesOnRunningPods(t *testing.T) {
	srv := newTestServer(t)
	intInstalled(t, srv, "crm", "1.0.0")
	prov := newFakeIntegrationProv()
	digest, err := srv.Store.IntegrationBundleDigest("crm", "1.0.0")
	if err != nil {
		t.Fatal(err)
	}
	prov.pods["crm"] = fakeIntegrationPod{version: "1.0.0", digest: digest, token: "token-from-before"}
	prov.pods["gone"] = fakeIntegrationPod{version: "1.0.0", token: "x"}
	syncPath := filepath.Join(t.TempDir(), "agents.json")
	srv.Store.SetEgressAgentsSyncPath(syncPath)
	if err := srv.Store.SetEgressScopedCredential(integrationpod.ProxyUser("gone"), egresscred.Hash("s"), []string{"api.gone.test"}); err != nil {
		t.Fatal(err)
	}
	reg, _ := withIntegrationRegistry(srv, prov)
	if err := reg.reconcile(context.Background(), true); err != nil {
		t.Fatal(err)
	}
	if got := prov.takeCalls(); !reflect.DeepEqual(got, []string{"stop gone"}) {
		t.Errorf("boot = %v", got)
	}
	if link, err := reg.connect("crm", "token-from-before", "1.0.0", ""); err != nil || link == nil {
		t.Errorf("the old pod's shim was refused: %v", err)
	}
	if got := srv.Store.EgressScopedCredentials(); len(got) != 0 {
		t.Errorf("scoped credentials left: %v", got)
	}
}

// The pod starts as root only for a version whose manifest declares
// packages, so the shim can install them before it leaves root. A
// version without packages must get today's pod, never root; one with
// them must get the root pod, or its software is never installed and
// every call that runs it fails. Approving a version that adds
// software makes a new pod.
func TestIntegrationPodRunsAsRootOnlyForSoftware(t *testing.T) {
	srv := newTestServer(t)
	prov := newFakeIntegrationProv()
	reg, _ := withIntegrationRegistry(srv, prov)
	intInstalled(t, srv, "crm", "1.0.0")
	if err := reg.Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	if pod := prov.pod("crm"); pod.version != "1.0.0" || pod.software {
		t.Errorf("without packages: %+v", pod)
	}
	m := integrationtest.Manifest("crm", "1.1.0")
	m["kivali"].(map[string]any)["packages"] = []any{"gh"}
	if rr := intUpload(t, srv, integrationtest.Zip(t, "", m, nil)); rr.Code != http.StatusCreated {
		t.Fatalf("upload: %d %s", rr.Code, rr.Body.String())
	}
	intPost(t, srv, intBase+"/crm/versions/1.1.0/approve")
	if err := reg.Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	if pod := prov.pod("crm"); pod.version != "1.1.0" || !pod.software {
		t.Errorf("with packages: %+v", pod)
	}
}

// A shim is taken on only with its pod's token and the approved
// version; its proxy hash becomes the integration's scoped credential
// with the approved version's hosts; its first event is start; a second
// connection replaces the first. A connection with a stale token or
// another version would let a replaced pod, or one running a version
// no longer approved, take calls.
func TestIntegrationConnectChecksTokenAndVersion(t *testing.T) {
	srv := newTestServer(t)
	reg, _ := withIntegrationRegistry(srv, newFakeIntegrationProv())
	syncPath := filepath.Join(t.TempDir(), "agents.json")
	srv.Store.SetEgressAgentsSyncPath(syncPath)
	intInstalled(t, srv, "crm", "1.0.0")
	if err := reg.Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	e, _ := entryOf(reg, "crm")
	for _, bad := range []struct{ token, version string }{{"nope", "1.0.0"}, {e.token, "0.9.0"}, {"", "1.0.0"}, {e.token, ""}} {
		if _, err := reg.connect("crm", bad.token, bad.version, ""); !errors.Is(err, errIntegrationRefused) {
			t.Errorf("connect %+v: %v", bad, err)
		}
	}
	if _, err := reg.connect("other", e.token, "1.0.0", ""); !errors.Is(err, errIntegrationRefused) {
		t.Errorf("another integration's name: %v", err)
	}
	hash := egresscred.Hash("proxy-secret")
	first, err := reg.connect("crm", e.token, "1.0.0", hash)
	if err != nil {
		t.Fatal(err)
	}
	if ev := <-first.events; ev.Type != integrationpod.EventStart {
		t.Errorf("first event = %s", ev.Type)
	}
	if !reg.Running("crm") {
		t.Error("not running with a link")
	}
	var file store.EgressAgents
	b, _ := os.ReadFile(syncPath)
	_ = json.Unmarshal(b, &file)
	if got := file.Scoped[integrationpod.ProxyUser("crm")]; got.Hash != hash || strings.Join(got.Hosts, ",") != "api.example.com" {
		t.Errorf("scoped credential = %+v", got)
	}
	second, err := reg.connect("crm", e.token, "1.0.0", hash)
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-first.closed:
	default:
		t.Error("the first link is still open")
	}
	reg.disconnect(first)
	if !reg.Running("crm") {
		t.Error("the first link's end took down the second")
	}
	reg.disconnect(second)
	if reg.Running("crm") {
		t.Error("running with no link")
	}
}

// A call goes out as an rpc event and comes back by its id; an error
// response is the server's error; no answer is a timeout at the
// method's deadline on the clock; a link that goes ends the calls
// waiting on it at once. A call that waited for ever, or for a server
// already gone, would hang Test connection.
func TestIntegrationCall(t *testing.T) {
	srv := newTestServer(t)
	reg, clk := withIntegrationRegistry(srv, newFakeIntegrationProv())
	intInstalled(t, srv, "crm", "1.0.0")
	answer := func(m integrationpod.Message) (any, *integrationpod.RPCError, bool) {
		switch m.Method {
		case "tools/list", "ping":
			return map[string]any{}, nil, true
		case "boom":
			return nil, &integrationpod.RPCError{Code: -32000, Message: "broken"}, true
		}
		return nil, nil, false
	}
	fs := attachShim(t, srv, "crm", answer)
	ctx := context.Background()
	if _, err := reg.Call(ctx, "crm", "ping", map[string]any{}); err != nil {
		t.Errorf("ping: %v", err)
	}
	var rpcErr *integrationpod.RPCError
	if _, err := reg.Call(ctx, "crm", "boom", nil); !errors.As(err, &rpcErr) || rpcErr.Message != "broken" {
		t.Errorf("boom: %v", err)
	}
	errc := make(chan error, 1)
	go func() {
		_, err := reg.Call(ctx, "crm", "tools/call", map[string]any{"name": "slow"})
		errc <- err
	}()
	clk.BlockUntil(1)
	clk.Advance(integrationCallTimeout)
	if err := <-errc; !errors.Is(err, errIntegrationTimeout) {
		t.Errorf("unanswered call: %v", err)
	}
	go func() {
		_, err := reg.Call(ctx, "crm", "silence", nil)
		errc <- err
	}()
	clk.BlockUntil(1)
	reg.disconnect(fs.link)
	if err := <-errc; !errors.Is(err, errIntegrationNotRunning) {
		t.Errorf("call when the link went: %v", err)
	}
	if _, err := reg.Call(ctx, "crm", "ping", nil); !errors.Is(err, errIntegrationNotRunning) {
		t.Errorf("no link: %v", err)
	}
	// A late response for a call that is over is dropped.
	reg.onMessage(ctx, "crm", integrationpod.Message{JSONRPC: "2.0", ID: json.RawMessage("9999"), Result: json.RawMessage("{}")})
}

// The health check pings each running server every 30 seconds; three
// pings missed in a row give the integration a new pod with a new
// token, and say so in its log. A ping answered, even with an error,
// resets the count. Without it a server that hangs would leave the
// integration "running" and every call timing out.
func TestIntegrationHealthReplacesAServerThatStopsAnswering(t *testing.T) {
	srv := newTestServer(t)
	prov := newFakeIntegrationProv()
	reg, clk := withIntegrationRegistry(srv, prov)
	intInstalled(t, srv, "crm", "1.0.0")
	var answering sync.Mutex
	answerPings := true
	answer := func(m integrationpod.Message) (any, *integrationpod.RPCError, bool) {
		answering.Lock()
		defer answering.Unlock()
		if m.Method == "ping" && !answerPings {
			return nil, nil, false
		}
		return map[string]any{}, nil, true
	}
	fs := attachShim(t, srv, "crm", answer)
	prov.takeCalls()
	before := prov.pod("crm").token
	ctx := context.Background()
	check := func(answered bool) {
		t.Helper()
		reg.health(ctx)
		if !answered {
			clk.BlockUntil(1)
			clk.Advance(integrationShortTimeout)
		}
		reg.checks.Wait()
	}
	check(true)
	answering.Lock()
	answerPings = false
	answering.Unlock()
	check(false)
	check(false)
	if e, _ := entryOf(reg, "crm"); e.misses != 2 || e.replace {
		t.Fatalf("after two misses: %+v", e)
	}
	check(false)
	if e, _ := entryOf(reg, "crm"); !e.replace {
		t.Fatal("three misses did not ask for a new pod")
	}
	select {
	case <-reg.kickCh:
	default:
		t.Fatal("no reconcile asked for")
	}
	if err := reg.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	if got := prov.takeCalls(); !reflect.DeepEqual(got, []string{"start crm 1.0.0"}) || prov.pod("crm").token == before {
		t.Errorf("replacement = %v", got)
	}
	select {
	case <-fs.link.closed:
	case <-time.After(failsafeWait):
		t.Error("the old link is still open")
	}
	rows, _ := srv.Store.ReadIntegrationLogTail("crm", 5)
	if len(rows) != 1 || !strings.Contains(rows[0].Message, "did not answer three checks in a row") || rows[0].Level != "error" {
		t.Errorf("log = %+v", rows)
	}
}

// An integration that should run but has had no shim for three checks
// gets its pod made again (Start is a no-op for a pod that is there),
// and a pod the cluster cannot place for want of memory is said in its
// log once. Without it a pod deleted by hand would never come back.
func TestIntegrationHealthRestartsAMissingPod(t *testing.T) {
	srv := newTestServer(t)
	prov := newFakeIntegrationProv()
	reg, _ := withIntegrationRegistry(srv, prov)
	intInstalled(t, srv, "crm", "1.0.0")
	ctx := context.Background()
	if err := reg.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	_ = prov.Stop(ctx, "crm")
	prov.takeCalls()
	prov.unsched["crm"] = "0/1 nodes are available: 1 Insufficient memory."
	for i := 0; i < integrationHealthMisses*2; i++ {
		reg.health(ctx)
	}
	select {
	case <-reg.kickCh:
	default:
		t.Fatal("no reconcile asked for")
	}
	_ = reg.Reconcile(ctx)
	if got := prov.takeCalls(); !reflect.DeepEqual(got, []string{"start crm 1.0.0"}) {
		t.Errorf("calls = %v", got)
	}
	rows, _ := srv.Store.ReadIntegrationLogTail("crm", 5)
	if len(rows) != 1 || rows[0].Message != "The integration cannot start: the team does not have enough memory for it." {
		t.Errorf("log = %+v", rows)
	}
}

// The pod's own messages. kivali/credential answers the server's env
// with the connection's values put in, and the values by key; with no
// connection it is the no-connection error. A log line is stored with
// every secret value of the connection replaced, whatever level it
// claims; after a connection change the shim is told to restart and the
// new secret is replaced too. Any other request is a method that does
// not exist. A secret in the log would be in every backup and on the
// owner's screen.
func TestIntegrationPodMessages(t *testing.T) {
	srv := newTestServer(t)
	reg, _ := withIntegrationRegistry(srv, newFakeIntegrationProv())
	intInstalled(t, srv, "crm", "1.0.0")
	fs := attachShim(t, srv, "crm", toolTexts(nil))
	ctx := context.Background()
	req := func(method string, params any) integrationpod.Message {
		t.Helper()
		p, _ := json.Marshal(params)
		out := reg.onMessage(ctx, "crm", integrationpod.Message{JSONRPC: "2.0", ID: json.RawMessage(`"r1"`), Method: method, Params: p})
		var m integrationpod.Message
		if err := json.Unmarshal(out, &m); err != nil {
			t.Fatalf("%s: %s", method, out)
		}
		return m
	}
	if m := req(integrationpod.MethodCredential, nil); m.Error == nil || m.Error.Code != integrationpod.CodeNoConnection {
		t.Errorf("no connection = %+v", m)
	}
	orgJSON(t, srv, "PUT", intBase+"/crm/connection", apitypes.ConnectionWrite{Fields: map[string]string{"token": "s3cret-one", "portal": "acme"}})
	fs.next(t, integrationpod.EventRestart)
	m := req(integrationpod.MethodCredential, nil)
	var cred integrationpod.Credential
	if m.Error != nil || json.Unmarshal(m.Result, &cred) != nil {
		t.Fatalf("credential = %+v", m)
	}
	if !reflect.DeepEqual(cred.Env, map[string]string{"TOKEN": "s3cret-one", "PORTAL": "acme"}) || !reflect.DeepEqual(cred.Fields, map[string]string{"token": "s3cret-one", "portal": "acme"}) {
		t.Errorf("credential = %+v", cred)
	}
	note := func(level, msg string) {
		p, _ := json.Marshal(integrationpod.LogParams{Level: level, Message: msg})
		if out := reg.onMessage(ctx, "crm", integrationpod.Message{JSONRPC: "2.0", Method: integrationpod.MethodLog, Params: p}); out != nil {
			t.Errorf("a notification was answered: %s", out)
		}
	}
	note("stderr", "using token s3cret-one for acme")
	note("shout", "plain")
	orgJSON(t, srv, "PUT", intBase+"/crm/connection", apitypes.ConnectionWrite{Fields: map[string]string{"token": "s3cret-two", "portal": "acme"}})
	fs.next(t, integrationpod.EventRestart)
	note("error", "rejected s3cret-two")
	if m := req(integrationpod.MethodLog, integrationpod.LogParams{Level: "info", Message: "as a request"}); m.Error != nil || string(m.Result) != "{}" {
		t.Errorf("log request = %+v", m)
	}
	rows, _ := srv.Store.ReadIntegrationLogTail("crm", 10)
	var got []string
	for _, r := range rows {
		got = append(got, r.Level+": "+r.Message)
	}
	want := []string{"stderr: using token [secret] for acme", "info: plain", "error: rejected [secret]", "info: as a request"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("log = %q, want %q", got, want)
	}
	assertSecretNotOnDisk(t, srv, "s3cret-one")
	assertSecretNotOnDisk(t, srv, "s3cret-two")
	if m := req("sampling/createMessage", nil); m.Error == nil || m.Error.Code != integrationpod.CodeMethodNotFound {
		t.Errorf("other request = %+v", m)
	}
}

// Shutdown closes every link and stops no pod: pods outlive the server
// and their shims reconnect to the next. Removing an integration, by
// contrast, sends its shim shutdown before the link closes. This fails
// if a restart of the server takes integrations down, or a removed one
// is left running until its pod is deleted.
func TestIntegrationShutdownAndRemoval(t *testing.T) {
	srv := newTestServer(t)
	prov := newFakeIntegrationProv()
	reg, _ := withIntegrationRegistry(srv, prov)
	intInstalled(t, srv, "crm", "1.0.0")
	intInstalled(t, srv, "blog", "1.0.0")
	crm := attachShim(t, srv, "crm", toolTexts(nil))
	blog := attachShim(t, srv, "blog", toolTexts(nil))
	prov.takeCalls()
	intDelete(t, srv, intBase+"/blog")
	_ = reg.Reconcile(context.Background())
	blog.next(t, integrationpod.EventShutdown)
	if got := prov.takeCalls(); !reflect.DeepEqual(got, []string{"stop blog"}) {
		t.Errorf("removal = %v", got)
	}
	reg.closeLinks()
	select {
	case <-crm.link.closed:
	default:
		t.Error("a link is still open after shutdown")
	}
	if got := prov.takeCalls(); len(got) != 0 {
		t.Errorf("shutdown touched pods: %v", got)
	}
	if reg.Running("crm") {
		t.Error("running after shutdown")
	}
}

// Notify sends a notification as an rpc event with no id, and reports
// false without a shim. The tools/list every connect asks for, with no
// agent on it, is kept (Tools). This fails if a notification carries an
// id, which the server would answer into nothing.
func TestIntegrationNotifyAndTools(t *testing.T) {
	srv := newTestServer(t)
	reg, _ := withIntegrationRegistry(srv, newFakeIntegrationProv())
	intInstalled(t, srv, "crm", "1.0.0")
	if reg.Notify("crm", "notifications/cancelled", map[string]any{}) {
		t.Error("a notification went out with no shim")
	}
	fs := attachShim(t, srv, "crm", func(m integrationpod.Message) (any, *integrationpod.RPCError, bool) {
		return map[string]any{"tools": []any{map[string]any{"name": "whoami"}}}, nil, true
	})
	if !reg.Notify("crm", "notifications/cancelled", map[string]any{"requestId": 1}) {
		t.Fatal("Notify = false")
	}
	for {
		ev := fs.next(t, integrationpod.EventRPC)
		var m integrationpod.Message
		_ = json.Unmarshal(ev.Data, &m)
		if m.Method == "notifications/cancelled" {
			if len(m.ID) != 0 {
				t.Errorf("notification with id %s", m.ID)
			}
			break
		}
	}
	// attachShim waited for the refresh every connect runs.
	if tools := string(reg.Tools("crm")); !strings.Contains(tools, "whoami") {
		t.Errorf("tools = %s", tools)
	}
}
