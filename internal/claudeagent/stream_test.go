package claudeagent

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"sync"
	"testing"

	"github.com/kivali-ai/kivali/internal/provider"
)

// TestContentBlocksForWireDropsToolUse locks in the contract that
// contentBlocksForWire emits only the two shapes valid inside a
// user-role message on stream-json stdin: text and tool_result.
// tool_use blocks are dropped because the SDK transport only feeds
// user releases — a well-formed user release has no tool_use. If a caller
// slips one in by mistake, we must not echo it to the CLI (which
// would log it as an orphan node in the session tree and 400 the
// next API call with "tool use concurrency issues").
func TestContentBlocksForWireDropsToolUse(t *testing.T) {
	blocks := []provider.ContentBlock{
		{Type: provider.ContentText, Text: "the CEO replied"},
		{
			// A stray tool_use that shouldn't be here — dropped, not narrated.
			Type:      provider.ContentToolUse,
			ToolUseID: "toolu_abc",
			ToolName:  "publish_ceo_approval_request",
			ToolInput: json.RawMessage(`{"title":"Hire"}`),
		},
		{
			Type:              provider.ContentToolResult,
			ToolResultID:      "toolu_prev",
			ToolResultContent: "published: ...",
			ToolResultIsError: false,
		},
	}
	got := contentBlocksForWire(blocks)
	if len(got) != 2 {
		t.Fatalf("want 2 wire blocks (text + tool_result, tool_use dropped), got %d: %+v", len(got), got)
	}
	if got[0]["type"] != "text" {
		t.Errorf("block 0: want text, got %v", got[0])
	}
	if got[1]["type"] != "tool_result" || got[1]["tool_use_id"] != "toolu_prev" {
		t.Errorf("block 1: tool_result shape wrong: %+v", got[1])
	}
}

// TestContextTokensIsLatestNotAccumulated locks in the distinction the
// context-fill ring depends on: across a multi-step tool loop, Usage
// ACCUMULATES (Anthropic bills the sum of every /v1/messages call) but
// ContextTokens is LATEST-WINS — the single last call's window
// occupancy. Conflating the two is exactly the under/over-count bug
// this guards: feeding the accumulated cache_read to the ring would
// over-count by the number of internal round-trips.
func TestContextTokensIsLatestNotAccumulated(t *testing.T) {
	mkMsg := func(in, cacheRead, cacheCreate int) json.RawMessage {
		b, err := json.Marshal(message{
			Role: "assistant",
			Usage: messageUsage{
				InputTokens:              in,
				CacheReadInputTokens:     cacheRead,
				CacheCreationInputTokens: cacheCreate,
			},
		})
		if err != nil {
			t.Fatalf("marshal message: %v", err)
		}
		return b
	}

	// Two back-to-back assistant calls in one turn. The second sees more
	// context (it's read the first call's tool result back in).
	call1 := mkMsg(1000, 5000, 200) // window occupancy 6,200
	call2 := mkMsg(1200, 9000, 300) // window occupancy 10,500

	assertFinal := func(t *testing.T, f *provider.CompleteResponse) {
		t.Helper()
		// Usage accumulates across both calls.
		if got, want := f.Usage.InputTokens, 2200; got != want {
			t.Errorf("Usage.InputTokens = %d, want %d (accumulated)", got, want)
		}
		if got, want := f.Usage.CacheReadTokens, 14000; got != want {
			t.Errorf("Usage.CacheReadTokens = %d, want %d (accumulated)", got, want)
		}
		// ContextTokens is the SECOND (last) call's occupancy only.
		if got, want := f.ContextTokens, 10500; got != want {
			t.Errorf("ContextTokens = %d, want %d (latest single call, not sum)", got, want)
		}
	}

	t.Run("subprocStream", func(t *testing.T) {
		s := &subprocStream{
			events: make(chan provider.StreamEvent, 16),
			done:   make(chan struct{}),
		}
		s.handleAssistantMessage(call1)
		s.handleAssistantMessage(call2)
		assertFinal(t, s.Final())
	})

	t.Run("runnerTurn", func(t *testing.T) {
		tn := &runnerTurn{
			events: make(chan provider.StreamEvent, 16),
			done:   make(chan struct{}),
		}
		tn.handleAssistantMessage(call1)
		tn.handleAssistantMessage(call2)
		assertFinal(t, tn.Final())
	})
}

// TestLatestUserMessage covers the "feed only the newest user release"
// rule. Callers still pass a full req.Messages slice as the
// conversation; the CLI driver feeds everything except the final
// user-role entry through --resume instead of stdin.
func TestLatestUserMessage(t *testing.T) {
	cases := []struct {
		name string
		msgs []provider.Message
		want string // text of the returned message's first text block, or "" for nil
	}{
		{
			name: "empty slice",
			msgs: nil,
			want: "",
		},
		{
			name: "single user message",
			msgs: []provider.Message{
				{Role: provider.RoleUser, Content: []provider.ContentBlock{{Type: provider.ContentText, Text: "hello"}}},
			},
			want: "hello",
		},
		{
			name: "multiple turns — last user wins",
			msgs: []provider.Message{
				{Role: provider.RoleUser, Content: []provider.ContentBlock{{Type: provider.ContentText, Text: "first ask"}}},
				{Role: provider.RoleAssistant, Content: []provider.ContentBlock{{Type: provider.ContentText, Text: "first reply"}}},
				{Role: provider.RoleUser, Content: []provider.ContentBlock{{Type: provider.ContentText, Text: "second ask"}}},
			},
			want: "second ask",
		},
		{
			name: "ends on assistant — searches backward to latest user",
			msgs: []provider.Message{
				{Role: provider.RoleUser, Content: []provider.ContentBlock{{Type: provider.ContentText, Text: "newest user"}}},
				{Role: provider.RoleAssistant, Content: []provider.ContentBlock{{Type: provider.ContentText, Text: "trailing reply"}}},
			},
			want: "newest user",
		},
		{
			name: "all assistant — no user message",
			msgs: []provider.Message{
				{Role: provider.RoleAssistant, Content: []provider.ContentBlock{{Type: provider.ContentText, Text: "x"}}},
			},
			want: "",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := latestUserMessage(tc.msgs)
			if tc.want == "" {
				if got != nil {
					t.Errorf("want nil, got %+v", got)
				}
				return
			}
			if got == nil {
				t.Fatalf("got nil, want a message with text %q", tc.want)
			}
			if len(got.Content) == 0 || got.Content[0].Text != tc.want {
				t.Errorf("got content %+v, want first text %q", got.Content, tc.want)
			}
		})
	}
}

// memSessionStore is a tiny in-memory SessionStore used to verify the
// client's interactions (read/write/clear) without needing an FSStore.
type memSessionStore struct {
	mu       sync.Mutex
	sessions map[string]string
}

func (m *memSessionStore) ReadClaudeSessionID(slug string) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.sessions[slug], nil
}

func (m *memSessionStore) WriteClaudeSessionID(slug, id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.sessions == nil {
		m.sessions = make(map[string]string)
	}
	m.sessions[slug] = id
	return nil
}

func (m *memSessionStore) ClearClaudeSessionID(slug string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.sessions, slug)
	return nil
}

// TestResolveResumeSessionIDClearsStale locks in the probe behavior:
// when the store has a session_id but no matching CLI log exists on
// disk, resolveResumeSessionID clears the stale id and returns empty
// so the next Stream call starts a fresh session.
func TestResolveResumeSessionIDClearsStale(t *testing.T) {
	store := &memSessionStore{}
	_ = store.WriteClaudeSessionID("cos", "stale-session-abc")

	// HomeDir points at a real temp dir with NO matching log file —
	// the probe must fail and clear the stored id.
	home := t.TempDir()
	c := &Driver{opts: Options{Sessions: store, HomeDir: home}}

	got := c.resolveResumeSessionID("cos")
	if got != "" {
		t.Errorf("resolve = %q, want empty (stale id should be cleared)", got)
	}
	stored, _ := store.ReadClaudeSessionID("cos")
	if stored != "" {
		t.Errorf("stored = %q, want empty (should have been cleared)", stored)
	}
}

// TestResolveResumeSessionIDReturnsFresh verifies that when the CLI
// log exists on disk, resolveResumeSessionID returns the stored id
// intact. Uses the CLI's actual `$HOME/.claude/projects/-/<id>.jsonl`
// shape.
func TestResolveResumeSessionIDReturnsFresh(t *testing.T) {
	store := &memSessionStore{}
	_ = store.WriteClaudeSessionID("cos", "fresh-session-xyz")

	home := t.TempDir()
	projDir := filepath.Join(home, ".claude", "projects", "-")
	if err := os.MkdirAll(projDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	logPath := filepath.Join(projDir, "fresh-session-xyz.jsonl")
	if err := os.WriteFile(logPath, []byte("{}\n"), 0o644); err != nil {
		t.Fatalf("write log: %v", err)
	}

	c := &Driver{opts: Options{Sessions: store, HomeDir: home}}
	got := c.resolveResumeSessionID("cos")
	if got != "fresh-session-xyz" {
		t.Errorf("resolve = %q, want fresh-session-xyz", got)
	}
	stored, _ := store.ReadClaudeSessionID("cos")
	if stored != "fresh-session-xyz" {
		t.Errorf("stored was unexpectedly modified: got %q", stored)
	}
}

// TestResolveResumeSessionIDNoStore returns empty when Sessions is
// not configured (e.g., tests that build a bare client).
func TestResolveResumeSessionIDNoStore(t *testing.T) {
	c := &Driver{opts: Options{}}
	if got := c.resolveResumeSessionID("cos"); got != "" {
		t.Errorf("no-store resolve = %q, want empty", got)
	}
}

// TestWriteMCPConfigDataMode locks in the in-process Kivali web shape:
// no CoreUDS → spawn args carry --data <dir>, no --core-uds.
func TestWriteMCPConfigDataMode(t *testing.T) {
	dest := filepath.Join(t.TempDir(), "mcp-config.json")
	if err := writeMCPConfig(dest, "/usr/bin/kivali", "/data", "", "alice"); err != nil {
		t.Fatalf("writeMCPConfig: %v", err)
	}
	got := readMCPArgs(t, dest)
	want := []string{"mcp", "--agent", "alice", "--data", "/data"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("args = %v, want %v", got, want)
	}
}

// TestWriteMCPConfigUDSMode locks in the agent-pod shape: CoreUDS
// non-empty → spawn args carry --core-uds <path>, no --data flag.
// Without this, the spawned `kivali mcp` falls back to FSStore mode
// and immediately fails because /data isn't mounted in the agent pod.
func TestWriteMCPConfigUDSMode(t *testing.T) {
	dest := filepath.Join(t.TempDir(), "mcp-config.json")
	if err := writeMCPConfig(dest, "/usr/bin/kivali", "/data", "/var/run/kivali/uds/core.sock", "alice"); err != nil {
		t.Fatalf("writeMCPConfig: %v", err)
	}
	got := readMCPArgs(t, dest)
	want := []string{"mcp", "--agent", "alice", "--core-uds", "/var/run/kivali/uds/core.sock"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("args = %v, want %v", got, want)
	}
}

func readMCPArgs(t *testing.T, path string) []string {
	t.Helper()
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read config: %v", err)
	}
	var cfg mcpServersConfig
	if err := json.Unmarshal(body, &cfg); err != nil {
		t.Fatalf("decode config: %v", err)
	}
	entry, ok := cfg.McpServers["kivali"]
	if !ok {
		t.Fatalf("kivali entry missing: %s", body)
	}
	return entry.Args
}
