// Package web (integration_setup_bundle.go) — a bundle's setup,
// requirements and uninstall, over its pod.
//
// A bundle whose manifest names method setup implements, in its MCP
// server, the hooks a built-in implements in Go, as requests core sends
// through the shim (integrationpod.MethodSetup, MethodRequirements,
// MethodUninstall). bundleHooks is the built-in's interfaces over those
// requests, so the setup runner, the requirements and Uninstall run a
// bundle by the same code as a built-in: the same declarations, the
// same scrubbing, deadlines and audit. The server reads the
// connection's values as it does for any call (its env, or
// kivali/credential), so the Connection a hook is given is not sent;
// a Keep restarts the server with what was kept.

package web

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/kivali-ai/kivali/internal/builtinintegrations"
	"github.com/kivali-ai/kivali/internal/integration"
	"github.com/kivali-ai/kivali/internal/integrationpod"
)

// bundleStartWait is how long a bundle's setup call waits for its
// server to be running (a bundle being switched on to install it)
// before the flow fails.
const bundleStartWait = 2 * time.Minute

// bundleHooks is a bundle's setup, requirements and uninstall, sent to
// its server.
type bundleHooks struct {
	s             *Server
	name, display string
}

// bundleHooksOf is v's hooks over its pod: a bundle whose manifest
// names method setup. ok is false for any other.
func (s *Server) bundleHooksOf(v *integrationView) (bundleHooks, bool) {
	if v.builtin != nil || v.staleBuiltin() || v.manifest == nil || v.manifest.Kivali.Connection.Method != integration.MethodSetup {
		return bundleHooks{}, false
	}
	return bundleHooks{s: s, name: v.it.Name, display: v.displayName()}, true
}

// setupDriverOf is what runs v's own setup flow: a built-in's
// SetupIntegration, or a setup bundle's server; ok false for neither.
func (s *Server) setupDriverOf(v *integrationView) (setupDriver, bool) {
	if si, ok := v.builtin.(builtinintegrations.SetupIntegration); ok {
		return si, true
	}
	if b, ok := s.bundleHooksOf(v); ok {
		return b, true
	}
	return nil, false
}

// requirementsOf is what gives v's own requirements: a built-in's
// RequirementsOf, or a setup bundle's server while it runs (one not
// running is not asked: the running row says so).
func (s *Server) requirementsOf(v *integrationView) (builtinintegrations.RequirementsOf, bool) {
	if ro, ok := v.builtin.(builtinintegrations.RequirementsOf); ok {
		return ro, true
	}
	if b, ok := s.bundleHooksOf(v); ok && v.running {
		return b, true
	}
	return nil, false
}

// uninstallerOf is what undoes v's installation on the service: a
// built-in's Uninstaller, or a setup bundle's server while it runs
// (uninstallRefusal says what Uninstall does when it does not run).
func (s *Server) uninstallerOf(v *integrationView) (builtinintegrations.Uninstaller, bool) {
	if u, ok := v.builtin.(builtinintegrations.Uninstaller); ok {
		return u, true
	}
	if b, ok := s.bundleHooksOf(v); ok && v.running {
		return b, true
	}
	return nil, false
}

// uninstallRefusal is why Uninstall of a setup bundle with a
// connection is refused: switched on with its server not running, it
// cannot ask the server to undo the installation on the service, and
// the server may be about to run. "" otherwise: one switched off is
// uninstalled without asking, its installation on the service left as
// it is.
func (s *Server) uninstallRefusal(v *integrationView) string {
	if _, ok := s.bundleHooksOf(v); !ok || v.conn == nil || v.running || !v.it.Enabled {
		return ""
	}
	return v.displayName() + " is not running, so it cannot undo its installation on the service; switch it off to uninstall it without that"
}

// errBundleNotRunning refuses a Fix of a setup bundle whose server is
// not running: its own flows run in it.
func errBundleNotRunning(display string) string {
	return display + " is not running, so its own flows cannot run; its log says why"
}

// isSetupBundle reports whether v is a bundle whose own setup flow
// makes its connection.
func (s *Server) isSetupBundle(v *integrationView) bool {
	_, ok := s.bundleHooksOf(v)
	return ok
}

// hookError is err of the bundle's server's answer to method in words
// for the owner: the server's own error message, or why it gave none.
// The deadline and a caller that went are passed on as they are, for
// the runner's and the route's own words.
func (b bundleHooks) hookError(method string, err error) error {
	var rpcErr *integrationpod.RPCError
	switch {
	case errors.As(err, &rpcErr) && rpcErr.Code == integrationpod.CodeMethodNotFound:
		return fmt.Errorf("%s's server does not answer %s; its author must add it", b.display, method)
	case errors.As(err, &rpcErr) && rpcErr.Message != "":
		return errors.New(rpcErr.Message)
	case errors.As(err, &rpcErr):
		return fmt.Errorf("%s failed and gave no reason", b.display)
	case errors.Is(err, errIntegrationNotRunning):
		return fmt.Errorf("%s stopped before it answered", b.display)
	}
	return err
}

// Setup is one call of the flow: kivali/setup with the flow and the
// Answer, answered with the next step. Its progress notifications are
// the flow's stages. A server not running yet (one switched on to
// install it) is waited for, bundleStartWait at most.
func (b bundleHooks) Setup(ctx context.Context, _ builtinintegrations.Connection, flow string, last builtinintegrations.Answer, p builtinintegrations.Progress) (builtinintegrations.Step, error) {
	reg := b.s.Integrations
	if !reg.Running(b.name) {
		waitCtx, cancel := context.WithCancel(ctx)
		t := b.s.clk().NewTimer(bundleStartWait)
		go func() {
			defer cancel()
			select {
			case <-t.C():
			case <-waitCtx.Done():
			}
		}()
		running := reg.awaitRunning(waitCtx, b.name)
		t.Stop()
		cancel()
		if !running {
			if ctx.Err() != nil {
				return builtinintegrations.Step{}, ctx.Err()
			}
			return builtinintegrations.Step{}, fmt.Errorf("%s is not running, so its setup cannot go on; its log says why. Start the setup again once it runs", b.display)
		}
	}
	token, stop := reg.watchProgress(b.name, func(pp integrationpod.ProgressParams) {
		total := 0
		if pp.Total != nil {
			total = int(*pp.Total)
		}
		p.Stage(pp.Message, int(pp.Progress), total)
	})
	defer stop()
	params := integrationpod.SetupParams{Flow: flow, Answer: integrationpod.SetupAnswerOf(last), Meta: &integrationpod.RequestMeta{ProgressToken: token}}
	raw, err := reg.Call(ctx, b.name, integrationpod.MethodSetup, params)
	if err != nil {
		return builtinintegrations.Step{}, b.hookError(integrationpod.MethodSetup, err)
	}
	step, err := integrationpod.ParseSetupResult(orEmptyObject(raw))
	if err != nil {
		return builtinintegrations.Step{}, fmt.Errorf("%s's setup answered with a step Kivali cannot read: %v", b.display, err)
	}
	return step, nil
}

// Requirements is kivali/requirements: the bundle's own rows.
func (b bundleHooks) Requirements(ctx context.Context, _ builtinintegrations.Connection) ([]builtinintegrations.Requirement, error) {
	raw, err := b.s.Integrations.Call(ctx, b.name, integrationpod.MethodRequirements, map[string]any{})
	if err != nil {
		return nil, b.hookError(integrationpod.MethodRequirements, err)
	}
	rows, err := integrationpod.ParseRequirements(orEmptyObject(raw))
	if err != nil {
		return nil, fmt.Errorf("it answered with rows Kivali cannot read: %v", err)
	}
	return rows, nil
}

// Uninstall is kivali/uninstall: the bundle undoes its installation on
// the service.
func (b bundleHooks) Uninstall(ctx context.Context, _ builtinintegrations.Connection) error {
	_, err := b.s.Integrations.Call(ctx, b.name, integrationpod.MethodUninstall, map[string]any{})
	return b.hookError(integrationpod.MethodUninstall, err)
}

// orEmptyObject is raw, or {} for a response with no result.
func orEmptyObject(raw []byte) []byte {
	if len(raw) == 0 || string(raw) == "null" {
		return []byte("{}")
	}
	return raw
}
