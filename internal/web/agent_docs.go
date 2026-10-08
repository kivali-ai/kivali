package web

import (
	"errors"
	"strings"
	"time"

	"github.com/kivali-ai/kivali/internal/store"
)

// This file holds the agent documents (role.md, agent_memory.md,
// principles) and the archive of past chats that the API's About and
// Past chats views read (api_agent_work.go, api_chat.go).

// agentDocKind distinguishes the editable agent documents: how each is
// read and written.
type agentDocKind struct {
	// required refuses an empty body on save: role refuses one (an
	// agent with no identity is a bug), memory allows one (clearing it
	// is a legitimate edit).
	required bool
	// read, write and updatedAt are the store accessors for this
	// document. Writes go through saveAgentDoc, which owns validation.
	read      func(*store.FSStore, string) (string, error)
	write     func(*store.FSStore, string, string) error
	updatedAt func(*store.FSStore, string) (time.Time, error)
}

var (
	agentRoleDoc = agentDocKind{
		required:  true,
		read:      (*store.FSStore).ReadRole,
		write:     (*store.FSStore).WriteRole,
		updatedAt: (*store.FSStore).RoleUpdatedAt,
	}
	agentMemoryDoc = agentDocKind{
		read: (*store.FSStore).ReadAgentMemory,
		// An empty body clears the memory.
		write:     (*store.FSStore).WriteAgentMemory,
		updatedAt: (*store.FSStore).AgentMemoryUpdatedAt,
	}
	agentHabitsDoc = agentDocKind{
		read: (*store.FSStore).ReadAgentHabits,
		// An empty body clears the file: revoking every habit is a
		// legitimate edit. The operator's edit path is ungated — the
		// rotation-only rule is on the agent's own writes.
		write:     (*store.FSStore).WriteAgentHabits,
		updatedAt: (*store.FSStore).AgentHabitsUpdatedAt,
	}
)

// errAgentDocRequired is saveAgentDoc's refusal of an empty body for a
// document that must not be empty (the role: an agent with no identity
// is a bug, and a blank form submit must not silently erase one).
var errAgentDocRequired = errors.New("body required")

// saveAgentDoc replaces one of an agent's documents with body. It is
// the one write path for the operator's edits, for
// PUT /api/v1/agents/{slug}/docs/{kind}. The caller has
// already refused the CEO slug. Returns errAgentDocRequired for an
// empty required document and store.ErrNotFound for an unknown agent.
func (s *Server) saveAgentDoc(kind agentDocKind, slug, body string) error {
	if kind.required && strings.TrimSpace(body) == "" {
		return errAgentDocRequired
	}
	return kind.write(s.Store, slug, body)
}

// ---- past chats ----

// archivedChatView is one row in the archive: the rotation timestamp
// (which is also its URL), the instant it parses to, and the opening
// line of the transcript.
type archivedChatView struct {
	Timestamp string
	At        time.Time
	Excerpt   string
}

// archiveTSLayout is the format store.ArchiveChat stamps directory
// names with. Parsing it back is what lets the UI render an archive
// row as a date instead of the raw directory name.
const archiveTSLayout = "20060102T150405.000000000Z"

func parseArchiveTS(ts string) (time.Time, bool) {
	t, err := time.Parse(archiveTSLayout, ts)
	if err != nil {
		return time.Time{}, false
	}
	return t.UTC(), true
}

// excerptMaxRunes caps an archive row's opening line. Long enough to
// recognise a chat, short enough that every row stays one line.
const excerptMaxRunes = 140

// trimExcerpt flattens a chat message to a single line of at most
// excerptMaxRunes. Newlines and runs of whitespace collapse to single
// spaces so a multi-paragraph opening message doesn't blow the row's
// height apart.
func trimExcerpt(s string) string {
	flat := strings.Join(strings.Fields(s), " ")
	r := []rune(flat)
	if len(r) <= excerptMaxRunes {
		return flat
	}
	return strings.TrimRight(string(r[:excerptMaxRunes]), " ") + "…"
}

// archivedChatViews turns a slice of archive entries into rows. A
// generation the episode writer has digested is labelled by its
// episode title — written to tell this chat apart from every similar
// one — and a generation still waiting on its digest by the
// transcript's opening line. Label read failures are dropped rather
// than surfaced: a row that can't be labelled is still a row worth
// linking to.
func (s *Server) archivedChatViews(slug string, chats []store.ArchivedChat) []archivedChatView {
	out := make([]archivedChatView, 0, len(chats))
	for _, c := range chats {
		v := archivedChatView{Timestamp: c.Timestamp}
		if at, ok := parseArchiveTS(c.Timestamp); ok {
			v.At = at
		}
		if title, ok := s.Store.EpisodeTitle(slug, c.Timestamp); ok {
			v.Excerpt = trimExcerpt(title)
		} else if ex, err := s.Store.ArchivedChatExcerpt(slug, c.Timestamp); err == nil {
			v.Excerpt = trimExcerpt(ex)
		}
		out = append(out, v)
	}
	return out
}
