package googleworkspace

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
	"unicode/utf8"
)

// api is one of Google's API origins a call reaches. Each is pinned:
// its base is the one address of the host the manifest declares, or
// plain http on loopback for the tests' fake.
type api int

const (
	// gmailAPI is Gmail's, gmail.googleapis.com.
	gmailAPI api = iota
	// googleAPI is www.googleapis.com: Calendar's and Drive's.
	googleAPI
	// docsAPI is Google Docs', docs.googleapis.com.
	docsAPI
)

// The APIs' addresses as the binary reaches them.
const (
	DefaultGmailBase = "https://gmail.googleapis.com"
	DefaultAPIBase   = "https://www.googleapis.com"
	DefaultDocsBase  = "https://docs.googleapis.com"
)

// The client's bounds: each request's time, the most of an answer it
// reads, the redirects it follows (a GET's, on the API's own origin
// only), and the pages a listing reads.
const (
	requestTimeout = 30 * time.Second
	maxAnswerBytes = 16 << 20
	maxRedirects   = 5
	maxPages       = 10
	// errorWordsMax bounds Google's words an error quotes.
	errorWordsMax = 1000
)

// base is a's address, checked: its default over https, or plain http
// on loopback for the tests' fake.
func (g *GoogleWorkspace) base(a api) (*url.URL, error) {
	raw, def := g.GmailBase, DefaultGmailBase
	switch a {
	case googleAPI:
		raw, def = g.APIBase, DefaultAPIBase
	case docsAPI:
		raw, def = g.DocsBase, DefaultDocsBase
	}
	raw = cmp.Or(raw, def)
	u, err := url.Parse(strings.TrimSuffix(raw, "/"))
	if err != nil || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Path != "" && u.Path != "/") {
		return nil, errors.New("the Google Workspace built-in's API base " + raw + " is not an origin")
	}
	switch u.Scheme {
	case "https":
		if u.Scheme+"://"+u.Host != def {
			return nil, errors.New("the Google Workspace built-in's API base " + raw + " is not " + def)
		}
	case "http":
		if h := u.Hostname(); h != "127.0.0.1" && h != "localhost" && h != "::1" {
			return nil, errors.New("the Google Workspace built-in's API base " + raw + " is plain http off loopback")
		}
	default:
		return nil, errors.New("the Google Workspace built-in's API base " + raw + " is not https")
	}
	u.Path = ""
	return u, nil
}

// httpClient is the built-in's one client: a deadline per request, and
// redirects followed only for a GET and only on the request's own
// origin.
func (g *GoogleWorkspace) httpClient() *http.Client {
	g.clientOnce.Do(func() {
		g.client = &http.Client{
			Timeout: requestTimeout,
			CheckRedirect: func(req *http.Request, via []*http.Request) error {
				// These are the cause in "Google did not answer: ...".
				if via[0].Method != http.MethodGet {
					return errors.New("it redirected a request that is not a read")
				}
				if len(via) >= maxRedirects {
					return errors.New("it redirected too many times")
				}
				if req.URL.Scheme != via[0].URL.Scheme || req.URL.Host != via[0].URL.Host {
					return errors.New("it redirected off its API")
				}
				// Go keeps the Authorization header on a redirect to the
				// same host, which is the only one followed.
				return nil
			},
		}
	})
	return g.client
}

// apiError is a request Google did not answer with success, or did not
// answer at all (Status 0). Message is Google's own words, cut and
// scrubbed of the request's token; Reason is the first reason Google
// gave for it ("insufficientPermissions", "SERVICE_DISABLED").
type apiError struct {
	Status  int
	Message string
	Reason  string
	// RateLimited is Google limiting the account's requests for now: a
	// 429, or a 403 whose reason says so.
	RateLimited bool
	// Timeout is Google not answering in time, for Status 0.
	Timeout bool
	// Unreadable is an answer Google gave that Kivali does not read
	// (too large, or not JSON), for Status 0: Message says which, whole.
	Unreadable bool
}

func (e *apiError) Error() string {
	if e.Unreadable {
		return e.Message
	}
	if e.Status == 0 {
		return "Google did not answer: " + e.Message
	}
	return fmt.Sprintf("Google answered %d: %s", e.Status, e.Message)
}

// scopeMissing reports whether Google refused the request because the
// sign-in was not granted the scope it needs: Google's consent screen
// lets the owner leave a permission unticked.
func (e *apiError) scopeMissing() bool {
	return e.Status == http.StatusForbidden && (e.Reason == "insufficientPermissions" || e.Reason == "ACCESS_TOKEN_SCOPE_INSUFFICIENT")
}

// apiOff reports whether Google refused the request because the API is
// not turned on in the project the owner's client belongs to.
func (e *apiError) apiOff() bool {
	return e.Status == http.StatusForbidden && (e.Reason == "accessNotConfigured" || e.Reason == "SERVICE_DISABLED")
}

// get GETs path (with its query) on a with the access token token, and
// decodes the JSON answer into out.
func (g *GoogleWorkspace) get(ctx context.Context, token string, a api, path string, out any) error {
	base, err := g.base(a)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base.String()+path, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "Kivali")
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := g.httpClient().Do(req)
	if err != nil {
		return transportError(ctx, err, token)
	}
	defer func() { _ = resp.Body.Close() }()
	b, err := io.ReadAll(io.LimitReader(resp.Body, maxAnswerBytes+1))
	switch {
	case err != nil:
		return transportError(ctx, err, token)
	case len(b) > maxAnswerBytes:
		return &apiError{Message: "Google's answer was larger than Kivali reads", Unreadable: true}
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		e := readError(b, resp.StatusCode)
		e.Message = scrub(e.Message, token)
		return e
	}
	if err := json.Unmarshal(b, out); err != nil {
		return &apiError{Message: "Google's answer was not one Kivali reads", Unreadable: true}
	}
	return nil
}

// readError is Google's error answer of status: its message and the
// first reason it gives, from errors[] (the older APIs') or details[]
// (google.rpc.ErrorInfo); the status's words when it has no message.
func readError(body []byte, status int) *apiError {
	var e struct {
		Error struct {
			Message string `json:"message"`
			Status  string `json:"status"`
			Errors  []struct {
				Reason string `json:"reason"`
			} `json:"errors"`
			Details []struct {
				Reason string `json:"reason"`
			} `json:"details"`
		} `json:"error"`
	}
	out := &apiError{Status: status}
	if json.Unmarshal(body, &e) == nil {
		out.Message = strings.Join(strings.Fields(e.Error.Message), " ")
		for _, x := range e.Error.Errors {
			if out.Reason == "" && x.Reason != "" {
				out.Reason = x.Reason
			}
		}
		for _, x := range e.Error.Details {
			if out.Reason == "" && x.Reason != "" {
				out.Reason = x.Reason
			}
		}
	}
	if out.Message == "" {
		out.Message = cmp.Or(http.StatusText(status), fmt.Sprintf("HTTP %d", status))
	}
	out.Message, _ = clip(out.Message, errorWordsMax)
	switch out.Reason {
	case "rateLimitExceeded", "userRateLimitExceeded", "RATE_LIMIT_EXCEEDED":
		out.RateLimited = status == http.StatusForbidden || status == http.StatusTooManyRequests
	}
	if status == http.StatusTooManyRequests {
		out.RateLimited = true
	}
	return out
}

// transportError is a failure to reach Google, or to read its answer,
// in words scrubbed of the request's token.
func transportError(ctx context.Context, err error, token string) *apiError {
	if ctx.Err() != nil || errors.Is(err, context.DeadlineExceeded) {
		return &apiError{Message: "not in time", Timeout: true}
	}
	cause := err
	var ue *url.Error
	if errors.As(err, &ue) {
		if ue.Timeout() {
			return &apiError{Message: "not in time", Timeout: true}
		}
		cause = ue.Err
	}
	w, _ := clip(cause.Error(), errorWordsMax)
	return &apiError{Message: scrub(w, token)}
}

// scrub is s with the request's token taken out: Google's answers do
// not echo one, and nothing passed on may.
func scrub(s, token string) string {
	if token == "" {
		return s
	}
	return strings.ReplaceAll(s, token, "[credential]")
}

// sentence is words without the full stop they may end with, for
// words that go on after them.
func sentence(s string) string { return strings.TrimRight(s, ".") }

// agentWords is a Google failure in words for the agent: what Google
// refused, and "ask the owner" where only the owner can mend it.
func agentWords(e *apiError) string {
	switch {
	case e.Timeout:
		return "Google did not answer in time; try again later."
	case e.Unreadable:
		return e.Message + "."
	case e.Status == 0:
		return "Google did not answer: " + sentence(e.Message) + ". Try again later."
	case e.RateLimited:
		return fmt.Sprintf("Google is limiting this account's requests for now (HTTP %d): %s. Try again later.", e.Status, sentence(e.Message))
	case e.Status == http.StatusUnauthorized:
		return fmt.Sprintf("Google refused the connection's sign-in (HTTP 401): %s. Ask the owner.", sentence(e.Message))
	case e.apiOff():
		return "Google refused this (HTTP 403): the API it needs is not turned on in the owner's Google Cloud project. Ask the owner."
	case e.scopeMissing():
		return "Google refused this (HTTP 403): the owner's sign-in does not allow it. Ask the owner."
	case e.Status == http.StatusForbidden:
		return fmt.Sprintf("Google refused this (HTTP 403): %s. Ask the owner.", sentence(e.Message))
	case e.Status >= 500:
		return fmt.Sprintf("Google answered %d: %s. Try again later.", e.Status, sentence(e.Message))
	}
	return fmt.Sprintf("Google answered %d: %s", e.Status, e.Message)
}

// ownerWords is a Google failure in words for the owner, as Test
// connection and the options of an access field show it.
func ownerWords(e *apiError) string {
	switch {
	case e.Timeout:
		return "Google did not answer in time; try again later."
	case e.Unreadable:
		return e.Message + "."
	case e.Status == 0:
		return "Google did not answer: " + e.Message
	case e.RateLimited:
		return fmt.Sprintf("Google is limiting this account's requests for now (HTTP %d): %s. Try again later.", e.Status, sentence(e.Message))
	case e.Status == http.StatusUnauthorized:
		return fmt.Sprintf("Google refused the sign-in (HTTP 401): %s. Sign in again.", sentence(e.Message))
	case e.apiOff():
		return fmt.Sprintf("Google refused this (HTTP 403): %s. Turn the API on in the app's Google Cloud project, as the setup notes say, and try again.", sentence(e.Message))
	case e.scopeMissing():
		return fmt.Sprintf("Google refused this (HTTP 403): %s. The sign-in lacks a permission Kivali asks for: sign out, sign in again and allow everything Google lists.", sentence(e.Message))
	case e.Status >= 500:
		return fmt.Sprintf("Google answered %d: %s. Try again later.", e.Status, sentence(e.Message))
	}
	return fmt.Sprintf("Google answered %d: %s", e.Status, e.Message)
}

// ownerError is err in words for the owner.
func ownerError(err error) error {
	var ae *apiError
	if errors.As(err, &ae) {
		return errors.New(ownerWords(ae))
	}
	return err
}

// clip is s cut to most characters, and whether it was.
func clip(s string, most int) (string, bool) {
	if utf8.RuneCountInString(s) <= most {
		return s, false
	}
	i, n := 0, 0
	for i = range s {
		if n == most {
			break
		}
		n++
	}
	return s[:i], true
}
