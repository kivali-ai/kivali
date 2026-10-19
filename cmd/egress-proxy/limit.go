package main

import (
	"net"
	"sync"
	"time"
)

// sourceRefusal is written to a connection refused for its source
// holding too many connections already.
const sourceRefusal = "HTTP/1.1 503 Service Unavailable\r\n" +
	"Content-Type: text/plain; charset=utf-8\r\n" +
	"Connection: close\r\n" +
	"Content-Length: 39\r\n\r\n" +
	"too many connections from this address\n"

// refusalWriteTimeout bounds the time spent telling a refused client
// why, so a client that never reads cannot hold a goroutine.
const refusalWriteTimeout = 5 * time.Second

// sourceLimitListener caps the connections open at once from each
// source IP. A cap per source rather than overall keeps one pod from
// starving every other agent. A connection over the cap gets a 503 and
// is closed; Accept moves on to the next.
type sourceLimitListener struct {
	net.Listener
	max int
	// logf reports a refusal. Nil means silent.
	logf func(string, ...any)

	mu   sync.Mutex
	open map[string]int
}

func newSourceLimitListener(ln net.Listener, max int, logf func(string, ...any)) *sourceLimitListener {
	return &sourceLimitListener{Listener: ln, max: max, logf: logf, open: map[string]int{}}
}

func (l *sourceLimitListener) Accept() (net.Conn, error) {
	for {
		c, err := l.Listener.Accept()
		if err != nil {
			return nil, err
		}
		src := sourceIP(c.RemoteAddr())
		if l.acquire(src) {
			return &limitedConn{Conn: c, release: func() { l.release(src) }}, nil
		}
		if l.logf != nil {
			l.logf("DENY source=%s reason=connections", src)
		}
		go func() {
			_ = c.SetWriteDeadline(time.Now().Add(refusalWriteTimeout))
			_, _ = c.Write([]byte(sourceRefusal))
			_ = c.Close()
		}()
	}
}

func (l *sourceLimitListener) acquire(src string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.max > 0 && l.open[src] >= l.max {
		return false
	}
	l.open[src]++
	return true
}

func (l *sourceLimitListener) release(src string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.open[src] <= 1 {
		delete(l.open, src)
		return
	}
	l.open[src]--
}

// openFrom is the number of connections open from src.
func (l *sourceLimitListener) openFrom(src string) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.open[src]
}

// sourceIP is addr's IP without the port; the whole address when it
// has none.
func sourceIP(addr net.Addr) string {
	if addr == nil {
		return ""
	}
	s := addr.String()
	if host, _, err := net.SplitHostPort(s); err == nil {
		return host
	}
	return s
}

// limitedConn gives its slot back on its first Close. A hijacked
// tunnel is still this conn, so its slot is held until the tunnel
// closes.
type limitedConn struct {
	net.Conn
	once    sync.Once
	release func()
}

func (c *limitedConn) Close() error {
	err := c.Conn.Close()
	c.once.Do(c.release)
	return err
}
