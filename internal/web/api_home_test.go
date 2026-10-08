package web

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kivali-ai/kivali/internal/agent"
	"github.com/kivali-ai/kivali/internal/assignments"
	"github.com/kivali-ai/kivali/internal/store"
	"github.com/kivali-ai/kivali/internal/web/apitypes"
)

// homeDo serves one request through Home's routes behind the API's
// own header and same-origin middleware, as wireAPIRoutes mounts
// them. Non-GET requests carry Sec-Fetch-Site: same-origin unless hdr
// says otherwise.
func homeDo(t *testing.T, srv *Server, method, path, contentType string, body []byte, hdr map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	mux := http.NewServeMux()
	srv.wireAPIHomeRoutes(mux)
	var req *http.Request
	if body == nil {
		req = httptest.NewRequest(method, path, nil)
	} else {
		req = httptest.NewRequest(method, path, bytes.NewReader(body))
		req.Header.Set("content-type", contentType)
	}
	if hdr == nil && method != http.MethodGet {
		hdr = map[string]string{"Sec-Fetch-Site": "same-origin"}
	}
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	rr := httptest.NewRecorder()
	apiHeaders(requireSameOrigin(mux)).ServeHTTP(rr, req)
	return rr
}

func homePostJSON(t *testing.T, srv *Server, path string, v any) *httptest.ResponseRecorder {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return homeDo(t, srv, http.MethodPost, path, "application/json", b, nil)
}

func getHome(t *testing.T, srv *Server) apitypes.Home {
	t.Helper()
	rr := homeDo(t, srv, http.MethodGet, "/api/v1/home", "", nil, nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("GET /api/v1/home: code %d body %s", rr.Code, rr.Body.String())
	}
	return decodeAPI[apitypes.Home](t, rr)
}

// needsForm builds the multipart body the web app sends to
// /api/v1/needs/*: path, message, and files under attachments[].
func needsForm(t *testing.T, path, message string, files map[string]string) ([]byte, string) {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	_ = mw.WriteField("path", path)
	if message != "" {
		_ = mw.WriteField("message", message)
	}
	for name, content := range files {
		fw, err := mw.CreateFormFile("attachments[]", name)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = fw.Write([]byte(content))
	}
	if err := mw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes(), mw.FormDataContentType()
}

// queueNotice queues a notice from chief-of-staff to alice, creating
// either agent when missing, and returns its path. Unlike
// seedPendingRelease it can be called more than once per server.
func queueNotice(t *testing.T, srv *Server, body string) string {
	t.Helper()
	for _, a := range []store.Agent{
		{Slug: "chief-of-staff", Role: "CoS", ReportsTo: agent.CEOSlug},
		{Slug: "alice", Role: "Analyst", ReportsTo: "chief-of-staff"},
	} {
		if _, err := srv.Store.GetAgent(a.Slug); err != nil {
			if err := srv.Store.CreateAgent(a, "# role\n"); err != nil {
				t.Fatal(err)
			}
		}
	}
	abs, err := srv.Store.WriteMessage(store.Message{
		Type: store.MsgNotice, Title: "Draft Q3", From: "chief-of-staff", To: store.Recipients{"alice"},
		Date: time.Now().UTC(), Body: body,
	})
	if err != nil {
		t.Fatal(err)
	}
	rel, _ := filepath.Rel(srv.Store.Root(), abs)
	q, _ := srv.Store.ReadMessageQueue()
	if q.Agents == nil {
		q.Agents = map[string]store.AgentQueue{}
	}
	aq := q.Agents["alice"]
	aq.Inbox = append(aq.Inbox, rel)
	q.Agents["alice"] = aq
	if err := srv.Store.WriteMessageQueue(q); err != nil {
		t.Fatal(err)
	}
	return rel
}

func seedHireProposal(t *testing.T, srv *Server) string {
	t.Helper()
	req := store.Message{
		Type:  store.MsgCEOApprovalRequest,
		Title: "Approve hire: Garden advisor",
		From:  "chief-of-staff",
		To:    store.Recipients{agent.CEOSlug},
		Date:  time.Now().UTC(),
		Body:  "We need someone on the garden.\n",
		Hire: &store.Hire{
			Slug:      "garden-advisor",
			Role:      "Garden advisor",
			ReportsTo: "chief-of-staff",
			Body:      "# Garden advisor\n",
		},
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

func TestAPIHomeEmpty(t *testing.T) {
	srv := newTestServer(t)
	rr := homeDo(t, srv, http.MethodGet, "/api/v1/home", "", nil, nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("code %d body %s", rr.Code, rr.Body.String())
	}
	for _, want := range []string{`"needs":[]`, `"queue":[]`, `"goals":[]`} {
		if !strings.Contains(rr.Body.String(), want) {
			t.Errorf("body lacks %s: %s", want, rr.Body.String())
		}
	}
	h := decodeAPI[apitypes.Home](t, rr)
	if h.AutoRelease != apitypes.AutoReleaseOff || h.HistoryTotal != 0 {
		t.Errorf("home = %+v", h)
	}
}

func TestAPIHomeNeedsProposalAndNotification(t *testing.T) {
	srv, _ := newTurnServer(t)
	if err := srv.Store.CreateAgent(store.Agent{Slug: "chief-of-staff", Role: "Chief of Staff", ReportsTo: agent.CEOSlug}, "# role\n"); err != nil {
		t.Fatal(err)
	}
	hire := seedHireProposal(t, srv)
	note := seedCEONotification(t, srv, "chief-of-staff", "Weekly report", "All quiet.")

	h := getHome(t, srv)
	if len(h.Needs) != 2 {
		t.Fatalf("needs = %+v", h.Needs)
	}
	byID := map[string]apitypes.NeedItem{}
	for _, n := range h.Needs {
		byID[n.ID] = n
	}
	p := byID[hire]
	if p.Kind != apitypes.NeedKindProposal || p.ProposalKind == nil || *p.ProposalKind != apitypes.ProposalKindHire {
		t.Errorf("proposal row = %+v", p)
	}
	if p.ReviewPath == nil || *p.ReviewPath != "/proposals/"+hire {
		t.Errorf("review_path = %v", p.ReviewPath)
	}
	if p.From != (apitypes.PersonRef{Slug: "chief-of-staff", Name: "Chief of Staff"}) || p.RawURL == nil || *p.RawURL != "/"+hire {
		t.Errorf("proposal from/raw = %+v %v", p.From, p.RawURL)
	}
	if p.BodyMD != "We need someone on the garden." {
		t.Errorf("body = %q", p.BodyMD)
	}
	n := byID[note]
	if n.Kind != apitypes.NeedKindNotification || n.ProposalKind != nil || n.ReviewPath != nil || n.Title != "Weekly report" {
		t.Errorf("notification row = %+v", n)
	}
}

func TestAPIHomeNeedsHelpAgent(t *testing.T) {
	srv := newTestServer(t)
	if err := srv.Store.CreateAgent(store.Agent{Slug: "buyer", Role: "Buyer", ReportsTo: agent.CEOSlug}, "# role\n"); err != nil {
		t.Fatal(err)
	}
	for _, m := range []store.ChatMessage{
		{Role: store.RoleReceived, Kind: "direct_chat", Content: "Get three quotes."},
		{Role: store.RoleSent, Kind: store.KindTurnError, Content: "The model returned an overload error."},
	} {
		if err := srv.Store.AppendChatMessage("buyer", m); err != nil {
			t.Fatal(err)
		}
	}
	h := getHome(t, srv)
	if len(h.Needs) != 1 {
		t.Fatalf("needs = %+v", h.Needs)
	}
	n := h.Needs[0]
	if n.ID != "agent:buyer" || n.Kind != apitypes.NeedKindNeedsHelp || n.Agent == nil || n.Agent.State != apitypes.AgentStateNeedsHelp {
		t.Errorf("needs-help row = %+v", n)
	}
	if n.BodyMD != "The model returned an overload error." || n.At.IsZero() || n.RawURL != nil {
		t.Errorf("needs-help body/at/raw = %q %v %v", n.BodyMD, n.At, n.RawURL)
	}
}

func TestAPIHomeQueueReleasesAtFollowsAutoRelease(t *testing.T) {
	srv, _ := newTurnServer(t)
	held := queueNotice(t, srv, "sent while off")
	if err := srv.setAutoRelease("2m"); err != nil {
		t.Fatal(err)
	}
	timed := queueNotice(t, srv, "sent at 2m")

	h := getHome(t, srv)
	if h.AutoRelease != apitypes.AutoRelease2m || len(h.Queue) != 2 {
		t.Fatalf("home = %+v", h)
	}
	byPath := map[string]apitypes.QueueItem{}
	for _, q := range h.Queue {
		byPath[q.Path] = q
	}
	if q := byPath[held]; q.ReleasesAt != nil || !q.Held {
		t.Errorf("message sent while off = %+v, want held with no releases_at", q)
	}
	q := byPath[timed]
	if q.ReleasesAt == nil || q.Held || !q.ReleasesAt.Equal(q.QueuedAt.Add(2*time.Minute)) {
		t.Errorf("message sent at 2m = %+v, want releases_at = queued_at + 2m", q)
	}
	if q.Kind != apitypes.QueueKindNotice || q.From.Name != "CoS" || len(q.To) != 1 || q.To[0] != (apitypes.PersonRef{Slug: "alice", Name: "Analyst"}) {
		t.Errorf("row = %+v", q)
	}
	if q.RawURL != "/"+timed || q.BodyMD != "sent at 2m" {
		t.Errorf("raw/body = %q %q", q.RawURL, q.BodyMD)
	}

	// Off cancels every countdown.
	if err := srv.setAutoRelease("off"); err != nil {
		t.Fatal(err)
	}
	for _, q := range getHome(t, srv).Queue {
		if q.ReleasesAt != nil || !q.Held {
			t.Errorf("after off: %+v", q)
		}
	}
}

func TestAPIQueueRelease(t *testing.T) {
	srv, _ := newTurnServer(t)
	path := queueNotice(t, srv, "please do the thing")

	rr := homePostJSON(t, srv, "/api/v1/queue/release", apitypes.QueueReleaseRequest{Path: path, Note: "  go ahead  "})
	if rr.Code != http.StatusOK {
		t.Fatalf("code %d body %s", rr.Code, rr.Body.String())
	}
	if got := decodeAPI[apitypes.QueueReleaseResponse](t, rr); got.Released != 1 {
		t.Errorf("released = %d", got.Released)
	}
	if q := getHome(t, srv).Queue; len(q) != 0 {
		t.Errorf("queue after release = %+v", q)
	}
	hist := readChat(t, srv, "alice")
	if len(hist) == 0 || !strings.Contains(hist[len(hist)-1].Content, "CEO: go ahead") {
		t.Errorf("alice chat = %+v", hist)
	}

	// Released already: no longer queued.
	assertAPIError(t, homePostJSON(t, srv, "/api/v1/queue/release", apitypes.QueueReleaseRequest{Path: path}), http.StatusNotFound)
	assertAPIError(t, homePostJSON(t, srv, "/api/v1/queue/release", apitypes.QueueReleaseRequest{Path: "../etc/passwd"}), http.StatusBadRequest)
}

func TestAPIQueueReleaseAll(t *testing.T) {
	srv, _ := newTurnServer(t)
	first := queueNotice(t, srv, "one")
	second := queueNotice(t, srv, "two")

	rr := homePostJSON(t, srv, "/api/v1/queue/release-all", apitypes.QueueReleaseAllRequest{
		Paths: []string{first}, Notes: map[string]string{first: "noted"},
	})
	if rr.Code != http.StatusOK {
		t.Fatalf("code %d body %s", rr.Code, rr.Body.String())
	}
	if got := decodeAPI[apitypes.QueueReleaseResponse](t, rr); got.Released != 1 {
		t.Errorf("released = %d, want 1", got.Released)
	}
	q := getHome(t, srv).Queue
	if len(q) != 1 || q[0].Path != second {
		t.Fatalf("queue = %+v, want only the second", q)
	}

	// A bad path is refused, not dropped into release-everything.
	assertAPIError(t, homePostJSON(t, srv, "/api/v1/queue/release-all", apitypes.QueueReleaseAllRequest{Paths: []string{"/etc/passwd"}}), http.StatusBadRequest)
	if len(getHome(t, srv).Queue) != 1 {
		t.Fatal("a refused release-all released something")
	}

	rr = homePostJSON(t, srv, "/api/v1/queue/release-all", apitypes.QueueReleaseAllRequest{})
	if got := decodeAPI[apitypes.QueueReleaseResponse](t, rr); rr.Code != http.StatusOK || got.Released != 1 {
		t.Errorf("release everything: code %d released %d", rr.Code, got.Released)
	}
	if q := getHome(t, srv).Queue; len(q) != 0 {
		t.Errorf("queue after release-all = %+v", q)
	}
}

func TestAPIQueueBounce(t *testing.T) {
	srv := assignmentsServer(t)
	path := queueNotice(t, srv, "please do the thing")

	assertAPIError(t, homePostJSON(t, srv, "/api/v1/queue/bounce", apitypes.QueueBounceRequest{Path: path, Comment: " "}), http.StatusBadRequest)
	rr := homePostJSON(t, srv, "/api/v1/queue/bounce", apitypes.QueueBounceRequest{Path: path, Comment: "Not now."})
	if rr.Code != http.StatusOK || strings.TrimSpace(rr.Body.String()) != "{}" {
		t.Fatalf("bounce: code %d body %s", rr.Code, rr.Body.String())
	}
	if q := getHome(t, srv).Queue; len(q) != 0 {
		t.Errorf("queue after bounce = %+v", q)
	}
	cos := readChat(t, srv, "chief-of-staff")
	if len(cos) == 0 || !strings.Contains(cos[len(cos)-1].Content, "Not now.") {
		t.Errorf("sender was not told: %+v", cos)
	}
	assertAPIError(t, homePostJSON(t, srv, "/api/v1/queue/bounce", apitypes.QueueBounceRequest{Path: path, Comment: "again"}), http.StatusNotFound)
}

func TestAPIQueueBounceAssignmentIsConflict(t *testing.T) {
	srv := assignmentsServer(t)
	if _, err := srv.tracker().Create(context.Background(), "chief-of-staff", assignments.CreateInput{Title: "Work", Assignee: "alice"}); err != nil {
		t.Fatal(err)
	}
	h := getHome(t, srv)
	if len(h.Queue) != 1 {
		t.Fatalf("queue = %+v", h.Queue)
	}
	item := h.Queue[0]
	if item.Kind != apitypes.QueueKindAssignment || item.Assignment == nil || *item.Assignment != (apitypes.AssignmentRef{ID: 1, Title: "Work"}) {
		t.Errorf("assignment row = %+v", item)
	}
	assertAPIError(t, homePostJSON(t, srv, "/api/v1/queue/bounce", apitypes.QueueBounceRequest{Path: item.Path, Comment: "no"}), http.StatusConflict)
	if q, _ := srv.Store.ReadMessageQueue(); len(q.Agents["alice"].Inbox) != 1 {
		t.Fatal("refused bounce dropped the queued wake")
	}
}

func TestAPINeedsApproveWithAttachment(t *testing.T) {
	srv, _ := newTurnServer(t)
	if err := srv.Store.CreateAgent(store.Agent{Slug: "chief-of-staff", Role: "CoS", ReportsTo: agent.CEOSlug}, "# role\n"); err != nil {
		t.Fatal(err)
	}
	path := seedCEOApprovalRequest(t, srv, "chief-of-staff", "Buy the fab quote", "Quote attached.")
	body, ct := needsForm(t, path, "Approved, see my notes.", map[string]string{"notes.txt": "keep it under budget"})

	// A multipart post from another site is refused like any write.
	rr := homeDo(t, srv, http.MethodPost, "/api/v1/needs/approve", ct, body, map[string]string{"Sec-Fetch-Site": "cross-site"})
	assertAPIError(t, rr, http.StatusForbidden)

	rr = homeDo(t, srv, http.MethodPost, "/api/v1/needs/approve", ct, body, nil)
	if rr.Code != http.StatusOK || strings.TrimSpace(rr.Body.String()) != "{}" {
		t.Fatalf("approve: code %d body %s", rr.Code, rr.Body.String())
	}
	if n := getHome(t, srv).Needs; len(n) != 0 {
		t.Errorf("needs after approve = %+v", n)
	}
	cos := readChat(t, srv, "chief-of-staff")
	last := cos[len(cos)-1]
	if len(last.Attachments) != 1 || last.Attachments[0].Name != "notes.txt" {
		t.Fatalf("delivered attachments = %+v", last.Attachments)
	}
	resp, err := srv.Store.ReadMessage(filepath.Join(srv.Store.Root(), last.MessageRef))
	if err != nil {
		t.Fatal(err)
	}
	if resp.Approved == nil || !*resp.Approved || strings.TrimSpace(resp.Body) != "Approved, see my notes." || resp.InReplyTo != path {
		t.Errorf("response = %+v", resp)
	}

	// Answered history shows the attachment with its size.
	hr := homeDo(t, srv, http.MethodGet, "/api/v1/home/history", "", nil, nil)
	hist := decodeAPI[apitypes.HistoryResponse](t, hr)
	if len(hist.Threads) != 1 || len(hist.Threads[0].Replies) != 1 {
		t.Fatalf("history = %+v", hist)
	}
	th := hist.Threads[0]
	if th.Decision == nil || *th.Decision != "approved" || th.Path != path {
		t.Errorf("thread = %+v", th)
	}
	att := th.Replies[0].Attachments
	if len(att) != 1 || att[0].Name != "notes.txt" || att[0].SizeBytes != int64(len("keep it under budget")) || !strings.HasPrefix(att[0].URL, "/attachments/") {
		t.Errorf("reply attachments = %+v", att)
	}
}

func TestAPINeedsHireReturnsResultAndLink(t *testing.T) {
	srv, _ := newTurnServer(t)
	if err := srv.Store.CreateAgent(store.Agent{Slug: "chief-of-staff", Role: "Chief of Staff", ReportsTo: agent.CEOSlug}, "# role\n"); err != nil {
		t.Fatal(err)
	}
	path := seedHireProposal(t, srv)
	body, ct := needsForm(t, path, "", nil)
	rr := homeDo(t, srv, http.MethodPost, "/api/v1/needs/approve", ct, body, nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("code %d body %s", rr.Code, rr.Body.String())
	}
	got := decodeAPI[apitypes.NeedActionResponse](t, rr)
	want := apitypes.NeedActionResponse{Result: "Garden advisor is hired and reports to Chief of Staff", Link: "/agents/garden-advisor"}
	if got != want {
		t.Errorf("result = %+v, want %+v", got, want)
	}
	if _, err := srv.Store.GetAgent("garden-advisor"); err != nil {
		t.Errorf("hire not provisioned: %v", err)
	}
}

// A proposal that cannot be applied is refused in a sentence without
// the form's "hire provision: " prefix, naming who can fix it: the CEO
// by denying, or the proposer by proposing again. The form path keeps
// its text.
func TestAPINeedsApplyRefusalIsASentence(t *testing.T) {
	srv, _ := newTurnServer(t)
	for _, a := range []store.Agent{
		{Slug: "chief-of-staff", Role: "Chief of Staff", ReportsTo: agent.CEOSlug},
		{Slug: "garden-advisor", Role: "Garden advisor", ReportsTo: "chief-of-staff"},
	} {
		if err := srv.Store.CreateAgent(a, "# role\n"); err != nil {
			t.Fatal(err)
		}
	}
	path := seedHireProposal(t, srv)
	body, ct := needsForm(t, path, "", nil)
	e := assertAPIError(t, homeDo(t, srv, http.MethodPost, "/api/v1/needs/approve", ct, body, nil), http.StatusConflict)
	if want := `The hire could not be applied: agent "garden-advisor" already exists`; e.Error != want {
		t.Errorf("error = %q, want %q", e.Error, want)
	}
	if want := "you, by denying it or asking Chief of Staff for a new proposal"; e.Who != want {
		t.Errorf("who = %q, want %q", e.Who, want)
	}

	_, err := srv.respondAsCEO(context.Background(), responseKindApproval, boolPtr(true), path, "", nil)
	var refusal *ceoRefusal
	if !errors.As(err, &refusal) || !strings.HasPrefix(refusal.Msg, "hire provision: ") {
		t.Errorf("form text = %v, want it unchanged with its hire provision: prefix", err)
	}
}

func TestPlainCause(t *testing.T) {
	for in, want := range map[string]string{
		`hire: agent "x" already exists`:     `agent "x" already exists`,
		"hire: reports_to: unknown agent":    "unknown agent",
		"hire bob: create: disk full":        "disk full",
		`agent "a: b" is gone`:               `agent "a: b" is gone`,
		"the role could not be written: EIO": "the role could not be written: EIO",
	} {
		if got := plainCause(errors.New(in)); got != want {
			t.Errorf("plainCause(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestAPINeedsDenyOffboardSaysWhoStays(t *testing.T) {
	srv, _ := newTurnServer(t)
	for _, a := range []store.Agent{
		{Slug: "chief-of-staff", Role: "Chief of Staff", ReportsTo: agent.CEOSlug},
		{Slug: "buyer", Role: "Buyer", ReportsTo: "chief-of-staff"},
	} {
		if err := srv.Store.CreateAgent(a, "# role\n"); err != nil {
			t.Fatal(err)
		}
	}
	path := seedOffboardProposal(t, srv, "buyer")
	body, ct := needsForm(t, path, "", nil)
	rr := homeDo(t, srv, http.MethodPost, "/api/v1/needs/deny", ct, body, nil)
	if got := decodeAPI[apitypes.NeedActionResponse](t, rr); rr.Code != http.StatusOK || got.Result != "Buyer stays on the team" || got.Link != "" {
		t.Errorf("deny: code %d result %+v", rr.Code, got)
	}
}

func TestAPINeedsRefusals(t *testing.T) {
	srv, _ := newTurnServer(t)
	if err := srv.Store.CreateAgent(store.Agent{Slug: "chief-of-staff", Role: "CoS", ReportsTo: agent.CEOSlug}, "# role\n"); err != nil {
		t.Fatal(err)
	}
	approval := seedCEOApprovalRequest(t, srv, "chief-of-staff", "Spend", "ok?")
	body, ct := needsForm(t, approval, "", nil)
	assertAPIError(t, homeDo(t, srv, http.MethodPost, "/api/v1/needs/ack", ct, body, nil), http.StatusBadRequest)
	body, ct = needsForm(t, "messages/2026-01-01/nope.md", "", nil)
	assertAPIError(t, homeDo(t, srv, http.MethodPost, "/api/v1/needs/approve", ct, body, nil), http.StatusNotFound)
	// A plain form (no files) is accepted too.
	form := url.Values{"path": {approval}}.Encode()
	rr := homeDo(t, srv, http.MethodPost, "/api/v1/needs/deny", "application/x-www-form-urlencoded", []byte(form), nil)
	if rr.Code != http.StatusOK {
		t.Errorf("urlencoded deny: code %d body %s", rr.Code, rr.Body.String())
	}
}

func TestAPIAssignmentCloseHandsBack(t *testing.T) {
	srv := assignmentsServer(t)
	ctx := context.Background()
	tr := srv.tracker()
	if _, err := tr.Create(ctx, "chief-of-staff", assignments.CreateInput{Title: "Work", Assignee: "alice"}); err != nil {
		t.Fatal(err)
	}
	if _, err := tr.Create(ctx, "alice", assignments.CreateInput{Title: "Budget?", Body: "How much?", Assignee: agent.CEOSlug, Parent: 1}); err != nil {
		t.Fatal(err)
	}
	h := getHome(t, srv)
	var need *apitypes.NeedItem
	for i := range h.Needs {
		if h.Needs[i].Kind == apitypes.NeedKindAssignment {
			need = &h.Needs[i]
		}
	}
	if need == nil || need.ID != "assignment:2" || need.Assignment == nil || *need.Assignment != (apitypes.AssignmentRef{ID: 2, Title: "Budget?"}) {
		t.Fatalf("needs = %+v", h.Needs)
	}

	assertAPIError(t, homePostJSON(t, srv, "/api/v1/assignments/2/close", map[string]string{"resolution": "maybe", "outcome": "x"}), http.StatusBadRequest)
	assertAPIError(t, homePostJSON(t, srv, "/api/v1/assignments/2/close", apitypes.AssignmentCloseRequest{Resolution: apitypes.AssignmentResolutionDone}), http.StatusBadRequest)
	assertAPIError(t, homePostJSON(t, srv, "/api/v1/assignments/99/close", apitypes.AssignmentCloseRequest{Resolution: apitypes.AssignmentResolutionDone, Outcome: "x"}), http.StatusNotFound)

	rr := homePostJSON(t, srv, "/api/v1/assignments/2/close", apitypes.AssignmentCloseRequest{Resolution: apitypes.AssignmentResolutionDone, Outcome: "$40k."})
	if rr.Code != http.StatusOK {
		t.Fatalf("close: code %d body %s", rr.Code, rr.Body.String())
	}
	for _, n := range getHome(t, srv).Needs {
		if n.Kind == apitypes.NeedKindAssignment {
			t.Errorf("assignment still needs you: %+v", n)
		}
	}
	alice := readChat(t, srv, "alice")
	if len(alice) == 0 || !strings.Contains(alice[len(alice)-1].Content, "Outcome: $40k.") {
		t.Errorf("alice chat = %+v", alice)
	}
	// Closed already: the rules refuse, as a conflict.
	assertAPIError(t, homePostJSON(t, srv, "/api/v1/assignments/2/close", apitypes.AssignmentCloseRequest{Resolution: apitypes.AssignmentResolutionDone, Outcome: "again"}), http.StatusConflict)
}

func TestAPIHomeHistoryPages(t *testing.T) {
	srv := newTestServer(t)
	base := time.Date(2026, 9, 1, 9, 0, 0, 0, time.UTC)
	var paths []string
	for i, title := range []string{"First", "Second", "Third"} {
		abs, err := srv.Store.WriteMessage(store.Message{
			Type: store.MsgNotice, Title: title, From: "chief-of-staff", To: store.Recipients{"alice"},
			Date: base.Add(time.Duration(i) * time.Minute), Body: title + " body",
		})
		if err != nil {
			t.Fatal(err)
		}
		rel, _ := filepath.Rel(srv.Store.Root(), abs)
		paths = append(paths, rel)
	}
	if got := getHome(t, srv).HistoryTotal; got != 3 {
		t.Errorf("history_total = %d, want 3", got)
	}

	rr := homeDo(t, srv, http.MethodGet, "/api/v1/home/history?limit=2", "", nil, nil)
	page := decodeAPI[apitypes.HistoryResponse](t, rr)
	if page.Total != 3 || len(page.Threads) != 2 || page.NextBefore == nil {
		t.Fatalf("first page = %+v", page)
	}
	if page.Threads[0].Path != paths[2] || page.Threads[1].Path != paths[1] {
		t.Errorf("first page order = %s, %s", page.Threads[0].Path, page.Threads[1].Path)
	}
	r0 := page.Threads[0].Request
	if r0.Title != "Third" || r0.Label != "Notice" || r0.BodyMD != "Third body" || r0.RawURL != "/"+paths[2] || len(r0.To) != 1 || r0.To[0].Slug != "alice" {
		t.Errorf("request = %+v", r0)
	}

	rr = homeDo(t, srv, http.MethodGet, "/api/v1/home/history?limit=2&before="+url.QueryEscape(*page.NextBefore), "", nil, nil)
	page = decodeAPI[apitypes.HistoryResponse](t, rr)
	if len(page.Threads) != 1 || page.Threads[0].Path != paths[0] || page.NextBefore != nil {
		t.Errorf("second page = %+v", page)
	}

	assertAPIError(t, homeDo(t, srv, http.MethodGet, "/api/v1/home/history?limit=0", "", nil, nil), http.StatusBadRequest)
	assertAPIError(t, homeDo(t, srv, http.MethodGet, "/api/v1/home/history?before=nope", "", nil, nil), http.StatusBadRequest)
}

// seedCEORequest files an approval request carrying the proposal set
// on m (none for a plain approval) in the CEO's inbox.
func seedCEORequest(t *testing.T, srv *Server, m store.Message) string {
	t.Helper()
	m.Type = store.MsgCEOApprovalRequest
	m.From = "chief-of-staff"
	m.To = store.Recipients{agent.CEOSlug}
	m.Date = time.Now().UTC()
	m.Body = "Please decide.\n"
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

func TestAPIHomeNeedsProposalKinds(t *testing.T) {
	srv := newTestServer(t)
	if err := srv.Store.CreateAgent(store.Agent{Slug: "chief-of-staff", Role: "Chief of Staff", ReportsTo: agent.CEOSlug}, "# role\n"); err != nil {
		t.Fatal(err)
	}
	want := map[string]apitypes.ProposalKind{}
	for kind, m := range map[apitypes.ProposalKind]store.Message{
		apitypes.ProposalKindHire:           {Title: "Hire", Hire: &store.Hire{Slug: "gardener", Role: "Gardener", ReportsTo: "chief-of-staff", Body: "# Gardener\n"}},
		apitypes.ProposalKindRoleUpdate:     {Title: "Role", RoleUpdate: &store.RoleUpdate{Slug: "chief-of-staff", Body: "# new role\n"}},
		apitypes.ProposalKindHandbookUpdate: {Title: "Handbook", HandbookUpdate: &store.HandbookUpdate{Body: "# rules\n"}},
		apitypes.ProposalKindOffboard:       {Title: "Offboard", Offboard: &store.Offboard{Slug: "chief-of-staff"}},
		apitypes.ProposalKindReorg:          {Title: "Reorg", Reorg: &store.Reorg{Moves: []store.ReorgMove{{Slug: "chief-of-staff", NewManager: agent.CEOSlug}}}},
	} {
		want[seedCEORequest(t, srv, m)] = kind
	}
	plain := seedCEORequest(t, srv, store.Message{Title: "Spend $40"})

	needs := getHome(t, srv).Needs
	if len(needs) != len(want)+1 {
		t.Fatalf("needs = %+v", needs)
	}
	for _, n := range needs {
		if n.ID == plain {
			if n.Kind != apitypes.NeedKindApproval || n.ProposalKind != nil || n.ReviewPath != nil {
				t.Errorf("plain approval = %+v", n)
			}
			continue
		}
		kind, ok := want[n.ID]
		if !ok {
			t.Errorf("unexpected need %+v", n)
			continue
		}
		if n.Kind != apitypes.NeedKindProposal || n.ProposalKind == nil || *n.ProposalKind != kind {
			t.Errorf("%s: kind %s proposal_kind %v, want proposal %s", n.Title, n.Kind, n.ProposalKind, kind)
		}
		if n.ReviewPath == nil || *n.ReviewPath != "/proposals/"+n.ID {
			t.Errorf("%s: review_path %v", n.Title, n.ReviewPath)
		}
		if n.RawURL == nil || *n.RawURL != "/"+n.ID || !strings.HasPrefix(n.ID, "messages/") {
			t.Errorf("%s: raw_url %v", n.Title, n.RawURL)
		}
	}
}

func TestAPIHomeQueueOneRowForThreeRecipients(t *testing.T) {
	srv := newTestServer(t)
	for _, a := range []store.Agent{
		{Slug: "chief-of-staff", Role: "CoS", ReportsTo: agent.CEOSlug},
		{Slug: "alice", Role: "Analyst", ReportsTo: "chief-of-staff"},
		{Slug: "bob", Role: "Buyer", ReportsTo: "chief-of-staff"},
		{Slug: "carol", Role: "Counsel", ReportsTo: "chief-of-staff"},
	} {
		if err := srv.Store.CreateAgent(a, "# role\n"); err != nil {
			t.Fatal(err)
		}
	}
	abs, err := srv.Store.WriteMessage(store.Message{
		Type: store.MsgNotice, Title: "Kickoff", From: "chief-of-staff", To: store.Recipients{"carol", "alice", "bob"},
		Date: time.Now().UTC(), Body: "Kickoff at noon.",
	})
	if err != nil {
		t.Fatal(err)
	}
	rel, _ := filepath.Rel(srv.Store.Root(), abs)
	q := store.MessageQueue{Agents: map[string]store.AgentQueue{}}
	for _, slug := range []string{"carol", "alice", "bob"} {
		q.Agents[slug] = store.AgentQueue{Inbox: []string{rel}}
	}
	if err := srv.Store.WriteMessageQueue(q); err != nil {
		t.Fatal(err)
	}
	queue := getHome(t, srv).Queue
	if len(queue) != 1 {
		t.Fatalf("queue = %+v, want one row", queue)
	}
	want := []apitypes.PersonRef{{Slug: "alice", Name: "Analyst"}, {Slug: "bob", Name: "Buyer"}, {Slug: "carol", Name: "Counsel"}}
	got := queue[0]
	if len(got.To) != 3 || got.To[0] != want[0] || got.To[1] != want[1] || got.To[2] != want[2] {
		t.Errorf("to = %+v, want %+v", got.To, want)
	}
	if got.Path != rel || got.Kind != apitypes.QueueKindNotice || got.Title != "Kickoff" || got.RawURL != "/"+rel || !got.Held {
		t.Errorf("row = %+v", got)
	}
}

func TestAPINeedsRefusesJSONAndEmptyReleaseAllReleasesNothing(t *testing.T) {
	srv, _ := newTurnServer(t)
	if err := srv.Store.CreateAgent(store.Agent{Slug: "chief-of-staff", Role: "CoS", ReportsTo: agent.CEOSlug}, "# role\n"); err != nil {
		t.Fatal(err)
	}
	approval := seedCEOApprovalRequest(t, srv, "chief-of-staff", "Spend", "ok?")
	assertAPIError(t, homePostJSON(t, srv, "/api/v1/needs/approve", map[string]string{"path": approval}), http.StatusUnsupportedMediaType)
	if len(getHome(t, srv).Needs) != 1 {
		t.Fatal("a refused JSON approve answered the request")
	}

	queued := queueNotice(t, srv, "hold on")
	rr := homeDo(t, srv, http.MethodPost, "/api/v1/queue/release-all", "application/json", []byte(`{"paths":[]}`), nil)
	if got := decodeAPI[apitypes.QueueReleaseResponse](t, rr); rr.Code != http.StatusOK || got.Released != 0 {
		t.Errorf("empty list: code %d released %d", rr.Code, got.Released)
	}
	if q := getHome(t, srv).Queue; len(q) != 1 || q[0].Path != queued {
		t.Errorf("an empty release-all list released something: %+v", q)
	}
}

func TestAPIHomeHistoryPagesThroughEqualTimes(t *testing.T) {
	srv := newTestServer(t)
	at := time.Date(2026, 9, 1, 9, 0, 0, 0, time.UTC)
	seen := map[string]bool{}
	// One timestamp, three recipients: three files, three threads.
	for _, to := range []string{"carol", "alice", "bob"} {
		if _, err := srv.Store.WriteMessage(store.Message{
			Type: store.MsgNotice, Title: "Kickoff", From: "chief-of-staff", To: store.Recipients{to}, Date: at, Body: "noon",
		}); err != nil {
			t.Fatal(err)
		}
	}
	var order []string
	before := ""
	for range 4 {
		u := "/api/v1/home/history?limit=1"
		if before != "" {
			u += "&before=" + url.QueryEscape(before)
		}
		page := decodeAPI[apitypes.HistoryResponse](t, homeDo(t, srv, http.MethodGet, u, "", nil, nil))
		for _, th := range page.Threads {
			if seen[th.Path] {
				t.Fatalf("%s served twice", th.Path)
			}
			seen[th.Path] = true
			order = append(order, th.Path)
		}
		if page.NextBefore == nil {
			break
		}
		before = *page.NextBefore
	}
	if len(order) != 3 || order[0] > order[1] || order[1] > order[2] {
		t.Errorf("equal-time threads paged as %v, want all three in path order", order)
	}
}
