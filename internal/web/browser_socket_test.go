package web

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/kivali-ai/kivali/internal/browserpod"
	"github.com/kivali-ai/kivali/internal/files"
)

// podConn is a fake pod on the real WebSocket: it reads core's frames
// and writes its own.
type podConn struct {
	t   *testing.T
	c   *websocket.Conn
	ctx context.Context
}

func (p *podConn) readMessage() (browserpod.Message, []byte) {
	p.t.Helper()
	typ, rd, err := p.c.Reader(p.ctx)
	if err != nil {
		p.t.Fatalf("pod read: %v", err)
	}
	if typ != websocket.MessageText {
		p.t.Fatalf("pod read: binary frame where a message was expected")
	}
	var msg browserpod.Message
	if err := json.NewDecoder(rd).Decode(&msg); err != nil {
		p.t.Fatalf("pod decode: %v", err)
	}
	var body []byte
	for int64(len(body)) < msg.Bytes {
		typ, rd, err := p.c.Reader(p.ctx)
		if err != nil || typ != websocket.MessageBinary {
			p.t.Fatalf("pod read body: %v %v", typ, err)
		}
		chunk, _ := io.ReadAll(rd)
		body = append(body, chunk...)
	}
	return msg, body
}

func (p *podConn) write(msg browserpod.Message, body []byte) {
	p.t.Helper()
	text, _ := json.Marshal(msg)
	if err := p.c.Write(p.ctx, websocket.MessageText, text); err != nil {
		p.t.Fatalf("pod write: %v", err)
	}
	// Bytes go in several frames on purpose.
	for len(body) > 0 {
		n := min(len(body), 7)
		if err := p.c.Write(p.ctx, websocket.MessageBinary, body[:n]); err != nil {
			p.t.Fatalf("pod write body: %v", err)
		}
		body = body[n:]
	}
}

// TestBrowserConnectOverTheWire runs the connect route end to end over
// a real WebSocket: the token gates the upgrade, the handshake carries
// the saved profile in binary frames, a command and its result pair
// up, a download split across frames lands on disk, and a save stores
// the profile the pod sends back.
func TestBrowserConnectOverTheWire(t *testing.T) {
	f := newBrowserFixture(t)
	saved := bytes.Repeat([]byte("old-profile-"), 100)
	if _, err := f.st.EnsureBrowserSession("alice", "open", "", f.clk.Now()); err != nil {
		t.Fatal(err)
	}
	if _, err := f.st.WriteBrowserProfile("alice", "open", bytes.NewReader(saved), f.clk.Now()); err != nil {
		t.Fatal(err)
	}
	srv := &Server{Browsers: f.reg}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/browser/{slug}/{session}/connect", srv.handleBrowserConnect)
	hs := httptest.NewServer(mux)
	defer hs.Close()
	wsURL := "ws" + strings.TrimPrefix(hs.URL, "http") + "/v1/browser/alice/open/connect"
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	type outcome struct {
		res browserpod.Message
		err error
	}
	got := make(chan outcome, 1)
	go func() {
		res, err := f.reg.Command(ctx, "alice", "open", "job1", browserpod.Message{Name: browserpod.CommandNavigate, Args: json.RawMessage(`{"url":"https://x.example/"}`)})
		got <- outcome{res, err}
	}()
	token := <-f.prov.startedC

	// The wrong token is refused before any upgrade.
	if _, resp, err := websocket.Dial(ctx, wsURL, &websocket.DialOptions{HTTPHeader: http.Header{browserpod.TokenHeader: {"nope"}}}); err == nil || resp == nil || resp.StatusCode != http.StatusForbidden {
		t.Fatalf("wrong token: err=%v resp=%v", err, resp)
	}
	c, _, err := websocket.Dial(ctx, wsURL, &websocket.DialOptions{HTTPHeader: http.Header{browserpod.TokenHeader: {token}}})
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	c.SetReadLimit(4 << 20)
	pod := &podConn{t: t, c: c, ctx: ctx}

	hello, profile := pod.readMessage()
	if hello.Type != browserpod.TypeHello || hello.Bytes != int64(len(saved)) || !bytes.Equal(profile, saved) {
		t.Fatalf("hello = %+v with %d bytes, want the saved profile", hello, len(profile))
	}
	if start, _ := pod.readMessage(); start.Type != browserpod.TypeStart {
		t.Fatalf("after hello: %+v, want start", start)
	}
	pod.write(browserpod.Message{Type: browserpod.TypeState, State: browserpod.StateRunning, Detail: "sandbox: on"}, nil)
	cmd, _ := pod.readMessage()
	if cmd.Type != browserpod.TypeCommand || cmd.Name != browserpod.CommandNavigate || cmd.Group != "job1" {
		t.Fatalf("command = %+v", cmd)
	}
	pod.write(browserpod.Message{Type: browserpod.TypeNavigated, Group: "job1", Tab: "t1", URL: "https://x.example/", Title: "X"}, nil)
	csv := []byte("a,b\n1,2\n3,4\n5,6\n7,8\n")
	pod.write(browserpod.Message{Type: browserpod.TypeDownload, Group: "job1", Tab: "t1", FileName: "report.csv", Bytes: int64(len(csv))}, csv)
	pod.write(browserpod.Message{Type: browserpod.TypeResult, ID: cmd.ID, OK: true, Text: "Loaded X — https://x.example/"}, nil)
	out := <-got
	if out.err != nil || !out.res.OK || !strings.HasPrefix(out.res.Text, "Loaded X") {
		t.Fatalf("Command = %+v, %v", out.res, out.err)
	}
	// The download was read across frames before the result was handled.
	if body, err := os.ReadFile(filepath.Join(f.root, files.UntrustedDownloadsDir, "report.csv")); err != nil || !bytes.Equal(body, csv) {
		t.Errorf("download = %q, %v", body, err)
	}

	// Release, reap, save: the profile the pod sends is stored.
	facts, downloads := f.reg.ReleaseGroup("alice", "open", "job1")
	if facts.Pages != 1 || facts.Origin != "https://x.example" || len(downloads) != 1 {
		t.Errorf("facts = %+v %v", facts, downloads)
	}
	if cg, _ := pod.readMessage(); cg.Type != browserpod.TypeCloseGroup {
		t.Fatalf("after release: %+v", cg)
	}
	f.clk.Advance(browserIdleAfter)
	f.reg.sweep(f.clk.Now())
	if save, _ := pod.readMessage(); save.Type != browserpod.TypeSave {
		t.Fatalf("after reap: %+v", save)
	}
	fresh := bytes.Repeat([]byte("new-profile-"), 200)
	pod.write(browserpod.Message{Type: browserpod.TypeProfile, Bytes: int64(len(fresh))}, fresh)
	pod.write(browserpod.Message{Type: browserpod.TypeSaved}, nil)
	// Core closes the connection once it is done.
	if _, _, err := c.Reader(ctx); err == nil {
		t.Error("connection stayed open after the save")
	}
	r, n, err := f.st.OpenBrowserProfile("alice", "open")
	if err != nil || n != int64(len(fresh)) {
		t.Fatalf("stored profile after save = %d, %v", n, err)
	}
	gotProfile, _ := io.ReadAll(r)
	_ = r.Close()
	if !bytes.Equal(gotProfile, fresh) {
		t.Error("stored profile differs from what the pod sent")
	}
	if stopped := f.prov.awaitStop(t); stopped != "alice/open" {
		t.Errorf("pod stop = %q", stopped)
	}
}
