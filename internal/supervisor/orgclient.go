package supervisor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"strings"

	"github.com/kivali-ai/kivali/internal/auth"
)

// orgBase is the address the supervisor calls the org's server at. The
// connection goes through the guest agent's proxy to the NodePort, so
// the host part is only what the server reads: loopback, which the
// handoff requires and the session cookie is issued for.
const orgBase = "http://127.0.0.1"

// orgClient is signed in to the org's own server as its owner, so the
// supervisor backs up and restores through the app's endpoints, the
// same code a person's click runs.
type orgClient struct {
	http *http.Client
}

// signIn mints a handoff token (see Handoff) and trades it for a
// session on the org's server, over the guest's proxy to the NodePort.
func (s *Supervisor) signIn(ctx context.Context) (*orgClient, error) {
	g, err := s.running()
	if err != nil {
		return nil, err
	}
	h, err := s.Handoff(ctx)
	if err != nil {
		return nil, err
	}
	jar, err := cookiejar.New(nil)
	if err != nil {
		return nil, err
	}
	c := &http.Client{
		Jar: jar,
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				return g.Proxy(ctx, NodePort)
			},
			// One connection per request: each is a fresh proxy stream.
			DisableKeepAlives: true,
		},
		// The handoff answers with a redirect to the app; the cookie it
		// sets is all that is wanted.
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, orgBase+auth.HandoffPath+"?t="+url.QueryEscape(h.Token), nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.Do(req)
	if err != nil {
		return nil, fmt.Errorf("sign in to the org: %w", err)
	}
	_ = resp.Body.Close()
	base, _ := url.Parse(orgBase)
	if len(jar.Cookies(base)) == 0 {
		return nil, errors.New("sign in to the org: the server did not accept the supervisor's handoff (is the owner still allowed to sign in?)")
	}
	return &orgClient{http: c}, nil
}

// post sends body to path as the app's own page would.
func (c *orgClient) post(ctx context.Context, path, contentType string, body io.Reader) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, orgBase+path, body)
	if err != nil {
		return nil, err
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	req.Header.Set("Origin", orgBase)
	return c.http.Do(req)
}

// orgError reads the server's JSON error, {"error", "who"}, from a
// response that is not a success.
func orgError(what string, resp *http.Response) error {
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	var e struct {
		Error string `json:"error"`
	}
	msg := strings.TrimSpace(string(b))
	if json.Unmarshal(b, &e) == nil && e.Error != "" {
		msg = e.Error
	}
	if msg == "" {
		msg = resp.Status
	}
	return fmt.Errorf("%s: %s", what, msg)
}
