package googleworkspace

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/kivali-ai/kivali/internal/builtinintegrations"
	"github.com/kivali-ai/kivali/internal/integration"
)

// Why this test exists: the manifest is what the install screen, the
// Connect block, the access form and the Actions list are drawn from,
// and what core signs in with. It must pass the built-in check (a setup
// connection that hands over to a browser sign-in, keeping the client
// id and secret for it, and the hosts its setup and its calls reach),
// sign in at Google's endpoints with the scopes, PKCE and the offline,
// consent prompt a refresh token needs, keep the access form's keys
// (stored access is read by them) and its three live lists, and name
// profile as Test connection without listing it. Each action must sit
// under the switch of its bucket, so the page groups it there, and be
// one Call runs, so the page never promises what an agent cannot do.
func TestManifest(t *testing.T) {
	m := New().Manifest()
	if f := integration.ValidateBuiltin(m); len(f) != 0 {
		t.Fatalf("findings: %v", f.Texts())
	}
	if m.Name != "google-workspace" || integration.ToolPrefix(m.Name) != "google_workspace_" || m.Version != "1.0.0" ||
		m.DisplayName != "Google Workspace" || len(m.UserConfig) != 0 {
		t.Errorf("manifest = %+v", m)
	}
	c := m.Kivali.Connection
	if c.Method != integration.MethodSetup || c.ThenMethod() != integration.MethodBrowser || c.Client != "" ||
		c.AuthorizationEndpoint != "https://accounts.google.com/o/oauth2/v2/auth" ||
		c.TokenEndpoint != "https://oauth2.googleapis.com/token" ||
		c.RevocationEndpoint != "https://oauth2.googleapis.com/revoke" ||
		!c.UsesPKCE() || c.PKCE == nil || c.CredentialKey() != CredentialKey ||
		len(c.ExtraAuthorizationParams) != 2 || c.ExtraAuthorizationParams["access_type"] != "offline" || c.ExtraAuthorizationParams["prompt"] != "consent" {
		t.Errorf("connection = %+v", c)
	}
	wantScopes := []string{
		"https://www.googleapis.com/auth/gmail.modify", "https://www.googleapis.com/auth/calendar.events",
		"https://www.googleapis.com/auth/calendar.readonly", "https://www.googleapis.com/auth/calendar.acls.readonly",
		"https://www.googleapis.com/auth/documents",
		"https://www.googleapis.com/auth/drive.readonly", "https://www.googleapis.com/auth/drive.file",
	}
	if !slices.Equal(c.Scopes, wantScopes) {
		t.Errorf("scopes = %v", c.Scopes)
	}
	s := c.Setup
	if s.Summary == "" || !slices.Equal(s.Hosts, []string{"console.cloud.google.com"}) {
		t.Errorf("setup = %+v", s)
	}
	if len(s.Fields) != 1 || s.Fields[0].Key != "client_json" || !s.Fields[0].Secret || !s.Fields[0].SetupOnly {
		t.Errorf("setup fields = %+v", s.Fields)
	}
	if want := []string{"client_id", "client_secret"}; !slices.Equal(s.Keeps.Keys(), want) || !s.Keeps.Secret("client_id") || !s.Keeps.Secret("client_secret") {
		t.Errorf("keeps = %+v", s.Keeps)
	}
	if want := []string{"gmail.googleapis.com", "www.googleapis.com", "docs.googleapis.com", "oauth2.googleapis.com", "accounts.google.com"}; !slices.Equal(m.Kivali.Hosts, want) {
		t.Errorf("hosts = %v", m.Kivali.Hosts)
	}
	if m.Kivali.Verify != "profile" || len(m.Kivali.Packages) != 0 || !strings.Contains(m.Kivali.SetupNotes, "{{redirect_url}}") {
		t.Errorf("kivali = %+v", m.Kivali)
	}

	form := integration.AccessForm(m)
	type field struct {
		key     string
		kind    integration.FieldKind
		choices bool
	}
	want := []field{
		{"gmail_read", integration.FieldSwitch, false}, {"gmail_write", integration.FieldSwitch, false}, {"gmail_send", integration.FieldSwitch, false},
		{"gmail_labels", integration.FieldChoices, true},
		{"calendar_read", integration.FieldSwitch, false}, {"calendar_write", integration.FieldSwitch, false}, {"calendar_send", integration.FieldSwitch, false},
		{"calendars", integration.FieldChoices, true},
		{"docs_read", integration.FieldSwitch, false}, {"docs_write", integration.FieldSwitch, false}, {"docs_send", integration.FieldSwitch, false},
		{"docs_folders", integration.FieldChoices, true},
		{"send_to", integration.FieldList, false},
	}
	var got []field
	for _, f := range form.Fields {
		got = append(got, field{f.Key, f.Kind, f.Choices})
		if f.Required || f.Title == "" || f.Description == "" || f.Title != strings.ToUpper(f.Title[:1])+f.Title[1:] {
			t.Errorf("field %s = %+v", f.Key, f)
		}
	}
	if !slices.Equal(got, want) {
		t.Errorf("access form = %+v", got)
	}

	buckets := map[string][]string{}
	for _, tl := range m.Tools {
		buckets[tl.Access] = append(buckets[tl.Access], tl.Name)
		a := actionNamed(tl.Name)
		if a == nil || tl.Description == "" || a.run == nil || strings.Contains(tl.Description, "Not available") {
			t.Errorf("%s: %q", tl.Name, tl.Description)
		}
	}
	wantBuckets := map[string][]string{
		"gmail_read":     {"list_labels", "search_messages", "get_message", "get_thread", "get_attachment"},
		"gmail_write":    {"create_draft", "delete_draft", "modify_labels"},
		"gmail_send":     {"send_email", "send_draft"},
		"calendar_read":  {"list_calendars", "list_events", "get_event", "find_free_time"},
		"calendar_write": {"create_event", "update_event", "delete_event"},
		"calendar_send":  {"respond_to_invitation"},
		"docs_read":      {"search_documents", "get_document", "list_comments"},
		"docs_write":     {"create_document", "append_to_document", "replace_text"},
		"docs_send":      {"add_comment", "reply_to_comment"},
	}
	if len(buckets) != len(wantBuckets) {
		t.Errorf("buckets = %v", buckets)
	}
	for k, names := range wantBuckets {
		if !slices.Equal(buckets[k], names) {
			t.Errorf("%s: tools %v, want %v", k, buckets[k], names)
		}
	}
	// A call that invites people, or changes or deletes an event that
	// invites others, is a send, which waits for the owner's approval:
	// the owner's words for the three actions that may make one say so.
	for _, tl := range m.Tools {
		if (tl.Name == "create_event" || tl.Name == "update_event" || tl.Name == "delete_event") && !strings.Contains(tl.Description, "waits for your approval") {
			t.Errorf("%s: %q", tl.Name, tl.Description)
		}
	}
}

// Why this test exists: an agent is given an action only while its
// access has that action's switch on, so it is never handed a tool
// that would only refuse. Every tool
// it is given must be in the manifest under that switch, carry an input
// schema and end with the sentence saying the owner decides; profile,
// Test connection's, is given to no one.
func TestToolsFollowTheSwitches(t *testing.T) {
	g := New()
	names := func(access map[string]any) []string {
		tools, err := g.Tools(context.Background(), agent(access))
		if err != nil {
			t.Fatal(err)
		}
		var out []string
		for _, tl := range tools {
			out = append(out, tl.Name)
			if !strings.HasSuffix(tl.Description, ownerDecides) || len(tl.InputSchema) == 0 {
				t.Errorf("%s: %q, schema %s", tl.Name, tl.Description, tl.InputSchema)
			}
		}
		return out
	}
	if got := names(map[string]any{}); len(got) != 0 {
		t.Errorf("no switch on: %v", got)
	}
	gmailRead := []string{"list_labels", "search_messages", "get_message", "get_thread", "get_attachment"}
	if got := names(map[string]any{"gmail_read": true}); !slices.Equal(got, gmailRead) {
		t.Errorf("gmail_read: %v", got)
	}
	// A switch stored as anything but true is off.
	if got := names(map[string]any{"gmail_read": "true", "calendar_read": "true"}); len(got) != 0 {
		t.Errorf("gmail_read as text: %v", got)
	}
	all := map[string]any{}
	for _, k := range switchKeys {
		all[k] = true
	}
	gmailWrite := []string{"create_draft", "delete_draft", "modify_labels"}
	gmailSend := []string{"send_email", "send_draft"}
	calendar := []string{"list_calendars", "list_events", "get_event", "find_free_time", "create_event", "update_event", "delete_event", "respond_to_invitation"}
	docs := []string{"search_documents", "get_document", "list_comments", "create_document", "append_to_document", "replace_text", "add_comment", "reply_to_comment"}
	if got := names(all); !slices.Equal(got, slices.Concat(gmailRead, gmailWrite, gmailSend, calendar, docs)) {
		t.Errorf("every switch: %v", got)
	}
	m := New().Manifest()
	for _, name := range names(all) {
		if !slices.ContainsFunc(m.Tools, func(tl integration.Tool) bool { return tl.Name == name }) {
			t.Errorf("%s is given and not in the manifest", name)
		}
	}
}

// Why this test exists: Call is the last check before Google is
// reached, and core's own checks only cover what the manifest says.
// An unknown action, Test connection's profile, an action whose switch
// is off, and a connection with no access token must each be refused
// as not allowed, in words naming the cause and the owner, with
// nothing sent to Google.
func TestCallRefuses(t *testing.T) {
	f, g := newFakeGoogle(t)
	reads := agent(map[string]any{"gmail_read": true, "calendar_read": true})
	refused := func(name string, caller builtinintegrations.Caller, conn builtinintegrations.Connection, text string) {
		t.Helper()
		res, err := g.Call(context.Background(), caller, conn, name, nil)
		var ref *builtinintegrations.Refusal
		if res != nil || !errors.As(err, &ref) || ref.Text != text || ref.Reason == "" {
			t.Errorf("%s: %+v, %v", name, res, err)
		}
	}
	refused("nothing", reads, testConn(), "There is no action named nothing.")
	refused("profile", reads, testConn(), "Your access to Google Workspace does not include profile; ask the owner.")
	refused("list_labels", agent(map[string]any{"calendar_read": true}), testConn(),
		"The owner has not given you reading mail in Google Workspace; ask the owner if your work needs it.")
	refused("create_event", reads, testConn(),
		"The owner has not given you changing events in Google Workspace; ask the owner if your work needs it.")
	noToken := testConn()
	delete(noToken.Fields, CredentialKey)
	refused("list_labels", reads, noToken, notSignedIn)

	if got := f.requests(); len(got) != 0 {
		t.Errorf("Google was sent %v", got)
	}
}

// Why this test exists: Test connection is how the owner learns the
// sign-in works and which Google account agents act in. It must ask
// Gmail for the profile on Gmail's origin with the access token core
// handed in, and say the account; Google refusing the token, or an API
// left off in the project, must reach the owner in words saying what
// to do, with the token nowhere in them: the fake quotes the request's
// Authorization header in its 401, so the words must carry the
// scrubbed marker where the token was.
func TestVerify(t *testing.T) {
	f, g := newFakeGoogle(t)
	msg, err := g.Verify(context.Background(), testConn())
	if err != nil || msg != "Signed in as dana@example.com" {
		t.Fatalf("verify = %q, %v", msg, err)
	}
	if got := f.requests(); !slices.Equal(got, []string{"gmail /gmail/v1/users/me/profile?"}) {
		t.Errorf("requests = %v", got)
	}

	stale := testConn()
	stale.Fields[CredentialKey] = "ya29.stale"
	if _, err := g.Verify(context.Background(), stale); err == nil || !strings.Contains(err.Error(), "HTTP 401") ||
		!strings.Contains(err.Error(), "Sign in again") || !strings.Contains(err.Error(), "Bearer [credential]") || strings.Contains(err.Error(), "ya29") {
		t.Errorf("a token Google refuses: %v", err)
	}

	f.failWith("/gmail/v1/users/me/profile", 403, `{"error":{"code":403,"message":"Gmail API has not been used in project 1 before or it is disabled.","status":"PERMISSION_DENIED","details":[{"@type":"type.googleapis.com/google.rpc.ErrorInfo","reason":"SERVICE_DISABLED"}]}}`)
	if _, err := g.Verify(context.Background(), testConn()); err == nil || !strings.Contains(err.Error(), "Gmail API has not been used") ||
		!strings.Contains(err.Error(), "Turn the API on in the app's Google Cloud project") {
		t.Errorf("an API left off: %v", err)
	}

	noToken := testConn()
	delete(noToken.Fields, CredentialKey)
	if _, err := g.Verify(context.Background(), noToken); err == nil || err.Error() != notConnected {
		t.Errorf("no token: %v", err)
	}
}

// Why this test exists: the access form's three lists are filled from
// the owner's account, and what the owner picks is stored as the
// option's text, which later calls read back for Google's id. Each
// list must come from its own API on its own origin, with the token,
// read past Google's first page, in the order the owner reads them,
// each option naming the item and its id so optionID finds the id
// again, even for a name holding parentheses.
func TestChoices(t *testing.T) {
	f, g := newFakeGoogle(t)
	list := func(name string) []string {
		t.Helper()
		out, err := g.Choices(context.Background(), testConn(), name)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		return out
	}

	labels := list("gmail_labels")
	if want := []string{"Inbox (INBOX)", "Sent (SENT)", "Drafts (DRAFT)", "Trash (TRASH)", "Alpha/Work (Label_3)", "Clients (old) (Label_12)", "zeta (Label_9)"}; !slices.Equal(labels, want) {
		t.Errorf("labels = %q", labels)
	}
	calendars := list("calendars")
	if want := []string{"dana@example.com (dana@example.com)", "Bank holidays (b@group.calendar.google.com)", "Team (team@group.calendar.google.com)", "x@group.calendar.google.com (x@group.calendar.google.com)"}; !slices.Equal(calendars, want) {
		t.Errorf("calendars = %q", calendars)
	}
	folders := list("docs_folders")
	if want := []string{"Archive (f1)", "Budget (2026) (f3)", "projects (f2)"}; !slices.Equal(folders, want) {
		t.Errorf("folders = %q", folders)
	}
	ids := map[string]string{"Clients (old) (Label_12)": "Label_12", "Bank holidays (b@group.calendar.google.com)": "b@group.calendar.google.com", "Budget (2026) (f3)": "f3"}
	for opt, id := range ids {
		if got := optionID(opt); got != id {
			t.Errorf("optionID(%q) = %q, want %q", opt, got, id)
		}
	}

	var origins []string
	for _, r := range f.requests() {
		origins = append(origins, strings.SplitN(r, "?", 2)[0])
	}
	if want := []string{"gmail /gmail/v1/users/me/labels", "www /calendar/v3/users/me/calendarList", "www /calendar/v3/users/me/calendarList", "www /drive/v3/files", "www /drive/v3/files"}; !slices.Equal(origins, want) {
		t.Errorf("requests = %v", f.requests())
	}

	if _, err := g.Choices(context.Background(), testConn(), "colors"); err == nil {
		t.Error("a list the form does not have was answered")
	}
	f.failWith("/drive/v3/files", 403, `{"error":{"code":403,"message":"Request had insufficient authentication scopes.","errors":[{"reason":"insufficientPermissions"}]}}`)
	if _, err := g.Choices(context.Background(), testConn(), "docs_folders"); err == nil || !strings.Contains(err.Error(), "allow everything Google lists") {
		t.Errorf("a permission left unticked: %v", err)
	}
}

// Why this test exists: every call carries the owner's access token,
// so the built-in must send it only to Google's own API origins. A base
// that names another host over https, plain http off loopback, or
// carries a path must be refused before any request goes; only the
// tests' loopback fake is let through over plain http.
func TestBasesArePinned(t *testing.T) {
	for _, base := range []string{"https://evil.example", "http://gmail.googleapis.com", "https://gmail.googleapis.com/x", "ftp://127.0.0.1", "https://www.googleapis.com"} {
		g := &GoogleWorkspace{GmailBase: base}
		if _, err := g.base(gmailAPI); err == nil {
			t.Errorf("gmail base %s was taken", base)
		}
	}
	for a, def := range map[api]string{gmailAPI: DefaultGmailBase, googleAPI: DefaultAPIBase, docsAPI: DefaultDocsBase} {
		u, err := New().base(a)
		if err != nil || u.String() != def {
			t.Errorf("default %d = %v, %v", a, u, err)
		}
	}
	if u, err := (&GoogleWorkspace{APIBase: "http://127.0.0.1:9/"}).base(googleAPI); err != nil || u.String() != "http://127.0.0.1:9" {
		t.Errorf("loopback = %v, %v", u, err)
	}
}
