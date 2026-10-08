package provider

import "context"

// Provider is what the core knows about the models a driver runs.
//
// One provider serves an org. Model ids stay opaque strings in stored
// data (agent.yaml pins, usage rows, transcripts, subagent records) and
// are resolved through this interface when they are read: a label for
// the page, a price for a rollup, a context window for the fill gauge.
type Provider interface {
	Name() string                           // "claude": labels and storage keys
	Models() []ModelInfo                    // what a person may pick, in display order
	Resolve(id string) (ModelInfo, bool)    // any stored id (dated, retired, suffixed, with or without a "<name>:" prefix) to the row that prices and labels it
	Current(id string) string               // where a pin on this id's lineage should move (boot, restore); id itself when nothing newer
	Price(id string, u TokenUsage) float64  // USD for a usage row; 0 for an unknown id
	Defaults() Defaults                     // models for agent, subagent and summary work
	Effort(model, id string) (Effort, bool) // validate id for model; false and the model's default when not offered
	Credentials() Credentials
}

// ModelInfo is one model as the core sees it.
type ModelInfo struct {
	// ID is the id a person picks and a pin stores. Resolve answers
	// with the row's ID, which may differ from the id it was asked
	// about (a dated snapshot, a retired id resolves to its own row).
	ID string
	// Label is the model's name in words ("Opus 5.5").
	Label string
	// Lineage is the family a pin follows across versions ("opus").
	Lineage string
	// Current is true for the row a person may newly pick.
	Current bool
	// ContextWindow is the tokens the model holds, for the id as
	// asked (a suffix asking for a larger window counts).
	ContextWindow int
	// Efforts are the reasoning levels this model offers, low to
	// high, one of them Default. Empty: no reasoning control.
	Efforts []Effort
}

// Effort is one reasoning level a model offers.
type Effort struct {
	ID, Label string
	Default   bool
}

// Defaults are the models the core uses when nothing more specific
// was chosen.
type Defaults struct{ Agent, Subagent, Summary string }

// Credentials is how a provider's calls are authorized, as far as the
// core needs to know: whether it is signed in and to what, how a person
// signs it in, and where it keeps that sign-in.
type Credentials interface {
	Status(ctx context.Context) (CredentialStatus, error) // signed in?, who, billed to what; never spends a model call
	Guidance() string                                     // one plain sentence: who can sign the server in (no commands, variables or paths)
	HomeDir() string                                      // the directory under the data volume the provider keeps its state in ("claude-home")
}

// CredentialStatus is how the provider is signed in.
type CredentialStatus struct {
	// Present is true when the provider is signed in.
	Present bool
	// Who is the signed-in identity when the provider reports one.
	Who string
	// Billing names, in words, what calls are billed to ("Claude Max",
	// "Amazon Bedrock"); empty when not signed in or not known.
	Billing string
}

// Label is id's name in words: the resolved row's label, or id itself
// when p does not know it.
func Label(p Provider, id string) string {
	if id == "" {
		return ""
	}
	if m, ok := p.Resolve(id); ok && m.Label != "" {
		return m.Label
	}
	return id
}

// EffortIDs are the ids of efforts, in order.
func EffortIDs(efforts []Effort) []string {
	out := make([]string, 0, len(efforts))
	for _, e := range efforts {
		out = append(out, e.ID)
	}
	return out
}

// DefaultEffort is the effort marked Default in efforts, or the zero
// Effort when there is none.
func DefaultEffort(efforts []Effort) Effort {
	for _, e := range efforts {
		if e.Default {
			return e
		}
	}
	return Effort{}
}
