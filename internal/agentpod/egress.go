package agentpod

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/kivali-ai/kivali/internal/agentshell"
	"github.com/kivali-ai/kivali/internal/egresscred"
)

// EgressCredentialPath is the core.sock route an agent runtime
// registers its egress proxy credentials on. The calling agent is the
// SlugHeader; the body is EgressCredentialRequest.
const EgressCredentialPath = "/v1/egress/credential"

// EgressCredentialRequest is the body of a registration: the SHA-256
// of each of the runtime's secrets (egresscred.Hash), never a secret.
type EgressCredentialRequest struct {
	// SHA256 is the hash of the runtime's own secret, which the agent
	// processes it starts present.
	SHA256 string `json:"sha256"`
	// ShellSHA256 is the hash of the secret the runtime hands the
	// agent's shell. Empty when the pod has no shell to hand one to.
	ShellSHA256 string `json:"shell_sha256,omitempty"`
}

// ErrEgressCredentialRefused is a registration core answered 400: the
// hash is malformed, which no retry changes.
var ErrEgressCredentialRefused = errors.New("agentpod: core refused the egress credential")

// WithScrubber has every request c sends to core, on both its
// transports, scrubbed of the credential s knows
// (egresscred.Scrubber.RoundTripper): stream events, tool results,
// web commands, result submissions, error strings and everything else
// the runtime relays. Must be called before the first request. A nil
// s changes nothing.
func (c *Client) WithScrubber(s *egresscred.Scrubber) *Client {
	if s != nil {
		c.http.Transport = s.RoundTripper(c.http.Transport)
		c.long.Transport = s.RoundTripper(c.long.Transport)
	}
	return c
}

// RegisterEgressCredential tells core the hashes of this runtime's
// egress proxy secrets, so the proxy lets the agent processes that
// present the first reach the public internet, and the agent's shell,
// presenting the second, reach its allowance. A failed attempt (core
// not up yet, a dropped socket, any answer but 204 and 400) is retried
// with backoff on c's clock until core accepts or ctx ends. A 400
// returns ErrEgressCredentialRefused at once.
func (c *Client) RegisterEgressCredential(ctx context.Context, reg EgressCredentialRequest) error {
	const minBackoff = 200 * time.Millisecond
	const maxBackoff = 10 * time.Second
	body, err := json.Marshal(reg)
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

// reregisterEgressCredential sends the accepted hashes again, once. A
// failure is logged; the next reconnect tries again.
func (c *Client) reregisterEgressCredential(ctx context.Context) {
	if c.egressReg.SHA256 == "" {
		return
	}
	body, err := json.Marshal(c.egressReg)
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
// credentials: it draws a secret for the agent processes it starts and
// one for the agent's shell, scrubs both from everything c sends, and
// registers their hashes with core, waiting until core accepts. c
// registers them again each time RunEvents reconnects. The returned
// credential's Env is what every agent process the runtime starts
// gets, and its Scrubber scrubs both secrets; the first secret exists
// nowhere else.
//
// The shell's credential grants only what the shell may reach without
// it, the team's list, plus the agent's own hosts, and lets the proxy
// tell this agent's shell from others. Once core has its hash it is
// written, as a credentialed proxy URL, to the agentshell
// ProxyCredentialFile beside shellSocket, where the agent-shell daemon
// reads it for each command; a file an earlier runtime in this pod
// left is removed first. Empty shellSocket means the pod has no shell:
// no shell secret is drawn. A failed write is logged, and the shell's
// commands keep the bare proxy URL.
//
// Open egress for the agent processes is safe because no tool in the
// agent container sends arbitrary headers or bodies: the CLI's
// built-ins are WebFetch (GET only) and WebSearch (run server-side
// inside the model call), cliBuiltinNames (internal/claudeagent) maps
// no shell, and model-written commands run only in the agent-shell
// container. So the model hosts the proxy leaves out of the upload
// count carry nothing out of the agent container but the model calls
// themselves.
//
// bareProxyURL is the pod's own proxy URL (HTTPS_PROXY in the agent
// container's environment, an IP with no credentials). Empty means
// the pod has no proxy: it returns nil and nothing is registered.
func SetUpEgressCredential(ctx context.Context, c *Client, bareProxyURL, shellSocket string) (*egresscred.Credential, error) {
	if strings.TrimSpace(bareProxyURL) == "" {
		return nil, nil
	}
	cred, err := egresscred.New(bareProxyURL, c.Slug())
	if err != nil {
		return nil, err
	}
	reg := EgressCredentialRequest{SHA256: cred.Hash}
	var shell *egresscred.Credential
	var shellFile string
	if shellSocket != "" {
		if shell, err = egresscred.New(bareProxyURL, c.Slug()); err != nil {
			return nil, err
		}
		reg.ShellSHA256 = shell.Hash
		cred.Scrubber = egresscred.Combine(cred.Scrubber, shell.Scrubber)
		shellFile = filepath.Join(filepath.Dir(shellSocket), agentshell.ProxyCredentialFile)
		if err := os.Remove(shellFile); err != nil && !errors.Is(err, fs.ErrNotExist) {
			log.Printf("agentpod: remove the shell's earlier egress credential: %v", err)
		}
	}
	c.WithScrubber(cred.Scrubber)
	if err := c.RegisterEgressCredential(ctx, reg); err != nil {
		return nil, err
	}
	c.egressReg = reg
	if shell != nil {
		if err := writeShellCredential(shellFile, shell.URL); err != nil {
			log.Printf("agentpod: hand the shell its egress credential: %v; its commands keep the bare proxy URL", err)
		}
	}
	return cred, nil
}

// writeShellCredential writes proxyURL, one line, to path: a temporary
// file in the same directory, 0600, renamed into place, so the daemon
// never reads half of it.
func writeShellCredential(path, proxyURL string) error {
	f, err := os.CreateTemp(filepath.Dir(path), "."+agentshell.ProxyCredentialFile+"-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer func() { _ = os.Remove(tmp) }()
	if err := f.Chmod(0o600); err != nil {
		_ = f.Close()
		return err
	}
	if _, err := f.WriteString(proxyURL + "\n"); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
