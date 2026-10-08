package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/kivali-ai/kivali/internal/store"
)

// SearchPastChatsToolName is the registered MCP name. Kept exported
// so main.go's --allowedTools wiring can reference it without
// duplicating the string.
const SearchPastChatsToolName = "search_past_chats"

// searchPastChatsMaxResults caps the total matches we return across
// all chats. Too many matches flood the tool_result with noise; if
// the agent needs more, they can narrow the query or paginate
// manually by re-querying with a tighter phrase. The cap is
// deliberately generous — 30 is enough for most "did we discuss X"
// questions while keeping the response readable.
const searchPastChatsMaxResults = 30

// searchPastChatsSnippetContext is the number of characters to
// surround each match with on either side. Enough to disambiguate
// which thread a match came from without drowning in noise.
const searchPastChatsSnippetContext = 80

// PastChatsReader is the read surface search_past_chats needs:
// list of archive timestamps + per-archive history. Mirrors the
// FSStore methods that back the in-process implementation.
//
// Two impls:
//
//   - storePastChatsReader (this file) — wraps an FSStore + slug,
//     used by the Kivali web in-process MCP fleet.
//   - The agent-pod MCP subprocess wraps an agentpod.Client; both
//     methods round-trip to core.
type PastChatsReader interface {
	ListArchivedChats(ctx context.Context) ([]store.ArchivedChat, error)
	ReadArchivedChat(ctx context.Context, ts string) ([]store.ChatMessage, error)
}

// SearchPastChatsTool returns a grep-style search over the agent's
// archived chat generations. The reader is bound to the calling
// agent's slug (FSStore wrapper) or to the same slug via the UDS
// path; we never cross agents.
//
// Why this exists: agents can already browse /files/past-chats/
// via file_view, but that's per-chat file reads. When the user
// says "we spoke about X a while back," the agent needs to find
// WHICH chat contains X without loading every archive into context.
// Simple substring grep is enough — fancy indexing is premature
// given the corpus size (dozens of chats, ~100 entries each).
func SearchPastChatsTool(r PastChatsReader) Tool {
	return Tool{
		Name:        SearchPastChatsToolName,
		Description: searchPastChatsDescription,
		InputSchema: json.RawMessage(`{
			"type":"object",
			"properties":{
				"query":{"type":"string","description":"literal substring to search for; case-insensitive"},
				"max_results":{"type":"integer","description":"cap on total matches returned across all chats (default 30)"}
			},
			"required":["query"]
		}`),
		ReadOnly: true,
		Handler:  searchPastChatsHandler(r),
	}
}

// NewStorePastChatsReader returns the in-process reader backed by
// an FSStore + slug. Used by the Kivali web in-process MCP fleet.
func NewStorePastChatsReader(s *store.FSStore, slug string) PastChatsReader {
	return storePastChatsReader{s: s, slug: slug}
}

type storePastChatsReader struct {
	s    *store.FSStore
	slug string
}

func (r storePastChatsReader) ListArchivedChats(_ context.Context) ([]store.ArchivedChat, error) {
	return r.s.ListArchivedChats(r.slug)
}

func (r storePastChatsReader) ReadArchivedChat(_ context.Context, ts string) ([]store.ChatMessage, error) {
	return r.s.ReadArchivedChat(r.slug, ts)
}

type searchPastChatsInput struct {
	Query      string `json:"query"`
	MaxResults int    `json:"max_results,omitempty"`
}

// pastChatMatch is one hit: a chat timestamp, the entry's role/kind,
// and the snippet centered on the match.
type pastChatMatch struct {
	ChatTS    string
	EntryRole string
	EntryKind string
	EntryIdx  int // 0-based position within the chat's entries
	Snippet   string
}

func searchPastChatsHandler(r PastChatsReader) func(ctx context.Context, raw json.RawMessage) (*ToolResult, error) {
	return func(ctx context.Context, raw json.RawMessage) (*ToolResult, error) {
		var in searchPastChatsInput
		if err := json.Unmarshal(raw, &in); err != nil {
			return &ToolResult{IsError: true, Content: []string{"invalid input: " + err.Error()}}, nil
		}
		query := strings.TrimSpace(in.Query)
		if query == "" {
			return &ToolResult{IsError: true, Content: []string{"query is required and cannot be empty"}}, nil
		}
		max := in.MaxResults
		if max <= 0 {
			max = searchPastChatsMaxResults
		}
		needle := strings.ToLower(query)

		chats, err := r.ListArchivedChats(ctx)
		if err != nil {
			return &ToolResult{IsError: true, Content: []string{"list past chats: " + err.Error()}}, nil
		}
		if len(chats) == 0 {
			return &ToolResult{Content: []string{fmt.Sprintf("No past chats to search. (%q: 0 matches in 0 chats)", query)}}, nil
		}

		// Newest-first — agents typically care about recent context
		// more than ancient context, and the cap is applied as we go.
		sort.Slice(chats, func(i, j int) bool { return chats[i].Timestamp > chats[j].Timestamp })

		var matches []pastChatMatch
		chatsSearched := 0
		chatsWithHits := 0
		for _, c := range chats {
			if len(matches) >= max {
				break
			}
			chatsSearched++
			hist, err := r.ReadArchivedChat(ctx, c.Timestamp)
			if err != nil {
				continue
			}
			hitsInChat := 0
			for i, m := range hist {
				if len(matches) >= max {
					break
				}
				// Case-insensitive substring match on Content. We
				// don't grep structured fields (tool_name, tool_input)
				// — those generate too many spurious hits for
				// natural-language queries. An agent looking for a
				// specific tool call can use file_view on the chat
				// file directly.
				content := m.Content
				if content == "" {
					continue
				}
				hay := strings.ToLower(content)
				idx := strings.Index(hay, needle)
				if idx < 0 {
					continue
				}
				matches = append(matches, pastChatMatch{
					ChatTS:    c.Timestamp,
					EntryRole: m.Role,
					EntryKind: m.Kind,
					EntryIdx:  i,
					Snippet:   snippetAround(content, idx, len(query), searchPastChatsSnippetContext),
				})
				hitsInChat++
			}
			if hitsInChat > 0 {
				chatsWithHits++
			}
		}

		return &ToolResult{Content: []string{renderPastChatSearchResults(query, matches, chatsSearched, chatsWithHits, len(chats))}}, nil
	}
}

// snippetAround returns a window of `context` characters on either
// side of a match at `matchIdx` of `matchLen`, with ellipses when
// the window doesn't reach the string boundary. Collapses embedded
// newlines into spaces so the snippet reads as one line in the
// model's view.
func snippetAround(content string, matchIdx, matchLen, context int) string {
	start := matchIdx - context
	if start < 0 {
		start = 0
	}
	end := matchIdx + matchLen + context
	if end > len(content) {
		end = len(content)
	}
	snippet := content[start:end]
	snippet = strings.ReplaceAll(snippet, "\n", " ")
	snippet = strings.ReplaceAll(snippet, "\r", " ")
	// Collapse runs of whitespace so UI + model don't see jagged blank space.
	for strings.Contains(snippet, "  ") {
		snippet = strings.ReplaceAll(snippet, "  ", " ")
	}
	snippet = strings.TrimSpace(snippet)
	if start > 0 {
		snippet = "…" + snippet
	}
	if end < len(content) {
		snippet = snippet + "…"
	}
	return snippet
}

// renderPastChatSearchResults formats the grep output as a human-
// readable tool_result body. Groups matches by chat; each match
// shows enough context for the agent to decide "do I need the full
// chat?" and tells them exactly how to load it if so.
func renderPastChatSearchResults(query string, matches []pastChatMatch, chatsSearched, chatsWithHits, totalChats int) string {
	if len(matches) == 0 {
		return fmt.Sprintf("No matches for %q in any of the %d past chat(s) searched.", query, chatsSearched)
	}
	// Group by chat TS, preserving the newest-first order we arrived in.
	order := []string{}
	byChat := map[string][]pastChatMatch{}
	for _, m := range matches {
		if _, seen := byChat[m.ChatTS]; !seen {
			order = append(order, m.ChatTS)
		}
		byChat[m.ChatTS] = append(byChat[m.ChatTS], m)
	}

	var b strings.Builder
	fmt.Fprintf(&b, "Found %d match(es) for %q across %d of %d past chat(s).\n",
		len(matches), query, chatsWithHits, totalChats)
	if chatsSearched < totalChats {
		fmt.Fprintf(&b, "(Stopped at the max-results cap; %d chat(s) remain unsearched — narrow the query or raise max_results to see more.)\n", totalChats-chatsSearched)
	}
	b.WriteString("\n")

	for _, ts := range order {
		fmt.Fprintf(&b, "=== /files/past-chats/%s/chat.jsonl ===\n", ts)
		for _, m := range byChat[ts] {
			fmt.Fprintf(&b, "[entry #%d, role=%s", m.EntryIdx, m.EntryRole)
			if m.EntryKind != "" {
				fmt.Fprintf(&b, " kind=%s", m.EntryKind)
			}
			b.WriteString("]\n")
			b.WriteString("  ")
			b.WriteString(m.Snippet)
			b.WriteString("\n")
		}
		b.WriteString("\n")
	}

	b.WriteString("To read the full chat, use file_view on the path shown above.")
	return b.String()
}

// Description kept verbatim aligned with internal/agent/state_tools.go.
const searchPastChatsDescription = `Grep-style search across your OWN archived past chats (case-insensitive substring on Content fields only — not tool_name or tool_input). Returns matching snippets grouped by chat with the path you can then open via file_view. Newest-first. See handbook §/files/past-chats/ for when to reach for this. Inputs: query (required, keep it specific — common words flood), max_results (optional, default 30).`
