package web

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"math"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
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

// orgDo serves one request through the Org routes behind the session
// middleware and the API's own header and same-origin middleware, as
// wireAPIRoutes mounts them. It signs in as email (the test owner when
// empty). Non-GET requests carry Sec-Fetch-Site: same-origin.
func orgDo(t *testing.T, srv *Server, email, method, path, contentType string, body []byte) *httptest.ResponseRecorder {
	t.Helper()
	mux := http.NewServeMux()
	srv.wireAPIOrgRoutes(mux)
	var req *http.Request
	if body == nil {
		req = httptest.NewRequest(method, path, nil)
	} else {
		req = httptest.NewRequest(method, path, bytes.NewReader(body))
		req.Header.Set("content-type", contentType)
	}
	if method != http.MethodGet {
		req.Header.Set("Sec-Fetch-Site", "same-origin")
	}
	if email == "" {
		email = testOwnerEmail
	}
	tok, err := srv.AuthMW.Codec.EncodeSession(auth.NewSession(email))
	if err != nil {
		t.Fatal(err)
	}
	req.AddCookie(&http.Cookie{Name: auth.CookieName, Value: tok})
	rr := httptest.NewRecorder()
	srv.AuthMW.Wrap(apiHeaders(requireSameOrigin(mux))).ServeHTTP(rr, req)
	return rr
}

func orgGet(t *testing.T, srv *Server, path string) *httptest.ResponseRecorder {
	t.Helper()
	return orgDo(t, srv, "", http.MethodGet, path, "", nil)
}

func orgJSON(t *testing.T, srv *Server, method, path string, v any) *httptest.ResponseRecorder {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return orgDo(t, srv, "", method, path, "application/json", b)
}

// orgOK decodes a 200 response, failing on any other status.
func orgOK[T any](t *testing.T, rr *httptest.ResponseRecorder) T {
	t.Helper()
	if rr.Code != http.StatusOK {
		t.Fatalf("code = %d, want 200 (body %s)", rr.Code, rr.Body.String())
	}
	return decodeAPI[T](t, rr)
}

// multipartBody builds a multipart body: files maps field → filename →
// bytes (one file per field entry), fields are plain values.
func orgMultipart(t *testing.T, files []multipartFile, fields map[string]string) ([]byte, string) {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	for k, v := range fields {
		if err := mw.WriteField(k, v); err != nil {
			t.Fatal(err)
		}
	}
	for _, f := range files {
		fw, err := mw.CreateFormFile(f.field, f.name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := fw.Write(f.data); err != nil {
			t.Fatal(err)
		}
	}
	if err := mw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes(), mw.FormDataContentType()
}

type multipartFile struct {
	field, name string
	data        []byte
}

func nearUSD(a, b float64) bool { return math.Abs(a-b) < 1e-9 }

// ---- usage ----

var orgNow = time.Date(2026, 9, 28, 15, 0, 0, 0, time.UTC)

func TestAPIOrgUsage(t *testing.T) {
	srv := newTestServer(t)
	srv.Clock = clock.NewFakeAt(orgNow)
	for _, a := range []store.Agent{
		{Slug: "chief-of-staff", Role: "Chief of Staff", ReportsTo: "ceo"},
		{Slug: "engineering-lead", Role: "Engineering lead", ReportsTo: "chief-of-staff"},
	} {
		if err := srv.Store.CreateAgent(a, "# role\n"); err != nil {
			t.Fatal(err)
		}
	}
	const priced = provider.MockModelLarge
	rows := []store.UsageRecord{
		// Outside every window, and outside the rollup's read.
		{TS: orgNow.Add(-35 * 24 * time.Hour), Agent: "engineering-lead", Model: priced, InputTokens: 9_000_000},
		// Twenty days ago: 30-day only, by an agent no longer on file.
		{TS: orgNow.Add(-20 * 24 * time.Hour), Agent: "retired", Model: priced, InputTokens: 10_000},
		// Three days ago: 7 and 30 days.
		{TS: orgNow.Add(-3 * 24 * time.Hour), Agent: "engineering-lead", Model: priced, OutputTokens: 10_000},
		// 23:00 yesterday UTC: inside 24 h, not today.
		{TS: orgNow.Add(-16 * time.Hour), Agent: "chief-of-staff", Model: priced, InputTokens: 100_000, CacheReadTokens: 300_000, CacheCreateTokens: 100_000},
		// Today.
		{TS: orgNow.Add(-2 * time.Hour), Agent: "engineering-lead", Model: priced, InputTokens: 1_000_000, CostUSD: 999},
		{TS: orgNow.Add(-1 * time.Hour), Agent: "", Purpose: "inbox_summary", Model: priced, InputTokens: 1_000},
		{TS: orgNow.Add(-1 * time.Hour), Agent: "chief-of-staff", Model: "mock-unknown-0", InputTokens: 5_000_000, OutputTokens: 7},
	}
	cost := make([]float64, len(rows))
	for i, r := range rows {
		if err := srv.Store.AppendUsage(r); err != nil {
			t.Fatal(err)
		}
		cost[i], _ = priceUsage(provider.MockProvider{}, r)
	}

	u := orgOK[apitypes.Usage](t, orgGet(t, srv, "/api/v1/org/usage"))

	// Daily: 30 UTC days, oldest first, ending today.
	if len(u.Daily) != 30 || u.Daily[0].Date != "2026-08-30" || u.Daily[29].Date != "2026-09-28" {
		t.Fatalf("daily = %d days, %s..%s", len(u.Daily), u.Daily[0].Date, u.Daily[len(u.Daily)-1].Date)
	}
	wantDaily := map[string]float64{
		"2026-09-08": cost[1],
		"2026-09-25": cost[2],
		"2026-09-27": cost[3],
		"2026-09-28": cost[4] + cost[5],
	}
	for _, d := range u.Daily {
		if !nearUSD(d.Spend, wantDaily[d.Date]) {
			t.Errorf("daily %s = %v, want %v", d.Date, d.Spend, wantDaily[d.Date])
		}
	}

	// Tiles: today since 00:00 UTC; calls count the unpriced call.
	if tl := u.Tiles.Today; tl.Calls != 3 || !nearUSD(tl.Spend, cost[4]+cost[5]) || tl.CacheHitPct != 0 {
		t.Errorf("today = %+v", tl)
	}
	if tl := u.Tiles.D7; tl.Calls != 5 || !nearUSD(tl.Spend, cost[2]+cost[3]+cost[4]+cost[5]) {
		t.Errorf("d7 = %+v", tl)
	}
	// 300k cache reads over 100k + 300k + 100k + 1M + 1k + 5M input-side.
	if want := int(math.Round(100 * 300_000.0 / 6_501_000.0)); u.Tiles.D7.CacheHitPct != want {
		t.Errorf("d7 cache hit = %d, want %d", u.Tiles.D7.CacheHitPct, want)
	}
	if tl := u.Tiles.D30; tl.Calls != 6 || !nearUSD(tl.Spend, cost[1]+cost[2]+cost[3]+cost[4]+cost[5]) {
		t.Errorf("d30 = %+v", tl)
	}

	// Home and Org price the same way: the snapshot's readouts match.
	today, week := srv.spendReadouts(orgNow)
	if !nearUSD(today, u.Tiles.Today.Spend) || !nearUSD(week, u.Tiles.D7.Spend) {
		t.Errorf("snapshot spend today/7d = %v/%v, org tiles = %v/%v", today, week, u.Tiles.Today.Spend, u.Tiles.D7.Spend)
	}

	// By agent: 30-day spend descending, display names, "Kivali" for
	// calls made for no agent, the slug for an agent not on file.
	var got []string
	for _, a := range u.ByAgent {
		got = append(got, a.Slug+"="+a.Name)
	}
	want := []string{"engineering-lead=Engineering lead", "chief-of-staff=Chief of Staff", "retired=retired", "=Kivali"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("by_agent = %v, want %v", got, want)
	}
	if a := u.ByAgent[0]; !nearUSD(a.D30, cost[2]+cost[4]) || !nearUSD(a.D7, cost[2]+cost[4]) {
		t.Errorf("engineering-lead = %+v", a)
	}
	if a := u.ByAgent[2]; a.D7 != 0 || !nearUSD(a.D30, cost[1]) {
		t.Errorf("retired = %+v", a)
	}

	// Windows: the settings table, unpriced tokens counted apart.
	h := u.Windows.H24
	if h.Calls != 4 || h.Unpriced != 5_000_007 || h.CacheRead != 300_000 || h.CacheCreate != 100_000 ||
		h.TokensIn != 100_000+300_000+100_000+1_000_000+1_000+5_000_000 || h.TokensOut != 7 {
		t.Errorf("h24 = %+v", h)
	}
	if !nearUSD(h.Spend, cost[3]+cost[4]+cost[5]) {
		t.Errorf("h24 spend = %v", h.Spend)
	}
	if h.UnpricedCalls != 1 {
		t.Errorf("24h unpriced_calls = %d, want 1", h.UnpricedCalls)
	}
	if u.Windows.D30.Calls != 6 || u.Windows.D30.Unpriced != 5_000_007 || u.Windows.D30.UnpricedCalls != 1 {
		t.Errorf("d30 window = %+v", u.Windows.D30)
	}
}

// The rollup reads usage.jsonl once per usage version: a write that
// bypasses the store's append hook is not seen, an append is.
func TestAPIOrgUsageCachedPerUsageVersion(t *testing.T) {
	srv := newTestServer(t)
	srv.Clock = clock.NewFakeAt(orgNow)
	rec := store.UsageRecord{TS: orgNow.Add(-time.Hour), Agent: "a", Model: provider.MockModelLarge, InputTokens: 1_000}
	if err := srv.Store.AppendUsage(rec); err != nil {
		t.Fatal(err)
	}
	if u := orgOK[apitypes.Usage](t, orgGet(t, srv, "/api/v1/org/usage")); u.Tiles.Today.Calls != 1 {
		t.Fatalf("calls = %d, want 1", u.Tiles.Today.Calls)
	}
	line, _ := json.Marshal(rec)
	f, err := os.OpenFile(filepath.Join(srv.Store.Root(), "usage.jsonl"), os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = f.Write(append(line, '\n'))
	_ = f.Close()
	if u := orgOK[apitypes.Usage](t, orgGet(t, srv, "/api/v1/org/usage")); u.Tiles.Today.Calls != 1 {
		t.Fatalf("calls = %d after an unhooked write, want the cached 1", u.Tiles.Today.Calls)
	}
	if err := srv.Store.AppendUsage(rec); err != nil {
		t.Fatal(err)
	}
	if u := orgOK[apitypes.Usage](t, orgGet(t, srv, "/api/v1/org/usage")); u.Tiles.Today.Calls != 3 {
		t.Fatalf("calls = %d after an append, want 3", u.Tiles.Today.Calls)
	}
}

func TestAPIOrgUsageEmpty(t *testing.T) {
	srv := newTestServer(t)
	srv.Clock = clock.NewFakeAt(orgNow)
	u := orgOK[apitypes.Usage](t, orgGet(t, srv, "/api/v1/org/usage"))
	if len(u.Daily) != 30 || len(u.ByAgent) != 0 || u.Tiles.D30.Calls != 0 {
		t.Errorf("empty usage = %+v", u)
	}
}

// ---- organization ----

func TestAPIOrgNameAndLogo(t *testing.T) {
	srv := newTestServer(t)
	if o := orgOK[apitypes.Org](t, orgGet(t, srv, "/api/v1/org")); o.Name != "" || o.HasLogo || o.LogoURL != nil {
		t.Fatalf("fresh org = %+v", o)
	}
	if o := orgOK[apitypes.Org](t, orgJSON(t, srv, http.MethodPut, "/api/v1/org", apitypes.OrgPut{Name: "  Acme  "})); o.Name != "Acme" {
		t.Errorf("name = %q", o.Name)
	}
	assertAPIError(t, orgJSON(t, srv, http.MethodPut, "/api/v1/org", apitypes.OrgPut{Name: strings.Repeat("x", 65)}), http.StatusBadRequest)

	body, ct := orgMultipart(t, []multipartFile{{"logo", "logo.png", squarePNG(t, 256)}}, nil)
	o := orgOK[apitypes.Org](t, orgDo(t, srv, "", http.MethodPost, "/api/v1/org/logo", ct, body))
	if !o.HasLogo || o.LogoURL == nil || *o.LogoURL != "/branding/icon-512.png" {
		t.Fatalf("after upload = %+v", o)
	}
	if _, _, err := srv.Store.ReadFaviconAsset("icon-512.png"); err != nil {
		t.Errorf("logo asset not written: %v", err)
	}

	bad, ct := orgMultipart(t, []multipartFile{{"logo", "wide.png", nonSquarePNG(t, 64, 32)}}, nil)
	e := assertAPIError(t, orgDo(t, srv, "", http.MethodPost, "/api/v1/org/logo", ct, bad), http.StatusBadRequest)
	if !strings.Contains(e.Error, "square") {
		t.Errorf("bad logo error = %q", e.Error)
	}
	assertAPIError(t, orgDo(t, srv, "", http.MethodPost, "/api/v1/org/logo", "application/json", []byte(`{}`)), http.StatusUnsupportedMediaType)

	if o := orgOK[apitypes.Org](t, orgDo(t, srv, "", http.MethodDelete, "/api/v1/org/logo", "", nil)); o.HasLogo || o.LogoURL != nil || o.Name != "Acme" {
		t.Errorf("after delete = %+v", o)
	}
}

// ---- handbook ----

func TestAPIHandbook(t *testing.T) {
	srv := newTestServer(t)
	c := orgOK[apitypes.Handbook](t, orgGet(t, srv, "/api/v1/org/handbook"))
	if !c.Draft || c.Content != seed.Handbook || c.UpdatedAt != nil || len(c.Sections) == 0 {
		t.Fatalf("draft = draft:%v updated:%v sections:%d", c.Draft, c.UpdatedAt, len(c.Sections))
	}

	c = orgOK[apitypes.Handbook](t, orgJSON(t, srv, http.MethodPut, "/api/v1/org/handbook",
		apitypes.HandbookPut{Content: "We ship.\n\n## Money\n- Under budget\n\n- On time\n## Hiring\nSlowly.\n"}))
	// line_count leaves blank lines out; lines keeps them.
	want := []apitypes.DocSection{
		{Title: "Opening", Lines: []string{"We ship.", ""}, LineCount: 1},
		{Title: "Money", Lines: []string{"- Under budget", "", "- On time"}, LineCount: 2},
		{Title: "Hiring", Lines: []string{"Slowly.", ""}, LineCount: 1},
	}
	if c.Draft || c.UpdatedAt == nil || !reflect.DeepEqual(c.Sections, want) {
		t.Errorf("saved = draft:%v updated:%v sections:%+v", c.Draft, c.UpdatedAt, c.Sections)
	}
	if got, _ := srv.Store.ReadHandbook(); !strings.HasPrefix(got, "We ship.") {
		t.Errorf("stored = %q", got)
	}
	assertAPIError(t, orgJSON(t, srv, http.MethodPut, "/api/v1/org/handbook", apitypes.HandbookPut{Content: "  \n"}), http.StatusBadRequest)
}

// splitDocSections follows DocDiff.splitSections line for line; these
// cases include the design system's own tests.
func TestSplitDocSections(t *testing.T) {
	titles := func(md string) []string {
		out := []string{}
		for _, s := range splitDocSections(md) {
			out = append(out, s.Title)
		}
		return out
	}
	for _, tc := range []struct {
		md   string
		want []string
	}{
		{"Opening line.\n## Priorities\n- Water\n## Budget\n- Under $500", []string{"Opening", "Priorities", "Budget"}},
		{"## A\nx", []string{"A"}},
		{"", []string{}},
		{"\n\n## A\n## B\nx", []string{"A", "B"}},
		{"x\n## A\n## B\ny", []string{"Opening", "B"}},
		{"### Not a section\n##NoSpace\n## Real  \nz", []string{"Opening", "Real"}},
	} {
		if got := titles(tc.md); !reflect.DeepEqual(got, tc.want) {
			t.Errorf("splitDocSections(%q) = %v, want %v", tc.md, got, tc.want)
		}
	}
}

// ---- files ----

func TestAPIFiles(t *testing.T) {
	srv := newTestServer(t)
	if f := orgOK[apitypes.ProjectFiles](t, orgGet(t, srv, "/api/v1/org/files")); len(f.Files) != 0 {
		t.Fatalf("fresh files = %+v", f)
	}
	body, ct := orgMultipart(t, []multipartFile{
		{"file", "notes.md", []byte("# Notes\nhello\n")},
		{"file", "plan.md", []byte("# Plan\nship\n")},
	}, map[string]string{"kind": "reference"})
	f := orgOK[apitypes.ProjectFiles](t, orgDo(t, srv, "", http.MethodPost, "/api/v1/org/files", ct, body))
	if len(f.Files) != 2 {
		t.Fatalf("after upload = %+v", f)
	}
	notes := f.Files[0]
	if notes.Name != "notes.md" || notes.SizeBytes != 14 || !notes.Extracted || notes.Kind != "reference" ||
		notes.URL != "/api/v1/org/files/"+notes.SHA+"/original" || notes.UploadedAt.IsZero() {
		t.Errorf("row = %+v", notes)
	}

	dl := orgGet(t, srv, notes.URL)
	if dl.Code != http.StatusOK || dl.Body.String() != "# Notes\nhello\n" || !strings.Contains(dl.Header().Get("content-disposition"), "notes.md") {
		t.Errorf("download = %d %q %q", dl.Code, dl.Body.String(), dl.Header().Get("content-disposition"))
	}

	f = orgOK[apitypes.ProjectFiles](t, orgDo(t, srv, "", http.MethodDelete, "/api/v1/org/files/"+notes.SHA, "", nil))
	if len(f.Files) != 1 || f.Files[0].Name != "plan.md" {
		t.Errorf("after delete = %+v", f)
	}
	assertAPIError(t, orgDo(t, srv, "", http.MethodDelete, "/api/v1/org/files/"+notes.SHA, "", nil), http.StatusNotFound)
	assertAPIError(t, orgDo(t, srv, "", http.MethodDelete, "/api/v1/org/files/not-a-sha", "", nil), http.StatusNotFound)

	assertAPIError(t, orgJSON(t, srv, http.MethodPost, "/api/v1/org/files/bulk-delete", apitypes.FilesBulkDelete{SHAs: []string{".."}}), http.StatusBadRequest)
	assertAPIError(t, orgJSON(t, srv, http.MethodPost, "/api/v1/org/files/bulk-delete", apitypes.FilesBulkDelete{SHAs: []string{}}), http.StatusBadRequest)
	f = orgOK[apitypes.ProjectFiles](t, orgJSON(t, srv, http.MethodPost, "/api/v1/org/files/bulk-delete", apitypes.FilesBulkDelete{SHAs: []string{f.Files[0].SHA}}))
	if len(f.Files) != 0 {
		t.Errorf("after bulk delete = %+v", f)
	}

	bad, ct := orgMultipart(t, []multipartFile{{"file", "x.md", []byte("x")}}, map[string]string{"kind": "nonsense"})
	assertAPIError(t, orgDo(t, srv, "", http.MethodPost, "/api/v1/org/files", ct, bad), http.StatusBadRequest)
	none, ct := orgMultipart(t, nil, nil)
	assertAPIError(t, orgDo(t, srv, "", http.MethodPost, "/api/v1/org/files", ct, none), http.StatusBadRequest)
	assertAPIError(t, orgDo(t, srv, "", http.MethodPost, "/api/v1/org/files", "application/json", []byte(`{}`)), http.StatusUnsupportedMediaType)
}

// ---- skills ----

func skillMD(name, version, desc string) []byte {
	return []byte("---\nname: " + name + "\nversion: " + version + "\ndescription: " + desc + "\n---\nDo it.\n")
}

func findSkill(t *testing.T, s apitypes.Skills, name string) apitypes.Skill {
	t.Helper()
	for _, k := range s.Skills {
		if k.Name == name {
			return k
		}
	}
	t.Fatalf("skill %q not in %+v", name, s.Skills)
	return apitypes.Skill{}
}

func TestAPISkills(t *testing.T) {
	srv := newTestServer(t)
	upload := func(md []byte, fields map[string]string) *httptest.ResponseRecorder {
		body, ct := orgMultipart(t, []multipartFile{{"file", "SKILL.md", md}}, fields)
		return orgDo(t, srv, "", http.MethodPost, "/api/v1/org/skills", ct, body)
	}

	s := orgOK[apitypes.Skills](t, upload(skillMD("quote-check", "1.0.0", "Check a quote."), nil))
	if k := findSkill(t, s, "quote-check"); k.Version != "1.0.0" || !k.Enabled || k.Builtin || k.Description != "Check a quote." {
		t.Errorf("uploaded = %+v", k)
	}

	s = orgOK[apitypes.Skills](t, orgDo(t, srv, "", http.MethodPost, "/api/v1/org/skills/quote-check/disable", "", nil))
	if findSkill(t, s, "quote-check").Enabled {
		t.Error("still enabled after disable")
	}
	s = orgOK[apitypes.Skills](t, orgDo(t, srv, "", http.MethodPost, "/api/v1/org/skills/quote-check/enable", "", nil))
	if !findSkill(t, s, "quote-check").Enabled {
		t.Error("still disabled after enable")
	}
	assertAPIError(t, orgDo(t, srv, "", http.MethodPost, "/api/v1/org/skills/ghost/enable", "", nil), http.StatusNotFound)

	// Same version: the downgrade 409, with both versions in the body
	// and the form route's headers; nothing written.
	rr := upload(skillMD("quote-check", "1.0.0", "Edited."), nil)
	if rr.Code != http.StatusConflict {
		t.Fatalf("same version = %d %s", rr.Code, rr.Body.String())
	}
	down := decodeAPI[apitypes.SkillDowngrade](t, rr)
	if down.OldVersion != "1.0.0" || down.NewVersion != "1.0.0" || down.Error == "" || down.Who == "" {
		t.Errorf("downgrade body = %+v", down)
	}
	if rr.Header().Get("X-Kivali-Downgrade") != "1" || rr.Header().Get("X-Kivali-Skill-Old-Version") != "1.0.0" {
		t.Errorf("downgrade headers = %v", rr.Header())
	}
	if k, _ := srv.Store.ReadSkill("quote-check"); k.Description != "Check a quote." {
		t.Errorf("refused upload was written: %q", k.Description)
	}

	// Confirmed, it lands; a newer version needs no confirmation.
	s = orgOK[apitypes.Skills](t, upload(skillMD("quote-check", "0.9.0", "Rolled back."), map[string]string{"confirm_downgrade": "true"}))
	if k := findSkill(t, s, "quote-check"); k.Version != "0.9.0" || k.Description != "Rolled back." {
		t.Errorf("confirmed downgrade = %+v", k)
	}
	s = orgOK[apitypes.Skills](t, upload(skillMD("quote-check", "1.1.0", "Newer."), nil))
	if k := findSkill(t, s, "quote-check"); k.Version != "1.1.0" {
		t.Errorf("upgrade = %+v", k)
	}

	assertAPIError(t, upload([]byte("---\nname: nover\n---\n"), nil), http.StatusBadRequest)
	assertAPIError(t, orgDo(t, srv, "", http.MethodPost, "/api/v1/org/skills", "application/json", []byte(`{}`)), http.StatusUnsupportedMediaType)

	s = orgOK[apitypes.Skills](t, orgDo(t, srv, "", http.MethodDelete, "/api/v1/org/skills/quote-check", "", nil))
	for _, k := range s.Skills {
		if k.Name == "quote-check" {
			t.Error("still listed after delete")
		}
	}
	assertAPIError(t, orgDo(t, srv, "", http.MethodDelete, "/api/v1/org/skills/quote-check", "", nil), http.StatusNotFound)
}

// ---- network ----

func TestAPINetwork(t *testing.T) {
	srv := newTestServer(t)
	n := orgOK[apitypes.Network](t, orgGet(t, srv, "/api/v1/org/network"))
	if !hasHost(n, "api.anthropic.com") {
		t.Fatalf("default hosts = %+v", n)
	}
	n = orgOK[apitypes.Network](t, orgJSON(t, srv, http.MethodPost, "/api/v1/org/network", apitypes.NetworkAdd{Host: " Example.COM "}))
	if !hasHost(n, "example.com") {
		t.Errorf("after add = %+v", n)
	}
	assertAPIError(t, orgJSON(t, srv, http.MethodPost, "/api/v1/org/network", apitypes.NetworkAdd{Host: "https://example.org/x"}), http.StatusBadRequest)
	n = orgOK[apitypes.Network](t, orgDo(t, srv, "", http.MethodDelete, "/api/v1/org/network/example.com", "", nil))
	if hasHost(n, "example.com") || !hasHost(n, "api.anthropic.com") {
		t.Errorf("after remove = %+v", n)
	}
	assertAPIError(t, orgDo(t, srv, "", http.MethodDelete, "/api/v1/org/network/example.com", "", nil), http.StatusNotFound)
	al, _ := srv.Store.ReadEgressAllowlist()
	for _, p := range al.Patterns {
		if p == "example.com" {
			t.Error("removed host still in the allowlist file")
		}
	}
}

func hasHost(n apitypes.Network, host string) bool {
	for _, h := range n.Hosts {
		if h.Host == host {
			return true
		}
	}
	return false
}

// ---- backup and restore ----

func TestAPIBackupAndRestore(t *testing.T) {
	src := newTestServer(t)
	src.Clock = clock.NewFakeAt(orgNow)
	if err := src.Store.WriteHandbook("# the rules\n"); err != nil {
		t.Fatal(err)
	}
	if err := src.Store.AppendUsage(store.UsageRecord{TS: orgNow.Add(-time.Hour), Agent: "a", Model: provider.MockModelLarge, InputTokens: 1_000}); err != nil {
		t.Fatal(err)
	}
	bk := orgDo(t, src, "", http.MethodPost, "/api/v1/org/backup", "", nil)
	if bk.Code != http.StatusOK || bk.Header().Get("content-type") != "application/zip" {
		t.Fatalf("backup = %d %q", bk.Code, bk.Header().Get("content-type"))
	}
	if _, err := zip.NewReader(bytes.NewReader(bk.Body.Bytes()), int64(bk.Body.Len())); err != nil {
		t.Fatalf("backup is not a zip: %v", err)
	}

	dst := newTestServer(t)
	dst.Clock = clock.NewFakeAt(orgNow)
	// Load the usage cache first: the restore must invalidate it.
	if u := orgOK[apitypes.Usage](t, orgGet(t, dst, "/api/v1/org/usage")); u.Tiles.Today.Calls != 0 {
		t.Fatalf("fresh usage = %+v", u.Tiles)
	}
	assertAPIError(t, orgDo(t, dst, "", http.MethodPost, "/api/v1/org/restore", "application/json", []byte(`{}`)), http.StatusUnsupportedMediaType)
	body, ct := orgMultipart(t, []multipartFile{{"archive", "backup.zip", bk.Body.Bytes()}}, nil)
	orgOK[apitypes.Empty](t, orgDo(t, dst, "", http.MethodPost, "/api/v1/org/restore", ct, body))
	if got, _ := dst.Store.ReadHandbook(); got != "# the rules\n" {
		t.Errorf("restored handbook = %q", got)
	}
	if u := orgOK[apitypes.Usage](t, orgGet(t, dst, "/api/v1/org/usage")); u.Tiles.Today.Calls != 1 {
		t.Errorf("usage after restore = %+v, want the restored call", u.Tiles)
	}

	// Once the deployment is not fresh, a restore is refused.
	if err := dst.Store.CreateAgent(store.Agent{Slug: "analyst", Role: "Analyst"}, "k"); err != nil {
		t.Fatal(err)
	}
	body, ct = orgMultipart(t, []multipartFile{{"archive", "backup.zip", bk.Body.Bytes()}}, nil)
	assertAPIError(t, orgDo(t, dst, "", http.MethodPost, "/api/v1/org/restore", ct, body), http.StatusConflict)
}

// An upload past the 32 MB memory threshold spills to temporary files.
// The session middleware hands the handler a copy of the request, so
// net/http's own cleanup never sees that copy's form; the handler must
// remove the files itself.
func TestAPIRestoreRemovesSpilledTempFiles(t *testing.T) {
	dst := newTestServer(t) // before TMPDIR moves: its store is a temp dir too
	tmp := t.TempDir()
	t.Setenv("TMPDIR", tmp)
	junk := bytes.Repeat([]byte("x"), setupFormMemory+1<<20)
	body, ct := orgMultipart(t, []multipartFile{{"archive", "backup.zip", junk}}, nil)
	rr := orgDo(t, dst, "", http.MethodPost, "/api/v1/org/restore", ct, body)
	if rr.Code == http.StatusOK {
		t.Fatalf("a junk archive restored: %s", rr.Body.String())
	}
	left, err := os.ReadDir(tmp)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range left {
		t.Errorf("temporary file left behind: %s", e.Name())
	}
}

func TestAPIOrgWritesNeedSameOrigin(t *testing.T) {
	srv := newTestServer(t)
	mux := http.NewServeMux()
	srv.wireAPIOrgRoutes(mux)
	req := httptest.NewRequest(http.MethodPut, "/api/v1/org", strings.NewReader(`{"name":"x"}`))
	req.Header.Set("content-type", "application/json")
	req.Header.Set("Sec-Fetch-Site", "cross-site")
	rr := httptest.NewRecorder()
	apiHeaders(requireSameOrigin(mux)).ServeHTTP(rr, req)
	assertAPIError(t, rr, http.StatusForbidden)
	if br, _ := srv.Store.ReadBranding(); br.CompanyName != "" {
		t.Error("cross-site write landed")
	}
}
