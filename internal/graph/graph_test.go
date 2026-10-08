package graph

import (
	"strings"
	"testing"
	"time"
)

var t0 = time.Date(2026, 9, 21, 12, 0, 0, 0, time.UTC)

func md(owner, path, body string) FileInput {
	return FileInput{Owner: owner, Path: path, Markdown: true, Body: []byte(body),
		Versions: []Version{{N: 1, TS: t0, SHA: "sha-" + path}}}
}

func bin(owner, path string) FileInput {
	return FileInput{Owner: owner, Path: path, Versions: []Version{{N: 1, TS: t0, SHA: "sha-" + path}}}
}

var org = []AgentInput{
	{Slug: "chief-of-staff", Role: "Chief of Staff", ReportsTo: "ceo"},
	{Slug: "vp", Role: "Engineering lead", ReportsTo: "ceo"},
	{Slug: "cs", Role: "Chief Scientist", ReportsTo: "vp"},
	{Slug: "old", Role: "Former analyst", ReportsTo: "vp", Archived: true},
}

func build(t *testing.T, files ...FileInput) *Index {
	t.Helper()
	return Build(Input{Now: t0, Agents: org, Files: files})
}

func mustNode(t *testing.T, ix *Index, id string) *Node {
	t.Helper()
	n, ok := ix.Get(id)
	if !ok {
		t.Fatalf("node %q missing; have %v", id, ids(ix.Sorted()))
	}
	return n
}

func ids(nodes []*Node) []string {
	out := make([]string, 0, len(nodes))
	for _, n := range nodes {
		out = append(out, n.ID)
	}
	return out
}

func hasLine(lines []string, sub string) bool {
	for _, l := range lines {
		if strings.Contains(l, sub) {
			return true
		}
	}
	return false
}

func TestAgentsAreNodesAndTheCEOAlwaysExists(t *testing.T) {
	ix := build(t)
	ceo := mustNode(t, ix, "ceo")
	if ceo.Type != TypeAgent || ceo.Status != StatusActive || ceo.Owner != "ceo" {
		t.Errorf("ceo = %+v", ceo)
	}
	vp := mustNode(t, ix, "vp")
	if vp.Owner != ChiefOfStaffSlug || vp.ReportsTo != "ceo" || vp.Summary != "Engineering lead" {
		t.Errorf("vp = %+v", vp)
	}
	if mustNode(t, ix, "old").Status != StatusArchived {
		t.Error("archived agent should carry status archived")
	}
}

func TestBareFilesAreArtifactsWithDerivedIds(t *testing.T) {
	ix := build(t,
		md("vp", "notes/Plan B.md", "# Plan B\n\nsome text"),
		bin("vp", "dist/Release V3.ZIP"),
	)
	n := mustNode(t, ix, "vp/notes/plan-b")
	if n.Manifested || n.Rejected != "" || n.Kind != "" || n.Status != StatusCurrent {
		t.Errorf("bare markdown = %+v", n)
	}
	if n.Summary != "" {
		t.Errorf("a bare file has no summary to offer; got %q", n.Summary)
	}
	z := mustNode(t, ix, "vp/dist/release-v3.zip")
	if z.Path != "dist/Release V3.ZIP" || z.CurrentVersion().N != 1 {
		t.Errorf("bare binary = %+v", z)
	}
}

func TestManifestFieldsLandOnTheNode(t *testing.T) {
	ix := build(t,
		md("vp", "ingest.md", "---\nid: ingest\n---\n# Ingest service\n\nThe design."),
		md("vp", "reqs/retries.md", `---
id: no-dropped-batches
kind: requirement
about: vp/ingest
status: provisional
condition: until the load test lands
check: a full day's ingest log shows zero dropped batches
evidence: ["[[ep:2026-09-17T01]]"]
depends_on: ceo/never-lose-data
---
No batch is ever dropped.
Second line.`),
		FileInput{Owner: "ceo", Path: "never-lose-data", Markdown: true, Body: []byte("---\nkind: requirement\nabout: vp/ingest\n---\nNever lose customer data."),
			Versions: []Version{{N: 1, TS: t0, SHA: "a"}, {N: 2, TS: t0, SHA: "b"}}},
	)
	n := mustNode(t, ix, "vp/no-dropped-batches")
	if !n.Manifested || n.Kind != KindRequirement || n.Status != StatusProvisional || n.DeclaredStatus != StatusProvisional {
		t.Fatalf("node = %+v", n)
	}
	if n.Condition == "" || n.Check == "" || len(n.Evidence) != 1 || n.Lines != 2 {
		t.Errorf("fields = cond %q check %q evidence %v lines %d", n.Condition, n.Check, n.Evidence, n.Lines)
	}
	if n.Summary != "No batch is ever dropped." {
		t.Errorf("summary should be the body's first line; got %q", n.Summary)
	}
	if n.About == nil || !n.About.Resolved || n.About.ID != "vp/ingest" {
		t.Errorf("about = %+v", n.About)
	}
	if len(n.DependsOn) != 1 || !n.DependsOn[0].Resolved {
		t.Errorf("depends_on = %+v", n.DependsOn)
	}
	if len(n.Problems) != 0 || n.Flagged {
		t.Errorf("clean node should have no problems or flags; got %v %v", n.Problems, n.Flags)
	}
	api := mustNode(t, ix, "vp/ingest")
	if want := []string{"ceo/never-lose-data", "vp/no-dropped-batches"}; strings.Join(api.AboutMe, ",") != strings.Join(want, ",") {
		t.Errorf("about_me = %v, want %v", api.AboutMe, want)
	}
	if deps := mustNode(t, ix, "ceo/never-lose-data").Dependents; len(deps) != 1 || deps[0] != "vp/no-dropped-batches" {
		t.Errorf("dependents = %v", deps)
	}
}

func TestDeclaredIdWinsOverPath(t *testing.T) {
	ix := build(t, md("vp", "some/deep/file.md", "---\nid: stable\n---\nbody"))
	n := mustNode(t, ix, "vp/stable")
	if n.Path != "some/deep/file.md" {
		t.Errorf("path = %q", n.Path)
	}
	if _, derived := ix.Get("vp/some/deep/file"); derived {
		t.Error("a file with a declared id must not also appear under its derived id")
	}
}

func TestRejections(t *testing.T) {
	cases := []struct {
		name string
		body string
		want string
	}{
		{"bad yaml", "---\nkind: [\n---\nx", "not valid YAML"},
		{"unknown kind", "---\nkind: datasheet\nsource: x\n---\nx", `kind "datasheet" is not one of requirement, decision, certificate, reference`},
		{"unknown status", "---\nstatus: frozen\n---\nx", `status "frozen" is not one of`},
		{"provisional without condition", "---\nstatus: provisional\n---\nx", "requires a condition"},
		{"requirement without about", "---\nkind: requirement\n---\nx", "must say what it is about"},
		{"decision without about", "---\nkind: decision\n---\nx", "must say what it is about"},
		{"certificate without deps", "---\nkind: certificate\nabout: vp/x\n---\nx", "at least one pinned"},
		{"certificate unpinned dep", "---\nkind: certificate\nabout: vp/x\ndepends_on: [vp/y@1, vp/z]\n---\nx", `"vp/z" is not pinned`},
		{"reference without source", "---\nkind: reference\n---\nx", "must name its source"},
		{"bad id", "---\nid: Not A Slug\n---\nx", "is not a slug path"},
		{"missing payload", "---\nfile: gone.zip\n---\nx", "does not exist beside"},
		{"escaping payload", "---\nfile: ../../etc/passwd\n---\nx", "escapes the public directory"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ix := build(t, md("vp", "n.md", tc.body))
			n := mustNode(t, ix, "vp/n")
			if n.Manifested {
				t.Fatalf("manifest should have been rejected")
			}
			if !strings.Contains(n.Rejected, tc.want) {
				t.Errorf("rejected = %q, want it to contain %q", n.Rejected, tc.want)
			}
			if n.Kind != "" || n.About != nil || len(n.DependsOn) != 0 || n.Status != StatusCurrent {
				t.Errorf("a rejected manifest must be indexed as a bare artifact; got %+v", n)
			}
			findings := ix.OwnerFindings("vp")
			if !hasLine(findings, "n.md: front matter rejected") {
				t.Errorf("owner findings should carry the rejection; got %v", findings)
			}
		})
	}
}

// A markdown file that opens with a horizontal rule and prose is not a
// manifest, however much its first line looks like a fence. Plenty of
// ordinary documents look like this; they must index as bare
// artifacts, not as rejections the owner is nagged about.
func TestFenceAroundProseIsNotFrontMatter(t *testing.T) {
	cases := map[string]string{
		"prose between rules":  "---\nSome introductory paragraph.\n---\n\n# Real heading\n",
		"list between rules":   "---\n- a bullet\n- another\n---\nbody\n",
		"number between rules": "---\n42\n---\nbody\n",
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			ix := build(t, md("vp", "doc.md", body))
			n := mustNode(t, ix, "vp/doc")
			if n.Rejected != "" || n.Manifested {
				t.Errorf("non-mapping front matter should be a bare artifact; got rejected=%q manifested=%v", n.Rejected, n.Manifested)
			}
		})
	}
	// Two fences with nothing between them are a bare artifact too.
	ix := build(t, md("vp", "empty.md", "---\n---\nbody\n"))
	if n := mustNode(t, ix, "vp/empty"); n.Manifested || n.Rejected != "" || n.Status != StatusCurrent {
		t.Errorf("empty front matter = %+v", n)
	}
	// A markdown file too large to parse still derives the same id a
	// parseable one would, so shrinking it later does not move it.
	ix = build(t, FileInput{Owner: "vp", Path: "huge.md", Markdown: false, Versions: []Version{{N: 1, TS: t0, SHA: "h"}}})
	mustNode(t, ix, "vp/huge")
	// A mapping that is not valid YAML is still a rejection.
	ix = build(t, md("vp", "bad.md", "---\nkind: [\n---\nbody\n"))
	if n := mustNode(t, ix, "vp/bad"); n.Rejected == "" {
		t.Error("broken YAML mapping must still be rejected")
	}
}

func TestRejectedManifestKeepsItsDeclaredId(t *testing.T) {
	// A rule violation must not move the node: peers already point at
	// the declared id, and a cascade of dangling edges would hide the
	// one real fault.
	ix := build(t, md("vp", "path.md", "---\nid: declared\nkind: requirement\n---\nx"))
	n := mustNode(t, ix, "vp/declared")
	if n.Rejected == "" {
		t.Fatal("expected a rejection")
	}
}

func TestDefaultStatusIsCurrentAndKindIsOptional(t *testing.T) {
	ix := build(t, md("vp", "spec.md", "---\nid: spec\nsummary: The API spec\n---\nlong body"))
	n := mustNode(t, ix, "vp/spec")
	if !n.Manifested || n.Kind != "" || n.Status != StatusCurrent || n.Summary != "The API spec" {
		t.Errorf("node = %+v", n)
	}
}

func TestPayloadFoldsIntoItsManifest(t *testing.T) {
	ix := build(t,
		md("vp", "rel/v3.md", "---\nid: v3\nfile: v3.zip\n---\nRelease 3."),
		bin("vp", "rel/v3.zip"),
		bin("vp", "rel/other.zip"),
	)
	n := mustNode(t, ix, "vp/v3")
	if n.Payload != "rel/v3.zip" {
		t.Errorf("payload = %q", n.Payload)
	}
	if _, asNode := ix.Get("vp/rel/v3.zip"); asNode {
		t.Error("a payload must not be a node of its own")
	}
	mustNode(t, ix, "vp/rel/other.zip")
}

func TestPayloadClaimedTwiceRejectsTheSecond(t *testing.T) {
	ix := build(t,
		md("vp", "a.md", "---\nfile: p.zip\n---\nx"),
		md("vp", "b.md", "---\nfile: p.zip\n---\nx"),
		bin("vp", "p.zip"),
	)
	if a := mustNode(t, ix, "vp/a"); a.Rejected != "" || a.Payload != "p.zip" {
		t.Errorf("first claim should win: %+v", a)
	}
	if b := mustNode(t, ix, "vp/b"); !strings.Contains(b.Rejected, "already the payload of a.md") {
		t.Errorf("second claim should be rejected: %+v", b)
	}
}

func TestPayloadMayNotBeAManifest(t *testing.T) {
	ix := build(t,
		md("vp", "a.md", "---\nfile: b.md\n---\nx"),
		md("vp", "b.md", "---\nid: b\n---\nx"),
	)
	if a := mustNode(t, ix, "vp/a"); !strings.Contains(a.Rejected, "is itself a node manifest") {
		t.Errorf("a = %+v", a)
	}
	mustNode(t, ix, "vp/b")
}

func TestDuplicateIdsResolveInPathOrder(t *testing.T) {
	ix := build(t,
		md("vp", "b.md", "---\nid: same\n---\nsecond by path"),
		md("vp", "a.md", "---\nid: same\n---\nfirst by path"),
	)
	if n := mustNode(t, ix, "vp/same"); n.Path != "a.md" || n.Rejected != "" {
		t.Errorf("first path should keep the id: %+v", n)
	}
	if n := mustNode(t, ix, "vp/b"); !strings.Contains(n.Rejected, `id "same" is also declared by a.md`) {
		t.Errorf("second should fall back to its derived id with a rejection: %+v", n)
	}
}

func TestDerivedIdTakenByDeclaredIdDropsTheFile(t *testing.T) {
	ix := build(t,
		md("vp", "a.md", "---\nid: b\n---\nclaims b"),
		md("vp", "b.md", "plain file whose derived id is b"),
	)
	if n := mustNode(t, ix, "vp/b"); n.Path != "a.md" {
		t.Errorf("declared id sorted first should hold vp/b: %+v", n)
	}
	drops := ix.OwnerFindings("vp")
	if !hasLine(drops, "b.md: not indexed") {
		t.Errorf("owner findings should report the dropped file; got %v", drops)
	}
}

func TestDifferentOwnersMaySharePlainNames(t *testing.T) {
	ix := build(t,
		md("vp", "x.md", "---\nid: notes\n---\nx"),
		md("cs", "x.md", "---\nid: notes\n---\nx"),
	)
	mustNode(t, ix, "vp/notes")
	mustNode(t, ix, "cs/notes")
}

func TestUnresolvedEdges(t *testing.T) {
	ix := build(t,
		md("vp", "r.md", "---\nkind: decision\nabout: vp/ghost\ndepends_on: [vp/nothing, vp/api@9]\nsupersedes: vp/never\n---\nx"),
		md("vp", "api.md", "---\nid: api\n---\nx"),
	)
	n := mustNode(t, ix, "vp/r")
	if !n.Manifested {
		t.Fatalf("relationship faults are problems, not rejections: %+v", n)
	}
	for _, want := range []string{"about vp/ghost: no such node", "depends_on vp/api@9: no such version", "supersedes vp/never: no such node"} {
		if !hasLine(n.Problems, want) {
			t.Errorf("problems %v should contain %q", n.Problems, want)
		}
	}
	if !n.Flagged || !hasLine(n.Flags, "rests on vp/nothing, which does not exist") {
		t.Errorf("a missing premise is a flag; got flagged=%v flags=%v", n.Flagged, n.Flags)
	}
	if hasLine(n.Flags, "vp/api") {
		t.Errorf("a bad pin on an existing node is a problem, not a flag: %v", n.Flags)
	}
}

// A bare release zip gets pinned by a certificate. Its id is its
// filename, so the day the owner ships the next release under a new
// name the pin dangles and the certificate is flagged for a retraction
// that never happened. The index tells the zip's owner while the pin
// still resolves, so they can give the file a manifest with a declared
// id first. Anything with a declared or store-supplied id is left
// alone, and so is a file with no pointers or one already rejected.
func TestPointedAtPathDerivedIdIsAProblemOnTheTarget(t *testing.T) {
	ix := build(t,
		bin("vp", "ingest-1.2.zip"),
		bin("vp", "unpinned.zip"),
		md("vp", "noid.md", "---\nkind: decision\nabout: vp\n---\nx"),
		md("vp", "declared.md", "---\nid: declared\n---\nx"),
		md("vp", "broken.md", "---\nkind: decision\n---\nno about, so rejected"),
		md("cs", "cert.md", "---\nkind: certificate\nabout: vp/ingest-1.2.zip\ndepends_on: [vp/ingest-1.2.zip@1]\n---\nx"),
		md("cs", "rests.md", "---\nid: rests\ndepends_on: [vp/noid, vp/declared, vp/broken]\n---\nx"),
		md("cs", "newer.md", "---\nid: newer\nsupersedes: [vp/noid]\n---\nx"),
		FileInput{Owner: "ceo", Path: "abc123-brief.pdf", Name: "brief.pdf", Project: true, Kind: KindReference, Versions: []Version{{N: 1, TS: t0, SHA: "sha-brief"}}},
		md("cs", "cites.md", "---\nid: cites\nabout: ceo/brief.pdf\n---\nx"),
	)
	zip := mustNode(t, ix, "vp/ingest-1.2.zip")
	if !hasLine(zip.Problems, "pointed at by cs/cert; its id comes from its path") {
		t.Errorf("a pinned bare file should be told who pins it: %v", zip.Problems)
	}
	if !hasLine(zip.Problems, "Declare an id") {
		t.Errorf("the note should say what to do: %v", zip.Problems)
	}
	if c := mustNode(t, ix, "cs/cert"); len(c.Problems) != 0 || c.Flagged {
		t.Errorf("the pointer is fine while the pin resolves: %v %v", c.Problems, c.Flags)
	}
	noid := mustNode(t, ix, "vp/noid")
	if !hasLine(noid.Problems, "pointed at by cs/newer, cs/rests;") {
		t.Errorf("a manifest without id: is path-named too; pointers listed once each, sorted: %v", noid.Problems)
	}
	for _, id := range []string{"vp/unpinned.zip", "vp/declared", "ceo/brief.pdf"} {
		if n := mustNode(t, ix, id); hasLine(n.Problems, "pointed at by") {
			t.Errorf("%s should carry no note: %v", id, n.Problems)
		}
	}
	if b := mustNode(t, ix, "vp/broken"); b.Rejected == "" || hasLine(b.Problems, "pointed at by") {
		t.Errorf("a rejected manifest is already a louder finding; rejected=%q problems=%v", b.Rejected, b.Problems)
	}
}

// The note is a change: it appears on the pass where the first pointer
// lands and moves the target's ChangedSeq, so the owner's wake trailer
// carries it.
func TestPointedAtNoteAdvancesTheTargetsChangedSeq(t *testing.T) {
	first := build(t, bin("vp", "ingest-1.2.zip"))
	before := mustNode(t, first, "vp/ingest-1.2.zip").ChangedSeq
	second := Build(Input{Now: t0, Prev: first, Agents: org, Files: []FileInput{
		bin("vp", "ingest-1.2.zip"),
		md("cs", "cert.md", "---\nkind: certificate\nabout: vp/ingest-1.2.zip\ndepends_on: [vp/ingest-1.2.zip@1]\n---\nx"),
	}})
	after := mustNode(t, second, "vp/ingest-1.2.zip").ChangedSeq
	if after <= before {
		t.Errorf("ChangedSeq should advance when the note appears: before=%d after=%d", before, after)
	}
}

func TestPinsResolveByNumberOrShaPrefix(t *testing.T) {
	target := FileInput{Owner: "cs", Path: "analysis.md", Markdown: true, Body: []byte("---\nid: analysis\n---\nx"),
		Versions: []Version{{N: 1, TS: t0, SHA: "0123456789abcdef"}, {N: 2, TS: t0, SHA: "fedcba9876543210", PayloadSHA: "aaaaaaaabbbbbbbb"}}}
	ix := build(t, target,
		md("vp", "c.md", "---\nkind: certificate\nabout: cs/analysis\ndepends_on: [cs/analysis@2, cs/analysis@0123456, cs/analysis@aaaaaaaa]\n---\nx"),
		md("vp", "d.md", "---\nkind: certificate\nabout: cs/analysis\ndepends_on: [cs/analysis@3, cs/analysis@012345]\n---\nx"),
	)
	c := mustNode(t, ix, "vp/c")
	for i, r := range c.DependsOn {
		if !r.Resolved {
			t.Errorf("dep %d %s should resolve", i, r)
		}
	}
	d := mustNode(t, ix, "vp/d")
	for i, r := range d.DependsOn {
		if r.Resolved {
			t.Errorf("dep %d %s should not resolve (unknown number / prefix under 7 chars)", i, r)
		}
	}
}

func TestSupersededIsDerivedNeverDeclared(t *testing.T) {
	ix := build(t,
		md("vp", "old.md", "---\nid: r1\nkind: decision\nabout: vp\nstatus: current\n---\nold ruling"),
		md("vp", "new.md", "---\nid: r2\nkind: decision\nabout: vp\nsupersedes: vp/r1\n---\nnew ruling"),
		md("vp", "draft.md", "---\nid: r3\nkind: decision\nabout: vp\nstatus: draft\nsupersedes: vp/r2\n---\nnot yet"),
		md("cs", "dep.md", "---\nkind: decision\nabout: vp\ndepends_on: vp/r1\n---\nrests on the old one"),
	)
	old := mustNode(t, ix, "vp/r1")
	if old.Status != StatusSuperseded || old.DeclaredStatus != StatusCurrent || len(old.SupersededBy) != 1 {
		t.Errorf("old = status %s declared %s by %v", old.Status, old.DeclaredStatus, old.SupersededBy)
	}
	if n := mustNode(t, ix, "vp/r2"); n.Status != StatusCurrent {
		t.Errorf("a draft cannot supersede anything; r2 = %s", n.Status)
	}
	dep := mustNode(t, ix, "cs/dep")
	if !dep.Flagged || !hasLine(dep.Flags, "rests on vp/r1, which is superseded") {
		t.Errorf("dependent of a superseded node must be flagged; got %v", dep.Flags)
	}
}

func TestWithdrawnPremiseFlagsEveryNodeDownstream(t *testing.T) {
	// analysis <- certificate <- register entry <- backlog entry: the
	// fact lives at every hop, so a retraction at the top must reach
	// the bottom rather than stop at the first dependent. Each flag
	// names the premise one hop up and nothing loses its status.
	root := md("ceo", "a.md", "---\nid: a\nstatus: withdrawn\n---\nx")
	rest := []FileInput{
		md("vp", "b.md", "---\nid: b\ndepends_on: ceo/a\n---\nx"),
		md("cs", "c.md", "---\nid: c\ndepends_on: vp/b\n---\nx"),
		md("chief-of-staff", "d.md", "---\nid: d\ndepends_on: cs/c\n---\nx"),
	}
	ix := build(t, append([]FileInput{root}, rest...)...)
	for id, want := range map[string]string{
		"vp/b":             "rests on ceo/a, which is withdrawn",
		"cs/c":             "rests on vp/b, which is flagged",
		"chief-of-staff/d": "rests on cs/c, which is flagged",
	} {
		n := mustNode(t, ix, id)
		if !n.Flagged || n.Status != StatusCurrent || len(n.Flags) != 1 || n.Flags[0] != want {
			t.Errorf("%s: flagged=%v status=%s flags=%v; want one flag %q", id, n.Flagged, n.Status, n.Flags, want)
		}
	}
	// Repairing the top clears the whole chain on the next pass.
	root.Body = []byte("---\nid: a\n---\nx")
	ix = build(t, append([]FileInput{root}, rest...)...)
	for _, id := range []string{"vp/b", "cs/c", "chief-of-staff/d"} {
		if n := mustNode(t, ix, id); n.Flagged {
			t.Errorf("%s still flagged after the premise came back: %v", id, n.Flags)
		}
	}
}

func TestAnEdgeDroppedForACycleCarriesNoFlag(t *testing.T) {
	// b rests on withdrawn a and on c; c rests on b. breakCycles drops
	// b→c as a problem on b, so c's flag must not travel back to b.
	ix := build(t,
		md("ceo", "a.md", "---\nid: a\nstatus: withdrawn\n---\nx"),
		md("vp", "b.md", "---\nid: b\ndepends_on:\n  - ceo/a\n  - cs/c\n---\nx"),
		md("cs", "c.md", "---\nid: c\ndepends_on: vp/b\n---\nx"),
	)
	b := mustNode(t, ix, "vp/b")
	if len(b.Flags) != 1 || b.Flags[0] != "rests on ceo/a, which is withdrawn" {
		t.Errorf("b flags = %v", b.Flags)
	}
	if !hasLine(b.Problems, "closes a dependency cycle") {
		t.Errorf("b problems = %v", b.Problems)
	}
	if c := mustNode(t, ix, "cs/c"); !hasLine(c.Flags, "rests on vp/b, which is flagged") {
		t.Errorf("c flags = %v", c.Flags)
	}
}

func TestRejectedPremiseFlags(t *testing.T) {
	ix := build(t,
		md("vp", "a.md", "---\nid: a\nkind: requirement\n---\nno about, rejected"),
		md("cs", "b.md", "---\nid: b\ndepends_on: vp/a\n---\nx"),
	)
	if b := mustNode(t, ix, "cs/b"); !hasLine(b.Flags, "rests on vp/a, whose front matter was rejected") {
		t.Errorf("flags = %v", b.Flags)
	}
}

func TestDependencyCycleDropsOneEdgeDeterministically(t *testing.T) {
	files := []FileInput{
		md("vp", "a.md", "---\nid: a\ndepends_on: vp/b\n---\nx"),
		md("vp", "b.md", "---\nid: b\ndepends_on: vp/c\n---\nx"),
		md("vp", "c.md", "---\nid: c\ndepends_on: vp/a\n---\nx"),
	}
	ix := build(t, files...)
	dropped := 0
	var who string
	for _, id := range []string{"vp/a", "vp/b", "vp/c"} {
		n := mustNode(t, ix, id)
		for _, r := range n.DependsOn {
			if !r.Resolved {
				dropped++
				who = id
			}
		}
	}
	if dropped != 1 {
		t.Fatalf("exactly one edge should be dropped, got %d", dropped)
	}
	if !hasLine(mustNode(t, ix, who).Problems, "closes a dependency cycle") {
		t.Errorf("the losing node should carry the problem")
	}
	// Same input in a different order: same loser.
	again := build(t, files[2], files[0], files[1])
	for _, id := range []string{"vp/a", "vp/b", "vp/c"} {
		for i, r := range mustNode(t, again, id).DependsOn {
			if r.Resolved != mustNode(t, ix, id).DependsOn[i].Resolved {
				t.Fatalf("cycle breaking depends on input order at %s", id)
			}
		}
	}
	// No dependents entry survives for the dropped edge.
	total := 0
	for _, id := range []string{"vp/a", "vp/b", "vp/c"} {
		total += len(mustNode(t, ix, id).Dependents)
	}
	if total != 2 {
		t.Errorf("dependents should reflect two live edges, got %d", total)
	}
}

func TestBindingKindOverLengthIsAProblem(t *testing.T) {
	long := strings.Repeat("a line of substance\n", bindingKindMaxLines+1)
	ix := build(t,
		md("vp", "r.md", "---\nkind: requirement\nabout: vp\n---\n"+long),
		md("vp", "s.md", "---\nid: spec\n---\n"+long),
	)
	if r := mustNode(t, ix, "vp/r"); !r.Manifested || !hasLine(r.Problems, "should be a few lines") {
		t.Errorf("long requirement should be accepted with a problem: %+v", r)
	}
	if s := mustNode(t, ix, "vp/spec"); len(s.Problems) != 0 {
		t.Errorf("length is only a problem for binding kinds: %v", s.Problems)
	}
}

func TestUnknownFrontMatterFieldIsAProblem(t *testing.T) {
	ix := build(t, md("vp", "r.md", "---\ntitle: Hello\ndepend_on: vp/x\n---\nx"))
	n := mustNode(t, ix, "vp/r")
	if !n.Manifested {
		t.Fatalf("unknown fields must not reject: %+v", n)
	}
	if !hasLine(n.Problems, `"depend_on"`) || !hasLine(n.Problems, `"title"`) {
		t.Errorf("problems = %v", n.Problems)
	}
}

func TestAgentsAsSubjectsButNotPremises(t *testing.T) {
	ix := build(t, md("vp", "r.md", "---\nkind: decision\nabout: cs\ndepends_on: cs\n---\nx"))
	n := mustNode(t, ix, "vp/r")
	if n.About == nil || !n.About.Resolved {
		t.Errorf("about an agent should resolve: %+v", n.About)
	}
	if n.DependsOn[0].Resolved || !hasLine(n.Problems, "an agent is not something to rest on") {
		t.Errorf("depends_on an agent should be refused: %+v %v", n.DependsOn, n.Problems)
	}
	if cs := mustNode(t, ix, "cs"); len(cs.AboutMe) != 1 {
		t.Errorf("agent should collect about_me: %v", cs.AboutMe)
	}
}

func TestProjectFilesAreCEOArtifacts(t *testing.T) {
	ix := build(t,
		FileInput{Owner: "ceo", Path: "Business Plan.pdf", Project: true, Kind: KindReference, Summary: "The plan", Source: "ceo upload",
			Versions: []Version{{N: 1, TS: t0, SHA: "p1"}, {N: 2, TS: t0, SHA: "p2"}}},
		FileInput{Owner: "ceo", Path: "odd.txt", Project: true, Kind: "datasheet"},
	)
	n := mustNode(t, ix, "ceo/business-plan.pdf")
	if n.Kind != KindReference || n.Summary != "The plan" || n.CurrentVersion().N != 2 || n.Status != StatusCurrent {
		t.Errorf("project node = %+v", n)
	}
	if odd := mustNode(t, ix, "ceo/odd.txt"); odd.Kind != "" || !hasLine(odd.Problems, `kind "datasheet"`) {
		t.Errorf("an unlisted upload kind is dropped with a problem: %+v", odd)
	}
}

func TestQueryFilters(t *testing.T) {
	ix := build(t,
		md("vp", "api.md", "---\nid: api\n---\nx"),
		md("vp", "r1.md", "---\nid: r1\nkind: requirement\nabout: vp/api\ncheck: measure it\n---\nx"),
		md("vp", "r2.md", "---\nid: r2\nkind: requirement\nabout: vp/api\nstatus: draft\n---\nx"),
		md("cs", "d1.md", "---\nid: d1\nkind: decision\nabout: vp/api\nstatus: provisional\ncondition: c\n---\nx"),
		md("cs", "rep.md", "---\nid: rep\nabout: vp/api\n---\nfindings"),
		md("cs", "old.md", "---\nid: old\nkind: decision\nabout: vp/api\nstatus: withdrawn\n---\nx"),
	)
	// Q1: what binds api right now.
	got := ids(ix.Query(Filter{About: "vp/api", KindSet: []Kind{KindRequirement, KindDecision}, InForce: true}))
	if want := "cs/d1,vp/r1"; strings.Join(got, ",") != want {
		t.Errorf("binding = %v, want %s", got, want)
	}
	// Q7: everything about api.
	if got := ids(ix.Query(Filter{About: "vp/api"})); len(got) != 5 {
		t.Errorf("about = %v", got)
	}
	// Q6: by owner.
	if got := ids(ix.Query(Filter{Owner: "cs", Type: TypeArtifact})); strings.Join(got, ",") != "cs/d1,cs/old,cs/rep" {
		t.Errorf("owner = %v", got)
	}
	// Q5: checks.
	var checks []string
	for _, n := range ix.Query(Filter{About: "vp/api", Kind: KindRequirement, InForce: true}) {
		if n.Check != "" {
			checks = append(checks, n.ID)
		}
	}
	if strings.Join(checks, ",") != "vp/r1" {
		t.Errorf("checks = %v", checks)
	}
	if got := ids(ix.Query(Filter{Type: TypeAgent, Status: StatusArchived})); strings.Join(got, ",") != "old" {
		t.Errorf("archived agents = %v", got)
	}
}

func TestChangedSeqAdvancesOnlyWhenSomethingChanged(t *testing.T) {
	a := md("vp", "a.md", "---\nid: a\n---\nx")
	b := md("cs", "b.md", "---\nid: b\ndepends_on: vp/a\n---\nx")
	first := Build(Input{Now: t0, Agents: org, Files: []FileInput{a, b}})
	if first.Seq != 1 {
		t.Fatalf("first build seq = %d", first.Seq)
	}
	same := Build(Input{Now: t0.Add(time.Hour), Prev: first, Agents: org, Files: []FileInput{a, b}})
	if same.Seq != 1 {
		t.Errorf("unchanged inputs must not advance seq; got %d", same.Seq)
	}
	if len(same.ChangedSince(1)) != 0 {
		t.Errorf("nothing changed since 1; got %v", ids(same.ChangedSince(1)))
	}
	// A new version of a flips a's fingerprint; b is untouched (a pin
	// would be, a status change would flag it, a new version does not).
	a2 := a
	a2.Versions = append([]Version{}, a.Versions...)
	a2.Versions = append(a2.Versions, Version{N: 2, TS: t0, SHA: "sha-a2"})
	third := Build(Input{Now: t0, Prev: same, Agents: org, Files: []FileInput{a2, b}})
	if third.Seq != 2 {
		t.Errorf("seq = %d", third.Seq)
	}
	if got := ids(third.ChangedSince(1)); strings.Join(got, ",") != "vp/a" {
		t.Errorf("changed since 1 = %v", got)
	}
	// Withdrawing a flags b: both change.
	aw := md("vp", "a.md", "---\nid: a\nstatus: withdrawn\n---\nx")
	aw.Versions = a2.Versions
	fourth := Build(Input{Now: t0, Prev: third, Agents: org, Files: []FileInput{aw, b}})
	if got := ids(fourth.ChangedSince(2)); strings.Join(got, ",") != "cs/b,vp/a" {
		t.Errorf("changed since 2 = %v", got)
	}
	// Removing a node advances seq even though no remaining node changed.
	fifth := Build(Input{Now: t0, Prev: fourth, Agents: org, Files: []FileInput{aw}})
	if fifth.Seq != fourth.Seq+1 {
		t.Errorf("removal should advance seq: %d vs %d", fifth.Seq, fourth.Seq)
	}
}

func TestChangedSeqSurvivesAJSONRoundTrip(t *testing.T) {
	// The previous index comes back from disk without the unexported
	// fingerprint; ChangedSeq must still hold steady.
	a := md("vp", "a.md", "---\nid: a\nkind: decision\nabout: vp\n---\nx")
	first := Build(Input{Now: t0, Agents: org, Files: []FileInput{a}})
	restored := &Index{Seq: first.Seq, Nodes: map[string]*Node{}}
	for id, n := range first.Nodes {
		c := *n
		c.fingerprint = ""
		restored.Nodes[id] = &c
	}
	second := Build(Input{Now: t0, Prev: restored, Agents: org, Files: []FileInput{a}})
	if second.Seq != first.Seq || mustNode(t, second, "vp/a").ChangedSeq != 1 {
		t.Errorf("seq %d→%d, changed_seq %d", first.Seq, second.Seq, mustNode(t, second, "vp/a").ChangedSeq)
	}
}

func TestRelevantTo(t *testing.T) {
	ix := build(t,
		md("vp", "api.md", "---\nid: api\n---\nx"),
		md("cs", "r.md", "---\nid: r\nkind: decision\nabout: vp/api\n---\nx"),
		md("cs", "a.md", "---\nid: a\n---\nx"),
		md("vp", "d.md", "---\nid: d\ndepends_on: cs/a\n---\nx"),
		md("cs", "unrelated.md", "---\nid: u\n---\nx"),
	)
	cases := map[string]bool{
		"vp/api": true, // owned
		"cs/r":   true, // about something vp owns
		"cs/a":   true, // vp/d depends on it
		"vp":     true, // itself
		"cs/u":   false,
		"cs":     false,
	}
	for id, want := range cases {
		if got := ix.RelevantTo("vp", mustNode(t, ix, id)); got != want {
			t.Errorf("RelevantTo(vp, %s) = %v, want %v", id, got, want)
		}
	}
}

func TestParseRefAndNames(t *testing.T) {
	if r := ParseRef(" vp/x@12 "); r.ID != "vp/x" || r.Pin != "12" {
		t.Errorf("ref = %+v", r)
	}
	if r := ParseRef("vp/x"); r.Pin != "" || r.String() != "vp/x" {
		t.Errorf("ref = %+v", r)
	}
	for in, want := range map[string]string{
		"Notes/Plan B.md":   "notes/plan-b",
		"a/b/c.MD":          "a/b/c",
		"weird name!!.md":   "weird-name",
		"README":            "readme",
		"/leading/slash.md": "leading/slash",
	} {
		if got := DeriveName(in); got != want {
			t.Errorf("DeriveName(%q) = %q, want %q", in, got, want)
		}
	}
	for name, ok := range map[string]bool{"a": true, "a/b-c.d_e": true, "A": false, "-a": false, "a//b": false, "": false} {
		if ValidName(name) != ok {
			t.Errorf("ValidName(%q) = %v", name, !ok)
		}
	}
	if p, ok := ResolvePayload("rel/x.md", "sub/y.zip"); !ok || p != "rel/sub/y.zip" {
		t.Errorf("payload = %q %v", p, ok)
	}
	if p, ok := ResolvePayload("rel/x.md", "/top.zip"); !ok || p != "top.zip" {
		t.Errorf("absolute payload = %q %v", p, ok)
	}
	if _, ok := ResolvePayload("x.md", "../out.zip"); ok {
		t.Error("escape should fail")
	}
}

func TestOwnerFindingsAreSortedAndScoped(t *testing.T) {
	ix := build(t,
		md("vp", "z.md", "---\nkind: requirement\n---\nx"),
		md("vp", "a.md", "---\nabout: vp/ghost\n---\nx"),
		md("cs", "c.md", "---\nkind: requirement\n---\nx"),
	)
	got := ix.OwnerFindings("vp")
	if len(got) != 2 || !strings.HasPrefix(got[0], "a.md:") || !strings.HasPrefix(got[1], "z.md:") {
		t.Errorf("findings = %v", got)
	}
	if len(ix.OwnerFindings("ceo")) != 0 {
		t.Error("ceo has nothing to fix")
	}
}

// Version is the lookup graph_node uses to serve a pinned version's
// text: a number, or a SHA prefix of seven or more characters of
// either hash; anything else, and an agent, resolve to nothing.
func TestNodeVersionResolvesAPin(t *testing.T) {
	n := &Node{ID: "cs/analysis", Type: TypeArtifact, Versions: []Version{
		{N: 1, TS: t0, SHA: "0123456789abcdef"},
		{N: 2, TS: t0, SHA: "fedcba9876543210", PayloadSHA: "aaaaaaaabbbbbbbb"},
	}}
	for pin, want := range map[string]int{"1": 1, "2": 2, "0123456": 1, "fedcba98": 2, "aaaaaaaa": 2} {
		v, ok := n.Version(pin)
		if !ok || v.N != want {
			t.Errorf("Version(%q) = %+v, %v; want v%d", pin, v, ok, want)
		}
	}
	for _, pin := range []string{"", "3", "0", "012345", "zzzzzzzz"} {
		if v, ok := n.Version(pin); ok {
			t.Errorf("Version(%q) = %+v; want no match", pin, v)
		}
	}
	agent := &Node{ID: "cs", Type: TypeAgent}
	if _, ok := agent.Version("1"); ok {
		t.Error("an agent has no versions to pin")
	}
}
