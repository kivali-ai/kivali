package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/kivali-ai/kivali/internal/agentpod"
)

// recordingCaller answers every call with resp or err and records
// what it was asked.
type recordingCaller struct {
	resp  *agentpod.IntegrationCallResponse
	err   error
	names []string
	args  []string
}

func (c *recordingCaller) IntegrationCall(_ context.Context, name string, args json.RawMessage) (*agentpod.IntegrationCallResponse, error) {
	c.names = append(c.names, name)
	c.args = append(c.args, string(args))
	return c.resp, c.err
}

// Why this test exists: an integration action's handler is a relay,
// and the model reads what it returns. Core's answer must reach the
// model as it is (its text and its error flag), and a call that never
// reached core must say so as a tool error rather than fail the MCP
// request, which the CLI would show the model as a broken server. A
// name a Kivali tool already has must not be registered over it.
func TestIntegrationToolsRelayToCore(t *testing.T) {
	c := &recordingCaller{resp: &agentpod.IntegrationCallResponse{Content: []string{"posted", "and logged"}}}
	listed := []agentpod.IntegrationTool{
		{Name: "crm_create_post", Description: "Create a post.", InputSchema: json.RawMessage(`{"type":"object"}`)},
		{Name: "share_file", Description: "Not this one."},
	}
	tools := IntegrationTools(c, listed, []Tool{{Name: "share_file"}})
	if len(tools) != 1 || tools[0].Name != "crm_create_post" || tools[0].ReadOnly || string(tools[0].InputSchema) != `{"type":"object"}` {
		t.Fatalf("tools = %+v", tools)
	}
	res, err := tools[0].Handler(context.Background(), json.RawMessage(`{"title":"x"}`))
	if err != nil || res.IsError || len(res.Content) != 2 || res.Content[1] != "and logged" {
		t.Errorf("result = %+v, %v", res, err)
	}
	if len(c.names) != 1 || c.names[0] != "crm_create_post" || c.args[0] != `{"title":"x"}` {
		t.Errorf("relayed %v %v", c.names, c.args)
	}

	c.resp = &agentpod.IntegrationCallResponse{Content: []string{"You have no access to the CRM integration."}, IsError: true}
	if res, _ := tools[0].Handler(context.Background(), nil); !res.IsError || res.Content[0] != "You have no access to the CRM integration." {
		t.Errorf("refusal = %+v", res)
	}

	c.resp, c.err = nil, errors.New("dial unix: no such file")
	res, err = tools[0].Handler(context.Background(), nil)
	if err != nil || !res.IsError || res.Content[0] != "The call did not reach Kivali: dial unix: no such file" {
		t.Errorf("transport error = %+v, %v", res, err)
	}
}
