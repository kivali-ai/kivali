package agent

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/kivali-ai/kivali/internal/graph"
	"github.com/kivali-ai/kivali/internal/store"
)

// The publish reply is how the agent learns at once what the index
// made of each file; every field the store reports has to reach it.
func TestRenderPublishReport(t *testing.T) {
	out := RenderPublishReport(store.PublishReport{
		Owner: "vp",
		Files: []store.PublishedFile{
			{Path: "specs/api.md", NodeID: "vp/api", Version: 2, Status: graph.StatusCurrent},
			{Path: "bad.md", NodeID: "vp/bad", Version: 1, Status: graph.StatusCurrent, Rejected: `status "sideways" is not one of`, Problems: []string{"no about"}},
			{Path: "data.csv", NodeID: "vp/model", Version: 3, Status: graph.StatusProvisional, Payload: true, Flags: []string{"depends on withdrawn cs/x"}},
			{Path: "twin.md", Dropped: `not indexed, its id "twin" is taken by other.md`},
		},
		Skipped: []string{"artifacts/private/kit/.swp"},
	})
	for _, want := range []string{
		"Published 4 files to /files/artifacts/public/ (peers read them at /files/artifacts/shared/vp/)",
		"- specs/api.md → node vp/api v2, current",
		`- bad.md → node vp/bad v1, current; front matter rejected: status "sideways" is not one of; problem: no about`,
		"- data.csv → payload of node vp/model v3, provisional, flagged: depends on withdrawn cs/x",
		`- twin.md → not indexed, its id "twin" is taken by other.md`,
		`Skipped (names starting with "."): artifacts/private/kit/.swp`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("reply lacks %q:\n%s", want, out)
		}
	}

	out = RenderPublishReport(store.PublishReport{Owner: "vp", Files: []store.PublishedFile{{Path: "a.md"}}, IndexErr: errors.New("disk full")})
	if !strings.Contains(out, "- a.md\n") || !strings.Contains(out, "could not be updated (disk full)") {
		t.Errorf("index failure reply:\n%s", out)
	}

	// Skipped dot-names are summarised like the files.
	var many []string
	for i := 0; i < publishReportMaxLines+7; i++ {
		many = append(many, fmt.Sprintf("kit/.x%d", i))
	}
	out = RenderPublishReport(store.PublishReport{Owner: "vp", Files: []store.PublishedFile{{Path: "a.md"}}, Skipped: many})
	if !strings.Contains(out, many[publishReportMaxLines-1]+", … and 7 more") || strings.Contains(out, many[publishReportMaxLines]) {
		t.Errorf("long skipped list:\n%s", out)
	}
}

func TestRenderUnpublishReport(t *testing.T) {
	out := RenderUnpublishReport(store.PublishReport{
		Owner:     "vp",
		Files:     []store.PublishedFile{{Path: "kit/a.md"}, {Path: "kit/data.csv"}},
		Gone:      []string{"vp/a"},
		Remaining: []string{"vp/model"},
	})
	for _, want := range []string{
		"Unpublished 2 files from /files/artifacts/public/: kit/a.md, kit/data.csv.",
		"Removed from the graph: vp/a.",
		"Still in the graph (other files of theirs are still published): vp/model.",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("reply lacks %q:\n%s", want, out)
		}
	}
	out = RenderUnpublishReport(store.PublishReport{Owner: "vp", Files: []store.PublishedFile{{Path: "x.bin"}}})
	if !strings.Contains(out, "Unpublished 1 file from") || !strings.Contains(out, "No graph node was built from these files.") {
		t.Errorf("no-node reply:\n%s", out)
	}
}
