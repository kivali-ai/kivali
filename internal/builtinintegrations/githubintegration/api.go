package githubintegration

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// DefaultAPIBase is GitHub's REST API.
const DefaultAPIBase = "https://api.github.com"

// The client's bounds: each request's time, the most of an answer it
// reads, and the redirects it follows (on the API's own origin only).
const (
	requestTimeout = 30 * time.Second
	maxAnswerBytes = 16 << 20
	maxRedirects   = 5
	// errorWordsMax bounds GitHub's words an error quotes.
	errorWordsMax = 1000
)

// The media types the client asks for.
const (
	mediaJSON = "application/vnd.github+json"
	mediaDiff = "application/vnd.github.diff"
)

// apiVersion is the REST API version every request names.
const apiVersion = "2022-11-28"

// apiError is a request GitHub did not answer with success, or did not
// answer at all (Status 0). Message is GitHub's own words, cut and
// scrubbed of the request's credential.
type apiError struct {
	Status  int
	Message string
	// RateLimited is a 403 or 429 that says the app's requests are
	// limited for now.
	RateLimited bool
	// Timeout is GitHub not answering in time, for Status 0.
	Timeout bool
	// Transport is the failure to reach GitHub, for Status 0.
	Transport error
}

func (e *apiError) Error() string {
	if e.Status == 0 {
		return "GitHub did not answer: " + e.Message
	}
	return fmt.Sprintf("GitHub answered %d: %s", e.Status, e.Message)
}

// base is the API's address, checked: https, or plain http on loopback
// for the tests' fake.
func (g *GitHub) base() (*url.URL, error) {
	raw := g.APIBase
	if raw == "" {
		raw = DefaultAPIBase
	}
	u, err := url.Parse(strings.TrimSuffix(raw, "/"))
	if err != nil || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return nil, errors.New("the GitHub built-in's API base is not an address")
	}
	switch u.Scheme {
	case "https":
	case "http":
		if h := u.Hostname(); h != "127.0.0.1" && h != "localhost" && h != "::1" {
			return nil, errors.New("the GitHub built-in's API base is plain http off loopback")
		}
	default:
		return nil, errors.New("the GitHub built-in's API base is not https")
	}
	return u, nil
}

// httpClient is the built-in's one client: a deadline per request, and
// redirects followed only on the API's own origin.
func (g *GitHub) httpClient() *http.Client {
	g.clientOnce.Do(func() {
		g.client = &http.Client{
			Timeout: requestTimeout,
			CheckRedirect: func(req *http.Request, via []*http.Request) error {
				if len(via) >= maxRedirects {
					return errors.New("GitHub redirected too many times")
				}
				if req.URL.Scheme != via[0].URL.Scheme || req.URL.Host != via[0].URL.Host {
					return errors.New("GitHub redirected off its API")
				}
				return nil
			},
		}
	})
	return g.client
}

// request is one REST request.
type request struct {
	method string
	// path is under the API base, with its query: "/repos/a/b/issues?state=open".
	path string
	// auth is the bearer credential: the app's JWT or an installation
	// token.
	auth string
	// body, when set, is sent as JSON.
	body any
	// accept is the media type asked for; empty is mediaJSON.
	accept string
}

// do sends r and reads the answer: its body and headers on success,
// an *apiError otherwise.
func (g *GitHub) do(ctx context.Context, r request) ([]byte, http.Header, error) {
	base, err := g.base()
	if err != nil {
		return nil, nil, err
	}
	var body io.Reader
	if r.body != nil {
		b, err := json.Marshal(r.body)
		if err != nil {
			return nil, nil, err
		}
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, r.method, base.String()+r.path, body)
	if err != nil {
		return nil, nil, err
	}
	req.Header.Set("Accept", cmp.Or(r.accept, mediaJSON))
	req.Header.Set("X-GitHub-Api-Version", apiVersion)
	req.Header.Set("User-Agent", "Kivali")
	req.Header.Set("Authorization", "Bearer "+r.auth)
	if r.body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := g.httpClient().Do(req)
	if err != nil {
		return nil, nil, transportError(ctx, err, r.auth)
	}
	defer func() { _ = resp.Body.Close() }()
	b, err := io.ReadAll(io.LimitReader(resp.Body, maxAnswerBytes+1))
	switch {
	case err != nil:
		return nil, nil, transportError(ctx, err, r.auth)
	case len(b) > maxAnswerBytes:
		return nil, nil, &apiError{Message: "its answer is larger than Kivali reads", Transport: errors.New("answer too large")}
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, nil, &apiError{
			Status:      resp.StatusCode,
			Message:     scrub(errorWords(b, resp.Status), r.auth),
			RateLimited: rateLimited(resp),
		}
	}
	return b, resp.Header, nil
}

// getJSON GETs path and decodes its JSON answer into out.
func (g *GitHub) getJSON(ctx context.Context, auth, path string, out any) error {
	return g.sendJSON(ctx, request{method: http.MethodGet, path: path, auth: auth}, out)
}

// sendJSON sends r and decodes its JSON answer into out.
func (g *GitHub) sendJSON(ctx context.Context, r request, out any) error {
	b, _, err := g.do(ctx, r)
	if err != nil {
		return err
	}
	if out == nil {
		return nil
	}
	if err := json.Unmarshal(b, out); err != nil {
		return &apiError{Status: 0, Message: "its answer is not JSON Kivali reads", Transport: err}
	}
	return nil
}

// maxPages bounds the pages a listing reads.
const maxPages = 100

// pages GETs path page by page, 100 a page, and gives each page's body
// to each until it says stop, a page holds fewer than 100, or most
// pages (at most maxPages) are read. each returns how many items the
// page held.
func (g *GitHub) pages(ctx context.Context, auth, path string, most int, each func(body []byte) (n int, stop bool, err error)) error {
	sep := "&"
	if !strings.Contains(path, "?") {
		sep = "?"
	}
	for page := 1; page <= min(most, maxPages); page++ {
		b, _, err := g.do(ctx, request{method: http.MethodGet, path: path + sep + "per_page=100&page=" + strconv.Itoa(page), auth: auth})
		if err != nil {
			return err
		}
		n, stop, err := each(b)
		if err != nil || stop || n < 100 {
			return err
		}
	}
	return nil
}

// errorWords is GitHub's message in an error answer, with each of its
// errors' own messages: "Validation Failed: A pull request already
// exists for acme:x." The status's words when it has none.
func errorWords(body []byte, status string) string {
	var e struct {
		Message string `json:"message"`
		Errors  []struct {
			Message string `json:"message"`
			Code    string `json:"code"`
			Field   string `json:"field"`
		} `json:"errors"`
	}
	words := ""
	if json.Unmarshal(body, &e) == nil {
		words = strings.TrimSpace(e.Message)
		var more []string
		for _, x := range e.Errors {
			switch {
			case x.Message != "":
				more = append(more, x.Message)
			case x.Field != "" && x.Code != "":
				more = append(more, x.Field+" "+x.Code)
			}
		}
		if len(more) > 0 {
			words += ": " + strings.Join(more, "; ")
		}
	}
	if words == "" {
		words = status
	}
	words = strings.Join(strings.Fields(words), " ")
	w, _ := clip(words, errorWordsMax)
	return w
}

// rateLimited reports whether a 403 or 429 is GitHub limiting the
// app's requests rather than refusing them.
func rateLimited(resp *http.Response) bool {
	if resp.StatusCode != http.StatusForbidden && resp.StatusCode != http.StatusTooManyRequests {
		return false
	}
	return resp.Header.Get("X-RateLimit-Remaining") == "0" || resp.Header.Get("Retry-After") != ""
}

// transportError is a failure to reach GitHub, or to read its answer,
// in words scrubbed of the request's credential.
func transportError(ctx context.Context, err error, secret string) *apiError {
	if ctx.Err() != nil || errors.Is(err, context.DeadlineExceeded) {
		return &apiError{Message: "not in time", Timeout: true, Transport: err}
	}
	cause := err
	var ue *url.Error
	if errors.As(err, &ue) {
		if ue.Timeout() {
			return &apiError{Message: "not in time", Timeout: true, Transport: err}
		}
		cause = ue.Err
	}
	w, _ := clip(cause.Error(), errorWordsMax)
	return &apiError{Message: scrub(w, secret), Transport: err}
}

// sentence is GitHub's words without the full stop they may end with,
// for words that go on after them.
func sentence(s string) string { return strings.TrimRight(s, ".") }

// scrub is s with secret, a credential the request carried, taken
// out. GitHub's answers do not echo one, and nothing passed on may.
func scrub(s, secret string) string {
	if secret == "" {
		return s
	}
	return strings.ReplaceAll(s, secret, "[credential]")
}

// agentWords is a GitHub failure in words for the agent: what GitHub
// refused, and "ask the owner" where only the owner can mend it.
func agentWords(e *apiError) string {
	switch {
	case e.Timeout:
		return "GitHub did not answer in time; try again later."
	case e.Status == 0:
		return "GitHub did not answer: " + sentence(e.Message) + ". Try again later."
	case e.RateLimited:
		return fmt.Sprintf("GitHub is limiting the GitHub App's requests for now (HTTP %d): %s. Try again later.", e.Status, sentence(e.Message))
	case e.Status == http.StatusUnauthorized:
		return fmt.Sprintf("GitHub refused the GitHub App's credentials (HTTP 401): %s. Ask the owner.", sentence(e.Message))
	case e.Status == http.StatusForbidden:
		return fmt.Sprintf("GitHub refused this (HTTP 403): %s. The GitHub App may lack the permission, or not be installed on the repository; ask the owner.", sentence(e.Message))
	}
	return fmt.Sprintf("GitHub answered %d: %s", e.Status, e.Message)
}

// ownerWords is a GitHub failure in words for the owner, as Test
// connection shows it.
func ownerWords(e *apiError) string {
	switch {
	case e.Timeout:
		return "GitHub did not answer in time; try again later."
	case e.Status == 0:
		return "GitHub did not answer: " + e.Message
	case e.RateLimited:
		return fmt.Sprintf("GitHub is limiting the GitHub App's requests for now (HTTP %d): %s. Try again later.", e.Status, sentence(e.Message))
	case e.Status == http.StatusUnauthorized:
		return fmt.Sprintf("GitHub refused the GitHub App's key (HTTP 401): %s. Set GitHub up again.", sentence(e.Message))
	case e.Status == http.StatusNotFound:
		return fmt.Sprintf("GitHub has no installation of the GitHub App that Kivali keeps (HTTP 404): %s. Set GitHub up again.", sentence(e.Message))
	}
	return fmt.Sprintf("GitHub answered %d: %s", e.Status, e.Message)
}
