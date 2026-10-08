package main

import (
	"testing"

	"github.com/kivali-ai/kivali/internal/claudeauth"
)

// The fake's `auth status --json` reads as a signed-in subscription,
// so a demo or e2e server reports a model connected.
func TestAuthStatusParses(t *testing.T) {
	st, err := claudeauth.Parse([]byte(authStatus))
	if err != nil || !st.LoggedIn || st.Billing() != "Claude Max" {
		t.Fatalf("auth status %+v billing %q, %v", st, st.Billing(), err)
	}
}
