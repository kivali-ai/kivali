package web

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/kivali-ai/kivali/internal/agent"
	"github.com/kivali-ai/kivali/internal/store"
)

// TestInboxOrderingNewestFirst covers the three inbox surfaces that
// the user sees and asserts they all rank by most-recent-activity-first:
//
//  1. For-agents (pending deliveries) — `buildPendingList`. Sort key is
//     the message's own Date; recipient does NOT influence ordering.
//  2. CEO inbox History — `buildCEOInbox`. Sort key is max(Request.Date,
//     Response.Date) so a freshly-resolved old request outranks a
//     stale-resolved newer one.
//  3. Message history threads — `buildHistoryThreads`. Sort key is the
//     thread's LastActivity (root + any reply), so an old root with a
//     fresh reply outranks a brand-new root that has no replies.
//
// One test exercises all three so the contract is visible in one place:
// "newest activity first, everywhere."
func TestInboxOrderingNewestFirst(t *testing.T) {
	srv, _ := newTurnServer(t)
	_ = srv.Store.CreateAgent(store.Agent{Slug: "chief-of-staff", Role: "CoS", ReportsTo: "ceo"}, "k")
	_ = srv.Store.CreateAgent(store.Agent{Slug: "alice", Role: "Analyst", ReportsTo: "chief-of-staff"}, "k")
	_ = srv.Store.CreateAgent(store.Agent{Slug: "bob", Role: "Bookkeeper", ReportsTo: "chief-of-staff"}, "k")
	_ = srv.Store.CreateAgent(store.Agent{Slug: agent.CEOSlug, Role: "CEO"}, "")

	now := time.Now().UTC().Truncate(time.Second)

	// --- 1) For-agents (pending) -----------------------------------
	// Two recipients with deliberately interleaved dates. The newest
	// delivery is for "bob" (alphabetically later than alice) — so a
	// sort that puts recipient first would put alice ahead of bob and
	// break this assertion.
	oldestPending := writePendingAt(t, srv, "chief-of-staff", "alice", "old pending", now.Add(-3*time.Hour))
	middlePending := writePendingAt(t, srv, "chief-of-staff", "alice", "mid pending", now.Add(-2*time.Hour))
	newestPending := writePendingAt(t, srv, "chief-of-staff", "bob", "new pending", now.Add(-1*time.Hour))

	ts, _ := srv.Store.ReadMessageQueue()
	pending := srv.buildPendingList(ts)
	if len(pending) != 3 {
		t.Fatalf("pending len = %d, want 3", len(pending))
	}
	wantPending := []string{newestPending, middlePending, oldestPending}
	gotPending := make([]string, 0, len(pending))
	for _, p := range pending {
		gotPending = append(gotPending, p.Path)
	}
	for i := range wantPending {
		if gotPending[i] != wantPending[i] {
			t.Errorf("pending[%d] = %s, want %s", i, gotPending[i], wantPending[i])
		}
	}

	// --- 2) CEO inbox History --------------------------------------
	// Three approval requests with strictly decreasing original dates.
	// Resolve the oldest with a fresh reply and the middle one with a
	// stale reply; leave the newest unresolved. By "newest activity"
	// the resolved-old thread must lead the History bucket.
	oldReq := seedCEOApprovalRequestAt(t, srv, "alice", "old req", "...", now.Add(-10*time.Hour))
	midReq := seedCEOApprovalRequestAt(t, srv, "alice", "mid req", "...", now.Add(-5*time.Hour))
	newReq := seedCEOApprovalRequestAt(t, srv, "alice", "new req", "...", now.Add(-1*time.Hour))

	writeApprovalResponseAt(t, srv, "alice", oldReq, true, now.Add(-30*time.Minute))
	writeApprovalResponseAt(t, srv, "alice", midReq, true, now.Add(-4*time.Hour))

	view, err := srv.buildCEOInbox(1)
	if err != nil {
		t.Fatalf("buildCEOInbox: %v", err)
	}
	if len(view.Needs) != 1 || view.Needs[0].RequestPath != newReq {
		t.Errorf("Needs = %+v, want one entry for %s", view.Needs, newReq)
	}
	if len(view.History) != 2 {
		t.Fatalf("History len = %d, want 2; got %+v", len(view.History), view.History)
	}
	if view.History[0].RequestPath != oldReq {
		t.Errorf("History[0] = %s, want %s (latest reply on this thread is the newest activity)",
			view.History[0].RequestPath, oldReq)
	}
	if view.History[1].RequestPath != midReq {
		t.Errorf("History[1] = %s, want %s", view.History[1].RequestPath, midReq)
	}

	// --- 3) Message history threads --------------------------------
	// freshRoot has no replies; oldRoot has a very recent reply.
	// "Newest activity first" must put oldRoot ahead of freshRoot.
	freshRoot := writeTaskRequestAt(t, srv, "chief-of-staff", "alice", "fresh root", now.Add(-2*time.Hour))
	oldRoot := writeTaskRequestAt(t, srv, "chief-of-staff", "alice", "old root", now.Add(-20*time.Hour))
	writeStatusReplyAt(t, srv, "alice", "chief-of-staff", oldRoot, now.Add(-15*time.Minute))

	all, err := srv.Store.ListMessages(store.MessageFilter{})
	if err != nil {
		t.Fatalf("ListMessages: %v", err)
	}
	threads, _ := srv.buildHistoryThreads(all, nil, 100)
	posOld, posFresh := -1, -1
	for i, th := range threads {
		switch th.RootPath {
		case oldRoot:
			posOld = i
		case freshRoot:
			posFresh = i
		}
	}
	if posOld == -1 || posFresh == -1 {
		t.Fatalf("missing thread: posOld=%d posFresh=%d", posOld, posFresh)
	}
	if posOld >= posFresh {
		t.Errorf("old-rooted thread with fresh reply (pos=%d) must outrank fresh-rooted thread with no replies (pos=%d)",
			posOld, posFresh)
	}
}

// writePendingAt writes a notice from `from` to `to` with the
// given Date, queues it on the recipient's release-state inbox, and
// returns the relative path of the message file. Used to seed the
// For-agents pending list at a controlled timestamp.
func writePendingAt(t *testing.T, srv *Server, from, to, body string, when time.Time) string {
	t.Helper()
	m := store.Message{
		Type:  store.MsgNotice,
		Title: body,
		From:  from,
		To:    store.Recipients{to},
		Date:  when,
		Body:  body,
	}
	abs, err := srv.Store.WriteMessage(m)
	if err != nil {
		t.Fatalf("write pending: %v", err)
	}
	rel, _ := filepath.Rel(srv.Store.Root(), abs)
	ts, _ := srv.Store.ReadMessageQueue()
	if ts.Agents == nil {
		ts.Agents = map[string]store.AgentQueue{}
	}
	rt := ts.Agents[to]
	rt.Inbox = append(rt.Inbox, rel)
	ts.Agents[to] = rt
	if err := srv.Store.WriteMessageQueue(ts); err != nil {
		t.Fatalf("write message queue: %v", err)
	}
	return rel
}

// seedCEOApprovalRequestAt is the Date-controlled variant of
// seedCEOApprovalRequest. The plain helper uses time.Now() which
// collides under fast successive calls; ordering tests need explicit
// timestamps so the test result doesn't depend on wall-clock spacing.
func seedCEOApprovalRequestAt(t *testing.T, srv *Server, from, title, body string, when time.Time) string {
	t.Helper()
	req := store.Message{
		Type:  store.MsgCEOApprovalRequest,
		Title: title,
		From:  from,
		To:    store.Recipients{agent.CEOSlug},
		Date:  when,
		Body:  body,
	}
	abs, err := srv.Store.WriteMessage(req)
	if err != nil {
		t.Fatalf("write approval req: %v", err)
	}
	rel, _ := filepath.Rel(srv.Store.Root(), abs)
	if err := srv.Store.AppendChatMessage(agent.CEOSlug, store.ChatMessage{
		Role:       store.RoleReceived,
		Content:    title,
		Kind:       "ceo_inbox",
		MessageRef: rel,
	}); err != nil {
		t.Fatalf("append ceo inbox: %v", err)
	}
	return rel
}

// writeApprovalResponseAt writes a ceo_approval_response that targets
// `inReplyTo` and records a kind="ceo_reply" chat entry on the CEO's
// chat.jsonl so buildCEOInbox can pair the response with its request.
// Mirrors the production approve handler in ceo.go, minus the side-
// effects (no delivery, no provisioning) the inbox builder doesn't
// care about.
func writeApprovalResponseAt(t *testing.T, srv *Server, to, inReplyTo string, approved bool, when time.Time) string {
	t.Helper()
	respMsg := store.Message{
		Type:      store.MsgCEOApprovalResponse,
		Title:     "Approved: " + inReplyTo,
		From:      agent.CEOSlug,
		To:        store.Recipients{to},
		Date:      when,
		InReplyTo: inReplyTo,
		Approved:  &approved,
		Body:      "ok",
	}
	abs, err := srv.Store.WriteMessage(respMsg)
	if err != nil {
		t.Fatalf("write resp: %v", err)
	}
	rel, _ := filepath.Rel(srv.Store.Root(), abs)
	if err := srv.Store.AppendChatMessage(agent.CEOSlug, store.ChatMessage{
		Role:              store.RoleSent,
		Content:           respMsg.Title,
		Kind:              "ceo_reply",
		MessageRef:        rel,
		ReplyToMessageRef: inReplyTo,
	}); err != nil {
		t.Fatalf("append ceo reply: %v", err)
	}
	return rel
}

// writeTaskRequestAt writes a notice with an explicit Date and
// returns the relative path. Bare message write — does NOT queue the
// message on any agent's release-state inbox, so it ends up in
// history rather than the For-agents bucket.
func writeTaskRequestAt(t *testing.T, srv *Server, from, to, title string, when time.Time) string {
	t.Helper()
	m := store.Message{
		Type:  store.MsgNotice,
		Title: title,
		From:  from,
		To:    store.Recipients{to},
		Date:  when,
		Body:  title,
	}
	abs, err := srv.Store.WriteMessage(m)
	if err != nil {
		t.Fatalf("write task request: %v", err)
	}
	rel, _ := filepath.Rel(srv.Store.Root(), abs)
	return rel
}

// writeStatusReplyAt writes a notice with InReplyTo pointing at
// `inReplyTo` so buildHistoryThreads groups it under that root.
// Returns the new message's relative path.
func writeStatusReplyAt(t *testing.T, srv *Server, from, to, inReplyTo string, when time.Time) string {
	t.Helper()
	m := store.Message{
		Type:      store.MsgNotice,
		Title:     "reply",
		From:      from,
		To:        store.Recipients{to},
		Date:      when,
		InReplyTo: inReplyTo,
		Body:      "reply body",
	}
	abs, err := srv.Store.WriteMessage(m)
	if err != nil {
		t.Fatalf("write status reply: %v", err)
	}
	rel, _ := filepath.Rel(srv.Store.Root(), abs)
	return rel
}
