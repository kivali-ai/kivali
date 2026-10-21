package egresscred

import (
	"bytes"
	"encoding/base64"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"sync"
)

// Placeholder is what a Scrubber writes in place of the credential.
const Placeholder = "[proxy credential]"

// Scrubber replaces a credential with Placeholder in each form it can
// be written: the whole credentialed proxy URL, the base64 of
// `<slug>:<secret>` that a Proxy-Authorization header carries, and the
// secret itself. A nil *Scrubber passes everything through unchanged.
type Scrubber struct {
	needles  [][]byte
	replacer *strings.Replacer
}

// NewScrubber is the scrubber for secret, presented as slug, inside
// proxyURL. proxyURL may be empty.
func NewScrubber(slug, secret, proxyURL string) *Scrubber {
	if secret == "" {
		return nil
	}
	set := map[string]bool{
		secret: true,
		base64.StdEncoding.EncodeToString([]byte(slug + ":" + secret)): true,
	}
	if proxyURL != "" {
		set[proxyURL] = true
	}
	return newScrubber(set)
}

// Combine is the scrubber for every credential the given scrubbers
// know: a runtime that holds two credentials scrubs both from what it
// relays. Nil scrubbers are skipped; nil when all are nil.
func Combine(scrubbers ...*Scrubber) *Scrubber {
	set := map[string]bool{}
	for _, s := range scrubbers {
		if s == nil {
			continue
		}
		for _, n := range s.needles {
			set[string(n)] = true
		}
	}
	if len(set) == 0 {
		return nil
	}
	return newScrubber(set)
}

// newScrubber replaces each needle in set.
func newScrubber(set map[string]bool) *Scrubber {
	needles := make([]string, 0, len(set))
	for n := range set {
		needles = append(needles, n)
	}
	// Longest first: strings.Replacer tries its pairs in argument
	// order at each position, so the whole URL wins over the secret
	// inside it.
	sort.Slice(needles, func(i, j int) bool {
		if len(needles[i]) != len(needles[j]) {
			return len(needles[i]) > len(needles[j])
		}
		return needles[i] < needles[j]
	})
	s := &Scrubber{}
	pairs := make([]string, 0, 2*len(needles))
	for _, n := range needles {
		s.needles = append(s.needles, []byte(n))
		pairs = append(pairs, n, Placeholder)
	}
	s.replacer = strings.NewReplacer(pairs...)
	return s
}

// String is text with the credential replaced.
func (s *Scrubber) String(text string) string {
	if s == nil {
		return text
	}
	return s.replacer.Replace(text)
}

// Bytes is b with the credential replaced. b itself is returned when
// it holds none.
func (s *Scrubber) Bytes(b []byte) []byte {
	if s == nil || !s.contains(b) {
		return b
	}
	return []byte(s.replacer.Replace(string(b)))
}

func (s *Scrubber) contains(b []byte) bool {
	for _, n := range s.needles {
		if bytes.Contains(b, n) {
			return true
		}
	}
	return false
}

// Writer is a writer that scrubs what it passes to w. A write that ends
// part way into a needle is held until the next write or Flush shows
// whether it completes, so the caller must Flush once the last write is
// done. Safe for concurrent use.
type Writer struct {
	s *Scrubber
	w io.Writer

	mu      sync.Mutex
	pending []byte
}

// Writer wraps w. A nil Scrubber's Writer passes writes straight
// through.
func (s *Scrubber) Writer(w io.Writer) *Writer { return &Writer{s: s, w: w} }

// Write scrubs p and writes all but the tail that could still be the
// start of a needle. It reports len(p) written unless w fails.
func (sw *Writer) Write(p []byte) (int, error) {
	if sw.s == nil {
		return sw.w.Write(p)
	}
	sw.mu.Lock()
	defer sw.mu.Unlock()
	out, rest := sw.s.scan(append(sw.pending, p...))
	sw.pending = append([]byte(nil), rest...)
	if len(out) > 0 {
		if _, err := sw.w.Write(out); err != nil {
			return 0, err
		}
	}
	return len(p), nil
}

// Flush writes what Write held back.
func (sw *Writer) Flush() error {
	if sw.s == nil {
		return nil
	}
	sw.mu.Lock()
	defer sw.mu.Unlock()
	if len(sw.pending) == 0 {
		return nil
	}
	out := sw.s.Bytes(sw.pending)
	sw.pending = nil
	_, err := sw.w.Write(out)
	return err
}

// scan scrubs buf up to the first position where what is left could be
// the start of a needle that has not arrived whole, and returns that
// remainder unscrubbed. At each position the needles are tried longest
// first, as Bytes does, so the result once the remainder is flushed is
// the same as scrubbing the whole stream at once.
func (s *Scrubber) scan(buf []byte) (out, rest []byte) {
	out = make([]byte, 0, len(buf))
	i := 0
next:
	for i < len(buf) {
		tail := buf[i:]
		for _, n := range s.needles {
			if bytes.HasPrefix(tail, n) {
				out = append(out, Placeholder...)
				i += len(n)
				continue next
			}
			if len(tail) < len(n) && bytes.HasPrefix(n, tail) {
				return out, tail
			}
		}
		out = append(out, buf[i])
		i++
	}
	return out, nil
}

// RoundTripper wraps next so every request it sends has the credential
// replaced in its body and query. A nil Scrubber returns next.
func (s *Scrubber) RoundTripper(next http.RoundTripper) http.RoundTripper {
	if s == nil {
		return next
	}
	if next == nil {
		next = http.DefaultTransport
	}
	return &scrubTransport{s: s, next: next}
}

type scrubTransport struct {
	s    *Scrubber
	next http.RoundTripper
}

func (t *scrubTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	query := req.URL.RawQuery
	if t.s.contains([]byte(query)) {
		query = strings.ReplaceAll(t.s.String(query), Placeholder, url.QueryEscape(Placeholder))
	}
	if (req.Body == nil || req.Body == http.NoBody) && query == req.URL.RawQuery {
		return t.next.RoundTrip(req)
	}
	out := req.Clone(req.Context())
	out.URL.RawQuery = query
	if req.Body != nil && req.Body != http.NoBody {
		body, err := io.ReadAll(req.Body)
		_ = req.Body.Close()
		if err != nil {
			return nil, err
		}
		body = t.s.Bytes(body)
		out.Body = io.NopCloser(bytes.NewReader(body))
		out.ContentLength = int64(len(body))
		out.GetBody = func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(body)), nil }
	}
	return t.next.RoundTrip(out)
}
