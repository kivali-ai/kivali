package agentpod

import (
	"slices"
	"strings"
	"testing"
)

// TestWebSubagentPromptStatesItsGuards: a task that uploads files is
// told what a person approved, that it is held to its site and what
// that does not decide, and has no web_eval; every task is told what
// it sends out is counted.
func TestWebSubagentPromptStatesItsGuards(t *testing.T) {
	upload := WebSubagentSystemPrompt("alice", "send the quote", OpenWebSession, "vendor.example", "https://portal.vendor.example/", []string{"quote.pdf"}, false)
	for _, want := range []string{"THIS TASK IS HELD TO ONE SITE", "a person approved uploading this task's files", "and nothing more", "puts a file only on a page of that site", "has changed since", "up to the site", "held to vendor.example", "quote.pdf", "WHAT YOU SEND OUT IS COUNTED"} {
		if !strings.Contains(upload, want) {
			t.Errorf("upload prompt lacks %q:\n%s", want, upload)
		}
	}
	if strings.Contains(upload, "`web_eval` runs JavaScript") {
		t.Errorf("upload prompt names web_eval:\n%s", upload)
	}
	if slices.Contains(WebSubagentTools(OpenWebSession, "vendor.example", false).Kivali, WebEvalTool) {
		t.Error("a task held to a site has web_eval")
	}

	open := WebSubagentSystemPrompt("alice", "price it", OpenWebSession, "", "https://vendor.example/", nil, false)
	for _, want := range []string{"NOT SIGNED IN ANYWHERE", "WHAT YOU SEND OUT IS COUNTED", "2 KB an action and 8 KB a task"} {
		if !strings.Contains(open, want) {
			t.Errorf("open prompt lacks %q:\n%s", want, open)
		}
	}
	if !slices.Contains(WebSubagentTools(OpenWebSession, "", false).Kivali, WebEvalTool) {
		t.Error("the open session lost web_eval")
	}

	site := WebSubagentSystemPrompt("alice", "check the invoice", "vendor.example", "vendor.example", "", nil, true)
	if !strings.Contains(site, "LOCKED TO ONE SITE") || !strings.Contains(site, "WHAT YOU SEND OUT IS COUNTED") {
		t.Errorf("site-session prompt:\n%s", site)
	}
}
