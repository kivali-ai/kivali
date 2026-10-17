package agentpod

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// WebCommandRequest is one web_* tool call as the subagent's MCP
// server posts it to core: the command (the tool's name without its
// web_ prefix) and the tool's input as the model wrote it.
type WebCommandRequest struct {
	Command string          `json:"command"`
	Args    json.RawMessage `json:"args,omitempty"`
}

// WebCommandResponse is core's answer, relayed to the model as the
// tool's result: the text, an optional image block, and whether it is
// an error (a refused navigation, a ref that no longer exists, a
// browser that could not be started).
type WebCommandResponse struct {
	Text    string    `json:"text,omitempty"`
	Image   *WebImage `json:"image,omitempty"`
	IsError bool      `json:"is_error,omitempty"`
}

// WebImage is an image block in a web tool's result.
type WebImage struct {
	MIME string `json:"mime"`
	Data []byte `json:"data"`
}

// WebCommand posts one web tool call for a web subagent to core, which
// checks the task's session and state, relays the command to the
// task's browser and returns what the model should read.
//
// POST /v1/agent/{slug}/subagents/{id}/web. A non-2xx response is a
// transport error: the id is not one of this agent's running web
// tasks, or core refused the request outright. Core answers only once
// the browser has: the first command on a cold browser waits for its
// pod to start, a navigate for the page to settle. The call runs on
// the client with no response-header timeout, so only ctx bounds it.
func (c *Client) WebCommand(ctx context.Context, subagentID string, req WebCommandRequest) (*WebCommandResponse, error) {
	if strings.TrimSpace(subagentID) == "" {
		return nil, fmt.Errorf("agentpod: WebCommand: subagent id is required")
	}
	if strings.TrimSpace(req.Command) == "" {
		return nil, fmt.Errorf("agentpod: WebCommand: command is required")
	}
	body, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("agentpod: marshal WebCommandRequest: %w", err)
	}
	url := fmt.Sprintf("http://core/v1/agent/%s/subagents/%s/web", c.slug, subagentID)
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("agentpod: build WebCommand request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set(SlugHeader, c.slug)
	resp, err := c.long.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("agentpod: WebCommand: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode/100 != 2 {
		errBody, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("agentpod: WebCommand HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(errBody)))
	}
	var out WebCommandResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("agentpod: decode WebCommandResponse: %w", err)
	}
	return &out, nil
}
