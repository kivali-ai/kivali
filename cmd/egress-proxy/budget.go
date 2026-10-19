package main

import (
	"errors"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"github.com/kivali-ai/kivali/internal/clock"
)

// budgetRefusal is the text a request gets when its agent is held.
const budgetRefusal = "this agent sent more data out than its budget allows"

// errOverBudget closes a connection that passed the per-connection cap
// while the budget is enforced.
var errOverBudget = errors.New(budgetRefusal)

// budgetLimits are the byte caps on what an agent sends out in open
// mode, outside the model hosts.
type budgetLimits struct {
	Conn   int64 // one connection
	TenMin int64 // rolling 10 minutes
	Day    int64 // rolling 24 hours
}

// budget counts the bytes each agent sends out through open-mode
// connections: hostnames, and every byte written upstream as it is
// written. Crossing a limit is logged once per crossing. With enforce
// set, an agent that crosses a window limit is held, and a held agent's
// new open-mode connections are refused. Holds live in memory only.
type budget struct {
	clk     clock.Clock
	limits  budgetLimits
	enforce bool
	logf    func(string, ...any)

	mu     sync.Mutex
	agents map[string]*agentUsage
}

type agentUsage struct {
	tenMin window
	day    window
	// held is set when a window limit is crossed with enforce on and
	// stays set for the life of the process.
	held bool
}

func newBudget(clk clock.Clock, limits budgetLimits, enforce bool, logf func(string, ...any)) *budget {
	return &budget{clk: clk, limits: limits, enforce: enforce, logf: logf, agents: map[string]*agentUsage{}}
}

func (b *budget) usage(slug string) *agentUsage {
	u, ok := b.agents[slug]
	if !ok {
		u = &agentUsage{
			tenMin: newWindow(time.Minute, 10),
			day:    newWindow(time.Hour, 24),
		}
		b.agents[slug] = u
	}
	return u
}

// Held reports whether slug's open-mode connections are refused.
func (b *budget) Held(slug string) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	u, ok := b.agents[slug]
	return ok && u.held
}

// Add counts n bytes slug sent toward host and reports whether slug is
// held afterwards.
func (b *budget) Add(slug, host string, n int64) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	now := b.clk.Now()
	u := b.usage(slug)
	b.addTo(&u.tenMin, now, n, slug, host, "10m", b.limits.TenMin, u)
	b.addTo(&u.day, now, n, slug, host, "day", b.limits.Day, u)
	return u.held
}

// Usage returns slug's rolling totals.
func (b *budget) Usage(slug string) (tenMin, day int64) {
	b.mu.Lock()
	defer b.mu.Unlock()
	now := b.clk.Now()
	u := b.usage(slug)
	return u.tenMin.sum(now), u.day.sum(now)
}

// addTo adds n to w, logging when that takes w's total from at or
// under limit to over it. Enforced, the crossing holds the agent.
func (b *budget) addTo(w *window, now time.Time, n int64, slug, host, name string, limit int64, u *agentUsage) {
	before := w.sum(now)
	w.add(now, n)
	after := before + n
	if before > limit || after <= limit {
		return
	}
	b.logf("OVER agent=%s limit=%s bytes=%d host=%s enforced=%t", slug, name, after, host, b.enforce)
	if b.enforce {
		u.held = true
	}
}

// window is a rolling total kept in fixed-width buckets: the current
// bucket plus the len-1 before it.
type window struct {
	width   time.Duration
	buckets []bucket
}

type bucket struct {
	idx int64
	n   int64
}

func newWindow(width time.Duration, n int) window {
	return window{width: width, buckets: make([]bucket, n)}
}

func (w *window) index(now time.Time) int64 {
	return now.UnixNano() / int64(w.width)
}

func (w *window) add(now time.Time, n int64) {
	idx := w.index(now)
	b := &w.buckets[idx%int64(len(w.buckets))]
	if b.idx != idx {
		*b = bucket{idx: idx}
	}
	b.n += n
}

func (w *window) sum(now time.Time) int64 {
	idx := w.index(now)
	var total int64
	for _, b := range w.buckets {
		if b.idx <= idx && b.idx > idx-int64(len(w.buckets)) {
			total += b.n
		}
	}
	return total
}

// meteredConn is an upstream connection that tallies bytes each way
// for the close log. When counted, each write upstream is added to the
// agent's budget as it happens and checked against the per-connection
// cap.
type meteredConn struct {
	net.Conn
	budget  *budget // nil when not counted
	slug    string
	host    string
	up      atomic.Int64
	down    atomic.Int64
	connOut bool // the per-connection crossing has been logged
	// closed is set when the connection was closed for passing the
	// per-connection cap.
	closed atomic.Bool
}

// Write passes p upstream. Counted and enforced, a write that would
// take the connection past the per-connection cap sends only the bytes
// up to the cap, then closes the connection.
func (c *meteredConn) Write(p []byte) (int, error) {
	if c.closed.Load() {
		return 0, errOverBudget
	}
	if c.budget == nil || len(p) == 0 {
		n, err := c.Conn.Write(p)
		c.up.Add(int64(n))
		return n, err
	}
	sent := c.up.Load()
	c.budget.Add(c.slug, c.host, int64(len(p)))
	if total := sent + int64(len(p)); total > c.budget.limits.Conn && !c.connOut {
		c.connOut = true
		c.budget.logf("OVER agent=%s limit=conn bytes=%d host=%s enforced=%t", c.slug, total, c.host, c.budget.enforce)
	}
	if !c.connOut || !c.budget.enforce {
		n, err := c.Conn.Write(p)
		c.up.Add(int64(n))
		return n, err
	}
	allowed := max(c.budget.limits.Conn-sent, 0)
	var n int
	if allowed > 0 {
		var err error
		n, err = c.Conn.Write(p[:allowed])
		c.up.Add(int64(n))
		if err != nil {
			return n, err
		}
	}
	c.closed.Store(true)
	_ = c.Close()
	return n, errOverBudget
}

func (c *meteredConn) Read(p []byte) (int, error) {
	n, err := c.Conn.Read(p)
	c.down.Add(int64(n))
	return n, err
}
