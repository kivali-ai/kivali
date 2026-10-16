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

// SubagentResultResponse is core's answer to a submit_result call,
// relayed to the model as the tool's result: the text to show and
// whether it is an error (an invalid result, with what to fix).
type SubagentResultResponse struct {
	Body    string `json:"body,omitempty"`
	IsError bool   `json:"is_error,omitempty"`
}

// SubmitSubagentResult posts a subagent's submit_result call to core,
// which validates it against the task's schema and either accepts it
// as the job's answer or returns the errors. The body is the
// submission exactly as the model wrote it; nothing is checked here.
//
// POST /v1/agent/{slug}/subagents/{id}/result. A non-2xx response is
// a transport error: the subagent id was not one of this agent's
// running tasks, or core refused the request outright.
func (c *Client) SubmitSubagentResult(ctx context.Context, subagentID string, submission json.RawMessage) (*SubagentResultResponse, error) {
	if strings.TrimSpace(subagentID) == "" {
		return nil, fmt.Errorf("agentpod: SubmitSubagentResult: subagent id is required")
	}
	if !json.Valid(submission) {
		return nil, fmt.Errorf("agentpod: SubmitSubagentResult: submission is not valid JSON")
	}
	url := fmt.Sprintf("http://core/v1/agent/%s/subagents/%s/result", c.slug, subagentID)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(submission))
	if err != nil {
		return nil, fmt.Errorf("agentpod: build SubmitSubagentResult request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(SlugHeader, c.slug)
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("agentpod: SubmitSubagentResult: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode/100 != 2 {
		errBody, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("agentpod: SubmitSubagentResult HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(errBody)))
	}
	var out SubagentResultResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("agentpod: decode SubagentResultResponse: %w", err)
	}
	return &out, nil
}
