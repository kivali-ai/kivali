package web

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/kivali-ai/kivali/internal/agent"
	"github.com/kivali-ai/kivali/internal/agentpod"
	"github.com/kivali-ai/kivali/internal/provider"
	"github.com/kivali-ai/kivali/internal/store"
	"github.com/kivali-ai/kivali/internal/web/apitypes"
)

// TestAPIHomeWebSignInNeeds: a web task waiting for a person is a
// Needs-you row worded for what the page asks, naming the task and the
// page its window shows, and the org snapshot lists every such task so
// Home learns of the row at once.
func TestAPIHomeWebSignInNeeds(t *testing.T) {
	srv := newTestServer(t)
	if err := srv.Store.CreateAgent(store.Agent{Slug: "buyer", Role: "Buyer", ReportsTo: agent.CEOSlug}, "# role\n"); err != nil {
		t.Fatal(err)
	}
	srv.SubagentService = &SubagentService{Store: srv.Store, Provider: provider.MockProvider{}}
	at := time.Date(2026, 10, 16, 9, 0, 0, 0, time.UTC)
	for i, reason := range []string{"sign_in", "mfa", "captcha", "consent"} {
		srv.SubagentService.registerJob(&subagentJob{
			ID: "w" + reason, Parent: "buyer", Description: "price the renewal", Kind: agentpod.SubagentKindWeb,
			Session: "vendor.example", Site: "vendor.example", State: subagentJobWaiting, WaitReason: reason,
			WaitSince: at.Add(time.Duration(i) * time.Minute), QueuedAt: at, Depth: 1, done: make(chan struct{}),
		})
	}
	srv.SubagentService.registerJob(&subagentJob{ID: "r1", Parent: "buyer", State: subagentJobRunning, QueuedAt: at, Depth: 1, done: make(chan struct{})})
	h := getHome(t, srv)
	byID := map[string]apitypes.NeedItem{}
	for _, n := range h.Needs {
		byID[n.ID] = n
	}
	want := map[string][2]string{
		"web_sign_in:buyer/wsign_in": {"Buyer needs you to sign in to vendor.example", "The page asks for a sign-in. Open the browser, sign in, then choose Continue the task."},
		"web_sign_in:buyer/wmfa":     {"Buyer needs your second factor for vendor.example", "The page asks for a second factor. Open the browser, enter it, then choose Continue the task."},
		"web_sign_in:buyer/wcaptcha": {"Buyer needs you to solve a CAPTCHA on vendor.example", "The page shows a CAPTCHA. Open the browser, solve it, then choose Continue the task."},
		"web_sign_in:buyer/wconsent": {"Buyer needs your consent on vendor.example", "The page asks for a consent only a person can give. Open the browser, answer it, then choose Continue the task."},
	}
	if len(byID) != len(want) {
		t.Fatalf("needs = %+v", h.Needs)
	}
	for id, w := range want {
		n, ok := byID[id]
		if !ok || n.Kind != apitypes.NeedKindWebSignIn || n.Title != w[0] || n.BodyMD != w[1] {
			t.Errorf("%s = %+v, want %q / %q", id, n, w[0], w[1])
			continue
		}
		if n.Subagent == nil || n.Subagent.Site != "vendor.example" || n.Subagent.Description != "price the renewal" || n.Subagent.PageURL != "" || n.Subagent.PageTitle != "" {
			t.Errorf("%s subagent = %+v", id, n.Subagent)
		}
	}
	var snap orgSnapshot
	if err := json.Unmarshal(srv.buildOrgSnapshot(), &snap); err != nil {
		t.Fatal(err)
	}
	if got, want := snap.SignInWaits, []string{"buyer/wcaptcha", "buyer/wconsent", "buyer/wmfa", "buyer/wsign_in"}; !reflect.DeepEqual(got, want) {
		t.Errorf("snapshot sign_in_waits = %v, want %v", got, want)
	}
	// No waits: an empty list, never null.
	srv.SubagentService = nil
	if body := string(srv.buildOrgSnapshot()); !strings.Contains(body, `"sign_in_waits":[]`) {
		t.Errorf("snapshot without waits: %s", body)
	}
}
