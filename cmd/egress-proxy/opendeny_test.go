package main

import (
	"flag"
	"net/netip"
	"testing"
)

// TestOpenDenyCIDRBindsOpenModeOnly: --open-deny-cidr, which the chart
// fills with the ranges browser pods may not reach, refuses those
// addresses for the agent's own fetches and leaves the shell's listed
// hosts to the allowlist's rules; Azure's WireServer, metadata at a
// public address, is refused in both modes.
func TestOpenDenyCIDRBindsOpenModeOnly(t *testing.T) {
	var openDeny cidrFlag
	fs := flag.NewFlagSet("egress-proxy", flag.ContinueOnError)
	fs.Var(&openDeny, "open-deny-cidr", "")
	if err := fs.Parse([]string{"--open-deny-cidr=10.0.0.0/8", "--open-deny-cidr=203.0.113.0/24", "--open-deny-cidr="}); err != nil {
		t.Fatal(err)
	}
	g := &guard{openDeny: openDeny}
	for _, tc := range []struct {
		addr                string
		allowlist, openMode bool
	}{
		{"203.0.113.7", false, true},
		{"::ffff:203.0.113.7", false, true},
		{"10.1.2.3", false, true},
		{"168.63.129.16", true, true},
		{"198.51.100.7", false, false},
	} {
		if got := g.refusal(netip.MustParseAddr(tc.addr), false) != ""; got != tc.allowlist {
			t.Errorf("allowlist mode %s refused=%v, want %v", tc.addr, got, tc.allowlist)
		}
		if got := g.refusal(netip.MustParseAddr(tc.addr), true) != ""; got != tc.openMode {
			t.Errorf("open mode %s refused=%v, want %v", tc.addr, got, tc.openMode)
		}
	}
}
