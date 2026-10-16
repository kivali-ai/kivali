package main

import (
	"context"
	"fmt"
	"net"
	"net/netip"
	"syscall"
	"time"
)

// The open listener forwards to any host on the internet, and only
// there. A browser pod's Chromium, driven by a page or a hijacked web
// subagent, must not reach the server pod's own ports, cloud metadata,
// cluster services or anything else on a private network through it.
// The check runs on the address the dial actually uses, after DNS, so
// a name that resolves to a private address, or rebinds to one, is
// refused all the same.

// privateRanges are the address ranges the open listener refuses:
// loopback, link-local (cloud metadata lives at 169.254.169.254),
// RFC 1918 and ULA private space, carrier-grade NAT, and the
// unspecified and multicast ranges.
var privateRanges = func() []netip.Prefix {
	var out []netip.Prefix
	for _, s := range []string{
		"0.0.0.0/8", "10.0.0.0/8", "100.64.0.0/10", "127.0.0.0/8", "169.254.0.0/16",
		"172.16.0.0/12", "192.0.0.0/24", "192.168.0.0/16", "198.18.0.0/15", "224.0.0.0/4", "240.0.0.0/4",
		"::/128", "::1/128", "fc00::/7", "fe80::/10", "ff00::/8",
	} {
		out = append(out, netip.MustParsePrefix(s))
	}
	return out
}()

// publicAddress reports whether ip is one the open listener may dial.
func publicAddress(ip netip.Addr) bool {
	ip = ip.Unmap()
	if !ip.IsValid() {
		return false
	}
	for _, p := range privateRanges {
		if p.Contains(ip) {
			return false
		}
	}
	return true
}

// errPrivateAddress is what a refused dial reports.
var errPrivateAddress = fmt.Errorf("the address is not on the public internet")

// guardedDialer dials only public addresses. The check sits in the
// dialer's Control hook, which sees the resolved address of every
// connection attempt.
func guardedDialer(timeout time.Duration) *net.Dialer {
	return &net.Dialer{
		Timeout: timeout,
		Control: func(_, address string, _ syscall.RawConn) error {
			host, _, err := net.SplitHostPort(address)
			if err != nil {
				return err
			}
			ip, err := netip.ParseAddr(host)
			if err != nil {
				return err
			}
			if !publicAddress(ip) {
				return errPrivateAddress
			}
			return nil
		},
	}
}

// dialFunc is how a proxy reaches an upstream.
type dialFunc func(ctx context.Context, network, address string) (net.Conn, error)
