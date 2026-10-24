// Package githubintegration is the GitHub built-in integration: agents
// read and open issues, comments and pull requests in GitHub
// repositories, acting as a private GitHub App that the owner's setup
// makes on their account and installs on the repositories they choose.
//
// Every call acts as the app's installation: core signs a JSON web
// token with the app's private key and trades it for an installation
// access token (POST /app/installations/{id}/access_tokens), narrowed
// to the call's repository and to the permissions its action needs
// (token.go). A token is kept in this process's memory until a minute
// before it expires, and nowhere else. The actions are GitHub's REST
// API (actions.go), each held to the agent's access first (access.go).
//
// The connection is made by the integration's own setup flow (method
// setup): GitHub's app-manifest flow keeps the app's id, slug, client
// id and secret and private key after GitHub's conversion, and the
// installation id once the app is installed. This package does not
// implement that flow, the integration's requirements or its
// uninstall, and it is not in builtins.All().
package githubintegration

import (
	"context"
	"crypto/rsa"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/kivali-ai/kivali/internal/builtinintegrations"
	"github.com/kivali-ai/kivali/internal/clock"
	"github.com/kivali-ai/kivali/internal/integration"
)

// Name is the built-in's name; its actions are github_<action>.
const Name = "github"

// The connection's fields: what setup keeps, read as conn.Fields.
// client_id and client_secret are kept for the app's own sign-in and
// read by no action.
const (
	FieldAppID          = "app_id"
	FieldSlug           = "slug"
	FieldClientID       = "client_id"
	FieldClientSecret   = "client_secret"
	FieldPrivateKey     = "private_key"
	FieldInstallationID = "installation_id"
)

// GitHub is the built-in. APIBase and Clock are test hooks; New gives
// what the binary runs.
type GitHub struct {
	// APIBase is GitHub's REST API; empty is DefaultAPIBase. It must be
	// https, or plain http on loopback for the tests' fake.
	APIBase string
	// Clock is the server's clock, which JWTs are signed on and cached
	// tokens expire on; nil is the system's.
	Clock clock.Clock

	clientOnce sync.Once
	client     *http.Client

	tokensMu sync.Mutex
	tokens   map[tokenKey]cachedToken
}

// New is the built-in as the binary runs it.
func New() *GitHub { return &GitHub{APIBase: DefaultAPIBase, Clock: clock.New()} }

var _ builtinintegrations.Integration = (*GitHub)(nil)

func (g *GitHub) now() time.Time {
	if g.Clock == nil {
		return time.Now()
	}
	return g.Clock.Now()
}

// setupSummary is what setup does, in words for the owner.
const setupSummary = "Creates a private GitHub App on your account that may write contents, issues and pull requests, and installs it on the repositories you choose."

// accessSchema is the access form: the repositories an agent may use,
// and whether it may create issues, comments and pull requests there.
const accessSchema = `{"type":"object","properties":{` +
	`"repositories":{"type":"array","items":{"type":"string"},"title":"Repositories","description":"Each one owner/name, such as acme/widgets, or a pattern such as acme/*, which covers every repository directly under acme. * is the only wildcard. Capitals do not matter."},` +
	`"write":{"type":"boolean","title":"May create and comment","description":"Lets the agent open issues and pull requests and comment on them in those repositories, as the GitHub App."}},` +
	`"required":["repositories"]}`

// Manifest is GitHub's manifest.
func (g *GitHub) Manifest() *integration.Manifest {
	tools := make([]integration.Tool, len(actions))
	for i, a := range actions {
		tools[i] = integration.Tool{Name: a.name, Description: a.ownerDescription}
	}
	return &integration.Manifest{
		ManifestVersion: "0.3",
		Name:            Name,
		Version:         "2.0.0",
		DisplayName:     "GitHub",
		Description:     "Read and open issues, comments and pull requests in GitHub repositories, as a private GitHub App on your account.",
		Author:          integration.Author{Name: "Kivali"},
		UserConfig:      integration.UserConfig{},
		Tools:           tools,
		Kivali: integration.Kivali{
			Connection: integration.Connection{
				Method: integration.MethodSetup,
				Setup: &integration.Setup{
					Summary: setupSummary,
					Hosts:   []string{"github.com"},
					Fields: integration.SetupFields{
						{Key: "account", Words: "Your personal account, or an organization's name"},
					},
					Keeps: integration.SetupKeeps{
						{Key: FieldAppID, Words: "The app's id"},
						{Key: FieldSlug, Words: "The app's name on GitHub"},
						{Key: FieldClientID, Words: "The app's client id"},
						{Key: FieldClientSecret, Words: "The app's client secret"},
						{Key: FieldPrivateKey, Words: "The app's private key, which Kivali signs with to get a token for each call"},
						{Key: FieldInstallationID, Words: "Where the app is installed"},
					},
				},
			},
			Hosts:  []string{"api.github.com", "github.com"},
			Access: &integration.Access{Schema: json.RawMessage(accessSchema)},
			Verify: actionWhoami,
		},
	}
}

// Tools lists every action the caller's access allows: the write
// actions only with write, and never whoami, which is Test
// connection's.
func (g *GitHub) Tools(_ context.Context, caller builtinintegrations.Caller) ([]builtinintegrations.Tool, error) {
	acc := readAccess(caller.Access)
	var out []builtinintegrations.Tool
	for _, a := range actions {
		if a.ownerOnly || (a.write && !acc.write) {
			continue
		}
		out = append(out, builtinintegrations.Tool{Name: a.name, Description: a.description + ownerDecides, InputSchema: json.RawMessage(a.schema)})
	}
	return out, nil
}

// Choices: GitHub's access form lists no options live.
func (g *GitHub) Choices(context.Context, builtinintegrations.Connection, string) ([]string, error) {
	return nil, errors.New("GitHub lists no options")
}

// connection is a connection's values as the actions use them.
type connection struct {
	id             string
	appID          string
	slug           string
	installationID string
	key            *rsa.PrivateKey
}

// notSetUp is the words of a call on a connection setup has not
// finished, or whose values do not read.
const notSetUp = "GitHub's connection is not set up; ask the owner to set it up again."

// readConnection reads conn's app id, private key and installation id,
// all three needed for a token; the ids are whole numbers, as GitHub
// gives them. The error says what is wrong, for the audit.
func readConnection(conn builtinintegrations.Connection) (connection, error) {
	c := connection{
		id:             conn.ID,
		appID:          strings.TrimSpace(conn.Fields[FieldAppID]),
		slug:           strings.TrimSpace(conn.Fields[FieldSlug]),
		installationID: strings.TrimSpace(conn.Fields[FieldInstallationID]),
	}
	pemKey := conn.Fields[FieldPrivateKey]
	if c.appID == "" || c.installationID == "" || strings.TrimSpace(pemKey) == "" {
		return c, errors.New("the connection is incomplete")
	}
	if !digits(c.appID) || !digits(c.installationID) {
		return c, errors.New("the connection's app id or installation id is not a number")
	}
	key, err := parsePrivateKey(pemKey)
	if err != nil {
		return c, errors.New("the connection's private key does not read")
	}
	c.key = key
	return c, nil
}

func digits(s string) bool {
	if s == "" || len(s) > 20 {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// Call runs the action name for the caller: the action, its access and
// the connection are checked before anything reaches GitHub, each
// refused with a Refusal; a GitHub failure is an error result in words.
func (g *GitHub) Call(ctx context.Context, caller builtinintegrations.Caller, conn builtinintegrations.Connection, name string, raw json.RawMessage) (*builtinintegrations.Result, error) {
	a := actionNamed(name)
	switch {
	case a == nil:
		return nil, refuse("There is no action named "+name+".", "no such action")
	case a.ownerOnly:
		return nil, refuse("Your access to GitHub does not include "+name+"; ask the owner.", name+" is Test connection's alone")
	}
	acc := readAccess(caller.Access)
	if a.write && !acc.write {
		return nil, refuse(writeRefusal, "the access does not include creating issues, comments or pull requests")
	}
	c, err := readConnection(conn)
	if err != nil {
		return nil, refuse(notSetUp, err.Error())
	}
	in, err := readArgs(raw)
	if err != nil {
		return nil, err
	}
	answer, err := a.run(ctx, &call{g: g, conn: c, access: acc, args: in})
	if err != nil {
		var ref *builtinintegrations.Refusal
		var te *tokenError
		var ae *apiError
		switch {
		case errors.As(err, &ref):
			return nil, err
		case errors.As(err, &te):
			return failed(te.words()), nil
		case errors.As(err, &ae):
			return failed(agentWords(ae)), nil
		}
		return nil, err
	}
	text, err := answerText(answer)
	if err != nil {
		return nil, err
	}
	return &builtinintegrations.Result{Content: []string{text}}, nil
}

func failed(words string) *builtinintegrations.Result {
	return &builtinintegrations.Result{Content: []string{words}, IsError: true}
}

// clipNote follows an answer cut at answerClip.
const clipNote = "\n[The answer is cut to its first 60000 characters.]"

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

// Verify is Test connection: the app GitHub knows the key as (GET
// /app) and the account its installation is on (GET
// /app/installations/{id}), both with the app's JWT. "Connected as
// kivali-acme[bot], installed on acme".
func (g *GitHub) Verify(ctx context.Context, conn builtinintegrations.Connection) (string, error) {
	c, err := readConnection(conn)
	if err != nil {
		return "", errors.New("GitHub's connection is not set up; set it up again")
	}
	jwt, err := g.jwt(c)
	if err != nil {
		return "", err
	}
	var app struct {
		Slug string `json:"slug"`
	}
	if err := g.getJSON(ctx, jwt, "/app", &app); err != nil {
		return "", ownerError(err)
	}
	var inst struct {
		Account struct {
			Login string `json:"login"`
		} `json:"account"`
	}
	if err := g.getJSON(ctx, jwt, "/app/installations/"+url.PathEscape(c.installationID), &inst); err != nil {
		return "", ownerError(err)
	}
	slug := app.Slug
	if slug == "" {
		slug = c.slug
	}
	return "Connected as " + slug + "[bot], installed on " + inst.Account.Login, nil
}

// ownerError is err in words for the owner.
func ownerError(err error) error {
	var ae *apiError
	if errors.As(err, &ae) {
		return errors.New(ownerWords(ae))
	}
	return err
}

// Setup, Requirements and Uninstall (builtinintegrations.SetupIntegration,
// RequirementsOf and Uninstaller) are not implemented here; they belong
// in setup.go, beside this file. The flow keeps the fields above (the
// installation id after checking it against installations, since the
// one GitHub sends the browser back with can be forged), and Uninstall
// is DELETE /app/installations/{id} with the app's JWT.
