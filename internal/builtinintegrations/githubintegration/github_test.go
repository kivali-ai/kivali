package githubintegration

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"github.com/kivali-ai/kivali/internal/builtinintegrations"
	"github.com/kivali-ai/kivali/internal/integration"
)

// Why this test exists: the manifest is what the install screen, the
// connection block and the access form are drawn from. It must pass
// the built-in check (a setup connection with its declarations, the
// hosts its setup and its calls reach, no server or packages), declare
// exactly what setup may do as the design names it (GitHub's own pages
// only, one field, the six keeps and no hand-over to a sign-in), keep
// the access form the shipped bundle has, so agents' access carries
// over, and name whoami as Test connection.
func TestManifest(t *testing.T) {
	m := New().Manifest()
	if f := integration.ValidateBuiltin(m); len(f) != 0 {
		t.Fatalf("findings: %v", f.Texts())
	}
	if m.Name != "github" || m.Version != "2.0.0" || m.DisplayName != "GitHub" || len(m.UserConfig) != 0 || m.Kivali.SetupNotes != "" {
		t.Errorf("manifest = %+v", m)
	}
	c := m.Kivali.Connection
	if c.Method != integration.MethodSetup || c.Setup == nil || c.Client != "" || c.Multiple {
		t.Fatalf("connection = %+v", c)
	}
	s := c.Setup
	if s.Summary == "" || !slices.Equal(s.Hosts, []string{"github.com"}) || s.Then != "" {
		t.Errorf("setup = %+v", s)
	}
	if len(s.Fields) != 1 || s.Fields[0].Key != "account" || s.Fields[0].Secret || s.Fields[0].SetupOnly {
		t.Errorf("setup fields = %+v", s.Fields)
	}
	if want := []string{"app_id", "slug", "client_id", "client_secret", "private_key", "installation_id"}; !slices.Equal(s.Keeps.Keys(), want) {
		t.Errorf("keeps = %v, want %v", s.Keeps.Keys(), want)
	}
	if !slices.Equal(m.Kivali.Hosts, []string{"api.github.com", "github.com"}) || m.Kivali.Verify != "whoami" || len(m.Kivali.Packages) != 0 {
		t.Errorf("kivali = %+v", m.Kivali)
	}
	form := integration.AccessForm(m)
	if form == nil || len(form.Fields) != 2 ||
		form.Fields[0].Key != "repositories" || form.Fields[0].Kind != integration.FieldList || !form.Fields[0].Required ||
		form.Fields[1].Key != "write" || form.Fields[1].Kind != integration.FieldSwitch || form.Fields[1].Title != "May create and comment" {
		t.Errorf("access form = %+v", form)
	}
	var names []string
	for _, tl := range m.Tools {
		names = append(names, tl.Name)
		if tl.Description == "" || strings.Contains(tl.Description, "signed in with") {
			t.Errorf("%s: %q", tl.Name, tl.Description)
		}
	}
	want := []string{"list_repositories", "get_repository", "list_issues", "get_issue", "create_issue", "comment",
		"list_pull_requests", "get_pull_request", "create_pull_request", "list_branches", "compare", "whoami"}
	if !slices.Equal(names, want) {
		t.Errorf("tools = %v", names)
	}
}

// Why this test exists: core keeps only the actions the manifest
// declares, and an agent sees what Tools lists. An agent without write
// must not be shown the actions that create something, an agent with it
// must be, whoami (Test connection's) is listed to none, and each
// action's schema must be JSON a model reads, naming the repository it
// acts on. Only list_issues takes a search, the one place a GitHub
// search runs.
func TestToolsFollowTheManifestAndTheAccess(t *testing.T) {
	g := New()
	var declared []string
	for _, tl := range g.Manifest().Tools {
		declared = append(declared, tl.Name)
	}
	for _, c := range []struct {
		write bool
		want  []string
	}{
		{false, []string{"list_repositories", "get_repository", "list_issues", "get_issue", "list_pull_requests", "get_pull_request", "list_branches", "compare"}},
		{true, []string{"list_repositories", "get_repository", "list_issues", "get_issue", "create_issue", "comment", "list_pull_requests", "get_pull_request", "create_pull_request", "list_branches", "compare"}},
	} {
		tools, err := g.Tools(context.Background(), agent(c.write, "acme/*"))
		if err != nil {
			t.Fatal(err)
		}
		var names []string
		for _, tl := range tools {
			names = append(names, tl.Name)
			if !slices.Contains(declared, tl.Name) {
				t.Errorf("%s is not in the manifest", tl.Name)
			}
			var s struct {
				Type       string                     `json:"type"`
				Properties map[string]json.RawMessage `json:"properties"`
				Required   []string                   `json:"required"`
			}
			if err := json.Unmarshal(tl.InputSchema, &s); err != nil || s.Type != "object" {
				t.Errorf("%s: schema %s", tl.Name, tl.InputSchema)
			}
			if _, ok := s.Properties["search"]; ok != (tl.Name == "list_issues") {
				t.Errorf("%s: search %v", tl.Name, ok)
			}
			if tl.Name != "list_repositories" && !slices.Contains(s.Required, "repository") {
				t.Errorf("%s: required %v", tl.Name, s.Required)
			}
			if !strings.HasSuffix(tl.Description, ownerDecides) || strings.Contains(tl.Description, "signed in with") {
				t.Errorf("%s: %q", tl.Name, tl.Description)
			}
		}
		if !slices.Equal(names, c.want) {
			t.Errorf("write %v: tools %v, want %v", c.write, names, c.want)
		}
	}
	// Access without the switch is off.
	tools, _ := g.Tools(context.Background(), builtinintegrations.Caller{Access: map[string]any{"repositories": []any{"acme/*"}}})
	for _, tl := range tools {
		if tl.Name == "create_issue" {
			t.Error("access without write lists create_issue")
		}
	}
}

// Why this test exists: a connection setup has not finished (no app
// id, key or installation yet) or whose values do not read cannot mint
// a token. Every call on it must be refused before anything reaches
// GitHub, in words that send the agent to the owner, and audited with
// what is wrong; a PKCS#8 key, which a key converted by hand often is,
// must read as well as GitHub's own PKCS#1.
func TestAnIncompleteConnectionRefusesEveryCall(t *testing.T) {
	g, f := newTest(t)
	for _, c := range []struct {
		drop, set, value, reason string
	}{
		{drop: FieldAppID, reason: "the connection is incomplete"},
		{drop: FieldPrivateKey, reason: "the connection is incomplete"},
		{drop: FieldInstallationID, reason: "the connection is incomplete"},
		{set: FieldPrivateKey, value: "not a key", reason: "the connection's private key does not read"},
		{set: FieldInstallationID, value: "42/../../user", reason: "the connection's app id or installation id is not a number"},
		{set: FieldAppID, value: "abc", reason: "the connection's app id or installation id is not a number"},
	} {
		conn := testConn()
		if c.drop != "" {
			delete(conn.Fields, c.drop)
		} else {
			conn.Fields[c.set] = c.value
		}
		for _, name := range []string{"list_repositories", "get_repository"} {
			r := g.callConn(t, agent(true, "acme/*"), conn, name, map[string]any{"repository": "acme/widgets"})
			if r.refusal == nil || r.text != "GitHub's connection is not set up; ask the owner to set it up again." || r.refusal.Reason != c.reason {
				t.Errorf("%+v %s: %+v", c, name, r)
			}
		}
		if _, err := g.Verify(context.Background(), conn); err == nil || !strings.Contains(err.Error(), "not set up") {
			t.Errorf("%+v: verify %v", c, err)
		}
	}
	if n := len(f.requests()); n != 0 {
		t.Errorf("GitHub took %d requests", n)
	}

	conn := testConn()
	conn.Fields[FieldPrivateKey] = pkcs8PEM(t, testKey())
	f.route("GET /repos/acme/widgets", 200, `{"full_name":"acme/widgets"}`)
	if r := g.callConn(t, agent(false, "acme/*"), conn, "get_repository", map[string]any{"repository": "acme/widgets"}); r.isError {
		t.Errorf("PKCS#8 key: %+v", r)
	}
}

// Why this test exists: whoami is the manifest's kivali.verify, what
// Test connection shows the owner: the app GitHub knows the key as and
// the account it is installed on, read with the app's own JWT (an
// installation token cannot read /app). Agents never call it: it names
// the owner's setup, not a repository.
func TestWhoamiNamesTheAppAndTheAccount(t *testing.T) {
	g, f := newTest(t)
	f.route("GET /app", 200, `{"id":1234,"slug":"kivali-acme","name":"Kivali acme"}`)
	f.route("GET /app/installations/42", 200, `{"id":42,"account":{"login":"acme","type":"Organization"}}`)
	msg, err := g.Verify(context.Background(), testConn())
	if err != nil || msg != "Connected as kivali-acme[bot], installed on acme" {
		t.Fatalf("verify = %q, %v", msg, err)
	}
	reqs := f.requests()
	if len(reqs) != 2 || reqs[0].JWT == nil || reqs[1].JWT == nil || reqs[0].JWT.ISS != testAppID {
		t.Errorf("requests %+v", reqs)
	}

	r := g.callT(t, agent(true, "*/*"), "whoami", map[string]any{})
	if r.refusal == nil || r.text != "Your access to GitHub does not include whoami; ask the owner." {
		t.Errorf("agent's whoami = %+v", r)
	}

	// The key GitHub no longer accepts reads as a setup to do again.
	f.route("GET /app", 401, `{"message":"A JSON web token could not be decoded"}`)
	if _, err := g.Verify(context.Background(), testConn()); err == nil ||
		err.Error() != "GitHub refused the GitHub App's key (HTTP 401): A JSON web token could not be decoded. Set GitHub up again." {
		t.Errorf("verify on a refused key = %v", err)
	}

	// An installation the owner removed on GitHub reads as a setup to do
	// again, not as GitHub's bare 404.
	f.route("GET /app", 200, `{"slug":"kivali-acme"}`)
	f.route("GET /app/installations/42", 404, `{"message":"Not Found"}`)
	if _, err := g.Verify(context.Background(), testConn()); err == nil ||
		err.Error() != "GitHub has no installation of the GitHub App that Kivali keeps (HTTP 404): Not Found. Set GitHub up again." {
		t.Errorf("verify on a removed installation = %v", err)
	}
}

// Why this test exists: an action the built-in does not have, and
// arguments that are not an object, are the model's slips; each is
// refused by name before anything reaches GitHub.
func TestUnknownActionsAndArgumentsAreRefused(t *testing.T) {
	g, f := newTest(t)
	if r := g.callT(t, agent(true, "*/*"), "delete_repository", nil); r.refusal == nil || r.text != "There is no action named delete_repository." {
		t.Errorf("%+v", r)
	}
	res, err := g.Call(context.Background(), agent(true, "*/*"), testConn(), "get_repository", json.RawMessage(`["acme/widgets"]`))
	if ref, ok := err.(*builtinintegrations.Refusal); res != nil || !ok || ref.Text != "The arguments are an object." {
		t.Errorf("%+v %v", res, err)
	}
	if n := len(f.requests()); n != 0 {
		t.Errorf("GitHub took %d requests", n)
	}
}
