package connect

// discover.go — finding a vendor's authorization server for a client
// core registers itself (a manifest's client: registered): the
// protected resource's metadata (RFC 9728), which names the
// authorization server, and that server's own metadata (RFC 8414, or
// OpenID Connect discovery), which names its endpoints.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"
)

// RequestTimeout bounds each request of discovery, registration and
// deregistration, on the engine's clock: as long as core's HTTP client
// gives any request to a vendor's OAuth endpoint.
const RequestTimeout = 8 * time.Second

// maxMetadataBytes bounds what is read of a metadata document or a
// registration's answer. A document larger than this is refused, not
// cut.
const maxMetadataBytes = 256 << 10

// maxRedirects bounds the redirects a metadata request follows, each on
// the host it was sent to.
const maxRedirects = 5

// Metadata is what Discover found: where the owner signs in, where core
// takes the tokens from and registers itself, and what the server says
// it offers.
type Metadata struct {
	// Resource is the protected resource's identifier, as its metadata
	// gives it (RFC 9728), the resource the tokens are asked for (RFC
	// 8707); empty when the discover address named the authorization
	// server itself.
	Resource string
	// Issuer is the authorization server's identifier.
	Issuer string
	// The authorization server's endpoints. Authorization and Token are
	// always set; Revocation and Registration are empty when the server
	// names none.
	Authorization, Token, Revocation, Registration string
	// PKCEMethods are the PKCE methods the server lists
	// (code_challenge_methods_supported), as given; S256 among them
	// when it lists any.
	PKCEMethods []string
	// AuthMethods are how the server lets a client authenticate at its
	// token endpoint (token_endpoint_auth_methods_supported), as given;
	// empty when it lists none.
	AuthMethods []string
}

// Endpoints are m's endpoints, with its resource.
func (m *Metadata) Endpoints() Endpoints {
	return Endpoints{Authorization: m.Authorization, Token: m.Token, Revocation: m.Revocation, Resource: m.Resource}
}

// resourceDoc is the protected resource's metadata (RFC 9728, 2), the
// parts Kivali reads.
type resourceDoc struct {
	Resource             string   `json:"resource"`
	AuthorizationServers []string `json:"authorization_servers"`
}

// serverDoc is the authorization server's metadata (RFC 8414, 2), the
// parts Kivali reads.
type serverDoc struct {
	Issuer                string   `json:"issuer"`
	AuthorizationEndpoint string   `json:"authorization_endpoint"`
	TokenEndpoint         string   `json:"token_endpoint"`
	RevocationEndpoint    string   `json:"revocation_endpoint"`
	RegistrationEndpoint  string   `json:"registration_endpoint"`
	ResponseTypes         []string `json:"response_types_supported"`
	GrantTypes            []string `json:"grant_types_supported"`
	PKCEMethods           []string `json:"code_challenge_methods_supported"`
	AuthMethods           []string `json:"token_endpoint_auth_methods_supported"`
}

// Discover reads the metadata at discover, the address a manifest gives
// for a registered client, and returns the authorization server's
// endpoints. hosts are the hosts the endpoints may be on (the
// manifest's); discover's own host is one of them.
//
// It reads the protected resource's metadata at
// /.well-known/oauth-protected-resource with discover's path after it
// (RFC 9728, 3.1), then without the path, and follows the first
// authorization server it names. With none there, discover is taken
// for the authorization server itself, at its path and then at its
// root. An authorization server's metadata is read at
// /.well-known/oauth-authorization-server with the issuer's path after
// it (RFC 8414, 3.1), then at /.well-known/openid-configuration the
// same way, then, for an issuer with a path, at that path followed by
// /.well-known/openid-configuration. A 404, 410 or 403, or a 200 that is
// no JSON object (a site's page for any path), is a document not there;
// any other status, no answer, or a JSON object Kivali cannot read is
// an error.
//
// The metadata is refused when the resource document names no resource
// or one that is not discover, the issuer it names is not the one it
// was read for (RFC 8414, 3.3), an endpoint or the authorization server
// is not an https:// address (plain http only on loopback) or is on a
// host not in hosts (hostAllowed), or the server does not offer the
// authorization code grant or does not list PKCE with S256. A vendor's
// value an error names is cut and quoted (vendorValue). Each request is bounded by
// RequestTimeout on the engine's clock and by maxMetadataBytes, and
// follows redirects only on the host it was sent to.
func (e *Engine) Discover(ctx context.Context, discover string, hosts []string) (*Metadata, error) {
	u, err := url.Parse(discover)
	if err != nil || !fetchable(u) {
		return nil, fmt.Errorf("%q is not an https:// address to read the service's metadata from", discover)
	}
	allowed := append(slices.Clone(hosts), strings.ToLower(u.Host))
	origin := u.Scheme + "://" + u.Host
	path := strings.TrimSuffix(u.EscapedPath(), "/")
	self := origin + path

	at := []string{origin + "/.well-known/oauth-protected-resource" + path}
	if path != "" {
		at = append(at, origin+"/.well-known/oauth-protected-resource")
	}
	for i, doc := range at {
		var rd resourceDoc
		found, err := e.getJSON(ctx, doc, allowed, &rd)
		if err != nil {
			return nil, err
		}
		if !found {
			continue
		}
		// The resource a document is for is the one its address was
		// made from (RFC 9728, 3.3): discover, or at the root also the
		// origin.
		want := []string{self}
		if i > 0 {
			want = append(want, origin)
		}
		switch {
		case rd.Resource == "":
			return nil, fmt.Errorf("the service's metadata at %s names no resource", doc)
		case !slices.Contains(want, strings.TrimSuffix(rd.Resource, "/")):
			return nil, fmt.Errorf("the service's metadata at %s is for %s, not %s", doc, vendorValue(rd.Resource), discover)
		case len(rd.AuthorizationServers) == 0:
			return nil, fmt.Errorf("the service's metadata at %s names no authorization server", doc)
		}
		issuer := rd.AuthorizationServers[0]
		iu, err := url.Parse(issuer)
		switch {
		case err != nil || !fetchable(iu):
			return nil, fmt.Errorf("the service's metadata names an authorization server, %s, that is not an https:// address", vendorValue(issuer))
		case !hostAllowed(iu, allowed):
			return nil, fmt.Errorf("the service's metadata names its authorization server on %s, which the manifest does not list", vendorValue(strings.ToLower(iu.Host)))
		}
		m, err := e.serverMetadata(ctx, issuer, allowed)
		if err != nil {
			return nil, err
		}
		if m == nil {
			return nil, fmt.Errorf("the authorization server %s publishes no metadata", vendorValue(issuer))
		}
		m.Resource = rd.Resource
		return m, nil
	}
	issuers := []string{self}
	if path != "" {
		issuers = append(issuers, origin)
	}
	for _, issuer := range issuers {
		m, err := e.serverMetadata(ctx, issuer, allowed)
		if err != nil || m != nil {
			return m, err
		}
	}
	return nil, fmt.Errorf("%s publishes no metadata that names its authorization server", strings.ToLower(u.Host))
}

// serverMetadata reads the metadata of the authorization server issuer
// (RFC 8414) and checks it; nil with no error when it publishes none.
func (e *Engine) serverMetadata(ctx context.Context, issuer string, allowed []string) (*Metadata, error) {
	u, err := url.Parse(issuer)
	if err != nil {
		return nil, fmt.Errorf("%q is not an authorization server's address", issuer)
	}
	origin := u.Scheme + "://" + u.Host
	path := strings.TrimSuffix(u.EscapedPath(), "/")
	at := []string{
		origin + "/.well-known/oauth-authorization-server" + path,
		origin + "/.well-known/openid-configuration" + path,
	}
	if path != "" {
		at = append(at, origin+path+"/.well-known/openid-configuration")
	}
	for _, doc := range at {
		var sd serverDoc
		found, err := e.getJSON(ctx, doc, allowed, &sd)
		if err != nil {
			return nil, err
		}
		if !found {
			continue
		}
		return checkServerDoc(doc, origin+path, sd, allowed)
	}
	return nil, nil
}

// checkServerDoc is sd, the metadata read at doc for the issuer issuer,
// as Metadata, or why it is refused.
func checkServerDoc(doc, issuer string, sd serverDoc, allowed []string) (*Metadata, error) {
	if strings.TrimSuffix(sd.Issuer, "/") != issuer {
		return nil, fmt.Errorf("the service's metadata at %s names the issuer %s, not %s", doc, vendorValue(sd.Issuer), issuer)
	}
	for _, ep := range []struct{ name, value string }{
		{"authorization endpoint", sd.AuthorizationEndpoint},
		{"token endpoint", sd.TokenEndpoint},
		{"revocation endpoint", sd.RevocationEndpoint},
		{"registration endpoint", sd.RegistrationEndpoint},
	} {
		if ep.value == "" {
			if ep.name == "authorization endpoint" || ep.name == "token endpoint" {
				return nil, fmt.Errorf("the service's metadata names no %s", ep.name)
			}
			continue
		}
		u, err := url.Parse(ep.value)
		switch {
		case err != nil || !fetchable(u):
			return nil, fmt.Errorf("the service's metadata names its %s as %s, which is not an https:// address", ep.name, vendorValue(ep.value))
		case !hostAllowed(u, allowed):
			return nil, fmt.Errorf("the service's metadata names its %s on %s, which the manifest does not list", ep.name, vendorValue(strings.ToLower(u.Host)))
		}
	}
	switch {
	case len(sd.ResponseTypes) > 0 && !slices.Contains(sd.ResponseTypes, "code"),
		len(sd.GrantTypes) > 0 && !slices.Contains(sd.GrantTypes, "authorization_code"):
		return nil, errors.New("the service does not offer the authorization code sign-in Kivali uses")
	case !slices.Contains(sd.PKCEMethods, "S256"):
		// RFC 8414: a server that lists no methods offers no PKCE; MCP's
		// authorization has a client refuse it.
		return nil, errors.New("the service's metadata does not list PKCE with S256 (code_challenge_methods_supported), which Kivali signs in with")
	}
	return &Metadata{
		Issuer:        sd.Issuer,
		Authorization: sd.AuthorizationEndpoint, Token: sd.TokenEndpoint,
		Revocation: sd.RevocationEndpoint, Registration: sd.RegistrationEndpoint,
		PKCEMethods: sd.PKCEMethods, AuthMethods: sd.AuthMethods,
	}, nil
}

// fetchable reports whether u is an address core sends a request to:
// https://, or plain http:// on loopback (a vendor run in-process in
// tests), with a host, no user and no fragment.
func fetchable(u *url.URL) bool {
	if u == nil || u.Host == "" || u.User != nil || u.Fragment != "" {
		return false
	}
	switch u.Scheme {
	case "https":
		return true
	case "http":
		h := u.Hostname()
		return h == "localhost" || h == "127.0.0.1" || h == "::1"
	}
	return false
}

// hostAllowed reports whether u's host is among hosts: as given, port
// and all, or by its name alone when u names no port, the scheme's
// default port, or a loopback host (a vendor run in-process in tests,
// on a port of its own). A listed host name is not every port on it.
func hostAllowed(u *url.URL, hosts []string) bool {
	if slices.Contains(hosts, strings.ToLower(u.Host)) {
		return true
	}
	name := strings.ToLower(u.Hostname())
	switch port := u.Port(); {
	case port == "", u.Scheme == "https" && port == "443", u.Scheme == "http" && port == "80",
		name == "localhost", name == "127.0.0.1", name == "::1":
		return slices.Contains(hosts, name)
	}
	return false
}

// maxVendorValueRunes bounds a value from a vendor's document that an
// error carries, such as an endpoint or a host it names.
const maxVendorValueRunes = 200

// vendorValue is s, a value from a vendor's document or answer, cut to
// maxVendorValueRunes and quoted, for an error's words and the log.
func vendorValue(s string) string { return strconv.Quote(clipRunes(s, maxVendorValueRunes)) }

// statusWords is an HTTP status in words, from its code alone: the
// reason phrase a server sends is its own text.
func statusWords(code int) string { return strconv.Itoa(code) + " " + http.StatusText(code) }

// errRequestTimeout is the cause of a request's context that
// RequestTimeout ended.
var errRequestTimeout = errors.New("request timeout")

// bounded is ctx ended after RequestTimeout on the engine's clock, with
// errRequestTimeout as its cause, and the func that releases it.
func (e *Engine) bounded(ctx context.Context) (context.Context, func()) {
	ctx, cancel := context.WithCancelCause(ctx)
	t := e.clk().NewTimer(RequestTimeout)
	stop := make(chan struct{})
	go func() {
		select {
		case <-t.C():
			cancel(errRequestTimeout)
		case <-stop:
		}
	}()
	return ctx, func() {
		t.Stop()
		close(stop)
		cancel(context.Canceled)
	}
}

// sameHostOnly is a redirect policy that follows at most maxRedirects,
// each to an address fetchable on the host the first request went to.
func sameHostOnly(req *http.Request, via []*http.Request) error {
	switch {
	case len(via) >= maxRedirects:
		return redirectRefused{fmt.Sprintf("%s redirected Kivali too many times", via[0].URL.Host)}
	case !strings.EqualFold(req.URL.Host, via[0].URL.Host):
		return redirectRefused{fmt.Sprintf("%s sent Kivali on to %s, another host", via[0].URL.Host, vendorValue(req.URL.Host))}
	case !fetchable(req.URL):
		return redirectRefused{fmt.Sprintf("%s sent Kivali on to an address that is not https://", via[0].URL.Host)}
	}
	return nil
}

// redirectRefused is a redirect sameHostOnly did not follow, in words.
type redirectRefused struct{ words string }

func (r redirectRefused) Error() string { return r.words }

// noRedirects is a redirect policy that follows none: the answer is the
// redirect itself.
func noRedirects(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }

// clientWith is the engine's HTTP client with the redirect policy
// policy.
func (e *Engine) clientWith(policy func(*http.Request, []*http.Request) error) *http.Client {
	c := *e.client()
	c.CheckRedirect = policy
	return &c
}

// requestFailure is a request's failure err to reach host in words:
// the engine's timeout, a redirect refused, or the transport's error.
func requestFailure(ctx context.Context, host string, err error) error {
	if errors.Is(context.Cause(ctx), errRequestTimeout) {
		return fmt.Errorf("%s did not answer within %s", host, RequestTimeout)
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	var rr redirectRefused
	if errors.As(err, &rr) {
		return rr
	}
	var ue *url.Error
	if errors.As(err, &ue) {
		err = ue.Err
	}
	return fmt.Errorf("%s did not answer: %w", host, err)
}

// readBounded reads body whole, refusing one longer than
// maxMetadataBytes.
func readBounded(ctx context.Context, host string, body io.Reader) ([]byte, error) {
	b, err := io.ReadAll(io.LimitReader(body, maxMetadataBytes+1))
	if err != nil {
		return nil, requestFailure(ctx, host, err)
	}
	if len(b) > maxMetadataBytes {
		return nil, fmt.Errorf("%s answered with more than %d KiB", host, maxMetadataBytes>>10)
	}
	return b, nil
}

// getJSON reads the JSON document at raw into into: true when it is
// there, false when the server answers a 4xx (no document there). The
// address must be fetchable on one of allowed; the request is bounded
// (bounded, readBounded) and follows redirects on its host alone.
func (e *Engine) getJSON(ctx context.Context, raw string, allowed []string, into any) (bool, error) {
	u, err := url.Parse(raw)
	if err != nil || !fetchable(u) || !hostAllowed(u, allowed) {
		return false, fmt.Errorf("%s is not an address Kivali reads the service's metadata from", raw)
	}
	host := strings.ToLower(u.Host)
	ctx, done := e.bounded(ctx)
	defer done()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, raw, nil)
	if err != nil {
		return false, err
	}
	req.Header.Set("Accept", "application/json")
	resp, err := e.clientWith(sameHostOnly).Do(req)
	if err != nil {
		return false, requestFailure(ctx, host, err)
	}
	defer func() { _ = resp.Body.Close() }()
	switch resp.StatusCode {
	case http.StatusOK:
	case http.StatusNotFound, http.StatusGone, http.StatusForbidden:
		// No document there.
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxMetadataBytes))
		return false, nil
	default:
		return false, fmt.Errorf("%s answered %s for %s", host, statusWords(resp.StatusCode), raw)
	}
	b, err := readBounded(ctx, host, resp.Body)
	if err != nil {
		return false, err
	}
	if !bytes.HasPrefix(bytes.TrimSpace(b), []byte("{")) {
		// A page that is no JSON object, as a site answers any path
		// with: no document there.
		return false, nil
	}
	if err := json.Unmarshal(b, into); err != nil {
		return false, fmt.Errorf("the service's metadata at %s is not a document Kivali reads", raw)
	}
	return true, nil
}
