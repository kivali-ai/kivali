package main

import (
	"bytes"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func writeAllowlist(t *testing.T, dir string, lines []string) string {
	t.Helper()
	p := filepath.Join(dir, "allowlist.yaml")
	body := "patterns:\n"
	for _, l := range lines {
		// Quote: YAML treats `*` as anchor/alias, so `*.pypi.org` must
		// be a quoted scalar to round-trip intact.
		body += "  - \"" + l + "\"\n"
	}
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestAllowlistPatterns(t *testing.T) {
	dir := t.TempDir()
	path := writeAllowlist(t, dir, []string{"pypi.org", "*.pypi.org", "deb.debian.org"})
	var a Allowlist
	if err := a.Load(path); err != nil {
		t.Fatalf("Load: %v", err)
	}
	cases := map[string]bool{
		"pypi.org":           true,
		"files.pypi.org":     true,
		"a.b.pypi.org":       true,
		"deb.debian.org":     true,
		"deb.debian.org:80":  true,
		"pypi.org.evil.com":  false, // suffix-match must not leak
		"fakedeb.debian.org": false, // label boundary, not raw suffix
		"example.com":        false,
	}
	for host, want := range cases {
		if got := a.Allowed(host); got != want {
			t.Errorf("Allowed(%q) = %v, want %v", host, got, want)
		}
	}
}

// A bare domain is implicitly wildcarded downward: apex plus every
// subdomain, at any depth, without the operator writing "*.".
func TestAllowlistBareDomainCoversSubdomains(t *testing.T) {
	dir := t.TempDir()
	path := writeAllowlist(t, dir, []string{"example.com"})
	var a Allowlist
	if err := a.Load(path); err != nil {
		t.Fatalf("Load: %v", err)
	}
	cases := map[string]bool{
		"example.com":       true,
		"api.example.com":   true,
		"a.b.c.example.com": true,
		"notexample.com":    false,
		"example.com.evil":  false,
		"example.org":       false,
	}
	for host, want := range cases {
		if got := a.Allowed(host); got != want {
			t.Errorf("Allowed(%q) = %v, want %v", host, got, want)
		}
	}
}

// An explicit subdomain entry scopes downward from that subdomain —
// its own children are in, the parent apex stays out.
func TestAllowlistSubdomainEntryDoesNotCoverApex(t *testing.T) {
	dir := t.TempDir()
	path := writeAllowlist(t, dir, []string{"api.example.com"})
	var a Allowlist
	if err := a.Load(path); err != nil {
		t.Fatalf("Load: %v", err)
	}
	cases := map[string]bool{
		"api.example.com":    true,
		"v2.api.example.com": true,
		"example.com":        false,
		"www.example.com":    false,
		"notapi.example.com": false,
	}
	for host, want := range cases {
		if got := a.Allowed(host); got != want {
			t.Errorf("Allowed(%q) = %v, want %v", host, got, want)
		}
	}
}

// A "*." entry is taken as written and means exactly what the bare
// form means — operators with existing wildcards see no change.
func TestAllowlistWildcardEqualsBareForm(t *testing.T) {
	dir := t.TempDir()
	var wild, bare Allowlist
	if err := wild.Load(writeAllowlist(t, t.TempDir(), []string{"*.example.com"})); err != nil {
		t.Fatalf("Load wildcard: %v", err)
	}
	if err := bare.Load(writeAllowlist(t, dir, []string{"example.com"})); err != nil {
		t.Fatalf("Load bare: %v", err)
	}
	for _, host := range []string{"example.com", "api.example.com", "a.b.example.com", "notexample.com", "example.org"} {
		if w, b := wild.Allowed(host), bare.Allowed(host); w != b {
			t.Errorf("Allowed(%q): wildcard=%v bare=%v, want equal", host, w, b)
		}
	}
}

// A glob matches label by label: "*" stays inside its label, a class
// holds one character, and the names it matches bring their subdomains.
// The default Bedrock and Vertex entries reach every region's endpoint
// and nothing a stranger can name under the same parent.
func TestAllowlistGlobMatchesLabelByLabel(t *testing.T) {
	var a Allowlist
	if err := a.Load(writeAllowlist(t, t.TempDir(), []string{
		"bedrock-runtime.[a-z][a-z]-*-[0-9].amazonaws.com",
		"sts.[a-z][a-z]-*-[0-9].amazonaws.com",
		"*-aiplatform.googleapis.com",
		"aiplatform.*.rep.googleapis.com",
		"bad[.example.com",
	})); err != nil {
		t.Fatalf("Load: %v", err)
	}
	cases := map[string]bool{
		"bedrock-runtime.us-east-1.amazonaws.com":      true,
		"bedrock-runtime.ap-southeast-2.amazonaws.com": true,
		"bedrock-runtime.us-gov-west-1.amazonaws.com":  true,
		"bedrock-runtime.eu-west-3.amazonaws.com:443":  true,
		"x.bedrock-runtime.us-east-1.amazonaws.com":    true,
		"sts.eu-central-1.amazonaws.com":               true,
		"us-east5-aiplatform.googleapis.com":           true,
		"europe-west1-aiplatform.googleapis.com":       true,
		"aiplatform.eu.rep.googleapis.com":             true,
		// S3 names a bucket can own under amazonaws.com.
		"bedrock-runtime.s3.amazonaws.com":             false,
		"bedrock-runtime.s3-us-west-2.amazonaws.com":   false,
		"bedrock-runtime.evil.s3.amazonaws.com":        false,
		"bedrock-runtime.us-east-1.amazonaws.com.evil": false,
		"bedrock-runtime.us-east-1.evil.amazonaws.com": false,
		"s3.us-east-1.amazonaws.com":                   false,
		"amazonaws.com":                                false,
		"storage.googleapis.com":                       false,
		"aiplatform.googleapis.com.evil.io":            false,
		"us-east5-aiplatform.googleapis.com.evil.io":   false,
		"aiplatform.eu.rep.googleapis.com.evil.io":     false,
		"aiplatform.a.b.rep.googleapis.com":            false,
	}
	for host, want := range cases {
		if got := a.Allowed(host); got != want {
			t.Errorf("Allowed(%q) = %v, want %v", host, got, want)
		}
	}
}

// The default Microsoft Foundry entries reach every resource's
// endpoint and Entra ID's token host, and stop at the label boundary:
// "*." covers everything under services.ai.azure.com and nothing that
// merely ends in it or carries it as a prefix.
func TestAllowlistFoundryDefaults(t *testing.T) {
	var a Allowlist
	if err := a.Load(writeAllowlist(t, t.TempDir(), []string{"*.services.ai.azure.com", "login.microsoftonline.com"})); err != nil {
		t.Fatalf("Load: %v", err)
	}
	cases := map[string]bool{
		"my-resource.services.ai.azure.com":      true,
		"my-resource.services.ai.azure.com:443":  true,
		"login.microsoftonline.com":              true,
		"login.microsoftonline.com:443":          true,
		"services.ai.azure.com.evil.io":          false,
		"my-resource.services.ai.azure.com.evil": false,
		"evilservices.ai.azure.com":              false,
		"ai.azure.com":                           false,
		"my-resource.openai.azure.com":           false,
		"evil-login.microsoftonline.com.io":      false,
		"microsoftonline.com":                    false,
	}
	for host, want := range cases {
		if got := a.Allowed(host); got != want {
			t.Errorf("Allowed(%q) = %v, want %v", host, got, want)
		}
	}
}

// Nothing lives under an IP address, so an IP literal matches only
// itself — no phantom "subdomain" of an address.
func TestAllowlistIPLiteralExactOnly(t *testing.T) {
	dir := t.TempDir()
	path := writeAllowlist(t, dir, []string{"10.0.0.5"})
	var a Allowlist
	if err := a.Load(path); err != nil {
		t.Fatalf("Load: %v", err)
	}
	cases := map[string]bool{
		"10.0.0.5":      true,
		"10.0.0.5:443":  true,
		"host.10.0.0.5": false,
		"10.0.0.6":      false,
	}
	for host, want := range cases {
		if got := a.Allowed(host); got != want {
			t.Errorf("Allowed(%q) = %v, want %v", host, got, want)
		}
	}
}

func TestAllowlistEmptyFile(t *testing.T) {
	dir := t.TempDir()
	// No file at all → empty allowlist, deny-all.
	var a Allowlist
	if err := a.Load(filepath.Join(dir, "none.yaml")); err != nil {
		t.Fatalf("Load missing: %v", err)
	}
	if a.Allowed("pypi.org") {
		t.Error("deny-by-default expected")
	}
}

func TestWatcherReloadsOnChange(t *testing.T) {
	dir := t.TempDir()
	path := writeAllowlist(t, dir, []string{"one.example.com"})
	var a Allowlist
	if err := a.Load(path); err != nil {
		t.Fatal(err)
	}
	if a.Allowed("two.example.com") {
		t.Fatal("precondition")
	}
	_ = writeAllowlist(t, dir, []string{"two.example.com"})
	// Ensure mtime is distinct.
	time.Sleep(10 * time.Millisecond)
	_ = os.Chtimes(path, time.Now(), time.Now())

	w := &Watcher{Path: path}
	if err := w.pollOnce(&a); err != nil {
		t.Fatalf("pollOnce: %v", err)
	}
	if !a.Allowed("two.example.com") {
		t.Errorf("did not pick up new allowlist: %v", a.Patterns())
	}
}

func TestConnectAllowed(t *testing.T) {
	// Stand up a toy TCP echo server; the proxy should tunnel raw
	// bytes to it when the host is allowed.
	echo := newEchoServer(t)
	defer echo.Close()

	host, _, _ := net.SplitHostPort(echo.host)
	var a Allowlist
	a.patterns.Store(&[]string{host})

	srv := httptest.NewServer(&proxy{allow: &a})
	defer srv.Close()

	conn, err := net.Dial("tcp", srv.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	_, _ = fmt.Fprintf(conn, "CONNECT %s HTTP/1.1\r\nHost: %s\r\n\r\n", echo.Addr(), echo.Addr())
	buf := make([]byte, 256)
	n, err := conn.Read(buf)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if !strings.Contains(string(buf[:n]), "200") {
		t.Fatalf("unexpected response: %s", buf[:n])
	}
	// Now it's a raw tunnel; write and read back.
	if _, err := conn.Write([]byte("hi\n")); err != nil {
		t.Fatal(err)
	}
	_ = conn.SetReadDeadline(time.Now().Add(1 * time.Second))
	n, err = conn.Read(buf)
	if err != nil {
		t.Fatalf("tunnel read: %v", err)
	}
	if !strings.Contains(string(buf[:n]), "hi") {
		t.Errorf("tunnel echo = %q", buf[:n])
	}
}

func TestConnectDenied(t *testing.T) {
	var a Allowlist
	a.patterns.Store(&[]string{"allowed.example.com"})
	srv := httptest.NewServer(&proxy{allow: &a})
	defer srv.Close()
	conn, err := net.Dial("tcp", srv.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	_, _ = fmt.Fprintf(conn, "CONNECT denied.example.com:443 HTTP/1.1\r\nHost: denied.example.com:443\r\n\r\n")
	buf := make([]byte, 256)
	n, _ := conn.Read(buf)
	if !strings.Contains(string(buf[:n]), "403") {
		t.Errorf("expected 403, got %s", buf[:n])
	}
}

func TestHTTPProxyAllowed(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = fmt.Fprintf(w, "hello %s", r.URL.Path)
	}))
	defer upstream.Close()
	upURL, _ := url.Parse(upstream.URL)

	var a Allowlist
	a.patterns.Store(&[]string{upURL.Hostname()})

	proxySrv := httptest.NewServer(&proxy{allow: &a})
	defer proxySrv.Close()

	client := &http.Client{
		Transport: &http.Transport{
			Proxy: http.ProxyURL(mustURL(t, proxySrv.URL)),
		},
	}
	resp, err := client.Get(upstream.URL + "/ping")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(resp.Body)
	if string(body) != "hello /ping" {
		t.Errorf("body = %q", body)
	}
}

func TestHTTPProxyDenied(t *testing.T) {
	var a Allowlist
	a.patterns.Store(&[]string{"nothing.example.com"})
	proxySrv := httptest.NewServer(&proxy{allow: &a})
	defer proxySrv.Close()

	client := &http.Client{Transport: &http.Transport{Proxy: http.ProxyURL(mustURL(t, proxySrv.URL))}}
	resp, err := client.Get("http://denied.example.com/x")
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("status = %d", resp.StatusCode)
	}
}

func mustURL(t *testing.T, raw string) *url.URL {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	return u
}

// echoServer is a minimal TCP echo server used by the CONNECT tests.
// It records a hit counter so tests can assert the tunnel actually
// delivered bytes to the far side.
type echoServer struct {
	ln   net.Listener
	host string
	hits atomic.Int32
}

func newEchoServer(t *testing.T) *echoServer {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	e := &echoServer{ln: ln, host: ln.Addr().String()}
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer func() { _ = c.Close() }()
				e.hits.Add(1)
				var buf bytes.Buffer
				b := make([]byte, 256)
				n, _ := c.Read(b)
				buf.Write(b[:n])
				_, _ = c.Write(buf.Bytes())
			}(c)
		}
	}()
	return e
}
func (e *echoServer) Addr() string { return e.host }
func (e *echoServer) Close()       { _ = e.ln.Close() }
