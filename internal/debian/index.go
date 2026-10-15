// Package debian answers whether a name is a package an agent's shell
// can install: a binary package in the Debian release and component
// the agent-shell image installs from (Dockerfile.agent-shell), for the
// architecture its pods run on.
//
// The Index reads the archive's own package list (the Packages index
// apt-get update downloads) and keeps the names in memory. It is what
// the software lists and propose_software_install check a name against
// before it is saved; the shell checks again when it installs.
package debian

import (
	"bufio"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"runtime"
	"sort"
	"strings"
	"time"

	"github.com/kivali-ai/kivali/internal/clock"
)

// Suite is the Debian release the agent-shell image is built on and
// installs from. TestSuiteMatchesAgentShellImage keeps it in step with
// Dockerfile.agent-shell's base image.
const Suite = "trixie"

// Release is Suite as the owner and agents read it.
const Release = "Debian 13"

// PackagesURL is the Packages index of Suite's main component for
// arch, a Go GOARCH (amd64 and arm64 are Debian's names too). The
// image's apt sources list main only, so contrib and non-free packages
// cannot be installed and are not in the index.
func PackagesURL(arch string) string {
	return fmt.Sprintf("https://deb.debian.org/debian/dists/%s/main/binary-%s/Packages.gz", Suite, arch)
}

// DefaultURL is PackagesURL for this process's architecture: the
// server and the agent pods run on the same nodes.
func DefaultURL() string { return PackagesURL(runtime.GOARCH) }

const (
	// maxAge is how long a downloaded index is used before it is
	// fetched again. The archive changes at point releases.
	maxAge = 24 * time.Hour
	// retryAfter is how long after a failed download Check answers
	// ErrUnavailable without trying again, so an add while the archive
	// is unreachable does not wait for every attempt.
	retryAfter = time.Minute
	// maxSuggestions bounds Unknown.Suggestions.
	maxSuggestions = 3
)

// ErrUnavailable is wrapped by Check when no index could be read: the
// archive could not be reached and nothing was downloaded before.
var ErrUnavailable = errors.New("the Debian package list could not be read")

// Unknown is a name that is not a package in the index.
type Unknown struct {
	Name string
	// ProvidedBy lists the packages that provide Name when it is a
	// virtual package (a name packages declare in Provides, which apt
	// cannot install by itself). Sorted.
	ProvidedBy []string
	// Suggestions are package names close to Name, closest first.
	Suggestions []string
}

// Checker is what callers need from an Index; tests substitute their
// own.
type Checker interface {
	Check(ctx context.Context, names []string) ([]Unknown, error)
	Warm(ctx context.Context)
}

var _ Checker = (*Index)(nil)

// Index is the in-memory package list. The zero value is not usable;
// use NewIndex. Safe for concurrent use.
type Index struct {
	url    string
	client *http.Client
	clock  clock.Clock

	// sem serializes downloads and guards the fields below; a channel
	// so a caller waiting on someone else's download can give up with
	// its context.
	sem      chan struct{}
	names    map[string]bool
	sorted   []string
	provides map[string][]string
	etag     string
	fetched  time.Time
	failed   time.Time
	lastErr  error
}

// NewIndex returns an Index that downloads url (a gzipped Packages
// file) with client when first asked, and again once a day.
func NewIndex(url string, client *http.Client, clk clock.Clock) *Index {
	return &Index{url: url, client: client, clock: clk, sem: make(chan struct{}, 1)}
}

// Warm downloads the index if it is missing or stale, so a check soon
// after does not wait. Errors are only remembered.
func (ix *Index) Warm(ctx context.Context) {
	_ = ix.withIndex(ctx, func() {})
}

// Check returns the names that are not packages in the index, in the
// order given. The error wraps ErrUnavailable when there is no index
// to check against, or is ctx's error.
func (ix *Index) Check(ctx context.Context, names []string) ([]Unknown, error) {
	var unknown []Unknown
	err := ix.withIndex(ctx, func() {
		for _, n := range names {
			if ix.names[n] {
				continue
			}
			unknown = append(unknown, Unknown{Name: n, ProvidedBy: ix.provides[n], Suggestions: ix.suggest(n)})
		}
	})
	return unknown, err
}

// withIndex runs f holding the index, after downloading it if it is
// missing or older than maxAge. A stale index whose refresh fails is
// still used.
func (ix *Index) withIndex(ctx context.Context, f func()) error {
	select {
	case ix.sem <- struct{}{}:
	case <-ctx.Done():
		return ctx.Err()
	}
	defer func() { <-ix.sem }()

	now := ix.clock.Now()
	stale := ix.names == nil || now.Sub(ix.fetched) >= maxAge
	recentlyFailed := !ix.failed.IsZero() && now.Sub(ix.failed) < retryAfter
	if stale && !recentlyFailed {
		if err := ix.fetch(ctx); err != nil {
			ix.failed, ix.lastErr = ix.clock.Now(), err
		} else {
			ix.failed, ix.lastErr = time.Time{}, nil
		}
	}
	if ix.names == nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return fmt.Errorf("%w: %v", ErrUnavailable, ix.lastErr)
	}
	f()
	return nil
}

// fetch downloads and parses the index; the caller holds sem.
func (ix *Index) fetch(ctx context.Context) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, ix.url, nil)
	if err != nil {
		return err
	}
	if ix.names != nil && ix.etag != "" {
		req.Header.Set("If-None-Match", ix.etag)
	}
	resp, err := ix.client.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	switch resp.StatusCode {
	case http.StatusNotModified:
		if ix.names == nil {
			return fmt.Errorf("%s: not modified, but nothing was downloaded before", ix.url)
		}
		ix.fetched = ix.clock.Now()
		return nil
	case http.StatusOK:
	default:
		return fmt.Errorf("%s: %s", ix.url, resp.Status)
	}
	zr, err := gzip.NewReader(resp.Body)
	if err != nil {
		return fmt.Errorf("%s: %w", ix.url, err)
	}
	names, provides, err := parse(zr)
	if err != nil {
		return fmt.Errorf("%s: %w", ix.url, err)
	}
	ix.names, ix.provides, ix.etag, ix.fetched = names, provides, resp.Header.Get("ETag"), ix.clock.Now()
	ix.sorted = make([]string, 0, len(names))
	for n := range names {
		ix.sorted = append(ix.sorted, n)
	}
	sort.Strings(ix.sorted)
	return nil
}

// parse reads a Packages file: the Package and Provides fields of each
// stanza. Provides names that are packages themselves are left out of
// the result's provides.
func parse(r io.Reader) (map[string]bool, map[string][]string, error) {
	names := map[string]bool{}
	providers := map[string]map[string]bool{}
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64*1024), 4*1024*1024)
	var pkg string
	for sc.Scan() {
		line := sc.Text()
		switch {
		case line == "":
			pkg = ""
		case strings.HasPrefix(line, "Package:"):
			pkg = strings.TrimSpace(strings.TrimPrefix(line, "Package:"))
			names[pkg] = true
		case strings.HasPrefix(line, "Provides:") && pkg != "":
			for _, p := range strings.Split(strings.TrimPrefix(line, "Provides:"), ",") {
				// "name (= version)" or "name:any"
				p = strings.TrimSpace(p)
				if i := strings.IndexAny(p, " (:"); i >= 0 {
					p = p[:i]
				}
				if p == "" {
					continue
				}
				if providers[p] == nil {
					providers[p] = map[string]bool{}
				}
				providers[p][pkg] = true
			}
		}
	}
	if err := sc.Err(); err != nil {
		return nil, nil, err
	}
	if len(names) == 0 {
		return nil, nil, errors.New("no packages in the index")
	}
	provides := map[string][]string{}
	for v, by := range providers {
		if names[v] {
			continue
		}
		list := make([]string, 0, len(by))
		for p := range by {
			list = append(list, p)
		}
		sort.Strings(list)
		provides[v] = list
	}
	return names, provides, nil
}

// suggest returns up to maxSuggestions package names close to name:
// those that begin with name and a '-', shortest first, then those at
// the smallest edit distance found within a small limit. The caller
// holds sem.
func (ix *Index) suggest(name string) []string {
	var longer []string
	for _, n := range ix.sorted {
		if strings.HasPrefix(n, name+"-") {
			longer = append(longer, n)
		}
	}
	sort.SliceStable(longer, func(i, j int) bool { return len(longer[i]) < len(longer[j]) })

	limit := 2
	if len(name) <= 4 {
		limit = 1
	}
	var near []string
	for _, n := range ix.sorted {
		if d := len(n) - len(name); d > limit || -d > limit {
			continue
		}
		switch d := distance(name, n, limit); {
		case d < limit:
			limit, near = d, []string{n}
		case d == limit:
			near = append(near, n)
		}
	}

	out := []string{}
	for _, n := range append(longer, near...) {
		if len(out) == maxSuggestions {
			break
		}
		out = append(out, n)
	}
	return out
}

// distance is the Levenshtein distance between a and b, or limit+1
// once it is certain to exceed limit.
func distance(a, b string, limit int) int {
	prev := make([]int, len(b)+1)
	cur := make([]int, len(b)+1)
	for j := range prev {
		prev[j] = j
	}
	for i := 1; i <= len(a); i++ {
		cur[0] = i
		best := cur[0]
		for j := 1; j <= len(b); j++ {
			cost := 1
			if a[i-1] == b[j-1] {
				cost = 0
			}
			cur[j] = min(prev[j]+1, cur[j-1]+1, prev[j-1]+cost)
			best = min(best, cur[j])
		}
		if best > limit {
			return limit + 1
		}
		prev, cur = cur, prev
	}
	return prev[len(b)]
}

// Describe is u as one sentence for the owner or an agent.
func (u Unknown) Describe() string {
	switch {
	case len(u.ProvidedBy) > 0:
		return fmt.Sprintf("%s is not a package itself in %s; it is provided by %s, so name one of those.", u.Name, Release, orList(u.ProvidedBy))
	case len(u.Suggestions) > 0:
		return fmt.Sprintf("%s has no package called %s. Did you mean %s?", Release, u.Name, orList(u.Suggestions))
	default:
		return fmt.Sprintf("%s has no package called %s.", Release, u.Name)
	}
}

// orList joins names as "a", "a or b", "a, b or c"; more than five
// are cut to five and "others".
func orList(names []string) string {
	if len(names) > 5 {
		names = append(append([]string{}, names[:5]...), "others")
	}
	if len(names) == 1 {
		return names[0]
	}
	return strings.Join(names[:len(names)-1], ", ") + " or " + names[len(names)-1]
}
