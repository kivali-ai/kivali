package main

import (
	"bufio"
	"errors"
	"io"
	"net"
	"net/http"
	"testing"
)

// chanListener accepts the connections a test hands it.
type chanListener struct {
	ch     chan net.Conn
	closed chan struct{}
}

func newChanListener() *chanListener {
	return &chanListener{ch: make(chan net.Conn, 16), closed: make(chan struct{})}
}

func (l *chanListener) Accept() (net.Conn, error) {
	select {
	case c := <-l.ch:
		return c, nil
	case <-l.closed:
		return nil, errors.New("closed")
	}
}

func (l *chanListener) Close() error   { close(l.closed); return nil }
func (l *chanListener) Addr() net.Addr { return &net.TCPAddr{} }

// fromConn is one end of a pipe that reports a chosen remote address.
type fromConn struct {
	net.Conn
	remote net.Addr
}

func (c fromConn) RemoteAddr() net.Addr { return c.remote }

// dialFrom queues a connection from ip and returns the client's end.
func (l *chanListener) dialFrom(ip string) net.Conn {
	server, client := net.Pipe()
	l.ch <- fromConn{Conn: server, remote: &net.TCPAddr{IP: net.ParseIP(ip), Port: 40000}}
	return client
}

func TestSourceLimitListener(t *testing.T) {
	cl := newChanListener()
	logs := newLogSink()
	l := newSourceLimitListener(cl, 2, logs.logf)

	cl.dialFrom("10.42.0.7")
	cl.dialFrom("10.42.0.7")
	a1, err := l.Accept()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := l.Accept(); err != nil {
		t.Fatal(err)
	}
	if n := l.openFrom("10.42.0.7"); n != 2 {
		t.Fatalf("open = %d", n)
	}

	// A third from the same pod is refused; another pod is not.
	refused := cl.dialFrom("10.42.0.7")
	cl.dialFrom("10.42.0.8")
	b1, err := l.Accept()
	if err != nil {
		t.Fatal(err)
	}
	if got := sourceIP(b1.RemoteAddr()); got != "10.42.0.8" {
		t.Fatalf("accepted %s, want the other pod", got)
	}
	resp, err := http.ReadResponse(bufio.NewReader(refused), nil)
	if err != nil {
		t.Fatalf("refused connection: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusServiceUnavailable || string(body) != "too many connections from this address\n" {
		t.Fatalf("refusal = %d %q", resp.StatusCode, body)
	}
	if got := logs.matching("DENY source=10.42.0.7 reason=connections"); len(got) != 1 {
		t.Fatalf("log = %q", logs.all())
	}

	// Closing one frees its slot, once.
	_ = a1.Close()
	_ = a1.Close()
	if n := l.openFrom("10.42.0.7"); n != 1 {
		t.Fatalf("after close open = %d, want 1", n)
	}
	cl.dialFrom("10.42.0.7")
	if c, err := l.Accept(); err != nil || sourceIP(c.RemoteAddr()) != "10.42.0.7" {
		t.Fatalf("freed slot not reused: %v", err)
	}
	if n := l.openFrom("10.42.0.7"); n != 2 {
		t.Fatalf("open = %d", n)
	}
}

// Through the proxy: a tunnel holds its slot until it closes.
func TestSourceLimitThroughProxy(t *testing.T) {
	e := newEchoServer(t)
	h := newHarness(t, harnessConfig{allow: []string{"site.test"}, maxPerSource: 1})
	h.upstream("site.test", e.Addr())

	resp, conn, br := h.connect(t, "site.test:443", "")
	if resp.StatusCode != 200 {
		t.Fatal(resp.StatusCode)
	}
	echo(t, conn, br, []byte("hi"))
	resp2, _, _ := h.connect(t, "site.test:443", "")
	if resp2.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("second tunnel from the same source: %d", resp2.StatusCode)
	}
}
