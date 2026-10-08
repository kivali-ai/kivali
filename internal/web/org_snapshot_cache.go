package web

import (
	"sort"
	"sync"

	"github.com/kivali-ai/kivali/internal/agent"
	"github.com/kivali-ai/kivali/internal/store"
)

// snapshotCache keeps what the org snapshot derives from files that
// rarely change between two snapshots, so a rebuild reads only what
// moved: an agent's chat history (its fill and its gate-derived
// flags), and the CEO's inbox (what still needs the CEO). Each entry
// is keyed on the stamps of the files it was derived from, taken
// before the read: a write landing during the read leaves the entry
// keyed one version behind, and the next rebuild reads again.
//
// The zero value is ready; tests that build a Server literal get an
// empty cache.
type snapshotCache struct {
	mu     sync.Mutex
	agents map[string]agentChatFacts
	ceo    *ceoNeedsEntry
}

// agentChatFacts is one agent's snapshot facts from its chat history.
type agentChatFacts struct {
	chat, window store.FileStamp
	// model and defaultModel resolve the context window the fill is
	// measured against.
	model, defaultModel string

	verdict     store.SpawnVerdict
	disruptions int
	fillPct     int
}

// ceoNeedsEntry is the CEO inbox's open items: their count and the
// request paths, as the snapshot carries them.
type ceoNeedsEntry struct {
	chat               store.FileStamp
	assignmentsVersion int64
	// messages is the message count: a message file moved away
	// (redacted) drops its card without touching the CEO's chat.
	messages int

	count int
	paths []string
}

// chatFactsFor is a's snapshot facts, read from its chat history only
// when that (or its context-window sidecar, or the model) changed since
// the last snapshot.
func (s *Server) chatFactsFor(a store.Agent) agentChatFacts {
	chat := s.Store.ChatHistoryStamp(a.Slug)
	window := s.Store.ContextWindowStamp(a.Slug)
	c := &s.snapshotCache
	c.mu.Lock()
	prev, ok := c.agents[a.Slug]
	c.mu.Unlock()
	if ok && prev.model == a.Model && prev.defaultModel == s.AgentModel && prev.chat.Matches(chat) && prev.window.Matches(window) {
		return prev
	}

	hist, _ := s.Store.ReadChatHistory(a.Slug)
	_, pct := s.contextFillForHistory(a, hist)
	facts := agentChatFacts{
		chat: chat, window: window, model: a.Model, defaultModel: s.AgentModel,
		verdict:     store.SpawnDecision(hist),
		disruptions: store.ConsecutiveRuntimeDisruptions(hist),
		fillPct:     pct,
	}
	c.mu.Lock()
	if c.agents == nil {
		c.agents = map[string]agentChatFacts{}
	}
	c.agents[a.Slug] = facts
	c.mu.Unlock()
	return facts
}

// ceoNeeds is how many CEO inbox items still need the CEO and their
// request paths, sorted. The inbox is rebuilt only when the CEO's chat,
// the tracker or the message count moved since the last call.
func (s *Server) ceoNeeds() (count int, paths []string) {
	chat := s.Store.ChatHistoryStamp(agent.CEOSlug)
	version := s.assignmentsVersion.Load()
	messages, err := s.Store.CountMessages()
	if err != nil {
		messages = -1 // never matches: an unreadable tree is not cached
	}
	c := &s.snapshotCache
	c.mu.Lock()
	prev := c.ceo
	c.mu.Unlock()
	if prev != nil && messages >= 0 && prev.chat.Matches(chat) && prev.assignmentsVersion == version && prev.messages == messages {
		return prev.count, prev.paths
	}

	view, err := s.buildCEOInbox(1)
	if err != nil {
		return 0, []string{}
	}
	paths = make([]string, 0, len(view.Needs))
	for _, it := range view.Needs {
		paths = append(paths, it.RequestPath)
	}
	sort.Strings(paths)
	entry := &ceoNeedsEntry{chat: chat, assignmentsVersion: version, messages: messages, count: view.NeedsCount(), paths: paths}
	c.mu.Lock()
	c.ceo = entry
	c.mu.Unlock()
	return entry.count, entry.paths
}

// forgetSnapshotCache drops every cached snapshot fact: a restore puts
// back files whose stamps an entry could mistake for its own.
func (s *Server) forgetSnapshotCache() {
	c := &s.snapshotCache
	c.mu.Lock()
	c.agents = nil
	c.ceo = nil
	c.mu.Unlock()
	s.Store.ForgetReadCaches()
}
