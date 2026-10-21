package kube

import (
	"context"
	"fmt"
	"net"
	"net/netip"
	"net/url"
	"slices"
	"strings"
)

// ProxyByIP is raw, the configured egress proxy URL, as a provisioned
// pod gets it: scheme, the host resolved to an IP (LowestAddr), and the
// port. A pod has no DNS, so the name is resolved once here and the
// address goes in its spec. A host that is already an IP is kept.
// Empty raw is no proxy. resolve nil means net.DefaultResolver.
func ProxyByIP(ctx context.Context, raw string, resolve func(context.Context, string) ([]string, error)) (string, error) {
	if strings.TrimSpace(raw) == "" {
		return "", nil
	}
	u, err := url.Parse(raw)
	if err != nil || u.Scheme == "" || u.Hostname() == "" {
		return "", fmt.Errorf("egress proxy URL %q: want scheme://host:port", raw)
	}
	if u.User != nil {
		return "", fmt.Errorf("egress proxy URL carries credentials; pods get the proxy without them")
	}
	host, port := u.Hostname(), u.Port()
	ip := net.ParseIP(host)
	if ip == nil {
		if resolve == nil {
			resolve = net.DefaultResolver.LookupHost
		}
		addrs, err := resolve(ctx, host)
		if err != nil {
			return "", fmt.Errorf("egress proxy: resolve %s: %w", host, err)
		}
		ip = LowestAddr(addrs)
		if ip == nil {
			return "", fmt.Errorf("egress proxy: %s resolves to no address", host)
		}
	}
	addr := ip.String()
	if port != "" {
		addr = net.JoinHostPort(addr, port)
	} else if ip.To4() == nil {
		addr = "[" + addr + "]"
	}
	return (&url.URL{Scheme: u.Scheme, Host: addr}).String(), nil
}

// LowestAddr is the lowest IPv4 address in addrs, else the lowest IPv6
// one, else nil. Lowest rather than first: a name with several records
// resolves in any order, and the pick lands in a pod spec, whose hash
// must not change from one lookup to the next.
func LowestAddr(addrs []string) net.IP {
	var v4, v6 []netip.Addr
	for _, a := range addrs {
		ip, err := netip.ParseAddr(a)
		if err != nil {
			continue
		}
		if ip = ip.Unmap(); ip.Is4() {
			v4 = append(v4, ip)
		} else {
			v6 = append(v6, ip.WithZone(""))
		}
	}
	for _, set := range [][]netip.Addr{v4, v6} {
		if len(set) > 0 {
			slices.SortFunc(set, netip.Addr.Compare)
			return net.IP(set[0].AsSlice())
		}
	}
	return nil
}
