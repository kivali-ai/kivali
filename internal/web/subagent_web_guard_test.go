package web

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/kivali-ai/kivali/internal/agent"
	"github.com/kivali-ai/kivali/internal/agentpod"
	"github.com/kivali-ai/kivali/internal/browserpod"
	"github.com/kivali-ai/kivali/internal/clock"
	"github.com/kivali-ai/kivali/internal/files"
	"github.com/kivali-ai/kivali/internal/provider"
	"github.com/kivali-ai/kivali/internal/store"
	"github.com/kivali-ai/kivali/internal/structured"
	"github.com/kivali-ai/kivali/internal/web/apitypes"
)

// webNeedsServer is a server whose subagent service runs web tasks for
// alice, so Home lists their needs and answers them over HTTP. The
// service runs on clk, and alice has quote.pdf under her /files/.
func webNeedsServer(t *testing.T, driver SubagentDriver, clk clock.Clock) (*Server, *SubagentService, *deliveryRecorder, *fakeBrowserProv) {
	t.Helper()
	srv, _ := newTurnServer(t)
	if err := srv.Store.CreateAgent(store.Agent{Slug: "alice", Role: "Analyst", ReportsTo: agent.CEOSlug}, "role"); err != nil {
		t.Fatal(err)
	}
	agentRoot := filepath.Join(srv.Store.Root(), "agents", "alice")
	if err := files.Sync(files.BootstrapOptions{AgentRoot: agentRoot}); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(files.StorageRoot(agentRoot), "artifacts", "private", "quote.pdf"), []byte("pdf"), 0o644); err != nil {
		t.Fatal(err)
	}
	del := newDeliveryRecorder(t)
	svc := &SubagentService{
		Provider:        provider.MockProvider{},
		Store:           srv.Store,
		Driver:          driver,
		EmitToParent:    func(string, string, any) {},
		DeliverToParent: del.deliver,
		Clock:           clk,
	}
	prov := newFakeBrowserProv()
	svc.Browsers = newBrowserRegistry(srv.Store, prov, clock.New(), 3, t.Logf, func(slug, id string) string { return subagentFilesRoot(srv.Store, slug, id) })
	svc.Browsers.markBooted()
	srv.SubagentService = svc
	endTasksAtCleanup(t, svc)
	return srv, svc, del, prov
}

// endTasksAtCleanup cancels whatever the test left awaiting approval or
// running and waits for each to end, before the test's directories go.
func endTasksAtCleanup(t *testing.T, svc *SubagentService) {
	t.Helper()
	t.Cleanup(func() {
		svc.CancelAllSubagents("alice")
		for _, j := range svc.listJobs("alice") {
			if j.cancel == nil {
				continue // registered by the test, with nothing running
			}
			select {
			case <-j.done:
			case <-time.After(10 * time.Second):
				t.Errorf("task %s never ended", j.ID)
			}
		}
	})
}

// uploadArgs is a web task that uploads quote.pdf, starting on
// vendor.com.
func uploadArgs(t *testing.T) json.RawMessage {
	return webArgs(t, map[string]any{
		"start_url": "https://portal.vendor.com/quotes/new",
		"files":     []string{"/files/artifacts/private/quote.pdf"},
	})
}

// answerNeed posts an answer to a Needs-you row the way the web app does.
func answerNeed(t *testing.T, srv *Server, verb, id, message string) (int, apitypes.NeedActionResponse, string) {
	t.Helper()
	body, ct := needsForm(t, id, message, nil)
	rr := homeDo(t, srv, http.MethodPost, "/api/v1/needs/"+verb, ct, body, nil)
	var out apitypes.NeedActionResponse
	if rr.Code == http.StatusOK {
		out = decodeAPI[apitypes.NeedActionResponse](t, rr)
	}
	return rr.Code, out, rr.Body.String()
}

// onlyJob is alice's one job.
func onlyJob(t *testing.T, svc *SubagentService) subagentJob {
	t.Helper()
	jobs := svc.listJobs("alice")
	if len(jobs) != 1 {
		t.Fatalf("jobs = %d, want 1", len(jobs))
	}
	return jobs[0]
}

// needWithID is Home's row with id, if listed.
func needWithID(t *testing.T, srv *Server, id string) (apitypes.NeedItem, bool) {
	t.Helper()
	for _, n := range getHome(t, srv).Needs {
		if n.ID == id {
			return n, true
		}
	}
	return apitypes.NeedItem{}, false
}

// TestWebUploadNeedsAStartURLOnOneSite: a task that names files needs a
// start URL on a site, inside its session's site in a site session,
// and only a task the agent dispatches itself may name files.
func TestWebUploadNeedsAStartURLOnOneSite(t *testing.T) {
	svc, parentRoot, _, _ := webService(t, &fakeSubagentDriver{})
	endTasksAtCleanup(t, svc)
	if err := os.WriteFile(filepath.Join(parentRoot, "artifacts", "private", "quote.pdf"), []byte("pdf"), 0o644); err != nil {
		t.Fatal(err)
	}
	quote := []string{"/files/artifacts/private/quote.pdf"}
	cases := []struct {
		name string
		args json.RawMessage
		want string
	}{
		{"files without a start url", webArgs(t, map[string]any{"files": quote}), "needs a start_url on the site it uploads to"},
		{"start url on an address", webArgs(t, map[string]any{"files": quote, "start_url": "https://203.0.113.9/upload"}), "is not on a site"},
		{"start url off the session's site", webArgs(t, map[string]any{"files": quote, "session": "vendor.com", "start_url": "https://other.com/upload"}), "is off the session's site, vendor.com"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := svc.StartBatch(context.Background(), "alice", tc.args); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("err = %v, want %q", err, tc.want)
			}
		})
	}
	nested := []SubagentTaskInput{{Description: "d", Prompt: "p", Kind: agentpod.SubagentKindWeb, Schema: json.RawMessage(quoteSchema), StartURL: "https://vendor.com/", Files: quote}}
	if err := svc.validateWebTasksFor("alice", "lead1", nested); err == nil || !strings.Contains(err.Error(), "dispatched by a subagent cannot upload files") {
		t.Errorf("nested upload: %v", err)
	}
	if len(svc.listJobs("alice")) != 0 {
		t.Error("a refused dispatch registered a job")
	}
	// In a site session, a start URL on the session's site is accepted
	// and the task is held to it.
	if _, err := svc.StartBatch(context.Background(), "alice", webArgs(t, map[string]any{"files": quote, "session": "vendor.com", "start_url": "https://portal.vendor.com/"})); err != nil {
		t.Fatal(err)
	}
	if j := onlyJob(t, svc); j.State != subagentJobAwaitingApproval || j.LockSite != "vendor.com" || j.Site != "vendor.com" {
		t.Errorf("site-session upload job: state %s lock %q site %q", j.State, j.LockSite, j.Site)
	}
}

// TestWebUploadWaitsForApprovalThenRuns: a task that uploads files
// waits, with nothing run, until a person approves it on Home; the
// agent's status and Home both show it; approval starts it, held to the
// start URL's site, and a second answer is refused.
func TestWebUploadWaitsForApprovalThenRuns(t *testing.T) {
	var svc *SubagentService
	ran := make(chan agentpod.SubagentSpec, 1)
	driver := &fakeSubagentDriver{respond: func(req fakeDriveCall) (string, error) {
		ran <- req.Spec
		if _, isErr := svc.SubmitResult("alice", req.SubagentID, json.RawMessage(`{"outcome":"found","result":{"vendor":"Acme","price":12}}`)); isErr {
			t.Error("a valid result was refused")
		}
		return "", nil
	}}
	srv, s, del, _ := webNeedsServer(t, driver, nil)
	svc = s
	receipt, err := svc.StartBatch(context.Background(), "alice", uploadArgs(t))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(receipt, "waits for a person to approve uploading 1 file to vendor.com") {
		t.Errorf("receipt = %s", receipt)
	}
	j := onlyJob(t, svc)
	if j.State != subagentJobAwaitingApproval || j.LockSite != "vendor.com" || j.Approved {
		t.Fatalf("job before approval: state %s lock %q approved %v", j.State, j.LockSite, j.Approved)
	}
	id := webUploadNeedPrefix + "alice/" + j.ID
	need, ok := needWithID(t, srv, id)
	if !ok {
		t.Fatalf("Home lists no %s", id)
	}
	if need.Kind != apitypes.NeedKindWebUpload || need.Title != "Upload 1 file to vendor.com" || need.From.Name != "Analyst" || need.WebUpload == nil ||
		need.WebUpload.StartURL != "https://portal.vendor.com/quotes/new" || !slices.Equal(need.WebUpload.Files, []string{"/files/artifacts/private/quote.pdf"}) ||
		need.WebUpload.Prompt != "find the price" || need.WebUpload.Site != "vendor.com" || need.WebUpload.Description != "price the renewal" {
		t.Errorf("web_upload row = %+v %+v", need, need.WebUpload)
	}
	apitypes.NoNilSlices(t, need)
	// Each file links to a download of the bytes approved at dispatch,
	// read again and checked; nothing of it is kept in the meantime.
	fileURL := "/api/v1/agents/alice/subagents/" + j.ID + "/files/quote.pdf"
	if len(need.Attachments) != 1 || need.Attachments[0].Name != "quote.pdf" || need.Attachments[0].SizeBytes != 3 || need.Attachments[0].URL != fileURL {
		t.Errorf("attachments = %+v", need.Attachments)
	}
	if _, err := srv.Store.GetAttachment(j.ApprovedFiles[0].SHA); err == nil {
		t.Error("the file was copied into the attachment store")
	}
	rr := homeDo(t, srv, http.MethodGet, fileURL, "", nil, nil)
	if body, _ := io.ReadAll(rr.Body); rr.Code != http.StatusOK || string(body) != "pdf" ||
		rr.Header().Get("Content-Type") != "application/octet-stream" || !strings.HasPrefix(rr.Header().Get("Content-Disposition"), "attachment;") {
		t.Errorf("download: %d %q %v", rr.Code, body, rr.Header())
	}
	if rr := homeDo(t, srv, http.MethodGet, "/api/v1/agents/alice/subagents/"+j.ID+"/files/other.pdf", "", nil, nil); rr.Code != http.StatusNotFound {
		t.Errorf("a file not the task's: %d", rr.Code)
	}
	if rr := homeDo(t, srv, http.MethodGet, "/api/v1/agents/bob/subagents/"+j.ID+"/files/quote.pdf", "", nil, nil); rr.Code != http.StatusNotFound {
		t.Errorf("another agent's path: %d", rr.Code)
	}
	quotePath := filepath.Join(files.StorageRoot(filepath.Join(srv.Store.Root(), "agents", "alice")), "artifacts", "private", "quote.pdf")
	if err := os.WriteFile(quotePath, []byte("PDF"), 0o644); err != nil {
		t.Fatal(err)
	}
	if rr := homeDo(t, srv, http.MethodGet, fileURL, "", nil, nil); rr.Code != http.StatusConflict || !strings.Contains(rr.Body.String(), "has changed since the task was dispatched") {
		t.Errorf("a changed file: %d %s", rr.Code, rr.Body.String())
	}
	if err := os.WriteFile(quotePath, []byte("pdf"), 0o644); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(need.BodyMD, "puts the files only on its pages. What the site does with a file after that is up to the site") {
		t.Errorf("body = %s", need.BodyMD)
	}
	if snap := srv.orgSnapshotValue(); !slices.Contains(snap.Inbox.CEOPaths, id) || snap.Inbox.Unactioned < 1 {
		t.Errorf("snapshot inbox = %+v", snap.Inbox)
	}
	if status := svc.SubagentStatus("alice"); !strings.Contains(status, "WAITING FOR APPROVAL") || !strings.Contains(status, "upload 1 file to vendor.com") {
		t.Errorf("status = %s", status)
	}
	if view := srv.subagentMetaView("alice", "alice", j.ID, nil); view.State != apitypes.SubagentTaskStateAwaitingApproval || view.Browser != "" {
		t.Errorf("task view: state %s browser %q", view.State, view.Browser)
	}
	select {
	case spec := <-ran:
		t.Fatalf("the task ran before approval: %+v", spec)
	default:
	}
	// The wrong verb is refused and changes nothing.
	if code, _, body := answerNeed(t, srv, "ack", id, ""); code != http.StatusBadRequest {
		t.Errorf("ack on an upload: %d %s", code, body)
	}

	code, out, body := answerNeed(t, srv, "approve", id, "")
	if code != http.StatusOK || out.Result != "Upload approved. The task starts now" || out.Link != "/agents/alice/subagents/"+j.ID {
		t.Fatalf("approve: %d %+v %s", code, out, body)
	}
	spec := <-ran
	if spec.Site != "vendor.com" || slices.Contains(spec.Tools.Kivali, agentpod.WebEvalTool) {
		t.Errorf("approved spec: site %q tools %v", spec.Site, spec.Tools.Kivali)
	}
	if delivered := del.waitFor(1)[0].Content; !strings.Contains(delivered, `"Acme"`) {
		t.Errorf("delivered = %s", delivered)
	}
	if j, _ := svc.lookupJob(j.ID); !j.Approved || j.State != subagentJobCompleted {
		t.Errorf("final job: approved %v state %s", j.Approved, j.State)
	}
	if _, listed := needWithID(t, srv, id); listed {
		t.Error("the row stayed on Home after approval")
	}
	if code, _, _ := answerNeed(t, srv, "deny", id, "too late"); code != http.StatusConflict {
		t.Errorf("a second answer: %d", code)
	}
}

// TestWebUploadDeniedEndsBlockedWithTheReason: a denial ends the task
// without running it, blocked, upload_denied, with the person's words.
func TestWebUploadDeniedEndsBlockedWithTheReason(t *testing.T) {
	driver := &fakeSubagentDriver{respond: func(fakeDriveCall) (string, error) {
		t.Error("a denied task ran")
		return "", nil
	}}
	srv, svc, del, prov := webNeedsServer(t, driver, nil)
	if _, err := svc.StartBatch(context.Background(), "alice", uploadArgs(t)); err != nil {
		t.Fatal(err)
	}
	j := onlyJob(t, svc)
	code, out, body := answerNeed(t, srv, "deny", webUploadNeedPrefix+"alice/"+j.ID, "Not that vendor; use the other portal.")
	if code != http.StatusOK || out.Result != "Upload denied. The task ends without uploading anything" {
		t.Fatalf("deny: %d %+v %s", code, out, body)
	}
	delivered := del.waitFor(1)[0].Content
	for _, want := range []string{`"outcome": "blocked"`, `"blocked_on": "upload_denied"`, `"reason": "Not that vendor; use the other portal."`} {
		if !strings.Contains(delivered, want) {
			t.Errorf("delivered lacks %s:\n%s", want, delivered)
		}
	}
	if j, _ := svc.lookupJob(j.ID); j.State != subagentJobCompleted {
		t.Errorf("state = %s", j.State)
	}
	if len(prov.started) != 0 {
		t.Error("a denied task started a browser")
	}
}

// TestWebUploadEndsBlockedWhenNobodyAnswers: after the limit on the
// service's clock the task ends blocked, user_did_not_respond, and a
// late answer is refused.
func TestWebUploadEndsBlockedWhenNobodyAnswers(t *testing.T) {
	driver := &fakeSubagentDriver{respond: func(fakeDriveCall) (string, error) {
		t.Error("an unanswered task ran")
		return "", nil
	}}
	clk := clock.NewFakeAt(time.Date(2026, 10, 19, 9, 0, 0, 0, time.UTC))
	srv, svc, del, _ := webNeedsServer(t, driver, clk)
	if _, err := svc.StartBatch(context.Background(), "alice", uploadArgs(t)); err != nil {
		t.Fatal(err)
	}
	j := onlyJob(t, svc)
	clk.Advance(webApprovalLimit - time.Second)
	if j, _ := svc.lookupJob(j.ID); j.State != subagentJobAwaitingApproval {
		t.Fatalf("state before the limit = %s", j.State)
	}
	clk.Advance(time.Second)
	delivered := del.waitFor(1)[0].Content
	if !strings.Contains(delivered, `"blocked_on": "user_did_not_respond"`) {
		t.Errorf("delivered = %s", delivered)
	}
	if code, _, _ := answerNeed(t, srv, "approve", webUploadNeedPrefix+"alice/"+j.ID, ""); code != http.StatusConflict {
		t.Errorf("a late approval: %d", code)
	}
}

// TestWebUploadAwaitingApprovalCanBeCancelled: subagent_cancel ends a
// task awaiting approval with nothing delivered, and its row leaves.
func TestWebUploadAwaitingApprovalCanBeCancelled(t *testing.T) {
	srv, svc, del, _ := webNeedsServer(t, &fakeSubagentDriver{}, nil)
	if _, err := svc.StartBatch(context.Background(), "alice", uploadArgs(t)); err != nil {
		t.Fatal(err)
	}
	j := onlyJob(t, svc)
	if !svc.CancelSubagent("alice", j.ID) {
		t.Fatal("cancel refused")
	}
	select {
	case <-j.done:
	case <-time.After(10 * time.Second):
		t.Fatal("the cancelled task never ended")
	}
	if j, _ := svc.lookupJob(j.ID); j.State != subagentJobCancelled {
		t.Errorf("state = %s", j.State)
	}
	if n := len(del.waitFor(0)); n != 0 {
		t.Errorf("a cancelled task delivered %d message(s)", n)
	}
	if _, listed := needWithID(t, srv, webUploadNeedPrefix+"alice/"+j.ID); listed {
		t.Error("the row stayed on Home after cancel")
	}
}

// TestWebURLCost: ordinary URLs, ids and searches cost nothing; past
// the free allowance every byte of a path and query, and of a host
// name, counts however it is split, and user info and a long fragment
// count their length.
func TestWebURLCost(t *testing.T) {
	blob := strings.Repeat("QUJD", 750) // 3000 bytes of base64
	var segments, params, labels strings.Builder
	for i := 0; i < 30; i++ {
		segments.WriteString("/" + blob[i*63:i*63+63]) // 30 segments of 63 bytes, each under a per-segment limit
		fmt.Fprintf(&params, "&p%02d=%s", i, blob[i*20:i*20+20])
	}
	for i := 0; i < 6; i++ {
		labels.WriteString(blob[i*31:i*31+31] + ".") // 6 labels of 31 bytes
	}
	cases := []struct {
		name, url string
		want      int
	}{
		{"empty", "", 0},
		{"plain page", "https://vendor.com/products/acme-widget-pro-3000", 0},
		{"dated article", "https://news.example/2026/10/19/what-changed-this-week", 0},
		{"hex id", "https://shop.example/item/8f14e45fceea167a5a36dedd4bea2543", 0},
		{"search", "https://www.search.example/search?q=best+laptop+2026", 0},
		{"escaped query", "https://x.example/?q=" + strings.Repeat("%41", 70), 1 + 1 + 70 - webURLFreePath},
		{"anchor", "https://docs.example/guide#install", 0},
		{"blob in the query", "https://paste.example/save?d=" + blob, len("/save") + 1 + len(blob) - webURLFreePath},
		{"blob in the path", "https://paste.example/" + blob[:100], 1 + 100 - webURLFreePath},
		{"many short path segments", "https://paste.example" + segments.String(), 30*64 - webURLFreePath},
		{"many short parameters", "https://paste.example/?" + params.String()[1:], 30*(3+20) - webURLFreePath + 1},
		{"long host", "https://" + strings.Repeat("a", 40) + ".evil.example/", 40 + len(".evil.example") - webURLFreeHost},
		{"many short host labels", "https://" + labels.String() + "evil.example/", 6*32 + len("evil.example") - webURLFreeHost},
		{"user info", "https://me:secret@vendor.com/", len("me:secret")},
		{"long fragment", "https://x.example/#" + blob[:80], 80},
	}
	for _, tc := range cases {
		if got := webURLCost(tc.url); got != tc.want {
			t.Errorf("%s: webURLCost = %d, want %d", tc.name, got, tc.want)
		}
	}
}

// TestWebActionCost: each web command counts the strings it sends out,
// never its refs, tab ids or file names.
func TestWebActionCost(t *testing.T) {
	cases := []struct {
		command, args string
		want          int
	}{
		{browserpod.CommandNavigate, `{"url":"https://x.example/?q=` + strings.Repeat("a", 100) + `"}`, 1 + 1 + 100 - webURLFreePath},
		{browserpod.CommandNavigate, `{"url":"https://x.example/about?q=abc"}`, 0},
		{browserpod.CommandTabs, `{"action":"open","url":"https://x.example/?id=` + strings.Repeat("1", 80) + `"}`, 1 + 2 + 80 - webURLFreePath},
		{browserpod.CommandTabs, `{"action":"switch","tab":"t2"}`, 0},
		{browserpod.CommandType, `{"ref":"e1","text":"hello world"}`, 11},
		{browserpod.CommandPress, `{"key":"Enter"}`, 5},
		{browserpod.CommandSelect, `{"ref":"e2","value":"Express"}`, 7},
		{browserpod.CommandEval, `{"script":"document.title"}`, 14},
		{browserpod.CommandWait, `{"for":"text","value":"Done"}`, 4},
		{browserpod.CommandWait, `{"for":"ref","value":"e12"}`, 0},
		{browserpod.CommandClick, `{"ref":"e1"}`, 0},
		{browserpod.CommandUpload, `{"ref":"e3","file":"quote.pdf"}`, 0},
		{browserpod.CommandScroll, `{"direction":"down"}`, 0},
		{browserpod.CommandSnapshot, `{}`, 0},
	}
	for _, tc := range cases {
		if got := webActionCost(tc.command, json.RawMessage(tc.args)); got != tc.want {
			t.Errorf("%s %s: cost %d, want %d", tc.command, tc.args, got, tc.want)
		}
	}
}

// typeArgs is a web_type of n bytes.
func typeArgs(n int) json.RawMessage {
	b, _ := json.Marshal(map[string]any{"ref": "e1", "text": strings.Repeat("x", n)})
	return b
}

// TestWebBudgetLimits: an action over 2 KB, a task over 8 KB and an
// agent's tasks over 32 KB in a rolling day are refused with a reason
// the model reads, never reaching the browser; the day rolls over on
// the service's clock; each task posts one notice, which Home lists and
// an acknowledgement clears.
func TestWebBudgetLimits(t *testing.T) {
	clk := clock.NewFakeAt(time.Date(2026, 10, 19, 9, 0, 0, 0, time.UTC))
	srv, svc, _, prov := webNeedsServer(t, &fakeSubagentDriver{}, clk)
	schema, _ := structured.Parse([]byte(quoteSchema))
	register := func(id string) {
		svc.registerJob(&subagentJob{ID: id, Parent: "alice", Description: "task " + id, Kind: agentpod.SubagentKindWeb, Session: agentpod.OpenWebSession, State: subagentJobRunning, Schema: schema, Depth: 1, done: make(chan struct{})})
	}
	ctx := context.Background()
	charge := func(id string, n int) string {
		j, _ := svc.lookupJob(id)
		return svc.chargeWebAction(j, browserpod.CommandType, typeArgs(n), func() string { return "paste.example" })
	}

	// One action: 2 KB passes, a byte more is refused, through the relay
	// as the model would see it.
	register("a1")
	if r := charge("a1", webBudgetPerAction); r != "" {
		t.Fatalf("an action of exactly 2 KB: %s", r)
	}
	resp := svc.WebCommand(ctx, "alice", "a1", agentpod.WebCommandRequest{Command: browserpod.CommandType, Args: typeArgs(webBudgetPerAction + 1)})
	if !resp.IsError || !strings.Contains(resp.Text, "one action may send at most 2 KB") || !strings.Contains(resp.Text, "budget on what they send out") {
		t.Errorf("an action over 2 KB: %+v", resp)
	}
	if len(prov.started) != 0 {
		t.Fatal("a refused action reached a browser")
	}

	// One task: four 2 KB actions fill it; the fifth is refused.
	register("t1")
	for i := 0; i < 4; i++ {
		if r := charge("t1", webBudgetPerAction); r != "" {
			t.Fatalf("action %d of t1: %s", i, r)
		}
	}
	if r := charge("t1", 1); !strings.Contains(r, "past the 8 KB it may send out") {
		t.Errorf("t1 over its task budget: %q", r)
	}
	// A second breach in the same task posts no second notice.
	if r := charge("t1", 10); r == "" {
		t.Error("t1 went on sending")
	}

	// The day: 2 KB (a1) + 8 KB (t1) so far; t2 and t3 add 16 KB, t4
	// 6 KB to reach 32 KB, and t5's first byte is refused.
	for _, id := range []string{"t2", "t3"} {
		register(id)
		for i := 0; i < 4; i++ {
			if r := charge(id, webBudgetPerAction); r != "" {
				t.Fatalf("%s: %s", id, r)
			}
		}
	}
	clk.Advance(time.Hour)
	register("t4")
	for i := 0; i < 3; i++ {
		if r := charge("t4", webBudgetPerAction); r != "" {
			t.Fatalf("t4: %s", r)
		}
	}
	register("t5")
	if r := charge("t5", 1); !strings.Contains(r, "past the 32 KB a day") {
		t.Errorf("t5 over the day: %q", r)
	}
	// 24 hours after the first charges they fall out of the window,
	// and the day has room again; t4's, an hour younger, still count.
	clk.Advance(23 * time.Hour)
	if r := charge("t5", webBudgetPerAction); r != "" {
		t.Errorf("after the window rolled: %s", r)
	}

	// Notices: one per task that went over, a1, t1 and t5.
	notices := svc.webBudgetNotices()
	if len(notices) != 3 {
		t.Fatalf("notices = %+v", notices)
	}
	if n := notices[0]; n.ID != "a1" || n.Limit != webBudgetLimitAction || n.Bytes != webBudgetPerAction+1 {
		t.Errorf("first notice = %+v", n)
	}
	id := webBudgetNeedPrefix + "alice/t1"
	need, ok := needWithID(t, srv, id)
	if !ok || need.Kind != apitypes.NeedKindWebBudget || need.Title != "Analyst's web task tried to send 1 byte to paste.example" ||
		need.WebBudget == nil || need.WebBudget.Limit != apitypes.WebBudgetLimitTask || need.WebBudget.Bytes != 1 || !strings.Contains(need.BodyMD, "One task may send 8 KB.") {
		t.Errorf("web_budget row = %+v %+v", need, need.WebBudget)
	}
	if snap := srv.orgSnapshotValue(); !slices.Contains(snap.Inbox.CEOPaths, id) {
		t.Errorf("snapshot ceo paths = %v", snap.Inbox.CEOPaths)
	}
	if code, _, body := answerNeed(t, srv, "approve", id, ""); code != http.StatusBadRequest {
		t.Errorf("approve on a notice: %d %s", code, body)
	}
	if code, _, body := answerNeed(t, srv, "ack", id, ""); code != http.StatusOK {
		t.Fatalf("ack: %d %s", code, body)
	}
	if _, listed := needWithID(t, srv, id); listed {
		t.Error("the notice stayed after its acknowledgement")
	}
	if code, _, _ := answerNeed(t, srv, "ack", id, ""); code != http.StatusConflict {
		t.Errorf("a second ack: %d", code)
	}
	// An acknowledged task still posts nothing new.
	if r := charge("t1", 1); r == "" || len(svc.webBudgetNotices()) != 2 {
		t.Errorf("after the ack: refusal %q, notices %d", r, len(svc.webBudgetNotices()))
	}
}
