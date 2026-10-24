package echotest

import (
	"context"
	"errors"
	"fmt"
	"net/url"

	"github.com/kivali-ai/kivali/internal/builtinintegrations"
	"github.com/kivali-ai/kivali/internal/integration"
)

// SetupHost is the service echo's setup installs its app on: the one
// host its setup declares.
const SetupHost = "setup.example"

// OtherHost is a host echo's setup does not declare, where StrayHost
// sends the browser.
const OtherHost = "elsewhere.example"

// FlowPermissions is the flow the permissions requirement's Fix runs.
const FlowPermissions = "permissions"

// The pages of echo's flow on SetupHost.
const (
	// InstallURL is where the Go step sends the browser: posting the
	// key, or, with the key left blank, by a link, the page asking for
	// it there. The service sends the browser back to the callback the
	// step gave it (redirect_url) with ?code=.
	InstallURL = "https://" + SetupHost + "/install"
	// AppURL is the page the Wait step names, where the owner approves
	// the app.
	AppURL = "https://" + SetupHost + "/apps/echo"
)

// Stray is a step of echo's own flow that leaves its manifest's
// declarations, for a test of core holding it to them.
type Stray string

const (
	// StrayHost: the Go step sends the browser to OtherHost.
	StrayHost Stray = "host"
	// StrayField: the Form step asks for password, a field the manifest
	// does not declare.
	StrayField Stray = "field"
	// StrayKeep: the Go step keeps password, a key the manifest does
	// not declare.
	StrayKeep Stray = "keep"
)

// SetupScript decides echo's setup steps in place of its own flow: n
// counts the calls of Setup from 1, and the rest is what the call was
// given. It is for tests of the runner's own mechanics (deadlines,
// panics, several legs, a hand-over), which echo's flow does not take.
type SetupScript func(n int, ctx context.Context, conn builtinintegrations.Connection, last builtinintegrations.Answer, p builtinintegrations.Progress) (builtinintegrations.Step, error)

// RequirementsScript answers echo's Requirements in place of its own
// rows, for tests of what core makes of rows no integration should
// give.
type RequirementsScript func(ctx context.Context, conn builtinintegrations.Connection) ([]builtinintegrations.Requirement, error)

// setupKnobs are what the test sets; guarded by Echo.mu.
type setupKnobs struct {
	installed, permissions   bool
	failSetup, failUninstall bool
	stray                    Stray
	setupScript              SetupScript
	requirementsScript       RequirementsScript
	setupCalls               int
}

// SetInstalled says whether the app is installed on the service: the
// Wait step's check finds it and the flow is done.
func (e *Echo) SetInstalled(v bool) { e.knob(func(k *setupKnobs) { k.installed = v }) }

// SetPermissions says whether the app has the permissions it needs:
// the permissions requirement is met. Its Fix's flow sets it.
func (e *Echo) SetPermissions(v bool) { e.knob(func(k *setupKnobs) { k.permissions = v }) }

// FailSetup makes the Wait step's check fail the flow, in words that
// quote the kept account, the kept token and the setup-only key, so a
// test sees the account as it is and the other two scrubbed.
func (e *Echo) FailSetup(v bool) { e.knob(func(k *setupKnobs) { k.failSetup = v }) }

// FailUninstall makes Uninstall fail, in words that quote the token.
func (e *Echo) FailUninstall(v bool) { e.knob(func(k *setupKnobs) { k.failUninstall = v }) }

// SetStray makes echo's flow take the stray step s; "" for none.
func (e *Echo) SetStray(s Stray) { e.knob(func(k *setupKnobs) { k.stray = s }) }

// ScriptSetup has script decide setup's steps; nil gives echo's own
// flow back. The count of calls goes on.
func (e *Echo) ScriptSetup(script SetupScript) {
	e.knob(func(k *setupKnobs) { k.setupScript = script })
}

// ScriptRequirements has script answer Requirements; nil gives echo's
// own rows back.
func (e *Echo) ScriptRequirements(script RequirementsScript) {
	e.knob(func(k *setupKnobs) { k.requirementsScript = script })
}

func (e *Echo) knob(set func(k *setupKnobs)) {
	e.mu.Lock()
	defer e.mu.Unlock()
	set(&e.knobs)
}

func (e *Echo) knobsNow() setupKnobs {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.knobs
}

// setupManifest makes m's connection a setup one: the account and a
// setup-only key asked for, the browser sent to SetupHost alone, and
// the token and the account kept, the account declared not secret. With HandOver, the connection has
// HandOver's endpoints, setup.then names its method, and the keeps add
// the app's client id and secret, which echo's last step keeps
// (ClientID, ClientSecret) for that sign-in.
func (e *Echo) setupManifest(m *integration.Manifest) {
	setup := integration.Setup{
		Summary: "Installs the echo app on your " + SetupHost + " account.",
		Hosts:   []string{SetupHost},
		Fields: integration.SetupFields{
			{Key: "account", Title: "Account", Words: "Your account on " + SetupHost + "."},
			{Key: "key", Title: "Key", Words: "From the account's key page. Setup sends it to " + SetupHost + " and keeps it nowhere.", Secret: true, SetupOnly: true},
		},
		Keeps: integration.SetupKeeps{
			{Key: "token", Words: "The echo app's token"},
			{Key: "account", Words: "The account the app is installed on", NotSecret: true},
		},
	}
	conn := integration.Connection{Method: integration.MethodSetup}
	if e.HandOver != nil {
		conn = *e.HandOver
		conn.Method = integration.MethodSetup
		setup.Then = e.HandOver.Method
		setup.Keeps = append(setup.Keeps,
			integration.SetupKeep{Key: integration.FieldClientID, Words: "The app's client id"},
			integration.SetupKeep{Key: integration.FieldClientSecret, Words: "The app's client secret"})
	}
	conn.Setup = &setup
	m.UserConfig = nil
	m.Kivali.Connection = conn
	m.Kivali.Hosts = []string{SetupHost}
}

// Setup is echo's setup flow. Installing (FlowSetup) is a Form asking
// for the account and the key (asked again while the account is blank);
// a Go to InstallURL posting the key (a link with none), which keeps the
// account and expects the browser back with ?code=; a Wait on AppURL,
// after keeping the token the code stands for, checked until the app is
// installed (SetInstalled); then Done, keeping ClientID and
// ClientSecret with HandOver. State carries where the flow is and the
// setup-only key; what was kept is read back from conn.Fields.
//
// Echo has no service to trade the code with, so it takes the code as
// it came back. A real Setup trades the code with the service before
// keeping anything, and treats Return as untrusted: anyone can make the
// browser load the callback, and a replayed earlier leg arrives as this
// step's Return.
//
// The permissions Fix (FlowPermissions) is one step: Done, with the
// permissions granted.
func (e *Echo) Setup(ctx context.Context, conn builtinintegrations.Connection, flow string, last builtinintegrations.Answer, p builtinintegrations.Progress) (builtinintegrations.Step, error) {
	e.mu.Lock()
	e.knobs.setupCalls++
	n := e.knobs.setupCalls
	k := e.knobs
	e.mu.Unlock()
	e.record(Seen{Action: "setup", Conn: conn, Flow: flow, Answer: last})
	if k.setupScript != nil {
		return k.setupScript(n, ctx, conn, last, p)
	}
	switch flow {
	case builtinintegrations.FlowSetup:
		return e.install(k, conn, last)
	case FlowPermissions:
		e.SetPermissions(true)
		return builtinintegrations.Step{Done: true}, nil
	}
	return builtinintegrations.Step{}, fmt.Errorf("echo has no flow %s", flow)
}

// install is the install flow's next step after last, on conn as the
// steps before kept it.
func (e *Echo) install(k setupKnobs, conn builtinintegrations.Connection, last builtinintegrations.Answer) (builtinintegrations.Step, error) {
	state := last.State
	switch state["at"] {
	case "":
		fields := []string{"account", "key"}
		if k.stray == StrayField {
			fields = append(fields, "password")
		}
		return builtinintegrations.Step{
			Form:  &builtinintegrations.Form{Words: "Name your " + SetupHost + " account and paste its key.", Fields: fields},
			State: map[string]string{"at": "form"},
		}, nil

	case "form":
		account, key := last.Form["account"], last.Form["key"]
		if account == "" {
			return builtinintegrations.Step{
				Form:  &builtinintegrations.Form{Words: "Name your " + SetupHost + " account; it cannot be left blank.", Fields: []string{"account", "key"}},
				State: state,
			}, nil
		}
		host := SetupHost
		if k.stray == StrayHost {
			host = OtherHost
		}
		step := builtinintegrations.Step{
			Go:    &builtinintegrations.Go{Words: "Install the echo app on " + SetupHost + "."},
			Keep:  map[string]string{"account": account},
			State: map[string]string{"at": "go", "key": key},
		}
		if key != "" {
			step.Go.URL = "https://" + host + "/install"
			step.Go.Post = url.Values{"key": {key}, "redirect_url": {last.Callback}}
		} else {
			step.Go.URL = "https://" + host + "/install?" + url.Values{"redirect_url": {last.Callback}}.Encode()
		}
		if k.stray == StrayKeep {
			step.Keep["password"] = "hunter22"
		}
		return step, nil

	case "go":
		// Echo has no service to trade the code with, so it takes it as
		// it came back. A real Setup trades it with the service before it
		// keeps anything, since Return is untrusted (a replayed earlier
		// leg arrives here too).
		code := last.Return.Get("code")
		if code == "" {
			return builtinintegrations.Step{}, errors.New(SetupHost + " sent the browser back with no code")
		}
		return builtinintegrations.Step{
			Wait:  &builtinintegrations.Wait{Words: "Approve the echo app on " + SetupHost + ".", URL: AppURL},
			Keep:  map[string]string{"token": "tok-" + code},
			State: map[string]string{"at": "wait", "key": state["key"]},
		}, nil

	case "wait":
		if !last.Checked {
			return builtinintegrations.Step{}, errors.New("echo's Wait was answered without a check")
		}
		switch {
		case k.failSetup:
			return builtinintegrations.Step{}, fmt.Errorf("%s dropped the app of %s with token %s and key %s", SetupHost, conn.Fields["account"], conn.Fields["token"], state["key"])
		case k.installed:
			done := builtinintegrations.Step{Done: true}
			if e.HandOver != nil {
				done.Keep = map[string]string{integration.FieldClientID: e.ClientID, integration.FieldClientSecret: e.ClientSecret}
			}
			return done, nil
		}
		return builtinintegrations.Step{
			Wait:  &builtinintegrations.Wait{Words: "Approve the echo app on " + SetupHost + ".", URL: AppURL},
			State: state,
		}, nil
	}
	return builtinintegrations.Step{}, fmt.Errorf("echo's setup is at %q, which it does not know", state["at"])
}

// Requirements is echo's own health with SetupFlow set: installed on
// SetupHost, always met; and its permissions, met once granted
// (SetPermissions, or the Fix's flow), with the Fix FlowPermissions
// while not. Without SetupFlow it has none, and records no call.
func (e *Echo) Requirements(ctx context.Context, conn builtinintegrations.Connection) ([]builtinintegrations.Requirement, error) {
	if !e.SetupFlow {
		return nil, nil
	}
	k := e.knobsNow()
	e.record(Seen{Action: "requirements", Conn: conn})
	if k.requirementsScript != nil {
		return k.requirementsScript(ctx, conn)
	}
	perm := builtinintegrations.Requirement{ID: "permissions", Title: "Permissions", Words: "The echo app lacks a permission it needs.", Fix: FlowPermissions}
	if k.permissions {
		perm.Words, perm.Met, perm.Fix = "The echo app has every permission it needs.", true, ""
	}
	return []builtinintegrations.Requirement{
		{ID: "installed", Title: "Installed on " + SetupHost, Words: "The echo app is installed.", Met: true},
		perm,
	}, nil
}

// Uninstall, with SetupFlow set, records the call, and fails, quoting
// the token, when FailUninstall says so. Without SetupFlow there is
// nothing on a service to undo, and it records no call.
func (e *Echo) Uninstall(_ context.Context, conn builtinintegrations.Connection) error {
	if !e.SetupFlow {
		return nil
	}
	k := e.knobsNow()
	e.record(Seen{Action: "uninstall", Conn: conn})
	if k.failUninstall {
		return fmt.Errorf("%s kept the app of token %s", SetupHost, conn.Fields["token"])
	}
	return nil
}

var (
	_ builtinintegrations.SetupIntegration = (*Echo)(nil)
	_ builtinintegrations.RequirementsOf   = (*Echo)(nil)
	_ builtinintegrations.Uninstaller      = (*Echo)(nil)
)
