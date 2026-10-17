package browserpod

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sync"

	"github.com/coder/websocket"
)

// maxTextFrame bounds one text frame: a Message, which carries
// commands, results, screenshots and a window's input events, a paste
// at most, but never bulk bytes. It is above core's read limit for a
// window, so an input event core accepted still fits once wrapped.
const maxTextFrame = 2 << 20

// errMessageTooLong is readMessage's answer to a text frame over
// maxTextFrame. The frame has been read to its end, so the connection
// is still in step: the caller refuses that one message and reads on.
var errMessageTooLong = errors.New("browserpod: message over the frame limit")

// writeChunk is the size of the binary frames the pod writes.
const writeChunk = 256 << 10

// wire is the framed protocol over the one WebSocket: text frames are
// Messages, and a Message with Bytes > 0 is followed by exactly that
// many bytes as binary frames. Reads happen on one goroutine; writes
// come from many and are serialised by wmu.
type wire struct {
	c   *websocket.Conn
	wmu sync.Mutex
}

func newWire(c *websocket.Conn) *wire {
	// Binary frames may be any size; text frames are bounded in
	// readMessage instead.
	c.SetReadLimit(-1)
	return &wire{c: c}
}

// readMessage reads the next text frame. When the message announces
// bytes, the returned reader yields exactly that many, read from the
// binary frames that follow; the caller consumes it (or drains it)
// before calling readMessage again. Without bytes the reader is empty.
func (w *wire) readMessage(ctx context.Context) (Message, io.Reader, error) {
	typ, r, err := w.c.Reader(ctx)
	if err != nil {
		return Message{}, nil, err
	}
	if typ != websocket.MessageText {
		return Message{}, nil, errors.New("browserpod: binary frame where a message was expected")
	}
	data, err := io.ReadAll(io.LimitReader(r, maxTextFrame+1))
	if err != nil {
		return Message{}, nil, err
	}
	if len(data) > maxTextFrame {
		if _, err := io.Copy(io.Discard, r); err != nil {
			return Message{}, nil, err
		}
		return Message{}, nil, errMessageTooLong
	}
	var msg Message
	if err := json.Unmarshal(data, &msg); err != nil {
		return Message{}, nil, fmt.Errorf("browserpod: bad message: %w", err)
	}
	if msg.Bytes < 0 {
		return Message{}, nil, errors.New("browserpod: negative byte count")
	}
	return msg, &binaryReader{ctx: ctx, c: w.c, remaining: msg.Bytes}, nil
}

// binaryReader reads the binary frames that follow a message, frame
// after frame, until the announced count is consumed.
type binaryReader struct {
	ctx       context.Context
	c         *websocket.Conn
	remaining int64
	cur       io.Reader
}

func (b *binaryReader) Read(p []byte) (int, error) {
	for {
		if b.remaining == 0 {
			if b.cur != nil {
				// The frame must end exactly where the count does.
				var one [1]byte
				if n, _ := b.cur.Read(one[:]); n > 0 {
					return 0, errors.New("browserpod: binary frame longer than announced")
				}
				b.cur = nil
			}
			return 0, io.EOF
		}
		if b.cur == nil {
			typ, r, err := b.c.Reader(b.ctx)
			if err != nil {
				return 0, err
			}
			if typ != websocket.MessageBinary {
				return 0, errors.New("browserpod: message where binary bytes were expected")
			}
			b.cur = r
		}
		if int64(len(p)) > b.remaining {
			p = p[:b.remaining]
		}
		n, err := b.cur.Read(p)
		b.remaining -= int64(n)
		if errors.Is(err, io.EOF) {
			b.cur = nil
			err = nil
		}
		if n > 0 || err != nil {
			return n, err
		}
	}
}

// writeMessage sends msg and, when msg.Bytes > 0, exactly that many
// bytes from body as binary frames. Concurrent callers are serialised
// so a message and its bytes are never interleaved with another.
func (w *wire) writeMessage(ctx context.Context, msg Message, body io.Reader) error {
	data, err := json.Marshal(msg)
	if err != nil {
		return err
	}
	w.wmu.Lock()
	defer w.wmu.Unlock()
	if err := w.c.Write(ctx, websocket.MessageText, data); err != nil {
		return err
	}
	if msg.Bytes == 0 {
		return nil
	}
	if body == nil {
		return errors.New("browserpod: message announces bytes but has no body")
	}
	buf := make([]byte, writeChunk)
	left := msg.Bytes
	for left > 0 {
		chunk := buf
		if int64(len(chunk)) > left {
			chunk = chunk[:left]
		}
		n, err := io.ReadFull(body, chunk)
		if n > 0 {
			if werr := w.c.Write(ctx, websocket.MessageBinary, chunk[:n]); werr != nil {
				return werr
			}
			left -= int64(n)
		}
		if err != nil && left > 0 {
			// The announced count can no longer be met and the stream
			// is out of step, so the connection cannot go on.
			_ = w.c.Close(websocket.StatusInternalError, "short body")
			return fmt.Errorf("browserpod: body ended %d bytes short: %w", left, err)
		}
	}
	return nil
}

// sendBytes is writeMessage for an in-memory body.
func (w *wire) sendBytes(ctx context.Context, msg Message, data []byte) error {
	msg.Bytes = int64(len(data))
	return w.writeMessage(ctx, msg, bytes.NewReader(data))
}
