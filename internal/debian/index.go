// Package debian answers whether a name is a package an agent's shell
// can install: a binary package in the Debian release and component
// the agent-shell image installs from (Dockerfile.agent-shell), for the
// architecture its pods run on.
//
// The Index reads the archive's own package list (the Packages index
// apt-get update downloads), keeps it in memory and a copy on disk. It
// is what the software lists and propose_software_install check a name
// against before it is saved; the shell checks again when it installs.
// It also answers each package's version and one-line description, which
// an Install software request shows the owner.
//
// Once one download has succeeded, checks answer from that list (kept
// across restarts in the cache file) and a failed refresh only means
// the list stays as it is. Only an Index that has never had a list
// answers ErrUnavailable.
package debian

import (
	"bufio"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
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
// arch, a Go GOARCH (amd64 and arm64 are Debian's names too), on the
// mirror the image's apt uses. The image's apt sources list main only,
// so contrib and non-free packages cannot be installed and are not in
// the index.
func PackagesURL(arch string) string {
	return fmt.Sprintf("https://deb.debian.org/debian/dists/%s/main/binary-%s/Packages.gz", Suite, arch)
}

// DefaultURL is PackagesURL for this process's architecture: the
// server and the agent pods run on the same nodes.
func DefaultURL() string { return PackagesURL(runtime.GOARCH) }

// DefaultCachePath is where the server keeps its copy of the list:
// under the user cache directory ($HOME/.cache on Linux, which the
// server's image puts on the data volume, outside what backups take).
// Empty when there is no such directory.
func DefaultCachePath() string {
	dir, err := os.UserCacheDir()
	if err != nil {
		return ""
	}
	return filepath.Join(dir, "kivali", fmt.Sprintf("debian-%s-main-%s.json.gz", Suite, runtime.GOARCH))
}

const (
	// maxAge is how long a list is used before it is fetched again.
	// An older list is still used while the refresh runs, and for as
	// long as refreshes fail.
	maxAge = 24 * time.Hour
	// retryAfter is how long after a failed download no new one is
	// started.
	retryAfter = time.Minute
	// refreshTimeout bounds a background refresh.
	refreshTimeout = 5 * time.Minute
	// maxSuggestions bounds Unknown.Suggestions.
	maxSuggestions = 3
)

// maxIndexBytes bounds what one download reads: the compressed body
// and what it decompresses to, each. A Packages index is tens of
// megabytes; a response past this is a failed refresh, not a read
// that runs on. A variable so tests can lower it.
var maxIndexBytes int64 = 256 << 20

// ErrUnavailable is wrapped by Check when there is no list to check
// against: no download has ever succeeded and none is cached.
var ErrUnavailable = errors.New("the Debian package list could not be read")

// Unknown is a name apt cannot install.
type Unknown struct {
	Name string
	// ProvidedBy lists the packages that provide Name when it is a
	// virtual package that more than one package provides: apt-get
	// will not choose between them. Sorted.
	ProvidedBy []string
	// Suggestions are package names close to Name, closest first.
	Suggestions []string
}

// Checker is what callers need from an Index; tests substitute their
// own.
type Checker interface {
	Check(ctx context.Context, names []string) ([]Unknown, error)
	Lookup(names []string) []Package
	Warm(ctx context.Context)
}

// Package is what the list says about a name: the package apt-get
// installs for it, its version and its one-line description. Fields
// the list does not have are empty.
type Package struct {
	Name string
	// ProvidedBy is the package apt-get installs when Name is a virtual
	// package exactly one package provides; Version and Synopsis are
	// that package's.
	ProvidedBy string
	// Version is the upstream version: the Debian version without its
	// epoch or Debian revision (7:7.1.1-1+b1 is 7.1.1).
	Version string
	// Synopsis is the first line of the package's Description.
	Synopsis string
}

var _ Checker = (*Index)(nil)

// list is one downloaded package list. Never changed once built.
type list struct {
	ETag    string    `json:"etag"`
	Fetched time.Time `json:"fetched"`
	// Names are the packages, sorted.
	Names []string `json:"names"`
	// Provides maps each virtual package (a name only in Provides) to
	// the packages that provide it, sorted.
	Provides map[string][]string `json:"provides"`
	// Info is each package's version and synopsis, where its stanza
	// has them.
	Info map[string]info `json:"info"`

	set map[string]bool
}

// info is one package's Version field and the first line of its
// Description.
type info struct {
	Version  string `json:"v,omitempty"`
	Synopsis string `json:"s,omitempty"`
}

func (l *list) index() {
	l.set = make(map[string]bool, len(l.Names))
	for _, n := range l.Names {
		l.set[n] = true
	}
}

// Options configure an Index.
type Options struct {
	// URL is the Packages.gz to download.
	URL    string
	Client *http.Client
	Clock  clock.Clock
	// CachePath is the file the last good list is kept in; empty keeps
	// it in memory only.
	CachePath string
}

// Index is the package list. Use NewIndex. Safe for concurrent use.
type Index struct {
	opts Options
	// spawn runs a background refresh; a test seam.
	spawn func(func())

	loadOnce sync.Once
	// fetch serializes downloads; a channel so a caller waiting on
	// someone else's can give up with its context.
	fetch chan struct{}

	mu         sync.Mutex
	cur        *list
	failed     time.Time
	lastErr    error
	refreshing bool
}

// NewIndex returns an Index that loads opts.CachePath when first used,
// and downloads when it has no list or one older than a day.
func NewIndex(opts Options) *Index {
	return &Index{opts: opts, spawn: func(f func()) { go f() }, fetch: make(chan struct{}, 1)}
}

// Warm makes the list ready: it loads the cached copy, and downloads
// when there is none or it is due for a refresh. Errors are only
// remembered and logged.
func (ix *Index) Warm(ctx context.Context) {
	ix.load()
	if _, due := ix.state(); due {
		ix.refresh(ctx)
	}
}

// Check returns the names apt cannot install, in the order given: a
// name that is neither a package nor a virtual package exactly one
// package provides. It answers from the list it has, refreshing it in
// the background when it is old. The error wraps ErrUnavailable when
// there is no list at all, or is ctx's error.
func (ix *Index) Check(ctx context.Context, names []string) ([]Unknown, error) {
	ix.load()
	cur, due := ix.state()
	switch {
	case cur != nil && due:
		ix.startRefresh()
	case cur == nil && due:
		ix.refresh(ctx)
		cur, _ = ix.state()
	}
	if cur == nil {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		ix.mu.Lock()
		err := ix.lastErr
		ix.mu.Unlock()
		return nil, fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	var unknown []Unknown
	for _, n := range names {
		if cur.set[n] {
			continue
		}
		by := cur.Provides[n]
		if len(by) == 1 {
			continue // apt-get installs the one provider
		}
		unknown = append(unknown, Unknown{Name: n, ProvidedBy: by, Suggestions: suggest(cur.Names, n)})
	}
	return unknown, nil
}

// Lookup returns what the list says about each name, in the order
// given. It never waits for a download: with no list yet each Package
// carries only its Name, and an old list is refreshed in the
// background.
func (ix *Index) Lookup(names []string) []Package {
	ix.load()
	cur, due := ix.state()
	if due {
		ix.startRefresh()
	}
	out := make([]Package, 0, len(names))
	for _, n := range names {
		p := Package{Name: n}
		if cur != nil {
			pkg := n
			if by := cur.Provides[n]; !cur.set[n] && len(by) == 1 {
				pkg, p.ProvidedBy = by[0], by[0]
			}
			in := cur.Info[pkg]
			p.Version, p.Synopsis = upstreamVersion(in.Version), in.Synopsis
		}
		out = append(out, p)
	}
	return out
}

// upstreamVersion is a Debian version without its epoch ("7:") or
// Debian revision ("-1+b1").
func upstreamVersion(v string) string {
	if i := strings.IndexByte(v, ':'); i >= 0 {
		v = v[i+1:]
	}
	if i := strings.LastIndexByte(v, '-'); i >= 0 {
		v = v[:i]
	}
	return v
}

// state is the current list and whether a download is due: the list is
// missing or older than maxAge, and no download failed within
// retryAfter.
func (ix *Index) state() (*list, bool) {
	ix.mu.Lock()
	defer ix.mu.Unlock()
	now := ix.opts.Clock.Now()
	stale := ix.cur == nil || now.Sub(ix.cur.Fetched) >= maxAge
	backingOff := !ix.failed.IsZero() && now.Sub(ix.failed) < retryAfter
	return ix.cur, stale && !backingOff
}

// startRefresh refreshes in the background, unless a background
// refresh is already running.
func (ix *Index) startRefresh() {
	ix.mu.Lock()
	if ix.refreshing {
		ix.mu.Unlock()
		return
	}
	ix.refreshing = true
	ix.mu.Unlock()
	ix.spawn(func() {
		defer func() {
			ix.mu.Lock()
			ix.refreshing = false
			ix.mu.Unlock()
		}()
		ctx, cancel := context.WithTimeout(context.Background(), refreshTimeout)
		defer cancel()
		ix.refresh(ctx)
	})
}

// refresh downloads the list unless another download, finished while
// this one waited, made it unnecessary. A failure keeps the list there
// is.
func (ix *Index) refresh(ctx context.Context) {
	select {
	case ix.fetch <- struct{}{}:
	case <-ctx.Done():
		return
	}
	defer func() { <-ix.fetch }()
	cur, due := ix.state()
	if !due {
		return
	}
	next, err := ix.download(ctx, cur)
	ix.mu.Lock()
	if err != nil {
		ix.failed, ix.lastErr = ix.opts.Clock.Now(), err
	} else {
		ix.cur, ix.failed, ix.lastErr = next, time.Time{}, nil
	}
	ix.mu.Unlock()
	if err != nil {
		log.Printf("debian: downloading the package list: %v", err)
		return
	}
	ix.save(next)
}

// download fetches the list; a 304 to cur's ETag renews cur.
func (ix *Index) download(ctx context.Context, cur *list) (*list, error) {
	url := ix.opts.URL
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	if cur != nil && cur.ETag != "" {
		req.Header.Set("If-None-Match", cur.ETag)
	}
	resp, err := ix.opts.Client.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	switch {
	case resp.StatusCode == http.StatusNotModified && cur != nil:
		renewed := *cur
		renewed.Fetched = ix.opts.Clock.Now()
		return &renewed, nil
	case resp.StatusCode != http.StatusOK:
		return nil, fmt.Errorf("%s: %s", url, resp.Status)
	}
	// Each reader stops one byte past the bound, so a body or an index
	// that reaches it is told apart from one that fits exactly.
	body := &countingReader{r: io.LimitReader(resp.Body, maxIndexBytes+1)}
	zr, err := gzip.NewReader(body)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", url, err)
	}
	plain := &countingReader{r: io.LimitReader(zr, maxIndexBytes+1)}
	l, perr := parse(plain)
	if body.n > maxIndexBytes || plain.n > maxIndexBytes {
		return nil, fmt.Errorf("%s: the index is larger than %d bytes", url, maxIndexBytes)
	}
	if perr != nil {
		return nil, fmt.Errorf("%s: %w", url, perr)
	}
	l.ETag, l.Fetched = resp.Header.Get("ETag"), ix.opts.Clock.Now()
	return l, nil
}

// countingReader counts the bytes read through it.
type countingReader struct {
	r io.Reader
	n int64
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += int64(n)
	return n, err
}

// cacheFile is the cache file's content.
type cacheFile struct {
	// URL is Options.URL: a cache written for another release or
	// architecture is not used.
	URL  string `json:"url"`
	List *list  `json:"list"`
}

// load reads the cache file, once. A missing or unreadable file is no
// list.
func (ix *Index) load() {
	ix.loadOnce.Do(func() {
		if ix.opts.CachePath == "" {
			return
		}
		f, err := os.Open(ix.opts.CachePath)
		if err != nil {
			if !errors.Is(err, os.ErrNotExist) {
				log.Printf("debian: reading the cached package list: %v", err)
			}
			return
		}
		defer func() { _ = f.Close() }()
		var c cacheFile
		zr, err := gzip.NewReader(f)
		if err == nil {
			err = json.NewDecoder(zr).Decode(&c)
		}
		if err != nil || c.URL != ix.opts.URL || c.List == nil || len(c.List.Names) == 0 {
			log.Printf("debian: ignoring the cached package list %s (%v)", ix.opts.CachePath, err)
			return
		}
		if c.List.Info == nil {
			// Written before the list kept versions and descriptions:
			// used for checks, and downloaded again in full now.
			c.List.Fetched, c.List.ETag = time.Time{}, ""
		}
		c.List.index()
		ix.mu.Lock()
		if ix.cur == nil {
			ix.cur = c.List
		}
		ix.mu.Unlock()
	})
}

// save writes l to the cache file: a temporary file renamed into
// place, so a reader never sees half of one.
func (ix *Index) save(l *list) {
	if ix.opts.CachePath == "" {
		return
	}
	err := func() error {
		dir := filepath.Dir(ix.opts.CachePath)
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return err
		}
		tmp, err := os.CreateTemp(dir, ".tmp-debian-")
		if err != nil {
			return err
		}
		defer func() { _ = os.Remove(tmp.Name()) }()
		zw := gzip.NewWriter(tmp)
		err = json.NewEncoder(zw).Encode(cacheFile{URL: ix.opts.URL, List: l})
		if err == nil {
			err = zw.Close()
		}
		if cerr := tmp.Close(); err == nil {
			err = cerr
		}
		if err != nil {
			return err
		}
		return os.Rename(tmp.Name(), ix.opts.CachePath)
	}()
	if err != nil {
		log.Printf("debian: caching the package list: %v", err)
	}
}

// parse reads a Packages file: the Package, Provides, Version and
// Description fields of each stanza. Only Description's first line,
// the synopsis, is kept.
func parse(r io.Reader) (*list, error) {
	names := map[string]bool{}
	infos := map[string]info{}
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
		case strings.HasPrefix(line, "Version:") && pkg != "":
			in := infos[pkg]
			in.Version = strings.TrimSpace(strings.TrimPrefix(line, "Version:"))
			infos[pkg] = in
		case strings.HasPrefix(line, "Description:") && pkg != "":
			in := infos[pkg]
			in.Synopsis = strings.TrimSpace(strings.TrimPrefix(line, "Description:"))
			infos[pkg] = in
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
		return nil, err
	}
	if len(names) == 0 {
		return nil, errors.New("no packages in the index")
	}
	l := &list{Names: make([]string, 0, len(names)), Provides: map[string][]string{}, Info: infos}
	for n := range names {
		l.Names = append(l.Names, n)
	}
	sort.Strings(l.Names)
	for v, by := range providers {
		if names[v] {
			continue
		}
		p := make([]string, 0, len(by))
		for n := range by {
			p = append(p, n)
		}
		sort.Strings(p)
		l.Provides[v] = p
	}
	l.index()
	return l, nil
}

// suggest returns up to maxSuggestions of the sorted names close to
// name: those that begin with name and a '-', shortest first, then
// those at the smallest edit distance found within a small limit.
func suggest(sorted []string, name string) []string {
	var longer []string
	for _, n := range sorted {
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
	for _, n := range sorted {
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
		return fmt.Sprintf("%s is not a package itself in %s; several packages provide it (%s), so name the one you want.", u.Name, Release, orList(u.ProvidedBy))
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
