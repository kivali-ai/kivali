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
package builtinintegrations

import (
	"context"
	"encoding/json"

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
// included.
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
// than its form does. PUT .../connection calls CheckConnection after
// the form's own checks and before it stores anything; an error is a
// 400 with its words, a sentence for the owner. conn.ID is empty for a
// first connection.
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
