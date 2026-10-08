// Package messaging owns the message-routing layer between agents.
//
// It's the layer above persistence (internal/store) and below the
// HTTP handlers (internal/web). When an agent publishes a message,
// messaging is what decides where it lands:
//
//   - agent_role -> instantiate a new agent immediately (no queue).
//   - ceo_approval_request / ceo_notification -> instant-append to
//     the CEO's chat.jsonl as Kind="ceo_inbox".
//   - everything else -> the recipient's Inbox queue, awaiting the
//     next /release.
//
// A publish routes inside the tool call: the agent pod's staged
// commit calls Messenger.RouteProduced under the queue lock, so a
// message is queued the moment it is sent, not when the sending turn
// ends. In-process publishers (tests) reach the same rules through
// Messenger.Route. FSStore.LockMessageQueue serializes MessageQueue
// writes across all callers — Messenger and non-Messenger (hire flow,
// state tools).
package messaging

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/kivali-ai/kivali/internal/agent"
	"github.com/kivali-ai/kivali/internal/store"
)

// ErrNotQueued is returned when Bounce can't find the supplied path
// in any agent's inbox queue. The HTTP layer maps this to 409 Conflict.
// ReleaseSelected surfaces the same "stale card" case via its notFound
// return value instead of an error (so a partially-stale batch still
// drains the valid paths).
var ErrNotQueued = errors.New("messaging: message is not currently queued for delivery")

// DeliveryHooks bundles the web-layer callbacks Messenger needs
// when delivering a message into an agent's chat (vs. queueing it
// onto the release inbox). Each is optional — a nil hook falls back
// to a direct Store write or a no-op wake.
//
//   - DeliverToAgent routes a chat-bubble through web's mid-flight
//     buffer: appends to chat.jsonl when the agent is idle, or
//     holds the message in pendingDeliveries when a loop is running
//     for that agent (flushed at loop-end). Without buffering, an
//     instant delivery could interleave between a running response's
//     tool flow and its final reply.
//
//   - WakeAgent spawns the recipient's chat loop so the new
//     delivery is responded to without waiting for the next
//     /release. Returns true on a fresh spawn; false if a loop was
//     already running (the buffer flush at that loop's end will
//     handle the new entry).
type DeliveryHooks struct {
	DeliverToAgent func(slug string, msg store.ChatMessage) error
	WakeAgent      func(slug string) bool
}

// Delivery captures one inbox delivery or agent_role instantiation
// produced during routing.
type Delivery struct {
	DocPath      string
	Recipient    string
	Instantiated bool
}

// RouteOutcome bundles the side-effects of a routing pass.
// Warnings are non-fatal issues (dead-letter, summarizer failure).
// Deliveries lists the inbox entries / instantiated agents.
type RouteOutcome struct {
	Warnings   []string
	Deliveries []Delivery
}

// Messenger applies the message-routing rules. One per process.
//
// MessageQueue serialization lives on FSStore (Lock/UnlockMessageQueue);
// Messenger's Lock/Unlock methods delegate to it so callers outside
// the messaging package (the tracker's queue prune) participate in
// the same critical section. Route holds
// the lock for the read-modify-write cycle on a single message; the
// runtime holds it (via Lock/Unlock) for the duration of a ReleaseAll
// batch so chat-path Route calls block until the batch finishes.
type Messenger struct {
	Store   *store.FSStore
	Runtime *agent.Runtime
}

// New constructs a Messenger.
func New(s *store.FSStore, r *agent.Runtime) *Messenger {
	return &Messenger{Store: s, Runtime: r}
}

// Lock acquires the message-queue serialization lock. Used by the
// runtime to hold it across a full ReleaseAll batch so chat-path
// Route calls queue up behind the batch.
func (m *Messenger) Lock() { m.Store.LockMessageQueue() }

// Unlock releases the message-queue serialization lock.
func (m *Messenger) Unlock() { m.Store.UnlockMessageQueue() }

// Route routes a single message produced outside of a normal release
// (e.g. an agent published from direct chat). Reads the current
// MessageQueue, applies the routing rules, and writes the state back.
// Locks the messenger for the duration so concurrent Route calls and
// any in-flight ReleaseAll batch serialize correctly.
func (m *Messenger) Route(ctx context.Context, d store.Message) (*RouteOutcome, error) {
	m.Store.LockMessageQueue()
	defer m.Store.UnlockMessageQueue()
	ts, err := m.Store.ReadMessageQueue()
	if err != nil {
		return nil, fmt.Errorf("read release state: %w", err)
	}
	out := &RouteOutcome{}
	if err := m.RouteProduced(ctx, &ts, []store.Message{d}, out); err != nil {
		return out, err
	}
	if err := m.Store.WriteMessageQueue(ts); err != nil {
		return out, fmt.Errorf("write release state: %w", err)
	}
	return out, nil
}

// RouteProduced applies routing rules to a batch of produced messages,
// mutating ts in place with updated inboxes. Two fast paths bypass
// the normal release inbox queue:
//
//  1. ceo_approval_request / ceo_notification — instant-append to
//     the CEO's chat.jsonl as Kind="ceo_inbox".
//  2. Everything else — goes through the recipient's Inbox queue.
//
// CALLER must serialize concurrent ts writers — typically by holding
// Messenger.Lock() for the duration of a release batch, or by going
// through Messenger.Route which locks per-call.
func (m *Messenger) RouteProduced(ctx context.Context, ts *store.MessageQueue, produced []store.Message, out *RouteOutcome) error {
	for _, d := range produced {
		relPath := m.relPath(d.Path)
		if d.DeliversToCEO() {
			if err := m.DeliverToCEONow(d, relPath); err != nil {
				out.Warnings = append(out.Warnings, fmt.Sprintf("ceo inbox %s: %v", relPath, err))
			}
			continue
		}
		// Route to an agent inbox — one pointer per recipient. An
		// assignment_event has exactly one; a notice can have several, and
		// each is moderated on its own, so an unreachable recipient
		// dead-letters without affecting the pointers that did land.
		for _, recipient := range d.To {
			if _, err := m.Store.GetAgent(recipient); err != nil {
				// Dead-letter: send a CEO-authored notice back to the
				// sender so they actually learn their message went
				// nowhere. Without this the message is silently dropped
				// — the publishing agent's tool returned success but the
				// recipient never gets the message and the sender never
				// gets a signal anything went wrong. Sender re-tries
				// indefinitely or waits forever for a reply that won't
				// come.
				//
				// The dead-letter goes through the same queue path as
				// any notice — the CEO sees it on the next release,
				// the sender sees it after that release fires.
				// Symmetric with Bounce, just without an explicit CEO
				// click.
				if w := m.queueDeadLetter(ts, d, relPath, recipient); w != "" {
					out.Warnings = append(out.Warnings, w)
				}
				continue
			}
			rt := ts.Agents[recipient]
			// A path is one message; queueing it twice for the same
			// recipient is always a replay (the tracker re-routing a
			// wake after a crash), never a second message.
			if !containsPath(rt.Inbox, relPath) {
				rt.Inbox = append(rt.Inbox, relPath)
			}
			ts.Agents[recipient] = rt
		}
	}
	return nil
}

func containsPath(paths []string, p string) bool {
	for _, q := range paths {
		if q == p {
			return true
		}
	}
	return false
}

// queueDeadLetter writes a CEO-authored notice explaining that
// `recipient` doesn't exist, and queues it in d.From's inbox.
// Returns a non-empty warning string when the dead-letter itself can't
// be delivered (sender also archived, disk failure on the dead-letter
// write); the caller appends it to the route outcome's warnings.
//
// Scoped to one recipient rather than the whole message: a notice to
// three agents where one has been archived dead-letters that one and
// delivers the other two, and the sender's dead-letter names which.
//
// Mutates ts in place. Caller must hold Messenger.mu (RouteProduced
// is documented to require this).
func (m *Messenger) queueDeadLetter(ts *store.MessageQueue, d store.Message, relPath, recipient string) string {
	// Sender also archived — nowhere to bounce to. Warn and stop;
	// don't recurse.
	if _, err := m.Store.GetAgent(d.From); err != nil {
		return fmt.Sprintf("dead-letter %s: no active recipient %q AND sender %q is also archived; message orphaned", relPath, recipient, d.From)
	}
	dl := store.Message{
		// Same shape as a bounce, for the same reason: telling a sender
		// their message went nowhere is a tell. See ceoUndeliveredIsATell.
		Type:  ceoUndeliveredIsATell,
		Title: "Could not deliver: " + d.Title,
		From:  agent.CEOSlug,
		To:    store.Recipients{d.From},
		Date:  time.Now().UTC(),
		Body: fmt.Sprintf(
			"Your %s to %q could not be delivered: that agent does not exist or has been archived. The original message is preserved at %s for the forensic record.\n",
			d.Type, recipient, relPath,
		),
	}
	abs, err := m.Store.WriteMessage(dl)
	if err != nil {
		return fmt.Sprintf("dead-letter %s: write notice: %v", relPath, err)
	}
	dlRel, _ := filepath.Rel(m.Store.Root(), abs)
	rt := ts.Agents[d.From]
	rt.Inbox = append(rt.Inbox, dlRel)
	ts.Agents[d.From] = rt
	return ""
}

// DeliverToCEONow instant-appends a CEO-bound message to the CEO's
// chat.jsonl as a Kind="ceo_inbox" entry. The inbox UI renders these
// with Approve/Deny/Ack actions. No summarization — the title carries
// the semantics; the full body is one click away.
func (m *Messenger) DeliverToCEONow(d store.Message, relPath string) error {
	if _, err := m.Store.GetAgent(agent.CEOSlug); err != nil {
		// CEO record missing on a fresh deployment — create it. A
		// failure here that ISN'T "agent already active" (which would
		// happen if a concurrent caller raced to create it; tolerate
		// silently) is genuinely fatal: the AppendChatMessage below
		// would return ErrNotFound with a less actionable message.
		if cerr := m.Store.CreateAgent(store.Agent{Slug: agent.CEOSlug, Role: "CEO"}, ""); cerr != nil && !strings.Contains(cerr.Error(), "already active") {
			return fmt.Errorf("create ceo record: %w", cerr)
		}
	}
	return m.Store.AppendChatMessage(agent.CEOSlug, store.ChatMessage{
		Role:        store.RoleReceived,
		Content:     d.Title,
		Kind:        "ceo_inbox",
		MessageRef:  relPath,
		Attachments: d.Attachments,
	})
}

func (m *Messenger) relPath(abs string) string {
	rel, err := filepath.Rel(m.Store.Root(), abs)
	if err != nil {
		return abs
	}
	return rel
}

func (m *Messenger) absPath(rel string) string {
	return filepath.Join(m.Store.Root(), rel)
}

// DeliverToAgent is the canonical "deliver this published message
// into an agent's chat right now" primitive. Used by Release,
// Bounce, and the CEO-reply flow.
//
// The flow:
//
//  1. Validate the recipient (non-empty, not the CEO slug, exists
//     in the store).
//  2. Render the inbox-delivery body via agent.RenderInboxBody.
//  3. Append to chat.jsonl through the buffered hook so we never
//     interleave into a mid-flight response — the buffer flushes
//     atomically at loop-end. Falls back to a direct Store write
//     when no hook is supplied (test harnesses).
//  4. Leave /files/attachments/ to the store: AppendChatMessage links
//     a row's files when the row lands. The web hook covers the one
//     case that cannot: a message folded into a turn already running
//     makes its own links before it is folded.
//  5. Wake the recipient unless the message asks for no turn (see
//     noTurn): it declares itself Quiet, or it is a pure receipt.
//     Such a delivery is marked Quiet so the spawn gate walks past it
//     too; it folds into a turn already running and is otherwise read
//     with the next wake.
//
// Returns false on recipient-validation failure or buffered-write
// failure; the caller decides whether to fall back (CEO-reply
// path) or treat it as a no-op (Release/Bounce paths).
func (m *Messenger) DeliverToAgent(ctx context.Context, recipient string, msg store.Message, relPath string, hooks DeliveryHooks) bool {
	if recipient == "" || recipient == agent.CEOSlug {
		return false
	}
	if _, err := m.Store.GetAgent(recipient); err != nil {
		return false
	}
	// The delivery is the message and nothing else. What the recipient
	// holds in the assignment tracker is not repeated here: the runtime's
	// wake note (agent.BuildWakeUpdate) lists it once per wake, behind
	// whatever woke the agent, and a message folded into a running turn
	// brings no copy of it.
	chatMsg := store.ChatMessage{
		Role:        store.RoleReceived,
		Content:     agent.RenderInboxBody(msg, relPath),
		Kind:        "inbox_delivery",
		MessageRef:  relPath,
		Attachments: msg.Attachments,
		Quiet:       noTurn(msg),
	}
	if hooks.DeliverToAgent != nil {
		if err := hooks.DeliverToAgent(recipient, chatMsg); err != nil {
			return false
		}
	} else {
		if err := m.Store.AppendChatMessage(recipient, chatMsg); err != nil {
			return false
		}
	}
	// AppendChatMessage linked the row's attachments as it landed
	// (the flush does the same for a buffered one). No per-edge
	// guard here.
	if hooks.WakeAgent != nil && !chatMsg.Quiet {
		_ = hooks.WakeAgent(recipient)
	}
	return true
}

// ReleaseAll drains every agent's pending inbox into chat.jsonl and
// wakes every active agent. Fire-and-forget — returns once the drain
// is persisted; per-agent chat loops run async via the WakeAgent
// hook (web's spawnChatLoopIfIdle).
//
// "Release-all" is a CEO-paced batch dispatch: the queue accumulated
// during the prior release window, this call releases it. Each
// recipient sees all of their pending mail at once (preserves the
// "alice plans across her tasks" UX), ordered oldest-first by when
// each sender sent it, then their chat loop fires.
//
// Wakes every active agent — not just the ones with new mail —
// because spawnChatLoopIfIdle is idempotent and gates on chat
// history (last entry must be unanswered received). Agents stuck
// on prior unanswered content from a server crash, a paused chat,
// or anything else get a fresh wake here too.
//
// `comments` is an optional per-path CEO annotation map keyed by
// relPath. For each path with a non-empty entry, the message body is
// rewritten in place to append a `> CEO: ...` markdown block BEFORE
// the chat-delivery, so the released body and any later replay see
// the annotation. The rewrite happens inside the same queue-lock as
// the drain, so the batch stays atomic from a queue-state perspective.
// `nil` / empty map skips annotation for every path.
//
// Returns warnings from delivery failures (a missing recipient,
// archived agent, etc.). Catastrophic store errors return the
// error and skip the wake; the per-message warnings are non-fatal.
func (m *Messenger) ReleaseAll(ctx context.Context, comments map[string]string, hooks DeliveryHooks) ([]string, error) {
	_, _, warnings, err := m.releaseQueue(ctx, nil, comments, hooks)
	return warnings, err
}

// ReleaseSelected drains the supplied subset of pending messages into
// their recipients' chat.jsonl and wakes every active agent. Same
// atomic-drain semantics as ReleaseAll (single lock, all annotations
// applied before any delivery, broadcast wake) — just restricted to
// the requested paths. Use this for single-card quick-release (one
// path) and multi-select Release-selected (N paths) alike, so both
// share the same batching contract.
//
// A message addressed to several agents (a notice) holds one pointer
// per recipient. Releasing it releases all of them: the CEO moderates
// the message, not the individual deliveries, so one card in the inbox
// means one decision here.
//
// Returns:
//   - delivered: paths whose chat-append landed, once per path however
//     many recipients it reached.
//   - notFound:  paths from the request that weren't in any agent's
//     inbox at lock-acquire time. Callers that need strict
//     "stale card" semantics (single quick-release → 409)
//     check this; batched callers can discard it.
//   - warnings:  non-fatal per-path issues (read/annotate/append
//     failures). Same shape as ReleaseAll's warnings.
//
// `paths` must be non-empty; empty input is a no-op returning all-nil
// slices.
func (m *Messenger) ReleaseSelected(ctx context.Context, paths []string, comments map[string]string, hooks DeliveryHooks) (delivered, notFound, warnings []string, err error) {
	if len(paths) == 0 {
		return nil, nil, nil, nil
	}
	return m.releaseQueue(ctx, paths, comments, hooks)
}

// queuedMessage pairs an inbox pointer with the message it resolves
// to, so a release batch can be ordered by send time before any of it
// is appended to chat.jsonl.
type queuedMessage struct {
	path string
	msg  store.Message
}

// releaseQueue is the shared body behind ReleaseAll and ReleaseSelected.
//
// When `filter` is nil, every queued message is drained (the
// ReleaseAll path). When `filter` is non-nil, only paths whose
// relPath appears in the slice are drained; the remainder stays in
// the queue for the next release cycle.
//
// Within one recipient, the drained batch is appended oldest-first by
// the sender's send time (Message.Date) rather than by queue position
// — see the comment on the drain loop for why those differ.
//
// Both shapes serialize on the same Store.LockMessageQueue so concurrent
// Release/Bounce/Route writers don't see a partial drain. The wake
// fan-out runs after the unlock — spawnChatLoopIfIdle's idempotent
// last-entry gate makes broadcast wake safe (and necessary, for
// agents recovering from a prior crash).
//
// delivered/notFound are populated only for the filtered shape; callers
// of the nil-filter path receive nil for both (no requested set to
// compare against).
func (m *Messenger) releaseQueue(ctx context.Context, filter []string, comments map[string]string, hooks DeliveryHooks) (delivered, notFound, warnings []string, err error) {
	var wanted map[string]struct{}
	if filter != nil {
		wanted = make(map[string]struct{}, len(filter))
		for _, p := range filter {
			wanted[p] = struct{}{}
		}
	}

	m.Store.LockMessageQueue()
	ts, rerr := m.Store.ReadMessageQueue()
	if rerr != nil {
		m.Store.UnlockMessageQueue()
		return nil, nil, nil, fmt.Errorf("read release state: %w", rerr)
	}

	// Track which of the requested paths landed in any inbox so we
	// can report notFound to filtered callers. Only allocated when a
	// filter is in play.
	var seen map[string]bool
	if wanted != nil {
		seen = make(map[string]bool, len(wanted))
	}
	// A path queued for several recipients drains every pointer in one
	// pass, but reports as one delivered entry — the caller asked about
	// a message, not about each of its deliveries.
	reported := map[string]bool{}

	// Drain matching messages from each agent's pending inbox into
	// their chat.jsonl. Deliveries route through DeliverToAgent so a
	// recipient who's already mid-loop (direct CEO chat, prior follow-up
	// still running) gets the released message buffered and flushed at
	// loop-end — never interleaved with their own tool_use /
	// tool_result entries. Falls back to a direct Store write when no
	// hook is wired (test harnesses without the web layer).
	for slug, rt := range ts.Agents {
		if len(rt.Inbox) == 0 {
			continue
		}
		if _, gerr := m.Store.GetAgent(slug); gerr != nil {
			// Recipient archived between queue and release; drop the
			// agent's queue entries silently (matches the prior
			// release-engine semantics).
			continue
		}
		// Two passes over the agent's queue. The first partitions it
		// into the pointers this release drains and the ones that
		// stay, reading each drained message off disk. The second
		// orders that batch by send time and appends it to chat.jsonl.
		//
		// The split exists for the ordering. Queue position is the
		// order pointers were ROUTED; Message.Date is when each was
		// SENT. A publish routes inside the tool call, so the two
		// agree for a fresh message, but a pointer can be queued
		// later than its message was sent — the tracker re-routing a
		// wake after a crash, a replayed commit — and draining in
		// queue position would then hand the recipient a conversation
		// that reads out of sequence: the close of an assignment above its
		// assignment, with nothing but the bodies to reconstruct what
		// actually happened first.
		kept := make([]string, 0, len(rt.Inbox))
		batch := make([]queuedMessage, 0, len(rt.Inbox))
		for _, path := range rt.Inbox {
			if wanted != nil {
				if _, ok := wanted[path]; !ok {
					kept = append(kept, path)
					continue
				}
				seen[path] = true
			}
			abs := m.absPath(path)
			msg, mrerr := m.Store.ReadMessage(abs)
			if mrerr != nil {
				warnings = append(warnings, fmt.Sprintf("read %s: %v", path, mrerr))
				// Drop the pointer — keeping it would retry the same
				// failing read on every subsequent release.
				continue
			}
			batch = append(batch, queuedMessage{path: path, msg: msg})
		}
		// Oldest first. Stable, so messages sharing a timestamp — and
		// any carrying a zero date, which sort to the front — keep
		// their queue order instead of shuffling between releases.
		sort.SliceStable(batch, func(i, j int) bool {
			return batch[i].msg.Date.Before(batch[j].msg.Date)
		})
		for _, q := range batch {
			path, msg := q.path, q.msg
			// Apply the optional CEO annotation before chat-delivery
			// so the body delivered into chat and any later replay see
			// the same text. A failed write still releases the message
			// unannotated; the caller sees a warning, the recipient
			// doesn't lose the message on a transient disk error.
			if c := comments[path]; c != "" {
				msg.Body = strings.TrimRight(msg.Body, "\n") + "\n\n> " + m.ownerLabel() + ": " + c + "\n"
				if _, werr := m.Store.WriteMessage(msg); werr != nil {
					warnings = append(warnings, fmt.Sprintf("annotate %s: %v", path, werr))
				}
			}
			chatMsg := store.ChatMessage{
				Role:        store.RoleReceived,
				Content:     agent.RenderInboxBody(msg, path),
				Kind:        "inbox_delivery",
				MessageRef:  path,
				Attachments: msg.Attachments,
				Quiet:       noTurn(msg),
			}
			var aerr error
			if hooks.DeliverToAgent != nil {
				aerr = hooks.DeliverToAgent(slug, chatMsg)
			} else {
				aerr = m.Store.AppendChatMessage(slug, chatMsg)
			}
			if aerr != nil {
				warnings = append(warnings, fmt.Sprintf("append %s: %v", slug, aerr))
				continue
			}
			if !reported[path] {
				reported[path] = true
				delivered = append(delivered, path)
			}
			// AppendChatMessage links the row's attachments as it
			// lands.
		}
		rt.Inbox = kept
		ts.Agents[slug] = rt
	}

	if werr := m.Store.WriteMessageQueue(ts); werr != nil {
		m.Store.UnlockMessageQueue()
		return delivered, notFound, warnings, fmt.Errorf("write release state: %w", werr)
	}
	m.Store.UnlockMessageQueue()

	if wanted != nil {
		for _, p := range filter {
			if !seen[p] {
				notFound = append(notFound, p)
			}
		}
	}

	// Wake every active agent. spawnChatLoopIfIdle is idempotent and
	// gates internally on "last entry is unanswered received," so
	// agents who have nothing to do see no spawn. Cheap chat-history
	// read per agent.
	if hooks.WakeAgent != nil {
		actives, lerr := m.Store.ListActiveAgents()
		if lerr != nil {
			warnings = append(warnings, fmt.Sprintf("list actives: %v", lerr))
			return delivered, notFound, warnings, nil
		}
		for _, a := range actives {
			if a.Slug == agent.CEOSlug {
				continue
			}
			hooks.WakeAgent(a.Slug)
		}
	}

	return delivered, notFound, warnings, nil
}

// Bounce cancels a queued message and tells its sender. It drops every
// inbox pointer, relocates the message out of the live ledger, and
// emits a CEO-authored notice back to the original sender carrying the
// comment. comment is required by the HTTP layer; an empty comment is
// rejected upstream before reaching here.
//
// The relocation is what makes the cancellation true rather than
// cosmetic. Dropping the pointer stops the delivery, but the file stays
// discoverable by every messages/ scan, and a cancelled message that
// every scan still finds reads as sent. The file keeps its forensic
// trail under messages/.redacted/.
func (m *Messenger) Bounce(ctx context.Context, relPath string, comment string, hooks DeliveryHooks) error {
	abs := m.absPath(relPath)
	original, err := m.Store.ReadMessage(abs)
	if err != nil {
		return fmt.Errorf("read %s: %w", relPath, err)
	}
	dropped, err := m.dropPendingPointer(relPath)
	if err != nil {
		return err
	}
	// Name the recipients this bounce actually cancelled — every
	// addressee still queued, since a bounce cancels the message
	// rather than one of its deliveries.
	bouncedTo := store.Recipients(dropped).String()
	bounce := store.Message{
		// A bounce is a tell: the CEO is reporting that a message did
		// not go out, not answering an ask. See ceoUndeliveredIsATell.
		Type:  ceoUndeliveredIsATell,
		Title: "Bounced by " + m.ownerLabel() + ": " + original.Title,
		From:  agent.CEOSlug,
		To:    store.Recipients{original.From},
		Date:  time.Now().UTC(),
		Body: fmt.Sprintf(
			"Your %s to %s did not go through. %s's comment:\n\n%s\n",
			original.Type, bouncedTo, m.ownerLabel(), comment,
		),
	}
	path, err := m.Store.WriteMessage(bounce)
	if err != nil {
		return fmt.Errorf("write bounce: %w", err)
	}
	bounce.Path = path
	bounceRel := m.relPath(path)
	m.DeliverToAgent(ctx, original.From, bounce, bounceRel, hooks)
	// Last, so a disk failure here can never cost the sender their
	// notification: by this point they have been told, and the worst
	// case is a cancelled message left in the ledger, surfaced as an
	// error rather than silence.
	if _, err := m.Store.MoveToRedacted(abs); err != nil {
		return fmt.Errorf("bounce of %s was delivered to %s, but the cancelled message could not be relocated out of the live ledger: %w", relPath, original.From, err)
	}
	return nil
}

// dropPendingPointer walks every agent's inbox and removes the
// supplied relPath wherever it appears. Returns the recipient slugs it
// was queued under, sorted, or ErrNotQueued if nothing matched. A
// notice queued for three agents drops all three pointers: bouncing
// cancels the message, so it cannot leave some recipients still
// waiting on a delivery the sender has been told did not go through.
//
// Locks Messenger.mu for the read-modify-write cycle so concurrent
// Release/Bounce/Route calls and any in-flight ReleaseAll batch
// serialize correctly.
func (m *Messenger) dropPendingPointer(relPath string) ([]string, error) {
	m.Store.LockMessageQueue()
	defer m.Store.UnlockMessageQueue()
	ts, err := m.Store.ReadMessageQueue()
	if err != nil {
		return nil, fmt.Errorf("read release state: %w", err)
	}
	var recipients []string
	for slug, rt := range ts.Agents {
		filtered := rt.Inbox[:0]
		for _, p := range rt.Inbox {
			if p == relPath {
				recipients = append(recipients, slug)
				continue
			}
			filtered = append(filtered, p)
		}
		rt.Inbox = filtered
		ts.Agents[slug] = rt
	}
	if len(recipients) == 0 {
		return nil, ErrNotQueued
	}
	sort.Strings(recipients)
	if err := m.Store.WriteMessageQueue(ts); err != nil {
		return nil, fmt.Errorf("write release state: %w", err)
	}
	return recipients, nil
}

// ceoUndeliveredIsATell records why a CEO-authored "your message did
// not go out" — a bounce or a dead-letter — is a parentless notice
// whatever it is about.
//
// The CEO is not answering the sender's ask; there is no ask, the
// sender was the one speaking. They are telling them the message went
// nowhere. Nothing is owed back: if the sender still needs the thing,
// they re-send it, and if they need something from someone else they
// file an assignment — exactly what the delivery lead on a notice tells
// them. A notice is also the one agent-bound type nothing may name in
// in_reply_to, so threading a bounce under the original is not an
// option.
const ceoUndeliveredIsATell = store.MsgNotice

// isPureReceipt is true when a CEO-originated message carries no
// content to act on — a notification_ack with no body text and no
// attachments. Waking the recipient for a pure receipt invites the
// model to re-interpret earlier context and confabulate work.
//
// Approval responses (approve/deny) always wake the recipient — the
// decision itself is the action signal, even with an empty body.
// Acks with a typed-through message or attachment also wake — the
// CEO may be relaying something actionable.
func isPureReceipt(m store.Message) bool {
	if m.Type != store.MsgCEONotificationAck {
		return false
	}
	if strings.TrimSpace(m.Body) != "" {
		return false
	}
	if len(m.Attachments) > 0 {
		return false
	}
	return true
}

// noTurn reports whether a delivery asks for no turn of its own: the
// producer declared it Quiet (see store.Message.Quiet; the tracker
// does so on a hold), or it is a pure receipt, which is quiet by its
// content whether or not the producer said so. Either way the
// delivery is marked Quiet on the chat entry so the spawn gate agrees
// on every later broadcast wake: it folds into a turn already running
// and is otherwise read with the next wake.
func noTurn(m store.Message) bool { return m.Quiet || isPureReceipt(m) }

// ownerLabel is what the org calls its person in a label, read now:
// their chosen name, else "CEO" on a work team and "Owner" on a
// personal one. It is also the markup marker ("> Jane: …"), which every
// agent's system prompt names from the same stored name each turn
// (owner.Term.Section), together with the markers used before.
func (m *Messenger) ownerLabel() string {
	br, _ := m.Store.ReadBranding()
	return br.Owner().Label()
}
