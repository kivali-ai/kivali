package googleworkspace

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"testing"
)

// Why this test exists: list_labels is the one action this version
// runs end to end, so it proves the path from an agent's call to
// Gmail and back: the token core handed in, Gmail's origin, the answer
// as the agent reads it. An agent whose access names labels must see
// only those, however the owner's entries name them (an option as the
// form stores it, or a bare id); an empty list is every label; and a
// list naming none of the account's labels says so rather than
// answering nothing.
func TestListLabels(t *testing.T) {
	f, g := newFakeGoogle(t)
	answer := func(access map[string]any) string {
		t.Helper()
		access["gmail_read"] = true
		res, err := g.Call(context.Background(), agent(access), testConn(), "list_labels", json.RawMessage(`{}`))
		if err != nil || res == nil || res.IsError || len(res.Content) != 1 {
			t.Fatalf("list_labels = %+v, %v", res, err)
		}
		return res.Content[0]
	}

	all := answer(map[string]any{})
	if want := "Inbox (INBOX)\nSent (SENT)\nDrafts (DRAFT)\nTrash (TRASH)\nAlpha/Work (Label_3)\nClients (old) (Label_12)\nzeta (Label_9)"; all != want {
		t.Errorf("every label:\n%s", all)
	}
	some := answer(map[string]any{"gmail_labels": []string{"Inbox (INBOX)", "Clients (old) (Label_12)", "Label_9", "Gone (Label_404)"}})
	if want := "Inbox (INBOX)\nClients (old) (Label_12)\nzeta (Label_9)"; some != want {
		t.Errorf("the access's labels:\n%s", some)
	}
	none := answer(map[string]any{"gmail_labels": []string{"Gone (Label_404)"}})
	if none != "Your access includes none of the account's labels." {
		t.Errorf("none of them: %q", none)
	}
	for _, r := range f.requests() {
		if r != "gmail /gmail/v1/users/me/labels?" {
			t.Errorf("request %s", r)
		}
	}
	if len(f.requests()) != 3 {
		t.Errorf("requests = %v", f.requests())
	}
}

// Why this test exists: Google's failures reach the agent as an error
// it reads, never as the integration failing, and never with the
// owner's token in them: a token Google refuses sends the agent to the
// owner, as does a permission the owner left unticked when signing in.
func TestListLabelsFailures(t *testing.T) {
	f, g := newFakeGoogle(t)
	reads := agent(map[string]any{"gmail_read": true})
	stale := testConn()
	stale.Fields[CredentialKey] = "ya29.stale"
	res, err := g.Call(context.Background(), reads, stale, "list_labels", nil)
	if err != nil || res == nil || !res.IsError || !strings.Contains(res.Content[0], "HTTP 401") || !strings.HasSuffix(res.Content[0], "Ask the owner.") ||
		strings.Contains(res.Content[0], "ya29") {
		t.Errorf("a token Google refuses: %+v, %v", res, err)
	}

	f.failWith("/gmail/v1/users/me/labels", 403, `{"error":{"code":403,"message":"Request had insufficient authentication scopes.","status":"PERMISSION_DENIED","details":[{"reason":"ACCESS_TOKEN_SCOPE_INSUFFICIENT"}]}}`)
	res, err = g.Call(context.Background(), reads, testConn(), "list_labels", nil)
	if err != nil || res == nil || !res.IsError || res.Content[0] != "Google refused this (HTTP 403): the owner's sign-in does not allow it. Ask the owner." {
		t.Errorf("a permission left unticked: %+v, %v", res, err)
	}

	f.failWith("/gmail/v1/users/me/labels", 429, `{"error":{"code":429,"message":"Too many requests.","status":"RESOURCE_EXHAUSTED"}}`)
	res, _ = g.Call(context.Background(), reads, testConn(), "list_labels", nil)
	if res == nil || !res.IsError || !strings.Contains(res.Content[0], "limiting") || !strings.HasSuffix(res.Content[0], "Try again later.") {
		t.Errorf("a rate limit: %+v", res)
	}

	if _, err := g.Call(context.Background(), reads, testConn(), "list_labels", json.RawMessage(`[1]`)); err == nil {
		t.Error("arguments that are not an object were taken")
	}
	if got := slices.Index(f.requests(), "gmail /gmail/v1/users/me/labels?"); got < 0 {
		t.Errorf("requests = %v", f.requests())
	}
}
