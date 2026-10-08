package agent

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kivali-ai/kivali/internal/assignments"
	"github.com/kivali-ai/kivali/internal/graph"
	"github.com/kivali-ai/kivali/internal/store"
)

var graphT0 = time.Date(2026, 9, 21, 12, 0, 0, 0, time.UTC)

func wakeUpdateStore(t *testing.T) *store.FSStore {
	t.Helper()
	s, err := store.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range []store.Agent{
		{Slug: "chief-of-staff", Role: "Chief of Staff", ReportsTo: "ceo"},
		{Slug: "vp", Role: "VP", ReportsTo: "ceo"},
		{Slug: "cs", Role: "Scientist", ReportsTo: "vp"},
	} {
		if err := s.CreateAgent(a, "# role"); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.WriteHandbook("v1"); err != nil {
		t.Fatal(err)
	}
	return s
}

var wakeUpdateOrg = []graph.AgentInput{
	{Slug: "chief-of-staff", Role: "Chief of Staff", ReportsTo: "ceo"},
	{Slug: "vp", Role: "VP", ReportsTo: "ceo"},
	{Slug: "cs", Role: "Scientist", ReportsTo: "vp"},
}

func gfile(owner, path, body string, versions ...graph.Version) graph.FileInput {
	if len(versions) == 0 {
		versions = []graph.Version{{N: 1, TS: graphT0, SHA: "sha-" + path}}
	}
	return graph.FileInput{Owner: owner, Path: path, Markdown: true, Body: []byte(body), Versions: versions}
}

func gbuild(prev *graph.Index, files ...graph.FileInput) *graph.Index {
	return graph.Build(graph.Input{Now: graphT0, Prev: prev, Agents: wakeUpdateOrg, Files: files})
}

func TestWakeUpdateFirstWakeIsSilentAndSetsTheWatermark(t *testing.T) {
	s := wakeUpdateStore(t)
	ix := gbuild(nil, gfile("vp", "api.md", "---\nid: api\n---\nx"))
	text, err := PrepareWakeUpdate(s, ix, "vp")
	if err != nil || text != "" {
		t.Fatalf("first wake: text=%q err=%v", text, err)
	}
	wm, ok, err := s.ReadGraphWatermark("vp")
	if err != nil || !ok || wm.Seq != ix.Seq || wm.Fingerprints["handbook"] == "" {
		t.Errorf("watermark = %+v ok=%v err=%v", wm, ok, err)
	}
}

func TestWakeUpdateListsRelevantChangesAndIgnoresTheRest(t *testing.T) {
	s := wakeUpdateStore(t)
	api := gfile("vp", "api.md", "---\nid: api\nsummary: Release plan\n---\nx")
	first := gbuild(nil, api)
	if _, err := PrepareWakeUpdate(s, first, "vp"); err != nil {
		t.Fatal(err)
	}
	// cs publishes a decision about vp's api (relevant to vp) and an
	// unrelated note (not relevant).
	second := gbuild(first, api,
		gfile("cs", "r.md", "---\nid: no-cap\nkind: decision\nabout: vp/api\n---\nRetries stay unbounded"),
		gfile("cs", "note.md", "---\nid: note\n---\nunrelated"),
	)
	text, err := PrepareWakeUpdate(s, second, "vp")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		WakeUpdateLead,
		"Changed in the knowledge graph since your last turn",
		"- cs/no-cap  decision current v1 · owner cs · about vp/api — Retries stay unbounded",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("trailer missing %q:\n%s", want, text)
		}
	}
	if strings.Contains(text, "cs/note") || strings.Contains(text, "elsewhere") {
		t.Errorf("an unrelated node is neither named nor counted:\n%s", text)
	}
	// Watermark advanced; a third wake with nothing new is silent.
	text, err = PrepareWakeUpdate(s, second, "vp")
	if err != nil || text != "" {
		t.Errorf("quiet wake: text=%q err=%v", text, err)
	}
}

// A wake after which only unrelated nodes changed produces no trailer
// at all: nothing is appended to the chat, no card is shown, and the
// watermark still advances so the next relevant change is reported
// against the right baseline.
func TestWakeUpdateIsSilentWhenOnlyUnrelatedNodesChanged(t *testing.T) {
	s := wakeUpdateStore(t)
	api := gfile("vp", "api.md", "---\nid: api\nsummary: Release plan\n---\nx")
	first := gbuild(nil, api)
	if _, err := PrepareWakeUpdate(s, first, "vp"); err != nil {
		t.Fatal(err)
	}
	second := gbuild(first, api,
		gfile("cs", "a.md", "---\nid: a\n---\nunrelated"),
		gfile("cs", "b.md", "---\nid: b\n---\nunrelated"),
	)
	text, err := PrepareWakeUpdate(s, second, "vp")
	if err != nil {
		t.Fatal(err)
	}
	if text != "" {
		t.Fatalf("four unrelated changes should be no trailer, got:\n%s", text)
	}
	wm, ok, err := s.ReadGraphWatermark("vp")
	if err != nil || !ok || wm.Seq != second.Seq {
		t.Fatalf("watermark did not advance on a silent wake: %+v ok=%v err=%v", wm, ok, err)
	}
	// The next relevant change is reported on its own.
	third := gbuild(second, api,
		gfile("cs", "a.md", "---\nid: a\n---\nunrelated"),
		gfile("cs", "b.md", "---\nid: b\n---\nunrelated"),
		gfile("cs", "r.md", "---\nid: no-cap\nkind: decision\nabout: vp/api\n---\nRetries stay unbounded"),
	)
	text, err = PrepareWakeUpdate(s, third, "vp")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(text, "cs/no-cap") || strings.Contains(text, "elsewhere") {
		t.Fatalf("trailer after a relevant change:\n%s", text)
	}
}

func TestWakeUpdateReportsFlagsOnOwnNodes(t *testing.T) {
	s := wakeUpdateStore(t)
	premise := gfile("cs", "a.md", "---\nid: analysis\n---\nnumbers")
	dep := gfile("vp", "d.md", "---\nid: d\nkind: decision\nabout: vp\ndepends_on: cs/analysis\n---\nrests on the analysis")
	first := gbuild(nil, premise, dep)
	if _, err := PrepareWakeUpdate(s, first, "vp"); err != nil {
		t.Fatal(err)
	}
	withdrawn := gfile("cs", "a.md", "---\nid: analysis\nstatus: withdrawn\n---\nnumbers",
		graph.Version{N: 1, TS: graphT0, SHA: "sha-a.md"}, graph.Version{N: 2, TS: graphT0, SHA: "sha-a2"})
	second := gbuild(first, withdrawn, dep)
	text, err := PrepareWakeUpdate(s, second, "vp")
	if err != nil {
		t.Fatal(err)
	}
	// Both the withdrawn premise (vp rests on it) and vp's own flagged
	// decision are relevant.
	for _, want := range []string{"cs/analysis  artifact withdrawn v2", "vp/d  decision current, flagged v1"} {
		if !strings.Contains(text, want) {
			t.Errorf("trailer missing %q:\n%s", want, text)
		}
	}
}

func TestWakeUpdateReportsAFlagTwoHopsDown(t *testing.T) {
	// cs/analysis <- vp/a2 <- chief-of-staff/s58: the Chief of Staff
	// rests on the analysis only through vp's node, and must still be
	// told at its next wake that its own node is flagged.
	s := wakeUpdateStore(t)
	premise := gfile("cs", "a.md", "---\nid: analysis\n---\nnumbers")
	mid := gfile("vp", "a2.md", "---\nid: a2\ndepends_on: cs/analysis\n---\nrests on the analysis")
	far := gfile("chief-of-staff", "s58.md", "---\nid: s58\ndepends_on: vp/a2\n---\nrests on a2")
	first := gbuild(nil, premise, mid, far)
	if _, err := PrepareWakeUpdate(s, first, "chief-of-staff"); err != nil {
		t.Fatal(err)
	}
	withdrawn := gfile("cs", "a.md", "---\nid: analysis\nstatus: withdrawn\n---\nnumbers",
		graph.Version{N: 1, TS: graphT0, SHA: "sha-a.md"}, graph.Version{N: 2, TS: graphT0, SHA: "sha-a-v2"})
	second := gbuild(first, withdrawn, mid, far)
	text, err := PrepareWakeUpdate(s, second, "chief-of-staff")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"chief-of-staff/s58  artifact current, flagged v1", "vp/a2  artifact current, flagged v1"} {
		if !strings.Contains(text, want) {
			t.Errorf("trailer missing %q:\n%s", want, text)
		}
	}
}

func TestWakeUpdateReportsFindingsOnceUntilTheyChange(t *testing.T) {
	s := wakeUpdateStore(t)
	first := gbuild(nil)
	if _, err := PrepareWakeUpdate(s, first, "vp"); err != nil {
		t.Fatal(err)
	}
	bad := gfile("vp", "bad.md", "---\nkind: requirement\n---\nno about")
	second := gbuild(first, bad)
	text, err := PrepareWakeUpdate(s, second, "vp")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(text, "The index found problems in your published files") || !strings.Contains(text, "bad.md: front matter rejected: a requirement must say what it is about") {
		t.Errorf("findings missing:\n%s", text)
	}
	// Same findings, nothing else new: silence, not a nag.
	text, err = PrepareWakeUpdate(s, second, "vp")
	if err != nil || text != "" {
		t.Errorf("unchanged findings should be silent: text=%q err=%v", text, err)
	}
	// Fixed: the findings clear and the node is the agent's own edit,
	// which it knows about. Nothing to say.
	fixed := gfile("vp", "bad.md", "---\nkind: requirement\nabout: vp\n---\nnow about something",
		graph.Version{N: 1, TS: graphT0, SHA: "sha-bad.md"}, graph.Version{N: 2, TS: graphT0, SHA: "sha-bad2"})
	third := gbuild(second, fixed)
	text, err = PrepareWakeUpdate(s, third, "vp")
	if err != nil || text != "" {
		t.Errorf("after the fix the trailer should be silent: %q err=%v", text, err)
	}
}

// An agent's own edits are not news to it. Only what the index derived
// about its nodes is: a flag from a withdrawn premise, or a peer's
// supersession. Peers' nodes about its objects are still reported, and
// its own edit does not even count among "elsewhere".
func TestWakeUpdateOmitsTheAgentsOwnEdits(t *testing.T) {
	s := wakeUpdateStore(t)
	api := gfile("vp", "api.md", "---\nid: api\n---\nv1")
	first := gbuild(nil, api)
	if _, err := PrepareWakeUpdate(s, first, "vp"); err != nil {
		t.Fatal(err)
	}
	// vp edits its own node: silence.
	api2 := gfile("vp", "api.md", "---\nid: api\n---\nv2",
		graph.Version{N: 1, TS: graphT0, SHA: "sha-api.md"}, graph.Version{N: 2, TS: graphT0, SHA: "sha-api2"})
	second := gbuild(first, api2)
	if text, err := PrepareWakeUpdate(s, second, "vp"); err != nil || text != "" {
		t.Errorf("own edit echoed back: %q err=%v", text, err)
	}
	// A peer's decision about vp's node: the peer's node is reported,
	// vp's node (whose about-me list moved) is not listed a second time.
	third := gbuild(second, api2, gfile("cs", "d.md", "---\nid: d\nkind: decision\nabout: vp/api\n---\nx"))
	text, err := PrepareWakeUpdate(s, third, "vp")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(text, "- cs/d  decision") || strings.Contains(text, "- vp/api ") {
		t.Errorf("peer's node should be the only line:\n%s", text)
	}
	// A peer supersedes vp's node: that is news to vp.
	fourth := gbuild(third, api2,
		gfile("cs", "d.md", "---\nid: d\nkind: decision\nabout: vp/api\n---\nx"),
		gfile("cs", "b2.md", "---\nid: api-2\nsupersedes: vp/api\n---\nx"))
	text, err = PrepareWakeUpdate(s, fourth, "vp")
	if err != nil || !strings.Contains(text, "- vp/api  artifact superseded") {
		t.Errorf("supersession of an own node must be reported:\n%s\nerr=%v", text, err)
	}
}

// Findings that already exist when an agent's watermark is first set —
// front matter the graph rejects —
// must still reach the owner once. The silent bootstrap sets the
// watermark; it must not also mark those findings as already shown.
func TestWakeUpdateReportsFindingsPresentAtBootstrap(t *testing.T) {
	s := wakeUpdateStore(t)
	bad := gfile("vp", "datasheet.md", "---\nkind: datasheet\n---\na file with a kind the graph does not know")
	ix := gbuild(nil, bad)
	if text, err := PrepareWakeUpdate(s, ix, "vp"); err != nil || text != "" {
		t.Fatalf("bootstrap wake should be silent: text=%q err=%v", text, err)
	}
	text, err := PrepareWakeUpdate(s, ix, "vp")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(text, `datasheet.md: front matter rejected: kind "datasheet"`) {
		t.Errorf("pre-existing findings never reached the owner:\n%q", text)
	}
	// And only once.
	if text, err := PrepareWakeUpdate(s, ix, "vp"); err != nil || text != "" {
		t.Errorf("third wake should be quiet: text=%q err=%v", text, err)
	}
}

// Build reads and Commit writes; the caller appends in between. If the
// watermark moved on Build, a failed append would lose that window's
// changes for good.
func TestWakeUpdateBuildDoesNotMoveTheWatermark(t *testing.T) {
	s := wakeUpdateStore(t)
	ix := gbuild(nil, gfile("vp", "a.md", "---\nid: a\n---\nx"))
	u, err := BuildWakeUpdate(s, ix, "vp")
	if err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := s.ReadGraphWatermark("vp"); ok {
		t.Fatal("Build must not write the watermark")
	}
	if err := CommitWakeUpdate(s, "vp", u); err != nil {
		t.Fatal(err)
	}
	wm, ok, _ := s.ReadGraphWatermark("vp")
	if !ok || wm.Seq != ix.Seq {
		t.Errorf("after Commit: ok=%v wm=%+v", ok, wm)
	}
	// A second Build against the same index, uncommitted, keeps
	// producing the same (empty) update: nothing was consumed.
	again, err := BuildWakeUpdate(s, ix, "vp")
	if err != nil || again.Text != "" || again.Watermark.Seq != ix.Seq {
		t.Errorf("rebuild = %+v err=%v", again, err)
	}
	// Commit with no index behind it is a no-op, not a zeroed watermark.
	if err := CommitWakeUpdate(s, "vp", WakeUpdate{}); err != nil {
		t.Fatal(err)
	}
	if wm2, _, _ := s.ReadGraphWatermark("vp"); wm2.Seq != ix.Seq {
		t.Errorf("empty commit clobbered the watermark: %+v", wm2)
	}
}

func TestWakeUpdateReportsHandbookRoleAndSkills(t *testing.T) {
	s := wakeUpdateStore(t)
	ix := gbuild(nil)
	if _, err := PrepareWakeUpdate(s, ix, "vp"); err != nil {
		t.Fatal(err)
	}
	if err := s.WriteHandbook("v2"); err != nil {
		t.Fatal(err)
	}
	if err := s.WriteRole("vp", "# new role"); err != nil {
		t.Fatal(err)
	}
	text, err := PrepareWakeUpdate(s, ix, "vp")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(text, "The handbook changed since your last turn.") || !strings.Contains(text, "Your role changed since your last turn.") {
		t.Errorf("outside-the-graph changes missing:\n%s", text)
	}
	// cs's role did not change, and cs sees nothing about vp's.
	if _, err := PrepareWakeUpdate(s, ix, "cs"); err != nil {
		t.Fatal(err)
	}
	text, err = PrepareWakeUpdate(s, ix, "cs")
	if err != nil || strings.Contains(text, "Your role changed") {
		t.Errorf("cs: text=%q err=%v", text, err)
	}
}

// A torn watermark file re-bootstraps the agent silently instead of
// disabling its trailer for good.
func TestWakeUpdateSurvivesACorruptWatermark(t *testing.T) {
	s := wakeUpdateStore(t)
	if err := os.WriteFile(filepath.Join(s.Root(), "agents", "vp", "graph_watermark.json"), []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	ix := gbuild(nil, gfile("vp", "a.md", "---\nid: a\n---\nx"))
	if text, err := PrepareWakeUpdate(s, ix, "vp"); err != nil || text != "" {
		t.Fatalf("torn watermark must re-bootstrap, not wedge: text=%q err=%v", text, err)
	}
	if wm, ok, _ := s.ReadGraphWatermark("vp"); !ok || wm.Seq != ix.Seq {
		t.Errorf("watermark not rewritten: %+v ok=%v", wm, ok)
	}
}

// When the index is rebuilt and its sequence falls below what the
// agent last saw, ChangedSince would be silent until the counter
// caught up. The trailer says the index was rebuilt and rebases.
func TestWakeUpdateHandlesAnIndexReset(t *testing.T) {
	s := wakeUpdateStore(t)
	a := gfile("vp", "a.md", "---\nid: a\n---\nx")
	if _, err := PrepareWakeUpdate(s, gbuild(nil, a), "vp"); err != nil {
		t.Fatal(err)
	}
	if err := s.WriteGraphWatermark("vp", store.GraphWatermark{Seq: 57, Fingerprints: map[string]string{}}); err != nil {
		t.Fatal(err)
	}
	fresh := gbuild(nil, a, gfile("cs", "d.md", "---\nid: d\nkind: decision\nabout: vp/a\n---\ny"))
	text, err := PrepareWakeUpdate(s, fresh, "vp")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(text, "rebuilt") || !strings.Contains(text, "graph_query") {
		t.Errorf("a reset should be named and point at the tools:\n%s", text)
	}
	if wm, _, _ := s.ReadGraphWatermark("vp"); wm.Seq != fresh.Seq {
		t.Errorf("watermark should rebase to the new sequence: %+v", wm)
	}
	// From here on, normal service.
	next := gbuild(fresh, a, gfile("cs", "d.md", "---\nid: d\nkind: decision\nabout: vp/a\n---\ny"), gfile("cs", "e.md", "---\nid: e\nkind: decision\nabout: vp/a\n---\nz"))
	text, err = PrepareWakeUpdate(s, next, "vp")
	if err != nil || !strings.Contains(text, "cs/e") || strings.Contains(text, "rebuilt") {
		t.Errorf("after the reset the trailer should be ordinary:\n%s\nerr=%v", text, err)
	}
}

func TestSkillsDiffNamesWhatMoved(t *testing.T) {
	got := skillsDiff("a@1,b@1,c@2", "a@1,b@2,d@1")
	if got != "added d@1; removed c; updated b 1→2" {
		t.Errorf("diff = %q", got)
	}
}

func wakeAssignment(t *testing.T, s *store.FSStore, id int, title, assignee string) {
	t.Helper()
	if err := s.WriteAssignment(&assignments.Assignment{
		ID: id, Title: title, Status: assignments.StatusOpen, Assignee: assignee, Creator: "chief-of-staff", Created: graphT0, Updated: graphT0,
		Log: []assignments.Entry{{Seq: 1, TS: graphT0, By: "chief-of-staff", Op: assignments.OpCreated}},
	}); err != nil {
		t.Fatal(err)
	}
}

// What the agent holds is state, not news: listed on the first wake
// (where the graph sections are silent), listed again on the next
// wake with nothing else to say, and absent for an agent that holds
// nothing.
func TestWakeUpdateListsOpenAssignmentsEveryWake(t *testing.T) {
	s := wakeUpdateStore(t)
	wakeAssignment(t, s, 1, "Draft the release notes", "vp")
	ix := gbuild(nil)
	text, err := PrepareWakeUpdate(s, ix, "vp")
	if err != nil {
		t.Fatal(err)
	}
	want := WakeUpdateLead + "\n\nYour open assignments:\n- #1 \"Draft the release notes\" (ready)"
	if text != want {
		t.Errorf("first wake:\n%q\nwant\n%q", text, want)
	}
	if wm, ok, _ := s.ReadGraphWatermark("vp"); !ok || wm.Seq != ix.Seq {
		t.Errorf("first wake did not set the watermark: %+v ok=%v", wm, ok)
	}
	// Nothing moved in the graph; the assignment is still listed.
	if text, err := PrepareWakeUpdate(s, ix, "vp"); err != nil || text != want {
		t.Errorf("second wake: %q err=%v", text, err)
	}
	// cs holds nothing: no section, and with nothing else to say, no
	// note at all.
	if text, err := PrepareWakeUpdate(s, ix, "cs"); err != nil || text != "" {
		t.Errorf("cs: %q err=%v", text, err)
	}
	// The graph's news and the assignments share one note, graph first.
	if _, err := PrepareWakeUpdate(s, ix, "cs"); err != nil {
		t.Fatal(err)
	}
	wakeAssignment(t, s, 2, "Answer vp", "cs")
	if err := s.WriteRole("cs", "# new role"); err != nil {
		t.Fatal(err)
	}
	text, err = PrepareWakeUpdate(s, ix, "cs")
	if err != nil {
		t.Fatal(err)
	}
	if text != WakeUpdateLead+"\n\nYour role changed since your last turn.\n\nYour open assignments:\n- #2 \"Answer vp\" (ready)" {
		t.Errorf("combined note:\n%s", text)
	}
}

// The nudge (an assignment of the agent's gained children and has no
// acceptance items) is told once: the note carries it while the flag
// stands, Build leaves the flag alone, and Commit clears it on the
// file with no log entry.
func TestWakeUpdateTellsTheNudgeOnce(t *testing.T) {
	s := wakeUpdateStore(t)
	if err := s.WriteAssignment(&assignments.Assignment{
		ID: 1, Title: "Epic", Status: assignments.StatusOpen, Assignee: "vp", Creator: "chief-of-staff", Nudge: true, Created: graphT0, Updated: graphT0,
		Log: []assignments.Entry{{Seq: 1, TS: graphT0, By: "chief-of-staff", Op: assignments.OpCreated}},
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.WriteAssignment(&assignments.Assignment{
		ID: 2, Title: "Part", Status: assignments.StatusOpen, Assignee: "cs", Creator: "vp", Parent: 1, Created: graphT0, Updated: graphT0,
		Log: []assignments.Entry{{Seq: 1, TS: graphT0, By: "vp", Op: assignments.OpCreated}},
	}); err != nil {
		t.Fatal(err)
	}
	ix := gbuild(nil)
	u, err := BuildWakeUpdate(s, ix, "vp")
	if err != nil {
		t.Fatal(err)
	}
	want := WakeUpdateLead + "\n\nYour open assignments:\n- #1 \"Epic\" (blocked by #2)\n\n#1 now has parts and no Done-when conditions."
	if u.Text != want || len(u.Nudged) != 1 || u.Nudged[0] != 1 {
		t.Fatalf("first note = %+v\nwant text\n%q", u, want)
	}
	if epic, _ := s.ReadAssignment(1); !epic.Nudge {
		t.Fatal("Build cleared the nudge; only Commit may")
	}
	if err := CommitWakeUpdate(s, "vp", u); err != nil {
		t.Fatal(err)
	}
	epic, _ := s.ReadAssignment(1)
	if epic.Nudge {
		t.Fatal("Commit did not clear the nudge")
	}
	if epic.LastSeq() != 1 {
		t.Fatalf("clearing the nudge added a log entry: %+v", epic.Log)
	}
	text, err := PrepareWakeUpdate(s, ix, "vp")
	if err != nil || strings.Contains(text, "no acceptance items") || !strings.Contains(text, `#1 "Epic"`) {
		t.Errorf("second wake: %q err=%v", text, err)
	}
	// cs holds the child, not the parent: no nudge for cs.
	if text, err := PrepareWakeUpdate(s, ix, "cs"); err != nil || strings.Contains(text, "no acceptance items") {
		t.Errorf("cs: %q err=%v", text, err)
	}
}

// The graph index pass can fail (the note is then built without one);
// what the agent holds is still worth saying, and the watermark stays
// put so the graph's news is not skipped.
func TestWakeUpdateWithoutAnIndexStillListsOpenAssignments(t *testing.T) {
	s := wakeUpdateStore(t)
	wakeAssignment(t, s, 1, "Draft the release notes", "vp")
	text, err := PrepareWakeUpdate(s, nil, "vp")
	if err != nil || !strings.Contains(text, `- #1 "Draft the release notes" (ready)`) {
		t.Errorf("no index: text=%q err=%v", text, err)
	}
	if _, ok, _ := s.ReadGraphWatermark("vp"); ok {
		t.Error("no index: the watermark must stay unset")
	}
}

// A tracker paused on a file the CEO must fix by hand fails every
// read the same way. The note says so and still carries the rest; the
// agent's assignment tools will say the same when it reaches for them.
func TestWakeUpdateSaysWhenTheTrackerCannotBeRead(t *testing.T) {
	s := wakeUpdateStore(t)
	wakeAssignment(t, s, 1, "Draft the release notes", "vp")
	if err := os.WriteFile(filepath.Join(s.Root(), "assignments", "000002.md"), []byte("not an assignment"), 0o644); err != nil {
		t.Fatal(err)
	}
	ix := gbuild(nil)
	if _, err := PrepareWakeUpdate(s, ix, "vp"); err != nil {
		t.Fatal(err)
	}
	if err := s.WriteHandbook("v2"); err != nil {
		t.Fatal(err)
	}
	text, err := PrepareWakeUpdate(s, ix, "vp")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(text, "The handbook changed since your last turn.") || !strings.Contains(text, "Your open assignments could not be listed this wake: ") || !strings.Contains(text, "000002.md") {
		t.Errorf("note with a paused tracker:\n%s", text)
	}
}
