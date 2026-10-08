package web

import (
	"compress/gzip"
	"io"
	"mime"
	"net/http"
	"strconv"
	"strings"
	"sync"
)

// gzipMinBytes is the declared length below which a response goes out
// as is: a gzip header and trailer cost about 20 bytes, so a short body
// would grow. A body without a declared length is always compressed;
// the handler cannot know its size before writing it.
const gzipMinBytes = 1024

var gzipWriters = sync.Pool{New: func() any {
	gz, _ := gzip.NewWriterLevel(io.Discard, gzip.DefaultCompression)
	return gz
}}

// compressResponses gzips text responses for clients that accept it:
// the JSON API, the web app's build files, raw messages. It leaves
// alone what would break or not shrink: event streams (each event
// must reach the browser the moment it is flushed, and proxies buffer
// compressed streams), binary types (images, zips, fonts), partial
// content, HEAD, and bodies another layer already encoded.
func compressResponses(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Add("Vary", "Accept-Encoding")
		if r.Method == http.MethodHead || !acceptsGzip(r.Header.Get("Accept-Encoding")) {
			next.ServeHTTP(w, r)
			return
		}
		cw := &compressWriter{ResponseWriter: w}
		defer cw.finish()
		next.ServeHTTP(cw, r)
	})
}

// acceptsGzip reports whether an Accept-Encoding header allows gzip:
// listed (or "*") with a nonzero q.
func acceptsGzip(header string) bool {
	for _, part := range strings.Split(header, ",") {
		name, params, _ := strings.Cut(strings.TrimSpace(part), ";")
		name = strings.ToLower(strings.TrimSpace(name))
		if name != "gzip" && name != "*" {
			continue
		}
		q := 1.0
		if k, v, ok := strings.Cut(strings.TrimSpace(params), "="); ok && strings.TrimSpace(k) == "q" {
			if f, err := strconv.ParseFloat(strings.TrimSpace(v), 64); err == nil {
				q = f
			}
		}
		return q > 0
	}
	return false
}

// compressibleType reports whether a Content-Type is text that gzip
// shrinks. Event streams are text but are never compressed here.
func compressibleType(contentType string) bool {
	mt, _, err := mime.ParseMediaType(contentType)
	if err != nil {
		return false
	}
	switch {
	case mt == "text/event-stream":
		return false
	case strings.HasPrefix(mt, "text/"):
		return true
	}
	switch mt {
	case "application/json", "application/javascript", "application/xml",
		"application/manifest+json", "image/svg+xml":
		return true
	}
	return false
}

// compressWriter decides at the first WriteHeader (or Write) whether
// the response is compressed, from the headers the handler set by then.
type compressWriter struct {
	http.ResponseWriter
	decided bool
	gz      *gzip.Writer
}

func (c *compressWriter) decide(status int) {
	if c.decided {
		return
	}
	c.decided = true
	h := c.Header()
	if status < 200 || status == http.StatusNoContent || status == http.StatusNotModified ||
		status == http.StatusPartialContent || h.Get("Content-Encoding") != "" || h.Get("Content-Range") != "" ||
		!compressibleType(h.Get("Content-Type")) {
		return
	}
	if n, err := strconv.Atoi(h.Get("Content-Length")); err == nil && n < gzipMinBytes {
		return
	}
	h.Del("Content-Length")
	h.Set("Content-Encoding", "gzip")
	gz := gzipWriters.Get().(*gzip.Writer)
	gz.Reset(c.ResponseWriter)
	c.gz = gz
}

func (c *compressWriter) WriteHeader(status int) {
	c.decide(status)
	c.ResponseWriter.WriteHeader(status)
}

func (c *compressWriter) Write(p []byte) (int, error) {
	if !c.decided {
		// net/http sniffs a missing Content-Type on the first write;
		// do the same so the decision sees it.
		if c.Header().Get("Content-Type") == "" {
			c.Header().Set("Content-Type", http.DetectContentType(p))
		}
		c.WriteHeader(http.StatusOK)
	}
	if c.gz != nil {
		return c.gz.Write(p)
	}
	return c.ResponseWriter.Write(p)
}

// Flush sends what is buffered so far: through the compressor first
// when there is one. A flush before anything was written sends the
// headers, so the decision is made here; a later Write must not turn
// on gzip under headers already sent without it.
func (c *compressWriter) Flush() {
	if !c.decided {
		c.WriteHeader(http.StatusOK)
	}
	if c.gz != nil {
		_ = c.gz.Flush()
	}
	if f, ok := c.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// Unwrap lets http.ResponseController reach the connection.
func (c *compressWriter) Unwrap() http.ResponseWriter { return c.ResponseWriter }

func (c *compressWriter) finish() {
	if c.gz == nil {
		return
	}
	_ = c.gz.Close()
	c.gz.Reset(io.Discard)
	gzipWriters.Put(c.gz)
	c.gz = nil
}
