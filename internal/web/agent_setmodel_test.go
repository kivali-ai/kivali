package web

import (
	"cmp"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/kivali-ai/kivali/internal/agent"
	"github.com/kivali-ai/kivali/internal/provider"
	"github.com/kivali-ai/kivali/internal/store"
	"github.com/kivali-ai/kivali/internal/web/apitypes"
)

// The per-agent model and effort pickers post to
// POST /api/v1/agents/{slug}/model and /effort, and read what they
// offer and what is current from GET /api/v1/agents/{slug}/chat. These
// drive the full stack (session middleware included) against the mock
// provider and assert both halves: the value round-trips through the
// store, and the chat view the picker is drawn from reports it as
// current. The Claude catalog's own rows are tested in
// internal/claudeagent.

// postAgentSetting posts {"<field>": value} to the agent's model or
// effort endpoint.
func postAgentSetting(t *testing.T, srv *Server, slug, field, value string) *httptest.ResponseRecorder {
	t.Helper()
	body, err := json.Marshal(map[string]string{field: value})
	if err != nil {
		t.Fatal(err)
	}
	return apiDo(t, srv, http.MethodPost, "/api/v1/agents/"+slug+"/"+field, string(body), nil)
}

// agentChatView is GET /api/v1/agents/{slug}/chat, which carries the
// picker's options and the current model and effort.
func agentChatView(t *testing.T, srv *Server, slug string) apitypes.Chat {
	t.Helper()
	rr := apiDo(t, srv, http.MethodGet, "/api/v1/agents/"+slug+"/chat", "", nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("chat %s: code = %d body = %s", slug, rr.Code, rr.Body.String())
	}
	return decodeAPI[apitypes.Chat](t, rr)
}

// offered finds id among the picker's options.
func offered(c apitypes.Chat, id string) (apitypes.ModelOption, bool) {
	for _, m := range c.Models {
		if m.ID == id {
			return m, true
		}
	}
	return apitypes.ModelOption{}, false
}

func seedAliceModel(t *testing.T, srv *Server, model string) {
	t.Helper()
	if err := srv.Store.CreateAgent(store.Agent{Slug: "alice", Role: "Eng", ReportsTo: "ceo", Model: model}, "k"); err != nil {
		t.Fatalf("seed: %v", err)
	}
}

// TestSetModelPersistsAndRenders: the user-visible symptom guarded is
// "I pick a model but the picker still shows the default", so the chat
// view's current model, not just the store read, is the contract.
func TestSetModelPersistsAndRenders(t *testing.T) {
	srv := newTestServer(t)
	srv.AgentModel = provider.MockModelLarge // cluster default
	seedAliceModel(t, srv, "")

	if rr := postAgentSetting(t, srv, "alice", "model", provider.MockModelSmall); rr.Code != http.StatusOK {
		t.Fatalf("set-model: code = %d body = %s", rr.Code, rr.Body.String())
	}
	if a, _ := srv.Store.GetAgent("alice"); a.Model != provider.MockModelSmall {
		t.Fatalf("persisted model = %q, want %s", a.Model, provider.MockModelSmall)
	}
	if c := agentChatView(t, srv, "alice"); c.CurrentModel != provider.MockModelSmall {
		t.Errorf("current model = %q, want %s", c.CurrentModel, provider.MockModelSmall)
	}
}

// TestModelPickerRowsComeFromTheProvider: an agent with no per-agent
// override reads as running the fleet default model, named by its
// concrete id rather than a separate "default" row, and each option is
// the provider's row: its label, window, efforts and provider name.
func TestModelPickerRowsComeFromTheProvider(t *testing.T) {
	srv := newTestServer(t)
	srv.AgentModel = provider.MockModelLarge // fleet default
	seedAliceModel(t, srv, "")

	c := agentChatView(t, srv, "alice")
	if c.CurrentModel != provider.MockModelLarge {
		t.Errorf("inherit agent current model = %q, want the fleet default", c.CurrentModel)
	}
	if _, ok := offered(c, "default"); ok {
		t.Error("picker offers a separate default row")
	}
	p := provider.MockProvider{}
	if len(c.Models) != len(p.Models()) {
		t.Fatalf("picker = %+v, want the provider's %d models", c.Models, len(p.Models()))
	}
	for i, want := range p.Models() {
		got := c.Models[i]
		if got.ID != want.ID || got.Label != want.Label || !got.Current || got.Legacy ||
			got.ContextWindow != want.ContextWindow || got.Provider != "mock" || len(got.Efforts) != len(want.Efforts) {
			t.Errorf("option %d = %+v, want the provider's row %+v", i, got, want)
		}
		for j, e := range want.Efforts {
			if g := got.Efforts[j]; g.ID != e.ID || g.Label != e.Label || g.Default != e.Default {
				t.Errorf("option %s effort %d = %+v, want %+v", want.ID, j, g, e)
			}
		}
	}
	if m, _ := offered(c, provider.MockModelSmall); m.Efforts == nil || len(m.Efforts) != 0 {
		t.Errorf("a model with no reasoning control carries efforts %#v, want an empty list", m.Efforts)
	}
}

// TestSetEffortPersistsAndRenders mirrors the model picker: an effort
// the agent's model offers round-trips and reads as current, one it
// does not offer is refused leaving the prior value, and "default"
// clears the override so the model's default reads as current.
func TestSetEffortPersistsAndRenders(t *testing.T) {
	srv := newTestServer(t)
	srv.AgentModel = provider.MockModelLarge
	seedAliceModel(t, srv, "")

	if rr := postAgentSetting(t, srv, "alice", "effort", "low"); rr.Code != http.StatusOK {
		t.Fatalf("set-effort: code = %d body = %s", rr.Code, rr.Body.String())
	}
	if a, _ := srv.Store.GetAgent("alice"); a.Effort != "low" {
		t.Fatalf("persisted effort = %q, want low", a.Effort)
	}
	if c := agentChatView(t, srv, "alice"); c.CurrentEffort != "low" {
		t.Errorf("current effort = %q, want low", c.CurrentEffort)
	}
	if rr := postAgentSetting(t, srv, "alice", "effort", "turbo"); rr.Code != http.StatusBadRequest {
		t.Errorf("unsupported effort: code = %d, want 400", rr.Code)
	}
	if a, _ := srv.Store.GetAgent("alice"); a.Effort != "low" {
		t.Errorf("effort changed on rejected input = %q, want low", a.Effort)
	}
	if rr := postAgentSetting(t, srv, "alice", "effort", "default"); rr.Code != http.StatusOK {
		t.Fatalf("clear-effort: code = %d", rr.Code)
	}
	if a, _ := srv.Store.GetAgent("alice"); a.Effort != "" {
		t.Errorf("effort after default = %q, want empty (inherit)", a.Effort)
	}
	if c := agentChatView(t, srv, "alice"); c.CurrentEffort != "high" {
		t.Errorf("inherit agent current effort = %q, want the model's default (high)", c.CurrentEffort)
	}
}

// TestSetEffortFollowsTheAgentsModel: an effort is validated against
// the agent's own model, so a model with no reasoning control refuses
// every level and reports no current effort.
func TestSetEffortFollowsTheAgentsModel(t *testing.T) {
	srv := newTestServer(t)
	seedAliceModel(t, srv, provider.MockModelSmall)
	if rr := postAgentSetting(t, srv, "alice", "effort", "low"); rr.Code != http.StatusBadRequest {
		t.Errorf("effort on a model without efforts: code = %d, want 400", rr.Code)
	}
	if c := agentChatView(t, srv, "alice"); c.CurrentEffort != "" {
		t.Errorf("current effort = %q, want none for a model without efforts", c.CurrentEffort)
	}
}

// TestChatBubbleRendersModelAndEffortPills: a recorded reply carries
// the model (as the provider's label) and effort it ran at, so the chat
// can say what each reply cost.
func TestChatBubbleRendersModelAndEffortPills(t *testing.T) {
	srv := newTestServer(t)
	seedAliceModel(t, srv, "")
	if err := srv.Store.AppendChatMessage("alice", store.ChatMessage{
		Role: store.RoleSent, Kind: "direct_chat", Content: "done", Model: provider.MockModelRetired, Effort: "low",
	}); err != nil {
		t.Fatalf("append: %v", err)
	}
	c := agentChatView(t, srv, "alice")
	if len(c.Rows) != 1 {
		t.Fatalf("rows = %+v", c.Rows)
	}
	r := c.Rows[0]
	if r.Model == nil || *r.Model != "Mock Large 0" || r.Effort == nil || *r.Effort != "low" {
		t.Errorf("reply model/effort = %v/%v, want Mock Large 0/low", r.Model, r.Effort)
	}
}

// TestAgentSetModelRefusesRetiredRows: a model the provider still
// resolves but no longer offers is not newly assignable (boot would
// move such a pin to the lineage's current row, so the picker would
// promise something it does not deliver), and a prefix of an offered
// id is not mistaken for it.
func TestAgentSetModelRefusesRetiredRows(t *testing.T) {
	srv := newTestServer(t)
	seedAliceModel(t, srv, "")
	for _, refused := range []string{provider.MockModelRetired, "mock-larg", provider.MockModelLarge + "x"} {
		if rr := postAgentSetting(t, srv, "alice", "model", refused); rr.Code != http.StatusBadRequest {
			t.Errorf("%s newly assignable: code = %d, body = %s", refused, rr.Code, rr.Body.String())
		}
	}
	c := agentChatView(t, srv, "alice")
	if _, ok := offered(c, provider.MockModelRetired); ok {
		t.Errorf("picker offers the retired %s", provider.MockModelRetired)
	}
}

// TestAgentSetModelLegacyPinIsGrandfathered covers an agent pinned to
// a model the picker does not offer. The chat view must still
// say what the agent runs (as a trailing legacy option, with the
// provider's label when it still knows the id), the value must survive
// a round-trip, and the grandfather is scoped to the agent that already
// holds the pin.
func TestAgentSetModelLegacyPinIsGrandfathered(t *testing.T) {
	for _, pin := range []string{provider.MockModelRetired, "some-model-nobody-knows"} {
		t.Run(pin, func(t *testing.T) {
			srv := newTestServer(t)
			seedAliceModel(t, srv, pin)

			c := agentChatView(t, srv, "alice")
			if c.CurrentModel != pin {
				t.Errorf("current model = %q, want the legacy pin", c.CurrentModel)
			}
			m, ok := offered(c, pin)
			if !ok || !m.Legacy || m.Current || c.Models[len(c.Models)-1].ID != pin {
				t.Errorf("legacy pin option = %+v (offered %v), want a trailing legacy row", m, ok)
			}
			if want := provider.Label(provider.MockProvider{}, pin); m.Label != want {
				t.Errorf("legacy label = %q, want %q", m.Label, want)
			}
			if rr := postAgentSetting(t, srv, "alice", "model", pin); rr.Code != http.StatusOK {
				t.Errorf("re-posting the legacy pin: code = %d, body = %s", rr.Code, rr.Body.String())
			}
			if err := srv.Store.CreateAgent(store.Agent{Slug: "bob", Role: "Analyst"}, "k"); err != nil {
				t.Fatalf("seed bob: %v", err)
			}
			if rr := postAgentSetting(t, srv, "bob", "model", pin); rr.Code != http.StatusBadRequest {
				t.Errorf("dropped model newly assignable to another agent: code = %d", rr.Code)
			}
		})
	}
}

func TestAgentSetModelDefaultClearsToEmpty(t *testing.T) {
	srv := newTestServer(t)
	seedAliceModel(t, srv, provider.MockModelSmall)
	if rr := postAgentSetting(t, srv, "alice", "model", "default"); rr.Code != http.StatusOK {
		t.Fatalf("code = %d", rr.Code)
	}
	if a, _ := srv.Store.GetAgent("alice"); a.Model != "" {
		t.Errorf("model should be cleared to empty (use cluster default), got %q", a.Model)
	}
}

func TestAgentSetModelRejectsUnsupported(t *testing.T) {
	srv := newTestServer(t)
	seedAliceModel(t, srv, "")
	if rr := postAgentSetting(t, srv, "alice", "model", "mock-prerelease-not-real"); rr.Code != http.StatusBadRequest {
		t.Errorf("unsupported model should 400, got %d body=%s", rr.Code, rr.Body.String())
	}
}

func TestAgentSetModelRejectsCEOSlug(t *testing.T) {
	srv := newTestServer(t)
	if rr := postAgentSetting(t, srv, agent.CEOSlug, "model", "default"); rr.Code != http.StatusBadRequest {
		t.Errorf("CEO slug should be rejected, got %d", rr.Code)
	}
}

// TestTurnRequestCarriesTheResolvedEffort: the request a turn sends the
// driver carries an effort valid for its model — the stored level when
// the model offers it, else the model's default — never the raw stored
// value. The same resolution the chat header shows.
func TestTurnRequestCarriesTheResolvedEffort(t *testing.T) {
	for _, tc := range []struct{ stored, want string }{
		{"low", "low"},
		{"max", "high"},  // not offered by mock-large: its default
		{"HIGH", "high"}, // miscased: not an id it offers
		{"", "high"},
	} {
		t.Run(cmp.Or(tc.stored, "unset"), func(t *testing.T) {
			srv := newChatServer(t)
			if err := srv.Store.CreateAgent(store.Agent{Slug: "alice", Role: "Analyst", ReportsTo: "chief-of-staff",
				Model: provider.MockModelLarge, Effort: tc.stored}, "# role"); err != nil {
				t.Fatal(err)
			}
			fake := installFakeAgentPod(t, srv, "alice")
			req := sendMessage(t, srv, fake, "hello")
			if req.Model != provider.MockModelLarge || req.Effort != tc.want {
				t.Errorf("request model/effort = %q/%q, want %s/%q", req.Model, req.Effort, provider.MockModelLarge, tc.want)
			}
		})
	}
}

// TestLegacyPinCarriesItsStoredEffort: for a pin the provider does not
// know, the picker's legacy row offers exactly the agent's stored effort
// (labelled with its id) and the API accepts exactly that value back for
// that agent, so the chip and the API agree. With no stored effort the
// row offers none and the chip hides.
func TestLegacyPinCarriesItsStoredEffort(t *testing.T) {
	const pin = "some-model-nobody-knows"
	srv := newTestServer(t)
	if err := srv.Store.CreateAgent(store.Agent{Slug: "alice", Role: "Eng", ReportsTo: "ceo", Model: pin, Effort: "turbo"}, "k"); err != nil {
		t.Fatal(err)
	}
	c := agentChatView(t, srv, "alice")
	m, ok := offered(c, pin)
	if !ok || !m.Legacy || len(m.Efforts) != 1 || m.Efforts[0].ID != "turbo" || m.Efforts[0].Label != "turbo" || !m.Efforts[0].Default {
		t.Fatalf("legacy row = %+v, want the stored effort as its one option", m)
	}
	if c.CurrentEffort != "turbo" {
		t.Errorf("current effort = %q, want the stored one", c.CurrentEffort)
	}
	if rr := postAgentSetting(t, srv, "alice", "effort", "turbo"); rr.Code != http.StatusOK {
		t.Errorf("re-posting the legacy effort: code = %d, body = %s", rr.Code, rr.Body.String())
	}
	if rr := postAgentSetting(t, srv, "alice", "effort", "ludicrous"); rr.Code != http.StatusBadRequest {
		t.Errorf("an effort neither offered nor stored: code = %d, want 400", rr.Code)
	}

	// Scoped to the agent that holds it.
	if err := srv.Store.CreateAgent(store.Agent{Slug: "bob", Role: "Eng", ReportsTo: "ceo", Model: pin}, "k"); err != nil {
		t.Fatal(err)
	}
	if rr := postAgentSetting(t, srv, "bob", "effort", "turbo"); rr.Code != http.StatusBadRequest {
		t.Errorf("another agent's legacy effort newly assignable: code = %d", rr.Code)
	}
	if m, _ := offered(agentChatView(t, srv, "bob"), pin); m.Efforts == nil || len(m.Efforts) != 0 {
		t.Errorf("legacy row with no stored effort = %+v, want no efforts", m)
	}
}
