package browserpod

import (
	"strings"
	"testing"
)

var (
	themeCookie = map[string]any{"name": "theme", "value": "dark", "domain": "vendor.example", "path": "/", "expires": -1, "session": true, "size": 9}
	sidCookie   = map[string]any{"name": "sid", "value": "abc", "domain": ".vendor.example", "path": "/", "expires": 1893456000, "secure": true, "httpOnly": true, "sameSite": "Lax", "priority": "Medium", "sourceScheme": "Secure", "sourcePort": 443}
	mfaCookie   = map[string]any{"name": "mfa", "value": "ok", "domain": "auth.vendor.example", "path": "/", "expires": -1}
)

// cookieFrames is how many cookie snapshots core has received without
// asking for them by type: those an expectType for another type held.
func cookieFrames(h *podHarness) int {
	h.core.mu.Lock()
	defer h.core.mu.Unlock()
	n := 0
	for _, r := range h.core.held {
		if r.Type == TypeCookies {
			n++
		}
	}
	return n
}

// TestCookieSnapshotsFollowChanges: a site session's cookies go to
// core when they have changed, at the interval or at once when core
// asks, in the shape Storage.setCookies takes; a jar that has not
// changed sends nothing.
func TestCookieSnapshotsFollowChanges(t *testing.T) {
	h := startPod(t, podOptions{site: "vendor.example", jar: []map[string]any{themeCookie}})
	h.clk.BlockUntil(1) // the snapshot ticker is armed
	// The jar the browser started with is the baseline.
	h.clk.Advance(snapshotEvery)
	// A sign-in sets a cookie: the next look sends the jar.
	h.cdp.setJar(themeCookie, sidCookie)
	h.clk.Advance(snapshotEvery)
	got := h.core.expectType(TypeCookies)
	body := string(got.data)
	if got.Bytes != int64(len(got.data)) || !strings.Contains(body, `"name":"sid"`) {
		t.Fatalf("first snapshot = %d bytes %q, want the jar with sid", got.Bytes, body)
	}
	// The snapshot is what setCookies takes: a session cookie carries no
	// expiry, getCookies' own fields are gone, and the order is fixed.
	if strings.Contains(body, `-1`) || strings.Contains(body, `"size"`) || strings.Contains(body, `"session"`) {
		t.Errorf("snapshot carries getCookies-only fields: %s", body)
	}
	if !strings.Contains(body, `"expires":1893456000`) || !strings.Contains(body, `"httpOnly":true`) || !strings.Contains(body, `"sourcePort":443`) {
		t.Errorf("snapshot lost cookie attributes: %s", body)
	}
	if strings.Index(body, `"name":"sid"`) > strings.Index(body, `"name":"theme"`) {
		t.Errorf("snapshot not in domain order: %s", body)
	}
	// Core asks after a window closed: unchanged, nothing; changed, at once.
	h.core.send(Message{Type: TypeSnapshot})
	h.cdp.setJar(themeCookie, sidCookie, mfaCookie)
	h.core.send(Message{Type: TypeSnapshot})
	got = h.core.expectType(TypeCookies)
	if !strings.Contains(string(got.data), `"name":"mfa"`) {
		t.Fatalf("snapshot on request = %q, want the jar with mfa", got.data)
	}
	if n := cookieFrames(h); n != 0 {
		t.Errorf("%d snapshot(s) sent for an unchanged jar", n)
	}
}

// TestCookiesFromCoreAreSetBeforeRunning: the snapshot core sends
// before start is set on Chromium before the pod reports running.
func TestCookiesFromCoreAreSetBeforeRunning(t *testing.T) {
	jar := []byte(`[{"name":"sid","value":"abc","domain":".vendor.example","path":"/","secure":true,"httpOnly":true}]`)
	h := startPod(t, podOptions{site: "vendor.example", cookies: jar})
	calls := h.cdp.called("Storage.setCookies")
	if len(calls) != 1 || !strings.Contains(calls[0], `"name":"sid"`) || !strings.Contains(calls[0], `"httpOnly":true`) {
		t.Fatalf("Storage.setCookies calls = %q, want one with the snapshot", calls)
	}
	// Nothing to set is no call.
	h2 := startPod(t, podOptions{site: "vendor.example", cookies: []byte(`[]`)})
	if calls := h2.cdp.called("Storage.setCookies"); len(calls) != 0 {
		t.Errorf("Storage.setCookies called for an empty snapshot: %q", calls)
	}
}

// TestOpenSessionTakesNoCookieSnapshots: the open session keeps
// nothing, so its browser never looks at its cookies.
func TestOpenSessionTakesNoCookieSnapshots(t *testing.T) {
	h := startPod(t, podOptions{ephemeral: true, jar: []map[string]any{sidCookie}})
	h.clk.Advance(snapshotEvery)
	h.core.send(Message{Type: TypeSnapshot})
	if res := h.core.command("g1", "", CommandNavigate, NavigateArgs{URL: "https://example.com/"}); !res.OK {
		t.Fatalf("navigate: %+v", res)
	}
	if calls := h.cdp.called("Storage.getCookies"); len(calls) != 0 {
		t.Errorf("Storage.getCookies called in the open session: %q", calls)
	}
	if n := cookieFrames(h); n != 0 {
		t.Errorf("%d snapshot(s) sent from the open session", n)
	}
}
