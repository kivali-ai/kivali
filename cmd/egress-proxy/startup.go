package main

import (
	"errors"
	"io/fs"
	"os"
	"time"

	"github.com/kivali-ai/kivali/internal/clock"
)

// defaultAllowlistWait is --allowlist-wait's default: as long as an
// agent shell's install waits for the proxy to answer.
const defaultAllowlistWait = 2 * time.Minute

// allowlistPollInterval is how often awaitFile looks for the file.
const allowlistPollInterval = 250 * time.Millisecond

// awaitFile waits until path exists, looking every
// allowlistPollInterval, for at most bound on clk, and reports whether
// it does. A file that cannot be checked for some other reason (no
// permission, say) counts as there: Load reports that error.
//
// The proxy runs it before it listens. The server writes the allowlist
// into a volume that is empty when the pod starts, and the egress
// Service publishes the pod before it is ready, so a proxy listening
// before that write would refuse every host to the agent shells
// already waiting for it; one installing its software at start would
// then go without it.
func awaitFile(path string, bound time.Duration, clk clock.Clock) bool {
	start := clk.Now()
	for {
		if _, err := os.Stat(path); err == nil || !errors.Is(err, fs.ErrNotExist) {
			return true
		}
		if clock.Since(clk, start) >= bound {
			return false
		}
		t := clk.NewTimer(allowlistPollInterval)
		<-t.C()
	}
}
