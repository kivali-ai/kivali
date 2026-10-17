package browserpod

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// recordedCalls hears the calls the pod makes with the named methods,
// as "method session", through the fake's override hook. hang names
// methods the fake never answers, so a command stays in flight.
type recordedCalls struct {
	c    chan string
	hang map[string]bool
}

func recordCalls(h *podHarness, methods ...string) *recordedCalls {
	rc := &recordedCalls{c: make(chan string, 64), hang: map[string]bool{}}
	want := map[string]bool{}
	for _, m := range methods {
		want[m] = true
	}
	h.cdp.mu.Lock()
	h.cdp.override = func(ctx context.Context, session, method string, _ json.RawMessage) (any, bool, error) {
		if want[method] {
			rc.c <- method + " " + session
		}
		h.cdp.mu.Lock()
		hang := rc.hang[method]
		h.cdp.mu.Unlock()
		if hang {
			<-ctx.Done()
			return nil, true, ctx.Err()
		}
		return nil, false, nil
	}
	h.cdp.mu.Unlock()
	return rc
}

func watchScreencastCalls(h *podHarness) *recordedCalls {
	return recordCalls(h, "Page.startScreencast", "Page.stopScreencast")
}

func (rc *recordedCalls) hangOn(h *podHarness, method string, on bool) {
	h.cdp.mu.Lock()
	rc.hang[method] = on
	h.cdp.mu.Unlock()
}

func (rc *recordedCalls) next(t *testing.T) string {
	t.Helper()
	select {
	case s := <-rc.c:
		return s
	case <-time.After(failsafe):
		t.Fatal("no recorded call")
		return ""
	}
}

// sessionAt is the fake's session for the target at url.
func sessionAt(h *podHarness, url string) string {
	h.cdp.mu.Lock()
	var target string
	for tg, u := range h.cdp.urls {
		if u == url {
			target = tg
		}
	}
	h.cdp.mu.Unlock()
	return sessionOf(h, target)
}

// sessionOf is the fake's session for a target.
func sessionOf(h *podHarness, target string) string {
	h.cdp.mu.Lock()
	defer h.cdp.mu.Unlock()
	for s, tg := range h.cdp.sessions {
		if tg == target {
			return s
		}
	}
	return ""
}

// TestGroupScreencastFollowsTheCurrentTab: a group's screencast is on
// its current tab, a frame reaches core as bytes after its message, the
// cast moves to a tab the group opens, it goes off on request, and one
// tab can still be cast on its own.
func TestGroupScreencastFollowsTheCurrentTab(t *testing.T) {
	h := startPod(t, podOptions{})
	sc := watchScreencastCalls(h)
	if res := h.core.command("a", "", CommandNavigate, NavigateArgs{URL: "https://a.test/"}); !res.OK {
		t.Fatalf("navigate: %+v", res)
	}
	s1 := sessionOf(h, "T1")
	h.core.send(Message{Type: TypeScreencast, Group: "a", OK: true})
	if got := sc.next(t); got != "Page.startScreencast "+s1 {
		t.Fatalf("after watch: %q", got)
	}

	jpeg := []byte("jpeg-bytes")
	h.cdp.emit(s1, "Page.screencastFrame", map[string]any{"data": base64.StdEncoding.EncodeToString(jpeg), "sessionId": 7, "metadata": map[string]any{}})
	fr := h.core.expectType(TypeFrame)
	if fr.Group != "a" || fr.Tab != "t1" || fr.Bytes != int64(len(jpeg)) || string(fr.data) != string(jpeg) {
		t.Fatalf("frame = %+v with %q", fr.Message, fr.data)
	}

	// A new tab becomes the group's current tab; the cast follows.
	if res := h.core.command("a", "", CommandTabs, TabsArgs{Action: TabsOpen, URL: "https://a.test/2"}); !res.OK {
		t.Fatalf("open: %+v", res)
	}
	stop, start := sc.next(t), sc.next(t)
	if stop != "Page.stopScreencast "+s1 || !strings.HasPrefix(start, "Page.startScreencast ") || strings.HasSuffix(start, " "+s1) {
		t.Fatalf("after a new tab: %q then %q", stop, start)
	}

	h.core.send(Message{Type: TypeScreencast, Group: "a", OK: false})
	if got := sc.next(t); !strings.HasPrefix(got, "Page.stopScreencast ") {
		t.Fatalf("after unwatch: %q", got)
	}

	// One tab on its own, as before.
	h.core.send(Message{Type: TypeScreencast, Tab: "t1", OK: true})
	if got := sc.next(t); got != "Page.startScreencast "+s1 {
		t.Fatalf("single tab: %q", got)
	}
}

// TestWatchedTabIsKeptInFront: a watched group's tab is brought to the
// front when the watch starts, back to the front after another group's
// command, before each input event from the person's window even while
// another group's command runs, and when Chromium reports it hidden.
func TestWatchedTabIsKeptInFront(t *testing.T) {
	h := startPod(t, podOptions{})
	if res := h.core.command("a", "", CommandNavigate, NavigateArgs{URL: "https://a.test/"}); !res.OK {
		t.Fatalf("a: %+v", res)
	}
	if res := h.core.command("b", "", CommandNavigate, NavigateArgs{URL: "https://b.test/"}); !res.OK {
		t.Fatalf("b: %+v", res)
	}
	s1, s2 := sessionAt(h, "https://a.test/"), sessionAt(h, "https://b.test/")
	if s1 == "" || s2 == "" || s1 == s2 {
		t.Fatalf("sessions: %q %q", s1, s2)
	}
	rc := recordCalls(h, "Page.bringToFront", "Input.dispatchMouseEvent", "Page.navigate")

	// b's navigate left b's tab in front; watching a fronts a's tab.
	h.core.send(Message{Type: TypeScreencast, Group: "a", OK: true})
	if got := rc.next(t); got != "Page.bringToFront "+s1 {
		t.Fatalf("after watch: %q", got)
	}

	// A command on b fronts b's tab for its duration, then a's comes
	// back.
	if res := h.core.command("b", "", CommandNavigate, NavigateArgs{URL: "https://b.test/2"}); !res.OK {
		t.Fatalf("b again: %+v", res)
	}
	for i, want := range []string{"Page.bringToFront " + s2, "Page.navigate " + s2, "Page.bringToFront " + s1} {
		if got := rc.next(t); got != want {
			t.Fatalf("call %d after b's command: %q, want %q", i, got, want)
		}
	}

	// While b's command is in flight, the person's input fronts a's
	// tab before it is dispatched.
	rc.hangOn(h, "Page.navigate", true)
	id := h.core.sendCommand("b", "", CommandNavigate, NavigateArgs{URL: "https://b.test/3"})
	for i, want := range []string{"Page.bringToFront " + s2, "Page.navigate " + s2} {
		if got := rc.next(t); got != want {
			t.Fatalf("call %d of b's hung command: %q, want %q", i, got, want)
		}
	}
	move, _ := json.Marshal(InputArgs{Kind: "mouse", Action: "moved", X: 10, Y: 20})
	h.core.send(Message{Type: TypeInput, Tab: "t1", Args: move})
	for i, want := range []string{"Page.bringToFront " + s1, "Input.dispatchMouseEvent " + s1} {
		if got := rc.next(t); got != want {
			t.Fatalf("call %d after input: %q, want %q", i, got, want)
		}
	}
	h.clk.BlockUntil(1)
	h.clk.Advance(navTimeout)
	if res := h.core.result(id); res.Code != CodeTimeout {
		t.Fatalf("hung navigate: %+v", res)
	}
	rc.hangOn(h, "Page.navigate", false)

	// Chromium says a's tab is hidden (a page opened a tab, say): it is
	// brought back. A command on a itself fronts nothing else.
	h.cdp.emit(s1, "Page.screencastVisibilityChanged", map[string]any{"visible": false})
	if got := rc.next(t); got != "Page.bringToFront "+s1 {
		t.Fatalf("after hidden: %q", got)
	}
	if res := h.core.command("a", "", CommandNavigate, NavigateArgs{URL: "https://a.test/2"}); !res.OK {
		t.Fatalf("a again: %+v", res)
	}
	if got := rc.next(t); got != "Page.navigate "+s1 {
		t.Fatalf("after a's own command: %q", got)
	}
	select {
	case got := <-rc.c:
		t.Fatalf("an extra call after a's own command: %q", got)
	default:
	}
}
