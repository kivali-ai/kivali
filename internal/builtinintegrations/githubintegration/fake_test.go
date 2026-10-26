package githubintegration

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kivali-ai/kivali/internal/builtinintegrations"
	"github.com/kivali-ai/kivali/internal/clock"
)

// testKey is the app's private key in every test: generated once, as
// GitHub would issue it.
var testKey = sync.OnceValue(func() *rsa.PrivateKey {
	k, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		panic(err)
	}
	return k
})

func pkcs1PEM(k *rsa.PrivateKey) string {
	return string(pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(k)}))
}

func pkcs8PEM(t *testing.T, k *rsa.PrivateKey) string {
	b, err := x509.MarshalPKCS8PrivateKey(k)
	if err != nil {
		t.Fatal(err)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: b}))
}

// The connection's values in every test, and the app's client id and
// secret, which GitHub's conversion answer carries and setup does not
// keep.
const (
	testAppID          = "1234"
	testSlug           = "kivali-acme"
	testInstallationID = "42"
	testClientID       = "Iv23liTest"
	testClientSecret   = "test-client-secret-0000"
)

func testConn() builtinintegrations.Connection {
	return builtinintegrations.Connection{ID: "c0ffee01", Fields: map[string]string{
		FieldAppID: testAppID, FieldSlug: testSlug, FieldPrivateKey: pkcs1PEM(testKey()),
		FieldInstallationID: testInstallationID, FieldInstallationAccount: "acme",
	}}
}

// clientConn is testConn holding the app's client id and secret as
// well, as a setup that kept them left it.
func clientConn() builtinintegrations.Connection {
	c := testConn()
	c.Fields["client_id"], c.Fields["client_secret"] = testClientID, testClientSecret
	return c
}

// agent is a caller whose access names patterns, writing or not.
func agent(write bool, patterns ...string) builtinintegrations.Caller {
	list := make([]any, len(patterns))
	for i, p := range patterns {
		list[i] = p
	}
	return builtinintegrations.Caller{Agent: "dana", Access: map[string]any{"repositories": list, "write": write}}
}

// jwtClaims are a JWT's claims as the fake read them.
type jwtClaims struct {
	IAT int64  `json:"iat"`
	EXP int64  `json:"exp"`
	ISS string `json:"iss"`
}

// grant is what an installation token the fake minted may do.
type grant struct {
	Repositories []string          `json:"repositories"`
	Permissions  map[string]string `json:"permissions"`
}

// recorded is one request the fake took.
type recorded struct {
	Method string
	Path   string
	Query  url.Values
	Header http.Header
	// Bearer is the Authorization header's credential.
	Bearer string
	// JWT is the claims of a bearer JWT whose signature the app's key
	// made; nil for any other credential.
	JWT *jwtClaims
	// Grant is what the bearer installation token may do; nil for any
	// other credential.
	Grant *grant
	Body  string
}

// reply is a route's answer.
type reply struct {
	Status int
	Body   string
	Header map[string]string
}

// fakeGitHub is GitHub's REST API in the test's process: it mints
// installation tokens from JWTs the app's key signs, on the test's
// clock, records every request, and answers each route the test gives.
type fakeGitHub struct {
	t   *testing.T
	srv *httptest.Server
	clk *clock.Fake

	mu     sync.Mutex
	reqs   []recorded
	routes map[string]func(r recorded) reply
	grants map[string]grant
	minted int
	// tokenLife is how long a minted token lasts.
	tokenLife time.Duration
	// mintReply, when set, answers the token endpoint instead.
	mintReply *reply
}

func newFake(t *testing.T) *fakeGitHub {
	f := &fakeGitHub{t: t, clk: clock.NewFake(), routes: map[string]func(recorded) reply{}, grants: map[string]grant{}, tokenLife: time.Hour}
	f.srv = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.srv.Close)
	return f
}

// newTest is the built-in pointed at a fresh fake, on its clock.
func newTest(t *testing.T) (*GitHub, *fakeGitHub) {
	f := newFake(t)
	return &GitHub{APIBase: f.srv.URL, Clock: f.clk}, f
}

// route answers method and path (no query) with r.
func (f *fakeGitHub) route(methodPath string, status int, body string) {
	f.routeFunc(methodPath, func(recorded) reply { return reply{Status: status, Body: body} })
}

func (f *fakeGitHub) routeFunc(methodPath string, fn func(r recorded) reply) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.routes[methodPath] = fn
}

// requests is every request taken, in order; api the ones that were
// not for a token.
func (f *fakeGitHub) requests() []recorded {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]recorded(nil), f.reqs...)
}

func (f *fakeGitHub) api() []recorded {
	var out []recorded
	for _, r := range f.requests() {
		if !strings.HasSuffix(r.Path, "/access_tokens") {
			out = append(out, r)
		}
	}
	return out
}

func (f *fakeGitHub) mints() []recorded {
	var out []recorded
	for _, r := range f.requests() {
		if strings.HasSuffix(r.Path, "/access_tokens") {
			out = append(out, r)
		}
	}
	return out
}

// verifyJWT reads tok as a JWT the app's key signed with RS256.
func verifyJWT(tok string) (*jwtClaims, error) {
	parts := strings.Split(tok, ".")
	if len(parts) != 3 {
		return nil, errors.New("not a JWT")
	}
	header, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return nil, err
	}
	var h struct{ Alg, Typ string }
	if err := json.Unmarshal(header, &h); err != nil || h.Alg != "RS256" || h.Typ != "JWT" {
		return nil, fmt.Errorf("header %s", header)
	}
	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		return nil, err
	}
	sum := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	if err := rsa.VerifyPKCS1v15(&testKey().PublicKey, crypto.SHA256, sum[:], sig); err != nil {
		return nil, err
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return nil, err
	}
	var c jwtClaims
	if err := json.Unmarshal(payload, &c); err != nil {
		return nil, err
	}
	return &c, nil
}

// mintPath is the token endpoint of any installation.
var mintPath = regexp.MustCompile(`^/app/installations/[0-9]+/access_tokens$`)

func (f *fakeGitHub) serve(w http.ResponseWriter, req *http.Request) {
	body, _ := io.ReadAll(req.Body)
	rec := recorded{Method: req.Method, Path: req.URL.Path, Query: req.URL.Query(), Header: req.Header.Clone(), Body: string(body)}
	rec.Bearer, _ = strings.CutPrefix(req.Header.Get("Authorization"), "Bearer ")
	if c, err := verifyJWT(rec.Bearer); err == nil {
		rec.JWT = c
	}
	f.mu.Lock()
	if g, ok := f.grants[rec.Bearer]; ok {
		rec.Grant = &g
	}
	f.reqs = append(f.reqs, rec)
	fn := f.routes[req.Method+" "+req.URL.Path]
	mintReply := f.mintReply
	f.mu.Unlock()

	write := func(r reply) {
		for k, v := range r.Header {
			w.Header().Set(k, v)
		}
		w.WriteHeader(r.Status)
		_, _ = io.WriteString(w, r.Body)
	}
	if req.Method == http.MethodPost && mintPath.MatchString(req.URL.Path) {
		switch {
		case mintReply != nil:
			write(*mintReply)
		case rec.JWT == nil:
			write(reply{Status: 401, Body: `{"message":"A JSON web token could not be decoded"}`})
		default:
			write(f.mint(body))
		}
		return
	}
	if fn == nil {
		write(reply{Status: 404, Body: `{"message":"Not Found","documentation_url":"https://docs.github.com/rest"}`})
		return
	}
	write(fn(rec))
}

// mint answers the token endpoint: a fresh token for what body asks.
func (f *fakeGitHub) mint(body []byte) reply {
	var g grant
	if err := json.Unmarshal(body, &g); err != nil {
		return reply{Status: 400, Body: `{"message":"Problems parsing JSON"}`}
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.minted++
	tok := fmt.Sprintf("ghs_fakeinstallationtoken%04d", f.minted)
	f.grants[tok] = g
	b, _ := json.Marshal(map[string]any{
		"token": tok, "expires_at": f.clk.Now().Add(f.tokenLife).UTC().Format(time.RFC3339),
		"permissions": g.Permissions, "repository_selection": "selected",
	})
	return reply{Status: 201, Body: string(b)}
}

// callResult is a call's outcome: its text, whether it is an error
// result, and the refusal when it was refused.
type callResult struct {
	text    string
	isError bool
	refusal *builtinintegrations.Refusal
	// audit is the Result's audit reason.
	audit string
}

func (g *GitHub) callT(t *testing.T, caller builtinintegrations.Caller, name string, a map[string]any) callResult {
	t.Helper()
	return g.callConn(t, caller, testConn(), name, a)
}

func (g *GitHub) callConn(t *testing.T, caller builtinintegrations.Caller, conn builtinintegrations.Connection, name string, a map[string]any) callResult {
	t.Helper()
	raw, err := json.Marshal(a)
	if err != nil {
		t.Fatal(err)
	}
	res, err := g.Call(context.Background(), caller, conn, name, raw)
	var ref *builtinintegrations.Refusal
	switch {
	case errors.As(err, &ref):
		return callResult{text: ref.Text, isError: true, refusal: ref}
	case err != nil:
		t.Fatalf("%s: %v", name, err)
	}
	if len(res.Content) != 1 {
		t.Fatalf("%s: content %q", name, res.Content)
	}
	return callResult{text: res.Content[0], isError: res.IsError, audit: res.Audit}
}
