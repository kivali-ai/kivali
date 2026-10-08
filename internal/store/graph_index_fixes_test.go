package store

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A manifest with a DECLARED id that the pass cannot read keeps that
// id. Before, only the versions were kept: the file was planned under
// its path-derived name, so `cs/qs` became `cs/queue-sizing` for one
// pass, the real node vanished, and everything resting on it was
// flagged — then un-flagged a pass later. The existing unreadable-file
// test used a file whose declared and derived ids coincide and could
// not see this.
func TestUnreadableDeclaredIdManifestKeepsItsId(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("permission bits do not bind root")
	}
	s := graphStore(t)
	writePublic(t, s, "cs", "queue-sizing.md", "---\nid: qs\n---\nx\n")
	writePublic(t, s, "vp", "d.md", "---\nid: d\ndepends_on: cs/qs\n---\nx\n")
	refresh(t, s)
	p := filepath.Join(publicDir(s, "cs"), "queue-sizing.md")
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
	qs, ok := ix.Get("cs/qs")
	if !ok || qs.CurrentVersion().N != 1 {
		t.Fatalf("cs/qs should survive an unreadable pass under its declared id: %+v ok=%v", qs, ok)
	}
	if _, derived := ix.Get("cs/queue-sizing"); derived {
		t.Error("the unreadable file was re-indexed under its path-derived id")
	}
	if d, _ := ix.Get("vp/d"); d.Flagged {
		t.Errorf("a transient read error must not flag dependents: %v", d.Flags)
	}
}

// Deleting the NEWEST upload of a name makes the older upload current
// again; that is not a new version of the node. Before, the pass saw
// "current SHA differs from the last record" and cut v3 carrying v1's
// bytes, so the sequence claimed a re-upload that never happened.
func TestDeletingTheNewestUploadCutsNoPhantomVersion(t *testing.T) {
	s := graphStore(t)
	if _, err := s.AddProjectFile("plan.md", strings.NewReader("v1")); err != nil {
		t.Fatal(err)
	}
	pf2, err := s.AddProjectFile("plan.md", strings.NewReader("v2"))
	if err != nil {
		t.Fatal(err)
	}
	ix := refresh(t, s)
	if n, _ := ix.Get("ceo/plan"); n == nil || n.CurrentVersion().N != 2 {
		t.Fatalf("precondition: two versions: %+v", n)
	}
	if err := s.RemoveProjectFile(pf2.SHA); err != nil {
		t.Fatal(err)
	}
	ix = refresh(t, s)
	n, _ := ix.Get("ceo/plan")
	if n == nil || len(n.Versions) != 2 || n.CurrentVersion().N != 2 {
		t.Fatalf("deleting the newest upload cut a phantom version: %+v", n)
	}
}
