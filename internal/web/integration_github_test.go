package web

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
	"html"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kivali-ai/kivali/internal/builtinintegrations"
	"github.com/kivali-ai/kivali/internal/builtinintegrations/githubintegration"
	"github.com/kivali-ai/kivali/internal/clock"
	"github.com/kivali-ai/kivali/internal/connect"
	"github.com/kivali-ai/kivali/internal/integration"
	"github.com/kivali-ai/kivali/internal/secrets"
	"github.com/kivali-ai/kivali/internal/store"
	"github.com/kivali-ai/kivali/internal/web/apitypes"
)

// The tests here drive the GitHub built-in's whole setup over the HTTP
// API, on the fake clock, against a fake of GitHub's REST API: the
// manifest flow, its requirements and their Fixes, Uninstall, and the
// install over a connection GitHub 1.0.0 made.

// ghKey is the private key the fake GitHub issues for the app it makes.
var ghKey = sync.OnceValue(func() *rsa.PrivateKey {
	k, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		panic(err)
	}
	return k
})

// The app the fake GitHub makes.
const (
	ghSlug         = "kivali-acme"
	ghClientSecret = "gh-client-secret-0001"
	ghWebhook      = "gh-webhook-secret-0001"
	ghCode         = "manifest-code-0001"
)

// ghRequest is one request the fake GitHub took.
type ghRequest struct {
	Method, Path string
	Header       http.Header
	Body         string
	// JWT is true for a bearer the app's key signed.
	JWT bool
}

// fakeGitHubAPI is GitHub's REST API for the app setup makes: the
// manifest conversion (once per code), the app's installations, its
// tokens and the uninstall, holding what the test sets.
type fakeGitHubAPI struct {
	t   *testing.T
	srv *httptest.Server

	mu   sync.Mutex
	reqs []ghRequest
	// installed is the installation's id, 0 for none; perms its
	// permissions, suspended whether it is, repos how many repositories
	// it reaches.
	installed int64
	perms     map[string]string
	suspended bool
	repos     int
	// converted counts the conversions; codeUsed is true once the code
	// was traded.
	codeUsed bool
	// deleteStatus answers DELETE /app/installations/{id}.
	deleteStatus int
	// grants is every grant revoked, by access token.
	grants chan string
}

func newFakeGitHubAPI(t *testing.T) *fakeGitHubAPI {
	f := &fakeGitHubAPI{t: t, perms: map[string]string{"metadata": "read", "contents": "read", "issues": "write", "pull_requests": "write"},
		repos: 2, deleteStatus: http.StatusNoContent, grants: make(chan string, 8)}
	f.srv = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeGitHubAPI) set(fn func(f *fakeGitHubAPI)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	fn(f)
}

func (f *fakeGitHubAPI) requests() []ghRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.reqs)
}

// requestsTo is the requests taken of method and path.
func (f *fakeGitHubAPI) requestsTo(method, path string) []ghRequest {
	var out []ghRequest
	for _, r := range f.requests() {
		if r.Method == method && r.Path == path {
			out = append(out, r)
		}
	}
	return out
}

// signedByApp reports whether tok is a JWT the app's key signed.
func signedByApp(tok string) bool {
	parts := strings.Split(tok, ".")
	if len(parts) != 3 {
		return false
	}
	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		return false
	}
	sum := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	return rsa.VerifyPKCS1v15(&ghKey().PublicKey, crypto.SHA256, sum[:], sig) == nil
}

func (f *fakeGitHubAPI) serve(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	bearer, _ := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	rec := ghRequest{Method: r.Method, Path: r.URL.Path, Header: r.Header.Clone(), Body: string(body), JWT: signedByApp(bearer)}
	f.mu.Lock()
	f.reqs = append(f.reqs, rec)
	defer f.mu.Unlock()
	answer := func(status int, v any) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		if v != nil {
			_ = json.NewEncoder(w).Encode(v)
		}
	}
	notFound := func() { answer(http.StatusNotFound, map[string]string{"message": "Not Found"}) }
	inst := func() map[string]any {
		m := map[string]any{"id": f.installed, "account": map[string]string{"login": "acme", "type": "User"},
			"repository_selection": "selected", "permissions": f.perms, "suspended_at": nil}
		if f.suspended {
			m["suspended_at"] = "2026-10-01T00:00:00Z"
		}
		return m
	}
	path := r.Method + " " + r.URL.Path
	switch {
	case path == "POST /app-manifests/"+ghCode+"/conversions":
		if f.codeUsed || r.Header.Get("Authorization") != "" {
			notFound()
			return
		}
		f.codeUsed = true
		answer(http.StatusCreated, map[string]any{
			"id": 1234, "slug": ghSlug, "name": "Kivali Acme", "client_id": "Iv23liNew", "client_secret": ghClientSecret,
			"webhook_secret": ghWebhook, "owner": map[string]string{"login": "acme", "type": "User"},
			"pem": string(pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(ghKey())})),
		})
	case strings.HasPrefix(path, "POST /app-manifests/"):
		notFound()
	case (r.URL.Path == "/app" || strings.HasPrefix(r.URL.Path, "/app/")) && !rec.JWT:
		answer(http.StatusUnauthorized, map[string]string{"message": "A JSON web token could not be decoded"})
	case path == "GET /app":
		// The app asks for every permission this version needs; its
		// installation may not have approved them all (perms).
		answer(http.StatusOK, map[string]any{"id": 1234, "slug": ghSlug, "name": "Kivali Acme", "owner": map[string]string{"login": "acme", "type": "User"},
			"permissions": map[string]string{"metadata": "read", "contents": "read", "issues": "write", "pull_requests": "write"}})
	case path == "GET /app/installations":
		list := []any{}
		if f.installed != 0 {
			list = append(list, inst())
		}
		answer(http.StatusOK, list)
	case f.installed != 0 && path == "GET /app/installations/"+itoa(f.installed):
		answer(http.StatusOK, inst())
	case f.installed != 0 && path == "POST /app/installations/"+itoa(f.installed)+"/access_tokens":
		answer(http.StatusCreated, map[string]any{"token": "ghs_faketoken0001", "expires_at": time.Now().Add(time.Hour).UTC().Format(time.RFC3339)})
	case f.installed != 0 && path == "DELETE /app/installations/"+itoa(f.installed):
		answer(f.deleteStatus, nil)
	case path == "GET /installation/repositories":
		answer(http.StatusOK, map[string]any{"total_count": f.repos, "repositories": []any{}})
	case path == "DELETE /applications/Iv1old/grant":
		var b struct {
			AccessToken string `json:"access_token"`
		}
		_ = json.Unmarshal(body, &b)
		if user, pass, ok := r.BasicAuth(); !ok || user != "Iv1old" || pass != "old-client-secret" {
			answer(http.StatusUnauthorized, map[string]string{"message": "Bad credentials"})
			return
		}
		f.grants <- b.AccessToken
		answer(http.StatusNoContent, nil)
	default:
		notFound()
	}
}

func itoa(n int64) string { return strconv.FormatInt(n, 10) }

// githubServer is a server on a fake clock with the GitHub built-in
// against the fake GitHub, named Acme, its Secrets in memory.
func githubServer(t *testing.T) (*Server, *fakeGitHubAPI, *secrets.Memory, *clock.Fake) {
	t.Helper()
	srv := newTestServer(t)
	clk := clock.NewFakeAt(signInAt)
	srv.Clock = clk
	mem := secrets.NewMemory()
	srv.Secrets = mem
	f := newFakeGitHubAPI(t)
	srv.BuiltinIntegrations = builtinintegrations.Registry{&githubintegration.GitHub{APIBase: f.srv.URL, Clock: clk}}
	withIntegrationRegistry(srv, nil)
	if err := srv.Store.InstallBuiltinIntegrations(srv.BuiltinIntegrations.Manifests(), signInAt); err != nil {
		t.Fatal(err)
	}
	if err := srv.Store.WriteCompanyName("Acme"); err != nil {
		t.Fatal(err)
	}
	return srv, f, mem, clk
}

func githubPage(t *testing.T, srv *Server) apitypes.IntegrationPage {
	t.Helper()
	return orgOK[apitypes.IntegrationPage](t, orgGet(t, srv, intBase+"/github"))
}

// manifestInput is the manifest the start page's form posts.
var manifestInput = regexp.MustCompile(`<input type="hidden" name="manifest" value="([^"]*)">`)

// formAction is the start page's form's address.
var formAction = regexp.MustCompile(`<form id="go" method="post" action="([^"]*)">`)

// githubCreate takes GitHub's install from its start to the Go that
// creates the app, account blank, and returns the address the start
// page posts to, the manifest it posts and the flow's cookie.
func githubCreate(t *testing.T, srv *Server) (*url.URL, map[string]any, *http.Cookie) {
	t.Helper()
	startSetupFlow(t, srv, "github")
	f := waitFlow(t, srv, "github", apitypes.SetupFlowStateForm)
	if len(f.Step.Fields) != 1 || f.Step.Fields[0].Key != "account" {
		t.Fatalf("form = %+v", f.Step)
	}
	orgOK[apitypes.SetupFlow](t, flowDo(t, srv, http.MethodPost, intBase+"/github/setup/answer", apitypes.SetupFlowAnswer{Fields: map[string]string{"account": ""}}))
	f = waitFlow(t, srv, "github", apitypes.SetupFlowStateGo)
	if textOf(f.Step.Host) != "github.com" || f.Step.NoReturn || !strings.Contains(f.Step.Words, "GitHub may ask you to change the name") {
		t.Fatalf("create = %+v", f.Step)
	}
	rr, cookie := startPage(t, srv, f)
	body := rr.Body.String()
	action, input := formAction.FindStringSubmatch(body), manifestInput.FindStringSubmatch(body)
	if rr.Code != http.StatusOK || cookie == nil || action == nil || input == nil {
		t.Fatalf("start = %d, cookie %v, page %s", rr.Code, cookie, body)
	}
	to := mustURL(t, html.UnescapeString(action[1]))
	var m map[string]any
	if err := json.Unmarshal([]byte(html.UnescapeString(input[1])), &m); err != nil {
		t.Fatal(err)
	}
	return to, m, cookie
}

// githubBack is GitHub sending the browser back to address with query
// added, in the browser holding cookie, and returns where Kivali sends
// it on.
func githubBack(t *testing.T, srv *Server, address string, cookie *http.Cookie, query url.Values) *url.URL {
	t.Helper()
	back := mustURL(t, address)
	q := back.Query()
	for k, v := range query {
		q[k] = v
	}
	back.RawQuery = q.Encode()
	return callback(t, srv, &url.URL{Path: back.Path, RawQuery: back.RawQuery}, cookie)
}

// githubInstall runs GitHub's install to its end: created, the code
// traded, installed on GitHub (the fake lists installation 42) and the
// browser back at the setup address.
func githubInstall(t *testing.T, srv *Server, f *fakeGitHubAPI) {
	t.Helper()
	_, m, cookie := githubCreate(t, srv)
	landedOnSetup(t, githubBack(t, srv, m["redirect_url"].(string), cookie, url.Values{"code": {ghCode}}), "github")
	fl := waitFlow(t, srv, "github", apitypes.SetupFlowStateGo)
	_, cookie = startPage(t, srv, fl)
	f.set(func(f *fakeGitHubAPI) { f.installed = 42 })
	landedOnSetup(t, githubBack(t, srv, m["setup_url"].(string), cookie, url.Values{"installation_id": {"42"}, "setup_action": {"install"}}), "github")
	if fl := waitFlow(t, srv, "github", apitypes.SetupFlowStateDone); fl.Error != nil {
		t.Fatalf("done = %+v", fl)
	}
}

// githubSecret is the GitHub connection's record and Secret.
func githubSecret(t *testing.T, srv *Server, mem *secrets.Memory) (store.IntegrationConnection, map[string]string) {
	t.Helper()
	rec, err := srv.Store.GetIntegrationConnection("github")
	if err != nil {
		t.Fatal(err)
	}
	data, _ := mem.Get(context.Background(), rec.SecretName)
	return rec, data
}

// Why this test exists: this is the owner's install of GitHub, two
// clicks on GitHub and nothing copied, as core runs it end to end. The
// start page must post the manifest (named for the team, sending GitHub
// back to this flow's callback, the setup address without a query) to
// the personal account's new-app page with the flow's state; nothing is
// kept before GitHub made the app; the code GitHub sends back is traded
// once, and only the app's values are kept, never the code or the
// webhook secret; the return from the install page carries no state of
// Kivali's and is taken by the flow's cookie; the installation kept is
// the one the app's key lists, not the id in the address; and the
// integration then reads installed with every requirement met. A step
// missing is an install that cannot finish.
func TestGitHubSetupInstalls(t *testing.T) {
	srv, f, mem, _ := githubServer(t)
	to, m, cookie := githubCreate(t, srv)
	redirect := mustURL(t, m["redirect_url"].(string))
	if to.Scheme+"://"+to.Host+to.Path != "https://github.com/settings/apps/new" || to.Query().Get("state") == "" ||
		to.Query().Get("state") != redirect.Query().Get("state") {
		t.Errorf("posts to %s, sends back to %s", to, redirect)
	}
	if m["name"] != "Kivali Acme" || m["url"] != loopbackOrigin || m["setup_url"] != loopbackOrigin+"/connect/callback" ||
		!strings.HasPrefix(redirect.String(), loopbackOrigin+"/connect/callback?state=") || m["public"] != false || m["setup_on_update"] != true {
		t.Errorf("manifest = %v", m)
	}
	if _, err := srv.Store.GetIntegrationConnection("github"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("kept before GitHub made the app: %v", err)
	}
	if page := githubPage(t, srv); page.Summary.State != apitypes.IntegrationStateSettingUp {
		t.Errorf("while setting up = %s", page.Summary.State)
	}

	landedOnSetup(t, githubBack(t, srv, redirect.String(), cookie, url.Values{"code": {ghCode}}), "github")
	fl := waitFlow(t, srv, "github", apitypes.SetupFlowStateGo)
	if conv := f.requestsTo("POST", "/app-manifests/"+ghCode+"/conversions"); len(conv) != 1 || conv[0].Header.Get("Authorization") != "" {
		t.Errorf("conversions = %+v", conv)
	}
	rec, data := githubSecret(t, srv, mem)
	if want := []string{"app_id", "client_id", "client_secret", "private_key", "slug"}; !slices.Equal(rec.SecretKeys, want) || len(rec.Fields) != 0 ||
		data["app_id"] != "1234" || data["slug"] != ghSlug || data["client_secret"] != ghClientSecret || !strings.HasPrefix(data["private_key"], "-----BEGIN RSA PRIVATE KEY-----") {
		t.Fatalf("kept %v %v", rec.SecretKeys, data)
	}
	for _, v := range data {
		if v == ghWebhook || v == ghCode {
			t.Errorf("kept %q", v)
		}
	}
	rr, cookie := startPage(t, srv, fl)
	install := mustURL(t, rr.Header().Get("Location"))
	if rr.Code != http.StatusSeeOther || install.Scheme+"://"+install.Host+install.Path != "https://github.com/apps/"+ghSlug+"/installations/new" {
		t.Errorf("install = %d %s", rr.Code, install)
	}

	// GitHub sends the browser to the setup address with an installation
	// id anyone could have put there, and no state of Kivali's.
	f.set(func(f *fakeGitHubAPI) { f.installed = 42 })
	landedOnSetup(t, githubBack(t, srv, m["setup_url"].(string), cookie, url.Values{"installation_id": {"999"}, "setup_action": {"install"}}), "github")
	waitFlow(t, srv, "github", apitypes.SetupFlowStateDone)
	if rec, data = githubSecret(t, srv, mem); data["installation_id"] != "42" || !slices.Contains(rec.SecretKeys, "installation_id") {
		t.Errorf("installation kept = %v", data["installation_id"])
	}
	if list := f.requestsTo("GET", "/app/installations"); len(list) != 1 || !list[0].JWT {
		t.Errorf("listed = %+v", list)
	}
	if got := strings.Join(setupAudit(t, srv, "github"), "|"); got != "started|returned|returned|done" {
		t.Errorf("audit = %s", got)
	}
	page := githubPage(t, srv)
	if !installedAndMet(page) || !slices.Equal(requirementIDs(page), []string{"version", "connection", "key", "installed", "repositories", "permissions"}) {
		t.Fatalf("page = %s %v %v", page.Summary.State, requirementIDs(page), unmetIDs(page))
	}
	for _, r := range page.Requirements {
		if strings.Contains(r.Words, "[secret]") {
			t.Errorf("%s reads %q", r.ID, r.Words)
		}
	}
	if r, _ := requirementOf(page, "repositories"); r.Words != "Installed on acme, reaching 2 repositories." {
		t.Errorf("repositories = %q", r.Words)
	}
	// Test connection names the app in words core's scrubbing of the
	// kept values leaves readable.
	page = orgOK[apitypes.IntegrationPage](t, intPost(t, srv, intBase+"/github/connection/verify"))
	if c := page.Connection; c == nil || c.Verified == nil || !*c.Verified || textOf(c.VerifyMessage) != "Connected as the app Kivali Acme, installed on acme" {
		t.Errorf("test connection = %+v %q", c, textOf(c.VerifyMessage))
	}
}

// Why this test exists: GitHub may send the browser back before the app
// is installed anywhere (an organization's owners approve it later).
// That must wait, checked only while the owner watches, and finish once
// GitHub lists the installation, rather than fail or keep a guess.
func TestGitHubSetupWaitsForTheInstallation(t *testing.T) {
	srv, f, mem, clk := githubServer(t)
	_, m, cookie := githubCreate(t, srv)
	githubBack(t, srv, m["redirect_url"].(string), cookie, url.Values{"code": {ghCode}})
	fl := waitFlow(t, srv, "github", apitypes.SetupFlowStateGo)
	_, cookie = startPage(t, srv, fl)
	githubBack(t, srv, m["setup_url"].(string), cookie, url.Values{"setup_action": {"request"}})
	fl = waitFlow(t, srv, "github", apitypes.SetupFlowStateWait)
	if textOf(fl.Step.URL) != "" || !strings.Contains(fl.Step.Words, "approve") {
		t.Errorf("wait = %+v", fl.Step)
	}
	if _, data := githubSecret(t, srv, mem); data["installation_id"] != "" {
		t.Errorf("kept an installation: %v", data)
	}
	f.set(func(f *fakeGitHubAPI) { f.installed = 42 })
	run, _, ch, _ := srv.setupSubscribe("github")
	defer srv.setupUnsubscribe(run, ch)
	callsEnd(t, run)
	clk.BlockUntil(1)
	clk.Advance(setupWaitEvery)
	waitFlow(t, srv, "github", apitypes.SetupFlowStateDone)
	if _, data := githubSecret(t, srv, mem); data["installation_id"] != "42" {
		t.Errorf("installation = %v", data["installation_id"])
	}
}

// Why this test exists: a setup that fails or is cancelled before
// GitHub made the app keeps nothing, and one that fails after keeps
// nothing either: the integration is available again with no record
// and no Secret, so the next install starts clean.
func TestGitHubSetupFailureKeepsNothing(t *testing.T) {
	srv, f, mem, _ := githubServer(t)
	_, _, _ = githubCreate(t, srv)
	if rr := flowDo(t, srv, http.MethodPost, intBase+"/github/setup/cancel", nil); rr.Code != http.StatusNoContent {
		t.Fatalf("cancel = %d", rr.Code)
	}
	if _, err := srv.Store.GetIntegrationConnection("github"); !errors.Is(err, store.ErrNotFound) || len(mem.Names()) != 0 {
		t.Errorf("cancelled at the create: %v %v", err, mem.Names())
	}

	// A code GitHub no longer takes fails the flow in GitHub's words.
	_, m, cookie := githubCreate(t, srv)
	f.set(func(f *fakeGitHubAPI) { f.codeUsed = true })
	githubBack(t, srv, m["redirect_url"].(string), cookie, url.Values{"code": {ghCode}})
	if fl := waitFlow(t, srv, "github", apitypes.SetupFlowStateFailed); !strings.HasPrefix(textOf(fl.Error), "GitHub did not take the app's code (HTTP 404)") {
		t.Errorf("failed = %q", textOf(fl.Error))
	}
	if _, err := srv.Store.GetIntegrationConnection("github"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("a failed conversion kept a connection: %v", err)
	}

	// Made, then cancelled before the installation: the app's values go.
	f.set(func(f *fakeGitHubAPI) { f.codeUsed = false })
	_, m, cookie = githubCreate(t, srv)
	githubBack(t, srv, m["redirect_url"].(string), cookie, url.Values{"code": {ghCode}})
	waitFlow(t, srv, "github", apitypes.SetupFlowStateGo)
	flowDo(t, srv, http.MethodPost, intBase+"/github/setup/cancel", nil)
	srv.setupAwaitDiscard(context.Background(), "github")
	if _, err := srv.Store.GetIntegrationConnection("github"); !errors.Is(err, store.ErrNotFound) || len(mem.Names()) != 0 {
		t.Errorf("cancelled after the conversion: %v %v", err, mem.Names())
	}
	if page := githubPage(t, srv); page.Summary.State != apitypes.IntegrationStateAvailable {
		t.Errorf("after = %s", page.Summary.State)
	}
}

// githubRequirements is the page's rows after the requirements cache
// lapsed, so GitHub is asked again.
func githubRequirements(t *testing.T, srv *Server, clk *clock.Fake) apitypes.IntegrationPage {
	t.Helper()
	clk.Advance(requirementsCacheTTL)
	return githubPage(t, srv)
}

// Why this test exists: each requirement GitHub's health can fail must
// be a Needs-you row whose Fix mends it through the same runner as
// installing: lacking permissions go to the installation's settings on
// GitHub (a Go that comes back nowhere) and wait, checked, until
// GitHub shows them granted; a removed installation installs again.
// A Fix that names a page off its declarations, or that cannot finish,
// leaves the owner with a row nothing mends.
func TestGitHubRequirementsAndFixes(t *testing.T) {
	srv, f, mem, clk := githubServer(t)
	githubInstall(t, srv, f)

	f.set(func(f *fakeGitHubAPI) {
		f.perms = map[string]string{"metadata": "read", "contents": "read", "issues": "read", "pull_requests": "write"}
	})
	page := githubRequirements(t, srv, clk)
	if r, _ := requirementOf(page, "permissions"); r.Met || r.Fix != "permissions" || page.Summary.Needs != 1 {
		t.Fatalf("lacking = %+v, needs %d", r, page.Summary.Needs)
	}
	if needs := requirementNeeds(t, srv); needs["integration_requirement:github/permissions"].Title != "GitHub: Permissions current" {
		t.Errorf("needs = %v", needs)
	}
	orgOK[apitypes.SetupFlow](t, flowDo(t, srv, http.MethodPost, intBase+"/github/setup", apitypes.SetupFlowStart{Flow: "permissions"}))
	fl := waitFlow(t, srv, "github", apitypes.SetupFlowStateGo)
	if !fl.Step.NoReturn || textOf(fl.Step.Host) != "github.com" {
		t.Fatalf("fix = %+v", fl.Step)
	}
	rr, _ := startPage(t, srv, fl)
	if loc := rr.Header().Get("Location"); rr.Code != http.StatusSeeOther || loc != "https://github.com/settings/installations/42" {
		t.Errorf("fix goes to %d %q", rr.Code, loc)
	}
	fl = waitFlow(t, srv, "github", apitypes.SetupFlowStateWait)
	if textOf(fl.Step.URL) != "https://github.com/settings/installations/42" {
		t.Errorf("wait = %+v", fl.Step)
	}
	f.set(func(f *fakeGitHubAPI) { f.perms["issues"] = "write" })
	run, _, ch, _ := srv.setupSubscribe("github")
	callsEnd(t, run)
	clk.BlockUntil(1)
	clk.Advance(setupWaitEvery)
	waitFlow(t, srv, "github", apitypes.SetupFlowStateDone)
	srv.setupUnsubscribe(run, ch)
	if page := githubPage(t, srv); !installedAndMet(page) {
		t.Errorf("after the fix = %v", unmetIDs(page))
	}

	// Removed on GitHub: the Fix installs it again, and keeps the new
	// installation.
	f.set(func(f *fakeGitHubAPI) { f.installed = 0 })
	page = githubRequirements(t, srv, clk)
	if r, _ := requirementOf(page, "installed"); r.Met || r.Fix != "install" || r.Words != "The app is no longer installed on GitHub." {
		t.Fatalf("removed = %+v", r)
	}
	orgOK[apitypes.SetupFlow](t, flowDo(t, srv, http.MethodPost, intBase+"/github/setup", apitypes.SetupFlowStart{Flow: "install"}))
	fl = waitFlow(t, srv, "github", apitypes.SetupFlowStateGo)
	_, cookie := startPage(t, srv, fl)
	f.set(func(f *fakeGitHubAPI) { f.installed = 77 })
	githubBack(t, srv, loopbackOrigin+"/connect/callback", cookie, url.Values{"installation_id": {"77"}, "setup_action": {"install"}})
	waitFlow(t, srv, "github", apitypes.SetupFlowStateDone)
	if _, data := githubSecret(t, srv, mem); data["installation_id"] != "77" {
		t.Errorf("installation = %v", data["installation_id"])
	}
}

// Why this test exists: Uninstall must remove the installation from
// GitHub with the app's key before Kivali forgets that key, or the app
// keeps its access with nothing in Kivali to remove it; an installation
// already gone on GitHub must not block the owner's Uninstall.
func TestGitHubUninstall(t *testing.T) {
	srv, f, mem, _ := githubServer(t)
	githubInstall(t, srv, f)
	if rr := intDelete(t, srv, intBase+"/github"); rr.Code != http.StatusNoContent {
		t.Fatalf("uninstall = %d %s", rr.Code, rr.Body.String())
	}
	if del := f.requestsTo("DELETE", "/app/installations/42"); len(del) != 1 || !del[0].JWT {
		t.Errorf("deleted = %+v", del)
	}
	if page := githubPage(t, srv); page.Summary.State != apitypes.IntegrationStateAvailable || len(mem.Names()) != 0 {
		t.Errorf("after = %s, secrets %v", page.Summary.State, mem.Names())
	}

	f.set(func(f *fakeGitHubAPI) { f.codeUsed, f.installed = false, 0 })
	githubInstall(t, srv, f)
	f.set(func(f *fakeGitHubAPI) { f.deleteStatus = http.StatusNotFound })
	if rr := intDelete(t, srv, intBase+"/github"); rr.Code != http.StatusNoContent {
		t.Errorf("uninstall of one already gone = %d %s", rr.Code, rr.Body.String())
	}
	f.set(func(f *fakeGitHubAPI) {
		f.codeUsed, f.installed, f.deleteStatus = false, 0, http.StatusInternalServerError
	})
	githubInstall(t, srv, f)
	if rr := intDelete(t, srv, intBase+"/github"); rr.Code != http.StatusBadGateway || !strings.Contains(rr.Body.String(), "GitHub did not remove the app's installation") {
		t.Errorf("uninstall GitHub refused = %d %s", rr.Code, rr.Body.String())
	}
}

// githubV1 writes GitHub's record as GitHub 1.0.0 left it: version
// 1.0.0 in use, then this binary's boot; a connection holding the
// owner's own app's client id (in the record) and secret and a sign-in
// with its tokens (in the Secret); and dana's access.
func githubV1(t *testing.T, srv *Server, mem *secrets.Memory) store.IntegrationConnection {
	t.Helper()
	old := *srv.BuiltinIntegrations.Lookup("github").Manifest()
	old.Version = "1.0.0"
	old.Kivali.Connection = integration.Connection{Method: integration.MethodBrowser, Client: "operator"}
	boot := signInAt.Add(-time.Hour)
	if err := srv.Store.InstallBuiltinIntegrations([]*integration.Manifest{&old}, boot); err != nil {
		t.Fatal(err)
	}
	intTeam(t, srv, "dana")
	rec := store.IntegrationConnection{
		ID: "c0ffee01", CreatedAt: boot, Fields: map[string]string{"client_id": "Iv1old"}, SecretKeys: []string{"client_secret"},
		SecretName: secrets.ConnectionSecretName("github", "c0ffee01"), SignIn: &store.ConnectionSignIn{Method: "browser", SignedInAt: boot},
	}
	if err := mem.Put(context.Background(), rec.SecretName, map[string]string{
		"client_secret": "old-client-secret", connect.SecretKeyAccessToken: "ghu_oldaccess", connect.SecretKeyRefreshToken: "ghr_oldrefresh",
	}); err != nil {
		t.Fatal(err)
	}
	if err := srv.Store.PutIntegrationConnection("github", rec); err != nil {
		t.Fatal(err)
	}
	if err := srv.Store.SetIntegrationAccess("github", store.IntegrationAccess{Slug: "dana", SetAt: boot, SetBy: "ceo", Access: map[string]any{"repositories": []any{"acme/*"}, "write": true}}); err != nil {
		t.Fatal(err)
	}
	if err := srv.Store.InstallBuiltinIntegrations(srv.BuiltinIntegrations.Manifests(), signInAt); err != nil {
		t.Fatal(err)
	}
	return rec
}

// Why this test exists: a team upgraded from GitHub 1.0.0 holds a
// connection to the owner's own app that 2.0.0 cannot use. It must not
// read installed (agents' calls would fail, with Needs you saying
// nothing), nor be deleted at boot: the version in use becomes 2.0.0
// with 1.0.0 superseded, the record and Secret stay as they are, and
// GitHub reads available with one row saying to set it up again. The
// install then takes the record over at its first Keep: every 1.0.0
// value goes (the old app's grant revoked with GitHub), the record's id
// and dana's access stay, and once installed dana can use GitHub again
// with nothing set again.
func TestGitHubInstallsOverAOneZeroConnection(t *testing.T) {
	srv, f, mem, _ := githubServer(t)
	srv.BuiltinIntegrations.Lookup("github").(*githubintegration.GitHub).TokenURL = f.srv.URL + "/login/oauth/access_token"
	old := githubV1(t, srv, mem)
	vs, _ := srv.Store.IntegrationVersions("github")
	if len(vs) != 2 || vs[0].Version != "2.0.0" || vs[0].State != store.IntegrationVersionApproved || vs[1].State != store.IntegrationVersionSuperseded {
		t.Fatalf("versions = %+v", vs)
	}
	page := githubPage(t, srv)
	if page.Summary.State != apitypes.IntegrationStateAvailable || len(page.Requirements) != 1 || page.Requirements[0].ID != "connection" ||
		page.Requirements[0].Met || page.Requirements[0].Fix != "setup" || page.Requirements[0].Words != "Set up GitHub again: this version makes its connection by its own setup, and the one an earlier version made does not fit it. Every agent's access is kept." || page.CanUninstall {
		t.Fatalf("1.0.0's connection reads %s %+v", page.Summary.State, page.Requirements)
	}
	if len(page.Access) != 1 || page.Access[0].Invalid != nil {
		t.Errorf("access = %+v", page.Access)
	}
	if needs := requirementNeeds(t, srv); len(needs) != 0 {
		t.Errorf("needs = %v", needs)
	}
	if _, data := githubSecret(t, srv, mem); data[connect.SecretKeyAccessToken] != "ghu_oldaccess" {
		t.Errorf("boot changed the Secret: %v", data)
	}

	githubInstall(t, srv, f)
	rec, data := githubSecret(t, srv, mem)
	if rec.ID != old.ID || len(rec.Fields) != 0 || rec.SignIn != nil || data[connect.SecretKeyAccessToken] != "" || data[connect.SecretKeyRefreshToken] != "" ||
		data["client_secret"] != ghClientSecret || data["installation_id"] != "42" {
		t.Errorf("after the install: %+v %v", rec, data)
	}
	select {
	case tok := <-f.grants:
		if tok != "ghu_oldaccess" {
			t.Errorf("revoked %q", tok)
		}
	case <-time.After(failsafeWait):
		t.Error("1.0.0's grant was not revoked")
	}
	page = githubPage(t, srv)
	if !installedAndMet(page) || len(page.Access) != 1 || page.Access[0].Invalid != nil {
		t.Errorf("installed = %s %v, access %+v", page.Summary.State, unmetIDs(page), page.Access)
	}
	tab := orgOK[apitypes.AgentIntegrations](t, orgGet(t, srv, "/api/v1/agents/dana/integrations"))
	if rows := tab.Connected; len(rows) != 1 || !rows[0].Usable {
		t.Errorf("dana's tab = %+v", tab)
	}
}

// Why this test exists: an install over 1.0.0's connection that ends
// without being done after its first Keep (here, cancelled on the
// install step) must not leave the half-made app's values, nor delete
// the record and dana's access with them: the record stays, holding
// nothing, GitHub reads available again, and the next install takes it
// over as before.
func TestGitHubInstallOverOneZeroCancelledKeepsTheAccess(t *testing.T) {
	srv, f, mem, _ := githubServer(t)
	old := githubV1(t, srv, mem)
	_, m, cookie := githubCreate(t, srv)
	githubBack(t, srv, m["redirect_url"].(string), cookie, url.Values{"code": {ghCode}})
	waitFlow(t, srv, "github", apitypes.SetupFlowStateGo)
	flowDo(t, srv, http.MethodPost, intBase+"/github/setup/cancel", nil)
	srv.setupAwaitDiscard(context.Background(), "github")
	rec, err := srv.Store.GetIntegrationConnection("github")
	if err != nil || rec.ID != old.ID || len(rec.SecretKeys) != 0 || rec.SecretName != "" || len(mem.Names()) != 0 {
		t.Fatalf("after the cancel: %+v %v, secrets %v", rec, err, mem.Names())
	}
	page := githubPage(t, srv)
	if page.Summary.State != apitypes.IntegrationStateAvailable || len(page.Access) != 1 {
		t.Errorf("after = %s, access %+v", page.Summary.State, page.Access)
	}
	f.set(func(f *fakeGitHubAPI) { f.codeUsed = false })
	githubInstall(t, srv, f)
	if rec, _ := githubSecret(t, srv, mem); rec.ID != old.ID {
		t.Errorf("record = %+v", rec)
	}
	if page := githubPage(t, srv); !installedAndMet(page) || len(page.Access) != 1 {
		t.Errorf("installed again = %v, access %+v", unmetIDs(page), page.Access)
	}
}
