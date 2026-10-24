// Package web (integration_builtin.go) — the integrations that ship
// with Kivali, as core runs them.
//
// A built-in integration (internal/builtinintegrations) has no pod and
// no shim: core calls its Go functions directly, with the connection's
// values read from the record and its Secret at that moment. Everything
// around the call is a bundle's: the same access checks, the same
// filter of its list against the manifest, the same answer for the
// agent (each built-in answer goes through integrationCallAnswer as a
// pod's MCP result would), the same scrubber, the same audit. It is
// always running while its code is in the binary.
//
// A shipped bundle (builtinintegrations.ShippedBundle) is built in too,
// so the owner's refusals are a built-in's, but it runs as an uploaded
// bundle does: in its own pod, through its shim, running while the
// shim is connected. Of a built-in record, builtinFor gives the Go
// code, shippedBundle says the binary carries it as a bundle, and one
// that is neither is a record this binary does not carry
// (integrationView.staleBuiltin).

package web

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"slices"
	"strings"
	"time"

	"github.com/kivali-ai/kivali/internal/agentpod"
	"github.com/kivali-ai/kivali/internal/builtinintegrations"
	"github.com/kivali-ai/kivali/internal/clock"
	"github.com/kivali-ai/kivali/internal/integrationpod"
	"github.com/kivali-ai/kivali/internal/store"
)

// builtinFor is the code of it, when it is an integration that ships
// with Kivali: its record is built in and this binary carries it. nil
// for any other.
func (s *Server) builtinFor(it store.Integration) builtinintegrations.Integration {
	if !it.Builtin {
		return nil
	}
	return s.BuiltinIntegrations.Lookup(it.Name)
}

// shippedBundle reports whether it ships with Kivali as a bundle this
// binary carries and installed: its record is built in, its name is in
// ShippedBundles and no built-in of Go code has it, and boot (or a
// restore) installed it with the version the record has in use. One
// whose install failed, or whose record names another version, runs
// nothing: what it would run is not the binary's.
func (s *Server) shippedBundle(it store.Integration) bool {
	if !it.Builtin || it.CurrentVersion == "" || s.BuiltinIntegrations.Lookup(it.Name) != nil || !s.ShippedBundles.Has(it.Name) {
		return false
	}
	s.shippedMu.Lock()
	defer s.shippedMu.Unlock()
	return s.shippedInstalled[it.Name] == it.CurrentVersion
}

// SetShippedBundlesInstalled records the shipped bundles boot or a
// restore installed (store.InstallShippedBundles), by name, with the
// version each made the one in use. main.go calls it at boot.
func (s *Server) SetShippedBundlesInstalled(installed map[string]string) {
	s.shippedMu.Lock()
	defer s.shippedMu.Unlock()
	s.shippedInstalled = installed
}

// integrationRunning reports whether it can be called now: a built-in
// one always, a bundle (a shipped one too) while its shim is
// connected.
func (s *Server) integrationRunning(it store.Integration) bool {
	return s.builtinFor(it) != nil || s.Integrations.Running(it.Name)
}

// refuseNoConnection answers a call of a built-in integration that has
// no connection to call with.
func refuseNoConnection(display string) string {
	return fmt.Sprintf("The %s integration has no connection yet. Ask the owner to connect it.", display)
}

// builtinConnection is name's connection connID ("" for the oldest)
// for a built-in's function: its id and every field's value by key,
// secrets included; the secret of its scoped proxy credential ("" for
// none), which is not a field and which core alone puts in a shell
// job; and the scrubber of all of these secrets. store.ErrNotFound
// with no such connection.
func (s *Server) builtinConnection(ctx context.Context, name, connID string) (builtinintegrations.Connection, string, *strings.Replacer, error) {
	conn, proxySecret, secretValues, err := s.builtinConnectionSecrets(ctx, name, connID)
	if err != nil {
		return builtinintegrations.Connection{}, "", nil, err
	}
	return conn, proxySecret, newScrubber(secretValues), nil
}

// builtinConnectionSecrets is builtinConnection with the secret values
// themselves in place of their scrubber: the sensitive fields' values
// and the proxy credential's secret. For a connection made by signing
// in, Fields holds the current access token under its credential_keys
// name (refreshed first when it is about to expire) and never the
// client secret; errSignInNeeded and errVendorUnavailable say why
// there is none.
func (s *Server) builtinConnectionSecrets(ctx context.Context, name, connID string) (builtinintegrations.Connection, string, []string, error) {
	_, rec, values, secretValues, proxySecret, err := s.Integrations.readConnection(ctx, name, connID)
	if err != nil {
		return builtinintegrations.Connection{}, "", nil, err
	}
	if proxySecret != "" {
		secretValues = append(secretValues, proxySecret)
	}
	return builtinintegrations.Connection{ID: rec.ID, Fields: values}, proxySecret, secretValues, nil
}

// builtinStoredConnection is builtinConnection as the connection is
// stored, with no sign-in's access token and no refresh: for a caller
// that holds lockIntegrations, which a refresh takes.
func (s *Server) builtinStoredConnection(ctx context.Context, name, connID string) (builtinintegrations.Connection, string, error) {
	_, rec, values, _, proxySecret, _, err := s.Integrations.readStoredConnection(ctx, name, connID)
	if err != nil {
		return builtinintegrations.Connection{}, "", err
	}
	return builtinintegrations.Connection{ID: rec.ID, Fields: values}, proxySecret, nil
}

// refuseSignInNeeded answers a call of a built-in whose connection
// needs the owner to sign in again.
func refuseSignInNeeded(display string) string {
	return fmt.Sprintf("%s's connection needs the owner to sign in again; ask the owner.", display)
}

// refuseVendorUnavailable answers a call of a built-in whose access
// token expired while the vendor did not answer its refresh.
func refuseVendorUnavailable(display string) string {
	return fmt.Sprintf("%s did not answer when Kivali renewed the connection's sign-in; try again later or tell the owner.", display)
}

// jobConnectionNote is what the audit row of a call's second request
// adds when its job ran through another connection than the
// integration has now, jobID, and the scrubber of both connections'
// secrets, rowSecrets being the current connection's. A job connection
// that is no longer there leaves rowSecrets alone, and the note says
// so. jobID is text from the agent's pod: it only picks which secrets
// are scrubbed.
func (s *Server) jobConnectionNote(ctx context.Context, v agentIntegration, jobID string, rowSecrets []string) (string, *strings.Replacer) {
	// As stored: building a scrubber never sends a vendor a refresh.
	// The stored tokens and the proxy credential's secret are among the
	// secrets.
	_, _, _, jobSecrets, proxySecret, _, err := s.Integrations.readStoredConnection(ctx, v.name, jobID)
	if proxySecret != "" {
		jobSecrets = append(jobSecrets, proxySecret)
	}
	if err != nil {
		return " (it ran through a connection that is no longer there, so only the current connection's secrets were scrubbed)", newScrubber(rowSecrets)
	}
	return " (it ran through an earlier connection)", newScrubber(append(slices.Clone(rowSecrets), jobSecrets...))
}

// callerOf is the agent and its access as a built-in reads them.
func callerOf(slug string, access json.RawMessage) builtinintegrations.Caller {
	c := builtinintegrations.Caller{Agent: slug, Access: map[string]any{}}
	_ = json.Unmarshal(access, &c.Access)
	if c.Access == nil {
		c.Access = map[string]any{}
	}
	return c
}

// runBuiltin runs fn for at most d on clk, as a pod's request waits: a
// function still running then is told to stop through its context and
// the answer is errIntegrationTimeout. A function that panics fails
// with an error rather than taking the server down.
func runBuiltin[T any](ctx context.Context, clk clock.Clock, d time.Duration, fn func(context.Context) (T, error)) (T, error) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	type outcome struct {
		v   T
		err error
	}
	// The deadline is armed before fn starts, so it counts from the call.
	t := clk.NewTimer(d)
	defer t.Stop()
	ch := make(chan outcome, 1)
	go func() {
		defer func() {
			if p := recover(); p != nil {
				var zero T
				ch <- outcome{zero, fmt.Errorf("the integration failed: %v", p)}
			}
		}()
		v, err := fn(ctx)
		ch <- outcome{v, err}
	}()
	var zero T
	select {
	case o := <-ch:
		return o.v, o.err
	case <-t.C():
		return zero, errIntegrationTimeout
	case <-ctx.Done():
		return zero, ctx.Err()
	}
}

// builtinResultJSON is a built-in's answer as the MCP tools/call result
// a pod's server would send, so it is read by the same code: each
// content string a text block. An error of the function's own is a
// JSON-RPC error with its words; the deadline and a caller that went
// away are passed on as they are.
func builtinResultJSON(res *builtinintegrations.Result, err error) (json.RawMessage, error) {
	switch {
	case errors.Is(err, errIntegrationTimeout), errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return nil, err
	case err != nil:
		return nil, &integrationpod.RPCError{Code: -32603, Message: err.Error()}
	}
	type block struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	out := struct {
		Content []block `json:"content"`
		IsError bool    `json:"isError"`
	}{Content: []block{}}
	if res != nil {
		for _, c := range res.Content {
			out.Content = append(out.Content, block{Type: "text", Text: c})
		}
		out.IsError = res.IsError
	}
	return json.Marshal(out)
}

// builtinListedTools is v's actions for slug as a built-in lists them,
// in the shape of a pod's tools/list answer, for agentToolsOf to
// filter.
func (s *Server) builtinListedTools(ctx context.Context, v agentIntegration, slug string) (listedTools, error) {
	tools, err := runBuiltin(ctx, s.clk(), integrationShortTimeout, func(ctx context.Context) ([]builtinintegrations.Tool, error) {
		return v.builtin.Tools(ctx, callerOf(slug, v.access))
	})
	if err != nil {
		return listedTools{}, err
	}
	var list listedTools
	for _, t := range tools {
		list.Tools = append(list.Tools, listedTool{Name: t.Name, Description: t.Description, InputSchema: t.InputSchema})
	}
	return list, nil
}

// builtinAgentCall runs slug's call of tool on v's built-in with the
// connection as it stands, and answers as runAgentIntegrationCall does
// for a pod: scrubbed of the connection's secrets, with the audit row
// written.
//
// A built-in that runs part of an action in the agent's shell
// (builtinintegrations.ShellJobIntegration) answers the first request
// with a job: core adds the connection's scoped proxy credential to it
// and hands it to the agent's MCP server, with no audit row yet. The
// second request carries result, what the shell gave back; core has
// worked out the integration, the agent's access and the connection
// again exactly as for any call, and the built-in's Complete puts the
// result in words. That writes the call's one audit row. A Refusal, on
// either request, is audited as not allowed and answered at once.
//
// Each request is a call of its own under integrationCallTimeout; the
// job runs between them in the shell, under the job's own bound.
func (s *Server) builtinAgentCall(ctx context.Context, start time.Time, slug string, v agentIntegration, tool string, args json.RawMessage, result *agentpod.IntegrationShellJobResult) agentpod.IntegrationCallResponse {
	// The integration's connection, which every agent's call uses.
	conn, proxySecret, rowSecrets, err := s.builtinConnectionSecrets(ctx, v.name, "")
	scrub := newScrubber(rowSecrets)
	switch {
	case errors.Is(err, store.ErrNotFound):
		s.auditAgentCall(v.name, start, slug, tool, false, "the integration has no connection")
		return integrationError(refuseNoConnection(v.display))
	case errors.Is(err, errSignInNeeded):
		s.auditAgentCall(v.name, start, slug, tool, false, errSignInNeeded.Error())
		return integrationError(refuseSignInNeeded(v.display))
	case errors.Is(err, errVendorUnavailable):
		s.auditAgentCall(v.name, start, slug, tool, true, "the vendor did not answer the connection's refresh")
		return integrationError(refuseVendorUnavailable(v.display))
	case err != nil:
		s.auditAgentCall(v.name, start, slug, tool, true, "the connection could not be read")
		return integrationError(fmt.Sprintf("Kivali could not read the %s integration's connection; try again later or tell the owner.", v.display))
	}
	caller := callerOf(slug, v.access)
	run := func(ctx context.Context) (*builtinintegrations.Result, error) {
		return v.builtin.Call(ctx, caller, conn, tool, args)
	}
	jobNote := ""
	if result != nil {
		sj, ok := v.builtin.(builtinintegrations.ShellJobIntegration)
		if !ok {
			s.auditAgentCall(v.name, start, slug, tool, false, reasonNoShellJob)
			return integrationError(refuseNoShellJob(v.display))
		}
		// The result is text from the agent's pod. It only becomes the
		// agent's answer and the audit's reason, both scrubbed, so
		// nothing binds it to the job the first request handed out; its
		// connection id only widens the scrubber and adds a note to the
		// audit (jobConnectionNote). The row counts from the job's
		// start.
		start = start.Add(-shellJobDuration(*result))
		// A job that ran through another connection than the
		// integration has now (it was uninstalled and installed again
		// while the job ran): its words may quote that connection's
		// secrets, and the audit row says so.
		if g := result.Git; g != nil && g.ConnectionID != "" && g.ConnectionID != conn.ID {
			jobNote, scrub = s.jobConnectionNote(ctx, v, g.ConnectionID, rowSecrets)
		}
		run = func(ctx context.Context) (*builtinintegrations.Result, error) {
			return sj.Complete(ctx, caller, conn, tool, args, *result)
		}
	}
	res, callErr := runBuiltin(ctx, s.clk(), integrationCallTimeout, run)
	var refusal *builtinintegrations.Refusal
	if errors.As(callErr, &refusal) {
		s.auditAgentCall(v.name, start, slug, tool, false, scrub.Replace(refusal.Reason)+jobNote)
		return integrationError(scrub.Replace(refusal.Text))
	}
	if callErr == nil && res != nil && res.ShellJob != nil {
		if result != nil {
			s.auditAgentCall(v.name, start, slug, tool, true, "the integration asked for a second shell job")
			return integrationError(fmt.Sprintf("The %s integration did not finish the call; tell the owner.", v.display))
		}
		return s.builtinShellJob(ctx, start, slug, v, tool, conn, proxySecret, res.ShellJob, scrub.Replace(res.Audit))
	}
	raw, callErr := builtinResultJSON(res, callErr)
	resp, failed := integrationCallAnswer(v.display, raw, callErr)
	for i := range resp.Content {
		resp.Content[i] = scrub.Replace(resp.Content[i])
	}
	if callErr == nil && res != nil && res.Audit != "" {
		failed = res.Audit
	}
	s.auditAgentCall(v.name, start, slug, tool, true, scrub.Replace(failed)+jobNote)
	return resp
}

// builtinShellJob is the first request's answer for a call whose
// built-in asked for job: the job, with the connection's scoped proxy
// credential added to a git one. It is not yet the call's answer, but
// the job carries the token out, so it writes an audit row of its own,
// with started (the built-in's Result.Audit, "started: <owner/name>"
// for git) as its reason: a second request that never comes still
// leaves the owner a row. The second request writes the row of how it
// ended.
func (s *Server) builtinShellJob(ctx context.Context, start time.Time, slug string, v agentIntegration, tool string, conn builtinintegrations.Connection, proxySecret string, job *agentpod.IntegrationShellJob, started string) agentpod.IntegrationCallResponse {
	if job.Git != nil {
		if _, ok := v.builtin.(builtinintegrations.HostsOf); ok && proxySecret == "" {
			var err error
			if proxySecret, err = s.builtinProxySecret(ctx, v.name, v.builtin, conn.ID); err != nil {
				log.Printf("integration %s: the connection's proxy credential: %v", v.name, err)
				s.auditAgentCall(v.name, start, slug, tool, true, "the connection's proxy credential could not be made")
				return integrationError(fmt.Sprintf("Kivali could not prepare the %s integration's connection; try again later or tell the owner.", v.display))
			}
		}
		if proxySecret != "" {
			job.Git.Proxy = agentpod.GitProxy{Username: builtinintegrations.ProxyUser(v.name, conn.ID), Password: proxySecret}
		}
	}
	if started == "" {
		started = "started"
	}
	s.auditAgentCall(v.name, start, slug, tool, true, started)
	return agentpod.IntegrationCallResponse{Content: []string{}, ShellJob: job}
}

// shellJobMaxDuration bounds the duration a shell job's result reports,
// which the audit row counts.
const shellJobMaxDuration = 15 * time.Minute

// shellJobDuration is how long result says its job ran, within
// [0, shellJobMaxDuration].
func shellJobDuration(result agentpod.IntegrationShellJobResult) time.Duration {
	switch {
	case result.Git == nil || result.Git.DurationMs <= 0:
		return 0
	case result.Git.DurationMs >= shellJobMaxDuration.Milliseconds():
		return shellJobMaxDuration
	}
	return time.Duration(result.Git.DurationMs) * time.Millisecond
}

// builtinOwnerCall is the owner's call of a built-in with its
// connection connID ("" for the oldest) for Test connection (verify)
// or the options a field lists (choices name), as the MCP tools/call
// result a pod's server would answer: the function's words as a text
// block, its error as an error result; and the scrubber of that
// connection's secrets (nil when it could not be read). With no
// connection the error says so.
func (s *Server) builtinOwnerCall(ctx context.Context, bi builtinintegrations.Integration, name, connID string, verify bool, choices string) (json.RawMessage, *strings.Replacer, error) {
	conn, _, scrub, err := s.builtinConnection(ctx, name, connID)
	switch {
	case errors.Is(err, store.ErrNotFound):
		raw, err := builtinResultJSON(&builtinintegrations.Result{Content: []string{"the integration has no connection"}, IsError: true}, nil)
		return raw, nil, err
	case errors.Is(err, errSignInNeeded), errors.Is(err, errVendorUnavailable):
		raw, err := builtinResultJSON(&builtinintegrations.Result{Content: []string{err.Error()}, IsError: true}, nil)
		return raw, newScrubber(nil), err
	case err != nil:
		raw, err := builtinResultJSON(&builtinintegrations.Result{Content: []string{"the connection could not be read"}, IsError: true}, nil)
		return raw, nil, err
	}
	res, err := runBuiltin(ctx, s.clk(), integrationCallTimeout, func(ctx context.Context) (*builtinintegrations.Result, error) {
		if verify {
			msg, err := bi.Verify(ctx, conn)
			if err != nil {
				return &builtinintegrations.Result{Content: []string{err.Error()}, IsError: true}, nil
			}
			return &builtinintegrations.Result{Content: []string{msg}}, nil
		}
		options, err := bi.Choices(ctx, conn, choices)
		if err != nil {
			return &builtinintegrations.Result{Content: []string{err.Error()}, IsError: true}, nil
		}
		b, err := json.Marshal(nonNilStrings(options))
		if err != nil {
			return nil, err
		}
		return &builtinintegrations.Result{Content: []string{string(b)}}, nil
	})
	raw, err := builtinResultJSON(res, err)
	return raw, scrub, err
}
