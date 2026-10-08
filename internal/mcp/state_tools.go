package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/kivali-ai/kivali/internal/agent"
	"github.com/kivali-ai/kivali/internal/store"
	"github.com/kivali-ai/kivali/internal/tracker"
)

// Tool names for the live-state lookup family. Exposed to every agent
// so the authoritative org chart and skills catalogue can be fetched
// on demand — not pre-inlined into the system prompt, where models
// tend to read mutable blocks as invariant background facts and miss
// changes that happened within the current chat.
//
// ReadAgentRole is the exception: gated to the Chief of Staff so a
// generic agent can't read another agent's durable identity. See
// StateTools' isCoS gate.
const (
	GetOrgChartToolName   = "get_org_chart"
	ListSkillsToolName    = "list_skills"
	ReadAgentRoleToolName = "read_agent_role"
	ReadHandbookToolName  = agent.ReadHandbookToolName
)

// StateDispatcher executes one state-tool invocation, returning the
// rendered tool_result body + tool-level error flag.
//
// The production implementation wraps an agentpod.Client: the agent
// pod's MCP subprocess forwards (tool, raw) over UDS to core, where
// DispatchStateToolInProcess runs against the authoritative store
// and returns (body, isError) back over the wire.
type StateDispatcher interface {
	DispatchStateTool(ctx context.Context, tool string, raw json.RawMessage) (body string, isError bool, err error)
}

// StateTools returns the MCP tool definitions for live-state
// lookups, the knowledge-graph reads and the assignment tracker. isCoS
// controls CoS-only tool visibility (currently: read_agent_role); the
// dispatcher is expected to enforce the CoS-only check
// defense-in-depth at execution time.
func StateTools(d StateDispatcher, isCoS bool) []Tool {
	if d == nil {
		return nil
	}
	out := []Tool{
		readOnlyStateTool(d, GetOrgChartToolName, getOrgChartDescription, `{"type":"object","properties":{}}`),
		readOnlyStateTool(d, ListSkillsToolName, listSkillsDescription, `{"type":"object","properties":{}}`),
		readOnlyStateTool(d, ReadHandbookToolName, agent.ReadHandbookDescription, agent.ReadHandbookInputSchema),
	}
	out = append(out, GraphTools(d)...)
	out = append(out, AssignmentTools(d)...)
	if isCoS {
		out = append(out, readOnlyStateTool(d, ReadAgentRoleToolName, readAgentRoleDescription,
			`{"type":"object","properties":{"slug":{"type":"string","description":"slug of the agent whose role.md you want to read (active or archived)"}},"required":["slug"]}`))
	}
	return out
}

// ListSkillsTool returns just the list_skills tool, dispatched
// through d. Used by the subagent toolkit which gets only this one
// state tool — skills are reachable from the agent pod's runtime
// so the subagent can pick what's runnable without org context.
func ListSkillsTool(d StateDispatcher) Tool {
	return readOnlyStateTool(d, ListSkillsToolName, listSkillsDescription, `{"type":"object","properties":{}}`)
}

func stateTool(d StateDispatcher, name, description, schema string) Tool {
	return buildStateTool(d, name, description, schema, false)
}

func readOnlyStateTool(d StateDispatcher, name, description, schema string) Tool {
	return buildStateTool(d, name, description, schema, true)
}

func buildStateTool(d StateDispatcher, name, description, schema string, readOnly bool) Tool {
	return Tool{
		Name:        name,
		Description: description,
		InputSchema: json.RawMessage(schema),
		ReadOnly:    readOnly,
		Handler: func(ctx context.Context, raw json.RawMessage) (*ToolResult, error) {
			body, isErr, err := d.DispatchStateTool(ctx, name, raw)
			if err != nil {
				return &ToolResult{IsError: true, Content: []string{name + ": " + err.Error()}}, nil
			}
			return &ToolResult{Content: []string{body}, IsError: isErr}, nil
		},
	}
}

// StateDispatchDeps is the dependency bundle for the server-side
// state-tool dispatcher (agent-pod path's handleAgentpodStateDispatch).
// Slug binds the dispatch to one agent (the assignment tools' caller and
// read_agent_role's CoS gate). IsChiefOfStaff is checked again
// server-side for read_agent_role even when the toolkit doesn't
// expose the tool — defense in depth.
type StateDispatchDeps struct {
	Store          *store.FSStore
	Slug           string
	IsChiefOfStaff bool
	// Tracker serves the assignment_* tools. nil answers them with "not
	// configured" rather than panicking, for deployments and tests
	// without a messenger to route wakes through.
	Tracker *tracker.Service
}

// DispatchStateToolInProcess runs one state tool against the
// authoritative store and returns (body, isError). Called by the
// agent-pod path's server-side handleAgentpodStateDispatch — single
// source of truth for state-tool rendering.
//
// Unknown tool names return IsError=true rather than an error
// because the wire transport surfaces tool-level failures via
// IsError, not the error return.
func DispatchStateToolInProcess(deps StateDispatchDeps, tool string, raw json.RawMessage) (string, bool) {
	if isAssignmentTool(tool) {
		return dispatchAssignmentTool(deps, tool, raw)
	}
	switch tool {
	case GetOrgChartToolName:
		return renderOrgChart(deps.Store)
	case ListSkillsToolName:
		return renderSkillsListing(deps.Store)
	case ReadHandbookToolName:
		return renderReadHandbook(deps.Store, raw, time.Now())
	case GraphQueryToolName:
		return renderGraphQuery(deps.Store, deps.Slug, raw)
	case GraphNodeToolName:
		return renderGraphNode(deps.Store, deps.Slug, raw)
	case ReadAgentRoleToolName:
		if !deps.IsChiefOfStaff {
			return fmt.Sprintf("read_agent_role: only the Chief of Staff may read agent roles (caller: %q)", deps.Slug), true
		}
		return renderReadAgentRole(deps.Store, raw)
	default:
		return "unknown state tool: " + tool, true
	}
}

func renderOrgChart(s *store.FSStore) (string, bool) {
	actives, err := s.ListActiveAgents()
	if err != nil {
		return "org chart: " + err.Error(), true
	}
	now := time.Now().UTC().Format("2006-01-02 15:04:05 UTC")
	var b strings.Builder
	fmt.Fprintf(&b, "Org chart snapshot as of %s:\n\n", now)
	b.WriteString(agent.OrgChartMarkdown(actives, agent.OwnerLabel(s)))
	return b.String(), false
}

func renderSkillsListing(s *store.FSStore) (string, bool) {
	skills, err := s.ListEnabledSkills()
	if err != nil {
		return "list_skills: " + err.Error(), true
	}
	now := time.Now().UTC().Format("2006-01-02 15:04:05 UTC")
	if len(skills) == 0 {
		return fmt.Sprintf("Skills snapshot as of %s: no skills installed.", now), false
	}
	var b strings.Builder
	fmt.Fprintf(&b, "Skills snapshot as of %s:\n\n", now)
	for _, sk := range skills {
		desc := sk.Description
		if desc == "" {
			desc = "(no description)"
		}
		when := sk.WhenToUse
		if when == "" {
			when = "(no when_to_use)"
		}
		// aux = everything except the SKILL.md manifest. "files: 0"
		// means markdown-only; "files: N" hints that there are
		// scripts/assets worth glancing at.
		aux := sk.FileCount - 1
		if aux < 0 {
			aux = 0
		}
		fmt.Fprintf(&b, "- %s (%d aux files) — %s · when_to_use: %s\n    manifest: /files/skills/%s/SKILL.md · scripts: /files/skills/%s/\n",
			sk.Name, aux, desc, when, sk.Name, sk.Name)
	}
	return b.String(), false
}

func renderReadAgentRole(s *store.FSStore, raw json.RawMessage) (string, bool) {
	var in struct {
		Slug string `json:"slug"`
	}
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &in); err != nil {
			return "read_agent_role: invalid input: " + err.Error(), true
		}
	}
	slug := strings.ToLower(strings.TrimSpace(in.Slug))
	if slug == "" {
		return "read_agent_role: `slug` is required", true
	}
	body, err := s.ReadRole(slug)
	if err == nil {
		now := time.Now().UTC().Format("2006-01-02 15:04:05 UTC")
		return fmt.Sprintf("Role for `%s` as of %s:\n\n%s", slug, now, body), false
	}
	if abody, aerr := readArchivedRole(s, slug); aerr == nil {
		now := time.Now().UTC().Format("2006-01-02 15:04:05 UTC")
		return fmt.Sprintf("Role for `%s` (ARCHIVED) as of %s:\n\n%s", slug, now, abody), false
	}
	return fmt.Sprintf("read_agent_role: no agent %q (active or archived): %v", slug, err), true
}

// readArchivedRole reads role.md from agents/_archived/<slug>/. Kept
// here rather than in store/ because read_agent_role is the only
// caller; promoting it would invite drift.
func readArchivedRole(s *store.FSStore, slug string) (string, error) {
	if _, err := s.GetArchivedAgent(slug); err != nil {
		return "", err
	}
	b, err := os.ReadFile(filepath.Join(s.Root(), "agents", "_archived", slug, "role.md"))
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// Descriptions kept verbatim aligned with internal/agent/state_tools.go
// so model-facing text is identical across transports.

const getOrgChartDescription = `Returns the current org chart — every active agent and who they report to — as a timestamped snapshot. No input parameters. Not pre-inlined in your system prompt; the "as of <timestamp>" line lets you tell whether a lookup in chat history is still current.`

// renderReadHandbook answers read_handbook: the saved handbook, or one
// section of it, under a timestamp line. now stamps the snapshot.
func renderReadHandbook(s *store.FSStore, raw json.RawMessage, now time.Time) (string, bool) {
	var in struct {
		Section string `json:"section"`
	}
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &in); err != nil {
			return "read_handbook: invalid input: " + err.Error(), true
		}
	}
	body, err := s.ReadHandbook()
	if errors.Is(err, store.ErrNotFound) {
		return "read_handbook: no handbook has been saved yet", true
	}
	if err != nil {
		return "read_handbook: " + err.Error(), true
	}
	stamp := now.UTC().Format("2006-01-02 15:04:05 UTC")
	want := strings.TrimSpace(strings.TrimLeft(strings.TrimSpace(in.Section), "#"))
	if want == "" {
		return fmt.Sprintf("Handbook as of %s:\n\n%s", stamp, body), false
	}
	section, headings := handbookSection(body, want)
	if section == "" {
		return fmt.Sprintf("read_handbook: no section %q. Its headings:\n%s", want, strings.Join(headings, "\n")), true
	}
	return fmt.Sprintf("Handbook section %q as of %s:\n\n%s", want, stamp, section), false
}

// handbookSection returns the section of md whose heading text is
// title (case-insensitive), from its heading to the next heading at
// its level or above, and every heading line in md. Lines inside code
// fences are never headings.
func handbookSection(md, title string) (string, []string) {
	lines := strings.Split(md, "\n")
	var headings []string
	start, level := -1, 0
	end := len(lines)
	fenced := false
	for i, l := range lines {
		if strings.HasPrefix(strings.TrimSpace(l), "```") {
			fenced = !fenced
			continue
		}
		n := len(l) - len(strings.TrimLeft(l, "#"))
		if fenced || n == 0 || n > 6 || !strings.HasPrefix(l[n:], " ") {
			continue
		}
		headings = append(headings, l)
		text := strings.TrimSpace(l[n:])
		switch {
		case start < 0 && strings.EqualFold(text, title):
			start, level = i, n
		case start >= 0 && end == len(lines) && n <= level:
			end = i
		}
	}
	if start < 0 {
		return "", headings
	}
	return strings.TrimRight(strings.Join(lines[start:end], "\n"), "\n"), headings
}

const readAgentRoleDescription = `Read the current ` + "`role.md`" + ` of an existing agent (active or archived under ` + "`agents/_archived/<slug>/`" + `). Chief of Staff only. Returns the verbatim contents prefixed with a snapshot timestamp. Used as the first step of a propose_role_update — see cos_role.md §Updating an existing agent's role.`

const listSkillsDescription = `List the org-wide skills available to you (name, description, when_to_use, file count). No input parameters. Skills are shared procedures rooted at /files/skills/<name>/; read the chosen one with file_view /files/skills/<name>/SKILL.md. Run their scripts directly from run_shell, e.g. bash /files/skills/<name>/scripts/foo.sh.`
