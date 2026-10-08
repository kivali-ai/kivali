package web

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kivali-ai/kivali/internal/files"
	"github.com/kivali-ai/kivali/internal/store"
	"github.com/kivali-ai/kivali/internal/web/apitypes"
)

func graphDo(t *testing.T, srv *Server, path string) *httptest.ResponseRecorder {
	t.Helper()
	mux := http.NewServeMux()
	srv.wireAPIGraphRoutes(mux)
	req := httptest.NewRequest(http.MethodGet, path, nil)
	rr := httptest.NewRecorder()
	apiHeaders(requireSameOrigin(mux)).ServeHTTP(rr, req)
	return rr
}

func getGraph(t *testing.T, srv *Server, query string) apitypes.Graph {
	t.Helper()
	rr := graphDo(t, srv, "/api/v1/graph"+query)
	if rr.Code != http.StatusOK {
		t.Fatalf("GET /api/v1/graph%s: code %d body %s", query, rr.Code, rr.Body.String())
	}
	return decodeAPI[apitypes.Graph](t, rr)
}

// graphFixture seeds two agents and a project file. vp owns seven
// notes (n1..n7), a withdrawn node "old", a node resting on it (so
// flagged), and a file whose front matter is rejected (a problem).
func graphFixture(t *testing.T) (*Server, func(rel, body string)) {
	t.Helper()
	srv := newTestServer(t)
	for _, a := range []store.Agent{
		{Slug: "chief-of-staff", Role: "Chief of Staff", ReportsTo: "ceo"},
		{Slug: "vp", Role: "Engineering lead", ReportsTo: "ceo"},
	} {
		if err := srv.Store.CreateAgent(a, "# role"); err != nil {
			t.Fatal(err)
		}
	}
	pub := files.PublishedDir(srv.Store.Root(), "vp")
	if err := os.MkdirAll(pub, 0o755); err != nil {
		t.Fatal(err)
	}
	// A publish landing, as artifact_publish lands one: the file
	// written into vp's published tree, then the maintainer told.
	write := func(rel, body string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(pub, rel), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		if _, _, err := srv.Store.Graph().Published("vp", []string{rel}); err != nil {
			t.Fatal(err)
		}
	}
	for i := 1; i <= 7; i++ {
		write(fmt.Sprintf("n%d.md", i), fmt.Sprintf("---\nid: n%d\nsummary: Note %d\n---\nbody %d", i, i, i))
	}
	write("old.md", "---\nid: old\nstatus: withdrawn\nsummary: Old power plan\n---\nwithdrawn")
	write("rests.md", "---\nid: rests\nsummary: Export pipeline\ndepends_on: [vp/old]\n---\nrests on old")
	write("bad.md", "---\nkind: datasheet\n---\nx")
	if _, err := srv.Store.AddProjectFile("plan.md", strings.NewReader("the plan")); err != nil {
		t.Fatal(err)
	}
	// What the upload handler's afterProjectFilesChanged does.
	srv.Store.Graph().ProjectFilesChanged()
	return srv, write
}

func ownerSlugs(g apitypes.Graph) []string {
	out := []string{}
	for _, o := range g.Owners {
		out = append(out, o.Slug)
	}
	return out
}

func TestAPIGraphOwnersReadoutsAndPaging(t *testing.T) {
	srv, _ := graphFixture(t)
	g := getGraph(t, srv, "")
	if g.Readouts != (apitypes.GraphReadouts{Nodes: 11, Flagged: 1, Problems: 1}) {
		t.Errorf("readouts = %+v", g.Readouts)
	}
	// The chief of staff owns nothing and is left out.
	if got := ownerSlugs(g); strings.Join(got, ",") != "ceo,vp" {
		t.Fatalf("owners = %v, want ceo then vp", got)
	}
	ceo, vp := g.Owners[0], g.Owners[1]
	if ceo.Name != "You" || ceo.Count != 1 || ceo.HasMore || len(ceo.Nodes) != 1 || ceo.Nodes[0].ID != "ceo/plan" {
		t.Errorf("ceo section = %+v", ceo)
	}
	if vp.Name != "Engineering lead" || vp.Count != 10 || !vp.HasMore || len(vp.Nodes) != graphDefaultLimit {
		t.Errorf("vp section: name %q count %d has_more %v nodes %d", vp.Name, vp.Count, vp.HasMore, len(vp.Nodes))
	}
	first := vp.Nodes[0]
	if first.ID != "vp/bad" || first.Problem == nil || !strings.Contains(*first.Problem, "front matter rejected") || first.Kind != "artifact" || first.Version != 1 || first.Updated == nil {
		t.Errorf("first vp row = %+v", first)
	}

	// Show all for one owner, paged.
	page := getGraph(t, srv, "?owner=vp&offset=8&limit=5")
	if got := ownerSlugs(page); strings.Join(got, ",") != "vp" {
		t.Fatalf("owner page owners = %v", got)
	}
	o := page.Owners[0]
	if o.Count != 10 || o.HasMore || len(o.Nodes) != 2 || o.Nodes[0].ID != "vp/old" || o.Nodes[1].ID != "vp/rests" {
		t.Errorf("vp page from 8 = %+v", o)
	}
	all := getGraph(t, srv, "?owner=vp&limit=100").Owners[0]
	if len(all.Nodes) != 10 || all.HasMore {
		t.Errorf("show all = %d nodes, has_more %v", len(all.Nodes), all.HasMore)
	}
	// A limit at the int maximum with an offset must not overflow.
	huge := getGraph(t, srv, "?owner=vp&offset=3&limit=9223372036854775807").Owners[0]
	if len(huge.Nodes) != 7 || huge.HasMore {
		t.Errorf("huge limit from 3 = %d nodes, has_more %v", len(huge.Nodes), huge.HasMore)
	}
	// Exactly the remainder: no more to show.
	exact := getGraph(t, srv, "?owner=vp&offset=5&limit=5").Owners[0]
	if len(exact.Nodes) != 5 || exact.HasMore {
		t.Errorf("offset 5 limit 5 = %d nodes, has_more %v", len(exact.Nodes), exact.HasMore)
	}
	// Past the end: the section stays, with its count and no rows.
	past := getGraph(t, srv, "?owner=vp&offset=50").Owners[0]
	if len(past.Nodes) != 0 || past.HasMore || past.Count != 10 {
		t.Errorf("offset past the end = %+v", past)
	}
	// Garbage in a node id is a 404, never a 500.
	for _, p := range []string{"/api/v1/graph/nodes/..%2F..%2Fetc%2Fpasswd", "/api/v1/graph/nodes/vp/rests@", "/api/v1/graph/nodes/@1", "/api/v1/graph/nodes/vp/n1@@2"} {
		if rr := graphDo(t, srv, p); rr.Code != http.StatusNotFound && rr.Code != http.StatusOK {
			t.Errorf("%s: code %d", p, rr.Code)
		}
	}
	// Unowned (owner=) has nothing here, so no section.
	if got := getGraph(t, srv, "?owner="); len(got.Owners) != 0 {
		t.Errorf("owner= (unowned) = %v", ownerSlugs(got))
	}

	assertAPIError(t, graphDo(t, srv, "/api/v1/graph?limit=0"), http.StatusBadRequest)
	assertAPIError(t, graphDo(t, srv, "/api/v1/graph?offset=-1"), http.StatusBadRequest)
}

func TestAPIGraphSearchAndFilters(t *testing.T) {
	srv, _ := graphFixture(t)

	flagged := getGraph(t, srv, "?flagged=1")
	if got := ownerSlugs(flagged); strings.Join(got, ",") != "vp" || flagged.Owners[0].Count != 1 {
		t.Fatalf("flagged owners = %v", got)
	}
	if n := flagged.Owners[0].Nodes[0]; n.ID != "vp/rests" || !n.Flagged {
		t.Errorf("flagged row = %+v", n)
	}
	// Readouts do not move with the filters.
	if flagged.Readouts.Nodes != 11 {
		t.Errorf("filtered readouts = %+v", flagged.Readouts)
	}

	problems := getGraph(t, srv, "?problems=1")
	if len(problems.Owners) != 1 || problems.Owners[0].Count != 1 || problems.Owners[0].Nodes[0].ID != "vp/bad" {
		t.Errorf("problems = %+v", problems.Owners)
	}

	// Search matches the summary, case-insensitively...
	bySummary := getGraph(t, srv, "?q=PIPELINE")
	if len(bySummary.Owners) != 1 || bySummary.Owners[0].Nodes[0].ID != "vp/rests" {
		t.Errorf("q=PIPELINE = %+v", bySummary.Owners)
	}
	// ...and the id.
	byID := getGraph(t, srv, "?q=vp/n3")
	if len(byID.Owners) != 1 || byID.Owners[0].Count != 1 || byID.Owners[0].Nodes[0].Title != "Note 3" {
		t.Errorf("q=vp/n3 = %+v", byID.Owners)
	}
	if none := getGraph(t, srv, "?q=nothing-like-this"); len(none.Owners) != 0 {
		t.Errorf("no match = %v", ownerSlugs(none))
	}
	// Filters AND together.
	if both := getGraph(t, srv, "?flagged=1&problems=1"); len(both.Owners) != 0 {
		t.Errorf("flagged and problems = %v", ownerSlugs(both))
	}
}

func TestAPIGraphNode(t *testing.T) {
	srv, write := graphFixture(t)
	// rests.md is v1; republishing it cuts v2.
	write("rests.md", "---\nid: rests\nsummary: Export pipeline, rev 2\ndepends_on: [vp/old]\n---\nrests on old, again")

	rr := graphDo(t, srv, "/api/v1/graph/nodes/vp/rests")
	if rr.Code != http.StatusOK {
		t.Fatalf("node: code %d body %s", rr.Code, rr.Body.String())
	}
	n := decodeAPI[apitypes.GraphNode](t, rr)
	f := n.Fields
	if f.ID != "vp/rests" || f.Type != "artifact" || f.Owner.Slug != "vp" || f.Path != "rests.md" || !f.Flagged || len(f.Flags) != 1 ||
		len(f.DependsOn) != 1 || f.DependsOn[0].ID != "vp/old" || !f.DependsOn[0].Resolved || len(f.Versions) != 2 || !f.Manifested {
		t.Errorf("fields = %+v", f)
	}
	// The text leaves the front matter out: the fields carry it.
	if n.Version == nil || n.Version.N != 2 || n.Version.URL == nil || n.BodyMD == nil || *n.BodyMD != "rests on old, again" {
		t.Errorf("current version %+v body %v", n.Version, n.BodyMD)
	}

	pinned := decodeAPI[apitypes.GraphNode](t, graphDo(t, srv, "/api/v1/graph/nodes/vp/rests@1"))
	if pinned.Version == nil || pinned.Version.N != 1 || pinned.BodyMD == nil || *pinned.BodyMD != "rests on old" {
		t.Errorf("pinned version %+v body %v", pinned.Version, pinned.BodyMD)
	}

	old := decodeAPI[apitypes.GraphNode](t, graphDo(t, srv, "/api/v1/graph/nodes/vp/old"))
	if old.Fields.Status != "withdrawn" || len(old.Fields.Dependents) != 1 || old.Fields.Dependents[0] != "vp/rests" {
		t.Errorf("old fields = %+v", old.Fields)
	}

	agentNode := decodeAPI[apitypes.GraphNode](t, graphDo(t, srv, "/api/v1/graph/nodes/vp"))
	if agentNode.Fields.Type != "agent" || agentNode.Version != nil || agentNode.BodyMD != nil || agentNode.Fields.ReportsTo != "ceo" {
		t.Errorf("agent node = %+v", agentNode)
	}

	plan := decodeAPI[apitypes.GraphNode](t, graphDo(t, srv, "/api/v1/graph/nodes/ceo/plan"))
	if plan.Version == nil || plan.Version.URL != nil || plan.BodyMD == nil || *plan.BodyMD != "the plan" {
		t.Errorf("project file node = %+v body %v", plan.Version, plan.BodyMD)
	}

	assertAPIError(t, graphDo(t, srv, "/api/v1/graph/nodes/vp/nope"), http.StatusNotFound)
	assertAPIError(t, graphDo(t, srv, "/api/v1/graph/nodes/vp/rests@9"), http.StatusNotFound)
}

// A node's text drops a manifest and keeps everything else as written:
// prose between two rules is not front matter, and a rejected manifest
// stays visible to the owner who has to fix it.
func TestAPIGraphNodeBody(t *testing.T) {
	cases := []struct{ name, in, want string }{
		{"manifest", "---\nid: a\nsummary: A\n---\n\n# Plan\n\nText.\n", "# Plan\n\nText.\n"},
		{"crlf", "---\r\nid: a\r\n---\r\nText.", "Text."},
		{"bare", "# Plan\n\nText.", "# Plan\n\nText."},
		{"prose between rules", "---\nJust a thought.\n---\nMore.", "---\nJust a thought.\n---\nMore."},
		{"rejected", "---\nid: [a\n---\nText.", "---\nid: [a\n---\nText."},
	}
	for _, c := range cases {
		if got := graphNodeBody(c.in); got != c.want {
			t.Errorf("%s: graphNodeBody(%q) = %q, want %q", c.name, c.in, got, c.want)
		}
	}
}

// Every read answers from the maintained index: a publish is visible
// to the very next search or node read, and no read walks the
// published trees.
func TestAPIGraphReadsTheMaintainedIndex(t *testing.T) {
	srv, write := graphFixture(t)
	if rr := graphDo(t, srv, "/api/v1/graph/nodes/vp/n1"); rr.Code != http.StatusOK {
		t.Fatalf("first node read: code %d body %s", rr.Code, rr.Body.String())
	}
	walks := srv.Store.Graph().Walks()
	write("fresh.md", "---\nid: fresh\nsummary: Fresh note\n---\nnew")
	if got := getGraph(t, srv, "?q=fresh"); len(got.Owners) != 1 || got.Owners[0].Nodes[0].ID != "vp/fresh" {
		t.Errorf("search after a publish = %+v", got.Owners)
	}
	if rr := graphDo(t, srv, "/api/v1/graph/nodes/vp/fresh"); rr.Code != http.StatusOK {
		t.Errorf("node read after a publish: code %d", rr.Code)
	}
	for _, q := range []string{"", "?owner=vp&limit=100", "?flagged=1", "?problems=1"} {
		if g := getGraph(t, srv, q); g.Stale {
			t.Errorf("%s: stale", q)
		}
	}
	if g := getGraph(t, srv, ""); g.Readouts.Nodes != 12 {
		t.Errorf("readouts after a publish: %+v", g.Readouts)
	}
	if got := srv.Store.Graph().Walks(); got != walks {
		t.Errorf("graph reads walked the published trees %d times", got-walks)
	}
}
