package web

import (
	"errors"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/kivali-ai/kivali/internal/agent"
	"github.com/kivali-ai/kivali/internal/store"
)

// Home's Needs you and its message history refresh whenever a message
// moves anywhere in the org, so what they read is kept between
// refreshes and only what changed is parsed. Both rest on how messages
// are stored: a message file is written once (WriteMessage, atomically,
// under a name that carries its timestamp) and only ever moved away
// (MoveToRedacted), never rewritten.

// ceoInboxCache follows the CEO's chat.jsonl, which New chat never
// rotates, so it only grows. It keeps the chat's ceo_inbox and ceo_reply
// rows and the messages they point to, and nothing else.
type ceoInboxCache struct {
	mu   sync.Mutex
	tail *store.ChatTail
	rows []store.ChatMessage
	msgs map[string]stampedMessage
}

type stampedMessage struct {
	stamp store.FileStamp
	msg   store.Message
}

// isCEOInboxRow reports whether a CEO chat row is one buildCEOInbox
// reads: a request, notification or assignment event to the CEO, or the
// CEO's reply, each pointing at its message file.
func isCEOInboxRow(m store.ChatMessage) bool {
	return (m.Kind == "ceo_inbox" || m.Kind == "ceo_reply") && m.MessageRef != ""
}

// ceoInboxRead returns the CEO chat's inbox rows in order, and the
// messages they point to by their store-relative path. A message that
// cannot be read is left out, as buildCEOInbox skips its row. Each
// message is checked against its file stamp, so one moved away since
// the last read drops out, and only a new or changed one is parsed.
func (s *Server) ceoInboxRead() ([]store.ChatMessage, map[string]store.Message, error) {
	c := &s.ceoInboxCache
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.tail == nil {
		c.tail = s.Store.ChatHistoryTail(agent.CEOSlug)
	}
	r, err := c.tail.Read()
	if r.Reset {
		c.rows = nil
	}
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		return nil, nil, err
	}
	for _, m := range r.Rows {
		if isCEOInboxRow(m) {
			c.rows = append(c.rows, m)
		}
	}
	// A capped slice: appending the unterminated final row copies, so
	// it never lands in the kept rows.
	rows := c.rows[:len(c.rows):len(c.rows)]
	if r.Tail != nil && isCEOInboxRow(*r.Tail) {
		rows = append(rows, *r.Tail)
	}

	kept := make(map[string]stampedMessage, len(rows))
	msgs := make(map[string]store.Message, len(rows))
	for _, e := range rows {
		if _, ok := msgs[e.MessageRef]; ok {
			continue
		}
		abs := filepath.Join(s.Store.Root(), e.MessageRef)
		stamp := s.Store.MessageStamp(abs)
		sm, ok := c.msgs[e.MessageRef]
		if !ok || !sm.stamp.Matches(stamp) {
			m, err := s.Store.ReadMessage(abs)
			if err != nil {
				continue
			}
			sm = stampedMessage{stamp: stamp, msg: m}
		}
		kept[e.MessageRef] = sm
		msgs[e.MessageRef] = sm.msg
	}
	c.msgs = kept
	return rows, msgs, nil
}

// forgetHomeCaches drops what Home's inbox and history keep, for a
// restore: it writes the CEO's chat over in place, and the files it puts
// back are not the ones the kept rows, heads and messages were read
// from.
func (s *Server) forgetHomeCaches() {
	c := &s.ceoInboxCache
	c.mu.Lock()
	c.tail, c.rows, c.msgs = nil, nil, nil
	c.mu.Unlock()
	h := &s.historyCache
	h.mu.Lock()
	h.heads = nil
	h.mu.Unlock()
}

// historyCache keeps, for every message on disk, the little the
// history's thread order needs: what it replies to and when it was
// sent. A message's head is read once; a message moved away drops out
// when the next listing no longer has it.
type historyCache struct {
	mu    sync.Mutex
	heads map[string]messageHead
}

// messageHead is a message as the history's grouping and order see it.
type messageHead struct {
	// Path is relative to the store root, the form InReplyTo uses.
	Path      string
	InReplyTo string
	Date      time.Time
}

// messageHeads returns the head of every message ListMessages would
// read, in its order (by path), parsing only the messages not seen
// before. A message that cannot be read fails the call, as it fails
// ListMessages.
func (s *Server) messageHeads() ([]messageHead, error) {
	paths, err := s.Store.ListMessagePaths()
	if err != nil {
		return nil, err
	}
	c := &s.historyCache
	c.mu.Lock()
	defer c.mu.Unlock()
	next := make(map[string]messageHead, len(paths))
	out := make([]messageHead, 0, len(paths))
	for _, p := range paths {
		rel := s.storeRel(p)
		h, ok := c.heads[rel]
		if !ok {
			m, err := s.Store.ReadMessage(p)
			if err != nil {
				return nil, err
			}
			h = messageHead{Path: rel, InReplyTo: m.InReplyTo, Date: m.Date}
		}
		next[rel] = h
		out = append(out, h)
	}
	c.heads = next
	return out, nil
}

// storeRel is p relative to the store root, or p when it is not under
// it.
func (s *Server) storeRel(p string) string {
	if r, err := filepath.Rel(s.Store.Root(), p); err == nil {
		return r
	}
	return p
}

// historyThreadRef is a history thread as its order needs it: its
// root's path, its replies' paths oldest first, and its last activity.
// loadHistoryThread reads its messages for the page that shows it.
type historyThreadRef struct {
	RootPath     string
	ReplyPaths   []string
	LastActivity time.Time
}

// groupHistoryThreads groups messages by their thread root, walking
// InReplyTo chains, and returns the threads newest activity first.
// heads are in path order. A thread whose root is in excludePaths is
// left out: it is still pending (a CEO Need, a queued delivery) and
// shows in its own bucket. LastActivity is the newest date across the
// root and its replies; equal activity breaks on the root path, so the
// order (and the history API's cursor) does not depend on map order.
func groupHistoryThreads(heads []messageHead, excludePaths map[string]bool) []historyThreadRef {
	byPath := make(map[string]messageHead, len(heads))
	for _, h := range heads {
		byPath[h.Path] = h
	}
	// rootOf walks a message's InReplyTo chain up to the first message
	// without a parent, or the first broken link.
	rootOf := func(h messageHead) messageHead {
		cur := h
		visited := map[string]bool{h.Path: true}
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
		return cur
	}

	type group struct {
		ref     historyThreadRef
		replies []messageHead
	}
	grouped := map[string]*group{}
	for _, h := range heads {
		root := rootOf(h)
		if excludePaths[root.Path] {
			continue
		}
		g, ok := grouped[root.Path]
		if !ok {
			g = &group{ref: historyThreadRef{RootPath: root.Path, LastActivity: root.Date}}
			grouped[root.Path] = g
		}
		if h.Path == root.Path {
			continue
		}
		g.replies = append(g.replies, h)
		if h.Date.After(g.ref.LastActivity) {
			g.ref.LastActivity = h.Date
		}
	}
	out := make([]historyThreadRef, 0, len(grouped))
	for _, g := range grouped {
		// Within a thread, replies sort oldest to newest.
		sort.SliceStable(g.replies, func(i, j int) bool { return g.replies[i].Date.Before(g.replies[j].Date) })
		for _, r := range g.replies {
			g.ref.ReplyPaths = append(g.ref.ReplyPaths, r.Path)
		}
		out = append(out, g.ref)
	}
	sort.SliceStable(out, func(i, j int) bool {
		if !out[i].LastActivity.Equal(out[j].LastActivity) {
			return out[i].LastActivity.After(out[j].LastActivity)
		}
		return out[i].RootPath < out[j].RootPath
	})
	return out
}

// loadHistoryThread reads a thread's root and replies, for a page that
// shows it. A message moved away (MoveToRedacted) since ref was listed
// is out of every view: a reply is left out, and a root is
// store.ErrNotFound, the whole thread gone from this page.
func (s *Server) loadHistoryThread(ref historyThreadRef) (historyThread, error) {
	root, err := s.Store.ReadMessage(filepath.Join(s.Store.Root(), ref.RootPath))
	if err != nil {
		return historyThread{}, err
	}
	t := historyThread{
		Root:         root,
		RootPath:     ref.RootPath,
		RootView:     agent.BuildInboxView(root, ref.RootPath),
		LastActivity: ref.LastActivity,
	}
	for _, p := range ref.ReplyPaths {
		m, err := s.Store.ReadMessage(filepath.Join(s.Store.Root(), p))
		if errors.Is(err, store.ErrNotFound) {
			continue
		}
		if err != nil {
			return historyThread{}, err
		}
		t.Replies = append(t.Replies, historyReply{Message: m, Path: p, View: agent.BuildInboxView(m, p)})
	}
	return t, nil
}
