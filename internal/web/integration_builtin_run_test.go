package web

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/kivali-ai/kivali/internal/clock"
)

// Why this test exists: a built-in's function runs in core's process.
// One that never returns must not hold an agent's call (or the owner's
// Test connection) past the deadline a pod's request has, and must be
// told to stop; one that panics must fail the call, not take the
// server and every agent down with it.
func TestRunBuiltinDeadlineAndPanic(t *testing.T) {
	clk := clock.NewFake()
	stopped := make(chan struct{})
	type answer struct {
		v   string
		err error
	}
	got := make(chan answer, 1)
	go func() {
		v, err := runBuiltin(context.Background(), clk, integrationCallTimeout, func(ctx context.Context) (string, error) {
			<-ctx.Done()
			close(stopped)
			return "late", nil
		})
		got <- answer{v, err}
	}()
	clk.BlockUntil(1)
	clk.Advance(integrationCallTimeout)
	a := <-got
	if !errors.Is(a.err, errIntegrationTimeout) || a.v != "" {
		t.Errorf("past the deadline = %q, %v", a.v, a.err)
	}
	<-stopped

	_, err := runBuiltin(context.Background(), clk, integrationCallTimeout, func(context.Context) (string, error) {
		panic("boom")
	})
	if err == nil || !strings.Contains(err.Error(), "boom") {
		t.Errorf("a panic = %v", err)
	}
	v, err := runBuiltin(context.Background(), clk, integrationCallTimeout, func(context.Context) (string, error) { return "ok", nil })
	if v != "ok" || err != nil {
		t.Errorf("an answer = %q, %v", v, err)
	}
}
