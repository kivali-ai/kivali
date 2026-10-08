package main

import "testing"

// isLoopbackAddr is what stops DEV_MODE — which disables authentication
// entirely — from being reachable off the machine it runs on. It must
// fail closed: anything it can't positively prove is loopback has to
// come back false, because the consequence of a false positive is an
// unauthenticated server on a routable interface.
func TestIsLoopbackAddr(t *testing.T) {
	cases := []struct {
		addr string
		want bool
		why  string
	}{
		{"127.0.0.1:8080", true, "canonical IPv4 loopback"},
		{"127.0.0.1:0", true, "port 0 is still loopback"},
		{"127.1.2.3:8080", true, "all of 127/8 is loopback"},
		{"localhost:8080", true, "the one hostname we special-case"},
		{"[::1]:8080", true, "IPv6 loopback"},

		// Every one of these must be refused.
		{":8080", false, "bare port binds every interface"},
		{"", false, "empty binds every interface"},
		{"0.0.0.0:8080", false, "explicit all-interfaces"},
		{"[::]:8080", false, "IPv6 all-interfaces"},
		{"192.168.1.10:8080", false, "LAN address"},
		{"10.0.0.5:8080", false, "private range is not loopback"},
		{"8.8.8.8:8080", false, "public address"},
		{"example.com:8080", false, "hostnames other than localhost are not resolved"},
		{"127.0.0.1", false, "no port — not a valid listen address"},
		{"garbage", false, "unparseable"},
	}
	for _, c := range cases {
		if got := isLoopbackAddr(c.addr); got != c.want {
			t.Errorf("isLoopbackAddr(%q) = %v, want %v (%s)", c.addr, got, c.want, c.why)
		}
	}
}
