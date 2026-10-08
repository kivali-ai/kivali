// Package controlclient is the typed client for talking to the Kivali
// web process's control socket from spawned `kivali mcp` subprocesses.
//
// The control socket is a Unix-domain HTTP listener owned by the web
// process (see internal/web/control_socket.go). Each MCP subprocess
// connects here to coordinate with the parent — today only RouteMessage,
// but the API is shaped to grow as the MCP fleet's needs do.
//
// Why HTTP-over-unix and not a custom protocol:
//   - Pure stdlib (`net/http` with a `unix` dialer).
//   - Curl-debuggable: `curl --unix-socket <path> http://x/route-message`.
//   - Routes-as-versioning: new endpoints land at new paths without
//     touching existing callers.
package controlclient

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"time"

	"github.com/kivali-ai/kivali/internal/agentpod"
	"github.com/kivali-ai/kivali/internal/store"
)

// Client wraps an http.Client whose transport dials the control
// socket.
//
// Most calls share one short timeout: they are fast RPCs where a hang
// should surface as a failure rather than a wedge. RunNestedSubagent
// is the single exception and uses the unbounded `blocking` client,
// because a sub-lead's dispatch really does wait for the work — see
// that method. Adding a second long-running endpoint here should mean
// justifying it the same way, not reaching for `blocking` because it
// happens to exist.
//
// The slug field is the bound caller identity: every MCP subprocess
// is launched with `--agent <slug>` and constructs its Client with
// that slug. RouteMessage and RunSubagent stamp the X-Kivali-Agent
// header from this field on every call so the parent can
// authoritatively bind the request to a slug — body fields like
// store.Message.From are NOT trusted; the parent overrides them
// from the header.
//
// Safe for concurrent use; one Client per subprocess is enough.
type Client struct {
	slug string
	http *http.Client

	// blocking is the same transport with no timeout, used only by
	// RunNestedSubagent. See that method for why one call needs it
	// and why every other call must not have it.
	blocking *http.Client
}

// New returns a Client that talks to the control socket at socketPath
// on behalf of slug. The Client itself does no I/O — pass the path;
// failures surface on the first call. Slug is required; an empty
// slug would let the parent's slug-binding gate fall through.
func New(socketPath, slug string) *Client {
	dial := func(ctx context.Context, _, _ string) (net.Conn, error) {
		var d net.Dialer
		return d.DialContext(ctx, "unix", socketPath)
	}
	return &Client{
		slug: slug,
		http: &http.Client{
			Transport: &http.Transport{DialContext: dial},
			Timeout:   10 * time.Second,
		},
		blocking: &http.Client{
			Transport: &http.Transport{DialContext: dial},
		},
	}
}

// RunSubagent is the MCP-side entry to the parent web process's
// subagent runner. The MCP handler forwards the raw JSON arguments
// (the LLM's `subagent` tool input) here; the web process owns
// staging, runner spawn, transcript watching, and live progress
// emission to the parent's chat hub. Returns the DISPATCH RECEIPT for
// the MCP handler to echo back to Claude Code — the jobs themselves
// run on past this call.
//
// On the short client: dispatch returns as soon as the jobs are
// registered, so the 10s timeout is ample and an unbounded wait would
// only serve to hide a wedged core as an agent hanging forever on a
// tool call.
func (c *Client) RunSubagent(ctx context.Context, parent string, arguments json.RawMessage) (string, error) {
	body, err := json.Marshal(map[string]any{
		"parent":    parent,
		"arguments": arguments,
	})
	if err != nil {
		return "", fmt.Errorf("controlclient: marshal: %w", err)
	}
	url := fmt.Sprintf("http://x/v1/agent/%s/run-subagent", c.slug)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return "", fmt.Errorf("controlclient: new request: %w", err)
	}
	req.Header.Set("content-type", "application/json")
	req.Header.Set(agentpod.SlugHeader, c.slug)
	resp, err := c.http.Do(req)
	if err != nil {
		return "", fmt.Errorf("controlclient: run-subagent: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("controlclient: run-subagent: %s: %s", resp.Status, bytes.TrimSpace(respBody))
	}
	return string(respBody), nil
}

// RunNestedSubagent is a SUB-LEAD's dispatch: it blocks until every
// worker has finished and returns their answers, not a receipt.
//
// callerID is the dispatching subagent's own id. Core resolves it in
// the job registry to find the caller's recorded depth, and derives
// the new jobs' depth from that — nothing in this body is trusted to
// say how deep it is.
//
// It runs on a SEPARATE, unbounded client. The short client exists
// because ordinary control calls are fast RPCs where a hang should
// surface as a failure rather than a wedge. This call genuinely blocks for as long as the work takes, so the
// 10s timeout would not protect anything — it would just sever a
// perfectly healthy batch at ten seconds and report a failure that did
// not happen. The caller's own ctx (the subagent's turn) is what bounds
// it, which is the right bound: when the CEO stops the turn, the
// cancellation cascades to the workers too.
func (c *Client) RunNestedSubagent(ctx context.Context, callerID string, arguments json.RawMessage) (string, error) {
	body, err := json.Marshal(map[string]any{
		"parent":             c.slug,
		"caller_subagent_id": callerID,
		"arguments":          arguments,
	})
	if err != nil {
		return "", fmt.Errorf("controlclient: marshal: %w", err)
	}
	url := fmt.Sprintf("http://x/v1/agent/%s/run-subagent", c.slug)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return "", fmt.Errorf("controlclient: new request: %w", err)
	}
	req.Header.Set("content-type", "application/json")
	req.Header.Set(agentpod.SlugHeader, c.slug)
	resp, err := c.blocking.Do(req)
	if err != nil {
		return "", fmt.Errorf("controlclient: run-subagent (nested): %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("controlclient: run-subagent (nested): %s: %s", resp.Status, bytes.TrimSpace(respBody))
	}
	return string(respBody), nil
}

// SubagentStatus fetches the caller's background-job list, already
// rendered for the model by core.
//
// A map lookup at the far end, so it shares the short timeout with
// everything else here — a status call that hangs is worse than one
// that fails.
func (c *Client) SubagentStatus(ctx context.Context, parent string) (string, error) {
	return c.subagentControl(ctx, "subagent-status", map[string]any{"parent": parent})
}

// SubagentCancel asks core to stop one of the caller's background
// jobs. Core checks ownership; a slug cannot cancel another agent's
// work by guessing an id.
func (c *Client) SubagentCancel(ctx context.Context, parent, id string) (string, error) {
	return c.subagentControl(ctx, "subagent-cancel", map[string]any{"parent": parent, "id": id})
}

// subagentControl is the shared POST for the two job-control
// endpoints: same slug binding, same short timeout, same "body is the
// model-facing text" response contract.
func (c *Client) subagentControl(ctx context.Context, path string, payload map[string]any) (string, error) {
	body, err := json.Marshal(payload)
	if err != nil {
		return "", fmt.Errorf("controlclient: marshal: %w", err)
	}
	url := fmt.Sprintf("http://x/v1/agent/%s/%s", c.slug, path)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return "", fmt.Errorf("controlclient: new request: %w", err)
	}
	req.Header.Set("content-type", "application/json")
	req.Header.Set(agentpod.SlugHeader, c.slug)
	resp, err := c.http.Do(req)
	if err != nil {
		return "", fmt.Errorf("controlclient: %s: %w", path, err)
	}
	defer func() { _ = resp.Body.Close() }()
	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("controlclient: %s: %s: %s", path, resp.Status, bytes.TrimSpace(respBody))
	}
	return string(respBody), nil
}

// RouteMessage POSTs a published message to the parent's
// slug-scoped route endpoint. The parent overrides msg.From with the
// caller's bound slug from the X-Kivali-Agent header — body's From is
// NOT trusted. The parent's single-writer messenger lock serializes
// concurrent routes from any caller.
//
// Errors carry the parent's error body when available so the MCP
// subprocess can surface a meaningful tool_result to the agent.
func (c *Client) RouteMessage(ctx context.Context, msg store.Message) error {
	body, err := json.Marshal(msg)
	if err != nil {
		return fmt.Errorf("controlclient: marshal: %w", err)
	}
	// Host header is irrelevant for unix sockets but http.NewRequest
	// requires a parseable URL. The "x" host is convention; the
	// Transport's DialContext ignores it entirely.
	url := fmt.Sprintf("http://x/v1/agent/%s/route-message", c.slug)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("controlclient: new request: %w", err)
	}
	req.Header.Set("content-type", "application/json")
	req.Header.Set(agentpod.SlugHeader, c.slug)
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("controlclient: route-message: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode == http.StatusNoContent {
		return nil
	}
	// Read up to 8 KB of error body for the message — enough context
	// for an agent-visible tool_result without unbounded copies.
	buf, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<10))
	return fmt.Errorf("controlclient: route-message: %s: %s", resp.Status, bytes.TrimSpace(buf))
}
