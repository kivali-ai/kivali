package web

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/kivali-ai/kivali/internal/agent"
	"github.com/kivali-ai/kivali/internal/store"
	"github.com/kivali-ai/kivali/internal/web/apitypes"
)

// proposalDo serves one GET through the proposal route behind the
// API's own middleware, as wireAPIRoutes mounts it.
func proposalDo(t *testing.T, srv *Server, path string) *httptest.ResponseRecorder {
	t.Helper()
	mux := http.NewServeMux()
	srv.wireAPIProposalRoutes(mux)
	rr := httptest.NewRecorder()
	apiHeaders(requireSameOrigin(mux)).ServeHTTP(rr, httptest.NewRequest(http.MethodGet, path, nil))
	return rr
}

func getProposal(t *testing.T, srv *Server, reqPath string) apitypes.Proposal {
	t.Helper()
	rr := proposalDo(t, srv, "/api/v1/proposals/"+reqPath)
	if rr.Code != http.StatusOK {
		t.Fatalf("GET proposal %s: code %d body %s", reqPath, rr.Code, rr.Body.String())
	}
	p := decodeAPI[apitypes.Proposal](t, rr)
	apitypes.NoNilSlices(t, p)
	return p
}

// seedProposal writes a chief-of-staff approval request carrying the
// proposal set on m and puts it in the CEO's inbox, as the propose_*
// tools do. Returns its path.
func seedProposal(t *testing.T, srv *Server, m store.Message) string {
	t.Helper()
	m.Type = store.MsgCEOApprovalRequest
	m.From = "chief-of-staff"
	m.To = store.Recipients{agent.CEOSlug}
	m.Date = time.Now().UTC()
	abs, err := srv.Store.WriteMessage(m)
	if err != nil {
		t.Fatal(err)
	}
	rel, _ := filepath.Rel(srv.Store.Root(), abs)
	_ = srv.Store.CreateAgent(store.Agent{Slug: agent.CEOSlug, Role: "CEO"}, "")
	if err := srv.Store.AppendChatMessage(agent.CEOSlug, store.ChatMessage{
		Role: store.RoleReceived, Content: m.Title, Kind: "ceo_inbox", MessageRef: rel,
	}); err != nil {
		t.Fatal(err)
	}
	return rel
}

// proposalTeam is chief-of-staff under you and a buyer under it, both
// pinned to a model so the tile's model line is known.
func proposalTeam(t *testing.T, srv *Server) {
	t.Helper()
	for _, a := range []store.Agent{
		{Slug: "chief-of-staff", Role: "Chief of Staff", ReportsTo: agent.CEOSlug, Model: "mock-large"},
		{Slug: "buyer", Role: "Buyer", ReportsTo: "chief-of-staff", Model: "mock-large", Effort: "medium"},
	} {
		if err := srv.Store.CreateAgent(a, "# "+a.Role+"\n\n## Scope\nHandles purchases.\n"); err != nil {
			t.Fatal(err)
		}
	}
}

// answerProposal posts approve or deny for reqPath through Needs you's route,
// the one the proposal page uses, and returns what it said.
func answerProposal(t *testing.T, srv *Server, action, reqPath string) apitypes.NeedActionResponse {
	t.Helper()
	body, ct := needsForm(t, reqPath, "", nil)
	rr := homeDo(t, srv, http.MethodPost, "/api/v1/needs/"+action, ct, body, nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("%s: code %d body %s", action, rr.Code, rr.Body.String())
	}
	return decodeAPI[apitypes.NeedActionResponse](t, rr)
}

func TestAPIProposalHire(t *testing.T) {
	srv, _ := newTurnServer(t)
	proposalTeam(t, srv)
	path := seedProposal(t, srv, store.Message{
		Title: "Approve hire: Garden advisor",
		Body:  "We need someone on the garden.\n",
		Hire: &store.Hire{
			Slug: "garden-advisor", Role: "Garden advisor", ReportsTo: "chief-of-staff",
			// CRLF and a lost trailing newline are not edits.
			Body:               "# Garden advisor\r\n\r\n## Scope\r\nBeds and borders.",
			InitialAgentMemory: "## Context\nThe beds face south.\n",
		},
	})

	p := getProposal(t, srv, path)
	if p.Path != path || p.Kind != apitypes.ProposalKindHire || p.Title != "Approve hire: Garden advisor" {
		t.Errorf("head = %q %q %q", p.Path, p.Kind, p.Title)
	}
	if p.Proposer != (apitypes.PersonRef{Slug: "chief-of-staff", Name: "Chief of Staff"}) || p.ReasonMD != "We need someone on the garden." || p.ProposedAt.IsZero() {
		t.Errorf("proposer/reason = %+v %q %v", p.Proposer, p.ReasonMD, p.ProposedAt)
	}
	wantAgent := apitypes.ProposalAgent{
		Slug: "garden-advisor", Name: "Garden advisor", RoleTitle: "Garden advisor",
		ReportsTo: apitypes.PersonRef{Slug: "chief-of-staff", Name: "Chief of Staff"},
		Model:     "mock-large-0", ModelLabel: "Mock Large 0", Effort: "high",
	}
	if p.Summary.Agent == nil || *p.Summary.Agent != wantAgent {
		t.Errorf("agent = %+v", p.Summary.Agent)
	}
	wantFacts := []apitypes.ProposalFact{{Label: "Reports to", Value: "Chief of Staff"}, {Label: "Model", Value: "Mock Large 0 · high"}}
	if !reflect.DeepEqual(p.Summary.Facts, wantFacts) || len(p.Summary.Moves) != 0 {
		t.Errorf("facts/moves = %+v %+v", p.Summary.Facts, p.Summary.Moves)
	}
	wantDocs := []apitypes.ProposalDoc{
		{Key: apitypes.ProposalDocKeyRole, Title: "Role", Meta: "role.md · 4 lines", After: "# Garden advisor\n\n## Scope\nBeds and borders.\n"},
		{Key: apitypes.ProposalDocKeyMemory, Title: "Initial memory", Meta: "memory.md · 2 lines", After: "## Context\nThe beds face south.\n"},
	}
	if !reflect.DeepEqual(p.Docs, wantDocs) {
		t.Errorf("docs = %+v", p.Docs)
	}
	if p.Resolved != nil || p.OffboardSentence != nil {
		t.Errorf("pending hire has resolved %+v / sentence %v", p.Resolved, p.OffboardSentence)
	}
	// A new document has no before on the wire at all.
	if rr := proposalDo(t, srv, "/api/v1/proposals/"+path); strings.Contains(rr.Body.String(), `"before"`) {
		t.Errorf("hire docs carry a before: %s", rr.Body.String())
	}
}

func TestAPIProposalRoleUpdate(t *testing.T) {
	srv, _ := newTurnServer(t)
	proposalTeam(t, srv)
	proposed := "# Buyer\n\n## Scope\nHandles purchases.\nOwns vendor quotes.\n"
	path := seedProposal(t, srv, store.Message{
		Title: "Update role: Buyer", Body: "Quotes belong with the buyer.",
		RoleUpdate: &store.RoleUpdate{Slug: "buyer", Body: proposed},
	})

	p := getProposal(t, srv, path)
	if p.Kind != apitypes.ProposalKindRoleUpdate {
		t.Errorf("kind = %q", p.Kind)
	}
	a := p.Summary.Agent
	if a == nil || a.Slug != "buyer" || a.Name != "Buyer" || a.ReportsTo.Name != "Chief of Staff" || a.ModelLabel != "Mock Large" || a.Effort != "medium" {
		t.Errorf("agent = %+v", a)
	}
	if len(p.Docs) != 1 {
		t.Fatalf("docs = %+v", p.Docs)
	}
	d := p.Docs[0]
	if d.Key != apitypes.ProposalDocKeyRole || d.Title != "Role" || d.Meta != "role.md · 5 lines" || d.After != proposed {
		t.Errorf("doc = %+v", d)
	}
	if d.Before == nil || *d.Before != "# Buyer\n\n## Scope\nHandles purchases.\n" {
		t.Errorf("before = %v", d.Before)
	}
}

func TestAPIProposalHandbookUpdate(t *testing.T) {
	srv, _ := newTurnServer(t)
	proposalTeam(t, srv)
	if err := srv.Store.WriteHandbook("# Handbook\n\n## Values\nBe kind.\n"); err != nil {
		t.Fatal(err)
	}
	proposed := "# Handbook\n\n## Values\nBe kind.\nBe brief.\n"
	path := seedProposal(t, srv, store.Message{
		Title: "Update the handbook", Body: "Add brevity.",
		HandbookUpdate: &store.HandbookUpdate{Body: proposed},
	})

	p := getProposal(t, srv, path)
	if p.Kind != apitypes.ProposalKindHandbookUpdate || p.Summary.Agent != nil {
		t.Errorf("kind/agent = %q %+v", p.Kind, p.Summary.Agent)
	}
	if want := []apitypes.ProposalFact{{Label: "Applies to", Value: "All 2 agents on the team"}}; !reflect.DeepEqual(p.Summary.Facts, want) {
		t.Errorf("facts = %+v", p.Summary.Facts)
	}
	if len(p.Docs) != 1 {
		t.Fatalf("docs = %+v", p.Docs)
	}
	d := p.Docs[0]
	if d.Key != apitypes.ProposalDocKeyHandbook || d.Title != "Handbook" || d.Meta != "handbook.md · 5 lines" || d.After != proposed {
		t.Errorf("doc = %+v", d)
	}
	if d.Before == nil || *d.Before != "# Handbook\n\n## Values\nBe kind.\n" {
		t.Errorf("before = %v", d.Before)
	}
}

func TestAPIProposalOffboard(t *testing.T) {
	srv, _ := newTurnServer(t)
	proposalTeam(t, srv)
	path := seedOffboardProposal(t, srv, "buyer")

	p := getProposal(t, srv, path)
	if p.Kind != apitypes.ProposalKindOffboard || p.ReasonMD != "Scope has been folded elsewhere." {
		t.Errorf("kind/reason = %q %q", p.Kind, p.ReasonMD)
	}
	if p.OffboardSentence == nil || *p.OffboardSentence != "Its files move to the archive. Nothing is deleted, and you can restore it later." {
		t.Errorf("sentence = %v", p.OffboardSentence)
	}
	if a := p.Summary.Agent; a == nil || a.Slug != "buyer" || a.Name != "Buyer" {
		t.Errorf("agent = %+v", a)
	}
	// The reason matches the body, so it is not said twice.
	want := []apitypes.ProposalFact{
		{Label: "Reports to", Value: "Chief of Staff"},
		{Label: "Model", Value: "Mock Large · medium"},
		{Label: "Its assignments move to", Value: "Chief of Staff"},
	}
	if !reflect.DeepEqual(p.Summary.Facts, want) || len(p.Docs) != 0 {
		t.Errorf("facts/docs = %+v %+v", p.Summary.Facts, p.Docs)
	}
}

func TestAPIProposalReorg(t *testing.T) {
	srv, _ := newTurnServer(t)
	proposalTeam(t, srv)
	if err := srv.Store.CreateAgent(store.Agent{Slug: "tester", Role: "Tester", ReportsTo: "buyer"}, "# Tester\n"); err != nil {
		t.Fatal(err)
	}
	path := seedProposal(t, srv, store.Message{
		Title: "Reorg: flatten", Body: "Fewer layers.",
		Reorg: &store.Reorg{Moves: []store.ReorgMove{
			{Slug: "buyer", NewManager: agent.CEOSlug},
			{Slug: "tester", NewManager: "chief-of-staff"},
			// Already there: nothing to move from.
			{Slug: "chief-of-staff", NewManager: agent.CEOSlug},
		}},
	})

	p := getProposal(t, srv, path)
	cos := apitypes.PersonRef{Slug: "chief-of-staff", Name: "Chief of Staff"}
	buyer := apitypes.PersonRef{Slug: "buyer", Name: "Buyer"}
	you := apitypes.PersonRef{Slug: agent.CEOSlug, Name: "You"}
	want := []apitypes.ProposalMove{
		{Slug: "buyer", Name: "Buyer", From: &cos, To: you},
		{Slug: "tester", Name: "Tester", From: &buyer, To: cos},
		{Slug: "chief-of-staff", Name: "Chief of Staff", To: you},
	}
	if p.Kind != apitypes.ProposalKindReorg || !reflect.DeepEqual(p.Summary.Moves, want) {
		t.Errorf("moves = %+v", p.Summary.Moves)
	}
	if f := p.Summary.Facts; len(f) != 1 || f[0] != (apitypes.ProposalFact{Label: "Changes", Value: "3 reporting lines"}) {
		t.Errorf("facts = %+v", f)
	}
	if p.Summary.Agent != nil || len(p.Docs) != 0 {
		t.Errorf("agent/docs = %+v %+v", p.Summary.Agent, p.Docs)
	}
}

// Approving from the page: the answer's sentence and link are what the
// page says afterwards, and the replaced role has no before to diff.
func TestAPIProposalResolvedAfterApprove(t *testing.T) {
	srv, _ := newTurnServer(t)
	proposalTeam(t, srv)
	proposed := "# Buyer\n\n## Scope\nHandles purchases.\nOwns vendor quotes.\n"
	path := seedProposal(t, srv, store.Message{
		Title: "Update role: Buyer", Body: "Quotes belong with the buyer.",
		RoleUpdate: &store.RoleUpdate{Slug: "buyer", Body: proposed},
	})
	got := answerProposal(t, srv, "approve", path)
	if got.Result != "Buyer has its new role" || got.Link != "/agents/buyer" {
		t.Errorf("approve said %+v", got)
	}

	p := getProposal(t, srv, path)
	r := p.Resolved
	if r == nil || !r.Approved || r.At.IsZero() || r.Result != got.Result || r.Link != got.Link {
		t.Fatalf("resolved = %+v, answer %+v", r, got)
	}
	if len(p.Docs) != 1 || p.Docs[0].Before != nil || p.Docs[0].After != proposed {
		t.Errorf("docs after approve = %+v", p.Docs)
	}
}

// An approved offboard still names the agent it archived, as its
// answer did.
func TestAPIProposalResolvedAfterApproveOffboard(t *testing.T) {
	srv, _ := newTurnServer(t)
	proposalTeam(t, srv)
	path := seedOffboardProposal(t, srv, "buyer")
	got := answerProposal(t, srv, "approve", path)

	p := getProposal(t, srv, path)
	if p.Resolved == nil || !p.Resolved.Approved || p.Resolved.Result != got.Result || p.Resolved.Link != got.Link {
		t.Fatalf("resolved = %+v, answer %+v", p.Resolved, got)
	}
	if got.Result != "Buyer is offboarded and its files are in the archive" {
		t.Errorf("answer = %+v", got)
	}
	if a := p.Summary.Agent; a == nil || a.Name != "Buyer" {
		t.Errorf("archived agent tile = %+v", a)
	}
}

func TestAPIProposalResolvedAfterDeny(t *testing.T) {
	srv, _ := newTurnServer(t)
	proposalTeam(t, srv)
	path := seedHireProposal(t, srv)
	got := answerProposal(t, srv, "deny", path)
	if got.Result != "Garden advisor is not hired" || got.Link != "" {
		t.Errorf("deny said %+v", got)
	}

	p := getProposal(t, srv, path)
	if r := p.Resolved; r == nil || r.Approved || r.Result != got.Result || r.Link != "" {
		t.Fatalf("resolved = %+v", r)
	}
	// The proposal is still readable in full after the answer.
	if len(p.Docs) != 1 || p.Docs[0].After != "# Garden advisor\n" {
		t.Errorf("docs = %+v", p.Docs)
	}
	if _, err := srv.Store.GetAgent("garden-advisor"); err == nil {
		t.Error("a denied hire created the agent")
	}
}

// A denied role update diffs against the role as it is now and says so;
// a waiting one's current role is exactly what approval replaces, so it
// carries no caveat.
func TestAPIProposalDeniedRoleUpdateBeforeIsCurrent(t *testing.T) {
	srv, _ := newTurnServer(t)
	proposalTeam(t, srv)
	path := seedProposal(t, srv, store.Message{
		Title: "Update role: Buyer", Body: "Quotes belong with the buyer.",
		RoleUpdate: &store.RoleUpdate{Slug: "buyer", Body: "# Buyer\n\nOwns vendor quotes.\n"},
	})
	if d := getProposal(t, srv, path).Docs[0]; d.Before == nil || d.BeforeIsCurrent {
		t.Errorf("waiting doc = %+v", d)
	}
	answerProposal(t, srv, "deny", path)
	if err := srv.Store.WriteRole("buyer", "# Buyer\n\nEdited since.\n"); err != nil {
		t.Fatal(err)
	}
	d := getProposal(t, srv, path).Docs[0]
	if d.Before == nil || *d.Before != "# Buyer\n\nEdited since.\n" || !d.BeforeIsCurrent {
		t.Errorf("denied doc = %+v", d)
	}
}

// A role update for an agent offboarded since has no before: its role.md
// moved to the archive, and an empty before would show the whole role
// as added.
func TestAPIProposalRoleUpdateForOffboardedAgent(t *testing.T) {
	srv, _ := newTurnServer(t)
	proposalTeam(t, srv)
	path := seedProposal(t, srv, store.Message{
		Title: "Update role: Buyer", Body: "Quotes belong with the buyer.",
		RoleUpdate: &store.RoleUpdate{Slug: "buyer", Body: "# Buyer\n"},
	})
	if err := srv.Store.ArchiveAgent("buyer"); err != nil {
		t.Fatal(err)
	}
	p := getProposal(t, srv, path)
	if d := p.Docs[0]; d.Before != nil {
		t.Errorf("before = %q", *d.Before)
	}
	if a := p.Summary.Agent; a == nil || a.Name != "Buyer" || a.ReportsTo.Name != "Chief of Staff" {
		t.Errorf("archived agent tile = %+v", a)
	}
}

func TestAPIProposalNotFound(t *testing.T) {
	srv, _ := newTurnServer(t)
	proposalTeam(t, srv)
	plain := seedCEOApprovalRequest(t, srv, "chief-of-staff", "Buy the fab quote", "Quote attached.")
	note := seedCEONotification(t, srv, "chief-of-staff", "Weekly report", "All quiet.")
	for _, p := range []string{
		// Plain approvals are answered on Home; notifications are not
		// proposals either.
		"/api/v1/proposals/" + plain,
		"/api/v1/proposals/" + note,
		"/api/v1/proposals/messages/2026-01-01/0001-nobody.md",
		// Outside messages/, or climbing out of it.
		"/api/v1/proposals/agents/buyer/role.md",
		"/api/v1/proposals/messages%2F..%2Fsystem_instructions.md",
		// An absolute path, even one ending under a messages/ directory.
		"/api/v1/proposals/%2F" + plain,
		"/api/v1/proposals/%2Fetc%2Fpasswd",
		"/api/v1/proposals/messages",
	} {
		rr := proposalDo(t, srv, p)
		assertAPIError(t, rr, http.StatusNotFound)
	}
}
