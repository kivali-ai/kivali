package web

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"maps"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/kivali-ai/kivali/internal/auth"
	"github.com/kivali-ai/kivali/internal/clock"
	"github.com/kivali-ai/kivali/internal/provider"
	"github.com/kivali-ai/kivali/internal/seed"
	"github.com/kivali-ai/kivali/internal/store"
	"github.com/kivali-ai/kivali/internal/web/apitypes"
)

// newSetupServer is a Server on a fake clock with the given model
// client (nil for none).
func newSetupServer(t *testing.T, c provider.Client) (*Server, *clock.Fake) {
	t.Helper()
	st, err := store.New(t.TempDir())
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	fake := clock.NewFake()
	srv, err := NewServer(&Server{
		Store:    st,
		Claude:   c,
		Provider: provider.MockProvider{},
		Clock:    fake,
	})
	if err != nil {
		t.Fatalf("server: %v", err)
	}
	return srv, fake
}

// setupDo serves one request through the setup routes, behind the same
// API wrapping api.go gives every route and a signed-in owner. Writes
// carry Sec-Fetch-Site: same-origin, as a page on this origin sends.
func setupDo(t *testing.T, srv *Server, method, path, contentType string, body io.Reader) *httptest.ResponseRecorder {
	t.Helper()
	mux := http.NewServeMux()
	srv.wireAPISetupRoutes(mux)
	h := apiHeaders(requireSameOrigin(auth.DevBypass(testOwnerEmail)(mux)))
	req := httptest.NewRequest(method, path, body)
	if contentType != "" {
		req.Header.Set("content-type", contentType)
	}
	if method != http.MethodGet {
		req.Header.Set("Sec-Fetch-Site", "same-origin")
	}
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	return rr
}

func setupJSON(t *testing.T, srv *Server, method, path string, v any) *httptest.ResponseRecorder {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return setupDo(t, srv, method, path, "application/json", bytes.NewReader(b))
}

func getSetup(t *testing.T, srv *Server) apitypes.Setup {
	t.Helper()
	rr := setupDo(t, srv, http.MethodGet, "/api/v1/setup", "", nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("GET setup = %d: %s", rr.Code, rr.Body.String())
	}
	return decodeAPI[apitypes.Setup](t, rr)
}

func getProgress(t *testing.T, srv *Server) apitypes.SetupProgress {
	t.Helper()
	rr := setupDo(t, srv, http.MethodGet, "/api/v1/setup/progress", "", nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("GET progress = %d: %s", rr.Code, rr.Body.String())
	}
	return decodeAPI[apitypes.SetupProgress](t, rr)
}

// setupUpload builds a multipart body with each file under field.
func setupUpload(t *testing.T, field string, files map[string][]byte) (*bytes.Buffer, string) {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	for name, data := range files {
		fw, err := mw.CreateFormFile(field, name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := fw.Write(data); err != nil {
			t.Fatal(err)
		}
	}
	if err := mw.Close(); err != nil {
		t.Fatal(err)
	}
	return &buf, mw.FormDataContentType()
}

// seedReply is a model client whose seed call returns reply, or waits
// on gate first when gate is non-nil (announcing on entered).
func seedReply(reply string, entered chan<- struct{}, gate <-chan struct{}) *provider.MockClient {
	return &provider.MockClient{CompleteFn: func(ctx context.Context, req provider.CompleteRequest) (*provider.CompleteResponse, error) {
		if req.Purpose == "seed" && gate != nil {
			entered <- struct{}{}
			<-gate
		}
		return &provider.CompleteResponse{Content: []provider.ContentBlock{{Type: provider.ContentText, Text: reply}}}, nil
	}}
}

func waitSeed(t *testing.T, srv *Server) {
	t.Helper()
	run := srv.seedTracker().current()
	if run == nil {
		t.Fatal("no hire was started")
	}
	<-run.done
}

func stageStates(p apitypes.SetupProgress) string {
	var parts []string
	for _, s := range p.Stages {
		parts = append(parts, s.Label+"="+string(s.State))
	}
	return strings.Join(parts, ", ")
}

// ---- GET /api/v1/login ----

func TestAPILoginIsPublicAndSaysOnlyWhatSignInNeeds(t *testing.T) {
	srv := newTestServer(t)
	get := func() (*httptest.ResponseRecorder, map[string]any) {
		t.Helper()
		rr := httptest.NewRecorder()
		srv.Handler().ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/api/v1/login", nil)) // no session
		if rr.Code != http.StatusOK {
			t.Fatalf("code = %d, want 200 without a session (body %q)", rr.Code, rr.Body.String())
		}
		var raw map[string]any
		if err := json.Unmarshal(rr.Body.Bytes(), &raw); err != nil {
			t.Fatalf("decode: %v", err)
		}
		return rr, raw
	}

	rr, raw := get()
	if cc := rr.Header().Get("cache-control"); cc != "no-store" {
		t.Errorf("cache-control = %q, want no-store", cc)
	}
	if got := sortedKeys(raw); got != "auth_ready,dev_mode,org" {
		t.Errorf("top-level keys = %s, want exactly auth_ready,dev_mode,org", got)
	}
	org, _ := raw["org"].(map[string]any)
	if got := sortedKeys(org); got != "has_logo,name" {
		t.Errorf("org keys = %s, want exactly has_logo,name", got)
	}
	l := decodeAPI[apitypes.Login](t, rr)
	if l != (apitypes.Login{}) {
		t.Errorf("fresh server = %+v, want every field zero (no name, no sign-in, no dev mode)", l)
	}

	if err := srv.Store.WriteCompanyName("Plainsong"); err != nil {
		t.Fatal(err)
	}
	srv.OAuth = &auth.GoogleOAuth{ClientID: "id", ClientSecret: "secret"}
	srv.DevUser = "dev@example.com"
	rr, _ = get()
	l = decodeAPI[apitypes.Login](t, rr)
	want := apitypes.Login{Org: apitypes.MeOrg{Name: "Plainsong"}, AuthReady: true, DevMode: true}
	if l != want {
		t.Errorf("configured server = %+v, want %+v", l, want)
	}
	if strings.Contains(rr.Body.String(), "dev@example.com") || strings.Contains(rr.Body.String(), "secret") {
		t.Errorf("login data leaks the dev account or the client secret: %s", rr.Body.String())
	}

	// The public route opens nothing else under /api/.
	rr = httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/api/v1/me", nil))
	assertAPIError(t, rr, http.StatusUnauthorized)
}

func sortedKeys(m map[string]any) string {
	return strings.Join(slices.Sorted(maps.Keys(m)), ",")
}

// ---- GET /api/v1/setup and the org and files steps ----

func TestAPISetupStepFollowsWhatIsSet(t *testing.T) {
	srv, _ := newSetupServer(t, nil)

	st := getSetup(t, srv)
	if !st.Needed || st.Step != apitypes.SetupStepWelcome || len(st.Files) != 0 || st.CoS.Exists {
		t.Fatalf("fresh setup = needed %v step %q files %d cos %v, want needed welcome, no files, no cos",
			st.Needed, st.Step, len(st.Files), st.CoS.Exists)
	}
	if st.CoS.DefaultRoleMD != seed.ChiefOfStaffRole || st.CoS.DefaultHandbookMD != seed.Handbook {
		t.Error("fresh setup should offer the built-in role and handbook")
	}
	if !st.RestoreAvailable {
		t.Error("a fresh server can take a restore")
	}

	// Naming the org moves setup to the files step.
	rr := setupJSON(t, srv, http.MethodPost, "/api/v1/setup/org", apitypes.SetupOrgRequest{Name: "  Plainsong  "})
	if rr.Code != http.StatusOK {
		t.Fatalf("POST org = %d: %s", rr.Code, rr.Body.String())
	}
	st = decodeAPI[apitypes.Setup](t, rr)
	if st.Org.Name != "Plainsong" || st.Step != apitypes.SetupStepFiles {
		t.Errorf("after naming: org %q step %q, want Plainsong / files", st.Org.Name, st.Step)
	}
	if br, _ := srv.Store.ReadBranding(); br.CompanyName != "Plainsong" {
		t.Errorf("stored name = %q", br.CompanyName)
	}

	// Uploading a file moves it to the Chief of Staff step.
	body, ct := setupUpload(t, "files[]", map[string][]byte{"plan.md": []byte("# Plan\nSell fences.")})
	rr = setupDo(t, srv, http.MethodPost, "/api/v1/setup/files", ct, body)
	if rr.Code != http.StatusOK {
		t.Fatalf("POST files = %d: %s", rr.Code, rr.Body.String())
	}
	st = decodeAPI[apitypes.Setup](t, rr)
	if st.Step != apitypes.SetupStepCoS || len(st.Files) != 1 {
		t.Fatalf("after upload: step %q files %d, want cos / 1", st.Step, len(st.Files))
	}
	f := st.Files[0]
	if f.Name != "plan.md" || f.SizeBytes != int64(len("# Plan\nSell fences.")) || !f.Extracted || f.SHA == "" {
		t.Errorf("file = %+v, want plan.md with its size, extracted", f)
	}

	// Clearing the name with files present is the org step.
	rr = setupJSON(t, srv, http.MethodPost, "/api/v1/setup/org", apitypes.SetupOrgRequest{Name: ""})
	if st = decodeAPI[apitypes.Setup](t, rr); st.Step != apitypes.SetupStepOrg {
		t.Errorf("files without a name: step %q, want org", st.Step)
	}
	setupJSON(t, srv, http.MethodPost, "/api/v1/setup/org", apitypes.SetupOrgRequest{Name: "Plainsong"})

	// Removing the file goes back to the files step; removing it again
	// is a 404.
	rr = setupDo(t, srv, http.MethodDelete, "/api/v1/setup/files/"+f.SHA, "", nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("DELETE file = %d: %s", rr.Code, rr.Body.String())
	}
	if st = decodeAPI[apitypes.Setup](t, rr); st.Step != apitypes.SetupStepFiles || len(st.Files) != 0 {
		t.Errorf("after delete: step %q files %d, want files / 0", st.Step, len(st.Files))
	}
	assertAPIError(t, setupDo(t, srv, http.MethodDelete, "/api/v1/setup/files/"+f.SHA, "", nil), http.StatusNotFound)

	// A Chief of Staff means setup is done.
	if err := srv.Store.CreateAgent(store.Agent{Slug: cosSlug, Role: "Chief of Staff", ReportsTo: "ceo"}, "role"); err != nil {
		t.Fatal(err)
	}
	if st = getSetup(t, srv); st.Needed || st.Step != apitypes.SetupStepDone || !st.CoS.Exists {
		t.Errorf("with a chief of staff: needed %v step %q exists %v, want false / done / true", st.Needed, st.Step, st.CoS.Exists)
	}
}

func TestAPISetupOrgRefusals(t *testing.T) {
	srv, _ := newSetupServer(t, nil)
	e := assertAPIError(t, setupJSON(t, srv, http.MethodPost, "/api/v1/setup/org",
		apitypes.SetupOrgRequest{Name: strings.Repeat("x", maxCompanyNameRunes+1)}), http.StatusBadRequest)
	if e.Who != whoYou {
		t.Errorf("too-long name who = %q", e.Who)
	}
	rr := setupDo(t, srv, http.MethodPost, "/api/v1/setup/org", "application/x-www-form-urlencoded", strings.NewReader("name=x"))
	assertAPIError(t, rr, http.StatusUnsupportedMediaType)
	rr = setupDo(t, srv, http.MethodPost, "/api/v1/setup/files", "application/json", strings.NewReader("{}"))
	assertAPIError(t, rr, http.StatusUnsupportedMediaType)
	body, ct := setupUpload(t, "other", map[string][]byte{"a.md": []byte("a")})
	assertAPIError(t, setupDo(t, srv, http.MethodPost, "/api/v1/setup/files", ct, body), http.StatusBadRequest)
}

// squarePNG is in branding_test.go.
func TestAPISetupLogo(t *testing.T) {
	srv, _ := newSetupServer(t, nil)

	body, ct := setupUpload(t, "logo", map[string][]byte{"mark.png": []byte("not an image")})
	e := assertAPIError(t, setupDo(t, srv, http.MethodPost, "/api/v1/setup/org/logo", ct, body), http.StatusBadRequest)
	if e.Who != whoYou {
		t.Errorf("bad image who = %q", e.Who)
	}

	body, ct = setupUpload(t, "logo", map[string][]byte{"mark.png": squarePNG(t, 64)})
	rr := setupDo(t, srv, http.MethodPost, "/api/v1/setup/org/logo", ct, body)
	if rr.Code != http.StatusOK {
		t.Fatalf("POST logo = %d: %s", rr.Code, rr.Body.String())
	}
	if st := decodeAPI[apitypes.Setup](t, rr); !st.Org.HasLogo {
		t.Error("has_logo should be true after an upload")
	}
}

func TestAPISetupCredentialGuidance(t *testing.T) {
	// Guidance is a sentence for a person: no commands, environment
	// variables or paths.
	machinery := regexp.MustCompile("[`$/=]|[A-Z_]{4,}")
	signedIn := provider.CredentialStatus{Present: true, Who: "ops@example.com", Billing: "Mock Max"}
	cases := []struct {
		status provider.CredentialStatus
		client provider.Client
		want   apitypes.SetupCredential
	}{
		{provider.CredentialStatus{}, nil, apitypes.SetupCredential{}},
		{signedIn, nil, apitypes.SetupCredential{Present: true, Who: "ops@example.com", Billing: "Mock Max"}},
		{provider.CredentialStatus{}, &provider.MockClient{}, apitypes.SetupCredential{Ready: true}},
		{signedIn, &provider.MockClient{}, apitypes.SetupCredential{Ready: true, Present: true, Who: "ops@example.com", Billing: "Mock Max"}},
	}
	for _, tc := range cases {
		srv, _ := newSetupServer(t, tc.client)
		srv.Provider = provider.MockProvider{Cred: tc.status}
		got := getSetup(t, srv).Credential
		if got.Ready != tc.want.Ready || got.Present != tc.want.Present || got.Who != tc.want.Who ||
			got.Billing != tc.want.Billing || got.Provider != "mock" {
			t.Errorf("status %+v client %v: %+v, want %+v", tc.status, tc.client != nil, got, tc.want)
		}
		if got.Ready != (got.Guidance == "") {
			t.Errorf("status %+v: guidance %q should be empty exactly when ready", tc.status, got.Guidance)
		}
		if machinery.MatchString(got.Guidance) || strings.Count(got.Guidance, ".") > 1 {
			t.Errorf("status %+v: guidance %q is not one plain sentence", tc.status, got.Guidance)
		}
	}
}

// ---- POST /api/v1/setup/seed-cos, GET /api/v1/setup/progress ----

func TestAPISetupSeedCoSRunsInTheBackground(t *testing.T) {
	entered, gate := make(chan struct{}), make(chan struct{})
	srv, fake := newSetupServer(t, seedReply("# Briefing\n\nFences.", entered, gate))
	if _, err := srv.Store.AddProjectFile("plan.md", strings.NewReader("# Plan")); err != nil {
		t.Fatal(err)
	}

	if p := getProgress(t, srv); p.State != apitypes.SetupProgressStateIdle || len(p.Stages) != 0 {
		t.Fatalf("before a hire: %+v, want idle with no stages", p)
	}

	rr := setupJSON(t, srv, http.MethodPost, "/api/v1/setup/seed-cos", apitypes.SeedCoSRequest{})
	if rr.Code != http.StatusAccepted {
		t.Fatalf("POST seed-cos = %d: %s", rr.Code, rr.Body.String())
	}
	if got := strings.TrimSpace(rr.Body.String()); got != "{}" {
		t.Errorf("202 body = %s, want {}", got)
	}

	<-entered // the model call is under way
	fake.Advance(12 * time.Second)
	p := getProgress(t, srv)
	want := "Reading your files=done, Writing its first briefing=running, Hiring=queued"
	if p.State != apitypes.SetupProgressStateRunning || stageStates(p) != want || p.ElapsedS != 12 {
		t.Fatalf("mid-hire: %s [%s] %ds, want running [%s] 12s", p.State, stageStates(p), p.ElapsedS, want)
	}
	if st := getSetup(t, srv); !st.Needed || st.Step != apitypes.SetupStepCoS {
		t.Errorf("mid-hire setup: needed %v step %q, want true / cos", st.Needed, st.Step)
	}

	// A second hire while one runs is refused.
	e := assertAPIError(t, setupJSON(t, srv, http.MethodPost, "/api/v1/setup/seed-cos", apitypes.SeedCoSRequest{}), http.StatusConflict)
	if !strings.Contains(e.Error, "being hired") {
		t.Errorf("409 while running = %q", e.Error)
	}

	close(gate)
	waitSeed(t, srv)
	fake.Advance(time.Minute) // elapsed freezes when the hire ends
	p = getProgress(t, srv)
	want = "Reading your files=done, Writing its first briefing=done, Hiring=done"
	if p.State != apitypes.SetupProgressStateDone || stageStates(p) != want || p.ElapsedS != 12 || p.Error != "" {
		t.Fatalf("after: %s [%s] %ds %q, want done [%s] 12s", p.State, stageStates(p), p.ElapsedS, p.Error, want)
	}

	if mem, _ := srv.Store.ReadAgentMemory(cosSlug); !strings.Contains(mem, "Fences.") {
		t.Errorf("memory = %q", mem)
	}
	if c, err := srv.Store.ReadHandbook(); err != nil || c != seed.Handbook {
		t.Errorf("handbook: the default should be written when none is stored (err %v)", err)
	}
	if hist, _ := srv.Store.ReadChatHistory(cosSlug); len(hist) != 0 {
		t.Errorf("without handbook_from_files the chief of staff gets no first message; chat = %+v", hist)
	}
	if st := getSetup(t, srv); st.Needed || st.Step != apitypes.SetupStepDone {
		t.Errorf("after the hire: needed %v step %q", st.Needed, st.Step)
	}

	e = assertAPIError(t, setupJSON(t, srv, http.MethodPost, "/api/v1/setup/seed-cos", apitypes.SeedCoSRequest{}), http.StatusConflict)
	if !strings.Contains(e.Error, "already hired") {
		t.Errorf("409 once hired = %q", e.Error)
	}
}

func TestAPISetupSeedCoSHandbookFromFilesSendsTheFirstMessage(t *testing.T) {
	srv, _ := newSetupServer(t, seedReply("briefing", nil, nil))
	role := "# Chief of Staff\n\nEdited."
	rr := setupJSON(t, srv, http.MethodPost, "/api/v1/setup/seed-cos", apitypes.SeedCoSRequest{
		HandbookFromFiles: true,
		RoleMD:            &role,
	})
	if rr.Code != http.StatusAccepted {
		t.Fatalf("POST seed-cos = %d: %s", rr.Code, rr.Body.String())
	}
	waitSeed(t, srv)

	p := getProgress(t, srv)
	want := "Reading your files=done, Writing its first briefing=done, Hiring=done, Asking it to draft the handbook=done"
	if p.State != apitypes.SetupProgressStateDone || stageStates(p) != want {
		t.Fatalf("progress = %s [%s], want done [%s]", p.State, stageStates(p), want)
	}

	hist, err := srv.Store.ReadChatHistory(cosSlug)
	if err != nil || len(hist) != 1 {
		t.Fatalf("chief of staff chat = %+v (err %v), want the one first message", hist, err)
	}
	m := hist[0]
	if m.Role != store.RoleReceived || m.Kind != "direct_chat" {
		t.Errorf("first message role %q kind %q, want received direct_chat", m.Role, m.Kind)
	}
	if m.Content != strings.TrimSpace(seed.HandbookFromFilesMessage) {
		t.Errorf("first message = %q, want seed.HandbookFromFilesMessage", m.Content)
	}
	for _, want := range []string{"propose_handbook_update", "publish_ceo_notification", `"The company"`} {
		if !strings.Contains(m.Content, want) {
			t.Errorf("first message never says %s", want)
		}
	}
	// The stored handbook is the default, whose company section the
	// Chief of Staff is asked to replace.
	if c, _ := srv.Store.ReadHandbook(); c != seed.Handbook {
		t.Error("handbook_from_files should store the default handbook as the starting point")
	}
	if got, _ := srv.Store.ReadRole(cosSlug); got != strings.TrimSpace(role) {
		t.Errorf("role = %q, want the edited one", got)
	}
	if st := getSetup(t, srv); st.RestoreAvailable {
		t.Error("a chief of staff with a chat is no longer a fresh server")
	}
}

func TestAPISetupSeedCoSEditedHandbookAndRefusals(t *testing.T) {
	srv, _ := newSetupServer(t, nil)
	edited := "# Our rules"
	// No model connected.
	assertAPIError(t, setupJSON(t, srv, http.MethodPost, "/api/v1/setup/seed-cos", apitypes.SeedCoSRequest{}), http.StatusServiceUnavailable)

	srv, _ = newSetupServer(t, seedReply("briefing", nil, nil))
	blank := "  "
	assertAPIError(t, setupJSON(t, srv, http.MethodPost, "/api/v1/setup/seed-cos", apitypes.SeedCoSRequest{HandbookMD: &blank}), http.StatusBadRequest)
	assertAPIError(t, setupJSON(t, srv, http.MethodPost, "/api/v1/setup/seed-cos", apitypes.SeedCoSRequest{RoleMD: &blank}), http.StatusBadRequest)
	assertAPIError(t, setupDo(t, srv, http.MethodPost, "/api/v1/setup/seed-cos", "application/json", strings.NewReader(`{"surprise":true}`)), http.StatusBadRequest)
	if srv.seedTracker().current() != nil {
		t.Fatal("a refused request must not start a hire")
	}

	rr := setupJSON(t, srv, http.MethodPost, "/api/v1/setup/seed-cos", apitypes.SeedCoSRequest{HandbookMD: &edited})
	if rr.Code != http.StatusAccepted {
		t.Fatalf("POST = %d: %s", rr.Code, rr.Body.String())
	}
	waitSeed(t, srv)
	if c, _ := srv.Store.ReadHandbook(); c != edited {
		t.Errorf("handbook = %q, want the edited one", c)
	}
}

func TestAPISetupSeedCoSFailureIsReportedAndRetryable(t *testing.T) {
	var fail = true
	mock := &provider.MockClient{CompleteFn: func(ctx context.Context, req provider.CompleteRequest) (*provider.CompleteResponse, error) {
		if fail {
			return nil, errors.New("overloaded")
		}
		return &provider.CompleteResponse{Content: []provider.ContentBlock{{Type: provider.ContentText, Text: "briefing"}}}, nil
	}}
	srv, _ := newSetupServer(t, mock)
	if rr := setupJSON(t, srv, http.MethodPost, "/api/v1/setup/seed-cos", apitypes.SeedCoSRequest{}); rr.Code != http.StatusAccepted {
		t.Fatalf("POST = %d", rr.Code)
	}
	waitSeed(t, srv)
	p := getProgress(t, srv)
	want := "Reading your files=done, Writing its first briefing=failed, Hiring=queued"
	if p.State != apitypes.SetupProgressStateFailed || stageStates(p) != want || p.Error == "" || p.Who == "" {
		t.Fatalf("failed hire: %s [%s] %q / %q, want failed [%s] with error and who", p.State, stageStates(p), p.Error, p.Who, want)
	}
	if strings.Contains(p.Error, "overloaded") {
		t.Errorf("the page should say what happened in words, not the raw error: %q", p.Error)
	}
	if st := getSetup(t, srv); st.Step != apitypes.SetupStepCoS || !st.Needed {
		t.Errorf("after a failed hire setup is on %q (needed %v), want cos", st.Step, st.Needed)
	}
	if _, err := srv.Store.ReadHandbook(); !errors.Is(err, store.ErrNotFound) {
		t.Error("a hire that failed before hiring must not write the handbook")
	}

	fail = false // safe: the failed run has ended, and the retry starts after this write
	if rr := setupJSON(t, srv, http.MethodPost, "/api/v1/setup/seed-cos", apitypes.SeedCoSRequest{}); rr.Code != http.StatusAccepted {
		t.Fatalf("retry = %d: %s", rr.Code, rr.Body.String())
	}
	waitSeed(t, srv)
	if p := getProgress(t, srv); p.State != apitypes.SetupProgressStateDone {
		t.Errorf("retry: %s", p.State)
	}
}

// A seed call that hangs is cancelled at the hire's deadline, on the
// server's clock, and the run ends as failed at the briefing stage
// with the deadline in words; a retry is accepted.
func TestAPISetupSeedCoSHungModelCallTimesOut(t *testing.T) {
	entered := make(chan struct{}, 1)
	var hang = true
	mock := &provider.MockClient{CompleteFn: func(ctx context.Context, req provider.CompleteRequest) (*provider.CompleteResponse, error) {
		if hang {
			entered <- struct{}{}
			<-ctx.Done()
			return nil, ctx.Err()
		}
		return &provider.CompleteResponse{Content: []provider.ContentBlock{{Type: provider.ContentText, Text: "briefing"}}}, nil
	}}
	srv, fake := newSetupServer(t, mock)
	srv.SeedTimeout = 2 * time.Minute
	if rr := setupJSON(t, srv, http.MethodPost, "/api/v1/setup/seed-cos", apitypes.SeedCoSRequest{}); rr.Code != http.StatusAccepted {
		t.Fatalf("POST = %d: %s", rr.Code, rr.Body.String())
	}
	<-entered
	fake.Advance(2*time.Minute - time.Second)
	if p := getProgress(t, srv); p.State != apitypes.SetupProgressStateRunning {
		t.Fatalf("before the deadline: %s, want running", p.State)
	}
	fake.Advance(time.Second)
	waitSeed(t, srv)

	p := getProgress(t, srv)
	want := "Reading your files=done, Writing its first briefing=failed, Hiring=queued"
	if p.State != apitypes.SetupProgressStateFailed || stageStates(p) != want {
		t.Fatalf("timed-out hire: %s [%s], want failed [%s]", p.State, stageStates(p), want)
	}
	if !strings.Contains(p.Error, "did not answer within 2 minutes") || !strings.Contains(p.Who, "trying again") {
		t.Errorf("timed-out hire says %q / %q", p.Error, p.Who)
	}
	if srv.cosExists() {
		t.Error("a timed-out hire must not create the chief of staff")
	}

	hang = false // safe: the timed-out run has ended
	if rr := setupJSON(t, srv, http.MethodPost, "/api/v1/setup/seed-cos", apitypes.SeedCoSRequest{}); rr.Code != http.StatusAccepted {
		t.Fatalf("retry = %d: %s", rr.Code, rr.Body.String())
	}
	waitSeed(t, srv)
	if p := getProgress(t, srv); p.State != apitypes.SetupProgressStateDone {
		t.Errorf("retry: %s", p.State)
	}
}

func TestSeedTimeoutWords(t *testing.T) {
	for d, want := range map[time.Duration]string{
		10 * time.Minute: "10 minutes",
		time.Minute:      "1 minute",
		45 * time.Second: "45 seconds",
		90 * time.Second: "90 seconds",
	} {
		if got := (&seedTimeoutError{After: d}).words(); got != want {
			t.Errorf("words(%v) = %q, want %q", d, got, want)
		}
	}
}

// A panic inside the hire ends the run as failed instead of leaving the
// tracker running, so a retry is accepted rather than refused with 409.
func TestAPISetupSeedCoSPanicIsReportedAsFailed(t *testing.T) {
	var panicking = true
	mock := &provider.MockClient{CompleteFn: func(ctx context.Context, req provider.CompleteRequest) (*provider.CompleteResponse, error) {
		if panicking {
			panic("boom")
		}
		return &provider.CompleteResponse{Content: []provider.ContentBlock{{Type: provider.ContentText, Text: "briefing"}}}, nil
	}}
	srv, _ := newSetupServer(t, mock)
	if rr := setupJSON(t, srv, http.MethodPost, "/api/v1/setup/seed-cos", apitypes.SeedCoSRequest{}); rr.Code != http.StatusAccepted {
		t.Fatalf("POST = %d", rr.Code)
	}
	waitSeed(t, srv)
	p := getProgress(t, srv)
	want := "Reading your files=done, Writing its first briefing=failed, Hiring=queued"
	if p.State != apitypes.SetupProgressStateFailed || stageStates(p) != want {
		t.Fatalf("panicked hire: %s [%s], want failed [%s]", p.State, stageStates(p), want)
	}
	if strings.Contains(p.Error, "boom") {
		t.Errorf("the page should not carry the panic value: %q", p.Error)
	}

	panicking = false // safe: the panicked run has ended
	if rr := setupJSON(t, srv, http.MethodPost, "/api/v1/setup/seed-cos", apitypes.SeedCoSRequest{}); rr.Code != http.StatusAccepted {
		t.Fatalf("retry after a panic = %d: %s", rr.Code, rr.Body.String())
	}
	waitSeed(t, srv)
	if p := getProgress(t, srv); p.State != apitypes.SetupProgressStateDone {
		t.Errorf("retry: %s", p.State)
	}
}

// After a restart there is no run in memory; an existing Chief of
// Staff still reads as done.
func TestAPISetupProgressWithoutARun(t *testing.T) {
	srv, _ := newSetupServer(t, nil)
	if err := srv.Store.CreateAgent(store.Agent{Slug: cosSlug, Role: "Chief of Staff", ReportsTo: "ceo"}, "role"); err != nil {
		t.Fatal(err)
	}
	if p := getProgress(t, srv); p.State != apitypes.SetupProgressStateDone || len(p.Stages) != 0 {
		t.Errorf("progress = %+v, want done with no stages", p)
	}
}
