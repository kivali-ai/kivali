package devshell

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/kivali-ai/kivali/internal/clock"
)

// sidecarConn is the HTTP-over-UDS connection to the daemon that
// SidecarShell and SidecarFiles share: one http.Client dialing the
// socket, and the startup retry both need because either can make the
// first call of a fresh pod.
type sidecarConn struct {
	socketPath   string
	clk          clock.Clock
	dialTimeout  time.Duration
	startupGrace time.Duration

	once  sync.Once
	httpC *http.Client
}

// newSidecarConn fills in the defaults: the real clock, a 5s dial
// timeout, and a 30s startup grace, which covers a slow sidecar image
// pull.
func newSidecarConn(socketPath string, clk clock.Clock, dialTimeout, startupGrace time.Duration) *sidecarConn {
	if clk == nil {
		clk = clock.New()
	}
	if dialTimeout <= 0 {
		dialTimeout = 5 * time.Second
	}
	if startupGrace <= 0 {
		startupGrace = 30 * time.Second
	}
	return &sidecarConn{
		socketPath:   socketPath,
		clk:          clk,
		dialTimeout:  dialTimeout,
		startupGrace: startupGrace,
	}
}

// do issues the request, retrying a failed dial during the startup
// grace window. The retry loop covers the common case where the agent
// container's first run_shell or file_* call fires before the
// dev-shell container has finished booting — the daemon's image pull
// alone can take 10–20s on a cold node.
//
// Only a dial is retried: until the connection exists the daemon
// cannot have seen the request. Any dial failure counts, whatever its
// errno, because the kernel's answer varies with platform and listen
// state ("invalid argument" on darwin when the path exists but isn't a
// socket, "no such file" on linux when the path is missing,
// "connection refused" once the path exists but nothing listens yet);
// a real configuration error (wrong socket path, wrong permissions)
// still fails after the grace expires. An error after the dial — the
// daemon died mid-call — is returned as it is: the daemon may already
// have applied the file edit or run the command, and sending it again
// would do it twice.
func (c *sidecarConn) do(req *http.Request) (*http.Response, error) {
	deadline := c.clk.Now().Add(c.startupGrace)
	backoff := 100 * time.Millisecond

	var lastErr error
	for {
		// Each retry needs a fresh body — http.Client.Do takes
		// ownership of req.Body and may close it even on a dial
		// failure. NewRequestWithContext with a *bytes.Reader
		// populates GetBody for exactly this case.
		if req.GetBody != nil {
			body, err := req.GetBody()
			if err != nil {
				return nil, fmt.Errorf("devshell: GetBody: %w", err)
			}
			req.Body = body
		}
		resp, err := c.httpClient().Do(req)
		if err == nil {
			return resp, nil
		}
		if !isDialError(err) {
			return nil, fmt.Errorf("devshell: %s: %w", c.socketPath, err)
		}
		lastErr = err
		if c.clk.Now().After(deadline) || req.Context().Err() != nil {
			return nil, fmt.Errorf("devshell: dial %s after %v: %w", c.socketPath, c.startupGrace, lastErr)
		}
		wait := c.clk.NewTimer(backoff)
		select {
		case <-req.Context().Done():
			wait.Stop()
			return nil, req.Context().Err()
		case <-wait.C():
		}
		if backoff < 2*time.Second {
			backoff *= 2
		}
	}
}

// isDialError reports whether err failed while connecting to the
// socket, before any byte of the request was sent.
func isDialError(err error) bool {
	var op *net.OpError
	return errors.As(err, &op) && op.Op == "dial"
}

func (c *sidecarConn) httpClient() *http.Client {
	c.once.Do(func() {
		dial := func(ctx context.Context, _, _ string) (net.Conn, error) {
			d := net.Dialer{Timeout: c.dialTimeout}
			return d.DialContext(ctx, "unix", c.socketPath)
		}
		c.httpC = &http.Client{
			Transport: &http.Transport{
				DialContext:           dial,
				IdleConnTimeout:       30 * time.Second,
				ResponseHeaderTimeout: 0, // commands can take up to MaxTimeout (10 min)
			},
			// No top-level Timeout — per-request timeouts ride on ctx.
		}
	})
	return c.httpC
}
