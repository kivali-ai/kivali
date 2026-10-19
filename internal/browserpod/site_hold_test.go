package browserpod

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"strings"
	"testing"
)

// TestGroupHeldToASiteRefusesOffSiteNavigation: in a browser whose
// session is not locked, a group core holds to a site (a task that
// uploads files) may not leave it, by web_navigate or web_tabs open,
// while another group goes anywhere; the first site a group is given
// is kept.
func TestGroupHeldToASiteRefusesOffSiteNavigation(t *testing.T) {
	h := startPod(t, podOptions{ephemeral: true})
	n := 0
	held := func(name, site string, args any) Message {
		t.Helper()
		n++
		id := fmt.Sprintf("held-%d", n)
		raw, err := json.Marshal(args)
		if err != nil {
			t.Fatal(err)
		}
		h.core.send(Message{Type: TypeCommand, ID: id, Group: "up", Name: name, Args: raw, Site: site})
		return h.core.result(id)
	}
	if res := held(CommandNavigate, "Vendor.com", NavigateArgs{URL: "https://evil.test/collect"}); res.Code != CodeOffSite || !strings.Contains(res.Error, "off the site this task is held to") {
		t.Fatalf("off-site navigate: %+v", res)
	}
	if res := held(CommandNavigate, "vendor.com", NavigateArgs{URL: "https://upload.vendor.com/new"}); !res.OK {
		t.Fatalf("on-site navigate: %+v", res)
	}
	if res := held(CommandTabs, "vendor.com", TabsArgs{Action: TabsOpen, URL: "https://evil.test/"}); res.Code != CodeOffSite {
		t.Fatalf("off-site tab: %+v", res)
	}
	// A later command naming another site, or none, does not move the hold.
	if res := held(CommandNavigate, "evil.test", NavigateArgs{URL: "https://evil.test/"}); res.Code != CodeOffSite {
		t.Fatalf("navigate naming another site: %+v", res)
	}
	if res := held(CommandNavigate, "", NavigateArgs{URL: "https://evil.test/"}); res.Code != CodeOffSite {
		t.Fatalf("navigate naming no site: %+v", res)
	}
	if res := h.core.command("free", "", CommandNavigate, NavigateArgs{URL: "https://evil.test/"}); !res.OK {
		t.Fatalf("another group: %+v", res)
	}
	for _, c := range h.cdp.called("Page.navigate") {
		if strings.Contains(c, "evil.test/collect") {
			t.Fatalf("navigated off-site: %s", c)
		}
	}
}

// TestHeldGroupAllowsOnlyItsSite: what a page of a held group may load,
// the check the page's own navigations go through too.
func TestHeldGroupAllowsOnlyItsSite(t *testing.T) {
	b := newBrowser(nil, nil, log.New(io.Discard, "", 0), "", t.TempDir(), func(Message, io.Reader) error { return nil })
	b.holdGroup("up", "vendor.com")
	b.holdGroup("up", "other.example")
	for _, tc := range []struct {
		group, url string
		want       bool
	}{
		{"up", "https://vendor.com/a", true},
		{"up", "https://cdn.vendor.com/a", true},
		{"up", "https://other.example/", false},
		{"up", "https://notvendor.com/", false},
		{"up", "about:blank", true},
		{"free", "https://other.example/", true},
	} {
		if got := b.allowed(tc.group, tc.url); got != tc.want {
			t.Errorf("allowed(%s, %s) = %v, want %v", tc.group, tc.url, got, tc.want)
		}
	}
}
