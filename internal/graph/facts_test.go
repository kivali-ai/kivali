package graph

import (
	"encoding/json"
	"testing"
	"time"
)

// A build from cached Facts is the build from the bytes: the graph
// maintainer keeps Facts, not bodies, and nothing it indexes may read
// differently for that.
func TestBuildFromFactsEqualsBuildFromBody(t *testing.T) {
	bodies := map[string]string{
		"spec.md":     "---\nid: spec\nkind: requirement\nabout: vp/widget\nsummary: \n---\n# The widget\n\nline two\n",
		"widget.md":   "---\nid: widget\nevidence: [a, b]\nfile: widget.zip\ntitle: x\n---\nWidget manifest\n",
		"broken.md":   "---\nkind: [nope\n---\nbody\n",
		"prose.md":    "---\nnot yaml at all, just a rule\n---\nbody\n",
		"bare.md":     "# Just notes\n\ntext\n",
		"decision.md": "---\nid: dec\nkind: decision\nabout: vp/widget\n---\n" + manyLines(45),
	}
	var fromBody, fromFacts []FileInput
	for p, b := range bodies {
		fromBody = append(fromBody, FileInput{Owner: "vp", Path: p, Markdown: true, Body: []byte(b)})
		f := ReadFacts([]byte(b))
		fromFacts = append(fromFacts, FileInput{Owner: "vp", Path: p, Markdown: true, Facts: &f})
	}
	fromBody = append(fromBody, FileInput{Owner: "vp", Path: "widget.zip"})
	fromFacts = append(fromFacts, FileInput{Owner: "vp", Path: "widget.zip"})
	agents := []AgentInput{{Slug: "vp", Role: "VP"}}
	now := time.Date(2026, 9, 30, 0, 0, 0, 0, time.UTC)
	a := Build(Input{Now: now, Agents: agents, Files: fromBody})
	b := Build(Input{Now: now, Agents: agents, Files: fromFacts})
	ja, _ := json.Marshal(a)
	jb, _ := json.Marshal(b)
	if string(ja) != string(jb) {
		t.Fatalf("build from facts differs:\nbody:  %s\nfacts: %s", ja, jb)
	}
	if n, ok := a.Get("vp/spec"); !ok || n.Summary != "The widget" {
		t.Fatalf("summary from first line: %+v", n)
	}
	// A second build over the same Facts sees nothing change.
	c := Build(Input{Now: now, Prev: b, Agents: agents, Files: fromFacts})
	if c.Seq != b.Seq {
		t.Fatalf("rebuild from the same facts moved seq %d -> %d", b.Seq, c.Seq)
	}
}

func manyLines(n int) string {
	s := ""
	for i := 0; i < n; i++ {
		s += "a line\n"
	}
	return s
}
