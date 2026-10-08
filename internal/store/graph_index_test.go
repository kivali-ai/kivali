package store

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kivali-ai/kivali/internal/files"
	"github.com/kivali-ai/kivali/internal/graph"
)

func graphStore(t *testing.T) *FSStore {
	t.Helper()
	s := mustStore(t)
	for _, a := range []Agent{
		{Slug: "chief-of-staff", Role: "Chief of Staff", ReportsTo: "ceo"},
		{Slug: "vp", Role: "Engineering lead", ReportsTo: "ceo"},
		{Slug: "cs", Role: "Chief Scientist", ReportsTo: "vp"},
	} {
		if err := s.CreateAgent(a, "# role\n"); err != nil {
			t.Fatalf("CreateAgent %s: %v", a.Slug, err)
		}
	}
	return s
}

func publicDir(s *FSStore, slug string) string {
	return files.PublishedDir(s.root, slug)
}

func writePublic(t *testing.T, s *FSStore, slug, rel, body string) {
	t.Helper()
	p := filepath.Join(publicDir(s, slug), filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	// A distinct mtime per write, so the scan sees a rewrite of the
	// same size even where the filesystem's clock is coarser than two
	// writes in a row.
	mt := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC).Add(time.Duration(publicWrites.Add(1)) * time.Second)
	if err := os.Chtimes(p, mt, mt); err != nil {
		t.Fatal(err)
	}
}

var publicWrites atomic.Int64

// scanGraph is an out-of-band edit being noticed: the anti-entropy
// scan, returning what it built.
func scanGraph(s *FSStore) (*graph.Index, []string, error) {
	return s.Graph().scan(context.Background())
}

// restartGraph is a process restart: the maintainer forgets what it
// knew and loads everything from disk again.
func restartGraph(s *FSStore) (*graph.Index, []string, error) {
	warnings, err := s.Graph().reload()
	return s.Graph().Index(), warnings, err
}

func refresh(t *testing.T, s *FSStore) *graph.Index {
	t.Helper()
	ix, warnings, err := scanGraph(s)
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	if len(warnings) > 0 {
		t.Fatalf("scan warnings: %v", warnings)
	}
	return ix
}

func versionLines(t *testing.T, s *FSStore) []string {
	t.Helper()
	b, err := os.ReadFile(s.path("graph", "versions.jsonl"))
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		t.Fatal(err)
	}
	return strings.Split(strings.TrimSpace(string(b)), "\n")
}

func TestRefreshGraphIndexesPublicFilesAndVersionsThem(t *testing.T) {
	s := graphStore(t)
	writePublic(t, s, "vp", "release-plan.md", "---\nid: release-plan\n---\n# Release plan\n")
	writePublic(t, s, "vp", "reqs/phase.md", "---\nid: phase\nkind: requirement\nabout: vp/release-plan\n---\nEvery release has notes.\n")
	writePublic(t, s, "cs", "notes.md", "plain notes, no front matter\n")
	writePublic(t, s, "cs", ".hidden.md", "---\nid: hidden\n---\nignored\n")

	ix := refresh(t, s)
	if ix.Seq != 1 {
		t.Errorf("first pass seq = %d", ix.Seq)
	}
	for _, id := range []string{"ceo", "chief-of-staff", "vp", "cs", "vp/release-plan", "vp/phase", "cs/notes"} {
		if _, ok := ix.Get(id); !ok {
			t.Errorf("missing node %s", id)
		}
	}
	if _, ok := ix.Get("cs/hidden"); ok {
		t.Error("dotfiles must not be indexed")
	}
	phase, _ := ix.Get("vp/phase")
	if phase.Kind != graph.KindRequirement || phase.About == nil || !phase.About.Resolved {
		t.Errorf("phase = %+v", phase)
	}
	if v := phase.CurrentVersion(); v.N != 1 || v.SHA == "" {
		t.Errorf("phase version = %+v", v)
	}
	if got := len(versionLines(t, s)); got != 3 {
		t.Errorf("versions.jsonl should have one line per artifact, got %d", got)
	}
	// The snapshot is in the attachment store under the version's SHA.
	if _, err := s.GetAttachment(phase.CurrentVersion().SHA); err != nil {
		t.Errorf("snapshot for %s missing: %v", phase.ID, err)
	}
	// Persisted and readable back.
	again, err := s.ReadGraphIndex()
	if err != nil || again.Seq != ix.Seq || len(again.Nodes) != len(ix.Nodes) {
		t.Fatalf("ReadGraphIndex = %v, %v", again, err)
	}

	// A second pass over unchanged files: same seq, no new versions.
	second := refresh(t, s)
	if second.Seq != 1 || len(versionLines(t, s)) != 3 {
		t.Errorf("unchanged pass: seq %d, %d version lines", second.Seq, len(versionLines(t, s)))
	}

	// Edit the manifest: version 2, seq advances, node listed as changed.
	writePublic(t, s, "vp", "reqs/phase.md", "---\nid: phase\nkind: requirement\nabout: vp/release-plan\nstatus: withdrawn\n---\nWithdrawn.\n")
	third := refresh(t, s)
	phase, _ = third.Get("vp/phase")
	if third.Seq != 2 || phase.CurrentVersion().N != 2 || phase.Status != graph.StatusWithdrawn {
		t.Errorf("after edit: seq %d, version %d, status %s", third.Seq, phase.CurrentVersion().N, phase.Status)
	}
	if changed := third.ChangedSince(1); len(changed) != 1 || changed[0].ID != "vp/phase" {
		t.Errorf("changed since 1 = %v", changed)
	}
	if len(versionLines(t, s)) != 4 {
		t.Errorf("one new version line expected, have %d", len(versionLines(t, s)))
	}
}

func TestRefreshGraphDeclaredIdKeepsVersionsAcrossRename(t *testing.T) {
	s := graphStore(t)
	writePublic(t, s, "vp", "old-name.md", "---\nid: stable\n---\none\n")
	refresh(t, s)
	writePublic(t, s, "vp", "old-name.md", "---\nid: stable\n---\ntwo\n")
	refresh(t, s)
	if err := os.Rename(filepath.Join(publicDir(s, "vp"), "old-name.md"), filepath.Join(publicDir(s, "vp"), "new-name.md")); err != nil {
		t.Fatal(err)
	}
	ix := refresh(t, s)
	n, ok := ix.Get("vp/stable")
	if !ok {
		t.Fatalf("renamed node missing; have %v", ix.Sorted())
	}
	if n.Path != "new-name.md" || n.CurrentVersion().N != 2 {
		t.Errorf("after rename: path %q version %d (rename alone is not a new version)", n.Path, n.CurrentVersion().N)
	}
}

func TestRefreshGraphPayloadVersions(t *testing.T) {
	s := graphStore(t)
	writePublic(t, s, "vp", "rel/v3.md", "---\nid: v3\nfile: v3.zip\n---\nRelease 3.\n")
	writePublic(t, s, "vp", "rel/v3.zip", "not really a zip, v1")
	ix := refresh(t, s)
	n, _ := ix.Get("vp/v3")
	if n == nil || n.Payload != "rel/v3.zip" || n.CurrentVersion().PayloadSHA == "" {
		t.Fatalf("v3 = %+v", n)
	}
	if _, folded := ix.Get("vp/rel/v3.zip"); folded {
		t.Error("payload must not be its own node")
	}
	if _, err := s.GetAttachment(n.CurrentVersion().PayloadSHA); err != nil {
		t.Errorf("payload snapshot missing: %v", err)
	}
	// Payload-only change cuts a version even though the manifest is unchanged.
	writePublic(t, s, "vp", "rel/v3.zip", "not really a zip, v2")
	ix = refresh(t, s)
	n, _ = ix.Get("vp/v3")
	if n.CurrentVersion().N != 2 || n.Versions[0].PayloadSHA == n.Versions[1].PayloadSHA || n.Versions[0].SHA != n.Versions[1].SHA {
		t.Errorf("payload change: versions = %+v", n.Versions)
	}
}

func TestRefreshGraphProjectFilesAreCEONodes(t *testing.T) {
	s := graphStore(t)
	pf1, err := s.AddProjectFile("Business Plan.md", strings.NewReader("plan v1"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.AddProjectFile("Business Plan.md", strings.NewReader("plan v2")); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AddProjectFile("datasheet.pdf", strings.NewReader("%PDF-1.4 pretend")); err != nil {
		t.Fatal(err)
	}
	ix := refresh(t, s)
	// Markdown ids drop the extension whether or not the file was read
	// as a manifest; project files never are.
	plan, ok := ix.Get("ceo/business_plan")
	if !ok {
		t.Fatalf("project node missing; have %v", ix.Query(graph.Filter{Owner: "ceo"}))
	}
	if plan.Kind != graph.KindReference || plan.Owner != "ceo" || len(plan.Versions) != 2 || plan.Versions[0].SHA != pf1.SHA {
		t.Errorf("plan = %+v", plan)
	}
	if plan.Source != graphProjectFileSource {
		t.Errorf("source = %q", plan.Source)
	}
	if err := s.SetProjectFileKind(pf1.SHA, "bogus"); err == nil {
		t.Error("unknown kind must be refused")
	}
	// The kind is read from the newest upload of that name.
	pfs, _ := s.ListProjectFiles()
	for _, pf := range pfs {
		if pf.OriginalName == "datasheet.pdf" {
			if err := s.SetProjectFileKind(pf.SHA, ""); err != nil {
				t.Fatal(err)
			}
		}
	}
	// Project files version through the same append-only log as every
	// other node, seeded oldest-first the first time a name is seen.
	known, err := s.readGraphVersions()
	if err != nil {
		t.Fatal(err)
	}
	if got := known.byID["ceo/business_plan"]; len(got) != 2 || got[0].SHA != pf1.SHA {
		t.Errorf("project versions in the log = %+v", got)
	}
	// The node's Path is the newest upload's link, which the sync farm
	// SHA-prefixes on a same-name re-upload; the id did not move.
	if !strings.HasSuffix(plan.Path, "-Business_Plan.md") {
		t.Errorf("path should be the newest upload's link: %q", plan.Path)
	}
}

// Deleting an old upload must not renumber the survivors: a
// certificate pinned to ceo/plan@2 still means the same bytes.
func TestProjectFileVersionsSurviveDeletion(t *testing.T) {
	s := graphStore(t)
	pf1, err := s.AddProjectFile("plan.md", strings.NewReader("v1"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.AddProjectFile("plan.md", strings.NewReader("v2")); err != nil {
		t.Fatal(err)
	}
	writePublic(t, s, "vp", "cert.md", "---\nkind: certificate\nabout: ceo/plan\ndepends_on: ceo/plan@2\n---\nok\n")
	ix := refresh(t, s)
	if c, _ := ix.Get("vp/cert"); c == nil || !c.DependsOn[0].Resolved {
		t.Fatalf("precondition: @2 resolves: %+v", c)
	}
	if err := s.RemoveProjectFile(pf1.SHA); err != nil {
		t.Fatal(err)
	}
	ix = refresh(t, s)
	if c, _ := ix.Get("vp/cert"); !c.DependsOn[0].Resolved {
		t.Error("deleting an older upload renumbered the survivor; @2 dangles")
	}
	if n, _ := ix.Get("ceo/plan"); n == nil || n.CurrentVersion().N != 2 {
		t.Errorf("plan = %+v", n)
	}
}

// Two different documents whose names sanitise alike are two nodes,
// the second under its SHA-prefixed link, exactly as /files/project/
// shows them; neither is dropped or merged.
func TestDistinctProjectFilesWithLikeNamesStayApart(t *testing.T) {
	s := graphStore(t)
	if _, err := s.AddProjectFile("Q3 plan.md", strings.NewReader("a")); err != nil {
		t.Fatal(err)
	}
	pf2, err := s.AddProjectFile("Q3_plan.md", strings.NewReader("b"))
	if err != nil {
		t.Fatal(err)
	}
	ix := refresh(t, s)
	first, ok := ix.Get("ceo/q3_plan")
	if !ok || len(first.Versions) != 1 || first.Path != "Q3_plan.md" {
		t.Errorf("first document = %+v", first)
	}
	second, ok := ix.Get("ceo/" + graph.NameForPath(pf2.SHA+"-Q3_plan.md"))
	if !ok || len(second.Versions) != 1 || second.Versions[0].SHA != pf2.SHA {
		t.Errorf("second document = %+v (have %v)", second, ix.Query(graph.Filter{Owner: "ceo"}))
	}
	if d := ix.Dropped["ceo"]; len(d) != 0 {
		t.Errorf("a CEO upload fell out of the graph: %v", d)
	}
}

func TestRefreshGraphKeepsArchivedOwnersNodes(t *testing.T) {
	s := graphStore(t)
	writePublic(t, s, "cs", "analysis.md", "---\nid: analysis\n---\nx\n")
	writePublic(t, s, "vp", "d.md", "---\nkind: decision\nabout: vp\ndepends_on: cs/analysis\n---\nx\n")
	refresh(t, s)
	if err := s.ArchiveAgent("cs"); err != nil {
		t.Fatal(err)
	}
	ix := refresh(t, s)
	if n, ok := ix.Get("cs/analysis"); !ok || n.Owner != "cs" {
		t.Fatalf("archived owner's node should survive: %v %v", n, ok)
	}
	if cs, _ := ix.Get("cs"); cs.Status != graph.StatusArchived {
		t.Errorf("cs status = %s", cs.Status)
	}
	if d, _ := ix.Get("vp/d"); d.Flagged || !d.DependsOn[0].Resolved {
		t.Errorf("edge into an archived owner's node still resolves: %+v", d)
	}
	// Archiving moved the agent's record, not its published tree: peers
	// keep reading it at shared/cs/ (the mount of public/).
	if b, err := os.ReadFile(filepath.Join(publicDir(s, "cs"), "analysis.md")); err != nil || !strings.Contains(string(b), "id: analysis") {
		t.Errorf("public/cs/analysis.md after archive: %q, %v", b, err)
	}
	if n, _ := ix.Get("cs/analysis"); n.Path != "analysis.md" {
		t.Errorf("archived node path = %q", n.Path)
	}
}

func TestGraphWatermarkRoundTrip(t *testing.T) {
	s := graphStore(t)
	if _, ok, err := s.ReadGraphWatermark("vp"); ok || err != nil {
		t.Fatalf("fresh agent: ok=%v err=%v", ok, err)
	}
	if err := s.WriteGraphWatermark("vp", GraphWatermark{Seq: 7, Fingerprints: map[string]string{"handbook": "abc"}}); err != nil {
		t.Fatal(err)
	}
	wm, ok, err := s.ReadGraphWatermark("vp")
	if err != nil || !ok || wm.Seq != 7 || wm.Fingerprints["handbook"] != "abc" || wm.UpdatedAt.IsZero() {
		t.Errorf("watermark = %+v ok=%v err=%v", wm, ok, err)
	}
	if err := s.WriteGraphWatermark("nobody", GraphWatermark{}); err == nil {
		t.Error("unknown agent should be ErrNotFound")
	}
}

// A crash between the write and the fsync of versions.jsonl can leave a
// torn last line. The pass must skip it and heal: the version it
// described is cut again on the next pass, and the snapshot it points
// at already exists.
func TestRefreshGraphHealsATornVersionsLog(t *testing.T) {
	s := graphStore(t)
	writePublic(t, s, "vp", "a.md", "---\nid: a\n---\none\n")
	refresh(t, s)
	writePublic(t, s, "vp", "a.md", "---\nid: a\n---\ntwo\n")
	refresh(t, s)
	lines := versionLines(t, s)
	if len(lines) != 2 {
		t.Fatalf("want 2 version lines, got %d", len(lines))
	}
	// Tear the last line in half and drop the index, as a crash would.
	torn := lines[0] + "\n" + lines[1][:len(lines[1])/2]
	if err := os.WriteFile(s.path("graph", "versions.jsonl"), []byte(torn), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(s.path("graph", "index.json")); err != nil {
		t.Fatal(err)
	}
	ix, warnings, err := restartGraph(s)
	if err != nil || len(warnings) > 0 {
		t.Fatalf("restart: %v %v", err, warnings)
	}
	n, _ := ix.Get("vp/a")
	if n == nil || n.CurrentVersion().N != 2 {
		t.Fatalf("after healing: %+v", n)
	}
	// The torn half-line stays (the log is never rewritten), the
	// re-cut record lands on its own line after it, and the log parses
	// to exactly the two versions that exist.
	known, err := s.readGraphVersions()
	if err != nil {
		t.Fatal(err)
	}
	if got := known.byID["vp/a"]; len(got) != 2 || got[1].N != 2 {
		t.Errorf("healed log should parse to two versions of vp/a, got %+v", got)
	}
	if _, err := s.GetAttachment(n.CurrentVersion().SHA); err != nil {
		t.Errorf("snapshot missing after heal: %v", err)
	}
	// And a third pass changes nothing.
	again := refresh(t, s)
	if again.Seq != ix.Seq {
		t.Errorf("a healed log must not keep cutting versions: seq %d -> %d", ix.Seq, again.Seq)
	}
}

// A version is recorded only once its bytes are stored. When the
// store cannot take the snapshot, no record is cut and the next pass
// tries again; when a snapshot goes missing later, it is taken again.
func TestVersionsAreRecordedOnlyWithTheirSnapshot(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("permission bits do not bind root")
	}
	s := graphStore(t)
	writePublic(t, s, "vp", "r.md", "---\nid: r\nfile: r.bin\n---\nx\n")
	writePublic(t, s, "vp", "r.bin", "payload bytes")
	if err := os.Chmod(s.path("attachments"), 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(s.path("attachments"), 0o755) })
	ix, warnings, err := scanGraph(s)
	if err != nil {
		t.Fatalf("a snapshot failure must not fail the pass: %v", err)
	}
	if len(warnings) == 0 {
		t.Fatal("expected snapshot warnings")
	}
	if n, _ := ix.Get("vp/r"); n == nil || len(n.Versions) != 0 {
		t.Errorf("no version may be recorded without its snapshot: %+v", n)
	}
	if err := os.Chmod(s.path("attachments"), 0o755); err != nil {
		t.Fatal(err)
	}
	ix = refresh(t, s)
	n, _ := ix.Get("vp/r")
	if n == nil || n.CurrentVersion().N != 1 {
		t.Fatalf("after the store is writable again: %+v", n)
	}
	for _, sha := range []string{n.CurrentVersion().SHA, n.CurrentVersion().PayloadSHA} {
		if _, err := s.GetAttachment(sha); err != nil {
			t.Errorf("snapshot %s missing: %v", sha, err)
		}
	}
	// A snapshot removed behind the pass's back comes back on the next
	// pass, without a new version.
	if err := os.RemoveAll(s.path("attachments", n.CurrentVersion().PayloadSHA)); err != nil {
		t.Fatal(err)
	}
	ix = refresh(t, s)
	n, _ = ix.Get("vp/r")
	if n.CurrentVersion().N != 1 {
		t.Errorf("restoring a snapshot must not cut a version: %+v", n.Versions)
	}
	if _, err := s.GetAttachment(n.CurrentVersion().PayloadSHA); err != nil {
		t.Errorf("payload snapshot not restored: %v", err)
	}
}

// Snapshots keep bytes, not text: a PDF payload must not run the
// canonicaliser under the pass's lock.
func TestSnapshotsSkipCanonicalisation(t *testing.T) {
	s := graphStore(t)
	writePublic(t, s, "vp", "spec.md", "---\nid: spec\nfile: spec.pdf\n---\nx\n")
	writePublic(t, s, "vp", "spec.pdf", "%PDF-1.4 not really")
	ix := refresh(t, s)
	n, _ := ix.Get("vp/spec")
	att, err := s.GetAttachment(n.CurrentVersion().PayloadSHA)
	if err != nil {
		t.Fatal(err)
	}
	if att.CanonicalName != "" {
		t.Errorf("snapshot should carry no canonical form: %+v", att)
	}
}

// A corrupt index.json is moved aside and rebuilt; the sequence
// resumes from the floor the version log recorded rather than from
// one, so watermarks above it still mean something.
func TestCorruptIndexIsMovedAsideAndSeqResumes(t *testing.T) {
	s := graphStore(t)
	writePublic(t, s, "vp", "a.md", "---\nid: a\n---\none\n")
	refresh(t, s)
	writePublic(t, s, "vp", "a.md", "---\nid: a\n---\ntwo\n")
	before := refresh(t, s)
	if before.Seq < 2 {
		t.Fatalf("precondition: seq %d", before.Seq)
	}
	if err := os.WriteFile(s.path("graph", "index.json"), []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	ix, warnings, err := restartGraph(s)
	if err != nil {
		t.Fatalf("corrupt index must be rebuilt, not fatal: %v", err)
	}
	if len(warnings) == 0 || !strings.Contains(warnings[0], "moved to") {
		t.Errorf("expected a moved-aside warning: %v", warnings)
	}
	entries, _ := os.ReadDir(s.path("graph"))
	aside := false
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "index.json.corrupt-") {
			aside = true
		}
	}
	if !aside {
		t.Error("corrupt index should be kept aside for inspection")
	}
	if ix.Seq < before.Seq {
		t.Errorf("seq went backwards after the rebuild: %d -> %d", before.Seq, ix.Seq)
	}
	if n, _ := ix.Get("vp/a"); n == nil || n.CurrentVersion().N != 2 {
		t.Errorf("versions lost in the rebuild: %+v", n)
	}
}

// A markdown file the maintainer cannot read when it loads keeps its
// node and its versions rather than vanishing and flagging everything
// that rests on it.
func TestUnreadableFileKeepsItsNode(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("permission bits do not bind root")
	}
	s := graphStore(t)
	writePublic(t, s, "cs", "a.md", "---\nid: a\n---\nx\n")
	writePublic(t, s, "vp", "d.md", "---\nid: d\ndepends_on: cs/a\n---\nx\n")
	refresh(t, s)
	p := filepath.Join(publicDir(s, "cs"), "a.md")
	if err := os.Chmod(p, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(p, 0o644) })
	ix, warnings, err := restartGraph(s)
	if err != nil {
		t.Fatal(err)
	}
	if len(warnings) == 0 {
		t.Error("expected a warning about the unreadable file")
	}
	a, ok := ix.Get("cs/a")
	if !ok || a.CurrentVersion().N != 1 {
		t.Fatalf("node should survive an unreadable pass: %+v ok=%v", a, ok)
	}
	if d, _ := ix.Get("vp/d"); d.Flagged {
		t.Errorf("a transient read error must not flag dependents: %v", d.Flags)
	}
}

// Symlinks under public/ are not the owner's artifacts: an agent must
// not be able to point the pass (and its snapshots) at a file outside
// its own tree.
func TestRefreshGraphSkipsSymlinksAndHiddenEntries(t *testing.T) {
	s := graphStore(t)
	writePublic(t, s, "vp", "real.md", "---\nid: real\n---\nx\n")
	outside := filepath.Join(t.TempDir(), "secret.md")
	if err := os.WriteFile(outside, []byte("---\nid: secret\n---\nnot yours\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(publicDir(s, "vp"), "link.md")); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(publicDir(s, "vp"), ".scratch"), 0o755); err != nil {
		t.Fatal(err)
	}
	writePublic(t, s, "vp", ".scratch/note.md", "---\nid: hidden\n---\nx\n")
	ix := refresh(t, s)
	for _, id := range []string{"vp/secret", "vp/link", "vp/hidden", "vp/.scratch/note"} {
		if _, ok := ix.Get(id); ok {
			t.Errorf("%s must not be indexed", id)
		}
	}
	if _, ok := ix.Get("vp/real"); !ok {
		t.Error("the real file should still be indexed")
	}
	if got := len(versionLines(t, s)); got != 1 {
		t.Errorf("only the real file should be versioned, got %d lines", got)
	}
}

// Publishes, scans and reads run from several goroutines at once (an
// agent publishing, the anti-entropy ticker, agents waking and the CEO
// opening the page). Version numbers per node must stay a strict 1..k
// sequence with no duplicates, and the final index must describe the
// final bytes.
func TestGraphConcurrentPublishScanAndReadKeepVersionsMonotonic(t *testing.T) {
	s := graphStore(t)
	const edits = 12
	var wg sync.WaitGroup
	writer := func() {
		defer wg.Done()
		for i := 0; i < edits; i++ {
			writePublic(t, s, "vp", "a.md", fmt.Sprintf("---\nid: a\n---\nedit %d\n", i))
			if _, _, err := s.Graph().Published("vp", []string{"a.md"}); err != nil {
				t.Errorf("publish: %v", err)
			}
		}
	}
	scanner := func() {
		defer wg.Done()
		for i := 0; i < edits; i++ {
			if err := s.Graph().Scan(context.Background()); err != nil {
				t.Errorf("scan: %v", err)
			}
		}
	}
	reader := func() {
		defer wg.Done()
		for i := 0; i < edits; i++ {
			if ix := s.Graph().Index(); ix != nil {
				_ = ix.Sorted()
			}
		}
	}
	wg.Add(4)
	go writer()
	go scanner()
	go scanner()
	go reader()
	wg.Wait()

	ix := refresh(t, s)
	n, ok := ix.Get("vp/a")
	if !ok {
		t.Fatal("node missing")
	}
	seen := map[int]bool{}
	for _, line := range versionLines(t, s) {
		var rec graphVersionRecord
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			t.Fatalf("bad line %q: %v", line, err)
		}
		if rec.ID != "vp/a" {
			continue
		}
		if seen[rec.N] {
			t.Errorf("version %d recorded twice", rec.N)
		}
		seen[rec.N] = true
	}
	for i := 1; i <= n.CurrentVersion().N; i++ {
		if !seen[i] {
			t.Errorf("version %d missing from the log", i)
		}
	}
	// The final version's bytes are the final edit.
	sum := sha256.Sum256([]byte(fmt.Sprintf("---\nid: a\n---\nedit %d\n", edits-1)))
	if n.CurrentVersion().SHA != hex.EncodeToString(sum[:]) {
		t.Errorf("final version sha does not match the final bytes")
	}
}

func TestRefreshGraphReportsOwnerFindings(t *testing.T) {
	s := graphStore(t)
	writePublic(t, s, "vp", "bad.md", "---\nkind: datasheet\n---\nx\n")
	ix := refresh(t, s)
	got := ix.OwnerFindings("vp")
	if len(got) != 1 || !strings.Contains(got[0], `bad.md: front matter rejected: kind "datasheet"`) {
		t.Errorf("findings = %v", got)
	}
}
