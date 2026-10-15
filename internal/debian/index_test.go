package debian

import (
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kivali-ai/kivali/internal/clock"
)

const packagesFixture = `Package: ffmpeg
Version: 7:7.1.1-1
Description: Tools for transcoding, streaming and playing of multimedia files
 A long description line that is not a field.
 Package: not-a-package

Package: poppler-utils
Version: 25.03.0-5

Package: postgresql-client-17
Provides: postgresql-client (= 17), postgresql-client:any

Package: postgresql-client
Version: 17+278

Package: mawk
Provides: awk

Package: gawk
Provides: awk (= 1:5.2.1)

Package: exim4-daemon-light
Version: 4.98.2-1+b1
Provides: mail-transport-agent
Description: Exim MTA (v4) daemon with basic features

Package: python3
Package-Type: deb

Package: python3-numpy

Package: python3-pandas

Package: vim
`

// archive serves a gzipped Packages file with an ETag, answering 304
// to a matching If-None-Match, and counts requests and full downloads.
type archive struct {
	body      []byte
	etag      string
	status    atomic.Int32
	downloads atomic.Int32
	requests  atomic.Int32
}

func newArchive(t *testing.T, packages string) (*archive, *httptest.Server) {
	t.Helper()
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	if _, err := zw.Write([]byte(packages)); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	a := &archive{body: buf.Bytes(), etag: `"v1"`}
	a.status.Store(http.StatusOK)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		a.requests.Add(1)
		if s := int(a.status.Load()); s != http.StatusOK {
			w.WriteHeader(s)
			return
		}
		if r.Header.Get("If-None-Match") == a.etag {
			w.WriteHeader(http.StatusNotModified)
			return
		}
		a.downloads.Add(1)
		w.Header().Set("ETag", a.etag)
		_, _ = w.Write(a.body)
	}))
	t.Cleanup(srv.Close)
	return a, srv
}

// newTestIndex is an Index on srv whose background refreshes run
// before startRefresh returns.
func newTestIndex(srv *httptest.Server, clk clock.Clock, cachePath string) *Index {
	ix := NewIndex(Options{URL: srv.URL + "/Packages.gz", Client: srv.Client(), Clock: clk, CachePath: cachePath})
	ix.spawn = func(f func()) { f() }
	return ix
}

func names(unknown []Unknown) []string {
	out := []string{}
	for _, u := range unknown {
		out = append(out, u.Name)
	}
	return out
}

func TestCheckKnowsPackagesAndNothingElse(t *testing.T) {
	_, srv := newArchive(t, packagesFixture)
	ix := newTestIndex(srv, clock.NewFake(), "")
	unknown, err := ix.Check(context.Background(), []string{"ffmpeg", "poppler-utils", "postgresql-client", "not-a-package", "version"})
	if err != nil {
		t.Fatal(err)
	}
	// A continuation line that looks like a field is not a package.
	if got, want := names(unknown), []string{"not-a-package", "version"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("unknown = %v, want %v", got, want)
	}
}

// apt-get installs a virtual package's provider when there is one, and
// will not choose when there are several.
func TestCheckVirtualPackages(t *testing.T) {
	_, srv := newArchive(t, packagesFixture)
	ix := newTestIndex(srv, clock.NewFake(), "")
	unknown, err := ix.Check(context.Background(), []string{"awk", "mail-transport-agent"})
	if err != nil {
		t.Fatal(err)
	}
	if len(unknown) != 1 || unknown[0].Name != "awk" {
		t.Fatalf("unknown = %+v, want only awk", unknown)
	}
	if want := []string{"gawk", "mawk"}; !reflect.DeepEqual(unknown[0].ProvidedBy, want) {
		t.Errorf("awk provided by %v, want %v", unknown[0].ProvidedBy, want)
	}
	if got := unknown[0].Describe(); got != "awk is not a package itself in Debian 13; several packages provide it (gawk or mawk), so name the one you want." {
		t.Errorf("Describe = %q", got)
	}
}

func TestCheckSuggestsCloseNames(t *testing.T) {
	_, srv := newArchive(t, packagesFixture)
	ix := newTestIndex(srv, clock.NewFake(), "")
	cases := []struct {
		name string
		want []string
	}{
		{"ffmpg", []string{"ffmpeg"}},
		{"popler-utils", []string{"poppler-utils"}},
		{"postgres-client", []string{"postgresql-client"}},
		{"python3-numpi", []string{"python3-numpy"}},
		// Only the closest: postgresql-client is two edits away.
		{"postgresql-client-1", []string{"postgresql-client-17"}},
		// Short names allow only one edit.
		{"vmi", []string{}},
		{"zzzzzz", []string{}},
	}
	for _, c := range cases {
		unknown, err := ix.Check(context.Background(), []string{c.name})
		if err != nil {
			t.Fatal(err)
		}
		if len(unknown) != 1 {
			t.Fatalf("%s: unknown = %+v", c.name, unknown)
		}
		if !reflect.DeepEqual(unknown[0].Suggestions, c.want) {
			t.Errorf("%s: suggestions %v, want %v", c.name, unknown[0].Suggestions, c.want)
		}
	}
}

func TestCheckSuggestsLongerNamesForAPrefix(t *testing.T) {
	_, srv := newArchive(t, "Package: poppler-utils\n\nPackage: poppler-data\n\nPackage: toppler\n")
	ix := newTestIndex(srv, clock.NewFake(), "")
	unknown, err := ix.Check(context.Background(), []string{"poppler"})
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"poppler-data", "poppler-utils", "toppler"}; !reflect.DeepEqual(unknown[0].Suggestions, want) {
		t.Fatalf("suggestions %v, want %v", unknown[0].Suggestions, want)
	}
	if got := unknown[0].Describe(); got != "Debian 13 has no package called poppler. Did you mean poppler-data, poppler-utils or toppler?" {
		t.Errorf("Describe = %q", got)
	}
}

// A day-old list answers at once and is revalidated in the background;
// a failed refresh keeps it.
func TestIndexRefreshesOnceADayAndKeepsTheListWhenThatFails(t *testing.T) {
	a, srv := newArchive(t, packagesFixture)
	clk := clock.NewFake()
	ix := newTestIndex(srv, clk, "")
	ctx := context.Background()
	for range 3 {
		if _, err := ix.Check(ctx, []string{"ffmpeg"}); err != nil {
			t.Fatal(err)
		}
	}
	if got := a.requests.Load(); got != 1 {
		t.Fatalf("requests = %d after three checks, want 1", got)
	}
	clk.Advance(maxAge)
	if _, err := ix.Check(ctx, []string{"ffmpeg"}); err != nil {
		t.Fatal(err)
	}
	if req, dl := a.requests.Load(), a.downloads.Load(); req != 2 || dl != 1 {
		t.Fatalf("requests = %d, downloads = %d after a day; want a 304 revalidation (2, 1)", req, dl)
	}
	a.status.Store(http.StatusBadGateway)
	clk.Advance(maxAge)
	unknown, err := ix.Check(ctx, []string{"ffmpeg", "ffmpg"})
	if err != nil {
		t.Fatalf("the list there is was not used: %v", err)
	}
	if got := names(unknown); !reflect.DeepEqual(got, []string{"ffmpg"}) {
		t.Fatalf("unknown = %v, want ffmpg", got)
	}
}

// The cached list answers after a restart while the archive is down.
func TestIndexUsesItsCacheAcrossARestart(t *testing.T) {
	a, srv := newArchive(t, packagesFixture)
	clk := clock.NewFake()
	cache := filepath.Join(t.TempDir(), "cache", "debian.json.gz")
	newTestIndex(srv, clk, cache).Warm(context.Background())
	if _, err := os.Stat(cache); err != nil {
		t.Fatalf("no cache written: %v", err)
	}

	a.status.Store(http.StatusServiceUnavailable)
	clk.Advance(3 * maxAge)
	restarted := newTestIndex(srv, clk, cache)
	unknown, err := restarted.Check(context.Background(), []string{"ffmpeg", "ffmpg"})
	if err != nil {
		t.Fatalf("cached list not used: %v", err)
	}
	if got := names(unknown); !reflect.DeepEqual(got, []string{"ffmpg"}) {
		t.Fatalf("unknown = %v, want ffmpg", got)
	}

	// A cache for another URL (release or architecture) is not used.
	other := NewIndex(Options{URL: srv.URL + "/other/Packages.gz", Client: srv.Client(), Clock: clk, CachePath: cache})
	if _, err := other.Check(context.Background(), []string{"ffmpeg"}); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("err = %v, want ErrUnavailable", err)
	}
}

func TestLookupGivesVersionAndSynopsis(t *testing.T) {
	_, srv := newArchive(t, packagesFixture)
	ix := newTestIndex(srv, clock.NewFake(), "")
	ix.Warm(context.Background())
	got := ix.Lookup([]string{"ffmpeg", "postgresql-client", "mail-transport-agent", "vim", "not-a-package"})
	want := []Package{
		// The epoch and Debian revision are dropped; only the
		// Description's first line is kept.
		{Name: "ffmpeg", Version: "7.1.1", Synopsis: "Tools for transcoding, streaming and playing of multimedia files"},
		// A native package has no revision to drop.
		{Name: "postgresql-client", Version: "17+278"},
		// A virtual name one package provides is described by that package.
		{Name: "mail-transport-agent", ProvidedBy: "exim4-daemon-light", Version: "4.98.2", Synopsis: "Exim MTA (v4) daemon with basic features"},
		{Name: "vim"},
		{Name: "not-a-package"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Lookup =\n%+v\nwant\n%+v", got, want)
	}
}

// Lookup never waits for the archive: without a list it has only names.
func TestLookupWithoutAList(t *testing.T) {
	a, srv := newArchive(t, packagesFixture)
	a.status.Store(http.StatusServiceUnavailable)
	ix := newTestIndex(srv, clock.NewFake(), "")
	if got, want := ix.Lookup([]string{"ffmpeg"}), []Package{{Name: "ffmpeg"}}; !reflect.DeepEqual(got, want) {
		t.Fatalf("Lookup = %+v, want %+v", got, want)
	}
}

// A cache written before the list kept versions and descriptions still
// answers checks, and is downloaded again in full, not revalidated.
func TestIndexDownloadsAgainOverACacheWithoutInfo(t *testing.T) {
	a, srv := newArchive(t, packagesFixture)
	clk := clock.NewFake()
	url := srv.URL + "/Packages.gz"
	cache := filepath.Join(t.TempDir(), "debian.json.gz")
	old := `{"url":` + strconv.Quote(url) + `,"list":{"etag":` + strconv.Quote(a.etag) + `,"fetched":` + strconv.Quote(clk.Now().Format(time.RFC3339Nano)) + `,"names":["ffmpeg"],"provides":{}}}`
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	_, _ = zw.Write([]byte(old))
	_ = zw.Close()
	if err := os.WriteFile(cache, buf.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}

	ix := newTestIndex(srv, clk, cache)
	ix.Lookup([]string{"ffmpeg"})
	if got := a.downloads.Load(); got != 1 {
		t.Fatalf("downloads = %d, want 1 full download", got)
	}
	if got := ix.Lookup([]string{"ffmpeg"})[0].Version; got != "7.1.1" {
		t.Fatalf("version = %q after the download", got)
	}
}

func TestIndexUnavailableWithoutAList(t *testing.T) {
	a, srv := newArchive(t, packagesFixture)
	a.status.Store(http.StatusServiceUnavailable)
	clk := clock.NewFake()
	ix := newTestIndex(srv, clk, filepath.Join(t.TempDir(), "missing.json.gz"))
	ctx := context.Background()
	if _, err := ix.Check(ctx, []string{"ffmpeg"}); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("err = %v, want ErrUnavailable", err)
	}
	// Within retryAfter a check does not try again.
	if _, err := ix.Check(ctx, []string{"ffmpeg"}); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("err = %v, want ErrUnavailable", err)
	}
	if got := a.requests.Load(); got != 1 {
		t.Fatalf("requests = %d, want 1 within retryAfter", got)
	}
	a.status.Store(http.StatusOK)
	clk.Advance(retryAfter)
	if unknown, err := ix.Check(ctx, []string{"ffmpeg"}); err != nil || len(unknown) != 0 {
		t.Fatalf("after the archive is back: unknown = %v, err = %v", unknown, err)
	}
}

func TestCheckGivesUpWithItsContext(t *testing.T) {
	_, srv := newArchive(t, packagesFixture)
	ix := newTestIndex(srv, clock.NewFake(), "")
	ix.fetch <- struct{}{} // someone else is downloading
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := ix.Check(ctx, []string{"ffmpeg"}); !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
}

func TestPackagesURL(t *testing.T) {
	if got, want := PackagesURL("arm64"), "https://deb.debian.org/debian/dists/trixie/main/binary-arm64/Packages.gz"; got != want {
		t.Fatalf("PackagesURL = %q, want %q", got, want)
	}
}

// The index must be the release the agent-shell image installs from.
func TestSuiteMatchesAgentShellImage(t *testing.T) {
	b, err := os.ReadFile("../../Dockerfile.agent-shell")
	if err != nil {
		t.Fatal(err)
	}
	var base string
	for _, line := range strings.Split(string(b), "\n") {
		if strings.HasPrefix(line, "FROM debian:") {
			base = line
		}
	}
	if !strings.HasPrefix(base, "FROM debian:"+Suite+"-") && !strings.HasPrefix(base, "FROM debian:"+Suite+"@") {
		t.Fatalf("Dockerfile.agent-shell is based on %q; debian.Suite is %q", base, Suite)
	}
}
