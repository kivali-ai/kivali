package claudeagent

import (
	"context"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/kivali-ai/kivali/internal/claudeauth"
	"github.com/kivali-ai/kivali/internal/clock"
)

// WatchModelHosts reports the hosts the CLI's model calls reach
// (claudeauth.ModelHosts) for its current sign-in: once at start, then
// each time they change, until ctx ends. Every interval it checks the
// files a sign-in writes under HOME (.claude/.credentials.json,
// .claude/settings.json, .aws/config) and asks the CLI again only when
// one of them changed. A host left out for want of a region or resource
// is logged. Blocks; run it on its own goroutine.
func (c *Driver) WatchModelHosts(ctx context.Context, interval time.Duration, report func(hosts []string)) {
	w := newModelHostWatch(c.opts.ClaudeBinary, c.opts.HomeDir, c.opts.clock)
	w.run(ctx, interval, report)
}

// modelHostWatch is WatchModelHosts' state: the sign-in files' last
// stamp and the hosts last reported.
type modelHostWatch struct {
	home   string
	clk    clock.Clock
	status func(ctx context.Context) (claudeauth.Status, error)
	logf   func(format string, args ...any)

	stamp    string
	reported bool
	hosts    []string
}

func newModelHostWatch(binary, home string, clk clock.Clock) *modelHostWatch {
	if binary == "" {
		binary = "claude"
	}
	if clk == nil {
		clk = clock.New()
	}
	return &modelHostWatch{
		home:   home,
		clk:    clk,
		status: func(ctx context.Context) (claudeauth.Status, error) { return authStatus(ctx, binary, nil, nil) },
		logf:   log.Printf,
	}
}

func (w *modelHostWatch) run(ctx context.Context, interval time.Duration, report func([]string)) {
	w.check(ctx, report)
	t := w.clk.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C():
			w.check(ctx, report)
		}
	}
}

// check works the hosts out again when the sign-in files changed since
// the last check, and reports them when they differ from the last
// report. A status the CLI cannot give is logged and asked again at the
// next check.
func (w *modelHostWatch) check(ctx context.Context, report func([]string)) {
	stamp := w.filesStamp()
	if w.reported && stamp == w.stamp {
		return
	}
	st, err := w.status(ctx)
	if err != nil {
		w.logf("egress: model hosts: sign-in check: %v", err)
		return
	}
	w.stamp = stamp
	settings, _ := os.ReadFile(filepath.Join(w.home, ".claude", "settings.json"))
	awsConfig, _ := os.ReadFile(filepath.Join(w.home, ".aws", "config"))
	hosts, missing := claudeauth.ModelHosts(st, settings, awsConfig)
	if len(missing) > 0 {
		w.logf("egress: model hosts: left out, so counted as uploads: %s", strings.Join(missing, "; "))
	}
	if w.reported && slices.Equal(hosts, w.hosts) {
		return
	}
	w.reported, w.hosts = true, hosts
	report(hosts)
}

// filesStamp is the size and modification time of each file a sign-in
// writes ("-" for a missing one). Empty home stamps nothing, so the
// hosts are worked out once.
func (w *modelHostWatch) filesStamp() string {
	if w.home == "" {
		return ""
	}
	var b strings.Builder
	for _, p := range []string{
		filepath.Join(w.home, ".claude", ".credentials.json"),
		filepath.Join(w.home, ".claude", "settings.json"),
		filepath.Join(w.home, ".aws", "config"),
	} {
		fi, err := os.Stat(p)
		if err != nil {
			b.WriteString("-;")
			continue
		}
		fmt.Fprintf(&b, "%d@%d;", fi.Size(), fi.ModTime().UnixNano())
	}
	return b.String()
}
