package main

import (
	"context"
	"fmt"
	"net"
	"net/http/httptest"
	"net/netip"
	"strings"
	"testing"
	"time"
)

// TestOpenListenerForwardsAnyPublicHostButNothingPrivate: the open
// listener is not held to the allowlist, and it never dials a private
// address, so a browser pod cannot reach the server pod, the cluster or
// cloud metadata through it. The echo server here is on loopback, so
// the open listener must refuse it while the allowlisted one, told the
// host, tunnels to it.
func TestOpenListenerForwardsAnyPublicHostButNothingPrivate(t *testing.T) {
	echo := newEchoServer(t)
	defer echo.Close()
	host, _, _ := net.SplitHostPort(echo.host)
	var a Allowlist
	a.patterns.Store(&[]string{host})
	allowlisted := httptest.NewServer(&proxy{allowed: a.Allowed, label: "allowlist"})
	defer allowlisted.Close()
	open := httptest.NewServer(&proxy{allowed: func(string) bool { return true }, label: "open", dial: guardedDialer(time.Second).DialContext})
	defer open.Close()

	connect := func(proxyAddr string) string {
		conn, err := net.Dial("tcp", proxyAddr)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = conn.Close() }()
		_, _ = fmt.Fprintf(conn, "CONNECT %s HTTP/1.1\r\nHost: %s\r\n\r\n", echo.Addr(), echo.Addr())
		buf := make([]byte, 512)
		_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
		n, err := conn.Read(buf)
		if err != nil {
			t.Fatalf("read: %v", err)
		}
		return string(buf[:n])
	}
	if got := connect(allowlisted.Listener.Addr().String()); !strings.Contains(got, "200") {
		t.Errorf("allowlisted listener refused its own host: %q", got)
	}
	if got := connect(open.Listener.Addr().String()); !strings.Contains(got, "502") || !strings.Contains(got, "not on the public internet") {
		t.Errorf("open listener dialed loopback: %q", got)
	}
	// The open listener does not consult the allowlist: an unlisted
	// public name reaches the dialer and fails only there.
	if _, err := guardedDialer(time.Second).DialContext(context.Background(), "tcp", "127.0.0.1:1"); err == nil || !strings.Contains(err.Error(), "not on the public internet") {
		t.Errorf("guarded dial of loopback: %v", err)
	}
}

func TestPublicAddress(t *testing.T) {
	private := []string{"127.0.0.1", "10.1.2.3", "172.16.0.1", "172.31.255.255", "192.168.1.1", "169.254.169.254", "100.64.0.1", "0.0.0.0", "224.0.0.1", "::1", "fe80::1", "fd00::1", "::ffff:127.0.0.1", "::ffff:10.0.0.1"}
	for _, s := range private {
		if publicAddress(netip.MustParseAddr(s)) {
			t.Errorf("%s counted as public", s)
		}
	}
	public := []string{"8.8.8.8", "140.82.112.3", "172.32.0.1", "2606:4700::1111", "::ffff:8.8.8.8"}
	for _, s := range public {
		if !publicAddress(netip.MustParseAddr(s)) {
			t.Errorf("%s counted as private", s)
		}
	}
}
