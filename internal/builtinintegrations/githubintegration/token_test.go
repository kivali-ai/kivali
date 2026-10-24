package githubintegration

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/pem"
	"slices"
	"strings"
	"testing"
	"time"
)

// Why this test exists: GitHub takes an app's JWT only when it is
// RS256, signed by the app's key, names the app as its issuer and
// lasts at most ten minutes; one issued a moment in GitHub's future
// is refused. The claims are worked out on the server's clock: issued
// a minute back, expiring nine minutes ahead.
func TestTheJWTIsSignedOnTheServersClock(t *testing.T) {
	g, f := newTest(t)
	f.clk.Advance(90 * time.Minute)
	f.route("GET /repos/acme/widgets", 200, repoJSON)
	if r := g.callT(t, agent(false, "acme/*"), "get_repository", map[string]any{"repository": "acme/widgets"}); r.isError {
		t.Fatal(r.text)
	}
	mints := f.mints()
	if len(mints) != 1 || mints[0].JWT == nil {
		t.Fatalf("mints %+v", mints)
	}
	now := f.clk.Now()
	if c := mints[0].JWT; c.ISS != testAppID || c.IAT != now.Add(-time.Minute).Unix() || c.EXP != now.Add(9*time.Minute).Unix() {
		t.Errorf("claims %+v at %d", c, now.Unix())
	}
	if mints[0].Path != "/app/installations/42/access_tokens" || mints[0].Method != "POST" {
		t.Errorf("mint %s %s", mints[0].Method, mints[0].Path)
	}
}

// Why this test exists: an installation token reaches whatever the
// app may do on the repositories it names, for an hour. Each call's
// token is narrowed to the call's repository and to the permissions
// its action needs, and handed out again only for the same connection,
// repository and permissions, until a minute before it expires on the
// server's clock, so a call never holds more than it needs nor a token
// GitHub is about to refuse. The token is never in an answer, nor in an
// error that quotes GitHub's words.
func TestTokensAreMintedPerCallNarrowedAndCachedUntilNearExpiry(t *testing.T) {
	g, f := newTest(t)
	f.route("GET /repos/acme/widgets", 200, repoJSON)
	f.route("GET /repos/Acme/Widgets", 200, repoJSON)
	f.route("GET /repos/acme/web", 200, repoJSON)
	f.route("GET /repos/acme/widgets/branches", 200, `[]`)
	reader := agent(false, "acme/*")
	get := func(repo string) callResult {
		t.Helper()
		r := g.callT(t, reader, "get_repository", map[string]any{"repository": repo})
		if r.isError || strings.Contains(r.text, "ghs_") {
			t.Fatalf("%s: %+v", repo, r)
		}
		return r
	}
	get("acme/widgets")
	get("Acme/Widgets.git")
	if n := len(f.mints()); n != 1 {
		t.Fatalf("%d mints for one repository and one set of permissions", n)
	}
	m := f.mints()[0]
	if m.Body != `{"permissions":{"metadata":"read"},"repositories":["widgets"]}` {
		t.Errorf("mint body %s", m.Body)
	}
	api := f.api()
	if api[0].Bearer != api[1].Bearer || api[0].Grant == nil {
		t.Errorf("the second call did not reuse the token")
	}

	// Another repository, other permissions, another connection: each
	// a token of its own.
	get("acme/web")
	if r := g.callT(t, reader, "list_branches", map[string]any{"repository": "acme/widgets"}); r.isError {
		t.Fatal(r.text)
	}
	other := testConn()
	other.ID = "c0ffee02"
	if r := g.callConn(t, reader, other, "get_repository", map[string]any{"repository": "acme/widgets"}); r.isError {
		t.Fatal(r.text)
	}
	if n := len(f.mints()); n != 4 {
		t.Fatalf("%d mints, want 4", n)
	}
	var grants []string
	for _, q := range f.api() {
		grants = append(grants, strings.Join(q.Grant.Repositories, ",")+" "+permissions(q.Grant.Permissions).key())
	}
	want := []string{"widgets metadata=read", "widgets metadata=read", "web metadata=read", "widgets contents=read,metadata=read", "widgets metadata=read"}
	if !slices.Equal(grants, want) {
		t.Errorf("grants %v, want %v", grants, want)
	}

	// Minted at 0 for an hour: handed out until 59 minutes, then a new one.
	f.clk.Advance(59*time.Minute - time.Second)
	get("acme/widgets")
	if n := len(f.mints()); n != 4 {
		t.Errorf("a token a minute and a second from expiry was not reused: %d mints", n)
	}
	f.clk.Advance(time.Second)
	get("acme/widgets")
	if n := len(f.mints()); n != 5 {
		t.Errorf("a token a minute from expiry was reused: %d mints", n)
	}
	api = f.api()
	if last := api[len(api)-1]; last.Bearer == api[0].Bearer {
		t.Error("the call near expiry used the old token")
	}

	// GitHub's error words are passed on without the token, even if
	// they held it.
	f.routeFunc("GET /repos/acme/widgets", func(r recorded) reply {
		return reply{Status: 403, Body: `{"message":"token ` + r.Bearer + ` may not read this"}`}
	})
	r := g.callT(t, reader, "get_repository", map[string]any{"repository": "acme/widgets"})
	if !r.isError || strings.Contains(r.text, "ghs_") || !strings.Contains(r.text, "token [credential] may not read this") {
		t.Errorf("error words %q", r.text)
	}
}

// Why this test exists: a token GitHub says expires within the minute
// (or whose expiry does not read) must not be cached, or the next call
// would carry a token GitHub refuses; it is still used for the call it
// was minted for.
func TestAShortLivedTokenIsNotCached(t *testing.T) {
	g, f := newTest(t)
	f.tokenLife = 30 * time.Second
	f.route("GET /repos/acme/widgets", 200, repoJSON)
	for range 2 {
		if r := g.callT(t, agent(false, "acme/*"), "get_repository", map[string]any{"repository": "acme/widgets"}); r.isError {
			t.Fatal(r.text)
		}
	}
	if n := len(f.mints()); n != 2 {
		t.Errorf("%d mints, want 2", n)
	}
}

// Why this test exists: GitHub issues an app's key as PKCS#1 ("RSA
// PRIVATE KEY"); a key converted by hand is often PKCS#8. Both must
// read, and anything that is not an RSA key must not, since a JWT
// signed otherwise is one GitHub refuses.
func TestPrivateKeyFormats(t *testing.T) {
	if _, err := parsePrivateKey(pkcs1PEM(testKey())); err != nil {
		t.Errorf("PKCS#1: %v", err)
	}
	if _, err := parsePrivateKey("\n  " + pkcs8PEM(t, testKey()) + "\n"); err != nil {
		t.Errorf("PKCS#8: %v", err)
	}
	ec, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := x509.MarshalPKCS8PrivateKey(ec)
	for _, bad := range []string{
		string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: b})),
		string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: []byte("x")})),
		"-----BEGIN RSA PRIVATE KEY-----\nAAAA\n-----END RSA PRIVATE KEY-----\n",
		"not a key",
	} {
		if _, err := parsePrivateKey(bad); err == nil {
			t.Errorf("%q read as a key", bad[:min(len(bad), 40)])
		}
	}
}

// Why this test exists: setup finds the app's installation by listing
// them with the app's JWT, since the installation id GitHub sends the
// browser back with can be forged. The listing reads every page and
// carries the JWT, not an installation token.
func TestInstallationsListsWithTheJWT(t *testing.T) {
	g, f := newTest(t)
	f.route("GET /app/installations", 200, `[{"id":42,"account":{"login":"acme"}},{"id":43,"account":{"login":"dana"}}]`)
	c, err := readConnection(testConn())
	if err != nil {
		t.Fatal(err)
	}
	got, err := g.installations(context.Background(), c)
	if err != nil || len(got) != 2 || got[0] != (Installation{ID: 42, Account: "acme"}) || got[1].Account != "dana" {
		t.Fatalf("installations = %+v, %v", got, err)
	}
	reqs := f.requests()
	if len(reqs) != 1 || reqs[0].JWT == nil || reqs[0].Query.Get("per_page") != "100" {
		t.Errorf("requests %+v", reqs)
	}
}
