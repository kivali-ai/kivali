package web

import (
	"cmp"
	"errors"
	"fmt"
	"strings"

	"github.com/kivali-ai/kivali/internal/provider"
	"github.com/kivali-ai/kivali/internal/web/apitypes"
)

// The web layer's questions about models, answered by s.Provider. A
// model id is an opaque string here: stored as written, resolved when
// read. Nothing in this package knows which provider runs underneath.

// fallbackContextWindow sizes the fill gauge for a model the provider
// does not know. Small on purpose: under-reporting capacity makes the
// ring warn early rather than read "calm" while the session compacts.
const fallbackContextWindow = 200_000

// modelLabel is id's name in words, or id itself when the provider does
// not know it; "" for "".
func (s *Server) modelLabel(id string) string {
	return provider.Label(s.Provider, id)
}

// modelContextWindow is the token capacity of id's context window, for
// the context-fill ring and the chat-rotation cue.
func (s *Server) modelContextWindow(id string) int {
	if m, ok := s.Provider.Resolve(id); ok && m.ContextWindow > 0 {
		return m.ContextWindow
	}
	return fallbackContextWindow
}

// modelOffered reports whether id is one of the models a person may
// pick, exactly as Models() spells it.
func (s *Server) modelOffered(id string) bool {
	for _, m := range s.Provider.Models() {
		if m.ID == id {
			return true
		}
	}
	return false
}

// legacyPin returns the agent's current model when the picker no
// longer offers it, else "".
//
// Boot moves a pin along its lineage (store.UpgradeModelPins through
// Provider.Current), so after a restart the only pins outside the list
// are the ones with nowhere to go — a model the provider never knew, or
// one whose whole lineage has left the picker. Those still have to stay
// visible or the picker lies: a pin missing from the list leaves
// nothing selected, and a select shows its FIRST option. The model list
// appends the live pin as a trailing entry marked legacy: honest about
// what the agent runs, not offered to anyone not already on it.
func (s *Server) legacyPin(selected string) string {
	if selected == "" || s.modelOffered(selected) {
		return ""
	}
	return selected
}

// effectiveEffort is the effort a run on model uses for the stored
// value effort: effort itself when the model offers it, else the
// model's default ("" for a model with no reasoning control). For a
// model the provider does not know (a legacy pin), the stored value as
// is: the legacy picker row offers exactly that, and the driver
// answers for an id its catalog lacks.
func (s *Server) effectiveEffort(model, effort string) string {
	if _, ok := s.Provider.Resolve(model); !ok {
		return effort
	}
	e, _ := s.Provider.Effort(model, effort)
	return e.ID
}

// apiModelOption is one model row as the API serves it.
func (s *Server) apiModelOption(m provider.ModelInfo, legacy bool) apitypes.ModelOption {
	efforts := make([]apitypes.EffortOption, 0, len(m.Efforts))
	for _, e := range m.Efforts {
		efforts = append(efforts, apitypes.EffortOption{ID: e.ID, Label: e.Label, Default: e.Default})
	}
	return apitypes.ModelOption{
		ID:            m.ID,
		Label:         m.Label,
		Legacy:        legacy,
		Current:       m.Current,
		ContextWindow: m.ContextWindow,
		Efforts:       efforts,
		Provider:      s.Provider.Name(),
	}
}

// modelOptions is the picker's list: every model on offer, then the
// agent's current pin as a legacy row when the list no longer offers
// it. A legacy pin the provider still resolves keeps its label, window
// and efforts. One it does not know carries its id as its label and,
// as its one effort, exactly the agent's stored effort (storedEffort,
// labelled with its id; none when empty) — what the agent runs at, and
// what setAgentEffort grandfathers, so the chip and the API agree.
func (s *Server) modelOptions(current, storedEffort string) []apitypes.ModelOption {
	models := s.Provider.Models()
	out := make([]apitypes.ModelOption, 0, len(models)+1)
	for _, m := range models {
		out = append(out, s.apiModelOption(m, false))
	}
	if legacy := s.legacyPin(current); legacy != "" {
		m, ok := s.Provider.Resolve(legacy)
		if !ok {
			m = provider.ModelInfo{Label: legacy, ContextWindow: fallbackContextWindow}
			if storedEffort != "" {
				m.Efforts = []provider.Effort{{ID: storedEffort, Label: storedEffort, Default: true}}
			}
		}
		m.ID, m.Current = legacy, false
		out = append(out, s.apiModelOption(m, true))
	}
	return out
}

// errUnsupportedSetting is setAgentModel's and setAgentEffort's refusal
// of a value the picker does not offer.
var errUnsupportedSetting = errors.New("unsupported")

// setAgentModel pins slug to model, or clears the pin for "" or
// "default" so the agent inherits the cluster-wide AgentModel. The
// agent's next model call picks it up. Serves POST
// /api/v1/agents/{slug}/model.
func (s *Server) setAgentModel(slug, model string) error {
	model = strings.TrimSpace(model)
	// Allow "default" (empty string in storage) so the agent inherits
	// the cluster-wide AgentModel config.
	if model != "" && model != "default" && !s.modelOffered(model) {
		// Grandfather the agent's existing pin. The model list carries
		// a retired model as a trailing "legacy" entry so the picker
		// doesn't misreport what the agent runs (see legacyPin); that
		// entry has to survive a round-trip, or re-sending it would 400
		// on a value the server itself offered. Scoped to this agent's
		// current value, so a retired model can't be newly assigned.
		if cur, err := s.Store.GetAgent(slug); err != nil || cur.Model != model {
			return fmt.Errorf("%w model: %s", errUnsupportedSetting, model)
		}
	}
	if model == "default" {
		model = ""
	}
	return s.Store.SetAgentModel(slug, model)
}

// setAgentEffort sets slug's reasoning level, or clears it for "" or
// "default" so the agent inherits its model's default. The level must
// be one the agent's model offers (Provider.Effort), or the agent's own
// stored effort: like setAgentModel's legacy pin, a value the server
// itself offered (a legacy row's one effort) survives a round-trip,
// and only for the agent that already holds it.
func (s *Server) setAgentEffort(slug, effort string) error {
	effort = strings.TrimSpace(effort)
	if effort == "default" {
		effort = ""
	}
	if effort != "" {
		model, stored := s.AgentModel, ""
		if a, err := s.Store.GetAgent(slug); err == nil {
			model, stored = cmp.Or(a.Model, s.AgentModel), a.Effort
		}
		if _, ok := s.Provider.Effort(model, effort); !ok && effort != stored {
			return fmt.Errorf("%w effort: %s", errUnsupportedSetting, effort)
		}
	}
	return s.Store.SetAgentEffort(slug, effort)
}
