package agentshell

import (
	"bytes"
	"compress/zlib"
	"encoding/base64"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/config"
	"github.com/go-git/go-git/v5/plumbing"
	githttp "github.com/go-git/go-git/v5/plumbing/transport/http"
)

// pushJob is a push of specs from the working copy at path to
// acme/widgets on srv, allowed agent/* branches and every tag.
func pushJob(srv *testGitServer, path string, specs ...string) GitRequest {
	return GitRequest{
		Op:                  GitOpPush,
		Path:                path,
		URL:                 srv.URL + "/acme/widgets.git",
		RefSpecs:            specs,
		AllowedRepositories: []string{"acme/*"},
		AllowedRefs:         []string{"refs/heads/agent/*", "refs/tags/*"},
		Host:                srv.host(),
		Auth:                GitAuth{Username: "x-access-token", Password: gitTestToken},
		Proxy:               GitProxy{Username: gitTestProxyUser, Password: gitTestProxySecret},
		TimeoutSeconds:      60,
	}
}

// bareRev is what ref names in srv's acme/widgets, or "" when it has
// no such ref.
func (s *testGitServer) bareRev(t *testing.T, ref string) string {
	t.Helper()
	out, err := gitCommand(t, filepath.Join(s.root, "acme/widgets.git"), "rev-parse", "--verify", "-q", ref).Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// commit adds a commit to the working copy wc on its current branch
// and returns it.
func commit(t *testing.T, wc, content string) string {
	t.Helper()
	if err := os.WriteFile(filepath.Join(wc, "README"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	runGitCLI(t, wc, "commit", "-q", "-am", content)
	return runGitCLI(t, wc, "rev-parse", "HEAD")
}

// Why this test exists: a push is the one operation that writes to the
// owner's repository with the owner's token. A new branch, a tag (as
// the tag object) and HEAD pushed to a branch must each land on the
// host and be reported with the hash it moved from and to (go-git
// reports none, so the daemon works them out); pushing again must be
// success with nothing moved; the push must go through the proxy with
// the connection's credential; and the token must stay off disk.
func TestGitPush(t *testing.T) {
	srv := newTestGitServer(t)
	srv.seed(t, "acme/widgets", "one")
	proxy := newFakeProxy(t)
	d := newGitDaemon(t, proxy.URL)
	if res := d.run(t, cloneJob(srv, "wc")); res.Error != "" {
		t.Fatalf("clone = %+v", res)
	}
	wc := filepath.Join(d.root, "wc")
	runGitCLI(t, wc, "checkout", "-q", "-b", "agent/x")
	head := commit(t, wc, "two")

	res := d.run(t, pushJob(srv, "wc", "refs/heads/agent/x:refs/heads/agent/x"))
	if res.Error != "" || res.Repository != "acme/widgets" || len(res.Updated) != 1 ||
		res.Updated[0] != (GitRefUpdate{Ref: "refs/heads/agent/x", New: head}) {
		t.Fatalf("push of a new branch = %+v", res)
	}
	if got := srv.bareRev(t, "refs/heads/agent/x"); got != head {
		t.Errorf("the host has agent/x at %q, want %s", got, head)
	}

	// An annotated tag goes as the tag object; HEAD to another branch
	// as the commit it names; agent/x again moves nothing.
	runGitCLI(t, wc, "tag", "-a", "-m", "two", "v2")
	tagObj := runGitCLI(t, wc, "rev-parse", "refs/tags/v2")
	next := commit(t, wc, "three")
	res = d.run(t, pushJob(srv, "wc", "refs/tags/v2:refs/tags/v2", "HEAD:refs/heads/agent/y"))
	want := []GitRefUpdate{{Ref: "refs/heads/agent/y", New: next}, {Ref: "refs/tags/v2", New: tagObj}}
	if res.Error != "" || len(res.Updated) != 2 || res.Updated[0] != want[0] || res.Updated[1] != want[1] {
		t.Fatalf("push of a tag and HEAD = %+v, want %+v", res, want)
	}
	if srv.bareRev(t, "refs/tags/v2") != tagObj || srv.bareRev(t, "refs/heads/agent/y") != next {
		t.Error("the host lacks v2 or agent/y")
	}
	// A fast-forward of an existing branch names where it moved from.
	res = d.run(t, pushJob(srv, "wc", "refs/heads/agent/x:refs/heads/agent/x"))
	if res.Error != "" || len(res.Updated) != 1 || res.Updated[0] != (GitRefUpdate{Ref: "refs/heads/agent/x", Old: head, New: next}) {
		t.Fatalf("a fast-forward = %+v", res)
	}
	if res := d.run(t, pushJob(srv, "wc", "refs/heads/agent/x:refs/heads/agent/x")); res.Error != "" || len(res.Updated) != 0 {
		t.Errorf("pushed again = %+v", res)
	}

	auths, hosts := proxy.seen()
	wantAuth := "Basic " + base64.StdEncoding.EncodeToString([]byte(gitTestProxyUser+":"+gitTestProxySecret))
	for i := range auths {
		if auths[i] != wantAuth || hosts[i] != srv.host() {
			t.Errorf("proxy request %d: auth %q host %q", i, auths[i], hosts[i])
		}
	}
	cfg, err := os.ReadFile(filepath.Join(wc, ".git", "config"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(cfg), gitTestToken) || strings.Contains(string(cfg), gitTestProxySecret) || strings.Contains(string(cfg), "anonymous") {
		t.Errorf(".git/config after a push:\n%s", cfg)
	}
}

// Why this test exists: a push that is not a fast-forward overwrites
// history on the owner's repository. Without + it must be refused;
// with + it must be refused by the daemon itself when the job does not
// allow force (core's check is not the only one), before anything
// reaches the host; with the job's Force it must go through. A host
// that refuses a push (its pre-receive hook) must come back as the
// push's error, not as success.
func TestGitPushForceAndRejection(t *testing.T) {
	srv := newTestGitServer(t)
	srv.seed(t, "acme/widgets", "one")
	d := newGitDaemon(t, "")
	if res := d.run(t, cloneJob(srv, "wc")); res.Error != "" {
		t.Fatalf("clone = %+v", res)
	}
	wc := filepath.Join(d.root, "wc")
	runGitCLI(t, wc, "checkout", "-q", "-b", "agent/x")
	mine := commit(t, wc, "mine")
	if res := d.run(t, pushJob(srv, "wc", "refs/heads/agent/x:refs/heads/agent/x")); res.Error != "" {
		t.Fatalf("push = %+v", res)
	}
	// Someone else moves agent/x on the host; the working copy rewrites
	// its own.
	other := t.TempDir()
	runGitCLI(t, "", "clone", "-q", "-b", "agent/x", filepath.Join(srv.root, "acme/widgets.git"), other)
	theirs := commit(t, other, "theirs")
	runGitCLI(t, other, "push", "-q", "origin", "agent/x")
	runGitCLI(t, wc, "commit", "-q", "--amend", "-m", "rewritten")
	rewritten := runGitCLI(t, wc, "rev-parse", "HEAD")

	if res := d.run(t, pushJob(srv, "wc", "refs/heads/agent/x:refs/heads/agent/x")); !strings.Contains(res.Error, "non-fast-forward update; the remote has commits") {
		t.Errorf("a non-fast-forward push = %+v", res)
	}
	before := srv.requests.Load()
	if res := d.run(t, pushJob(srv, "wc", "+refs/heads/agent/x:refs/heads/agent/x")); !strings.Contains(res.Error, "does not allow force-pushing") {
		t.Errorf("a + without the job's force = %+v", res)
	}
	if srv.requests.Load() != before {
		t.Error("a refused force push reached the host")
	}
	if got := srv.bareRev(t, "refs/heads/agent/x"); got != theirs {
		t.Fatalf("agent/x moved to %s", got)
	}
	job := pushJob(srv, "wc", "+refs/heads/agent/x:refs/heads/agent/x")
	job.Force = true
	res := d.run(t, job)
	if res.Error != "" || len(res.Updated) != 1 || res.Updated[0] != (GitRefUpdate{Ref: "refs/heads/agent/x", Old: theirs, New: rewritten}) {
		t.Fatalf("a force push = %+v", res)
	}
	if got := srv.bareRev(t, "refs/heads/agent/x"); got != rewritten || got == mine {
		t.Errorf("agent/x is %s, want %s", got, rewritten)
	}

	// The host's pre-receive hook refuses every push.
	hook := filepath.Join(srv.root, "acme/widgets.git", "hooks", "pre-receive")
	if err := os.MkdirAll(filepath.Dir(hook), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(hook, []byte("#!/bin/sh\necho 'refused by policy' >&2\nexit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	runGitCLI(t, wc, "checkout", "-q", "-b", "agent/z")
	commit(t, wc, "z")
	res = d.run(t, pushJob(srv, "wc", "refs/heads/agent/z:refs/heads/agent/z"))
	if !strings.Contains(res.Error, "declined") || len(res.Updated) != 0 {
		t.Errorf("a push the host refuses = %+v", res)
	}
	if srv.bareRev(t, "refs/heads/agent/z") != "" {
		t.Error("agent/z is on the host")
	}
}

// Why this test exists: the job's refspecs come from core, but the
// daemon holds them to the access itself, as it does the URL. A
// destination outside AllowedRefs, a deletion, a ref outside branches
// and tags, a job with no refs or no AllowedRefs, a source the working
// copy does not have, a destination named twice, and a repository
// (named or the working copy's origin) outside the ones the agent may
// push to must each be refused before any byte reaches the host; the
// last in words that say it is pushing it may not do, since the agent
// may still pull that repository.
func TestGitPushRefusals(t *testing.T) {
	srv := newTestGitServer(t)
	srv.seed(t, "acme/widgets", "one")
	d := newGitDaemon(t, "")
	if res := d.run(t, cloneJob(srv, "wc")); res.Error != "" {
		t.Fatalf("clone = %+v", res)
	}
	before := srv.requests.Load()
	for name, c := range map[string]struct {
		job   GitRequest
		words string
	}{
		"outside the refs": {pushJob(srv, "wc", "refs/heads/main:refs/heads/main"), "refs/heads/main is not a branch or tag the access lets you push"},
		"a deletion":       {pushJob(srv, "wc", ":refs/heads/agent/x"), "deletes a remote ref"},
		"a note":           {pushJob(srv, "wc", "refs/heads/main:refs/notes/x"), "is not a refspec of a push"},
		"a wildcard":       {pushJob(srv, "wc", "refs/heads/*:refs/heads/agent/*"), "is not a refspec of a push"},
		"no refs":          {pushJob(srv, "wc"), "needs at least one ref"},
		"no source":        {pushJob(srv, "wc", "refs/heads/nope:refs/heads/agent/x"), "refs/heads/nope is not in the working copy"},
		"twice":            {pushJob(srv, "wc", "refs/heads/main:refs/heads/agent/x", "HEAD:refs/heads/agent/x"), "named twice"},
		"no allowed refs": {func() GitRequest {
			j := pushJob(srv, "wc", "refs/heads/main:refs/heads/agent/x")
			j.AllowedRefs = nil
			return j
		}(), "is not a branch or tag the access lets you push"},
		"another repository": {func() GitRequest {
			j := pushJob(srv, "wc", "refs/heads/main:refs/heads/agent/x")
			j.AllowedRepositories = []string{"other/*"}
			return j
		}(), "acme/widgets is not a repository the access lets you push to"},
		"an origin outside": {func() GitRequest {
			j := pushJob(srv, "wc", "refs/heads/main:refs/heads/agent/x")
			j.URL, j.AllowedRepositories = "", []string{"other/*"}
			return j
		}(), "the working copy's origin, acme/widgets, is not a repository the access lets you push to"},
	} {
		if res := d.run(t, c.job); !strings.Contains(res.Error, c.words) {
			t.Errorf("%s = %+v, want %q", name, res, c.words)
		}
	}
	if n := srv.requests.Load() - before; n != 0 {
		t.Errorf("%d requests of refused pushes reached the host", n)
	}
	if srv.bareRev(t, "refs/heads/agent/x") != "" {
		t.Error("agent/x is on the host")
	}
}

// Why this test exists: a push runs against the agent's own working
// copy, whose .git/config and hooks the agent writes. A pre-push hook
// that refuses (and leaves a mark), an insteadOf or pushInsteadOf rule
// and a pushurl to another server, the same rules in the global
// configuration: none may change where a push goes or run anything.
// Without the job's URL the daemon must use origin's url as written
// and refuse it off the connection's host. The control shows a push
// through the named remote follows the poison, so the test proves
// something.
func TestGitPushReachesOnlyTheCheckedURL(t *testing.T) {
	good := newTestGitServer(t)
	good.seed(t, "acme/widgets", "one")
	evil := newTestGitServer(t)
	evil.seed(t, "acme/widgets", "evil")
	d := newGitDaemon(t, "")
	if res := d.run(t, cloneJob(good, "wc")); res.Error != "" {
		t.Fatalf("clone = %+v", res)
	}
	wc := filepath.Join(d.root, "wc")
	runGitCLI(t, wc, "checkout", "-q", "-b", "agent/x")
	head := commit(t, wc, "two")

	runGitCLI(t, wc, "config", "url."+evil.URL+"/.insteadOf", good.URL+"/")
	runGitCLI(t, wc, "config", "url."+evil.URL+"/.pushInsteadOf", good.URL+"/")
	runGitCLI(t, wc, "config", "remote.origin.pushurl", evil.URL+"/acme/widgets.git")
	if err := os.WriteFile(filepath.Join(os.Getenv("HOME"), ".gitconfig"),
		[]byte("[url \""+evil.URL+"/\"]\n\tinsteadOf = "+good.URL+"/\n\tpushInsteadOf = "+good.URL+"/\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	mark := filepath.Join(t.TempDir(), "hook-ran")
	if err := os.MkdirAll(filepath.Join(wc, ".git", "hooks"), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, hook := range []string{"pre-push", "reference-transaction", "post-update"} {
		if err := os.WriteFile(filepath.Join(wc, ".git", "hooks", hook), []byte("#!/bin/sh\ntouch "+mark+"\nexit 1\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	evilBefore := evil.requests.Load()

	res := d.run(t, pushJob(good, "wc", "refs/heads/agent/x:refs/heads/agent/x"))
	if res.Error != "" || good.bareRev(t, "refs/heads/agent/x") != head {
		t.Fatalf("push with the job's URL = %+v", res)
	}
	if _, err := os.Stat(mark); err == nil {
		t.Error("a hook ran")
	}
	// Without a URL: origin's url as written is the good server; the
	// pushurl and the rules point at the evil one, and must not count.
	job := pushJob(good, "wc", "HEAD:refs/heads/agent/y")
	job.URL = ""
	if res := d.run(t, job); res.Error != "" || good.bareRev(t, "refs/heads/agent/y") != head {
		t.Fatalf("push from origin = %+v", res)
	}
	if n := evil.requests.Load() - evilBefore; n != 0 {
		t.Fatalf("%d requests reached the evil server", n)
	}
	// origin itself on another host is refused.
	runGitCLI(t, wc, "config", "remote.origin.url", evil.URL+"/acme/widgets.git")
	if res := d.run(t, job); !strings.Contains(res.Error, "origin") {
		t.Errorf("an origin off the host = %+v", res)
	}
	if evil.requests.Load() != evilBefore {
		t.Fatal("a refused push reached the evil server")
	}
	if _, err := os.Stat(mark); err == nil {
		t.Error("a hook ran")
	}

	// The control: go-git through the named remote goes where the
	// working copy's configuration says.
	r, err := git.PlainOpen(wc)
	if err != nil {
		t.Fatal(err)
	}
	_ = r.Push(&git.PushOptions{
		RemoteName: "origin",
		RefSpecs:   []config.RefSpec{"refs/heads/agent/x:refs/heads/agent/x"},
		Auth:       &githttp.BasicAuth{Username: "x-access-token", Password: gitTestToken},
	})
	if evil.requests.Load() == evilBefore {
		t.Error("the control push through the named remote did not reach the evil server: the poison proves nothing")
	}
}

// Why this test exists: a host that refuses a push may quote the
// credentials it was sent. The push's error must name the refusal and
// hold neither the token nor the proxy secret in any form.
func TestGitPushScrubsCredentials(t *testing.T) {
	srv := newTestGitServer(t)
	srv.seed(t, "acme/widgets", "one")
	d := newGitDaemon(t, "")
	if res := d.run(t, cloneJob(srv, "wc")); res.Error != "" {
		t.Fatalf("clone = %+v", res)
	}
	job := pushJob(srv, "wc", "refs/heads/main:refs/heads/agent/x")
	job.Auth.Password = gitTestToken + "-wrong"
	res := d.run(t, job)
	if !strings.Contains(res.Error, "push:") || !strings.Contains(res.Error, GitTokenPlaceholder) {
		t.Fatalf("a refused token = %q", res.Error)
	}
	for _, leak := range []string{gitTestToken, base64.StdEncoding.EncodeToString([]byte("x-access-token:" + gitTestToken + "-wrong"))} {
		if strings.Contains(res.Error, leak) {
			t.Errorf("%q holds %q", res.Error, leak)
		}
	}
}

// pushedPacks is how many receive-pack POSTs, the requests that carry
// a push's objects and ref updates, srv was sent: a push refused
// before them wrote nothing.
func (s *testGitServer) pushedPacks() int32 { return s.receivePacks.Load() }

// Why this test exists: GitHub answers a renamed or transferred
// repository with a 301. A push that followed it would write the
// owner's token and commits to a repository core never checked; it
// must fail with the words clone and fetch use, and nothing may reach
// the repository the redirect names.
func TestGitPushRefusesRedirects(t *testing.T) {
	srv := newTestGitServer(t)
	srv.seed(t, "acme/widgets", "one")
	srv.redirects = map[string]string{"/acme/old.git": "/acme/widgets.git"}
	d := newGitDaemon(t, "")
	if res := d.run(t, cloneJob(srv, "wc")); res.Error != "" {
		t.Fatalf("clone = %+v", res)
	}
	before := srv.servedOf("/acme/widgets.git")
	job := pushJob(srv, "wc", "refs/heads/main:refs/heads/agent/x")
	job.URL = srv.URL + "/acme/old.git"
	if res := d.run(t, job); res.Error != gitMovedError || len(res.Updated) != 0 {
		t.Errorf("push = %+v", res)
	}
	if n := srv.servedOf("/acme/widgets.git") - before; n != 0 {
		t.Errorf("%d requests reached the repository the redirect named", n)
	}
}

// Why this test exists: go-git checks a tag's move like a branch's, so
// without this a lightweight tag could be moved forward without force,
// which git's own client refuses ("already exists") and hosts accept:
// a release tag rewritten by an agent the owner never let force. An
// existing tag must not move without + and the job's Force, refused
// before anything is sent; with both it moves.
func TestGitPushNeverMovesATagWithoutForce(t *testing.T) {
	srv := newTestGitServer(t)
	srv.seed(t, "acme/widgets", "one")
	d := newGitDaemon(t, "")
	if res := d.run(t, cloneJob(srv, "wc")); res.Error != "" {
		t.Fatalf("clone = %+v", res)
	}
	wc := filepath.Join(d.root, "wc")
	v1 := srv.bareRev(t, "refs/tags/v1")
	head := commit(t, wc, "two")
	packs := srv.pushedPacks()
	res := d.run(t, pushJob(srv, "wc", "HEAD:refs/tags/v1"))
	if res.Error != "refs/tags/v1 exists; moving a tag needs force" || len(res.Updated) != 0 {
		t.Errorf("a tag moved forward without force = %+v", res)
	}
	if srv.bareRev(t, "refs/tags/v1") != v1 || srv.pushedPacks() != packs+1 {
		t.Errorf("v1 is %s; %d receive-pack requests, want only the advertisement", srv.bareRev(t, "refs/tags/v1"), srv.pushedPacks()-packs)
	}
	job := pushJob(srv, "wc", "+HEAD:refs/tags/v1")
	job.Force = true
	res = d.run(t, job)
	if res.Error != "" || len(res.Updated) != 1 || res.Updated[0] != (GitRefUpdate{Ref: "refs/tags/v1", Old: v1, New: head}) {
		t.Errorf("a forced tag move = %+v", res)
	}
	if srv.bareRev(t, "refs/tags/v1") != head {
		t.Error("v1 did not move")
	}
}

// writeLooseObject stores content as a commit object of wc under the
// name name, whatever content hashes to, as an agent can.
func writeLooseObject(t *testing.T, wc string, name plumbing.Hash, content []byte) {
	t.Helper()
	var b bytes.Buffer
	z := zlib.NewWriter(&b)
	_, _ = fmt.Fprintf(z, "commit %d\x00", len(content))
	_, _ = z.Write(content)
	_ = z.Close()
	h := name.String()
	dir := filepath.Join(wc, ".git", "objects", h[:2])
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, h[2:]), b.Bytes(), 0o444); err != nil {
		t.Fatal(err)
	}
}

// Why this test exists: without force a push may only move a branch
// forward, and that rests on an ancestry walk through the working
// copy's objects, which the agent writes. A commit stored under a name
// its content does not hash to, claiming the remote's tip as its
// parent, would pass go-git's walk and overwrite the remote's history.
// The daemon's own walk checks each object against its name and must
// refuse that before the push is sent; an honest fast-forward must
// pass; and a push that is not a fast-forward must be refused by the
// daemon's own check, not only by go-git's.
func TestGitPushChecksTheObjectsItWalks(t *testing.T) {
	srv := newTestGitServer(t)
	srv.seed(t, "acme/widgets", "one")
	d := newGitDaemon(t, "")
	if res := d.run(t, cloneJob(srv, "wc")); res.Error != "" {
		t.Fatalf("clone = %+v", res)
	}
	wc := filepath.Join(d.root, "wc")
	if res := d.run(t, pushJob(srv, "wc", "refs/heads/main:refs/heads/agent/x")); res.Error != "" {
		t.Fatalf("push = %+v", res)
	}
	tip := srv.bareRev(t, "refs/heads/agent/x")
	tree := runGitCLI(t, wc, "rev-parse", "HEAD^{tree}")

	content := []byte("tree " + tree + "\nparent " + tip + "\nauthor T <t@example.com> 0 +0000\ncommitter T <t@example.com> 0 +0000\n\nforged\n")
	forged := plumbing.ComputeHash(plumbing.CommitObject, append(append([]byte(nil), content...), 'x'))
	writeLooseObject(t, wc, forged, content)
	// Named by its hash: refused as the source is read, before any
	// request.
	packs := srv.pushedPacks()
	res := d.run(t, pushJob(srv, "wc", forged.String()+":refs/heads/agent/x"))
	if res.Error != errObjectMismatch.Error() || len(res.Updated) != 0 || srv.pushedPacks() != packs {
		t.Errorf("a forged commit by hash = %+v after %d receive-pack requests", res, srv.pushedPacks()-packs)
	}
	// Named by a ref, which go-git pushes by name: the daemon's walk
	// reads the forged commit and refuses it, after the advertisement
	// and before the push.
	if err := os.WriteFile(filepath.Join(wc, ".git", "refs", "heads", "forged"), []byte(forged.String()+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	res = d.run(t, pushJob(srv, "wc", "refs/heads/forged:refs/heads/agent/x"))
	if res.Error != errObjectMismatch.Error() || len(res.Updated) != 0 {
		t.Errorf("a forged commit by ref = %+v", res)
	}
	if srv.pushedPacks() != packs+1 || srv.bareRev(t, "refs/heads/agent/x") != tip {
		t.Errorf("the forged push reached the host: %d receive-pack requests", srv.pushedPacks()-packs)
	}

	// An honest fast-forward passes.
	next := commit(t, wc, "two")
	if res := d.run(t, pushJob(srv, "wc", "HEAD:refs/heads/agent/x")); res.Error != "" || srv.bareRev(t, "refs/heads/agent/x") != next {
		t.Fatalf("a fast-forward = %+v", res)
	}
	// Not a fast-forward: refused by the daemon's walk before the push.
	runGitCLI(t, wc, "reset", "-q", "--hard", "HEAD~1")
	commit(t, wc, "other")
	packs = srv.pushedPacks()
	res = d.run(t, pushJob(srv, "wc", "HEAD:refs/heads/agent/x"))
	if !strings.Contains(res.Error, "refs/heads/agent/x: non-fast-forward update; the remote has commits") || srv.pushedPacks() != packs+1 {
		t.Errorf("a non-fast-forward = %+v after %d receive-pack requests", res, srv.pushedPacks()-packs)
	}
}

// Why this test exists: a push of several refs is audited as one
// ("push of a, b: declined"). A host that refused one ref and took the
// other would make that row false and leave the remote half pushed.
// The push must be atomic, so a refusal of one ref takes none; and a
// host that does not take atomic pushes must refuse a push of several
// refs in words the agent can act on, while one ref still goes.
func TestGitPushIsAtomic(t *testing.T) {
	srv := newTestGitServer(t)
	srv.seed(t, "acme/widgets", "one")
	d := newGitDaemon(t, "")
	if res := d.run(t, cloneJob(srv, "wc")); res.Error != "" {
		t.Fatalf("clone = %+v", res)
	}
	bare := filepath.Join(srv.root, "acme/widgets.git")
	hook := filepath.Join(bare, "hooks", "update")
	if err := os.MkdirAll(filepath.Dir(hook), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(hook, []byte("#!/bin/sh\n[ \"$1\" = refs/heads/agent/bad ] && exit 1\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	res := d.run(t, pushJob(srv, "wc", "refs/heads/main:refs/heads/agent/good", "refs/heads/main:refs/heads/agent/bad"))
	if res.Error == "" || len(res.Updated) != 0 {
		t.Errorf("one ref refused = %+v", res)
	}
	if srv.bareRev(t, "refs/heads/agent/good") != "" {
		t.Error("the ref the host would take landed alone")
	}

	runGitCLI(t, bare, "config", "receive.advertiseAtomic", "false")
	res = d.run(t, pushJob(srv, "wc", "refs/heads/main:refs/heads/agent/good", "refs/heads/main:refs/heads/agent/two"))
	if !strings.Contains(res.Error, "does not take atomic pushes") || srv.bareRev(t, "refs/heads/agent/good") != "" {
		t.Errorf("several refs to a host without atomic pushes = %+v", res)
	}
	if res := d.run(t, pushJob(srv, "wc", "refs/heads/main:refs/heads/agent/good")); res.Error != "" || srv.bareRev(t, "refs/heads/agent/good") == "" {
		t.Errorf("one ref to a host without atomic pushes = %+v", res)
	}
}
