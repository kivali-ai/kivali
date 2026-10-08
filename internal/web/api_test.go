package web

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/kivali-ai/kivali/internal/provider"
	"github.com/kivali-ai/kivali/internal/store"
	"github.com/kivali-ai/kivali/internal/web/apitypes"
)

// apiDo serves one request through the real handler stack with the
// test session attached. Non-GET requests carry Sec-Fetch-Site:
// same-origin unless the caller set their own headers, which is what
// a browser on this origin sends.
func apiDo(t *testing.T, srv *Server, method, path, body string, hdr map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	var req *http.Request
	if body == "" {
		req = httptest.NewRequest(method, path, nil)
	} else {
		req = httptest.NewRequest(method, path, strings.NewReader(body))
		req.Header.Set("content-type", "application/json")
	}
	if hdr == nil && method != http.MethodGet {
		hdr = map[string]string{"Sec-Fetch-Site": "same-origin"}
	}
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	rr := httptest.NewRecorder()
	authedHandler(t, srv).ServeHTTP(rr, req)
	return rr
}

// decodeAPI decodes a JSON response into v and fails on a non-JSON
// content type or a nil slice anywhere in it.
func decodeAPI[T any](t *testing.T, rr *httptest.ResponseRecorder) T {
	t.Helper()
	if ct := rr.Header().Get("content-type"); ct != "application/json" {
		t.Fatalf("content-type = %q, want application/json (body %q)", ct, rr.Body.String())
	}
	var v T
	if err := json.Unmarshal(rr.Body.Bytes(), &v); err != nil {
		t.Fatalf("decode %T: %v (body %s)", v, err, rr.Body.String())
	}
	apitypes.NoNilSlices(t, v)
	return v
}

func assertAPIError(t *testing.T, rr *httptest.ResponseRecorder, status int) apitypes.ErrorBody {
	t.Helper()
	if rr.Code != status {
		t.Fatalf("code = %d, want %d (body %q)", rr.Code, status, rr.Body.String())
	}
	e := decodeAPI[apitypes.ErrorBody](t, rr)
	if e.Error == "" || e.Who == "" {
		t.Errorf("error body = %+v, want both error and who", e)
	}
	return e
}

func TestAPIUnauthenticatedIs401JSON(t *testing.T) {
	srv := newTestServer(t)
	for _, method := range []string{http.MethodGet, http.MethodPost} {
		req := httptest.NewRequest(method, "/api/v1/me", nil)
		rr := httptest.NewRecorder()
		srv.Handler().ServeHTTP(rr, req) // no session cookie
		e := assertAPIError(t, rr, http.StatusUnauthorized)
		if e.Error != "unauthenticated" || e.Who != "sign in again" {
			t.Errorf("%s: body = %+v", method, e)
		}
	}
}

// The page routes that take a plain form refuse another site's post the
// way the API does: a form on another loopback port is the same site to
// the browser, and would carry the session cookie.
func TestPageFormPostsRefuseOtherSites(t *testing.T) {
	srv := newTestServer(t)
	for _, path := range []string{"/agents/alice/messages", "/agents/alice/stop"} {
		for _, site := range []string{"same-site", "cross-site"} {
			req := httptest.NewRequest(http.MethodPost, path, strings.NewReader("text=hi"))
			req.Header.Set("content-type", "application/x-www-form-urlencoded")
			req.Header.Set("Sec-Fetch-Site", site)
			rr := httptest.NewRecorder()
			authedHandler(t, srv).ServeHTTP(rr, req)
			if rr.Code != http.StatusForbidden {
				t.Errorf("%s from %s: code = %d, want 403", path, site, rr.Code)
			}
		}
	}
}

func TestAPISameOriginGate(t *testing.T) {
	srv := newTestServer(t)
	body := `{"value":"2m"}`
	cases := []struct {
		name string
		hdr  map[string]string
		want int
	}{
		{"cross-site fetch", map[string]string{"Sec-Fetch-Site": "cross-site"}, http.StatusForbidden},
		{"same-site but other origin", map[string]string{"Sec-Fetch-Site": "same-site"}, http.StatusForbidden},
		{"no headers at all", map[string]string{}, http.StatusForbidden},
		{"foreign origin", map[string]string{"Origin": "https://evil.example"}, http.StatusForbidden},
		{"origin with wrong scheme", map[string]string{"Origin": "https://example.com"}, http.StatusForbidden},
		{"same-origin fetch", map[string]string{"Sec-Fetch-Site": "same-origin"}, http.StatusOK},
		// A user-initiated navigation cannot carry a JSON body; our own
		// page's fetch() says same-origin. So none is never a write.
		{"typed by the user", map[string]string{"Sec-Fetch-Site": "none"}, http.StatusForbidden},
		{"null origin", map[string]string{"Origin": "null"}, http.StatusForbidden},
		{"origin with a port we are not on", map[string]string{"Origin": "http://example.com:8443"}, http.StatusForbidden},
		{"matching origin", map[string]string{"Origin": "http://example.com"}, http.StatusOK},
		{"matching origin behind a TLS proxy", map[string]string{"Origin": "https://example.com", "X-Forwarded-Proto": "https"}, http.StatusOK},
		{"matching origin behind a host-rewriting proxy", map[string]string{"Origin": "https://kivali.example", "X-Forwarded-Proto": "https, http", "X-Forwarded-Host": "kivali.example, kivali:8080"}, http.StatusOK},
		{"foreign origin behind a host-rewriting proxy", map[string]string{"Origin": "https://evil.example", "X-Forwarded-Proto": "https", "X-Forwarded-Host": "kivali.example"}, http.StatusForbidden},
		{"fetch metadata wins over a matching origin", map[string]string{"Sec-Fetch-Site": "cross-site", "Origin": "http://example.com"}, http.StatusForbidden},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rr := apiDo(t, srv, http.MethodPost, "/api/v1/auto-release", body, tc.hdr)
			if tc.want == http.StatusForbidden {
				assertAPIError(t, rr, http.StatusForbidden)
				return
			}
			if rr.Code != tc.want {
				t.Fatalf("code = %d, want %d (body %q)", rr.Code, tc.want, rr.Body.String())
			}
		})
	}
	// GET is never gated: it changes nothing.
	rr := apiDo(t, srv, http.MethodGet, "/api/v1/me", "", map[string]string{"Sec-Fetch-Site": "cross-site"})
	if rr.Code != http.StatusOK {
		t.Errorf("cross-site GET code = %d, want 200", rr.Code)
	}
}

func TestAPIUnknownRouteAndMethodAreJSON(t *testing.T) {
	srv := newTestServer(t)
	assertAPIError(t, apiDo(t, srv, http.MethodGet, "/api/v1/nope", "", nil), http.StatusNotFound)
	rr := apiDo(t, srv, http.MethodPost, "/api/v1/me", "{}", nil)
	assertAPIError(t, rr, http.StatusMethodNotAllowed)
	if allow := rr.Header().Get("allow"); allow != http.MethodGet {
		t.Errorf("allow = %q, want GET", allow)
	}
}

func TestAPIResponsesAreNotCached(t *testing.T) {
	srv := newTestServer(t)
	for _, p := range []string{"/api/v1/me", "/api/v1/snapshot", "/api/v1/agents", "/api/v1/nope"} {
		rr := apiDo(t, srv, http.MethodGet, p, "", nil)
		if cc := rr.Header().Get("cache-control"); cc != "no-store" {
			t.Errorf("%s: cache-control = %q", p, cc)
		}
	}
}

func TestAPIMe(t *testing.T) {
	srv := newTestServer(t)
	srv.VersionName = "v9.9.9"
	if err := srv.Store.WriteCompanyName("Plainsong"); err != nil {
		t.Fatalf("branding: %v", err)
	}
	rr := apiDo(t, srv, http.MethodGet, "/api/v1/me", "", nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("code = %d body %q", rr.Code, rr.Body.String())
	}
	me := decodeAPI[apitypes.Me](t, rr)
	if me.User.Email != testOwnerEmail || me.User.Name != "Test" || me.User.Initials != "T" {
		t.Errorf("user = %+v", me.User)
	}
	if me.Org.Name != "Plainsong" || me.Org.HasLogo {
		t.Errorf("org = %+v", me.Org)
	}
	if me.Version != "v9.9.9" || me.DevMode {
		t.Errorf("version = %q dev_mode = %v", me.Version, me.DevMode)
	}
}

func TestNameFromEmail(t *testing.T) {
	cases := []struct{ email, name, initials string }{
		{"maya2645@example.com", "Maya", "M"},
		{"jane.doe@example.com", "Jane Doe", "JD"},
		{"mary-ann_de.la.cruz+work@example.com", "Mary Ann De La Cruz", "MC"},
		{"12345@example.com", "12345", "1"},
		{"", "", ""},
	}
	for _, c := range cases {
		name, initials := nameFromEmail(c.email)
		if name != c.name || initials != c.initials {
			t.Errorf("nameFromEmail(%q) = %q, %q; want %q, %q", c.email, name, initials, c.name, c.initials)
		}
	}
}

// The API's snapshot is the SSE snapshot, byte for byte in shape.
func TestAPISnapshotMatchesStream(t *testing.T) {
	srv := newTestServer(t)
	if err := srv.Store.CreateAgent(store.Agent{Slug: "chief-of-staff", Role: "Chief of Staff", ReportsTo: "ceo"}, "k"); err != nil {
		t.Fatalf("seed: %v", err)
	}
	rr := apiDo(t, srv, http.MethodGet, "/api/v1/snapshot", "", nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("code = %d", rr.Code)
	}
	got := decodeAPI[apitypes.OrgSnapshot](t, rr)
	var want apitypes.OrgSnapshot
	if err := json.Unmarshal(srv.buildOrgSnapshot(), &want); err != nil {
		t.Fatal(err)
	}
	gotJSON, _ := json.Marshal(got)
	wantJSON, _ := json.Marshal(want)
	if string(gotJSON) != string(wantJSON) {
		t.Errorf("api snapshot\n %s\nstream snapshot\n %s", gotJSON, wantJSON)
	}
	if len(got.Agents) != 1 || got.Agents[0].Name != "Chief of Staff" || got.Agents[0].State != apitypes.AgentStateIdle {
		t.Errorf("agents = %+v", got.Agents)
	}
	// Empty lists are [] on the wire, not null or absent.
	for _, key := range []string{`"goals":[]`, `"pending_paths":[]`, `"ceo_paths":[]`} {
		if !strings.Contains(rr.Body.String(), key) {
			t.Errorf("body lacks %s: %s", key, rr.Body.String())
		}
	}
}

func seedTeam(t *testing.T, srv *Server) {
	t.Helper()
	for _, a := range []store.Agent{
		{Slug: "chief-of-staff", Role: "Chief of Staff", ReportsTo: "ceo", Model: provider.MockModelSmall},
		{Slug: "engineering-lead", Role: "Engineering lead", ReportsTo: "chief-of-staff"},
		{Slug: "analyst", Role: "", ReportsTo: "engineering-lead"},
		{Slug: "orphan", Role: "Orphan", ReportsTo: "gone"},
		{Slug: "retired", Role: "Retired", ReportsTo: "ceo"},
	} {
		if err := srv.Store.CreateAgent(a, ""); err != nil {
			t.Fatalf("seed %s: %v", a.Slug, err)
		}
	}
	if err := srv.Store.ArchiveAgent("retired"); err != nil {
		t.Fatalf("archive: %v", err)
	}
}

func TestAPIAgents(t *testing.T) {
	srv := newTestServer(t)
	srv.AgentModel = provider.MockModelLarge
	seedTeam(t, srv)
	srv.chatHubs = map[string]*chatHub{"engineering-lead": {hub: newHub(), slug: "engineering-lead", spawnSource: "chat"}}

	rr := apiDo(t, srv, http.MethodGet, "/api/v1/agents", "", nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("code = %d body %q", rr.Code, rr.Body.String())
	}
	resp := decodeAPI[apitypes.AgentsResponse](t, rr)
	type row struct {
		slug, name string
		depth      int
		state      apitypes.AgentState
	}
	want := []row{
		{"chief-of-staff", "Chief of Staff", 1, apitypes.AgentStateIdle},
		{"engineering-lead", "Engineering lead", 2, apitypes.AgentStateRunning},
		{"analyst", "analyst", 3, apitypes.AgentStateIdle},
		{"orphan", "Orphan", 1, apitypes.AgentStateIdle},
	}
	if len(resp.Agents) != len(want) {
		t.Fatalf("agents = %+v", resp.Agents)
	}
	for i, w := range want {
		a := resp.Agents[i]
		if a.Slug != w.slug || a.Name != w.name || a.Depth != w.depth || a.State != w.state {
			t.Errorf("agents[%d] = %+v, want %+v", i, a, w)
		}
	}
	cos := resp.Agents[0]
	if cos.Model != provider.MockModelSmall || cos.ModelLabel != "Mock Small" || cos.Effort != "" || cos.ReportsTo != "ceo" || cos.Created.IsZero() {
		t.Errorf("chief-of-staff = %+v", cos)
	}
	if vp := resp.Agents[1]; vp.Model != provider.MockModelLarge || vp.ModelLabel != "Mock Large" || vp.Effort != "high" {
		t.Errorf("engineering-lead inherits fleet defaults, got model %q effort %q", vp.Model, vp.Effort)
	}
	if len(resp.Archived) != 1 || resp.Archived[0].Slug != "retired" || resp.Archived[0].ArchivedAt == nil || resp.Archived[0].Depth != 0 {
		t.Errorf("archived = %+v", resp.Archived)
	}
}

func TestAPIAgentDetail(t *testing.T) {
	srv := newTestServer(t)
	srv.AgentModel = provider.MockModelLarge
	seedTeam(t, srv)
	if err := srv.Store.WriteRole("engineering-lead", "\n# Engineering lead, owns the release\n\nMore text.\n"); err != nil {
		t.Fatalf("role: %v", err)
	}
	if err := srv.Store.AppendChatMessage("engineering-lead", store.ChatMessage{Role: store.RoleSent, Content: strings.Repeat("x", 4000), TS: time.Now()}); err != nil {
		t.Fatalf("chat: %v", err)
	}

	rr := apiDo(t, srv, http.MethodGet, "/api/v1/agents/engineering-lead", "", nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("code = %d body %q", rr.Code, rr.Body.String())
	}
	d := decodeAPI[apitypes.AgentDetail](t, rr)
	if d.Slug != "engineering-lead" || d.Depth != 2 || d.Archived || d.RoleLine != "Engineering lead, owns the release" {
		t.Errorf("detail = %+v", d)
	}
	if len(d.Models) != len(srv.Provider.Models()) {
		t.Errorf("models = %+v, want the %d selectable ones and no legacy row", d.Models, len(srv.Provider.Models()))
	}
	for _, m := range d.Models {
		if m.Legacy || m.Label == "" {
			t.Errorf("model option %+v", m)
		}
	}
	if d.ModelLabel != provider.Label(srv.Provider, d.Model) || d.Counts.PastChats != 0 || d.Counts.Background != 0 {
		t.Errorf("model label %q counts %+v", d.ModelLabel, d.Counts)
	}
	if d.Context.Tokens != 1000 || d.Context.Limit == 0 || d.Context.Pct != d.ContextPct {
		t.Errorf("context = %+v (summary pct %d)", d.Context, d.ContextPct)
	}
	if !strings.Contains(rr.Body.String(), `"slug":"engineering-lead"`) {
		t.Errorf("summary fields must be inlined, body %s", rr.Body.String())
	}

	// A pin the picker no longer offers stays visible, marked legacy.
	if err := srv.Store.SetAgentModel("engineering-lead", "claude-retired-1"); err != nil {
		t.Fatalf("pin: %v", err)
	}
	d = decodeAPI[apitypes.AgentDetail](t, apiDo(t, srv, http.MethodGet, "/api/v1/agents/engineering-lead", "", nil))
	last := d.Models[len(d.Models)-1]
	if !last.Legacy || last.ID != "claude-retired-1" || d.Model != "claude-retired-1" {
		t.Errorf("models tail = %+v, model = %q", last, d.Model)
	}

	// No role document: the role title; no title either: the slug.
	d = decodeAPI[apitypes.AgentDetail](t, apiDo(t, srv, http.MethodGet, "/api/v1/agents/analyst", "", nil))
	if d.RoleLine != "analyst" {
		t.Errorf("analyst role_line = %q", d.RoleLine)
	}

	// Archived agents are served, marked archived.
	d = decodeAPI[apitypes.AgentDetail](t, apiDo(t, srv, http.MethodGet, "/api/v1/agents/retired", "", nil))
	if !d.Archived || d.ArchivedAt == nil || d.RoleLine != "Retired" {
		t.Errorf("retired = %+v", d)
	}

	for _, slug := range []string{"ceo", "nobody"} {
		assertAPIError(t, apiDo(t, srv, http.MethodGet, "/api/v1/agents/"+slug, "", nil), http.StatusNotFound)
	}
}

func TestAPIAutoRelease(t *testing.T) {
	srv := newTestServer(t)
	for _, v := range []string{"now", "30s", "2m", "5m", "20m", "off"} {
		rr := apiDo(t, srv, http.MethodPost, "/api/v1/auto-release", `{"value":"`+v+`"}`, nil)
		if rr.Code != http.StatusOK {
			t.Fatalf("%s: code = %d body %q", v, rr.Code, rr.Body.String())
		}
		if got := decodeAPI[apitypes.AutoReleaseResponse](t, rr); string(got.Value) != v {
			t.Errorf("%s: response %+v", v, got)
		}
		if got := autoReleaseKey(srv.Store.ReadAutoRelease()); got != v {
			t.Errorf("%s: stored as %q", v, got)
		}
	}
	assertAPIError(t, apiDo(t, srv, http.MethodPost, "/api/v1/auto-release", `{"value":"7m"}`, nil), http.StatusBadRequest)
	assertAPIError(t, apiDo(t, srv, http.MethodPost, "/api/v1/auto-release", `{"value":"2m","extra":1}`, nil), http.StatusBadRequest)
	assertAPIError(t, apiDo(t, srv, http.MethodPost, "/api/v1/auto-release", `{"value":"2m"}{}`, nil), http.StatusBadRequest)
	assertAPIError(t, apiDo(t, srv, http.MethodPost, "/api/v1/auto-release", `not json`, nil), http.StatusBadRequest)
	huge := `{"value":"` + strings.Repeat("x", apiMaxJSONBody) + `"}`
	assertAPIError(t, apiDo(t, srv, http.MethodPost, "/api/v1/auto-release", huge, nil), http.StatusRequestEntityTooLarge)
	if got := autoReleaseKey(srv.Store.ReadAutoRelease()); got != "off" {
		t.Errorf("a refused request changed the setting to %q", got)
	}
}

// A JSON endpoint takes only application/json: what an HTML form can
// send (urlencoded, multipart, text/plain) is refused with 415 even
// when the origin check passes, and a charset parameter is fine.
func TestAPIJSONEndpointsRefuseFormBodies(t *testing.T) {
	srv := newTestServer(t)
	for _, ct := range []string{"application/x-www-form-urlencoded", "multipart/form-data; boundary=x", "text/plain", ""} {
		req := httptest.NewRequest(http.MethodPost, "/api/v1/auto-release", strings.NewReader(`{"value":"2m"}`))
		req.Header.Set("Sec-Fetch-Site", "same-origin")
		if ct != "" {
			req.Header.Set("Content-Type", ct)
		}
		rr := httptest.NewRecorder()
		authedHandler(t, srv).ServeHTTP(rr, req)
		if rr.Code != http.StatusUnsupportedMediaType {
			t.Errorf("content-type %q: code = %d, want 415 (body %q)", ct, rr.Code, rr.Body.String())
			continue
		}
		assertAPIError(t, rr, http.StatusUnsupportedMediaType)
	}
	if got := autoReleaseKey(srv.Store.ReadAutoRelease()); got != "off" {
		t.Errorf("a refused body changed the setting to %q", got)
	}
	rr := apiDo(t, srv, http.MethodPost, "/api/v1/auto-release", `{"value":"2m"}`, map[string]string{"Sec-Fetch-Site": "same-origin", "Content-Type": "application/json; charset=utf-8"})
	if rr.Code != http.StatusOK {
		t.Errorf("json with charset: code = %d body %q", rr.Code, rr.Body.String())
	}
}

// A slug that is not a single path segment never reaches the store,
// which would otherwise join it into a filesystem path.
func TestAPIAgentRefusesPathLikeSlugs(t *testing.T) {
	srv := newTestServer(t)
	seedTeam(t, srv)
	for _, p := range []string{"/api/v1/agents/_archived%2Fretired", "/api/v1/agents/..%2F..%2Fetc", "/api/v1/agents/.hidden", "/api/v1/agents/_archived"} {
		assertAPIError(t, apiDo(t, srv, http.MethodGet, p, "", nil), http.StatusNotFound)
	}
}
