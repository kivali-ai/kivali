package egresscred

import (
	"bytes"
	"encoding/base64"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

func TestNewSecretIs32BytesBase64URLUnpadded(t *testing.T) {
	a, err := NewSecret()
	if err != nil {
		t.Fatal(err)
	}
	b, _ := NewSecret()
	if a == b {
		t.Fatal("two secrets are equal")
	}
	raw, err := base64.RawURLEncoding.DecodeString(a)
	if err != nil {
		t.Fatalf("not base64url without padding: %q: %v", a, err)
	}
	if len(raw) != SecretBytes {
		t.Fatalf("secret holds %d bytes, want %d", len(raw), SecretBytes)
	}
	if strings.ContainsAny(a, "=+/") {
		t.Fatalf("secret %q has padding or standard base64 characters", a)
	}
}

func TestHashIsLowercaseHexSHA256(t *testing.T) {
	// sha256("abc")
	const want = "ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad"
	if got := Hash("abc"); got != want {
		t.Fatalf("Hash = %s, want %s", got, want)
	}
	if !ValidHash(want) {
		t.Fatal("ValidHash refuses a hash")
	}
	for _, bad := range []string{"", strings.ToUpper(want), want[:63], want + "0", strings.Repeat("g", 64)} {
		if ValidHash(bad) {
			t.Errorf("ValidHash(%q) = true", bad)
		}
	}
}

func TestProxyURLCarriesTheCredential(t *testing.T) {
	got, err := ProxyURL("http://10.43.0.12:3128", "alice", "s3cr-t_")
	if err != nil {
		t.Fatal(err)
	}
	if got != "http://alice:s3cr-t_@10.43.0.12:3128" {
		t.Fatalf("ProxyURL = %q", got)
	}
	u, _ := url.Parse(got)
	if p, _ := u.User.Password(); p != "s3cr-t_" || u.User.Username() != "alice" {
		t.Fatalf("user info = %v", u.User)
	}
	for _, bare := range []string{"", "10.0.0.1:3128", "http://x:y@10.0.0.1:3128"} {
		if _, err := ProxyURL(bare, "alice", "s"); err == nil {
			t.Errorf("ProxyURL(%q) accepted", bare)
		}
	}
}

func TestCredentialEnvSetsEveryProxyVariable(t *testing.T) {
	c, err := New("http://10.43.0.12:3128", "alice")
	if err != nil {
		t.Fatal(err)
	}
	env := c.Env()
	if len(env) != 4 {
		t.Fatalf("env = %v", env)
	}
	for i, k := range []string{"HTTP_PROXY", "HTTPS_PROXY", "http_proxy", "https_proxy"} {
		if env[i] != k+"="+c.URL {
			t.Errorf("env[%d] = %q", i, env[i])
		}
	}
	u, _ := url.Parse(c.URL)
	secret, _ := u.User.Password()
	if c.Hash != Hash(secret) {
		t.Fatal("Hash is not the hash of the secret in the URL")
	}
}

// fixture is a credential whose secret is known to the test.
func fixture(t *testing.T) (*Credential, string) {
	t.Helper()
	const secret = "Zm9vYmFyLWJhei1xdXV4LXNlY3JldC12YWx1ZS0xMjM"
	c, err := newCredential("http://10.43.0.12:3128", "alice", secret)
	if err != nil {
		t.Fatal(err)
	}
	return c, secret
}

func TestScrubberReplacesEveryFormOfTheCredential(t *testing.T) {
	c, secret := fixture(t)
	basic := base64.StdEncoding.EncodeToString([]byte("alice:" + secret))
	for name, in := range map[string]string{
		"url":    "proxy error dialing " + c.URL + "/ after 3 tries",
		"basic":  "Proxy-Authorization: Basic " + basic,
		"secret": `{"text":"the key is ` + secret + `"}`,
		"twice":  secret + " and " + secret,
	} {
		out := c.Scrubber.String(in)
		if strings.Contains(out, secret) || strings.Contains(out, basic) {
			t.Errorf("%s: %q still holds the credential", name, out)
		}
		if !strings.Contains(out, Placeholder) {
			t.Errorf("%s: %q has no placeholder", name, out)
		}
		if got := string(c.Scrubber.Bytes([]byte(in))); got != out {
			t.Errorf("%s: Bytes = %q, String = %q", name, got, out)
		}
	}
	// The whole URL goes, not only the secret inside it.
	if out := c.Scrubber.String(c.URL); out != Placeholder {
		t.Errorf("URL scrubs to %q, want %q", out, Placeholder)
	}
	clean := []byte("nothing to see")
	if got := c.Scrubber.Bytes(clean); &got[0] != &clean[0] {
		t.Error("Bytes copied clean input")
	}
}

func TestNilScrubberPassesThrough(t *testing.T) {
	var s *Scrubber
	if s.String("x") != "x" || string(s.Bytes([]byte("y"))) != "y" {
		t.Fatal("nil scrubber changed its input")
	}
	var buf bytes.Buffer
	w := s.Writer(&buf)
	_, _ = w.Write([]byte("z"))
	_ = w.Flush()
	if buf.String() != "z" {
		t.Fatalf("nil scrubber writer wrote %q", buf.String())
	}
	if NewScrubber("alice", "", "") != nil {
		t.Fatal("a scrubber for no secret")
	}
}

// A needle split across writes, at every possible split point, never
// reaches the underlying writer.
func TestWriterScrubsAcrossWriteBoundaries(t *testing.T) {
	c, secret := fixture(t)
	text := "stderr: proxy " + c.URL + " refused; raw " + secret + " end\n"
	want := c.Scrubber.String(text)
	for split := 0; split <= len(text); split++ {
		var buf bytes.Buffer
		w := c.Scrubber.Writer(&buf)
		n1, _ := w.Write([]byte(text[:split]))
		n2, _ := w.Write([]byte(text[split:]))
		if n1+n2 != len(text) {
			t.Fatalf("split %d: reported %d bytes written", split, n1+n2)
		}
		if err := w.Flush(); err != nil {
			t.Fatal(err)
		}
		if buf.String() != want {
			t.Fatalf("split %d: wrote %q, want %q", split, buf.String(), want)
		}
	}
	// Byte at a time.
	var buf bytes.Buffer
	w := c.Scrubber.Writer(&buf)
	for i := 0; i < len(text); i++ {
		_, _ = w.Write([]byte{text[i]})
	}
	_ = w.Flush()
	if buf.String() != want {
		t.Fatalf("byte at a time: wrote %q", buf.String())
	}
}

func TestRoundTripperScrubsBodyAndQuery(t *testing.T) {
	c, secret := fixture(t)
	var gotBody, gotQuery string
	var gotLen int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		gotBody, gotQuery, gotLen = string(b), r.URL.RawQuery, r.ContentLength
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()
	client := &http.Client{Transport: c.Scrubber.RoundTripper(http.DefaultTransport)}

	body := `{"type":"tool_result","text":"fetch via ` + c.URL + ` failed: ` + secret + `"}`
	resp, err := client.Post(srv.URL+"/x?note="+secret, "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("status %d", resp.StatusCode)
	}
	if strings.Contains(gotBody, secret) || strings.Contains(gotQuery, secret) {
		t.Fatalf("core received the secret: body %q query %q", gotBody, gotQuery)
	}
	if q, _ := url.ParseQuery(gotQuery); q.Get("note") != Placeholder {
		t.Errorf("query %q", gotQuery)
	}
	if gotBody != c.Scrubber.String(body) || gotLen != int64(len(gotBody)) {
		t.Fatalf("body %q (length %d)", gotBody, gotLen)
	}

	// A GET with nothing to scrub goes through as it is.
	resp, err = client.Get(srv.URL + "/y?a=b")
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if gotQuery != "a=b" || gotBody != "" {
		t.Fatalf("GET arrived as body %q query %q", gotBody, gotQuery)
	}
}

func TestScrubberFromEnv(t *testing.T) {
	c, secret := fixture(t)
	env := map[string]string{"HTTPS_PROXY": c.URL}
	s := ScrubberFromEnv(func(k string) string { return env[k] })
	if s == nil {
		t.Fatal("no scrubber from a credentialed proxy URL")
	}
	if strings.Contains(s.String("x "+secret), secret) {
		t.Fatal("scrubber from env does not scrub the secret")
	}
	if s.String(c.URL) != Placeholder {
		t.Fatal("scrubber from env does not scrub the URL")
	}
	bare := map[string]string{"HTTPS_PROXY": "http://10.43.0.12:3128"}
	if ScrubberFromEnv(func(k string) string { return bare[k] }) != nil {
		t.Fatal("a scrubber from a bare proxy URL")
	}
}

func TestBareProxyURLDropsUserInfo(t *testing.T) {
	for in, want := range map[string]string{
		"":                                    "",
		"http://10.43.0.12:3128":              "http://10.43.0.12:3128",
		"http://alice:s3cret@10.43.0.12:3128": "http://10.43.0.12:3128",
		"http://alice@10.43.0.12:3128":        "http://10.43.0.12:3128",
		"alice:s3cret@10.43.0.12:3128":        "",
	} {
		if got := BareProxyURL(in); got != want {
			t.Errorf("BareProxyURL(%q) = %q, want %q", in, got, want)
		}
	}
}

// A runtime holding two credentials scrubs both, each in every form,
// with one scrubber; nils are skipped. Without Combine the shell's
// credential would reach core unscrubbed whenever a relay quoted it.
// This fails if either credential, its base64 form or its URL survives,
// or if nothing but nils yields a scrubber.
func TestCombineScrubsEveryCredential(t *testing.T) {
	open, openSecret := fixture(t)
	const shellSecret = "c2hlbGwtc2VjcmV0LWZvci1jb21iaW5lLXRlc3QtdmFsdWU"
	shell, err := newCredential("http://10.43.0.12:3128", "alice", shellSecret)
	if err != nil {
		t.Fatal(err)
	}
	s := Combine(nil, open.Scrubber, shell.Scrubber)
	basic := func(secret string) string { return base64.StdEncoding.EncodeToString([]byte("alice:" + secret)) }
	for _, in := range []string{
		openSecret, shellSecret, open.URL, shell.URL, basic(openSecret), basic(shellSecret),
	} {
		if out := s.String("x " + in + " y"); out != "x "+Placeholder+" y" {
			t.Errorf("String(%q) = %q", in, out)
		}
	}
	if Combine(nil, nil) != nil || Combine() != nil {
		t.Fatal("a scrubber from no credential")
	}
}
