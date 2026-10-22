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

package web

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
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

// integrationRunning reports whether it can be called now: a built-in
// one always, a bundle while its shim is connected.
func (s *Server) integrationRunning(it store.Integration) bool {
	return s.builtinFor(it) != nil || s.Integrations.Running(it.Name)
}

// refuseNoConnection answers a call of a built-in integration that has
// no connection to call with.
func refuseNoConnection(display string) string {
	return fmt.Sprintf("The %s integration has no connection yet. Ask the owner to connect it.", display)
}

// builtinConnection is name's connection for a built-in's function:
// its id and every field's value by key, secrets included, and the
// scrubber of those secrets. store.ErrNotFound with no connection.
func (s *Server) builtinConnection(ctx context.Context, name string) (builtinintegrations.Connection, *strings.Replacer, error) {
	_, rec, values, secretValues, err := s.Integrations.readConnection(ctx, name)
	if err != nil {
		return builtinintegrations.Connection{}, nil, err
	}
	return builtinintegrations.Connection{ID: rec.ID, Fields: values}, newScrubber(secretValues), nil
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
func (s *Server) builtinAgentCall(ctx context.Context, start time.Time, slug string, v agentIntegration, tool string, args json.RawMessage) agentpod.IntegrationCallResponse {
	conn, scrub, err := s.builtinConnection(ctx, v.name)
	switch {
	case errors.Is(err, store.ErrNotFound):
		s.auditAgentCall(v.name, start, slug, tool, false, "the integration has no connection")
		return integrationError(refuseNoConnection(v.display))
	case err != nil:
		s.auditAgentCall(v.name, start, slug, tool, true, "the connection could not be read")
		return integrationError(fmt.Sprintf("Kivali could not read the %s integration's connection; try again later or tell the owner.", v.display))
	}
	caller := callerOf(slug, v.access)
	res, callErr := runBuiltin(ctx, s.clk(), integrationCallTimeout, func(ctx context.Context) (*builtinintegrations.Result, error) {
		return v.builtin.Call(ctx, caller, conn, tool, args)
	})
	raw, callErr := builtinResultJSON(res, callErr)
	resp, failed := integrationCallAnswer(v.display, raw, callErr)
	for i := range resp.Content {
		resp.Content[i] = scrub.Replace(resp.Content[i])
	}
	s.auditAgentCall(v.name, start, slug, tool, true, scrub.Replace(failed))
	return resp
}

// builtinOwnerCall is the owner's call of a built-in for Test
// connection (verify) or the options a field lists (choices name), as
// the MCP tools/call result a pod's server would answer: the function's
// words as a text block, its error as an error result. With no
// connection the error says so.
func (s *Server) builtinOwnerCall(ctx context.Context, bi builtinintegrations.Integration, name string, verify bool, choices string) (json.RawMessage, error) {
	conn, _, err := s.builtinConnection(ctx, name)
	switch {
	case errors.Is(err, store.ErrNotFound):
		return builtinResultJSON(&builtinintegrations.Result{Content: []string{"the integration has no connection"}, IsError: true}, nil)
	case err != nil:
		return builtinResultJSON(&builtinintegrations.Result{Content: []string{"the connection could not be read"}, IsError: true}, nil)
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
	return builtinResultJSON(res, err)
}
