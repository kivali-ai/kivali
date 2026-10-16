// Package web (browser_socket.go) — the browser pod's one connection.
//
// GET /v1/browser/{slug}/{session}/connect on core.sock, upgraded to a
// WebSocket. The pod presents the token core put in its environment;
// core accepts the connection only for the browser it is waiting for.
// Text frames are browserpod.Message; a message announcing bytes is
// followed by that many bytes as binary frames, which the handler
// hands to the registry as a reader before it reads the next message.

package web

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sync"

	"github.com/coder/websocket"

	"github.com/kivali-ai/kivali/internal/browserpod"
)

// browserSocketReadLimit bounds one text frame from the pod: a result
// with a snapshot or a base64 screenshot. Binary frames are read in
// sequence under the announcing message's byte count.
const browserSocketReadLimit = 4 << 20

// handleBrowserConnect accepts a browser pod's connection.
func (s *Server) handleBrowserConnect(w http.ResponseWriter, r *http.Request) {
	slug, session := r.PathValue("slug"), r.PathValue("session")
	if s.Browsers == nil {
		http.Error(w, "browsers not configured", http.StatusServiceUnavailable)
		return
	}
	token := r.Header.Get(browserpod.TokenHeader)
	b := s.Browsers.claim(slug, session, token)
	if b == nil || subtle.ConstantTimeCompare([]byte(b.token), []byte(token)) != 1 {
		http.Error(w, "no browser is waiting for this connection", http.StatusForbidden)
		return
	}
	c, err := websocket.Accept(w, r, &websocket.AcceptOptions{
		// The socket is a Unix socket reachable only from pods on this
		// node; there is no browser origin to check.
		InsecureSkipVerify: true,
	})
	if err != nil {
		return
	}
	c.SetReadLimit(browserSocketReadLimit)
	ctx := r.Context()
	link := &wsLink{c: c}
	if err := s.Browsers.attach(ctx, b, link); err != nil {
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

// wsLink sends to the pod. Writes are serialised: several goroutines
// relay commands at once, and a message's binary frames must follow
// it with nothing between.
type wsLink struct {
	mu sync.Mutex
	c  *websocket.Conn
}

func (l *wsLink) send(ctx context.Context, msg browserpod.Message, body io.Reader) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	text, err := json.Marshal(msg)
	if err != nil {
		return err
	}
	if err := l.c.Write(ctx, websocket.MessageText, text); err != nil {
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
			if err := l.c.Write(ctx, websocket.MessageBinary, buf[:n]); err != nil {
				return err
			}
			sent += int64(n)
		}
		if rerr != nil {
			if sent == msg.Bytes {
				break
			}
			return fmt.Errorf("body ended after %d of %d bytes: %w", sent, msg.Bytes, rerr)
		}
	}
	return nil
}

func (l *wsLink) close() {
	_ = l.c.Close(websocket.StatusNormalClosure, "")
}

// frameBody reads the bytes a message announced, across as many binary
// frames as the pod sent them in.
type frameBody struct {
	ctx       context.Context
	c         *websocket.Conn
	remaining int64
	cur       io.Reader
	err       error
}

func (f *frameBody) Read(p []byte) (int, error) {
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
			peek, _ := io.ReadAll(io.LimitReader(rd, 200))
			f.err = fmt.Errorf("a text frame arrived inside a byte body with %d bytes left: %q", f.remaining, peek)
			return 0, f.err
		}
		f.cur = rd
	}
	if int64(len(p)) > f.remaining {
		p = p[:f.remaining]
	}
	n, err := f.cur.Read(p)
	f.remaining -= int64(n)
	if errors.Is(err, io.EOF) {
		f.cur = nil
		err = nil
		if n == 0 {
			return f.Read(p)
		}
	}
	if err != nil {
		f.err = err
	}
	return n, err
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
