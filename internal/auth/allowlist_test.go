package auth

import "testing"

func TestAllowlistEmptyAdmitsNobody(t *testing.T) {
	a := NewAllowlist()
	if a.Allow("anyone@example.com") {
		t.Error("an empty allowlist admitted someone")
	}
	if a.Allow("") {
		t.Error("an empty email was admitted")
	}
}

func TestAllowlistAdmitsOnlyOwners(t *testing.T) {
	a := NewAllowlist("owner@example.com", " second@example.com ", "")
	for _, e := range []string{"owner@example.com", "second@example.com"} {
		if !a.Allow(e) {
			t.Errorf("%s should be allowed", e)
		}
	}
	if a.Allow("stranger@example.com") {
		t.Error("non-owner should be denied")
	}
	if a.Allow("") {
		t.Error("a blank owner entry admitted the empty email")
	}
}

func TestAllowlistCaseInsensitive(t *testing.T) {
	a := NewAllowlist("Owner@Example.COM")
	if !a.Allow("owner@example.com") {
		t.Error("owner match should be case-insensitive")
	}
	if !a.Allow(" OWNER@EXAMPLE.COM ") {
		t.Error("owner match should be case-insensitive and trimmed (input)")
	}
}
