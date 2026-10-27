// Package googleworkspace is the Google Workspace built-in integration:
// agents work in the owner's Gmail, Google Calendar and Google Docs,
// signed in with an OAuth client the owner makes on Google's Auth
// Platform, in a Google Cloud project of theirs.
//
// The connection is made by the integration's own setup flow (method
// setup, setup.go): one Form, where the owner pastes the client's JSON,
// whose client id and secret are kept; then core's browser sign-in with
// that client (setup.then), whose tokens core holds and renews. Every
// call is given the current access token in Connection.Fields, under
// CredentialKey, and nothing else of the sign-in. Its requirements
// (requirements.go) add Google's likely cause to a sign-in Google no
// longer accepts.
//
// The access form (access.go) has three switches per service, one per
// bucket: read (sees what is in the account), write (changes something
// in the account that no other person is told about or can see) and
// send (anything that makes Google deliver something to another
// person, or changes a calendar or a document another person can see,
// shared.go),
// and per service the labels, calendars or folders an agent may use,
// which Choices lists live. Every action belongs to one bucket, and an
// agent is given it only while its access has that switch on. The
// actions are per service (gmail.go, calendar.go, docs.go), joined in
// actions; Gmail's reading is gmail_read.go, the text it hands the
// agent gmail_text.go, its attachments gmail_attach.go and its sends
// gmail_send.go. A send never reaches anyone from its call: it holds
// the call for the owner's approval (held), and only the hold's Run,
// once the owner approves, sends what the call captured.
//
// Calls reach Google's REST APIs (api.go) at three origins, each
// pinned: gmail.googleapis.com, www.googleapis.com (Calendar and Drive)
// and docs.googleapis.com.
package googleworkspace

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/kivali-ai/kivali/internal/builtinintegrations"
	"github.com/kivali-ai/kivali/internal/clock"
	"github.com/kivali-ai/kivali/internal/integration"
)

// Name is the built-in's name; its actions are google_workspace_<action>.
const Name = "google-workspace"

// displayName is the built-in's name in words.
const displayName = "Google Workspace"

// CredentialKey is the name the access token goes under in
// Connection.Fields (the manifest's credential_keys).
const CredentialKey = "GOOGLE_ACCESS_TOKEN"

// Google's OAuth endpoints, which core signs in, renews and revokes
// with.
const (
	authorizationEndpoint = "https://accounts.google.com/o/oauth2/v2/auth"
	tokenEndpoint         = "https://oauth2.googleapis.com/token"
	revocationEndpoint    = "https://oauth2.googleapis.com/revoke"
)

// scopes are what the sign-in asks Google for: Gmail's modify alone,
// which reads, composes, sends and changes the labels of mail (and
// takes nothing from its settings); Calendar's events and calendars
// read, and its sharing read, for who sees a change to a calendar
// (calendarShare); Docs; Drive read, for finding documents and folders
// and who they are shared with (fileShare); and Drive's files Kivali
// makes, for creating a document with Kivali's mark, in its folder, and
// commenting on the documents Kivali made, the only ones Drive takes a
// comment on under it.
var scopes = []string{
	"https://www.googleapis.com/auth/gmail.modify",
	"https://www.googleapis.com/auth/calendar.events",
	"https://www.googleapis.com/auth/calendar.readonly",
	"https://www.googleapis.com/auth/calendar.acls.readonly",
	"https://www.googleapis.com/auth/documents",
	"https://www.googleapis.com/auth/drive.readonly",
	"https://www.googleapis.com/auth/drive.file",
}

// GoogleWorkspace is the built-in. GmailBase, APIBase and DocsBase are
// test hooks: Google's API origins, empty for DefaultGmailBase,
// DefaultAPIBase and DefaultDocsBase. Each must be its default, or
// plain http on loopback for the tests' fake.
type GoogleWorkspace struct {
	GmailBase, APIBase, DocsBase string

	// Clock is the time source for what Kivali dates and for Calendar's
	// "now"; nil is the system's.
	Clock clock.Clock

	clientOnce sync.Once
	client     *http.Client
}

// now is the time by Clock, or the system's.
func (g *GoogleWorkspace) now() time.Time {
	if g.Clock == nil {
		return time.Now()
	}
	return g.Clock.Now()
}

// New is the built-in as the binary runs it.
func New() *GoogleWorkspace { return &GoogleWorkspace{} }

var (
	_ builtinintegrations.SetupIntegration = (*GoogleWorkspace)(nil)
	_ builtinintegrations.SignInAdviser    = (*GoogleWorkspace)(nil)
	_ builtinintegrations.AccessChecker    = (*GoogleWorkspace)(nil)
)

// action is one action. description is what the agent's model reads,
// ownerDescription the manifest's, for the owner. bucket is the access
// form's switch that gives it, the manifest tool's access. ownerOnly
// keeps an action for Test connection, out of the manifest's tools,
// and with no run.
type action struct {
	name             string
	description      string
	ownerDescription string
	schema           string
	bucket           string
	ownerOnly        bool
	run              func(ctx context.Context, c *call) (any, error)
}

// actions is every action, Gmail's, then Calendar's, then Docs': the
// manifest's tools are these in this order, less the ownerOnly one.
var actions = slices.Concat(gmailActions, calendarActions, docsActions)

// actionNamed is the action called name, nil for none.
func actionNamed(name string) *action {
	for i := range actions {
		if actions[i].name == name {
			return &actions[i]
		}
	}
	return nil
}

// ownerDecides ends every description an agent reads.
const ownerDecides = " The owner decides what you may read, change and send in Gmail, Google Calendar and Google Docs, and which labels, calendars and folders you may use."

// setupSummary is what setup does, in words for the owner.
const setupSummary = "Keeps the client id and secret of the OAuth client you make on Google's Auth Platform, from the client's JSON you paste, then signs you in to Google with that client. Kivali makes nothing on Google itself."

// Manifest is Google Workspace's manifest. Its tools are every action
// an agent may be given, each under the switch that gives it; profile
// is Test connection's (kivali.verify) and no tool.
func (g *GoogleWorkspace) Manifest() *integration.Manifest {
	var tools []integration.Tool
	for _, a := range actions {
		if a.ownerOnly {
			continue
		}
		tools = append(tools, integration.Tool{Name: a.name, Description: a.ownerDescription, Access: a.bucket})
	}
	pkce := true
	return &integration.Manifest{
		ManifestVersion: "0.3",
		Name:            Name,
		Version:         "1.0.0",
		DisplayName:     displayName,
		Description:     "Gmail, Google Calendar and Google Docs, signed in with an OAuth client you make on Google's Auth Platform. Agents can read your Gmail, write drafts in it, label its mail and send mail you approve, read your Google Calendar, change its events, and invite people and answer invitations once you approve, and read and write your Google Docs and comment on those Kivali made once you approve.",
		Author:          integration.Author{Name: "Kivali"},
		UserConfig:      integration.UserConfig{},
		Tools:           tools,
		Kivali: integration.Kivali{
			Connection: integration.Connection{
				Method:                   integration.MethodSetup,
				AuthorizationEndpoint:    authorizationEndpoint,
				TokenEndpoint:            tokenEndpoint,
				RevocationEndpoint:       revocationEndpoint,
				Scopes:                   slices.Clone(scopes),
				PKCE:                     &pkce,
				ExtraAuthorizationParams: map[string]string{"access_type": "offline", "prompt": "consent"},
				CredentialKeys:           map[string]string{integration.CredentialAccessToken: CredentialKey},
				Setup: &integration.Setup{
					Summary: setupSummary,
					Hosts:   []string{"console.cloud.google.com"},
					Fields: integration.SetupFields{
						{Key: fieldClientJSON, Title: "Client JSON", Words: clientJSONWords, Secret: true, SetupOnly: true},
					},
					Keeps: integration.SetupKeeps{
						{Key: integration.FieldClientID, Words: "The OAuth client's id, which Kivali signs in to Google with"},
						{Key: integration.FieldClientSecret, Words: "The OAuth client's secret, which Kivali signs in to Google and renews the sign-in with"},
					},
					Then: integration.MethodBrowser,
				},
			},
			Hosts: []string{"gmail.googleapis.com", "www.googleapis.com", "docs.googleapis.com", "oauth2.googleapis.com", "accounts.google.com"},
			Access: &integration.Access{
				Schema:  json.RawMessage(accessSchema),
				Choices: map[string]string{keyGmailLabels: keyGmailLabels, keyCalendars: keyCalendars, keyDocsFolders: keyDocsFolders},
			},
			Verify:     actionProfile,
			SetupNotes: setupNotes,
		},
	}
}

// Tools lists every action the caller's access gives, in the
// manifest's order: those whose switch it has on, never profile, which
// is Test connection's.
func (g *GoogleWorkspace) Tools(_ context.Context, caller builtinintegrations.Caller) ([]builtinintegrations.Tool, error) {
	acc := readAccess(caller.Access)
	var out []builtinintegrations.Tool
	for _, a := range actions {
		if a.ownerOnly || !acc.on(a.bucket) {
			continue
		}
		out = append(out, builtinintegrations.Tool{Name: a.name, Description: a.description + ownerDecides, InputSchema: json.RawMessage(a.schema)})
	}
	return out, nil
}

// call is one action's run: the access token core handed in, the
// caller's access, the model's arguments and where it may save a file
// (Caller.Save, nil when core gave none). A run sets audit, the reason
// the owner's audit keeps, kept on an error result too (failedWith),
// and outside, who outside the owner's domain wrote what it returned
// (Result.Audit and Result.Outside). me and labelList are what the run
// read of the account, kept for this call alone.
type call struct {
	g      *GoogleWorkspace
	token  string
	access access
	args   args
	save   builtinintegrations.SaveFunc
	// agent is the calling agent's slug (Caller.Agent), which an event,
	// a document or a draft Kivali makes is marked with (marker).
	agent string

	audit   string
	outside []string

	me        *account
	labelList []label
}

// agentSlugPattern is an agent's slug as a mark writes it.
var agentSlugPattern = regexp.MustCompile(`^[A-Za-z0-9._-]{1,64}$`)

// marker is what marks a thing the calling agent makes as Kivali's: an
// event's private property (kivaliMarker), a document's app property
// (kivaliMark), a draft's header (markerHeader). It is the agent's
// slug, or "kivali" for a call that names none or a slug a mark cannot
// carry as it is.
func (c *call) marker() string {
	if agentSlugPattern.MatchString(c.agent) {
		return c.agent
	}
	return "kivali"
}

// notSignedIn is the words of a call whose connection holds no access
// token: core refuses a call while the sign-in needs the owner, so it
// is a connection setup did not finish.
const notSignedIn = "Google Workspace's connection is not signed in; ask the owner to set it up again."

// accessToken is the access token core put in conn; an error, for the
// audit, when there is none.
func accessToken(conn builtinintegrations.Connection) (string, error) {
	tok := strings.TrimSpace(conn.Fields[CredentialKey])
	if tok == "" {
		return "", errors.New("the connection holds no access token")
	}
	return tok, nil
}

// Call runs the action name for the caller: the action, its switch and
// the connection are checked before anything reaches Google, each
// refused with a Refusal; a Google failure is an error result in
// words, with the audit the run had set; a send that held its call
// answers Result.Hold (holdResult).
func (g *GoogleWorkspace) Call(ctx context.Context, caller builtinintegrations.Caller, conn builtinintegrations.Connection, name string, raw json.RawMessage) (*builtinintegrations.Result, error) {
	a := actionNamed(name)
	switch {
	case a == nil:
		return nil, refuse("There is no action named "+name+".", "no such action")
	case a.ownerOnly:
		return nil, refuse("Your access to Google Workspace does not include "+name+"; ask the owner.", name+" is Test connection's alone")
	}
	acc := readAccess(caller.Access)
	if !acc.on(a.bucket) {
		return nil, notGiven(a.bucket)
	}
	tok, err := accessToken(conn)
	if err != nil {
		return nil, refuse(notSignedIn, err.Error())
	}
	in, err := readArgs(raw)
	if err != nil {
		return nil, err
	}
	c := &call{g: g, token: tok, access: acc, args: in, save: caller.Save, agent: caller.Agent}
	answer, err := a.run(ctx, c)
	if h, ok := answer.(*held); ok && err == nil {
		return g.holdResult(c.agent, h), nil
	}
	return c.result(answer, err)
}

// result is a run's answer, or its failure, as Call answers it: a
// Refusal as it is; a Google failure, or the run's own, as an error
// result in words, with the audit the run had set; the answer as its
// text, with the call's audit and outside.
func (c *call) result(answer any, err error) (*builtinintegrations.Result, error) {
	if err != nil {
		var ref *builtinintegrations.Refusal
		var ae *apiError
		var f failure
		switch {
		case errors.As(err, &ref):
			return nil, err
		case errors.As(err, &ae):
			return c.failedWith(agentWords(ae)), nil
		case errors.As(err, &f):
			return c.failedWith(string(f)), nil
		}
		return nil, err
	}
	text, err := answerText(answer)
	if err != nil {
		return nil, err
	}
	return &builtinintegrations.Result{Content: []string{text}, Audit: c.audit, Outside: c.outside}, nil
}

func failed(words string) *builtinintegrations.Result {
	return &builtinintegrations.Result{Content: []string{words}, IsError: true}
}

// held is a send's answer from its run: the call waits for the owner's
// approval (Result.Hold), shown to the owner as title, fields and
// recipients, and nothing has reached anyone. needs is the send switch
// that lets the agent ask for it (gmail_send, calendar_send or
// docs_send), which core holds the access to again at approval
// (Hold.Switch). run is what the owner approves: it does exactly what
// the fields show, from what the send captured when it held the call,
// never reading the agent's arguments or Google again for what it
// sends.
type held struct {
	title      string
	fields     []builtinintegrations.HoldField
	recipients []string
	needs      string
	run        func(ctx context.Context, c *call) (any, error)
}

// field is one line of a held call's card.
func field(label, value string) builtinintegrations.HoldField {
	return builtinintegrations.HoldField{Label: label, Value: value}
}

// holdResult is h as Call answers it, for the agent agent, its Switch
// h.needs. Its Run reads the access token core hands it, at the owner's
// approval, and runs h.run with a call of its own, answering as Call
// does: the agent's access is not read again, since core has checked
// it then, h.needs included.
func (g *GoogleWorkspace) holdResult(agent string, h *held) *builtinintegrations.Result {
	return &builtinintegrations.Result{Hold: &builtinintegrations.Hold{
		Title:      h.title,
		Fields:     h.fields,
		Recipients: h.recipients,
		Switch:     h.needs,
		Run: func(ctx context.Context, conn builtinintegrations.Connection) (*builtinintegrations.Result, error) {
			tok, err := accessToken(conn)
			if err != nil {
				return nil, refuse(notSignedIn, err.Error())
			}
			c := &call{g: g, token: tok, agent: agent}
			answer, err := h.run(ctx, c)
			return c.result(answer, err)
		},
	}}
}

// shownLine is text a card shows the owner and Kivali does not send,
// such as the subject of the message a reply answers or the name of a
// document: one line of at most most characters (oneLine), with no
// format character left, since core refuses a card that holds one and
// this text is not the agent's to change. shownText is the same for
// text of many lines, cut at shownMost characters.
func shownLine(s string, most int) string { return dropFormat(oneLine(s, most)) }

func shownText(s string) string {
	s, _ = cleanText(s)
	if cut, clipped := clip(strings.TrimSpace(s), shownMost); clipped {
		s = cut + "..."
	}
	return dropFormat(strings.TrimSpace(s))
}

// shownMost bounds a card's text that Kivali shows and does not send.
const shownMost = 20000

// dropFormat is s without its Unicode format characters (Cf), the
// zero-width joiner cleanText keeps among them.
func dropFormat(s string) string {
	return strings.Map(func(r rune) rune {
		if unicode.Is(unicode.Cf, r) {
			return -1
		}
		return r
	}, s)
}

// hidesText reports whether s holds a Unicode format character (Cf)
// other than the zero-width joiner emoji are made with: one that shows
// as nothing on the owner's card, which core refuses.
func hidesText(s string) bool {
	return strings.ContainsFunc(s, func(r rune) bool { return r != '\u200d' && unicode.Is(unicode.Cf, r) })
}

// gone is a send's Run finding what it acts on no longer there, Google
// answering 404 or 410: a Refusal, since nothing was done, in the words
// "<what> is gone; nothing was done." (what is "The event", "The
// comment", ...), with reason for the audit. nil for any other failure.
func gone(err error, what, reason string) error {
	var ae *apiError
	if errors.As(err, &ae) && (ae.Status == http.StatusNotFound || ae.Status == http.StatusGone) {
		return refuse(what+" is gone; nothing was done.", reason)
	}
	return nil
}

// unshownPeople names, on a card and in an answer, the people a send
// reaches whose address Google does not show.
const unshownPeople = "others whose address Google does not show"

// withUnshown is people in words, then, when unknown, unshownPeople:
// "alice@example.org and others whose address Google does not show".
// It is "" for no one.
func withUnshown(people []string, unknown bool) string {
	w := strings.Join(people, ", ")
	switch {
	case !unknown:
		return w
	case w == "":
		return "people whose address Google does not show"
	}
	return w + " and " + unshownPeople
}

// unshownField is the field a card ends with when the send reaches
// people whose address Google does not show.
func unshownField() builtinintegrations.HoldField {
	return field("Also reaches", "People whose address Google does not show")
}

// waitsWords end the description of every action that waits for the
// owner's approval, and sendWaitsWords of one whose calls wait when
// they are sends, which the agent reads.
const (
	waitsTail      = "it answers at once that it is waiting, and you hear in your chat whether it was done, with what Google answered, or the owner's note if they denied it."
	waitsWords     = " The call waits for the owner's approval: " + waitsTail
	sendWaitsWords = " A send needs the owner to have given you inviting people, and waits for the owner's approval: " + waitsTail
)

// failedWith is the error result of a run that failed with words. Its
// audit is what the run had set, what it did or was doing, then the
// words, so a write Google did not answer still names its document;
// with none set, core keeps the words alone.
func (c *call) failedWith(words string) *builtinintegrations.Result {
	r := failed(words)
	if c.audit != "" {
		r.Audit = c.audit + "; " + words
	}
	return r
}

// failure is an action that reached Google and did not do what it was
// asked, in its own words for the agent: an error result.
type failure string

func (f failure) Error() string { return string(f) }

// answerClip is the most an answer's text holds, whatever the action,
// and clipNote follows an answer cut there.
const (
	answerClip = 60000
	clipNote   = "\n[The answer is cut to its first 60000 characters.]"
)

// answerText is an action's answer as the agent reads it: text, or
// compact JSON, cut at answerClip characters with a note saying so.
func answerText(v any) (string, error) {
	s, ok := v.(string)
	if !ok {
		var b strings.Builder
		enc := json.NewEncoder(&b)
		enc.SetEscapeHTML(false)
		if err := enc.Encode(v); err != nil {
			return "", err
		}
		s = strings.TrimSuffix(b.String(), "\n")
	}
	if cut, clipped := clip(s, answerClip); clipped {
		s = cut + clipNote
	}
	return s, nil
}

// notConnected is Test connection's and the options' words for a
// connection that holds no access token, which follow core's ("The
// connection test failed: ...").
const notConnected = "the connection holds no sign-in; sign in again"

// Verify is Test connection: the Google account the sign-in is for, as
// Gmail names it (users/me/profile). "Signed in as dana@example.com".
func (g *GoogleWorkspace) Verify(ctx context.Context, conn builtinintegrations.Connection) (string, error) {
	tok, err := accessToken(conn)
	if err != nil {
		return "", errors.New(notConnected)
	}
	p, err := g.profile(ctx, tok)
	if err != nil {
		return "", ownerError(err)
	}
	if p.EmailAddress == "" {
		return "", errors.New("the profile Gmail answered names no account")
	}
	return "Signed in as " + p.EmailAddress, nil
}

// Choices lists the options of the access form's live lists, each as
// option writes it: the labels (gmail.go), the calendars (calendar.go)
// and the folders (docs.go) the sign-in reaches.
func (g *GoogleWorkspace) Choices(ctx context.Context, conn builtinintegrations.Connection, name string) ([]string, error) {
	var list func(ctx context.Context, token string) ([]string, error)
	switch name {
	case keyGmailLabels:
		list = g.labelOptions
	case keyCalendars:
		list = g.calendarOptions
	case keyDocsFolders:
		list = g.folderOptions
	default:
		return nil, errors.New("Google Workspace lists no options for " + name)
	}
	tok, err := accessToken(conn)
	if err != nil {
		return nil, errors.New(notConnected)
	}
	out, err := list(ctx, tok)
	if err != nil {
		return nil, ownerError(err)
	}
	return out, nil
}
