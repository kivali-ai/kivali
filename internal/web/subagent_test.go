package web

import (
	"reflect"
	"testing"
)

func TestIsSubagentToolName(t *testing.T) {
	cases := []struct {
		in   string
		want bool
	}{
		{"subagent", true},
		{"file_view", false},
		{"publish_notice", false},
		{"", false},
	}
	for _, tc := range cases {
		if got := isSubagentToolName(tc.in); got != tc.want {
			t.Errorf("isSubagentToolName(%q) = %v, want %v", tc.in, got, tc.want)
		}
	}
}

func TestSubagentTranscriptIDsFromOutput(t *testing.T) {
	body := `Subagent batch as of 2026-04-28 21:03:23 UTC — 2 tasks:

=== task 1: alpha ===
id: 96329059 · transcript: /agents/alice/subagents/96329059
ok done

=== task 2: beta ===
id: deadbeef · transcript: /agents/alice/subagents/deadbeef
also ok
`
	got := subagentTranscriptIDsFromOutput(body)
	want := []string{"96329059", "deadbeef"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

func TestSubagentTranscriptIDsTolerateMissingID(t *testing.T) {
	// Early failure produces a task section with no id line; the
	// helper returns "" so the slot still aligns with the input
	// task array.
	body := `Subagent batch as of 2026-04-28 21:03:23 UTC — 2 tasks:

=== task 1: alpha ===
[error: alloc id: rng broken]

=== task 2: beta ===
id: deadbeef · transcript: /agents/alice/subagents/deadbeef
ok
`
	got := subagentTranscriptIDsFromOutput(body)
	want := []string{"", "deadbeef"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}
