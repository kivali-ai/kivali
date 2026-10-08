package message

import (
	"encoding/json"
	"testing"
)

func TestAllToolsComplete(t *testing.T) {
	tools := AllTools()
	if len(tools) != 3 {
		t.Fatalf("AllTools count = %d, want 3", len(tools))
	}
	names := map[string]bool{}
	for _, tt := range tools {
		if tt.Name == "" || tt.Description == "" || len(tt.Schema) == 0 {
			t.Errorf("incomplete tool: %+v", tt)
		}
		if names[tt.Name] {
			t.Errorf("duplicate tool name: %s", tt.Name)
		}
		names[tt.Name] = true
		if !isValidJSON(tt.Schema) {
			t.Errorf("tool %s: schema is not valid JSON", tt.Name)
		}
	}
	want := []string{ToolNotice, ToolCEOApprovalRequest, ToolCEONotification}
	for _, n := range want {
		if !names[n] {
			t.Errorf("missing tool %s", n)
		}
	}
}

// The org-mutating propose_* family is CoS-only: CoS sees AllTools
// plus every ProposeTools entry, and nobody else sees any of them.
// Driven off ProposeTools rather than a hardcoded list so adding a
// sixth proposal can't quietly skip the "non-CoS must not see it"
// half of the assertion.
func TestToolsForGatesProposalsToChiefOfStaff(t *testing.T) {
	cos := ToolsFor(ChiefOfStaff, true)
	other := ToolsFor("market-analyst", false)
	if want := len(AllTools()) + len(ProposeTools()); len(cos) != want {
		t.Fatalf("CoS should get %d publish tools (AllTools + ProposeTools); got %d", want, len(cos))
	}
	if len(other) != len(AllTools()) {
		t.Fatalf("non-CoS should get %d publish tools; got %d", len(AllTools()), len(other))
	}
	cosNames := map[string]bool{}
	for _, tt := range cos {
		cosNames[tt.Name] = true
	}
	otherNames := map[string]bool{}
	for _, tt := range other {
		otherNames[tt.Name] = true
	}
	for _, p := range ProposeTools() {
		if !cosNames[p.Name] {
			t.Errorf("CoS missing %s", p.Name)
		}
		if otherNames[p.Name] {
			t.Errorf("non-CoS should not see %s", p.Name)
		}
	}
	// Spot-check the two that carry the highest blast radius, so an
	// empty ProposeTools still fails this test.
	for _, n := range []string{ToolProposeHire, ToolProposeOffboard} {
		if !cosNames[n] {
			t.Errorf("CoS missing %s", n)
		}
	}
}

func TestAsClaudeToolsPreservesShape(t *testing.T) {
	got := AsClaudeTools(AllTools())
	if len(got) != 3 {
		t.Fatalf("len = %d", len(got))
	}
	for i, ct := range got {
		if ct.Name == "" || len(ct.InputSchema) == 0 {
			t.Errorf("tool[%d] = %+v", i, ct)
		}
	}
}

func isValidJSON(b json.RawMessage) bool {
	var v any
	return json.Unmarshal(b, &v) == nil
}
