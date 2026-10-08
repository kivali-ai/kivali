package graph

import (
	"strings"
	"testing"
	"unicode/utf8"
)

// Tests from the second bug hunt over the graph package. Each pinned a
// defect before its fix; they stay as the contract.

// A wrong-typed field is valid YAML, so the rejection must say which
// field and what shape it wanted, not "not valid YAML".
func TestTypeMismatchRejectionNamesTheField(t *testing.T) {
	cases := map[string]string{
		"about as list":       "---\nkind: decision\nabout: [vp/a, vp/b]\n---\nx",
		"id as mapping":       "---\nid: {a: b}\n---\nx",
		"depends_on as map":   "---\ndepends_on: {a: b}\n---\nx",
		"summary as sequence": "---\nsummary: [a]\n---\nx",
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			ix := build(t, md("vp", "n.md", body))
			n := mustNode(t, ix, "vp/n")
			if n.Rejected == "" {
				t.Fatal("expected a rejection")
			}
			field := strings.SplitN(name, " ", 2)[0]
			if !strings.Contains(n.Rejected, `"`+field+`"`) || strings.Contains(n.Rejected, "not valid YAML") {
				t.Errorf("rejection should name field %q and its expected shape; got %q", field, n.Rejected)
			}
		})
	}
}

// Prose between two horizontal rules that happens to contain ": " does
// not parse as YAML; that is still prose, not a broken manifest.
func TestProseThatFailsYAMLIsStillBare(t *testing.T) {
	ix := build(t, md("vp", "doc.md", "---\nThis is a draft.\nStatus: needs review\n---\nbody\n"))
	n := mustNode(t, ix, "vp/doc")
	if n.Rejected != "" || n.Manifested {
		t.Errorf("prose should be bare; got rejected=%q manifested=%v", n.Rejected, n.Manifested)
	}
	// Whereas a block that starts with a key and then breaks IS a broken
	// manifest, and the owner hears about it.
	ix = build(t, md("vp", "bad.md", "---\nkind: decision\nabout vp/x\n  : oops\n---\nbody\n"))
	if n := mustNode(t, ix, "vp/bad"); n.Rejected == "" {
		t.Error("a key-led block that fails to parse must be rejected, not silently bare")
	}
}

// A heredoc habit or an editor BOM in front of the fence must not turn
// a manifest into a silent bare file: either it parses, or the owner
// is told. It parses.
func TestFenceAfterLeadingNewlineOrBOMIsFrontMatter(t *testing.T) {
	for name, body := range map[string]string{
		"leading newline": "\n---\nid: x\nkind: requirement\nabout: vp\n---\nbody",
		"two blank lines": "\n\n---\nid: x\nkind: requirement\nabout: vp\n---\nbody",
		"utf-8 bom":       "\xEF\xBB\xBF---\nid: x\nkind: requirement\nabout: vp\n---\nbody",
		"trailing space":  "--- \nid: x\nkind: requirement\nabout: vp\n--- \nbody",
	} {
		t.Run(name, func(t *testing.T) {
			ix := build(t, md("vp", "n.md", body))
			n := mustNode(t, ix, "vp/x")
			if !n.Manifested || n.Kind != KindRequirement {
				t.Errorf("manifest not applied: %+v", n)
			}
		})
	}
}

// The extension of a non-markdown file is kept verbatim (lowercased),
// whatever it is, and a compound extension is not half-stripped.
func TestKeepExtHandlesEveryExtension(t *testing.T) {
	for in, want := range map[string]string{
		"lib.X":          "lib.x",
		".x":             ".x",
		"archive.tar.gz": "archive.tar.gz",
		"Release V3.ZIP": "release-v3.zip",
		"README":         "readme",
	} {
		if got := deriveNameKeepExt(in); got != want {
			t.Errorf("deriveNameKeepExt(%q) = %q, want %q", in, got, want)
		}
	}
	ix := build(t, md("vp", "lib.md", "x"), bin("vp", "lib.X"))
	mustNode(t, ix, "vp/lib")
	mustNode(t, ix, "vp/lib.x")
	if len(ix.OwnerFindings("vp")) != 0 {
		t.Errorf("no collision expected: %v", ix.OwnerFindings("vp"))
	}
}

// A payload folded into a manifest that is then dropped for an id
// collision must come back as a node of its own, not vanish.
func TestPayloadOfADroppedManifestIsNotLost(t *testing.T) {
	ix := build(t,
		md("vp", "0.md", "---\nid: a\n---\nx"),
		md("vp", "a.md", "---\nfile: p.zip\n---\nx"),
		bin("vp", "p.zip"),
	)
	if _, ok := ix.Get("vp/p.zip"); !ok {
		t.Errorf("p.zip fell out of the graph; have %v", ids(ix.Sorted()))
	}
	// Plan agrees: the payload has an id of its own.
	found := false
	for _, p := range Plan(Input{Files: []FileInput{
		md("vp", "0.md", "---\nid: a\n---\nx"),
		md("vp", "a.md", "---\nfile: p.zip\n---\nx"),
		bin("vp", "p.zip"),
	}}) {
		if p.Path == "p.zip" && p.ID == "vp/p.zip" && !p.Folded {
			found = true
		}
	}
	if !found {
		t.Error("Plan should place the released payload under its own id")
	}
}

func TestSupersedesBadPinIsAProblem(t *testing.T) {
	ix := build(t,
		md("vp", "old.md", "---\nid: r1\n---\nx"),
		md("vp", "new.md", "---\nid: r2\nsupersedes: vp/r1@999\n---\nx"),
	)
	r2 := mustNode(t, ix, "vp/r2")
	if !hasLine(r2.Problems, "supersedes vp/r1@999: no such version") || r2.Supersedes[0].Resolved {
		t.Errorf("bad pin on supersedes accepted: %+v %v", r2.Supersedes, r2.Problems)
	}
	if r1 := mustNode(t, ix, "vp/r1"); r1.Status == StatusSuperseded {
		t.Error("a supersedes edge that did not resolve must not supersede")
	}
}

func TestNewlyDroppedFileAdvancesSeq(t *testing.T) {
	a := md("vp", "a.md", "---\nid: b\n---\nx")
	first := Build(Input{Now: t0, Agents: org, Files: []FileInput{a}})
	second := Build(Input{Now: t0, Prev: first, Agents: org, Files: []FileInput{a, md("vp", "b.md", "bare")}})
	if second.Seq == first.Seq {
		t.Error("a dropped file changed the index but Seq stood still")
	}
	third := Build(Input{Now: t0, Prev: second, Agents: org, Files: []FileInput{a, md("vp", "b.md", "bare")}})
	if third.Seq != second.Seq {
		t.Error("unchanged drops must not advance Seq")
	}
}

// "What is about X" and X's about-me list must agree. A subject whose
// pin does not resolve is still about that subject; the bad pin is a
// problem on the pointing node.
func TestQueryAndAboutMeAgree(t *testing.T) {
	ix := build(t,
		md("cs", "analysis.md", "---\nid: analysis\n---\nx"),
		md("vp", "c.md", "---\nid: c\nabout: cs/analysis@9\n---\nx"),
	)
	q := ids(ix.Query(Filter{About: "cs/analysis"}))
	am := mustNode(t, ix, "cs/analysis").AboutMe
	if strings.Join(q, ",") != "vp/c" || strings.Join(am, ",") != "vp/c" {
		t.Errorf("query %v vs about_me %v", q, am)
	}
	c := mustNode(t, ix, "vp/c")
	if c.About.Resolved || !hasLine(c.Problems, "about cs/analysis@9: no such version") {
		t.Errorf("bad pin should be a problem and unresolved: %+v %v", c.About, c.Problems)
	}
}

func TestSummaryTruncationIsValidUTF8(t *testing.T) {
	// 100 two-byte runes is 200 bytes: over a 100-byte cut, under the
	// rune limit, so it must come back whole.
	if s := firstLine(strings.Repeat("é", 100)); s != strings.Repeat("é", 100) {
		t.Errorf("a 100-rune line must not be cut: %q", s)
	}
	s := firstLine(strings.Repeat("é", 200))
	if !utf8.ValidString(s) {
		t.Errorf("summary is not valid UTF-8: %q", s)
	}
	if !strings.HasSuffix(s, "…") || utf8.RuneCountInString(s) > summaryMaxLen {
		t.Errorf("summary should be cut on a rune boundary to at most %d runes: %q", summaryMaxLen, s)
	}
}

func TestStringListErrorNamesTheShape(t *testing.T) {
	ix := build(t, md("vp", "n.md", "---\ndepends_on: {a: b}\n---\nx"))
	n := mustNode(t, ix, "vp/n")
	if n.Rejected == "" || strings.Contains(n.Rejected, "kind 4") {
		t.Errorf("rejection should name the shape, not a yaml kind number: %q", n.Rejected)
	}
}
