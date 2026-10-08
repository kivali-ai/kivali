package mcp

import (
	"strings"
	"testing"
)

// hold on assignment_update pauses the tree through the tool, and every
// read surface says so.
func TestHoldThroughTheTools(t *testing.T) {
	s, tr := assignmentFixture(t)
	assignmentCall(t, s, tr, "alice", AssignmentCreateToolName, `{"title":"Epic","assignee":"bob"}`)
	assignmentCall(t, s, tr, "bob", AssignmentCreateToolName, `{"title":"Part","assignee":"chief-of-staff","parent":1}`)

	assignmentRefused(t, s, tr, "bob", AssignmentUpdateToolName, `{"id":1,"hold":true,"note":"x"}`, "only #1's creator (alice) or the owner can put it on hold")
	assignmentRefused(t, s, tr, "alice", AssignmentUpdateToolName, `{"id":1,"hold":true}`, "note is required")
	assignmentRefused(t, s, tr, "alice", AssignmentUpdateToolName, `{"id":1,"hold":true,"title":"Epic 2","note":"x"}`, "a hold is a change on its own")

	out := assignmentCall(t, s, tr, "alice", AssignmentUpdateToolName, `{"id":1,"hold":true,"note":"Rethinking."}`)
	if want := `Updated #1 "Epic": put on hold with everything under it. Now on hold, assignee bob. Told to stop: bob (when the owner releases it), chief-of-staff (when the owner releases it).`; out != want {
		t.Fatalf("hold = %q", out)
	}
	list := assignmentCall(t, s, tr, "chief-of-staff", AssignmentListToolName, `{}`)
	if !strings.Contains(list, `#2 on hold under #1 "Part"`) {
		t.Fatalf("list = %s", list)
	}
	if list = assignmentCall(t, s, tr, "chief-of-staff", AssignmentListToolName, `{"ready":true}`); !strings.Contains(list, "0 match") {
		t.Fatalf("ready list under a hold = %s", list)
	}
	view := assignmentCall(t, s, tr, "bob", AssignmentViewToolName, `{"id":1}`)
	if !strings.Contains(view, "state: on hold\n") || !strings.Contains(view, "alice put on hold: Rethinking.") || !strings.Contains(view, `#2 "Part" — assignee chief-of-staff, on hold under #1`) {
		t.Fatalf("view = %s", view)
	}
	out = assignmentCall(t, s, tr, "alice", AssignmentUpdateToolName, `{"id":1,"hold":false,"note":"Go."}`)
	if want := `Updated #1 "Epic": resumed with everything under it. Now blocked by #2, assignee bob. Woken: bob (when the owner releases it), chief-of-staff (when the owner releases it).`; out != want {
		t.Fatalf("resume = %q", out)
	}
	out = assignmentCall(t, s, tr, "ceo", AssignmentUpdateToolName, `{"id":1,"hold":true,"note":"Pause."}`)
	if !strings.HasSuffix(out, `Told to stop: bob (now if mid-turn, else with their next wake), chief-of-staff (now if mid-turn, else with their next wake).`) {
		t.Fatalf("CEO hold = %q", out)
	}
}
