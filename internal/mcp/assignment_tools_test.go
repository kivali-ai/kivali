package mcp

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/kivali-ai/kivali/internal/agent"
	"github.com/kivali-ai/kivali/internal/messaging"
	"github.com/kivali-ai/kivali/internal/store"
	"github.com/kivali-ai/kivali/internal/tracker"
)

// assignmentFixture is a store with four agents and a tracker wired to a
// bare messenger, so wakes queue in message_queue.json and CEO-bound
// ones land in the CEO's chat, exactly as production routes them.
func assignmentFixture(t *testing.T) (*store.FSStore, *tracker.Service) {
	t.Helper()
	s, err := store.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range []store.Agent{
		{Slug: "ceo", Role: "CEO"},
		{Slug: "chief-of-staff", Role: "Chief of Staff", ReportsTo: "ceo"},
		{Slug: "alice", Role: "Analyst", ReportsTo: "chief-of-staff"},
		{Slug: "bob", Role: "Engineer", ReportsTo: "chief-of-staff"},
	} {
		if err := s.CreateAgent(a, "# role"); err != nil {
			t.Fatal(err)
		}
	}
	return s, tracker.New(s, messaging.New(s, nil), messaging.DeliveryHooks{})
}

func assignmentCall(t *testing.T, s *store.FSStore, tr *tracker.Service, caller, tool, input string) string {
	t.Helper()
	body, isErr := DispatchStateToolInProcess(StateDispatchDeps{Store: s, Slug: caller, Tracker: tr}, tool, json.RawMessage(input))
	if isErr {
		t.Fatalf("%s %s: tool error: %s", tool, input, body)
	}
	return body
}

func assignmentRefused(t *testing.T, s *store.FSStore, tr *tracker.Service, caller, tool, input, contains string) {
	t.Helper()
	body, isErr := DispatchStateToolInProcess(StateDispatchDeps{Store: s, Slug: caller, Tracker: tr}, tool, json.RawMessage(input))
	if !isErr {
		t.Fatalf("%s %s: expected a refusal containing %q, got success: %s", tool, input, contains, body)
	}
	if !strings.Contains(body, contains) {
		t.Fatalf("%s %s: refusal %q does not contain %q", tool, input, body, contains)
	}
	if strings.Contains(body, "internal error") {
		t.Fatalf("%s %s: a rule refusal was reported as an internal error: %s", tool, input, body)
	}
}

func TestAssignmentToolsEndToEnd(t *testing.T) {
	s, tr := assignmentFixture(t)
	// CoS files an epic for alice.
	ack := assignmentCall(t, s, tr, "chief-of-staff", AssignmentCreateToolName, `{"title":"v3 program","description":"Ship v3.","assignee":"alice"}`)
	if !strings.Contains(ack, `Opened #1 "v3 program" — assignee alice, ready.`) || !strings.Contains(ack, "Woken: alice (when the owner releases it).") {
		t.Fatalf("create ack = %q", ack)
	}
	// Alice, the assignee, decomposes: a child for bob.
	ack = assignmentCall(t, s, tr, "alice", AssignmentCreateToolName, `{"title":"Build script","assignee":"bob","parent":1}`)
	if !strings.Contains(ack, "Part of #1.") || !strings.Contains(ack, "Woken: bob (when the owner releases it).") {
		t.Fatalf("child ack = %q", ack)
	}
	// Bob asks the CEO a question under his assignment: instant CEO inbox.
	ack = assignmentCall(t, s, tr, "bob", AssignmentCreateToolName, `{"title":"Which CI runner?","description":"A or B?","assignee":"ceo","parent":2}`)
	if !strings.Contains(ack, "Woken: the owner (in their inbox now).") {
		t.Fatalf("question ack = %q", ack)
	}
	// Bob cannot close as done with the question open; the refusal
	// names the child.
	assignmentRefused(t, s, tr, "bob", AssignmentCloseToolName, `{"id":2,"resolution":"done","outcome":"x"}`, "open parts #3")
	// Alice cannot reassign her own assignment; the refusal names the move.
	assignmentRefused(t, s, tr, "alice", AssignmentUpdateToolName, `{"id":1,"assignee":"bob","note":"take it"}`, "does not hand an assignment on")
	// Listing: bob's default view is his open assignments.
	list := assignmentCall(t, s, tr, "bob", AssignmentListToolName, `{}`)
	if !strings.Contains(list, "(assignee=bob, status=open): 1 match") || !strings.Contains(list, `#2 blocked by #3 "Build script" — assignee bob, creator alice, part of #1, 1 part (1 open); blocks #1`) {
		t.Fatalf("bob's list:\n%s", list)
	}
	// Everyone's.
	list = assignmentCall(t, s, tr, "bob", AssignmentListToolName, `{"assignee":"any"}`)
	if !strings.Contains(list, "3 match") {
		t.Fatalf("any list:\n%s", list)
	}
	// View shows the tree both ways and the log.
	view := assignmentCall(t, s, tr, "alice", AssignmentViewToolName, `{"id":2}`)
	for _, want := range []string{
		`Assignment #2 "Build script"`, "state: blocked by #3", "part of: #1 \"v3 program\" (assignee alice, blocked by #2)",
		"parts (1, 1 open):", `#3 "Which CI runner?" — assignee ceo, ready`, "holds up: #1", "Description:\n(none)",
		"alice created, assigned to bob",
	} {
		if !strings.Contains(view, want) {
			t.Errorf("view missing %q:\n%s", want, view)
		}
	}
	// The CEO answers (through the same dispatcher a web handler would
	// not use, but the rules are the same); bob's #2 becomes ready.
	ack = assignmentCall(t, s, tr, "ceo", AssignmentCloseToolName, `{"id":3,"resolution":"done","outcome":"Chip B."}`)
	if !strings.Contains(ack, "Closed #3 \"Which CI runner?\" as done. Now ready: #2 \"Build script\". Woken: bob (now).") {
		t.Fatalf("answer ack = %q", ack)
	}
	// Bob finishes; alice is woken for the rollup.
	ack = assignmentCall(t, s, tr, "bob", AssignmentCloseToolName, `{"id":2,"resolution":"done","outcome":"Script at /files/artifacts/public/build.md"}`)
	if !strings.Contains(ack, "Now ready: #1 \"v3 program\". Woken: alice (when the owner releases it).") {
		t.Fatalf("finish ack = %q", ack)
	}
	// Update ack names each change.
	ack = assignmentCall(t, s, tr, "chief-of-staff", AssignmentUpdateToolName, `{"id":1,"title":"v3 programme","note":"spelling"}`)
	if !strings.Contains(ack, `Updated #1 "v3 programme": amended title. Now ready, assignee alice. Woken: alice (when the owner releases it).`) {
		t.Fatalf("update ack = %q", ack)
	}
	// Reopen by the creator.
	ack = assignmentCall(t, s, tr, "alice", AssignmentReopenToolName, `{"id":2,"note":"The script does not build v3."}`)
	if !strings.Contains(ack, `Reopened #2 "Build script" — assignee bob, ready. Woken: bob (when the owner releases it).`) {
		t.Fatalf("reopen ack = %q", ack)
	}
	// Bad input is a tool error, not a crash.
	assignmentRefused(t, s, tr, "alice", AssignmentListToolName, `{"status":"blocked"}`, "status must be open, closed or all")
	assignmentRefused(t, s, tr, "alice", AssignmentViewToolName, `{"id":99}`, "no assignment #99")
}

func TestAssignmentToolsWithoutATracker(t *testing.T) {
	s, _ := assignmentFixture(t)
	body, isErr := DispatchStateToolInProcess(StateDispatchDeps{Store: s, Slug: "alice"}, AssignmentListToolName, json.RawMessage(`{}`))
	if !isErr || !strings.Contains(body, "not configured") {
		t.Fatalf("no tracker: %v %q", isErr, body)
	}
}

// The MCP definitions are built from the native constants, so the two
// transports cannot drift; this pins that they are all registered and
// that the writes are not marked read-only (the CLI would run them
// concurrently).
func TestAssignmentToolsMatchAcrossTransportsAndMarkWrites(t *testing.T) {
	s, tr := assignmentFixture(t)
	d := fakeStateDispatcher{deps: StateDispatchDeps{Store: s, Slug: "alice", Tracker: tr}}
	mcpSide := map[string]Tool{}
	for _, tool := range StateTools(d, false) {
		mcpSide[tool.Name] = tool
	}
	for _, native := range agent.AssignmentTools() {
		got, ok := mcpSide[native.Name]
		if !ok {
			t.Errorf("%s is not registered on the MCP side", native.Name)
			continue
		}
		if got.Description != native.Description {
			t.Errorf("%s describes itself differently per transport", native.Name)
		}
		if string(got.InputSchema) != string(native.InputSchema) {
			t.Errorf("%s has a different schema per transport", native.Name)
		}
		wantReadOnly := native.Name == AssignmentListToolName || native.Name == AssignmentViewToolName
		if got.ReadOnly != wantReadOnly {
			t.Errorf("%s read-only = %v, want %v", native.Name, got.ReadOnly, wantReadOnly)
		}
	}
}
