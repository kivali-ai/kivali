package agent

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/kivali-ai/kivali/internal/provider"
	"github.com/kivali-ai/kivali/internal/store"
)

// Tool names for the agent_memory family. Kept in the agent package
// alongside the provider.Tool definitions; dispatchers (web/chat and
// release/engine) import from here.
//
// Two documents, one tool shape each: agent_memory.md is semantic
// memory (what the agent knows to be true) and agent_memory_habits.md
// is the short list of learned rules
// for how it acts. Same three verbs on both.
const (
	AgentMemoryViewToolName       = "agent_memory_view"
	AgentMemoryAppendToolName     = "agent_memory_append"
	AgentMemoryStrReplaceToolName = "agent_memory_str_replace"

	AgentHabitsViewToolName       = "agent_memory_habits_view"
	AgentHabitsAppendToolName     = "agent_memory_habits_append"
	AgentHabitsStrReplaceToolName = "agent_memory_habits_str_replace"
)

// IsAgentMemoryTool reports whether a tool-call name belongs to the
// agent_memory_* family (as distinct from the /files/ file_*
// family, which targets the virtual filesystem).
func IsAgentMemoryTool(name string) bool {
	return memoryDocFor(name) != nil
}

// IsAgentHabitsMutation reports whether name writes the habits file.
// Habits are
// rewritten only during the rotation turn; the core-side dispatch
// endpoint uses this to refuse a write outside one. The view is never
// gated — reading is harmless.
func IsAgentHabitsMutation(name string) bool {
	return name == AgentHabitsAppendToolName || name == AgentHabitsStrReplaceToolName
}

// AgentMemoryStore is the narrow subset of store.FSStore the agent
// memory dispatcher needs. Lets tests pass in a fake without dragging
// in a full FSStore.
type AgentMemoryStore interface {
	ReadAgentMemory(slug string) (string, error)
	AppendAgentMemory(slug, text string) error
	StrReplaceAgentMemory(slug, oldStr, newStr string) error
	ReadAgentHabits(slug string) (string, error)
	AppendAgentHabits(slug, text string) error
	StrReplaceAgentHabits(slug, oldStr, newStr string) error
}

// Descriptions are exported so the MCP twin (internal/mcp) can carry
// the same words; TestAgentMemoryDescriptionsMatchAcrossTransports
// enforces it.
const (
	AgentMemoryViewDescription       = `Return the current contents of your agent memory. Use this when you need to see what's there before deciding whether to append or str_replace — especially when an str_replace failed because old_str didn't match. No input parameters.`
	AgentMemoryAppendDescription     = `Append text to your own agent memory (separated from prior content by a blank line). Your agent memory is inlined into your system prompt on every future call; target under 32 KB total and prefer agent_memory_str_replace over unbounded growth. Rewritten only at chat rotation — see handbook §Keep your agent memory lean and load-bearing for when to reach for this.`
	AgentMemoryStrReplaceDescription = `Replace a unique substring inside your own agent memory. Fails if old_str occurs zero times or more than once. Use to update, correct, or prune (pass empty new_str to delete). When old_str isn't unique, expand it to include enough surrounding context to match exactly once. Match is byte-exact (after Unicode NFC normalization): if old_str contains characters like em dashes, curly quotes, or non-breaking spaces, copy them verbatim from a prior agent_memory_view instead of retyping — the dash you type may not be the dash already in memory.`

	AgentHabitsViewDescription       = `Return the current contents of your habits — the short list of learned rules for how you act, inlined into your system prompt above your agent memory. No input parameters.`
	AgentHabitsAppendDescription     = `Append one habit, with a one-line why, to your habits (separated from prior content by a blank line). A habit is a rule for how you act, not a fact about the world — facts go in agent_memory_append. Habits are rewritten only at chat rotation; a call outside the rotation turn is refused. Keep the list short.`
	AgentHabitsStrReplaceDescription = `Replace a unique substring inside your habits — to revise one, or to revoke one by passing an empty new_str. Fails if old_str occurs zero times or more than once; expand old_str with surrounding context until it matches exactly once. Habits are rewritten only at chat rotation; a call outside the rotation turn is refused. Match is byte-exact (after Unicode NFC normalization): copy old_str verbatim from a prior agent_memory_habits_view rather than retyping it.`
)

// AgentMemoryTools returns the provider.Tool definitions that let an
// agent curate its own memory. Both
// documents are rewritten at chat rotation and only there — semantic
// memory by prompt, habits by the dispatch endpoint's gate as well. Policy lives in the
// handbook under "Keep your agent memory lean and load-bearing";
// descriptions here cover mechanics only.
func AgentMemoryTools() []provider.Tool {
	return []provider.Tool{
		memoryViewTool(AgentMemoryViewToolName, AgentMemoryViewDescription),
		memoryAppendTool(AgentMemoryAppendToolName, AgentMemoryAppendDescription, "content to append to your agent memory (markdown)"),
		memoryStrReplaceTool(AgentMemoryStrReplaceToolName, AgentMemoryStrReplaceDescription, "exact substring to replace; must occur exactly once in your agent memory"),
		memoryViewTool(AgentHabitsViewToolName, AgentHabitsViewDescription),
		memoryAppendTool(AgentHabitsAppendToolName, AgentHabitsAppendDescription, "one habit to append (markdown), with a one-line why"),
		memoryStrReplaceTool(AgentHabitsStrReplaceToolName, AgentHabitsStrReplaceDescription, "exact substring to replace; must occur exactly once in your habits"),
	}
}

func memoryViewTool(name, description string) provider.Tool {
	return provider.Tool{
		Name:        name,
		Description: description,
		InputSchema: json.RawMessage(`{
  "type": "object",
  "properties": {}
}`),
	}
}

func memoryAppendTool(name, description, textDescription string) provider.Tool {
	return provider.Tool{
		Name:        name,
		Description: description,
		InputSchema: json.RawMessage(fmt.Sprintf(`{
  "type": "object",
  "properties": {
    "text": {"type": "string", "description": %q}
  },
  "required": ["text"]
}`, textDescription)),
	}
}

func memoryStrReplaceTool(name, description, oldStrDescription string) provider.Tool {
	return provider.Tool{
		Name:        name,
		Description: description,
		InputSchema: json.RawMessage(fmt.Sprintf(`{
  "type": "object",
  "properties": {
    "old_str": {"type": "string", "description": %q},
    "new_str": {"type": "string", "description": "replacement text (may be empty to delete the matched range)"}
  },
  "required": ["old_str", "new_str"]
}`, oldStrDescription)),
	}
}

// memoryDoc binds one tool trio to the document it edits. The
// dispatcher is written once against this shape; the two documents
// differ only in name and store accessors.
type memoryDoc struct {
	label      string // how tool_result text names the document
	viewTool   string // what to tell the agent to call before retrying
	read       func(s AgentMemoryStore, slug string) (string, error)
	appendFn   func(s AgentMemoryStore, slug, text string) error
	strReplace func(s AgentMemoryStore, slug, oldStr, newStr string) error
	verb       string // "view" | "append" | "str_replace"
}

var (
	semanticMemoryDoc = memoryDoc{
		label:      "agent memory",
		viewTool:   AgentMemoryViewToolName,
		read:       AgentMemoryStore.ReadAgentMemory,
		appendFn:   AgentMemoryStore.AppendAgentMemory,
		strReplace: AgentMemoryStore.StrReplaceAgentMemory,
	}
	habitsDoc = memoryDoc{
		label:      "habits",
		viewTool:   AgentHabitsViewToolName,
		read:       AgentMemoryStore.ReadAgentHabits,
		appendFn:   AgentMemoryStore.AppendAgentHabits,
		strReplace: AgentMemoryStore.StrReplaceAgentHabits,
	}
)

// memoryDocFor resolves a tool name to its document and verb; nil for
// a name outside the family.
func memoryDocFor(name string) *memoryDoc {
	var d memoryDoc
	switch name {
	case AgentMemoryViewToolName:
		d, d.verb = semanticMemoryDoc, "view"
	case AgentMemoryAppendToolName:
		d, d.verb = semanticMemoryDoc, "append"
	case AgentMemoryStrReplaceToolName:
		d, d.verb = semanticMemoryDoc, "str_replace"
	case AgentHabitsViewToolName:
		d, d.verb = habitsDoc, "view"
	case AgentHabitsAppendToolName:
		d, d.verb = habitsDoc, "append"
	case AgentHabitsStrReplaceToolName:
		d, d.verb = habitsDoc, "str_replace"
	default:
		return nil
	}
	return &d
}

// DispatchAgentMemoryTool executes one agent_memory_* tool call and
// returns (body, isError) suitable for wrapping in a tool_result.
//
// Runs on core, reached from the agent pod's MCP subprocess over the
// UDS memory/dispatch endpoint, so both transports share one
// implementation. The rotation-only gate on habits writes lives
// at that endpoint, not here: this function has no view of whether a
// rotation is pending.
func DispatchAgentMemoryTool(s AgentMemoryStore, slug, toolName string, input json.RawMessage) (body string, isError bool) {
	d := memoryDocFor(toolName)
	if d == nil {
		return "unknown agent memory tool: " + toolName, true
	}
	switch d.verb {
	case "view":
		body, err := d.read(s, slug)
		if err != nil {
			if errors.Is(err, store.ErrNotFound) {
				return "Your " + d.label + " is empty.", false
			}
			return err.Error(), true
		}
		if strings.TrimSpace(body) == "" {
			return "Your " + d.label + " is empty.", false
		}
		return body, false

	case "append":
		var in struct {
			Text string `json:"text"`
		}
		if err := json.Unmarshal(input, &in); err != nil {
			return "invalid input: " + err.Error(), true
		}
		if strings.TrimSpace(in.Text) == "" {
			return "text is required", true
		}
		if err := d.appendFn(s, slug, in.Text); err != nil {
			return err.Error(), true
		}
		return fmt.Sprintf("appended %d chars to your %s", len(in.Text), d.label), false

	default: // str_replace
		var in struct {
			OldStr string `json:"old_str"`
			NewStr string `json:"new_str"`
		}
		if err := json.Unmarshal(input, &in); err != nil {
			return "invalid input: " + err.Error(), true
		}
		if in.OldStr == "" {
			return "old_str cannot be empty", true
		}
		if err := d.strReplace(s, slug, in.OldStr, in.NewStr); err != nil {
			switch {
			case errors.Is(err, store.ErrNotFound):
				return "your " + d.label + " is empty; use the append tool to start it", true
			case errors.Is(err, store.ErrAgentMemoryStrNotFound):
				return "old_str was not found in your " + d.label + "; call " + d.viewTool + " first or double-check spacing/indentation", true
			case errors.Is(err, store.ErrAgentMemoryStrMultiple):
				return "old_str matches more than one place; expand the match to include enough surrounding context to be unique", true
			case errors.Is(err, store.ErrAgentMemoryStrFoldOnly):
				return "old_str doesn't byte-match your " + d.label + ", but a visually similar fragment exists (em/en dash vs '-', curly vs straight quotes, no-break space vs space, ellipsis '…' vs '...'). Call " + d.viewTool + " and copy the exact bytes — don't retype.", true
			}
			return err.Error(), true
		}
		return d.label + " updated", false
	}
}
