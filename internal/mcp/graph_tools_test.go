package mcp

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kivali-ai/kivali/internal/agent"
	"github.com/kivali-ai/kivali/internal/agentpod"
	"github.com/kivali-ai/kivali/internal/files"
	"github.com/kivali-ai/kivali/internal/provider"
	"github.com/kivali-ai/kivali/internal/store"
)

// graphFixture seeds three agents and a small graph: a service design,
// a requirement and a decision binding it, a withdrawn premise, a
// bare file, and a project file. The files are written straight into
// the published trees; the first tool call's Index loads and scans
// them, as a boot would.
func graphFixture(t *testing.T) *store.FSStore {
	t.Helper()
	s, err := store.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range []store.Agent{
		{Slug: "chief-of-staff", Role: "Chief of Staff", ReportsTo: "ceo"},
		{Slug: "vp", Role: "Head of Platform", ReportsTo: "ceo"},
		{Slug: "cs", Role: "Data Lead", ReportsTo: "vp"},
	} {
		if err := s.CreateAgent(a, "# role"); err != nil {
			t.Fatal(err)
		}
	}
	write := func(slug, rel, body string) {
		p := filepath.Join(files.PublishedDir(s.Root(), slug), filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("vp", "ingest.md", "---\nid: ingest\nsummary: Ingest service design\n---\nLong design body that must never appear in a tool result.\n")
	write("vp", "reqs/retries.md", "---\nid: retries\nkind: requirement\nabout: vp/ingest\nstatus: provisional\ncondition: until the load test lands\ncheck: a full day's ingest log shows zero dropped batches\n---\nNo batch is ever dropped.\n")
	write("cs", "cap.md", "---\nid: no-retry-cap\nkind: decision\nabout: vp/ingest\ndepends_on: cs/old-analysis\n---\nRetries stay unbounded.\nRejected a fixed cap: it hides sizing faults.\n")
	write("cs", "old.md", "---\nid: old-analysis\nstatus: withdrawn\n---\nSuperseded numbers.\n")
	write("cs", "scratch.txt", "just bytes")
	pf, err := s.AddProjectFile("Business Plan.md", strings.NewReader("the plan"))
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SetCanonical(pf.SHA, "original.md", "text/markdown"); err != nil {
		t.Fatal(err)
	}
	return s
}

func graphCall(t *testing.T, s *store.FSStore, caller, tool, input string) string {
	t.Helper()
	body, isErr := DispatchStateToolInProcess(StateDispatchDeps{Store: s, Slug: caller}, tool, json.RawMessage(input))
	if isErr {
		t.Fatalf("%s %s: tool error: %s", tool, input, body)
	}
	return body
}

func TestGraphQueryAnswersTheAcceptanceQueries(t *testing.T) {
	s := graphFixture(t)

	// Q1: what binds ingest right now.
	out := graphCall(t, s, "vp", GraphQueryToolName, `{"about":"vp/ingest","binding":true,"status":"in_force"}`)
	if !strings.Contains(out, "2 nodes match") || !strings.Contains(out, "- vp/retries  requirement provisional v1") || !strings.Contains(out, "- cs/no-retry-cap  decision current, flagged v1") {
		t.Errorf("Q1 =\n%s", out)
	}
	// The summary is the body's first line by design; the rest of the
	// body must never appear.
	if strings.Contains(out, "Long design body") || strings.Contains(out, "Rejected a fixed cap") {
		t.Errorf("tool results must never carry bodies:\n%s", out)
	}
	// Q7: everything about ingest.
	out = graphCall(t, s, "vp", GraphQueryToolName, `{"about":"vp/ingest"}`)
	if !strings.Contains(out, "2 nodes match") {
		t.Errorf("Q7 =\n%s", out)
	}
	// Q6: what has cs published, bare files included.
	out = graphCall(t, s, "vp", GraphQueryToolName, `{"owner":"cs"}`)
	for _, want := range []string{"cs/no-retry-cap", "cs/old-analysis", "cs/scratch.txt  artifact current v1 · owner cs — scratch.txt"} {
		if !strings.Contains(out, want) {
			t.Errorf("Q6 missing %q in\n%s", want, out)
		}
	}
	// Q2: of record for ingest — the current binding node, with its version.
	out = graphCall(t, s, "vp", GraphQueryToolName, `{"about":"vp/ingest","status":"current"}`)
	if !strings.Contains(out, "1 node match") || !strings.Contains(out, "cs/no-retry-cap") {
		t.Errorf("Q2 =\n%s", out)
	}
	// The CEO's project file is a node.
	out = graphCall(t, s, "vp", GraphQueryToolName, `{"owner":"ceo"}`)
	if !strings.Contains(out, "ceo/business_plan  reference current v1") {
		t.Errorf("project file row missing:\n%s", out)
	}
	// Agents are nodes, and can be filtered by their own statuses.
	out = graphCall(t, s, "vp", GraphQueryToolName, `{"type":"agent","status":"active"}`)
	if !strings.Contains(out, "- cs  agent active, reports to vp — Data Lead") || !strings.Contains(out, "- ceo  agent active") {
		t.Errorf("agents =\n%s", out)
	}
}

// Agents copy ids out of graph_node, which prints them pinned
// ("vp/ingest@1"). A pinned id in the about filter must still find
// the artifacts about that node.
func TestGraphQueryAboutAcceptsAPinnedId(t *testing.T) {
	s := graphFixture(t)
	out := graphCall(t, s, "vp", GraphQueryToolName, `{"about":"vp/ingest@1"}`)
	if !strings.Contains(out, "2 nodes match") {
		t.Errorf("pinned about should match the node's subject rows:\n%s", out)
	}
}

func TestGraphQueryRefusesBadFilters(t *testing.T) {
	s := graphFixture(t)
	for _, in := range []string{`{"kind":"datasheet"}`, `{"status":"frozen"}`, `{"type":"claim"}`} {
		if _, isErr := DispatchStateToolInProcess(StateDispatchDeps{Store: s, Slug: "vp"}, GraphQueryToolName, json.RawMessage(in)); !isErr {
			t.Errorf("%s should be refused", in)
		}
	}
	out := graphCall(t, s, "vp", GraphQueryToolName, `{"owner":"nobody"}`)
	if !strings.Contains(out, "0 nodes match (owner=nobody)") {
		t.Errorf("empty result =\n%s", out)
	}
}

// Every status the handler accepts is in the schema enum the model is
// shown, and vice versa; otherwise the CLI refuses a value the code
// would have taken, or the code refuses one the schema advertised.
func TestGraphQueryStatusEnumMatchesTheHandler(t *testing.T) {
	var schema struct {
		Properties struct {
			Status struct {
				Enum []string `json:"enum"`
			} `json:"status"`
		} `json:"properties"`
	}
	if err := json.Unmarshal([]byte(agent.GraphQueryInputSchema), &schema); err != nil {
		t.Fatal(err)
	}
	s := graphFixture(t)
	for _, st := range schema.Properties.Status.Enum {
		if _, isErr := DispatchStateToolInProcess(StateDispatchDeps{Store: s, Slug: "vp"}, GraphQueryToolName, json.RawMessage(`{"status":"`+st+`"}`)); isErr {
			t.Errorf("schema advertises status %q but the handler refuses it", st)
		}
	}
	for _, st := range []string{"active", "archived", "in_force", "superseded"} {
		found := false
		for _, e := range schema.Properties.Status.Enum {
			found = found || e == st
		}
		if !found {
			t.Errorf("handler accepts status %q but the schema does not offer it", st)
		}
	}
}

func TestGraphQueryLimit(t *testing.T) {
	s := graphFixture(t)
	out := graphCall(t, s, "vp", GraphQueryToolName, `{"limit":2}`)
	if !strings.Contains(out, "more; narrow the filters") {
		t.Errorf("limit should truncate with a hint:\n%s", out)
	}
}

func TestGraphNodeShowsEdgesVersionsAndTheReadPath(t *testing.T) {
	s := graphFixture(t)
	out := graphCall(t, s, "vp", GraphNodeToolName, `{"id":"cs/no-retry-cap"}`)
	for _, want := range []string{
		"Node cs/no-retry-cap (decision)",
		"owner: cs",
		"read: file_view /files/artifacts/shared/cs/cap.md",
		"status: current",
		"flagged: rests on cs/old-analysis, which is withdrawn",
		"about: vp/ingest",
		"depends on: cs/old-analysis",
		"versions: v1 ",
		"(pin as cs/no-retry-cap@1; graph_node cs/no-retry-cap@N returns the file at version N)",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("graph_node missing %q in\n%s", want, out)
		}
	}
	if strings.Contains(out, "Rejected a fixed cap") {
		t.Errorf("body leaked:\n%s", out)
	}
	// Reverse edges and the caller's own path.
	out = graphCall(t, s, "vp", GraphNodeToolName, `{"id":"vp/ingest"}`)
	for _, want := range []string{
		"read: file_view /files/artifacts/public/ingest.md",
		"about this (2): cs/no-retry-cap, vp/retries",
		"summary: Ingest service design",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("graph_node missing %q in\n%s", want, out)
		}
	}
	// Pinned lookups resolve to the node.
	out = graphCall(t, s, "vp", GraphNodeToolName, `{"id":"vp/retries@1"}`)
	if !strings.Contains(out, "Node vp/retries (requirement)") || !strings.Contains(out, "condition: until the load test lands") || !strings.Contains(out, "check: a full day's ingest log") {
		t.Errorf("pinned lookup =\n%s", out)
	}
	// Project files read from /files/project/. The id is lowercased and
	// loses its .md; the path keeps the link name's case.
	out = graphCall(t, s, "vp", GraphNodeToolName, `{"id":"ceo/business_plan"}`)
	if !strings.Contains(out, "read: file_view /files/project/Business_Plan.md") || !strings.Contains(out, "source: owner upload") {
		t.Errorf("project node =\n%s", out)
	}
	// Agents.
	out = graphCall(t, s, "vp", GraphNodeToolName, `{"id":"cs"}`)
	if !strings.Contains(out, "Node cs (agent)") || !strings.Contains(out, "owns: 3 artifacts (graph_query owner=cs)") || !strings.Contains(out, "reports to: vp") {
		t.Errorf("agent node =\n%s", out)
	}
	// Unknown id is a tool error with a hint.
	if body, isErr := DispatchStateToolInProcess(StateDispatchDeps{Store: s, Slug: "vp"}, GraphNodeToolName, json.RawMessage(`{"id":"vp/ghost"}`)); !isErr || !strings.Contains(body, "no node") {
		t.Errorf("unknown id: isErr=%v body=%s", isErr, body)
	}
}

// An agent that types the id as it sees the file — with its case and
// its .md — should still land on the node, since ids are derived by
// lowercasing and dropping the extension.
func TestGraphNodeForgivesCaseAndExtension(t *testing.T) {
	s := graphFixture(t)
	for _, id := range []string{"ceo/Business_Plan.md", "ceo/business_plan.md", "CEO/Business_Plan", "vp/Ingest.md"} {
		out := graphCall(t, s, "vp", GraphNodeToolName, `{"id":"`+id+`"}`)
		if !strings.Contains(out, "Node ceo/business_plan (reference)") && !strings.Contains(out, "Node vp/ingest (artifact)") {
			t.Errorf("%s should resolve:\n%s", id, out)
		}
	}
}

// Every path graph_node prints has to be readable by the caller's
// file_view. The archived-owner case is the one that breaks silently:
// an offboarded agent's nodes stay in the graph, and its withdrawn
// premises are exactly what peers must still be able to read.
func TestGraphNodePathsResolveForTheCaller(t *testing.T) {
	s := graphFixture(t)
	if err := s.ArchiveAgent("cs"); err != nil {
		t.Fatal(err)
	}
	if err := s.SyncAgentFilesystem("vp"); err != nil {
		t.Fatal(err)
	}
	backend := files.Dispatcher{DataDir: s.Root()}.BackendFor("vp")
	for _, id := range []string{"cs/old-analysis", "cs/no-retry-cap", "vp/ingest", "ceo/business_plan"} {
		out := graphCall(t, s, "vp", GraphNodeToolName, `{"id":"`+id+`"}`)
		path := ""
		for _, line := range strings.Split(out, "\n") {
			if rest, ok := strings.CutPrefix(strings.TrimSpace(line), "read: file_view "); ok {
				path = rest
			}
		}
		if path == "" {
			t.Fatalf("%s: no read path printed:\n%s", id, out)
		}
		if _, err := backend.View(path, files.ViewOptions{}); err != nil {
			t.Errorf("%s: printed path %s does not resolve for the caller: %v", id, path, err)
		}
	}
}

// publishRaw is a publish landing, as artifact_publish lands one: the
// file written into the published tree, then the graph maintainer told.
func publishRaw(t *testing.T, s *store.FSStore, owner, rel, body string) {
	t.Helper()
	p := filepath.Join(files.PublishedDir(s.Root(), owner), filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, warnings, err := s.Graph().Published(owner, []string{rel}); err != nil || len(warnings) > 0 {
		t.Fatalf("Published %s/%s: %v %v", owner, rel, err, warnings)
	}
}

func TestGraphToolsSeePublishesFromEarlierInTheTurn(t *testing.T) {
	// A publish updates the index before it returns, so a node the
	// agent just published is visible to its next tool call, and the
	// tools themselves never walk the published trees.
	s := graphFixture(t)
	graphCall(t, s, "vp", GraphQueryToolName, `{}`)
	walks := s.Graph().Walks()
	publishRaw(t, s, "vp", "new.md", "---\nid: brand-new\n---\nx\n")
	out := graphCall(t, s, "vp", GraphNodeToolName, `{"id":"vp/brand-new"}`)
	if !strings.Contains(out, "Node vp/brand-new (artifact)") {
		t.Errorf("fresh publish not visible:\n%s", out)
	}
	graphCall(t, s, "vp", GraphQueryToolName, `{"owner":"vp"}`)
	if got := s.Graph().Walks(); got != walks {
		t.Errorf("graph tool calls walked the published trees %d times", got-walks)
	}
}

func TestGraphToolDefinitionsMatchAcrossTransports(t *testing.T) {
	native := map[string]string{}
	for _, tool := range agent.GraphTools() {
		native[tool.Name] = tool.Description
	}
	s, _ := store.New(t.TempDir())
	d := fakeStateDispatcher{deps: StateDispatchDeps{Store: s, Slug: "alice"}}
	for _, tool := range GraphTools(d) {
		if native[tool.Name] != tool.Description {
			t.Errorf("%s: descriptions differ across transports", tool.Name)
		}
		if !tool.ReadOnly {
			t.Errorf("%s must be read-only", tool.Name)
		}
	}
	if len(native) != 2 {
		t.Errorf("expected two graph tools, got %v", native)
	}
	// And the full-agent state tool set carries them.
	names := map[string]bool{}
	for _, tool := range StateTools(d, false) {
		names[tool.Name] = true
	}
	if !names[GraphQueryToolName] || !names[GraphNodeToolName] {
		t.Errorf("state tools missing the graph pair: %v", names)
	}
}

// A subagent run is given exactly SubagentTools' kivali names; a tool
// the subagent toolkit registers but that list omits is one the model
// is shown and then refused for calling. Both lists are hand-kept.
func TestSubagentToolsCoverTheSubagentToolkit(t *testing.T) {
	be := &files.Backend{Root: t.TempDir()}
	for _, depth := range []int{0, 1, agentpod.MaxSubagentDepth} {
		deps := SubagentDeps{Client: newTestClient("alice"), SubagentID: "st-1", Files: NewLocalFilesDispatcher(be), ShellExec: &fakeShellExec{}, Provider: provider.MockProvider{}}
		if agentpod.CanDelegate(depth) {
			deps.NestedSubagent = stubNested{}
		}
		allowed := map[string]bool{}
		for _, n := range agentpod.SubagentTools(depth).Kivali {
			allowed[n] = true
		}
		for _, tool := range SubagentToolkit(deps).Tools {
			if !allowed[tool.Name] {
				t.Errorf("depth %d: toolkit registers %q but SubagentTools omits it", depth, tool.Name)
			}
		}
	}
}

// A pinned id returns the file as it was at that version — the text a
// certificate or a pinned depends_on actually names — which file_view
// cannot, since it reads the current file. Assignment #320: a reviewer
// could not read a peer's rule at the version an RC was cut under
// once the rule had moved on.
func TestGraphNodePinnedReturnsTheFileAtThatVersion(t *testing.T) {
	s := graphFixture(t)
	graphCall(t, s, "cs", GraphQueryToolName, `{}`) // index v1 of everything
	// The body's first line is the derived summary, which the header
	// prints for any version; the second line is body-only and is the
	// sentinel for "the current text leaked".
	publishRaw(t, s, "vp", "reqs/retries.md", "---\nid: retries\nkind: requirement\nabout: vp/ingest\nstatus: current\ncheck: a full day's ingest log shows zero dropped batches\n---\nNo batch is ever dropped.\nRetries are capped at five.\n")

	// The old version: its own front matter and body, under a header
	// that is the CURRENT index state, with a line saying how stale it is.
	out := graphCall(t, s, "cs", GraphNodeToolName, `{"id":"vp/retries@1"}`)
	for _, want := range []string{
		"Node vp/retries (requirement)",
		"status: current\n",
		"versions: v1 ",
		"(pin as vp/retries@2; graph_node vp/retries@N returns the file at version N)",
		"vp/retries@1 — the file as versioned ",
		"1 newer version; v2 is current and is what file_view reads.",
		"----- begin vp/retries@1 -----\n---\nid: retries\n",
		"status: provisional\ncondition: until the load test lands\n",
		"---\nNo batch is ever dropped.\n----- end vp/retries@1 -----\n",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("graph_node @1 missing %q in\n%s", want, out)
		}
	}
	if strings.Contains(out, "capped at five") {
		t.Errorf("@1 leaked the current body:\n%s", out)
	}

	// The current version pinned: the same text file_view would read.
	out = graphCall(t, s, "cs", GraphNodeToolName, `{"id":"vp/retries@2"}`)
	if !strings.Contains(out, "the current version; file_view reads the same text.") || !strings.Contains(out, "----- begin vp/retries@2 -----") || !strings.Contains(out, "capped at five.\n----- end vp/retries@2 -----") {
		t.Errorf("graph_node @2 =\n%s", out)
	}
	if strings.Contains(out, "condition: until the load test lands") {
		t.Errorf("@2 carried the old front matter:\n%s", out)
	}

	// A SHA prefix is a pin too, as a certificate may write it.
	ix, err := s.ReadGraphIndex()
	if err != nil {
		t.Fatal(err)
	}
	n, _ := ix.Get("vp/retries")
	out = graphCall(t, s, "cs", GraphNodeToolName, `{"id":"vp/retries@`+n.Versions[0].SHA[:8]+`"}`)
	if !strings.Contains(out, "----- begin vp/retries@1 -----") {
		t.Errorf("sha-prefix pin =\n%s", out)
	}

	// Unpinned stays body-free: file_view is the read path for the
	// current file, and the versions line says how to read an old one.
	out = graphCall(t, s, "cs", GraphNodeToolName, `{"id":"vp/retries"}`)
	if strings.Contains(out, "----- begin") || strings.Contains(out, "capped at five") {
		t.Errorf("unpinned lookup carried a body:\n%s", out)
	}

	// A pin to nothing is an error that lists what there is; an agent
	// has nothing to pin.
	if body, isErr := DispatchStateToolInProcess(StateDispatchDeps{Store: s, Slug: "cs"}, GraphNodeToolName, json.RawMessage(`{"id":"vp/retries@9"}`)); !isErr || !strings.Contains(body, `vp/retries has no version "9"; its versions are v1 `) || !strings.Contains(body, "(pin as vp/retries@N)") {
		t.Errorf("bad pin: isErr=%v body=%s", isErr, body)
	}
	if body, isErr := DispatchStateToolInProcess(StateDispatchDeps{Store: s, Slug: "vp"}, GraphNodeToolName, json.RawMessage(`{"id":"cs@1"}`)); !isErr || !strings.Contains(body, "agents have no versions") {
		t.Errorf("agent pin: isErr=%v body=%s", isErr, body)
	}
}

// A project file's versions are the CEO's uploads; a bare binary's
// version is reported, not rendered.
func TestGraphNodePinnedProjectFileAndBinary(t *testing.T) {
	s := graphFixture(t)
	out := graphCall(t, s, "vp", GraphNodeToolName, `{"id":"ceo/business_plan@1"}`)
	if !strings.Contains(out, "----- begin ceo/business_plan@1 -----\nthe plan\n----- end ceo/business_plan@1 -----") {
		t.Errorf("project file @1 =\n%s", out)
	}
	publishRaw(t, s, "cs", "logo.png", "\x89PNG\r\n\x1a\n\x00\x00\x00\rIHDR")
	out = graphCall(t, s, "vp", GraphNodeToolName, `{"id":"cs/logo.png@1"}`)
	if !strings.Contains(out, "cs/logo.png@1: 16 bytes, not text; not shown.") || strings.Contains(out, "----- begin") {
		t.Errorf("binary @1 =\n%s", out)
	}
}
