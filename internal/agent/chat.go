package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"path/filepath"
	"strings"
	"time"

	"github.com/kivali-ai/kivali/internal/provider"
	"github.com/kivali-ai/kivali/internal/store"
)

// StreamEmitter is the minimal interface the chat-path UI events use
// to publish to subscribers. Decoupled from http.ResponseWriter so a
// detached goroutine can emit after the originating HTTP request has
// closed. The web package's chatHub satisfies this; the agentpod
// turn handler in internal/web emits through it on every TurnEvent
// the agent runtime POSTs back.
type StreamEmitter interface {
	Emit(kind string, payload any)
	Checkpoint()
	TrimDeltas()
}

// SharedFile is one (sha, display-name) pair pulled from a share_file
// ack. The list is what the live UI renderer iterates to draw one
// download link per file inside the chat bubble.
type SharedFile struct {
	SHA  string
	Name string
}

// EmitFileSharedFromMCPAck synthesizes a file_shared SSE event for the
// chat client when a share_file MCP tool_use completes. The MCP-side
// handler already persisted the chat-bubble (with all attachments);
// this just gets the live UI to paint it without a page reload.
// Mirrors the publish_* path's EmitDocPublishedFromMCPAck.
//
// Used by the agent-pod tool-result handler in internal/web when
// forwarding tool_result events from the agent runtime over UDS.
//
// now stamps the event's ts (unix ms, as chat_message carries) so the
// transcript can place the chip among the turn's rows.
func EmitFileSharedFromMCPAck(em StreamEmitter, tu provider.StreamEvent, ack string, now time.Time) {
	files := parseShareFileAck(ack)
	if len(files) == 0 {
		return
	}
	var input struct {
		Caption string `json:"caption"`
	}
	_ = json.Unmarshal(tu.ToolInput, &input)
	out := make([]map[string]any, 0, len(files))
	for _, f := range files {
		out = append(out, map[string]any{"sha": f.SHA, "name": f.Name})
	}
	em.Emit("file_shared", map[string]any{
		"tool_use_id": tu.ToolUseID,
		"files":       out,
		"caption":     strings.TrimSpace(input.Caption),
		"ts":          now.UnixMilli(),
	})
}

// parseShareFileAck pulls per-file (sha, name) pairs out of the MCP
// share_file ack string. The ack body produced by the dispatcher is:
//
//	Shared in this conversation:
//	- <name> (sha=<sha>)
//	- <name> (sha=<sha>)
//	[Caption: <text>]
//
// Each "- <name> (sha=<sha>)" line surfaces one entry. Lines that
// don't fit the pattern (a stray header line, an unexpected suffix)
// are skipped. Returns nil when the ack doesn't have any parseable
// file lines.
func parseShareFileAck(ack string) []SharedFile {
	const linePrefix = "- "
	const shaSep = " (sha="
	var out []SharedFile
	for _, line := range strings.Split(ack, "\n") {
		line = strings.TrimRight(line, "\r")
		if !strings.HasPrefix(line, linePrefix) {
			continue
		}
		rest := line[len(linePrefix):]
		i := strings.LastIndex(rest, shaSep)
		if i < 0 {
			continue
		}
		name := rest[:i]
		tail := rest[i+len(shaSep):]
		j := strings.Index(tail, ")")
		if j < 0 {
			continue
		}
		sha := tail[:j]
		if name == "" || sha == "" {
			continue
		}
		out = append(out, SharedFile{SHA: sha, Name: name})
	}
	return out
}

// EmitDocPublishedFromMCPAck synthesizes a doc_published SSE event +
// chat.jsonl row for the chat client when a publish_* MCP tool_use
// completes. Used by the agent-pod tool-result handler in internal/web
// to drive the friendly chip UI when forwarding tool_result events
// from the agent runtime over UDS.
//
// Method on Runtime (vs. emitFileSharedFromMCPAck which is a free
// function) because the row write needs access to r.Store. now is both
// the row's TS and the event's ts (unix ms, as chat_message carries).
func (r *Runtime) EmitDocPublishedFromMCPAck(slug string, em StreamEmitter, tu provider.StreamEvent, ack string, now time.Time) {
	parsed := parseMCPPublishAck(tu.ToolName, tu.ToolInput, ack)
	payload := map[string]any{
		"tool_use_id": tu.ToolUseID,
		"tool_name":   tu.ToolName,
		"doc_type":    parsed.MessageType,
		"doc_title":   parsed.Title,
		"doc_to":      parsed.To,
		"doc_path":    parsed.Path,
		"ts":          now.UnixMilli(),
	}
	// The published message itself, so the live row expands to the
	// card the transcript draws after a reload.
	if parsed.Path != "" {
		if msg, err := r.Store.ReadMessage(filepath.Join(r.Store.Root(), parsed.Path)); err == nil {
			payload["doc_body"] = strings.TrimRight(msg.Body, "\n")
			files := make([]map[string]any, 0, len(msg.Attachments))
			for _, a := range msg.Attachments {
				files = append(files, map[string]any{"sha": a.SHA, "name": a.Name})
			}
			payload["doc_files"] = files
			if msg.Assignment != nil {
				payload["doc_assignment"] = msg.Assignment.ID
			}
		}
	}
	em.Emit("doc_published", payload)
	content := fmt.Sprintf("📄 %s %q → %s", parsed.MessageType, parsed.Title, parsed.To)
	if err := r.Store.AppendChatMessage(slug, store.ChatMessage{
		Role:       store.RoleSent,
		Kind:       "doc_published",
		Content:    content,
		ToolUseID:  tu.ToolUseID,
		ToolName:   tu.ToolName,
		MessageRef: parsed.Path,
		TS:         now.UTC(),
	}); err != nil {
		log.Printf("chat %s: append doc_published: %v", slug, err)
	}
}

// mcpPublishAck is the decoded shape of a publish_* MCP tool_use +
// its paired tool_result ack string.
type mcpPublishAck struct {
	MessageType string
	Title       string
	To          string
	Path        string
}

// parseMCPPublishAck derives doc_published metadata from a publish_*
// tool_use's name + input + the MCP server's ack string.
//
// Ack format produced by internal/mcp/publish_tools.go:
//
//	publish_<type> published: "<title>" → <to> (saved to <path>)
//
// Title and `to` come from the tool input (ground truth); path comes
// from the ack's suffix. If any field is unparseable the
// corresponding field is returned empty.
func parseMCPPublishAck(toolName string, toolInput json.RawMessage, ack string) mcpPublishAck {
	var input struct {
		Title string `json:"title"`
		To    string `json:"to"`
	}
	_ = json.Unmarshal(toolInput, &input)
	out := mcpPublishAck{
		MessageType: strings.TrimPrefix(toolName, "publish_"),
		Title:       input.Title,
		To:          input.To,
	}
	if i := strings.LastIndex(ack, "(saved to "); i >= 0 {
		rest := ack[i+len("(saved to "):]
		if j := strings.Index(rest, ")"); j >= 0 {
			out.Path = rest[:j]
		}
	}
	if out.To == "" {
		if i := strings.Index(ack, "→ "); i >= 0 {
			rest := ack[i+len("→ "):]
			if j := strings.Index(rest, " (saved to "); j >= 0 {
				out.To = rest[:j]
			}
		}
	}
	return out
}

// BuildChatContext loads everything needed for one BuildRequest.
//
// It is the ONLY builder: every per-turn input is wired here and
// nowhere else.
//
// It takes a pre-read chat history so the caller can do the
// rotation-replay reordering before us.
func (r *Runtime) BuildChatContext(ctx context.Context, a store.Agent, hist []store.ChatMessage) (Context, error) {
	handbook, err := r.Store.ReadHandbook()
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		return Context{}, err
	}
	actives, err := r.Store.ListActiveAgents()
	if err != nil {
		return Context{}, err
	}
	files, err := r.Store.ListProjectFiles()
	if err != nil {
		return Context{}, err
	}
	skills, err := r.Store.ListEnabledSkills()
	if err != nil {
		return Context{}, err
	}
	role, err := r.Store.ReadRole(a.Slug)
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		return Context{}, err
	}
	agentMemory, err := r.Store.ReadAgentMemory(a.Slug)
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		return Context{}, err
	}
	principles, err := r.Store.ReadAgentHabits(a.Slug)
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		return Context{}, err
	}
	br, err := r.Store.ReadBranding()
	if err != nil {
		return Context{}, err
	}
	return Context{
		Agent:          a,
		IsChiefOfStaff: a.Slug == "chief-of-staff",
		Handbook:       handbook,
		Owner:          br.Owner(),
		OrgChart:       OrgChartMarkdown(actives, br.Owner().Label()),
		ProjectFiles:   files,
		Skills:         skills,
		Role:           role,
		AgentMemory:    agentMemory,
		AgentHabits:    principles,
		ChatHistory:    hist,
		FileText:       FileTextFn(ctx, r.Store),
		Attachments:    r.Store,
		Model:          modelFor(a, r.Defaults.AgentModel),
		Effort:         a.Effort,
		MaxTokens:      r.Defaults.MaxTokens,
		Temperature:    r.Defaults.Temperature,
		Purpose:        "chat",
	}, nil
}

// modelFor returns the agent's per-agent model when set, else the
// runtime default.
func modelFor(a store.Agent, defaultModel string) string {
	if a.Model != "" {
		return a.Model
	}
	return defaultModel
}
