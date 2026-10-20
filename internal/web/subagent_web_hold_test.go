package web

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kivali-ai/kivali/internal/agentpod"
	"github.com/kivali-ai/kivali/internal/browserpod"
	"github.com/kivali-ai/kivali/internal/files"
	"github.com/kivali-ai/kivali/internal/structured"
	"github.com/kivali-ai/kivali/internal/web/apitypes"
)

// spentToday is what alice's web tasks have sent in the window.
func spentToday(svc *SubagentService) int {
	b := &svc.webBudget
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.spentLocked("alice", svc.clk().Now())
}

// TestWebStartURLCountsAgainstTheBudget: every web task's start URL,
// an upload task's too, is counted as it registers, and one that would
// take the agent past its day is refused at dispatch.
func TestWebStartURLCountsAgainstTheBudget(t *testing.T) {
	svc, parentRoot, _, _ := webService(t, &fakeSubagentDriver{})
	if err := os.WriteFile(filepath.Join(parentRoot, "artifacts", "private", "quote.pdf"), []byte("pdf"), 0o644); err != nil {
		t.Fatal(err)
	}
	endTasksAtCleanup(t, svc)
	if _, err := svc.StartBatch(context.Background(), "alice", webArgs(t, map[string]any{"start_url": "https://vendor.com/save?d=" + strings.Repeat("A", 1500), "files": []string{"/files/artifacts/private/quote.pdf"}})); err != nil {
		t.Fatal(err)
	}
	upload := len("/save") + 1 + 1500 - webURLFreePath
	if got := spentToday(svc); got != upload {
		t.Errorf("spent after an upload task's start URL = %d, want %d", got, upload)
	}
	if _, err := svc.StartBatch(context.Background(), "alice", webArgs(t, map[string]any{"start_url": "https://x.example/?q=" + strings.Repeat("a", 100)})); err != nil {
		t.Fatal(err)
	}
	spent := spentToday(svc)
	if want := upload + 1 + 1 + 100 - webURLFreePath; spent != want {
		t.Errorf("spent after a second start URL = %d, want %d", spent, want)
	}
	b := &svc.webBudget
	b.mu.Lock()
	b.chargeLocked("alice", "earlier", webBudgetPerDay-spent-100, svc.clk().Now())
	b.mu.Unlock()
	if _, err := svc.StartBatch(context.Background(), "alice", webArgs(t, map[string]any{"start_url": "https://x.example/?q=" + strings.Repeat("b", 200)})); err == nil || !strings.Contains(err.Error(), "32 KB a day") {
		t.Errorf("a start URL past the day: %v", err)
	}
}

// heldUploadTask registers u1: an approved task in the open session
// that may upload quote.pdf ("pdf" when dispatched), held to
// vendor.com.
func heldUploadTask(t *testing.T) (*SubagentService, string, *fakeBrowserProv) {
	t.Helper()
	svc, parentRoot, _, prov := webService(t, &fakeSubagentDriver{})
	quote := "/files/artifacts/private/quote.pdf"
	if err := os.WriteFile(filepath.Join(parentRoot, "artifacts", "private", "quote.pdf"), []byte("pdf"), 0o644); err != nil {
		t.Fatal(err)
	}
	snaps, err := svc.digestWebFiles("alice", []string{quote})
	if err != nil {
		t.Fatal(err)
	}
	schema, _ := structured.Parse([]byte(quoteSchema))
	svc.registerJob(&subagentJob{ID: "u1", Parent: "alice", Kind: agentpod.SubagentKindWeb, Session: agentpod.OpenWebSession, LockSite: "vendor.com", Approved: true,
		State: subagentJobRunning, Schema: schema, Files: []string{quote}, ApprovedFiles: snaps, Depth: 1, done: make(chan struct{})})
	return svc, parentRoot, prov
}

// TestApprovedWebTaskIsHeldAndCounted: an approved upload task cannot
// leave its site through core, uploads only the approved bytes and only
// onto a page of its site, has no web_eval, and is counted by the
// budget like any task; a person's own input in the window is not.
func TestApprovedWebTaskIsHeldAndCounted(t *testing.T) {
	svc, parentRoot, prov := heldUploadTask(t)
	ctx := context.Background()
	do := func(command, args string) agentpod.WebCommandResponse {
		return svc.WebCommand(ctx, "alice", "u1", agentpod.WebCommandRequest{Command: command, Args: json.RawMessage(args)})
	}
	refusals := []struct {
		name, command, args, want string
	}{
		{"eval", browserpod.CommandEval, `{"script":"1"}`, "not available to a task that uploads files"},
		{"navigate off the site", browserpod.CommandNavigate, `{"url":"https://evil.test/"}`, "https://evil.test/ is off vendor.com, the site this task is held to"},
		{"a lookalike", browserpod.CommandNavigate, `{"url":"https://notvendor.com/"}`, "off vendor.com"},
		{"a tab off the site", browserpod.CommandTabs, `{"action":"open","url":"https://evil.test/"}`, "off vendor.com"},
		{"over the budget", browserpod.CommandType, string(typeArgs(5000)), "one action may send at most 2 KB"},
		{"upload with no page", browserpod.CommandUpload, `{"ref":"e1","file":"quote.pdf"}`, "only on a page of vendor.com, and your tab is at no page yet"},
	}
	for _, tc := range refusals {
		if r := do(tc.command, tc.args); !r.IsError || !strings.Contains(r.Text, tc.want) {
			t.Errorf("%s: %+v, want a refusal naming %q", tc.name, r, tc.want)
		}
	}
	if len(prov.started) != 0 {
		t.Fatal("a refused command started a browser")
	}

	// On the site: the command carries the hold, and the page it
	// reaches is where an upload may go.
	got := make(chan agentpod.WebCommandResponse, 1)
	go func() { got <- do(browserpod.CommandNavigate, `{"url":"https://portal.vendor.com/quotes/new"}`) }()
	token := <-prov.startedC
	link := newFakeLink()
	b := svc.Browsers.claim("alice", agentpod.OpenWebSession, token, link)
	if b == nil {
		t.Fatal("claim found no waiting browser")
	}
	if err := svc.Browsers.attach(b); err != nil {
		t.Fatal(err)
	}
	svc.Browsers.onMessage(b, browserpod.Message{Type: browserpod.TypeState, State: browserpod.StateRunning}, nil)
	cmd := link.next(t, browserpod.TypeCommand)
	if cmd.msg.Name != browserpod.CommandNavigate || cmd.msg.Group != "u1" || cmd.msg.Site != "vendor.com" {
		t.Errorf("command frame = %+v", cmd.msg)
	}
	svc.Browsers.onMessage(b, browserpod.Message{Type: browserpod.TypeTab, Group: "u1", Tab: "t1", URL: "about:blank"}, nil)
	if r := svc.WebCommand(ctx, "alice", "u1", agentpod.WebCommandRequest{Command: browserpod.CommandUpload, Args: json.RawMessage(`{"ref":"e1","file":"quote.pdf"}`)}); !r.IsError || !urlInPageContent(r.Text, "about:blank") {
		t.Errorf("upload into a blank tab: %+v", r)
	}
	svc.Browsers.onMessage(b, browserpod.Message{Type: browserpod.TypeNavigated, Group: "u1", Tab: "t1", URL: "https://portal.vendor.com/quotes/new"}, nil)
	svc.Browsers.onMessage(b, browserpod.Message{Type: browserpod.TypeResult, ID: cmd.msg.ID, OK: true, Text: "Loaded"}, nil)
	if r := <-got; r.IsError {
		t.Fatalf("on-site navigate: %+v", r)
	}

	// The approved bytes go up.
	go func() { got <- do(browserpod.CommandUpload, `{"ref":"e1","file":"quote.pdf"}`) }()
	file := link.next(t, browserpod.TypeFile)
	if file.msg.FileName != "quote.pdf" || string(file.body) != "pdf" {
		t.Errorf("file frame = %+v %q", file.msg, file.body)
	}
	cmd = link.next(t, browserpod.TypeCommand)
	svc.Browsers.onMessage(b, browserpod.Message{Type: browserpod.TypeResult, ID: cmd.msg.ID, OK: true, Text: "Attached quote.pdf"}, nil)
	if r := <-got; r.IsError {
		t.Fatalf("upload: %+v", r)
	}

	// A file changed after approval is refused, same size or not.
	quote := filepath.Join(parentRoot, "artifacts", "private", "quote.pdf")
	for _, content := range []string{"PDF", "a longer pdf"} {
		if err := os.WriteFile(quote, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
		if r := do(browserpod.CommandUpload, `{"ref":"e1","file":"quote.pdf"}`); !r.IsError || !strings.Contains(r.Text, `"quote.pdf" has changed since a person approved this task`) {
			t.Errorf("changed file (%q): %+v", content, r)
		}
	}
	if err := os.WriteFile(quote, []byte("pdf"), 0o644); err != nil {
		t.Fatal(err)
	}
	// No file goes up while any tab of the task is off the site,
	// whatever the pod did, even when the last navigation was on it.
	svc.Browsers.onMessage(b, browserpod.Message{Type: browserpod.TypeTab, Group: "u1", Tab: "t2", Opener: "t1", URL: "https://evil.test/drop"}, nil)
	svc.Browsers.onMessage(b, browserpod.Message{Type: browserpod.TypeNavigated, Group: "u1", Tab: "t1", URL: "https://portal.vendor.com/done"}, nil)
	if r := do(browserpod.CommandUpload, `{"ref":"e1","file":"quote.pdf"}`); !r.IsError || !urlInPageContent(r.Text, "https://evil.test/drop") {
		t.Errorf("upload with a tab off the site: %+v", r)
	}
	svc.Browsers.onMessage(b, browserpod.Message{Type: browserpod.TypeTabClosed, Group: "u1", Tab: "t2"}, nil)

	// A person's own input in the window is not counted.
	w, err := svc.Browsers.Watch(ctx, "alice", agentpod.OpenWebSession, "u1", true)
	if err != nil {
		t.Fatal(err)
	}
	link.next(t, browserpod.TypeScreencast)
	nextEvent(t, w)
	svc.Browsers.onMessage(b, browserpod.Message{Type: browserpod.TypeTab, Group: "u1", Tab: "t1", URL: "https://vendor.com/"}, nil)
	svc.Browsers.onMessage(b, browserpod.Message{Type: browserpod.TypeFrame, Group: "u1", Tab: "t1", Bytes: 1}, strings.NewReader("x"))
	nextEvent(t, w)
	nextFrame(t, w)
	if err := svc.Browsers.Input(w, apitypes.BrowserInput{Kind: "paste", Text: strings.Repeat("p", 40<<10)}); err != nil {
		t.Fatal(err)
	}
	link.next(t, browserpod.TypeInput)
	if got := spentToday(svc); got != 0 {
		t.Errorf("spent after refused actions and a person's input = %d", got)
	}
	if n := len(svc.webBudgetNotices()); n != 1 {
		t.Errorf("notices = %d, want 1 for the refused type", n)
	}
}

// TestWebSitesCompareInPunycode: an international name is a site in its
// punycode form, as the browser names it, and core's checks agree.
func TestWebSitesCompareInPunycode(t *testing.T) {
	site, err := webSessionSite("Shop.Bücher.de")
	if err != nil || site != "xn--bcher-kva.de" {
		t.Fatalf("webSessionSite = %q, %v", site, err)
	}
	if s, err := webURLSite("https://shop.bücher.de/upload"); err != nil || s != site {
		t.Errorf("webURLSite = %q, %v", s, err)
	}
	for _, u := range []string{"https://shop.bücher.de/", "https://shop.xn--bcher-kva.de/", "https://XN--BCHER-KVA.de./"} {
		if !onWebSite(site, u) {
			t.Errorf("%s is not on %s", u, site)
		}
	}
	if onWebSite(site, "https://bucher.de/") {
		t.Error("an ASCII lookalike is on the site")
	}
}

// TestWebUploadAnswerLimits: a note over 2 KB is refused and leaves the
// task waiting; two files with one name are refused at dispatch.
func TestWebUploadAnswerLimits(t *testing.T) {
	srv, svc, _, _ := webNeedsServer(t, &fakeSubagentDriver{}, nil)
	if _, err := svc.StartBatch(context.Background(), "alice", uploadArgs(t)); err != nil {
		t.Fatal(err)
	}
	j := onlyJob(t, svc)
	if code, _, body := answerNeed(t, srv, "deny", webUploadNeedPrefix+"alice/"+j.ID, strings.Repeat("n", maxWebDenyNote+1)); code != http.StatusBadRequest || !strings.Contains(body, "over 2 KB") {
		t.Errorf("a long note: %d %s", code, body)
	}
	if j, _ := svc.lookupJob(j.ID); j.State != subagentJobAwaitingApproval {
		t.Errorf("state after a refused note = %s", j.State)
	}
	agentRoot := filepath.Join(svc.Store.Root(), "agents", "alice")
	if err := os.MkdirAll(filepath.Join(files.StorageRoot(agentRoot), "artifacts", "private", "old"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(files.StorageRoot(agentRoot), "artifacts", "private", "old", "quote.pdf"), []byte("other"), 0o644); err != nil {
		t.Fatal(err)
	}
	two := webArgs(t, map[string]any{"start_url": "https://vendor.com/", "files": []string{"/files/artifacts/private/quote.pdf", "/files/artifacts/private/old/quote.pdf"}})
	if _, err := svc.StartBatch(context.Background(), "alice", two); err == nil || !strings.Contains(err.Error(), `two files are named "quote.pdf"`) {
		t.Errorf("two files with one name: %v", err)
	}
}

// TestRestartFailsAWebTaskAwaitingApproval: at boot, a task an earlier
// run left awaiting approval is marked errored like any unfinished
// task, without its wait reason; nothing answers its Needs-you row now.
func TestRestartFailsAWebTaskAwaitingApproval(t *testing.T) {
	svc, _, _, _ := webService(t, &fakeSubagentDriver{})
	path := subagentMetaPath(svc.Store.Root(), "alice", "aaaa0001")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	body, _ := json.Marshal(map[string]any{"id": "aaaa0001", "status": subagentJobAwaitingApproval, "kind": agentpod.SubagentKindWeb, "wait_reason": "mfa", "description": "task", "session": "open"})
	if err := os.WriteFile(path, body, 0o644); err != nil {
		t.Fatal(err)
	}
	if n, _ := svc.FailOrphanedSubagents(); n != 1 {
		t.Fatalf("marked %d, want 1", n)
	}
	meta, err := readSubagentMeta(path)
	if err != nil || meta.Status != "errored" || meta.Error != subagentRestartError {
		t.Errorf("meta = %+v, %v", meta, err)
	}
	if body, _ := os.ReadFile(path); strings.Contains(string(body), "wait_reason") {
		t.Errorf("the wait reason stayed: %s", body)
	}
}

// urlInPageContent reports whether a refusal names u once, on a URL
// line between the page content lines at its end. A tab's URL is the
// page's to set (a redirect, pushState), so a refusal that puts it in
// core's own sentence lets a page write text the model reads as
// Kivali's; the upload refusals above would then carry it.
func urlInPageContent(text, u string) bool {
	tail := strings.TrimPrefix(browserpod.WithPage("x", browserpod.URLLine(u)), "x")
	return strings.HasSuffix(text, tail) && strings.Count(text, u) == 1
}
