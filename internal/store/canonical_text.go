package store

import (
	"context"
	"io"
	"strings"
)

// MaxInlinedFileBytes is the pragmatic ceiling when a caller inlines a
// file's text into a prompt instead of serving it via file_view. The
// file_view path has no cap — agents read full canonical text via
// tool_result — so this applies where text is read into memory whole:
// attachment and graph snapshot reads. We set it high (10 MiB) so normal files
// pass through intact; the cap exists only to stop a runaway file from
// exhausting process memory in one gulp. Truncation, when it happens,
// is surfaced with a loud marker so it is never silent.
const MaxInlinedFileBytes = 10 << 20

// ReadCanonicalText returns the text content of a project file for
// inlining into agent prompts: a text canonical (.txt, .md, any
// text/* MIME) is read; a file with no text canonical (images, unknown
// types) reads as the empty string.
//
// Result is capped at maxInlinedFileBytes with a truncation marker.
func (s *FSStore) ReadCanonicalText(ctx context.Context, sha string) (string, error) {
	pf, err := s.GetProjectFile(sha)
	if err != nil {
		return "", err
	}
	if pf.CanonicalName == "" {
		return "", nil
	}
	lower := strings.ToLower(pf.CanonicalName)
	if strings.HasSuffix(lower, ".txt") || strings.HasSuffix(lower, ".md") || strings.HasPrefix(pf.MIME, "text/") {
		return s.readCanonicalFile(pf)
	}
	return "", nil
}

func (s *FSStore) readCanonicalFile(pf ProjectFile) (string, error) {
	r, err := s.OpenCanonical(pf.SHA)
	if err != nil {
		return "", err
	}
	defer func() { _ = r.Close() }()
	return readCapped(r, MaxInlinedFileBytes)
}

func readCapped(r io.Reader, cap int) (string, error) {
	b, err := io.ReadAll(io.LimitReader(r, int64(cap)+1))
	if err != nil {
		return "", err
	}
	if len(b) > cap {
		return truncate(string(b[:cap]), cap), nil
	}
	return string(b), nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "\n\n" +
		"########################################################################\n" +
		"[!] FILE TRUNCATED AT " + humanBytes(int64(n)) + " — content beyond this point is NOT in this request.\n" +
		"[!] The file_view tool (file_view /files/...) returns the full file without truncation.\n" +
		"########################################################################\n"
}

func humanBytes(n int64) string {
	switch {
	case n >= 1<<20:
		return itoa(n>>20) + " MiB"
	case n >= 1<<10:
		return itoa(n>>10) + " KiB"
	}
	return itoa(n) + " B"
}

func itoa(n int64) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		b[i] = '-'
	}
	return string(b[i:])
}
