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
//   - Its manifest has no server section and declares no hosts
//     (integration.ValidateBuiltin): a built-in runs inside Kivali, and
//     its network reach is Kivali's own, the hosts its connection names.
//   - It needs no install approval: the owner approved the binary. Boot
//     writes its record and its version (store.InstallBuiltinIntegrations).
//   - While the binary carries it, it cannot be removed or replaced by an
//     upload; it can be switched off.
//
// To add one, implement Integration in a package of its own and list it
// in builtins.All (internal/builtinintegrations/builtins).
//
// A shipped bundle (ShippedBundle) is the other kind of integration
// that ships with Kivali: built in as above (no approval, no removal,
// its version follows the binary), but its implementation is a bundle
// embedded in the binary, which boot stores
// (store.InstallShippedBundles) and core runs in a pod of its own like
// any uploaded bundle. To add one, embed its directory in a package of
// its own and list it in builtins.Shipped.
package builtinintegrations

import (
	"context"
	"encoding/json"
	"io/fs"
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
// each of its connections a scoped proxy credential of its own,
// admitted to those hosts and no others (ProxyUser), and puts it in the
// job.
type HostsOf interface {
	Hosts(conn Connection) []string
}

// ConnectionChecker is an Integration that holds a connection to more
// than its form does. Saving a connection (PUT .../connection, POST
// .../connections, PUT .../connections/{id}) calls CheckConnection
// after the form's own checks and before it stores anything; an error
// is a 400 with its words, a sentence for the owner. conn.ID is empty
// for a new connection.
type ConnectionChecker interface {
	CheckConnection(conn Connection) error
}

// ConnectionLabeler is an Integration that takes several connections
// (the manifest's kivali.connection.multiple) and names one the owner
// gave no label: ConnectionLabel is that label, or "" for none. conn
// holds the connection's fields that are not secret, so the label never
// quotes a secret.
type ConnectionLabeler interface {
	ConnectionLabel(conn Connection) string
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
// is shown to the owner, scrubbed of every kept value, and the flow
// keeps nothing more; a flow that fails or is cancelled before its
// first Keep leaves the connection as it was.
type SetupIntegration interface {
	Integration
	Setup(ctx context.Context, conn Connection, flow string, last Answer, p Progress) (Step, error)
}

// FlowSetup is the flow that installs the integration: the one the
// owner starts from Available.
const FlowSetup = "setup"

// Step is what the owner does next. Exactly one of Form, Go, Wait or
// Done is set.
type Step struct {
	// Form asks the owner for fields.
	Form *Form
	// Go sends the owner's browser to the service and back.
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
// service that takes a POST), and expects it back at the callback the
// Answer gave (Answer.Callback), where the service's query reaches
// Setup as the next Answer's Return.
type Go struct {
	// Words say what the owner will do there.
	Words string
	// URL is where the browser goes; its host is one of setup.hosts.
	URL string
	// Post, when set, is posted to URL by a form in the owner's browser
	// instead of a link.
	Post url.Values
}

// Wait is a step that waits for something to happen on the service.
type Wait struct {
	// Words say what to do, or what is being waited for.
	Words string
	// URL is a page for the owner to open meanwhile, on one of
	// setup.hosts; empty for none.
	URL string
}

// Answer is what the last step gave back: Setup reads it and returns
// the next Step. The first call of a flow gets an Answer with State
// nil, no Form, no Return and Checked false.
type Answer struct {
	// State is the State the last Step carried, as it was.
	State map[string]string
	// Form is what the owner entered, by field key, after a Form step.
	Form map[string]string
	// Return is the query the service sent the browser back with,
	// after a Go step. It is untrusted: the service sent it, but anyone
	// can make the browser load the callback with any query. Setup
	// checks it against the service before relying on it.
	Return url.Values
	// Checked is true for a Wait step's check: Setup looks at the
	// service and returns the same Wait to keep waiting, or the next
	// Step.
	Checked bool
	// Callback is the address a Go step's service may send the browser
	// back to for this step, with the flow's state in it: what Setup
	// puts in a manifest's redirect_url or a form's return field. A
	// service that returns elsewhere than it was told comes back to the
	// flow all the same, by the browser that started it.
	Callback string
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
// with the same steps as installing. Requirements runs under a request's
// deadline, concurrently with everything else, and must honour ctx.
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
// Secret and drops every agent's access. An error is shown to the
// owner and leaves the installation as it is.
type Uninstaller interface {
	Uninstall(ctx context.Context, conn Connection) error
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

// ShippedBundle is an integration that ships with Kivali as a bundle:
// its files, laid out as a bundle's (manifest.json at the root, the
// server's files beside it), embedded in the binary.
type ShippedBundle struct {
	// Name is the integration's name, the one its manifest gives.
	Name string
	// FS is the bundle's files, rooted where manifest.json is: an
	// embed.FS's directory (fs.Sub), embedded with the all: prefix so
	// a file whose name starts with "_" or "." is kept.
	FS fs.FS
}

// ShippedBundles is a set of shipped bundles.
type ShippedBundles []ShippedBundle

// Has reports whether the set ships an integration called name.
func (b ShippedBundles) Has(name string) bool {
	for _, sb := range b {
		if sb.Name == name {
			return true
		}
	}
	return false
}

// ByName is every shipped bundle's files by its name, as
// store.InstallShippedBundles takes them.
func (b ShippedBundles) ByName() map[string]fs.FS {
	out := make(map[string]fs.FS, len(b))
	for _, sb := range b {
		out[sb.Name] = sb.FS
	}
	return out
}
