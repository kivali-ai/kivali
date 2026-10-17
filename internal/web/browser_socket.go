// Package web (browser_socket.go) — the browser pod's one connection.
//
// GET /v1/browser/{slug}/{session}/connect on browser.sock, upgraded
// to a WebSocket. The socket is the pod's own: it serves this route and
// nothing else, so a pod never reaches the agent routes on core.sock.
// The pod presents the token core put in its environment; core accepts
// the connection only for the browser it is waiting for. Text frames
// are browserpod.Message; a message announcing bytes is followed by
// that many bytes as binary frames, which the handler hands to the
// registry as a reader before it reads the next message.

package web

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/coder/websocket"

	"github.com/kivali-ai/kivali/internal/browserpod"
	"github.com/kivali-ai/kivali/internal/clock"
)

// browserSocketReadLimit bounds one text frame from the pod: a result
// with a snapshot or a base64 screenshot. Binary frames are read in
// sequence under the announcing message's byte count.
const browserSocketReadLimit = 8 << 20

// maxEmptyFrames is how many empty binary frames a byte body may
// carry before the connection is taken as hostile.
const maxEmptyFrames = 64

// StartBrowserSocket binds the browser pods' socket and serves the
// connect route on it, and only that. Returns the stop function.
func (s *Server) StartBrowserSocket(path string) (func(), error) {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/browser/{slug}/{session}/connect", s.handleBrowserConnect)
	return s.startUDSListenerWith(path, mux)
}

// handleBrowserConnect accepts a browser pod's connection.
func (s *Server) handleBrowserConnect(w http.ResponseWriter, r *http.Request) {
	slug, session := r.PathValue("slug"), r.PathValue("session")
	if s.Browsers == nil {
		http.Error(w, "browsers not configured", http.StatusServiceUnavailable)
		return
	}
	token := r.Header.Get(browserpod.TokenHeader)
	// Claim before the upgrade: a connection nobody is waiting for is
	// refused as plain HTTP, and two connections with one token cannot
	// both be taken, because the link is bound under the claim's lock.
	link := &wsLink{clk: s.Browsers.clk}
	b := s.Browsers.claim(slug, session, token, link)
	if b == nil {
		http.Error(w, "no browser is waiting for this connection", http.StatusForbidden)
		return
	}
	c, err := websocket.Accept(w, r, &websocket.AcceptOptions{
		// The socket is a Unix socket reachable only from pods on this
		// node; there is no browser origin to check.
		InsecureSkipVerify: true,
	})
	if err != nil {
		s.Browsers.failStart(b, fmt.Errorf("upgrade: %w", err))
		return
	}
	c.SetReadLimit(browserSocketReadLimit)
	link.set(c)
	ctx := r.Context()
	if err := s.Browsers.attach(b); err != nil {
		s.Browsers.logf("browser %s: handshake: %v", b.key(), err)
		s.Browsers.failStart(b, fmt.Errorf("handshake: %w", err))
		return
	}
	defer s.Browsers.onLost(b)
	for {
		typ, rd, err := c.Reader(ctx)
		if err != nil {
			return
		}
		if typ != websocket.MessageText {
			// Bytes outside an announcing message: drain and go on.
			_, _ = io.Copy(io.Discard, rd)
			continue
		}
		var msg browserpod.Message
		if err := json.NewDecoder(rd).Decode(&msg); err != nil {
			s.Browsers.logf("browser %s: bad frame: %v", b.key(), err)
			return
		}
		// The message's reader must reach its end before the next
		// frame is asked for.
		_, _ = io.Copy(io.Discard, rd)
		if msg.Bytes > 0 {
			body := &frameBody{ctx: ctx, c: c, remaining: msg.Bytes}
			s.Browsers.onMessage(b, msg, body)
			if err := body.drain(); err != nil {
				return
			}
			continue
		}
		s.Browsers.onMessage(b, msg, nil)
	}
}

// wsLink sends to the pod. Writes are serialised by mu: several
// goroutines relay commands at once, and a message's binary frames
// must follow it with nothing between. A write's time limit counts
// from when it takes mu, after any write ahead of it, so a command
// queued behind a long upload is not failed for the upload's time.
// The connection is set once the upgrade has happened; a send before
// that fails. close does not take mu: a pod that has stopped reading
// holds a writer until that writer's limit, and must not hold the
// close with it.
type wsLink struct {
	clk clock.Clock
	mu  sync.Mutex
	c   atomic.Pointer[websocket.Conn]
}

func (l *wsLink) set(c *websocket.Conn) { l.c.Store(c) }

// send writes msg and then, when msg announces bytes, that many bytes
// of body as binary frames, within limit of taking the write lock. A
// body that ends short leaves the stream out of step, so the
// connection is closed, as the pod does for its own short body: the
// read loop sees the close and the registry ends the browser.
func (l *wsLink) send(limit time.Duration, msg browserpod.Message, body io.Reader) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	c := l.c.Load()
	if c == nil {
		return errors.New("the pod is not connected")
	}
	text, err := json.Marshal(msg)
	if err != nil {
		return err
	}
	ctx, cancel := l.deadline(limit)
	defer cancel()
	if err := c.Write(ctx, websocket.MessageText, text); err != nil {
		return err
	}
	if msg.Bytes <= 0 || body == nil {
		return nil
	}
	buf := make([]byte, browserSendChunk)
	var sent int64
	for sent < msg.Bytes {
		want := int64(len(buf))
		if rem := msg.Bytes - sent; rem < want {
			want = rem
		}
		n, rerr := io.ReadFull(body, buf[:want])
		if n > 0 {
			if err := c.Write(ctx, websocket.MessageBinary, buf[:n]); err != nil {
				return err
			}
			sent += int64(n)
		}
		if rerr != nil {
			if sent == msg.Bytes {
				break
			}
			_ = c.Close(websocket.StatusInternalError, "short body")
			return fmt.Errorf("body ended after %d of %d bytes: %w", sent, msg.Bytes, rerr)
		}
	}
	return nil
}

// deadline is the context a write runs under: detached from any
// caller, because the WebSocket library closes the connection when a
// write's context ends and a caller's cancellation must never reach a
// write that other groups share, and ended by the clock after limit.
// The returned cancel releases the timer.
func (l *wsLink) deadline(limit time.Duration) (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithCancel(context.Background())
	t := l.clk.NewTimer(limit)
	go func() {
		select {
		case <-t.C():
			cancel()
		case <-ctx.Done():
		}
	}()
	return ctx, func() {
		t.Stop()
		cancel()
	}
}

// close ends the connection at once, without the close handshake and
// without waiting for a write in progress, which fails.
func (l *wsLink) close() {
	if c := l.c.Load(); c != nil {
		_ = c.CloseNow()
	}
}

// frameBody reads the bytes a message announced, across as many binary
// frames as the pod sent them in.
type frameBody struct {
	ctx       context.Context
	c         *websocket.Conn
	remaining int64
	cur       io.Reader
	// curBytes is what the current frame has delivered so far; a
	// frame that ends at zero is an empty frame.
	curBytes int64
	empty    int
	err      error
}

func (f *frameBody) Read(p []byte) (int, error) {
	for {
		if f.err != nil {
			return 0, f.err
		}
		if f.remaining <= 0 {
			return 0, io.EOF
		}
		if f.cur == nil {
			typ, rd, err := f.c.Reader(f.ctx)
			if err != nil {
				f.err = err
				return 0, err
			}
			if typ != websocket.MessageBinary {
				f.err = fmt.Errorf("a text frame arrived inside a byte body with %d bytes left", f.remaining)
				return 0, f.err
			}
			f.cur = rd
		}
		if int64(len(p)) > f.remaining {
			p = p[:f.remaining]
		}
		n, err := f.cur.Read(p)
		f.remaining -= int64(n)
		f.curBytes += int64(n)
		if errors.Is(err, io.EOF) {
			// The frame is over. One that carried nothing at all is an
			// empty frame: a few are a quirk, a stream of them is a pod
			// trying to spin core.
			emptyFrame := f.curBytes == 0
			f.cur, f.curBytes = nil, 0
			if n == 0 {
				if emptyFrame {
					f.empty++
					if f.empty > maxEmptyFrames {
						f.err = errors.New("too many empty frames in a byte body")
						return 0, f.err
					}
				}
				continue
			}
			return n, nil
		}
		if err != nil {
			f.err = err
		}
		return n, err
	}
}

// drain consumes whatever the receiver left unread, so the next text
// frame is the next message.
func (f *frameBody) drain() error {
	_, err := io.Copy(io.Discard, f)
	if f.cur != nil {
		_, _ = io.Copy(io.Discard, f.cur)
	}
	return err
}
