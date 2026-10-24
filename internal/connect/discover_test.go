package connect_test

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/kivali-ai/kivali/internal/clock"
	"github.com/kivali-ai/kivali/internal/connect"
	"github.com/kivali-ai/kivali/internal/connect/connecttest"
)

// docServer is an in-process server that answers each path with the
// document docs gives it (a 404 for a path it does not have), keyed by
// host and path, and records the paths asked for.
type docServer struct {
	*httptest.Server
	mu    sync.Mutex
	docs  map[string]string
	paths []string
}

func newDocServer(t *testing.T) *docServer {
	t.Helper()
	d := &docServer{docs: map[string]string{}}
	d.Server = httptest.NewServer(http.HandlerFunc(d.serve))
	t.Cleanup(d.Close)
	return d
}

func (d *docServer) serve(w http.ResponseWriter, r *http.Request) {
	d.mu.Lock()
	d.paths = append(d.paths, r.URL.Path)
	body, ok := d.docs[r.URL.Path]
	d.mu.Unlock()
	if !ok {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write([]byte(strings.ReplaceAll(body, "{{base}}", d.URL)))
}

// set serves body at path; {{base}} in it is the server's address.
func (d *docServer) set(path, body string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.docs[path] = body
}

func (d *docServer) asked() []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return slices.Clone(d.paths)
}

// serverDoc is an authorization server's metadata whose issuer is
// issuer and whose endpoints are on the docServer.
func serverDoc(issuer string) string {
	return `{"issuer":"` + issuer + `","authorization_endpoint":"{{base}}/authorize","token_endpoint":"{{base}}/token",` +
		`"revocation_endpoint":"{{base}}/revoke","registration_endpoint":"{{base}}/register",` +
		`"response_types_supported":["code"],"code_challenge_methods_supported":["S256"]}`
}

func discoverEngine() *connect.Engine {
	return &connect.Engine{Clock: clock.NewFake(), HTTP: &http.Client{}}
}

// Why this test exists: a registered client's manifest gives one
// address, an MCP server's, and core must find the authorization
// server from it the way MCP clients do: the protected resource's
// metadata at the path-inserted well-known address names the server,
// whose own metadata names the endpoints the sign-in, the tokens and
// the registration go to. If any of that were read wrong the owner
// could not sign in, or would be sent somewhere the server did not
// name. The resource is kept for the token requests (RFC 8707).
func TestDiscoverFollowsTheResourceToItsServer(t *testing.T) {
	v := connecttest.New(t, clock.NewFake())
	m, err := discoverEngine().Discover(context.Background(), v.Resource(), []string{"127.0.0.1"})
	if err != nil {
		t.Fatal(err)
	}
	want := connect.Metadata{
		Resource: v.URL + "/mcp", Issuer: v.URL,
		Authorization: v.URL + "/authorize", Token: v.URL + "/token", Revocation: v.URL + "/revoke", Registration: v.URL + "/register",
		PKCEMethods: []string{"S256"}, AuthMethods: []string{"client_secret_basic", "client_secret_post"},
	}
	if !equalMetadata(*m, want) {
		t.Errorf("metadata = %+v\nwant %+v", *m, want)
	}
	if ep := m.Endpoints(); ep.Authorization != want.Authorization || ep.Token != want.Token || ep.Revocation != want.Revocation || ep.Resource != want.Resource {
		t.Errorf("endpoints = %+v", ep)
	}
	var paths []string
	for _, r := range v.Requests() {
		paths = append(paths, r.Path)
	}
	if !slices.Equal(paths, []string{"/.well-known/oauth-protected-resource/mcp", "/.well-known/oauth-authorization-server"}) {
		t.Errorf("asked for %v", paths)
	}
}

func equalMetadata(a, b connect.Metadata) bool {
	return a.Resource == b.Resource && a.Issuer == b.Issuer && a.Authorization == b.Authorization && a.Token == b.Token &&
		a.Revocation == b.Revocation && a.Registration == b.Registration &&
		slices.Equal(a.PKCEMethods, b.PKCEMethods) && slices.Equal(a.AuthMethods, b.AuthMethods)
}

// The documents ZoomInfo's MCP server published on 2026-10-23, as they
// were read then, byte for byte.
const (
	zoomInfoResource = `{"resource":"https://mcp.zoominfo.com/mcp","authorization_servers":["https://mcp.zoominfo.com"],"scopes_supported":["openid","profile","zi_api","email","offline_access","zi_mcp","api:data:mcp"],"bearer_methods_supported":["header"],"resource_documentation":"https://mcp.zoominfo.com/docs"}`
	zoomInfoServer   = `{"authorization_endpoint":"https://mcp.zoominfo.com/oauth/authorize","code_challenge_methods_supported":["S256"],"grant_types_supported":["authorization_code","refresh_token"],"issuer":"https://mcp.zoominfo.com","jwks_uri":"https://okta-login.zoominfo.com/oauth2/default/v1/keys","registration_endpoint":"https://mcp.zoominfo.com/oauth/register","response_modes_supported":["query"],"response_types_supported":["code"],"revocation_endpoint":"https://okta-login.zoominfo.com/oauth2/default/v1/revoke","scopes_supported":["openid","profile","zi_api","email","offline_access","zi_mcp","api:data:mcp"],"token_endpoint":"https://okta-login.zoominfo.com/oauth2/default/v1/token","token_endpoint_auth_methods_supported":["client_secret_basic","client_secret_post"]}`
)

// zoomInfoEngine is an engine whose every https request reaches an
// in-process server that answers as mcp.zoominfo.com did, whatever host
// it names: nothing leaves the machine.
func zoomInfoEngine(t *testing.T) (*connect.Engine, *[]string) {
	t.Helper()
	var mu sync.Mutex
	var asked []string
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		asked = append(asked, r.Host+r.URL.Path)
		mu.Unlock()
		var body string
		switch r.Host + r.URL.Path {
		case "mcp.zoominfo.com/.well-known/oauth-protected-resource/mcp":
			body = zoomInfoResource
		case "mcp.zoominfo.com/.well-known/oauth-authorization-server":
			body = zoomInfoServer
		default:
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	tlsConf := srv.Client().Transport.(*http.Transport).TLSClientConfig.Clone()
	// The test server's certificate names example.com; it is checked
	// against that name, whatever host the request is for.
	tlsConf.ServerName = "example.com"
	tr := &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "tcp", srv.Listener.Addr().String())
		},
		TLSClientConfig: tlsConf,
	}
	t.Cleanup(tr.CloseIdleConnections)
	return &connect.Engine{Clock: clock.NewFake(), HTTP: &http.Client{Transport: tr}}, &asked
}

// Why this test exists: ZoomInfo is the service client: registered was
// made for, and its documents put the token and revocation endpoints on
// another host (its Okta) than the server it names. Read verbatim, they
// must give the endpoints its manifest's hosts allow; with Okta's host
// left out of the hosts, the same documents must be refused, naming
// the host, rather than send the tokens there.
func TestDiscoverReadsZoomInfoAsItPublishes(t *testing.T) {
	e, asked := zoomInfoEngine(t)
	hosts := []string{"mcp.zoominfo.com", "okta-login.zoominfo.com"}
	m, err := e.Discover(context.Background(), "https://mcp.zoominfo.com/mcp", hosts)
	if err != nil {
		t.Fatal(err)
	}
	want := connect.Metadata{
		Resource: "https://mcp.zoominfo.com/mcp", Issuer: "https://mcp.zoominfo.com",
		Authorization: "https://mcp.zoominfo.com/oauth/authorize",
		Token:         "https://okta-login.zoominfo.com/oauth2/default/v1/token",
		Revocation:    "https://okta-login.zoominfo.com/oauth2/default/v1/revoke",
		Registration:  "https://mcp.zoominfo.com/oauth/register",
		PKCEMethods:   []string{"S256"}, AuthMethods: []string{"client_secret_basic", "client_secret_post"},
	}
	if !equalMetadata(*m, want) {
		t.Errorf("metadata = %+v\nwant %+v", *m, want)
	}
	if got := *asked; !slices.Equal(got, []string{
		"mcp.zoominfo.com/.well-known/oauth-protected-resource/mcp",
		"mcp.zoominfo.com/.well-known/oauth-authorization-server",
	}) {
		t.Errorf("asked for %v", got)
	}
	if method, err := connect.ChooseAuthMethod(m.AuthMethods); err != nil || method != connect.AuthSecretBasic {
		t.Errorf("auth method = %q, %v", method, err)
	}

	_, err = e.Discover(context.Background(), "https://mcp.zoominfo.com/mcp", []string{"mcp.zoominfo.com"})
	if err == nil || !strings.Contains(err.Error(), "names its token endpoint on okta-login.zoominfo.com, which the manifest does not list") {
		t.Errorf("without Okta's host: %v", err)
	}
}

// Why this test exists: services publish their metadata at different
// places the standards allow, and a discover address may be the
// resource or the authorization server itself, with a path or without.
// Each place must be tried in the standards' order and the first
// document found used; one missed would leave a service that follows
// the standards unreachable, and one tried out of order could read a
// document meant for another resource on the same host.
func TestDiscoverTriesEachPlaceInOrder(t *testing.T) {
	cases := []struct {
		name     string
		discover string
		docs     map[string]string
		asked    []string
		issuer   string
		resource string
	}{
		{
			name: "resource metadata at the root", discover: "/mcp",
			docs: map[string]string{
				"/.well-known/oauth-protected-resource":   `{"resource":"{{base}}/mcp","authorization_servers":["{{base}}"]}`,
				"/.well-known/oauth-authorization-server": serverDoc("{{base}}"),
			},
			asked:  []string{"/.well-known/oauth-protected-resource/mcp", "/.well-known/oauth-protected-resource", "/.well-known/oauth-authorization-server"},
			issuer: "{{base}}", resource: "{{base}}/mcp",
		},
		{
			name: "a resource with no path", discover: "",
			docs: map[string]string{
				"/.well-known/oauth-protected-resource":          `{"resource":"{{base}}","authorization_servers":["{{base}}/tenant"]}`,
				"/.well-known/oauth-authorization-server/tenant": serverDoc("{{base}}/tenant"),
			},
			asked:  []string{"/.well-known/oauth-protected-resource", "/.well-known/oauth-authorization-server/tenant"},
			issuer: "{{base}}/tenant", resource: "{{base}}",
		},
		{
			name: "an issuer's OpenID configuration after its path", discover: "/mcp",
			docs: map[string]string{
				"/.well-known/oauth-protected-resource/mcp": `{"resource":"{{base}}/mcp","authorization_servers":["{{base}}/tenant"]}`,
				"/tenant/.well-known/openid-configuration":  serverDoc("{{base}}/tenant"),
			},
			asked: []string{
				"/.well-known/oauth-protected-resource/mcp",
				"/.well-known/oauth-authorization-server/tenant", "/.well-known/openid-configuration/tenant", "/tenant/.well-known/openid-configuration",
			},
			issuer: "{{base}}/tenant", resource: "{{base}}/mcp",
		},
		{
			name: "no resource metadata: the server itself, at its root", discover: "/mcp",
			docs: map[string]string{"/.well-known/openid-configuration": serverDoc("{{base}}")},
			asked: []string{
				"/.well-known/oauth-protected-resource/mcp", "/.well-known/oauth-protected-resource",
				"/.well-known/oauth-authorization-server/mcp", "/.well-known/openid-configuration/mcp", "/mcp/.well-known/openid-configuration",
				"/.well-known/oauth-authorization-server", "/.well-known/openid-configuration",
			},
			issuer: "{{base}}", resource: "",
		},
		{
			name: "a page that is no JSON is no document", discover: "/mcp",
			docs: map[string]string{
				"/.well-known/oauth-protected-resource/mcp": `<html>sign in</html>`,
				"/.well-known/oauth-protected-resource":     `{"authorization_servers":["{{base}}"]}`,
				"/.well-known/oauth-authorization-server":   serverDoc("{{base}}/"),
			},
			asked:  []string{"/.well-known/oauth-protected-resource/mcp", "/.well-known/oauth-protected-resource", "/.well-known/oauth-authorization-server"},
			issuer: "{{base}}/", resource: "{{base}}/mcp",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			d := newDocServer(t)
			for p, body := range c.docs {
				d.set(p, body)
			}
			m, err := discoverEngine().Discover(context.Background(), d.URL+c.discover, []string{"127.0.0.1"})
			if err != nil {
				t.Fatal(err)
			}
			base := func(s string) string { return strings.ReplaceAll(s, "{{base}}", d.URL) }
			if m.Issuer != base(c.issuer) || m.Resource != base(c.resource) || m.Token != d.URL+"/token" {
				t.Errorf("metadata = %+v", *m)
			}
			if got := d.asked(); !slices.Equal(got, c.asked) {
				t.Errorf("asked for %v\nwant %v", got, c.asked)
			}
		})
	}
}

// Why this test exists: the manifest's hosts are the promise the
// install screen makes about where the owner's browser goes and where
// the tokens come from, and the metadata is the service's word, which
// core must hold to that promise and to the standards: a document for
// another resource or issuer (a mix-up), an endpoint or server on a
// host the manifest does not list or over plain http, a server that
// does not offer the flow Kivali runs, a server that fails, a document
// Kivali cannot read, one too large, and a redirect to another host
// are each refused in words that say what, before anything is sent to
// the host they name.
func TestDiscoverRefusesWhatTheManifestDoesNotAllow(t *testing.T) {
	prm := `{"resource":"{{base}}/mcp","authorization_servers":["{{base}}"]}`
	edit := func(field, value string) string {
		var doc map[string]any
		_ = json.Unmarshal([]byte(serverDoc("{{base}}")), &doc)
		if value == "" {
			delete(doc, field)
		} else {
			var v any
			if err := json.Unmarshal([]byte(value), &v); err != nil {
				v = value
			}
			doc[field] = v
		}
		b, _ := json.Marshal(doc)
		return string(b)
	}
	cases := []struct {
		name string
		prm  string
		as   string
		want string
	}{
		{"another resource's metadata", `{"resource":"{{base}}/other","authorization_servers":["{{base}}"]}`, serverDoc("{{base}}"), "/mcp is for "},
		{"no authorization server", `{"resource":"{{base}}/mcp","authorization_servers":[]}`, "", "names no authorization server"},
		{"a server on another host", `{"resource":"{{base}}/mcp","authorization_servers":["https://login.evil.example"]}`, "", "names its authorization server on login.evil.example, which the manifest does not list"},
		{"a server over plain http", `{"resource":"{{base}}/mcp","authorization_servers":["http://login.example.com"]}`, "", `names an authorization server, "http://login.example.com", that is not an https:// address`},
		{"another issuer's metadata", prm, serverDoc("https://other.example"), `names the issuer "https://other.example"`},
		{"a token endpoint on another host", prm, edit("token_endpoint", "https://tokens.evil.example/token"), "names its token endpoint on tokens.evil.example, which the manifest does not list"},
		{"a token endpoint on another loopback name", prm, edit("token_endpoint", "http://localhost:1/token"), "names its token endpoint on localhost:1, which the manifest does not list"},
		{"an authorization endpoint over plain http", prm, edit("authorization_endpoint", "http://login.example.com/authorize"), "names its authorization endpoint as \"http://login.example.com/authorize\", which is not an https:// address"},
		{"a registration endpoint on another host", prm, edit("registration_endpoint", "https://evil.example/register"), "names its registration endpoint on evil.example"},
		{"no token endpoint", prm, edit("token_endpoint", ""), "names no token endpoint"},
		{"no code flow", prm, edit("response_types_supported", `["token"]`), "does not offer the authorization code sign-in Kivali uses"},
		{"no code grant", prm, edit("grant_types_supported", `["client_credentials"]`), "does not offer the authorization code sign-in Kivali uses"},
		{"no S256", prm, edit("code_challenge_methods_supported", `["plain"]`), "does not offer PKCE with S256"},
		{"a document Kivali cannot read", `{"resource":"{{base}}/mcp","authorization_servers":"{{base}}"}`, "", "is not a document Kivali reads"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			d := newDocServer(t)
			d.set("/.well-known/oauth-protected-resource/mcp", c.prm)
			if c.as != "" {
				d.set("/.well-known/oauth-authorization-server", c.as)
			}
			_, err := discoverEngine().Discover(context.Background(), d.URL+"/mcp", []string{"127.0.0.1"})
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("err = %v, want %q", err, c.want)
			}
			for _, p := range d.asked() {
				if !strings.HasPrefix(p, "/.well-known/") {
					t.Errorf("asked for %s", p)
				}
			}
		})
	}

	t.Run("a server that fails", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusBadGateway) }))
		t.Cleanup(srv.Close)
		_, err := discoverEngine().Discover(context.Background(), srv.URL+"/mcp", nil)
		if err == nil || !strings.Contains(err.Error(), "answered 502 Bad Gateway") {
			t.Errorf("err = %v", err)
		}
	})
	t.Run("a document too large", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte(`{"resource":"` + strings.Repeat("x", 300<<10) + `"}`))
		}))
		t.Cleanup(srv.Close)
		_, err := discoverEngine().Discover(context.Background(), srv.URL+"/mcp", nil)
		if err == nil || !strings.Contains(err.Error(), "answered with more than 256 KiB") {
			t.Errorf("err = %v", err)
		}
	})
	t.Run("a redirect to another host", func(t *testing.T) {
		other := newDocServer(t)
		other.set("/.well-known/oauth-protected-resource/mcp", prm)
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// The same port on another loopback name is another host.
			to := strings.Replace(other.URL, "127.0.0.1", "localhost", 1)
			http.Redirect(w, r, to+r.URL.Path, http.StatusFound)
		}))
		t.Cleanup(srv.Close)
		_, err := discoverEngine().Discover(context.Background(), srv.URL+"/mcp", []string{"localhost"})
		if err == nil || !strings.Contains(err.Error(), "another host") {
			t.Errorf("err = %v", err)
		}
		if len(other.asked()) != 0 {
			t.Errorf("the other host was asked for %v", other.asked())
		}
	})
	t.Run("a redirect on the same host", func(t *testing.T) {
		d := newDocServer(t)
		d.set("/meta/resource", prm)
		d.set("/.well-known/oauth-authorization-server", serverDoc("{{base}}"))
		d.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/.well-known/oauth-protected-resource/mcp" {
				http.Redirect(w, r, "/meta/resource", http.StatusMovedPermanently)
				return
			}
			d.serve(w, r)
		})
		if _, err := discoverEngine().Discover(context.Background(), d.URL+"/mcp", nil); err != nil {
			t.Errorf("err = %v", err)
		}
	})
	t.Run("a discover address over plain http", func(t *testing.T) {
		_, err := discoverEngine().Discover(context.Background(), "http://mcp.example.com/mcp", []string{"mcp.example.com"})
		if err == nil || !strings.Contains(err.Error(), "is not an https:// address") {
			t.Errorf("err = %v", err)
		}
	})
}

// Why this test exists: discovery runs inside a sign-in the owner
// waits on, and a service that never answers must not hold it: each
// request ends after connect.RequestTimeout on the engine's clock, in
// words that say who did not answer, whatever the HTTP client's own
// timeout.
func TestDiscoverTimesOutOnTheEnginesClock(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-release:
		}
	}))
	t.Cleanup(func() { close(release); srv.Close() })
	clk := clock.NewFake()
	e := &connect.Engine{Clock: clk, HTTP: &http.Client{}}
	errc := make(chan error, 1)
	go func() {
		_, err := e.Discover(context.Background(), srv.URL+"/mcp", nil)
		errc <- err
	}()
	clk.BlockUntil(1)
	clk.Advance(connect.RequestTimeout)
	err := <-errc
	if err == nil || !strings.Contains(err.Error(), "did not answer within 8s") {
		t.Errorf("err = %v", err)
	}
}
