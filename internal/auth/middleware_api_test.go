package auth

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestMiddlewareAPIUnauthenticatedIsJSON401 pins the API half of Wrap:
// under /api/ a missing or bad session is a JSON 401 for every method,
// never a redirect. A fetch() follows a 302 to the login page and sees
// an HTML 200, which the client cannot tell from a real answer.
func TestMiddlewareAPIUnauthenticatedIsJSON401(t *testing.T) {
	mw, codec := newMW(t, "alice@example.com")
	h := mw.Wrap(passthrough())
	good, _ := codec.EncodeSession(NewSession("alice@example.com"))
	cases := []struct {
		name, method, cookie string
	}{
		{"GET without cookie", http.MethodGet, ""},
		{"POST without cookie", http.MethodPost, ""},
		{"GET with tampered cookie", http.MethodGet, good + "x"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(tc.method, "/api/v1/me", nil)
			if tc.cookie != "" {
				req.AddCookie(&http.Cookie{Name: CookieName, Value: tc.cookie})
			}
			rr := httptest.NewRecorder()
			h.ServeHTTP(rr, req)
			if rr.Code != http.StatusUnauthorized {
				t.Fatalf("code = %d, want 401", rr.Code)
			}
			if loc := rr.Header().Get("location"); loc != "" {
				t.Errorf("location = %q, want no redirect", loc)
			}
			if ct := rr.Header().Get("content-type"); ct != "application/json" {
				t.Errorf("content-type = %q", ct)
			}
			var body map[string]string
			if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
				t.Fatalf("body %q: %v", rr.Body.String(), err)
			}
			if body["error"] != "unauthenticated" || body["who"] != "sign in again" {
				t.Errorf("body = %v", body)
			}
		})
	}
}

// TestMiddlewareAPIForbiddenIsJSON403: a valid session for an email the
// allowlist refuses gets the API's error shape too.
func TestMiddlewareAPIForbiddenIsJSON403(t *testing.T) {
	mw, codec := newMW(t, "alice@example.com")
	h := mw.Wrap(passthrough())
	tok, _ := codec.EncodeSession(NewSession("eve@example.com"))
	req := httptest.NewRequest(http.MethodGet, "/api/v1/me", nil)
	req.AddCookie(&http.Cookie{Name: CookieName, Value: tok})
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusForbidden {
		t.Fatalf("code = %d, want 403", rr.Code)
	}
	var body map[string]string
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatalf("body %q: %v", rr.Body.String(), err)
	}
	if body["error"] == "" || body["who"] == "" {
		t.Errorf("body = %v, want error and who", body)
	}
}

// TestMiddlewarePagesStillRedirect: the page routes keep redirecting;
// only /api/ changed. "/apiary" shares the letters but not the prefix.
func TestMiddlewarePagesStillRedirect(t *testing.T) {
	mw, _ := newMW(t)
	h := mw.Wrap(passthrough())
	for _, path := range []string{"/app", "/apiary"} {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, req)
		if rr.Code != http.StatusFound {
			t.Errorf("%s: code = %d, want 302", path, rr.Code)
		}
	}
}
