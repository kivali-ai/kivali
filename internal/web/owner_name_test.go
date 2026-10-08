package web

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/kivali-ai/kivali/internal/mcp"
	"github.com/kivali-ai/kivali/internal/seed"
	"github.com/kivali-ai/kivali/internal/store"
	"github.com/kivali-ai/kivali/internal/web/apitypes"
)

// systemPrompt is what the agent's next turn would send as its system
// prompt.
func systemPrompt(t *testing.T, srv *Server, slug string) string {
	t.Helper()
	a, err := srv.Store.GetAgent(slug)
	if err != nil {
		t.Fatal(err)
	}
	actx, err := srv.Runtime.BuildChatContext(context.Background(), a, nil)
	if err != nil {
		t.Fatal(err)
	}
	var parts []string
	for _, b := range actx.BuildRequest().System {
		parts = append(parts, b.Text)
	}
	return strings.Join(parts, "\n\n")
}

// release releases a queued message to alice with a markup comment and
// returns the body alice received.
func releaseWithMarkup(t *testing.T, srv *Server, comment string) string {
	t.Helper()
	relPath := seedPendingRelease(t, srv, "please draft the Q3 plan")
	if rr := postRelease(t, srv, relPath, comment); rr.Code != http.StatusOK {
		t.Fatalf("release = %d: %s", rr.Code, rr.Body.String())
	}
	got, err := srv.Store.ReadMessage(filepath.Join(srv.Store.Root(), relPath))
	if err != nil {
		t.Fatal(err)
	}
	return got.Body
}

// With no name chosen, a work team's marker and label are CEO and a
// personal team's Owner; with one, both are the name. The prompt's
// owner section names the same marker the release writes.
func TestOwnerMarkerAndPromptLine(t *testing.T) {
	for _, tc := range []struct{ kind, name, label string }{
		{"work", "", "CEO"},
		{"work", "Jane", "Jane"},
		{"personal", "", "Owner"},
		{"personal", "Human Overlord", "Human Overlord"},
	} {
		t.Run(tc.kind+"/"+tc.name, func(t *testing.T) {
			srv, _ := newTurnServer(t)
			srv.Claude = nil
			if err := srv.Store.WriteTeamKind(tc.kind); err != nil {
				t.Fatal(err)
			}
			if err := srv.Store.WriteOwnerName(tc.name); err != nil {
				t.Fatal(err)
			}
			if body := releaseWithMarkup(t, srv, "include APAC"); !strings.Contains(body, "> "+tc.label+": include APAC") {
				t.Errorf("body = %q, want the marker > %s:", body, tc.label)
			}
			sys := systemPrompt(t, srv, "alice")
			if !strings.Contains(sys, "## The owner\n\n") || !strings.Contains(sys, "a line starting `> "+tc.label+":`") {
				t.Errorf("the system prompt does not name the marker > %s:\n%s", tc.label, sys)
			}
			if tc.name != "" && !strings.Contains(sys, "is called "+tc.name+". Address them and refer to them as "+tc.name+".") {
				t.Errorf("the system prompt does not name %s", tc.name)
			}
		})
	}
}

// A rename changes the next turn's owner section and the marker, and
// touches no stored text: the handbook and the role keep their bytes,
// and neither ever carried a name. The section says markup under the
// older markers is theirs too.
func TestOwnerRenameLandsNextTurnWithoutRewritingStoredText(t *testing.T) {
	srv, _ := newTurnServer(t)
	srv.Claude = nil
	if err := srv.Store.WriteTeamKind("work"); err != nil {
		t.Fatal(err)
	}
	if err := srv.Store.WriteHandbook(seed.HandbookFor("work")); err != nil {
		t.Fatal(err)
	}
	if err := srv.Store.WriteOwnerName("Jane"); err != nil {
		t.Fatal(err)
	}
	if body := releaseWithMarkup(t, srv, "first"); !strings.Contains(body, "> Jane: first") {
		t.Fatalf("body = %q", body)
	}
	handbookPath := filepath.Join(srv.Store.Root(), store.HandbookFilename)
	rolePath := filepath.Join(srv.Store.Root(), "agents", "alice", "role.md")
	before := map[string][]byte{}
	for _, p := range []string{handbookPath, rolePath} {
		b, err := os.ReadFile(p)
		if err != nil {
			t.Fatal(err)
		}
		before[p] = b
	}
	if sys := systemPrompt(t, srv, "alice"); !strings.Contains(sys, "is called Jane.") {
		t.Fatalf("before the name change:\n%s", sys)
	}

	mom := "Mom"
	orgOK[apitypes.Org](t, orgJSON(t, srv, http.MethodPut, "/api/v1/org", apitypes.OrgPut{Name: "Hearth", OwnerName: &mom}))

	sys := systemPrompt(t, srv, "alice")
	if !strings.Contains(sys, "is called Mom. Address them and refer to them as Mom.") || !strings.Contains(sys, "`> Mom:`") || strings.Contains(sys, "Jane") {
		t.Errorf("after the rename the owner section is not Mom's:\n%s", sys)
	}
	if !strings.Contains(sys, "Notes under `> CEO:`, `> Owner:` or a name they used before are theirs too.") {
		t.Error("the owner section does not cover markup under an older marker")
	}
	if body := releaseWithMarkup(t, srv, "second"); !strings.Contains(body, "> Mom: second") {
		t.Errorf("body after the rename = %q", body)
	}
	for p, b := range before {
		after, err := os.ReadFile(p)
		if err != nil {
			t.Fatal(err)
		}
		if string(after) != string(b) {
			t.Errorf("%s changed on a rename", p)
		}
		if strings.Contains(string(after), "Jane") || strings.Contains(string(after), "Mom") {
			t.Errorf("%s carries a name", p)
		}
	}
}

// Setup stores the seed text for the team's kind whatever the person is
// called: no name in it, and no CEO in a personal team's.
func TestAPISetupSeedCoSStoresNoName(t *testing.T) {
	for _, tc := range []struct{ kind, name string }{{"work", "Jane"}, {"personal", "Mom"}} {
		t.Run(tc.kind, func(t *testing.T) {
			var capture seedCapture
			srv, _ := newSetupServer(t, capture.client())
			if err := srv.SeedBranding("Hearth", tc.kind); err != nil {
				t.Fatal(err)
			}
			if err := srv.SeedOwnerName(tc.name); err != nil {
				t.Fatal(err)
			}
			st := getSetup(t, srv)
			if st.CoS.DefaultRoleMD != seed.ChiefOfStaffRoleFor(tc.kind) || st.CoS.DefaultHandbookMD != seed.HandbookFor(tc.kind) {
				t.Error("setup does not offer the kind's seed text")
			}
			req := apitypes.SeedCoSRequest{HandbookFromFiles: true}
			if rr := setupJSON(t, srv, http.MethodPost, "/api/v1/setup/seed-cos", req); rr.Code != http.StatusAccepted {
				t.Fatalf("POST seed-cos = %d: %s", rr.Code, rr.Body.String())
			}
			waitSeed(t, srv)
			h, _ := srv.Store.ReadHandbook()
			r, _ := srv.Store.ReadRole(cosSlug)
			hist, err := srv.Store.ReadChatHistory(cosSlug)
			if err != nil || len(hist) != 1 {
				t.Fatalf("chief of staff chat = %+v (err %v)", hist, err)
			}
			for what, text := range map[string]string{"handbook": h, "role": r, "first message": hist[0].Content} {
				if strings.Contains(text, tc.name) {
					t.Errorf("the stored %s carries the name %q", what, tc.name)
				}
				if tc.kind == "personal" && regexp.MustCompile(`\bCEO\b`).MatchString(text) {
					t.Errorf("the personal %s says CEO", what)
				}
			}
		})
	}
}

// KIVALI_OWNER_NAME is stored at boot when no name is stored, never
// over one, and every API that describes the org carries it.
func TestSeedOwnerNameStoresOnlyWhatIsUnset(t *testing.T) {
	srv, _ := newSetupServer(t, nil)
	if err := srv.SeedOwnerName("  "); err != nil {
		t.Fatal(err)
	}
	if st := getSetup(t, srv); st.Org.OwnerName != "" {
		t.Fatalf("blank seeded a name: %q", st.Org.OwnerName)
	}
	if err := srv.SeedOwnerName("  Mom "); err != nil {
		t.Fatal(err)
	}
	if st := getSetup(t, srv); st.Org.OwnerName != "Mom" {
		t.Errorf("setup owner_name = %q, want Mom", st.Org.OwnerName)
	}
	if err := srv.SeedOwnerName("Jane"); err != nil {
		t.Fatal(err)
	}
	if br, _ := srv.Store.ReadBranding(); br.OwnerName != "Mom" {
		t.Errorf("a second boot overwrote the name: %q", br.OwnerName)
	}
	if err := srv.SeedOwnerName(strings.Repeat("x", 41)); err != nil {
		t.Errorf("a long name at boot over a stored one should do nothing, got %v", err)
	}
}

// A name the person cleared or changed in the app is not seeded again
// at the next boot, though KIVALI_OWNER_NAME stays in the deployment.
func TestSeedOwnerNameDoesNotUndoTheApp(t *testing.T) {
	srv := newTestServer(t)
	if err := srv.SeedOwnerName("Mom"); err != nil {
		t.Fatal(err)
	}
	empty := ""
	if o := orgOK[apitypes.Org](t, orgJSON(t, srv, http.MethodPut, "/api/v1/org", apitypes.OrgPut{Name: "Hearth", OwnerName: &empty})); o.OwnerName != "" {
		t.Fatalf("clear = %q", o.OwnerName)
	}
	if err := srv.SeedOwnerName("Mom"); err != nil {
		t.Fatal(err)
	}
	if br, _ := srv.Store.ReadBranding(); br.OwnerName != "" || !br.OwnerNameSet {
		t.Errorf("a restart after clearing: branding %+v, want no name, set", br)
	}
	jane := "Jane"
	orgOK[apitypes.Org](t, orgJSON(t, srv, http.MethodPut, "/api/v1/org", apitypes.OrgPut{Name: "Hearth", OwnerName: &jane}))
	if err := srv.SeedOwnerName("Mom"); err != nil {
		t.Fatal(err)
	}
	if br, _ := srv.Store.ReadBranding(); br.OwnerName != "Jane" {
		t.Errorf("a restart after a change: %q, want Jane", br.OwnerName)
	}

	// Cleared before boot ever seeded (never set, then set to empty in
	// the app): boot does not seed either.
	other := newTestServer(t)
	if err := other.Store.WriteOwnerName(""); err != nil {
		t.Fatal(err)
	}
	if err := other.SeedOwnerName("Mom"); err != nil {
		t.Fatal(err)
	}
	if br, _ := other.Store.ReadBranding(); br.OwnerName != "" {
		t.Errorf("seeded over a name cleared in the app: %q", br.OwnerName)
	}
}

// The org settings change the name; a long one is refused and changes
// nothing; a PUT without it leaves it; an empty one clears it.
func TestAPIOrgOwnerName(t *testing.T) {
	srv := newTestServer(t)
	mom := "Mom"
	if o := orgOK[apitypes.Org](t, orgJSON(t, srv, http.MethodPut, "/api/v1/org", apitypes.OrgPut{Name: "Hearth", OwnerName: &mom})); o.OwnerName != "Mom" {
		t.Fatalf("after PUT = %+v", o)
	}
	long := strings.Repeat("x", 41)
	assertAPIError(t, orgJSON(t, srv, http.MethodPut, "/api/v1/org", apitypes.OrgPut{Name: "Renamed", OwnerName: &long}), http.StatusBadRequest)
	if o := orgOK[apitypes.Org](t, orgGet(t, srv, "/api/v1/org")); o.OwnerName != "Mom" || o.Name != "Hearth" {
		t.Errorf("a refused PUT changed the org: %+v", o)
	}
	if o := orgOK[apitypes.Org](t, orgJSON(t, srv, http.MethodPut, "/api/v1/org", apitypes.OrgPut{Name: "Hearth"})); o.OwnerName != "Mom" {
		t.Errorf("a PUT without owner_name changed it: %q", o.OwnerName)
	}
	empty := ""
	if o := orgOK[apitypes.Org](t, orgJSON(t, srv, http.MethodPut, "/api/v1/org", apitypes.OrgPut{Name: "Hearth", OwnerName: &empty})); o.OwnerName != "" {
		t.Errorf("an empty owner_name did not clear it: %q", o.OwnerName)
	}
}

// The org chart agents read labels the person with their name, or with
// the kind's default when they chose none.
func TestOwnerNameReachesTheOrgChart(t *testing.T) {
	srv := newTestServer(t)
	chart := func() string {
		body, isErr := mcp.DispatchStateToolInProcess(mcp.StateDispatchDeps{Store: srv.Store, Slug: cosSlug}, mcp.GetOrgChartToolName, nil)
		if isErr {
			t.Fatal(body)
		}
		return body
	}
	if got := chart(); !strings.Contains(got, "- ceo (CEO, human)") {
		t.Errorf("unnamed work chart:\n%s", got)
	}
	if err := srv.Store.WriteTeamKind("personal"); err != nil {
		t.Fatal(err)
	}
	if got := chart(); !strings.Contains(got, "- ceo (Owner, human)") {
		t.Errorf("unnamed personal chart:\n%s", got)
	}
	if err := srv.Store.WriteOwnerName("Mom"); err != nil {
		t.Fatal(err)
	}
	if got := chart(); !strings.Contains(got, "- ceo (Mom, human)") {
		t.Errorf("named chart:\n%s", got)
	}
}
