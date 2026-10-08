package auth

import "strings"

// Allowlist is the set of Google accounts that can sign in: the team's
// owner (OWNER_EMAILS), fixed at construction. Emails compare
// case-insensitively.
type Allowlist struct {
	owners map[string]struct{}
}

// NewAllowlist returns an Allowlist admitting exactly owners.
func NewAllowlist(owners ...string) *Allowlist {
	o := make(map[string]struct{}, len(owners))
	for _, e := range owners {
		e = strings.ToLower(strings.TrimSpace(e))
		if e != "" {
			o[e] = struct{}{}
		}
	}
	return &Allowlist{owners: o}
}

// Allow reports whether email is one of the owners.
func (a *Allowlist) Allow(email string) bool {
	email = strings.ToLower(strings.TrimSpace(email))
	if email == "" {
		return false
	}
	_, ok := a.owners[email]
	return ok
}
