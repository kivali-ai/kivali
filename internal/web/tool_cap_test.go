package web

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/kivali-ai/kivali/internal/store"
	"github.com/kivali-ai/kivali/internal/web/apitypes"
)

func TestCapToolOutput(t *testing.T) {
	short := strings.Repeat("a", toolTextCap)
	if got, cut := capToolOutput(short); got != short || cut {
		t.Errorf("an output at the cap changed: cut=%v", cut)
	}
	// A multi-byte rune straddling the cap is dropped whole.
	long := strings.Repeat("a", toolTextCap-1) + "é" + strings.Repeat("b", 50)
	got, cut := capToolOutput(long)
	if !cut || got != strings.Repeat("a", toolTextCap-1)+toolTextEllipsis || !utf8.ValidString(got) {
		t.Errorf("capToolOutput = %q (cut %v)", got[len(got)-10:], cut)
	}
}

func TestCapToolInput(t *testing.T) {
	big := strings.Repeat("x", 3*toolTextCap)
	in, _ := json.Marshal(map[string]any{"path": "notes/plan.md", "content": big, "n": 3})
	// The model's key order, which json.Marshal of a map would not keep.
	raw := `{"path":"notes/plan.md","content":"` + big + `","n":3,"tags":["a","b"]}`
	got, cut := capToolInput(raw)
	if !cut || !json.Valid([]byte(got)) {
		t.Fatalf("capped input is not valid JSON (cut %v): %.80q", cut, got)
	}
	if !strings.HasPrefix(got, `{"path":"notes/plan.md","content":"xxx`) || !strings.HasSuffix(got, `…","n":3,"tags":["a","b"]}`) {
		t.Errorf("keys or short values moved: %.60q … %q", got, got[len(got)-30:])
	}
	if len(got) > len(in)/2 {
		t.Errorf("capped input is %d bytes of %d", len(got), len(in))
	}

	// A cut never lands inside an escape or a rune: the result decodes,
	// and the string keeps only whole characters.
	for _, unit := range []string{`\n`, `\"`, `é`, "é", `\\`} {
		lit := strings.Repeat(unit, toolTextCap) // well past the cap in bytes
		got, cut := capToolInput(`{"s":"` + lit + `"}`)
		var v struct{ S string }
		if err := json.Unmarshal([]byte(got), &v); err != nil || !cut {
			t.Errorf("unit %q: %v (cut %v)", unit, err, cut)
			continue
		}
		if !strings.HasSuffix(v.S, toolTextEllipsis) || !utf8.ValidString(v.S) {
			t.Errorf("unit %q: decoded tail %q", unit, v.S[len(v.S)-8:])
		}
	}

	// Input that is not JSON is cut as text; short input is untouched.
	if got, cut := capToolInput(big); !cut || len(got) != toolTextCap+len(toolTextEllipsis) {
		t.Errorf("non-JSON input: %d bytes, cut %v", len(got), cut)
	}
	if got, cut := capToolInput(`{"a":1}`); got != `{"a":1}` || cut {
		t.Errorf("short input changed")
	}
}

// The current chat caps tool calls and the tool-calls endpoint answers
// them in full; past chats are not capped.
func TestAPIChatCapsToolCalls(t *testing.T) {
	srv := transcriptServer(t)
	bigIn := `{"path":"a.md","content":"` + strings.Repeat("y", 5000) + `"}`
	bigOut := strings.Repeat("z", 5000)
	for _, m := range []store.ChatMessage{
		{Role: store.RoleSent, Kind: "tool_use", ToolUseID: "big", ToolName: "file_create", ToolInput: bigIn, TS: tsAt(1)},
		{Role: store.RoleReceived, Kind: "tool_result", ToolUseID: "big", Content: bigOut, TS: tsAt(2)},
		{Role: store.RoleSent, Kind: "tool_use", ToolUseID: "small", ToolName: "Bash", ToolInput: `{"cmd":"ls"}`, TS: tsAt(3)},
		{Role: store.RoleReceived, Kind: "tool_result", ToolUseID: "small", Content: "ok", TS: tsAt(4)},
		{Role: store.RoleReceived, Kind: "tool_result", ToolUseID: "orphan", Content: bigOut, TS: tsAt(5)},
	} {
		if err := srv.Store.AppendChatMessage("alice", m); err != nil {
			t.Fatal(err)
		}
	}
	c := decodeAPI[apitypes.Chat](t, chatDo(t, srv, http.MethodGet, "/api/v1/agents/alice/chat", ""))
	if len(c.Rows) != 3 {
		t.Fatalf("rows = %+v", c.Rows)
	}
	big, small, orphan := c.Rows[0], c.Rows[1], c.Rows[2]
	if !big.InputTruncated || !big.OutputTruncated || len(*big.Input) >= len(bigIn) || len(*big.Output) >= len(bigOut) {
		t.Errorf("big call not capped: in %d/%v out %d/%v", len(*big.Input), big.InputTruncated, len(*big.Output), big.OutputTruncated)
	}
	if !strings.Contains(*big.Input, `"path":"a.md"`) {
		t.Errorf("capped input lost its path: %.40q", *big.Input)
	}
	if small.InputTruncated || small.OutputTruncated || *small.Input != `{"cmd":"ls"}` || *small.Output != "ok" {
		t.Errorf("small call changed: %+v", small)
	}
	if !orphan.OutputTruncated {
		t.Errorf("orphan result not capped")
	}

	full := decodeAPI[apitypes.ToolCallDetail](t, chatDo(t, srv, http.MethodGet, "/api/v1/agents/alice/tool-calls/big", ""))
	if full.ToolUseID != "big" || full.Input != bigIn || full.Output != bigOut {
		t.Errorf("full call = %d/%d bytes", len(full.Input), len(full.Output))
	}
	if o := decodeAPI[apitypes.ToolCallDetail](t, chatDo(t, srv, http.MethodGet, "/api/v1/agents/alice/tool-calls/orphan", "")); o.Output != bigOut || o.Input != "" {
		t.Errorf("orphan call = %+v", o)
	}
	assertAPIError(t, chatDo(t, srv, http.MethodGet, "/api/v1/agents/alice/tool-calls/gone", ""), http.StatusNotFound)
	assertAPIError(t, chatDo(t, srv, http.MethodGet, "/api/v1/agents/nobody/tool-calls/big", ""), http.StatusNotFound)

	// Once rotated, the chat is past: served whole, and the call has left
	// the current chat.
	const gen = "20260901T090000.000000000Z"
	if err := srv.Store.ArchiveChatAs("alice", gen); err != nil {
		t.Fatal(err)
	}
	past := decodeAPI[apitypes.PastChatDetail](t, chatDo(t, srv, http.MethodGet, "/api/v1/agents/alice/chats/"+gen, ""))
	if len(past.Rows) == 0 || *past.Rows[0].Input != bigIn || past.Rows[0].InputTruncated {
		t.Errorf("past chat row was capped")
	}
	assertAPIError(t, chatDo(t, srv, http.MethodGet, "/api/v1/agents/alice/tool-calls/big", ""), http.StatusNotFound)
}
