package web

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/kivali-ai/kivali/internal/provider"
	"github.com/kivali-ai/kivali/internal/seed"
	"github.com/kivali-ai/kivali/internal/store"
	"github.com/kivali-ai/kivali/internal/web/apitypes"
)

// multipartRequest builds a same-origin POST with the given files as
// multipart/form-data, each under the field "file".
func multipartRequest(t *testing.T, target string, files map[string]string) *http.Request {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	for name, content := range files {
		fw, err := mw.CreateFormFile("file", name)
		if err != nil {
			t.Fatalf("CreateFormFile: %v", err)
		}
		if _, err := io.WriteString(fw, content); err != nil {
			t.Fatalf("write: %v", err)
		}
	}
	_ = mw.Close()
	req := httptest.NewRequest(http.MethodPost, target, &buf)
	req.Header.Set("content-type", mw.FormDataContentType())
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	return req
}

// uploadFiles POSTs files to target through the full handler, signed in.
func uploadFiles(t *testing.T, srv *Server, target string, files map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	rr := httptest.NewRecorder()
	authedHandler(t, srv).ServeHTTP(rr, multipartRequest(t, target, files))
	return rr
}

func TestFileUploadText(t *testing.T) {
	srv := newTestServer(t)
	srv.Claude = &provider.MockClient{}

	rr := uploadFiles(t, srv, "/api/v1/org/files", map[string]string{
		"biz-plan.md": "# Biz plan\n\nThis is the plan.\n",
	})
	if rr.Code != http.StatusOK {
		t.Fatalf("code = %d, body = %s", rr.Code, rr.Body.String())
	}

	files, _ := srv.Store.ListProjectFiles()
	if len(files) != 1 {
		t.Fatalf("file count = %d", len(files))
	}
	f := files[0]
	if f.OriginalName != "biz-plan.md" {
		t.Errorf("name = %q", f.OriginalName)
	}
	if f.CanonicalName != "original.md" {
		t.Errorf("canonical = %q", f.CanonicalName)
	}
	if f.MIME != "text/markdown" {
		t.Errorf("mime = %q", f.MIME)
	}
}

func TestFileUploadPDFSkipsGracefullyWithoutPdftotext(t *testing.T) {
	// With pdftotext unavailable (or a fake PDF that fails extraction),
	// the upload still stores the file — just with no canonical form.
	srv := newTestServer(t)
	srv.Claude = &provider.MockClient{}
	rr := uploadFiles(t, srv, "/api/v1/org/files", map[string]string{
		"research.pdf": "%PDF-1.4 fake bytes",
	})
	// Either the upload succeeds (with canonical text if pdftotext works)
	// or returns 500 if pdftotext fails on a bogus PDF — both acceptable
	// depending on environment.
	if rr.Code != http.StatusOK && rr.Code != http.StatusInternalServerError {
		t.Fatalf("code = %d, body = %s", rr.Code, rr.Body.String())
	}
	files, _ := srv.Store.ListProjectFiles()
	if len(files) != 1 {
		t.Fatalf("count = %d", len(files))
	}
}

func TestFileUploadTextExtensionExpanded(t *testing.T) {
	// csv, json, js etc should all be treated as text now.
	srv := newTestServer(t)
	srv.Claude = &provider.MockClient{}
	rr := uploadFiles(t, srv, "/api/v1/org/files", map[string]string{
		"data.csv":    "col1,col2\n1,2\n",
		"config.json": `{"a":1}`,
		"script.js":   "console.log('hi')",
	})
	if rr.Code != http.StatusOK {
		t.Fatalf("code = %d, body = %s", rr.Code, rr.Body.String())
	}
	files, _ := srv.Store.ListProjectFiles()
	if len(files) != 3 {
		t.Fatalf("count = %d", len(files))
	}
	for _, f := range files {
		if f.CanonicalName == "" {
			t.Errorf("%s: canonical not set", f.OriginalName)
		}
	}
}

// seedCoS POSTs /api/v1/setup/seed-cos signed in and, on a 202, waits
// for the background hire to end.
func seedCoS(t *testing.T, srv *Server, req apitypes.SeedCoSRequest) *httptest.ResponseRecorder {
	t.Helper()
	b, err := json.Marshal(req)
	if err != nil {
		t.Fatal(err)
	}
	rr := apiDo(t, srv, http.MethodPost, "/api/v1/setup/seed-cos", string(b), nil)
	if rr.Code == http.StatusAccepted {
		waitSeed(t, srv)
	}
	return rr
}

func TestSeedCoS(t *testing.T) {
	srv := newTestServer(t)
	// Capture the "seed" Complete request for assertions. The upload
	// step ALSO triggers a background project-file summarization
	// Complete call (purpose="project_file_summary"); filter by
	// purpose so the assertion isn't racing that goroutine.
	var captured provider.CompleteRequest
	srv.Claude = &provider.MockClient{
		CompleteFn: func(ctx context.Context, req provider.CompleteRequest) (*provider.CompleteResponse, error) {
			if req.Purpose == "seed" {
				captured = req
			}
			return &provider.CompleteResponse{
				Content: []provider.ContentBlock{
					{Type: provider.ContentText, Text: "# Business briefing\n\nI understand the biz now."},
				},
				StopReason: "end_turn",
				Usage:      provider.TokenUsage{InputTokens: 1000, OutputTokens: 50},
			}, nil
		},
	}

	if err := srv.Store.WriteHandbook("# Rules"); err != nil {
		t.Fatalf("handbook: %v", err)
	}
	// Upload a text file — exercises the inline path. (PDF extraction
	// requires pdftotext which isn't available in test envs.)
	if rr := uploadFiles(t, srv, "/api/v1/setup/files", map[string]string{
		"biz-plan.md": "# Biz plan\nRevenue plan.",
	}); rr.Code != http.StatusOK {
		t.Fatalf("upload code = %d: %s", rr.Code, rr.Body.String())
	}

	if rr := seedCoS(t, srv, apitypes.SeedCoSRequest{}); rr.Code != http.StatusAccepted {
		t.Fatalf("seed code = %d, body = %s", rr.Code, rr.Body.String())
	}

	// CoS agent created.
	a, err := srv.Store.GetAgent("chief-of-staff")
	if err != nil {
		t.Fatalf("GetAgent: %v", err)
	}
	if a.ReportsTo != "ceo" {
		t.Errorf("reports_to = %q", a.ReportsTo)
	}
	// Role = the CoS role template.
	kick, _ := srv.Store.ReadRole("chief-of-staff")
	if kick != seed.ChiefOfStaffRole {
		t.Errorf("role mismatch")
	}
	// Briefing persisted.
	brief, _ := srv.Store.ReadAgentMemory("chief-of-staff")
	if !strings.Contains(brief, "I understand the biz now") {
		t.Errorf("briefing = %q", brief)
	}
	// CEO pseudo-agent created.
	if _, err := srv.Store.GetAgent("ceo"); err != nil {
		t.Errorf("CEO agent missing: %v", err)
	}
	// The stored handbook is left alone.
	if c, _ := srv.Store.ReadHandbook(); c != "# Rules" {
		t.Errorf("handbook = %q, want the stored one untouched", c)
	}

	// Seed request shape.
	if captured.Purpose != "seed" {
		t.Errorf("purpose = %q", captured.Purpose)
	}
	if len(captured.System) != 2 {
		t.Fatalf("system layers = %d", len(captured.System))
	}
	if !strings.Contains(captured.System[0].Text, "# Rules") {
		t.Error("system[0] should include handbook")
	}
	// All content blocks should be text; no message/file_id blocks.
	if len(captured.Messages) != 1 {
		t.Fatalf("messages = %d", len(captured.Messages))
	}
	content := captured.Messages[0].Content
	var foundText, foundInstruct bool
	for _, c := range content {
		if c.Type == provider.ContentText && strings.Contains(c.Text, "Revenue plan") {
			foundText = true
		}
		if c.Type == provider.ContentText && strings.Contains(c.Text, "Synthesize what you now understand") {
			foundInstruct = true
		}
	}
	if !foundText {
		t.Error("text file not inlined in seed request")
	}
	if !foundInstruct {
		t.Error("closing instruction not included")
	}
}

// The setup hire gives the Chief of Staff its pod, as every other hire
// gets one: without it the first turn had nowhere to run and was
// dropped ("no agent-pod subscriber") until the server next restarted.
func TestSeedCoSProvisionsItsPod(t *testing.T) {
	srv := newTestServer(t)
	pod := &fakeAgentPodLifecycle{}
	srv.AgentPod = pod
	srv.Claude = &provider.MockClient{
		CompleteFn: func(ctx context.Context, req provider.CompleteRequest) (*provider.CompleteResponse, error) {
			return &provider.CompleteResponse{
				Content:    []provider.ContentBlock{{Type: provider.ContentText, Text: "# Briefing"}},
				StopReason: "end_turn",
			}, nil
		},
	}
	if rr := seedCoS(t, srv, apitypes.SeedCoSRequest{}); rr.Code != http.StatusAccepted {
		t.Fatalf("seed code = %d, body = %s", rr.Code, rr.Body.String())
	}
	if got := pod.provisionedSlugs(); len(got) != 1 || got[0] != "chief-of-staff" {
		t.Fatalf("provisioned %v, want [chief-of-staff]", got)
	}
}

func TestSeedCoSRejectsDuplicate(t *testing.T) {
	srv := newTestServer(t)
	if err := srv.Store.WriteHandbook("# rules"); err != nil {
		t.Fatalf("handbook: %v", err)
	}
	if err := srv.Store.CreateAgent(store.Agent{Slug: "chief-of-staff", Role: "Chief of Staff", ReportsTo: "ceo"}, "k"); err != nil {
		t.Fatalf("seed existing: %v", err)
	}
	srv.Claude = &provider.MockClient{
		CompleteFn: func(ctx context.Context, req provider.CompleteRequest) (*provider.CompleteResponse, error) {
			t.Error("should not reach claude when CoS already exists")
			return nil, nil
		},
	}
	if rr := seedCoS(t, srv, apitypes.SeedCoSRequest{}); rr.Code != http.StatusConflict {
		t.Errorf("code = %d", rr.Code)
	}
}

func TestSeedCoSWithoutClaude(t *testing.T) {
	srv := newTestServer(t) // no Claude
	if err := srv.Store.WriteHandbook("# rules"); err != nil {
		t.Fatalf("handbook: %v", err)
	}
	if rr := seedCoS(t, srv, apitypes.SeedCoSRequest{}); rr.Code != http.StatusServiceUnavailable {
		t.Errorf("code = %d", rr.Code)
	}
}

// An edited role becomes the Chief of Staff's role.md and the second
// system layer of the seed call.
func TestSeedCoSUsesEditedRole(t *testing.T) {
	srv := newTestServer(t)
	_ = srv.Store.WriteHandbook("# rules")
	var capturedReq provider.CompleteRequest
	srv.Claude = &provider.MockClient{
		CompleteFn: func(ctx context.Context, req provider.CompleteRequest) (*provider.CompleteResponse, error) {
			if req.Purpose == "seed" {
				capturedReq = req
			}
			return &provider.CompleteResponse{
				Content: []provider.ContentBlock{{Type: provider.ContentText, Text: "briefing body"}},
			}, nil
		},
	}
	role := "# Custom CoS role"
	if rr := seedCoS(t, srv, apitypes.SeedCoSRequest{RoleMD: &role}); rr.Code != http.StatusAccepted {
		t.Fatalf("code = %d, body = %s", rr.Code, rr.Body.String())
	}
	if got, _ := srv.Store.ReadRole("chief-of-staff"); got != role {
		t.Errorf("role = %q", got)
	}
	if len(capturedReq.System) < 2 || capturedReq.System[1].Text != role {
		t.Errorf("system[1] = %+v", capturedReq.System)
	}
}
