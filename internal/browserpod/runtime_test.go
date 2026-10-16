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
