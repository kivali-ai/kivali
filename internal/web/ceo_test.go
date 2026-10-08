package web

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kivali-ai/kivali/internal/agent"
	"github.com/kivali-ai/kivali/internal/store"
)

// seedCEOApprovalRequest writes a ceo_approval_request from `from` and
// records it in the CEO's chat as a Kind="ceo_inbox" entry (the same
// way routeProduced would). Returns the relative path of the request.
func seedCEOApprovalRequest(t *testing.T, srv *Server, from, title, body string) string {
	t.Helper()
	req := store.Message{
		Type:  store.MsgCEOApprovalRequest,
		Title: title,
		From:  from,
		To:    store.Recipients{agent.CEOSlug},
		Date:  time.Now().UTC(),
		Body:  body,
	}
	abs, err := srv.Store.WriteMessage(req)
	if err != nil {
		t.Fatalf("write req: %v", err)
	}
	rel, _ := filepath.Rel(srv.Store.Root(), abs)
	_ = srv.Store.CreateAgent(store.Agent{Slug: agent.CEOSlug, Role: "CEO"}, "")
	if err := srv.Store.AppendChatMessage(agent.CEOSlug, store.ChatMessage{
		Role:       store.RoleReceived,
		Content:    title,
		Kind:       "ceo_inbox",
		MessageRef: rel,
	}); err != nil {
		t.Fatalf("append ceo inbox: %v", err)
	}
	return rel
}

func seedCEONotification(t *testing.T, srv *Server, from, title, body string) string {
	t.Helper()
	req := store.Message{
		Type:  store.MsgCEONotification,
		Title: title,
		From:  from,
		To:    store.Recipients{agent.CEOSlug},
		Date:  time.Now().UTC(),
		Body:  body,
	}
	abs, err := srv.Store.WriteMessage(req)
	if err != nil {
		t.Fatalf("write req: %v", err)
	}
	rel, _ := filepath.Rel(srv.Store.Root(), abs)
	_ = srv.Store.CreateAgent(store.Agent{Slug: agent.CEOSlug, Role: "CEO"}, "")
	if err := srv.Store.AppendChatMessage(agent.CEOSlug, store.ChatMessage{
		Role:       store.RoleReceived,
		Content:    title,
		Kind:       "ceo_inbox",
		MessageRef: rel,
	}); err != nil {
		t.Fatalf("append ceo inbox: %v", err)
	}
	return rel
}

// seedOffboardProposal writes a CoS-authored ceo_approval_request
// carrying an Offboard for slug and puts it in the CEO's inbox,
// exactly as a propose_offboard tool call would. Returns the request's
// relative path, ready to POST to /ceo/approve.
func seedOffboardProposal(t *testing.T, srv *Server, slug string) string {
	t.Helper()
	req := store.Message{
		Type:     store.MsgCEOApprovalRequest,
		Title:    "Offboard: " + slug,
		From:     "chief-of-staff",
		To:       store.Recipients{agent.CEOSlug},
		Date:     time.Now().UTC(),
		Body:     "Scope has been folded elsewhere.",
		Offboard: &store.Offboard{Slug: slug, Reason: "Scope has been folded elsewhere."},
	}
	abs, err := srv.Store.WriteMessage(req)
	if err != nil {
		t.Fatalf("write offboard req: %v", err)
	}
	rel, _ := filepath.Rel(srv.Store.Root(), abs)
	_ = srv.Store.CreateAgent(store.Agent{Slug: agent.CEOSlug, Role: "CEO"}, "")
	if err := srv.Store.AppendChatMessage(agent.CEOSlug, store.ChatMessage{
		Role: store.RoleReceived, Content: req.Title,
		Kind: "ceo_inbox", MessageRef: rel,
	}); err != nil {
		t.Fatalf("append ceo inbox: %v", err)
	}
	return rel
}

// approve POSTs /ceo/approve for a seeded request path.
func approve(t *testing.T, srv *Server, relPath string) *httptest.ResponseRecorder {
	t.Helper()
	form := strings.NewReader("path=" + relPath)
	r := httptest.NewRequest(http.MethodPost, "/api/v1/needs/approve", form)
	r.Header.Set("content-type", "application/x-www-form-urlencoded")
	r.Header.Set("Sec-Fetch-Site", "same-origin")
	rr := httptest.NewRecorder()
	authedHandler(t, srv).ServeHTTP(rr, r)
	return rr
}

// The replacement for the Fire button, end to end: a CoS proposal the
// CEO approves archives the agent and tells CoS it happened.
func TestCEOApproveOffboardArchivesAgent(t *testing.T) {
	srv, _ := newTurnServer(t)
	_ = srv.Store.CreateAgent(store.Agent{Slug: "chief-of-staff", Role: "CoS"}, "# role\n")
	_ = srv.Store.CreateAgent(store.Agent{Slug: "market-analyst", Role: "Analyst", ReportsTo: "chief-of-staff"}, "# Analyst\n")
	rel := seedOffboardProposal(t, srv, "market-analyst")

	if rr := approve(t, srv, rel); rr.Code != http.StatusOK {
		t.Fatalf("code = %d, body = %s", rr.Code, rr.Body.String())
	}

	if _, err := srv.Store.GetAgent("market-analyst"); err == nil {
		t.Error("market-analyst should no longer be active")
	}
	if _, err := srv.Store.GetArchivedAgent("market-analyst"); err != nil {
		t.Errorf("market-analyst should be archived, not deleted: %v", err)
	}

	// The response carries the slug (so the paraphrase can name who
	// left) but drops the reason, which lives on the request file.
	resps, _ := srv.Store.ListMessages(store.MessageFilter{Type: store.MsgCEOApprovalResponse})
	if len(resps) != 1 {
		t.Fatalf("expected 1 approval response; got %d", len(resps))
	}
	off := resps[0].Offboard
	if off == nil {
		t.Fatal("response should carry offboard metadata")
	}
	if off.Slug != "market-analyst" {
		t.Errorf("offboard slug on response = %q", off.Slug)
	}
	if off.Reason != "" {
		t.Errorf("reason should be dropped from the response; got %q", off.Reason)
	}

	// CoS must be told the archive already happened — otherwise it
	// reads "approved" as permission to go do it, and there is no
	// longer anything it can call to do it.
	cosChat, _ := srv.Store.ReadChatHistory("chief-of-staff")
	if len(cosChat) == 0 {
		t.Fatal("cos chat has no entries")
	}
	last := cosChat[len(cosChat)-1].Content
	if !strings.Contains(last, "letting `market-analyst` go") {
		t.Errorf("delivery should name who left: %q", last)
	}
	if !strings.Contains(last, "archived") {
		t.Errorf("delivery should state the archive is done: %q", last)
	}
}

// Denying leaves the agent alone and says so.
func TestCEODenyOffboardLeavesAgentActive(t *testing.T) {
	srv, _ := newTurnServer(t)
	_ = srv.Store.CreateAgent(store.Agent{Slug: "chief-of-staff", Role: "CoS"}, "# role\n")
	_ = srv.Store.CreateAgent(store.Agent{Slug: "market-analyst", Role: "Analyst", ReportsTo: "chief-of-staff"}, "# Analyst\n")
	rel := seedOffboardProposal(t, srv, "market-analyst")

	form := strings.NewReader("path=" + rel + "&message=not+yet")
	r := httptest.NewRequest(http.MethodPost, "/api/v1/needs/deny", form)
	r.Header.Set("content-type", "application/x-www-form-urlencoded")
	r.Header.Set("Sec-Fetch-Site", "same-origin")
	rr := httptest.NewRecorder()
	authedHandler(t, srv).ServeHTTP(rr, r)
	if rr.Code != http.StatusOK {
		t.Fatalf("code = %d, body = %s", rr.Code, rr.Body.String())
	}
	if _, err := srv.Store.GetAgent("market-analyst"); err != nil {
		t.Errorf("a denied offboard must leave the agent active: %v", err)
	}
	if _, err := srv.Store.GetArchivedAgent("market-analyst"); err == nil {
		t.Error("a denied offboard must not archive the agent")
	}
	// The denial has to name who was spared. CoS can have several
	// proposals in flight, and a bare "I denied your request" against
	// a queue of them identifies nothing.
	cosChat, _ := srv.Store.ReadChatHistory("chief-of-staff")
	if len(cosChat) == 0 {
		t.Fatal("cos chat has no entries")
	}
	last := cosChat[len(cosChat)-1].Content
	if !strings.Contains(last, "market-analyst") {
		t.Errorf("denial should name the agent who stays: %q", last)
	}
	if !strings.Contains(last, "remain an active agent") {
		t.Errorf("denial should state they are still active: %q", last)
	}
}

// The direct-reports gate: approving would orphan bob, so the approval
// is refused and the card stays in the inbox for CoS to fix with a
// propose_reorg and the CEO to approve again.
func TestCEOApproveOffboardRefusesWhenAgentHasDirectReports(t *testing.T) {
	srv, _ := newTurnServer(t)
	_ = srv.Store.CreateAgent(store.Agent{Slug: "chief-of-staff", Role: "CoS"}, "# role\n")
	_ = srv.Store.CreateAgent(store.Agent{Slug: "manager", Role: "Lead", ReportsTo: "chief-of-staff"}, "# Lead\n")
	_ = srv.Store.CreateAgent(store.Agent{Slug: "bob", Role: "IC", ReportsTo: "manager"}, "# IC\n")
	rel := seedOffboardProposal(t, srv, "manager")

	rr := approve(t, srv, rel)
	if rr.Code != http.StatusConflict {
		t.Fatalf("code = %d, want 409; body = %s", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), "bob") {
		t.Errorf("refusal should name the orphaned report: %q", rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), "propose_reorg") {
		t.Errorf("refusal should say how to fix it: %q", rr.Body.String())
	}
	if _, err := srv.Store.GetAgent("manager"); err != nil {
		t.Errorf("manager must stay active after a refused offboard: %v", err)
	}
	// No response written — the request is still actionable.
	resps, _ := srv.Store.ListMessages(store.MessageFilter{Type: store.MsgCEOApprovalResponse})
	if len(resps) != 0 {
		t.Errorf("a refused offboard must not write an approval response; got %d", len(resps))
	}

	// Reorg bob away, and the same proposal now applies.
	if _, err := srv.Runtime.ApplyReorg(store.Reorg{Moves: []store.ReorgMove{{Slug: "bob", NewManager: "chief-of-staff"}}}); err != nil {
		t.Fatalf("ApplyReorg: %v", err)
	}
	if rr := approve(t, srv, rel); rr.Code != http.StatusOK {
		t.Fatalf("after reorg, offboard should apply: code = %d, body = %s", rr.Code, rr.Body.String())
	}
	if _, err := srv.Store.GetArchivedAgent("manager"); err != nil {
		t.Errorf("manager should be archived once it has no reports: %v", err)
	}
}

// The CEO and the Chief of Staff can't be offboarded even if a
// proposal naming them reaches the approval handler — the parser
// refuses these, and ApplyOffboard refuses them again.
func TestCEOApproveOffboardRefusesLoadBearingSlugs(t *testing.T) {
	for _, slug := range []string{agent.CEOSlug, "chief-of-staff"} {
		srv, _ := newTurnServer(t)
		_ = srv.Store.CreateAgent(store.Agent{Slug: "chief-of-staff", Role: "CoS"}, "# role\n")
		rel := seedOffboardProposal(t, srv, slug)
		rr := approve(t, srv, rel)
		if rr.Code != http.StatusConflict {
			t.Errorf("offboarding %q should be refused; code = %d", slug, rr.Code)
		}
		if _, err := srv.Store.GetArchivedAgent(slug); err == nil {
			t.Errorf("%q must not be archived", slug)
		}
	}
}

func TestCEOApproveWritesResponseAndDeliversToSender(t *testing.T) {
	srv, _ := newTurnServer(t)
	_ = srv.Store.CreateAgent(store.Agent{Slug: "chief-of-staff", Role: "CoS"}, "# role\n")
	path := seedCEOApprovalRequest(t, srv, "chief-of-staff", "Approve hire: Analyst", "please approve")

	form := strings.NewReader("path=" + path + "&message=go+for+it")
	req := httptest.NewRequest(http.MethodPost, "/api/v1/needs/approve", form)
	req.Header.Set("content-type", "application/x-www-form-urlencoded")
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	rr := httptest.NewRecorder()
	authedHandler(t, srv).ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("code = %d, body = %s", rr.Code, rr.Body.String())
	}

	cosChat, err := srv.Store.ReadChatHistory("chief-of-staff")
	if err != nil {
		t.Fatalf("cos chat: %v", err)
	}
	if len(cosChat) == 0 {
		t.Fatal("cos chat has no entries")
	}
	last := cosChat[len(cosChat)-1]
	if last.Kind != "inbox_delivery" {
		t.Errorf("last entry kind = %q, want inbox_delivery", last.Kind)
	}
	if last.Role != store.RoleReceived {
		t.Errorf("last entry role = %q", last.Role)
	}
	if !strings.Contains(last.Content, "I approved your request.") {
		t.Errorf("content missing approval speech act: %q", last.Content)
	}
	if !strings.Contains(last.Content, "go for it") {
		t.Errorf("content missing CEO's custom message: %q", last.Content)
	}

	ceoChat, _ := srv.Store.ReadChatHistory(agent.CEOSlug)
	var found bool
	for _, e := range ceoChat {
		if e.Kind == "ceo_reply" && e.Role == store.RoleSent {
			found = true
			break
		}
	}
	if !found {
		t.Error("ceo chat missing sent-response entry")
	}
}

func TestCEODenyPersistsApprovedFalse(t *testing.T) {
	srv, _ := newTurnServer(t)
	_ = srv.Store.CreateAgent(store.Agent{Slug: "cos", Role: "CoS"}, "# role\n")
	path := seedCEOApprovalRequest(t, srv, "cos", "Approve: X", "pls")

	form := strings.NewReader("path=" + path + "&message=too+risky")
	req := httptest.NewRequest(http.MethodPost, "/api/v1/needs/deny", form)
	req.Header.Set("content-type", "application/x-www-form-urlencoded")
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	rr := httptest.NewRecorder()
	authedHandler(t, srv).ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("code = %d, body = %s", rr.Code, rr.Body.String())
	}
	list, _ := srv.Store.ListMessages(store.MessageFilter{Type: store.MsgCEOApprovalResponse})
	if len(list) != 1 {
		t.Fatalf("responses = %d, want 1", len(list))
	}
	resp := list[0]
	if resp.Approved == nil || *resp.Approved {
		t.Errorf("Approved should be false-ptr, got %v", resp.Approved)
	}
	if resp.InReplyTo != path {
		t.Errorf("InReplyTo = %q, want %q", resp.InReplyTo, path)
	}
}

func TestCEOAckRequiresNotificationType(t *testing.T) {
	srv, _ := newTurnServer(t)
	_ = srv.Store.CreateAgent(store.Agent{Slug: "cos", Role: "CoS"}, "# role\n")
	path := seedCEOApprovalRequest(t, srv, "cos", "Approve", "pls")

	form := strings.NewReader("path=" + path)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/needs/ack", form)
	req.Header.Set("content-type", "application/x-www-form-urlencoded")
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	rr := httptest.NewRecorder()
	authedHandler(t, srv).ServeHTTP(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Errorf("code = %d, want 400", rr.Code)
	}
}

func TestCEOApproveRejectsNotificationType(t *testing.T) {
	srv, _ := newTurnServer(t)
	_ = srv.Store.CreateAgent(store.Agent{Slug: "cos", Role: "CoS"}, "# role\n")
	path := seedCEONotification(t, srv, "cos", "FYI", "heads up")

	form := strings.NewReader("path=" + path)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/needs/approve", form)
	req.Header.Set("content-type", "application/x-www-form-urlencoded")
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	rr := httptest.NewRecorder()
	authedHandler(t, srv).ServeHTTP(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Errorf("code = %d, want 400", rr.Code)
	}
}

// Approving a hire-flavored ceo_approval_request must provision the
// new agent BEFORE writing the approval response, so the
// requester's only path to "agent created" runs through the CEO's
// explicit approve click. No publish_agent_role tool exists; the
// provisioning is purely server-side.
func TestCEOApproveHireProvisionsAgentBeforeResponse(t *testing.T) {
	srv, _ := newTurnServer(t)
	_ = srv.Store.CreateAgent(store.Agent{Slug: "chief-of-staff", Role: "CoS"}, "# role\n")

	// Seed a hire-flavored approval request directly.
	req := store.Message{
		Type:  store.MsgCEOApprovalRequest,
		Title: "Approve hire: Analyst",
		From:  "chief-of-staff",
		To:    store.Recipients{agent.CEOSlug},
		Date:  time.Now().UTC(),
		Body:  "please approve",
		Hire: &store.Hire{
			Slug:               "market-analyst",
			Role:               "Market Analyst",
			Icon:               "chart-line",
			ReportsTo:          "chief-of-staff",
			Body:               "# Market Analyst\n\nOwns market sizing.\n",
			InitialAgentMemory: "Starting fresh.\n",
		},
	}
	abs, err := srv.Store.WriteMessage(req)
	if err != nil {
		t.Fatalf("write req: %v", err)
	}
	relPath, _ := filepath.Rel(srv.Store.Root(), abs)
	_ = srv.Store.CreateAgent(store.Agent{Slug: agent.CEOSlug, Role: "CEO"}, "")
	_ = srv.Store.AppendChatMessage(agent.CEOSlug, store.ChatMessage{
		Role: store.RoleReceived, Content: req.Title,
		Kind: "ceo_inbox", MessageRef: relPath,
	})

	// Pre-condition: market-analyst does not exist.
	if _, err := srv.Store.GetAgent("market-analyst"); err == nil {
		t.Fatal("market-analyst should not exist before approve")
	}

	form := strings.NewReader("path=" + relPath + "&message=welcome")
	r := httptest.NewRequest(http.MethodPost, "/api/v1/needs/approve", form)
	r.Header.Set("content-type", "application/x-www-form-urlencoded")
	r.Header.Set("Sec-Fetch-Site", "same-origin")
	rr := httptest.NewRecorder()
	authedHandler(t, srv).ServeHTTP(rr, r)
	if rr.Code != http.StatusOK {
		t.Fatalf("code = %d, body = %s", rr.Code, rr.Body.String())
	}

	// Post-condition: agent exists with role.md + agent_memory.md.
	got, err := srv.Store.GetAgent("market-analyst")
	if err != nil {
		t.Fatalf("agent should exist after approve: %v", err)
	}
	if got.ReportsTo != "chief-of-staff" {
		t.Errorf("ReportsTo = %q, want chief-of-staff", got.ReportsTo)
	}
	// The icon the Chief of Staff picked travels through the stored
	// request into the agent's record.
	if got.Icon != "chart-line" {
		t.Errorf("Icon = %q, want chart-line", got.Icon)
	}
	if role, _ := srv.Store.ReadRole("market-analyst"); !strings.Contains(role, "Market Analyst") {
		t.Errorf("role.md missing expected content: %q", role)
	}
	if mem, _ := srv.Store.ReadAgentMemory("market-analyst"); !strings.Contains(mem, "Starting fresh") {
		t.Errorf("agent_memory.md missing initial seed: %q", mem)
	}

	// And the approval response is delivered to CoS as usual.
	cosChat, _ := srv.Store.ReadChatHistory("chief-of-staff")
	if len(cosChat) == 0 {
		t.Fatal("cos chat has no entries")
	}
	last := cosChat[len(cosChat)-1]
	if last.Kind != "inbox_delivery" || last.Role != store.RoleReceived {
		t.Errorf("last entry kind=%q role=%q; want inbox_delivery/received", last.Kind, last.Role)
	}

	// Reported-speech frame: the CEO's "voice" carries the
	// decision AND the provisioning facts, so the model reads a
	// coherent speech act rather than structured labels.
	if !strings.Contains(last.Content, "I approved your hire of Market Analyst") {
		t.Errorf("delivery should narrate the hire in CEO's voice: %q", last.Content)
	}
	if !strings.Contains(last.Content, "The agent has been provisioned and is now live.") {
		t.Errorf("delivery should state provisioning as part of the speech act: %q", last.Content)
	}

	// Hire metadata must be persisted on the response message so
	// downstream rendering and any future replay draw from the same
	// source of truth as the paraphrase. Body/memory dropped (they
	// live on the provisioned agent now).
	resps, _ := srv.Store.ListMessages(store.MessageFilter{Type: store.MsgCEOApprovalResponse})
	if len(resps) != 1 {
		t.Fatalf("expected 1 approval response; got %d", len(resps))
	}
	rh := resps[0].Hire
	if rh == nil {
		t.Fatal("response should carry the hire metadata after approving a hire request")
	}
	if rh.Slug != "market-analyst" || rh.Role != "Market Analyst" || rh.ReportsTo != "chief-of-staff" {
		t.Errorf("hire metadata wrong on response: %+v", rh)
	}
	if rh.Body != "" || rh.InitialAgentMemory != "" {
		t.Errorf("hire body/memory should be dropped from the response to avoid bloat; got body=%q memory=%q", rh.Body, rh.InitialAgentMemory)
	}
}

// Denying a hire-flavored request must NOT provision the agent.
// The CEO's optional message can instruct the sender to revise and
// resubmit — it's pure communication, no server-side side effect.
func TestCEODenyHireDoesNotProvision(t *testing.T) {
	srv, _ := newTurnServer(t)
	_ = srv.Store.CreateAgent(store.Agent{Slug: "chief-of-staff", Role: "CoS"}, "# role\n")

	req := store.Message{
		Type:  store.MsgCEOApprovalRequest,
		Title: "Approve hire: Analyst",
		From:  "chief-of-staff",
		To:    store.Recipients{agent.CEOSlug},
		Date:  time.Now().UTC(),
		Body:  "please approve",
		Hire: &store.Hire{
			Slug:      "analyst",
			Role:      "Analyst",
			ReportsTo: "chief-of-staff",
			Body:      "# Analyst\n",
		},
	}
	abs, _ := srv.Store.WriteMessage(req)
	relPath, _ := filepath.Rel(srv.Store.Root(), abs)
	_ = srv.Store.CreateAgent(store.Agent{Slug: agent.CEOSlug, Role: "CEO"}, "")
	_ = srv.Store.AppendChatMessage(agent.CEOSlug, store.ChatMessage{
		Role: store.RoleReceived, Kind: "ceo_inbox", MessageRef: relPath,
	})

	form := strings.NewReader("path=" + relPath + "&message=resubmit+with+X")
	r := httptest.NewRequest(http.MethodPost, "/api/v1/needs/deny", form)
	r.Header.Set("content-type", "application/x-www-form-urlencoded")
	r.Header.Set("Sec-Fetch-Site", "same-origin")
	rr := httptest.NewRecorder()
	authedHandler(t, srv).ServeHTTP(rr, r)
	if rr.Code != http.StatusOK {
		t.Fatalf("code = %d, body = %s", rr.Code, rr.Body.String())
	}
	if _, err := srv.Store.GetAgent("analyst"); err == nil {
		t.Error("analyst should NOT exist after deny")
	}
}

// If provisioning would conflict (slug already taken), approve must
// fail without writing a response — the request stays pending so
// the CEO can retry or deny with a modification message.
func TestCEOApproveHireFailsOnSlugConflict(t *testing.T) {
	srv, _ := newTurnServer(t)
	_ = srv.Store.CreateAgent(store.Agent{Slug: "chief-of-staff", Role: "CoS"}, "# role\n")
	_ = srv.Store.CreateAgent(store.Agent{Slug: "analyst", Role: "Analyst"}, "# existing\n")

	req := store.Message{
		Type:  store.MsgCEOApprovalRequest,
		Title: "Approve hire: Analyst",
		From:  "chief-of-staff",
		To:    store.Recipients{agent.CEOSlug},
		Date:  time.Now().UTC(),
		Body:  "please approve",
		Hire: &store.Hire{
			Slug: "analyst", Role: "Analyst", ReportsTo: "chief-of-staff",
			Body: "# Analyst\n",
		},
	}
	abs, _ := srv.Store.WriteMessage(req)
	relPath, _ := filepath.Rel(srv.Store.Root(), abs)
	_ = srv.Store.CreateAgent(store.Agent{Slug: agent.CEOSlug, Role: "CEO"}, "")

	form := strings.NewReader("path=" + relPath)
	r := httptest.NewRequest(http.MethodPost, "/api/v1/needs/approve", form)
	r.Header.Set("content-type", "application/x-www-form-urlencoded")
	r.Header.Set("Sec-Fetch-Site", "same-origin")
	rr := httptest.NewRecorder()
	authedHandler(t, srv).ServeHTTP(rr, r)
	if rr.Code < 400 {
		t.Fatalf("expected 4xx on slug conflict, got %d: %s", rr.Code, rr.Body.String())
	}
	// No approval response should have been written.
	list, _ := srv.Store.ListMessages(store.MessageFilter{Type: store.MsgCEOApprovalResponse})
	if len(list) != 0 {
		t.Errorf("expected no approval response on failed provisioning; got %d", len(list))
	}
}

// Approving a role-update proposal must overwrite role.md atomically
// BEFORE writing the approval response, so the ack landing in the
// proposer's chat is itself the signal that the new role is live.
// Mirrors the hire-approve path.
func TestCEOApproveRoleUpdateOverwritesRoleMd(t *testing.T) {
	srv, _ := newTurnServer(t)
	_ = srv.Store.CreateAgent(store.Agent{Slug: "chief-of-staff", Role: "CoS"}, "# role\n")
	_ = srv.Store.CreateAgent(store.Agent{Slug: "market-analyst", Role: "Analyst", ReportsTo: "chief-of-staff"}, "# Analyst\n\nOriginal scope.\n")

	req := store.Message{
		Type:  store.MsgCEOApprovalRequest,
		Title: "Update market-analyst role: expand to APAC",
		From:  "chief-of-staff",
		To:    store.Recipients{agent.CEOSlug},
		Date:  time.Now().UTC(),
		Body:  "Adding APAC coverage to scope; the rest of the role unchanged.",
		RoleUpdate: &store.RoleUpdate{
			Slug: "market-analyst",
			Body: "# Analyst\n\nOriginal scope plus APAC coverage.\n",
		},
	}
	abs, err := srv.Store.WriteMessage(req)
	if err != nil {
		t.Fatalf("write req: %v", err)
	}
	relPath, _ := filepath.Rel(srv.Store.Root(), abs)
	_ = srv.Store.CreateAgent(store.Agent{Slug: agent.CEOSlug, Role: "CEO"}, "")
	_ = srv.Store.AppendChatMessage(agent.CEOSlug, store.ChatMessage{
		Role: store.RoleReceived, Content: req.Title,
		Kind: "ceo_inbox", MessageRef: relPath,
	})

	// Pre-condition: role.md does NOT yet contain the new content.
	if before, _ := srv.Store.ReadRole("market-analyst"); strings.Contains(before, "APAC") {
		t.Fatalf("role.md unexpectedly already contains the proposed change")
	}

	form := strings.NewReader("path=" + relPath + "&message=looks+good")
	r := httptest.NewRequest(http.MethodPost, "/api/v1/needs/approve", form)
	r.Header.Set("content-type", "application/x-www-form-urlencoded")
	r.Header.Set("Sec-Fetch-Site", "same-origin")
	rr := httptest.NewRecorder()
	authedHandler(t, srv).ServeHTTP(rr, r)
	if rr.Code != http.StatusOK {
		t.Fatalf("code = %d, body = %s", rr.Code, rr.Body.String())
	}

	// Post-condition: role.md was overwritten with the new body.
	got, err := srv.Store.ReadRole("market-analyst")
	if err != nil {
		t.Fatalf("read role.md: %v", err)
	}
	if !strings.Contains(got, "APAC coverage") {
		t.Errorf("role.md was not overwritten; got %q", got)
	}

	// Approval response carries RoleUpdate metadata for downstream
	// rendering, but carries no Body, to avoid bloating the
	// response file.
	resps, _ := srv.Store.ListMessages(store.MessageFilter{Type: store.MsgCEOApprovalResponse})
	if len(resps) != 1 {
		t.Fatalf("expected 1 approval response; got %d", len(resps))
	}
	ru := resps[0].RoleUpdate
	if ru == nil {
		t.Fatal("response should carry the role_update metadata after approving a role update")
	}
	if ru.Slug != "market-analyst" {
		t.Errorf("role_update slug wrong on response: %q", ru.Slug)
	}
	if ru.Body != "" {
		t.Errorf("role_update body should be dropped from the response to avoid bloat; got %q", ru.Body)
	}

	// CoS sees the ack as reported speech, with the role.md update
	// stated in the same speech act so the model doesn't misread it
	// as "approval acknowledged, now go apply the update yourself."
	cosChat, _ := srv.Store.ReadChatHistory("chief-of-staff")
	if len(cosChat) == 0 {
		t.Fatal("cos chat has no entries")
	}
	last := cosChat[len(cosChat)-1]
	if !strings.Contains(last.Content, "I approved your role update for `market-analyst`") {
		t.Errorf("delivery should narrate the role update in CEO's voice: %q", last.Content)
	}
	if !strings.Contains(last.Content, "now live") {
		t.Errorf("delivery should state the role.md is live: %q", last.Content)
	}
}

// Approving a handbook_update overwrites the on-disk handbook
// atomically before writing the approval response. Mirrors the
// role-update test.
func TestCEOApproveHandbookUpdateOverwritesHandbook(t *testing.T) {
	srv, _ := newTurnServer(t)
	_ = srv.Store.CreateAgent(store.Agent{Slug: "chief-of-staff", Role: "CoS"}, "# role\n")
	if err := srv.Store.WriteHandbook("# Handbook\n\nOriginal rules.\n"); err != nil {
		t.Fatalf("seed handbook: %v", err)
	}

	req := store.Message{
		Type:  store.MsgCEOApprovalRequest,
		Title: "Handbook: tighten escalation rule",
		From:  "chief-of-staff",
		To:    store.Recipients{agent.CEOSlug},
		Date:  time.Now().UTC(),
		Body:  "Customer issues need a stricter escalation path.",
		HandbookUpdate: &store.HandbookUpdate{
			Body: "# Handbook\n\nOriginal rules plus stricter escalation.\n",
		},
	}
	abs, err := srv.Store.WriteMessage(req)
	if err != nil {
		t.Fatalf("write req: %v", err)
	}
	relPath, _ := filepath.Rel(srv.Store.Root(), abs)
	_ = srv.Store.CreateAgent(store.Agent{Slug: agent.CEOSlug, Role: "CEO"}, "")
	_ = srv.Store.AppendChatMessage(agent.CEOSlug, store.ChatMessage{
		Role: store.RoleReceived, Content: req.Title,
		Kind: "ceo_inbox", MessageRef: relPath,
	})

	if before, _ := srv.Store.ReadHandbook(); strings.Contains(before, "stricter escalation") {
		t.Fatalf("handbook unexpectedly already contains the proposed change")
	}

	form := strings.NewReader("path=" + relPath + "&message=looks+good")
	r := httptest.NewRequest(http.MethodPost, "/api/v1/needs/approve", form)
	r.Header.Set("content-type", "application/x-www-form-urlencoded")
	r.Header.Set("Sec-Fetch-Site", "same-origin")
	rr := httptest.NewRecorder()
	authedHandler(t, srv).ServeHTTP(rr, r)
	if rr.Code != http.StatusOK {
		t.Fatalf("code = %d, body = %s", rr.Code, rr.Body.String())
	}

	got, err := srv.Store.ReadHandbook()
	if err != nil {
		t.Fatalf("read handbook: %v", err)
	}
	if !strings.Contains(got, "stricter escalation") {
		t.Errorf("handbook was not overwritten; got %q", got)
	}

	resps, _ := srv.Store.ListMessages(store.MessageFilter{Type: store.MsgCEOApprovalResponse})
	if len(resps) != 1 {
		t.Fatalf("expected 1 approval response; got %d", len(resps))
	}
	cu := resps[0].HandbookUpdate
	if cu == nil {
		t.Fatal("response should carry the handbook_update marker after approving")
	}
	if cu.Body != "" {
		t.Errorf("handbook_update body should be dropped from the response to avoid bloat; got %q", cu.Body)
	}

	cosChat, _ := srv.Store.ReadChatHistory("chief-of-staff")
	if len(cosChat) == 0 {
		t.Fatal("cos chat has no entries")
	}
	last := cosChat[len(cosChat)-1]
	if !strings.Contains(last.Content, "I approved your handbook update") {
		t.Errorf("delivery should narrate the handbook update in CEO's voice: %q", last.Content)
	}
	if !strings.Contains(last.Content, "now live") {
		t.Errorf("delivery should state the handbook is live: %q", last.Content)
	}
}

// Approve on an unknown-slug role_update must fail without writing
// the response, so the proposal stays in inbox for the CEO to retry
// or deny.
func TestCEOApproveRoleUpdateFailsOnMissingAgent(t *testing.T) {
	srv, _ := newTurnServer(t)
	_ = srv.Store.CreateAgent(store.Agent{Slug: "chief-of-staff", Role: "CoS"}, "# role\n")

	req := store.Message{
		Type:  store.MsgCEOApprovalRequest,
		Title: "Update ghost role",
		From:  "chief-of-staff",
		To:    store.Recipients{agent.CEOSlug},
		Date:  time.Now().UTC(),
		Body:  "rationale",
		RoleUpdate: &store.RoleUpdate{
			Slug: "does-not-exist",
			Body: "# replacement\n",
		},
	}
	abs, _ := srv.Store.WriteMessage(req)
	relPath, _ := filepath.Rel(srv.Store.Root(), abs)
	_ = srv.Store.CreateAgent(store.Agent{Slug: agent.CEOSlug, Role: "CEO"}, "")

	form := strings.NewReader("path=" + relPath)
	r := httptest.NewRequest(http.MethodPost, "/api/v1/needs/approve", form)
	r.Header.Set("content-type", "application/x-www-form-urlencoded")
	r.Header.Set("Sec-Fetch-Site", "same-origin")
	rr := httptest.NewRecorder()
	authedHandler(t, srv).ServeHTTP(rr, r)
	if rr.Code < 400 {
		t.Fatalf("expected 4xx for missing agent, got %d: %s", rr.Code, rr.Body.String())
	}
	list, _ := srv.Store.ListMessages(store.MessageFilter{Type: store.MsgCEOApprovalResponse})
	if len(list) != 0 {
		t.Errorf("expected no approval response when role_update fails to apply; got %d", len(list))
	}
}

func TestCEOInboxBuckets(t *testing.T) {
	srv, _ := newTurnServer(t)
	_ = srv.Store.CreateAgent(store.Agent{Slug: "cos", Role: "CoS"}, "# role\n")

	openPath := seedCEOApprovalRequest(t, srv, "cos", "Approve A", "...")
	resolvedPath := seedCEOApprovalRequest(t, srv, "cos", "Approve B", "...")

	form := strings.NewReader("path=" + resolvedPath + "&message=ok")
	req := httptest.NewRequest(http.MethodPost, "/api/v1/needs/approve", form)
	req.Header.Set("content-type", "application/x-www-form-urlencoded")
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	rr := httptest.NewRecorder()
	authedHandler(t, srv).ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("approve: code = %d", rr.Code)
	}

	view, err := srv.buildCEOInbox(1)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if len(view.Needs) != 1 || view.Needs[0].RequestPath != openPath {
		t.Errorf("needs-action = %+v (want just %s)", view.Needs, openPath)
	}
	if view.HistoryTotal != 1 || len(view.History) != 1 || view.History[0].RequestPath != resolvedPath {
		t.Errorf("history total=%d items=%+v (want 1 item for %s)", view.HistoryTotal, view.History, resolvedPath)
	}
	if !view.History[0].Resolved || view.History[0].Response == nil {
		t.Errorf("resolved item should carry Response, got %+v", view.History[0])
	}
}
