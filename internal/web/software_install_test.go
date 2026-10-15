package web

import (
	"context"
	"errors"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kivali-ai/kivali/internal/agent"
	"github.com/kivali-ai/kivali/internal/debian"
	"github.com/kivali-ai/kivali/internal/store"
	"github.com/kivali-ai/kivali/internal/web/apitypes"
)

// softwarePod records, at the moment a pod is provisioned, what the
// store holds: the agent's own list and how many approval responses
// exist, so a test can say the pod was recreated BEFORE the answer.
type softwarePod struct {
	st *store.FSStore
	// fail, when set, is what every Provision returns.
	fail error

	mu        sync.Mutex
	slugs     []string
	software  [][]string
	responses []int
	ctxErrs   []error
}

func (p *softwarePod) Provision(ctx context.Context, slug string) error {
	eff, _ := p.st.EffectiveSoftware(slug)
	resps, _ := p.st.ListMessages(store.MessageFilter{Type: store.MsgCEOApprovalResponse})
	p.mu.Lock()
	defer p.mu.Unlock()
	p.slugs = append(p.slugs, slug)
	p.software = append(p.software, eff)
	p.responses = append(p.responses, len(resps))
	p.ctxErrs = append(p.ctxErrs, ctx.Err())
	return p.fail
}

// approvedSoftware is the one approval response's SoftwareInstall.
func approvedSoftware(t *testing.T, srv *Server) *store.SoftwareInstall {
	t.Helper()
	resps, _ := srv.Store.ListMessages(store.MessageFilter{Type: store.MsgCEOApprovalResponse})
	if len(resps) != 1 || resps[0].SoftwareInstall == nil {
		t.Fatalf("responses = %+v", resps)
	}
	return resps[0].SoftwareInstall
}

// lastChat is the newest entry in slug's chat.
func lastChat(t *testing.T, srv *Server, slug string) string {
	t.Helper()
	chat, _ := srv.Store.ReadChatHistory(slug)
	if len(chat) == 0 {
		t.Fatalf("%s chat has no entries", slug)
	}
	return chat[len(chat)-1].Content
}

// The owner closing the page while the shell is recreated cancels the
// request; the recreate goes on regardless, on a context the request
// cannot cancel.
func TestCEOApproveSoftwareInstallProvisionsPastACanceledRequest(t *testing.T) {
	srv, _ := newTurnServer(t)
	softwareTeam(t, srv)
	pod := &softwarePod{st: srv.Store}
	srv.AgentPod = pod
	rel := seedSoftwareProposal(t, srv, "buyer", "ffmpeg")

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := srv.respondAsCEO(ctx, responseKindApproval, boolPtr(true), rel, "", nil); err != nil {
		t.Fatal(err)
	}
	pod.mu.Lock()
	defer pod.mu.Unlock()
	if len(pod.ctxErrs) != 1 || pod.ctxErrs[0] != nil {
		t.Errorf("provision saw context errors %v, want one uncanceled call", pod.ctxErrs)
	}
	if got := approvedSoftware(t, srv).Outcome; got != store.SoftwareRestarted {
		t.Errorf("outcome = %q", got)
	}
}

// Software the agent already gets is not added again, the shell is not
// recreated, and both the agent and the owner hear that it is there.
func TestCEOApproveSoftwareInstallAlreadyInstalled(t *testing.T) {
	srv, _ := newTurnServer(t)
	softwareTeam(t, srv)
	pod := &softwarePod{st: srv.Store}
	srv.AgentPod = pod
	if err := srv.Store.WriteOrgSoftware([]string{"ffmpeg"}); err != nil {
		t.Fatal(err)
	}
	rel := seedSoftwareProposal(t, srv, "buyer", "ffmpeg")

	body, ct := needsForm(t, rel, "", nil)
	rr := homeDo(t, srv, http.MethodPost, "/api/v1/needs/approve", ct, body, nil)
	got := decodeAPI[apitypes.NeedActionResponse](t, rr)
	if rr.Code != http.StatusOK || got.Result != "Buyer already has this software" {
		t.Errorf("approve: code %d result %+v", rr.Code, got)
	}
	if si := approvedSoftware(t, srv); si.Outcome != store.SoftwareAlreadyInstalled || strings.Join(si.Packages, ",") != "ffmpeg" {
		t.Errorf("response = %+v", si)
	}
	if len(pod.slugs) != 0 {
		t.Errorf("provisioned %v for software already there", pod.slugs)
	}
	if last := lastChat(t, srv, "buyer"); !strings.Contains(last, "`ffmpeg` is already in your shell.") {
		t.Errorf("delivery = %q", last)
	}
}

// A shell that cannot be recreated leaves the software saved; the
// answer says so rather than claiming a restart.
func TestCEOApproveSoftwareInstallSavedNotApplied(t *testing.T) {
	srv, _ := newTurnServer(t)
	softwareTeam(t, srv)
	pod := &softwarePod{st: srv.Store, fail: errors.New("cluster unreachable")}
	srv.AgentPod = pod
	rel := seedSoftwareProposal(t, srv, "buyer", "ffmpeg")

	body, ct := needsForm(t, rel, "", nil)
	rr := homeDo(t, srv, http.MethodPost, "/api/v1/needs/approve", ct, body, nil)
	got := decodeAPI[apitypes.NeedActionResponse](t, rr)
	if rr.Code != http.StatusOK || got.Result != "The software is saved for Buyer and installs the next time it starts" {
		t.Errorf("approve: code %d result %+v", rr.Code, got)
	}
	if a, _ := srv.Store.GetAgent("buyer"); strings.Join(a.Software, ",") != "ffmpeg" {
		t.Errorf("own list = %v", a.Software)
	}
	if si := approvedSoftware(t, srv); si.Outcome != store.SoftwareSavedNotApplied {
		t.Errorf("outcome = %q", si.Outcome)
	}
	last := lastChat(t, srv, "buyer")
	if !strings.Contains(last, "It is saved and installs the next time your shell starts.") || strings.Contains(last, "restarted") {
		t.Errorf("delivery = %q", last)
	}
}

func (p *softwarePod) Destroy(context.Context, string) error { return nil }

// seedSoftwareProposal files a software install from slug, as the
// propose_software_install tool does, and returns its path.
func seedSoftwareProposal(t *testing.T, srv *Server, slug string, packages ...string) string {
	t.Helper()
	req := store.Message{
		Type:            store.MsgCEOApprovalRequest,
		Title:           "Install software: PDF tools",
		From:            slug,
		To:              store.Recipients{agent.CEOSlug},
		Date:            time.Now().UTC(),
		Body:            "Quotes arrive as PDFs and I need to read their text.",
		SoftwareInstall: &store.SoftwareInstall{Slug: slug, Packages: packages},
	}
	abs, err := srv.Store.WriteMessage(req)
	if err != nil {
		t.Fatal(err)
	}
	rel, _ := filepath.Rel(srv.Store.Root(), abs)
	_ = srv.Store.CreateAgent(store.Agent{Slug: agent.CEOSlug, Role: "CEO"}, "")
	if err := srv.Store.AppendChatMessage(agent.CEOSlug, store.ChatMessage{
		Role: store.RoleReceived, Content: req.Title, Kind: "ceo_inbox", MessageRef: rel,
	}); err != nil {
		t.Fatal(err)
	}
	return rel
}

func softwareTeam(t *testing.T, srv *Server) {
	t.Helper()
	for _, a := range []store.Agent{
		{Slug: "chief-of-staff", Role: "Chief of Staff", ReportsTo: agent.CEOSlug},
		{Slug: "buyer", Role: "Buyer", ReportsTo: "chief-of-staff"},
	} {
		if err := srv.Store.CreateAgent(a, "# role\n"); err != nil {
			t.Fatal(err)
		}
	}
}

func TestCEOApproveSoftwareInstallSavesListAndRecreatesShellBeforeAnswering(t *testing.T) {
	srv, _ := newTurnServer(t)
	softwareTeam(t, srv)
	pod := &softwarePod{st: srv.Store}
	srv.AgentPod = pod
	rel := seedSoftwareProposal(t, srv, "buyer", "poppler-utils", "ffmpeg")

	if rr := approve(t, srv, rel); rr.Code != http.StatusOK {
		t.Fatalf("code = %d, body = %s", rr.Code, rr.Body.String())
	}

	a, _ := srv.Store.GetAgent("buyer")
	if strings.Join(a.Software, ",") != "ffmpeg,poppler-utils" {
		t.Errorf("own list = %v", a.Software)
	}
	pod.mu.Lock()
	defer pod.mu.Unlock()
	if len(pod.slugs) != 1 || pod.slugs[0] != "buyer" {
		t.Fatalf("provisioned %v, want [buyer]", pod.slugs)
	}
	if strings.Join(pod.software[0], ",") != "ffmpeg,poppler-utils" {
		t.Errorf("the pod was built with %v, want the new list", pod.software[0])
	}
	if pod.responses[0] != 0 {
		t.Errorf("the pod was recreated after %d approval response(s) were written, want before", pod.responses[0])
	}

	resps, _ := srv.Store.ListMessages(store.MessageFilter{Type: store.MsgCEOApprovalResponse})
	if len(resps) != 1 || resps[0].SoftwareInstall == nil || strings.Join(resps[0].SoftwareInstall.Packages, ",") != "ffmpeg,poppler-utils" {
		t.Fatalf("responses = %+v", resps)
	}
	chat, _ := srv.Store.ReadChatHistory("buyer")
	if len(chat) == 0 {
		t.Fatal("buyer chat has no entries")
	}
	last := chat[len(chat)-1].Content
	if !strings.Contains(last, "I approved installing `ffmpeg`, `poppler-utils`") || !strings.Contains(last, "restarted") {
		t.Errorf("delivery = %q", last)
	}
}

func TestCEODenySoftwareInstallChangesNothing(t *testing.T) {
	srv, _ := newTurnServer(t)
	softwareTeam(t, srv)
	pod := &softwarePod{st: srv.Store}
	srv.AgentPod = pod
	rel := seedSoftwareProposal(t, srv, "buyer", "ffmpeg")

	body, ct := needsForm(t, rel, "", nil)
	rr := homeDo(t, srv, http.MethodPost, "/api/v1/needs/deny", ct, body, nil)
	got := decodeAPI[apitypes.NeedActionResponse](t, rr)
	if rr.Code != http.StatusOK || got.Result != "Nothing is installed for Buyer" || got.Link != "" {
		t.Errorf("deny: code %d result %+v", rr.Code, got)
	}
	if a, _ := srv.Store.GetAgent("buyer"); len(a.Software) != 0 {
		t.Errorf("own list = %v, want empty", a.Software)
	}
	if len(pod.slugs) != 0 {
		t.Errorf("a denied request provisioned %v", pod.slugs)
	}
	chat, _ := srv.Store.ReadChatHistory("buyer")
	if len(chat) == 0 || !strings.Contains(chat[len(chat)-1].Content, "I denied installing `ffmpeg`") {
		t.Errorf("buyer chat = %+v", chat)
	}
}

func TestAPINeedsApproveSoftwareInstallOutcome(t *testing.T) {
	srv, _ := newTurnServer(t)
	softwareTeam(t, srv)
	srv.AgentPod = &softwarePod{st: srv.Store}
	rel := seedSoftwareProposal(t, srv, "buyer", "ffmpeg")
	body, ct := needsForm(t, rel, "", nil)
	rr := homeDo(t, srv, http.MethodPost, "/api/v1/needs/approve", ct, body, nil)
	got := decodeAPI[apitypes.NeedActionResponse](t, rr)
	if rr.Code != http.StatusOK || got.Result != "Buyer is restarting to install the software" || got.Link != "/agents/buyer" {
		t.Errorf("approve: code %d result %+v", rr.Code, got)
	}
	if si := approvedSoftware(t, srv); si.Outcome != store.SoftwareRestarted {
		t.Errorf("outcome = %q", si.Outcome)
	}
}

func TestAPIProposalSoftwareInstall(t *testing.T) {
	srv, _ := newTurnServer(t)
	softwareTeam(t, srv)
	path := seedSoftwareProposal(t, srv, "buyer", "poppler-utils")
	srv.Packages = fakePackages{described: map[string]debian.Package{
		"poppler-utils": {Name: "poppler-utils", Version: "25.03.0", Synopsis: "PDF utilities (based on Poppler)"},
	}}

	p := getProposal(t, srv, path)
	if p.Kind != apitypes.ProposalKindSoftwareInstall {
		t.Fatalf("proposal %+v", p)
	}
	if p.Summary.Agent == nil || p.Summary.Agent.Slug != "buyer" || len(p.Summary.Packages) != 1 || p.Summary.Packages[0].Name != "poppler-utils" || p.Summary.Packages[0].Version == nil || *p.Summary.Packages[0].Version != "25.03.0" {
		t.Errorf("summary = %+v", p.Summary)
	}
	if p.ReasonMD != "Quotes arrive as PDFs and I need to read their text." || p.Resolved != nil {
		t.Errorf("reason %q resolved %+v", p.ReasonMD, p.Resolved)
	}
}
