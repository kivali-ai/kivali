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

// allowlistPollInterval is how often awaitConfig looks for the files.
const allowlistPollInterval = 250 * time.Millisecond

// awaitConfig waits until the allowlist file exists, looking every
// allowlistPollInterval, for at most bound on clk. It returns true once
// it does, and false when the agents file turns up first or bound runs
// out. A file that cannot be checked for some other reason (no
// permission, say) counts as there: Load reports that error.
//
// The proxy runs it before it listens. The server writes the allowlist
// into a volume that is empty when the pod starts, and the egress
// Service publishes the pod before it is ready, so a proxy listening
// before that write would refuse every host to the agent shells
// already waiting for it; one installing its software at start would
// then go without it.
//
// The server writes the agents file right after the allowlist, even
// when it could not write the allowlist. Seeing it alone means no
// allowlist is coming, and the proxy listens without one rather than
// keep agents' credentialed traffic waiting for the rest of bound.
func awaitConfig(allowlist, agents string, bound time.Duration, clk clock.Clock) bool {
	start := clk.Now()
	for {
		if exists(allowlist) {
			return true
		}
		if exists(agents) || clock.Since(clk, start) >= bound {
			// Written just after the agents file, the allowlist is
			// here and better than none.
			return exists(allowlist)
		}
		t := clk.NewTimer(allowlistPollInterval)
		<-t.C()
	}
}

// exists reports whether path is anything but missing.
func exists(path string) bool {
	_, err := os.Stat(path)
	return err == nil || !errors.Is(err, fs.ErrNotExist)
}
