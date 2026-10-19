package main

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"sync"
	"testing"
)

// fakeResolver answers from a table and records every name asked.
type fakeResolver struct {
	mu    sync.Mutex
	names map[string][]netip.Addr
	calls []string
}

func (f *fakeResolver) LookupNetIP(_ context.Context, _, host string) ([]netip.Addr, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, host)
	addrs, ok := f.names[host]
	if !ok {
		return nil, &net.DNSError{Err: "no such host", Name: host, IsNotFound: true}
	}
	return addrs, nil
}

func (f *fakeResolver) set(host string, addrs ...string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.names == nil {
		f.names = map[string][]netip.Addr{}
	}
	var out []netip.Addr
	for _, a := range addrs {
		out = append(out, netip.MustParseAddr(a))
	}
	f.names[host] = out
}

func (f *fakeResolver) asked() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.calls...)
}

// testDialer stands in for the network: it records every address the
// guard dials and connects the ones routed to a local listener.
type testDialer struct {
	mu     sync.Mutex
	routes map[netip.Addr]string
	dialed []string
}

func (d *testDialer) route(ip, to string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.routes == nil {
		d.routes = map[netip.Addr]string{}
	}
	d.routes[netip.MustParseAddr(ip)] = to
}

func (d *testDialer) dial(ctx context.Context, network, addr string) (net.Conn, error) {
	d.mu.Lock()
	d.dialed = append(d.dialed, addr)
	ap, err := netip.ParseAddrPort(addr)
	to, ok := d.routes[ap.Addr()]
	d.mu.Unlock()
	if err != nil {
		return nil, fmt.Errorf("dialer handed a name: %q", addr)
	}
	if !ok {
		return nil, errors.New("no route to " + addr)
	}
	var nd net.Dialer
	return nd.DialContext(ctx, network, to)
}

func (d *testDialer) calls() []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]string(nil), d.dialed...)
}

func TestGuardRefusalTable(t *testing.T) {
	g := &guard{deny: mustPrefixes("10.42.0.0/16", "10.43.0.0/16", "fd00:42::/48")}
	type want struct{ open, allowlist bool } // true = refused
	cases := map[string]want{
		// Always refused.
		"127.0.0.1":                {true, true},
		"127.1.2.3":                {true, true},
		"::1":                      {true, true},
		"0.0.0.0":                  {true, true},
		"0.1.2.3":                  {true, true},
		"::":                       {true, true},
		"169.254.169.254":          {true, true},
		"169.254.0.1":              {true, true},
		"fe80::1":                  {true, true},
		"::ffff:127.0.0.1":         {true, true},
		"::ffff:169.254.169.254":   {true, true},
		"::ffff:0.0.0.0":           {true, true},
		"10.42.7.9":                {true, true}, // --deny-cidr: pod range
		"10.43.0.10":               {true, true}, // --deny-cidr: service range
		"::ffff:10.42.0.1":         {true, true},
		"fd00:42::5":               {true, true},
		"fe80::1%eth0":             {true, true},
		"10.0.0.5":                 {true, false},
		"10.255.255.255":           {true, false},
		"172.16.0.1":               {true, false},
		"172.31.255.255":           {true, false},
		"192.168.1.1":              {true, false},
		"::ffff:192.168.1.1":       {true, false},
		"100.64.0.1":               {true, false},
		"100.127.255.254":          {true, false},
		"224.0.0.1":                {true, false},
		"239.255.255.250":          {true, false},
		"255.255.255.255":          {true, false},
		"240.0.0.1":                {true, false},
		"fc00::1":                  {true, false},
		"fd12:3456::1":             {true, false},
		"ff02::1":                  {true, false},
		"64:ff9b::a00:1":           {true, false},
		"2002:a00:1::1":            {true, false},
		"::a00:1":                  {true, false},
		"8.8.8.8":                  {false, false},
		"203.0.113.7":              {false, false},
		"172.32.0.1":               {false, false},
		"100.128.0.1":              {false, false},
		"::ffff:8.8.8.8":           {false, false},
		"2606:4700:4700::1111":     {false, false},
		"2001:4860:4860::8888":     {false, false},
		"::ffff:203.0.113.7":       {false, false},
		"2a00:1450:4001:80b::200e": {false, false},
	}
	for s, w := range cases {
		a := netip.MustParseAddr(s)
		if got := g.refusal(a, true) != ""; got != w.open {
			t.Errorf("open %s refused=%v, want %v", s, got, w.open)
		}
		if got := g.refusal(a, false) != ""; got != w.allowlist {
			t.Errorf("allowlist %s refused=%v, want %v", s, got, w.allowlist)
		}
	}
}

func TestGuardDialRefusesBeforeDialling(t *testing.T) {
	res := &fakeResolver{}
	res.set("loopback.test", "127.0.0.1")
	res.set("mapped.test", "::ffff:127.0.0.1")
	res.set("metadata.test", "169.254.169.254")
	res.set("intranet.test", "10.0.0.5")
	d := &testDialer{}
	g := &guard{resolver: res, dial: d.dial}

	for _, tc := range []struct {
		host string
		open bool
	}{
		{"loopback.test:443", true},
		{"loopback.test:443", false},
		{"mapped.test:443", false},
		{"metadata.test:80", false},
		{"intranet.test:443", true},
		{"127.0.0.1:8080", false},
		{"[::1]:8080", true},
		{"[::ffff:169.254.169.254]:80", false},
		{"169.254.169.254:80", true},
	} {
		if _, err := g.Dial(context.Background(), tc.host, tc.open); !errors.Is(err, errDestination) {
			t.Errorf("Dial(%s, open=%v) err=%v, want errDestination", tc.host, tc.open, err)
		}
	}
	if calls := d.calls(); len(calls) != 0 {
		t.Fatalf("dialled refused addresses: %v", calls)
	}
}

// A name resolving to a mix of addresses is dialled only at the ones
// its mode allows, by address, never by name.
func TestGuardDialMixedAddresses(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ln.Close() }()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			_ = c.Close()
		}
	}()

	res := &fakeResolver{}
	res.set("mixed.test", "::ffff:127.0.0.1", "10.0.0.5", "203.0.113.9")
	d := &testDialer{}
	d.route("203.0.113.9", ln.Addr().String())
	d.route("10.0.0.5", ln.Addr().String())
	g := &guard{resolver: res, dial: d.dial}

	c, err := g.Dial(context.Background(), "mixed.test:443", true)
	if err != nil {
		t.Fatalf("open dial: %v", err)
	}
	_ = c.Close()
	if got := d.calls(); len(got) != 1 || got[0] != "203.0.113.9:443" {
		t.Fatalf("open mode dialled %v, want only the public address", got)
	}

	d.dialed = nil
	c, err = g.Dial(context.Background(), "mixed.test:443", false)
	if err != nil {
		t.Fatalf("allowlist dial: %v", err)
	}
	_ = c.Close()
	if got := d.calls(); len(got) != 1 || got[0] != "10.0.0.5:443" {
		t.Fatalf("allowlist mode dialled %v, want the first allowed address", got)
	}
	if asked := res.asked(); len(asked) != 2 {
		t.Fatalf("resolved %d times, want once per dial: %v", len(asked), asked)
	}
}

func TestCIDRFlag(t *testing.T) {
	var c cidrFlag
	for _, v := range []string{"10.42.0.0/16, 10.43.0.0/16", "", "fd00:42::/48", "192.0.2.1", "10.44.1.2/16"} {
		if err := c.Set(v); err != nil {
			t.Fatalf("Set(%q): %v", v, err)
		}
	}
	want := "10.42.0.0/16,10.43.0.0/16,fd00:42::/48,192.0.2.1/32,10.44.0.0/16"
	if got := c.String(); got != want {
		t.Fatalf("String() = %q, want %q", got, want)
	}
	for _, bad := range []string{"10.0.0.0/33", "not-a-cidr", "10.0.0/8", "::ffff:0:0/80"} {
		var c cidrFlag
		if err := c.Set(bad); err == nil {
			t.Errorf("Set(%q) accepted", bad)
		}
	}
}
