package browserpod

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sync"
)

// cdpConn is the Chrome DevTools Protocol as the pod uses it: calls
// correlated by id and routed by session, and events delivered to
// subscribers. The browser session is the empty session id.
//
// Event handlers run one at a time, in the order Chromium sent the
// events, on a goroutine of their own; a handler may Call.
type cdpConn interface {
	Call(ctx context.Context, sessionID, method string, params any) (json.RawMessage, error)
	On(sessionID, method string, fn func(json.RawMessage)) (off func())
}

// cdpError is an error response from Chromium.
type cdpError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Data    string `json:"data,omitempty"`
}

func (e *cdpError) Error() string {
	if e.Data != "" {
		return fmt.Sprintf("%s (%s)", e.Message, e.Data)
	}
	return e.Message
}

// errCDPClosed is returned by calls on a connection whose browser is
// gone.
var errCDPClosed = errors.New("browserpod: browser connection closed")

type cdpKey struct{ session, method string }

type cdpHandler struct{ fn func(json.RawMessage) }

type cdpReply struct {
	result json.RawMessage
	err    error
}

type cdpEvent struct {
	key    cdpKey
	params json.RawMessage
}

// pipeCDP speaks CDP over --remote-debugging-pipe: JSON messages
// separated by NUL bytes.
type pipeCDP struct {
	w   io.WriteCloser
	wmu sync.Mutex

	mu       sync.Mutex
	next     int64
	pending  map[int64]chan cdpReply
	handlers map[cdpKey][]*cdpHandler
	queue    []cdpEvent
	wake     chan struct{}
	closed   chan struct{}
	isClosed bool
}

func newPipeCDP(r io.Reader, w io.WriteCloser) *pipeCDP {
	c := &pipeCDP{
		w:        w,
		pending:  map[int64]chan cdpReply{},
		handlers: map[cdpKey][]*cdpHandler{},
		wake:     make(chan struct{}, 1),
		closed:   make(chan struct{}),
	}
	go c.readLoop(r)
	go c.eventLoop()
	return c
}

type cdpOutgoing struct {
	ID        int64  `json:"id"`
	Method    string `json:"method"`
	Params    any    `json:"params,omitempty"`
	SessionID string `json:"sessionId,omitempty"`
}

type cdpIncoming struct {
	ID        int64           `json:"id"`
	Method    string          `json:"method"`
	Params    json.RawMessage `json:"params"`
	SessionID string          `json:"sessionId"`
	Result    json.RawMessage `json:"result"`
	Error     *cdpError       `json:"error"`
}

func (c *pipeCDP) Call(ctx context.Context, sessionID, method string, params any) (json.RawMessage, error) {
	c.mu.Lock()
	if c.isClosed {
		c.mu.Unlock()
		return nil, errCDPClosed
	}
	c.next++
	id := c.next
	ch := make(chan cdpReply, 1)
	c.pending[id] = ch
	c.mu.Unlock()

	data, err := json.Marshal(cdpOutgoing{ID: id, Method: method, Params: params, SessionID: sessionID})
	if err != nil {
		c.forget(id)
		return nil, err
	}
	data = append(data, 0)
	c.wmu.Lock()
	_, err = c.w.Write(data)
	c.wmu.Unlock()
	if err != nil {
		c.forget(id)
		return nil, fmt.Errorf("browserpod: write %s: %w", method, err)
	}
	select {
	case rep := <-ch:
		return rep.result, rep.err
	case <-ctx.Done():
		c.forget(id)
		return nil, ctx.Err()
	case <-c.closed:
		return nil, errCDPClosed
	}
}

func (c *pipeCDP) forget(id int64) {
	c.mu.Lock()
	delete(c.pending, id)
	c.mu.Unlock()
}

func (c *pipeCDP) On(sessionID, method string, fn func(json.RawMessage)) func() {
	h := &cdpHandler{fn: fn}
	k := cdpKey{sessionID, method}
	c.mu.Lock()
	c.handlers[k] = append(c.handlers[k], h)
	c.mu.Unlock()
	return func() {
		c.mu.Lock()
		defer c.mu.Unlock()
		hs := c.handlers[k]
		for i, x := range hs {
			if x == h {
				c.handlers[k] = append(hs[:i:i], hs[i+1:]...)
				break
			}
		}
		if len(c.handlers[k]) == 0 {
			delete(c.handlers, k)
		}
	}
}

// Closed is closed once the pipe from Chromium has ended.
func (c *pipeCDP) Closed() <-chan struct{} { return c.closed }

func (c *pipeCDP) readLoop(r io.Reader) {
	br := bufio.NewReaderSize(r, 1<<20)
	for {
		data, err := br.ReadBytes(0)
		if err != nil {
			c.shut()
			return
		}
		data = data[:len(data)-1]
		var in cdpIncoming
		if json.Unmarshal(data, &in) != nil {
			continue
		}
		if in.ID != 0 {
			c.mu.Lock()
			ch := c.pending[in.ID]
			delete(c.pending, in.ID)
			c.mu.Unlock()
			if ch != nil {
				if in.Error != nil {
					ch <- cdpReply{err: in.Error}
				} else {
					ch <- cdpReply{result: in.Result}
				}
			}
			continue
		}
		if in.Method == "" {
			continue
		}
		c.mu.Lock()
		c.queue = append(c.queue, cdpEvent{key: cdpKey{in.SessionID, in.Method}, params: in.Params})
		c.mu.Unlock()
		select {
		case c.wake <- struct{}{}:
		default:
		}
	}
}

func (c *pipeCDP) shut() {
	c.mu.Lock()
	if !c.isClosed {
		c.isClosed = true
		close(c.closed)
	}
	c.mu.Unlock()
	_ = c.w.Close()
}

// eventLoop delivers queued events in order. It runs apart from the
// reader so a handler may Call and still get its answer.
func (c *pipeCDP) eventLoop() {
	for {
		c.mu.Lock()
		if len(c.queue) == 0 {
			c.mu.Unlock()
			select {
			case <-c.wake:
				continue
			case <-c.closed:
				return
			}
		}
		ev := c.queue[0]
		c.queue = c.queue[1:]
		hs := append([]*cdpHandler(nil), c.handlers[ev.key]...)
		c.mu.Unlock()
		for _, h := range hs {
			h.fn(ev.params)
		}
	}
}
