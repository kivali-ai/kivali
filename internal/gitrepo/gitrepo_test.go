package gitrepo

import (
	"errors"
	"net"
	"testing"
)

// Why this test exists: Test connection runs in Kivali's server, which
// reaches its own cluster and machine directly. A connection host that
// resolves to any loopback, private, link-local, unspecified or
// multicast address, even one among public ones, would let Test
// connection probe them with the owner's token; only hosts whose every
// address is public may be reached.
func TestPublicAddresses(t *testing.T) {
	ips := func(ss ...string) []net.IP {
		var out []net.IP
		for _, s := range ss {
			out = append(out, net.ParseIP(s))
		}
		return out
	}
	for _, c := range []struct {
		ips  []net.IP
		want bool
	}{
		{ips("140.82.112.3"), true},
		{ips("2606:50c0:8000::153"), true},
		{ips("140.82.112.3", "2606:50c0:8000::153"), true},
		{nil, false},
		{ips("127.0.0.1"), false},
		{ips("::1"), false},
		{ips("10.1.2.3"), false},
		{ips("172.16.0.1"), false},
		{ips("192.168.1.1"), false},
		{ips("fd00::1"), false},
		{ips("169.254.169.254"), false},
		{ips("fe80::1"), false},
		{ips("0.0.0.0"), false},
		{ips("::"), false},
		{ips("224.0.0.1"), false},
		{ips("ff02::1"), false},
		{ips("::ffff:10.0.0.1"), false},
		{ips("140.82.112.3", "10.0.0.1"), false},
	} {
		if got := PublicAddresses(c.ips); got != c.want {
			t.Errorf("PublicAddresses(%v) = %v, want %v", c.ips, got, c.want)
		}
	}
}

// Why this test exists: the host is what the token is sent to and what
// the egress proxy admits. A host that kept a scheme, a path or user
// info would register a pattern the proxy cannot match, or name
// another host than the owner meant; each must be refused, and the
// forms an owner pastes (https:// in front, a slash behind, capitals)
// made exact.
func TestNormalizeHost(t *testing.T) {
	for raw, want := range map[string]string{
		"github.com":           "github.com",
		" GitHub.com/ ":        "github.com",
		"https://gitlab.com":   "gitlab.com",
		"HTTPS://git.acme.io/": "git.acme.io",
		"git.acme.io:8443":     "git.acme.io:8443",
		"127.0.0.1:5000":       "127.0.0.1:5000",
	} {
		got, err := NormalizeHost(raw)
		if err != nil || got != want {
			t.Errorf("NormalizeHost(%q) = %q, %v; want %q", raw, got, err, want)
		}
	}
	for _, bad := range []string{"", "http://github.com", "github.com/acme", "user@github.com", "git hub.com", "*.github.com",
		"github.com:0", "github.com:99999", "github.com:08", ".github.com", "github..com", "-a.com"} {
		if got, err := NormalizeHost(bad); err == nil {
			t.Errorf("NormalizeHost(%q) = %q, want an error", bad, got)
		}
	}
	if HostName("git.acme.io:8443") != "git.acme.io" || HostName("github.com") != "github.com" {
		t.Error("HostName keeps the port")
	}
}

// Why this test exists: the URL core builds, with the owner's token,
// is made from what the model wrote. A URL with user info, a query, a
// fragment, another scheme or another host, or a path that climbs out
// with "..", would send the token somewhere the access never named; a
// shorthand or URL that names a repository in any of the accepted
// forms must give the same owner/name.
func TestParse(t *testing.T) {
	const host = "github.com"
	for ref, want := range map[string]string{
		"acme/widgets":                           "acme/widgets",
		"/acme/widgets.git/":                     "acme/widgets",
		"https://github.com/acme/widgets":        "acme/widgets",
		"https://github.com/acme/widgets.git":    "acme/widgets",
		"https://GitHub.com/Acme/Widgets.git":    "Acme/Widgets",
		"https://github.com/group/sub/project":   "group/sub/project",
		"https://github.com/acme/my.repo_v2-x/":  "acme/my.repo_v2-x",
		"  https://github.com/acme/widgets.git ": "acme/widgets",
	} {
		r, err := Parse(ref, host, false)
		if err != nil || r.Path != want || r.Host != host {
			t.Errorf("Parse(%q) = %+v, %v; want %q", ref, r, err, want)
		}
	}
	for _, bad := range []string{
		"", "widgets", "acme/", "acme/../../etc", "acme/./x", "acme/wid gets", "acme/w%2fx",
		"git@github.com:acme/widgets.git", "ssh://github.com/acme/widgets", "http://github.com/acme/widgets",
		"https://x-access-token:t@github.com/acme/widgets", "https://github.com/acme/widgets?x=1",
		"https://github.com/acme/widgets#frag", "file:///tmp/acme/widgets", "https://github.com/acme",
	} {
		if r, err := Parse(bad, host, false); err == nil {
			t.Errorf("Parse(%q) = %+v, want an error", bad, r)
		}
	}
	var other *OtherHostError
	if _, err := Parse("https://gitlab.com/acme/widgets", host, false); !errors.As(err, &other) || other.Host != host {
		t.Errorf("another host = %v", err)
	}
	if _, err := Parse("https://github.com:444/acme/widgets", host, false); !errors.As(err, &other) {
		t.Errorf("another port = %v", err)
	}
	// http only where a test allows it.
	if r, err := Parse("http://127.0.0.1:5000/acme/widgets.git", "127.0.0.1:5000", true); err != nil || r.Path != "acme/widgets" {
		t.Errorf("insecure = %+v, %v", r, err)
	}
	if _, err := ParseURL("acme/widgets", host, false); err == nil {
		t.Error("ParseURL took a shorthand")
	}
	r, _ := Parse("Acme/Widgets", host, false)
	if r.URL() != "https://github.com/Acme/Widgets.git" || r.Key() != "acme/widgets" {
		t.Errorf("URL %q key %q", r.URL(), r.Key())
	}
	if r, _ := Parse("acme/widgets", "127.0.0.1:5000", true); r.URL() != "http://127.0.0.1:5000/acme/widgets.git" {
		t.Errorf("insecure shorthand URL %q", r.URL())
	}
}

// Why this test exists: the access decides which repositories an
// agent's clone and fetch reach with the owner's token. A * that
// crossed a / would hand acme/* every nested group's repository, a
// case difference would refuse what the owner meant, and a pattern
// that does not parse must grant nothing.
func TestMatch(t *testing.T) {
	for _, c := range []struct {
		patterns []string
		key      string
		want     bool
	}{
		{[]string{"acme/widgets"}, "acme/widgets", true},
		{[]string{"Acme/Widgets.git"}, "acme/widgets", true},
		{[]string{"acme/widgets"}, "ACME/WIDGETS", true},
		{[]string{"acme/widgets"}, "acme/widgets2", false},
		{[]string{"acme/*"}, "acme/widgets", true},
		{[]string{"acme/*"}, "acme/sub/widgets", false},
		{[]string{"acme/*/*"}, "acme/sub/widgets", true},
		{[]string{"*/widgets"}, "acme/widgets", true},
		{[]string{"acme/w?dgets"}, "acme/widgets", true},
		{[]string{"acme/w?dgets"}, "acme/wdgets", false},
		{[]string{"acme/[a-"}, "acme/a", false},
		{[]string{"", "  "}, "acme/widgets", false},
		{nil, "acme/widgets", false},
		{[]string{"other/x", "acme/*"}, "acme/widgets", true},
	} {
		if got := Match(c.patterns, c.key); got != c.want {
			t.Errorf("Match(%q, %q) = %v, want %v", c.patterns, c.key, got, c.want)
		}
	}
	if ValidPattern("acme/[a-") || ValidPattern(" ") || !ValidPattern("acme/*") {
		t.Error("ValidPattern")
	}
}
