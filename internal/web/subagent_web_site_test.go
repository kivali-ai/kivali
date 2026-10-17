package web

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/kivali-ai/kivali/internal/agentpod"
	"github.com/kivali-ai/kivali/internal/browserpod"
	"github.com/kivali-ai/kivali/internal/structured"
)

// TestWebSessionSiteRefusesAddressesAndPrivateNames: a session names a
// site on the public web. An address in any spelling a browser reads
// as one, a name under no public top-level domain, and a bare suffix
// are refused; a private suffix under a public one is a site.
func TestWebSessionSiteRefusesAddressesAndPrivateNames(t *testing.T) {
	for _, in := range []string{
		"127.0.0.1", "127.1", "0x7f.1", "0x7f000001", "0177.0.0.1", "2130706433", "vendor.0x", "::1", "[::1]",
		"localhost", "foo.localhost", "corp", "intranet.corp", "vendor.example", "vendor.com-", "vendor.test",
		"co.uk", "github.io",
	} {
		if got, err := webSessionSite(in); err == nil {
			t.Errorf("webSessionSite(%q) = %q, want a refusal", in, got)
		}
	}
	for in, want := range map[string]string{
		"vendor.com": "vendor.com", "Shop.Vendor.com.": "vendor.com", "docs.vendor.co.uk": "vendor.co.uk",
		"sub.example.github.io": "example.github.io", "1password.com": "1password.com", "a1.b2.vendor.io": "vendor.io",
	} {
		if got, err := webSessionSite(in); err != nil || got != want {
			t.Errorf("webSessionSite(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
}

// TestWebTaskStartURLStaysOnTheSite: a site session's start_url off
// the site is refused at dispatch.
func TestWebTaskStartURLStaysOnTheSite(t *testing.T) {
	svc, _, _, _ := webService(t, &fakeSubagentDriver{})
	_, err := svc.StartBatch(context.Background(), "alice", webArgs(t, map[string]any{"session": "vendor.com", "start_url": "https://other.example.com/"}))
	if err == nil || !strings.Contains(err.Error(), "off the session's site, vendor.com") {
		t.Errorf("start_url off the site: %v", err)
	}
	if len(svc.listJobs("alice")) != 0 {
		t.Error("a refused dispatch registered a job")
	}
}

// TestWebCommandKeepsASiteSessionOnItsSite: core refuses a navigation
// or a new tab off the session's site, and a tab opened at a URL that
// is not http or https, before anything reaches the pod; the site and
// its subdomains are relayed.
func TestWebCommandKeepsASiteSessionOnItsSite(t *testing.T) {
	svc, _, _, prov := webService(t, &fakeSubagentDriver{})
	schema, _ := structured.Parse([]byte(quoteSchema))
	svc.registerJob(&subagentJob{ID: "s1", Parent: "alice", Kind: agentpod.SubagentKindWeb, Session: "vendor.com", Site: "vendor.com", State: subagentJobRunning, Depth: 1, Schema: schema, done: make(chan struct{})})
	svc.registerJob(&subagentJob{ID: "o1", Parent: "alice", Kind: agentpod.SubagentKindWeb, Session: agentpod.OpenWebSession, State: subagentJobRunning, Depth: 1, Schema: schema, done: make(chan struct{})})
	ctx := context.Background()
	refusals := []struct {
		name, id string
		req      agentpod.WebCommandRequest
		want     string
	}{
		{"navigate off site", "s1", agentpod.WebCommandRequest{Command: "navigate", Args: json.RawMessage(`{"url":"https://evil.example/"}`)}, "off this session's site, vendor.com"},
		{"navigate to a lookalike", "s1", agentpod.WebCommandRequest{Command: "navigate", Args: json.RawMessage(`{"url":"https://vendor.com.evil.example/"}`)}, "off this session's site"},
		{"navigate to a suffix match", "s1", agentpod.WebCommandRequest{Command: "navigate", Args: json.RawMessage(`{"url":"https://notvendor.com/"}`)}, "off this session's site"},
		{"open a tab off site", "s1", agentpod.WebCommandRequest{Command: "tabs", Args: json.RawMessage(`{"action":"open","url":"https://evil.example/"}`)}, "off this session's site"},
		{"open a tab at a file URL", "o1", agentpod.WebCommandRequest{Command: "tabs", Args: json.RawMessage(`{"action":"open","url":"file:///etc/passwd"}`)}, "http or https"},
		{"open a tab at a javascript URL", "s1", agentpod.WebCommandRequest{Command: "tabs", Args: json.RawMessage(`{"action":"open","url":"javascript:alert(1)"}`)}, "http or https"},
	}
	for _, tc := range refusals {
		t.Run(tc.name, func(t *testing.T) {
			resp := svc.WebCommand(ctx, "alice", tc.id, tc.req)
			if !resp.IsError || !strings.Contains(resp.Text, tc.want) {
				t.Errorf("response = %+v, want a refusal naming %q", resp, tc.want)
			}
		})
	}
	if len(prov.started) != 0 {
		t.Fatal("a refused command started a browser")
	}

	// On the site, and a subdomain of it, the command is relayed.
	got := make(chan agentpod.WebCommandResponse, 1)
	go func() {
		got <- svc.WebCommand(ctx, "alice", "s1", agentpod.WebCommandRequest{Command: "tabs", Args: json.RawMessage(`{"action":"open","url":"https://Shop.vendor.com./cart"}`)})
	}()
	token := <-prov.startedC
	link := newFakeLink()
	b := svc.Browsers.claim("alice", "vendor.com", token, link)
	if b == nil {
		t.Fatal("claim found no waiting browser")
	}
	if err := svc.Browsers.attach(b); err != nil {
		t.Fatal(err)
	}
	svc.Browsers.onMessage(b, browserpod.Message{Type: browserpod.TypeState, State: browserpod.StateRunning}, nil)
	cmd := link.next(t, browserpod.TypeCommand)
	if cmd.msg.Name != browserpod.CommandTabs {
		t.Fatalf("relayed command = %+v", cmd.msg)
	}
	svc.Browsers.onMessage(b, browserpod.Message{Type: browserpod.TypeResult, ID: cmd.msg.ID, OK: true, Text: "Opened t2"}, nil)
	if resp := <-got; resp.IsError || resp.Text != "Opened t2" {
		t.Errorf("relayed response = %+v", resp)
	}
	go func() {
		got <- svc.WebCommand(ctx, "alice", "s1", agentpod.WebCommandRequest{Command: "navigate", Args: json.RawMessage(`{"url":"http://vendor.com/"}`)})
	}()
	cmd = link.next(t, browserpod.TypeCommand)
	svc.Browsers.onMessage(b, browserpod.Message{Type: browserpod.TypeResult, ID: cmd.msg.ID, OK: true, Text: "Loaded"}, nil)
	if resp := <-got; resp.IsError {
		t.Errorf("navigate on the site = %+v", resp)
	}
}
