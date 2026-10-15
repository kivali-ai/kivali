package debian

import (
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
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
Provides: mail-transport-agent

Package: python3
Package-Type: deb

Package: python3-numpy

Package: python3-pandas

Package: vim
`

// archive serves a gzipped Packages file with an ETag, answering 304
// to a matching If-None-Match, and counts full downloads.
type archive struct {
	body      []byte
	etag      string
	status    int
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
	a := &archive{body: buf.Bytes(), etag: `"v1"`, status: http.StatusOK}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		a.requests.Add(1)
		if a.status != http.StatusOK {
			w.WriteHeader(a.status)
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

func TestCheckKnowsPackagesAndNothingElse(t *testing.T) {
	_, srv := newArchive(t, packagesFixture)
	ix := NewIndex(srv.URL, srv.Client(), clock.NewFake())
	unknown, err := ix.Check(context.Background(), []string{"ffmpeg", "poppler-utils", "postgresql-client", "not-a-package", "version"})
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, u := range unknown {
		got = append(got, u.Name)
	}
	// A continuation line that looks like a field is not a package.
	if want := []string{"not-a-package", "version"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("unknown = %v, want %v", got, want)
	}
}

func TestCheckNamesWhatProvidesAVirtualPackage(t *testing.T) {
	_, srv := newArchive(t, packagesFixture)
	ix := NewIndex(srv.URL, srv.Client(), clock.NewFake())
	unknown, err := ix.Check(context.Background(), []string{"awk", "mail-transport-agent"})
	if err != nil {
		t.Fatal(err)
	}
	if len(unknown) != 2 {
		t.Fatalf("unknown = %+v, want awk and mail-transport-agent", unknown)
	}
	if want := []string{"gawk", "mawk"}; !reflect.DeepEqual(unknown[0].ProvidedBy, want) {
		t.Errorf("awk provided by %v, want %v", unknown[0].ProvidedBy, want)
	}
	if want := []string{"exim4-daemon-light"}; !reflect.DeepEqual(unknown[1].ProvidedBy, want) {
		t.Errorf("mail-transport-agent provided by %v, want %v", unknown[1].ProvidedBy, want)
	}
	if got := unknown[0].Describe(); got != "awk is not a package itself in Debian 13; it is provided by gawk or mawk, so name one of those." {
		t.Errorf("Describe = %q", got)
	}
}

func TestCheckSuggestsCloseNames(t *testing.T) {
	_, srv := newArchive(t, packagesFixture)
	ix := NewIndex(srv.URL, srv.Client(), clock.NewFake())
	cases := []struct {
		name string
		want []string
	}{
		{"ffmpg", []string{"ffmpeg"}},
		{"popler-utils", []string{"poppler-utils"}},
		{"postgres-client", []string{"postgresql-client"}},
		// A prefix of longer names, shortest first.
		{"python3-numpi", []string{"python3-numpy"}},
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
	_, srv := newArchive(t, "Package: poppler-utils\n\nPackage: poppler-data\n\nPackage: poppler-utils-extra\n")
	ix := NewIndex(srv.URL, srv.Client(), clock.NewFake())
	unknown, err := ix.Check(context.Background(), []string{"poppler"})
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"poppler-data", "poppler-utils", "poppler-utils-extra"}; !reflect.DeepEqual(unknown[0].Suggestions, want) {
		t.Fatalf("suggestions %v, want %v", unknown[0].Suggestions, want)
	}
	if got := unknown[0].Describe(); got != "Debian 13 has no package called poppler. Did you mean poppler-data, poppler-utils or poppler-utils-extra?" {
		t.Errorf("Describe = %q", got)
	}
}

func TestIndexDownloadsOnceADayAndRevalidates(t *testing.T) {
	a, srv := newArchive(t, packagesFixture)
	clk := clock.NewFake()
	ix := NewIndex(srv.URL, srv.Client(), clk)
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
}

func TestIndexUnavailableWithoutADownload(t *testing.T) {
	a, srv := newArchive(t, packagesFixture)
	a.status = http.StatusServiceUnavailable
	clk := clock.NewFake()
	ix := NewIndex(srv.URL, srv.Client(), clk)
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
	a.status = http.StatusOK
	clk.Advance(retryAfter)
	if unknown, err := ix.Check(ctx, []string{"ffmpeg"}); err != nil || len(unknown) != 0 {
		t.Fatalf("after the archive is back: unknown = %v, err = %v", unknown, err)
	}
}

func TestIndexKeepsAStaleListWhenRefreshFails(t *testing.T) {
	a, srv := newArchive(t, packagesFixture)
	clk := clock.NewFake()
	ix := NewIndex(srv.URL, srv.Client(), clk)
	ctx := context.Background()
	ix.Warm(ctx)
	a.status = http.StatusBadGateway
	clk.Advance(maxAge + time.Hour)
	unknown, err := ix.Check(ctx, []string{"ffmpeg", "ffmpg"})
	if err != nil {
		t.Fatalf("stale index not used: %v", err)
	}
	if len(unknown) != 1 || unknown[0].Name != "ffmpg" {
		t.Fatalf("unknown = %+v, want ffmpg", unknown)
	}
}

func TestCheckGivesUpWithItsContext(t *testing.T) {
	_, srv := newArchive(t, packagesFixture)
	ix := NewIndex(srv.URL, srv.Client(), clock.NewFake())
	ix.sem <- struct{}{} // someone else is downloading
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
