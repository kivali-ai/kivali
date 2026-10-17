package browserpod

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
)

func TestWireBytesFollowTheirMessage(t *testing.T) {
	conns := make(chan *websocket.Conn, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		conns <- c
	}))
	defer srv.Close()
	client, _, err := websocket.Dial(context.Background(), srv.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = client.CloseNow() }()
	server := <-conns
	defer func() { _ = server.CloseNow() }()

	ctx := context.Background()
	sender, reader := newWire(client), newWire(server)

	big := make([]byte, 3*writeChunk+17)
	for i := range big {
		big[i] = byte(i * 7)
	}
	go func() {
		_ = sender.sendBytes(ctx, Message{Type: TypeDownload, FileName: "a.bin"}, big)
		// Bytes split into many small frames, as another sender may.
		_ = sender.c.Write(ctx, websocket.MessageText, []byte(`{"type":"file","file_name":"b.txt","bytes":10}`))
		for _, part := range []string{"012", "3456", "789"} {
			_ = sender.c.Write(ctx, websocket.MessageBinary, []byte(part))
		}
		_ = sender.writeMessage(ctx, Message{Type: TypeSaved}, nil)
	}()

	msg, body, err := reader.readMessage(ctx)
	if err != nil || msg.Type != TypeDownload || msg.Bytes != int64(len(big)) {
		t.Fatalf("first message: %+v, %v", msg, err)
	}
	got, err := io.ReadAll(body)
	if err != nil || !bytes.Equal(got, big) {
		t.Fatalf("bytes: %d read, equal %v, err %v", len(got), bytes.Equal(got, big), err)
	}
	msg, body, err = reader.readMessage(ctx)
	if err != nil || msg.Type != TypeFile {
		t.Fatalf("second message: %+v, %v", msg, err)
	}
	got, _ = io.ReadAll(body)
	if string(got) != "0123456789" {
		t.Fatalf("split bytes: %q", got)
	}
	msg, _, err = reader.readMessage(ctx)
	if err != nil || msg.Type != TypeSaved {
		t.Fatalf("third message: %+v, %v", msg, err)
	}
}

// TestOverLongMessageIsRefusedNotFatal: a text frame over the limit (a
// paste no cap caught) is read to its end and refused; the session
// answers the next command as if it had not come.
func TestOverLongMessageIsRefusedNotFatal(t *testing.T) {
	h := startPod(t, podOptions{})
	if res := h.core.command("g", "", CommandNavigate, NavigateArgs{URL: "https://a.test/"}); !res.OK {
		t.Fatalf("navigate: %+v", res)
	}
	paste, _ := json.Marshal(InputArgs{Kind: "paste", Text: strings.Repeat("x", maxTextFrame)})
	raw, _ := json.Marshal(Message{Type: TypeInput, Tab: "t1", Args: paste})
	if len(raw) <= maxTextFrame {
		t.Fatalf("test frame is %d bytes, within the limit", len(raw))
	}
	if err := h.core.w.c.Write(context.Background(), websocket.MessageText, raw); err != nil {
		t.Fatal(err)
	}
	if res := h.core.command("g", "", CommandTabs, TabsArgs{Action: TabsList}); !res.OK || !strings.Contains(res.Text, "t1") {
		t.Fatalf("after the over-long frame: %+v", res)
	}
	if calls := h.cdp.called("Input.insertText"); len(calls) != 0 {
		t.Fatalf("the over-long paste reached the page: %d calls", len(calls))
	}
}

func TestSessionStartsAndAnswersCommands(t *testing.T) {
	// A saved profile from core lands in the profile directory.
	src := t.TempDir()
	if err := os.WriteFile(filepath.Join(src, "Cookies"), []byte("jar"), 0o600); err != nil {
		t.Fatal(err)
	}
	var packed bytes.Buffer
	if err := packProfile(src, &packed); err != nil {
		t.Fatal(err)
	}
	h := startPod(t, podOptions{profile: packed.Bytes()})
	h.fc.mu.Lock()
	path, token := h.fc.path, h.fc.token
	h.fc.mu.Unlock()
	if path != "/v1/browser/ada/s1/connect" || token != "tok" {
		t.Fatalf("connect: path %q token %q", path, token)
	}
	if b, err := os.ReadFile(filepath.Join(h.dir, "profile", "Cookies")); err != nil || string(b) != "jar" {
		t.Fatalf("profile not unpacked: %q %v", b, err)
	}

	res := h.core.command("g1", "", CommandNavigate, NavigateArgs{URL: "https://example.com/"})
	if !res.OK || res.Text != "Loaded Title of https://example.com/ — https://example.com/" {
		t.Fatalf("navigate: %+v", res)
	}
	tabMsg := h.core.expectType(TypeTab)
	if tabMsg.Group != "g1" || tabMsg.Tab != "t1" {
		t.Fatalf("tab: %+v", tabMsg.Message)
	}
	nav := h.core.expectType(TypeNavigated)
	if nav.Tab != "t1" || nav.URL != "https://example.com/" || nav.Title != "Title of https://example.com/" {
		t.Fatalf("navigated: %+v", nav.Message)
	}

	res = h.core.command("g1", "", CommandTabs, TabsArgs{Action: TabsList})
	if !res.OK || res.Text != "* t1 https://example.com/ — Title of https://example.com/" {
		t.Fatalf("tabs: %q", res.Text)
	}

	res = h.core.command("g1", "", CommandClick, map[string]any{})
	if res.OK || res.Code != CodeBadArgs || !strings.Contains(res.Error, "ref") {
		t.Fatalf("click without ref: %+v", res)
	}
	res = h.core.command("g1", "", CommandClick, ClickArgs{Ref: "e1"})
	if res.Code != CodeNotFound || !strings.Contains(res.Error, "snapshot") {
		t.Fatalf("click before a snapshot: %+v", res)
	}
	res = h.core.command("g1", "", "fly", nil)
	if res.Code != CodeBadArgs {
		t.Fatalf("unknown command: %+v", res)
	}
	res = h.core.command("g1", "", CommandNavigate, NavigateArgs{URL: "javascript:alert(1)"})
	if res.Code != CodeBadArgs {
		t.Fatalf("javascript URL: %+v", res)
	}

	h.core.send(Message{Type: TypeClose})
	if err := h.runResult(); err != nil {
		t.Fatalf("Run after close: %v", err)
	}
}

func TestCommandsSeeOnlyTheirGroup(t *testing.T) {
	h := startPod(t, podOptions{})
	if res := h.core.command("a", "", CommandNavigate, NavigateArgs{URL: "https://a.test/"}); !res.OK {
		t.Fatalf("a: %+v", res)
	}
	res := h.core.command("b", "t1", CommandSnapshot, nil)
	if res.OK || res.Code != CodeNotFound {
		t.Fatalf("b reaching a's tab: %+v", res)
	}
	res = h.core.command("b", "", CommandTabs, TabsArgs{Action: TabsSwitch, Tab: "t1"})
	if res.Code != CodeNotFound {
		t.Fatalf("b switching to a's tab: %+v", res)
	}
	res = h.core.command("b", "", CommandTabs, TabsArgs{Action: TabsList})
	if res.Text != "This task has no tab open." {
		t.Fatalf("b's list: %q", res.Text)
	}
	res = h.core.command("b", "", CommandTabs, TabsArgs{Action: TabsClose, Tab: "t1"})
	if res.Code != CodeNotFound {
		t.Fatalf("b closing a's tab: %+v", res)
	}
	if res := h.core.command("a", "t1", CommandTabs, TabsArgs{Action: TabsList}); !strings.Contains(res.Text, "t1") {
		t.Fatalf("a lost its tab: %q", res.Text)
	}
}

func TestTabLimitAcrossGroups(t *testing.T) {
	h := startPod(t, podOptions{})
	for i := 0; i < MaxTabsPerBrowser; i++ {
		group := fmt.Sprintf("g%d", i%3)
		if res := h.core.command(group, "", CommandTabs, TabsArgs{Action: TabsOpen}); !res.OK {
			t.Fatalf("tab %d: %+v", i+1, res)
		}
	}
	res := h.core.command("g9", "", CommandNavigate, NavigateArgs{URL: "https://x.test/"})
	if res.Code != CodeTabLimit {
		t.Fatalf("ninth tab: %+v", res)
	}
	// Closing one makes room.
	if res := h.core.command("g0", "", CommandTabs, TabsArgs{Action: TabsClose}); !res.OK {
		t.Fatalf("close: %+v", res)
	}
	h.core.expectType(TypeTabClosed)
	if res := h.core.command("g9", "", CommandNavigate, NavigateArgs{URL: "https://x.test/"}); !res.OK {
		t.Fatalf("after closing one: %+v", res)
	}
}

func TestSiteSessionRefusesOffSiteNavigation(t *testing.T) {
	h := startPod(t, podOptions{site: "example.com"})
	res := h.core.command("g", "", CommandNavigate, NavigateArgs{URL: "https://evil.test/login"})
	if res.Code != CodeOffSite || !strings.Contains(res.Error, "https://evil.test/login is off this session's site") {
		t.Fatalf("off-site: %+v", res)
	}
	if res := h.core.command("g", "", CommandNavigate, NavigateArgs{URL: "https://www.example.com/a"}); !res.OK {
		t.Fatalf("subdomain: %+v", res)
	}
	res = h.core.command("g", "", CommandNavigate, NavigateArgs{URL: "https://notexample.com/"})
	if res.Code != CodeOffSite {
		t.Fatalf("lookalike host: %+v", res)
	}
	for _, c := range h.cdp.called("Page.navigate") {
		if strings.Contains(c, "evil.test") || strings.Contains(c, "notexample") {
			t.Fatalf("navigated off-site: %s", c)
		}
	}
}

// TestSiteSessionFailsOffSiteDocumentRequests: in a site session every
// page pauses its document requests (Fetch); a main-frame request off
// the site, a redirect hop included, is failed and noted for the model,
// an on-site one and a subframe's go on. An open session pauses nothing.
func TestSiteSessionFailsOffSiteDocumentRequests(t *testing.T) {
	h := startPod(t, podOptions{site: "example.com"})
	if res := h.core.command("g", "", CommandNavigate, NavigateArgs{URL: "https://example.com/"}); !res.OK {
		t.Fatalf("navigate: %+v", res)
	}
	s1 := sessionOf(h, "T1")
	enabled := h.cdp.called("Fetch.enable")
	if len(enabled) != 1 || !strings.Contains(enabled[0], `"resourceType":"Document"`) || !strings.Contains(enabled[0], `"requestStage":"Request"`) {
		t.Fatalf("Fetch.enable calls: %q", enabled)
	}
	rc := recordCalls(h, "Fetch.failRequest", "Fetch.continueRequest")
	paused := func(id, frame, typ, url string) {
		h.cdp.emit(s1, "Fetch.requestPaused", map[string]any{
			"requestId": id, "frameId": frame, "resourceType": typ,
			"request": map[string]any{"url": url, "method": "GET"},
		})
	}
	// The first hop is on the site; the second is not.
	paused("R1", "T1", "Document", "https://example.com/bounce")
	if got := rc.next(t); got != "Fetch.continueRequest "+s1 {
		t.Fatalf("on-site hop: %q", got)
	}
	paused("R2", "T1", "Document", "https://evil.test/two")
	if got := rc.next(t); got != "Fetch.failRequest "+s1 {
		t.Fatalf("off-site hop: %q", got)
	}
	if calls := h.cdp.called("Fetch.failRequest"); len(calls) != 1 || !strings.Contains(calls[0], `"errorReason":"Aborted"`) || !strings.Contains(calls[0], `"requestId":"R2"`) {
		t.Fatalf("failRequest: %q", calls)
	}
	// A subframe's document is not the tab's navigation.
	paused("R3", "F1", "Document", "https://evil.test/frame")
	if got := rc.next(t); got != "Fetch.continueRequest "+s1 {
		t.Fatalf("subframe: %q", got)
	}
	// The next command on the tab carries the note.
	res := h.core.command("g", "", CommandBack, nil)
	if !strings.Contains(res.Text, "The page tried to go to https://evil.test/two, which is off this session's site, and was stopped.") || strings.Contains(res.Text, "evil.test/frame") {
		t.Fatalf("notes: %q", res.Text)
	}

	open := startPod(t, podOptions{})
	if res := open.core.command("g", "", CommandNavigate, NavigateArgs{URL: "https://example.com/"}); !res.OK {
		t.Fatalf("open navigate: %+v", res)
	}
	if calls := open.cdp.called("Fetch.enable"); len(calls) != 0 {
		t.Fatalf("an open session enabled Fetch: %q", calls)
	}
}

func TestCommandTimesOutOnTheClock(t *testing.T) {
	h := startPod(t, podOptions{})
	hung := make(chan struct{})
	h.cdp.setOverride(func(ctx context.Context, session, method string, params json.RawMessage) (any, bool, error) {
		if method == "Page.navigate" {
			// A page that never finishes loading.
			close(hung)
			return map[string]any{"frameId": "T1", "loaderId": "L-hang"}, true, nil
		}
		return nil, false, nil
	})
	id := h.core.sendCommand("g", "", CommandNavigate, NavigateArgs{URL: "https://slow.test/"})
	<-hung
	h.clk.BlockUntil(1)
	h.clk.Advance(navTimeout)
	res := h.core.result(id)
	if res.OK || res.Code != CodeTimeout || !strings.Contains(res.Error, "45 s") {
		t.Fatalf("navigate timeout: %+v", res)
	}
}

// abortingNavigate answers Page.navigate with net::ERR_ABORTED, as
// Chromium does for a download, a 204 or a superseded navigation.
func abortingNavigate(ctx context.Context, session, method string, params json.RawMessage) (any, bool, error) {
	if method == "Page.navigate" {
		return map[string]any{"frameId": "T1", "loaderId": "L-abort", "errorText": "net::ERR_ABORTED"}, true, nil
	}
	return nil, false, nil
}

func TestAbortedNavigationIsADownloadOnlyWhenOneBegins(t *testing.T) {
	h := startPod(t, podOptions{})
	if res := h.core.command("g", "", CommandNavigate, NavigateArgs{URL: "https://a.test/"}); !res.OK {
		t.Fatalf("navigate: %+v", res)
	}
	h.cdp.setOverride(abortingNavigate)
	// The pod's own ticker is armed already; each aborted navigation
	// arms the command timeout and then the download wait.
	armed := h.clk.Waiters() + 2
	// A download announces itself on the tab's frame: the answer says so.
	id := h.core.sendCommand("g", "", CommandNavigate, NavigateArgs{URL: "https://a.test/report.pdf"})
	h.clk.BlockUntil(armed)
	h.cdp.emit("", "Browser.downloadWillBegin", map[string]any{"frameId": "T1", "guid": "g1", "url": "https://a.test/report.pdf", "suggestedFilename": "report.pdf"})
	res := h.core.result(id)
	if !res.OK || res.Text != "https://a.test/report.pdf started a download instead of loading as a page; the tab is at https://a.test/ — Title of https://a.test/" {
		t.Fatalf("download: %+v", res)
	}
	// Nothing begins: after the wait the navigation was just aborted.
	id = h.core.sendCommand("g", "", CommandNavigate, NavigateArgs{URL: "https://a.test/204"})
	h.clk.BlockUntil(armed)
	h.clk.Advance(downloadWait)
	res = h.core.result(id)
	if !res.OK || !strings.HasPrefix(res.Text, "The navigation to https://a.test/204 was aborted before a page loaded") || strings.Contains(res.Text, "download") {
		t.Fatalf("aborted: %+v", res)
	}
	// A download on another tab's frame is not this navigation's.
	id = h.core.sendCommand("g", "", CommandNavigate, NavigateArgs{URL: "https://a.test/other"})
	h.clk.BlockUntil(armed)
	h.cdp.emit("", "Browser.downloadWillBegin", map[string]any{"frameId": "T9", "guid": "g2", "url": "https://a.test/x", "suggestedFilename": "x"})
	h.clk.Advance(downloadWait)
	res = h.core.result(id)
	if !res.OK || !strings.Contains(res.Text, "was aborted before a page loaded") {
		t.Fatalf("another tab's download: %+v", res)
	}
}

func TestWaitRunsOutOnTheClock(t *testing.T) {
	h := startPod(t, podOptions{})
	if res := h.core.command("g", "", CommandNavigate, NavigateArgs{URL: "https://a.test/"}); !res.OK {
		t.Fatalf("navigate: %+v", res)
	}
	id := h.core.sendCommand("g", "", CommandWait, WaitArgs{For: WaitText, Value: "Welcome", TimeoutMS: 5000})
	// The command timeout, the wait's own timer and its poll ticker.
	h.clk.BlockUntil(3)
	h.clk.Advance(5 * time.Second)
	res := h.core.result(id)
	if res.Code != CodeTimeout || !strings.Contains(res.Error, "https://a.test/") {
		t.Fatalf("wait: %+v", res)
	}
}

func TestOneGroupWaitingDoesNotBlockAnother(t *testing.T) {
	h := startPod(t, podOptions{})
	if res := h.core.command("slow", "", CommandNavigate, NavigateArgs{URL: "https://a.test/"}); !res.OK {
		t.Fatalf("navigate: %+v", res)
	}
	waitID := h.core.sendCommand("slow", "", CommandWait, WaitArgs{For: WaitNavigation})
	h.clk.BlockUntil(2)
	if res := h.core.command("fast", "", CommandNavigate, NavigateArgs{URL: "https://b.test/"}); !res.OK {
		t.Fatalf("other group blocked: %+v", res)
	}
	h.clk.Advance(maxWait)
	if res := h.core.result(waitID); res.Code != CodeTimeout {
		t.Fatalf("wait: %+v", res)
	}
}

func TestSaveSendsTheProfile(t *testing.T) {
	h := startPod(t, podOptions{})
	if err := os.WriteFile(filepath.Join(h.dir, "profile", "Preferences"), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	h.core.send(Message{Type: TypeSave})
	h.core.expectState(StateSaving)
	prof := h.core.expectType(TypeProfile)
	if prof.Bytes == 0 || int64(len(prof.data)) != prof.Bytes {
		t.Fatalf("profile: %d bytes announced, %d sent", prof.Bytes, len(prof.data))
	}
	out := t.TempDir()
	if err := unpackProfile(bytes.NewReader(prof.data), out); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(out, "Preferences")); err != nil {
		t.Fatalf("saved profile lacks Preferences: %v", err)
	}
	h.core.expectType(TypeSaved)
	h.core.expectState(StateStopped)
	_ = h.core.conn.Close(websocket.StatusNormalClosure, "")
	if err := h.runResult(); err != nil {
		t.Fatalf("Run after save: %v", err)
	}
}

// TestSaveAfterChromiumExitedSendsNoProfile: Chromium that died before
// core asked for a save left a profile it never closed. Whichever side
// of the pod sees the exit first, core hears that the browser stopped
// and gets no profile for it.
func TestSaveAfterChromiumExitedSendsNoProfile(t *testing.T) {
	h := startPod(t, podOptions{})
	h.proc.Kill()
	// The pod may already be closing the connection; a failed send is
	// the exit having been reported first.
	_ = h.core.w.writeMessage(context.Background(), Message{Type: TypeSave}, nil)
	st := h.core.expectState(StateStopped)
	if !strings.Contains(st.Detail, "chromium exited") {
		t.Fatalf("detail: %q", st.Detail)
	}
	if err := h.runResult(); err == nil {
		t.Fatal("Run returned nil after Chromium died")
	}
	var got []received
	h.core.mu.Lock()
	got = append(got, h.core.held...)
	h.core.mu.Unlock()
	for drained := false; !drained; {
		select {
		case r := <-h.core.in:
			got = append(got, r)
		default:
			drained = true
		}
	}
	for _, r := range got {
		if r.err == nil && (r.Type == TypeProfile || r.Type == TypeSaved || (r.Type == TypeState && r.State == StateSaving)) {
			t.Fatalf("after Chromium died the pod sent %+v", r.Message)
		}
	}
}

func TestBrowserExitIsReported(t *testing.T) {
	h := startPod(t, podOptions{})
	h.proc.Kill()
	st := h.core.expectState(StateStopped)
	if !strings.Contains(st.Detail, "chromium exited") {
		t.Fatalf("detail: %q", st.Detail)
	}
	if err := h.runResult(); err == nil {
		t.Fatal("Run returned nil after Chromium died")
	}
}

// TestDownloadFromAnIframeIsTheTabs: a download that an iframe starts
// is attributed to the tab holding the iframe, whether the pod saw the
// frame attach or has to find it in the tab's frame tree.
func TestDownloadFromAnIframeIsTheTabs(t *testing.T) {
	h := startPod(t, podOptions{})
	if res := h.core.command("g", "", CommandNavigate, NavigateArgs{URL: "https://a.test/"}); !res.OK {
		t.Fatalf("navigate: %+v", res)
	}
	s1 := sessionOf(h, "T1")
	h.cdp.setOverride(func(_ context.Context, session, method string, _ json.RawMessage) (any, bool, error) {
		if method == "Page.getFrameTree" && session == s1 {
			return map[string]any{"frameTree": map[string]any{
				"frame":       map[string]any{"id": "T1"},
				"childFrames": []any{map[string]any{"frame": map[string]any{"id": "F-early"}}},
			}}, true, nil
		}
		return nil, false, nil
	})
	download := func(frameID, guid, name, content string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Join(h.dir, "downloads"), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(h.dir, "downloads", guid), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		h.cdp.emit("", "Browser.downloadWillBegin", map[string]any{"frameId": frameID, "guid": guid, "url": "https://a.test/" + name, "suggestedFilename": name})
		h.cdp.emit("", "Browser.downloadProgress", map[string]any{"guid": guid, "state": "completed", "totalBytes": len(content), "receivedBytes": len(content)})
	}

	// A frame the pod saw attach.
	h.cdp.emit(s1, "Page.frameAttached", map[string]any{"frameId": "F1", "parentFrameId": "T1"})
	download("F1", "guid-1", "export.csv", "a,b")
	dl := h.core.expectType(TypeDownload)
	if dl.Group != "g" || dl.Tab != "t1" || dl.FileName != "export.csv" || string(dl.data) != "a,b" {
		t.Fatalf("download from a known frame: %+v %q", dl.Message, dl.data)
	}

	// A frame attached before the pod watched the tab, found in the
	// tab's frame tree.
	download("F-early", "guid-2", "report.pdf", "%PDF")
	dl = h.core.expectType(TypeDownload)
	if dl.Tab != "t1" || dl.FileName != "report.pdf" {
		t.Fatalf("download from a frame in the tree: %+v", dl.Message)
	}

	// A detached frame is forgotten; a download from no tab is dropped,
	// before the next event (a download of the tab's own) is handled.
	h.cdp.emit(s1, "Page.frameDetached", map[string]any{"frameId": "F1", "reason": "remove"})
	download("F1", "guid-3", "late.bin", "x")
	download("T1", "guid-4", "own.txt", "own")
	if dl = h.core.expectType(TypeDownload); dl.FileName != "own.txt" {
		t.Fatalf("a download from a detached frame was sent: %+v", dl.Message)
	}
	if _, err := os.Stat(filepath.Join(h.dir, "downloads", "guid-3")); !os.IsNotExist(err) {
		t.Fatal("the dropped download was kept")
	}
}

// TestClosingGroupClosesItsPopups: a popup that a tab opens while its
// group is being closed (onbeforeunload, say) is closed too, not
// adopted into a group that is going away.
func TestClosingGroupClosesItsPopups(t *testing.T) {
	h := startPod(t, podOptions{})
	if res := h.core.command("g", "", CommandNavigate, NavigateArgs{URL: "https://a.test/"}); !res.OK {
		t.Fatalf("navigate: %+v", res)
	}
	if res := h.core.command("g", "", CommandTabs, TabsArgs{Action: TabsOpen, URL: "https://a.test/2"}); !res.OK {
		t.Fatalf("open: %+v", res)
	}
	h.core.expectType(TypeTab)
	h.core.expectType(TypeTab)
	second := func() string {
		h.cdp.mu.Lock()
		defer h.cdp.mu.Unlock()
		for tg, u := range h.cdp.urls {
			if u == "https://a.test/2" {
				return tg
			}
		}
		return ""
	}()
	// Closing the first tab, the page opens a popup from the second,
	// still-registered tab; the pod handles it before the close returns.
	h.cdp.setOverride(func(_ context.Context, _, method string, params json.RawMessage) (any, bool, error) {
		if method == "Target.closeTarget" && strings.Contains(string(params), `"T1"`) {
			h.cdp.emit("", "Target.attachedToTarget", map[string]any{
				"sessionId": "SP", "targetInfo": map[string]any{"targetId": "P1", "type": "page", "url": "https://a.test/popup", "openerId": second},
			})
			handled := make(chan struct{})
			h.cdp.events.push(func() { close(handled) })
			<-handled
		}
		return nil, false, nil
	})
	h.core.send(Message{Type: TypeCloseGroup, Group: "g"})
	if res := h.core.command("g", "", CommandTabs, TabsArgs{Action: TabsList}); res.Text != "This task has no tab open." {
		t.Fatalf("after close: %q", res.Text)
	}
	closed := h.cdp.called("Target.closeTarget")
	var popupClosed bool
	for _, c := range closed {
		if strings.Contains(c, `"P1"`) {
			popupClosed = true
		}
	}
	if !popupClosed {
		t.Fatalf("the popup was not closed: %q", closed)
	}
	h.core.mu.Lock()
	defer h.core.mu.Unlock()
	for _, r := range h.core.held {
		if r.Type == TypeTab && r.Opener != "" {
			t.Fatalf("the popup was adopted: %+v", r.Message)
		}
	}
}

// TestLastTabClosedLeavesAFreshSpare: closing the browser's last tab
// makes a fresh page first and keeps it as the spare, then closes the
// tab; the next group gets that fresh page, so its web_back has none
// of the closed tab's history to land in.
func TestLastTabClosedLeavesAFreshSpare(t *testing.T) {
	h := startPod(t, podOptions{})
	if res := h.core.command("g", "", CommandNavigate, NavigateArgs{URL: "https://a.test/"}); !res.OK {
		t.Fatalf("navigate: %+v", res)
	}
	if res := h.core.command("g", "", CommandTabs, TabsArgs{Action: TabsClose, Tab: "t1"}); !res.OK {
		t.Fatalf("close: %+v", res)
	}
	h.core.expectType(TypeTabClosed)
	if created, closed := h.cdp.called("Target.createTarget"), h.cdp.called("Target.closeTarget"); len(created) != 2 || len(closed) != 1 || !strings.Contains(closed[0], `"T1"`) {
		t.Fatalf("created %q, closed %q", created, closed)
	}
	// The next group gets the spare and makes no page of its own.
	if res := h.core.command("h", "", CommandNavigate, NavigateArgs{URL: "https://b.test/"}); !res.OK {
		t.Fatalf("next group: %+v", res)
	}
	if created := h.cdp.called("Target.createTarget"); len(created) != 2 {
		t.Fatalf("createTarget calls: %q", created)
	}
	if res := h.core.command("h", "", CommandTabs, TabsArgs{Action: TabsList}); res.Text != "* t2 https://b.test/ — Title of https://b.test/" {
		t.Fatalf("h's tabs: %q", res.Text)
	}
}

func TestUploadNeedsTheFile(t *testing.T) {
	h := startPod(t, podOptions{files: map[string]string{"g/report.pdf": "%PDF"}})
	if b, err := os.ReadFile(filepath.Join(h.dir, "files", "g", "report.pdf")); err != nil || string(b) != "%PDF" {
		t.Fatalf("file not stored: %q %v", b, err)
	}
	res := h.core.command("g", "", CommandUpload, UploadArgs{Ref: "e1", File: "other.pdf"})
	if res.Code != CodeNotFound || !strings.Contains(res.Error, "other.pdf") {
		t.Fatalf("missing file: %+v", res)
	}
	// Another group cannot use g's files.
	res = h.core.command("h", "", CommandUpload, UploadArgs{Ref: "e1", File: "report.pdf"})
	if res.Code != CodeNotFound {
		t.Fatalf("another group's file: %+v", res)
	}
}
