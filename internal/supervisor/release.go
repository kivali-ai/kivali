package supervisor

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"
)

// Release is release.json, the update feed (docs/developers/supervisor.md).
type Release struct {
	// Version is the Kivali release, e.g. 0.16.0 (a leading v is
	// tolerated).
	Version string `json:"version"`
	// Chart is the packaged chart.
	Chart Asset `json:"chart"`
	// Images maps a platform (linux/arm64, linux/amd64) to the image
	// bundle: a tar (optionally .gz or .zst) of the kivali,
	// kivali-egress-proxy and kivali-dev-shell images at one tag.
	Images map[string]Asset `json:"images"`
	// MinDesktopVersion is the oldest Kivali Desktop that can run it.
	MinDesktopVersion string `json:"min_desktop_version"`
	// VMImage is the VM image version the release was tested with.
	VMImage string `json:"vm_image"`
	// ManualSteps is set when the upgrade needs steps the app cannot do.
	ManualSteps bool   `json:"manual_steps"`
	NotesURL    string `json:"notes_url,omitempty"`
}

// Asset is one release file. Name resolves against the release.json
// URL unless URL is set.
type Asset struct {
	Name   string `json:"name"`
	URL    string `json:"url,omitempty"`
	SHA256 string `json:"sha256"`
	Size   int64  `json:"size,omitempty"`
}

// Check outcomes.
const (
	CheckUpToDate     = "up-to-date"
	CheckUpgrade      = "upgrade-available"
	CheckAppTooOld    = "desktop-update-required"
	CheckManualSteps  = "manual-steps"
	CheckFailed       = "failed"
	CheckNotInstalled = "not-installed"
)

// CheckResult is the outcome of a feed check.
type CheckResult struct {
	Status  string `json:"status"`
	Current string `json:"current"`
	Latest  string `json:"latest,omitempty"`
	// MinDesktopVersion is the release's minimum Kivali Desktop version,
	// passed through on every successful check so the shell can compare
	// it with its own version.
	MinDesktopVersion string    `json:"min_desktop_version,omitempty"`
	Message           string    `json:"message"`
	NotesURL          string    `json:"notes_url,omitempty"`
	CheckedAt         time.Time `json:"checked_at"`
	Feed              string    `json:"feed"`
}

// feedTimeout bounds the release.json fetch (docs/developers/desktop-app.md).
const feedTimeout = 10 * time.Second

// isLocal reports whether ref is a file path or file:// URL, and the
// path.
func isLocal(ref string) (string, bool) {
	if strings.HasPrefix(ref, "file://") {
		u, err := url.Parse(ref)
		if err != nil {
			return "", false
		}
		return filepath.FromSlash(fileURLPath(u, runtime.GOOS)), true
	}
	if !strings.Contains(ref, "://") {
		return ref, true
	}
	return "", false
}

// fileURLPath is a file URL's path with forward slashes. On Windows
// file:///C:/x is C:/x (the URL path's leading slash dropped) and
// file://server/share/x is //server/share/x (a UNC path).
func fileURLPath(u *url.URL, goos string) string {
	if goos != "windows" {
		return u.Path
	}
	if u.Host != "" && u.Host != "localhost" {
		return "//" + u.Host + u.Path
	}
	p := u.Path
	if len(p) >= 3 && p[0] == '/' && p[2] == ':' {
		return p[1:]
	}
	return p
}

// open opens a feed URL or local path.
func (s *Supervisor) open(ctx context.Context, ref string) (io.ReadCloser, error) {
	if p, ok := isLocal(ref); ok {
		return os.Open(p)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, ref, nil)
	if err != nil {
		return nil, err
	}
	resp, err := s.o.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		_ = resp.Body.Close()
		return nil, fmt.Errorf("GET %s: %s", ref, resp.Status)
	}
	return resp.Body, nil
}

// fetchRelease reads release.json within feedTimeout.
func (s *Supervisor) fetchRelease(ctx context.Context, feed string) (Release, error) {
	ctx, cancel := context.WithTimeout(ctx, feedTimeout)
	defer cancel()
	rc, err := s.open(ctx, feed)
	if err != nil {
		return Release{}, err
	}
	defer func() { _ = rc.Close() }()
	var r Release
	if err := json.NewDecoder(io.LimitReader(rc, 1<<20)).Decode(&r); err != nil {
		return Release{}, fmt.Errorf("release.json: %w", err)
	}
	if r.Version == "" {
		return Release{}, errors.New("release.json: no version")
	}
	return r, nil
}

// validAssetName reports whether name is a plain file name on every OS:
// no separator of either kind, no parent reference.
func validAssetName(name string) bool {
	return name != "" && name != "." && name != ".." && !strings.ContainsAny(name, `/\`)
}

// assetRef resolves an asset against the feed's location.
func assetRef(feed string, a Asset) (string, error) {
	if a.URL != "" {
		return a.URL, nil
	}
	if !validAssetName(a.Name) {
		return "", fmt.Errorf("asset name %q", a.Name)
	}
	if p, ok := isLocal(feed); ok {
		return filepath.Join(filepath.Dir(p), a.Name), nil
	}
	u, err := url.Parse(feed)
	if err != nil {
		return "", err
	}
	return u.ResolveReference(&url.URL{Path: a.Name}).String(), nil
}

// download fetches an asset into dir, verifying its digest; the file
// appears under its name only once verified.
func (s *Supervisor) download(ctx context.Context, feed string, a Asset, dir string) (string, error) {
	if len(a.SHA256) != 64 {
		return "", fmt.Errorf("asset %s has no sha256", a.Name)
	}
	ref, err := assetRef(feed, a)
	if err != nil {
		return "", err
	}
	name := a.Name
	if name == "" {
		name = filepath.Base(ref)
	}
	if !validAssetName(name) {
		return "", fmt.Errorf("asset name %q", name)
	}
	dst := filepath.Join(dir, name)
	rc, err := s.open(ctx, ref)
	if err != nil {
		return "", err
	}
	defer func() { _ = rc.Close() }()
	tmp, err := os.CreateTemp(dir, ".dl-*")
	if err != nil {
		return "", err
	}
	defer func() { _ = os.Remove(tmp.Name()) }()
	h := sha256.New()
	if _, err := io.Copy(io.MultiWriter(tmp, h), rc); err != nil {
		_ = tmp.Close()
		return "", fmt.Errorf("download %s: %w", ref, err)
	}
	if err := tmp.Close(); err != nil {
		return "", err
	}
	if got := hex.EncodeToString(h.Sum(nil)); !strings.EqualFold(got, a.SHA256) {
		return "", fmt.Errorf("%s: sha256 %s, release.json says %s", name, got, a.SHA256)
	}
	if err := os.Rename(tmp.Name(), dst); err != nil {
		return "", err
	}
	return dst, nil
}

// semver parses MAJOR.MINOR.PATCH with an optional leading v and
// ignores any pre-release or build suffix.
func semver(v string) ([3]int, bool) {
	var out [3]int
	v = strings.TrimPrefix(strings.TrimSpace(v), "v")
	if i := strings.IndexAny(v, "-+"); i >= 0 {
		v = v[:i]
	}
	parts := strings.Split(v, ".")
	if len(parts) != 3 {
		return out, false
	}
	for i, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil {
			return out, false
		}
		out[i] = n
	}
	return out, true
}

// compareVersions returns -1, 0 or 1; unparsable versions compare as
// equal.
func compareVersions(a, b string) int {
	x, ok1 := semver(a)
	y, ok2 := semver(b)
	if !ok1 || !ok2 {
		return 0
	}
	for i := range x {
		switch {
		case x[i] < y[i]:
			return -1
		case x[i] > y[i]:
			return 1
		}
	}
	return 0
}

// desktopTooOld reports whether this supervisor is older than min. A
// development build ("dev", or anything unparsable) is never too old.
func desktopTooOld(version, min string) bool {
	if min == "" {
		return false
	}
	if _, ok := semver(version); !ok {
		return false
	}
	return compareVersions(version, min) < 0
}

// evaluate turns a release into a check result for the installed
// version.
func (s *Supervisor) evaluate(r Release, current string) CheckResult {
	res := CheckResult{Current: current, Latest: strings.TrimPrefix(r.Version, "v"), NotesURL: r.NotesURL, MinDesktopVersion: r.MinDesktopVersion}
	switch {
	case current == "":
		res.Status = CheckNotInstalled
		res.Message = fmt.Sprintf("Kivali is not installed; the latest release is %s", res.Latest)
	case compareVersions(r.Version, current) <= 0:
		res.Status = CheckUpToDate
		res.Message = fmt.Sprintf("Kivali %s is up to date", current)
	case desktopTooOld(s.o.Version, r.MinDesktopVersion):
		res.Status = CheckAppTooOld
		res.Message = fmt.Sprintf("Kivali %s needs Kivali Desktop %s or later (this is %s): update Kivali Desktop first", res.Latest, r.MinDesktopVersion, s.o.Version)
	case r.ManualSteps:
		res.Status = CheckManualSteps
		res.Message = fmt.Sprintf("Kivali %s needs steps the app can't do yet; see the upgrade notes", res.Latest)
	default:
		res.Status = CheckUpgrade
		res.Message = fmt.Sprintf("Upgrade to Kivali %s available", res.Latest)
	}
	return res
}

// Check fetches release.json (feed, or local.json's feed) and records
// the outcome. A failed check is recorded too, and never fails anything
// else.
func (s *Supervisor) Check(ctx context.Context, feed string, logf Logf) (CheckResult, error) {
	logf = s.tee(ctx, logf)
	if feed == "" {
		feed = s.State().Feed
	}
	now := s.o.Clock.Now().UTC()
	r, err := s.fetchRelease(ctx, feed)
	if err != nil {
		res := CheckResult{Status: CheckFailed, Current: s.State().Kivali, Feed: feed, CheckedAt: now,
			Message: fmt.Sprintf("Couldn't check for updates: %v", err)}
		if uerr := s.update(func(st *State) { st.LastCheck = &now; st.LastCheckOK = false }); uerr != nil {
			return res, uerr
		}
		logf("%s", res.Message)
		return res, nil
	}
	res := s.evaluate(r, s.State().Kivali)
	res.CheckedAt, res.Feed = now, feed
	if err := s.update(func(st *State) { st.LastCheck = &now; st.LastCheckOK = true; st.Latest = &res }); err != nil {
		return res, err
	}
	logf("%s", res.Message)
	return res, nil
}
