// Package gitrepo is a repository on a git host as Kivali's Git
// integration names it: the connection's host and the repository's
// path on it, owner/name (or group/sub/project, as GitLab nests
// groups). Core reads an agent's call with it, and the agent-shell
// daemon reads a working copy's origin with it, so both hold a URL to
// the same rules. It imports nothing from this module.
package gitrepo

import (
	"errors"
	"fmt"
	"net"
	"net/url"
	"path"
	"strconv"
	"strings"
)

// maxPathLen bounds a repository's path.
const maxPathLen = 512

// Repository is one repository: the scheme it is reached by (https, or
// http where a test allows it), the connection's host (a host name,
// with a port when it has one) and its path, segments as written.
type Repository struct {
	Scheme string
	Host   string
	Path   string
}

// Key is the repository's path as access patterns match it: lower
// case. The path as written is kept for the URL Kivali builds.
func (r Repository) Key() string { return strings.ToLower(r.Path) }

// URL is the repository's URL with no user info: its scheme, the
// host, the path as written and .git.
func (r Repository) URL() string {
	return r.Scheme + "://" + r.Host + "/" + r.Path + ".git"
}

// OtherHostError is a URL on a host other than the connection's.
type OtherHostError struct{ Host string }

func (e *OtherHostError) Error() string {
	return "the connection is for " + e.Host
}

// ErrNotHTTPS is a URL whose scheme is not https.
var ErrNotHTTPS = errors.New("only an HTTPS URL is supported")

// NormalizeHost is a connection's host as the owner wrote it, made
// exact: spaces, an https:// in front and slashes behind dropped, in
// lower case. It must be a host name or an IP address, with an
// optional port, and nothing else.
func NormalizeHost(raw string) (string, error) {
	h := strings.TrimSpace(raw)
	if len(h) >= len("https://") && strings.EqualFold(h[:len("https://")], "https://") {
		h = h[len("https://"):]
	}
	h = strings.ToLower(strings.TrimRight(h, "/"))
	name, port, hasPort := strings.Cut(h, ":")
	if hasPort {
		n, err := strconv.Atoi(port)
		if err != nil || n < 1 || n > 65535 || strconv.Itoa(n) != port {
			return "", fmt.Errorf("%q is not a host name", raw)
		}
	}
	if !validHostName(name) {
		return "", fmt.Errorf("%q is not a host name", raw)
	}
	return h, nil
}

// HostName is host without its port.
func HostName(host string) string {
	name, _, _ := strings.Cut(host, ":")
	return name
}

func validHostName(h string) bool {
	if h == "" || len(h) > 253 || strings.HasPrefix(h, ".") || strings.HasSuffix(h, ".") || strings.Contains(h, "..") {
		return false
	}
	for _, label := range strings.Split(h, ".") {
		if label == "" || len(label) > 63 || strings.HasPrefix(label, "-") || strings.HasSuffix(label, "-") {
			return false
		}
		if strings.IndexFunc(label, func(c rune) bool { return !lowerOrDigit(c) && c != '-' }) >= 0 {
			return false
		}
	}
	return true
}

func lowerOrDigit(c rune) bool { return c >= 'a' && c <= 'z' || c >= '0' && c <= '9' }

// nameRune reports whether c may be in a segment of a repository's
// path.
func nameRune(c rune) bool {
	return lowerOrDigit(c) || c >= 'A' && c <= 'Z' || c == '.' || c == '_' || c == '-'
}

// Parse is ref, a repository's URL or its owner/name, on host (as
// NormalizeHost returns it). A URL must be https://<host>/<path>, with
// an optional .git and no user info, query or fragment (http too when
// insecureHTTP, for tests); a URL on another host is an
// *OtherHostError. Anything without :// is owner/name, used as it is,
// reached over https (http when insecureHTTP).
func Parse(ref, host string, insecureHTTP bool) (Repository, error) {
	ref = strings.TrimSpace(ref)
	if !strings.Contains(ref, "://") {
		if strings.ContainsAny(ref, ":@") {
			return Repository{}, fmt.Errorf("%q is neither an HTTPS URL nor owner/name", ref)
		}
		p, err := cleanPath(ref)
		if err != nil {
			return Repository{}, err
		}
		scheme := "https"
		if insecureHTTP {
			scheme = "http"
		}
		return Repository{Scheme: scheme, Host: host, Path: p}, nil
	}
	return ParseURL(ref, host, insecureHTTP)
}

// ParseURL is Parse for a URL alone: a ref without a scheme is refused.
func ParseURL(raw, host string, insecureHTTP bool) (Repository, error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return Repository{}, errors.New("the URL does not parse")
	}
	switch {
	case u.Scheme == "https":
	case u.Scheme == "http" && insecureHTTP:
	default:
		return Repository{}, ErrNotHTTPS
	}
	if u.User != nil {
		return Repository{}, errors.New("the URL carries a user name or password; give it without")
	}
	if u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || u.Opaque != "" {
		return Repository{}, errors.New("the URL has a query or a fragment")
	}
	if !strings.EqualFold(u.Host, host) {
		return Repository{}, &OtherHostError{Host: host}
	}
	p, err := cleanPath(u.Path)
	if err != nil {
		return Repository{}, err
	}
	return Repository{Scheme: u.Scheme, Host: host, Path: p}, nil
}

// cleanPath is a repository's path with slashes around it and a .git
// suffix trimmed: two or more segments of letters, digits, '.', '_'
// and '-', none of them "." or "..".
func cleanPath(p string) (string, error) {
	p = strings.Trim(p, "/")
	p = strings.TrimSuffix(p, ".git")
	p = strings.Trim(p, "/")
	if p == "" || len(p) > maxPathLen {
		return "", errors.New("name the repository as owner/name")
	}
	segs := strings.Split(p, "/")
	if len(segs) < 2 {
		return "", fmt.Errorf("%q is not owner/name", p)
	}
	for _, s := range segs {
		if s == "" || s == "." || s == ".." {
			return "", fmt.Errorf("%q is not owner/name", p)
		}
		if strings.IndexFunc(s, func(c rune) bool { return !nameRune(c) }) >= 0 {
			return "", fmt.Errorf("%q has a character a repository's name cannot have", p)
		}
	}
	return p, nil
}

// PublicAddresses reports whether every one of ips, a host's addresses,
// is public: none loopback, private (RFC 1918, fc00::/7), link-local
// (169.254.0.0/16, fe80::/10), unspecified or multicast. No addresses
// is not public.
func PublicAddresses(ips []net.IP) bool {
	if len(ips) == 0 {
		return false
	}
	for _, ip := range ips {
		if ip == nil || ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() ||
			ip.IsUnspecified() || ip.IsMulticast() {
			return false
		}
	}
	return true
}

// NormalizePattern is an access pattern as Match reads it: slashes
// around it and a .git suffix trimmed, in lower case.
func NormalizePattern(p string) string {
	p = strings.Trim(strings.TrimSpace(p), "/")
	p = strings.TrimSuffix(p, ".git")
	return strings.ToLower(strings.Trim(p, "/"))
}

// ValidPattern reports whether p is a pattern Match can read: not
// empty, and a pattern of path.Match.
func ValidPattern(p string) bool {
	n := NormalizePattern(p)
	if n == "" {
		return false
	}
	_, err := path.Match(n, "")
	return err == nil
}

// Match reports whether the repository with key (Repository.Key) is
// one of patterns: each is matched with path.Match semantics, so * and
// ? stand within one segment and never across a /, case-insensitively;
// a pattern without * or ? is the one repository it names. A pattern
// that does not parse matches nothing.
func Match(patterns []string, key string) bool {
	key = strings.ToLower(key)
	for _, p := range patterns {
		n := NormalizePattern(p)
		if n == "" {
			continue
		}
		if ok, err := path.Match(n, key); err == nil && ok {
			return true
		}
	}
	return false
}
