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
