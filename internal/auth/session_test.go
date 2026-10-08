package auth

import (
	"strings"
	"testing"
	"time"
)

func TestNewCodecRejectsShortKey(t *testing.T) {
	if _, err := NewCodec([]byte("short")); err == nil {
		t.Fatal("expected error for short key")
	}
}

func TestSignVerifyRoundtrip(t *testing.T) {
	c, _ := NewCodec([]byte("01234567890123456789012345678901"))
	payload := []byte("hello world")
	signed := c.Sign(payload)
	got, err := c.Verify(signed)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if string(got) != "hello world" {
		t.Errorf("got %q", got)
	}
}

func TestVerifyDetectsTampering(t *testing.T) {
	c, _ := NewCodec([]byte("01234567890123456789012345678901"))
	signed := c.Sign([]byte("original payload bytes"))
	// Flip the first byte of the payload segment to produce a different
	// base64 body with the same signature — Verify should reject it.
	dot := strings.Index(signed, ".")
	if dot < 1 {
		t.Fatalf("expected dot in %q", signed)
	}
	flip := byte('A')
	if signed[0] == 'A' {
		flip = 'B'
	}
	tampered := string(flip) + signed[1:]
	if _, err := c.Verify(tampered); err == nil {
		t.Fatal("expected error on tampered token")
	}
}

func TestVerifyRejectsBadFormat(t *testing.T) {
	c, _ := NewCodec([]byte("01234567890123456789012345678901"))
	if _, err := c.Verify("nodot"); err == nil {
		t.Error("expected error")
	}
}

func TestSessionRoundtrip(t *testing.T) {
	c, _ := NewCodec([]byte("01234567890123456789012345678901"))
	s := NewSession("User@Example.COM")
	if s.Email != "user@example.com" {
		t.Errorf("email = %q", s.Email)
	}
	if s.ExpiresAt.Before(s.IssuedAt) {
		t.Errorf("exp < iat")
	}
	token, err := c.EncodeSession(s)
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	got, err := c.DecodeSession(token)
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if got.Email != s.Email {
		t.Errorf("email = %q", got.Email)
	}
}

func TestSessionExpired(t *testing.T) {
	c, _ := NewCodec([]byte("01234567890123456789012345678901"))
	s := Session{Email: "a@b.com", IssuedAt: time.Now().Add(-2 * time.Hour), ExpiresAt: time.Now().Add(-time.Hour)}
	token, _ := c.EncodeSession(s)
	if _, err := c.DecodeSession(token); err == nil {
		t.Fatal("expected error for expired session")
	}
}

func TestVerifyAcrossCodecsRejected(t *testing.T) {
	a, _ := NewCodec([]byte("11111111111111111111111111111111"))
	b, _ := NewCodec([]byte("22222222222222222222222222222222"))
	token := a.Sign([]byte("hi"))
	if _, err := b.Verify(token); err == nil {
		t.Error("expected signature mismatch across keys")
	}
}
