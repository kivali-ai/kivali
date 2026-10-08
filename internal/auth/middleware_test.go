package auth

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func newMW(t *testing.T, allowedEmails ...string) (*Middleware, *Codec) {
	t.Helper()
	codec, err := NewCodec([]byte("01234567890123456789012345678901"))
	if err != nil {
		t.Fatalf("codec: %v", err)
	}
	al := NewAllowlist(allowedEmails...)
	return &Middleware{Codec: codec, Allowlist: al}, codec
}

func passthrough() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if email := UserFromContext(r.Context()); email != "" {
			w.Header().Set("x-test-user", email)
		}
		w.WriteHeader(http.StatusOK)
	})
}

func TestMiddlewareRedirectsOnMissingCookie(t *testing.T) {
	mw, _ := newMW(t)
	h := mw.Wrap(passthrough())

	req := httptest.NewRequest(http.MethodGet, "/agents/alice", nil)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusFound {
		t.Fatalf("code = %d", rr.Code)
	}
	loc := rr.Header().Get("location")
	if !strings.HasPrefix(loc, "/auth/login") || !strings.Contains(loc, "next=%2Fagents%2Falice") {
		t.Errorf("location = %q", loc)
	}
}

func TestMiddlewareRejectsNonGET(t *testing.T) {
	mw, _ := newMW(t)
	h := mw.Wrap(passthrough())
	req := httptest.NewRequest(http.MethodPost, "/hire", nil)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusUnauthorized {
		t.Errorf("code = %d, want 401", rr.Code)
	}
}

func TestMiddlewareAllowsValidSession(t *testing.T) {
	mw, codec := newMW(t, "alice@example.com")
	h := mw.Wrap(passthrough())

	tok, _ := codec.EncodeSession(NewSession("alice@example.com"))
	req := httptest.NewRequest(http.MethodGet, "/agents/alice", nil)
	req.AddCookie(&http.Cookie{Name: CookieName, Value: tok})
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Errorf("code = %d", rr.Code)
	}
	if rr.Header().Get("x-test-user") != "alice@example.com" {
		t.Errorf("user not injected: %q", rr.Header().Get("x-test-user"))
	}
}

func TestMiddlewareForbidsNonAllowlisted(t *testing.T) {
	mw, codec := newMW(t, "alice@example.com")
	h := mw.Wrap(passthrough())
	tok, _ := codec.EncodeSession(NewSession("eve@example.com"))
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.AddCookie(&http.Cookie{Name: CookieName, Value: tok})
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusForbidden {
		t.Errorf("code = %d", rr.Code)
	}
}

func TestMiddlewareRejectsTamperedCookie(t *testing.T) {
	mw, codec := newMW(t, "alice@example.com")
	h := mw.Wrap(passthrough())
	tok, _ := codec.EncodeSession(NewSession("alice@example.com"))
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.AddCookie(&http.Cookie{Name: CookieName, Value: tok + "x"})
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusFound {
		t.Errorf("code = %d, want 302 (redirect)", rr.Code)
	}
}
