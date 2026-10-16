package browserpod

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// screencastCalls hears every screencast call the pod makes, by method
// and session, through the fake's override hook.
type screencastCalls struct {
	c chan string
}

func watchScreencastCalls(h *podHarness) *screencastCalls {
	sc := &screencastCalls{c: make(chan string, 64)}
	h.cdp.mu.Lock()
	h.cdp.override = func(_ context.Context, session, method string, _ json.RawMessage) (any, bool, error) {
		if method == "Page.startScreencast" || method == "Page.stopScreencast" {
			sc.c <- method + " " + session
		}
		return nil, false, nil
	}
	h.cdp.mu.Unlock()
	return sc
}

func (sc *screencastCalls) next(t *testing.T) string {
	t.Helper()
	select {
	case s := <-sc.c:
		return s
	case <-time.After(failsafe):
		t.Fatal("no screencast call")
		return ""
	}
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
