package web

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/kivali-ai/kivali/internal/mcp"
	"github.com/kivali-ai/kivali/internal/provider"
	"github.com/kivali-ai/kivali/internal/store"
)

// isSubagentToolName reports whether a stored tool_use row's tool name
// is the subagent tool. The driver strips its provider's prefix from
// every event (internal/claudeagent), so stored rows hold bare names.
func isSubagentToolName(name string) bool {
	return name == mcp.SubagentToolName
}

// subagentTaskSpec is what one task looks like as the agent asked for
// it: the fields of the tool_use input a chip row needs. Model and
// effort are RESOLVED, not raw — an omitted model is the fleet default
// and an omitted effort is high, and the row says so rather than
// showing a blank.
type subagentTaskSpec struct {
	Description string
	Model       string
	Effort      string
}

// subagentTasksFromInput parses the JSON tool_input on a subagent
// tool_use and returns one spec per task, in order. Returns nil on any
// decoding failure — the task list is best-effort display, not
// load-bearing data.
func subagentTasksFromInput(p provider.Provider, raw string) []subagentTaskSpec {
	var in struct {
		Tasks []struct {
			Description string `json:"description"`
			Model       string `json:"model"`
			Effort      string `json:"effort"`
		} `json:"tasks"`
	}
	if err := json.Unmarshal([]byte(raw), &in); err != nil {
		return nil
	}
	out := make([]subagentTaskSpec, 0, len(in.Tasks))
	for _, t := range in.Tasks {
		out = append(out, subagentTaskSpec{
			Description: t.Description,
			Model:       resolveSubagentModel(p, t.Model),
			Effort:      resolveSubagentEffort(p, resolveSubagentModel(p, t.Model), t.Effort),
		})
	}
	return out
}

// SubagentTaskView is one task of a subagent tool_use, as the
// transcript's subagents row lists it.
// Built per (description, id) pair: description, model and effort from
// the tool_use's JSON input; id parsed out of the tool_result body;
// status / error / final_text read off disk (meta.json + chat.jsonl).
//
// Status takes one of: "running" (subagent in flight or ID not known
// yet), "completed", "errored", "unknown" (no meta.json on disk —
// shouldn't happen post-bootstrap but is the safe default).
//
// Model is the bare id; the transcript names it through
// the provider's label as it does the parent's own rows. Effort is
// the level's id.
type SubagentTaskView struct {
	ID          string
	Description string
	Model       string
	Effort      string
	Status      string
	Error       string
	FinalText   string
}

// SubagentAggregateView summarizes a batch for the chip header.
// Status is the worst state across tasks; Label is the human string
// to render.
type SubagentAggregateView struct {
	Total     int
	Running   int
	Completed int
	Errored   int
	Status    string // running | completed | errored
	Label     string
}

// subagentTaskViews builds per-task views for one subagent tool_use.
// Called by the transcript with the tool_use's input JSON and the
// matching tool_result body (empty string while the tool is running). The
// number of rows == number of tasks in the input; mid-flight rows
// have empty ID and Status="running".
func (s *Server) subagentTaskViews(parent, toolInput, toolResultBody string) []SubagentTaskView {
	specs := subagentTasksFromInput(s.Provider, toolInput)
	ids := subagentTranscriptIDsFromOutput(toolResultBody)
	out := make([]SubagentTaskView, 0, len(specs))
	for i, spec := range specs {
		var id string
		if i < len(ids) {
			id = ids[i]
		}
		if id == "" {
			out = append(out, SubagentTaskView{
				Description: spec.Description,
				Model:       spec.Model,
				Effort:      spec.Effort,
				Status:      "running",
			})
			continue
		}
		v := deriveSubagentTaskView(s.Store.Root(), parent, id, spec.Description)
		// meta.json records what the job actually ran on (see
		// runJob); the tool input is what was asked for. They agree
		// for anything dispatched since both were resolved in core,
		// and for an older job whose meta has no effort the input's
		// resolved value is the right fallback.
		if v.Model == "" {
			v.Model = spec.Model
		}
		if v.Effort == "" {
			v.Effort = spec.Effort
		}
		out = append(out, v)
	}
	return out
}

// deriveSubagentTaskView reads meta.json + chat.jsonl off disk and
// fills in the per-task row. FinalText is the LAST sent direct_chat
// in the transcript — the verbatim assistant answer. We rely on
// in-disk state because the chip data flow has no live channel
// (yet) — once the runner is moved into the web process, this can
// fold into a hub-based event stream and the read-on-render dance
// goes away.
func deriveSubagentTaskView(storeRoot, parent, id, description string) SubagentTaskView {
	out := SubagentTaskView{ID: id, Description: description, Status: "running"}
	if body, err := os.ReadFile(subagentMetaPath(storeRoot, parent, id)); err == nil {
		var meta subagentMeta
		if err := json.Unmarshal(body, &meta); err == nil {
			if meta.Status != "" {
				out.Status = meta.Status
			}
			out.Error = meta.Error
			out.Model = meta.Model
			out.Effort = meta.Effort
		}
	} else {
		out.Status = "unknown"
	}
	chatPath := filepath.Join(storeRoot, "agents", parent, "subagents", id, "chat.jsonl")
	if hist, err := readSubagentHistory(chatPath); err == nil {
		for i := len(hist) - 1; i >= 0; i-- {
			m := hist[i]
			if string(m.Role) == "sent" && m.Kind == "direct_chat" {
				out.FinalText = m.Content
				break
			}
		}
	}
	return out
}

// subagentIDPattern extracts the per-task ids from the subagent tool's
// rendered tool_result. The output format is:
//
//	=== task N: <description> ===
//	id: <id> · transcript: /agents/<parent>/subagents/<id>
//
// We pull the "id: <id>" line per task. If it's missing for a given
// task (e.g. early-fail before id assignment), the slice element is
// "".
var subagentIDPattern = regexp.MustCompile(`(?m)^id:\s+([0-9a-f]+)\s`)

// subagentReceiptIDPattern extracts the per-task ids from a background
// batch's dispatch receipt, whose task lines read "  <id> — <desc>".
var subagentReceiptIDPattern = regexp.MustCompile(`(?m)^ {2}([0-9a-f]{8,}) — `)

// subagentTranscriptIDsFromOutput pulls per-task transcript IDs out of
// the rendered tool_result text. Order matches the input task array
// (one ID per "=== task N: ... ===" section).
func subagentTranscriptIDsFromOutput(text string) []string {
	if text == "" {
		return nil
	}
	// Walk by task header so missing-id tasks (early failures) get an
	// empty slot rather than shifting subsequent IDs.
	sections := strings.Split(text, "\n=== task ")
	if len(sections) <= 1 {
		// No task sections: the dispatch receipt a background batch
		// returns (renderSubagentDispatch), one "  <id> — <description>"
		// line per task, in order.
		var out []string
		for _, m := range subagentReceiptIDPattern.FindAllStringSubmatch(text, -1) {
			out = append(out, m[1])
		}
		return out
	}
	out := make([]string, 0, len(sections)-1)
	for _, sec := range sections[1:] {
		m := subagentIDPattern.FindStringSubmatch(sec)
		if len(m) == 2 {
			out = append(out, m[1])
		} else {
			out = append(out, "")
		}
	}
	return out
}

// subagentMetaPath is where a job's meta.json lives: beside its
// chat.jsonl, under the parent's subagents/<id>/ dir. ONE helper for
// the writer and both readers — they had drifted, with the writer a
// level above the files overlay and the readers here, so the file was
// written on every state change and never read. See
// SubagentService.writeSubagentMeta.
func subagentMetaPath(storeRoot, parent, id string) string {
	return filepath.Join(storeRoot, "agents", parent, "subagents", id, "meta.json")
}

// subagentMeta is the shape persisted by SubagentService.writeSubagentMeta.
// Decoded here for the background task transcript and the task rows.
type subagentMeta struct {
	ID          string `json:"id"`
	Description string `json:"description"`
	Prompt      string `json:"prompt"`
	Model       string `json:"model"`
	Effort      string `json:"effort"`
	Status      string `json:"status"`
	Error       string `json:"error"`
	UpdatedAt   string `json:"updated_at"`
}

func readSubagentMeta(path string) (subagentMeta, error) {
	body, err := os.ReadFile(path)
	if err != nil {
		return subagentMeta{}, err
	}
	var out subagentMeta
	if err := json.Unmarshal(body, &out); err != nil {
		return subagentMeta{}, err
	}
	return out, nil
}

// readSubagentHistory reads a subagent's chat.jsonl off disk into a
// []store.ChatMessage, tolerating a final row cut short by a power cut
// as the store's own chat reads do.
func readSubagentHistory(path string) ([]store.ChatMessage, error) {
	return store.ReadChatFile(path)
}
