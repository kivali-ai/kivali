package web

import (
	"strings"
)

// normalizeForDiff collapses surface-level differences that aren't
// real content changes before diffing. Two sources of noise would
// otherwise show as "the whole file changed" when only a single line
// did:
//
//  1. CRLF vs LF mismatch. The proposed body arrives as a JSON string
//     from the model (always LF), but the on-disk current file may be
//     CRLF (a hand-edit submitted via the Settings textarea from a
//     Windows browser, or a paste from a CRLF source). Without
//     stripping \r, every line differs by a trailing \r.
//
//  2. Trailing-newline drift through the YAML round-trip. The proposed
//     body is persisted on the approval-request message as a YAML
//     block scalar; the gopkg.in/yaml.v3 parser sometimes drops the
//     scalar's final \n when there's no blank line between it and the
//     next frontmatter key. The on-disk role.md / handbook still
//     ends in \n. So even when no content changed, the last line
//     boundary differs, producing a spurious "-" line at EOF.
//
// Fix: strip embedded CRs and collapse trailing newlines to exactly
// one. Both sides we diff are file-shaped markdown ending in \n, so
// this is content-preserving; the diff now focuses on actual edits.
func normalizeForDiff(s string) string {
	if strings.ContainsRune(s, '\r') {
		s = strings.ReplaceAll(s, "\r", "")
	}
	s = strings.TrimRight(s, "\n") + "\n"
	return s
}
