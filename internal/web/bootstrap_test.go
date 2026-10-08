package web

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kivali-ai/kivali/internal/provider"
	"github.com/kivali-ai/kivali/internal/web/apitypes"
)

// bootstrap_test.go is the functional test for first-run setup as the
// web app drives it: name the org, upload project files, hire the
// Chief of Staff (POST /api/v1/setup/org, /setup/files, /setup/seed-cos).
// Every step goes through Server.Handler, session middleware included,
// so the real router and handlers are on the hot path. The step
// machine itself is pinned by TestAPISetupStepFollowsWhatIsSet; this
// test is about the SEQUENCE and what it leaves in /data.
//
// Runs with no external dependencies:
//   - filesystem: t.TempDir() via newTestServer
//   - Claude: provider.MockClient with a scripted CompleteFn

// TestBootstrapFlow exercises setup end to end on a fresh install:
//  1. GET /api/v1/setup says setup is needed
//  2. POST /api/v1/setup/org names the org
//  3. POST /api/v1/setup/files uploads a project file
//  4. POST /api/v1/setup/seed-cos with an edited handbook hires the
//     Chief of Staff: memory written, .seed/prompt.md + .seed/response.md
//     written, chat.jsonl untouched, the seed call carrying the
//     handbook, the role, the file and the closing instruction
//  5. GET /api/v1/setup says setup is done
func TestBootstrapFlow(t *testing.T) {
	srv := newTestServer(t)

	const cosBriefing = "# CoS briefing\n\nI am the Chief of Staff. I read everything.\n"
	srv.Claude = &provider.MockClient{
		CompleteFn: func(ctx context.Context, req provider.CompleteRequest) (*provider.CompleteResponse, error) {
			return &provider.CompleteResponse{
				Content: []provider.ContentBlock{{Type: provider.ContentText, Text: cosBriefing}},
			}, nil
		},
	}

	// 1. Fresh install needs setup.
	if st := decodeAPI[apitypes.Setup](t, apiDo(t, srv, http.MethodGet, "/api/v1/setup", "", nil)); !st.Needed {
		t.Fatalf("fresh install: setup needed = false")
	}

	// 2. Name the org.
	if rr := apiDo(t, srv, http.MethodPost, "/api/v1/setup/org", `{"name":"Acme Corp"}`, nil); rr.Code != http.StatusOK {
		t.Fatalf("POST setup/org: code=%d body=%s", rr.Code, rr.Body.String())
	}
	if br, err := srv.Store.ReadBranding(); err != nil || br.CompanyName != "Acme Corp" {
		t.Errorf("branding = %+v (err=%v), want CompanyName=Acme Corp", br, err)
	}

	// 3. Upload a project file.
	if rr := uploadFiles(t, srv, "/api/v1/setup/files", map[string]string{
		"company-overview.md": "# Acme Corp\n\nWe make scheduling software. Test-specific marker: PLUGH.\n",
	}); rr.Code != http.StatusOK {
		t.Fatalf("POST setup/files: code=%d body=%s", rr.Code, rr.Body.String())
	}
	files, _ := srv.Store.ListProjectFiles()
	if len(files) != 1 || files[0].OriginalName != "company-overview.md" {
		t.Fatalf("file list = %+v", files)
	}

	// 4. Hire the Chief of Staff with an edited handbook.
	const myHandbook = "# My handbook\n\nAgents follow these rules. Test-specific marker: XYZZY.\n"
	handbook := myHandbook
	if rr := seedCoS(t, srv, apitypes.SeedCoSRequest{HandbookMD: &handbook}); rr.Code != http.StatusAccepted {
		t.Fatalf("POST setup/seed-cos: code=%d body=%s", rr.Code, rr.Body.String())
	}
	if got, _ := srv.Store.ReadHandbook(); got != myHandbook {
		t.Errorf("handbook persisted = %q, want %q", got, myHandbook)
	}
	cos, err := srv.Store.GetAgent("chief-of-staff")
	if err != nil {
		t.Fatalf("chief-of-staff not provisioned: %v", err)
	}
	if cos.Role != "Chief of Staff" {
		t.Errorf("cos role = %q", cos.Role)
	}
	gotMemory, err := os.ReadFile(filepath.Join(srv.Store.Root(), "agents", "chief-of-staff", "agent_memory.md"))
	if err != nil {
		t.Fatalf("read agent_memory.md: %v", err)
	}
	if string(gotMemory) != cosBriefing {
		t.Errorf("agent_memory.md = %q, want %q", string(gotMemory), cosBriefing)
	}

	// Seed trace files exist, name the handbook AND the project
	// file (both were in the request), and the response file contains
	// the mocked briefing. The dot-prefix subdir is deliberate — hides
	// from casual `ls` but is a cat/grep away for debugging.
	seedDir := filepath.Join(srv.Store.Root(), "agents", "chief-of-staff", ".seed")
	gotPrompt, err := os.ReadFile(filepath.Join(seedDir, "prompt.md"))
	if err != nil {
		t.Fatalf("read .seed/prompt.md: %v", err)
	}
	for _, want := range []string{"CoS seed — prompt", "XYZZY", "PLUGH", "Chief of Staff"} {
		if !strings.Contains(string(gotPrompt), want) {
			t.Errorf(".seed/prompt.md missing %q", want)
		}
	}
	gotResp, err := os.ReadFile(filepath.Join(seedDir, "response.md"))
	if err != nil {
		t.Fatalf("read .seed/response.md: %v", err)
	}
	if !strings.Contains(string(gotResp), cosBriefing) {
		t.Errorf(".seed/response.md missing briefing")
	}

	// chat.jsonl is untouched — the seed prompt and response never show
	// in the chat. If CreateAgent ever writes a seed entry, update this
	// assertion rather than weakening it.
	if history, err := srv.Store.ReadChatHistory("chief-of-staff"); err == nil && len(history) > 0 {
		t.Errorf("chat.jsonl has %d entries on a freshly seeded CoS; seed prompt/response must not render", len(history))
	}

	// The seed call's shape: 2 system blocks (handbook + role), and
	// user content that inlines the uploaded file + the closing
	// instruction. The file upload fires a background summarization
	// call too, so filter by Purpose rather than asserting call count.
	mock := srv.Claude.(*provider.MockClient)
	var call *provider.CompleteRequest
	for i := range mock.Calls {
		if mock.Calls[i].Purpose == "seed" {
			call = &mock.Calls[i]
			break
		}
	}
	if call == nil {
		t.Fatalf("no seed call captured; got %d call(s) with purposes %v", len(mock.Calls), callPurposes(mock.Calls))
	}
	if len(call.System) < 2 {
		t.Fatalf("system blocks = %d, want ≥ 2", len(call.System))
	}
	if !strings.Contains(call.System[0].Text, "XYZZY") {
		t.Errorf("system[0] missing handbook marker")
	}
	if !strings.Contains(call.System[1].Text, "Chief of Staff") {
		t.Errorf("system[1] missing role template")
	}
	var joined strings.Builder
	for _, m := range call.Messages {
		for _, c := range m.Content {
			if c.Type == provider.ContentText {
				joined.WriteString(c.Text)
			}
		}
	}
	for _, want := range []string{"PLUGH", defaultSeedInstruction} {
		if !strings.Contains(joined.String(), want) {
			t.Errorf("claude request user content missing %q", want)
		}
	}

	// 5. Setup is done.
	if st := decodeAPI[apitypes.Setup](t, apiDo(t, srv, http.MethodGet, "/api/v1/setup", "", nil)); st.Needed || st.Step != apitypes.SetupStepDone {
		t.Errorf("after the hire: needed %v step %q, want false / done", st.Needed, st.Step)
	}
}

// TestBootstrapSeedPostRefusesReentry covers the server-side safety
// net that prevents a replayed request from clobbering an
// already-seeded CoS.
func TestBootstrapSeedPostRefusesReentry(t *testing.T) {
	srv := newTestServer(t)
	srv.Claude = &provider.MockClient{
		CompleteFn: func(ctx context.Context, req provider.CompleteRequest) (*provider.CompleteResponse, error) {
			return &provider.CompleteResponse{Content: []provider.ContentBlock{{Type: provider.ContentText, Text: "ok"}}}, nil
		},
	}
	if err := srv.Store.WriteHandbook("# rules"); err != nil {
		t.Fatal(err)
	}
	if rr := seedCoS(t, srv, apitypes.SeedCoSRequest{}); rr.Code != http.StatusAccepted {
		t.Fatalf("first seed: code=%d body=%s", rr.Code, rr.Body.String())
	}
	if rr := seedCoS(t, srv, apitypes.SeedCoSRequest{}); rr.Code != http.StatusConflict {
		t.Errorf("second seed: code=%d, want 409", rr.Code)
	}
}

// callPurposes extracts the Purpose field from each captured call,
// used in failure messages to explain what the mock actually saw.
func callPurposes(calls []provider.CompleteRequest) []string {
	out := make([]string, 0, len(calls))
	for _, c := range calls {
		out = append(out, c.Purpose)
	}
	return out
}
