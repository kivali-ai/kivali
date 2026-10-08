package web

import (
	"github.com/kivali-ai/kivali/internal/store"
)

// saveEgressPatterns replaces the egress allowlist. The store trims,
// lowercases, de-duplicates and sorts, and drops blank and comment
// lines; the proxy sidecar reloads on the file's mtime change.
func (s *Server) saveEgressPatterns(patterns []string) error {
	return s.Store.WriteEgressAllowlist(store.EgressAllowlist{Patterns: patterns})
}
