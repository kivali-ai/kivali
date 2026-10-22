package supervisor

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strconv"
	"sync"
	"time"

	"github.com/kivali-ai/kivali/internal/supervisor/guestapi"
)

// Forwarder accepts TCP connections on 127.0.0.1:<port> and carries
// each over its own new agent connection (Machine.DialAgent; vsock on
// macOS) to the guest agent's proxy. Every connection is independent;
// agent connections are cheap, so nothing is multiplexed here.
type Forwarder struct {
	ln   net.Listener
	port int
	dial func(ctx context.Context) (net.Conn, error)
	clk  Clock
	logf Logf
	// ctx ends on Close, abandoning dials still in progress.
	ctx    context.Context
	cancel context.CancelFunc

	mu    sync.Mutex
	conns map[net.Conn]struct{}
	wg    sync.WaitGroup
}

// StartForward binds 127.0.0.1:port. The bind is loopback only, by
// construction: a local org has no network-facing sign-in story.
func StartForward(port int, dial func(ctx context.Context) (net.Conn, error), clk Clock, logf Logf) (*Forwarder, error) {
	ln, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)))
	if err != nil {
		return nil, fmt.Errorf("forward: %w", err)
	}
	return serveForward(ln, dial, clk, logf), nil
}

// serveForward forwards the connections ln accepts.
func serveForward(ln net.Listener, dial func(ctx context.Context) (net.Conn, error), clk Clock, logf Logf) *Forwarder {
	ctx, cancel := context.WithCancel(context.Background())
	f := &Forwarder{ln: ln, dial: dial, clk: clk, logf: logf, ctx: ctx, cancel: cancel, conns: map[net.Conn]struct{}{}}
	if a, ok := ln.Addr().(*net.TCPAddr); ok {
		f.port = a.Port
	}
	f.wg.Add(1)
	go f.serve()
	return f
}

// Accept retry delays: the first after a failed accept, and the most
// it doubles to while accepts keep failing.
const (
	acceptRetryFirst = 5 * time.Millisecond
	acceptRetryMax   = time.Second
)

// Port is the bound port.
func (f *Forwarder) Port() int { return f.port }

// Addr is host:port.
func (f *Forwarder) Addr() string { return f.ln.Addr().String() }

// serve accepts until Close. A failed accept is retried after a delay
// that doubles while accepts keep failing, so one bad accept never
// ends the forward and a listener that keeps failing (no descriptors
// left) does not spin.
func (f *Forwarder) serve() {
	defer f.wg.Done()
	var delay time.Duration
	for {
		c, err := f.ln.Accept()
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return
			}
			if delay == 0 {
				delay = acceptRetryFirst
			} else if delay = 2 * delay; delay > acceptRetryMax {
				delay = acceptRetryMax
			}
			f.logf("forward: accept: %v; retrying in %v", err, delay)
			select {
			case <-f.ctx.Done():
				return
			case <-f.clk.After(delay):
			}
			continue
		}
		delay = 0
		f.track(c, true)
		f.wg.Add(1)
		go func() {
			defer f.wg.Done()
			defer f.track(c, false)
			ctx, cancel := context.WithTimeout(f.ctx, 15*time.Second)
			up, err := f.dial(ctx)
			cancel()
			if err != nil {
				f.logf("forward: %v", err)
				_ = c.Close()
				return
			}
			f.track(up, true)
			defer f.track(up, false)
			guestapi.Splice(c, nil, up)
		}()
	}
}

func (f *Forwarder) track(c net.Conn, add bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if add {
		f.conns[c] = struct{}{}
	} else {
		delete(f.conns, c)
	}
}

// Close stops listening, abandons dials in progress and drops every
// open connection, then waits for the connection goroutines. Callers
// must not hold a lock the dial function takes.
func (f *Forwarder) Close() {
	_ = f.ln.Close()
	f.cancel()
	f.mu.Lock()
	for c := range f.conns {
		_ = c.Close()
	}
	f.mu.Unlock()
	f.wg.Wait()
}
