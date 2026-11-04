package agentshell

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// lfsJob is req with LFS on and no LFS host beyond the repository's.
func lfsJob(req GitRequest) GitRequest {
	req.LFS = &GitLFS{}
	return req
}

// commitLFS commits content in the working copy wc as an LFS file at
// name: the pointer in the commit and the content in .git/lfs/objects,
// as git-lfs leaves them; keep false leaves the content out.
func commitLFS(t *testing.T, wc, name, content string, keep bool) string {
	t.Helper()
	oid, pointer := lfsFile(content)
	if err := os.WriteFile(filepath.Join(wc, name), []byte(pointer), 0o644); err != nil {
		t.Fatal(err)
	}
	if keep {
		keepLocal(t, wc, content)
	}
	runGitCLI(t, wc, "add", name)
	runGitCLI(t, wc, "commit", "-q", "-m", "add "+name)
	return oid
}

// Why this test exists: an LFS file pushed as its pointer alone leaves
// the host holding a pointer nobody can download, which breaks every
// checkout of the commit. A push with LFS must upload the files the
// host lacks before any ref moves, through the repository's own LFS API
// with the token, with the file's content sent to the storage without
// it; pushing the same branch again uploads what the host still lacks;
// and the answer counts what went up.
func TestGitPushUploadsLFSFilesBeforeTheRefs(t *testing.T) {
	srv := newTestGitServer(t)
	srv.seed(t, "acme/widgets", "one")
	proxy := newFakeProxy(t)
	d := newGitDaemon(t, proxy.URL)
	if res := d.run(t, cloneJob(srv, "wc")); res.Error != "" {
		t.Fatalf("clone = %+v", res)
	}
	wc := filepath.Join(d.root, "wc")
	runGitCLI(t, wc, "checkout", "-q", "-b", "agent/x")
	oid := commitLFS(t, wc, "part.step", "a solid model", true)
	var refAtUpload string
	srv.lfs.onUpload = func(string) { refAtUpload = srv.bareRev(t, "refs/heads/agent/x") }

	res := d.run(t, lfsJob(pushJob(srv, "wc", "refs/heads/agent/x:refs/heads/agent/x")))
	if res.Error != "" || res.LFSFiles != 1 || res.LFSBytes != int64(len("a solid model")) || len(res.Updated) != 1 {
		t.Fatalf("push = %+v", res)
	}
	if !srv.lfs.has(oid) || refAtUpload != "" {
		t.Errorf("uploaded %v, with the host's agent/x at %q", srv.lfs.has(oid), refAtUpload)
	}
	srv.lfs.noTokenOnTransfers(t)

	// The same push again: the host has it all, so nothing goes up.
	res = d.run(t, lfsJob(pushJob(srv, "wc", "refs/heads/agent/x:refs/heads/agent/x")))
	if res.Error != "" || res.LFSFiles != 0 || len(srv.lfs.uploaded) != 1 {
		t.Errorf("again = %+v, uploads %v", res, srv.lfs.uploaded)
	}

	// The branch's file lost from the host: pushing it again, up to
	// date, puts it back.
	srv.lfs.mu.Lock()
	delete(srv.lfs.objects, oid)
	srv.lfs.mu.Unlock()
	res = d.run(t, lfsJob(pushJob(srv, "wc", "refs/heads/agent/x:refs/heads/agent/x")))
	if res.Error != "" || res.LFSFiles != 1 || len(res.Updated) != 0 || !srv.lfs.has(oid) {
		t.Errorf("an up-to-date branch whose file the host lacks = %+v", res)
	}
}

// Why this test exists: a pointer whose file the working copy does not
// hold must stop the push before any ref moves, naming the file, so the
// host never takes a pointer it cannot serve.
func TestGitPushWithoutAnLFSFileMovesNothing(t *testing.T) {
	srv := newTestGitServer(t)
	srv.seed(t, "acme/widgets", "one")
	d := newGitDaemon(t, "")
	if res := d.run(t, cloneJob(srv, "wc")); res.Error != "" {
		t.Fatalf("clone = %+v", res)
	}
	wc := filepath.Join(d.root, "wc")
	runGitCLI(t, wc, "checkout", "-q", "-b", "agent/x")
	commitLFS(t, wc, "part.step", "content never kept", false)
	res := d.run(t, lfsJob(pushJob(srv, "wc", "refs/heads/agent/x:refs/heads/agent/x")))
	if res.Error != "LFS: part.step is an LFS file whose content is not in the working copy's .git/lfs/objects, and the host does not have it; no ref was pushed" || len(res.Updated) != 0 {
		t.Errorf("push = %+v", res)
	}
	if got := srv.bareRev(t, "refs/heads/agent/x"); got != "" {
		t.Errorf("the host took agent/x at %s", got)
	}
}

// Why this test exists: a clone with LFS must leave the files its HEAD
// names in .git/lfs/objects, where the agent's git-lfs checks them out,
// and a fetch the files of the refs it writes, even when they were
// already up to date; a file the host cannot serve leaves the clone as
// it landed, saying which. A job without LFS, as Git (Basic)'s, asks no
// LFS API anything.
func TestGitCloneAndFetchDownloadLFSFiles(t *testing.T) {
	srv := newTestGitServer(t)
	work := srv.seed(t, "acme/widgets", "one")
	oid := commitLFS(t, work, "part.step", "a solid model", false)
	srv.lfs.put("a solid model")
	runGitCLI(t, work, "push", "-q", filepath.Join(srv.root, "acme/widgets.git"), "main")
	proxy := newFakeProxy(t)
	d := newGitDaemon(t, proxy.URL)

	res := d.run(t, cloneJob(srv, "plain"))
	if res.Error != "" || res.LFSFiles != 0 || len(srv.lfs.batches) != 0 {
		t.Fatalf("a clone without LFS = %+v, batches %v", res, srv.lfs.batches)
	}

	res = d.run(t, lfsJob(cloneJob(srv, "wc")))
	if res.Error != "" || res.LFSError != "" || res.LFSFiles != 1 {
		t.Fatalf("clone = %+v", res)
	}
	kept := filepath.Join(d.root, "wc", ".git", "lfs", "objects", oid[:2], oid[2:4], oid)
	if b, err := os.ReadFile(kept); err != nil || string(b) != "a solid model" {
		t.Errorf("kept = %q %v", b, err)
	}
	if b, _ := os.ReadFile(filepath.Join(d.root, "wc", "part.step")); !strings.HasPrefix(string(b), lfsVersion) {
		t.Errorf("the working tree's file = %q, want the pointer", b)
	}
	srv.lfs.noTokenOnTransfers(t)

	// A fetch, up to date, of a working copy that lost the file gets it
	// again.
	if err := os.Remove(kept); err != nil {
		t.Fatal(err)
	}
	fetch := lfsJob(cloneJob(srv, "wc"))
	fetch.Op = GitOpFetch
	res = d.run(t, fetch)
	if res.Error != "" || res.LFSFiles != 1 {
		t.Errorf("fetch = %+v", res)
	}

	// A file the host lacks: the clone lands, and says which file.
	work2 := srv.seed(t, "acme/gadgets", "one")
	commitLFS(t, work2, "lost.step", "never uploaded", false)
	runGitCLI(t, work2, "push", "-q", filepath.Join(srv.root, "acme/gadgets.git"), "main")
	job := lfsJob(cloneJob(srv, "gadgets"))
	job.URL = srv.URL + "/acme/gadgets.git"
	res = d.run(t, job)
	if res.Error != "" || !hasRef(res.Updated, "refs/heads/main") || res.LFSFiles != 0 ||
		res.LFSError != "lost.step: the repository's LFS API refused it (404): Object does not exist on the server" {
		t.Errorf("a clone whose file the host lacks = %+v", res)
	}
}

// Why this test exists: the LFS API decides where a file's content
// goes, and the daemon must send it nowhere but the repository's host
// and the job's LFS hosts. A storage address elsewhere stops the push
// before any ref moves and sends that host nothing.
func TestGitPushLFSGoesOnlyToTheJobsHosts(t *testing.T) {
	srv := newTestGitServer(t)
	srv.seed(t, "acme/widgets", "one")
	srv.lfs.hrefBase = "http://storage.example"
	proxy := newFakeProxy(t)
	d := newGitDaemon(t, proxy.URL)
	if res := d.run(t, cloneJob(srv, "wc")); res.Error != "" {
		t.Fatalf("clone = %+v", res)
	}
	wc := filepath.Join(d.root, "wc")
	runGitCLI(t, wc, "checkout", "-q", "-b", "agent/x")
	commitLFS(t, wc, "part.step", "a solid model", true)
	res := d.run(t, lfsJob(pushJob(srv, "wc", "refs/heads/agent/x:refs/heads/agent/x")))
	if !strings.Contains(res.Error, "keeps files on storage.example, which is not a host this connection's LFS files go to") || len(res.Updated) != 0 {
		t.Errorf("push = %+v", res)
	}
	if _, hosts := proxy.seen(); slices.Contains(hosts, "storage.example") {
		t.Error("the daemon reached a host the job does not name")
	}
}
