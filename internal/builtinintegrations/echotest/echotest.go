// Package echotest is a built-in integration for tests: echo, which
// answers with what core gave it. Only tests import it; it is never in
// builtins.All.
//
// Its manifest has a sensitive token and a greeting that is not
// secret; an access form of a list field, colors, whose options it
// lists live, and a switch, loud; Test connection; and two actions:
// echo, which answers with its arguments, the caller's agent and access
// as JSON text (upper case when the access is loud), and whoami, which
// answers with the greeting and the token's first three characters, so
// a test sees the secret reach the Go function. It is a SignInAdviser,
// whose words for a refused sign-in are Advise's.
//
// With Holds set it has a third action, hold, whose call waits for the
// owner's approval (Result.Hold): the title, fields and recipients are
// its arguments' (HoldArgs), and its Run, approved, answers with how
// many fields it echoed and whoami of the connection it is run with,
// so a test sees the connection read at approval rather than at the
// call. HoldRun, when set, runs in its place.
//
// With SetupFlow set its connection is a setup one instead (method
// setup, setup.go), and it implements every optional hook of the setup
// contract: SetupIntegration, RequirementsOf and Uninstaller. Its flow
// installs the echo app on SetupHost, its requirements are one row met
// and one with a Fix, and its Uninstall records the call. Knobs steer
// each hook to the path a test needs, and every call is recorded as
// Calls gives it.
package echotest

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"

	"github.com/kivali-ai/kivali/internal/builtinintegrations"
	"github.com/kivali-ai/kivali/internal/integration"
)

// Name is the built-in's name; its actions are echo_echo and
// echo_whoami.
const Name = "echo"

// Colors is what Choices lists for the colors field.
var Colors = []string{"blue", "green", "red"}

// FailGreeting is the greeting that makes Verify fail, with the token
// in its words, so a test sees them scrubbed.
const FailGreeting = "fail"

// Echo is the built-in. Version is its manifest's version; a test
// changes it to see boot supersede the version before. SignIn, when
// set, is its manifest's connection section in place of a token
// connection's: a connection made by signing in, for tests of core's
// credential path.
//
// SetupFlow makes its connection a setup one (setup.go). HandOver, with
// it, is the sign-in setup hands over to (setup.then): its method and
// endpoints. Neither changes once the built-in is registered.
type Echo struct {
	Version   string
	SignIn    *integration.Connection
	SetupFlow bool
	HandOver  *integration.Connection
	// ClientID and ClientSecret are what echo's flow keeps as the app's
	// client id and secret at its end, with HandOver, for the sign-in
	// that takes it on.
	ClientID, ClientSecret string
	// Advise, when set, is what SignInAdvice answers a refused
	// sign-in with, for tests of core adding a built-in's words to its
	// Sign-in requirement; nil adds nothing.
	Advise func(refusal string) string
	// Outside is what echo's Result names as the senders, outside the
	// owner's domain, of what it returned (Result.Outside), for tests
	// of core keeping it in the audit.
	Outside []string
	// Holds adds the action hold. HoldRun, when set, is the Run of
	// hold's Hold in place of echo's own, for tests of a Run that
	// refuses or fails; it is recorded as echo's is. Neither changes
	// once the built-in is registered.
	Holds   bool
	HoldRun func(ctx context.Context, conn builtinintegrations.Connection) (*builtinintegrations.Result, error)

	// Watch, when set, is sent every call of Setup as it is recorded,
	// for a test that waits on a call its flow's state does not show (a
	// Wait's check that finds the same Wait). It is set before the
	// built-in is registered, with room for every call the test makes:
	// a call that finds it full is not sent.
	Watch chan<- Seen

	mu    sync.Mutex
	calls []Seen
	knobs setupKnobs
}

// Seen is one call Echo was given: the action (an action's name, or
// verify, choices, setup, requirements, uninstall, or hold run for a
// held call's Run), the caller and the connection. For setup, Flow and
// Answer are what the call was given.
type Seen struct {
	Action string
	Caller builtinintegrations.Caller
	Conn   builtinintegrations.Connection
	Flow   string
	Answer builtinintegrations.Answer
}

// New is Echo at version 1.0.0.
func New() *Echo { return &Echo{Version: "1.0.0"} }

// Calls is every call Echo was given, oldest first.
func (e *Echo) Calls() []Seen {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]Seen(nil), e.calls...)
}

func (e *Echo) see(action string, caller builtinintegrations.Caller, conn builtinintegrations.Connection) {
	e.record(Seen{Action: action, Caller: caller, Conn: conn})
}

func (e *Echo) record(seen Seen) {
	e.mu.Lock()
	e.calls = append(e.calls, seen)
	e.mu.Unlock()
	if e.Watch != nil && seen.Action == "setup" {
		// Never blocks: a call made as a test ends, with nobody reading,
		// must still return, or the runner's drain would wait on it.
		select {
		case e.Watch <- seen:
		default:
		}
	}
}

// Manifest is echo's manifest.
func (e *Echo) Manifest() *integration.Manifest {
	conn := integration.Connection{Method: "token"}
	if e.SignIn != nil {
		conn = *e.SignIn
	}
	m := &integration.Manifest{
		ManifestVersion: "0.3",
		Name:            Name,
		Version:         e.Version,
		DisplayName:     "Echo",
		Description:     "Answers with what Kivali gave it.",
		Author:          integration.Author{Name: "Kivali"},
		UserConfig: integration.UserConfig{
			{Key: "token", Type: "string", Title: "Token", Description: "Any value; whoami shows its start.", Sensitive: true, Required: true},
			{Key: "greeting", Type: "string", Title: "Greeting", Description: "What whoami says."},
		},
		Tools: []integration.Tool{
			{Name: "echo", Description: "Answer with the arguments, the agent and its access."},
			{Name: "whoami", Description: "Say the greeting and the token's start."},
		},
		Kivali: integration.Kivali{
			Connection: conn,
			Access: &integration.Access{
				Schema: json.RawMessage(`{"type":"object","properties":{` +
					`"colors":{"type":"array","items":{"type":"string"},"title":"Colors"},` +
					`"loud":{"type":"boolean","title":"Loud"}}}`),
				Choices: map[string]string{"colors": "colors"},
			},
			Verify: "whoami",
		},
	}
	if e.Holds {
		m.Tools = append(m.Tools, integration.Tool{Name: "hold", Description: "Wait for the owner's approval, then echo the fields."})
	}
	if e.SetupFlow {
		e.setupManifest(m)
	}
	return m
}

// Tools lists echo and whoami for every caller, hold with Holds, and an
// action the manifest does not declare, which core must leave out.
func (e *Echo) Tools(_ context.Context, _ builtinintegrations.Caller) ([]builtinintegrations.Tool, error) {
	tools := []builtinintegrations.Tool{
		{Name: "echo", Description: "Answer with the arguments."},
		{Name: "whoami", Description: "Say who is connected.", InputSchema: json.RawMessage(`{"type":"object","properties":{}}`)},
		{Name: "undeclared", Description: "Not in the manifest."},
	}
	if e.Holds {
		tools = append(tools, builtinintegrations.Tool{Name: "hold", Description: "Wait for the owner, then echo the fields."})
	}
	return tools, nil
}

// HoldArgs are hold's arguments: the held call's title, its fields
// and whom it reaches.
type HoldArgs struct {
	Title      string                          `json:"title"`
	Fields     []builtinintegrations.HoldField `json:"fields"`
	Recipients []string                        `json:"recipients"`
}

// hold is the held call of hold with args. Its Run (HoldRun, or echo's
// own) is recorded as hold run, with the connection it is given.
func (e *Echo) hold(caller builtinintegrations.Caller, args json.RawMessage) (*builtinintegrations.Result, error) {
	var a HoldArgs
	if len(args) > 0 {
		if err := json.Unmarshal(args, &a); err != nil {
			return &builtinintegrations.Result{Content: []string{"The arguments are not hold's: " + err.Error()}, IsError: true}, nil
		}
	}
	run := e.HoldRun
	if run == nil {
		run = func(_ context.Context, conn builtinintegrations.Connection) (*builtinintegrations.Result, error) {
			res := &builtinintegrations.Result{Content: []string{fmt.Sprintf("Echoed %d fields; %s", len(a.Fields), whoami(conn))}}
			if len(a.Recipients) > 0 {
				res.Audit = "echoed to " + strings.Join(a.Recipients, ", ")
			}
			return res, nil
		}
	}
	return &builtinintegrations.Result{Hold: &builtinintegrations.Hold{
		Title:      cmp.Or(a.Title, "Echo the fields"),
		Fields:     a.Fields,
		Recipients: a.Recipients,
		Run: func(ctx context.Context, conn builtinintegrations.Connection) (*builtinintegrations.Result, error) {
			e.see("hold run", caller, conn)
			return run(ctx, conn)
		},
	}}, nil
}

// Call runs echo, whoami, or hold with Holds.
func (e *Echo) Call(_ context.Context, caller builtinintegrations.Caller, conn builtinintegrations.Connection, name string, args json.RawMessage) (*builtinintegrations.Result, error) {
	e.see(name, caller, conn)
	switch name {
	case "echo":
		b, err := json.Marshal(map[string]any{"arguments": args, "agent": caller.Agent, "access": caller.Access})
		if err != nil {
			return nil, err
		}
		text := string(b)
		if loud, _ := caller.Access["loud"].(bool); loud {
			text = strings.ToUpper(text)
		}
		return &builtinintegrations.Result{Content: []string{text}, Outside: e.Outside}, nil
	case "whoami":
		return &builtinintegrations.Result{Content: []string{whoami(conn)}}, nil
	case "hold":
		if e.Holds {
			return e.hold(caller, args)
		}
	}
	return nil, fmt.Errorf("echo has no action %s", name)
}

// whoami is the greeting (for a setup connection, which has none, the
// account) and the token's first three characters.
func whoami(conn builtinintegrations.Connection) string {
	tok := conn.Fields["token"]
	return fmt.Sprintf("%s; token %s...", cmp.Or(conn.Fields["greeting"], conn.Fields["account"]), tok[:min(3, len(tok))])
}

// Verify answers as whoami does, and fails, quoting the token, when the
// greeting is FailGreeting.
func (e *Echo) Verify(_ context.Context, conn builtinintegrations.Connection) (string, error) {
	e.see("verify", builtinintegrations.Caller{}, conn)
	if conn.Fields["greeting"] == FailGreeting {
		return "", fmt.Errorf("the token %s was refused", conn.Fields["token"])
	}
	return whoami(conn), nil
}

// SignInAdvice is Advise's words for refusal, or nothing without it.
func (e *Echo) SignInAdvice(refusal string) string {
	if e.Advise == nil {
		return ""
	}
	return e.Advise(refusal)
}

var _ builtinintegrations.SignInAdviser = (*Echo)(nil)

// Choices lists Colors for colors.
func (e *Echo) Choices(_ context.Context, conn builtinintegrations.Connection, name string) ([]string, error) {
	e.see("choices", builtinintegrations.Caller{}, conn)
	if name != "colors" {
		return nil, errors.New("echo lists no options for " + name)
	}
	return append([]string(nil), Colors...), nil
}
