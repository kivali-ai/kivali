package agentshell

// Git LFS for a job with LFS set: the daemon moves the LFS files its
// commits name itself, through the repository's LFS batch API, with no
// git-lfs program, no hook and nothing the working copy configures.
//
//   - The API is the job's own checked URL plus /info/lfs, never
//     lfs.url, .lfsconfig or anything else the working copy holds, so
//     the token goes only where the repository's git traffic goes.
//   - A file's content goes to, or comes from, the address the API names
//     for it, which must be https (http only where the daemon allows it
//     for git, in tests), carry no user info and be on the repository's
//     host or one of the job's LFS hosts. It is sent with the headers
//     the API names for it and no credential of the job's.
//   - Requests go through the daemon's bare proxy URL with the job's
//     proxy credential, never a proxy from the environment, and follow
//     no redirect.
//   - A push uploads, before any ref moves, the files the host lacks of
//     those the pushed tips' trees and the pushed commits name; a file
//     the host lacks and the working copy does not hold, or holds with
//     other content than its pointer names, stops the push with nothing
//     moved. A clone downloads the files HEAD's tree names, and a fetch
//     those the tips of the refs its refspecs write name, into
//     .git/lfs/objects; a file that does not arrive whole and matching
//     its pointer is not kept. Neither touches the working tree: the
//     agent's git-lfs puts files there (git lfs checkout).
//   - Files are read and written through an os.Root at the working
//     copy's .git, so no link leads them out of it.
//   - An operation moves at most lfsMaxObjects files and lfsMaxBytes,
//     and a push walks at most lfsMaxCommits commits for pointers.

import (
	"bytes"
	"cmp"
	"context"
	"crypto/rand"
	"crypto/sha256"
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
	"strconv"
	"strings"
	"time"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/filemode"
	"github.com/go-git/go-git/v5/plumbing/object"

	"github.com/kivali-ai/kivali/internal/egresscred"
	"github.com/kivali-ai/kivali/internal/gitrepo"
)

// The bounds of an operation's LFS work.
const (
	// lfsPointerMaxBytes: a pointer file is smaller than this, as the
	// LFS spec has it.
	lfsPointerMaxBytes = 1024
	// lfsMaxObjects is the most LFS files one operation names.
	lfsMaxObjects = 1000
	// lfsMaxBytes is the most bytes of LFS files one operation moves.
	lfsMaxBytes int64 = 5 << 30
	// lfsMaxCommits is the most commits a push walks for pointers,
	// beyond its tips' trees.
	lfsMaxCommits = 1000
	// lfsBatchSize is the most files one batch request names.
	lfsBatchSize = 100
	// lfsMaxAnswerBytes bounds a batch answer.
	lfsMaxAnswerBytes = 4 << 20
	// lfsMediaType is the batch API's.
	lfsMediaType = "application/vnd.git-lfs+json"
	// lfsVersion is a pointer's first line.
	lfsVersion = "version https://git-lfs.github.com/spec/v1"
)

// lfsObject is one LFS file a pointer names: its sha256 in hex, its
// size, and the first path it was seen at, for the words.
type lfsObject struct {
	oid  string
	size int64
	path string
}

// parseLFSPointer reads b as an LFS pointer file: the version line
// first, then "key value" lines, among them oid sha256:<hex> and size
// <bytes>, each line ending in a newline, the whole smaller than
// lfsPointerMaxBytes. Anything else is not a pointer.
func parseLFSPointer(b []byte) (oid string, size int64, ok bool) {
	if len(b) == 0 || len(b) >= lfsPointerMaxBytes || b[len(b)-1] != '\n' {
		return "", 0, false
	}
	lines := strings.Split(string(b[:len(b)-1]), "\n")
	if lines[0] != lfsVersion {
		return "", 0, false
	}
	size = -1
	for _, l := range lines[1:] {
		k, v, found := strings.Cut(l, " ")
		if !found {
			return "", 0, false
		}
		switch k {
		case "oid":
			h, found := strings.CutPrefix(v, "sha256:")
			if !found || !isSHA256Hex(h) {
				return "", 0, false
			}
			oid = h
		case "size":
			n, err := strconv.ParseInt(v, 10, 64)
			if err != nil || n < 0 || strconv.FormatInt(n, 10) != v {
				return "", 0, false
			}
			size = n
		}
	}
	return oid, size, oid != "" && size >= 0
}

func isSHA256Hex(s string) bool {
	if len(s) != 64 {
		return false
	}
	for _, c := range s {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

// errLFSTooMany is an operation naming more than lfsMaxObjects files.
var errLFSTooMany = fmt.Errorf("the commits name more than %d LFS files, more than one operation moves", lfsMaxObjects)

// lfsScan gathers the LFS pointers of trees and commits, each blob read
// once and each file named once.
type lfsScan struct {
	r     *git.Repository
	blobs map[plumbing.Hash]bool
	oids  map[string]bool
	found []lfsObject
}

func newLFSScan(r *git.Repository) *lfsScan {
	return &lfsScan{r: r, blobs: map[plumbing.Hash]bool{}, oids: map[string]bool{}}
}

// blob reads the blob h, at path, when it is small enough to be a
// pointer. A blob the working copy does not hold is passed over.
func (s *lfsScan) blob(h plumbing.Hash, at string) error {
	if s.blobs[h] {
		return nil
	}
	s.blobs[h] = true
	obj, err := s.r.Storer.EncodedObject(plumbing.BlobObject, h)
	switch {
	case errors.Is(err, plumbing.ErrObjectNotFound):
		return nil
	case err != nil:
		return fmt.Errorf("blob %s does not read: %v", h, err)
	case obj.Size() >= lfsPointerMaxBytes:
		return nil
	}
	rd, err := obj.Reader()
	if err != nil {
		return fmt.Errorf("blob %s does not read: %v", h, err)
	}
	content, err := io.ReadAll(io.LimitReader(rd, lfsPointerMaxBytes))
	_ = rd.Close()
	if err != nil {
		return fmt.Errorf("blob %s does not read: %v", h, err)
	}
	oid, size, ok := parseLFSPointer(content)
	if !ok || s.oids[oid] {
		return nil
	}
	if len(s.found) == lfsMaxObjects {
		return errLFSTooMany
	}
	s.oids[oid] = true
	s.found = append(s.found, lfsObject{oid: oid, size: size, path: at})
	return nil
}

// isFile is a tree entry git checks out as a file's content: a regular
// or executable file, not a link or a submodule.
func isFile(m filemode.FileMode) bool {
	return m == filemode.Regular || m == filemode.Executable || m == filemode.Deprecated
}

// tree reads every file of t.
func (s *lfsScan) tree(ctx context.Context, t *object.Tree) error {
	w := object.NewTreeWalker(t, true, nil)
	defer w.Close()
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		name, entry, err := w.Next()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("a tree does not read: %v", err)
		}
		if isFile(entry.Mode) {
			if err := s.blob(entry.Hash, name); err != nil {
				return err
			}
		}
	}
}

// commit reads the files c adds or changes against its first parent;
// every file, for a commit whose parent the working copy does not hold.
func (s *lfsScan) commit(ctx context.Context, c *object.Commit) error {
	tree, err := c.Tree()
	if err != nil {
		return fmt.Errorf("commit %s's tree does not read: %v", c.Hash, err)
	}
	var parent *object.Tree
	if c.NumParents() > 0 {
		if p, err := c.Parent(0); err == nil {
			parent, _ = p.Tree()
		}
	}
	if parent == nil {
		return s.tree(ctx, tree)
	}
	changes, err := object.DiffTreeWithOptions(ctx, parent, tree, nil)
	if err != nil {
		return fmt.Errorf("commit %s does not compare with its parent: %v", c.Hash, err)
	}
	for _, ch := range changes {
		if ch.To.Name == "" || !isFile(ch.To.TreeEntry.Mode) {
			continue
		}
		if err := s.blob(ch.To.TreeEntry.Hash, ch.To.Name); err != nil {
			return err
		}
	}
	return nil
}

// commitAt is the commit h names, through an annotated tag.
func commitAt(r *git.Repository, h plumbing.Hash) (*object.Commit, error) {
	if c, err := r.CommitObject(h); err == nil {
		return c, nil
	}
	t, err := r.TagObject(h)
	if err != nil {
		return nil, err
	}
	return t.Commit()
}

// lfsPushObjects is every LFS file a push of specs names: those of each
// pushed tip's tree, and those the pushed commits add or change, walked
// from each tip back to a commit the remote's refs (before) name, at
// most lfsMaxCommits commits. A tip that is not a commit names none.
func lfsPushObjects(ctx context.Context, r *git.Repository, specs []pushSpec, before map[string]plumbing.Hash) ([]lfsObject, error) {
	known := map[plumbing.Hash]bool{}
	for _, h := range before {
		known[h] = true
	}
	s := newLFSScan(r)
	walked := 0
	seen := map[plumbing.Hash]bool{}
	for _, sp := range specs {
		tip, err := commitAt(r, sp.new)
		if err != nil {
			continue
		}
		tree, err := tip.Tree()
		if err != nil {
			return nil, fmt.Errorf("commit %s's tree does not read: %v", tip.Hash, err)
		}
		if err := s.tree(ctx, tree); err != nil {
			return nil, err
		}
		queue := []*object.Commit{tip}
		for len(queue) > 0 && walked < lfsMaxCommits {
			c := queue[0]
			queue = queue[1:]
			if known[c.Hash] || seen[c.Hash] {
				continue
			}
			seen[c.Hash] = true
			walked++
			if err := s.commit(ctx, c); err != nil {
				return nil, err
			}
			for _, ph := range c.ParentHashes {
				if known[ph] || seen[ph] {
					continue
				}
				if p, err := r.CommitObject(ph); err == nil {
					queue = append(queue, p)
				}
			}
		}
	}
	return s.found, nil
}

// lfsTipObjects is every LFS file the trees of tips name.
func lfsTipObjects(ctx context.Context, r *git.Repository, tips []plumbing.Hash) ([]lfsObject, error) {
	s := newLFSScan(r)
	for _, h := range tips {
		c, err := commitAt(r, h)
		if err != nil {
			continue
		}
		tree, err := c.Tree()
		if err != nil {
			return nil, fmt.Errorf("commit %s's tree does not read: %v", c.Hash, err)
		}
		if err := s.tree(ctx, tree); err != nil {
			return nil, err
		}
	}
	return s.found, nil
}

// lfsClient is the repository's LFS batch API and the transfers it
// names, for one job.
type lfsClient struct {
	http     *http.Client
	batchURL string
	auth     GitAuth
	// gitHost and hosts are where a transfer may go: the repository's
	// host and the job's LFS hosts, lower case, without a port.
	gitHost  string
	hosts    map[string]bool
	insecure bool
}

// errLFSRedirect is a redirect, which no LFS request follows.
var errLFSRedirect = errors.New("redirected")

// newLFSClient is the LFS client of repo for req: its batch API at
// repo's URL plus /info/lfs/objects/batch, through the daemon's proxy
// with req's proxy credential.
func newLFSClient(cfg ServerConfig, req GitRequest, repo gitrepo.Repository) (*lfsClient, error) {
	tr, err := daemonTransport(cfg, req.Proxy)
	if err != nil {
		return nil, err
	}
	base, err := url.Parse(repo.URL())
	if err != nil {
		return nil, errors.New("the repository's URL does not read")
	}
	c := &lfsClient{
		http: &http.Client{Transport: tr, CheckRedirect: func(*http.Request, []*http.Request) error {
			return errLFSRedirect
		}},
		batchURL: repo.URL() + "/info/lfs/objects/batch",
		auth:     req.Auth,
		gitHost:  strings.ToLower(base.Hostname()),
		hosts:    map[string]bool{},
		insecure: cfg.GitInsecureHTTP,
	}
	if req.LFS != nil {
		for _, h := range req.LFS.Hosts {
			c.hosts[strings.ToLower(strings.TrimSpace(h))] = true
		}
	}
	return c, nil
}

func (c *lfsClient) close() { c.http.CloseIdleConnections() }

// daemonTransport is what a job's HTTP requests go by: the daemon's
// bare proxy URL with the job's proxy credential, never a proxy from
// the environment.
func daemonTransport(cfg ServerConfig, proxy GitProxy) (*http.Transport, error) {
	tr := &http.Transport{ForceAttemptHTTP2: true, TLSHandshakeTimeout: 30 * time.Second, ResponseHeaderTimeout: 2 * time.Minute}
	if cfg.GitProxyURL == "" {
		return tr, nil
	}
	raw := cfg.GitProxyURL
	if proxy.Password != "" {
		var err error
		if raw, err = egresscred.ProxyURL(cfg.GitProxyURL, proxy.Username, proxy.Password); err != nil {
			return nil, errors.New("the daemon's proxy URL does not read")
		}
	}
	u, err := url.Parse(raw)
	if err != nil {
		return nil, errors.New("the daemon's proxy URL does not read")
	}
	tr.Proxy = http.ProxyURL(u)
	return tr, nil
}

// lfsAction is a transfer the batch API names for a file.
type lfsAction struct {
	Href   string            `json:"href"`
	Header map[string]string `json:"header"`
}

// lfsResult is one file of a batch answer.
type lfsResult struct {
	OID     string               `json:"oid"`
	Size    int64                `json:"size"`
	Actions map[string]lfsAction `json:"actions"`
	Error   *struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

// batch asks the API for op (upload or download) of objs, with the
// job's token.
func (c *lfsClient) batch(ctx context.Context, op string, objs []lfsObject) ([]lfsResult, error) {
	type item struct {
		OID  string `json:"oid"`
		Size int64  `json:"size"`
	}
	items := make([]item, 0, len(objs))
	for _, o := range objs {
		items = append(items, item{o.oid, o.size})
	}
	body, err := json.Marshal(map[string]any{"operation": op, "transfers": []string{"basic"}, "objects": items, "hash_algo": "sha256"})
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.batchURL, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", lfsMediaType)
	req.Header.Set("Content-Type", lfsMediaType)
	if c.auth.Password != "" {
		req.SetBasicAuth(cmp.Or(c.auth.Username, "x-access-token"), c.auth.Password)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		if errors.Is(err, errLFSRedirect) {
			return nil, errors.New(gitMovedError)
		}
		return nil, errors.New("the repository's LFS API did not answer")
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, lfsMaxAnswerBytes+1))
	if err != nil {
		return nil, errors.New("the repository's LFS API's answer did not arrive")
	}
	if resp.StatusCode != http.StatusOK {
		var e struct {
			Message string `json:"message"`
		}
		_ = json.Unmarshal(raw, &e)
		return nil, fmt.Errorf("the repository's LFS API answered %d: %s", resp.StatusCode, clipUTF8(cmp.Or(strings.TrimSpace(e.Message), http.StatusText(resp.StatusCode)), 300))
	}
	if len(raw) > lfsMaxAnswerBytes {
		return nil, errors.New("the repository's LFS API's answer is larger than the daemon reads")
	}
	var out struct {
		Transfer string      `json:"transfer"`
		Objects  []lfsResult `json:"objects"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, errors.New("the repository's LFS API's answer is not JSON the daemon reads")
	}
	if out.Transfer != "" && out.Transfer != "basic" {
		return nil, fmt.Errorf("the repository's LFS API chose the transfer %q; the daemon makes basic transfers alone", clipUTF8(out.Transfer, 40))
	}
	return out.Objects, nil
}

// transferURL is the address an action names, when a transfer may go
// there: https (http where the daemon allows it for git), no user info,
// on the repository's host or one of the job's LFS hosts.
func (c *lfsClient) transferURL(raw string) (*url.URL, error) {
	u, err := url.Parse(raw)
	if err != nil || u.User != nil || u.Host == "" || (u.Scheme != "https" && (!c.insecure || u.Scheme != "http")) {
		return nil, errors.New("the repository's LFS API named a transfer address the daemon does not use")
	}
	h := strings.ToLower(u.Hostname())
	if h != c.gitHost && !c.hosts[h] {
		return nil, fmt.Errorf("the repository's LFS API keeps files on %s, which is not a host this connection's LFS files go to; tell the owner", clipUTF8(h, 200))
	}
	return u, nil
}

// lfsHeaderRefused are the headers an action may not set: the
// connection's and the transport's own.
var lfsHeaderRefused = map[string]bool{
	"Host": true, "Content-Length": true, "Transfer-Encoding": true, "Connection": true, "Upgrade": true,
	"Proxy-Authorization": true, "Proxy-Connection": true, "Te": true, "Trailer": true, "Keep-Alive": true,
}

// transfer sends one action's request, its body body of length n (-1
// for none), with the action's headers and no credential of the job's.
// The answer must be a success; its body is the caller's to read and
// close.
func (c *lfsClient) transfer(ctx context.Context, method string, a lfsAction, body io.Reader, n int64, contentType string) (*http.Response, error) {
	u, err := c.transferURL(a.Href)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, method, u.String(), body)
	if err != nil {
		return nil, errors.New("the repository's LFS API named a transfer address the daemon does not use")
	}
	if n >= 0 {
		req.ContentLength = n
	}
	for k, v := range a.Header {
		if lfsHeaderRefused[http.CanonicalHeaderKey(k)] {
			continue
		}
		req.Header.Set(k, v)
	}
	if contentType != "" && req.Header.Get("Content-Type") == "" {
		req.Header.Set("Content-Type", contentType)
	}
	host := u.Hostname()
	resp, err := c.http.Do(req)
	if err != nil {
		// The address is signed: only its host is named.
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, fmt.Errorf("%s did not answer", host)
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		_ = resp.Body.Close()
		return nil, fmt.Errorf("%s answered %d", host, resp.StatusCode)
	}
	return resp, nil
}

// lfsStore is the working copy's .git, where LFS files are kept under
// lfs/objects, read and written so no link leads out of it.
type lfsStore struct{ root *os.Root }

func openLFSStore(dir string) (*lfsStore, error) {
	r, err := os.OpenRoot(filepath.Join(dir, git.GitDirName))
	if err != nil {
		return nil, errors.New("the working copy's .git does not open")
	}
	return &lfsStore{root: r}, nil
}

func (s *lfsStore) close() { _ = s.root.Close() }

func lfsPath(oid string) string { return path.Join("lfs", "objects", oid[:2], oid[2:4], oid) }

// errLFSMissing and errLFSMismatch are a file the working copy does not
// hold, and one it holds with other content than its pointer names.
var (
	errLFSMissing  = errors.New("missing")
	errLFSMismatch = errors.New("mismatch")
)

// open is o's file, checked: a regular file of o's size whose sha256 is
// o's, read from its start.
func (s *lfsStore) open(o lfsObject) (*os.File, error) {
	f, err := s.root.Open(lfsPath(o.oid))
	if err != nil {
		return nil, errLFSMissing
	}
	fi, err := f.Stat()
	if err != nil || !fi.Mode().IsRegular() || fi.Size() != o.size {
		_ = f.Close()
		return nil, errLFSMismatch
	}
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil || hex.EncodeToString(h.Sum(nil)) != o.oid {
		_ = f.Close()
		return nil, errLFSMismatch
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		_ = f.Close()
		return nil, errLFSMismatch
	}
	return f, nil
}

// has reports whether the working copy holds a file of o's size where
// o's goes.
func (s *lfsStore) has(o lfsObject) bool {
	fi, err := s.root.Stat(lfsPath(o.oid))
	return err == nil && fi.Mode().IsRegular() && fi.Size() == o.size
}

// write keeps r's content as o's file once all of it has arrived, of
// o's size and sha256; anything else leaves no file.
func (s *lfsStore) write(o lfsObject, r io.Reader) error {
	final := lfsPath(o.oid)
	if err := s.root.MkdirAll(path.Dir(final), 0o755); err != nil {
		return errors.New("lfs/objects does not take the file")
	}
	if err := s.root.MkdirAll(path.Join("lfs", "tmp"), 0o755); err != nil {
		return errors.New("lfs/tmp does not take the file")
	}
	var suffix [8]byte
	_, _ = rand.Read(suffix[:])
	tmp := path.Join("lfs", "tmp", o.oid+"-"+hex.EncodeToString(suffix[:]))
	f, err := s.root.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		return errors.New("lfs/tmp does not take the file")
	}
	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(f, h), io.LimitReader(r, o.size+1))
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	switch {
	case err != nil:
		_ = s.root.Remove(tmp)
		return errors.New("it did not arrive whole")
	case n != o.size || hex.EncodeToString(h.Sum(nil)) != o.oid:
		_ = s.root.Remove(tmp)
		return errors.New("what arrived does not match its pointer")
	}
	if err := s.root.Rename(tmp, final); err != nil {
		_ = s.root.Remove(tmp)
		return errors.New("lfs/objects does not take the file")
	}
	return nil
}

// totalSize is the bytes of objs, and whether they are more than
// lfsMaxBytes.
func totalSize(objs []lfsObject) (int64, bool) {
	var n int64
	for _, o := range objs {
		n += o.size
		if n > lfsMaxBytes {
			return n, true
		}
	}
	return n, false
}

// chunks is objs in batches of at most lfsBatchSize.
func chunks(objs []lfsObject) [][]lfsObject {
	var out [][]lfsObject
	for len(objs) > 0 {
		n := min(len(objs), lfsBatchSize)
		out = append(out, objs[:n])
		objs = objs[n:]
	}
	return out
}

// upload sends the host each of objs it lacks, from the working copy:
// a file the host lacks that the working copy does not hold, or holds
// with other content than its pointer names, is an error, as is any
// the API or a transfer refuses. It answers the files and bytes sent.
func (c *lfsClient) upload(ctx context.Context, store *lfsStore, objs []lfsObject) (files int, sent int64, err error) {
	if _, over := totalSize(objs); over {
		return 0, 0, fmt.Errorf("the commits name more than %d GiB of LFS files, more than one push uploads", lfsMaxBytes>>30)
	}
	for _, batch := range chunks(objs) {
		results, err := c.batch(ctx, "upload", batch)
		if err != nil {
			return files, sent, err
		}
		byOID := map[string]lfsObject{}
		for _, o := range batch {
			byOID[o.oid] = o
		}
		for _, r := range results {
			o, ok := byOID[strings.ToLower(r.OID)]
			if !ok {
				return files, sent, errors.New("the repository's LFS API answered for a file it was not asked about")
			}
			delete(byOID, o.oid)
			if r.Error != nil {
				return files, sent, fmt.Errorf("%s: the repository's LFS API refused it (%d): %s", o.path, r.Error.Code, clipUTF8(r.Error.Message, 300))
			}
			up, ok := r.Actions["upload"]
			if !ok {
				continue // the host has it
			}
			if err := c.uploadOne(ctx, store, o, up, r.Actions["verify"]); err != nil {
				return files, sent, err
			}
			files++
			sent += o.size
		}
	}
	return files, sent, nil
}

func (c *lfsClient) uploadOne(ctx context.Context, store *lfsStore, o lfsObject, up, verify lfsAction) error {
	f, err := store.open(o)
	switch {
	case errors.Is(err, errLFSMissing):
		return fmt.Errorf("%s is an LFS file whose content is not in the working copy's .git/lfs/objects, and the host does not have it", o.path)
	case err != nil:
		return fmt.Errorf("%s is an LFS file whose content in .git/lfs/objects does not match its pointer", o.path)
	}
	defer func() { _ = f.Close() }()
	resp, err := c.transfer(ctx, http.MethodPut, up, f, o.size, "application/octet-stream")
	if err != nil {
		return fmt.Errorf("%s did not upload: %v", o.path, err)
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
	_ = resp.Body.Close()
	if verify.Href == "" {
		return nil
	}
	body, _ := json.Marshal(map[string]any{"oid": o.oid, "size": o.size})
	resp, err = c.transfer(ctx, http.MethodPost, verify, bytes.NewReader(body), int64(len(body)), lfsMediaType)
	if err != nil {
		return fmt.Errorf("%s did not verify: %v", o.path, err)
	}
	_ = resp.Body.Close()
	return nil
}

// download fetches each of objs the working copy does not hold into
// .git/lfs/objects. It goes on past a file that does not arrive, and
// answers the files and bytes kept and the first failure.
func (c *lfsClient) download(ctx context.Context, store *lfsStore, objs []lfsObject) (files int, got int64, err error) {
	var need []lfsObject
	for _, o := range objs {
		if !store.has(o) {
			need = append(need, o)
		}
	}
	if _, over := totalSize(need); over {
		return 0, 0, fmt.Errorf("the refs name more than %d GiB of LFS files, more than one operation downloads", lfsMaxBytes>>30)
	}
	var first error
	note := func(e error) {
		if first == nil {
			first = e
		}
	}
	for _, batch := range chunks(need) {
		results, err := c.batch(ctx, "download", batch)
		if err != nil {
			return files, got, err
		}
		byOID := map[string]lfsObject{}
		for _, o := range batch {
			byOID[o.oid] = o
		}
		for _, r := range results {
			o, ok := byOID[strings.ToLower(r.OID)]
			if !ok {
				return files, got, errors.New("the repository's LFS API answered for a file it was not asked about")
			}
			delete(byOID, o.oid)
			dl, ok := r.Actions["download"]
			switch {
			case r.Error != nil:
				note(fmt.Errorf("%s: the repository's LFS API refused it (%d): %s", o.path, r.Error.Code, clipUTF8(r.Error.Message, 300)))
				continue
			case !ok:
				note(fmt.Errorf("%s: the repository's LFS API gave no address for it", o.path))
				continue
			}
			resp, err := c.transfer(ctx, http.MethodGet, dl, nil, -1, "")
			if err != nil {
				if ctx.Err() != nil {
					return files, got, ctx.Err()
				}
				note(fmt.Errorf("%s did not download: %v", o.path, err))
				continue
			}
			err = store.write(o, resp.Body)
			_ = resp.Body.Close()
			if err != nil {
				note(fmt.Errorf("%s did not download: %v", o.path, err))
				continue
			}
			files++
			got += o.size
		}
		for _, o := range byOID {
			note(fmt.Errorf("%s: the repository's LFS API did not answer for it", o.path))
		}
	}
	return files, got, first
}

// pushLFS uploads the LFS files a push of specs names that the host
// lacks, before the push.
func pushLFS(ctx context.Context, cfg ServerConfig, req GitRequest, r *git.Repository, repo gitrepo.Repository, dir string, specs []pushSpec, before map[string]plumbing.Hash) (int, int64, error) {
	objs, err := lfsPushObjects(ctx, r, specs, before)
	if err != nil || len(objs) == 0 {
		return 0, 0, err
	}
	return withLFS(cfg, req, repo, dir, func(c *lfsClient, s *lfsStore) (int, int64, error) {
		return c.upload(ctx, s, objs)
	})
}

// fetchLFS downloads the LFS files the trees of tips name into the
// working copy at dir, answering the files and bytes kept and why any
// did not arrive.
func fetchLFS(ctx context.Context, cfg ServerConfig, req GitRequest, r *git.Repository, repo gitrepo.Repository, dir string, tips []plumbing.Hash) (int, int64, string) {
	objs, err := lfsTipObjects(ctx, r, tips)
	if err == nil && len(objs) > 0 {
		var n int
		var b int64
		n, b, err = withLFS(cfg, req, repo, dir, func(c *lfsClient, s *lfsStore) (int, int64, error) {
			return c.download(ctx, s, objs)
		})
		if err == nil {
			return n, b, ""
		}
		return n, b, err.Error()
	}
	if err != nil {
		return 0, 0, err.Error()
	}
	return 0, 0, ""
}

func withLFS(cfg ServerConfig, req GitRequest, repo gitrepo.Repository, dir string, fn func(*lfsClient, *lfsStore) (int, int64, error)) (int, int64, error) {
	c, err := newLFSClient(cfg, req, repo)
	if err != nil {
		return 0, 0, err
	}
	defer c.close()
	s, err := openLFSStore(dir)
	if err != nil {
		return 0, 0, err
	}
	defer s.close()
	return fn(c, s)
}
