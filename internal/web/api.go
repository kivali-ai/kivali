package web

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"strings"

	"github.com/kivali-ai/kivali/internal/auth"
	"github.com/kivali-ai/kivali/internal/web/apitypes"
)

// The JSON API the Kivali web app reads. Every route is under
// /api/v1/, behind the same session middleware as the pages (which
// answers an unauthenticated /api/ request with a JSON 401, never a
// redirect; see auth.Middleware.Wrap), and every non-GET request must
// come from this origin (requireSameOrigin). Handlers live in
// api_*.go, one file per area; response shapes live in apitypes.

// apiMaxJSONBody caps a JSON request body. The API's bodies are small
// settings and short texts; attachments travel as multipart.
const apiMaxJSONBody = 1 << 20

// whoDevelopers is the "who can fix it" of an error the person using
// the app did not cause and cannot fix: the client and server disagree,
// which is a bug.
const whoDevelopers = "Kivali's developers"

// wireAPIRoutes mounts the API on the protected mux.
//
// The routes live on their own mux so the whole tree shares one
// middleware stack and one JSON 404/405. It is mounted once per method
// rather than as a bare "/api/" because the protected mux's catch-all
// is "GET /{path...}", and a method-less "/api/" would overlap it with
// neither pattern more specific — a registration panic.
func (s *Server) wireAPIRoutes(protected *http.ServeMux) {
	api := http.NewServeMux()
	api.HandleFunc("GET /api/v1/me", s.handleAPIMe)
	api.HandleFunc("GET /api/v1/snapshot", s.handleAPISnapshot)
	api.HandleFunc("GET /api/v1/agents", s.handleAPIAgents)
	api.HandleFunc("GET /api/v1/agents/{slug}", s.handleAPIAgent)
	api.HandleFunc("POST /api/v1/auto-release", s.handleAPIAutoRelease)
	s.wireAPIPendingRoutes(api)
	s.wireAPIHomeRoutes(api)
	s.wireAPIChatRoutes(api)
	s.wireAPIProposalRoutes(api)
	s.wireAPIAgentWorkRoutes(api)
	s.wireAPIWorkRoutes(api)
	s.wireAPIGraphRoutes(api)
	s.wireAPISetupRoutes(api)
	s.wireAPIOrgRoutes(api)
	s.wireAPIDesktopRoutes(api)
	api.Handle(auth.APIPrefix, apiFallback(api))

	h := apiHeaders(requireSameOrigin(api))
	for _, m := range apiMethods {
		protected.Handle(m+" "+auth.APIPrefix, h)
	}
}

// apiMethods are the methods the API answers. GET covers HEAD.
var apiMethods = []string{http.MethodGet, http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete}

// apiFallback answers what no API route claims: 405 when the path
// exists under another method, else 404, both as JSON. The mux's own
// answers would be plain text.
func apiFallback(api *http.ServeMux) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var allow []string
		for _, m := range apiMethods {
			if m == r.Method {
				continue
			}
			probe := r.Clone(r.Context())
			probe.Method = m
			if _, pattern := api.Handler(probe); pattern != "" && pattern != auth.APIPrefix {
				allow = append(allow, m)
			}
		}
		if len(allow) > 0 {
			w.Header().Set("allow", strings.Join(allow, ", "))
			writeAPIError(w, http.StatusMethodNotAllowed, r.Method+" is not supported here", whoDevelopers)
			return
		}
		writeAPIError(w, http.StatusNotFound, "no such API endpoint", whoDevelopers)
	})
}

// apiHeaders sets what every API response carries: nothing under
// /api/ is cacheable, since it all describes live state.
func apiHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("cache-control", "no-store")
		next.ServeHTTP(w, r)
	})
}

// requireSameOrigin refuses a state-changing request that did not come
// from a page on this origin, with a JSON 403. GET and HEAD pass: they
// change nothing, and the session cookie alone gates what they read.
//
// A browser that sends Sec-Fetch-Site is trusted on it, and only
// same-origin passes: same-site is another origin under the same
// registrable domain, cross-site is anyone, and none (typed into the
// address bar, a bookmark, a browser extension) is never how a page on
// this origin writes — fetch() from our own page always says
// same-origin. Without that header, Origin must equal the request's
// own scheme and host — the scheme taken from TLS or, behind a
// TLS-terminating proxy, X-Forwarded-Proto; the host from
// X-Forwarded-Host when a proxy rewrites Host. A request with neither
// header is refused:
// every browser this app supports sends one of them on a cross-site
// write, and a form POST always carries Origin.
func requireSameOrigin(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet || r.Method == http.MethodHead {
			next.ServeHTTP(w, r)
			return
		}
		if !sameOrigin(r) {
			writeAPIError(w, http.StatusForbidden, "request refused because it came from another site", "you, from Kivali's own address")
			return
		}
		next.ServeHTTP(w, r)
	})
}

func sameOrigin(r *http.Request) bool {
	if site := r.Header.Get("Sec-Fetch-Site"); site != "" {
		return site == "same-origin"
	}
	origin := r.Header.Get("Origin")
	if origin == "" || origin == "null" {
		return false
	}
	u, err := url.Parse(origin)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return false
	}
	return strings.EqualFold(u.Scheme, requestScheme(r)) && strings.EqualFold(u.Host, requestHost(r))
}

// requestScheme is the scheme the browser used to reach this server.
func requestScheme(r *http.Request) string {
	if r.TLS != nil {
		return "https"
	}
	if p := r.Header.Get("X-Forwarded-Proto"); p != "" {
		return strings.ToLower(firstForwarded(p))
	}
	return "http"
}

// requestHost is the host (with any port) the browser used to reach
// this server: X-Forwarded-Host when a proxy rewrote Host, else Host.
func requestHost(r *http.Request) string {
	if h := r.Header.Get("X-Forwarded-Host"); h != "" {
		return firstForwarded(h)
	}
	return r.Host
}

// firstForwarded is the first value of a comma-joined X-Forwarded-*
// header: the one the outermost proxy set.
func firstForwarded(v string) string {
	return strings.TrimSpace(strings.Split(v, ",")[0])
}

// writeJSON (agentpod_socket.go) writes a value as the JSON body; the
// API shares it. apiHeaders has already set no-store.

// writeJSONBytes writes an already-marshalled JSON body.
func writeJSONBytes(w http.ResponseWriter, status int, body []byte) {
	w.Header().Set("content-type", "application/json")
	w.Header().Set("cache-control", "no-store")
	w.WriteHeader(status)
	_, _ = w.Write(body)
	_, _ = w.Write([]byte{'\n'})
}

// writeAPIError writes the API's error shape: what happened, then who
// can fix it. Both are sentences for a person, never a stack trace or
// an instruction to run something.
func writeAPIError(w http.ResponseWriter, status int, what, who string) {
	writeJSON(w, status, apitypes.ErrorBody{Error: what, Who: who})
}

// errBodyTooLarge is decodeJSON's refusal of a body over
// apiMaxJSONBody.
var errBodyTooLarge = errors.New("request body too large")

// errNotJSON is decodeJSON's refusal of a body not declared as JSON.
var errNotJSON = errors.New("request body is not application/json")

// decodeJSON decodes r's body into v: declared application/json, at
// most apiMaxJSONBody bytes, exactly one JSON value, no fields v does
// not declare. An unknown field is an error rather than ignored so a
// client built against a newer contract fails loudly instead of having
// half its request dropped.
//
// The content-type check is the second wall behind requireSameOrigin:
// an HTML form can only send urlencoded, multipart or text/plain, so
// a JSON endpoint that insists on application/json cannot be driven
// by one even if the origin check were ever bypassed.
func decodeJSON(r *http.Request, v any) error {
	if r.Body == nil {
		return errors.New("empty request body")
	}
	mt, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || mt != "application/json" {
		return errNotJSON
	}
	dec := json.NewDecoder(http.MaxBytesReader(nil, r.Body, apiMaxJSONBody))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			return errBodyTooLarge
		}
		if errors.Is(err, io.EOF) {
			return errors.New("empty request body")
		}
		return err
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			return errBodyTooLarge
		}
		return errors.New("request body holds more than one JSON value")
	}
	return nil
}

// decodeJSONOrError decodes like decodeJSON and, on failure, writes the
// error response itself. Reports whether the handler should go on.
func decodeJSONOrError(w http.ResponseWriter, r *http.Request, v any) bool {
	err := decodeJSON(r, v)
	switch {
	case err == nil:
		return true
	case errors.Is(err, errBodyTooLarge):
		writeAPIError(w, http.StatusRequestEntityTooLarge,
			fmt.Sprintf("request body is over %d KB", apiMaxJSONBody>>10), whoDevelopers)
	case errors.Is(err, errNotJSON):
		writeAPIError(w, http.StatusUnsupportedMediaType, err.Error(), whoDevelopers)
	default:
		writeAPIError(w, http.StatusBadRequest, "request body is not valid: "+err.Error(), whoDevelopers)
	}
	return false
}
