package agentpod

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/kivali-ai/kivali/internal/egresscred"
)

// EgressCredentialPath is the core.sock route an agent runtime
// registers its egress proxy credential on. The calling agent is the
// SlugHeader; the body is EgressCredentialRequest.
const EgressCredentialPath = "/v1/egress/credential"

// EgressCredentialRequest is the body of a registration: the SHA-256
// of the runtime's secret (egresscred.Hash), never the secret.
type EgressCredentialRequest struct {
	SHA256 string `json:"sha256"`
}

// ErrEgressCredentialRefused is a registration core answered 400: the
// hash is malformed, which no retry changes.
var ErrEgressCredentialRefused = errors.New("agentpod: core refused the egress credential")

// WithScrubber has every request c sends to core scrubbed of the
// credential s knows (egresscred.Scrubber.RoundTripper): stream
// events, tool results, error strings and everything else the runtime
// relays. Must be called before the first request. A nil s changes
// nothing.
func (c *Client) WithScrubber(s *egresscred.Scrubber) *Client {
	if s != nil {
		c.http.Transport = s.RoundTripper(c.http.Transport)
	}
	return c
}

// RegisterEgressCredential tells core the hash of this runtime's
// egress proxy secret, so the proxy lets the agent processes that
// present it reach the public internet. A failed attempt (core not up
// yet, a dropped socket, any answer but 204 and 400) is retried with
// backoff on c's clock until core accepts or ctx ends. A 400 returns
// ErrEgressCredentialRefused at once.
func (c *Client) RegisterEgressCredential(ctx context.Context, sha256Hex string) error {
	const minBackoff = 200 * time.Millisecond
	const maxBackoff = 10 * time.Second
	body, err := json.Marshal(EgressCredentialRequest{SHA256: sha256Hex})
	if err != nil {
		return err
	}
	backoff := minBackoff
	for {
		err := c.postEgressCredential(ctx, body)
		if err == nil || errors.Is(err, ErrEgressCredentialRefused) {
			return err
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		wait := c.clk.NewTimer(backoff)
		select {
		case <-ctx.Done():
			wait.Stop()
			return ctx.Err()
		case <-wait.C():
		}
		backoff *= 2
		if backoff > maxBackoff {
			backoff = maxBackoff
		}
	}
}

// reregisterEgressCredential sends the accepted hash again, once. A
// failure is logged; the next reconnect tries again.
func (c *Client) reregisterEgressCredential(ctx context.Context) {
	if c.egressHash == "" {
		return
	}
	body, err := json.Marshal(EgressCredentialRequest{SHA256: c.egressHash})
	if err == nil {
		err = c.postEgressCredential(ctx, body)
	}
	if err != nil && ctx.Err() == nil {
		log.Printf("agentpod: register the egress credential again: %v", err)
	}
}

func (c *Client) postEgressCredential(ctx context.Context, body []byte) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://core"+EgressCredentialPath, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(SlugHeader, c.slug)
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("agentpod: register egress credential: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	msg, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	switch resp.StatusCode {
	case http.StatusNoContent:
		return nil
	case http.StatusBadRequest:
		return fmt.Errorf("%w: %s", ErrEgressCredentialRefused, strings.TrimSpace(string(msg)))
	default:
		return fmt.Errorf("agentpod: register egress credential: HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(msg)))
	}
}

// SetUpEgressCredential gives this runtime its egress proxy
// credential: it draws a secret, scrubs it from everything c sends,
// and registers its hash with core, waiting until core accepts. c
// registers it again each time RunEvents reconnects. The returned
// credential's Env is what every agent process the runtime starts
// gets; the secret exists nowhere else.
//
// Open egress for those processes is safe because no tool in the agent
// container sends arbitrary headers or bodies: the CLI's built-ins are
// WebFetch (GET only) and WebSearch (run server-side inside the model
// call), cliBuiltinNames (internal/claudeagent) maps no shell, and
// model-written commands run only in the agent-shell container. So the
// model hosts the proxy leaves out of the upload count carry nothing
// out of the agent container but the model calls themselves.
//
// bareProxyURL is the pod's own proxy URL (HTTPS_PROXY in the agent
// container's environment, an IP with no credentials). Empty means
// the pod has no proxy: it returns nil and nothing is registered.
func SetUpEgressCredential(ctx context.Context, c *Client, bareProxyURL string) (*egresscred.Credential, error) {
	if strings.TrimSpace(bareProxyURL) == "" {
		return nil, nil
	}
	cred, err := egresscred.New(bareProxyURL, c.Slug())
	if err != nil {
		return nil, err
	}
	c.WithScrubber(cred.Scrubber)
	if err := c.RegisterEgressCredential(ctx, cred.Hash); err != nil {
		return nil, err
	}
	c.egressHash = cred.Hash
	return cred, nil
}
