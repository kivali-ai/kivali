package web

import (
	"bufio"
	"compress/gzip"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func serveCompressed(t *testing.T, h http.HandlerFunc, acceptEncoding string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/x", nil)
	if acceptEncoding != "" {
		req.Header.Set("Accept-Encoding", acceptEncoding)
	}
	rec := httptest.NewRecorder()
	compressResponses(h).ServeHTTP(rec, req)
	return rec
}

func gunzip(t *testing.T, b []byte) string {
	t.Helper()
	zr, err := gzip.NewReader(strings.NewReader(string(b)))
	if err != nil {
		t.Fatalf("gzip reader: %v", err)
	}
	out, err := io.ReadAll(zr)
	if err != nil {
		t.Fatalf("gunzip: %v", err)
	}
	return string(out)
}

func TestCompressResponsesGzipsJSON(t *testing.T) {
	body := strings.Repeat(`{"kind":"tool","output":"same text again"},`, 200)
	rec := serveCompressed(t, func(w http.ResponseWriter, _ *http.Request) {
		writeJSONBytes(w, http.StatusOK, []byte(body))
	}, "br, gzip;q=0.8")
	if got := rec.Header().Get("Content-Encoding"); got != "gzip" {
		t.Fatalf("Content-Encoding = %q, want gzip", got)
	}
	if got := rec.Header().Get("Vary"); got != "Accept-Encoding" {
		t.Errorf("Vary = %q", got)
	}
	if rec.Body.Len() >= len(body) {
		t.Errorf("compressed %d bytes into %d", len(body), rec.Body.Len())
	}
	if got := gunzip(t, rec.Body.Bytes()); got != body+"\n" {
		t.Errorf("round trip differs: %d bytes", len(got))
	}
}

func TestCompressResponsesLeavesAlone(t *testing.T) {
	big := strings.Repeat("a", 4*gzipMinBytes)
	cases := []struct {
		name   string
		accept string
		h      http.HandlerFunc
	}{
		{"client does not accept gzip", "identity", func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, big)
		}},
		{"gzip refused with q=0", "gzip;q=0", func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, big)
		}},
		{"event stream", "gzip", func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = io.WriteString(w, big)
		}},
		{"binary type", "gzip", func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/zip")
			_, _ = io.WriteString(w, big)
		}},
		{"short declared length", "gzip", func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "text/plain")
			w.Header().Set("Content-Length", "2")
			_, _ = io.WriteString(w, "ok")
		}},
		{"partial content", "gzip", func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "text/plain")
			w.WriteHeader(http.StatusPartialContent)
			_, _ = io.WriteString(w, big)
		}},
		{"not modified", "gzip", func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusNotModified)
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := serveCompressed(t, tc.h, tc.accept)
			if got := rec.Header().Get("Content-Encoding"); got != "" {
				t.Fatalf("Content-Encoding = %q, want none", got)
			}
		})
	}
}

func TestCompressResponsesSniffsMissingContentType(t *testing.T) {
	page := "<!doctype html><html><body>" + strings.Repeat("<p>hello</p>", 200) + "</body></html>"
	rec := serveCompressed(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, page)
	}, "gzip")
	if got := rec.Header().Get("Content-Encoding"); got != "gzip" {
		t.Fatalf("Content-Encoding = %q, want gzip", got)
	}
	if got := gunzip(t, rec.Body.Bytes()); got != page {
		t.Errorf("round trip differs")
	}
}

// A flushed compressed response reaches the client before the handler
// returns: what is written so far decodes on its own.
func TestCompressResponsesFlushes(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(compressResponses(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		_, _ = io.WriteString(w, "first line\n")
		w.(http.Flusher).Flush()
		<-release
		_, _ = io.WriteString(w, "second line\n")
	})))
	defer srv.Close()
	defer close(release)

	resp, err := http.Get(srv.URL) // the transport asks for gzip and decodes it
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if !resp.Uncompressed {
		t.Fatalf("response was not gzipped")
	}
	line, err := bufio.NewReader(resp.Body).ReadString('\n')
	if err != nil {
		t.Fatal(err)
	}
	if line != "first line\n" {
		t.Errorf("first line = %q", line)
	}
}

// A flush before the first write sends the headers; what is written
// after must match them, compressed or not.
func TestCompressResponsesFlushBeforeWrite(t *testing.T) {
	body := strings.Repeat("the same text again\n", 200)
	srv := httptest.NewServer(compressResponses(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		w.(http.Flusher).Flush()
		_, _ = io.WriteString(w, body)
	})))
	defer srv.Close()
	resp, err := http.Get(srv.URL) // the transport asks for gzip and decodes it
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	got, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != body {
		t.Errorf("body = %d bytes starting %q, want the text written", len(got), got[:min(len(got), 16)])
	}
}

func TestAcceptsGzip(t *testing.T) {
	for header, want := range map[string]bool{
		"":                    false,
		"gzip":                true,
		"GZIP":                true,
		"br, gzip":            true,
		"gzip;q=0":            false,
		"gzip; q=0.5":         true,
		"*":                   true,
		"deflate, br":         false,
		"identity;q=1, *;q=0": false,
	} {
		if got := acceptsGzip(header); got != want {
			t.Errorf("acceptsGzip(%q) = %v, want %v", header, got, want)
		}
	}
}
