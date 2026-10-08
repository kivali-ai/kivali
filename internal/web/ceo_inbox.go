package web

import (
	"errors"
	"path/filepath"
	"sort"
	"time"

	"github.com/kivali-ai/kivali/internal/agent"
	"github.com/kivali-ai/kivali/internal/assignments"
	"github.com/kivali-ai/kivali/internal/store"
)

// CEOInboxItem represents one request → (optional) response pair in the
// CEO inbox. Request is the agent-published ceo_approval_request or
// ceo_notification; Response is the CEO's reply (approval_response or
// notification_ack) when present. Resolved is true iff Response != nil.
type CEOInboxItem struct {
	Queue       store.ChatMessage // the incoming Kind="ceo_inbox" entry in CEO chat.jsonl
	Request     store.Message
	RequestPath string // relative to store root
	RequestKind string // "approval" | "notification" | "assignment"

	Response     *store.Message
	ResponsePath string
	Resolved     bool
	// ApprovedLabel is "approved" / "denied" / "acknowledged" when
	// resolved, empty otherwise. Drives pill styling + thread header.
	ApprovedLabel string
	// Assignment is the assignment an assignment event card is about
	// (RequestKind "assignment"). The card links to it and, while the
	// assignment is open and
	// assigned to the CEO, carries the close form that answers it.
	Assignment *assignments.Assignment
}

// CEOInboxView is the CEO's inbox split in two. Needs contains
// un-resolved items (awaiting an approve/deny/ack action); History
// contains resolved items (already responded to). Both buckets are
// ordered newest-first.
type CEOInboxView struct {
	Needs   []CEOInboxItem
	History []CEOInboxItem
	// HistoryPage + HistoryPages + HistoryTotal track pagination on
	// the History bucket when the caller asked for a specific page.
	// The Needs bucket is never paginated — unresolved items should
	// always be fully visible.
	HistoryPage  int
	HistoryPages int
	HistoryTotal int
}

func (v CEOInboxView) NeedsCount() int   { return len(v.Needs) }
func (v CEOInboxView) HistoryCount() int { return len(v.History) }

// historyPageSize is the default page size for the CEO inbox History
// bucket. Resolved items older than the first page are reachable via
// ?page=N on /agents/ceo.
const historyPageSize = 25

// decisionLabel releases a CEO response message into its inbox label.
// Empty string if msg is not a known CEO reply type.
func decisionLabel(msg store.Message) string {
	switch {
	case msg.Type == store.MsgCEOApprovalResponse && msg.Approved != nil && *msg.Approved:
		return "approved"
	case msg.Type == store.MsgCEOApprovalResponse && msg.Approved != nil && !*msg.Approved:
		return "denied"
	case msg.Type == store.MsgCEONotificationAck:
		return "acknowledged"
	}
	return ""
}

// buildCEOInbox reads the CEO's chat.jsonl, pairs each ceo_inbox entry
// with its corresponding response (located by the response message's
// InReplyTo field), and splits into needs-action / history buckets.
// Page is 1-based; 0 or negative is coerced to 1. Missing store reads
// are treated as "skip this item" rather than a hard error — the inbox
// view is a structural projection, not a full log.
func (s *Server) buildCEOInbox(page int) (CEOInboxView, error) {
	hist, err := s.Store.ReadChatHistory(agent.CEOSlug)
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		return CEOInboxView{}, err
	}

	// Pre-scan chat.jsonl for CEO reply entries and, for each, read
	// the underlying response message so we can look up its InReplyTo
	// (the target request path).
	responses := map[string]struct {
		entry store.ChatMessage
		msg   store.Message
	}{}
	for _, e := range hist {
		if e.Kind != "ceo_reply" || e.MessageRef == "" {
			continue
		}
		resp, err := s.Store.ReadMessage(filepath.Join(s.Store.Root(), e.MessageRef))
		if err != nil {
			continue
		}
		if resp.InReplyTo == "" {
			continue
		}
		responses[resp.InReplyTo] = struct {
			entry store.ChatMessage
			msg   store.Message
		}{entry: e, msg: resp}
	}

	// The assignment set is read once for every assignment_event card on the
	// page; a tracker that cannot be read leaves those cards without
	// their assignment rather than failing the whole inbox.
	var assignmentSet *assignments.Set
	if tr := s.tracker(); tr != nil {
		assignmentSet, _ = tr.Set()
	}

	// Walk the ceo_inbox entries in order (they're the "received"
	// side of the thread), build CEOInboxItems, pair with responses.
	var items []CEOInboxItem
	for _, e := range hist {
		if e.Kind != "ceo_inbox" || e.MessageRef == "" {
			continue
		}
		req, err := s.Store.ReadMessage(filepath.Join(s.Store.Root(), e.MessageRef))
		if err != nil {
			continue
		}
		item := CEOInboxItem{
			Queue:       e,
			Request:     req,
			RequestPath: e.MessageRef,
		}
		switch req.Type {
		case store.MsgCEOApprovalRequest:
			item.RequestKind = "approval"
		case store.MsgCEONotification:
			item.RequestKind = "notification"
		case store.MsgAssignmentEvent:
			// The tracker speaking to the CEO. Nothing is replied to;
			// the card is resolved unless it is an assignment the CEO
			// still holds, which the close form on the card answers.
			item.RequestKind = "assignment"
			iss, needsAction := s.assignmentEventCEOItem(assignmentSet, req)
			item.Assignment = iss
			item.Resolved = !needsAction
			items = append(items, item)
			continue
		default:
			// Not a CEO-flow message — ignore.
			continue
		}
		if r, ok := responses[e.MessageRef]; ok {
			msg := r.msg
			item.Response = &msg
			item.ResponsePath = r.entry.MessageRef
			item.Resolved = true
			item.ApprovedLabel = decisionLabel(msg)
		}
		items = append(items, item)
	}

	// Newest-activity-first within each bucket. "Activity" is the most
	// recent of (request date, response date): a stale request that
	// just got a fresh reply ranks above a more recently-filed but
	// stale-resolved one. Mirrors buildHistoryThreads's LastActivity
	// sort so Needs you and Home's history follow the same rule. The
	// Needs bucket is
	// always Resolved=false, so its sort key reduces to Request.Date —
	// no behavior change there.
	lastActivity := func(it CEOInboxItem) time.Time {
		if it.Response != nil && it.Response.Date.After(it.Request.Date) {
			return it.Response.Date
		}
		return it.Request.Date
	}
	sort.SliceStable(items, func(i, j int) bool {
		return lastActivity(items[i]).After(lastActivity(items[j]))
	})

	// One assignment, one card to act on. An assignment assigned to the CEO,
	// closed, then reopened has two assignment events on file; only
	// the newest asks for anything, the rest are history.
	newest := map[int]int64{}
	for _, it := range items {
		if it.RequestKind == "assignment" && !it.Resolved && it.Request.Assignment != nil && it.Request.Assignment.Seq > newest[it.Request.Assignment.ID] {
			newest[it.Request.Assignment.ID] = it.Request.Assignment.Seq
		}
	}
	for i := range items {
		it := &items[i]
		if it.RequestKind == "assignment" && !it.Resolved && it.Request.Assignment != nil && it.Request.Assignment.Seq < newest[it.Request.Assignment.ID] {
			it.Resolved = true
		}
	}

	var v CEOInboxView
	for _, it := range items {
		if it.Resolved {
			v.History = append(v.History, it)
		} else {
			v.Needs = append(v.Needs, it)
		}
	}
	v.HistoryTotal = len(v.History)
	if page < 1 {
		page = 1
	}
	v.HistoryPage = page
	v.HistoryPages = (v.HistoryTotal + historyPageSize - 1) / historyPageSize
	start := (page - 1) * historyPageSize
	if start >= len(v.History) {
		v.History = nil
	} else {
		end := start + historyPageSize
		if end > len(v.History) {
			end = len(v.History)
		}
		v.History = v.History[start:end]
	}
	return v, nil
}

// pendingCEOInbox returns the count of unresolved ceo_inbox items.
// Feeds QueueStatus.InboxUnactioned in the org snapshot.
func (s *Server) pendingCEOInbox() int {
	n, _ := s.ceoNeeds()
	return n
}
