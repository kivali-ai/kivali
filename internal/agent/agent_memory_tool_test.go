package agent

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/kivali-ai/kivali/internal/store"
)

type fakeAgentMemoryStore struct {
	memory        string
	readErr       error
	appendErr     error
	strReplaceErr error
	appendCalls   []appendCall
	replaceCalls  []replaceCall

	// The principles document, tracked separately so a test can
	// assert a principles tool never touched semantic memory.
	principles             string
	principlesAppendCalls  []appendCall
	principlesReplaceCalls []replaceCall
}

type appendCall struct{ slug, text string }
type replaceCall struct{ slug, old, new string }

func (f *fakeAgentMemoryStore) ReadAgentMemory(_ string) (string, error) {
	return f.memory, f.readErr
}

func (f *fakeAgentMemoryStore) AppendAgentMemory(slug, text string) error {
	f.appendCalls = append(f.appendCalls, appendCall{slug, text})
	return f.appendErr
}

func (f *fakeAgentMemoryStore) StrReplaceAgentMemory(slug, oldStr, newStr string) error {
	f.replaceCalls = append(f.replaceCalls, replaceCall{slug, oldStr, newStr})
	return f.strReplaceErr
}

func (f *fakeAgentMemoryStore) ReadAgentHabits(_ string) (string, error) {
	if f.principles == "" {
		return "", store.ErrNotFound
	}
	return f.principles, nil
}

func (f *fakeAgentMemoryStore) AppendAgentHabits(slug, text string) error {
	f.principlesAppendCalls = append(f.principlesAppendCalls, appendCall{slug, text})
	return nil
}

func (f *fakeAgentMemoryStore) StrReplaceAgentHabits(slug, oldStr, newStr string) error {
	f.principlesReplaceCalls = append(f.principlesReplaceCalls, replaceCall{slug, oldStr, newStr})
	return f.strReplaceErr
}

func TestIsAgentMemoryTool(t *testing.T) {
	cases := map[string]bool{
		AgentMemoryViewToolName:       true,
		AgentMemoryAppendToolName:     true,
		AgentMemoryStrReplaceToolName: true,
		AgentHabitsViewToolName:       true,
		AgentHabitsAppendToolName:     true,
		AgentHabitsStrReplaceToolName: true,
		"file_view":                   false,
		"publish_notice":              false,
		"":                            false,
	}
	for name, want := range cases {
		if got := IsAgentMemoryTool(name); got != want {
			t.Errorf("IsAgentMemoryTool(%q) = %v, want %v", name, got, want)
		}
	}
}

// TestIsAgentHabitsMutation pins the rotation gate's predicate: only
// the two habits WRITES are gated. Reading habits,
// and every semantic-memory tool, stays available mid-chat.
func TestIsAgentHabitsMutation(t *testing.T) {
	cases := map[string]bool{
		AgentHabitsAppendToolName:     true,
		AgentHabitsStrReplaceToolName: true,
		AgentHabitsViewToolName:       false,
		AgentMemoryAppendToolName:     false,
		AgentMemoryStrReplaceToolName: false,
		"file_create":                 false,
	}
	for name, want := range cases {
		if got := IsAgentHabitsMutation(name); got != want {
			t.Errorf("IsAgentHabitsMutation(%q) = %v, want %v", name, got, want)
		}
	}
}

func TestAgentMemoryToolsShape(t *testing.T) {
	tools := AgentMemoryTools()
	if len(tools) != 6 {
		t.Fatalf("want 6 tools, got %d", len(tools))
	}
	names := map[string]bool{}
	for _, tl := range tools {
		names[tl.Name] = true
		if tl.Description == "" {
			t.Errorf("tool %q missing description", tl.Name)
		}
		if len(tl.InputSchema) == 0 {
			t.Errorf("tool %q missing schema", tl.Name)
		}
		if !json.Valid(tl.InputSchema) {
			t.Errorf("tool %q schema is not valid JSON: %s", tl.Name, tl.InputSchema)
		}
	}
	for _, want := range []string{
		AgentMemoryViewToolName, AgentMemoryAppendToolName, AgentMemoryStrReplaceToolName,
		AgentHabitsViewToolName, AgentHabitsAppendToolName, AgentHabitsStrReplaceToolName,
	} {
		if !names[want] {
			t.Errorf("tool %q missing; names = %v", want, names)
		}
	}
}

// TestDispatchPrinciplesToolsTargetPrinciples pins that the principles
// trio edits agent_memory_habits.md and never agent_memory.md —
// the two documents share a dispatcher, and a crossed wire here would
// silently file a rule as a fact (or worse, overwrite one with the
// other).
func TestDispatchPrinciplesToolsTargetPrinciples(t *testing.T) {
	f := &fakeAgentMemoryStore{memory: "a fact\n", principles: "- Verify before claiming. Why: ...\n"}

	body, isErr := DispatchAgentMemoryTool(f, "alice", AgentHabitsViewToolName, json.RawMessage(`{}`))
	if isErr || body != f.principles {
		t.Fatalf("principles view = (%q, %v), want the principles body", body, isErr)
	}

	in, _ := json.Marshal(map[string]string{"text": "- Ship the fix, then the doc. Why: docs drift."})
	body, isErr = DispatchAgentMemoryTool(f, "alice", AgentHabitsAppendToolName, in)
	if isErr {
		t.Fatalf("principles append errored: %s", body)
	}
	if len(f.principlesAppendCalls) != 1 || len(f.appendCalls) != 0 {
		t.Fatalf("append routed to principles=%d memory=%d, want 1/0", len(f.principlesAppendCalls), len(f.appendCalls))
	}
	if !strings.Contains(body, "habits") {
		t.Errorf("append ack should name the document: %q", body)
	}

	in, _ = json.Marshal(map[string]string{"old_str": "- Verify before claiming. Why: ...", "new_str": ""})
	body, isErr = DispatchAgentMemoryTool(f, "alice", AgentHabitsStrReplaceToolName, in)
	if isErr {
		t.Fatalf("principles str_replace errored: %s", body)
	}
	if len(f.principlesReplaceCalls) != 1 || len(f.replaceCalls) != 0 {
		t.Fatalf("str_replace routed to principles=%d memory=%d, want 1/0", len(f.principlesReplaceCalls), len(f.replaceCalls))
	}

	// An empty principles file tells the agent to append, not to
	// retry a str_replace that can never match.
	empty := &fakeAgentMemoryStore{strReplaceErr: store.ErrNotFound}
	body, isErr = DispatchAgentMemoryTool(empty, "alice", AgentHabitsStrReplaceToolName, in)
	if !isErr || !strings.Contains(body, "append") {
		t.Errorf("str_replace on empty principles = (%q, %v), want an error pointing at append", body, isErr)
	}
}

func TestDispatchViewReturnsContents(t *testing.T) {
	f := &fakeAgentMemoryStore{memory: "## State\n- thing 1\n- thing 2\n"}
	body, isErr := DispatchAgentMemoryTool(f, "alice", AgentMemoryViewToolName, json.RawMessage(`{}`))
	if isErr {
		t.Fatalf("unexpected error body: %s", body)
	}
	if body != f.memory {
		t.Errorf("body = %q, want %q", body, f.memory)
	}
}

func TestDispatchViewEmptyMemory(t *testing.T) {
	f := &fakeAgentMemoryStore{readErr: store.ErrNotFound}
	body, isErr := DispatchAgentMemoryTool(f, "alice", AgentMemoryViewToolName, json.RawMessage(`{}`))
	if isErr {
		t.Fatalf("empty memory should not surface as error: %s", body)
	}
	if !strings.Contains(body, "empty") {
		t.Errorf("body should describe empty state; got %q", body)
	}
}

func TestDispatchViewWhitespaceOnly(t *testing.T) {
	f := &fakeAgentMemoryStore{memory: "   \n\n"}
	body, isErr := DispatchAgentMemoryTool(f, "alice", AgentMemoryViewToolName, json.RawMessage(`{}`))
	if isErr {
		t.Fatalf("whitespace-only memory should not error: %s", body)
	}
	if !strings.Contains(body, "empty") {
		t.Errorf("whitespace-only memory should report empty; got %q", body)
	}
}

func TestDispatchAppendSuccess(t *testing.T) {
	f := &fakeAgentMemoryStore{}
	body, isErr := DispatchAgentMemoryTool(f, "alice", AgentMemoryAppendToolName, json.RawMessage(`{"text":"note X"}`))
	if isErr {
		t.Fatalf("unexpected error body: %s", body)
	}
	if !strings.Contains(body, "appended") {
		t.Errorf("body = %q", body)
	}
	if len(f.appendCalls) != 1 || f.appendCalls[0].text != "note X" {
		t.Errorf("append calls = %+v", f.appendCalls)
	}
}

func TestDispatchAppendEmptyText(t *testing.T) {
	f := &fakeAgentMemoryStore{}
	_, isErr := DispatchAgentMemoryTool(f, "alice", AgentMemoryAppendToolName, json.RawMessage(`{"text":"   "}`))
	if !isErr {
		t.Error("expected error on empty text")
	}
	if len(f.appendCalls) != 0 {
		t.Error("should not have called store")
	}
}

func TestDispatchAppendInvalidJSON(t *testing.T) {
	f := &fakeAgentMemoryStore{}
	_, isErr := DispatchAgentMemoryTool(f, "alice", AgentMemoryAppendToolName, json.RawMessage(`not json`))
	if !isErr {
		t.Error("expected error on bad JSON")
	}
}

func TestDispatchStrReplaceSuccess(t *testing.T) {
	f := &fakeAgentMemoryStore{}
	body, isErr := DispatchAgentMemoryTool(f, "alice", AgentMemoryStrReplaceToolName, json.RawMessage(`{"old_str":"foo","new_str":"bar"}`))
	if isErr {
		t.Fatalf("unexpected error body: %s", body)
	}
	if len(f.replaceCalls) != 1 || f.replaceCalls[0].old != "foo" || f.replaceCalls[0].new != "bar" {
		t.Errorf("replace calls = %+v", f.replaceCalls)
	}
}

func TestDispatchStrReplaceNotFoundMessage(t *testing.T) {
	f := &fakeAgentMemoryStore{strReplaceErr: store.ErrAgentMemoryStrNotFound}
	body, isErr := DispatchAgentMemoryTool(f, "alice", AgentMemoryStrReplaceToolName, json.RawMessage(`{"old_str":"x","new_str":"y"}`))
	if !isErr {
		t.Fatal("expected error")
	}
	if !strings.Contains(body, "was not found") {
		t.Errorf("body = %q", body)
	}
}

func TestDispatchStrReplaceMultipleMessage(t *testing.T) {
	f := &fakeAgentMemoryStore{strReplaceErr: store.ErrAgentMemoryStrMultiple}
	body, isErr := DispatchAgentMemoryTool(f, "alice", AgentMemoryStrReplaceToolName, json.RawMessage(`{"old_str":"x","new_str":"y"}`))
	if !isErr {
		t.Fatal("expected error")
	}
	if !strings.Contains(body, "matches more than one") {
		t.Errorf("body = %q", body)
	}
}

func TestDispatchStrReplaceEmptyOldStr(t *testing.T) {
	f := &fakeAgentMemoryStore{}
	_, isErr := DispatchAgentMemoryTool(f, "alice", AgentMemoryStrReplaceToolName, json.RawMessage(`{"old_str":"","new_str":"y"}`))
	if !isErr {
		t.Error("expected error on empty old_str")
	}
	if len(f.replaceCalls) != 0 {
		t.Error("should not have called store")
	}
}

func TestDispatchUnknownTool(t *testing.T) {
	f := &fakeAgentMemoryStore{}
	_, isErr := DispatchAgentMemoryTool(f, "alice", "agent_memory_nope", json.RawMessage(`{}`))
	if !isErr {
		t.Error("expected error on unknown tool")
	}
}
