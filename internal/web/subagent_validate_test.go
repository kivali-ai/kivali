package web

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/kivali-ai/kivali/internal/mcp"
	"github.com/kivali-ai/kivali/internal/provider"
)

// TestUnavailableModelIsRefusedAtDispatch pins the fast failure. Passed
// through, a bad id reaches the driver and kills the subagent at spawn
// — surfacing minutes later as a failed job quoting a driver error for
// a mistake the agent could have fixed instantly.
func TestUnavailableModelIsRefusedAtDispatch(t *testing.T) {
	svc, calls := modelEffortService(t)

	_, err := svc.StartBatch(context.Background(), "alice", taskArgs("mock-huge-9-turbo", "high"))
	if err == nil {
		t.Fatal("dispatch accepted a model that does not exist")
	}
	// The error has to be actionable on its own: the agent cannot see
	// the picker, so naming the alternatives is the whole point.
	if !strings.Contains(err.Error(), "not available") {
		t.Errorf("error does not say the model is unavailable: %v", err)
	}
	for _, want := range (provider.MockProvider{}).Models() {
		if !strings.Contains(err.Error(), want.ID) {
			t.Errorf("error does not list %s among the choices: %v", want.ID, err)
		}
	}

	select {
	case c := <-calls:
		t.Fatalf("spawned %q despite the refusal", c.SubagentID)
	default:
	}
}

// TestNestedUnavailableModelIsRefused covers the sub-lead's path. A
// rule enforced on one dispatch path only is a rule one tier can route
// around.
func TestNestedUnavailableModelIsRefused(t *testing.T) {
	svc, calls := modelEffortService(t)
	registerTestJob(svc, "lead1", "alice", 1, subagentJobRunning)

	_, err := svc.RunNestedBatch(context.Background(), "alice", "lead1", taskArgs("gpt-5", "high"))
	if err == nil {
		t.Fatal("nested dispatch accepted a model that does not exist")
	}
	select {
	case c := <-calls:
		t.Fatalf("spawned %q despite the refusal", c.SubagentID)
	default:
	}
}

// TestEveryAdvertisedModelIsAccepted is the other half: the schema enum
// and the validator read the same provider, so anything the agent is
// offered must work — every model, and every effort that model offers.
// If these ever diverge, the agent is being shown choices that get
// rejected.
func TestEveryAdvertisedModelIsAccepted(t *testing.T) {
	p := provider.MockProvider{}
	for _, m := range p.Models() {
		if err := validateTaskModel(p, 0, m.ID); err != nil {
			t.Errorf("advertised model %s is refused by the validator: %v", m.ID, err)
		}
		for _, e := range m.Efforts {
			if err := validateTaskEffort(p, 0, m.ID, e.ID); err != nil {
				t.Errorf("advertised effort %s for %s is refused: %v", e.ID, m.ID, err)
			}
		}
	}
	// Omission stays valid — core resolves the provider's defaults.
	if err := validateTaskModel(p, 0, ""); err != nil {
		t.Errorf("empty model should mean the fleet default, got: %v", err)
	}
	if err := validateTaskEffort(p, 0, provider.MockModelSmall, ""); err != nil {
		t.Errorf("empty effort should mean the model's default, got: %v", err)
	}
}

// dispatchSchema is the part of a dispatch tool's input schema the
// server validates against.
type dispatchSchema struct {
	Properties struct {
		Tasks struct {
			Items struct {
				Properties map[string]struct {
					Enum        []string `json:"enum"`
					Description string   `json:"description"`
				} `json:"properties"`
			} `json:"items"`
		} `json:"tasks"`
	} `json:"properties"`
}

// TestSchemaAdvertisesExactlyTheProvidersModels closes the loop between
// what the agent is told and what it is allowed, on both dispatch
// tools. Two lists that drift produce the worst version of this bug: a
// model the tool suggests and the server refuses. The effort enum is
// every level any model offers, and its description names which levels
// each model takes, since the enum alone cannot.
func TestSchemaAdvertisesExactlyTheProvidersModels(t *testing.T) {
	p := provider.MockProvider{}
	for name, raw := range map[string]json.RawMessage{
		"subagent":        mcp.SubagentTool(mcp.SubagentToolConfig{Provider: p}).InputSchema,
		"nested subagent": mcp.NestedSubagentTool(mcp.NestedSubagentToolConfig{Provider: p}).InputSchema,
	} {
		var schema dispatchSchema
		if err := json.Unmarshal(raw, &schema); err != nil {
			t.Fatalf("%s schema is not valid JSON: %v", name, err)
		}
		props := schema.Properties.Tasks.Items.Properties
		got := props["model"].Enum
		if len(got) != len(p.Models()) {
			t.Fatalf("%s schema advertises %d models, the provider offers %d", name, len(got), len(p.Models()))
		}
		for i, m := range p.Models() {
			if got[i] != m.ID {
				t.Errorf("%s model enum[%d] = %q, the provider offers %q", name, i, got[i], m.ID)
			}
		}
		if !strings.Contains(props["model"].Description, provider.Label(p, p.Defaults().Subagent)) {
			t.Errorf("%s model description %q does not name the default", name, props["model"].Description)
		}

		efforts := props["effort"].Enum
		if strings.Join(efforts, ",") != "low,medium,high" {
			t.Errorf("%s effort enum = %v, want every level any model offers", name, efforts)
		}
		desc := props["effort"].Description
		for _, want := range []string{provider.MockModelLarge + ": low, medium, high (default high)", provider.MockModelSmall + ": none"} {
			if !strings.Contains(desc, want) {
				t.Errorf("%s effort description %q does not say %q", name, desc, want)
			}
		}
	}
}
