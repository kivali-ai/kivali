package provider

import (
	"context"
	"strings"
)

// The mock provider's models. Two are on offer: MockModelLarge with
// three efforts and MockModelSmall with none. MockModelRetired is
// priced and labelled but not offered; its lineage's current row is
// MockModelLarge.
const (
	MockModelLarge   = "mock-large"
	MockModelSmall   = "mock-small"
	MockModelRetired = "mock-large-0"
)

// mockRate is USD per million tokens: input, output, cache write,
// cache read.
type mockRate struct{ in, out, write, read float64 }

var mockEfforts = []Effort{
	{ID: "low", Label: "Low"},
	{ID: "medium", Label: "Medium"},
	{ID: "high", Label: "High", Default: true},
}

var mockRows = []struct {
	info ModelInfo
	rate mockRate
}{
	{ModelInfo{ID: MockModelSmall, Label: "Mock Small", Lineage: "small", Current: true, ContextWindow: 200_000},
		mockRate{1, 5, 1.25, 0.10}},
	{ModelInfo{ID: MockModelLarge, Label: "Mock Large", Lineage: "large", Current: true, ContextWindow: 1_000_000, Efforts: mockEfforts},
		mockRate{10, 50, 12.5, 1}},
	{ModelInfo{ID: MockModelRetired, Label: "Mock Large 0", Lineage: "large", ContextWindow: 200_000, Efforts: mockEfforts},
		mockRate{5, 25, 6.25, 0.5}},
}

// MockProvider is a Provider with a fixed two-model catalog and fixed
// prices, so core tests do not depend on any real provider's catalog.
// The zero value is ready; Cred and CredErr script Credentials().Status.
type MockProvider struct {
	Cred    CredentialStatus
	CredErr error
}

var _ Provider = MockProvider{}

// Name is "mock".
func (MockProvider) Name() string { return "mock" }

// Models are MockModelSmall then MockModelLarge.
func (MockProvider) Models() []ModelInfo {
	var out []ModelInfo
	for _, r := range mockRows {
		if r.info.Current {
			out = append(out, cloneInfo(r.info))
		}
	}
	return out
}

// Resolve knows the three mock ids, with or without a "mock:" prefix.
func (p MockProvider) Resolve(id string) (ModelInfo, bool) {
	id = strings.TrimPrefix(id, p.Name()+":")
	for _, r := range mockRows {
		if r.info.ID == id {
			return cloneInfo(r.info), true
		}
	}
	return ModelInfo{}, false
}

// Current moves MockModelRetired to MockModelLarge.
func (p MockProvider) Current(id string) string {
	m, ok := p.Resolve(id)
	if !ok || m.Current {
		return id
	}
	for _, r := range mockRows {
		if r.info.Current && r.info.Lineage == m.Lineage {
			return r.info.ID
		}
	}
	return id
}

// Price is u at the model's fixed rate; 0 for an unknown id.
func (p MockProvider) Price(id string, u TokenUsage) float64 {
	id = strings.TrimPrefix(id, p.Name()+":")
	for _, r := range mockRows {
		if r.info.ID == id {
			return (float64(u.InputTokens)*r.rate.in +
				float64(u.OutputTokens)*r.rate.out +
				float64(u.CacheCreateTokens)*r.rate.write +
				float64(u.CacheReadTokens)*r.rate.read) / 1_000_000
		}
	}
	return 0
}

// Defaults: MockModelLarge for agents, MockModelSmall (no efforts) for
// subagents and summaries.
func (MockProvider) Defaults() Defaults {
	return Defaults{Agent: MockModelLarge, Subagent: MockModelSmall, Summary: MockModelSmall}
}

// Effort accepts the model's own effort ids. An unknown model offers
// nothing.
func (p MockProvider) Effort(model, id string) (Effort, bool) {
	m, _ := p.Resolve(model)
	for _, e := range m.Efforts {
		if e.ID == id {
			return e, true
		}
	}
	return DefaultEffort(m.Efforts), false
}

// Credentials reports Cred.
func (p MockProvider) Credentials() Credentials { return mockCredentials(p) }

type mockCredentials MockProvider

func (c mockCredentials) Status(context.Context) (CredentialStatus, error) {
	return c.Cred, c.CredErr
}

func (mockCredentials) Guidance() string {
	return "Whoever runs this server can sign it in to the mock provider."
}
func (mockCredentials) HomeDir() string { return "mock-home" }

func cloneInfo(m ModelInfo) ModelInfo {
	m.Efforts = append([]Effort(nil), m.Efforts...)
	return m
}
