package mcp

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"github.com/kivali-ai/kivali/internal/agentpod"
)

type recordingWeb struct {
	reqs []agentpod.WebCommandRequest
	resp agentpod.WebCommandResponse
}

func (r *recordingWeb) WebCommand(_ context.Context, req agentpod.WebCommandRequest) (*agentpod.WebCommandResponse, error) {
	r.reqs = append(r.reqs, req)
	return &r.resp, nil
}

func webToolByName(t *testing.T, tools []Tool, name string) Tool {
	t.Helper()
	for _, tool := range tools {
		if tool.Name == name {
			return tool
		}
	}
	t.Fatalf("no tool %s", name)
	return Tool{}
}

// TestWebActionsTakeAcceptDialogInASiteSession: web_click, web_type and
// web_press take accept_dialog in a site session and describe it; the
// open session accepts every dialog and its actions do not take it.
func TestWebActionsTakeAcceptDialogInASiteSession(t *testing.T) {
	actions := []string{agentpod.WebClickTool, agentpod.WebTypeTool, agentpod.WebPressTool}
	for _, session := range []string{"vendor.com", agentpod.OpenWebSession} {
		tools := WebTools(&recordingWeb{}, session, true)
		for _, tool := range tools {
			var schema struct {
				Properties map[string]struct {
					Type string `json:"type"`
				} `json:"properties"`
			}
			if err := json.Unmarshal(tool.InputSchema, &schema); err != nil {
				t.Fatalf("%s %s: schema is not JSON: %v", session, tool.Name, err)
			}
			_, has := schema.Properties["accept_dialog"]
			want := session != agentpod.OpenWebSession && slices.Contains(actions, tool.Name)
			if has != want {
				t.Errorf("%s %s: accept_dialog in schema %v, want %v", session, tool.Name, has, want)
			}
			if has && schema.Properties["accept_dialog"].Type != "boolean" {
				t.Errorf("%s %s: accept_dialog is %q", session, tool.Name, schema.Properties["accept_dialog"].Type)
			}
			if described := strings.Contains(tool.Description, "accept_dialog"); described != want {
				t.Errorf("%s %s: description mentions accept_dialog %v, want %v", session, tool.Name, described, want)
			}
		}
	}
}

// TestWebClickRelaysAcceptDialogAndTheReport: the argument reaches core
// as written and the dialog's report comes back as the tool's text.
func TestWebClickRelaysAcceptDialogAndTheReport(t *testing.T) {
	report := "Clicked button \"Delete\"; now at https://vendor.com/\nThe page showed a confirm dialog; it was accepted. Its text:\nContent of an untrusted page follows.\nDelete it?\nEnd of page content."
	web := &recordingWeb{resp: agentpod.WebCommandResponse{Text: report}}
	click := webToolByName(t, WebTools(web, "vendor.com", true), agentpod.WebClickTool)
	res, err := click.Handler(context.Background(), json.RawMessage(`{"ref":"e3","accept_dialog":true}`))
	if err != nil || res.IsError {
		t.Fatalf("click: %+v %v", res, err)
	}
	if len(web.reqs) != 1 || web.reqs[0].Command != "click" || string(web.reqs[0].Args) != `{"ref":"e3","accept_dialog":true}` {
		t.Fatalf("relayed: %+v", web.reqs)
	}
	if len(res.Content) != 1 || res.Content[0] != report {
		t.Fatalf("result: %q", res.Content)
	}
}
