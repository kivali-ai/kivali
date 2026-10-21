package main

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"strconv"
	"strings"
)

// Resolver looks a name up to its addresses. *net.Resolver satisfies
// it; tests substitute a table so a name can resolve to any address.
type Resolver interface {
	LookupNetIP(ctx context.Context, network, host string) ([]netip.Addr, error)
}

// dialFunc opens a TCP connection to an "ip:port" address the guard
// has already checked. It is never handed a name.
type dialFunc func(ctx context.Context, network, addr string) (net.Conn, error)

// errDestination is the guard's refusal: the host resolved only to
// addresses the request's mode may not reach.
var errDestination = errors.New("destination address not allowed")

// guard resolves a host once, checks every candidate address against
// the request's mode, and dials only an address that passed. It is the
// one dialer for CONNECT tunnels and plain HTTP alike.
type guard struct {
	resolver Resolver
	dial     dialFunc
	// deny holds the cluster's own ranges (pod and service CIDRs, the
	// node's address), refused for every request (--deny-cidr).
	deny []netip.Prefix
	// openDeny holds ranges refused under open mode's address rules
	// only, on top of openRefused (--open-deny-cidr): the chart passes
	// the ranges browser pods may not reach, so the agent's own fetches
	// are held to the same.
	openDeny []netip.Prefix
}

// alwaysRefused are ranges no request reaches, in any mode. The
// proxy runs inside the server pod, so loopback there is the Kivali
// server itself; link-local holds cloud metadata (169.254.169.254), and
// Azure's WireServer is metadata at a public address.
var alwaysRefused = mustPrefixes(
	"0.0.0.0/8",        // "this network", including the unspecified address
	"127.0.0.0/8",      // loopback
	"169.254.0.0/16",   // link-local, cloud metadata
	"168.63.129.16/32", // Azure WireServer, cloud metadata
	"::/128",           // unspecified
	"::1/128",          // loopback
	"fe80::/10",        // link-local
)

// openRefused are the ranges refused on top of alwaysRefused under open
// mode's address rules: for a request that may reach any public host
// (open and exempt mode) to a host on no list. A listed host gets these
// through, so operators can list intranet hosts.
var openRefused = mustPrefixes(
	"10.0.0.0/8",     // private
	"172.16.0.0/12",  // private
	"192.168.0.0/16", // private
	"100.64.0.0/10",  // carrier-grade NAT
	"224.0.0.0/4",    // multicast
	"240.0.0.0/4",    // reserved, including broadcast 255.255.255.255
	"fc00::/7",       // unique local
	"fec0::/10",      // site-local
	"ff00::/8",       // multicast
	"::/96",          // IPv4-compatible: embeds an IPv4 address
	"64:ff9b::/96",   // NAT64: embeds an IPv4 address
	"64:ff9b:1::/48", // local-use NAT64
	"2002::/16",      // 6to4: embeds an IPv4 address
)

func mustPrefixes(ss ...string) []netip.Prefix {
	out := make([]netip.Prefix, 0, len(ss))
	for _, s := range ss {
		out = append(out, netip.MustParsePrefix(s))
	}
	return out
}

// refusal reports why addr may not be dialled in the given mode, or ""
// when it may. IPv4-mapped IPv6 addresses are judged as the IPv4
// address they carry.
func (g *guard) refusal(addr netip.Addr, open bool) string {
	if !addr.IsValid() {
		return "invalid address"
	}
	if addr.Zone() != "" {
		return "scoped address"
	}
	addr = addr.Unmap()
	for _, p := range alwaysRefused {
		if p.Contains(addr) {
			return "local address " + p.String()
		}
	}
	for _, p := range g.deny {
		if p.Contains(addr) {
			return "cluster address " + p.String()
		}
	}
	if open {
		for _, p := range openRefused {
			if p.Contains(addr) {
				return "non-public address " + p.String()
			}
		}
		for _, p := range g.openDeny {
			if p.Contains(addr) {
				return "refused range " + p.String()
			}
		}
	}
	return ""
}

// Dial connects to hostport. An IP-literal host is checked as given; a
// name is resolved once and each candidate checked. Only addresses
// that pass are dialled, by address, in the order the resolver gave.
func (g *guard) Dial(ctx context.Context, hostport string, open bool) (net.Conn, error) {
	host, portStr, err := net.SplitHostPort(hostport)
	if err != nil {
		return nil, fmt.Errorf("bad address %q: %w", hostport, err)
	}
	port, err := strconv.ParseUint(portStr, 10, 16)
	if err != nil {
		return nil, fmt.Errorf("bad port %q", portStr)
	}

	var candidates []netip.Addr
	if ip, perr := netip.ParseAddr(host); perr == nil {
		candidates = []netip.Addr{ip}
	} else {
		candidates, err = g.resolver.LookupNetIP(ctx, "ip", strings.ToLower(strings.TrimSuffix(host, ".")))
		if err != nil {
			return nil, fmt.Errorf("lookup %s: %w", host, err)
		}
	}

	var allowed []netip.Addr
	for _, c := range candidates {
		if g.refusal(c, open) == "" {
			allowed = append(allowed, c.Unmap())
		}
	}
	if len(allowed) == 0 {
		return nil, errDestination
	}
	var lastErr error
	for _, a := range allowed {
		conn, err := g.dial(ctx, "tcp", netip.AddrPortFrom(a, uint16(port)).String())
		if err == nil {
			return conn, nil
		}
		lastErr = err
	}
	return nil, lastErr
}

// cidrFlag is the repeatable, comma-separated --deny-cidr flag. A bare
// address is taken as a single-address prefix; empty items are skipped
// so an unset chart value passes through harmlessly.
type cidrFlag []netip.Prefix

func (c *cidrFlag) String() string {
	parts := make([]string, len(*c))
	for i, p := range *c {
		parts[i] = p.String()
	}
	return strings.Join(parts, ",")
}

func (c *cidrFlag) Set(v string) error {
	for _, s := range strings.Split(v, ",") {
		s = strings.TrimSpace(s)
		if s == "" {
			continue
		}
		if !strings.Contains(s, "/") {
			a, err := netip.ParseAddr(s)
			if err != nil {
				return fmt.Errorf("deny-cidr %q: %w", s, err)
			}
			*c = append(*c, netip.PrefixFrom(a.Unmap(), a.Unmap().BitLen()))
			continue
		}
		p, err := netip.ParsePrefix(s)
		if err != nil {
			return fmt.Errorf("deny-cidr %q: %w", s, err)
		}
		// The guard judges IPv4-mapped addresses as IPv4, so a mapped
		// prefix is kept as the IPv4 prefix it covers.
		if p.Addr().Is4In6() {
			if p.Bits() < 96 {
				return fmt.Errorf("deny-cidr %q: an IPv4-mapped prefix needs at least /96", s)
			}
			p = netip.PrefixFrom(p.Addr().Unmap(), p.Bits()-96)
		}
		*c = append(*c, p.Masked())
	}
	return nil
}
