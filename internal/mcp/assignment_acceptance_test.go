package mcp

import (
	"strings"
	"testing"
)

// The tools carry the two fields: the release's table in assignment_view,
// the counts in assignment_list, the refusal that names the parent's
// items, the add/remove path on assignment_update, and the warning on
// assignment_close.
func TestAssignmentToolsAcceptanceItems(t *testing.T) {
	s, tr := assignmentFixture(t)
	ack := assignmentCall(t, s, tr, "chief-of-staff", AssignmentCreateToolName, `{"title":"v3 release","assignee":"alice","acceptance":["login page reviewed","export page reviewed","settings page reviewed","version tagged","release notes"]}`)
	if !strings.Contains(ack, `Done when: "login page reviewed", "export page reviewed", "settings page reviewed", "version tagged", "release notes".`) {
		t.Fatalf("create ack = %q", ack)
	}
	// A child naming an item the parent does not declare is refused
	// with the parent's list.
	assignmentRefused(t, s, tr, "alice", AssignmentCreateToolName, `{"title":"Review billing page","assignee":"bob","parent":1,"satisfies":["step D"]}`,
		`#1's Done-when conditions are "login page reviewed", "export page reviewed", "settings page reviewed", "version tagged", "release notes"`)
	// Three children claim three items.
	ack = assignmentCall(t, s, tr, "alice", AssignmentCreateToolName, `{"title":"Review login page","assignee":"bob","parent":1,"satisfies":["login page reviewed"]}`)
	if !strings.Contains(ack, `Part of #1; counts toward "login page reviewed".`) {
		t.Fatalf("child ack = %q", ack)
	}
	assignmentCall(t, s, tr, "alice", AssignmentCreateToolName, `{"title":"Review export page","assignee":"bob","parent":1,"satisfies":["export page reviewed"]}`)
	assignmentCall(t, s, tr, "alice", AssignmentCreateToolName, `{"title":"Tag the version","assignee":"bob","parent":1,"satisfies":["version tagged"]}`)
	for _, id := range []string{"2", "3", "4"} {
		assignmentCall(t, s, tr, "bob", AssignmentCloseToolName, `{"id":`+id+`,"resolution":"done","outcome":"done"}`)
	}
	// The list row shows the counts and not the child counts.
	list := assignmentCall(t, s, tr, "alice", AssignmentListToolName, `{}`)
	if !strings.Contains(list, `#1 ready "v3 release" — assignee alice, creator chief-of-staff, done when 3 met / 0 claimed-open / 2 unclaimed`) || strings.Contains(list, "child") {
		t.Fatalf("list:\n%s", list)
	}
	// The view shows the table.
	view := assignmentCall(t, s, tr, "alice", AssignmentViewToolName, `{"id":1}`)
	for _, want := range []string{
		"done when (3 met / 0 claimed-open / 2 unclaimed):",
		`[met] "login page reviewed" — by #2 (bob)`,
		`[unclaimed] "settings page reviewed" — nothing opened names it`,
		`[unclaimed] "release notes" — nothing opened names it`,
		`#2 "Review login page" — assignee bob, closed (done), counts toward "login page reviewed"`,
	} {
		if !strings.Contains(view, want) {
			t.Errorf("view missing %q:\n%s", want, view)
		}
	}
	// A child's view names the parent item it satisfies.
	view = assignmentCall(t, s, tr, "bob", AssignmentViewToolName, `{"id":2}`)
	if !strings.Contains(view, `counts toward: "login page reviewed" of #1`) {
		t.Errorf("child view:\n%s", view)
	}
	// assignment_update adds and removes items by name, and refuses to
	// drop one an open child claims.
	assignmentCall(t, s, tr, "alice", AssignmentCreateToolName, `{"title":"Notes","assignee":"bob","parent":1,"satisfies":["release notes"]}`)
	assignmentRefused(t, s, tr, "alice", AssignmentUpdateToolName, `{"id":1,"remove_acceptance":["release notes"]}`, `"release notes" is claimed by open #5`)
	ack = assignmentCall(t, s, tr, "alice", AssignmentUpdateToolName, `{"id":1,"add_acceptance":["billing page reviewed"],"remove_acceptance":["settings page reviewed"],"note":"C is descoped, D is in"}`)
	if !strings.Contains(ack, `Done-when conditions now "login page reviewed", "export page reviewed", "version tagged", "release notes", "billing page reviewed"`) {
		t.Fatalf("update ack = %q", ack)
	}
	// A child's claim comes off by name; the last one gone leaves it
	// satisfying nothing.
	ack = assignmentCall(t, s, tr, "bob", AssignmentUpdateToolName, `{"id":5,"remove_satisfies":["release notes"]}`)
	if !strings.Contains(ack, "counts toward nothing now") {
		t.Fatalf("clear ack = %q", ack)
	}
	assignmentCall(t, s, tr, "bob", AssignmentCloseToolName, `{"id":5,"resolution":"dropped","outcome":"no notes this rev"}`)
	// Closing as done with two unmet succeeds and returns the warning.
	ack = assignmentCall(t, s, tr, "alice", AssignmentCloseToolName, `{"id":1,"resolution":"done","outcome":"Shipped."}`)
	if !strings.Contains(ack, `Closed #1 "v3 release" as done. Warning: closed as done with 2 Done-when conditions unmet: "release notes", "billing page reviewed".`) {
		t.Fatalf("close ack = %q", ack)
	}
	view = assignmentCall(t, s, tr, "alice", AssignmentViewToolName, `{"id":1}`)
	if !strings.Contains(view, `alice closed as done with 2 Done-when conditions unmet ("release notes", "billing page reviewed"): Shipped.`) {
		t.Errorf("closed view log:\n%s", view)
	}
}
