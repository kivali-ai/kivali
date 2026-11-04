package agentshell

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"

	"github.com/kivali-ai/kivali/internal/gitrepo"
)

// testLFS is a repository's LFS batch API and its storage, as GitHub
// serves them: the batch API (at <repo>.git/info/lfs/objects/batch)
// takes the job's token as basic auth and names, for each file, a
// transfer at /lfs-store/<oid> signed with an X-Lfs-Sig header, and a
// verify at /lfs-verify. It records what each request carried.
type testLFS struct {
	// hrefBase is where transfers are named; the server's own URL
	// unless a test names another host.
	hrefBase string

	mu        sync.Mutex
	objects   map[string][]byte
	batches   []string
	uploaded  []string
	verified  []string
	got       []string
	transfers []http.Header
	// onUpload, when set, runs as an upload arrives, before it is kept.
	onUpload func(oid string)
	// corrupt names files the storage serves with other content.
	corrupt map[string]bool
}

func newTestLFS() *testLFS {
	return &testLFS{objects: map[string][]byte{}, corrupt: map[string]bool{}}
}

// lfsFile is content as an LFS file: its oid and its pointer.
func lfsFile(content string) (oid string, pointer string) {
	sum := sha256.Sum256([]byte(content))
	oid = hex.EncodeToString(sum[:])
	return oid, fmt.Sprintf("%s\noid sha256:%s\nsize %d\n", lfsVersion, oid, len(content))
}

func (l *testLFS) has(oid string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	_, ok := l.objects[oid]
	return ok
}

func (l *testLFS) put(content string) string {
	oid, _ := lfsFile(content)
	l.mu.Lock()
	l.objects[oid] = []byte(content)
	l.mu.Unlock()
	return oid
}

// serve answers r when it is an LFS request, and reports whether it was.
func (l *testLFS) serve(w http.ResponseWriter, r *http.Request) bool {
	switch {
	case strings.HasSuffix(r.URL.Path, ".git/info/lfs/objects/batch"):
		l.batch(w, r)
	case strings.HasPrefix(r.URL.Path, "/lfs-store/"):
		l.store(w, r, strings.TrimPrefix(r.URL.Path, "/lfs-store/"))
	case r.URL.Path == "/lfs-verify":
		l.mu.Lock()
		l.transfers = append(l.transfers, r.Header.Clone())
		var v struct{ OID string }
		_ = json.NewDecoder(r.Body).Decode(&v)
		l.verified = append(l.verified, v.OID)
		l.mu.Unlock()
		w.WriteHeader(http.StatusOK)
	default:
		return false
	}
	return true
}

func (l *testLFS) batch(w http.ResponseWriter, r *http.Request) {
	user, pass, ok := r.BasicAuth()
	if !ok || user != "x-access-token" || pass != gitTestToken || r.Header.Get("Content-Type") != lfsMediaType {
		http.Error(w, `{"message":"Bad credentials"}`, http.StatusUnauthorized)
		return
	}
	var req struct {
		Operation string `json:"operation"`
		Objects   []struct {
			OID  string `json:"oid"`
			Size int64  `json:"size"`
		} `json:"objects"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	base := l.hrefBase
	if base == "" {
		base = "http://" + r.Host
	}
	l.mu.Lock()
	l.batches = append(l.batches, req.Operation)
	out := []map[string]any{}
	for _, o := range req.Objects {
		_, have := l.objects[o.OID]
		item := map[string]any{"oid": o.OID, "size": o.Size}
		signed := map[string]string{"X-Lfs-Sig": "sig-" + o.OID}
		switch {
		case req.Operation == "upload" && !have:
			item["actions"] = map[string]any{
				"upload": map[string]any{"href": base + "/lfs-store/" + o.OID, "header": signed},
				"verify": map[string]any{"href": base + "/lfs-verify", "header": signed},
			}
		case req.Operation == "download" && have:
			item["actions"] = map[string]any{"download": map[string]any{"href": base + "/lfs-store/" + o.OID, "header": signed}}
		case req.Operation == "download":
			item["error"] = map[string]any{"code": 404, "message": "Object does not exist on the server"}
		}
		out = append(out, item)
	}
	l.mu.Unlock()
	w.Header().Set("Content-Type", lfsMediaType)
	_ = json.NewEncoder(w).Encode(map[string]any{"transfer": "basic", "objects": out})
}

func (l *testLFS) store(w http.ResponseWriter, r *http.Request, oid string) {
	l.mu.Lock()
	l.transfers = append(l.transfers, r.Header.Clone())
	l.mu.Unlock()
	if r.Header.Get("X-Lfs-Sig") != "sig-"+oid {
		http.Error(w, "unsigned", http.StatusForbidden)
		return
	}
	switch r.Method {
	case http.MethodPut:
		body, _ := io.ReadAll(r.Body)
		if l.onUpload != nil {
			l.onUpload(oid)
		}
		if sum := sha256.Sum256(body); hex.EncodeToString(sum[:]) != oid {
			http.Error(w, "content does not match", http.StatusBadRequest)
			return
		}
		l.mu.Lock()
		l.objects[oid] = body
		l.uploaded = append(l.uploaded, oid)
		l.mu.Unlock()
	case http.MethodGet:
		l.mu.Lock()
		body, ok := l.objects[oid]
		if l.corrupt[oid] {
			body = append([]byte("tampered "), body...)
		}
		l.got = append(l.got, oid)
		l.mu.Unlock()
		if !ok {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write(body)
	default:
		http.Error(w, "method", http.StatusMethodNotAllowed)
	}
}

// noTokenOnTransfers fails t when a transfer carried any credential of
// the job's.
func (l *testLFS) noTokenOnTransfers(t *testing.T) {
	t.Helper()
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, h := range l.transfers {
		if h.Get("Authorization") != "" {
			t.Errorf("a transfer carried Authorization %q", h.Get("Authorization"))
		}
		for _, vs := range h {
			for _, v := range vs {
				if strings.Contains(v, gitTestToken) || strings.Contains(v, gitTestProxySecret) {
					t.Errorf("a transfer carried a credential: %q", v)
				}
			}
		}
	}
}

// lfsUnit is a client of a testLFS on its own server, and a working
// copy's store, for tests of the client alone.
func lfsUnit(t *testing.T, extraHosts ...string) (*testLFS, *lfsClient, *lfsStore, string) {
	t.Helper()
	l := newTestLFS()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !l.serve(w, r) {
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	u, _ := url.Parse(srv.URL)
	c, err := newLFSClient(ServerConfig{GitInsecureHTTP: true},
		GitRequest{Auth: GitAuth{Username: "x-access-token", Password: gitTestToken}, LFS: &GitLFS{Hosts: extraHosts}},
		gitrepo.Repository{Scheme: "http", Host: u.Host, Path: "acme/widgets"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(c.close)
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	s, err := openLFSStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.close)
	return l, c, s, dir
}

// keepLocal writes content into the working copy at dir's
// .git/lfs/objects, as git-lfs keeps it.
func keepLocal(t *testing.T, dir, content string) lfsObject {
	t.Helper()
	oid, _ := lfsFile(content)
	p := filepath.Join(dir, ".git", "lfs", "objects", oid[:2], oid[2:4], oid)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return lfsObject{oid: oid, size: int64(len(content)), path: "models/" + oid[:6] + ".step"}
}

// Why this test exists: a pointer is what a commit holds in place of an
// LFS file, and only a well-formed one may lead the daemon to read or
// write a file under .git/lfs: the version line first, oid as sha256 in
// lower-case hex, size a plain number, a newline after each line, the
// whole smaller than 1024 bytes. Anything else is an ordinary file.
func TestParseLFSPointer(t *testing.T) {
	oid, pointer := lfsFile("solid model")
	if got, size, ok := parseLFSPointer([]byte(pointer)); !ok || got != oid || size != 11 {
		t.Errorf("pointer = %q %d %v", got, size, ok)
	}
	withExt := lfsVersion + "\next-0-foo sha256:" + oid + "\noid sha256:" + oid + "\nsize 11\n"
	if _, _, ok := parseLFSPointer([]byte(withExt)); !ok {
		t.Error("a pointer with an extension line is not read")
	}
	for name, b := range map[string]string{
		"no newline":    strings.TrimSuffix(pointer, "\n"),
		"other version": strings.Replace(pointer, "spec/v1", "spec/v2", 1),
		"upper hex":     strings.Replace(pointer, oid, strings.ToUpper(oid), 1),
		"short oid":     strings.Replace(pointer, oid, oid[:63], 1),
		"md5":           strings.Replace(pointer, "sha256:", "md5:", 1),
		"signed size":   strings.Replace(pointer, "size 11", "size +11", 1),
		"no size":       strings.Replace(pointer, "size 11\n", "", 1),
		"no oid":        lfsVersion + "\nsize 11\n",
		"text":          "just a README\n",
		"too long":      pointer + strings.Repeat("x-key value\n", 100),
		"path traverse": strings.Replace(pointer, oid, "../../../../etc/passwd", 1),
	} {
		if _, _, ok := parseLFSPointer([]byte(b)); ok {
			t.Errorf("%s: read as a pointer", name)
		}
	}
}

// Why this test exists: a push uploads only the LFS files the host
// lacks, after checking each against its pointer, through the
// repository's own LFS API with the token, while the file's content goes
// to the address the API named with the headers it named and no
// credential of the job's. The verify the API asks for follows each
// upload.
func TestLFSUploadSendsWhatTheHostLacksAndNoToken(t *testing.T) {
	l, c, s, dir := lfsUnit(t)
	had := keepLocal(t, dir, "already on the host")
	l.put("already on the host")
	lacked := keepLocal(t, dir, "a new solid model")
	files, sent, err := c.upload(context.Background(), s, []lfsObject{had, lacked})
	if err != nil || files != 1 || sent != lacked.size {
		t.Fatalf("upload = %d %d %v", files, sent, err)
	}
	if !slices.Equal(l.uploaded, []string{lacked.oid}) || !slices.Equal(l.verified, []string{lacked.oid}) || !slices.Equal(l.batches, []string{"upload"}) {
		t.Errorf("uploaded %v verified %v batches %v", l.uploaded, l.verified, l.batches)
	}
	l.noTokenOnTransfers(t)
}

// Why this test exists: a pointer whose file the working copy does not
// hold, or holds with other content, must stop the push before any ref
// moves, naming the file: pushed anyway, the host would hold a pointer
// to a file nobody can download, which is the failure LFS support
// exists to end. A file the host already has needs no local copy.
func TestLFSUploadRefusesAMissingOrAlteredFile(t *testing.T) {
	l, c, s, dir := lfsUnit(t)
	oid, _ := lfsFile("never added")
	missing := lfsObject{oid: oid, size: 11, path: "models/missing.step"}
	if _, _, err := c.upload(context.Background(), s, []lfsObject{missing}); err == nil ||
		err.Error() != "models/missing.step is an LFS file whose content is not in the working copy's .git/lfs/objects, and the host does not have it" {
		t.Errorf("missing = %v", err)
	}
	altered := keepLocal(t, dir, "the real content")
	p := filepath.Join(dir, ".git", "lfs", "objects", altered.oid[:2], altered.oid[2:4], altered.oid)
	if err := os.WriteFile(p, []byte("the fake content"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, _, err := c.upload(context.Background(), s, []lfsObject{altered}); err == nil || !strings.Contains(err.Error(), "does not match its pointer") {
		t.Errorf("altered = %v", err)
	}
	if len(l.uploaded) != 0 {
		t.Errorf("uploaded %v", l.uploaded)
	}
	l.put("never added")
	if files, _, err := c.upload(context.Background(), s, []lfsObject{missing}); err != nil || files != 0 {
		t.Errorf("a file the host has, not held locally = %d %v", files, err)
	}
}

// Why this test exists: the LFS API's answer chooses where a file's
// content goes. The daemon must send it only over https (http only
// where tests allow it), to no address with user info, and only to the
// repository's host or one the job names; anywhere else is refused,
// naming the host so the owner can tell what is missing.
func TestLFSTransfersStayOnTheJobsHosts(t *testing.T) {
	_, c, _, _ := lfsUnit(t, "github-cloud.githubusercontent.com")
	for href, want := range map[string]string{
		"https://github-cloud.githubusercontent.com/alambic/x":      "",
		"https://GITHUB-CLOUD.githubusercontent.com/alambic/x":      "",
		"http://127.0.0.1:1/lfs-store/x":                            "",
		"https://evil.example/x":                                    "keeps files on evil.example, which is not a host",
		"https://github-cloud.githubusercontent.com.evil.example/x": "which is not a host",
		"https://user:pw@github-cloud.githubusercontent.com/x":      "a transfer address the daemon does not use",
		"ftp://github-cloud.githubusercontent.com/x":                "a transfer address the daemon does not use",
		"/relative": "a transfer address the daemon does not use",
	} {
		_, err := c.transferURL(href)
		if (want == "") != (err == nil) || (err != nil && !strings.Contains(err.Error(), want)) {
			t.Errorf("%s: %v", href, err)
		}
	}
	strict := *c
	strict.insecure = false
	if _, err := strict.transferURL("http://127.0.0.1:1/lfs-store/x"); err == nil {
		t.Error("plain http is let through outside tests")
	}
}

// Why this test exists: a download keeps a file only once all of it has
// arrived and it matches its pointer, so a working copy never holds a
// file under an oid it does not hash to; it goes on past a file that
// fails, says which, and skips a file the working copy already holds.
func TestLFSDownloadKeepsOnlyMatchingFiles(t *testing.T) {
	l, c, s, dir := lfsUnit(t)
	good := l.put("a good model")
	bad := l.put("a model served wrong")
	l.corrupt[bad] = true
	held := keepLocal(t, dir, "held already")
	l.put("held already")
	gone, _ := lfsFile("never uploaded")
	objs := []lfsObject{
		{oid: good, size: 12, path: "good.step"},
		{oid: bad, size: 20, path: "bad.step"},
		held,
		{oid: gone, size: 14, path: "gone.step"},
	}
	files, got, err := c.download(context.Background(), s, objs)
	if files != 1 || got != 12 || err == nil || !strings.HasPrefix(err.Error(), "bad.step did not download: what arrived does not match its pointer") {
		t.Errorf("download = %d %d %v", files, got, err)
	}
	if b, err := os.ReadFile(filepath.Join(dir, ".git", "lfs", "objects", good[:2], good[2:4], good)); err != nil || string(b) != "a good model" {
		t.Errorf("good = %q %v", b, err)
	}
	if _, err := os.Stat(filepath.Join(dir, ".git", "lfs", "objects", bad[:2], bad[2:4], bad)); err == nil {
		t.Error("a file that does not match its pointer was kept")
	}
	if entries, _ := os.ReadDir(filepath.Join(dir, ".git", "lfs", "tmp")); len(entries) != 0 {
		t.Errorf("tmp holds %d files", len(entries))
	}
	if slices.Contains(l.got, held.oid) {
		t.Error("a file the working copy held was downloaded again")
	}
	l.noTokenOnTransfers(t)
}

// Why this test exists: .git/lfs is the agent's to write, so a link
// there could lead the daemon to read a file outside the working copy
// and upload it, or to write one there. Every read and write goes
// through an os.Root at .git, which refuses a link out of it.
func TestLFSStoreStaysInTheWorkingCopy(t *testing.T) {
	_, _, s, dir := lfsUnit(t)
	outside := t.TempDir()
	content := "a file outside the working copy"
	oid, _ := lfsFile(content)
	if err := os.MkdirAll(filepath.Join(outside, oid[2:4]), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(outside, oid[2:4], oid), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, ".git", "lfs", "objects"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(dir, ".git", "lfs", "objects", oid[:2])); err != nil {
		t.Skipf("symlinks: %v", err)
	}
	o := lfsObject{oid: oid, size: int64(len(content))}
	if f, err := s.open(o); err == nil {
		_ = f.Close()
		t.Error("a file outside the working copy was opened through a link")
	}
	if s.has(o) {
		t.Error("a file outside the working copy reads as held")
	}
	if err := s.write(o, strings.NewReader(content)); err == nil {
		t.Error("a file was written through a link out of the working copy")
	}
}

// Why this test exists: a push names the LFS files of its tips' trees
// (so pushing a branch the host already has still uploads what the host
// lacks) and those its new commits add or change, even when a later
// commit removed them; it walks back only to a commit the remote's refs
// name.
func TestLFSPushObjectsNamesTipsAndNewCommits(t *testing.T) {
	gitCLI(t)
	t.Setenv("HOME", t.TempDir())
	wc := t.TempDir()
	runGitCLI(t, wc, "init", "-q", "-b", "main")
	write := func(name, content string) {
		if err := os.WriteFile(filepath.Join(wc, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	oldOID, oldPointer := lfsFile("on the remote, removed later")
	write("old.step", oldPointer)
	runGitCLI(t, wc, "add", ".")
	runGitCLI(t, wc, "commit", "-q", "-m", "one")
	remoteTip := runGitCLI(t, wc, "rev-parse", "HEAD")
	_, keptPointer := lfsFile("kept at the tip")
	write("kept.step", keptPointer)
	write("README", "not a pointer\n")
	_, gonePointer := lfsFile("added then removed")
	write("gone.step", gonePointer)
	runGitCLI(t, wc, "add", ".")
	runGitCLI(t, wc, "commit", "-q", "-m", "two")
	runGitCLI(t, wc, "rm", "-q", "gone.step", "old.step")
	runGitCLI(t, wc, "commit", "-q", "-m", "three")
	tip := runGitCLI(t, wc, "rev-parse", "HEAD")

	r, err := git.PlainOpen(wc)
	if err != nil {
		t.Fatal(err)
	}
	specs := []pushSpec{{dst: "refs/heads/main", new: plumbing.NewHash(tip)}}
	before := map[string]plumbing.Hash{"refs/heads/main": plumbing.NewHash(remoteTip)}
	objs, err := lfsPushObjects(context.Background(), r, specs, before)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, o := range objs {
		got = append(got, o.path)
	}
	slices.Sort(got)
	if !slices.Equal(got, []string{"gone.step", "kept.step"}) {
		t.Errorf("objects = %v", got)
	}
	// The remote's own commit is not walked: old.step, which only it
	// names, is the host's already.
	for _, o := range objs {
		if o.oid == oldOID {
			t.Error("a file of a commit the remote has was named")
		}
	}
}
