package web

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/kivali-ai/kivali/internal/browserpod"
)

// TestBrowserConnectEndsOnAnEmptyFrameFlood: a byte body split into
// empty binary frames is read in a loop, not by recursion, and a pod
// that sends a stream of them loses its connection.
func TestBrowserConnectEndsOnAnEmptyFrameFlood(t *testing.T) {
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
	pod.readMessage() // the command

	text, _ := json.Marshal(browserpod.Message{Type: browserpod.TypeDownload, Group: "job1", Tab: "t1", FileName: "x.bin", Bytes: 5})
	if err := c.Write(ctx, websocket.MessageText, text); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < maxEmptyFrames*4; i++ {
		if err := c.Write(ctx, websocket.MessageBinary, nil); err != nil {
			break // core closed the connection: what we want
		}
	}
	if _, _, err := c.Reader(ctx); err == nil {
		t.Error("the connection survived an empty-frame flood")
	}
	if err := <-errs; err == nil || !strings.Contains(err.Error(), "lost") {
		t.Errorf("the command in flight ended with %v, want lost", err)
	}
	f.stopped(t)
}
