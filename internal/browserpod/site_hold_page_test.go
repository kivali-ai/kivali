package browserpod

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// pageAt is the target and session of the fake page at u.
func (f *fakeCDP) pageAt(u string) (target, session string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for s, t := range f.sessions {
		if f.urls[t] == u {
			return t, s
		}
	}
	return "", ""
}

// heldPod is a pod whose group "up" is held to vendor.com with a tab on
// it, and a sender of the group's commands as core sends them.
func heldPod(t *testing.T) (*podHarness, func(name string, args any) Message, string, string) {
	t.Helper()
	h := startPod(t, podOptions{ephemeral: true})
	n := 0
	held := func(name string, args any) Message {
		t.Helper()
		n++
		id := fmt.Sprintf("hold-%d", n)
		raw, err := json.Marshal(args)
		if err != nil {
			t.Fatal(err)
		}
		h.core.send(Message{Type: TypeCommand, ID: id, Group: "up", Name: name, Args: raw, Site: "vendor.com"})
		return h.core.result(id)
	}
	if res := held(CommandNavigate, NavigateArgs{URL: "https://upload.vendor.com/new"}); !res.OK {
		t.Fatalf("on-site navigate: %+v", res)
	}
	target, session := h.cdp.pageAt("https://upload.vendor.com/new")
	if target == "" {
		t.Fatal("no page for the held tab")
	}
	return h, held, target, session
}

// TestHeldGroupStopsThePagesOwnNavigationsOffSite: a held tab's page
// that starts to leave the site is stopped, and one that got there is
// sent back to a blank page, each with a note the model reads.
func TestHeldGroupStopsThePagesOwnNavigationsOffSite(t *testing.T) {
	h, held, target, session := heldPod(t)
	h.cdp.emit(session, "Page.frameRequestedNavigation", map[string]any{"frameId": target, "url": "https://evil.test/collect?d=secret", "disposition": "currentTab"})
	res := held(CommandNavigate, NavigateArgs{URL: "https://vendor.com/a"})
	if !strings.Contains(res.Text, "The page tried to go to https://evil.test/collect?d=secret, which is off the site this task is held to, and was stopped.") {
		t.Errorf("after a requested navigation: %+v", res)
	}
	if len(h.cdp.called("Page.stopLoading")) == 0 {
		t.Error("the off-site navigation was not stopped")
	}
	target, session = h.cdp.pageAt("https://vendor.com/a")
	h.cdp.emit(session, "Page.frameNavigated", map[string]any{"frame": map[string]any{"id": target, "loaderId": "Lx", "url": "https://evil.test/landed"}})
	res = held(CommandNavigate, NavigateArgs{URL: "https://vendor.com/b"})
	if !strings.Contains(res.Text, "The page went to https://evil.test/landed, which is off the site this task is held to; the tab was sent to about:blank.") {
		t.Errorf("after a committed navigation: %+v", res)
	}
}

// TestHeldGroupClosesAnOffSitePopup: a popup an on-site page opens off
// the site is closed and never becomes a tab of the task; one on the
// site is adopted.
func TestHeldGroupClosesAnOffSitePopup(t *testing.T) {
	h, held, target, _ := heldPod(t)
	h.cdp.emit("", "Target.attachedToTarget", map[string]any{
		"sessionId": "SP1", "targetInfo": map[string]any{"targetId": "TP1", "type": "page", "url": "https://evil.test/upload", "openerId": target},
	})
	res := held(CommandNavigate, NavigateArgs{URL: "https://upload.vendor.com/new"})
	if !strings.Contains(res.Text, "The page opened a new tab at https://evil.test/upload, which is off the site this task is held to; it was closed.") {
		t.Errorf("no note of the closed popup: %+v", res)
	}
	closed := false
	for _, c := range h.cdp.called("Target.closeTarget") {
		closed = closed || strings.Contains(c, `"TP1"`)
	}
	if !closed {
		t.Errorf("the off-site popup was not closed: %v", h.cdp.called("Target.closeTarget"))
	}
	if list := held(CommandTabs, TabsArgs{Action: TabsList}); strings.Contains(list.Text, "evil.test") {
		t.Errorf("the off-site popup became a tab: %s", list.Text)
	}
	target, _ = h.cdp.pageAt("https://upload.vendor.com/new")
	h.cdp.emit("", "Target.attachedToTarget", map[string]any{
		"sessionId": "SP2", "targetInfo": map[string]any{"targetId": "TP2", "type": "page", "url": "https://help.vendor.com/", "openerId": target},
	})
	if tab := h.core.next("the on-site popup", func(m Message) bool { return m.Type == TypeTab && m.Opener != "" }); tab.Group != "up" || tab.URL != "https://help.vendor.com/" {
		t.Errorf("on-site popup = %+v", tab.Message)
	}
}

// TestHeldGroupKeepsInputOutOfOffSiteFrames: while the focus is inside
// an embedded frame from another site, a held task's key presses are
// refused and nothing reaches the page. The pod fails no frame's
// request: frames load as pages ask.
func TestHeldGroupKeepsInputOutOfOffSiteFrames(t *testing.T) {
	h, held, _, _ := heldPod(t)
	h.cdp.setOverride(func(_ context.Context, _, method string, params json.RawMessage) (any, bool, error) {
		if method == "Runtime.evaluate" && strings.Contains(string(params), "kivali-frame") {
			return map[string]any{"result": map[string]any{"type": "string", "value": "cross:https://evil.test/frame"}}, true, nil
		}
		return nil, false, nil
	})
	before := len(h.cdp.called("Input.dispatchKeyEvent"))
	res := held(CommandPress, PressArgs{Key: "a"})
	if res.Code != CodeOffSite || !strings.Contains(res.Error, "embedded frame from another site (https://evil.test/frame)") {
		t.Fatalf("a key into an off-site frame: %+v", res)
	}
	if after := len(h.cdp.called("Input.dispatchKeyEvent")); after != before {
		t.Errorf("%d key events reached the page", after-before)
	}
	if n := len(h.cdp.called("Fetch.failRequest")); n != 0 {
		t.Errorf("the pod failed %d requests", n)
	}
	// A held tab pauses its own document requests, as a site
	// session's do, so one off the site fails before it is sent.
	if len(h.cdp.called("Fetch.enable")) == 0 {
		t.Error("a held tab does not pause its document requests")
	}
}

// TestUploadGoesOnlyOntoAWebPageOfTheSite: the pod sets a file input
// only in a tab at a web page, and in a held group one on its site,
// whichever tab the command names.
func TestUploadGoesOnlyOntoAWebPageOfTheSite(t *testing.T) {
	h, held, _, _ := heldPod(t)
	h.core.sendBytes(Message{Type: TypeFile, Group: "up", FileName: "quote.pdf"}, []byte("pdf"))
	h.core.sendBytes(Message{Type: TypeFile, Group: "free", FileName: "quote.pdf"}, []byte("pdf"))
	if res := h.core.command("free", "", CommandTabs, TabsArgs{Action: TabsOpen}); !res.OK {
		t.Fatalf("a blank tab: %+v", res)
	}
	if res := h.core.command("free", "", CommandUpload, UploadArgs{Ref: "e1", File: "quote.pdf"}); res.Code != CodeOffSite || !strings.Contains(res.Error, "this tab is at about:blank") {
		t.Errorf("upload into a blank tab: %+v", res)
	}
	// On the site the check passes, and the upload goes on to find its
	// ref (there is no snapshot here).
	if res := held(CommandUpload, UploadArgs{Ref: "e1", File: "quote.pdf"}); res.Code != CodeNotFound {
		t.Errorf("upload on the site: %+v", res)
	}
}

// TestFrameRefusal: where an action would land decides whether a held
// group may take it.
func TestFrameRefusal(t *testing.T) {
	b := newBrowser(nil, nil, log.New(io.Discard, "", 0), "", t.TempDir(), func(Message, io.Reader) error { return nil })
	b.holdGroup("up", "vendor.com")
	for _, tc := range []struct {
		where string
		ok    bool
	}{
		{"", true},
		{"https://forms.vendor.com/embed", true},
		{"about:srcdoc", true},
		{"https://evil.test/frame", false},
		{"cross:https://forms.vendor.com/embed", false},
		{"cross:", false},
	} {
		if err := b.frameRefusal("up", tc.where, "the click"); (err == nil) != tc.ok {
			t.Errorf("frameRefusal(%q) = %v, want ok %v", tc.where, err, tc.ok)
		}
	}
	// An unheld group is never checked: it has no site to keep to.
	if b.heldSite("free") != "" {
		t.Error("an unheld group has a site")
	}
}

// TestUploadIgnoresASignInUnlock: a person's unlock of a site
// session's group lets the person's tabs leave the site, but a file
// still goes only onto a page of the site.
func TestUploadIgnoresASignInUnlock(t *testing.T) {
	dir := t.TempDir()
	b := newBrowser(nil, nil, log.New(io.Discard, "", 0), "vendor.com", dir, func(Message, io.Reader) error { return nil })
	b.mu.Lock()
	b.groupLocked("g").unlocked = true
	b.mu.Unlock()
	if !b.allowed("g", "https://idp.example.org/login") {
		t.Fatal("the unlocked group does not allow an off-site page; the test would prove nothing")
	}
	if err := os.MkdirAll(filepath.Join(dir, "files", "g"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "files", "g", "quote.pdf"), []byte("pdf"), 0o644); err != nil {
		t.Fatal(err)
	}
	tb := &tab{group: "g", url: "https://idp.example.org/login"}
	_, err := b.cmdUpload(context.Background(), tb, "g", UploadArgs{Ref: "e1", File: "quote.pdf"})
	var ce *cmdError
	if !errors.As(err, &ce) || ce.code != CodeOffSite {
		t.Fatalf("upload into an off-site tab of an unlocked group: %v, want %s", err, CodeOffSite)
	}
}
