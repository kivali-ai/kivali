package web

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/kivali-ai/kivali/internal/auth"
	"github.com/kivali-ai/kivali/internal/provider"
	"github.com/kivali-ai/kivali/internal/seed"
	"github.com/kivali-ai/kivali/internal/store"
	"github.com/kivali-ai/kivali/internal/web/apitypes"
)

// The name and kind a process is started with are stored at boot when
// nothing is stored yet, and never over what is.
func TestSeedBrandingStoresOnlyWhatIsUnset(t *testing.T) {
	srv, _ := newSetupServer(t, nil)

	if err := srv.SeedBranding("", ""); err != nil {
		t.Fatal(err)
	}
	if st := getSetup(t, srv); st.Org.Name != "" || st.Kind != apitypes.TeamKindUnset || st.Step != apitypes.SetupStepWelcome {
		t.Fatalf("nothing seeded: name %q kind %q step %q", st.Org.Name, st.Kind, st.Step)
	}

	if err := srv.SeedBranding("  Hearth  ", "personal"); err != nil {
		t.Fatal(err)
	}
	st := getSetup(t, srv)
	if st.Org.Name != "Hearth" || st.Kind != apitypes.TeamKindPersonal {
		t.Fatalf("seeded: name %q kind %q", st.Org.Name, st.Kind)
	}
	// A seeded name skips welcome and org.
	if st.Step != apitypes.SetupStepFiles {
		t.Errorf("seeded step = %q, want files", st.Step)
	}
	if st.CoS.DefaultHandbookMD != seed.PersonalHandbook {
		t.Error("a personal team should be offered the personal handbook")
	}

	if err := srv.SeedBranding("Other", "work"); err != nil {
		t.Fatal(err)
	}
	if br, _ := srv.Store.ReadBranding(); br.CompanyName != "Hearth" || br.TeamKind != store.TeamKindPersonal {
		t.Errorf("a second boot overwrote the branding: %+v", br)
	}
}

// A name over the limit is refused as setup would refuse it, and the
// kind is still stored.
func TestSeedBrandingRefusesALongName(t *testing.T) {
	srv, _ := newSetupServer(t, nil)
	if err := srv.SeedBranding(strings.Repeat("x", maxCompanyNameRunes+1), "work"); err == nil {
		t.Fatal("a name over the limit was accepted")
	}
	if br, _ := srv.Store.ReadBranding(); br.CompanyName != "" || br.TeamKind != store.TeamKindWork {
		t.Errorf("branding = %+v, want no name and kind work", br)
	}
}

// seedCapture is a model client that records the seed call.
type seedCapture struct {
	mu  sync.Mutex
	req provider.CompleteRequest
}

func (c *seedCapture) client() *provider.MockClient {
	return &provider.MockClient{CompleteFn: func(_ context.Context, req provider.CompleteRequest) (*provider.CompleteResponse, error) {
		if req.Purpose == "seed" {
			c.mu.Lock()
			c.req = req
			c.mu.Unlock()
		}
		return &provider.CompleteResponse{Content: []provider.ContentBlock{{Type: provider.ContentText, Text: "briefing"}}}, nil
	}}
}

func (c *seedCapture) sent(t *testing.T) string {
	t.Helper()
	c.mu.Lock()
	defer c.mu.Unlock()
	b, err := json.Marshal(c.req)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// A personal team's Chief of Staff starts from the personal
// handbook and always gets the personal first message, with no
// project files and with handbook_from_files alike.
func TestAPISetupSeedCoSPersonal(t *testing.T) {
	for name, req := range map[string]apitypes.SeedCoSRequest{
		"no files":            {},
		"handbook_from_files": {HandbookFromFiles: true},
	} {
		t.Run(name, func(t *testing.T) {
			var capture seedCapture
			srv, _ := newSetupServer(t, capture.client())
			if err := srv.SeedBranding("Hearth", "personal"); err != nil {
				t.Fatal(err)
			}
			if rr := setupJSON(t, srv, http.MethodPost, "/api/v1/setup/seed-cos", req); rr.Code != http.StatusAccepted {
				t.Fatalf("POST seed-cos = %d: %s", rr.Code, rr.Body.String())
			}
			waitSeed(t, srv)

			p := getProgress(t, srv)
			want := "Reading your files=done, Writing its first briefing=done, Hiring=done, Asking it to get to know you=done"
			if p.State != apitypes.SetupProgressStateDone || stageStates(p) != want {
				t.Fatalf("progress = %s [%s], want done [%s]", p.State, stageStates(p), want)
			}
			if c, _ := srv.Store.ReadHandbook(); c != seed.PersonalHandbook {
				t.Error("the personal handbook was not stored")
			}
			hist, err := srv.Store.ReadChatHistory(cosSlug)
			if err != nil || len(hist) != 1 || hist[0].Content != strings.TrimSpace(seed.PersonalFirstMessage) {
				t.Fatalf("chief of staff chat = %+v (err %v), want the personal first message", hist, err)
			}
			if sent := capture.sent(t); !strings.Contains(sent, "the person this team works for") {
				t.Error("the seed call did not use the personal instruction")
			}
		})
	}
}

// A work team, or one with no kind, keeps the work seed exactly.
func TestAPISetupSeedCoSWorkKeepsTheWorkSeed(t *testing.T) {
	var capture seedCapture
	srv, _ := newSetupServer(t, capture.client())
	if err := srv.SeedBranding("Plainsong", "work"); err != nil {
		t.Fatal(err)
	}
	if rr := setupJSON(t, srv, http.MethodPost, "/api/v1/setup/seed-cos", apitypes.SeedCoSRequest{}); rr.Code != http.StatusAccepted {
		t.Fatalf("POST seed-cos = %d: %s", rr.Code, rr.Body.String())
	}
	waitSeed(t, srv)
	if c, _ := srv.Store.ReadHandbook(); c != seed.Handbook {
		t.Error("a work team should store the work handbook")
	}
	if hist, _ := srv.Store.ReadChatHistory(cosSlug); len(hist) != 0 {
		t.Errorf("a work team without handbook_from_files gets no first message; chat = %+v", hist)
	}
	if sent := capture.sent(t); strings.Contains(sent, "the person this team works for") || !strings.Contains(sent, "about the business") {
		t.Error("a work team's seed call should use the work instruction")
	}
}

func TestAPIOrgKind(t *testing.T) {
	srv := newTestServer(t)
	if o := orgOK[apitypes.Org](t, orgGet(t, srv, "/api/v1/org")); o.Kind != apitypes.TeamKindUnset {
		t.Fatalf("fresh kind = %q", o.Kind)
	}
	personal := apitypes.TeamKindPersonal
	if o := orgOK[apitypes.Org](t, orgJSON(t, srv, http.MethodPut, "/api/v1/org", apitypes.OrgPut{Name: "Hearth", Kind: &personal})); o.Kind != personal || o.Name != "Hearth" {
		t.Fatalf("after PUT = %+v", o)
	}
	if c := orgOK[apitypes.Handbook](t, orgGet(t, srv, "/api/v1/org/handbook")); !c.Draft || c.Content != seed.PersonalHandbook {
		t.Error("a personal team's draft handbook should be the personal one")
	}

	bad := apitypes.TeamKind("family")
	assertAPIError(t, orgJSON(t, srv, http.MethodPut, "/api/v1/org", apitypes.OrgPut{Name: "Renamed", Kind: &bad}), http.StatusBadRequest)
	if o := orgOK[apitypes.Org](t, orgGet(t, srv, "/api/v1/org")); o.Name != "Hearth" || o.Kind != personal {
		t.Errorf("a refused PUT changed the org: %+v", o)
	}

	// A PUT without a kind leaves it.
	if o := orgOK[apitypes.Org](t, orgJSON(t, srv, http.MethodPut, "/api/v1/org", apitypes.OrgPut{Name: "Home"})); o.Kind != personal {
		t.Errorf("kind after a name-only PUT = %q", o.Kind)
	}
}

// The not-invited page needs no session.
func TestNotInvitedPageIsPublic(t *testing.T) {
	srv := newTestServer(t)
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, httptest.NewRequest(http.MethodGet, auth.NotInvitedPath, nil))
	if rr.Code != http.StatusForbidden || !strings.Contains(rr.Body.String(), "is not this team's owner") {
		t.Fatalf("code = %d, body = %s", rr.Code, rr.Body.String())
	}
}

// Logout clears the session cookie under the name the middleware reads.
func TestLogoutWithoutSignInClearsTheSuffixedCookie(t *testing.T) {
	srv := newTestServer(t)
	srv.AuthMW.CookieSuffix = "home"
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/auth/logout", nil))
	var cleared bool
	for _, c := range rr.Result().Cookies() {
		if c.Name == "kivali_session_home" && c.MaxAge < 0 {
			cleared = true
		}
	}
	if !cleared {
		t.Errorf("logout set %v", rr.Result().Cookies())
	}
}
