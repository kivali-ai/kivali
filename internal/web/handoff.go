package web

import (
	"errors"
	"fmt"
	"log"
	"strings"

	"github.com/kivali-ai/kivali/internal/agent"
	"github.com/kivali-ai/kivali/internal/agentpod"
	"github.com/kivali-ai/kivali/internal/store"
)

// startNewChat's refusals, told apart by the API handler;
// store.ErrNotFound means no active agent has the slug.
var (
	errNewChatInvalid  = errors.New("new chat: invalid target")
	errNewChatNoClaude = errors.New("new chat: Claude not configured")
	errNewChatBusy     = errors.New("new chat: agent is still responding")
)

// startNewChat is handleNewChat's work, shared with the JSON API.
// started is false, with no error, when the chat is empty: there is
// nothing to distill, so nothing happens.
func (s *Server) startNewChat(slug string) (started bool, err error) {
	if slug == "" || slug == agent.CEOSlug {
		return false, errNewChatInvalid
	}
	if _, err := s.Store.GetAgent(slug); err != nil {
		return false, store.ErrNotFound
	}
	if s.Claude == nil {
		return false, errNewChatNoClaude
	}

	hist, err := s.Store.ReadChatHistory(slug)
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		return false, err
	}
	if len(hist) == 0 {
		return false, nil
	}

	// Don't start a rotation while the agent is already mid-response.
	// The rotation prompt would queue behind whatever is currently in
	// flight and produce confusing ordering in chat.jsonl.
	if s.ChatHubActive(slug) {
		return false, errNewChatBusy
	}

	// Snapshot the prior agent memory and habits BEFORE the chat
	// loop begins. The snapshots travel with the archive dir so the
	// old memory is always recoverable; finalizeRotation reads them
	// back from pending_rotation.json at end-of-turn.
	prior, _ := s.Store.ReadAgentMemory(slug)
	priorHabits, _ := s.Store.ReadAgentHabits(slug)

	// Mint the archive generation now, not at finalize: the episode
	// the writer will produce for this chat is named by it, and the
	// agent cites that id in semantic memory during the turn below.
	episodeTS := store.NewArchiveTimestamp()

	// Inject the rotation prompt as a user message. The agent's
	// next inference sees the whole chat history via --resume + this
	// final user release asking for the new memory. The instruction is
	// size-aware — if the prior memory is large, the prompt carries
	// a "prune aggressively" nudge.
	//
	// We route through deliverToAgent for ordering parity with the
	// other external write paths. The ChatHubActive check above
	// ensures the agent is idle, so this is a direct write in
	// practice — but using the same path keeps the invariant
	// "external writes go through deliverToAgent" intact.
	promptBody := buildMemoryUpdateInstruction(len(prior)+len(priorHabits), episodeTS) + "\n\n" + agentMemoryUpdateAsk
	if err := s.deliverToAgent(slug, store.ChatMessage{
		Role:    store.RoleReceived,
		Content: promptBody,
		Kind:    "rotation_prompt",
	}); err != nil {
		return false, err
	}

	// Mark the agent for rotation by writing the on-disk marker.
	// The chat-loop goroutine's post-completion defer reads it via
	// consumeRotationIfReady. The marker survives a Kivali crash, so
	// the rotation completes correctly even if we restart between
	// here and finalize — the recovered chat-turn's finalize finds
	// the marker and archives.
	if err := s.Store.WritePendingRotation(slug, store.PendingRotation{
		PriorMemory: prior,
		PriorHabits: priorHabits,
		Timestamp:   episodeTS,
		RequestedBy: "ceo",
	}); err != nil {
		return false, fmt.Errorf("mark rotation: %w", err)
	}

	// Kick off the response loop — the agent streams the new memory
	// as their next chat message.
	if !s.spawnChatLoopIfIdle(slug, "chat") {
		// Race: someone else claimed the hub between our
		// ChatHubActive check and spawn. The rotation prompt is
		// already in chat.jsonl; best we can do is clear the pending
		// marker so we don't finalize on the wrong response.
		if cerr := s.Store.ClearPendingRotation(slug); cerr != nil {
			log.Printf("new-chat %s: clear rotation marker after spawn failure: %v", slug, cerr)
		}
		return false, errors.New("failed to spawn response loop")
	}
	return true, nil
}

// hasRotationPending reports whether slug has a rotation queued that
// the chat loop hasn't finalized yet. Peek-only — does NOT consume,
// unlike consumeRotationIfReady. Used to decorate the "done" SSE
// event so the client knows a chat.jsonl archive + replace is about
// to happen and can hold the stream open for the subsequent
// rotation_done event rather than closing on "done" and missing it.
func (s *Server) hasRotationPending(slug string) bool {
	_, ok, err := s.Store.ReadPendingRotation(slug)
	if err != nil {
		log.Printf("hasRotationPending %s: %v", slug, err)
		return false
	}
	return ok
}

// rotating reports whether slug's chat is on its way to the archive
// right now: a rotation is pending and the turn that folds the chat
// into memory is running, or has ended and is being finalized. It is
// what the chat API's `rotating` says.
//
// The marker alone is not that. It survives a restart, and an idle
// agent holding one is doing nothing until its next turn (see
// deliverToAgent), so a page must not show it as busy rotating.
func (s *Server) rotating(slug string) bool {
	return s.hasRotationPending(slug) && (s.ChatHubActive(slug) || s.turnInFlight(slug))
}

// consumeRotationIfReady returns true when slug has a pending
// rotation. The agent was asked to update memory via the
// agent_memory_* tools during this release; whatever they did (or
// didn't) ride in agent_memory.md by the time the loop exits, and
// the caller's defer archives the chat either way.
//
// Reads the marker from disk and stashes the prior-memory snapshot
// in latestRotations for finalizeRotation to pick up. The on-disk
// marker is NOT removed here — finalizeRotation removes it after
// successful archive so a crash between consume and finalize would
// re-find the marker on the next turn-end and re-archive (idempotent:
// the marker carries the generation name, and ArchiveChatAs leaves a
// generation that already holds a transcript alone rather than moving
// the fresh chat over it). Without this ordering, the on-disk marker would be cleared
// before its data was preserved on disk in the archive — a crash in
// that window would lose the rotation semantics for good.
func (s *Server) consumeRotationIfReady(slug string) bool {
	marker, ok, err := s.Store.ReadPendingRotation(slug)
	if err != nil {
		log.Printf("consumeRotationIfReady %s: read marker: %v", slug, err)
		return false
	}
	if !ok {
		return false
	}
	s.attachLatestRotation(slug, marker)
	return true
}

// latestRotation is the bundle finalizeRotation pulls: the prior
// memory for the archive snapshot. Stashed between
// consumeRotationIfReady and finalizeRotation via latestRotations so
// finalize doesn't re-read chat.jsonl after we archived it.
type latestRotation struct {
	priorMemory string
	priorHabits string
	// timestamp is the generation minted at request time (empty for a
	// marker written before generations were minted up front).
	timestamp string
}

func (s *Server) attachLatestRotation(slug string, p store.PendingRotation) {
	s.streamMu.Lock()
	if s.latestRotations == nil {
		s.latestRotations = map[string]latestRotation{}
	}
	s.latestRotations[slug] = latestRotation{
		priorMemory: p.PriorMemory,
		priorHabits: p.PriorHabits,
		timestamp:   p.Timestamp,
	}
	s.streamMu.Unlock()
}

// finalizeRotation archives the just-completed chat, snapshots the
// prior memory into the archive dir, clears the CLI session_id,
// removes the on-disk pending-rotation marker, and refreshes the
// filesystem view. The new memory is whatever the agent left in
// agent_memory.md after running their edits during the rotation
// release — finalize does not write it, just archives the snapshot
// of what was there BEFORE the agent started editing.
//
// Order matters: archive + write prior-memory snapshot FIRST, then
// clear the marker. A crash between archive and clear is safe — the
// next turn re-finds the marker and runs ArchiveChatAs again under the
// same generation, which is a no-op once that generation holds a
// transcript, then re-clears.
func (s *Server) finalizeRotation(slug string) {
	s.streamMu.Lock()
	lr, ok := s.latestRotations[slug]
	delete(s.latestRotations, slug)
	s.streamMu.Unlock()
	if !ok {
		return
	}
	// Archive under the generation minted at request time so the
	// episode id the agent just cited resolves to this transcript. A
	// marker from before generations were minted up front carries no
	// timestamp; mint one now — the citation was never made.
	ts := lr.timestamp
	if ts == "" {
		ts = store.NewArchiveTimestamp()
	}
	if err := s.Store.ArchiveChatAs(slug, ts); err != nil {
		log.Printf("new-chat %s: archive failed: %v", slug, err)
		return
	}
	if strings.TrimSpace(lr.priorMemory) != "" {
		if err := s.Store.WriteArchivedAgentMemory(slug, ts, lr.priorMemory); err != nil {
			log.Printf("new-chat %s: prior-memory snapshot failed: %v", slug, err)
		}
	}
	if strings.TrimSpace(lr.priorHabits) != "" {
		if err := s.Store.WriteArchivedAgentHabits(slug, ts, lr.priorHabits); err != nil {
			log.Printf("new-chat %s: prior-habits snapshot failed: %v", slug, err)
		}
	}
	// The transcript is on disk; hand it to the episode writer. Off the
	// critical path on purpose — the digest lands in the background and
	// the agent's next turn links it under /files/episodes/.
	s.NotifyEpisode(slug, ts)
	if err := s.Store.ClearClaudeSessionID(slug); err != nil {
		log.Printf("new-chat %s: clear CLI session failed: %v", slug, err)
	}
	// Clearing the stored id is inert against the agent pod's warm
	// `claude -p` runner: it keeps the full pre-rotation conversation in
	// memory and would --resume it on the next turn, so the rotation
	// would be invisible to the model (transcript rotates, context window
	// doesn't). Signal the pod to tear the runner down — the next turn
	// then reads the now-empty id and spawns a genuinely fresh session.
	if err := s.PublishAgentpodEvent(slug, agentpod.EventSessionReset, agentpod.SessionResetEvent{Slug: slug}); err != nil {
		log.Printf("new-chat %s: signal session reset failed: %v", slug, err)
	}
	// The archived chat's occupancy says nothing about the fresh one;
	// drop the sidecar so the ring snaps back to the cold-start estimate
	// instead of showing the old chat's fill until the first new turn.
	if err := s.Store.ClearContextWindow(slug); err != nil {
		log.Printf("new-chat %s: clear context-window stat failed: %v", slug, err)
	}
	// The finished background tasks belong to the chat that dispatched
	// them, which is now in the archive with their chips and results;
	// the panel and subagent_status start the fresh chat with an empty
	// history. Live tasks stay — they are owed to the new chat.
	if n := s.SubagentService.ForgetFinished(slug); n > 0 {
		log.Printf("new-chat %s: forgot %d finished background task(s)", slug, n)
	}
	if err := s.Store.SyncAgentFilesystem(slug); err != nil {
		log.Printf("new-chat %s: files sync failed: %v", slug, err)
	}
	if err := s.Store.ClearPendingRotation(slug); err != nil {
		log.Printf("new-chat %s: clear pending-rotation marker: %v", slug, err)
	}
	log.Printf("new-chat %s: rotation finalized (archived=%s)", slug, ts)
}

// agentMemoryUpdateInstruction is the reconcile prompt — the one turn
// in which the two prompt-resident memory tiers are rewritten. It does
// the heavy lifting of the whole memory design, so its shape is
// deliberate: the three questions are answered in order because
// nothing else in the loop ever asks the first one; provenance is
// demanded because without it "newer wins" is undecidable; the
// third question moves any rule found in agent_memory.md into
// habits, one rotation at a time, with the snapshot as the safety
// net; and the first question
// also evicts narrative, because episodes exist to make that deletion
// safe and no other surface says so — a story is neither contradicted
// nor a rule, so without this it survives every rotation.
//
// {{ts}} is the archive generation minted for this rotation — the
// episode id the agent cites. memoryUpdateInstructionFor fills it.
const agentMemoryUpdateInstruction = `The owner has requested a chat rotation. Before this chat archives, reconcile your memory. It has three tiers, and this turn is the only time the first two are rewritten:

- **Habits** (` + "`agent_memory_habits_view` / `_append` / `_str_replace`" + `) — the short list of rules for how you act, each with a one-line why. Rewritten only here.
- **Semantic memory** (` + "`agent_memory_view` / `_append` / `_str_replace`" + `) — what you know to be true, atemporal, in your own voice. Inlined into every future system prompt. Reconciled here and only here.
- **Episodes** (` + "`/files/episodes/<ts>.md`" + `, read-only) — what happened, one digest per archived chat, written for you after this chat archives. You never edit them. This chat's episode id is ` + "`{{ts}}`" + `; cite it as ` + "`[[ep:{{ts}}]]`" + `.

Start with ` + "`agent_memory_view`" + ` and ` + "`agent_memory_habits_view`" + ` so you are editing what is actually there. Then answer three questions, in this order:

1. **What should I stop believing?** Every entry this chat contradicted, superseded, or made irrelevant: delete it (` + "`str_replace`" + ` with an empty new_str) or rewrite it. Do this before adding anything. A "fact" that one tool call could look up — which branch is merged, what is deployed, who is on call — is a cache with no invalidation: record how to find it, not the value. When a durable lesson is attached to a volatile status, keep the lesson and drop the status. Narrative goes the same way: semantic memory holds what is true, not what happened, and what happened is in the episodes — one per archived chat, this chat's at ` + "`[[ep:{{ts}}]]`" + ` — so a story in memory duplicates a record that already exists. Where an entry recounts what was asked, tried, or said, keep the fact or the current state it produced with its ` + "`[[ep:<ts>]]`" + ` pointer and delete the telling. An open thread keeps its state and its next step; how it got there is the episode's job.
2. **What did I learn that is atemporal?** Add it to semantic memory with its provenance: ` + "`[[ep:{{ts}}]]`" + ` for something learned in this chat, ` + "`[[stated]]`" + ` for something a person told you. If it only holds for one environment, one agent, or one project, say so in the entry — most apparent contradictions are missing qualifiers, not disagreements. Prefer rewriting a related entry over adding a sibling.
3. **What changed about how I should act?** Habits only, each with a why. Revoking one is as valid as adding one. Any entry in semantic memory that is a rule rather than a fact belongs here — move it.

When entries conflict: a newer observation beats an older one; what a person stated beats what you concluded; the handbook and your role beat everything in memory; "dev does X" and "prod does X" are not a conflict — scope both.

Keep both tiers in your own voice, first person where natural, and lean: semantic memory rides in every future call, so every byte costs on every reply, and habits are a short list or they are not habits.

Memory is distinct from the /files/ file tool. Long-lived, summary-level context goes in memory; disposable scratch work goes in /files/artifacts/private/.`

// memoryUpdateInstructionFor renders the reconcile prompt for one
// rotation, naming the episode generation the agent should cite.
func memoryUpdateInstructionFor(episodeTS string) string {
	return strings.ReplaceAll(agentMemoryUpdateInstruction, "{{ts}}", episodeTS)
}

const agentMemoryUpdateAsk = "Do your memory edits now. When both tiers are in the shape you want future-you to see at the start of the next chat, finish your reply — the chat will archive and a fresh one will start. No specific reply text is needed; the tool calls are the work."

// Size nudge thresholds. Agent memory is inlined into every future
// system prompt, so bloat costs tokens on every call. Soft nudge asks
// the agent to prefer trimming; hard nudge asks for aggressive pruning
// and invites a structural/org-change suggestion if they can't shed
// load without losing real context.
const (
	memoryNudgeSoftBytes = 16 * 1024
	memoryNudgeHardBytes = 32 * 1024
)

// buildMemoryUpdateInstruction returns the system-block instruction for
// the rewrite, with a size-aware addendum when the prior memory is
// large. Below the soft threshold the instruction is unchanged.
func buildMemoryUpdateInstruction(priorMemoryBytes int, episodeTS string) string {
	base := memoryUpdateInstructionFor(episodeTS)
	if priorMemoryBytes < memoryNudgeSoftBytes {
		return base
	}
	kb := priorMemoryBytes / 1024
	if priorMemoryBytes >= memoryNudgeHardBytes {
		return base + fmt.Sprintf(`

---

**Size check — your memory is ~%d KB, over budget.** Every byte here rides in every future system prompt, so bloat costs real tokens on every reply and crowds out the conversation itself. This rotation, prune aggressively: drop superseded decisions, collapse resolved threads, consolidate overlapping sections. Target under 32 KB.

If you find you cannot trim further without losing load-bearing context, that's itself a signal worth surfacing. It usually means the scope you're holding has outgrown a single agent. Before the next rotation, consider whether to propose an organizational change to the owner — splitting your responsibilities, hiring a new agent to absorb a specific domain, or delegating an area you've been carrying alone. If you believe that's the right call, raise it explicitly (an assignment for your manager, or in your next chat) rather than silently continuing to absorb the load. Include this reasoning in the memory so future-you remembers the suggestion is outstanding.`, kb)
	}
	return base + fmt.Sprintf(`

---

**Size check — your memory is ~%d KB, on the larger side.** It rides in every future system prompt, so prefer trimming over adding this round: drop stale context, collapse resolved threads, and keep only what's load-bearing.

If trimming is hard because you're carrying a lot of distinct threads, think about whether the shape of your work has grown past what one agent should hold. Worth raising with your manager or the owner whether to split responsibilities or hire a new agent to take a specific domain off your plate — don't just silently shoulder it.`, kb)
}

// rotationOutcome is what one rotation left behind: the episode digest
// the writer produced for generation ts (the whole file, frontmatter
// included; empty until it is written), and the memory and habits
// as they stood when the rotation was asked for and after it ran.
type rotationOutcome struct {
	Episode      string
	MemoryBefore string
	MemoryAfter  string
	HabitsBefore string
	HabitsAfter  string
}

// rotationOutcomeFor reads generation ts's outcome. Shared by the
// archived-chat page and the past-chat JSON.
func (s *Server) rotationOutcomeFor(slug, ts string) rotationOutcome {
	var out rotationOutcome
	out.MemoryBefore, _ = s.Store.ReadArchivedAgentMemory(slug, ts)
	out.HabitsBefore, _ = s.Store.ReadArchivedAgentHabits(slug, ts)
	out.MemoryAfter, out.HabitsAfter = s.memoryAfterGeneration(slug, ts)
	out.Episode, _ = s.Store.ReadEpisode(slug, ts)
	return out
}

// memoryAfterGeneration returns the memory and habits as they
// stood once generation ts's rotation had run: the snapshots stored
// beside the next-newer generation (taken when THAT rotation was
// requested), or the live files when ts is the latest generation. A
// missing snapshot reads as empty — a rotation that started with no
// memory left no file, and the diff should show everything as added.
func (s *Server) memoryAfterGeneration(slug, ts string) (memory, habits string) {
	gens, err := s.Store.ListArchivedChats(slug) // newest first
	if err != nil {
		return "", ""
	}
	for i, g := range gens {
		if g.Timestamp != ts {
			continue
		}
		if i == 0 {
			memory, _ = s.Store.ReadAgentMemory(slug)
			habits, _ = s.Store.ReadAgentHabits(slug)
			return memory, habits
		}
		next := gens[i-1].Timestamp
		memory, _ = s.Store.ReadArchivedAgentMemory(slug, next)
		habits, _ = s.Store.ReadArchivedAgentHabits(slug, next)
		return memory, habits
	}
	return "", ""
}
