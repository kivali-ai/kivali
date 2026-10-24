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
// a test sees the secret reach the Go function.
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
// changes it to see boot supersede the version before. Multiple makes
// its manifest take several connections (kivali.connection.multiple).
// SignIn, when set, is its manifest's connection section in place of a
// token connection's: a connection made by signing in, for tests of
// core's credential path (Multiple still applies).
//
// SetupFlow makes its connection a setup one (setup.go). HandOver, with
// it, is the sign-in setup hands over to (setup.then): its method and
// endpoints. Neither changes once the built-in is registered.
type Echo struct {
	Version   string
	Multiple  bool
	SignIn    *integration.Connection
	SetupFlow bool
	HandOver  *integration.Connection

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
// verify, choices, setup, requirements or uninstall), the caller and
// the connection. For setup, Flow and Answer are what the call was
// given.
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
	conn := integration.Connection{Method: "token", Multiple: e.Multiple}
	if e.SignIn != nil {
		conn = *e.SignIn
		conn.Multiple = e.Multiple
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
	if e.SetupFlow {
		e.setupManifest(m)
	}
	return m
}

// Tools lists echo and whoami for every caller, and an action the
// manifest does not declare, which core must leave out.
func (e *Echo) Tools(_ context.Context, _ builtinintegrations.Caller) ([]builtinintegrations.Tool, error) {
	return []builtinintegrations.Tool{
		{Name: "echo", Description: "Answer with the arguments."},
		{Name: "whoami", Description: "Say who is connected.", InputSchema: json.RawMessage(`{"type":"object","properties":{}}`)},
		{Name: "undeclared", Description: "Not in the manifest."},
	}, nil
}

// Call runs echo or whoami.
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
		return &builtinintegrations.Result{Content: []string{text}}, nil
	case "whoami":
		return &builtinintegrations.Result{Content: []string{whoami(conn)}}, nil
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

// Choices lists Colors for colors.
func (e *Echo) Choices(_ context.Context, conn builtinintegrations.Connection, name string) ([]string, error) {
	e.see("choices", builtinintegrations.Caller{}, conn)
	if name != "colors" {
		return nil, errors.New("echo lists no options for " + name)
	}
	return append([]string(nil), Colors...), nil
}
