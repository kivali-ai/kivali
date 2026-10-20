package broker

import (
	"context"
	"net"
	"time"

	"github.com/kivali-ai/kivali/internal/clock"
)

// dialWhenListening calls dial until it connects or fails for a reason
// other than missing (the pipe does not exist yet), waiting retry
// between tries, for up to wait after the first. It answers the last
// dial's result, or, when ctx ends while it waits, the missing pipe's
// error.
func dialWhenListening(ctx context.Context, clk clock.Clock, wait, retry time.Duration, dial func(context.Context) (net.Conn, error), missing func(error) bool) (net.Conn, error) {
	deadline := clk.Now().Add(wait)
	for {
		c, err := dial(ctx)
		if err == nil || !missing(err) || !clk.Now().Before(deadline) {
			return c, err
		}
		t := clk.NewTimer(retry)
		select {
		case <-ctx.Done():
			t.Stop()
			return nil, err
		case <-t.C():
		}
	}
}
