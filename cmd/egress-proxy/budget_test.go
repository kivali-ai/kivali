package main

import (
	"fmt"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kivali-ai/kivali/internal/clock"
)

// logSink collects log lines and hands each to anyone waiting.
type logSink struct {
	mu    sync.Mutex
	lines []string
	ch    chan string
}

func newLogSink() *logSink { return &logSink{ch: make(chan string, 1024)} }

func (l *logSink) logf(format string, args ...any) {
	line := fmt.Sprintf(format, args...)
	l.mu.Lock()
	l.lines = append(l.lines, line)
	l.mu.Unlock()
	l.ch <- line
}

func (l *logSink) all() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]string(nil), l.lines...)
}

func (l *logSink) matching(prefix string) []string {
	var out []string
	for _, line := range l.all() {
		if strings.HasPrefix(line, prefix) {
			out = append(out, line)
		}
	}
	return out
}

// next waits for the next line starting with prefix. The timeout only
// turns a hang into a failure; lines arrive as events.
func (l *logSink) next(t *testing.T, prefix string) string {
	t.Helper()
	timeout := time.NewTimer(10 * time.Second)
	defer timeout.Stop()
	for {
		select {
		case line := <-l.ch:
			if strings.HasPrefix(line, prefix) {
				return line
			}
		case <-timeout.C:
			t.Fatalf("no %q line; have %q", prefix, l.all())
			return ""
		}
	}
}

func TestBudgetWindowsRollOver(t *testing.T) {
	clk := clock.NewFake()
	logs := newLogSink()
	b := newBudget(clk, budgetLimits{Conn: 1 << 30, TenMin: 1000, Day: 5000}, false, logs.logf)

	b.Add("alice", "a.test:443", 600)
	clk.Advance(5 * time.Minute)
	b.Add("alice", "a.test:443", 300)
	if ten, day := b.Usage("alice"); ten != 900 || day != 900 {
		t.Fatalf("usage = %d, %d; want 900, 900", ten, day)
	}
	if over := logs.matching("OVER"); len(over) != 0 {
		t.Fatalf("logged under the limit: %v", over)
	}
	b.Add("alice", "b.test:443", 200)
	b.Add("alice", "b.test:443", 50) // still over: no second line
	if over := logs.matching("OVER"); len(over) != 1 ||
		over[0] != "OVER agent=alice limit=10m bytes=1100 host=b.test:443 enforced=false" {
		t.Fatalf("OVER lines = %q", over)
	}

	// Ten minutes after the first add, its bucket has left the window.
	clk.Advance(5 * time.Minute)
	if ten, day := b.Usage("alice"); ten != 550 || day != 1150 {
		t.Fatalf("after roll-over usage = %d, %d; want 550, 1150", ten, day)
	}
	// Under again, so the next crossing logs again.
	b.Add("alice", "c.test:443", 500)
	if over := logs.matching("OVER"); len(over) != 2 || !strings.Contains(over[1], "limit=10m bytes=1050 host=c.test:443") {
		t.Fatalf("second crossing: %q", over)
	}
	if b.Held("alice") {
		t.Fatal("held without enforcement")
	}

	// The day window: 24 hourly buckets.
	for i := 0; i < 6; i++ {
		clk.Advance(time.Hour)
		b.Add("alice", "d.test:443", 700)
	}
	if over := logs.matching("OVER agent=alice limit=day"); len(over) != 1 {
		t.Fatalf("day crossing: %q", logs.all())
	}
	clk.Advance(24 * time.Hour)
	if ten, day := b.Usage("alice"); ten != 0 || day != 0 {
		t.Fatalf("a day later usage = %d, %d; want 0, 0", ten, day)
	}

	// Agents are counted apart.
	if ten, _ := b.Usage("bob"); ten != 0 {
		t.Fatalf("bob counted alice's bytes: %d", ten)
	}
}

func TestBudgetEnforcementHolds(t *testing.T) {
	clk := clock.NewFake()
	logs := newLogSink()
	b := newBudget(clk, budgetLimits{Conn: 1 << 30, TenMin: 100, Day: 1 << 30}, true, logs.logf)
	if b.Add("alice", "a.test:443", 100) {
		t.Fatal("held at the limit")
	}
	if !b.Add("alice", "a.test:443", 1) || !b.Held("alice") {
		t.Fatal("not held over the limit")
	}
	if got := logs.matching("OVER"); len(got) != 1 || !strings.HasSuffix(got[0], "enforced=true") {
		t.Fatalf("OVER lines = %q", got)
	}
	// The hold outlasts the window.
	clk.Advance(time.Hour)
	if !b.Held("alice") {
		t.Fatal("hold lifted by time")
	}
	if b.Held("bob") {
		t.Fatal("bob held for alice's bytes")
	}
}

// discardConn accepts every write.
type discardConn struct{ net.Conn }

func (discardConn) Write(p []byte) (int, error) { return len(p), nil }
func (discardConn) Close() error                { return nil }

func TestMeteredConnPerConnectionCap(t *testing.T) {
	for _, enforce := range []bool{false, true} {
		t.Run(fmt.Sprintf("enforce=%v", enforce), func(t *testing.T) {
			logs := newLogSink()
			b := newBudget(clock.NewFake(), budgetLimits{Conn: 100, TenMin: 1 << 30, Day: 1 << 30}, enforce, logs.logf)
			c := &meteredConn{Conn: discardConn{}, budget: b, slug: "alice", host: "a.test:443"}
			if _, err := c.Write(make([]byte, 60)); err != nil {
				t.Fatal(err)
			}
			if ten, _ := b.Usage("alice"); ten != 60 {
				t.Fatalf("counted %d live, want 60", ten)
			}
			_, err := c.Write(make([]byte, 50))
			if enforce != (err != nil) {
				t.Fatalf("second write err=%v with enforce=%v", err, enforce)
			}
			want := fmt.Sprintf("OVER agent=alice limit=conn bytes=110 host=a.test:443 enforced=%t", enforce)
			if got := logs.matching("OVER"); len(got) != 1 || got[0] != want {
				t.Fatalf("OVER lines = %q, want %q", got, want)
			}
			wantUp := int64(110)
			if enforce {
				wantUp = 100 // sent up to the cap, then closed
			}
			if got := c.closed.Load(); got != enforce {
				t.Fatalf("closed = %v with enforce=%v", got, enforce)
			}
			if got := c.up.Load(); got != wantUp {
				t.Fatalf("up = %d, want %d", got, wantUp)
			}
		})
	}
}

// A connection that is not counted tallies for the log but never
// touches a budget.
func TestMeteredConnUncounted(t *testing.T) {
	c := &meteredConn{Conn: discardConn{}, slug: "alice", host: "api.anthropic.com:443"}
	if _, err := c.Write(make([]byte, 1<<20)); err != nil {
		t.Fatal(err)
	}
	if c.up.Load() != 1<<20 {
		t.Fatalf("up = %d", c.up.Load())
	}
}
