package agentpod

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/kivali-ai/kivali/internal/clock"
)

// Client is the agent runtime's typed wrapper around core's UDS API.
// One Client per agent-pod process; safe for concurrent use; failures
// surface on the first call (constructor does no I/O).
//
// The transport is a stdlib http.Client with a Unix-domain dialer.
// Routes are HTTP-over-UDS; same pattern as internal/controlclient
// uses for the existing MCP-to-core control socket.
//
// An optional Cache (set via WithCache) accelerates the read path:
// content-addressed reads (attachment / project-file blob+canonical)
// are cached forever once a SHA lands; path-addressed reads (role,
// memory, skill manifest, skill files) ride on ETag/If-None-Match
// so repeats short-circuit to 304 + cached body. The cache is purely
// an optimization — a nil Cache silently degrades to direct round
// trips against core.
type Client struct {
	socketPath string
	slug       string
	http       *http.Client
	cache      *Cache

	// clk drives the reconnect backoff in RunEvents. Defaults to the
	// real clock; WithClock injects a fake so a test can prove the
	// retry behaviour without waiting seconds for it.
	clk clock.Clock
}

// WithClock returns c with a different time source. Test seam.
func (c *Client) WithClock(clk clock.Clock) *Client {
	if clk != nil {
		c.clk = clk
	}
	return c
}

// NewClient returns a client that dials socketPath on every request
// and stamps slug into the SlugHeader of every outgoing request.
// Connection establishment + handshake costs are amortized across
// requests by the http.Transport's connection pool.
func NewClient(socketPath, slug string) *Client {
	dial := func(ctx context.Context, _, _ string) (net.Conn, error) {
		var d net.Dialer
		return d.DialContext(ctx, "unix", socketPath)
	}
	return &Client{
		socketPath: socketPath,
		slug:       slug,
		clk:        clock.New(),
		http: &http.Client{
			Transport: &http.Transport{
				DialContext:           dial,
				IdleConnTimeout:       30 * time.Second,
				ResponseHeaderTimeout: 10 * time.Second,
			},
			// No top-level Timeout — the events stream is long-lived
			// (server pushes with no overall deadline), and per-call
			// requests rely on the caller's ctx for cancellation.
		},
	}
}

// SocketPath returns the UDS path the client dials. Exposed so test
// fixtures and observability can derive auxiliary paths from it.
func (c *Client) SocketPath() string { return c.socketPath }

// Slug returns the slug this client authenticates as.
func (c *Client) Slug() string { return c.slug }

// WithCache attaches a Cache to the client. Returns the same client
// for chainability. Pass nil to disable caching (default after
// NewClient). Safe to call before any read; not safe to call
// concurrently with reads.
func (c *Client) WithCache(cache *Cache) *Client {
	c.cache = cache
	return c
}

// Cache reports the attached Cache (or nil). Used by the runtime's
// cache-invalidate event handler to drop named keys after core
// pushes an invalidation.
func (c *Client) Cache() *Cache { return c.cache }

// OpenEvents opens the long-lived events SSE stream
// GET /v1/agent/{slug}/events on core.sock. Returns a channel of
// decoded Event frames and an error. The channel closes when the
// stream drops, the ctx is canceled, or the server returns a
// non-200; the caller is responsible for reconnect-with-backoff
// (see RunEvents below for a turnkey implementation).
//
// Events arrive line-buffered per SSE framing. We don't fan multi-
// line "data:" payloads (we control both sides; events are
// single-line JSON objects).
func (c *Client) OpenEvents(ctx context.Context) (<-chan Event, error) {
	url := fmt.Sprintf("http://core/v1/agent/%s/events", c.slug)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("agentpod: build events request: %w", err)
	}
	req.Header.Set("Accept", "text/event-stream")
	req.Header.Set("Cache-Control", "no-cache")
	req.Header.Set(SlugHeader, c.slug)

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("agentpod: open events stream: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		return nil, fmt.Errorf("agentpod: events stream HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	if got := resp.Header.Get("Content-Type"); !strings.HasPrefix(got, "text/event-stream") {
		_ = resp.Body.Close()
		return nil, fmt.Errorf("agentpod: events stream content-type = %q, want text/event-stream", got)
	}

	out := make(chan Event, 16)
	go func() {
		defer close(out)
		defer func() { _ = resp.Body.Close() }()
		readSSE(resp.Body, out, ctx.Done())
	}()
	return out, nil
}

// RunEvents opens the events stream and dispatches every received
// Event via handle. On stream drop or transient error it reconnects
// with exponential backoff (capped). Returns when ctx is canceled
// or the handler returns a non-nil error (which Stop() considers
// terminal).
//
// Most callers want this rather than OpenEvents directly: the
// reconnect loop is non-trivial, the cap is the single place to
// tune, and tests can drive a fake server through the same path.
func (c *Client) RunEvents(ctx context.Context, handle func(Event) error) error {
	const minBackoff = 200 * time.Millisecond
	const maxBackoff = 5 * time.Second

	backoff := minBackoff
	for {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		evs, err := c.OpenEvents(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			// Open failed — retry after backoff.
			wait := c.clk.NewTimer(backoff)
			select {
			case <-ctx.Done():
				wait.Stop()
				return ctx.Err()
			case <-wait.C():
			}
			backoff *= 2
			if backoff > maxBackoff {
				backoff = maxBackoff
			}
			continue
		}
		// Stream is up — reset backoff for the next reconnect cycle.
		backoff = minBackoff
		for ev := range evs {
			if err := handle(ev); err != nil {
				return err
			}
		}
		// Stream drained: server closed or ctx canceled. Loop will
		// re-check ctx and either return or reconnect.
	}
}

// PostTurnEvent reports one TurnEvent to core for an in-flight
// chat turn. Path-scoped via /v1/agent/{slug}/chat-turn/{turnID}/event
// so core can correlate the event back to the originating chat-turn
// SSE wake-up.
//
// Returns nil on 2xx, an error otherwise. Idempotent on the
// server side for delta-class events; "done" / "failed" should be
// posted exactly once per turn (the server doesn't enforce this
// today, but multiple "done"s would emit duplicate hub events).
func (c *Client) PostTurnEvent(ctx context.Context, turnID string, ev TurnEvent) error {
	if turnID == "" {
		return fmt.Errorf("agentpod: PostTurnEvent: turnID is required")
	}
	body, err := json.Marshal(ev)
	if err != nil {
		return fmt.Errorf("agentpod: marshal TurnEvent: %w", err)
	}
	url := fmt.Sprintf("http://core/v1/agent/%s/chat-turn/%s/event", c.slug, turnID)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("agentpod: build PostTurnEvent request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(SlugHeader, c.slug)
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("agentpod: PostTurnEvent: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode/100 != 2 {
		errBody, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("agentpod: PostTurnEvent HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(errBody)))
	}
	return nil
}

// readSSE reads SSE-framed events from r and delivers them on out.
// Returns when r is exhausted, errors, or done is closed.
//
// Frame shape: any number of "field: value" lines, terminated by an
// empty line. We honor the two fields we use (`event:` and `data:`);
// other fields (id, retry, comment lines starting with ":") are
// recognized but ignored.
func readSSE(r io.Reader, out chan<- Event, done <-chan struct{}) {
	br := bufio.NewReader(r)
	var (
		evType EventType
		data   strings.Builder
	)
	flush := func() {
		if evType == "" && data.Len() == 0 {
			return
		}
		ev := Event{Type: evType, Data: []byte(data.String())}
		select {
		case out <- ev:
		case <-done:
		}
		evType = ""
		data.Reset()
	}
	for {
		select {
		case <-done:
			return
		default:
		}
		line, err := br.ReadString('\n')
		if errors.Is(err, io.EOF) {
			flush()
			return
		}
		if err != nil {
			flush()
			return
		}
		// Strip trailing \n (and \r if CRLF).
		line = strings.TrimRight(line, "\r\n")
		if line == "" {
			// Frame boundary.
			flush()
			continue
		}
		if strings.HasPrefix(line, ":") {
			// Comment line (heartbeat). Ignore.
			continue
		}
		field, value, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		// Per SSE spec, leading single space after the colon is
		// stripped if present.
		value = strings.TrimPrefix(value, " ")
		switch field {
		case "event":
			evType = EventType(value)
		case "data":
			if data.Len() > 0 {
				data.WriteByte('\n')
			}
			data.WriteString(value)
		case "id", "retry":
			// Recognized, not used.
		default:
			// Unknown field — ignore.
		}
	}
}
