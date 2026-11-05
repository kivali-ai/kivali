package agentshell

// POST /v1/download: one file fetched through an integration's
// connection, into the agent's files. A built-in integration's core
// code checks the call and mints a token narrowed to it; the file then
// reaches the agent's shell without passing through core.
//
// The fetch holds the job to what core asked, whatever it is answered:
//
//   - The URL is GET with the job's token, https (http where the daemon
//     allows it for git, in tests), no user info. No redirect is
//     followed on the way.
//   - A redirect's address must be https, without user info, on a host
//     the job names, and is then fetched with no credential of the
//     job's: such an address is signed, and the token is the API's
//     alone. Only its host is ever named, in any word the daemon says.
//   - Requests go through the daemon's bare proxy URL with the job's
//     proxy credential, never a proxy from the environment.
//   - The file arrives under a temporary name in its folder, through an
//     os.Root, and takes its own name once it is whole and within the
//     job's bound: anything else leaves no file, even a daemon that
//     stops part way leaves only the temporary one.

import (
	"bufio"
	"bytes"
	"cmp"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/kivali-ai/kivali/internal/egresscred"
)

// DownloadMaxBodyBytes caps a POST /v1/download body.
const DownloadMaxBodyBytes = 64 << 10

// The bounds of one download: a request's TimeoutSeconds, at most
// DownloadMaxTimeout, DownloadDefaultTimeout when it gives none; and
// its MaxBytes, at most DownloadMaxBytes.
const (
	DownloadDefaultTimeout = 5 * time.Minute
	DownloadMaxTimeout     = 15 * time.Minute
	DownloadMaxBytes       = 5 << 30
)

// downloadTempPrefix starts the name a file has until it is whole.
const downloadTempPrefix = ".download-"

// downloadZipName is what ends the name of a file NameZip names.
const downloadZipName = ".zip"

// DownloadTimeoutFor is the bound of a download given seconds.
func DownloadTimeoutFor(seconds int) time.Duration {
	if seconds <= 0 {
		return DownloadDefaultTimeout
	}
	return min(time.Duration(seconds)*time.Second, DownloadMaxTimeout)
}

func handleDownload(cfg ServerConfig) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		r.Body = http.MaxBytesReader(w, r.Body, DownloadMaxBodyBytes)
		var req DownloadRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			// A decode error names a field or a character, never a value.
			status := http.StatusBadRequest
			var tooBig *http.MaxBytesError
			if errors.As(err, &tooBig) {
				status = http.StatusRequestEntityTooLarge
			}
			http.Error(w, "decode DownloadRequest: "+err.Error(), status)
			return
		}
		scrub := DownloadScrubber(req, cfg.GitProxyURL)
		res := runDownload(r.Context(), cfg, req)
		res.Error = clipUTF8(scrub(res.Error), GitErrorMaxBytes)
		writeJSON(w, http.StatusOK, res)
	}
}

// DownloadScrubber is what scrubs req's token and proxy credential from
// a text, as GitScrubber does a git job's.
func DownloadScrubber(req DownloadRequest, bareProxy string) func(string) string {
	return GitScrubber(GitRequest{Auth: GitAuth{Password: req.Token}, Proxy: req.Proxy}, bareProxy)
}

// runDownload runs req under its own bound: its answer carries the
// job's connection id and how long it took, whatever became of it.
func runDownload(ctx context.Context, cfg ServerConfig, req DownloadRequest) DownloadResponse {
	start := cfg.Clock.Now()
	ctx, cancel := context.WithTimeout(ctx, DownloadTimeoutFor(req.TimeoutSeconds))
	defer cancel()
	res := fetchDownload(ctx, cfg, req)
	res.TimedOut = res.Error != "" && errors.Is(ctx.Err(), context.DeadlineExceeded)
	res.ConnectionID = req.ConnectionID
	res.DurationMs = cfg.Clock.Now().Sub(start).Milliseconds()
	return res
}

// fetchDownload GETs the job's URL with its token, follows one redirect
// to a host the job names with no credential, and keeps the body in the
// job's folder.
func fetchDownload(ctx context.Context, cfg ServerConfig, req DownloadRequest) DownloadResponse {
	if cfg.GitUnavailable != "" {
		return DownloadResponse{Error: cfg.GitUnavailable}
	}
	first, err := downloadURL(cfg, req.URL)
	if err != nil {
		return DownloadResponse{Error: "the job's address does not read"}
	}
	if req.Name == "" || req.Name != path.Base(req.Name) || req.Name == "." || req.Name == ".." || strings.HasPrefix(req.Name, downloadTempPrefix) {
		return DownloadResponse{Error: "the job's file name is not a plain name"}
	}
	limit := int64(DownloadMaxBytes)
	if req.MaxBytes > 0 {
		limit = min(req.MaxBytes, limit)
	}
	dir, err := resolveGitPath(cfg, req.Folder)
	if err != nil {
		return DownloadResponse{Error: err.Error()}
	}
	tr, err := daemonTransport(cfg, req.Proxy)
	if err != nil {
		return DownloadResponse{Error: err.Error()}
	}
	defer tr.CloseIdleConnections()
	client := &http.Client{Transport: tr, CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}}

	hreq, err := http.NewRequestWithContext(ctx, http.MethodGet, first.String(), nil)
	if err != nil {
		return DownloadResponse{Error: "the job's address does not read"}
	}
	hreq.Header.Set("Authorization", "Bearer "+req.Token)
	hreq.Header.Set("User-Agent", "Kivali")
	if req.Accept != "" {
		hreq.Header.Set("Accept", req.Accept)
	}
	resp, err := client.Do(hreq)
	if err != nil {
		return DownloadResponse{Error: first.Hostname() + " did not answer"}
	}
	defer func() { _ = resp.Body.Close() }()
	body := resp.Body
	switch resp.StatusCode {
	case http.StatusOK:
	case http.StatusFound, http.StatusSeeOther, http.StatusTemporaryRedirect, http.StatusMovedPermanently, http.StatusPermanentRedirect:
		next, err := downloadRedirect(cfg, req, first, resp.Header.Get("Location"))
		if err != nil {
			return DownloadResponse{Error: err.Error()}
		}
		sreq, err := http.NewRequestWithContext(ctx, http.MethodGet, next.String(), nil)
		if err != nil {
			return DownloadResponse{Error: "the redirect's address does not read"}
		}
		sreq.Header.Set("User-Agent", "Kivali")
		sresp, err := client.Do(sreq)
		if err != nil {
			// The address is signed: only its host is named.
			return DownloadResponse{Error: next.Hostname() + " did not answer"}
		}
		defer func() { _ = sresp.Body.Close() }()
		if sresp.StatusCode != http.StatusOK {
			return DownloadResponse{Error: fmt.Sprintf("%s answered %d", next.Hostname(), sresp.StatusCode)}
		}
		body = sresp.Body
	default:
		return DownloadResponse{Status: resp.StatusCode, Error: fmt.Sprintf("%s answered %d: %s", first.Hostname(), resp.StatusCode, downloadErrorWords(resp))}
	}
	return keepDownload(ctx, req, dir, body, limit)
}

// downloadURL is raw as an address the job may GET: https (http where
// the daemon allows it for git), with a host and no user info.
func downloadURL(cfg ServerConfig, raw string) (*url.URL, error) {
	u, err := url.Parse(raw)
	if err != nil || u.User != nil || u.Host == "" || (u.Scheme != "https" && (!cfg.GitInsecureHTTP || u.Scheme != "http")) {
		return nil, errors.New("not an address the daemon uses")
	}
	return u, nil
}

// downloadRedirect is the address a redirect names, resolved against
// first, when the job may go there: an address downloadURL takes, on a
// host of the job's Hosts.
func downloadRedirect(cfg ServerConfig, req DownloadRequest, first *url.URL, loc string) (*url.URL, error) {
	u, err := first.Parse(loc)
	if err != nil || loc == "" {
		return nil, errors.New("the redirect names no address the daemon reads")
	}
	if u, err = downloadURL(cfg, u.String()); err != nil {
		return nil, errors.New("the redirect names no address the daemon reads")
	}
	h := strings.ToLower(u.Hostname())
	if !egresscred.HostMatches(h, req.Hosts) {
		return nil, fmt.Errorf("the file is kept on %s, which is not a host this connection's downloads go to; tell the owner", clipUTF8(h, 200))
	}
	return u, nil
}

// downloadErrorWords is the message of a JSON error answer, or the
// status's own words.
func downloadErrorWords(resp *http.Response) string {
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	var e struct {
		Message string `json:"message"`
	}
	_ = json.Unmarshal(raw, &e)
	return clipUTF8(cmp.Or(strings.TrimSpace(e.Message), http.StatusText(resp.StatusCode)), 300)
}

// keepDownload writes body, at most limit bytes, under a temporary name
// in dir, and gives it its own name once it is whole.
func keepDownload(ctx context.Context, req DownloadRequest, dir string, body io.Reader, limit int64) DownloadResponse {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return DownloadResponse{Error: req.Folder + " does not take the file"}
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		return DownloadResponse{Error: req.Folder + " does not take the file"}
	}
	defer func() { _ = root.Close() }()
	var suffix [8]byte
	_, _ = rand.Read(suffix[:])
	tmp := downloadTempPrefix + hex.EncodeToString(suffix[:])
	f, err := root.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		return DownloadResponse{Error: req.Folder + " does not take the file"}
	}
	br := bufio.NewReader(body)
	zipped := startsAsZip(br)
	n, err := io.Copy(f, io.LimitReader(br, limit+1))
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	switch {
	case err != nil && ctx.Err() != nil:
		_ = root.Remove(tmp)
		return DownloadResponse{Error: "it did not finish in time"}
	case errors.Is(err, syscall.ENOSPC):
		_ = root.Remove(tmp)
		return DownloadResponse{Error: "the disk is full"}
	case err != nil:
		_ = root.Remove(tmp)
		return DownloadResponse{Error: "it stopped arriving part way"}
	case n > limit:
		_ = root.Remove(tmp)
		return DownloadResponse{Error: fmt.Sprintf("it is larger than the %d MiB the job may save", limit>>20), TooLarge: true}
	}
	name := req.Name
	if req.NameZip && zipped && !strings.HasSuffix(strings.ToLower(name), downloadZipName) {
		name += downloadZipName
	}
	if err := root.Rename(tmp, name); err != nil {
		_ = root.Remove(tmp)
		return DownloadResponse{Error: req.Folder + " does not take the file"}
	}
	return DownloadResponse{Path: filepath.Join(req.Folder, name), Bytes: n, Zip: zipped}
}

// The first four bytes of a zip with entries, and of an empty one.
var (
	zipEntry = []byte("PK\x03\x04")
	zipEmpty = []byte("PK\x05\x06")
)

// startsAsZip reports whether what br will read starts as a zip,
// consuming none of it.
func startsAsZip(br *bufio.Reader) bool {
	head, _ := br.Peek(len(zipEntry))
	return bytes.Equal(head, zipEntry) || bytes.Equal(head, zipEmpty)
}
