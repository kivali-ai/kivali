package mcp

import (
	"encoding/json"
	"strings"

	"github.com/kivali-ai/kivali/internal/provider"
)

// modelEffortProperties renders the `model` and `effort` properties of
// a dispatch tool's task schema from the provider: the model enum is
// Models() in display order, the effort enum every effort id any model
// offers, and the effort description says which ids each model takes,
// since the enum alone cannot. The server validates a task's effort
// against its model at call time (Provider.Effort). effortAdvice is
// the tool's own sentence on when to move off the default.
//
// Rendered from data, not a hand-typed copy, so the tool advertises
// exactly what the provider accepts: a free-form string left the agent
// inferring model ids from its own training data — the one source
// guaranteed to be stale — and a wrong guess reached the driver and
// failed the run minutes later.
//
// A provider whose models offer no effort gets no effort property.
func modelEffortProperties(p provider.Provider, effortAdvice string) string {
	models := p.Models()
	ids := make([]string, 0, len(models))
	for _, m := range models {
		ids = append(ids, m.ID)
	}
	var b strings.Builder
	b.WriteString(`"model": {"type": "string", "enum": `)
	b.WriteString(jsonEnum(ids))
	b.WriteString(`, "description": `)
	b.WriteString(jsonString("optional model for this task. Omit for the default (" +
		provider.Label(p, p.Defaults().Subagent) + "). Cheapest first; pick the smallest that can do the job reliably."))
	b.WriteString(`}`)

	efforts, perModel := effortsByModel(models)
	if len(efforts) == 0 {
		return b.String()
	}
	b.WriteString(",\n          ")
	b.WriteString(`"effort": {"type": "string", "enum": `)
	b.WriteString(jsonEnum(efforts))
	b.WriteString(`, "description": `)
	b.WriteString(jsonString("optional reasoning depth; " + perModel + ". Omit for the model's default. " + effortAdvice))
	b.WriteString(`}`)
	return b.String()
}

// effortsByModel is the union of the models' effort ids in first-seen
// order, and a phrase naming which ids each model takes, grouping
// models that take the same set ("a, b: low, high (default high); c:
// none").
func effortsByModel(models []provider.ModelInfo) ([]string, string) {
	var (
		union  []string
		seen   = map[string]bool{}
		groups []string
		byKey  = map[string]int{}
		names  [][]string
	)
	for _, m := range models {
		for _, e := range m.Efforts {
			if !seen[e.ID] {
				seen[e.ID] = true
				union = append(union, e.ID)
			}
		}
		key := "none"
		if len(m.Efforts) > 0 {
			key = strings.Join(provider.EffortIDs(m.Efforts), ", ")
			if d := provider.DefaultEffort(m.Efforts); d.ID != "" {
				key += " (default " + d.ID + ")"
			}
		}
		i, ok := byKey[key]
		if !ok {
			i = len(groups)
			byKey[key] = i
			groups = append(groups, key)
			names = append(names, nil)
		}
		names[i] = append(names[i], m.ID)
	}
	parts := make([]string, len(groups))
	for i, g := range groups {
		parts[i] = strings.Join(names[i], ", ") + ": " + g
	}
	return union, strings.Join(parts, "; ")
}

// jsonEnum renders a list as a JSON array for a schema enum.
func jsonEnum(values []string) string {
	if values == nil {
		values = []string{}
	}
	b, err := json.Marshal(values)
	if err != nil {
		// Marshalling a []string cannot fail; if it somehow did, an
		// absent enum is better than a malformed schema.
		return `[]`
	}
	return string(b)
}

// jsonString renders s as a JSON string literal.
func jsonString(s string) string {
	b, err := json.Marshal(s)
	if err != nil {
		return `""`
	}
	return string(b)
}
