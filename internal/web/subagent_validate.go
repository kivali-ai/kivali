// Package web (subagent_validate.go) — what a dispatch must look like
// before anything is spawned.
//
// Shared by both dispatch paths. A rule enforced on one of them is a
// rule a sub-lead can route around, and the two paths are reached by
// different callers, so the checks live in one place rather than twice.

package web

import (
	"fmt"
	"strings"

	"github.com/kivali-ai/kivali/internal/provider"
)

// validateTasks rejects a batch before any subagent is spawned.
//
// Failing here costs the agent a tool error it can read and correct in
// the same turn. Failing later costs a subagent that spawns, dies on a
// driver error, and reports minutes afterwards as a failed job with a
// message about flags the agent never saw — for a mistake it could have
// fixed immediately.
func validateTasks(p provider.Provider, tasks []SubagentTaskInput) error {
	for i, t := range tasks {
		if strings.TrimSpace(t.Description) == "" {
			return fmt.Errorf("tasks[%d].description is required", i)
		}
		if strings.TrimSpace(t.Prompt) == "" {
			return fmt.Errorf("tasks[%d].prompt is required", i)
		}
		if err := validateTaskModel(p, i, t.Model); err != nil {
			return err
		}
		if err := validateTaskEffort(p, i, resolveSubagentModel(p, t.Model), t.Effort); err != nil {
			return err
		}
	}
	return nil
}

// validateTaskModel checks a per-task model against what the provider
// offers (Models).
//
// Rejected rather than silently defaulted: an unusable model means the
// agent was reaching for a specific cost or capability and did not get
// it — quietly running its cheap grep on the expensive default, or its
// hard reasoning on the cheap one, with nothing anywhere to say so.
// Better to hand back the list and let it choose again.
//
// Empty is valid and means the provider's subagent default — see
// resolveSubagentModel.
func validateTaskModel(p provider.Provider, i int, model string) error {
	model = strings.TrimSpace(model)
	if model == "" {
		return nil
	}
	ids := make([]string, 0, len(p.Models()))
	for _, m := range p.Models() {
		if m.ID == model {
			return nil
		}
		ids = append(ids, m.ID)
	}
	return fmt.Errorf("tasks[%d].model %q is not available — pick one of: %s (or omit it for the default)",
		i, model, strings.Join(ids, ", "))
}

// validateTaskEffort checks a per-task effort against what the task's
// model offers (Provider.Effort). The schema's enum is every effort any
// model takes, so a level one model offers can still be named for a
// model that does not; that is refused with the model's own list. Empty
// is valid and means the model's default.
func validateTaskEffort(p provider.Provider, i int, model, effort string) error {
	effort = strings.TrimSpace(effort)
	if effort == "" {
		return nil
	}
	if _, ok := p.Effort(model, effort); ok {
		return nil
	}
	m, _ := p.Resolve(model)
	if len(m.Efforts) == 0 {
		return fmt.Errorf("tasks[%d].effort %q: %s takes no effort — omit it", i, effort, model)
	}
	return fmt.Errorf("tasks[%d].effort %q is not available for %s — pick one of: %s (or omit it for the default)",
		i, effort, model, strings.Join(provider.EffortIDs(m.Efforts), ", "))
}

// resolveSubagentModel turns what the agent asked for into what runs:
// the model as given, or the provider's subagent default when it left
// the field empty. Applied ONCE, at registration, and the resolved
// value is what the job record, the pod spec, meta.json and every chip
// carry — so the panel never shows a blank where the pod quietly picked
// the default.
//
// The companion for effort is resolveSubagentEffort; the two are
// applied side by side in launch.
func resolveSubagentModel(p provider.Provider, model string) string {
	if m := strings.TrimSpace(model); m != "" {
		return m
	}
	return p.Defaults().Subagent
}

// resolveSubagentEffort is the effort a task on model runs at: effort
// when the model offers it, else the model's default ("" for a model
// with no reasoning control).
func resolveSubagentEffort(p provider.Provider, model, effort string) string {
	e, _ := p.Effort(model, strings.TrimSpace(effort))
	return e.ID
}
