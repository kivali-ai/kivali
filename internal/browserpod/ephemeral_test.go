package browserpod

import (
	"slices"
	"testing"
)

// TestEphemeralSessionIsolatesGroups: in an ephemeral session each
// group's tabs are created in a browser context of the group's own, so
// two tasks share no cookies or storage, and closing a group disposes
// of its context and no other.
func TestEphemeralSessionIsolatesGroups(t *testing.T) {
	h := startPod(t, podOptions{ephemeral: true})
	if res := h.core.command("a", "", CommandNavigate, NavigateArgs{URL: "https://a.test/"}); !res.OK {
		t.Fatalf("a: %+v", res)
	}
	if res := h.core.command("b", "", CommandNavigate, NavigateArgs{URL: "https://b.test/"}); !res.OK {
		t.Fatalf("b: %+v", res)
	}
	// One target each, in contexts made in that order.
	h.cdp.mu.Lock()
	var ctxs []string
	for _, c := range h.cdp.contexts {
		ctxs = append(ctxs, c)
	}
	h.cdp.mu.Unlock()
	slices.Sort(ctxs)
	if len(ctxs) != 2 || ctxs[0] == "" || ctxs[0] == ctxs[1] {
		t.Fatalf("contexts of the targets = %v, want two distinct ones", ctxs)
	}
	ctxA, ctxB := ctxs[0], ctxs[1]

	h.core.send(Message{Type: TypeCloseGroup, Group: "a"})
	// The close runs on a's queue; a command behind it sees it done.
	if res := h.core.command("a", "", CommandTabs, TabsArgs{Action: TabsList}); res.Text != "This task has no tab open." {
		t.Fatalf("a after close: %q", res.Text)
	}
	h.cdp.mu.Lock()
	disposed := slices.Clone(h.cdp.disposed)
	h.cdp.mu.Unlock()
	if !slices.Contains(disposed, ctxA) || slices.Contains(disposed, ctxB) {
		t.Errorf("disposed = %v, want a's context %q and not b's %q", disposed, ctxA, ctxB)
	}
	if res := h.core.command("b", "t2", CommandTabs, TabsArgs{Action: TabsList}); !res.OK || res.Text == "This task has no tab open." {
		t.Errorf("b lost its tab: %+v", res)
	}
}

// TestSharedSessionUsesTheDefaultContext: without Ephemeral every group
// shares the profile's default context, as a signed-in site session
// must.
func TestSharedSessionUsesTheDefaultContext(t *testing.T) {
	h := startPod(t, podOptions{})
	if res := h.core.command("a", "", CommandNavigate, NavigateArgs{URL: "https://a.test/"}); !res.OK {
		t.Fatalf("a: %+v", res)
	}
	h.cdp.mu.Lock()
	made := h.cdp.nextC
	var inContext []string
	for target, c := range h.cdp.contexts {
		if c != "" {
			inContext = append(inContext, target)
		}
	}
	h.cdp.mu.Unlock()
	if made != 0 || len(inContext) != 0 {
		t.Errorf("a shared session made %d browser context(s); targets in one: %v", made, inContext)
	}
}
