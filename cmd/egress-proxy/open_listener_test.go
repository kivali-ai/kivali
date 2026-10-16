package main

import (
	"fmt"
	"net"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// TestOpenListenerForwardsAnyHost: the open listener, which browser
// pods use, tunnels to a host the allowlisted one refuses.
func TestOpenListenerForwardsAnyHost(t *testing.T) {
	echo := newEchoServer(t)
	defer echo.Close()
	var a Allowlist
	a.patterns.Store(&[]string{"allowed.example.com"})
	allowlisted := httptest.NewServer(&proxy{allowed: a.Allowed, label: "allowlist"})
	defer allowlisted.Close()
	open := httptest.NewServer(&proxy{allowed: func(string) bool { return true }, label: "open"})
	defer open.Close()

	connect := func(proxyAddr string) string {
		conn, err := net.Dial("tcp", proxyAddr)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = conn.Close() }()
		_, _ = fmt.Fprintf(conn, "CONNECT %s HTTP/1.1\r\nHost: %s\r\n\r\n", echo.Addr(), echo.Addr())
		buf := make([]byte, 256)
		_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
		n, err := conn.Read(buf)
		if err != nil {
			t.Fatalf("read: %v", err)
		}
		return string(buf[:n])
	}
	if got := connect(allowlisted.Listener.Addr().String()); !strings.Contains(got, "403") {
		t.Errorf("allowlisted listener admitted an unlisted host: %q", got)
	}
	if got := connect(open.Listener.Addr().String()); !strings.Contains(got, "200") {
		t.Errorf("open listener refused: %q", got)
	}
}
