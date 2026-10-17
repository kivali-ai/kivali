package web

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/kivali-ai/kivali/internal/browserpod"
	"github.com/kivali-ai/kivali/internal/clock"
)

// TestBrowserConnectEndsOnAShortBody: a body that ends before the bytes
// its message announced leaves the stream out of step, so core closes
// the pod's connection with an internal error, the browser is finished
// at once and the commands in flight fail.
func TestBrowserConnectEndsOnAShortBody(t *testing.T) {
	f := newBrowserFixture(t)
	srv := &Server{Browsers: f.reg}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/browser/{slug}/{session}/connect", srv.handleBrowserConnect)
	hs := httptest.NewServer(mux)
	defer hs.Close()
	wsURL := "ws" + strings.TrimPrefix(hs.URL, "http") + "/v1/browser/alice/vendor.example/connect"
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	errs := make(chan error, 1)
	go func() {
		_, err := f.reg.Command(ctx, "alice", "vendor.example", "job1", browserpod.Message{Name: browserpod.CommandSnapshot})
		errs <- err
	}()
	token := <-f.prov.startedC
	c, _, err := websocket.Dial(ctx, wsURL, &websocket.DialOptions{HTTPHeader: http.Header{browserpod.TokenHeader: {token}}})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.CloseNow() }()
	pod := &podConn{t: t, c: c, ctx: ctx}
	pod.readMessage() // hello
	pod.readMessage() // start
	pod.write(browserpod.Message{Type: browserpod.TypeState, State: browserpod.StateRunning}, nil)
	pod.readMessage() // the first command
	go func() {
		_, err := f.reg.Command(ctx, "alice", "vendor.example", "job1", browserpod.Message{Name: browserpod.CommandSnapshot})
		errs <- err
	}()
	pod.readMessage() // the second command, left in flight

	// Ten bytes announced, three delivered. The pod reads meanwhile, as
	// a pod does, so the close handshake completes at once.
	sent := make(chan error, 1)
	go func() {
		sent <- f.reg.SendFile(ctx, "alice", "vendor.example", "job1", "short.pdf", strings.NewReader("abc"), 10)
	}()
	// The pod sees the file message, its three bytes, then the close.
	typ, rd, err := c.Reader(ctx)
	if err != nil || typ != websocket.MessageText {
		t.Fatalf("file message: %v %v", typ, err)
	}
	_, _ = io.Copy(io.Discard, rd)
	for {
		typ, rd, err := c.Reader(ctx)
		if err != nil {
			var ce websocket.CloseError
			if !errors.As(err, &ce) || ce.Code != websocket.StatusInternalError {
				t.Fatalf("connection ended with %v, want an internal error close", err)
			}
			break
		}
		if typ != websocket.MessageBinary {
			t.Fatalf("a %v frame after the short body", typ)
		}
		_, _ = io.Copy(io.Discard, rd)
	}
	if err := <-sent; err == nil || !strings.Contains(err.Error(), "body ended after 3 of 10 bytes") {
		t.Fatalf("SendFile with a short body: %v", err)
	}
	for i := 0; i < 2; i++ {
		if err := <-errs; err == nil || !strings.Contains(err.Error(), "lost") {
			t.Errorf("command in flight ended with %v, want lost", err)
		}
	}
	if stopped := f.stopped(t); stopped != "alice/vendor.example" {
		t.Errorf("stop = %q", stopped)
	}
}

// gatedReader yields head at once, then blocks until gate is closed,
// then yields tail. head is a whole send chunk, so the link writes it
// as a frame before it asks for more.
type gatedReader struct {
	head, tail []byte
	gate       chan struct{}
	stage      int
}

func (r *gatedReader) Read(p []byte) (int, error) {
	switch r.stage {
	case 0:
		r.stage = 1
		return copy(p, r.head), nil
	case 1:
		<-r.gate
		r.stage = 2
		return copy(p, r.tail), nil
	}
	return 0, io.EOF
}

// TestWSLinkTimesAWriteFromItsStart: a write's limit counts from when
// it takes the write lock, so a command queued behind a long upload is
// not failed for the upload's time. The clock moves past the command's
// limit while the upload holds the lock; the command still goes out
// once the upload has.
func TestWSLinkTimesAWriteFromItsStart(t *testing.T) {
	clk := clock.NewFake()
	link := &wsLink{clk: clk}
	ready := make(chan *websocket.Conn, 1)
	hs := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := websocket.Accept(w, r, &websocket.AcceptOptions{InsecureSkipVerify: true})
		if err != nil {
			return
		}
		ready <- c
		<-r.Context().Done()
	}))
	defer hs.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	pod, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(hs.URL, "http"), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = pod.CloseNow() }()
	pod.SetReadLimit(4 << 20)
	core := <-ready
	defer func() { _ = core.CloseNow() }()
	link.set(core)

	gate := make(chan struct{})
	head := bytes.Repeat([]byte("a"), browserSendChunk)
	body := &gatedReader{head: head, tail: []byte("b"), gate: gate}
	upload := make(chan error, 1)
	go func() {
		upload <- link.send(browserFileSendTimeout, browserpod.Message{Type: browserpod.TypeFile, Group: "job1", FileName: "x.pdf", Bytes: int64(len(head) + 1)}, body)
	}()
	clk.BlockUntil(1) // the upload's limit is armed
	read := func(want websocket.MessageType) []byte {
		t.Helper()
		typ, rd, err := pod.Reader(ctx)
		if err != nil || typ != want {
			t.Fatalf("read: %v %v, want %v", typ, err, want)
		}
		data, _ := io.ReadAll(rd)
		return data
	}
	var file browserpod.Message
	if err := json.Unmarshal(read(websocket.MessageText), &file); err != nil || file.Type != browserpod.TypeFile {
		t.Fatalf("file message = %+v, %v", file, err)
	}
	if got := read(websocket.MessageBinary); !bytes.Equal(got, head) {
		t.Fatalf("first chunk = %d bytes, want the whole first chunk", len(got))
	}

	// A command queued behind the upload; the clock passes its limit
	// while it waits for the lock.
	command := make(chan error, 1)
	go func() {
		command <- link.send(browserSendTimeout, browserpod.Message{Type: browserpod.TypeCommand, ID: "c1", Name: browserpod.CommandSnapshot}, nil)
	}()
	clk.Advance(browserSendTimeout + time.Second)
	close(gate)
	if err := <-upload; err != nil {
		t.Fatalf("upload: %v", err)
	}
	if got := read(websocket.MessageBinary); string(got) != "b" {
		t.Fatalf("second chunk = %q", got)
	}
	var cmd browserpod.Message
	if err := json.Unmarshal(read(websocket.MessageText), &cmd); err != nil || cmd.ID != "c1" {
		t.Fatalf("command after the upload = %+v, %v", cmd, err)
	}
	if err := <-command; err != nil {
		t.Errorf("the command queued behind the upload: %v", err)
	}
	if n := clk.Waiters(); n != 0 {
		t.Errorf("%d write limit(s) still armed after both writes", n)
	}
}
