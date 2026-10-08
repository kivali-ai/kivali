package agentpod

import (
	"encoding/json"
	"reflect"
	"testing"
)

// TestSubagentSpecRoundTrip exercises the wire shape end-to-end so
// renames or json-tag changes get caught: marshal a populated
// SubagentSpec on a ChatTurnEvent, unmarshal, every field comes
// back unchanged.
func TestSubagentSpecRoundTrip(t *testing.T) {
	want := ChatTurnEvent{
		TurnID: "turn-7",
		Slug:   "alice",
		Source: "subagent",
		Model:  "claude-sonnet-4-6",
		Kind:   ChatTurnKindSubagent,
		Subagent: &SubagentSpec{
			SubagentID:   "ab12cd34",
			Description:  "summarize the doc",
			Model:        "claude-sonnet-4-6",
			SystemPrompt: "you are a subagent",
			UserPrompt:   "do it",
			Tools:        SubagentTools(1),
			Depth:        1,
		},
	}
	body, err := json.Marshal(want)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var got ChatTurnEvent
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.Kind != ChatTurnKindSubagent {
		t.Errorf("Kind = %q, want %q", got.Kind, ChatTurnKindSubagent)
	}
	if got.Subagent == nil {
		t.Fatal("Subagent missing after round trip")
	}
	if !reflect.DeepEqual(got.Subagent, want.Subagent) {
		t.Errorf("Subagent mismatch:\n got %+v\nwant %+v", got.Subagent, want.Subagent)
	}
}
