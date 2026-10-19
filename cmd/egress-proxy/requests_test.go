package main

import (
	"bufio"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"strings"
	"testing"
)

// raw sends req verbatim to the proxy and reads one response.
func (h *harness) raw(t *testing.T, req string) (*http.Response, string) {
	t.Helper()
	conn, err := net.Dial("tcp", h.srv.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	if _, err := io.WriteString(conn, req); err != nil {
		t.Fatal(err)
	}
	method := strings.SplitN(req, " ", 2)[0]
	resp, err := http.ReadResponse(bufio.NewReader(conn), &http.Request{Method: method})
	if err != nil {
		t.Fatalf("response: %v", err)
	}
	if resp.StatusCode == 200 && method == http.MethodConnect {
		return resp, ""
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	return resp, string(body)
}

// The target of a CONNECT is the authority on its request line. The
// Host header never chooses it, and a CONNECT without one is refused.
func TestConnectAuthorityFromRequestLine(t *testing.T) {
	e := newEchoServer(t)
	h := newHarness(t, harnessConfig{allow: []string{"listed.test"}})
	h.upstream("listed.test", e.Addr())
	h.upstream("unlisted.test", e.Addr())

	if r, _ := h.raw(t, "CONNECT /tunnel HTTP/1.1\r\nHost: listed.test:443\r\n\r\n"); r.StatusCode != 400 {
		t.Fatalf("CONNECT with the target only in Host: %d", r.StatusCode)
	}
	if r, _ := h.raw(t, "CONNECT unlisted.test:443 HTTP/1.1\r\nHost: listed.test:443\r\n\r\n"); r.StatusCode != 403 {
		t.Fatalf("Host header chose the target: %d", r.StatusCode)
	}
	if r, _ := h.raw(t, "CONNECT listed.test:443 HTTP/1.1\r\nHost: unlisted.test:443\r\n\r\n"); r.StatusCode != 200 {
		t.Fatalf("request line target refused: %d", r.StatusCode)
	}
	for _, name := range h.res.asked() {
		if name != "listed.test" {
			t.Fatalf("resolved %q", name)
		}
	}
}

// A header the client names in Connection stops at the proxy, the
// credentials included, and naming Proxy-Authorization there does not
// stop the proxy reading it.
func TestConnectionNamedHeadersStripped(t *testing.T) {
	up := newHeaderServer(t)
	h := newHarness(t, harnessConfig{
		allow:   []string{"listed.test"},
		secrets: map[string]string{"alice": "s3cret"},
	})
	h.upstream("anywhere.test", up.addr())
	h.upstream("listed.test", up.addr())

	r, _ := h.raw(t, "GET http://anywhere.test/x HTTP/1.1\r\nHost: anywhere.test\r\n"+
		"Proxy-Authorization: "+basicAuth("alice", "s3cret")+"\r\n"+
		"Connection: Proxy-Authorization\r\n\r\n")
	if r.StatusCode != 200 {
		t.Fatalf("open mode with Connection: Proxy-Authorization: %d", r.StatusCode)
	}
	if got := up.last(); got.Get("Proxy-Authorization") != "" || strings.Contains(got.Get("Connection"), "Proxy") {
		t.Fatalf("forwarded %v", got)
	}

	r, _ = h.raw(t, "GET http://listed.test/x HTTP/1.1\r\nHost: listed.test\r\n"+
		"Connection: keep-alive, X-Internal-Token\r\nX-Internal-Token: abc\r\nX-Kept: 1\r\n\r\n")
	if r.StatusCode != 200 {
		t.Fatalf("shell request: %d", r.StatusCode)
	}
	if got := up.last(); got.Get("X-Internal-Token") != "" || got.Get("X-Kept") != "1" {
		t.Fatalf("forwarded %v", got)
	}
}

// A failed upstream gets a fixed text: the addresses a name resolved
// to stay in the proxy's log.
func TestErrorBodiesNameNoAddresses(t *testing.T) {
	e := newEchoServer(t)
	h := newHarness(t, harnessConfig{secrets: map[string]string{"alice": "s3cret"}})
	agent := basicAuth("alice", "s3cret")
	h.res.set("down.test", publicIP) // resolves, but nothing is routed there
	h.res.set("garbled.test", "203.0.113.8")
	h.dialer.route("203.0.113.8", e.Addr()) // echoes the request back: not a response

	checks := []struct {
		req  string
		want string
	}{
		{"CONNECT down.test:443 HTTP/1.1\r\nHost: down.test:443\r\nProxy-Authorization: " + agent + "\r\n\r\n", "upstream dial failed\n"},
		{"CONNECT nowhere.test:443 HTTP/1.1\r\nHost: nowhere.test:443\r\nProxy-Authorization: " + agent + "\r\n\r\n", "upstream dial failed\n"},
		{"GET http://down.test/ HTTP/1.1\r\nHost: down.test\r\nProxy-Authorization: " + agent + "\r\n\r\n", "upstream dial failed\n"},
		{"GET http://garbled.test/ HTTP/1.1\r\nHost: garbled.test\r\nProxy-Authorization: " + agent + "\r\n\r\n", "upstream request failed\n"},
	}
	for _, c := range checks {
		r, body := h.raw(t, c.req)
		if r.StatusCode != http.StatusBadGateway || body != c.want {
			t.Errorf("%q: %d %q, want 502 %q", strings.SplitN(c.req, "\r\n", 2)[0], r.StatusCode, body, c.want)
		}
	}
	if got := h.logs.matching("FAIL agent=alice mode=open host=down.test:"); len(got) != 2 ||
		!strings.Contains(got[0], publicIP) {
		t.Fatalf("dial detail not logged: %q", h.logs.all())
	}
	if got := h.logs.matching("FAIL agent=alice mode=open host=garbled.test:80 reason=request err="); len(got) != 1 {
		t.Fatalf("request detail not logged: %q", h.logs.all())
	}
}

// Over plain HTTP, a request that passes the per-connection cap is
// cut at the cap and answered with the budget text.
func TestHTTPPerConnectionCap(t *testing.T) {
	for _, enforce := range []bool{false, true} {
		t.Run(fmt.Sprintf("enforce=%v", enforce), func(t *testing.T) {
			up := newHeaderServer(t)
			h := newHarness(t, harnessConfig{
				secrets: map[string]string{"alice": "s3cret"},
				limits:  budgetLimits{Conn: 100, TenMin: 1 << 30, Day: 1 << 30},
				enforce: enforce,
			})
			h.upstream("site.test", up.addr())
			body := strings.Repeat("x", 500)
			r, got := h.raw(t, "POST http://site.test/upload HTTP/1.1\r\nHost: site.test\r\n"+
				"Proxy-Authorization: "+basicAuth("alice", "s3cret")+"\r\n"+
				fmt.Sprintf("Content-Length: %d\r\n\r\n", len(body))+body)
			over := h.logs.next(t, "OVER")
			if !strings.HasPrefix(over, "OVER agent=alice limit=conn ") || !strings.HasSuffix(over, fmt.Sprintf("enforced=%t", enforce)) {
				t.Fatalf("OVER = %q", over)
			}
			if !enforce {
				if r.StatusCode != 200 {
					t.Fatalf("unenforced: %d %q", r.StatusCode, got)
				}
				return
			}
			if r.StatusCode != 403 || got != budgetRefusal+"\n" {
				t.Fatalf("enforced: %d %q", r.StatusCode, got)
			}
			if line := h.logs.next(t, "CONN"); line != "CONN agent=alice mode=open host=site.test:80 up=100 down=0" {
				t.Fatalf("close line = %q", line)
			}
		})
	}
}

// The chart passes the pod CIDR and the node's own IP from the
// downward API; a bare IP is one address and an empty value is
// ignored.
func TestDenyCIDRFlagFromChart(t *testing.T) {
	var deny cidrFlag
	fs := flag.NewFlagSet("egress-proxy", flag.ContinueOnError)
	fs.Var(&deny, "deny-cidr", "")
	if err := fs.Parse([]string{"--deny-cidr=10.42.0.0/16", "--deny-cidr=", "--deny-cidr=192.0.2.10", "--deny-cidr=2001:db8::5"}); err != nil {
		t.Fatal(err)
	}
	if got, want := deny.String(), "10.42.0.0/16,192.0.2.10/32,2001:db8::5/128"; got != want {
		t.Fatalf("deny = %q, want %q", got, want)
	}
	g := &guard{deny: deny}
	for addr, refused := range map[string]bool{
		"192.0.2.10":           true,
		"::ffff:192.0.2.10":    true,
		"192.0.2.11":           false,
		"2001:db8::5":          true,
		"2001:db8::6":          false,
		"10.42.200.1":          true,
		"2001:4860:4860::8888": false,
	} {
		if got := g.refusal(netip.MustParseAddr(addr), false) != ""; got != refused {
			t.Errorf("allowlist mode %s refused=%v, want %v", addr, got, refused)
		}
	}
}
