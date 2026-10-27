// Package builtinintegrations holds the integrations that ship with
// Kivali.
//
// An integration is normally a bundle: a Python server the owner (or
// the Kivali Developer) publishes, approves and runs in a pod of its
// own. A built-in integration lives in the binary instead, and its
// actions are Go functions core calls directly, with no pod and no
// shim.
//
// It has the same contract surface as a bundle. Its manifest has the
// bundle's shape (name, display_name, description, user_config, tools,
// kivali.connection, kivali.access, kivali.verify), so the Org page,
// the install screen, the connection form and the access form are drawn
// by the same code, and core checks every agent's access as it does for
// a bundle. What differs:
//
//   - Its manifest has no server section and no packages
//     (integration.ValidateBuiltin): a built-in runs inside Kivali, and
//     its network reach is Kivali's own, the hosts its connection names;
//     one whose own setup flow makes its connection declares the hosts
//     its setup and its calls reach, for the install screen.
//   - It needs no install approval: the owner approved the binary. Boot
//     writes its record and its version (store.InstallBuiltinIntegrations).
//   - While the binary carries it, it cannot be replaced by an upload,
//     and Disconnect takes it back to installed, with no connection,
//     rather than Uninstall off the team; it can be disabled.
//
// To add one, implement Integration in a package of its own and list it
// in builtins.All (internal/builtinintegrations/builtins).
package builtinintegrations

import (
	"context"
	"encoding/json"
	"net/url"

	"github.com/kivali-ai/kivali/internal/integration"
	"github.com/kivali-ai/kivali/internal/shelljob"
)

// Integration is one built-in integration. Core calls every method
// after it has checked the caller's access; the connection is read at
// the moment of the call and kept by no one.
//
// Tools, Call, Verify and Choices run concurrently: many agents call at
// once, and every agent's list is asked for in parallel with the other
// integrations'. Each runs under the deadline a pod's request has (60
// seconds for a call, Test connection and options, 20 for a list); one
// still running then is abandoned and its answer dropped, and its
// context ends, which it must honour so it stops.
type Integration interface {
	// Manifest is the integration's manifest. Its version is the
	// built-in's own, semantic, bumped only when the manifest's surface
	// changes (connection fields, actions, the access form). It is
	// called on every lookup, concurrently, so it must be cheap and
	// safe to call from many goroutines.
	Manifest() *integration.Manifest
	// Tools is the actions caller is listed, unprefixed. Every name
	// must be one of the manifest's tools: core leaves out any other.
	// It may leave out the actions caller's access does not allow.
	Tools(ctx context.Context, caller Caller) ([]Tool, error)
	// Call runs the action name for caller with args, the model's
	// arguments as JSON. A failure the agent should read is a Result
	// with IsError; an error is the integration's own failure, which
	// the agent reads as the integration answering with an error.
	Call(ctx context.Context, caller Caller, conn Connection, name string, args json.RawMessage) (*Result, error)
	// Verify is Test connection: a nil error is a connection that
	// works, with message saying what it found ("Connected as ...").
	// An error is one that does not, in words for the owner.
	Verify(ctx context.Context, conn Connection) (message string, err error)
	// Choices lists the options of an access field listed live: name is
	// what kivali.access.choices maps the field to.
	Choices(ctx context.Context, conn Connection, name string) ([]string, error)
}

// Caller is the agent a call or a list is for, and its access: the
// agent's filled access form (empty for an integration without one).
type Caller struct {
	Agent  string
	Access map[string]any
}

// Connection is the connection's values, read by core at the moment of
// the call: its id and every field by user_config key, secrets
// included. For a connection made by signing in, Fields also holds the
// client id and the current access token, under the name the
// manifest's credential_keys gives it, and never the client secret or
// the refresh token.
type Connection struct {
	ID     string
	Fields map[string]string
}

// Tool is one action as an agent is given it: its unprefixed name, its
// description and its input schema (an object's; empty is
// {"type":"object"}).
type Tool struct {
	Name        string
	Description string
	InputSchema json.RawMessage
}

// Result is a call's answer: each string one text content for the
// agent, and whether the call failed.
//
// Audit, when set, is what the owner's audit keeps as the call's
// reason; empty, it keeps the answer's words when the call failed, as
// for a bundle.
//
// ShellJob, set by Call of a ShellJobIntegration, is work for the
// agent's shell: core hands it to the agent's MCP server, which runs it
// and sends the call again with the result, and core then calls
// Complete. Content and IsError are not read then; Audit is the reason
// of the row core writes as it hands the job out ("started: ..."), and
// Complete's Result gives the reason of the row that ends the call.
type Result struct {
	Content  []string
	IsError  bool
	Audit    string
	ShellJob *shelljob.Job
}

// ShellJobIntegration is an Integration that runs part of an action in
// the agent's shell: its Call answers with Result.ShellJob, and Complete
// turns what the shell gave back into the answer. Core calls Complete
// with the agent, its access and the connection worked out again on
// the second request, exactly as for Call, and with the same name and
// arguments; result is untrusted text from the agent's pod, which
// Complete only puts in words.
type ShellJobIntegration interface {
	Integration
	Complete(ctx context.Context, caller Caller, conn Connection, name string, args json.RawMessage, result shelljob.Result) (*Result, error)
}

// HostsOf is an Integration whose shell jobs reach the network through
// the egress proxy: Hosts is what its connection reaches. Core gives
// the connection a scoped proxy credential of its own, admitted to
// those hosts and no others (ProxyUser), and puts it in the job.
type HostsOf interface {
	Hosts(conn Connection) []string
}

// ConnectionChecker is an Integration that holds a connection to more
// than its form does. Saving the connection (PUT .../connection, and a
// setup flow's Form) calls CheckConnection after the form's own checks and before it stores anything; an error
// is a 400 with its words, a sentence for the owner. conn.ID is empty
// for a new connection.
type ConnectionChecker interface {
	CheckConnection(conn Connection) error
}

// AccessChecker is an Integration that holds an agent's access to more
// than its form does. PUT .../access/{slug} calls CheckAccess after
// integration.ValidateAccess; an error is a 400 with its words.
type AccessChecker interface {
	CheckAccess(access map[string]any) error
}

// AccessSummarizer is an Integration that puts one agent's access in
// words itself, for an access form whose fields mean something only
// together (Git's push and the refs it may push). Core shows it in
// place of integration.AccessSummary's field-by-field words. access is
// the stored form, which may no longer fit the manifest's.
type AccessSummarizer interface {
	AccessSummary(access map[string]any) string
}

// SetupIntegration is an Integration whose manifest names method setup:
// its own flow makes the connection, from steps core runs and draws.
// Core calls Setup in the background, off any request, under a
// deadline on its clock: first with an Answer holding nothing, then
// after each step with what that step gave back, until Setup returns a
// Step that is Done or an error.
//
// flow is FlowSetup for installing, or the Fix a Requirement named.
// conn.Fields holds what earlier steps kept, so a later step reads it
// back. Every Step is held to the manifest's kivali.connection.setup:
// a Form may ask only for declared fields, a Go or a Wait may name a
// page only on a declared host, and Keep may hold only declared keys;
// a Step that does not keep to that fails the flow. An error's text
// is shown to the owner, scrubbed of every secret kept value, and the flow
// keeps nothing more. An install (FlowSetup, from no connection) that
// fails, is cancelled or is forgotten keeps nothing at all: core first
// calls Uninstall, for an Uninstaller, with the connection as kept so
// far, best effort, then forgets the connection its Keep made. An
// install started while another is under way takes that one's place,
// and the connection it made with what it kept. A Fix's flow that
// fails leaves what it kept.
type SetupIntegration interface {
	Integration
	Setup(ctx context.Context, conn Connection, flow string, last Answer, p Progress) (Step, error)
}

// FlowSetup is the flow that installs the integration: the one the
// owner starts from Installed, to connect it.
const FlowSetup = "setup"

// Step is what the owner does next. Exactly one of Form, Go, Wait or
// Done is set.
type Step struct {
	// Form asks the owner for fields.
	Form *Form
	// Go sends the owner's browser to the service, and back unless it
	// says otherwise.
	Go *Go
	// Wait shows words and a page, and has core call Setup again with
	// Checked every few seconds while the owner's page is open.
	Wait *Wait
	// Done ends the flow: the connection exists (installing) or the Fix
	// is made.
	Done bool

	// Keep is written into the connection's Secret now, under keys the
	// manifest's setup.keeps declares. A kept value is the connection's
	// from then on, read back as conn.Fields.
	Keep map[string]string
	// State is carried to the next Answer as it is, in core's memory,
	// for this flow alone: a setup-only field's value, a code to trade
	// later, where the flow is. Never written anywhere.
	State map[string]string
}

// Form is a step the owner fills in. Core draws the fields under the
// integration's name.
type Form struct {
	// Words say what to do, for the owner.
	Words string
	// Fields names the fields to ask for, in the order drawn: keys of
	// the manifest's setup.fields.
	Fields []string
}

// Go is a step that sends the owner's browser to the service, by a
// link (a 303 to URL) or by a form that posts itself (Post to URL, for a
// service that takes a POST). Unless NoReturn is set, core expects the
// browser back at the callback the Answer gave (Answer.Callback), where
// the service's query reaches Setup as the next Answer's Return.
type Go struct {
	// Words say what the owner will do there.
	Words string
	// URL is where the browser goes; its host is one of setup.hosts.
	URL string
	// Post, when set, is posted to URL by a form in the owner's browser
	// instead of a link.
	Post url.Values
	// NoReturn says the service sends the browser nowhere afterwards (a
	// settings page the owner acts on): core sends the browser and
	// calls Setup again at once with Answer.Went, so Setup can follow
	// with a Wait that checks the service.
	NoReturn bool
	// Stateless says the service sends the browser back without the
	// state it was given (an address fixed when an app was made that
	// carries none): core accepts a return with no state for this step,
	// matched by the flow's cookie alone. Without it, a return with no
	// state fails the flow, since any page the owner visits can make
	// the browser load the callback with the cookie and a query of its
	// own.
	Stateless bool
}

// Wait is a step that waits for something to happen on the service.
type Wait struct {
	// Words say what to do, or what is being waited for.
	Words string
	// URL is a page for the owner to open meanwhile, on one of
	// setup.hosts; empty for none.
	URL string
	// Code is a code the owner enters on that page, shown large with a
	// Copy button; empty for none. It is shown as given, cut to 64
	// characters and never scrubbed: Setup chose to show it, so it is
	// never a value Setup keeps secret.
	Code string
}

// Answer is what the last step gave back: Setup reads it and returns
// the next Step. The first call of a flow gets an Answer with State
// nil, no Form, no Return, and Checked and Went false.
type Answer struct {
	// State is the State the last Step carried, as it was.
	State map[string]string
	// Form is what the owner entered, by field key, after a Form step:
	// every key the Form asked for is there, empty for a field left
	// blank.
	Form map[string]string
	// Return is the query the service sent the browser back with,
	// after a Go step. It is untrusted: the service sent it, but anyone
	// can make the browser load the callback with any query, and a
	// callback replayed from an earlier leg (with a state core issued
	// before) is delivered as the current Go step's Return. Setup checks
	// it against the service before relying on it. Core scrubs from the
	// flow's words the values of code, state and any key whose name
	// holds token, secret, password or key; no other returned value is
	// scrubbed, so Setup puts anything else it must keep hidden into
	// State, whose values are.
	Return url.Values
	// Checked is true for a Wait step's check: Setup looks at the
	// service and returns the same Wait to keep waiting, or the next
	// Step.
	Checked bool
	// Went is true after a Go step with NoReturn: the browser was sent,
	// and nothing came back.
	Went bool
	// Leg counts the calls of Setup in this flow, from 1 for the first,
	// so a driver can tell which call an Answer is for.
	Leg int
	// Callback is the address a Go step's service may send the browser
	// back to, with a state of this flow's in it: what Setup puts in a
	// manifest's redirect_url or a form's return field. Core issues a
	// fresh state for each call (a Wait's check is given the latest
	// again, as it may come every few seconds for as long as the Wait
	// lasts) and accepts, while the flow lives, any
	// state it issued for the flow, so an address fixed in an earlier
	// step (a setup URL registered with the app) still comes back to
	// the flow. A return with no state is accepted only for a Go step
	// that says Stateless, in the browser that left.
	Callback string
	// Addresses is every address the team signs in at, each the bare
	// callback (<scheme>://<host>/connect/callback, no query): the one
	// the request that last touched the flow reached Kivali at, the
	// address Callback is built from, first, then the team's external
	// address when it has one. A setup that registers callbacks with an
	// app the service makes (redirect URIs for a sign-in Then hands over
	// to) registers them all, so a sign-in from any of them comes back.
	Addresses []string
	// Team is the team's name as its owner set it, for naming what
	// setup makes on the service; empty when the team has none.
	Team string
}

// Progress names the stage Setup is at; the page shows it as it runs.
// done and total give a real progress bar where there is a count,
// 0 and 0 where there is none.
type Progress interface {
	Stage(words string, done, total int)
}

// RequirementsOf is an Integration with health of its own: rows that
// are met or not, each with a fix. Core lists its own rows first (an
// approved version, the connection, a sign-in still accepted, the
// connection test passing, the integration running), then these. An
// unmet row is a Needs you item, and its Fix runs a flow through Setup
// with the same steps as installing. Requirements is called when the
// page, the list or Home is read, under a five-second deadline,
// concurrently with everything else, and must honour ctx.
type RequirementsOf interface {
	Requirements(ctx context.Context, conn Connection) ([]Requirement, error)
}

// Requirement is one row of an integration's health.
type Requirement struct {
	// ID names the row, stable across calls: letters, digits, hyphens
	// and underscores.
	ID string
	// Title is the row in a few words ("Installed on GitHub"); Words
	// say what is, met or not ("Installed on acme, reaching 12
	// repositories", "The app is no longer installed on GitHub").
	Title, Words string
	Met          bool
	// Fix is the flow its Fix button runs through Setup, in place of
	// FlowSetup; "" for a row with nothing to run.
	Fix string
}

// Uninstaller is an Integration that undoes its installation on the
// service: Uninstall is called before core forgets the connection's
// Secret and drops every agent's access, once per connection. An error
// is shown to the owner and leaves the installation as it is.
//
// Core may call Uninstall again for an installation it already undid:
// when forgetting the connection failed after Uninstall succeeded, the
// owner uninstalls again, and conn may then lack values an earlier
// step kept. Uninstall treats an installation that is already gone on
// the service, or that conn cannot name, as done, and returns nil.
//
// Core may also call Uninstall for a partial installation: an install
// that failed, was cancelled or was forgotten before it was done, with
// the connection as its steps kept it so far, before core forgets it.
// Uninstall undoes what that connection names and treats what is
// missing as already gone. That call is best effort, under the same
// deadline: an error is logged, and the connection goes all the same.
type Uninstaller interface {
	Uninstall(ctx context.Context, conn Connection) error
}

// SignInReplacer is a setup integration whose earlier version made its
// connection by signing in with an app of the owner's. When an install
// of this version takes over such a connection, its first Keep
// replaces every value the connection held, and core then hands the
// sign-in it held to RevokeSignIn, once, off any lock and best effort:
// an error is logged, and nothing else depends on it.
type SignInReplacer interface {
	RevokeSignIn(ctx context.Context, old SignIn) error
}

// SignInAdviser is an Integration connected by signing in, or by a
// setup that hands over to a sign-in, that knows its service's likely
// cause of a refused renewal. While the service no longer accepts the
// sign-in, core's Sign-in requirement, and the Needs you row it raises,
// follow core's own words with SignInAdvice's: refusal is core's words
// of the refusal ("the vendor refused the sign-in (invalid_grant):
// ..."), and "" adds nothing. It is called each time the requirements
// are read, with no connection and no deadline, so it must be cheap and
// must not reach the service. Core calls it from several goroutines at
// once (Home builds each integration's rows on its own), so it must be
// safe for concurrent use.
type SignInAdviser interface {
	SignInAdvice(refusal string) string
}

// SignIn is a sign-in a connection held: the app's client and the
// tokens the service issued to it. Any of them may be empty.
type SignIn struct {
	ClientID, ClientSecret    string
	AccessToken, RefreshToken string
}

// ProxySecretKey is the key, in a connection's Secret, of its scoped
// proxy credential's secret. It is not a user_config key (those have
// no hyphen), so it is never a connection field, never in the fields
// a built-in is given and never listed to the owner.
const ProxySecretKey = "kivali-proxy-secret"

// ProxyUser is the egress proxy user of name's connection id: the
// "integration-" a bundle's server has, then the name, a dot and the
// id. An integration name has no dot, so no bundle's proxy user
// (integration-<name>) is ever one of these.
func ProxyUser(name, connID string) string {
	return "integration-" + name + "." + connID
}

// Refusal is an error of Call or Complete that refuses the call before
// anything ran: core audits it as not allowed, with Reason, and answers
// the agent with Text and no more. Text names only the cause and who
// can change it.
type Refusal struct {
	Text   string
	Reason string
}

func (r *Refusal) Error() string { return r.Text }

// Registry is a set of built-in integrations.
type Registry []Integration

// Lookup is the built-in integration called name, nil for none.
func (r Registry) Lookup(name string) Integration {
	for _, bi := range r {
		if m := bi.Manifest(); m != nil && m.Name == name {
			return bi
		}
	}
	return nil
}

// Manifests is every built-in's manifest, in the registry's order.
func (r Registry) Manifests() []*integration.Manifest {
	out := make([]*integration.Manifest, 0, len(r))
	for _, bi := range r {
		out = append(out, bi.Manifest())
	}
	return out
}
