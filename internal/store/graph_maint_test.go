package store

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kivali-ai/kivali/internal/clock"
	"github.com/kivali-ai/kivali/internal/graph"
)

// published is a publish landing: the file written, then the
// maintainer told, as artifact_publish does.
func published(t *testing.T, s *FSStore, owner, rel, body string) *graph.Index {
	t.Helper()
	writePublic(t, s, owner, rel, body)
	ix, warnings, err := s.Graph().Published(owner, []string{rel})
	if err != nil || len(warnings) > 0 {
		t.Fatalf("Published %s/%s: %v %v", owner, rel, err, warnings)
	}
	return ix
}

// A publish updates the index before it returns, reading only what it
// names: the version is cut, Index returns it, index.json holds it,
// and no tree is walked.
func TestPublishedUpdatesTheIndexSynchronously(t *testing.T) {
	s := graphStore(t)
	writePublic(t, s, "cs", "old.md", "---\nid: old\n---\nx\n")
	refresh(t, s)
	walks := s.Graph().Walks()

	ix := published(t, s, "vp", "spec.md", "---\nid: spec\nkind: requirement\nabout: vp\n---\nThe spec\n")
	n, ok := ix.Get("vp/spec")
	if !ok || n.CurrentVersion().N != 1 || n.Summary != "The spec" {
		t.Fatalf("after publish: %+v ok=%v", n, ok)
	}
	if s.Graph().Index() != ix {
		t.Error("Index should return the index the publish built")
	}
	ix = published(t, s, "vp", "spec.md", "---\nid: spec\nkind: requirement\nabout: vp\n---\nThe spec, revised\n")
	if n, _ := ix.Get("vp/spec"); n.CurrentVersion().N != 2 || n.Summary != "The spec, revised" {
		t.Fatalf("after republish: %+v", n)
	}
	// Publishing the same bytes again cuts nothing and moves nothing.
	seq := ix.Seq
	if again, _, _ := s.Graph().Published("vp", []string{"spec.md"}); again.Seq != seq {
		t.Errorf("an unchanged republish moved seq %d -> %d", seq, again.Seq)
	}
	onDisk, err := s.ReadGraphIndex()
	if err != nil || onDisk.Seq != seq {
		t.Fatalf("index.json seq = %v, %v; want %d", onDisk, err, seq)
	}
	if _, ok := ix.Get("cs/old"); !ok {
		t.Error("files the publish did not name must stay indexed")
	}
	if got := s.Graph().Walks(); got != walks {
		t.Errorf("publishes walked the published trees %d times", got-walks)
	}
}

// An unpublish is a file vanishing: its node goes, and what rested on
// it is flagged.
func TestPublishedRemovalDropsTheNode(t *testing.T) {
	s := graphStore(t)
	published(t, s, "cs", "a.md", "---\nid: a\n---\nx\n")
	published(t, s, "vp", "d.md", "---\nid: d\ndepends_on: cs/a\n---\nx\n")
	if err := os.Remove(filepath.Join(publicDir(s, "cs"), "a.md")); err != nil {
		t.Fatal(err)
	}
	ix, _, err := s.Graph().Published("cs", []string{"a.md"})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := ix.Get("cs/a"); ok {
		t.Error("an unpublished file must leave the index")
	}
	if d, _ := ix.Get("vp/d"); !d.Flagged {
		t.Errorf("a dependent of a vanished node should be flagged: %+v", d)
	}
}

// The anti-entropy scan, on its ticker, finds an edit nobody reported
// and a file that vanished behind core's back.
func TestScanTickerFindsOutOfBandEdits(t *testing.T) {
	s := graphStore(t)
	writePublic(t, s, "vp", "a.md", "---\nid: a\n---\none\n")
	writePublic(t, s, "vp", "gone.md", "---\nid: gone\n---\nx\n")
	clk := clock.NewFake()
	scanned := make(chan struct{}, 1)
	m := s.Graph()
	m.afterScan = func() { scanned <- struct{}{} }
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	m.Start(ctx, clk)
	<-scanned // the boot scan
	clk.BlockUntil(1)
	if n, ok := m.Index().Get("vp/a"); !ok || n.CurrentVersion().N != 1 {
		t.Fatalf("boot scan: %+v ok=%v", n, ok)
	}

	writePublic(t, s, "vp", "a.md", "---\nid: a\n---\ntwo\n")
	if err := os.Remove(filepath.Join(publicDir(s, "vp"), "gone.md")); err != nil {
		t.Fatal(err)
	}
	if n, _ := m.Index().Get("vp/a"); n.CurrentVersion().N != 1 {
		t.Fatal("nothing should notice before the scan")
	}
	clk.Advance(GraphScanInterval)
	<-scanned
	ix := m.Index()
	if n, _ := ix.Get("vp/a"); n.CurrentVersion().N != 2 {
		t.Errorf("the scan should cut v2: %+v", n.Versions)
	}
	if _, ok := ix.Get("vp/gone"); ok {
		t.Error("the scan should drop a vanished file")
	}
}

// A scan reads outside the maintainer's lock. A publish that lands
// between the scan's plan and its apply wins: the scan never puts back
// bytes older than what the publish read.
func TestScanDoesNotUndoAConcurrentPublish(t *testing.T) {
	s := graphStore(t)
	published(t, s, "vp", "a.md", "---\nid: a\n---\none\n")
	writePublic(t, s, "vp", "a.md", "---\nid: a\n---\ntwo\n")

	m := s.Graph()
	w, err := m.walk(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	m.mu.Lock()
	plan := m.planScanLocked(w, m.gen, m.epoch)
	m.mu.Unlock()
	got, err := m.readScan(context.Background(), plan) // reads "two"
	if err != nil {
		t.Fatal(err)
	}

	published(t, s, "vp", "a.md", "---\nid: a\n---\nthree\n")

	m.mu.Lock()
	ix, _, err := m.applyScanLocked(plan, got)
	m.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	n, _ := ix.Get("vp/a")
	if n.CurrentVersion().N != 2 {
		t.Fatalf("want v2 (the publish of three), got %+v", n.Versions)
	}
	b, _ := os.ReadFile(s.path("attachments", n.CurrentVersion().SHA, "blob.md"))
	if !strings.Contains(string(b), "three") {
		t.Errorf("current version bytes = %q, want the publish's", b)
	}
}

// Likewise an unpublish between the scan's plan and its apply: the
// file the scan read must not come back.
func TestScanDoesNotRestoreAConcurrentUnpublish(t *testing.T) {
	s := graphStore(t)
	published(t, s, "vp", "a.md", "---\nid: a\n---\none\n")
	writePublic(t, s, "vp", "a.md", "---\nid: a\n---\ntwo\n") // so the scan reads it

	m := s.Graph()
	w, err := m.walk(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	m.mu.Lock()
	plan := m.planScanLocked(w, m.gen, m.epoch)
	m.mu.Unlock()
	got, err := m.readScan(context.Background(), plan)
	if err != nil {
		t.Fatal(err)
	}
	if got[graphKey{owner: "vp", rel: "a.md"}] == nil {
		t.Fatal("precondition: the scan read a.md")
	}

	if err := os.Remove(filepath.Join(publicDir(s, "vp"), "a.md")); err != nil {
		t.Fatal(err)
	}
	if _, _, err := m.Published("vp", []string{"a.md"}); err != nil {
		t.Fatal(err)
	}

	m.mu.Lock()
	ix, _, err := m.applyScanLocked(plan, got)
	m.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := ix.Get("vp/a"); ok {
		t.Error("the scan brought back a file unpublished after it planned")
	}
}

// The walk runs without the lock, so a publish can land while it
// lists. The file the walk did not see is newer than the walk, not
// gone: in a tree the walk listed, and in a tree the publish created
// after the walk found none.
func TestScanKeepsAPublishLandingDuringItsWalk(t *testing.T) {
	s := graphStore(t)
	published(t, s, "vp", "a.md", "---\nid: a\n---\none\n")
	m := s.Graph()
	m.afterWalk = func() {
		m.afterWalk = nil
		published(t, s, "vp", "new.md", "---\nid: new\n---\nx\n")
		published(t, s, "cs", "first.md", "---\nid: first\n---\nx\n") // cs had no tree
	}
	ix, _, err := m.scan(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, ix := range []*graph.Index{ix, m.Index()} {
		for _, id := range []string{"vp/a", "vp/new", "cs/first"} {
			if _, ok := ix.Get(id); !ok {
				t.Errorf("%s: the scan dropped a file published during its walk", id)
			}
		}
	}
}

// An unpublish during the walk: the file the walk listed must not come
// back from the scan.
func TestScanDropsAnUnpublishLandingDuringItsWalk(t *testing.T) {
	s := graphStore(t)
	published(t, s, "vp", "a.md", "---\nid: a\n---\none\n")
	writePublic(t, s, "vp", "a.md", "---\nid: a\n---\ntwo\n") // so the scan plans a read
	m := s.Graph()
	m.afterWalk = func() {
		m.afterWalk = nil
		if err := os.Remove(filepath.Join(publicDir(s, "vp"), "a.md")); err != nil {
			t.Error(err)
		}
		if _, _, err := m.Published("vp", []string{"a.md"}); err != nil {
			t.Error(err)
		}
	}
	ix, _, err := m.scan(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := ix.Get("vp/a"); ok {
		t.Error("the scan brought back a file unpublished during its walk")
	}
}

// A Reload during the walk replaced what the walk listed: the scan
// applies nothing, and what the Reload loaded stands.
func TestScanAppliesNothingAfterAReloadDuringItsWalk(t *testing.T) {
	s := graphStore(t)
	published(t, s, "vp", "a.md", "---\nid: a\n---\none\n")
	m := s.Graph()
	m.afterWalk = func() {
		m.afterWalk = nil
		if err := os.RemoveAll(publicDir(s, "vp")); err != nil {
			t.Error(err)
		}
		writePublic(t, s, "vp", "b.md", "---\nid: b\n---\nb\n")
		if err := m.Reload(); err != nil {
			t.Error(err)
		}
	}
	if _, _, err := m.scan(context.Background()); err != nil {
		t.Fatal(err)
	}
	ix := m.Index()
	if _, ok := ix.Get("vp/b"); !ok {
		t.Error("a scan planned before a Reload removed what the Reload loaded")
	}
	if _, ok := ix.Get("vp/a"); ok {
		t.Error("vp/a is gone from disk")
	}
}

// Once started, nothing but the boot scan walks: before it applies,
// Index answers from what Start loaded (nothing, on a fresh install)
// and a publish waits for it rather than walking with the lock held.
func TestBeforeTheBootScanNothingElseWalks(t *testing.T) {
	s := graphStore(t)
	writePublic(t, s, "vp", "a.md", "---\nid: a\n---\nx\n")
	m := s.Graph()
	walked, hold := make(chan struct{}), make(chan struct{})
	m.afterWalk = func() {
		m.afterWalk = nil
		close(walked)
		<-hold
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	m.Start(ctx, clock.NewFake())
	<-walked // the boot scan has walked and not applied

	if ix := m.Index(); ix != nil {
		t.Errorf("Index before the boot scan on a fresh install = seq %d, want nil", ix.Seq)
	}
	writePublic(t, s, "vp", "b.md", "---\nid: b\n---\nx\n")
	type result struct {
		ix  *graph.Index
		err error
	}
	done := make(chan result)
	go func() {
		ix, _, err := m.Published("vp", []string{"b.md"})
		done <- result{ix, err}
	}()
	close(hold)
	r := <-done
	if r.err != nil {
		t.Fatal(r.err)
	}
	for _, id := range []string{"vp/a", "vp/b"} {
		if _, ok := r.ix.Get(id); !ok {
			t.Errorf("%s missing from the index the publish returned", id)
		}
	}
	if got := m.Walks(); got != 1 {
		t.Errorf("walks = %d, want only the boot scan's", got)
	}
}

// Hires, archives and reporting-line changes reach the index from the
// store's own mutators, without a walk.
func TestOrgChangesRebuildWithoutAWalk(t *testing.T) {
	s := graphStore(t)
	published(t, s, "cs", "a.md", "---\nid: a\n---\nx\n")
	walks := s.Graph().Walks()

	if err := s.CreateAgent(Agent{Slug: "eng", Role: "Engineer", ReportsTo: "vp"}, "# role\n"); err != nil {
		t.Fatal(err)
	}
	if n, ok := s.Graph().Index().Get("eng"); !ok || n.ReportsTo != "vp" {
		t.Fatalf("hire: %+v ok=%v", n, ok)
	}
	if err := s.SetAgentReportsTo("eng", "ceo"); err != nil {
		t.Fatal(err)
	}
	if n, _ := s.Graph().Index().Get("eng"); n.ReportsTo != "ceo" {
		t.Errorf("reporting line: %+v", n)
	}
	if err := s.ArchiveAgent("cs"); err != nil {
		t.Fatal(err)
	}
	ix := s.Graph().Index()
	if n, _ := ix.Get("cs"); n.Status != graph.StatusArchived {
		t.Errorf("archive: %+v", n)
	}
	if _, ok := ix.Get("cs/a"); !ok {
		t.Error("an archived owner's nodes stay")
	}
	if got := s.Graph().Walks(); got != walks {
		t.Errorf("org changes walked the published trees %d times", got-walks)
	}
}

// Project-file changes rebuild from the project-file index.
func TestProjectFilesChangedRebuilds(t *testing.T) {
	s := graphStore(t)
	refresh(t, s)
	pf, err := s.AddProjectFile("plan.md", strings.NewReader("v1"))
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := s.Graph().Index().Get("ceo/plan"); ok {
		t.Fatal("precondition: the upload is not indexed until the maintainer hears of it")
	}
	s.Graph().ProjectFilesChanged()
	n, ok := s.Graph().Index().Get("ceo/plan")
	if !ok || n.CurrentVersion().SHA != pf.SHA {
		t.Fatalf("after ProjectFilesChanged: %+v ok=%v", n, ok)
	}
	if err := s.SetProjectFileSummary(pf.SHA, "The plan"); err != nil {
		t.Fatal(err)
	}
	s.Graph().ProjectFilesChanged()
	if n, _ := s.Graph().Index().Get("ceo/plan"); n.Summary != "The plan" {
		t.Errorf("summary: %+v", n)
	}
}

// Reload forgets the cache and loads what is on disk now: a restore
// replaced the published trees and the version log underneath.
func TestReloadLoadsWhatIsOnDiskNow(t *testing.T) {
	s := graphStore(t)
	published(t, s, "vp", "a.md", "---\nid: a\n---\none\n")
	published(t, s, "vp", "a.md", "---\nid: a\n---\ntwo\n")

	// Swap the data underneath: a different tree, no index, no log.
	if err := os.RemoveAll(publicDir(s, "vp")); err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{"index.json", "versions.jsonl"} {
		if err := os.Remove(s.path("graph", f)); err != nil {
			t.Fatal(err)
		}
	}
	writePublic(t, s, "vp", "b.md", "---\nid: b\n---\nb\n")
	if err := s.Graph().Reload(); err != nil {
		t.Fatal(err)
	}
	ix := s.Graph().Index()
	if _, ok := ix.Get("vp/a"); ok {
		t.Error("a file the restore removed must not survive a reload")
	}
	if n, ok := ix.Get("vp/b"); !ok || n.CurrentVersion().N != 1 {
		t.Errorf("the restored file: %+v ok=%v", n, ok)
	}
	if onDisk, err := s.ReadGraphIndex(); err != nil || onDisk.Seq != ix.Seq {
		t.Errorf("index.json after reload: %v %v", onDisk, err)
	}
}

// Index never walks once loaded: a burst of reads is a pointer load.
func TestIndexReadsDoNotWalk(t *testing.T) {
	s := graphStore(t)
	writePublic(t, s, "vp", "a.md", "---\nid: a\n---\nx\n")
	first := s.Graph().Index() // the first read loads and scans
	walks := s.Graph().Walks()
	for i := 0; i < 5; i++ {
		if s.Graph().Index() != first {
			t.Fatal("Index changed with nothing changing")
		}
	}
	if got := s.Graph().Walks(); got != walks {
		t.Errorf("reads walked %d times", got-walks)
	}
}

// A publish writes a temp file and renames it into place. A rewrite of
// the same size that keeps the mtime (inside the filesystem's
// granularity) is still a new inode: the scan reads it, and does not
// take the old snapshot's hint for its bytes.
func TestScanSeesASameSizeSameMtimeReplacement(t *testing.T) {
	s := graphStore(t)
	writePublic(t, s, "vp", "data.bin", "aaaa")
	refresh(t, s)
	p := filepath.Join(publicDir(s, "vp"), "data.bin")
	before, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	tmp := filepath.Join(publicDir(s, "vp"), ".data.bin.tmp")
	if err := os.WriteFile(tmp, []byte("bbbb"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(tmp, before.ModTime(), before.ModTime()); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(tmp, p); err != nil {
		t.Fatal(err)
	}
	ix := refresh(t, s)
	n := nodeForPublishedFile(ix, "vp", "data.bin")
	if n == nil {
		t.Fatal("no node for vp/data.bin")
	}
	v := n.CurrentVersion()
	b, _ := os.ReadFile(s.path("attachments", v.SHA, "blob.bin"))
	if v.N != 2 || string(b) != "bbbb" {
		t.Errorf("after a same-size, same-mtime replacement: v%d bytes %q, want v2 %q", v.N, b, "bbbb")
	}
}

// A payload a scan could not open is cached with the walk's stat, no
// snapshot and no read error. The next scan reads it again, rather
// than leaving it to every rebuild to retry the snapshot.
func TestScanRereadsAFileWithNoSnapshot(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("permission bits do not bind root")
	}
	s := graphStore(t)
	writePublic(t, s, "vp", "data.bin", "bytes")
	p := filepath.Join(publicDir(s, "vp"), "data.bin")
	if err := os.Chmod(p, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(p, 0o644) })
	m := s.Graph()
	if _, _, err := m.scan(context.Background()); err != nil {
		t.Fatal(err)
	}
	k := graphKey{owner: "vp", rel: "data.bin"}
	m.mu.Lock()
	e := m.files[k]
	m.mu.Unlock()
	if e == nil || e.sha != "" || e.readErr != nil {
		t.Fatalf("precondition: an unopened payload is cached with no sha and no readErr: %+v", e)
	}
	if err := os.Chmod(p, 0o644); err != nil {
		t.Fatal(err)
	}
	w, err := m.walk(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	m.mu.Lock()
	plan := m.planScanLocked(w, m.gen, m.epoch)
	m.mu.Unlock()
	if _, ok := plan.read[k]; !ok {
		t.Error("the scan should plan to read a file with no snapshot")
	}
}
