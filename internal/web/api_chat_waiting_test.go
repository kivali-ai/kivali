package web

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/kivali-ai/kivali/internal/agentpod"
	"github.com/kivali-ai/kivali/internal/provider"
	"github.com/kivali-ai/kivali/internal/store"
	"github.com/kivali-ai/kivali/internal/web/apitypes"
)

// TestAPISubagentWaitingForSignIn: a web task waiting for a person
// carries its state, site, wait reason and browser state on the wire.
func TestAPISubagentWaitingForSignIn(t *testing.T) {
	srv := transcriptServer(t)
	srv.SubagentService = &SubagentService{Store: srv.Store, Provider: provider.MockProvider{}}
	srv.Browsers = newBrowserRegistry(srv.Store, newFakeBrowserProv(), srv.clk(), 3, t.Logf, func(slug, id string) string { return subagentFilesRoot(srv.Store, slug, id) })
	srv.SubagentService.Browsers = srv.Browsers
	at := time.Date(2026, 10, 16, 9, 0, 0, 0, time.UTC)
	srv.SubagentService.registerJob(&subagentJob{
		ID: "cccc3333", Parent: "alice", Description: "price the renewal", Kind: agentpod.SubagentKindWeb, Model: provider.MockModelRetired, Effort: "low",
		Session: "vendor.example", Site: "vendor.example", State: subagentJobWaiting, WaitReason: "mfa", WaitSince: at, StartedAt: at, QueuedAt: at, Depth: 1, done: make(chan struct{}),
	})
	writeSubagentMetaFile(t, srv, "cccc3333", subagentMeta{ID: "cccc3333", Description: "price the renewal", Model: provider.MockModelRetired, Effort: "low", Status: subagentJobWaiting, Kind: agentpod.SubagentKindWeb, Session: "vendor.example"})
	if err := srv.Store.AppendSubagentMessage("alice", "cccc3333", store.ChatMessage{Role: store.RoleReceived, Kind: "direct_chat", Content: "find the price", TS: at}); err != nil {
		t.Fatal(err)
	}
	rr := chatDo(t, srv, http.MethodGet, "/api/v1/agents/alice/subagents/cccc3333", "")
	if rr.Code != http.StatusOK {
		t.Fatalf("code = %d: %s", rr.Code, rr.Body.String())
	}
	m := decodeAPI[apitypes.SubagentTranscript](t, rr).Meta
	if m.State != apitypes.SubagentTaskStateWaitingForSignIn || m.Kind != apitypes.SubagentKindWeb || m.Session == nil || *m.Session != "vendor.example" ||
		m.Site == nil || *m.Site != "vendor.example" || m.WaitReason != apitypes.WebSignInReasonMFA || m.Browser != apitypes.BrowserStateStopped || m.Ended != nil || m.Error != nil {
		t.Errorf("meta = %+v", m)
	}
	if body := rr.Body.String(); !strings.Contains(body, `"wait_reason":"mfa"`) || !strings.Contains(body, `"site":"vendor.example"`) || !strings.Contains(body, `"browser":"stopped"`) || strings.Contains(body, `"input"`) {
		t.Errorf("body = %s", body)
	}
}
