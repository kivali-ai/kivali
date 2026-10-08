package store

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/kivali-ai/kivali/internal/graph"
	"golang.org/x/text/unicode/norm"
	"gopkg.in/yaml.v3"
)

// AgentStatus is the lifecycle state of an agent.
type AgentStatus string

const (
	StatusActive   AgentStatus = "active"
	StatusArchived AgentStatus = "archived"
)

// Agent is an agent in the Kivali org. Slug + Role are immutable
// identity; ReportsTo can shift when the Chief of Staff proposes a
// reorg and the CEO approves (see Runtime.ApplyReorg / SetAgentReportsTo).
// Lifecycle fields (Status, ArchivedAt) move with archive.
type Agent struct {
	Slug string `yaml:"slug"`
	Role string `yaml:"role"`
	// Icon is the role icon its avatar shows (a roleicon name), picked
	// by whoever hired it. Empty: the avatar shows initials only.
	Icon      string      `yaml:"icon,omitempty"`
	ReportsTo string      `yaml:"reports_to,omitempty"`
	Status    AgentStatus `yaml:"status"`
	Model     string      `yaml:"model,omitempty"`
	// Effort is the per-agent reasoning level (an effort id the model
	// offers). Empty means "use the model's default" (Provider.Effort).
	// Mirrors Model: set from the chat-page selector, read at request-
	// build time, applied to the agent's next fresh CLI session.
	Effort     string     `yaml:"effort,omitempty"`
	CreatedAt  time.Time  `yaml:"created_at"`
	ArchivedAt *time.Time `yaml:"archived_at,omitempty"`
}

// Message role values. Named after email/memo convention rather than the
// Claude API's user/assistant so the data model reads naturally in an
// organizational context. Conversion to the wire format (user/assistant)
// happens in internal/agent.
const (
	RoleReceived = "received" // authored by someone other than this agent
	RoleSent     = "sent"     // authored by this agent
)

// ChatMessage is one entry in an agent's chat.jsonl.
//
// Tool interactions (`Kind` = "tool_use" / "tool_result") use the same
// shape as regular direct-chat messages: a tool_use from the agent is
// a RoleSent entry whose Content carries a human-readable summary and
// whose ToolUseID / ToolName / ToolInput preserve the structured
// payload. A tool_result is a RoleReceived entry with matching
// ToolUseID. Keeping these as distinct JSONL lines (instead of a
// multi-block struct) keeps the file human-greppable and the schema
// minimal.
type ChatMessage struct {
	Role    string    `json:"role"`           // RoleReceived | RoleSent
	Content string    `json:"content"`        // text content or serialized tool block
	TS      time.Time `json:"ts"`             // when appended
	Kind    string    `json:"kind,omitempty"` // "direct_chat" | "inbox_delivery" | "ceo_reply" | "tool_use" | "tool_result" | ... | ""
	// MessageRef is the relative path of the standardized Message
	// this chat entry references (a doc_published entry points at what
	// was just emitted, a ceo_reply at the reply it wrote, etc.).
	MessageRef string `json:"message_ref,omitempty"`
	// ReplyToMessageRef, when non-empty, pairs a ceo_reply bubble
	// with the message it answers. Set only on the sent/ceo_reply
	// side. Used by the CEO inbox view to thread replies under their
	// originating items.
	ReplyToMessageRef string              `json:"reply_to_message_ref,omitempty"`
	Attachments       []MessageAttachment `json:"attachments,omitempty"`

	// Quiet marks a received entry that asks for no turn of its own:
	// it is read with whatever next wakes the agent, and folds into a
	// turn already running, but never starts one. The spawn gate walks
	// past it. Set by the messaging layer from Message.Quiet (a hold,
	// for one) and for a pure receipt.
	Quiet bool `json:"quiet,omitempty"`

	// Model is the Claude model ID that produced this message, recorded
	// only on the agent's own direct_chat output (RoleSent). Set at flush
	// time from the turn's resolved model so the UI can label each model
	// bubble with the friendly name of the model that actually wrote it —
	// stable even if the agent's model is changed later. Empty on user
	// messages and tool rows, and on any row written without a model
	// (the UI suppresses the pill in those cases).
	Model string `json:"model,omitempty"`

	// Effort is the reasoning level (an effort id the model offers) the
	// turn ran at, recorded alongside Model on the agent's own direct_chat
	// output. Stamped at flush time from the turn's resolved (normalized)
	// effort, so each bubble shows the depth it actually ran at — stable
	// even if the agent's effort is changed later. Empty on user messages
	// and tool rows, and on any row written without an effort (the UI
	// suppresses the pill in those cases).
	Effort string `json:"effort,omitempty"`

	// Tool interaction metadata. ToolUseID correlates a tool_use with
	// its tool_result across two consecutive chat.jsonl lines.
	ToolUseID string `json:"tool_use_id,omitempty"`
	ToolName  string `json:"tool_name,omitempty"`
	ToolInput string `json:"tool_input,omitempty"`
	IsError   bool   `json:"is_error,omitempty"`
}

// MessageAttachment references a blob in attachments/<sha>/ from a chat
// message or inbox message. Name is the display alias (may differ from
// the blob's first-upload name when the caller chose a context-specific
// label).
type MessageAttachment struct {
	SHA  string `json:"sha" yaml:"sha"`
	Name string `json:"name" yaml:"name"`
}

// Per-agent mutex for chat.jsonl append serialization within a process.
var chatMu sync.Map

func chatLock(slug string) *sync.Mutex {
	v, _ := chatMu.LoadOrStore(slug, &sync.Mutex{})
	return v.(*sync.Mutex)
}

// Per-agent mutex held across LinkChatAttachments' check and sync, so
// a caller that finds nothing marked knows no link is still running.
var chatLinkMu sync.Map

func chatLinkLock(slug string) *sync.Mutex {
	v, _ := chatLinkMu.LoadOrStore(slug, &sync.Mutex{})
	return v.(*sync.Mutex)
}

// CreateAgent creates a new agent directory with agent.yaml, role.md,
// and an empty chat.jsonl. Errors if the slug is already in use (active
// or archived).
func (s *FSStore) CreateAgent(a Agent, role string) error {
	if a.Slug == "" {
		return errors.New("agent: empty slug")
	}
	if _, err := s.GetAgent(a.Slug); err == nil {
		return fmt.Errorf("agent: slug %q already active", a.Slug)
	}
	if _, err := os.Stat(s.path("agents", "_archived", a.Slug)); err == nil {
		return fmt.Errorf("agent: slug %q exists in archive", a.Slug)
	}
	if a.CreatedAt.IsZero() {
		a.CreatedAt = time.Now().UTC()
	}
	a.Status = StatusActive
	a.ArchivedAt = nil
	dir := s.path("agents", a.Slug)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	meta, err := yaml.Marshal(a)
	if err != nil {
		return err
	}
	if err := writeAtomic(filepath.Join(dir, "agent.yaml"), meta, 0o644); err != nil {
		return err
	}
	if err := writeAtomic(filepath.Join(dir, "role.md"), []byte(role), 0o644); err != nil {
		return err
	}
	f, err := os.OpenFile(filepath.Join(dir, "chat.jsonl"), os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	s.Graph().OrgChanged()
	return nil
}

// GetAgent returns the active agent with the given slug.
func (s *FSStore) GetAgent(slug string) (Agent, error) {
	return s.readAgent(s.path("agents", slug))
}

// GetArchivedAgent returns an archived agent.
func (s *FSStore) GetArchivedAgent(slug string) (Agent, error) {
	return s.readAgent(s.path("agents", "_archived", slug))
}

func (s *FSStore) readAgent(dir string) (Agent, error) {
	b, err := os.ReadFile(filepath.Join(dir, "agent.yaml"))
	if errors.Is(err, os.ErrNotExist) {
		return Agent{}, ErrNotFound
	}
	if err != nil {
		return Agent{}, err
	}
	var a Agent
	if err := yaml.Unmarshal(b, &a); err != nil {
		return Agent{}, fmt.Errorf("agent %s: %w", filepath.Base(dir), err)
	}
	return a, nil
}

// ListActiveAgents returns the active agents sorted by slug.
func (s *FSStore) ListActiveAgents() ([]Agent, error) {
	return s.listAgentsIn(s.path("agents"), true)
}

// ListArchivedAgents returns the archived agents sorted by slug.
func (s *FSStore) ListArchivedAgents() ([]Agent, error) {
	return s.listAgentsIn(s.path("agents", "_archived"), false)
}

func (s *FSStore) listAgentsIn(dir string, skipUnderscore bool) ([]Agent, error) {
	entries, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []Agent
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		if skipUnderscore && strings.HasPrefix(e.Name(), "_") {
			continue
		}
		a, err := s.readAgent(filepath.Join(dir, e.Name()))
		if err != nil {
			if errors.Is(err, ErrNotFound) {
				continue
			}
			return nil, err
		}
		out = append(out, a)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Slug < out[j].Slug })
	return out, nil
}

// SetAgentModel updates just the Model field on an active agent's
// agent.yaml. All other identity and lifecycle fields are preserved.
// Returns ErrNotFound if the agent isn't active.
func (s *FSStore) SetAgentModel(slug, model string) error {
	a, err := s.GetAgent(slug)
	if err != nil {
		return err
	}
	a.Model = model
	meta, err := yaml.Marshal(a)
	if err != nil {
		return err
	}
	return writeAtomic(filepath.Join(s.path("agents", slug), "agent.yaml"), meta, 0o644)
}

// ModelPinUpgrade records one agent.yaml that UpgradeModelPins rewrote.
type ModelPinUpgrade struct {
	Slug string
	From string
	To   string
}

// UpgradeModelPins moves every active agent's model pin to what
// `current` says it should be today, and reports each pin it moved.
//
// Runs at boot and after a restore, with Provider.Current as the
// resolver: an agent hired while the fleet default was Opus 4.8 has
// "claude-opus-4-8" in its agent.yaml forever otherwise, because a hire
// pins the concrete id rather than inheriting. When Kivali retires 4.8
// from the picker, this is what puts that agent on Opus 5.5 — the
// resolver decides where a pin goes, this only writes it down.
//
// The pin is rewritten rather than resolved on every read so that the
// file, the picker, the chips and the runner all agree: a persisted
// "claude-opus-4-8" that ran as 5.5 would show one thing on disk and
// another in the transcript, and a form saved from the detail page
// would persist whichever the page happened to render.
//
// An empty pin is "use the fleet default" and stays empty; config
// resolves the default separately. A pin the resolver returns
// unchanged is left alone, so the pass is idempotent and a no-op on a
// fleet with nothing retired. Archived agents are not touched: they
// do not run, and their pin is a record of what they ran on.
func (s *FSStore) UpgradeModelPins(current func(model string) string) ([]ModelPinUpgrade, error) {
	agents, err := s.ListActiveAgents()
	if err != nil {
		return nil, err
	}
	var moved []ModelPinUpgrade
	for _, a := range agents {
		if a.Model == "" {
			continue
		}
		to := current(a.Model)
		if to == "" || to == a.Model {
			continue
		}
		if err := s.SetAgentModel(a.Slug, to); err != nil {
			return moved, fmt.Errorf("agent %s: %w", a.Slug, err)
		}
		moved = append(moved, ModelPinUpgrade{Slug: a.Slug, From: a.Model, To: to})
	}
	return moved, nil
}

// SetAgentEffort overwrites Effort on an active agent's agent.yaml.
// Empty string clears the override so the agent inherits the fleet
// default. Mirrors SetAgentModel; the change applies on the agent's
// next fresh CLI session (the persistent runner pins effort at spawn).
func (s *FSStore) SetAgentEffort(slug, effort string) error {
	a, err := s.GetAgent(slug)
	if err != nil {
		return err
	}
	a.Effort = effort
	meta, err := yaml.Marshal(a)
	if err != nil {
		return err
	}
	return writeAtomic(filepath.Join(s.path("agents", slug), "agent.yaml"), meta, 0o644)
}

// SetAgentReportsTo overwrites ReportsTo on an active agent's
// agent.yaml. The single sanctioned mutator for the manager link;
// callers should prefer Runtime.ApplyReorg, which layers existence and
// cycle checks on top. Returns ErrNotFound if the agent isn't active.
func (s *FSStore) SetAgentReportsTo(slug, newManager string) error {
	a, err := s.GetAgent(slug)
	if err != nil {
		return err
	}
	a.ReportsTo = newManager
	meta, err := yaml.Marshal(a)
	if err != nil {
		return err
	}
	if err := writeAtomic(filepath.Join(s.path("agents", slug), "agent.yaml"), meta, 0o644); err != nil {
		return err
	}
	s.Graph().OrgChanged()
	return nil
}

// ArchiveAgent moves an active agent into agents/_archived/ with status
// and archived_at updated.
//
// The agent's published tree, public/<slug>, stays where it is: its
// nodes stay in the graph, owned by an archived agent.
//
// The move holds orgMu: the graph maintainer lists active agents and
// then archived ones under it, and a rename landing between the two
// lists would show the agent twice. The graph hears of the archive
// once the move is done.
func (s *FSStore) ArchiveAgent(slug string) error {
	if err := s.archiveAgent(slug); err != nil {
		return err
	}
	s.Graph().OrgChanged()
	return nil
}

func (s *FSStore) archiveAgent(slug string) error {
	s.orgMu.Lock()
	defer s.orgMu.Unlock()
	a, err := s.GetAgent(slug)
	if err != nil {
		return err
	}
	now := time.Now().UTC()
	a.Status = StatusArchived
	a.ArchivedAt = &now
	src := s.path("agents", slug)
	meta, err := yaml.Marshal(a)
	if err != nil {
		return err
	}
	if err := writeAtomic(filepath.Join(src, "agent.yaml"), meta, 0o644); err != nil {
		return err
	}
	dst := s.path("agents", "_archived", slug)
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	return os.Rename(src, dst)
}

// ReadRole returns an agent's role.md body.
func (s *FSStore) ReadRole(slug string) (string, error) {
	return s.readAgentFile(slug, "role.md")
}

// WriteRole replaces an agent's role.md atomically.
func (s *FSStore) WriteRole(slug, body string) error {
	return s.writeAgentFile(slug, "role.md", body)
}

// agentMemoryFilename is the on-disk name for the "agent memory"
// message — the evolving summary an agent curates for themselves
// and for their future incarnations.
const agentMemoryFilename = "agent_memory.md"

// ReadAgentMemory returns the agent's accumulated memory. Returns
// ErrNotFound when no memory file exists yet.
func (s *FSStore) ReadAgentMemory(slug string) (string, error) {
	return s.readAgentFile(slug, agentMemoryFilename)
}

// WriteAgentMemory replaces the agent's memory file atomically.
func (s *FSStore) WriteAgentMemory(slug, body string) error {
	return s.writeAgentFile(slug, agentMemoryFilename, body)
}

// Errors returned by StrReplaceAgentMemory so dispatchers can render
// meaningful tool_result messages.
var (
	ErrAgentMemoryStrNotFound = errors.New("old_str not found in agent memory")
	ErrAgentMemoryStrMultiple = errors.New("old_str matches more than once in agent memory; expand the match until it is unique")
	// ErrAgentMemoryStrFoldOnly fires when old_str doesn't byte-match
	// the stored memory even after NFC normalization, but a
	// confusables-folded version (em/en dash → "-", curly → straight
	// quotes, NBSP → space, etc.) does match. Signals the agent typed
	// visually-similar but byte-different characters; the fix is to
	// view memory and copy bytes verbatim rather than retype.
	ErrAgentMemoryStrFoldOnly = errors.New("old_str only matches agent memory after folding visually-similar characters (em/en dash, curly quotes, NBSP, etc.); view memory and copy the exact bytes instead of retyping")
)

// AppendAgentMemory appends text to the agent's agent_memory.md with a
// blank-line separator. Creates the file if it does not yet exist.
// Both the prior content and the appended text are NFC-normalized so the
// stored file converges to one canonical form, which is what later
// str_replace calls match against.
func (s *FSStore) AppendAgentMemory(slug, text string) error {
	priorRaw, err := s.ReadAgentMemory(slug)
	if err != nil && !errors.Is(err, ErrNotFound) {
		return err
	}
	return s.WriteAgentMemory(slug, appendMemoryText(priorRaw, text))
}

// StrReplaceAgentMemory performs an exactly-once substring replacement
// inside the agent's agent_memory.md. See strReplaceMemoryText for the
// matching rules and the errors it returns.
func (s *FSStore) StrReplaceAgentMemory(slug, oldStr, newStr string) error {
	raw, err := s.ReadAgentMemory(slug)
	if err != nil {
		return err
	}
	next, err := strReplaceMemoryText(raw, oldStr, newStr)
	if err != nil {
		return err
	}
	return s.WriteAgentMemory(slug, next)
}

// AgentMemoryHabitsFilename holds the agent's habits — the short list
// of learned rules of behaviour that sits above semantic
// memory in the system prompt and below the handbook and role. A
// separate file from agent_memory.md so the rotation-only write gate
// and the archive snapshot can treat it on its own, and prefixed
// agent_memory_ so a directory listing keeps the memory family
// together: role.md is governance, agent_memory_* is memory. Archived
// generations keep the same name.
const AgentMemoryHabitsFilename = "agent_memory_habits.md"

// ReadAgentPrinciples returns the agent's operating principles, or
// ErrNotFound before the first one is written.
func (s *FSStore) ReadAgentHabits(slug string) (string, error) {
	return s.readAgentFile(slug, AgentMemoryHabitsFilename)
}

// WriteAgentPrinciples replaces the principles file atomically.
func (s *FSStore) WriteAgentHabits(slug, body string) error {
	return s.writeAgentFile(slug, AgentMemoryHabitsFilename, body)
}

// AppendAgentPrinciples appends one principle with a blank-line
// separator, creating the file on first use. Same normalization as
// AppendAgentMemory.
func (s *FSStore) AppendAgentHabits(slug, text string) error {
	priorRaw, err := s.ReadAgentHabits(slug)
	if err != nil && !errors.Is(err, ErrNotFound) {
		return err
	}
	return s.WriteAgentHabits(slug, appendMemoryText(priorRaw, text))
}

// StrReplaceAgentPrinciples is StrReplaceAgentMemory for the
// principles file — same matching rules, same errors.
func (s *FSStore) StrReplaceAgentHabits(slug, oldStr, newStr string) error {
	raw, err := s.ReadAgentHabits(slug)
	if err != nil {
		return err
	}
	next, err := strReplaceMemoryText(raw, oldStr, newStr)
	if err != nil {
		return err
	}
	return s.WriteAgentHabits(slug, next)
}

// appendMemoryText is the append rule shared by agent_memory.md and
// agent_memory_habits.md: NFC-normalize both sides, join with one
// blank line, end with a newline.
func appendMemoryText(priorRaw, text string) string {
	prior := strings.TrimRight(norm.NFC.String(priorRaw), "\n")
	add := norm.NFC.String(text)
	var next string
	if prior == "" {
		next = add
	} else {
		next = prior + "\n\n" + add
	}
	if !strings.HasSuffix(next, "\n") {
		next += "\n"
	}
	return next
}

// strReplaceMemoryText is the exactly-once substring replacement shared
// by agent_memory.md and agent_memory_habits.md. Returns the new
// body.
//
// Matching is byte-exact after NFC normalization of both the file and
// the agent's old_str/new_str. NFC handles the "same character, different
// byte sequence" case (e.g. macOS's NFD `e` + U+0301 vs the more common
// NFC U+00E9 for `é`) without changing visible character identity.
//
// On a tier-1 miss, a tier-2 confusables fold (em/en dash → "-", curly
// quotes → straight, NBSP → space, ellipsis → "...") is applied to both
// sides as a hint check only — if the folded version matches, we return
// ErrAgentMemoryStrFoldOnly so the dispatcher can tell the agent it
// typed a different (but visually similar) character. The replacement
// itself never uses the fold; we don't silently swap characters.
//
// Returns ErrAgentMemoryStrNotFound when old_str doesn't match under
// either tier, ErrAgentMemoryStrMultiple when NFC matches more than
// once, ErrAgentMemoryStrFoldOnly for the tier-2 hint case. new_str may
// be empty (deletes the matched range).
func strReplaceMemoryText(raw, oldStr, newStr string) (string, error) {
	if oldStr == "" {
		return "", errors.New("old_str cannot be empty")
	}
	prior := norm.NFC.String(raw)
	oldS := norm.NFC.String(oldStr)
	newS := norm.NFC.String(newStr)
	switch strings.Count(prior, oldS) {
	case 1:
		return strings.Replace(prior, oldS, newS, 1), nil
	case 0:
		if strings.Contains(memoryConfusablesFold(prior), memoryConfusablesFold(oldS)) {
			return "", ErrAgentMemoryStrFoldOnly
		}
		return "", ErrAgentMemoryStrNotFound
	default:
		return "", ErrAgentMemoryStrMultiple
	}
}

// memoryConfusablesReplacer maps visually-similar Unicode characters to
// their plain ASCII equivalents. Used only to detect tier-2 fold-only
// matches in StrReplaceAgentMemory — never applied to stored content.
//
// Coverage is deliberately limited to the families the model commonly
// re-types incorrectly (dashes, quotes, the NBSP, the ellipsis). Adding
// more aggressive folds here risks masking real character mistakes.
var memoryConfusablesReplacer = strings.NewReplacer(
	"—", "-", // em dash
	"–", "-", // en dash
	"‒", "-", // figure dash
	"―", "-", // horizontal bar
	"−", "-", // minus sign
	"“", `"`, // left double quote
	"”", `"`, // right double quote
	"„", `"`, // double low-9 quote
	"‟", `"`, // double high-reversed-9 quote
	"‘", "'", // left single quote
	"’", "'", // right single quote
	"‚", "'", // single low-9 quote
	"‛", "'", // single high-reversed-9 quote
	" ", " ", // no-break space
	" ", " ", // thin space
	" ", " ", // hair space
	" ", " ", // narrow no-break space
	"…", "...", // horizontal ellipsis
)

func memoryConfusablesFold(s string) string {
	return memoryConfusablesReplacer.Replace(s)
}

func (s *FSStore) readAgentFile(slug, name string) (string, error) {
	b, err := os.ReadFile(s.path("agents", slug, name))
	if errors.Is(err, os.ErrNotExist) {
		return "", ErrNotFound
	}
	if err != nil {
		return "", err
	}
	return string(b), nil
}

func (s *FSStore) writeAgentFile(slug, name, body string) error {
	dir := s.path("agents", slug)
	if _, err := os.Stat(dir); errors.Is(err, fs.ErrNotExist) {
		return ErrNotFound
	}
	return writeAtomic(filepath.Join(dir, name), []byte(body), 0o644)
}

// claudeSessionFilename is the per-agent sidecar that stores the
// Claude Code CLI session_id we resume against on subsequent stream
// calls. Lives next to chat.jsonl so it shares the chat's lifecycle
// — archived/rotated/deleted together with the chat state.
const claudeSessionFilename = "claude_session.json"

type claudeSessionFile struct {
	SessionID string    `json:"session_id"`
	UpdatedAt time.Time `json:"updated_at"`
}

// ReadClaudeSessionID returns the persisted Claude Code CLI session_id
// for slug (empty string when no session has been established yet).
// Used by the SDK transport to decide whether to pass --resume.
func (s *FSStore) ReadClaudeSessionID(slug string) (string, error) {
	b, err := os.ReadFile(s.path("agents", slug, claudeSessionFilename))
	if errors.Is(err, os.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	var sf claudeSessionFile
	if err := json.Unmarshal(b, &sf); err != nil {
		return "", fmt.Errorf("%s: %w", claudeSessionFilename, err)
	}
	return sf.SessionID, nil
}

// WriteClaudeSessionID persists the Claude Code CLI session_id for
// slug so the next stream call can --resume against it. No-op when id
// is empty.
func (s *FSStore) WriteClaudeSessionID(slug, id string) error {
	if id == "" {
		return nil
	}
	dir := s.path("agents", slug)
	if _, err := os.Stat(dir); errors.Is(err, fs.ErrNotExist) {
		return ErrNotFound
	}
	body, err := json.MarshalIndent(claudeSessionFile{SessionID: id, UpdatedAt: time.Now().UTC()}, "", "  ")
	if err != nil {
		return err
	}
	return writeAtomic(filepath.Join(dir, claudeSessionFilename), body, 0o644)
}

// ClearClaudeSessionID removes the persisted session_id for slug so
// the next stream call starts a fresh CLI session. Called on chat
// rotation (New chat button) and on detected stale sessions (e.g.
// the CLI's session log is gone). Missing file is not an error.
func (s *FSStore) ClearClaudeSessionID(slug string) error {
	err := os.Remove(s.path("agents", slug, claudeSessionFilename))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

// activeTurnFilename is the per-agent marker file the chat loop writes
// when a turn begins. It survives across Kivali process restarts so the
// boot scan can detect a turn that was killed mid-flight (OOM, segfault,
// pod restart) and surface a runtime-disruption entry.
const activeTurnFilename = "active_turn.json"

// ActiveTurnMarker is the on-disk shape of the per-agent
// active_turn.json file. Carried as diagnostic info in the
// runtime-disruption chat entry so an operator (or the agent
// reading their own history) can see what they were doing when
// they were killed.
type ActiveTurnMarker struct {
	StartedAt time.Time `json:"started_at"`
	Source    string    `json:"source"` // "chat" | "release" | …
	Model     string    `json:"model,omitempty"`
}

// MarkActiveTurn writes the active-turn marker for slug. Call at the
// top of a chat-loop spawn (before any model work). Atomic write so a
// crash during the write doesn't leave a half-baked marker.
func (s *FSStore) MarkActiveTurn(slug string, m ActiveTurnMarker) error {
	if m.StartedAt.IsZero() {
		m.StartedAt = time.Now().UTC()
	}
	dir := s.path("agents", slug)
	if _, err := os.Stat(dir); errors.Is(err, fs.ErrNotExist) {
		return ErrNotFound
	}
	body, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	return writeAtomic(filepath.Join(dir, activeTurnFilename), body, 0o644)
}

// ClearActiveTurn removes slug's active-turn marker. Called from the
// chat loop's post-completion defer (turn ended cleanly) and from the
// stop handler (graceful interrupt — we don't want it to look like a
// kill on the next boot scan). Missing file is not an error.
func (s *FSStore) ClearActiveTurn(slug string) error {
	err := os.Remove(s.path("agents", slug, activeTurnFilename))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

// ReadActiveTurn returns the marker for slug, or (zero, nil) when no
// marker is present. Used by the boot scan to populate the diagnostic
// content of the runtime-disruption chat entry.
func (s *FSStore) ReadActiveTurn(slug string) (ActiveTurnMarker, error) {
	b, err := os.ReadFile(s.path("agents", slug, activeTurnFilename))
	if errors.Is(err, os.ErrNotExist) {
		return ActiveTurnMarker{}, nil
	}
	if err != nil {
		return ActiveTurnMarker{}, err
	}
	var m ActiveTurnMarker
	if err := json.Unmarshal(b, &m); err != nil {
		return ActiveTurnMarker{}, fmt.Errorf("%s: %w", activeTurnFilename, err)
	}
	return m, nil
}

// contextWindowFilename is the per-agent sidecar holding the most
// recent turn's real context-window occupancy. It exists because the
// context-fill ring is rendered on plain page loads (no live turn in
// flight), and chat.jsonl carries no token counts — the only way to
// show the TRUE occupancy after a reload is to persist what the last
// turn's model call actually reported. Without this file the ring
// falls back to a chars/4 estimate of the transcript, which omits the
// system prompt, role, tool/skill/MCP schemas, injected memory, and
// pulled-in file content — in practice the larger half of the prompt.
const contextWindowFilename = "context_window.json"

// ContextWindowStat is the on-disk shape of context_window.json.
// ContextTokens is the single-call window occupancy (input +
// cache_read + cache_create) of the last assistant request of the most
// recent turn — deliberately NOT the per-turn accumulated billing
// total, which over-counts by the number of internal tool-loop
// round-trips. See provider.CompleteResponse.ContextTokens.
type ContextWindowStat struct {
	ContextTokens int       `json:"context_tokens"`
	UpdatedAt     time.Time `json:"updated_at"`
}

// WriteContextWindow records slug's latest real context occupancy.
// Called from the chat-turn done handler. Atomic write; overwrites the
// prior value (latest turn wins). No-op for a non-positive count so a
// transport that doesn't report per-call usage can't blow away a good
// prior reading with a zero.
func (s *FSStore) WriteContextWindow(slug string, contextTokens int) error {
	if contextTokens <= 0 {
		return nil
	}
	dir := s.path("agents", slug)
	if _, err := os.Stat(dir); errors.Is(err, fs.ErrNotExist) {
		return ErrNotFound
	}
	body, err := json.MarshalIndent(ContextWindowStat{
		ContextTokens: contextTokens,
		UpdatedAt:     time.Now().UTC(),
	}, "", "  ")
	if err != nil {
		return err
	}
	return writeAtomic(filepath.Join(dir, contextWindowFilename), body, 0o644)
}

// ReadContextWindow returns slug's last recorded occupancy, or
// (zero, nil) when no turn has run yet (cold start — callers fall back
// to the chars/4 estimate).
func (s *FSStore) ReadContextWindow(slug string) (ContextWindowStat, error) {
	b, err := os.ReadFile(s.path("agents", slug, contextWindowFilename))
	if errors.Is(err, os.ErrNotExist) {
		return ContextWindowStat{}, nil
	}
	if err != nil {
		return ContextWindowStat{}, err
	}
	var st ContextWindowStat
	if err := json.Unmarshal(b, &st); err != nil {
		return ContextWindowStat{}, fmt.Errorf("%s: %w", contextWindowFilename, err)
	}
	return st, nil
}

// ClearContextWindow removes slug's occupancy sidecar so the ring
// snaps back to the cold-start estimate. Called on chat rotation (New
// chat) — the archived transcript's occupancy says nothing about the
// fresh chat. Missing file is not an error.
func (s *FSStore) ClearContextWindow(slug string) error {
	err := os.Remove(s.path("agents", slug, contextWindowFilename))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

// pendingRotationFilename is the per-agent marker file written when
// a chat-rotation has been requested but not yet finalized. The chat
// loop's post-completion defer (consumeRotationIfReady) reads and
// removes it; finalizeRotation archives the chat using the snapshot
// it carries. On disk so a Kivali crash mid-rotation doesn't lose
// the rotation semantics — the next finalize after recovery still
// finds the marker and archives.
const pendingRotationFilename = "pending_rotation.json"

// PendingRotation is the on-disk shape of the rotation marker.
// PriorMemory is a snapshot of agent_memory.md taken at the moment
// the rotation was requested — kept so finalizeRotation can write it
// alongside the archive dir even though agent_memory.md will have
// been overwritten by the agent's own edits during the rotation
// chat. RequestedBy is purely diagnostic; the future agent-self-
// rotation tool stamps it differently from the CEO-clicks-new-chat
// path so logs are easy to read.
type PendingRotation struct {
	PriorMemory string    `json:"prior_memory"`
	RequestedAt time.Time `json:"requested_at"`
	RequestedBy string    `json:"requested_by"` // "ceo" | "agent" | …

	// Timestamp is the archive generation this rotation will land
	// in, minted when the rotation was requested so the memory-update
	// turn can cite the coming episode as [[ep:<Timestamp>]]. Empty on
	// a marker written before this field existed; finalize mints one.
	Timestamp string `json:"timestamp,omitempty"`
	// PriorHabits is the habits snapshot (agent_memory_habits.md),
	// same role as PriorMemory.
	PriorHabits string `json:"prior_habits,omitempty"`
}

// WritePendingRotation persists the rotation marker for slug. Atomic
// write so a crash during the write doesn't leave a half-baked file.
// Overwrites any existing marker — the latest request wins, which
// matches the in-memory semantics this replaced.
func (s *FSStore) WritePendingRotation(slug string, m PendingRotation) error {
	if m.RequestedAt.IsZero() {
		m.RequestedAt = time.Now().UTC()
	}
	dir := s.path("agents", slug)
	if _, err := os.Stat(dir); errors.Is(err, fs.ErrNotExist) {
		return ErrNotFound
	}
	body, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	return writeAtomic(filepath.Join(dir, pendingRotationFilename), body, 0o644)
}

// ReadPendingRotation returns the marker for slug. Returns (zero,
// false, nil) when no marker is present — the common path. Returns
// (zero, false, err) only on an actual filesystem read failure or a
// corrupt JSON file.
func (s *FSStore) ReadPendingRotation(slug string) (PendingRotation, bool, error) {
	b, err := os.ReadFile(s.path("agents", slug, pendingRotationFilename))
	if errors.Is(err, os.ErrNotExist) {
		return PendingRotation{}, false, nil
	}
	if err != nil {
		return PendingRotation{}, false, err
	}
	var m PendingRotation
	if err := json.Unmarshal(b, &m); err != nil {
		return PendingRotation{}, false, fmt.Errorf("%s: %w", pendingRotationFilename, err)
	}
	return m, true, nil
}

// ClearPendingRotation removes slug's rotation marker. Called from
// finalizeRotation after the archive lands and from the agent fire
// flow's drain helper. Missing file is not an error.
func (s *FSStore) ClearPendingRotation(slug string) error {
	err := os.Remove(s.path("agents", slug, pendingRotationFilename))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

// ListOrphanActiveTurns returns the slugs of every agent whose
// active_turn.json marker is present. Called once at Kivali boot.
// In a clean shutdown every marker has been cleared by the chat
// loop's post-completion defer, so the list is empty; anything
// returned here means a turn was killed mid-flight and the agent
// needs a runtime-disruption entry + auto-spawn (subject to the
// circuit-breaker check).
func (s *FSStore) ListOrphanActiveTurns() ([]string, error) {
	agents, err := s.ListActiveAgents()
	if err != nil {
		return nil, err
	}
	var orphans []string
	for _, a := range agents {
		marker, err := s.ReadActiveTurn(a.Slug)
		if err != nil {
			// Log-and-skip rather than fail boot — a corrupt
			// marker file shouldn't block the rest of the scan.
			continue
		}
		if marker.StartedAt.IsZero() {
			continue
		}
		orphans = append(orphans, a.Slug)
	}
	return orphans, nil
}

// AppendChatMessage appends a JSONL entry to the agent's chat file. Safe
// for concurrent callers within a single process via a per-agent mutex.
//
// A row that carries attachments is the edge that keeps the agent's
// /files/attachments/ farm current: once the row is on disk its files
// are linked, so they resolve before any turn that reads the row
// starts. A failed sync is logged, not returned: the row landed. The
// CEO has no farm; its rows link nothing.
func (s *FSStore) AppendChatMessage(slug string, msg ChatMessage) error {
	if err := s.AppendChatMessageLinkLater(slug, msg); err != nil {
		return err
	}
	s.LinkChatAttachments(slug)
	return nil
}

// AppendChatMessageLinkLater is AppendChatMessage for a caller holding
// a lock the attachment sync must not run under (the web layer's
// streamMu, which every stream event waits on). The row lands and its
// attachments are marked unlinked; the caller runs LinkChatAttachments
// once it has released its lock. A turn's spawn runs it too, before it
// reads the chat, so a row never reaches a turn ahead of its links.
func (s *FSStore) AppendChatMessageLinkLater(slug string, msg ChatMessage) error {
	if err := s.appendChatMessage(slug, msg); err != nil {
		return err
	}
	if len(msg.Attachments) > 0 && slug != graph.CEOSlug {
		s.unlinkedChatMu.Lock()
		if s.unlinkedChat == nil {
			s.unlinkedChat = map[string]bool{}
		}
		s.unlinkedChat[slug] = true
		s.unlinkedChatMu.Unlock()
	}
	return nil
}

// LinkChatAttachments links the attachments of every row appended with
// AppendChatMessageLinkLater since the last link; nothing to do costs a
// map lookup. A caller that returns has the links in place: one that
// arrives while another links waits for it. A failed sync is logged
// and left marked, for the next caller to retry.
func (s *FSStore) LinkChatAttachments(slug string) {
	mu := chatLinkLock(slug)
	mu.Lock()
	defer mu.Unlock()
	s.unlinkedChatMu.Lock()
	marked := s.unlinkedChat[slug]
	delete(s.unlinkedChat, slug)
	s.unlinkedChatMu.Unlock()
	if !marked {
		return
	}
	if _, err := s.SyncAgentAttachments(slug, nil); err != nil {
		log.Printf("chat %s: link attachments: %v", slug, err)
		s.unlinkedChatMu.Lock()
		if s.unlinkedChat == nil {
			s.unlinkedChat = map[string]bool{}
		}
		s.unlinkedChat[slug] = true
		s.unlinkedChatMu.Unlock()
	}
}

func (s *FSStore) appendChatMessage(slug string, msg ChatMessage) error {
	if msg.TS.IsZero() {
		msg.TS = time.Now().UTC()
	}
	mu := chatLock(slug)
	mu.Lock()
	defer mu.Unlock()
	dir := s.path("agents", slug)
	if _, err := os.Stat(dir); errors.Is(err, fs.ErrNotExist) {
		return ErrNotFound
	}
	line, err := json.Marshal(msg)
	if err != nil {
		return err
	}
	if err := appendJSONL(filepath.Join(dir, "chat.jsonl"), append(line, '\n')); err != nil {
		return err
	}
	// The row is on disk, so any of its attachments that were linked
	// ahead of it (see SyncAgentAttachments) can now be found by the
	// walk.
	s.releasePendingAttachments(slug, msg.Attachments)
	return nil
}

// AppendSubagentMessage appends a JSONL entry to the subagent's chat
// file at agents/<parent>/subagents/<id>/chat.jsonl. Same shape as
// AppendChatMessage but routed to the subagent transcript instead of
// the parent's. Used by the agent-pod path's subagent runs:
// TurnEvents posted by an in-pod `claude -p` for a subagent land in
// the subagent transcript so the transcript view shows the run
// stream by stream.
//
// The mutex is keyed on parent+id so concurrent subagents under the
// same parent don't serialize each other's appends. Creates the dir
// + file on first use (subagent runs allocate the id before any
// stream event lands).
func (s *FSStore) AppendSubagentMessage(parent, id string, msg ChatMessage) error {
	if parent == "" || id == "" {
		return errors.New("AppendSubagentMessage: parent + id required")
	}
	if msg.TS.IsZero() {
		msg.TS = time.Now().UTC()
	}
	mu := chatLock(parent + "/subagents/" + id)
	mu.Lock()
	defer mu.Unlock()
	dir := s.path("agents", parent, "subagents", id)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	line, err := json.Marshal(msg)
	if err != nil {
		return err
	}
	return appendJSONL(filepath.Join(dir, "chat.jsonl"), append(line, '\n'))
}

// ReadChatHistory returns the agent's chat messages in order.
func (s *FSStore) ReadChatHistory(slug string) ([]ChatMessage, error) {
	path := s.path("agents", slug, "chat.jsonl")
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	var out []ChatMessage
	err = jsonlLines(f, 8*1024*1024, func(line []byte) error {
		var m ChatMessage
		if err := json.Unmarshal(line, &m); err != nil {
			return fmt.Errorf("chat.jsonl %s: %w", slug, err)
		}
		out = append(out, m)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}
