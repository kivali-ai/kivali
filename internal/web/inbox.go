package web

import (
	"fmt"
	"path/filepath"
	"sort"
	"time"

	"github.com/kivali-ai/kivali/internal/agent"
	"github.com/kivali-ai/kivali/internal/store"
)

// historyThread groups a root message with its replies for the
// Message history section. Root is the first message in a conversation
// (a notice, an assignment event, or a ceo_approval_request); Replies are
// every subsequent message in the thread, sorted by date ascending.
// LastActivity is the newest Date across root+replies, which is what
// the history section sorts threads by.
type historyThread struct {
	Root         store.Message
	RootPath     string
	RootView     agent.InboxView
	Replies      []historyReply
	LastActivity time.Time
}

type historyReply struct {
	Message store.Message
	Path    string
	View    agent.InboxView
}

// inboxDefaultHistoryThreads caps the number of threads rendered on
// first paint. Clicking "load more" steps up by this same amount via
// ?history=<N>.
const inboxDefaultHistoryThreads = 20

// buildPendingList materializes the For-agents inbox cards from a
// release-state snapshot. One card per MESSAGE, not per queued pointer:
// a notice addressed to three agents holds three pointers, but the CEO
// makes one decision about it, so it renders once with all three
// recipients on the routing line.
//
// Sorted newest-first by the message's own Date — matches Needs you
// and the history, so Home reads as "most-recent activity at the top."
// Path is the deterministic tiebreaker for messages with identical
// timestamps, which keeps the order stable between reads.
func (s *Server) buildPendingList(ts store.MessageQueue) []pendingDelivery {
	// Recipients accumulate in sorted-slug order rather than map order
	// so the routing line doesn't reshuffle between reads.
	slugs := make([]string, 0, len(ts.Agents))
	for slug := range ts.Agents {
		slugs = append(slugs, slug)
	}
	sort.Strings(slugs)

	// Each card carries its own auto-release deadline, fixed by the
	// slider setting that held when the message was sent; the pill
	// counts to it and a later slider move leaves it alone.
	sched := s.Store.ReadAutoReleaseSchedule()
	index := map[string]int{}
	var pending []pendingDelivery
	for _, slug := range slugs {
		for _, p := range ts.Agents[slug].Inbox {
			if i, ok := index[p]; ok {
				pending[i].Recipients = append(pending[i].Recipients, slug)
				continue
			}
			d, err := s.Store.ReadMessage(filepath.Join(s.Store.Root(), p))
			if err != nil {
				continue
			}
			index[p] = len(pending)
			due, _ := autoReleaseDue(sched, d)
			pending = append(pending, pendingDelivery{
				Path:       p,
				Recipients: []string{slug},
				Message:    d,
				View:       agent.BuildInboxView(d, p),
				Due:        due,
			})
		}
	}
	sort.SliceStable(pending, func(i, j int) bool {
		if !pending[i].Message.Date.Equal(pending[j].Message.Date) {
			return pending[i].Message.Date.After(pending[j].Message.Date)
		}
		return pending[i].Path < pending[j].Path
	})
	return pending
}

// buildHistoryThreads groups every message on disk by its thread root
// (walking InReplyTo chains) and returns the newest-first slice,
// trimmed to `limit`. `excludePaths` names paths that are currently
// pending (un-actioned CEO Needs, release-state inboxes) and must not
// appear in the history — they live in the pending buckets above.
//
// totalThreads is the full count before trimming, so the UI can show
// "showing N of M" and surface a load-more affordance.
func (s *Server) buildHistoryThreads(messages []store.Message, excludePaths map[string]bool, limit int) (threads []historyThread, totalThreads int) {
	root := s.Store.Root()
	rel := func(p string) string {
		if r, err := filepath.Rel(root, p); err == nil {
			return r
		}
		return p
	}

	// Index every message by its relpath so InReplyTo walks can find
	// parents in O(1).
	byPath := make(map[string]store.Message, len(messages))
	for _, m := range messages {
		byPath[rel(m.Path)] = m
	}

	// rootOf walks a message's InReplyTo chain up to the first
	// message without a parent (or the first broken link) and
	// returns that root + its relpath.
	rootOf := func(m store.Message) (store.Message, string) {
		cur := m
		visited := map[string]bool{rel(m.Path): true}
		for cur.InReplyTo != "" {
			parent, ok := byPath[cur.InReplyTo]
			if !ok {
				break
			}
			if visited[cur.InReplyTo] {
				// Cycle defense: shouldn't happen with our write
				// semantics, but breaks cleanly if one slips in.
				break
			}
			visited[cur.InReplyTo] = true
			cur = parent
		}
		return cur, rel(cur.Path)
	}

	grouped := map[string]*historyThread{}
	for _, m := range messages {
		rootMsg, rootPath := rootOf(m)
		if excludePaths[rootPath] {
			continue
		}
		t, ok := grouped[rootPath]
		if !ok {
			t = &historyThread{
				Root:         rootMsg,
				RootPath:     rootPath,
				RootView:     agent.BuildInboxView(rootMsg, rootPath),
				LastActivity: rootMsg.Date,
			}
			grouped[rootPath] = t
		}
		relM := rel(m.Path)
		if relM == rootPath {
			continue
		}
		t.Replies = append(t.Replies, historyReply{
			Message: m,
			Path:    relM,
			View:    agent.BuildInboxView(m, relM),
		})
		if m.Date.After(t.LastActivity) {
			t.LastActivity = m.Date
		}
	}
	// Within a thread, replies sort oldest→newest.
	for _, t := range grouped {
		sort.SliceStable(t.Replies, func(i, j int) bool {
			return t.Replies[i].Message.Date.Before(t.Replies[j].Message.Date)
		})
	}
	// Threads sort newest-activity first.
	ordered := make([]*historyThread, 0, len(grouped))
	for _, t := range grouped {
		ordered = append(ordered, t)
	}
	// Equal activity times break on the root path, so the order (and
	// the history API's cursor) does not depend on map iteration.
	sort.SliceStable(ordered, func(i, j int) bool {
		if !ordered[i].LastActivity.Equal(ordered[j].LastActivity) {
			return ordered[i].LastActivity.After(ordered[j].LastActivity)
		}
		return ordered[i].RootPath < ordered[j].RootPath
	})
	totalThreads = len(ordered)
	if limit > 0 && len(ordered) > limit {
		ordered = ordered[:limit]
	}
	threads = make([]historyThread, len(ordered))
	for i, t := range ordered {
		threads[i] = *t
	}
	return threads, totalThreads
}

// atoiPositive parses a positive integer, returning an error on
// negative values or non-numeric input.
func atoiPositive(s string) (int, error) {
	n := 0
	if s == "" {
		return 0, fmt.Errorf("empty")
	}
	for _, c := range s {
		if c < '0' || c > '9' {
			return 0, fmt.Errorf("not a number")
		}
		n = n*10 + int(c-'0')
	}
	if n == 0 {
		return 0, fmt.Errorf("zero")
	}
	return n, nil
}
