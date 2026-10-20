package web

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/kivali-ai/kivali/internal/store"
)

// settleStoreFile backdates the file at rel (under the store root) so its stamp
// is settled: a cache may then trust it until it changes. Files written
// in a test are always too new for that, so without this every cached
// read would be a fresh read and the cache would go untested.
func settleStoreFile(t *testing.T, srv *Server, rel string) {
	t.Helper()
	old := time.Now().Add(-time.Hour)
	if err := os.Chtimes(filepath.Join(srv.Store.Root(), rel), old, old); err != nil {
		t.Fatal(err)
	}
}

// coldServer reads the same store with nothing cached, as a server that
// has just started would.
func coldServer(srv *Server) *Server { return &Server{Store: srv.Store} }

// Needs you and the CEO inbox's history are built from what earlier
// builds read plus what was appended or changed since. If that drifted
// from what the CEO's chat and its message files say, the owner would
// see a request they already answered as still waiting, or miss one
// that arrived, until the server restarted. At every step the cached
// build must equal a cold one. A request moved away after it was cached
// (MoveToRedacted, as a bounce does) must drop out too: only its file
// stamp tells the cache, since the chat row pointing at it stays.
func TestCEOInboxCacheAgreesWithAColdRead(t *testing.T) {
	srv, _ := newTurnServer(t)
	_ = srv.Store.CreateAgent(store.Agent{Slug: "cos", Role: "CoS"}, "# role\n")
	check := func(step string) CEOInboxView {
		t.Helper()
		warm, err := srv.buildCEOInbox(1)
		if err != nil {
			t.Fatalf("%s: cached build: %v", step, err)
		}
		cold, err := coldServer(srv).buildCEOInbox(1)
		if err != nil {
			t.Fatalf("%s: cold build: %v", step, err)
		}
		if !reflect.DeepEqual(warm, cold) {
			t.Fatalf("%s: cached build differs from a cold one\ncached: %+v\ncold:   %+v", step, warm, cold)
		}
		return warm
	}

	a := seedCEOApprovalRequest(t, srv, "cos", "Approve A", "...")
	b := seedCEOApprovalRequest(t, srv, "cos", "Approve B", "...")
	settleStoreFile(t, srv, a)
	settleStoreFile(t, srv, b)
	check("two requests")

	form := strings.NewReader("path=" + b + "&message=ok")
	req := httptest.NewRequest(http.MethodPost, "/api/v1/needs/approve", form)
	req.Header.Set("content-type", "application/x-www-form-urlencoded")
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	rr := httptest.NewRecorder()
	authedHandler(t, srv).ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("approve: code = %d", rr.Code)
	}
	check("B answered")

	c := seedCEOApprovalRequest(t, srv, "cos", "Approve C", "...")
	check("a third request")

	if _, err := srv.Store.MoveToRedacted(filepath.Join(srv.Store.Root(), a)); err != nil {
		t.Fatal(err)
	}
	v := check("A moved away")
	if len(v.Needs) != 1 || v.Needs[0].RequestPath != c {
		t.Errorf("needs = %+v, want only %s", v.Needs, c)
	}
	if len(v.History) != 1 || v.History[0].RequestPath != b {
		t.Errorf("history = %+v, want only %s", v.History, b)
	}
}

// The point of the cache: a message the inbox has read, and whose file
// has not changed since, is not parsed again. Shown by changing the
// file's bytes behind the cache's back while keeping its stamp (size
// and time): the cached build still has the original. Touching the file
// moves its stamp, and the next build reads it again.
func TestCEOInboxParsesASettledMessageOnce(t *testing.T) {
	srv, _ := newTurnServer(t)
	_ = srv.Store.CreateAgent(store.Agent{Slug: "cos", Role: "CoS"}, "# role\n")
	rel := seedCEOApprovalRequest(t, srv, "cos", "Approve A", "...")
	settleStoreFile(t, srv, rel)
	if v, err := srv.buildCEOInbox(1); err != nil || len(v.Needs) != 1 {
		t.Fatalf("first build: %v, %+v", err, v)
	}

	abs := filepath.Join(srv.Store.Root(), rel)
	fi, err := os.Stat(abs)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(abs, bytes.Repeat([]byte("x"), int(fi.Size())), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(abs, fi.ModTime(), fi.ModTime()); err != nil {
		t.Fatal(err)
	}
	v, err := srv.buildCEOInbox(1)
	if err != nil || len(v.Needs) != 1 || v.Needs[0].Request.Title != "Approve A" {
		t.Fatalf("an unchanged stamp was read again: %v, %+v", err, v.Needs)
	}

	now := time.Now()
	if err := os.Chtimes(abs, now, now); err != nil {
		t.Fatal(err)
	}
	if v, _ := srv.buildCEOInbox(1); len(v.Needs) != 0 {
		t.Fatalf("a changed stamp was not read again: needs %+v", v.Needs)
	}
}

// Home refetches the message history whenever any message moves in
// the org, and Home's own count of history threads comes from the same
// grouping. Reading and parsing every message ever written on each
// refresh made that cost grow with the team and its age, so only a
// message's head is kept, read once. The thread grouping from cached
// heads must equal a cold read at every step: a new root, a reply, a
// reply whose parent is missing (its own root), and a root moved away.
func TestHomeHistoryAgreesWithAColdRead(t *testing.T) {
	srv, _ := newTurnServer(t)
	now := time.Now().UTC()
	check := func(step string) []historyThreadRef {
		t.Helper()
		warm, err := srv.messageHeads()
		if err != nil {
			t.Fatalf("%s: cached heads: %v", step, err)
		}
		cold, err := coldServer(srv).messageHeads()
		if err != nil {
			t.Fatalf("%s: cold heads: %v", step, err)
		}
		w, c := groupHistoryThreads(warm, nil), groupHistoryThreads(cold, nil)
		if !reflect.DeepEqual(w, c) {
			t.Fatalf("%s: cached threads differ from a cold read\ncached: %+v\ncold:   %+v", step, w, c)
		}
		return w
	}

	a := writeTaskRequestAt(t, srv, "chief-of-staff", "alice", "root A", now.Add(-3*time.Hour))
	check("one root")
	writeStatusReplyAt(t, srv, "alice", "chief-of-staff", a, now.Add(-2*time.Hour))
	check("a reply")
	b := writeTaskRequestAt(t, srv, "chief-of-staff", "bob", "root B", now.Add(-time.Hour))
	check("a second root")
	orphan := writeStatusReplyAt(t, srv, "bob", "chief-of-staff", "messages/2000-01-01/gone.md", now.Add(-30*time.Minute))
	check("a reply to a missing message")
	if _, err := srv.Store.MoveToRedacted(filepath.Join(srv.Store.Root(), b)); err != nil {
		t.Fatal(err)
	}
	threads := check("B moved away")

	roots := map[string]int{}
	for _, th := range threads {
		roots[th.RootPath] = len(th.ReplyPaths)
	}
	if len(roots) != 2 || roots[a] != 1 || roots[orphan] != 0 {
		t.Errorf("threads = %+v, want %s with its reply and %s on its own", threads, a, orphan)
	}
}

// A message's head is parsed once: changing the file's bytes after it
// was read leaves the cached head in place, which is safe only because
// a message file is never rewritten. A new message still appears on the
// next read.
func TestHomeHistoryParsesEachMessageOnce(t *testing.T) {
	srv, _ := newTurnServer(t)
	now := time.Now().UTC()
	a := writeTaskRequestAt(t, srv, "chief-of-staff", "alice", "root A", now.Add(-time.Hour))
	if _, err := srv.messageHeads(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(srv.Store.Root(), a), []byte("not a message"), 0o644); err != nil {
		t.Fatal(err)
	}
	b := writeTaskRequestAt(t, srv, "chief-of-staff", "bob", "root B", now)
	heads, err := srv.messageHeads()
	if err != nil {
		t.Fatalf("an already-read message was parsed again: %v", err)
	}
	if len(heads) != 2 || heads[0].Path != a || heads[1].Path != b {
		t.Errorf("heads = %+v, want %s then %s", heads, a, b)
	}
}
