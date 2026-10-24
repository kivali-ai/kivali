package connect_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kivali-ai/kivali/internal/clock"
	"github.com/kivali-ai/kivali/internal/connect"
	"github.com/kivali-ai/kivali/internal/connect/connecttest"
)

// signInWith runs the authorization code grant with c at v: the
// authorization address, the vendor's approval, the exchange.
func signInWith(t *testing.T, e *connect.Engine, v *connecttest.FakeVendor, ep connect.Endpoints, c connect.Client, redirect string) connect.Tokens {
	t.Helper()
	verifier := connect.NewPKCEVerifier()
	authorize := e.AuthorizeURL(ep, c, redirect, "state-1", []string{"zi_mcp"}, verifier, nil)
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := client.Get(authorize)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	back, err := url.Parse(resp.Header.Get("Location"))
	if err != nil || back.Query().Get("code") == "" {
		t.Fatalf("the vendor answered %d, back to %q", resp.StatusCode, resp.Header.Get("Location"))
	}
	tok, err := e.Exchange(context.Background(), ep, c, back.Query().Get("code"), redirect, verifier)
	if err != nil {
		t.Fatal(err)
	}
	return tok
}

// Why this test exists: registration is what spares the owner from
// registering an app, so the request must be the standard one a server
// takes (RFC 7591: the redirect addresses, a name, the two grants Kivali
// uses, the code response type, the token endpoint authentication the
// server lists, the scopes), and the client it gives must then sign in,
// refresh, revoke and be deleted (RFC 7592) as that authentication says:
// HTTP Basic, with nothing of the secret in the body. A registration
// read wrong would leave the owner at an error page with nothing to
// paste instead.
func TestRegisterMakesAClientThatSignsIn(t *testing.T) {
	e, v, _ := setup(t)
	redirects := []string{"http://127.0.0.1:8080/connect/callback", "https://team.example.com/connect/callback"}
	reg, err := e.Register(context.Background(), v.URL+"/register", connect.RegisterRequest{
		RedirectURIs: redirects, ClientName: "Kivali Acme", Scope: "zi_mcp offline_access",
		AuthMethods: []string{connect.AuthSecretPost, connect.AuthSecretBasic},
	}, []string{"127.0.0.1"})
	if err != nil {
		t.Fatal(err)
	}
	got := v.Registered()
	if len(got) != 1 || got[0].ID != reg.Client.ID || got[0].Secret != reg.Client.Secret || reg.Client.AuthMethod != connect.AuthSecretBasic {
		t.Fatalf("registered %+v, kept %+v", got, reg)
	}
	if !slices.Equal(reg.RedirectURIs, redirects) || reg.ClientURI != v.URL+"/register/"+reg.Client.ID || reg.AccessToken == "" || !reg.SecretExpiresAt.IsZero() {
		t.Errorf("registration = %+v", reg)
	}
	body := v.Requests()[0].JSON
	want := map[string]string{
		"client_name": `Kivali Acme`, "scope": `zi_mcp offline_access`, "token_endpoint_auth_method": `client_secret_basic`,
		"redirect_uris":  `[http://127.0.0.1:8080/connect/callback https://team.example.com/connect/callback]`,
		"grant_types":    `[authorization_code refresh_token]`,
		"response_types": `[code]`,
	}
	for k, w := range want {
		if toString(body[k]) != w {
			t.Errorf("request %s = %v, want %s", k, body[k], w)
		}
	}

	ep := connect.Endpoints{Authorization: v.URL + "/authorize", Token: v.URL + "/token", Revocation: v.URL + "/revoke", Resource: v.Resource()}
	tok := signInWith(t, e, v, ep, reg.Client, redirects[1])
	if !v.Valid(tok.Access) {
		t.Fatalf("tokens %+v", tok)
	}
	if _, err := e.Refresh(context.Background(), ep, reg.Client, tok.Refresh); err != nil {
		t.Fatal(err)
	}
	if err := e.Revoke(context.Background(), ep, reg.Client, tok); err != nil {
		t.Fatal(err)
	}
	for _, r := range v.Requests() {
		if r.Path != "/token" && r.Path != "/revoke" {
			continue
		}
		user, pass, ok := (&http.Request{Header: http.Header{"Authorization": {r.Authorization}}}).BasicAuth()
		if !ok || user != reg.Client.ID || pass != reg.Client.Secret || r.Form.Has("client_secret") || r.Form.Has("client_id") {
			t.Errorf("%s: Authorization %q, form %v", r.Path, r.Authorization, r.Form)
		}
		if r.Path == "/token" && r.Form.Get("resource") != v.Resource() {
			t.Errorf("%s did not name the resource: %v", r.Path, r.Form)
		}
	}
	if len(v.Revoked()) != 2 {
		t.Errorf("revoked %v", v.Revoked())
	}
	if err := e.Deregister(context.Background(), reg.ClientURI, reg.AccessToken); err != nil {
		t.Fatal(err)
	}
	if got := v.Deregistered(); !slices.Equal(got, []string{reg.Client.ID}) || len(v.Registered()) != 0 {
		t.Errorf("deregistered %v, still registered %v", got, v.Registered())
	}
	// Deleting it again finds nothing to delete: done.
	if err := e.Deregister(context.Background(), reg.ClientURI, reg.AccessToken); err != nil {
		t.Errorf("again: %v", err)
	}
}

func toString(v any) string {
	switch x := v.(type) {
	case string:
		return x
	case []any:
		parts := make([]string, 0, len(x))
		for _, p := range x {
			parts = append(parts, toString(p))
		}
		return "[" + strings.Join(parts, " ") + "]"
	}
	return ""
}

// Why this test exists: a server that registers a public client gives
// no secret, and its token endpoint takes the client id alone; sending
// an empty secret or a Basic header would be refused, so such a client
// authenticates as none, and every grant still works.
func TestRegisterTakesAPublicClient(t *testing.T) {
	e, v, _ := setup(t)
	v.Set(func(k *connecttest.Knobs) { k.RegisterNoSecret = true })
	reg, err := e.Register(context.Background(), v.URL+"/register", connect.RegisterRequest{RedirectURIs: []string{"http://127.0.0.1:8080/connect/callback"}}, []string{"127.0.0.1"})
	if err != nil {
		t.Fatal(err)
	}
	if reg.Client.Secret != "" || reg.Client.AuthMethod != connect.AuthNone {
		t.Fatalf("client %+v", reg.Client)
	}
	ep := connect.Endpoints{Authorization: v.URL + "/authorize", Token: v.URL + "/token"}
	signInWith(t, e, v, ep, reg.Client, "http://127.0.0.1:8080/connect/callback")
	reqs := v.Requests()
	last := reqs[len(reqs)-1]
	if last.Authorization != "" || last.Form.Has("client_secret") || last.Form.Get("client_id") != reg.Client.ID {
		t.Errorf("exchange: Authorization %q, form %v", last.Authorization, last.Form)
	}
}

// Why this test exists: the token endpoint's authentication is the one
// the server lists, picked in a fixed order, so a server that takes
// only one way gets that way, and one that takes none Kivali has (a
// signed JWT) is refused before anything is registered rather than
// registering a client that can never take a token.
func TestChooseAuthMethod(t *testing.T) {
	cases := []struct {
		supported []string
		want      string
	}{
		{nil, connect.AuthSecretBasic},
		{[]string{"client_secret_post", "client_secret_basic"}, connect.AuthSecretBasic},
		{[]string{"client_secret_post"}, connect.AuthSecretPost},
		{[]string{"private_key_jwt", "none"}, connect.AuthNone},
	}
	for _, c := range cases {
		if got, err := connect.ChooseAuthMethod(c.supported); err != nil || got != c.want {
			t.Errorf("%v: %q, %v", c.supported, got, err)
		}
	}
	if _, err := connect.ChooseAuthMethod([]string{"private_key_jwt"}); err == nil || !strings.Contains(err.Error(), `only by "private_key_jwt", which Kivali does not`) {
		t.Errorf("private_key_jwt alone: %v", err)
	}
}

// Why this test exists: client credentials sent by HTTP Basic are each
// form-encoded first (RFC 6749, 2.3.1); a client id or secret with a
// colon or a plus sent raw would be split or read as another value, and
// the vendor would refuse a client it registered.
func TestBasicAuthEncodesTheCredentials(t *testing.T) {
	var header string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		header = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"access_token":"a"}`))
	}))
	t.Cleanup(srv.Close)
	e := &connect.Engine{Clock: clock.NewFake(), HTTP: &http.Client{}}
	c := connect.Client{ID: "id:with+plus", Secret: "s3cr:et/=", AuthMethod: connect.AuthSecretBasic}
	if _, err := e.Exchange(context.Background(), connect.Endpoints{Token: srv.URL}, c, "code", "http://127.0.0.1/cb", ""); err != nil {
		t.Fatal(err)
	}
	user, pass, ok := (&http.Request{Header: http.Header{"Authorization": {header}}}).BasicAuth()
	if !ok || user != url.QueryEscape(c.ID) || pass != url.QueryEscape(c.Secret) {
		t.Errorf("Authorization %q: %q %q", header, user, pass)
	}
}

// Why this test exists: a registration's answer is the server's word on
// the client Kivali signs in as, and only a whole one is kept: the
// server's refusal in its words, an answer with no client id, one that
// leaves out an address Kivali signs in at, one naming an
// authentication Kivali does not use, a failure, a redirect (never
// followed with the request), or an endpoint on a host the manifest
// does not list are each refused, and an RFC 7592 address on such a
// host is dropped, so the registration token is never sent there.
func TestRegisterRefusesWhatItCannotUse(t *testing.T) {
	req := connect.RegisterRequest{RedirectURIs: []string{"http://127.0.0.1:8080/connect/callback"}}
	t.Run("the server's refusal", func(t *testing.T) {
		e, v, _ := setup(t)
		v.Set(func(k *connecttest.Knobs) { k.RegisterError = "invalid_redirect_uri" })
		_, err := e.Register(context.Background(), v.URL+"/register", req, []string{"127.0.0.1"})
		var refused *connect.RegistrationRefused
		if !errors.As(err, &refused) || refused.Code != "invalid_redirect_uri" || err.Error() != "the service refused to register Kivali (invalid_redirect_uri): refused by the test" {
			t.Errorf("err = %v", err)
		}
	})
	t.Run("no registration endpoint", func(t *testing.T) {
		e, _, _ := setup(t)
		if _, err := e.Register(context.Background(), "", req, nil); err == nil || !strings.Contains(err.Error(), "names no registration endpoint") {
			t.Errorf("err = %v", err)
		}
	})
	t.Run("an endpoint on another host", func(t *testing.T) {
		e, v, _ := setup(t)
		if _, err := e.Register(context.Background(), v.URL+"/register", req, []string{"localhost"}); err == nil || !strings.Contains(err.Error(), "is not an address Kivali registers itself at") {
			t.Errorf("err = %v", err)
		}
		if len(v.Requests()) != 0 {
			t.Errorf("the vendor was asked %v", v.Requests())
		}
	})
	answers := []struct {
		name   string
		status int
		body   string
		want   string
	}{
		{"no client id", http.StatusCreated, `{"client_secret":"s"}`, "gave no client id"},
		{"an address left out", http.StatusCreated, `{"client_id":"c","client_secret":"s","redirect_uris":["https://other.example/cb"]}`, "did not register http://127.0.0.1:8080/connect/callback, the address Kivali signs in at"},
		{"an authentication Kivali does not use", http.StatusCreated, `{"client_id":"c","client_secret":"s","token_endpoint_auth_method":"private_key_jwt"}`, `by "private_key_jwt", which Kivali does not`},
		{"a failure", http.StatusInternalServerError, `oops`, "answered 500 Internal Server Error to Kivali's registration"},
		{"an answer Kivali cannot read", http.StatusCreated, `<html>`, "is not one Kivali reads"},
		{"a redirect", http.StatusTemporaryRedirect, ``, "answered 307 Temporary Redirect"},
	}
	for _, a := range answers {
		t.Run(a.name, func(t *testing.T) {
			hits := 0
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				hits++
				if a.status == http.StatusTemporaryRedirect {
					http.Redirect(w, r, "/elsewhere", a.status)
					return
				}
				w.WriteHeader(a.status)
				_, _ = w.Write([]byte(a.body))
			}))
			t.Cleanup(srv.Close)
			e := &connect.Engine{Clock: clock.NewFake(), HTTP: &http.Client{}}
			_, err := e.Register(context.Background(), srv.URL+"/register", req, []string{"127.0.0.1"})
			if err == nil || !strings.Contains(err.Error(), a.want) {
				t.Errorf("err = %v, want %q", err, a.want)
			}
			if hits != 1 {
				t.Errorf("the server was asked %d times", hits)
			}
		})
	}
	t.Run("a management address on another host", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"client_id":"c","client_secret":"s","client_secret_expires_at":1893456000,` +
				`"registration_access_token":"rat","registration_client_uri":"https://evil.example/register/c"}`))
		}))
		t.Cleanup(srv.Close)
		e := &connect.Engine{Clock: clock.NewFake(), HTTP: &http.Client{}}
		reg, err := e.Register(context.Background(), srv.URL+"/register", req, []string{"127.0.0.1"})
		if err != nil {
			t.Fatal(err)
		}
		if reg.ClientURI != "" || reg.AccessToken != "" || !reg.SecretExpiresAt.Equal(time.Unix(1893456000, 0)) || reg.Client.AuthMethod != connect.AuthSecretBasic {
			t.Errorf("registration = %+v", reg)
		}
	})
}

// Why this test exists: registration runs inside the sign-in the owner
// waits on, so a registration endpoint that never answers must end
// after connect.RequestTimeout on the engine's clock, as discovery
// does, and so must deleting a registration, which runs as a
// connection is deleted.
func TestRegisterTimesOutOnTheEnginesClock(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-release:
		}
	}))
	t.Cleanup(func() { close(release); srv.Close() })
	for _, call := range []func(e *connect.Engine) error{
		func(e *connect.Engine) error {
			_, err := e.Register(context.Background(), srv.URL+"/register", connect.RegisterRequest{RedirectURIs: []string{"http://127.0.0.1/cb"}}, []string{"127.0.0.1"})
			return err
		},
		func(e *connect.Engine) error { return e.Deregister(context.Background(), srv.URL+"/register/c", "rat") },
	} {
		clk := clock.NewFake()
		e := &connect.Engine{Clock: clk, HTTP: &http.Client{}}
		errc := make(chan error, 1)
		go func() { errc <- call(e) }()
		clk.BlockUntil(1)
		clk.Advance(connect.RequestTimeout)
		if err := <-errc; err == nil || !strings.Contains(err.Error(), "did not answer within 8s") {
			t.Errorf("err = %v", err)
		}
	}
}

// Why this test exists: a token or revocation request carries the
// code, the PKCE verifier, the refresh token and the client secret in
// its body, and a 307 or 308 makes an HTTP client send that body again
// to wherever the redirect names. Every such request must follow no
// redirect, so another host sees nothing, and the redirect is an
// answer Kivali does not take.
func TestTokenRequestsFollowNoRedirect(t *testing.T) {
	var other atomic.Int32
	elsewhere := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { other.Add(1) }))
	t.Cleanup(elsewhere.Close)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// The same port on another loopback name is another host.
		http.Redirect(w, r, strings.Replace(elsewhere.URL, "127.0.0.1", "localhost", 1)+r.URL.Path, http.StatusTemporaryRedirect)
	}))
	t.Cleanup(srv.Close)
	e := &connect.Engine{Clock: clock.NewFake(), HTTP: &http.Client{}}
	ep := connect.Endpoints{Token: srv.URL + "/token", Revocation: srv.URL + "/revoke"}
	for _, c := range []connect.Client{
		{ID: "id", Secret: "client-secret-value"},
		{ID: "id", Secret: "client-secret-value", AuthMethod: connect.AuthSecretBasic},
	} {
		if _, err := e.Exchange(context.Background(), ep, c, "code", "http://127.0.0.1/cb", "verifier"); err == nil || !strings.Contains(err.Error(), "the vendor answered 307 Temporary Redirect") {
			t.Errorf("exchange: %v", err)
		}
		if _, err := e.Refresh(context.Background(), ep, c, "refresh"); err == nil || !strings.Contains(err.Error(), "307") {
			t.Errorf("refresh: %v", err)
		}
		if err := e.Revoke(context.Background(), ep, c, connect.Tokens{Access: "a", Refresh: "r"}); err == nil || !strings.Contains(err.Error(), "307") {
			t.Errorf("revoke: %v", err)
		}
	}
	if n := other.Load(); n != 0 {
		t.Errorf("the other host took %d requests", n)
	}
}

// Why this test exists: a manifest's hosts are host names, and one
// listed is the service's on its usual port, not every port on that
// machine: an endpoint on another port could be another service. A
// loopback host (a vendor run in-process in tests) has a port of its
// own, and an address given with its port matches as given.
func TestHostAllowedHoldsToThePort(t *testing.T) {
	hosts := []string{"login.example.com", "127.0.0.1", "api.example.com:8443"}
	cases := map[string]bool{
		"https://login.example.com/token":     true,
		"https://login.example.com:443/token": true,
		"https://login.example.com:8443/x":    false,
		"http://127.0.0.1:54321/token":        true,
		"https://api.example.com:8443/token":  true,
		"https://api.example.com/token":       false,
		"https://evil.example/token":          false,
	}
	for raw, want := range cases {
		u, _ := url.Parse(raw)
		if got := connect.HostAllowed(u, hosts); got != want {
			t.Errorf("%s: %v, want %v", raw, got, want)
		}
	}
}

// Why this test exists: deleting a registration runs as the owner
// signs out or removes the connection, and a service that no longer
// takes the registration's token (it expired it, or deleted the client
// itself) answers 401; that is a registration already gone, not a
// failure to log on every delete.
func TestDeregisterTakesAnUnknownTokenForDone(t *testing.T) {
	e, v, _ := setup(t)
	reg, err := e.Register(context.Background(), v.URL+"/register", connect.RegisterRequest{RedirectURIs: []string{"http://127.0.0.1/cb"}}, []string{"127.0.0.1"})
	if err != nil {
		t.Fatal(err)
	}
	if err := e.Deregister(context.Background(), reg.ClientURI, "not-the-registration-token"); err != nil {
		t.Errorf("a 401: %v", err)
	}
	if len(v.Deregistered()) != 0 {
		t.Errorf("deleted with the wrong token: %v", v.Deregistered())
	}
}
