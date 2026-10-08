package store

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/kivali-ai/kivali/internal/files"
	"github.com/kivali-ai/kivali/internal/graph"
)

// writeWorkspace writes rel under an agent's /files/ root.
func writeWorkspace(t *testing.T, s *FSStore, slug, rel, body string) {
	t.Helper()
	p := filepath.Join(files.StorageRoot(s.path("agents", slug)), filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func readPublished(t *testing.T, s *FSStore, slug, rel string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(publicDir(s, slug), filepath.FromSlash(rel)))
	if err != nil {
		t.Fatalf("published %s/%s: %v", slug, rel, err)
	}
	return string(b)
}

func TestPublishFileReportsWhatTheIndexMadeOfIt(t *testing.T) {
	s := graphStore(t)
	writeWorkspace(t, s, "vp", "artifacts/private/specs/api.md", "---\nid: api\nkind: requirement\nabout: vp\ncheck: the API answers\n---\nThe API answers.\n")
	rep, err := s.Publish(PublishRequest{Owner: "vp", Source: "/files/artifacts/private/specs/api.md"})
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Files) != 1 {
		t.Fatalf("files = %+v", rep.Files)
	}
	f := rep.Files[0]
	if f.Path != "specs/api.md" || f.NodeID != "vp/api" || f.Version != 1 || f.Status != graph.StatusCurrent || rep.IndexErr != nil {
		t.Errorf("report = %+v (index err %v)", f, rep.IndexErr)
	}
	if got := readPublished(t, s, "vp", "specs/api.md"); !strings.Contains(got, "The API answers.") {
		t.Errorf("published bytes = %q", got)
	}

	// Publishing over it replaces it and cuts a new version.
	writeWorkspace(t, s, "vp", "artifacts/private/specs/api.md", "---\nid: api\nkind: requirement\nabout: vp\ncheck: the API answers fast\n---\nThe API answers fast.\n")
	rep, err = s.Publish(PublishRequest{Owner: "vp", Source: "/files/artifacts/private/specs/api.md"})
	if err != nil {
		t.Fatal(err)
	}
	if f := rep.Files[0]; f.Version != 2 {
		t.Errorf("republish version = %d, want 2", f.Version)
	}

	// Front matter the index cannot accept comes back in the reply.
	writeWorkspace(t, s, "vp", "artifacts/private/bad.md", "---\nid: bad\nstatus: sideways\n---\nx\n")
	rep, err = s.Publish(PublishRequest{Owner: "vp", Source: "/files/artifacts/private/bad.md", Dest: "/files/artifacts/public/misc/bad.md"})
	if err != nil {
		t.Fatal(err)
	}
	if f := rep.Files[0]; f.Path != "misc/bad.md" || f.NodeID == "" || f.Rejected == "" {
		t.Errorf("rejected manifest report = %+v", f)
	}
	// And a dangling dependency is the node's problem, or its flag.
	writeWorkspace(t, s, "vp", "artifacts/private/dep.md", "---\nid: dep\nkind: decision\nabout: vp\ndepends_on: cs/nothing-here\n---\nx\n")
	rep, err = s.Publish(PublishRequest{Owner: "vp", Source: "/files/artifacts/private/dep.md"})
	if err != nil {
		t.Fatal(err)
	}
	if f := rep.Files[0]; len(f.Problems) == 0 && len(f.Flags) == 0 {
		t.Errorf("a dangling depends_on produced no finding: %+v", f)
	}
}

func TestPublishDirectoryDefaultDestsAndDotfiles(t *testing.T) {
	s := graphStore(t)
	writeWorkspace(t, s, "vp", "artifacts/private/kit/a.md", "a")
	writeWorkspace(t, s, "vp", "artifacts/private/kit/sub/b.csv", "b")
	writeWorkspace(t, s, "vp", "artifacts/private/kit/.a.md.swp", "swap")
	rep, err := s.Publish(PublishRequest{Owner: "vp", Source: "/files/artifacts/private/kit"})
	if err != nil {
		t.Fatal(err)
	}
	var paths []string
	for _, f := range rep.Files {
		paths = append(paths, f.Path)
	}
	if !reflect.DeepEqual(paths, []string{"kit/a.md", "kit/sub/b.csv"}) {
		t.Errorf("published = %v", paths)
	}
	if !reflect.DeepEqual(rep.Skipped, []string{"artifacts/private/kit/.a.md.swp"}) {
		t.Errorf("skipped = %v", rep.Skipped)
	}
	if _, err := os.Stat(filepath.Join(publicDir(s, "vp"), "kit", ".a.md.swp")); err == nil {
		t.Error("a dotfile was published")
	}
	for _, f := range rep.Files {
		if f.NodeID == "" {
			t.Errorf("%s is in no node: %+v", f.Path, f)
		}
	}

	// Outside artifacts/private/, the default is the base name.
	writeWorkspace(t, s, "vp", "background/report.md", "r")
	if _, err := s.Publish(PublishRequest{Owner: "vp", Source: "/files/background/report.md"}); err != nil {
		t.Fatal(err)
	}
	if got := readPublished(t, s, "vp", "report.md"); got != "r" {
		t.Errorf("report.md = %q", got)
	}
}

func TestPublishRefusals(t *testing.T) {
	s := graphStore(t)
	victim := t.TempDir()
	if err := os.WriteFile(filepath.Join(victim, "secret.md"), []byte("secret"), 0o644); err != nil {
		t.Fatal(err)
	}
	writeWorkspace(t, s, "vp", "artifacts/private/kit/a.md", "a")
	storage := files.StorageRoot(s.path("agents", "vp"))
	if err := os.Symlink(filepath.Join(victim, "secret.md"), filepath.Join(storage, "artifacts", "private", "kit", "leak.md")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(victim, "secret.md"), filepath.Join(storage, "artifacts", "private", "link.md")); err != nil {
		t.Fatal(err)
	}
	writeWorkspace(t, s, "vp", "artifacts/private/ok.md", "ok")
	cases := []struct {
		req  PublishRequest
		want string
	}{
		{PublishRequest{Owner: "vp", Source: "/files/artifacts/private/kit"}, "kit/leak.md is a symbolic link"},
		{PublishRequest{Owner: "vp", Source: "/files/artifacts/private/link.md"}, "symbolic link"},
		{PublishRequest{Owner: "vp", Source: "/files/project/plan.md"}, "cannot be published from"},
		{PublishRequest{Owner: "vp", Source: "/files/artifacts/public/x.md"}, "cannot be published from"},
		{PublishRequest{Owner: "vp", Source: "/files/artifacts/shared/cs/x.md"}, "cannot be published from"},
		{PublishRequest{Owner: "vp", Source: "/files/../etc/passwd"}, ".."},
		{PublishRequest{Owner: "vp", Source: "/etc/passwd"}, "under /files/"},
		{PublishRequest{Owner: "vp", Source: "/files"}, "not the whole tree"},
		{PublishRequest{Owner: "vp", Source: "/files/artifacts/private/ok.md", Dest: "../cs/x.md"}, ".."},
		{PublishRequest{Owner: "vp", Source: "/files/artifacts/private/ok.md", Dest: ".hidden.md"}, "starting with ."},
		{PublishRequest{Owner: "vp", Source: "/files/artifacts/private/missing.md"}, "not found"},
		{PublishRequest{Owner: "ceo", Source: "/files/x.md"}, "cannot publish"},
		{PublishRequest{Owner: "nobody", Source: "/files/x.md"}, "not an active agent"},
		{PublishRequest{Owner: "vp", SubagentID: "../cs", Source: "/files/artifacts/private/ok.md"}, "bad subagent id"},
		{PublishRequest{Owner: "vp", SubagentID: "ghost", Source: "/files/artifacts/private/ok.md"}, "no workspace"},
	}
	for _, c := range cases {
		_, err := s.Publish(c.req)
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("Publish(%+v) = %v; want an error containing %q", c.req, err, c.want)
		}
	}
	if entries, _ := os.ReadDir(publicDir(s, "vp")); len(entries) != 0 {
		t.Errorf("a refused publish wrote %d entries", len(entries))
	}
}

// A subagent's source is a path in its view of the parent's tree; what
// it publishes lands in the parent's area.
func TestPublishFromASubagentLandsInTheParentsArea(t *testing.T) {
	s := graphStore(t)
	storage := files.StorageRoot(s.path("agents", "vp"))
	overlay := filepath.Join(storage, "subagents", "sa-1")
	if err := files.BuildSubagentOverlay(files.SubagentOverlay{Root: overlay}); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(overlay, "artifacts", "private", "finding.md"), []byte("from the subagent"), 0o644); err != nil {
		t.Fatal(err)
	}
	// The parent has a file of the same name; the subagent's view is
	// not the parent's.
	writeWorkspace(t, s, "vp", "artifacts/private/finding.md", "from the parent")
	writeWorkspace(t, s, "vp", "background/shared.md", "blackboard")

	rep, err := s.Publish(PublishRequest{Owner: "vp", SubagentID: "sa-1", Source: "/files/artifacts/private/finding.md"})
	if err != nil {
		t.Fatal(err)
	}
	if rep.Owner != "vp" || rep.Files[0].Path != "finding.md" {
		t.Errorf("report = %+v", rep)
	}
	if got := readPublished(t, s, "vp", "finding.md"); got != "from the subagent" {
		t.Errorf("published = %q", got)
	}
	// background/ is the parent's, read where core linked it.
	if _, err := s.Publish(PublishRequest{Owner: "vp", SubagentID: "sa-1", Source: "/files/background/shared.md"}); err != nil {
		t.Fatal(err)
	}
	if got := readPublished(t, s, "vp", "shared.md"); got != "blackboard" {
		t.Errorf("background publish = %q", got)
	}
	// Every other link in the view is refused, never followed.
	if _, err := s.Publish(PublishRequest{Owner: "vp", SubagentID: "sa-1", Source: "/files/project"}); err == nil {
		t.Error("a subagent published through its project/ link")
	}
}

func TestUnpublishReportsTheNodesThatWent(t *testing.T) {
	s := graphStore(t)
	writeWorkspace(t, s, "vp", "artifacts/private/kit/a.md", "---\nid: a\n---\nx\n")
	writeWorkspace(t, s, "vp", "artifacts/private/kit/b.md", "---\nid: b\n---\ny\n")
	writeWorkspace(t, s, "vp", "artifacts/private/keep.md", "---\nid: keep\n---\nz\n")
	for _, src := range []string{"/files/artifacts/private/kit", "/files/artifacts/private/keep.md"} {
		if _, err := s.Publish(PublishRequest{Owner: "vp", Source: src}); err != nil {
			t.Fatal(err)
		}
	}
	rep, err := s.Unpublish("vp", "/files/artifacts/public/kit")
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(rep.Gone, []string{"vp/a", "vp/b"}) || len(rep.Remaining) != 0 || len(rep.Files) != 2 {
		t.Errorf("unpublish report = %+v", rep)
	}
	if _, err := os.Stat(filepath.Join(publicDir(s, "vp"), "kit")); !os.IsNotExist(err) {
		t.Errorf("kit/ still published: %v", err)
	}
	ix, err := s.ReadGraphIndex()
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := ix.Get("vp/keep"); !ok {
		t.Error("an unrelated node went too")
	}
	for _, p := range []string{"kit", "", "../cs/x", "."} {
		if _, err := s.Unpublish("vp", p); err == nil {
			t.Errorf("Unpublish(%q) succeeded", p)
		}
	}
}

// A publish's reply carries the warnings about its own files: those
// naming owner/path as their subject, not another file whose path
// merely contains it.
func TestWarningsAboutAnchorsOnThePath(t *testing.T) {
	warnings := []string{
		"vp/a.md: snapshot: disk full (no version cut this pass)",
		"vp/a.md.bak: unreadable (keeping its previous versions)",
		"cs/vp/a.md: unreadable (keeping its previous versions)",
		"vp/kit/a.md: unreadable (keeping its previous versions)",
		"graph/index.json: read-only (the index is current in memory; written with the next change)",
	}
	got := warningsAbout("vp", []string{"a.md"}, warnings)
	if want := warnings[:1]; !reflect.DeepEqual(got, want) {
		t.Errorf("warningsAbout = %q, want %q", got, want)
	}
}
